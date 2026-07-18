package orchestrator

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	aurav1 "github.com/aura/controller/gen/aura/v1"
	"github.com/aura/controller/internal/scheduler"
	"github.com/aura/controller/internal/store"
)

// progResp 是 fakeDispatcher 对某节点的预置回执。
type progResp struct {
	resp   *aurav1.ToolResponse
	taskID string
	code   string
}

// fakeDispatcher 实现 dispatcher 窄接口：按节点回预置结果，并统计每节点调用次数与最大并发度。
// barrier>0 时启用「全到齐」栅栏：每次调用入栅计数，直至并发数达 barrier 才放行——串行 fan-out 会
// 卡死在栅栏（2s 超时兜底避免死锁，令并发断言失败而非挂起），以此确定性地证明并发下发。
type fakeDispatcher struct {
	mu        sync.Mutex
	responses map[string]progResp
	calls     map[string]int
	active    int
	maxActive int
	barrier   int
	arrived   chan struct{}
}

func newFakeDispatcher() *fakeDispatcher {
	return &fakeDispatcher{responses: map[string]progResp{}, calls: map[string]int{}, arrived: make(chan struct{})}
}

func (f *fakeDispatcher) program(nodeID string, r progResp) {
	f.responses[nodeID] = r
}

func (f *fakeDispatcher) DispatchTracked(_ context.Context, nodeID, _ string, _ []byte, _ int64, _, _ string) (*aurav1.ToolResponse, string, string, error) {
	f.mu.Lock()
	f.calls[nodeID]++
	f.active++
	if f.active > f.maxActive {
		f.maxActive = f.active
	}
	reached := f.barrier > 0 && f.active == f.barrier
	f.mu.Unlock()

	if f.barrier > 0 {
		if reached {
			close(f.arrived) // 最后一个到齐者放行全体
		}
		select {
		case <-f.arrived:
		case <-time.After(2 * time.Second): // 串行会卡死于此：超时放行让 maxActive 断言失败而非挂起
		}
	}

	f.mu.Lock()
	f.active--
	f.mu.Unlock()

	r := f.responses[nodeID]
	return r.resp, r.taskID, r.code, nil
}

// fakeStore 实现 taskStore 窄接口，记录编排落表与关联调用供断言。
type fakeStore struct {
	created     *store.OrchestrationRecord
	updStatus   string
	updPassed   int32
	updFailed   int32
	updCalled   bool
	assocID     string
	assocTasks  []string
	assocCalled bool
}

func (f *fakeStore) CreateOrchestration(_ context.Context, o store.OrchestrationRecord) error {
	f.created = &o
	return nil
}

func (f *fakeStore) UpdateOrchestrationResult(_ context.Context, _, status string, passed, failed int32) error {
	f.updStatus, f.updPassed, f.updFailed, f.updCalled = status, passed, failed, true
	return nil
}

func (f *fakeStore) SetTaskOrchestration(_ context.Context, orchestrationID string, taskIDs []string) error {
	f.assocID, f.assocTasks, f.assocCalled = orchestrationID, taskIDs, true
	return nil
}

// fakeLister 实现 nodeLister 窄接口，回预置节点快照供 EnvGroup 平台过滤。
type fakeLister struct {
	nodes []*aurav1.NodeInfo
}

func (f *fakeLister) List() []*aurav1.NodeInfo { return f.nodes }

func okResp(env string) *aurav1.ToolResponse {
	return &aurav1.ToolResponse{JsonEnvelope: []byte(env)}
}

// findEnv 在 PerEnv 中按 nodeID 取结果（顺序无关断言）。
func findEnv(res Result, nodeID string) (EnvResult, bool) {
	for _, e := range res.PerEnv {
		if e.NodeID == nodeID {
			return e, true
		}
	}
	return EnvResult{}, false
}

// TestRun_AllPass_ConcurrentFanout 验证全 pass：status=done，并发 fan-out（maxActive 达目标数，证并发不破
// 串行——目标为不同 node，各仅一次），编排落表 + orchestration_id 关联全部任务。
func TestRun_AllPass_ConcurrentFanout(t *testing.T) {
	fd := newFakeDispatcher()
	fd.barrier = 3
	fd.program("n1", progResp{resp: okResp("e1"), taskID: "t1"})
	fd.program("n2", progResp{resp: okResp("e2"), taskID: "t2"})
	fd.program("n3", progResp{resp: okResp("e3"), taskID: "t3"})
	fs := &fakeStore{}
	o := &Orchestrator{sched: fd, store: fs}

	res, err := o.Run(context.Background(), OrchestrationSpec{Tool: "click", NodeIDs: []string{"n1", "n2", "n3"}, Who: "tester"})
	if err != nil {
		t.Fatalf("Run: unexpected err %v", err)
	}
	if res.Status != StatusDone || res.Total != 3 || res.Passed != 3 || res.Failed != 0 {
		t.Fatalf("聚合错误: status=%s total=%d passed=%d failed=%d", res.Status, res.Total, res.Passed, res.Failed)
	}
	if fd.maxActive != 3 {
		t.Fatalf("并发度=%d, want 3（fan-out 应并发下发全部目标）", fd.maxActive)
	}
	for _, n := range []string{"n1", "n2", "n3"} {
		if fd.calls[n] != 1 {
			t.Fatalf("节点 %s 下发 %d 次, want 1（每目标恰一次，per-node 不重复）", n, fd.calls[n])
		}
	}
	for _, e := range res.PerEnv {
		if e.Status != EnvSucceeded {
			t.Fatalf("节点 %s status=%s, want succeeded", e.NodeID, e.Status)
		}
	}
	// 落表：running 初态 total=3 + 终态 done(3,0) + 关联 3 个任务。
	if fs.created == nil || fs.created.Status != StatusRunning || fs.created.Total != 3 {
		t.Fatalf("CreateOrchestration 未正确落 running 初态: %+v", fs.created)
	}
	if !fs.updCalled || fs.updStatus != StatusDone || fs.updPassed != 3 || fs.updFailed != 0 {
		t.Fatalf("UpdateOrchestrationResult 未正确落终态: called=%v status=%s passed=%d failed=%d", fs.updCalled, fs.updStatus, fs.updPassed, fs.updFailed)
	}
	if !fs.assocCalled || fs.assocID != res.OrchestrationID || !sameSet(fs.assocTasks, []string{"t1", "t2", "t3"}) {
		t.Fatalf("SetTaskOrchestration 关联错误: called=%v id=%s tasks=%v", fs.assocCalled, fs.assocID, fs.assocTasks)
	}
}

// TestToolEnvelopeFailure 覆盖业务信封 ok 字段判失败的保守语义：仅显式 ok:false 翻转，
// 空 / 非法 JSON / 无 ok 字段一律非失败（不误伤传输成功但信封非标准的历史回执）。
func TestToolEnvelopeFailure(t *testing.T) {
	cases := []struct {
		name     string
		env      string
		wantCode string
		wantFail bool
	}{
		{"empty", "", "", false},
		{"invalid_json", "e1", "", false},
		{"ok_true", `{"ok":true,"data":{}}`, "", false},
		{"no_ok_field", `{"data":{}}`, "", false},
		{"ok_false_with_code", `{"ok":false,"error":{"code":"E_CAPTURE_FAILED","message":"x"}}`, "E_CAPTURE_FAILED", true},
		{"ok_false_no_code", `{"ok":false}`, "E_TOOL_FAILED", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, fail := toolEnvelopeFailure([]byte(c.env))
			if fail != c.wantFail || code != c.wantCode {
				t.Fatalf("toolEnvelopeFailure(%q) = (%q,%v), want (%q,%v)", c.env, code, fail, c.wantCode, c.wantFail)
			}
		})
	}
}

// TestRun_EnvelopeOkFalse_IntoFailedBucket 验证传输层送达（空 code）但业务信封 ok:false 的目标计入
// failed 桶——passed 反映工具在该环境真实成败而非仅送达（与用户直觉一致）；envelope 的 error.code
// 覆写空传输码，令 PerEnv 行携可读失败码。
func TestRun_EnvelopeOkFalse_IntoFailedBucket(t *testing.T) {
	fd := newFakeDispatcher()
	fd.program("n1", progResp{resp: okResp(`{"ok":true,"data":{}}`), taskID: "t1"})
	fd.program("n2", progResp{resp: okResp(`{"ok":false,"error":{"code":"E_CAPTURE_FAILED","message":"display index 0 out of range"}}`), taskID: "t2"})
	fs := &fakeStore{}
	o := &Orchestrator{sched: fd, store: fs}

	res, err := o.Run(context.Background(), OrchestrationSpec{Tool: "screenshot", NodeIDs: []string{"n1", "n2"}})
	if err != nil {
		t.Fatalf("Run: unexpected err %v", err)
	}
	if res.Status != StatusPartial || res.Passed != 1 || res.Failed != 1 {
		t.Fatalf("信封 ok:false 应计 failed: status=%s passed=%d failed=%d", res.Status, res.Passed, res.Failed)
	}
	if e, ok := findEnv(res, "n2"); !ok || e.Status != EnvFailed || e.Code != "E_CAPTURE_FAILED" {
		t.Fatalf("n2 应 failed 且携 envelope error.code: ok=%v env=%+v", ok, e)
	}
	if e, ok := findEnv(res, "n1"); !ok || e.Status != EnvSucceeded {
		t.Fatalf("n1 (ok:true) 应 succeeded: ok=%v env=%+v", ok, e)
	}
}

// TestRun_Partial_NoFuseBreak 验证部分失败：一个环境 fail 其余 pass → status=partial，失败环境结果保留
// （不熔断），有 task_id 的失败任务仍被关联。
func TestRun_Partial_NoFuseBreak(t *testing.T) {
	fd := newFakeDispatcher()
	fd.program("n1", progResp{resp: okResp("e1"), taskID: "t1"})
	fd.program("n2", progResp{code: scheduler.CodeInternal, taskID: "t2"}) // execute 期失败，task 已建
	fd.program("n3", progResp{resp: okResp("e3"), taskID: "t3"})
	fs := &fakeStore{}
	o := &Orchestrator{sched: fd, store: fs}

	res, err := o.Run(context.Background(), OrchestrationSpec{Tool: "click", NodeIDs: []string{"n1", "n2", "n3"}})
	if err != nil {
		t.Fatalf("Run: unexpected err %v", err)
	}
	if res.Status != StatusPartial || res.Passed != 2 || res.Failed != 1 {
		t.Fatalf("partial 聚合错误: status=%s passed=%d failed=%d", res.Status, res.Passed, res.Failed)
	}
	// 失败环境结果保留（不熔断）：n2 在 PerEnv 中且状态 failed；n1/n3 succeeded 未被丢弃。
	if e, ok := findEnv(res, "n2"); !ok || e.Status != EnvFailed {
		t.Fatalf("失败环境 n2 未保留或状态错: ok=%v env=%+v", ok, e)
	}
	if e, ok := findEnv(res, "n1"); !ok || e.Status != EnvSucceeded {
		t.Fatalf("成功环境 n1 应保留 succeeded: ok=%v env=%+v", ok, e)
	}
	if !fs.assocCalled || !sameSet(fs.assocTasks, []string{"t1", "t2", "t3"}) {
		t.Fatalf("关联任务错误（含失败但有 task_id 的 t2）: %v", fs.assocTasks)
	}
}

// TestRun_Timeout_IntoFailedBucket 验证 dispatch 超时：该环境记 timeout（EnvTimeout）且计入 failed 桶；
// 混合时 partial，全超时时 failed。
func TestRun_Timeout_IntoFailedBucket(t *testing.T) {
	// 混合：n1 成功 + n2 超时 → partial，failed 桶含超时。
	fd := newFakeDispatcher()
	fd.program("n1", progResp{resp: okResp("e1"), taskID: "t1"})
	fd.program("n2", progResp{code: scheduler.CodeTimeout, taskID: "t2"})
	o := &Orchestrator{sched: fd, store: &fakeStore{}}
	res, err := o.Run(context.Background(), OrchestrationSpec{Tool: "click", NodeIDs: []string{"n1", "n2"}})
	if err != nil {
		t.Fatalf("Run: unexpected err %v", err)
	}
	if res.Status != StatusPartial || res.Passed != 1 || res.Failed != 1 {
		t.Fatalf("超时混合聚合错误: status=%s passed=%d failed=%d", res.Status, res.Passed, res.Failed)
	}
	if e, ok := findEnv(res, "n2"); !ok || e.Status != EnvTimeout {
		t.Fatalf("超时环境 n2 应记 timeout: ok=%v env=%+v", ok, e)
	}

	// 全超时 → status=failed（全 fail，含全 timeout）。
	fd2 := newFakeDispatcher()
	fd2.program("n1", progResp{code: scheduler.CodeTimeout, taskID: "t1"})
	fd2.program("n2", progResp{code: scheduler.CodeTimeout, taskID: "t2"})
	o2 := &Orchestrator{sched: fd2, store: &fakeStore{}}
	res2, err := o2.Run(context.Background(), OrchestrationSpec{Tool: "click", NodeIDs: []string{"n1", "n2"}})
	if err != nil {
		t.Fatalf("Run: unexpected err %v", err)
	}
	if res2.Status != StatusFailed || res2.Passed != 0 || res2.Failed != 2 {
		t.Fatalf("全超时应 failed: status=%s passed=%d failed=%d", res2.Status, res2.Passed, res2.Failed)
	}
}

// TestRun_EnvGroup_PlatformFilter 验证 EnvGroup 解析：仅在线且 platform 匹配的节点入选（unhealthy/异
// platform 排除）。
func TestRun_EnvGroup_PlatformFilter(t *testing.T) {
	fl := &fakeLister{nodes: []*aurav1.NodeInfo{
		{NodeId: "n1", Platform: "linux", Status: "online"},
		{NodeId: "n2", Platform: "linux", Status: "online"},
		{NodeId: "n3", Platform: "android", Status: "online"},
		{NodeId: "n4", Platform: "linux", Status: "unhealthy"},
	}}
	fd := newFakeDispatcher()
	fd.program("n1", progResp{resp: okResp("e1"), taskID: "t1"})
	fd.program("n2", progResp{resp: okResp("e2"), taskID: "t2"})
	o := &Orchestrator{reg: fl, sched: fd, store: &fakeStore{}}

	res, err := o.Run(context.Background(), OrchestrationSpec{Tool: "click", EnvGroup: "linux"})
	if err != nil {
		t.Fatalf("Run: unexpected err %v", err)
	}
	if res.Total != 2 {
		t.Fatalf("EnvGroup=linux 应解析 2 个在线 linux 节点, got total=%d", res.Total)
	}
	if fd.calls["n3"] != 0 || fd.calls["n4"] != 0 {
		t.Fatalf("异 platform/unhealthy 节点不应入选: n3=%d n4=%d", fd.calls["n3"], fd.calls["n4"])
	}
	if fd.calls["n1"] != 1 || fd.calls["n2"] != 1 {
		t.Fatalf("在线 linux 节点应各下发一次: n1=%d n2=%d", fd.calls["n1"], fd.calls["n2"])
	}
}

// TestRun_NodeIDsDedup 验证 NodeIDs 去重保序（含空串剔除）：同 node 只下发一次，免 per-node 队列自撞。
func TestRun_NodeIDsDedup(t *testing.T) {
	fd := newFakeDispatcher()
	fd.program("n1", progResp{resp: okResp("e1"), taskID: "t1"})
	fd.program("n2", progResp{resp: okResp("e2"), taskID: "t2"})
	o := &Orchestrator{sched: fd, store: &fakeStore{}}

	res, err := o.Run(context.Background(), OrchestrationSpec{Tool: "click", NodeIDs: []string{"n1", "n1", "n2", ""}})
	if err != nil {
		t.Fatalf("Run: unexpected err %v", err)
	}
	if res.Total != 2 || fd.calls["n1"] != 1 || fd.calls["n2"] != 1 {
		t.Fatalf("去重错误: total=%d n1=%d n2=%d", res.Total, fd.calls["n1"], fd.calls["n2"])
	}
}

// TestRun_NoTargets 验证空目标（node_ids/env_group 均空，或 env_group 解析空集）→ ErrNoTargets，不下发。
func TestRun_NoTargets(t *testing.T) {
	fd := newFakeDispatcher()
	o := &Orchestrator{reg: &fakeLister{}, sched: fd, store: &fakeStore{}}

	if _, err := o.Run(context.Background(), OrchestrationSpec{Tool: "click"}); err != ErrNoTargets {
		t.Fatalf("空目标: want ErrNoTargets, got %v", err)
	}
	if _, err := o.Run(context.Background(), OrchestrationSpec{Tool: "click", EnvGroup: "windows"}); err != ErrNoTargets {
		t.Fatalf("env_group 无匹配: want ErrNoTargets, got %v", err)
	}
	if len(fd.calls) != 0 {
		t.Fatalf("空目标不应发生任何 dispatch, got %v", fd.calls)
	}
}

// TestRun_TooManyTargets 验证目标数超上限 → ErrTooManyTargets（有界并发防御），不下发。
func TestRun_TooManyTargets(t *testing.T) {
	fd := newFakeDispatcher()
	o := &Orchestrator{sched: fd, store: &fakeStore{}}
	nodes := make([]string, maxFanout+1)
	for i := range nodes {
		nodes[i] = fmt.Sprintf("node-%d", i)
	}
	if _, err := o.Run(context.Background(), OrchestrationSpec{Tool: "click", NodeIDs: nodes}); err != ErrTooManyTargets {
		t.Fatalf("超上限: want ErrTooManyTargets, got %v", err)
	}
	if len(fd.calls) != 0 {
		t.Fatalf("超上限应在 fan-out 前拒绝, got %d 次 dispatch", len(fd.calls))
	}
}

// TestRun_NilStore_InMemory 验证 store 为 nil（纯内存）时不 panic，仍产出 join 聚合。
func TestRun_NilStore_InMemory(t *testing.T) {
	fd := newFakeDispatcher()
	fd.program("n1", progResp{resp: okResp("e1"), taskID: "t1"})
	o := &Orchestrator{sched: fd} // store 为 nil 接口

	res, err := o.Run(context.Background(), OrchestrationSpec{Tool: "click", NodeIDs: []string{"n1"}})
	if err != nil {
		t.Fatalf("Run(nil store): unexpected err %v", err)
	}
	if res.Status != StatusDone || res.Total != 1 || res.Passed != 1 {
		t.Fatalf("纯内存聚合错误: status=%s total=%d passed=%d", res.Status, res.Total, res.Passed)
	}
}

// sameSet 比较两个字符串切片是否为同一集合（顺序无关，供 task_id 关联断言）。
func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	m := make(map[string]int, len(a))
	for _, x := range a {
		m[x]++
	}
	for _, x := range b {
		m[x]--
	}
	for _, v := range m {
		if v != 0 {
			return false
		}
	}
	return true
}
