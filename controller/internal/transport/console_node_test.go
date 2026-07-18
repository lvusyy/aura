package transport

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	aurav1 "github.com/aura/controller/gen/aura/v1"
	"github.com/aura/controller/internal/registry"
)

// TestDeleteNodeGuards 验证舰队治理删除的守卫链（无需真 PG）：空 node_id→InvalidArgument；在线节点（活跃
// 会话在册）→FailedPrecondition（E_NODE_ONLINE，防误删活跃）；offline 但纯内存（store=nil）→Unavailable。
// 真删 nodes+node_certs 台账 + node_removed 广播由 store SQL（build 验证）与 registry ReapOnce/集成覆盖，
// 此处专验 handler 守卫分支。
func TestDeleteNodeGuards(t *testing.T) {
	reg := registry.NewRegistry(nil)
	reg.Add(registry.NewSession("node-online", "linux", nil, "", 1))
	srv := NewConsoleServiceServer(reg, nil, nil, nil, nil)
	ctx := context.Background()

	// 空 node_id → InvalidArgument。
	if _, err := srv.DeleteNode(ctx, connect.NewRequest(&aurav1.DeleteNodeRequest{})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("DeleteNode 空 node_id: want InvalidArgument, got %v", err)
	}
	// 在线节点（活跃会话在册）→ FailedPrecondition（E_NODE_ONLINE，拒删活跃，先于 store 判空）。
	if _, err := srv.DeleteNode(ctx, connect.NewRequest(&aurav1.DeleteNodeRequest{NodeId: "node-online"})); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("DeleteNode 在线节点: want FailedPrecondition(E_NODE_ONLINE), got %v", err)
	}
	// offline 节点但纯内存（store=nil）→ Unavailable（无持久台账后端）。
	if _, err := srv.DeleteNode(ctx, connect.NewRequest(&aurav1.DeleteNodeRequest{NodeId: "ghost-offline"})); connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("DeleteNode offline+纯内存: want Unavailable, got %v", err)
	}
}

// TestRevokeNodeCertGuards 验证吊销触发面 handler 守卫（无需真 PG）：空 node_id→InvalidArgument；纯内存
// （store=nil）→Unavailable。较 DeleteNode 无「在线拒绝」守卫——吊销在线/离线皆可（阻可疑节点反连准入，
// 续签多 serial 全吊销）。真 revoked+清 cert_fp 由 store SQL（build/live curl 验证）与 T06/T11 反连 403
// 执行面覆盖，此处专验 handler 守卫分支。
func TestRevokeNodeCertGuards(t *testing.T) {
	reg := registry.NewRegistry(nil)
	reg.Add(registry.NewSession("node-online", "linux", nil, "", 1))
	srv := NewConsoleServiceServer(reg, nil, nil, nil, nil)
	ctx := context.Background()

	// 空 node_id → InvalidArgument。
	if _, err := srv.RevokeNodeCert(ctx, connect.NewRequest(&aurav1.RevokeNodeCertRequest{})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("RevokeNodeCert 空 node_id: want InvalidArgument, got %v", err)
	}
	// 在线节点但纯内存（store=nil）→ Unavailable（无持久证书台账后端）——吊销不设在线守卫，直落 store 判空。
	if _, err := srv.RevokeNodeCert(ctx, connect.NewRequest(&aurav1.RevokeNodeCertRequest{NodeId: "node-online"})); connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("RevokeNodeCert 纯内存: want Unavailable, got %v", err)
	}
}
