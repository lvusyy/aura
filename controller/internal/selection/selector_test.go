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
	mu          sync.Mutex
	calls       []string
	lastArgs    map[string][]byte
	tree        fusion.A11yTree
	postTree    *fusion.A11yTree
	shotB64     string
	postShotB64 string
	snapshotID  string
	clicked     bool
	scale       float64
	nativeW     int32
	nativeH     int32
	displayW    int32
	displayH    int32
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
		if m.clicked && m.postTree != nil {
			data = *m.postTree
		} else {
			data = m.tree
		}
	case "screenshot":
		b64 := m.shotB64
		if m.clicked && m.postShotB64 != "" {
			b64 = m.postShotB64
		}
		meta := map[string]any{
			"native_w":  m.nativeW,
			"native_h":  m.nativeH,
			"display_w": m.displayW,
			"display_h": m.displayH,
			"scale":     m.scale,
		}
		if m.snapshotID != "" {
			meta["snapshot_id"] = m.snapshotID
		}
		data = map[string]any{
			"image_base64": b64,
			"mime":         "image/webp",
			"meta":         meta,
		}
	case "click":
		m.clicked = true
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

func TestNoulZeroAndOmittedChoiceIndexJsonUnmarshal(t *testing.T) {
	rawJSON := `{
		"answers": {
			"exists": {"type": "noul", "noul": 0.0},
			"pick": {"type": "choice", "choice": "1: [button] Cancel @(200,200,80,32)", "confidence": 0.92}
		}
	}`
	var parsed SystemOneResponse
	if err := json.Unmarshal([]byte(rawJSON), &parsed); err != nil {
		t.Fatalf("Unmarshal SystemOneResponse: %v", err)
	}
	ex := parsed.Answers["exists"]
	if !ex.HasNoul || ex.Noul != 0.0 {
		t.Fatalf("expected HasNoul=true and Noul=0.0, got %+v", ex)
	}
	pick := parsed.Answers["pick"]
	if pick.HasChoiceIndex || pick.ChoiceIndex != -1 {
		t.Fatalf("expected omitted choice_index to set HasChoiceIndex=false and ChoiceIndex=-1, got %+v", pick)
	}

	// Verify selector blocks click when JSON has {"noul": 0.0} (even without "type" field)
	var noTypeResp SystemOneResponse
	_ = json.Unmarshal([]byte(`{"answers":{"exists":{"noul":0.0}}}`), &noTypeResp)
	disp := &mockNodeDisp{
		shotB64:  base64.StdEncoding.EncodeToString([]byte("fake-webp-noul0")),
		nativeW:  1280,
		nativeH:  720,
		displayW: 1280,
		displayH: 720,
		scale:    1.0,
	}
	eng := NewSelectorEngine(disp, nil, "http://127.0.0.1:30808", "tok").
		WithOverrides(&fusion.MockDetector{}, &mockJev{resp: &noTypeResp})
	loc, err := eng.Locate(context.Background(), LocateInput{NodeID: "n1", Target: "Missing"})
	if err != nil {
		t.Fatalf("Locate: %v", err)
	}
	if loc.Found || loc.Status != "not_found" || loc.ExistenceProb != 0.0 {
		t.Fatalf("expected noul=0.0 to gate not_found, got %+v", loc)
	}

	// Verify omitted choice_index resolves candidate #1 via Choice string
	var pick1Resp SystemOneResponse
	_ = json.Unmarshal([]byte(`{
		"answers": {
			"exists": {"noul": 0.95},
			"pick": {"choice": "1: [icon] Cancel @(200,100,80,32)", "confidence": 0.89}
		}
	}`), &pick1Resp)
	det := &fusion.MockDetector{
		Boxes: []fusion.VisualBox{
			{Bbox: [4]int32{50, 100, 80, 32}, Kind: "icon", Caption: "Submit", Confidence: 0.9},
			{Bbox: [4]int32{200, 100, 80, 32}, Kind: "icon", Caption: "Cancel", Confidence: 0.9},
		},
	}
	eng2 := NewSelectorEngine(disp, nil, "http://127.0.0.1:30808", "tok").
		WithOverrides(det, &mockJev{resp: &pick1Resp})
	loc2, err := eng2.Locate(context.Background(), LocateInput{NodeID: "n1", Target: "Cancel"})
	if err != nil {
		t.Fatalf("Locate 2: %v", err)
	}
	if !loc2.Found || loc2.Bounds != [4]int32{200, 100, 80, 32} {
		t.Fatalf("expected candidate #1 bounds [200 100 80 32], got %+v", loc2)
	}
}

func TestSnapshotIDPropagatesAndSameScreenCacheHits(t *testing.T) {
	disp := &mockNodeDisp{
		shotB64:    base64.StdEncoding.EncodeToString([]byte("static-screen-bytes-12345")),
		snapshotID: "snap-42",
		nativeW:    1920,
		nativeH:    1080,
		displayW:   1280,
		displayH:   720,
		scale:      1.5,
	}
	det := &fusion.MockDetector{
		Boxes: []fusion.VisualBox{
			{Bbox: [4]int32{100, 200, 80, 40}, Kind: "icon", Caption: "Confirm", Confidence: 0.95},
		},
	}
	jev := &mockJev{
		resp: &SystemOneResponse{
			Answers: map[string]SystemOneAnswer{
				"exists": {Type: QuestionNoul, Noul: 0.96, HasNoul: true},
				"pick":   {Type: QuestionChoice, ChoiceIndex: 0, HasChoiceIndex: true, Confidence: 0.92},
			},
		},
	}
	eng := NewSelectorEngine(disp, nil, "http://127.0.0.1:30808", "tok").WithOverrides(det, jev)

	// First Locate populates candidate cache and returns snapshot_id
	loc1, err := eng.Locate(context.Background(), LocateInput{NodeID: "node-cache", Target: "Confirm"})
	if err != nil {
		t.Fatalf("Locate 1: %v", err)
	}
	if loc1.SnapshotID != "snap-42" {
		t.Fatalf("expected SnapshotID=snap-42, got %q", loc1.SnapshotID)
	}
	if len(det.DetectOpts) != 1 {
		t.Fatalf("expected 1 detector call after first Locate, got %d", len(det.DetectOpts))
	}

	// Second Locate on identical screenshot hits same-screen candidate cache (0 new detector calls)
	loc2, err := eng.Locate(context.Background(), LocateInput{NodeID: "node-cache", Target: "Confirm"})
	if err != nil {
		t.Fatalf("Locate 2: %v", err)
	}
	if !loc2.Found || len(det.DetectOpts) != 1 {
		t.Fatalf("expected cache hit with 1 total detector call, got %d calls", len(det.DetectOpts))
	}

	// Click passes snapshot_id="snap-42" to node click tool and invalidates cache
	clickOut, err := eng.Click(context.Background(), ClickInput{NodeID: "node-cache", Target: "Confirm"})
	if err != nil {
		t.Fatalf("Click: %v", err)
	}
	if !clickOut.Clicked {
		t.Fatalf("expected clicked=true, got %+v", clickOut)
	}
	var sentClick map[string]any
	if err := json.Unmarshal(disp.lastArgs["click"], &sentClick); err != nil {
		t.Fatalf("unmarshal click args: %v", err)
	}
	if sentClick["snapshot_id"] != "snap-42" || sentClick["coord_space"] != "display" {
		t.Fatalf("expected snapshot_id=snap-42 and coord_space=display in click args, got %+v", sentClick)
	}
}

func TestPostClickVerifyRealStateDiff(t *testing.T) {
	b1 := [4]int32{100, 100, 80, 30}
	b2 := [4]int32{100, 100, 80, 30}
	// Case 1: Frozen screen (neither A11y nor screenshot changes) -> Verified = false
	frozenDisp := &mockNodeDisp{
		tree: fusion.A11yTree{
			Nodes: []fusion.A11yNode{{Role: "button", Name: "Save", Bounds: &b1}},
		},
	}
	engFrozen := NewSelectorEngine(frozenDisp, nil, "http://127.0.0.1:30808", "tok")
	outFrozen, err := engFrozen.Click(context.Background(), ClickInput{
		NodeID: "node-frozen",
		Target: "Save",
		Verify: true,
	})
	if err != nil {
		t.Fatalf("Click frozen: %v", err)
	}
	if !outFrozen.Clicked || outFrozen.Verified {
		t.Fatalf("expected Clicked=true and Verified=false on unchanged state, got %+v", outFrozen)
	}

	// Case 2: Post-click A11y state changes ("Save" -> "Saved!") -> Verified = true
	changedDisp := &mockNodeDisp{
		tree: fusion.A11yTree{
			Nodes: []fusion.A11yNode{{Role: "button", Name: "Save", Bounds: &b1}},
		},
		postTree: &fusion.A11yTree{
			Nodes: []fusion.A11yNode{{Role: "button", Name: "Saved!", Bounds: &b2}},
		},
	}
	engChanged := NewSelectorEngine(changedDisp, nil, "http://127.0.0.1:30808", "tok")
	outChanged, err := engChanged.Click(context.Background(), ClickInput{
		NodeID: "node-changed",
		Target: "Save",
		Verify: true,
	})
	if err != nil {
		t.Fatalf("Click changed: %v", err)
	}
	if !outChanged.Clicked || !outChanged.Verified {
		t.Fatalf("expected Clicked=true and Verified=true when A11y state changes, got %+v", outChanged)
	}
}

func TestMaskAPIKeyHintSingleCharAndMatchA11yGuards(t *testing.T) {
	if got := store.MaskAPIKeyHint("x"); got != "x…" {
		t.Fatalf("MaskAPIKeyHint(\"x\") = %q, want \"x…\"", got)
	}
	if got := store.MaskAPIKeyHint(""); got != "" {
		t.Fatalf("MaskAPIKeyHint(\"\") = %q, want \"\"", got)
	}

	// Interactive role beats container role with same name
	cands := []fusion.FusedElement{
		{Role: "panel", Name: "Submit", Bounds: [4]int32{0, 0, 800, 600}, Source: "a11y"},
		{Role: "button", Name: "Submit", Bounds: [4]int32{100, 100, 80, 32}, Source: "a11y"},
	}
	matched, score, ok := matchA11yCandidate(cands, "Submit")
	if !ok || matched.Role != "button" || score < 0.99 {
		t.Fatalf("expected interactive button to win over panel, got role=%q score=%v", matched.Role, score)
	}

	// Negation query bypasses deterministic substring Tier 0
	if _, _, ok := matchA11yCandidate(cands, "not Submit"); ok {
		t.Fatal("expected negation query 'not Submit' to bypass Tier 0 substring match")
	}
}
