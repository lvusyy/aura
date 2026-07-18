//! 屏幕能力子 trait：截图 / 区域放大 / 显示器枚举与切换。

use async_trait::async_trait;
use schemars::JsonSchema;
use serde::{Deserialize, Serialize};

use crate::types::{CapError, DisplayInfo, Region, ScreenshotResult};

/// 截图编码可选项（M11 REC-2 additive）：两字段均缺省 `None` 时行为与历史常量路径逐字节一致
/// （screenshot 走 q80/长边 1280，zoom 走 q80/不降采样），默认值单源保留在平台层 const。
/// 出域裁剪（clamp）由传输层 handler 在参数解析处统一实施，本层只消费已净化的值。
/// 定义于本文件（trait 同文件）而非 types.rs——与 TASK-003（改 types.rs）保持写集零交集。
#[derive(Debug, Clone, Copy, Default, Serialize, Deserialize, JsonSchema)]
pub struct ScreenshotOpts {
    /// WebP 有损编码质量（10–100）；缺省用平台默认 q80。
    #[serde(default)]
    pub quality: Option<f32>,
    /// 缩放长边上限（px）；缺省 screenshot=1280（XGA）、zoom=不降采样。
    #[serde(default)]
    pub max_dim: Option<u32>,
}

/// 屏幕能力。所有方法平台无关，由 aura-platform 以平台原生实现填充。
#[async_trait]
pub trait ScreenDriver: Send + Sync {
    /// 截取指定（或当前）显示器，返回缩放后的 WebP base64 + 元数据。
    async fn screenshot(
        &self,
        display: Option<u32>,
        opts: ScreenshotOpts,
    ) -> Result<ScreenshotResult, CapError>;

    /// 放大并截取指定区域（P0 契约 [x1,y1,x2,y2]）。
    async fn zoom(&self, region: Region, opts: ScreenshotOpts) -> Result<ScreenshotResult, CapError>;

    /// 列出所有显示器。
    async fn list_displays(&self) -> Result<Vec<DisplayInfo>, CapError>;

    /// 切换当前活动显示器，返回目标显示器信息。
    async fn switch_display(&self, display: u32) -> Result<DisplayInfo, CapError>;
}
