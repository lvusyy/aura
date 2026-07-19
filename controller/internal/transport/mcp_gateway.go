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
func McpGatewayHandler(reg *registry.NodeRegistry, sched *scheduler.Scheduler) http.Handler {
	limiter := newGatewayLimiter()
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
		writeMcpProxyReply(w, int(resp.GetStatus()), resp.GetContentType(), resp.GetBody())
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
