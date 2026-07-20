//go:build e2e

package e2e

import (
	"path/filepath"
	"testing"
	"time"
)

// TestEnrollFullChain 验证接入全链（M16 T1.2 场景①）：GenerateEnrollToken → aura-node enroll 换
// per-node 证书 → 以之 mTLS 反连 → 现身舰队。这是 controller(CA 签发+PG 台账) + node(CSR+反连)
// 的地基路径，其余场景皆依赖节点在册。
func TestEnrollFullChain(t *testing.T) {
	dataDir := filepath.Join(h.dataRoot, "node-enroll")

	token := h.generateToken(t, "linux", "")
	nodeID := h.enrollNode(t, dataDir, token, "linux")
	if nodeID == "" {
		t.Fatal("enroll returned empty node_id")
	}
	t.Logf("enrolled node_id=%s", nodeID)

	stop, _ := h.startNode(t, dataDir, "desktop")
	defer stop()

	h.waitNodeInFleet(t, nodeID, 30*time.Second)
	t.Logf("node %s present in fleet", nodeID)
}
