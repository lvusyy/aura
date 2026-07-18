// Package provisioner 管理 PVE 上的环境生命周期：从模板克隆节点 VM、启动、等 guest-agent 就绪，
// ephemeral 环境建基线 snapshot 供秒级 rollback 复位，persistent 环境保留不 rollback。
//
// 分层：
//   - pveAPI 接口抽象 PVE 操作（Clone/Start/WaitAgent/Snapshot/Rollback/Destroy/Status），
//     把第三方 go-proxmox 表面隔离在一处，编排逻辑与之解耦、可脱离真 PVE 单测；
//   - Provisioner 是编排层：vmid 分配、内存 env 表、可选 store 持久化（store 为 nil 时纯内存自洽，
//     镜像 registry/scheduler 的 store-optional 模式）。
//
// 运行时仅用 clone/snapshot/rollback/destroy/status（Locked 决策 5）；模板一次性由 make-template.sh
// 经 SSH+qm 制作，不确定的模板 API 移出关键路径。存储为 local-lvm(LVM-thin)，snapshot/rollback 走
// go-proxmox/qm 通用实现，不硬编码 ZFS 命令。
package provisioner

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/luthermonson/go-proxmox"

	"github.com/aura/controller/internal/observability"
	"github.com/aura/controller/internal/store"
)

// 环境类型。
const (
	KindEphemeral  = "ephemeral"
	KindPersistent = "persistent"
)

// 环境记录状态（写入 environments.status）。
const (
	statusReady      = "ready"
	statusDestroying = "destroying"
	statusDegraded   = "degraded" // rollback 失败降级：待 destroy+reclone 深度清理
	statusError      = "error"
)

// ErrResetFailed 表示 ephemeral rollback 复位失败；调用方据此走 destroy+reclone 深度清理。
var ErrResetFailed = errors.New("ephemeral reset (snapshot rollback) failed")

// pveAPI 抽象 provisioner 需要的 PVE 操作，隔离 go-proxmox 表面（便于 mock 单测）。
// 所有方法阻塞至对应 PVE task 完成或 ctx 取消。
type pveAPI interface {
	// Clone 全量克隆 templateVMID 到 newVMID（name 仅供可读），阻塞至克隆 task 完成。
	Clone(ctx context.Context, templateVMID, newVMID int, name string) error
	// Start 开机并阻塞至 start task 完成。
	Start(ctx context.Context, vmid int) error
	// WaitAgent 阻塞至 qemu-guest-agent 应答或 timeout 到期（VM 已启动完成的判据）。
	WaitAgent(ctx context.Context, vmid int, timeout time.Duration) error
	// Snapshot 建命名快照并阻塞至完成。
	Snapshot(ctx context.Context, vmid int, name string) error
	// Rollback 回滚到命名快照并阻塞至完成。
	Rollback(ctx context.Context, vmid int, name string) error
	// Destroy 停机（若在运行）并删除 VM，阻塞至完成。
	Destroy(ctx context.Context, vmid int) error
	// Status 返回 VM 运行态（"running" | "stopped" | ...）。
	Status(ctx context.Context, vmid int) (string, error)
	// ClusterNextID 返回 PVE 集群建议的下一个空闲 VMID（/cluster/nextid），供启动播种 nextVMID
	// 游标避免与集群内既有 VM（含非 AURA 建的）撞号；不可得时由调用方回落其他源（PG / VMIDBase）。
	ClusterNextID(ctx context.Context) (int, error)
}

// vmidStore 抽象 vmid 游标的 PG 权威面（*store.PGStore 满足之）；单测注入 fake 实现，
// 不依赖真 PG（对齐 pveAPI 的 mock 隔离规约：第三方/外部依赖经窄接口消费）。
type vmidStore interface {
	// MaxVMID 供启动三源播种（environments 表存量最大 vmid）。
	MaxVMID(ctx context.Context) (int, error)
	// AllocVMID 经 PG 单行游标原子取号（UPDATE..RETURNING），双副本并发 create 不撞号（M10）。
	AllocVMID(ctx context.Context) (int, error)
	// SeedVMID 幂等抬升游标下限（GREATEST 只升不降），双副本启动并发播种安全（M10）。
	SeedVMID(ctx context.Context, floor int) error
}

// Config 是 provisioner 配置（NewProvisionerFromEnv 从环境变量填充，带默认值）。
type Config struct {
	Node         string        // PVE 节点名（PVE 集群内的宿主名）
	TemplateVMID int           // 默认模板 VMID（make-template.sh 建的模板，默认 9100）
	VMIDBase     int           // vmid 分配基址（自增），默认 9200，避开控制面 9000 / 模板 9100
	BaseSnapshot string        // ephemeral 基线快照名，默认 "aura-base"
	AgentTimeout time.Duration // 等 guest-agent 就绪的上限，默认 120s
}

// DefaultConfig 返回带默认值的配置。
func DefaultConfig() Config {
	return Config{
		Node:         "pve",
		TemplateVMID: 9100,
		VMIDBase:     9200,
		BaseSnapshot: "aura-base",
		AgentTimeout: 120 * time.Second,
	}
}

// Environment 是一个已置备环境（内存视图）。
type Environment struct {
	ID     string // 控制面分配的环境 UUID
	VMID   int    // PVE VMID（PVE 专用；K8s 环境为 0，句柄见 ProviderRef）
	Kind   string // ephemeral | persistent
	NodeID string // 环境内节点注册后的 UUID；always empty at provision time（Register 帧无 env_id，节点启动后异步反连注册）——env↔node 关联经 auractl node list（G-3/ISS-07）

	// Provider 标识承载后端（"pve" | "k8s"）；ProviderRef 是 provider 专属句柄
	// （PVE=vmid 串 / K8s=VMI 名），供跨内存/store 恢复后定位底层资源（M3 additive）。
	Provider    string
	ProviderRef string
}

// Provisioner 编排环境生命周期。store 可为 nil（纯内存）。
type Provisioner struct {
	api     pveAPI
	store   *store.PGStore // 可为 nil
	vmidSrc vmidStore      // vmid 游标权威源：播种+原子取号（默认=store；单测注入 fake，规避真 PG 依赖）
	cfg     Config

	mu       sync.Mutex
	envs     map[string]*Environment // env_id -> 环境（内存主表）
	nextVMID int                     // vmid 自增游标（构造期经 seedNextVMID 播种）
}

// NewProvisioner 用给定 pveAPI 构造编排层（供单测注入 mock）。store 可为 nil。
// 构造即播种 nextVMID 游标（seedNextVMID：max(VMIDBase, PG MaxVMID+1, PVE nextid)）。
func NewProvisioner(api pveAPI, st *store.PGStore, cfg Config) *Provisioner {
	var src vmidStore
	if st != nil {
		src = st // 仅在非 nil 时赋值，规避 typed-nil（具体 nil 指针赋接口后接口非 nil，MEMORY）
	}
	return newProvisioner(api, st, src, cfg)
}

// newProvisioner 是 NewProvisioner 的内部实现，vmidSrc 显式注入：生产为 *store.PGStore，
// 单测传 fake vmidStore（不依赖真 PG）。构造期同步播种 nextVMID。
func newProvisioner(api pveAPI, st *store.PGStore, vmidSrc vmidStore, cfg Config) *Provisioner {
	if cfg.VMIDBase == 0 {
		cfg.VMIDBase = DefaultConfig().VMIDBase
	}
	p := &Provisioner{
		api:      api,
		store:    st,
		vmidSrc:  vmidSrc,
		cfg:      cfg,
		envs:     make(map[string]*Environment),
		nextVMID: cfg.VMIDBase,
	}
	p.seedNextVMID(context.Background())
	return p
}

// NewProvisionerFromEnv 从环境变量构造生产用 provisioner（go-proxmox + API token）。
//   - AURA_PVE_URL：PVE API 基址，如 https://<pve-host>:8006/api2/json
//   - AURA_PVE_TOKEN_ID：API token id，如 root@pam!aura
//   - AURA_PVE_SECRET：API token secret（UUID）
//   - AURA_PVE_INSECURE：非 "false" 时跳过 TLS 校验（PVE 自签证书，默认 true）
//   - AURA_PVE_NODE / AURA_PVE_TEMPLATE_VMID / AURA_PVE_VMID_BASE：可选覆盖默认配置
//
// store 可为 nil（未配置 PG 时环境仅存内存）。
func NewProvisionerFromEnv(st *store.PGStore) (*Provisioner, error) {
	url := os.Getenv("AURA_PVE_URL")
	tokenID := os.Getenv("AURA_PVE_TOKEN_ID")
	secret := os.Getenv("AURA_PVE_SECRET")
	if url == "" || tokenID == "" || secret == "" {
		return nil, errors.New("AURA_PVE_URL / AURA_PVE_TOKEN_ID / AURA_PVE_SECRET must all be set")
	}

	insecure := os.Getenv("AURA_PVE_INSECURE") != "false"
	httpClient := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			// PVE 默认自签证书；M2 跳过校验（Deferred：证书轮换/校验入 M3）。
			TLSClientConfig: &tls.Config{InsecureSkipVerify: insecure}, //nolint:gosec // PVE 自签，M2 显式放行
		},
	}

	// go-proxmox 客户端构造 + API token 认证。
	// NOTE: go-proxmox NewClient/WithAPIToken/WithHTTPClient 签名待远程 `go build` 验证。
	client := proxmox.NewClient(url,
		proxmox.WithHTTPClient(httpClient),
		proxmox.WithAPIToken(tokenID, secret),
	)

	cfg := DefaultConfig()
	if v := os.Getenv("AURA_PVE_NODE"); v != "" {
		cfg.Node = v
	}
	if v := os.Getenv("AURA_PVE_TEMPLATE_VMID"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.TemplateVMID = n
		}
	}
	if v := os.Getenv("AURA_PVE_VMID_BASE"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.VMIDBase = n
		}
	}

	api := &goProxmoxAPI{client: client, node: cfg.Node}
	return NewProvisioner(api, st, cfg), nil
}

// CreateEnvironment 从模板克隆一个节点 VM 并置备：clone → start → 等 guest-agent →
// ephemeral 建基线快照。返回环境（NodeID 为空，节点启动后自行反连注册）。
// kind 空默认 ephemeral；template 空用默认模板 VMID，非空须为数字 VMID 字符串。
func (p *Provisioner) CreateEnvironment(ctx context.Context, kind, template string) (*Environment, error) {
	start := time.Now()
	kind = normalizeKind(kind)
	templateVMID, err := p.resolveTemplate(template)
	if err != nil {
		return nil, err
	}

	vmid := p.allocVMID(ctx)
	envID := uuid.NewString()
	name := fmt.Sprintf("aura-node-%d", vmid)

	// 1) 克隆。
	if err := p.api.Clone(ctx, templateVMID, vmid, name); err != nil {
		return nil, fmt.Errorf("clone template %d -> %d: %w", templateVMID, vmid, err)
	}
	// 2) 开机。失败尽力回收克隆体。
	if err := p.api.Start(ctx, vmid); err != nil {
		p.destroyQuietly(vmid)
		return nil, fmt.Errorf("start vm %d: %w", vmid, err)
	}
	// 3) 等 guest-agent 就绪（VM 启动完成判据）。
	if err := p.api.WaitAgent(ctx, vmid, p.cfg.AgentTimeout); err != nil {
		p.destroyQuietly(vmid)
		return nil, fmt.Errorf("wait guest agent on vm %d: %w", vmid, err)
	}
	// 4) ephemeral：建基线快照供 rollback 秒级复位；persistent 不建。
	if kind == KindEphemeral {
		if err := p.api.Snapshot(ctx, vmid, p.cfg.BaseSnapshot); err != nil {
			p.destroyQuietly(vmid)
			return nil, fmt.Errorf("baseline snapshot vm %d: %w", vmid, err)
		}
	}

	env := &Environment{ID: envID, VMID: vmid, Kind: kind, Provider: providerPVE, ProviderRef: strconv.Itoa(vmid)}
	p.trackEnv(env)
	p.persistCreate(env)
	observability.ObserveProvision(providerPVE, kind, time.Since(start).Seconds())
	slog.Info("environment created", "env_id", envID, "vmid", vmid, "kind", kind)
	return env, nil
}

// DestroyEnvironment 销毁环境：停机 + 删除 VM，出内存表并更新持久化。
// destroy 目标经 destroyTarget 以 PG 为权威（store 在时）——绝不以本副本 stale 内存条目动手：
// 他副本已毁的环境其 vmid 会被 PVE nextid 即刻复用，stale destroy 将误毁复用者 VM（M-9 根除）。
func (p *Provisioner) DestroyEnvironment(ctx context.Context, envID string) error {
	env, err := p.destroyTarget(ctx, envID)
	if err != nil {
		return err
	}
	p.persistStatus(envID, statusDestroying)
	if err := p.api.Destroy(ctx, env.VMID); err != nil {
		p.persistStatus(envID, statusError)
		return fmt.Errorf("destroy vm %d: %w", env.VMID, err)
	}
	p.untrackEnv(envID)
	p.persistDelete(envID)
	slog.Info("environment destroyed", "env_id", envID, "vmid", env.VMID)
	return nil
}

// destroyTarget 解析 destroy 目标。与读路径 lookupEnv 的「内存优先」相反，destroy 路径在 store
// 在时一律重读 PG（跨副本权威源）：行不存在（他副本已毁/未知）→ 拒绝并顺带清本地 stale cache；
// PG 读故障 → 如实上抛拒绝动手（破坏性操作 fail-closed，调用方可重试——不同于 Redis 读路径的
// fail-open 降级原则，误毁不可逆）。store 为 nil 保持纯内存 lookupEnv 契约（单副本零变化）。
func (p *Provisioner) destroyTarget(ctx context.Context, envID string) (*Environment, error) {
	if p.store == nil {
		return p.lookupEnv(ctx, envID)
	}
	rec, err := p.store.GetEnvironment(ctx, envID)
	if err != nil {
		if store.IsNotFound(err) {
			p.untrackEnv(envID)
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

// ResetEphemeral 复位一个 ephemeral 环境到基线快照（秒级 rollback）。
// 非 ephemeral 拒绝。rollback 失败即降级：标记 degraded 并返回 ErrResetFailed，
// 由调用方/运维走 destroy+reclone 深度清理（避免 M2 内隐式换 vmid 的复杂度）。
//
// M2 未接入 admin RPC（proto 仅有 Create/DestroyEnvironment），作为 provisioner 能力预留，
// 供后续“会话结束回收”触发。
func (p *Provisioner) ResetEphemeral(ctx context.Context, envID string) error {
	env, err := p.lookupEnv(ctx, envID)
	if err != nil {
		return err
	}
	if env.Kind != KindEphemeral {
		return fmt.Errorf("environment %s is %s, not resettable", envID, env.Kind)
	}
	if err := p.api.Rollback(ctx, env.VMID, p.cfg.BaseSnapshot); err != nil {
		p.persistStatus(envID, statusDegraded)
		return fmt.Errorf("%w: vm %d rollback to %q: %v", ErrResetFailed, env.VMID, p.cfg.BaseSnapshot, err)
	}
	p.persistStatus(envID, statusReady)
	slog.Info("environment reset", "env_id", envID, "vmid", env.VMID)
	return nil
}

// Status 返回环境底层 VM 的原始运行态（PVE/qemu 词表："running" | "stopped" | ...），不归一到统一
// 状态机（provider-aware，见 EnvProvider.Status 文档 / DD4）。与 K8sProvisioner.Status 的 VMI phase
// 词表语义不同，调用方须按部署所选 provider 解读。
func (p *Provisioner) Status(ctx context.Context, envID string) (string, error) {
	env, err := p.lookupEnv(ctx, envID)
	if err != nil {
		return "", err
	}
	return p.api.Status(ctx, env.VMID)
}

// ===== 内部：vmid 分配 / 模板解析 / kind 归一 =====

// allocVMID 分配 vmid：PG 游标在（vmidSrc 非 nil）时经 AllocVMID 原子取号——双副本并发 create
// 不撞号（各副本进程内独立自增是撞号根源，M10 根除）；PG 取号失败回退进程内游标并告警（未配 PG
// 的单副本纯内存契约不变；双副本部署契约须 PG 在，回退仅兜 CI/dev 与瞬断）。
func (p *Provisioner) allocVMID(ctx context.Context) int {
	if p.vmidSrc != nil {
		vmid, err := p.vmidSrc.AllocVMID(ctx)
		if err == nil {
			return vmid
		}
		slog.Warn("alloc vmid via PG cursor failed, falling back to in-memory counter", "err", err)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	id := p.nextVMID
	p.nextVMID++
	return id
}

// seedNextVMID 播种 vmid 游标：floor = max(VMIDBase, PG MaxVMID+1, PVE cluster nextid)。
// 消除 M2 裸自增（控制面重启后从 VMIDBase 重新计数）与存量 VM 撞号的跨重启风险（自愈三件套之三）。
// 任一数据源不可得（未配 store / PVE 查询失败）即跳过该源，尽力而为，至少回落 VMIDBase。
// floor 双写：进程内游标（PG 不可用时 allocVMID 的回退基准）+ PG vmid_cursor（SeedVMID GREATEST
// 只升不降，双副本启动并发播种安全——分配权威在 PG，M10）。
// K8s 侧句柄为 provider_ref（VMI 名）非 vmid，无撞号问题，故播种仅 PVE Provisioner 需要。
func (p *Provisioner) seedNextVMID(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	seed := p.cfg.VMIDBase
	if p.vmidSrc != nil {
		if maxVMID, err := p.vmidSrc.MaxVMID(ctx); err != nil {
			slog.Warn("seed nextVMID: PG MaxVMID query failed, skipping source", "err", err)
		} else if maxVMID+1 > seed {
			seed = maxVMID + 1
		}
	}
	if nextID, err := p.api.ClusterNextID(ctx); err != nil {
		slog.Warn("seed nextVMID: PVE cluster nextid query failed, skipping source", "err", err)
	} else if nextID > seed {
		seed = nextID
	}

	p.mu.Lock()
	p.nextVMID = seed
	p.mu.Unlock()
	if p.vmidSrc != nil {
		if err := p.vmidSrc.SeedVMID(ctx, seed); err != nil {
			slog.Warn("seed nextVMID: PG cursor seed failed", "floor", seed, "err", err)
		}
	}
	if seed != p.cfg.VMIDBase {
		slog.Info("nextVMID seeded above base", "next_vmid", seed, "vmid_base", p.cfg.VMIDBase)
	}
}

// resolveTemplate 解析模板入参：空用默认模板 VMID，非空须为数字 VMID 字符串。
func (p *Provisioner) resolveTemplate(template string) (int, error) {
	if template == "" {
		return p.cfg.TemplateVMID, nil
	}
	vmid, err := strconv.Atoi(template)
	if err != nil {
		return 0, fmt.Errorf("template must be a numeric VMID, got %q: %w", template, err)
	}
	return vmid, nil
}

// normalizeKind 归一环境类型，空/未知默认 ephemeral。
func normalizeKind(kind string) string {
	if kind == KindPersistent {
		return KindPersistent
	}
	return KindEphemeral
}

// ===== 内部：内存 env 表 =====

func (p *Provisioner) trackEnv(env *Environment) {
	p.mu.Lock()
	p.envs[env.ID] = env
	p.mu.Unlock()
}

func (p *Provisioner) untrackEnv(envID string) {
	p.mu.Lock()
	delete(p.envs, envID)
	p.mu.Unlock()
}

// lookupEnv 取环境：内存优先；未命中（如控制面重启后）且 store 存在则回读 store 恢复 vmid。
func (p *Provisioner) lookupEnv(ctx context.Context, envID string) (*Environment, error) {
	p.mu.Lock()
	env, ok := p.envs[envID]
	p.mu.Unlock()
	if ok {
		return env, nil
	}
	if p.store != nil {
		rec, err := p.store.GetEnvironment(ctx, envID)
		if err != nil {
			return nil, fmt.Errorf("environment %s not found: %w", envID, err)
		}
		// 回读补齐 Provider/ProviderRef（与 k8s.go lookupEnv 对齐）：控制面重启后从 store 恢复的
		// 环境须带 provider 句柄，否则跨重启的 provider-aware 消费方无从区分 PVE/K8s 承载。
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

// destroyQuietly 尽力回收（置备中途失败的清理），错误仅告警。用独立 ctx 不受调用方取消影响。
func (p *Provisioner) destroyQuietly(vmid int) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := p.api.Destroy(ctx, vmid); err != nil {
		slog.Warn("cleanup destroy failed", "vmid", vmid, "err", err)
	}
}

// ===== 内部：可选持久化（store 为 nil 时全部跳过，镜像 scheduler 审计写入模式）=====

func (p *Provisioner) persistCreate(env *Environment) {
	if p.store == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rec := store.EnvironmentRecord{
		ID:          env.ID,
		VMID:        int32(env.VMID),
		Kind:        env.Kind,
		NodeID:      env.NodeID,
		Status:      statusReady,
		Provider:    env.Provider,
		ProviderRef: env.ProviderRef,
	}
	if err := p.store.CreateEnvironment(ctx, rec); err != nil {
		slog.Warn("persist environment failed", "env_id", env.ID, "err", err)
	}
}

func (p *Provisioner) persistStatus(envID, status string) {
	if p.store == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := p.store.UpdateEnvironmentStatus(ctx, envID, status); err != nil {
		slog.Warn("update environment status failed", "env_id", envID, "status", status, "err", err)
	}
}

func (p *Provisioner) persistDelete(envID string) {
	if p.store == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := p.store.DeleteEnvironment(ctx, envID); err != nil {
		slog.Warn("delete environment failed", "env_id", envID, "err", err)
	}
}

// ===== go-proxmox 适配器（隔离第三方表面）=====
//
// NOTE: 以下 go-proxmox(luthermonson/go-proxmox) 调用签名待远程 `go build` 验证——
// 第三方库方法名/返回值可能与此处假设不同（Clone 返回值、VirtualMachineCloneOptions 字段、
// Task.Wait 签名、Snapshot/Rollback 方法名、Agent 探测方法、vm.Status 访问方式）。
// 编排层（Provisioner）不受影响，只需按编译错误修正本适配器。

// goProxmoxAPI 用 go-proxmox 实现 pveAPI。
type goProxmoxAPI struct {
	client *proxmox.Client
	node   string
}

// vm 取指定 vmid 的 VirtualMachine 句柄。
func (g *goProxmoxAPI) vm(ctx context.Context, vmid int) (*proxmox.VirtualMachine, error) {
	node, err := g.client.Node(ctx, g.node)
	if err != nil {
		return nil, fmt.Errorf("get pve node %s: %w", g.node, err)
	}
	vm, err := node.VirtualMachine(ctx, vmid)
	if err != nil {
		return nil, fmt.Errorf("get vm %d: %w", vmid, err)
	}
	return vm, nil
}

func (g *goProxmoxAPI) Clone(ctx context.Context, templateVMID, newVMID int, name string) error {
	tmpl, err := g.vm(ctx, templateVMID)
	if err != nil {
		return err
	}
	// Full=true：完整克隆（local-lvm/LVM-thin 下独立盘，非 linked-clone）。
	_, task, err := tmpl.Clone(ctx, &proxmox.VirtualMachineCloneOptions{
		NewID: newVMID,
		Name:  name,
		Full:  proxmox.IntOrBool(true),
	})
	if err != nil {
		return err
	}
	return waitTask(ctx, task)
}

func (g *goProxmoxAPI) Start(ctx context.Context, vmid int) error {
	vm, err := g.vm(ctx, vmid)
	if err != nil {
		return err
	}
	task, err := vm.Start(ctx)
	if err != nil {
		return err
	}
	return waitTask(ctx, task)
}

func (g *goProxmoxAPI) WaitAgent(ctx context.Context, vmid int, timeout time.Duration) error {
	vm, err := g.vm(ctx, vmid)
	if err != nil {
		return err
	}
	// 通用轮询探测 guest-agent（不依赖库特定的 WaitForAgent 便捷方法，降低 API 假设）：
	// agent 未起时接口调用报错，起来后成功。
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for {
		if _, err := vm.AgentGetNetworkIFaces(ctx); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if time.Now().After(deadline) {
				return fmt.Errorf("guest agent not ready on vm %d after %s", vmid, timeout)
			}
		}
	}
}

func (g *goProxmoxAPI) Snapshot(ctx context.Context, vmid int, name string) error {
	vm, err := g.vm(ctx, vmid)
	if err != nil {
		return err
	}
	task, err := vm.NewSnapshot(ctx, name)
	if err != nil {
		return err
	}
	return waitTask(ctx, task)
}

func (g *goProxmoxAPI) Rollback(ctx context.Context, vmid int, name string) error {
	vm, err := g.vm(ctx, vmid)
	if err != nil {
		return err
	}
	// v0.8.0：rollback 挂在快照对象上（vm.Snapshot 仅本地构造，不发请求）。
	task, err := vm.Snapshot(name).Rollback(ctx)
	if err != nil {
		return err
	}
	return waitTask(ctx, task)
}

func (g *goProxmoxAPI) Destroy(ctx context.Context, vmid int) error {
	vm, err := g.vm(ctx, vmid)
	if err != nil {
		return err
	}
	// 运行中先停机（已停则忽略），再删除。用字符串字面量而非库常量（避免常量名假设；
	// vm.Status 若为具名 string 类型，未定型字面量 "running" 会自动定型匹配）。
	if vm.Status == "running" {
		if stopTask, err := vm.Stop(ctx); err == nil {
			_ = waitTask(ctx, stopTask)
		}
	}
	// nil options = PVE 默认删除行为。
	task, err := vm.Delete(ctx, nil)
	if err != nil {
		return err
	}
	return waitTask(ctx, task)
}

func (g *goProxmoxAPI) Status(ctx context.Context, vmid int) (string, error) {
	vm, err := g.vm(ctx, vmid)
	if err != nil {
		return "", err
	}
	return vm.Status, nil
}

// ClusterNextID 查 PVE /cluster/nextid（go-proxmox v0.8.0：Client.Cluster(ctx).NextID(ctx)）。
func (g *goProxmoxAPI) ClusterNextID(ctx context.Context) (int, error) {
	cluster, err := g.client.Cluster(ctx)
	if err != nil {
		return 0, fmt.Errorf("get pve cluster: %w", err)
	}
	return cluster.NextID(ctx)
}

// waitTask 阻塞至 PVE task 完成或 ctx 取消。
// NOTE: go-proxmox Task.Wait(ctx, interval, timeout) 签名待验证。
func waitTask(ctx context.Context, task *proxmox.Task) error {
	if task == nil {
		return nil
	}
	return task.Wait(ctx, 2*time.Second, 10*time.Minute)
}
