package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

// —— M15 API 访问令牌 store：api_tokens 表访问 ——————————————————————————————————————————
// 管控 bearer 令牌 DB 实体（名字身份/档位/项目归属/过期/吊销/最近使用）。应用层策略（secret 生成、
// hint 截取、项目 admin 视界约束的取值）归 transport handler；本文件只做表访问原语（数据结构存事实
// 不存策略，同 enrollment.go 纪律）。两副本共享 PG（Locked-5）：任一副本建/吊，另一副本
// BearerMiddleware 点查即时可见。

// ApiToken 是 api_tokens 表的一行。SecretHash 为 sha256(明文) hex（本结构不携明文，明文仅创建响应
// 一次性返回）；ExpiresAt/LastUsedAt 零值=NULL（永不过期/从未使用），读写路径双向还原。
type ApiToken struct {
	ID         string
	Name       string
	SecretHash string // sha256(明文) hex
	SecretHint string // 明文前缀提示（列表辨识）
	Scope      string // ro | ops | admin（批E C1 三档字面）
	Project    string // ''=全域；非空=项目令牌（M15 唯一隔离规则的主体侧）
	CreatedBy  string
	CreatedAt  time.Time // 读路径回填（写入取 SQL DEFAULT now()）
	ExpiresAt  time.Time // 零值=永不过期（NULL）
	LastUsedAt time.Time // 零值=从未使用（NULL）
	Revoked    bool
}

// InsertApiToken 插入一条令牌（created_at 取 SQL DEFAULT now()、revoked 取 DEFAULT false）。
// ExpiresAt 零值落 NULL（永不过期）。
func (s *PGStore) InsertApiToken(ctx context.Context, t ApiToken) error {
	id, err := pgUUID(t.ID)
	if err != nil {
		return err
	}
	expires := pgtype.Timestamptz{Time: t.ExpiresAt, Valid: !t.ExpiresAt.IsZero()}
	const q = `INSERT INTO api_tokens (id, name, secret_hash, secret_hint, scope, project, created_by, expires_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`
	if _, err := s.pool.Exec(ctx, q, id, t.Name, t.SecretHash, t.SecretHint, t.Scope, t.Project, t.CreatedBy, expires); err != nil {
		return fmt.Errorf("insert api token: %w", err)
	}
	return nil
}

// LookupApiToken 按 secret 哈希点查一条**当前有效**令牌（BearerMiddleware 鉴权热路径）：未吊销且
// 未过期（有效性判据收敛在 SQL 单点，无 TOCTOU）。未命中返回 pgx.ErrNoRows（调用方经 store.IsNotFound
// 判定 401，不与 DB 故障混淆）。UNIQUE(secret_hash) 隐式索引支撑点查。
func (s *PGStore) LookupApiToken(ctx context.Context, secretHash string) (ApiToken, error) {
	const q = `SELECT id, name, secret_hash, secret_hint, scope, project, created_by, created_at, expires_at, last_used_at, revoked
FROM api_tokens
WHERE secret_hash = $1 AND NOT revoked AND (expires_at IS NULL OR expires_at > now())`
	return scanApiToken(s.pool.QueryRow(ctx, q, secretHash))
}

// TouchApiToken 节流回写最近使用时刻：WHERE 自含 60s 粒度节流条件，高频调用绝大多数为 no-op 空
// UPDATE（不产生写放大）。best-effort：失败仅调用方告警，绝不阻断鉴权链路。
func (s *PGStore) TouchApiToken(ctx context.Context, id string) error {
	tid, err := pgUUID(id)
	if err != nil {
		return err
	}
	const q = `UPDATE api_tokens SET last_used_at = now()
WHERE id = $1 AND (last_used_at IS NULL OR last_used_at < now() - interval '60 seconds')`
	if _, err := s.pool.Exec(ctx, q, tid); err != nil {
		return fmt.Errorf("touch api token: %w", err)
	}
	return nil
}

// ListApiTokens 全量列举（console 治理表读路径），按 created_at DESC（最新在前）。project 非空=仅列
// 该项目令牌（项目 admin 视界约束，取值由 transport 从请求身份注入）；空=全量（全域视界）。admin
// 小表全列不分页（同 enrollment ListTokens 先例）。
func (s *PGStore) ListApiTokens(ctx context.Context, project string) ([]ApiToken, error) {
	const q = `SELECT id, name, secret_hash, secret_hint, scope, project, created_by, created_at, expires_at, last_used_at, revoked
FROM api_tokens WHERE ($1 = '' OR project = $1) ORDER BY created_at DESC`
	rows, err := s.pool.Query(ctx, q, project)
	if err != nil {
		return nil, fmt.Errorf("query api tokens: %w", err)
	}
	defer rows.Close()

	var out []ApiToken
	for rows.Next() {
		rec, err := scanApiToken(rows)
		if err != nil {
			return nil, fmt.Errorf("scan api token: %w", err)
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate api tokens: %w", err)
	}
	return out, nil
}

// RevokeApiToken 吊销一枚令牌（幂等）：仅当存在且未吊销时置 revoked=true。project 非空=仅允许吊本
// 项目令牌（项目 admin 约束在 SQL 单点收口，无 TOCTOU）。返回是否发生吊销（不存在/已吊销/越界
// → false，契合 RevokeApiTokenResponse.revoked 语义）。
func (s *PGStore) RevokeApiToken(ctx context.Context, id, project string) (bool, error) {
	tid, err := pgUUID(id)
	if err != nil {
		return false, err
	}
	const q = `UPDATE api_tokens SET revoked = true WHERE id = $1 AND NOT revoked AND ($2 = '' OR project = $2)`
	tag, err := s.pool.Exec(ctx, q, tid, project)
	if err != nil {
		return false, fmt.Errorf("revoke api token: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// scanApiToken 从一行扫描 ApiToken（列序须为 id,name,secret_hash,secret_hint,scope,project,created_by,
// created_at,expires_at,last_used_at,revoked）。expires_at/last_used_at 可空，NULL 还原零值。
func scanApiToken(sc rowScanner) (ApiToken, error) {
	var (
		rec      ApiToken
		id       pgtype.UUID
		expires  pgtype.Timestamptz
		lastUsed pgtype.Timestamptz
	)
	if err := sc.Scan(&id, &rec.Name, &rec.SecretHash, &rec.SecretHint, &rec.Scope, &rec.Project, &rec.CreatedBy,
		&rec.CreatedAt, &expires, &lastUsed, &rec.Revoked); err != nil {
		return ApiToken{}, err
	}
	rec.ID = uuidString(id)
	if expires.Valid {
		rec.ExpiresAt = expires.Time
	}
	if lastUsed.Valid {
		rec.LastUsedAt = lastUsed.Time
	}
	return rec, nil
}

// —— M15 节点项目归属读写（隔离规则的客体侧）————————————————————————————————————————————

// NodeProject 点查节点项目归属（网关/派发等节点寻址口的隔离判据）。节点不存在视同未归属（''）——
// 后续 Ready/存在性检查自会给出 404，本方法不重复承担存在性语义。
func (s *PGStore) NodeProject(ctx context.Context, nodeID string) (string, error) {
	id, err := pgUUID(nodeID)
	if err != nil {
		return "", err
	}
	var project string
	err = s.pool.QueryRow(ctx, `SELECT COALESCE(project, '') FROM nodes WHERE id = $1`, id).Scan(&project)
	if IsNotFound(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("query node project: %w", err)
	}
	return project, nil
}

// ProjectNodeIDs 列举某项目全部节点 ID（列表面过滤用：tasks/traces/agent obs 的 WHERE node_id=ANY）。
// 舰队规模有界（fleet 治理常态个位数~百级），全列无分页。
func (s *PGStore) ProjectNodeIDs(ctx context.Context, project string) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT id FROM nodes WHERE COALESCE(project, '') = $1`, project)
	if err != nil {
		return nil, fmt.Errorf("query project nodes: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var id pgtype.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan project node id: %w", err)
		}
		out = append(out, uuidString(id))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate project nodes: %w", err)
	}
	return out, nil
}
