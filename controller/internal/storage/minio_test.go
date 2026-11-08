package storage

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
)

func decodeS3Body(r *http.Request) []byte {
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
		semi := strings.IndexByte(hdr, ';')
		if semi > 0 {
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

// TestParseZoneEndpoints 证 AURA_MINIO_ENDPOINTS 解析：zone=endpoint 逗号分隔、空项跳过、
// 首个等号分隔（endpoint host:port 不含等号无歧义）；格式非法 fail fast。
func TestParseZoneEndpoints(t *testing.T) {
	// 合法：多域 + 前后空白 + 空项跳过。
	got, err := parseZoneEndpoints(" lan=192.168.22.240:9000 , jump=100.78.22.127:9000 , ")
	if err != nil {
		t.Fatalf("parseZoneEndpoints valid: unexpected error %v", err)
	}
	want := map[string]string{
		"lan":  "192.168.22.240:9000",
		"jump": "100.78.22.127:9000",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseZoneEndpoints = %v, want %v", got, want)
	}

	// 非法项 fail fast（缺等号 / 空 zone / 空 endpoint）。
	for _, bad := range []string{"noequal", "=192.168.22.240:9000", "lan=", "lan=1.2.3.4:9000,badpair"} {
		if _, err := parseZoneEndpoints(bad); err == nil {
			t.Errorf("parseZoneEndpoints(%q): expected error, got nil", bad)
		}
	}
}

// TestPresignedPutDispatchesByZone 证 GrantUpload 分派地基：AURA_MINIO_ENDPOINTS 建多 zone client，
// PresignedPut 按 zone 选对应端点签发（URL host 即该域端点），空/未知 zone 回落默认 zone
//（AURA_MINIO_PUBLIC_ENDPOINT）。预签名为离线签发（固定 region），无需真 MinIO 后端。
func TestPresignedPutDispatchesByZone(t *testing.T) {
	t.Setenv("AURA_MINIO_ENDPOINT", "127.0.0.1:9000")
	t.Setenv("AURA_MINIO_ACCESS_KEY", "testak")
	t.Setenv("AURA_MINIO_SECRET_KEY", "testsk")
	t.Setenv("AURA_MINIO_PUBLIC_ENDPOINT", "192.168.22.240:9000")
	t.Setenv("AURA_MINIO_ENDPOINTS", "lan=192.168.22.240:9000,jump=100.78.22.127:9000")

	store, err := NewMinioStoreFromEnv()
	if err != nil {
		t.Fatalf("NewMinioStoreFromEnv: %v", err)
	}
	if store == nil {
		t.Fatal("NewMinioStoreFromEnv: got nil store (AURA_MINIO_ENDPOINT set)")
	}

	ctx := context.Background()
	cases := []struct {
		name     string
		zone     string
		wantHost string
	}{
		{"lan 域→直连端点", "lan", "192.168.22.240:9000"},
		{"jump 域→跳板端点（收口 ISS-20260714-003）", "jump", "100.78.22.127:9000"},
		{"空域→默认 zone（PUBLIC_ENDPOINT）", "", "192.168.22.240:9000"},
		{"未知域→回落默认 zone", "unknown-zone", "192.168.22.240:9000"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			u, err := store.PresignedPut(ctx, "test/key.bin", time.Minute, c.zone)
			if err != nil {
				t.Fatalf("PresignedPut(zone=%q): %v", c.zone, err)
			}
			if u.Host != c.wantHost {
				t.Errorf("PresignedPut(zone=%q): host = %q, want %q", c.zone, u.Host, c.wantHost)
			}
		})
	}
}

// TestPresignedPutFallsBackWithoutEndpoints 证向后兼容：仅 AURA_MINIO_PUBLIC_ENDPOINT（无 ENDPOINTS）时
// 任意 zone 均回落默认端点——既有单端点部署零行为变化。
func TestPresignedPutFallsBackWithoutEndpoints(t *testing.T) {
	t.Setenv("AURA_MINIO_ENDPOINT", "127.0.0.1:9000")
	t.Setenv("AURA_MINIO_ACCESS_KEY", "testak")
	t.Setenv("AURA_MINIO_SECRET_KEY", "testsk")
	t.Setenv("AURA_MINIO_PUBLIC_ENDPOINT", "192.168.22.240:9000")

	store, err := NewMinioStoreFromEnv()
	if err != nil {
		t.Fatalf("NewMinioStoreFromEnv: %v", err)
	}
	ctx := context.Background()
	for _, zone := range []string{"", "lan", "jump", "anything"} {
		u, err := store.PresignedPut(ctx, "test/key.bin", time.Minute, zone)
		if err != nil {
			t.Fatalf("PresignedPut(zone=%q): %v", zone, err)
		}
		if u.Host != "192.168.22.240:9000" {
			t.Errorf("PresignedPut(zone=%q): host = %q, want 192.168.22.240:9000 (无 ENDPOINTS 全回落默认)", zone, u.Host)
		}
	}
}

func TestMinioStoreFakeS3RoundTrip(t *testing.T) {
	var mu sync.Mutex
	objs := make(map[string][]byte)
	mpParts := make(map[string]map[int][]byte)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/")
		parts := strings.SplitN(path, "/", 2)
		if len(parts) == 1 || parts[1] == "" {
			if _, ok := r.URL.Query()["location"]; ok {
				w.Header().Set("Content-Type", "application/xml")
				_, _ = io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?><LocationConstraint xmlns="http://s3.amazonaws.com/doc/2006-03-01/">us-east-1</LocationConstraint>`)
				return
			}
			w.WriteHeader(http.StatusOK)
			return
		}
		key := parts[0] + "/" + parts[1]
		q := r.URL.Query()
		switch r.Method {
		case http.MethodPost:
			if _, ok := q["uploads"]; ok {
				mu.Lock()
				mpParts[key] = make(map[int][]byte)
				mu.Unlock()
				w.Header().Set("Content-Type", "application/xml")
				_, _ = io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?><InitiateMultipartUploadResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Bucket>`+parts[0]+`</Bucket><Key>`+parts[1]+`</Key><UploadId>u1</UploadId></InitiateMultipartUploadResult>`)
				return
			}
			if q.Get("uploadId") != "" {
				_, _ = io.ReadAll(r.Body)
				mu.Lock()
				pm := mpParts[key]
				var combined bytes.Buffer
				for i := 1; i <= len(pm); i++ {
					combined.Write(pm[i])
				}
				objs[key] = combined.Bytes()
				delete(mpParts, key)
				mu.Unlock()
				w.Header().Set("Content-Type", "application/xml")
				_, _ = io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?><CompleteMultipartUploadResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Bucket>`+parts[0]+`</Bucket><Key>`+parts[1]+`</Key><ETag>"fake-etag"</ETag></CompleteMultipartUploadResult>`)
				return
			}
			w.WriteHeader(http.StatusOK)
		case http.MethodPut:
			body := decodeS3Body(r)
			if pnStr := q.Get("partNumber"); pnStr != "" {
				pn, _ := strconv.Atoi(pnStr)
				mu.Lock()
				if mpParts[key] == nil {
					mpParts[key] = make(map[int][]byte)
				}
				mpParts[key][pn] = body
				mu.Unlock()
				w.Header().Set("ETag", `"part-etag"`)
				w.WriteHeader(http.StatusOK)
				return
			}
			mu.Lock()
			objs[key] = body
			mu.Unlock()
			w.Header().Set("ETag", `"fake-etag"`)
			w.WriteHeader(http.StatusOK)
		case http.MethodHead, http.MethodGet:
			mu.Lock()
			body, ok := objs[key]
			mu.Unlock()
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("ETag", `"fake-etag"`)
			w.Header().Set("Last-Modified", time.Now().UTC().Format(http.TimeFormat))
			http.ServeContent(w, r, parts[1], time.Now().UTC(), bytes.NewReader(body))
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()

	u, _ := url.Parse(srv.URL)
	store, err := NewMinioStore(u.Host, "aura", "aura-secret", false)
	if err != nil {
		t.Fatalf("NewMinioStore: %v", err)
	}
	ctx := context.Background()
	if err := store.EnsureBucket(ctx); err != nil {
		t.Fatalf("EnsureBucket: %v", err)
	}
	if err := store.EnsureRecordingRetention(ctx, 7); err != nil {
		t.Fatalf("EnsureRecordingRetention: %v", err)
	}
	// 20 MiB 触发 minio-go Multipart Upload 分片上传路径（模拟 >16MiB 的 aura-node 二进制）
	want := bytes.Repeat([]byte("AURA-20MB-TEST-"), 1400000)
	if err := store.PutObject(ctx, "releases/linux-x86_64/0.4.1/aura-node", want, "application/octet-stream"); err != nil {
		t.Fatalf("PutObject: %v", err)
	}
	got, _, err := store.GetObject(ctx, "releases/linux-x86_64/0.4.1/aura-node")
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("GetObject len = %d, want %d", len(got), len(want))
	}
	dlURL, err := store.PresignedGet(ctx, "releases/linux-x86_64/0.4.1/aura-node", time.Minute)
	if err != nil {
		t.Fatalf("PresignedGet: %v", err)
	}
	resp, err := http.Get(dlURL.String())
	if err != nil {
		t.Fatalf("GET PresignedGet URL: %v", err)
	}
	defer resp.Body.Close()
	dlBytes, _ := io.ReadAll(resp.Body)
	if !bytes.Equal(dlBytes, want) {
		t.Fatalf("PresignedGet download len = %d, want %d", len(dlBytes), len(want))
	}
}

func TestLocalDiskS3ServerPersistenceAndListRecordings(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("AURA_MINIO_ENDPOINT", "local")
	t.Setenv("AURA_ARTIFACT_DIR", tmpDir)

	store, err := NewMinioStoreFromEnv()
	if err != nil {
		t.Fatalf("NewMinioStoreFromEnv(local): %v", err)
	}
	if store == nil {
		t.Fatal("expected non-nil store when AURA_MINIO_ENDPOINT=local")
	}
	defer store.Close()

	ctx := context.Background()
	if err := store.EnsureBucket(ctx); err != nil {
		t.Fatalf("EnsureBucket: %v", err)
	}
	if err := store.EnsureRecordingRetention(ctx, 30); err != nil {
		t.Fatalf("EnsureRecordingRetention: %v", err)
	}

	mp4Data := []byte("FAKE-MP4-STREAM-CONTENT-0123456789")
	if err := store.PutObject(ctx, "recordings/rec-local-1.mp4", mp4Data, "video/mp4"); err != nil {
		t.Fatalf("PutObject recording: %v", err)
	}

	recs, err := store.ListRecordings(ctx)
	if err != nil {
		t.Fatalf("ListRecordings: %v", err)
	}
	if len(recs) != 1 || recs[0].Key != "recordings/rec-local-1.mp4" || recs[0].Size != int64(len(mp4Data)) {
		t.Fatalf("unexpected ListRecordings result: %+v", recs)
	}

	rc, _, err := store.OpenObject(ctx, "recordings/rec-local-1.mp4")
	if err != nil {
		t.Fatalf("OpenObject: %v", err)
	}
	defer rc.Close()
	if _, err := rc.Seek(5, io.SeekStart); err != nil {
		t.Fatalf("Seek: %v", err)
	}
	tail, err := io.ReadAll(rc)
	if err != nil || string(tail) != string(mp4Data[5:]) {
		t.Fatalf("Range Seek read = %q, want %q (err=%v)", string(tail), string(mp4Data[5:]), err)
	}
}

func TestLocalDiskS3SecurityAndHardening(t *testing.T) {
	tmpDir := t.TempDir()

	// 1. 默认拒绝非回环监听地址，除非显式 AllowRemote=true
	if _, _, err := StartLocalS3ServerWithOptions(LocalS3Options{
		DataDir:    tmpDir,
		ListenAddr: "0.0.0.0:0",
		AccessKey:  "ak",
		SecretKey:  "sk-secret-123",
	}); err == nil {
		t.Fatal("expected non-loopback ListenAddr without AllowRemote to be rejected")
	}

	ep, closeFn, err := StartLocalS3ServerWithOptions(LocalS3Options{
		DataDir:   tmpDir,
		AccessKey: "test-ak",
		SecretKey: "test-sk-secret",
	})
	if err != nil {
		t.Fatalf("StartLocalS3ServerWithOptions: %v", err)
	}
	defer closeFn()

	store, err := NewMinioStore(ep, "test-ak", "test-sk-secret", false)
	if err != nil {
		t.Fatalf("NewMinioStore: %v", err)
	}
	ctx := context.Background()
	if err := store.EnsureBucket(ctx); err != nil {
		t.Fatalf("EnsureBucket: %v", err)
	}
	if err := store.PutObject(ctx, "recordings/sec.mp4", []byte("secret-video"), "video/mp4"); err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	// 2. 未签名请求（GET / PUT / DELETE）必须返回 403 Forbidden
	baseURL := "http://" + ep + "/aura-artifacts/recordings/sec.mp4"
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		req, _ := http.NewRequest(method, baseURL, strings.NewReader("pwned"))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("unauthenticated %s error: %v", method, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("unauthenticated %s status = %d, want 403", method, resp.StatusCode)
		}
	}

	// 3. 篡改签名的预签名 URL 必须返回 403 Forbidden
	preGet, err := store.PresignedGet(ctx, "recordings/sec.mp4", time.Minute)
	if err != nil {
		t.Fatalf("PresignedGet: %v", err)
	}
	tampered := *preGet
	q := tampered.Query()
	q.Set("X-Amz-Signature", strings.Repeat("0", 64))
	tampered.RawQuery = q.Encode()
	resp, err := http.Get(tampered.String())
	if err != nil {
		t.Fatalf("GET tampered presigned URL: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("tampered presigned URL status = %d, want 403", resp.StatusCode)
	}

	// 4. 禁止写入 .aura-internal 内部元数据命名空间，且相邻后缀键 (.ct / .tmp) 互不污染
	if err := store.PutObject(ctx, ".aura-internal/hack", []byte("bad"), "text/plain"); err == nil {
		t.Fatal("expected writing to .aura-internal to fail")
	}
	for _, k := range []string{"siblings/item", "siblings/item.ct", "siblings/item.tmp"} {
		if err := store.PutObject(ctx, k, []byte("val:"+k), "text/plain"); err != nil {
			t.Fatalf("PutObject(%q): %v", k, err)
		}
	}
	for _, k := range []string{"siblings/item", "siblings/item.ct", "siblings/item.tmp"} {
		got, _, err := store.GetObject(ctx, k)
		if err != nil || string(got) != "val:"+k {
			t.Fatalf("GetObject(%q) = %q, want %q (err=%v)", k, string(got), "val:"+k, err)
		}
	}
}

func TestLocalDiskS3MultipartIsolationQuotaAndPagination(t *testing.T) {
	// 1. 配额保护测试：设置 MaxUploadBytes=128，验证超额写入返回 413 且不破坏已提交对象
	quotaDir := t.TempDir()
	qEp, qClose, err := StartLocalS3ServerWithOptions(LocalS3Options{
		DataDir:        quotaDir,
		AccessKey:      "test-ak",
		SecretKey:      "test-sk-secret",
		MaxUploadBytes: 128,
	})
	if err != nil {
		t.Fatalf("StartLocalS3ServerWithOptions(quota): %v", err)
	}
	defer qClose()

	qStore, err := NewMinioStore(qEp, "test-ak", "test-sk-secret", false)
	if err != nil {
		t.Fatalf("NewMinioStore(quota): %v", err)
	}
	ctx := context.Background()
	if err := qStore.EnsureBucket(ctx); err != nil {
		t.Fatalf("EnsureBucket(quota): %v", err)
	}
	committed := []byte("COMMITTED-V1-PAYLOAD")
	if err := qStore.PutObject(ctx, "recordings/atomic.mp4", committed, "video/mp4"); err != nil {
		t.Fatalf("initial PutObject: %v", err)
	}
	prePut, err := qStore.PresignedPut(ctx, "recordings/atomic.mp4", time.Minute, "")
	if err != nil {
		t.Fatalf("PresignedPut: %v", err)
	}
	overReq, _ := http.NewRequest(http.MethodPut, prePut.String(), bytes.NewReader(bytes.Repeat([]byte("X"), 256)))
	overResp, err := http.DefaultClient.Do(overReq)
	if err != nil {
		t.Fatalf("over-quota PUT: %v", err)
	}
	overResp.Body.Close()
	if overResp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("over-quota PUT status = %d, want 413", overResp.StatusCode)
	}
	gotAfterOver, _, err := qStore.GetObject(ctx, "recordings/atomic.mp4")
	if err != nil || !bytes.Equal(gotAfterOver, committed) {
		t.Fatalf("committed object corrupted after over-quota PUT: got %q, want %q", string(gotAfterOver), string(committed))
	}

	// 2. Multipart 大文件分片上传 + >1000 对象的 ListObjectsV2 分页完整遍历
	tmpDir := t.TempDir()
	ep, closeFn, err := StartLocalS3ServerWithOptions(LocalS3Options{
		DataDir:   tmpDir,
		AccessKey: "test-ak",
		SecretKey: "test-sk-secret",
	})
	if err != nil {
		t.Fatalf("StartLocalS3ServerWithOptions: %v", err)
	}
	defer closeFn()

	store, err := NewMinioStore(ep, "test-ak", "test-sk-secret", false)
	if err != nil {
		t.Fatalf("NewMinioStore: %v", err)
	}
	if err := store.EnsureBucket(ctx); err != nil {
		t.Fatalf("EnsureBucket: %v", err)
	}

	// 20 MiB 触发真实 Local S3 的 Multipart Upload 会话创建、Part ETag 校验与 Complete 合并
	bigPayload := bytes.Repeat([]byte("LOCAL-S3-MP-"), 1750000)
	if err := store.PutObject(ctx, "recordings/multipart.mp4", bigPayload, "video/mp4"); err != nil {
		t.Fatalf("Multipart PutObject on LocalS3: %v", err)
	}
	gotBig, _, err := store.GetObject(ctx, "recordings/multipart.mp4")
	if err != nil || !bytes.Equal(gotBig, bigPayload) {
		t.Fatalf("Multipart GetObject len = %d, want %d (err=%v)", len(gotBig), len(bigPayload), err)
	}

	const totalRecs = 1005
	for i := 0; i < totalRecs; i++ {
		key := "recordings/batch-" + strconv.Itoa(i) + ".mp4"
		if err := store.PutObject(ctx, key, []byte("m"), "video/mp4"); err != nil {
			t.Fatalf("PutObject #%d: %v", i, err)
		}
	}
	// 写入字典序排在最后但修改时间最新的录屏对象，验证跨 1000 键分页扫描后最新录屏排在首位
	time.Sleep(10 * time.Millisecond)
	if err := store.PutObject(ctx, "recordings/zz-latest.mp4", []byte("latest"), "video/mp4"); err != nil {
		t.Fatalf("PutObject zz-latest: %v", err)
	}

	// 直接验证 S3 ListObjectsV2 跨 1000 键边界完整分页（1005 + multipart + zz-latest = 1007）
	rawCount := 0
	for obj := range store.client.ListObjects(ctx, store.bucket, minio.ListObjectsOptions{Prefix: "recordings/"}) {
		if obj.Err != nil {
			t.Fatalf("ListObjects pagination err: %v", obj.Err)
		}
		rawCount++
	}
	if rawCount != totalRecs+2 {
		t.Fatalf("S3 ListObjectsV2 paginated count = %d, want %d", rawCount, totalRecs+2)
	}

	recs, err := store.ListRecordings(ctx)
	if err != nil {
		t.Fatalf("ListRecordings over >1000 objects: %v", err)
	}
	if len(recs) != maxRecordingsList {
		t.Fatalf("ListRecordings count = %d, want %d", len(recs), maxRecordingsList)
	}
	if recs[0].Key != "recordings/zz-latest.mp4" {
		t.Fatalf("newest recording key = %q, want recordings/zz-latest.mp4 (cross-page sort failed)", recs[0].Key)
	}
}

func TestLocalDiskS3DigestAndCaseInsensitiveInternalPath(t *testing.T) {
	addr, closeFn, err := StartLocalS3ServerWithOptions(LocalS3Options{
		DataDir:    t.TempDir(),
		ListenAddr: "127.0.0.1:0",
		AccessKey:  "test-ak",
		SecretKey:  "test-secret-key",
	})
	if err != nil {
		t.Fatalf("StartLocalS3ServerWithOptions: %v", err)
	}
	defer closeFn()

	ctx := context.Background()
	store, err := NewMinioStore(addr, "test-ak", "test-secret-key", false)
	if err != nil {
		t.Fatalf("NewMinioStore: %v", err)
	}
	if err := store.EnsureBucket(ctx); err != nil {
		t.Fatalf("EnsureBucket: %v", err)
	}

	// 1) 验证 .AURA-INTERNAL 大小写变体在路径上被拒绝（防止 Windows 大小写不敏感文件系统绕过内部目录隔离）
	amzDate := time.Now().UTC().Format("20060102T150405Z")
	dateStamp := amzDate[:8]
	scope := dateStamp + "/us-east-1/s3/aws4_request"
	emptySha := sha256Hex(nil)
	reqInternalCase, _ := http.NewRequest(http.MethodGet, "http://"+addr+"/aura-artifacts/.AURA-INTERNAL/meta/test.json", nil)
	reqInternalCase.Header.Set("X-Amz-Date", amzDate)
	reqInternalCase.Header.Set("X-Amz-Content-Sha256", emptySha)
	canonGet := strings.Join([]string{
		http.MethodGet,
		"/aura-artifacts/.AURA-INTERNAL/meta/test.json",
		"",
		"host:" + addr + "\n" + "x-amz-content-sha256:" + emptySha + "\n" + "x-amz-date:" + amzDate + "\n",
		"host;x-amz-content-sha256;x-amz-date",
		emptySha,
	}, "\n")
	sigGet := computeSigV4("test-secret-key", dateStamp, "us-east-1", "s3", "AWS4-HMAC-SHA256\n"+amzDate+"\n"+scope+"\n"+sha256Hex([]byte(canonGet)))
	reqInternalCase.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=test-ak/"+scope+", SignedHeaders=host;x-amz-content-sha256;x-amz-date, Signature="+sigGet)
	respInternalCase, err := http.DefaultClient.Do(reqInternalCase)
	if err != nil {
		t.Fatalf("GET .AURA-INTERNAL failed: %v", err)
	}
	_ = respInternalCase.Body.Close()
	if respInternalCase.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected .AURA-INTERNAL path to be rejected with 400 BadRequest, got HTTP %d", respInternalCase.StatusCode)
	}

	// 2) 验证带签名 Header 的非流式 PUT 若请求体内容被篡改（与已签名的 X-Amz-Content-Sha256 不一致），服务端返回 400 BadDigest 拒绝落盘
	claimedHex := sha256Hex([]byte("signed-authentic-payload"))
	tamperedBody := []byte("tampered-corrupt-payload")
	reqBadDigest, _ := http.NewRequest(http.MethodPut, "http://"+addr+"/aura-artifacts/traces/bad-digest.bin", bytes.NewReader(tamperedBody))
	reqBadDigest.ContentLength = int64(len(tamperedBody))
	reqBadDigest.Header.Set("X-Amz-Date", amzDate)
	reqBadDigest.Header.Set("X-Amz-Content-Sha256", claimedHex)
	canonHeaders := "host:" + addr + "\n" + "x-amz-content-sha256:" + claimedHex + "\n" + "x-amz-date:" + amzDate + "\n"
	signedHeaders := "host;x-amz-content-sha256;x-amz-date"
	canonReq := strings.Join([]string{
		http.MethodPut,
		"/aura-artifacts/traces/bad-digest.bin",
		"",
		canonHeaders,
		signedHeaders,
		claimedHex,
	}, "\n")
	strToSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + sha256Hex([]byte(canonReq))
	sig := computeSigV4("test-secret-key", dateStamp, "us-east-1", "s3", strToSign)
	reqBadDigest.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=test-ak/"+scope+", SignedHeaders="+signedHeaders+", Signature="+sig)
	respBadDigest, err := http.DefaultClient.Do(reqBadDigest)
	if err != nil {
		t.Fatalf("PUT with mismatched X-Amz-Content-Sha256 failed: %v", err)
	}
	badDigestBody, _ := io.ReadAll(respBadDigest.Body)
	_ = respBadDigest.Body.Close()
	if respBadDigest.StatusCode != http.StatusBadRequest || !strings.Contains(string(badDigestBody), "BadDigest") {
		t.Fatalf("expected 400 BadDigest on payload SHA256 mismatch, got HTTP %d (%s)", respBadDigest.StatusCode, string(badDigestBody))
	}
}
