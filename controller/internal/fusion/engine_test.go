package fusion

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	aurav1 "github.com/aura/controller/gen/aura/v1"
)

// —— 离线替身：mock Dispatcher（不真 dispatch）+ fake ObjectPutter + fake jobStore ————————

// mockDispatcher 可编程 Dispatcher 替身：按工具名返回预置 envelope；delayN>0 时前 N 次
// slowTool 调用前 sleep delay（模拟 per-node 队列插队/慢执行或平台慢工具，驱动时差路径）；
// failTool 指定工具返回传输层错误。
type mockDispatcher struct {
	mu        sync.Mutex
	calls     []string // 依序记录 tool 名
	deadlines []int64
	shotEnv   []byte
	treeEnv   []byte
	slowTool  string
	delay     time.Duration
	delayN    int
	slowSeen  int
	failTool  string
}

func (m *mockDispatcher) Dispatch(_ context.Context, _, tool string, _ []byte, deadlineMs int64, _, _ string) (*aurav1.ToolResponse, string, error) {
	m.mu.Lock()
	m.calls = append(m.calls, tool)
	m.deadlines = append(m.deadlines, deadlineMs)
	var delay time.Duration
	if tool == m.slowTool {
		m.slowSeen++
		if m.slowSeen <= m.delayN {
			delay = m.delay
		}
	}
	fail := m.failTool != "" && m.failTool == tool
	var env []byte
	switch tool {
	case "screenshot":
		env = m.shotEnv
	case "get_a11y_tree":
		env = m.treeEnv
	}
	m.mu.Unlock()

	if fail {
		return nil, "E_NODE_OFFLINE", errors.New("node gone (mock)")
	}
	if delay > 0 {
		time.Sleep(delay)
	}
	return &aurav1.ToolResponse{JsonEnvelope: env}, "", nil
}

func (m *mockDispatcher) toolCalls(tool string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, c := range m.calls {
		if c == tool {
			n++
		}
	}
	return n
}

// countingDetector 包装 MockDetector 并计数 Detect 调用（SC-3 vision_invoked 留证断言）。
type countingDetector struct {
	MockDetector
	mu    sync.Mutex
	calls int
}

func (c *countingDetector) Detect(ctx context.Context, image []byte, mime string) ([]VisualBox, error) {
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()
	return c.MockDetector.Detect(ctx, image, mime)
}

type fakePutter struct {
	mu    sync.Mutex
	calls int
	key   string
	data  []byte
	ct    string
}

func (f *fakePutter) PutObject(_ context.Context, key string, data []byte, contentType string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.key, f.data, f.ct = key, data, contentType
	return nil
}

type fakeJobStore struct {
	mu            sync.Mutex
	calls         int
	id, status    string
	visionInvoked bool
	resultKey     string
}

func (f *fakeJobStore) UpdateFusionJob(_ context.Context, id, status string, visionInvoked bool, resultKey string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.id, f.status, f.visionInvoked, f.resultKey = id, status, visionInvoked, resultKey
	return nil
}

// —— envelope 构造脚手架（复刻 node 统一信封 {ok,data}）——————————————————————————

// testMeta 是测试基准屏：native 1920×1080、display 960×540、scale=2.0（display→native ×2）。
var testMeta = screenshotMeta{NativeW: 1920, NativeH: 1080, DisplayW: 960, DisplayH: 540, Scale: 2.0}

func shotEnvelope(t *testing.T, image []byte, mime string, meta screenshotMeta) []byte {
	t.Helper()
	env, err := json.Marshal(map[string]any{
		"ok": true,
		"data": screenshotPayload{
			ImageBase64: base64.StdEncoding.EncodeToString(image),
			Mime:        mime,
			Meta:        meta,
		},
	})
	if err != nil {
		t.Fatalf("marshal screenshot envelope: %v", err)
	}
	return env
}

func treeEnvelope(t *testing.T, tree A11yTree) []byte {
	t.Helper()
	env, err := json.Marshal(map[string]any{"ok": true, "data": tree})
	if err != nil {
		t.Fatalf("marshal a11y envelope: %v", err)
	}
	return env
}

// sufficientTree 是 a11y 充分样本：target「确定」按 name 命中（SC-3 不触发视觉路径）。
func sufficientTree() A11yTree {
	return A11yTree{Nodes: []A11yNode{
		boundsNode("button", "确定", [4]int32{100, 100, 200, 80}),
		boundsNode("button", "取消", [4]int32{400, 100, 200, 80}),
		boundsNode("edit", "输入框", [4]int32{100, 300, 500, 60}),
	}}
}

// partialTree 是 partial-a11y 样本（M3）：菜单区 3 小框、其余大片盲区——count 达标但
// 覆盖率低，无 target 时触发视觉。
func partialTree() A11yTree {
	return A11yTree{Nodes: []A11yNode{
		boundsNode("menuitem", "文件", [4]int32{0, 0, 100, 40}),
		boundsNode("menuitem", "编辑", [4]int32{100, 0, 100, 40}),
		boundsNode("menuitem", "帮助", [4]int32{200, 0, 100, 40}),
	}}
}

// newTestEngine 组装离线引擎（同包字面量构造注入 fake jobStore，绕过 NewEngine 的具体
// 类型参数；maxSkew 直取生产缺省，需要注入小值的用例自行覆盖）。
func newTestEngine(md *mockDispatcher, det DetectorClient, fjs *fakeJobStore, fp *fakePutter) *Engine {
	return &Engine{disp: md, det: det, store: fjs, putter: fp, maxSkew: maxCollectSkew}
}

func decodeResult(t *testing.T, data []byte) FusionResult {
	t.Helper()
	var res FusionResult
	if err := json.Unmarshal(data, &res); err != nil {
		t.Fatalf("unmarshal result.json: %v", err)
	}
	return res
}

// —— 用例 ————————————————————————————————————————————————————————————————————

// TestEngineA11ySufficientSkipsVision 验证 a11y-first 主路径（SC-3）：target 命中 →
// 不调 detector（调用计数=0）、result.json vision_invoked=false、元素全 a11y、
// 卸桶 key=fusion/<job>/result.json、job 终态 done。
func TestEngineA11ySufficientSkipsVision(t *testing.T) {
	md := &mockDispatcher{
		shotEnv: shotEnvelope(t, []byte("fake-webp"), "image/webp", testMeta),
		treeEnv: treeEnvelope(t, sufficientTree()),
	}
	det := &countingDetector{MockDetector: MockDetector{Boxes: []VisualBox{{Bbox: [4]int32{1, 1, 5, 5}}}}}
	fjs := &fakeJobStore{}
	fp := &fakePutter{}
	e := newTestEngine(md, det, fjs, fp)

	req := &aurav1.SubmitFusionRequest{NodeId: "node-1", Target: "确定", Who: "test"}
	if err := e.Run(context.Background(), "job-a11y", req); err != nil {
		t.Fatalf("Run: unexpected error: %v", err)
	}

	if det.calls != 0 {
		t.Errorf("detector calls = %d, want 0（a11y 命中不触发视觉，SC-3）", det.calls)
	}
	if fp.calls != 1 || fp.key != "fusion/job-a11y/result.json" || fp.ct != resultContentType {
		t.Errorf("putter: calls=%d key=%q ct=%q, want 1 / fusion/job-a11y/result.json / %s", fp.calls, fp.key, fp.ct, resultContentType)
	}
	res := decodeResult(t, fp.data)
	if res.VisionInvoked {
		t.Error("result.json vision_invoked = true, want false")
	}
	if res.JobID != "job-a11y" || res.NodeID != "node-1" {
		t.Errorf("result identity: got job=%q node=%q", res.JobID, res.NodeID)
	}
	if res.Screen != (ScreenDim{W: 1920, H: 1080}) {
		t.Errorf("result screen: got %+v, want native 1920x1080", res.Screen)
	}
	if len(res.Elements) != 3 {
		t.Fatalf("elements: got %d, want 3 (all a11y)", len(res.Elements))
	}
	for _, el := range res.Elements {
		if el.Source != SourceA11y {
			t.Errorf("element source: got %q, want a11y", el.Source)
		}
	}
	if fjs.status != statusDone || fjs.visionInvoked || fjs.resultKey != fp.key || fjs.id != "job-a11y" {
		t.Errorf("job store: got %+v, want done/false/%s", fjs, fp.key)
	}
	// 采集 dispatch 的 deadline_ms 缺省显式 40s（req.deadline_ms 未设路径）。
	for i, d := range md.deadlines {
		if d != collectDeadlineMs {
			t.Errorf("dispatch[%d] deadline_ms = %d, want %d", i, d, collectDeadlineMs)
		}
	}
}

// TestEnginePartialInvokesVisionAndAligns 验证 partial-a11y 融合路径：无 target 覆盖率
// 触发 → detector 调用 1 次 → 视觉框 ×scale 对齐 native → 与 a11y 重叠者滤除、增量补
// source=vision → vision_invoked=true。
func TestEnginePartialInvokesVisionAndAligns(t *testing.T) {
	md := &mockDispatcher{
		shotEnv: shotEnvelope(t, []byte("fake-webp"), "image/webp", testMeta),
		treeEnv: treeEnvelope(t, partialTree()),
	}
	// detector 出框在 display 空间（scale=2 → native ×2）：
	//   dup   display [0,0,50,20]   → native [0,0,100,40]，与 a11y「文件」框 IoU=1 → 滤除；
	//   fresh display [400,300,60,30] → native [800,600,120,60]，无重叠 → 补 vision。
	det := &countingDetector{MockDetector: MockDetector{Boxes: []VisualBox{
		{Bbox: [4]int32{0, 0, 50, 20}, Kind: "icon", Caption: "file menu", Confidence: 0.9},
		{Bbox: [4]int32{400, 300, 60, 30}, Kind: "icon", Caption: "play button", Confidence: 0.8},
	}}}
	fjs := &fakeJobStore{}
	fp := &fakePutter{}
	e := newTestEngine(md, det, fjs, fp)

	req := &aurav1.SubmitFusionRequest{NodeId: "node-1", Who: "test"}
	if err := e.Run(context.Background(), "job-vis", req); err != nil {
		t.Fatalf("Run: unexpected error: %v", err)
	}

	if det.calls != 1 {
		t.Errorf("detector calls = %d, want 1", det.calls)
	}
	res := decodeResult(t, fp.data)
	if !res.VisionInvoked {
		t.Error("result.json vision_invoked = false, want true")
	}
	if res.IouThreshold != DefaultIoUThreshold {
		t.Errorf("iou_threshold: got %v, want default %v", res.IouThreshold, DefaultIoUThreshold)
	}
	if len(res.Elements) != 4 {
		t.Fatalf("elements: got %d, want 4 (3 a11y + 1 vision; 重叠视觉框须滤除): %+v", len(res.Elements), res.Elements)
	}
	v := res.Elements[3]
	if v.Source != SourceVision || v.Bounds != [4]int32{800, 600, 120, 60} {
		t.Errorf("vision element: got %+v, want source=vision bounds=[800,600,120,60]（display×2 对齐 native，D11）", v)
	}
	if v.Role != "icon" || v.Caption != "play button" || v.Confidence != 0.8 {
		t.Errorf("vision element fields: got %+v", v)
	}
	if fjs.status != statusDone || !fjs.visionInvoked {
		t.Errorf("job store: got %+v, want done/true", fjs)
	}
}

// TestEngineForceVision 验证 force_vision 封口：a11y 充分（target 命中）仍强制触发视觉。
func TestEngineForceVision(t *testing.T) {
	md := &mockDispatcher{
		shotEnv: shotEnvelope(t, []byte("fake-webp"), "image/webp", testMeta),
		treeEnv: treeEnvelope(t, sufficientTree()),
	}
	det := &countingDetector{}
	e := newTestEngine(md, det, &fakeJobStore{}, &fakePutter{})

	req := &aurav1.SubmitFusionRequest{NodeId: "node-1", Target: "确定", ForceVision: true}
	if err := e.Run(context.Background(), "job-force", req); err != nil {
		t.Fatalf("Run: unexpected error: %v", err)
	}
	if det.calls != 1 {
		t.Errorf("detector calls = %d, want 1 (force_vision)", det.calls)
	}
}

// TestEngineRecollectOnSkew 验证原子采集时差重采（M1/D8）：首轮 screenshot（第二次
// dispatch，其往返即 skew 窗口）慢于时差上限 → 整对重采（a11y 与 screenshot 各 2 次）→
// 次轮通过、job 正常 done。
func TestEngineRecollectOnSkew(t *testing.T) {
	md := &mockDispatcher{
		shotEnv:  shotEnvelope(t, []byte("fake-webp"), "image/webp", testMeta),
		treeEnv:  treeEnvelope(t, sufficientTree()),
		slowTool: "screenshot",
		delay:    80 * time.Millisecond,
		delayN:   1,
	}
	fjs := &fakeJobStore{}
	e := newTestEngine(md, &countingDetector{}, fjs, &fakePutter{})
	e.maxSkew = 30 * time.Millisecond // 注入小上限，免测试真睡 500ms

	req := &aurav1.SubmitFusionRequest{NodeId: "node-1", Target: "确定"}
	if err := e.Run(context.Background(), "job-skew", req); err != nil {
		t.Fatalf("Run: unexpected error: %v", err)
	}
	if got := md.toolCalls("screenshot"); got != 2 {
		t.Errorf("screenshot dispatches = %d, want 2（超限重采整对）", got)
	}
	if got := md.toolCalls("get_a11y_tree"); got != 2 {
		t.Errorf("get_a11y_tree dispatches = %d, want 2", got)
	}
	if fjs.status != statusDone {
		t.Errorf("job status = %q, want done", fjs.status)
	}
}

// TestEngineSkewExhaustedFails 验证重采穷尽语义：时差持续超限 → maxCollectAttempts 轮
// 后失败、job failed、不卸桶（高动态屏如实报错优于产出错位融合表）。
func TestEngineSkewExhaustedFails(t *testing.T) {
	md := &mockDispatcher{
		shotEnv:  shotEnvelope(t, []byte("fake-webp"), "image/webp", testMeta),
		treeEnv:  treeEnvelope(t, sufficientTree()),
		slowTool: "screenshot",
		delay:    80 * time.Millisecond,
		delayN:   1 << 30, // 恒慢
	}
	fjs := &fakeJobStore{}
	fp := &fakePutter{}
	e := newTestEngine(md, &countingDetector{}, fjs, fp)
	e.maxSkew = 30 * time.Millisecond

	err := e.Run(context.Background(), "job-dyn", &aurav1.SubmitFusionRequest{NodeId: "node-1"})
	if err == nil || !strings.Contains(err.Error(), "skew") {
		t.Fatalf("Run: got err %v, want collect skew failure", err)
	}
	if got := md.toolCalls("screenshot"); got != maxCollectAttempts {
		t.Errorf("screenshot dispatches = %d, want %d", got, maxCollectAttempts)
	}
	if fjs.status != statusFailed || fjs.visionInvoked {
		t.Errorf("job store: got %+v, want failed/false", fjs)
	}
	if fp.calls != 0 {
		t.Errorf("putter calls = %d, want 0", fp.calls)
	}
}

// TestEngineSlowA11yNotSkew 固化 T13 修复语义（android/iOS 慢采集 driver）：get_a11y_tree
// 恒慢于时差上限（android uiautomator dump 实测 ~2.07s vs 500ms 的缩比重演）也不触发重采——
// a11y 置前后其耗时不落入 skew 窗口；顺序契约 a11y → screenshot 一并断言。
func TestEngineSlowA11yNotSkew(t *testing.T) {
	md := &mockDispatcher{
		shotEnv:  shotEnvelope(t, []byte("fake-webp"), "image/webp", testMeta),
		treeEnv:  treeEnvelope(t, sufficientTree()),
		slowTool: "get_a11y_tree",
		delay:    80 * time.Millisecond,
		delayN:   1 << 30, // 恒慢
	}
	fjs := &fakeJobStore{}
	e := newTestEngine(md, &countingDetector{}, fjs, &fakePutter{})
	e.maxSkew = 30 * time.Millisecond

	req := &aurav1.SubmitFusionRequest{NodeId: "node-1", Target: "确定"}
	if err := e.Run(context.Background(), "job-slow-a11y", req); err != nil {
		t.Fatalf("Run: unexpected error: %v（慢 a11y 不得计入 skew）", err)
	}
	if got := md.toolCalls("get_a11y_tree"); got != 1 {
		t.Errorf("get_a11y_tree dispatches = %d, want 1（无重采）", got)
	}
	if got := md.toolCalls("screenshot"); got != 1 {
		t.Errorf("screenshot dispatches = %d, want 1（无重采）", got)
	}
	if len(md.calls) != 2 || md.calls[0] != "get_a11y_tree" || md.calls[1] != "screenshot" {
		t.Errorf("dispatch order = %v, want [get_a11y_tree screenshot]（顺序契约：慢者置前）", md.calls)
	}
	if fjs.status != statusDone {
		t.Errorf("job status = %q, want done", fjs.status)
	}
}

// TestNewEngineMaxSkewEnvOverride 验证 AURA_FUSION_MAX_SKEW_MS 运维阀：合法值覆盖上限、
// 非法/非正值忽略保持缺省（不静默改语义）。
func TestNewEngineMaxSkewEnvOverride(t *testing.T) {
	cases := []struct {
		env  string
		want time.Duration
	}{
		{"3000", 3 * time.Second},
		{"abc", maxCollectSkew},
		{"-5", maxCollectSkew},
		{"", maxCollectSkew},
	}
	for _, c := range cases {
		t.Setenv("AURA_FUSION_MAX_SKEW_MS", c.env)
		if got := NewEngine(nil, nil, nil, nil).maxSkew; got != c.want {
			t.Errorf("AURA_FUSION_MAX_SKEW_MS=%q: maxSkew = %v, want %v", c.env, got, c.want)
		}
	}
}

// TestEngineDispatchErrorFails 验证采集传输层错误不重采：screenshot 返 E_NODE_OFFLINE →
// 单次即失败（重试决策归 RPC 调用方）、job failed。a11y（第一次 dispatch）给好信封。
func TestEngineDispatchErrorFails(t *testing.T) {
	md := &mockDispatcher{failTool: "screenshot", treeEnv: treeEnvelope(t, sufficientTree())}
	fjs := &fakeJobStore{}
	e := newTestEngine(md, &countingDetector{}, fjs, &fakePutter{})

	err := e.Run(context.Background(), "job-off", &aurav1.SubmitFusionRequest{NodeId: "node-1"})
	if err == nil || !strings.Contains(err.Error(), "E_NODE_OFFLINE") {
		t.Fatalf("Run: got err %v, want E_NODE_OFFLINE dispatch failure", err)
	}
	if got := md.toolCalls("screenshot"); got != 1 {
		t.Errorf("screenshot dispatches = %d, want 1（传输层错误不重采）", got)
	}
	if fjs.status != statusFailed {
		t.Errorf("job status = %q, want failed", fjs.status)
	}
}

// TestEngineDetectorErrorFailsJob 验证 detector 失败路径：引擎不重试（T7 契约：单 worker
// 队列，重试只加深积压）、job failed 且 vision_invoked=true 如实回写。
func TestEngineDetectorErrorFailsJob(t *testing.T) {
	md := &mockDispatcher{
		shotEnv: shotEnvelope(t, []byte("fake-webp"), "image/webp", testMeta),
		treeEnv: treeEnvelope(t, partialTree()),
	}
	det := &countingDetector{MockDetector: MockDetector{Err: errors.New("inference blew up")}}
	fjs := &fakeJobStore{}
	fp := &fakePutter{}
	e := newTestEngine(md, det, fjs, fp)

	err := e.Run(context.Background(), "job-det", &aurav1.SubmitFusionRequest{NodeId: "node-1"})
	if err == nil || !strings.Contains(err.Error(), "detector") {
		t.Fatalf("Run: got err %v, want detector failure", err)
	}
	if det.calls != 1 {
		t.Errorf("detector calls = %d, want 1（引擎不重试）", det.calls)
	}
	if fjs.status != statusFailed || !fjs.visionInvoked {
		t.Errorf("job store: got %+v, want failed + vision_invoked=true（如实回写）", fjs)
	}
	if fp.calls != 0 {
		t.Errorf("putter calls = %d, want 0", fp.calls)
	}
}

// TestEngineNoPutterFails 验证结果出路缺失语义：putter 未配置 → job failed（fusion_jobs
// 无内联结果列，MinIO 是唯一结果出路，静默 done 会造成结果不可读的隐藏失败）。
func TestEngineNoPutterFails(t *testing.T) {
	md := &mockDispatcher{
		shotEnv: shotEnvelope(t, []byte("fake-webp"), "image/webp", testMeta),
		treeEnv: treeEnvelope(t, sufficientTree()),
	}
	fjs := &fakeJobStore{}
	e := &Engine{disp: md, det: &countingDetector{}, store: fjs, maxSkew: maxCollectSkew} // putter 缺席

	err := e.Run(context.Background(), "job-nop", &aurav1.SubmitFusionRequest{NodeId: "node-1", Target: "确定"})
	if err == nil || !strings.Contains(err.Error(), "unconfigured") {
		t.Fatalf("Run: got err %v, want result store unconfigured", err)
	}
	if fjs.status != statusFailed {
		t.Errorf("job status = %q, want failed", fjs.status)
	}
}

// TestEngineEnvelopeErrorFails 验证节点信封业务错误（ok=false + E_ 码）转 Go error。
// a11y 信封给好值，让流程走到第二次 dispatch（screenshot）的坏信封。
func TestEngineEnvelopeErrorFails(t *testing.T) {
	md := &mockDispatcher{
		shotEnv: []byte(`{"ok":false,"error":{"code":"E_CAPTURE_FAILED","message":"no display"}}`),
		treeEnv: treeEnvelope(t, sufficientTree()),
	}
	fjs := &fakeJobStore{}
	e := newTestEngine(md, &countingDetector{}, fjs, &fakePutter{})

	err := e.Run(context.Background(), "job-env", &aurav1.SubmitFusionRequest{NodeId: "node-1"})
	if err == nil || !strings.Contains(err.Error(), "E_CAPTURE_FAILED") {
		t.Fatalf("Run: got err %v, want envelope E_CAPTURE_FAILED", err)
	}
	if fjs.status != statusFailed {
		t.Errorf("job status = %q, want failed", fjs.status)
	}
}
