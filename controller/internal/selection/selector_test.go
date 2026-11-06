package selection

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	aurav1 "github.com/aura/controller/gen/aura/v1"
	"github.com/aura/controller/internal/fusion"
	"github.com/aura/controller/internal/store"
)

type mockNodeDisp struct {
	mu       sync.Mutex
	calls    []string
	lastArgs map[string][]byte
	tree     fusion.A11yTree
	shotB64  string
	scale    float64
	nativeW  int32
	nativeH  int32
	displayW int32
	displayH int32
}

func (m *mockNodeDisp) Dispatch(_ context.Context, _, tool string, jsonArgs []byte, _ int64, _, _ string) (*aurav1.ToolResponse, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, tool)
	if m.lastArgs == nil {
		m.lastArgs = make(map[string][]byte)
	}
	m.lastArgs[tool] = append([]byte(nil), jsonArgs...)

	var data any
	switch tool {
	case "get_a11y_tree":
		data = m.tree
	case "screenshot":
		data = map[string]any{
			"image_base64": m.shotB64,
			"mime":         "image/webp",
			"meta": map[string]any{
				"native_w":  m.nativeW,
				"native_h":  m.nativeH,
				"display_w": m.displayW,
				"display_h": m.displayH,
				"scale":     m.scale,
			},
		}
	case "click":
		data = map[string]any{"ok": true}
	}
	raw, _ := json.Marshal(map[string]any{
		"ok":   true,
		"data": data,
	})
	return &aurav1.ToolResponse{JsonEnvelope: raw}, "", nil
}

type mockJev struct {
	resp *SystemOneResponse
	err  error
	reqs []SystemOneRequest
}

func (m *mockJev) Evaluate(_ context.Context, req SystemOneRequest) (*SystemOneResponse, error) {
	m.reqs = append(m.reqs, req)
	return m.resp, m.err
}

func TestTier0A11yShortCircuitsWithoutScreenshot(t *testing.T) {
	b := [4]int32{200, 100, 120, 40}
	disp := &mockNodeDisp{
		tree: fusion.A11yTree{
			Nodes: []fusion.A11yNode{
				{Role: "button", Name: "立即登录", Bounds: &b},
			},
		},
	}
	eng := NewSelectorEngine(disp, nil, "http://127.0.0.1:30808", "tok")
	out, err := eng.Locate(context.Background(), LocateInput{
		NodeID: "node-1",
		Target: "立即登录",
	})
	if err != nil {
		t.Fatalf("Locate error: %v", err)
	}
	if !out.Found || out.Source != "a11y" {
		t.Fatalf("expected Tier 0 a11y hit, got %+v", out)
	}
	if out.ClickPoint != [2]int32{260, 120} {
		t.Fatalf("unexpected click_point: %v", out.ClickPoint)
	}
	for _, c := range disp.calls {
		if c == "screenshot" {
			t.Fatalf("Tier 0 A11y hit must not invoke screenshot, calls=%v", disp.calls)
		}
	}
}

func TestTier1JevChoiceAndDisplayToNativeAlignment(t *testing.T) {
	disp := &mockNodeDisp{
		shotB64:  base64.StdEncoding.EncodeToString([]byte("fake-webp")),
		nativeW:  1920,
		nativeH:  1080,
		displayW: 960,
		displayH: 540,
		scale:    2.0,
	}
	det := &fusion.MockDetector{
		Boxes: []fusion.VisualBox{
			// display 空间 [100, 50, 40, 20] -> scale=2.0 -> native [200, 100, 80, 40]
			{Bbox: [4]int32{100, 50, 40, 20}, Kind: "icon", Confidence: 0.92},
		},
	}
	jev := &mockJev{
		resp: &SystemOneResponse{
			Answers: map[string]SystemOneAnswer{
				"exists": {Type: QuestionNoul, Noul: 0.94},
				"pick":   {Type: QuestionChoice, ChoiceIndex: 0, Confidence: 0.91},
			},
		},
	}
	eng := NewSelectorEngine(disp, nil, "http://127.0.0.1:30808", "tok").WithOverrides(det, jev)
	clickOut, err := eng.Click(context.Background(), ClickInput{
		NodeID: "node-1",
		Target: "设置齿轮图标",
	})
	if err != nil {
		t.Fatalf("Click error: %v", err)
	}
	if !clickOut.Clicked || !clickOut.Located.Found {
		t.Fatalf("expected clicked=true, got %+v", clickOut)
	}
	// native bounds = [200, 100, 80, 40], center = [240, 120]
	if clickOut.Located.Bounds != [4]int32{200, 100, 80, 40} {
		t.Fatalf("native bounds mismatch: got %v, want [200 100 80 40]", clickOut.Located.Bounds)
	}
	// display bounds = [100, 50, 40, 20], center = [120, 60]
	if clickOut.Located.DisplayBounds != [4]int32{100, 50, 40, 20} {
		t.Fatalf("display bounds mismatch: got %v, want [100 50 40 20]", clickOut.Located.DisplayBounds)
	}
	if clickOut.Located.ClickPoint != [2]int32{240, 120} || clickOut.Located.DisplayClickPoint != [2]int32{120, 60} {
		t.Fatalf("click points mismatch: native=%v display=%v", clickOut.Located.ClickPoint, clickOut.Located.DisplayClickPoint)
	}
}

func TestNoulThresholdGatesFalsePositiveClick(t *testing.T) {
	disp := &mockNodeDisp{
		shotB64:  base64.StdEncoding.EncodeToString([]byte("fake-webp")),
		nativeW:  1280,
		nativeH:  720,
		displayW: 1280,
		displayH: 720,
		scale:    1.0,
	}
	jev := &mockJev{
		resp: &SystemOneResponse{
			Answers: map[string]SystemOneAnswer{
				"exists": {Type: QuestionNoul, Noul: 0.12},
			},
		},
	}
	eng := NewSelectorEngine(disp, nil, "http://127.0.0.1:30808", "tok").WithOverrides(&fusion.MockDetector{}, jev)
	clickOut, err := eng.Click(context.Background(), ClickInput{
		NodeID: "node-1",
		Target: "不存在的按钮",
	})
	if err != nil {
		t.Fatalf("Click error: %v", err)
	}
	if clickOut.Clicked || clickOut.Located.Found || clickOut.Status != "not_found" {
		t.Fatalf("expected Noul gate to block click, got %+v", clickOut)
	}
}

func TestSettingsOptimisticLockAndAPIKeyMasking(t *testing.T) {
	eng := NewSelectorEngine(nil, nil, "http://192.168.22.225:30808", "tok")
	cur, err := eng.GetSettings(context.Background())
	if err != nil {
		t.Fatalf("GetSettings: %v", err)
	}
	cur.Mode = "cloud"
	updated, err := eng.UpdateSettings(context.Background(), cur, "set", "ts_secret_1234567890")
	if err != nil {
		t.Fatalf("UpdateSettings set: %v", err)
	}
	if updated.APIKey != "ts_secret_1234567890" || updated.APIKeyHint != "ts_secre…" {
		t.Fatalf("unexpected api key / hint: %+v", updated)
	}

	// Keep preserves APIKey while incrementing revision
	updated2, err := eng.UpdateSettings(context.Background(), updated, "keep", "")
	if err != nil {
		t.Fatalf("UpdateSettings keep: %v", err)
	}
	if updated2.APIKey != "ts_secret_1234567890" || updated2.Revision != updated.Revision+1 {
		t.Fatalf("keep failed: %+v", updated2)
	}

	// Stale revision fails with ErrVisionRevisionConflict
	stale := updated
	if _, err := eng.UpdateSettings(context.Background(), stale, "keep", ""); !errors.Is(err, store.ErrVisionRevisionConflict) {
		t.Fatalf("expected ErrVisionRevisionConflict, got %v", err)
	}
}
