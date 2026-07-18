// Package scheduler 将管理面下发的工具调用调度到目标节点：
//   - per-node 串行有界队列（buffered chan + 单 worker goroutine），队列满即 E_BUSY；
//   - 节点无会话/unhealthy 直接 E_NODE_OFFLINE，不入队；
//   - 经 registry.NodeSession.Dispatch 下发，外加控制面兜底 timer（deadline_ms + grace）→ E_TIMEOUT；
//   - 每次调用写 tasks 审计表（store 为 nil 时跳过）。
//
// 传输层错误以 proto ErrorCode 枚举名的字符串码返回，由 gateway 合成 Envelope。
package scheduler

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"

	aurav1 "github.com/aura/controller/gen/aura/v1"
	"github.com/aura/controller/internal/observability"
	"github.com/aura/controller/internal/registry"
	"github.com/aura/controller/internal/store"
)

// 传输层错误码，派生自生成的 proto ErrorCode 枚举名（.String() 返回枚举成员名）。
// 用 var 而非 const：枚举 .String() 是运行时调用，非常量表达式。此派生把「字符串码 ↔ proto
// 枚举」耦合前移到编译期——proto 改名/删除对应枚举成员即在此编译失败（G-2）。
var (
	CodeBusy        = aurav1.ErrorCode_E_BUSY.String()
	CodeNodeOffline = aurav1.ErrorCode_E_NODE_OFFLINE.String()
	CodeTimeout     = aurav1.ErrorCode_E_TIMEOUT.String()
	CodeInternal    = aurav1.ErrorCode_E_INTERNAL.String()
	// CodeUnsupported 用于只读旁路白名单外工具的拒绝（T11）：与 MCP 层剔除工具的 call-time 兜底同族语义。
	CodeUnsupported = aurav1.ErrorCode_E_UNSUPPORTED.String()
)

const (
	// maxQueue 是 per-node 队列深度（含排队与在执行），满即 E_BUSY。
	maxQueue = 16
	// graceTimeout 是控制面兜底 timer 相对 deadline_ms 的宽限，覆盖网络往返。
	graceTimeout = 2 * time.Second
	// defaultTimeout 用于 deadline_ms<=0 的调用。
	defaultTimeout = 30 * time.Second
	// defaultLeaseTTL 是录制会话独占租约的默认存活期（NewScheduler 收到 <=0 时取此值）。
	// 防泄漏：崩溃/失联的录制方不能永久锁住节点，惰性过期后自动释放（Locked-2）。
	defaultLeaseTTL = 30 * time.Minute

	// per-node 分布式锁参数（T6，ha-contract §1.3）：获锁失败（他副本持有）有界短重试后 E_BUSY——
	// worker 本就串行，阻塞 1.5s 无害；锁 TTL 在 job 执行上限外再留 slack（锁先于 job 过期 =
	// 串行性窗口，必须 ≥ 执行上限）。
	nodeLockRetries    = 3
	nodeLockRetryDelay = 500 * time.Millisecond
	nodeLockSlack      = 2 * time.Second
)

// ObjectPutter 是 M6 capture 旁路卸载逐步截图所需的最小对象存储窄接口（storage.MinioStore 实现之）。
// 在 scheduler 侧定义遵循依赖倒置 + 第三方 SDK 窄接口隔离（coding spec）：scheduler 不 import storage
// 完整表面、单测可注入 fake，MinIO SDK 漂移收敛在 storage adapter。为 nil（未配置 MinIO）时截图不卸桶，
// 保留内联截图存原 envelope（best-effort 降级，Locked-4）。
type ObjectPutter interface {
	PutObject(ctx context.Context, key string, data []byte, contentType string) error
}

// NodeLocker 是 execute 段 per-node 分布式互斥锁的最小窄接口（store.RedisStore 实现之，T6，
// ha-contract §1.3）。与 node:owner 归属登记两原语分立不可混同：owner=转发路由依据（T8 消费），
// lock=per-node 任务互斥——scheduler per-node 队列串行不变量的分布式兜底，仅在「双副本同时自认
// owner」的脑裂窗口起作用。为 nil（未配置 Redis / 单副本）时整段旁路：进程内单 worker 已串行。
type NodeLocker interface {
	// AcquireNodeLock 尝试获锁（SET NX，值=replicaID），返回是否获得。
	AcquireNodeLock(ctx context.Context, nodeID, replicaID string, ttl time.Duration) (bool, error)
	// ReleaseNodeLock 释放锁（compare-and-del：仍为 replicaID 持有才删，防误删他副本后获的锁）。
	ReleaseNodeLock(ctx context.Context, nodeID, replicaID string) error
}

// OwnerReader 是 dispatch 入口 owner 路由的最小窄接口（store.RedisStore 实现之，T8，
// ha-contract §1.4）：node:owner 归属键由 transport 侧三点位登记/续租/清理（T6），此处只读，
// 作为 Ready 失败时跨副本转发的路由依据。键不存在返回 ("", nil)——视同无 owner，不转发。
type OwnerReader interface {
	GetNodeOwner(ctx context.Context, nodeID string) (string, error)
}

// Scheduler 持有 per-node 队列与可选的审计 store。
type Scheduler struct {
	registry *registry.NodeRegistry
	store    *store.PGStore // 可为 nil（未配置 PG 时不写审计）
	storage  ObjectPutter   // 可为 nil（未配置 MinIO 时逐步截图不卸桶，M6 capture）

	mu       sync.Mutex
	queues   map[string]chan job
	draining bool // SIGTERM 排水中：Dispatch 停收新任务回 E_BUSY（受 mu 保护）

	// inflight 计数已受理（admit）但未执行完的任务；Drain 据此等待在途任务排空。
	inflight sync.WaitGroup

	// M6 录制会话独占租约（Locked-2），T4（ha-contract §3）起经 LeaseStore 抽象：单副本进程内
	//（InMemoryLeaseStore：进程死即租约随 map 消失，self-heal 自愈）或多副本 Redis 共享
	//（RedisLeaseStore：StartTrace 独占/checkLease 门控/seq 单调跨副本一致）。TTL（进程内惰性/
	// Redis 原生）兜底崩溃/失联的录制方不能永久锁住节点。
	leases   store.LeaseStore
	leaseTTL time.Duration // 租约存活期（Acquire 时传入，默认 30min 可配）

	// T6 per-node 分布式锁（ha-contract §1.3，脑裂兜底）：execute 段每 job 执行前 Acquire、执行毕
	// Release。locker 为 nil（未配置 Redis）时整段旁路——单副本行为零变化红线。replicaID 是锁值
	//（compare-and-del 匹配依据），与 transport 侧 owner 登记同源 env（T8 于 main.go 装配注入）。
	locker    NodeLocker
	replicaID string

	// T8 跨副本转发（ha-contract §1.4）：dispatch 入口 Ready 失败时按 node:owner 归属把请求转投
	// owner 副本 REST 全链（见 forward.go）。nil（未配置 AURA_REPLICA_PEERS/Redis）= 全部现行
	// 路径——单副本行为零变化红线。
	forwarder *Forwarder
}

// NewScheduler 构造调度器。store 可为 nil。leaseTTL 是录制会话独占租约的存活期（<=0 取
// defaultLeaseTTL=30min）——TTL 过期兜底崩溃的录制方不能永久锁节点（Locked-2）。leases 是
// 租约存储（T4 双实现，ha-contract §3）：nil 取进程内 InMemoryLeaseStore（纯内存契约，单副本/
// 测试默认形态，回归零变化）；多副本部署由 main.go 注入 RedisLeaseStore。
func NewScheduler(reg *registry.NodeRegistry, st *store.PGStore, leaseTTL time.Duration, leases store.LeaseStore) *Scheduler {
	if leaseTTL <= 0 {
		leaseTTL = defaultLeaseTTL
	}
	if leases == nil {
		leases = store.NewInMemoryLeaseStore()
	}
	return &Scheduler{
		registry: reg,
		store:    st,
		queues:   make(map[string]chan job),
		leases:   leases,
		leaseTTL: leaseTTL,
	}
}

// SetStorage 注入 M6 capture 逐步截图卸桶用对象存储（装配期一次性调用，早于任何 Dispatch，与
// gateway.SetUploader 同惯例——规避 sched 构造早于 minioStore 的时序）。传入 nil（未配置 MinIO）时
// capture 保留内联截图存原 envelope、不阻断 traces INSERT（best-effort 降级，Locked-4）。
func (s *Scheduler) SetStorage(p ObjectPutter) {
	s.storage = p
}

// SetNodeLocker 注入 execute 段 per-node 分布式锁与本副本标识（装配期一次性调用，早于任何
// Dispatch，SetStorage 同惯例；多副本装配归 T8 的 main.go 窗口，与 forwarder/peers 一并注入
// AURA_REPLICA_ID 同源值）。l 为 nil 时锁段旁路（单副本/纯内存形态行为零变化）。
func (s *Scheduler) SetNodeLocker(l NodeLocker, replicaID string) {
	s.locker = l
	s.replicaID = replicaID
}

// SetForwarder 注入跨副本转发器（装配期一次性调用，早于任何 Dispatch，SetNodeLocker 同惯例；
// main.go 在 AURA_REPLICA_PEERS 与 Redis 同时就位才装配）。nil 时 Ready 失败一律现行
// E_NODE_OFFLINE（单副本零变化红线）。
func (s *Scheduler) SetForwarder(f *Forwarder) {
	s.forwarder = f
}

// job 是一次待执行的下发；resultCh 缓冲 1，保证 worker 回填不阻塞（调用方可能已因超时离开）。
type job struct {
	ctx      context.Context
	req      *aurav1.ToolRequest
	resultCh chan result
	traceID  string // M6 活跃录制会话标识（空=非录制 dispatch）；execute 侧 capture 消费于 TASK-004
}

type result struct {
	resp *aurav1.ToolResponse
	code string // 传输层错误码；空表示成功
	err  error
}

// Dispatch 同步下发一次工具调用，返回节点响应或传输层错误码（既有签名，gateway/M6 调用面不变）。
// code 非空即传输层错误（resp 为 nil）；code 为空即成功（resp 有效）。内部委托 dispatch，丢弃 task_id。
func (s *Scheduler) Dispatch(ctx context.Context, nodeID, tool string, jsonArgs []byte, deadlineMs int64, who, traceID string) (*aurav1.ToolResponse, string, error) {
	resp, _, code, err := s.dispatch(ctx, nodeID, tool, jsonArgs, deadlineMs, who, traceID)
	return resp, code, err
}

// DispatchTracked 同 Dispatch，额外返回本次下发生成的 task_id（审计关联键）。供并行编排层（orchestrator）
// 在 fan-out 后回填 tasks.orchestration_id、并填充 EnvResult.task_id——编排概念不下沉 scheduler，仅借此
// 归还审计键（自然的下发副产物，非队列语义改动，per-node 串行不变）。task_id 为空表示下发在建审计行前
// 即被拒（draining/offline/lease busy），无 tasks 行可关联。
func (s *Scheduler) DispatchTracked(ctx context.Context, nodeID, tool string, jsonArgs []byte, deadlineMs int64, who, traceID string) (*aurav1.ToolResponse, string, string, error) {
	return s.dispatch(ctx, nodeID, tool, jsonArgs, deadlineMs, who, traceID)
}

// dispatch 是下发核心实现，较对外 Dispatch 多归还生成的 task_id（第 2 返回值）。队列语义（admit/enqueue/
// worker 单点串行）与 M2 完全一致——本次仅把内部已生成的 task_id 透出，供编排层关联，不触碰任何队列路径。
func (s *Scheduler) dispatch(ctx context.Context, nodeID, tool string, jsonArgs []byte, deadlineMs int64, who, traceID string) (*aurav1.ToolResponse, string, string, error) {
	// 出站 span（REST→gRPC 两跳的控制面侧一跳）：父为 REST 拦截器 span，经 ctx 关联；
	// 未启用追踪时为 no-op。task_id 生成后补为 span 属性。
	ctx, span := observability.StartDispatchSpan(ctx, nodeID, tool)
	defer span.End()

	// 0) 排水闸门 + in-flight 计数原子化：draining 检查与 inflight.Add(1) 并入同一 mu 临界区（见 admit），
	// 杜绝「已过闸门尚未计数」窗口——否则 Drain 置 draining 后 Wait 可能漏等此刻正要入队的任务。
	// draining 中直接 E_BUSY（不计数、不入队；SIGTERM 优雅关停触发，kill -9 不经此路）。
	if !s.admit() {
		return nil, "", CodeBusy, errors.New("controller draining, not accepting new tasks")
	}

	// 1) 就绪校验：无会话或 unhealthy → 先试跨副本转发（T8，ha-contract §1.4），不可转发则回滚
	// 计数并拒绝，不入队。转发发生在「入队前」的入口层：非 owner 副本对该 node 不建队列、不写
	// 审计、不做 checkLease（租约在 owner 侧全链恰好执行一次，租约表经 T4 已共享）。入站已带
	// 转发标记时不查 owner（GetNodeOwner 此刻指向必 stale）、不二次转发——只信本副本
	// registry.Ready，直接 E_NODE_OFFLINE 终态（一跳上限，m-1 防 A↔B 弹球，错误码表行 8）。
	if _, ok := s.registry.Ready(nodeID); !ok {
		if !isInboundForward(ctx) && s.forwarder != nil {
			if env, ferr, attempted := s.forwarder.TryForward(ctx, nodeID, tool, jsonArgs, deadlineMs, who, traceID); attempted {
				// inflight 计数（admit 的 Add(1)）在转发全程保持，返回前 Done——转发也是本副本
				// 已受理的请求，SIGTERM 排水等待在途转发，零特判。
				s.inflight.Done()
				if ferr != nil {
					// 转发网络错/非 2xx：一跳终态，不重试不改投（错误码表行 7，err 含 peer 端点与原因）。
					return nil, "", CodeNodeOffline, ferr
				}
				// 行 6：owner 所产 envelope 逐字节透传。task_id=""（owner 侧生成并落审计，D12 响应
				// 不加字段回传——与「下发在建审计行前被拒」语义同形，orchestrator 已容忍空 task_id）；
				// code 恒 ""（不做 envelope 回解析再派生 code，哑管道纪律）。
				return &aurav1.ToolResponse{JsonEnvelope: env}, "", "", nil
			}
		}
		s.inflight.Done()
		return nil, "", CodeNodeOffline, fmt.Errorf("node %s offline or unhealthy", nodeID)
	}

	// 1') 录制租约校验（M6，Locked-2）：置于 recordTask(下一步) 之前——被拒的 dispatch 不写 tasks
	// 审计行（checker 建议）。node 无活跃租约 → 放行（空 trace_id 的常规 dispatch 不受影响）；非持有者
	// （含空 trace_id）对被租 node 的 dispatch → 复用 E_BUSY 枚举零新码拒绝。惰性 TTL 过期在 checkLease
	// 内自动释放（见 checkLease）。
	if err := s.checkLease(ctx, nodeID, traceID); err != nil {
		s.inflight.Done()
		return nil, "", CodeBusy, err
	}

	// 2) 生成 task_id（同时作为 NodeSession 请求/响应关联键）并写审计。审计（CreateTask/INSERT）在
	// Add 之后、锁外，且须在入队前完成：否则 worker 可能先于 INSERT 执行 UpdateTaskStatus 致审计错乱。
	taskID := uuid.NewString()
	span.SetAttributes(attribute.String("aura.task_id", taskID))
	// 批E：trace_id 随审计行落库（录制会话 → 任务下钻关联；空=非录制，store 侧落 NULL）。
	s.recordTask(store.TaskRecord{ID: taskID, NodeID: nodeID, Tool: tool, Status: "queued", Who: who, TraceID: traceID})

	j := job{
		ctx: ctx,
		req: &aurav1.ToolRequest{
			TaskId:     taskID,
			Tool:       tool,
			JsonArgs:   jsonArgs,
			DeadlineMs: deadlineMs,
			// GAP-4 controller leg（D11）：录制会话标识穿透至节点（proto ToolRequest.trace_id=5
			// 字段已在，节点默认忽略 additive）；转发路径经 DispatchToolRequest.trace_id 到 owner
			// 后走同一构造点，与本地路径同源穿透。
			TraceId: traceID,
		},
		resultCh: make(chan result, 1),
		traceID:  traceID,
	}

	// 3) 入队：懒建 per-node 队列并非阻塞入队（enqueue 全程持 mu，与 ReclaimNode 的 close 互斥，
	// 杜绝 close/send 竞态的 send-on-closed panic）。满即回滚计数 + E_BUSY（task_id 已归还，编排层可关联该失败行）。
	depth, ok := s.enqueue(nodeID, j)
	if !ok {
		s.inflight.Done()
		s.finishTask(taskID, "busy", nil)
		return nil, taskID, CodeBusy, fmt.Errorf("node %s queue full", nodeID)
	}
	// 族3：入队后刷新该节点队列深度（低基数 label：node_id）。
	observability.SetQueueDepth(nodeID, depth)

	// 4) 等结果或调用方 ctx 取消。
	select {
	case r := <-j.resultCh:
		return r.resp, taskID, r.code, r.err
	case <-ctx.Done():
		return nil, taskID, CodeTimeout, ctx.Err()
	}
}

// —— T11 console 读旁路（读写分离，M10-P1 SC-3①）——————————————————————————————————————

// readOnlyTools 是读旁路白名单：无副作用、不产 needs_upload 大产物旁路、信封自含（screenshot 内联
// base64）——三条全满足才可绕 per-node 队列直达会话（未来扩 get_a11y_tree 等按同准则准入）。
// 串行约束的真源是控制面 per-node 队列（node 侧每请求独立 spawn 本就并发），故旁路只动控制面单侧。
var readOnlyTools = map[string]struct{}{
	"screenshot": {},
}

// readOnlyDefaultTimeout 是读旁路 deadline_ms<=0 时的兜底超时：截图秒级往返，无需 defaultTimeout
// 的 30s 长窗——读通道快失败快重试，console 轮询下一帧自愈。
const readOnlyDefaultTimeout = 10 * time.Second

// DispatchReadOnly 同步下发一次只读工具调用，绕 per-node 串行队列直达节点会话（console 读通道与
// agent 任务写通道分离）：
//   - 白名单外工具拒绝（E_UNSUPPORTED）：只读通道不承接任何有副作用的调用；
//   - 不 admit/不入队/不写审计：NodeSession pending-map 多路复用天然并发安全，读流量不占 per-node
//     worker，任务队列时延与读吞吐互不影响（SC-3① 判据）；亦不参与排水——读是幂等轮询，关停时
//     在途读随流断自然失败，排水语义只覆盖会改设备状态的任务；
//   - 租约豁免：录制租约期允许只读看屏正是 UX 目标——只读无副作用不破坏录制独占，checkLease
//     门控只保护写通道（非只读 dispatch 的 E_BUSY 语义不变）；看屏也不计录制步（TraceId 恒空）；
//   - Ready 失败复用 T8 owner 路由：经 forwarder 转投 owner 副本的 ReadNodeScreen 同 RPC（owner
//     侧同样落本方法，绕队列+租约豁免语义端到端保持）；入站已带转发标记不查 owner 不二次转发
//     （m-1 一跳上限，与写路径同纪律）。
//
// 返回语义与 Dispatch 一致：code 非空即传输层错误（resp 为 nil）；code 为空即成功（resp 有效）。
func (s *Scheduler) DispatchReadOnly(ctx context.Context, nodeID, tool string, jsonArgs []byte, deadlineMs int64, who string) (*aurav1.ToolResponse, string, error) {
	if _, ok := readOnlyTools[tool]; !ok {
		return nil, CodeUnsupported, fmt.Errorf("tool %s is not in the read-only whitelist", tool)
	}

	sess, ok := s.registry.Ready(nodeID)
	if !ok {
		if !isInboundForward(ctx) && s.forwarder != nil {
			if env, ferr, attempted := s.forwarder.TryForwardReadOnly(ctx, nodeID, tool, jsonArgs, deadlineMs, who); attempted {
				if ferr != nil {
					return nil, CodeNodeOffline, ferr
				}
				// owner 所产 envelope 逐字节透传，code 恒 ""（哑管道纪律与写路径行 6 同源）。
				return &aurav1.ToolResponse{JsonEnvelope: env}, "", nil
			}
		}
		return nil, CodeNodeOffline, fmt.Errorf("node %s offline or unhealthy", nodeID)
	}

	timeout := readOnlyDefaultTimeout
	if deadlineMs > 0 {
		timeout = time.Duration(deadlineMs)*time.Millisecond + graceTimeout
	}
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// task_id 仅作 NodeSession pending-map 的请求/响应关联键，读不落 tasks 审计表。
	resp, err := sess.Dispatch(rctx, &aurav1.ToolRequest{
		TaskId:     uuid.NewString(),
		Tool:       tool,
		JsonArgs:   jsonArgs,
		DeadlineMs: deadlineMs,
	})
	if err != nil {
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			return nil, CodeTimeout, err
		case errors.Is(err, registry.ErrNodeGone):
			return nil, CodeNodeOffline, err
		}
		return nil, CodeInternal, err
	}
	return resp, "", nil
}

// admit 在单一 mu 临界区内完成「排水闸门检查 + in-flight 计数」：未排水则 Add(1) 返回 true，
// 排水中返回 false（不计数）。与 Drain 置 draining 的临界区互斥，保证一旦 Drain 置位，
// 后续 admit 一律拒绝，Drain.Wait 不会漏等任何已受理任务（Fix：闸门与 WaitGroup 原子化）。
func (s *Scheduler) admit() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.draining {
		return false
	}
	s.inflight.Add(1)
	return true
}

// enqueue 懒建 per-node 队列（首次起单 worker，保证同节点串行执行）并尝试非阻塞入队。
// 全程持 mu：与 ReclaimNode 的 close(q) 互斥，杜绝并发 close/send 竞态（send-on-closed panic）。
// 返回入队后的队列深度与是否入队成功（false = 队列满）。
func (s *Scheduler) enqueue(nodeID string, j job) (int, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	q, ok := s.queues[nodeID]
	if !ok {
		q = make(chan job, maxQueue)
		s.queues[nodeID] = q
		go s.worker(nodeID, q)
	}
	select {
	case q <- j:
		return len(q), true
	default:
		return 0, false
	}
}

// ReclaimNode 回收指定节点的 per-node 队列：从 map 摘除并 close 队列通道，触发其 worker 在途任务
// 排空后自然退出（range 结束）；worker 退出时清理队列深度指标序列（见 worker）。由 registry 移除
// 节点会话时经 SetRemovalHook 注入的闭包调用（registry 不 import scheduler，维持 M2 分层）。
// 持 mu 与 enqueue 的非阻塞 send 互斥，杜绝 close/send 竞态。节点无队列时 no-op（幂等）。
//
// 竞态说明：若移除后仍有并发 Dispatch 抢在 close 前已过 registry.Ready、又在 close 后经 enqueue 重建
// 队列，则该请求由 worker 的 execute 二次 Ready 检查兜底回 E_NODE_OFFLINE；重建的空队列留待该节点
// 下次移除时再回收（可接受残留，不追求即时回收以换取简单正确）。
func (s *Scheduler) ReclaimNode(nodeID string) {
	s.mu.Lock()
	if q, ok := s.queues[nodeID]; ok {
		delete(s.queues, nodeID)
		close(q)
	}
	s.mu.Unlock()
}

// QueueDepth 返回 nodeID 当前 per-node 队列深度（缓冲通道内排队 + 待执行的 job 数）；节点无队列返回 0
// （nil chan 的 len 为 0，无需额外判空）。只读快照：持 s.mu 与 enqueue 的入队、ReclaimNode 的 close 互斥，
// 杜绝读到正被 close 的通道。供 console 执行墙展示各节点积压（对抗 G）；零改动既有 Dispatch/enqueue 语义。
func (s *Scheduler) QueueDepth(nodeID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.queues[nodeID])
}

// worker 串行消费单节点队列。队列被 ReclaimNode close 且在途任务排空后 range 自然结束，
// worker 退出前清理该节点的队列深度指标序列（避免 gauge 序列随节点 churn 泄漏）。
func (s *Scheduler) worker(nodeID string, ch <-chan job) {
	for j := range ch {
		s.execute(nodeID, j)
		s.inflight.Done() // 与 admit 的 Add(1) 配平，供 Drain 等待
		// 族3：出队执行后刷新队列深度，反映排空。
		observability.SetQueueDepth(nodeID, len(ch))
	}
	// 队列已回收（close + drain 完毕）：清理指标序列。清理置于 worker 退出处（而非 ReclaimNode 内）
	// 是刻意为之——排空在途任务时上面的 SetQueueDepth 会重建序列，若在 close 时即删会被随即重建而复漏；
	// 退出点在最后一次 SetQueueDepth 之后，删除即终态。若节点已重连重建队列（map 中已有新通道），
	// 则新 worker 接管该序列，本处跳过以免误删。
	s.mu.Lock()
	_, rebuilt := s.queues[nodeID]
	s.mu.Unlock()
	if !rebuilt {
		observability.DeleteQueueDepth(nodeID)
	}
}

// ReconcileOrphans 启动自愈（HA 单副本自愈三件套之一）：把上次控制面异常退出（kill -9，SIGTERM
// 优雅排水未及运行）时遗留在非终态（queued/running）的任务行如实置 orphaned，消除悬挂 running。
// at-least-once 语义：不盲目重放原任务——审计表不留 json_args/deadline 无从精确重建，且不承诺
// exactly-once 续跑；系统恢复由后续新派发的同类任务证明（见 tools/m3-chaos.py）。store 为 nil
// （纯内存）时 no-op。用调用方传入的启动 ctx（与请求生命周期解耦），返回置换行数。
func (s *Scheduler) ReconcileOrphans(ctx context.Context) (int64, error) {
	if s.store == nil {
		return 0, nil
	}
	n, err := s.store.MarkOrphanedTasks(ctx)
	if err != nil {
		return 0, err
	}
	if n > 0 {
		slog.Warn("startup reconcile: orphan tasks marked orphaned", "count", n)
	}
	return n, nil
}

// Drain 优雅排水（SIGTERM 触发，main.go 关停序调用）：置 draining 停收新任务（Dispatch 起点回
// E_BUSY），等 per-node 队列在途任务全部执行完（审计随各 execute 同步 flush）或 ctx 超时后返回。
// 净新符号（ISS-05：信号在 main.go 早已 signal.Notify 注册，此处不重复注册）。kill -9 不产生信号，
// 排水自然不跑——遗留孤儿由下次启动 ReconcileOrphans 兜底（语义分清：Drain=优雅退出零丢在途；
// Reconcile=崩溃退出事后归置）。
func (s *Scheduler) Drain(ctx context.Context) {
	s.mu.Lock()
	s.draining = true
	s.mu.Unlock()
	s.drainQueues(ctx)
}

// drainQueues 阻塞至所有 per-node 队列在途任务执行完毕（inflight 归零）或 ctx 到期。
func (s *Scheduler) drainQueues(ctx context.Context) {
	done := make(chan struct{})
	go func() {
		s.inflight.Wait()
		close(done)
	}()
	select {
	case <-done:
		slog.Info("scheduler drained: all in-flight tasks completed")
	case <-ctx.Done():
		slog.Warn("scheduler drain timed out; in-flight tasks may be incomplete", "err", ctx.Err())
	}
}

// execute 执行单次下发：兜底 timer + 错误码映射 + 审计更新。
func (s *Scheduler) execute(nodeID string, j job) {
	taskID := j.req.GetTaskId()

	sess, ok := s.registry.Ready(nodeID)
	if !ok {
		s.finishTask(taskID, "offline", nil)
		j.resultCh <- result{code: CodeNodeOffline, err: fmt.Errorf("node %s gone", nodeID)}
		return
	}

	timeout := defaultTimeout
	if j.req.GetDeadlineMs() > 0 {
		timeout = time.Duration(j.req.GetDeadlineMs())*time.Millisecond + graceTimeout
	}

	// T6 per-node 分布式锁（脑裂兜底，ha-contract §1.3）：sess.Dispatch 前获锁、执行毕（含错误路径）
	// defer 释放。TTL = job 执行上限 + slack（deadline 分支 timeout 已含 grace；default 分支补齐
	// grace）——锁先于 job 过期即串行性窗口，必须 ≥ 执行上限。获锁失败（他副本正持有）＝双副本
	// 同时自认 owner 的脑裂窗口，有界短重试后该 job 以 E_BUSY 失败：错误可见，「node 正被他副本
	// 执行」与 E_BUSY 家族语义一致。locker 未注入（单副本/纯内存）时 acquire/release 均直通。
	lockTTL := timeout + nodeLockSlack
	if j.req.GetDeadlineMs() <= 0 {
		lockTTL = defaultTimeout + graceTimeout + nodeLockSlack
	}
	if !s.acquireNodeLock(j.ctx, nodeID, lockTTL) {
		s.finishTask(taskID, "busy", nil)
		j.resultCh <- result{code: CodeBusy, err: fmt.Errorf("node %s is locked by another replica", nodeID)}
		return
	}
	defer s.releaseNodeLock(nodeID)

	s.updateTask(taskID, "running")

	ctx, cancel := context.WithTimeout(j.ctx, timeout)
	defer cancel()

	resp, err := sess.Dispatch(ctx, j.req)
	if err != nil {
		code, status := CodeInternal, "error"
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			code, status = CodeTimeout, "timeout"
		case errors.Is(err, registry.ErrNodeGone):
			code, status = CodeNodeOffline, "offline"
		}
		s.finishTask(taskID, status, nil)
		j.resultCh <- result{code: code, err: err}
		return
	}
	// 批E：终态回执随审计落库（GetTask 下钻查因数据源）。剥离内联截图复用录制同款纯函数——
	// 详情面看回执结构与错误码，图不入 tasks 行（体积纪律与 traces 一致）。
	strippedEnv, _ := stripInlineScreenshot(resp.GetJsonEnvelope())
	s.finishTask(taskID, "done", strippedEnv)
	j.resultCh <- result{resp: resp}

	// M6 trace capture 旁路（Locked-3）：仅活跃录制会话（j.traceID 非空）的成功步才录。★保序前提
	// （MAJOR-2）：seq 与 who 在此串行 worker 路径内、goroutine 之外同步捕获——per-node 单 worker 保证
	// execute 串行，同步捕获即保序。nextTraceStep 单次加锁原子取 (seq, who)（GAP-2）：消 nextSeq/traceWho
	// 两次独立加锁间 StopTrace 交织致 seq>0 却 who 空列的 TOCTOU；who 取 lease 登记态（job 无 who 字段，
	// MINOR-a）。异步 recordTraceStep 仅做 I/O（剥离/PutObject/INSERT），置于 resultCh 回填之后，不阻塞
	// 调用方、不扰动 dispatch 延迟。seq==0 表示租约已释放（StopTrace 与在途 dispatch 竞态）→ 跳过不录空步。
	if j.traceID != "" {
		if seq, who := s.nextTraceStep(j.traceID); seq > 0 {
			go s.recordTraceStep(j.traceID, seq, nodeID, j.req.GetTool(), j.req.GetJsonArgs(), resp.GetJsonEnvelope(), who)
		}
	}
}

// acquireNodeLock 在 execute 前获取 per-node 分布式锁：locker 未注入直通；被占（他副本持有）有界
// 短重试（nodeLockRetries×nodeLockRetryDelay）后放弃，调用方回 E_BUSY；Redis 故障 fail-open 放行 +
// Warn——锁是脑裂窗口的串行性兜底而非安全性质，fail-closed 会把 Redis 抖动放大为全节点 dispatch
// 停摆（ha-contract §7#5 读降级原则同源：可用性倒挂不可接受），降级窗口内串行性退回进程内单 worker
// 保证。重试等待期间调用方 ctx 取消即提前放弃（调用方已离开，无谓再争）。
func (s *Scheduler) acquireNodeLock(ctx context.Context, nodeID string, ttl time.Duration) bool {
	if s.locker == nil {
		return true
	}
	for attempt := 0; ; attempt++ {
		ok, err := s.locker.AcquireNodeLock(ctx, nodeID, s.replicaID, ttl)
		if err != nil {
			slog.Warn("node lock acquire failed; failing open", "node_id", nodeID, "err", err)
			return true
		}
		if ok {
			return true
		}
		if attempt >= nodeLockRetries {
			return false
		}
		select {
		case <-time.After(nodeLockRetryDelay):
		case <-ctx.Done():
			return false
		}
	}
}

// releaseNodeLock 执行毕释放 per-node 锁（compare-and-del：仍为本副本持有才删，防误删他副本在本锁
// TTL 过期后新获的锁）。独立短超时 ctx：execute 的请求 ctx 此刻可能已超时/取消（recordTask 独立 ctx
// 同族先例）；失败仅 Warn，TTL 兜底自动过期。
func (s *Scheduler) releaseNodeLock(nodeID string) {
	if s.locker == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.locker.ReleaseNodeLock(ctx, nodeID, s.replicaID); err != nil {
		slog.Warn("node lock release failed; relying on TTL expiry", "node_id", nodeID, "err", err)
	}
}

// recordTask 写入审计初始记录；store 为 nil 时跳过。审计用独立 ctx，不受调用方取消影响。
// 批E：写失败升 Error + 计 aura_store_op_failures_total——「已配置但故障」自此对监控可见，
// 与「未配置」（store==nil 预期降级）区分（C5 故障对称性）。
func (s *Scheduler) recordTask(t store.TaskRecord) {
	if s.store == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.store.CreateTask(ctx, t); err != nil {
		observability.IncStoreOpFailure("record_task")
		slog.Error("record task failed", "task_id", t.ID, "err", err)
	}
}

// updateTask 推进审计状态（running 等非终态）；store 为 nil 时跳过。
func (s *Scheduler) updateTask(taskID, status string) {
	if s.store == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.store.UpdateTaskStatus(ctx, taskID, status); err != nil {
		observability.IncStoreOpFailure("update_task")
		slog.Error("update task failed", "task_id", taskID, "status", status, "err", err)
	}
}

// finishTask 落任务终态（批E）：status + finished_at + 终态回执 envelope（已剥离内联截图；错误终态
// 传 nil）。store 为 nil 时跳过；独立 ctx 同 recordTask 纪律。
func (s *Scheduler) finishTask(taskID, status string, envelope []byte) {
	if s.store == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.store.FinishTask(ctx, taskID, status, envelope); err != nil {
		observability.IncStoreOpFailure("finish_task")
		slog.Error("finish task failed", "task_id", taskID, "status", status, "err", err)
	}
}

// —— M6 录制会话独占租约（Locked-2）——————————————————————————————————————————————
// StartTrace 即租、StopTrace/TTL 释放、checkLease 门控 dispatch、nextTraceStep 供 capture 编号。
// T4（ha-contract §3）起租约状态经 s.leases（LeaseStore）读写：进程内实现保 single-copy 自愈
// 语义（进程死即租约消失），Redis 实现供多副本共享。ctx 策略：checkLease 用 dispatch 请求 ctx；
// StartTrace/StopTrace/nextTraceStep 用独立 Background+短超时（复刻 recordTask 审计独立 ctx
// 先例——nextTraceStep 在 execute 的 resultCh 回填之后执行，j.ctx 此刻可能已取消，复用必致
// 伪失败；StartTrace/StopTrace 对外签名保持无 ctx，rest.go 调用点零动）。

// errTraceBusy 是非持有者 dispatch 被租约拒绝的哨兵错误；Dispatch 据此合成 CodeBusy(=E_BUSY) 返回，
// StartTrace 二次租同 node 时亦返此（rest.go 合成 E_BUSY 语义码）。复用枚举零新码（Locked-2）。
var errTraceBusy = errors.New("node is being traced by another session (E_BUSY)")

// StartTrace 为 nodeID 建立录制会话独占租约并返回新 trace_id。node 已被另一活跃 trace 租用 →
// errTraceBusy，保证 per-node 独占；既有租约已过期则由存储侧清理、允许新租约覆盖。存储故障
//（Redis）如实上抛（写路径错误可见，ha-contract §7#5；管理面低频调用，调用方可重试）。
func (s *Scheduler) StartTrace(nodeID, who string) (string, error) {
	traceID := uuid.NewString()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.leases.Acquire(ctx, nodeID, traceID, who, s.leaseTTL); err != nil {
		if errors.Is(err, store.ErrLeaseHeld) {
			return "", errTraceBusy
		}
		return "", err
	}
	return traceID, nil
}

// StopTrace 显式释放 traceID 对应的 per-node 租约（幂等：未知 trace_id no-op）。
// 返回是否真实释放了一条活跃租约：true=本次调用释放了租约；false=trace_id 不存在/已释放。
// 该真值即 StopTraceResponse.stopped 的语义源（proto:205「trace_id 不存在/已释放则 false」），
// 供 rest.go 忠实转发真实释放状态而非硬编码 true（TASK-006 milestone-audit 观察项修正）。
// 存储故障返回 false 并 Warn（签名无错误位；stopped=false 对调用方可见、可重试）。
func (s *Scheduler) StopTrace(traceID string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	released, err := s.leases.Release(ctx, traceID)
	if err != nil {
		slog.Warn("stop trace: lease release failed", "trace_id", traceID, "err", err)
		return false
	}
	return released
}

// checkLease 门控一次对 nodeID 的 dispatch：
//   - node 无活跃租约 → nil 放行（空 trace_id 的常规 dispatch 不受影响）；
//   - node 有租约且 traceID 匹配持有者 → nil 放行；
//   - node 有租约且 traceID 不匹配（含空 traceID）→ errTraceBusy（Dispatch 合成 E_BUSY）。
//
// TTL 过期由存储侧承载（进程内读时惰性释放 / Redis 原生 PEXPIRE），过期即视为无租约放行。
// 读路径 fail-open（ha-contract §7#5）：存储读故障 → Warn 后放行——录制独占是录制保真性质而
// 非安全性质，checkLease 在每次 dispatch 热路径上，fail-closed 会把 Redis 抖动放大为全局
// dispatch 停摆（可用性倒挂）。降级窗口内非持有者 dispatch 可能混入录制，Warn 留痕，接受。
func (s *Scheduler) checkLease(ctx context.Context, nodeID, traceID string) error {
	lease, ok, err := s.leases.Get(ctx, nodeID)
	if err != nil {
		slog.Warn("lease check failed; failing open", "node_id", nodeID, "err", err)
		return nil
	}
	if !ok {
		return nil // node 无租约（或已过期）：放行
	}
	if traceID == lease.TraceID {
		return nil // 持有者放行
	}
	return errTraceBusy // 非持有者（含空 traceID）：E_BUSY
}

// nextTraceStep 原子取「下一步序号（++）+ 租约登记的 who」（GAP-2：单临界区/单 Lua 同出一份
// 存活租约快照），供 execute capture 旁路同步捕获用：要么得 (seq>0, who)，要么得 (0, "")（租约
// 不存在/已释放）——消除 StopTrace 交织下「seq>0 却 who 空列」的中间态，旁路 seq>0 守卫据此
// 跳过不录空步。存储故障同样返 (0, "") + Warn（该步不录、不阻塞 dispatch——recordTraceStep
// 同族 best-effort）。独立 ctx：本方法在 resultCh 回填之后执行，j.ctx 可能已取消。
func (s *Scheduler) nextTraceStep(traceID string) (int64, string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	seq, who, err := s.leases.NextStep(ctx, traceID)
	if err != nil {
		slog.Warn("trace capture: next step failed; skipping step", "trace_id", traceID, "err", err)
		return 0, ""
	}
	return seq, who
}

// —— M6 录制步捕获数据面（Locked-3,4 / MAJOR-2,3）——————————————————————————————————
// execute 旁路同步捕获 seq/who 后经 `go s.recordTraceStep(...)` 异步落库，逐步截图剥离卸 MinIO，
// GetTrace 分页供回放读取。

// recordTraceStep 持久化一条录制步（由 execute 旁路 `go` 异步调用；seq/who 已在 execute 串行路径同步
// 捕获后传入）。复刻 recordTask 模式：独立 context.Background()+超时、best-effort slog.Warn、store==nil
// no-op（nil-safe 不扰动 dispatch）。逐步截图剥离卸桶 + traces INSERT（ON CONFLICT 幂等）。
func (s *Scheduler) recordTraceStep(traceID string, seq int64, nodeID, tool string, jsonArgs, envelope []byte, who string) {
	if s.store == nil {
		return // 未配置 PG：capture no-op（与 recordTask 一致）
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stripped, key := s.offloadScreenshot(ctx, traceID, seq, envelope)
	if err := s.store.CreateTraceStep(ctx, store.TraceStep{TraceID: traceID, Seq: seq, NodeID: nodeID, Tool: tool, JsonArgs: jsonArgs, JsonEnvelope: stripped, ScreenshotKey: key, Who: who}); err != nil {
		observability.IncStoreOpFailure("trace_step")
		slog.Error("trace capture: create trace step failed", "trace_id", traceID, "seq", seq, "err", err)
	}
}

// offloadScreenshot 从 envelope 剥离内联 base64 截图（MAJOR-3 必修）：解出图像经 PutObject 卸桶
// trace/<trace_id>/<seq>.webp（内部 client），返回剥离后 envelope + screenshot_key。无内联截图 /
// 未配置 MinIO / 卸桶失败 → 返回原 envelope + 空 key（best-effort：不丢图像信息、不阻断 INSERT）。
func (s *Scheduler) offloadScreenshot(ctx context.Context, traceID string, seq int64, envelope []byte) ([]byte, string) {
	stripped, image := stripInlineScreenshot(envelope)
	if image == nil {
		return envelope, "" // 非截图步 / 无内联图像：envelope 原样
	}
	if s.storage == nil {
		// 未配置 MinIO：保留原 envelope（内联图像不丢失），warn 一次。
		slog.Warn("trace capture: MinIO unconfigured; keeping inline screenshot in envelope", "trace_id", traceID, "seq", seq)
		return envelope, ""
	}
	key := fmt.Sprintf("trace/%s/%d.webp", traceID, seq)
	if err := s.storage.PutObject(ctx, key, image, "image/webp"); err != nil {
		slog.Warn("trace capture: screenshot upload failed; keeping full envelope", "trace_id", traceID, "seq", seq, "err", err)
		return envelope, ""
	}
	return stripped, key
}

// stripInlineScreenshot 从节点回执 envelope 剥离 data.image_base64 内联截图，返回剥离后的 envelope 与
// 解出的原始图像字节（供卸桶）。仅当 envelope 为 JSON 对象且 data.image_base64 为非空可解码 base64 串
// 时剥离——screenshot/zoom 步命中，其余（a11y/文本/错误信封等无内联图像）原样返回 + nil 图像。用
// map[string]json.RawMessage 保留同级字段（ok/error、data.mime/data.meta）原字节，仅重排 key 顺序并剔除
// image_base64；任何解析/解码失败一律降级为「不剥离」（原 envelope + nil），best-effort 不吞信息。
func stripInlineScreenshot(envelope []byte) ([]byte, []byte) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(envelope, &top); err != nil {
		return envelope, nil // 非 JSON 对象信封：原样
	}
	dataRaw, ok := top["data"]
	if !ok {
		return envelope, nil
	}
	var data map[string]json.RawMessage
	if err := json.Unmarshal(dataRaw, &data); err != nil {
		return envelope, nil // data 非对象（如 null/数组）：原样
	}
	b64Raw, ok := data["image_base64"]
	if !ok {
		return envelope, nil // 无内联截图字段：原样
	}
	var b64 string
	if err := json.Unmarshal(b64Raw, &b64); err != nil || b64 == "" {
		return envelope, nil // 非字符串 / 空串：原样
	}
	image, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return envelope, nil // 解码失败：不剥离、保留原信封
	}
	delete(data, "image_base64")
	newData, err := json.Marshal(data)
	if err != nil {
		return envelope, nil
	}
	top["data"] = newData
	stripped, err := json.Marshal(top)
	if err != nil {
		return envelope, nil
	}
	return stripped, image
}

// GetTrace 分页读取 traceID 的录制步序（回放读路径，供 rest.go GetTrace handler → TASK-006 replay）。
// 薄委托 store：seq 游标分页（seq>seqCursor 前 pageSize 步，升序）+ 录制源 node_id/platform。store 未
// 配置（纯内存）时返回空——无持久化 trace 可读。
func (s *Scheduler) GetTrace(ctx context.Context, traceID string, pageSize, seqCursor int64) ([]store.TraceStep, string, string, error) {
	if s.store == nil {
		return nil, "", "", nil
	}
	return s.store.GetTrace(ctx, traceID, pageSize, seqCursor)
}
