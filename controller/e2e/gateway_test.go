//go:build e2e

package e2e

import (
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestGatewayGating 验证 M14 网关准入门控（M16 T1.2 场景③）：坏 token→401、不存在节点→404。
// 401 在 bearer 层拒（先于节点寻址），故不必真节点；404 需 admin token 打不存在 node。
func TestGatewayGating(t *testing.T) {
	const listBody = `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`

	// 坏 token → 401（bearer 鉴权层拒绝）。
	if status, _ := h.gatewayPost(nonexistentUUID(), "bogus-token", listBody); status != http.StatusUnauthorized {
		t.Fatalf("bad token: status=%d, want 401", status)
	}

	// admin token + 不存在节点 → 404（鉴权过，节点寻址未命中）。
	if status, body := h.gatewayPost(nonexistentUUID(), h.adminToken, listBody); status != http.StatusNotFound {
		t.Fatalf("nonexistent node: status=%d body=%s, want 404", status, truncate(body, 200))
	}
}

// TestDispatchEcho 验证经网关的工具派发闭环（M16 T1.2 场景④）：run_command echo 回读 marker，
// 证明 controller→节点 反连派发链路通（无需 DISPLAY，headless 可跑）。
func TestDispatchEcho(t *testing.T) {
	dataDir := filepath.Join(h.dataRoot, "node-dispatch")
	token := h.generateToken(t, "linux", "")
	nodeID := h.enrollNode(t, dataDir, token, "linux")
	stop, _ := h.startNode(t, dataDir, "desktop")
	defer stop()
	h.waitDispatchable(t, nodeID, 30*time.Second)

	const marker = "AURA-E2E-DISPATCH-7391"
	data := h.toolCall(t, nodeID, "run_command", map[string]any{
		"cmd": "/bin/echo", "args": []string{marker},
	})
	if ok, _ := data["ok"].(bool); !ok {
		t.Fatalf("run_command envelope not ok: %v", data)
	}
	inner, _ := data["data"].(map[string]any)
	stdout, _ := inner["stdout"].(string)
	if !strings.Contains(stdout, marker) {
		t.Fatalf("echo stdout = %q, want to contain %q", stdout, marker)
	}
	t.Logf("dispatch echo round-trip ok: %q", stdout)
}
