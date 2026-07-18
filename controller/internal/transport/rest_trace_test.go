package transport

import (
	"testing"

	"github.com/aura/controller/internal/store"
)

// TestNormalizeTracePageSize：<=0 取默认 200，超上限截为 500，区间内原样（MAJOR-3 防单页超 max-recv-bytes）。
func TestNormalizeTracePageSize(t *testing.T) {
	cases := []struct{ in, want int64 }{
		{0, defaultTracePageSize},
		{-5, defaultTracePageSize},
		{10, 10},
		{maxTracePageSize, maxTracePageSize},
		{maxTracePageSize + 1, maxTracePageSize},
		{99999, maxTracePageSize},
	}
	for _, c := range cases {
		if got := normalizeTracePageSize(c.in); got != c.want {
			t.Errorf("normalizeTracePageSize(%d) = %d, want %d", c.in, got, c.want)
		}
	}
}

// TestParseTracePageToken：空=0（首页）、合法十进制 seq、非法/负数报错。
func TestParseTracePageToken(t *testing.T) {
	if got, err := parseTracePageToken(""); err != nil || got != 0 {
		t.Errorf(`parseTracePageToken("") = %d,%v, want 0,nil`, got, err)
	}
	if got, err := parseTracePageToken("42"); err != nil || got != 42 {
		t.Errorf("parseTracePageToken(42) = %d,%v, want 42,nil", got, err)
	}
	for _, bad := range []string{"abc", "-1", "1.5", "99999999999999999999999"} {
		if _, err := parseTracePageToken(bad); err == nil {
			t.Errorf("parseTracePageToken(%q): expected error", bad)
		}
	}
}

// TestNextTracePageToken：满页→末步 seq（可能有下页）；不足一页/空页→空（末页终止分页循环）。
func TestNextTracePageToken(t *testing.T) {
	mk := func(seqs ...int64) []store.TraceStep {
		steps := make([]store.TraceStep, len(seqs))
		for i, sq := range seqs {
			steps[i] = store.TraceStep{Seq: sq}
		}
		return steps
	}
	if got := nextTracePageToken(mk(1, 2, 3), 3); got != "3" {
		t.Errorf("full page next token = %q, want 3", got)
	}
	if got := nextTracePageToken(mk(1, 2), 3); got != "" {
		t.Errorf("partial page next token = %q, want empty", got)
	}
	if got := nextTracePageToken(nil, 3); got != "" {
		t.Errorf("empty page next token = %q, want empty", got)
	}
}
