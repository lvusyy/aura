package transport

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"connectrpc.com/connect"

	aurav1 "github.com/aura/controller/gen/aura/v1"
	"github.com/aura/controller/internal/gateway"
	"github.com/aura/controller/internal/observability"
	"github.com/aura/controller/internal/provisioner"
	"github.com/aura/controller/internal/registry"
	"github.com/aura/controller/internal/scheduler"
	"github.com/aura/controller/internal/store"
)

// ControllerAdminServer 实现 aurav1connect.ControllerAdminHandler，服务 auractl/agent 管理面。
type ControllerAdminServer struct {
	registry    *registry.NodeRegistry
	gateway     *gateway.Gateway
	scheduler   *scheduler.Scheduler
	provisioner provisioner.EnvProvider // 可为 nil（未配置任何 provider 时环境接口返回 Unavailable）
}

// NewControllerAdminServer 构造管理面服务。prov 是 EnvProvider（PVE 或 K8s，单选装配），
// 可为 nil（未配置任何 provider；main.go 保持接口为 nil 而非 typed-nil 以令下方 nil 判断生效）。
// sched 承接 M6 录制会话租约（StartTrace/StopTrace），与 gateway 直接持有各自所需组件的惯例一致。
func NewControllerAdminServer(reg *registry.NodeRegistry, gw *gateway.Gateway, sched *scheduler.Scheduler, prov provisioner.EnvProvider) *ControllerAdminServer {
	return &ControllerAdminServer{registry: reg, gateway: gw, scheduler: sched, provisioner: prov}
}

// ListNodes 返回所有在线节点及其 online/unhealthy 状态。
func (s *ControllerAdminServer) ListNodes(
	_ context.Context,
	_ *connect.Request[aurav1.ListNodesRequest],
) (*connect.Response[aurav1.ListNodesResponse], error) {
	nodes := s.registry.List()
	return connect.NewResponse(&aurav1.ListNodesResponse{Nodes: nodes}), nil
}

// DispatchTool 经控制面转发工具调用到指定节点。
// 由 gateway 规约为 Envelope JSON：成功透传节点信封，传输层错误合成同构错误信封。
// 批E C1：入口做令牌作用域门控（ro 拒 dispatch / ops 拒高影响工具）+ 结构化审计日志——
// node 侧 requires_user_interaction_meta 声明的高危工具（kill_process/run_command）自此在执行侧
// 真正强制（须 admin 档令牌），声明即强制。
func (s *ControllerAdminServer) DispatchTool(
	ctx context.Context,
	req *connect.Request[aurav1.DispatchToolRequest],
) (*connect.Response[aurav1.DispatchToolResponse], error) {
	if err := CheckDispatchScope(ctx, req.Msg.GetTool()); err != nil {
		return nil, err
	}
	auditDispatch(ctx, "DispatchTool", req.Msg.GetWho(), req.Msg.GetNodeId(), req.Msg.GetTool(), len(req.Msg.GetJsonArgs()))
	env := s.gateway.Dispatch(ctx, req.Msg)
	return connect.NewResponse(&aurav1.DispatchToolResponse{JsonEnvelope: env}), nil
}

// CreateEnvironment 从 PVE 模板克隆并置备一个环境（ephemeral 建基线快照 / persistent 保留）。
// 未配置 provisioner 时返回 Unavailable。返回 env_id + vmid；node_id 置备时为空（节点启动后自行反连注册）。
func (s *ControllerAdminServer) CreateEnvironment(
	ctx context.Context,
	req *connect.Request[aurav1.CreateEnvironmentRequest],
) (*connect.Response[aurav1.CreateEnvironmentResponse], error) {
	if s.provisioner == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("provisioner not configured (set AURA_PVE_* or AURA_K8S_KUBECONFIG env)"))
	}
	env, err := s.provisioner.CreateEnvironment(ctx, req.Msg.GetKind(), req.Msg.GetTemplate())
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	// node_id: always empty at provision time; nodes register asynchronously after boot
	// （Register 帧无 env_id 关联，现数据模型下无「注册后回填」路径）——env↔node 关联经
	// `auractl node list` 完成，不引入 enrollment-token↔env 关联机制（G-3/ISS-07）。
	return connect.NewResponse(&aurav1.CreateEnvironmentResponse{
		EnvId:  env.ID,
		Vmid:   int32(env.VMID),
		NodeId: env.NodeID,
	}), nil
}

// DestroyEnvironment 停机并删除环境底层 VM。未配置 provisioner 时返回 Unavailable。
func (s *ControllerAdminServer) DestroyEnvironment(
	ctx context.Context,
	req *connect.Request[aurav1.DestroyEnvironmentRequest],
) (*connect.Response[aurav1.DestroyEnvironmentResponse], error) {
	if s.provisioner == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("provisioner not configured (set AURA_PVE_* or AURA_K8S_KUBECONFIG env)"))
	}
	if err := s.provisioner.DestroyEnvironment(ctx, req.Msg.GetEnvId()); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&aurav1.DestroyEnvironmentResponse{Destroyed: true}), nil
}

// StartTrace 开始录制会话：对目标节点建立 per-node 独占租约并返回新 trace_id（TASK-002 真实现）。
// node 已被其他会话录制中 → CodeResourceExhausted（E_BUSY 语义，per-node 独占）。经 BearerMiddleware 鉴权。
func (s *ControllerAdminServer) StartTrace(
	_ context.Context,
	req *connect.Request[aurav1.StartTraceRequest],
) (*connect.Response[aurav1.StartTraceResponse], error) {
	traceID, err := s.scheduler.StartTrace(req.Msg.GetNodeId(), req.Msg.GetWho())
	if err != nil {
		return nil, connect.NewError(connect.CodeResourceExhausted, err)
	}
	return connect.NewResponse(&aurav1.StartTraceResponse{TraceId: traceID}), nil
}

// StopTrace 停止录制会话：释放 trace_id 对应的 per-node 租约（幂等，TASK-002 真实现）。经 BearerMiddleware 鉴权。
// stopped 转发 scheduler 的真实释放状态（proto:205「trace_id 不存在/已释放则 false」）——修正 TASK-006
// milestone-audit 观察项：此前硬编码 Stopped:true 与 proto 契约不符（未知/重复 stop 应报 false）。
func (s *ControllerAdminServer) StopTrace(
	_ context.Context,
	req *connect.Request[aurav1.StopTraceRequest],
) (*connect.Response[aurav1.StopTraceResponse], error) {
	stopped := s.scheduler.StopTrace(req.Msg.GetTraceId())
	return connect.NewResponse(&aurav1.StopTraceResponse{Stopped: stopped}), nil
}

// trace 分页默认/上限（防单页步数×envelope 超 connect 默认 max-recv-bytes，MAJOR-3）。
const (
	defaultTracePageSize int64 = 200
	maxTracePageSize     int64 = 500
)

// GetTrace 分页读取录制步序（回放读路径，TASK-004 真实现替 001 桩）。page_token=上页末 seq 游标（空=
// 首页），page_size=单页步数（0→默认 200，上限 500）；返回按 seq 升序的步序 + 录制源 node_id/platform +
// 下页游标（不足一页即末页，next_page_token 空）。经 BearerMiddleware 鉴权。
func (s *ControllerAdminServer) GetTrace(
	ctx context.Context,
	req *connect.Request[aurav1.GetTraceRequest],
) (*connect.Response[aurav1.GetTraceResponse], error) {
	seqCursor, err := parseTracePageToken(req.Msg.GetPageToken())
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	pageSize := normalizeTracePageSize(req.Msg.GetPageSize())

	steps, nodeID, platform, err := s.scheduler.GetTrace(ctx, req.Msg.GetTraceId(), pageSize, seqCursor)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	pbSteps := make([]*aurav1.TraceStep, 0, len(steps))
	for _, st := range steps {
		pbSteps = append(pbSteps, &aurav1.TraceStep{
			Seq:           st.Seq,
			Tool:          st.Tool,
			JsonArgs:      st.JsonArgs,
			JsonEnvelope:  st.JsonEnvelope, // 已在持久化前剥离内联截图（screenshot_key 引用 MinIO）
			ScreenshotKey: st.ScreenshotKey,
			TsUnixMs:      st.Ts.UnixMilli(),
		})
	}
	return connect.NewResponse(&aurav1.GetTraceResponse{
		Steps:         pbSteps,
		NodeId:        nodeID,
		Platform:      platform,
		NextPageToken: nextTracePageToken(steps, pageSize),
	}), nil
}

// normalizeTracePageSize 归一化请求 page_size：<=0 取默认 200，超上限截为 500（防单页过大超 connect
// max-recv-bytes，MAJOR-3）。
func normalizeTracePageSize(n int64) int64 {
	if n <= 0 {
		return defaultTracePageSize
	}
	if n > maxTracePageSize {
		return maxTracePageSize
	}
	return n
}

// parseTracePageToken 解析 page_token 为 seq 游标：空串=0（首页）；否则十进制解析，非法/负数报错
// （CodeInvalidArgument）。
func parseTracePageToken(token string) (int64, error) {
	if token == "" {
		return 0, nil
	}
	seq, err := strconv.ParseInt(token, 10, 64)
	if err != nil || seq < 0 {
		return 0, errors.New("invalid page_token: must be a non-negative seq cursor")
	}
	return seq, nil
}

// nextTracePageToken 计算下页游标：仅当本页填满（len==pageSize，可能还有下页）才回末步 seq 字符串；
// 不足一页（末页）返回空串终止分页循环。
func nextTracePageToken(steps []store.TraceStep, pageSize int64) string {
	if len(steps) == 0 || int64(len(steps)) < pageSize {
		return ""
	}
	return strconv.FormatInt(steps[len(steps)-1].Seq, 10)
}

// ===== 令牌作用域分级（批E C1）=====
// 管控令牌从单档扩三档（additive：仅配 AURA_BEARER_TOKEN 的既有部署恒 admin，行为零变化）：
//   ro    只读查询（列表/详情/dashboard/artifact）；一切 dispatch/编排/治理写操作拒绝
//   ops   常规派发（dispatch/编排/录制）；高影响工具（interactionGatedTools）拒绝
//   admin 全权（含高影响工具、节点治理、令牌治理）
// scope 经 middleware 注入请求 ctx，检查点收敛在 dispatch 入口（DispatchTool/RunOrchestration）——
// 查询面天然全档放行，写面按档拒绝，检查面最小。

// 令牌作用域档位。
const (
	ScopeReadOnly = "ro"
	ScopeOps      = "ops"
	ScopeAdmin    = "admin"
)

// interactionGatedTools 是高影响工具集（须 admin 档）：与 node 侧 requires_user_interaction_meta
// 声明的高危工具（kill_process/run_command）对齐 + file_push（写目标文件系统同级影响面）。
// controller 侧执行门控是该声明的强制面（声明即强制，M2 RBAC 欠债收口）。
var interactionGatedTools = map[string]struct{}{
	"run_command":  {},
	"kill_process": {},
	"file_push":    {},
}

// TokenScopes 是 bearer token → 作用域档位映射（main.go 装配：AURA_BEARER_TOKEN=admin、
// AURA_BEARER_TOKEN_OPS=ops、AURA_BEARER_TOKEN_RO=ro、AURA_FORWARD_TOKEN=admin）。
type TokenScopes map[string]string

// SingleToken 组装单 token 全权映射（既有单令牌部署/测试便捷构造）。
func SingleToken(token string) TokenScopes {
	if token == "" {
		return TokenScopes{}
	}
	return TokenScopes{token: ScopeAdmin}
}

// scopeKeyT 是请求 ctx 的 scope 注入键（unexported 空结构防跨包碰撞，同 peerCertFPKey 纪律）。
type scopeKeyT struct{}

var scopeKey = scopeKeyT{}

// ScopeFromContext 取回 middleware 注入的令牌作用域；未注入（内部调用/测试裸 ctx）返回空串——
// 检查侧把空 scope 视同 admin（兼容：单 token 部署与既有测试零变化）。
func ScopeFromContext(ctx context.Context) string {
	s, _ := ctx.Value(scopeKey).(string)
	return s
}

// CheckDispatchScope 按令牌作用域门控一次工具派发（DispatchTool/RunOrchestration 入口调用）：
// ro 拒一切派发；ops 拒高影响工具（interactionGatedTools，须 admin）；admin/空 scope 放行。
// 拒绝以 CodePermissionDenied 返回（调用方可辨识是权限而非节点/传输故障）。
func CheckDispatchScope(ctx context.Context, tool string) error {
	switch ScopeFromContext(ctx) {
	case ScopeReadOnly:
		return connect.NewError(connect.CodePermissionDenied,
			errors.New("token scope 'ro' cannot dispatch tools (read-only)"))
	case ScopeOps:
		if _, gated := interactionGatedTools[tool]; gated {
			return connect.NewError(connect.CodePermissionDenied,
				fmt.Errorf("tool %q requires an admin-scope token (high-impact, user-interaction gated)", tool))
		}
	}
	return nil
}

// auditDispatch 落一行结构化派发审计日志（批E C1：调用方 + 工具 + 节点 + 参数体量 + 令牌档；
// 参数内容不落日志——可能含敏感文本，体量足够定位异常调用）。tasks 表行是持久审计，此行是
// 实时可 grep 的操作留痕，两者互补。
func auditDispatch(ctx context.Context, entry, who, nodeID, tool string, argsBytes int) {
	slog.Info("dispatch audit",
		"entry", entry, "who", who, "node_id", nodeID, "tool", tool,
		"args_bytes", argsBytes, "scope", ScopeFromContext(ctx))
}

// BearerMiddleware 校验 Authorization: Bearer <token>，不匹配返回 401；命中即把该 token 的作用域
// 档位注入请求 ctx（批E C1 分级）。用于 :18080 REST 端口（该端口不做 mTLS，凭 bearer token 鉴权）。
// 未配置任何 token（空映射）时一律拒绝（fail closed），避免误开无鉴权入口。逐 token 常量时间比较
// （映射至多 4 词条，遍历成本可忽略；不以 map 查找短路，防时序侧信道）。
func BearerMiddleware(scopes TokenScopes, next http.Handler) http.Handler {
	const prefix = "Bearer "
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(scopes) == 0 {
			observability.IncAuthFailure("bearer")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, prefix) {
			observability.IncAuthFailure("bearer")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		got := []byte(strings.TrimPrefix(auth, prefix))
		matched := ""
		for token, scope := range scopes {
			if subtle.ConstantTimeCompare(got, []byte(token)) == 1 {
				matched = scope
			}
		}
		if matched == "" {
			observability.IncAuthFailure("bearer")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), scopeKey, matched)))
	})
}
