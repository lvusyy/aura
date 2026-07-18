package scheduler

// forward_test.go 覆盖 T8 跨副本转发（ha-contract §1.4 错误码语义表全场景）：
// 转发发出+envelope 逐字节透传（行 6）/六字段穿透+hop header/防环一跳终态（行 8）/无 owner 键
// （行 3）/owner==self（行 4）/owner 不在 peers 表（行 5）/转发网络错终态不重试（行 7）/owner
// 读错误降级（行 10）/未配置 forwarder 现行路径（行 2）+ GAP-4 本地路径 TraceId 穿透。
// fake owner 端点用 httptest + 生成 ControllerAdminHandler（与真 owner 副本同协议面）。

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	aurav1 "github.com/aura/controller/gen/aura/v1"
	"github.com/aura/controller/gen/aura/v1/aurav1connect"
	"github.com/aura/controller/internal/registry"
)

// fakeOwners 是 OwnerReader 的测试替身；calls 计数供防环用例断言「入站转发不查 owner 表」。
type fakeOwners struct {
	mu    sync.Mutex
	owner string
	err   error
	calls int
}

func (f *fakeOwners) GetNodeOwner(_ context.Context, _ string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.owner, f.err
}

func (f *fakeOwners) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// fakePeerAdmin 以生成 handler 形态扮演 owner 副本的 ControllerAdmin：记录收到的 DispatchTool
// 请求（六字段+header），按配置返回固定 envelope 或错误。
type fakePeerAdmin struct {
	aurav1connect.UnimplementedControllerAdminHandler

	mu       sync.Mutex
	requests []*aurav1.DispatchToolRequest
	headers  []http.Header
	envelope []byte
	failWith error
}

func (f *fakePeerAdmin) DispatchTool(_ context.Context, req *connect.Request[aurav1.DispatchToolRequest]) (*connect.Response[aurav1.DispatchToolResponse], error) {
	f.mu.Lock()
	f.requests = append(f.requests, req.Msg)
	f.headers = append(f.headers, req.Header().Clone())
	fail, env := f.failWith, f.envelope
	f.mu.Unlock()
	if fail != nil {
		return nil, connect.NewError(connect.CodeUnavailable, fail)
	}
	return connect.NewResponse(&aurav1.DispatchToolResponse{JsonEnvelope: env}), nil
}

func (f *fakePeerAdmin) requestCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

func (f *fakePeerAdmin) recorded(i int) (*aurav1.DispatchToolRequest, http.Header) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests[i], f.headers[i]
}

// startFakePeer 起 httptest owner 端点（明文 HTTP，connect unary 走 h1 即可）。
func startFakePeer(t *testing.T, peer *fakePeerAdmin) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	path, handler := aurav1connect.NewControllerAdminHandler(peer)
	mux.Handle(path, handler)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// newForwardingScheduler 构造「本副本无该 node 会话」的 scheduler 并装配 forwarder：
// peers 表含 self（replica-self，NewForwarder 应过滤）与指向 fake owner 端点的 replica-peer。
func newForwardingScheduler(t *testing.T, owners *fakeOwners, peerURL string) *Scheduler {
	t.Helper()
	s := NewScheduler(registry.NewRegistry(nil), nil, 0, nil)
	peers := map[string]string{
		"replica-self": "https://self.invalid:18080",
		"replica-peer": peerURL,
	}
	s.SetForwarder(NewForwarder("replica-self", peers, owners, http.DefaultClient, "test-token", time.Minute))
	return s
}

// TestForwardDispatchToOwnerPeer：错误码表行 6——Ready 失败 + owner=peer → 转发发出，owner 所产
// envelope（含错误信封）逐字节透传、code 恒 ""；DispatchToolRequest 六字段原样复制（who/trace_id
// 经 proto 字段穿透，无 header 携带）；hop 标记与 bearer 随请求；DispatchTracked 归还 task_id=""
// （owner 侧生成并落审计，D12 响应不加字段）。
func TestForwardDispatchToOwnerPeer(t *testing.T) {
	// owner 侧 E_BUSY 错误信封：印证「B 侧一切结果统一为 envelope bytes 透传，入口不回解析派生 code」。
	env := []byte(`{"ok":false,"error":{"code":"E_BUSY","message":"lease held by another trace"}}`)
	peer := &fakePeerAdmin{envelope: env}
	srv := startFakePeer(t, peer)
	owners := &fakeOwners{owner: "replica-peer"}
	s := newForwardingScheduler(t, owners, srv.URL)

	args := []byte(`{"x":1,"y":2}`)
	resp, taskID, code, err := s.DispatchTracked(context.Background(), "node-x", "click", args, 5000, "alice", "tr-42")
	if err != nil {
		t.Fatalf("forwarded dispatch: unexpected err %v", err)
	}
	if code != "" {
		t.Fatalf("forwarded dispatch: got code %q, want empty (no envelope re-derivation)", code)
	}
	if taskID != "" {
		t.Errorf("forwarded dispatch: got task_id %q, want empty (owner-side audit, D12 no response field)", taskID)
	}
	if !bytes.Equal(resp.GetJsonEnvelope(), env) {
		t.Errorf("forwarded envelope not byte-identical:\n got %s\nwant %s", resp.GetJsonEnvelope(), env)
	}

	if n := peer.requestCount(); n != 1 {
		t.Fatalf("peer received %d requests, want exactly 1", n)
	}
	req, hdr := peer.recorded(0)
	if req.GetNodeId() != "node-x" || req.GetTool() != "click" || !bytes.Equal(req.GetJsonArgs(), args) ||
		req.GetDeadlineMs() != 5000 || req.GetWho() != "alice" || req.GetTraceId() != "tr-42" {
		t.Errorf("forwarded request fields not copied verbatim: %+v", req)
	}
	if got := hdr.Get(ForwardedByHeader); got != "replica-self" {
		t.Errorf("hop header %s = %q, want originating replica id %q", ForwardedByHeader, got, "replica-self")
	}
	if got := hdr.Get("Authorization"); got != "Bearer test-token" {
		t.Errorf("forwarded Authorization = %q, want bearer of shared token", got)
	}
}

// TestInboundForwardMarkerMiddleware：入站带 X-Aura-Forwarded-By 的请求经 middleware 后 ctx 打标；
// 不带 header 零标记（标记经 connect handler ctx 贯通至 dispatch 的前提）。
func TestInboundForwardMarkerMiddleware(t *testing.T) {
	var marked bool
	inner := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		marked = isInboundForward(r.Context())
	})
	h := InboundForwardMarker(inner)

	req := httptest.NewRequest(http.MethodPost, "/aura.v1.ControllerAdmin/DispatchTool", nil)
	req.Header.Set(ForwardedByHeader, "replica-1")
	h.ServeHTTP(httptest.NewRecorder(), req)
	if !marked {
		t.Errorf("request with %s header: ctx not marked as inbound forward", ForwardedByHeader)
	}

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/aura.v1.ControllerAdmin/DispatchTool", nil))
	if marked {
		t.Errorf("request without %s header: ctx unexpectedly marked", ForwardedByHeader)
	}
}

// TestInboundForwardNoSecondHop：错误码表行 8（m-1 防环）——入站已带转发标记 + Ready 失败 →
// 不查 owner 表（此刻指向必 stale）、不二次转发，直接 E_NODE_OFFLINE 终态。
func TestInboundForwardNoSecondHop(t *testing.T) {
	peer := &fakePeerAdmin{envelope: []byte(`{"ok":true}`)}
	srv := startFakePeer(t, peer)
	owners := &fakeOwners{owner: "replica-peer"} // owner 表指向 peer，但入站标记必须让它根本不被读
	s := newForwardingScheduler(t, owners, srv.URL)

	ctx := context.WithValue(context.Background(), inboundForwardKey{}, true)
	resp, code, err := s.Dispatch(ctx, "node-x", "click", nil, 0, "replica-peer-gw", "")
	if code != CodeNodeOffline {
		t.Fatalf("inbound-forwarded dispatch on non-ready node: got code %q, want %q", code, CodeNodeOffline)
	}
	if resp != nil || err == nil {
		t.Errorf("inbound-forwarded dispatch: want nil resp + non-nil err, got resp=%v err=%v", resp, err)
	}
	if n := owners.callCount(); n != 0 {
		t.Errorf("owner table read %d times, want 0 (bounce guard must not consult stale owner)", n)
	}
	if n := peer.requestCount(); n != 0 {
		t.Errorf("peer received %d requests, want 0 (one-hop terminal)", n)
	}
}

// TestForwardNoOwnerKeyCurrentError：错误码表行 3——无 owner 键（GetNodeOwner 缺键 ("",nil)，
// 节点全域离线）→ 不转发，现行 E_NODE_OFFLINE 文案不变。
func TestForwardNoOwnerKeyCurrentError(t *testing.T) {
	peer := &fakePeerAdmin{envelope: []byte(`{"ok":true}`)}
	srv := startFakePeer(t, peer)
	owners := &fakeOwners{owner: ""}
	s := newForwardingScheduler(t, owners, srv.URL)

	_, code, err := s.Dispatch(context.Background(), "node-x", "click", nil, 0, "t", "")
	if code != CodeNodeOffline {
		t.Fatalf("no owner key: got code %q, want %q", code, CodeNodeOffline)
	}
	if err == nil || !strings.Contains(err.Error(), "offline or unhealthy") {
		t.Errorf("no owner key: want current error wording, got %v", err)
	}
	if n := peer.requestCount(); n != 0 {
		t.Errorf("peer received %d requests, want 0", n)
	}
}

// TestForwardOwnerSelfCurrentError：错误码表行 4——owner==self（stale 自指：节点刚断，键未过期）
// → 不转发，现行错误。
func TestForwardOwnerSelfCurrentError(t *testing.T) {
	peer := &fakePeerAdmin{envelope: []byte(`{"ok":true}`)}
	srv := startFakePeer(t, peer)
	owners := &fakeOwners{owner: "replica-self"}
	s := newForwardingScheduler(t, owners, srv.URL)

	_, code, err := s.Dispatch(context.Background(), "node-x", "click", nil, 0, "t", "")
	if code != CodeNodeOffline || err == nil || !strings.Contains(err.Error(), "offline or unhealthy") {
		t.Fatalf("owner==self: got code %q err %v, want current E_NODE_OFFLINE path", code, err)
	}
	if n := peer.requestCount(); n != 0 {
		t.Errorf("peer received %d requests, want 0", n)
	}
}

// TestForwardOwnerUnknownPeerCurrentError：错误码表行 5——owner 是未知副本（不在 peers 表，
// 配置缺口）→ 不转发 + Warn，现行错误。
func TestForwardOwnerUnknownPeerCurrentError(t *testing.T) {
	peer := &fakePeerAdmin{envelope: []byte(`{"ok":true}`)}
	srv := startFakePeer(t, peer)
	owners := &fakeOwners{owner: "replica-ghost"}
	s := newForwardingScheduler(t, owners, srv.URL)

	_, code, err := s.Dispatch(context.Background(), "node-x", "click", nil, 0, "t", "")
	if code != CodeNodeOffline || err == nil || !strings.Contains(err.Error(), "offline or unhealthy") {
		t.Fatalf("owner not in peers: got code %q err %v, want current E_NODE_OFFLINE path", code, err)
	}
	if n := peer.requestCount(); n != 0 {
		t.Errorf("peer received %d requests, want 0", n)
	}
}

// TestForwardOwnerReadErrorCurrentError：错误码表行 10——GetNodeOwner 读错误（Redis 故障）视同
// 无 owner（§7 读降级原则）→ 不转发 + Warn，现行错误。
func TestForwardOwnerReadErrorCurrentError(t *testing.T) {
	peer := &fakePeerAdmin{envelope: []byte(`{"ok":true}`)}
	srv := startFakePeer(t, peer)
	owners := &fakeOwners{err: errors.New("redis: connection refused")}
	s := newForwardingScheduler(t, owners, srv.URL)

	_, code, err := s.Dispatch(context.Background(), "node-x", "click", nil, 0, "t", "")
	if code != CodeNodeOffline || err == nil || !strings.Contains(err.Error(), "offline or unhealthy") {
		t.Fatalf("owner read error: got code %q err %v, want fail-open to current path", code, err)
	}
	if n := peer.requestCount(); n != 0 {
		t.Errorf("peer received %d requests, want 0", n)
	}
}

// TestForwardPeerErrorIsTerminal：错误码表行 7——转发网络错/非 2xx 一跳终态：不重试不改投，
// E_NODE_OFFLINE 且 err 含 peer 端点与原因。
func TestForwardPeerErrorIsTerminal(t *testing.T) {
	peer := &fakePeerAdmin{failWith: errors.New("peer draining")}
	srv := startFakePeer(t, peer)
	owners := &fakeOwners{owner: "replica-peer"}
	s := newForwardingScheduler(t, owners, srv.URL)

	resp, code, err := s.Dispatch(context.Background(), "node-x", "click", nil, 0, "t", "")
	if code != CodeNodeOffline || resp != nil {
		t.Fatalf("peer failure: got code %q resp %v, want %q + nil resp", code, resp, CodeNodeOffline)
	}
	if err == nil || !strings.Contains(err.Error(), srv.URL) || !strings.Contains(err.Error(), "replica-peer") {
		t.Errorf("peer failure err should name peer replica and endpoint, got: %v", err)
	}
	if n := peer.requestCount(); n != 1 {
		t.Errorf("peer received %d requests, want exactly 1 (no retry, no re-route)", n)
	}
}

// TestNoForwarderCurrentPath：错误码表行 2（红线）——未装配 forwarder（未配置 AURA_REPLICA_PEERS/
// Redis 的单副本形态）→ Ready 失败走现行 E_NODE_OFFLINE，行为零变化。
func TestNoForwarderCurrentPath(t *testing.T) {
	s := NewScheduler(registry.NewRegistry(nil), nil, 0, nil)
	resp, code, err := s.Dispatch(context.Background(), "node-x", "click", nil, 0, "t", "")
	if code != CodeNodeOffline || resp != nil {
		t.Fatalf("no forwarder: got code %q resp %v, want current %q path", code, resp, CodeNodeOffline)
	}
	if err == nil || !strings.Contains(err.Error(), "offline or unhealthy") {
		t.Errorf("no forwarder: want current error wording, got %v", err)
	}
}

// TestParseReplicaPeers：AURA_REPLICA_PEERS 解析（空白容忍/非法条目报错/空表报错）与 NewForwarder
// 的 self 过滤（全量对称表含 self，转发目标不含自身）。
func TestParseReplicaPeers(t *testing.T) {
	peers, err := ParseReplicaPeers(" replica-1 = https://192.168.22.240:18080 , replica-2=https://192.168.22.225:18080 ")
	if err != nil {
		t.Fatalf("parse valid peers: %v", err)
	}
	if len(peers) != 2 || peers["replica-1"] != "https://192.168.22.240:18080" || peers["replica-2"] != "https://192.168.22.225:18080" {
		t.Errorf("parsed peers mismatch: %v", peers)
	}

	if _, err := ParseReplicaPeers("replica-1"); err == nil {
		t.Errorf("entry without '=' should be rejected")
	}
	if _, err := ParseReplicaPeers("=https://a"); err == nil {
		t.Errorf("entry with empty id should be rejected")
	}
	if _, err := ParseReplicaPeers(" , "); err == nil {
		t.Errorf("empty table should be rejected")
	}

	f := NewForwarder("replica-1", peers, &fakeOwners{}, http.DefaultClient, "tok", time.Minute)
	if _, ok := f.peers["replica-1"]; ok {
		t.Errorf("NewForwarder must filter self out of forwarding targets")
	}
	if pt, ok := f.peers["replica-2"]; !ok || pt.endpoint != "https://192.168.22.225:18080" {
		t.Errorf("NewForwarder peer targets mismatch: %+v", f.peers)
	}
}

// TestDispatchCarriesTraceIDToNode：GAP-4 controller leg（D11）——本地路径 ToolRequest 构造携带
// trace_id 穿透至节点侧帧（转发路径经 DispatchToolRequest.trace_id 到 owner 后走同一构造点）。
// fake writer 消费会话下行队列断言帧内容并回填响应。
func TestDispatchCarriesTraceIDToNode(t *testing.T) {
	reg := registry.NewRegistry(nil)
	s := NewScheduler(reg, nil, 0, nil)

	sess := registry.NewSession("node-t", "windows", []string{"click"}, "", 4)
	reg.Add(sess)

	writerCtx, cancelWriter := context.WithCancel(context.Background())
	defer cancelWriter()
	got := make(chan *aurav1.ToolRequest, 1)
	go sess.RunWriter(writerCtx, func(frame *aurav1.ControllerToNode) error {
		if tr := frame.GetToolRequest(); tr != nil {
			got <- tr
			sess.DeliverResponse(&aurav1.ToolResponse{TaskId: tr.GetTaskId(), JsonEnvelope: []byte(`{"ok":true}`)})
		}
		return nil
	})

	resp, code, err := s.Dispatch(context.Background(), "node-t", "click", nil, 0, "alice", "tr-gap4")
	if err != nil || code != "" || resp == nil {
		t.Fatalf("local dispatch: resp=%v code=%q err=%v", resp, code, err)
	}
	select {
	case tr := <-got:
		if tr.GetTraceId() != "tr-gap4" {
			t.Errorf("ToolRequest.trace_id = %q, want %q (GAP-4 controller leg)", tr.GetTraceId(), "tr-gap4")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("node-side frame not observed")
	}
}
