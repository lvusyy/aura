//! RecordDriver 平台实现：Windows WGC 持续采集 + Media Foundation 编码 MP4；Linux ffmpeg x11grab
//! 子进程编码 MP4；macOS CGDisplayCreateImage 帧循环 + ffmpeg rawvideo 管道编码 MP4。
//!
//! 采集后端只此一层分平台（`#[cfg]` 门控），capability 层保持平台无关：
//!   - Windows：`windows-capture`（WGC）持续帧循环——对照 screen.rs 单帧抓取（首帧即 stop），本实现
//!     保持帧循环不停，每帧经 fps 采样后 `VideoEncoder::send_frame` 投递 Media Foundation 编码器
//!     （内部转码线程异步落盘 H.264/MP4）。编码器为 windows-capture 既有依赖（=2.0.0）内建，无新原生库、
//!     无新 Cargo 依赖，且已 `[target.'cfg(windows)']` 门控——不入三平台依赖矩阵（Locked：codec spike 定案）。
//!   - Linux：ffmpeg 子进程（`-f x11grab` 抓 DISPLAY 指向的 X 会话 → libx264 → MP4）。RECORD_BACKEND=
//!     ffmpeg 系 M11 W1 冒烟定字（真机/容器均缺 GStreamer x264enc，ffmpeg 单包补装面最小）；子进程
//!     生命周期由控制线程管理，`-t` 时长封顶自终止兜底节点进程亡故的孤儿子进程场景。
//!   - macOS：进程内 `screen.rs::capture_monitor_rgba`（CG 直采，虚拟屏可用）按 fps 逐帧取材，
//!     经 stdin 管道喂 ffmpeg 子进程纯编码（`-f rawvideo` → libx264 → MP4）。整屏采集类 API 在
//!     macOS 26 上实测全灭：`screencapture -v` 对 DeskPad 虚拟屏挂死（SCK 同根）、AVCaptureScreenInput
//!     已被移除（avfoundation 设备列表无屏幕项）——截图同源的 CG 帧循环是唯一帧源。停止 = 关 stdin，
//!     ffmpeg 收 EOF 排空收尾；节点亡故管道自断、编码器自退，孤儿保护由管道语义免费提供。
//!
//! 安全语义（Locked-8，缓解 R-3 RDP 采集稳定性；M11 Locked-5：Linux 与 Windows 三件对齐、trait 层零改）：
//! fps / 时长封顶 + watchdog。单一控制线程为唯一停止决策方（显式 stop / 时长封顶 / 看门狗），三实装
//! 后端同构落地：Windows 触发即 `CaptureControl::stop()`（投递 WM_QUIT 打破消息循环，不依赖帧到达——
//! 帧源停供亦能优雅终止不悬挂），随后编码器经 Drop finalize 落盘；Linux 触发即 SIGINT（ffmpeg 优雅写
//! trailer + faststart 收尾），宽限超时升级 SIGKILL；macOS 触发即置 halt 令采帧线程关 stdin（ffmpeg
//! 收 EOF 排空收尾），采帧线程卡死在阻塞 write（编码器 wedge）时宽限后杀子进程以 EPIPE 解锁。看门狗
//! 判据分别为帧到达停滞（Windows / macOS 同判 last_frame_ms）与 ffmpeg `-progress` 文件增长停滞 /
//! 子进程异常退出（Linux）。

use std::path::PathBuf;

use async_trait::async_trait;

use aura_capability::{CapError, RecordArtifact, RecordDriver, RecordParams};

use crate::PlatformDriver;

#[async_trait]
impl RecordDriver for PlatformDriver {
    async fn start_recording(
        &self,
        rec_id: String,
        params: RecordParams,
        output_path: PathBuf,
    ) -> Result<(), CapError> {
        backend::start_recording(rec_id, params, output_path).await
    }

    async fn stop_recording(&self, rec_id: String) -> Result<RecordArtifact, CapError> {
        backend::stop_recording(rec_id).await
    }
}

// ===== 安全上限（Locked-8）与跨后端共享件：Windows / Linux / macOS 三实装后端共用 =====
// 六值语义零改（M11 Locked-5），仅由原 windows mod 内上提至此共享；mac 后端入列（批G）
// 使 BITRATE/fps/时长封顶三平台单源对齐（P3 码率 2.5Mbps 对 mac 自动生效）。

#[cfg(any(windows, target_os = "linux", target_os = "macos"))]
mod caps {
    use std::time::{Duration, SystemTime, UNIX_EPOCH};

    /// 帧率上限（帧/秒）：封顶防高帧率压垮 RDP / X 会话。
    pub(super) const MAX_FPS: u32 = 10;
    /// 缺省帧率。
    pub(super) const DEFAULT_FPS: u32 = 5;
    /// 最长时长上限（秒）：达上限自动优雅终止。
    pub(super) const MAX_DURATION_SECS: u64 = 120;
    /// 缺省时长（秒）。
    pub(super) const DEFAULT_DURATION_SECS: u64 = 30;
    /// 看门狗：超过此时长（毫秒）无采集进展即判定源停供（Windows：无新帧；Linux：ffmpeg progress
    /// 停滞），优雅终止并标 truncated。
    pub(super) const WATCHDOG_TIMEOUT_MS: i64 = 5_000;
    /// H.264 目标码率（bps）。5fps 屏幕内容取 2.5Mbps：低帧率 + 大片静态区，2.5Mbps 下文字仍可读、
    /// 压缩痕迹极轻，而体积仅为原 8Mbps 的约 0.31×（此前 8Mbps 对 5fps 屏录偏奢——120s 产物 ~120MB，
    /// 降后 ~37MB）。分辨率保持原生（未加缩放，跨后端一致、零 WGC 编码尺寸风险）。
    pub(super) const BITRATE: u32 = 2_500_000;
    /// 控制线程轮询间隔。
    pub(super) const CONTROL_POLL: Duration = Duration::from_millis(250);
    /// SESSIONS 滞留回收上限（毫秒）：远超时长封顶 MAX_DURATION_SECS，保证到期条目其控制线程必已
    /// 退出。封顶/看门狗自终止后 stop_recording 缺席时，条目由后续 start/stop 经本 TTL 惰性清除。
    pub(super) const RECLAIM_TTL_MS: i64 = 300_000;

    /// 当前 Unix 毫秒。
    pub(super) fn now_unix_ms() -> i64 {
        SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .map(|d| d.as_millis() as i64)
            .unwrap_or(0)
    }
}

// ===== Windows 采集后端：WGC 持续帧循环 + Media Foundation VideoEncoder =====

#[cfg(windows)]
mod backend {
    use std::collections::HashMap;
    use std::path::PathBuf;
    use std::sync::atomic::{AtomicBool, AtomicI64, AtomicU64, Ordering};
    use std::sync::{Arc, Mutex, OnceLock};
    use std::thread::JoinHandle;
    use std::time::{Duration, Instant};

    use windows_capture::capture::{CaptureControl, Context, GraphicsCaptureApiHandler};
    use windows_capture::encoder::{
        AudioSettingsBuilder, ContainerSettingsBuilder, VideoEncoder, VideoSettingsBuilder,
        VideoSettingsSubType,
    };
    use windows_capture::frame::Frame;
    use windows_capture::graphics_capture_api::InternalCaptureControl;
    use windows_capture::monitor::Monitor;
    use windows_capture::settings::{
        ColorFormat, CursorCaptureSettings, DirtyRegionSettings, DrawBorderSettings,
        MinimumUpdateIntervalSettings, SecondaryWindowSettings, Settings,
    };

    use aura_capability::{CapError, RecordArtifact, RecordParams};

    // 安全上限（Locked-8，缓解 R-3）与时钟 helper：定义上提至文件级 caps mod（Windows/Linux 共享）。
    use super::caps::{
        now_unix_ms, BITRATE, CONTROL_POLL, DEFAULT_DURATION_SECS, DEFAULT_FPS, MAX_DURATION_SECS,
        MAX_FPS, RECLAIM_TTL_MS, WATCHDOG_TIMEOUT_MS,
    };

    /// 会话共享态：跨采集线程（写 frame_count/last_frame_ms）+ 控制线程（写 truncated）+
    /// stop 调用（写 stop_requested）+ stop 读取。
    struct Shared {
        /// 已编码帧数。
        frame_count: AtomicU64,
        /// 最近成功编码帧的 Unix 毫秒（看门狗据此判帧源停供）。0=尚无帧。
        last_frame_ms: AtomicI64,
        /// 是否被封顶/看门狗强制截断。
        truncated: AtomicBool,
        /// 显式停止请求（stop_recording 置位，控制线程观察后停采集）。
        stop_requested: AtomicBool,
        /// 会话起点（时长封顶 + duration 计算，只读 Copy 字段）。
        started: Instant,
        /// 控制线程退出时刻（Unix 毫秒，0=运行中）。封顶/看门狗自终止后 stop_recording 未必到来，
        /// 据此 + TTL 惰性回收 SESSIONS 滞留条目（ISS-008），防 JoinHandle/map 泄漏。
        finished_at: AtomicI64,
    }

    impl Shared {
        fn new() -> Self {
            Self {
                frame_count: AtomicU64::new(0),
                last_frame_ms: AtomicI64::new(0),
                truncated: AtomicBool::new(false),
                stop_requested: AtomicBool::new(false),
                started: Instant::now(),
                finished_at: AtomicI64::new(0),
            }
        }
    }

    /// 活动会话条目：共享态 + 落盘路径 + 控制线程句柄（stop 时 join 保证 finalize 完成）。
    struct SessionEntry {
        shared: Arc<Shared>,
        output_path: PathBuf,
        controller: Option<JoinHandle<()>>,
    }

    /// 进程内会话注册表（rec_id → 活动会话）。复刻 a11y UIA_TX OnceLock 惰性单例先例。
    static SESSIONS: OnceLock<Mutex<HashMap<String, SessionEntry>>> = OnceLock::new();

    fn sessions() -> &'static Mutex<HashMap<String, SessionEntry>> {
        SESSIONS.get_or_init(|| Mutex::new(HashMap::new()))
    }

    /// 取会话注册表锁（批E D6：毒锁不再二次 panic）。注册表在 catch_unwind 工具边界之外，`.unwrap()`
    /// 遇毒锁会真崩节点；表为无跨字段不变量的普通 map，中毒源 panic 已被边界记录，继续用内层数据安全。
    fn sessions_guard() -> std::sync::MutexGuard<'static, HashMap<String, SessionEntry>> {
        sessions().lock().unwrap_or_else(std::sync::PoisonError::into_inner)
    }

    /// 惰性回收已终止且超 [`RECLAIM_TTL_MS`] 的 SESSIONS 条目（自终止但未显式 stop 者，ISS-008）。
    /// 在 start_recording 登记新会话前调用（piggyback GC，无后台线程）；`finished_at==0`（运行中）
    /// 及未到期条目保留——故 stop_recording 仍能在 TTL 窗口内取回自终止会话的 artifact 元数据。
    fn reclaim_finished(now_ms: i64) {
        sessions_guard().retain(|_, e| {
            let fin = e.shared.finished_at.load(Ordering::Relaxed);
            fin == 0 || now_ms.saturating_sub(fin) < RECLAIM_TTL_MS
        });
    }

    /// 传入 WGC handler 构造的 flags：落盘路径 + 编码尺寸 + fps + 共享态。
    struct RecordFlags {
        output_path: PathBuf,
        width: u32,
        height: u32,
        fps: u32,
        shared: Arc<Shared>,
    }

    /// 持续录屏 handler：对照 screen.rs FrameGrabber 首帧即 stop——本 handler 保持帧循环，
    /// 每帧经 fps 采样后 send_frame 给 Media Foundation 编码器（编码器内部转码线程异步落盘）。
    struct RecordHandler {
        /// 编码器（new 时建，控制线程停采集后经 callback 句柄取出 finish 收尾）。
        encoder: Option<VideoEncoder>,
        /// 帧间隔（1/fps）：fps 封顶采样。
        frame_interval: Duration,
        /// 上次编码帧时刻（fps 采样基准）。
        last_encoded: Option<Instant>,
        shared: Arc<Shared>,
    }

    impl GraphicsCaptureApiHandler for RecordHandler {
        type Flags = RecordFlags;
        type Error = Box<dyn std::error::Error + Send + Sync>;

        fn new(ctx: Context<Self::Flags>) -> Result<Self, Self::Error> {
            let f = ctx.flags;
            // H.264/MP4（universal 可播放，非默认 HEVC）；fps 与几何显式设定，音频禁用（audio_inject defer）。
            let encoder = VideoEncoder::new(
                VideoSettingsBuilder::new(f.width, f.height)
                    .sub_type(VideoSettingsSubType::H264)
                    .frame_rate(f.fps)
                    .bitrate(BITRATE),
                AudioSettingsBuilder::default().disabled(true),
                ContainerSettingsBuilder::default(),
                &f.output_path,
            )?;
            Ok(Self {
                encoder: Some(encoder),
                frame_interval: Duration::from_secs_f64(1.0 / f.fps.max(1) as f64),
                last_encoded: None,
                shared: f.shared,
            })
        }

        fn on_frame_arrived(
            &mut self,
            frame: &mut Frame,
            _capture_control: InternalCaptureControl,
        ) -> Result<(), Self::Error> {
            // fps 采样：距上次编码帧不足 frame_interval 则丢弃本帧（封顶帧率，减小体积/负载）。
            let now = Instant::now();
            if let Some(last) = self.last_encoded {
                if now.duration_since(last) < self.frame_interval {
                    return Ok(());
                }
            }
            if let Some(enc) = self.encoder.as_mut() {
                // GPU 表面直投编码器（send_frame 复制表面后交内部转码线程，快速返回）。
                enc.send_frame(frame)?;
                self.last_encoded = Some(now);
                self.shared.frame_count.fetch_add(1, Ordering::Relaxed);
                self.shared.last_frame_ms.store(now_unix_ms(), Ordering::Relaxed);
            }
            Ok(())
        }
        // on_closed 不覆盖：start_free_threaded 外部停止不触发之，finalize 统一由控制线程经 callback 句柄执行。
    }

    /// 控制线程：单一停止决策方——观察显式停止/时长封顶/帧看门狗，触发即 stop 采集线程。
    /// 编码器 finalize 不在此显式调用：`CaptureControl::stop()` join 采集线程后，start_free_threaded 线程
    /// 收尾时 handler 随作用域析构，`VideoEncoder::Drop`（发 EOS + join 转码线程 → SinkWriter Finalize，
    /// 与 `finish()` 同效）在采集线程上落盘完整 MP4——规避跨线程 finish 与 COM 亲和性问题。
    /// 帧源停供（无帧）时 stop() 投递 WM_QUIT 打破消息循环（不依赖帧到达），保证优雅终止不悬挂。
    fn controller_loop(
        capture_control: CaptureControl<RecordHandler, Box<dyn std::error::Error + Send + Sync>>,
        shared: Arc<Shared>,
        max_duration: Duration,
    ) {
        // Option 承载：`take()` 干净移出 CaptureControl 供 stop() 消费（消除 in-loop move 借用歧义）。
        let mut cc = Some(capture_control);
        loop {
            std::thread::sleep(CONTROL_POLL);
            // 采集线程自行结束（编码器错误/捕获项关闭）：其析构已 finalize，直接退出，无需 stop。
            if cc.as_ref().map(|c| c.is_finished()).unwrap_or(true) {
                break;
            }
            let over_max = shared.started.elapsed() >= max_duration;
            let last = shared.last_frame_ms.load(Ordering::Relaxed);
            // 看门狗：已出过帧后超阈值无新帧 → 帧源停供。首帧未到（last==0）时由时长封顶兜底。
            let starved = last != 0 && (now_unix_ms() - last) >= WATCHDOG_TIMEOUT_MS;
            let stop = shared.stop_requested.load(Ordering::Relaxed);
            if stop || over_max || starved {
                // 封顶/看门狗为强制截断（部分产物）；显式 stop 为正常收尾（不标 truncated）。
                if over_max || starved {
                    shared.truncated.store(true, Ordering::Relaxed);
                }
                // stop() 投递 WM_QUIT 打破消息循环并 join 采集线程（消费 CaptureControl）；
                // join 返回时 handler 已析构 → 编码器 Drop 已 finalize，文件落盘完整。
                if let Some(c) = cc.take() {
                    let _ = c.stop();
                }
                break;
            }
        }
        // 控制线程退出（显式 stop / 时长封顶 / 看门狗 / 采集线程自终）→ 打 finished 戳。自终止
        // （over_max/starved）会话的 stop_recording 未必到来，条目由 reclaim_finished 经 TTL 清除，
        // 防 SESSIONS 无界滞留（ISS-008）；显式 stop 路径由 stop_recording 直接移除，本戳无害。
        shared.finished_at.store(now_unix_ms(), Ordering::Relaxed);
    }

    /// 按 0 基序号选显示器（缺省 0），语义同 screen.rs 后端。
    fn pick_monitor(index: Option<u32>) -> Result<Monitor, CapError> {
        let monitors = Monitor::enumerate()
            .map_err(|e| CapError::CaptureFailed(format!("enumerate monitors failed: {e}")))?;
        let idx = index.unwrap_or(0) as usize;
        monitors
            .into_iter()
            .nth(idx)
            .ok_or_else(|| CapError::InvalidArg(format!("display index {idx} out of range")))
    }

    pub(super) async fn start_recording(
        rec_id: String,
        params: RecordParams,
        output_path: PathBuf,
    ) -> Result<(), CapError> {
        let fps = params.fps.unwrap_or(DEFAULT_FPS).clamp(1, MAX_FPS);
        let max_duration = Duration::from_secs(
            params
                .max_duration_secs
                .unwrap_or(DEFAULT_DURATION_SECS)
                .clamp(1, MAX_DURATION_SECS),
        );
        let display = params.display;

        // WGC 建流/start_free_threaded 为阻塞 COM 调用，收敛进 spawn_blocking（不阻塞异步运行时）。
        tokio::task::spawn_blocking(move || -> Result<(), CapError> {
            // 登记新会话前惰性回收自终止滞留条目（封顶/看门狗后未显式 stop 者，ISS-008）。
            reclaim_finished(now_unix_ms());
            if sessions_guard().contains_key(&rec_id) {
                return Err(CapError::InvalidArg(format!(
                    "recording session already exists: {rec_id}"
                )));
            }
            let monitor = pick_monitor(display)?;
            // 编码几何取显示器尺寸，H.264 要求偶数边 → 向下取偶（WGC 帧尺寸差异由编码器内部 padding 吸收）。
            let width = monitor
                .width()
                .map_err(|e| CapError::CaptureFailed(format!("monitor width failed: {e}")))?
                & !1;
            let height = monitor
                .height()
                .map_err(|e| CapError::CaptureFailed(format!("monitor height failed: {e}")))?
                & !1;
            if width == 0 || height == 0 {
                return Err(CapError::CaptureFailed("monitor has zero dimensions".to_string()));
            }

            let shared = Arc::new(Shared::new());
            let flags = RecordFlags {
                output_path: output_path.clone(),
                width,
                height,
                fps,
                shared: shared.clone(),
            };
            // Cursor/Border 取 Default（同 screen.rs：RDP 会话下显式 config 返回 BorderConfigUnsupported）。
            // ColorFormat::Rgba8 与 windows-capture VideoEncoder send_frame 官方示例一致。
            let settings = Settings::new(
                monitor,
                CursorCaptureSettings::Default,
                DrawBorderSettings::Default,
                SecondaryWindowSettings::Default,
                MinimumUpdateIntervalSettings::Default,
                DirtyRegionSettings::Default,
                ColorFormat::Rgba8,
                flags,
            );

            let capture_control = RecordHandler::start_free_threaded(settings)
                .map_err(|e| CapError::CaptureFailed(format!("WGC recording start failed: {e:?}")))?;
            let shared_ctrl = shared.clone();
            let controller = std::thread::Builder::new()
                .name("aura-rec-ctrl".into())
                .spawn(move || controller_loop(capture_control, shared_ctrl, max_duration))
                .map_err(|e| CapError::Internal(format!("spawn recording controller failed: {e}")))?;

            sessions_guard().insert(
                rec_id,
                SessionEntry {
                    shared,
                    output_path,
                    controller: Some(controller),
                },
            );
            Ok(())
        })
        .await
        .map_err(|e| CapError::Internal(format!("start_recording task join failed: {e}")))?
    }

    pub(super) async fn stop_recording(rec_id: String) -> Result<RecordArtifact, CapError> {
        tokio::task::spawn_blocking(move || -> Result<RecordArtifact, CapError> {
            let entry = sessions_guard().remove(&rec_id).ok_or_else(|| {
                CapError::InvalidArg(format!("no active recording session: {rec_id}"))
            })?;

            // 请求停止 → 控制线程执行 stop + finalize；join 保证编码器已 finish、文件落盘完整。
            entry.shared.stop_requested.store(true, Ordering::Relaxed);
            if let Some(h) = entry.controller {
                let _ = h.join();
            }

            let frame_count = entry.shared.frame_count.load(Ordering::Relaxed);
            let truncated = entry.shared.truncated.load(Ordering::Relaxed);
            let duration_ms = entry.shared.started.elapsed().as_millis() as u64;
            let size_bytes = std::fs::metadata(&entry.output_path).map(|m| m.len()).unwrap_or(0);

            Ok(RecordArtifact {
                mime: "video/mp4".to_string(),
                size_bytes,
                duration_ms,
                frame_count,
                truncated,
            })
        })
        .await
        .map_err(|e| CapError::Internal(format!("stop_recording task join failed: {e}")))?
    }

    #[cfg(test)]
    mod tests {
        use super::*;

        /// ISS-008：控制线程自终止（封顶/看门狗）打 finished 戳后，SESSIONS 条目经 [`reclaim_finished`]
        /// 的 TTL sweep 清除——自终止不再滞留至显式 stop（sweep 后 `sessions()` 不含该 rec_id）。
        #[test]
        fn self_terminated_session_reclaimed_after_ttl() {
            let rec_id = "test-iss008-reclaim".to_string();
            let shared = Arc::new(Shared::new());
            // 模拟自终止：控制线程退出打 finished 戳（controller_loop 末尾行为）。
            shared.finished_at.store(1_000, Ordering::Relaxed);
            sessions_guard().insert(
                rec_id.clone(),
                SessionEntry {
                    shared,
                    output_path: PathBuf::from("dummy.mp4"),
                    controller: None,
                },
            );
            // TTL 未到：自终止会话在窗口内保留（stop_recording 仍可取回 artifact 元数据）。
            reclaim_finished(1_000 + RECLAIM_TTL_MS - 1);
            assert!(
                sessions_guard().contains_key(&rec_id),
                "TTL 未到的自终止会话应保留以供 stop 取元数据"
            );
            // TTL 已过：滞留条目被回收，sessions() 不再含该 rec_id。
            reclaim_finished(1_000 + RECLAIM_TTL_MS + 1);
            assert!(
                !sessions_guard().contains_key(&rec_id),
                "自终止后超 TTL 的会话应被回收，sessions() 不含该 rec_id"
            );
        }
    }
}

// ===== Linux 采集后端：ffmpeg x11grab 子进程（RECORD_BACKEND=ffmpeg，M11 W1 冒烟定字）=====

#[cfg(target_os = "linux")]
mod backend {
    use std::collections::HashMap;
    use std::path::{Path, PathBuf};
    use std::process::{Child, Command, Stdio};
    use std::sync::atomic::{AtomicBool, AtomicI64, AtomicU64, Ordering};
    use std::sync::{Arc, Mutex, OnceLock};
    use std::thread::JoinHandle;
    use std::time::{Duration, Instant};

    use aura_capability::{CapError, RecordArtifact, RecordParams};

    // 安全上限（Locked-8）与时钟 helper：定义上提至文件级 caps mod（Windows/Linux 共享，六值零改）。
    use super::caps::{
        now_unix_ms, BITRATE, CONTROL_POLL, DEFAULT_DURATION_SECS, DEFAULT_FPS, MAX_DURATION_SECS,
        MAX_FPS, RECLAIM_TTL_MS, WATCHDOG_TIMEOUT_MS,
    };

    /// SIGINT 优雅收尾宽限：ffmpeg 收 SIGINT 后写 trailer + faststart 二次 pass（120s/2.5Mbps 产物
    /// 约 37MB 重写），宽限内未退才升级 SIGKILL（产物大概率缺 moov，如实标 truncated）。
    const STOP_GRACE: Duration = Duration::from_secs(10);
    /// spawn 后启动观察窗：窗口内子进程即退视为启动失败（x11grab 连不上 DISPLAY / 参数错），
    /// 读 stderr 尾段透出结构化错误。
    const SPAWN_PROBE: Duration = Duration::from_millis(500);

    /// 会话共享态：控制线程（写 truncated/frame_count/finished_at）+ stop 调用（写 stop_requested）
    /// + stop 读取。骨架复刻 Windows backend；Windows 的 last_frame_ms 帧活性由 ffmpeg `-progress`
    /// 文件增长活性替代（控制线程局部变量，无需跨线程共享）。
    struct Shared {
        /// 已编码帧数：控制线程收尾时从 `-progress` 文件解析回填（best-effort，解析失败保持 0——
        /// Windows 侧为逐帧真值，Linux 侧尽力语义）。
        frame_count: AtomicU64,
        /// 是否被封顶/看门狗/子进程异常退出强制截断。
        truncated: AtomicBool,
        /// 显式停止请求（stop_recording 置位，控制线程观察后 SIGINT 收尾）。
        stop_requested: AtomicBool,
        /// 会话起点（时长封顶 + duration 计算，只读 Copy 字段）。
        started: Instant,
        /// 控制线程退出时刻（Unix 毫秒，0=运行中）。封顶/看门狗自终止后 stop_recording 未必到来，
        /// 据此 + TTL 惰性回收 SESSIONS 滞留条目（ISS-008），防 JoinHandle/map 泄漏。
        finished_at: AtomicI64,
    }

    impl Shared {
        fn new() -> Self {
            Self {
                frame_count: AtomicU64::new(0),
                truncated: AtomicBool::new(false),
                stop_requested: AtomicBool::new(false),
                started: Instant::now(),
                finished_at: AtomicI64::new(0),
            }
        }
    }

    /// 活动会话条目：共享态 + 落盘路径 + 控制线程句柄（stop 时 join 保证子进程已收尾、产物落盘完成）。
    struct SessionEntry {
        shared: Arc<Shared>,
        output_path: PathBuf,
        controller: Option<JoinHandle<()>>,
    }

    /// 进程内会话注册表（rec_id → 活动会话）。复刻 Windows backend / a11y UIA_TX OnceLock 惰性单例先例。
    static SESSIONS: OnceLock<Mutex<HashMap<String, SessionEntry>>> = OnceLock::new();

    fn sessions() -> &'static Mutex<HashMap<String, SessionEntry>> {
        SESSIONS.get_or_init(|| Mutex::new(HashMap::new()))
    }

    /// 取会话注册表锁（批E D6：毒锁不再二次 panic）。注册表在 catch_unwind 工具边界之外，`.unwrap()`
    /// 遇毒锁会真崩节点；表为无跨字段不变量的普通 map，中毒源 panic 已被边界记录，继续用内层数据安全。
    fn sessions_guard() -> std::sync::MutexGuard<'static, HashMap<String, SessionEntry>> {
        sessions().lock().unwrap_or_else(std::sync::PoisonError::into_inner)
    }

    /// 惰性回收已终止且超 [`RECLAIM_TTL_MS`] 的 SESSIONS 条目（自终止但未显式 stop 者，ISS-008）。
    /// 在 start_recording 登记新会话前调用（piggyback GC，无后台线程）；`finished_at==0`（运行中）
    /// 及未到期条目保留——故 stop_recording 仍能在 TTL 窗口内取回自终止会话的 artifact 元数据。
    fn reclaim_finished(now_ms: i64) {
        sessions_guard().retain(|_, e| {
            let fin = e.shared.finished_at.load(Ordering::Relaxed);
            fin == 0 || now_ms.saturating_sub(fin) < RECLAIM_TTL_MS
        });
    }

    /// 参数封顶（纯函数，单测面）：fps clamp 到 [1, MAX_FPS] 缺省 DEFAULT_FPS；时长 clamp 到
    /// [1, MAX_DURATION_SECS] 缺省 DEFAULT_DURATION_SECS。语义与 Windows backend 完全一致。
    fn clamp_params(params: &RecordParams) -> (u32, u64) {
        let fps = params.fps.unwrap_or(DEFAULT_FPS).clamp(1, MAX_FPS);
        let duration_secs = params
            .max_duration_secs
            .unwrap_or(DEFAULT_DURATION_SECS)
            .clamp(1, MAX_DURATION_SECS);
        (fps, duration_secs)
    }

    /// x11grab 输入串：DISPLAY 原值直用；请求了显示器序号且 DISPLAY 无 screen 后缀时拼 `.{n}`
    /// （X screen 语义，无该 screen 则 ffmpeg 启动失败经快速路径透出）。注意 X screen ≠ RandR
    /// 多输出（常见多显示器为单 X screen 多输出，序号→输出几何映射留 stretch）；单显环境
    /// None / Some(0) 与全屏语义等价。
    fn display_input(display: &str, screen: Option<u32>) -> String {
        match screen {
            Some(n) if !display.contains('.') => format!("{display}.{n}"),
            _ => display.to_string(),
        }
    }

    /// ffmpeg x11grab 命令组装（纯函数，单测面），argv[0]="ffmpeg"：
    ///   - `-t` 为时长封顶第一重（ffmpeg 到点自终止，moov 完整落盘），控制线程墙钟封顶为冗余双保险，
    ///     且节点进程亡故时 `-t` 仍兜底孤儿子进程（子进程至多再跑到封顶点）；
    ///   - `-video_size` 省略取整屏（ffmpeg≥4.0 xcbgrab 缺省 full desktop），crop 滤镜向下取偶对齐
    ///     Windows 侧 `& !1`（libx264 yuv420p 要求偶数边，规避奇数分辨率启动失败）；
    ///   - `-movflags +faststart` 前置 moov 可流播；
    ///   - `-progress` 落独立文件供看门狗活性判定与 frame_count 解析（机器可读节律输出，且 stderr
    ///     重定向至日志文件后无管道反压——不占用管道即无需排空线程）。
    fn build_capture_command(
        params: &RecordParams,
        display_input: &str,
        output_path: &Path,
        progress_path: &Path,
    ) -> Vec<String> {
        let (fps, duration_secs) = clamp_params(params);
        vec![
            "ffmpeg".to_string(),
            "-y".to_string(),
            "-nostdin".to_string(),
            "-f".to_string(),
            "x11grab".to_string(),
            "-framerate".to_string(),
            fps.to_string(),
            "-i".to_string(),
            display_input.to_string(),
            "-t".to_string(),
            duration_secs.to_string(),
            "-c:v".to_string(),
            "libx264".to_string(),
            "-pix_fmt".to_string(),
            "yuv420p".to_string(),
            "-b:v".to_string(),
            BITRATE.to_string(),
            "-vf".to_string(),
            "crop=trunc(iw/2)*2:trunc(ih/2)*2".to_string(),
            "-movflags".to_string(),
            "+faststart".to_string(),
            "-progress".to_string(),
            progress_path.display().to_string(),
            output_path.display().to_string(),
        ]
    }

    /// 控制线程停止决策（纯函数，单测面）：`Some(truncated)`=本轮停止，`None`=继续。
    /// truncated 语义精确对齐 Windows controller_loop：显式 stop 正常收尾=false；到点封顶/看门狗=true
    /// （stop 与封顶同轮竞态时封顶语义优先，同 Windows 的 `if over_max || starved` 先判）。
    fn stop_decision(stop_requested: bool, over_max: bool, starved: bool) -> Option<bool> {
        if stop_requested || over_max || starved {
            Some(over_max || starved)
        } else {
            None
        }
    }

    /// 从 `-progress` 文件解析最后一个 `frame=` 值（best-effort：文件缺失/无 frame 行 → 0，禁 panic）。
    fn parse_last_frame(progress_path: &Path) -> u64 {
        let Ok(content) = std::fs::read_to_string(progress_path) else {
            return 0;
        };
        content
            .lines()
            .rev()
            .find_map(|l| l.strip_prefix("frame=").and_then(|v| v.trim().parse::<u64>().ok()))
            .unwrap_or(0)
    }

    /// 读 stderr 日志尾段（启动失败诊断透出，按 UTF-8 字符边界截取；文件缺失 → 空串）。
    fn stderr_tail(stderr_path: &Path, max_bytes: usize) -> String {
        let Ok(content) = std::fs::read_to_string(stderr_path) else {
            return String::new();
        };
        let mut start = content.len().saturating_sub(max_bytes);
        while !content.is_char_boundary(start) {
            start += 1;
        }
        content[start..].trim().to_string()
    }

    /// 产物同目录辅助文件路径 `<output>.<ext>`（rec_id 唯一 → 并发会话不互踩；控制线程用毕回收，
    /// 防 artifacts 目录残骸累积）。
    fn aux_path(output_path: &Path, ext: &str) -> PathBuf {
        let mut s = output_path.as_os_str().to_owned();
        s.push(format!(".{ext}"));
        PathBuf::from(s)
    }

    /// SIGINT → 宽限内轮询退出 → 超时 SIGKILL 兜底。返回 true=SIGINT 优雅退出（trailer 完整），
    /// false=SIGKILL（产物大概率缺 moov）。SIGINT 经 `kill -INT` 子命令发送——依赖树无 nix/libc
    /// 直接依赖（零新 crate 红线），kill(1) 系 procps 必备件。
    fn graceful_stop(child: &mut Child) -> bool {
        let sigint_ok = Command::new("kill")
            .args(["-INT", &child.id().to_string()])
            .status()
            .map(|s| s.success())
            .unwrap_or(false);
        if sigint_ok {
            let deadline = Instant::now() + STOP_GRACE;
            while Instant::now() < deadline {
                if let Ok(Some(_)) = child.try_wait() {
                    return true;
                }
                std::thread::sleep(Duration::from_millis(100));
            }
        }
        let _ = child.kill();
        let _ = child.wait();
        false
    }

    /// 控制线程：单一停止决策方——观察显式停止 / 时长封顶（`-t` 之外的冗余墙钟保险）/ 看门狗
    /// （progress 文件增长停滞 = 采集卡死，或子进程异常退出），触发即 SIGINT 优雅收尾。
    /// 一切退出路径均已 wait 收口子进程（无僵尸）：try_wait 命中即已 reap；stop 路径经
    /// [`graceful_stop`] 内 wait。
    fn controller_loop(
        mut child: Child,
        shared: Arc<Shared>,
        max_duration: Duration,
        progress_path: PathBuf,
        stderr_path: PathBuf,
    ) {
        // progress 活性基准：len 增长即有进展。首块未出（文件未建，len=0）从线程启动时刻起算——
        // ffmpeg 正常运行下 progress 每 ~0.5s 节律写入，超看门狗阈值仍无进展即采集卡死判定
        // （对照 Windows「首帧未到由时长封顶兜底」：子进程路线 progress 缺席即异常，提早止损）。
        let mut last_len: u64 = 0;
        let mut last_progress = Instant::now();
        loop {
            std::thread::sleep(CONTROL_POLL);
            // 子进程自行退出（`-t` 封顶自终止 / 异常崩溃）：非显式 stop 的终止一律截断态（部分产物）。
            // 显式 stop 的 SIGINT 退出不走此分支（下方 stop 分支内 graceful_stop 收口）。
            match child.try_wait() {
                Ok(Some(_)) => {
                    shared.truncated.store(true, Ordering::Relaxed);
                    break;
                }
                Ok(None) => {}
                Err(_) => {
                    // try_wait 自身错（罕见）：失联止损，杀掉并收口。
                    let _ = child.kill();
                    let _ = child.wait();
                    shared.truncated.store(true, Ordering::Relaxed);
                    break;
                }
            }
            let len = std::fs::metadata(&progress_path).map(|m| m.len()).unwrap_or(0);
            if len > last_len {
                last_len = len;
                last_progress = Instant::now();
            }
            let starved = last_progress.elapsed().as_millis() as i64 >= WATCHDOG_TIMEOUT_MS;
            let over_max = shared.started.elapsed() >= max_duration;
            let stop = shared.stop_requested.load(Ordering::Relaxed);
            if let Some(truncated) = stop_decision(stop, over_max, starved) {
                if truncated {
                    shared.truncated.store(true, Ordering::Relaxed);
                }
                // SIGINT 优雅收尾（写 trailer + faststart 二次 pass）；宽限超时 SIGKILL 兜底——
                // 兜底路径产物大概率缺 moov，如实标截断。
                if !graceful_stop(&mut child) {
                    shared.truncated.store(true, Ordering::Relaxed);
                }
                break;
            }
        }
        // 收尾：frame_count 从 progress 文件回填（best-effort）；辅助文件回收；打 finished 戳供
        // TTL 惰性回收（同 Windows，ISS-008）。
        shared
            .frame_count
            .store(parse_last_frame(&progress_path), Ordering::Relaxed);
        let _ = std::fs::remove_file(&progress_path);
        let _ = std::fs::remove_file(&stderr_path);
        shared.finished_at.store(now_unix_ms(), Ordering::Relaxed);
    }

    pub(super) async fn start_recording(
        rec_id: String,
        params: RecordParams,
        output_path: PathBuf,
    ) -> Result<(), CapError> {
        // 子进程 spawn / 启动观察窗为阻塞调用，收敛进 spawn_blocking（不阻塞异步运行时，同 Windows）。
        tokio::task::spawn_blocking(move || -> Result<(), CapError> {
            // 登记新会话前惰性回收自终止滞留条目（封顶/看门狗后未显式 stop 者，ISS-008）。
            reclaim_finished(now_unix_ms());
            if sessions_guard().contains_key(&rec_id) {
                return Err(CapError::InvalidArg(format!(
                    "recording session already exists: {rec_id}"
                )));
            }
            // DISPLAY 单源：真机 unit 固化（如 :10）/ Selkies sidecar 容器 pod env 注入
            // （entrypoint-desktop.sh `export DISPLAY`）。取不到 → 结构化错误（非 panic）。
            let display = std::env::var("DISPLAY").map_err(|_| {
                CapError::CaptureFailed(
                    "DISPLAY not set; x11grab recording requires an X session".to_string(),
                )
            })?;
            let input = display_input(&display, params.display);
            let progress_path = aux_path(&output_path, "progress");
            let stderr_path = aux_path(&output_path, "log");
            let argv = build_capture_command(&params, &input, &output_path, &progress_path);
            let (_, duration_secs) = clamp_params(&params);
            let max_duration = Duration::from_secs(duration_secs);

            // stderr 重定向至日志文件：零管道反压（无需排空线程），启动失败时读尾段透出诊断。
            let stderr_file = std::fs::File::create(&stderr_path).map_err(|e| {
                CapError::Internal(format!("create recorder stderr log failed: {e}"))
            })?;
            let shared = Arc::new(Shared::new());
            let mut child = Command::new(&argv[0])
                .args(&argv[1..])
                .stdin(Stdio::null())
                .stdout(Stdio::null())
                .stderr(Stdio::from(stderr_file))
                .spawn()
                .map_err(|e| {
                    CapError::CaptureFailed(format!(
                        "spawn ffmpeg failed: {e} (is ffmpeg installed?)"
                    ))
                })?;

            // 启动失败快速路径：观察窗内即退（x11grab 连不上 DISPLAY 的典型报错要能透出）。
            std::thread::sleep(SPAWN_PROBE);
            if let Ok(Some(status)) = child.try_wait() {
                let tail = stderr_tail(&stderr_path, 800);
                let _ = std::fs::remove_file(&stderr_path);
                let _ = std::fs::remove_file(&progress_path);
                return Err(CapError::CaptureFailed(format!(
                    "ffmpeg exited at startup ({status}): {tail}"
                )));
            }

            let shared_ctrl = shared.clone();
            let controller = std::thread::Builder::new()
                .name("aura-rec-ctrl".into())
                .spawn(move || {
                    controller_loop(child, shared_ctrl, max_duration, progress_path, stderr_path)
                })
                .map_err(|e| CapError::Internal(format!("spawn recording controller failed: {e}")))?;

            sessions_guard().insert(
                rec_id,
                SessionEntry {
                    shared,
                    output_path,
                    controller: Some(controller),
                },
            );
            Ok(())
        })
        .await
        .map_err(|e| CapError::Internal(format!("start_recording task join failed: {e}")))?
    }

    pub(super) async fn stop_recording(rec_id: String) -> Result<RecordArtifact, CapError> {
        tokio::task::spawn_blocking(move || -> Result<RecordArtifact, CapError> {
            let entry = sessions_guard().remove(&rec_id).ok_or_else(|| {
                CapError::InvalidArg(format!("no active recording session: {rec_id}"))
            })?;

            // 请求停止 → 控制线程 SIGINT 收尾（trailer + faststart）；join 保证产物落盘完成。
            entry.shared.stop_requested.store(true, Ordering::Relaxed);
            if let Some(h) = entry.controller {
                let _ = h.join();
            }

            let frame_count = entry.shared.frame_count.load(Ordering::Relaxed);
            let truncated = entry.shared.truncated.load(Ordering::Relaxed);
            let duration_ms = entry.shared.started.elapsed().as_millis() as u64;
            let size_bytes = std::fs::metadata(&entry.output_path).map(|m| m.len()).unwrap_or(0);
            // 子进程路线下零字节/缺失产物 = ffmpeg 未产出任何数据（失败态），结构化报错而非交付
            // 空产物元数据（Windows 编码器路径无此失败形态，Linux 特有分支——差异记终报）。
            if size_bytes == 0 {
                return Err(CapError::CaptureFailed(
                    "recording produced no output (ffmpeg wrote zero bytes)".to_string(),
                ));
            }

            Ok(RecordArtifact {
                mime: "video/mp4".to_string(),
                size_bytes,
                duration_ms,
                frame_count,
                truncated,
            })
        })
        .await
        .map_err(|e| CapError::Internal(format!("stop_recording task join failed: {e}")))?
    }

    #[cfg(test)]
    mod tests {
        use super::*;

        /// 命令组装 shape（RECORD_BACKEND=ffmpeg 定字）：x11grab 输入、fps/时长 clamp 后透传、
        /// `-t` 封顶自终止、码率取安全 const、faststart、输出/进度路径透传。
        #[test]
        fn linux_build_capture_command_shape() {
            let params = RecordParams {
                fps: Some(99),
                max_duration_secs: Some(999),
                display: None,
            };
            let argv = build_capture_command(
                &params,
                ":10",
                Path::new("/var/lib/aura/artifacts/recordings/r1.mp4"),
                Path::new("/var/lib/aura/artifacts/recordings/r1.mp4.progress"),
            );
            assert_eq!(argv[0], "ffmpeg");
            let joined = argv.join(" ");
            assert!(joined.contains("-f x11grab"), "采集格式应为 x11grab: {joined}");
            assert!(joined.contains("-framerate 10"), "fps 应 clamp 到 MAX_FPS: {joined}");
            assert!(joined.contains("-i :10"), "DISPLAY 输入应透传: {joined}");
            assert!(joined.contains("-t 120"), "时长应 clamp 到 MAX_DURATION_SECS: {joined}");
            assert!(
                joined.contains(&format!("-b:v {BITRATE}")),
                "码率应取安全 const BITRATE: {joined}"
            );
            assert!(joined.contains("-movflags +faststart"), "moov 应前置: {joined}");
            assert!(
                joined.contains("-progress /var/lib/aura/artifacts/recordings/r1.mp4.progress"),
                "progress 路径应透传: {joined}"
            );
            assert_eq!(
                argv.last().unwrap(),
                "/var/lib/aura/artifacts/recordings/r1.mp4",
                "输出路径应为末位参数"
            );
        }

        /// 参数封顶：超限 clamp、缺省取 DEFAULT、下限 1（语义与 Windows backend 一致）。
        #[test]
        fn linux_params_clamp() {
            let p = |fps, dur| RecordParams {
                fps,
                max_duration_secs: dur,
                display: None,
            };
            assert_eq!(
                clamp_params(&p(Some(99), Some(999))),
                (MAX_FPS, MAX_DURATION_SECS),
                "超限应 clamp 到安全上限"
            );
            assert_eq!(
                clamp_params(&p(None, None)),
                (DEFAULT_FPS, DEFAULT_DURATION_SECS),
                "缺省应取 DEFAULT 值"
            );
            assert_eq!(clamp_params(&p(Some(0), Some(0))), (1, 1), "下限应为 1");
        }

        /// 显示器序号 → X screen 输入串：无序号/已带 screen 后缀原值直用，带序号拼 `.{n}`。
        #[test]
        fn linux_display_input_mapping() {
            assert_eq!(display_input(":10", None), ":10");
            assert_eq!(display_input(":10", Some(0)), ":10.0");
            assert_eq!(display_input(":10", Some(1)), ":10.1");
            assert_eq!(display_input(":10.0", Some(1)), ":10.0", "已带 screen 后缀不重拼");
        }

        /// DISPLAY 缺失 → 结构化 CapError（非 panic）。
        #[tokio::test]
        async fn linux_display_missing_structured_error() {
            std::env::remove_var("DISPLAY");
            let err = start_recording(
                "test-t4-no-display".to_string(),
                RecordParams {
                    fps: None,
                    max_duration_secs: None,
                    display: None,
                },
                PathBuf::from("/tmp/aura-test-t4-no-display.mp4"),
            )
            .await
            .expect_err("DISPLAY 缺失应返回结构化错误而非 panic");
            assert!(
                matches!(err, CapError::CaptureFailed(_)),
                "应为 CaptureFailed 结构化错误: {err:?}"
            );
        }

        /// truncated 三态判定（精确对齐 Windows）：显式停=false；封顶/看门狗=true；无触发=继续。
        #[test]
        fn linux_truncated_semantics() {
            assert_eq!(stop_decision(false, false, false), None, "无触发应继续轮询");
            assert_eq!(stop_decision(true, false, false), Some(false), "显式 stop 正常收尾不截断");
            assert_eq!(stop_decision(false, true, false), Some(true), "时长封顶应截断");
            assert_eq!(stop_decision(false, false, true), Some(true), "看门狗应截断");
            assert_eq!(
                stop_decision(true, true, false),
                Some(true),
                "stop 与封顶同轮竞态：封顶语义优先（同 Windows）"
            );
        }

        /// progress 文件 frame= 尾值解析（best-effort：缺失/无 frame 行 → 0，禁 panic）。
        #[test]
        fn linux_parse_last_frame() {
            let p = std::env::temp_dir().join(format!(
                "aura-t4-progress-{}-{}.txt",
                std::process::id(),
                now_unix_ms()
            ));
            std::fs::write(
                &p,
                "frame=3\nfps=5.0\nprogress=continue\nframe=17\nout_time_ms=3400000\nprogress=end\n",
            )
            .unwrap();
            assert_eq!(parse_last_frame(&p), 17, "应取最后一个 frame= 值");
            std::fs::remove_file(&p).unwrap();
            assert_eq!(
                parse_last_frame(Path::new("/nonexistent/aura-t4-progress")),
                0,
                "文件缺失应返回 0 而非 panic"
            );
        }

        /// ISS-008 复刻（同 Windows backend 测试）：控制线程自终止打 finished 戳后，SESSIONS 条目
        /// 经 [`reclaim_finished`] 的 TTL sweep 清除——自终止不再滞留至显式 stop。
        #[test]
        fn self_terminated_session_reclaimed_after_ttl() {
            let rec_id = "test-iss008-reclaim-linux".to_string();
            let shared = Arc::new(Shared::new());
            shared.finished_at.store(1_000, Ordering::Relaxed);
            sessions_guard().insert(
                rec_id.clone(),
                SessionEntry {
                    shared,
                    output_path: PathBuf::from("dummy.mp4"),
                    controller: None,
                },
            );
            reclaim_finished(1_000 + RECLAIM_TTL_MS - 1);
            assert!(
                sessions_guard().contains_key(&rec_id),
                "TTL 未到的自终止会话应保留以供 stop 取元数据"
            );
            reclaim_finished(1_000 + RECLAIM_TTL_MS + 1);
            assert!(
                !sessions_guard().contains_key(&rec_id),
                "自终止后超 TTL 的会话应被回收，sessions() 不含该 rec_id"
            );
        }
    }
}

// ===== macOS 采集后端：CGDisplayCreateImage 帧循环 + ffmpeg rawvideo 管道纯编码（批G）=====
//
// 帧源 = screen.rs mac 采集面（CG 直采 + SCK 回退，虚拟屏实证可用）；编码 = ffmpeg 子进程
// （`-f rawvideo -pixel_format rgba` 从 stdin 收帧 → libx264 → MP4）。macOS 26 上整屏采集类
// 现成路线实测全灭（screencapture -v 对虚拟屏挂死、AVCaptureScreenInput 被移除），故取
// 「截图同源帧循环 + 子进程编码」混成：会话簿记/封顶/看门狗复刻 Windows 骨架（进程内帧循环、
// frame_count 逐帧真值），子进程收尾纪律复刻 Linux（宽限 + 强杀兜底、stderr 尾段诊断透出）。

#[cfg(target_os = "macos")]
mod backend {
    use std::collections::HashMap;
    use std::io::Write;
    use std::path::{Path, PathBuf};
    use std::process::{Child, ChildStdin, Command, Stdio};
    use std::sync::atomic::{AtomicBool, AtomicI64, AtomicU64, Ordering};
    use std::sync::{Arc, Mutex, OnceLock};
    use std::thread::JoinHandle;
    use std::time::{Duration, Instant};

    use aura_capability::{CapError, RecordArtifact, RecordParams};

    // 安全上限（Locked-8）与时钟 helper：文件级 caps mod 三平台共享（六值零改）。
    use super::caps::{
        now_unix_ms, BITRATE, CONTROL_POLL, DEFAULT_DURATION_SECS, DEFAULT_FPS, MAX_DURATION_SECS,
        MAX_FPS, RECLAIM_TTL_MS, WATCHDOG_TIMEOUT_MS,
    };

    /// EOF 优雅收尾宽限：采帧线程关 stdin 后 ffmpeg 排空编码队列 + 写 trailer/faststart 二次
    /// pass；宽限内未退才升级 SIGKILL（产物大概率缺 moov，如实标 truncated）。同 Linux 取值。
    const STOP_GRACE: Duration = Duration::from_secs(10);
    /// spawn 后启动观察窗：窗口内子进程即退视为启动失败（ffmpeg 缺失/参数错），读 stderr
    /// 尾段透出结构化错误（同 Linux）。
    const SPAWN_PROBE: Duration = Duration::from_millis(500);

    /// 会话共享态：采帧线程（写 frame_count/last_frame_ms）+ 控制线程（写 truncated/halt/
    /// finished_at）+ stop 调用（写 stop_requested）+ stop 读取。骨架复刻 Windows backend；
    /// halt 为 mac 特有——控制线程是唯一停止决策方，采帧线程只观察 halt 不自行决策。
    struct Shared {
        /// 已入管帧数（逐帧真值，同 Windows 语义）。
        frame_count: AtomicU64,
        /// 最近成功入管帧的 Unix 毫秒（看门狗据此判帧源停滞）。0=尚无帧。
        last_frame_ms: AtomicI64,
        /// 是否被封顶/看门狗/编码器异常退出强制截断。
        truncated: AtomicBool,
        /// 显式停止请求（stop_recording 置位，控制线程观察后决策）。
        stop_requested: AtomicBool,
        /// 控制线程 → 采帧线程的停机信号（任何停止原因统一经此下达）。
        halt: AtomicBool,
        /// 会话起点（时长封顶 + duration 计算，只读 Copy 字段）。
        started: Instant,
        /// 控制线程退出时刻（Unix 毫秒，0=运行中）。封顶/看门狗自终止后 stop_recording 未必
        /// 到来，据此 + TTL 惰性回收 SESSIONS 滞留条目（ISS-008），防 JoinHandle/map 泄漏。
        finished_at: AtomicI64,
    }

    impl Shared {
        fn new() -> Self {
            Self {
                frame_count: AtomicU64::new(0),
                last_frame_ms: AtomicI64::new(0),
                truncated: AtomicBool::new(false),
                stop_requested: AtomicBool::new(false),
                halt: AtomicBool::new(false),
                started: Instant::now(),
                finished_at: AtomicI64::new(0),
            }
        }
    }

    /// 活动会话条目：共享态 + 落盘路径 + 控制线程句柄（stop 时 join 保证子进程已收尾、产物落盘完成）。
    struct SessionEntry {
        shared: Arc<Shared>,
        output_path: PathBuf,
        controller: Option<JoinHandle<()>>,
    }

    /// 进程内会话注册表（rec_id → 活动会话）。复刻 Windows/Linux backend OnceLock 惰性单例先例。
    static SESSIONS: OnceLock<Mutex<HashMap<String, SessionEntry>>> = OnceLock::new();

    fn sessions() -> &'static Mutex<HashMap<String, SessionEntry>> {
        SESSIONS.get_or_init(|| Mutex::new(HashMap::new()))
    }

    /// 取会话注册表锁（批E D6：毒锁不再二次 panic）。语义同 Windows/Linux backend 同名函数。
    fn sessions_guard() -> std::sync::MutexGuard<'static, HashMap<String, SessionEntry>> {
        sessions().lock().unwrap_or_else(std::sync::PoisonError::into_inner)
    }

    /// 惰性回收已终止且超 [`RECLAIM_TTL_MS`] 的 SESSIONS 条目（自终止但未显式 stop 者，ISS-008）。
    /// 语义同 Windows/Linux backend 同名函数。
    fn reclaim_finished(now_ms: i64) {
        sessions_guard().retain(|_, e| {
            let fin = e.shared.finished_at.load(Ordering::Relaxed);
            fin == 0 || now_ms.saturating_sub(fin) < RECLAIM_TTL_MS
        });
    }

    /// 参数封顶（纯函数，单测面）：语义与 Windows/Linux backend 完全一致。
    fn clamp_params(params: &RecordParams) -> (u32, u64) {
        let fps = params.fps.unwrap_or(DEFAULT_FPS).clamp(1, MAX_FPS);
        let duration_secs = params
            .max_duration_secs
            .unwrap_or(DEFAULT_DURATION_SECS)
            .clamp(1, MAX_DURATION_SECS);
        (fps, duration_secs)
    }

    /// ffmpeg 可执行定位（纯函数骨架，单测面）：AURA_FFMPEG_PATH 显式覆写 → 常见安装位
    /// （/opt/homebrew/bin、/usr/local/bin——launchd 会话 PATH 不含二者，不能裸靠 PATH）→
    /// 裸 "ffmpeg" 兜底（交互 shell 场景）。mac 无系统包管理器预装 ffmpeg，生产部署为
    /// /usr/local/bin 落静态单文件二进制。
    fn ffmpeg_program() -> String {
        if let Ok(p) = std::env::var("AURA_FFMPEG_PATH") {
            if !p.is_empty() {
                return p;
            }
        }
        for cand in ["/opt/homebrew/bin/ffmpeg", "/usr/local/bin/ffmpeg"] {
            if Path::new(cand).exists() {
                return cand.to_string();
            }
        }
        "ffmpeg".to_string()
    }

    /// ffmpeg 纯编码命令组装（纯函数，单测面）：
    ///   - 输入 `-f rawvideo -pixel_format rgba -video_size WxH -framerate fps -i -`（stdin 管道，
    ///     几何/帧率由采帧侧保证；无 `-nostdin`——stdin 即帧数据面，与 Linux x11grab 相反）；
    ///   - 无 `-t` 时长封顶：时长由采帧线程墙钟封顶后关 stdin 实现（EOF 即收尾）；节点亡故时
    ///     管道自断 EOF，孤儿保护由管道语义提供（对照 Linux 靠 `-t` 兜底）；
    ///   - crop 向下取偶对齐 Windows `& !1` / Linux 同款滤镜（libx264 yuv420p 要求偶数边）；
    ///   - 码率取共享 caps::BITRATE（P3 三平台对齐）；`-movflags +faststart` 前置 moov 可流播。
    fn build_encode_command(
        program: &str,
        fps: u32,
        width: u32,
        height: u32,
        output_path: &Path,
    ) -> Vec<String> {
        vec![
            program.to_string(),
            "-y".to_string(),
            "-f".to_string(),
            "rawvideo".to_string(),
            "-pixel_format".to_string(),
            "rgba".to_string(),
            "-video_size".to_string(),
            format!("{width}x{height}"),
            "-framerate".to_string(),
            fps.to_string(),
            "-i".to_string(),
            "-".to_string(),
            "-an".to_string(),
            "-c:v".to_string(),
            "libx264".to_string(),
            "-pix_fmt".to_string(),
            "yuv420p".to_string(),
            "-b:v".to_string(),
            BITRATE.to_string(),
            "-vf".to_string(),
            "crop=trunc(iw/2)*2:trunc(ih/2)*2".to_string(),
            "-movflags".to_string(),
            "+faststart".to_string(),
            output_path.display().to_string(),
        ]
    }

    /// 控制线程停止决策（纯函数，单测面）：语义与 Windows/Linux 逐字对齐——显式 stop 正常
    /// 收尾=false；封顶/看门狗=true（同轮竞态封顶语义优先）；无触发=None 继续。
    fn stop_decision(stop_requested: bool, over_max: bool, starved: bool) -> Option<bool> {
        if stop_requested || over_max || starved {
            Some(over_max || starved)
        } else {
            None
        }
    }

    /// 读 stderr 日志尾段（启动失败诊断透出，按 UTF-8 字符边界截取；文件缺失 → 空串）。同 Linux。
    fn stderr_tail(stderr_path: &Path, max_bytes: usize) -> String {
        let Ok(content) = std::fs::read_to_string(stderr_path) else {
            return String::new();
        };
        let mut start = content.len().saturating_sub(max_bytes);
        while !content.is_char_boundary(start) {
            start += 1;
        }
        content[start..].trim().to_string()
    }

    /// 产物同目录辅助文件路径 `<output>.<ext>`（同 Linux；mac 仅 stderr 日志一件，无 progress——
    /// 帧活性由进程内 last_frame_ms 逐帧真值承担，无需子进程侧回报）。
    fn aux_path(output_path: &Path, ext: &str) -> PathBuf {
        let mut s = output_path.as_os_str().to_owned();
        s.push(format!(".{ext}"));
        PathBuf::from(s)
    }

    /// 到期补帧数（纯函数，单测面）：CFR 时间轴对齐墙钟。rawvideo 管道无逐帧时间戳，ffmpeg 按
    /// `-framerate` 恒速解读——mac 高分辨率 CG 采集单帧成本可达数百 ms（3360x2100 实测 ~2fps），
    /// 采集追不上 fps 时若一帧只写一次，48 帧墙钟 22.6s 会被 5fps CFR 解读成 9.6s 的 2.35× 快放。
    /// 故以当前帧补齐所有到期 tick（重复帧=画面保持，正是低帧率屏录语义）：target = 已流逝 tick
    /// 数 +1（首帧占 tick 0），dups = 距 target 缺口，下限 1（本帧至少写一次）、上限 MAX_FPS
    /// （1s 量，防采集长停后爆发灌管；持续性落后由 target 差值自然限速，不会超写）。
    fn frames_due(elapsed_secs: f64, fps: u32, emitted: u64) -> u64 {
        let target = (elapsed_secs * f64::from(fps)) as u64 + 1;
        target.saturating_sub(emitted).clamp(1, u64::from(MAX_FPS))
    }

    /// 采帧线程：按 fps 节拍 `capture_monitor_rgba` 逐帧取材写入 ffmpeg stdin，采集慢于节拍时
    /// 经 [`frames_due`] 补帧对齐墙钟（回放时序真实）。观察 halt 停机；写失败（EPIPE=编码器
    /// 亡故）即退出（控制线程 try_wait 收尸并定性）；采集瞬时失败不中断（看门狗按 last_frame_ms
    /// 停滞判定持续性故障）；几何漂移帧（显示器重配）丢弃——rawvideo 逐帧字节数契约不容尺寸
    /// 变化，混入即花屏。退出时 drop stdin 关管道 → ffmpeg 收 EOF 收尾。
    fn writer_loop(
        mut stdin: ChildStdin,
        shared: Arc<Shared>,
        fps: u32,
        display: Option<u32>,
        expect_w: u32,
        expect_h: u32,
    ) {
        let interval = Duration::from_secs_f64(1.0 / fps.max(1) as f64);
        // 探采首帧已由 start_recording 入管（占 tick 0）。
        let mut emitted: u64 = 1;
        loop {
            if shared.halt.load(Ordering::Relaxed) {
                break;
            }
            match crate::screen::backend::capture_monitor_rgba(display) {
                Ok((rgba, w, h)) if w == expect_w && h == expect_h => {
                    let dups =
                        frames_due(shared.started.elapsed().as_secs_f64(), fps, emitted);
                    let mut wrote_all = true;
                    for _ in 0..dups {
                        if stdin.write_all(&rgba).is_err() {
                            wrote_all = false;
                            break;
                        }
                    }
                    if !wrote_all {
                        // 编码器亡故（EPIPE）：退出，由控制线程 try_wait 收尸并标截断。
                        break;
                    }
                    emitted += dups;
                    shared.frame_count.fetch_add(dups, Ordering::Relaxed);
                    shared.last_frame_ms.store(now_unix_ms(), Ordering::Relaxed);
                }
                // 几何漂移/采集瞬时失败：丢帧不中断，持续性故障由看门狗判停。
                Ok(_) | Err(_) => {}
            }
            // 睡至下一到期 tick（emitted 帧覆盖到 t0 + emitted*interval）；落后即刻续采。
            let due = shared.started + interval * (emitted.min(u64::from(u32::MAX)) as u32);
            let now = Instant::now();
            if due > now {
                std::thread::sleep(due - now);
            }
        }
        // drop(stdin) 关管道：ffmpeg 收 EOF 排空编码队列并写 trailer/faststart 落盘。
    }

    /// 控制线程：单一停止决策方——观察显式停止 / 时长封顶 / 看门狗（last_frame_ms 停滞）/
    /// 编码器异常退出，触发即置 halt 令采帧线程退出关 stdin，随后等 ffmpeg EOF 优雅收尾。
    /// 采帧线程卡死在阻塞 write（编码器 wedge、管道满）时宽限后杀子进程以 EPIPE 解锁——
    /// 一切退出路径均 join 采帧线程 + wait 收口子进程（无悬挂线程、无僵尸）。
    fn controller_loop(
        mut child: Child,
        writer: JoinHandle<()>,
        shared: Arc<Shared>,
        max_duration: Duration,
        stderr_path: PathBuf,
    ) {
        loop {
            std::thread::sleep(CONTROL_POLL);
            // 编码器自行退出 = 异常（正常收尾必经显式 stop/封顶路径先关 stdin）：截断态。
            match child.try_wait() {
                Ok(Some(_)) => {
                    shared.truncated.store(true, Ordering::Relaxed);
                    break;
                }
                Ok(None) => {}
                Err(_) => {
                    let _ = child.kill();
                    let _ = child.wait();
                    shared.truncated.store(true, Ordering::Relaxed);
                    break;
                }
            }
            let last = shared.last_frame_ms.load(Ordering::Relaxed);
            // 看门狗：已出过帧后超阈值无新帧 → 帧源停滞（采集持续失败/写阻塞）。首帧未出
            // （last==0）由时长封顶兜底（start 前已探采成功一帧，首帧缺席仅剩极端竞态）。
            let starved = last != 0 && (now_unix_ms() - last) >= WATCHDOG_TIMEOUT_MS;
            let over_max = shared.started.elapsed() >= max_duration;
            let stop = shared.stop_requested.load(Ordering::Relaxed);
            if let Some(truncated) = stop_decision(stop, over_max, starved) {
                if truncated {
                    shared.truncated.store(true, Ordering::Relaxed);
                }
                break;
            }
        }
        // 停机下达：采帧线程观察 halt 退出并关 stdin（EOF 收尾起点）。
        shared.halt.store(true, Ordering::Relaxed);
        // 采帧线程收口：正常路径下一节拍即退；卡死在阻塞 write（编码器 wedge）时宽限后杀
        // 子进程令写端 EPIPE 解锁（JoinHandle 无带时限 join，以 is_finished 轮询等价实现）。
        let writer_deadline = Instant::now() + STOP_GRACE;
        while !writer.is_finished() && Instant::now() < writer_deadline {
            std::thread::sleep(Duration::from_millis(50));
        }
        if !writer.is_finished() {
            let _ = child.kill();
            shared.truncated.store(true, Ordering::Relaxed);
        }
        let _ = writer.join();
        // 子进程收口：EOF 排空 + faststart 二次 pass 需时，宽限内未退 SIGKILL 兜底（产物
        // 大概率缺 moov，如实标截断）。
        let deadline = Instant::now() + STOP_GRACE;
        let mut exited = false;
        while Instant::now() < deadline {
            if let Ok(Some(_)) = child.try_wait() {
                exited = true;
                break;
            }
            std::thread::sleep(Duration::from_millis(100));
        }
        if !exited {
            let _ = child.kill();
            let _ = child.wait();
            shared.truncated.store(true, Ordering::Relaxed);
        }
        let _ = std::fs::remove_file(&stderr_path);
        shared.finished_at.store(now_unix_ms(), Ordering::Relaxed);
    }

    pub(super) async fn start_recording(
        rec_id: String,
        params: RecordParams,
        output_path: PathBuf,
    ) -> Result<(), CapError> {
        // 探采 + 子进程 spawn 为阻塞调用，收敛进 spawn_blocking（同 Windows/Linux）。
        tokio::task::spawn_blocking(move || -> Result<(), CapError> {
            reclaim_finished(now_unix_ms());
            if sessions_guard().contains_key(&rec_id) {
                return Err(CapError::InvalidArg(format!(
                    "recording session already exists: {rec_id}"
                )));
            }
            let (fps, duration_secs) = clamp_params(&params);
            let max_duration = Duration::from_secs(duration_secs);
            // 首帧探采：几何真值（rawvideo -video_size 必须与逐帧字节数逐一吻合）+ 授权/
            // 显示器序号 fail-fast（无屏幕录制授权/序号越界在此结构化报错，不起烂尾子进程）。
            let (probe, width, height) = crate::screen::backend::capture_monitor_rgba(params.display)?;

            let program = ffmpeg_program();
            let argv = build_encode_command(&program, fps, width, height, &output_path);
            let stderr_path = aux_path(&output_path, "log");
            let stderr_file = std::fs::File::create(&stderr_path).map_err(|e| {
                CapError::Internal(format!("create recorder stderr log failed: {e}"))
            })?;
            let mut child = Command::new(&argv[0])
                .args(&argv[1..])
                .stdin(Stdio::piped())
                .stdout(Stdio::null())
                .stderr(Stdio::from(stderr_file))
                .spawn()
                .map_err(|e| {
                    CapError::CaptureFailed(format!(
                        "spawn ffmpeg failed: {e} (expected at /usr/local/bin/ffmpeg or AURA_FFMPEG_PATH)"
                    ))
                })?;
            let mut stdin = child
                .stdin
                .take()
                .ok_or_else(|| CapError::Internal("ffmpeg stdin pipe missing".to_string()))?;

            // 启动失败快速路径：观察窗内即退（ffmpeg 参数错/编码器缺失），读 stderr 尾段透出。
            std::thread::sleep(SPAWN_PROBE);
            if let Ok(Some(status)) = child.try_wait() {
                let tail = stderr_tail(&stderr_path, 800);
                let _ = std::fs::remove_file(&stderr_path);
                return Err(CapError::CaptureFailed(format!(
                    "ffmpeg exited at startup ({status}): {tail}"
                )));
            }

            let shared = Arc::new(Shared::new());
            // 探采帧直接作首帧入管（不弃真材）；写失败视同启动失败（管道即断=子进程已烂）。
            if stdin.write_all(&probe).is_err() {
                let _ = child.kill();
                let _ = child.wait();
                let tail = stderr_tail(&stderr_path, 800);
                let _ = std::fs::remove_file(&stderr_path);
                return Err(CapError::CaptureFailed(format!(
                    "ffmpeg rejected first frame: {tail}"
                )));
            }
            shared.frame_count.store(1, Ordering::Relaxed);
            shared.last_frame_ms.store(now_unix_ms(), Ordering::Relaxed);

            let writer = {
                let shared = shared.clone();
                let display = params.display;
                std::thread::Builder::new()
                    .name("aura-rec-frames".into())
                    .spawn(move || writer_loop(stdin, shared, fps, display, width, height))
                    .map_err(|e| CapError::Internal(format!("spawn recording writer failed: {e}")))?
            };
            let controller = {
                let shared = shared.clone();
                std::thread::Builder::new()
                    .name("aura-rec-ctrl".into())
                    .spawn(move || controller_loop(child, writer, shared, max_duration, stderr_path))
                    .map_err(|e| {
                        CapError::Internal(format!("spawn recording controller failed: {e}"))
                    })?
            };

            sessions_guard().insert(
                rec_id,
                SessionEntry {
                    shared,
                    output_path,
                    controller: Some(controller),
                },
            );
            Ok(())
        })
        .await
        .map_err(|e| CapError::Internal(format!("start_recording task join failed: {e}")))?
    }

    pub(super) async fn stop_recording(rec_id: String) -> Result<RecordArtifact, CapError> {
        tokio::task::spawn_blocking(move || -> Result<RecordArtifact, CapError> {
            let entry = sessions_guard().remove(&rec_id).ok_or_else(|| {
                CapError::InvalidArg(format!("no active recording session: {rec_id}"))
            })?;

            // 请求停止 → 控制线程置 halt → 采帧线程关 stdin → ffmpeg EOF 收尾；join 保证产物
            // 落盘完成（含 faststart 二次 pass）。
            entry.shared.stop_requested.store(true, Ordering::Relaxed);
            if let Some(h) = entry.controller {
                let _ = h.join();
            }

            let frame_count = entry.shared.frame_count.load(Ordering::Relaxed);
            let truncated = entry.shared.truncated.load(Ordering::Relaxed);
            let duration_ms = entry.shared.started.elapsed().as_millis() as u64;
            let size_bytes = std::fs::metadata(&entry.output_path).map(|m| m.len()).unwrap_or(0);
            // 子进程路线下零字节/缺失产物 = 编码器未产出任何数据（失败态），结构化报错（同 Linux）。
            if size_bytes == 0 {
                return Err(CapError::CaptureFailed(
                    "recording produced no output (ffmpeg wrote zero bytes)".to_string(),
                ));
            }

            Ok(RecordArtifact {
                mime: "video/mp4".to_string(),
                size_bytes,
                duration_ms,
                frame_count,
                truncated,
            })
        })
        .await
        .map_err(|e| CapError::Internal(format!("stop_recording task join failed: {e}")))?
    }

    #[cfg(test)]
    mod tests {
        use super::*;

        /// 命令组装 shape：rawvideo 管道输入、几何/帧率透传、码率取安全 const、偶数边 crop、
        /// faststart、输出路径末位；且**无 -nostdin**（stdin 即帧数据面）、**无 -t**（时长由
        /// 采帧侧墙钟封顶 + EOF 语义实现）。
        #[test]
        fn mac_build_encode_command_shape() {
            let argv = build_encode_command(
                "/usr/local/bin/ffmpeg",
                5,
                3360,
                2100,
                Path::new("/tmp/aura/recordings/r1.mp4"),
            );
            assert_eq!(argv[0], "/usr/local/bin/ffmpeg");
            let joined = argv.join(" ");
            assert!(joined.contains("-f rawvideo"), "输入应为 rawvideo 管道: {joined}");
            assert!(joined.contains("-pixel_format rgba"), "像素格式应为 rgba: {joined}");
            assert!(joined.contains("-video_size 3360x2100"), "几何应透传: {joined}");
            assert!(joined.contains("-framerate 5"), "帧率应透传: {joined}");
            assert!(joined.contains("-i -"), "输入应为 stdin: {joined}");
            assert!(
                joined.contains(&format!("-b:v {BITRATE}")),
                "码率应取安全 const BITRATE: {joined}"
            );
            assert!(joined.contains("-movflags +faststart"), "moov 应前置: {joined}");
            assert!(!joined.contains("-nostdin"), "stdin 是帧数据面，禁 -nostdin: {joined}");
            assert!(!joined.contains(" -t "), "时长封顶由采帧侧实现，无 -t: {joined}");
            assert_eq!(argv.last().unwrap(), "/tmp/aura/recordings/r1.mp4", "输出路径应为末位参数");
        }

        /// 参数封顶：语义与 Windows/Linux backend 一致（共享 caps 常量）。
        #[test]
        fn mac_params_clamp() {
            let p = |fps, dur| RecordParams {
                fps,
                max_duration_secs: dur,
                display: None,
            };
            assert_eq!(clamp_params(&p(Some(99), Some(999))), (MAX_FPS, MAX_DURATION_SECS));
            assert_eq!(clamp_params(&p(None, None)), (DEFAULT_FPS, DEFAULT_DURATION_SECS));
            assert_eq!(clamp_params(&p(Some(0), Some(0))), (1, 1));
        }

        /// truncated 三态判定（逐字对齐 Windows/Linux）。
        #[test]
        fn mac_truncated_semantics() {
            assert_eq!(stop_decision(false, false, false), None);
            assert_eq!(stop_decision(true, false, false), Some(false), "显式 stop 不截断");
            assert_eq!(stop_decision(false, true, false), Some(true), "时长封顶截断");
            assert_eq!(stop_decision(false, false, true), Some(true), "看门狗截断");
            assert_eq!(stop_decision(true, true, false), Some(true), "竞态封顶语义优先");
        }

        /// 补帧对齐墙钟：跟上节拍 → 1；落后 N tick → 补 N；长停 → 封顶 MAX_FPS；超前 → 仍 1
        /// （下限保证本帧至少写一次，禁 0 帧空转）。
        #[test]
        fn mac_frames_due_wallclock_alignment() {
            // tick 0 已发（emitted=1），0.2s@5fps 恰到 tick 1 → 补 1。
            assert_eq!(frames_due(0.2, 5, 1), 1, "跟上节拍应写 1 帧");
            // 2.0s@5fps 应有 11 帧（tick 0..10），已发 5 → 缺 6。
            assert_eq!(frames_due(2.0, 5, 5), 6, "落后应补齐缺口");
            // 长停 60s：缺口远超上限 → 封顶 MAX_FPS（1s 量）。
            assert_eq!(frames_due(60.0, 5, 1), u64::from(MAX_FPS), "爆发灌管应封顶");
            // 超前（emitted 已覆盖未来 tick）→ 下限 1。
            assert_eq!(frames_due(0.1, 5, 10), 1, "超前仍至少写 1 帧");
        }

        /// ffmpeg 定位：AURA_FFMPEG_PATH 显式覆写优先（存在性不校验——显式即信任）。
        #[test]
        fn mac_ffmpeg_program_env_override() {
            std::env::set_var("AURA_FFMPEG_PATH", "/custom/ffmpeg");
            assert_eq!(ffmpeg_program(), "/custom/ffmpeg");
            std::env::remove_var("AURA_FFMPEG_PATH");
            let p = ffmpeg_program();
            assert!(
                p == "/opt/homebrew/bin/ffmpeg" || p == "/usr/local/bin/ffmpeg" || p == "ffmpeg",
                "无覆写时应落常见安装位或裸名兜底: {p}"
            );
        }

        /// ISS-008 复刻（同 Windows/Linux backend 测试）：自终止条目经 TTL sweep 回收。
        #[test]
        fn self_terminated_session_reclaimed_after_ttl() {
            let rec_id = "test-iss008-reclaim-mac".to_string();
            let shared = Arc::new(Shared::new());
            shared.finished_at.store(1_000, Ordering::Relaxed);
            sessions_guard().insert(
                rec_id.clone(),
                SessionEntry {
                    shared,
                    output_path: PathBuf::from("dummy.mp4"),
                    controller: None,
                },
            );
            reclaim_finished(1_000 + RECLAIM_TTL_MS - 1);
            assert!(sessions_guard().contains_key(&rec_id), "TTL 未到应保留");
            reclaim_finished(1_000 + RECLAIM_TTL_MS + 1);
            assert!(!sessions_guard().contains_key(&rec_id), "超 TTL 应回收");
        }
    }
}
