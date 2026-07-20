//! 节点服务生命周期管理子命令（M16 T1.5）：跨平台探测 supervisor + 查状态 / 重启。
//!
//! 边界（DRY）：**不重复** install.sh / install.ps1 的安装逻辑——unit/plist/schtasks 的生成与
//! 已固化教训（显式 --data-dir、禁 After=graphical.target、chown 凭据、TCC 提示等）归安装脚本单一源。
//! 本模块只做安装脚本**不覆盖**的两件运维刚需：
//!   - `status`：探测本机 supervisor 形态（systemd/launchd/schtasks）+ 报告服务活跃态——
//!     供 self-update 换二进制后验证「新版本起来了吗」，及运维统一诊断入口。
//!   - `restart`：跨平台统一重启入口（systemctl restart / launchctl kickstart -k / schtasks end+run），
//!     免运维记各平台命令。注：self-update 常规路径靠「优雅退出（T1.4）+ supervisor 自动拉起」即可，
//!     本命令是显式运维/兜底入口。
//!
//! 平台命令经 std::process::Command 外壳调用，`#[cfg(target_os)]` 门控集中于本模块。

use anyhow::{anyhow, Result};
use std::process::Command;

/// `aura-node service` CLI 参数（clap Args）。
#[derive(clap::Args, Clone, Debug)]
pub struct ServiceArgs {
    #[command(subcommand)]
    action: ServiceAction,
}

/// service 子动作。
#[derive(clap::Subcommand, Clone, Debug)]
enum ServiceAction {
    /// 查服务状态（探测 supervisor 形态 + 活跃态）。找到且活跃退出 0，否则非 0（供脚本/self-update 判定）。
    Status(ServiceTarget),
    /// 重启服务（跨平台统一入口）。成功退出 0。
    Restart(ServiceTarget),
}

/// 服务标识：默认名平台派生（Linux aura-node / macOS com.aura.node / Windows AuraNode），--name 覆盖。
/// 生产 Windows 计划任务名可能不同（如 AuraNodeE2E），须以 --name 显式指定。
#[derive(clap::Args, Clone, Debug)]
pub struct ServiceTarget {
    /// 服务/单元/任务名；缺省按平台派生。
    #[arg(long)]
    name: Option<String>,
}

impl ServiceTarget {
    /// 平台默认服务名。
    fn resolved(&self) -> String {
        self.name.clone().unwrap_or_else(|| default_service_name().to_string())
    }
}

/// 平台默认服务名（与 install.sh/install.ps1 装载名对齐）。
fn default_service_name() -> &'static str {
    #[cfg(target_os = "linux")]
    {
        "aura-node"
    }
    #[cfg(target_os = "macos")]
    {
        "com.aura.node"
    }
    #[cfg(target_os = "windows")]
    {
        "AuraNode"
    }
    #[cfg(not(any(target_os = "linux", target_os = "macos", target_os = "windows")))]
    {
        "aura-node"
    }
}

/// service 子命令入口（一次性，跑完即退；不启动 driver/反连服务面）。
pub fn run(args: ServiceArgs) -> Result<()> {
    let (action_kind, target) = match &args.action {
        ServiceAction::Status(t) => ("status", t),
        ServiceAction::Restart(t) => ("restart", t),
    };
    let name = target.resolved();
    // 本二进制版本供对照（self-update 后 status 可核对预期版本）。
    let self_version = env!("CARGO_PKG_VERSION");

    match action_kind {
        "status" => {
            let report = status(&name)?;
            // 状态是命令查询结果，走 stdout（本命令非 stdio MCP 传输，无洁净流约束）。
            println!("service: {name}");
            println!("supervisor: {}", report.supervisor);
            println!("active: {}", report.active);
            if let Some(detail) = &report.detail {
                println!("detail: {detail}");
            }
            println!("aura-node binary version (this cli): {self_version}");
            if report.active {
                Ok(())
            } else {
                // 未活跃/未找到 → 非 0 退出，供 self-update/脚本判定失败。
                Err(anyhow!("service {name} not active (supervisor={})", report.supervisor))
            }
        }
        _ => restart(&name),
    }
}

/// 服务状态报告。
struct StatusReport {
    /// supervisor 形态：systemd / launchd / schtasks / unknown。
    supervisor: &'static str,
    /// 是否活跃（运行中）。
    active: bool,
    /// 附加细节（单元路径 / 任务状态行等，best-effort）。
    detail: Option<String>,
}

// ===== Linux：systemd（systemctl）=====
#[cfg(target_os = "linux")]
fn status(name: &str) -> Result<StatusReport> {
    // system 与 user 两 scope 独立探测，**任一 active 即报 active**——不能让 inactive 的 system unit
    // 遮蔽 active 的 user unit（生产为 system unit，但两 scope 同名并存时须都判）。
    // systemctl_is_active 返回 Some(true/false)=该 scope 已知（active/非active），None=该 scope 不存在。
    let system = systemctl_is_active(name, false);
    let user = systemctl_is_active(name, true);
    let (active, scope, is_user) = if system == Some(true) {
        (true, "system", false)
    } else if user == Some(true) {
        (true, "user", true)
    } else if system.is_some() {
        (false, "system", false) // system 存在但非 active（user 亦非 active 或不存在）
    } else if user.is_some() {
        (false, "user", true)
    } else {
        return Ok(StatusReport {
            supervisor: "systemd",
            active: false,
            detail: Some(format!("unit {name} not found (system or --user)")),
        });
    };
    // 取 FragmentPath 佐证（best-effort）。
    let frag = systemctl_show(name, is_user, "FragmentPath");
    Ok(StatusReport {
        supervisor: "systemd",
        active,
        detail: Some(format!("scope={scope}{}", frag.map(|f| format!(" unit={f}")).unwrap_or_default())),
    })
}

/// systemctl is-active：返回 Some(true/false) 表命中该 scope（active/inactive），None 表单元不存在于该 scope。
#[cfg(target_os = "linux")]
fn systemctl_is_active(name: &str, user: bool) -> Option<bool> {
    let mut cmd = Command::new("systemctl");
    if user {
        cmd.arg("--user");
    }
    cmd.args(["is-active", name]);
    let out = cmd.output().ok()?;
    let s = String::from_utf8_lossy(&out.stdout);
    match s.trim() {
        "active" => Some(true),
        // inactive/failed/activating/deactivating 均表单元已知但非稳态 active。
        "inactive" | "failed" | "activating" | "deactivating" => Some(false),
        // "unknown" 或空 → 单元不存在于该 scope。
        _ => None,
    }
}

/// systemctl show -p <prop> --value：取单元属性（best-effort，失败返 None）。
#[cfg(target_os = "linux")]
fn systemctl_show(name: &str, user: bool, prop: &str) -> Option<String> {
    let mut cmd = Command::new("systemctl");
    if user {
        cmd.arg("--user");
    }
    cmd.args(["show", "-p", prop, "--value", name]);
    let out = cmd.output().ok()?;
    let v = String::from_utf8_lossy(&out.stdout).trim().to_string();
    if v.is_empty() {
        None
    } else {
        Some(v)
    }
}

#[cfg(target_os = "linux")]
fn restart(name: &str) -> Result<()> {
    // 优先 system（生产形态），失败回退 --user。system 服务重启需 root（非 root 由 systemctl 报错透传）。
    if run_ok(Command::new("systemctl").args(["restart", name])) {
        println!("restarted {name} (systemd system)");
        return Ok(());
    }
    if run_ok(Command::new("systemctl").args(["--user", "restart", name])) {
        println!("restarted {name} (systemd --user)");
        return Ok(());
    }
    Err(anyhow!(
        "systemctl restart {name} failed (system and --user); need root for system unit?"
    ))
}

// ===== macOS：launchd（launchctl）=====
#[cfg(target_os = "macos")]
fn status(name: &str) -> Result<StatusReport> {
    let uid = unsafe { libc_getuid() };
    let target = format!("gui/{uid}/{name}");
    let out = Command::new("launchctl").args(["print", &target]).output();
    match out {
        Ok(o) if o.status.success() => {
            let text = String::from_utf8_lossy(&o.stdout);
            // launchctl print 输出含 "state = running"；pid 存在亦表活跃。
            let active = text.contains("state = running") || text.contains("pid =");
            Ok(StatusReport {
                supervisor: "launchd",
                active,
                detail: Some(format!("label={name} domain=gui/{uid}")),
            })
        }
        _ => Ok(StatusReport {
            supervisor: "launchd",
            active: false,
            detail: Some(format!("service {name} not loaded in gui/{uid}")),
        }),
    }
}

/// getuid 直取（避免引入依赖；launchd domain 需当前用户 uid）。
#[cfg(target_os = "macos")]
extern "C" {
    #[link_name = "getuid"]
    fn libc_getuid() -> u32;
}

#[cfg(target_os = "macos")]
fn restart(name: &str) -> Result<()> {
    let uid = unsafe { libc_getuid() };
    let target = format!("gui/{uid}/{name}");
    // kickstart -k：强制重启（先杀再拉），launchd 管理服务的标准重启姿势。
    if run_ok(Command::new("launchctl").args(["kickstart", "-k", &target])) {
        println!("restarted {name} (launchd {target})");
        Ok(())
    } else {
        Err(anyhow!("launchctl kickstart -k {target} failed (service loaded?)"))
    }
}

// ===== Windows：计划任务（schtasks / PowerShell）=====
#[cfg(target_os = "windows")]
fn status(name: &str) -> Result<StatusReport> {
    // 用 PowerShell 取 ScheduledTask.State 枚举名（Running/Ready/Disabled）——.NET 枚举 ToString **不随
    // 系统语言本地化**。schtasks /fo list 的 Status 文本会本地化（中文系统输出「正在运行」），
    // 若 text.contains("Running") 判活跃则中文 Windows（生产形态）恒误判 inactive。State=Running 表
    // 任务正在执行=活跃。-TaskName 收裸名（不带路径反斜杠）跨所有 TaskPath 匹配。
    let bare = name.trim_start_matches('\\');
    let ps = format!(
        "$ErrorActionPreference='SilentlyContinue'; (Get-ScheduledTask -TaskName '{}').State",
        ps_single_quote_escape(bare)
    );
    let out = Command::new("powershell")
        .args(["-NoProfile", "-NonInteractive", "-Command", &ps])
        .output();
    match out {
        Ok(o) => {
            let state = String::from_utf8_lossy(&o.stdout).trim().to_string();
            if state.is_empty() {
                return Ok(StatusReport {
                    supervisor: "schtasks",
                    active: false,
                    detail: Some(format!("task {bare} not found")),
                });
            }
            Ok(StatusReport {
                supervisor: "schtasks",
                active: state.eq_ignore_ascii_case("Running"),
                detail: Some(format!("task={bare} state={state}")),
            })
        }
        Err(e) => Ok(StatusReport {
            supervisor: "schtasks",
            active: false,
            detail: Some(format!("powershell query failed: {e}")),
        }),
    }
}

/// PowerShell 单引号字符串内转义单引号（'' 表一个字面单引号）；防 --name 含引号破坏命令。
#[cfg(target_os = "windows")]
fn ps_single_quote_escape(s: &str) -> String {
    s.replace('\'', "''")
}

/// schtasks 任务名规范化：补前导反斜杠（根命名空间任务，schtasks /tn 需要）。
#[cfg(target_os = "windows")]
fn normalize_task_name(name: &str) -> String {
    if name.starts_with('\\') {
        name.to_string()
    } else {
        format!("\\{name}")
    }
}

#[cfg(target_os = "windows")]
fn restart(name: &str) -> Result<()> {
    let task = normalize_task_name(name);
    // schtasks 无原子 restart：先 /end 停再 /run 起（与 install.ps1 滚更姿势一致）。
    let _ = Command::new("schtasks").args(["/end", "/tn", &task]).output();
    if run_ok(Command::new("schtasks").args(["/run", "/tn", &task])) {
        println!("restarted {name} (schtasks {task})");
        Ok(())
    } else {
        Err(anyhow!("schtasks /run /tn {task} failed (task exists?)"))
    }
}

// ===== 其余平台：不支持 =====
#[cfg(not(any(target_os = "linux", target_os = "macos", target_os = "windows")))]
fn status(_name: &str) -> Result<StatusReport> {
    Ok(StatusReport {
        supervisor: "unknown",
        active: false,
        detail: Some("service management unsupported on this platform".to_string()),
    })
}

#[cfg(not(any(target_os = "linux", target_os = "macos", target_os = "windows")))]
fn restart(_name: &str) -> Result<()> {
    Err(anyhow!("service restart unsupported on this platform"))
}

/// 执行命令并判成功（退出码 0）；命令不存在/非 0 均返 false。stderr 透传至本进程 stderr 供诊断。
#[cfg(any(target_os = "linux", target_os = "macos", target_os = "windows"))]
fn run_ok(cmd: &mut Command) -> bool {
    match cmd.status() {
        Ok(s) => s.success(),
        Err(_) => false,
    }
}
