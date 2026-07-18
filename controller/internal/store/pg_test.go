package store

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestPageCursorRoundTrip 校验 encodeCursor/decodeCursor 复合游标 (ts,id) 往返无损：时间取微秒精度
// （PG timestamptz 分辨率），UUID 逐字还原。keyset 分页跨页续读的正确性根基。
func TestPageCursorRoundTrip(t *testing.T) {
	id := uuid.NewString()
	ts := time.Now().Truncate(time.Microsecond) // PG timestamptz 微秒精度

	cur := encodeCursor(ts, id)
	gotTs, gotID, err := decodeCursor(cur)
	if err != nil {
		t.Fatalf("decodeCursor(%q): %v", cur, err)
	}
	if !gotTs.Valid || !gotTs.Time.Equal(ts) {
		t.Errorf("ts 往返: got %v (valid=%v), want %v", gotTs.Time, gotTs.Valid, ts)
	}
	if !gotID.Valid || uuid.UUID(gotID.Bytes).String() != id {
		t.Errorf("id 往返: got %s (valid=%v), want %s", uuid.UUID(gotID.Bytes).String(), gotID.Valid, id)
	}
}

// TestDecodeCursorRejectsMalformed 校验非法游标（空/无分隔/纳秒非数字/UUID 不合法）一律返回 error——
// store 不猜测外来游标意图，交调用方裁决（非静默回退首页）。
func TestDecodeCursorRejectsMalformed(t *testing.T) {
	bad := []string{
		"",                        // 空串
		"no-dot",                  // 无分隔点
		"abc." + uuid.NewString(), // 纳秒非数字
		"123.not-a-uuid",          // UUID 不合法
		"123",                     // 无分隔点（纯数字）
	}
	for _, c := range bad {
		if _, _, err := decodeCursor(c); err == nil {
			t.Errorf("decodeCursor(%q) 应返回 error, got nil", c)
		}
	}
}

// TestClampPageSize 校验页大小夹取 [1,maxPageSize]：<=0（未指定/非法）与超限均取上限兜底，区间内原样
// 返回（防挂死上限：杜绝无界大页）。
func TestClampPageSize(t *testing.T) {
	cases := []struct {
		in, want int64
	}{
		{0, maxPageSize},
		{-1, maxPageSize},
		{maxPageSize + 1, maxPageSize},
		{1, 1},
		{50, 50},
		{maxPageSize, maxPageSize},
	}
	for _, c := range cases {
		if got := clampPageSize(c.in); got != c.want {
			t.Errorf("clampPageSize(%d) = %d, want %d", c.in, got, c.want)
		}
	}
}

// TestKeysetPageQuery 校验 SQL 组装：首页无 WHERE、LIMIT $1、单参数；续页含 (col)<($1,$2)、LIMIT $3、
// 三参数末位为 limit；非法游标透传 error。
func TestKeysetPageQuery(t *testing.T) {
	// 首页（空游标）：无 WHERE。
	q, args, err := keysetPageQuery(listTasksBase, "created_at", "id", "", 10)
	if err != nil {
		t.Fatalf("首页 keysetPageQuery: %v", err)
	}
	if !strings.Contains(q, "ORDER BY created_at DESC, id DESC") || !strings.Contains(q, "LIMIT $1") || strings.Contains(q, "WHERE") {
		t.Errorf("首页 SQL 异常: %q", q)
	}
	if len(args) != 1 || args[0].(int64) != 10 {
		t.Errorf("首页 args 应为 [10], got %v", args)
	}

	// 续页（有游标）：含键集 WHERE，LIMIT 后移至 $3。
	cur := encodeCursor(time.Now().Truncate(time.Microsecond), uuid.NewString())
	q, args, err = keysetPageQuery(listTasksBase, "created_at", "id", cur, 25)
	if err != nil {
		t.Fatalf("续页 keysetPageQuery: %v", err)
	}
	if !strings.Contains(q, "WHERE (created_at, id) < ($1, $2)") || !strings.Contains(q, "LIMIT $3") {
		t.Errorf("续页 SQL 异常: %q", q)
	}
	if len(args) != 3 || args[2].(int64) != 25 {
		t.Errorf("续页 args 应 3 参、末位 25, got %v", args)
	}

	// 非法游标透传 error。
	if _, _, err := keysetPageQuery(listTasksBase, "created_at", "id", "garbage", 10); err == nil {
		t.Error("非法游标应透传 error, got nil")
	}
}
