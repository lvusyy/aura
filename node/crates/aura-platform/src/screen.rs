//! ScreenDriver 平台实现：平台特化采集 → fast_image_resize 降采样 → webp 有损编码。
//!
//! 采集后端只此一层分平台（其余流水线平台无关，见 [`backend`] 模块）：
//!   - Linux（X11）：xcap =0.4.1（pre-pipewire，规避 0.5+ 的 pipewire→libspa
//!     与旧系统头不兼容）。该版无 `capture_region`，zoom 整屏采集后内存裁剪。
//!   - macOS：CG Online 枚举 + ScreenCaptureKit 单帧采集（批F）。xcap 走的
//!     CGGetActiveDisplayList 在 macOS 26 看不到虚拟显示器（合盖无头方案的常驻主屏），
//!     CGDisplayCreateImage 采集面亦被 26 SDK 封禁，见 mac [`backend`] 模块注释。
//!   - Windows：windows-capture（WGC / Windows.Graphics.Capture）。WGC 对 RDP/无头会话截图友好，
//!     规避 xcap GDI BitBlt 在 RDP 会话隔离下句柄无效（0x80070006）。回调式采集经 channel 收敛为同步单帧。
//! 采集层统一交付 (RGBA8 像素, width, height)；缩放 / 编码 / 坐标回映射逻辑两平台复用。
//!
//! 流水线（SUMMARY 定案）：采集 RGBA8 → 长边 > 1280px 时等比降采样（SIMD，默认 Lanczos3）
//! → webp q80 编码 → base64。全流水线置于单个 `tokio::task::spawn_blocking` 闭包内
//! （采集器闭包内建、不跨 `.await`，满足 ARCHITECTURE §3 决策3 的阻塞边界铁律）。
//!
//! 显示器选择：对外 `display` = 枚举 0 基序号（同 [`DisplayInfo::id`]），缺省 0 命中首个显示器。

use async_trait::async_trait;
use base64::prelude::{Engine as _, BASE64_STANDARD};
use fast_image_resize::images::Image;
use fast_image_resize::{PixelType, ResizeOptions, Resizer};
use webp::Encoder;

use aura_capability::{
    CapError, DisplayInfo, Region, ScreenDriver, ScreenshotMeta, ScreenshotOpts, ScreenshotResult,
};

use crate::PlatformDriver;

/// 缩放长边上限（Anthropic XGA 最佳实践：长边 ≤1280px 再编码）。
/// M11 起为 `ScreenshotOpts::max_dim` 缺省值单源（`pub(crate)`：android/ios 同引）。
pub(crate) const MAX_DIM: u32 = 1280;
/// WebP 有损编码质量（0–100，q80 兼顾体积与清晰度）。
/// M11 起为 `ScreenshotOpts::quality` 缺省值单源（`pub(crate)`：android/ios 同引）。
pub(crate) const WEBP_QUALITY: f32 = 80.0;

/// 采集交付：原生 RGBA8 像素缓冲（长度 = w*h*4）+ 宽 + 高。平台采集层统一产出此三元组，
/// 作为采集（平台特化）与流水线（平台无关）之间无依赖的清晰边界。
/// `pub(crate)`：mac 录屏后端（record.rs）复用本采集面逐帧取材（虚拟屏唯一可用帧源）。
pub(crate) type CapturedRgba = (Vec<u8>, u32, u32);

#[async_trait]
impl ScreenDriver for PlatformDriver {
    async fn screenshot(
        &self,
        display: Option<u32>,
        opts: ScreenshotOpts,
    ) -> Result<ScreenshotResult, CapError> {
        // 阻塞的采集 + CPU 密集的缩放/编码全部收敛进单个 spawn_blocking 闭包。
        tokio::task::spawn_blocking(move || -> Result<ScreenshotResult, CapError> {
            let (pixels, w, h) = backend::capture_monitor_rgba(display)?;
            encode_scaled(
                pixels,
                w,
                h,
                opts.max_dim.unwrap_or(MAX_DIM),
                opts.quality.unwrap_or(WEBP_QUALITY),
            )
        })
        .await
        .map_err(|e| CapError::CaptureFailed(format!("screenshot task join failed: {e}")))?
    }

    async fn zoom(&self, region: Region, opts: ScreenshotOpts) -> Result<ScreenshotResult, CapError> {
        // P0 契约：region = [x1,y1,x2,y2]（左上角 + 右下角，显示器本地像素）。
        // 整屏采集后在内存裁剪两角区域，缺省不降采样（max_dim = u32::MAX）保留原生分辨率以观察小字；
        // 显式传 max_dim 时以参数为准（M11 高保真参数不破坏缺省高保真先例）。
        let (x, y, w, h) = region_to_xywh(region)?;
        tokio::task::spawn_blocking(move || -> Result<ScreenshotResult, CapError> {
            // trait 未透传当前显示器，故区域放大取默认显示器（序号 0）；多显示器区域放大延后。
            let (full, iw, ih) = backend::capture_monitor_rgba(None)?;
            let cropped = crop_rgba(&full, iw, ih, x, y, w, h)?;
            encode_scaled(
                cropped,
                w,
                h,
                opts.max_dim.unwrap_or(u32::MAX),
                opts.quality.unwrap_or(WEBP_QUALITY),
            )
        })
        .await
        .map_err(|e| CapError::CaptureFailed(format!("zoom task join failed: {e}")))?
    }

    async fn list_displays(&self) -> Result<Vec<DisplayInfo>, CapError> {
        tokio::task::spawn_blocking(backend::list_monitors)
            .await
            .map_err(|e| CapError::CaptureFailed(format!("list_displays task join failed: {e}")))?
    }

    async fn switch_display(&self, display: u32) -> Result<DisplayInfo, CapError> {
        tokio::task::spawn_blocking(move || backend::get_monitor_info(display))
            .await
            .map_err(|e| CapError::CaptureFailed(format!("switch_display task join failed: {e}")))?
    }
}

// ===== 平台无关流水线（缩放 / 编码 / 裁剪 / 坐标换算），两平台复用 =====

/// 缩放（长边 > max_dim 时等比降采样）并按 quality WebP 编码，产出带回映射元数据的结果。
/// 入参为原生 RGBA8 像素缓冲 + 尺寸（采集层统一交付格式）；max_dim/quality 由调用方以
/// `opts.max_dim.unwrap_or(MAX_DIM)` / `opts.quality.unwrap_or(WEBP_QUALITY)` 解析（缺省单源在 const）。
///
/// 元数据：native_w/h = 采集原生尺寸，display_w/h = 缩放后尺寸，
/// scale = native_w / display_w（native 与 display 等比，长/宽 scale 相等；未缩放时 = 1.0）。
/// 模型按 display 坐标操作，input 域据 scale 回映射至原生像素执行。
///
/// `pub(crate)`：除桌面 [`PlatformDriver`] 外，[`crate::android::AndroidDriver`] 亦复用此流水线
/// （screencap raw 帧 → 同一 XGA 缩放 + WebP 编码），避免为 Android 另引 image/png decoder 依赖。
pub(crate) fn encode_scaled(
    native_pixels: Vec<u8>,
    native_w: u32,
    native_h: u32,
    max_dim: u32,
    quality: f32,
) -> Result<ScreenshotResult, CapError> {
    if native_w == 0 || native_h == 0 {
        return Err(CapError::CaptureFailed("captured image is empty".to_string()));
    }

    let longest = native_w.max(native_h);
    let (display_w, display_h, pixels) = if longest > max_dim {
        // 等比降采样：目标长边 = max_dim，另一边按比例四舍五入（至少 1px 避免 0 尺寸）。
        let ratio = max_dim as f64 / longest as f64;
        let dw = ((native_w as f64 * ratio).round() as u32).max(1);
        let dh = ((native_h as f64 * ratio).round() as u32).max(1);
        let resized = resize_rgba(native_pixels, native_w, native_h, dw, dh)?;
        (dw, dh, resized)
    } else {
        (native_w, native_h, native_pixels)
    };

    let webp_mem = Encoder::from_rgba(&pixels, display_w, display_h).encode(quality);
    let image_base64 = BASE64_STANDARD.encode(&*webp_mem);
    let scale = native_w as f64 / display_w as f64;

    Ok(ScreenshotResult {
        image_base64,
        mime: "image/webp".to_string(),
        meta: ScreenshotMeta {
            native_w,
            native_h,
            display_w,
            display_h,
            scale,
        },
    })
}

/// SIMD 等比降采样（fast_image_resize，RGBA U8x4，默认 Lanczos3 卷积）。
/// 入参为原生 RGBA8 缓冲（长度 = src_w*src_h*4），返回缩放后缓冲（dst_w*dst_h*4）。
/// `pub(crate)`：android zoom/screenshot 流水线复用（见 [`encode_scaled`]）。
pub(crate) fn resize_rgba(
    src_pixels: Vec<u8>,
    src_w: u32,
    src_h: u32,
    dst_w: u32,
    dst_h: u32,
) -> Result<Vec<u8>, CapError> {
    let src = Image::from_vec_u8(src_w, src_h, src_pixels, PixelType::U8x4)
        .map_err(|e| CapError::CaptureFailed(format!("resize src build failed: {e}")))?;
    let mut dst = Image::new(dst_w, dst_h, PixelType::U8x4);
    let opts = ResizeOptions::new();
    let mut resizer = Resizer::new();
    resizer
        .resize(&src, &mut dst, Some(&opts))
        .map_err(|e| CapError::CaptureFailed(format!("resize failed: {e}")))?;
    Ok(dst.into_vec())
}

/// 从整屏 RGBA8 缓冲裁剪 (x, y, w, h) 区域，返回裁剪后缓冲（长度 = w*h*4）。
/// 越界（区域超出显示器尺寸）返回 InvalidArg；索引用 usize 计算避免大图溢出。
/// `pub(crate)`：android zoom 复用（screencap 全屏帧内存裁剪指定区域）。
pub(crate) fn crop_rgba(
    src: &[u8],
    iw: u32,
    ih: u32,
    x: u32,
    y: u32,
    w: u32,
    h: u32,
) -> Result<Vec<u8>, CapError> {
    if x as u64 + w as u64 > iw as u64 || y as u64 + h as u64 > ih as u64 {
        return Err(CapError::InvalidArg(format!(
            "zoom region [x={x},y={y},w={w},h={h}] exceeds display {iw}x{ih}"
        )));
    }
    let stride = iw as usize * 4;
    let row_bytes = w as usize * 4;
    let mut out = Vec::with_capacity(row_bytes * h as usize);
    for row in 0..h as usize {
        let start = (y as usize + row) * stride + x as usize * 4;
        out.extend_from_slice(&src[start..start + row_bytes]);
    }
    Ok(out)
}

/// P0 两角 region [x1,y1,x2,y2] → 裁剪所需 (x, y, width, height)。
/// 校验右下角严格大于左上角、且原点非负（裁剪按 u32 计算）。
/// `pub(crate)`：android zoom 复用（Region 两角 → crop_rgba 所需 xywh）。
pub(crate) fn region_to_xywh(region: Region) -> Result<(u32, u32, u32, u32), CapError> {
    if region.x2 <= region.x1 || region.y2 <= region.y1 {
        return Err(CapError::InvalidArg(format!(
            "invalid region: expected x2>x1 && y2>y1, got [{},{},{},{}]",
            region.x1, region.y1, region.x2, region.y2
        )));
    }
    if region.x1 < 0 || region.y1 < 0 {
        return Err(CapError::InvalidArg(format!(
            "negative region origin: [{},{}]",
            region.x1, region.y1
        )));
    }
    let w = (region.x2 - region.x1) as u32;
    let h = (region.y2 - region.y1) as u32;
    Ok((region.x1 as u32, region.y1 as u32, w, h))
}

// ===== 采集后端：平台特化（只此层分平台）=====

/// Linux（X11）采集后端：xcap =0.4.1。macOS 已迁出（虚拟显示器失明，见 mac backend），
/// cfg 收紧为 Linux 专属，Linux 侧行为与依赖零变化。
#[cfg(all(unix, not(target_os = "macos")))]
mod backend {
    use aura_capability::{CapError, DisplayInfo};
    use xcap::Monitor;

    use super::CapturedRgba;

    /// 把 xcap 错误统一折叠为 `CapError::CaptureFailed`（E_CAPTURE_FAILED）。
    fn cap_err(e: xcap::XCapError) -> CapError {
        CapError::CaptureFailed(e.to_string())
    }

    /// 按 0 基序号选取显示器（缺省序号 0 = 首个显示器）。
    fn pick_monitor(index: Option<u32>) -> Result<Monitor, CapError> {
        let monitors = Monitor::all().map_err(cap_err)?;
        let idx = index.unwrap_or(0) as usize;
        monitors
            .into_iter()
            .nth(idx)
            .ok_or_else(|| CapError::InvalidArg(format!("display index {idx} out of range")))
    }

    /// xcap `Monitor` → 平台无关 `DisplayInfo`。`id` 由调用方注入（枚举序号）。
    fn monitor_to_info(id: u32, m: &Monitor) -> Result<DisplayInfo, CapError> {
        Ok(DisplayInfo {
            id,
            name: m.name().map_err(cap_err)?,
            x: m.x().map_err(cap_err)?,
            y: m.y().map_err(cap_err)?,
            width: m.width().map_err(cap_err)?,
            height: m.height().map_err(cap_err)?,
            is_primary: m.is_primary().map_err(cap_err)?,
            scale: m.scale_factor().map_err(cap_err)? as f64,
        })
    }

    /// 采集指定显示器整屏为 (RGBA8, w, h)。xcap capture_image 已归一化为 RGBA 序。
    pub(super) fn capture_monitor_rgba(index: Option<u32>) -> Result<CapturedRgba, CapError> {
        let monitor = pick_monitor(index)?;
        let img = monitor.capture_image().map_err(cap_err)?;
        let (w, h) = (img.width(), img.height());
        Ok((img.into_raw(), w, h))
    }

    pub(super) fn list_monitors() -> Result<Vec<DisplayInfo>, CapError> {
        let monitors = Monitor::all().map_err(cap_err)?;
        // id 用枚举序号（0 基），对外稳定且默认 0 命中首个显示器。
        monitors
            .iter()
            .enumerate()
            .map(|(idx, m)| monitor_to_info(idx as u32, m))
            .collect()
    }

    pub(super) fn get_monitor_info(index: u32) -> Result<DisplayInfo, CapError> {
        let monitors = Monitor::all().map_err(cap_err)?;
        let m = monitors
            .get(index as usize)
            .ok_or_else(|| CapError::InvalidArg(format!("display index {index} out of range")))?;
        monitor_to_info(index, m)
    }
}

/// macOS 采集后端：CG Online 枚举 + ScreenCaptureKit 单帧采集（批F）。
///
/// 为什么弃 xcap：其 mac 侧枚举走 CGGetActiveDisplayList，而虚拟显示器（DeskPad，
/// MacBook 合盖无头场景的常驻主屏）在 macOS 26 上 active=0/online=1/main=1——active
/// 列表恒空致枚举/截图全灭；采集面 CGDisplayCreateImage 亦被 26 SDK 封禁
/// （unavailable: Please use ScreenCaptureKit）。故枚举走 CGGetOnlineDisplayList
/// （CGDirectDisplay.h 元数据 C API 健在），采集走 SCK 单帧（系统 screencapture 同
/// 路径，实证虚拟屏出真帧）。
///
/// SCK 异步 completion 经 sync_channel 收敛为同步单帧（调用点已在 spawn_blocking，
/// 复刻 Windows WGC 后端同款收敛惯例）；SCK 对象不跨线程持有——SCShareableContent
/// 回调线程内一口气完成 找屏→建 filter→captureImage→CGImage blit，只有像素缓冲过
/// channel（规避 Retained 跨线程 Send 约束）。CGImage → RGBA8 经 CGBitmapContext
/// （RGBA8888 big-endian、premultiplied-last）重绘一遍，免猜源图 BGRA/字节序/行距
/// padding（截图 alpha 恒 255，premultiplied 无损）。
#[cfg(target_os = "macos")]
pub(crate) mod backend {
    use std::ffi::c_void;
    use std::sync::mpsc;
    use std::time::Duration;

    use aura_capability::{CapError, DisplayInfo};
    use block2::RcBlock;
    use objc2::AnyThread;
    use objc2_core_graphics::CGImage;
    use objc2_foundation::{NSArray, NSError};
    use objc2_screen_capture_kit::{
        SCContentFilter, SCScreenshotManager, SCShareableContent, SCStreamConfiguration, SCWindow,
    };

    use super::CapturedRgba;

    // ---- CoreGraphics C API 直调面（显示器元数据 + CGImage blit；26 上未废弃）----
    // 几何类型自定义 repr(C)（仅供本模块 extern 签名，不与 objc2 生态类型混用）。

    #[repr(C)]
    #[derive(Clone, Copy)]
    struct CGPoint {
        x: f64,
        y: f64,
    }
    #[repr(C)]
    #[derive(Clone, Copy)]
    struct CGSize {
        width: f64,
        height: f64,
    }
    #[repr(C)]
    #[derive(Clone, Copy)]
    struct CGRect {
        origin: CGPoint,
        size: CGSize,
    }

    #[link(name = "CoreGraphics", kind = "framework")]
    extern "C" {
        fn CGGetOnlineDisplayList(
            max_displays: u32,
            online_displays: *mut u32,
            display_count: *mut u32,
        ) -> i32;
        fn CGDisplayBounds(display: u32) -> CGRect;
        fn CGDisplayIsMain(display: u32) -> u32;
        fn CGDisplayCopyDisplayMode(display: u32) -> *mut c_void;
        fn CGDisplayModeGetPixelWidth(mode: *mut c_void) -> usize;
        fn CGDisplayModeGetPixelHeight(mode: *mut c_void) -> usize;
        fn CGDisplayModeRelease(mode: *mut c_void);
        fn CGImageGetWidth(image: *const c_void) -> usize;
        fn CGImageGetHeight(image: *const c_void) -> usize;
        // CGDisplayCreateImage：按 CGDirectDisplayID 直采整屏。macOS 26 SDK 标弃用（"Please use
        // ScreenCaptureKit"）但符号仍在 CoreGraphics dylib——手写 extern 绕过 SDK availability 注解直调。
        // 关键：SCShareableContent 看不到 DeskPad 类 CGVirtualDisplay 虚拟屏（返回空显示器列表），而
        // CG 直采能抓（与 CGGetOnlineDisplayList 同源 ID）。需屏幕录制授权；返回 +1 owned CGImage 须 Release。
        fn CGDisplayCreateImage(display: u32) -> *const c_void;
        fn CGImageRelease(image: *const c_void);
        fn CGColorSpaceCreateDeviceRGB() -> *mut c_void;
        fn CGColorSpaceRelease(space: *mut c_void);
        fn CGBitmapContextCreate(
            data: *mut c_void,
            width: usize,
            height: usize,
            bits_per_component: usize,
            bytes_per_row: usize,
            space: *mut c_void,
            bitmap_info: u32,
        ) -> *mut c_void;
        fn CGContextDrawImage(ctx: *mut c_void, rect: CGRect, image: *const c_void);
        fn CGContextRelease(ctx: *mut c_void);
    }

    /// kCGImageAlphaPremultipliedLast(1) | kCGBitmapByteOrder32Big(4<<12)：
    /// 内存序 R,G,B,A —— 与流水线 RGBA8 交付契约一致。
    const RGBA_BITMAP_INFO: u32 = 1 | (4 << 12);

    /// 在线显示器 CGDirectDisplayID 列表。枚举序（0 基）即对外 display 序号，
    /// 语义同 Linux xcap 后端（对外契约见文件头注释）。
    fn online_ids() -> Result<Vec<u32>, CapError> {
        let mut ids = [0u32; 16];
        let mut n: u32 = 0;
        let err = unsafe { CGGetOnlineDisplayList(16, ids.as_mut_ptr(), &mut n) };
        if err != 0 {
            return Err(CapError::CaptureFailed(format!(
                "CGGetOnlineDisplayList failed: cgerror {err}"
            )));
        }
        Ok(ids[..n as usize].to_vec())
    }

    /// 显示器像素尺寸（display mode 真值；mode 缺失时回退 bounds 点值 = scale 1.0）。
    fn pixel_size(id: u32) -> (u32, u32) {
        unsafe {
            let mode = CGDisplayCopyDisplayMode(id);
            if mode.is_null() {
                let b = CGDisplayBounds(id);
                return (b.size.width as u32, b.size.height as u32);
            }
            let wh = (
                CGDisplayModeGetPixelWidth(mode) as u32,
                CGDisplayModeGetPixelHeight(mode) as u32,
            );
            CGDisplayModeRelease(mode);
            wh
        }
    }

    /// CG 元数据 → 平台无关 `DisplayInfo`。width/height 为逻辑点（bounds），
    /// scale = 像素宽/逻辑宽——与 xcap 后端对外语义一致（内建 Retina 屏历史值 2.0）。
    fn display_info(seq: u32, id: u32) -> DisplayInfo {
        let b = unsafe { CGDisplayBounds(id) };
        let (pw, _) = pixel_size(id);
        let logical_w = if b.size.width > 0.0 { b.size.width } else { 1.0 };
        DisplayInfo {
            id: seq,
            name: format!("Display {id}"),
            x: b.origin.x as i32,
            y: b.origin.y as i32,
            width: b.size.width as u32,
            height: b.size.height as u32,
            is_primary: unsafe { CGDisplayIsMain(id) } != 0,
            scale: f64::from(pw) / logical_w,
        }
    }

    pub(super) fn list_monitors() -> Result<Vec<DisplayInfo>, CapError> {
        Ok(online_ids()?
            .iter()
            .enumerate()
            .map(|(i, &id)| display_info(i as u32, id))
            .collect())
    }

    pub(super) fn get_monitor_info(index: u32) -> Result<DisplayInfo, CapError> {
        let ids = online_ids()?;
        let &id = ids.get(index as usize).ok_or_else(|| {
            CapError::InvalidArg(format!("display index {index} out of range"))
        })?;
        Ok(display_info(index, id))
    }

    /// 主显示器 backing scale（像素/逻辑点，Retina 常为 2.0）。input 域坐标折算复用
    /// （批F：input 原经 xcap 取主屏 scale，mac 迁出 xcap 后改与本枚举同源——CG Online
    /// 列表下虚拟显示器亦可见）。主屏缺失回落首屏；再缺失或值非法回落 1.0 恒等（安全降级，
    /// 语义与 input 域原实现一致）。
    pub(crate) fn primary_backing_scale() -> f64 {
        let Ok(ids) = online_ids() else { return 1.0 };
        let id = ids
            .iter()
            .copied()
            .find(|&id| unsafe { CGDisplayIsMain(id) } != 0)
            .or_else(|| ids.first().copied());
        let Some(id) = id else { return 1.0 };
        let b = unsafe { CGDisplayBounds(id) };
        let logical_w = if b.size.width > 0.0 { b.size.width } else { 1.0 };
        let (pw, _) = pixel_size(id);
        let s = f64::from(pw) / logical_w;
        if s.is_finite() && s > 0.0 {
            s
        } else {
            1.0
        }
    }

    /// 采集指定显示器整屏为 (RGBA8, w, h)。首选 CGDisplayCreateImage 直采（按 CG 显示 ID，能抓
    /// SCShareableContent 看不到的虚拟屏）；nil 时回退 SCK 单帧采集（真实多显示器等 SCK 更稳的场景）。
    /// `pub(crate)`：截图流水线之外，mac 录屏后端（record.rs）按 fps 逐帧调用本函数取材——
    /// screencapture -v 对虚拟屏挂死、AVCaptureScreenInput 已被 macOS 26 移除，本采集面是唯一帧源。
    pub(crate) fn capture_monitor_rgba(index: Option<u32>) -> Result<CapturedRgba, CapError> {
        let ids = online_ids()?;
        let idx = index.unwrap_or(0) as usize;
        let &cgid = ids
            .get(idx)
            .ok_or_else(|| CapError::InvalidArg(format!("display index {idx} out of range")))?;
        match cg_capture(cgid) {
            Ok(rgba) => Ok(rgba),
            Err(_) => {
                let (pw, ph) = pixel_size(cgid);
                sck_capture(cgid, pw, ph)
            }
        }
    }

    /// CGDisplayCreateImage 直采：按 CGDirectDisplayID 抓整屏 → CGImage → RGBA8。CG 枚举能见
    /// CGVirtualDisplay 虚拟屏（DeskPad），SCShareableContent 却不能，故此为无头虚拟屏首选路径。
    /// 需屏幕录制授权；返回 +1 owned CGImage，转换后 Release。nil（无授权/该 id 无法采集）→ Err 供回退。
    fn cg_capture(cgid: u32) -> Result<CapturedRgba, CapError> {
        let image = unsafe { CGDisplayCreateImage(cgid) };
        if image.is_null() {
            return Err(CapError::CaptureFailed(format!(
                "CGDisplayCreateImage returned nil for display id={cgid}"
            )));
        }
        let out = cgimage_to_rgba(image);
        unsafe { CGImageRelease(image) };
        out.map_err(CapError::CaptureFailed)
    }

    /// CGImage → RGBA8 缓冲：按图像自身像素尺寸建 RGBA8888 位图上下文重绘。由 cg_capture 直采路径
    /// 或 sck_capture 回调线程调用（前者转换后直接返回，后者产物 Vec 过 channel）。
    fn cgimage_to_rgba(image: *const c_void) -> Result<CapturedRgba, String> {
        unsafe {
            let (w, h) = (CGImageGetWidth(image), CGImageGetHeight(image));
            if w == 0 || h == 0 {
                return Err("SCK returned empty image".into());
            }
            let mut buf = vec![0u8; w * h * 4];
            let space = CGColorSpaceCreateDeviceRGB();
            if space.is_null() {
                return Err("CGColorSpaceCreateDeviceRGB failed".into());
            }
            let ctx = CGBitmapContextCreate(
                buf.as_mut_ptr().cast(),
                w,
                h,
                8,
                w * 4,
                space,
                RGBA_BITMAP_INFO,
            );
            if ctx.is_null() {
                CGColorSpaceRelease(space);
                return Err("CGBitmapContextCreate failed".into());
            }
            let rect = CGRect {
                origin: CGPoint { x: 0.0, y: 0.0 },
                size: CGSize {
                    width: w as f64,
                    height: h as f64,
                },
            };
            CGContextDrawImage(ctx, rect, image);
            CGContextRelease(ctx);
            CGColorSpaceRelease(space);
            Ok((buf, w as u32, h as u32))
        }
    }

    /// SCK 单帧采集：getShareableContent → 按 CGDirectDisplayID 匹配 SCDisplay →
    /// SCScreenshotManager.captureImage → blit RGBA → channel 交付。
    fn sck_capture(cgid: u32, pw: u32, ph: u32) -> Result<CapturedRgba, CapError> {
        let (tx, rx) = mpsc::sync_channel::<Result<CapturedRgba, String>>(2);

        let content_block = RcBlock::new(
            move |content: *mut SCShareableContent, error: *mut NSError| {
                if content.is_null() {
                    let msg = unsafe { error.as_ref() }
                        .map(|e| e.localizedDescription().to_string())
                        .unwrap_or_else(|| "no shareable content".into());
                    let _ = tx.try_send(Err(format!(
                        "SCShareableContent failed: {msg} (check screen-capture authorization for aura-node)"
                    )));
                    return;
                }
                let content = unsafe { &*content };
                let displays = unsafe { content.displays() };
                // 优先按 CG 枚举的 displayID 精确匹配；但 DeskPad 虚拟屏在 CGGetOnlineDisplayList 与
                // SCShareableContent 两侧的 ID 空间未必同源（虚拟屏编号不一致），精确匹配不到时回退到
                // SCK 自己列表的首个显示器——无头单虚拟屏场景即唯一目标，天然正确。仅空列表才真失败，
                // 错误携可见 SCK id 供诊断。
                let display = displays
                    .iter()
                    .find(|d| unsafe { d.displayID() } == cgid)
                    .or_else(|| displays.iter().next());
                let Some(display) = display else {
                    let ids: Vec<u32> = displays.iter().map(|d| unsafe { d.displayID() }).collect();
                    let _ = tx.try_send(Err(format!(
                        "SCK has no shareable displays (requested cg id={cgid}, sck ids={ids:?})"
                    )));
                    return;
                };
                let filter = unsafe {
                    SCContentFilter::initWithDisplay_excludingWindows(
                        SCContentFilter::alloc(),
                        &display,
                        &NSArray::<SCWindow>::new(),
                    )
                };
                let config = unsafe { SCStreamConfiguration::new() };
                unsafe {
                    config.setWidth(pw as usize);
                    config.setHeight(ph as usize);
                    config.setShowsCursor(true);
                }
                let tx2 = tx.clone();
                let image_block =
                    RcBlock::new(move |image: *mut CGImage, error: *mut NSError| {
                        if image.is_null() {
                            let msg = unsafe { error.as_ref() }
                                .map(|e| e.localizedDescription().to_string())
                                .unwrap_or_else(|| "captureImage returned nil".into());
                            let _ = tx2.try_send(Err(format!("SCK captureImage failed: {msg}")));
                            return;
                        }
                        let _ = tx2.try_send(cgimage_to_rgba(image.cast_const().cast()));
                    });
                unsafe {
                    SCScreenshotManager::captureImageWithFilter_configuration_completionHandler(
                        &filter,
                        &config,
                        Some(&image_block),
                    );
                }
            },
        );
        unsafe {
            SCShareableContent::getShareableContentExcludingDesktopWindows_onScreenWindowsOnly_completionHandler(
                false,
                false,
                &content_block,
            );
        }

        rx.recv_timeout(Duration::from_secs(15))
            .map_err(|_| CapError::CaptureFailed("SCK capture timed out (15s)".into()))?
            .map_err(CapError::CaptureFailed)
    }
}

/// Windows 采集后端：windows-capture（WGC）。回调式采集经 sync_channel 收敛为同步单帧。
#[cfg(windows)]
mod backend {
    use std::sync::mpsc::{sync_channel, SyncSender};

    use aura_capability::{CapError, DisplayInfo};
    use windows::Win32::Foundation::RECT;
    use windows::Win32::Graphics::Gdi::{GetMonitorInfoW, HMONITOR, MONITORINFO, MONITORINFOEXW};
    use windows::Win32::UI::HiDpi::{GetDpiForMonitor, MDT_EFFECTIVE_DPI};
    use windows_capture::capture::{Context, GraphicsCaptureApiHandler};
    use windows_capture::frame::Frame;
    use windows_capture::graphics_capture_api::InternalCaptureControl;
    use windows_capture::monitor::Monitor;
    use windows_capture::settings::{
        ColorFormat, CursorCaptureSettings, DirtyRegionSettings, DrawBorderSettings,
        MinimumUpdateIntervalSettings, SecondaryWindowSettings, Settings,
    };

    use super::CapturedRgba;

    /// 把任意实现 Display 的 WGC 错误折叠为 `CapError::CaptureFailed`。
    fn wgc_err<E: std::fmt::Display>(e: E) -> CapError {
        CapError::CaptureFailed(e.to_string())
    }

    /// 按 0 基序号（枚举顺序）选取显示器，与非 Windows 后端语义一致。
    fn pick_monitor(index: Option<u32>) -> Result<Monitor, CapError> {
        let monitors = Monitor::enumerate().map_err(wgc_err)?;
        let idx = index.unwrap_or(0) as usize;
        monitors
            .into_iter()
            .nth(idx)
            .ok_or_else(|| CapError::InvalidArg(format!("display index {idx} out of range")))
    }

    /// 以 WGC `Monitor` 内含的 HMONITOR 直调 Win32，取显示器元数据真值 (x, y, scale)：
    /// `GetMonitorInfoW` → `MONITORINFOEXW.rcMonitor.left/top` = 虚拟屏坐标（primary 恒 (0,0)
    /// 为 Windows 坐标系约定）；`GetDpiForMonitor(MDT_EFFECTIVE_DPI)` → scale = dpi/96.0
    /// （RDP 会话常见 96dpi → 1.0 属正常值）。windows-capture 2.0 的 `Monitor` 是 HMONITOR
    /// 纯包装（`as_raw_hmonitor()` 外露句柄）但不暴露位置/DPI，故在句柄层补齐——同句柄直调
    /// 免重新枚举，不存在 device name 对齐失配面。调用失败回退占位 (0, 0, 1.0) 并 stderr
    /// 留痕（fallback 显式设计，禁 panic；platform 层无 tracing 为既有现状，不在此引入）。
    pub(super) fn monitor_truth(m: &Monitor) -> (i32, i32, f64) {
        let hmon = HMONITOR(m.as_raw_hmonitor());

        let mut info = MONITORINFOEXW {
            monitorInfo: MONITORINFO {
                cbSize: std::mem::size_of::<MONITORINFOEXW>() as u32,
                rcMonitor: RECT::default(),
                rcWork: RECT::default(),
                dwFlags: 0,
            },
            szDevice: [0; 32],
        };
        // SAFETY: hmon 为 windows-capture 枚举所得的有效句柄；info 以 MONITORINFOEXW 全尺寸
        // cbSize 初始化后 cast 至 MONITORINFO*，是该 API 文档化的扩展体调用形态。
        let ok = unsafe { GetMonitorInfoW(hmon, &mut info as *mut MONITORINFOEXW as *mut MONITORINFO) };
        let (x, y) = if ok.as_bool() {
            (info.monitorInfo.rcMonitor.left, info.monitorInfo.rcMonitor.top)
        } else {
            eprintln!("aura-platform screen: GetMonitorInfoW failed, fallback x/y = (0, 0)");
            (0, 0)
        };

        let (mut dpi_x, mut dpi_y) = (0u32, 0u32);
        // SAFETY: 出参为栈上有效 u32 指针；失败时不读出参。
        let dpi_ok = unsafe { GetDpiForMonitor(hmon, MDT_EFFECTIVE_DPI, &mut dpi_x, &mut dpi_y) };
        let _ = dpi_y; // 有效 DPI x/y 恒相等，scale 取 x 轴单值
        let scale = match dpi_ok {
            Ok(()) if dpi_x > 0 => f64::from(dpi_x) / 96.0,
            _ => {
                eprintln!("aura-platform screen: GetDpiForMonitor failed, fallback scale = 1.0");
                1.0
            }
        };

        (x, y, scale)
    }

    /// WGC `Monitor` → `DisplayInfo`。`id` 为枚举序号；is_primary 与 primary 显示器序号比对得出。
    /// x/y/scale 经 [`monitor_truth`] 以 HMONITOR 直调 Win32 取真值，与非 Windows xcap 后端语义
    /// 对齐（单显 primary x/y=(0,0)、96dpi scale=1.0 与旧占位数值巧合相同——判据看取值路径）。
    /// 多显示器实机验收 fragile（252.20 headless 无多显），逻辑正确性由 API 语义单测背书。
    fn monitor_to_info(
        id: u32,
        m: &Monitor,
        primary_idx: Option<usize>,
    ) -> Result<DisplayInfo, CapError> {
        let idx = m.index().map_err(wgc_err)?;
        let (x, y, scale) = monitor_truth(m);
        Ok(DisplayInfo {
            id,
            name: m.name().map_err(wgc_err)?,
            x,
            y,
            width: m.width().map_err(wgc_err)?,
            height: m.height().map_err(wgc_err)?,
            is_primary: primary_idx == Some(idx),
            scale,
        })
    }

    /// 单帧抓取回调：首帧转 (RGBA8, w, h) 送 channel 后立即 stop（只需一帧）。
    struct FrameGrabber {
        tx: SyncSender<Result<CapturedRgba, String>>,
        sent: bool,
    }

    impl GraphicsCaptureApiHandler for FrameGrabber {
        type Flags = SyncSender<Result<CapturedRgba, String>>;
        type Error = Box<dyn std::error::Error + Send + Sync>;

        fn new(ctx: Context<Self::Flags>) -> Result<Self, Self::Error> {
            Ok(Self {
                tx: ctx.flags,
                sent: false,
            })
        }

        fn on_frame_arrived(
            &mut self,
            frame: &mut Frame,
            capture_control: InternalCaptureControl,
        ) -> Result<(), Self::Error> {
            if !self.sent {
                self.sent = true;
                let _ = self.tx.try_send(grab_frame(frame));
            }
            // 仅需首帧：抓到即停，start() 随之返回。
            capture_control.stop();
            Ok(())
        }
    }

    /// 从 WGC 帧提取去行填充（stride）的紧凑 RGBA8 缓冲。ColorFormat::Rgba8 已请求 RGBA 序，无需换序。
    fn grab_frame(frame: &mut Frame) -> Result<CapturedRgba, String> {
        let fb = frame.buffer().map_err(|e| e.to_string())?;
        let (w, h) = (fb.width(), fb.height());
        let mut scratch = Vec::new();
        let pixels = fb.as_nopadding_buffer(&mut scratch);
        Ok((pixels.to_vec(), w, h))
    }

    /// 采集指定显示器整屏为 (RGBA8, w, h)。start() 阻塞当前（spawn_blocking）线程跑 WGC 消息循环，
    /// 首帧回调 stop 后返回；随后 try_recv 取回该帧（同步单帧封装）。
    pub(super) fn capture_monitor_rgba(index: Option<u32>) -> Result<CapturedRgba, CapError> {
        let monitor = pick_monitor(index)?;
        let (tx, rx) = sync_channel::<Result<CapturedRgba, String>>(1);
        // Cursor/Border 均取 Default：显式请求（WithoutCursor/WithoutBorder）需要较新的
        // WGC config API，目标机在 RDP 会话下返回 BorderConfigUnsupported；Default 不发
        // config 请求、兼容性最大，且光标可见对 computer-use 观察鼠标位置反而有利。
        let settings = Settings::new(
            monitor,
            CursorCaptureSettings::Default,
            DrawBorderSettings::Default,
            SecondaryWindowSettings::Default,
            MinimumUpdateIntervalSettings::Default,
            DirtyRegionSettings::Default,
            ColorFormat::Rgba8,
            tx,
        );
        FrameGrabber::start(settings)
            .map_err(|e| CapError::CaptureFailed(format!("WGC capture failed: {e:?}")))?;
        rx.try_recv()
            .map_err(|_| CapError::CaptureFailed("WGC produced no frame".to_string()))?
            .map_err(CapError::CaptureFailed)
    }

    pub(super) fn list_monitors() -> Result<Vec<DisplayInfo>, CapError> {
        let monitors = Monitor::enumerate().map_err(wgc_err)?;
        let primary_idx = Monitor::primary().ok().and_then(|m| m.index().ok());
        monitors
            .iter()
            .enumerate()
            .map(|(idx, m)| monitor_to_info(idx as u32, m, primary_idx))
            .collect()
    }

    pub(super) fn get_monitor_info(index: u32) -> Result<DisplayInfo, CapError> {
        let monitors = Monitor::enumerate().map_err(wgc_err)?;
        let m = monitors
            .get(index as usize)
            .ok_or_else(|| CapError::InvalidArg(format!("display index {index} out of range")))?;
        let primary_idx = Monitor::primary().ok().and_then(|mm| mm.index().ok());
        monitor_to_info(index, m, primary_idx)
    }
}

#[cfg(test)]
mod tests {
    // super::* 已带入 BASE64_STANDARD 与 Engine（下方 decode 校验用）。
    use super::*;

    /// 合成 RGBA8 渐变缓冲（w*h*4 字节），供无显示器环境下的流水线单测。
    /// 像素 = [x%256, y%256, 128, 255]。
    fn synth_rgba(w: u32, h: u32) -> Vec<u8> {
        let mut v = Vec::with_capacity((w as usize) * (h as usize) * 4);
        for y in 0..h {
            for x in 0..w {
                v.extend_from_slice(&[(x % 256) as u8, (y % 256) as u8, 128, 255]);
            }
        }
        v
    }

    #[test]
    fn encode_scaled_downsamples_and_reports_scale() {
        // 1600x900（长边 1600 > 1280）→ 等比 0.8 → 1280x720，scale = 1600/1280 = 1.25。
        let r = encode_scaled(synth_rgba(1600, 900), 1600, 900, MAX_DIM, WEBP_QUALITY).unwrap();
        assert_eq!(r.meta.native_w, 1600);
        assert_eq!(r.meta.native_h, 900);
        assert_eq!(r.meta.display_w, 1280);
        assert_eq!(r.meta.display_h, 720);
        assert!((r.meta.scale - 1.25).abs() < 1e-9, "scale = {}", r.meta.scale);
        assert_eq!(r.mime, "image/webp");
        assert!(!r.image_base64.is_empty());
    }

    #[test]
    fn encode_scaled_keeps_images_within_limit() {
        // 800x600（长边 < 1280）不缩放，scale = 1.0。
        let r = encode_scaled(synth_rgba(800, 600), 800, 600, MAX_DIM, WEBP_QUALITY).unwrap();
        assert_eq!(r.meta.native_w, 800);
        assert_eq!(r.meta.display_w, 800);
        assert_eq!(r.meta.display_h, 600);
        assert!((r.meta.scale - 1.0).abs() < 1e-9);
    }

    #[test]
    fn webp_1280x720_under_200kb() {
        // 合成 1280x720 图 WebP q80 体积应远小于 200KB（base64 长度 * 3/4 ≈ 原始字节）。
        let r = encode_scaled(synth_rgba(1280, 720), 1280, 720, MAX_DIM, WEBP_QUALITY).unwrap();
        let approx_bytes = r.image_base64.len() * 3 / 4;
        assert!(approx_bytes < 200 * 1024, "webp too large: {approx_bytes} bytes");
    }

    #[test]
    fn region_two_corner_to_xywh() {
        // [x1,y1,x2,y2] = [100,50,300,250] → 左上 (100,50) + 尺寸 200x200。
        let region = Region { x1: 100, y1: 50, x2: 300, y2: 250 };
        assert_eq!(region_to_xywh(region).unwrap(), (100, 50, 200, 200));
    }

    #[test]
    fn region_rejects_inverted_corners() {
        // 右下角不大于左上角 → InvalidArg。
        let region = Region { x1: 300, y1: 250, x2: 100, y2: 50 };
        assert!(region_to_xywh(region).is_err());
    }

    #[test]
    fn resize_rgba_produces_correct_buffer_len() {
        let out = resize_rgba(synth_rgba(400, 200), 400, 200, 200, 100).unwrap();
        assert_eq!(out.len(), (200 * 100 * 4) as usize);
    }

    #[test]
    fn crop_rgba_extracts_region() {
        // 4x4 图裁 (1,1) 起 2x2；synth 像素 = [x, y, 128, 255]，故左上 = (1,1)。
        let img = synth_rgba(4, 4);
        let out = crop_rgba(&img, 4, 4, 1, 1, 2, 2).unwrap();
        assert_eq!(out.len(), 2 * 2 * 4);
        assert_eq!(&out[0..4], &[1u8, 1, 128, 255]);
    }

    #[test]
    fn crop_rgba_rejects_out_of_bounds() {
        let img = synth_rgba(4, 4);
        assert!(crop_rgba(&img, 4, 4, 3, 3, 4, 4).is_err());
    }

    #[test]
    fn screenshot_encodes_valid_webp_base64() {
        // 解码 base64 应还原出 WebP 容器（RIFF....WEBP），证明编码链正确。
        let r = encode_scaled(synth_rgba(64, 48), 64, 48, MAX_DIM, WEBP_QUALITY).unwrap();
        let bytes = BASE64_STANDARD.decode(&r.image_base64).unwrap();
        assert_eq!(&bytes[0..4], b"RIFF");
        assert_eq!(&bytes[8..12], b"WEBP");
    }

    /// M11 REC-2 verification：缺省 quality/max_dim 输出与旧常量路径逐字节相等。
    /// 基线指纹（len/head64/tail64）为参数化改造**之前**的版本在构建机 dump 所得
    /// （evidence/m11/t2-baseline-sha.txt），同输入像素下任何默认值漂移都会在此失配。
    #[test]
    fn default_encode_bytes_unchanged() {
        let r = encode_scaled(synth_rgba(1600, 900), 1600, 900, MAX_DIM, WEBP_QUALITY).unwrap();
        let b = &r.image_base64;
        assert_eq!(b.len(), 14016);
        assert_eq!(
            &b[..64],
            "UklGRgYpAABXRUJQVlA4IPooAACw3wGdASoABdACPm02mEimIyKqJGgBQA2JY27h"
        );
        assert_eq!(
            &b[b.len() - 64..],
            "K5qTw4HIGMeTf3zqejbYoOenIlTiWcbQQtbDFmfsYA5IvMGM8MyMNUjFmAkgAA=="
        );
        // 负对照：quality 参数真实生效（q40 与 q80 产出必然不同）。
        let low = encode_scaled(synth_rgba(1600, 900), 1600, 900, MAX_DIM, 40.0).unwrap();
        assert_ne!(low.image_base64, r.image_base64);
    }

    /// M11 T5：WGC 显示器元数据真值的 API 语义单测。多显示器实机验收 fragile
    /// （252.20 headless 无多显示器），逻辑正确性以此处语义不变式背书：
    /// 枚举非空、scale 合理域、primary 至多一（序号可解析时恰一）、多显时 x/y 互异。
    /// 非交互 ssh 会话下枚举得虚拟显示器（WinDisc，实测 primary/0,0/96dpi）——断言全量可跑；
    /// 设备名非 `\\.\DISPLAYn` 格式时既有 index() 解析失败致 is_primary 全 false，属既有语义。
    #[cfg(windows)]
    #[test]
    fn monitor_metadata_semantics() {
        use windows_capture::monitor::Monitor;

        let monitors = match Monitor::enumerate() {
            Ok(m) if !m.is_empty() => m,
            other => {
                eprintln!("skip monitor_metadata_semantics: no monitor in this session ({other:?})");
                return;
            }
        };

        // 真值取值路径（GetMonitorInfoW + GetDpiForMonitor）语义：scale 合理域 [0.5, 4.0]；
        // 多显时至少一台不在虚拟屏原点（各显示器 rcMonitor 拓扑互异）。
        let truths: Vec<(i32, i32, f64)> = monitors.iter().map(backend::monitor_truth).collect();
        for &(_, _, scale) in &truths {
            assert!((0.5..=4.0).contains(&scale), "scale {scale} out of sane range");
        }
        if truths.len() > 1 {
            assert!(
                truths.iter().any(|&(x, y, _)| x != 0 || y != 0),
                "multi-monitor: expected at least one non-origin x/y, got {truths:?}"
            );
        }

        // 完整 DisplayInfo 链。虚拟显示器设备名非 `\\.\DISPLAYn` 时既有 index() 解析失败
        // （ssh 会话 WinDisc 实测）、name()（DisplayConfig）亦可能 NameNotFound——皆属既有
        // 路径而非本次真值取值，跳过后半段不掩盖前半段断言（真实 RDP/console 会话可跑全量）。
        match backend::list_monitors() {
            Ok(infos) => {
                assert_eq!(infos.len(), monitors.len());
                let primaries = infos.iter().filter(|i| i.is_primary).count();
                assert!(primaries <= 1, "expected at most one primary, got {primaries}");
                if Monitor::primary().ok().and_then(|m| m.index().ok()).is_some() {
                    assert_eq!(primaries, 1, "primary index resolvable but {primaries} marked");
                }
                for (info, &(x, y, scale)) in infos.iter().zip(&truths) {
                    assert_eq!((info.x, info.y), (x, y), "DisplayInfo x/y != monitor_truth");
                    assert!((info.scale - scale).abs() < 1e-9, "DisplayInfo scale != monitor_truth");
                }
            }
            Err(e) => eprintln!("skip DisplayInfo-chain assertions (pre-existing index/name path): {e:?}"),
        }
    }
}
