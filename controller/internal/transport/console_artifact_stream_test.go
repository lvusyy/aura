package transport

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// fakeOpener 是 artifactOpener 的内存实现（免 MinIO 直测 handler 的鉴权/白名单/流式分支）。
type fakeOpener struct {
	data []byte
	err  error
}

func (f *fakeOpener) OpenObject(_ context.Context, _ string) (io.ReadSeekCloser, time.Time, error) {
	if f.err != nil {
		return nil, time.Time{}, f.err
	}
	return nopSeekCloser{bytes.NewReader(f.data)}, time.Unix(1_600_000_000, 0), nil
}

// nopSeekCloser 给 bytes.Reader（已实现 ReadSeeker）补空 Close，凑成 io.ReadSeekCloser。
type nopSeekCloser struct{ io.ReadSeeker }

func (nopSeekCloser) Close() error { return nil }

// TestArtifactStreamHandler_RejectsBadToken：无 token / 错 token 均 401（?token= 承载 bearer，常量时比对）。
func TestArtifactStreamHandler_RejectsBadToken(t *testing.T) {
	h := ArtifactStreamHandler(&fakeOpener{data: []byte("MP4DATA")}, SingleToken("secret"))
	for _, q := range []string{"", "?token=", "?token=wrong"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/artifact/recordings/x.mp4"+q, nil))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("token %q: want 401, got %d", q, rec.Code)
		}
	}
}

// TestArtifactStreamHandler_RejectsBadKey：token 有效但 key 越白名单（.. 穿越 / 非白名单前缀）→ 400。
func TestArtifactStreamHandler_RejectsBadKey(t *testing.T) {
	h := ArtifactStreamHandler(&fakeOpener{data: []byte("MP4DATA")}, SingleToken("secret"))
	for _, path := range []string{"/artifact/recordings/../secret.env", "/artifact/other/x.mp4"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path+"?token=secret", nil))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("path %q: want 400, got %d", path, rec.Code)
		}
	}
}

// TestArtifactStreamHandler_ServesFull：token 有效 + key 合法 + 无 Range → 200 全量、Content-Type video/mp4。
func TestArtifactStreamHandler_ServesFull(t *testing.T) {
	data := []byte("MP4DATA")
	h := ArtifactStreamHandler(&fakeOpener{data: data}, SingleToken("secret"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/artifact/recordings/x.mp4?token=secret", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "video/mp4" {
		t.Fatalf("Content-Type: want video/mp4, got %q", got)
	}
	if !bytes.Equal(rec.Body.Bytes(), data) {
		t.Fatalf("body: want %q, got %q", data, rec.Body.Bytes())
	}
}

// TestArtifactStreamHandler_ServesRange：Range 请求 → 206 partial + 只回请求区间字节（流式回放拖动 seek
// 的底座：http.ServeContent 消费 ReadSeeker 自动切片，不整对象入内存）。
func TestArtifactStreamHandler_ServesRange(t *testing.T) {
	h := ArtifactStreamHandler(&fakeOpener{data: []byte("MP4DATA")}, SingleToken("secret"))
	req := httptest.NewRequest(http.MethodGet, "/artifact/recordings/x.mp4?token=secret", nil)
	req.Header.Set("Range", "bytes=0-2")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusPartialContent {
		t.Fatalf("want 206, got %d", rec.Code)
	}
	if got := rec.Body.String(); got != "MP4" {
		t.Fatalf("range body: want %q, got %q", "MP4", got)
	}
	if cr := rec.Header().Get("Content-Range"); cr != "bytes 0-2/7" {
		t.Fatalf("Content-Range: want %q, got %q", "bytes 0-2/7", cr)
	}
}

// TestArtifactStreamHandler_OpenError：token/key 均合法但对象打开失败（不存在/过期删除）→ 404。
func TestArtifactStreamHandler_OpenError(t *testing.T) {
	h := ArtifactStreamHandler(&fakeOpener{err: io.EOF}, SingleToken("secret"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/artifact/recordings/gone.mp4?token=secret", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", rec.Code)
	}
}
