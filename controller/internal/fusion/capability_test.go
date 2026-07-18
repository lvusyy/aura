package fusion

import "testing"

// TestFusionCapable 覆盖 fusion_capable 推导三态：全含 true / 缺一 false / 空与 nil false
// （SC-4②：从既有 tools 纯推导，判定即 tools ⊇ {screenshot, get_a11y_tree}）。
func TestFusionCapable(t *testing.T) {
	cases := []struct {
		name  string
		tools []string
		want  bool
	}{
		{"both plus extras", []string{"screenshot", "get_a11y_tree", "click", "type_text"}, true},
		{"exactly required", []string{"get_a11y_tree", "screenshot"}, true},
		{"missing a11y", []string{"screenshot", "click"}, false},
		{"missing screenshot", []string{"get_a11y_tree", "click"}, false},
		{"empty", []string{}, false},
		{"nil", nil, false},
	}
	for _, c := range cases {
		if got := FusionCapable(c.tools); got != c.want {
			t.Errorf("%s: FusionCapable(%v) = %v, want %v", c.name, c.tools, got, c.want)
		}
	}
}
