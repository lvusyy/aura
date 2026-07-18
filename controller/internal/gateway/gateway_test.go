package gateway

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

// fakeUploader 记录 GrantAndAwait 调用，供 gateway 探测逻辑单测（不依赖 transport / MinIO）。
type fakeUploader struct {
	calls   int
	nodeID  string
	key     string
	taskID  string
	traceID string
	err     error
}

func (f *fakeUploader) GrantAndAwait(_ context.Context, nodeID, key, taskID, traceID string) error {
	f.calls++
	f.nodeID = nodeID
	f.key = key
	f.taskID = taskID
	f.traceID = traceID
	return f.err
}

// TestAwaitBypassUploadTriggersOnNeedsUpload：envelope 声明 needs_upload+key 时触发 uploader，
// 并透传正确的 (node_id,key)。
func TestAwaitBypassUploadTriggersOnNeedsUpload(t *testing.T) {
	up := &fakeUploader{}
	g := NewGateway(nil)
	g.SetUploader(up)

	env := []byte(`{"ok":true,"data":{"needs_upload":true,"key":"artifacts/rec-1.mp4","resource_link":"s3://aura/rec-1"}}`)
	g.awaitBypassUpload(context.Background(), "node-1", "task-1", "trace-1", env)

	if up.calls != 1 {
		t.Fatalf("expected uploader called once, got %d", up.calls)
	}
	if up.nodeID != "node-1" || up.key != "artifacts/rec-1.mp4" {
		t.Fatalf("uploader called with wrong args: node=%q key=%q", up.nodeID, up.key)
	}
}

// TestAwaitBypassUploadSkips：非 needs_upload / 缺 key / 非 JSON / 空 envelope 一律不触发 uploader
// （探测专用：任何解析失败或字段缺失均忽略，原样透传）。
func TestAwaitBypassUploadSkips(t *testing.T) {
	up := &fakeUploader{}
	g := NewGateway(nil)
	g.SetUploader(up)

	for _, env := range [][]byte{
		[]byte(`{"ok":true,"data":{"text":"hello"}}`),       // 无 needs_upload 字段
		[]byte(`{"ok":true,"data":{"needs_upload":false}}`), // 显式 false
		[]byte(`{"ok":true,"data":{"needs_upload":true}}`),  // 缺 key
		[]byte(`not-json-at-all`),                           // 非 JSON
		{},                                                  // 空 envelope
	} {
		g.awaitBypassUpload(context.Background(), "node-1", "task-1", "trace-1", env)
	}
	if up.calls != 0 {
		t.Fatalf("expected uploader never called, got %d", up.calls)
	}
}

// TestAwaitBypassUploadNilUploader：未装配 uploader（MinIO 未配）时 needs_upload 路径降级、不 panic。
func TestAwaitBypassUploadNilUploader(t *testing.T) {
	g := NewGateway(nil) // 未 SetUploader
	env := []byte(`{"ok":true,"data":{"needs_upload":true,"key":"k"}}`)
	g.awaitBypassUpload(context.Background(), "node-1", "task-1", "trace-1", env) // 不应 panic
}

// TestAwaitBypassUploadDegradesOnError：uploader 失败仅降级（不 panic、不吞不改写 envelope），
// 调用方仍返回原 envelope（产物在节点本地，resource_link 契约不破）。
func TestAwaitBypassUploadDegradesOnError(t *testing.T) {
	up := &fakeUploader{err: errors.New("upload did not complete within TTL")}
	g := NewGateway(nil)
	g.SetUploader(up)

	env := []byte(`{"ok":true,"data":{"needs_upload":true,"key":"k"}}`)
	g.awaitBypassUpload(context.Background(), "node-1", "task-1", "trace-1", env)

	if up.calls != 1 {
		t.Fatalf("expected uploader called once even on error path, got %d", up.calls)
	}
}

// captureLogs 以内存 handler 临时替换默认 slog logger，返回读取函数（测试毕自动恢复）。
func captureLogs(t *testing.T) func() string {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf.String
}

// TestAwaitBypassUploadLogsDistinguishFailureFromTimeout（T10 降级日志区分 + 透传断言）：
// 节点显式失败（UploadFailed 帧唤醒，错误链无 DeadlineExceeded）打 "bypass upload failed"；
// 兜底超时（awaitUploadTimeout 窗满，错误链含 DeadlineExceeded）打 "bypass upload await timed out"。
// 两路降级 envelope 均逐字节原样（不改写不吞）。
func TestAwaitBypassUploadLogsDistinguishFailureFromTimeout(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		wantLog  string
		rejected string
	}{
		{
			name:     "explicit node failure",
			err:      errors.New(`node reported upload failure for node node-1 key "k": presigned PUT failed: HTTP 503`),
			wantLog:  "bypass upload failed",
			rejected: "bypass upload await timed out",
		},
		{
			name:     "fallback timeout",
			err:      fmt.Errorf("await upload complete for node node-1 key %q: %w", "k", context.DeadlineExceeded),
			wantLog:  "bypass upload await timed out",
			rejected: "bypass upload failed",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureLogs(t)
			g := NewGateway(nil)
			g.SetUploader(&fakeUploader{err: tc.err})

			env := []byte(`{"ok":true,"data":{"needs_upload":true,"key":"k","resource_link":"s3://aura/k"}}`)
			orig := append([]byte(nil), env...)
			g.awaitBypassUpload(context.Background(), "node-1", "task-1", "trace-1", env)

			if !bytes.Equal(env, orig) {
				t.Fatal("degraded path must pass the original envelope through byte-for-byte")
			}
			got := logs()
			if !strings.Contains(got, tc.wantLog) {
				t.Fatalf("log must contain %q, got:\n%s", tc.wantLog, got)
			}
			if strings.Contains(got, tc.rejected) {
				t.Fatalf("log must not contain %q (discrimination broken), got:\n%s", tc.rejected, got)
			}
		})
	}
}
