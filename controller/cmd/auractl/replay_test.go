package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	"connectrpc.com/connect"

	aurav1 "github.com/aura/controller/gen/aura/v1"
	aurav1connect "github.com/aura/controller/gen/aura/v1/aurav1connect"
)

// fakeAdminClient 是 ControllerAdminClient 的内存桩，供回放引擎单测（分页 / 三分支 / 子集预检 / 报告）驱动。
type fakeAdminClient struct {
	pages            map[string]*aurav1.GetTraceResponse // GetTrace 分页：按传入 page_token 返回分片（""=首页）
	nodes            []*aurav1.NodeInfo                  // ListNodes 返回的节点表
	dispatchEnvelope func(req *aurav1.DispatchToolRequest) ([]byte, error)
	createResp       *aurav1.CreateEnvironmentResponse
	createErr        error

	// 记录（断言用）。
	dispatched   []*aurav1.DispatchToolRequest
	createdCount int
	destroyed    []string
	listCount    int
	getTraceReqs []*aurav1.GetTraceRequest
}

// 编译期确认桩实现完整接口。
var _ aurav1connect.ControllerAdminClient = (*fakeAdminClient)(nil)

func (f *fakeAdminClient) ListNodes(ctx context.Context, req *connect.Request[aurav1.ListNodesRequest]) (*connect.Response[aurav1.ListNodesResponse], error) {
	f.listCount++
	return connect.NewResponse(&aurav1.ListNodesResponse{Nodes: f.nodes}), nil
}

func (f *fakeAdminClient) DispatchTool(ctx context.Context, req *connect.Request[aurav1.DispatchToolRequest]) (*connect.Response[aurav1.DispatchToolResponse], error) {
	f.dispatched = append(f.dispatched, req.Msg)
	env := []byte(`{"ok":true,"data":{"done":true}}`)
	if f.dispatchEnvelope != nil {
		b, err := f.dispatchEnvelope(req.Msg)
		if err != nil {
			return nil, err
		}
		env = b
	}
	return connect.NewResponse(&aurav1.DispatchToolResponse{JsonEnvelope: env}), nil
}

func (f *fakeAdminClient) CreateEnvironment(ctx context.Context, req *connect.Request[aurav1.CreateEnvironmentRequest]) (*connect.Response[aurav1.CreateEnvironmentResponse], error) {
	f.createdCount++
	if f.createErr != nil {
		return nil, f.createErr
	}
	resp := f.createResp
	if resp == nil {
		resp = &aurav1.CreateEnvironmentResponse{EnvId: "env-default", NodeId: "eph-default"}
	}
	return connect.NewResponse(resp), nil
}

func (f *fakeAdminClient) DestroyEnvironment(ctx context.Context, req *connect.Request[aurav1.DestroyEnvironmentRequest]) (*connect.Response[aurav1.DestroyEnvironmentResponse], error) {
	f.destroyed = append(f.destroyed, req.Msg.GetEnvId())
	return connect.NewResponse(&aurav1.DestroyEnvironmentResponse{Destroyed: true}), nil
}

func (f *fakeAdminClient) StartTrace(ctx context.Context, req *connect.Request[aurav1.StartTraceRequest]) (*connect.Response[aurav1.StartTraceResponse], error) {
	return connect.NewResponse(&aurav1.StartTraceResponse{TraceId: "trace-new"}), nil
}

func (f *fakeAdminClient) StopTrace(ctx context.Context, req *connect.Request[aurav1.StopTraceRequest]) (*connect.Response[aurav1.StopTraceResponse], error) {
	return connect.NewResponse(&aurav1.StopTraceResponse{Stopped: true}), nil
}

func (f *fakeAdminClient) GetTrace(ctx context.Context, req *connect.Request[aurav1.GetTraceRequest]) (*connect.Response[aurav1.GetTraceResponse], error) {
	f.getTraceReqs = append(f.getTraceReqs, req.Msg)
	resp, ok := f.pages[req.Msg.GetPageToken()]
	if !ok {
		return nil, fmt.Errorf("fake: no page for token %q", req.Msg.GetPageToken())
	}
	return connect.NewResponse(resp), nil
}

// TestFetchTraceStepsPagination 覆盖 GetTrace 分页循环：翻页至空 token 聚全步序（保序）+ 源节点/平台回填。
func TestFetchTraceStepsPagination(t *testing.T) {
	mk := func(seqs ...int64) []*aurav1.TraceStep {
		var s []*aurav1.TraceStep
		for _, q := range seqs {
			s = append(s, &aurav1.TraceStep{Seq: q, Tool: "screenshot"})
		}
		return s
	}
	f := &fakeAdminClient{pages: map[string]*aurav1.GetTraceResponse{
		"":   {Steps: mk(1, 2), NodeId: "n-src", Platform: "linux", NextPageToken: "p1"},
		"p1": {Steps: mk(3, 4), NodeId: "n-src", Platform: "linux", NextPageToken: "p2"},
		"p2": {Steps: mk(5), NodeId: "n-src", Platform: "linux", NextPageToken: ""},
	}}
	steps, node, platform, err := fetchTraceSteps(context.Background(), f, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 5 {
		t.Fatalf("want 5 aggregated steps, got %d", len(steps))
	}
	for i, s := range steps {
		if s.GetSeq() != int64(i+1) {
			t.Fatalf("step %d out of order: seq=%d", i, s.GetSeq())
		}
	}
	if node != "n-src" || platform != "linux" {
		t.Fatalf("source mismatch: node=%q platform=%q", node, platform)
	}
	if len(f.getTraceReqs) != 3 {
		t.Fatalf("want 3 paged GetTrace calls (turn pages to empty token), got %d", len(f.getTraceReqs))
	}
}

// TestResolveTargetExplicitNode 分支 1：--node 显式指定 → 直接指向，不 ListNodes/CreateEnvironment，不销毁。
func TestResolveTargetExplicitNode(t *testing.T) {
	f := &fakeAdminClient{}
	node, mode, cleanup, err := resolveTarget(context.Background(), f, "explicit-1", "linux")
	if err != nil {
		t.Fatal(err)
	}
	if node != "explicit-1" || mode != "explicit-node" || cleanup != "" {
		t.Fatalf("explicit: node=%q mode=%q cleanup=%q", node, mode, cleanup)
	}
	if f.listCount != 0 || f.createdCount != 0 {
		t.Fatalf("explicit branch must not ListNodes(%d)/Create(%d)", f.listCount, f.createdCount)
	}
}

// TestResolveTargetSamePlatform 分支 2：同 platform 在线节点 → best-effort，不建环境（引擎只警示不清场）。
func TestResolveTargetSamePlatform(t *testing.T) {
	f := &fakeAdminClient{nodes: []*aurav1.NodeInfo{
		{NodeId: "n-lin", Platform: "linux", Status: "online"},
	}}
	node, mode, cleanup, err := resolveTarget(context.Background(), f, "", "linux")
	if err != nil {
		t.Fatal(err)
	}
	if node != "n-lin" || mode != "same-platform-best-effort" || cleanup != "" {
		t.Fatalf("same-platform: node=%q mode=%q cleanup=%q", node, mode, cleanup)
	}
	if f.createdCount != 0 {
		t.Fatalf("same-platform branch must not CreateEnvironment (got %d)", f.createdCount)
	}
}

// TestResolveTargetEphemeral 分支 3：无 --node 且无同型在线节点 → CreateEnvironment S0 ephemeral，返回 env_id 供销毁。
func TestResolveTargetEphemeral(t *testing.T) {
	f := &fakeAdminClient{
		nodes:      []*aurav1.NodeInfo{{NodeId: "n-win", Platform: "windows", Status: "online"}},
		createResp: &aurav1.CreateEnvironmentResponse{EnvId: "env-42", NodeId: "eph-9"},
	}
	node, mode, cleanup, err := resolveTarget(context.Background(), f, "", "linux")
	if err != nil {
		t.Fatal(err)
	}
	if node != "eph-9" || mode != "ephemeral" || cleanup != "env-42" {
		t.Fatalf("ephemeral: node=%q mode=%q cleanup=%q", node, mode, cleanup)
	}
	if f.createdCount != 1 {
		t.Fatalf("want 1 CreateEnvironment, got %d", f.createdCount)
	}
}

// TestRunReplaySubsetFailFast 覆盖子集预检 fail-fast：目标不支持工具的步标 UNSUPPORTED（不静默跳过、不下发），
// 且终态被拉低为 FAIL（即便 assert 步节点判 PASS）。
func TestRunReplaySubsetFailFast(t *testing.T) {
	steps := []*aurav1.TraceStep{
		{Seq: 1, Tool: "screenshot", JsonArgs: []byte(`{}`)},
		{Seq: 2, Tool: "android_scroll", JsonArgs: []byte(`{}`)}, // desktop 目标不支持
		{Seq: 3, Tool: "assert", JsonArgs: []byte(`{"mode":"a11y","expect":"OK"}`)},
	}
	f := &fakeAdminClient{
		pages: map[string]*aurav1.GetTraceResponse{
			"": {Steps: steps, NodeId: "n-android", Platform: "android", NextPageToken: ""},
		},
		nodes: []*aurav1.NodeInfo{
			{NodeId: "n-lin", Platform: "linux", Status: "online", Tools: []string{"screenshot", "click", "assert"}},
		},
		dispatchEnvelope: func(req *aurav1.DispatchToolRequest) ([]byte, error) {
			if req.GetTool() == "assert" {
				return []byte(`{"ok":true,"data":{"passed":true,"detail":"node matched"}}`), nil
			}
			return []byte(`{"ok":true,"data":{"done":true}}`), nil
		},
	}
	report, err := runReplay(context.Background(), f, "t1", "n-lin", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if report.Unsupported != 1 {
		t.Fatalf("want 1 unsupported step, got %d", report.Unsupported)
	}
	var sawUnsupported bool
	for _, s := range report.Steps {
		if s.Seq == 2 && s.Status == "UNSUPPORTED" {
			sawUnsupported = true
		}
	}
	if !sawUnsupported {
		t.Fatalf("seq2 must be marked UNSUPPORTED (not silently skipped): %+v", report.Steps)
	}
	for _, d := range f.dispatched {
		if d.GetTool() == "android_scroll" {
			t.Fatal("unsupported tool must not be dispatched (fail-fast pre-check)")
		}
	}
	if report.Verdict != "FAIL" {
		t.Fatalf("want verdict FAIL due to unsupported step, got %q", report.Verdict)
	}
	if report.TerminalAssert != "PASS" {
		t.Fatalf("assert step should be node-judged PASS, got %q", report.TerminalAssert)
	}
}

// TestRunReplayTerminalAssert 覆盖终态=末 assert 步节点评判 + assert 步逐字重发 + 非录制 dispatch（trace_id 空 / who=replay）。
func TestRunReplayTerminalAssert(t *testing.T) {
	steps := []*aurav1.TraceStep{
		{Seq: 1, Tool: "screenshot", JsonArgs: []byte(`{}`)},
		{Seq: 2, Tool: "assert", JsonArgs: []byte(`{"mode":"a11y","expect":"OK"}`)},
	}
	cases := []struct {
		name        string
		assertPass  bool
		wantVerdict string
	}{
		{"assert passes -> PASS", true, "PASS"},
		{"assert fails -> FAIL", false, "FAIL"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := &fakeAdminClient{
				pages: map[string]*aurav1.GetTraceResponse{
					"": {Steps: steps, NodeId: "n-src", Platform: "linux", NextPageToken: ""},
				},
				dispatchEnvelope: func(req *aurav1.DispatchToolRequest) ([]byte, error) {
					if req.GetTool() == "assert" {
						if c.assertPass {
							return []byte(`{"ok":true,"data":{"passed":true,"detail":"matched"}}`), nil
						}
						return []byte(`{"ok":true,"data":{"passed":false,"detail":"not found"}}`), nil
					}
					return []byte(`{"ok":true,"data":{"done":true}}`), nil
				},
			}
			// --node 指向不在 fleet 的节点 → 子集未知 → 跳过预检，全步 dispatch（隔离报告路径）。
			report, err := runReplay(context.Background(), f, "t1", "n-target", "", 0)
			if err != nil {
				t.Fatal(err)
			}
			if report.Verdict != c.wantVerdict {
				t.Fatalf("verdict: want %q got %q", c.wantVerdict, report.Verdict)
			}
			if report.TerminalAssert != c.wantVerdict {
				t.Fatalf("terminal_assert: want %q got %q", c.wantVerdict, report.TerminalAssert)
			}
			var sawAssert bool
			for _, d := range f.dispatched {
				if d.GetTool() != "assert" {
					continue
				}
				sawAssert = true
				if string(d.GetJsonArgs()) != `{"mode":"a11y","expect":"OK"}` {
					t.Fatalf("assert args not replayed verbatim: %s", d.GetJsonArgs())
				}
				if d.GetTraceId() != "" {
					t.Fatalf("replay dispatch must carry empty trace_id (non-recording), got %q", d.GetTraceId())
				}
				if d.GetWho() != "replay" {
					t.Fatalf("replay dispatch who must be 'replay', got %q", d.GetWho())
				}
			}
			if !sawAssert {
				t.Fatal("assert step not dispatched")
			}
		})
	}
}

// TestEvalStep 覆盖单步 envelope 判定：通用步看 ok；assert 步看 ok && data.passed；错误/不可解析 → FAIL。
func TestEvalStep(t *testing.T) {
	cases := []struct {
		name     string
		tool     string
		envelope string
		want     string
	}{
		{"non-assert ok", "screenshot", `{"ok":true,"data":{"done":true}}`, "PASS"},
		{"assert passed", "assert", `{"ok":true,"data":{"passed":true,"detail":"m"}}`, "PASS"},
		{"assert failed", "assert", `{"ok":true,"data":{"passed":false,"detail":"x"}}`, "FAIL"},
		{"envelope error", "click", `{"ok":false,"error":{"code":"E_TIMEOUT","message":"deadline"}}`, "FAIL"},
		{"unparseable", "click", `not json`, "FAIL"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, detail := evalStep(c.tool, []byte(c.envelope))
			if got != c.want {
				t.Fatalf("evalStep(%s,%s)=%q detail=%q, want %q", c.tool, c.envelope, got, detail, c.want)
			}
		})
	}
}

// TestCmdTraceStartStop 覆盖 trace start/stop 子命令挂载：消费 StartTrace/StopTrace，未知子命令报错。
func TestCmdTraceStartStop(t *testing.T) {
	f := &fakeAdminClient{}
	if err := cmdTrace(context.Background(), f, []string{"start", "n1", "--who", "tester"}); err != nil {
		t.Fatalf("trace start: %v", err)
	}
	if err := cmdTrace(context.Background(), f, []string{"stop", "trace-1"}); err != nil {
		t.Fatalf("trace stop: %v", err)
	}
	if err := cmdTrace(context.Background(), f, []string{"bogus"}); err == nil {
		t.Fatal("unknown trace subcommand must error")
	}
}

// captureStderr 在 fn 执行期间将 os.Stderr 重定向到管道并返回捕获文本（还原 os.Stderr）。
// 供断言 runReplay 的 WARN 走 stderr 的用例（这些用例不并行，全局 os.Stderr 交换安全）。
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stderr = w
	defer func() { os.Stderr = old }()
	fn()
	if err := w.Close(); err != nil {
		t.Fatalf("close pipe writer: %v", err)
	}
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		t.Fatalf("read captured stderr: %v", err)
	}
	return buf.String()
}

// TestFetchTraceStepsHangGuard 覆盖分页防挂死守卫（GAP-1）：服务端游标零推进（非空 next_page_token 恒
// 返自身、零步）时 fetchTraceSteps 报错退出而非无限翻页，且在数页内即中止（不空转至 maxPages 上限）。
func TestFetchTraceStepsHangGuard(t *testing.T) {
	f := &fakeAdminClient{pages: map[string]*aurav1.GetTraceResponse{
		"":      {Steps: nil, NodeId: "n-src", Platform: "linux", NextPageToken: "stuck"},
		"stuck": {Steps: nil, NextPageToken: "stuck"}, // 非空 token 恒返自身、零步 → 无进展
	}}
	_, _, _, err := fetchTraceSteps(context.Background(), f, "t1")
	if err == nil {
		t.Fatal("want error on non-progressing pagination token, got nil (would hang forever)")
	}
	if !strings.Contains(err.Error(), "progress") {
		t.Fatalf("error should signal no-progress guard, got %v", err)
	}
	// 守卫在第 2 次请求即触发（next_page_token 与上页请求 token 相同），远未达 maxPages。
	if len(f.getTraceReqs) > 3 {
		t.Fatalf("hang guard must abort within a few pages, got %d requests", len(f.getTraceReqs))
	}
}

// TestRunReplayWarnsOnUnbackfilledToolset 覆盖子集预检数据源缺失路径（GAP-3）：目标节点在 fleet 但
// Tools 未回填（空）→ 跳过预检交节点侧兜底，并向 stderr 打 WARN 保诚实（非硬失败、非静默跳过），
// 全步仍照常 dispatch、不误标 UNSUPPORTED。
func TestRunReplayWarnsOnUnbackfilledToolset(t *testing.T) {
	steps := []*aurav1.TraceStep{{Seq: 1, Tool: "screenshot", JsonArgs: []byte(`{}`)}}
	f := &fakeAdminClient{
		pages: map[string]*aurav1.GetTraceResponse{
			"": {Steps: steps, NodeId: "n-src", Platform: "linux", NextPageToken: ""},
		},
		nodes: []*aurav1.NodeInfo{
			{NodeId: "n-target", Platform: "linux", Status: "online"}, // Tools 空 = 未回填
		},
	}
	var report replayReport
	var rerr error
	stderr := captureStderr(t, func() {
		report, rerr = runReplay(context.Background(), f, "t1", "n-target", "", 0)
	})
	if rerr != nil {
		t.Fatalf("runReplay: %v", rerr)
	}
	if !strings.Contains(stderr, "no backfilled toolset") {
		t.Fatalf("want stderr WARN about unbackfilled toolset (GAP-3), got %q", stderr)
	}
	// 未回填 → 跳过预检（0 unsupported），但仍全步 dispatch 交节点侧兜底。
	if report.Unsupported != 0 {
		t.Fatalf("unbackfilled toolset must skip precheck (0 unsupported), got %d", report.Unsupported)
	}
	if len(f.dispatched) != 1 {
		t.Fatalf("want 1 dispatched step (precheck skipped, node-side fallback), got %d", len(f.dispatched))
	}
}
