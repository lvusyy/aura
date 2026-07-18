package storage

import (
	"bytes"
	"context"
	"os"
	"testing"

	"github.com/google/uuid"
)

// TestGetObjectIntegration 对真 MinIO 演练 GetObject：PutObject 写入 → GetObject 读回字节 + content-type
// 一致；不存在的 key 返回明确 error。需 AURA_TEST_MINIO_ENDPOINT(+ACCESS_KEY/SECRET_KEY)；缺省 skip 使
// 无后端机器 `go test ./internal/storage` 仍绿（沿 capture_integration_test 惯例）。
func TestGetObjectIntegration(t *testing.T) {
	endpoint := os.Getenv("AURA_TEST_MINIO_ENDPOINT")
	if endpoint == "" {
		t.Skip("set AURA_TEST_MINIO_ENDPOINT (+ACCESS_KEY/SECRET_KEY) to run GetObject integration")
	}
	ctx := context.Background()
	ak := os.Getenv("AURA_TEST_MINIO_ACCESS_KEY")
	sk := os.Getenv("AURA_TEST_MINIO_SECRET_KEY")

	store, err := NewMinioStore(endpoint, ak, sk, false)
	if err != nil {
		t.Fatalf("NewMinioStore: %v", err)
	}
	if err := store.EnsureBucket(ctx); err != nil {
		t.Fatalf("EnsureBucket: %v", err)
	}

	// PutObject 写入 → GetObject 读回，字节与 content-type 一致。
	key := "test/getobject/" + uuid.NewString() + ".bin"
	want := []byte("hello aura getobject 你好，字节安全")
	const wantCT = "application/octet-stream"
	if err := store.PutObject(ctx, key, want, wantCT); err != nil {
		t.Fatalf("PutObject: %v", err)
	}
	got, ct, err := store.GetObject(ctx, key)
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("GetObject 字节不一致: got %d bytes, want %d bytes", len(got), len(want))
	}
	if ct != wantCT {
		t.Errorf("GetObject content-type = %q, want %q", ct, wantCT)
	}

	// 不存在的 key 返回明确 error（经 Stat 暴露）。
	if _, _, err := store.GetObject(ctx, "test/getobject/does-not-exist-"+uuid.NewString()); err == nil {
		t.Error("GetObject(不存在 key) 应返回 error, got nil")
	}

	t.Logf("GetObject 集成通过: key=%s round-trip %d bytes ct=%s", key, len(got), ct)
}
