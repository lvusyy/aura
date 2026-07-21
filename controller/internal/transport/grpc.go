// Package transport 承载控制面两个对外服务的 connect-go handler：
//   - NodeControl（:7443 mTLS gRPC）：节点反连双向流；
//   - ControllerAdmin（:18080 TLS+bearer REST）：auractl/agent 管理面。
//
// 遵循哑管道设计：本层不解析 json_args / json_envelope 内容，仅原样搬运。
package transport

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"

	aurav1 "github.com/aura/controller/gen/aura/v1"
	"github.com/aura/controller/internal/observability"
	"github.com/aura/controller/internal/registry"
	"github.com/aura/controller/internal/storage"
	"github.com/aura/controller/internal/store"
)

// defaultSendBuffer 是每节点下行帧队列的缓冲深度。
const defaultSendBuffer = 16

// nodeHealthTTL 是 Redis 健康键的存活时长（应为应用层心跳周期的数倍）。
const nodeHealthTTL = 90 * time.Second

// nodeOwnerTTL 是 node:owner 归属键的存活时长（ha-contract §1.1：与 nodeHealthTTL 同值同理——
// 节点心跳 15s 续租，6 拍冗余，与 health 键同一心智模型）。
const nodeOwnerTTL = nodeHealthTTL

// replicaID 是本控制面副本标识（ha-contract §1.1）：env AURA_REPLICA_ID，空值 fallback 主机名，
// 仍失败取 "single"（两生产机 hostname 必不同，误配也不同值）。包级启动时一次读取——replicaID
// 的读取点按 D6 归属钉死在 transport 包，main.go 不参与（T8 的 scheduler 侧经 main.go 注入同一
// env 值，同进程同 env 两处消费一致）。生产定值 replica-1/replica-2 由部署资产（T12）写死。
var replicaID = resolveReplicaID()

// resolveReplicaID 按「env > hostname > "single"」解析副本标识。
func resolveReplicaID() string {
	if id := os.Getenv("AURA_REPLICA_ID"); id != "" {
		return id
	}
	if hn, err := os.Hostname(); err == nil && hn != "" {
		return hn
	}
	return "single"
}

// grantUploadTTL 是大产物旁路上传预签名 URL 的默认有效期（节点须在期内完成 HTTP PUT）。
const grantUploadTTL = 15 * time.Minute

// awaitUploadTimeout 是 awaitUpload 的兜底超时：与 grantUploadTTL（URL 有效期/重发授权窗）语义
// 解耦（T10，SC-4）——URL 有效期保持 15min 不变，但 gateway 同步等待窗收敛至 ~330s：略大于节点侧
// 单次 PUT 硬超时 PUT_TIMEOUT=300s（node upload.rs）+ 余量，节点 PUT 挂死时控制面必然先于节点
// 判定失败的时代结束，任何情况下 Dispatch 阻塞 ≤ 本值而非 15min。旧节点无 UploadFailed 帧时
// 本兜底独立成立（两案纵深）。
const awaitUploadTimeout = 330 * time.Second

// pendingKey 是旁路上传完成等待的复合键（node_id + 对象 key）：以复合键注册/resolve，防跨节点同 key
// 误 resolve（不同节点可能各自上传同名 key）。
type pendingKey struct {
	nodeID string
	key    string
}

// NodeControlServer 实现 aurav1connect.NodeControlHandler，处理节点反连双向流。
type NodeControlServer struct {
	registry   *registry.NodeRegistry
	redis      *store.RedisStore   // 可为 nil（未配置 Redis 时纯内存判活）
	artifacts  *storage.MinioStore // 可为 nil（未配置 MinIO 时旁路上传不可用）
	sendBuffer int

	// uploadPending 关联 (node_id,key) -> 等待上传结果的 channel（旁路上传请求/回执关联，复刻
	// registry.NodeSession pending 模板）。GrantAndAwait 注册；UploadComplete 收帧 resolve nil（成功）、
	// UploadFailed 收帧 resolve 节点错误（T10 提前唤醒，免等满兜底窗）。载荷改造收敛在本进程内
	//（ha-contract §2.2：resolve 全在 owner 副本，零跨副本特判）。
	uploadMu      sync.Mutex
	uploadPending map[pendingKey]chan error

	// awaitTimeout 是 awaitUpload 的兜底超时（生产恒 awaitUploadTimeout；仅测试注入缩短，禁生产改小）。
	awaitTimeout time.Duration

	// metaStore 是录屏对象 → 源节点映射的写面（M12 批C，recordings_meta 表）：UploadComplete 收帧
	// 时补记 (key → node_id)，供 ListRecordings 回填。经 SetMetaStore 装配期可空注入（仿 gateway.
	// SetUploader 惯例，不改构造签名）；nil（纯内存运行/未注入）时映射记录整体旁路。
	metaStore *store.PGStore

	// agentObs 是直连 MCP agent 活动记录器（M13）：收 AgentActivity 帧时落库/入内存环形缓冲，供 console
	// 「接入观测」页读。经 SetAgentObs 装配期一次性注入（PG 配置时 pg-backed、否则内存兜底，两情形均非
	// nil）；未注入（早期单测裸构造）时保 nil，收帧臂判空整体旁路（观测缺失不影响反连主链）。
	agentObs *store.AgentObs
	// agentObsSem 限制在途 agent 活动落库 goroutine 数（信号量；满即整批丢弃 + 计数）：PG 变慢时
	// 每帧一 goroutine 会无界积压，与观测面「有界 best-effort」纪律相悖——观测宁丢不积。
	agentObsSem chan struct{}
}

// NewNodeControlServer 构造节点控制服务。redis 与 artifacts 均可为 nil。
func NewNodeControlServer(reg *registry.NodeRegistry, redis *store.RedisStore, artifacts *storage.MinioStore) *NodeControlServer {
	return &NodeControlServer{
		registry:      reg,
		redis:         redis,
		artifacts:     artifacts,
		sendBuffer:    defaultSendBuffer,
		uploadPending: make(map[pendingKey]chan error),
		awaitTimeout:  awaitUploadTimeout,
		agentObsSem:   make(chan struct{}, agentObsMaxInflight),
	}
}

// SetMetaStore 注入 PG store（装配期一次性调用，早于服务监听——仿 gateway.SetUploader / console 的
// SetFusionEngine 可空注入惯例，既有调用点零改）。当前唯一消费点：UploadComplete 收帧时记录
// recordings/ 对象 → 源节点映射（recordUploadMeta）。未调用（纯内存运行）时保 nil、映射整体旁路。
func (s *NodeControlServer) SetMetaStore(pg *store.PGStore) {
	s.metaStore = pg
}

// SetAgentObs 注入直连 MCP agent 活动记录器（M13，装配期一次性调用；仿 SetMetaStore 可空注入惯例）。
// 收 AgentActivity 帧时经此落库/入内存缓冲。未注入时保 nil、收帧臂旁路（观测缺失不影响反连主链）。
func (s *NodeControlServer) SetAgentObs(ao *store.AgentObs) {
	s.agentObs = ao
}

// GrantUpload 为指定在线节点签发大产物旁路上传授权（G-5）：经 MinIO PresignedPut 按节点上报的网络域
//（sess.NetworkZone，T07/REC-6）签发指向该域可达端点的预签名 PUT URL，下发 UploadUrlGrant 帧。节点收帧后
// 直连对象存储 HTTP PUT，绕开双向流 16MB 内联上限，完成后回 UploadComplete。本方法为旁路上传 producer；
// 录屏 / 大 file_pull 的实际触发在 TASK-010 消费侧接线。返回签发的 grant 供调用方审计/关联。
func (s *NodeControlServer) GrantUpload(ctx context.Context, nodeID, key string) (*aurav1.UploadUrlGrant, error) {
	if s.artifacts == nil {
		return nil, errors.New("artifact store not configured (set AURA_MINIO_ENDPOINT)")
	}
	sess, ok := s.registry.Ready(nodeID)
	if !ok {
		return nil, registry.ErrNodeGone
	}
	// 按节点上报的网络域签发预签名 URL（T07/REC-6）：不同网络域节点（如 252.x 经跳板 vs lan 直连）
	// 可达 MinIO 端点不同，据 sess.NetworkZone 选对应端点——空/未知域回落默认（AURA_MINIO_PUBLIC_ENDPOINT），
	// 兼容未探测上报的旧节点。收口 ISS-20260714-003（252.x 上传腿指向其可达跳板端点）。
	u, err := s.artifacts.PresignedPut(ctx, key, grantUploadTTL, sess.NetworkZone)
	if err != nil {
		return nil, err
	}
	grant := &aurav1.UploadUrlGrant{
		Key:          key,
		PresignedUrl: u.String(),
		TtlSecs:      int64(grantUploadTTL / time.Second),
	}
	frame := &aurav1.ControllerToNode{
		Payload: &aurav1.ControllerToNode_UploadUrlGrant{UploadUrlGrant: grant},
	}
	if err := sess.Send(ctx, frame); err != nil {
		return nil, err
	}
	slog.Info("upload url granted", "node_id", nodeID, "key", key, "zone", sess.NetworkZone, "ttl_secs", grant.GetTtlSecs())
	return grant, nil
}

// GrantAndAwait 为节点签发旁路上传授权并同步等待其上传结果（带超时），令 gateway 在 needs_upload
// 路径返回时对象已落 MinIO、resource_link 立即可用。复刻 registry.go NodeSession pending 模板：
// 以 (node_id,key) 复合键注册结果 channel，先注册再发帧，避免节点回执早于注册的竞态。
// 返回 nil 表示上传已完成；节点回 UploadFailed 帧即时返回其失败原因（提前唤醒，免等满窗）；
// ctx 取消或超过 awaitUploadTimeout 兜底窗未决则返回超时错误（调用方降级处理）。
// 批E：taskID/traceID 是产出该对象的 dispatch 上下文（gateway 携入）——上传完成即携其补记
// recordings_meta 关联（录屏 → 任务/录制会话下钻）；收帧臂兜底写无此上下文，两路 COALESCE 合流。
func (s *NodeControlServer) GrantAndAwait(ctx context.Context, nodeID, key, taskID, traceID string) error {
	pk := pendingKey{nodeID: nodeID, key: key}
	ch := s.registerUpload(pk)
	defer s.releaseUpload(pk, ch)

	// 先注册 pending 再发帧：若反序（先 GrantUpload 后注册），节点可能在注册前就完成上传并回
	// UploadComplete，导致 resolve 落空、GrantAndAwait 空等至超时。
	if _, err := s.GrantUpload(ctx, nodeID, key); err != nil {
		return err
	}
	if err := s.awaitUpload(ctx, ch, nodeID, key); err != nil {
		return err
	}
	// 上传已完成：携 dispatch 上下文全量补记映射（收帧臂的兜底写 task/trace 为空，此处才有关联）。
	s.recordUploadMeta(nodeID, key, taskID, traceID)
	return nil
}

// awaitUpload 阻塞至 ch 被 resolveUpload 唤醒（nil=上传完成；非 nil=节点显式失败，即时返回）、
// ctx 取消或超过 awaitTimeout 兜底窗（返回带 context.DeadlineExceeded 链的超时错误——gateway 据
// 错误链区分「节点显式失败」与「兜底超时」两种降级日志）。
func (s *NodeControlServer) awaitUpload(ctx context.Context, ch <-chan error, nodeID, key string) error {
	awaitCtx, cancel := context.WithTimeout(ctx, s.awaitTimeout)
	defer cancel()
	select {
	case err := <-ch:
		if err != nil {
			return fmt.Errorf("node reported upload failure for node %s key %q: %s", nodeID, key, err)
		}
		return nil
	case <-awaitCtx.Done():
		return fmt.Errorf("await upload complete for node %s key %q: %w", nodeID, key, awaitCtx.Err())
	}
}

// registerUpload 注册一个复合键的结果等待 channel（缓冲 1，令 resolve 非阻塞）。同键在途时覆盖
// （重复触发以最新等待方为准，旧等待方由自身 ctx/超时自解）。
func (s *NodeControlServer) registerUpload(pk pendingKey) chan error {
	ch := make(chan error, 1)
	s.uploadMu.Lock()
	s.uploadPending[pk] = ch
	s.uploadMu.Unlock()
	return ch
}

// releaseUpload 注销等待 channel，仅当当前条目仍为本 channel 时删除（避免误删同键新等待方，镜像
// registry.Remove 的重连竞态防护）。
func (s *NodeControlServer) releaseUpload(pk pendingKey, ch chan error) {
	s.uploadMu.Lock()
	if cur, ok := s.uploadPending[pk]; ok && cur == ch {
		delete(s.uploadPending, pk)
	}
	s.uploadMu.Unlock()
}

// resolveUpload 唤醒等待指定 (node_id,key) 旁路上传结果的 GrantAndAwait 调用方：收 UploadComplete
// 传 nil（成功），收 UploadFailed 传节点错误（提前唤醒降级）。无人等待时静默丢弃（镜像
// registry.DeliverResponse）。
func (s *NodeControlServer) resolveUpload(nodeID, key string, uploadErr error) {
	pk := pendingKey{nodeID: nodeID, key: key}
	s.uploadMu.Lock()
	ch, ok := s.uploadPending[pk]
	s.uploadMu.Unlock()
	if !ok {
		return
	}
	select {
	case ch <- uploadErr:
	default:
	}
}

// recordUploadMeta 把 recordings/ 前缀的旁路上传对象补记 (key → node_id/task/trace) 映射
// （recordings_meta 表，M12 批C + 批E 关联富化）。写入方两路：GrantAndAwait 完成点携 dispatch 上下文
// 全量写；UploadComplete 收帧臂兜底写（taskID/traceID 空——覆盖 GrantAndAwait 已超时返回但上传迟到
// 的对象，store 侧 COALESCE 保留非空关联）。goroutine + 独立 ctx（审计写入纪律：不复用流 ctx——节点
// 断流取消恰丢最后一笔；异步化不阻塞收帧循环）。best-effort：metaStore 未注入 / 非录屏对象 / 写失败
// 均不影响上传链路，失败仅告警计数（映射缺失 = 该对象 node_id 留空，ListRecordings 如实呈现）。
func (s *NodeControlServer) recordUploadMeta(nodeID, key, taskID, traceID string) {
	if s.metaStore == nil || !strings.HasPrefix(key, recordingsKeyPrefix) {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := s.metaStore.UpsertRecordingMeta(ctx, key, nodeID, taskID, traceID); err != nil {
			observability.IncStoreOpFailure("recording_meta")
			slog.Warn("record recording meta failed", "node_id", nodeID, "key", key, "err", err)
		}
	}()
}

// agentObsMaxInflight 是 agent 活动落库的在途 goroutine 上限（超限整批丢弃）：正常节奏每节点 ≤1 帧/2s、
// 每批一次 PG 往返，32 足以覆盖百节点级舰队；打满意味着 PG 已拥塞，观测按纪律丢弃而非积压。
const agentObsMaxInflight = 32

// recordAgentActivity 记录一批直连 MCP agent 活动事件（M13 收 AgentActivity 帧）：打 Prometheus 计数 +
// 落库/入内存缓冲。best-effort：未注入 agentObs 时仅计数不落库；落库失败仅告警不断反连流（收帧臂调用，
// 不能因观测写失败中断节点主链）。落库放 goroutine 避免给收帧循环添 PG 往返延迟（同 recordUploadMeta
// 惯例），但经 agentObsSem 有界——PG 拥塞时丢批计数，不无界积压 goroutine。
func (s *NodeControlServer) recordAgentActivity(nodeID string, act *aurav1.AgentActivity) {
	events := act.GetEvents()
	if len(events) == 0 {
		return
	}
	// Prometheus 计数（method 归一到有界集防标签基数爆炸）。同步打点（计数廉价，无 IO）。
	for _, e := range events {
		observability.IncAgentCall(agentMethodLabel(e.GetMethod()))
	}
	if s.agentObs == nil {
		return // 未注入记录器（裸构造单测）：仅计数，不落库。
	}
	select {
	case s.agentObsSem <- struct{}{}:
	default:
		// 在途写入已满（PG 拥塞/落库变慢）：整批丢弃并计数——观测宁丢不积（有界 best-effort）。
		observability.IncStoreOpFailure("agent_activity_overflow")
		return
	}
	go func() {
		defer func() { <-s.agentObsSem }()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := s.agentObs.Insert(ctx, nodeID, events); err != nil {
			observability.IncStoreOpFailure("agent_activity")
			slog.Warn("record agent activity failed", "node_id", nodeID, "count", len(events), "err", err)
		}
	}()
}

// agentMethodLabel 把 JSON-RPC 方法归一到有界 Prometheus 标签集（客户端可发任意 method 串，直用会致
// 标签基数爆炸）。已知方法原样保留、notifications/* 归 notification、其余归 other。
func agentMethodLabel(method string) string {
	switch method {
	case "initialize", "tools/list", "tools/call", "resources/list", "prompts/list", "ping":
		return method
	}
	if strings.HasPrefix(method, "notifications/") {
		return "notification"
	}
	return "other"
}

// refreshHealth 尽力刷新节点在 Redis 的健康 TTL 键（失败仅记 debug，不阻断流）。
func (s *NodeControlServer) refreshHealth(ctx context.Context, nodeID string) {
	if s.redis == nil {
		return
	}
	if err := s.redis.Heartbeat(ctx, nodeID, nodeHealthTTL); err != nil {
		slog.Debug("redis health refresh failed", "node_id", nodeID, "err", err)
	}
}

// setOwner 尽力登记/续租 node→replica 归属键（SET EX 无 NX：重连覆盖语义——节点迁移到新副本时
// 新 Connect 直接改写 stale owner，ha-contract §1.1）。写路径 best-effort：失败仅记 debug，不断流
// （owner 设施降级 = T8 转发降级为现行 E_NODE_OFFLINE，不引入新故障面）——refreshHealth 同族。
func (s *NodeControlServer) setOwner(ctx context.Context, nodeID string) {
	if s.redis == nil {
		return
	}
	if err := s.redis.SetNodeOwner(ctx, nodeID, replicaID, nodeOwnerTTL); err != nil {
		slog.Debug("redis owner refresh failed", "node_id", nodeID, "replica_id", replicaID, "err", err)
	}
}

// clearOwner 尽力清理本副本登记的归属键（Lua compare-and-del：仍为本副本才删，防误清他副本已
// 接管改写的新归属）。独立短超时 ctx：调用点在流 defer 区，流 ctx 已随断开取消。
func (s *NodeControlServer) clearOwner(nodeID string) {
	if s.redis == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := s.redis.ClearNodeOwner(ctx, nodeID, replicaID); err != nil {
		slog.Debug("redis owner clear failed", "node_id", nodeID, "replica_id", replicaID, "err", err)
	}
}

// Connect 处理一条节点拨出的长驻双向流。
//
// 时序：上行首帧必为 Register -> 分配/确认 UUID -> 回 RegisterAck -> 建会话入表 ->
// 循环收帧（Heartbeat 刷新活跃时间 / ToolResponse 路由回等待方）。流结束即出表并关闭会话。
func (s *NodeControlServer) Connect(
	ctx context.Context,
	stream *connect.BidiStream[aurav1.NodeToController, aurav1.ControllerToNode],
) error {
	// 1) 首帧必为 Register。
	first, err := stream.Receive()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return nil
		}
		return err
	}
	reg := first.GetRegister()
	if reg == nil {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("first frame must be Register"))
	}

	// 2) 分配/确认 node_id（空则分配 UUID）+ 落库节点元数据（M12：name/label/location/network_zone/
	// cert_fp）。cert_fp 从 mTLS peer 证书取（peerCertFP 由 gRPC 端口 PeerCertFPMiddleware 注入 ctx；
	// 空=无 peer 证书/提取失败，store 侧 COALESCE 保留现值）。eff 为库回读的生效元数据，供会话以库
	// 权威值建 fleet 帧（重连+既有 console 编辑不被节点空自报抹除）。
	nodeID, eff, err := s.registry.Register(ctx, reg, peerCertFP(ctx))
	if err != nil {
		return connect.NewError(connect.CodePermissionDenied, err)
	}

	// 3) 建会话并入表；defer 保证流结束时清理。
	// contract_version 落地链（M6 MAJOR-1）：Register 帧 → 此处 NewSession 加参 → NodeSession.ContractVersion
	// → registry.List() 回填 NodeInfo → auractl node list 显示与偏斜告警。
	// M12 可读元数据落地链：Register 帧自报 name(hostname)/label/location/network_zone → 会话回填（Name/
	// NetworkZone 注册即定，label/location 取 eff 库权威值）→ nodeInfo/ListFleet → FleetPage 替裸 UUID。
	sess := registry.NewSession(nodeID, reg.GetPlatform(), reg.GetTools(), reg.GetContractVersion(), s.sendBuffer)
	sess.Name = eff.GetName()
	sess.NetworkZone = reg.GetNetworkZone()
	// M12 批B：节点自报系统辨识（os_version/ip_address）回填会话——仅内存态回填 NodeInfo 供 console 渐进
	// 披露，不落库（ip 敏感）。未滚更节点上报空串，NodeInfo 对应字段空（前端不显，兼容渐进滚更）。
	sess.OsVersion = reg.GetOsVersion()
	sess.IpAddress = reg.GetIpAddress()
	// M12 批D：基础设施标注回填会话（在线 fleet 帧免读库；持久面已由 registry.Register→UpsertNode 落
	// nodes 表，离线节点走 ListFleet 表分支回填——离线定位是这组字段的核心场景）。
	sess.RuntimeKind = reg.GetRuntimeKind()
	sess.InfraHost = reg.GetInfraHost()
	sess.Attached = reg.GetAttached()
	// 批E：节点二进制版本回填会话（滚更可见性；持久面同上由 UpsertNode 落库）。
	sess.NodeVersion = reg.GetNodeVersion()
	// M16：二进制宿主平台回填会话（SelfUpdateNode 制品选型判据；持久面同上由 UpsertNode 落库）。
	sess.HostPlatform = reg.GetHostPlatform()
	sess.SetMeta(eff.GetLabel(), eff.GetLocation())
	s.registry.Add(sess)
	defer func() {
		sess.Close()
		s.registry.Remove(sess)
		// 断流清理归属键（T6 三点位之三，ha-contract §1.2）。同副本重连覆盖保护：节点闪断重连本副本
		// 时新 Connect 已 SetNodeOwner，旧流的 defer 若无条件 Clear 会删掉新流刚写的键——仅当本副本
		// 已无该 node 会话才清理（镜像 registry.Remove 的 cur==s 会话指针比对惯例）。Get 复核后、DEL 前
		// 的 µs 级残余窗口由 15s 心跳续租自愈，接受不加锁。
		if _, ok := s.registry.Get(nodeID); !ok {
			s.clearOwner(nodeID)
		}
	}()

	// 4) 启动单一 writer goroutine，独占 stream.Send。
	writerCtx, cancelWriter := context.WithCancel(ctx)
	defer cancelWriter()
	go sess.RunWriter(writerCtx, stream.Send)

	// 5) 回 RegisterAck（经会话队列，走同一 writer）。
	ack := &aurav1.ControllerToNode{
		Payload: &aurav1.ControllerToNode_RegisterAck{
			RegisterAck: &aurav1.RegisterAck{NodeId: nodeID, Accepted: true},
		},
	}
	if err := sess.Send(ctx, ack); err != nil {
		return err
	}
	s.refreshHealth(ctx, nodeID)
	// Connect 登记归属（T6 三点位之一）：节点连上即认领 owner（无 NX——重连新副本改写 stale owner
	// 就是接管语义本身）。
	s.setOwner(ctx, nodeID)
	slog.Info("node registered", "node_id", nodeID, "platform", reg.GetPlatform())

	// 6) 收帧主循环。
	for {
		frame, err := stream.Receive()
		if err != nil {
			if errors.Is(err, io.EOF) {
				slog.Info("node disconnected", "node_id", nodeID)
				return nil
			}
			slog.Info("node stream error", "node_id", nodeID, "err", err)
			return err
		}
		switch {
		case frame.GetHeartbeat() != nil:
			sess.Touch()
			s.refreshHealth(ctx, nodeID)
			// 心跳续租归属（T6 三点位之二）：SET 幂等即续租，单方法两用（ha-contract §1.2）。
			s.setOwner(ctx, nodeID)
		case frame.GetToolResponse() != nil:
			sess.Touch()
			sess.DeliverResponse(frame.GetToolResponse())
		case frame.GetMcpProxyResponse() != nil:
			// M14：MCP 网关代理响应——依 request_id 路由回网关等待方（哑管道，不解释 body）。
			sess.Touch()
			sess.DeliverMcpResponse(frame.GetMcpProxyResponse())
		case frame.GetSelfUpdateResult() != nil:
			// M16：self-update 结果回执——路由回 SelfUpdateNode 等待方。ok=true 意味着节点已换刀、
			// 随即重启（本流将断，重注册走新流）；失败时现网二进制未动，错误上抛调用方。
			sess.Touch()
			sur := frame.GetSelfUpdateResult()
			slog.Info("node self-update result", "node_id", nodeID, "version", sur.GetVersion(), "ok", sur.GetOk(), "err", sur.GetError())
			sess.DeliverSelfUpdateResult(sur)
		case frame.GetUploadComplete() != nil:
			// 大产物旁路上传完成回执（G-5）：记审计并 resolve 等待中的 GrantAndAwait。产物本体已经
			// 预签名 PUT 落对象存储，经 resource_link / auractl artifact get 取回；此处不再搬运字节
			// （绕开 16MB 内联上限），仅以 (node_id,key) 复合键唤醒 needs_upload 路径的同步等待方。
			sess.Touch()
			uc := frame.GetUploadComplete()
			slog.Info("node bypass upload complete", "node_id", nodeID, "key", uc.GetKey(), "etag", uc.GetEtag())
			s.resolveUpload(nodeID, uc.GetKey(), nil)
			// M12 批C：录屏对象补记源节点映射（先 resolve 再记账，不给 dispatch 返回路径添延迟）。
			// 批E：收帧臂无 dispatch 上下文，task/trace 空兜底写（全量关联在 GrantAndAwait 完成点）。
			s.recordUploadMeta(nodeID, uc.GetKey(), "", "")
		case frame.GetUploadFailed() != nil:
			// 旁路上传失败即时通知（T10，UploadComplete 对偶帧）：以节点错误 resolve 等待方，令
			// gateway 秒级感知降级（免等满 awaitUploadTimeout 兜底窗）。帧本身 best-effort：节点
			// 发不出（流已断）时兜底超时仍独立生效。resolve 全在 owner 副本（ha-contract §2.2）。
			sess.Touch()
			uf := frame.GetUploadFailed()
			slog.Warn("node bypass upload failed", "node_id", nodeID, "key", uf.GetKey(), "err", uf.GetError())
			s.resolveUpload(nodeID, uf.GetKey(), errors.New(uf.GetError()))
		case frame.GetAgentActivity() != nil:
			// M13 直连 MCP agent 观测：节点 /mcp 中间件采集的直连 agent 活动批量帧。落库/入内存缓冲供
			// console「接入观测」读 + 打 Prometheus 计数。观测旁路：不 Touch（非节点健康信号，直连
			// agent 活动经 http 面、与反连心跳无关）、best-effort（记录失败仅告警，绝不断反连流）。
			s.recordAgentActivity(nodeID, frame.GetAgentActivity())
		default:
			// 忽略重复 Register 等非预期上行帧。
		}
	}
}

// ===== mTLS peer 证书指纹（M12：cert_fp 兑现写入）=====
//
// nodes.cert_fp 列 M2 起在场但从不写。M12 在 Register 落库时从 mTLS peer 证书回填其 SHA256 指纹
// （通用证书 CN=aura-node 也回填兼容；per-node 证书 TASK-006 上线后 fp 精确、可反查吊销）。
// connect-go v1.18 的 BidiStream handler 不直接暴露底层 TLS state，故在 http 层从 r.TLS.PeerCertificates
// 提取并注入请求 ctx——connect handler ctx 派生自 http 请求 ctx，注入值向下贯通至 Connect。

// peerCertFPKeyT 是 ctx 键类型（unexported 空结构，杜绝跨包键碰撞）。
type peerCertFPKeyT struct{}

var peerCertFPKey = peerCertFPKeyT{}

// PeerCertFPMiddleware 在 mTLS gRPC 入站请求 ctx 注入客户端叶证书 SHA256 指纹（hex），供 Connect
// handler 经 peerCertFP 取回落库（main.go newGRPCServer 包裹 NodeControl handler）。无 peer 证书
// （RequireAndVerifyClientCert 下理论不发生）时不注入，peerCertFP 返空、store COALESCE 保留现值。
func PeerCertFPMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS != nil && len(r.TLS.PeerCertificates) > 0 {
			fp := certFingerprint(r.TLS.PeerCertificates[0])
			r = r.WithContext(context.WithValue(r.Context(), peerCertFPKey, fp))
		}
		next.ServeHTTP(w, r)
	})
}

// certFingerprint 计算证书 DER（cert.Raw）的 SHA256 指纹（hex 小写），与 CA 签发台账 node_certs.cert_fp
// 同口径（TASK-002 design §3.4：cert_fp = hex(SHA256(cert.Raw))）。
func certFingerprint(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.Raw)
	return hex.EncodeToString(sum[:])
}

// peerCertFP 从请求 ctx 取回 mTLS peer 证书指纹（PeerCertFPMiddleware 注入）；未注入返空串。
func peerCertFP(ctx context.Context) string {
	fp, _ := ctx.Value(peerCertFPKey).(string)
	return fp
}
