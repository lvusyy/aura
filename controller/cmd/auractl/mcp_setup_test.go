package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMcpSetupMergesNonDestructivelyAndRemoves(t *testing.T) {
	tmpHome := t.TempDir()
	tmpAppData := filepath.Join(tmpHome, "AppData", "Roaming")

	// 预先在 Cursor 配置中写入另一个已有 MCP server（验证无损合并，不覆盖用户原有配置）
	cursorPath := resolveAgentConfigPath("cursor", tmpHome, tmpAppData, "linux")
	if err := os.MkdirAll(filepath.Dir(cursorPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cursorPath, []byte(`{"mcpServers":{"playwright":{"command":"npx","args":["@playwright/mcp"]}}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	// 预先在 Codex config.toml 中写入带行内注释的其他 section（修复 Finding #7：确保 [other] # comment 不被误删）
	codexPath := resolveAgentConfigPath("codex", tmpHome, tmpAppData, "linux")
	if err := os.MkdirAll(filepath.Dir(codexPath), 0o755); err != nil {
		t.Fatal(err)
	}
	initialCodexTOML := "model = \"gpt-5.5\"\n\n[mcp_servers.aura]\nurl = \"http://old\"\n\n[profiles.work] # inline section comment\nmodel = \"o3\"\n"
	if err := os.WriteFile(codexPath, []byte(initialCodexTOML), 0o600); err != nil {
		t.Fatal(err)
	}

	opts := mcpSetupOptions{
		ServerName: "aura",
		NodeID:     "node-win11",
		Controller: "https://controller.example.com:18080",
		Token:      "secret-admin-tok",
		Insecure:   true,
		Mode:       "auto",
		Executable: "/usr/local/bin/auractl",
		HomeDir:    tmpHome,
		AppDataDir: tmpAppData,
		GOOS:       "linux",
	}

	for _, spec := range supportedAgents {
		cfgPath := resolveAgentConfigPath(spec.ID, tmpHome, tmpAppData, "linux")
		out, err := applyAgentConfigFile(spec, cfgPath, opts, false)
		if err != nil {
			t.Fatalf("applyAgentConfigFile(%s) failed: %v", spec.ID, err)
		}
		if !strings.Contains(out, "node-win11") {
			t.Fatalf("expected %s config to reference node-win11, got:\n%s", spec.ID, out)
		}
	}

	// 验证 Cursor 原有的 playwright server 完好保留，且备份文件 .bak 已生成
	cursorAfter, err := os.ReadFile(cursorPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(cursorAfter), `"playwright"`) || !strings.Contains(string(cursorAfter), `"aura"`) {
		t.Fatalf("cursor config lost existing server or missing aura:\n%s", string(cursorAfter))
	}
	if _, err := os.Stat(cursorPath + ".bak"); err != nil {
		t.Fatalf("expected backup file %s.bak to exist: %v", cursorPath, err)
	}

	// 验证 Codex TOML 中带行内注释的 [profiles.work] # inline section comment 完好保留
	codexBytes, err := os.ReadFile(codexPath)
	if err != nil {
		t.Fatal(err)
	}
	codexStr := string(codexBytes)
	if !strings.Contains(codexStr, "[mcp_servers.aura]") || !strings.Contains(codexStr, "[profiles.work] # inline section comment") || !strings.Contains(codexStr, `model = "o3"`) {
		t.Fatalf("codex config lost commented section or missing aura:\n%s", codexStr)
	}

	// 验证含特殊字符的 serverName（如 "my.aura"）在 TOML 中自动加引号且可无损移除
	codexSpec := supportedAgents[4] // codex
	dotOpts := opts
	dotOpts.ServerName = "my.aura"
	dotOut, err := applyAgentConfigFile(codexSpec, codexPath, dotOpts, false)
	if err != nil || !strings.Contains(dotOut, `[mcp_servers."my.aura"]`) {
		t.Fatalf("expected quoted TOML server name, got err=%v:\n%s", err, dotOut)
	}
	if removed, err := removeAgentConfigEntry(codexSpec, codexPath, "my.aura"); err != nil || !removed {
		t.Fatalf("expected remove my.aura to succeed, removed=%v err=%v", removed, err)
	}

	// 验证 remove 仅移除 aura 而不破坏 Cursor 里的 playwright 或 Codex 里的 [profiles.work]
	for _, spec := range supportedAgents {
		cfgPath := resolveAgentConfigPath(spec.ID, tmpHome, tmpAppData, "linux")
		removed, err := removeAgentConfigEntry(spec, cfgPath, "aura")
		if err != nil || !removed {
			t.Fatalf("removeAgentConfigEntry(%s) failed: removed=%v err=%v", spec.ID, removed, err)
		}
	}
	cursorCleaned, _ := os.ReadFile(cursorPath)
	if !strings.Contains(string(cursorCleaned), `"playwright"`) || strings.Contains(string(cursorCleaned), `"aura"`) {
		t.Fatalf("unexpected cursor config after remove:\n%s", string(cursorCleaned))
	}
	codexCleaned, _ := os.ReadFile(codexPath)
	if !strings.Contains(string(codexCleaned), "[profiles.work] # inline section comment") || strings.Contains(string(codexCleaned), "mcp_servers.aura") {
		t.Fatalf("unexpected codex config after remove:\n%s", string(codexCleaned))
	}
	// 验证含空白、单引号及多行字符串内伪造 [mcp_servers.aura] 的 Codex TOML 能被精准剥离与保留
	trickyTOML := strings.Join([]string{
		`notes = """`,
		`[mcp_servers.aura]`,
		`url = "http://inside-multiline-string-must-stay"`,
		`"""`,
		``,
		`[ mcp_servers . 'aura' ]`,
		`url = "http://old-single-quoted"`,
		``,
		`[profiles.work]`,
		`model = "o3"`,
	}, "\n")
	if err := os.WriteFile(codexPath, []byte(trickyTOML), 0o600); err != nil {
		t.Fatal(err)
	}
	mergedTricky, err := applyAgentConfigFile(codexSpec, codexPath, opts, false)
	if err != nil {
		t.Fatalf("applyAgentConfigFile on tricky TOML failed: %v", err)
	}
	if strings.Contains(mergedTricky, "http://old-single-quoted") {
		t.Fatalf("expected [ mcp_servers . 'aura' ] section to be replaced, got:\n%s", mergedTricky)
	}
	if !strings.Contains(mergedTricky, "http://inside-multiline-string-must-stay") {
		t.Fatalf("expected [mcp_servers.aura] inside multiline string to stay untouched, got:\n%s", mergedTricky)
	}
}

func TestRunMcpStdioBridgeForwardsAndHandlesErrors(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("{\n  \"jsonrpc\": \"2.0\",\n  \"id\": 1,\n  \"result\": {\"tools\": []}\n}\n"))
	}))
	defer srv.Close()

	in := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}` + "\n")
	var out bytes.Buffer
	if err := runMcpStdioBridge(context.Background(), srv.Client(), srv.URL, "tok-123", in, &out); err != nil {
		t.Fatalf("runMcpStdioBridge failed: %v", err)
	}
	if gotAuth != "Bearer tok-123" {
		t.Fatalf("expected Bearer tok-123, got %q", gotAuth)
	}
	line := strings.TrimSpace(out.String())
	if strings.Contains(line, "\n") {
		t.Fatalf("stdio bridge output must be single-line JSON, got:\n%s", line)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(line), &parsed); err != nil {
		t.Fatalf("invalid JSON from bridge: %v (%s)", err, line)
	}
}
