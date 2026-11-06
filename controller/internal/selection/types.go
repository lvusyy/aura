// Package selection 实现基于 Jev (TypeSafe System One / 本地 k3s GPU 推理) 的快速 UI 元素定位与一步点击引擎。
//
// 核心分层（Three-Tier Hybrid Selection）：
//   - Tier 0 (0ms 视觉开销)：A11y 树确定性语义匹配（精确名/角色匹配即直接返回原生坐标，零截图、零 GPU 开销）；
//   - Tier 1 (快速几何 + Jev Choice & Noul)：YOLO 毫秒级几何框 (?caption=0) + A11y 合并 → 候选集构造 →
//     Jev 单次并发评估 Choice（候选精排）+ Noul（目标存在性概率门控）；
//   - Tier 2 (Jev Box 开放视觉定位兜底)：当候选集为空或全部低置信度且 EnableBox=true 时，调用 Jev Box
//     原语（本地 Florence-2 Phrase Grounding / 云端 SystemOne Box）直接回归归一化边界框。
package selection

import (
	"context"

	"github.com/aura/controller/internal/fusion"
)

const (
	QuestionNoul   = "noul"
	QuestionChoice = "choice"
	QuestionScore  = "score"
	QuestionBox    = "box"
)

// StateBlock 表示传给 /v1/systemone 的多模态状态块（文本或 base64 图像）。
type StateBlock struct {
	Type   string       `json:"type"`
	Text   string       `json:"text,omitempty"`
	Source *ImageSource `json:"source,omitempty"`
}

// ImageSource 表示 base64 图像载荷。
type ImageSource struct {
	Type      string `json:"type"`       // "base64"
	MediaType string `json:"media_type"` // "image/webp" | "image/png" | "image/jpeg"
	Data      string `json:"data"`
}

// QuestionSpec 定义单条 System One 原语问题。
type QuestionSpec struct {
	Type     string   `json:"type"`               // noul | choice | score | box
	Question string   `json:"question"`           // 问题提示词
	Choices  []string `json:"choices,omitempty"`  // choice 类型的候选列表
}

// SystemOneRequest 是 POST /v1/systemone 的请求体（云端 TypeSafe AI 与本地 k3s GPU 同构）。
type SystemOneRequest struct {
	Model     string                  `json:"model"`
	State     []StateBlock            `json:"state"`
	Questions map[string]QuestionSpec `json:"questions"`
	Target    string                  `json:"target,omitempty"`
}

// NormalizedBox 是 Jev Box 原语返回的 [0,1] 归一化边界框。
type NormalizedBox struct {
	Top    float64 `json:"top"`
	Bottom float64 `json:"bottom"`
	Left   float64 `json:"left"`
	Right  float64 `json:"right"`
}

// SystemOneAnswer 是单个问题的返回结果（兼容云端顶层/嵌套字段与本地 GPU 端）。
type SystemOneAnswer struct {
	Type          string             `json:"type,omitempty"`
	Noul          float64            `json:"noul,omitempty"`
	Score         float64            `json:"score,omitempty"`
	Choice        string             `json:"choice,omitempty"`
	ChoiceIndex   int                `json:"choice_index,omitempty"`
	Confidence    float64            `json:"confidence,omitempty"`
	Probabilities map[string]float64 `json:"probabilities,omitempty"`
	Box           *NormalizedBox     `json:"box,omitempty"`
}

// SystemOneResponse 是 POST /v1/systemone 的响应体。
type SystemOneResponse struct {
	Answers   map[string]SystemOneAnswer `json:"answers"`
	Model     string                     `json:"model,omitempty"`
	Device    string                     `json:"device,omitempty"`
	LatencyMs float64                    `json:"latency_ms,omitempty"`
}

// JevClient 是 Jev 原语推理窄接口（HTTPJevClient 与单测替身均实现之）。
type JevClient interface {
	Evaluate(ctx context.Context, req SystemOneRequest) (*SystemOneResponse, error)
}

// LocateInput 是 SelectorEngine.Locate 的入参。
type LocateInput struct {
	NodeID      string
	Target      string
	ForceVision bool
	DeadlineMs  int64
	Who         string
}

// LocateOutput 是 SelectorEngine.Locate 的结构化输出（同时携带 native 与 display 坐标防二次缩放）。
type LocateOutput struct {
	Found             bool
	Status            string // matched | not_found | low_confidence | vision_disabled
	Source            string // a11y | vision_jev | vision_local | vision_heuristic
	Bounds            [4]int32
	DisplayBounds     [4]int32
	ClickPoint        [2]int32
	DisplayClickPoint [2]int32
	Role              string
	Name              string
	Caption           string
	ExistenceProb     float64
	SelectionProb     float64
	Confidence        float64
	ScreenW           int32
	ScreenH           int32
	Scale             float64
	SnapshotID        string
	LatencyMs         int64
	Candidates        []fusion.FusedElement
	Message           string
}

// ClickInput 是 SelectorEngine.Click 的入参。
type ClickInput struct {
	NodeID      string
	Target      string
	Button      string
	ForceVision bool
	Verify      bool
	DeadlineMs  int64
	Who         string
}

// ClickOutput 是 SelectorEngine.Click 的结构化输出。
type ClickOutput struct {
	Located        LocateOutput
	Clicked        bool
	Verified       bool
	Status         string // clicked | not_found | low_confidence | click_failed
	TotalLatencyMs int64
	Message        string
}
