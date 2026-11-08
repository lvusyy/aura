package selection

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var (
	somBracketRe   = regexp.MustCompile(`^\s*\[(\d+)\]\s*$`)
	uiTarsCallRe   = regexp.MustCompile(`(?s)(?:Action:\s*)?([a-zA-Z_][a-zA-Z0-9_]*)\s*\((.*)\)\s*$`)
	uiTarsKVRe     = regexp.MustCompile(`([a-zA-Z_][a-zA-Z0-9_]*)\s*=\s*(?:'((?:\\'|[^'])*)'|"((?:\\"|[^"])*)"|([^\s,)]+))`)
	uiTarsNumberRe = regexp.MustCompile(`-?\d+(?:\.\d+)?`)
)

// ParseUITarsAction 解析 UI-TARS / OmniParser 风格动作指令（支持带 Thought: 前缀的多行输出），
// 并将其规整为 ComputerActionInput。UI-TARS 默认使用 [0, 1000] 归一化坐标系（normalized_1000）。
func ParseUITarsAction(raw string) (ComputerActionInput, string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ComputerActionInput{}, "", errors.New("empty ui_tars_action")
	}

	var thought string
	actionPart := trimmed
	if idx := strings.LastIndex(trimmed, "Action:"); idx >= 0 {
		thought = strings.TrimSpace(strings.TrimPrefix(trimmed[:idx], "Thought:"))
		actionPart = strings.TrimSpace(trimmed[idx+len("Action:"):])
	}

	m := uiTarsCallRe.FindStringSubmatch(actionPart)
	if m == nil {
		return ComputerActionInput{}, thought, fmt.Errorf("invalid UI-TARS action syntax: %q", actionPart)
	}
	fnName := strings.ToLower(strings.TrimSpace(m[1]))
	argBody := strings.TrimSpace(m[2])

	kv := make(map[string]string)
	for _, match := range uiTarsKVRe.FindAllStringSubmatch(argBody, -1) {
		key := strings.ToLower(strings.TrimSpace(match[1]))
		val := match[2]
		if val == "" {
			val = match[3]
		}
		if val == "" {
			val = match[4]
		}
		val = strings.ReplaceAll(val, `\'`, `'`)
		val = strings.ReplaceAll(val, `\"`, `"`)
		kv[key] = val
	}

	out := ComputerActionInput{
		CoordSpace: "normalized_1000",
	}
	parseBoxCenter := func(boxStr string) ([]int32, bool) {
		nums := uiTarsNumberRe.FindAllString(boxStr, -1)
		if len(nums) >= 4 {
			x1, _ := strconv.ParseFloat(nums[0], 64)
			y1, _ := strconv.ParseFloat(nums[1], 64)
			x2, _ := strconv.ParseFloat(nums[2], 64)
			y2, _ := strconv.ParseFloat(nums[3], 64)
			return []int32{int32(math.Round((x1 + x2) / 2.0)), int32(math.Round((y1 + y2) / 2.0))}, true
		}
		if len(nums) >= 2 {
			x, _ := strconv.ParseFloat(nums[0], 64)
			y, _ := strconv.ParseFloat(nums[1], 64)
			return []int32{int32(math.Round(x)), int32(math.Round(y))}, true
		}
		return nil, false
	}

	startStr := firstNonEmpty(kv["start_box"], kv["point"], kv["box"], kv["coordinate"])
	endStr := firstNonEmpty(kv["end_box"], kv["end_point"])

	switch fnName {
	case "click", "left_single", "left_click", "tap":
		out.Action = "left_click"
		if pt, ok := parseBoxCenter(startStr); ok {
			out.Coordinate = pt
		}
	case "left_double", "double_click":
		out.Action = "double_click"
		if pt, ok := parseBoxCenter(startStr); ok {
			out.Coordinate = pt
		}
	case "right_single", "right_click":
		out.Action = "right_click"
		if pt, ok := parseBoxCenter(startStr); ok {
			out.Coordinate = pt
		}
	case "middle_click":
		out.Action = "middle_click"
		if pt, ok := parseBoxCenter(startStr); ok {
			out.Coordinate = pt
		}
	case "move", "mouse_move", "hover":
		out.Action = "mouse_move"
		if pt, ok := parseBoxCenter(startStr); ok {
			out.Coordinate = pt
		}
	case "drag", "left_click_drag", "swipe":
		out.Action = "left_click_drag"
		if pt1, ok := parseBoxCenter(startStr); ok {
			out.StartCoordinate = pt1
		}
		if pt2, ok := parseBoxCenter(endStr); ok {
			out.Coordinate = pt2
		}
	case "type":
		out.Action = "type"
		out.Text = firstNonEmpty(kv["content"], kv["text"])
	case "hotkey", "key", "press_key", "keypress":
		out.Action = "key"
		rawKey := firstNonEmpty(kv["key"], kv["hotkey"], kv["keys"])
		out.Text = normalizeHotkeyChord(rawKey)
	case "scroll":
		out.Action = "scroll"
		if pt, ok := parseBoxCenter(startStr); ok {
			out.Coordinate = pt
		}
		out.ScrollDirection = strings.ToLower(firstNonEmpty(kv["direction"], "down"))
		if amt, err := strconv.Atoi(kv["amount"]); err == nil && amt > 0 {
			out.ScrollAmount = int32(amt)
		} else {
			out.ScrollAmount = 3
		}
	case "wait":
		out.Action = "wait"
		if sec, err := strconv.ParseFloat(firstNonEmpty(kv["duration"], kv["seconds"], "1"), 64); err == nil && sec > 0 {
			out.Duration = sec
		} else {
			out.Duration = 1.0
		}
	case "screenshot":
		out.Action = "screenshot"
	case "finished", "finish", "done", "complete":
		out.Action = "finished"
		out.Text = firstNonEmpty(kv["content"], kv["message"], thought)
	default:
		return ComputerActionInput{}, thought, fmt.Errorf("unsupported UI-TARS action %q", fnName)
	}
	return out, thought, nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// normalizeHotkeyChord 将 UI-TARS / OpenAI 的按键表示（如 "ctrl c"、"enter"、"cmd+shift+p"）规整为 AURA 节点可识别的格式。
func normalizeHotkeyChord(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	var parts []string
	if strings.Contains(s, "+") {
		parts = strings.Split(s, "+")
	} else {
		parts = strings.Fields(s)
	}
	if len(parts) == 0 {
		return s
	}
	mapped := make([]string, 0, len(parts))
	for _, p := range parts {
		low := strings.ToLower(strings.TrimSpace(p))
		switch low {
		case "ctrl", "control":
			mapped = append(mapped, "Ctrl")
		case "alt", "option":
			mapped = append(mapped, "Alt")
		case "shift":
			mapped = append(mapped, "Shift")
		case "cmd", "command", "meta", "super", "win":
			mapped = append(mapped, "Super")
		case "enter", "return":
			mapped = append(mapped, "Return")
		case "esc", "escape":
			mapped = append(mapped, "Escape")
		case "tab":
			mapped = append(mapped, "Tab")
		case "space", "spacebar":
			mapped = append(mapped, "space")
		case "backspace":
			mapped = append(mapped, "BackSpace")
		case "delete", "del":
			mapped = append(mapped, "Delete")
		case "up", "arrowup":
			mapped = append(mapped, "Up")
		case "down", "arrowdown":
			mapped = append(mapped, "Down")
		case "left", "arrowleft":
			mapped = append(mapped, "Left")
		case "right", "arrowright":
			mapped = append(mapped, "Right")
		default:
			mapped = append(mapped, strings.TrimSpace(p))
		}
	}
	return strings.Join(mapped, "+")
}

func isSupportedComputerAction(action string) bool {
	switch action {
	case "finished", "finish", "done",
		"screenshot", "annotate", "annotate_screen", "zoom",
		"left_click", "click", "left_single", "right_click", "right_single", "middle_click", "double_click", "left_double", "triple_click",
		"mouse_move", "move", "hover",
		"left_click_drag", "drag", "swipe",
		"type", "key", "keypress", "hotkey",
		"scroll", "wait":
		return true
	default:
		return false
	}
}

func isDirectTargetClickAction(action string) bool {
	switch action {
	case "left_click", "click", "left_single", "right_click", "right_single", "middle_click", "double_click", "left_double":
		return true
	default:
		return false
	}
}

func isTargetLocateAction(action string) bool {
	switch action {
	case "triple_click", "mouse_move", "move", "hover", "scroll":
		return true
	default:
		return false
	}
}

func (e *SelectorEngine) fetchNodeShot(ctx context.Context, nodeID string, deadlineMs int64, who, traceID string) (shotPayload, error) {
	raw, err := e.dispatchNodeTool(ctx, nodeID, "screenshot", nil, deadlineMs, who, traceID)
	if err != nil {
		return shotPayload{}, err
	}
	var shot shotPayload
	if err := json.Unmarshal(raw, &shot); err != nil {
		return shotPayload{}, fmt.Errorf("decode screenshot payload: %w", err)
	}
	if shot.Meta.Scale <= 0 {
		shot.Meta.Scale = 1.0
	}
	if shot.Mime == "" {
		shot.Mime = "image/webp"
	}
	return shot, nil
}

// ExecuteComputerAction 执行统一的 Computer-Use 动作（兼容 Anthropic computer_20250124、OpenAI Operator、
// UI-TARS 指令、Set-of-Mark 索引及 AURA 混合语义目标）。
func (e *SelectorEngine) ExecuteComputerAction(ctx context.Context, in ComputerActionInput) (ComputerActionOutput, error) {
	start := time.Now()
	if strings.TrimSpace(in.NodeID) == "" {
		return ComputerActionOutput{}, errors.New("node_id is required")
	}
	deadlineMs := in.DeadlineMs
	if deadlineMs <= 0 {
		deadlineMs = defaultClickDeadlineMs
	}
	who := in.Who
	if who == "" {
		who = "computer-use"
	}

	// 若传入了 UI-TARS 原始动作串，先解析并合并至入参
	if strings.TrimSpace(in.UITarsAction) != "" {
		parsed, _, err := ParseUITarsAction(in.UITarsAction)
		if err != nil {
			return ComputerActionOutput{}, err
		}
		if in.Action == "" {
			in.Action = parsed.Action
		}
		if len(in.Coordinate) == 0 && len(parsed.Coordinate) == 2 {
			in.Coordinate = parsed.Coordinate
		}
		if len(in.StartCoordinate) == 0 && len(parsed.StartCoordinate) == 2 {
			in.StartCoordinate = parsed.StartCoordinate
		}
		if in.CoordSpace == "" {
			in.CoordSpace = parsed.CoordSpace
		}
		if in.Text == "" {
			in.Text = parsed.Text
		}
		if in.ScrollDirection == "" {
			in.ScrollDirection = parsed.ScrollDirection
		}
		if in.ScrollAmount <= 0 {
			in.ScrollAmount = parsed.ScrollAmount
		}
		if in.Duration <= 0 {
			in.Duration = parsed.Duration
		}
	}

	// 兼容 OpenAI Operator 单点 x/y 与 path[]
	if len(in.Coordinate) < 2 && in.X != nil && in.Y != nil {
		in.Coordinate = []int32{*in.X, *in.Y}
	}
	if len(in.Path) >= 2 {
		if len(in.StartCoordinate) < 2 {
			in.StartCoordinate = []int32{in.Path[0].X, in.Path[0].Y}
		}
		if len(in.Coordinate) < 2 {
			last := in.Path[len(in.Path)-1]
			in.Coordinate = []int32{last.X, last.Y}
		}
	}
	if in.Text == "" && len(in.Keys) > 0 {
		in.Text = normalizeHotkeyChord(strings.Join(in.Keys, "+"))
	}

	// 支持 Target="[0]" 语法糖自动映射为 SoMIndex
	if in.SoMIndex == nil && in.Target != "" {
		if m := somBracketRe.FindStringSubmatch(in.Target); m != nil {
			if idx, err := strconv.Atoi(m[1]); err == nil && idx >= 0 {
				in.SoMIndex = &idx
				in.Target = ""
			}
		}
	}

	action := strings.ToLower(strings.TrimSpace(in.Action))
	if action == "" {
		if in.SoMIndex != nil || in.Target != "" {
			action = "left_click"
		} else {
			return ComputerActionOutput{}, errors.New("action is required")
		}
	}

	// 在执行任何 Target/SoM 定位或节点工具调用前，先严格校验动作名与参数组合合法性（修复 Finding #6）
	if !isSupportedComputerAction(action) {
		return ComputerActionOutput{}, fmt.Errorf("unsupported computer action %q", action)
	}

	coordSpace := strings.ToLower(strings.TrimSpace(in.CoordSpace))
	if coordSpace == "" {
		coordSpace = "display"
	}

	// 分支 1：终态宣告（UI-TARS finished / Agent Loop finish）
	if action == "finished" || action == "finish" || action == "done" {
		if in.Target != "" {
			return ComputerActionOutput{}, fmt.Errorf("action %q cannot be combined with target %q", action, in.Target)
		}
		return ComputerActionOutput{
			Executed:       true,
			Finished:       true,
			NormalizedOp:   "finished",
			Text:           in.Text,
			TotalLatencyMs: time.Since(start).Milliseconds(),
			Message:        firstNonEmpty(in.Text, "task finished"),
		}, nil
	}

	// 分支 2：截图或 Set-of-Mark 视觉标注
	if action == "screenshot" {
		if in.Target != "" {
			return ComputerActionOutput{}, fmt.Errorf("action %q cannot be combined with target %q", action, in.Target)
		}
		shot, err := e.fetchNodeShot(ctx, in.NodeID, deadlineMs, who, in.TraceID)
		if err != nil {
			return ComputerActionOutput{}, err
		}
		return ComputerActionOutput{
			Executed:       true,
			NormalizedOp:   "screenshot",
			SnapshotID:     firstNonEmpty(shot.Meta.SnapshotID, shot.SnapshotID),
			ImageBase64:    shot.ImageBase64,
			ImageMime:      shot.Mime,
			ScreenW:        shot.Meta.NativeW,
			ScreenH:        shot.Meta.NativeH,
			DisplayW:       shot.Meta.DisplayW,
			DisplayH:       shot.Meta.DisplayH,
			Scale:          shot.Meta.Scale,
			TotalLatencyMs: time.Since(start).Milliseconds(),
			Message:        fmt.Sprintf("captured screenshot (%dx%d display, scale=%.2f)", shot.Meta.DisplayW, shot.Meta.DisplayH, shot.Meta.Scale),
		}, nil
	}
	if action == "annotate" || action == "annotate_screen" {
		if in.Target != "" {
			return ComputerActionOutput{}, fmt.Errorf("action %q cannot be combined with target %q", action, in.Target)
		}
		ann, err := e.Annotate(ctx, AnnotateInput{
			NodeID:      in.NodeID,
			ForceVision: in.ForceVision,
			DeadlineMs:  deadlineMs,
			Who:         who,
			TraceID:     in.TraceID,
		})
		if err != nil {
			return ComputerActionOutput{}, err
		}
		return ComputerActionOutput{
			Executed:       true,
			NormalizedOp:   "annotate_screen",
			SnapshotID:     ann.SnapshotID,
			ImageBase64:    ann.SoMImageBase64,
			ImageMime:      ann.SoMMime,
			Candidates:     ann.Candidates,
			Choices:        ann.Choices,
			ScreenW:        ann.ScreenW,
			ScreenH:        ann.ScreenH,
			DisplayW:       ann.DisplayW,
			DisplayH:       ann.DisplayH,
			Scale:          ann.Scale,
			TotalLatencyMs: time.Since(start).Milliseconds(),
			Message:        ann.Message,
		}, nil
	}
	if action == "zoom" {
		if in.Target != "" {
			return ComputerActionOutput{}, fmt.Errorf("action %q cannot be combined with target %q", action, in.Target)
		}
		if len(in.Region) != 4 {
			return ComputerActionOutput{}, errors.New("zoom requires region [x0, y0, x1, y1]")
		}
		zoomArgs, _ := json.Marshal(map[string]any{
			"region":      in.Region,
			"coord_space": coordSpace,
		})
		raw, err := e.dispatchNodeTool(ctx, in.NodeID, "zoom", zoomArgs, deadlineMs, who, in.TraceID)
		if err != nil {
			return ComputerActionOutput{}, err
		}
		var shot shotPayload
		_ = json.Unmarshal(raw, &shot)
		return ComputerActionOutput{
			Executed:       true,
			NormalizedOp:   "zoom",
			ImageBase64:    shot.ImageBase64,
			ImageMime:      firstNonEmpty(shot.Mime, "image/webp"),
			ScreenW:        shot.Meta.NativeW,
			ScreenH:        shot.Meta.NativeH,
			DisplayW:       shot.Meta.DisplayW,
			DisplayH:       shot.Meta.DisplayH,
			Scale:          shot.Meta.Scale,
			TotalLatencyMs: time.Since(start).Milliseconds(),
			Message:        "captured zoomed region",
		}, nil
	}

	var locatedForOut *LocateOutput

	// 若指定了语义 Target（且无显式坐标），按动作语义分流处理（修复 Finding #6：非点击动作绝不误触发 Click）
	if in.Target != "" && len(in.Coordinate) < 2 && in.SoMIndex == nil {
		if action == "type" && strings.TrimSpace(in.Text) == "" {
			return ComputerActionOutput{}, errors.New("type requires non-empty text")
		}
		if isDirectTargetClickAction(action) || action == "type" {
			btn := strings.ToLower(strings.TrimSpace(in.Button))
			dbl := false
			switch action {
			case "right_click", "right_single":
				btn = "right"
			case "middle_click":
				btn = "middle"
			case "double_click", "left_double":
				dbl = true
			}
			if btn == "" {
				btn = "left"
			}
			clickOut, err := e.Click(ctx, ClickInput{
				NodeID:      in.NodeID,
				Target:      in.Target,
				Button:      btn,
				DoubleClick: dbl,
				TypeText:    in.Text,
				ForceVision: in.ForceVision,
				Verify:      in.Verify,
				DeadlineMs:  deadlineMs,
				Who:         who,
				TraceID:     in.TraceID,
			})
			if err != nil {
				return ComputerActionOutput{}, err
			}
			out := ComputerActionOutput{
				Executed:       clickOut.Clicked,
				NormalizedOp:   "click_element",
				CoordSpace:     "native",
				Coordinate:     clickOut.Located.ClickPoint,
				Located:        &clickOut.Located,
				TotalLatencyMs: time.Since(start).Milliseconds(),
				Message:        clickOut.Message,
			}
			if !clickOut.Clicked {
				return out, nil
			}
			return e.attachPostObservation(ctx, in, out, deadlineMs, who, start)
		}

		if isTargetLocateAction(action) {
			loc, err := e.Locate(ctx, LocateInput{
				NodeID:      in.NodeID,
				Target:      in.Target,
				ForceVision: in.ForceVision,
				DeadlineMs:  deadlineMs,
				Who:         who,
				TraceID:     in.TraceID,
			})
			if err != nil {
				return ComputerActionOutput{}, err
			}
			if !loc.Found {
				return ComputerActionOutput{
					Executed:       false,
					NormalizedOp:   action,
					Located:        &loc,
					TotalLatencyMs: time.Since(start).Milliseconds(),
					Message:        loc.Message,
				}, nil
			}
			in.Coordinate = []int32{loc.ClickPoint[0], loc.ClickPoint[1]}
			coordSpace = "native"
			locatedForOut = &loc
		} else {
			return ComputerActionOutput{}, fmt.Errorf("action %q cannot be combined with semantic target %q", action, in.Target)
		}
	} else if in.Target != "" {
		if !isDirectTargetClickAction(action) && !isTargetLocateAction(action) && action != "type" {
			return ComputerActionOutput{}, fmt.Errorf("action %q cannot be combined with semantic target %q", action, in.Target)
		}
	}

	// 若指定了 SoMIndex，优先校验/复用已缓存的快照候选表（修复 Finding #10：绑定快照防并发/重排点错元素）
	resolvedSnapshotID := strings.TrimSpace(in.SnapshotID)
	if in.SoMIndex != nil && len(in.Coordinate) < 2 {
		targetIdx := *in.SoMIndex
		var candidates []AnnotatedCandidate
		cached, hasCached := e.getSoMSnapshot(in.NodeID)

		if resolvedSnapshotID != "" {
			if !hasCached || cached.out.SnapshotID != resolvedSnapshotID || cached.mutated {
				return ComputerActionOutput{
					Executed:       false,
					NormalizedOp:   action,
					SnapshotID:     resolvedSnapshotID,
					TotalLatencyMs: time.Since(start).Milliseconds(),
					Message:        fmt.Sprintf("som snapshot %q is stale or mismatched (re-run annotate_screen)", resolvedSnapshotID),
				}, nil
			}
			candidates = cached.out.Candidates
		} else if hasCached && !cached.mutated && time.Since(cached.at) <= candidateCacheTTL {
			for _, cand := range cached.out.Candidates {
				if cand.Index == targetIdx {
					candidates = cached.out.Candidates
					resolvedSnapshotID = cached.out.SnapshotID
					break
				}
			}
		}

		if candidates == nil {
			ann, err := e.Annotate(ctx, AnnotateInput{
				NodeID:      in.NodeID,
				ForceVision: in.ForceVision,
				MaxElements: max(48, targetIdx+4),
				DeadlineMs:  deadlineMs,
				Who:         who,
				TraceID:     in.TraceID,
			})
			if err != nil {
				return ComputerActionOutput{}, fmt.Errorf("resolve som_index %d: %w", targetIdx, err)
			}
			candidates = ann.Candidates
			resolvedSnapshotID = ann.SnapshotID
		}

		found := false
		for _, cand := range candidates {
			if cand.Index == targetIdx {
				in.Coordinate = []int32{cand.ClickPoint[0], cand.ClickPoint[1]}
				coordSpace = "native"
				found = true
				break
			}
		}
		if !found {
			return ComputerActionOutput{
				Executed:       false,
				NormalizedOp:   action,
				SnapshotID:     resolvedSnapshotID,
				TotalLatencyMs: time.Since(start).Milliseconds(),
				Message:        fmt.Sprintf("som_index [%d] not found in %d candidates", targetIdx, len(candidates)),
			}, nil
		}
	}

	// 若坐标空间为 normalized_1000（UI-TARS [0,1000] 归一化坐标），换算为节点 native 像素坐标
	if coordSpace == "normalized_1000" && (len(in.Coordinate) >= 2 || len(in.StartCoordinate) >= 2) {
		shot, err := e.fetchNodeShot(ctx, in.NodeID, deadlineMs, who, in.TraceID)
		if err != nil {
			return ComputerActionOutput{}, fmt.Errorf("resolve screen size for normalized_1000: %w", err)
		}
		w, h := float64(shot.Meta.NativeW), float64(shot.Meta.NativeH)
		if w <= 0 || h <= 0 {
			w, h = float64(shot.Meta.DisplayW), float64(shot.Meta.DisplayH)
		}
		if w > 0 && h > 0 {
			if len(in.Coordinate) >= 2 {
				in.Coordinate = []int32{
					int32(math.Round(float64(in.Coordinate[0]) * w / 1000.0)),
					int32(math.Round(float64(in.Coordinate[1]) * h / 1000.0)),
				}
			}
			if len(in.StartCoordinate) >= 2 {
				in.StartCoordinate = []int32{
					int32(math.Round(float64(in.StartCoordinate[0]) * w / 1000.0)),
					int32(math.Round(float64(in.StartCoordinate[1]) * h / 1000.0)),
				}
			}
			coordSpace = "native"
		}
	}

	var out ComputerActionOutput
	out.CoordSpace = coordSpace
	out.SnapshotID = resolvedSnapshotID
	out.Located = locatedForOut

	switch action {
	case "left_click", "click", "left_single", "right_click", "right_single", "middle_click", "double_click", "left_double", "triple_click":
		if len(in.Coordinate) < 2 {
			return ComputerActionOutput{}, fmt.Errorf("%s requires coordinate [x, y], som_index, or target", action)
		}
		btn := strings.ToLower(strings.TrimSpace(in.Button))
		switch action {
		case "right_click", "right_single":
			btn = "right"
		case "middle_click":
			btn = "middle"
		default:
			if btn == "" || btn == "wheel" {
				if btn == "wheel" {
					btn = "middle"
				} else {
					btn = "left"
				}
			}
		}
		clicks := 1
		if action == "double_click" || action == "left_double" {
			clicks = 2
		} else if action == "triple_click" {
			clicks = 3
		}
		// 对齐 Rust aura-node ClickParams 契约：coordinate: [x, y]
		clickPayload := map[string]any{
			"coordinate":  []int32{in.Coordinate[0], in.Coordinate[1]},
			"button":      btn,
			"coord_space": coordSpace,
		}
		if resolvedSnapshotID != "" && coordSpace == "display" {
			clickPayload["snapshot_id"] = resolvedSnapshotID
		}
		clickArgs, _ := json.Marshal(clickPayload)
		for i := 0; i < clicks; i++ {
			if i > 0 {
				time.Sleep(40 * time.Millisecond)
			}
			if _, err := e.dispatchNodeTool(ctx, in.NodeID, "click", clickArgs, deadlineMs, who, in.TraceID); err != nil {
				return ComputerActionOutput{}, err
			}
		}
		e.invalidateNodeCache(in.NodeID)
		out.Executed = true
		out.NormalizedOp = action
		out.Coordinate = [2]int32{in.Coordinate[0], in.Coordinate[1]}
		out.Message = fmt.Sprintf("executed %s at (%d,%d) [%s]", action, in.Coordinate[0], in.Coordinate[1], coordSpace)

	case "mouse_move", "move", "hover":
		if len(in.Coordinate) < 2 {
			return ComputerActionOutput{}, fmt.Errorf("%s requires coordinate [x, y]", action)
		}
		// 对齐 Rust aura-node MoveMouseParams 契约：工具名 move_mouse，参数 coordinate: [x, y]
		movePayload := map[string]any{
			"coordinate":  []int32{in.Coordinate[0], in.Coordinate[1]},
			"coord_space": coordSpace,
		}
		if resolvedSnapshotID != "" && coordSpace == "display" {
			movePayload["snapshot_id"] = resolvedSnapshotID
		}
		moveArgs, _ := json.Marshal(movePayload)
		if _, err := e.dispatchNodeTool(ctx, in.NodeID, "move_mouse", moveArgs, deadlineMs, who, in.TraceID); err != nil {
			return ComputerActionOutput{}, err
		}
		out.Executed = true
		out.NormalizedOp = "move_mouse"
		out.Coordinate = [2]int32{in.Coordinate[0], in.Coordinate[1]}
		out.Message = fmt.Sprintf("moved cursor to (%d,%d) [%s]", in.Coordinate[0], in.Coordinate[1], coordSpace)

	case "left_click_drag", "drag", "swipe":
		if len(in.StartCoordinate) < 2 || len(in.Coordinate) < 2 {
			return ComputerActionOutput{}, fmt.Errorf("%s requires start_coordinate [x0, y0] and coordinate [x1, y1]", action)
		}
		// 对齐 Rust aura-node DragParams 契约：from: [x0, y0], to: [x1, y1]
		dragPayload := map[string]any{
			"from":        []int32{in.StartCoordinate[0], in.StartCoordinate[1]},
			"to":          []int32{in.Coordinate[0], in.Coordinate[1]},
			"coord_space": coordSpace,
		}
		if resolvedSnapshotID != "" && coordSpace == "display" {
			dragPayload["snapshot_id"] = resolvedSnapshotID
		}
		dragArgs, _ := json.Marshal(dragPayload)
		if _, err := e.dispatchNodeTool(ctx, in.NodeID, "drag", dragArgs, deadlineMs, who, in.TraceID); err != nil {
			return ComputerActionOutput{}, err
		}
		e.invalidateNodeCache(in.NodeID)
		out.Executed = true
		out.NormalizedOp = "drag"
		out.Coordinate = [2]int32{in.StartCoordinate[0], in.StartCoordinate[1]}
		out.EndCoordinate = [2]int32{in.Coordinate[0], in.Coordinate[1]}
		out.Message = fmt.Sprintf("dragged (%d,%d) -> (%d,%d) [%s]",
			in.StartCoordinate[0], in.StartCoordinate[1], in.Coordinate[0], in.Coordinate[1], coordSpace)

	case "type":
		if in.Text == "" {
			return ComputerActionOutput{}, errors.New("type requires non-empty text")
		}
		textToType := in.Text
		pressReturn := false
		if strings.HasSuffix(textToType, "\n") {
			textToType = strings.TrimSuffix(textToType, "\n")
			pressReturn = true
		}
		if textToType != "" {
			typeArgs, _ := json.Marshal(map[string]any{"text": textToType})
			if _, err := e.dispatchNodeTool(ctx, in.NodeID, "type", typeArgs, deadlineMs, who, in.TraceID); err != nil {
				return ComputerActionOutput{}, err
			}
		}
		if pressReturn {
			keyArgs, _ := json.Marshal(map[string]any{"keys": "Return"})
			if _, err := e.dispatchNodeTool(ctx, in.NodeID, "key", keyArgs, deadlineMs, who, in.TraceID); err != nil {
				return ComputerActionOutput{}, err
			}
		}
		e.invalidateNodeCache(in.NodeID)
		out.Executed = true
		out.NormalizedOp = "type"
		out.Text = in.Text
		out.Message = fmt.Sprintf("typed %d chars", len([]rune(in.Text)))

	case "key", "keypress", "hotkey":
		chord := normalizeHotkeyChord(in.Text)
		if chord == "" {
			return ComputerActionOutput{}, errors.New("key requires non-empty text or keys")
		}
		keyArgs, _ := json.Marshal(map[string]any{"keys": chord})
		if _, err := e.dispatchNodeTool(ctx, in.NodeID, "key", keyArgs, deadlineMs, who, in.TraceID); err != nil {
			return ComputerActionOutput{}, err
		}
		e.invalidateNodeCache(in.NodeID)
		out.Executed = true
		out.NormalizedOp = "key"
		out.Text = chord
		out.Message = fmt.Sprintf("pressed key %q", chord)

	case "scroll":
		dir := strings.ToLower(strings.TrimSpace(in.ScrollDirection))
		amt := in.ScrollAmount
		if dir == "" {
			if in.ScrollY > 0 {
				dir = "down"
				amt = max(1, in.ScrollY/100)
			} else if in.ScrollY < 0 {
				dir = "up"
				amt = max(1, (-in.ScrollY)/100)
			} else if in.ScrollX > 0 {
				dir = "right"
				amt = max(1, in.ScrollX/100)
			} else if in.ScrollX < 0 {
				dir = "left"
				amt = max(1, (-in.ScrollX)/100)
			} else {
				dir = "down"
			}
		}
		if amt <= 0 {
			amt = 3
		}
		// Rust aura-node ScrollParams 要求必填 coordinate: [x, y]；若调用方未显式传坐标，取当前屏幕中心作为滚动锚点
		if len(in.Coordinate) < 2 {
			cx, cy := int32(640), int32(360)
			if shot, serr := e.fetchNodeShot(ctx, in.NodeID, deadlineMs, who, in.TraceID); serr == nil {
				if coordSpace == "native" && shot.Meta.NativeW > 0 && shot.Meta.NativeH > 0 {
					cx, cy = shot.Meta.NativeW/2, shot.Meta.NativeH/2
				} else if shot.Meta.DisplayW > 0 && shot.Meta.DisplayH > 0 {
					cx, cy = shot.Meta.DisplayW/2, shot.Meta.DisplayH/2
					coordSpace = "display"
				} else if shot.Meta.NativeW > 0 && shot.Meta.NativeH > 0 {
					cx, cy = shot.Meta.NativeW/2, shot.Meta.NativeH/2
					coordSpace = "native"
				}
			}
			in.Coordinate = []int32{cx, cy}
		}
		scrollPayload := map[string]any{
			"coordinate":       []int32{in.Coordinate[0], in.Coordinate[1]},
			"scroll_direction": dir,
			"scroll_amount":    amt,
			"coord_space":      coordSpace,
		}
		if resolvedSnapshotID != "" && coordSpace == "display" {
			scrollPayload["snapshot_id"] = resolvedSnapshotID
		}
		scrollArgs, _ := json.Marshal(scrollPayload)
		if _, err := e.dispatchNodeTool(ctx, in.NodeID, "scroll", scrollArgs, deadlineMs, who, in.TraceID); err != nil {
			return ComputerActionOutput{}, err
		}
		e.invalidateNodeCache(in.NodeID)
		out.Executed = true
		out.NormalizedOp = "scroll"
		out.CoordSpace = coordSpace
		out.Coordinate = [2]int32{in.Coordinate[0], in.Coordinate[1]}
		out.Message = fmt.Sprintf("scrolled %s by %d at (%d,%d)", dir, amt, in.Coordinate[0], in.Coordinate[1])

	case "wait":
		durMs := int64(1000)
		if in.Duration > 0 {
			durMs = int64(math.Round(in.Duration * 1000.0))
		}
		if durMs > 15000 {
			durMs = 15000
		}
		waitArgs, _ := json.Marshal(map[string]any{"duration_ms": durMs})
		if _, err := e.dispatchNodeTool(ctx, in.NodeID, "wait", waitArgs, deadlineMs, who, in.TraceID); err != nil {
			return ComputerActionOutput{}, err
		}
		out.Executed = true
		out.NormalizedOp = "wait"
		out.Message = fmt.Sprintf("waited %dms", durMs)

	default:
		return ComputerActionOutput{}, fmt.Errorf("unsupported computer action %q", action)
	}

	return e.attachPostObservation(ctx, in, out, deadlineMs, who, start)
}

func (e *SelectorEngine) attachPostObservation(
	ctx context.Context,
	in ComputerActionInput,
	out ComputerActionOutput,
	deadlineMs int64,
	who string,
	start time.Time,
) (ComputerActionOutput, error) {
	if in.AutoAnnotate {
		time.Sleep(120 * time.Millisecond)
		ann, err := e.Annotate(ctx, AnnotateInput{
			NodeID:      in.NodeID,
			ForceVision: in.ForceVision,
			DeadlineMs:  deadlineMs,
			Who:         who,
			TraceID:     in.TraceID,
		})
		if err != nil {
			out.Message += fmt.Sprintf(" (post-annotate failed: %v)", err)
		} else {
			out.SnapshotID = ann.SnapshotID
			out.ImageBase64 = ann.SoMImageBase64
			out.ImageMime = ann.SoMMime
			out.Candidates = ann.Candidates
			out.Choices = ann.Choices
			out.ScreenW = ann.ScreenW
			out.ScreenH = ann.ScreenH
			out.DisplayW = ann.DisplayW
			out.DisplayH = ann.DisplayH
			out.Scale = ann.Scale
		}
	} else if in.AutoScreenshot {
		time.Sleep(120 * time.Millisecond)
		shot, err := e.fetchNodeShot(ctx, in.NodeID, deadlineMs, who, in.TraceID)
		if err != nil {
			out.Message += fmt.Sprintf(" (post-screenshot failed: %v)", err)
		} else {
			out.SnapshotID = firstNonEmpty(shot.Meta.SnapshotID, shot.SnapshotID)
			out.ImageBase64 = shot.ImageBase64
			out.ImageMime = shot.Mime
			out.ScreenW = shot.Meta.NativeW
			out.ScreenH = shot.Meta.NativeH
			out.DisplayW = shot.Meta.DisplayW
			out.DisplayH = shot.Meta.DisplayH
			out.Scale = shot.Meta.Scale
		}
	}
	out.TotalLatencyMs = time.Since(start).Milliseconds()
	return out, nil
}

// resolveAgentLLMConfig 将 endpoint、credential 与 model 作为统一 provider 配置解析（修复 Finding #1）：
// 当调用方覆盖自定义 LLMEndpoint（且与服务端默认 AURA_AGENT_LLM_ENDPOINT 不同）时，严禁继承服务端默认 API Key，
// 阻断恶意请求借自定义地址窃取服务端凭据与截图的路径。
func resolveAgentLLMConfig(in AgentTaskInput) (endpoint, apiKey, model string, err error) {
	reqEndpoint := strings.TrimRight(strings.TrimSpace(in.LLMEndpoint), "/")
	envEndpoint := strings.TrimRight(strings.TrimSpace(os.Getenv("AURA_AGENT_LLM_ENDPOINT")), "/")
	model = firstNonEmpty(in.LLMModel, os.Getenv("AURA_AGENT_LLM_MODEL"), "gpt-4o")

	if reqEndpoint != "" {
		u, uerr := url.Parse(reqEndpoint)
		if uerr != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return "", "", "", fmt.Errorf("invalid llm_endpoint %q (must be http:// or https://)", in.LLMEndpoint)
		}
		if reqEndpoint != envEndpoint {
			if strings.EqualFold(strings.TrimSpace(os.Getenv("AURA_ALLOW_CUSTOM_LLM_ENDPOINT")), "false") {
				return "", "", "", errors.New("custom llm_endpoint is disabled by server policy")
			}
			// 自定义 endpoint 仅允许使用调用方自带的 LLMAPIKey，绝不回落读取服务端 AURA_AGENT_LLM_API_KEY
			return reqEndpoint, strings.TrimSpace(in.LLMAPIKey), model, nil
		}
		return reqEndpoint, firstNonEmpty(in.LLMAPIKey, os.Getenv("AURA_AGENT_LLM_API_KEY")), model, nil
	}
	if envEndpoint != "" {
		return envEndpoint, firstNonEmpty(in.LLMAPIKey, os.Getenv("AURA_AGENT_LLM_API_KEY")), model, nil
	}
	return "", "", model, nil
}

func (e *SelectorEngine) resolveAgentPlanner() AgentPlannerClient {
	e.mu.RLock()
	override := e.plannerOverride
	e.mu.RUnlock()
	if override != nil {
		return override
	}
	return NewHTTPVLMPlannerClient(e.httpClient)
}

// RunAgentTask 执行端到端多步 GUI 任务闭环（支持外部 OpenAI 兼容 VLM + Set-of-Mark 规划器，
// 以及无需外部 LLM 的内置 Hybrid Semantic 三层定位执行器）。
func (e *SelectorEngine) RunAgentTask(ctx context.Context, in AgentTaskInput) (AgentTaskOutput, error) {
	start := time.Now()
	if strings.TrimSpace(in.NodeID) == "" {
		return AgentTaskOutput{}, errors.New("node_id is required")
	}
	goal := strings.TrimSpace(in.Goal)
	if goal == "" {
		return AgentTaskOutput{}, errors.New("goal is required")
	}
	maxSteps := in.MaxSteps
	if maxSteps <= 0 {
		maxSteps = 8
	}
	if maxSteps > 25 {
		maxSteps = 25
	}
	deadlineMs := in.DeadlineMs
	if deadlineMs <= 0 {
		deadlineMs = defaultClickDeadlineMs
	}
	who := in.Who
	if who == "" {
		who = "aura-agent"
	}

	llmEndpoint, llmAPIKey, llmModel, err := resolveAgentLLMConfig(in)
	if err != nil {
		return AgentTaskOutput{}, err
	}

	e.mu.RLock()
	hasPlannerOverride := e.plannerOverride != nil
	e.mu.RUnlock()

	if llmEndpoint != "" || hasPlannerOverride {
		return e.runVLMSoMLoop(ctx, in, goal, maxSteps, deadlineMs, who, llmEndpoint, llmAPIKey, llmModel, start)
	}
	return e.runHybridSemanticLoop(ctx, in, goal, maxSteps, deadlineMs, who, start)
}

var subGoalSplitRe = regexp.MustCompile(`\s*(?:->|→|;|\n|\bthen\b|然后)\s*`)

func splitSubGoals(goal string) []string {
	rawParts := subGoalSplitRe.Split(goal, -1)
	var out []string
	for _, p := range rawParts {
		s := strings.TrimSpace(p)
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

func (e *SelectorEngine) runHybridSemanticLoop(
	ctx context.Context,
	in AgentTaskInput,
	goal string,
	maxSteps int,
	deadlineMs int64,
	who string,
	start time.Time,
) (AgentTaskOutput, error) {
	allSubGoals := splitSubGoals(goal)
	totalSubGoals := len(allSubGoals)
	truncatedByBudget := false
	execSubGoals := allSubGoals
	if len(execSubGoals) > maxSteps {
		execSubGoals = execSubGoals[:maxSteps]
		truncatedByBudget = true
	}
	steps := make([]AgentStepLog, 0, len(execSubGoals))

	for idx, sub := range execSubGoals {
		if err := ctx.Err(); err != nil {
			return AgentTaskOutput{}, err
		}
		stepStart := time.Now()
		stepNum := idx + 1
		actIn, waitIn, isWaitFor, thought := parseSubGoalIntent(in.NodeID, sub, in.ForceVision, in.Verify, deadlineMs, who, in.TraceID)

		if isWaitFor {
			wOut, err := e.WaitFor(ctx, waitIn)
			logEntry := AgentStepLog{
				Step:      stepNum,
				SubGoal:   sub,
				Thought:   thought,
				Action:    "wait_for_element",
				Target:    waitIn.Target,
				LatencyMs: time.Since(stepStart).Milliseconds(),
				Detail:    wOut.Message,
			}
			if err != nil || !wOut.Satisfied {
				logEntry.Status = "failed"
				if err != nil {
					logEntry.Detail = err.Error()
				}
				steps = append(steps, logEntry)
				return AgentTaskOutput{
					NodeID:         in.NodeID,
					Goal:           goal,
					Completed:      false,
					Status:         "failed",
					Planner:        "hybrid_semantic",
					Steps:          steps,
					TotalLatencyMs: time.Since(start).Milliseconds(),
					Message:        fmt.Sprintf("step %d (%s) failed: %s", stepNum, sub, logEntry.Detail),
				}, nil
			}
			logEntry.Status = "ok"
			logEntry.Coordinate = wOut.Last.ClickPoint
			steps = append(steps, logEntry)
			continue
		}

		cOut, err := e.ExecuteComputerAction(ctx, actIn)
		logEntry := AgentStepLog{
			Step:       stepNum,
			SubGoal:    sub,
			Thought:    thought,
			Action:     firstNonEmpty(cOut.NormalizedOp, actIn.Action),
			Target:     actIn.Target,
			SoMIndex:   actIn.SoMIndex,
			Coordinate: cOut.Coordinate,
			LatencyMs:  time.Since(stepStart).Milliseconds(),
			Detail:     cOut.Message,
		}
		if err != nil || !cOut.Executed {
			logEntry.Status = "failed"
			if err != nil {
				logEntry.Detail = err.Error()
			}
			steps = append(steps, logEntry)
			return AgentTaskOutput{
				NodeID:         in.NodeID,
				Goal:           goal,
				Completed:      false,
				Status:         "failed",
				Planner:        "hybrid_semantic",
				Steps:          steps,
				TotalLatencyMs: time.Since(start).Milliseconds(),
				Message:        fmt.Sprintf("step %d (%s) failed: %s", stepNum, sub, logEntry.Detail),
			}, nil
		}
		logEntry.Status = "ok"
		steps = append(steps, logEntry)
	}

	// 修复 Finding #8：当子目标数量超出 max_steps 时，返回 Completed:false 与 max_steps_reached，不误报完整成功
	if truncatedByBudget {
		return AgentTaskOutput{
			NodeID:         in.NodeID,
			Goal:           goal,
			Completed:      false,
			Status:         "max_steps_reached",
			Planner:        "hybrid_semantic",
			Steps:          steps,
			TotalLatencyMs: time.Since(start).Milliseconds(),
			Message:        fmt.Sprintf("reached max_steps (%d/%d sub-goals executed)", len(steps), totalSubGoals),
		}, nil
	}

	return AgentTaskOutput{
		NodeID:         in.NodeID,
		Goal:           goal,
		Completed:      true,
		Status:         "completed",
		Planner:        "hybrid_semantic",
		Steps:          steps,
		TotalLatencyMs: time.Since(start).Milliseconds(),
		Message:        fmt.Sprintf("completed %d step(s) via hybrid_semantic planner", len(steps)),
	}, nil
}

// parseSubGoalIntent 将单条自然语言/DSL 子目标解析为 ComputerActionInput 或 WaitForInput。
func parseSubGoalIntent(
	nodeID, sub string,
	forceVision, verify bool,
	deadlineMs int64,
	who, traceID string,
) (ComputerActionInput, WaitForInput, bool, string) {
	trimmed := strings.TrimSpace(sub)
	lower := strings.ToLower(trimmed)

	// 1. UI-TARS 函数调用语法支持（如 click(start_box='(500,300)')）
	if uiTarsCallRe.MatchString(trimmed) {
		if parsed, thought, err := ParseUITarsAction(trimmed); err == nil {
			parsed.NodeID = nodeID
			parsed.ForceVision = forceVision
			parsed.Verify = verify
			parsed.DeadlineMs = deadlineMs
			parsed.Who = who
			parsed.TraceID = traceID
			return parsed, WaitForInput{}, false, firstNonEmpty(thought, "parsed UI-TARS action")
		}
	}

	// 2. wait_for / 等待元素出现或消失
	for _, prefix := range []string{"wait_for_absent ", "wait_absent ", "等待消失 "} {
		if strings.HasPrefix(lower, prefix) {
			target := strings.Trim(strings.TrimSpace(trimmed[len(prefix):]), `"'`)
			return ComputerActionInput{}, WaitForInput{
				NodeID:      nodeID,
				Target:      target,
				State:       "absent",
				TimeoutMs:   5000,
				ForceVision: forceVision,
				DeadlineMs:  deadlineMs,
				Who:         who,
				TraceID:     traceID,
			}, true, fmt.Sprintf("wait for %q to disappear", target)
		}
	}
	for _, prefix := range []string{"wait_for ", "wait for ", "等待 "} {
		if strings.HasPrefix(lower, prefix) {
			target := strings.Trim(strings.TrimSpace(trimmed[len(prefix):]), `"'`)
			return ComputerActionInput{}, WaitForInput{
				NodeID:      nodeID,
				Target:      target,
				State:       "present",
				TimeoutMs:   5000,
				ForceVision: forceVision,
				DeadlineMs:  deadlineMs,
				Who:         who,
				TraceID:     traceID,
			}, true, fmt.Sprintf("wait for %q to appear", target)
		}
	}

	// 3. key / press / 按键
	for _, prefix := range []string{"key ", "press ", "hotkey ", "按键 "} {
		if strings.HasPrefix(lower, prefix) {
			k := strings.Trim(strings.TrimSpace(trimmed[len(prefix):]), `"'`)
			return ComputerActionInput{
				NodeID:     nodeID,
				Action:     "key",
				Text:       k,
				DeadlineMs: deadlineMs,
				Who:        who,
				TraceID:    traceID,
			}, WaitForInput{}, false, fmt.Sprintf("press key %q", k)
		}
	}

	// 4. type / 输入文本
	for _, prefix := range []string{"type ", "input ", "输入 "} {
		if strings.HasPrefix(lower, prefix) {
			txt := strings.Trim(strings.TrimSpace(trimmed[len(prefix):]), `"'`)
			return ComputerActionInput{
				NodeID:     nodeID,
				Action:     "type",
				Text:       txt,
				DeadlineMs: deadlineMs,
				Who:        who,
				TraceID:    traceID,
			}, WaitForInput{}, false, fmt.Sprintf("type text %q", txt)
		}
	}

	// 5. scroll / 滚动
	for _, prefix := range []string{"scroll ", "滚动 "} {
		if strings.HasPrefix(lower, prefix) {
			dir := strings.ToLower(strings.TrimSpace(trimmed[len(prefix):]))
			return ComputerActionInput{
				NodeID:          nodeID,
				Action:          "scroll",
				ScrollDirection: firstNonEmpty(dir, "down"),
				ScrollAmount:    3,
				DeadlineMs:      deadlineMs,
				Who:             who,
				TraceID:         traceID,
			}, WaitForInput{}, false, fmt.Sprintf("scroll %s", dir)
		}
	}

	// 6. double_click / 双击 或 right_click / 右击 或 click / 点击
	action := "left_click"
	target := trimmed
	for _, p := range []struct {
		prefix string
		act    string
	}{
		{"double_click ", "double_click"},
		{"double click ", "double_click"},
		{"双击 ", "double_click"},
		{"双击", "double_click"},
		{"right_click ", "right_click"},
		{"right click ", "right_click"},
		{"右击 ", "right_click"},
		{"右键 ", "right_click"},
		{"click ", "left_click"},
		{"tap ", "left_click"},
		{"点击 ", "left_click"},
		{"点击", "left_click"},
	} {
		if strings.HasPrefix(lower, p.prefix) {
			action = p.act
			target = strings.TrimSpace(trimmed[len(p.prefix):])
			break
		}
	}
	target = strings.Trim(target, `"'`)

	return ComputerActionInput{
		NodeID:      nodeID,
		Action:      action,
		Target:      target,
		ForceVision: forceVision,
		Verify:      verify,
		DeadlineMs:  deadlineMs,
		Who:         who,
		TraceID:     traceID,
	}, WaitForInput{}, false, fmt.Sprintf("%s on target %q", action, target)
}

func (e *SelectorEngine) runVLMSoMLoop(
	ctx context.Context,
	in AgentTaskInput,
	goal string,
	maxSteps int,
	deadlineMs int64,
	who string,
	llmEndpoint, llmAPIKey, llmModel string,
	start time.Time,
) (AgentTaskOutput, error) {
	planner := e.resolveAgentPlanner()
	steps := make([]AgentStepLog, 0, maxSteps)
	for stepNum := 1; stepNum <= maxSteps; stepNum++ {
		if err := ctx.Err(); err != nil {
			return AgentTaskOutput{}, err
		}
		stepStart := time.Now()
		ann, err := e.Annotate(ctx, AnnotateInput{
			NodeID:      in.NodeID,
			ForceVision: in.ForceVision,
			MaxElements: 48,
			DeadlineMs:  deadlineMs,
			Who:         who,
			TraceID:     in.TraceID,
		})
		if err != nil {
			return AgentTaskOutput{
				NodeID:         in.NodeID,
				Goal:           goal,
				Completed:      false,
				Status:         "failed",
				Planner:        "vlm_som",
				Steps:          steps,
				TotalLatencyMs: time.Since(start).Milliseconds(),
				Message:        fmt.Sprintf("step %d annotate failed: %v", stepNum, err),
			}, nil
		}

		actIn, thought, err := planner.PlanNextAction(ctx, VLMPlanRequest{
			Endpoint:  llmEndpoint,
			APIKey:    llmAPIKey,
			Model:     llmModel,
			Goal:      goal,
			Annotated: ann,
			History:   steps,
		})
		if err != nil {
			return AgentTaskOutput{
				NodeID:         in.NodeID,
				Goal:           goal,
				Completed:      false,
				Status:         "failed",
				Planner:        "vlm_som",
				Steps:          steps,
				TotalLatencyMs: time.Since(start).Milliseconds(),
				Message:        fmt.Sprintf("step %d VLM planner failed: %v", stepNum, err),
			}, nil
		}
		actIn.NodeID = in.NodeID
		actIn.ForceVision = in.ForceVision
		actIn.Verify = in.Verify
		actIn.DeadlineMs = deadlineMs
		actIn.Who = who
		actIn.TraceID = in.TraceID
		if actIn.SnapshotID == "" {
			actIn.SnapshotID = ann.SnapshotID
		}

		// 若 VLM 决策为 SoM 索引，直接从本次已捕获的 ann.Candidates 解析 native 坐标，避免重复 Annotate
		if actIn.SoMIndex != nil && len(actIn.Coordinate) < 2 {
			targetIdx := *actIn.SoMIndex
			for _, cand := range ann.Candidates {
				if cand.Index == targetIdx {
					actIn.Coordinate = []int32{cand.ClickPoint[0], cand.ClickPoint[1]}
					actIn.CoordSpace = "native"
					break
				}
			}
		}

		cOut, execErr := e.ExecuteComputerAction(ctx, actIn)
		logEntry := AgentStepLog{
			Step:       stepNum,
			Thought:    thought,
			Action:     firstNonEmpty(cOut.NormalizedOp, actIn.Action),
			Target:     actIn.Target,
			SoMIndex:   actIn.SoMIndex,
			Coordinate: cOut.Coordinate,
			LatencyMs:  time.Since(stepStart).Milliseconds(),
			Detail:     cOut.Message,
		}
		if execErr != nil || !cOut.Executed {
			logEntry.Status = "failed"
			if execErr != nil {
				logEntry.Detail = execErr.Error()
			}
			steps = append(steps, logEntry)
			return AgentTaskOutput{
				NodeID:         in.NodeID,
				Goal:           goal,
				Completed:      false,
				Status:         "failed",
				Planner:        "vlm_som",
				Steps:          steps,
				TotalLatencyMs: time.Since(start).Milliseconds(),
				Message:        fmt.Sprintf("step %d execution failed: %s", stepNum, logEntry.Detail),
			}, nil
		}
		if cOut.Finished {
			logEntry.Status = "completed"
			steps = append(steps, logEntry)
			return AgentTaskOutput{
				NodeID:         in.NodeID,
				Goal:           goal,
				Completed:      true,
				Status:         "completed",
				Planner:        "vlm_som",
				Steps:          steps,
				TotalLatencyMs: time.Since(start).Milliseconds(),
				Message:        cOut.Message,
			}, nil
		}
		logEntry.Status = "ok"
		steps = append(steps, logEntry)
	}

	return AgentTaskOutput{
		NodeID:         in.NodeID,
		Goal:           goal,
		Completed:      false,
		Status:         "max_steps_reached",
		Planner:        "vlm_som",
		Steps:          steps,
		TotalLatencyMs: time.Since(start).Milliseconds(),
		Message:        fmt.Sprintf("reached max_steps (%d) without finished signal", maxSteps),
	}, nil
}

// HTTPVLMPlannerClient 是 OpenAI 兼容 Chat Completions VLM 规划器的窄接口适配器实现。
type HTTPVLMPlannerClient struct {
	client *http.Client
}

// NewHTTPVLMPlannerClient 构造安全隔离的 HTTP VLM 规划器客户端（拒绝跨源重定向携带 Authorization）。
func NewHTTPVLMPlannerClient(base *http.Client) *HTTPVLMPlannerClient {
	timeout := 25 * time.Second
	var transport http.RoundTripper
	if base != nil {
		if base.Timeout > 0 {
			timeout = base.Timeout
		}
		transport = base.Transport
	}
	safeClient := &http.Client{
		Timeout:   timeout,
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 3 {
				return errors.New("stopped after 3 redirects")
			}
			if len(via) > 0 && !strings.EqualFold(req.URL.Host, via[0].URL.Host) {
				req.Header.Del("Authorization")
			}
			return nil
		},
	}
	return &HTTPVLMPlannerClient{client: safeClient}
}

// PlanNextAction 发送带 Set-of-Mark 视觉标注图与候选索引的提示词，解析下一步 GUI 动作决策。
func (c *HTTPVLMPlannerClient) PlanNextAction(ctx context.Context, req VLMPlanRequest) (ComputerActionInput, string, error) {
	urlStr := strings.TrimRight(strings.TrimSpace(req.Endpoint), "/")
	if !strings.HasSuffix(urlStr, "/chat/completions") {
		if strings.HasSuffix(urlStr, "/v1") {
			urlStr += "/chat/completions"
		} else {
			urlStr += "/v1/chat/completions"
		}
	}

	ann := req.Annotated
	var histBuf strings.Builder
	for _, h := range req.History {
		somStr := "-"
		if h.SoMIndex != nil {
			somStr = strconv.Itoa(*h.SoMIndex)
		}
		fmt.Fprintf(&histBuf, "- Step %d: action=%s som_index=%s target=%q status=%s detail=%s\n",
			h.Step, h.Action, somStr, h.Target, h.Status, h.Detail)
	}
	prompt := fmt.Sprintf(
		"You are an autonomous GUI Computer-Use agent controlling a screen (%dx%d).\n"+
			"Goal: %s\n\n"+
			"Previous steps:\n%s\n"+
			"Numbered Set-of-Mark UI candidates:\n%s\n\n"+
			"Respond ONLY with a single JSON object (or UI-TARS Thought/Action):\n"+
			`{"thought":"...","action":"left_click|double_click|right_click|type|key|scroll|wait|finished","som_index":0,"target":"","text":""}`,
		ann.DisplayW, ann.DisplayH, req.Goal, histBuf.String(), strings.Join(ann.Choices, "\n"),
	)

	userContent := []map[string]any{
		{"type": "text", "text": prompt},
	}
	if ann.SoMImageBase64 != "" {
		mime := firstNonEmpty(ann.SoMMime, "image/jpeg")
		userContent = append(userContent, map[string]any{
			"type": "image_url",
			"image_url": map[string]any{
				"url": fmt.Sprintf("data:%s;base64,%s", mime, ann.SoMImageBase64),
			},
		})
	}

	reqPayload, _ := json.Marshal(map[string]any{
		"model":       req.Model,
		"temperature": 0.0,
		"messages": []map[string]any{
			{"role": "user", "content": userContent},
		},
	})

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, urlStr, bytes.NewReader(reqPayload))
	if err != nil {
		return ComputerActionInput{}, "", err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if req.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+req.APIKey)
	}
	resp, err := c.client.Do(httpReq)
	if err != nil {
		return ComputerActionInput{}, "", err
	}
	defer resp.Body.Close()
	bodyBytes, err := io.ReadAll(io.LimitReader(resp.Body, 2*1024*1024))
	if err != nil {
		return ComputerActionInput{}, "", fmt.Errorf("read VLM response body: %w", err)
	}
	if resp.StatusCode >= 300 {
		return ComputerActionInput{}, "", fmt.Errorf("VLM HTTP %d: %s", resp.StatusCode, string(bodyBytes))
	}

	var chatResp struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(bodyBytes, &chatResp); err != nil || len(chatResp.Choices) == 0 {
		return ComputerActionInput{}, "", fmt.Errorf("decode VLM chat completion: %w", err)
	}
	reply := strings.TrimSpace(chatResp.Choices[0].Message.Content)
	reply = strings.TrimPrefix(reply, "```json")
	reply = strings.TrimPrefix(reply, "```")
	reply = strings.TrimSuffix(reply, "```")
	reply = strings.TrimSpace(reply)

	var vlmDec struct {
		Thought         string  `json:"thought"`
		Action          string  `json:"action"`
		SoMIndex        *int    `json:"som_index"`
		Target          string  `json:"target"`
		Coordinate      []int32 `json:"coordinate"`
		Text            string  `json:"text"`
		ScrollDirection string  `json:"scroll_direction"`
		Finished        bool    `json:"finished"`
	}
	if json.Unmarshal([]byte(reply), &vlmDec) == nil && (vlmDec.Action != "" || vlmDec.Finished) {
		act := vlmDec.Action
		if vlmDec.Finished {
			act = "finished"
		}
		return ComputerActionInput{
			Action:          act,
			SoMIndex:        vlmDec.SoMIndex,
			Target:          vlmDec.Target,
			Coordinate:      vlmDec.Coordinate,
			Text:            vlmDec.Text,
			ScrollDirection: vlmDec.ScrollDirection,
		}, vlmDec.Thought, nil
	}

	// 回退尝试解析 UI-TARS "Thought: ... Action: ..." 格式
	return ParseUITarsAction(reply)
}
