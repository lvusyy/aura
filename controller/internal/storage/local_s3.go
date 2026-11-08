package storage

import (
	"bufio"
	"bytes"
	"crypto/hmac"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	internalDirName       = ".aura-internal"
	defaultMaxUploadBytes = 512 * 1024 * 1024 // 512 MiB 单次 PUT / 分片上限
	multipartSessionTTL   = 2 * time.Hour
)

type objectMeta struct {
	ContentType string `json:"content_type"`
	ETag        string `json:"etag"`
	Size        int64  `json:"size"`
}

type multipartPart struct {
	PartNumber int
	ETag       string
	Size       int64
	FilePath   string
}

type multipartSession struct {
	UploadID   string
	Bucket     string
	Key        string
	CreatedAt  time.Time
	Completing bool
	Parts      map[int]multipartPart
}

type lifecycleRule struct {
	ID     string `json:"id"`
	Prefix string `json:"prefix"`
	Days   int    `json:"days"`
	Status string `json:"status"`
}

// LocalS3Options 配置本地持久化 S3 兼容服务的安全与配额参数。
type LocalS3Options struct {
	DataDir        string
	ListenAddr     string
	AccessKey      string
	SecretKey      string
	MaxUploadBytes int64
	AllowRemote    bool
}

// StartLocalS3Server 启动带默认凭据（aura / aurasecret）的本地持久化 S3 兼容网关。
func StartLocalS3Server(dataDir, listenAddr string) (string, func(), error) {
	return StartLocalS3ServerWithOptions(LocalS3Options{
		DataDir:    dataDir,
		ListenAddr: listenAddr,
		AccessKey:  "aura",
		SecretKey:  "aurasecret",
	})
}

// StartLocalS3ServerWithOptions 启动零外部依赖的本地磁盘持久化 S3 兼容网关：
//   - 强制校验 AWS SigV4 Header 签名与 Presigned URL 签名/过期时间（修复 Finding #3）
//   - 默认强制绑定 loopback 地址（除非显式开启 AllowRemote）
//   - 唯一 UploadId 会话隔离、PartNumber/ETag 校验与 AbortMultipartUpload 隔离（修复 Finding #4）
//   - 流式有界写入、Content-Length/aws-chunked 截断检测与临时文件原子提交（修复 Finding #5）
//   - ListObjectsV2/V1 完整分页（ContinuationToken / NextContinuationToken）与 Lifecycle 过期清理（修复 Finding #11）
//   - 下载流释放全局锁后再执行 ServeContent，防慢客户端阻塞写入（修复 Finding #12）
//   - 内部元数据与临时文件隔离在 .aura-internal 命名空间，并校验 symlink 防逃逸（修复 Finding #14）
func StartLocalS3ServerWithOptions(opts LocalS3Options) (string, func(), error) {
	dataDir := strings.TrimSpace(opts.DataDir)
	if dataDir == "" {
		dataDir = "./aura-data/artifacts"
	}
	absDir, err := filepath.Abs(dataDir)
	if err != nil {
		return "", nil, fmt.Errorf("resolve local s3 dir: %w", err)
	}
	if err := os.MkdirAll(absDir, 0o755); err != nil {
		return "", nil, fmt.Errorf("mkdir local s3 dir %q: %w", absDir, err)
	}
	realRoot, err := filepath.EvalSymlinks(absDir)
	if err != nil {
		realRoot = absDir
	}
	for _, sub := range []string{"tmp", "uploads", "meta"} {
		if err := os.MkdirAll(filepath.Join(absDir, internalDirName, sub), 0o755); err != nil {
			return "", nil, fmt.Errorf("mkdir internal s3 dir: %w", err)
		}
	}

	listenAddr := strings.TrimSpace(opts.ListenAddr)
	if listenAddr == "" {
		listenAddr = "127.0.0.1:0"
	}
	if !opts.AllowRemote {
		host, _, splitErr := net.SplitHostPort(listenAddr)
		if splitErr != nil {
			return "", nil, fmt.Errorf("invalid local s3 listen addr %q: %w", listenAddr, splitErr)
		}
		if !isLoopbackHost(host) {
			return "", nil, fmt.Errorf("local s3 refuses non-loopback listen addr %q without AURA_LOCAL_S3_ALLOW_REMOTE=true", listenAddr)
		}
	}

	maxUpload := opts.MaxUploadBytes
	if maxUpload <= 0 {
		maxUpload = defaultMaxUploadBytes
	}

	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return "", nil, fmt.Errorf("listen local s3 %q: %w", listenAddr, err)
	}

	var mu sync.RWMutex
	var seqCounter atomic.Uint64
	mpSessions := make(map[string]*multipartSession)
	lifecycleRules := make(map[string][]lifecycleRule)

	nextID := func(prefix string) string {
		n := seqCounter.Add(1)
		return fmt.Sprintf("%s-%d-%d", prefix, time.Now().UTC().UnixNano(), n)
	}

	checkNoSymlinkEscape := func(candidate string) error {
		cur := candidate
		for {
			if _, serr := os.Lstat(cur); serr == nil {
				resolved, EErr := filepath.EvalSymlinks(cur)
				if EErr != nil {
					return fmt.Errorf("eval symlinks %q: %w", cur, EErr)
				}
				rel, rerr := filepath.Rel(realRoot, resolved)
				if rerr != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
					return fmt.Errorf("path escapes storage root via symlink: %q", candidate)
				}
				return nil
			}
			parent := filepath.Dir(cur)
			if parent == cur {
				break
			}
			cur = parent
		}
		return nil
	}

	safePath := func(bucket, key string) (string, error) {
		cleanBucket := filepath.Clean(bucket)
		if cleanBucket == "." || cleanBucket == ".." || cleanBucket == internalDirName || strings.ContainsAny(cleanBucket, `/\:`) {
			return "", fmt.Errorf("invalid bucket %q", bucket)
		}
		bDir := filepath.Join(absDir, cleanBucket)
		if key == "" {
			if err := checkNoSymlinkEscape(bDir); err != nil {
				return "", err
			}
			return bDir, nil
		}
		slashKey := filepath.ToSlash(key)
		for _, seg := range strings.Split(slashKey, "/") {
			if seg == "" || seg == "." || seg == ".." || seg == internalDirName {
				return "", fmt.Errorf("invalid key segment in %q", key)
			}
		}
		cleanKey := filepath.Clean(filepath.FromSlash(slashKey))
		if cleanKey == "." || cleanKey == ".." || strings.HasPrefix(cleanKey, "..") || filepath.IsAbs(cleanKey) || filepath.VolumeName(cleanKey) != "" {
			return "", fmt.Errorf("invalid key %q", key)
		}
		full := filepath.Join(bDir, cleanKey)
		rel, err := filepath.Rel(bDir, full)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return "", fmt.Errorf("key escapes bucket: %q", key)
		}
		if err := checkNoSymlinkEscape(full); err != nil {
			return "", err
		}
		return full, nil
	}

	metaPathFor := func(bucket, key string) string {
		cleanKey := filepath.Clean(filepath.FromSlash(key))
		return filepath.Join(absDir, internalDirName, "meta", filepath.Clean(bucket), cleanKey+".json")
	}

	writeMeta := func(bucket, key string, m objectMeta) error {
		mp := metaPathFor(bucket, key)
		if err := os.MkdirAll(filepath.Dir(mp), 0o755); err != nil {
			return err
		}
		raw, err := json.Marshal(m)
		if err != nil {
			return err
		}
		tmp := filepath.Join(absDir, internalDirName, "tmp", nextID("meta")+".tmp")
		if err := os.WriteFile(tmp, raw, 0o644); err != nil {
			return err
		}
		if err := os.Rename(tmp, mp); err != nil {
			_ = os.Remove(tmp)
			return err
		}
		return nil
	}

	readMeta := func(bucket, key string) objectMeta {
		mp := metaPathFor(bucket, key)
		var m objectMeta
		if b, err := os.ReadFile(mp); err == nil {
			_ = json.Unmarshal(b, &m)
		}
		if m.ContentType == "" {
			m.ContentType = "application/octet-stream"
		}
		if m.ETag == "" {
			m.ETag = `"local-etag"`
		}
		return m
	}

	gcExpiredUploadsLocked := func(now time.Time) {
		for id, sess := range mpSessions {
			if !sess.Completing && now.Sub(sess.CreatedAt) > multipartSessionTTL {
				_ = os.RemoveAll(filepath.Join(absDir, internalDirName, "uploads", id))
				delete(mpSessions, id)
			}
		}
	}

	applyBucketLifecycleLocked := func(bucket, bDir string, now time.Time) {
		rules := lifecycleRules[bucket]
		if len(rules) == 0 {
			return
		}
		_ = filepath.WalkDir(bDir, func(p string, d fs.DirEntry, werr error) error {
			if werr != nil || d.IsDir() {
				return nil
			}
			rel, rerr := filepath.Rel(bDir, p)
			if rerr != nil {
				return nil
			}
			k := filepath.ToSlash(rel)
			info, ierr := d.Info()
			if ierr != nil {
				return nil
			}
			for _, rule := range rules {
				if !strings.EqualFold(rule.Status, "Enabled") || rule.Days <= 0 {
					continue
				}
				if rule.Prefix != "" && !strings.HasPrefix(k, rule.Prefix) {
					continue
				}
				cutoff := now.Add(-time.Duration(rule.Days) * 24 * time.Hour)
				if info.ModTime().Before(cutoff) {
					_ = os.Remove(p)
					_ = os.Remove(metaPathFor(bucket, k))
					break
				}
			}
			return nil
		})
	}

	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// 强制校验 AWS SigV4（Header 或 Presigned URL 查询参数）
			if opts.AccessKey != "" && opts.SecretKey != "" {
				if err := verifyRequestSigV4(r, opts.AccessKey, opts.SecretKey, time.Now().UTC()); err != nil {
					writeS3Error(w, http.StatusForbidden, "AccessDenied", err.Error())
					return
				}
			}

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
					writeS3Error(w, http.StatusBadRequest, "InvalidBucketName", err.Error())
					return
				}
				if _, ok := q["location"]; ok {
					w.Header().Set("Content-Type", "application/xml")
					_, _ = io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?><LocationConstraint xmlns="http://s3.amazonaws.com/doc/2006-03-01/">us-east-1</LocationConstraint>`)
					return
				}
				if _, ok := q["lifecycle"]; ok {
					switch r.Method {
					case http.MethodPut:
						body, rerr := io.ReadAll(io.LimitReader(r.Body, 64*1024))
						if rerr != nil {
							writeS3Error(w, http.StatusBadRequest, "MalformedXML", rerr.Error())
							return
						}
						parsedRules := parseLifecycleXML(body)
						mu.Lock()
						lifecycleRules[bucket] = parsedRules
						applyBucketLifecycleLocked(bucket, bDir, time.Now().UTC())
						mu.Unlock()
						w.WriteHeader(http.StatusOK)
					case http.MethodGet:
						w.Header().Set("Content-Type", "application/xml")
						w.WriteHeader(http.StatusOK)
					default:
						w.WriteHeader(http.StatusOK)
					}
					return
				}

				switch r.Method {
				case http.MethodPut:
					if err := os.MkdirAll(bDir, 0o755); err != nil {
						writeS3Error(w, http.StatusInternalServerError, "InternalError", err.Error())
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
					contToken := q.Get("continuation-token")
					startAfter := q.Get("start-after")
					marker := q.Get("marker")
					afterKey := firstNonEmptyStr(contToken, startAfter, marker)

					type s3Content struct {
						Key          string `xml:"Key"`
						LastModified string `xml:"LastModified"`
						ETag         string `xml:"ETag"`
						Size         int64  `xml:"Size"`
						StorageClass string `xml:"StorageClass"`
					}
					var allContents []s3Content
					mu.Lock()
					applyBucketLifecycleLocked(bucket, bDir, time.Now().UTC())
					mu.Unlock()

					mu.RLock()
					_ = filepath.WalkDir(bDir, func(p string, d fs.DirEntry, werr error) error {
						if werr != nil || d.IsDir() {
							return nil
						}
						rel, rerr := filepath.Rel(bDir, p)
						if rerr != nil {
							return nil
						}
						k := filepath.ToSlash(rel)
						if strings.HasPrefix(k, internalDirName+"/") || k == internalDirName {
							return nil
						}
						if prefix != "" && !strings.HasPrefix(k, prefix) {
							return nil
						}
						info, ierr := d.Info()
						if ierr != nil {
							return nil
						}
						m := readMeta(bucket, k)
						allContents = append(allContents, s3Content{
							Key:          k,
							LastModified: info.ModTime().UTC().Format("2006-01-02T15:04:05.000Z"),
							ETag:         m.ETag,
							Size:         info.Size(),
							StorageClass: "STANDARD",
						})
						return nil
					})
					mu.RUnlock()

					sort.Slice(allContents, func(i, j int) bool { return allContents[i].Key < allContents[j].Key })

					var filtered []s3Content
					for _, item := range allContents {
						if afterKey != "" && item.Key <= afterKey {
							continue
						}
						filtered = append(filtered, item)
					}

					isTruncated := false
					nextToken := ""
					if len(filtered) > maxKeys {
						isTruncated = true
						nextToken = filtered[maxKeys-1].Key
						filtered = filtered[:maxKeys]
					}

					type listBucketV2 struct {
						XMLName               xml.Name    `xml:"http://s3.amazonaws.com/doc/2006-03-01/ ListBucketResult"`
						Name                  string      `xml:"Name"`
						Prefix                string      `xml:"Prefix"`
						KeyCount              int         `xml:"KeyCount"`
						MaxKeys               int         `xml:"MaxKeys"`
						IsTruncated           bool        `xml:"IsTruncated"`
						ContinuationToken     string      `xml:"ContinuationToken,omitempty"`
						NextContinuationToken string      `xml:"NextContinuationToken,omitempty"`
						Marker                string      `xml:"Marker,omitempty"`
						NextMarker            string      `xml:"NextMarker,omitempty"`
						Contents              []s3Content `xml:"Contents"`
					}
					w.Header().Set("Content-Type", "application/xml")
					outXML, _ := xml.Marshal(listBucketV2{
						Name:                  bucket,
						Prefix:                prefix,
						KeyCount:              len(filtered),
						MaxKeys:               maxKeys,
						IsTruncated:           isTruncated,
						ContinuationToken:     contToken,
						NextContinuationToken: nextToken,
						Marker:                marker,
						NextMarker:            nextToken,
						Contents:              filtered,
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
				writeS3Error(w, http.StatusBadRequest, "InvalidKey", err.Error())
				return
			}

			switch r.Method {
			case http.MethodPost:
				if _, ok := q["uploads"]; ok {
					upID := nextID("upload")
					upDir := filepath.Join(absDir, internalDirName, "uploads", upID)
					if err := os.MkdirAll(upDir, 0o755); err != nil {
						writeS3Error(w, http.StatusInternalServerError, "InternalError", err.Error())
						return
					}
					mu.Lock()
					gcExpiredUploadsLocked(time.Now().UTC())
					mpSessions[upID] = &multipartSession{
						UploadID:  upID,
						Bucket:    bucket,
						Key:       key,
						CreatedAt: time.Now().UTC(),
						Parts:     make(map[int]multipartPart),
					}
					mu.Unlock()
					w.Header().Set("Content-Type", "application/xml")
					_, _ = io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?><InitiateMultipartUploadResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Bucket>`+xmlEscape(bucket)+`</Bucket><Key>`+xmlEscape(key)+`</Key><UploadId>`+upID+`</UploadId></InitiateMultipartUploadResult>`)
					return
				}

				if upID := q.Get("uploadId"); upID != "" {
					reqBody, rerr := io.ReadAll(io.LimitReader(r.Body, 2*1024*1024))
					if rerr != nil {
						writeS3Error(w, http.StatusBadRequest, "IncompleteBody", rerr.Error())
						return
					}
					var compReq struct {
						Parts []struct {
							PartNumber int    `xml:"PartNumber"`
							ETag       string `xml:"ETag"`
						} `xml:"Part"`
					}
					if err := xml.Unmarshal(reqBody, &compReq); err != nil || len(compReq.Parts) == 0 {
						writeS3Error(w, http.StatusBadRequest, "MalformedXML", "invalid CompleteMultipartUpload XML")
						return
					}

					mu.Lock()
					sess, ok := mpSessions[upID]
					if !ok || sess.Completing || sess.Bucket != bucket || sess.Key != key {
						mu.Unlock()
						writeS3Error(w, http.StatusNotFound, "NoSuchUpload", "multipart upload session not found")
						return
					}
					var orderedParts []multipartPart
					prevNum := 0
					for _, rp := range compReq.Parts {
						if rp.PartNumber <= prevNum {
							mu.Unlock()
							writeS3Error(w, http.StatusBadRequest, "InvalidPartOrder", "parts must be in strictly ascending order")
							return
						}
						prevNum = rp.PartNumber
						stored, exists := sess.Parts[rp.PartNumber]
						if !exists {
							mu.Unlock()
							writeS3Error(w, http.StatusBadRequest, "InvalidPart", fmt.Sprintf("part %d not found", rp.PartNumber))
							return
						}
						if normalizeETag(rp.ETag) != "" && normalizeETag(rp.ETag) != normalizeETag(stored.ETag) {
							mu.Unlock()
							writeS3Error(w, http.StatusBadRequest, "InvalidPart", fmt.Sprintf("part %d ETag mismatch", rp.PartNumber))
							return
						}
						orderedParts = append(orderedParts, stored)
					}
					sess.Completing = true
					mu.Unlock()

					tmpCombined := filepath.Join(absDir, internalDirName, "tmp", nextID("complete")+".tmp")
					outFile, cerr := os.OpenFile(tmpCombined, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
					if cerr != nil {
						mu.Lock()
						sess.Completing = false
						mu.Unlock()
						writeS3Error(w, http.StatusInternalServerError, "InternalError", cerr.Error())
						return
					}
					hasher := md5.New()
					var totalSize int64
					var copyErr error
					for _, pt := range orderedParts {
						pf, oerr := os.Open(pt.FilePath)
						if oerr != nil {
							copyErr = oerr
							break
						}
						n, cerr := io.Copy(io.MultiWriter(outFile, hasher), pf)
						_ = pf.Close()
						if cerr != nil {
							copyErr = cerr
							break
						}
						totalSize += n
					}
					_ = outFile.Close()
					if copyErr != nil {
						_ = os.Remove(tmpCombined)
						mu.Lock()
						sess.Completing = false
						mu.Unlock()
						writeS3Error(w, http.StatusInternalServerError, "InternalError", copyErr.Error())
						return
					}

					etag := `"` + hex.EncodeToString(hasher.Sum(nil)) + `"`
					mu.Lock()
					if err := os.MkdirAll(filepath.Dir(objPath), 0o755); err != nil {
						_ = os.Remove(tmpCombined)
						sess.Completing = false
						mu.Unlock()
						writeS3Error(w, http.StatusInternalServerError, "InternalError", err.Error())
						return
					}
					if err := os.Rename(tmpCombined, objPath); err != nil {
						_ = os.Remove(tmpCombined)
						sess.Completing = false
						mu.Unlock()
						writeS3Error(w, http.StatusInternalServerError, "InternalError", err.Error())
						return
					}
					_ = writeMeta(bucket, key, objectMeta{
						ContentType: "application/octet-stream",
						ETag:        etag,
						Size:        totalSize,
					})
					delete(mpSessions, upID)
					mu.Unlock()
					_ = os.RemoveAll(filepath.Join(absDir, internalDirName, "uploads", upID))

					w.Header().Set("Content-Type", "application/xml")
					_, _ = io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?><CompleteMultipartUploadResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Bucket>`+xmlEscape(bucket)+`</Bucket><Key>`+xmlEscape(key)+`</Key><ETag>`+etag+`</ETag></CompleteMultipartUploadResult>`)
					return
				}
				w.WriteHeader(http.StatusOK)

			case http.MethodPut:
				if pnStr := q.Get("partNumber"); pnStr != "" && q.Get("uploadId") != "" {
					upID := q.Get("uploadId")
					pn, err := strconv.Atoi(pnStr)
					if err != nil || pn < 1 || pn > 10000 {
						writeS3Error(w, http.StatusBadRequest, "InvalidArgument", "partNumber must be between 1 and 10000")
						return
					}
					mu.RLock()
					sess, ok := mpSessions[upID]
					validSess := ok && !sess.Completing && sess.Bucket == bucket && sess.Key == key
					mu.RUnlock()
					if !validSess {
						writeS3Error(w, http.StatusNotFound, "NoSuchUpload", "multipart upload session not found")
						return
					}

					tmpPart := filepath.Join(absDir, internalDirName, "tmp", nextID("part")+".tmp")
					size, etag, derr := streamAWSBodyToFile(tmpPart, r, maxUpload)
					if derr != nil {
						_ = os.Remove(tmpPart)
						status := http.StatusBadRequest
						code := "IncompleteBody"
						if errors.Is(derr, errUploadTooLarge) {
							status = http.StatusRequestEntityTooLarge
							code = "EntityTooLarge"
						}
						writeS3Error(w, status, code, derr.Error())
						return
					}

					finalPart := filepath.Join(absDir, internalDirName, "uploads", upID, fmt.Sprintf("part-%05d", pn))
					mu.Lock()
					sess, ok = mpSessions[upID]
					if !ok || sess.Completing {
						mu.Unlock()
						_ = os.Remove(tmpPart)
						writeS3Error(w, http.StatusNotFound, "NoSuchUpload", "multipart upload session closed")
						return
					}
					if err := os.Rename(tmpPart, finalPart); err != nil {
						mu.Unlock()
						_ = os.Remove(tmpPart)
						writeS3Error(w, http.StatusInternalServerError, "InternalError", err.Error())
						return
					}
					sess.Parts[pn] = multipartPart{
						PartNumber: pn,
						ETag:       etag,
						Size:       size,
						FilePath:   finalPart,
					}
					mu.Unlock()

					w.Header().Set("ETag", etag)
					w.WriteHeader(http.StatusOK)
					return
				}

				tmpPut := filepath.Join(absDir, internalDirName, "tmp", nextID("put")+".tmp")
				size, etag, derr := streamAWSBodyToFile(tmpPut, r, maxUpload)
				if derr != nil {
					_ = os.Remove(tmpPut)
					status := http.StatusBadRequest
					code := "IncompleteBody"
					if errors.Is(derr, errUploadTooLarge) {
						status = http.StatusRequestEntityTooLarge
						code = "EntityTooLarge"
					}
					writeS3Error(w, status, code, derr.Error())
					return
				}
				ct := r.Header.Get("Content-Type")
				if ct == "" {
					ct = "application/octet-stream"
				}

				mu.Lock()
				if err := os.MkdirAll(filepath.Dir(objPath), 0o755); err != nil {
					mu.Unlock()
					_ = os.Remove(tmpPut)
					writeS3Error(w, http.StatusInternalServerError, "InternalError", err.Error())
					return
				}
				if err := os.Rename(tmpPut, objPath); err != nil {
					mu.Unlock()
					_ = os.Remove(tmpPut)
					writeS3Error(w, http.StatusInternalServerError, "InternalError", err.Error())
					return
				}
				_ = writeMeta(bucket, key, objectMeta{
					ContentType: ct,
					ETag:        etag,
					Size:        size,
				})
				mu.Unlock()

				w.Header().Set("ETag", etag)
				w.WriteHeader(http.StatusOK)

			case http.MethodHead, http.MethodGet:
				// 仅在打开文件句柄与读取元数据时持有读锁，网络流传输前立即释放读锁（修复 Finding #12）
				mu.RLock()
				f, err := os.Open(objPath)
				if err != nil {
					mu.RUnlock()
					w.WriteHeader(http.StatusNotFound)
					return
				}
				st, err := f.Stat()
				if err != nil || st.IsDir() {
					_ = f.Close()
					mu.RUnlock()
					w.WriteHeader(http.StatusNotFound)
					return
				}
				m := readMeta(bucket, key)
				mu.RUnlock()

				defer f.Close()
				w.Header().Set("Content-Type", m.ContentType)
				w.Header().Set("ETag", m.ETag)
				w.Header().Set("Last-Modified", st.ModTime().UTC().Format(http.TimeFormat))
				http.ServeContent(w, r, filepath.Base(objPath), st.ModTime().UTC(), f)

			case http.MethodDelete:
				// AbortMultipartUpload：仅清理指定 uploadId 的临时分片，绝不删除已提交的同名对象（修复 Finding #4）
				if upID := q.Get("uploadId"); upID != "" {
					mu.Lock()
					sess, ok := mpSessions[upID]
					if !ok || sess.Completing || sess.Bucket != bucket || sess.Key != key {
						mu.Unlock()
						writeS3Error(w, http.StatusNotFound, "NoSuchUpload", "multipart upload session not found")
						return
					}
					delete(mpSessions, upID)
					mu.Unlock()
					_ = os.RemoveAll(filepath.Join(absDir, internalDirName, "uploads", upID))
					w.WriteHeader(http.StatusNoContent)
					return
				}

				mu.Lock()
				_ = os.Remove(objPath)
				_ = os.Remove(metaPathFor(bucket, key))
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

var errUploadTooLarge = errors.New("upload exceeds maximum allowed size")

func streamAWSBodyToFile(tmpPath string, r *http.Request, maxBytes int64) (int64, string, error) {
	if !strings.HasPrefix(r.Header.Get("x-amz-content-sha256"), "STREAMING-") && r.ContentLength > maxBytes {
		return 0, "", errUploadTooLarge
	}
	f, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return 0, "", err
	}
	defer f.Close()

	hasher := md5.New()
	dst := io.MultiWriter(f, hasher)

	if !strings.HasPrefix(r.Header.Get("x-amz-content-sha256"), "STREAMING-") {
		limited := io.LimitReader(r.Body, maxBytes+1)
		n, cerr := io.Copy(dst, limited)
		if cerr != nil {
			return 0, "", fmt.Errorf("read request body: %w", cerr)
		}
		if n > maxBytes {
			return 0, "", errUploadTooLarge
		}
		if r.ContentLength >= 0 && n != r.ContentLength {
			return 0, "", fmt.Errorf("truncated request body: got %d bytes, expected %d", n, r.ContentLength)
		}
		if err := f.Close(); err != nil {
			return 0, "", err
		}
		etag := `"` + hex.EncodeToString(hasher.Sum(nil)) + `"`
		return n, etag, nil
	}

	// AWS SigV4 STREAMING-AWS4-HMAC-SHA256-PAYLOAD 分块流解码与完整性校验
	br := bufio.NewReader(r.Body)
	var total int64
	sawTerminalZeroChunk := false
	for {
		hdr, herr := br.ReadString('\n')
		if herr != nil {
			return 0, "", fmt.Errorf("malformed aws-chunked header: %w", herr)
		}
		hdr = strings.TrimRight(hdr, "\r\n")
		if semi := strings.IndexByte(hdr, ';'); semi >= 0 {
			hdr = hdr[:semi]
		}
		chunkLen, perr := strconv.ParseInt(strings.TrimSpace(hdr), 16, 64)
		if perr != nil || chunkLen < 0 {
			return 0, "", fmt.Errorf("invalid aws-chunked size %q", hdr)
		}
		if chunkLen == 0 {
			sawTerminalZeroChunk = true
			// 消费结尾 CRLF
			_, _ = br.ReadString('\n')
			break
		}
		if total+chunkLen > maxBytes {
			return 0, "", errUploadTooLarge
		}
		written, cerr := io.CopyN(dst, br, chunkLen)
		if cerr != nil || written != chunkLen {
			return 0, "", fmt.Errorf("truncated aws-chunked payload: expected %d bytes, got %d (%v)", chunkLen, written, cerr)
		}
		total += written
		trailer, terr := br.ReadString('\n')
		if terr != nil || !strings.HasSuffix(trailer, "\n") {
			return 0, "", errors.New("missing CRLF after aws-chunked data")
		}
	}
	if !sawTerminalZeroChunk {
		return 0, "", errors.New("incomplete aws-chunked stream: missing terminal 0 chunk")
	}
	if err := f.Close(); err != nil {
		return 0, "", err
	}
	etag := `"` + hex.EncodeToString(hasher.Sum(nil)) + `"`
	return total, etag, nil
}

func isLoopbackHost(host string) bool {
	h := strings.TrimSpace(host)
	if strings.EqualFold(h, "localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

func normalizeETag(s string) string {
	return strings.Trim(strings.TrimSpace(s), `"`)
}

func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func xmlEscape(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

func writeS3Error(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>`+xmlEscape(code)+`</Code><Message>`+xmlEscape(msg)+`</Message></Error>`)
}

func parseLifecycleXML(raw []byte) []lifecycleRule {
	var cfg struct {
		Rules []struct {
			ID     string `xml:"ID"`
			Prefix string `xml:"Prefix"`
			Filter struct {
				Prefix string `xml:"Prefix"`
			} `xml:"Filter"`
			Status     string `xml:"Status"`
			Expiration struct {
				Days int `xml:"Days"`
			} `xml:"Expiration"`
		} `xml:"Rule"`
	}
	if err := xml.Unmarshal(raw, &cfg); err != nil {
		return nil
	}
	var out []lifecycleRule
	for _, r := range cfg.Rules {
		pfx := r.Prefix
		if pfx == "" {
			pfx = r.Filter.Prefix
		}
		out = append(out, lifecycleRule{
			ID:     r.ID,
			Prefix: pfx,
			Days:   r.Expiration.Days,
			Status: r.Status,
		})
	}
	return out
}

// verifyRequestSigV4 验证 AWS Signature Version 4（支持 Header Authorization 与 Presigned URL Query 参数）。
func verifyRequestSigV4(r *http.Request, accessKey, secretKey string, now time.Time) error {
	q := r.URL.Query()
	if gotSig := q.Get("X-Amz-Signature"); gotSig != "" {
		return verifyPresignedSigV4(r, q, gotSig, accessKey, secretKey, now)
	}
	authHdr := strings.TrimSpace(r.Header.Get("Authorization"))
	if authHdr != "" {
		return verifyHeaderSigV4(r, authHdr, accessKey, secretKey, now)
	}
	return errors.New("missing AWS SigV4 Authorization header or presigned X-Amz-Signature")
}

func verifyPresignedSigV4(r *http.Request, q url.Values, gotSig, accessKey, secretKey string, now time.Time) error {
	if q.Get("X-Amz-Algorithm") != "AWS4-HMAC-SHA256" {
		return errors.New("unsupported X-Amz-Algorithm")
	}
	cred := q.Get("X-Amz-Credential")
	credParts := strings.Split(cred, "/")
	if len(credParts) != 5 || credParts[0] != accessKey || credParts[3] != "s3" || credParts[4] != "aws4_request" {
		return errors.New("invalid X-Amz-Credential")
	}
	dateStamp, region, service := credParts[1], credParts[2], credParts[3]

	amzDate := q.Get("X-Amz-Date")
	if !strings.HasPrefix(amzDate, dateStamp) {
		return errors.New("X-Amz-Date does not match credential dateStamp")
	}
	signedAt, err := time.Parse("20060102T150405Z", amzDate)
	if err != nil {
		return errors.New("invalid X-Amz-Date")
	}
	expSec, err := strconv.ParseInt(q.Get("X-Amz-Expires"), 10, 64)
	if err != nil || expSec <= 0 || expSec > 604800 {
		return errors.New("invalid X-Amz-Expires")
	}
	if now.After(signedAt.Add(time.Duration(expSec) * time.Second)) {
		return errors.New("presigned URL has expired")
	}

	signedHeaders := strings.TrimSpace(q.Get("X-Amz-SignedHeaders"))
	if signedHeaders == "" {
		return errors.New("missing X-Amz-SignedHeaders")
	}
	canonHeaders, err := buildCanonicalHeaders(r, signedHeaders)
	if err != nil {
		return err
	}
	payloadHash := firstNonEmptyStr(q.Get("X-Amz-Content-Sha256"), r.Header.Get("X-Amz-Content-Sha256"), "UNSIGNED-PAYLOAD")

	canonReq := strings.Join([]string{
		r.Method,
		s3CanonicalURI(r.URL.EscapedPath()),
		s3CanonicalQueryString(q, "X-Amz-Signature"),
		canonHeaders,
		signedHeaders,
		payloadHash,
	}, "\n")

	scope := dateStamp + "/" + region + "/" + service + "/aws4_request"
	stringToSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + sha256Hex([]byte(canonReq))
	expectedSig := computeSigV4(secretKey, dateStamp, region, service, stringToSign)
	if !hmac.Equal([]byte(strings.ToLower(gotSig)), []byte(expectedSig)) {
		return errors.New("presigned signature mismatch")
	}
	return nil
}

func verifyHeaderSigV4(r *http.Request, authHdr, accessKey, secretKey string, now time.Time) error {
	const prefix = "AWS4-HMAC-SHA256 "
	if !strings.HasPrefix(authHdr, prefix) {
		return errors.New("unsupported Authorization scheme")
	}
	fields := make(map[string]string)
	for _, part := range strings.Split(authHdr[len(prefix):], ",") {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) == 2 {
			fields[kv[0]] = kv[1]
		}
	}
	credParts := strings.Split(fields["Credential"], "/")
	if len(credParts) != 5 || credParts[0] != accessKey || credParts[3] != "s3" || credParts[4] != "aws4_request" {
		return errors.New("invalid Credential in Authorization header")
	}
	dateStamp, region, service := credParts[1], credParts[2], credParts[3]
	signedHeaders := fields["SignedHeaders"]
	gotSig := fields["Signature"]
	if signedHeaders == "" || gotSig == "" {
		return errors.New("missing SignedHeaders or Signature")
	}

	amzDate := strings.TrimSpace(r.Header.Get("X-Amz-Date"))
	if amzDate == "" {
		return errors.New("missing X-Amz-Date header")
	}
	signedAt, err := time.Parse("20060102T150405Z", amzDate)
	if err != nil || !strings.HasPrefix(amzDate, dateStamp) {
		return errors.New("invalid X-Amz-Date header")
	}
	if now.Sub(signedAt) > 15*time.Minute || signedAt.Sub(now) > 15*time.Minute {
		return errors.New("request timestamp skewed beyond 15m")
	}

	canonHeaders, err := buildCanonicalHeaders(r, signedHeaders)
	if err != nil {
		return err
	}
	payloadHash := firstNonEmptyStr(r.Header.Get("X-Amz-Content-Sha256"), "UNSIGNED-PAYLOAD")

	canonReq := strings.Join([]string{
		r.Method,
		s3CanonicalURI(r.URL.EscapedPath()),
		s3CanonicalQueryString(r.URL.Query(), ""),
		canonHeaders,
		signedHeaders,
		payloadHash,
	}, "\n")

	scope := dateStamp + "/" + region + "/" + service + "/aws4_request"
	stringToSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + sha256Hex([]byte(canonReq))
	expectedSig := computeSigV4(secretKey, dateStamp, region, service, stringToSign)
	if !hmac.Equal([]byte(strings.ToLower(gotSig)), []byte(expectedSig)) {
		return errors.New("header signature mismatch")
	}
	return nil
}

func buildCanonicalHeaders(r *http.Request, signedHeaders string) (string, error) {
	var b strings.Builder
	for _, h := range strings.Split(signedHeaders, ";") {
		hLower := strings.ToLower(strings.TrimSpace(h))
		if hLower == "" {
			continue
		}
		var val string
		if hLower == "host" {
			val = strings.TrimSpace(r.Host)
		} else {
			vals := r.Header.Values(hLower)
			if len(vals) == 0 {
				return "", fmt.Errorf("missing signed header %q", hLower)
			}
			trimmed := make([]string, 0, len(vals))
			for _, v := range vals {
				trimmed = append(trimmed, strings.Join(strings.Fields(strings.TrimSpace(v)), " "))
			}
			val = strings.Join(trimmed, ",")
		}
		b.WriteString(hLower)
		b.WriteByte(':')
		b.WriteString(val)
		b.WriteByte('\n')
	}
	return b.String(), nil
}

func s3CanonicalURI(escapedPath string) string {
	if escapedPath == "" {
		return "/"
	}
	if !strings.HasPrefix(escapedPath, "/") {
		return "/" + escapedPath
	}
	return escapedPath
}

func s3CanonicalQueryString(q url.Values, excludeKey string) string {
	var pairs []string
	keys := make([]string, 0, len(q))
	for k := range q {
		if k == excludeKey {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		vals := append([]string(nil), q[k]...)
		sort.Strings(vals)
		encK := s3PercentEncode(k)
		for _, v := range vals {
			pairs = append(pairs, encK+"="+s3PercentEncode(v))
		}
	}
	return strings.Join(pairs, "&")
}

func s3PercentEncode(s string) string {
	enc := url.QueryEscape(s)
	enc = strings.ReplaceAll(enc, "+", "%20")
	enc = strings.ReplaceAll(enc, "%7E", "~")
	return enc
}

func hmacSHA256(key []byte, data string) []byte {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(data))
	return mac.Sum(nil)
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func computeSigV4(secretKey, dateStamp, region, service, stringToSign string) string {
	kDate := hmacSHA256([]byte("AWS4"+secretKey), dateStamp)
	kRegion := hmacSHA256(kDate, region)
	kService := hmacSHA256(kRegion, service)
	kSigning := hmacSHA256(kService, "aws4_request")
	return hex.EncodeToString(hmacSHA256(kSigning, stringToSign))
}
