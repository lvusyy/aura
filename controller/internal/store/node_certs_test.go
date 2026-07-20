package store

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
)

// readNodeCertFPRaw 直读 nodes.cert_fp（白盒，验证吊销清空 / enroll 建行写入）。
func readNodeCertFPRaw(t *testing.T, pg *PGStore, ctx context.Context, nodeID string) pgtype.Text {
	t.Helper()
	id, err := pgUUID(nodeID)
	if err != nil {
		t.Fatalf("pgUUID: %v", err)
	}
	var fp pgtype.Text
	if err := pg.pool.QueryRow(ctx, "SELECT cert_fp FROM nodes WHERE id = $1", id).Scan(&fp); err != nil {
		t.Fatalf("read nodes.cert_fp: %v", err)
	}
	return fp
}

// TestNodeCertsLifecycleIntegration 对真 PG 演练 M12 per-node 证书台账（TASK-006）：enroll 双写（建 nodes
// 行 + node_certs 台账）→ IsCertRevoked 未吊销/未命中放行 → 吊销（幂等 + 清 nodes.cert_fp）→ 吊销后命中拒
// → ListExpiring 窗口内/外/已吊销过滤。需 AURA_TEST_PG_DSN；缺省 skip（沿 enrollment_test 惯例，无 PG
// 机器 go test ./internal/store 仍绿）。UUID/fp/serial 唯一后缀避免与既有行/并发碰撞，不清场（白盒点查断言）。
func TestNodeCertsLifecycleIntegration(t *testing.T) {
	dsn := os.Getenv("AURA_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("set AURA_TEST_PG_DSN to run node_certs integration")
	}
	ctx := context.Background()
	pg, err := NewPGStore(ctx, dsn)
	if err != nil {
		t.Fatalf("NewPGStore: %v", err)
	}
	defer pg.Close()

	uniq := func(prefix string) string { return prefix + "-" + uuid.NewString() }

	// —— enroll 双写：SetNodeCertFP 建 nodes 行（节点未反连）+ InsertNodeCert 台账 ——
	nodeID := uuid.NewString()
	fp1 := uniq("fp")
	serial1 := uniq("serial")
	notAfter := time.Now().Add(90 * 24 * time.Hour)

	if err := pg.SetNodeCertFP(ctx, nodeID, "linux", fp1, ""); err != nil {
		t.Fatalf("SetNodeCertFP (enroll bootstrap): %v", err)
	}
	if err := pg.InsertNodeCert(ctx, nodeID, serial1, fp1, notAfter); err != nil {
		t.Fatalf("InsertNodeCert: %v", err)
	}
	// nodes.cert_fp 写入（design §8 当前生效指纹）。
	if got := readNodeCertFPRaw(t, pg, ctx, nodeID); !got.Valid || got.String != fp1 {
		t.Errorf("nodes.cert_fp = %+v, want %q", got, fp1)
	}

	// —— IsCertRevoked：新证书未吊销放行；未命中（通用证书/未入台账）放行（Locked-7）——
	if rev, err := pg.IsCertRevoked(ctx, fp1); err != nil || rev {
		t.Errorf("fresh cert IsCertRevoked = (%v, %v), want (false, nil)", rev, err)
	}
	if rev, err := pg.IsCertRevoked(ctx, uniq("unknown-fp")); err != nil || rev {
		t.Errorf("unknown fp IsCertRevoked = (%v, %v), want (false, nil)（未命中放行）", rev, err)
	}

	// —— 吊销（幂等）+ 清 nodes.cert_fp（design §7）——
	did, err := pg.RevokeNodeCert(ctx, nodeID, serial1)
	if err != nil || !did {
		t.Fatalf("RevokeNodeCert = (%v, %v), want (true, nil)", did, err)
	}
	if again, _ := pg.RevokeNodeCert(ctx, nodeID, serial1); again {
		t.Error("RevokeNodeCert 幂等：二次吊销应返回 false")
	}
	// 吊销后命中拒。
	if rev, err := pg.IsCertRevoked(ctx, fp1); err != nil || !rev {
		t.Errorf("revoked cert IsCertRevoked = (%v, %v), want (true, nil)", rev, err)
	}
	// nodes.cert_fp 清空（当前生效指纹作废）。
	if got := readNodeCertFPRaw(t, pg, ctx, nodeID); got.Valid {
		t.Errorf("nodes.cert_fp after revoke = %q, want NULL", got.String)
	}

	// —— ListExpiring：窗口内命中 / 窗口外不命中 / 已吊销排除 ——
	node2 := uuid.NewString()
	soonFP, soonSerial := uniq("fp-soon"), uniq("serial-soon")
	farFP, farSerial := uniq("fp-far"), uniq("serial-far")
	if err := pg.InsertNodeCert(ctx, node2, soonSerial, soonFP, time.Now().Add(10*24*time.Hour)); err != nil {
		t.Fatalf("InsertNodeCert soon: %v", err)
	}
	if err := pg.InsertNodeCert(ctx, node2, farSerial, farFP, time.Now().Add(60*24*time.Hour)); err != nil {
		t.Fatalf("InsertNodeCert far: %v", err)
	}
	expiring, err := pg.ListExpiring(ctx, 30*24*time.Hour)
	if err != nil {
		t.Fatalf("ListExpiring: %v", err)
	}
	foundSoon, foundFar := false, false
	for _, c := range expiring {
		if c.CertFP == soonFP {
			foundSoon = true
		}
		if c.CertFP == farFP {
			foundFar = true
		}
	}
	if !foundSoon {
		t.Error("ListExpiring 应含窗口内（10d < 30d）到期证书")
	}
	if foundFar {
		t.Error("ListExpiring 不应含窗口外（60d > 30d）证书")
	}
	// 已吊销证书排除续签扫描。
	if _, err := pg.RevokeNodeCert(ctx, node2, soonSerial); err != nil {
		t.Fatalf("RevokeNodeCert soon: %v", err)
	}
	expiring2, err := pg.ListExpiring(ctx, 30*24*time.Hour)
	if err != nil {
		t.Fatalf("ListExpiring after revoke: %v", err)
	}
	for _, c := range expiring2 {
		if c.CertFP == soonFP {
			t.Error("已吊销证书应从 ListExpiring 排除")
		}
	}
}
