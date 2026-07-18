package storage

import (
	"context"
	"reflect"
	"testing"
	"time"
)

// TestParseZoneEndpoints 证 AURA_MINIO_ENDPOINTS 解析：zone=endpoint 逗号分隔、空项跳过、
// 首个等号分隔（endpoint host:port 不含等号无歧义）；格式非法 fail fast。
func TestParseZoneEndpoints(t *testing.T) {
	// 合法：多域 + 前后空白 + 空项跳过。
	got, err := parseZoneEndpoints(" lan=192.168.22.240:9000 , jump=100.78.22.127:9000 , ")
	if err != nil {
		t.Fatalf("parseZoneEndpoints valid: unexpected error %v", err)
	}
	want := map[string]string{
		"lan":  "192.168.22.240:9000",
		"jump": "100.78.22.127:9000",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseZoneEndpoints = %v, want %v", got, want)
	}

	// 非法项 fail fast（缺等号 / 空 zone / 空 endpoint）。
	for _, bad := range []string{"noequal", "=192.168.22.240:9000", "lan=", "lan=1.2.3.4:9000,badpair"} {
		if _, err := parseZoneEndpoints(bad); err == nil {
			t.Errorf("parseZoneEndpoints(%q): expected error, got nil", bad)
		}
	}
}

// TestPresignedPutDispatchesByZone 证 GrantUpload 分派地基：AURA_MINIO_ENDPOINTS 建多 zone client，
// PresignedPut 按 zone 选对应端点签发（URL host 即该域端点），空/未知 zone 回落默认 zone
//（AURA_MINIO_PUBLIC_ENDPOINT）。预签名为离线签发（固定 region），无需真 MinIO 后端。
func TestPresignedPutDispatchesByZone(t *testing.T) {
	t.Setenv("AURA_MINIO_ENDPOINT", "127.0.0.1:9000")
	t.Setenv("AURA_MINIO_ACCESS_KEY", "testak")
	t.Setenv("AURA_MINIO_SECRET_KEY", "testsk")
	t.Setenv("AURA_MINIO_PUBLIC_ENDPOINT", "192.168.22.240:9000")
	t.Setenv("AURA_MINIO_ENDPOINTS", "lan=192.168.22.240:9000,jump=100.78.22.127:9000")

	store, err := NewMinioStoreFromEnv()
	if err != nil {
		t.Fatalf("NewMinioStoreFromEnv: %v", err)
	}
	if store == nil {
		t.Fatal("NewMinioStoreFromEnv: got nil store (AURA_MINIO_ENDPOINT set)")
	}

	ctx := context.Background()
	cases := []struct {
		name     string
		zone     string
		wantHost string
	}{
		{"lan 域→直连端点", "lan", "192.168.22.240:9000"},
		{"jump 域→跳板端点（收口 ISS-20260714-003）", "jump", "100.78.22.127:9000"},
		{"空域→默认 zone（PUBLIC_ENDPOINT）", "", "192.168.22.240:9000"},
		{"未知域→回落默认 zone", "unknown-zone", "192.168.22.240:9000"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			u, err := store.PresignedPut(ctx, "test/key.bin", time.Minute, c.zone)
			if err != nil {
				t.Fatalf("PresignedPut(zone=%q): %v", c.zone, err)
			}
			if u.Host != c.wantHost {
				t.Errorf("PresignedPut(zone=%q): host = %q, want %q", c.zone, u.Host, c.wantHost)
			}
		})
	}
}

// TestPresignedPutFallsBackWithoutEndpoints 证向后兼容：仅 AURA_MINIO_PUBLIC_ENDPOINT（无 ENDPOINTS）时
// 任意 zone 均回落默认端点——既有单端点部署零行为变化。
func TestPresignedPutFallsBackWithoutEndpoints(t *testing.T) {
	t.Setenv("AURA_MINIO_ENDPOINT", "127.0.0.1:9000")
	t.Setenv("AURA_MINIO_ACCESS_KEY", "testak")
	t.Setenv("AURA_MINIO_SECRET_KEY", "testsk")
	t.Setenv("AURA_MINIO_PUBLIC_ENDPOINT", "192.168.22.240:9000")

	store, err := NewMinioStoreFromEnv()
	if err != nil {
		t.Fatalf("NewMinioStoreFromEnv: %v", err)
	}
	ctx := context.Background()
	for _, zone := range []string{"", "lan", "jump", "anything"} {
		u, err := store.PresignedPut(ctx, "test/key.bin", time.Minute, zone)
		if err != nil {
			t.Fatalf("PresignedPut(zone=%q): %v", zone, err)
		}
		if u.Host != "192.168.22.240:9000" {
			t.Errorf("PresignedPut(zone=%q): host = %q, want 192.168.22.240:9000 (无 ENDPOINTS 全回落默认)", zone, u.Host)
		}
	}
}
