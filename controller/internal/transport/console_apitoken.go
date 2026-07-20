package transport

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"

	aurav1 "github.com/aura/controller/gen/aura/v1"
	"github.com/aura/controller/internal/store"
)

// M15 API 访问令牌治理 handler（CreateApiToken / ListApiTokens / RevokeApiToken）。管控 bearer 令牌
// 从 env 静态三档（批E C1）扩为 DB 实体：名字身份/档位/项目归属/过期/吊销/最近使用。仅 admin 档可用
// （ConsoleService 无逐 RPC 档位门控，故本组 handler 自持 requireAdmin 守卫）；项目 admin 只治理本项目
// 令牌（创建强制归本项目、列表/吊销限本项目——store 层 SQL 单点收口 + handler 层双保险）。明文 secret
// 仅创建响应返回一次，服务端只存 sha256 哈希（高熵随机秘密无需慢哈希）。ConsoleService handler 分文件
// 纪律：本文件专注 API 令牌治理，与 console_enrollment.go（enroll token）互斥零交集。

const (
	// apiTokenPrefix 是明文令牌前缀（辨识 + 与其它 bearer 区分；非安全边界）。
	apiTokenPrefix = "aura_"
	// apiTokenSecretBytes 是随机主体字节数（32B=256bit 熵，hex 展开 64 字符）。
	apiTokenSecretBytes = 32
	// apiTokenHintLen 是列表展示的明文前缀提示长度（不足以重建秘密；含 prefix）。
	apiTokenHintLen = 12
)

// requireAdmin 守卫：仅 admin 档令牌可治理 API 令牌（ro/ops 拒绝）。空 scope（env 单 token 部署/内部
// 裸 ctx）视同 admin——与 CheckDispatchScope「空 scope 放行」兼容语义一致。
func requireAdmin(ctx context.Context) error {
	switch ScopeFromContext(ctx) {
	case ScopeReadOnly, ScopeOps:
		return connect.NewError(connect.CodePermissionDenied,
			errors.New("API token administration requires an admin-scope token"))
	}
	return nil
}

// CreateApiToken 创建一枚 DB 实体令牌：生成随机 secret → sha256 存哈希 → 明文仅此响应返回一次。
// scope 必填三档之一；name 必填（审计身份）；ttl=0 永不过期。M15 项目隔离：项目 admin 令牌只能创建
// 本项目令牌（强制覆写请求 project——防越权造全域/他项目令牌提权）；全域 admin 可指定任意 project。
func (s *ConsoleServiceServer) CreateApiToken(
	ctx context.Context,
	req *connect.Request[aurav1.CreateApiTokenRequest],
) (*connect.Response[aurav1.CreateApiTokenResponse], error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	if s.store == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("api token store not configured (PostgreSQL required)"))
	}
	m := req.Msg
	if m.GetName() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("token name is required"))
	}
	scope := m.GetScope()
	switch scope {
	case ScopeReadOnly, ScopeOps, ScopeAdmin:
	default:
		return nil, connect.NewError(connect.CodeInvalidArgument,
			fmt.Errorf("invalid scope %q (want ro|ops|admin)", scope))
	}

	// 项目视界强制：项目 admin 只能造本项目令牌（无法自造全域或他项目令牌越权）；全域 admin 随请求。
	project := m.GetProject()
	if tokProject := IdentityFromContext(ctx).Project; tokProject != "" {
		project = tokProject
	}

	secret, err := newAPITokenSecret()
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	sum := sha256.Sum256([]byte(secret))
	hint := secret
	if len(hint) > apiTokenHintLen {
		hint = hint[:apiTokenHintLen]
	}

	var expiresAt time.Time
	if ttl := m.GetTtlSecs(); ttl > 0 {
		expiresAt = time.Now().Add(time.Duration(ttl) * time.Second)
	}

	rec := store.ApiToken{
		ID:         uuid.NewString(),
		Name:       m.GetName(),
		SecretHash: hex.EncodeToString(sum[:]),
		SecretHint: hint,
		Scope:      scope,
		Project:    project,
		CreatedBy:  m.GetWho(),
		ExpiresAt:  expiresAt,
	}
	if err := s.store.InsertApiToken(ctx, rec); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("create api token: %w", err))
	}
	// 回读创建时刻（DB DEFAULT now()）：直接以本地时刻近似（毫秒精度，展示用；避免额外一次点查）。
	rec.CreatedAt = time.Now()
	return connect.NewResponse(&aurav1.CreateApiTokenResponse{
		Id:     rec.ID,
		Secret: secret,
		Info:   apiTokenInfo(rec),
	}), nil
}

// ListApiTokens 列举 API 令牌（治理表；不含 secret/哈希）。项目 admin 仅见本项目令牌（store 层 SQL
// WHERE project 过滤）。无 PG → 空列表（诚实降级，同 ListEnrollTokens）。
func (s *ConsoleServiceServer) ListApiTokens(
	ctx context.Context,
	_ *connect.Request[aurav1.ListApiTokensRequest],
) (*connect.Response[aurav1.ListApiTokensResponse], error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	if s.store == nil {
		return connect.NewResponse(&aurav1.ListApiTokensResponse{}), nil
	}
	toks, err := s.store.ListApiTokens(ctx, IdentityFromContext(ctx).Project)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("list api tokens: %w", err))
	}
	infos := make([]*aurav1.ApiTokenInfo, 0, len(toks))
	for _, t := range toks {
		infos = append(infos, apiTokenInfo(t))
	}
	return connect.NewResponse(&aurav1.ListApiTokensResponse{Tokens: infos}), nil
}

// RevokeApiToken 吊销一枚 API 令牌（立即失效：BearerMiddleware 查验 WHERE NOT revoked）。项目 admin
// 仅能吊本项目令牌（store 层 SQL 单点约束）。不存在/已吊销/越界 → revoked=false。无 PG → Unavailable。
func (s *ConsoleServiceServer) RevokeApiToken(
	ctx context.Context,
	req *connect.Request[aurav1.RevokeApiTokenRequest],
) (*connect.Response[aurav1.RevokeApiTokenResponse], error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	if s.store == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("api token store not configured (PostgreSQL required)"))
	}
	revoked, err := s.store.RevokeApiToken(ctx, req.Msg.GetId(), IdentityFromContext(ctx).Project)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("revoke api token: %w", err))
	}
	return connect.NewResponse(&aurav1.RevokeApiTokenResponse{Revoked: revoked}), nil
}

// apiTokenInfo 把 store.ApiToken 投影为 proto ApiTokenInfo（不含 secret/哈希）。零值时刻还原 0（毫秒）。
func apiTokenInfo(t store.ApiToken) *aurav1.ApiTokenInfo {
	info := &aurav1.ApiTokenInfo{
		Id:         t.ID,
		Name:       t.Name,
		Scope:      t.Scope,
		Project:    t.Project,
		SecretHint: t.SecretHint,
		CreatedMs:  msOrZero(t.CreatedAt),
		Revoked:    t.Revoked,
		CreatedBy:  t.CreatedBy,
	}
	info.ExpiresAtMs = msOrZero(t.ExpiresAt)
	info.LastUsedMs = msOrZero(t.LastUsedAt)
	return info
}

// msOrZero 返回毫秒时间戳；零值时刻（NULL 还原）返回 0（proto 语义：0=永不过期/从未使用）。
func msOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

// newAPITokenSecret 生成一枚不透明明文令牌：prefix + crypto/rand 32B hex（高熵，鉴权以哈希点查恒时）。
func newAPITokenSecret() (string, error) {
	b := make([]byte, apiTokenSecretBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate api token: %w", err)
	}
	return apiTokenPrefix + hex.EncodeToString(b), nil
}
