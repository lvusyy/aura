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

// fakeK8s 是 k8sAPI 的可控 mock，记录调用并可配置特定操作失败（复刻 fakePVE 模板）。
type fakeK8s struct {
	mu      sync.Mutex
	created []vmiSpec
	deleted []string

	statuses map[string]string // name -> phase（缺省 "Running"）

	failCreate      bool
	failWaitRunning bool
	failDelete      bool
}

func (f *fakeK8s) CreateVMI(_ context.Context, spec vmiSpec) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failCreate {
		return errors.New("create vmi failed")
	}
	f.created = append(f.created, spec)
	return nil
}

func (f *fakeK8s) DeleteVMI(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failDelete {
		return errors.New("delete vmi failed")
	}
	f.deleted = append(f.deleted, name)
	return nil
}

func (f *fakeK8s) GetVMIStatus(_ context.Context, name string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.statuses != nil {
		if s, ok := f.statuses[name]; ok {
			return s, nil
		}
	}
	return "Running", nil
}

func (f *fakeK8s) WaitVMIRunning(_ context.Context, _ string, _ time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failWaitRunning {
		return errors.New("vmi not running")
	}
	return nil
}

func (f *fakeK8s) WaitVMIGone(_ context.Context, _ string, _ time.Duration) error {
	return nil
}

func newTestK8sProvisioner(api k8sAPI) *K8sProvisioner {
	// store 为 nil：纯内存路径。
	return NewK8sProvisioner(api, nil, DefaultK8sConfig())
}

// 默认落 "aura" namespace（SAFETY：AURA 资源与集群既有 VM 隔离）。
func TestK8sDefaultNamespaceIsolation(t *testing.T) {
	if ns := DefaultK8sConfig().Namespace; ns != "aura" {
		t.Errorf("default namespace = %q, want aura (SAFETY isolation)", ns)
	}
}

// ephemeral 置备：建 1 个 aura-ephem- 前缀 VMI、等 Running，env 记 provider=k8s + ProviderRef=VMI 名。
func TestK8sCreateEphemeralRunsVMI(t *testing.T) {
	fake := &fakeK8s{}
	k := newTestK8sProvisioner(fake)

	env, err := k.CreateEnvironment(context.Background(), KindEphemeral, "")
	if err != nil {
		t.Fatalf("CreateEnvironment: %v", err)
	}
	if env.Kind != KindEphemeral {
		t.Errorf("kind = %q, want ephemeral", env.Kind)
	}
	if env.Provider != providerK8s {
		t.Errorf("provider = %q, want k8s", env.Provider)
	}
	if env.VMID != 0 {
		t.Errorf("vmid = %d, want 0 (vmid is PVE-only)", env.VMID)
	}
	if env.NodeID != "" {
		t.Errorf("node_id = %q, want empty at create", env.NodeID)
	}
	if len(fake.created) != 1 {
		t.Fatalf("created VMIs = %d, want 1", len(fake.created))
	}
	got := fake.created[0]
	if !strings.HasPrefix(got.Name, "aura-ephem-") {
		t.Errorf("vmi name = %q, want aura-ephem- prefix", got.Name)
	}
	if env.ProviderRef != got.Name {
		t.Errorf("provider_ref = %q, want %q (VMI name)", env.ProviderRef, got.Name)
	}
	if got.Labels["app.kubernetes.io/managed-by"] != "aura" {
		t.Errorf("managed-by label = %q, want aura (SAFETY audit)", got.Labels["app.kubernetes.io/managed-by"])
	}
	if got.Labels["aura.io/kind"] != KindEphemeral {
		t.Errorf("kind label = %q, want ephemeral", got.Labels["aura.io/kind"])
	}
}

// persistent 置备：前缀 aura-persist-。
func TestK8sCreatePersistentPrefix(t *testing.T) {
	fake := &fakeK8s{}
	k := newTestK8sProvisioner(fake)

	env, err := k.CreateEnvironment(context.Background(), KindPersistent, "")
	if err != nil {
		t.Fatalf("CreateEnvironment: %v", err)
	}
	if env.Kind != KindPersistent {
		t.Errorf("kind = %q, want persistent", env.Kind)
	}
	if len(fake.created) != 1 || !strings.HasPrefix(fake.created[0].Name, "aura-persist-") {
		t.Errorf("vmi = %+v, want aura-persist- prefix", fake.created)
	}
}

// 空 kind 默认 ephemeral。
func TestK8sCreateDefaultsToEphemeral(t *testing.T) {
	fake := &fakeK8s{}
	k := newTestK8sProvisioner(fake)

	env, err := k.CreateEnvironment(context.Background(), "", "")
	if err != nil {
		t.Fatalf("CreateEnvironment: %v", err)
	}
	if env.Kind != KindEphemeral {
		t.Errorf("empty kind -> %q, want ephemeral", env.Kind)
	}
}

// template 非空覆盖 containerDisk 镜像。
func TestK8sCreateTemplateOverridesImage(t *testing.T) {
	fake := &fakeK8s{}
	k := newTestK8sProvisioner(fake)

	const img = "registry.example/custom-disk:latest"
	if _, err := k.CreateEnvironment(context.Background(), KindEphemeral, img); err != nil {
		t.Fatalf("CreateEnvironment: %v", err)
	}
	if len(fake.created) != 1 || fake.created[0].Image != img {
		t.Errorf("image = %+v, want %q", fake.created, img)
	}
}

// create 失败 → 报错且环境未登记。
func TestK8sCreateFailureNotTracked(t *testing.T) {
	fake := &fakeK8s{failCreate: true}
	k := newTestK8sProvisioner(fake)

	env, err := k.CreateEnvironment(context.Background(), KindEphemeral, "")
	if err == nil {
		t.Fatal("expected create failure error")
	}
	if env != nil {
		t.Errorf("env = %+v, want nil on failure", env)
	}
	k.mu.Lock()
	n := len(k.envs)
	k.mu.Unlock()
	if n != 0 {
		t.Errorf("tracked envs = %d, want 0 after create failure", n)
	}
}

// wait-running 失败 → 尽力回收（DeleteVMI 被调用）。
func TestK8sCreateWaitFailureCleansUp(t *testing.T) {
	fake := &fakeK8s{failWaitRunning: true}
	k := newTestK8sProvisioner(fake)

	if _, err := k.CreateEnvironment(context.Background(), KindEphemeral, ""); err == nil {
		t.Fatal("expected wait-running failure error")
	}
	if len(fake.deleted) != 1 {
		t.Errorf("deleted = %v, want cleanup delete after wait failure", fake.deleted)
	}
}

// 销毁：删除对应 VMI 并出内存表。
func TestK8sDestroyEnvironment(t *testing.T) {
	fake := &fakeK8s{}
	k := newTestK8sProvisioner(fake)

	env, err := k.CreateEnvironment(context.Background(), KindEphemeral, "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := k.DestroyEnvironment(context.Background(), env.ID); err != nil {
		t.Fatalf("destroy: %v", err)
	}
	if len(fake.deleted) != 1 || fake.deleted[0] != env.ProviderRef {
		t.Errorf("deleted = %v, want [%s]", fake.deleted, env.ProviderRef)
	}
	if _, err := k.lookupEnv(context.Background(), env.ID); err == nil {
		t.Error("env should be gone after destroy")
	}
}

// 销毁不存在的环境 → 报错。
func TestK8sDestroyUnknownEnvironment(t *testing.T) {
	fake := &fakeK8s{}
	k := newTestK8sProvisioner(fake)

	if err := k.DestroyEnvironment(context.Background(), "does-not-exist"); err == nil {
		t.Fatal("expected error destroying unknown environment")
	}
}

// ephemeral 复位：删旧 VMI + 按原名重建（containerDisk 无快照，删+重建=净初态）。
func TestK8sResetEphemeralRecreates(t *testing.T) {
	fake := &fakeK8s{}
	k := newTestK8sProvisioner(fake)

	env, err := k.CreateEnvironment(context.Background(), KindEphemeral, "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	name := env.ProviderRef
	if err := k.ResetEphemeral(context.Background(), env.ID); err != nil {
		t.Fatalf("reset: %v", err)
	}
	if len(fake.deleted) != 1 || fake.deleted[0] != name {
		t.Errorf("deleted = %v, want [%s] on reset", fake.deleted, name)
	}
	// 初次 create + reset 重建 = 2 次 CreateVMI，均同名。
	if len(fake.created) != 2 {
		t.Fatalf("created = %d, want 2 (initial + reset recreate)", len(fake.created))
	}
	if fake.created[1].Name != name {
		t.Errorf("recreated vmi name = %q, want %q (same name)", fake.created[1].Name, name)
	}
}

// persistent 环境不可复位。
func TestK8sResetPersistentRejected(t *testing.T) {
	fake := &fakeK8s{}
	k := newTestK8sProvisioner(fake)

	env, err := k.CreateEnvironment(context.Background(), KindPersistent, "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := k.ResetEphemeral(context.Background(), env.ID); err == nil {
		t.Fatal("expected error resetting persistent environment")
	}
	if len(fake.deleted) != 0 {
		t.Errorf("delete should not run for persistent reset, got %v", fake.deleted)
	}
}

// reset 删除失败 → ErrResetFailed（降级信号）。
func TestK8sResetDeleteFailureDegrades(t *testing.T) {
	fake := &fakeK8s{}
	k := newTestK8sProvisioner(fake)

	env, err := k.CreateEnvironment(context.Background(), KindEphemeral, "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	fake.failDelete = true
	err = k.ResetEphemeral(context.Background(), env.ID)
	if !errors.Is(err, ErrResetFailed) {
		t.Errorf("err = %v, want ErrResetFailed", err)
	}
}

// Status 透传底层 VMI phase。
func TestK8sStatusReportsPhase(t *testing.T) {
	fake := &fakeK8s{}
	k := newTestK8sProvisioner(fake)

	env, err := k.CreateEnvironment(context.Background(), KindEphemeral, "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	fake.statuses = map[string]string{env.ProviderRef: "Scheduling"}
	phase, err := k.Status(context.Background(), env.ID)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if phase != "Scheduling" {
		t.Errorf("phase = %q, want Scheduling", phase)
	}
}

// K8sProvisioner 满足 EnvProvider（编译期已由 provider.go 断言，此处运行期再确认可注入）。
func TestK8sSatisfiesEnvProvider(t *testing.T) {
	var _ EnvProvider = newTestK8sProvisioner(&fakeK8s{})
}

// M10 M-9 同构对齐（TASK-007，需 AURA_TEST_PG_DSN 缺省 skip）：K8s 侧 stale destroy 防护——
// 副本 A create → 副本 B destroy（PG 行删除）→ A 以 stale 内存条目再 destroy → not found 拒绝
// 且 A 的 DeleteVMI 零调用（不误删他方同名重建的 VMI）。
func TestK8sDualReplicaStaleDestroyPG(t *testing.T) {
	dsn := os.Getenv("AURA_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("set AURA_TEST_PG_DSN to run k8s dual-replica stale destroy integration")
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

	fakeA, fakeB := &fakeK8s{}, &fakeK8s{}
	kA := NewK8sProvisioner(fakeA, stA, DefaultK8sConfig())
	kB := NewK8sProvisioner(fakeB, stB, DefaultK8sConfig())

	env, err := kA.CreateEnvironment(ctx, KindEphemeral, "")
	if err != nil {
		t.Fatalf("A create: %v", err)
	}
	// B 副本销毁：B 内存无该 env → PG 权威读命中 provider_ref → 动手 + 删行。
	if err := kB.DestroyEnvironment(ctx, env.ID); err != nil {
		t.Fatalf("B destroy: %v", err)
	}
	if len(fakeB.deleted) != 1 || fakeB.deleted[0] != env.ProviderRef {
		t.Errorf("B deleted=%v, want [%s]（PG 权威读跨副本命中 provider_ref）", fakeB.deleted, env.ProviderRef)
	}
	// A 以 stale 内存条目重试 destroy → PG 行已无 → 拒绝，且 A 的 DeleteVMI 绝不触发。
	err = kA.DestroyEnvironment(ctx, env.ID)
	if err == nil {
		t.Fatal("A stale destroy 应被拒绝（PG 行已删），实际成功")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("stale destroy 错误=%q, want 含 not found", err)
	}
	if len(fakeA.deleted) != 0 {
		t.Errorf("A deleted=%v, want 空——stale 条目不得触发 DeleteVMI", fakeA.deleted)
	}
}
