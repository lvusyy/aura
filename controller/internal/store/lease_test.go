package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
)

// leaseImpl 是双实现测试表的一项：build 构造 LeaseStore，expire 把活跃租约推进到过期
//（进程内=真实等待毫秒级 TTL；miniredis=FastForward 假时钟——两实现过期机制不同，对外语义
// 一致「过期即视为不存在」，ha-contract §3.2 行为差异表）。
type leaseImpl struct {
	name  string
	build func(t *testing.T) (LeaseStore, func(d time.Duration))
}

func leaseImpls() []leaseImpl {
	return []leaseImpl{
		{
			name: "inmemory",
			build: func(t *testing.T) (LeaseStore, func(d time.Duration)) {
				return NewInMemoryLeaseStore(), func(d time.Duration) { time.Sleep(d) }
			},
		},
		{
			name: "redis",
			build: func(t *testing.T) (LeaseStore, func(d time.Duration)) {
				mr := miniredis.RunT(t)
				rs, err := NewRedisStore(context.Background(), mr.Addr())
				if err != nil {
					t.Fatalf("NewRedisStore(miniredis): %v", err)
				}
				t.Cleanup(func() { _ = rs.Close() })
				return NewRedisLeaseStore(rs, 30*time.Minute), mr.FastForward
			},
		},
	}
}

// TestLeaseStoreBasicFlow：Acquire 独占 → Get 载荷 → NextStep 单调+who → Release 真值 → 释放后
// Get/NextStep 归零——四方法基本流转双实现同表（ha-contract §3.4）。
func TestLeaseStoreBasicFlow(t *testing.T) {
	for _, impl := range leaseImpls() {
		t.Run(impl.name, func(t *testing.T) {
			ls, _ := impl.build(t)
			ctx := context.Background()

			if err := ls.Acquire(ctx, "node-a", "trace-1", "recorder-1", 30*time.Minute); err != nil {
				t.Fatalf("Acquire: %v", err)
			}
			// 独占：他 trace 再租同 node → ErrLeaseHeld。
			if err := ls.Acquire(ctx, "node-a", "trace-2", "recorder-2", 30*time.Minute); !errors.Is(err, ErrLeaseHeld) {
				t.Errorf("second Acquire: got %v, want ErrLeaseHeld", err)
			}

			lease, ok, err := ls.Get(ctx, "node-a")
			if err != nil || !ok {
				t.Fatalf("Get: ok=%v err=%v, want active lease", ok, err)
			}
			if lease.TraceID != "trace-1" || lease.Who != "recorder-1" || lease.NodeID != "node-a" {
				t.Errorf("Get lease = %+v, want {node-a trace-1 recorder-1}", lease)
			}
			// 无租约 node → 不存在。
			if _, ok, err := ls.Get(ctx, "node-b"); ok || err != nil {
				t.Errorf("Get(unleased) = ok=%v err=%v, want (false, nil)", ok, err)
			}

			// NextStep：首步=1、单调 ++、who 恒为登记者（GAP-2 同出一份快照）。
			for want := int64(1); want <= 3; want++ {
				seq, who, nerr := ls.NextStep(ctx, "trace-1")
				if nerr != nil || seq != want || who != "recorder-1" {
					t.Errorf("NextStep #%d = (%d, %q, %v), want (%d, recorder-1, nil)", want, seq, who, nerr, want)
				}
			}
			// 未知 trace → (0, "")。
			if seq, who, nerr := ls.NextStep(ctx, "unknown"); seq != 0 || who != "" || nerr != nil {
				t.Errorf("NextStep(unknown) = (%d, %q, %v), want (0, \"\", nil)", seq, who, nerr)
			}

			// Release：首次 true，幂等再调 false；释放后 Get 不存在、NextStep 归零。
			if released, rerr := ls.Release(ctx, "trace-1"); rerr != nil || !released {
				t.Errorf("Release = (%v, %v), want (true, nil)", released, rerr)
			}
			if released, rerr := ls.Release(ctx, "trace-1"); rerr != nil || released {
				t.Errorf("Release(again) = (%v, %v), want (false, nil)", released, rerr)
			}
			if _, ok, _ := ls.Get(ctx, "node-a"); ok {
				t.Error("Get after Release: lease still present")
			}
			if seq, who, _ := ls.NextStep(ctx, "trace-1"); seq != 0 || who != "" {
				t.Errorf("NextStep(released) = (%d, %q), want (0, \"\")", seq, who)
			}
		})
	}
}

// TestLeaseStoreTTLExpiry：TTL 过期后租约视为不存在且可重租（进程内惰性判定 / Redis 原生 PEXPIRE）。
func TestLeaseStoreTTLExpiry(t *testing.T) {
	for _, impl := range leaseImpls() {
		t.Run(impl.name, func(t *testing.T) {
			ls, expire := impl.build(t)
			ctx := context.Background()

			if err := ls.Acquire(ctx, "node-a", "trace-1", "recorder-1", 50*time.Millisecond); err != nil {
				t.Fatalf("Acquire: %v", err)
			}
			expire(60 * time.Millisecond)

			if _, ok, err := ls.Get(ctx, "node-a"); ok || err != nil {
				t.Errorf("Get(expired) = ok=%v err=%v, want (false, nil)", ok, err)
			}
			// 过期后重租放行（新 trace）。
			if err := ls.Acquire(ctx, "node-a", "trace-2", "recorder-2", 30*time.Minute); err != nil {
				t.Errorf("re-Acquire after expiry: %v", err)
			}
		})
	}
}

// TestLeaseStoreReleaseDoesNotClobberNewLease：旧 trace 的迟到 Release 不误删同 node 新租约
//（进程内 releaseLocked 反查保护 / Redis 正查 trace_id 比对，镜像语义）。
func TestLeaseStoreReleaseDoesNotClobberNewLease(t *testing.T) {
	for _, impl := range leaseImpls() {
		t.Run(impl.name, func(t *testing.T) {
			ls, _ := impl.build(t)
			ctx := context.Background()

			if err := ls.Acquire(ctx, "node-a", "trace-1", "recorder-1", 30*time.Minute); err != nil {
				t.Fatalf("Acquire trace-1: %v", err)
			}
			if released, _ := ls.Release(ctx, "trace-1"); !released {
				t.Fatal("Release trace-1: want true")
			}
			if err := ls.Acquire(ctx, "node-a", "trace-2", "recorder-2", 30*time.Minute); err != nil {
				t.Fatalf("Acquire trace-2: %v", err)
			}

			// 迟到的旧 trace 再释放：不得伤及新租约。
			if released, _ := ls.Release(ctx, "trace-1"); released {
				t.Error("stale Release(trace-1) = true, want false (已释放)")
			}
			lease, ok, err := ls.Get(ctx, "node-a")
			if err != nil || !ok || lease.TraceID != "trace-2" {
				t.Errorf("Get after stale release = (%+v, %v, %v), want trace-2 intact", lease, ok, err)
			}
		})
	}
}

// TestLeaseStoreList（T13 租约期 UX 读面）：List 返回全部活跃租约快照——空表起步、多节点齐活、
// 过期项不入表（进程内跳过 / Redis 键亡）、Release 后出表。双实现同表。
func TestLeaseStoreList(t *testing.T) {
	for _, impl := range leaseImpls() {
		t.Run(impl.name, func(t *testing.T) {
			ls, expire := impl.build(t)
			ctx := context.Background()

			if leases, err := ls.List(ctx); err != nil || len(leases) != 0 {
				t.Fatalf("List(empty) = (%v, %v), want ([], nil)", leases, err)
			}

			// 两长约 + 一短 TTL 约；推进 60ms 后短约过期不入表。
			for _, a := range []struct {
				node, trace, who string
				ttl              time.Duration
			}{
				{"node-a", "trace-1", "alice", 30 * time.Minute},
				{"node-b", "trace-2", "bob", 30 * time.Minute},
				{"node-c", "trace-3", "carol", 50 * time.Millisecond},
			} {
				if err := ls.Acquire(ctx, a.node, a.trace, a.who, a.ttl); err != nil {
					t.Fatalf("Acquire(%s): %v", a.node, err)
				}
			}
			expire(60 * time.Millisecond)

			leases, err := ls.List(ctx)
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			byNode := make(map[string]Lease, len(leases))
			for _, l := range leases {
				byNode[l.NodeID] = l
			}
			if len(byNode) != 2 {
				t.Fatalf("List after expiry = %v, want exactly {node-a, node-b}（过期项不入表）", byNode)
			}
			if l := byNode["node-a"]; l.TraceID != "trace-1" || l.Who != "alice" {
				t.Errorf("List[node-a] = %+v, want {trace-1 alice}", l)
			}
			if l := byNode["node-b"]; l.TraceID != "trace-2" || l.Who != "bob" {
				t.Errorf("List[node-b] = %+v, want {trace-2 bob}", l)
			}

			// Release 后出表。
			if released, rerr := ls.Release(ctx, "trace-1"); rerr != nil || !released {
				t.Fatalf("Release = (%v, %v), want (true, nil)", released, rerr)
			}
			leases, err = ls.List(ctx)
			if err != nil {
				t.Fatalf("List after release: %v", err)
			}
			if len(leases) != 1 || leases[0].NodeID != "node-b" {
				t.Errorf("List after release = %v, want [node-b only]", leases)
			}
		})
	}
}

// TestRedisLeaseSeqKeyTTL：Redis 专项——NextStep 首步后 trace:seq 键必须带 TTL（与租约同长，
// 活跃期内不先死；Release 后由 TTL 自然回收，ha-contract §3.2 seq 键论证）。
func TestRedisLeaseSeqKeyTTL(t *testing.T) {
	mr := miniredis.RunT(t)
	rs, err := NewRedisStore(context.Background(), mr.Addr())
	if err != nil {
		t.Fatalf("NewRedisStore(miniredis): %v", err)
	}
	t.Cleanup(func() { _ = rs.Close() })
	ls := NewRedisLeaseStore(rs, time.Minute)
	ctx := context.Background()

	if err := ls.Acquire(ctx, "node-a", "trace-1", "recorder-1", time.Minute); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if seq, _, err := ls.NextStep(ctx, "trace-1"); err != nil || seq != 1 {
		t.Fatalf("NextStep = (%d, %v), want (1, nil)", seq, err)
	}
	if ttl := mr.TTL(traceSeqKey("trace-1")); ttl <= 0 {
		t.Errorf("trace:seq TTL = %v, want > 0 (首步补 TTL)", ttl)
	}
	// seq 键 TTL 到期后残键消失（Release 不删 seq 键，由 TTL 回收）。
	if _, rerr := ls.Release(ctx, "trace-1"); rerr != nil {
		t.Fatalf("Release: %v", rerr)
	}
	mr.FastForward(2 * time.Minute)
	if mr.Exists(traceSeqKey("trace-1")) {
		t.Error("trace:seq key survived past TTL, want expired")
	}
}
