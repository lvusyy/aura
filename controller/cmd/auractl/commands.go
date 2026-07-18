package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"text/tabwriter"
	"time"

	"connectrpc.com/connect"

	aurav1 "github.com/aura/controller/gen/aura/v1"
	aurav1connect "github.com/aura/controller/gen/aura/v1/aurav1connect"
	"github.com/aura/controller/internal/storage"
)

// expectedContractVersion 是 controller 侧期望的节点契约版本，与 node 侧 CONTRACT_VERSION 常量
// （aura-node transport 层，M6 TASK-003 单点真源）手工同步（版本格式为 Free 约定）。node 上报版本
// 与此不符即视为契约偏斜，`node list` 打印 WARN（告警非拒绝——旧版本节点仍在表，仅提醒运维升级）。
const expectedContractVersion = "aura.v1/2026-07"

// contractSkewWarning 返回节点契约版本偏斜告警文本；版本为空（节点未上报）或与期望一致时返回空串。
func contractSkewWarning(nodeID, contractVersion string) string {
	if contractVersion == "" || contractVersion == expectedContractVersion {
		return ""
	}
	return fmt.Sprintf("WARN: node %s contract skew: got %s want %s", nodeID, contractVersion, expectedContractVersion)
}

// cmdNode 处理 `node list`：表格输出 node_id/platform/status/last_seen/tools/contract/version。
// tools 列为节点能力子集大小（fleet 面回填：desktop 20 / android 17），contract 为契约版本，
// version 为节点二进制版本（批E C4 滚更盘点：此前版本偏斜仅 stderr WARN 可推断，无正面版本列）；
// 契约版本偏斜的节点在表格后打印 WARN 至 stderr（fleet 可见性 consumer 落地，Locked-6）。
func cmdNode(ctx context.Context, client aurav1connect.ControllerAdminClient, args []string) error {
	if len(args) < 1 || args[0] != "list" {
		return errors.New("usage: auractl node list")
	}
	resp, err := client.ListNodes(ctx, connect.NewRequest(&aurav1.ListNodesRequest{}))
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "NODE_ID\tPLATFORM\tSTATUS\tLAST_SEEN\tTOOLS\tCONTRACT\tVERSION")
	var warnings []string
	for _, n := range resp.Msg.GetNodes() {
		last := "-"
		if ms := n.GetLastSeenMs(); ms > 0 {
			last = time.UnixMilli(ms).Format(time.RFC3339)
		}
		contract := n.GetContractVersion()
		if contract == "" {
			contract = "-"
		}
		version := n.GetNodeVersion()
		if version == "" {
			version = "-"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%s\t%s\n",
			n.GetNodeId(), n.GetPlatform(), n.GetStatus(), last, len(n.GetTools()), contract, version)
		if warn := contractSkewWarning(n.GetNodeId(), n.GetContractVersion()); warn != "" {
			warnings = append(warnings, warn)
		}
	}
	if err := w.Flush(); err != nil {
		return err
	}
	// 偏斜告警走 stderr，与 stdout 表格分离（便于 `node list | grep` 不混入告警行）。
	for _, warn := range warnings {
		fmt.Fprintln(os.Stderr, warn)
	}
	return nil
}

// cmdTool 处理 `tool call <node-id> <tool>`：下发工具调用，输出解码后的 Envelope JSON。
func cmdTool(ctx context.Context, client aurav1connect.ControllerAdminClient, args []string) error {
	if len(args) < 3 || args[0] != "call" {
		return errors.New("usage: auractl tool call <node-id> <tool> [--args JSON] [--deadline-ms N] [--who W]")
	}
	nodeID, tool := args[1], args[2]
	fs := flag.NewFlagSet("tool call", flag.ContinueOnError)
	argsJSON := fs.String("args", "{}", "tool arguments as JSON")
	deadlineMs := fs.Int64("deadline-ms", 0, "controller-side deadline in ms (0=default)")
	who := fs.String("who", "auractl", "audit caller identity")
	if err := fs.Parse(args[3:]); err != nil {
		return err
	}
	resp, err := client.DispatchTool(ctx, connect.NewRequest(&aurav1.DispatchToolRequest{
		NodeId:     nodeID,
		Tool:       tool,
		JsonArgs:   []byte(*argsJSON),
		DeadlineMs: *deadlineMs,
		Who:        *who,
	}))
	if err != nil {
		return err
	}
	return printJSON(resp.Msg.GetJsonEnvelope())
}

// cmdEnv 处理 `env create` / `env destroy`（provisioner 由 TASK-007 实现，此处仅 REST 转发）。
func cmdEnv(ctx context.Context, client aurav1connect.ControllerAdminClient, args []string) error {
	if len(args) < 1 {
		return errors.New("usage: auractl env create|destroy ...")
	}
	switch args[0] {
	case "create":
		fs := flag.NewFlagSet("env create", flag.ContinueOnError)
		kind := fs.String("kind", "ephemeral", "environment kind: ephemeral|persistent")
		template := fs.String("template", "", "template ref (empty=provider default): PVE=numeric VMID (validated) | K8s=containerDisk image ref (unvalidated)")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		resp, err := client.CreateEnvironment(ctx, connect.NewRequest(&aurav1.CreateEnvironmentRequest{
			Kind:     *kind,
			Template: *template,
		}))
		if err != nil {
			return err
		}
		fmt.Printf("env_id=%s vmid=%d node_id=%s\n", resp.Msg.GetEnvId(), resp.Msg.GetVmid(), resp.Msg.GetNodeId())
		return nil
	case "destroy":
		if len(args) < 2 {
			return errors.New("usage: auractl env destroy <env-id>")
		}
		resp, err := client.DestroyEnvironment(ctx, connect.NewRequest(&aurav1.DestroyEnvironmentRequest{EnvId: args[1]}))
		if err != nil {
			return err
		}
		fmt.Printf("destroyed=%t\n", resp.Msg.GetDestroyed())
		return nil
	default:
		return fmt.Errorf("unknown env subcommand %q", args[0])
	}
}

// cmdArtifact 处理 `artifact get <key>`：签发预签名 GET URL 并下载到文件或 stdout。
func cmdArtifact(ctx context.Context, args []string, cfg minioConfig) error {
	if len(args) < 2 || args[0] != "get" {
		return errors.New("usage: auractl artifact get <key> [--out FILE]")
	}
	key := args[1]
	fs := flag.NewFlagSet("artifact get", flag.ContinueOnError)
	out := fs.String("out", "", "output file path (default stdout)")
	if err := fs.Parse(args[2:]); err != nil {
		return err
	}
	if cfg.endpoint == "" {
		return errors.New("minio not configured (set --minio-endpoint or AURA_MINIO_ENDPOINT)")
	}

	ms, err := storage.NewMinioStore(cfg.endpoint, cfg.accessKey, cfg.secretKey, cfg.secure)
	if err != nil {
		return err
	}
	u, err := ms.PresignedGet(ctx, key, 15*time.Minute)
	if err != nil {
		return err
	}

	resp, err := http.Get(u.String())
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download failed: HTTP %d", resp.StatusCode)
	}

	dst := os.Stdout
	if *out != "" {
		f, err := os.Create(*out)
		if err != nil {
			return err
		}
		defer f.Close()
		dst = f
	}
	_, err = io.Copy(dst, resp.Body)
	return err
}

// ===== 录制回放引擎（M6，Locked-5；客户端驱动）=====

// replayStepResult 承载单步回放结果（逐步报告行 + pass/fail/unsupported 计数依据）。
type replayStepResult struct {
	Seq    int64  `json:"seq"`
	Tool   string `json:"tool"`
	Status string `json:"status"` // PASS | FAIL | UNSUPPORTED
	Detail string `json:"detail"` // 判定详情：assert data.detail / 错误码 / unsupported 原因
}

// replayReport 承载整段回放报告：逐步结果 + 终态判定 + pass/fail/unsupported 计数。
type replayReport struct {
	TraceID        string             `json:"trace_id"`
	SourceNodeID   string             `json:"source_node_id"`
	SourcePlatform string             `json:"source_platform"`
	TargetNodeID   string             `json:"target_node_id"`
	TargetMode     string             `json:"target_mode"` // explicit-node | same-platform-best-effort | ephemeral
	Steps          []replayStepResult `json:"steps"`
	Passed         int                `json:"passed"`
	Failed         int                `json:"failed"`
	Unsupported    int                `json:"unsupported"`
	TerminalAssert string             `json:"terminal_assert"` // 末 assert 步节点评判 PASS|FAIL（无 assert 步则空）
	Verdict        string             `json:"verdict"`         // 终态判定 PASS|FAIL
}

// envHead 解析节点回执 envelope 顶层 {ok,data,error}（回放判定只需这三段，data 延迟解析）。
type envHead struct {
	Ok    bool            `json:"ok"`
	Data  json.RawMessage `json:"data"`
	Error *envErr         `json:"error"`
}

// envErr 是 envelope.error 的最小视图（E_ 机器码 + 描述）。
type envErr struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// assertData 是 assert 步 envelope.data 的最小视图（passed 存在谓词判定 + detail 详情）。
type assertData struct {
	Passed bool   `json:"passed"`
	Detail string `json:"detail"`
}

// cmdTrace 处理 `trace start <node-id> [--who W]` / `trace stop <trace-id>`：
// start 对 node 建 per-node 独占租约并返回 trace_id（Locked-2 lease 语义，非持有者 dispatch 拒 E_BUSY）；
// stop 释放租约。消费 TASK-002 StartTrace/StopTrace RPC。
func cmdTrace(ctx context.Context, client aurav1connect.ControllerAdminClient, args []string) error {
	if len(args) < 1 {
		return errors.New("usage: auractl trace start <node-id> [--who W] | trace stop <trace-id>")
	}
	switch args[0] {
	case "start":
		if len(args) < 2 {
			return errors.New("usage: auractl trace start <node-id> [--who W]")
		}
		nodeID := args[1]
		fs := flag.NewFlagSet("trace start", flag.ContinueOnError)
		who := fs.String("who", "auractl", "recording session holder identity (lease owner)")
		if err := fs.Parse(args[2:]); err != nil {
			return err
		}
		resp, err := client.StartTrace(ctx, connect.NewRequest(&aurav1.StartTraceRequest{
			NodeId: nodeID,
			Who:    *who,
		}))
		if err != nil {
			return err
		}
		fmt.Printf("trace_id=%s\n", resp.Msg.GetTraceId())
		return nil
	case "stop":
		if len(args) < 2 {
			return errors.New("usage: auractl trace stop <trace-id>")
		}
		resp, err := client.StopTrace(ctx, connect.NewRequest(&aurav1.StopTraceRequest{TraceId: args[1]}))
		if err != nil {
			return err
		}
		fmt.Printf("stopped=%t\n", resp.Msg.GetStopped())
		return nil
	default:
		return fmt.Errorf("unknown trace subcommand %q", args[0])
	}
}

// cmdReplay 处理 `replay <trace-id> [--node <id>] [--platform <p>] [--deadline-ms N]`：客户端驱动回放引擎。
// 流程（Locked-5）：GetTrace 分页读全步序 → 目标定位三分支 → 子集预检 → 逐步 dispatch 保序 →
// assert 步逐字重发节点评判 → 子集不匹配 fail-fast 标 UNSUPPORTED → 逐步+终态报告 → 仅 ephemeral 分支销毁。
func cmdReplay(ctx context.Context, client aurav1connect.ControllerAdminClient, args []string) error {
	if len(args) < 1 {
		return errors.New("usage: auractl replay <trace-id> [--node <id>] [--platform <p>] [--deadline-ms N]")
	}
	traceID := args[0]
	fs := flag.NewFlagSet("replay", flag.ContinueOnError)
	nodeFlag := fs.String("node", "", "target node id (explicit override; highest precedence, E2E harness 指向重建节点)")
	platformFlag := fs.String("platform", "", "override source platform for same-platform / ephemeral template selection")
	deadlineMs := fs.Int64("deadline-ms", 0, "per-step controller-side deadline in ms (0=default)")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	report, err := runReplay(ctx, client, traceID, *nodeFlag, *platformFlag, *deadlineMs)
	if err != nil {
		return err
	}
	return printReplayReport(report)
}

// runReplay 执行回放核心逻辑并返回结构化报告（与 I/O 打印分离，供 fake client 单测直接断言）。
func runReplay(ctx context.Context, client aurav1connect.ControllerAdminClient, traceID, nodeFlag, platformOverride string, deadlineMs int64) (replayReport, error) {
	// (1) GetTrace 分页循环读全步序 + 录制源 node_id/platform（MAJOR-3，与 001 additive 分页契约一致）。
	steps, srcNode, srcPlatform, err := fetchTraceSteps(ctx, client, traceID)
	if err != nil {
		return replayReport{}, err
	}
	if len(steps) == 0 {
		return replayReport{}, fmt.Errorf("trace %q has no steps to replay", traceID)
	}
	platform := srcPlatform
	if platformOverride != "" {
		platform = platformOverride
	}

	// (2) 目标定位三分支（优先级降序，MAJOR-4/D2）；仅 ephemeral 分支返回需清理的 env_id。
	targetNode, targetMode, cleanupEnvID, err := resolveTarget(ctx, client, nodeFlag, platform)
	if err != nil {
		return replayReport{}, err
	}
	if cleanupEnvID != "" {
		// (6) 仅 CreateEnvironment ephemeral 分支回放毕销毁（--node/既有节点分支不建不销）。
		defer func() {
			if _, derr := client.DestroyEnvironment(ctx, connect.NewRequest(&aurav1.DestroyEnvironmentRequest{EnvId: cleanupEnvID})); derr != nil {
				fmt.Fprintf(os.Stderr, "WARN: destroy ephemeral env %s failed: %v\n", cleanupEnvID, derr)
			}
		}()
	}

	// 子集预检数据源：目标节点 NodeInfo.Tools（TASK-005 fleet 回填）；未知则跳过预检交节点侧兜底。
	targetTools, err := nodeToolset(ctx, client, targetNode)
	if err != nil {
		return replayReport{}, err
	}
	if targetTools == nil {
		// 目标未回填工具集（刚注册未及回填 / 重建 ephemeral 尚未上报）：跳过客户端子集预检，交真
		// dispatch 由节点侧兜底。warn 保诚实、不硬失败（GAP-3）——目标可能是刚注册未及回填。
		fmt.Fprintf(os.Stderr, "WARN: target node %s has no backfilled toolset; skipping replay subset precheck (deferring to node-side fallback)\n", targetNode)
	}

	report := replayReport{
		TraceID: traceID, SourceNodeID: srcNode, SourcePlatform: srcPlatform,
		TargetNodeID: targetNode, TargetMode: targetMode,
	}
	// (3)(4) 逐步 dispatch（per-node 串行天然保序，无需客户端 barrier）+ assert 步复演。
	for _, step := range steps {
		res := replayStepResult{Seq: step.GetSeq(), Tool: step.GetTool()}
		// 子集预检 fail-fast：目标不支持则标 UNSUPPORTED 不静默跳过（Locked-5）。
		if targetTools != nil && !targetTools[step.GetTool()] {
			res.Status, res.Detail = "UNSUPPORTED", "unsupported on target subset"
			report.Unsupported++
			report.Steps = append(report.Steps, res)
			continue
		}
		// dispatch：trace_id 空=非录制回放不触租约；per-node 串行保序由 scheduler 保证。
		resp, derr := client.DispatchTool(ctx, connect.NewRequest(&aurav1.DispatchToolRequest{
			NodeId:     targetNode,
			Tool:       step.GetTool(),
			JsonArgs:   step.GetJsonArgs(), // (4) 逐字重发录制 args（assert 步含 AssertParams{mode:"a11y"} 存在谓词 + 录制期钉死 depth/max_nodes）
			DeadlineMs: deadlineMs,
			Who:        "replay",
			TraceId:    "",
		}))
		if derr != nil {
			res.Status, res.Detail = "FAIL", derr.Error()
			report.Failed++
			report.Steps = append(report.Steps, res)
			continue
		}
		res.Status, res.Detail = evalStep(step.GetTool(), resp.Msg.GetJsonEnvelope())
		if res.Status == "PASS" {
			report.Passed++
		} else {
			report.Failed++
		}
		if step.GetTool() == "assert" {
			// assert 步天然关键步；终态=末 assert 步节点评判（Decision 3，非客户端重判非全树 diff）。
			report.TerminalAssert = res.Status
		}
		report.Steps = append(report.Steps, res)
	}

	// 终态判定：以末 assert 步为准（Decision 3）；子集不匹配/dispatch 失败一律拉低为 FAIL；无 assert 步则按累计。
	report.Verdict = "PASS"
	if report.Failed > 0 || report.Unsupported > 0 || report.TerminalAssert == "FAIL" {
		report.Verdict = "FAIL"
	}
	return report, nil
}

// fetchTraceSteps 是 cmdReplay 的 GetTrace 分页循环：翻页至 next_page_token 为空聚全步序，
// 返回全步序 + 录制源 node_id/platform（分页保 connect max-recv-bytes 干净，MAJOR-3）。
func fetchTraceSteps(ctx context.Context, client aurav1connect.ControllerAdminClient, traceID string) ([]*aurav1.TraceStep, string, string, error) {
	const pageSize = 200
	// maxPages 是分页迭代上限（防挂死守卫，GAP-1）：200 步/页 × 10000 页 = 200 万步，远超任何真实
	// trace；触顶即判定服务端分页游标异常，报错退出而非无限翻页。
	const maxPages = 10000
	var steps []*aurav1.TraceStep
	var srcNode, srcPlatform, pageToken string
	for pages := 0; ; pages++ {
		if pages >= maxPages {
			return nil, "", "", fmt.Errorf("trace %q pagination exceeded %d pages; aborting (suspect server cursor bug)", traceID, maxPages)
		}
		resp, err := client.GetTrace(ctx, connect.NewRequest(&aurav1.GetTraceRequest{
			TraceId:   traceID,
			PageSize:  pageSize,
			PageToken: pageToken,
		}))
		if err != nil {
			return nil, "", "", err
		}
		steps = append(steps, resp.Msg.GetSteps()...)
		// 源节点/平台每页恒定，首个非空即取（末页 steps 空但源信息仍可能带出）。
		if srcNode == "" {
			srcNode = resp.Msg.GetNodeId()
		}
		if srcPlatform == "" {
			srcPlatform = resp.Msg.GetPlatform()
		}
		next := resp.Msg.GetNextPageToken()
		if next == "" {
			break
		}
		// 防挂死守卫（GAP-1）：next_page_token 与本次请求 token 相同 = 游标零推进（服务端异常），
		// 报错退出而非无限重取同页。
		if next == pageToken {
			return nil, "", "", fmt.Errorf("trace %q pagination made no progress (token %q repeated); aborting to avoid hang", traceID, next)
		}
		pageToken = next
	}
	return steps, srcNode, srcPlatform, nil
}

// resolveTarget 目标定位三分支（优先级降序，MAJOR-4/D2）：
//  1. --node 显式指定（最高优先级）：E2E harness 指向 android pod 重建后的新节点；不建不销。
//  2. 同 platform 在线节点：ListNodes 唯一同型选择器；持久节点无 S0 reset → best-effort 降级警示并继续。
//  3. CreateEnvironment ephemeral（create-from-baseline=S0 主路径）：返回 env_id 供回放毕销毁。
//
// 返回目标 node_id、定位模式、需清理的 env_id（仅 ephemeral 分支非空）。
func resolveTarget(ctx context.Context, client aurav1connect.ControllerAdminClient, nodeFlag, platform string) (nodeID, mode, cleanupEnvID string, err error) {
	// 分支 1：--node 显式指定。
	if nodeFlag != "" {
		return nodeFlag, "explicit-node", "", nil
	}
	// 分支 2：同 platform 在线节点。
	if platform != "" {
		resp, lerr := client.ListNodes(ctx, connect.NewRequest(&aurav1.ListNodesRequest{}))
		if lerr != nil {
			return "", "", "", lerr
		}
		for _, n := range resp.Msg.GetNodes() {
			if n.GetPlatform() == platform && n.GetStatus() == "online" {
				// 持久节点无 S0 reset → best-effort 降级警示（D2；desktop MARK 式清场由 E2E harness 负责，引擎只警示不清场）。
				fmt.Fprintf(os.Stderr, "WARN: replay on persistent node %s (platform %s) is best-effort: no S0 reset, prior state may leak; harness must ensure clean baseline\n", n.GetNodeId(), platform)
				return n.GetNodeId(), "same-platform-best-effort", "", nil
			}
		}
	}
	// 分支 3：CreateEnvironment 建 S0 ephemeral（create-from-baseline 主路径，模板可用时 M3 provisioner）。
	resp, cerr := client.CreateEnvironment(ctx, connect.NewRequest(&aurav1.CreateEnvironmentRequest{
		Kind:     "ephemeral",
		Template: platform,
	}))
	if cerr != nil {
		return "", "", "", fmt.Errorf("no --node and no online %q node; create ephemeral env failed: %w", platform, cerr)
	}
	return resp.Msg.GetNodeId(), "ephemeral", resp.Msg.GetEnvId(), nil
}

// nodeToolset 查目标节点 NodeInfo.Tools（TASK-005 fleet 回填）作为子集预检数据源。
// 返回工具名集合；目标不在 fleet 列表或未回填工具集（如新建 ephemeral / 重建节点尚未注册）→ 返回 nil
// 表示子集未知，跳过预检（真 dispatch 交节点侧兜底），不误报 UNSUPPORTED。
func nodeToolset(ctx context.Context, client aurav1connect.ControllerAdminClient, nodeID string) (map[string]bool, error) {
	resp, err := client.ListNodes(ctx, connect.NewRequest(&aurav1.ListNodesRequest{}))
	if err != nil {
		return nil, err
	}
	for _, n := range resp.Msg.GetNodes() {
		if n.GetNodeId() == nodeID {
			tools := n.GetTools()
			if len(tools) == 0 {
				return nil, nil
			}
			set := make(map[string]bool, len(tools))
			for _, t := range tools {
				set[t] = true
			}
			return set, nil
		}
	}
	return nil, nil
}

// evalStep 依节点回执 envelope 判定单步 PASS/FAIL。
// 通用步：envelope.ok 即 PASS（工具执行成功）。
// assert 步（D3/MINOR-b）：逐字重发录制 args 后由节点评判——除 envelope.ok 外再取 data.passed
// （AssertResult 存在谓词判定，非客户端重判非全树 diff）；ok && passed 才 PASS。
func evalStep(tool string, envelope []byte) (status, detail string) {
	var env envHead
	if err := json.Unmarshal(envelope, &env); err != nil {
		return "FAIL", "unparseable envelope: " + err.Error()
	}
	if !env.Ok {
		if env.Error != nil {
			return "FAIL", fmt.Sprintf("%s: %s", env.Error.Code, env.Error.Message)
		}
		return "FAIL", "envelope ok=false with no error object"
	}
	if tool == "assert" {
		var ar assertData
		if err := json.Unmarshal(env.Data, &ar); err != nil {
			return "FAIL", "unparseable assert result: " + err.Error()
		}
		if ar.Passed {
			return "PASS", "assert passed: " + ar.Detail
		}
		return "FAIL", "assert failed: " + ar.Detail
	}
	return "PASS", "ok"
}

// printReplayReport 输出逐步报告（每步 seq/tool/status/detail）+ 终态判定 + pass/fail/unsupported 计数。
// 文本表格走 stdout 便于 grep，随附 JSON 结构机读留证（json/文本双格式，Free 约定）。
func printReplayReport(r replayReport) error {
	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintf(w, "REPLAY\ttrace=%s\ttarget=%s(%s)\tsource=%s/%s\n",
		r.TraceID, r.TargetNodeID, r.TargetMode, r.SourceNodeID, r.SourcePlatform)
	fmt.Fprintln(w, "SEQ\tTOOL\tSTATUS\tDETAIL")
	for _, s := range r.Steps {
		fmt.Fprintf(w, "%d\t%s\t%s\t%s\n", s.Seq, s.Tool, s.Status, s.Detail)
	}
	if err := w.Flush(); err != nil {
		return err
	}
	fmt.Printf("VERDICT=%s passed=%d failed=%d unsupported=%d terminal_assert=%q\n",
		r.Verdict, r.Passed, r.Failed, r.Unsupported, r.TerminalAssert)
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(b))
	return nil
}

// printJSON 尽力缩进输出 JSON 字节；非 JSON 则原样写出。
func printJSON(b []byte) error {
	if len(b) == 0 {
		return nil
	}
	if json.Valid(b) {
		var buf bytes.Buffer
		if err := json.Indent(&buf, b, "", "  "); err == nil {
			fmt.Println(buf.String())
			return nil
		}
	}
	_, err := os.Stdout.Write(b)
	return err
}
