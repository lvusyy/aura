package selection

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	aurav1 "github.com/aura/controller/gen/aura/v1"
	"github.com/aura/controller/internal/fusion"
)

type goldenToolSpec struct {
	Required   []string          `json:"required"`
	Properties map[string]string `json:"properties"`
	Aliases    map[string]string `json:"aliases"`
}

type goldenNodeContract struct {
	ContractVersion string                    `json:"contract_version"`
	ToolNames       []string                  `json:"tool_names"`
	Tools           map[string]goldenToolSpec `json:"tools"`
}

type goldenContractDisp struct {
	t        *testing.T
	contract goldenNodeContract
	shotB64  string

	mu        sync.Mutex
	callCount map[string]int
}

func (g *goldenContractDisp) Dispatch(_ context.Context, _, tool string, args []byte, _ int64, _, _ string) (*aurav1.ToolResponse, string, error) {
	g.mu.Lock()
	if g.callCount == nil {
		g.callCount = make(map[string]int)
	}
	g.callCount[tool]++
	g.mu.Unlock()

	spec, ok := g.contract.Tools[tool]
	if !ok {
		// 模拟真实 Rust aura-node tool_dispatch.rs：未知工具返回 E_UNSUPPORTED
		if tool != "double_click" {
			g.t.Errorf("controller dispatched unknown tool %q not present in proto/node_tool_contract.json", tool)
		}
		raw, _ := json.Marshal(map[string]any{
			"ok":    false,
			"error": map[string]any{"code": "E_UNSUPPORTED", "message": "unknown tool: " + tool},
		})
		return &aurav1.ToolResponse{JsonEnvelope: raw}, "", nil
	}

	var payload map[string]any
	if len(args) > 0 {
		if err := json.Unmarshal(args, &payload); err != nil {
			g.t.Errorf("tool %q dispatched invalid JSON %s: %v", tool, string(args), err)
			raw, _ := json.Marshal(map[string]any{
				"ok":    false,
				"error": map[string]any{"code": "E_INVALID_ARG", "message": err.Error()},
			})
			return &aurav1.ToolResponse{JsonEnvelope: raw}, "", nil
		}
	}
	if payload == nil {
		payload = map[string]any{}
	}

	// 将 serde alias 归一化到规范属性名后校验必填项与类型
	normalized := make(map[string]any, len(payload))
	for k, v := range payload {
		canon := k
		if target, isAlias := spec.Aliases[k]; isAlias {
			canon = target
		}
		expType, allowed := spec.Properties[canon]
		if !allowed {
			g.t.Errorf("tool %q dispatched undeclared property %q (payload=%s)", tool, k, string(args))
			continue
		}
		if err := validateGoldenFieldType(expType, v); err != nil {
			g.t.Errorf("tool %q property %q violated schema type %q: %v (payload=%s)", tool, k, expType, err, string(args))
		}
		normalized[canon] = v
	}

	for _, reqKey := range spec.Required {
		if _, present := normalized[reqKey]; !present {
			g.t.Errorf("tool %q missing required field %q (payload=%s)", tool, reqKey, string(args))
		}
	}

	var data any
	switch tool {
	case "get_a11y_tree":
		b := [4]int32{120, 90, 160, 44}
		data = fusion.A11yTree{
			Nodes: []fusion.A11yNode{
				{Role: "button", Name: "SubmitOrder", Bounds: &b},
			},
		}
	case "screenshot":
		data = map[string]any{
			"image_base64": g.shotB64,
			"mime":         "image/webp",
			"meta": map[string]any{
				"native_w":    1920,
				"native_h":    1080,
				"display_w":   1280,
				"display_h":   720,
				"scale":       1.5,
				"snapshot_id": "snap-contract-1",
			},
		}
	default:
		data = map[string]any{"done": true}
	}
	raw, _ := json.Marshal(map[string]any{"ok": true, "data": data})
	return &aurav1.ToolResponse{JsonEnvelope: raw}, "", nil
}

func validateGoldenFieldType(expType string, val any) error {
	switch expType {
	case "string":
		if _, ok := val.(string); !ok {
			return fmt.Errorf("expected string, got %T", val)
		}
	case "integer":
		num, ok := val.(float64)
		if !ok || num != float64(int64(num)) {
			return fmt.Errorf("expected integer, got %v (%T)", val, val)
		}
	case "number":
		if _, ok := val.(float64); !ok {
			return fmt.Errorf("expected number, got %T", val)
		}
	case "boolean":
		if _, ok := val.(bool); !ok {
			return fmt.Errorf("expected boolean, got %T", val)
		}
	case "array_int2":
		arr, ok := val.([]any)
		if !ok || len(arr) != 2 {
			return fmt.Errorf("expected [i32; 2] array of len 2, got %v", val)
		}
		for i, elem := range arr {
			num, ok := elem.(float64)
			if !ok || num != float64(int32(num)) {
				return fmt.Errorf("element [%d] not int32: %v", i, elem)
			}
		}
	case "array_int4":
		arr, ok := val.([]any)
		if !ok || len(arr) != 4 {
			return fmt.Errorf("expected [i32; 4] array of len 4, got %v", val)
		}
		for i, elem := range arr {
			num, ok := elem.(float64)
			if !ok || num != float64(int32(num)) {
				return fmt.Errorf("element [%d] not int32: %v", i, elem)
			}
		}
	case "array_string":
		arr, ok := val.([]any)
		if !ok {
			return fmt.Errorf("expected []string, got %T", val)
		}
		for i, elem := range arr {
			if _, ok := elem.(string); !ok {
				return fmt.Errorf("element [%d] not string: %T", i, elem)
			}
		}
	case "object":
		if _, ok := val.(map[string]any); !ok {
			return fmt.Errorf("expected JSON object, got %T", val)
		}
	default:
		return fmt.Errorf("unsupported schema type %q in golden contract", expType)
	}
	return nil
}

// TestControllerNodeDispatchMatchesGoldenContract 跨语言契约回归守卫（P0）：
// 加载与 Rust `aura-node` 共用的 `proto/node_tool_contract.json` 真源，触发控制面 6 项高阶工具与
// Computer-Use 动作分支（含 zoom），验证 Go 控制器下发的底层节点工具名、必填字段与数据类型符合契约定义。
func TestControllerNodeDispatchMatchesGoldenContract(t *testing.T) {
	contractPath := filepath.Join("..", "..", "..", "proto", "node_tool_contract.json")
	raw, err := os.ReadFile(contractPath)
	if err != nil {
		t.Fatalf("read %s: %v", contractPath, err)
	}
	var contract goldenNodeContract
	if err := json.Unmarshal(raw, &contract); err != nil {
		t.Fatalf("unmarshal %s: %v", contractPath, err)
	}
	if contract.ContractVersion != "aura.v1/2026-07" || len(contract.ToolNames) != 21 {
		t.Fatalf("unexpected contract metadata: version=%q tools=%d", contract.ContractVersion, len(contract.ToolNames))
	}

	disp := &goldenContractDisp{
		t:        t,
		contract: contract,
		shotB64:  base64.StdEncoding.EncodeToString([]byte("fake-webp-bytes")),
	}
	eng := NewSelectorEngine(disp, nil, "", "")
	ctx := context.Background()

	// 1. Locate / Annotate / WaitFor / Click (含 DoubleClick 回退 + TypeText + PressKey)
	if _, err := eng.Locate(ctx, LocateInput{NodeID: "n-1", Target: "SubmitOrder"}); err != nil {
		t.Fatalf("Locate: %v", err)
	}
	if _, err := eng.Annotate(ctx, AnnotateInput{NodeID: "n-1"}); err != nil {
		t.Fatalf("Annotate: %v", err)
	}
	if _, err := eng.WaitFor(ctx, WaitForInput{NodeID: "n-1", Target: "SubmitOrder", State: "present", TimeoutMs: 500}); err != nil {
		t.Fatalf("WaitFor: %v", err)
	}
	if out, err := eng.Click(ctx, ClickInput{
		NodeID:      "n-1",
		Target:      "SubmitOrder",
		DoubleClick: true,
		TypeText:    "hello",
		PressKey:    "Return",
	}); err != nil || !out.Clicked {
		t.Fatalf("Click: out=%+v err=%v", out, err)
	}

	// 2. 遍历 ExecuteComputerAction 的各动作形态（含 zoom display->native 转换与契约校验）
	actions := []ComputerActionInput{
		{NodeID: "n-1", Action: "left_click", Coordinate: []int32{200, 150}},
		{NodeID: "n-1", Action: "right_click", Coordinate: []int32{200, 150}},
		{NodeID: "n-1", Action: "middle_click", Coordinate: []int32{200, 150}},
		{NodeID: "n-1", Action: "double_click", Coordinate: []int32{200, 150}},
		{NodeID: "n-1", Action: "triple_click", Target: "SubmitOrder"},
		{NodeID: "n-1", Action: "mouse_move", Target: "SubmitOrder"},
		{NodeID: "n-1", Action: "drag", StartCoordinate: []int32{100, 100}, Coordinate: []int32{300, 300}},
		{NodeID: "n-1", Action: "scroll", ScrollDirection: "down", ScrollAmount: 4},
		{NodeID: "n-1", Action: "type", Text: "line with newline\n"},
		{NodeID: "n-1", Action: "key", Keys: []string{"ctrl", "s"}},
		{NodeID: "n-1", Action: "wait", Duration: 0.05},
		{NodeID: "n-1", Action: "screenshot"},
		{NodeID: "n-1", Action: "zoom", Region: []int32{10, 20, 300, 200}, CoordSpace: "display"},
	}
	for _, act := range actions {
		out, err := eng.ExecuteComputerAction(ctx, act)
		if err != nil || !out.Executed {
			t.Fatalf("ExecuteComputerAction(%s): out=%+v err=%v", act.Action, out, err)
		}
	}

	// 3. RunAgentTask 复合流水线
	taskOut, err := eng.RunAgentTask(ctx, AgentTaskInput{
		NodeID: "n-1",
		Goal:   "wait_for SubmitOrder -> click SubmitOrder -> type confirmed -> key Return -> scroll down",
	})
	if err != nil || !taskOut.Completed {
		t.Fatalf("RunAgentTask: out=%+v err=%v", taskOut, err)
	}

	// 确认控制面调用的底层节点工具均已被实际触发并通过黄金契约校验
	for _, mustCall := range []string{"get_a11y_tree", "screenshot", "zoom", "click", "move_mouse", "drag", "scroll", "type", "key", "wait"} {
		if disp.callCount[mustCall] == 0 {
			t.Fatalf("expected golden contract test to exercise node tool %q, count=0", mustCall)
		}
	}
}
