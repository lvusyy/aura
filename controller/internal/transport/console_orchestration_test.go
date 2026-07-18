package transport

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"

	aurav1 "github.com/aura/controller/gen/aura/v1"
	"github.com/aura/controller/internal/orchestrator"
	"github.com/aura/controller/internal/registry"
	"github.com/aura/controller/internal/scheduler"
	"github.com/aura/controller/internal/store"
)

// 编排入口 handler 单测（批E C8：同目录 console_*.go 均有测试，唯编排三 handler 缺）：
// RunOrchestration 目标解析失败折射 InvalidArgument / 令牌作用域门控（批E C1）折射 PermissionDenied；
// GetOrchestration/ListOrchestrations 的 store==nil 降级分支。store 真分页/NotFound 需真 PG，交
// pg_integration_test 与构建机 live curl（console_query_test 同口径，不构真 PG）。

// newOrchTestServer 组装免 PG 的编排测试面：真 registry（空舰队）+ 真 scheduler（纯内存）+
// 真 orchestrator（store nil）——RunOrchestration 的目标解析/门控分支在 handler 层即返回，不触 PG。
func newOrchTestServer() *ConsoleServiceServer {
	reg := registry.NewRegistry(nil)
	sched := scheduler.NewScheduler(reg, nil, time.Minute, nil)
	orch := orchestrator.NewOrchestrator(reg, sched, nil)
	return NewConsoleServiceServer(reg, nil, sched, orch, nil)
}

func TestRunOrchestrationNoTargetsInvalidArgument(t *testing.T) {
	s := newOrchTestServer()
	_, err := s.RunOrchestration(context.Background(), connect.NewRequest(&aurav1.RunOrchestrationRequest{
		Tool: "screenshot", // node_ids 与 env_group 均空 → ErrNoTargets
	}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("empty targets: code = %v, want InvalidArgument (err=%v)", connect.CodeOf(err), err)
	}
}

func TestRunOrchestrationTooManyTargetsInvalidArgument(t *testing.T) {
	s := newOrchTestServer()
	ids := make([]string, 300) // > maxFanout(256)
	for i := range ids {
		ids[i] = "node-" + string(rune('a'+i%26)) + string(rune('0'+i%10)) + string(rune('0'+i/10%10)) + string(rune('0'+i/100))
	}
	_, err := s.RunOrchestration(context.Background(), connect.NewRequest(&aurav1.RunOrchestrationRequest{
		Tool:    "screenshot",
		NodeIds: ids,
	}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("over fan-out limit: code = %v, want InvalidArgument (err=%v)", connect.CodeOf(err), err)
	}
}

// TestRunOrchestrationScopeGate（批E C1）：ro 档拒一切编排派发；ops 档拒高影响工具；admin/空 scope
// 放行至目标解析（空目标 InvalidArgument 即证已通过门控）。
func TestRunOrchestrationScopeGate(t *testing.T) {
	s := newOrchTestServer()
	cases := []struct {
		name  string
		scope string
		tool  string
		want  connect.Code
	}{
		{"ro rejects any dispatch", ScopeReadOnly, "screenshot", connect.CodePermissionDenied},
		{"ops rejects gated run_command", ScopeOps, "run_command", connect.CodePermissionDenied},
		{"ops rejects gated kill_process", ScopeOps, "kill_process", connect.CodePermissionDenied},
		{"ops allows regular tool", ScopeOps, "screenshot", connect.CodeInvalidArgument}, // 过门控，落空目标
		{"admin allows gated tool", ScopeAdmin, "run_command", connect.CodeInvalidArgument},
		{"no scope (legacy/internal) allows", "", "run_command", connect.CodeInvalidArgument},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			if tc.scope != "" {
				ctx = context.WithValue(ctx, scopeKey, tc.scope)
			}
			_, err := s.RunOrchestration(ctx, connect.NewRequest(&aurav1.RunOrchestrationRequest{Tool: tc.tool}))
			if connect.CodeOf(err) != tc.want {
				t.Fatalf("scope=%q tool=%q: code = %v, want %v (err=%v)", tc.scope, tc.tool, connect.CodeOf(err), tc.want, err)
			}
		})
	}
}

func TestGetOrchestrationStoreNilNotFound(t *testing.T) {
	s := newOrchTestServer() // store == nil（纯内存）
	_, err := s.GetOrchestration(context.Background(), connect.NewRequest(&aurav1.GetOrchestrationRequest{
		OrchestrationId: "00000000-0000-0000-0000-000000000000",
	}))
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("store nil: code = %v, want NotFound (err=%v)", connect.CodeOf(err), err)
	}
}

func TestListOrchestrationsStoreNilEmptyPage(t *testing.T) {
	s := newOrchTestServer()
	resp, err := s.ListOrchestrations(context.Background(), connect.NewRequest(&aurav1.ListOrchestrationsRequest{}))
	if err != nil {
		t.Fatalf("store nil list: err = %v, want nil (empty-page degrade)", err)
	}
	if len(resp.Msg.GetOrchestrations()) != 0 || resp.Msg.GetNextPageToken() != "" {
		t.Fatalf("store nil list = %d rows token=%q, want empty page", len(resp.Msg.GetOrchestrations()), resp.Msg.GetNextPageToken())
	}
}

// TestToProtoSummaryMapping：orchestrations 表行 → proto 摘要的字段映射（含 trace_id 持久列回填）。
func TestToProtoSummaryMapping(t *testing.T) {
	at := time.UnixMilli(1_700_000_000_000)
	got := toProtoSummary(store.OrchestrationRecord{
		ID: "o1", Tool: "click", Status: "partial", Total: 3, Passed: 2, Failed: 1,
		Who: "agent", TraceID: "abc123", CreatedAt: at,
	})
	if got.GetOrchestrationId() != "o1" || got.GetTool() != "click" || got.GetStatus() != "partial" ||
		got.GetTotal() != 3 || got.GetPassed() != 2 || got.GetFailed() != 1 ||
		got.GetWho() != "agent" || got.GetTraceId() != "abc123" || got.GetStartedMs() != at.UnixMilli() {
		t.Fatalf("toProtoSummary mapping mismatch: %+v", got)
	}
}

// TestToProtoEnvResultsMapping：编排层 EnvResult → proto 的逐字段透传（envelope 字节原样）。
func TestToProtoEnvResultsMapping(t *testing.T) {
	rs := toProtoEnvResults([]orchestrator.EnvResult{
		{NodeID: "n1", TaskID: "t1", Status: "succeeded", JsonEnvelope: []byte(`{"ok":true}`), LatencyMs: 42},
	})
	if len(rs) != 1 {
		t.Fatalf("len = %d, want 1", len(rs))
	}
	r := rs[0]
	if r.GetNodeId() != "n1" || r.GetTaskId() != "t1" || r.GetStatus() != "succeeded" ||
		string(r.GetJsonEnvelope()) != `{"ok":true}` || r.GetLatencyMs() != 42 {
		t.Fatalf("toProtoEnvResults mapping mismatch: %+v", r)
	}
}
