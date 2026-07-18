//! 无障碍（a11y）能力子 trait：读取目标窗口的结构化无障碍树。
//!
//! 无障碍树为语义化定位 / 断言提供确定性输入（相对基于像素/OCR 的脆弱定位）。
//! 平台无关：由 aura-platform 以 Windows UI Automation / Linux AT-SPI 实现，
//! macOS 明确 defer（返回 E_UNSUPPORTED，见平台层 `#[cfg]` 门控）。

use async_trait::async_trait;

use crate::types::{A11yParams, A11yTree, CapError};

/// 无障碍能力。默认浅层遍历 + depth/root/role/max_nodes 过滤 + 截断标志（Locked-6，防树爆）。
#[async_trait]
pub trait A11yDriver: Send + Sync {
    /// 读取目标窗口的无障碍树。入参控制子树根、遍历深度、角色过滤与节点上限；
    /// 命中 depth / max_nodes 上限时结果 `truncated=true`（树未完整返回）。
    async fn get_a11y_tree(&self, params: A11yParams) -> Result<A11yTree, CapError>;
}
