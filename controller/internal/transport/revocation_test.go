package transport

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// fakeRevChecker 是 certRevocationChecker 测试替身：注入 revoked/err，记录被查指纹。
type fakeRevChecker struct {
	revoked bool
	err     error
	gotFP   string
	called  bool
}

func (f *fakeRevChecker) IsCertRevoked(_ context.Context, fp string) (bool, error) {
	f.called = true
	f.gotFP = fp
	return f.revoked, f.err
}

// serveWithFP 以注入 peer 证书指纹的请求驱动中间件（模拟 PeerCertFPMiddleware 注入 ctx；空 fp=不注入）。
func serveWithFP(h http.Handler, fp string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/aura.v1.NodeControl/Connect", nil)
	if fp != "" {
		req = req.WithContext(context.WithValue(req.Context(), peerCertFPKey, fp))
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func passthroughHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("passed")) })
}

// TestRevocationMiddleware_RejectsRevoked 验证命中吊销 → 403，且以正确指纹反查。
func TestRevocationMiddleware_RejectsRevoked(t *testing.T) {
	chk := &fakeRevChecker{revoked: true}
	rec := serveWithFP(RevocationMiddleware(chk, passthroughHandler()), "fp-revoked")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("revoked status = %d, want 403", rec.Code)
	}
	if chk.gotFP != "fp-revoked" {
		t.Errorf("checker queried fp %q, want fp-revoked", chk.gotFP)
	}
}

// TestRevocationMiddleware_AllowsUnrevoked 验证未吊销（含未命中，checker 返 false）→ 放行。
func TestRevocationMiddleware_AllowsUnrevoked(t *testing.T) {
	rec := serveWithFP(RevocationMiddleware(&fakeRevChecker{revoked: false}, passthroughHandler()), "fp-ok")
	if rec.Body.String() != "passed" {
		t.Fatalf("unrevoked should pass through, got %q (code %d)", rec.Body.String(), rec.Code)
	}
}

// TestRevocationMiddleware_FailOpenOnError 验证 DB 查询失败 → fail-open 放行（可用性优先，design §7）。
func TestRevocationMiddleware_FailOpenOnError(t *testing.T) {
	rec := serveWithFP(RevocationMiddleware(&fakeRevChecker{err: errors.New("pg down")}, passthroughHandler()), "fp-x")
	if rec.Body.String() != "passed" {
		t.Fatalf("DB error should fail-open (allow), got %q (code %d)", rec.Body.String(), rec.Code)
	}
}

// TestRevocationMiddleware_NilChecker 验证 checker nil（纯内存无 PG）→ 直通（退化纯 mTLS）。
func TestRevocationMiddleware_NilChecker(t *testing.T) {
	rec := serveWithFP(RevocationMiddleware(nil, passthroughHandler()), "fp-any")
	if rec.Body.String() != "passed" {
		t.Fatalf("nil checker should pass through, got %q", rec.Body.String())
	}
}

// TestRevocationMiddleware_EmptyFP 验证无 peer 指纹（提取失败/无证书）→ 放行且不查台账。
func TestRevocationMiddleware_EmptyFP(t *testing.T) {
	chk := &fakeRevChecker{revoked: true}
	rec := serveWithFP(RevocationMiddleware(chk, passthroughHandler()), "")
	if rec.Body.String() != "passed" {
		t.Fatalf("empty fp should pass through, got %q", rec.Body.String())
	}
	if chk.called {
		t.Errorf("checker must not be queried when fp is empty")
	}
}
