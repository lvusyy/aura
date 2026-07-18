package transport

import (
	aurav1connect "github.com/aura/controller/gen/aura/v1/aurav1connect"
	"github.com/aura/controller/internal/fusion"
	"github.com/aura/controller/internal/orchestrator"
	"github.com/aura/controller/internal/registry"
	"github.com/aura/controller/internal/scheduler"
	"github.com/aura/controller/internal/storage"
	"github.com/aura/controller/internal/store"
)

// 编译期断言：*ConsoleServiceServer 完整实现 ConsoleService 的 12 方法接口（M8 9 方法 + M9
// SubmitFusion/GetFusionResult + M10 ReadNodeScreen）。任一 handler stub 缺失/签名不符即在本包编译失败（而非延迟到
// main.go 装配处），令各真实现任务替换独立 stub 文件时立即得到接口契约反馈。
var _ aurav1connect.ConsoleServiceHandler = (*ConsoleServiceServer)(nil)

// ConsoleServiceServer 实现 aurav1connect.ConsoleServiceHandler，服务 aura-console 管理台（M8）。
// 依赖全字段一次性装配：reg/store/sched 供只读查询面（TASK-007），orch 供并行编排面（TASK-005），
// minio 供产物代理读（TASK-011）。handler 分置于 console_*.go，各真实现任务替换独立文件零冲突
// （对抗 H：connect 接口一次性满足编译约束，多真实现并行改同 struct 会同文件冲突，故分文件 stub）。
type ConsoleServiceServer struct {
	reg   *registry.NodeRegistry
	store *store.PGStore
	sched *scheduler.Scheduler
	orch  *orchestrator.Orchestrator
	minio *storage.MinioStore // 可为 nil（未配置 MinIO 时 GetArtifact 返回 Unavailable）

	// M9 融合面（T10）：engine 经 SetFusionEngine 装配期可空注入（detector 未配置保 nil，
	// SubmitFusion 降级 Unavailable）；fusionJobs/fusionResults 是 store/minio 的窄接口投影
	// （console_fusion.go 定义），构造时按 typed-nil 纪律装配——同包单测注入 fake 以覆盖
	// submit/poll 正反路径（PGStore/MinioStore 为具体类型无法直接 mock）。
	engine        *fusion.Engine
	fusionJobs    fusionJobStore
	fusionResults fusionResultGetter

	// M10-P1 租约期 UX（T13）：WatchFleet 帧回填录制占用态（FleetEvent.recordings）的租约读面，
	// 经 SetLeaseStore 装配期可空注入（沿 SetFusionEngine 惯例）。nil（裸构造单测/未注入）时
	// recordings 恒空——缺省字段即「无录制」语义，零断裂。
	leases store.LeaseStore

	// M13 直连 MCP agent 观测记录器：三 RPC（GetAgentObservability/ListAgentSessions/ListAgentCalls）
	// 读面。经 SetAgentObs 装配期注入——与 NodeControlServer 共享同一实例（内存兜底模式下写读须同缓冲）。
	// nil（裸构造单测/未注入）时三 handler 返回空/Unavailable，不 panic。
	agentObs *store.AgentObs
}

// NewConsoleServiceServer 构造管理台服务。pg 可为 nil（纯内存运行）、minio 可为 nil（未配置 MinIO 时
// 产物代理降级 Unavailable）——与 main.go typed-nil 纪律一致（保持接口/指针为 nil 而非 typed-nil）。
func NewConsoleServiceServer(reg *registry.NodeRegistry, pg *store.PGStore, sched *scheduler.Scheduler, orch *orchestrator.Orchestrator, minio *storage.MinioStore) *ConsoleServiceServer {
	s := &ConsoleServiceServer{reg: reg, store: pg, sched: sched, orch: orch, minio: minio}
	// 融合面窄接口仅在依赖真实非 nil 时装配（typed-nil 纪律：具体类型 nil 指针经接口中转会成
	// 非 nil 接口，致 handler 判空失效）。
	if pg != nil {
		s.fusionJobs = pg
	}
	if minio != nil {
		s.fusionResults = minio
	}
	return s
}

// SetFusionEngine 注入 M9 视觉融合引擎（装配期一次性调用，早于服务监听——仿 scheduler.SetStorage /
// gateway.SetUploader 可空注入惯例，不改 5 参构造签名，既有调用点零改）。未调用（detector 端点
// 未配置）时 engine 保 nil，SubmitFusion 降级 Unavailable。
func (s *ConsoleServiceServer) SetFusionEngine(e *fusion.Engine) {
	s.engine = e
}

// SetLeaseStore 注入租约读面（T13 租约期 UX：fleet 帧携「谁在录制」显式态，补齐被租节点只见
// 泛化 E_BUSY 的观测盲区）。装配期一次性调用，早于服务监听——不改 5 参构造签名，既有调用点零改。
// console 面直连 LeaseStore 读（C-3 最小形态），scheduler 零动。
func (s *ConsoleServiceServer) SetLeaseStore(ls store.LeaseStore) {
	s.leases = ls
}

// SetAgentObs 注入直连 MCP agent 观测记录器（M13，装配期一次性调用）。须与 NodeControlServer.SetAgentObs
// 传入同一实例——内存兜底模式下写（收帧）读（console）共享同一环形缓冲，PG 模式下两实例读写同库亦可。
func (s *ConsoleServiceServer) SetAgentObs(ao *store.AgentObs) {
	s.agentObs = ao
}
