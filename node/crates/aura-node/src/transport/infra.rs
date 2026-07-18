//! 基础设施形态探测（M12 批D）：runtime_kind（k8s | container | vm | baremetal）+ infra_host（宿主链）。
//!
//! 宿主事实采集，与 DeviceDriver 无关（attached 才是驱动派生，见 aura-capability）；grpc_reverse::run()
//! 启动时一次探测跨重连复用（同 os_version 批B 惯例——形态/宿主稳定，免每次重连重付子进程开销）。
//! 探测纪律（同 os_version）：**宁可兜底勿错报**——kind 判定链天然收敛 baremetal 兜底；infra_host
//! 采不到即空串（前端不显），绝不猜测。
//!
//! Linux 判定序（先内后外，k8s pod 里 systemd-detect-virt 亦报 container 故 k8s 必须先判）：
//!   1. k8s：env KUBERNETES_SERVICE_HOST 或 serviceaccount 目录存在（pod 内默认注入）；
//!   2. container：/.dockerenv 存在，或 /proc/1/cgroup 含 docker/containerd/libpod/kubepods；
//!   3. vm：systemd-detect-virt --vm 输出非 none（回落 DMI product_name 含虚拟化特征串）；
//!   4. 兜底 baremetal。
//! Windows：注册表 SystemProductName（`reg query`，零新依赖）含 Virtual/VMware/QEMU/KVM → vm，
//!   否则/失败 baremetal。macOS：baremetal（虚拟化 mac 非目标形态，YAGNI）。
//!
//! infra_host 斜杠链编码 `<host>[/<ns>/<pod>]`（proto Register.infra_host 契约，console parse 展示
//! 所属链）：k8s = downward API env AURA_K8S_NODE（manifest fieldRef spec.nodeName 注入）+ ns
//! （serviceaccount namespace 文件）+ pod 名（env AURA_K8S_POD，manifest fieldRef metadata.name——
//! **hostNetwork pod 的 hostname 是宿主名不可当 pod 名**，故必须 downward API）；vm/baremetal =
//! 自身 hostname；container = env AURA_INFRA_HOST 兜底（无从自探，空则空）。

/// serviceaccount 挂载目录（k8s pod 内默认投影；ns 文件在其下）。
const K8S_SA_DIR: &str = "/var/run/secrets/kubernetes.io/serviceaccount";

/// 探测结果：运行形态 + 宿主链。
pub struct InfraFacts {
    /// k8s | container | vm | baremetal（恒非空，判定链兜底 baremetal）。
    pub runtime_kind: String,
    /// 宿主链 `<host>[/<ns>/<pod>]`；采不到为空串（前端不显）。
    pub infra_host: String,
}

/// 探测入口：按平台分派，返回形态 + 宿主链。`host` 为调用方已采集的宿主 hostname（grpc_reverse 的
/// node_hostname()，gethostname 随 grpc feature 门控——本模块以入参消费之，自身零 optional 依赖，
/// 免 feature 门控、裸 `cargo test --workspace` 全覆盖单测）。
pub fn detect(host: &str) -> InfraFacts {
    #[cfg(target_os = "linux")]
    {
        detect_linux(host)
    }
    #[cfg(target_os = "windows")]
    {
        detect_windows(host)
    }
    #[cfg(not(any(target_os = "linux", target_os = "windows")))]
    {
        // macOS 及其他：裸机 + 自身 hostname（虚拟化 mac 非目标形态，YAGNI；接入时补探测）。
        InfraFacts {
            runtime_kind: "baremetal".to_string(),
            infra_host: host.to_string(),
        }
    }
}

// ===== Linux =====

#[cfg(target_os = "linux")]
fn detect_linux(host: &str) -> InfraFacts {
    // 1) k8s pod：serviceaccount 投影 / KUBERNETES_SERVICE_HOST env 皆是 pod 默认注入（hostNetwork 亦有）。
    if std::env::var("KUBERNETES_SERVICE_HOST").map(|v| !v.is_empty()).unwrap_or(false)
        || std::path::Path::new(K8S_SA_DIR).exists()
    {
        return InfraFacts {
            runtime_kind: "k8s".to_string(),
            infra_host: k8s_host_chain(),
        };
    }
    // 2) 非 k8s 容器：/.dockerenv（docker）或 /proc/1/cgroup 运行时特征。
    if std::path::Path::new("/.dockerenv").exists() || cgroup_says_container() {
        return InfraFacts {
            runtime_kind: "container".to_string(),
            // 容器内无从探宿主：env AURA_INFRA_HOST 兜底（部署面注入），空则空（不猜测）。
            infra_host: std::env::var("AURA_INFRA_HOST").unwrap_or_default(),
        };
    }
    // 3) vm：systemd-detect-virt 权威（none=物理机），命令缺失回落 DMI。
    if linux_is_vm() {
        return InfraFacts {
            runtime_kind: "vm".to_string(),
            infra_host: host.to_string(),
        };
    }
    // 4) 兜底裸机。
    InfraFacts {
        runtime_kind: "baremetal".to_string(),
        infra_host: host.to_string(),
    }
}

/// /proc/1/cgroup 是否携容器运行时特征（docker/containerd/libpod/kubepods）。
#[cfg(target_os = "linux")]
fn cgroup_says_container() -> bool {
    match std::fs::read_to_string("/proc/1/cgroup") {
        Ok(c) => cgroup_content_says_container(&c),
        Err(_) => false,
    }
}

/// 纯函数（单测缝）：cgroup 文本 → 是否容器。cgroup v2 统一层级下容器常为 "0::/"（无特征），
/// 故本谓词只作正向证据（命中=容器），不命中不代表非容器——上游判定链的 /.dockerenv 先例互补。
#[cfg(target_os = "linux")]
fn cgroup_content_says_container(content: &str) -> bool {
    ["docker", "containerd", "libpod", "kubepods"]
        .iter()
        .any(|kw| content.contains(kw))
}

/// systemd-detect-virt --vm：stdout 非 "none" 即虚拟机（kvm/qemu/vmware/microsoft/oracle/xen…）。
/// 命令缺失/失败回落 DMI 特征串——product_name 与 sys_vendor **并读**：QEMU Q35 机型 product_name
/// 是 "Standard PC (Q35 + ICH9, 2009)" 不含虚拟化字样，vendor 才是 "QEMU"（单读 product 会漏判）。
#[cfg(target_os = "linux")]
fn linux_is_vm() -> bool {
    if let Ok(out) = std::process::Command::new("systemd-detect-virt").arg("--vm").output() {
        // 退出码 0 = 检出虚拟化；非 0 时 stdout 为 "none"（物理机），两面一致取 stdout 判。
        let v = String::from_utf8_lossy(&out.stdout).trim().to_string();
        if !v.is_empty() {
            return v != "none";
        }
    }
    let mut dmi = String::new();
    for f in ["/sys/class/dmi/id/product_name", "/sys/class/dmi/id/sys_vendor"] {
        if let Ok(s) = std::fs::read_to_string(f) {
            dmi.push_str(&s);
            dmi.push('\n');
        }
    }
    if dmi.is_empty() {
        return false; // DMI 也读不到：不猜测，交兜底 baremetal
    }
    dmi_product_says_vm(&dmi)
}

/// 纯函数（单测缝）：DMI product_name → 是否虚拟机特征。
#[cfg(target_os = "linux")]
fn dmi_product_says_vm(product: &str) -> bool {
    let p = product.to_ascii_lowercase();
    ["qemu", "kvm", "vmware", "virtual", "xen", "bochs", "parallels"]
        .iter()
        .any(|kw| p.contains(kw))
}

/// k8s 宿主链 `<node>[/<ns>/<pod>]`：node 名取 downward API env AURA_K8S_NODE（manifest fieldRef
/// spec.nodeName，缺失回落 env AURA_INFRA_HOST，再缺失空）；ns 读 serviceaccount namespace 文件；
/// pod 名取 env AURA_K8S_POD（fieldRef metadata.name）——hostNetwork pod 的 hostname=宿主名（批C
/// 实证 fleet 两卡同显宿主名），绝不可回落 hostname 充当 pod 名。ns/pod 齐备才拼链尾（半截不拼）。
#[cfg(target_os = "linux")]
fn k8s_host_chain() -> String {
    let node = std::env::var("AURA_K8S_NODE")
        .or_else(|_| std::env::var("AURA_INFRA_HOST"))
        .unwrap_or_default();
    let ns = std::fs::read_to_string(format!("{K8S_SA_DIR}/namespace"))
        .map(|s| s.trim().to_string())
        .unwrap_or_default();
    let pod = std::env::var("AURA_K8S_POD").unwrap_or_default();
    join_host_chain(&node, &ns, &pod)
}

/// 纯函数（单测缝）：宿主链拼装。ns 与 pod 齐备才拼 `/<ns>/<pod>` 尾段；node 空而 ns/pod 在
/// （downward API 未注入的旧 manifest）时链头留空段不可取——此时仅回 ns/pod 也无定位价值，
/// 统一：node 空则整链空（宁空勿半截误导）。
#[cfg(any(target_os = "linux", test))]
fn join_host_chain(node: &str, ns: &str, pod: &str) -> String {
    if node.is_empty() {
        return String::new();
    }
    if !ns.is_empty() && !pod.is_empty() {
        format!("{node}/{ns}/{pod}")
    } else {
        node.to_string()
    }
}

// ===== Windows =====

/// Windows 形态：注册表 SystemProductName（`reg query`，wmic 已 deprecated、零新依赖）含虚拟化
/// 特征串 → vm，否则/查询失败 → baremetal（探测失败宁兜底勿错报）。infra_host=自身 hostname。
#[cfg(target_os = "windows")]
fn detect_windows(host: &str) -> InfraFacts {
    let kind = if windows_is_vm() { "vm" } else { "baremetal" };
    InfraFacts {
        runtime_kind: kind.to_string(),
        infra_host: host.to_string(),
    }
}

#[cfg(target_os = "windows")]
fn windows_is_vm() -> bool {
    let out = match std::process::Command::new("reg")
        .args([
            "query",
            r"HKLM\HARDWARE\DESCRIPTION\System\BIOS",
            "/v",
            "SystemProductName",
        ])
        .output()
    {
        Ok(o) => o,
        Err(_) => return false,
    };
    win_product_says_vm(&String::from_utf8_lossy(&out.stdout))
}

/// 纯函数（单测缝）：reg query 输出文本 → 是否虚拟机特征（Virtual/VMware/QEMU/KVM/Xen/Hyper-V 关键词，
/// 大小写无关；ASCII 关键词不受 OEM 码页 mojibake 影响，同批B ver parse 语言无关纪律）。
#[cfg(any(target_os = "windows", test))]
fn win_product_says_vm(reg_output: &str) -> bool {
    let p = reg_output.to_ascii_lowercase();
    ["virtual", "vmware", "qemu", "kvm", "xen", "hyper-v"]
        .iter()
        .any(|kw| p.contains(kw))
}

#[cfg(test)]
mod tests {
    use super::*;

    /// detect() 恒返回四枚举之一（判定链兜底不变量，任何环境下成立）。
    #[test]
    fn detect_kind_is_always_enumerated() {
        let f = detect("test-host");
        assert!(
            matches!(f.runtime_kind.as_str(), "k8s" | "container" | "vm" | "baremetal"),
            "unexpected kind: {}",
            f.runtime_kind
        );
    }

    /// 宿主链拼装：node 空整链空（宁空勿半截）；ns/pod 齐备才拼尾段。
    #[test]
    fn host_chain_join_rules() {
        assert_eq!(join_host_chain("", "aura", "pod-1"), "");
        assert_eq!(join_host_chain("n1", "", "pod-1"), "n1");
        assert_eq!(join_host_chain("n1", "aura", ""), "n1");
        assert_eq!(join_host_chain("n1", "aura", "pod-1"), "n1/aura/pod-1");
    }

    /// Windows 产品名判定（语言无关：ASCII 关键词 + 大小写折叠；QEMU VM 实测格式样例）。
    #[test]
    fn win_product_detection() {
        assert!(win_product_says_vm("SystemProductName    REG_SZ    QEMU Standard PC (i440FX)"));
        assert!(win_product_says_vm("SystemProductName    REG_SZ    VMware Virtual Platform"));
        assert!(win_product_says_vm("SystemProductName    REG_SZ    Virtual Machine")); // Hyper-V
        assert!(!win_product_says_vm("SystemProductName    REG_SZ    MS-7D70")); // 物理主板型号
        assert!(!win_product_says_vm("")); // 查询失败兜底非 vm
    }

    /// Linux cgroup / DMI 纯函数判定。
    #[cfg(target_os = "linux")]
    #[test]
    fn linux_evidence_detection() {
        assert!(cgroup_content_says_container("12:pids:/docker/abc123"));
        assert!(cgroup_content_says_container("0::/system.slice/containerd.service/kubepods-pod1"));
        assert!(!cgroup_content_says_container("0::/init.scope"));
        assert!(dmi_product_says_vm("Standard PC (Q35 + ICH9, 2009)\n") == false); // QEMU Q35 无 qemu 字样→靠 detect-virt 主判
        assert!(dmi_product_says_vm("QEMU Virtual Machine"));
        assert!(dmi_product_says_vm("VMware7,1"));
        assert!(!dmi_product_says_vm("20Y7S00T00")); // ThinkPad 型号
    }
}
