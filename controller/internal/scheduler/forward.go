package scheduler

// forward.go 承载 T8 跨副本转发（ha-contract §1.4）：dispatch 入口 registry.Ready 失败时按
// node:owner 归属把请求整体转投 owner 副本的 :18080 ControllerAdmin DispatchTool REST 全链。
// owner 侧完整执行 gateway.Dispatch（含 needs_upload 探测→awaitBypassUpload→GrantAndAwait），
// upload-await 复合语义全在 owner 闭环（§2 候选 a：GrantUpload 的活流 sess.Send、UploadComplete
// 的节点流、uploadPending 私有 map 三面全在 owner，天然闭环）；envelope 逐字节透传回调用方
// （哑管道：不回解析 envelope 派生 code——节点自身也产 E_TIMEOUT/E_INTERNAL 信封，内容层与
// 传输层错误在信封形态下不可靠区分，任一方向回派生都制造语义斜移）。

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"

	aurav1 "github.com/aura/controller/gen/aura/v1"
	"github.com/aura/controller/gen/aura/v1/aurav1connect"
)

// ForwardedByHeader 是跨副本转发请求的 hop 标记 header（m-1 stale-owner 弹球防护）：值为发起
// 副本 replicaID（诊断价值>布尔）。出站转发必带；入站经 InboundForwardMarker 打 ctx 标，dispatch
// 读标后不查 owner 表、不二次转发——一跳上限即终态，A↔B 弹球在协议层不可能发生。
const ForwardedByHeader = "X-Aura-Forwarded-By"

// forwardTimeoutSlack 是转发兜底硬超时在执行上限之外的固定余量（ha-contract §1.4#7）：兜底只防
// owner 病态挂起泄漏连接，正常上限由 owner 侧节点 deadline + 控制面 timer 决定。
const forwardTimeoutSlack = 30 * time.Second

// peerFailTTL 是 peer 失联短路标记的存活窗（批E C7）：标记窗内对同 peer 的转发直接快速失败
// （调用方合成 E_NODE_OFFLINE），不再逐请求撞最坏 ~392s 兜底窗；窗过自动放行重试探活。短 TTL
// 权衡：peer 恢复后最迟本窗时长重新可达，误标代价有界。
const peerFailTTL = 8 * time.Second

// inboundForwardKey 是「本请求由他副本转发而来」的 ctx 标记键。
type inboundForwardKey struct{}

// InboundForwardMarker 包裹 REST adminHandler（main.go newRESTServer 装配点）：入站请求带
// ForwardedByHeader 即在请求 ctx 打标，标记经 connect handler → gateway → scheduler.dispatch
// 贯通（middleware 形态，rest.go/transport 零动）。
func InboundForwardMarker(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(ForwardedByHeader) != "" {
			r = r.WithContext(context.WithValue(r.Context(), inboundForwardKey{}, true))
		}
		next.ServeHTTP(w, r)
	})
}

// isInboundForward 报告 ctx 是否携带入站转发标记。被转发方 Ready 失败时据此直接走 E_NODE_OFFLINE
// 终态——此刻 Redis owner 指向必 stale，只信本副本 registry.Ready（错误码表行 8）。
func isInboundForward(ctx context.Context) bool {
	marked, _ := ctx.Value(inboundForwardKey{}).(bool)
	return marked
}

// ParseReplicaPeers 解析 AURA_REPLICA_PEERS 副本直连对称表：
// "replica-1=https://<replica1-host>:18080,replica-2=https://<replica2-host>:18080"。
// 全量表含 self（同一配置值可部署到两副本），self 条目由 NewForwarder 按 selfID 过滤。端点用
// 副本直连地址而非 VIP：VIP 指向 MASTER 自身时形成自转发一跳浪费，直连表零歧义（§1.4#8）。
func ParseReplicaPeers(spec string) (map[string]string, error) {
	peers := make(map[string]string)
	for _, entry := range strings.Split(spec, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		id, endpoint, ok := strings.Cut(entry, "=")
		id, endpoint = strings.TrimSpace(id), strings.TrimSpace(endpoint)
		if !ok || id == "" || endpoint == "" {
			return nil, fmt.Errorf("invalid replica peer entry %q (want <replica-id>=<https://host:port>)", entry)
		}
		peers[id] = endpoint
	}
	if len(peers) == 0 {
		return nil, fmt.Errorf("replica peers table is empty")
	}
	return peers, nil
}

// peerTarget 是单个 peer 副本的转发目标：装配期预构造的生成 client（协议零漂移，自写 net/http
// 拼 connect 协议否决）+ 端点原文（转发失败时的诊断信息，错误码表行 7 要求 err 含 peer 端点）。
// console client 供 T11 只读旁路转发（ReadNodeScreen，owner 侧落 DispatchReadOnly 保读语义端到端）。
type peerTarget struct {
	endpoint string
	client   aurav1connect.ControllerAdminClient
	console  aurav1connect.ConsoleServiceClient
}

// Forwarder 把 Ready 失败的 dispatch 按 node:owner 归属转投 owner 副本（ha-contract §1.4）。
// 转发发生在「入队前」的入口层：非 owner 副本对该 node 不建队列、不写审计、不做 checkLease
// （租约在 owner 侧全链恰好执行一次），串行性由 owner 单点队列保持——这是「队列以所有权路由
// 消解」的实现语义。
type Forwarder struct {
	selfID         string
	owners         OwnerReader
	peers          map[string]peerTarget
	bearerToken    string
	httpClient     connect.HTTPClient // 裸 HTTP 转发腿（M14 MCP 网关）：与 connect clients 同源 TLS 配置
	awaitUploadCap time.Duration // owner 侧 upload-await 上限项（main.go 装配注入，transport 常量同源）

	// peer 失联短路标记（批E C7）：转发遇传输层不可达（CodeUnavailable）即记 replicaID→时刻，
	// peerFailTTL 窗内同 peer 转发快速失败。业务层错误（owner 侧 PermissionDenied/信封错误）不标
	// ——peer 活着只是拒绝，短路会误伤后续可成功请求。
	failMu   sync.Mutex
	peerDown map[string]time.Time
}

// NewForwarder 构造跨副本转发器（main.go 装配）。peers 是全量对称表（含 self，此处按 selfID
// 过滤，转发永不打自身）；httpClient 由装配方带 TLS 配置（RootCAs=cert-dir/ca.crt +
// AURA_PEER_TLS_SERVERNAME）；bearerToken 与本副本 REST 同源（副本间 token 同源配置，T12 保证）。
func NewForwarder(selfID string, peers map[string]string, owners OwnerReader, httpClient connect.HTTPClient, bearerToken string, awaitUploadCap time.Duration) *Forwarder {
	targets := make(map[string]peerTarget, len(peers))
	for id, ep := range peers {
		if id == selfID {
			continue
		}
		targets[id] = peerTarget{
			endpoint: ep,
			client:   aurav1connect.NewControllerAdminClient(httpClient, ep),
			console:  aurav1connect.NewConsoleServiceClient(httpClient, ep),
		}
	}
	return &Forwarder{
		selfID:         selfID,
		owners:         owners,
		peers:          targets,
		bearerToken:    bearerToken,
		httpClient:     httpClient,
		awaitUploadCap: awaitUploadCap,
		peerDown:       make(map[string]time.Time),
	}
}

// ForwardMcp 把一次 MCP 网关请求（M14）转投 owner 副本的 `/v1/mcp/<node_id>`。owner 判定与
// TryForward 同表镜像（无 owner/自指/peers 缺口/读错误 → attempted=false 落回现行 404）；hop 防环
// 经 ForwardedByHeader——被转发方见此头即不二次转发。转发体为原始 JSON-RPC 字节（哑管道），
// 白名单头透传保 rmcp 协商与观测语义。返回 owner 侧的 status/content-type/body 原样。
func (f *Forwarder) ForwardMcp(ctx context.Context, nodeID string, body []byte, hdr http.Header) (status int, contentType string, respBody []byte, err error, attempted bool) {
	owner, oerr := f.owners.GetNodeOwner(ctx, nodeID)
	if oerr != nil {
		slog.Warn("node owner lookup failed; treating as no owner", "node_id", nodeID, "err", oerr)
		return 0, "", nil, nil, false
	}
	if owner == "" || owner == f.selfID {
		return 0, "", nil, nil, false
	}
	peer, known := f.peers[owner]
	if !known {
		slog.Warn("node owner not in replica peers table; cannot forward mcp", "node_id", nodeID, "owner_replica", owner, "self_replica", f.selfID)
		return 0, "", nil, nil, false
	}
	if f.peerRecentlyDown(owner) {
		return 0, "", nil, fmt.Errorf("forward mcp for node %s: replica %s (%s) recently unreachable (fast-fail within %s)", nodeID, owner, peer.endpoint, peerFailTTL), true
	}

	req, rerr := http.NewRequestWithContext(ctx, http.MethodPost, peer.endpoint+"/v1/mcp/"+nodeID, bytes.NewReader(body))
	if rerr != nil {
		return 0, "", nil, rerr, true
	}
	for _, h := range []string{"Content-Type", "Accept", "User-Agent", "Mcp-Protocol-Version"} {
		if v := hdr.Get(h); v != "" {
			req.Header.Set(h, v)
		}
	}
	req.Header.Set("Authorization", "Bearer "+f.bearerToken)
	req.Header.Set(ForwardedByHeader, f.selfID)

	resp, derr := f.httpClient.Do(req)
	if derr != nil {
		f.markPeerDown(owner, derr)
		return 0, "", nil, fmt.Errorf("forward mcp for node %s to replica %s (%s): %w", nodeID, owner, peer.endpoint, derr), true
	}
	defer resp.Body.Close()
	b, berr := io.ReadAll(io.LimitReader(resp.Body, mcpForwardRespCap))
	if berr != nil {
		return 0, "", nil, fmt.Errorf("read forwarded mcp response from replica %s: %w", owner, berr), true
	}
	return resp.StatusCode, resp.Header.Get("Content-Type"), b, nil, true
}

// mcpForwardRespCap 是跨副本 MCP 转发响应体上限（与节点反连帧 16MB 上限同数量级；截图 base64
// 内联是最大合法载荷）。
const mcpForwardRespCap = 32 * 1024 * 1024

// peerRecentlyDown 报告 owner 副本是否处于失联短路窗内（批E C7）。命中即调用方快速失败，
// 免逐请求撞满转发兜底窗。
func (f *Forwarder) peerRecentlyDown(owner string) bool {
	f.failMu.Lock()
	defer f.failMu.Unlock()
	at, ok := f.peerDown[owner]
	if !ok {
		return false
	}
	if time.Since(at) > peerFailTTL {
		delete(f.peerDown, owner) // 窗过清标，放行探活
		return false
	}
	return true
}

// markPeerDown 按错误性质标记 peer 失联：仅传输层不可达（CodeUnavailable——连接拒绝/网络不可达
// 的 connect 折射码）标记；业务错误/超时不标（peer 活着或只是慢，短路误伤面大于收益）。
func (f *Forwarder) markPeerDown(owner string, err error) {
	if connect.CodeOf(err) != connect.CodeUnavailable {
		return
	}
	f.failMu.Lock()
	f.peerDown[owner] = time.Now()
	f.failMu.Unlock()
	slog.Warn("replica peer marked unreachable; fast-failing forwards within TTL", "owner_replica", owner, "ttl", peerFailTTL.String(), "err", err)
}

// TryForward 尝试把一次 Ready 失败的 dispatch 转投 owner 副本。
// attempted=false：本请求不可转发（无 owner 键/owner==self/owner 不在 peers 表/owner 读错误，
// 错误码表行 3/4/5/10），调用方落回现行 E_NODE_OFFLINE 路径，文案不变。
// attempted=true：转发已发出——err==nil 时 env 为 owner 所产 envelope（含 owner 侧 E_BUSY 租约
// 拒等错误信封，逐字节透传，行 6）；err!=nil 为转发网络错/非 2xx，一跳终态不重试不改投（行 7），
// 调用方以 E_NODE_OFFLINE 返回（err 含 peer 端点与原因）。
func (f *Forwarder) TryForward(ctx context.Context, nodeID, tool string, jsonArgs []byte, deadlineMs int64, who, traceID string) (env []byte, err error, attempted bool) {
	owner, oerr := f.owners.GetNodeOwner(ctx, nodeID)
	if oerr != nil {
		// 行 10：owner 读错误视同无 owner（§7 读降级原则）——owner 设施降级=转发降级为现行
		// E_NODE_OFFLINE，不把 Redis 故障放大为新故障面。
		slog.Warn("node owner lookup failed; treating as no owner", "node_id", nodeID, "err", oerr)
		return nil, nil, false
	}
	if owner == "" || owner == f.selfID {
		// 行 3（无 owner 键：节点全域离线）/ 行 4（stale 自指：节点刚断，键未过期）：不转发。
		return nil, nil, false
	}
	peer, known := f.peers[owner]
	if !known {
		// 行 5：owner 不在 peers 表——配置缺口 Warn 后回现行错误。
		slog.Warn("node owner not in replica peers table; cannot forward", "node_id", nodeID, "owner_replica", owner, "self_replica", f.selfID)
		return nil, nil, false
	}
	// 批E C7：失联短路窗内快速失败（attempted=true 语义不变——本请求确曾指向该 peer）。
	if f.peerRecentlyDown(owner) {
		return nil, fmt.Errorf("forward dispatch for node %s: replica %s (%s) recently unreachable (fast-fail within %s)", nodeID, owner, peer.endpoint, peerFailTTL), true
	}
	env, err = f.forward(ctx, peer, nodeID, tool, jsonArgs, deadlineMs, who, traceID)
	if err != nil {
		f.markPeerDown(owner, err)
		return nil, fmt.Errorf("forward dispatch for node %s to replica %s (%s): %w", nodeID, owner, peer.endpoint, err), true
	}
	return env, nil, true
}

// forward 执行一次到 peer 的 DispatchTool 转发调用：DispatchToolRequest 六字段原样复制（who/
// trace_id 经既有 proto 字段穿透，node.proto:159-160，无需 header 携带）+ hop 标记 + bearer。
// 携调用方 ctx（取消传播）之外另设兜底硬超时 max(deadline, defaultTimeout) + graceTimeout +
// awaitUploadCap + slack。
func (f *Forwarder) forward(ctx context.Context, peer peerTarget, nodeID, tool string, jsonArgs []byte, deadlineMs int64, who, traceID string) ([]byte, error) {
	execCap := defaultTimeout
	if d := time.Duration(deadlineMs) * time.Millisecond; d > execCap {
		execCap = d
	}
	fctx, cancel := context.WithTimeout(ctx, execCap+graceTimeout+f.awaitUploadCap+forwardTimeoutSlack)
	defer cancel()

	req := connect.NewRequest(&aurav1.DispatchToolRequest{
		NodeId:     nodeID,
		Tool:       tool,
		JsonArgs:   jsonArgs,
		DeadlineMs: deadlineMs,
		Who:        who,
		TraceId:    traceID,
	})
	req.Header().Set("Authorization", "Bearer "+f.bearerToken)
	req.Header().Set(ForwardedByHeader, f.selfID)
	resp, err := peer.client.DispatchTool(fctx, req)
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetJsonEnvelope(), nil
}

// —— T11 只读旁路转发（读写分离的跨副本腿）—————————————————————————————————————————————

// TryForwardReadOnly 尝试把一次 Ready 失败的只读请求转投 owner 副本的 ConsoleService.ReadNodeScreen
// ——owner 侧以同 RPC 落 DispatchReadOnly，绕队列+租约豁免的读语义端到端保持（经 DispatchTool 写
// 通道转发会在 owner 侧入队，读写分离即失效，否决）。owner 路由判定与 TryForward 同表镜像（行
// 3/4/5/10 → attempted=false 落回现行 E_NODE_OFFLINE；行 7 一跳终态）；T8 既有转发逻辑零动，本
// 方法只作只读分支的并列实现。hop 防环同 header：console 面经 InboundForwardMarker 同款包裹
// （main.go 装配），owner 侧 Ready 失败不查 owner 不三次转发。
func (f *Forwarder) TryForwardReadOnly(ctx context.Context, nodeID, tool string, jsonArgs []byte, deadlineMs int64, who string) (env []byte, err error, attempted bool) {
	owner, oerr := f.owners.GetNodeOwner(ctx, nodeID)
	if oerr != nil {
		slog.Warn("node owner lookup failed; treating as no owner", "node_id", nodeID, "err", oerr)
		return nil, nil, false
	}
	if owner == "" || owner == f.selfID {
		return nil, nil, false
	}
	peer, known := f.peers[owner]
	if !known {
		slog.Warn("node owner not in replica peers table; cannot forward read", "node_id", nodeID, "owner_replica", owner, "self_replica", f.selfID)
		return nil, nil, false
	}
	// 批E C7：读旁路同享失联短路（console 截图轮询对失联 peer 的快速失败尤其重要——轮询积压面大）。
	if f.peerRecentlyDown(owner) {
		return nil, fmt.Errorf("forward read-only %s for node %s: replica %s (%s) recently unreachable (fast-fail within %s)", tool, nodeID, owner, peer.endpoint, peerFailTTL), true
	}
	env, err = f.forwardReadOnly(ctx, peer, nodeID, jsonArgs, deadlineMs, who)
	if err != nil {
		f.markPeerDown(owner, err)
		return nil, fmt.Errorf("forward read-only %s for node %s to replica %s (%s): %w", tool, nodeID, owner, peer.endpoint, err), true
	}
	return env, nil, true
}

// forwardReadOnly 执行一次到 peer 的 ReadNodeScreen 转发调用（hop 标记 + bearer 与写路径同源）。
// 兜底硬超时不含 awaitUploadCap 项：只读白名单准入即排除 needs_upload 大产物旁路，读窗=执行上限+
// 网络余量。
func (f *Forwarder) forwardReadOnly(ctx context.Context, peer peerTarget, nodeID string, jsonArgs []byte, deadlineMs int64, who string) ([]byte, error) {
	execCap := readOnlyDefaultTimeout
	if d := time.Duration(deadlineMs) * time.Millisecond; d > execCap {
		execCap = d
	}
	fctx, cancel := context.WithTimeout(ctx, execCap+graceTimeout+forwardTimeoutSlack)
	defer cancel()

	req := connect.NewRequest(&aurav1.ReadNodeScreenRequest{
		NodeId:     nodeID,
		JsonArgs:   jsonArgs,
		DeadlineMs: deadlineMs,
		Who:        who,
	})
	req.Header().Set("Authorization", "Bearer "+f.bearerToken)
	req.Header().Set(ForwardedByHeader, f.selfID)
	resp, err := peer.console.ReadNodeScreen(fctx, req)
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetJsonEnvelope(), nil
}
