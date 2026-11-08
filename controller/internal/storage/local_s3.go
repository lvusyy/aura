package storage

import (
	"bufio"
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// StartLocalS3Server 启动一个零外部依赖的本地磁盘持久化 S3 兼容网关（供 AURA_MINIO_ENDPOINT=local 或
// AURA_ARTIFACT_DIR 单二进制部署使用）。支持建桶、GetBucketLocation、PutObject（含 AWS SigV4 chunked 流与
// Multipart Upload 大文件分片）、GetObject/OpenObject（含 HTTP Range 流式回放）、ListObjectsV2/V1 列举与
// Presigned URL 直传/直下。
func StartLocalS3Server(dataDir, listenAddr string) (string, func(), error) {
	if strings.TrimSpace(dataDir) == "" {
		dataDir = "./aura-data/artifacts"
	}
	absDir, err := filepath.Abs(dataDir)
	if err != nil {
		return "", nil, fmt.Errorf("resolve local s3 dir: %w", err)
	}
	if err := os.MkdirAll(absDir, 0o755); err != nil {
		return "", nil, fmt.Errorf("mkdir local s3 dir %q: %w", absDir, err)
	}
	if strings.TrimSpace(listenAddr) == "" {
		listenAddr = "127.0.0.1:0"
	}
	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return "", nil, fmt.Errorf("listen local s3 %q: %w", listenAddr, err)
	}

	var mu sync.RWMutex
	mpParts := make(map[string]map[int][]byte)

	safePath := func(bucket, key string) (string, error) {
		cleanBucket := filepath.Clean(bucket)
		if cleanBucket == "." || cleanBucket == ".." || strings.ContainsAny(cleanBucket, `/\`) {
			return "", fmt.Errorf("invalid bucket %q", bucket)
		}
		if key == "" {
			return filepath.Join(absDir, cleanBucket), nil
		}
		cleanKey := filepath.Clean(filepath.FromSlash(key))
		if cleanKey == "." || cleanKey == ".." || strings.HasPrefix(cleanKey, "..") || filepath.IsAbs(cleanKey) {
			return "", fmt.Errorf("invalid key %q", key)
		}
		full := filepath.Join(absDir, cleanBucket, cleanKey)
		rel, err := filepath.Rel(filepath.Join(absDir, cleanBucket), full)
		if err != nil || strings.HasPrefix(rel, "..") {
			return "", fmt.Errorf("key escapes bucket: %q", key)
		}
		return full, nil
	}

	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			path := strings.TrimPrefix(r.URL.Path, "/")
			parts := strings.SplitN(path, "/", 2)
			bucket := parts[0]
			if bucket == "" {
				w.WriteHeader(http.StatusOK)
				return
			}
			q := r.URL.Query()

			// Bucket-level operations
			if len(parts) == 1 || parts[1] == "" {
				bDir, err := safePath(bucket, "")
				if err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
				if _, ok := q["location"]; ok {
					w.Header().Set("Content-Type", "application/xml")
					_, _ = io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?><LocationConstraint xmlns="http://s3.amazonaws.com/doc/2006-03-01/">us-east-1</LocationConstraint>`)
					return
				}
				if _, ok := q["lifecycle"]; ok {
					w.WriteHeader(http.StatusOK)
					return
				}
				switch r.Method {
				case http.MethodPut:
					if err := os.MkdirAll(bDir, 0o755); err != nil {
						http.Error(w, err.Error(), http.StatusInternalServerError)
						return
					}
					w.WriteHeader(http.StatusOK)
				case http.MethodHead:
					if st, err := os.Stat(bDir); err == nil && st.IsDir() {
						w.WriteHeader(http.StatusOK)
					} else {
						w.WriteHeader(http.StatusNotFound)
					}
				case http.MethodGet:
					prefix := q.Get("prefix")
					maxKeys := 1000
					if mk := q.Get("max-keys"); mk != "" {
						if n, err := strconv.Atoi(mk); err == nil && n > 0 {
							maxKeys = n
						}
					}
					type s3Content struct {
						Key          string `xml:"Key"`
						LastModified string `xml:"LastModified"`
						ETag         string `xml:"ETag"`
						Size         int64  `xml:"Size"`
						StorageClass string `xml:"StorageClass"`
					}
					var contents []s3Content
					mu.RLock()
					_ = filepath.WalkDir(bDir, func(p string, d fs.DirEntry, werr error) error {
						if werr != nil || d.IsDir() || strings.HasSuffix(p, ".ct") {
							return nil
						}
						rel, rerr := filepath.Rel(bDir, p)
						if rerr != nil {
							return nil
						}
						k := filepath.ToSlash(rel)
						if prefix != "" && !strings.HasPrefix(k, prefix) {
							return nil
						}
						info, ierr := d.Info()
						if ierr != nil {
							return nil
						}
						contents = append(contents, s3Content{
							Key:          k,
							LastModified: info.ModTime().UTC().Format("2006-01-02T15:04:05.000Z"),
							ETag:         `"local-etag"`,
							Size:         info.Size(),
							StorageClass: "STANDARD",
						})
						return nil
					})
					mu.RUnlock()
					sort.Slice(contents, func(i, j int) bool { return contents[i].Key < contents[j].Key })
					if len(contents) > maxKeys {
						contents = contents[:maxKeys]
					}
					type listBucketV2 struct {
						XMLName     xml.Name    `xml:"http://s3.amazonaws.com/doc/2006-03-01/ ListBucketResult"`
						Name        string      `xml:"Name"`
						Prefix      string      `xml:"Prefix"`
						KeyCount    int         `xml:"KeyCount"`
						MaxKeys     int         `xml:"MaxKeys"`
						IsTruncated bool        `xml:"IsTruncated"`
						Contents    []s3Content `xml:"Contents"`
					}
					w.Header().Set("Content-Type", "application/xml")
					outXML, _ := xml.Marshal(listBucketV2{
						Name:        bucket,
						Prefix:      prefix,
						KeyCount:    len(contents),
						MaxKeys:     maxKeys,
						IsTruncated: false,
						Contents:    contents,
					})
					_, _ = w.Write([]byte(xml.Header))
					_, _ = w.Write(outXML)
				default:
					w.WriteHeader(http.StatusOK)
				}
				return
			}

			// Object-level operations
			key := parts[1]
			objPath, err := safePath(bucket, key)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			mpKey := bucket + "/" + key

			switch r.Method {
			case http.MethodPost:
				if _, ok := q["uploads"]; ok {
					mu.Lock()
					mpParts[mpKey] = make(map[int][]byte)
					mu.Unlock()
					w.Header().Set("Content-Type", "application/xml")
					_, _ = io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?><InitiateMultipartUploadResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Bucket>`+bucket+`</Bucket><Key>`+key+`</Key><UploadId>local-u1</UploadId></InitiateMultipartUploadResult>`)
					return
				}
				if q.Get("uploadId") != "" {
					_, _ = io.ReadAll(r.Body)
					mu.Lock()
					pm := mpParts[mpKey]
					var combined bytes.Buffer
					for i := 1; i <= len(pm); i++ {
						combined.Write(pm[i])
					}
					delete(mpParts, mpKey)
					data := combined.Bytes()
					err := writeObjectDisk(objPath, data, "application/octet-stream")
					mu.Unlock()
					if err != nil {
						http.Error(w, err.Error(), http.StatusInternalServerError)
						return
					}
					sum := md5.Sum(data)
					etag := `"` + hex.EncodeToString(sum[:]) + `"`
					w.Header().Set("Content-Type", "application/xml")
					_, _ = io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?><CompleteMultipartUploadResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Bucket>`+bucket+`</Bucket><Key>`+key+`</Key><ETag>`+etag+`</ETag></CompleteMultipartUploadResult>`)
					return
				}
				w.WriteHeader(http.StatusOK)

			case http.MethodPut:
				body := decodeAWSBody(r)
				if pnStr := q.Get("partNumber"); pnStr != "" && q.Get("uploadId") != "" {
					pn, _ := strconv.Atoi(pnStr)
					sum := md5.Sum(body)
					etag := `"` + hex.EncodeToString(sum[:]) + `"`
					mu.Lock()
					if mpParts[mpKey] == nil {
						mpParts[mpKey] = make(map[int][]byte)
					}
					mpParts[mpKey][pn] = body
					mu.Unlock()
					w.Header().Set("ETag", etag)
					w.WriteHeader(http.StatusOK)
					return
				}
				ct := r.Header.Get("Content-Type")
				if ct == "" {
					ct = "application/octet-stream"
				}
				mu.Lock()
				werr := writeObjectDisk(objPath, body, ct)
				mu.Unlock()
				if werr != nil {
					http.Error(w, werr.Error(), http.StatusInternalServerError)
					return
				}
				sum := md5.Sum(body)
				w.Header().Set("ETag", `"`+hex.EncodeToString(sum[:])+`"`)
				w.WriteHeader(http.StatusOK)

			case http.MethodHead, http.MethodGet:
				mu.RLock()
				f, err := os.Open(objPath)
				if err != nil {
					mu.RUnlock()
					w.WriteHeader(http.StatusNotFound)
					return
				}
				defer func() {
					_ = f.Close()
					mu.RUnlock()
				}()
				st, err := f.Stat()
				if err != nil || st.IsDir() {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				ct := readObjectContentType(objPath)
				w.Header().Set("Content-Type", ct)
				w.Header().Set("ETag", `"local-etag"`)
				w.Header().Set("Last-Modified", st.ModTime().UTC().Format(http.TimeFormat))
				http.ServeContent(w, r, filepath.Base(objPath), st.ModTime().UTC(), f)

			case http.MethodDelete:
				mu.Lock()
				_ = os.Remove(objPath)
				_ = os.Remove(objPath + ".ct")
				mu.Unlock()
				w.WriteHeader(http.StatusNoContent)

			default:
				w.WriteHeader(http.StatusOK)
			}
		}),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		_ = srv.Serve(ln)
	}()

	closeFn := func() {
		_ = srv.Close()
	}
	return ln.Addr().String(), closeFn, nil
}

func writeObjectDisk(objPath string, data []byte, contentType string) error {
	if err := os.MkdirAll(filepath.Dir(objPath), 0o755); err != nil {
		return err
	}
	tmp := objPath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, objPath); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if contentType != "" {
		_ = os.WriteFile(objPath+".ct", []byte(contentType), 0o644)
	}
	return nil
}

func readObjectContentType(objPath string) string {
	if b, err := os.ReadFile(objPath + ".ct"); err == nil && len(b) > 0 {
		return strings.TrimSpace(string(b))
	}
	return "application/octet-stream"
}

func decodeAWSBody(r *http.Request) []byte {
	raw, _ := io.ReadAll(r.Body)
	if !strings.HasPrefix(r.Header.Get("x-amz-content-sha256"), "STREAMING-") {
		return raw
	}
	br := bufio.NewReader(bytes.NewReader(raw))
	var out bytes.Buffer
	for {
		hdr, err := br.ReadString('\n')
		if err != nil {
			break
		}
		hdr = strings.TrimRight(hdr, "\r\n")
		if semi := strings.IndexByte(hdr, ';'); semi > 0 {
			hdr = hdr[:semi]
		}
		n, err := strconv.ParseInt(strings.TrimSpace(hdr), 16, 64)
		if err != nil || n == 0 {
			break
		}
		_, _ = io.CopyN(&out, br, n)
		_, _ = br.ReadString('\n')
	}
	return out.Bytes()
}
