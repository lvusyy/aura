package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// ErrVisionRevisionConflict 表示乐观并发更新时 revision 不匹配（另一管理员已先行修改）。
var ErrVisionRevisionConflict = errors.New("vision settings revision conflict")

// VisionSettings 是 vision_settings 单行表的 Go 映射。
type VisionSettings struct {
	Mode          string
	LocalEndpoint string
	CloudEndpoint string
	APIKey        string // 仅服务端内部消费，对外 RPC 绝不回传明文
	APIKeyHint    string // 脱敏前缀提示（如 ts_ab12…）
	Model         string
	CaptionMode   string
	NoulThreshold float64
	EnableBox     bool
	Revision      int64
	UpdatedAt     time.Time
}

// DefaultVisionSettings 返回缺省 Jev 视觉配置（默认 auto 本地优先，localEndpoint 从 AURA_DETECTOR_ENDPOINT 注入）。
func DefaultVisionSettings(defaultLocalEndpoint string) VisionSettings {
	ep := strings.TrimRight(strings.TrimSpace(defaultLocalEndpoint), "/")
	if ep == "" {
		ep = "http://192.168.22.225:30808"
	}
	return VisionSettings{
		Mode:          "auto",
		LocalEndpoint: ep,
		CloudEndpoint: "https://api.typesafe.ai/v1/systemone",
		Model:         "jev-latest",
		CaptionMode:   "none",
		NoulThreshold: 0.50,
		EnableBox:     true,
		Revision:      1,
		UpdatedAt:     time.Now().UTC(),
	}
}

// MaskAPIKeyHint 生成云端 API 令牌的脱敏前缀提示。
func MaskAPIKeyHint(key string) string {
	k := strings.TrimSpace(key)
	if k == "" {
		return ""
	}
	if len(k) < 2 {
		return k + "…"
	}
	if len(k) <= 8 {
		return k[:2] + "…"
	}
	return k[:8] + "…"
}

// GetVisionSettings 读取单行 vision_settings；未初始化时返回 pgx.ErrNoRows（调用方用 DefaultVisionSettings 兜底）。
func (s *PGStore) GetVisionSettings(ctx context.Context) (VisionSettings, error) {
	const q = `SELECT mode, local_endpoint, cloud_endpoint, api_key, api_key_hint, model, caption_mode, noul_threshold, enable_box, revision, updated_at
FROM vision_settings WHERE id = 1`
	var out VisionSettings
	err := s.pool.QueryRow(ctx, q).Scan(
		&out.Mode,
		&out.LocalEndpoint,
		&out.CloudEndpoint,
		&out.APIKey,
		&out.APIKeyHint,
		&out.Model,
		&out.CaptionMode,
		&out.NoulThreshold,
		&out.EnableBox,
		&out.Revision,
		&out.UpdatedAt,
	)
	if err != nil {
		return VisionSettings{}, fmt.Errorf("get vision settings: %w", err)
	}
	return out, nil
}

// UpsertVisionSettings 保存单行 vision_settings。
// apiKeyAction: "keep"（保留库内既有 api_key/api_key_hint）| "set"（写入 newAPIKey）| "clear"（清空令牌）。
// 若 expectedRevision > 0 且表中已有行，要求当前 revision == expectedRevision，否则返回 ErrVisionRevisionConflict。
func (s *PGStore) UpsertVisionSettings(
	ctx context.Context,
	in VisionSettings,
	apiKeyAction string,
	newAPIKey string,
) (VisionSettings, error) {
	action := strings.ToLower(strings.TrimSpace(apiKeyAction))
	if action == "" {
		action = "keep"
	}
	var keyToSet, hintToSet string
	switch action {
	case "set":
		keyToSet = strings.TrimSpace(newAPIKey)
		hintToSet = MaskAPIKeyHint(keyToSet)
	case "clear":
		keyToSet = ""
		hintToSet = ""
	case "keep":
		// 首次插入时若选 keep 则为空；更新已有行时 SQL 保留原列值
	default:
		return VisionSettings{}, fmt.Errorf("invalid api_key_action %q", apiKeyAction)
	}

	const q = `INSERT INTO vision_settings (
    id, mode, local_endpoint, cloud_endpoint, api_key, api_key_hint, model, caption_mode, noul_threshold, enable_box, revision, updated_at
) VALUES (
    1, $1, $2, $3, $4, $5, $6, $7, $8, $9, 1, now()
)
ON CONFLICT (id) DO UPDATE SET
    mode           = EXCLUDED.mode,
    local_endpoint = EXCLUDED.local_endpoint,
    cloud_endpoint = EXCLUDED.cloud_endpoint,
    api_key        = CASE WHEN $10 = 'keep' THEN vision_settings.api_key ELSE EXCLUDED.api_key END,
    api_key_hint   = CASE WHEN $10 = 'keep' THEN vision_settings.api_key_hint ELSE EXCLUDED.api_key_hint END,
    model          = EXCLUDED.model,
    caption_mode   = EXCLUDED.caption_mode,
    noul_threshold = EXCLUDED.noul_threshold,
    enable_box     = EXCLUDED.enable_box,
    revision       = vision_settings.revision + 1,
    updated_at     = now()
WHERE ($11 <= 0 OR vision_settings.revision = $11)
RETURNING mode, local_endpoint, cloud_endpoint, api_key, api_key_hint, model, caption_mode, noul_threshold, enable_box, revision, updated_at`

	var out VisionSettings
	err := s.pool.QueryRow(
		ctx,
		q,
		in.Mode,
		in.LocalEndpoint,
		in.CloudEndpoint,
		keyToSet,
		hintToSet,
		in.Model,
		in.CaptionMode,
		in.NoulThreshold,
		in.EnableBox,
		action,
		in.Revision,
	).Scan(
		&out.Mode,
		&out.LocalEndpoint,
		&out.CloudEndpoint,
		&out.APIKey,
		&out.APIKeyHint,
		&out.Model,
		&out.CaptionMode,
		&out.NoulThreshold,
		&out.EnableBox,
		&out.Revision,
		&out.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return VisionSettings{}, ErrVisionRevisionConflict
		}
		return VisionSettings{}, fmt.Errorf("upsert vision settings: %w", err)
	}
	return out, nil
}
