//go:build e2e

package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestUILoop 验证键鼠注入真正落在 UI 上（M16 T1.2 场景⑤，端到端最强证据）：detach 起 xterm →
// 截图确认窗口 → click 窗口内 → type 命令 + 回车 → run_command 验证文件内容。文件只能由 xterm 里
// 敲的命令产生，杜绝 shell 侧作弊。需 DISPLAY + xterm（CI 经 xvfb+openbox 提供），缺失则 skip。
func TestUILoop(t *testing.T) {
	if os.Getenv("DISPLAY") == "" {
		t.Skip("no DISPLAY; UI loop needs xvfb+openbox+xterm (runs in CI e2e job)")
	}
	if _, err := exec.LookPath("xterm"); err != nil {
		t.Skip("xterm not found; UI loop scenario skipped")
	}

	dataDir := filepath.Join(h.dataRoot, "node-ui")
	token := h.generateToken(t, "linux", "")
	nodeID := h.enrollNode(t, dataDir, token, "linux")
	stop, _ := h.startNode(t, dataDir, "desktop")
	defer stop()
	h.waitDispatchable(t, nodeID, 30*time.Second)

	markFile := filepath.Join(t.TempDir(), "aura-mark.txt")
	const marker = "AURA-MARK-7391"

	// ① detach 起 xterm（绝对路径，几何固定左上）——detach=true 令 run_command 仅启动不等待
	//    （否则 30s 超时经 kill_on_drop 连窗口一起杀，UI 闭环无法开场）。
	launch := h.toolCall(t, nodeID, "run_command", map[string]any{
		"cmd": "/usr/bin/xterm", "args": []string{"-geometry", "100x30+50+50"}, "detach": true,
	})
	if ok, _ := launch["ok"].(bool); !ok {
		t.Fatalf("xterm launch envelope not ok: %v", launch)
	}

	// ② 等窗口起，截图确认（screenshot 返回带 scale meta 的 display 空间图）。
	time.Sleep(1500 * time.Millisecond)
	shot := h.toolCall(t, nodeID, "screenshot", nil)
	inner, _ := shot["data"].(map[string]any)
	if img, _ := inner["image_base64"].(string); len(img) == 0 {
		t.Fatalf("screenshot returned empty image")
	}

	// ③ click 窗口内部（display 空间坐标；节点按 scale 自动回映射到原生像素，无需手动乘 scale）。
	//    xterm 左上 +50+50、约 600x390，取窗口体中部一点。
	h.toolCall(t, nodeID, "click", map[string]any{"coordinate": []int{240, 170}})
	time.Sleep(400 * time.Millisecond)

	// ④ type 写文件命令 + 回车（键鼠注入落在 xterm 焦点上）。
	h.toolCall(t, nodeID, "type", map[string]any{"text": "echo " + marker + " > " + markFile})
	time.Sleep(300 * time.Millisecond)
	h.toolCall(t, nodeID, "key", map[string]any{"keys": "enter"})
	time.Sleep(800 * time.Millisecond)

	// ⑤ run_command 验证文件内容——只能由 xterm 里敲的命令产生（键鼠真落 UI 的铁证）。
	data := h.toolCall(t, nodeID, "run_command", map[string]any{"cmd": "/bin/cat", "args": []string{markFile}})
	catInner, _ := data["data"].(map[string]any)
	stdout, _ := catInner["stdout"].(string)
	if !strings.Contains(stdout, marker) {
		t.Fatalf("mark file content = %q, want to contain %q (键鼠未落在 UI 上?)", stdout, marker)
	}
	t.Logf("UI loop closed: mark file carries %q", marker)

	// 清理 xterm（best-effort）。
	h.toolCall(t, nodeID, "run_command", map[string]any{"cmd": "/usr/bin/pkill", "args": []string{"-x", "xterm"}})
}
