package transport

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestReleaseUploadHandlerValidation 验证上传端点的入参门（不触后端，MinIO/PG 全路径归 e2e）：
// 非 POST 405；platform/version 含路径逃逸字符 400（白名单收口——拼对象键，放行斜杠即前缀逃逸）；
// 空体 400。裸 ctx（无 bearer middleware 注入）空 scope 视同 admin，与既有兼容语义一致。
func TestReleaseUploadHandlerValidation(t *testing.T) {
	h := ReleaseUploadHandler(nil, nil)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, releasesPath, nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET want 405, got %d", rec.Code)
	}

	for _, bad := range []string{"../escape", "a/b", "", ".hidden"} {
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost,
			releasesPath+"?platform="+bad+"&version=0.3.0", strings.NewReader("x")))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("platform %q want 400, got %d", bad, rec.Code)
		}
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost,
		releasesPath+"?platform=linux-x86_64&version=0.3.0", strings.NewReader("")))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty body want 400, got %d", rec.Code)
	}
}

// TestReleaseUploadHandlerScopeGate 验证 ro/ops 档令牌被拒（403）：发布制品即舰队可执行内容，admin 专属。
func TestReleaseUploadHandlerScopeGate(t *testing.T) {
	h := ReleaseUploadHandler(nil, nil)
	for _, scope := range []string{ScopeReadOnly, ScopeOps} {
		req := httptest.NewRequest(http.MethodPost,
			releasesPath+"?platform=linux-x86_64&version=0.3.0", strings.NewReader("x"))
		req = req.WithContext(WithIdentity(req.Context(), TokenIdentity{Name: "t", Scope: scope}))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("scope %s want 403, got %d", scope, rec.Code)
		}
	}
}

// TestRESTHandlerReleasesRoute 验证 /v1/releases 路由前置于 enroll 宽前缀且经 bearer：无 token 401
// （而非落公开 enroll 面）；releases 为 nil 时回落（此处 enroll 亦 nil → SPA）。
func TestRESTHandlerReleasesRoute(t *testing.T) {
	spa := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("spa")) })
	enroll := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("enroll")) })
	releases := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("releases")) })

	h := NewRESTHandler(SingleToken("secret"), nil, spa, spa, nil, enroll, nil, releases, spa)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, releasesPath+"?platform=p&version=v", strings.NewReader("x")))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no-token /v1/releases want 401 (bearer-gated, not public enroll), got %d body=%q", rec.Code, rec.Body.String())
	}

	req := httptest.NewRequest(http.MethodPost, releasesPath, strings.NewReader("x"))
	req.Header.Set("Authorization", "Bearer secret")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Body.String() != "releases" {
		t.Fatalf("bearer /v1/releases want releases handler, got %q", rec.Body.String())
	}

	// releases 未挂载（nil）：路径落 enroll 宽前缀（既有语义不变）。
	h = NewRESTHandler(SingleToken("secret"), nil, spa, spa, nil, enroll, nil, nil, spa)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, releasesPath, strings.NewReader("x")))
	if rec.Body.String() != "enroll" {
		t.Fatalf("nil releases want enroll fallback, got %q", rec.Body.String())
	}
}
