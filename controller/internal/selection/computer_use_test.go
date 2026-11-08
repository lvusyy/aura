package selection

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aura/controller/internal/fusion"
)

func TestParseUITarsActionAndComputerUseAdapter(t *testing.T) {
	// 1. 验证 UI-TARS 语法解析（含 Thought、点框中心换算、快捷键规整）
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

	// 2. 验证 ExecuteComputerAction 跨方言执行（UI-TARS normalized_1000 自动换算物理像素 + SoMIndex 点击）
	b := [4]int32{200, 100, 120, 40}
	disp := &mockNodeDisp{
		tree: fusion.A11yTree{
			Nodes: []fusion.A11yNode{
				{Role: "button", Name: "Submit", Bounds: &b},
			},
		},
		shotB64:  base64.StdEncoding.EncodeToString([]byte("fake-webp-bytes")),
		nativeW:  1920,
		nativeH:  1080,
		displayW: 960,
		displayH: 540,
		scale:    2.0,
	}
	eng := NewSelectorEngine(disp, nil, "", "")

	// 节点屏幕为 1920x1080，UI-TARS (500,500) 应换算为 native (960,540)
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

	// 验证 SoMIndex 直接点击 0 号候选元素 (Submit: [200,100,120,40] -> center (260,120))
	idx0 := 0
	somOut, err := eng.ExecuteComputerAction(context.Background(), ComputerActionInput{
		NodeID:   "node-1",
		Action:   "left_click",
		SoMIndex: &idx0,
	})
	if err != nil || !somOut.Executed {
		t.Fatalf("ExecuteComputerAction SoMIndex failed: out=%+v, err=%v", somOut, err)
	}
	if somOut.Coordinate != [2]int32{260, 120} {
		t.Fatalf("expected SoM [0] click coordinate (260,120), got %v", somOut.Coordinate)
	}
}

func TestRunAgentTaskHybridAndVLMSoMLoop(t *testing.T) {
	b := [4]int32{200, 100, 120, 40}
	disp := &mockNodeDisp{
		tree: fusion.A11yTree{
			Nodes: []fusion.A11yNode{
				{Role: "button", Name: "Submit", Bounds: &b},
			},
		},
		shotB64:  base64.StdEncoding.EncodeToString([]byte("fake-webp-bytes")),
		nativeW:  1920,
		nativeH:  1080,
		displayW: 960,
		displayH: 540,
		scale:    2.0,
	}
	eng := NewSelectorEngine(disp, nil, "", "")

	// 1. 验证无需外部 LLM 的内置 Hybrid Semantic 多步任务执行器
	hybridOut, err := eng.RunAgentTask(context.Background(), AgentTaskInput{
		NodeID: "node-1",
		Goal:   "wait_for Submit -> click Submit -> type hello@aura.dev -> key Return",
	})
	if err != nil {
		t.Fatalf("RunAgentTask hybrid failed: %v", err)
	}
	if !hybridOut.Completed || hybridOut.Status != "completed" || hybridOut.Planner != "hybrid_semantic" || len(hybridOut.Steps) != 4 {
		t.Fatalf("unexpected hybrid task output: %+v", hybridOut)
	}

	// 2. 验证外部 OpenAI 兼容 VLM + Set-of-Mark 多步闭环规划器
	callCount := 0
	vlmSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.Header().Set("Content-Type", "application/json")
		var content string
		if callCount == 1 {
			content = `{"thought":"Click candidate [0] Submit button","action":"left_click","som_index":0}`
		} else {
			content = `{"thought":"Submit clicked and form submitted","action":"finished","text":"form submitted","finished":true}`
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{"message": map[string]any{"content": content}},
			},
		})
	}))
	defer vlmSrv.Close()

	vlmOut, err := eng.RunAgentTask(context.Background(), AgentTaskInput{
		NodeID:      "node-1",
		Goal:        "Click the Submit button",
		MaxSteps:    5,
		LLMEndpoint: vlmSrv.URL,
	})
	if err != nil {
		t.Fatalf("RunAgentTask VLM SoM failed: %v", err)
	}
	if !vlmOut.Completed || vlmOut.Planner != "vlm_som" || len(vlmOut.Steps) != 2 {
		t.Fatalf("unexpected VLM task output: %+v", vlmOut)
	}
}
