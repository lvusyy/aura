//! audio 域工具（1 个：audio_inject）+ audio_router。body 委派 driver.audio_inject（M11 REC-3）。
//!
//! 入参复用 capability 层 [`AudioInjectParams`]（自带 JsonSchema），MCP schema 与能力层同源；
//! gRPC 反连侧经 `tool_dispatch` 注册表派发同一执行核，MCP/gRPC 两侧工具集由集合相等断言强制同步。
//! 返回维持 `Json<Envelope<T>>` 形态（无 image 输出形态，不沾 screenshot 的 CallToolResult 改造面）。
//! 广告面 plan D6：Linux desktop 广告（pactl 主线）；Windows/macOS 经 `supports_tool` 剔除但
//! dispatch 超集网不滤，call-time 返 E_NO_AUDIO_DEV 结构化降级。

use rmcp::handler::server::wrapper::Parameters;
use rmcp::{tool, tool_router, Json};

use aura_capability::{AudioInjectParams, AudioInjectResult, Envelope};

use super::AuraTools;

#[tool_router(router = audio_router, vis = "pub(crate)")]
impl AuraTools {
    /// 向被控设备的（虚拟）音频输出注入一段 WAV 并同步等待播完。
    #[tool(
        description = "Inject a WAV audio clip into the device's (virtual) audio output and wait for playback to finish. Provide exactly one of 'wav_base64' (inline WAV, base64) or 'file' (node-local WAV path). On Linux this plays into the PulseAudio null sink 'aura_inject' (verify via its monitor source); platforms without a virtual audio device return E_NO_AUDIO_DEV."
    )]
    async fn audio_inject(
        &self,
        Parameters(p): Parameters<AudioInjectParams>,
    ) -> Json<Envelope<AudioInjectResult>> {
        Json(self.guard(self.driver.audio_inject(p)).await)
    }
}
