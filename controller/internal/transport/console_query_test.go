package transport

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"

	aurav1 "github.com/aura/controller/gen/aura/v1"
	"github.com/aura/controller/internal/registry"
	"github.com/aura/controller/internal/scheduler"
	"github.com/aura/controller/internal/store"
)

// 纯函数覆盖聚合分类/字段映射（reg/store/sched 具体类型无法 mock，纯函数是免 PG 的单测缝）；
// handler 用例覆盖空依赖降级 + 真 registry 节点计数 + 真 scheduler 队列深度。store 分页/错误路径需
// 真 PG，交构建机 live curl（TASK-012）验，本单测不构真 PG。

func TestCountNodes(t *testing.T) {
	tests := []struct {
		name                                                string
		nodes                                               []*aurav1.NodeInfo
		wantTotal, wantOnline, wantUnhealthy, wantOffline   int32
	}{
		{"empty", nil, 0, 0, 0, 0},
		{"all online", []*aurav1.NodeInfo{{Status: "online"}, {Status: "online"}}, 2, 2, 0, 0},
		{"mixed online+unhealthy", []*aurav1.NodeInfo{{Status: "online"}, {Status: "unhealthy"}, {Status: "online"}}, 3, 2, 1, 0},
		// ListFleet 全集含 offline 持久身份（PROBE 2 三口径对齐：摘要与墙面同源计 offline）。
		{"fleet full set with offline", []*aurav1.NodeInfo{{Status: "online"}, {Status: "offline"}, {Status: "offline"}, {Status: "unhealthy"}}, 4, 1, 1, 2},
		{"unknown status counts total only", []*aurav1.NodeInfo{{Status: "online"}, {Status: "weird"}}, 2, 1, 0, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			total, online, unhealthy, offline := countNodes(tt.nodes)
			if total != tt.wantTotal || online != tt.wantOnline || unhealthy != tt.wantUnhealthy || offline != tt.wantOffline {
				t.Fatalf("countNodes = (total=%d online=%d unhealthy=%d offline=%d), want (%d %d %d %d)",
					total, online, unhealthy, offline, tt.wantTotal, tt.wantOnline, tt.wantUnhealthy, tt.wantOffline)
			}
		})
	}
}

func TestBucketTaskStatusCounts(t *testing.T) {
	// GROUP BY status 计数映射分桶：running=queued+running=6；succeeded=done=3；failed=其余终态+未知=10。
	counts := map[string]int64{
		"queued": 4, "running": 2, // running 桶 = 6（在途）
		"done": 3, // succeeded 桶 = 3
		"error": 1, "timeout": 1, "busy": 1, "offline": 1, "orphaned": 1, "mystery": 5, // failed 桶 = 10（含未知 default）
	}
	running, succeeded, failed := bucketTaskStatusCounts(counts)
	if running != 6 || succeeded != 3 || failed != 10 {
		t.Fatalf("bucketTaskStatusCounts = (running=%d succeeded=%d failed=%d), want (6 3 10)",
			running, succeeded, failed)
	}
	// 空映射（空表 / nil）→ 全零桶。
	if r, s, f := bucketTaskStatusCounts(nil); r != 0 || s != 0 || f != 0 {
		t.Fatalf("nil 映射应全零, got (%d %d %d)", r, s, f)
	}
	if r, s, f := bucketTaskStatusCounts(map[string]int64{}); r != 0 || s != 0 || f != 0 {
		t.Fatalf("空 map 应全零, got (%d %d %d)", r, s, f)
	}
}

func TestTaskRowToSummary(t *testing.T) {
	ts := time.UnixMilli(1720000000000)
	got := taskRowToSummary(store.TaskRow{
		ID: "task-1", NodeID: "node-1", Tool: "screenshot", Status: "done",
		Who: "cli", OrchestrationID: "orch-1", CreatedAt: ts,
	})
	if got.GetTaskId() != "task-1" || got.GetNodeId() != "node-1" || got.GetTool() != "screenshot" ||
		got.GetStatus() != "done" || got.GetWho() != "cli" || got.GetOrchestrationId() != "orch-1" {
		t.Fatalf("taskRowToSummary 字段错配: %+v", got)
	}
	if got.GetCreatedMs() != 1720000000000 {
		t.Fatalf("CreatedMs = %d, want 1720000000000", got.GetCreatedMs())
	}
	// 空可空字段（store 侧 NULL→空串）原样透传空串。
	empty := taskRowToSummary(store.TaskRow{ID: "t", CreatedAt: ts})
	if empty.GetNodeId() != "" || empty.GetWho() != "" || empty.GetOrchestrationId() != "" {
		t.Fatalf("空可空字段须映射空串: %+v", empty)
	}
}

func TestTraceRowToSummary(t *testing.T) {
	ts := time.UnixMilli(1720000005000)
	got := traceRowToSummary(store.TraceSummary{
		TraceID: "trace-1", Tool: "click", NodeID: "node-2",
		Platform: "android", StepCount: 7, Who: "cli", Status: "stopped", Ts: ts,
	})
	if got.GetTraceId() != "trace-1" || got.GetNodeId() != "node-2" || got.GetStartedMs() != 1720000005000 {
		t.Fatalf("traceRowToSummary 核心字段错配: %+v", got)
	}
	// 富化字段（AUD-5）原样透传：platform/step_count/who/status。store.Tool 无 proto 落点，丢弃。
	if got.GetPlatform() != "android" || got.GetStepCount() != 7 || got.GetWho() != "cli" || got.GetStatus() != "stopped" {
		t.Fatalf("富化字段映射错: platform=%q step=%d who=%q status=%q",
			got.GetPlatform(), got.GetStepCount(), got.GetWho(), got.GetStatus())
	}
	// 空可空字段（store 侧 NULL→空串）原样透传空串/零。
	empty := traceRowToSummary(store.TraceSummary{TraceID: "t", Ts: ts})
	if empty.GetPlatform() != "" || empty.GetWho() != "" || empty.GetStepCount() != 0 {
		t.Fatalf("空富化字段须透传零值: %+v", empty)
	}
}

func TestGetQueueDepth(t *testing.T) {
	ctx := context.Background()

	// nil sched：降级回 depth 0，无错误。
	srvNil := NewConsoleServiceServer(nil, nil, nil, nil, nil)
	resp, err := srvNil.GetQueueDepth(ctx, connect.NewRequest(&aurav1.GetQueueDepthRequest{NodeId: "any"}))
	if err != nil {
		t.Fatalf("nil sched GetQueueDepth err: %v", err)
	}
	if resp.Msg.GetDepth() != 0 {
		t.Fatalf("nil sched depth = %d, want 0", resp.Msg.GetDepth())
	}

	// 真 scheduler，未知节点无队列 → 0（对抗 G 轮询前查）。
	sched := scheduler.NewScheduler(nil, nil, 0, nil)
	srv := NewConsoleServiceServer(nil, nil, sched, nil, nil)
	resp, err = srv.GetQueueDepth(ctx, connect.NewRequest(&aurav1.GetQueueDepthRequest{NodeId: "unknown-node"}))
	if err != nil {
		t.Fatalf("GetQueueDepth err: %v", err)
	}
	if resp.Msg.GetDepth() != 0 {
		t.Fatalf("unknown node depth = %d, want 0", resp.Msg.GetDepth())
	}
}

func TestListTasks_NilStoreEmptyPage(t *testing.T) {
	srv := NewConsoleServiceServer(nil, nil, nil, nil, nil)
	resp, err := srv.ListTasks(context.Background(), connect.NewRequest(&aurav1.ListTasksRequest{PageSize: 50}))
	if err != nil {
		t.Fatalf("nil store ListTasks err: %v", err)
	}
	if len(resp.Msg.GetTasks()) != 0 || resp.Msg.GetNextPageToken() != "" {
		t.Fatalf("nil store 须回空页, got %d tasks token=%q", len(resp.Msg.GetTasks()), resp.Msg.GetNextPageToken())
	}
}

func TestListTraces_NilStoreEmptyPage(t *testing.T) {
	srv := NewConsoleServiceServer(nil, nil, nil, nil, nil)
	resp, err := srv.ListTraces(context.Background(), connect.NewRequest(&aurav1.ListTracesRequest{PageSize: 50}))
	if err != nil {
		t.Fatalf("nil store ListTraces err: %v", err)
	}
	if len(resp.Msg.GetTraces()) != 0 || resp.Msg.GetNextPageToken() != "" {
		t.Fatalf("nil store 须回空页, got %d traces token=%q", len(resp.Msg.GetTraces()), resp.Msg.GetNextPageToken())
	}
}

func TestGetDashboard_NilDepsAllZero(t *testing.T) {
	srv := NewConsoleServiceServer(nil, nil, nil, nil, nil)
	resp, err := srv.GetDashboard(context.Background(), connect.NewRequest(&aurav1.GetDashboardRequest{}))
	if err != nil {
		t.Fatalf("nil deps GetDashboard err: %v", err)
	}
	m := resp.Msg
	if m.GetNodesTotal() != 0 || m.GetNodesOnline() != 0 || m.GetTasksTotal() != 0 ||
		m.GetOrchestrationsTotal() != 0 || m.GetTracesTotal() != 0 {
		t.Fatalf("nil deps dashboard 须全零, got %+v", m)
	}
}

func TestGetDashboard_NodeCountsNilStore(t *testing.T) {
	reg := registry.NewRegistry(nil)
	reg.Add(registry.NewSession("n1", "windows", nil, "v1", 1))
	reg.Add(registry.NewSession("n2", "android", nil, "v1", 1))
	reg.Add(registry.NewSession("n3", "linux", nil, "v1", 1))

	srv := NewConsoleServiceServer(reg, nil, nil, nil, nil)
	resp, err := srv.GetDashboard(context.Background(), connect.NewRequest(&aurav1.GetDashboardRequest{}))
	if err != nil {
		t.Fatalf("GetDashboard err: %v", err)
	}
	m := resp.Msg
	// 新建会话 lastSeen=now → 全 online。
	if m.GetNodesTotal() != 3 || m.GetNodesOnline() != 3 || m.GetNodesUnhealthy() != 0 {
		t.Fatalf("节点计数 = total%d online%d unhealthy%d, want 3/3/0",
			m.GetNodesTotal(), m.GetNodesOnline(), m.GetNodesUnhealthy())
	}
	// nil store：任务/编排/录制计数全 0（不触发 store 查询）。
	if m.GetTasksTotal() != 0 || m.GetOrchestrationsTotal() != 0 || m.GetTracesTotal() != 0 {
		t.Fatalf("nil store 统计须为零, got tasks%d orch%d traces%d",
			m.GetTasksTotal(), m.GetOrchestrationsTotal(), m.GetTracesTotal())
	}
}
