package transport

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aura/controller/internal/store"
)

// fakeAPITokenSource 是 ApiTokenSource 的内存替身（免真 PG）：按 secret_hash 点查，LookupApiToken 仅返
// 「当前有效」令牌（吊销/过期由本替身预筛，复刻 SQL WHERE NOT revoked AND expires_at 语义）。
type fakeAPITokenSource struct {
	byHash     map[string]store.ApiToken
	touchCalls int
}

func (f *fakeAPITokenSource) LookupApiToken(_ context.Context, secretHash string) (store.ApiToken, error) {
	rec, ok := f.byHash[secretHash]
	if !ok || rec.Revoked {
		return store.ApiToken{}, store.ErrNoRowsSentinel
	}
	return rec, nil
}

func (f *fakeAPITokenSource) TouchApiToken(_ context.Context, _ string) error {
	f.touchCalls++
	return nil
}

func hashOf(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// captureIdentity 是记录 ctx 内 TokenIdentity 的末端 handler（验证中间件注入的身份）。
type captureIdentity struct{ got TokenIdentity }

func (c *captureIdentity) ServeHTTP(_ http.ResponseWriter, r *http.Request) {
	c.got = IdentityFromContext(r.Context())
}

// TestBearerMiddlewareDB_EnvTokenPrecedence 验证 env 静态令牌优先命中且注入 env:<scope> 身份（DB 源不查）。
func TestBearerMiddlewareDB_EnvTokenPrecedence(t *testing.T) {
	src := &fakeAPITokenSource{byHash: map[string]store.ApiToken{}}
	cap := &captureIdentity{}
	h := BearerMiddlewareDB(TokenScopes{"env-admin": ScopeAdmin}, src, cap)

	req := httptest.NewRequest(http.MethodPost, "/aura.v1.X/Y", nil)
	req.Header.Set("Authorization", "Bearer env-admin")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("env token: want 200, got %d", rec.Code)
	}
	if cap.got.Scope != ScopeAdmin || cap.got.Project != "" {
		t.Fatalf("env identity = %+v, want admin/全域", cap.got)
	}
	if src.touchCalls != 0 {
		t.Fatalf("env token 命中不应触 DB touch, got %d", src.touchCalls)
	}
}

// TestBearerMiddlewareDB_DBTokenIdentity 验证 DB 实体令牌命中：注入 name/scope/project 身份 + 触发 touch。
func TestBearerMiddlewareDB_DBTokenIdentity(t *testing.T) {
	secret := "aura_projtoken"
	src := &fakeAPITokenSource{byHash: map[string]store.ApiToken{
		hashOf(secret): {ID: "id-1", Name: "team-a-bot", Scope: ScopeAdmin, Project: "team-a"},
	}}
	cap := &captureIdentity{}
	h := BearerMiddlewareDB(TokenScopes{"env-admin": ScopeAdmin}, src, cap)

	req := httptest.NewRequest(http.MethodPost, "/aura.v1.X/Y", nil)
	req.Header.Set("Authorization", "Bearer "+secret)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("db token: want 200, got %d", rec.Code)
	}
	if cap.got.Name != "team-a-bot" || cap.got.Scope != ScopeAdmin || cap.got.Project != "team-a" {
		t.Fatalf("db identity = %+v, want team-a-bot/admin/team-a", cap.got)
	}
}

// TestBearerMiddlewareDB_RevokedRejected 验证吊销/未知令牌 401（fakeSource 对 revoked 返 NotFound）。
func TestBearerMiddlewareDB_RevokedRejected(t *testing.T) {
	secret := "aura_revoked"
	src := &fakeAPITokenSource{byHash: map[string]store.ApiToken{
		hashOf(secret): {ID: "id-2", Name: "dead", Scope: ScopeAdmin, Revoked: true},
	}}
	h := BearerMiddlewareDB(nil, src, &captureIdentity{})

	for _, tok := range []string{secret, "aura_unknown"} {
		req := httptest.NewRequest(http.MethodPost, "/aura.v1.X/Y", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("token %q: want 401, got %d", tok, rec.Code)
		}
	}
}

// TestBearerMiddlewareDB_NoSourcesFailClosed 验证 env 空映射 + 无 DB 源时一律 401（fail closed）。
func TestBearerMiddlewareDB_NoSourcesFailClosed(t *testing.T) {
	h := BearerMiddlewareDB(nil, nil, &captureIdentity{})
	req := httptest.NewRequest(http.MethodPost, "/aura.v1.X/Y", nil)
	req.Header.Set("Authorization", "Bearer anything")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no sources: want 401, got %d", rec.Code)
	}
}

// TestCheckNodeProjectAccess_GlobalTokenShortCircuit 验证全域令牌（Project="")对任意节点零成本放行
// （pg=nil 亦不触发解析）。
func TestCheckNodeProjectAccess_GlobalTokenShortCircuit(t *testing.T) {
	ctx := WithIdentity(context.Background(), TokenIdentity{Scope: ScopeAdmin, Project: ""})
	if err := CheckNodeProjectAccess(ctx, nil, "any-node"); err != nil {
		t.Fatalf("global token should pass any node, got %v", err)
	}
}

// TestCheckNodeProjectAccess_ProjectTokenNilPGFailClosed 验证项目令牌在无 PG（无法解析节点归属）时
// fail-closed 拒绝——宁拒不越界（全域已短路，仅项目令牌受此约束）。
func TestCheckNodeProjectAccess_ProjectTokenNilPGFailClosed(t *testing.T) {
	ctx := WithIdentity(context.Background(), TokenIdentity{Scope: ScopeAdmin, Project: "team-a"})
	// pg=nil：nodeProject 解析为空串（未归属），team-a != "" → 拒绝。
	if err := CheckNodeProjectAccess(ctx, nil, "unknown-node"); err == nil {
		t.Fatal("project token vs unresolvable node should be denied (fail-closed)")
	}
}
