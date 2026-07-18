// Package observability 汇聚控制面的可观测性装配：Prometheus 指标与 OpenTelemetry 追踪。
//
// 指标经 promauto 注册到默认 Registry，由 main.go 挂载的 promhttp.Handler() 暴露在 /metrics。
// 七族指标（低基数 label）：
//   - aura_dispatch_duration_seconds{tool}            工具调用端到端耗时（histogram，gateway 观测）
//   - aura_dispatch_errors_total{code}                传输层错误码计数（counter，gateway 观测）
//   - aura_scheduler_queue_depth{node_id}             per-node 队列深度（gauge，scheduler 观测）
//   - aura_registry_sessions                          在线反连会话数（GaugeFunc 采样 registry）
//   - aura_provision_duration_seconds{provider,kind}  环境置备耗时（histogram，TASK-006 埋点）
//   - aura_node_up{node_id,platform}                  节点在线态（gauge，批E：告警基础——WatchStatus tick 维护）
//   - aura_store_op_failures_total{op}                状态存储读写失败计数（counter，批E：区分「未配置」与「已配置但故障」）
//
// label 基数守低：仅 tool/code/node_id/provider/kind/op，绝不把 task_id 等高基数值入 label。
package observability

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	// dispatchDuration 记录一次工具调用在控制面的端到端耗时（成功与失败均记）。
	dispatchDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "aura_dispatch_duration_seconds",
		Help:    "Controller-side end-to-end tool dispatch latency in seconds.",
		Buckets: prometheus.DefBuckets,
	}, []string{"tool"})

	// dispatchErrors 按传输层错误码计数（E_BUSY/E_NODE_OFFLINE/E_TIMEOUT/E_INTERNAL）。
	dispatchErrors = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "aura_dispatch_errors_total",
		Help: "Total tool dispatches that failed with a transport-layer error code.",
	}, []string{"code"})

	// queueDepth 反映每节点串行队列的当前深度（含排队与在执行）。
	queueDepth = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "aura_scheduler_queue_depth",
		Help: "Current per-node scheduler queue depth.",
	}, []string{"node_id"})

	// provisionDuration 记录环境置备耗时；本任务注册 collector，TASK-006 在 provider 内埋点调用。
	provisionDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "aura_provision_duration_seconds",
		Help:    "Environment provisioning duration in seconds.",
		Buckets: []float64{1, 5, 10, 30, 60, 120, 300},
	}, []string{"provider", "kind"})

	// nodeUp 是节点在线态（批E：1=online，0=unhealthy/offline）。registry WatchStatus tick（10s）全量
	// 刷新，节点会话移除时删除序列（防 churn 泄漏，同 queueDepth 纪律）。设备掉线告警的指标基础
	// （alertmanager 规则：aura_node_up == 0 for 1m）。
	nodeUp = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "aura_node_up",
		Help: "Node liveness by reverse-connection health (1=online, 0=unhealthy or offline).",
	}, []string{"node_id", "platform"})

	// storeOpFailures 按操作名计数状态存储读写失败（批E：审计写失败此前仅 slog.Warn，调用方与监控
	// 双双无感——「已配置但故障」自此可告警，与「未配置」的预期降级区分开）。
	storeOpFailures = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "aura_store_op_failures_total",
		Help: "Total failed operations against the configured state store (audit/trace/meta writes and reads).",
	}, []string{"op"})

	// authFailures 按入口面计数认证失败（批E D2 纵深防御：bearer 401/tango 401/enroll 429——
	// 暴力枚举/令牌误配自此可从指标面告警，替代翻日志）。
	authFailures = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "aura_auth_failures_total",
		Help: "Total rejected authentication attempts by surface (bearer/tango) and rate-limited requests (enroll).",
	}, []string{"surface"})

	// agentCalls 按 JSON-RPC 方法计数直连 MCP agent 调用（M13：外部 agent 直连节点 /mcp 的活动此前
	// 指标面不可见）。method 由调用方归一到有界集（initialize/tools/call/tools/list/notification/other）
	// 防标签基数爆炸——客户端可发任意 method 串（同全文件低基数纪律：绝不把 peer/tool 等入 label）。
	agentCalls = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "aura_agent_calls_total",
		Help: "Total direct-MCP agent calls observed at nodes, by JSON-RPC method (M13).",
	}, []string{"method"})
)

// ObserveDispatch 记录一次工具调用耗时（gateway.Dispatch 调用，无论成功或传输层错误）。
func ObserveDispatch(tool string, seconds float64) {
	dispatchDuration.WithLabelValues(tool).Observe(seconds)
}

// IncDispatchError 累加一次传输层错误码计数（gateway.Dispatch 在 code!="" 时调用）。
func IncDispatchError(code string) {
	dispatchErrors.WithLabelValues(code).Inc()
}

// SetQueueDepth 更新某节点当前队列深度（scheduler 入队/出队后调用）。
func SetQueueDepth(nodeID string, depth int) {
	queueDepth.WithLabelValues(nodeID).Set(float64(depth))
}

// DeleteQueueDepth 移除某节点的队列深度序列（scheduler 回收 per-node 队列后调用）。
// 节点会话掉线时清除对应 aura_scheduler_queue_depth{node_id} 子序列，避免 gauge 序列随节点 churn 无界泄漏。
func DeleteQueueDepth(nodeID string) {
	queueDepth.DeleteLabelValues(nodeID)
}

// ObserveProvision 记录一次环境置备耗时（TASK-006 provider 埋点调用）。
func ObserveProvision(provider, kind string, seconds float64) {
	provisionDuration.WithLabelValues(provider, kind).Observe(seconds)
}

// SetNodeUp 更新节点在线态 gauge（registry WatchStatus tick 调用；online=true 记 1，其余记 0）。
func SetNodeUp(nodeID, platform string, up bool) {
	v := 0.0
	if up {
		v = 1.0
	}
	nodeUp.WithLabelValues(nodeID, platform).Set(v)
}

// DeleteNodeUp 移除节点在线态序列（节点会话移除/身份删除时调用，防 gauge 序列随节点 churn 泄漏）。
// platform 维度用 DeletePartialMatch 兜底（调用点未必都持有 platform）。
func DeleteNodeUp(nodeID string) {
	nodeUp.DeletePartialMatch(prometheus.Labels{"node_id": nodeID})
}

// IncStoreOpFailure 累加一次状态存储操作失败（op 为低基数操作名：record_task/finish_task/
// trace_step/recording_meta 等）。
func IncStoreOpFailure(op string) {
	storeOpFailures.WithLabelValues(op).Inc()
}

// IncAuthFailure 累加一次认证失败/限流拒绝（surface 为低基数入口名：bearer/tango/enroll-ratelimit）。
func IncAuthFailure(surface string) {
	authFailures.WithLabelValues(surface).Inc()
}

// IncAgentCall 累加一次直连 MCP agent 调用（M13）。method 须由调用方归一到有界集（见 agentMethodLabel）。
func IncAgentCall(method string) {
	agentCalls.WithLabelValues(method).Inc()
}

// RegisterSessionCount 以 GaugeFunc 采样在线会话数（免改 registry：传入取数闭包，
// 每次 /metrics 抓取时调用 sample 反映当下会话数）。应在启动装配阶段调用一次。
func RegisterSessionCount(sample func() int) {
	promauto.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "aura_registry_sessions",
		Help: "Number of live reverse-connected node sessions.",
	}, func() float64 { return float64(sample()) })
}

// Prewarm 预热 labeled 指标子序列，使零流量时 /metrics 即含对应族名：
//   - 传输错误码：对已知 code 做 Add(0)（实例化 count=0 子序列，非伪造样本）；
//   - 置备耗时：对已知 provider×kind 组合实例化 count=0 子序列（仅建子序列，不注入伪观测）。
func Prewarm(dispatchErrorCodes []string) {
	for _, code := range dispatchErrorCodes {
		dispatchErrors.WithLabelValues(code).Add(0)
	}
	// 已知置备维度：M2=pve、M3=k8s；kind=ephemeral/persistent。仅实例化，计数为 0。
	for _, provider := range []string{"pve", "k8s"} {
		for _, kind := range []string{"ephemeral", "persistent"} {
			provisionDuration.WithLabelValues(provider, kind)
		}
	}
}
