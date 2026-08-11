//! doctor 子命令（M17 T1）：本地全能力自检——把「节点在线 ≠ 能力可用」的静默降级面翻出来。
//!
//! 背景：真机 e2e 实证过截图后端故障（WGC E_CAPTURE_FAILED）、锁屏黑帧、TCC 未授权等
//! 环境性故障下节点其余功能正常、fleet 面仍「在线」，agent 视角无从分辨，只能靠人肉
//! ground-truth 核验。doctor 逐项实测能力通路并分级，一次性跑完即退。
//!
//! 检查项与分级：
//!   - display / screenshot / a11y / input 为核心闭环能力：实测失败 → `degraded`；
//!   - audio / process 为辅助能力：实测失败 → `warn`；
//!   - `E_UNSUPPORTED` 与广告面已剔（`supports_tool` false）→ `skipped`（平台矩阵预期，非故障）；
//!   - 截图成功但近单色（锁屏黑帧 / 无信号疑点）→ `degraded`；a11y 空树 → `warn`。
//! 退出码：存在 degraded → 1（脚本/CI 可判），否则 0。
//!
//! 探针无副作用纪律：输入探针用单发 "shift"（无字符/焦点副作用，真实走 enigo/adb 注入通路）；
//! 音频探针用运行时生成的 100ms 静音 WAV（无可闻副作用）。type/click/录屏等有副作用面不实测。

use std::io::Write as _;
use std::sync::Arc;
use std::time::{Duration, Instant};

use anyhow::Result;
use clap::Args;
use serde::Serialize;

use aura_capability::{
    decode_webp_rgb, A11yNode, A11yParams, A11yTree, AudioInjectParams, CapError, DeviceDriver,
    DisplayInfo, ProcessInfo, ScreenshotOpts, ScreenshotResult,
};

/// 单项检查超时：截图/取树/adb 往返正常在数秒内，超过视为该能力通路卡死（同样是故障信号）。
const CHECK_TIMEOUT: Duration = Duration::from_secs(10);

/// 近单色判定阈值：解码后 RGB 全体字节 max-min 极差 ≤ 此值视为整屏单色。
/// WebP 有损压缩对纯色帧引入的噪声实测在 ±2 内，取 4 留余量；正常桌面（含任务栏/dock）极差远大于此。
const MONOCHROME_RANGE: u8 = 4;

/// 能力自检参数。
#[derive(Args, Clone, Debug)]
pub struct DoctorArgs {
    /// 输出 JSON 结构化报告（默认人类可读文本）。
    #[arg(long)]
    pub json: bool,
    /// 跳过输入注入探针（单发 shift，无副作用；桌面真实走 enigo 通路，可暴露 TCC 未授权类故障）。
    #[arg(long)]
    pub skip_input: bool,
    /// 跳过音频注入探针（100ms 静音 WAV，无可闻副作用；仅 Linux 桌面广告此能力）。
    #[arg(long)]
    pub skip_audio: bool,
    /// 指定探测的显示器 id（缺省当前活动显示器）。
    #[arg(long)]
    pub display: Option<u32>,
}

/// 检查分级（JSON 报告序列化为小写）。
#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize)]
#[serde(rename_all = "lowercase")]
enum Status {
    Ok,
    Warn,
    Degraded,
    Skipped,
}

impl Status {
    fn label(self) -> &'static str {
        match self {
            Status::Ok => "ok",
            Status::Warn => "warn",
            Status::Degraded => "degraded",
            Status::Skipped => "skipped",
        }
    }
}

#[derive(Serialize)]
struct CheckOutcome {
    name: &'static str,
    status: Status,
    detail: String,
    latency_ms: u64,
}

#[derive(Serialize)]
struct DoctorReport {
    version: &'static str,
    driver: &'static str,
    host_os: &'static str,
    healthy: bool,
    checks: Vec<CheckOutcome>,
}

/// 执行全部检查并输出报告。返回 `healthy`（无 degraded）；调用方据此定进程退出码。
pub async fn run(driver: Arc<dyn DeviceDriver>, args: DoctorArgs) -> Result<bool> {
    let d = &*driver;
    let mut checks = Vec::new();

    checks.push(
        probe(d, "display", "list_displays", true, d.list_displays(), judge_displays).await,
    );
    checks.push(
        probe(
            d,
            "screenshot",
            "screenshot",
            true,
            d.screenshot(args.display, ScreenshotOpts::default()),
            judge_screenshot,
        )
        .await,
    );
    checks.push(
        probe(d, "a11y", "get_a11y_tree", true, d.get_a11y_tree(A11yParams::default()), judge_a11y)
            .await,
    );
    checks.push(if args.skip_input {
        skipped("input", "skipped by --skip-input")
    } else {
        probe(d, "input", "key", true, d.key("shift".to_string()), |_| {
            (Status::Ok, "key 'shift' injected (side-effect-free probe)".to_string())
        })
        .await
    });
    checks.push(if args.skip_audio {
        skipped("audio", "skipped by --skip-audio")
    } else {
        probe(
            d,
            "audio",
            "audio_inject",
            false,
            d.audio_inject(AudioInjectParams {
                wav_base64: Some(silent_wav_base64()),
                file: None,
            }),
            |_| (Status::Ok, "100ms silent wav played".to_string()),
        )
        .await
    });
    checks.push(
        probe(d, "process", "list_processes", false, d.list_processes(), judge_processes).await,
    );

    let healthy = !checks.iter().any(|c| c.status == Status::Degraded);
    let report = DoctorReport {
        version: env!("CARGO_PKG_VERSION"),
        driver: driver.platform(),
        host_os: std::env::consts::OS,
        healthy,
        checks,
    };

    if args.json {
        println!("{}", serde_json::to_string_pretty(&report)?);
    } else {
        print_human(&report);
    }
    // 一次性命令：确保报告落 stdout 后再由调用方定退出码。
    std::io::stdout().flush().ok();
    Ok(healthy)
}

/// 非侵入巡检子集（M17 T1b 舰队健康）：display / screenshot / a11y 三核心探针，供反连层
/// 周期自检消费。input/audio/process 不巡检——input 有注入语义（巡检须绝对无侵入）、audio 有
/// 播放开销、process 枚举对舰队健康无增量信号。返回 (能力名, 分级标签, 诊断文本) 三元组，
/// 调用方（grpc_reverse）转 pb::CapabilityHealth 随心跳上报。分级语义与 doctor 完全一致。
pub async fn passive_probe(driver: &dyn DeviceDriver) -> Vec<(&'static str, &'static str, String)> {
    let mut checks = Vec::with_capacity(3);
    checks.push(
        probe(driver, "display", "list_displays", true, driver.list_displays(), judge_displays)
            .await,
    );
    checks.push(
        probe(
            driver,
            "screenshot",
            "screenshot",
            true,
            driver.screenshot(None, ScreenshotOpts::default()),
            judge_screenshot,
        )
        .await,
    );
    checks.push(
        probe(driver, "a11y", "get_a11y_tree", true, driver.get_a11y_tree(A11yParams::default()), judge_a11y)
            .await,
    );
    checks.into_iter().map(|c| (c.name, c.status.label(), c.detail)).collect()
}

/// 单项检查统一封装：广告面先决（`supports_tool` false → skipped 不实测）→ 超时 → 错误分级 → 成败判读。
/// `core` 区分核心闭环能力（失败 degraded）与辅助能力（失败 warn）。
async fn probe<T, Fut>(
    driver: &dyn DeviceDriver,
    name: &'static str,
    tool: &'static str,
    core: bool,
    fut: Fut,
    judge: impl FnOnce(&T) -> (Status, String),
) -> CheckOutcome
where
    Fut: std::future::Future<Output = Result<T, CapError>>,
{
    if !driver.supports_tool(tool) {
        return skipped(name, "not advertised on this driver");
    }
    let start = Instant::now();
    let (status, detail) = match tokio::time::timeout(CHECK_TIMEOUT, fut).await {
        Err(_) => (
            fail_status(core),
            format!("timed out after {}s (capability path hung)", CHECK_TIMEOUT.as_secs()),
        ),
        Ok(Err(e)) => classify_error(core, &e),
        Ok(Ok(v)) => judge(&v),
    };
    CheckOutcome { name, status, detail, latency_ms: start.elapsed().as_millis() as u64 }
}

fn skipped(name: &'static str, detail: &str) -> CheckOutcome {
    CheckOutcome { name, status: Status::Skipped, detail: detail.to_string(), latency_ms: 0 }
}

fn fail_status(core: bool) -> Status {
    if core {
        Status::Degraded
    } else {
        Status::Warn
    }
}

/// 错误 → 分级：`E_UNSUPPORTED` 是平台矩阵预期（skipped）；`E_NO_AUDIO_DEV` 是环境缺配疑点
/// （warn 并给排障提示，不掩盖——广告面承诺了该能力）；其余按 core/辅助落 degraded/warn。
fn classify_error(core: bool, e: &CapError) -> (Status, String) {
    match e {
        CapError::Unsupported(_) => (Status::Skipped, format!("not supported: {e}")),
        CapError::NoAudioDev(_) => (
            Status::Warn,
            format!("audio backend unavailable: {e} (PulseAudio unreachable / XDG_RUNTIME_DIR missing?)"),
        ),
        _ => (fail_status(core), format!("probe failed: {e}")),
    }
}

fn judge_displays(displays: &Vec<DisplayInfo>) -> (Status, String) {
    let Some(primary) = displays.iter().find(|d| d.is_primary).or_else(|| displays.first()) else {
        return (Status::Degraded, "no displays enumerated".to_string());
    };
    if primary.width == 0 || primary.height == 0 {
        return (
            Status::Degraded,
            format!("display #{} reports zero size ({}x{})", primary.id, primary.width, primary.height),
        );
    }
    (
        Status::Ok,
        format!(
            "{} display(s); primary #{} {}x{} scale {:.2}",
            displays.len(),
            primary.id,
            primary.width,
            primary.height,
            primary.scale
        ),
    )
}

fn judge_screenshot(shot: &ScreenshotResult) -> (Status, String) {
    if shot.meta.display_w == 0 || shot.meta.display_h == 0 {
        return (
            Status::Degraded,
            format!("zero-size capture ({}x{})", shot.meta.display_w, shot.meta.display_h),
        );
    }
    let approx_kb = shot.image_base64.len() * 3 / 4 / 1024;
    match decode_webp_rgb(&shot.image_base64) {
        Err(e) => (Status::Degraded, format!("capture returned undecodable image: {e}")),
        Ok(img) => judge_screenshot_pixels(img.w, img.h, &img.data, approx_kb),
    }
}

/// 截图像素判读（解码后）：近单色帧 = 锁屏黑帧 / 无信号 / 采集故障疑点 → degraded。
/// 与解码拆开为纯函数，便于单测直接构造像素而不依赖 WebP 编码器。
fn judge_screenshot_pixels(w: usize, h: usize, rgb: &[u8], approx_kb: usize) -> (Status, String) {
    let range = byte_range(rgb);
    if range <= MONOCHROME_RANGE {
        (
            Status::Degraded,
            format!(
                "{w}x{h} captured but near-uniform (byte range {range}) — screen may be locked/blank/no-signal"
            ),
        )
    } else {
        (Status::Ok, format!("{w}x{h} WebP ~{approx_kb}KB; byte range {range}"))
    }
}

fn judge_a11y(tree: &A11yTree) -> (Status, String) {
    let total = count_nodes(&tree.nodes);
    if total == 0 {
        return (
            Status::Warn,
            "a11y tree is empty (no windows, or accessibility permission denied?)".to_string(),
        );
    }
    (
        Status::Ok,
        format!("{total} node(s), {} top-level, truncated={}", tree.nodes.len(), tree.truncated),
    )
}

fn judge_processes(procs: &Vec<ProcessInfo>) -> (Status, String) {
    if procs.is_empty() {
        return (Status::Warn, "no processes enumerated".to_string());
    }
    (Status::Ok, format!("{} process(es)", procs.len()))
}

fn count_nodes(nodes: &[A11yNode]) -> usize {
    nodes.iter().map(|n| 1 + count_nodes(&n.children)).sum()
}

/// RGB 裸字节全体极差（max-min）：0 = 完全单色。O(n) 单遍，无分配。
fn byte_range(data: &[u8]) -> u8 {
    let mut lo = u8::MAX;
    let mut hi = u8::MIN;
    for &b in data {
        lo = lo.min(b);
        hi = hi.max(b);
    }
    hi.saturating_sub(lo)
}

/// 运行时生成 100ms 8kHz 16-bit mono 静音 WAV（44 字节标准头 + 全零数据）→ base64。
/// 零资产文件、无可闻副作用；Linux 上真实走 pactl/paplay 通路验证音频后端。
fn silent_wav_base64() -> String {
    use base64::prelude::{Engine as _, BASE64_STANDARD};
    const SAMPLE_RATE: u32 = 8000;
    const SAMPLES: u32 = SAMPLE_RATE / 10; // 100ms
    let data_len = SAMPLES * 2; // 16-bit mono
    let mut wav = Vec::with_capacity(44 + data_len as usize);
    wav.extend_from_slice(b"RIFF");
    wav.extend_from_slice(&(36 + data_len).to_le_bytes());
    wav.extend_from_slice(b"WAVEfmt ");
    wav.extend_from_slice(&16u32.to_le_bytes()); // fmt chunk 大小
    wav.extend_from_slice(&1u16.to_le_bytes()); // PCM
    wav.extend_from_slice(&1u16.to_le_bytes()); // mono
    wav.extend_from_slice(&SAMPLE_RATE.to_le_bytes());
    wav.extend_from_slice(&(SAMPLE_RATE * 2).to_le_bytes()); // byte rate
    wav.extend_from_slice(&2u16.to_le_bytes()); // block align
    wav.extend_from_slice(&16u16.to_le_bytes()); // bits per sample
    wav.extend_from_slice(b"data");
    wav.extend_from_slice(&data_len.to_le_bytes());
    wav.resize(44 + data_len as usize, 0);
    BASE64_STANDARD.encode(&wav)
}

fn print_human(report: &DoctorReport) {
    println!(
        "aura-node doctor — driver={} host_os={} version={}",
        report.driver, report.host_os, report.version
    );
    println!();
    for c in &report.checks {
        println!(
            "[{:<8}] {:<11} {}  ({}ms)",
            c.status.label(),
            c.name,
            c.detail,
            c.latency_ms
        );
    }
    println!();
    let count = |s: Status| report.checks.iter().filter(|c| c.status == s).count();
    println!(
        "summary: {} — {} ok, {} degraded, {} warn, {} skipped",
        if report.healthy { "HEALTHY" } else { "DEGRADED" },
        count(Status::Ok),
        count(Status::Degraded),
        count(Status::Warn),
        count(Status::Skipped),
    );
}

#[cfg(test)]
mod tests {
    use super::*;

    // ---- 静音 WAV 结构 ----

    #[test]
    fn silent_wav_is_valid_riff() {
        use base64::prelude::{Engine as _, BASE64_STANDARD};
        let wav = BASE64_STANDARD.decode(silent_wav_base64()).unwrap();
        assert_eq!(&wav[0..4], b"RIFF");
        assert_eq!(&wav[8..12], b"WAVE");
        // RIFF 声明长度 = 文件长 - 8（RIFF 头自身）。
        let riff_len = u32::from_le_bytes(wav[4..8].try_into().unwrap()) as usize;
        assert_eq!(riff_len, wav.len() - 8);
        // data 段全零（静音）。
        assert!(wav[44..].iter().all(|&b| b == 0));
        // 100ms @ 8kHz 16-bit mono = 1600 字节数据。
        assert_eq!(wav.len() - 44, 1600);
    }

    // ---- 极差与单色判定 ----

    #[test]
    fn byte_range_flat_and_varied() {
        assert_eq!(byte_range(&[0, 0, 0]), 0);
        assert_eq!(byte_range(&[10, 12, 11]), 2);
        assert_eq!(byte_range(&[0, 255]), 255);
        assert_eq!(byte_range(&[]), 0, "空数据极差为 0（防 0x0 图 panic）");
    }

    #[test]
    fn screenshot_pixels_monochrome_is_degraded() {
        // 全黑帧（锁屏黑帧形态）→ degraded。
        let black = vec![0u8; 30 * 20 * 3];
        let (status, detail) = judge_screenshot_pixels(30, 20, &black, 1);
        assert_eq!(status, Status::Degraded);
        assert!(detail.contains("near-uniform"), "detail={detail}");
        // 压缩噪声容差内（±2）仍判单色。
        let noisy: Vec<u8> = (0..30 * 20 * 3).map(|i| 40 + (i % 3) as u8).collect();
        assert_eq!(judge_screenshot_pixels(30, 20, &noisy, 1).0, Status::Degraded);
    }

    #[test]
    fn screenshot_pixels_normal_is_ok() {
        // 有内容帧（梯度）→ ok。
        let grad: Vec<u8> = (0..30 * 20 * 3).map(|i| (i % 256) as u8).collect();
        let (status, detail) = judge_screenshot_pixels(30, 20, &grad, 12);
        assert_eq!(status, Status::Ok);
        assert!(detail.contains("30x20"), "detail={detail}");
    }

    // ---- 错误分级 ----

    #[test]
    fn classify_unsupported_is_skipped() {
        let (s, _) = classify_error(true, &CapError::Unsupported("x".into()));
        assert_eq!(s, Status::Skipped);
    }

    #[test]
    fn classify_no_audio_dev_is_warn_with_hint() {
        let (s, d) = classify_error(false, &CapError::NoAudioDev("pactl missing".into()));
        assert_eq!(s, Status::Warn);
        assert!(d.contains("XDG_RUNTIME_DIR"), "排障提示须在 detail 内：{d}");
    }

    #[test]
    fn classify_real_error_by_core() {
        let e = CapError::CaptureFailed("wgc".into());
        assert_eq!(classify_error(true, &e).0, Status::Degraded);
        assert_eq!(classify_error(false, &e).0, Status::Warn);
    }

    // ---- 判读函数 ----

    #[test]
    fn judge_displays_empty_and_zero_size() {
        assert_eq!(judge_displays(&vec![]).0, Status::Degraded);
        let zero = DisplayInfo {
            id: 0,
            name: "d".into(),
            x: 0,
            y: 0,
            width: 0,
            height: 0,
            is_primary: true,
            scale: 1.0,
        };
        assert_eq!(judge_displays(&vec![zero]).0, Status::Degraded);
    }

    #[test]
    fn judge_a11y_empty_is_warn() {
        let empty = A11yTree { nodes: vec![], truncated: false };
        assert_eq!(judge_a11y(&empty).0, Status::Warn);
        let one = A11yTree {
            nodes: vec![A11yNode {
                role: "window".into(),
                name: "w".into(),
                bounds: None,
                value: None,
                children: vec![],
            }],
            truncated: false,
        };
        assert_eq!(judge_a11y(&one).0, Status::Ok);
    }
}
