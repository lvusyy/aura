// Package registry 管理在线反连节点的会话集合。
//
// 会话仅驻内存：一条会话对应一条活跃的 gRPC 双向流，流不可持久化，故节点掉线即出表。
// 节点元数据（node_id/platform/labels 等）的持久化由 Store 接口承接（TASK-004 接 PostgreSQL），
// 本包在 store 为 nil 时以纯内存自洽运行。
package registry

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"

	aurav1 "github.com/aura/controller/gen/aura/v1"
	"github.com/aura/controller/internal/observability"
)

// ErrNodeGone 表示会话对应的节点流已结束（掉线），等待中的下发/响应据此解除阻塞。
var ErrNodeGone = errors.New("node session closed")

// Store 抽象节点元数据持久化。M2 骨架允许为 nil（纯内存）；TASK-004 用 PostgreSQL 实现。
type Store interface {
	// UpsertNode 落库/更新一个节点的元数据（首次注册与重连均调用）。NodeInfo 携带 node_id/platform/
	// status/name/label/location；hostname（自报原始名，审计列）/networkZone（探测网络域，T07 presigned
	// 分派用）/certFP（mTLS peer 证书指纹）不在 NodeInfo 契约面故独立传参。返回回读的生效元数据
	// （name/label/location），供会话以库权威值建 fleet 帧（重连+既有 console 编辑不被节点空自报抹除）。
	UpsertNode(ctx context.Context, node *aurav1.NodeInfo, hostname, networkZone, certFP string) (*aurav1.NodeInfo, error)
	// ListNodes 读全部持久节点身份为 NodeInfo（offline 展示源，ListFleet 合并底本）。
	ListNodes(ctx context.Context) ([]*aurav1.NodeInfo, error)
	// UpdateNodeMeta 更新节点 label/location/project（console 编辑权威路径），返回是否命中行。
	// M15：project 取 presence 语义（*string；nil=不改归属）。
	UpdateNodeMeta(ctx context.Context, nodeID, label, location string, project *string) (bool, error)
	// ReapOfflineNodes 删除 last_seen < before 且不在 protected（当前活跃会话）集合的节点身份台账，返回被删
	// 节点 ID（舰队治理自动遗忘长期离线僵尸，M12-P1）。protected 排除令长驻在线节点绝不被误删（last_seen 仅
	// Register 刷新，长会话可 stale——活跃会话才是「在线」权威信号）。
	ReapOfflineNodes(ctx context.Context, before time.Time, protected []string) ([]string, error)
}

// NodeSession 是单个反连节点的会话，被建模为一个 actor：
//   - 下行帧经 sendCh 汇聚，由 transport 提供的单一 writer goroutine（见 RunWriter）串行发送，
//     规避 connect BidiStream.Send 的非并发安全问题；
//   - 上行 ToolResponse 依 task_id 经 pending 关联回等待方（见 Dispatch / DeliverResponse）。
//
// NodeID / Platform / Tools / ContractVersion / Name / NetworkZone 在创建后（Add 发布前设妥）不可变，
// 可无锁读取；label / location 为 console 可编辑元数据（UpdateNodeMeta 改），故经 mu 守护读写。
type NodeSession struct {
	NodeID          string
	Platform        string
	Tools           []string
	ContractVersion string
	// M12 节点可读元数据（注册即定，随 NodeID 无锁读）：Name=节点自报 hostname（fleet 替裸 UUID）；
	// NetworkZone=节点探测上报的可达网络域（T07 presigned 分派消费 + M12 批B fleet 面回填）。
	Name        string
	NetworkZone string
	// M12 批B（节点元数据扩展，注册即定无锁读）：OsVersion=节点自报简短系统版本（node 采集）；IpAddress=
	// 节点自报主内网 IP（敏感，仅内存会话回填 NodeInfo 不入库）；connectedAt=会话建立时刻（在线时长单一
	// 源，NewSession 设一次不可变——区别于随 Touch 漂移的 lastSeen）。
	OsVersion   string
	IpAddress   string
	connectedAt time.Time
	// M12 批D（基础设施标注，注册即定无锁读）：RuntimeKind=运行形态（k8s|container|vm|baremetal）；
	// InfraHost=宿主链（"<host>[/<ns>/<pod>]"）；Attached=派生服务标识。三者同时落 nodes 表（离线
	// 定位），在线会话经此回填 NodeInfo 免读库。
	RuntimeKind string
	InfraHost   string
	Attached    string
	// 批E（滚更可见性，注册即定无锁读）：节点二进制版本自报（CARGO_PKG_VERSION），同落 nodes 表
	// （离线成员滚更盘点经 ListFleet 表分支回填最后已知版本）。
	NodeVersion string
	// M16（self-update，注册即定无锁读）：二进制宿主平台自报（{OS}-{ARCH}，发布制品选型判据——
	// platform 是设备类，android 节点的二进制实际跑在 linux 宿主）。同落 nodes 表。
	HostPlatform string

	mu       sync.RWMutex
	lastSeen time.Time
	// label / location：console 可编辑元数据，会话建时以库权威值（Register 经 UpsertNode RETURNING 取）
	// 初始化，UpdateNodeMeta 经 SetMeta 同步——令 fleet 帧（快照 ListFleet + 增量 broadcast）始终携库
	// 一致 label/location（消除节点重连自报空值覆盖既有 console 编辑的 stale 展示）。
	label    string
	location string

	// sendCh 是下行帧队列；仅 RunWriter 消费，保证单一发送者。
	sendCh chan *aurav1.ControllerToNode

	// pending 关联 task_id -> 等待 ToolResponse 的 channel（请求/响应关联）。
	pendingMu sync.Mutex
	pending   map[string]chan *aurav1.ToolResponse
	// mcpPending 关联 request_id -> 等待 McpProxyResponse 的 channel（M14 网关代理，与 pending
	// 同锁不同表：两类响应 id 空间独立，混表会让哑管道请求依赖 ToolResponse 类型）。
	mcpPending map[string]chan *aurav1.McpProxyResponse
	// selfUpdatePending 等待 SelfUpdateResult 的单槽（M16；同 pendingMu 守护）：per-node 单飞——
	// 同一节点同时至多一个 self-update 在途（结果帧无关联 id，单槽即天然闸）。nil=无在途。
	selfUpdatePending chan *aurav1.SelfUpdateResult

	// done 在会话关闭时闭合，唤醒阻塞在 Send / Dispatch 上的调用方。
	done      chan struct{}
	closeOnce sync.Once
}

// NewSession 建一个新会话。contractVersion 为节点上报的契约版本（Register.contract_version，
// 空串表示节点未上报）；sendBuffer 是下行帧队列容量（0 时退化为无缓冲）。
func NewSession(nodeID, platform string, tools []string, contractVersion string, sendBuffer int) *NodeSession {
	if sendBuffer < 0 {
		sendBuffer = 0
	}
	now := time.Now()
	return &NodeSession{
		NodeID:          nodeID,
		Platform:        platform,
		Tools:           tools,
		ContractVersion: contractVersion,
		lastSeen:        now,
		connectedAt:     now, // M12 批B：会话建立时刻（在线时长源，不可变）
		sendCh:          make(chan *aurav1.ControllerToNode, sendBuffer),
		pending:         make(map[string]chan *aurav1.ToolResponse),
		mcpPending:      make(map[string]chan *aurav1.McpProxyResponse),
		done:            make(chan struct{}),
	}
}

// Touch 刷新最近活跃时间（收到 Heartbeat 或任意上行帧时调用）。
func (s *NodeSession) Touch() {
	s.mu.Lock()
	s.lastSeen = time.Now()
	s.mu.Unlock()
}

// LastSeen 返回最近活跃时间。
func (s *NodeSession) LastSeen() time.Time {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.lastSeen
}

// SetMeta 设置 console 可编辑元数据 label/location（会话建时初始化 + UpdateNodeMeta 编辑时同步）。
func (s *NodeSession) SetMeta(label, location string) {
	s.mu.Lock()
	s.label = label
	s.location = location
	s.mu.Unlock()
}

// metaSnapshot 一次 RLock 取会话可变态（lastSeen + label + location），供 nodeInfo 组装免多次加锁。
func (s *NodeSession) metaSnapshot() (lastSeen time.Time, label, location string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.lastSeen, s.label, s.location
}

// Send 将一帧下行消息入队，交由 writer goroutine 串行发送。
// ctx 取消或会话关闭时立即返回，避免在队列满时无限阻塞。
func (s *NodeSession) Send(ctx context.Context, frame *aurav1.ControllerToNode) error {
	select {
	case s.sendCh <- frame:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-s.done:
		return ErrNodeGone
	}
}

// Dispatch 向节点下发一次工具请求并等待对应 ToolResponse，是 scheduler（TASK-006）的下发入口。
// 依 task_id 关联响应；ctx 到期（控制面兜底 timer）或节点掉线时返回错误。
func (s *NodeSession) Dispatch(ctx context.Context, req *aurav1.ToolRequest) (*aurav1.ToolResponse, error) {
	taskID := req.GetTaskId()
	if taskID == "" {
		return nil, errors.New("tool request requires task_id")
	}

	ch := make(chan *aurav1.ToolResponse, 1)
	s.pendingMu.Lock()
	s.pending[taskID] = ch
	s.pendingMu.Unlock()
	defer func() {
		s.pendingMu.Lock()
		delete(s.pending, taskID)
		s.pendingMu.Unlock()
	}()

	frame := &aurav1.ControllerToNode{
		Payload: &aurav1.ControllerToNode_ToolRequest{ToolRequest: req},
	}
	if err := s.Send(ctx, frame); err != nil {
		return nil, err
	}

	select {
	case resp := <-ch:
		return resp, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.done:
		return nil, ErrNodeGone
	}
}

// ProxyMcp 向节点下发一次 MCP 网关代理请求并等待 McpProxyResponse（M14，transport 网关入口）。
// 复刻 Dispatch 的 pending 模板：依 request_id 关联；ctx 到期（网关兜底 timer）或节点掉线时返回错误。
func (s *NodeSession) ProxyMcp(ctx context.Context, req *aurav1.McpProxyRequest) (*aurav1.McpProxyResponse, error) {
	requestID := req.GetRequestId()
	if requestID == "" {
		return nil, errors.New("mcp proxy request requires request_id")
	}

	ch := make(chan *aurav1.McpProxyResponse, 1)
	s.pendingMu.Lock()
	s.mcpPending[requestID] = ch
	s.pendingMu.Unlock()
	defer func() {
		s.pendingMu.Lock()
		delete(s.mcpPending, requestID)
		s.pendingMu.Unlock()
	}()

	frame := &aurav1.ControllerToNode{
		Payload: &aurav1.ControllerToNode_McpProxyRequest{McpProxyRequest: req},
	}
	if err := s.Send(ctx, frame); err != nil {
		return nil, err
	}

	select {
	case resp := <-ch:
		return resp, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.done:
		return nil, ErrNodeGone
	}
}

// SelfUpdate 向节点下发 self-update 指令并等待 SelfUpdateResult（M16，SelfUpdateNode 管理面入口）。
// 单槽 pending（结果帧无关联 id）：同节点已有在途 self-update 即拒绝（per-node 单飞闸）；
// ctx 到期（调用方兜底 timer）或节点掉线时返回错误。ok=true 的结果意味着节点已换刀、随即重启重注册。
func (s *NodeSession) SelfUpdate(ctx context.Context, req *aurav1.SelfUpdate) (*aurav1.SelfUpdateResult, error) {
	ch := make(chan *aurav1.SelfUpdateResult, 1)
	s.pendingMu.Lock()
	if s.selfUpdatePending != nil {
		s.pendingMu.Unlock()
		return nil, errors.New("self-update already in flight for this node")
	}
	s.selfUpdatePending = ch
	s.pendingMu.Unlock()
	defer func() {
		s.pendingMu.Lock()
		if s.selfUpdatePending == ch {
			s.selfUpdatePending = nil
		}
		s.pendingMu.Unlock()
	}()

	frame := &aurav1.ControllerToNode{
		Payload: &aurav1.ControllerToNode_SelfUpdate{SelfUpdate: req},
	}
	if err := s.Send(ctx, frame); err != nil {
		return nil, err
	}

	for {
		select {
		case resp := <-ch:
			// 晚到串扰防护：结果帧无关联 id，仅 version 回声可辨——上一轮超时后节点补发的旧回执
			// 与本请求版本不符即丢弃续等，防止旧结果冒充本轮答复（单槽跨请求残留唯一入口）。
			if resp.GetVersion() != req.GetVersion() {
				continue
			}
			return resp, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-s.done:
			return nil, ErrNodeGone
		}
	}
}

// DeliverSelfUpdateResult 将收到的 SelfUpdateResult 路由回等待方（transport 收帧循环调用）。
// 无人等待或已交付时静默丢弃（同 DeliverResponse 纪律）。
func (s *NodeSession) DeliverSelfUpdateResult(resp *aurav1.SelfUpdateResult) {
	s.pendingMu.Lock()
	ch := s.selfUpdatePending
	s.pendingMu.Unlock()
	if ch == nil {
		return
	}
	select {
	case ch <- resp:
	default:
	}
}

// DeliverMcpResponse 将收到的 McpProxyResponse 路由回等待方（transport 收帧循环调用）。
// 无人等待或已交付时静默丢弃（同 DeliverResponse 纪律）。
func (s *NodeSession) DeliverMcpResponse(resp *aurav1.McpProxyResponse) {
	s.pendingMu.Lock()
	ch, ok := s.mcpPending[resp.GetRequestId()]
	s.pendingMu.Unlock()
	if !ok {
		return
	}
	select {
	case ch <- resp:
	default:
	}
}

// DeliverResponse 将收到的 ToolResponse 路由回等待方（transport 收帧循环调用）。
// 无人等待或已交付时静默丢弃。
func (s *NodeSession) DeliverResponse(resp *aurav1.ToolResponse) {
	s.pendingMu.Lock()
	ch, ok := s.pending[resp.GetTaskId()]
	s.pendingMu.Unlock()
	if !ok {
		return
	}
	select {
	case ch <- resp:
	default:
	}
}

// RunWriter 独占消费 sendCh 并调用 send 发送下行帧，须由 transport 在独立 goroutine 中运行，
// send 参数即该流的发送原语（如 connect BidiStream.Send）。
// ctx 取消、发送失败或会话关闭时返回。
func (s *NodeSession) RunWriter(ctx context.Context, send func(*aurav1.ControllerToNode) error) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.done:
			return
		case frame := <-s.sendCh:
			if err := send(frame); err != nil {
				// 发送失败通常意味着流已断，退出 writer；收帧循环会随后清理会话。
				return
			}
		}
	}
}

// Close 关闭会话（幂等），解除所有阻塞的 Send / Dispatch。
func (s *NodeSession) Close() {
	s.closeOnce.Do(func() { close(s.done) })
}

// NodeRegistry 维护 node_id -> 在线会话 的映射。
type NodeRegistry struct {
	mu       sync.RWMutex
	sessions map[string]*NodeSession

	// store 为 nil 时纯内存运行；非 nil 时由 TASK-004 落库节点元数据。
	store Store

	// unhealthyAfter 决定 List 中 online/unhealthy 的分界。
	unhealthyAfter time.Duration

	// onRemove 是节点会话被移除时的可选回调（node_id 传入）。装配期一次性注入（SetRemovalHook）、
	// 此后只读，故无需锁保护。用于让 scheduler 回收 per-node 队列/指标而不使 registry 反向 import
	// scheduler（维持 M2 分层：闭包注入，方向由 main.go 装配）。nil 时 Remove 为纯出表。
	onRemove func(nodeID string)

	// --- 舰队状态变更广播（M8 WatchFleet 事件源）---
	// registry 是节点状态单一真源，却无现成 Watch/Notify；此处新建 observer 注册表：Subscribe 注册有界
	// chan，Add/Remove 与 status tick 经 broadcast 扇出增量事件。分层沿 SetRemovalHook 先例——事件类型
	// FleetEvent 在本包定义（不反向 import transport），handler 侧转 proto。
	// 锁序纪律：obsMu 独立于 mu，且 broadcast 恒在 mu 释放后调用（见 Add/Remove），二者从不嵌套持有，
	// 故与 List(mu) 无死锁面。
	obsMu     sync.Mutex
	observers map[uint64]chan FleetEvent
	nextObsID uint64
	evSeq     int64 // 单调递增事件序号（obsMu 下自增）；跳号=慢消费者被丢弃，客户端据此重拉快照重同步
	dropped   int64 // 慢消费者累计丢弃事件数（obsMu 下自增）；DroppedEvents 暴露供观测/单测
}

// FleetEvent 是舰队状态变更的内部事件（observer chan 的载荷）。Type 用语义串而非 proto 枚举，令 registry
// 与 aura.v1 proto 解耦——WatchFleet handler 侧再转 aurav1.FleetEvent（criterion：handler 侧转 proto）。
// Node 为增量事件（add/remove/status_changed）的目标节点快照；全量首帧快照由 handler 直接经 List() 构造，
// 不经本事件流（故无 Snapshot 字段，保持事件轻量）。
type FleetEvent struct {
	Seq  int64
	Type string
	Node *aurav1.NodeInfo
}

// 舰队事件语义类型（WatchFleet handler 映射到 aurav1.FleetEventType）。
const (
	EventNodeAdded     = "node_added"
	EventNodeRemoved   = "node_removed"
	EventStatusChanged = "status_changed"
)

// observerBufSize 是每个订阅者有界 chan 的容量。满即丢弃本次事件（慢消费者绝不阻塞 registry 核心写路径
// ——R6 不变量 / 对抗 D）；客户端经 seq 跳号检测后重拉 ListNodes 全量快照重同步。
const observerBufSize = 64

// 默认超过 30s 无心跳判为 unhealthy（应用层心跳周期的数倍冗余）。
const defaultUnhealthyAfter = 30 * time.Second

// NewRegistry 建注册表。store 可为 nil。
func NewRegistry(store Store) *NodeRegistry {
	return &NodeRegistry{
		sessions:       make(map[string]*NodeSession),
		observers:      make(map[uint64]chan FleetEvent),
		store:          store,
		unhealthyAfter: defaultUnhealthyAfter,
	}
}

// SetRemovalHook 注册节点会话移除回调（装配期一次性调用，早于任何连接进入 Remove）。
// hook 在会话确被移除后、registry 锁外被调用，接收被移除的 node_id。用于 scheduler 回收 per-node
// 队列与队列深度指标序列（main.go 装配 reg.SetRemovalHook(sched.ReclaimNode)），使 registry 无需
// import scheduler（保持依赖单向）。
func (r *NodeRegistry) SetRemovalHook(hook func(nodeID string)) {
	r.onRemove = hook
}

// Register 处理注册首帧，返回最终 node_id 与生效元数据快照（供调用方以库权威值建会话）。
// node_id 为空视为首次注册，分配新 UUID（禁机器指纹——PVE clone 会趋同）；非空视为重连，原样沿用。
// certFP 为 mTLS peer 证书指纹（grpc.go 从 TLS state 取，空=无 peer 证书/提取失败——UpsertNode
// COALESCE 保留现值不抹）。store 非 nil 时落库并回读生效 name/label/location（重连+既有 console 编辑时，
// 节点自报空 label 不覆盖库值，会话据 eff 持库权威值）；nil 时纯内存，eff 直取 Register 自报值。
func (r *NodeRegistry) Register(ctx context.Context, reg *aurav1.Register, certFP string) (string, *aurav1.NodeInfo, error) {
	if reg == nil {
		return "", nil, errors.New("nil register frame")
	}
	nodeID := reg.GetNodeId()
	if nodeID == "" {
		nodeID = uuid.NewString()
	}
	// 落库输入：name 取节点自报 hostname（Register.name）；label/location 取节点 --label/--location 引导值；
	// hostname 列独立存自报原始名（不可变审计）；network_zone/cert_fp 见 store.UpsertNode authority 分层。
	info := &aurav1.NodeInfo{
		NodeId:     nodeID,
		Platform:   reg.GetPlatform(),
		Status:     "online",
		LastSeenMs: time.Now().UnixMilli(),
		Name:       reg.GetName(),
		Label:      reg.GetLabel(),
		Location:   reg.GetLocation(),
		// M12 批D：基础设施标注自报（形态/宿主链/派生服务）——UpsertNode 落库（EXCLUDED 优先，
		// 空报保留现值），离线定位核心场景（ListNodes 表分支回填）。
		RuntimeKind: reg.GetRuntimeKind(),
		InfraHost:   reg.GetInfraHost(),
		Attached:    reg.GetAttached(),
		// 批E：节点二进制版本自报落库（滚更进度盘点）。
		NodeVersion: reg.GetNodeVersion(),
		// M16：二进制宿主平台自报落库（rollout 制品选型 + 离线漂移盘点）。
		HostPlatform: reg.GetHostPlatform(),
	}
	if r.store == nil {
		return nodeID, info, nil // 纯内存：eff 直取自报值
	}
	eff, err := r.store.UpsertNode(ctx, info, reg.GetName(), reg.GetNetworkZone(), certFP)
	if err != nil {
		return "", nil, err
	}
	return nodeID, eff, nil
}

// Add 登记一条在线会话（重连时覆盖旧条目）。
func (r *NodeRegistry) Add(s *NodeSession) {
	r.mu.Lock()
	r.sessions[s.NodeID] = s
	r.mu.Unlock()
	// 批E：节点在线态 gauge（aura_node_up）——连上即 1；掉线置 0（Remove）、健康迁移由 WatchStatus
	// tick 刷新。序列删除归身份注销点（DeleteNode/reap），离线保留 0 值恰是告警消费面。
	observability.SetNodeUp(s.NodeID, s.Platform, true)
	// 广播 node_added（在 mu 外，沿 onRemove 先例：不在 registry 锁内触发扇出）。刚登记，快照按 online。
	r.broadcast(EventNodeAdded, r.nodeInfo(s))
}

// Remove 注销会话，仅当当前条目确为 s 时删除（避免重连竞态误删新会话）。
// 会话确被移除时触发 onRemove 回调（在 mu 外调用，避免与 scheduler 锁交叉持有致死锁）；
// 重连竞态下当前条目已非 s（未删除）则不回调，防误回收新会话的队列。
func (r *NodeRegistry) Remove(s *NodeSession) {
	r.mu.Lock()
	removed := false
	if cur, ok := r.sessions[s.NodeID]; ok && cur == s {
		delete(r.sessions, s.NodeID)
		removed = true
	}
	r.mu.Unlock()
	if !removed {
		return
	}
	// 批E：断流即置 aura_node_up=0（保留序列供 up==0 告警；序列删除归身份注销点）。
	observability.SetNodeUp(s.NodeID, s.Platform, false)
	if r.onRemove != nil {
		r.onRemove(s.NodeID)
	}
	// 广播 node_removed（在 mu 外，与 onRemove 同侧）。节点已出表，快照记 offline（无活跃流）。M12：携
	// 可读元数据（Name/Label/Location）——令增量帧与 ListFleet 快照 NodeInfo 同形，前端按 node_id upsert
	// 时不因 node_removed 缺字段而清空展示名/标签。
	last, label, location := s.metaSnapshot()
	r.broadcast(EventNodeRemoved, &aurav1.NodeInfo{
		NodeId:     s.NodeID,
		Platform:   s.Platform,
		Status:     "offline",
		LastSeenMs: last.UnixMilli(),
		Name:       s.Name,
		Label:      label,
		Location:   location,
	})
}

// Get 按 node_id 取在线会话。
func (r *NodeRegistry) Get(nodeID string) (*NodeSession, bool) {
	r.mu.RLock()
	s, ok := r.sessions[nodeID]
	r.mu.RUnlock()
	return s, ok
}

// Ready 返回可下发的在线会话；不存在或超过 unhealthy 阈值未活跃时 ok=false。
// 与 List 共用同一阈值，作为 scheduler 的单一就绪判据。
func (r *NodeRegistry) Ready(nodeID string) (*NodeSession, bool) {
	r.mu.RLock()
	s, ok := r.sessions[nodeID]
	r.mu.RUnlock()
	if !ok {
		return nil, false
	}
	if time.Since(s.LastSeen()) > r.unhealthyAfter {
		return nil, false
	}
	return s, true
}

// nodeInfo 快照单个会话为 NodeInfo，status 依最近活跃时间判 online/unhealthy（与 List 同口径）。
// 供 List / ListFleet / broadcast 复用（DRY）；自身不取 r.mu，调用方按需持锁（List/ListFleet 持
// r.mu.RLock；broadcast 在 mu 外）。label/location 经 metaSnapshot 一次 s.mu.RLock 取（会话可编辑态）。
func (r *NodeRegistry) nodeInfo(s *NodeSession) *aurav1.NodeInfo {
	last, label, location := s.metaSnapshot()
	status := "online"
	if time.Since(last) > r.unhealthyAfter {
		status = "unhealthy"
	}
	return &aurav1.NodeInfo{
		NodeId:          s.NodeID,
		Platform:        s.Platform,
		Status:          status,
		LastSeenMs:      last.UnixMilli(),
		Tools:           s.Tools,           // 回填能力子集（NodeSession.Tools 既存态，TASK-003 driver 过滤后子集）
		ContractVersion: s.ContractVersion, // 回填契约版本（fleet 面消费 + auractl 偏斜告警）
		Name:            s.Name,            // M12 节点自报可读名（fleet 替裸 UUID）
		Label:           label,             // M12 用户标签（库同步，console 编辑权威）
		Location:        location,          // M12 用户位置（库同步）
		// M12 批B（节点元数据扩展，在线会话回填）：OsVersion/IpAddress 节点自报（Phase3 node 采集，
		// 未滚更节点空）；NetworkZone 会话既定（node 探测上报，含未滚更节点）；ConnectedAtMs 会话建立时刻
		// （console 算 now−此=本次在线时长）。离线节点走 ListFleet 表分支（无会话，os/ip/conn 空，zone 取表）。
		OsVersion:     s.OsVersion,
		IpAddress:     s.IpAddress,
		NetworkZone:   s.NetworkZone,
		ConnectedAtMs: s.connectedAt.UnixMilli(),
		// M12 批D：基础设施标注（在线会话回填；离线走 ListFleet 表分支——三字段已持久化 nodes 表）。
		RuntimeKind: s.RuntimeKind,
		InfraHost:   s.InfraHost,
		Attached:    s.Attached,
		// 批E：节点二进制版本（在线会话回填；离线走 ListFleet 表分支回填最后已知版本）。
		NodeVersion: s.NodeVersion,
		// M16：二进制宿主平台（在线会话回填；离线走 ListFleet 表分支——已持久化 nodes 表）。
		HostPlatform: s.HostPlatform,
	}
}

// List 快照所有在线会话为 NodeInfo，status 依最近活跃时间判 online/unhealthy。仅内存在线会话（不含
// offline）——会话数语义（metrics GaugeFunc / WatchStatus tick 消费）恒定；含 offline 的舰队全集展示走
// ListFleet（M12 加，读 nodes 表持久身份）。
func (r *NodeRegistry) List() []*aurav1.NodeInfo {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*aurav1.NodeInfo, 0, len(r.sessions))
	for _, s := range r.sessions {
		out = append(out, r.nodeInfo(s))
	}
	return out
}

// ListFleet 快照全舰队（在线会话 + nodes 表 offline 持久身份），供 WatchFleet 快照帧展示 offline 节点
// 可读名（M12：FleetPage 不再只见在线节点）。合并口径：nodes 表为全集底本，每行有活跃会话则以会话
// live 态呈现（status online/unhealthy + tools/contract_version + 库同步的 label/location），否则置
// offline（元数据/last_seen 取表，不信表内 stale online status——断流不改表）；会话在表外（注册落库与
// Add 间瞬时窗）者并入防漏。store 为 nil（纯内存）或读故障时降级为在线-only（读降级：offline 展示是
// 装饰面，不因表读故障断 fleet 流——同 console_fleet fleetRecordings 惯例）。
func (r *NodeRegistry) ListFleet(ctx context.Context) []*aurav1.NodeInfo {
	// 在线会话快照（持 r.mu.RLock 组装 nodeInfo，锁外再读库，不在 registry 锁内做 DB IO）。
	r.mu.RLock()
	online := make(map[string]*aurav1.NodeInfo, len(r.sessions))
	for id, s := range r.sessions {
		online[id] = r.nodeInfo(s)
	}
	r.mu.RUnlock()

	if r.store == nil {
		return mapValues(online)
	}
	persisted, err := r.store.ListNodes(ctx)
	if err != nil {
		slog.Warn("fleet list: nodes read failed; degrading to online-only", "err", err)
		return mapValues(online)
	}

	out := make([]*aurav1.NodeInfo, 0, len(persisted)+len(online))
	seen := make(map[string]struct{}, len(persisted))
	for _, row := range persisted {
		id := row.GetNodeId()
		seen[id] = struct{}{}
		if live, ok := online[id]; ok {
			out = append(out, live) // 在线：live 态（label/location 已由会话与库同步）
		} else {
			row.Status = "offline" // 无活跃会话：一律 offline（不信表内 stale status）
			out = append(out, row)
		}
	}
	for id, live := range online { // 会话尚未入表（注册落库与展示间竞态）者并入，防漏
		if _, ok := seen[id]; !ok {
			out = append(out, live)
		}
	}
	return out
}

// UpdateNodeMeta 更新节点 label/location（console 编辑权威）：落库 + 同步在线会话缓存 + 广播 fleet
// 事件令 console 实时刷新（M8 FleetEvent 先例）。返回是否命中（node_id 库中不存在返 false，供 handler
// 区分 not-found）。store 为 nil（纯内存）时不支持编辑（返错，handler 转 Unavailable）。离线节点（无
// 会话）仅落库，实时刷新靠 WatchFleet 周期快照兜底（≤30s）——编辑离线节点非时敏，前端亦可乐观更新。
// M15：project 取 presence 语义（*string，proto optional 对位）——nil=不改归属，非 nil（含空串=清除）
// 即写入。归属是持久列，在线会话缓存不承载 project（fleet 帧按库侧集合过滤，见 filterNodesByProject），
// 故命中在线节点时仅同步 label/location 缓存、project 只落库不进会话缓存。
func (r *NodeRegistry) UpdateNodeMeta(ctx context.Context, nodeID, label, location string, project *string) (bool, error) {
	if r.store == nil {
		return false, errors.New("node metadata store not configured")
	}
	updated, err := r.store.UpdateNodeMeta(ctx, nodeID, label, location, project)
	if err != nil || !updated {
		return updated, err
	}
	// 命中：在线会话同步缓存并广播（令快照与增量携一致 label/location）。复用 status_changed 事件类型
	// （载荷 NodeInfo 带新 label/location 即元数据变更信号，前端按 node_id 幂等 upsert，无需新增枚举）。
	if s, ok := r.Get(nodeID); ok {
		s.SetMeta(label, location)
		r.broadcast(EventStatusChanged, r.nodeInfo(s))
	}
	return true, nil
}

// mapValues 取 map 全部值为切片（ListFleet 纯内存/读降级分支复用）。
func mapValues(m map[string]*aurav1.NodeInfo) []*aurav1.NodeInfo {
	out := make([]*aurav1.NodeInfo, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	return out
}

// Subscribe 注册一个舰队事件订阅者，返回有界事件 chan、订阅时刻的基线 seq、退订闭包。
// 基线 seq 供 WatchFleet 首帧快照携带：注册 chan 与读 evSeq 为单一临界区（obsMu），此后任何 broadcast 的
// seq 必 > 基线——首帧快照（seq=基线）与后续增量事件（seq>基线）严格有序，慢消费者被丢弃即在客户端表现为
// seq 跳号（重同步信号，对抗 D）。订阅先于取快照 + 客户端按 node_id 幂等 upsert，令快照与增量至多重叠、绝不丢事件。
// cancel 幂等：仅从 observers 删除本订阅者，不 close chan——broadcast 是唯一写方且与删除同持 obsMu，删除后
// 不再写，chan 随 handler 释放引用被 GC，从根本规避 send-on-closed panic。handler 在 ctx 取消/流断时 defer
// cancel 退订，无 goroutine / chan 泄漏。
func (r *NodeRegistry) Subscribe() (events <-chan FleetEvent, snapshotSeq int64, cancel func()) {
	r.obsMu.Lock()
	id := r.nextObsID
	r.nextObsID++
	ch := make(chan FleetEvent, observerBufSize)
	r.observers[id] = ch
	seq := r.evSeq
	r.obsMu.Unlock()

	var once sync.Once
	cancel = func() {
		once.Do(func() {
			r.obsMu.Lock()
			delete(r.observers, id)
			r.obsMu.Unlock()
		})
	}
	return ch, seq, cancel
}

// CurrentSeq 返回事件流当前最新 seq（obsMu 只读）。供 WatchFleet 周期心跳快照携带：
// 快照 seq ≥ 此前任何已广播事件 seq，客户端收到后整体重建视图并推进基线（漂移自愈）。
func (r *NodeRegistry) CurrentSeq() int64 {
	r.obsMu.Lock()
	defer r.obsMu.Unlock()
	return r.evSeq
}

// broadcast 分配单调 seq 并向所有订阅者非阻塞扇出一条增量事件。chan 满即丢弃该订阅者本次事件
// （慢消费者绝不阻塞 registry 核心写路径——R6 不变量 / 对抗 D），累加 dropped 供观测。
// 全程持 obsMu：seq 自增与扇出原子化，保证所有订阅者见同一 seq 顺序；每次 send 经 select-default 恒不阻塞，
// 故持锁扇出安全。调用方须在 r.mu 之外调用（见 Add/Remove），避免与 List 的锁面嵌套。
func (r *NodeRegistry) broadcast(typ string, node *aurav1.NodeInfo) {
	r.obsMu.Lock()
	r.evSeq++
	ev := FleetEvent{Seq: r.evSeq, Type: typ, Node: node}
	for _, ch := range r.observers {
		select {
		case ch <- ev:
		default:
			r.dropped++
		}
	}
	r.obsMu.Unlock()
}

// WatchStatus 周期扫描 List 比对上次状态，对 online↔unhealthy 迁移的节点 broadcast status_changed。
// status 迁移由 lastSeen 超时判定、无显式事件（不同于 add/remove），故用 tick 近似（对抗 D / criterion 6）。
// 仅对「上次已知且本次状态变化」的节点广播：新节点（首次见）由 Add 的 node_added 覆盖，消失节点由 Remove 的
// node_removed 覆盖，此处只补状态迁移。装配期由 main.go 起独立 goroutine 运行；ctx 取消即退出（随进程关停）。
func (r *NodeRegistry) WatchStatus(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	lastStatus := make(map[string]string)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			nodes := r.List()
			seen := make(map[string]struct{}, len(nodes))
			for _, n := range nodes {
				seen[n.GetNodeId()] = struct{}{}
				// 批E：tick 顺带刷新在线态 gauge（online→1，unhealthy→0）——心跳超时无显式事件，
				// 与下方 status 迁移广播同源同拍（C3 告警基础）。
				observability.SetNodeUp(n.GetNodeId(), n.GetPlatform(), n.GetStatus() == "online")
				prev, ok := lastStatus[n.GetNodeId()]
				lastStatus[n.GetNodeId()] = n.GetStatus()
				if ok && prev != n.GetStatus() {
					r.broadcast(EventStatusChanged, n)
				}
			}
			// 清理已消失节点的 diff 状态（其 node_removed 已由 Remove 推送），防 map 随 churn 无界增长。
			for id := range lastStatus {
				if _, ok := seen[id]; !ok {
					delete(lastStatus, id)
				}
			}
		}
	}
}

// —— 舰队治理：长期离线僵尸节点自动遗忘（M12-P1，reap 兜底）——————————————————————————————————
// 手动删除（DeleteNode RPC）清即时可辨的离线节点；本 reap 是超长期（default 30d）无人问津僵尸的定时兜底，
// 免历史每个接入节点永久留库堆积。安全核心：只删无活跃会话且 last_seen 超期者——活跃会话经 protected 排除
// （在线权威信号，不信 stale last_seen）。

// defaultReapDays 是长期离线节点自动遗忘的默认阈值（天）：远长于任何正常离线维护窗口，只清超长期僵尸。
const defaultReapDays = 30

// reapDaysEnv 是自动遗忘阈值的环境变量名（运维可调更激进/更保守的清理策略）。
const reapDaysEnv = "AURA_NODE_REAP_DAYS"

// ReapAge 解析 AURA_NODE_REAP_DAYS（天）为时长；未设/非法/非正取默认 30d。装配期由 main.go 调用传入
// ReapLoop（env 读收敛在 registry 包，与 reap 逻辑同处）。
func ReapAge() time.Duration {
	days := defaultReapDays
	if v := os.Getenv(reapDaysEnv); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			days = n
		}
	}
	return time.Duration(days) * 24 * time.Hour
}

// liveNodeIDs 快照当前活跃会话的 node_id 集（reap 保护集：活跃会话绝不被自动遗忘）。
func (r *NodeRegistry) liveNodeIDs() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	ids := make([]string, 0, len(r.sessions))
	for id := range r.sessions {
		ids = append(ids, id)
	}
	return ids
}

// ReapOnce 执行一次自动遗忘：删 last_seen < now-age 且无活跃会话的僵尸节点身份台账，逐个广播 node_removed
// 令 console 实时出墙，返回被删数。store 为 nil（纯内存）时 no-op。活跃会话经 protected 排除（在线权威
// 信号）；HA 跨副本仅本副本会话可见，故配合 30d 长阈值（远长于重连周期，重连即刷新 last_seen）令误删概率
// 可忽略。抽为独立方法便于 ReapLoop 复用与单测直调（免 goroutine/定时）。
func (r *NodeRegistry) ReapOnce(ctx context.Context, age time.Duration) (int, error) {
	if r.store == nil {
		return 0, nil
	}
	before := time.Now().Add(-age)
	reaped, err := r.store.ReapOfflineNodes(ctx, before, r.liveNodeIDs())
	if err != nil {
		return 0, err
	}
	for _, id := range reaped {
		r.BroadcastNodeRemoved(id)
	}
	return len(reaped), nil
}

// ReapLoop 后台周期（interval）自动遗忘长期离线（age）僵尸节点。装配期由 main.go 起独立 goroutine（沿
// WatchStatus 惯例）；ctx 取消即退出（随进程关停）。失败仅告警不中断（下个 tick 重试）。
func (r *NodeRegistry) ReapLoop(ctx context.Context, interval, age time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if n, err := r.ReapOnce(ctx, age); err != nil {
				slog.Warn("fleet reap: offline node reap failed", "err", err)
			} else if n > 0 {
				slog.Info("fleet reap: long-offline zombie nodes forgotten", "count", n, "reap_age", age)
			}
		}
	}
}

// BroadcastNodeRemoved 广播一条 node_removed FleetEvent（console DeleteNode 手动删除 / reap 自动遗忘共用）：
// 被删节点无活跃会话（删除仅限 offline），故只携最小 NodeInfo（node_id + offline status）；前端按 node_id
// 出墙（fleet.ts NODE_REMOVED 分支删卡片）。经 broadcast 在 mu 外扇出（沿 Add/Remove 先例，不在核心锁内）。
func (r *NodeRegistry) BroadcastNodeRemoved(nodeID string) {
	// 批E：身份注销即删 aura_node_up 序列（离线态保 0 供告警，注销后序列随身份消失防 churn 泄漏）。
	observability.DeleteNodeUp(nodeID)
	r.broadcast(EventNodeRemoved, &aurav1.NodeInfo{NodeId: nodeID, Status: "offline"})
}

// DroppedEvents 返回累计丢弃的事件数（慢消费者背压观测；单测据此证 registry 写路径不因慢订阅者阻塞而丢弃兜底）。
func (r *NodeRegistry) DroppedEvents() int64 {
	r.obsMu.Lock()
	defer r.obsMu.Unlock()
	return r.dropped
}

// ObserverCount 返回当前活跃订阅者数（fleet watcher 数观测；单测据此证退订清理无泄漏）。
func (r *NodeRegistry) ObserverCount() int {
	r.obsMu.Lock()
	defer r.obsMu.Unlock()
	return len(r.observers)
}
