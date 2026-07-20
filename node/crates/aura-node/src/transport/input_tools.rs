//! input 域工具（7 个）+ input_router。body 委派 driver，TASK-005 填全实现细节。

use rmcp::handler::server::wrapper::Parameters;
use rmcp::{tool, tool_router, Json};
use schemars::JsonSchema;
use serde::Deserialize;

use aura_capability::{Ack, Envelope, MouseButton, ScrollDirection};

use super::AuraTools;

/// click 入参：P0 契约 coordinate:[x,y]，可选按键（缺省 left）。
#[derive(Debug, Deserialize, JsonSchema)]
pub struct ClickParams {
    /// 点击坐标 [x,y]，取自最近一次 screenshot 返回图上量取的像素（display 空间）。
    /// 节点按该截图的 scale 自动回映射到原生像素——直接用截图坐标，切勿手动乘 scale。
    pub coordinate: [i32; 2],
    /// 鼠标按键：left/right/middle，缺省 left。
    #[serde(default)]
    pub button: Option<String>,
}

/// type_text 入参。
#[derive(Debug, Deserialize, JsonSchema)]
pub struct TypeTextParams {
    /// 待输入文本（含 Unicode）。
    pub text: String,
}

/// key 入参：P0 契约单字符串（如 "ctrl+c"）。
#[derive(Debug, Deserialize, JsonSchema)]
pub struct KeyParams {
    /// 组合键字符串，如 "ctrl+c"、"enter"。
    pub keys: String,
}

/// scroll 入参：坐标 + 方向 + 步进量。
#[derive(Debug, Deserialize, JsonSchema)]
pub struct ScrollParams {
    /// 滚动锚点坐标 [x,y]，display 空间（取截图上量取的像素，节点自动回映射，勿手动乘 scale）。
    pub coordinate: [i32; 2],
    /// 方向：up/down/left/right，缺省 down。
    #[serde(default)]
    pub scroll_direction: Option<String>,
    /// 步进量（滚动格数），缺省 3。
    #[serde(default)]
    pub scroll_amount: Option<i32>,
}

/// drag 入参：起点 + 终点。
#[derive(Debug, Deserialize, JsonSchema)]
pub struct DragParams {
    /// 起点坐标 [x,y]，display 空间（取截图上量取的像素，节点自动回映射，勿手动乘 scale）。
    pub from: [i32; 2],
    /// 终点坐标 [x,y]，display 空间（取截图上量取的像素，节点自动回映射，勿手动乘 scale）。
    pub to: [i32; 2],
}

/// move_mouse 入参。
#[derive(Debug, Deserialize, JsonSchema)]
pub struct MoveMouseParams {
    /// 目标坐标 [x,y]，display 空间（取截图上量取的像素，节点自动回映射，勿手动乘 scale）。
    pub coordinate: [i32; 2],
}

/// wait 入参。
#[derive(Debug, Deserialize, JsonSchema)]
pub struct WaitParams {
    /// 等待毫秒。
    pub duration_ms: u64,
}

#[tool_router(router = input_router, vis = "pub(crate)")]
impl AuraTools {
    /// 在指定坐标点击鼠标。
    ///
    /// MCP 注解：destructiveHint=true（注入点击事件，改变目标环境）。
    /// coordinate 为 display 空间坐标，执行前经 to_native 按当前 scale 回映射到原生像素。
    #[tool(
        description = "Click the mouse at coordinate [x,y] in screenshot (display) pixel space. Use coordinates read directly off the latest screenshot; the node auto-maps them to native pixels — do not pre-multiply by scale.",
        annotations(destructive_hint = true)
    )]
    async fn click(&self, Parameters(p): Parameters<ClickParams>) -> Json<Envelope<Ack>> {
        let at = self.to_native(p.coordinate[0], p.coordinate[1]);
        let button = match p.button.as_deref() {
            Some("right") => MouseButton::Right,
            Some("middle") => MouseButton::Middle,
            _ => MouseButton::Left,
        };
        Json(self.guard(self.driver.click(at, button)).await)
    }

    /// 输入文本（含 Unicode）。
    ///
    /// MCP 注解：destructiveHint=true（注入键盘事件，改变目标环境）。
    /// 对外工具名对齐 computer use 动作 "type"（内部方法名 type_text 规避 Rust 关键字）。
    #[tool(
        name = "type",
        description = "Type text (including Unicode)",
        annotations(destructive_hint = true)
    )]
    async fn type_text(&self, Parameters(p): Parameters<TypeTextParams>) -> Json<Envelope<Ack>> {
        Json(self.guard(self.driver.type_text(p.text)).await)
    }

    /// 发送组合键（单字符串，如 "ctrl+c"）。
    ///
    /// MCP 注解：destructiveHint=true（注入键盘事件，改变目标环境）。
    #[tool(
        description = "Send a key combination as a single string, e.g. ctrl+c",
        annotations(destructive_hint = true)
    )]
    async fn key(&self, Parameters(p): Parameters<KeyParams>) -> Json<Envelope<Ack>> {
        Json(self.guard(self.driver.key(p.keys)).await)
    }

    /// 在指定坐标滚动。
    ///
    /// MCP 注解：destructiveHint=true（注入滚动事件，改变目标环境）。
    /// coordinate 为 display 空间锚点，执行前经 to_native 回映射到原生像素。
    #[tool(
        description = "Scroll at a coordinate (screenshot/display pixel space) with direction and amount. Coordinates come straight from the latest screenshot; the node maps them to native pixels.",
        annotations(destructive_hint = true)
    )]
    async fn scroll(&self, Parameters(p): Parameters<ScrollParams>) -> Json<Envelope<Ack>> {
        let at = self.to_native(p.coordinate[0], p.coordinate[1]);
        let direction = match p.scroll_direction.as_deref() {
            Some("up") => ScrollDirection::Up,
            Some("left") => ScrollDirection::Left,
            Some("right") => ScrollDirection::Right,
            _ => ScrollDirection::Down,
        };
        let amount = p.scroll_amount.unwrap_or(3);
        Json(self.guard(self.driver.scroll(at, direction, amount)).await)
    }

    /// 从起点拖拽到终点。
    ///
    /// MCP 注解：destructiveHint=true（注入拖拽事件，改变目标环境）。
    /// from/to 均为 display 空间坐标，执行前各自经 to_native 回映射到原生像素。
    #[tool(
        description = "Drag from one coordinate to another (screenshot/display pixel space). Coordinates come straight from the latest screenshot; the node maps them to native pixels.",
        annotations(destructive_hint = true)
    )]
    async fn drag(&self, Parameters(p): Parameters<DragParams>) -> Json<Envelope<Ack>> {
        let from = self.to_native(p.from[0], p.from[1]);
        let to = self.to_native(p.to[0], p.to[1]);
        Json(self.guard(self.driver.drag(from, to)).await)
    }

    /// 移动鼠标到指定坐标。
    ///
    /// MCP 注解：destructiveHint=true（移动光标，改变目标环境状态）。
    /// coordinate 为 display 空间坐标，执行前经 to_native 回映射到原生像素。
    #[tool(
        description = "Move the mouse to coordinate [x,y] in screenshot (display) pixel space. Coordinates come straight from the latest screenshot; the node maps them to native pixels.",
        annotations(destructive_hint = true)
    )]
    async fn move_mouse(
        &self,
        Parameters(p): Parameters<MoveMouseParams>,
    ) -> Json<Envelope<Ack>> {
        let to = self.to_native(p.coordinate[0], p.coordinate[1]);
        Json(self.guard(self.driver.move_mouse(to)).await)
    }

    /// 等待指定毫秒（纯延时，无坐标、无环境副作用）。
    #[tool(description = "Wait for a number of milliseconds")]
    async fn wait(&self, Parameters(p): Parameters<WaitParams>) -> Json<Envelope<Ack>> {
        Json(self.guard(self.driver.wait(p.duration_ms)).await)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// P0 契约：click 入参 coordinate 反序列化为 [x,y] 数组。
    #[test]
    fn click_params_deserialize_coordinate_array() {
        let p: ClickParams = serde_json::from_str(r#"{"coordinate":[10,20]}"#).unwrap();
        assert_eq!(p.coordinate, [10, 20]);
        assert!(p.button.is_none());

        let p: ClickParams =
            serde_json::from_str(r#"{"coordinate":[3,4],"button":"right"}"#).unwrap();
        assert_eq!(p.coordinate, [3, 4]);
        assert_eq!(p.button.as_deref(), Some("right"));
    }

    /// P0 契约：scroll 入参保留 scroll_direction + scroll_amount 语义。
    #[test]
    fn scroll_params_deserialize_direction_and_amount() {
        let p: ScrollParams = serde_json::from_str(
            r#"{"coordinate":[1,2],"scroll_direction":"up","scroll_amount":5}"#,
        )
        .unwrap();
        assert_eq!(p.coordinate, [1, 2]);
        assert_eq!(p.scroll_direction.as_deref(), Some("up"));
        assert_eq!(p.scroll_amount, Some(5));

        // 方向 / 步进量可缺省（body 内回退 down / 3）。
        let p: ScrollParams = serde_json::from_str(r#"{"coordinate":[0,0]}"#).unwrap();
        assert!(p.scroll_direction.is_none());
        assert!(p.scroll_amount.is_none());
    }

    /// drag 入参：起点 + 终点均为 [x,y] 数组。
    #[test]
    fn drag_params_deserialize_from_to_arrays() {
        let p: DragParams =
            serde_json::from_str(r#"{"from":[1,2],"to":[30,40]}"#).unwrap();
        assert_eq!(p.from, [1, 2]);
        assert_eq!(p.to, [30, 40]);
    }

    /// key 入参：P0 单字符串契约。
    #[test]
    fn key_params_deserialize_single_string() {
        let p: KeyParams = serde_json::from_str(r#"{"keys":"ctrl+c"}"#).unwrap();
        assert_eq!(p.keys, "ctrl+c");
    }
}
