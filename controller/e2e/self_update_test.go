//go:build e2e

package e2e

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"

	"connectrpc.com/connect"

	aurav1 "github.com/aura/controller/gen/aura/v1"
)

// TestSelfUpdate 场景⑥（M16 Phase2）：节点 self-update 全链——坏制品拒斥（不砖机）+ 好制品换刀重启。
//
// 流程：节点跑「拷贝」二进制 → ① 上传非可执行垃圾制品 → SelfUpdateNode → staged=false（sanity 探针
// 换刀前拦下）、盘上二进制未动、节点照常可派发；② 上传尾部加噪声的同二进制（sha 变、ELF 仍可执行）→
// staged=true → 节点 exec 自替换重启 → 重注册（ConnectedAtMs 变化）→ 盘上 sha == 制品 sha（换刀铁证）
// → 网关照常可派发。需 MinIO（AURA_E2E_MINIO_ENDPOINT，TestMain 已映射给 controller）；未配则 skip。
func TestSelfUpdate(t *testing.T) {
	if os.Getenv("AURA_E2E_MINIO_ENDPOINT") == "" {
		t.Skip("AURA_E2E_MINIO_ENDPOINT unset; self-update scenario skipped")
	}

	dataDir := filepath.Join(h.dataRoot, "node-selfupdate")
	token := h.generateToken(t, "linux", "")
	nodeID := h.enrollNode(t, dataDir, token, "linux")

	// 节点跑拷贝二进制（换刀改写自身文件，绝不动共享构建产物）；bin/ 目录布局与 install.sh M16 布局同形。
	liveBin := filepath.Join(dataDir, "bin", "aura-node")
	origBytes := copyFile(t, h.nodeBin, liveBin)

	stop, _ := h.startNodeBin(t, liveBin, dataDir, "desktop")
	defer stop()
	h.waitDispatchable(t, nodeID, 30*time.Second)

	admin := h.adminClient()
	info := h.nodeInfo(t, admin, nodeID)
	if info == nil {
		t.Fatal("node not in ListNodes after dispatchable")
	}
	hostPlatform := info.GetHostPlatform()
	if hostPlatform == "" {
		t.Fatal("node did not report host_platform (Register field 16)")
	}
	t.Logf("node host_platform=%s node_version=%s", hostPlatform, info.GetNodeVersion())

	// ① 坏制品：非可执行内容。sha/size 服务端登记一致（下载校验会过），sanity `--version` 探针在换刀前拦下。
	junk := make([]byte, 4096)
	if _, err := rand.Read(junk); err != nil {
		t.Fatal(err)
	}
	h.uploadRelease(t, hostPlatform, "0.0.0-broken", junk)
	resp, err := admin.SelfUpdateNode(context.Background(), connect.NewRequest(&aurav1.SelfUpdateNodeRequest{
		NodeId: nodeID, Version: "0.0.0-broken",
	}))
	if err != nil {
		t.Fatalf("SelfUpdateNode(broken): %v", err)
	}
	if resp.Msg.GetStaged() {
		t.Fatal("broken artifact must not stage")
	}
	t.Logf("broken artifact refused as expected: %s", resp.Msg.GetMessage())
	if got := sha256File(t, liveBin); got != sha256Bytes(origBytes) {
		t.Fatal("binary on disk changed after FAILED update (must be untouched)")
	}
	echo := h.toolCall(t, nodeID, "run_command", map[string]any{"cmd": "/bin/echo", "args": []string{"still-alive"}})
	if ok, _ := echo["ok"].(bool); !ok {
		t.Fatalf("node not dispatchable after failed update: %v", echo)
	}

	// ② 好制品：同二进制尾部加噪声——sha 必变、ELF 加载器按段偏移读不受尾部影响仍可执行。
	// 验「换刀确实发生」用 sha 而非版本号（同源二进制版本自报不变，sha 才是地面真值）。
	noise := make([]byte, 16)
	if _, err := rand.Read(noise); err != nil {
		t.Fatal(err)
	}
	padded := append(append([]byte{}, origBytes...), noise...)
	paddedSha := sha256Bytes(padded)
	h.uploadRelease(t, hostPlatform, "0.3.0-e2e", padded)

	prevConnected := h.nodeInfo(t, admin, nodeID).GetConnectedAtMs()
	resp, err = admin.SelfUpdateNode(context.Background(), connect.NewRequest(&aurav1.SelfUpdateNodeRequest{
		NodeId: nodeID, Version: "0.3.0-e2e",
	}))
	if err != nil {
		t.Fatalf("SelfUpdateNode(good): %v", err)
	}
	if !resp.Msg.GetStaged() {
		t.Fatalf("good artifact must stage, refused: %s", resp.Msg.GetMessage())
	}

	// 等 exec 自替换后的新进程重注册（ConnectedAtMs 变化 = 新会话；控制面侧信号免时钟假设）。
	deadline := time.Now().Add(60 * time.Second)
	for {
		if time.Now().After(deadline) {
			t.Fatal("node did not re-register within 60s after staged self-update")
		}
		time.Sleep(time.Second)
		n := h.nodeInfo(t, admin, nodeID)
		if n != nil && n.GetStatus() == "online" && n.GetConnectedAtMs() != prevConnected {
			break
		}
	}

	if got := sha256File(t, liveBin); got != paddedSha {
		t.Fatalf("binary on disk sha=%s, want uploaded artifact sha=%s (swap did not happen?)", got, paddedSha)
	}
	h.waitDispatchable(t, nodeID, 30*time.Second)
	echo = h.toolCall(t, nodeID, "run_command", map[string]any{"cmd": "/bin/echo", "args": []string{"updated"}})
	if ok, _ := echo["ok"].(bool); !ok {
		t.Fatalf("node not dispatchable after update: %v", echo)
	}
	t.Logf("self-update loop closed: swapped sha=%s, node re-registered and dispatchable", paddedSha[:12])
}

// copyFile 拷贝 src → dst（0755，父目录自动建），返回源内容字节。
func copyFile(t *testing.T, src, dst string) []byte {
	t.Helper()
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read %s: %v", src, err)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(dst), err)
	}
	if err := os.WriteFile(dst, b, 0o755); err != nil {
		t.Fatalf("write %s: %v", dst, err)
	}
	return b
}

// sha256File / sha256Bytes：hex 小写摘要（与 releases 登记同口径）。
func sha256File(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return sha256Bytes(b)
}

func sha256Bytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
