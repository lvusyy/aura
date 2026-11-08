package main

import (
	"strings"
	"testing"

	aurav1 "github.com/aura/controller/gen/aura/v1"
)

func TestFormatTraceExportShAndJSON(t *testing.T) {
	steps := []*aurav1.TraceStep{
		{
			Seq:      1,
			Tool:     "click",
			JsonArgs: []byte(`{"x":100,"y":200,"button":"left"}`),
		},
		{
			Seq:      2,
			Tool:     "type",
			JsonArgs: []byte(`{"text":"it's ready"}`),
		},
	}

	shOut, err := formatTraceExport("trace-123", "node-src", "linux", "", "sh", steps)
	if err != nil {
		t.Fatalf("formatTraceExport sh failed: %v", err)
	}
	if !strings.Contains(shOut, `NODE_ID="${1:-node-src}"`) {
		t.Fatalf("missing default NODE_ID in sh output:\n%s", shOut)
	}
	if !strings.Contains(shOut, `auractl tool call "$NODE_ID" click --args '{"x":100,"y":200,"button":"left"}'`) {
		t.Fatalf("missing step 1 command in sh output:\n%s", shOut)
	}
	if !strings.Contains(shOut, `'{"text":"it'\''s ready"}'`) {
		t.Fatalf("single quote not escaped in sh output:\n%s", shOut)
	}

	jsonOut, err := formatTraceExport("trace-123", "node-src", "linux", "node-target", "json", steps)
	if err != nil {
		t.Fatalf("formatTraceExport json failed: %v", err)
	}
	if !strings.Contains(jsonOut, `"target_node_id": "node-target"`) || !strings.Contains(jsonOut, `"tool": "click"`) {
		t.Fatalf("unexpected json output:\n%s", jsonOut)
	}
}

// TestContractSkewWarning 覆盖 `node list` 契约版本偏斜告警判定：
// 空版本（节点未上报）/ 与期望一致 → 无告警；不一致 → WARN 文本含 node_id / got / want / skew。
func TestContractSkewWarning(t *testing.T) {
	cases := []struct {
		name       string
		nodeID     string
		version    string
		wantWarn   bool
		wantSubstr []string
	}{
		{"empty version no warn", "n1", "", false, nil},
		{"matching version no warn", "n1", expectedContractVersion, false, nil},
		{
			"skewed version warns", "node-42", "aura.v1/2025-01", true,
			[]string{"node-42", "aura.v1/2025-01", expectedContractVersion, "skew"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := contractSkewWarning(c.nodeID, c.version)
			if c.wantWarn && got == "" {
				t.Fatalf("contractSkewWarning(%q,%q): want warning, got empty", c.nodeID, c.version)
			}
			if !c.wantWarn && got != "" {
				t.Fatalf("contractSkewWarning(%q,%q): want no warning, got %q", c.nodeID, c.version, got)
			}
			for _, sub := range c.wantSubstr {
				if !strings.Contains(got, sub) {
					t.Fatalf("contractSkewWarning(%q,%q)=%q: missing substring %q", c.nodeID, c.version, got, sub)
				}
			}
		})
	}
}
