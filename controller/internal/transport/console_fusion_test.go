package transport

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"

	aurav1 "github.com/aura/controller/gen/aura/v1"
	"github.com/aura/controller/internal/fusion"
	"github.com/aura/controller/internal/registry"
	"github.com/aura/controller/internal/store"
)

// fusionCapableTools 是 fusion_capable 节点的最小工具集（融合前置两快工具 + 常规工具）。
var fusionCapableTools = []string{"screenshot", "get_a11y_tree", "click"}

// fakeFusionJobs 是 fusionJobStore 的离线替身：记录 CreateFusionJob 入参、按预设返回 GetFusionJob。
type fakeFusionJobs struct {
	mu      sync.Mutex
	created []store.FusionJobRecord
	getRec  store.FusionJobRecord
	getErr  error
}

func (f *fakeFusionJobs) CreateFusionJob(_ context.Context, rec store.FusionJobRecord) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.created = append(f.created, rec)
	return nil
}

func (f *fakeFusionJobs) GetFusionJob(_ context.Context, _ string) (store.FusionJobRecord, error) {
	return f.getRec, f.getErr
}

func (f *fakeFusionJobs) createdRecords() []store.FusionJobRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]store.FusionJobRecord(nil), f.created...)
}

// fakeResultGetter 是 fusionResultGetter 的离线替身：按预设返回 result.json 字节。
type fakeResultGetter struct {
	data []byte
	err  error
}

func (f *fakeResultGetter) GetObject(_ context.Context, _ string) ([]byte, string, error) {
	return f.data, "application/json", f.err
}

// fakeFusionDispatcher 实现 fusion.Dispatcher：快失败 + 首调信号。供断言 SubmitFusion 确已异步
// 起跑 engine.Run（Run 首步即 dispatch screenshot，收到信号即证明后台 goroutine 真执行），
// 且快失败令遗留 goroutine 立即收敛。
type fakeFusionDispatcher struct {
	called chan struct{}
	once   sync.Once
}

func (d *fakeFusionDispatcher) Dispatch(context.Context, string, string, []byte, int64, string, string) (*aurav1.ToolResponse, string, error) {
	d.once.Do(func() { close(d.called) })
	return nil, "E_NODE_OFFLINE", errors.New("fake dispatcher: fail fast")
}

// newFusionTestServer 组装融合面测试服务：在线节点（node-f，指定 tools）+ fake job store /
// result getter + 真 fusion.Engine（fake Dispatcher + MockDetector，store/putter nil）。
func newFusionTestServer(tools []string) (*ConsoleServiceServer, *fakeFusionJobs, *fakeFusionDispatcher) {
	reg := registry.NewRegistry(nil)
	reg.Add(registry.NewSession("node-f", "windows", tools, "aura.v1/2026-07", 1))
	srv := NewConsoleServiceServer(reg, nil, nil, nil, nil)
	fj := &fakeFusionJobs{}
	srv.fusionJobs = fj
	srv.fusionResults = &fakeResultGetter{}
	disp := &fakeFusionDispatcher{called: make(chan struct{})}
	srv.SetFusionEngine(fusion.NewEngine(disp, &fusion.MockDetector{}, nil, nil))
	return srv, fj, disp
}

// TestSubmitFusion_CapableAsyncRun 验证 capable 节点提交：返回非空 job_id、建 status=running 行
// （target/iou_threshold/who 透传）、engine.Run 于独立 goroutine 异步触发（submit 立返不阻塞）。
func TestSubmitFusion_CapableAsyncRun(t *testing.T) {
	srv, fj, disp := newFusionTestServer(fusionCapableTools)
	resp, err := srv.SubmitFusion(context.Background(), connect.NewRequest(&aurav1.SubmitFusionRequest{
		NodeId:       "node-f",
		Target:       "settings",
		IouThreshold: 0.2,
		Who:          "tester",
	}))
	if err != nil {
		t.Fatalf("SubmitFusion: %v", err)
	}
	if resp.Msg.GetJobId() == "" {
		t.Fatal("SubmitFusion returned empty job_id")
	}

	created := fj.createdRecords()
	if len(created) != 1 {
		t.Fatalf("CreateFusionJob calls = %d, want 1", len(created))
	}
	rec := created[0]
	if rec.ID != resp.Msg.GetJobId() || rec.NodeID != "node-f" || rec.Status != "running" ||
		rec.Target != "settings" || rec.IouThreshold != 0.2 || rec.Who != "tester" {
		t.Errorf("fusion job record = %+v, want running row mirroring request", rec)
	}

	select {
	case <-disp.called:
		// engine.Run 已在后台起跑（首步 dispatch screenshot 被 fake 捕获）。
	case <-time.After(2 * time.Second):
		t.Fatal("engine.Run was not triggered asynchronously within 2s")
	}
}

// TestSubmitFusion_NotCapable 验证 tools 缺 get_a11y_tree 的节点被拒 FailedPrecondition
// （E_UNSUPPORTED 语义），且不建 job 行。
func TestSubmitFusion_NotCapable(t *testing.T) {
	srv, fj, _ := newFusionTestServer([]string{"screenshot", "click"})
	_, err := srv.SubmitFusion(context.Background(), connect.NewRequest(&aurav1.SubmitFusionRequest{NodeId: "node-f"}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("SubmitFusion on non-capable node: want FailedPrecondition, got %v", err)
	}
	if n := len(fj.createdRecords()); n != 0 {
		t.Errorf("CreateFusionJob calls = %d, want 0 (rejected before persist)", n)
	}
}

// TestSubmitFusion_EngineNil 验证 engine 未装配（SetFusionEngine 未调，detector 端点未配）时
// 返回 Unavailable（优雅降级，同 GetArtifact nil minio 惯例）。
func TestSubmitFusion_EngineNil(t *testing.T) {
	srv := NewConsoleServiceServer(nil, nil, nil, nil, nil)
	_, err := srv.SubmitFusion(context.Background(), connect.NewRequest(&aurav1.SubmitFusionRequest{NodeId: "node-f"}))
	if connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("SubmitFusion with nil engine: want Unavailable, got %v", err)
	}
}

// TestSubmitFusion_NodeNotConnected 验证目标节点无在线会话时返回 NotFound（无 tools 可判，
// 融合首步 dispatch 也必然 offline）。
func TestSubmitFusion_NodeNotConnected(t *testing.T) {
	srv, _, _ := newFusionTestServer(fusionCapableTools)
	_, err := srv.SubmitFusion(context.Background(), connect.NewRequest(&aurav1.SubmitFusionRequest{NodeId: "node-gone"}))
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("SubmitFusion on offline node: want NotFound, got %v", err)
	}
}

// TestGetFusionResult_Running 验证未终态 job 如实回 status=running、不带元素表。
func TestGetFusionResult_Running(t *testing.T) {
	srv, fj, _ := newFusionTestServer(fusionCapableTools)
	fj.getRec = store.FusionJobRecord{ID: "j1", Status: "running"}
	resp, err := srv.GetFusionResult(context.Background(), connect.NewRequest(&aurav1.GetFusionResultRequest{JobId: "j1"}))
	if err != nil {
		t.Fatalf("GetFusionResult: %v", err)
	}
	if resp.Msg.GetStatus() != "running" || len(resp.Msg.GetElements()) != 0 {
		t.Errorf("running job: status=%q elements=%d, want running with no elements", resp.Msg.GetStatus(), len(resp.Msg.GetElements()))
	}
}

// TestGetFusionResult_Done 验证 done job 从 result_key 读桶反序列化 fusion.FusionResult：内联
// 元素表字段一一映射、iou_threshold 取实际生效值（表内 0=请求默认，result.json 为引擎回落值）、
// 基准屏尺寸回填。
func TestGetFusionResult_Done(t *testing.T) {
	srv, fj, _ := newFusionTestServer(fusionCapableTools)
	fj.getRec = store.FusionJobRecord{
		ID: "j2", Status: "done", VisionInvoked: true,
		ResultKey: "fusion/j2/result.json", IouThreshold: 0, // 表内存请求原值（0=默认）
	}
	payload, err := json.Marshal(fusion.FusionResult{
		JobID: "j2", NodeID: "node-f", VisionInvoked: true, IouThreshold: 0.10,
		Screen: fusion.ScreenDim{W: 1920, H: 1080},
		Elements: []fusion.FusedElement{
			{Source: "a11y", Bounds: [4]int32{10, 20, 100, 40}, Role: "button", Name: "OK", Confidence: 1.0},
			{Source: "vision", Bounds: [4]int32{200, 300, 48, 48}, Caption: "settings gear icon", Confidence: 0.87},
		},
	})
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	srv.fusionResults = &fakeResultGetter{data: payload}

	resp, err := srv.GetFusionResult(context.Background(), connect.NewRequest(&aurav1.GetFusionResultRequest{JobId: "j2"}))
	if err != nil {
		t.Fatalf("GetFusionResult: %v", err)
	}
	m := resp.Msg
	if m.GetStatus() != "done" || !m.GetVisionInvoked() || m.GetResultKey() != "fusion/j2/result.json" {
		t.Errorf("done job header = status %q vision %v key %q", m.GetStatus(), m.GetVisionInvoked(), m.GetResultKey())
	}
	if m.GetIouThreshold() != 0.10 {
		t.Errorf("iou_threshold = %v, want effective 0.10 from result.json (not row value 0)", m.GetIouThreshold())
	}
	if m.GetScreenW() != 1920 || m.GetScreenH() != 1080 {
		t.Errorf("screen = %dx%d, want 1920x1080", m.GetScreenW(), m.GetScreenH())
	}
	els := m.GetElements()
	if len(els) != 2 {
		t.Fatalf("elements = %d, want 2", len(els))
	}
	a := els[0]
	if a.GetSource() != "a11y" || a.GetRole() != "button" || a.GetName() != "OK" || a.GetConfidence() != 1.0 {
		t.Errorf("a11y element mismatch: %+v", a)
	}
	if b := a.GetBounds(); len(b) != 4 || b[0] != 10 || b[1] != 20 || b[2] != 100 || b[3] != 40 {
		t.Errorf("a11y bounds = %v, want [10 20 100 40]", b)
	}
	v := els[1]
	if v.GetSource() != "vision" || v.GetCaption() != "settings gear icon" || v.GetConfidence() != 0.87 {
		t.Errorf("vision element mismatch: %+v", v)
	}
}

// TestGetFusionResult_NotFound 验证未知 job_id（pgx.ErrNoRows）与 store 未配置均返回 NotFound。
func TestGetFusionResult_NotFound(t *testing.T) {
	srv, fj, _ := newFusionTestServer(fusionCapableTools)
	fj.getErr = pgx.ErrNoRows
	_, err := srv.GetFusionResult(context.Background(), connect.NewRequest(&aurav1.GetFusionResultRequest{JobId: "nope"}))
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("GetFusionResult unknown job: want NotFound, got %v", err)
	}

	bare := NewConsoleServiceServer(nil, nil, nil, nil, nil)
	_, err = bare.GetFusionResult(context.Background(), connect.NewRequest(&aurav1.GetFusionResultRequest{JobId: "j1"}))
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("GetFusionResult without store: want NotFound, got %v", err)
	}
}
