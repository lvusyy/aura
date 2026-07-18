package transport

import (
	"context"
	"fmt"

	"connectrpc.com/connect"

	aurav1 "github.com/aura/controller/gen/aura/v1"
)

// 本文件 3 个直连 MCP agent 观测 handler（M13）：概览统计 + 接入会话列表 + 调用流水分页。数据源
// s.agentObs（PG 配置时落 agent_calls/agent_sessions 两表、否则内存环形缓冲兜底）。agentObs 为 nil
//（裸构造单测/未注入）时返回空响应——与 ListTasks 的「store nil 空页」降级同族：不 panic、不误导为错误
//（区别于点查 GetTask 的 Unavailable：列表/统计面「暂无数据」是合理空态，非「不可用」）。

// GetAgentObservability 概览统计（活跃接入 / 窗口调用总数 / 失败数 / p95 时延 / top 工具）。
// window_hours=0 取服务端默认 24h（归一在 store.AgentObs.Observability）。
func (s *ConsoleServiceServer) GetAgentObservability(
	ctx context.Context,
	req *connect.Request[aurav1.GetAgentObservabilityRequest],
) (*connect.Response[aurav1.GetAgentObservabilityResponse], error) {
	if s.agentObs == nil {
		return connect.NewResponse(&aurav1.GetAgentObservabilityResponse{}), nil
	}
	resp, err := s.agentObs.Observability(ctx, req.Msg.GetWindowHours())
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("agent observability: %w", err))
	}
	return connect.NewResponse(resp), nil
}

// ListAgentSessions 接入会话列表（「谁连着」视图）。node_id 空=全部；按 last_seen 降序（活跃在前），有界不分页。
func (s *ConsoleServiceServer) ListAgentSessions(
	ctx context.Context,
	req *connect.Request[aurav1.ListAgentSessionsRequest],
) (*connect.Response[aurav1.ListAgentSessionsResponse], error) {
	if s.agentObs == nil {
		return connect.NewResponse(&aurav1.ListAgentSessionsResponse{}), nil
	}
	sessions, err := s.agentObs.Sessions(ctx, req.Msg.GetNodeId())
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("list agent sessions: %w", err))
	}
	return connect.NewResponse(&aurav1.ListAgentSessionsResponse{Sessions: sessions}), nil
}

// ListAgentCalls 调用流水键集游标分页（「调用了什么」审计表）。node_id/tool 可选等值过滤（空=不过滤）。
// 内存兜底不支持真游标，返回近期一页（next_page_token 恒空）。
func (s *ConsoleServiceServer) ListAgentCalls(
	ctx context.Context,
	req *connect.Request[aurav1.ListAgentCallsRequest],
) (*connect.Response[aurav1.ListAgentCallsResponse], error) {
	if s.agentObs == nil {
		return connect.NewResponse(&aurav1.ListAgentCallsResponse{}), nil
	}
	calls, next, err := s.agentObs.Calls(ctx, req.Msg.GetPageSize(), req.Msg.GetPageToken(),
		req.Msg.GetNodeId(), req.Msg.GetTool())
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("list agent calls: %w", err))
	}
	return connect.NewResponse(&aurav1.ListAgentCallsResponse{Calls: calls, NextPageToken: next}), nil
}
