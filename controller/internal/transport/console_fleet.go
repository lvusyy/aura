package transport

import (
	"context"
	"log/slog"
	"time"

	"connectrpc.com/connect"

	aurav1 "github.com/aura/controller/gen/aura/v1"
	"github.com/aura/controller/internal/registry"
)

// WatchFleet 舰队状态变更事件流（server-streaming，aura.v1 首个 streaming RPC）。TASK-006 真实现：
// 首帧发 registry.List() 全量快照（HEARTBEAT_SNAPSHOT），之后转发 registry observer 广播的增量事件
// （node_added / node_removed / status_changed）。对抗 D 漂移收敛三件套：有界 chan（registry 侧 cap 64）
// + 事件单调 seq + 慢消费者丢弃；跳号由客户端（TASK-008）检测并重拉 ListNodes 全量快照重同步。
// connect server-streaming 浏览器 HTTP/1.1 即可订阅（research §4）；ctx 取消/流断即退订释放订阅者槽。
func (s *ConsoleServiceServer) WatchFleet(
	ctx context.Context,
	_ *connect.Request[aurav1.WatchFleetRequest],
	stream *connect.ServerStream[aurav1.FleetEvent],
) error {
	// 订阅先于取快照：注册 chan 后发生的增量事件必入 chan；快照晚于订阅点经 List() 取，二者至多幂等重叠
	// （客户端按 node_id upsert），绝不丢事件——对抗 D「快照+增量无缝衔接」前提。
	events, snapshotSeq, cancel := s.reg.Subscribe()
	defer cancel() // ctx 取消 / 流断即退订，释放订阅者槽（无 goroutine / chan 泄漏，criterion 4）

	// 首帧：全量快照（HEARTBEAT_SNAPSHOT），seq=订阅基线。客户端据此建初始视图，之后按增量事件 seq 单调推进。
	// ListFleet（M12）：在线会话 + nodes 表 offline 持久身份合并，令 FleetPage 展示 offline 节点可读名/label
	// （List 仅在线，会话数语义留给 metrics/WatchStatus）。
	if err := stream.Send(&aurav1.FleetEvent{
		Seq:        snapshotSeq,
		Type:       aurav1.FleetEventType_FLEET_EVENT_TYPE_HEARTBEAT_SNAPSHOT,
		Snapshot:   s.reg.ListFleet(ctx),
		Recordings: s.fleetRecordings(ctx),
	}); err != nil {
		return err
	}

	// 增量推送循环：registry observer 广播 → proto FleetEvent。慢消费者已在 registry 侧被丢弃（chan 满），
	// 此处只顺序转发到达事件；丢弃在客户端表现为 seq 跳号。ctx 取消（客户端断开）即正常收尾。
	//
	// 周期心跳快照（30s）：HTTP/1.1 长驻 chunked 响应无协议级 ping，空闲期零字节会被中间层/OS idle
	// 超时静默回收且客户端无从区分「静默 vs 断流」；周期重发全量快照一石二鸟——既作应用层心跳
	// （前端 liveness：超时无帧即重连），又让慢消费者被丢事件造成的视图漂移在一个周期内自愈
	// （coalesce-to-latest 语义的快照近似；节点规模个位数，全量快照开销可忽略）。
	heartbeat := time.NewTicker(30 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case ev := <-events:
			pe := toProtoFleetEvent(ev)
			// 增量帧回填覆盖节点的录制占用态（T13）：租约恰随节点事件变化时即时收敛；
			// 纯 StartTrace/StopTrace（无节点事件）由 30s 心跳快照兜底刷新。
			if n := pe.GetNode(); n != nil {
				pe.Recordings = s.nodeRecording(ctx, n.GetNodeId())
			}
			if err := stream.Send(pe); err != nil {
				return err // 发送失败=流断，退出（defer cancel 清理订阅）
			}
		case <-heartbeat.C:
			// 心跳快照 seq=当前最新（≥此前任何事件 seq），客户端按快照类型整体重建视图并推进基线。
			if err := stream.Send(&aurav1.FleetEvent{
				Seq:        s.reg.CurrentSeq(),
				Type:       aurav1.FleetEventType_FLEET_EVENT_TYPE_HEARTBEAT_SNAPSHOT,
				Snapshot:   s.reg.ListFleet(ctx), // M12：含 offline 节点（周期快照兜底 offline 元数据/编辑刷新）
				Recordings: s.fleetRecordings(ctx),
			}); err != nil {
				return err // 心跳发送失败=流断（含中间层已回收连接的显式暴露）
			}
		}
	}
}

// fleetRecordings 组装快照帧（首帧/心跳）的录制占用态：List 全部活跃租约 → recordings 表
//（T13 租约期 UX）。覆盖语义：快照帧覆盖全舰队，不在表即未被录制（前端据此清 badge）。
// 读故障 → Warn + 空表（读降级 ha-contract §7#5：录制态是装饰性信息面，不因租约存储故障断
// fleet 流）；leases 未注入（裸构造单测）同空。
func (s *ConsoleServiceServer) fleetRecordings(ctx context.Context) []*aurav1.NodeRecording {
	if s.leases == nil {
		return nil
	}
	leases, err := s.leases.List(ctx)
	if err != nil {
		slog.Warn("fleet recordings: lease list failed; omitting recording state", "err", err)
		return nil
	}
	recs := make([]*aurav1.NodeRecording, 0, len(leases))
	for _, l := range leases {
		recs = append(recs, &aurav1.NodeRecording{NodeId: l.NodeID, Who: l.Who, TraceId: l.TraceID})
	}
	return recs
}

// nodeRecording 组装增量帧覆盖节点的录制占用态（0/1 项）：Get 单节点租约（较 List 免全量扫描）。
// 语义同 fleetRecordings：无租约/读故障/未注入 → nil（覆盖节点不在表=未录制）。
func (s *ConsoleServiceServer) nodeRecording(ctx context.Context, nodeID string) []*aurav1.NodeRecording {
	if s.leases == nil || nodeID == "" {
		return nil
	}
	lease, ok, err := s.leases.Get(ctx, nodeID)
	if err != nil {
		slog.Warn("fleet recordings: lease get failed; omitting recording state", "node_id", nodeID, "err", err)
		return nil
	}
	if !ok {
		return nil
	}
	return []*aurav1.NodeRecording{{NodeId: lease.NodeID, Who: lease.Who, TraceId: lease.TraceID}}
}

// toProtoFleetEvent 将 registry 内部事件转 proto FleetEvent（criterion：handler 侧转 proto，registry 不依赖 proto 枚举）。
func toProtoFleetEvent(ev registry.FleetEvent) *aurav1.FleetEvent {
	return &aurav1.FleetEvent{
		Seq:  ev.Seq,
		Type: fleetEventTypeToProto(ev.Type),
		Node: ev.Node,
	}
}

// fleetEventTypeToProto 映射 registry 事件语义串到 proto 枚举；未知类型落 UNSPECIFIED（防御性兜底）。
func fleetEventTypeToProto(typ string) aurav1.FleetEventType {
	switch typ {
	case registry.EventNodeAdded:
		return aurav1.FleetEventType_FLEET_EVENT_TYPE_NODE_ADDED
	case registry.EventNodeRemoved:
		return aurav1.FleetEventType_FLEET_EVENT_TYPE_NODE_REMOVED
	case registry.EventStatusChanged:
		return aurav1.FleetEventType_FLEET_EVENT_TYPE_STATUS_CHANGED
	default:
		return aurav1.FleetEventType_FLEET_EVENT_TYPE_UNSPECIFIED
	}
}
