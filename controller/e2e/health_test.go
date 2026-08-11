//go:build e2e

// M17 场景：①能力健康巡检心跳上报（节点周期非侵入自检 → Heartbeat.health → NodeInfo.health）；
// ②self-update 启动健康回滚（pending 标记 + 试用窗内未注册 → 回滚 .old + 落盘取证）。
package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// TestCapabilityHealthReporting：起真节点 → 等首个携 health 的心跳（节点巡检首拍立即、心跳周期
// 15s）→ 断言 NodeInfo.health 覆盖三核心探针且分级在词表内。CI xvfb 桌面下 display/screenshot
// 应为 ok；无头环境 degraded 同为合法健康数据——断言词表而非具体分级（环境无关）。
func TestCapabilityHealthReporting(t *testing.T) {
	dataDir := filepath.Join(h.dataRoot, "node-health")
	token := h.generateToken(t, "", "")
	nodeID := h.enrollNode(t, dataDir, token, "")
	stop, _ := h.startNode(t, dataDir, "desktop")
	defer stop()
	h.waitNodeInFleet(t, nodeID, 30*time.Second)

	// 轮询 ListNodes 直到 health 非空（心跳 15s 周期 + 巡检首拍耗时，45s 窗）。
	admin := h.adminClient()
	deadline := time.Now().Add(45 * time.Second)
	var got map[string]string // name → status
	for time.Now().Before(deadline) {
		if n := h.nodeInfo(t, admin, nodeID); n != nil && len(n.GetHealth()) > 0 {
			got = map[string]string{}
			for _, c := range n.GetHealth() {
				got[c.GetName()] = c.GetStatus()
				if c.GetCheckedAtMs() <= 0 {
					t.Fatalf("health %s carries no checked_at_ms", c.GetName())
				}
			}
			break
		}
		time.Sleep(1 * time.Second)
	}
	if got == nil {
		t.Fatal("NodeInfo.health never populated within 45s (heartbeat should carry probe results)")
	}
	valid := map[string]bool{"ok": true, "degraded": true, "warn": true, "skipped": true}
	for _, name := range []string{"display", "screenshot", "a11y"} {
		status, present := got[name]
		if !present {
			t.Fatalf("health missing core probe %q (got %v)", name, got)
		}
		if !valid[status] {
			t.Fatalf("health %s has out-of-vocabulary status %q", name, status)
		}
	}
	t.Logf("capability health reported: %v", got)
}

// TestSelfUpdateStartupRollback：模拟「换刀后新版本起而不连」——真二进制拷贝为安装位 + `.old`，
// 铺 pending 标记，以不可达 controller 地址 + 3s 试用窗启动 → watchdog 窗满未注册 → 回滚舞步
// （install→.failed 取证、.old→install、pending 清除、rollback.json 落盘）。崩溃循环路径
// （尝试计数超限）的文件舞步同源，由 aura-node 单测覆盖。
func TestSelfUpdateStartupRollback(t *testing.T) {
	dataDir := filepath.Join(h.dataRoot, "node-rollback")
	token := h.generateToken(t, "", "")
	_ = h.enrollNode(t, dataDir, token, "") // 铺 per-node 证书（节点启动 into_config 要求文件在位）

	// 安装位与 .old 均为真二进制拷贝（回滚验证的是舞步执行，不依赖新旧内容差异；
	// 但 .failed/.old 判别需内容可辨——给 .old 尾部追加哨兵字节令两者可区分）。
	exe := filepath.Join(dataDir, "aura-node")
	copyFile(t, h.nodeBin, exe)
	oldBytes := append(copyFile(t, h.nodeBin, exe+".old"), []byte("#e2e-old-sentinel")...)
	if err := os.WriteFile(exe+".old", oldBytes, 0o755); err != nil {
		t.Fatalf("write .old sentinel: %v", err)
	}

	// pending 标记：像「刚换刀完、新二进制首次启动」。
	pending := map[string]any{
		"version": "9.9.9-e2e-bad", "prev_version": "e2e-prev", "attempts": 0,
		"staged_at_ms": time.Now().UnixMilli(),
	}
	pb, _ := json.Marshal(pending)
	if err := os.MkdirAll(filepath.Join(dataDir, "self_update"), 0o755); err != nil {
		t.Fatalf("mkdir self_update: %v", err)
	}
	pendingPath := filepath.Join(dataDir, "self_update", "pending.json")
	if err := os.WriteFile(pendingPath, pb, 0o644); err != nil {
		t.Fatalf("write pending: %v", err)
	}

	// 以不可达 controller（借空闲端口，无监听）启动：register 永败 → 试用窗（3s）满 → 回滚。
	unreachable := fmt.Sprintf("127.0.0.1:%d", freePort())
	cmd := exec.Command(exe,
		"--controller", unreachable,
		"--tls-domain", "aura-controller",
		"--ca", filepath.Join(dataDir, "ca.crt"),
		"--cert", filepath.Join(dataDir, "node.crt"),
		"--key", filepath.Join(dataDir, "node.key"),
		"--data-dir", dataDir,
		"http", "--bind", bindAddr(freePort()),
	)
	cmd.Env = append(os.Environ(), "AURA_SELF_UPDATE_TRIAL_SECS=3")
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start node with pending marker: %v", err)
	}
	defer func() {
		// exec 自替换同 PID：kill 仍指向回滚后重启的进程（回滚重启进 .old 内容、继续连不可达地址）。
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()

	// 等回滚取证记录出现（3s 试用窗 + 舞步/重启余量）。
	rollbackPath := filepath.Join(dataDir, "self_update", "rollback.json")
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(rollbackPath); err == nil {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	raw, err := os.ReadFile(rollbackPath)
	if err != nil {
		t.Fatalf("rollback.json never appeared within 30s (trial watchdog should have rolled back): %v", err)
	}
	var note struct {
		RolledBackVersion string `json:"rolled_back_version"`
		Reason            string `json:"reason"`
	}
	if err := json.Unmarshal(raw, &note); err != nil {
		t.Fatalf("parse rollback.json: %v (raw=%s)", err, raw)
	}
	if note.RolledBackVersion != "9.9.9-e2e-bad" {
		t.Fatalf("rollback.json version = %q, want 9.9.9-e2e-bad", note.RolledBackVersion)
	}

	// 文件舞步终态：pending 清除；.failed=被换下的坏二进制；install=.old 内容（哨兵尾）；.old 消失。
	if _, err := os.Stat(pendingPath); !os.IsNotExist(err) {
		t.Fatalf("pending marker should be cleared after rollback (stat err=%v)", err)
	}
	if _, err := os.Stat(exe + ".failed"); err != nil {
		t.Fatalf(".failed forensic copy missing: %v", err)
	}
	restored, err := os.ReadFile(exe)
	if err != nil {
		t.Fatalf("read restored install binary: %v", err)
	}
	if len(restored) != len(oldBytes) {
		t.Fatalf("install binary not restored from .old (len %d, want %d with sentinel)", len(restored), len(oldBytes))
	}
	if _, err := os.Stat(exe + ".old"); !os.IsNotExist(err) {
		t.Fatalf(".old should be consumed by rollback (stat err=%v)", err)
	}
	t.Logf("startup rollback verified: reason=%q, forensic .failed kept, .old restored", note.Reason)
}
