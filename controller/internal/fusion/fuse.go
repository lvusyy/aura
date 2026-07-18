// fuse.go 承载融合纯逻辑（UFO² a11y-优先 + IoU 滤除核，arXiv 2504.14603 §3.4）：
// 几何（iou/alignBox）、触发判据（fusionNeeded，target-aware）、融合（flattenA11y/merge）。
// 本文件无任何 I/O，是离线单测核心；I/O 编排见 engine.go。
package fusion

import (
	"math"
	"slices"
	"sort"
	"strings"
)

// 融合元素来源（FusedElement.Source 取值，对齐 console.proto FusedElement.source 注释）。
const (
	SourceA11y   = "a11y"
	SourceVision = "vision"
)

const (
	// DefaultIoUThreshold 是视觉框滤除的 IoU 阈值起点（UFO² §3.4 原值 0.10，语义#4）：
	// 视觉框与任一 a11y 框 IoU > 阈值即滤除该视觉框——a11y 恒赢，滤除而非合并。低阈值 =
	// 激进偏向 a11y（轻微重叠即判重复）。SubmitFusionRequest.iou_threshold<=0 时取此值；
	// 起点可调，a11y 弱样本回归集回归后再定。
	DefaultIoUThreshold = 0.10

	// minInteractable 是无 target 时触发视觉的可交互元素数下限（覆盖率启发第一支，起点可调）：
	// 带有效 bounds 的 a11y 元素少于此数即视为稀疏 → 触发视觉。
	minInteractable = 3

	// coverageMin 是无 target 时触发视觉的 a11y 框并集覆盖率下限（覆盖率启发第二支，起点可调）：
	// a11y 框并集面积 / 屏面积低于此值 → 触发视觉。覆盖率维度（非纯 count）应对 partial-a11y
	// 屏（M3：菜单区有框、游戏/canvas 区无框——count 判据会误跳过视觉，覆盖率不会）。
	coverageMin = 0.15
)

// A11yNode 是 node 侧 aura-capability A11yNode（types.rs）的 Go 镜像（get_a11y_tree
// envelope data 解码用）。bounds 为原生像素 [x,y,w,h]，平台不可得时缺省。
type A11yNode struct {
	Role     string     `json:"role"`
	Name     string     `json:"name"`
	Bounds   *[4]int32  `json:"bounds,omitempty"`
	Value    *string    `json:"value,omitempty"`
	Children []A11yNode `json:"children"`
}

// A11yTree 是 node 侧 A11yTree 的 Go 镜像：顶层节点集 + 截断标志。
type A11yTree struct {
	Nodes     []A11yNode `json:"nodes"`
	Truncated bool       `json:"truncated"`
}

// FusedElement 是融合元素表单项（result.json elements[]，字段对齐 console.proto
// FusedElement）。Bounds 为 native 像素 [x,y,w,h]（与 A11yNode.bounds 同制，回
// DispatchTool tap 零换算）；source=a11y 时 confidence 恒 1.0，vision 时为检测分。
type FusedElement struct {
	Source     string   `json:"source"`
	Bounds     [4]int32 `json:"bounds"`
	Role       string   `json:"role,omitempty"`
	Name       string   `json:"name,omitempty"`
	Caption    string   `json:"caption,omitempty"`
	Confidence float64  `json:"confidence"`
}

// VisualBoxNative 是坐标对齐（alignBox ×scale→native）后的视觉检测框，merge 的视觉侧输入。
// 与 VisualBox 的区别仅在 Bbox 坐标空间：VisualBox 为 detector 输入图（display）像素，
// 本类型为 native 像素（与 a11y bounds 同基，可直接 IoU）。
type VisualBoxNative struct {
	Bbox       [4]int32 // native 像素 [x, y, w, h]
	Kind       string
	Caption    string
	Confidence float64
}

// iou 计算两个 [x,y,w,h] 矩形的交并比（Intersection over Union，[0,1]）。
// 任一侧零/负面积或无交集返回 0（脏框不构成重叠证据）。
func iou(a, b [4]int32) float64 {
	if a[2] <= 0 || a[3] <= 0 || b[2] <= 0 || b[3] <= 0 {
		return 0
	}
	iw := float64(min(a[0]+a[2], b[0]+b[2]) - max(a[0], b[0]))
	ih := float64(min(a[1]+a[3], b[1]+b[3]) - max(a[1], b[1]))
	if iw <= 0 || ih <= 0 {
		return 0
	}
	inter := iw * ih
	areaA := float64(a[2]) * float64(a[3])
	areaB := float64(b[2]) * float64(b[3])
	return inter / (areaA + areaB - inter)
}

// alignBox 把视觉框从 display 像素空间回映射到 native 像素空间（D11 坐标对齐，融合正确性
// 核心）：detector 输入图 = screenshot display 空间（长边 ≤1280 缩放），a11y bounds = native
// 像素——IoU 前四分量 ×ScreenshotMeta.scale（= native_w/display_w）并就近取整，与 node 侧
// to_native/scale_coord 的回映射同制（display 640 × scale 1.148 = 734.72 → 735）。
func alignBox(vb VisualBox, scale float64) [4]int32 {
	var out [4]int32
	for i, v := range vb.Bbox {
		out[i] = int32(math.Round(float64(v) * scale))
	}
	return out
}

// flattenA11y 把 a11y 树递归扁平为融合元素列表（先序）：仅收带有效 bounds（w>0 且 h>0）
// 的节点——幽灵/零面积节点 node 侧已滤（M5 Locked-8），此处防御性再滤；无框节点自身跳过
// 但其子树照走。产出 source=a11y、confidence=1.0（原生结构证据，恒赢仲裁的基准侧）。
func flattenA11y(tree A11yTree) []FusedElement {
	out := make([]FusedElement, 0)
	var walk func(nodes []A11yNode)
	walk = func(nodes []A11yNode) {
		for i := range nodes {
			n := &nodes[i]
			if b := n.Bounds; b != nil && b[2] > 0 && b[3] > 0 {
				out = append(out, FusedElement{
					Source:     SourceA11y,
					Bounds:     *b,
					Role:       n.Role,
					Name:       n.Name,
					Confidence: 1.0,
				})
			}
			walk(n.Children)
		}
	}
	walk(tree.Nodes)
	return out
}

// fusionNeeded 判定是否需要触发视觉检测（target-aware 触发判据，D9/M3）：
//   - 空树 / 无任何带框元素 → true（a11y 全盲，必触发）；
//   - 有 target：a11y 带框元素的 name/role 命中（不区分大小写子串，与 node 侧 assert
//     contains 缺省语义一致）→ false（a11y-first 已可定位目标，不触发，SC-3）；未命中 → true。
//     命中判定只看带框元素——命中但无 bounds 的节点无法定位，不构成「充分」；
//   - 无 target：覆盖率启发（非纯 count，M3）——带框元素数 < minInteractable（稀疏）OR
//     a11y 框并集覆盖屏面积比 < coverageMin（partial-a11y：菜单有框、游戏区大片盲区，
//     count 充分但覆盖率暴露盲区）→ true。
//
// screenW/screenH 为 native 像素屏尺寸（ScreenshotMeta.native_w/native_h，与 a11y bounds 同基）。
func fusionNeeded(tree A11yTree, target string, screenW, screenH int32) bool {
	flat := flattenA11y(tree)
	if len(flat) == 0 {
		return true
	}
	if target != "" {
		return !a11yHit(flat, target)
	}
	if len(flat) < minInteractable {
		return true
	}
	return coveredFraction(flat, screenW, screenH) < coverageMin
}

// a11yHit 判 target 是否命中任一元素的 name/role（不区分大小写子串包含）。
func a11yHit(flat []FusedElement, target string) bool {
	q := strings.ToLower(target)
	for i := range flat {
		if strings.Contains(strings.ToLower(flat[i].Name), q) ||
			strings.Contains(strings.ToLower(flat[i].Role), q) {
			return true
		}
	}
	return false
}

// coveredFraction 计算元素框并集面积占屏面积的比例（[0,1]，框先裁剪到屏内）。
// 并集用坐标压缩 + 二维差分精确计算：a11y 树扁平后父容器框天然包含子控件框，朴素面积
// 求和会系统性高估覆盖率（把 partial-a11y 误判为充分、误跳过视觉），必须按并集算。
// 断点数 ≤ 2n（get_a11y_tree max_nodes 缺省 200），格子规模 O(n²) 可忽略。
func coveredFraction(flat []FusedElement, screenW, screenH int32) float64 {
	if screenW <= 0 || screenH <= 0 {
		return 0
	}
	type rect struct{ x1, y1, x2, y2 int32 }
	var rects []rect
	var xs, ys []int32
	for i := range flat {
		b := flat[i].Bounds
		x1, y1 := max(b[0], 0), max(b[1], 0)
		x2, y2 := min(b[0]+b[2], screenW), min(b[1]+b[3], screenH)
		if x2 <= x1 || y2 <= y1 {
			continue
		}
		rects = append(rects, rect{x1, y1, x2, y2})
		xs = append(xs, x1, x2)
		ys = append(ys, y1, y2)
	}
	if len(rects) == 0 {
		return 0
	}
	slices.Sort(xs)
	slices.Sort(ys)
	xs = slices.Compact(xs)
	ys = slices.Compact(ys)

	// 二维差分标记覆盖：格 (i,j) = [xs[i],xs[i+1]) × [ys[j],ys[j+1])，每框对其覆盖的
	// 索引区间四角 ±1，前缀和后 >0 即被至少一框覆盖。
	nx, ny := len(xs)-1, len(ys)-1
	diff := make([][]int32, nx+1)
	for i := range diff {
		diff[i] = make([]int32, ny+1)
	}
	for _, r := range rects {
		i1 := sort.Search(len(xs), func(i int) bool { return xs[i] >= r.x1 })
		i2 := sort.Search(len(xs), func(i int) bool { return xs[i] >= r.x2 })
		j1 := sort.Search(len(ys), func(j int) bool { return ys[j] >= r.y1 })
		j2 := sort.Search(len(ys), func(j int) bool { return ys[j] >= r.y2 })
		diff[i1][j1]++
		diff[i2][j1]--
		diff[i1][j2]--
		diff[i2][j2]++
	}
	var covered float64
	for i := 0; i < nx; i++ {
		for j := 0; j < ny; j++ {
			if i > 0 {
				diff[i][j] += diff[i-1][j]
			}
			if j > 0 {
				diff[i][j] += diff[i][j-1]
			}
			if i > 0 && j > 0 {
				diff[i][j] -= diff[i-1][j-1]
			}
			if diff[i][j] > 0 {
				covered += float64(xs[i+1]-xs[i]) * float64(ys[j+1]-ys[j])
			}
		}
	}
	return covered / (float64(screenW) * float64(screenH))
}

// merge 融合 a11y 元素与已对齐 native 的视觉框（UFO² 融合核，D7/语义#4）：
// 结果 = 全部 a11y 元素；逐视觉框与 a11y 框算 IoU，与任一 a11y 框 IoU > iouThreshold
// 即丢弃该视觉框——a11y 恒赢，语义是滤除（重叠区只留 a11y 节点）而非合并/坐标平均；
// 仅不与任何 a11y 框重叠的视觉框补为 source=vision 增量元素（role=检测类名 kind、
// caption=Florence-2 描述、confidence=检测分，name 留空归 a11y 源专用）。
// iouThreshold<=0 取 DefaultIoUThreshold（0.10 起点）；零/负面积视觉框直接丢弃。
func merge(a11yFlat []FusedElement, visualNative []VisualBoxNative, iouThreshold float64) []FusedElement {
	if iouThreshold <= 0 {
		iouThreshold = DefaultIoUThreshold
	}
	out := make([]FusedElement, 0, len(a11yFlat)+len(visualNative))
	out = append(out, a11yFlat...)
	for _, v := range visualNative {
		if v.Bbox[2] <= 0 || v.Bbox[3] <= 0 {
			continue
		}
		overlapped := false
		for i := range a11yFlat {
			if iou(v.Bbox, a11yFlat[i].Bounds) > iouThreshold {
				overlapped = true
				break
			}
		}
		if overlapped {
			continue
		}
		out = append(out, FusedElement{
			Source:     SourceVision,
			Bounds:     v.Bbox,
			Role:       v.Kind,
			Caption:    v.Caption,
			Confidence: v.Confidence,
		})
	}
	return out
}
