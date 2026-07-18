// Package fusion 承载 M9 视觉融合：detector（OmniParser 外置服务）的窄接口客户端与
// a11y×视觉融合引擎（T8）。detector 是第三方外置 HTTP 服务（AGPL arm's length 隔离），
// 本包经 DetectorClient 窄接口消费之（第三方 SDK 隔离 spec，同 ObjectPutter/pveAPI 规约）：
// HTTP 细节收敛在 HTTPDetector 单文件，融合引擎/handler 依赖接口即可离线单测
// （MockDetector 替身），检测后端可替换（D3：换 Grounding DINO 等只需同接口新实现）。
//
// /detect、/healthz 契约单一真源：controller/deploy/detector/README.md。
package fusion

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"time"
)

const (
	// defaultDetectTimeout 是 HTTP client 硬超时缺省值。detector 为单 worker 串行推理队列
	// （契约：并发语义节），client 超时须涵盖「队列等待 + 单帧推理」总时长：GPU ~1s/帧下
	// 40s 留足队列冗余；CPU 变体 15–25s/帧时装配侧（T10 配置注入）应传更大值。
	defaultDetectTimeout = 40 * time.Second
	// maxDetectResponse 是 /detect 响应体读取上限（防御异常大响应撑爆控制面内存，同
	// storage.maxGetObjectSize 惯例）；正常响应为数百元素的 JSON，数十 KB 量级。
	maxDetectResponse = 8 << 20 // 8 MiB
	// maxErrorDetail 是非 2xx 错误响应体的读取上限（错误信息只需片段，如 401 的 {"detail":...}）。
	maxErrorDetail = 512
)

// ErrUnauthorized 表示 detector 拒绝了 bearer token（401，契约 C1）。调用方以 errors.Is
// 区分鉴权失败（token 配置错，重试无意义）与其他 HTTP/网络错误。
var ErrUnauthorized = errors.New("detector rejected bearer token (401 unauthorized)")

// VisualBox 是 detector 返回的单个 UI 元素检测框（/detect 响应 elements[] 的窄投影）。
//
// Bbox=[x,y,w,h]，(x,y)=左上角，坐标为**输入图像像素空间**（D11）：服务侧已把检测框映射回
// 输入图原始分辨率，与 a11y 树坐标同基对齐，消费方（T8 融合 IoU）零换算。契约 wire 值为
// 浮点像素，解码时就近取整为 int32（融合匹配无需亚像素精度）。
type VisualBox struct {
	Bbox       [4]int32 // [x, y, w, h] 输入图像像素空间
	Kind       string   // 检测头类名（wire 字段 type）；OmniParser v2 icon_detect 恒 "icon"，换权重后为对应类集，勿硬编码假设
	Caption    string   // Florence-2 功能语义描述（英文短语，如 "settings gear icon"）
	Confidence float64  // 检测置信度 0–1
}

// DetectorClient 是融合引擎消费视觉检测的最小窄接口：image=图像原始字节（非 base64），
// mime=Content-Type（image/png|image/jpeg|image/webp，由服务端校验）。返回框按 detector
// 输出序，无排序保证。融合引擎（T8）与装配层（T10）只依赖本接口，不触 HTTP 细节。
type DetectorClient interface {
	Detect(ctx context.Context, image []byte, mime string) ([]VisualBox, error)
}

// 编译期接口符合性断言：实现漂移在此编译失败，不外泄到消费方。
var (
	_ DetectorClient = (*HTTPDetector)(nil)
	_ DetectorClient = (*MockDetector)(nil)
)

// HTTPDetector 是 DetectorClient 的 HTTP 实现：POST {endpoint}/detect + bearer 鉴权。
// controller 侧用标准库 net/http（node 侧 upload.rs 的 hyper-util 选型是为规避 rustls 双
// CryptoProvider panic，Go 无此约束）；endpoint 为 detector NodePort 直连地址（无 LB，如
// http://<k8s-node>:30081），token 对应服务端 AURA_DETECTOR_TOKEN——二者由装配侧
// （T10）从 env/flag 注入，不落码。
type HTTPDetector struct {
	endpoint string
	token    string
	hc       *http.Client
}

// NewHTTPDetector 构造 HTTP detector 客户端。timeout 是整次请求的硬超时（含连接、排队、
// 推理与响应读取），<=0 取 defaultDetectTimeout；endpoint 尾部斜杠自动剥除。
func NewHTTPDetector(endpoint, token string, timeout time.Duration) *HTTPDetector {
	if timeout <= 0 {
		timeout = defaultDetectTimeout
	}
	return &HTTPDetector{
		endpoint: strings.TrimRight(endpoint, "/"),
		token:    token,
		hc:       &http.Client{Timeout: timeout},
	}
}

// detectResponse 镜像 /detect 200 响应体（契约 README「HTTP 契约」节）。契约另含
// image_w/image_h（输入图真实分辨率，坐标对齐基准），但 bbox 已是输入图像像素空间、
// 与 a11y 树同基，窄接口无需透出——此处不解码（需要时再扩展）。
type detectResponse struct {
	Elements []detectElement `json:"elements"`
}

// detectElement 镜像 elements[] 单项。bbox 契约为浮点像素（左上角 x,y + w,h），先以
// float64 解码再就近取整进 VisualBox——直接以 [4]int32 解码时 encoding/json 遇 10.5 类
// 浮点值会整包报错。
type detectElement struct {
	Bbox       [4]float64 `json:"bbox"`
	Type       string     `json:"type"`
	Caption    string     `json:"caption"`
	Confidence float64    `json:"confidence"`
}

// Detect 实现 DetectorClient：POST {endpoint}/detect，body=图像原始字节（非 multipart、
// 非 base64），Content-Type=mime，Authorization: Bearer <token>。
//
// 显式失败语义：单次调用、快失败、不重试——detector 是单 worker 串行队列，client 侧重试
// 只会加深积压，重试/降级决策归调用方（T8 融合引擎按 job 语义处置）。401 以 ErrUnauthorized
// 可区分（errors.Is）；其余非 2xx 携带状态码与响应片段。
func (d *HTTPDetector) Detect(ctx context.Context, image []byte, mime string) ([]VisualBox, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.endpoint+"/detect", bytes.NewReader(image))
	if err != nil {
		return nil, fmt.Errorf("detector: build request: %w", err)
	}
	req.Header.Set("Content-Type", mime)
	req.Header.Set("Authorization", "Bearer "+d.token)

	resp, err := d.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("detector: POST /detect: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		return nil, fmt.Errorf("detector: %w", ErrUnauthorized)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorDetail))
		return nil, fmt.Errorf("detector: /detect returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(detail)))
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxDetectResponse))
	if err != nil {
		return nil, fmt.Errorf("detector: read response: %w", err)
	}
	var dr detectResponse
	if err := json.Unmarshal(body, &dr); err != nil {
		return nil, fmt.Errorf("detector: decode response: %w", err)
	}

	boxes := make([]VisualBox, 0, len(dr.Elements))
	for _, e := range dr.Elements {
		var bb [4]int32
		for i, v := range e.Bbox {
			bb[i] = int32(math.Round(v))
		}
		boxes = append(boxes, VisualBox{Bbox: bb, Kind: e.Type, Caption: e.Caption, Confidence: e.Confidence})
	}
	return boxes, nil
}

// MockDetector 是 DetectorClient 的离线测试替身（T8 融合引擎单测注入，不触网）：
// 返回固定框或固定错误。
type MockDetector struct {
	Boxes []VisualBox
	Err   error
}

// Detect 实现 DetectorClient：Err 非 nil 时返回之，否则返回 Boxes。
func (m *MockDetector) Detect(_ context.Context, _ []byte, _ string) ([]VisualBox, error) {
	if m.Err != nil {
		return nil, m.Err
	}
	return m.Boxes, nil
}
