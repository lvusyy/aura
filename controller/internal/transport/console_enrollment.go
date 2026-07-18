package transport

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"time"

	"connectrpc.com/connect"

	aurav1 "github.com/aura/controller/gen/aura/v1"
	"github.com/aura/controller/internal/store"
)

// M12-P1 设备接入 enrollment token 治理 handler（TASK-004，替 TASK-001 stub）。
// GenerateEnrollToken / RevokeEnrollToken / RotateEnrollToken / ListEnrollTokens 四 RPC + ValidateToken
// pub 函数（供 TASK-006 /v1/enroll 端点）。token 短期（default 1h）+ 限次（default 1）+ 可吊销 +
// 权限最小（platform_scope），写共享 PG，两副本等价校验（Locked-5）；ConsumeToken 原子扣减保并发竞态
// 安全。ConsoleService handler 全落 console_*.go（console.go:20-21 分文件设计）——本文件专注 token 治理，
// UpdateNodeMeta 分置 console_node.go（TASK-005 领地），各真实现任务替换独立文件避免同 struct 并行写冲突。

const (
	// defaultEnrollTTLSecs / defaultEnrollUses 是 GenerateEnrollToken 的应用层策略默认（非 DB 列 DEFAULT；
	// design §2.1）：短期 1h + 限次 1。console 生成 UI 可覆盖（req.ttl_secs / req.uses）。
	defaultEnrollTTLSecs int64 = 3600
	defaultEnrollUses    int32 = 1
)

// GenerateEnrollToken 生成一次性 enrollment join token（短期 + 限次 + 权限最小 platform_scope）：
// crypto/rand token → InsertToken（ttl/uses 应用层默认，platform_scope/label/who from req）→ 按
// platform_scope 组装 install_command（curl|sh / iwr|iex 形）。无 PG → Unavailable（token 无处落库）。
func (s *ConsoleServiceServer) GenerateEnrollToken(
	ctx context.Context,
	req *connect.Request[aurav1.GenerateEnrollTokenRequest],
) (*connect.Response[aurav1.GenerateEnrollTokenResponse], error) {
	if s.store == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("enrollment token store not configured (PostgreSQL required)"))
	}
	m := req.Msg

	ttlSecs := m.GetTtlSecs()
	if ttlSecs <= 0 {
		ttlSecs = defaultEnrollTTLSecs
	}
	uses := m.GetUses()
	if uses <= 0 {
		uses = defaultEnrollUses
	}

	token, err := newEnrollToken()
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	expiresAt := time.Now().Add(time.Duration(ttlSecs) * time.Second)
	if err := s.store.InsertToken(ctx, store.EnrollToken{
		Token:         token,
		PlatformScope: m.GetPlatformScope(),
		UsesLeft:      uses,
		ExpiresAt:     expiresAt,
		Label:         m.GetLabel(),
		CreatedBy:     m.GetWho(),
	}); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	releaseHost, controller := enrollHostConfig()
	return connect.NewResponse(&aurav1.GenerateEnrollTokenResponse{
		Token:          token,
		ExpiresAtMs:    expiresAt.UnixMilli(),
		InstallCommand: buildInstallCommand(m.GetPlatformScope(), token, releaseHost, controller, m.GetLabel()),
	}), nil
}

// RevokeEnrollToken 吊销一个 join token（立即失效，反连准入拒绝）。幂等：token 不存在/已吊销返回
// revoked=false（RevokeEnrollTokenResponse 语义）。无 PG → Unavailable。
func (s *ConsoleServiceServer) RevokeEnrollToken(
	ctx context.Context,
	req *connect.Request[aurav1.RevokeEnrollTokenRequest],
) (*connect.Response[aurav1.RevokeEnrollTokenResponse], error) {
	if s.store == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("enrollment token store not configured (PostgreSQL required)"))
	}
	revoked, err := s.store.RevokeToken(ctx, req.Msg.GetToken())
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&aurav1.RevokeEnrollTokenResponse{Revoked: revoked}), nil
}

// RotateEnrollToken 轮换一个 join token（吊销旧 + 生成新；SC-5）：读旧 token 承继 platform_scope/label
// 与原授予时长（ttl = 旧 expires_at - created_at，回落默认）、剩余次数（uses_left，至少 1 保可用）→
// 吊销旧 → 生成新 token 落库 → 组装 install_command。旧 token 立即失效，新 token 接替。旧 token 不存在
// → NotFound；无 PG → Unavailable。
func (s *ConsoleServiceServer) RotateEnrollToken(
	ctx context.Context,
	req *connect.Request[aurav1.RotateEnrollTokenRequest],
) (*connect.Response[aurav1.RotateEnrollTokenResponse], error) {
	if s.store == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("enrollment token store not configured (PostgreSQL required)"))
	}
	old := req.Msg.GetToken()

	prev, err := s.store.GetToken(ctx, old)
	if err != nil {
		if store.IsNotFound(err) {
			return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("enrollment token not found"))
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	// 承继旧 token 原授予窗（design：继承 ttl）：ttl = expires_at - created_at；已过期/异常回落默认。
	ttl := prev.ExpiresAt.Sub(prev.CreatedAt)
	if ttl <= 0 {
		ttl = time.Duration(defaultEnrollTTLSecs) * time.Second
	}
	// 承继剩余次数（至少 1，保新 token 可用——旧 token 若已耗尽仍轮换出可用新 token）。
	uses := prev.UsesLeft
	if uses <= 0 {
		uses = defaultEnrollUses
	}

	if _, err := s.store.RevokeToken(ctx, old); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	token, err := newEnrollToken()
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	expiresAt := time.Now().Add(ttl)
	if err := s.store.InsertToken(ctx, store.EnrollToken{
		Token:         token,
		PlatformScope: prev.PlatformScope,
		UsesLeft:      uses,
		ExpiresAt:     expiresAt,
		Label:         prev.Label,
		CreatedBy:     req.Msg.GetWho(),
	}); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	releaseHost, controller := enrollHostConfig()
	return connect.NewResponse(&aurav1.RotateEnrollTokenResponse{
		Token:          token,
		ExpiresAtMs:    expiresAt.UnixMilli(),
		InstallCommand: buildInstallCommand(prev.PlatformScope, token, releaseHost, controller, prev.Label),
	}), nil
}

// ListEnrollTokens 列举 enrollment token（console 治理表）：投影 enrollment_tokens 行为 EnrollTokenInfo
// （token/platform_scope/uses_left/expires_at_ms/revoked；派生 status 由 console 从三事实 revoked+
// expires_at+uses_left 计算，design §2.2 不落存储）。无 PG（纯内存）→ 空列表（治理表无持久 token，
// 诚实降级不报错，console 渲染空表）。
func (s *ConsoleServiceServer) ListEnrollTokens(
	ctx context.Context,
	req *connect.Request[aurav1.ListEnrollTokensRequest],
) (*connect.Response[aurav1.ListEnrollTokensResponse], error) {
	if s.store == nil {
		return connect.NewResponse(&aurav1.ListEnrollTokensResponse{}), nil
	}
	toks, err := s.store.ListTokens(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	infos := make([]*aurav1.EnrollTokenInfo, 0, len(toks))
	for _, t := range toks {
		infos = append(infos, &aurav1.EnrollTokenInfo{
			Token:         t.Token,
			PlatformScope: t.PlatformScope,
			UsesLeft:      t.UsesLeft,
			ExpiresAtMs:   t.ExpiresAt.UnixMilli(),
			Revoked:       t.Revoked,
		})
	}
	return connect.NewResponse(&aurav1.ListEnrollTokensResponse{Tokens: infos}), nil
}

// ValidateToken 校验并原子消费一个 enrollment token（供 TASK-006 /v1/enroll 端点编排「验 token→签
// cert」，design §2.3）：封装 store.ConsumeToken——验有效（未过期/uses_left>0/平台匹配/未吊销）并原子
// 扣减一次，返回 token 携带的 label（enroll 时赋新节点初始 label）。token 无效返回 store.ErrTokenInvalid
// （调用方映射 401/403）。两副本并发消费同一 token 由单 SQL UPDATE..RETURNING 天然串行，绝不超用。
// pub 方法：TASK-006 的 enroll 端点持 *ConsoleServiceServer 引用即可调；亦可直调 store.ConsumeToken
// （同签名）——二者等价，本方法是语义入口。
func (s *ConsoleServiceServer) ValidateToken(ctx context.Context, token, platform string) (string, error) {
	if s.store == nil {
		return "", errors.New("enrollment token store not configured")
	}
	return s.store.ConsumeToken(ctx, token, platform)
}

// newEnrollToken 生成一个不透明 enrollment token：crypto/rand 32B → hex（64 hex 字符；design §2.2）。
func newEnrollToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate enrollment token: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// enrollHostConfig 解析组装 install_command 所需的服务端部署配置，经环境变量注入（部署期配置，沿
// AURA_* 惯例——minio.go/pve.go 先例；内网地址不硬编码入公开 repo，context.md Locked-3C）：
//
//	AURA_RELEASE_HOST        : install.sh/install.ps1 托管 host+路径前缀
//	                           （形如 raw.githubusercontent.com/<org>/aura/main/install）
//	AURA_CONTROLLER_ENDPOINT : 对外 mTLS 反连端点 HOST:7443（HA 用 VIP）
//
// 未配置回落可辨识占位串（非误导性内网默认）：install_command 形态完整，部署前占位替换即可，不阻塞
// 开发/单测。
func enrollHostConfig() (releaseHost, controller string) {
	releaseHost = os.Getenv("AURA_RELEASE_HOST")
	if releaseHost == "" {
		releaseHost = "<release-host>"
	}
	controller = os.Getenv("AURA_CONTROLLER_ENDPOINT")
	if controller == "" {
		controller = "<VIP:7443>"
	}
	return releaseHost, controller
}

// buildInstallCommand 按平台组装一键安装命令（install-command-spec.md §6）：windows → PowerShell env
// 前置注入 iwr|iex 形（copy-paste 稳健）；linux/darwin/macos/空 → curl|sh 形（label 非空时附 --label）。
// 纯函数（无 IO），供单测直接覆盖平台分支。
func buildInstallCommand(platformScope, token, releaseHost, controller, label string) string {
	if platformScope == "windows" {
		return fmt.Sprintf(`$env:AURA_TOKEN=%q; $env:AURA_CONTROLLER=%q; iwr https://%s/install.ps1 -useb | iex`,
			token, controller, releaseHost)
	}
	cmd := fmt.Sprintf("curl -fsSL https://%s/install.sh | sh -s -- --token %s --controller %s",
		releaseHost, token, controller)
	if label != "" {
		cmd += fmt.Sprintf(" --label %q", label)
	}
	return cmd
}
