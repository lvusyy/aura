//! 进程与文件能力子 trait：进程枚举/结束、文件推送/拉取、命令执行。

use async_trait::async_trait;

use crate::types::{Ack, CapError, CmdResult, FileResult, ProcessInfo};

/// 进程与文件能力。所有方法平台无关，由 aura-platform 以 sysinfo 等实现填充。
#[async_trait]
pub trait ProcessFileDriver: Send + Sync {
    /// 列出运行中的进程。
    async fn list_processes(&self) -> Result<Vec<ProcessInfo>, CapError>;

    /// 按 pid 结束进程。
    async fn kill_process(&self, pid: u32) -> Result<Ack, CapError>;

    /// 推送文件到节点（base64 内容）。
    async fn file_push(
        &self,
        remote_path: String,
        content_base64: String,
    ) -> Result<FileResult, CapError>;

    /// 从节点拉取文件（返回 base64 内容）。
    async fn file_pull(&self, remote_path: String) -> Result<FileResult, CapError>;

    /// 执行命令并等待结束（可选超时与工作目录）。
    async fn run_command(
        &self,
        cmd: String,
        args: Vec<String>,
        timeout_ms: Option<u64>,
        cwd: Option<String>,
    ) -> Result<CmdResult, CapError>;
}
