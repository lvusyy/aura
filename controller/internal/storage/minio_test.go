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
		switch r.Method {
		case http.MethodPut:
			body := decodeS3Body(r)
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
	want := []byte("hello-s3-roundtrip")
	if err := store.PutObject(ctx, "releases/linux-x86_64/0.4.1/aura-node", want, "application/octet-stream"); err != nil {
		t.Fatalf("PutObject: %v", err)
	}
	got, _, err := store.GetObject(ctx, "releases/linux-x86_64/0.4.1/aura-node")
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("GetObject = %q, want %q", got, want)
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
		t.Fatalf("PresignedGet download = %q, want %q", dlBytes, want)
	}
}
