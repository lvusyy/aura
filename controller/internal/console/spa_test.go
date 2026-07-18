package console

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestHandler_ServesIndex 验证前缀根 /console/ 返回 index.html（200 HTML）。
func TestHandler_ServesIndex(t *testing.T) {
	h, err := Handler()
	if err != nil {
		t.Fatalf("Handler(): %v", err)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/console/", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /console/: want 200, got %d", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "<html") {
		t.Fatalf("GET /console/ body should be HTML, got %q", body)
	}
}

// TestHandler_FallbackToIndex 验证前缀内未知路径（SPA 深链，如 /console/replay/<id>）回退 index.html
// （200），而非 404——令前端路由深链刷新不 404。
func TestHandler_FallbackToIndex(t *testing.T) {
	h, err := Handler()
	if err != nil {
		t.Fatalf("Handler(): %v", err)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/console/replay/deep-link-42", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("SPA deep-link fallback: want 200, got %d", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "<html") {
		t.Fatalf("fallback body should be index.html HTML, got %q", body)
	}
}

// TestHandler_RedirectsRoot 验证前缀外路径（根 /）302 重定向到规范入口 /console/。
func TestHandler_RedirectsRoot(t *testing.T) {
	h, err := Handler()
	if err != nil {
		t.Fatalf("Handler(): %v", err)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusFound {
		t.Fatalf("GET /: want 302, got %d", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/console/" {
		t.Fatalf("GET / redirect Location: want /console/, got %q", loc)
	}
}

// TestHandler_IndexNoCache 验证 index.html 服务路径（根 /console/ 与 SPA 深链回退）响应带 Cache-Control:
// no-cache——回归防护：浏览器启发式缓存旧 index.html 会继续引用旧 hash 命名 bundle，令前端新版刷新不生效。
func TestHandler_IndexNoCache(t *testing.T) {
	h, err := Handler()
	if err != nil {
		t.Fatalf("Handler(): %v", err)
	}
	// 两条 index.html 服务路径：根前缀 + 前缀内未命中文件的深链回退，均须 no-cache。
	for _, p := range []string{"/console/", "/console/replay/deep-link-42"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
		if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "no-cache") {
			t.Fatalf("GET %s Cache-Control: want contains no-cache, got %q", p, cc)
		}
	}
}

// TestHandler_AssetsImmutableCache 验证 /console/assets/* 内容 hash 命名资源响应带 Cache-Control: immutable
// 长缓存（hash 变则 URL 变自动拉新，长缓存零陈旧风险）。资源文件名逐构建变，故从嵌入 dist 取真实名避免硬编码脆化。
func TestHandler_AssetsImmutableCache(t *testing.T) {
	h, err := Handler()
	if err != nil {
		t.Fatalf("Handler(): %v", err)
	}
	entries, err := distFS.ReadDir("dist/assets")
	if err != nil {
		t.Fatalf("read embedded dist/assets: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("embedded dist/assets is empty, expected hashed bundle assets")
	}
	asset := "/console/assets/" + entries[0].Name()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, asset, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s: want 200, got %d", asset, rec.Code)
	}
	if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "immutable") {
		t.Fatalf("GET %s Cache-Control: want contains immutable, got %q", asset, cc)
	}
}
