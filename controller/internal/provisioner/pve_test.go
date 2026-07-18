package provisioner

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aura/controller/internal/store"
)

// fakePVE 是 pveAPI 的可控 mock，记录调用并可配置特定操作失败。
type fakePVE struct {
	mu        sync.Mutex
	clones    []cloneCall
	started   []int
	waited    []int
	snapshots []snapCall
	rollbacks []snapCall
	destroyed []int

	failClone    bool
	failStart    bool
	failAgent    bool
	failSnapshot bool
	failRollback bool

	status string
	nextID int // ClusterNextID 返回值；0 表示 PVE 源不参与 nextVMID 播种
}

type cloneCall struct {
	tmpl, newID int
	name        string
}

type snapCall struct {
	vmid int
	name string
}

func (f *fakePVE) Clone(_ context.Context, tmpl, newID int, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failClone {
		return errors.New("clone failed")
	}
	f.clones = append(f.clones, cloneCall{tmpl, newID, name})
	return nil
}

func (f *fakePVE) Start(_ context.Context, vmid int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failStart {
		return errors.New("start failed")
	}
	f.started = append(f.started, vmid)
	return nil
}

func (f *fakePVE) WaitAgent(_ context.Context, vmid int, _ time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failAgent {
		return errors.New("agent timeout")
	}
	f.waited = append(f.waited, vmid)
	return nil
}

func (f *fakePVE) Snapshot(_ context.Context, vmid int, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failSnapshot {
		return errors.New("snapshot failed")
	}
	f.snapshots = append(f.snapshots, snapCall{vmid, name})
	return nil
}

func (f *fakePVE) Rollback(_ context.Context, vmid int, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failRollback {
		return errors.New("rollback failed")
	}
	f.rollbacks = append(f.rollbacks, snapCall{vmid, name})
	return nil
}

func (f *fakePVE) Destroy(_ context.Context, vmid int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.destroyed = append(f.destroyed, vmid)
	return nil
}

func (f *fakePVE) Status(_ context.Context, _ int) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.status == "" {
		return "running", nil
	}
	return f.status, nil
}

func (f *fakePVE) ClusterNextID(_ context.Context) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.nextID, nil // 0 → 不影响 seed（回落 PG / VMIDBase）
}

func newTestProvisioner(api pveAPI) *Provisioner {
	// store 为 nil：纯内存路径。
	return NewProvisioner(api, nil, DefaultConfig())
}

// fakeVMIDStore 是 vmidStore 的 mock，模拟 PG 单行游标语义（不依赖真 PG）：AllocVMID 自增共享
// 游标、SeedVMID GREATEST 只升不降——两 Provisioner 共享同一实例即模拟双副本共享 PG。allocErr
// 注入取号失败（测进程内游标回退路径）。
type fakeVMIDStore struct {
	mu       sync.Mutex
	maxVMID  int
	next     int
	allocErr error
}

func (f *fakeVMIDStore) MaxVMID(_ context.Context) (int, error) { return f.maxVMID, nil }

func (f *fakeVMIDStore) AllocVMID(_ context.Context) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.allocErr != nil {
		return 0, f.allocErr
	}
	id := f.next
	f.next++
	return id, nil
}

func (f *fakeVMIDStore) SeedVMID(_ context.Context, floor int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if floor > f.next {
		f.next = floor
	}
	return nil
}

// 播种（自愈三件套之三）：预置存量 env(vmid=9205)，新建 Provisioner 首分配应从 PG MaxVMID+1
// 播种（>9205），而非从 VMIDBase(9200) 裸自增起算——跨控制面重启不与存量 VM 撞号。
func TestNewProvisionerSeedsVMIDFromStoreMax(t *testing.T) {
	fake := &fakePVE{} // ClusterNextID 返回 0 → PVE 源不参与
	p := newProvisioner(fake, nil, &fakeVMIDStore{maxVMID: 9205}, DefaultConfig())

	env, err := p.CreateEnvironment(context.Background(), KindPersistent, "")
	if err != nil {
		t.Fatalf("CreateEnvironment: %v", err)
	}
	if env.VMID <= 9205 {
		t.Errorf("首分配 vmid=%d，应 >9205（从 PG MaxVMID+1 播种），而非 VMIDBase=%d",
			env.VMID, DefaultConfig().VMIDBase)
	}
	if env.VMID != 9206 {
		t.Errorf("首分配 vmid=%d，期望正好 9206 (=MaxVMID 9205 + 1)", env.VMID)
	}
}

// 播种取三源最大：PVE cluster nextid 高于 PG MaxVMID+1 与 VMIDBase 时以 PVE nextid 为准。
func TestNewProvisionerSeedsVMIDFromPVENextID(t *testing.T) {
	fake := &fakePVE{nextID: 9310} // PVE 建议 9310（高于 store max+1=9206）
	p := newProvisioner(fake, nil, &fakeVMIDStore{maxVMID: 9205}, DefaultConfig())

	env, err := p.CreateEnvironment(context.Background(), KindPersistent, "")
	if err != nil {
		t.Fatalf("CreateEnvironment: %v", err)
	}
	if env.VMID != 9310 {
		t.Errorf("首分配 vmid=%d，期望 9310（PVE nextid 为三源最大）", env.VMID)
	}
}

// M10 撞号根除：vmidSrc 在时 allocVMID 主路径经 PG 游标原子取号——两 Provisioner 共享同一
// vmidStore（模拟双副本共享 PG）交替 create，分配号全局唯一；若仍各走进程内游标则必撞
// （双方都从 VMIDBase 起算）。
func TestAllocVMIDViaSharedPGCursor(t *testing.T) {
	shared := &fakeVMIDStore{}
	pA := newProvisioner(&fakePVE{}, nil, shared, DefaultConfig())
	pB := newProvisioner(&fakePVE{}, nil, shared, DefaultConfig())

	seen := map[int]bool{}
	for i := 0; i < 3; i++ {
		envA, err := pA.CreateEnvironment(context.Background(), KindPersistent, "")
		if err != nil {
			t.Fatalf("A create %d: %v", i, err)
		}
		envB, err := pB.CreateEnvironment(context.Background(), KindPersistent, "")
		if err != nil {
			t.Fatalf("B create %d: %v", i, err)
		}
		for _, v := range []int{envA.VMID, envB.VMID} {
			if seen[v] {
				t.Errorf("vmid %d 重复分配（双副本撞号未根除）", v)
			}
			seen[v] = true
		}
	}
}

// M10 回退契约：PG 取号失败（allocErr 注入）回退进程内游标并继续 create——未配 PG 的单副本
// 纯内存形态与 PG 瞬断场景行为不破坏。
func TestAllocVMIDFallsBackOnPGError(t *testing.T) {
	src := &fakeVMIDStore{allocErr: errors.New("pg down")}
	p := newProvisioner(&fakePVE{}, nil, src, DefaultConfig())

	env, err := p.CreateEnvironment(context.Background(), KindPersistent, "")
	if err != nil {
		t.Fatalf("CreateEnvironment: %v", err)
	}
	if env.VMID != DefaultConfig().VMIDBase {
		t.Errorf("回退分配 vmid=%d, want VMIDBase=%d（进程内游标）", env.VMID, DefaultConfig().VMIDBase)
	}
}

// ephemeral 置备：clone -> start -> waitagent -> 基线快照，vmid 从 VMIDBase 起。
func TestCreateEphemeralSnapshotsBaseline(t *testing.T) {
	fake := &fakePVE{}
	p := newTestProvisioner(fake)

	env, err := p.CreateEnvironment(context.Background(), KindEphemeral, "")
	if err != nil {
		t.Fatalf("CreateEnvironment: %v", err)
	}
	if env.Kind != KindEphemeral {
		t.Errorf("kind = %q, want ephemeral", env.Kind)
	}
	if env.VMID != DefaultConfig().VMIDBase {
		t.Errorf("vmid = %d, want %d", env.VMID, DefaultConfig().VMIDBase)
	}
	if env.NodeID != "" {
		t.Errorf("node_id = %q, want empty at create", env.NodeID)
	}
	if len(fake.clones) != 1 || fake.clones[0].tmpl != DefaultConfig().TemplateVMID {
		t.Errorf("clone calls = %+v, want 1 from template %d", fake.clones, DefaultConfig().TemplateVMID)
	}
	if len(fake.started) != 1 || len(fake.waited) != 1 {
		t.Errorf("started=%v waited=%v, want one each", fake.started, fake.waited)
	}
	if len(fake.snapshots) != 1 || fake.snapshots[0].name != DefaultConfig().BaseSnapshot {
		t.Errorf("snapshots = %+v, want baseline %q", fake.snapshots, DefaultConfig().BaseSnapshot)
	}
}

// persistent 置备：不建基线快照。
func TestCreatePersistentNoSnapshot(t *testing.T) {
	fake := &fakePVE{}
	p := newTestProvisioner(fake)

	env, err := p.CreateEnvironment(context.Background(), KindPersistent, "")
	if err != nil {
		t.Fatalf("CreateEnvironment: %v", err)
	}
	if env.Kind != KindPersistent {
		t.Errorf("kind = %q, want persistent", env.Kind)
	}
	if len(fake.snapshots) != 0 {
		t.Errorf("snapshots = %+v, want none for persistent", fake.snapshots)
	}
}

// 空 kind 默认 ephemeral。
func TestCreateDefaultsToEphemeral(t *testing.T) {
	fake := &fakePVE{}
	p := newTestProvisioner(fake)

	env, err := p.CreateEnvironment(context.Background(), "", "")
	if err != nil {
		t.Fatalf("CreateEnvironment: %v", err)
	}
	if env.Kind != KindEphemeral {
		t.Errorf("empty kind -> %q, want ephemeral", env.Kind)
	}
}

// vmid 跨多次创建自增。
func TestVMIDAutoIncrement(t *testing.T) {
	fake := &fakePVE{}
	p := newTestProvisioner(fake)
	base := DefaultConfig().VMIDBase

	for i := 0; i < 3; i++ {
		env, err := p.CreateEnvironment(context.Background(), KindPersistent, "")
		if err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
		if env.VMID != base+i {
			t.Errorf("create %d vmid = %d, want %d", i, env.VMID, base+i)
		}
	}
}

// template 入参：数字 VMID 覆盖默认。
func TestResolveTemplateNumericOverride(t *testing.T) {
	fake := &fakePVE{}
	p := newTestProvisioner(fake)

	if _, err := p.CreateEnvironment(context.Background(), KindPersistent, "9105"); err != nil {
		t.Fatalf("CreateEnvironment: %v", err)
	}
	if len(fake.clones) != 1 || fake.clones[0].tmpl != 9105 {
		t.Errorf("clone template = %+v, want 9105", fake.clones)
	}
}

// template 非数字 → 报错，不发起 clone。
func TestResolveTemplateInvalid(t *testing.T) {
	fake := &fakePVE{}
	p := newTestProvisioner(fake)

	if _, err := p.CreateEnvironment(context.Background(), KindPersistent, "ubuntu-tmpl"); err == nil {
		t.Fatal("expected error for non-numeric template")
	}
	if len(fake.clones) != 0 {
		t.Errorf("clone should not run on invalid template, got %+v", fake.clones)
	}
}

// clone 失败 → 报错且环境未登记（destroy 亦不触发，因克隆未成）。
func TestCreateCloneFailureNotTracked(t *testing.T) {
	fake := &fakePVE{failClone: true}
	p := newTestProvisioner(fake)

	env, err := p.CreateEnvironment(context.Background(), KindEphemeral, "")
	if err == nil {
		t.Fatal("expected clone failure error")
	}
	if env != nil {
		t.Errorf("env = %+v, want nil on failure", env)
	}
	p.mu.Lock()
	n := len(p.envs)
	p.mu.Unlock()
	if n != 0 {
		t.Errorf("tracked envs = %d, want 0 after clone failure", n)
	}
}

// start 失败 → 尽力回收（destroy 被调用）。
func TestCreateStartFailureCleansUp(t *testing.T) {
	fake := &fakePVE{failStart: true}
	p := newTestProvisioner(fake)

	if _, err := p.CreateEnvironment(context.Background(), KindEphemeral, ""); err == nil {
		t.Fatal("expected start failure error")
	}
	if len(fake.destroyed) != 1 {
		t.Errorf("destroyed = %v, want cleanup destroy after start failure", fake.destroyed)
	}
}

// 销毁：删除对应 vmid 并出内存表。
func TestDestroyEnvironment(t *testing.T) {
	fake := &fakePVE{}
	p := newTestProvisioner(fake)

	env, err := p.CreateEnvironment(context.Background(), KindPersistent, "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := p.DestroyEnvironment(context.Background(), env.ID); err != nil {
		t.Fatalf("destroy: %v", err)
	}
	if len(fake.destroyed) != 1 || fake.destroyed[0] != env.VMID {
		t.Errorf("destroyed = %v, want [%d]", fake.destroyed, env.VMID)
	}
	if _, err := p.lookupEnv(context.Background(), env.ID); err == nil {
		t.Error("env should be gone after destroy")
	}
}

// 销毁不存在的环境 → 报错。
func TestDestroyUnknownEnvironment(t *testing.T) {
	fake := &fakePVE{}
	p := newTestProvisioner(fake)

	if err := p.DestroyEnvironment(context.Background(), "does-not-exist"); err == nil {
		t.Fatal("expected error destroying unknown environment")
	}
}

// ephemeral 复位：rollback 到基线快照。
func TestResetEphemeralRollsBack(t *testing.T) {
	fake := &fakePVE{}
	p := newTestProvisioner(fake)

	env, err := p.CreateEnvironment(context.Background(), KindEphemeral, "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := p.ResetEphemeral(context.Background(), env.ID); err != nil {
		t.Fatalf("reset: %v", err)
	}
	if len(fake.rollbacks) != 1 || fake.rollbacks[0].name != DefaultConfig().BaseSnapshot {
		t.Errorf("rollbacks = %+v, want baseline %q", fake.rollbacks, DefaultConfig().BaseSnapshot)
	}
}

// persistent 环境不可复位。
func TestResetPersistentRejected(t *testing.T) {
	fake := &fakePVE{}
	p := newTestProvisioner(fake)

	env, err := p.CreateEnvironment(context.Background(), KindPersistent, "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := p.ResetEphemeral(context.Background(), env.ID); err == nil {
		t.Fatal("expected error resetting persistent environment")
	}
	if len(fake.rollbacks) != 0 {
		t.Errorf("rollback should not run for persistent, got %+v", fake.rollbacks)
	}
}

// rollback 失败 → ErrResetFailed（降级信号）。
func TestResetRollbackFailureDegrades(t *testing.T) {
	fake := &fakePVE{failRollback: true}
	p := newTestProvisioner(fake)

	env, err := p.CreateEnvironment(context.Background(), KindEphemeral, "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	err = p.ResetEphemeral(context.Background(), env.ID)
	if !errors.Is(err, ErrResetFailed) {
		t.Errorf("err = %v, want ErrResetFailed", err)
	}
}

// —— M10 双副本 PG 集成（TASK-007）：需 AURA_TEST_PG_DSN，缺省 skip（沿 store 集成测试惯例）。——
// 两个独立 PGStore 连接池 = 双副本视角；测试库共享不清场，游标断言全用相对基准值。

// TestDualReplicaPGVMIDAndStaleDestroy 对真 PG 演练双副本安全三组语义：
//  1) SeedVMID 幂等只升（GREATEST：高 floor 抬升跨副本可见、低 floor 不拉回已分配区间）；
//  2) AllocVMID 并发唯一（两 store × 多 goroutine 取号全局无重号，PG 行锁串行化）；
//  3) envs stale destroy 防护（M-9 根除）：副本 A create → 副本 B destroy（PG 行删除）→ A 以
//     stale 内存条目再 destroy → not found 拒绝且 A 的 pveAPI.Destroy 零调用——PVE nextid 会
//     即刻复用已毁 vmid，stale destroy 一旦动手即误毁复用者 VM。
func TestDualReplicaPGVMIDAndStaleDestroy(t *testing.T) {
	dsn := os.Getenv("AURA_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("set AURA_TEST_PG_DSN to run dual-replica provisioner integration")
	}
	ctx := context.Background()
	stA, err := store.NewPGStore(ctx, dsn)
	if err != nil {
		t.Fatalf("NewPGStore A: %v", err)
	}
	defer stA.Close()
	stB, err := store.NewPGStore(ctx, dsn)
	if err != nil {
		t.Fatalf("NewPGStore B: %v", err)
	}
	defer stB.Close()

	// —— 1) SeedVMID 幂等只升 ——
	base, err := stA.AllocVMID(ctx)
	if err != nil {
		t.Fatalf("AllocVMID base: %v", err)
	}
	if err := stA.SeedVMID(ctx, base+100); err != nil {
		t.Fatalf("SeedVMID up: %v", err)
	}
	got, err := stB.AllocVMID(ctx) // 抬升对另一副本立即可见
	if err != nil {
		t.Fatalf("AllocVMID after seed: %v", err)
	}
	if got != base+100 {
		t.Errorf("seed 抬升后取号=%d, want %d", got, base+100)
	}
	if err := stB.SeedVMID(ctx, base); err != nil { // 后启动副本的低 floor
		t.Fatalf("SeedVMID down: %v", err)
	}
	got2, err := stA.AllocVMID(ctx)
	if err != nil {
		t.Fatalf("AllocVMID after low seed: %v", err)
	}
	if got2 != base+101 {
		t.Errorf("低 floor 播种后取号=%d, want %d（GREATEST 不得拉回游标）", got2, base+101)
	}

	// —— 2) AllocVMID 并发唯一 ——
	const perG, nG = 25, 4
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		ids  = map[int]int{} // vmid -> 分配次数
		errs []error
	)
	stores := []*store.PGStore{stA, stB}
	for g := 0; g < nG; g++ {
		wg.Add(1)
		go func(st *store.PGStore) {
			defer wg.Done()
			for i := 0; i < perG; i++ {
				v, err := st.AllocVMID(ctx)
				mu.Lock()
				if err != nil {
					errs = append(errs, err)
				} else {
					ids[v]++
				}
				mu.Unlock()
			}
		}(stores[g%2])
	}
	wg.Wait()
	if len(errs) > 0 {
		t.Fatalf("并发 AllocVMID 出错 %d 个，首个: %v", len(errs), errs[0])
	}
	if len(ids) != perG*nG {
		t.Errorf("唯一号数=%d, want %d（存在重号）", len(ids), perG*nG)
	}
	for v, n := range ids {
		if n > 1 {
			t.Errorf("vmid %d 被分配 %d 次（撞号）", v, n)
		}
	}

	// —— 3) stale destroy 防护（两 Provisioner 各挂独立 fakePVE + 各自 store 池 = 双副本）——
	fakeA, fakeB := &fakePVE{}, &fakePVE{}
	pA := newProvisioner(fakeA, stA, stA, DefaultConfig())
	pB := newProvisioner(fakeB, stB, stB, DefaultConfig())

	env, err := pA.CreateEnvironment(ctx, KindPersistent, "")
	if err != nil {
		t.Fatalf("A create: %v", err)
	}
	// B 副本销毁：B 内存无该 env → PG 权威读命中 → 对 PG 行的 vmid 动手 + 删行。
	if err := pB.DestroyEnvironment(ctx, env.ID); err != nil {
		t.Fatalf("B destroy: %v", err)
	}
	if len(fakeB.destroyed) != 1 || fakeB.destroyed[0] != env.VMID {
		t.Errorf("B destroyed=%v, want [%d]（PG 权威读跨副本命中）", fakeB.destroyed, env.VMID)
	}
	// A 以 stale 内存条目重试 destroy → PG 行已无 → 拒绝，且 A 的 api.Destroy 绝不触发。
	err = pA.DestroyEnvironment(ctx, env.ID)
	if err == nil {
		t.Fatal("A stale destroy 应被拒绝（PG 行已删），实际成功")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("stale destroy 错误=%q, want 含 not found", err)
	}
	if len(fakeA.destroyed) != 0 {
		t.Errorf("A destroyed=%v, want 空——stale 条目不得触发 api.Destroy（误毁 vmid 复用者）", fakeA.destroyed)
	}
}
