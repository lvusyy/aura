package transport

import (
	"context"
	"errors"
	"fmt"

	"connectrpc.com/connect"

	aurav1 "github.com/aura/controller/gen/aura/v1"
	"github.com/aura/controller/internal/store"
)

// 本文件 4 个只读查询 handler（TASK-007 真实现替 TASK-003 stub）：GetDashboard 聚合 reg+store，
// ListTasks/ListTraces 分页委托 store（page_size 归一沿 rest.go normalizeTracePageSize，page_token
// 为 store 侧不透明键集游标直传），GetQueueDepth 委托 sched.QueueDepth（对抗 G：console 轮询前查 >0
// 即跳帧）。依赖判空沿 main.go typed-nil 纪律：store/sched/reg 可选（纯内存运行时降级为空/零值）。
//
// 聚合/映射逻辑抽为下方纯函数（countNodes/bucketTaskStatusCounts/taskRowToSummary/traceRowToSummary）：
// reg/store/sched 均为具体类型无法 mock，纯函数是免 PG 的单测缝（console_query_test.go 直测分类与映射）。

// GetDashboard 首屏聚合摘要：节点分状态计数（reg.List() 活跃会话快照）+ 任务全表 COUNT（总数 + GROUP BY
// status 分健康桶）+ 编排/录制全表 COUNT。store 为 nil（纯内存）时仅回节点计数，其余为 0。计数走真全表
// COUNT（AUD-4），非旧 500 窗口下界——表规模超窗口后窗口读失真，COUNT 恒精确。
func (s *ConsoleServiceServer) GetDashboard(
	ctx context.Context,
	_ *connect.Request[aurav1.GetDashboardRequest],
) (*connect.Response[aurav1.GetDashboardResponse], error) {
	resp := &aurav1.GetDashboardResponse{}

	// 节点计数：ListFleet 全集（在线会话 + nodes 表 offline 持久身份合并）——与 WatchFleet 节点墙同源，消除
	// 三口径不一致（PROBE 2：旧版摘要仅计在线会话 reg.List()=4 vs 墙面 ListFleet=34 自相矛盾）。offline=无活跃
	// 会话的持久身份，nodes_offline 由此真实计数（旧版恒留 0）。离线僵尸经 DeleteNode 手动删 + reap 自动遗忘
	// 出集，摘要随墙面同步收敛。store 为 nil（纯内存）时 ListFleet 降级为在线-only（同 reg.List() 语义，offline 恒 0）。
	if s.reg != nil {
		resp.NodesTotal, resp.NodesOnline, resp.NodesUnhealthy, resp.NodesOffline = countNodes(s.reg.ListFleet(ctx))
	}

	// 任务/编排/录制计数：真全表 COUNT。任一 store 查询失败即整体 CodeInternal（同一 PG 池，单查失败多因
	// 连接/查询故障，快速失败而非呈半份误导数据）。
	if s.store != nil {
		total, err := s.store.CountTasks(ctx)
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("dashboard tasks count: %w", err))
		}
		resp.TasksTotal = total

		byStatus, err := s.store.CountTasksByStatus(ctx)
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("dashboard tasks status count: %w", err))
		}
		resp.TasksRunning, resp.TasksSucceeded, resp.TasksFailed = bucketTaskStatusCounts(byStatus)

		orchTotal, err := s.store.CountOrchestrations(ctx)
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("dashboard orchestrations count: %w", err))
		}
		resp.OrchestrationsTotal = orchTotal

		tracesTotal, err := s.store.CountTraces(ctx)
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("dashboard traces count: %w", err))
		}
		resp.TracesTotal = tracesTotal
	}

	return connect.NewResponse(resp), nil
}

// ListTasks 任务历史分页读（执行墙/任务列表数据源）。委托 store.ListTasks：page_size 归一沿 GetTrace
// 先例（normalizeTracePageSize，0→默认 200，上限 500），page_token 为 store 不透明键集游标直传，
// next_page_token 由 store 回（末页/空表为空）。node_id/orchestration_id 可选过滤透传 store（空=全部；
// M10 T9 收口 AUD-2 偏差：执行墙按节点/编排下钻）。store 为 nil（纯内存）时回空页。
func (s *ConsoleServiceServer) ListTasks(
	ctx context.Context,
	req *connect.Request[aurav1.ListTasksRequest],
) (*connect.Response[aurav1.ListTasksResponse], error) {
	if s.store == nil {
		return connect.NewResponse(&aurav1.ListTasksResponse{}), nil
	}
	pageSize := normalizeTracePageSize(req.Msg.GetPageSize())
	rows, next, err := s.store.ListTasks(ctx, pageSize, req.Msg.GetPageToken(), req.Msg.GetNodeId(), req.Msg.GetOrchestrationId())
	if err != nil {
		// store 游标解码/查询失败：游标为服务端签发的不透明串，异常即 Internal（防挂死上限已由 store clampPageSize 夹）。
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("list tasks: %w", err))
	}
	tasks := make([]*aurav1.TaskSummary, 0, len(rows))
	for _, r := range rows {
		tasks = append(tasks, taskRowToSummary(r))
	}
	return connect.NewResponse(&aurav1.ListTasksResponse{Tasks: tasks, NextPageToken: next}), nil
}

// GetTask 单任务详情钻取（批E：失败任务下钻查因）。委托 store.GetTask 点查 tasks 行（摘要 + 终态
// envelope/trace_id/finished_at 富化列）。store 为 nil（纯内存）→ Unavailable（详情面明确不可用，
// 非静默空——区别于列表页的空页降级：点查一个具体 task_id 却答「无」会误导为任务不存在）；
// 任务不存在 → NotFound。
func (s *ConsoleServiceServer) GetTask(
	ctx context.Context,
	req *connect.Request[aurav1.GetTaskRequest],
) (*connect.Response[aurav1.GetTaskResponse], error) {
	if req.Msg.GetTaskId() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("task_id is required"))
	}
	if s.store == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("task store not configured"))
	}
	d, err := s.store.GetTask(ctx, req.Msg.GetTaskId())
	if err != nil {
		if store.IsNotFound(err) {
			return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("task %q not found", req.Msg.GetTaskId()))
		}
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("get task: %w", err))
	}
	resp := &aurav1.GetTaskResponse{
		Task:         taskRowToSummary(d.TaskRow),
		JsonEnvelope: d.JsonEnvelope,
		TraceId:      d.TraceID,
	}
	if !d.FinishedAt.IsZero() {
		resp.FinishedMs = d.FinishedAt.UnixMilli()
	}
	return connect.NewResponse(resp), nil
}

// ListTraces 录制会话列表分页读（录放中心列表数据源）。委托 store.ListTraces（同 ListTasks 分页语义）。
// store 为 nil 时回空页。
//
// 注意：proto TraceSummary 的 platform/step_count/who/status 由 store.ListTraces 富化查询回真值
// （JOIN nodes 取 platform、窗口 COUNT 取 step_count、首步 who、status 恒 stopped——AUD-5）。
// store.TraceSummary.Tool 无 proto 落点，丢弃。
func (s *ConsoleServiceServer) ListTraces(
	ctx context.Context,
	req *connect.Request[aurav1.ListTracesRequest],
) (*connect.Response[aurav1.ListTracesResponse], error) {
	if s.store == nil {
		return connect.NewResponse(&aurav1.ListTracesResponse{}), nil
	}
	pageSize := normalizeTracePageSize(req.Msg.GetPageSize())
	rows, next, err := s.store.ListTraces(ctx, pageSize, req.Msg.GetPageToken())
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("list traces: %w", err))
	}
	traces := make([]*aurav1.TraceSummary, 0, len(rows))
	for _, r := range rows {
		traces = append(traces, traceRowToSummary(r))
	}
	return connect.NewResponse(&aurav1.ListTracesResponse{Traces: traces, NextPageToken: next}), nil
}

// GetQueueDepth 返回 per-node 串行队列深度（对抗 G：操作台轮询前查 >0 即跳帧，避让在途工具调用）。
// 委托 sched.QueueDepth（只读快照，节点无队列返 0）。sched 为 nil 时回 0（纯内存/未装配降级）。
func (s *ConsoleServiceServer) GetQueueDepth(
	_ context.Context,
	req *connect.Request[aurav1.GetQueueDepthRequest],
) (*connect.Response[aurav1.GetQueueDepthResponse], error) {
	if s.sched == nil {
		return connect.NewResponse(&aurav1.GetQueueDepthResponse{Depth: 0}), nil
	}
	depth := s.sched.QueueDepth(req.Msg.GetNodeId())
	return connect.NewResponse(&aurav1.GetQueueDepthResponse{Depth: int32(depth)}), nil
}

// —— 聚合/映射纯函数（免 store/reg mock 的单测缝）——————————————————————————————————————

// countNodes 按 NodeInfo.Status 分桶计数舰队全集（ListFleet：online/unhealthy/offline 三态）。offline=无
// 活跃会话的持久身份（ListFleet 对无会话者置 offline），与 WatchFleet 节点墙口径一致（PROBE 2 三口径对齐）。
// 未知 status 仅计入 total（防御性：契约外状态不误入健康桶）。
func countNodes(nodes []*aurav1.NodeInfo) (total, online, unhealthy, offline int32) {
	for _, n := range nodes {
		total++
		switch n.GetStatus() {
		case "online":
			online++
		case "unhealthy":
			unhealthy++
		case "offline":
			offline++
		}
	}
	return total, online, unhealthy, offline
}

// bucketTaskStatusCounts 将 tasks 按 status 分组的行数映射（store.CountTasksByStatus）分健康桶：
// running（在途：queued/running）、succeeded（done）、failed（其余终态：error/timeout/busy/offline/
// orphaned 及未知）。分桶语义与旧行级 countTaskStatuses 一致，改以 GROUP BY status COUNT 为源，免全表拉行。
// 总数由 CountTasks 单独取真 COUNT（见 GetDashboard），故此处不返回 total。
func bucketTaskStatusCounts(counts map[string]int64) (running, succeeded, failed int64) {
	for status, n := range counts {
		switch status {
		case "queued", "running":
			running += n
		case "done":
			succeeded += n
		default:
			failed += n
		}
	}
	return running, succeeded, failed
}

// taskRowToSummary 将 store.TaskRow 投影为 proto TaskSummary（created_at→毫秒；空 node_id/who/orch_id
// 由 store 侧 NULL 还原为空串，原样透传）。
func taskRowToSummary(r store.TaskRow) *aurav1.TaskSummary {
	return &aurav1.TaskSummary{
		TaskId:          r.ID,
		NodeId:          r.NodeID,
		Tool:            r.Tool,
		Status:          r.Status,
		Who:             r.Who,
		CreatedMs:       r.CreatedAt.UnixMilli(),
		OrchestrationId: r.OrchestrationID,
	}
}

// traceRowToSummary 将 store.TraceSummary 投影为 proto TraceSummary（ts→毫秒 started_ms）。
// platform/step_count/who/status 由 store 富化查询回真值，原样透传（status 恒 "stopped"，见 store 注释）。
func traceRowToSummary(r store.TraceSummary) *aurav1.TraceSummary {
	return &aurav1.TraceSummary{
		TraceId:   r.TraceID,
		NodeId:    r.NodeID,
		Platform:  r.Platform,
		StepCount: r.StepCount,
		Who:       r.Who,
		StartedMs: r.Ts.UnixMilli(),
		Status:    r.Status,
	}
}
