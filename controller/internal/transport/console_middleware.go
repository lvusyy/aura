package transport

import (
	"net/http"
	"strings"

	connectcors "connectrpc.com/cors"
	"github.com/rs/cors"
)

// apiPathPrefix 是需 bearer 鉴权的 Connect API 路由前缀。ControllerAdmin 与 ConsoleService 均在
// aura.v1 命名空间下（/aura.v1.ControllerAdmin/*、/aura.v1.ConsoleService/*）；SPA 静态资源在此前缀外，公开。
const apiPathPrefix = "/aura.v1."

// streamPathPrefix 是实时流 raw WS 桥路由前缀（M8-P2）：Tango Android 流挂 /stream/tango。桥非 Connect RPC，
// 且浏览器 WebSocket 不能设 Authorization 头，故不过 BearerMiddleware——桥自持 bearer（?token= / 子协议）校验。
const streamPathPrefix = "/stream/"

// enrollPathPrefix 是 M12 设备接入 bootstrap 端点前缀（TASK-006）：/v1/enroll 收节点 CSR 换 per-node
// cert。**公开路由不过 BearerMiddleware**——节点此刻无 admin bearer、无客户端证书，认证凭 body 内一次性
// enroll token（ConsumeToken 原子校验，design §3.2）。/v1/renew 走 :7443 mTLS 端口（非本 :18080 面）。
const enrollPathPrefix = "/v1/"

// NewRESTHandler 组合 :18080 REST 面的中间件链（对齐 research §4/§6）：
//
//	CORS(最外) → 按路径分流：/aura.v1.*  经 BearerMiddleware 鉴权     ┐
//	                          /stream/*   交 stream 桥（自持 bearer） ├→ 响应
//	                          其余路径     交 spa（embed 前端，公开）  ┘
//
//   - /aura.v1.*（Connect API）经 BearerMiddleware——行为与 M6 完全一致，既有 ControllerAdmin 鉴权零回归；
//   - /stream/*（实时流 raw WS 桥）自持 bearer 校验（WS 无 Authorization 头，经 ?token=/子协议承载）；
//   - /v1/*（M12 enroll bootstrap，TASK-006）公开路由——凭 body 内 enroll token 认证，不过 bearer；
//     enroll 为 nil（未配置 CA / PG）时该前缀回落 SPA（404），签发面未启用不影响既有面；
//   - SPA 静态路径不过 bearer，公开访问；
//   - CORS 置于最外层：浏览器预检 OPTIONS（不带 Authorization）由 CORS 直接应答，不下沉 bearer——
//     否则预检因缺 token 被 401，跨域失败。
// 批E C1：token 单值改 TokenScopes 多档映射（ro/ops/admin 分级 + forwarder 独立凭据同表准入）。
func NewRESTHandler(scopes TokenScopes, apiMux, stream, artifact, enroll, mcp, spa http.Handler) http.Handler {
	apiAuth := BearerMiddleware(scopes, apiMux)
	var mcpAuth http.Handler
	if mcp != nil {
		mcpAuth = BearerMiddleware(scopes, mcp)
	}
	router := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, apiPathPrefix):
			apiAuth.ServeHTTP(w, r)
		case strings.HasPrefix(r.URL.Path, streamPathPrefix):
			stream.ServeHTTP(w, r)
		case artifact != nil && strings.HasPrefix(r.URL.Path, artifactPathPrefix):
			artifact.ServeHTTP(w, r) // 录屏 Range 流式回放：自持 ?token=（<video src> 无 Authorization 头），不过 bearer
		// M14 MCP 网关（bearer 鉴权）：/v1/mcp/ 比 enroll 的宽前缀 /v1/ 更特异，必须前置判定，
		// 否则落公开 enroll 面（404 且绕过 bearer）。
		case mcpAuth != nil && strings.HasPrefix(r.URL.Path, mcpGatewayPathPrefix):
			mcpAuth.ServeHTTP(w, r)
		case enroll != nil && strings.HasPrefix(r.URL.Path, enrollPathPrefix):
			enroll.ServeHTTP(w, r) // 公开：enroll token 认证在 handler 内（ConsumeToken），不过 bearer
		default:
			spa.ServeHTTP(w, r)
		}
	})
	return withCORS(router)
}

// withCORS 用 connectrpc.com/cors + rs/cors 包裹 handler，暴露 connect-web 浏览器客户端所需的跨域头。
// Authorization 须手动并入 AllowedHeaders（connect 默认头集不含之，见 research §4）；AllowCredentials 保持
// false（bearer 走 Authorization 头而非 cookie，故通配 origin 安全）。
func withCORS(h http.Handler) http.Handler {
	middleware := cors.New(cors.Options{
		AllowedOrigins: []string{"*"},
		AllowedMethods: connectcors.AllowedMethods(),
		AllowedHeaders: append(connectcors.AllowedHeaders(), "Authorization"),
		ExposedHeaders: connectcors.ExposedHeaders(),
		MaxAge:         7200, // 预检缓存 2h，减少 OPTIONS 往返
	})
	return middleware.Handler(h)
}
