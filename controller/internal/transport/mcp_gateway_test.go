package transport

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"connectrpc.com/connect"

	aurav1 "github.com/aura/controller/gen/aura/v1"
	"github.com/aura/controller/internal/fusion"
	"github.com/aura/controller/internal/registry"
	"github.com/aura/controller/internal/scheduler"
	"github.com/aura/controller/internal/selection"
)

// gwRequest 构造一次带 scope 的网关请求（scope 经 ctx 注入，模拟 BearerMiddleware 之后的形态；
// 空 scope = 单 token 部署兼容语义，视同 admin）。
func gwRequest(t *testing.T, method, path, scope string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	reg := registry.NewRegistry(nil)
	h := McpGatewayHandler(reg, nil, nil) // sched/pg=nil：单测覆盖 nil 卫（生产恒注入；pg=nil→节点视同未归属，全域令牌路径）
	req := httptest.NewRequest(method, path, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	if scope != "" {
		req = req.WithContext(WithIdentity(req.Context(), TokenIdentity{Scope: scope}))
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

func TestMaybeAugmentToolsListIncludesAllSemanticTools(t *testing.T) {
	req := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	resp := []byte(`{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"screenshot"}]}}`)
	aug := string(maybeAugmentToolsList(req, resp))
	for _, want := range []string{"locate_element", "click_element", "annotate_screen", "wait_for_element", "computer", "run_agent_task"} {
		if !strings.Contains(aug, want) {
			t.Fatalf("expected augmented tools/list to contain %q, got %s", want, aug)
		}
	}
}

type fakeGatewayDisp struct {
	shotB64 string
}

func (f *fakeGatewayDisp) Dispatch(_ context.Context, _, tool string, args []byte, _ int64, _, _ string) (*aurav1.ToolResponse, string, error) {
	var data any
	switch tool {
	case "get_a11y_tree":
		b := [4]int32{100, 80, 160, 40}
		data = fusion.A11yTree{
			Nodes: []fusion.A11yNode{
				{Role: "button", Name: "Confirm", Bounds: &b},
			},
		}
	case "screenshot":
		data = map[string]any{
			"image_base64": f.shotB64,
			"mime":         "image/webp",
			"meta": map[string]any{
				"native_w":    1920,
				"native_h":    1080,
				"display_w":   960,
				"display_h":   540,
				"scale":       2.0,
				"snapshot_id": "snap-gw-e2e",
			},
		}
	case "click", "move_mouse":
		var p struct {
			Coordinate *[2]int32 `json:"coordinate"`
		}
		if err := json.Unmarshal(args, &p); err != nil || p.Coordinate == nil {
			raw, _ := json.Marshal(map[string]any{"ok": false, "error": map[string]any{"code": "INVALID_ARGS", "message": "missing coordinate [x,y]"}})
			return &aurav1.ToolResponse{JsonEnvelope: raw}, "", nil
		}
		data = map[string]any{"ok": true}
	case "drag":
		var p struct {
			From *[2]int32 `json:"from"`
			To   *[2]int32 `json:"to"`
		}
		if err := json.Unmarshal(args, &p); err != nil || p.From == nil || p.To == nil {
			raw, _ := json.Marshal(map[string]any{"ok": false, "error": map[string]any{"code": "INVALID_ARGS", "message": "missing from/to"}})
			return &aurav1.ToolResponse{JsonEnvelope: raw}, "", nil
		}
		data = map[string]any{"ok": true}
	case "scroll":
		var p struct {
			Coordinate      *[2]int32 `json:"coordinate"`
			ScrollDirection string    `json:"scroll_direction"`
		}
		if err := json.Unmarshal(args, &p); err != nil || p.Coordinate == nil || p.ScrollDirection == "" {
			raw, _ := json.Marshal(map[string]any{"ok": false, "error": map[string]any{"code": "INVALID_ARGS", "message": "missing coordinate or scroll_direction"}})
			return &aurav1.ToolResponse{JsonEnvelope: raw}, "", nil
		}
		data = map[string]any{"ok": true}
	case "type", "key", "wait":
		data = map[string]any{"ok": true}
	default:
		raw, _ := json.Marshal(map[string]any{"ok": false, "error": map[string]any{"code": "UNKNOWN_TOOL", "message": "unexpected node tool: " + tool}})
		return &aurav1.ToolResponse{JsonEnvelope: raw}, "", nil
	}
	raw, _ := json.Marshal(map[string]any{"ok": true, "data": data})
	return &aurav1.ToolResponse{JsonEnvelope: raw}, "", nil
}

// TestMcpGatewayAndRestAllSixSemanticToolsEndToEnd 系统性验证控制面合成的 6 项高阶 GUI 工具
// （locate_element / click_element / annotate_screen / wait_for_element / computer / run_agent_task）
// 在 `/v1/mcp/<node_id>` (MCP JSON-RPC) 与 `ControllerAdmin.DispatchTool` (REST) 双入口下的端到端闭环。
func TestMcpGatewayAndRestAllSixSemanticToolsEndToEnd(t *testing.T) {
	disp := &fakeGatewayDisp{
		shotB64: base64.StdEncoding.EncodeToString([]byte("fake-webp-bytes")),
	}
	sel := selection.NewSelectorEngine(disp, nil, "", "")
	reg := registry.NewRegistry(nil)
	gwHandler := McpGatewayHandler(reg, nil, nil, sel)
	adminSrv := &ControllerAdminServer{selector: sel}

	cases := []struct {
		name     string
		tool     string
		argsJSON string
		wantImg  bool
	}{
		{
			name:     "locate_element",
			tool:     "locate_element",
			argsJSON: `{"target":"Confirm"}`,
		},
		{
			name:     "click_element_with_type_and_key",
			tool:     "click_element",
			argsJSON: `{"target":"Confirm","double_click":true,"type_text":"aura","press_key":"Return"}`,
		},
		{
			name:     "annotate_screen_som",
			tool:     "annotate_screen",
			argsJSON: `{"max_elements":10}`,
			wantImg:  true,
		},
		{
			name:     "wait_for_element_present",
			tool:     "wait_for_element",
			argsJSON: `{"target":"Confirm","state":"present","timeout_ms":1000}`,
		},
		{
			name:     "computer_ui_tars_and_auto_screenshot",
			tool:     "computer",
			argsJSON: `{"ui_tars_action":"Action: click(start_box='(500, 500)')","auto_screenshot":true}`,
			wantImg:  true,
		},
		{
			name:     "run_agent_task_pipeline",
			tool:     "run_agent_task",
			argsJSON: `{"goal":"wait_for Confirm -> click Confirm -> type ready -> key Return"}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// 1. 验证 MCP Gateway `/v1/mcp/node-1`
			rpcPayload := `{"jsonrpc":"2.0","id":42,"method":"tools/call","params":{"name":"` + tc.tool + `","arguments":` + tc.argsJSON + `}}`
			req := httptest.NewRequest(http.MethodPost, "/v1/mcp/node-1", strings.NewReader(rpcPayload))
			rec := httptest.NewRecorder()
			gwHandler.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("MCP gateway %s: HTTP %d: %s", tc.tool, rec.Code, rec.Body.String())
			}
			var rpcResp struct {
				Result struct {
					IsError bool             `json:"isError"`
					Content []map[string]any `json:"content"`
				} `json:"result"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &rpcResp); err != nil {
				t.Fatalf("MCP gateway %s invalid JSON: %v", tc.tool, err)
			}
			if rpcResp.Result.IsError {
				t.Fatalf("MCP gateway %s returned isError=true: %s", tc.tool, rec.Body.String())
			}
			if tc.wantImg && len(rpcResp.Result.Content) < 2 {
				t.Fatalf("MCP gateway %s expected image content block, got %d blocks", tc.tool, len(rpcResp.Result.Content))
			}

			// 2. 验证 ControllerAdmin.DispatchTool REST 入口
			restResp, err := adminSrv.DispatchTool(context.Background(), connect.NewRequest(&aurav1.DispatchToolRequest{
				NodeId:   "node-1",
				Tool:     tc.tool,
				JsonArgs: []byte(tc.argsJSON),
			}))
			if err != nil {
				t.Fatalf("REST DispatchTool %s error: %v", tc.tool, err)
			}
			var env struct {
				Ok bool `json:"ok"`
			}
			if err := json.Unmarshal(restResp.Msg.GetJsonEnvelope(), &env); err != nil || !env.Ok {
				t.Fatalf("REST DispatchTool %s ok=false: %s", tc.tool, string(restResp.Msg.GetJsonEnvelope()))
			}
		})
	}
}
