package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// —— M12 enrollment join token store：enrollment_tokens 表访问（TASK-004）——————————————————————
// 两副本共享 PG（Locked-5）：两副本读写同库，ConsumeToken 单 SQL 原子扣减保并发竞态安全（沿
// AllocVMID UPDATE..RETURNING 先例）。生成/校验/轮换/吊销的应用层策略（ttl 默认 3600、uses 默认 1、
// platform_scope 权限最小）归 transport handler；本文件只做表访问原语（数据结构存事实不存策略）。

// EnrollToken 是 enrollment_tokens 表的一行（M12 设备接入 join token）。PlatformScope/Label/CreatedBy
// 可空（NULL 读还原空串）；CreatedAt 读路径回填（写入取 SQL DEFAULT now()）；Revoked 写入取 DEFAULT false。
type EnrollToken struct {
	Token         string
	PlatformScope string    // 空=不限平台（权限最小）
	UsesLeft      int32     // 剩余可用次数；ConsumeToken 原子 -1
	ExpiresAt     time.Time // 绝对过期时刻（generate 时 = now()+ttl）
	Label         string    // enroll 成功赋新节点的初始 label（ConsumeToken RETURNING 回传）
	CreatedBy     string    // 生成者标识（审计）
	CreatedAt     time.Time // 读路径回填
	Revoked       bool
}

// ErrTokenInvalid 表示 enrollment token 校验消费未命中（不存在/过期/耗尽/吊销/平台不匹配）——与真实
// DB 错误区分，供 enroll 端点（TASK-006）映射 401/403，不与 pgx.ErrNoRows 混淆。
var ErrTokenInvalid = errors.New("enrollment token invalid, expired, exhausted, or revoked")

// InsertToken 插入一条 enrollment token（created_at 取 SQL DEFAULT now()、revoked 取 DEFAULT false）。
func (s *PGStore) InsertToken(ctx context.Context, t EnrollToken) error {
	const q = `INSERT INTO enrollment_tokens (token, platform_scope, uses_left, expires_at, label, created_by)
VALUES ($1, $2, $3, $4, $5, $6)`
	if _, err := s.pool.Exec(ctx, q, t.Token, t.PlatformScope, t.UsesLeft, t.ExpiresAt, t.Label, t.CreatedBy); err != nil {
		return fmt.Errorf("insert enrollment token: %w", err)
	}
	return nil
}

// GetToken 按 token 主键读一行（不存在返回 pgx.ErrNoRows，同 GetOrchestration 惯例；消费方经
// store.IsNotFound 判定）。供 RotateEnrollToken 读旧 token 承继 platform_scope/label/ttl。
func (s *PGStore) GetToken(ctx context.Context, token string) (EnrollToken, error) {
	const q = `SELECT token, platform_scope, uses_left, expires_at, label, created_by, created_at, revoked
FROM enrollment_tokens WHERE token = $1`
	return scanEnrollToken(s.pool.QueryRow(ctx, q, token))
}

// ConsumeToken 原子校验+消费一次 token：单 SQL UPDATE..RETURNING 扣减 uses_left 并回传 label，WHERE
// 未过期/未耗尽/未吊销/平台匹配（platform_scope='' 短路放行不限平台）。命中 0 行（token 无效）返回
// ErrTokenInvalid；命中 1 行即有效且已原子扣减一次。两副本并发消费同一 token 由 PG 行锁天然串行，
// 仅一方扣到最后一次，杜绝超用（读-改-写会竞态；沿 AllocVMID UPDATE..RETURNING 先例）。platform=请求平台。
func (s *PGStore) ConsumeToken(ctx context.Context, token, platform string) (string, error) {
	const q = `
UPDATE enrollment_tokens
   SET uses_left = uses_left - 1
 WHERE token = $1
   AND uses_left > 0
   AND NOT revoked
   AND expires_at > now()
   AND (platform_scope = '' OR platform_scope = $2)
RETURNING label`
	var label pgtype.Text
	if err := s.pool.QueryRow(ctx, q, token, platform).Scan(&label); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", ErrTokenInvalid
		}
		return "", fmt.Errorf("consume enrollment token: %w", err)
	}
	return label.String, nil // NULL label → ""
}

// RevokeToken 吊销一个 token（幂等）：仅当 token 存在且此前未吊销时置 revoked=true。返回是否发生吊销
// （token 不存在/已吊销 → false，契合 RevokeEnrollTokenResponse.revoked 语义）。
func (s *PGStore) RevokeToken(ctx context.Context, token string) (bool, error) {
	const q = `UPDATE enrollment_tokens SET revoked = true WHERE token = $1 AND NOT revoked`
	tag, err := s.pool.Exec(ctx, q, token)
	if err != nil {
		return false, fmt.Errorf("revoke enrollment token: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// ListTokens 全量列举 enrollment token（console 治理表读路径），按 created_at DESC（最新在前）。token
// 为 admin 小表（数量有界），全列不分页（proto ListEnrollTokensRequest 无分页字段；design §1）。
func (s *PGStore) ListTokens(ctx context.Context) ([]EnrollToken, error) {
	const q = `SELECT token, platform_scope, uses_left, expires_at, label, created_by, created_at, revoked
FROM enrollment_tokens ORDER BY created_at DESC`
	rows, err := s.pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("query enrollment tokens: %w", err)
	}
	defer rows.Close()

	var out []EnrollToken
	for rows.Next() {
		rec, err := scanEnrollToken(rows)
		if err != nil {
			return nil, fmt.Errorf("scan enrollment token: %w", err)
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate enrollment tokens: %w", err)
	}
	return out, nil
}

// scanEnrollToken 从一行扫描 EnrollToken（列序须为 token,platform_scope,uses_left,expires_at,label,
// created_by,created_at,revoked）。platform_scope/label/created_by 可空，NULL 还原空串。复用 pg.go
// 的 rowScanner，QueryRow/Query 迭代共用同一解码逻辑。
func scanEnrollToken(sc rowScanner) (EnrollToken, error) {
	var (
		rec       EnrollToken
		scope     pgtype.Text
		label     pgtype.Text
		createdBy pgtype.Text
	)
	if err := sc.Scan(&rec.Token, &scope, &rec.UsesLeft, &rec.ExpiresAt, &label, &createdBy, &rec.CreatedAt, &rec.Revoked); err != nil {
		return EnrollToken{}, err
	}
	rec.PlatformScope = scope.String
	rec.Label = label.String
	rec.CreatedBy = createdBy.String
	return rec, nil
}
