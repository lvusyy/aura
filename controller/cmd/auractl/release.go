package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"connectrpc.com/connect"

	aurav1 "github.com/aura/controller/gen/aura/v1"
	aurav1connect "github.com/aura/controller/gen/aura/v1/aurav1connect"
)

// cmdRelease 处理 `release upload|list`（M16 节点 self-update 发布面）。
// upload 走 raw REST POST /v1/releases（制品字节不过 connect 消息上限；本地先算 sha256 随请求声明，
// 服务端复算交叉核验防传输损伤）；list 走 ControllerAdmin.ListReleases RPC。
func cmdRelease(ctx context.Context, client aurav1connect.ControllerAdminClient, args []string, rest restConfig) error {
	if len(args) < 1 {
		return errors.New("usage: auractl release upload --platform P --version V <file> | release list")
	}
	switch args[0] {
	case "upload":
		fs := flag.NewFlagSet("release upload", flag.ContinueOnError)
		platform := fs.String("platform", "", "binary host platform (linux-x86_64 / windows-x86_64 / macos-aarch64)")
		version := fs.String("version", "", "release version")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if *platform == "" || *version == "" || fs.NArg() != 1 {
			return errors.New("usage: auractl release upload --platform P --version V <file>")
		}
		return uploadRelease(ctx, rest, *platform, *version, fs.Arg(0))
	case "list":
		resp, err := client.ListReleases(ctx, connect.NewRequest(&aurav1.ListReleasesRequest{}))
		if err != nil {
			return err
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
		fmt.Fprintln(w, "PLATFORM\tVERSION\tSIZE\tSHA256\tCREATED")
		for _, r := range resp.Msg.GetReleases() {
			created := "-"
			if ms := r.GetCreatedAtMs(); ms > 0 {
				created = time.UnixMilli(ms).Format(time.RFC3339)
			}
			fmt.Fprintf(w, "%s\t%s\t%d\t%s\t%s\n", r.GetPlatform(), r.GetVersion(), r.GetSize(), r.GetSha256(), created)
		}
		return w.Flush()
	default:
		return fmt.Errorf("unknown release subcommand %q (want upload|list)", args[0])
	}
}

// uploadRelease 读制品文件 → 本地 sha256 → POST /v1/releases（bearer + octet-stream）→ 打印服务端登记回执。
func uploadRelease(ctx context.Context, rest restConfig, platform, version, path string) error {
	body, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read release file: %w", err)
	}
	sum := sha256.Sum256(body)
	q := url.Values{}
	q.Set("platform", platform)
	q.Set("version", version)
	q.Set("sha256", hex.EncodeToString(sum[:]))
	u := strings.TrimRight(rest.server, "/") + "/v1/releases?" + q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	if rest.token != "" {
		req.Header.Set("Authorization", "Bearer "+rest.token)
	}
	resp, err := rest.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("upload release: %w", err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("upload release: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(out)))
	}
	fmt.Print(string(out))
	return nil
}

// rolloutPollInterval 是 rollout 版本收敛轮询间隔。
const rolloutPollInterval = 3 * time.Second

// cmdRollout 处理 `rollout --version V [--nodes id1,id2|--all] [--timeout-s N]`：串行舰队滚更。
// 语义（金丝雀内建）：逐台 SelfUpdateNode → 等该台重注册且 node_version==目标 才滚下一台；任一台
// staged 失败 / 收敛超时即停（剩余节点不动）——串行 + 失败即停就是金丝雀。收敛判据全取控制面侧信号
// （ConnectedAtMs 变化 = 重注册、免客户端时钟偏斜）。--all 跳过离线 / 未上报 host_platform（未滚更过
// 的旧节点须手工滚一次）/ 已在目标版本的节点；--nodes 显式点名则一律下发（支持同版本 sha 修复推送）。
func cmdRollout(ctx context.Context, client aurav1connect.ControllerAdminClient, args []string) error {
	fs := flag.NewFlagSet("rollout", flag.ContinueOnError)
	version := fs.String("version", "", "target release version (required)")
	nodesCSV := fs.String("nodes", "", "comma-separated node ids (explicit targets)")
	all := fs.Bool("all", false, "target all eligible online nodes")
	timeoutS := fs.Int("timeout-s", 300, "per-node convergence timeout in seconds (staged wait + re-register)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *version == "" {
		return errors.New("rollout: --version is required")
	}
	if (*nodesCSV == "") == !*all {
		return errors.New("rollout: pick exactly one of --nodes or --all")
	}

	resp, err := client.ListNodes(ctx, connect.NewRequest(&aurav1.ListNodesRequest{}))
	if err != nil {
		return err
	}
	byID := make(map[string]*aurav1.NodeInfo, len(resp.Msg.GetNodes()))
	for _, n := range resp.Msg.GetNodes() {
		byID[n.GetNodeId()] = n
	}

	var targets []*aurav1.NodeInfo
	if *all {
		for _, n := range resp.Msg.GetNodes() {
			switch {
			case n.GetStatus() != "online":
				fmt.Printf("skip %s: %s\n", n.GetNodeId(), n.GetStatus())
			case n.GetHostPlatform() == "":
				fmt.Printf("skip %s: no host_platform reported (pre-self-update node, roll it manually once)\n", n.GetNodeId())
			case n.GetNodeVersion() == *version:
				fmt.Printf("skip %s: already at %s\n", n.GetNodeId(), *version)
			default:
				targets = append(targets, n)
			}
		}
	} else {
		for _, id := range strings.Split(*nodesCSV, ",") {
			id = strings.TrimSpace(id)
			if id == "" {
				continue
			}
			n, ok := byID[id]
			if !ok {
				return fmt.Errorf("rollout: node %s not found in fleet", id)
			}
			targets = append(targets, n)
		}
	}
	if len(targets) == 0 {
		fmt.Println("rollout: nothing to do")
		return nil
	}

	fmt.Printf("rollout %s: %d node(s), serial canary-first\n", *version, len(targets))
	for i, n := range targets {
		id := n.GetNodeId()
		fmt.Printf("[%d/%d] %s (%s %s -> %s) staging...\n", i+1, len(targets), id, n.GetHostPlatform(), orDash(n.GetNodeVersion()), *version)
		prevConnected := n.GetConnectedAtMs()

		su, err := client.SelfUpdateNode(ctx, connect.NewRequest(&aurav1.SelfUpdateNodeRequest{NodeId: id, Version: *version}))
		if err != nil {
			return fmt.Errorf("rollout halted at %s: %w (remaining nodes untouched)", id, err)
		}
		if !su.Msg.GetStaged() {
			return fmt.Errorf("rollout halted at %s: node refused: %s (remaining nodes untouched)", id, su.Msg.GetMessage())
		}
		fmt.Printf("[%d/%d] %s staged; waiting re-register at %s...\n", i+1, len(targets), id, *version)

		if err := waitConverged(ctx, client, id, *version, prevConnected, time.Duration(*timeoutS)*time.Second); err != nil {
			return fmt.Errorf("rollout halted at %s: %w (remaining nodes untouched)", id, err)
		}
		fmt.Printf("[%d/%d] %s converged at %s\n", i+1, len(targets), id, *version)
	}
	fmt.Printf("rollout %s complete: %d node(s)\n", *version, len(targets))
	return nil
}

// waitConverged 轮询 ListNodes 直至节点重注册（ConnectedAtMs 变化，控制面侧信号免时钟偏斜）且
// node_version==目标且在线；超时返回错误（调用方停止滚更）。
func waitConverged(ctx context.Context, client aurav1connect.ControllerAdminClient, nodeID, version string, prevConnected int64, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if time.Now().After(deadline) {
			return fmt.Errorf("convergence timeout after %s (staged ok but node did not re-register at %s; check node supervisor)", timeout, version)
		}
		time.Sleep(rolloutPollInterval)
		resp, err := client.ListNodes(ctx, connect.NewRequest(&aurav1.ListNodesRequest{}))
		if err != nil {
			continue // 轮询期瞬时错误容忍（控制面重启窗等），超时兜底
		}
		for _, n := range resp.Msg.GetNodes() {
			if n.GetNodeId() != nodeID {
				continue
			}
			if n.GetStatus() == "online" && n.GetNodeVersion() == version && n.GetConnectedAtMs() != prevConnected {
				return nil
			}
		}
	}
}

// orDash 空串显示占位 "-"（表格/进度行可读性）。
func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
