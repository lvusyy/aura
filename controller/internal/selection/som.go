package selection

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	_ "image/jpeg"
	"image/png"
	"math"
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
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, canvas); err != nil {
		return base64.StdEncoding.EncodeToString(rawImage), mime
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes()), "image/png"
}
