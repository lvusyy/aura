//! aura-platform：平台实现层。
//!
//! 实现 aura-capability 的三域子 trait 于统一的 [`PlatformDriver`]，平台特化代码以
//! `#[cfg(target_os = "...")]` 门控集中于此。M1 骨架阶段各方法为 stub（返回 E_INTERNAL），
//! 由 TASK-004/005/006 分域填充真实实现。

use std::sync::Arc;

use aura_capability::{AssertDriver, DeviceDriver};

mod screen;
mod input;
mod process_file;
mod a11y;
mod record;
mod audio;
mod android;
mod ios;

use android::AndroidDriver;
use ios::{DEFAULT_WDA_URL, IosDriver};

/// 驱动种类：运行时选择设备驱动实现（编译期→运行时，Locked-4/5）。与 [`build_driver`] 工厂共址于
/// 平台层——不放 aura-capability（该层 impl-agnostic，枚举 desktop/android 具体 impl 会向上泄漏 impl 身份）。
pub enum DriverKind {
    /// 桌面驱动（Windows/Linux/macOS 原生，[`PlatformDriver`]），默认。
    Desktop,
    /// Android 驱动（经 adb shell out，host-agnostic，[`AndroidDriver`]）。
    Android,
    /// iOS 驱动（经 WDA HTTP，host-agnostic，[`IosDriver`]；模拟器 / 真机双后端同契约）。
    Ios,
}

/// 平台设备驱动：以 `#[cfg(target_os)]` 门控各平台原生实现，统一实现三域能力子 trait。
pub struct PlatformDriver;

impl PlatformDriver {
    pub fn new() -> Self {
        PlatformDriver
    }
}

impl Default for PlatformDriver {
    fn default() -> Self {
        Self::new()
    }
}

// PlatformDriver 同时实现 ScreenDriver/InputDriver/ProcessFileDriver（见各域文件），
// 故可作为统一 DeviceDriver 接入。
// AssertDriver 为平台无关组合能力（text 纯逻辑 + a11y 复用本 driver 的 get_a11y_tree），
// 默认方法即全部实现，此处空 impl 并入 DeviceDriver bound，无需新平台后端。
impl AssertDriver for PlatformDriver {}

impl DeviceDriver for PlatformDriver {
    /// desktop 广告面（M11 plan D6）：Linux 广告全集 21（audio_inject pactl 主线实装）；
    /// Windows / macOS 无虚拟音频设备通路，剔除 audio_inject 广告保 20——call-time 仍可派发
    /// （dispatch 超集网不动）并返 E_NO_AUDIO_DEV 结构化降级，广告收窄与 call-time 语义成对，
    /// 防「广告 vs call-time 错位」旧债复刻。其余工具沿默认全支持。
    fn supports_tool(&self, tool: &str) -> bool {
        if tool == "audio_inject" {
            return cfg!(target_os = "linux");
        }
        true
    }
}

/// 构造设备驱动，返回 trait 对象供传输层持有。运行时按 `kind` 选择实现（编译期→运行时，
/// Locked-4/5）；`serial` 由 Android 臂（`adb -s <serial>` 设备选择）与 iOS 臂（承载 udid）消费，
/// Desktop 臂忽略；`wda_url` 仅 iOS 臂消费（WDA HTTP 端点，`None` 回退 `DEFAULT_WDA_URL`），
/// Desktop/Android 臂忽略。三参为内部签名（非 aura.v1 契约，desktop/android 忽略 wda_url 同惯例）。
///
/// Desktop 臂保持 `Arc::new(PlatformDriver::new())` 原构造不变——桌面行为回归零破坏。
pub fn build_driver(
    kind: DriverKind,
    serial: Option<String>,
    wda_url: Option<String>,
) -> Arc<dyn DeviceDriver> {
    match kind {
        DriverKind::Desktop => Arc::new(PlatformDriver::new()),
        DriverKind::Android => Arc::new(AndroidDriver::new(serial.unwrap_or_default())),
        DriverKind::Ios => Arc::new(IosDriver::new(
            serial.unwrap_or_default(),
            wda_url.unwrap_or_else(|| DEFAULT_WDA_URL.into()),
        )),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// driver-seam 运行时化：`platform()` 由 driver 派生（`PlatformDriver` 继承默认 `"desktop"`，
    /// `AndroidDriver` 覆写 `"android"`），且 [`build_driver`] 工厂按 [`DriverKind`] 正确分发。
    /// 取代 grpc_reverse.rs 旧 `platform_name()` 的 host `cfg!(target_os)` 空转集合断言。
    #[test]
    fn build_driver_derives_platform_by_kind() {
        // 直接构造：默认继承 desktop / 覆写 android / 覆写 ios。
        assert_eq!(PlatformDriver::new().platform(), "desktop");
        assert_eq!(
            AndroidDriver::new("emulator-5554".to_string()).platform(),
            "android"
        );
        assert_eq!(
            IosDriver::new("00008030-udid".to_string(), DEFAULT_WDA_URL.to_string()).platform(),
            "ios"
        );
        // 工厂分发：Desktop→desktop、Android→android（serial 透传）、Ios→ios（serial=udid + wda_url 透传）。
        assert_eq!(
            build_driver(DriverKind::Desktop, None, None).platform(),
            "desktop"
        );
        assert_eq!(
            build_driver(DriverKind::Android, Some("emulator-5554".to_string()), None).platform(),
            "android"
        );
        assert_eq!(
            build_driver(
                DriverKind::Ios,
                Some("00008030-udid".to_string()),
                Some("http://127.0.0.1:8100".to_string())
            )
            .platform(),
            "ios"
        );
    }
}
