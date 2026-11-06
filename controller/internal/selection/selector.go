package selection

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	aurav1 "github.com/aura/controller/gen/aura/v1"
	"github.com/aura/controller/internal/fusion"
	"github.com/aura/controller/internal/store"
)

const (
	defaultLocateDeadlineMs = 25_000
	defaultClickDeadlineMs  = 30_000
	candidateCacheTTL       = 15 * time.Second
	maxCandidateCacheNodes  = 32
)

type readOnlyDispatcher interface {
	DispatchReadOnly(ctx context.Context, nodeID, tool string, jsonArgs []byte, deadlineMs int64, who string) (*aurav1.ToolResponse, string, error)
}

type cachedVisualBoxes struct {
	exactHash   uint64
	dHash       uint64
	withCaption bool
	boxes       []fusion.VisualBox
	at          time.Time
}

// SelectorEngine 编排三层混合定位（Tier 0 A11y → Tier 1 YOLO+Jev Choice/Noul → Tier 2 Jev Box）与一步点击。
type SelectorEngine struct {
	disp                 fusion.Dispatcher
	st                   *store.PGStore
	defaultLocalEndpoint string
	localDetectorToken   string
	httpClient           *http.Client

	mu          sync.RWMutex
	memSettings store.VisionSettings
	jevOverride JevClient
	detOverride fusion.DetectorClient
	candCache   map[string]cachedVisualBoxes
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
		httpClient:           &http.Client{Timeout: 20 * time.Second},
		memSettings:          store.DefaultVisionSettings(defaultLocalEndpoint),
		candCache:            make(map[string]cachedVisualBoxes),
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
		updated, err := e.st.UpsertVisionSettings(ctx, norm, action, newAPIKey)
		if err == nil {
			ResetJevCircuitBreakers()
		}
		return updated, err
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
	ResetJevCircuitBreakers()
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
	SnapshotID  string `json:"snapshot_id"`
	Meta        struct {
		NativeW    int32   `json:"native_w"`
		NativeH    int32   `json:"native_h"`
		DisplayW   int32   `json:"display_w"`
		DisplayH   int32   `json:"display_h"`
		Scale      float64 `json:"scale"`
		SnapshotID string  `json:"snapshot_id"`
	} `json:"meta"`
}

func (e *SelectorEngine) dispatchNodeTool(ctx context.Context, nodeID, tool string, args []byte, deadlineMs int64, who, traceID string) (json.RawMessage, error) {
	if e.disp == nil {
		return nil, errors.New("selection: dispatcher not configured")
	}
	if len(args) == 0 {
		args = []byte("{}")
	}
	var (
		resp *aurav1.ToolResponse
		code string
		err  error
	)
	if traceID == "" && (tool == "screenshot" || tool == "get_a11y_tree") {
		if ro, ok := e.disp.(readOnlyDispatcher); ok {
			resp, code, err = ro.DispatchReadOnly(ctx, nodeID, tool, args, deadlineMs, who)
		} else {
			resp, code, err = e.disp.Dispatch(ctx, nodeID, tool, args, deadlineMs, who, "")
		}
	} else {
		resp, code, err = e.disp.Dispatch(ctx, nodeID, tool, args, deadlineMs, who, traceID)
	}
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

func isInteractiveRole(role string) bool {
	switch role {
	case "button", "push button", "toggle button", "radio button",
		"check box", "checkbox", "link", "menuitem", "menu item",
		"tab", "page tab", "entry", "text", "textbox", "searchbox",
		"combobox", "combo box", "slider", "spinbutton", "switch":
		return true
	default:
		return false
	}
}

func hasNegationIntent(q string) bool {
	for _, neg := range []string{"not ", "except ", "other than ", "非", "除了", "不要"} {
		if strings.Contains(q, neg) {
			return true
		}
	}
	return false
}

// matchA11yCandidate 在扁平化 A11y 元素中寻找最佳语义匹配（带可交互角色加权与否定词守卫）。
func matchA11yCandidate(flat []fusion.FusedElement, target string) (fusion.FusedElement, float64, bool) {
	q := strings.ToLower(strings.TrimSpace(target))
	if q == "" || len(flat) == 0 || hasNegationIntent(q) {
		return fusion.FusedElement{}, 0, false
	}
	var best fusion.FusedElement
	bestScore := 0.0
	bestArea := int64(math.MaxInt64)
	for _, el := range flat {
		name := strings.ToLower(strings.TrimSpace(el.Name))
		role := strings.ToLower(strings.TrimSpace(el.Role))
		score := 0.0
		switch {
		case name != "" && name == q:
			score = 0.97
		case name != "" && strings.Contains(name, q):
			score = 0.88
		case role != "" && (role == q || (name != "" && strings.Contains(role+" "+name, q))):
			score = 0.86
		case name != "" && strings.Contains(q, name) && len(name) >= 2:
			score = 0.78
		}
		if score > 0 && isInteractiveRole(role) {
			score = math.Min(1.0, score+0.03)
		}
		area := int64(max(1, el.Bounds[2])) * int64(max(1, el.Bounds[3]))
		if score > bestScore+1e-6 || (math.Abs(score-bestScore) <= 1e-6 && score > 0 && area < bestArea) {
			bestScore = score
			best = el
			bestArea = area
		}
	}
	return best, bestScore, bestScore > 0
}

func (e *SelectorEngine) getCachedBoxes(nodeID string, exactHash uint64, withCaption bool) ([]fusion.VisualBox, bool) {
	if exactHash == 0 {
		return nil, false
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	entry, ok := e.candCache[nodeID]
	if !ok || entry.exactHash != exactHash || entry.withCaption != withCaption {
		return nil, false
	}
	if time.Since(entry.at) > candidateCacheTTL {
		return nil, false
	}
	out := make([]fusion.VisualBox, len(entry.boxes))
	copy(out, entry.boxes)
	return out, true
}

func (e *SelectorEngine) putCachedBoxes(nodeID string, exactHash, dHash uint64, withCaption bool, boxes []fusion.VisualBox) {
	if nodeID == "" || exactHash == 0 {
		return
	}
	cp := make([]fusion.VisualBox, len(boxes))
	copy(cp, boxes)
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.candCache) >= maxCandidateCacheNodes {
		var oldestKey string
		var oldestTime time.Time
		for k, v := range e.candCache {
			if oldestKey == "" || v.at.Before(oldestTime) {
				oldestKey = k
				oldestTime = v.at
			}
		}
		if oldestKey != "" {
			delete(e.candCache, oldestKey)
		}
	}
	e.candCache[nodeID] = cachedVisualBoxes{
		exactHash:   exactHash,
		dHash:       dHash,
		withCaption: withCaption,
		boxes:       cp,
		at:          time.Now(),
	}
}

func (e *SelectorEngine) getLastShotHashes(nodeID string) (exactHash, dHash uint64, ok bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	entry, exists := e.candCache[nodeID]
	if !exists || entry.exactHash == 0 {
		return 0, 0, false
	}
	return entry.exactHash, entry.dHash, true
}

func (e *SelectorEngine) invalidateNodeCache(nodeID string) {
	e.mu.Lock()
	delete(e.candCache, nodeID)
	e.mu.Unlock()
}

func computeA11yFingerprint(elems []fusion.FusedElement) string {
	var b strings.Builder
	for _, el := range elems {
		if el.Source != "" && el.Source != "a11y" {
			continue
		}
		fmt.Fprintf(&b, "%s|%s|%d,%d,%d,%d;", el.Role, el.Name, el.Bounds[0], el.Bounds[1], el.Bounds[2], el.Bounds[3])
	}
	return b.String()
}

// summarizeCandidatesHint 提炼屏幕上可见的具名/可交互元素摘要，供未命中时辅助 Agent/操作员快速自愈改写查询。
func summarizeCandidatesHint(elems []fusion.FusedElement, scale float64, maxItems int) string {
	if len(elems) == 0 || maxItems <= 0 {
		return ""
	}
	if scale <= 0 {
		scale = 1.0
	}
	var items []string
	seen := make(map[string]struct{})
	// 优先收集可交互具名元素，再收集普通具名/带描述元素
	for pass := 0; pass < 2 && len(items) < maxItems; pass++ {
		for _, el := range elems {
			role := strings.ToLower(strings.TrimSpace(el.Role))
			if pass == 0 && !isInteractiveRole(role) {
				continue
			}
			label := strings.TrimSpace(el.Name)
			if label == "" {
				label = strings.TrimSpace(el.Caption)
			}
			if label == "" {
				continue
			}
			if len([]rune(label)) > 28 {
				label = string([]rune(label)[:28]) + "…"
			}
			key := role + "|" + strings.ToLower(label)
			if _, exists := seen[key]; exists {
				continue
			}
			seen[key] = struct{}{}
			db := NativeToDisplayBounds(el.Bounds, scale)
			dcp := CenterPoint(db)
			if role != "" {
				items = append(items, fmt.Sprintf("[%s] %q @(%d,%d)", role, label, dcp[0], dcp[1]))
			} else {
				items = append(items, fmt.Sprintf("%q @(%d,%d)", label, dcp[0], dcp[1]))
			}
			if len(items) >= maxItems {
				break
			}
		}
	}
	if len(items) == 0 {
		return ""
	}
	return "可见元素参考: " + strings.Join(items, ", ")
}

// Locate 执行三层混合元素定位（支持可选 WaitMs 条件轮询等待动态元素出现）。
func (e *SelectorEngine) Locate(ctx context.Context, in LocateInput) (LocateOutput, error) {
	if in.WaitMs <= 0 {
		return e.locateOnce(ctx, in)
	}
	waitCap := in.WaitMs
	if waitCap > 15_000 {
		waitCap = 15_000
	}
	start := time.Now()
	waitDur := time.Duration(waitCap) * time.Millisecond
	for {
		out, err := e.locateOnce(ctx, in)
		if err != nil || out.Found || out.Status == "vision_disabled" {
			out.LatencyMs = time.Since(start).Milliseconds()
			return out, err
		}
		if time.Since(start) >= waitDur {
			out.LatencyMs = time.Since(start).Milliseconds()
			return out, nil
		}
		// 轮询等待期间失效同屏缓存以确保重采最新画面
		e.invalidateNodeCache(in.NodeID)
		select {
		case <-ctx.Done():
			return out, ctx.Err()
		case <-time.After(350 * time.Millisecond):
		}
	}
}

func (e *SelectorEngine) locateOnce(ctx context.Context, in LocateInput) (LocateOutput, error) {
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
		// Fail-closed：当持久层发生非 NotFound 故障时，回落到 disabled（仅 A11y 确定性匹配），防止违背管理员已保存的禁用策略误发起外部视觉调用。
		cfg = store.DefaultVisionSettings(e.defaultLocalEndpoint)
		cfg.Mode = "disabled"
	}

	// 1. 采集 A11y 树（容错：若平台不支持 A11y 或采集失败，不阻断后续视觉分支）
	var flatA11y []fusion.FusedElement
	var a11yTruncated bool
	if treeRaw, aerr := e.dispatchNodeTool(ctx, in.NodeID, "get_a11y_tree", nil, deadlineMs, who, in.TraceID); aerr == nil {
		var tree fusion.A11yTree
		if json.Unmarshal(treeRaw, &tree) == nil {
			flatA11y = fusion.FlattenA11y(tree)
			a11yTruncated = tree.Truncated
		}
	}

	// 若默认 depth=3 / max_nodes=200 被截断且尚未命中高置信度候选，自动扩窗至 depth=16, max_nodes=1000 重采一次（修复深层控件漏检）
	if a11yTruncated {
		if _, score, ok := matchA11yCandidate(flatA11y, target); !ok || score < 0.85 {
			if treeRaw, aerr := e.dispatchNodeTool(ctx, in.NodeID, "get_a11y_tree", []byte(`{"depth":16,"max_nodes":1000}`), deadlineMs, who, in.TraceID); aerr == nil {
				var tree fusion.A11yTree
				if json.Unmarshal(treeRaw, &tree) == nil {
					flatA11y = fusion.FlattenA11y(tree)
				}
			}
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
			Message:    appendHint("A11y 未命中目标且视觉推理当前设为禁用 (disabled)", summarizeCandidatesHint(flatA11y, 1.0, 10)),
		}, nil
	}

	// 2. 采集节点屏幕截图
	shotRaw, err := e.dispatchNodeTool(ctx, in.NodeID, "screenshot", nil, deadlineMs, who, in.TraceID)
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
	snapshotID := strings.TrimSpace(meta.SnapshotID)
	if snapshotID == "" {
		snapshotID = strings.TrimSpace(shot.SnapshotID)
	}
	mime := shot.Mime
	if mime == "" {
		mime = "image/webp"
	}
	exactHash := ComputeExactHash(imgBytes)
	dHash, _ := ComputeDHash(imgBytes)

	// 3. Tier 1: 快速 YOLO 几何框检测 (?caption=0 跳过全框 Florence 自回归生成，支持同屏感知缓存) + A11y 合并
	candidates := flatA11y
	detClient := e.resolveDetector(cfg)
	if detClient != nil {
		withCaption := cfg.CaptionMode == "florence"
		boxes, cached := e.getCachedBoxes(in.NodeID, exactHash, withCaption)
		var derr error
		if !cached {
			if od, ok := detClient.(fusion.OptsDetectorClient); ok {
				boxes, derr = od.DetectWithOpts(ctx, imgBytes, mime, withCaption)
			} else {
				boxes, derr = detClient.Detect(ctx, imgBytes, mime)
			}
			if derr == nil {
				e.putCachedBoxes(in.NodeID, exactHash, dHash, withCaption, boxes)
			}
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
			candidates = MergeCandidatesIoU(flatA11y, aligned, DefaultCandidateIoUThreshold)
		}
	} else {
		e.putCachedBoxes(in.NodeID, exactHash, dHash, false, nil)
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
				if exAns.HasNoul || exAns.Type == QuestionNoul || exAns.Noul > 0 || len(exAns.Probabilities) > 0 {
					noulProb = exAns.Noul
					if yesP, ok := exAns.Probabilities["yes"]; ok && (!exAns.HasNoul || yesP > noulProb) {
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
					SnapshotID:    snapshotID,
					LatencyMs:     time.Since(t0).Milliseconds(),
					Candidates:    candidates,
					Message:       appendHint(fmt.Sprintf("Jev Noul 存在概率 %.2f 低于阈值 %.2f，判定目标 %q 不存在", noulProb, cfg.NoulThreshold, target), summarizeCandidatesHint(candidates, meta.Scale, 10)),
				}, nil
			}

			// 优先消费 Choice 候选选择结果
			if pickAns, ok := sresp.Answers["pick"]; ok && len(candidates) > 0 {
				idx := pickAns.ChoiceIndex
				if (!pickAns.HasChoiceIndex && pickAns.Choice != "" && idx == 0) || idx < 0 || idx >= len(candidates) {
					if parsed := parseChoiceIndex(pickAns.Choice, choices); parsed >= 0 {
						idx = parsed
					} else if !pickAns.HasChoiceIndex && pickAns.Choice != "" {
						idx = -1
					}
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
						SnapshotID:        snapshotID,
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
						SnapshotID:        snapshotID,
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
				SnapshotID:        snapshotID,
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
		SnapshotID: snapshotID,
		LatencyMs:  time.Since(t0).Milliseconds(),
		Candidates: candidates,
		Message:    appendHint(fmt.Sprintf("未定位到匹配 %q 的 UI 元素（共扫描 %d 个候选框）", target, len(candidates)), summarizeCandidatesHint(candidates, meta.Scale, 10)),
	}, nil
}

func appendHint(base, hint string) string {
	if hint == "" {
		return base
	}
	return base + "（" + hint + "）"
}

// Click 执行「定位 → 门控 → 点击/双击 → 可选输入/按键 → 可选校验」一步闭环。
//
// 坐标传递策略：
//   - 若 Locate 经过了 screenshot（Scale > 0 且 DisplayClickPoint 非零），同时传入 display 坐标与 snapshot_id，
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
		WaitMs:      in.WaitMs,
		DeadlineMs:  deadlineMs,
		Who:         who,
		TraceID:     in.TraceID,
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

	preA11yFP := computeA11yFingerprint(loc.Candidates)
	preExactHash, preDHash, hasPreShot := e.getLastShotHashes(in.NodeID)

	btn := strings.ToLower(strings.TrimSpace(in.Button))
	if btn == "" {
		btn = "left"
	}

	// 若本次 Locate 采集过 screenshot（Source != "a11y"），传入 DisplayClickPoint + snapshot_id；
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
		if loc.SnapshotID != "" {
			clickArgs["snapshot_id"] = loc.SnapshotID
		}
	}
	rawArgs, _ := json.Marshal(clickArgs)

	clickTool := "click"
	if in.DoubleClick {
		clickTool = "double_click"
	}
	if _, cerr := e.dispatchNodeTool(ctx, in.NodeID, clickTool, rawArgs, deadlineMs, who, in.TraceID); cerr != nil {
		return ClickOutput{
			Located:        loc,
			Clicked:        false,
			Verified:       false,
			Status:         "click_failed",
			TotalLatencyMs: time.Since(t0).Milliseconds(),
			Message:        fmt.Sprintf("定位成功但执行点击失败: %v", cerr),
		}, nil
	}

	// 点击已发生，立即失效该节点同屏候选缓存
	e.invalidateNodeCache(in.NodeID)

	typed := false
	if in.TypeText != "" {
		typeArgs, _ := json.Marshal(map[string]any{"text": in.TypeText})
		if _, terr := e.dispatchNodeTool(ctx, in.NodeID, "type", typeArgs, deadlineMs, who, in.TraceID); terr == nil {
			typed = true
		}
	}

	keyPressed := ""
	if k := strings.TrimSpace(in.PressKey); k != "" {
		keyArgs, _ := json.Marshal(map[string]any{"key": k})
		if _, kerr := e.dispatchNodeTool(ctx, in.NodeID, "key", keyArgs, deadlineMs, who, in.TraceID); kerr == nil {
			keyPressed = k
		}
	}

	verified := false
	verifyNote := ""
	if in.Verify {
		verified, verifyNote = e.verifyPostClickState(ctx, in.NodeID, preA11yFP, preExactHash, preDHash, hasPreShot, who, in.TraceID)
	}

	actionLabel := "已点击"
	if in.DoubleClick {
		actionLabel = "已双击"
	}
	msg := fmt.Sprintf("%s %q @ native(%d,%d) / display(%d,%d)", actionLabel, in.Target, loc.ClickPoint[0], loc.ClickPoint[1], loc.DisplayClickPoint[0], loc.DisplayClickPoint[1])
	if typed {
		msg += fmt.Sprintf(" · 已输入 %q", in.TypeText)
	}
	if keyPressed != "" {
		msg += fmt.Sprintf(" · 已按键 %s", keyPressed)
	}
	if verifyNote != "" {
		msg += " · " + verifyNote
	}

	return ClickOutput{
		Located:        loc,
		Clicked:        true,
		DoubleClicked:  in.DoubleClick,
		Typed:          typed,
		KeyPressed:     keyPressed,
		Verified:       verified,
		Status:         "clicked",
		TotalLatencyMs: time.Since(t0).Milliseconds(),
		Message:        msg,
	}, nil
}

// verifyPostClickState 执行点击后双层真实状态差分校验（L1 A11y 结构指纹对比 + L2 视觉感知哈希对比）。
func (e *SelectorEngine) verifyPostClickState(
	ctx context.Context,
	nodeID string,
	preA11yFP string,
	preExactHash, preDHash uint64,
	hasPreShot bool,
	who, traceID string,
) (bool, string) {
	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return false, "后置校验超时"
			case <-time.After(150 * time.Millisecond):
			}
		}
		if preA11yFP != "" {
			if treeRaw, aerr := e.dispatchNodeTool(ctx, nodeID, "get_a11y_tree", []byte(`{"depth":16,"max_nodes":1000}`), 5_000, who, traceID); aerr == nil {
				var tree fusion.A11yTree
				if json.Unmarshal(treeRaw, &tree) == nil {
					postFP := computeA11yFingerprint(fusion.FlattenA11y(tree))
					if postFP != preA11yFP {
						return true, "A11y 状态差分校验通过"
					}
				}
			}
		}
		if hasPreShot {
			if shotRaw, serr := e.dispatchNodeTool(ctx, nodeID, "screenshot", nil, 5_000, who, traceID); serr == nil {
				var shot shotPayload
				if json.Unmarshal(shotRaw, &shot) == nil {
					if postBytes, derr := base64.StdEncoding.DecodeString(shot.ImageBase64); derr == nil && len(postBytes) > 0 {
						postExact := ComputeExactHash(postBytes)
						postDHash, ok := ComputeDHash(postBytes)
						if (ok && HammingDistance(preDHash, postDHash) >= 1) || postExact != preExactHash {
							return true, "视觉帧差分校验通过"
						}
					}
				}
			}
		}
	}
	return false, "点击后界面状态未见显著变化"
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
		return NewHTTPJevClientWithClient(cfg.CloudEndpoint, cfg.APIKey, e.httpClient)
	case "local", "auto":
		ep := normalizeBaseURL(cfg.LocalEndpoint)
		if ep == "" {
			ep = e.defaultLocalEndpoint
		}
		if ep != "" {
			return NewHTTPJevClientWithClient(ep, e.localDetectorToken, e.httpClient)
		}
		if cfg.Mode == "auto" && cfg.CloudEndpoint != "" && cfg.APIKey != "" {
			return NewHTTPJevClientWithClient(cfg.CloudEndpoint, cfg.APIKey, e.httpClient)
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
	return NewHTTPJevClientWithClient(cfg.CloudEndpoint, cfg.APIKey, e.httpClient)
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
