package store

import (
	"context"
	"fmt"
	"time"
)

// Release 是一条发布制品登记（M16 节点 self-update；制品本体在对象存储，本行只存元数据）。
type Release struct {
	Platform  string
	Version   string
	SHA256    string
	Size      int64
	CreatedAt time.Time
}

// UpsertRelease 登记/覆盖一条发布制品（同 platform+version 重复上传为覆盖语义——修正坏制品，
// sha256/size/created_at 随之更新）。上传端点在对象写入成功后调用。
func (s *PGStore) UpsertRelease(ctx context.Context, r Release) error {
	const q = `
INSERT INTO releases (platform, version, sha256, size, created_at)
VALUES ($1, $2, $3, $4, now())
ON CONFLICT (platform, version) DO UPDATE SET
	sha256     = EXCLUDED.sha256,
	size       = EXCLUDED.size,
	created_at = now()`
	if _, err := s.pool.Exec(ctx, q, r.Platform, r.Version, r.SHA256, r.Size); err != nil {
		return fmt.Errorf("upsert release %s/%s: %w", r.Platform, r.Version, err)
	}
	return nil
}

// GetRelease 按 (platform, version) 取一条发布登记；不存在时错误经 store.IsNotFound 可辨
// （%w 保链，errors.Is 穿透包装——api_tokens 同惯例）。
func (s *PGStore) GetRelease(ctx context.Context, platform, version string) (Release, error) {
	const q = `SELECT platform, version, sha256, size, created_at FROM releases WHERE platform = $1 AND version = $2`
	var r Release
	err := s.pool.QueryRow(ctx, q, platform, version).Scan(&r.Platform, &r.Version, &r.SHA256, &r.Size, &r.CreatedAt)
	if err != nil {
		return Release{}, fmt.Errorf("get release %s/%s: %w", platform, version, err)
	}
	return r, nil
}

// ListReleases 列举全部发布登记（最新在前；发布为有界物料——平台数 × 版本数，admin 小表全列不分页，
// 同 enrollment_tokens 惯例）。
func (s *PGStore) ListReleases(ctx context.Context) ([]Release, error) {
	const q = `SELECT platform, version, sha256, size, created_at FROM releases ORDER BY created_at DESC`
	rows, err := s.pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("query releases: %w", err)
	}
	defer rows.Close()

	var out []Release
	for rows.Next() {
		var r Release
		if err := rows.Scan(&r.Platform, &r.Version, &r.SHA256, &r.Size, &r.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan release: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate releases: %w", err)
	}
	return out, nil
}
