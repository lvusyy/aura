package transport

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aura/controller/internal/registry"
	"github.com/aura/controller/internal/scheduler"
)

// gwRequest 构造一次带 scope 的网关请求（scope 经 ctx 注入，模拟 BearerMiddleware 之后的形态；
// 空 scope = 单 token 部署兼容语义，视同 admin）。
func gwRequest(t *testing.T, method, path, scope string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	reg := registry.NewRegistry(nil)
	h := McpGatewayHandler(reg, nil) // sched=nil：单测覆盖 nil 卫（生产恒注入）
	req := httptest.NewRequest(method, path, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	if scope != "" {
		req = req.WithContext(context.WithValue(req.Context(), scopeKey, scope))
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestMcpGateway_MethodNotAllowed 验证非 POST 与节点 stateless 端点同语义：405 + Allow: POST
// （GET SSE 探测/DELETE 会话终止都不是本端点的合法动作）。
func TestMcpGateway_MethodNotAllowed(t *testing.T) {
	for _, m := range []string{http.MethodGet, http.MethodDelete} {
		rec := gwRequest(t, m, "/v1/mcp/node-1", "", nil)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s: want 405, got %d", m, rec.Code)
		}
		if allow := rec.Header().Get("Allow"); allow != http.MethodPost {
			t.Fatalf("%s: Allow header = %q, want POST", m, allow)
		}
	}
}

// TestMcpGateway_PathValidation 验证缺 node_id 或带多余路径段时 404（不落入转发/会话查找）。
func TestMcpGateway_PathValidation(t *testing.T) {
	for _, p := range []string{"/v1/mcp/", "/v1/mcp/node-1/extra"} {
		rec := gwRequest(t, http.MethodPost, p, "", nil)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("POST %s: want 404, got %d", p, rec.Code)
		}
	}
}

// TestMcpGateway_ScopeGate 验证档位门控：网关等价节点全权 MCP 面，ops/ro 拒 403；
// admin 与空 scope（单 token 兼容）放行至会话查找（无会话 → 404，而非 403）。
func TestMcpGateway_ScopeGate(t *testing.T) {
	for _, scope := range []string{ScopeReadOnly, ScopeOps} {
		rec := gwRequest(t, http.MethodPost, "/v1/mcp/node-1", scope, nil)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("scope %q: want 403, got %d", scope, rec.Code)
		}
	}
	for _, scope := range []string{ScopeAdmin, ""} {
		rec := gwRequest(t, http.MethodPost, "/v1/mcp/node-1", scope, nil)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("scope %q: want 404 (past gate, node absent), got %d", scope, rec.Code)
		}
	}
}

// TestMcpGateway_HopMarkerNoReforward 验证被转发请求（携 ForwardedByHeader）在会话缺席时一跳
// 终态 404——sched=nil 下同路径亦不 panic（nil 卫）。
func TestMcpGateway_HopMarkerNoReforward(t *testing.T) {
	rec := gwRequest(t, http.MethodPost, "/v1/mcp/node-1", "",
		map[string]string{scheduler.ForwardedByHeader: "replica-x"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("forwarded request, node absent: want 404, got %d", rec.Code)
	}
}

// TestGatewayLimiter 验证 per-node 在途闸：占满上限后拒绝，释放后重新可占，且计数按 node 隔离。
func TestGatewayLimiter(t *testing.T) {
	l := newGatewayLimiter()
	for i := 0; i < mcpGatewayPerNodeInflight; i++ {
		if !l.acquire("node-a") {
			t.Fatalf("acquire #%d for node-a should succeed (under limit)", i+1)
		}
	}
	if l.acquire("node-a") {
		t.Fatal("acquire past limit for node-a should fail")
	}
	// 另一节点不受 node-a 占用影响（per-node 隔离）。
	if !l.acquire("node-b") {
		t.Fatal("node-b should have its own budget")
	}
	// 释放一个 node-a 名额后可重新占。
	l.release("node-a")
	if !l.acquire("node-a") {
		t.Fatal("after release, node-a should accept one more")
	}
	// 全部释放后键应从 map 删除（不驻留离线节点）。
	for i := 0; i < mcpGatewayPerNodeInflight; i++ {
		l.release("node-a")
	}
	l.release("node-b")
	if n := len(l.inflight); n != 0 {
		t.Fatalf("inflight map should be empty after full release, got %d keys", n)
	}
}
