//! stdio 传输：把工具集 serve 到 stdin/stdout，阻塞至连接关闭。

use anyhow::Result;
use rmcp::transport::stdio;
use rmcp::ServiceExt;

use super::AuraTools;

/// 以 stdio 传输启动 MCP 服务（stdout 专供 JSON-RPC，日志已在 main 定向 stderr）。
pub async fn serve(tools: AuraTools) -> Result<()> {
    let running = tools.serve(stdio()).await?;
    running.waiting().await?;
    Ok(())
}
