package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// —— M12 per-node 证书台账 store：node_certs 表访问 + nodes.cert_fp 双写（TASK-006）——————————————
// 两副本共享 PG（Locked-5）：签发/续签/吊销任一副本写，另一副本即时可见。签发编排（验 token/分配
// node-id/调 CA 签发）在 transport 层（enroll_rest.go）；本文件只做表访问原语（数据结构存事实不存策略）。
// cert_fp 双写（design §8）：nodes.cert_fp = 当前生效指纹（console 展示 + 反连核对）；node_certs =
// 全量台账（serial/not_after/issued_at，续签多行 + 吊销校验 + 到期扫描）。

// NodeCert 是 node_certs 表的一行（M12 per-node 证书台账）。一个 node 随续签产生多行（同 node_id
// 多 serial）；Revoked 写入取 SQL DEFAULT false，IssuedAt 写入取 DEFAULT now()（读路径回填）。
type NodeCert struct {
	NodeID   string
	Serial   string
	CertFP   string    // hex(SHA256(cert.Raw))；与 nodes.cert_fp 同值双写，反连按指纹反查吊销
	NotAfter time.Time // 有效期终点（签发时 = now()+90d）；ListExpiring 续签扫描依据
	IssuedAt time.Time // 签发时刻（读路径回填）
	Revoked  bool
}

// InsertNodeCert 向 node_certs 台账插入一张新签发证书的记录（issued_at 取 SQL DEFAULT now()、revoked
// 取 DEFAULT false）。enroll 首签与 renew 续签均调用（续签为同 node_id 新 serial 新行，保留旧行历史）。
// serial 为随机 128-bit（撞号概率可忽略），PK(node_id,serial) 冲突即签发逻辑异常，如实报错不吞。
func (s *PGStore) InsertNodeCert(ctx context.Context, nodeID, serial, certFP string, notAfter time.Time) error {
	id, err := pgUUID(nodeID)
	if err != nil {
		return err
	}
	const q = `INSERT INTO node_certs (node_id, serial, cert_fp, not_after) VALUES ($1, $2, $3, $4)`
	if _, err := s.pool.Exec(ctx, q, id, serial, certFP, notAfter); err != nil {
		return fmt.Errorf("insert node cert: %w", err)
	}
	return nil
}

// SetNodeCertFP 记录节点当前生效证书指纹到 nodes.cert_fp（design §8 双写之一：当前生效指纹）。
// UPSERT 语义：enroll 时节点尚未反连注册（无 nodes 行），故以 platform 引导 INSERT 一行（status
// 占位 'enrolled'，反连注册时 UpsertNode 会 ON CONFLICT 覆写 status=online + cert_fp COALESCE
// 保持同值）；nodes 行已存在（renew/已注册）时仅 DO UPDATE cert_fp，platform 引导值不生效（既有行
// platform 由 Register 权威管，不被本路径覆写）。platform 仅在「首次为 enroll 节点建行」时消费。
// M15：project 参数在 enroll 建行时落 nodes.project（token 携归属，节点入网即归属）；renew 传空串不改
// 归属（COALESCE(NULLIF(EXCLUDED.project,''), nodes.project) 对空保现值）——renew 只换证书不迁项目。
func (s *PGStore) SetNodeCertFP(ctx context.Context, nodeID, platform, certFP, project string) error {
	id, err := pgUUID(nodeID)
	if err != nil {
		return err
	}
	// status='enrolled'：enroll 建行占位态（节点未反连）；fleet 读路径对无活跃会话者一律置 offline
	// （registry.ListFleet 不信表内 status），故占位值不误导展示。反连注册即被 UpsertNode 覆写。
	const q = `
INSERT INTO nodes (id, platform, cert_fp, project, status)
VALUES ($1, $2, $3, NULLIF($4, ''), 'enrolled')
ON CONFLICT (id) DO UPDATE SET
	cert_fp = EXCLUDED.cert_fp,
	project = COALESCE(NULLIF(EXCLUDED.project, ''), nodes.project)`
	if _, err := s.pool.Exec(ctx, q, id, platform, certFP, project); err != nil {
		return fmt.Errorf("set node cert_fp: %w", err)
	}
	return nil
}

// RevokeNodeCert 吊销指定 (node_id, serial) 的证书并清空 nodes.cert_fp（design §7，console「移除设备」
// 触发）。单事务原子：node_certs.revoked=true（幂等——仅未吊销时置）+ nodes.cert_fp 清空（当前生效
// 指纹作废，展示与反连核对随之失效）。返回是否发生吊销（serial 不存在/已吊销 → false）。反连准入的
// 拒接由 IsCertRevoked 按指纹反查承接（应用层最小侵入，design §7）。
func (s *PGStore) RevokeNodeCert(ctx context.Context, nodeID, serial string) (bool, error) {
	id, err := pgUUID(nodeID)
	if err != nil {
		return false, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("begin revoke tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // Commit 成功后 Rollback 为 no-op（pgx 幂等）

	tag, err := tx.Exec(ctx, `UPDATE node_certs SET revoked = true WHERE node_id = $1 AND serial = $2 AND NOT revoked`, id, serial)
	if err != nil {
		return false, fmt.Errorf("revoke node cert: %w", err)
	}
	// 清当前生效指纹（design §7：清 nodes.cert_fp）。设备被移除即无当前有效证书，展示/反连核对置空。
	if _, err := tx.Exec(ctx, `UPDATE nodes SET cert_fp = NULL WHERE id = $1`, id); err != nil {
		return false, fmt.Errorf("clear node cert_fp on revoke: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit revoke tx: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// RevokeNodeCertsByNode 吊销一个节点名下全部未吊销证书并清空 nodes.cert_fp（console「吊销证书」触发面，
// M12-P1）。较 RevokeNodeCert（单 (node_id,serial) 精确吊销）：console 吊销入口仅有 node_id（不暴露 serial），
// 语义是「移除该设备反连准入」——续签可能多 serial 并存，须全吊销方彻底阻断（节点可持任一有效 cert 反连）。
// 单事务原子：node_certs.revoked=true（仅未吊销者，幂等）+ nodes.cert_fp 清空（当前生效指纹作废）。返回是否
// 发生吊销（该节点无未吊销证书 → false，供 handler 区分 not-found）。反连准入拒接由 IsCertRevoked 按指纹反查
// 承接（同 RevokeNodeCert，应用层最小侵入 design §7）。
func (s *PGStore) RevokeNodeCertsByNode(ctx context.Context, nodeID string) (bool, error) {
	id, err := pgUUID(nodeID)
	if err != nil {
		return false, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("begin revoke-by-node tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // Commit 成功后 Rollback 为 no-op（pgx 幂等）

	tag, err := tx.Exec(ctx, `UPDATE node_certs SET revoked = true WHERE node_id = $1 AND NOT revoked`, id)
	if err != nil {
		return false, fmt.Errorf("revoke node certs by node: %w", err)
	}
	// 清当前生效指纹（design §7：清 nodes.cert_fp）。设备被吊销即无当前有效证书，展示/反连核对置空。
	if _, err := tx.Exec(ctx, `UPDATE nodes SET cert_fp = NULL WHERE id = $1`, id); err != nil {
		return false, fmt.Errorf("clear node cert_fp on revoke-by-node: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit revoke-by-node tx: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// deleteNodeCertsTx 在事务内删除一个节点的全部证书台账行（DeleteNode 级联，M12-P1 舰队治理）。node 身份
// 被注销时其 per-node 证书行同去（台账不留悬空 cert）；node_certs 与 nodes 无 FK 约束，删序无依赖，同事务
// 原子即可。较 RevokeNodeCert（吊销=标 revoked 保留行、反连按指纹拒接）语义不同：删除是身份整体注销，行清空。
func deleteNodeCertsTx(ctx context.Context, tx pgx.Tx, id pgtype.UUID) error {
	if _, err := tx.Exec(ctx, `DELETE FROM node_certs WHERE node_id = $1`, id); err != nil {
		return fmt.Errorf("delete node certs: %w", err)
	}
	return nil
}

// deleteNodeCertsByIDsTx 在事务内批量删除一组节点的证书台账行（ReapOfflineNodes 级联）。ids 空即 no-op。
func deleteNodeCertsByIDsTx(ctx context.Context, tx pgx.Tx, ids []pgtype.UUID) error {
	if len(ids) == 0 {
		return nil
	}
	if _, err := tx.Exec(ctx, `DELETE FROM node_certs WHERE node_id = ANY($1)`, ids); err != nil {
		return fmt.Errorf("delete node certs (reap): %w", err)
	}
	return nil
}

// IsCertRevoked 按证书指纹反查吊销状态（design §7 反连准入校验，每次反连命中；idx_node_certs_fp 支撑）：
//   - 命中且 revoked=true  → (true, nil)   反连拒接（peer 证书已吊销）
//   - 命中且 revoked=false → (false, nil)  放行
//   - 未命中（通用证书 CN=aura-node 不入台账）→ (false, nil)  放行（Locked-7 兼容，不误伤 existing）
//
// cert_fp 为 SHA256(cert.Raw) 唯一（不同证书不同指纹），点查至多一行。DB 错误如实上抛，由调用方
// （RevocationMiddleware）裁决降级策略（可用性 vs 吊销即时性权衡）。
func (s *PGStore) IsCertRevoked(ctx context.Context, certFP string) (bool, error) {
	const q = `SELECT revoked FROM node_certs WHERE cert_fp = $1 LIMIT 1`
	var revoked bool
	if err := s.pool.QueryRow(ctx, q, certFP).Scan(&revoked); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil // 未命中：通用证书/未入台账 → 放行（Locked-7）
		}
		return false, fmt.Errorf("query cert revocation: %w", err)
	}
	return revoked, nil
}

// ListExpiring 扫描 within 窗口内即将到期且未吊销的证书（design §3.5/§6 续签扫描；idx_node_certs_not_after
// 支撑）。阈值时刻在 Go 侧算定（now+within）再点查 not_after<阈值，避免 SQL interval 拼接；按 not_after
// 升序（最先到期在前）。供续签编排（本 milestone 不实装自动续签调度，方法先备——node 侧自检续签为主路径）。
func (s *PGStore) ListExpiring(ctx context.Context, within time.Duration) ([]NodeCert, error) {
	threshold := time.Now().Add(within)
	const q = `SELECT node_id, serial, cert_fp, not_after, issued_at, revoked
FROM node_certs
WHERE not_after < $1 AND NOT revoked
ORDER BY not_after ASC`
	rows, err := s.pool.Query(ctx, q, threshold)
	if err != nil {
		return nil, fmt.Errorf("query expiring node certs: %w", err)
	}
	defer rows.Close()

	var out []NodeCert
	for rows.Next() {
		rec, err := scanNodeCert(rows)
		if err != nil {
			return nil, fmt.Errorf("scan node cert: %w", err)
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate node certs: %w", err)
	}
	return out, nil
}

// scanNodeCert 从一行扫描 NodeCert（列序须为 node_id,serial,cert_fp,not_after,issued_at,revoked）。
// node_id 为 UUID 列经 pgtype.UUID 解码还原字符串（复用 pg.go uuidString 惯例）。
func scanNodeCert(sc rowScanner) (NodeCert, error) {
	var (
		rec    NodeCert
		nodeID pgtype.UUID
	)
	if err := sc.Scan(&nodeID, &rec.Serial, &rec.CertFP, &rec.NotAfter, &rec.IssuedAt, &rec.Revoked); err != nil {
		return NodeCert{}, err
	}
	rec.NodeID = uuidString(nodeID)
	return rec, nil
}
