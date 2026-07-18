//! 录屏能力子 trait：会话化的持续屏幕采集 → 编码为视频文件。
//!
//! 与截图（单帧）正交：录屏为长驻会话（start → 持续采集编码 → stop finalize）。平台特化：
//!   - Windows：WGC 持续帧循环（对照 screen.rs 首帧即停的单帧抓取）→ Media Foundation 编码 MP4。
//!   - Linux / macOS：明确 defer 至 M4（返回 E_UNSUPPORTED，见平台层 `#[cfg]` 门控）。
//!
//! 安全语义（Locked-8，缓解 R-3 RDP 采集稳定性）：fps 与时长封顶（平台层强制安全上限），
//! frame watchdog——超时无新帧即优雅终止、上传已采集部分并标 `truncated=true`，杜绝 RDP
//! 会话抖动导致帧源停供时录屏线程悬挂。
//!
//! 会话态由平台层持有（进程内 rec_id → 活动会话注册表，复刻 a11y UIA 独占线程先例）；
//! `rec_id` 与落盘路径由上层（传输层）注入——与对象存储 key 同源，产物经 G-5 旁路 PUT
//! 上传后以 resource_link 交付。本 trait 平台无关，仅描述"起/停一个录屏会话"。

use std::path::PathBuf;

use async_trait::async_trait;
use schemars::JsonSchema;
use serde::{Deserialize, Serialize};

use crate::types::CapError;

/// 录屏启动入参（agent / MCP 工具面 schema）。fps 与时长为软请求，平台层按安全上限封顶。
#[derive(Debug, Clone, Serialize, Deserialize, JsonSchema)]
pub struct RecordParams {
    /// 目标帧率（帧/秒）。缺省 5；平台层封顶安全上限（超出取上限，防高帧率压垮 RDP 会话）。
    #[serde(default)]
    pub fps: Option<u32>,
    /// 最长录制时长（秒）。达上限自动优雅终止并标 `truncated=true`；缺省 30，平台层封顶安全上限。
    #[serde(default)]
    pub max_duration_secs: Option<u64>,
    /// 目标显示器 0 基序号；缺省 0（首个显示器），语义同 screenshot。
    #[serde(default)]
    pub display: Option<u32>,
}

/// 录屏产物元数据（平台采集 / 编码结果，平台无关形状）。
///
/// `truncated=true` 表示会话由时长封顶或 frame watchdog 强制终止——视频为部分产物（仍可交付回放）；
/// 显式 stop_recording 正常收尾则为 `false`。
#[derive(Debug, Clone, Serialize, Deserialize, JsonSchema)]
pub struct RecordArtifact {
    /// 容器 MIME 类型（如 `video/mp4`）。
    pub mime: String,
    /// 产物文件字节数（finalize 后落盘大小）。
    pub size_bytes: u64,
    /// 实际录制时长（毫秒，近似 = start 到 stop 的墙钟）。
    pub duration_ms: u64,
    /// 已编码帧数（经 fps 采样后实际写入的帧）。
    pub frame_count: u64,
    /// 是否被时长封顶 / 看门狗强制截断（部分产物）。
    pub truncated: bool,
}

/// 录屏能力。**Windows-only**（WGC 持续采集 + Media Foundation 编码）；Linux / macOS 返回
/// E_UNSUPPORTED（M4 补齐）。会话态由平台层内部持有；`rec_id` / `output_path` 由传输层注入。
#[async_trait]
pub trait RecordDriver: Send + Sync {
    /// 启动一个持续录屏会话（立即返回，采集在独立线程持续进行）。
    ///
    /// `rec_id` 唯一标识会话（对象存储 key 同源），`output_path` 为节点侧解析的视频落盘路径。
    /// fps / 时长封顶与 frame watchdog 由平台层强制。会话已存在或平台不支持 → [`CapError`]。
    async fn start_recording(
        &self,
        rec_id: String,
        params: RecordParams,
        output_path: PathBuf,
    ) -> Result<(), CapError>;

    /// 停止指定录屏会话并 finalize 视频文件，返回产物元数据。
    /// 会话不存在（未 start 或已 stop）→ E_INVALID_ARG。
    async fn stop_recording(&self, rec_id: String) -> Result<RecordArtifact, CapError>;
}
