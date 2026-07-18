package scheduler

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"github.com/aura/controller/internal/registry"
	"github.com/aura/controller/internal/store"
)

// newLeaseScheduler 构造注入指定 LeaseStore 的纯内存调度器（store=nil）用于租约状态机单测；
// leaseTTL 显式传入以便 TTL 用例控制过期。
func newLeaseScheduler(ttl time.Duration, ls store.LeaseStore) *Scheduler {
	return NewScheduler(registry.NewRegistry(nil), nil, ttl, ls)
}

// forEachLeaseImpl 以 InMemory 与 Redis(miniredis) 双实现分别构造 scheduler 跑同一用例体
//（ha-contract §3.4：既有语义断言双实现同表零删减）。ttl 是租约 TTL（StartTrace 经 Acquire
// 传入）；expire 把活跃租约推进到过期——进程内=真实等待（读时惰性判定 time.Now）；miniredis=
// FastForward 假时钟（原生 PEXPIRE）。原「手动改 expiry」手法随双 map 外置改为短 TTL 构造。
func forEachLeaseImpl(t *testing.T, ttl time.Duration, run func(t *testing.T, s *Scheduler, ls store.LeaseStore, expire func(time.Duration))) {
	t.Run("inmemory", func(t *testing.T) {
		ls := store.NewInMemoryLeaseStore()
		run(t, newLeaseScheduler(ttl, ls), ls, func(d time.Duration) { time.Sleep(d) })
	})
	t.Run("redis", func(t *testing.T) {
		mr := miniredis.RunT(t)
		rs, err := store.NewRedisStore(context.Background(), mr.Addr())
		if err != nil {
			t.Fatalf("NewRedisStore(miniredis): %v", err)
		}
		t.Cleanup(func() { _ = rs.Close() })
		ls := store.NewRedisLeaseStore(rs, ttl)
		run(t, newLeaseScheduler(ttl, ls), ls, mr.FastForward)
	})
}

// TestStartTraceAcquiresLease：StartTrace 即租——返回非空 trace_id，租约载荷（node/who）与活跃性
// 经 LeaseStore.Get 行为断言（原 traces/nodeLeased 双 map 直读断言的行为化改写）。
func TestStartTraceAcquiresLease(t *testing.T) {
	forEachLeaseImpl(t, 30*time.Minute, func(t *testing.T, s *Scheduler, ls store.LeaseStore, _ func(time.Duration)) {
		traceID, err := s.StartTrace("node-a", "recorder-1")
		if err != nil {
			t.Fatalf("StartTrace: unexpected err %v", err)
		}
		if traceID == "" {
			t.Fatal("StartTrace: expected non-empty trace_id")
		}
		lease, ok, gerr := ls.Get(context.Background(), "node-a")
		if gerr != nil || !ok {
			t.Fatalf("Get(node-a) = ok=%v err=%v, want active lease（活跃性=原 expiry 在未来断言）", ok, gerr)
		}
		if lease.Who != "recorder-1" || lease.NodeID != "node-a" {
			t.Errorf("lease fields = {node:%q who:%q}, want {node-a recorder-1}", lease.NodeID, lease.Who)
		}
		if lease.TraceID != traceID {
			t.Errorf("lease.TraceID = %q, want %q (反查独占)", lease.TraceID, traceID)
		}
	})
}

// TestCheckLeaseHolderVsNonHolder：持有者放行 / 非持有者（含空 trace_id）E_BUSY / 无关 node 放行。
func TestCheckLeaseHolderVsNonHolder(t *testing.T) {
	forEachLeaseImpl(t, 30*time.Minute, func(t *testing.T, s *Scheduler, _ store.LeaseStore, _ func(time.Duration)) {
		ctx := context.Background()
		traceID, _ := s.StartTrace("node-a", "recorder-1")

		// 持有者放行（同 trace_id）。
		if err := s.checkLease(ctx, "node-a", traceID); err != nil {
			t.Errorf("holder checkLease: got %v, want nil (放行)", err)
		}
		// 非持有者（他 trace_id）→ E_BUSY。
		if err := s.checkLease(ctx, "node-a", "some-other-trace"); !errors.Is(err, errTraceBusy) {
			t.Errorf("non-holder checkLease: got %v, want errTraceBusy", err)
		}
		// 空 trace_id 的常规 dispatch 对被租 node → E_BUSY（硬隔离）。
		if err := s.checkLease(ctx, "node-a", ""); !errors.Is(err, errTraceBusy) {
			t.Errorf("empty-trace checkLease on leased node: got %v, want errTraceBusy", err)
		}
		// 无租约的其他 node → 放行（常规 dispatch 不受影响）。
		if err := s.checkLease(ctx, "node-b", ""); err != nil {
			t.Errorf("unleased node checkLease: got %v, want nil", err)
		}
	})
}

// TestStopTraceReleasesLease：StopTrace 后 node 恢复放行且租约不复存在（原双 map 清理断言的
// 行为化改写：Get 不存在覆盖正查+反查两面）；未知 trace_id 幂等 no-op。
func TestStopTraceReleasesLease(t *testing.T) {
	forEachLeaseImpl(t, 30*time.Minute, func(t *testing.T, s *Scheduler, ls store.LeaseStore, _ func(time.Duration)) {
		ctx := context.Background()
		traceID, _ := s.StartTrace("node-a", "recorder-1")

		s.StopTrace(traceID)

		if _, ok, _ := ls.Get(ctx, "node-a"); ok {
			t.Error("StopTrace 后租约仍存在（正查/反查未清理）")
		}
		if err := s.checkLease(ctx, "node-a", ""); err != nil {
			t.Errorf("StopTrace 释放后 checkLease: got %v, want nil (放行)", err)
		}
		// 幂等：重复 StopTrace / 未知 trace_id 不 panic、无副作用。
		s.StopTrace(traceID)
		s.StopTrace("never-existed")
	})
}

// TestStopTraceReturnsReleaseStatus：StopTrace 返回真实释放状态（TASK-006 milestone-audit 观察项修正，
// StopTraceResponse.stopped 语义 proto:205）——首次释放活跃租约返 true；再次/未知 trace_id 返 false。
// rest.go StopTrace handler 忠实转发此真值（此前硬编码 Stopped:true 与 proto 契约不符）。
func TestStopTraceReturnsReleaseStatus(t *testing.T) {
	forEachLeaseImpl(t, 30*time.Minute, func(t *testing.T, s *Scheduler, _ store.LeaseStore, _ func(time.Duration)) {
		traceID, _ := s.StartTrace("node-a", "recorder-1")

		if got := s.StopTrace(traceID); got != true {
			t.Errorf("StopTrace(active) = %v, want true (真实释放了活跃租约)", got)
		}
		// 幂等再调：已释放 → false（无租约可释）。
		if got := s.StopTrace(traceID); got != false {
			t.Errorf("StopTrace(already-released) = %v, want false (幂等再调无租约可释)", got)
		}
		// 未知 trace_id → false。
		if got := s.StopTrace("never-existed"); got != false {
			t.Errorf("StopTrace(unknown) = %v, want false", got)
		}
	})
}

// TestLeaseTTLExpiryReleases：租约 TTL 过期后 checkLease 自动视为无租约并放行，租约不复可读
//（进程内惰性释放 / Redis 原生过期）。原「手动改 expiry」手法改为短 TTL 构造 + 时钟推进。
func TestLeaseTTLExpiryReleases(t *testing.T) {
	forEachLeaseImpl(t, 50*time.Millisecond, func(t *testing.T, s *Scheduler, ls store.LeaseStore, expire func(time.Duration)) {
		ctx := context.Background()
		if _, err := s.StartTrace("node-a", "recorder-1"); err != nil {
			t.Fatalf("StartTrace: %v", err)
		}

		expire(60 * time.Millisecond) // 推进过 TTL（等价于原 expiry 置过去）

		// 过期后即便非持有者（空 trace_id）也放行——自动释放。
		if err := s.checkLease(ctx, "node-a", ""); err != nil {
			t.Errorf("expired lease checkLease: got %v, want nil (TTL 自动释放)", err)
		}
		if _, ok, _ := ls.Get(ctx, "node-a"); ok {
			t.Error("过期后租约仍可读（正查/反查未清理）")
		}
	})
}

// TestStartTraceSameNodePerNodeExclusive：node 已被活跃 trace 租用时，二次 StartTrace 同 node →
// E_BUSY；而过期后可重新获租（新 trace_id）。
func TestStartTraceSameNodePerNodeExclusive(t *testing.T) {
	// 独占拒：活跃租约下二次租（长 TTL 消 flake）。
	t.Run("exclusive", func(t *testing.T) {
		forEachLeaseImpl(t, 30*time.Minute, func(t *testing.T, s *Scheduler, _ store.LeaseStore, _ func(time.Duration)) {
			if _, err := s.StartTrace("node-a", "recorder-1"); err != nil {
				t.Fatalf("StartTrace: %v", err)
			}
			if _, err := s.StartTrace("node-a", "recorder-2"); !errors.Is(err, errTraceBusy) {
				t.Errorf("二次租同 node: got %v, want errTraceBusy (per-node 独占)", err)
			}
		})
	})
	// 过期重租：短 TTL 构造（原「手动改 expiry」手法的行为化替代）。
	t.Run("expiry-reacquire", func(t *testing.T) {
		forEachLeaseImpl(t, 50*time.Millisecond, func(t *testing.T, s *Scheduler, _ store.LeaseStore, expire func(time.Duration)) {
			first, err := s.StartTrace("node-a", "recorder-1")
			if err != nil {
				t.Fatalf("StartTrace: %v", err)
			}
			expire(60 * time.Millisecond)
			second, err := s.StartTrace("node-a", "recorder-2")
			if err != nil {
				t.Fatalf("过期后 StartTrace: unexpected err %v", err)
			}
			if second == first || second == "" {
				t.Errorf("过期重租应得新 trace_id，got %q (first=%q)", second, first)
			}
		})
	})
}

// TestNextSeqIncrements：步序号在租约下原子 ++（原 nextSeq 基元已并入 nextTraceStep/NextStep，
// 旧 NextSeq 语义断言——1..3 单调、未知 trace 返回 0——保留于此）。
func TestNextSeqIncrements(t *testing.T) {
	forEachLeaseImpl(t, 30*time.Minute, func(t *testing.T, s *Scheduler, _ store.LeaseStore, _ func(time.Duration)) {
		traceID, _ := s.StartTrace("node-a", "recorder-1")

		for want := int64(1); want <= 3; want++ {
			if got, _ := s.nextTraceStep(traceID); got != want {
				t.Errorf("nextTraceStep seq #%d = %d, want %d", want, got, want)
			}
		}
		if got, _ := s.nextTraceStep("unknown-trace"); got != 0 {
			t.Errorf("nextTraceStep(unknown) seq = %d, want 0", got)
		}
	})
}

// TestDispatchRejectedByLeaseSkipsAudit：非持有者 dispatch 被 checkLease 拒（E_BUSY）。checkLease 置于
// recordTask/enqueue 之前——被拒即短路返回，不进入 recordTask+enqueue 块。以「未建 per-node 队列」
// 为无审计副作用的可观测证据（nil store 下 recordTask 本就 no-op，队列建立是其后唯一可观测的下游动作）。
// dispatch 集成面跑 InMemory 注入即可（ha-contract §3.4）。
func TestDispatchRejectedByLeaseSkipsAudit(t *testing.T) {
	reg := registry.NewRegistry(nil)
	reg.Add(registry.NewSession("node-a", "linux", nil, "", 1)) // 令 reg.Ready(node-a)=true
	s := NewScheduler(reg, nil, 30*time.Minute, nil)

	// node-a 被 recorder-1 租用；另一调用方以空 trace_id dispatch。
	if _, err := s.StartTrace("node-a", "recorder-1"); err != nil {
		t.Fatalf("StartTrace: %v", err)
	}

	resp, code, err := s.Dispatch(context.Background(), "node-a", "click", nil, 0, "attacker", "")
	if code != CodeBusy {
		t.Fatalf("non-holder dispatch: code=%q, want %q (E_BUSY)", code, CodeBusy)
	}
	if resp != nil || err == nil {
		t.Errorf("non-holder dispatch: expected nil resp + non-nil err, got resp=%v err=%v", resp, err)
	}

	// checkLease 先于 recordTask/enqueue：被拒 dispatch 不建 per-node 队列（无下游副作用）。
	s.mu.Lock()
	_, queued := s.queues["node-a"]
	s.mu.Unlock()
	if queued {
		t.Error("被拒 dispatch 不应建立 per-node 队列（证 checkLease 先于 recordTask/enqueue 短路）")
	}
}

// TestNextTraceStepAtomic 覆盖 (seq, who) 原子捕获（GAP-2，单临界区/单 Lua）：逐步 seq ++ 且 who
// 恒为登记者；未知/已释放 trace 一律返 (0, "")——结构性断言保证绝不出现「seq>0 却 who 空列」的
// TOCTOU 中间态。
func TestNextTraceStepAtomic(t *testing.T) {
	forEachLeaseImpl(t, 30*time.Minute, func(t *testing.T, s *Scheduler, _ store.LeaseStore, _ func(time.Duration)) {
		traceID, _ := s.StartTrace("node-a", "recorder-1")

		// 逐步 ++：seq 单调，who 与之同出一份存活租约快照。
		for want := int64(1); want <= 3; want++ {
			seq, who := s.nextTraceStep(traceID)
			if seq != want || who != "recorder-1" {
				t.Errorf("nextTraceStep #%d = (%d, %q), want (%d, recorder-1)", want, seq, who, want)
			}
		}

		// 未知 trace → (0, "")。
		if seq, who := s.nextTraceStep("unknown-trace"); seq != 0 || who != "" {
			t.Errorf("nextTraceStep(unknown) = (%d, %q), want (0, \"\")", seq, who)
		}

		// 已释放 trace → (0, "")：释放后绝无 seq>0∧who=="" 中间态（旁路 seq>0 守卫据此跳过不录空步）。
		s.StopTrace(traceID)
		if seq, who := s.nextTraceStep(traceID); seq != 0 || who != "" {
			t.Errorf("nextTraceStep(released) = (%d, %q), want (0, \"\") (释放后不录空步)", seq, who)
		}
	})
}

// TestStartTraceConcurrentExclusive：两 goroutine 争 StartTrace 同 node 恰一胜——InMemory 靠锁、
// Redis 靠 Lua 原子（双实现同表，T4 新增并发用例）。
func TestStartTraceConcurrentExclusive(t *testing.T) {
	forEachLeaseImpl(t, 30*time.Minute, func(t *testing.T, s *Scheduler, _ store.LeaseStore, _ func(time.Duration)) {
		var wg sync.WaitGroup
		results := make([]error, 2)
		for i := 0; i < 2; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				_, results[i] = s.StartTrace("node-a", fmt.Sprintf("racer-%d", i))
			}(i)
		}
		wg.Wait()

		okCount, busyCount := 0, 0
		for _, e := range results {
			switch {
			case e == nil:
				okCount++
			case errors.Is(e, errTraceBusy):
				busyCount++
			default:
				t.Fatalf("concurrent StartTrace: unexpected err %v", e)
			}
		}
		if okCount != 1 || busyCount != 1 {
			t.Errorf("concurrent StartTrace: ok=%d busy=%d, want 恰一胜 (1,1)", okCount, busyCount)
		}
	})
}

// TestCheckLeaseRedisFailOpen：Redis 读故障时 checkLease 放行 + 无 panic（读路径 fail-open，
// ha-contract §7#5——录制独占非安全性质，fail-closed 会把 Redis 抖动放大为 dispatch 停摆）。
func TestCheckLeaseRedisFailOpen(t *testing.T) {
	mr := miniredis.RunT(t)
	rs, err := store.NewRedisStore(context.Background(), mr.Addr())
	if err != nil {
		t.Fatalf("NewRedisStore(miniredis): %v", err)
	}
	t.Cleanup(func() { _ = rs.Close() })
	s := newLeaseScheduler(time.Minute, store.NewRedisLeaseStore(rs, time.Minute))

	mr.Close() // 模拟 Redis 故障：后续读一律连接错

	if err := s.checkLease(context.Background(), "node-a", ""); err != nil {
		t.Errorf("fail-open checkLease under redis outage: got %v, want nil (放行)", err)
	}
}
