package transport

import (
	"context"
	"errors"
	"fmt"

	"connectrpc.com/connect"

	aurav1 "github.com/aura/controller/gen/aura/v1"
)

// M12-P1 节点元数据编辑 handler（TASK-005，替 TASK-001 stub）。UpdateNodeMeta 落 nodes 表 label/location
// 持久化（console 编辑权威路径）+ 同步在线会话缓存 + 广播 FleetEvent 令 console 实时刷新。ConsoleService
// handler 全落 console_*.go（console.go 分文件设计）——本文件专注节点元数据编辑，与 console_enrollment.go
// （TASK-004 token 治理领地）互斥零交集，各真实现任务替换独立文件避免同 struct 并行写冲突（对抗 H）。
func (s *ConsoleServiceServer) UpdateNodeMeta(
	ctx context.Context,
	req *connect.Request[aurav1.UpdateNodeMetaRequest],
) (*connect.Response[aurav1.UpdateNodeMetaResponse], error) {
	nodeID := req.Msg.GetNodeId()
	if nodeID == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("node_id is required"))
	}
	if s.store == nil {
		// 纯内存运行（未配置 PG）：元数据无持久化后端，编辑不可用（降级 Unavailable，同 ReadNodeScreen
		// 未配 scheduler 惯例）。
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("node metadata store not configured"))
	}
	// registry.UpdateNodeMeta：落库 label/location（store.UpdateNodeMeta）+ 同步在线会话缓存 + 广播
	// FleetEvent（在线节点即时刷新，离线节点靠 WatchFleet 周期快照兜底）。label/location 与 name/hostname/
	// network_zone/cert_fp（Register 侧写）列分离，两写方零互抹。
	updated, err := s.reg.UpdateNodeMeta(ctx, nodeID, req.Msg.GetLabel(), req.Msg.GetLocation())
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("update node meta: %w", err))
	}
	if !updated {
		// node_id 不在 nodes 表（未注册/已清理）：not-found，前端据此提示而非静默成功。
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("node %q not found", nodeID))
	}
	return connect.NewResponse(&aurav1.UpdateNodeMetaResponse{Updated: true}), nil
}

// DeleteNode 删除离线僵尸节点（M12-P1 舰队治理追加，additive）。安全边界（防误删活跃节点）：仅 offline
// 节点可删——查 registry 活跃会话，在线（会话在册）即拒删返 E_NODE_ONLINE（FailedPrecondition）。活跃会话
// 是「在线」权威信号（不信 nodes.last_seen——仅 Register 刷新，长会话可 stale）；console 亦仅对 offline 节点
// 显删除按钮，后端此守卫为纵深防御。store.DeleteNode 单事务删 nodes + node_certs 身份台账（不删 tasks/
// traces 历史，仅注销身份）；成功广播 node_removed FleetEvent 令 console useFleetStream 自动出墙。长期离线
// 由 registry.ReapLoop 定时自动遗忘（AURA_NODE_REAP_DAYS default 30d），本 RPC 是即时手动路径。
func (s *ConsoleServiceServer) DeleteNode(
	ctx context.Context,
	req *connect.Request[aurav1.DeleteNodeRequest],
) (*connect.Response[aurav1.DeleteNodeResponse], error) {
	nodeID := req.Msg.GetNodeId()
	if nodeID == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("node_id is required"))
	}
	// offline 守卫（安全核心）：活跃会话在册=在线，拒删（防误删活跃节点）。先于 store 判空——「拒删在线」
	// 与持久化后端无关，是最优先的安全语义。
	if _, online := s.reg.Get(nodeID); online {
		return nil, connect.NewError(connect.CodeFailedPrecondition,
			fmt.Errorf("E_NODE_ONLINE: node %q is online; refuse to delete an active node", nodeID))
	}
	if s.store == nil {
		// 纯内存运行（未配 PG）：无持久身份台账后端，删除不可用（降级 Unavailable，同 UpdateNodeMeta 惯例）。
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("node store not configured"))
	}
	deleted, err := s.store.DeleteNode(ctx, nodeID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("delete node: %w", err))
	}
	if !deleted {
		// node_id 不在 nodes 表（未注册/已清理）：not-found（同 UpdateNodeMeta，前端据此提示而非静默成功）。
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("delete node: node %q not found", nodeID))
	}
	// 广播 node_removed 令 console 实时出墙（WatchFleet 增量 → useFleetStream NODE_REMOVED 分支删卡片）。
	s.reg.BroadcastNodeRemoved(nodeID)
	return connect.NewResponse(&aurav1.DeleteNodeResponse{Deleted: true}), nil
}

// RevokeNodeCert 吊销节点证书（M12-P1 吊销触发面，design §7）：委托 store.RevokeNodeCertsByNode 标记该节点
// 名下全部未吊销证书 revoked=true + 清 nodes.cert_fp，令节点持吊销 cert 反连即遭 RevocationMiddleware 403 拒
// （应用层准入校验，执行面 T06 已验）。console「吊销证书」按钮触发。较 DeleteNode（删身份台账，仅 offline，
// 防误删活跃）：吊销标记非删行（保留台账供审计），且在线/离线皆可吊销——吊销一个在线但可疑节点的反连准入，
// 其活跃会话续存至自然断开，重连即 403（本 RPC 是准入面，不强断在途会话，与 T11 实证语义一致）。续签多
// serial 并存时全吊销方彻底阻断（RevokeNodeCertsByNode node_id 维度批量）。返回是否发生吊销（该节点无未
// 吊销证书 → NotFound，前端据此提示而非静默成功）。
func (s *ConsoleServiceServer) RevokeNodeCert(
	ctx context.Context,
	req *connect.Request[aurav1.RevokeNodeCertRequest],
) (*connect.Response[aurav1.RevokeNodeCertResponse], error) {
	nodeID := req.Msg.GetNodeId()
	if nodeID == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("node_id is required"))
	}
	if s.store == nil {
		// 纯内存运行（未配 PG）：无持久证书台账后端，吊销不可用（降级 Unavailable，同 UpdateNodeMeta/DeleteNode 惯例）。
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("node cert store not configured"))
	}
	revoked, err := s.store.RevokeNodeCertsByNode(ctx, nodeID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("revoke node cert: %w", err))
	}
	if !revoked {
		// 该节点无未吊销证书（未 enroll 换 per-node cert / 已全吊销）：not-found，前端据此提示而非静默成功。
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("revoke node cert: node %q has no active cert", nodeID))
	}
	return connect.NewResponse(&aurav1.RevokeNodeCertResponse{Revoked: true}), nil
}
