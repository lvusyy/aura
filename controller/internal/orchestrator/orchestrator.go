// Package orchestrator 承载并行编排（fan-out/gather）：一条用例分发至 N 个目标节点，join 聚合每环境
// pass/fail + 动作延迟。组件与 gateway 平级，位于 scheduler 之上——并发调 DispatchTracked 复用 per-node
// 串行队列，零队列改动（SC-6：设备无法并行触摸，同 node 多任务仍由其队列串行化）。
//
// join 聚合语义（analyze M8-08 §3 track A）：全 pass=done；部分 fail=partial（不熔断其余，失败环境结果
// 照留）；dispatch 超时=该环境记 timeout 计入 failed 桶。编排落新 orchestrations 表（对抗 C，additive），
// fan-out 出的各 per-node 任务经 orchestration_id 回填与编排关联（执行墙钻取）。OTel：fan-out 顶 span →
// 各 node dispatch 子 span（既有 StartDispatchSpan）→ gather span，同一 trace_id 贯通全链。
package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	aurav1 "github.com/aura/controller/gen/aura/v1"
	"github.com/aura/controller/internal/observability"
	"github.com/aura/controller/internal/registry"
	"github.com/aura/controller/internal/scheduler"
	"github.com/aura/controller/internal/store"
)

// dispatcher 是编排层所需的 scheduler 最小下发面（*scheduler.Scheduler 实现）。窄接口令单测注入 fake
// 验并发 fan-out 与 partial/timeout 聚合，不依赖真 registry/反连流（第三方隔离规约同源：依赖倒置便测）。
type dispatcher interface {
	DispatchTracked(ctx context.Context, nodeID, tool string, jsonArgs []byte, deadlineMs int64, who, traceID string) (*aurav1.ToolResponse, string, string, error)
}

// nodeLister 是 EnvGroup 平台过滤所需的 registry 最小只读面（*registry.NodeRegistry 实现）。
type nodeLister interface {
	List() []*aurav1.NodeInfo
}

// taskStore 是编排落表与 orchestration_id 关联所需的 store 最小写面（*store.PGStore 实现）。可为 nil
// （纯内存运行时编排不落表；构造期 typed-nil 守卫保证接口本身为 nil，见 NewOrchestrator）。
type taskStore interface {
	CreateOrchestration(ctx context.Context, o store.OrchestrationRecord) error
	UpdateOrchestrationResult(ctx context.Context, id, status string, passed, failed int32) error
	SetTaskOrchestration(ctx context.Context, orchestrationID string, taskIDs []string) error
}

const (
	// —— 编排终态（落 orchestrations.status；running 为 CreateOrchestration 初态）——
	StatusRunning = "running"
	StatusDone    = "done"    // 全 pass
	StatusPartial = "partial" // 部分 fail（不熔断其余）
	StatusFailed  = "failed"  // 全 fail（含全 timeout）

	// —— 单目标结果状态（EnvResult.Status；对齐 proto EnvResult.status 词表）——
	EnvSucceeded = "succeeded"
	EnvFailed    = "failed"
	EnvTimeout   = "timeout"

	// maxFanout 是单次编排目标数硬上限（有界并发防御：目标数即并发 goroutine 数，防超大 env-group 撑爆）。
	maxFanout = 256

	// auditWriteTimeout 是编排落表写入的超时（独立 ctx，与请求生命周期解耦，见 auditContext）。
	auditWriteTimeout = 10 * time.Second
)

var (
	// ErrNoTargets：NodeIDs 与 EnvGroup 均空，或 EnvGroup 解析后无在线匹配节点（handler 折射为 InvalidArgument）。
	ErrNoTargets = errors.New("orchestration has no target nodes (node_ids and env_group both empty or unresolved)")
	// ErrTooManyTargets：目标数超过 maxFanout 上限（防御性拒绝）。
	ErrTooManyTargets = errors.New("orchestration target count exceeds fan-out limit")
)

// OrchestrationSpec 一次并行编排的输入：一条用例（tool+args）fan-out 至目标集（NodeIDs 直指 or EnvGroup
// 平台过滤，二选一），各目标以 DeadlineMs 为相对超时并发下发。
type OrchestrationSpec struct {
	Tool       string
	JsonArgs   []byte
	NodeIDs    []string // 目标节点直指（与 EnvGroup 二选一，优先）
	EnvGroup   string   // 环境组名（解析为 N 个在线同 platform 节点；与 NodeIDs 二选一）
	DeadlineMs int64    // 各目标相对超时（透传 scheduler.Dispatch）
	Who        string   // 审计：发起方标识
}

// EnvResult 单目标（一个环境）的执行结果，是 join 聚合的一格。
type EnvResult struct {
	NodeID       string
	TaskID       string // 该目标 dispatch 的审计任务 id（下发在建审计行前被拒时为空）
	Status       string // EnvSucceeded | EnvFailed | EnvTimeout
	Code         string // 传输层错误码（成功为空；E_TIMEOUT/E_BUSY/E_NODE_OFFLINE/E_INTERNAL）
	JsonEnvelope []byte // 节点回执 Envelope JSON（成功时原样回传，失败为空）
	LatencyMs    int64  // 该目标动作延迟（毫秒）
}

// Result 一次并行编排的 join 聚合结果。
type Result struct {
	OrchestrationID string
	Status          string // StatusDone | StatusPartial | StatusFailed
	Total           int32
	Passed          int32
	Failed          int32 // 含 timeout 桶
	PerEnv          []EnvResult
	TraceID         string // OTel 关联（未启用追踪为空）
}

// Orchestrator 并行编排组件。sched 承接 per-node 串行 DispatchTracked（并发调用天然保持串行语义），
// reg 供 EnvGroup 平台过滤，store 落 orchestrations 表与 tasks.orchestration_id 关联（可为 nil，纯内存）。
type Orchestrator struct {
	reg   nodeLister
	sched dispatcher
	store taskStore // 可为 nil（纯内存，编排不落表）
}

// NewOrchestrator 构造编排组件。reg/sched 为装配期必备（下发单点 + 目标解析）；pg 可为 nil（纯内存，与
// main.go typed-nil 纪律一致）——仅在非 nil 时赋给接口字段，避免「具体 nil 指针赋接口后接口非 nil」陷阱
// 致 store==nil 判断失效（M2 TASK-004 教训）。
func NewOrchestrator(reg *registry.NodeRegistry, sched *scheduler.Scheduler, pg *store.PGStore) *Orchestrator {
	o := &Orchestrator{reg: reg, sched: sched}
	if pg != nil {
		o.store = pg
	}
	return o
}

// Run 执行一次并行编排：解析目标 → 落 running → fan-out 并发下发 → gather join 聚合 → 落终态 + 回填
// orchestration_id。返回 join 聚合结果；仅目标解析失败（空目标/超限）返 error，单目标下发失败不熔断、
// 计入 failed 桶（partial 语义）。store 写入全程 best-effort（失败仅告警不阻断——节点动作已发生，审计
// 不可回退动作事实）。
func (o *Orchestrator) Run(ctx context.Context, spec OrchestrationSpec) (Result, error) {
	targets, err := o.resolveTargets(spec)
	if err != nil {
		return Result{}, err
	}

	// fan-out 顶 span：各目标 dispatch 子 span 与 gather span 均以本 span 上下文为父，trace_id 贯通全链。
	ctx, span := observability.StartOrchestrationSpan(ctx, spec.Tool, len(targets))
	defer span.End()
	traceID := observability.TraceIDFromContext(ctx)

	orchID := uuid.NewString()
	o.persistStart(orchID, traceID, spec, int32(len(targets)))

	// fan-out：每目标一 goroutine 并发下发；各 goroutine 独占 results[i]（下标私有，wg.Wait 后统一读，
	// 无共享写竞态）。per-node 串行由 scheduler 队列天然保持——编排层只并发调 DispatchTracked，目标为
	// 不同 node（NodeIDs 已去重），同 node 多任务仍由其队列串行化（SC-6 不变）。
	results := make([]EnvResult, len(targets))
	var wg sync.WaitGroup
	for i, node := range targets {
		wg.Add(1)
		go func(i int, node string) {
			defer wg.Done()
			start := time.Now()
			resp, taskID, code, _ := o.sched.DispatchTracked(ctx, node, spec.Tool, spec.JsonArgs, spec.DeadlineMs, spec.Who, "")
			results[i] = classifyOutcome(node, taskID, code, resp, time.Since(start))
		}(i, node)
	}
	wg.Wait()

	// gather：join 聚合每环境 pass/fail → 编排终态（done/partial/failed，timeout 计入 failed 桶）。
	_, gspan := observability.StartGatherSpan(ctx, len(targets))
	res := aggregate(orchID, traceID, results)
	gspan.End()

	o.persistResult(res)

	slog.Info("orchestration completed",
		"orchestration_id", orchID, "trace_id", traceID, "tool", spec.Tool,
		"status", res.Status, "total", res.Total, "passed", res.Passed, "failed", res.Failed)
	return res, nil
}

// resolveTargets 解析目标节点集：NodeIDs 直指优先（去重保序），否则 EnvGroup 平台过滤；二者皆空或解析
// 空集 → ErrNoTargets；超上限 → ErrTooManyTargets。
func (o *Orchestrator) resolveTargets(spec OrchestrationSpec) ([]string, error) {
	var targets []string
	switch {
	case len(spec.NodeIDs) > 0:
		targets = dedupe(spec.NodeIDs)
	case spec.EnvGroup != "":
		targets = o.resolveEnvGroup(spec.EnvGroup)
	}
	if len(targets) == 0 {
		return nil, ErrNoTargets
	}
	if len(targets) > maxFanout {
		return nil, ErrTooManyTargets
	}
	return targets, nil
}

// resolveEnvGroup 把环境组名解析为在线目标节点集：本 phase 取「platform 过滤」基础语义——env_group 匹配
// 节点 platform（大小写不敏感）且状态 online 者入选。完整 session-set/label 语义后续增强（YAGNI；NodeInfo
// 现无 label 维度，不臆造）。
func (o *Orchestrator) resolveEnvGroup(group string) []string {
	if o.reg == nil {
		return nil
	}
	var out []string
	for _, n := range o.reg.List() {
		if n.GetStatus() == "online" && strings.EqualFold(n.GetPlatform(), group) {
			out = append(out, n.GetNodeId())
		}
	}
	return out
}

// persistStart 落 running 初态编排行（best-effort，独立 ctx；store 为 nil 时 no-op）。traceID 是 Run 从
// fan-out 顶 span 捕获的 OTel trace id（TraceIDFromContext，未启用追踪为空串）——随行持久化
// （orchestrations.trace_id，M10 T9），console Summary 由此回填与追踪后端关联。
func (o *Orchestrator) persistStart(orchID, traceID string, spec OrchestrationSpec, total int32) {
	if o.store == nil {
		return
	}
	ctx, cancel := auditContext()
	defer cancel()
	rec := store.OrchestrationRecord{
		ID:       orchID,
		Tool:     spec.Tool,
		EnvGroup: spec.EnvGroup,
		Status:   StatusRunning,
		Total:    total,
		Who:      spec.Who,
		TraceID:  traceID,
	}
	if err := o.store.CreateOrchestration(ctx, rec); err != nil {
		slog.Warn("orchestration: create record failed", "orchestration_id", orchID, "err", err)
	}
}

// persistResult 落编排终态汇总 + 回填参与任务的 orchestration_id（best-effort，独立 ctx；store 为 nil 时
// no-op）。两写共用一个独立 ctx（与请求生命周期解耦，客户端取消不连带丢审计——审计写入用独立 context，
// M2 教训）。
func (o *Orchestrator) persistResult(res Result) {
	if o.store == nil {
		return
	}
	ctx, cancel := auditContext()
	defer cancel()
	if err := o.store.UpdateOrchestrationResult(ctx, res.OrchestrationID, res.Status, res.Passed, res.Failed); err != nil {
		slog.Warn("orchestration: update result failed", "orchestration_id", res.OrchestrationID, "err", err)
	}
	if err := o.store.SetTaskOrchestration(ctx, res.OrchestrationID, participatingTaskIDs(res.PerEnv)); err != nil {
		slog.Warn("orchestration: associate tasks failed", "orchestration_id", res.OrchestrationID, "err", err)
	}
}

// classifyOutcome 把单次 DispatchTracked 回执归类为 EnvResult（succeeded/failed/timeout）。纯函数，便于
// 离线单测覆盖桶归类。E_TIMEOUT → timeout（计入 failed 桶），其余非空 code → failed，空 code → succeeded。
func classifyOutcome(nodeID, taskID, code string, resp *aurav1.ToolResponse, latency time.Duration) EnvResult {
	r := EnvResult{
		NodeID:    nodeID,
		TaskID:    taskID,
		Code:      code,
		LatencyMs: latency.Milliseconds(),
	}
	switch code {
	case "":
		r.Status = EnvSucceeded
		if resp != nil {
			r.JsonEnvelope = resp.GetJsonEnvelope()
			// 传输层成功（空 code）后再探业务信封：显式 ok:false 表示工具在该环境实际执行失败
			// （如 mac 无显示器 "display index out of range"、win 锁屏 input failed），应计 failed 桶——
			// 「passed=用例在该环境真正通过」与用户直觉一致，否则传输送达即误计 passed。信封缺失/
			// 非标准回执保持 succeeded（保守，不误伤历史工具）。envelope 的 error.code 覆写空传输码，
			// 令 PerEnv 行携带可读失败码。
			if envCode, failed := toolEnvelopeFailure(r.JsonEnvelope); failed {
				r.Status = EnvFailed
				r.Code = envCode
			}
		}
	case scheduler.CodeTimeout:
		r.Status = EnvTimeout
	default:
		r.Status = EnvFailed
	}
	return r
}

// toolEnvelopeFailure 解析节点回执 Envelope JSON 的 ok 字段：显式 ok:false 时返回其 error.code
// （缺失则 "E_TOOL_FAILED"）与 true，表示工具在目标环境执行失败；否则返回 ("", false)。
// 保守语义：信封为空 / 非法 JSON / 无 ok 字段一律判非失败——仅显式 `"ok":false` 才翻转，避免误伤
// 传输成功但信封非标准形状的历史回执。纯函数，便于离线单测。
func toolEnvelopeFailure(env []byte) (string, bool) {
	if len(env) == 0 {
		return "", false
	}
	var probe struct {
		Ok    *bool `json:"ok"`
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(env, &probe); err != nil {
		return "", false
	}
	if probe.Ok != nil && !*probe.Ok {
		if probe.Error.Code != "" {
			return probe.Error.Code, true
		}
		return "E_TOOL_FAILED", true
	}
	return "", false
}

// aggregate 把各目标 EnvResult 聚合为编排终态：succeeded 计 passed，failed/timeout 同计 failed 桶
// （超时入 fail 桶）；状态由 rollupStatus 判定。纯函数，便于离线单测覆盖 partial/timeout 语义。
func aggregate(orchID, traceID string, results []EnvResult) Result {
	var passed, failed int32
	for _, r := range results {
		if r.Status == EnvSucceeded {
			passed++
		} else {
			failed++
		}
	}
	return Result{
		OrchestrationID: orchID,
		Status:          rollupStatus(passed, failed),
		Total:           int32(len(results)),
		Passed:          passed,
		Failed:          failed,
		PerEnv:          results,
		TraceID:         traceID,
	}
}

// rollupStatus 依 passed/failed 桶判编排终态：全 pass=done，全 fail=failed，混合=partial（部分失败不
// 熔断）。目标集非空由调用方保证（resolveTargets 空集提前 ErrNoTargets）。
func rollupStatus(passed, failed int32) string {
	switch {
	case failed == 0:
		return StatusDone
	case passed == 0:
		return StatusFailed
	default:
		return StatusPartial
	}
}

// participatingTaskIDs 摘出参与编排的非空任务 id（下发在建审计行前被拒的目标无 task_id，跳过——无行可关联）。
func participatingTaskIDs(results []EnvResult) []string {
	ids := make([]string, 0, len(results))
	for _, r := range results {
		if r.TaskID != "" {
			ids = append(ids, r.TaskID)
		}
	}
	return ids
}

// dedupe 去重并保序（NodeIDs 直指可能含重复/空；同 node 去重免 per-node 队列自撞 E_BUSY）。
func dedupe(ids []string) []string {
	seen := make(map[string]struct{}, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

// auditContext 返回与请求生命周期解耦的独立审计写入 ctx（客户端取消/超时不连带丢编排落表，M2 教训）。
func auditContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), auditWriteTimeout)
}
