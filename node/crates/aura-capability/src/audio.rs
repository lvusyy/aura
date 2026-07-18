//! 音频注入能力子 trait：向被控设备的（虚拟）音频输出播放一段 WAV（M11 REC-3，ISS-20260708-001）。
//!
//! 蓝本契约（compass :188）：`audio_inject | {wav_base64 或 file} | {ok} | E_NO_AUDIO_DEV`。
//! 与录屏音轨（aura-platform record.rs 的 AudioSettings disabled）正交：本原语是「注入」——把音频
//! 送进设备的音频通路（Linux PulseAudio null-sink），供被测应用「听到」；不是录制。
//!
//! 平台特化（镜像 record.rs 模式：trait + params 平台无关，实现下沉 aura-platform `#[cfg]` 门控
//! backend）：
//!   - Linux：pactl 幂等确保 null-sink `aura_inject` → paplay 同步播放（M11 主线，真机闭环）。
//!   - Windows / macOS：无虚拟音频设备通路，返回 E_NO_AUDIO_DEV 结构化降级（issue 记账）。
//!   - Android / iOS：矩阵 Unsupported（E_UNSUPPORTED，无宿主侧音频注入语义）。

use async_trait::async_trait;
use schemars::JsonSchema;
use serde::{Deserialize, Serialize};

use crate::types::CapError;

/// audio_inject 入参：`wav_base64` 与 `file` 恰取其一（蓝本契约二选一；双缺省 / 双给定均
/// E_INVALID_ARG）。复用为 MCP 工具入参（derive JsonSchema），字段文档即工具 schema 描述。
#[derive(Debug, Clone, Serialize, Deserialize, JsonSchema)]
pub struct AudioInjectParams {
    /// 内联 WAV 内容（base64）。节点解码后落唯一命名临时文件播放，用毕清理。
    #[serde(default)]
    pub wav_base64: Option<String>,
    /// 节点本地 WAV 文件路径（与 `wav_base64` 二选一）；路径不存在 → E_INVALID_ARG。
    #[serde(default)]
    pub file: Option<String>,
}

/// audio_inject 结果：成功语义由统一信封 `ok:true` 承载，数据体最小化。
#[derive(Debug, Clone, Serialize, Deserialize, JsonSchema)]
pub struct AudioInjectResult {
    /// 实际播放时长（毫秒，由 WAV 头 byte_rate/data 长度估算）；不可估算时省略。
    #[serde(skip_serializing_if = "Option::is_none")]
    pub played_ms: Option<u64>,
}

/// 音频注入能力。平台后端见 aura-platform `audio.rs`（Linux pactl/paplay 实装；Windows/macOS
/// E_NO_AUDIO_DEV 结构化降级；Android/iOS E_UNSUPPORTED stub，矩阵语义）。
#[async_trait]
pub trait AudioDriver: Send + Sync {
    /// 向设备（虚拟）音频输出注入一段 WAV 并同步等待播完。
    /// 音频后端不可用（pactl/paplay 缺失、PulseAudio 不可达、load-module 失败）→ E_NO_AUDIO_DEV；
    /// 参数非法（二选一违约 / 文件缺失 / 非 RIFF/WAVE / 坏 WAV 播放失败）→ E_INVALID_ARG。
    async fn audio_inject(&self, params: AudioInjectParams) -> Result<AudioInjectResult, CapError>;
}
