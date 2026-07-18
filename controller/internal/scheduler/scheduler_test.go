package scheduler

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/alicebob/miniredis/v2"

	aurav1 "github.com/aura/controller/gen/aura/v1"
	"github.com/aura/controller/gen/aura/v1/aurav1connect"
	"github.com/aura/controller/internal/registry"
	"github.com/aura/controller/internal/store"
)

// TestErrorCodesMatchProtoEnum 把「传输层错误码字面量 ↔ proto ErrorCode 枚举」契约耦合前移到测试期：
// 断言四个错误码均为生成枚举 aurav1.ErrorCode_value 的合法成员名，且与对应枚举成员 .String() 逐字对齐。
// proto 改名/删除枚举成员时此测试失败（与 scheduler.go 的 .String() 派生编译期绑定互为双保险，G-2）。
func TestErrorCodesMatchProtoEnum(t *testing.T) {
	cases := map[string]aurav1.ErrorCode{
		CodeBusy:        aurav1.ErrorCode_E_BUSY,
		CodeNodeOffline: aurav1.ErrorCode_E_NODE_OFFLINE,
		CodeTimeout:     aurav1.ErrorCode_E_TIMEOUT,
		CodeInternal:    aurav1.ErrorCode_E_INTERNAL,
		CodeUnsupported: aurav1.ErrorCode_E_UNSUPPORTED,
	}
	for code, want := range cases {
		// 字面量必须是生成枚举名集合（ErrorCode_value 的键集）中的成员。
		if _, ok := aurav1.ErrorCode_value[code]; !ok {
			t.Errorf("transport error code %q is not a member of aurav1.ErrorCode_name value set", code)
		}
		// 字面量必须与对应枚举成员名逐字相等（防静默漂移）。
		if code != want.String() {
			t.Errorf("transport error code %q != proto enum name %q", code, want.String())
		}
	}
}

// TestDrainRejectsNewDispatch：排水闸门置位后 Dispatch 必须以 E_BUSY 快速拒绝新任务（不计数、不入队、
// 不下发），验证 admit 的「draining 检查 + inflight.Add」原子闸门——Drain 优雅关停语义的核心不变量。
func TestDrainRejectsNewDispatch(t *testing.T) {
	s := NewScheduler(registry.NewRegistry(nil), nil, 0, nil)

	// 触发排水：inflight 为 0，Drain 立即完成并置 draining=true（Wait 锁外、即刻返回）。
	s.Drain(context.Background())

	resp, code, err := s.Dispatch(context.Background(), "node-x", "click", nil, 0, "test", "")
	if code != CodeBusy {
		t.Fatalf("draining dispatch: got code %q, want %q", code, CodeBusy)
	}
	if resp != nil {
		t.Errorf("draining dispatch: expected nil response, got %+v", resp)
	}
	if err == nil {
		t.Errorf("draining dispatch: expected non-nil error")
	}
}

// TestQueueDepth：QueueDepth 读 per-node 队列缓冲通道长度——未知节点返回 0（nil chan len），已建队列返回
// 其当前深度（对抗 G，console 执行墙积压展示）。白盒直填 s.queues（不起 worker，避免消费扰动深度快照）。
func TestQueueDepth(t *testing.T) {
	s := NewScheduler(registry.NewRegistry(nil), nil, 0, nil)

	// 未知节点：无队列 → 0。
	if got := s.QueueDepth("unknown-node"); got != 0 {
		t.Errorf("QueueDepth(unknown) = %d, want 0", got)
	}

	// 白盒注入一个含 2 个待执行 job 的队列（不 go worker，深度不被消费扰动）。
	ch := make(chan job, maxQueue)
	ch <- job{}
	ch <- job{}
	s.mu.Lock()
	s.queues["node-a"] = ch
	s.mu.Unlock()

	if got := s.QueueDepth("node-a"); got != 2 {
		t.Errorf("QueueDepth(node-a) = %d, want 2", got)
	}
}

// —— T6 execute 段 per-node 分布式锁（ha-contract §1.3）————————————————————————————————

// fakeLocker 是 NodeLocker 的可控注入实现。execute 白盒测试为同步单 goroutine 调用，无需加锁。
type fakeLocker struct {
	held     bool  // true=锁被他副本持有（Acquire 恒 ok=false）
	err      error // 非 nil=Acquire 返回错误（fail-open 路径）
	acquires int
	releases int
	lastTTL  time.Duration
	lastID   string
}

func (f *fakeLocker) AcquireNodeLock(_ context.Context, _, replicaID string, ttl time.Duration) (bool, error) {
	f.acquires++
	f.lastTTL = ttl
	f.lastID = replicaID
	if f.err != nil {
		return false, f.err
	}
	return !f.held, nil
}

func (f *fakeLocker) ReleaseNodeLock(_ context.Context, _, replicaID string) error {
	f.releases++
	f.lastID = replicaID
	return nil
}

// newLockedExecuteEnv 构造带就绪会话（node-l）与注入 locker（replica-1）的纯内存调度器。
// execute 白盒直调（不经 Dispatch 全链）：锁语义在 execute 段，队列/审计面不掺噪。
func newLockedExecuteEnv(t *testing.T, fl *fakeLocker) *Scheduler {
	t.Helper()
	reg := registry.NewRegistry(nil)
	s := NewScheduler(reg, nil, 0, nil)
	s.SetNodeLocker(fl, "replica-1")
	reg.Add(registry.NewSession("node-l", "win", nil, "", 4)) // 新会话 lastSeen=now，Ready 必过
	return s
}

// TestExecuteLockHeldByPeerFailsBusy：他副本持锁（脑裂窗口）→ 有界短重试（1+nodeLockRetries 次
// Acquire）后该 job 以 E_BUSY 失败——错误可见非吞没（convergence C3），且未获锁不得调 Release
//（误删他副本锁即破坏其串行性保护）。
func TestExecuteLockHeldByPeerFailsBusy(t *testing.T) {
	fl := &fakeLocker{held: true}
	s := newLockedExecuteEnv(t, fl)

	j := job{
		ctx:      context.Background(),
		req:      &aurav1.ToolRequest{TaskId: "t-lock-busy", Tool: "click"},
		resultCh: make(chan result, 1),
	}
	s.execute("node-l", j)

	r := <-j.resultCh
	if r.code != CodeBusy {
		t.Fatalf("locked execute: got code %q, want %q (E_BUSY)", r.code, CodeBusy)
	}
	if r.err == nil {
		t.Error("locked execute: expected visible non-nil error")
	}
	if want := 1 + nodeLockRetries; fl.acquires != want {
		t.Errorf("acquire attempts = %d, want %d (initial + bounded retries)", fl.acquires, want)
	}
	if fl.releases != 0 {
		t.Errorf("releases = %d, want 0 (must not release a lock we never held)", fl.releases)
	}
}

// TestExecuteLockAcquireReleaseAndTTL：获锁成功路径——执行毕（含错误路径：job ctx 已取消致
// sess.Dispatch 即刻失败）defer Release 必达；锁 TTL 按契约公式 deadline+grace+slack；锁值携带
// SetNodeLocker 注入的 replicaID。
func TestExecuteLockAcquireReleaseAndTTL(t *testing.T) {
	fl := &fakeLocker{}
	s := newLockedExecuteEnv(t, fl)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 预取消：Dispatch 立即失败，覆盖「错误路径也 Release」
	j := job{
		ctx:      ctx,
		req:      &aurav1.ToolRequest{TaskId: "t-lock-ttl", Tool: "click", DeadlineMs: 100},
		resultCh: make(chan result, 1),
	}
	s.execute("node-l", j)

	r := <-j.resultCh
	if r.code == "" || r.code == CodeBusy {
		t.Fatalf("canceled-ctx execute: got code %q, want transport error other than E_BUSY", r.code)
	}
	if fl.acquires != 1 || fl.releases != 1 {
		t.Errorf("acquires/releases = %d/%d, want 1/1 (release on error path too)", fl.acquires, fl.releases)
	}
	if want := 100*time.Millisecond + graceTimeout + nodeLockSlack; fl.lastTTL != want {
		t.Errorf("lock ttl = %v, want %v (deadline+grace+slack)", fl.lastTTL, want)
	}
	if fl.lastID != "replica-1" {
		t.Errorf("lock replica id = %q, want %q", fl.lastID, "replica-1")
	}
}

// TestExecuteLockStoreErrorFailsOpen：Redis 故障（Acquire 返回 error 而非被占）→ fail-open 放行执行
// 不重试——锁是脑裂窗口串行性兜底而非安全性质，fail-closed 会把 Redis 抖动放大为全节点 dispatch
// 停摆（ha-contract §7#5 同源）；降级窗口串行性退回进程内单 worker。
func TestExecuteLockStoreErrorFailsOpen(t *testing.T) {
	fl := &fakeLocker{err: errors.New("redis down")}
	s := newLockedExecuteEnv(t, fl)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	j := job{
		ctx:      ctx,
		req:      &aurav1.ToolRequest{TaskId: "t-lock-open", Tool: "click"},
		resultCh: make(chan result, 1),
	}
	s.execute("node-l", j)

	r := <-j.resultCh
	if r.code == CodeBusy {
		t.Fatalf("fail-open execute: got E_BUSY, want execution to proceed past the lock")
	}
	if fl.acquires != 1 {
		t.Errorf("acquire attempts = %d, want 1 (store error must not retry)", fl.acquires)
	}
}

// TestNodeLockPrimitives：store.RedisStore 锁原语桩语义修正（ha-contract §1.3，miniredis 真命令面）
// ——值写入 replicaID 而非恒 1；Release 由无条件 DEL 改 compare-and-del（他副本迟到的 Release 不得
// 误删本副本已获的锁）。
func TestNodeLockPrimitives(t *testing.T) {
	mr := miniredis.RunT(t)
	rs, err := store.NewRedisStore(context.Background(), mr.Addr())
	if err != nil {
		t.Fatalf("NewRedisStore(miniredis): %v", err)
	}
	t.Cleanup(func() { _ = rs.Close() })
	ctx := context.Background()

	ok, err := rs.AcquireNodeLock(ctx, "node-a", "replica-1", 5*time.Second)
	if err != nil || !ok {
		t.Fatalf("AcquireNodeLock(free) = %v,%v, want true,nil", ok, err)
	}
	if v, _ := mr.Get("node:lock:node-a"); v != "replica-1" {
		t.Errorf("lock value = %q, want %q (stub semantics fixed: value carries holder id)", v, "replica-1")
	}

	// 他副本争锁：SET NX 拒绝。
	if ok, err := rs.AcquireNodeLock(ctx, "node-a", "replica-2", 5*time.Second); err != nil || ok {
		t.Fatalf("AcquireNodeLock(held by replica-1) = %v,%v, want false,nil", ok, err)
	}
	// 他副本迟到的 Release：compare-and-del 不匹配，不得误删 replica-1 的锁。
	if err := rs.ReleaseNodeLock(ctx, "node-a", "replica-2"); err != nil {
		t.Fatalf("ReleaseNodeLock(mismatched): %v", err)
	}
	if !mr.Exists("node:lock:node-a") {
		t.Fatal("mismatched release must not delete another replica's lock")
	}
	// 持有者 Release：匹配删除，锁归还可用。
	if err := rs.ReleaseNodeLock(ctx, "node-a", "replica-1"); err != nil {
		t.Fatalf("ReleaseNodeLock(holder): %v", err)
	}
	if mr.Exists("node:lock:node-a") {
		t.Fatal("holder release must delete the lock")
	}
	if ok, _ := rs.AcquireNodeLock(ctx, "node-a", "replica-2", 5*time.Second); !ok {
		t.Error("lock must be acquirable after holder release")
	}
}

// —— T11 console 读旁路（读写分离，SC-3① 单测化）————————————————————————————————————————

// startEchoWriter 起 fake writer 消费会话下行帧并以固定 envelope 即时回填（节点侧替身：node 每
// 请求独立执行、本就并发，即回等价于「读请求不被节点侧串行化」的最简模型）。t.Cleanup 收尾。
func startEchoWriter(t *testing.T, sess *registry.NodeSession, env []byte) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go sess.RunWriter(ctx, func(frame *aurav1.ControllerToNode) error {
		if tr := frame.GetToolRequest(); tr != nil {
			sess.DeliverResponse(&aurav1.ToolResponse{TaskId: tr.GetTaskId(), JsonEnvelope: env})
		}
		return nil
	})
}

// startSelectiveWriter 起 fake writer：click 帧只计数不回填（慢工具占住 worker 的替身），
// screenshot 帧即时回填 env。返回 click 收帧信号 chan（占住时刻的同步点）。
func startSelectiveWriter(t *testing.T, sess *registry.NodeSession, env []byte) <-chan struct{} {
	t.Helper()
	clicks := make(chan struct{}, maxQueue*2)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go sess.RunWriter(ctx, func(frame *aurav1.ControllerToNode) error {
		tr := frame.GetToolRequest()
		if tr == nil {
			return nil
		}
		if tr.GetTool() == "screenshot" {
			sess.DeliverResponse(&aurav1.ToolResponse{TaskId: tr.GetTaskId(), JsonEnvelope: env})
			return nil
		}
		clicks <- struct{}{}
		return nil
	})
	return clicks
}

// waitForQueueDepth 轮询等待 per-node 队列达到指定深度（异步入队的同步点）。
func waitForQueueDepth(t *testing.T, s *Scheduler, nodeID string, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if s.QueueDepth(nodeID) == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("queue depth for %s did not reach %d (got %d)", nodeID, want, s.QueueDepth(nodeID))
}

// TestDispatchReadOnlyRejectsNonWhitelistedTool：白名单外工具拒绝（E_UNSUPPORTED），且校验前置于
// Ready——offline 节点上白名单外工具得 E_UNSUPPORTED（非 E_NODE_OFFLINE）证明顺序；白名单内工具
// 在 offline 节点走正常离线路径（对照）。
func TestDispatchReadOnlyRejectsNonWhitelistedTool(t *testing.T) {
	s := NewScheduler(registry.NewRegistry(nil), nil, 0, nil)

	resp, code, err := s.DispatchReadOnly(context.Background(), "ghost", "click", nil, 0, "console")
	if code != CodeUnsupported {
		t.Fatalf("non-whitelisted tool: got code %q, want %q", code, CodeUnsupported)
	}
	if resp != nil || err == nil {
		t.Errorf("non-whitelisted tool: want nil resp + visible err, got resp=%v err=%v", resp, err)
	}

	if _, code, _ := s.DispatchReadOnly(context.Background(), "ghost", "screenshot", nil, 0, "console"); code != CodeNodeOffline {
		t.Errorf("whitelisted tool on offline node: got code %q, want %q", code, CodeNodeOffline)
	}
}

// TestDispatchReadOnlyExemptFromLease：租约豁免——录制租约期 DispatchReadOnly 放行取帧；对照非只读
// 同 node 写 dispatch 仍被 checkLease 拒 E_BUSY（既有门控语义不变）；StopTrace 后写通道恢复。
func TestDispatchReadOnlyExemptFromLease(t *testing.T) {
	reg := registry.NewRegistry(nil)
	s := NewScheduler(reg, nil, 0, nil)
	sess := registry.NewSession("node-r", "android13", nil, "", 8)
	reg.Add(sess)
	env := []byte(`{"ok":true,"data":{"mime":"image/webp","image_base64":"aGk="}}`)
	startEchoWriter(t, sess, env)

	traceID, err := s.StartTrace("node-r", "recorder")
	if err != nil {
		t.Fatalf("StartTrace: %v", err)
	}

	// 对照：非持有者（空 trace_id）写 dispatch 被租约拒——E_BUSY 语义零变化。
	if _, code, _ := s.Dispatch(context.Background(), "node-r", "click", nil, 0, "other", ""); code != CodeBusy {
		t.Fatalf("non-holder write during lease: got code %q, want %q (existing gate unchanged)", code, CodeBusy)
	}

	// 租约期只读放行（豁免即 UX 目标：录制中仍可看屏）。
	resp, code, rerr := s.DispatchReadOnly(context.Background(), "node-r", "screenshot", nil, 0, "console")
	if rerr != nil || code != "" {
		t.Fatalf("read during lease: code=%q err=%v, want exempt pass", code, rerr)
	}
	if !bytes.Equal(resp.GetJsonEnvelope(), env) {
		t.Errorf("read during lease: envelope mismatch: %s", resp.GetJsonEnvelope())
	}

	// 收尾对照：释放租约后写通道恢复。
	if !s.StopTrace(traceID) {
		t.Fatal("StopTrace: want true for active lease")
	}
	if _, code, err := s.Dispatch(context.Background(), "node-r", "click", nil, 0, "other", ""); code != "" || err != nil {
		t.Errorf("write after StopTrace: code=%q err=%v, want pass", code, err)
	}
}

// TestDispatchReadOnlyBypassesBacklog：SC-3① 验收核心——慢工具占满 worker 且队列灌满（深度=
// maxQueue）时，DispatchReadOnly 仍即时返回成功（时延对照：积压写在 60s 执行窗内阻塞）；读全程
// 不入队不出队，任务队列深度不受读流量影响。
func TestDispatchReadOnlyBypassesBacklog(t *testing.T) {
	reg := registry.NewRegistry(nil)
	s := NewScheduler(reg, nil, 0, nil)
	sess := registry.NewSession("node-b", "windows", nil, "", maxQueue*2)
	reg.Add(sess)
	env := []byte(`{"ok":true,"data":{"mime":"image/webp"}}`)
	clicks := startSelectiveWriter(t, sess, env)

	wctx, wcancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	launchClick := func() {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, _, _ = s.dispatch(wctx, "node-b", "click", nil, 60_000, "agent", "")
		}()
	}
	t.Cleanup(func() { wcancel(); wg.Wait() })

	// 先占住 worker：首个慢写抵达节点侧（收帧不回填，阻塞在 execute 的 sess.Dispatch）。
	launchClick()
	select {
	case <-clicks:
	case <-time.After(5 * time.Second):
		t.Fatal("first slow write never reached the node session")
	}
	// 再灌满队列：worker 已阻塞，maxQueue 个写全部排队。
	for i := 0; i < maxQueue; i++ {
		launchClick()
	}
	waitForQueueDepth(t, s, "node-b", maxQueue)

	start := time.Now()
	resp, code, rerr := s.DispatchReadOnly(context.Background(), "node-b", "screenshot", nil, 0, "console")
	elapsed := time.Since(start)
	if rerr != nil || code != "" || !bytes.Equal(resp.GetJsonEnvelope(), env) {
		t.Fatalf("read under full backlog: resp=%v code=%q err=%v", resp, code, rerr)
	}
	if elapsed >= 2*time.Second {
		t.Fatalf("read latency %v under full backlog; want instant bypass, not queue wait", elapsed)
	}
	if got := s.QueueDepth("node-b"); got != maxQueue {
		t.Errorf("queue depth after read = %d, want %d unchanged (reads must not touch the queue)", got, maxQueue)
	}
}

// TestDispatchReadOnlyDoesNotEnterQueue：N 并发读全部成功且不建 per-node 队列、深度恒 0——
// 读流量不占队列容量（NodeSession pending-map 多路复用天然并发）。
func TestDispatchReadOnlyDoesNotEnterQueue(t *testing.T) {
	reg := registry.NewRegistry(nil)
	s := NewScheduler(reg, nil, 0, nil)
	sess := registry.NewSession("node-p", "android13", nil, "", 64)
	reg.Add(sess)
	env := []byte(`{"ok":true}`)
	startEchoWriter(t, sess, env)

	const readers = 8
	errs := make(chan error, readers)
	var wg sync.WaitGroup
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, code, err := s.DispatchReadOnly(context.Background(), "node-p", "screenshot", nil, 0, "console")
			if err != nil || code != "" || !bytes.Equal(resp.GetJsonEnvelope(), env) {
				errs <- fmt.Errorf("concurrent read: resp=%v code=%q err=%v", resp, code, err)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}

	if got := s.QueueDepth("node-p"); got != 0 {
		t.Errorf("queue depth after %d concurrent reads = %d, want 0", readers, got)
	}
	s.mu.Lock()
	_, hasQueue := s.queues["node-p"]
	s.mu.Unlock()
	if hasQueue {
		t.Error("read-only dispatch must not create a per-node queue")
	}
}

// fakePeerConsole 以生成 handler 形态扮演 owner 副本的 ConsoleService：记录 ReadNodeScreen 请求
// 与 header，回固定 envelope（读旁路转发腿的 owner 端替身，fakePeerAdmin 同款手法）。
type fakePeerConsole struct {
	aurav1connect.UnimplementedConsoleServiceHandler

	mu       sync.Mutex
	requests []*aurav1.ReadNodeScreenRequest
	headers  []http.Header
	envelope []byte
}

func (f *fakePeerConsole) ReadNodeScreen(_ context.Context, req *connect.Request[aurav1.ReadNodeScreenRequest]) (*connect.Response[aurav1.ReadNodeScreenResponse], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, req.Msg)
	f.headers = append(f.headers, req.Header().Clone())
	return connect.NewResponse(&aurav1.ReadNodeScreenResponse{JsonEnvelope: f.envelope}), nil
}

func (f *fakePeerConsole) requestCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

// TestDispatchReadOnlyForwardsToOwnerPeer：读旁路转发腿——Ready 失败 + owner=peer 时经
// ConsoleService.ReadNodeScreen 转投 owner（owner 侧落 DispatchReadOnly，读语义端到端），envelope
// 逐字节透传、四字段原样、hop+bearer 随请求；入站已带转发标记不查 owner 不二次转发（m-1 同纪律）。
func TestDispatchReadOnlyForwardsToOwnerPeer(t *testing.T) {
	env := []byte(`{"ok":true,"data":{"mime":"image/webp","image_base64":"aGk="}}`)
	peer := &fakePeerConsole{envelope: env}
	mux := http.NewServeMux()
	path, handler := aurav1connect.NewConsoleServiceHandler(peer)
	mux.Handle(path, handler)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	owners := &fakeOwners{owner: "replica-peer"}
	s := newForwardingScheduler(t, owners, srv.URL)

	args := []byte(`{"display":0}`)
	resp, code, err := s.DispatchReadOnly(context.Background(), "node-x", "screenshot", args, 3000, "console-operate")
	if err != nil || code != "" {
		t.Fatalf("forwarded read: code=%q err=%v", code, err)
	}
	if !bytes.Equal(resp.GetJsonEnvelope(), env) {
		t.Errorf("forwarded read envelope not byte-identical: %s", resp.GetJsonEnvelope())
	}
	if n := peer.requestCount(); n != 1 {
		t.Fatalf("peer received %d read requests, want exactly 1", n)
	}
	peer.mu.Lock()
	req, hdr := peer.requests[0], peer.headers[0]
	peer.mu.Unlock()
	if req.GetNodeId() != "node-x" || !bytes.Equal(req.GetJsonArgs(), args) || req.GetDeadlineMs() != 3000 || req.GetWho() != "console-operate" {
		t.Errorf("forwarded read fields not copied verbatim: %+v", req)
	}
	if got := hdr.Get(ForwardedByHeader); got != "replica-self" {
		t.Errorf("hop header %s = %q, want %q", ForwardedByHeader, got, "replica-self")
	}
	if got := hdr.Get("Authorization"); got != "Bearer test-token" {
		t.Errorf("forwarded Authorization = %q, want shared bearer", got)
	}

	// 防环：入站转发标记下 Ready 失败即终态——不查 owner、不再转发。
	before := owners.callCount()
	mctx := context.WithValue(context.Background(), inboundForwardKey{}, true)
	if _, code, err := s.DispatchReadOnly(mctx, "node-x", "screenshot", nil, 0, "console"); code != CodeNodeOffline || err == nil {
		t.Fatalf("inbound-forwarded read on non-ready node: code=%q err=%v, want %q terminal", code, err, CodeNodeOffline)
	}
	if owners.callCount() != before {
		t.Error("bounce guard: owner table must not be consulted on inbound-forwarded read")
	}
	if peer.requestCount() != 1 {
		t.Error("bounce guard: no second hop expected")
	}
}
