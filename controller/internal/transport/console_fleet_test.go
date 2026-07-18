package transport

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"connectrpc.com/connect"

	aurav1 "github.com/aura/controller/gen/aura/v1"
	aurav1connect "github.com/aura/controller/gen/aura/v1/aurav1connect"
	"github.com/aura/controller/internal/registry"
	"github.com/aura/controller/internal/store"
)

// newWatchFleetTestServer 起一个仅挂 ConsoleService 的 httptest 服务（明文 HTTP/1.1，等价 connect-web 浏览器
// 可达的传输面——research §4：server-streaming HTTP/1.1 即可），返回 connect 客户端。真实 connect 流路径端到端
// 验证 WatchFleet，规避在单测手造 *connect.ServerStream（connect v1 无公开构造器）。
// ls 为租约读面（T13 recordings 回填；nil=未注入路径，recordings 恒空）。
func newWatchFleetTestServer(t *testing.T, reg *registry.NodeRegistry, ls store.LeaseStore) (*httptest.Server, aurav1connect.ConsoleServiceClient) {
	t.Helper()
	consoleSrv := NewConsoleServiceServer(reg, nil, nil, nil, nil)
	consoleSrv.SetLeaseStore(ls)
	mux := http.NewServeMux()
	path, h := aurav1connect.NewConsoleServiceHandler(consoleSrv)
	mux.Handle(path, h)
	srv := httptest.NewServer(mux)
	client := aurav1connect.NewConsoleServiceClient(srv.Client(), srv.URL)
	return srv, client
}

// TestWatchFleetFirstFrameSnapshot 验证首帧为全量快照（HEARTBEAT_SNAPSHOT，携当前在线节点），
// criterion 2：首帧发 reg.List() 全量快照。
func TestWatchFleetFirstFrameSnapshot(t *testing.T) {
	reg := registry.NewRegistry(nil)
	reg.Add(registry.NewSession("node-1", "android", []string{"click"}, "aura.v1/2026-07", 1))
	reg.Add(registry.NewSession("node-2", "linux", nil, "", 1))

	srv, client := newWatchFleetTestServer(t, reg, nil)
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream, err := client.WatchFleet(ctx, connect.NewRequest(&aurav1.WatchFleetRequest{}))
	if err != nil {
		t.Fatalf("WatchFleet open: %v", err)
	}
	defer func() { _ = stream.Close() }()

	if !stream.Receive() {
		t.Fatalf("first frame Receive failed: %v", stream.Err())
	}
	first := stream.Msg()
	if first.GetType() != aurav1.FleetEventType_FLEET_EVENT_TYPE_HEARTBEAT_SNAPSHOT {
		t.Fatalf("first frame type = %v, want HEARTBEAT_SNAPSHOT", first.GetType())
	}
	if got := len(first.GetSnapshot()); got != 2 {
		t.Fatalf("first frame snapshot len = %d, want 2", got)
	}
}

// TestWatchFleetIncrementalEvent 验证订阅生效后新增节点即经流推送 node_added 增量事件，且 seq 严格 > 快照基线
// （criterion 2/5：首帧后推增量、seq 单调）。首帧到达即证服务端已完成 Subscribe，故其后的 Add 必被捕获（无竞态）。
func TestWatchFleetIncrementalEvent(t *testing.T) {
	reg := registry.NewRegistry(nil)
	srv, client := newWatchFleetTestServer(t, reg, nil)
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream, err := client.WatchFleet(ctx, connect.NewRequest(&aurav1.WatchFleetRequest{}))
	if err != nil {
		t.Fatalf("WatchFleet open: %v", err)
	}
	defer func() { _ = stream.Close() }()

	if !stream.Receive() { // 首帧快照（空舰队）
		t.Fatalf("first frame: %v", stream.Err())
	}
	snapSeq := stream.Msg().GetSeq()

	reg.Add(registry.NewSession("node-x", "android", nil, "", 1))
	if !stream.Receive() {
		t.Fatalf("incremental frame: %v", stream.Err())
	}
	ev := stream.Msg()
	if ev.GetType() != aurav1.FleetEventType_FLEET_EVENT_TYPE_NODE_ADDED {
		t.Fatalf("incremental type = %v, want NODE_ADDED", ev.GetType())
	}
	if ev.GetSeq() <= snapSeq {
		t.Fatalf("incremental seq %d not strictly > snapshot seq %d", ev.GetSeq(), snapSeq)
	}
	if ev.GetNode().GetNodeId() != "node-x" {
		t.Fatalf("incremental node = %q, want node-x", ev.GetNode().GetNodeId())
	}
}

// TestWatchFleetCancelNoLeak 验证客户端取消/流关闭时服务端正确退订释放订阅者槽（criterion 4：无 goroutine
// 泄漏）。取消后轮询 ObserverCount 归零（退订经 handler ctx.Done → defer cancel 异步完成）。
func TestWatchFleetCancelNoLeak(t *testing.T) {
	reg := registry.NewRegistry(nil)
	srv, client := newWatchFleetTestServer(t, reg, nil)
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	stream, err := client.WatchFleet(ctx, connect.NewRequest(&aurav1.WatchFleetRequest{}))
	if err != nil {
		t.Fatalf("WatchFleet open: %v", err)
	}
	if !stream.Receive() { // 首帧到达即证服务端已 Subscribe（observer 已注册）
		t.Fatalf("first frame: %v", stream.Err())
	}
	if got := reg.ObserverCount(); got != 1 {
		t.Fatalf("during stream: observer count = %d, want 1", got)
	}

	cancel()             // 客户端取消 → 服务端 ctx.Done → handler 退订
	_ = stream.Close()

	deadline := time.Now().Add(2 * time.Second)
	for reg.ObserverCount() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("after cancel: observer count = %d, want 0 (goroutine/chan leak)", reg.ObserverCount())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestWatchFleetRecordings（T13 租约期 UX）：快照帧对租约命中节点携 recordings 表项（who/trace_id
// 载荷）、未租约节点缺省；增量帧对覆盖节点同样回填（被租=有项 / 未租=空）。租约读面未注入路径
//（recordings 恒空、既有字段零断裂）由上方三个 nil 注入用例覆盖。
func TestWatchFleetRecordings(t *testing.T) {
	reg := registry.NewRegistry(nil)
	reg.Add(registry.NewSession("node-rec", "android", nil, "", 1))
	reg.Add(registry.NewSession("node-idle", "linux", nil, "", 1))

	ls := store.NewInMemoryLeaseStore()
	if err := ls.Acquire(context.Background(), "node-rec", "trace-9", "recorder-alice", 30*time.Minute); err != nil {
		t.Fatalf("Acquire(node-rec): %v", err)
	}

	srv, client := newWatchFleetTestServer(t, reg, ls)
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream, err := client.WatchFleet(ctx, connect.NewRequest(&aurav1.WatchFleetRequest{}))
	if err != nil {
		t.Fatalf("WatchFleet open: %v", err)
	}
	defer func() { _ = stream.Close() }()

	// 首帧快照：recordings 恰含被租的 node-rec 一项，node-idle 缺省（覆盖节点不在表=未录制）。
	if !stream.Receive() {
		t.Fatalf("first frame: %v", stream.Err())
	}
	first := stream.Msg()
	if got := len(first.GetRecordings()); got != 1 {
		t.Fatalf("snapshot recordings len = %d, want 1（命中节点入表、未租约节点缺省）", got)
	}
	rec := first.GetRecordings()[0]
	if rec.GetNodeId() != "node-rec" || rec.GetWho() != "recorder-alice" || rec.GetTraceId() != "trace-9" {
		t.Fatalf("snapshot recording = %+v, want {node-rec recorder-alice trace-9}", rec)
	}

	// 增量帧（被租节点）：先建租约再注册节点，NODE_ADDED 帧应回填该节点录制态。
	if err := ls.Acquire(context.Background(), "node-new", "trace-10", "recorder-bob", 30*time.Minute); err != nil {
		t.Fatalf("Acquire(node-new): %v", err)
	}
	reg.Add(registry.NewSession("node-new", "windows", nil, "", 1))
	if !stream.Receive() {
		t.Fatalf("incremental frame (leased): %v", stream.Err())
	}
	ev := stream.Msg()
	if ev.GetNode().GetNodeId() != "node-new" || len(ev.GetRecordings()) != 1 {
		t.Fatalf("incremental (leased) = node %q recordings %v, want node-new + 1 项",
			ev.GetNode().GetNodeId(), ev.GetRecordings())
	}
	if r := ev.GetRecordings()[0]; r.GetNodeId() != "node-new" || r.GetWho() != "recorder-bob" || r.GetTraceId() != "trace-10" {
		t.Fatalf("incremental recording = %+v, want {node-new recorder-bob trace-10}", r)
	}

	// 增量帧（未租节点）：recordings 缺省空。
	reg.Add(registry.NewSession("node-plain", "linux", nil, "", 1))
	if !stream.Receive() {
		t.Fatalf("incremental frame (plain): %v", stream.Err())
	}
	ev = stream.Msg()
	if ev.GetNode().GetNodeId() != "node-plain" || len(ev.GetRecordings()) != 0 {
		t.Fatalf("incremental (plain) = node %q recordings %v, want node-plain + 空",
			ev.GetNode().GetNodeId(), ev.GetRecordings())
	}
}
