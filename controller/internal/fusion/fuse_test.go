package fusion

import (
	"math"
	"testing"
)

const epsilon = 1e-9

func almostEqual(a, b float64) bool {
	return math.Abs(a-b) < epsilon
}

// boundsNode 构造带 bounds 的 a11y 节点（测试脚手架）。
func boundsNode(role, name string, b [4]int32, children ...A11yNode) A11yNode {
	bb := b
	return A11yNode{Role: role, Name: name, Bounds: &bb, Children: children}
}

// bareNode 构造无 bounds 的 a11y 节点（容器/平台不可得场景）。
func bareNode(role, name string, children ...A11yNode) A11yNode {
	return A11yNode{Role: role, Name: name, Children: children}
}

// TestFuseIoU 验证矩形交并比：同框=1、不相交/边贴/零面积=0、部分重叠为手算已知值。
func TestFuseIoU(t *testing.T) {
	cases := []struct {
		name string
		a, b [4]int32
		want float64
	}{
		{"identical", [4]int32{0, 0, 10, 10}, [4]int32{0, 0, 10, 10}, 1.0},
		{"disjoint", [4]int32{0, 0, 10, 10}, [4]int32{20, 20, 5, 5}, 0},
		{"edge-touching", [4]int32{0, 0, 10, 10}, [4]int32{10, 0, 10, 10}, 0},
		// inter=5×10=50，union=100+100-50=150 → 1/3。
		{"half-overlap", [4]int32{0, 0, 10, 10}, [4]int32{5, 0, 10, 10}, 1.0 / 3.0},
		{"zero-area-a", [4]int32{0, 0, 0, 10}, [4]int32{0, 0, 10, 10}, 0},
		{"negative-area-b", [4]int32{0, 0, 10, 10}, [4]int32{0, 0, -5, 10}, 0},
	}
	for _, c := range cases {
		if got := iou(c.a, c.b); !almostEqual(got, c.want) {
			t.Errorf("%s: iou(%v, %v) = %v, want %v", c.name, c.a, c.b, got, c.want)
		}
		// IoU 对称性：交并比与参数顺序无关。
		if got, rev := iou(c.a, c.b), iou(c.b, c.a); !almostEqual(got, rev) {
			t.Errorf("%s: iou not symmetric: %v vs %v", c.name, got, rev)
		}
	}
}

// TestFuseAlignBox 验证坐标对齐（D11）：display 像素 ×scale→native 就近取整，与 node 侧
// scale_coord 同制（其单测已知值：display 640 × scale 1.148 = 734.72 → 735）。
func TestFuseAlignBox(t *testing.T) {
	cases := []struct {
		name  string
		box   [4]int32
		scale float64
		want  [4]int32
	}{
		{"scale-2x", [4]int32{100, 50, 200, 25}, 2.0, [4]int32{200, 100, 400, 50}},
		// 与 node 侧 scale_coord_maps_display_to_native 同一已知值：640×1.148=734.72→735；
		// 100×1.148=114.8→115；480×1.148=551.04→551。
		{"scale-node-known-value", [4]int32{640, 100, 640, 480}, 1.148, [4]int32{735, 115, 735, 551}},
		{"scale-identity", [4]int32{7, 8, 9, 10}, 1.0, [4]int32{7, 8, 9, 10}},
	}
	for _, c := range cases {
		got := alignBox(VisualBox{Bbox: c.box}, c.scale)
		if got != c.want {
			t.Errorf("%s: alignBox(%v, %v) = %v, want %v", c.name, c.box, c.scale, got, c.want)
		}
	}
}

// TestFuseFlattenA11y 验证树扁平化：仅收带有效 bounds（w>0,h>0）的节点、无框节点自身
// 跳过但子树照走、零面积滤除、source=a11y/confidence=1.0。
func TestFuseFlattenA11y(t *testing.T) {
	tree := A11yTree{Nodes: []A11yNode{
		bareNode("window", "主窗口",
			boundsNode("button", "确定", [4]int32{100, 100, 200, 80}),
			boundsNode("button", "零面积", [4]int32{0, 0, 0, 40}),
			bareNode("pane", "容器",
				boundsNode("edit", "输入框", [4]int32{300, 100, 400, 60}),
			),
		),
	}}
	flat := flattenA11y(tree)
	if len(flat) != 2 {
		t.Fatalf("flatten: got %d elements, want 2 (零面积与无框节点须滤除): %+v", len(flat), flat)
	}
	want0 := FusedElement{Source: SourceA11y, Bounds: [4]int32{100, 100, 200, 80}, Role: "button", Name: "确定", Confidence: 1.0}
	if flat[0] != want0 {
		t.Errorf("flatten[0]: got %+v, want %+v", flat[0], want0)
	}
	if flat[1].Name != "输入框" || flat[1].Source != SourceA11y || flat[1].Confidence != 1.0 {
		t.Errorf("flatten[1]: got %+v, want 输入框/a11y/1.0", flat[1])
	}

	if got := flattenA11y(A11yTree{}); len(got) != 0 {
		t.Errorf("flatten empty tree: got %d elements, want 0", len(got))
	}
}

// TestFuseCoveredFraction 验证框并集覆盖率：嵌套框并集不重复计数（差分正确性钉子——
// 朴素面积求和在 a11y 父子嵌套框下会系统性高估）、越界裁剪、不相交求和。
func TestFuseCoveredFraction(t *testing.T) {
	el := func(b [4]int32) FusedElement { return FusedElement{Bounds: b} }
	cases := []struct {
		name string
		flat []FusedElement
		want float64
	}{
		{"single-quarter", []FusedElement{el([4]int32{0, 0, 500, 500})}, 0.25},
		{"two-disjoint", []FusedElement{el([4]int32{0, 0, 100, 100}), el([4]int32{200, 200, 100, 100})}, 0.02},
		// 嵌套：子框完全在父框内，并集=父框 25%（面积求和会错报 26%）。
		{"nested-no-double-count", []FusedElement{el([4]int32{0, 0, 500, 500}), el([4]int32{100, 100, 100, 100})}, 0.25},
		// 部分重叠：并集 = 300×100 = 3%（求和会错报 4%）。
		{"partial-overlap", []FusedElement{el([4]int32{0, 0, 200, 100}), el([4]int32{100, 0, 200, 100})}, 0.03},
		// 越界：裁剪到屏内 [0,0,200,200] = 4%。
		{"clamped-to-screen", []FusedElement{el([4]int32{-100, -100, 300, 300})}, 0.04},
		{"empty", nil, 0},
	}
	for _, c := range cases {
		if got := coveredFraction(c.flat, 1000, 1000); !almostEqual(got, c.want) {
			t.Errorf("%s: coveredFraction = %v, want %v", c.name, got, c.want)
		}
	}
	if got := coveredFraction([]FusedElement{el([4]int32{0, 0, 10, 10})}, 0, 0); got != 0 {
		t.Errorf("zero screen: coveredFraction = %v, want 0", got)
	}
}

// TestFuseFusionNeeded 验证 target-aware 触发判据（D9/M3）全路径：空树触发、target
// 命中不触发（SC-3 a11y-first）、未命中触发、无 target 走「元素数 + 覆盖率」双启发——
// 其中 partial-a11y 样本（count 达标但覆盖率低）是纯 count 判据会误跳过的病例。
func TestFuseFusionNeeded(t *testing.T) {
	const w, h = 1920, 1080

	// 空树 / 全无框树：a11y 全盲，必触发。
	if !fusionNeeded(A11yTree{}, "", w, h) {
		t.Error("empty tree: want fusionNeeded=true")
	}
	noBounds := A11yTree{Nodes: []A11yNode{bareNode("button", "确定")}}
	if !fusionNeeded(noBounds, "确定", w, h) {
		t.Error("tree without bounds: want true even when target matches a boundless node（命中但无框无法定位）")
	}

	// 有 target：命中 name/role（不区分大小写子串）→ 不触发；未命中 → 触发。
	menuTree := A11yTree{Nodes: []A11yNode{
		boundsNode("button", "OK Button", [4]int32{10, 10, 100, 40}),
		boundsNode("菜单项", "设置", [4]int32{10, 60, 100, 40}),
	}}
	if fusionNeeded(menuTree, "ok", w, h) {
		t.Error("target hits name (case-insensitive): want false（a11y 命中不触发，SC-3）")
	}
	if fusionNeeded(menuTree, "菜单", w, h) {
		t.Error("target hits role: want false")
	}
	if !fusionNeeded(menuTree, "开始游戏", w, h) {
		t.Error("target misses: want true")
	}
	// 命中的必须是带框元素：带框节点不命中 + 无框节点命中 → 触发。
	mixed := A11yTree{Nodes: []A11yNode{
		boundsNode("button", "设置", [4]int32{10, 10, 100, 40}),
		bareNode("button", "开始游戏"),
	}}
	if !fusionNeeded(mixed, "开始游戏", w, h) {
		t.Error("target only hits boundless node: want true")
	}

	// 无 target · 覆盖充分：3 框覆盖 36%（≥coverageMin）且 count≥minInteractable → 不触发。
	rich := A11yTree{Nodes: []A11yNode{
		boundsNode("pane", "a", [4]int32{0, 0, 400, 300}),
		boundsNode("pane", "b", [4]int32{500, 0, 400, 300}),
		boundsNode("pane", "c", [4]int32{0, 400, 400, 300}),
	}}
	if fusionNeeded(rich, "", 1000, 1000) {
		t.Error("rich coverage without target: want false")
	}

	// 无 target · 稀疏 count：2 框覆盖 80% 但元素数 < minInteractable → 触发。
	sparse := A11yTree{Nodes: []A11yNode{
		boundsNode("pane", "top", [4]int32{0, 0, 1000, 400}),
		boundsNode("pane", "bottom", [4]int32{0, 400, 1000, 400}),
	}}
	if !fusionNeeded(sparse, "", 1000, 1000) {
		t.Error("sparse element count: want true")
	}

	// 无 target · partial-a11y（M3 病例）：菜单区 3 个小框（count=3 通过 minInteractable，
	// 纯 count 判据会误判充分而跳过视觉），但覆盖率 3×(100×40)/(1920×1080)≈0.6% <
	// coverageMin——游戏/canvas 大片盲区，覆盖率启发正确触发。
	partial := A11yTree{Nodes: []A11yNode{
		boundsNode("menuitem", "文件", [4]int32{0, 0, 100, 40}),
		boundsNode("menuitem", "编辑", [4]int32{100, 0, 100, 40}),
		boundsNode("menuitem", "帮助", [4]int32{200, 0, 100, 40}),
	}}
	if !fusionNeeded(partial, "", w, h) {
		t.Error("partial-a11y (count ok, coverage low): want true（纯 count 判据在此误跳过，M3）")
	}
}

// TestFuseMerge 验证 IoU 滤除融合核（D7/语义#4）：视觉框与任一 a11y 框 IoU>阈值即丢弃
// （a11y 恒赢、滤除非合并），非重叠补 source=vision；a11y 元素恒全量保留不被污染。
func TestFuseMerge(t *testing.T) {
	a11y := []FusedElement{
		{Source: SourceA11y, Bounds: [4]int32{0, 0, 30, 30}, Role: "button", Name: "确定", Confidence: 1.0},
	}

	// 完全重叠（IoU=1 > 0.10）：视觉框滤除，输出=纯 a11y。
	dup := []VisualBoxNative{{Bbox: [4]int32{0, 0, 30, 30}, Kind: "icon", Caption: "ok button", Confidence: 0.9}}
	got := merge(a11y, dup, DefaultIoUThreshold)
	if len(got) != 1 || got[0] != a11y[0] {
		t.Fatalf("full overlap: got %+v, want a11y only（a11y 恒赢，重叠视觉框滤除）", got)
	}

	// 阈值边界：IoU=150/1650≈0.091 ≤ 0.10 → 保留；IoU=300/1500=0.2 > 0.10 → 滤除。
	nearMiss := []VisualBoxNative{{Bbox: [4]int32{25, 0, 30, 30}, Kind: "icon", Caption: "near", Confidence: 0.8}}
	if got := merge(a11y, nearMiss, DefaultIoUThreshold); len(got) != 2 {
		t.Errorf("IoU≈0.091 below threshold: got %d elements, want 2 (kept)", len(got))
	}
	overlap := []VisualBoxNative{{Bbox: [4]int32{20, 0, 30, 30}, Kind: "icon", Caption: "over", Confidence: 0.8}}
	if got := merge(a11y, overlap, DefaultIoUThreshold); len(got) != 1 {
		t.Errorf("IoU=0.2 above threshold: got %d elements, want 1 (dropped)", len(got))
	}

	// 阈值可调：threshold=0.25 时 0.2 重叠框保留；threshold<=0 回落默认 0.10（滤除）。
	if got := merge(a11y, overlap, 0.25); len(got) != 2 {
		t.Errorf("custom threshold 0.25: got %d elements, want 2 (kept)", len(got))
	}
	if got := merge(a11y, overlap, 0); len(got) != 1 {
		t.Errorf("threshold<=0 falls back to default 0.10: got %d elements, want 1 (dropped)", len(got))
	}

	// 非重叠视觉框补全：source=vision、role=kind、caption 保留、name 空、confidence=检测分。
	fresh := []VisualBoxNative{{Bbox: [4]int32{500, 500, 60, 60}, Kind: "icon", Caption: "gear icon", Confidence: 0.77}}
	got = merge(a11y, fresh, DefaultIoUThreshold)
	if len(got) != 2 {
		t.Fatalf("non-overlapping vision: got %d elements, want 2", len(got))
	}
	wantV := FusedElement{Source: SourceVision, Bounds: [4]int32{500, 500, 60, 60}, Role: "icon", Caption: "gear icon", Confidence: 0.77}
	if got[1] != wantV {
		t.Errorf("vision element: got %+v, want %+v", got[1], wantV)
	}

	// a11y 空：视觉框全保留（纯视觉退化路径）。
	if got := merge(nil, fresh, DefaultIoUThreshold); len(got) != 1 || got[0].Source != SourceVision {
		t.Errorf("empty a11y: got %+v, want single vision element", got)
	}

	// 零面积视觉框：直接丢弃。
	dirty := []VisualBoxNative{{Bbox: [4]int32{10, 10, 0, 50}, Kind: "icon", Confidence: 0.9}}
	if got := merge(a11y, dirty, DefaultIoUThreshold); len(got) != 1 {
		t.Errorf("zero-area vision box: got %d elements, want 1 (dropped)", len(got))
	}
}
