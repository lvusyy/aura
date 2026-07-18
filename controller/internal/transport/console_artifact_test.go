package transport

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	aurav1 "github.com/aura/controller/gen/aura/v1"
)

// TestValidArtifactKey 直测键白名单纯函数（免 MinIO）：仅 trace/ 或 recordings/ 前缀且无 ".." 穿越段
// 通过，防前端经代理任意读取产物桶。
func TestValidArtifactKey(t *testing.T) {
	cases := []struct {
		key  string
		want bool
	}{
		{"trace/abc/1.webp", true},
		{"trace/8f3c9a2b-1d4e-4f5a-9b6c/42.webp", true}, // UUID 式 trace_id（真实键形），无 ".." 段
		{"recordings/18c274d0846d376f0000.mp4", true},   // M12 录屏 MP4 键（GetArtifact 代取 <video> 字节）
		{"", false},                     // 空 key
		{"other/1.webp", false},         // 非白名单前缀
		{"aura-artifacts/secret", false}, // 任意读桶试探
		{"trace/../secret", false},      // 前缀内穿越段
		{"recordings/../secret", false}, // 录屏前缀内穿越段
		{"trace/a/../../etc/passwd", false},
		{"/trace/1.webp", false}, // 前缀须在串首
		{"Trace/1.webp", false},  // 大小写敏感
		{"Recordings/1.mp4", false}, // 大小写敏感（录屏前缀同）
	}
	for _, c := range cases {
		if got := validArtifactKey(c.key); got != c.want {
			t.Errorf("validArtifactKey(%q) = %v, want %v", c.key, got, c.want)
		}
	}
}

// TestGetArtifact_KeyValidation 校验非法 key 早于服务可用性即拒 InvalidArgument（请求校验先于依赖判空，
// 故 nil minio 亦可断言此路径）。
func TestGetArtifact_KeyValidation(t *testing.T) {
	srv := NewConsoleServiceServer(nil, nil, nil, nil, nil)
	ctx := context.Background()
	for _, key := range []string{"", "other/1.webp", "trace/../secret"} {
		_, err := srv.GetArtifact(ctx, connect.NewRequest(&aurav1.GetArtifactRequest{Key: key}))
		if connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Errorf("GetArtifact(key=%q): want InvalidArgument, got %v", key, err)
		}
	}
}

// TestGetArtifact_UnavailableWhenNoMinio 校验 key 合法但未配置 MinIO 时返回 Unavailable
// （产物不可达明确降级，非静默空响应）。happy path（真 GetObject 代取）由 dev 联调覆盖：minio 为具体
// 类型无法 mock，同 console_query.go 纯函数测试缝惯例。
func TestGetArtifact_UnavailableWhenNoMinio(t *testing.T) {
	srv := NewConsoleServiceServer(nil, nil, nil, nil, nil)
	ctx := context.Background()
	_, err := srv.GetArtifact(ctx, connect.NewRequest(&aurav1.GetArtifactRequest{Key: "trace/abc/1.webp"}))
	if connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("GetArtifact with nil minio: want Unavailable, got %v", err)
	}
}
