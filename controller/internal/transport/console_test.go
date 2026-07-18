package transport

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"connectrpc.com/connect"

	aurav1 "github.com/aura/controller/gen/aura/v1"
	"github.com/aura/controller/internal/orchestrator"
	"github.com/aura/controller/internal/registry"
	"github.com/aura/controller/internal/scheduler"
)

// newTestRESTHandler 用伪 API/stream/SPA handler 组合 NewRESTHandler，隔离中间件边界（不依赖真 connect handler）。
// 伪 API 命中即写 "api-ok"（证明 bearer 放行后路由到达）；伪 stream 写 "stream"；伪 SPA 写 "spa"。
func newTestRESTHandler(token string) http.Handler {
	apiMux := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("api-ok"))
	})
	stream := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("stream"))
	})
	enroll := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("enroll"))
	})
	spa := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("spa"))
	})
	artifact := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("artifact"))
	})
	return NewRESTHandler(SingleToken(token), apiMux, stream, artifact, enroll, spa)
}

// TestRESTHandler_StreamBridgeRoute 验证 /stream/* 路由到桥 handler（不过 BearerMiddleware——桥自持 bearer，
// WS 无 Authorization 头）：无 Authorization 头亦不被 401 拦截，直达桥。
func TestRESTHandler_StreamBridgeRoute(t *testing.T) {
	h := newTestRESTHandler("secret")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/stream/tango?node=n1", nil))
	if rec.Code == http.StatusUnauthorized {
		t.Fatalf("/stream/* 不应被 BearerMiddleware 拦截（桥自持 bearer），got 401")
	}
	if rec.Body.String() != "stream" {
		t.Fatalf("/stream/* 应路由到桥 handler, got %q", rec.Body.String())
	}
}

// TestRESTHandler_ArtifactRoute 验证 /artifact/* 路由到产物流 handler（不过 BearerMiddleware——<video src>/
// <a download> 无 Authorization 头，handler 自持 ?token= 校验）：无 Authorization 头亦不被 401 拦截，直达。
func TestRESTHandler_ArtifactRoute(t *testing.T) {
	h := newTestRESTHandler("secret")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/artifact/recordings/x.mp4?token=secret", nil))
	if rec.Code == http.StatusUnauthorized {
		t.Fatalf("/artifact/* 不应被 BearerMiddleware 拦截（handler 自持 ?token=），got 401")
	}
	if rec.Body.String() != "artifact" {
		t.Fatalf("/artifact/* 应路由到产物流 handler, got %q", rec.Body.String())
	}
}

// TestRESTHandler_SPAPublic 验证 SPA 静态路径公开（无 token 得 200，不过 bearer）。
func TestRESTHandler_SPAPublic(t *testing.T) {
	h := newTestRESTHandler("secret")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/index.html", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("SPA path: want 200, got %d", rec.Code)
	}
	if rec.Body.String() != "spa" {
		t.Fatalf("SPA path: want spa handler, got %q", rec.Body.String())
	}
}

// TestRESTHandler_EnrollPublic 验证 /v1/enroll 公开路由（M12 TASK-006）：无 Authorization 头亦不被
// BearerMiddleware 401 拦截，直达 enroll handler（认证凭 body enroll token，不过 bearer）。
func TestRESTHandler_EnrollPublic(t *testing.T) {
	h := newTestRESTHandler("secret")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/enroll", strings.NewReader("{}")))
	if rec.Code == http.StatusUnauthorized {
		t.Fatalf("/v1/enroll 不应被 BearerMiddleware 拦截（enroll token 认证在 handler 内），got 401")
	}
	if rec.Body.String() != "enroll" {
		t.Fatalf("/v1/enroll 应路由到 enroll handler, got %q", rec.Body.String())
	}
}

// TestRESTHandler_EnrollNilFallsBackToSPA 验证 enroll 为 nil（未配 CA/PG，签发面未启用）时 /v1/ 前缀
// 回落 SPA（不 panic、不 500）——签发面可空注入不影响既有面。
func TestRESTHandler_EnrollNilFallsBackToSPA(t *testing.T) {
	apiMux := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("api-ok")) })
	stream := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("stream")) })
	spa := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("spa")) })
	h := NewRESTHandler(SingleToken("secret"), apiMux, stream, nil, nil, spa)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/enroll", strings.NewReader("{}")))
	if rec.Body.String() != "spa" {
		t.Fatalf("enroll==nil 时 /v1/ 应回落 SPA, got %q", rec.Body.String())
	}
}

// TestRESTHandler_APIRequiresBearer 验证 /aura.v1.* 无 token 得 401（bearer 拦截）。
func TestRESTHandler_APIRequiresBearer(t *testing.T) {
	h := newTestRESTHandler("secret")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/aura.v1.ConsoleService/GetDashboard", nil))

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("API without token: want 401, got %d", rec.Code)
	}
}

// TestRESTHandler_APIWithBearer 验证 /aura.v1.* 带正确 token 放行到达 API handler。
func TestRESTHandler_APIWithBearer(t *testing.T) {
	h := newTestRESTHandler("secret")
	req := httptest.NewRequest(http.MethodPost, "/aura.v1.ConsoleService/GetDashboard", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("API with token: want 200, got %d", rec.Code)
	}
	if rec.Body.String() != "api-ok" {
		t.Fatalf("API with token: want api handler reached, got %q", rec.Body.String())
	}
}

// TestRESTHandler_CORSPreflightPublic 验证 CORS 预检 OPTIONS（不带 Authorization）由 CORS 直接应答，
// 不下沉 bearer（非 401），且 Authorization 在 Access-Control-Allow-Headers（criterion 5）。
func TestRESTHandler_CORSPreflightPublic(t *testing.T) {
	h := newTestRESTHandler("secret")
	req := httptest.NewRequest(http.MethodOptions, "/aura.v1.ConsoleService/GetDashboard", nil)
	req.Header.Set("Origin", "http://localhost:5173")
	req.Header.Set("Access-Control-Request-Method", "POST")
	// Access-Control-Request-Headers 必须为浏览器规范形式（小写 + 无空格 + 字典序排序）：rs/cors v1.11
	// 依赖 Fetch 规范保证做快速匹配，非规范化值（混合大小写/空格/未排序）会被显式 AllowedHeaders 列表拒绝。
	// 真实 connect-web 浏览器发规范化值，故生产配置（connectcors 头集 + Authorization）成立。
	req.Header.Set("Access-Control-Request-Headers", "authorization,content-type")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code == http.StatusUnauthorized {
		t.Fatalf("CORS preflight must not hit bearer, got 401")
	}
	if rec.Header().Get("Access-Control-Allow-Origin") == "" {
		t.Fatalf("CORS preflight should carry Access-Control-Allow-Origin header")
	}
	if allow := rec.Header().Get("Access-Control-Allow-Headers"); !strings.Contains(strings.ToLower(allow), "authorization") {
		t.Fatalf("Authorization must be in Access-Control-Allow-Headers, got %q", allow)
	}
}

// TestOrchestrationHandlers 验证 3 个编排 handler 真实现的接线与降级：空目标 → InvalidArgument；纯内存
// （store 为 nil）读路径 GetOrchestration → NotFound、ListOrchestrations → 空列表。真 registry/scheduler
// 空跑（0 目标不触发任何 dispatch），无需 mock 反连流；聚合桶语义由 orchestrator 包单测覆盖。
func TestOrchestrationHandlers(t *testing.T) {
	reg := registry.NewRegistry(nil)
	sched := scheduler.NewScheduler(reg, nil, 0, nil)
	orch := orchestrator.NewOrchestrator(reg, sched, nil)
	srv := NewConsoleServiceServer(reg, nil, sched, orch, nil)
	ctx := context.Background()

	// RunOrchestration 空目标（node_ids/env_group 均空）→ InvalidArgument（ErrNoTargets），不发生 dispatch。
	if _, err := srv.RunOrchestration(ctx, connect.NewRequest(&aurav1.RunOrchestrationRequest{Tool: "click"})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("RunOrchestration 空目标: want InvalidArgument, got %v", err)
	}
	// env_group 解析空集（无匹配在线节点）同样 → InvalidArgument。
	if _, err := srv.RunOrchestration(ctx, connect.NewRequest(&aurav1.RunOrchestrationRequest{Tool: "click", EnvGroup: "linux"})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("RunOrchestration 空 env_group: want InvalidArgument, got %v", err)
	}
	// GetOrchestration 纯内存（store 为 nil）→ NotFound。
	if _, err := srv.GetOrchestration(ctx, connect.NewRequest(&aurav1.GetOrchestrationRequest{OrchestrationId: "x"})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("GetOrchestration in-memory: want NotFound, got %v", err)
	}
	// ListOrchestrations 纯内存 → 空列表、无错。
	resp, err := srv.ListOrchestrations(ctx, connect.NewRequest(&aurav1.ListOrchestrationsRequest{}))
	if err != nil {
		t.Fatalf("ListOrchestrations in-memory: unexpected err %v", err)
	}
	if len(resp.Msg.GetOrchestrations()) != 0 || resp.Msg.GetNextPageToken() != "" {
		t.Fatalf("ListOrchestrations in-memory: want empty page, got %d rows, next=%q", len(resp.Msg.GetOrchestrations()), resp.Msg.GetNextPageToken())
	}
}
