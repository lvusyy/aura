package selection

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"hash/fnv"
	"image"
	"image/color"
	"image/draw"
	_ "image/jpeg"
	"image/png"
	"math"
	"math/bits"
	"strconv"
	"strings"

	"github.com/aura/controller/internal/fusion"
)

// NativeToDisplayBounds 将原生像素边界框 [x,y,w,h] 按 scale (native/display) 转换为截图显示空间边界框。
func NativeToDisplayBounds(b [4]int32, scale float64) [4]int32 {
	if scale <= 0 || !math.IsInf(scale, 0) && math.IsNaN(scale) {
		scale = 1.0
	}
	return [4]int32{
		int32(math.Round(float64(b[0]) / scale)),
		int32(math.Round(float64(b[1]) / scale)),
		int32(math.Round(float64(b[2]) / scale)),
		int32(math.Round(float64(b[3]) / scale)),
	}
}

// CenterPoint 计算边界框 [x,y,w,h] 的中心点 [cx,cy]。
func CenterPoint(b [4]int32) [2]int32 {
	return [2]int32{
		b[0] + b[2]/2,
		b[1] + b[3]/2,
	}
}

// FormatCandidateChoices 将候选 FusedElement 列表格式化为 Jev Choice 选项切片。
// 注意：@(x,y,w,h) 使用 display 空间像素（与传入 /v1/systemone 的截图分辨率严格同基），
// 使云端与本地 GPU 选择器均可直接在输入截图坐标系内对齐候选区域。
func FormatCandidateChoices(candidates []fusion.FusedElement, scale float64) []string {
	out := make([]string, 0, len(candidates))
	for i, c := range candidates {
		db := NativeToDisplayBounds(c.Bounds, scale)
		role := strings.TrimSpace(c.Role)
		if role == "" {
			role = c.Source
		}
		label := strings.TrimSpace(c.Name)
		if label == "" {
			label = strings.TrimSpace(c.Caption)
		}
		if label == "" {
			label = "unnamed"
		}
		out = append(out, fmt.Sprintf("%d: [%s] %s @(%d,%d,%d,%d)", i, role, label, db[0], db[1], db[2], db[3]))
	}
	return out
}

// RenderSoMOverlay 若输入图像为标准库可解码格式（PNG/JPEG），在候选框位置绘制高对比边框并编码为 base64 PNG；
// 若为 WebP 等格式则原样回传原始图像字节与 mime（零第三方图像库依赖）。
func RenderSoMOverlay(rawImage []byte, mime string, candidates []fusion.FusedElement, scale float64) (string, string) {
	if len(rawImage) == 0 {
		return "", mime
	}
	img, _, err := image.Decode(bytes.NewReader(rawImage))
	if err != nil || len(candidates) == 0 {
		return base64.StdEncoding.EncodeToString(rawImage), mime
	}
	b := img.Bounds()
	canvas := image.NewRGBA(b)
	draw.Draw(canvas, b, img, b.Min, draw.Src)

	palette := []color.RGBA{
		{R: 255, G: 59, B: 48, A: 255},
		{R: 0, G: 122, B: 255, A: 255},
		{R: 52, G: 199, B: 89, A: 255},
		{R: 255, G: 149, B: 0, A: 255},
	}

	for i, el := range candidates {
		db := NativeToDisplayBounds(el.Bounds, scale)
		x1 := max(b.Min.X, int(db[0]))
		y1 := max(b.Min.Y, int(db[1]))
		x2 := min(b.Max.X-1, int(db[0]+db[2]))
		y2 := min(b.Max.Y-1, int(db[1]+db[3]))
		if x2 <= x1 || y2 <= y1 {
			continue
		}
		col := palette[i%len(palette)]
		for x := x1; x <= x2; x++ {
			canvas.Set(x, y1, col)
			canvas.Set(x, max(b.Min.Y, y1+1), col)
			canvas.Set(x, y2, col)
			canvas.Set(x, min(b.Max.Y-1, y2-1), col)
		}
		for y := y1; y <= y2; y++ {
			canvas.Set(x1, y, col)
			canvas.Set(min(b.Max.X-1, x1+1), y, col)
			canvas.Set(x2, y, col)
			canvas.Set(max(b.Min.X, x2-1), y, col)
		}
		drawIndexBadge(canvas, b, x1, y1, i, col)
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, canvas); err != nil {
		return base64.StdEncoding.EncodeToString(rawImage), mime
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes()), "image/png"
}

// digitGlyphs 是 3x5 极简数字点阵，用于在 SoM 候选框左上角绘制候选序号角标。
var digitGlyphs = [10][5]uint8{
	{0b111, 0b101, 0b101, 0b101, 0b111}, // 0
	{0b010, 0b110, 0b010, 0b010, 0b111}, // 1
	{0b111, 0b001, 0b111, 0b100, 0b111}, // 2
	{0b111, 0b001, 0b111, 0b001, 0b111}, // 3
	{0b101, 0b101, 0b111, 0b001, 0b001}, // 4
	{0b111, 0b100, 0b111, 0b001, 0b111}, // 5
	{0b111, 0b100, 0b111, 0b101, 0b111}, // 6
	{0b111, 0b001, 0b010, 0b010, 0b010}, // 7
	{0b111, 0b101, 0b111, 0b101, 0b111}, // 8
	{0b111, 0b101, 0b111, 0b001, 0b111}, // 9
}

func drawIndexBadge(canvas *image.RGBA, bounds image.Rectangle, x1, y1, idx int, bg color.RGBA) {
	label := strconv.Itoa(idx)
	badgeW := len(label)*4 + 3
	badgeH := 7
	bx0 := max(bounds.Min.X, x1)
	by0 := max(bounds.Min.Y, y1)
	bx1 := min(bounds.Max.X-1, bx0+badgeW)
	by1 := min(bounds.Max.Y-1, by0+badgeH)
	for y := by0; y <= by1; y++ {
		for x := bx0; x <= bx1; x++ {
			canvas.Set(x, y, bg)
		}
	}
	fg := color.RGBA{R: 255, G: 255, B: 255, A: 255}
	for ci, ch := range label {
		if ch < '0' || ch > '9' {
			continue
		}
		glyph := digitGlyphs[ch-'0']
		gx := bx0 + 2 + ci*4
		gy := by0 + 1
		for row := 0; row < 5; row++ {
			for col := 0; col < 3; col++ {
				if (glyph[row]>>(2-col))&1 == 1 {
					px, py := gx+col, gy+row
					if px >= bounds.Min.X && px < bounds.Max.X && py >= bounds.Min.Y && py < bounds.Max.Y {
						canvas.Set(px, py, fg)
					}
				}
			}
		}
	}
}

// ComputeExactHash 计算图像字节的 64 位 FNV-1a 摘要，用于同屏候选框缓存键。
func ComputeExactHash(rawImage []byte) uint64 {
	if len(rawImage) == 0 {
		return 0
	}
	h := fnv.New64a()
	_, _ = h.Write(rawImage)
	return h.Sum64()
}

// ComputeDHash 计算图像的 64 位感知差分哈希（9x8 亮度网格水平梯度）。
// 若输入格式可被标准库解码（PNG/JPEG），计算真实像素级 dHash；
// 若为 WebP 等压缩流，计算 65 段分块强度梯度哈希，供点击前后视觉变化判定。
func ComputeDHash(rawImage []byte) (uint64, bool) {
	if len(rawImage) == 0 {
		return 0, false
	}
	if img, _, err := image.Decode(bytes.NewReader(rawImage)); err == nil {
		b := img.Bounds()
		w, h := b.Dx(), b.Dy()
		if w >= 9 && h >= 8 {
			var grid [8][9]float64
			for gy := 0; gy < 8; gy++ {
				y0 := b.Min.Y + (gy*h)/8
				y1 := max(y0+1, b.Min.Y+((gy+1)*h)/8)
				for gx := 0; gx < 9; gx++ {
					x0 := b.Min.X + (gx*w)/9
					x1 := max(x0+1, b.Min.X+((gx+1)*w)/9)
					var sum float64
					var count int
					stepY := max(1, (y1-y0)/4)
					stepX := max(1, (x1-x0)/4)
					for y := y0; y < y1; y += stepY {
						for x := x0; x < x1; x += stepX {
							r, g, bl, _ := img.At(x, y).RGBA()
							sum += 0.299*float64(r) + 0.587*float64(g) + 0.114*float64(bl)
							count++
						}
					}
					if count > 0 {
						grid[gy][gx] = sum / float64(count)
					}
				}
			}
			var hash uint64
			var bit uint
			for gy := 0; gy < 8; gy++ {
				for gx := 0; gx < 8; gx++ {
					if grid[gy][gx] > grid[gy][gx+1] {
						hash |= 1 << bit
					}
					bit++
				}
			}
			return hash, true
		}
	}
	payload := rawImage
	if len(payload) > 12 {
		payload = payload[12:]
	}
	if len(payload) < 65 {
		return ComputeExactHash(rawImage), true
	}
	var segAvg [65]float64
	segLen := len(payload) / 65
	for i := 0; i < 65; i++ {
		start := i * segLen
		end := start + segLen
		if i == 64 {
			end = len(payload)
		}
		var sum uint64
		for _, b := range payload[start:end] {
			sum += uint64(b)
		}
		segAvg[i] = float64(sum) / float64(max(1, end-start))
	}
	var hash uint64
	for i := 0; i < 64; i++ {
		if segAvg[i] > segAvg[i+1] {
			hash |= 1 << uint(i)
		}
	}
	return hash, true
}

// HammingDistance 返回两个 64 位感知哈希之间的汉明距离（0–64）。
func HammingDistance(a, b uint64) int {
	return bits.OnesCount64(a ^ b)
}

// DefaultCandidateIoUThreshold 是选择引擎合并 A11y 与视觉候选框的默认 IoU 阈值。
// 相比全屏融合的 0.10（父容器极易吞没内部视觉按钮），0.55 仅在两者指向同一控件尺度的重叠时合并，
// 既避免大面板吞没子控件，又消除同一控件的 A11y+YOLO 双重标号。
const (
	DefaultCandidateIoUThreshold = 0.55
	intraVisionNMSIoUThreshold   = 0.70
)

// ComputeIoU 计算两个 [x,y,w,h] 边界框的交并比（Intersection over Union，[0,1]）。
func ComputeIoU(a, b [4]int32) float64 {
	return fusion.IoU(a, b)
}

// MergeCandidatesIoU 执行视觉框内部 NMS 去重（IoU >= 0.70 保留高置信度框）与跨源 A11y×Vision 融合（IoU >= iouThreshold）：
//   - 当视觉框与 A11y 候选框高度重合（IoU >= iouThreshold）时，保留 A11y 原生精确边界与 Role/Name，
//     并将视觉框的非空 Caption 回填到 A11y 候选（补齐无文本图标按钮的视觉描述）；
//   - 当视觉框与所有 A11y 候选框的 IoU 均低于阈值时，作为独立视觉候选追加，确保大容器面板不会误吞内部自绘控件。
func MergeCandidatesIoU(a11yFlat []fusion.FusedElement, visualNative []fusion.VisualBoxNative, iouThreshold float64) []fusion.FusedElement {
	if iouThreshold <= 0 {
		iouThreshold = DefaultCandidateIoUThreshold
	}
	out := make([]fusion.FusedElement, len(a11yFlat), len(a11yFlat)+len(visualNative))
	copy(out, a11yFlat)

	// 1. 视觉框内部按置信度降序做 NMS（消除同一控件被 YOLO 检出多个高度重叠框导致的 SoM 标号膨胀）
	dedupVis := make([]fusion.VisualBoxNative, 0, len(visualNative))
	for _, v := range visualNative {
		if v.Bbox[2] <= 0 || v.Bbox[3] <= 0 {
			continue
		}
		suppressed := false
		for i := range dedupVis {
			if ComputeIoU(v.Bbox, dedupVis[i].Bbox) >= intraVisionNMSIoUThreshold {
				if v.Confidence > dedupVis[i].Confidence {
					if v.Caption == "" {
						v.Caption = dedupVis[i].Caption
					}
					dedupVis[i] = v
				} else if dedupVis[i].Caption == "" && v.Caption != "" {
					dedupVis[i].Caption = v.Caption
				}
				suppressed = true
				break
			}
		}
		if !suppressed {
			dedupVis = append(dedupVis, v)
		}
	}

	// 2. 跨源 A11y × Vision 融合（保留 A11y 框并回填 Caption；非重叠视觉框追加）
	for _, v := range dedupVis {
		bestIdx := -1
		bestIoU := 0.0
		for i := 0; i < len(a11yFlat); i++ {
			ov := ComputeIoU(v.Bbox, out[i].Bounds)
			if ov >= iouThreshold && ov > bestIoU {
				bestIoU = ov
				bestIdx = i
			}
		}
		if bestIdx >= 0 {
			if out[bestIdx].Caption == "" && strings.TrimSpace(v.Caption) != "" {
				out[bestIdx].Caption = strings.TrimSpace(v.Caption)
			}
			continue
		}
		out = append(out, fusion.FusedElement{
			Source:     fusion.SourceVision,
			Bounds:     v.Bbox,
			Role:       v.Kind,
			Caption:    v.Caption,
			Confidence: v.Confidence,
		})
	}
	return out
}
