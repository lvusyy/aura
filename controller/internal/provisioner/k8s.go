// k8s.go 在 KubeVirt 上编排 ephemeral/persistent 环境的生命周期，与 pve.go 平行：
// 复刻 pveAPI/fakePVE 模板，把 client-go 表面隔离在窄接口 k8sAPI 的单一适配器
// dynamicKubeVirtAPI 内（Locked-3：k8s.io/* SDK 不裸 import 散布），编排层 K8sProvisioner
// 只依赖 k8sAPI、可脱离真集群单测。
//
// 形态决策（ephemeral-spike 定案，见 evidence/m3/k8s-ephemeral-spike.txt）：
//   - client 选型=client-go dynamic client + GVR（避 kubevirt.io/client-go 重依赖）；
//     VMI 用 GVR {kubevirt.io, v1, virtualmachineinstances} 直接编排，消费既有 CRD，不建 CRD（D2）；
//   - ephemeral=containerDisk（镜像即盘，无 PVC）：集群无 CSI VolumeSnapshot（local-path 唯一 SC），
//     CDI DataVolume clone 退化为全盘拷贝，故复位=删除 VMI 并按原 spec 重建（overlay 天然弃写，
//     等价 PVE 的 rollback-to-baseline），无需快照。
//
// SAFETY：AURA 资源全部落独立 namespace（默认 "aura"）+ 名称前缀 aura-ephem-/aura-persist-；
// DestroyEnvironment 只删自建 VMI（按 ProviderRef 名），绝不触碰 default ns 既有 VM。
package provisioner

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/google/uuid"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/aura/controller/internal/observability"
	"github.com/aura/controller/internal/store"
)

// k8sAPI 抽象 K8sProvisioner 需要的 KubeVirt VMI 操作，隔离 client-go/dynamic 表面（便于 mock 单测）。
// 语义对齐 pveAPI：Create/Delete/GetStatus 为单次操作，WaitVMIRunning/WaitVMIGone 阻塞轮询至就绪/消失或超时。
type k8sAPI interface {
	// CreateVMI 在目标 namespace 建一个 containerDisk VMI（不阻塞等待就绪）。
	CreateVMI(ctx context.Context, spec vmiSpec) error
	// DeleteVMI 删除指定 VMI；NotFound 视为成功（幂等）。
	DeleteVMI(ctx context.Context, name string) error
	// GetVMIStatus 返回 VMI 的 status.phase（Pending|Scheduling|Running|Failed|Succeeded|...）。
	GetVMIStatus(ctx context.Context, name string) (string, error)
	// WaitVMIRunning 轮询至 VMI 进入 Running（或进入终态/超时/ctx 取消报错）。
	WaitVMIRunning(ctx context.Context, name string, timeout time.Duration) error
	// WaitVMIGone 轮询至 VMI 彻底消失（删除完成）或超时。
	WaitVMIGone(ctx context.Context, name string, timeout time.Duration) error
}

// vmiSpec 是建一个 VMI 所需的最小规格（编排层构造，adapter 翻译为 unstructured VMI）。
type vmiSpec struct {
	Name   string            // VMI 名（RFC1123 label；aura-ephem-/aura-persist- 前缀 + env UUID）
	Image  string            // containerDisk 镜像
	Memory string            // 内存请求，如 "128Mi"
	Labels map[string]string // 元数据 label（含 managed-by=aura 便于审计与安全边界）
}

// K8sConfig 是 K8sProvisioner 配置（NewK8sProvisionerFromEnv 从环境变量填充，带默认值）。
type K8sConfig struct {
	Namespace      string        // 隔离 namespace（SAFETY：AURA 资源全部落此），默认 "aura"
	EphemeralImage string        // 默认 containerDisk 镜像
	Memory         string        // VMI 内存请求，默认 "128Mi"
	RunningTimeout time.Duration // 等 VMI Running 上限，默认 180s
	DeleteTimeout  time.Duration // 等 VMI 消失上限，默认 120s
}

// DefaultK8sConfig 返回带默认值的配置。
func DefaultK8sConfig() K8sConfig {
	return K8sConfig{
		Namespace:      "aura",
		EphemeralImage: "quay.io/kubevirt/cirros-container-disk-demo",
		Memory:         "128Mi",
		RunningTimeout: 180 * time.Second,
		DeleteTimeout:  120 * time.Second,
	}
}

// K8sProvisioner 在 KubeVirt 上编排环境生命周期。store 可为 nil（纯内存自洽，同 PVE Provisioner）。
type K8sProvisioner struct {
	api   k8sAPI
	store *store.PGStore // 可为 nil
	cfg   K8sConfig

	mu   sync.Mutex
	envs map[string]*Environment // env_id -> 环境（内存主表）
}

// NewK8sProvisioner 用给定 k8sAPI 构造编排层（供单测注入 mock）。store 可为 nil。
func NewK8sProvisioner(api k8sAPI, st *store.PGStore, cfg K8sConfig) *K8sProvisioner {
	def := DefaultK8sConfig()
	if cfg.Namespace == "" {
		cfg.Namespace = def.Namespace
	}
	if cfg.EphemeralImage == "" {
		cfg.EphemeralImage = def.EphemeralImage
	}
	if cfg.Memory == "" {
		cfg.Memory = def.Memory
	}
	if cfg.RunningTimeout == 0 {
		cfg.RunningTimeout = def.RunningTimeout
	}
	if cfg.DeleteTimeout == 0 {
		cfg.DeleteTimeout = def.DeleteTimeout
	}
	return &K8sProvisioner{
		api:   api,
		store: st,
		cfg:   cfg,
		envs:  make(map[string]*Environment),
	}
}

// NewK8sProvisionerFromEnv 从环境变量构造生产用 K8sProvisioner（client-go dynamic client）。
//   - AURA_K8S_KUBECONFIG：kubeconfig 路径（必填；指向 k3s，server 为集群 API endpoint）
//   - AURA_K8S_NAMESPACE：隔离 namespace（可选，默认 "aura"）
//   - AURA_K8S_EPHEMERAL_IMAGE：ephemeral containerDisk 镜像（可选）
//
// store 可为 nil（未配置 PG 时环境仅存内存）。
func NewK8sProvisionerFromEnv(st *store.PGStore) (*K8sProvisioner, error) {
	kubeconfig := os.Getenv("AURA_K8S_KUBECONFIG")
	if kubeconfig == "" {
		return nil, errors.New("AURA_K8S_KUBECONFIG must be set")
	}
	restCfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("build kube client config from %q: %w", kubeconfig, err)
	}
	dyn, err := dynamic.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("new dynamic client: %w", err)
	}

	cfg := DefaultK8sConfig()
	if v := os.Getenv("AURA_K8S_NAMESPACE"); v != "" {
		cfg.Namespace = v
	}
	if v := os.Getenv("AURA_K8S_EPHEMERAL_IMAGE"); v != "" {
		cfg.EphemeralImage = v
	}

	api := &dynamicKubeVirtAPI{client: dyn, namespace: cfg.Namespace}
	return NewK8sProvisioner(api, st, cfg), nil
}

// CreateEnvironment 建一个 containerDisk VMI 并等待 Running。返回环境（NodeID 为空，节点自行反连注册）。
// kind 空默认 ephemeral；template 非空覆盖 containerDisk 镜像。
func (k *K8sProvisioner) CreateEnvironment(ctx context.Context, kind, template string) (*Environment, error) {
	start := time.Now()
	kind = normalizeKind(kind)
	envID := uuid.NewString()
	name := vmiName(kind, envID)
	image := k.cfg.EphemeralImage
	if template != "" {
		image = template
	}

	spec := k.specFor(name, image, kind, envID)
	if err := k.api.CreateVMI(ctx, spec); err != nil {
		return nil, fmt.Errorf("create vmi %s: %w", name, err)
	}
	if err := k.api.WaitVMIRunning(ctx, name, k.cfg.RunningTimeout); err != nil {
		k.deleteQuietly(name)
		return nil, fmt.Errorf("wait vmi %s running: %w", name, err)
	}

	env := &Environment{ID: envID, Kind: kind, Provider: providerK8s, ProviderRef: name}
	k.trackEnv(env)
	k.persistCreate(env)
	observability.ObserveProvision(providerK8s, kind, time.Since(start).Seconds())
	slog.Info("k8s environment created", "env_id", envID, "vmi", name, "kind", kind)
	return env, nil
}

// DestroyEnvironment 删除环境底层 VMI，出内存表并更新持久化。
// destroy 目标经 destroyTarget 以 PG 为权威（store 在时）——与 PVE 侧同构对齐（M-9）：他副本已毁
// 后本副本内存条目 stale，以其动手会误删同名重建的 VMI。
func (k *K8sProvisioner) DestroyEnvironment(ctx context.Context, envID string) error {
	env, err := k.destroyTarget(ctx, envID)
	if err != nil {
		return err
	}
	k.persistStatus(envID, statusDestroying)
	if err := k.api.DeleteVMI(ctx, env.ProviderRef); err != nil {
		k.persistStatus(envID, statusError)
		return fmt.Errorf("delete vmi %s: %w", env.ProviderRef, err)
	}
	k.untrackEnv(envID)
	k.persistDelete(envID)
	slog.Info("k8s environment destroyed", "env_id", envID, "vmi", env.ProviderRef)
	return nil
}

// destroyTarget 解析 destroy 目标（镜像 PVE Provisioner.destroyTarget，M-9 防误毁）：store 在时
// 一律重读 PG 权威源——行不存在 → 拒绝并清本地 stale cache；PG 读故障 → fail-closed 上抛拒绝
// 动手。store 为 nil 保持纯内存 lookupEnv 契约（单副本零变化）。
func (k *K8sProvisioner) destroyTarget(ctx context.Context, envID string) (*Environment, error) {
	if k.store == nil {
		return k.lookupEnv(ctx, envID)
	}
	rec, err := k.store.GetEnvironment(ctx, envID)
	if err != nil {
		if store.IsNotFound(err) {
			k.untrackEnv(envID)
			return nil, fmt.Errorf("environment %s not found (already destroyed or unknown)", envID)
		}
		return nil, fmt.Errorf("destroy %s: authoritative store lookup failed: %w", envID, err)
	}
	return &Environment{
		ID:          rec.ID,
		VMID:        int(rec.VMID),
		Kind:        rec.Kind,
		NodeID:      rec.NodeID,
		Provider:    rec.Provider,
		ProviderRef: rec.ProviderRef,
	}, nil
}

// ResetEphemeral 复位一个 ephemeral 环境到净初态：删除 VMI（弃置 containerDisk overlay 全部写入）
// 并按原 spec 重建。非 ephemeral 拒绝。任一步失败即降级：标记 degraded 并返回 ErrResetFailed，
// 由调用方走 destroy+重建深度清理（与 PVE ResetEphemeral 语义对齐）。
func (k *K8sProvisioner) ResetEphemeral(ctx context.Context, envID string) error {
	env, err := k.lookupEnv(ctx, envID)
	if err != nil {
		return err
	}
	if env.Kind != KindEphemeral {
		return fmt.Errorf("environment %s is %s, not resettable", envID, env.Kind)
	}
	name := env.ProviderRef
	if err := k.api.DeleteVMI(ctx, name); err != nil {
		k.persistStatus(envID, statusDegraded)
		return fmt.Errorf("%w: delete vmi %s: %v", ErrResetFailed, name, err)
	}
	// 等旧 VMI 彻底消失后再重建同名，避免 AlreadyExists（终止中残留）。
	if err := k.api.WaitVMIGone(ctx, name, k.cfg.DeleteTimeout); err != nil {
		k.persistStatus(envID, statusDegraded)
		return fmt.Errorf("%w: wait vmi %s gone: %v", ErrResetFailed, name, err)
	}
	if err := k.api.CreateVMI(ctx, k.specFor(name, k.cfg.EphemeralImage, env.Kind, env.ID)); err != nil {
		k.persistStatus(envID, statusDegraded)
		return fmt.Errorf("%w: recreate vmi %s: %v", ErrResetFailed, name, err)
	}
	if err := k.api.WaitVMIRunning(ctx, name, k.cfg.RunningTimeout); err != nil {
		k.persistStatus(envID, statusDegraded)
		return fmt.Errorf("%w: wait recreated vmi %s running: %v", ErrResetFailed, name, err)
	}
	k.persistStatus(envID, statusReady)
	slog.Info("k8s environment reset", "env_id", envID, "vmi", name)
	return nil
}

// Status 返回环境底层 VMI 的 status.phase（KubeVirt 词表："Pending" | "Scheduling" | "Running" |
// "Succeeded" | "Failed" | ...），不归一到统一状态机（provider-aware，见 EnvProvider.Status 文档 /
// DD4）。与 Provisioner.Status 的 PVE 原始 VM 态词表语义不同，调用方须按部署所选 provider 解读。
func (k *K8sProvisioner) Status(ctx context.Context, envID string) (string, error) {
	env, err := k.lookupEnv(ctx, envID)
	if err != nil {
		return "", err
	}
	return k.api.GetVMIStatus(ctx, env.ProviderRef)
}

// ===== 内部：命名 / spec 构造 =====

// vmiName 按 kind 加前缀（SAFETY：aura- 前缀便于安全边界识别）+ env UUID（唯一，≤63 RFC1123 label）。
func vmiName(kind, envID string) string {
	prefix := "aura-ephem-"
	if kind == KindPersistent {
		prefix = "aura-persist-"
	}
	return prefix + envID
}

// specFor 构造一个 VMI 规格（create 与 reset-重建共用，保证同名重建 spec 一致）。
func (k *K8sProvisioner) specFor(name, image, kind, envID string) vmiSpec {
	return vmiSpec{
		Name:   name,
		Image:  image,
		Memory: k.cfg.Memory,
		Labels: map[string]string{
			"app.kubernetes.io/managed-by": "aura",
			"aura.io/env-id":               envID,
			"aura.io/kind":                 kind,
		},
	}
}

// ===== 内部：内存 env 表（镜像 PVE Provisioner）=====

func (k *K8sProvisioner) trackEnv(env *Environment) {
	k.mu.Lock()
	k.envs[env.ID] = env
	k.mu.Unlock()
}

func (k *K8sProvisioner) untrackEnv(envID string) {
	k.mu.Lock()
	delete(k.envs, envID)
	k.mu.Unlock()
}

// lookupEnv 取环境：内存优先；未命中（如控制面重启后）且 store 存在则回读 store 恢复 ProviderRef。
func (k *K8sProvisioner) lookupEnv(ctx context.Context, envID string) (*Environment, error) {
	k.mu.Lock()
	env, ok := k.envs[envID]
	k.mu.Unlock()
	if ok {
		return env, nil
	}
	if k.store != nil {
		rec, err := k.store.GetEnvironment(ctx, envID)
		if err != nil {
			return nil, fmt.Errorf("environment %s not found: %w", envID, err)
		}
		return &Environment{
			ID:          rec.ID,
			VMID:        int(rec.VMID),
			Kind:        rec.Kind,
			NodeID:      rec.NodeID,
			Provider:    rec.Provider,
			ProviderRef: rec.ProviderRef,
		}, nil
	}
	return nil, fmt.Errorf("environment %s not found", envID)
}

// deleteQuietly 尽力回收（置备中途失败的清理），错误仅告警。用独立 ctx 不受调用方取消影响。
func (k *K8sProvisioner) deleteQuietly(name string) {
	ctx, cancel := context.WithTimeout(context.Background(), k.cfg.DeleteTimeout)
	defer cancel()
	if err := k.api.DeleteVMI(ctx, name); err != nil {
		slog.Warn("cleanup delete vmi failed", "vmi", name, "err", err)
	}
}

// ===== 内部：可选持久化（store 为 nil 时全部跳过，镜像 PVE Provisioner）=====

func (k *K8sProvisioner) persistCreate(env *Environment) {
	if k.store == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rec := store.EnvironmentRecord{
		ID:          env.ID,
		VMID:        int32(env.VMID), // K8s 为 0 -> store 存 NULL（vmid 保留 PVE 行专用）
		Kind:        env.Kind,
		NodeID:      env.NodeID,
		Status:      statusReady,
		Provider:    env.Provider,
		ProviderRef: env.ProviderRef,
	}
	if err := k.store.CreateEnvironment(ctx, rec); err != nil {
		slog.Warn("persist environment failed", "env_id", env.ID, "err", err)
	}
}

func (k *K8sProvisioner) persistStatus(envID, status string) {
	if k.store == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := k.store.UpdateEnvironmentStatus(ctx, envID, status); err != nil {
		slog.Warn("update environment status failed", "env_id", envID, "status", status, "err", err)
	}
}

func (k *K8sProvisioner) persistDelete(envID string) {
	if k.store == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := k.store.DeleteEnvironment(ctx, envID); err != nil {
		slog.Warn("delete environment failed", "env_id", envID, "err", err)
	}
}

// ===== client-go dynamic 适配器（隔离第三方表面，Locked-3：k8s.io/* 仅此文件）=====

// vmiGVR 是 KubeVirt VirtualMachineInstance 的 GroupVersionResource（消费既有 CRD，不建 CRD）。
var vmiGVR = schema.GroupVersionResource{Group: "kubevirt.io", Version: "v1", Resource: "virtualmachineinstances"}

// dynamicKubeVirtAPI 用 client-go dynamic client + GVR 实现 k8sAPI（避 kubevirt.io/client-go 重依赖，
// ephemeral-spike 定案）。所有 unstructured/GVR 细节收敛于此，编排层不感知。
type dynamicKubeVirtAPI struct {
	client    dynamic.Interface
	namespace string
}

func (d *dynamicKubeVirtAPI) vmis() dynamic.ResourceInterface {
	return d.client.Resource(vmiGVR).Namespace(d.namespace)
}

func (d *dynamicKubeVirtAPI) CreateVMI(ctx context.Context, spec vmiSpec) error {
	labels := make(map[string]any, len(spec.Labels))
	for key, val := range spec.Labels {
		labels[key] = val
	}
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kubevirt.io/v1",
		"kind":       "VirtualMachineInstance",
		"metadata": map[string]any{
			"name":      spec.Name,
			"namespace": d.namespace,
			"labels":    labels,
		},
		"spec": map[string]any{
			// ephemeral：guest 无需优雅关机，0 秒宽限加速拆除/复位（containerDisk 无持久盘）。
			"terminationGracePeriodSeconds": int64(0),
			"domain": map[string]any{
				"devices": map[string]any{
					"disks": []any{
						map[string]any{
							"name": "containerdisk",
							"disk": map[string]any{"bus": "virtio"},
						},
					},
				},
				"resources": map[string]any{
					"requests": map[string]any{"memory": spec.Memory},
				},
			},
			"volumes": []any{
				map[string]any{
					"name":          "containerdisk",
					"containerDisk": map[string]any{"image": spec.Image},
				},
			},
		},
	}}
	_, err := d.vmis().Create(ctx, obj, metav1.CreateOptions{})
	return err
}

func (d *dynamicKubeVirtAPI) DeleteVMI(ctx context.Context, name string) error {
	err := d.vmis().Delete(ctx, name, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return nil // 幂等
	}
	return err
}

func (d *dynamicKubeVirtAPI) GetVMIStatus(ctx context.Context, name string) (string, error) {
	u, err := d.vmis().Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	phase, _, err := unstructured.NestedString(u.Object, "status", "phase")
	if err != nil {
		return "", err
	}
	return phase, nil
}

func (d *dynamicKubeVirtAPI) WaitVMIRunning(ctx context.Context, name string, timeout time.Duration) error {
	return d.poll(ctx, timeout, func() (bool, error) {
		phase, err := d.GetVMIStatus(ctx, name)
		if err != nil {
			return false, nil // 尚不可见/瞬态错误，继续轮询
		}
		switch phase {
		case "Running":
			return true, nil
		case "Failed", "Succeeded":
			return false, fmt.Errorf("vmi %s entered terminal phase %q", name, phase)
		default:
			return false, nil
		}
	})
}

func (d *dynamicKubeVirtAPI) WaitVMIGone(ctx context.Context, name string, timeout time.Duration) error {
	return d.poll(ctx, timeout, func() (bool, error) {
		_, err := d.vmis().Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return false, nil
	})
}

// poll 每 3s 调 cond 一次直到 done、cond 报致命错、超时或 ctx 取消。首次立即探测。
func (d *dynamicKubeVirtAPI) poll(ctx context.Context, timeout time.Duration, cond func() (bool, error)) error {
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for {
		done, err := cond()
		if err != nil {
			return err
		}
		if done {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if time.Now().After(deadline) {
				return fmt.Errorf("timed out after %s", timeout)
			}
		}
	}
}
