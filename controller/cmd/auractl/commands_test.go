package main

import (
	"strings"
	"testing"
)

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
