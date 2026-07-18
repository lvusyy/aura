package store

import (
	"context"
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestEnrollmentTokenLifecycleIntegration 对真 PG 演练 M12 enrollment token 生命周期（TASK-004）：
// 生成→原子消费（label 回传 + uses_left 扣减）→耗尽拒→过期拒→平台不匹配拒/匹配放行/空 scope 不限→
// 吊销拒/幂等→并发消费仅一次成功（两副本竞态安全核心）→ListTokens。需 AURA_TEST_PG_DSN；缺省 skip
// （沿 pg_integration_test 惯例，无 PG 机器 go test ./internal/store 仍绿）。token 主键唯一后缀避免与
// 既有行/并发碰撞，不清场（白盒点查断言）。
func TestEnrollmentTokenLifecycleIntegration(t *testing.T) {
	dsn := os.Getenv("AURA_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("set AURA_TEST_PG_DSN to run enrollment token integration")
	}
	ctx := context.Background()
	pg, err := NewPGStore(ctx, dsn)
	if err != nil {
		t.Fatalf("NewPGStore: %v", err)
	}
	defer pg.Close()

	mk := func(suffix string) string { return "t04-" + uuid.NewString() + "-" + suffix }

	// —— 消费成功：uses=2 → 首消回 label + uses_left 扣到 1；再消到 0；第三次耗尽拒 ——
	tok := mk("consume")
	if err := pg.InsertToken(ctx, EnrollToken{
		Token: tok, PlatformScope: "", UsesLeft: 2,
		ExpiresAt: time.Now().Add(time.Hour), Label: "工位A", CreatedBy: "tester",
	}); err != nil {
		t.Fatalf("InsertToken: %v", err)
	}
	label, err := pg.ConsumeToken(ctx, tok, "linux")
	if err != nil {
		t.Fatalf("ConsumeToken #1: %v", err)
	}
	if label != "工位A" {
		t.Errorf("ConsumeToken label = %q, want 工位A", label)
	}
	got, err := pg.GetToken(ctx, tok)
	if err != nil {
		t.Fatalf("GetToken: %v", err)
	}
	if got.UsesLeft != 1 {
		t.Errorf("uses_left after 1 consume = %d, want 1", got.UsesLeft)
	}
	if _, err := pg.ConsumeToken(ctx, tok, "linux"); err != nil {
		t.Fatalf("ConsumeToken #2: %v", err)
	}
	if _, err := pg.ConsumeToken(ctx, tok, "linux"); !errors.Is(err, ErrTokenInvalid) {
		t.Errorf("ConsumeToken #3 (exhausted) err = %v, want ErrTokenInvalid", err)
	}

	// —— 过期拒 ——
	expired := mk("expired")
	if err := pg.InsertToken(ctx, EnrollToken{
		Token: expired, UsesLeft: 1, ExpiresAt: time.Now().Add(-time.Minute),
	}); err != nil {
		t.Fatalf("InsertToken expired: %v", err)
	}
	if _, err := pg.ConsumeToken(ctx, expired, "linux"); !errors.Is(err, ErrTokenInvalid) {
		t.Errorf("ConsumeToken expired err = %v, want ErrTokenInvalid", err)
	}

	// —— 平台不匹配拒 / 匹配放行（platform_scope=windows）——
	scoped := mk("scoped")
	if err := pg.InsertToken(ctx, EnrollToken{
		Token: scoped, PlatformScope: "windows", UsesLeft: 2, ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("InsertToken scoped: %v", err)
	}
	if _, err := pg.ConsumeToken(ctx, scoped, "linux"); !errors.Is(err, ErrTokenInvalid) {
		t.Errorf("ConsumeToken platform mismatch err = %v, want ErrTokenInvalid", err)
	}
	if _, err := pg.ConsumeToken(ctx, scoped, "windows"); err != nil {
		t.Errorf("ConsumeToken platform match err = %v, want nil", err)
	}

	// —— 吊销拒 + 幂等 ——
	revoked := mk("revoked")
	if err := pg.InsertToken(ctx, EnrollToken{
		Token: revoked, UsesLeft: 1, ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("InsertToken revoked: %v", err)
	}
	did, err := pg.RevokeToken(ctx, revoked)
	if err != nil || !did {
		t.Fatalf("RevokeToken = (%v, %v), want (true, nil)", did, err)
	}
	if again, _ := pg.RevokeToken(ctx, revoked); again {
		t.Error("RevokeToken 幂等：二次吊销应返回 false")
	}
	if _, err := pg.ConsumeToken(ctx, revoked, "linux"); !errors.Is(err, ErrTokenInvalid) {
		t.Errorf("ConsumeToken revoked err = %v, want ErrTokenInvalid", err)
	}

	// —— 并发消费仅一次成功（uses=1，N goroutine 抢，恰 1 成功；原子扣减杜绝超用，两副本竞态等价）——
	race := mk("race")
	if err := pg.InsertToken(ctx, EnrollToken{
		Token: race, UsesLeft: 1, ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("InsertToken race: %v", err)
	}
	const n = 16
	var success int64
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := pg.ConsumeToken(ctx, race, "linux"); err == nil {
				atomic.AddInt64(&success, 1)
			}
		}()
	}
	wg.Wait()
	if success != 1 {
		t.Errorf("并发消费 uses=1：成功次数 = %d, want 1（原子 UPDATE..RETURNING 杜绝超用）", success)
	}

	// —— ListTokens 含上述插入的 token（不清场，非空即可）——
	all, err := pg.ListTokens(ctx)
	if err != nil {
		t.Fatalf("ListTokens: %v", err)
	}
	if len(all) == 0 {
		t.Error("ListTokens 返回空，应含本测试插入的 token")
	}
}
