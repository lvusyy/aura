package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"text/tabwriter"

	"connectrpc.com/connect"

	aurav1 "github.com/aura/controller/gen/aura/v1"
	aurav1connect "github.com/aura/controller/gen/aura/v1/aurav1connect"
)

// mcpSetupOptions 描述 `auractl mcp setup` 的配置参数。
type mcpSetupOptions struct {
	ServerName string // MCP server 注册名（默认 "aura"）
	NodeID     string // 目标节点 ID（经控制面网关 /v1/mcp/<node_id>）
	DirectURL  string // 可选：直连节点 URL（如 http://192.168.1.10:7100/mcp），非空时优先于控制面网关
	Controller string // 控制面基础 URL（如 https://localhost:18080）
	Token      string // Bearer 令牌
	Insecure   bool   // 是否跳过 TLS 证书校验（自签名开发证书）
	CACert     string // 自定义 CA 证书路径
	Mode       string // "auto" | "http" | "bridge"
	Executable string // auractl 自身可执行文件路径（bridge 模式写入 command）
	HomeDir    string // 用户主目录（单测可注入临时目录）
	AppDataDir string // Windows %APPDATA% 目录（单测可注入临时目录）
	GOOS       string // 目标操作系统标识（默认 runtime.GOOS）
}

// agentSpec 定义单个主流 Coding Agent 的配置文件路径规则与格式生成逻辑。
type agentSpec struct {
	ID          string
	DisplayName string
	Format      string // "json-mcpServers" | "json-opencode" | "json-openclaw" | "toml-codex"
	HTTPTypeKey string // JSON 格式下 HTTP 传输类型字段名："type" | "transport" | ""
	HTTPTypeVal string // JSON 格式下 HTTP 传输类型值："http" | "streamableHttp" | "remote" | "streamable-http"
	URLKey      string // URL 字段名：默认 "url"（Windsurf 为 "serverUrl"）
	StdioOnly   bool   // 是否仅原生支持 stdio（如 Claude Desktop）
}

var supportedAgents = []agentSpec{
	{
		ID:          "claude-code",
		DisplayName: "Claude Code",
		Format:      "json-mcpServers",
		HTTPTypeKey: "type",
		HTTPTypeVal: "http",
		URLKey:      "url",
	},
	{
		ID:          "claude-desktop",
		DisplayName: "Claude Desktop",
		Format:      "json-mcpServers",
		StdioOnly:   true,
	},
	{
		ID:          "cursor",
		DisplayName: "Cursor",
		Format:      "json-mcpServers",
		URLKey:      "url",
	},
	{
		ID:          "windsurf",
		DisplayName: "Windsurf",
		Format:      "json-mcpServers",
		URLKey:      "serverUrl",
	},
	{
		ID:          "codex",
		DisplayName: "OpenAI Codex CLI",
		Format:      "toml-codex",
	},
	{
		ID:          "gemini",
		DisplayName: "Gemini CLI",
		Format:      "json-mcpServers",
		HTTPTypeKey: "type",
		HTTPTypeVal: "http",
		URLKey:      "url",
	},
	{
		ID:          "cline",
		DisplayName: "Cline (VS Code)",
		Format:      "json-mcpServers",
		HTTPTypeKey: "type",
		HTTPTypeVal: "streamableHttp",
		URLKey:      "url",
	},
	{
		ID:          "opencode",
		DisplayName: "OpenCode",
		Format:      "json-opencode",
		HTTPTypeKey: "type",
		HTTPTypeVal: "remote",
		URLKey:      "url",
	},
	{
		ID:          "openclaw",
		DisplayName: "OpenClaw",
		Format:      "json-openclaw",
		HTTPTypeKey: "transport",
		HTTPTypeVal: "streamable-http",
		URLKey:      "url",
	},
}

// cmdMcp 处理 `auractl mcp setup|bridge|remove|list-clients`。
func cmdMcp(ctx context.Context, client aurav1connect.ControllerAdminClient, rest restConfig, insecure bool, cacert string, args []string) error {
	if len(args) < 1 {
		return errors.New("usage: auractl mcp <setup|bridge|remove|list-clients> [args]")
	}
	switch args[0] {
	case "bridge":
		return cmdMcpBridge(ctx, rest, args[1:])
	case "setup", "install":
		return cmdMcpSetup(ctx, client, rest, insecure, cacert, args[1:])
	case "remove", "uninstall":
		return cmdMcpRemove(args[1:])
	case "list-clients", "clients":
		return cmdMcpListClients()
	default:
		return fmt.Errorf("unknown mcp subcommand %q (want setup|bridge|remove|list-clients)", args[0])
	}
}

// cmdMcpBridge 实现内置 `stdio` <-> Streamable HTTP MCP 桥接器：
// 从 stdin 逐行读取 MCP JSON-RPC 请求，附带控制面 TLS 配置与 Bearer Token 转发至 `/v1/mcp/<node-id>`（或 --url 直连端点），
// 并将响应原样单行写回 stdout。使 Claude Desktop 等仅支持 stdio 的客户端或自签名证书环境实现零依赖接入。
func cmdMcpBridge(ctx context.Context, rest restConfig, args []string) error {
	fs := flag.NewFlagSet("mcp bridge", flag.ContinueOnError)
	directURL := fs.String("url", "", "direct MCP HTTP URL (overrides controller gateway /v1/mcp/<node-id>)")
	var positional []string
	var flagArgs []string
	for i := 0; i < len(args); i++ {
		if strings.HasPrefix(args[i], "-") {
			flagArgs = append(flagArgs, args[i:]...)
			break
		}
		positional = append(positional, args[i])
	}
	if err := fs.Parse(flagArgs); err != nil {
		return err
	}
	positional = append(positional, fs.Args()...)

	targetURL := strings.TrimSpace(*directURL)
	if targetURL == "" {
		if len(positional) < 1 || strings.TrimSpace(positional[0]) == "" {
			return errors.New("usage: auractl mcp bridge <node-id> [--url HTTP_MCP_URL]")
		}
		base := strings.TrimRight(rest.server, "/")
		targetURL = base + "/v1/mcp/" + strings.TrimSpace(positional[0])
	}

	return runMcpStdioBridge(ctx, rest.httpClient, targetURL, rest.token, os.Stdin, os.Stdout)
}

func runMcpStdioBridge(ctx context.Context, httpClient *http.Client, targetURL, token string, in io.Reader, out io.Writer) error {
	scanner := bufio.NewScanner(in)
	// 放宽单行上限至 16MB 以容纳大 payload 请求
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	bw := bufio.NewWriter(out)
	defer bw.Flush()

	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(line))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("User-Agent", "auractl-mcp-bridge/0.4.2")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}

		resp, err := httpClient.Do(req)
		if err != nil {
			// 若请求含 JSON-RPC id，合成标准 JSON-RPC 错误回包以免客户端挂死
			if errLine := synthesizeBridgeJSONRPCError(line, -32000, "MCP bridge transport error: "+err.Error()); len(errLine) > 0 {
				_, _ = bw.Write(errLine)
				_ = bw.WriteByte('\n')
				_ = bw.Flush()
			}
			continue
		}
		respBytes, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()

		trimmedResp := bytes.TrimSpace(respBytes)
		if len(trimmedResp) == 0 {
			// notification 等 202/204 空响应无需向 stdout 输出行
			continue
		}
		if resp.StatusCode >= 400 && !json.Valid(trimmedResp) {
			if errLine := synthesizeBridgeJSONRPCError(line, -32001, fmt.Sprintf("MCP gateway HTTP %d: %s", resp.StatusCode, string(trimmedResp))); len(errLine) > 0 {
				_, _ = bw.Write(errLine)
				_ = bw.WriteByte('\n')
				_ = bw.Flush()
			}
			continue
		}
		// 压缩为单行 JSON（stdio MCP 规范要求每条消息占单行）
		var compact bytes.Buffer
		if json.Compact(&compact, trimmedResp) == nil {
			_, _ = bw.Write(compact.Bytes())
		} else {
			_, _ = bw.Write(bytes.ReplaceAll(trimmedResp, []byte("\n"), []byte(" ")))
		}
		_ = bw.WriteByte('\n')
		_ = bw.Flush()
	}
	return scanner.Err()
}

func synthesizeBridgeJSONRPCError(reqLine []byte, code int, msg string) []byte {
	var head struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
	}
	if json.Unmarshal(reqLine, &head) != nil || len(head.ID) == 0 || string(head.ID) == "null" {
		return nil
	}
	out, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      head.ID,
		"error": map[string]any{
			"code":    code,
			"message": msg,
		},
	})
	return out
}

func cmdMcpListClients() error {
	home, _ := os.UserHomeDir()
	appData := os.Getenv("APPDATA")
	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "CLIENT_ID\tNAME\tDETECTED\tCONFIG_PATH")
	for _, spec := range supportedAgents {
		cfgPath := resolveAgentConfigPath(spec.ID, home, appData, runtime.GOOS)
		detected := isAgentDetected(spec.ID, cfgPath)
		fmt.Fprintf(w, "%s\t%s\t%t\t%s\n", spec.ID, spec.DisplayName, detected, cfgPath)
	}
	return w.Flush()
}

func cmdMcpSetup(
	ctx context.Context,
	client aurav1connect.ControllerAdminClient,
	rest restConfig,
	insecure bool,
	cacert string,
	args []string,
) error {
	fs := flag.NewFlagSet("mcp setup", flag.ContinueOnError)
	clientFlag := fs.String("client", "auto", "target agent(s): auto|all|claude-code,claude-desktop,cursor,windsurf,codex,gemini,cline,opencode,openclaw")
	modeFlag := fs.String("mode", "auto", "connection mode: auto|http|bridge (bridge uses 'auractl mcp bridge' over stdio)")
	nameFlag := fs.String("name", "aura", "MCP server name in agent config")
	urlFlag := fs.String("url", "", "optional direct MCP URL (e.g. http://192.168.1.10:7100/mcp); default uses controller /v1/mcp/<node-id>")
	nodeFlag := fs.String("node", "", "target node ID (if omitted, positional arg or first online node is used)")
	dryRun := fs.Bool("dry-run", false, "print generated config changes without writing files")

	var positional []string
	var flagArgs []string
	for i := 0; i < len(args); i++ {
		if strings.HasPrefix(args[i], "-") {
			flagArgs = append(flagArgs, args[i:]...)
			break
		}
		positional = append(positional, args[i])
	}
	if err := fs.Parse(flagArgs); err != nil {
		return err
	}
	positional = append(positional, fs.Args()...)

	nodeID := strings.TrimSpace(*nodeFlag)
	if nodeID == "" && len(positional) > 0 {
		nodeID = strings.TrimSpace(positional[0])
	}
	directURL := strings.TrimSpace(*urlFlag)

	// 若未传 --url 且未指定 nodeID，自动从控制面查询首个在线节点（失败则退回任意已注册节点）
	if directURL == "" && nodeID == "" {
		resp, err := client.ListNodes(ctx, connect.NewRequest(&aurav1.ListNodesRequest{}))
		if err != nil {
			return fmt.Errorf("node_id not specified and ListNodes failed: %w (pass <node-id> or --url)", err)
		}
		nodes := resp.Msg.GetNodes()
		if len(nodes) == 0 {
			return errors.New("no registered nodes found on controller; pass <node-id> or --url explicitly")
		}
		for _, n := range nodes {
			if n.GetStatus() == "online" {
				nodeID = n.GetNodeId()
				break
			}
		}
		if nodeID == "" {
			nodeID = nodes[0].GetNodeId()
		}
	}

	exePath, err := os.Executable()
	if err != nil || exePath == "" {
		exePath = "auractl"
	}
	home, _ := os.UserHomeDir()
	appData := os.Getenv("APPDATA")

	opts := mcpSetupOptions{
		ServerName: strings.TrimSpace(*nameFlag),
		NodeID:     nodeID,
		DirectURL:  directURL,
		Controller: rest.server,
		Token:      rest.token,
		Insecure:   insecure,
		CACert:     cacert,
		Mode:       strings.ToLower(strings.TrimSpace(*modeFlag)),
		Executable: exePath,
		HomeDir:    home,
		AppDataDir: appData,
		GOOS:       runtime.GOOS,
	}

	targets, err := selectTargetAgents(*clientFlag, opts.HomeDir, opts.AppDataDir, opts.GOOS)
	if err != nil {
		return err
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "AGENT\tMODE\tSTATUS\tCONFIG_PATH")
	for _, spec := range targets {
		cfgPath := resolveAgentConfigPath(spec.ID, opts.HomeDir, opts.AppDataDir, opts.GOOS)
		effectiveMode := resolveEffectiveMode(spec, opts)
		updatedContent, err := applyAgentConfigFile(spec, cfgPath, opts, *dryRun)
		if err != nil {
			fmt.Fprintf(w, "%s\t%s\tERROR (%v)\t%s\n", spec.ID, effectiveMode, err, cfgPath)
			continue
		}
		status := "CONFIGURED"
		if *dryRun {
			status = "DRY-RUN"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", spec.ID, effectiveMode, status, cfgPath)
		if *dryRun {
			_ = w.Flush()
			fmt.Printf("\n--- [%s] %s ---\n%s\n", spec.ID, cfgPath, updatedContent)
		}
	}
	return w.Flush()
}

func cmdMcpRemove(args []string) error {
	fs := flag.NewFlagSet("mcp remove", flag.ContinueOnError)
	clientFlag := fs.String("client", "all", "target agent(s): all|claude-code,claude-desktop,cursor,...")
	nameFlag := fs.String("name", "aura", "MCP server name to remove")
	if err := fs.Parse(args); err != nil {
		return err
	}
	home, _ := os.UserHomeDir()
	appData := os.Getenv("APPDATA")
	targets, err := selectTargetAgents(*clientFlag, home, appData, runtime.GOOS)
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "AGENT\tSTATUS\tCONFIG_PATH")
	for _, spec := range targets {
		cfgPath := resolveAgentConfigPath(spec.ID, home, appData, runtime.GOOS)
		removed, err := removeAgentConfigEntry(spec, cfgPath, strings.TrimSpace(*nameFlag))
		if err != nil {
			fmt.Fprintf(w, "%s\tERROR (%v)\t%s\n", spec.ID, err, cfgPath)
			continue
		}
		status := "SKIPPED (not present)"
		if removed {
			status = "REMOVED"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\n", spec.ID, status, cfgPath)
	}
	return w.Flush()
}

func selectTargetAgents(clientFlag, homeDir, appDataDir, goos string) ([]agentSpec, error) {
	specMap := make(map[string]agentSpec, len(supportedAgents))
	for _, s := range supportedAgents {
		specMap[s.ID] = s
	}
	flagVal := strings.ToLower(strings.TrimSpace(clientFlag))
	if flagVal == "all" {
		return supportedAgents, nil
	}
	if flagVal == "" || flagVal == "auto" {
		var detected []agentSpec
		for _, s := range supportedAgents {
			p := resolveAgentConfigPath(s.ID, homeDir, appDataDir, goos)
			if isAgentDetected(s.ID, p) {
				detected = append(detected, s)
			}
		}
		// 若环境尚未初始化任何 Agent 目录，默认配置最常用的三大客户端（claude-code, cursor, codex）
		if len(detected) == 0 {
			return []agentSpec{specMap["claude-code"], specMap["cursor"], specMap["codex"]}, nil
		}
		return detected, nil
	}
	var out []agentSpec
	for _, part := range strings.Split(flagVal, ",") {
		id := strings.TrimSpace(part)
		if id == "" {
			continue
		}
		s, ok := specMap[id]
		if !ok {
			return nil, fmt.Errorf("unknown agent %q (run 'auractl mcp list-clients' to see supported agents)", id)
		}
		out = append(out, s)
	}
	if len(out) == 0 {
		return nil, errors.New("no target agents selected")
	}
	return out, nil
}

func isAgentDetected(agentID, cfgPath string) bool {
	if _, err := os.Stat(cfgPath); err == nil {
		return true
	}
	parent := filepath.Dir(cfgPath)
	if _, err := os.Stat(parent); err == nil && parent != "." && parent != "/" {
		// 对于 ~/.claude.json，其父目录是 $HOME，需额外检查 CLI 或 ~/.claude 目录
		if agentID == "claude-code" {
			if _, derr := os.Stat(filepath.Join(parent, ".claude")); derr == nil {
				return true
			}
		} else {
			return true
		}
	}
	binMap := map[string]string{
		"claude-code": "claude",
		"cursor":      "cursor",
		"windsurf":    "windsurf",
		"codex":       "codex",
		"gemini":      "gemini",
		"opencode":    "opencode",
		"openclaw":    "openclaw",
	}
	if bin, ok := binMap[agentID]; ok {
		if _, err := exec.LookPath(bin); err == nil {
			return true
		}
	}
	return false
}

func resolveAgentConfigPath(agentID, homeDir, appDataDir, goos string) string {
	if appDataDir == "" && goos == "windows" && homeDir != "" {
		appDataDir = filepath.Join(homeDir, "AppData", "Roaming")
	}
	switch agentID {
	case "claude-code":
		return filepath.Join(homeDir, ".claude.json")
	case "claude-desktop":
		switch goos {
		case "windows":
			return filepath.Join(appDataDir, "Claude", "claude_desktop_config.json")
		case "darwin":
			return filepath.Join(homeDir, "Library", "Application Support", "Claude", "claude_desktop_config.json")
		default:
			return filepath.Join(homeDir, ".config", "Claude", "claude_desktop_config.json")
		}
	case "cursor":
		return filepath.Join(homeDir, ".cursor", "mcp.json")
	case "windsurf":
		return filepath.Join(homeDir, ".codeium", "windsurf", "mcp_config.json")
	case "codex":
		return filepath.Join(homeDir, ".codex", "config.toml")
	case "gemini":
		return filepath.Join(homeDir, ".gemini", "settings.json")
	case "cline":
		switch goos {
		case "windows":
			return filepath.Join(appDataDir, "Code", "User", "globalStorage", "saoudrizwan.claude-dev", "settings", "cline_mcp_settings.json")
		case "darwin":
			return filepath.Join(homeDir, "Library", "Application Support", "Code", "User", "globalStorage", "saoudrizwan.claude-dev", "settings", "cline_mcp_settings.json")
		default:
			return filepath.Join(homeDir, ".config", "Code", "User", "globalStorage", "saoudrizwan.claude-dev", "settings", "cline_mcp_settings.json")
		}
	case "opencode":
		return filepath.Join(homeDir, ".config", "opencode", "opencode.json")
	case "openclaw":
		return filepath.Join(homeDir, ".openclaw", "openclaw.json")
	default:
		return filepath.Join(homeDir, "."+agentID, "mcp.json")
	}
}

func resolveEffectiveMode(spec agentSpec, opts mcpSetupOptions) string {
	if spec.StdioOnly {
		return "bridge"
	}
	switch opts.Mode {
	case "http", "bridge":
		return opts.Mode
	default:
		// 当使用自签名证书（--insecure 或 --cacert）访问 https 控制面时，外部 Agent 原生 HTTP 客户端通常会因 TLS 校验失败拒绝连接，
		// 自动启用 auractl mcp bridge (stdio) 即可透明承载 TLS 与鉴权！
		if opts.DirectURL == "" && strings.HasPrefix(strings.ToLower(opts.Controller), "https://") && (opts.Insecure || opts.CACert != "") {
			return "bridge"
		}
		return "http"
	}
}

func (o mcpSetupOptions) resolvedHTTPURL() string {
	if o.DirectURL != "" {
		return o.DirectURL
	}
	return strings.TrimRight(o.Controller, "/") + "/v1/mcp/" + o.NodeID
}

func (o mcpSetupOptions) bridgeCommandAndArgs() (string, []string, map[string]string) {
	cmd := o.Executable
	if cmd == "" {
		cmd = "auractl"
	}
	var args []string
	if o.DirectURL == "" && o.Controller != "" {
		args = append(args, "--server", o.Controller)
	}
	if o.Insecure {
		args = append(args, "--insecure")
	}
	if o.CACert != "" {
		args = append(args, "--cacert", o.CACert)
	}
	args = append(args, "mcp", "bridge")
	if o.DirectURL != "" {
		args = append(args, "--url", o.DirectURL)
	} else {
		args = append(args, o.NodeID)
	}
	env := make(map[string]string)
	if o.Token != "" {
		env["AURACTL_TOKEN"] = o.Token
	}
	return cmd, args, env
}

// applyAgentConfigFile 无损合并 AURA MCP 条目至目标 Agent 配置文件（带备份校验与同目录临时文件原子替换）。
func applyAgentConfigFile(spec agentSpec, cfgPath string, opts mcpSetupOptions, dryRun bool) (string, error) {
	serverName := opts.ServerName
	if serverName == "" {
		serverName = "aura"
	}
	mode := resolveEffectiveMode(spec, opts)

	var existingBytes []byte
	if b, err := os.ReadFile(cfgPath); err == nil {
		existingBytes = b
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("read existing config %q: %w", cfgPath, err)
	}

	var rendered string
	var err error
	if spec.Format == "toml-codex" {
		rendered, err = mergeCodexTOML(string(existingBytes), serverName, mode, opts)
	} else {
		rendered, err = mergeAgentJSON(existingBytes, spec, serverName, mode, opts)
	}
	if err != nil {
		return "", err
	}

	if !dryRun {
		if err := os.MkdirAll(filepath.Dir(cfgPath), 0o755); err != nil {
			return "", err
		}
		if len(existingBytes) > 0 && string(existingBytes) != rendered {
			if err := atomicWriteFile(cfgPath+".bak", existingBytes, 0o600); err != nil {
				return "", fmt.Errorf("backup existing config %q: %w", cfgPath+".bak", err)
			}
		}
		if err := atomicWriteFile(cfgPath, []byte(rendered), 0o600); err != nil {
			return "", err
		}
	}
	return rendered, nil
}

func atomicWriteFile(targetPath string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(targetPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmpFile, err := os.CreateTemp(dir, ".auractl-cfg-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmpFile.Name()
	cleanup := func() {
		_ = os.Remove(tmpPath)
	}
	if _, err := tmpFile.Write(data); err != nil {
		_ = tmpFile.Close()
		cleanup()
		return err
	}
	if err := tmpFile.Chmod(perm); err != nil {
		_ = tmpFile.Close()
		cleanup()
		return err
	}
	if err := tmpFile.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(tmpPath, targetPath); err != nil {
		cleanup()
		return err
	}
	return nil
}

func mergeAgentJSON(existing []byte, spec agentSpec, serverName, mode string, opts mcpSetupOptions) (string, error) {
	root := make(map[string]any)
	if len(bytes.TrimSpace(existing)) > 0 {
		if err := json.Unmarshal(existing, &root); err != nil {
			return "", fmt.Errorf("parse existing JSON: %w", err)
		}
	}

	entry := make(map[string]any)
	if mode == "bridge" {
		cmd, args, env := opts.bridgeCommandAndArgs()
		entry["command"] = cmd
		entry["args"] = args
		if len(env) > 0 {
			entry["env"] = env
		}
		if spec.Format == "json-opencode" {
			entry["type"] = "local"
			cmdSlice := append([]string{cmd}, args...)
			entry["command"] = cmdSlice
			delete(entry, "args")
		}
	} else {
		urlKey := spec.URLKey
		if urlKey == "" {
			urlKey = "url"
		}
		if spec.HTTPTypeKey != "" && spec.HTTPTypeVal != "" {
			entry[spec.HTTPTypeKey] = spec.HTTPTypeVal
		}
		entry[urlKey] = opts.resolvedHTTPURL()
		if opts.Token != "" {
			entry["headers"] = map[string]string{
				"Authorization": "Bearer " + opts.Token,
			}
		}
	}

	switch spec.Format {
	case "json-opencode":
		mcpObj, _ := root["mcp"].(map[string]any)
		if mcpObj == nil {
			mcpObj = make(map[string]any)
		}
		mcpObj[serverName] = entry
		root["mcp"] = mcpObj
	case "json-openclaw":
		mcpObj, _ := root["mcp"].(map[string]any)
		if mcpObj == nil {
			mcpObj = make(map[string]any)
		}
		srvObj, _ := mcpObj["servers"].(map[string]any)
		if srvObj == nil {
			srvObj = make(map[string]any)
		}
		srvObj[serverName] = entry
		mcpObj["servers"] = srvObj
		root["mcp"] = mcpObj
	default:
		srvObj, _ := root["mcpServers"].(map[string]any)
		if srvObj == nil {
			srvObj = make(map[string]any)
		}
		srvObj[serverName] = entry
		root["mcpServers"] = srvObj
	}

	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return "", err
	}
	return string(out) + "\n", nil
}

func formatTOMLServerKey(serverName string) string {
	for i := 0; i < len(serverName); i++ {
		c := serverName[i]
		isBare := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' || c == '-'
		if !isBare {
			b, _ := json.Marshal(serverName)
			return string(b)
		}
	}
	if serverName == "" {
		return `"aura"`
	}
	return serverName
}

func mergeCodexTOML(existing, serverName, mode string, opts mcpSetupOptions) (string, error) {
	cleaned := stripCodexTOMLSection(existing, serverName)
	var b strings.Builder
	if strings.TrimSpace(cleaned) != "" {
		b.WriteString(strings.TrimRight(cleaned, "\r\n"))
		b.WriteString("\n\n")
	}
	fmt.Fprintf(&b, "[mcp_servers.%s]\n", formatTOMLServerKey(serverName))
	if mode == "bridge" {
		cmd, args, env := opts.bridgeCommandAndArgs()
		cmdJSON, _ := json.Marshal(cmd)
		argsJSON, _ := json.Marshal(args)
		fmt.Fprintf(&b, "command = %s\n", string(cmdJSON))
		fmt.Fprintf(&b, "args = %s\n", string(argsJSON))
		if len(env) > 0 {
			keys := make([]string, 0, len(env))
			for k := range env {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			var pairs []string
			for _, k := range keys {
				vJSON, _ := json.Marshal(env[k])
				pairs = append(pairs, fmt.Sprintf("%s = %s", k, string(vJSON)))
			}
			fmt.Fprintf(&b, "env = { %s }\n", strings.Join(pairs, ", "))
		}
	} else {
		urlJSON, _ := json.Marshal(opts.resolvedHTTPURL())
		fmt.Fprintf(&b, "url = %s\n", string(urlJSON))
		if opts.Token != "" {
			tokJSON, _ := json.Marshal("Bearer " + opts.Token)
			fmt.Fprintf(&b, "http_headers = { \"Authorization\" = %s }\n", string(tokJSON))
		}
	}
	return b.String(), nil
}

// stripTOMLInlineComment 剥离 TOML 行内未加引号的 `# ...` 注释，确保 `[other] # comment` 被正确识别为新 section。
func stripTOMLInlineComment(line string) string {
	inSingle := false
	inDouble := false
	escaped := false
	for i := 0; i < len(line); i++ {
		ch := line[i]
		if escaped {
			escaped = false
			continue
		}
		if inDouble && ch == '\\' {
			escaped = true
			continue
		}
		if ch == '"' && !inSingle {
			inDouble = !inDouble
			continue
		}
		if ch == '\'' && !inDouble {
			inSingle = !inSingle
			continue
		}
		if ch == '#' && !inSingle && !inDouble {
			return line[:i]
		}
	}
	return line
}

func parseTOMLTablePath(headerNoComment string) ([]string, bool) {
	s := strings.TrimSpace(headerNoComment)
	if strings.HasPrefix(s, "[[") && strings.HasSuffix(s, "]]") {
		s = strings.TrimSpace(s[2 : len(s)-2])
	} else if strings.HasPrefix(s, "[") && strings.HasSuffix(s, "]") {
		s = strings.TrimSpace(s[1 : len(s)-1])
	} else {
		return nil, false
	}
	if s == "" {
		return nil, false
	}

	var segs []string
	i := 0
	n := len(s)
	for i < n {
		for i < n && (s[i] == ' ' || s[i] == '\t') {
			i++
		}
		if i >= n {
			return nil, false
		}
		switch s[i] {
		case '"':
			end := i + 1
			escaped := false
			for end < n {
				if escaped {
					escaped = false
					end++
					continue
				}
				if s[end] == '\\' {
					escaped = true
					end++
					continue
				}
				if s[end] == '"' {
					break
				}
				end++
			}
			if end >= n || s[end] != '"' {
				return nil, false
			}
			var unquoted string
			if err := json.Unmarshal([]byte(s[i:end+1]), &unquoted); err != nil {
				return nil, false
			}
			segs = append(segs, unquoted)
			i = end + 1
		case '\'':
			end := strings.IndexByte(s[i+1:], '\'')
			if end < 0 {
				return nil, false
			}
			segs = append(segs, s[i+1:i+1+end])
			i = i + 1 + end + 1
		default:
			start := i
			for i < n && s[i] != '.' && s[i] != ' ' && s[i] != '\t' {
				i++
			}
			if i == start {
				return nil, false
			}
			segs = append(segs, s[start:i])
		}
		for i < n && (s[i] == ' ' || s[i] == '\t') {
			i++
		}
		if i < n {
			if s[i] != '.' {
				return nil, false
			}
			i++
			if i >= n {
				return nil, false
			}
		}
	}
	return segs, len(segs) > 0
}

func isTargetCodexMCPHeader(headerNoComment, serverName string) bool {
	segs, ok := parseTOMLTablePath(headerNoComment)
	return ok && len(segs) >= 2 && segs[0] == "mcp_servers" && segs[1] == serverName
}

func advanceTOMLMultilineState(line, currentDelim string) string {
	i := 0
	inSingle := false
	inDouble := false
	escaped := false
	for i < len(line) {
		if currentDelim != "" {
			idx := strings.Index(line[i:], currentDelim)
			if idx < 0 {
				return currentDelim
			}
			i += idx + 3
			currentDelim = ""
			continue
		}
		ch := line[i]
		if escaped {
			escaped = false
			i++
			continue
		}
		if inDouble && ch == '\\' {
			escaped = true
			i++
			continue
		}
		if !inSingle && !inDouble {
			if strings.HasPrefix(line[i:], `"""`) {
				currentDelim = `"""`
				i += 3
				continue
			}
			if strings.HasPrefix(line[i:], `'''`) {
				currentDelim = `'''`
				i += 3
				continue
			}
			if ch == '#' {
				break
			}
		}
		if ch == '"' && !inSingle {
			inDouble = !inDouble
		} else if ch == '\'' && !inDouble {
			inSingle = !inSingle
		}
		i++
	}
	return currentDelim
}

func stripCodexTOMLSection(content, serverName string) string {
	lines := strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n")
	var kept []string
	skipping := false
	inMultiline := ""
	for _, line := range lines {
		if inMultiline != "" {
			inMultiline = advanceTOMLMultilineState(line, inMultiline)
			if !skipping {
				kept = append(kept, line)
			}
			continue
		}
		noComment := strings.TrimSpace(stripTOMLInlineComment(line))
		if _, isHeader := parseTOMLTablePath(noComment); isHeader {
			if isTargetCodexMCPHeader(noComment, serverName) {
				skipping = true
				inMultiline = advanceTOMLMultilineState(line, inMultiline)
				continue
			}
			skipping = false
		}
		inMultiline = advanceTOMLMultilineState(line, inMultiline)
		if !skipping {
			kept = append(kept, line)
		}
	}
	return strings.TrimSpace(strings.Join(kept, "\n"))
}

func removeAgentConfigEntry(spec agentSpec, cfgPath, serverName string) (bool, error) {
	b, err := os.ReadFile(cfgPath)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	if spec.Format == "toml-codex" {
		cleaned := stripCodexTOMLSection(string(b), serverName)
		if strings.TrimSpace(cleaned) == strings.TrimSpace(string(b)) {
			return false, nil
		}
		if cleaned != "" {
			cleaned += "\n"
		}
		return true, atomicWriteFile(cfgPath, []byte(cleaned), 0o600)
	}

	var root map[string]any
	if err := json.Unmarshal(b, &root); err != nil {
		return false, err
	}
	removed := false
	switch spec.Format {
	case "json-opencode":
		if mcpObj, ok := root["mcp"].(map[string]any); ok {
			if _, exists := mcpObj[serverName]; exists {
				delete(mcpObj, serverName)
				removed = true
			}
		}
	case "json-openclaw":
		if mcpObj, ok := root["mcp"].(map[string]any); ok {
			if srvObj, ok := mcpObj["servers"].(map[string]any); ok {
				if _, exists := srvObj[serverName]; exists {
					delete(srvObj, serverName)
					removed = true
				}
			}
		}
	default:
		if srvObj, ok := root["mcpServers"].(map[string]any); ok {
			if _, exists := srvObj[serverName]; exists {
				delete(srvObj, serverName)
				removed = true
			}
		}
	}
	if !removed {
		return false, nil
	}
	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return false, err
	}
	return true, atomicWriteFile(cfgPath, append(out, '\n'), 0o600)
}
