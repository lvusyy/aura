//! 输入能力子 trait：鼠标点击/移动/拖拽/滚动、键盘输入、组合键、等待。

use async_trait::async_trait;

use crate::types::{Ack, CapError, Coordinate, MouseButton, ScrollDirection};

/// 输入能力。所有方法平台无关，由 aura-platform 以 enigo 等实现填充。
#[async_trait]
pub trait InputDriver: Send + Sync {
    /// 在指定坐标按下鼠标按键。
    async fn click(&self, at: Coordinate, button: MouseButton) -> Result<Ack, CapError>;

    /// 输入文本（含 Unicode）。
    async fn type_text(&self, text: String) -> Result<Ack, CapError>;

    /// 发送组合键，P0 契约单字符串（如 "ctrl+c"）。
    async fn key(&self, keys: String) -> Result<Ack, CapError>;

    /// 在指定坐标滚动（方向 + 步进量）。
    async fn scroll(
        &self,
        at: Coordinate,
        direction: ScrollDirection,
        amount: i32,
    ) -> Result<Ack, CapError>;

    /// 从起点拖拽到终点。
    async fn drag(&self, from: Coordinate, to: Coordinate) -> Result<Ack, CapError>;

    /// 移动鼠标到指定坐标。
    async fn move_mouse(&self, to: Coordinate) -> Result<Ack, CapError>;

    /// 等待指定毫秒。
    async fn wait(&self, duration_ms: u64) -> Result<Ack, CapError>;
}
