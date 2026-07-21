package transport

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"

	aurav1 "github.com/aura/controller/gen/aura/v1"
	"github.com/aura/controller/internal/gateway"
	"github.com/aura/controller/internal/observability"
	"github.com/aura/controller/internal/provisioner"
	"github.com/aura/controller/internal/registry"
	"github.com/aura/controller/internal/scheduler"
	"github.com/aura/controller/internal/storage"
	"github.com/aura/controller/internal/store"
)

// ControllerAdminServer 实现 aurav1connect.ControllerAdminHandler，服务 auractl/agent 管理面。
type ControllerAdminServer struct {
	registry    *registry.NodeRegistry
	gateway     *gateway.Gateway
	scheduler   *scheduler.Scheduler
	provisioner provisioner.EnvProvider // 可为 nil（未配置任何 provider 时环境接口返回 Unavailable）
	pg          *store.PGStore          // 可为 nil（M15 项目隔离判据源；nil=节点视同未归属，见 CheckNodeProjectAccess）
	artifacts   *storage.MinioStore     // 可为 nil（M16 self-update 制品签发源；经 SetArtifacts 装配期注入）
}

// NewControllerAdminServer 构造管理面服务。prov 是 EnvProvider（PVE 或 K8s，单选装配），
// 可为 nil（未配置任何 provider；main.go 保持接口为 nil 而非 typed-nil 以令下方 nil 判断生效）。
// sched 承接 M6 录制会话租约（StartTrace/StopTrace），与 gateway 直接持有各自所需组件的惯例一致。
// pg 供 M15 项目隔离判据（nodes.project 解析 + 列表视界过滤），可为 nil。
func NewControllerAdminServer(reg *registry.NodeRegistry, gw *gateway.Gateway, sched *scheduler.Scheduler, prov provisioner.EnvProvider, pg *store.PGStore) *ControllerAdminServer {
	return &ControllerAdminServer{registry: reg, gateway: gw, scheduler: sched, provisioner: prov, pg: pg}
}

// ListNodes 返回所有在线节点及其 online/unhealthy 状态。M15：项目令牌仅见本项目节点（按
// nodes.project 集合过滤；全域令牌零成本全量）。
func (s *ControllerAdminServer) ListNodes(
	ctx context.Context,
	_ *connect.Request[aurav1.ListNodesRequest],
) (*connect.Response[aurav1.ListNodesResponse], error) {
	nodes := s.registry.List()
	nodes, err := filterNodesByProject(ctx, s.pg, nodes)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&aurav1.ListNodesResponse{Nodes: nodes}), nil
}

// filterNodesByProject 按令牌项目视界过滤 NodeInfo 集（M15）：全域令牌原样返回（零 DB 成本）；项目
// 令牌以 ProjectNodeIDs 集合取交（registry 会话不携 project——归属是管理面持久列非节点自报，故按
// 库侧集合过滤而非读 NodeInfo 字段，在线/离线路径同一判据）。解析失败 fail-closed 报错。
func filterNodesByProject(ctx context.Context, pg *store.PGStore, nodes []*aurav1.NodeInfo) ([]*aurav1.NodeInfo, error) {
	tok := IdentityFromContext(ctx)
	if tok.Project == "" {
		return nodes, nil
	}
	if pg == nil {
		return nil, nil // 无持久层即无归属节点：项目令牌视界为空集（不变量：无 PG 即无 DB 项目令牌）
	}
	ids, err := pg.ProjectNodeIDs(ctx, tok.Project)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("resolve project nodes: %w", err))
	}
	allowed := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		allowed[id] = struct{}{}
	}
	out := make([]*aurav1.NodeInfo, 0, len(nodes))
	for _, n := range nodes {
		if _, ok := allowed[n.GetNodeId()]; ok {
			out = append(out, n)
		}
	}
	return out, nil
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
	// M15 项目隔离 + 审计归因：越界节点拒派发；who 未填时以令牌身份归因（tasks.who 落 DB 令牌名）。
	if err := CheckNodeProjectAccess(ctx, s.pg, req.Msg.GetNodeId()); err != nil {
		return nil, err
	}
	if id := IdentityFromContext(ctx); req.Msg.GetWho() == "" && id.Name != "" {
		req.Msg.Who = id.Name
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
	ctx context.Context,
	req *connect.Request[aurav1.StartTraceRequest],
) (*connect.Response[aurav1.StartTraceResponse], error) {
	// M15：录制租约同为节点寻址操作，项目令牌越界即拒（StopTrace 以 trace_id 随机句柄自证持有，不重检）。
	if err := CheckNodeProjectAccess(ctx, s.pg, req.Msg.GetNodeId()); err != nil {
		return nil, err
	}
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

// TokenIdentity 是一次请求的令牌身份（M15）：审计名 + 档位 + 项目视界。middleware 注入 ctx，全部
// 检查点（dispatch 档位门控 / 项目隔离 / 审计归因）统一从 ctx 取。零值（未注入：内部调用/测试裸
// ctx）视同全域 admin——与批E「空 scope 视同 admin」兼容语义一脉相承。
type TokenIdentity struct {
	Name    string // 审计名（env 令牌 "env:<scope>"；DB 令牌为其 name）
	Scope   string // ro | ops | admin（空=admin 兼容）
	Project string // 项目视界（''=全域；非空=仅见/仅控 nodes.project 同值节点，M15 唯一隔离规则）
}

// scopeKeyT 是请求 ctx 的身份注入键（unexported 空结构防跨包碰撞，同 peerCertFPKey 纪律）。
type scopeKeyT struct{}

var scopeKey = scopeKeyT{}

// ScopeFromContext 取回 middleware 注入的令牌作用域；未注入（内部调用/测试裸 ctx）返回空串——
// 检查侧把空 scope 视同 admin（兼容：单 token 部署与既有测试零变化）。
func ScopeFromContext(ctx context.Context) string {
	return IdentityFromContext(ctx).Scope
}

// IdentityFromContext 取回 middleware 注入的完整令牌身份（M15）；未注入返回零值（=全域 admin 兼容）。
func IdentityFromContext(ctx context.Context) TokenIdentity {
	id, _ := ctx.Value(scopeKey).(TokenIdentity)
	return id
}

// WithIdentity 把令牌身份注入 ctx（测试构造 + stream 桥类自持鉴权面复用；生产主路径经 BearerMiddlewareDB）。
func WithIdentity(ctx context.Context, id TokenIdentity) context.Context {
	return context.WithValue(ctx, scopeKey, id)
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

// ApiTokenSource 是 BearerMiddlewareDB 的 DB 实体令牌查验窄接口（M15；*store.PGStore 实现）。nil=
// 未启用（无 PG 部署 env-only，行为与批E 完全一致）。**typed-nil 警戒**（arch spec）：装配方（main.go）
// 必须仅在 PG 在位时才赋非 nil 接口值，绝不把具体类型 nil 指针经接口中转。
type ApiTokenSource interface {
	LookupApiToken(ctx context.Context, secretHash string) (store.ApiToken, error)
	TouchApiToken(ctx context.Context, id string) error
}

// BearerMiddleware 校验 Authorization: Bearer <token>（env 静态映射 only 形态；既有调用面/测试零变化）。
// M15 后为 BearerMiddlewareDB 的无 DB 源便捷包装。
func BearerMiddleware(scopes TokenScopes, next http.Handler) http.Handler {
	return BearerMiddlewareDB(scopes, nil, next)
}

// BearerMiddlewareDB 校验 Authorization: Bearer <token>，不匹配返回 401；命中即把令牌身份（M15
// TokenIdentity：审计名/档位/项目视界）注入请求 ctx。用于 :18080 REST 端口（该端口不做 mTLS，凭
// bearer token 鉴权）。匹配序：env 静态映射恒时比较优先（批E 语义原样，env 令牌恒全域）；未命中且
// tokens 非 nil 时按 sha256(明文) 点查 DB 实体令牌（有效性判据收敛在 SQL：未吊销未过期）。env 映射
// 空且无 DB 源时一律拒绝（fail closed），避免误开无鉴权入口。逐 env token 常量时间比较（映射至多
// 4 词条，遍历成本可忽略；不以 map 查找短路，防时序侧信道；DB 路径以哈希点查天然恒时）。
// DB 命中后 best-effort 异步节流回写 last_used（独立短超时 ctx，绝不阻塞/阻断鉴权）。
func BearerMiddlewareDB(scopes TokenScopes, tokens ApiTokenSource, next http.Handler) http.Handler {
	const prefix = "Bearer "
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(scopes) == 0 && tokens == nil {
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
		var identity TokenIdentity
		switch {
		case matched != "":
			identity = TokenIdentity{Name: "env:" + matched, Scope: matched}
		case tokens != nil:
			sum := sha256.Sum256(got)
			rec, err := tokens.LookupApiToken(r.Context(), hex.EncodeToString(sum[:]))
			if err != nil {
				if store.IsNotFound(err) {
					observability.IncAuthFailure("bearer")
					http.Error(w, "unauthorized", http.StatusUnauthorized)
					return
				}
				// DB 故障与「令牌无效」诚实区分：503 供运维定位，仍 fail-closed 不放行。
				slog.Warn("api token lookup failed", "err", err)
				observability.IncAuthFailure("bearer")
				http.Error(w, "auth backend unavailable", http.StatusServiceUnavailable)
				return
			}
			identity = TokenIdentity{Name: rec.Name, Scope: rec.Scope, Project: rec.Project}
			go func(id string) {
				tctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				if terr := tokens.TouchApiToken(tctx, id); terr != nil {
					slog.Debug("api token touch failed", "err", terr)
				}
			}(rec.ID)
		default:
			observability.IncAuthFailure("bearer")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r.WithContext(WithIdentity(r.Context(), identity)))
	})
}

// CheckNodeProjectAccess 按令牌项目视界门控一次节点寻址操作（M15 唯一隔离规则：token.project ∈
// {全域, node.project}）。全域令牌（含 env/内部路径零值身份）零成本短路不触 DB；项目令牌解析
// nodes.project 判定，解析失败 fail-closed（宁拒不越界；全域已短路故仅项目令牌受影响）。拒绝以
// CodePermissionDenied 返回（权限语义；内网管控面不做存在性隐藏，简单诚实优先）。pg nil（无持久层）
// 时节点视同未归属——与「无 PG 即无 DB 项目令牌」不变量自洽，env 全域令牌行为零变化。
func CheckNodeProjectAccess(ctx context.Context, pg *store.PGStore, nodeID string) error {
	tok := IdentityFromContext(ctx)
	if tok.Project == "" {
		return nil
	}
	nodeProject := ""
	if pg != nil {
		p, err := pg.NodeProject(ctx, nodeID)
		if err != nil {
			slog.Warn("resolve node project failed", "node_id", nodeID, "err", err)
			return connect.NewError(connect.CodePermissionDenied,
				errors.New("cannot verify node project for this token"))
		}
		nodeProject = p
	}
	if tok.Project != nodeProject {
		return connect.NewError(connect.CodePermissionDenied,
			fmt.Errorf("token project %q does not cover this node", tok.Project))
	}
	return nil
}
