//go:build e2e

package e2e

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	aurav1 "github.com/aura/controller/gen/aura/v1"
)

// TestProjectIsolation 验证 M15 多租户隔离矩阵（M16 T1.2 场景②，纯管理面 API 无需节点）：
// 项目令牌越权造他项目令牌被强制归本项目、列表视界过滤、ro 档治理 403。
func TestProjectIsolation(t *testing.T) {
	ctx := context.Background()
	admin := h.consoleClient()

	// 1) 全域 admin 建 team-a 项目 admin 令牌 —— 归属应为 team-a。
	ta, err := admin.CreateApiToken(ctx, connect.NewRequest(&aurav1.CreateApiTokenRequest{
		Name: "e2e-team-a", Scope: "admin", Project: "team-a", Who: "e2e",
	}))
	if err != nil {
		t.Fatalf("create team-a token: %v", err)
	}
	if ta.Msg.Info.Project != "team-a" {
		t.Fatalf("team-a token project = %q, want team-a", ta.Msg.Info.Project)
	}
	teamA := h.consoleClientWithToken(ta.Msg.Secret)
	defer h.revokeToken(t, ta.Msg.Id)

	// 2) team-a 令牌建令牌指定 project=team-b → 应被强制归 team-a（禁越权造他项目令牌）。
	nested, err := teamA.CreateApiToken(ctx, connect.NewRequest(&aurav1.CreateApiTokenRequest{
		Name: "e2e-nested", Scope: "ops", Project: "team-b", Who: "e2e",
	}))
	if err != nil {
		t.Fatalf("team-a create nested token: %v", err)
	}
	defer h.revokeToken(t, nested.Msg.Id)
	if nested.Msg.Info.Project != "team-a" {
		t.Fatalf("nested token forced project = %q, want team-a (越权造他项目令牌未被拦)", nested.Msg.Info.Project)
	}

	// 3) team-a 令牌 ListApiTokens → 视界只含 team-a 项目令牌。
	list, err := teamA.ListApiTokens(ctx, connect.NewRequest(&aurav1.ListApiTokensRequest{}))
	if err != nil {
		t.Fatalf("team-a list tokens: %v", err)
	}
	for _, tok := range list.Msg.Tokens {
		if tok.Project != "team-a" {
			t.Fatalf("team-a 视界泄漏他项目令牌: id=%s project=%q", tok.Id, tok.Project)
		}
	}

	// 4) ro 档令牌治理（ListApiTokens 需 admin）→ PermissionDenied（requireAdmin）。
	ro, err := admin.CreateApiToken(ctx, connect.NewRequest(&aurav1.CreateApiTokenRequest{
		Name: "e2e-ro", Scope: "ro", Who: "e2e",
	}))
	if err != nil {
		t.Fatalf("create ro token: %v", err)
	}
	defer h.revokeToken(t, ro.Msg.Id)
	roClient := h.consoleClientWithToken(ro.Msg.Secret)
	_, err = roClient.ListApiTokens(ctx, connect.NewRequest(&aurav1.ListApiTokensRequest{}))
	if got := connect.CodeOf(err); got != connect.CodePermissionDenied {
		t.Fatalf("ro token ListApiTokens code = %v, want PermissionDenied", got)
	}
}

// revokeToken 吊销测试令牌（清理，避免测试库令牌堆积）。
func (h *harness) revokeToken(t *testing.T, id string) {
	t.Helper()
	_, err := h.consoleClient().RevokeApiToken(context.Background(), connect.NewRequest(&aurav1.RevokeApiTokenRequest{
		Id: id, Who: "e2e-cleanup",
	}))
	if err != nil {
		t.Logf("revoke token %s (cleanup, non-fatal): %v", id, err)
	}
}
