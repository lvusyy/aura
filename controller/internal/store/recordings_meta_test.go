package store

import (
	"context"
	"os"
	"testing"

	"github.com/google/uuid"
)

// TestRecordingsMetaIntegration 对真 PG 演练 recordings_meta 往返（M12 批C + 批E 关联富化）：Upsert 写入
// →RecordingMetas 批查命中→同 key 重传覆盖（UploadComplete 幂等重传语义）→兜底空写不回退既有 task/trace
// 关联（COALESCE 语义）→无映射 key 不出现在返回 map（老对象留空如实）→空键集免查询。需 AURA_TEST_PG_DSN；
// 缺省 skip（沿 enrollment_test 惯例，无 PG 机器 go test ./internal/store 仍绿）。key 带唯一后缀避免与既有
// 行碰撞，不清场（白盒点查断言）。
func TestRecordingsMetaIntegration(t *testing.T) {
	dsn := os.Getenv("AURA_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("set AURA_TEST_PG_DSN to run recordings meta integration")
	}
	ctx := context.Background()
	pg, err := NewPGStore(ctx, dsn)
	if err != nil {
		t.Fatalf("NewPGStore: %v", err)
	}
	defer pg.Close()

	suffix := uuid.NewString()
	keyA := "recordings/metaC-" + suffix + "-a.mp4"
	keyB := "recordings/metaC-" + suffix + "-b.mp4"
	keyGhost := "recordings/metaC-" + suffix + "-ghost.mp4" // 从未写入（模拟建表前老对象）
	nodeA := uuid.NewString()
	nodeB := uuid.NewString()
	taskA := uuid.NewString()
	traceA := uuid.NewString()

	// —— 写入两条（A 带 task/trace 关联，B 兜底空关联）+ 批查命中（含 ghost：无映射 key 不出现在 map）——
	if err := pg.UpsertRecordingMeta(ctx, keyA, nodeA, taskA, traceA); err != nil {
		t.Fatalf("UpsertRecordingMeta A: %v", err)
	}
	if err := pg.UpsertRecordingMeta(ctx, keyB, nodeB, "", ""); err != nil {
		t.Fatalf("UpsertRecordingMeta B: %v", err)
	}
	meta, err := pg.RecordingMetas(ctx, []string{keyA, keyB, keyGhost})
	if err != nil {
		t.Fatalf("RecordingMetas: %v", err)
	}
	if meta[keyA].NodeID != nodeA || meta[keyA].TaskID != taskA || meta[keyA].TraceID != traceA {
		t.Errorf("meta[A] = %+v, want node=%s task=%s trace=%s", meta[keyA], nodeA, taskA, traceA)
	}
	if meta[keyB].NodeID != nodeB || meta[keyB].TaskID != "" || meta[keyB].TraceID != "" {
		t.Errorf("meta[B] = %+v, want node=%s with empty task/trace", meta[keyB], nodeB)
	}
	if _, ok := meta[keyGhost]; ok {
		t.Errorf("ghost key unexpectedly present in meta map: %+v", meta[keyGhost])
	}

	// —— 兜底空写（收帧臂路径）不回退既有关联：node_id 覆盖，task/trace 经 COALESCE 保留 ——
	if err := pg.UpsertRecordingMeta(ctx, keyA, nodeB, "", ""); err != nil {
		t.Fatalf("UpsertRecordingMeta A(fallback overwrite): %v", err)
	}
	meta, err = pg.RecordingMetas(ctx, []string{keyA})
	if err != nil {
		t.Fatalf("RecordingMetas after fallback overwrite: %v", err)
	}
	if meta[keyA].NodeID != nodeB {
		t.Errorf("meta[A].NodeID after overwrite = %q, want %q", meta[keyA].NodeID, nodeB)
	}
	if meta[keyA].TaskID != taskA || meta[keyA].TraceID != traceA {
		t.Errorf("meta[A] task/trace regressed after fallback write = %+v, want task=%s trace=%s kept", meta[keyA], taskA, traceA)
	}

	// —— 空键集：免查询、空 map 非 nil ——
	empty, err := pg.RecordingMetas(ctx, nil)
	if err != nil {
		t.Fatalf("RecordingMetas(nil): %v", err)
	}
	if empty == nil || len(empty) != 0 {
		t.Errorf("RecordingMetas(nil) = %v, want empty non-nil map", empty)
	}
}
