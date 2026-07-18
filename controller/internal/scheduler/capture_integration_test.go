package scheduler

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	aurav1 "github.com/aura/controller/gen/aura/v1"
	"github.com/aura/controller/internal/registry"
	"github.com/aura/controller/internal/storage"
	"github.com/aura/controller/internal/store"
)

// TestCaptureDataPlaneIntegration 对真 PG + 真 MinIO 演练 M6 capture 数据面全链（录制模拟）：
//
//	StartTrace → 逐步 recordTraceStep（screenshot 步 + a11y 步交替）→ GetTrace seq 游标分页翻页至末页
//	+ 有序校验 + screenshot 步 json_envelope 剥离内联截图（行体积骤减）+ MinIO trace/<id>/<seq>.webp
//	对象在场 + 幂等重放 seq 不重复。
//
// 需 AURA_TEST_PG_DSN + AURA_TEST_MINIO_ENDPOINT/ACCESS_KEY/SECRET_KEY；缺省 skip，使
// `go test ./internal/scheduler` 在无后端机器仍绿。recordTraceStep 在此同步调用（真实 execute 走 `go`）
// 以获得确定性时序。
func TestCaptureDataPlaneIntegration(t *testing.T) {
	dsn := os.Getenv("AURA_TEST_PG_DSN")
	minioEndpoint := os.Getenv("AURA_TEST_MINIO_ENDPOINT")
	if dsn == "" || minioEndpoint == "" {
		t.Skip("set AURA_TEST_PG_DSN + AURA_TEST_MINIO_ENDPOINT (+ACCESS_KEY/SECRET_KEY) to run capture data-plane integration")
	}
	ctx := context.Background()

	pg, err := store.NewPGStore(ctx, dsn)
	if err != nil {
		t.Fatalf("NewPGStore: %v", err)
	}
	defer pg.Close()

	ak := os.Getenv("AURA_TEST_MINIO_ACCESS_KEY")
	sk := os.Getenv("AURA_TEST_MINIO_SECRET_KEY")
	mstore, err := storage.NewMinioStore(minioEndpoint, ak, sk, false)
	if err != nil {
		t.Fatalf("NewMinioStore: %v", err)
	}
	if err := mstore.EnsureBucket(ctx); err != nil {
		t.Fatalf("EnsureBucket: %v", err)
	}

	s := NewScheduler(registry.NewRegistry(nil), pg, 30*time.Minute, nil)
	s.SetStorage(mstore)

	// 录制源 node（写 nodes 表供 GetTrace 的 platform LEFT JOIN 反查）。
	nodeID := uuid.NewString()
	if _, err := pg.UpsertNode(ctx, &aurav1.NodeInfo{NodeId: nodeID, Platform: "linux", Status: "online"}, "", "", ""); err != nil {
		t.Fatalf("UpsertNode: %v", err)
	}

	tid, err := s.StartTrace(nodeID, "integration-recorder")
	if err != nil {
		t.Fatalf("StartTrace: %v", err)
	}
	t.Logf("trace_id=%s node_id=%s", tid, nodeID)

	// 模拟 N 步：奇数步 screenshot（内联截图 2KB），偶数步 get_a11y_tree（无截图）。
	const nSteps = 5
	img := []byte(strings.Repeat("X", 2048)) // 卸桶对象体（放大以显剥离前后体积差）
	fullScreenshotEnv := screenshotEnvelope(t, img)
	a11yEnv := []byte(`{"ok":true,"data":{"nodes":[{"role":"Button","name":"OK","children":[]}],"truncated":false}}`)

	for i := 1; i <= nSteps; i++ {
		seq, who := s.nextTraceStep(tid) // ★execute 内同步捕获（T4 起单临界区原子取 seq+who）
		if seq != int64(i) || who != "integration-recorder" {
			t.Fatalf("同步捕获 seq/who 异常: seq=%d who=%q", seq, who)
		}
		if i%2 == 1 {
			s.recordTraceStep(tid, seq, nodeID, "screenshot", []byte(`{"display":0}`), fullScreenshotEnv, who)
		} else {
			s.recordTraceStep(tid, seq, nodeID, "get_a11y_tree", []byte(`{"depth":3}`), a11yEnv, who)
		}
	}

	// —— seq 游标分页翻页至末页 + 有序校验（page_size=2）——
	var all []store.TraceStep
	var cursor int64
	pages := 0
	for {
		steps, gotNode, gotPlat, gerr := s.GetTrace(ctx, tid, 2, cursor)
		if gerr != nil {
			t.Fatalf("GetTrace: %v", gerr)
		}
		if len(steps) == 0 {
			break
		}
		pages++
		if gotNode != nodeID || gotPlat != "linux" {
			t.Errorf("GetTrace node/platform = %q/%q, want %q/linux", gotNode, gotPlat, nodeID)
		}
		all = append(all, steps...)
		cursor = steps[len(steps)-1].Seq
		if int64(len(steps)) < 2 {
			break
		}
	}
	if len(all) != nSteps {
		t.Fatalf("GetTrace 累计 %d 步, want %d", len(all), nSteps)
	}
	t.Logf("分页翻页: %d 页覆盖 %d 步（page_size=2）", pages, len(all))
	for i, st := range all {
		if st.Seq != int64(i+1) {
			t.Errorf("步 #%d seq=%d, want %d (有序)", i, st.Seq, i+1)
		}
		t.Logf("  seq=%d tool=%-13s screenshot_key=%-30q envelope_bytes=%d", st.Seq, st.Tool, st.ScreenshotKey, len(st.JsonEnvelope))
	}

	// —— screenshot 步剥离校验：json_envelope 无 image_base64，行体积远小于原信封 ——
	sc := all[0] // seq=1 screenshot
	if sc.Tool != "screenshot" {
		t.Fatalf("step1 tool=%s, want screenshot", sc.Tool)
	}
	if strings.Contains(string(sc.JsonEnvelope), "image_base64") {
		t.Error("screenshot 步 json_envelope 仍含 image_base64（未剥离）")
	}
	if len(sc.JsonEnvelope) >= len(fullScreenshotEnv) {
		t.Errorf("剥离后 envelope 未变小: stripped=%d full=%d", len(sc.JsonEnvelope), len(fullScreenshotEnv))
	}
	wantKey := "trace/" + tid + "/1.webp"
	if sc.ScreenshotKey != wantKey {
		t.Errorf("screenshot_key=%q, want %q", sc.ScreenshotKey, wantKey)
	}
	t.Logf("剥离校验: full=%dB → stripped=%dB (screenshot_key=%s)", len(fullScreenshotEnv), len(sc.JsonEnvelope), sc.ScreenshotKey)

	// a11y 步不剥离、无 screenshot_key。
	if all[1].ScreenshotKey != "" || string(all[1].JsonEnvelope) != string(a11yEnv) {
		t.Errorf("a11y 步应原样无 key: key=%q envelope=%s", all[1].ScreenshotKey, all[1].JsonEnvelope)
	}

	// —— MinIO 对象在场校验 ——
	raw, err := minio.New(minioEndpoint, &minio.Options{Creds: credentials.NewStaticV4(ak, sk, ""), Secure: false})
	if err != nil {
		t.Fatalf("raw minio client: %v", err)
	}
	info, err := raw.StatObject(ctx, "aura-artifacts", wantKey, minio.StatObjectOptions{})
	if err != nil {
		t.Fatalf("StatObject %q: %v", wantKey, err)
	}
	if info.Size != int64(len(img)) {
		t.Errorf("卸桶对象 size=%d, want %d", info.Size, len(img))
	}
	t.Logf("MinIO 对象在场: key=%s size=%dB etag=%s", wantKey, info.Size, info.ETag)

	// —— 幂等重放：同 (trace_id,seq) 重复 INSERT 不产生重复行（ON CONFLICT DO NOTHING）——
	if err := pg.CreateTraceStep(ctx, store.TraceStep{TraceID: tid, Seq: 1, NodeID: nodeID, Tool: "screenshot", JsonEnvelope: sc.JsonEnvelope}); err != nil {
		t.Fatalf("idempotent re-insert: %v", err)
	}
	steps2, _, _, err := s.GetTrace(ctx, tid, 100, 0)
	if err != nil {
		t.Fatalf("GetTrace after replay: %v", err)
	}
	if len(steps2) != nSteps {
		t.Errorf("幂等重放后步数 = %d, want %d (ON CONFLICT DO NOTHING)", len(steps2), nSteps)
	}
	t.Logf("幂等重放: 重复 INSERT (trace_id,seq=1) 后总步数仍 %d", len(steps2))
}
