//! a11y 域工具（1 个：get_a11y_tree）+ a11y_router。body 委派 driver（TASK-005）。
//!
//! 入参直接复用 capability 层 [`A11yParams`]（其自带 JsonSchema 派生），MCP 工具 schema 由该类型
//! 字段文档单一生成，与能力层同源；gRPC 反连侧经 `tool_dispatch` 注册表派发同一执行核，
//! MCP/gRPC 两侧工具集由集合相等断言强制同步（漏配即编译期测试失败）。

use rmcp::handler::server::wrapper::Parameters;
use rmcp::{tool, tool_router, Json};

use aura_capability::{A11yParams, A11yTree, Envelope};

use super::AuraTools;

#[tool_router(router = a11y_router, vis = "pub(crate)")]
impl AuraTools {
    /// 读取目标窗口的结构化无障碍树（默认浅层 + depth/root/role/max_nodes 过滤 + 截断标志）。
    ///
    /// MCP 注解：readOnlyHint=true（纯观察，无副作用）。为语义化定位 / 断言提供确定性输入。
    #[tool(
        description = "Read the structured accessibility tree of the target window (shallow by default; supports depth/root/role/max_nodes filters and returns a truncated flag)",
        annotations(read_only_hint = true)
    )]
    async fn get_a11y_tree(
        &self,
        Parameters(p): Parameters<A11yParams>,
    ) -> Json<Envelope<A11yTree>> {
        Json(self.guard(self.driver.get_a11y_tree(p)).await)
    }
}
