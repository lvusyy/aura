// mcp_gateway.go 承载 M14 控制面 MCP 网关：agent → `POST /v1/mcp/<node_id>`（TLS + bearer）→
// 节点反连流 McpProxyRequest/Response 帧对 → 节点自环本机 rmcp /mcp 面。定位：跨网/生产接入的
// 唯一对外入口——测试机保持纯内网（节点 /mcp 可只绑 loopback 或关闭对外暴露），controller 是
// 单点鉴权/审计/观测面。
//
// 哑管道纪律：网关不解析 JSON-RPC——initialize/tools/list/tools/call 的语义、schema、协议版本
// 协商全部由节点 rmcp 面单一承载，网关语义与 agent 直连节点严格一致（未来 MCP 演进零跟进成本）。
package transport

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	aurav1 "github.com/aura/controller/gen/aura/v1"
	"github.com/aura/controller/internal/registry"
	"github.com/aura/controller/internal/scheduler"
	"github.com/aura/controller/internal/selection"
	"github.com/aura/controller/internal/store"
)

// mcpGatewayPathPrefix 是网关路由前缀（console_middleware 路由分支消费；须先于 enroll 的宽前缀
// `/v1/` 判定，否则落公开 enroll 面）。
const mcpGatewayPathPrefix = "/v1/mcp/"

// mcpGatewayTimeout 是整链兜底硬超时：节点自环 120s 之外留传输余量。三层各兜各的
// （工具执行 deadline → 节点自环 → 网关整链），任一层挂起都有限时终态。
const mcpGatewayTimeout = 150 * time.Second

// mcpGatewayBodyCap 是请求体上限。MCP JSON-RPC 请求为控制消息量级（tools/call 入参 JSON），
// 8MB 已远超合法载荷；超限即 413，防经网关向节点灌注超帧载荷。
const mcpGatewayBodyCap = 8 * 1024 * 1024

// mcpGatewayPerNodeInflight 是单节点在途网关请求上限（bounded best-effort 纪律，同 agentObsSem/
// 内存 cap 家族）：合法并行工具调用远在其下，超限即 429——防单个 agent 的失控循环把一台节点的
// 反连流/pending 埋满、拖累该节点其他请求。admin-only 准入之外的纵深防御（可信令牌亦可能有 bug）。
const mcpGatewayPerNodeInflight = 8

// gatewayLimiter 以 node_id 计在途请求数。计数归零即从 map 删除，长期不驻留离线节点键。
type gatewayLimiter struct {
	mu       sync.Mutex
	inflight map[string]int
}

func newGatewayLimiter() *gatewayLimiter {
	return &gatewayLimiter{inflight: make(map[string]int)}
}

// acquire 尝试为 nodeID 占一个在途名额；已达上限返回 false（调用方 429）。
func (l *gatewayLimiter) acquire(nodeID string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.inflight[nodeID] >= mcpGatewayPerNodeInflight {
		return false
	}
	l.inflight[nodeID]++
	return true
}

// release 归还一个在途名额；归零即删键。
func (l *gatewayLimiter) release(nodeID string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.inflight[nodeID] <= 1 {
		delete(l.inflight, nodeID)
		return
	}
	l.inflight[nodeID]--
}

// McpGatewayHandler 构造网关 handler。鉴权（bearer）由外层 NewRESTHandler 统一包裹；本层只做
// 档位门控：网关等价节点全权 MCP 面（不解析工具名，无法按工具分级），故仅 admin 档（含单 token
// 部署的空 scope 兼容）放行；ops/ro 拒绝——需要分级派发的走 REST DispatchTool 面。
// M15：档位之后再过项目视界（CheckNodeProjectAccess 唯一规则）——项目 admin 令牌仅达本项目节点，
// 越界 403；pg 可为 nil（无持久层=节点未归属，仅全域令牌在场，行为零变化）。
func McpGatewayHandler(reg *registry.NodeRegistry, sched *scheduler.Scheduler, pg *store.PGStore, sel ...*selection.SelectorEngine) http.Handler {
	limiter := newGatewayLimiter()
	var selector *selection.SelectorEngine
	if len(sel) > 0 {
		selector = sel[0]
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nodeID := strings.TrimPrefix(r.URL.Path, mcpGatewayPathPrefix)
		if nodeID == "" || strings.Contains(nodeID, "/") {
			http.Error(w, "usage: POST /v1/mcp/<node_id>", http.StatusNotFound)
			auditMcpGateway(r, nodeID, http.StatusNotFound, 0, "bad-path")
			return
		}
		// 与节点 stateless Streamable HTTP 行为一致：仅 POST（GET SSE 探测/DELETE 会话终止均 405）。
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "method not allowed (stateless MCP endpoint accepts POST only)", http.StatusMethodNotAllowed)
			auditMcpGateway(r, nodeID, http.StatusMethodNotAllowed, 0, "method-not-allowed")
			return
		}
		switch ScopeFromContext(r.Context()) {
		case ScopeReadOnly, ScopeOps:
			http.Error(w, "MCP gateway requires an admin-scope token (gateway grants the node's full MCP surface; tiered dispatch is available on the REST DispatchTool plane)", http.StatusForbidden)
			auditMcpGateway(r, nodeID, http.StatusForbidden, 0, "forbidden-scope")
			return
		}
		// M15 项目视界：全域令牌零成本短路；项目令牌越界节点 403（connect 错误映射 http 状态）。
		if err := CheckNodeProjectAccess(r.Context(), pg, nodeID); err != nil {
			http.Error(w, "token project does not cover this node", http.StatusForbidden)
			auditMcpGateway(r, nodeID, http.StatusForbidden, 0, "forbidden-project")
			return
		}

		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, mcpGatewayBodyCap))
		if err != nil {
			http.Error(w, "request body too large or unreadable", http.StatusRequestEntityTooLarge)
			auditMcpGateway(r, nodeID, http.StatusRequestEntityTooLarge, 0, "body-too-large")
			return
		}

		// per-node 在途闸：占名额失败即 429（Retry-After 提示退避），不进派发/转发。
		if !limiter.acquire(nodeID) {
			w.Header().Set("Retry-After", "1")
			http.Error(w, "too many concurrent gateway requests for this node", http.StatusTooManyRequests)
			auditMcpGateway(r, nodeID, http.StatusTooManyRequests, len(body), "node-inflight-limit")
			return
		}
		defer limiter.release(nodeID)

		// 若请求为网关合成的 Jev 视觉定位/点击工具（locate_element / click_element），在控制面直接处理
		if selector != nil && tryServeGatewayJevTool(w, r, nodeID, body, selector) {
			return
		}

		sess, ok := reg.Ready(nodeID)
		if !ok {
			// 本副本无活会话：hop 防环（被转发请求不二次转发）之外尝试 owner 副本转发（HA）。
			// sched nil 卫：装配缺失/测试直建 handler 时安全落 404 而非 nil 解引用。
			if sched != nil && r.Header.Get(scheduler.ForwardedByHeader) == "" {
				// 转发腿同受整链兜底超时约束——裸 r.Context() 会让 owner 侧挂起时本副本
				// handler/连接槽位无限占用。
				fctx, fcancel := context.WithTimeout(r.Context(), mcpGatewayTimeout)
				status, ct, respBody, ferr, attempted := sched.ForwardMcp(fctx, nodeID, body, r.Header)
				fcancel()
				if attempted {
					if ferr != nil {
						http.Error(w, "forward to owner replica failed: "+ferr.Error(), http.StatusBadGateway)
						auditMcpGateway(r, nodeID, http.StatusBadGateway, len(body), "forward-error")
						return
					}
					respBody = maybeAugmentToolsList(body, respBody)
					writeMcpProxyReply(w, status, ct, respBody)
					auditMcpGateway(r, nodeID, status, len(body), "forwarded")
					return
				}
			}
			http.Error(w, "node not connected", http.StatusNotFound)
			auditMcpGateway(r, nodeID, http.StatusNotFound, len(body), "not-connected")
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), mcpGatewayTimeout)
		defer cancel()
		resp, perr := sess.ProxyMcp(ctx, &aurav1.McpProxyRequest{
			RequestId:       uuid.NewString(),
			Body:            body,
			ContentType:     r.Header.Get("Content-Type"),
			Accept:          r.Header.Get("Accept"),
			UserAgent:       r.Header.Get("User-Agent"),
			ProtocolVersion: r.Header.Get("Mcp-Protocol-Version"),
		})
		if perr != nil {
			// 老节点二进制不识别代理帧会安全忽略（oneof 未知 tag）→ 此处以超时收尾：错误文案给出
			// 可行动指引而非裸 timeout。
			http.Error(w,
				"node did not answer the MCP proxy request: "+perr.Error()+
					" (node offline/busy, or its aura-node binary predates gateway support — upgrade the node)",
				http.StatusGatewayTimeout)
			auditMcpGateway(r, nodeID, http.StatusGatewayTimeout, len(body), "proxy-error")
			return
		}
		respBody := maybeAugmentToolsList(body, resp.GetBody())
		writeMcpProxyReply(w, int(resp.GetStatus()), resp.GetContentType(), respBody)
		auditMcpGateway(r, nodeID, int(resp.GetStatus()), len(body), "ok")
	})
}

// writeMcpProxyReply 把节点侧响应原样回写 agent（status/content-type/body 透传，不改写）。
func writeMcpProxyReply(w http.ResponseWriter, status int, contentType string, body []byte) {
	if contentType != "" {
		w.Header().Set("Content-Type", contentType)
	}
	if status <= 0 {
		status = http.StatusBadGateway
	}
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// auditMcpGateway 落一行网关操作留痕（同 auditDispatch 纪律：不落请求内容，体量足够定位异常）。
func auditMcpGateway(r *http.Request, nodeID string, status, reqBytes int, outcome string) {
	slog.Info("mcp gateway audit",
		"node_id", nodeID, "status", status, "req_bytes", reqBytes,
		"outcome", outcome, "scope", ScopeFromContext(r.Context()),
		"ua", r.Header.Get("User-Agent"))
}

type jsonRPCReq struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	} `json:"params"`
}

func tryServeGatewayJevTool(
	w http.ResponseWriter,
	r *http.Request,
	nodeID string,
	body []byte,
	selector *selection.SelectorEngine,
) bool {
	var rpcReq jsonRPCReq
	if err := json.Unmarshal(body, &rpcReq); err != nil || rpcReq.Method != "tools/call" {
		return false
	}
	toolName := rpcReq.Params.Name
	if toolName != "locate_element" && toolName != "click_element" {
		return false
	}

	var args struct {
		Target      string `json:"target"`
		Button      string `json:"button"`
		ForceVision bool   `json:"force_vision"`
		Verify      bool   `json:"verify"`
		DeadlineMs  int64  `json:"deadline_ms"`
	}
	if len(rpcReq.Params.Arguments) > 0 {
		_ = json.Unmarshal(rpcReq.Params.Arguments, &args)
	}

	ctx, cancel := context.WithTimeout(r.Context(), mcpGatewayTimeout)
	defer cancel()

	who := IdentityFromContext(r.Context()).Name
	if who == "" {
		who = "mcp-gateway"
	}

	var payload any
	switch toolName {
	case "locate_element":
		out, err := selector.Locate(ctx, selection.LocateInput{
			NodeID:      nodeID,
			Target:      args.Target,
			ForceVision: args.ForceVision,
			DeadlineMs:  args.DeadlineMs,
			Who:         who,
		})
		if err != nil {
			payload = map[string]any{
				"ok":    false,
				"error": map[string]any{"code": "E_LOCATE_FAILED", "message": err.Error()},
			}
		} else {
			payload = map[string]any{
				"ok":   true,
				"data": out,
			}
		}
	case "click_element":
		out, err := selector.Click(ctx, selection.ClickInput{
			NodeID:      nodeID,
			Target:      args.Target,
			Button:      args.Button,
			ForceVision: args.ForceVision,
			Verify:      args.Verify,
			DeadlineMs:  args.DeadlineMs,
			Who:         who,
		})
		if err != nil {
			payload = map[string]any{
				"ok":    false,
				"error": map[string]any{"code": "E_CLICK_ELEMENT_FAILED", "message": err.Error()},
			}
		} else {
			payload = map[string]any{
				"ok":   true,
				"data": out,
			}
		}
	}

	textBytes, _ := json.Marshal(payload)
	rpcResp := map[string]any{
		"jsonrpc": "2.0",
		"id":      rpcReq.ID,
		"result": map[string]any{
			"content": []map[string]any{
				{"type": "text", "text": string(textBytes)},
			},
			"structuredContent": payload,
			"isError":           false,
		},
	}
	outBytes, _ := json.Marshal(rpcResp)
	writeMcpProxyReply(w, http.StatusOK, "application/json", outBytes)
	auditMcpGateway(r, nodeID, http.StatusOK, len(body), "jev-"+toolName)
	return true
}

func maybeAugmentToolsList(reqBody, respBody []byte) []byte {
	var rpcReq struct {
		Method string `json:"method"`
	}
	if err := json.Unmarshal(reqBody, &rpcReq); err != nil || rpcReq.Method != "tools/list" {
		return respBody
	}
	var rpcResp map[string]any
	if err := json.Unmarshal(respBody, &rpcResp); err != nil {
		return respBody
	}
	resObj, ok := rpcResp["result"].(map[string]any)
	if !ok {
		return respBody
	}
	toolsSlice, ok := resObj["tools"].([]any)
	if !ok {
		return respBody
	}
	seen := make(map[string]bool, len(toolsSlice))
	for _, item := range toolsSlice {
		if m, ok := item.(map[string]any); ok {
			if name, ok := m["name"].(string); ok {
				seen[name] = true
			}
		}
	}
	if !seen["locate_element"] {
		toolsSlice = append(toolsSlice, map[string]any{
			"name":        "locate_element",
			"description": "Fast hybrid UI element localization (A11y + YOLO geometry + Jev Choice/Noul/Box). Returns both native Bounds/ClickPoint and DisplayBounds/DisplayClickPoint.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"target":       map[string]any{"type": "string", "description": "Natural-language target element description (e.g. 'Submit button', 'Settings icon')"},
					"force_vision": map[string]any{"type": "boolean", "description": "Skip Tier 0 A11y shortcut and force visual inference"},
				},
				"required": []string{"target"},
			},
		})
	}
	if !seen["click_element"] {
		toolsSlice = append(toolsSlice, map[string]any{
			"name":        "click_element",
			"description": "One-step UI element localization and click via Jev (A11y + YOLO + Jev Choice/Noul/Box -> confidence gate -> click with exact coordinate alignment).",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"target":       map[string]any{"type": "string", "description": "Natural-language target element description"},
					"button":       map[string]any{"type": "string", "description": "Mouse button: left (default) | right | middle"},
					"force_vision": map[string]any{"type": "boolean", "description": "Force visual inference"},
					"verify":       map[string]any{"type": "boolean", "description": "Verify UI responsiveness after click"},
				},
				"required": []string{"target"},
			},
		})
	}
	resObj["tools"] = toolsSlice
	augmented, err := json.Marshal(rpcResp)
	if err != nil {
		return respBody
	}
	return augmented
}
