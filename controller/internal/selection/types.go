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
	"encoding/json"

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
	Type           string             `json:"type,omitempty"`
	Noul           float64            `json:"noul,omitempty"`
	HasNoul        bool               `json:"-"`
	Score          float64            `json:"score,omitempty"`
	Choice         string             `json:"choice,omitempty"`
	ChoiceIndex    int                `json:"choice_index,omitempty"`
	HasChoiceIndex bool               `json:"-"`
	Confidence     float64            `json:"confidence,omitempty"`
	Probabilities  map[string]float64 `json:"probabilities,omitempty"`
	Box            *NormalizedBox     `json:"box,omitempty"`
}

// UnmarshalJSON 解析 SystemOneAnswer 并记录 noul / choice_index 是否显式存在于 JSON 中，
// 防止 {"noul": 0.0} 被误判为未返回、或未返回 choice_index 时默认落到 0 号候选。
func (a *SystemOneAnswer) UnmarshalJSON(data []byte) error {
	type alias SystemOneAnswer
	var aux alias
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	*a = SystemOneAnswer(aux)
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err == nil {
		if _, ok := raw["noul"]; ok {
			a.HasNoul = true
		}
		if _, ok := raw["choice_index"]; ok {
			a.HasChoiceIndex = true
		} else if a.Choice != "" {
			a.ChoiceIndex = -1
		}
	}
	return nil
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
	NodeID      string `json:"node_id"`
	Target      string `json:"target"`
	ForceVision bool   `json:"force_vision"`
	WaitMs      int64  `json:"wait_ms,omitempty"`
	DeadlineMs  int64  `json:"deadline_ms"`
	Who         string `json:"who"`
	TraceID     string `json:"trace_id,omitempty"`
}

// LocateOutput 是 SelectorEngine.Locate 的结构化输出（同时携带 native 与 display 坐标防二次缩放）。
type LocateOutput struct {
	Found             bool                  `json:"found"`
	Status            string                `json:"status"` // matched | not_found | low_confidence | vision_disabled
	Source            string                `json:"source"` // a11y | vision_jev | vision_local | vision_heuristic
	Bounds            [4]int32              `json:"bounds"`
	DisplayBounds     [4]int32              `json:"display_bounds"`
	ClickPoint        [2]int32              `json:"click_point"`
	DisplayClickPoint [2]int32              `json:"display_click_point"`
	Role              string                `json:"role"`
	Name              string                `json:"name"`
	Caption           string                `json:"caption"`
	ExistenceProb     float64               `json:"existence_prob"`
	SelectionProb     float64               `json:"selection_prob"`
	Confidence        float64               `json:"confidence"`
	ScreenW           int32                 `json:"screen_w"`
	ScreenH           int32                 `json:"screen_h"`
	Scale             float64               `json:"scale"`
	SnapshotID        string                `json:"snapshot_id,omitempty"`
	LatencyMs         int64                 `json:"latency_ms"`
	Candidates        []fusion.FusedElement `json:"candidates"`
	Message           string                `json:"message"`
}

// ClickInput 是 SelectorEngine.Click 的入参。
type ClickInput struct {
	NodeID      string `json:"node_id"`
	Target      string `json:"target"`
	Button      string `json:"button"`
	DoubleClick bool   `json:"double_click,omitempty"`
	TypeText    string `json:"type_text,omitempty"`
	PressKey    string `json:"press_key,omitempty"`
	WaitMs      int64  `json:"wait_ms,omitempty"`
	ForceVision bool   `json:"force_vision"`
	Verify      bool   `json:"verify"`
	DeadlineMs  int64  `json:"deadline_ms"`
	Who         string `json:"who"`
	TraceID     string `json:"trace_id,omitempty"`
}

// ClickOutput 是 SelectorEngine.Click 的结构化输出。
type ClickOutput struct {
	Located        LocateOutput `json:"located"`
	Clicked        bool         `json:"clicked"`
	DoubleClicked  bool         `json:"double_clicked,omitempty"`
	Typed          bool         `json:"typed,omitempty"`
	KeyPressed     string       `json:"key_pressed,omitempty"`
	Verified       bool         `json:"verified"`
	Status         string       `json:"status"` // clicked | not_found | low_confidence | click_failed
	TotalLatencyMs int64        `json:"total_latency_ms"`
	Message        string       `json:"message"`
}

// AnnotateInput 是 SelectorEngine.Annotate（Set-of-Mark 视觉+无障碍标注）的入参。
type AnnotateInput struct {
	NodeID      string `json:"node_id"`
	ForceVision bool   `json:"force_vision,omitempty"`
	MaxElements int    `json:"max_elements,omitempty"`
	DeadlineMs  int64  `json:"deadline_ms,omitempty"`
	Who         string `json:"who,omitempty"`
	TraceID     string `json:"trace_id,omitempty"`
}

// AnnotatedCandidate 是 Set-of-Mark 标注后的带序号候选元素（同时携带 native 与 display 坐标）。
type AnnotatedCandidate struct {
	Index             int      `json:"index"`
	Source            string   `json:"source"`
	Role              string   `json:"role"`
	Name              string   `json:"name,omitempty"`
	Caption           string   `json:"caption,omitempty"`
	Bounds            [4]int32 `json:"bounds"`
	DisplayBounds     [4]int32 `json:"display_bounds"`
	ClickPoint        [2]int32 `json:"click_point"`
	DisplayClickPoint [2]int32 `json:"display_click_point"`
	Confidence        float64  `json:"confidence"`
}

// AnnotateOutput 是 SelectorEngine.Annotate 的结构化输出。
type AnnotateOutput struct {
	NodeID         string               `json:"node_id"`
	ScreenW        int32                `json:"screen_w"`
	ScreenH        int32                `json:"screen_h"`
	DisplayW       int32                `json:"display_w"`
	DisplayH       int32                `json:"display_h"`
	Scale          float64              `json:"scale"`
	SnapshotID     string               `json:"snapshot_id,omitempty"`
	SoMImageBase64 string               `json:"som_image_base64,omitempty"`
	SoMMime        string               `json:"som_mime,omitempty"`
	Choices        []string             `json:"choices"`
	Candidates     []AnnotatedCandidate `json:"candidates"`
	LatencyMs      int64                `json:"latency_ms"`
	Message        string               `json:"message"`
}

// WaitForInput 是 SelectorEngine.WaitFor（等待元素出现或消失）的入参。
type WaitForInput struct {
	NodeID      string `json:"node_id"`
	Target      string `json:"target"`
	State       string `json:"state,omitempty"`      // "present" (缺省，等待出现) | "absent" / "gone" (等待消失)
	TimeoutMs   int64  `json:"timeout_ms,omitempty"` // 缺省 5000，上限 30000
	ForceVision bool   `json:"force_vision,omitempty"`
	DeadlineMs  int64  `json:"deadline_ms,omitempty"`
	Who         string `json:"who,omitempty"`
	TraceID     string `json:"trace_id,omitempty"`
}

// WaitForOutput 是 SelectorEngine.WaitFor 的结构化输出。
type WaitForOutput struct {
	Satisfied bool         `json:"satisfied"`
	State     string       `json:"state"` // "present" | "absent"
	Target    string       `json:"target"`
	Polls     int          `json:"polls"`
	ElapsedMs int64        `json:"elapsed_ms"`
	Last      LocateOutput `json:"last"`
	Message   string       `json:"message"`
}
