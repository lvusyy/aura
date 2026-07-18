package transport

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	aurav1 "github.com/aura/controller/gen/aura/v1"
	"github.com/aura/controller/internal/storage"
)

// 纯函数覆盖分页归一/游标解析/切片（storage 具体类型无法 mock，纯函数是免 MinIO 的单测缝，同
// console_query.go/console_artifact.go 惯例）；handler 用例覆盖 nil-minio 降级。真 ListObjects 列举 +
// GetArtifact 代取 MP4 由构建机 live curl / 浏览器验收覆盖，本单测不构真 MinIO。

func TestNormalizeRecordingsPageSize(t *testing.T) {
	cases := []struct {
		in   int32
		want int
	}{
		{0, defaultRecordingsPageSize},   // 未指定 → 默认
		{-5, defaultRecordingsPageSize},  // 非法负数 → 默认
		{20, 20},                         // 正常值原样
		{maxRecordingsPageSize, maxRecordingsPageSize},
		{maxRecordingsPageSize + 1, maxRecordingsPageSize}, // 超限截断
	}
	for _, c := range cases {
		if got := normalizeRecordingsPageSize(c.in); got != c.want {
			t.Errorf("normalizeRecordingsPageSize(%d) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestParseOffsetToken(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"", 0},        // 首页
		{"20", 20},     // 正常游标
		{"abc", 0},     // 非法 → 0
		{"-3", 0},      // 负数 → 0
		{"0", 0},       // 显式 0
	}
	for _, c := range cases {
		if got := parseOffsetToken(c.in); got != c.want {
			t.Errorf("parseOffsetToken(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestPageRecordings(t *testing.T) {
	objs := []storage.RecordingObject{ // 5 条录屏（已按最后修改降序，pageRecordings 只切片不排序）
		{Key: "recordings/rec0.mp4"},
		{Key: "recordings/rec1.mp4"},
		{Key: "recordings/rec2.mp4"},
		{Key: "recordings/rec3.mp4"},
		{Key: "recordings/rec4.mp4"},
	}

	// 首页取 2 条，后有余 → next=偏移 2。
	page, next := pageRecordings(objs, 0, 2)
	if len(page) != 2 || next != "2" {
		t.Fatalf("首页(0,2): got len=%d next=%q, want len=2 next=2", len(page), next)
	}
	// 末页部分取满（offset 4, size 2）→ 只剩 1 条，next 空（末页）。
	page, next = pageRecordings(objs, 4, 2)
	if len(page) != 1 || next != "" {
		t.Fatalf("末页(4,2): got len=%d next=%q, want len=1 next=''", len(page), next)
	}
	// 恰好取满且到底（offset 3, size 2）→ 2 条，next 空。
	page, next = pageRecordings(objs, 3, 2)
	if len(page) != 2 || next != "" {
		t.Fatalf("到底(3,2): got len=%d next=%q, want len=2 next=''", len(page), next)
	}
	// offset 越界（>=总数）→ 空页 + 空游标（翻页终止）。
	page, next = pageRecordings(objs, 5, 2)
	if len(page) != 0 || next != "" {
		t.Fatalf("越界(5,2): got len=%d next=%q, want len=0 next=''", len(page), next)
	}
	// 空列表 → 空页 + 空游标。
	page, next = pageRecordings(nil, 0, 20)
	if len(page) != 0 || next != "" {
		t.Fatalf("空列表: got len=%d next=%q, want len=0 next=''", len(page), next)
	}
}

// TestListRecordings_UnavailableWhenNoMinio 校验未配置 MinIO 时返回 Unavailable（录屏物料不可达明确降级，
// 非静默空页，同 GetArtifact 惯例）。happy path（真 ListObjects + GetArtifact 代取）由构建机/浏览器验收覆盖。
func TestListRecordings_UnavailableWhenNoMinio(t *testing.T) {
	srv := NewConsoleServiceServer(nil, nil, nil, nil, nil)
	_, err := srv.ListRecordings(context.Background(), connect.NewRequest(&aurav1.ListRecordingsRequest{PageSize: 20}))
	if connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("ListRecordings with nil minio: want Unavailable, got %v", err)
	}
}
