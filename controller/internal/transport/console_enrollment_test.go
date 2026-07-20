package transport

import (
	"context"
	"encoding/hex"
	"strings"
	"testing"

	"connectrpc.com/connect"

	aurav1 "github.com/aura/controller/gen/aura/v1"
)

// TestBuildInstallCommand 覆盖 install_command 平台分支（install-command-spec.md §6）：windows →
// PowerShell env 注入 iwr|iex 形；其余（linux/darwin/macos/空）→ curl|sh 形，label 非空时附 --label。
func TestBuildInstallCommand(t *testing.T) {
	const (
		tok  = "deadbeefcafe"
		host = "raw.example/aura/main/install"
		ctrl = "vip.example:7443"
	)

	// linux（curl 形，无 label）
	got := buildInstallCommand("linux", tok, host, ctrl, "")
	if !strings.HasPrefix(got, "curl -fsSL https://"+host+"/install.sh | sh -s --") {
		t.Errorf("linux 形前缀不符: %s", got)
	}
	if !strings.Contains(got, "--token "+tok) || !strings.Contains(got, "--controller "+ctrl) {
		t.Errorf("linux 形缺 token/controller: %s", got)
	}
	if strings.Contains(got, "--label") {
		t.Errorf("空 label 不应出现 --label: %s", got)
	}

	// linux + label（带引号）
	if got := buildInstallCommand("linux", tok, host, ctrl, "工位A"); !strings.Contains(got, `--label "工位A"`) {
		t.Errorf("label 应带引号附加: %s", got)
	}

	// 空 platform_scope（不限）、darwin、macos → 均 curl 形
	for _, p := range []string{"", "darwin", "macos"} {
		if got := buildInstallCommand(p, tok, host, ctrl, ""); !strings.HasPrefix(got, "curl ") {
			t.Errorf("platform_scope=%q 应 curl 形: %s", p, got)
		}
	}

	// windows → iwr|iex 形 + env 前置注入
	got = buildInstallCommand("windows", tok, host, ctrl, "")
	if !strings.Contains(got, "iwr https://"+host+"/install.ps1 -useb | iex") {
		t.Errorf("windows 形缺 iwr|iex: %s", got)
	}
	if !strings.Contains(got, `$env:AURA_TOKEN="`+tok+`"`) || !strings.Contains(got, `$env:AURA_CONTROLLER="`+ctrl+`"`) {
		t.Errorf("windows 形缺 env 注入: %s", got)
	}
}

// TestNewEnrollToken 校验 token 形态：64 hex 字符（crypto/rand 32B）+ 两次生成不重复。
func TestNewEnrollToken(t *testing.T) {
	a, err := newEnrollToken()
	if err != nil {
		t.Fatalf("newEnrollToken: %v", err)
	}
	if len(a) != 64 {
		t.Errorf("token 长度 = %d, want 64 (32B hex)", len(a))
	}
	if _, err := hex.DecodeString(a); err != nil {
		t.Errorf("token 非合法 hex: %v", err)
	}
	if b, _ := newEnrollToken(); a == b {
		t.Error("两次生成 token 相同，crypto/rand 应唯一")
	}
}

// TestEnrollHandlersNilStore 校验无 PG（store nil，纯内存模式）降级：Generate/Revoke/Rotate →
// Unavailable（token 无处落库/校验）；List → 空列表 + nil err（治理表无持久 token，诚实降级）；
// ValidateToken → 非 nil error。裸构造 &ConsoleServiceServer{} 即 store 为 nil。
func TestEnrollHandlersNilStore(t *testing.T) {
	s := &ConsoleServiceServer{}
	ctx := context.Background()

	if _, err := s.GenerateEnrollToken(ctx, connect.NewRequest(&aurav1.GenerateEnrollTokenRequest{})); connect.CodeOf(err) != connect.CodeUnavailable {
		t.Errorf("GenerateEnrollToken nil store code = %v, want Unavailable", connect.CodeOf(err))
	}
	if _, err := s.RevokeEnrollToken(ctx, connect.NewRequest(&aurav1.RevokeEnrollTokenRequest{Token: "x"})); connect.CodeOf(err) != connect.CodeUnavailable {
		t.Errorf("RevokeEnrollToken nil store code = %v, want Unavailable", connect.CodeOf(err))
	}
	if _, err := s.RotateEnrollToken(ctx, connect.NewRequest(&aurav1.RotateEnrollTokenRequest{Token: "x"})); connect.CodeOf(err) != connect.CodeUnavailable {
		t.Errorf("RotateEnrollToken nil store code = %v, want Unavailable", connect.CodeOf(err))
	}
	resp, err := s.ListEnrollTokens(ctx, connect.NewRequest(&aurav1.ListEnrollTokensRequest{}))
	if err != nil {
		t.Errorf("ListEnrollTokens nil store err = %v, want nil (空列表降级)", err)
	} else if n := len(resp.Msg.GetTokens()); n != 0 {
		t.Errorf("ListEnrollTokens nil store tokens = %d, want 0", n)
	}
	if _, _, err := s.ValidateToken(ctx, "x", "linux"); err == nil {
		t.Error("ValidateToken nil store 应返回 error")
	}
}
