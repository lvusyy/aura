package transport

import (
	"context"
	"errors"
	"fmt"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"

	aurav1 "github.com/aura/controller/gen/aura/v1"
	"github.com/aura/controller/internal/orchestrator"
	"github.com/aura/controller/internal/store"
)

// 本文件 3 个编排 handler：RunOrchestration 委托 orchestrator.Run（fan-out/gather），GetOrchestration/
// ListOrchestrations 委托 store 读路径（钻取/分页）。目标解析失败折射 InvalidArgument，编排不存在折射
// NotFound（pgx.ErrNoRows，同 GetEnvironment 惯例）；store 未配置（纯内存）时读路径降级空/NotFound。

// RunOrchestration 并行编排触发（fan-out/gather）：一条用例分发至 N 个目标节点，join 聚合每环境 pass/fail
// + 延迟。委托 orchestrator.Run；空目标/超限 → InvalidArgument。
// 批E C1：与 DispatchTool 同门控——编排是同一工具的批量派发面，作用域检查与审计同纪律。
func (s *ConsoleServiceServer) RunOrchestration(
	ctx context.Context,
	req *connect.Request[aurav1.RunOrchestrationRequest],
) (*connect.Response[aurav1.RunOrchestrationResponse], error) {
	if err := CheckDispatchScope(ctx, req.Msg.GetTool()); err != nil {
		return nil, err
	}
	auditDispatch(ctx, "RunOrchestration", req.Msg.GetWho(),
		fmt.Sprintf("fanout:%d+group:%s", len(req.Msg.GetNodeIds()), req.Msg.GetEnvGroup()),
		req.Msg.GetTool(), len(req.Msg.GetJsonArgs()))
	m := req.Msg
	res, err := s.orch.Run(ctx, orchestrator.OrchestrationSpec{
		Tool:       m.GetTool(),
		JsonArgs:   m.GetJsonArgs(),
		NodeIDs:    m.GetNodeIds(),
		EnvGroup:   m.GetEnvGroup(),
		DeadlineMs: m.GetDeadlineMs(),
		Who:        m.GetWho(),
	})
	if err != nil {
		// 目标解析失败（node_ids/env_group 均空或解析空集、超上限）——调用方入参问题。
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	return connect.NewResponse(&aurav1.RunOrchestrationResponse{
		OrchestrationId: res.OrchestrationID,
		Status:          res.Status,
		Total:           res.Total,
		Passed:          res.Passed,
		Failed:          res.Failed,
		PerEnv:          toProtoEnvResults(res.PerEnv),
	}), nil
}

// GetOrchestration 编排详情钻取（orchestration_id → 汇总 + 关联 task_ids）。委托 store：读编排行 + 关联
// 任务行的 id 集（执行墙下钻 → ListTasks(orchestration_id) 明细）。不存在 → NotFound；store 未配置 →
// NotFound（纯内存无持久化编排）。
func (s *ConsoleServiceServer) GetOrchestration(
	ctx context.Context,
	req *connect.Request[aurav1.GetOrchestrationRequest],
) (*connect.Response[aurav1.GetOrchestrationResponse], error) {
	id := req.Msg.GetOrchestrationId()
	if s.store == nil {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("orchestration store not configured (in-memory mode)"))
	}
	rec, err := s.store.GetOrchestration(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("orchestration %s not found", id))
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	tasks, err := s.store.GetOrchestrationTasks(ctx, id)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	taskIDs := make([]string, 0, len(tasks))
	for _, t := range tasks {
		taskIDs = append(taskIDs, t.ID)
	}
	return connect.NewResponse(&aurav1.GetOrchestrationResponse{
		Orchestration: toProtoSummary(rec),
		TaskIds:       taskIDs,
	}), nil
}

// ListOrchestrations 编排历史分页读。委托 store.ListOrchestrations（键集游标分页）；store 未配置 → 空列表。
func (s *ConsoleServiceServer) ListOrchestrations(
	ctx context.Context,
	req *connect.Request[aurav1.ListOrchestrationsRequest],
) (*connect.Response[aurav1.ListOrchestrationsResponse], error) {
	if s.store == nil {
		return connect.NewResponse(&aurav1.ListOrchestrationsResponse{}), nil
	}
	recs, next, err := s.store.ListOrchestrations(ctx, req.Msg.GetPageSize(), req.Msg.GetPageToken())
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	out := make([]*aurav1.OrchestrationSummary, 0, len(recs))
	for _, r := range recs {
		out = append(out, toProtoSummary(r))
	}
	return connect.NewResponse(&aurav1.ListOrchestrationsResponse{
		Orchestrations: out,
		NextPageToken:  next,
	}), nil
}

// toProtoEnvResults 把编排层 EnvResult 映射为 proto EnvResult（per-env join 明细）。
func toProtoEnvResults(rs []orchestrator.EnvResult) []*aurav1.EnvResult {
	out := make([]*aurav1.EnvResult, 0, len(rs))
	for _, r := range rs {
		out = append(out, &aurav1.EnvResult{
			NodeId:       r.NodeID,
			TaskId:       r.TaskID,
			Status:       r.Status,
			JsonEnvelope: r.JsonEnvelope,
			LatencyMs:    r.LatencyMs,
		})
	}
	return out
}

// toProtoSummary 把 orchestrations 表行映射为 proto OrchestrationSummary。StartedMs 取 CreatedAt；trace_id
// 自 orchestrations.trace_id 持久化列回填（M10 T9：编排层 Run 从 fan-out 顶 span 捕获落表；未启用追踪
// 或 M10 前存量行为空串）。
func toProtoSummary(rec store.OrchestrationRecord) *aurav1.OrchestrationSummary {
	return &aurav1.OrchestrationSummary{
		OrchestrationId: rec.ID,
		Tool:            rec.Tool,
		Status:          rec.Status,
		Total:           rec.Total,
		Passed:          rec.Passed,
		Failed:          rec.Failed,
		Who:             rec.Who,
		StartedMs:       rec.CreatedAt.UnixMilli(),
		TraceId:         rec.TraceID,
	}
}
