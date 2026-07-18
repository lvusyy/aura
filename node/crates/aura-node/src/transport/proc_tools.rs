//! process/file/command 域工具（5 个）+ proc_router。body 委派 driver（TASK-006 填能力实现）。
//!
//! MCP 注解经 rmcp `#[tool(annotations(...))]` 实际 wiring 到 `tools/list`：
//! 只读观察类标 read_only_hint；命令 / 结束进程 / 写文件等高危类标 destructive_hint，
//! 其中 kill_process / run_command 另在 `_meta` 注入 anthropic/requiresUserInteraction=true
//! （即便 bypass 模式也强制人工确认，RBAC 埋点，M2 补细粒度 scope）。

use rmcp::handler::server::wrapper::Parameters;
use rmcp::{tool, tool_router, Json};
use schemars::JsonSchema;
use serde::Deserialize;

use aura_capability::{Ack, CmdResult, Envelope, FileResult, ProcessInfo};

use super::AuraTools;

/// kill_process 入参。
#[derive(Debug, Deserialize, JsonSchema)]
pub struct KillProcessParams {
    /// 目标进程 pid。
    pub pid: u32,
}

/// file_push 入参。
#[derive(Debug, Deserialize, JsonSchema)]
pub struct FilePushParams {
    /// 节点侧目标路径。
    pub remote_path: String,
    /// base64 编码的文件内容。
    pub content_base64: String,
}

/// file_pull 入参。
#[derive(Debug, Deserialize, JsonSchema)]
pub struct FilePullParams {
    /// 节点侧源路径。
    pub remote_path: String,
}

/// run_command 入参。
#[derive(Debug, Deserialize, JsonSchema)]
pub struct RunCommandParams {
    /// 可执行命令。
    pub cmd: String,
    /// 命令参数。
    #[serde(default)]
    pub args: Vec<String>,
    /// 超时毫秒（可选）。
    #[serde(default)]
    pub timeout_ms: Option<u64>,
    /// 工作目录（可选）。
    #[serde(default)]
    pub cwd: Option<String>,
}

#[tool_router(router = proc_router, vis = "pub(crate)")]
impl AuraTools {
    /// 列出运行中的进程。
    ///
    /// MCP 注解：readOnlyHint=true（只读观察，无副作用）。
    #[tool(
        description = "List running processes",
        annotations(read_only_hint = true)
    )]
    async fn list_processes(&self) -> Json<Envelope<Vec<ProcessInfo>>> {
        Json(self.guard(self.driver.list_processes()).await)
    }

    /// 按 pid 结束进程。
    ///
    /// MCP 注解：destructiveHint=true 且 _meta.requiresUserInteraction=true
    /// （高危操作，误杀关键进程风险；强制人工确认，M2 补 RBAC scope）。
    #[tool(
        description = "Kill a process by pid",
        annotations(destructive_hint = true),
        meta = crate::transport::requires_user_interaction_meta()
    )]
    async fn kill_process(
        &self,
        Parameters(p): Parameters<KillProcessParams>,
    ) -> Json<Envelope<Ack>> {
        Json(self.guard(self.driver.kill_process(p.pid)).await)
    }

    /// 推送文件到节点（base64 内容）。
    ///
    /// MCP 注解：destructiveHint=true（写节点文件系统，可覆盖既有文件）。
    #[tool(
        description = "Push a file to the node (base64 content)",
        annotations(destructive_hint = true)
    )]
    async fn file_push(
        &self,
        Parameters(p): Parameters<FilePushParams>,
    ) -> Json<Envelope<FileResult>> {
        Json(
            self.guard(self.driver.file_push(p.remote_path, p.content_base64))
                .await,
        )
    }

    /// 从节点拉取文件（base64 内容）。只读取节点文件内容，不修改环境
    /// （M1 未标 read-only 提示，与 file_push 成对，语义收口后续统一；见 summary）。
    #[tool(description = "Pull a file from the node (base64 content)")]
    async fn file_pull(
        &self,
        Parameters(p): Parameters<FilePullParams>,
    ) -> Json<Envelope<FileResult>> {
        Json(self.guard(self.driver.file_pull(p.remote_path)).await)
    }

    /// 执行命令并等待结束。
    ///
    /// MCP 注解：destructiveHint=true 且 _meta.requiresUserInteraction=true
    /// （可执行任意命令；强制人工确认，run_command 需显式授权 scope，M2 补 RBAC）。
    #[tool(
        description = "Run a command and wait for it to finish",
        annotations(destructive_hint = true),
        meta = crate::transport::requires_user_interaction_meta()
    )]
    async fn run_command(
        &self,
        Parameters(p): Parameters<RunCommandParams>,
    ) -> Json<Envelope<CmdResult>> {
        Json(
            self.guard(self.driver.run_command(p.cmd, p.args, p.timeout_ms, p.cwd))
                .await,
        )
    }
}
