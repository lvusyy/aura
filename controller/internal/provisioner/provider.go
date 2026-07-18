package provisioner

import "context"

// provider 标识（写入 environments.provider 列 + aura_provision_duration_seconds 的 provider label）。
const (
	providerPVE = "pve"
	providerK8s = "k8s"
)

// EnvProvider 是环境编排的统一抽象：PVE 与 K8s 两 provider 满足同接口，每部署按环境变量
// 单选装配（AURA_K8S_KUBECONFIG→K8s / AURA_PVE_URL→PVE，同配拒启）。抽象层次=编排语义
// （create→就绪→reset→destroy），不泄漏底层 SDK（go-proxmox / client-go）表面（Locked-2/3）。
type EnvProvider interface {
	// CreateEnvironment 置备一个环境并等待就绪，返回内存视图；NodeID 置备时恒空
	// （always empty at provision time，节点启动后异步反连注册，经 auractl node list 关联）。
	// kind 空默认 ephemeral；template 空用 provider 默认（PVE=默认模板 VMID / K8s=默认 containerDisk 镜像）。
	CreateEnvironment(ctx context.Context, kind, template string) (*Environment, error)
	// DestroyEnvironment 销毁环境底层资源（PVE=停机+删 VM / K8s=删 VMI）并出表。
	DestroyEnvironment(ctx context.Context, id string) error
	// ResetEphemeral 将 ephemeral 环境复位到净初态（PVE=rollback 基线快照 /
	// K8s=删+按原 spec 重建 containerDisk）；非 ephemeral 拒绝。
	ResetEphemeral(ctx context.Context, id string) error
	// Status 返回环境底层实例运行态字符串。注意：词表是 provider-aware 的、语义不归一——
	// PVE 返回底层 VM 原始态（"running" | "stopped" | ...，qemu 视角）；K8s 返回 VMI 的
	// status.phase（"Pending" | "Scheduling" | "Running" | "Succeeded" | "Failed" | ...，KubeVirt 视角）。
	// 当前无跨 provider 消费方，故不做运行时归一映射（YAGNI，DD4）；调用方须按部署所选 provider 解读。
	Status(ctx context.Context, id string) (string, error)
}

// 编译期断言：两 provider 均满足 EnvProvider（任一 provider 方法签名漂移即编译失败）。
var (
	_ EnvProvider = (*Provisioner)(nil)
	_ EnvProvider = (*K8sProvisioner)(nil)
)
