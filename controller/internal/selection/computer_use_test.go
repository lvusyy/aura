package selection

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	aurav1 "github.com/aura/controller/gen/aura/v1"
	"github.com/aura/controller/internal/fusion"
	"github.com/aura/controller/internal/store"
)

// strictContractDisp 模拟真实 Rust aura-node (tool_dispatch.rs) 的严格参数契约校验：
//   - click 必填 coordinate: [x, y]
//   - move_mouse 必填 coordinate: [x, y]（拒绝错误工具名 "move"）
//   - drag 必填 from: [x0, y0] 与 to: [x1, y1]
//   - scroll 必填 coordinate: [x, y]、scroll_direction、scroll_amount
//   - 未知工具或缺失必填字段一律返回错误，杜绝宽松 mock 漏检契约偏差
type strictContractDisp struct {
	mu          sync.Mutex
	calls       []string
	lastArgs    map[string][]byte
	tree        fusion.A11yTree
	a11yErr     error
	shotB64     string
	snapshotID  string
	scale       float64
	nativeW     int32
	nativeH     int32
	displayW    int32
	displayH    int32
	a11yPolls   int
	appearAfter int
}

func (s *strictContractDisp) Dispatch(_ context.Context, _, tool string, jsonArgs []byte, _ int64, _, _ string) (*aurav1.ToolResponse, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, tool)
	if s.lastArgs == nil {
		s.lastArgs = make(map[string][]byte)
	}
	s.lastArgs[tool] = append([]byte(nil), jsonArgs...)

	var data any
	switch tool {
	case "get_a11y_tree":
		s.a11yPolls++
		if s.a11yErr != nil {
			return nil, "", s.a11yErr
		}
		if s.appearAfter > 0 && s.a11yPolls < s.appearAfter {
			data = fusion.A11yTree{Nodes: nil}
		} else {
			data = s.tree
		}
	case "screenshot":
		meta := map[string]any{
			"native_w":  s.nativeW,
			"native_h":  s.nativeH,
			"display_w": s.displayW,
			"display_h": s.displayH,
			"scale":     s.scale,
		}
		if s.snapshotID != "" {
			meta["snapshot_id"] = s.snapshotID
		}
		data = map[string]any{
			"image_base64": s.shotB64,
			"mime":         "image/webp",
			"meta":         meta,
		}
	case "click":
		var p struct {
			Coordinate []int32 `json:"coordinate"`
			Button     string  `json:"button"`
			CoordSpace string  `json:"coord_space"`
		}
		if err := json.Unmarshal(jsonArgs, &p); err != nil || len(p.Coordinate) != 2 {
			return nil, "", fmt.Errorf("strict node click contract violation: require coordinate:[x,y], got %s", string(jsonArgs))
		}
		data = map[string]any{"ok": true}
	case "move_mouse":
		var p struct {
			Coordinate []int32 `json:"coordinate"`
			CoordSpace string  `json:"coord_space"`
		}
		if err := json.Unmarshal(jsonArgs, &p); err != nil || len(p.Coordinate) != 2 {
			return nil, "", fmt.Errorf("strict node move_mouse contract violation: require coordinate:[x,y], got %s", string(jsonArgs))
		}
		data = map[string]any{"ok": true}
	case "drag":
		var p struct {
			From       []int32 `json:"from"`
			To         []int32 `json:"to"`
			CoordSpace string  `json:"coord_space"`
		}
		if err := json.Unmarshal(jsonArgs, &p); err != nil || len(p.From) != 2 || len(p.To) != 2 {
			return nil, "", fmt.Errorf("strict node drag contract violation: require from:[x,y] & to:[x,y], got %s", string(jsonArgs))
		}
		data = map[string]any{"ok": true}
	case "scroll":
		var p struct {
			Coordinate      []int32 `json:"coordinate"`
			ScrollDirection string  `json:"scroll_direction"`
			ScrollAmount    int32   `json:"scroll_amount"`
		}
		if err := json.Unmarshal(jsonArgs, &p); err != nil || len(p.Coordinate) != 2 || p.ScrollDirection == "" || p.ScrollAmount <= 0 {
			return nil, "", fmt.Errorf("strict node scroll contract violation: require coordinate:[x,y], scroll_direction, scroll_amount, got %s", string(jsonArgs))
		}
		data = map[string]any{"ok": true}
	case "type":
		var p struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(jsonArgs, &p); err != nil || p.Text == "" {
			return nil, "", fmt.Errorf("strict node type contract violation: %s", string(jsonArgs))
		}
		data = map[string]any{"ok": true}
	case "key":
		var p struct {
			Keys string `json:"keys"`
			Key  string `json:"key"`
		}
		if err := json.Unmarshal(jsonArgs, &p); err != nil || (p.Keys == "" && p.Key == "") {
			return nil, "", fmt.Errorf("strict node key contract violation: %s", string(jsonArgs))
		}
		data = map[string]any{"ok": true}
	case "wait":
		var p struct {
			DurationMs int64 `json:"duration_ms"`
		}
		if err := json.Unmarshal(jsonArgs, &p); err != nil || p.DurationMs <= 0 {
			return nil, "", fmt.Errorf("strict node wait contract violation: %s", string(jsonArgs))
		}
		data = map[string]any{"ok": true}
	default:
		return nil, "", fmt.Errorf("E_UNSUPPORTED: unknown node tool %q", tool)
	}

	raw, _ := json.Marshal(map[string]any{
		"ok":   true,
		"data": data,
	})
	return &aurav1.ToolResponse{JsonEnvelope: raw}, "", nil
}

func TestParseUITarsActionAndComputerUseAdapter(t *testing.T) {
	in, thought, err := ParseUITarsAction("Thought: Click the login button in the center.\nAction: click(start_box='(400, 200, 600, 400)')")
	if err != nil {
		t.Fatalf("ParseUITarsAction click failed: %v", err)
	}
	if thought != "Click the login button in the center." {
		t.Fatalf("unexpected thought: %q", thought)
	}
	if in.Action != "left_click" || in.CoordSpace != "normalized_1000" || len(in.Coordinate) != 2 || in.Coordinate[0] != 500 || in.Coordinate[1] != 300 {
		t.Fatalf("unexpected parsed click: %+v", in)
	}

	hk, _, err := ParseUITarsAction("Action: hotkey(key='ctrl shift p')")
	if err != nil || hk.Action != "key" || hk.Text != "Ctrl+Shift+p" {
		t.Fatalf("unexpected parsed hotkey: %+v, err=%v", hk, err)
	}

	b := [4]int32{200, 100, 120, 40}
	disp := &strictContractDisp{
		tree: fusion.A11yTree{
			Nodes: []fusion.A11yNode{
				{Role: "button", Name: "Submit", Bounds: &b},
			},
		},
		shotB64:    base64.StdEncoding.EncodeToString([]byte("fake-webp-bytes")),
		snapshotID: "snap-101",
		nativeW:    1920,
		nativeH:    1080,
		displayW:   960,
		displayH:   540,
		scale:      2.0,
	}
	eng := NewSelectorEngine(disp, nil, "", "")

	// 1. UI-TARS (500,500) -> native (960,540)，且严格校验下发 click coordinate:[960,540]
	uiOut, err := eng.ExecuteComputerAction(context.Background(), ComputerActionInput{
		NodeID:       "node-1",
		UITarsAction: "Action: click(start_box='(500, 500)')",
	})
	if err != nil || !uiOut.Executed {
		t.Fatalf("ExecuteComputerAction UI-TARS failed: out=%+v, err=%v", uiOut, err)
	}
	if uiOut.Coordinate != [2]int32{960, 540} || uiOut.CoordSpace != "native" {
		t.Fatalf("expected normalized_1000 (500,500) -> native (960,540), got %v (%s)", uiOut.Coordinate, uiOut.CoordSpace)
	}

	// 2. 验证 move_mouse / drag / scroll 严格匹配 Rust 节点工具名与参数结构（修复 Finding #2）
	mvOut, err := eng.ExecuteComputerAction(context.Background(), ComputerActionInput{
		NodeID:     "node-1",
		Action:     "mouse_move",
		Coordinate: []int32{300, 200},
	})
	if err != nil || !mvOut.Executed || mvOut.NormalizedOp != "move_mouse" {
		t.Fatalf("ExecuteComputerAction mouse_move failed: out=%+v, err=%v", mvOut, err)
	}

	dragOut, err := eng.ExecuteComputerAction(context.Background(), ComputerActionInput{
		NodeID:          "node-1",
		Action:          "left_click_drag",
		StartCoordinate: []int32{100, 100},
		Coordinate:      []int32{400, 300},
	})
	if err != nil || !dragOut.Executed || dragOut.NormalizedOp != "drag" {
		t.Fatalf("ExecuteComputerAction drag failed: out=%+v, err=%v", dragOut, err)
	}

	scrollOut, err := eng.ExecuteComputerAction(context.Background(), ComputerActionInput{
		NodeID:          "node-1",
		Action:          "scroll",
		ScrollDirection: "down",
		ScrollAmount:    4,
	})
	if err != nil || !scrollOut.Executed || scrollOut.Coordinate != [2]int32{480, 270} {
		t.Fatalf("ExecuteComputerAction scroll failed: out=%+v, err=%v", scrollOut, err)
	}

	// 3. 验证 Target + 非点击动作绝不误触发 Click，非法组合零副作用报错（修复 Finding #6）
	preCalls := len(disp.calls)
	if _, err := eng.ExecuteComputerAction(context.Background(), ComputerActionInput{
		NodeID: "node-1",
		Action: "invalid_op",
		Target: "Submit",
	}); err == nil {
		t.Fatal("expected error for unsupported action + target")
	}
	if _, err := eng.ExecuteComputerAction(context.Background(), ComputerActionInput{
		NodeID: "node-1",
		Action: "key",
		Target: "Submit",
		Text:   "Return",
	}); err == nil {
		t.Fatal("expected error for key + target")
	}
	if len(disp.calls) != preCalls {
		t.Fatalf("expected zero node calls on invalid action/target combinations, got %d new calls", len(disp.calls)-preCalls)
	}

	// mouse_move + Target 应仅定位并调用 move_mouse，不调用 click
	disp.calls = nil
	hoverOut, err := eng.ExecuteComputerAction(context.Background(), ComputerActionInput{
		NodeID: "node-1",
		Action: "mouse_move",
		Target: "Submit",
	})
	if err != nil || !hoverOut.Executed || hoverOut.NormalizedOp != "move_mouse" || hoverOut.Coordinate != [2]int32{260, 120} {
		t.Fatalf("mouse_move + Target failed: out=%+v, err=%v", hoverOut, err)
	}
	for _, c := range disp.calls {
		if c == "click" {
			t.Fatalf("mouse_move + Target must not invoke click, calls=%v", disp.calls)
		}
	}

	// 4. 验证 SoMIndex 快照绑定与过期拒绝（修复 Finding #10）
	ann, err := eng.Annotate(context.Background(), AnnotateInput{NodeID: "node-1"})
	if err != nil || ann.SnapshotID != "snap-101" {
		t.Fatalf("Annotate failed: %+v, err=%v", ann, err)
	}
	idx0 := 0
	mismatchOut, err := eng.ExecuteComputerAction(context.Background(), ComputerActionInput{
		NodeID:     "node-1",
		Action:     "left_click",
		SoMIndex:   &idx0,
		SnapshotID: "snap-stale-999",
	})
	if err != nil || mismatchOut.Executed || !strings.Contains(mismatchOut.Message, "stale or mismatched") {
		t.Fatalf("expected stale snapshot_id to be rejected without execution, got out=%+v, err=%v", mismatchOut, err)
	}

	matchOut, err := eng.ExecuteComputerAction(context.Background(), ComputerActionInput{
		NodeID:     "node-1",
		Action:     "left_click",
		SoMIndex:   &idx0,
		SnapshotID: "snap-101",
	})
	if err != nil || !matchOut.Executed || matchOut.Coordinate != [2]int32{260, 120} {
		t.Fatalf("expected matching snapshot_id SoM click to succeed, got out=%+v, err=%v", matchOut, err)
	}

	// 点击后原快照已标记为 mutated，再次携带同 snapshot_id 点击应被拒绝
	mutatedOut, err := eng.ExecuteComputerAction(context.Background(), ComputerActionInput{
		NodeID:     "node-1",
		Action:     "left_click",
		SoMIndex:   &idx0,
		SnapshotID: "snap-101",
	})
	if err != nil || mutatedOut.Executed {
		t.Fatalf("expected post-click mutated snapshot_id to be rejected, got out=%+v, err=%v", mutatedOut, err)
	}
}

func TestRunAgentTaskSecurityBudgetAndWaitForSemantics(t *testing.T) {
	b := [4]int32{200, 100, 120, 40}
	disp := &strictContractDisp{
		tree: fusion.A11yTree{
			Nodes: []fusion.A11yNode{
				{Role: "button", Name: "Submit", Bounds: &b},
			},
		},
		shotB64:    base64.StdEncoding.EncodeToString([]byte("fake-webp-bytes")),
		snapshotID: "snap-202",
		nativeW:    1920,
		nativeH:    1080,
		displayW:   960,
		displayH:   540,
		scale:      2.0,
	}
	eng := NewSelectorEngine(disp, nil, "", "")

	// 1. 验证 Hybrid Semantic 完整执行与 max_steps 预算耗尽判定（修复 Finding #8）
	hybridOut, err := eng.RunAgentTask(context.Background(), AgentTaskInput{
		NodeID:   "node-1",
		Goal:     "wait_for Submit -> click Submit -> type hello@aura.dev -> key Return",
		MaxSteps: 4,
	})
	if err != nil || !hybridOut.Completed || hybridOut.Status != "completed" || len(hybridOut.Steps) != 4 {
		t.Fatalf("unexpected hybrid task output: %+v, err=%v", hybridOut, err)
	}

	partialOut, err := eng.RunAgentTask(context.Background(), AgentTaskInput{
		NodeID:   "node-1",
		Goal:     "click Submit -> type hello@aura.dev -> key Return",
		MaxSteps: 2,
	})
	if err != nil {
		t.Fatalf("RunAgentTask partial failed: %v", err)
	}
	if partialOut.Completed || partialOut.Status != "max_steps_reached" || len(partialOut.Steps) != 2 {
		t.Fatalf("expected Completed=false & Status=max_steps_reached when subGoals exceed max_steps, got %+v", partialOut)
	}

	// 2. 验证自定义 LLMEndpoint 绝不泄露服务端默认 AURA_AGENT_LLM_API_KEY（修复 Finding #1）
	t.Setenv("AURA_AGENT_LLM_ENDPOINT", "https://trusted-llm.internal/v1")
	t.Setenv("AURA_AGENT_LLM_API_KEY", "server-secret-master-key-DO-NOT-LEAK")

	var gotAuthHeader string
	callCount := 0
	customVLMSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuthHeader = r.Header.Get("Authorization")
		callCount++
		w.Header().Set("Content-Type", "application/json")
		var content string
		if callCount == 1 {
			content = `{"thought":"Click candidate [0] Submit button","action":"left_click","som_index":0}`
		} else {
			content = `{"thought":"Done","action":"finished","text":"done","finished":true}`
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{"message": map[string]any{"content": content}},
			},
		})
	}))
	defer customVLMSrv.Close()

	vlmOut, err := eng.RunAgentTask(context.Background(), AgentTaskInput{
		NodeID:      "node-1",
		Goal:        "Click the Submit button",
		MaxSteps:    5,
		LLMEndpoint: customVLMSrv.URL,
	})
	if err != nil || !vlmOut.Completed {
		t.Fatalf("RunAgentTask custom VLM failed: out=%+v, err=%v", vlmOut, err)
	}
	if gotAuthHeader != "" {
		t.Fatalf("SECURITY LEAK: custom LLMEndpoint received server Authorization header %q", gotAuthHeader)
	}

	// 3. 验证 WaitFor 在视觉禁用且 A11y 失败时不误判 absent，且在视觉禁用但 A11y 正常时持续轮询至元素出现（修复 Finding #9）
	_, _ = eng.UpdateSettings(context.Background(), store.VisionSettings{Mode: "disabled"}, "clear", "")
	disp.a11yErr = errors.New("a11y service down")
	absentOut, err := eng.WaitFor(context.Background(), WaitForInput{
		NodeID:    "node-1",
		Target:    "Submit",
		State:     "absent",
		TimeoutMs: 250,
	})
	if err != nil {
		t.Fatalf("WaitFor absent error: %v", err)
	}
	if absentOut.Satisfied || absentOut.Last.Status != "observation_unavailable" {
		t.Fatalf("expected WaitFor(absent) to fail when observation is unavailable, got %+v", absentOut)
	}

	// 恢复 A11y，设置第 2 次轮询才出现 Submit，验证 vision_disabled 下 WaitFor(present) 不会在第 1 次轮询后提前退出
	disp.a11yErr = nil
	disp.a11yPolls = 0
	disp.appearAfter = 2
	presentOut, err := eng.WaitFor(context.Background(), WaitForInput{
		NodeID:    "node-1",
		Target:    "Submit",
		State:     "present",
		TimeoutMs: 1500,
	})
	if err != nil || !presentOut.Satisfied || presentOut.Polls < 2 {
		t.Fatalf("expected WaitFor(present) with vision_disabled to poll until element appears on poll #2, got %+v, err=%v", presentOut, err)
	}
}
