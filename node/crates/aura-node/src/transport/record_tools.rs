//! record 域工具（2 个：start_recording / stop_recording）+ record_router。
//!
//! 录屏会话化：start 起会话返回 rec_id → 持续采集编码 → stop finalize 并以 resource_link 交付。
//! 会话执行核（[`AuraTools::start_recording_impl`] / [`AuraTools::stop_recording_impl`]）为 MCP 与
//! gRPC 反连两侧共用（tool_dispatch 派发同一核），杜绝双写；MCP/gRPC 工具集由集合相等断言强制同步。
//!
//! 交付链路（G-5，TASK-007 轨道）：产物落盘 `<data_dir>/artifacts/<key>`（与 grant 处理器读取路径同源）→
//! 控制面签发 UploadUrlGrant（预签名 PUT）→ 节点旁路 PUT 上传 → resource_link（`aura://artifact/<key>`）
//! 交付，`auractl artifact get <key>` 经预签名 GET 回取（桶 aura-artifacts）。

use std::collections::HashMap;
use std::path::PathBuf;
use std::sync::atomic::{AtomicU64, Ordering};
use std::time::{SystemTime, UNIX_EPOCH};

use rmcp::handler::server::wrapper::Parameters;
use rmcp::{tool, tool_router, Json};
use schemars::JsonSchema;
use serde::{Deserialize, Serialize};

use aura_capability::{Envelope, ErrObj, RecordParams};

use super::AuraTools;

/// 活动录屏会话的节点侧元数据。平台层持有采集/编码态；本结构仅承载 rec_id → key/落盘路径 映射，
/// 供 stop 时构造 resource_link（对象存储交付句柄）。
pub(crate) struct RecordSession {
    /// 对象存储键（桶内路径），与落盘 `<data_dir>/artifacts/<key>` 同源。
    pub key: String,
    /// 视频落盘绝对路径。
    pub output_path: PathBuf,
    /// 会话登记时刻（Unix 毫秒）。平台层录屏时长封顶（120s），故登记超 [`RECLAIM_TTL_MS`] 的条目
    /// 其底层会话必已（自）终止——供 stop_recording 缺席时惰性 TTL 回收，防封顶/看门狗自终止后
    /// 会话在显式 stop 前于 recordings 表无界滞留（JoinHandle/map 泄漏，ISS-008）。
    pub created_at_ms: i64,
}

/// stop_recording 入参：目标会话 rec_id。
#[derive(Debug, Clone, Deserialize, JsonSchema)]
pub struct StopRecordingParams {
    /// start_recording 返回的会话 id。
    pub rec_id: String,
}

/// start_recording 返回：会话 id（后续 stop 用）。
#[derive(Debug, Clone, Serialize, Deserialize, JsonSchema)]
pub struct RecordStartResult {
    /// 会话 id。
    pub rec_id: String,
}

/// stop_recording 返回：产物 resource_link + 元数据。
#[derive(Debug, Clone, Serialize, Deserialize, JsonSchema)]
pub struct RecordStopResult {
    /// 会话 id。
    pub rec_id: String,
    /// 对象存储键（桶 aura-artifacts 内路径）；`auractl artifact get <key>` 可回取。
    pub key: String,
    /// 是否需控制面触发旁路上传（产物有帧 `size_bytes>0` 即置 true）。控制面 gateway 据此显式
    /// 信号触发 GrantUpload（预签名 PUT），令录屏产物自动旁路上传对象存储（ISS-010，上传对象键即
    /// `key`）；空产物（无帧 / finalize 异常，`size_bytes==0`）不触发上传。
    pub needs_upload: bool,
    /// 逻辑资源链接 `aura://artifact/<key>`；控制面/agent 解析为对象存储预签名 GET。
    pub resource_link: String,
    /// 节点侧落盘绝对路径（旁路 PUT 上传源；调试/本地回放用）。
    pub local_path: String,
    /// 容器 MIME（`video/mp4`）。
    pub mime: String,
    /// 产物字节数（0 表示无帧/finalize 异常的空产物）。
    pub size_bytes: u64,
    /// 录制时长（毫秒，近似）。
    pub duration_ms: u64,
    /// 已编码帧数。
    pub frame_count: u64,
    /// 是否为封顶/看门狗强制截断的部分产物。
    pub truncated: bool,
}

/// 生成进程内唯一 rec_id（无 uuid 依赖）：Unix 纳秒 + 进程内自增计数，hex 拼接。
fn new_rec_id() -> String {
    static COUNTER: AtomicU64 = AtomicU64::new(0);
    let n = COUNTER.fetch_add(1, Ordering::Relaxed);
    let ts = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.as_nanos())
        .unwrap_or(0);
    format!("{ts:x}{n:04x}")
}

/// 录屏会话注册表 TTL 回收上限（毫秒）：远超平台层录屏时长封顶（120s），保证到期条目其底层会话
/// 必已终止。自终止（封顶/看门狗）后若 stop_recording 从不到来，条目由后续 start/stop 经本 TTL
/// 惰性清除，防 never-stop 会话在 recordings 表无界滞留（ISS-008）。
const RECLAIM_TTL_MS: i64 = 300_000;

/// 当前 Unix 毫秒（会话登记时刻戳，TTL 回收基准）。
fn now_unix_ms() -> i64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.as_millis() as i64)
        .unwrap_or(0)
}

/// 惰性回收：移除登记超过 [`RECLAIM_TTL_MS`] 的滞留会话条目（自终止但未显式 stop 者）。纯映射逻辑
/// 抽为自由函数（传入 map + now），便于单测直接覆盖，无需构造 AuraTools/驱动。在 start_recording
/// 登记新会话前调用（piggyback GC，无需后台 sweep 线程）。
fn reclaim_stale_recordings(map: &mut HashMap<String, RecordSession>, now_ms: i64) {
    map.retain(|_, s| now_ms.saturating_sub(s.created_at_ms) < RECLAIM_TTL_MS);
}

/// 取录制登记表锁（批E D6：毒锁不再二次 panic）。此登记本在 `catch_unwind` 工具边界（mod.rs 文档化
/// panic 收敛契约）之外——`.unwrap()` 遇毒锁会真崩节点，越过该边界。登记表是无跨字段不变量的普通
/// map，中毒源 panic 已被边界记录，继续使用内层数据安全（`PoisonError::into_inner`）。
fn recordings_guard<'a>(
    m: &'a std::sync::Mutex<HashMap<String, RecordSession>>,
) -> std::sync::MutexGuard<'a, HashMap<String, RecordSession>> {
    m.lock().unwrap_or_else(std::sync::PoisonError::into_inner)
}

impl AuraTools {
    /// 录屏产物落盘绝对路径 `<data_dir>/artifacts/<key>`：data_dir 由 [`AuraTools`] 承载（`--data-dir`
    /// CLI 经 [`AuraTools::with_data_dir`] 注入，纯 MCP/无反连场景回退 env 缺省），与
    /// `grpc_reverse::artifact_staging_path`（grant 处理器 PUT 读取路径）**同源同解析**——录屏落盘处即
    /// 旁路上传读取处，无独立 env 解析、无双解析漂移（原 `AURA_DATA_DIR` 手动同步约定就此消除）。
    pub(crate) fn artifact_output_path(&self, key: &str) -> PathBuf {
        self.data_dir.join("artifacts").join(key)
    }

    /// start_recording 执行核（MCP/反连共用）：生成 rec_id + 对象存储 key，解析落盘路径并建父目录，
    /// 委派 driver 起录屏会话，成功即登记会话态。失败透传 driver 错误信封（既有 E_ 码）。
    pub(crate) async fn start_recording_impl(
        &self,
        params: RecordParams,
    ) -> Envelope<RecordStartResult> {
        let rec_id = new_rec_id();
        let key = format!("recordings/{rec_id}.mp4");
        let output_path = self.artifact_output_path(&key);
        // 建父目录（落盘路径与 grant 处理器读取路径 <data_dir>/artifacts/<key> 对齐）。
        if let Some(parent) = output_path.parent() {
            if let Err(e) = std::fs::create_dir_all(parent) {
                return Envelope::error(ErrObj {
                    code: "E_FILE_FAILED".to_string(),
                    message: format!("create recording dir failed: {e}"),
                });
            }
        }
        let env = self
            .guard(
                self.driver
                    .start_recording(rec_id.clone(), params, output_path.clone()),
            )
            .await;
        if !env.ok {
            return Envelope::error(env.error.unwrap_or_else(|| ErrObj {
                code: "E_INTERNAL".to_string(),
                message: "start_recording failed".to_string(),
            }));
        }
        // 登记会话前先惰性回收自终止滞留条目（封顶/看门狗后未显式 stop 者，ISS-008），piggyback 在
        // 每次 start，无需后台 sweep 线程；随后登记本会话（带 created_at 戳供后续 TTL 回收基准）。
        {
            let mut guard = recordings_guard(&self.recordings);
            let now = now_unix_ms();
            reclaim_stale_recordings(&mut guard, now);
            guard.insert(
                rec_id.clone(),
                RecordSession {
                    key,
                    output_path,
                    created_at_ms: now,
                },
            );
        }
        Envelope::ok(RecordStartResult { rec_id })
    }

    /// stop_recording 执行核（MCP/反连共用）：取会话 → 委派 driver 停录 finalize → 构造 resource_link。
    /// 会话不存在（未 start 或已 stop）→ E_INVALID_ARG。driver 失败透传其错误信封。
    pub(crate) async fn stop_recording_impl(&self, rec_id: String) -> Envelope<RecordStopResult> {
        let session = recordings_guard(&self.recordings).remove(&rec_id);
        let Some(session) = session else {
            return Envelope::error(ErrObj {
                code: "E_INVALID_ARG".to_string(),
                message: format!("no active recording session: {rec_id}"),
            });
        };
        let env = self.guard(self.driver.stop_recording(rec_id.clone())).await;
        match env.data {
            Some(artifact) => Envelope::ok(RecordStopResult {
                rec_id,
                resource_link: format!("aura://artifact/{}", session.key),
                // 有帧产物（size_bytes>0）向控制面显式上报需旁路上传（ISS-010）；空产物不触发。
                needs_upload: artifact.size_bytes > 0,
                key: session.key,
                local_path: session.output_path.to_string_lossy().into_owned(),
                mime: artifact.mime,
                size_bytes: artifact.size_bytes,
                duration_ms: artifact.duration_ms,
                frame_count: artifact.frame_count,
                truncated: artifact.truncated,
            }),
            None => Envelope::error(env.error.unwrap_or_else(|| ErrObj {
                code: "E_INTERNAL".to_string(),
                message: "stop_recording failed".to_string(),
            })),
        }
    }
}

#[tool_router(router = record_router, vis = "pub(crate)")]
impl AuraTools {
    /// 启动持续录屏会话（Windows-only）。返回 rec_id；fps/时长封顶，frame watchdog 在 RDP 帧源
    /// 停供时优雅终止。产物经 stop_recording 的 resource_link 交付。
    #[tool(
        description = "Start a continuous screen recording session (Windows only). Returns a rec_id; fps and duration are capped and a frame watchdog gracefully stops on RDP frame starvation. Retrieve the result via stop_recording."
    )]
    async fn start_recording(
        &self,
        Parameters(p): Parameters<RecordParams>,
    ) -> Json<Envelope<RecordStartResult>> {
        Json(self.start_recording_impl(p).await)
    }

    /// 停止录屏会话，finalize 视频并返回 resource_link（会话被封顶/看门狗终止则 truncated=true 部分产物）。
    #[tool(
        description = "Stop a screen recording session by rec_id, finalize the video, and return a resource_link for retrieval. The artifact is partial (truncated=true) if the session was capped or watchdog-terminated."
    )]
    async fn stop_recording(
        &self,
        Parameters(p): Parameters<StopRecordingParams>,
    ) -> Json<Envelope<RecordStopResult>> {
        Json(self.stop_recording_impl(p.rec_id).await)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// ISS-008：录屏自终止（封顶/看门狗）后若 stop_recording 从不到来，会话条目经 TTL 由后续
    /// start/stop 惰性回收，不再滞留至显式 stop（防 recordings 表 JoinHandle/map 泄漏）。
    #[test]
    fn self_terminated_recording_reclaimed_after_ttl() {
        let mk = || {
            let mut m: HashMap<String, RecordSession> = HashMap::new();
            m.insert(
                "rec-x".to_string(),
                RecordSession {
                    key: "recordings/rec-x.mp4".to_string(),
                    output_path: PathBuf::from("rec-x.mp4"),
                    created_at_ms: 1_000,
                },
            );
            m
        };
        // TTL 未到：在录 / 新近会话保留（不误杀在录会话——录屏时长封顶 120s 远小于 TTL）。
        let mut before = mk();
        reclaim_stale_recordings(&mut before, 1_000 + RECLAIM_TTL_MS - 1);
        assert!(before.contains_key("rec-x"), "TTL 未到的会话不应被回收");
        // TTL 已过：自终止后未显式 stop 的滞留会话被回收，sessions 不再含该 rec_id。
        let mut after = mk();
        reclaim_stale_recordings(&mut after, 1_000 + RECLAIM_TTL_MS + 1);
        assert!(
            !after.contains_key("rec-x"),
            "自终止后超 TTL 的会话不应再滞留 recordings 注册表"
        );
    }
}
