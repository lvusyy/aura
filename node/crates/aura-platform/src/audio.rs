//! AudioDriver 平台实现：Linux PulseAudio/PipeWire null-sink 注入；Windows / macOS 结构化降级。
//!
//! 后端只此一层分平台（`#[cfg]` 门控，镜像 record.rs 布局），capability 层保持平台无关：
//!   - Linux：`pactl` 幂等确保 null-sink `aura_inject`（`pactl list short sinks` 按名列查 → 缺则
//!     `pactl load-module module-null-sink sink_name=aura_inject`）→ `paplay --device=aura_inject`
//!     子进程同步播放（播完返回即天然限速）。`wav_base64` 解码落唯一命名临时文件
//!     `<temp_dir>/aura-audio-<unix_ns>-<pid>.wav`（时间戳 + 进程 id 后缀——uuid 不在依赖树，
//!     零新 crate），用毕清理（含失败路径）；`file` 入参直接播节点本地路径（不删用户文件）。
//!     回采验证经 sink 的 monitor source（`aura_inject.monitor`，parecord）闭环，属 e2e 面。
//!   - Windows / macOS：无虚拟音频设备通路，恒返 E_NO_AUDIO_DEV（plan D6 结构化降级：广告面
//!     supports_tool 已剔，dispatch 超集网仍可派发，call-time 走本码；Win 虚拟声卡方案记 issue）。
//!
//! E_NO_AUDIO_DEV 触发条件（Locked-3）：pactl / paplay 二进制不在位、PulseAudio server 不可达
//! （pactl 非零退出）、load-module 失败。参数错误（二选一违约 / 文件缺失 / 非 RIFF/WAVE）与坏 WAV
//! （paplay 播放报错——server 已探活，播放失败多为内容问题）落 E_INVALID_ARG（既有码）。

use async_trait::async_trait;

use aura_capability::{AudioDriver, AudioInjectParams, AudioInjectResult, CapError};

use crate::PlatformDriver;

#[async_trait]
impl AudioDriver for PlatformDriver {
    async fn audio_inject(&self, params: AudioInjectParams) -> Result<AudioInjectResult, CapError> {
        // 参数契约平台无关：恰取其一 + file 存在性（E_INVALID_ARG 优先于平台能力判定，三平台同语义）。
        validate_params(&params)?;
        backend::audio_inject(params).await
    }
}

/// 入参契约校验（平台无关）：`wav_base64` 与 `file` 恰取其一；`file` 须指向存在的文件。
fn validate_params(params: &AudioInjectParams) -> Result<(), CapError> {
    match (params.wav_base64.as_deref(), params.file.as_deref()) {
        (None, None) | (Some(_), Some(_)) => Err(CapError::InvalidArg(
            "audio_inject requires exactly one of 'wav_base64' or 'file'".to_string(),
        )),
        (None, Some(file)) if !std::path::Path::new(file).is_file() => {
            Err(CapError::InvalidArg(format!("audio file not found: {file}")))
        }
        _ => Ok(()),
    }
}

/// WAV 头估算播放时长（毫秒）：RIFF/WAVE chunk 遍历，`fmt ` 取 byte_rate（chunk 数据区
/// offset 8..12）、`data` 取声明长度。头不完整 / byte_rate=0 → None（played_ms 省略，不阻断
/// 播放——估算是 best-effort，权威播放时长由 paplay 同步等待天然承载）。纯函数便于单测。
/// 非 Linux 平台仅测试消费（win/mac backend 直接降级不播放），豁免 dead_code 保三平台测试面。
#[cfg_attr(not(target_os = "linux"), allow(dead_code))]
fn wav_duration_ms(bytes: &[u8]) -> Option<u64> {
    if bytes.len() < 12 || &bytes[0..4] != b"RIFF" || &bytes[8..12] != b"WAVE" {
        return None;
    }
    let mut byte_rate: Option<u64> = None;
    let mut data_len: Option<u64> = None;
    let mut off = 12usize;
    while off + 8 <= bytes.len() {
        let id = &bytes[off..off + 4];
        let size = u32::from_le_bytes(bytes[off + 4..off + 8].try_into().ok()?) as u64;
        if id == b"fmt " && off + 20 <= bytes.len() {
            byte_rate = Some(u32::from_le_bytes(bytes[off + 16..off + 20].try_into().ok()?) as u64);
        } else if id == b"data" {
            // data 只需 header 声明长度（内容不读）——截断缓冲（如仅读文件头）亦可估算。
            data_len = Some(size);
        }
        if byte_rate.is_some() && data_len.is_some() {
            break;
        }
        // chunk 按 2 字节对齐推进；越界（截断头）即自然终止，用已收集要素判定。
        match off.checked_add(8 + size as usize + (size as usize & 1)) {
            Some(next) => off = next,
            None => break,
        }
    }
    match (byte_rate, data_len) {
        (Some(br), Some(dl)) if br > 0 => Some(dl * 1000 / br),
        _ => None,
    }
}

// ===== Linux 后端：pactl null-sink + paplay 子进程（M11 主线）=====

#[cfg(target_os = "linux")]
mod backend {
    use std::path::PathBuf;

    use base64::engine::general_purpose::STANDARD;
    use base64::Engine as _;
    use tokio::process::Command;

    use aura_capability::{AudioInjectParams, AudioInjectResult, CapError};

    /// 注入用 null-sink 名。幂等确保存在；agent 回采经其 monitor source `aura_inject.monitor`。
    const SINK_NAME: &str = "aura_inject";

    pub(super) async fn audio_inject(
        params: AudioInjectParams,
    ) -> Result<AudioInjectResult, CapError> {
        // 1) 先定型参数域错误（解码 / RIFF 校验，触 pactl 之前），wav 来源二选一（validate 已保证恰一）。
        let (wav_bytes, file_path) = match params.wav_base64.as_deref() {
            Some(b64) => {
                let bytes = STANDARD
                    .decode(b64.trim())
                    .map_err(|e| CapError::InvalidArg(format!("invalid base64 wav: {e}")))?;
                if bytes.len() < 12 || &bytes[0..4] != b"RIFF" || &bytes[8..12] != b"WAVE" {
                    return Err(CapError::InvalidArg(
                        "wav_base64 is not RIFF/WAVE data".to_string(),
                    ));
                }
                (Some(bytes), None)
            }
            None => {
                let file = params
                    .file
                    .clone()
                    .expect("validated: exactly one wav source");
                (None, Some(file))
            }
        };
        // 播放时长估算（best-effort）：内联用全字节；file 读头 4KB（data 长度是 header 声明值）。
        let played_ms = match (&wav_bytes, &file_path) {
            (Some(bytes), _) => super::wav_duration_ms(bytes),
            (None, Some(file)) => read_head_4k(file).as_deref().and_then(super::wav_duration_ms),
            _ => None,
        };

        // 2) 音频通路（E_NO_AUDIO_DEV 域）：幂等确保 null-sink。
        ensure_sink().await?;

        // 3) 内联字节落唯一命名临时文件（file 入参直接播，不落盘不清理）。
        let (wav_path, cleanup) = match (wav_bytes, file_path) {
            (Some(bytes), _) => {
                let path = temp_wav_path();
                tokio::fs::write(&path, &bytes)
                    .await
                    .map_err(|e| CapError::FileError(format!("write temp wav failed: {e}")))?;
                (path, true)
            }
            (None, Some(file)) => (PathBuf::from(file), false),
            _ => unreachable!("validated: exactly one wav source"),
        };

        // 4) paplay 同步等待播完；临时文件在成败两路径都先清理再定结果。
        let play = Command::new("paplay")
            .arg(format!("--device={SINK_NAME}"))
            .arg(&wav_path)
            .output()
            .await;
        if cleanup {
            let _ = tokio::fs::remove_file(&wav_path).await;
        }
        let out =
            play.map_err(|e| CapError::NoAudioDev(format!("paplay not available: {e}")))?;
        if !out.status.success() {
            // sink 已确保、server 已探活——此处失败多为坏 WAV / 编解码问题，归参数错误（结构化非 panic）。
            return Err(CapError::InvalidArg(format!(
                "paplay failed: {}",
                String::from_utf8_lossy(&out.stderr).trim()
            )));
        }
        Ok(AudioInjectResult { played_ms })
    }

    /// 幂等确保 null-sink 在位：`pactl list short sinks` 输出行 `<index>\t<name>\t...` 按名字列
    /// 精确匹配（contains 会误伤同前缀 sink），缺则 load-module。
    async fn ensure_sink() -> Result<(), CapError> {
        let list = run_pactl(&["list", "short", "sinks"]).await?;
        let exists = list
            .lines()
            .any(|l| l.split_whitespace().nth(1) == Some(SINK_NAME));
        if !exists {
            run_pactl(&[
                "load-module",
                "module-null-sink",
                &format!("sink_name={SINK_NAME}"),
            ])
            .await?;
        }
        Ok(())
    }

    /// 跑一条 pactl；spawn 失败（二进制不在位）与非零退出（server 不可达 / load-module 失败）统一
    /// E_NO_AUDIO_DEV（Locked-3 触发条件三合一：音频通路不可用是结构化可判定状态）。
    async fn run_pactl(args: &[&str]) -> Result<String, CapError> {
        let out = Command::new("pactl")
            .args(args)
            .output()
            .await
            .map_err(|e| CapError::NoAudioDev(format!("pactl not available: {e}")))?;
        if !out.status.success() {
            return Err(CapError::NoAudioDev(format!(
                "pactl {} failed: {}",
                args.join(" "),
                String::from_utf8_lossy(&out.stderr).trim()
            )));
        }
        Ok(String::from_utf8_lossy(&out.stdout).into_owned())
    }

    /// 唯一命名临时 wav：`<temp_dir>/aura-audio-<unix_ns>-<pid>.wav`（纳秒时间戳 + 进程 id——
    /// uuid 不在依赖树，零新 crate 红线；跨进程 pid 去重、同进程纳秒去重）。
    fn temp_wav_path() -> PathBuf {
        let ns = std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .map(|d| d.as_nanos())
            .unwrap_or(0);
        std::env::temp_dir().join(format!("aura-audio-{ns}-{}.wav", std::process::id()))
    }

    /// 读文件头 4KB（时长估算用，best-effort：失败 → None 不阻断播放）。
    fn read_head_4k(path: &str) -> Option<Vec<u8>> {
        use std::io::Read;
        let mut buf = vec![0u8; 4096];
        let mut f = std::fs::File::open(path).ok()?;
        let n = f.read(&mut buf).ok()?;
        buf.truncate(n);
        Some(buf)
    }
}

// ===== Windows / macOS 后端：结构化降级（plan D6，issue 记账归 TASK-009）=====

#[cfg(not(target_os = "linux"))]
mod backend {
    use aura_capability::{AudioInjectParams, AudioInjectResult, CapError};

    /// 无虚拟音频设备通路：广告面已剔（`supports_tool` 覆写 false 保 20）+ call-time 结构化
    /// E_NO_AUDIO_DEV 兜底——广告收窄与 call-time 语义成对，防「广告 vs call-time 错位」旧债复刻。
    pub(super) async fn audio_inject(
        _params: AudioInjectParams,
    ) -> Result<AudioInjectResult, CapError> {
        Err(CapError::NoAudioDev(
            "audio_inject requires a virtual audio device; not implemented on this platform"
                .to_string(),
        ))
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn params(wav_base64: Option<&str>, file: Option<&str>) -> AudioInjectParams {
        AudioInjectParams {
            wav_base64: wav_base64.map(str::to_string),
            file: file.map(str::to_string),
        }
    }

    /// 合成最小 PCM WAV 头（RIFF/WAVE + fmt(16B) + data header）：byte_rate 与 data 长度可控，
    /// 供时长估算确定性断言（data 内容不需要——估算只读 header 声明长度）。
    fn synth_wav_head(byte_rate: u32, data_len: u32) -> Vec<u8> {
        let mut v = Vec::new();
        v.extend_from_slice(b"RIFF");
        v.extend_from_slice(&(36 + data_len).to_le_bytes());
        v.extend_from_slice(b"WAVE");
        v.extend_from_slice(b"fmt ");
        v.extend_from_slice(&16u32.to_le_bytes()); // fmt chunk size（PCM 标准 16）
        v.extend_from_slice(&1u16.to_le_bytes()); // audio format = PCM
        v.extend_from_slice(&1u16.to_le_bytes()); // channels
        v.extend_from_slice(&8000u32.to_le_bytes()); // sample rate（估算不消费）
        v.extend_from_slice(&byte_rate.to_le_bytes()); // byte rate（估算分母）
        v.extend_from_slice(&2u16.to_le_bytes()); // block align
        v.extend_from_slice(&16u16.to_le_bytes()); // bits per sample
        v.extend_from_slice(b"data");
        v.extend_from_slice(&data_len.to_le_bytes());
        v
    }

    /// 二选一契约：双缺省 / 双给定 → E_INVALID_ARG（平台无关 validate 层，三平台同绿、不触后端）。
    #[tokio::test]
    async fn params_both_none_or_both_some_invalid_arg() {
        let d = PlatformDriver::new();
        assert_eq!(
            d.audio_inject(params(None, None)).await.unwrap_err().code(),
            "E_INVALID_ARG"
        );
        assert_eq!(
            d.audio_inject(params(Some("QQ=="), Some("/tmp/x.wav")))
                .await
                .unwrap_err()
                .code(),
            "E_INVALID_ARG"
        );
    }

    /// file 路径不存在 → E_INVALID_ARG（参数错误优先于平台能力判定，三平台同语义）。
    #[tokio::test]
    async fn params_missing_file_invalid_arg() {
        let d = PlatformDriver::new();
        let err = d
            .audio_inject(params(None, Some("/nonexistent/aura-audio-missing.wav")))
            .await
            .unwrap_err();
        assert_eq!(err.code(), "E_INVALID_ARG");
        assert!(err.to_string().contains("not found"), "err={err}");
    }

    /// Windows / macOS 结构化降级：合法入参 call-time 恒 E_NO_AUDIO_DEV（plan D6：广告剔除 +
    /// call-time 兜底同码，非 panic）。Linux 不适用（pactl 主线，行为依赖环境归 e2e）。
    #[cfg(not(target_os = "linux"))]
    #[tokio::test]
    async fn non_linux_backend_returns_no_audio_dev() {
        let d = PlatformDriver::new();
        let err = d.audio_inject(params(Some("QQ=="), None)).await.unwrap_err();
        assert_eq!(err.code(), "E_NO_AUDIO_DEV");
    }

    /// WAV 头时长估算：byte_rate/data 双要素齐 → data_len*1000/byte_rate；坏头 / byte_rate=0 → None。
    #[test]
    fn wav_duration_from_header() {
        // 16000 B/s × 32000 B data → 2000ms。
        assert_eq!(wav_duration_ms(&synth_wav_head(16_000, 32_000)), Some(2_000));
        // 截断到 data header 为止（无 data 内容）同样可估算——估算只读声明长度。
        let head = synth_wav_head(8_000, 4_000);
        assert_eq!(wav_duration_ms(&head), Some(500));
        // 非 RIFF / byte_rate=0 → None。
        assert_eq!(wav_duration_ms(b"not a wav"), None);
        assert_eq!(wav_duration_ms(&synth_wav_head(0, 4_000)), None);
    }
}
