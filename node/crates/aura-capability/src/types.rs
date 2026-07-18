//! 共享领域类型：坐标 / 区域 / 截图元数据 / 显示器 / 进程 / 命令 / 文件结果，及统一错误枚举。
//! 这些类型平台无关、传输无关，供能力层 trait 与上层适配共用。
//! 坐标序列化为 [x,y] 由上层入参负责，能力层内部一律用具名字段。

use schemars::JsonSchema;
use serde::{Deserialize, Serialize};

/// 屏幕坐标（原生像素）。
#[derive(Debug, Clone, Copy, Serialize, Deserialize, JsonSchema, PartialEq, Eq)]
pub struct Coordinate {
    pub x: i32,
    pub y: i32,
}

/// 屏幕区域，P0 契约：左上角 (x1,y1) + 右下角 (x2,y2)（像素）。
#[derive(Debug, Clone, Copy, Serialize, Deserialize, JsonSchema, PartialEq, Eq)]
pub struct Region {
    pub x1: i32,
    pub y1: i32,
    pub x2: i32,
    pub y2: i32,
}

/// 鼠标按键。
#[derive(Debug, Clone, Copy, Serialize, Deserialize, JsonSchema, PartialEq, Eq)]
#[serde(rename_all = "lowercase")]
pub enum MouseButton {
    Left,
    Right,
    Middle,
}

/// 滚动方向。
#[derive(Debug, Clone, Copy, Serialize, Deserialize, JsonSchema, PartialEq, Eq)]
#[serde(rename_all = "lowercase")]
pub enum ScrollDirection {
    Up,
    Down,
    Left,
    Right,
}

/// 截图缩放元数据：原生尺寸、显示（缩放后）尺寸、缩放系数。
/// 模型坐标按 scale 回映射到原生像素后执行。
#[derive(Debug, Clone, Copy, Serialize, Deserialize, JsonSchema)]
pub struct ScreenshotMeta {
    pub native_w: u32,
    pub native_h: u32,
    pub display_w: u32,
    pub display_h: u32,
    pub scale: f64,
}

/// 显示器信息。
#[derive(Debug, Clone, Serialize, Deserialize, JsonSchema)]
pub struct DisplayInfo {
    pub id: u32,
    pub name: String,
    pub x: i32,
    pub y: i32,
    pub width: u32,
    pub height: u32,
    pub is_primary: bool,
    pub scale: f64,
}

/// 进程信息。
#[derive(Debug, Clone, Serialize, Deserialize, JsonSchema)]
pub struct ProcessInfo {
    pub pid: u32,
    pub name: String,
    pub cpu: f32,
    pub memory_bytes: u64,
}

/// 命令执行结果。
#[derive(Debug, Clone, Serialize, Deserialize, JsonSchema)]
pub struct CmdResult {
    pub exit_code: i32,
    pub stdout: String,
    pub stderr: String,
}

/// 文件传输结果：pull 时 content_base64 携带内容，push 时为 None。
#[derive(Debug, Clone, Serialize, Deserialize, JsonSchema)]
pub struct FileResult {
    pub path: String,
    pub size_bytes: u64,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub content_base64: Option<String>,
}

/// 截图结果：WebP base64、mime 类型、缩放元数据。
#[derive(Debug, Clone, Serialize, Deserialize, JsonSchema)]
pub struct ScreenshotResult {
    pub image_base64: String,
    pub mime: String,
    pub meta: ScreenshotMeta,
}

/// 无返回值操作（click/type/key 等）的成功确认。
#[derive(Debug, Clone, Copy, Serialize, Deserialize, JsonSchema)]
pub struct Ack {
    pub done: bool,
}

impl Ack {
    /// 成功确认。
    pub fn ok() -> Self {
        Ack { done: true }
    }
}

/// get_a11y_tree 入参：子树根选择 + 深度 / 角色 / 节点数过滤（Locked-6，默认浅层防树爆）。
/// 复用为 MCP 工具入参（derive JsonSchema），字段文档即工具 schema 描述。
#[derive(Debug, Clone, Serialize, Deserialize, JsonSchema)]
pub struct A11yParams {
    /// 子树根选择器：缺省 / "root" / "desktop" = 平台根（桌面 / registry）；
    /// "focus" / "focused" = 当前焦点元素（仅 Windows，Linux 退化为 registry 根）。
    #[serde(default)]
    pub root: Option<String>,
    /// 遍历深度（每个顶层节点下的后代层数），缺省浅层 3，防上下文树爆（R-4）。
    #[serde(default = "default_a11y_depth")]
    pub depth: u32,
    /// 角色过滤（不区分大小写）：仅保留匹配角色的节点及其祖先路径；缺省不过滤。
    #[serde(default)]
    pub role: Option<String>,
    /// 节点总数上限，命中即截断（truncated=true）；缺省 200。
    #[serde(default = "default_a11y_max_nodes")]
    pub max_nodes: u32,
}

/// A11yParams.depth 默认值：浅层 3 层（防树爆）。
fn default_a11y_depth() -> u32 {
    3
}

/// A11yParams.max_nodes 默认值：200 节点上限（防上下文烧穿）。
fn default_a11y_max_nodes() -> u32 {
    200
}

/// 缺省 A11yParams：浅层根遍历（depth 3 / max_nodes 200 / 不过滤）。
/// 供 assert a11y 形态在未指定 `query` 时复用同一浅层默认（`#[serde(default)]` 依赖之）。
impl Default for A11yParams {
    fn default() -> Self {
        A11yParams {
            root: None,
            depth: default_a11y_depth(),
            role: None,
            max_nodes: default_a11y_max_nodes(),
        }
    }
}

/// 无障碍树节点：跨平台统一形状（role / name / bounds / value / children）。
#[derive(Debug, Clone, Serialize, Deserialize, JsonSchema)]
pub struct A11yNode {
    /// 控件角色（Windows：本地化控件类型；Linux：AT-SPI role name）。
    pub role: String,
    /// 可访问名称（标题 / 标签），无则空串。
    pub name: String,
    /// 屏幕边界 [x, y, width, height]（原生像素）；平台不可得时省略。
    #[serde(skip_serializing_if = "Option::is_none")]
    pub bounds: Option<[i32; 4]>,
    /// 控件值（如输入框文本）；best-effort，不可得时省略。
    #[serde(skip_serializing_if = "Option::is_none")]
    pub value: Option<String>,
    /// 子节点（受 depth / max_nodes 约束）。
    pub children: Vec<A11yNode>,
}

/// 无障碍树响应：顶层节点集 + 截断标志。
#[derive(Debug, Clone, Serialize, Deserialize, JsonSchema)]
pub struct A11yTree {
    /// 顶层节点（选定根的直接子节点，各为一棵子树）。
    pub nodes: Vec<A11yNode>,
    /// 是否因 depth / max_nodes 上限截断（true 表示树未完整返回）。
    pub truncated: bool,
}

/// assert 断言形态：text（纯文本比较）| a11y（无障碍树谓词）| image（区域视觉 diff）。
#[derive(Debug, Clone, Copy, Serialize, Deserialize, JsonSchema, PartialEq, Eq)]
#[serde(rename_all = "lowercase")]
pub enum AssertMode {
    /// 纯文本断言：对入参 `actual` 文本与 `expect` 按 `match_type` 比较（平台无关纯逻辑）。
    Text,
    /// 无障碍树断言：取 get_a11y_tree 结果，判定是否存在字段匹配 `expect` 的节点。
    A11y,
    /// 图像区域断言：对当前截图指定区域与参考图（base64 WebP）做像素/结构 diff。参考图来自入参
    /// `reference_image_base64`，或经 `baseline_key` 从节点侧 `<data_dir>/baselines/` 读取
    /// （M11 REC-4 落地；存取在传输层，本能力层 diff 核零改）。
    Image,
}

/// image 断言的差异度量方法：pixel（平均绝对像素差率）| ssim（结构相似性距离 1-SSIM）。
#[derive(Debug, Clone, Copy, Default, Serialize, Deserialize, JsonSchema, PartialEq, Eq)]
#[serde(rename_all = "lowercase")]
pub enum DiffMethod {
    /// 平均绝对像素差率（各通道 |a-b| 均值 / 255），缺省。0.0=完全一致，越大越不同。
    #[default]
    Pixel,
    /// 结构相似性距离（1 - 全局 SSIM，luma 通道），0.0=结构一致，越大越不同。
    Ssim,
}

/// 匹配方式：目标文本如何与 `expect` 比较。
#[derive(Debug, Clone, Copy, Default, Serialize, Deserialize, JsonSchema, PartialEq, Eq)]
#[serde(rename_all = "lowercase")]
pub enum MatchType {
    /// 子串包含（不区分大小写），缺省。最宽松，适合部分文本匹配。
    #[default]
    Contains,
    /// 全等（不区分大小写，忽略首尾空白）。
    Equals,
}

/// a11y 断言的匹配字段。应对 Windows role 本地化（zh-CN 控件类型）：默认匹配 name 稳定可靠，
/// role 匹配建议配 contains；any 则 name/role/value 任一命中即可（规避本地化差异）。
#[derive(Debug, Clone, Copy, Default, Serialize, Deserialize, JsonSchema, PartialEq, Eq)]
#[serde(rename_all = "lowercase")]
pub enum A11yField {
    /// 匹配节点可访问名 name（缺省；跨平台稳定，Windows 亦为真实文本）。
    #[default]
    Name,
    /// 匹配控件角色 role（注意 Windows 为本地化控件类型，建议配 contains）。
    Role,
    /// 匹配控件值 value（best-effort，当前平台多为空）。
    Value,
    /// 任一字段（name/role/value）匹配即命中（最宽松，规避 role 本地化）。
    Any,
}

/// assert 入参：mode 选形态 + 通用匹配语义（expect/match_type/present）+ 形态专属字段。
/// 复用为 MCP 工具入参（derive JsonSchema），字段文档即工具 schema 描述。
#[derive(Debug, Clone, Serialize, Deserialize, JsonSchema)]
pub struct AssertParams {
    /// 断言形态：`text`（纯文本比较）| `a11y`（无障碍树谓词）| `image`（区域视觉 diff）。
    /// `image` 形态另用：`reference_image_base64`（参考图，WebP base64，通常取自先前 screenshot 输出）、
    /// `region`（比对区域 `[x1,y1,x2,y2]`，缺省全图）、`threshold`（diff 阈值 [0,1]，`diff_score <= threshold`
    /// 判 passed，缺省 0.05）、`method`（`pixel` 缺省 / `ssim`）。
    pub mode: AssertMode,
    /// 期望匹配的目标串。
    pub expect: String,
    /// 匹配方式：`contains`（缺省）| `equals`。
    #[serde(default)]
    pub match_type: MatchType,
    /// 期望成立性：`true`（缺省）= 期望匹配存在（命中则 passed=true）；
    /// `false` = 反向断言，期望匹配不存在（未命中则 passed=true）。
    #[serde(default = "default_assert_present")]
    pub present: bool,
    /// text 形态：被断言的实际文本（agent 由 run_command / file_pull 等观察得到）。a11y 形态忽略。
    #[serde(default)]
    pub actual: Option<String>,
    /// a11y 形态：匹配字段 `name`（缺省）/ `role` / `value` / `any`。text 形态忽略。
    #[serde(default)]
    pub field: A11yField,
    /// a11y 形态：取树入参（root/depth/role/max_nodes），缺省浅层。text 形态忽略。
    #[serde(default)]
    pub query: A11yParams,
    /// image 形态：参考图 base64（WebP，通常取自先前 screenshot 输出）。缺省时可由
    /// `baseline_key` 从节点侧 baseline 存储读取；显式给定则优先于 baseline。text/a11y 形态忽略。
    #[serde(default)]
    pub reference_image_base64: Option<String>,
    /// image 形态：比对区域 `[x1,y1,x2,y2]`（display 像素坐标，同 zoom 契约）；缺省全图。text/a11y 形态忽略。
    #[serde(default)]
    pub region: Option<[i32; 4]>,
    /// image 形态：diff 阈值 [0.0,1.0]，`diff_score <= threshold` 判 passed。缺省 0.05。text/a11y 形态忽略。
    #[serde(default = "default_assert_threshold")]
    pub threshold: f64,
    /// image 形态：diff 方法 `pixel`（缺省）| `ssim`。text/a11y 形态忽略。
    #[serde(default)]
    pub method: DiffMethod,
    /// image 形态（M11 REC-4，additive）：节点侧 baseline 键（`[A-Za-z0-9._-]+`，无路径分隔符）。
    /// `reference_image_base64` 缺省时从 `<data_dir>/baselines/<key>.webp` 读参考图
    /// （文件缺失 → E_INVALID_ARG "baseline not found"）；与 `save_baseline` 连用为保存目标键。
    /// text/a11y 形态忽略。
    #[serde(default)]
    pub baseline_key: Option<String>,
    /// image 形态（M11 REC-4，additive）：`true` 时截取当前帧存为 `baseline_key` 指向的
    /// baseline 并短路返回（passed=true，detail="baseline saved: <key>"，不做 diff）；
    /// `baseline_key` 缺省 → E_INVALID_ARG。缺省不保存。
    #[serde(default)]
    pub save_baseline: Option<bool>,
}

/// AssertParams.present 默认值：期望匹配存在（正向断言）。
fn default_assert_present() -> bool {
    true
}

/// AssertParams.threshold 默认值：0.05（像素差率 / SSIM 距离容差，兼顾抗噪与灵敏）。
fn default_assert_threshold() -> f64 {
    0.05
}

/// assert 结果：**失败即数据（Locked-7）**——passed 无论真假均为正常返回值，经统一信封 `ok:true`
/// 回传，不落任何 E_ 错误码（不新增 E_ASSERT）；agent 据 passed 布尔自行分支。
/// 注：含 f64 `diff_score`，故不派生 `Eq`（浮点无全序）。
#[derive(Debug, Clone, Serialize, Deserialize, JsonSchema, PartialEq)]
pub struct AssertResult {
    /// 断言是否成立（实际命中与 present 期望一致即 true）。
    pub passed: bool,
    /// 人类可读判定详情（形态 + 期望 + present + 命中与否）。
    pub detail: String,
    /// 命中的文本 / 节点摘要（命中时给出，供 agent 分支与留证）；未命中为 None。
    #[serde(skip_serializing_if = "Option::is_none")]
    pub matched: Option<String>,
    /// image 形态：区域视觉差异度量（0.0=完全一致，越大越不同）；text/a11y 形态为 None。
    #[serde(skip_serializing_if = "Option::is_none")]
    pub diff_score: Option<f64>,
}

/// 能力层统一错误：每个变体映射一个 E_ 机器码（见 [`CapError::code`]）。
#[derive(Debug, thiserror::Error)]
pub enum CapError {
    #[error("node offline")]
    NodeOffline,
    #[error("coordinate out of bounds")]
    CoordOob,
    #[error("capture failed: {0}")]
    CaptureFailed(String),
    #[error("input failed: {0}")]
    InputFailed(String),
    #[error("process error: {0}")]
    ProcessError(String),
    #[error("file error: {0}")]
    FileError(String),
    #[error("invalid argument: {0}")]
    InvalidArg(String),
    #[error("unsupported operation: {0}")]
    Unsupported(String),
    /// 依赖资源瞬时不可达（区别于 [`CapError::Unsupported`] 的永久不支持、[`CapError::NodeOffline`]
    /// 的节点失联）：底层后端暂不就绪，调用方探活 / 重试后可恢复（analysis §3.3 L2 分级恢复码）。
    #[error("unavailable: {0}")]
    Unavailable(String),
    /// 音频注入无可用（虚拟）音频设备通路（M11 REC-3，蓝本契约 compass :188 错误码）：
    /// pactl/paplay 不在位、PulseAudio server 不可达、load-module 失败，或平台未实现
    /// （Windows/macOS 结构化降级——广告面已剔但 dispatch 超集网仍可派发，call-time 走本码）。
    #[error("no audio device: {0}")]
    NoAudioDev(String),
    #[error("internal error: {0}")]
    Internal(String),
}

impl CapError {
    /// 机器可读错误码（E_ 前缀），随统一信封返回。
    pub fn code(&self) -> &'static str {
        match self {
            CapError::NodeOffline => "E_NODE_OFFLINE",
            CapError::CoordOob => "E_COORD_OOB",
            CapError::CaptureFailed(_) => "E_CAPTURE_FAILED",
            CapError::InputFailed(_) => "E_INPUT_FAILED",
            CapError::ProcessError(_) => "E_PROCESS_FAILED",
            CapError::FileError(_) => "E_FILE_FAILED",
            CapError::InvalidArg(_) => "E_INVALID_ARG",
            CapError::Unsupported(_) => "E_UNSUPPORTED",
            CapError::Unavailable(_) => "E_UNAVAILABLE",
            CapError::NoAudioDev(_) => "E_NO_AUDIO_DEV",
            CapError::Internal(_) => "E_INTERNAL",
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn cap_error_code_mapping() {
        assert_eq!(CapError::NodeOffline.code(), "E_NODE_OFFLINE");
        assert_eq!(CapError::CoordOob.code(), "E_COORD_OOB");
        assert_eq!(CapError::CaptureFailed("x".into()).code(), "E_CAPTURE_FAILED");
        assert_eq!(CapError::Unavailable("x".into()).code(), "E_UNAVAILABLE");
        assert_eq!(CapError::NoAudioDev("x".into()).code(), "E_NO_AUDIO_DEV");
        assert_eq!(CapError::Internal("x".into()).code(), "E_INTERNAL");
    }
}
