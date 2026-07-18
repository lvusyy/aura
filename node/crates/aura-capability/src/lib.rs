//! aura-capability：平台无关、传输无关的能力抽象层。
//!
//! 定义设备能力子 trait（屏幕 / 输入 / 进程文件）、共享领域类型与统一响应信封。
//! 依赖方向：本 crate 不依赖任何上层或平台实现，由 aura-platform 实现其 trait，
//! 由上层传输适配持有 `Arc<dyn DeviceDriver>`。

use async_trait::async_trait;

pub mod types;
pub mod envelope;
pub mod screen;
pub mod input;
pub mod process_file;
pub mod a11y;
pub mod assert;
pub mod record;
pub mod audio;

pub use a11y::A11yDriver;
pub use assert::AssertDriver;
pub use audio::{AudioDriver, AudioInjectParams, AudioInjectResult};
pub use envelope::{Envelope, ErrObj};
pub use input::InputDriver;
pub use process_file::ProcessFileDriver;
pub use record::{RecordArtifact, RecordDriver, RecordParams};
pub use screen::{ScreenDriver, ScreenshotOpts};
pub use types::*;

/// MCP 工具对外名 canonical 单源（GAP-7）。**真源 = aura-node `tool_registry!` 注册表**（工具名
/// 与 gRPC 派发同一宏展开），本清单是其唯一可跨 crate 引用的编译期锚点：aura-node 侧测试断言
/// 「注册表 == 本清单」（逐字含顺序），aura-platform android/ios 两侧 triage 矩阵测试断言
/// 「矩阵键集 == 本清单」——任一侧增删改工具名，`cargo test --workspace` 必红，漂移不可能静默。
/// 新增工具：先改 tool_registry! 注册表，再同步本清单（顺序保持逐字一致）。
pub const TOOL_NAMES: &[&str] = &[
    "screenshot",
    "zoom",
    "list_displays",
    "switch_display",
    "click",
    "type",
    "key",
    "scroll",
    "drag",
    "move_mouse",
    "wait",
    "list_processes",
    "kill_process",
    "file_push",
    "file_pull",
    "run_command",
    "get_a11y_tree",
    "assert",
    "start_recording",
    "stop_recording",
    "audio_inject",
];

/// 设备驱动统一抽象：聚合屏幕 / 输入 / 进程文件 / 无障碍 / 断言 / 录屏 / 音频注入七域能力，所有平台实现经此单一 trait 接入。
/// 上层适配只持有 `Arc<dyn DeviceDriver>`，不感知具体平台。
/// assert 为平台无关的组合能力（复用 A11yDriver + 纯逻辑），经默认方法实现，平台空 impl 即并入。
/// record 为 Windows/Linux 能力（其余平台 impl 返回 E_UNSUPPORTED），经平台层 `#[cfg]` 门控并入。
/// audio 为 Linux 主线能力（Win/mac 返回 E_NO_AUDIO_DEV 结构化降级，Android/iOS 返回 E_UNSUPPORTED）。
#[async_trait]
pub trait DeviceDriver:
    ScreenDriver
    + InputDriver
    + ProcessFileDriver
    + A11yDriver
    + AssertDriver
    + RecordDriver
    + AudioDriver
    + Send
    + Sync
{
    /// 节点平台名（对齐 proto Register.platform）。默认 `"desktop"`——桌面驱动（`PlatformDriver`）
    /// 继承之；`AndroidDriver` 覆写为 `"android"`。传输层 Register 帧由本方法派生 platform，取代
    /// 旧的 host `cfg!(target_os)` 判定（node 宿主 OS ≠ 被控设备平台，故 platform 须由 driver 派生）。
    fn platform(&self) -> &'static str {
        "desktop"
    }

    /// 被控设备系统版本覆写钩子（M12 批C，镜像 [`DeviceDriver::platform`] 的驱动派生模式）：
    /// `None`（默认）= driver 无 per-device 语义，传输层回落宿主采集（/etc/os-release、`cmd /c ver`、
    /// sw_vers——桌面驱动被控即宿主，语义不变零行为漂移）；`Some(v)` = driver 权威值，传输层原样上报
    /// （`Some("")` 表示采集失败的显式空——前端不显，但**不**回落宿主采集：android 等远控驱动的节点
    /// 程序跑在 Linux 宿主上，宿主值是错报）。`AndroidDriver` 覆写经 adb getprop 采集；iOS 未接入
    /// 生产（无 ios 节点），`IosDriver` 暂不覆写，接入时同法经 WDA/ideviceinfo 补齐。
    async fn os_version(&self) -> Option<String> {
        None
    }

    /// 该节点驱动/派生的服务标识（M12 批D，Register.attached 自报，fleet 所属链定位）：
    /// `None`（默认）= 自身即被控设备（桌面驱动），无派生服务；`Some(v)` = driver 从自身配置拼装的
    /// 被驱动服务标识（`AndroidDriver` → "redroid@<serial>"；iOS 接入时同法 "wda@<udid>"）。
    /// 配置派生零 IO 故同步方法。传输层 env `AURA_ATTACHED` 存在时优先覆写本值（部署面显式声明
    /// 优先于驱动默认拼装，如 selkies 边车声明 "selkies-desktop@DISPLAY=:20"）。
    fn attached(&self) -> Option<String> {
        None
    }

    /// 节点对外**广告面**工具子集谓词（Locked-6，镜像 [`DeviceDriver::platform`] 的驱动派生模式）：
    /// 给定工具对外名，返回本 driver 是否将其纳入 Register.tools 上报与 MCP list_tools 暴露。
    /// 默认全支持（`true`）——桌面驱动继承全集广告；`AndroidDriver` 覆写剔除无对应移动语义的工具。
    /// 与 call-time `E_UNSUPPORTED`（dispatch 超集网）同粒度：单工具名恒可派发，本谓词仅收窄「广而告之」
    /// 的能力面，不阻断派发（三面过滤只影响广告面，M1-M5 dispatch 契约不回改）。
    /// impl-agnostic：本 trait 层不引 node `TOOL_NAMES`（跨 crate 不可行），故以 per-tool 谓词表达子集。
    fn supports_tool(&self, _tool: &str) -> bool {
        true
    }
}
