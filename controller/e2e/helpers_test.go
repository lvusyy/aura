//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	aurav1 "github.com/aura/controller/gen/aura/v1"
)

// gatewayPost 向 M14 网关 POST 一段 JSON-RPC（agent 单一入口 /v1/mcp/<node_id>），返回状态码 + 响应体。
// 直用 http（非 connect 客户端）——网关是原生 JSON-RPC 代理面，与 codex 走同一通道。
func (h *harness) gatewayPost(nodeID, token, body string) (int, string) {
	req, err := http.NewRequest(http.MethodPost, h.restBase+"/v1/mcp/"+nodeID, strings.NewReader(body))
	if err != nil {
		return 0, err.Error()
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := h.httpClient.Do(req)
	if err != nil {
		return 0, err.Error()
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// rpcCounter 递增 JSON-RPC id（进程内唯一即可，e2e 顺序调用）。
var rpcCounter int

// toolCall 经网关调一个 MCP 工具，返回工具信封的 data 字段（Envelope{ok,data}）。
// 处理网关的双形态响应体：Streamable HTTP 的 SSE（data: 前缀行）或 JSON 直返。
func (h *harness) toolCall(t *testing.T, nodeID, name string, args map[string]any) map[string]any {
	t.Helper()
	rpcCounter++
	reqBody, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": rpcCounter, "method": "tools/call",
		"params": map[string]any{"name": name, "arguments": args},
	})
	status, raw := h.gatewayPost(nodeID, h.adminToken, string(reqBody))
	if status != http.StatusOK {
		t.Fatalf("tools/call %s: status=%d body=%s", name, status, truncate(raw, 300))
	}
	envelope := extractToolData(t, raw)
	return envelope
}

// extractToolData 从网关响应（SSE 或 JSON）解出 result.structuredContent——即工具信封 Envelope{ok,data}
// 整体（structuredContent 本身就是信封，非再套一层 data；调用方读 ["ok"] 与 ["data"]["<字段>"]）。
func extractToolData(t *testing.T, raw string) map[string]any {
	t.Helper()
	payload := raw
	// SSE：取最后一个 data: 行为 JSON-RPC 帧。
	if strings.Contains(raw, "data:") {
		var last string
		for _, line := range strings.Split(raw, "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "data:") {
				last = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			}
		}
		if last != "" {
			payload = last
		}
	}
	var rpc struct {
		Result struct {
			StructuredContent map[string]any `json:"structuredContent"`
		} `json:"result"`
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal([]byte(payload), &rpc); err != nil {
		t.Fatalf("parse rpc result: %v (payload=%s)", err, truncate(payload, 300))
	}
	if len(rpc.Error) > 0 {
		t.Fatalf("rpc error: %s", string(rpc.Error))
	}
	return rpc.Result.StructuredContent
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// nonexistentUUID 是一个合法但不在册的 node UUID（网关 404 用例）。固定串即可。
func nonexistentUUID() string { return "00000000-0000-4000-8000-000000000000" }

// generateToken 经 admin ConsoleService 签发一次性 enroll token；project 空=不归属（M15）。
func (h *harness) generateToken(t *testing.T, platform, project string) string {
	t.Helper()
	resp, err := h.consoleClient().GenerateEnrollToken(context.Background(), connect.NewRequest(&aurav1.GenerateEnrollTokenRequest{
		PlatformScope: platform,
		TtlSecs:       600,
		Uses:          1,
		Label:         "e2e",
		Who:           "e2e",
		Project:       project,
	}))
	if err != nil {
		t.Fatalf("GenerateEnrollToken: %v", err)
	}
	return resp.Msg.Token
}

// enrollNode 跑一次性 `aura-node enroll` 换 per-node 证书，返回分配的 node_id（落 <dataDir>/node_id）。
func (h *harness) enrollNode(t *testing.T, dataDir, token, platform string) string {
	t.Helper()
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatalf("mkdir dataDir: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, h.nodeBin, "enroll",
		"--controller", h.restAddr,
		"--token", token,
		"--platform", platform,
		"--ca", filepath.Join(h.certDir, "ca.crt"),
		"--data-dir", dataDir,
	)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("aura-node enroll: %v", err)
	}
	idBytes, err := os.ReadFile(filepath.Join(dataDir, "node_id"))
	if err != nil {
		t.Fatalf("read node_id after enroll: %v", err)
	}
	return string(trimSpace(idBytes))
}

// startNode 起长驻反连节点（mTLS 反连 grpcAddr + 本地 MCP /mcp）；返回 stop 优雅关停 + MCP bind 地址。
func (h *harness) startNode(t *testing.T, dataDir, driver string) (stop func(), mcpBind string) {
	t.Helper()
	mcpPort := freePort()
	mcpBind = bindAddr(mcpPort)
	cmd := exec.Command(h.nodeBin,
		"--driver", driver,
		"--controller", h.grpcAddr,
		"--tls-domain", "aura-controller",
		"--ca", filepath.Join(dataDir, "ca.crt"),
		"--cert", filepath.Join(dataDir, "node.crt"),
		"--key", filepath.Join(dataDir, "node.key"),
		"--data-dir", dataDir,
		"http", "--bind", mcpBind,
	)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start aura-node: %v", err)
	}
	stop = func() {
		_ = cmd.Process.Signal(sigterm())
		done := make(chan struct{})
		go func() { _, _ = cmd.Process.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(8 * time.Second):
			_ = cmd.Process.Kill()
		}
	}
	return stop, mcpBind
}

// waitNodeInFleet 反复开 WatchFleet 读首帧快照，直到 nodeID 现身或超时（反连注册有延迟）。
func (h *harness) waitNodeInFleet(t *testing.T, nodeID string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if h.fleetHas(nodeID) {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("node %s did not appear in fleet within %s", nodeID, timeout)
}

// waitDispatchable 轮询网关 tools/list 直到返回 200（节点反连流进 scheduler 派发表就绪）或超时。
// 比 waitNodeInFleet 更强：fleet 可见（registry 有会话）与「可派发」（反连流就绪）之间有微竞态窗口，
// 依赖派发的场景（dispatch/UI）须等到网关真能打通节点，而非仅现身舰队。
func (h *harness) waitDispatchable(t *testing.T, nodeID string, timeout time.Duration) {
	t.Helper()
	const listBody = `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if status, _ := h.gatewayPost(nodeID, h.adminToken, listBody); status == 200 {
			return
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatalf("node %s not dispatchable via gateway within %s", nodeID, timeout)
}

// fleetHas 开一次 WatchFleet，读首帧快照判断 nodeID 是否在册（首帧 type=HEARTBEAT_SNAPSHOT，snapshot 全量）。
func (h *harness) fleetHas(nodeID string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := h.consoleClient().WatchFleet(ctx, connect.NewRequest(&aurav1.WatchFleetRequest{}))
	if err != nil {
		return false
	}
	defer stream.Close()
	if !stream.Receive() {
		return false
	}
	ev := stream.Msg()
	for _, n := range ev.Snapshot {
		if n.NodeId == nodeID {
			return true
		}
	}
	// 首帧后紧随的增量帧兜底（快照与增量竞态时目标可能在下一帧的 node 字段）。
	if ev.Node != nil && ev.Node.NodeId == nodeID {
		return true
	}
	return false
}

// trimSpace 去首尾空白（node_id 文件可能带换行）。避免引入 strings 仅为一次 TrimSpace。
func trimSpace(b []byte) []byte {
	start, end := 0, len(b)
	for start < end && isSpace(b[start]) {
		start++
	}
	for end > start && isSpace(b[end-1]) {
		end--
	}
	return b[start:end]
}

func isSpace(c byte) bool { return c == ' ' || c == '\n' || c == '\r' || c == '\t' }
