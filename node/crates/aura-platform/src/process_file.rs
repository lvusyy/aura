//! ProcessFileDriver 平台实现（TASK-006）。
//!
//! 进程枚举/结束用 sysinfo，文件推送/拉取用 tokio::fs + base64 + sha2 完整性校验，
//! 命令执行用 tokio::process 并以 tokio::time::timeout 控制超时。三类能力均平台无关，
//! 无需 `#[cfg(target_os)]` 门控（sysinfo/std/tokio 各自封装了平台差异）。

use std::fmt::Write as _;
use std::process::Stdio;
use std::time::Duration;

use async_trait::async_trait;
use base64::engine::general_purpose::STANDARD;
use base64::Engine as _;
use sha2::{Digest, Sha256};
use sysinfo::{Pid, ProcessesToUpdate, System};

use aura_capability::{Ack, CapError, CmdResult, FileResult, ProcessFileDriver, ProcessInfo};

use crate::PlatformDriver;

/// run_command 默认超时（毫秒）：30 秒，可由入参 timeout_ms 覆盖。
const DEFAULT_TIMEOUT_MS: u64 = 30_000;

/// file_pull 单文件大小上限（字节）：10 MiB。
/// 超限返回 InvalidArg，避免大文件经 base64 撑爆 MCP 响应体（大产物走 resource_link 是 M3 事项）。
const MAX_PULL_BYTES: u64 = 10 * 1024 * 1024;

/// 计算字节内容的 SHA-256 十六进制摘要（小写）。
fn sha256_hex(data: &[u8]) -> String {
    let mut hasher = Sha256::new();
    hasher.update(data);
    let digest = hasher.finalize();
    let mut hex = String::with_capacity(digest.len() * 2);
    for b in digest {
        // 定长两位小写十六进制，避免额外引入 hex crate
        let _ = write!(hex, "{:02x}", b);
    }
    hex
}

#[async_trait]
impl ProcessFileDriver for PlatformDriver {
    async fn list_processes(&self) -> Result<Vec<ProcessInfo>, CapError> {
        // sysinfo 枚举为阻塞系统调用，且每进程 CPU 占用需两次刷新求差值，
        // 整体放入 blocking 线程执行，避免阻塞异步执行器。
        let processes = tokio::task::spawn_blocking(|| {
            let mut sys = System::new_all();
            // CPU 占用 = 两次进程刷新之间的增量：首次采样（new_all）后等待最小间隔再刷新一次。
            std::thread::sleep(sysinfo::MINIMUM_CPU_UPDATE_INTERVAL);
            sys.refresh_processes(ProcessesToUpdate::All, true);
            sys.processes()
                .values()
                .map(|p| ProcessInfo {
                    pid: p.pid().as_u32(),
                    name: p.name().to_string_lossy().into_owned(),
                    cpu: p.cpu_usage(),
                    memory_bytes: p.memory(),
                })
                .collect::<Vec<_>>()
        })
        .await
        .map_err(|e| CapError::ProcessError(format!("process enumeration task join failed: {e}")))?;
        Ok(processes)
    }

    async fn kill_process(&self, pid: u32) -> Result<Ack, CapError> {
        // 仅刷新目标 pid 再结束，最小化系统扫描开销；同样放入 blocking 线程。
        let killed = tokio::task::spawn_blocking(move || {
            let target = Pid::from_u32(pid);
            let mut sys = System::new();
            sys.refresh_processes(ProcessesToUpdate::Some(&[target]), false);
            match sys.process(target) {
                Some(proc) => Ok(proc.kill()),
                None => Err(CapError::InvalidArg(format!("no such process: pid {pid}"))),
            }
        })
        .await
        .map_err(|e| CapError::ProcessError(format!("kill task join failed: {e}")))??;

        if killed {
            Ok(Ack::ok())
        } else {
            Err(CapError::ProcessError(format!(
                "failed to signal process pid {pid}"
            )))
        }
    }

    async fn file_push(
        &self,
        remote_path: String,
        content_base64: String,
    ) -> Result<FileResult, CapError> {
        // 无共享文件系统：内容经 MCP 以 base64 传入，节点侧解码后落盘。
        let bytes = STANDARD
            .decode(content_base64.as_bytes())
            .map_err(|e| CapError::InvalidArg(format!("invalid base64 content: {e}")))?;
        let expected = sha256_hex(&bytes);

        tokio::fs::write(&remote_path, &bytes)
            .await
            .map_err(|e| CapError::FileError(format!("write {remote_path} failed: {e}")))?;

        // sha256 完整性校验：读回落盘内容，摘要须与写入内容一致，防止静默写坏。
        let readback = tokio::fs::read(&remote_path)
            .await
            .map_err(|e| CapError::FileError(format!("verify read {remote_path} failed: {e}")))?;
        let actual = sha256_hex(&readback);
        if actual != expected {
            return Err(CapError::FileError(format!(
                "write integrity check failed for {remote_path}: sha256 {actual} != {expected}"
            )));
        }

        // push 语义：写入侧不回带内容，content_base64 置 None。
        Ok(FileResult {
            path: remote_path,
            size_bytes: bytes.len() as u64,
            content_base64: None,
        })
    }

    async fn file_pull(&self, remote_path: String) -> Result<FileResult, CapError> {
        // 先取元数据做大小上限校验，避免读取超大文件。
        let meta = tokio::fs::metadata(&remote_path)
            .await
            .map_err(|e| CapError::FileError(format!("stat {remote_path} failed: {e}")))?;
        if meta.len() > MAX_PULL_BYTES {
            return Err(CapError::InvalidArg(format!(
                "file too large to pull: {} bytes exceeds {MAX_PULL_BYTES} limit",
                meta.len()
            )));
        }

        let bytes = tokio::fs::read(&remote_path)
            .await
            .map_err(|e| CapError::FileError(format!("read {remote_path} failed: {e}")))?;

        // pull 语义：内容经 base64 回带，客户端可自行按 sha256 校验往返一致。
        let content = STANDARD.encode(&bytes);
        Ok(FileResult {
            path: remote_path,
            size_bytes: bytes.len() as u64,
            content_base64: Some(content),
        })
    }

    async fn run_command(
        &self,
        cmd: String,
        args: Vec<String>,
        timeout_ms: Option<u64>,
        cwd: Option<String>,
        detach: bool,
    ) -> Result<CmdResult, CapError> {
        if detach {
            // detach 语义：仅启动不等待，供拉起 GUI 应用等常驻程序——等待语义下超时
            // 会经 kill_on_drop 连被测窗口一起结束，UI 自动化闭环无法开场。
            // 输出定向 null：piped 无人消费时管道写满会阻塞子进程。
            let mut command = tokio::process::Command::new(&cmd);
            command
                .args(&args)
                .stdin(Stdio::null())
                .stdout(Stdio::null())
                .stderr(Stdio::null());
            // 独立进程组：不随节点进程组信号连带退出（Windows 无进程组概念，天然独立）。
            #[cfg(unix)]
            command.process_group(0);
            if let Some(dir) = cwd.as_ref() {
                command.current_dir(dir);
            }
            let child = command
                .spawn()
                .map_err(|e| CapError::ProcessError(format!("spawn '{cmd}' failed: {e}")))?;
            let pid = child.id().unwrap_or(0);
            // 不持有句柄：子进程退出后由 tokio 后台 reap，不留 zombie。
            return Ok(CmdResult {
                exit_code: 0,
                stdout: format!("detached pid={pid}"),
                stderr: String::new(),
            });
        }

        let mut command = tokio::process::Command::new(&cmd);
        command
            .args(&args)
            .stdout(Stdio::piped())
            .stderr(Stdio::piped())
            // 超时触发时 timeout 会 drop 掉 wait future 进而 drop 子进程句柄，
            // kill_on_drop 保证此时向子进程发出结束信号，避免留下孤儿进程。
            .kill_on_drop(true);
        if let Some(dir) = cwd.as_ref() {
            command.current_dir(dir);
        }

        let child = command
            .spawn()
            .map_err(|e| CapError::ProcessError(format!("spawn '{cmd}' failed: {e}")))?;

        let timeout = Duration::from_millis(timeout_ms.unwrap_or(DEFAULT_TIMEOUT_MS));
        let output = match tokio::time::timeout(timeout, child.wait_with_output()).await {
            Ok(Ok(out)) => out,
            Ok(Err(e)) => {
                return Err(CapError::ProcessError(format!(
                    "command '{cmd}' failed to complete: {e}"
                )))
            }
            Err(_elapsed) => {
                // 超时：子进程经 kill_on_drop 被结束，返回进程错误告知调用方。
                return Err(CapError::ProcessError(format!(
                    "command '{cmd}' timed out after {} ms",
                    timeout.as_millis()
                )));
            }
        };

        Ok(CmdResult {
            // 信号终止时 code() 为 None，统一以 -1 表示无正常退出码。
            exit_code: output.status.code().unwrap_or(-1),
            stdout: String::from_utf8_lossy(&output.stdout).into_owned(),
            stderr: String::from_utf8_lossy(&output.stderr).into_owned(),
        })
    }
}

#[cfg(test)]
mod tests {
    // `use super::*` 已带入父模块的 STANDARD 与 `Engine as _`（glob 传递匿名导入），
    // encode/decode 方法解析即可用，无需在此重复引入 Engine。
    use super::*;

    // run_command 跨平台命令：Windows 走 cmd /C，其余走 shell。
    #[cfg(windows)]
    fn echo_cmd() -> (String, Vec<String>) {
        (
            "cmd".to_string(),
            vec!["/C".to_string(), "echo".to_string(), "hello".to_string()],
        )
    }
    #[cfg(not(windows))]
    fn echo_cmd() -> (String, Vec<String>) {
        ("echo".to_string(), vec!["hello".to_string()])
    }

    #[cfg(windows)]
    fn exit3_cmd() -> (String, Vec<String>) {
        // 分词传参，规避 cmd.exe 对含空格参数的引号处理差异。
        (
            "cmd".to_string(),
            vec!["/C".to_string(), "exit".to_string(), "3".to_string()],
        )
    }
    #[cfg(not(windows))]
    fn exit3_cmd() -> (String, Vec<String>) {
        (
            "sh".to_string(),
            vec!["-c".to_string(), "exit 3".to_string()],
        )
    }

    // 长睡命令：用于超时用例。
    #[cfg(windows)]
    fn sleep_cmd() -> (String, Vec<String>) {
        (
            "ping".to_string(),
            vec![
                "127.0.0.1".to_string(),
                "-n".to_string(),
                "5".to_string(),
            ],
        )
    }
    #[cfg(not(windows))]
    fn sleep_cmd() -> (String, Vec<String>) {
        ("sleep".to_string(), vec!["5".to_string()])
    }

    #[tokio::test]
    async fn run_command_echo_returns_stdout() {
        let driver = PlatformDriver::new();
        let (cmd, args) = echo_cmd();
        let res = driver
            .run_command(cmd, args, Some(5000), None, false)
            .await
            .unwrap();
        assert_eq!(res.exit_code, 0);
        assert!(res.stdout.contains("hello"), "stdout was: {:?}", res.stdout);
    }

    #[tokio::test]
    async fn run_command_reports_nonzero_exit() {
        let driver = PlatformDriver::new();
        let (cmd, args) = exit3_cmd();
        let res = driver
            .run_command(cmd, args, Some(5000), None, false)
            .await
            .unwrap();
        assert_eq!(res.exit_code, 3);
    }

    #[tokio::test]
    async fn run_command_times_out() {
        let driver = PlatformDriver::new();
        let (cmd, args) = sleep_cmd();
        let err = driver
            .run_command(cmd, args, Some(200), None, false)
            .await
            .unwrap_err();
        assert_eq!(err.code(), "E_PROCESS_FAILED");
        assert!(
            err.to_string().contains("timed out"),
            "err was: {err}"
        );
    }

    #[tokio::test]
    async fn run_command_detach_returns_immediately() {
        let driver = PlatformDriver::new();
        // 长睡命令 + 200ms 超时：detach 语义须立即返回且不受 timeout_ms 影响
        //（等待语义下同参数是超时错误）；子进程数秒后自行退出，不留残留。
        let (cmd, args) = sleep_cmd();
        let started = std::time::Instant::now();
        let res = driver
            .run_command(cmd, args, Some(200), None, true)
            .await
            .unwrap();
        assert_eq!(res.exit_code, 0);
        assert!(
            res.stdout.contains("detached pid="),
            "stdout was: {:?}",
            res.stdout
        );
        assert!(
            started.elapsed() < Duration::from_secs(2),
            "detach must not wait for child exit"
        );
    }

    #[tokio::test]
    async fn file_push_pull_sha256_roundtrip() {
        let driver = PlatformDriver::new();
        let path = std::env::temp_dir().join(format!("aura_task006_{}.bin", std::process::id()));
        let path_str = path.to_string_lossy().into_owned();
        let content: &[u8] = b"AURA file transfer round-trip \x00\x01\x02 payload";
        let b64 = STANDARD.encode(content);

        let push = driver
            .file_push(path_str.clone(), b64)
            .await
            .unwrap();
        assert_eq!(push.size_bytes, content.len() as u64);
        assert!(push.content_base64.is_none());

        let pull = driver.file_pull(path_str.clone()).await.unwrap();
        assert_eq!(pull.size_bytes, content.len() as u64);
        let pulled = STANDARD
            .decode(pull.content_base64.expect("pull carries content"))
            .unwrap();
        assert_eq!(pulled.as_slice(), content);
        // 往返内容 sha256 一致。
        assert_eq!(sha256_hex(content), sha256_hex(&pulled));

        let _ = std::fs::remove_file(&path);
    }

    #[tokio::test]
    async fn file_pull_rejects_missing() {
        let driver = PlatformDriver::new();
        let missing = std::env::temp_dir()
            .join("aura_task006_does_not_exist.bin")
            .to_string_lossy()
            .into_owned();
        let err = driver.file_pull(missing).await.unwrap_err();
        assert_eq!(err.code(), "E_FILE_FAILED");
    }

    #[tokio::test]
    async fn list_processes_enumerates_current() {
        let driver = PlatformDriver::new();
        let procs = driver.list_processes().await.unwrap();
        assert!(!procs.is_empty(), "process list should not be empty");
        // 当前测试进程必在枚举结果中。
        let me = std::process::id();
        assert!(
            procs.iter().any(|p| p.pid == me),
            "current pid {me} not found in process list"
        );
    }
}
