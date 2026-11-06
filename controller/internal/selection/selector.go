package selection

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aura/controller/internal/fusion"
	"github.com/aura/controller/internal/store"
)

const (
	defaultLocateDeadlineMs = 25_000
	defaultClickDeadlineMs  = 30_000
)

// SelectorEngine 编排三层混合定位（Tier 0 A11y → Tier 1 YOLO+Jev Choice/Noul → Tier 2 Jev Box）与一步点击。
type SelectorEngine struct {
	disp                 fusion.Dispatcher
	st                   *store.PGStore
	defaultLocalEndpoint string
	localDetectorToken   string

	mu          sync.RWMutex
	memSettings store.VisionSettings
	jevOverride JevClient
	detOverride fusion.DetectorClient
}

// NewSelectorEngine 构造 SelectorEngine。st 可为 nil（纯内存模式兜底）。
func NewSelectorEngine(
	disp fusion.Dispatcher,
	st *store.PGStore,
	defaultLocalEndpoint string,
	localDetectorToken string,
) *SelectorEngine {
	return &SelectorEngine{
		disp:                 disp,
		st:                   st,
		defaultLocalEndpoint: strings.TrimRight(strings.TrimSpace(defaultLocalEndpoint), "/"),
		localDetectorToken:   strings.TrimSpace(localDetectorToken),
		memSettings:          store.DefaultVisionSettings(defaultLocalEndpoint),
	}
}

// WithOverrides 供离线单元测试注入 Mock Detector 与 Mock JevClient。
func (e *SelectorEngine) WithOverrides(det fusion.DetectorClient, jev JevClient) *SelectorEngine {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.detOverride = det
	e.jevOverride = jev
	return e
}

// GetSettings 获取当前生效的 VisionSettings（优先读 PG，未初始化时返回默认配置；无 PG 时读内存）。
func (e *SelectorEngine) GetSettings(ctx context.Context) (store.VisionSettings, error) {
	if e.st != nil {
		cfg, err := e.st.GetVisionSettings(ctx)
		if err == nil {
			if cfg.LocalEndpoint == "" && e.defaultLocalEndpoint != "" {
				cfg.LocalEndpoint = e.defaultLocalEndpoint
			}
			return cfg, nil
		}
		if store.IsNotFound(err) {
			return store.DefaultVisionSettings(e.defaultLocalEndpoint), nil
		}
		return store.VisionSettings{}, err
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.memSettings, nil
}

// ValidateAndNormalizeSettings 校验并规整 VisionSettings 字段。
func ValidateAndNormalizeSettings(in store.VisionSettings, defaultLocalEndpoint string) (store.VisionSettings, error) {
	out := in
	out.Mode = strings.ToLower(strings.TrimSpace(out.Mode))
	if out.Mode == "" {
		out.Mode = "auto"
	}
	switch out.Mode {
	case "disabled", "local", "cloud", "auto":
	default:
		return store.VisionSettings{}, fmt.Errorf("invalid mode %q (must be disabled, local, cloud, or auto)", out.Mode)
	}

	out.LocalEndpoint = strings.TrimRight(strings.TrimSpace(out.LocalEndpoint), "/")
	if out.LocalEndpoint == "" {
		out.LocalEndpoint = strings.TrimRight(strings.TrimSpace(defaultLocalEndpoint), "/")
		if out.LocalEndpoint == "" {
			out.LocalEndpoint = "http://192.168.22.225:30808"
		}
	}
	out.CloudEndpoint = strings.TrimRight(strings.TrimSpace(out.CloudEndpoint), "/")
	if out.CloudEndpoint == "" {
		out.CloudEndpoint = "https://api.typesafe.ai/v1/systemone"
	}
	out.Model = strings.TrimSpace(out.Model)
	if out.Model == "" {
		out.Model = "jev-latest"
	}
	out.CaptionMode = strings.ToLower(strings.TrimSpace(out.CaptionMode))
	if out.CaptionMode == "" {
		out.CaptionMode = "none"
	}
	if out.CaptionMode != "none" && out.CaptionMode != "florence" {
		return store.VisionSettings{}, fmt.Errorf("invalid caption_mode %q (must be none or florence)", out.CaptionMode)
	}
	if math.IsNaN(out.NoulThreshold) || out.NoulThreshold <= 0 {
		out.NoulThreshold = 0.50
	}
	if out.NoulThreshold > 1.0 {
		out.NoulThreshold = 1.0
	}
	return out, nil
}

// UpdateSettings 校验并持久化 VisionSettings（支持 PG 乐观并发与无 PG 内存兜底）。
func (e *SelectorEngine) UpdateSettings(
	ctx context.Context,
	in store.VisionSettings,
	apiKeyAction string,
	newAPIKey string,
) (store.VisionSettings, error) {
	norm, err := ValidateAndNormalizeSettings(in, e.defaultLocalEndpoint)
	if err != nil {
		return store.VisionSettings{}, err
	}
	action := strings.ToLower(strings.TrimSpace(apiKeyAction))
	if action == "" {
		action = "keep"
	}
	if action != "keep" && action != "set" && action != "clear" {
		return store.VisionSettings{}, fmt.Errorf("invalid api_key_action %q (must be keep, set, or clear)", apiKeyAction)
	}

	if e.st != nil {
		return e.st.UpsertVisionSettings(ctx, norm, action, newAPIKey)
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	if norm.Revision > 0 && e.memSettings.Revision > 0 && norm.Revision != e.memSettings.Revision {
		return store.VisionSettings{}, store.ErrVisionRevisionConflict
	}
	switch action {
	case "set":
		norm.APIKey = strings.TrimSpace(newAPIKey)
		norm.APIKeyHint = store.MaskAPIKeyHint(norm.APIKey)
	case "clear":
		norm.APIKey = ""
		norm.APIKeyHint = ""
	case "keep":
		norm.APIKey = e.memSettings.APIKey
		norm.APIKeyHint = e.memSettings.APIKeyHint
	}
	norm.Revision = e.memSettings.Revision + 1
	norm.UpdatedAt = time.Now().UTC()
	e.memSettings = norm
	return norm, nil
}

// TestSettings 测试草稿配置（当 apiKeyAction="keep" 时自动补齐已存的 APIKey）。
func (e *SelectorEngine) TestSettings(
	ctx context.Context,
	draft store.VisionSettings,
	apiKeyAction string,
	newAPIKey string,
) (ProbeResult, error) {
	norm, err := ValidateAndNormalizeSettings(draft, e.defaultLocalEndpoint)
	if err != nil {
		return ProbeResult{}, err
	}
	action := strings.ToLower(strings.TrimSpace(apiKeyAction))
	if action == "" {
		action = "keep"
	}
	switch action {
	case "set":
		norm.APIKey = strings.TrimSpace(newAPIKey)
	case "clear":
		norm.APIKey = ""
	case "keep":
		cur, err := e.GetSettings(ctx)
		if err == nil {
			norm.APIKey = cur.APIKey
		}
	default:
		return ProbeResult{}, fmt.Errorf("invalid api_key_action %q", apiKeyAction)
	}
	return ProbeVisionSettings(ctx, norm, e.localDetectorToken), nil
}

// nodeEnv 镜像节点统一信封。
type nodeEnv struct {
	OK    bool            `json:"ok"`
	Data  json.RawMessage `json:"data"`
	Error *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

type shotPayload struct {
	ImageBase64 string `json:"image_base64"`
	Mime        string `json:"mime"`
	Meta        struct {
		NativeW  int32   `json:"native_w"`
		NativeH  int32   `json:"native_h"`
		DisplayW int32   `json:"display_w"`
		DisplayH int32   `json:"display_h"`
		Scale    float64 `json:"scale"`
	} `json:"meta"`
}

func (e *SelectorEngine) dispatchNodeTool(ctx context.Context, nodeID, tool string, args []byte, deadlineMs int64, who string) (json.RawMessage, error) {
	if e.disp == nil {
		return nil, errors.New("selection: dispatcher not configured")
	}
	if len(args) == 0 {
		args = []byte("{}")
	}
	resp, code, err := e.disp.Dispatch(ctx, nodeID, tool, args, deadlineMs, who, "")
	if code != "" || err != nil {
		return nil, fmt.Errorf("dispatch %s to node %s failed (code=%s): %v", tool, nodeID, code, err)
	}
	var env nodeEnv
	if err := json.Unmarshal(resp.GetJsonEnvelope(), &env); err != nil {
		return nil, fmt.Errorf("decode %s envelope: %w", tool, err)
	}
	if !env.OK {
		if env.Error != nil {
			return nil, fmt.Errorf("%s failed: %s: %s", tool, env.Error.Code, env.Error.Message)
		}
		return nil, fmt.Errorf("%s failed: envelope not ok", tool)
	}
	return env.Data, nil
}

// matchA11yCandidate 在扁平化 A11y 元素中寻找最佳语义匹配。
func matchA11yCandidate(flat []fusion.FusedElement, target string) (fusion.FusedElement, float64, bool) {
	q := strings.ToLower(strings.TrimSpace(target))
	if q == "" || len(flat) == 0 {
		return fusion.FusedElement{}, 0, false
	}
	var best fusion.FusedElement
	bestScore := 0.0
	for _, el := range flat {
		name := strings.ToLower(strings.TrimSpace(el.Name))
		role := strings.ToLower(strings.TrimSpace(el.Role))
		score := 0.0
		switch {
		case name != "" && name == q:
			score = 1.0
		case name != "" && strings.Contains(name, q):
			score = 0.88
		case role != "" && (role == q || (name != "" && strings.Contains(role+" "+name, q))):
			score = 0.86
		case name != "" && strings.Contains(q, name) && len(name) >= 2:
			score = 0.78
		}
		if score > bestScore {
			bestScore = score
			best = el
		}
	}
	return best, bestScore, bestScore > 0
}

// Locate 执行三层混合元素定位。
func (e *SelectorEngine) Locate(ctx context.Context, in LocateInput) (LocateOutput, error) {
	t0 := time.Now()
	target := strings.TrimSpace(in.Target)
	if in.NodeID == "" {
		return LocateOutput{}, errors.New("node_id is required")
	}
	if target == "" {
		return LocateOutput{}, errors.New("target is required")
	}
	deadlineMs := in.DeadlineMs
	if deadlineMs <= 0 {
		deadlineMs = defaultLocateDeadlineMs
	}
	who := in.Who
	if who == "" {
		who = "console"
	}

	cfg, err := e.GetSettings(ctx)
	if err != nil {
		cfg = store.DefaultVisionSettings(e.defaultLocalEndpoint)
	}

	// 1. 采集 A11y 树（容错：若平台不支持 A11y 或采集失败，不阻断后续视觉分支）
	var flatA11y []fusion.FusedElement
	if treeRaw, aerr := e.dispatchNodeTool(ctx, in.NodeID, "get_a11y_tree", nil, deadlineMs, who); aerr == nil {
		var tree fusion.A11yTree
		if json.Unmarshal(treeRaw, &tree) == nil {
			flatA11y = fusion.FlattenA11y(tree)
		}
	}

	// Tier 0: 若未强制走视觉且 A11y 命中高置信度元素，直接毫秒级返回（零截图、零 GPU 开销）
	if !in.ForceVision {
		if matched, score, ok := matchA11yCandidate(flatA11y, target); ok && score >= 0.85 {
			cp := CenterPoint(matched.Bounds)
			return LocateOutput{
				Found:             true,
				Status:            "matched",
				Source:            "a11y",
				Bounds:            matched.Bounds,
				DisplayBounds:     matched.Bounds,
				ClickPoint:        cp,
				DisplayClickPoint: cp,
				Role:              matched.Role,
				Name:              matched.Name,
				ExistenceProb:     1.0,
				SelectionProb:     score,
				Confidence:        score,
				Scale:             1.0,
				LatencyMs:         time.Since(t0).Milliseconds(),
				Candidates:        flatA11y,
				Message:           fmt.Sprintf("A11y 确定性命中 (%s %q)", matched.Role, matched.Name),
			}, nil
		}
	}

	// 若视觉模式被显式禁用且未强制开启
	if cfg.Mode == "disabled" && !in.ForceVision {
		if matched, score, ok := matchA11yCandidate(flatA11y, target); ok && score >= 0.50 {
			cp := CenterPoint(matched.Bounds)
			return LocateOutput{
				Found:             true,
				Status:            "matched",
				Source:            "a11y",
				Bounds:            matched.Bounds,
				DisplayBounds:     matched.Bounds,
				ClickPoint:        cp,
				DisplayClickPoint: cp,
				Role:              matched.Role,
				Name:              matched.Name,
				ExistenceProb:     score,
				SelectionProb:     score,
				Confidence:        score,
				Scale:             1.0,
				LatencyMs:         time.Since(t0).Milliseconds(),
				Candidates:        flatA11y,
				Message:           "视觉已禁用，使用 A11y 子串匹配结果",
			}, nil
		}
		return LocateOutput{
			Found:      false,
			Status:     "vision_disabled",
			LatencyMs:  time.Since(t0).Milliseconds(),
			Candidates: flatA11y,
			Message:    "A11y 未命中目标且视觉推理当前设为禁用 (disabled)",
		}, nil
	}

	// 2. 采集节点屏幕截图
	shotRaw, err := e.dispatchNodeTool(ctx, in.NodeID, "screenshot", nil, deadlineMs, who)
	if err != nil {
		return LocateOutput{}, fmt.Errorf("locate screenshot: %w", err)
	}
	var shot shotPayload
	if err := json.Unmarshal(shotRaw, &shot); err != nil {
		return LocateOutput{}, fmt.Errorf("decode screenshot payload: %w", err)
	}
	imgBytes, err := base64.StdEncoding.DecodeString(shot.ImageBase64)
	if err != nil || len(imgBytes) == 0 {
		return LocateOutput{}, errors.New("locate: empty or invalid screenshot image")
	}
	meta := shot.Meta
	if meta.Scale <= 0 {
		meta.Scale = 1.0
	}
	mime := shot.Mime
	if mime == "" {
		mime = "image/webp"
	}

	// 3. Tier 1: 快速 YOLO 几何框检测 (?caption=0 跳过全框 Florence 自回归生成) + A11y 合并
	candidates := flatA11y
	detClient := e.resolveDetector(cfg)
	if detClient != nil {
		withCaption := cfg.CaptionMode == "florence"
		var boxes []fusion.VisualBox
		var derr error
		if hd, ok := detClient.(*fusion.HTTPDetector); ok {
			boxes, derr = hd.DetectWithOpts(ctx, imgBytes, mime, withCaption)
		} else {
			boxes, derr = detClient.Detect(ctx, imgBytes, mime)
		}
		if derr == nil && len(boxes) > 0 {
			aligned := make([]fusion.VisualBoxNative, 0, len(boxes))
			for _, b := range boxes {
				aligned = append(aligned, fusion.VisualBoxNative{
					Bbox:       fusion.AlignBox(b, meta.Scale),
					Kind:       b.Kind,
					Caption:    b.Caption,
					Confidence: b.Confidence,
				})
			}
			candidates = fusion.Merge(flatA11y, aligned, fusion.DefaultIoUThreshold)
		}
	}

	// 4. 组装 Jev SystemOne 请求（单次并发询问 Noul + Choice + Box）
	jevClient := e.resolveJevClient(cfg)
	if jevClient != nil {
		choices := FormatCandidateChoices(candidates, meta.Scale)
		somB64, somMime := RenderSoMOverlay(imgBytes, mime, candidates, meta.Scale)
		questions := map[string]QuestionSpec{
			"exists": {
				Type:     QuestionNoul,
				Question: fmt.Sprintf("Does the UI element '%s' exist on this screen?", target),
			},
		}
		if len(choices) > 0 {
			questions["pick"] = QuestionSpec{
				Type:     QuestionChoice,
				Question: fmt.Sprintf("Which candidate UI element best matches '%s'?", target),
				Choices:  choices,
			}
		}
		if cfg.EnableBox {
			questions["bbox"] = QuestionSpec{
				Type:     QuestionBox,
				Question: fmt.Sprintf("Locate the bounding box of '%s'", target),
			}
		}

		stateBlocks := []StateBlock{
			{
				Type: "image",
				Source: &ImageSource{
					Type:      "base64",
					MediaType: somMime,
					Data:      somB64,
				},
			},
		}
		if len(choices) > 0 {
			stateBlocks = append(stateBlocks, StateBlock{
				Type: "text",
				Text: "Candidate UI elements (index: [role] label @(display_x,display_y,display_w,display_h)):\n" + strings.Join(choices, "\n"),
			})
		}

		reqSpec := SystemOneRequest{
			Model:     cfg.Model,
			State:     stateBlocks,
			Questions: questions,
			Target:    target,
		}
		usedCloudFallback := false
		sresp, jerr := jevClient.Evaluate(ctx, reqSpec)
		if jerr != nil {
			if fb := e.resolveCloudFallbackJevClient(cfg); fb != nil {
				sresp, jerr = fb.Evaluate(ctx, reqSpec)
				usedCloudFallback = jerr == nil
			}
		}
		if jerr == nil && sresp != nil {
			noulProb := 1.0
			if exAns, ok := sresp.Answers["exists"]; ok {
				if exAns.Noul > 0 || len(exAns.Probabilities) > 0 {
					noulProb = exAns.Noul
					if yesP, ok := exAns.Probabilities["yes"]; ok && yesP > noulProb {
						noulProb = yesP
					}
				}
			}

			sourceTag := "vision_local"
			if cfg.Mode == "cloud" || usedCloudFallback {
				sourceTag = "vision_jev"
			}

			// Noul 存在性门控：低于阈值即判定目标不存在，绝不盲点
			if noulProb < cfg.NoulThreshold {
				return LocateOutput{
					Found:         false,
					Status:        "not_found",
					Source:        sourceTag,
					ExistenceProb: noulProb,
					ScreenW:       meta.NativeW,
					ScreenH:       meta.NativeH,
					Scale:         meta.Scale,
					LatencyMs:     time.Since(t0).Milliseconds(),
					Candidates:    candidates,
					Message:       fmt.Sprintf("Jev Noul 存在概率 %.2f 低于阈值 %.2f，判定目标 %q 不存在", noulProb, cfg.NoulThreshold, target),
				}, nil
			}

			// 优先消费 Choice 候选选择结果
			if pickAns, ok := sresp.Answers["pick"]; ok && len(candidates) > 0 {
				idx := pickAns.ChoiceIndex
				if (idx < 0 || idx >= len(candidates)) && pickAns.Choice != "" {
					idx = parseChoiceIndex(pickAns.Choice, choices)
				}
				selProb := pickAns.Confidence
				if selProb <= 0 && pickAns.Choice != "" {
					selProb = pickAns.Probabilities[pickAns.Choice]
				}
				if idx >= 0 && idx < len(candidates) && selProb >= 0.30 {
					chosen := candidates[idx]
					db := NativeToDisplayBounds(chosen.Bounds, meta.Scale)
					cp := CenterPoint(chosen.Bounds)
					dcp := CenterPoint(db)
					conf := math.Min(1.0, 0.5*noulProb+0.5*selProb)
					return LocateOutput{
						Found:             true,
						Status:            "matched",
						Source:            sourceTag,
						Bounds:            chosen.Bounds,
						DisplayBounds:     db,
						ClickPoint:        cp,
						DisplayClickPoint: dcp,
						Role:              chosen.Role,
						Name:              chosen.Name,
						Caption:           chosen.Caption,
						ExistenceProb:     noulProb,
						SelectionProb:     selProb,
						Confidence:        conf,
						ScreenW:           meta.NativeW,
						ScreenH:           meta.NativeH,
						Scale:             meta.Scale,
						LatencyMs:         time.Since(t0).Milliseconds(),
						Candidates:        candidates,
						Message:           fmt.Sprintf("Jev Choice 选中候选 #%d (noul=%.2f, prob=%.2f)", idx, noulProb, selProb),
					}, nil
				}
			}

			// Tier 2: 消费 Box 归一化视觉定位结果
			if boxAns, ok := sresp.Answers["bbox"]; ok && boxAns.Box != nil {
				bx := boxAns.Box
				if bx.Right > bx.Left && bx.Bottom > bx.Top {
					nw, nh := float64(meta.NativeW), float64(meta.NativeH)
					dw, dh := float64(meta.DisplayW), float64(meta.DisplayH)
					if dw <= 0 {
						dw = nw / meta.Scale
					}
					if dh <= 0 {
						dh = nh / meta.Scale
					}
					nativeBounds := [4]int32{
						int32(math.Round(bx.Left * nw)),
						int32(math.Round(bx.Top * nh)),
						int32(math.Round((bx.Right - bx.Left) * nw)),
						int32(math.Round((bx.Bottom - bx.Top) * nh)),
					}
					displayBounds := [4]int32{
						int32(math.Round(bx.Left * dw)),
						int32(math.Round(bx.Top * dh)),
						int32(math.Round((bx.Right - bx.Left) * dw)),
						int32(math.Round((bx.Bottom - bx.Top) * dh)),
					}
					bconf := boxAns.Confidence
					if bconf <= 0 {
						bconf = noulProb
					}
					cp := CenterPoint(nativeBounds)
					dcp := CenterPoint(displayBounds)
					return LocateOutput{
						Found:             true,
						Status:            "matched",
						Source:            sourceTag,
						Bounds:            nativeBounds,
						DisplayBounds:     displayBounds,
						ClickPoint:        cp,
						DisplayClickPoint: dcp,
						Role:              "box",
						Caption:           target,
						ExistenceProb:     noulProb,
						SelectionProb:     bconf,
						Confidence:        math.Min(1.0, 0.5*noulProb+0.5*bconf),
						ScreenW:           meta.NativeW,
						ScreenH:           meta.NativeH,
						Scale:             meta.Scale,
						LatencyMs:         time.Since(t0).Milliseconds(),
						Candidates:        candidates,
						Message:           fmt.Sprintf("Jev Box 视觉定位命中 (noul=%.2f, conf=%.2f)", noulProb, bconf),
					}, nil
				}
			}
		}
	}

	// 5. 启发式兜底：若候选集中有 caption/name 匹配项
	for idx, c := range candidates {
		label := strings.ToLower(strings.TrimSpace(c.Name + " " + c.Caption + " " + c.Role))
		if label != "" && strings.Contains(label, strings.ToLower(target)) {
			db := NativeToDisplayBounds(c.Bounds, meta.Scale)
			cp := CenterPoint(c.Bounds)
			dcp := CenterPoint(db)
			return LocateOutput{
				Found:             true,
				Status:            "matched",
				Source:            "vision_heuristic",
				Bounds:            c.Bounds,
				DisplayBounds:     db,
				ClickPoint:        cp,
				DisplayClickPoint: dcp,
				Role:              c.Role,
				Name:              c.Name,
				Caption:           c.Caption,
				ExistenceProb:     c.Confidence,
				SelectionProb:     c.Confidence,
				Confidence:        c.Confidence,
				ScreenW:           meta.NativeW,
				ScreenH:           meta.NativeH,
				Scale:             meta.Scale,
				LatencyMs:         time.Since(t0).Milliseconds(),
				Candidates:        candidates,
				Message:           fmt.Sprintf("融合候选 #%d 语义匹配命中", idx),
			}, nil
		}
	}

	return LocateOutput{
		Found:      false,
		Status:     "not_found",
		ScreenW:    meta.NativeW,
		ScreenH:    meta.NativeH,
		Scale:      meta.Scale,
		LatencyMs:  time.Since(t0).Milliseconds(),
		Candidates: candidates,
		Message:    fmt.Sprintf("未定位到匹配 %q 的 UI 元素（共扫描 %d 个候选框）", target, len(candidates)),
	}, nil
}

// Click 执行「定位 → 门控 → 点击 → 可选校验」一步闭环。
//
// 坐标传递策略：
//   - 若 Locate 经过了 screenshot（Scale > 0 且 DisplayClickPoint 非零），同时传入 display 坐标与默认空间，
//     或若仅经 Tier 0 A11y 命中（未触发 screenshot 更新节点 current_scale），显式传 coord_space="native"，
//     彻底根除 scale!=1.0 时的二次缩放偏移。
func (e *SelectorEngine) Click(ctx context.Context, in ClickInput) (ClickOutput, error) {
	t0 := time.Now()
	deadlineMs := in.DeadlineMs
	if deadlineMs <= 0 {
		deadlineMs = defaultClickDeadlineMs
	}
	who := in.Who
	if who == "" {
		who = "console"
	}

	loc, err := e.Locate(ctx, LocateInput{
		NodeID:      in.NodeID,
		Target:      in.Target,
		ForceVision: in.ForceVision,
		DeadlineMs:  deadlineMs,
		Who:         who,
	})
	if err != nil {
		return ClickOutput{}, err
	}
	if !loc.Found {
		return ClickOutput{
			Located:        loc,
			Clicked:        false,
			Verified:       false,
			Status:         loc.Status,
			TotalLatencyMs: time.Since(t0).Milliseconds(),
			Message:        loc.Message,
		}, nil
	}

	btn := strings.ToLower(strings.TrimSpace(in.Button))
	if btn == "" {
		btn = "left"
	}

	// 若本次 Locate 采集过 screenshot（Source != "a11y"），节点侧 current_scale 已被本次 screenshot 刷新为 loc.Scale，
	// 传 DisplayClickPoint（兼容旧版未升级 coord_space 的节点与新版节点）；
	// 若走 Tier 0 纯 A11y 命中（未调 screenshot），传原生坐标 ClickPoint + coord_space="native"。
	clickArgs := map[string]any{
		"button": btn,
	}
	if loc.Source == "a11y" {
		clickArgs["coordinate"] = []int32{loc.ClickPoint[0], loc.ClickPoint[1]}
		clickArgs["coord_space"] = "native"
	} else {
		clickArgs["coordinate"] = []int32{loc.DisplayClickPoint[0], loc.DisplayClickPoint[1]}
		clickArgs["coord_space"] = "display"
	}
	rawArgs, _ := json.Marshal(clickArgs)

	if _, cerr := e.dispatchNodeTool(ctx, in.NodeID, "click", rawArgs, deadlineMs, who); cerr != nil {
		return ClickOutput{
			Located:        loc,
			Clicked:        false,
			Verified:       false,
			Status:         "click_failed",
			TotalLatencyMs: time.Since(t0).Milliseconds(),
			Message:        fmt.Sprintf("定位成功但执行点击失败: %v", cerr),
		}, nil
	}

	verified := false
	if in.Verify {
		if _, verr := e.dispatchNodeTool(ctx, in.NodeID, "get_a11y_tree", nil, 5_000, who); verr == nil {
			verified = true
		}
	}

	return ClickOutput{
		Located:        loc,
		Clicked:        true,
		Verified:       verified,
		Status:         "clicked",
		TotalLatencyMs: time.Since(t0).Milliseconds(),
		Message:        fmt.Sprintf("已点击 %q @ native(%d,%d) / display(%d,%d)", in.Target, loc.ClickPoint[0], loc.ClickPoint[1], loc.DisplayClickPoint[0], loc.DisplayClickPoint[1]),
	}, nil
}

func (e *SelectorEngine) resolveDetector(cfg store.VisionSettings) fusion.DetectorClient {
	e.mu.RLock()
	override := e.detOverride
	e.mu.RUnlock()
	if override != nil {
		return override
	}
	ep := normalizeBaseURL(cfg.LocalEndpoint)
	if ep == "" {
		ep = e.defaultLocalEndpoint
	}
	if ep == "" || e.localDetectorToken == "" {
		return nil
	}
	return fusion.NewHTTPDetector(ep, e.localDetectorToken, 20*time.Second)
}

func (e *SelectorEngine) resolveJevClient(cfg store.VisionSettings) JevClient {
	e.mu.RLock()
	override := e.jevOverride
	e.mu.RUnlock()
	if override != nil {
		return override
	}
	switch cfg.Mode {
	case "cloud":
		if cfg.CloudEndpoint == "" || cfg.APIKey == "" {
			return nil
		}
		return NewHTTPJevClient(cfg.CloudEndpoint, cfg.APIKey, 20*time.Second)
	case "local", "auto":
		ep := normalizeBaseURL(cfg.LocalEndpoint)
		if ep == "" {
			ep = e.defaultLocalEndpoint
		}
		if ep != "" {
			return NewHTTPJevClient(ep, e.localDetectorToken, 20*time.Second)
		}
		if cfg.Mode == "auto" && cfg.CloudEndpoint != "" && cfg.APIKey != "" {
			return NewHTTPJevClient(cfg.CloudEndpoint, cfg.APIKey, 20*time.Second)
		}
		return nil
	default:
		return nil
	}
}

func (e *SelectorEngine) resolveCloudFallbackJevClient(cfg store.VisionSettings) JevClient {
	if cfg.Mode != "auto" || cfg.CloudEndpoint == "" || cfg.APIKey == "" {
		return nil
	}
	return NewHTTPJevClient(cfg.CloudEndpoint, cfg.APIKey, 20*time.Second)
}

func parseChoiceIndex(choice string, choices []string) int {
	for i, c := range choices {
		if c == choice {
			return i
		}
	}
	if colon := strings.IndexByte(choice, ':'); colon > 0 {
		if idx, err := strconv.Atoi(strings.TrimSpace(choice[:colon])); err == nil {
			return idx
		}
	}
	return -1
}
