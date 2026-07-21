//! 节点 self-update（M16，feature grpc 门控）。
//!
//! 收 `SelfUpdate` 帧 → staging 下载（presigned GET，http-only 同上传腿）→ sha256 强校验 →
//! `--version` sanity 探针（错平台/损坏制品在换刀前拦下）→ 原子换刀（先拷入安装目录再同目录
//! rename，规避跨文件系统 EXDEV；旧二进制留 `.old` 自愈位）→ 回 `SelfUpdateResult` → flush 录制 →
//! 重启：Unix `exec` 自替换（同 PID，不依赖 supervisor——覆盖 systemd/launchd/容器 PID1/裸 setsid
//! 全形态）；Windows 运行中 exe 不可覆盖但可 rename，detached PowerShell helper 等本进程退出后按
//! action 路径发现计划任务拉起（无任务则直接拉新二进制兜底）。
//!
//! 失败纪律：换刀前任何失败不动现网二进制（staging 自清）；换刀中途失败 rename 回滚 `.old`。
//! 回滚无专用机制：向旧版本 rollout 即回滚（同一通道）。

use std::ffi::OsString;
use std::net::IpAddr;
use std::path::{Path, PathBuf};
use std::process::Stdio;
use std::sync::atomic::{AtomicBool, Ordering};
use std::time::Duration;

use anyhow::{anyhow, bail, Context, Result};
use futures::channel::mpsc;
use futures::SinkExt;
use sha2::{Digest, Sha256};

use super::grpc_reverse::pb::{node_to_controller, NodeToController, SelfUpdate, SelfUpdateResult};
use super::AuraTools;

/// 重启前的出站帧冲刷等待：SelfUpdateResult 经 mpsc → writer → h2 需真实网络往返时间，
/// 立即重启会把结果帧闷死在本进程内（控制面只能兜底超时）。
const RESULT_FLUSH_DELAY: Duration = Duration::from_millis(1200);

/// staged 二进制 sanity 探针（`--version`）超时。
const SANITY_TIMEOUT: Duration = Duration::from_secs(15);

/// 启动早期捕获的自身安装身份（安装路径 + 原始参数）。换刀后 /proc/self/exe（及各平台等价物）
/// 指向被 rename 的 `.old` 文件——运行中再解析不可靠，必须用启动时捕获值。
#[derive(Clone)]
pub struct UpdateContext {
    pub exe: PathBuf,
    pub args: Vec<OsString>,
}

impl UpdateContext {
    /// 捕获当前进程安装路径与参数（反连 run() 启动时一次）。current_exe 失败（极端环境）返回
    /// None，self-update 收帧时诚实报错而非猜路径。
    pub fn capture() -> Option<Self> {
        std::env::current_exe().ok().map(|exe| Self {
            exe,
            args: std::env::args_os().skip(1).collect(),
        })
    }
}

/// per-process 单飞闸：同时至多一个 self-update 在途（控制面会话单槽之外的节点侧第二道闸，
/// 防重连后新流与旧在途任务并跑换刀）。
static IN_FLIGHT: AtomicBool = AtomicBool::new(false);

/// 处理一次 SelfUpdate 指令（grpc_reverse 收帧 spawn）：staging → 换刀 → 回结果帧 → 重启。
/// 失败路径回 ok=false + 原因并释放单飞闸；成活路径本函数不返回（exec / exit）。
pub async fn handle(
    su: SelfUpdate,
    ctx: Option<UpdateContext>,
    data_dir: PathBuf,
    local_addr: Option<IpAddr>,
    tools: AuraTools,
    mut resp_tx: mpsc::Sender<NodeToController>,
) {
    let version = su.version.clone();
    if IN_FLIGHT.swap(true, Ordering::SeqCst) {
        send_result(&mut resp_tx, &version, Err(anyhow!("self-update already in flight"))).await;
        return;
    }
    let Some(ctx) = ctx else {
        send_result(
            &mut resp_tx,
            &version,
            Err(anyhow!("cannot resolve own executable path (current_exe failed at startup)")),
        )
        .await;
        IN_FLIGHT.store(false, Ordering::SeqCst);
        return;
    };

    let staged = match stage(&su, &ctx, &data_dir, local_addr).await {
        Ok(p) => p,
        Err(e) => {
            tracing::warn!(version = %version, error = %format!("{:#}", e), "self-update staging failed; current binary untouched");
            send_result(&mut resp_tx, &version, Err(e)).await;
            IN_FLIGHT.store(false, Ordering::SeqCst);
            return;
        }
    };
    if let Err(e) = swap(&ctx.exe, &staged) {
        tracing::warn!(version = %version, error = %format!("{:#}", e), "self-update swap failed");
        send_result(&mut resp_tx, &version, Err(e)).await;
        IN_FLIGHT.store(false, Ordering::SeqCst);
        return;
    }

    tracing::info!(version = %version, exe = %ctx.exe.display(), "self-update swapped; reporting and restarting");
    send_result(&mut resp_tx, &version, Ok(())).await;
    tokio::time::sleep(RESULT_FLUSH_DELAY).await;
    // 进行中录制逐个 stop finalize（沿 T1.4 优雅退出同一收尾面）；in-flight 工具请求随重启中断，
    // rollout 应选空闲窗执行（文档约定，不做节点侧排空——KISS）。
    tools.flush_recordings_on_shutdown().await;
    restart(&ctx);
}

/// 下载 + 校验 + sanity + 预放置。返回已就位于安装目录内、可与安装路径原子 rename 的新二进制路径。
/// 任何失败不动现网二进制（下载产物自清）。
async fn stage(
    su: &SelfUpdate,
    ctx: &UpdateContext,
    data_dir: &Path,
    local_addr: Option<IpAddr>,
) -> Result<PathBuf> {
    if su.url.is_empty() || su.sha256.is_empty() {
        bail!("self-update frame missing url/sha256");
    }
    let dl = data_dir.join("self_update").join("aura-node.download");
    let size = super::upload::get_file(&su.url, &dl, local_addr)
        .await
        .context("download release artifact")?;
    if su.size > 0 && size != su.size as u64 {
        let _ = tokio::fs::remove_file(&dl).await;
        bail!("size mismatch: downloaded {size} bytes, release registered {}", su.size);
    }

    // sha256 强校验（spawn_blocking：数十 MB 哈希不占 async worker）。
    let want = su.sha256.to_ascii_lowercase();
    let hash_path = dl.clone();
    let got = tokio::task::spawn_blocking(move || sha256_file(&hash_path))
        .await
        .context("join hash task")??;
    if got != want {
        let _ = tokio::fs::remove_file(&dl).await;
        bail!("sha256 mismatch: downloaded {got}, release registered {want}");
    }

    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        tokio::fs::set_permissions(&dl, std::fs::Permissions::from_mode(0o755))
            .await
            .context("chmod staged binary")?;
    }
    if let Err(e) = sanity_check(&dl).await {
        let _ = tokio::fs::remove_file(&dl).await;
        return Err(e);
    }

    // 预放置到安装目录内：staging（data_dir）与安装路径常不同文件系统（容器挂载/独立分区），跨 fs
    // rename 会 EXDEV——先 copy 到安装目录隐藏名，换刀只做同目录 rename（原子）。copy 在 Unix 连带
    // 权限位（staged 已 0755）。此步失败最常见原因：安装目录对节点用户不可写（root 属主 /usr/local/bin
    // 旧布局 / 容器 readOnly hostPath）——诚实上报，现网二进制未动。
    let new_in_place = ctx.exe.with_file_name(".aura-node.new");
    tokio::fs::copy(&dl, &new_in_place).await.with_context(|| {
        format!(
            "place new binary beside install path {} (binary dir not writable by node user? readOnly mount?)",
            ctx.exe.display()
        )
    })?;
    let _ = tokio::fs::remove_file(&dl).await;
    Ok(new_in_place)
}

/// sha256 文件内容（hex 小写）。
fn sha256_file(path: &Path) -> Result<String> {
    let bytes = std::fs::read(path).with_context(|| format!("read staged file {}", path.display()))?;
    let mut hasher = Sha256::new();
    hasher.update(&bytes);
    Ok(format!("{:x}", hasher.finalize()))
}

/// staged sanity 探针：跑 `<staged> --version`（超时封顶）。错平台制品（exec format error）、
/// 截断/损坏二进制在换刀前拦下。不比对版本号输出——同版本 sha 修复推送是合法场景，能起动即够。
async fn sanity_check(bin: &Path) -> Result<()> {
    let mut cmd = tokio::process::Command::new(bin);
    cmd.arg("--version")
        .stdin(Stdio::null())
        .stdout(Stdio::null())
        .stderr(Stdio::null());
    #[cfg(windows)]
    {
        use std::os::windows::process::CommandExt;
        cmd.creation_flags(0x0800_0000); // CREATE_NO_WINDOW：探针不闪窗
    }
    let mut child = cmd
        .spawn()
        .context("spawn staged binary for sanity check (wrong-platform artifact?)")?;
    match tokio::time::timeout(SANITY_TIMEOUT, child.wait()).await {
        Ok(Ok(status)) if status.success() => Ok(()),
        Ok(Ok(status)) => bail!("staged binary sanity check exited with {status}"),
        Ok(Err(e)) => Err(e).context("wait staged binary sanity check"),
        Err(_) => {
            let _ = child.kill().await;
            bail!("staged binary sanity check timed out after {}s", SANITY_TIMEOUT.as_secs());
        }
    }
}

/// 原子换刀：install → install.old（自愈/取证位）→ new → install。stage 已保证同目录（同文件系统），
/// 两步均为 rename；第二步失败即把 `.old` rename 回来（不砖机）。
/// Windows 合法性依据：运行中 exe 锁写/删但允许 rename（NTFS）；Unix 运行中二进制 ETXTBSY 拒写但
/// rename 自由——两平台同一舞步。
fn swap(exe: &Path, new_in_place: &Path) -> Result<()> {
    let old = old_path(exe);
    let _ = std::fs::remove_file(&old); // 清上轮 .old（已无进程映射，可删；失败则下步 rename 自会暴露）
    std::fs::rename(exe, &old)
        .with_context(|| format!("rename running binary aside ({} -> {})", exe.display(), old.display()))?;
    if let Err(e) = std::fs::rename(new_in_place, exe) {
        // 回滚：老二进制放回原位（尽力而为；回滚失败一并上报——此时须人工介入，.old 仍在）。
        let restored = std::fs::rename(&old, exe);
        return Err(anyhow!(
            "place new binary at {} failed: {e} (rollback of .old: {})",
            exe.display(),
            match restored {
                Ok(()) => "ok".to_string(),
                Err(re) => format!("FAILED: {re}"),
            }
        ));
    }
    Ok(())
}

/// `.old` 自愈位路径：安装名整体追加后缀（`aura-node.exe` → `aura-node.exe.old`；不用 with_extension
/// ——那会吃掉 `.exe`）。
fn old_path(exe: &Path) -> PathBuf {
    let mut name = exe.file_name().unwrap_or_default().to_os_string();
    name.push(".old");
    exe.with_file_name(name)
}

/// 换刀后重启（不返回）。
/// Unix：`exec` 自替换——同 PID 原地换像，systemd MainPID 连续无重启事件、容器 PID1 与裸 setsid
/// 进程同样成立（不依赖任何 supervisor）；exec 仅失败才返回，此时退出交 supervisor 拉起兜底。
/// Windows：无 exec，detached PowerShell helper 等本进程退出后拉起（见 spawn_windows_relauncher）。
fn restart(ctx: &UpdateContext) -> ! {
    #[cfg(unix)]
    {
        use std::os::unix::process::CommandExt;
        tracing::info!(exe = %ctx.exe.display(), "exec-restarting into updated binary");
        let err = std::process::Command::new(&ctx.exe).args(&ctx.args).exec();
        tracing::error!(error = %err, "exec restart failed; exiting for supervisor pickup");
        std::process::exit(1);
    }
    #[cfg(windows)]
    {
        spawn_windows_relauncher(ctx);
        std::process::exit(0);
    }
}

/// Windows 重启 helper：detached PowerShell（-EncodedCommand 绕多层引号转义）——
/// ① Wait-Process 等本进程真正退出（rename 舞步已完成，退出即释放任务槽）；
/// ② 按 action 可执行路径匹配发现承载本节点的计划任务（生产任务名不可知——AuraNode/AuraNodeE2E
///    等历史名并存，按路径发现零配置），Start-ScheduledTask 拉起（保持 supervisor 所有权，避免
///    孤儿进程与开机双实例）；
/// ③ 无匹配任务（手工/开发形态）Start-Process 直接拉新二进制兜底（原参数逐字重放）。
#[cfg(windows)]
fn spawn_windows_relauncher(ctx: &UpdateContext) {
    use base64::Engine as _;
    use std::os::windows::process::CommandExt;

    let pid = std::process::id();
    let exe = ps_quote(&ctx.exe.to_string_lossy());
    let args_list = ctx
        .args
        .iter()
        .map(|a| ps_quote(&a.to_string_lossy()))
        .collect::<Vec<_>>()
        .join(", ");
    let fallback = if args_list.is_empty() {
        format!("Start-Process -FilePath {exe}")
    } else {
        format!("Start-Process -FilePath {exe} -ArgumentList @({args_list})")
    };
    let script = format!(
        "$ErrorActionPreference='SilentlyContinue'; \
         Wait-Process -Id {pid} -Timeout 120; Start-Sleep -Seconds 1; \
         $exe = {exe}; \
         $t = Get-ScheduledTask | Where-Object {{ $_.Actions | Where-Object {{ $_.Execute -and ($_.Execute.Trim('\"') -ieq $exe) }} }} | Select-Object -First 1; \
         if ($t) {{ Start-ScheduledTask -TaskName $t.TaskName -TaskPath $t.TaskPath }} else {{ {fallback} }}"
    );
    let utf16: Vec<u8> = script.encode_utf16().flat_map(|u| u.to_le_bytes()).collect();
    let encoded = base64::engine::general_purpose::STANDARD.encode(utf16);

    const CREATE_NO_WINDOW: u32 = 0x0800_0000;
    const DETACHED_PROCESS: u32 = 0x0000_0008;
    match std::process::Command::new("powershell")
        .args(["-NoProfile", "-NonInteractive", "-EncodedCommand", &encoded])
        .creation_flags(CREATE_NO_WINDOW | DETACHED_PROCESS)
        .stdin(Stdio::null())
        .stdout(Stdio::null())
        .stderr(Stdio::null())
        .spawn()
    {
        Ok(_) => tracing::info!("windows relauncher spawned; exiting for scheduled-task pickup"),
        Err(e) => {
            tracing::error!(error = %e, "spawn windows relauncher failed; node stays down until supervisor/manual restart")
        }
    }
}

/// PowerShell 单引号字面量转义（'' 表示 '，service.rs 同款纪律）。
#[cfg(windows)]
fn ps_quote(s: &str) -> String {
    format!("'{}'", s.replace('\'', "''"))
}

/// 回发 SelfUpdateResult 帧（best-effort：出站已断仅告警——控制面兜底超时独立生效，两案纵深）。
async fn send_result(tx: &mut mpsc::Sender<NodeToController>, version: &str, res: Result<()>) {
    let (ok, error) = match res {
        Ok(()) => (true, String::new()),
        Err(e) => (false, format!("{:#}", e)),
    };
    let frame = NodeToController {
        payload: Some(node_to_controller::Payload::SelfUpdateResult(SelfUpdateResult {
            version: version.to_string(),
            ok,
            error,
        })),
    };
    if tx.send(frame).await.is_err() {
        tracing::warn!("self-update result frame dropped: outbound stream closed");
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// `.old` 路径整体追加后缀：Windows `.exe` 不被吃掉（with_extension 陷阱回归）。
    #[test]
    fn old_path_appends_suffix() {
        assert_eq!(
            old_path(Path::new("/opt/aura/bin/aura-node")),
            Path::new("/opt/aura/bin/aura-node.old")
        );
        assert_eq!(
            old_path(Path::new(r"C:\aura\aura-node.exe")),
            Path::new(r"C:\aura\aura-node.exe.old")
        );
    }

    /// 换刀舞步（用普通文件模拟二进制；Unix 下运行中二进制 rename 合法性由真机金丝雀验证）：
    /// 新文件就位、旧文件成 .old。
    #[test]
    fn swap_places_new_and_keeps_old() {
        let dir = std::env::temp_dir().join(format!("aura-swap-test-{}", std::process::id()));
        std::fs::create_dir_all(&dir).unwrap();
        let exe = dir.join("aura-node");
        let new = dir.join(".aura-node.new");
        std::fs::write(&exe, b"old-binary").unwrap();
        std::fs::write(&new, b"new-binary").unwrap();

        swap(&exe, &new).unwrap();

        assert_eq!(std::fs::read(&exe).unwrap(), b"new-binary");
        assert_eq!(std::fs::read(old_path(&exe)).unwrap(), b"old-binary");
        assert!(!new.exists());
        let _ = std::fs::remove_dir_all(&dir);
    }

    /// 换刀第二步失败（new 缺失）时回滚：原二进制回到原位，不砖机。
    #[test]
    fn swap_rolls_back_when_new_missing() {
        let dir = std::env::temp_dir().join(format!("aura-swap-rb-test-{}", std::process::id()));
        std::fs::create_dir_all(&dir).unwrap();
        let exe = dir.join("aura-node");
        std::fs::write(&exe, b"old-binary").unwrap();

        let err = swap(&exe, &dir.join(".aura-node.new")).unwrap_err();
        assert!(err.to_string().contains("rollback of .old: ok"), "err={err}");
        assert_eq!(std::fs::read(&exe).unwrap(), b"old-binary");
        let _ = std::fs::remove_dir_all(&dir);
    }
}
