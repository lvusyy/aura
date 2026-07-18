package scheduler

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/aura/controller/internal/registry"
)

// fakePutter 记录 PutObject 调用，供 offloadScreenshot 单测（无需真 MinIO）。
type fakePutter struct {
	calls []fakePutCall
	err   error
}

type fakePutCall struct {
	key         string
	contentType string
	data        []byte
}

func (f *fakePutter) PutObject(_ context.Context, key string, data []byte, contentType string) error {
	f.calls = append(f.calls, fakePutCall{key: key, contentType: contentType, data: append([]byte(nil), data...)})
	return f.err
}

// screenshotEnvelope 构造一个内联 base64 截图的成功信封（形如节点 screenshot 工具回执 Envelope<ScreenshotResult>）。
func screenshotEnvelope(t *testing.T, img []byte) []byte {
	t.Helper()
	env := map[string]any{
		"ok": true,
		"data": map[string]any{
			"image_base64": base64.StdEncoding.EncodeToString(img),
			"mime":         "image/webp",
			"meta":         map[string]any{"native_w": 1920, "native_h": 1080, "display_w": 1280, "display_h": 720, "scale": 1.5},
		},
	}
	b, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal screenshot envelope: %v", err)
	}
	return b
}

// containsImageBase64 判定 envelope 的 data 是否仍含 image_base64 字段。
func containsImageBase64(env []byte) bool {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(env, &top); err != nil {
		return false
	}
	var data map[string]json.RawMessage
	if err := json.Unmarshal(top["data"], &data); err != nil {
		return false
	}
	_, ok := data["image_base64"]
	return ok
}

// TestStripInlineScreenshot_StripsScreenshot：截图信封剥离 image_base64、解出原图字节，且保留同级
// data.mime/data.meta 与顶层 ok（仅剔除内联截图，不吞其余信息）。
func TestStripInlineScreenshot_StripsScreenshot(t *testing.T) {
	img := []byte("fake-webp-bytes-\x00\x01\x02\xff")
	env := screenshotEnvelope(t, img)

	stripped, gotImg := stripInlineScreenshot(env)
	if string(gotImg) != string(img) {
		t.Errorf("decoded image = %q, want %q", gotImg, img)
	}
	if containsImageBase64(stripped) {
		t.Error("stripped envelope 仍含 image_base64")
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(stripped, &top); err != nil {
		t.Fatalf("stripped not JSON object: %v", err)
	}
	if _, ok := top["ok"]; !ok {
		t.Error("stripped envelope 丢失顶层 ok")
	}
	var data map[string]json.RawMessage
	if err := json.Unmarshal(top["data"], &data); err != nil {
		t.Fatalf("stripped data not object: %v", err)
	}
	if _, ok := data["mime"]; !ok {
		t.Error("stripped envelope 丢失 data.mime")
	}
	if _, ok := data["meta"]; !ok {
		t.Error("stripped envelope 丢失 data.meta")
	}
}

// TestStripInlineScreenshot_PassThrough：非截图 / 畸形信封原样返回 + nil 图像（不误剥离、不吞信息）。
func TestStripInlineScreenshot_PassThrough(t *testing.T) {
	cases := map[string]string{
		"a11y-no-image":  `{"ok":true,"data":{"nodes":[{"role":"Button","name":"OK"}],"truncated":false}}`,
		"empty-base64":   `{"ok":true,"data":{"image_base64":"","mime":"image/webp"}}`,
		"data-absent":    `{"ok":true}`,
		"data-null":      `{"ok":true,"data":null}`,
		"error-envelope": `{"ok":false,"error":{"code":"E_BUSY","message":"x"}}`,
		"non-json":       `not-a-json-envelope`,
		"invalid-base64": `{"ok":true,"data":{"image_base64":"!!!not-base64!!!"}}`,
	}
	for name, env := range cases {
		t.Run(name, func(t *testing.T) {
			stripped, img := stripInlineScreenshot([]byte(env))
			if img != nil {
				t.Errorf("expected nil image, got %d bytes", len(img))
			}
			if string(stripped) != env {
				t.Errorf("envelope mutated:\n got %s\nwant %s", stripped, env)
			}
		})
	}
}

// TestOffloadScreenshot_UploadsAndStrips：配置 storage 时截图步经 PutObject 卸桶
// key=trace/<tid>/<seq>.webp（contentType image/webp、data=原图），返回剥离后 envelope + 该 key。
func TestOffloadScreenshot_UploadsAndStrips(t *testing.T) {
	img := []byte("webp-\x00\xff-payload")
	env := screenshotEnvelope(t, img)
	fp := &fakePutter{}
	s := &Scheduler{storage: fp}

	stripped, key := s.offloadScreenshot(context.Background(), "trace-xyz", 7, env)

	if key != "trace/trace-xyz/7.webp" {
		t.Errorf("screenshot_key = %q, want trace/trace-xyz/7.webp", key)
	}
	if len(fp.calls) != 1 {
		t.Fatalf("PutObject 调用 %d 次, want 1", len(fp.calls))
	}
	c := fp.calls[0]
	if c.key != "trace/trace-xyz/7.webp" || c.contentType != "image/webp" || string(c.data) != string(img) {
		t.Errorf("PutObject 参数 = {key:%q ct:%q data:%q}, want {trace/trace-xyz/7.webp image/webp %q}", c.key, c.contentType, c.data, img)
	}
	if containsImageBase64(stripped) {
		t.Error("offloadScreenshot 返回的 envelope 仍含 image_base64")
	}
}

// TestOffloadScreenshot_NilStorageKeepsInline：未配置 storage 时保留原 envelope（内联图像不丢）+ 空 key，不 panic。
func TestOffloadScreenshot_NilStorageKeepsInline(t *testing.T) {
	env := screenshotEnvelope(t, []byte("img"))
	s := &Scheduler{} // storage nil

	stripped, key := s.offloadScreenshot(context.Background(), "t", 1, env)
	if key != "" {
		t.Errorf("nil storage: key = %q, want empty", key)
	}
	if !containsImageBase64(stripped) {
		t.Error("nil storage: 应保留内联 image_base64（不丢图像）")
	}
}

// TestOffloadScreenshot_UploadErrorKeepsFull：PutObject 失败时回退存原 envelope + 空 key（best-effort 不丢图像）。
func TestOffloadScreenshot_UploadErrorKeepsFull(t *testing.T) {
	env := screenshotEnvelope(t, []byte("img"))
	fp := &fakePutter{err: errors.New("minio down")}
	s := &Scheduler{storage: fp}

	stripped, key := s.offloadScreenshot(context.Background(), "t", 1, env)
	if key != "" {
		t.Errorf("upload error: key = %q, want empty", key)
	}
	if !containsImageBase64(stripped) {
		t.Error("upload error: 应回退保留原 envelope（含 image_base64）")
	}
}

// TestOffloadScreenshot_NonScreenshotPassThrough：非截图步（无 image_base64）不调用 PutObject，原样返回 + 空 key。
func TestOffloadScreenshot_NonScreenshotPassThrough(t *testing.T) {
	env := []byte(`{"ok":true,"data":{"nodes":[]}}`)
	fp := &fakePutter{}
	s := &Scheduler{storage: fp}

	stripped, key := s.offloadScreenshot(context.Background(), "t", 1, env)
	if key != "" || len(fp.calls) != 0 {
		t.Errorf("非截图步不应卸桶：key=%q calls=%d", key, len(fp.calls))
	}
	if string(stripped) != string(env) {
		t.Errorf("非截图步 envelope 被改动: %s", stripped)
	}
}

// TestCaptureSyncPrimitives：execute 内同步捕获 seq/who 的基元——nextTraceStep 单临界区原子取
// (seq ++, who)（T4 起 nextSeq/traceWho 并入 LeaseStore.NextStep）；未知/已释放 trace 返回 (0, "")
//（旁路 seq>0 守卫据此跳过不录空步，MAJOR-2/MINOR-a）。
func TestCaptureSyncPrimitives(t *testing.T) {
	s := NewScheduler(registry.NewRegistry(nil), nil, 30*time.Minute, nil)
	tid, err := s.StartTrace("node-a", "recorder-1")
	if err != nil {
		t.Fatalf("StartTrace: %v", err)
	}

	for want := int64(1); want <= 3; want++ {
		seq, who := s.nextTraceStep(tid)
		if seq != want {
			t.Errorf("nextTraceStep seq = %d, want %d", seq, want)
		}
		if who != "recorder-1" {
			t.Errorf("nextTraceStep who = %q, want recorder-1", who)
		}
	}
	if seq, who := s.nextTraceStep("unknown"); seq != 0 || who != "" {
		t.Errorf("nextTraceStep(unknown) = (%d, %q), want (0, \"\")", seq, who)
	}
	s.StopTrace(tid)
	if seq, _ := s.nextTraceStep(tid); seq != 0 {
		t.Errorf("released nextTraceStep seq = %d, want 0 (旁路跳过不录空步)", seq)
	}
}

// TestRecordTraceStep_NilStoreNoop：store 未配置时 recordTraceStep 直接返回、不 panic（nil-safe）。
func TestRecordTraceStep_NilStoreNoop(t *testing.T) {
	s := NewScheduler(registry.NewRegistry(nil), nil, time.Minute, nil)
	s.recordTraceStep("t", 1, "node", "screenshot", nil, screenshotEnvelope(t, []byte("x")), "who")
}
