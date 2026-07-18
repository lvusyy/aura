//! IosDriver：iOS 移动面设备驱动（M7 骨架）。
//!
//! 经 WebDriverAgent（WDA）HTTP 契约控制远程 iOS 设备，两后端共用同一份 driver 实现——
//! **模拟器**（主开发 / CI 路径，runner 共享宿主网络栈直连 localhost）与**真机 iPhone**
//! （验收锚点，经 tunneld 起 WDA + usbmux 转发 8100）差异全部下沉 provisioning 层（harness），
//! 与 M5 `AndroidDriver` 对 adb 契约同构（Redroid / 真机同协议）。驱动 host-agnostic——不以
//! `#[cfg(target_os)]` 门控（node 宿主为 mac，被控设备为 iOS sim/device，二者平台不同）。
//!
//! 分层：`IosDriver` → [`WdaClient`] 窄接口 adapter（唯一 WDA HTTP 出口，第三方隔离规约）→ WDA HTTP。
//! [`WdaClient`] 是**有状态** adapter（区别于 android `AdbCli` 的幂等 reconnect 模型）：缓存 WDA
//! session id 与运行时自推导的 device-scale（截图原生像素 ÷ WDA 逻辑点，analysis §3.2/§3.3）。
//!
//! **本文件（TASK-003）为骨架**：六族能力子 trait 全 stub（screen/input/process_file/a11y 返
//! `E_INTERNAL` 未实装、record 返 `E_UNSUPPORTED` defer），`platform()` 派生 `"ios"`。真实现由
//! TASK-005+ 逐族填充（WdaClient 方法 + 坐标 scale 校正 + session 状态机）；广告面 12/21 工具子集
//! （`supports_tool` 覆写剔 9，M11 增 audio_inject）归 TASK-004，本骨架继承默认全支持不预判。
//!
//! M5 学费固化：[`RecordDriver`] 无默认方法而 [`DeviceDriver`] bound 含之——必须显式桩 start/
//! stop_recording，漏任一即编译失败。iOS record 明确剔除（§3.5）：WDA 无原生录屏（模拟器
//! recordVideo 是宿主能力，两后端不一致）→ defer。

use std::path::PathBuf;
use std::sync::{Arc, Mutex};

use async_trait::async_trait;
use base64::engine::general_purpose::STANDARD;
use base64::Engine as _;

use aura_capability::{
    A11yDriver, A11yNode, A11yParams, A11yTree, Ack, AssertDriver, AudioDriver, AudioInjectParams,
    AudioInjectResult, CapError, CmdResult, Coordinate, DeviceDriver, DisplayInfo, FileResult,
    InputDriver, MouseButton, ProcessFileDriver, ProcessInfo, RecordArtifact, RecordDriver,
    RecordParams, Region, ScreenDriver, ScreenshotOpts, ScreenshotResult, ScrollDirection,
};

/// iOS WDA HTTP 端点默认值（M7 单实例固定 8100）。`--wda-url` 未显式给定、或工厂 / 测试 / 反连
/// 调用点传 `None` 时，[`build_driver`] 回退至此；池化多实例的每实例端口分配归 harness（M8）。
///
/// [`build_driver`]: crate::build_driver
pub(crate) const DEFAULT_WDA_URL: &str = "http://127.0.0.1:8100";

/// 截图缩放长边上限（XGA 最佳实践，与 android `SCREENSHOT_MAX_DIM` / 桌面 `screen::MAX_DIM` 一致）：
/// 长边 >1280px 时 `crate::screen::encode_scaled` 等比降采样再 WebP 编码，meta.scale 记录回映射比。
const SCREENSHOT_MAX_DIM: u32 = 1280;

/// iOS 录屏 defer 说明（§3.5）：WDA 无原生录屏，模拟器 recordVideo 属宿主侧、两后端不一致 → 剔除。
const RECORD_UNSUPPORTED: &str = "screen recording on iOS is unsupported: WDA has no native recording (simulator recordVideo is host-side, inconsistent across backends)";

// ===== 9 剔除工具（广告面 12/21，§3.5 + M11 audio）的永久不支持说明（`E_UNSUPPORTED`）=====
// 与「未实装支持项」（not_yet → E_INTERNAL）语义分层：剔除项 iOS 沙箱 / 触摸模型无等价能力，永久拒绝
// （CapError::Unsupported，types.rs:321 永久语义），非骨架占位；TASK-004 收敛，supports_tool 同步剔出广告面。

/// move_mouse 剔除（§3.5）：触摸输入无 hover 指针概念（同 android 触屏永久拒绝先例）。
const MOVE_MOUSE_UNSUPPORTED: &str =
    "move_mouse on iOS is unsupported: touch input has no hover pointer concept";
/// 进程域剔除（§3.5）：应用沙箱无进程枚举 / pid 通路（list_processes / kill_process）。
const PROCESS_UNSUPPORTED: &str =
    "process control on iOS is unsupported: app sandbox exposes no process enumeration or pid channel";
/// 文件域剔除（§3.5）：真机沙箱无通用文件通道（file_push / file_pull；两后端子集保持一致）。
const FILE_UNSUPPORTED: &str =
    "file transfer on iOS is unsupported: device sandbox has no general file channel";
/// run_command 剔除（§3.5）：设备无 shell。
const RUN_COMMAND_UNSUPPORTED: &str = "run_command on iOS is unsupported: no device shell";
/// audio_inject 剔除（M11）：iOS 沙箱无宿主侧音频注入通路（WDA 无音频注入端点）。
const AUDIO_UNSUPPORTED: &str =
    "audio_inject on iOS is unsupported: no host-side audio injection path via WDA";

/// 骨架期**未实装支持项**的统一错误（`E_INTERNAL`）——12 支持工具（assert 除外）待 TASK-005+ 逐族
/// 以 WDA 真实现替换。区别于 8 剔除项的 `E_UNSUPPORTED` 永久拒绝（见上剔除说明常量）。
fn not_yet(method: &str) -> CapError {
    CapError::Internal(format!("ios {method} not yet implemented"))
}

// ADAPTER: WebDriverAgent HTTP —— 第三方依赖（WDA REST 契约）经此窄接口隔离（规约：第三方
// SDK/API 一律经窄接口 adapter 隔离消费）。业务层（六族 trait impl）只经 WdaClient 方法出入，
// 不直接拼装 / 解析 WDA HTTP 细节；WDA 契约漂移的修复收敛于本 adapter 单文件。
//
// 有状态（analysis §3.3，区别于 AdbCli 幂等 reconnect）：sessionless 端点（GET /status /screenshot
// /source）零 session 依赖；有状态端点（tap/keys/actions/window/size）惰性建 session 并缓存 sid，
// 失效分级恢复（L1 重建 session / L2 探 /status 不通报 E_UNAVAILABLE / L3 设备重置=harness）。
// device_scale 首次截图时自推导（截图像素宽 ÷ WDA 逻辑宽）并缓存，禁硬编码。
//
// TASK-005 已填充窄面：get_status/screenshot/source（sessionless 读）+ window_size/actions/keys/
// press_button（有状态）+ ensure_session（惰性全局 session）+ L1（invalid session 重建重试一次）/
// L2（传输不通探 /status 报 E_UNAVAILABLE）+ 超时纪律（连 3s / 读 15s / /source 45s hung 熔断），
// 经 ureq 3.3.0（default-features=false，localhost 无 TLS）；纯函数与常量下沉子模块 `wda` 便离线单测。
#[derive(Debug)]
struct WdaClient {
    /// WDA HTTP 基地址（如 `http://127.0.0.1:8100`），由构造参 `wda_url` 注入。
    base_url: String,
    /// 运行时自推导并缓存的 device-scale（截图原生像素 ÷ WDA 逻辑点，§3.2）；首次截图前为 `None`。
    device_scale: Mutex<Option<f64>>,
    /// 惰性建立并缓存的 WDA session id（§3.3 有状态端点用）；未建 session 前为 `None`。
    sid: Mutex<Option<String>>,
}

impl WdaClient {
    /// 以 WDA HTTP 基地址构造。session / device-scale 缓存起始为空，由首次相应调用惰性填充。
    fn new(base_url: String) -> Self {
        WdaClient {
            base_url,
            device_scale: Mutex::new(None),
            sid: Mutex::new(None),
        }
    }
}

/// WdaClient 纯函数层（URL 构建 / sessionless 分类 / invalid-session 判定 / 超时选择 / JSON 信封解析）。
/// 下沉子模块便离线单测（无需 live WDA，同 android `reconnect_plan`/`is_device_disconnected` 纯函数先例）。
mod wda {
    use super::CapError;
    use serde_json::Value;
    use std::time::Duration;

    /// 连接建立超时（§3.3）：3s。localhost WDA 握手应瞬时，超此即判连接层异常。
    pub(crate) const CONNECT_TIMEOUT: Duration = Duration::from_secs(3);
    /// 默认整调用超时（§3.3 读 15s）：连接 + 收发全程上限，作 hung 熔断防阻塞单-writer。
    pub(crate) const READ_TIMEOUT: Duration = Duration::from_secs(15);
    /// `/source` 专属整调用超时（§3.3）：45s。深 UI 树快照耗时，单独放宽；超时即 fail 不自动重试（hung 熔断）。
    pub(crate) const SOURCE_READ_TIMEOUT: Duration = Duration::from_secs(45);
    /// 建 session 后 WDA 快照深度上限（§3.3 appium settings）：50（Appium 量级），防 /source 遍历爆栈 / 超时。
    /// 本层 a11y 另有 depth/max_nodes 后过滤（types.rs A11yParams），此为 WDA 侧硬上限，二者正交。
    pub(crate) const SNAPSHOT_MAX_DEPTH: u32 = 50;
    /// 响应体读取上限（32 MiB）：覆盖高分辨率截图 base64 PNG 与深 /source XML，防 ureq 默认 10MB 截断。
    pub(crate) const MAX_BODY_BYTES: u64 = 32 * 1024 * 1024;
    /// L2 不可达信息：WDA 进程重拉归 harness（node 不拥有 WDA，§3.4）。
    pub(crate) const WDA_UNREACHABLE: &str = "WDA unreachable; harness must re-provision runner";

    /// sessionless 端点清单（§3.3）：无 sid 依赖，读路径（截图 / a11y / 健康 / 锁屏态）直连 base；其余
    /// （tap/keys/actions/window/size 等）为有状态，须惰性建 session 后置 `/session/{sid}` 下。
    pub(crate) const SESSIONLESS: &[&str] = &[
        "/status",
        "/screenshot",
        "/source",
        "/wda/lock",
        "/wda/unlock",
        "/wda/locked",
    ];

    /// WDA HTTP 方法（窄面仅 GET/POST；PUT/DELETE 不在 M7 契约面）。
    #[derive(Debug, Clone, Copy)]
    pub(crate) enum HttpMethod {
        Get,
        Post,
    }

    /// 组装 WDA URL：sessionless（sid=None）直挂 base；有状态（sid=Some）置于 `/session/{sid}` 下。
    /// `path` 恒以 `/` 起（可含 query，如 `/source?format=xml`），`base` 末尾 `/` 归一避免 `//`。
    pub(crate) fn wda_url(base: &str, path: &str, sid: Option<&str>) -> String {
        let base = base.trim_end_matches('/');
        match sid {
            Some(s) => format!("{base}/session/{s}{path}"),
            None => format!("{base}{path}"),
        }
    }

    /// 判定路径是否 sessionless（§3.3 [`SESSIONLESS`] 清单）。比对剥 query（`/source?format=xml`→`/source`）。
    pub(crate) fn is_sessionless(path: &str) -> bool {
        let p = path.split('?').next().unwrap_or(path);
        SESSIONLESS.contains(&p)
    }

    /// 判定响应是否 session 失效（§3.3 L1 触发）：HTTP 404 或响应体含 `invalid session id`（大小写不敏感）。
    pub(crate) fn is_invalid_session(status: u16, body: &str) -> bool {
        status == 404 || body.to_ascii_lowercase().contains("invalid session id")
    }

    /// 依路径选整调用超时（§3.3）：`/source` 45s（深树快照），其余 15s。比对剥 query。
    pub(crate) fn read_timeout_for(path: &str) -> Duration {
        let p = path.split('?').next().unwrap_or(path);
        if p == "/source" {
            SOURCE_READ_TIMEOUT
        } else {
            READ_TIMEOUT
        }
    }

    /// 校验 2xx：非 2xx 经 `ctor`（如 `CapError::CaptureFailed`/`CapError::InputFailed`）构语义错误
    /// （invalid-session 已在 [`super::WdaClient::stateful_raw`] 先行处理，故此处非 2xx 属真失败）。
    pub(crate) fn require_2xx(
        status: u16,
        body: String,
        path: &str,
        ctor: fn(String) -> CapError,
    ) -> Result<String, CapError> {
        if (200..300).contains(&status) {
            Ok(body)
        } else {
            Err(ctor(format!(
                "WDA {path} returned HTTP {status}: {}",
                body.trim()
            )))
        }
    }

    /// 解出 WDA 响应信封 `value`（字符串形，截图 base64 / source XML 共用）。信封：`{"value":..,"sessionId":..}`。
    pub(crate) fn parse_wda_value_str(body: &str) -> Option<String> {
        let v: Value = serde_json::from_str(body).ok()?;
        v.get("value")?.as_str().map(|s| s.to_string())
    }

    /// 解出 WDA session id：兼容 JSONWP（顶层 `sessionId`）与 W3C（`value.sessionId`）两形，顶层优先。
    pub(crate) fn parse_session_id(body: &str) -> Option<String> {
        let v: Value = serde_json::from_str(body).ok()?;
        if let Some(s) = v.get("sessionId").and_then(Value::as_str) {
            return Some(s.to_string());
        }
        v.get("value")?
            .get("sessionId")?
            .as_str()
            .map(|s| s.to_string())
    }

    /// 解出 WDA 窗口逻辑尺寸 `value.{width,height}`（整数亦经 `as_f64`，供 device-scale 除法）。
    pub(crate) fn parse_window_size(body: &str) -> Option<(f64, f64)> {
        let v: Value = serde_json::from_str(body).ok()?;
        let val = v.get("value")?;
        let w = val.get("width")?.as_f64()?;
        let h = val.get("height")?.as_f64()?;
        Some((w, h))
    }

    /// device-scale 运行时自推导（§3.2，坐标校正根 / devil #1）：`截图原生像素宽 ÷ WDA 逻辑点宽`。
    /// 机型无关（@2x 模拟器 828/414=2.0、@3x 真机 1170/390=3.0 自适配，**禁硬编码**）。逻辑宽 ≤0
    /// （异常 window/size）→ `None`（不产 inf/NaN scale 污染下游坐标校正）。纯函数便离线单测（无 live WDA）。
    pub(crate) fn derive_device_scale(native_w: u32, logical_w: f64) -> Option<f64> {
        if logical_w <= 0.0 {
            return None;
        }
        Some(native_w as f64 / logical_w)
    }
}

use wda::*;

// WdaClient 窄面网络方法 + session 状态机（§3.3）。方法同步阻塞（ureq），须由调用方经
// `spawn_blocking` 在阻塞线程调用，勿直接跑 async worker（sid `std::sync::Mutex` 单-writer）。
impl WdaClient {
    // ---- sessionless 读窄面（§3.3，零 session 依赖）----

    /// 健康探针（sessionless `GET /status`）：2xx 即 WDA 就绪。传输不通 → `E_UNAVAILABLE`
    /// （/status 本身即探针，L2 直判不可达，见 [`WdaClient::map_transport_failure`]）。
    // F5 保留：生产路径经 map_transport_failure 直发 /status（http_call），本方法当前仅 wda_tests
    // 的 L2 分级验证消费；保留窄面健康探针语义完整（harness 探活候选），allow 收窄至本方法。
    #[allow(dead_code)]
    fn get_status(&self) -> Result<(), CapError> {
        let (status, body) = self.sessionless_raw(HttpMethod::Get, "/status", None)?;
        require_2xx(status, body, "/status", CapError::Unavailable).map(|_| ())
    }

    /// 截图字节（sessionless `GET /screenshot` → 信封 `value` base64 PNG 解码）。供 TASK-006 缩放管线 +
    /// device-scale 自推导。
    fn get_screenshot_png(&self) -> Result<Vec<u8>, CapError> {
        let (status, body) = self.sessionless_raw(HttpMethod::Get, "/screenshot", None)?;
        let body = require_2xx(status, body, "/screenshot", CapError::CaptureFailed)?;
        let b64 = parse_wda_value_str(&body).ok_or_else(|| {
            CapError::CaptureFailed(format!("WDA /screenshot response missing value: {body}"))
        })?;
        STANDARD.decode(b64.trim()).map_err(|e| {
            CapError::CaptureFailed(format!("WDA screenshot base64 decode failed: {e}"))
        })
    }

    /// UI 源树 XML（sessionless `GET /source?format=xml` → 信封 `value` XML 串）。读超时 45s 独立上限
    /// （§3.3 hung 熔断），超时即返错不自动重试。供 TASK-006 XCUIElementType → A11yNode 映射。
    fn get_source_xml(&self) -> Result<String, CapError> {
        let (status, body) = self.sessionless_raw(HttpMethod::Get, "/source?format=xml", None)?;
        let body = require_2xx(status, body, "/source", CapError::CaptureFailed)?;
        parse_wda_value_str(&body).ok_or_else(|| {
            CapError::CaptureFailed(format!("WDA /source response missing value: {body}"))
        })
    }

    // ---- 有状态窄面（§3.3，惰性全局 session + L1 恢复）----

    /// 窗口逻辑尺寸（有状态 `GET /session/{sid}/window/size` → `{width,height}`）。供 TASK-006
    /// device-scale 自推导（截图原生像素宽 ÷ 逻辑宽）。
    fn get_window_size(&self) -> Result<(f64, f64), CapError> {
        let (status, body) = self.stateful_raw(HttpMethod::Get, "/window/size", None)?;
        let body = require_2xx(status, body, "/window/size", CapError::CaptureFailed)?;
        parse_window_size(&body)
            .ok_or_else(|| CapError::CaptureFailed(format!("WDA /window/size parse failed: {body}")))
    }

    /// W3C 手势（有状态 `POST /session/{sid}/actions`）：调用方（TASK-006）构造 pointer 链 JSON，本层只负责
    /// session 注入 + 发送（横屏坐标 bug #798 促用 /actions 而非 /wda/tap，§3.2）。
    fn post_actions(&self, w3c_json: &str) -> Result<(), CapError> {
        let (status, body) =
            self.stateful_raw(HttpMethod::Post, "/actions", Some(w3c_json.to_string()))?;
        require_2xx(status, body, "/actions", CapError::InputFailed).map(|_| ())
    }

    /// 焦点文本输入（有状态 `POST /session/{sid}/wda/keys`，body `{"value":[text]}`）。需键盘焦点，无焦点
    /// 由 WDA 返非 2xx → InputFailed（闭环序保证先 tap 聚焦，§3.5）。
    fn post_keys(&self, text: &str) -> Result<(), CapError> {
        let body = serde_json::json!({ "value": [text] }).to_string();
        let (status, body) = self.stateful_raw(HttpMethod::Post, "/wda/keys", Some(body))?;
        require_2xx(status, body, "/wda/keys", CapError::InputFailed).map(|_| ())
    }

    /// 硬件键（有状态 `POST /session/{sid}/wda/pressButton`，body `{"name":name}`；如 home/volumeup）。
    fn press_button(&self, name: &str) -> Result<(), CapError> {
        let body = serde_json::json!({ "name": name }).to_string();
        let (status, body) = self.stateful_raw(HttpMethod::Post, "/wda/pressButton", Some(body))?;
        require_2xx(status, body, "/wda/pressButton", CapError::InputFailed).map(|_| ())
    }

    // ---- session 状态机内核（§3.3）----

    /// 惰性建全局 session 并缓存 sid（单-writer：`sid` Mutex 全程持锁串行化，杜绝并发重复建 session）。
    /// 空 `capabilities` = 无 bundleId = systemwide 全局作用域（XCUITest 跨 app：SpringBoard tap 开 app
    /// 不断 session，§3.3）。建成后设 `snapshotMaxDepth`（best-effort）。传输不通 → L2 `E_UNAVAILABLE`。
    fn ensure_session(&self) -> Result<String, CapError> {
        let mut guard = self.sid.lock().unwrap_or_else(|e| e.into_inner());
        if let Some(sid) = guard.as_ref() {
            return Ok(sid.clone());
        }
        let (status, body) = match self.http_call(
            HttpMethod::Post,
            "/session",
            None,
            Some(r#"{"capabilities":{}}"#),
        ) {
            Ok(r) => r,
            Err(e) => return Err(self.map_transport_failure("/session", e)),
        };
        if !(200..300).contains(&status) {
            return Err(CapError::Internal(format!(
                "WDA POST /session failed (HTTP {status}): {}",
                body.trim()
            )));
        }
        let sid = parse_session_id(&body).ok_or_else(|| {
            CapError::Internal(format!("WDA /session response missing sessionId: {body}"))
        })?;
        // 截深树（§3.3）：best-effort，失败不阻断（session 已可用，仅深树防护缺席）。
        let _ = self.set_snapshot_max_depth(&sid);
        *guard = Some(sid.clone());
        Ok(sid)
    }

    /// 清缓存 sid（L1 失效恢复：invalid session → 清后重建重试一次）。
    fn clear_sid(&self) {
        *self.sid.lock().unwrap_or_else(|e| e.into_inner()) = None;
    }

    /// 建 session 后设 WDA 快照深度上限（`POST /session/{sid}/appium/settings`）。直接 [`WdaClient::http_call`]
    /// 挂 sid，不经 [`WdaClient::stateful_raw`]，避免 `ensure_session` 递归自锁。
    fn set_snapshot_max_depth(&self, sid: &str) -> Result<(), CapError> {
        let body =
            serde_json::json!({ "settings": { "snapshotMaxDepth": SNAPSHOT_MAX_DEPTH } }).to_string();
        let (status, resp) =
            self.http_call(HttpMethod::Post, "/appium/settings", Some(sid), Some(&body))?;
        if (200..300).contains(&status) {
            Ok(())
        } else {
            Err(CapError::Internal(format!(
                "WDA appium/settings HTTP {status}: {}",
                resp.trim()
            )))
        }
    }

    /// sessionless 请求（sid=None）+ L2 传输失败分级。路径须属 [`SESSIONLESS`]（debug 断言守卫）。
    fn sessionless_raw(
        &self,
        method: HttpMethod,
        path: &str,
        json_body: Option<String>,
    ) -> Result<(u16, String), CapError> {
        debug_assert!(is_sessionless(path), "sessionless_raw got stateful path: {path}");
        self.http_call(method, path, None, json_body.as_deref())
            .map_err(|e| self.map_transport_failure(path, e))
    }

    /// 有状态请求：惰性 session 注入 + L1（invalid session 重建重试一次）+ L2（传输失败分级）。
    /// 路径须为 `/session/{sid}` 下后缀（非 [`SESSIONLESS`]，debug 断言守卫）。
    fn stateful_raw(
        &self,
        method: HttpMethod,
        path: &str,
        json_body: Option<String>,
    ) -> Result<(u16, String), CapError> {
        debug_assert!(!is_sessionless(path), "stateful_raw got sessionless path: {path}");
        let sid = self.ensure_session()?;
        match self.http_call(method, path, Some(&sid), json_body.as_deref()) {
            Ok((status, body)) if is_invalid_session(status, &body) => {
                // L1：sid 失效 → 清缓存 → 重建 session → 重试一次（仅一次，§3.3）。
                self.clear_sid();
                let sid2 = self.ensure_session()?;
                self.http_call(method, path, Some(&sid2), json_body.as_deref())
                    .map_err(|e| self.map_transport_failure(path, e))
            }
            Ok(ok) => Ok(ok),
            Err(e) => Err(self.map_transport_failure(path, e)),
        }
    }

    /// L2 分级（§3.3）：传输失败时探 `GET /status`——不通 → `E_UNAVAILABLE`（WDA 重拉归 harness）；通则说明
    /// 本调用另有其因（原样上抛，如 /source 超时但 WDA 活着）。`/status` 自身失败免二次探测直判不可达。
    fn map_transport_failure(&self, path: &str, original: CapError) -> CapError {
        if path == "/status" {
            return CapError::Unavailable(WDA_UNREACHABLE.to_string());
        }
        match self.http_call(HttpMethod::Get, "/status", None, None) {
            Ok(_) => original,
            Err(_) => CapError::Unavailable(WDA_UNREACHABLE.to_string()),
        }
    }

    /// 唯一 ureq 出入口（第三方隔离核心）。`http_status_as_error(false)`：4xx/5xx 亦返 `Ok((status,body))` 以便
    /// 读体判 invalid session（§3.3 L1）；仅传输层失败（超时 / 连接拒绝 / DNS / IO）返 `Err`，交 L2 分级。连接
    /// 3s + 整调用 [`read_timeout_for`]（/source 45s 其余 15s）作 hung 熔断（每调用独立 agent，localhost 无池化必要）。
    fn http_call(
        &self,
        method: HttpMethod,
        path: &str,
        sid: Option<&str>,
        json_body: Option<&str>,
    ) -> Result<(u16, String), CapError> {
        let url = wda_url(&self.base_url, path, sid);
        let agent: ureq::Agent = ureq::Agent::config_builder()
            .timeout_connect(Some(CONNECT_TIMEOUT))
            .timeout_global(Some(read_timeout_for(path)))
            .http_status_as_error(false)
            .build()
            .into();
        let sent = match method {
            HttpMethod::Get => agent.get(&url).call(),
            HttpMethod::Post => agent
                .post(&url)
                .header("Content-Type", "application/json")
                .send(json_body.unwrap_or("{}")),
        };
        let mut resp = sent.map_err(|e| {
            // 中性传输错误；是否升级 E_UNAVAILABLE 由 map_transport_failure 依 /status 探针裁定。
            let kind = if matches!(e, ureq::Error::Timeout(_)) {
                "timed out (hung breaker)"
            } else {
                "transport error"
            };
            CapError::CaptureFailed(format!("WDA {path} {kind}: {e}"))
        })?;
        let status = resp.status().as_u16();
        let text = resp
            .body_mut()
            .with_config()
            .limit(MAX_BODY_BYTES)
            .read_to_string()
            .map_err(|e| CapError::CaptureFailed(format!("WDA {path} body read failed: {e}")))?;
        Ok((status, text))
    }

    // ---- device-scale 自推导（§3.2，坐标校正根 / devil #1 CRITICAL）----

    /// 首帧 device-scale 自推导并缓存（§3.2）：`native_w（截图原生像素宽）÷ WDA 逻辑点宽`
    /// （`GET /window/size`）。已缓存则直返（**缓存一次**，机型无关：@2x 模拟器 / @3x 真机自适配，
    /// 禁硬编码）。`device_scale` Mutex 全程持锁串行化，杜绝并发首帧重复推导（单-writer，同 sid 状态机；
    /// 锁序 device_scale→sid 无环，不与他路交织死锁）。供 [`super::IosDriver::screenshot`] 首帧调用；
    /// 逻辑宽 ≤0 → `E_CAPTURE_FAILED`（不产 inf scale 静默毁全部坐标）。
    fn ensure_device_scale(&self, native_w: u32) -> Result<f64, CapError> {
        let mut guard = self.device_scale.lock().unwrap_or_else(|e| e.into_inner());
        if let Some(s) = *guard {
            return Ok(s);
        }
        let (logical_w, _) = self.get_window_size()?;
        let scale = derive_device_scale(native_w, logical_w).ok_or_else(|| {
            CapError::CaptureFailed(format!(
                "WDA window logical width {logical_w} invalid for device-scale derivation"
            ))
        })?;
        *guard = Some(scale);
        Ok(scale)
    }

    /// device-scale 访问器（供 TASK-007/008 坐标校正消费：native 像素 ÷ scale = WDA 逻辑点）。返回缓存值；
    /// 未推导（无截图先行）则惰性兜底——拉一帧 `/screenshot` 取原生宽 + `/window/size` 逻辑宽推导缓存。
    /// 闭环序 screenshot 恒先行时直命中缓存快路径。**须在 `spawn_blocking` 阻塞线程调用**（惰性兜底走
    /// 同步 ureq，同本 impl 其余方法；调用方克隆 `Arc<WdaClient>` 移入闭包）。
    pub(crate) fn device_scale(&self) -> Result<f64, CapError> {
        // 快路径：已缓存直返（锁在语句末即释放，不跨网络持锁）。
        if let Some(s) = *self.device_scale.lock().unwrap_or_else(|e| e.into_inner()) {
            return Ok(s);
        }
        // 惰性兜底：无截图先行，拉一帧取原生宽后推导（ensure_device_scale 内部再判缓存，防并发重复推导）。
        let png = self.get_screenshot_png()?;
        let (_, native_w, _) = decode_png_rgba(&png)?;
        self.ensure_device_scale(native_w)
    }
}

// ===== PNG 解码 adapter（WDA /screenshot 出 base64 PNG，非 android screencap raw RGBA）=====

// ADAPTER: png crate 第三方依赖经本节窄面隔离消费——业务层（screenshot/zoom）只见 RGBA8 裸像素 +
// (w,h)，不触 png 解码细节；png API 漂移的修复收敛于此。EXPAND 展开调色板 / 低位深灰度、STRIP_16 归
// 16bit→8bit，再按 color_type 统一补齐到 RGBA8（[`png_buf_to_rgba8`]）。

/// 解码 PNG 字节（`WdaClient::get_screenshot_png` 已解 base64 得 PNG 原始字节）→ 归一 RGBA8 像素缓冲
/// （长度 = w*h*4）+ `(w, h)`。前置本步再喂平台无关 `crate::screen::encode_scaled` XGA+WebP 流水线
/// （复用 android 同管线，零重复 image/decoder 依赖）。各色彩类型 / 位深统一 RGBA8（risks[0] 缓解：
/// WDA PNG 非 RGBA8/16bit/palette 亦归一，不静默产错像素）。
fn decode_png_rgba(bytes: &[u8]) -> Result<(Vec<u8>, u32, u32), CapError> {
    // png 0.18 Decoder 需 `BufRead + Seek`；`&[u8]` 无 Seek，经 Cursor 包裹满足（零拷贝，借用底层切片）。
    let mut decoder = png::Decoder::new(std::io::Cursor::new(bytes));
    // EXPAND：palette→RGB、<8bit gray→8bit、tRNS→alpha；STRIP_16：16bit→8bit。下方再按 color_type 补 RGBA8。
    decoder.set_transformations(png::Transformations::EXPAND | png::Transformations::STRIP_16);
    let mut reader = decoder
        .read_info()
        .map_err(|e| CapError::CaptureFailed(format!("WDA screenshot PNG read_info failed: {e}")))?;
    // png crate 默认解码限额下缓冲不下 → None（防解压炸弹 OOM）。
    let out_size = reader.output_buffer_size().ok_or_else(|| {
        CapError::CaptureFailed("WDA screenshot PNG output buffer too large".to_string())
    })?;
    let mut buf = vec![0u8; out_size];
    let info = reader
        .next_frame(&mut buf)
        .map_err(|e| CapError::CaptureFailed(format!("WDA screenshot PNG decode failed: {e}")))?;
    let (w, h) = (info.width, info.height);
    let rgba = png_buf_to_rgba8(&buf, info.color_type, w, h)?;
    Ok((rgba, w, h))
}

/// 归一 png 解码输出（经 EXPAND+STRIP_16 后为 8bit）到 RGBA8：RGB 补 alpha=255、Grayscale 复制到三通道
/// 补 alpha、GrayscaleAlpha 灰度复制三通道保留 alpha、RGBA 直通。Indexed（EXPAND 后不应残留）→
/// `E_CAPTURE_FAILED`（不静默产错像素）。入参 `buf` 长度须 ≥ 像素数×通道数。
fn png_buf_to_rgba8(
    buf: &[u8],
    color_type: png::ColorType,
    w: u32,
    h: u32,
) -> Result<Vec<u8>, CapError> {
    let px = (w as usize)
        .checked_mul(h as usize)
        .ok_or_else(|| CapError::CaptureFailed(format!("PNG dims overflow: {w}x{h}")))?;
    let short = |need: usize| {
        CapError::CaptureFailed(format!(
            "PNG buffer too short: {} bytes for {w}x{h} {color_type:?} (need {need})",
            buf.len()
        ))
    };
    let mut out = Vec::with_capacity(px * 4);
    match color_type {
        png::ColorType::Rgba => {
            let need = px * 4;
            if buf.len() < need {
                return Err(short(need));
            }
            out.extend_from_slice(&buf[..need]);
        }
        png::ColorType::Rgb => {
            let need = px * 3;
            if buf.len() < need {
                return Err(short(need));
            }
            for c in buf[..need].chunks_exact(3) {
                out.extend_from_slice(&[c[0], c[1], c[2], 255]);
            }
        }
        png::ColorType::GrayscaleAlpha => {
            let need = px * 2;
            if buf.len() < need {
                return Err(short(need));
            }
            for c in buf[..need].chunks_exact(2) {
                out.extend_from_slice(&[c[0], c[0], c[0], c[1]]);
            }
        }
        png::ColorType::Grayscale => {
            let need = px;
            if buf.len() < need {
                return Err(short(need));
            }
            for &g in &buf[..need] {
                out.extend_from_slice(&[g, g, g, 255]);
            }
        }
        png::ColorType::Indexed => {
            return Err(CapError::CaptureFailed(
                "PNG still indexed after EXPAND transform".to_string(),
            ));
        }
    }
    Ok(out)
}

/// iOS 设备驱动。经 [`WdaClient`] adapter 走 WDA HTTP 控制目标 iOS 设备（模拟器 / 真机）。
/// `Debug` 便于反连侧结构化日志（同 android `AndroidDriver`）。
#[derive(Debug)]
pub struct IosDriver {
    /// 目标设备序列号（`--serial` 承载 udid，sim/device 通用）。保留供真机 udid 诊断日志；
    /// **udid→WDA 端口的翻译归 provisioning 层**（harness），driver 不据此拨号（§3.1）。
    // F5 保留：暂无非测代码读取（udid 诊断日志归后续 harness/反连日志接线），allow 收窄至本字段。
    #[allow(dead_code)]
    serial: String,
    /// WDA HTTP 出口 adapter。`Arc` 包裹：screenshot/zoom 及 TASK-007/008 input/a11y 须将其克隆移入
    /// `spawn_blocking` 阻塞线程调用同步 ureq 方法（阻塞边界铁律），故共享所有权。
    wda: Arc<WdaClient>,
}

impl IosDriver {
    /// 以设备序列号（udid）与 WDA HTTP 端点构造。空 serial = 未指定设备（provisioning 层单实例默认）。
    pub fn new(serial: String, wda_url: String) -> Self {
        IosDriver {
            serial,
            wda: Arc::new(WdaClient::new(wda_url)),
        }
    }

    /// 在阻塞线程执行一次 WDA 同步输入操作并折叠为 [`Ack`]（spawn_blocking 边界铁律：WdaClient 同步
    /// ureq 方法须离开 async worker，同 [`IosDriver::screenshot`] 先例）。闭包内可连续调 `device_scale`
    /// / `post_actions` / `post_keys` / `press_button` 等同步方法（克隆 `Arc<WdaClient>` 移入 'static
    /// 闭包）；join 失败 → `E_INPUT_FAILED`。
    async fn wda_input<F>(&self, op: F) -> Result<Ack, CapError>
    where
        F: FnOnce(&WdaClient) -> Result<(), CapError> + Send + 'static,
    {
        let wda = Arc::clone(&self.wda);
        tokio::task::spawn_blocking(move || op(&wda))
            .await
            .map_err(|e| CapError::InputFailed(format!("ios input task join failed: {e}")))?
            .map(|()| Ack::ok())
    }
}

// ===== 六族能力子 trait（DeviceDriver bound 缺一族即编译失败，M5 学费）=====
// 分层（§3.5 iOS 子集 12/20）：支持项（screen 4 / input 支持 6：click/type/key/scroll/drag/wait / a11y 1）
// 返 E_INTERNAL 未实装占位，TASK-005+ 逐族替换为 WDA 真实现；剔除项（input move_mouse / process_file 5 /
// record 2）返 E_UNSUPPORTED 永久拒绝（TASK-004 收敛，见上剔除说明常量 + supports_tool 覆写 + contract_matrix）。

#[async_trait]
impl ScreenDriver for IosDriver {
    async fn screenshot(
        &self,
        _display: Option<u32>,
        opts: ScreenshotOpts,
    ) -> Result<ScreenshotResult, CapError> {
        // iOS 单显示（M7）：display 忽略。WDA `GET /screenshot`（sessionless）出 base64 PNG——非 android
        // screencap raw RGBA，故前置 decode_png_rgba 解出 RGBA8 裸像素，再喂平台无关 encode_scaled
        // XGA+WebP 流水线（复用 android 同管线）。WDA 同步 ureq 调用 + PNG 解码 + 缩放编码全收敛进单个
        // spawn_blocking（阻塞边界铁律）；Arc<WdaClient> 克隆移入 'static 闭包。首帧自推导 device-scale
        // 缓存（§3.2 坐标校正根，devil #1）——推导失败即 fail-loud，不返回会致后续 tap 精度静默失败的截图。
        let wda = Arc::clone(&self.wda);
        tokio::task::spawn_blocking(move || -> Result<ScreenshotResult, CapError> {
            let png = wda.get_screenshot_png()?;
            let (pixels, w, h) = decode_png_rgba(&png)?;
            wda.ensure_device_scale(w)?;
            crate::screen::encode_scaled(
                pixels,
                w,
                h,
                opts.max_dim.unwrap_or(SCREENSHOT_MAX_DIM),
                opts.quality.unwrap_or(crate::screen::WEBP_QUALITY),
            )
        })
        .await
        .map_err(|e| CapError::CaptureFailed(format!("ios screenshot task join failed: {e}")))?
    }

    async fn zoom(&self, region: Region, opts: ScreenshotOpts) -> Result<ScreenshotResult, CapError> {
        // P0 契约 region=[x1,y1,x2,y2]（dispatch 已将区域坐标 pre-apply 至截图原生像素空间）→ 裁剪所需
        // (x,y,w,h)。WDA PNG 解出原生帧后内存裁剪该区域，缺省不降采样（max_dim=u32::MAX）保留原生分辨率
        // 以观察小字（同 android zoom 原生帧裁剪语义），显式传 max_dim 时以参数为准。zoom 纯截图后处理、
        // 不发 WDA 命令，故不涉 device-scale。
        let (x, y, w, h) = crate::screen::region_to_xywh(region)?;
        let wda = Arc::clone(&self.wda);
        tokio::task::spawn_blocking(move || -> Result<ScreenshotResult, CapError> {
            let png = wda.get_screenshot_png()?;
            let (full, iw, ih) = decode_png_rgba(&png)?;
            let cropped = crate::screen::crop_rgba(&full, iw, ih, x, y, w, h)?;
            crate::screen::encode_scaled(
                cropped,
                w,
                h,
                opts.max_dim.unwrap_or(u32::MAX),
                opts.quality.unwrap_or(crate::screen::WEBP_QUALITY),
            )
        })
        .await
        .map_err(|e| CapError::CaptureFailed(format!("ios zoom task join failed: {e}")))?
    }

    async fn list_displays(&self) -> Result<Vec<DisplayInfo>, CapError> {
        Err(not_yet("list_displays"))
    }

    async fn switch_display(&self, _display: u32) -> Result<DisplayInfo, CapError> {
        Err(not_yet("switch_display"))
    }
}

// ===== input 域：坐标校正 + W3C /actions 手势构建器（纯函数便离线单测，无需 live WDA）=====
// §3.2 坐标管线：dispatch 已将 agent 坐标 to_native 回映射到「截图原生像素」→ 本层统一 ÷device-scale
// 得「WDA 逻辑点」→ W3C pointer 链。手势一律 POST /actions（W3C）而非 /wda/tap（规避横屏坐标错映
// bug #798，§3.2）；坐标除法与 body 构建拆纯函数便离线单测（tap/swipe 形状 + 逻辑点钉死值）。

/// 右键 → 同点长按时长（ms）：600ms 触发 iOS 长按 / 上下文语义（同 android `LONG_PRESS_MS` 先例）。
const LONG_PRESS_MS: u64 = 600;
/// 拖拽 pointerMove 时长（ms）：300ms 触发拖拽而非快速 flick（同 android `DRAG_DURATION_MS`）。
const DRAG_DURATION_MS: u64 = 300;
/// 滚动 pointerMove 时长（ms）。
const SCROLL_DURATION_MS: u64 = 300;
/// 滚动单步基距（**native 像素**）：位移 = amount × 本值（native），随端点一并 ÷device-scale 归一到
/// 逻辑点（§3.2 统一除法；同 android `SCROLL_STEP_PX`，amount 默认 3 → 300 native px）。
const SCROLL_STEP_PX: i32 = 100;

/// 原生像素 → WDA 逻辑点（§3.2 坐标校正核心，devil #1）：`native ÷ device-scale` 四舍五入到整点。
/// 机型无关（@2x：828÷2=414、@3x：1170÷3=390）。scale ≤0 兜底原样返回（[`WdaClient::device_scale`]
/// 恒 >0，此为纯函数防御，不产 inf/NaN 坐标污染下游）。纯函数便离线单测。
fn to_logical(native: i32, scale: f64) -> i32 {
    if scale <= 0.0 {
        return native;
    }
    (native as f64 / scale).round() as i32
}

/// 单指 touch pointer 链包裹为 W3C `POST /actions` 请求体：
/// `{"actions":[{"type":"pointer","id":"finger1","parameters":{"pointerType":"touch"},"actions":[steps]}]}`。
fn w3c_pointer_actions(steps: Vec<serde_json::Value>) -> String {
    serde_json::json!({
        "actions": [{
            "type": "pointer",
            "id": "finger1",
            "parameters": { "pointerType": "touch" },
            "actions": steps,
        }]
    })
    .to_string()
}

/// pointerMove 步：移到逻辑点 (x,y)，`duration_ms` 控制移动耗时（起始定位用 0=瞬时）。
fn ptr_move(x: i32, y: i32, duration_ms: u64) -> serde_json::Value {
    serde_json::json!({ "type": "pointerMove", "duration": duration_ms, "x": x, "y": y })
}

/// pointerDown 步（button 0 = 主触点）。
fn ptr_down() -> serde_json::Value {
    serde_json::json!({ "type": "pointerDown", "button": 0 })
}

/// pointerUp 步（button 0）。
fn ptr_up() -> serde_json::Value {
    serde_json::json!({ "type": "pointerUp", "button": 0 })
}

/// pause 步：原地保持 `duration_ms`（长按语义，不移动）。
fn ptr_pause(duration_ms: u64) -> serde_json::Value {
    serde_json::json!({ "type": "pause", "duration": duration_ms })
}

/// 轻点：移到 (x,y) → 按下 → 抬起（click Left）。
fn tap_actions(x: i32, y: i32) -> String {
    w3c_pointer_actions(vec![ptr_move(x, y, 0), ptr_down(), ptr_up()])
}

/// 长按：移到 (x,y) → 按下 → 原地 pause `hold_ms` → 抬起（click Right，iOS 无右键的近似语义，
/// 同 android Right→长按先例）。
fn long_press_actions(x: i32, y: i32, hold_ms: u64) -> String {
    w3c_pointer_actions(vec![ptr_move(x, y, 0), ptr_down(), ptr_pause(hold_ms), ptr_up()])
}

/// 滑动：移到起点 → 按下 → 用时 `duration_ms` 移到终点 → 抬起（drag / scroll 共用，同 android
/// `swipe_argv` 一函多用先例）。
fn swipe_actions(x1: i32, y1: i32, x2: i32, y2: i32, duration_ms: u64) -> String {
    w3c_pointer_actions(vec![
        ptr_move(x1, y1, 0),
        ptr_down(),
        ptr_move(x2, y2, duration_ms),
        ptr_up(),
    ])
}

/// 滚动位移向量（**native 像素**）：以 anchor 为起点沿「反向量」滑动——手指移动方向与内容滚动方向
/// 相反（Down=上滑露出下方内容 / Up=下滑 / Left=右滑 / Right=左滑）；amount 缩放距离（默认 3，
/// ≤0 兜底 1 步不产零位移），方向语义同 android。
fn scroll_delta(direction: ScrollDirection, amount: i32) -> (i32, i32) {
    let distance = amount.max(1) * SCROLL_STEP_PX;
    match direction {
        ScrollDirection::Down => (0, -distance),
        ScrollDirection::Up => (0, distance),
        ScrollDirection::Left => (distance, 0),
        ScrollDirection::Right => (-distance, 0),
    }
}

/// 命名键 → WDA `pressButton` 硬件按钮名（camelCase，WDA `fb_pressButton` 约定）。iOS 仅硬件键有此
/// 语义：home / 音量上下；其余（enter/back/tab 等 Android 特有软键）无映射返 None（上层判 Unsupported）。
/// 入参为 agent 侧 snake_case 键名（与 android `keycode_of` 契约表一致），映射到 WDA camelCase。
fn press_button_name(name: &str) -> Option<&'static str> {
    match name {
        "home" => Some("home"),
        "volume_up" => Some("volumeUp"),
        "volume_down" => Some("volumeDown"),
        _ => None,
    }
}

/// `/wda/keys` 失败诊断增强（devil #9：无 firstResponder 非静默丢弃）：WDA 无键盘焦点时 `FBKeyboard`
/// typeText 报错（keyboard 不可见 / 无 focus），原文偏晦涩——识别其签名则前缀明确提示并保留 WDA 原文；
/// 非焦点类 `InputFailed` 及其它错误码（如传输层 `E_UNAVAILABLE`）原样透传。纯函数便离线单测。
fn map_keys_error(e: CapError) -> CapError {
    match e {
        CapError::InputFailed(msg) if indicates_no_focus(&msg) => CapError::InputFailed(format!(
            "no firstResponder; tap to focus first (WDA: {})",
            msg.trim()
        )),
        other => other,
    }
}

/// 判定 WDA `/wda/keys` 错误文本是否指示「无键盘焦点 / firstResponder」（`FBKeyboard` 报错签名）。
fn indicates_no_focus(msg: &str) -> bool {
    let m = msg.to_ascii_lowercase();
    m.contains("keyboard")
        || m.contains("focus")
        || m.contains("first responder")
        || m.contains("firstresponder")
}

#[async_trait]
impl InputDriver for IosDriver {
    async fn click(&self, at: Coordinate, button: MouseButton) -> Result<Ack, CapError> {
        // 收 native 像素（dispatch 已 pre-apply to_native）→ ÷device-scale 得逻辑点 → W3C /actions。
        match button {
            // Left → 轻点（tap）。
            MouseButton::Left => {
                self.wda_input(move |wda| {
                    let scale = wda.device_scale()?;
                    wda.post_actions(&tap_actions(to_logical(at.x, scale), to_logical(at.y, scale)))
                })
                .await
            }
            // Right → 同点长按 600ms（iOS 无右键，长按替代上下文菜单语义，同 android 先例）。
            MouseButton::Right => {
                self.wda_input(move |wda| {
                    let scale = wda.device_scale()?;
                    wda.post_actions(&long_press_actions(
                        to_logical(at.x, scale),
                        to_logical(at.y, scale),
                        LONG_PRESS_MS,
                    ))
                })
                .await
            }
            // Middle → 无对应 iOS 触摸语义（call-time E_UNSUPPORTED，同 android 中键先例）。
            MouseButton::Middle => Err(CapError::Unsupported(
                "middle click has no iOS touch equivalent".to_string(),
            )),
        }
    }

    async fn type_text(&self, text: String) -> Result<Ack, CapError> {
        if text.is_empty() {
            return Ok(Ack::ok()); // 空文本无操作，视作成功（避免空 keys 打 WDA）。
        }
        // POST /wda/keys（需键盘焦点）；无 firstResponder → WDA 报错，经 map_keys_error 增强为明确
        // E_INPUT_FAILED（非静默丢弃，devil #9），保留 WDA 原文。
        self.wda_input(move |wda| wda.post_keys(&text).map_err(map_keys_error))
            .await
    }

    async fn key(&self, keys: String) -> Result<Ack, CapError> {
        let norm = keys.trim().to_ascii_lowercase();
        // 修饰符组合（如 "ctrl+c"）无 pressButton 表达 → call-time E_UNSUPPORTED（同 android 先例）。
        if norm.contains('+') {
            return Err(CapError::Unsupported(format!(
                "key chord '{keys}' unsupported on iOS (no modifier composition via pressButton)"
            )));
        }
        match press_button_name(&norm) {
            // 硬件键（home / 音量）→ POST /wda/pressButton。
            Some(name) => self.wda_input(move |wda| wda.press_button(name)).await,
            // 未知键（无 iOS 硬件按钮语义）→ call-time E_UNSUPPORTED。
            None => Err(CapError::Unsupported(format!(
                "unknown key '{keys}' has no iOS hardware button mapping"
            ))),
        }
    }

    async fn scroll(
        &self,
        at: Coordinate,
        direction: ScrollDirection,
        amount: i32,
    ) -> Result<Ack, CapError> {
        // 以 anchor 为起点沿反向量 swipe；位移在 native 空间算得，端点一并 ÷scale 归一到逻辑点（§3.2）。
        let (dx, dy) = scroll_delta(direction, amount);
        self.wda_input(move |wda| {
            let scale = wda.device_scale()?;
            wda.post_actions(&swipe_actions(
                to_logical(at.x, scale),
                to_logical(at.y, scale),
                to_logical(at.x + dx, scale),
                to_logical(at.y + dy, scale),
                SCROLL_DURATION_MS,
            ))
        })
        .await
    }

    async fn drag(&self, from: Coordinate, to: Coordinate) -> Result<Ack, CapError> {
        // 起止两点均 native（dispatch 已 to_native）→ 各 ÷scale 得逻辑点 → 300ms swipe（触发拖拽非 flick）。
        self.wda_input(move |wda| {
            let scale = wda.device_scale()?;
            wda.post_actions(&swipe_actions(
                to_logical(from.x, scale),
                to_logical(from.y, scale),
                to_logical(to.x, scale),
                to_logical(to.y, scale),
                DRAG_DURATION_MS,
            ))
        })
        .await
    }

    // move_mouse：iOS 剔除项（§3.5，触摸无 hover 指针）→ E_UNSUPPORTED 永久拒绝（非 not_yet 未实装）。
    async fn move_mouse(&self, _to: Coordinate) -> Result<Ack, CapError> {
        Err(CapError::Unsupported(MOVE_MOUSE_UNSUPPORTED.to_string()))
    }

    async fn wait(&self, duration_ms: u64) -> Result<Ack, CapError> {
        // 平台无关：本地 sleep，不经 WDA（同 android wait）。
        tokio::time::sleep(std::time::Duration::from_millis(duration_ms)).await;
        Ok(Ack::ok())
    }
}

// ProcessFileDriver 五方法对 iOS **整域剔除**（§3.5）：应用沙箱无进程枚举 / pid 通路 / 通用文件通道 /
// shell，全返 E_UNSUPPORTED 永久拒绝（非 not_yet 未实装）——supports_tool 同步剔出广告面（TASK-004）。
#[async_trait]
impl ProcessFileDriver for IosDriver {
    async fn list_processes(&self) -> Result<Vec<ProcessInfo>, CapError> {
        Err(CapError::Unsupported(PROCESS_UNSUPPORTED.to_string()))
    }

    async fn kill_process(&self, _pid: u32) -> Result<Ack, CapError> {
        Err(CapError::Unsupported(PROCESS_UNSUPPORTED.to_string()))
    }

    async fn file_push(
        &self,
        _remote_path: String,
        _content_base64: String,
    ) -> Result<FileResult, CapError> {
        Err(CapError::Unsupported(FILE_UNSUPPORTED.to_string()))
    }

    async fn file_pull(&self, _remote_path: String) -> Result<FileResult, CapError> {
        Err(CapError::Unsupported(FILE_UNSUPPORTED.to_string()))
    }

    async fn run_command(
        &self,
        _cmd: String,
        _args: Vec<String>,
        _timeout_ms: Option<u64>,
        _cwd: Option<String>,
    ) -> Result<CmdResult, CapError> {
        Err(CapError::Unsupported(RUN_COMMAND_UNSUPPORTED.to_string()))
    }
}

// ===== XCUIElement /source XML 手写解析 + XCUIElementType→A11yNode 净新映射（TASK-008）=====
// WDA `GET /source?format=xml` 出 XCUIElement 元素树（tag 即元素类型 `XCUIElementTypeButton`，属性
// type/name/label/value/enabled/visible/x/y/width/height）。schema 与 android uiautomator 完全不同
// （label/name/value/rect vs text/content-desc/bounds），故映射净新；解析器形状沿 android 手写风格但
// **本地实现**，不复用 android 私有 parser（避跨文件导出私有项耦合，少量重复换单文件内聚）。
// 关键差异（devil #1 未尽处闭合）：WDA rect 是**逻辑点**，须 ×device-scale(§3.2) 回射为**原生像素**，
// 与 screenshot 原生空间一致——android bounds 本就是原生像素直用（screencap 同空间），iOS 不乘则 harness
// 依 meta.scale 换算 click 目标脱靶。

/// 轻量 XML 元素：tag 名 + 属性 + 子节点（同 android `XmlEl`，本地副本避跨文件耦合）。
#[derive(Debug)]
struct XmlEl {
    tag: String,
    attrs: Vec<(String, String)>,
    children: Vec<XmlEl>,
}

impl XmlEl {
    /// 按名取属性值（线性扫描，元素属性数固定小）。
    fn attr(&self, key: &str) -> Option<&str> {
        self.attrs
            .iter()
            .find(|(k, _)| k == key)
            .map(|(_, v)| v.as_str())
    }
}

/// XML 实体解码：`&amp; &lt; &gt; &quot; &apos;` + 数字实体 `&#NN; / &#xHH;`。
fn xml_unescape(s: &str) -> String {
    if !s.contains('&') {
        return s.to_string();
    }
    let mut out = String::with_capacity(s.len());
    let mut rest = s;
    while let Some(amp) = rest.find('&') {
        out.push_str(&rest[..amp]);
        let tail = &rest[amp..];
        if let Some(semi) = tail.find(';') {
            let ent = &tail[1..semi];
            match ent {
                "amp" => out.push('&'),
                "lt" => out.push('<'),
                "gt" => out.push('>'),
                "quot" => out.push('"'),
                "apos" => out.push('\''),
                _ if ent.starts_with("#x") || ent.starts_with("#X") => {
                    if let Some(c) = u32::from_str_radix(&ent[2..], 16)
                        .ok()
                        .and_then(char::from_u32)
                    {
                        out.push(c);
                    }
                }
                _ if ent.starts_with('#') => {
                    if let Some(c) = ent[1..].parse::<u32>().ok().and_then(char::from_u32) {
                        out.push(c);
                    }
                }
                // 未知实体原样保留（不丢字符）。
                _ => {
                    out.push('&');
                    out.push_str(ent);
                    out.push(';');
                }
            }
            rest = &tail[semi + 1..];
        } else {
            // 无闭合分号：'&' 原样，继续扫描其后。
            out.push('&');
            rest = &tail[1..];
        }
    }
    out.push_str(rest);
    out
}

/// 解析单个标签体（`tag 名` + `key="value"` 属性序列）。value 经 [`xml_unescape`] 解码。
/// 全程 ASCII 字节扫描（`"`/`=`/空白均 <0x80，UTF-8 多字节序列不含这些字节，切片必在 char 边界）。
fn parse_tag_body(body: &str) -> (String, Vec<(String, String)>) {
    let body = body.trim();
    let bytes = body.as_bytes();
    let n = bytes.len();
    // tag 名 = 首个 ASCII 空白前。
    let mut t = 0;
    while t < n && !bytes[t].is_ascii_whitespace() {
        t += 1;
    }
    let tag = body[..t].to_string();
    let mut attrs = Vec::new();
    let mut k = t;
    while k < n {
        while k < n && bytes[k].is_ascii_whitespace() {
            k += 1;
        }
        if k >= n {
            break;
        }
        let key_start = k;
        while k < n && bytes[k] != b'=' && !bytes[k].is_ascii_whitespace() {
            k += 1;
        }
        let key = &body[key_start..k];
        while k < n && bytes[k] != b'=' {
            k += 1;
        }
        if k >= n {
            break;
        }
        k += 1; // 越过 '='
        while k < n && bytes[k].is_ascii_whitespace() {
            k += 1;
        }
        if k >= n || bytes[k] != b'"' {
            break; // 非引号值：WDA 属性恒双引号，异常则停止
        }
        k += 1; // 越过起始引号
        let val_start = k;
        while k < n && bytes[k] != b'"' {
            k += 1;
        }
        let raw_val = &body[val_start..k.min(n)];
        if k < n {
            k += 1; // 越过结束引号
        }
        if !key.is_empty() {
            attrs.push((key.to_string(), xml_unescape(raw_val)));
        }
    }
    (tag, attrs)
}

/// 手写扫描 XCUIElement `/source` XML → 元素树。识别开始/自闭合/结束标签与 `<?xml?>`/`<!--?-->` 声明，
/// 忽略标签间空白文本（XCUIElement 无有意义文本节点，全部数据在属性）。属性值内 `<>&"` 已实体化。
fn parse_xcui_xml(input: &str) -> Result<XmlEl, CapError> {
    let bytes = input.as_bytes();
    let n = bytes.len();
    let mut i = 0usize;
    let mut stack: Vec<XmlEl> = Vec::new();
    let mut root: Option<XmlEl> = None;
    let bad = |m: &str| CapError::CaptureFailed(format!("XCUIElement /source XML parse: {m}"));
    while i < n {
        while i < n && bytes[i] != b'<' {
            i += 1;
        }
        if i >= n {
            break;
        }
        if i + 1 < n && (bytes[i + 1] == b'?' || bytes[i + 1] == b'!') {
            // 声明 / 注释 / DOCTYPE：跳到 '>'。
            while i < n && bytes[i] != b'>' {
                i += 1;
            }
            i += 1;
            continue;
        }
        if i + 1 < n && bytes[i + 1] == b'/' {
            // 结束标签 </tag>：读标签名与栈顶比对（防交错），再跳到 '>'。
            let name_start = i + 2;
            let mut e = name_start;
            while e < n && bytes[e] != b'>' && !bytes[e].is_ascii_whitespace() {
                e += 1;
            }
            let close_name = &input[name_start..e.min(n)];
            while i < n && bytes[i] != b'>' {
                i += 1;
            }
            i += 1;
            let el = stack.pop().ok_or_else(|| bad("unbalanced closing tag"))?;
            if el.tag.as_str() != close_name {
                return Err(bad("mismatched closing tag"));
            }
            match stack.last_mut() {
                Some(parent) => parent.children.push(el),
                None => root = Some(el),
            }
            continue;
        }
        // 开始 / 自闭合标签：定位本标签结束 '>'。
        let tag_start = i + 1;
        let mut j = tag_start;
        while j < n && bytes[j] != b'>' {
            j += 1;
        }
        if j >= n {
            return Err(bad("unterminated tag"));
        }
        let self_closing = bytes[j - 1] == b'/';
        let inner_end = if self_closing { j - 1 } else { j };
        let (tag, attrs) = parse_tag_body(&input[tag_start..inner_end]);
        i = j + 1;
        let el = XmlEl {
            tag,
            attrs,
            children: Vec::new(),
        };
        if self_closing {
            match stack.last_mut() {
                Some(parent) => parent.children.push(el),
                None => root = Some(el),
            }
        } else {
            stack.push(el);
        }
    }
    if !stack.is_empty() {
        return Err(bad("unclosed tags"));
    }
    root.ok_or_else(|| bad("empty document"))
}

/// XCUIElement rect（`x`/`y`/`width`/`height` **逻辑点**属性）→ `[x,y,w,h]` **原生像素**（各 ×scale 取整，
/// §3.2）。属性缺失/非数字 → None；零/负面积（幽灵）→ None。scale 为运行时自推导 device-scale（禁硬编码）。
/// 与 android `parse_bounds` 差异关键：android bounds 已是原生像素直用，iOS 逻辑点须 ×scale 才同 screenshot 空间。
fn xcui_bounds(el: &XmlEl, scale: f64) -> Option<[i32; 4]> {
    let x: f64 = el.attr("x")?.trim().parse().ok()?;
    let y: f64 = el.attr("y")?.trim().parse().ok()?;
    let w: f64 = el.attr("width")?.trim().parse().ok()?;
    let h: f64 = el.attr("height")?.trim().parse().ok()?;
    if w <= 0.0 || h <= 0.0 {
        return None; // 零/负面积 = 幽灵
    }
    Some([
        (x * scale).round() as i32,
        (y * scale).round() as i32,
        (w * scale).round() as i32,
        (h * scale).round() as i32,
    ])
}

/// 遍历预算：剩余可创建节点数 + 是否已截断（命中 depth / max_nodes 上限）。与 android `Budget` 同语义。
struct Budget {
    remaining: u32,
    truncated: bool,
}

/// 将 XCUIElement XmlEl 映射为 [`A11yNode`]（幽灵过滤 + depth/max_nodes 预算截断 + rect ×scale）。
/// 幽灵判定：`visible="false"` 或 rect 缺失/非法/零面积——丢弃节点及其子树，不占预算（幽灵不应挤占
/// max_nodes）。映射：`type`→role（缺省回退 tag 名，二者在 WDA XML 同值）、`label` 优先空则 `name`→name、
/// `value`→value、rect ×scale→`[x,y,w,h]` 原生像素。
fn map_xcui_node(el: &XmlEl, depth: u32, budget: &mut Budget, scale: f64) -> Option<A11yNode> {
    if budget.remaining == 0 {
        budget.truncated = true;
        return None;
    }
    let visible = el.attr("visible").map(|v| v != "false").unwrap_or(true);
    let bounds = xcui_bounds(el, scale);
    if !visible || bounds.is_none() {
        return None; // 幽灵：丢弃节点 + 子树，不占预算
    }
    budget.remaining -= 1;
    // type 属性即 XCUIElementType（如 XCUIElementTypeButton）；缺省回退 tag 名（WDA XML 中二者同值）。
    let role = el.attr("type").unwrap_or(el.tag.as_str()).to_string();
    // label 优先（人类可读无障碍标签）空则回退 name 属性（无障碍标识符），同 android text 优先 content-desc。
    let label = el.attr("label").unwrap_or("");
    let name = if !label.is_empty() {
        label.to_string()
    } else {
        el.attr("name").unwrap_or("").to_string()
    };
    let value = el
        .attr("value")
        .filter(|s| !s.is_empty())
        .map(|s| s.to_string());
    let mut node = A11yNode {
        role,
        name,
        bounds,
        value,
        children: Vec::new(),
    };
    if depth == 0 {
        // 到达深度上限：确有子元素则标记截断（树未完整）。
        if !el.children.is_empty() {
            budget.truncated = true;
        }
    } else {
        for child in &el.children {
            if budget.remaining == 0 {
                budget.truncated = true;
                break;
            }
            if let Some(cn) = map_xcui_node(child, depth - 1, budget, scale) {
                node.children.push(cn);
            }
        }
    }
    Some(node)
}

/// 按角色剪枝：保留角色匹配的节点及其祖先路径（有匹配后代的节点亦保留），其余剪除。
/// role 为 None / 空串时调用方跳过本函数（与 android `prune_by_role` 同语义）。
fn prune_by_role(node: A11yNode, role: &str) -> Option<A11yNode> {
    let A11yNode {
        role: nr,
        name,
        bounds,
        value,
        children,
    } = node;
    let kept: Vec<A11yNode> = children
        .into_iter()
        .filter_map(|c| prune_by_role(c, role))
        .collect();
    if nr.eq_ignore_ascii_case(role) || !kept.is_empty() {
        Some(A11yNode {
            role: nr,
            name,
            bounds,
            value,
            children: kept,
        })
    } else {
        None
    }
}

/// 深度优先查找首个 `hasFocus="true"` 节点（root="focus" 选择器用；android 用 `focused`）。TASK-008 live
/// 冒烟实证：真实 WDA `format=xml` source 无 focus 类属性（全集 type/name/label/value/enabled/visible/
/// accessible/x/y/width/height/index/traits），故 iOS root="focus" 经 `unwrap_or(xml_root)` 优雅退化为
/// root（无匹配即取根，不脱靶）；焦点属性名/可得性（交互聚焦后 firstResponder 是否暴露）待 009 定夺。
fn find_focused(el: &XmlEl) -> Option<&XmlEl> {
    if el.attr("hasFocus") == Some("true") {
        return Some(el);
    }
    for c in &el.children {
        if let Some(f) = find_focused(c) {
            return Some(f);
        }
    }
    None
}

/// 由 XCUIElement XML 根 + [`A11yParams`] + device-scale 构建 [`A11yTree`]（纯函数便测）。
/// 顶层 nodes = 选定根的直接子节点（root="focus"/"focused" → focused 元素，否则 XML 根 =
/// `XCUIElementTypeApplication`，取其直接子同 android 取 `<hierarchy>` 直接子），各按 depth/max_nodes
/// 预算遍历（rect ×scale 回射原生像素）；role 指定则按角色剪枝。与 android `build_a11y_tree` 同形。
fn build_a11y_tree(xml_root: &XmlEl, params: &A11yParams, scale: f64) -> A11yTree {
    let sel = params.root.as_deref().map(|s| s.trim().to_ascii_lowercase());
    let subtree_root: &XmlEl = match sel.as_deref() {
        Some("focus") | Some("focused") => find_focused(xml_root).unwrap_or(xml_root),
        _ => xml_root,
    };
    let mut budget = Budget {
        remaining: params.max_nodes,
        truncated: false,
    };
    let mut nodes = Vec::new();
    for child in &subtree_root.children {
        if budget.remaining == 0 {
            budget.truncated = true;
            break;
        }
        if let Some(n) = map_xcui_node(child, params.depth, &mut budget, scale) {
            nodes.push(n);
        }
    }
    if let Some(role) = params.role.as_deref().filter(|r| !r.is_empty()) {
        nodes = nodes
            .into_iter()
            .filter_map(|n| prune_by_role(n, role))
            .collect();
    }
    A11yTree {
        nodes,
        truncated: budget.truncated,
    }
}

#[async_trait]
impl A11yDriver for IosDriver {
    async fn get_a11y_tree(&self, params: A11yParams) -> Result<A11yTree, CapError> {
        // WDA `GET /source?format=xml`（sessionless，45s hung 熔断，超时即 fail 不重试，§3.3）→ 手写解析
        // XCUIElement 树 → XCUIElementType→A11yNode 净新映射（rect ×device-scale 回射原生像素，与
        // screenshot 原生空间一致）+ 幽灵过滤 + depth/max_nodes 预算 + role 剪枝。device_scale 与 source
        // 均走同步 ureq，全收敛进单个 spawn_blocking（阻塞边界铁律）；Arc<WdaClient> 克隆移入 'static 闭包。
        // scale 先行取：闭环序 screenshot 恒先行时命中 device_scale 缓存快路径，否则惰性拉一帧推导（§3.2）。
        let wda = Arc::clone(&self.wda);
        tokio::task::spawn_blocking(move || -> Result<A11yTree, CapError> {
            let scale = wda.device_scale()?;
            let xml = wda.get_source_xml()?;
            let root = parse_xcui_xml(&xml)?;
            Ok(build_a11y_tree(&root, &params, scale))
        })
        .await
        .map_err(|e| CapError::CaptureFailed(format!("ios get_a11y_tree task join failed: {e}")))?
    }
}

// AssertDriver 为平台无关组合能力（text 纯逻辑 + a11y/image 复用本 driver 的 get_a11y_tree/
// screenshot），默认方法即全部实现；空 impl 并入 DeviceDriver bound（同 PlatformDriver/AndroidDriver）。
// TASK-008 后 get_a11y_tree 与 screenshot（006）均实装，text/a11y/image 三形态即可真实断言（不假 PASS）。
impl AssertDriver for IosDriver {}

// ===== TASK-008：XCUIElement 映射 + rect×scale + 幽灵/预算/剪枝/focus + assert 组合离线单测 =====
// 独立测试模块（与 contract_matrix/wda_tests/screen_tests + TASK-007 input 测试并存；紧邻 a11y 实装块
// 放置而非 EOF，规避与并行任务在文件尾追加测试模块的写冲突）。纯函数覆盖 XCUIElementType→A11yNode 映射
// （type→role/label→name/value→value）+ rect ×device-scale 原生像素钉死 + 幽灵过滤 + depth/max_nodes
// 预算截断 + role 剪枝 + root=focus，无 live WDA；assert(text) 复用 AssertDriver 空组合（纯逻辑无网络）。
#[cfg(test)]
mod a11y_tests {
    use super::{build_a11y_tree, parse_xcui_xml, IosDriver, DEFAULT_WDA_URL};
    use aura_capability::{
        A11yField, A11yParams, AssertDriver, AssertMode, AssertParams, DiffMethod, MatchType,
    };

    /// XCUIElement `/source?format=xml` 样例：Application > Window > 四子（Button/StaticText/Button/
    /// TextField），含幽灵（零面积 StaticText + visible=false Button）、label vs name 回退、value 属性、
    /// `&amp;` 实体、hasFocus 焦点子树。rect 均为**逻辑点**（build 时 ×scale 回射原生像素）。
    const XCUI_XML_FIXTURE: &str = r#"<?xml version="1.0" encoding="UTF-8"?>
<XCUIElementTypeApplication type="XCUIElementTypeApplication" name="MobileSafari" label="Safari" enabled="true" visible="true" x="0" y="0" width="390" height="844">
  <XCUIElementTypeWindow type="XCUIElementTypeWindow" name="" label="" enabled="true" visible="true" x="0" y="0" width="390" height="844">
    <XCUIElementTypeButton type="XCUIElementTypeButton" name="login_btn" label="Log In" enabled="true" visible="true" x="10" y="20" width="100" height="50" />
    <XCUIElementTypeStaticText type="XCUIElementTypeStaticText" name="ghost" label="" value="" enabled="true" visible="true" x="0" y="0" width="0" height="0" />
    <XCUIElementTypeButton type="XCUIElementTypeButton" name="hidden_btn" label="Hidden" enabled="true" visible="false" x="10" y="90" width="100" height="40" />
    <XCUIElementTypeTextField type="XCUIElementTypeTextField" name="search_field" label="" value="Search &amp; go" enabled="true" visible="true" hasFocus="true" x="10" y="160" width="200" height="30">
      <XCUIElementTypeStaticText type="XCUIElementTypeStaticText" name="ph" label="typed" enabled="true" visible="true" x="12" y="162" width="180" height="26" />
    </XCUIElementTypeTextField>
  </XCUIElementTypeWindow>
</XCUIElementTypeApplication>"#;

    /// 判据1：XCUIElementType→A11yNode 映射 + 幽灵过滤（scale=2.0）。type→role、label 优先→name、
    /// label 空回退 name 属性、value→value（含 `&` 实体解码）；零面积 StaticText + visible=false Button 均被过滤。
    #[test]
    fn xcui_map_filters_ghosts_and_maps_shape() {
        let root = parse_xcui_xml(XCUI_XML_FIXTURE).unwrap();
        let tree = build_a11y_tree(&root, &A11yParams::default(), 2.0);
        assert!(!tree.truncated, "depth 3 足够容纳全树");
        // 顶层 = Application 直接子（Window），同 android 取 <hierarchy> 直接子。
        assert_eq!(tree.nodes.len(), 1, "顶层 = Application 直接子（Window）");
        let window = &tree.nodes[0];
        assert_eq!(window.role, "XCUIElementTypeWindow", "type→role");
        // 零面积 ghost + visible=false hidden 均被过滤，仅 login/search 存活。
        assert_eq!(
            window.children.len(),
            2,
            "零面积 StaticText + visible=false Button 均被过滤，仅 login/search 存活"
        );
        // login button：type→role、label→name（label 优先）。
        let login = &window.children[0];
        assert_eq!(login.role, "XCUIElementTypeButton");
        assert_eq!(login.name, "Log In", "label 优先→name");
        // search field：label 空 → 回退 name 属性；value→value（& 实体解码）。
        let search = &window.children[1];
        assert_eq!(search.role, "XCUIElementTypeTextField");
        assert_eq!(search.name, "search_field", "label 空回退 name 属性");
        assert_eq!(
            search.value.as_deref(),
            Some("Search & go"),
            "value→value + 实体解码"
        );
        assert_eq!(search.children.len(), 1, "search 单子 typed");
        assert_eq!(search.children[0].name, "typed");
    }

    /// 判据2：rect 逻辑点 ×device-scale → 原生像素 bounds（risks[1] 方向钉死：该乘不该除）。login button
    /// 逻辑点 x=10,y=20,w=100,h=50 × scale 2.0 → 原生像素 [20,40,200,100]（与 screenshot native_w 同尺度，
    /// harness meta.scale 换算前提）。@3x 同验方向。
    #[test]
    fn a11y_bounds_multiplied_by_device_scale() {
        let root = parse_xcui_xml(XCUI_XML_FIXTURE).unwrap();
        // scale=2.0（@2x 模拟器）：逻辑点 ×2 → 原生像素。
        let tree2 = build_a11y_tree(&root, &A11yParams::default(), 2.0);
        let login2 = &tree2.nodes[0].children[0];
        assert_eq!(
            login2.bounds,
            Some([20, 40, 200, 100]),
            "逻辑点 [10,20,100,50] × 2.0 → 原生像素 [20,40,200,100]"
        );
        // scale=3.0（@3x 真机）：同方向，×3（禁反向除）。
        let tree3 = build_a11y_tree(&root, &A11yParams::default(), 3.0);
        let login3 = &tree3.nodes[0].children[0];
        assert_eq!(
            login3.bounds,
            Some([30, 60, 300, 150]),
            "逻辑点 [10,20,100,50] × 3.0 → 原生像素 [30,60,300,150]"
        );
    }

    /// 判据3：depth/max_nodes 截断 + role 剪枝 + root='focus'（复用 A11yParams 语义，同 android）。
    #[test]
    fn a11y_depth_maxnodes_truncate_and_prune() {
        let root = parse_xcui_xml(XCUI_XML_FIXTURE).unwrap();
        // depth 0：Window 有子 → 截断，Window 不含后代。
        let d0 = build_a11y_tree(
            &root,
            &A11yParams {
                depth: 0,
                ..A11yParams::default()
            },
            2.0,
        );
        assert!(d0.truncated, "depth 0 但 Window 有子 → 截断");
        assert_eq!(d0.nodes.len(), 1);
        assert!(d0.nodes[0].children.is_empty(), "depth 0 不含后代");
        // max_nodes 1：仅 Window 入预算 → 截断。
        let m1 = build_a11y_tree(
            &root,
            &A11yParams {
                max_nodes: 1,
                ..A11yParams::default()
            },
            2.0,
        );
        assert!(m1.truncated, "预算 1 < 存活节点数 → 截断");
        assert_eq!(m1.nodes.len(), 1);
        // role 剪枝：仅保留 TextField 及其祖先路径（Window），非匹配 login Button 旁支剪除。
        let pruned = build_a11y_tree(
            &root,
            &A11yParams {
                role: Some("XCUIElementTypeTextField".to_string()),
                ..A11yParams::default()
            },
            2.0,
        );
        assert_eq!(pruned.nodes.len(), 1, "Window 作 TextField 祖先保留");
        let window = &pruned.nodes[0];
        assert_eq!(window.children.len(), 1, "非匹配的 login Button 旁支剪除");
        assert_eq!(window.children[0].role, "XCUIElementTypeTextField");
        // root='focus'：焦点=search TextField（hasFocus=true），其直接子=typed。
        let focus = build_a11y_tree(
            &root,
            &A11yParams {
                root: Some("focus".to_string()),
                ..A11yParams::default()
            },
            2.0,
        );
        assert_eq!(focus.nodes.len(), 1, "焦点=search，其直接子=typed StaticText");
        assert_eq!(focus.nodes[0].name, "typed");
    }

    /// 判据3（assert 组合）：IosDriver 经 AssertDriver 空组合，assert(text) 命中/未命中双路径 passed
    /// true/false（get_a11y_tree+screenshot 实装后三形态真可断言；text 纯逻辑无 WDA 调用，离线可测）。
    #[tokio::test]
    async fn assert_text_both_paths() {
        let d = IosDriver::new("a11y-udid".to_string(), DEFAULT_WDA_URL.to_string());
        // 命中 → passed=true。
        let hit = d
            .assert(text_params("login succeeded", "succeeded"))
            .await
            .unwrap();
        assert!(hit.passed, "命中子串 → passed=true");
        assert!(hit.matched.is_some());
        // 未命中 → passed=false（失败即数据，Locked-7，仍 Ok）。
        let miss = d
            .assert(text_params("login succeeded", "denied"))
            .await
            .unwrap();
        assert!(!miss.passed, "未命中 → passed=false");
        assert!(miss.matched.is_none());
    }

    /// 构造 text 形态 assert 入参（其余字段占位缺省，text 形态忽略）。
    fn text_params(actual: &str, expect: &str) -> AssertParams {
        AssertParams {
            mode: AssertMode::Text,
            expect: expect.to_string(),
            match_type: MatchType::Contains,
            present: true,
            actual: Some(actual.to_string()),
            field: A11yField::Name,
            query: A11yParams::default(),
            reference_image_base64: None,
            region: None,
            threshold: 0.05,
            method: DiffMethod::Pixel,
            baseline_key: None,
            save_baseline: None,
        }
    }
}

// RecordDriver 无默认方法（record.rs），DeviceDriver bound 含之——必须显式桩，漏任一即编译失败
// （M5 学费）。iOS record 明确剔除（§3.5）：WDA 无原生录屏，defer（非「未实装」而是「不支持」）。
#[async_trait]
impl RecordDriver for IosDriver {
    async fn start_recording(
        &self,
        _rec_id: String,
        _params: RecordParams,
        _output_path: PathBuf,
    ) -> Result<(), CapError> {
        Err(CapError::Unsupported(RECORD_UNSUPPORTED.to_string()))
    }

    async fn stop_recording(&self, _rec_id: String) -> Result<RecordArtifact, CapError> {
        Err(CapError::Unsupported(RECORD_UNSUPPORTED.to_string()))
    }
}

/// audio 域（M11）：矩阵 Unsupported——call-time 恒 E_UNSUPPORTED（广告面 supports_tool 同步剔除，
/// dispatch 超集网不动），与其余剔除项同语义分层。
#[async_trait]
impl AudioDriver for IosDriver {
    async fn audio_inject(
        &self,
        _params: AudioInjectParams,
    ) -> Result<AudioInjectResult, CapError> {
        Err(CapError::Unsupported(AUDIO_UNSUPPORTED.to_string()))
    }
}

/// 聚合 DeviceDriver：七子 trait 全实现后并入；`platform()` 覆写为 `"ios"`（默认 `"desktop"`），
/// 传输层 Register 帧据此派生 platform（node 宿主 = mac ≠ 被控 iOS 设备平台，故须由 driver 派生）。
impl DeviceDriver for IosDriver {
    fn platform(&self) -> &'static str {
        "ios"
    }

    /// iOS 广告面剔除 9 个无 iOS 语义工具（§3.5 + M11，12/21）：`move_mouse`（触摸无 hover 指针）+
    /// `list_processes` / `kill_process`（沙箱无进程枚举 / pid 通路）+ `file_push` / `file_pull`（沙箱
    /// 无文件通道）+ `run_command`（无设备 shell）+ `start_recording` / `stop_recording`（WDA 无原生
    /// 录屏，两后端不一致 defer）+ `audio_inject`（M11：无宿主侧音频注入通路）。其余 12 进广告面
    /// （screenshot/zoom/list_displays/switch_display/click/type/key/scroll/drag/wait/get_a11y_tree/
    /// assert）。被剔 9 工具在 dispatch 超集网仍恒可派发，call-time 返 `E_UNSUPPORTED`（driver 各方法
    /// 早返回 Unsupported，tool_dispatch 全臂不动）；本谓词仅收窄 Register.tools / list_tools 广告面
    /// （三面同源，M6 能力协商 fleet 面 TOOLS 12/21 直接受益）。
    fn supports_tool(&self, tool: &str) -> bool {
        !matches!(
            tool,
            "move_mouse"
                | "list_processes"
                | "kill_process"
                | "file_push"
                | "file_pull"
                | "run_command"
                | "start_recording"
                | "stop_recording"
                | "audio_inject"
        )
    }
}

// ===== TASK-004：21 工具 triage 契约矩阵（iOS 子集 12/21：4 direct + 8 remap / 9 unsupported）=====

/// 21 工具 triage 契约矩阵（iOS 版，§3.5 + M11 audio）：按 driver 契约分类逐工具定位广告面归属，骨架期以四面守卫锁定
/// SC-4 子集 12/21——集合相等（矩阵 == TOOL_NAMES）+ 分区计数（12 supported / 9 unsupported）+ 9 剔除项
/// call-time `E_UNSUPPORTED` + 12 支持项 `supports_tool`==true 且子集大小 ==12。12 支持工具的真实行为断言
/// （direct/remap 的 WDA argv / 坐标 scale 校正）待 TASK-006-008 各族实装后由 TASK-011 补全（本骨架只做
/// 静态子集与剔除语义守卫，不做行为断言）。
///
///   - direct（4）：zoom / list_displays / wait / assert —— iOS 原生等价或平台无关（截图后处理 / 单显示
///     语义 / 本地 sleep / AssertDriver 零新代码组合，§3.5）。
///   - remap（8）：screenshot / switch_display / click / type / key / scroll / drag / get_a11y_tree ——
///     WDA HTTP 契约映射（/screenshot、/actions W3C tap、/wda/keys、/source 等，坐标经 device-scale 校正）。
///   - unsupported（9）：move_mouse（无指针概念）/ list_processes / kill_process（沙箱无进程枚举 / pid
///     通路）/ file_push / file_pull（沙箱无文件通道）/ run_command（无 shell）/ start_recording /
///     stop_recording（WDA 无原生录屏 defer）/ audio_inject（M11：无宿主侧音频注入通路）—— driver 层
///     各方法 call-time 返 `E_UNSUPPORTED`（永久剔除）。
///
/// supported（direct + remap）12 = 广告面（`supports_tool`==true）；unsupported 9 = 广告面剔除
/// （`supports_tool`==false，但仍在 TOOL_NAMES 超集，call-time `E_UNSUPPORTED`，dispatch 网不动）。集合
/// 相等：矩阵 21 工具名与 `aura_capability::TOOL_NAMES` canonical 集合相等（GAP-7 单源下沉：真源 =
/// aura-node `tool_registry!` 注册表，canonical 经 aura-node 侧 `canonical_tool_names_in_capability_
/// equal_registry` 断言与之钉死，本侧编译期直引 canonical，旧 cfg(test) 字面镜像已删）；MCP/广告面一致性
/// 另由 `mcp_router_names_equal_tool_names_registry` 与 `advertised_subset_ios_is_12` 守卫（三面同源）。
#[cfg(test)]
mod contract_matrix {
    use super::*;
    use aura_capability::TOOL_NAMES;
    use std::collections::HashSet;

    /// 工具 triage 分类（iOS driver 契约三分）。
    #[derive(Debug, Clone, Copy, PartialEq, Eq)]
    enum Triage {
        /// 语义等价：iOS 原生等价或平台无关，行为与桌面契约一致。
        Direct,
        /// 映射：桌面语义 remap 为 WDA HTTP 契约（端点 / W3C actions / 坐标 scale 校正）。
        Remap,
        /// 不支持：iOS 沙箱 / 触摸模型永久无等价（clean）或 record 域 defer——call-time 返 `E_UNSUPPORTED`。
        Unsupported,
    }

    /// 21 工具 triage 表：对外工具名（顺序对照 `aura_capability::TOOL_NAMES` canonical）→ iOS 分类（§3.5）。
    const IOS_TRIAGE_MATRIX: &[(&str, Triage)] = &[
        ("screenshot", Triage::Remap),
        ("zoom", Triage::Direct),
        ("list_displays", Triage::Direct),
        ("switch_display", Triage::Remap),
        ("click", Triage::Remap),
        ("type", Triage::Remap),
        ("key", Triage::Remap),
        ("scroll", Triage::Remap),
        ("drag", Triage::Remap),
        ("move_mouse", Triage::Unsupported),
        ("wait", Triage::Direct),
        ("list_processes", Triage::Unsupported),
        ("kill_process", Triage::Unsupported),
        ("file_push", Triage::Unsupported),
        ("file_pull", Triage::Unsupported),
        ("run_command", Triage::Unsupported),
        ("get_a11y_tree", Triage::Remap),
        ("assert", Triage::Direct),
        ("start_recording", Triage::Unsupported),
        ("stop_recording", Triage::Unsupported),
        ("audio_inject", Triage::Unsupported),
    ];

    /// 构造被测 driver（udid / wda_url 任意；矩阵不发起真 WDA 调用——剔除项走早返回 Unsupported、
    /// supports_tool 纯谓词，均不触网）。
    fn drv() -> IosDriver {
        IosDriver::new("matrix-udid".to_string(), DEFAULT_WDA_URL.to_string())
    }

    // ---------- 矩阵完整性守卫：集合相等 + triage 分区计数 ----------

    /// 集合相等：矩阵 21 工具名与 canonical `aura_capability::TOOL_NAMES` 集合相等，不缺不多、无重复
    /// （GAP-7：canonical 与 tool_registry! 真源由 aura-node 侧断言钉死，此处漂移即红）。
    #[test]
    fn matrix_covers_exactly_tool_names_set_equal() {
        let matrix: HashSet<&str> = IOS_TRIAGE_MATRIX.iter().map(|(n, _)| *n).collect();
        let tool_names: HashSet<&str> = TOOL_NAMES.iter().copied().collect();
        assert_eq!(IOS_TRIAGE_MATRIX.len(), 21, "矩阵 21 条目");
        assert_eq!(matrix.len(), 21, "21 工具名无重复");
        assert_eq!(matrix, tool_names, "矩阵工具集必须与 TOOL_NAMES 集合相等（不缺不多）");
    }

    /// triage 分区计数：4 direct / 8 remap / 9 unsupported，supported（direct+remap）= 广告面 12，剔除 9。
    #[test]
    fn triage_partition_counts_12_supported_9_unsupported() {
        let n = |t: Triage| IOS_TRIAGE_MATRIX.iter().filter(|(_, x)| *x == t).count();
        let (direct, remap, unsupported) =
            (n(Triage::Direct), n(Triage::Remap), n(Triage::Unsupported));
        assert_eq!(
            (direct, remap, unsupported),
            (4, 8, 9),
            "4 direct / 8 remap / 9 unsupported（§3.5 + M11 audio_inject）"
        );
        assert_eq!(direct + remap, 12, "supported（direct + remap）= 广告面 12");
        assert_eq!(unsupported, 9, "剔除 9");
        assert_eq!(direct + remap + unsupported, 21);
    }

    // ---------- 9 剔除项 call-time E_UNSUPPORTED（SC-4，骨架期即可全测）----------

    /// 9 剔除工具 driver 层 call-time 返 `E_UNSUPPORTED`（永久剔除，非 not_yet 未实装）：move_mouse +
    /// 进程 2 + 文件 2 + run_command + record 2 + audio_inject（M11），覆盖矩阵 Unsupported 分区全集。
    /// 骨架期即全绿——本任务已将 6 个非 record 剔除方法从 not_yet(E_INTERNAL) 收敛为
    /// Unsupported(E_UNSUPPORTED)（record 2 骨架期已是 Unsupported）。区别于 12 支持项中 11 个
    /// （assert 除外）仍返 E_INTERNAL 待 TASK-005+ 实装。
    #[tokio::test]
    async fn unsupported_tools_call_time_return_e_unsupported() {
        let d = drv();
        // move_mouse（触摸无 hover 指针）
        assert_eq!(
            d.move_mouse(Coordinate { x: 1, y: 2 }).await.unwrap_err().code(),
            "E_UNSUPPORTED"
        );
        // 进程域：list_processes / kill_process（沙箱无进程枚举 / pid 通路）
        assert_eq!(d.list_processes().await.unwrap_err().code(), "E_UNSUPPORTED");
        assert_eq!(d.kill_process(1234).await.unwrap_err().code(), "E_UNSUPPORTED");
        // 文件域：file_push / file_pull（沙箱无文件通道）
        assert_eq!(
            d.file_push("/tmp/x".to_string(), "Zm9v".to_string()).await.unwrap_err().code(),
            "E_UNSUPPORTED"
        );
        assert_eq!(
            d.file_pull("/tmp/x".to_string()).await.unwrap_err().code(),
            "E_UNSUPPORTED"
        );
        // run_command（无设备 shell）
        assert_eq!(
            d.run_command("id".to_string(), vec![], None, None).await.unwrap_err().code(),
            "E_UNSUPPORTED"
        );
        // record 域：start_recording / stop_recording（WDA 无原生录屏 defer）
        assert_eq!(
            d.start_recording(
                "rec-1".to_string(),
                RecordParams { fps: None, max_duration_secs: None, display: None },
                PathBuf::from("/tmp/rec.mp4"),
            )
            .await
            .unwrap_err()
            .code(),
            "E_UNSUPPORTED"
        );
        assert_eq!(
            d.stop_recording("rec-1".to_string()).await.unwrap_err().code(),
            "E_UNSUPPORTED"
        );
        // audio_inject（M11：无宿主侧音频注入通路）
        assert_eq!(
            d.audio_inject(AudioInjectParams { wav_base64: Some("QQ==".to_string()), file: None })
                .await
                .unwrap_err()
                .code(),
            "E_UNSUPPORTED"
        );
    }

    // ---------- 广告面子集：12 支持 supports_tool==true / 9 剔除 false（且仍在超集）----------

    /// 广告面子集精确 12：TOOL_NAMES 按 `supports_tool` 过滤 == 12；12 支持项逐一 true；9 剔除项逐一 false
    /// 但仍在 TOOL_NAMES 超集（call-time `E_UNSUPPORTED`，非从注册表删除，dispatch 网不动）；补集精确 9。
    /// 与 aura-node `advertised_subset_ios_is_12` 双测（platform driver 自证 + node 三面同源）。
    #[test]
    fn advertised_subset_is_exactly_12() {
        let d = drv();
        let names: HashSet<&str> = TOOL_NAMES.iter().copied().collect();

        // 广告面 = TOOL_NAMES（canonical）按 supports_tool 过滤，精确 12 且 ⊆ TOOL_NAMES。
        let advertised: Vec<&str> = TOOL_NAMES
            .iter()
            .copied()
            .filter(|&t| d.supports_tool(t))
            .collect();
        assert_eq!(advertised.len(), 12, "iOS 广告面 = 21 − 9 = 12");
        assert!(
            advertised.iter().all(|t| names.contains(t)),
            "广告面必须 ⊆ TOOL_NAMES"
        );

        // 12 支持项（矩阵 direct + remap）逐一 supports_tool==true。
        for tool in IOS_TRIAGE_MATRIX
            .iter()
            .filter(|(_, x)| *x != Triage::Unsupported)
            .map(|(n, _)| *n)
        {
            assert!(d.supports_tool(tool), "{tool} 属支持项，须在广告面");
        }
        // 9 剔除项（矩阵 unsupported）逐一 supports_tool==false，但仍在 TOOL_NAMES 超集。
        for tool in IOS_TRIAGE_MATRIX
            .iter()
            .filter(|(_, x)| *x == Triage::Unsupported)
            .map(|(n, _)| *n)
        {
            assert!(!d.supports_tool(tool), "{tool} 属剔除项，须被广告面剔除");
            assert!(
                names.contains(tool),
                "{tool} 仍在 TOOL_NAMES 超集（call-time E_UNSUPPORTED，非删除）"
            );
        }
        // 补集精确 9（广告面 + 剔除集 = 全集 21，无误剔）。
        assert_eq!(TOOL_NAMES.len() - advertised.len(), 9, "剔除补集精确 9");
    }
}

// ===== TASK-005：WdaClient 纯函数 + session 状态机 L2 离线单测（无需 live WDA）=====
// 独立测试模块（与 TASK-004 `contract_matrix` 并存，避免并行编辑同一 mod 冲突）。纯函数覆盖 URL 构建 /
// sessionless 分类 / invalid-session 判定 / 超时选择 / JSON 信封解析 / 2xx 映射；L2 分级用连接拒绝
// （127.0.0.1:59999 无监听 → loopback 即时 RST）离线实证 传输失败 → E_UNAVAILABLE，无需拉起 WDA。
#[cfg(test)]
mod wda_tests {
    use super::wda::*;
    use super::WdaClient;
    use aura_capability::CapError;
    use std::time::Duration;

    // ---- wda_url：sessionless 直挂 / 有状态置于 /session/{sid} 下 ----

    #[test]
    fn wda_url_builds_with_and_without_sid() {
        // 无 sid（sessionless）：直挂 base。
        assert_eq!(
            wda_url("http://127.0.0.1:8100", "/status", None),
            "http://127.0.0.1:8100/status"
        );
        // 有 sid（有状态）：置于 /session/{sid} 下。
        assert_eq!(
            wda_url("http://127.0.0.1:8100", "/window/size", Some("ABC123")),
            "http://127.0.0.1:8100/session/ABC123/window/size"
        );
        // base 末尾 / 归一，避免 //。
        assert_eq!(
            wda_url("http://127.0.0.1:8100/", "/actions", Some("S")),
            "http://127.0.0.1:8100/session/S/actions"
        );
        // 含 query 的 path 原样拼接（/source?format=xml）。
        assert_eq!(
            wda_url("http://h:8100", "/source?format=xml", None),
            "http://h:8100/source?format=xml"
        );
    }

    // ---- is_sessionless：§3.3 清单为真，有状态端点为假 ----

    #[test]
    fn wda_client_sessionless_classification() {
        // sessionless 清单（读路径 + 锁屏态）为真。
        assert!(is_sessionless("/status"));
        assert!(is_sessionless("/screenshot"));
        assert!(is_sessionless("/source"));
        assert!(is_sessionless("/source?format=xml"), "剥 query 后比对");
        assert!(is_sessionless("/wda/lock"));
        assert!(is_sessionless("/wda/unlock"));
        assert!(is_sessionless("/wda/locked"));
        // 有状态端点为假（须惰性 session）。
        assert!(!is_sessionless("/actions"));
        assert!(!is_sessionless("/window/size"));
        assert!(!is_sessionless("/wda/keys"));
        assert!(!is_sessionless("/wda/pressButton"));
        assert!(!is_sessionless("/session"));
    }

    // ---- is_invalid_session：404 或 body 含 invalid session id（§3.3 L1）----

    #[test]
    fn wda_client_invalid_session_detection() {
        // HTTP 404 → 失效（无论 body）。
        assert!(is_invalid_session(404, ""));
        // body 含 invalid session id（大小写不敏感）→ 失效。
        assert!(is_invalid_session(
            200,
            r#"{"value":{"error":"invalid session id","message":"Session ... does not exist"}}"#
        ));
        assert!(is_invalid_session(500, "Invalid Session ID"), "大小写不敏感");
        // 正常 2xx 且无失效标记 → 有效。
        assert!(!is_invalid_session(200, r#"{"value":{"width":390,"height":844}}"#));
        assert!(!is_invalid_session(400, "some other error"));
    }

    // ---- read_timeout_for：/source 45s，其余 15s（§3.3 hung 熔断）----

    #[test]
    fn wda_client_read_timeout_source_45s() {
        assert_eq!(read_timeout_for("/source"), Duration::from_secs(45));
        assert_eq!(
            read_timeout_for("/source?format=xml"),
            Duration::from_secs(45),
            "剥 query"
        );
        assert_eq!(read_timeout_for("/status"), Duration::from_secs(15));
        assert_eq!(read_timeout_for("/screenshot"), Duration::from_secs(15));
        assert_eq!(read_timeout_for("/actions"), Duration::from_secs(15));
        assert_eq!(read_timeout_for("/window/size"), Duration::from_secs(15));
    }

    // ---- WDA 响应信封 value 解析（截图 base64 / source XML 共用）----

    #[test]
    fn parse_wda_value_str_unwraps_envelope() {
        // 截图：value 为 base64 串。
        assert_eq!(
            parse_wda_value_str(r#"{"value":"iVBORw0KGgo=","sessionId":"S"}"#).as_deref(),
            Some("iVBORw0KGgo=")
        );
        // source：value 为 XML 串。
        assert_eq!(
            parse_wda_value_str(r#"{"value":"<XCUIElementTypeApplication/>","sessionId":null}"#)
                .as_deref(),
            Some("<XCUIElementTypeApplication/>")
        );
        // 缺 value / value 非串 / 非 JSON → None。
        assert_eq!(parse_wda_value_str(r#"{"sessionId":"S"}"#), None);
        assert_eq!(parse_wda_value_str(r#"{"value":{"width":1}}"#), None);
        assert_eq!(parse_wda_value_str("not json"), None);
    }

    // ---- session id 解析：JSONWP 顶层 / W3C value.sessionId 两形 ----

    #[test]
    fn parse_session_id_covers_jsonwp_and_w3c() {
        // JSONWP：顶层 sessionId。
        assert_eq!(
            parse_session_id(r#"{"sessionId":"TOP-123","value":{}}"#).as_deref(),
            Some("TOP-123")
        );
        // W3C：value.sessionId（顶层无）。
        assert_eq!(
            parse_session_id(r#"{"value":{"sessionId":"NESTED-456","capabilities":{}}}"#).as_deref(),
            Some("NESTED-456")
        );
        // 顶层优先。
        assert_eq!(
            parse_session_id(r#"{"sessionId":"TOP","value":{"sessionId":"NESTED"}}"#).as_deref(),
            Some("TOP")
        );
        // 两处皆无 → None。
        assert_eq!(parse_session_id(r#"{"value":{"capabilities":{}}}"#), None);
    }

    // ---- window/size 解析：value.{width,height}（整数→f64 供 scale 除法）----

    #[test]
    fn parse_window_size_extracts_wh() {
        assert_eq!(
            parse_window_size(r#"{"value":{"width":390,"height":844},"sessionId":"S"}"#),
            Some((390.0, 844.0))
        );
        // 缺字段 / value 非对象 / 非 JSON → None。
        assert_eq!(parse_window_size(r#"{"value":{"width":390}}"#), None);
        assert_eq!(parse_window_size(r#"{"value":42}"#), None);
        assert_eq!(parse_window_size("nope"), None);
    }

    // ---- require_2xx：2xx 透传 body，非 2xx 经 ctor 构语义错误码 ----

    #[test]
    fn require_2xx_passes_and_maps_error() {
        // 2xx → Ok(body) 透传。
        assert_eq!(
            require_2xx(200, "ok-body".to_string(), "/x", CapError::CaptureFailed).unwrap(),
            "ok-body"
        );
        // 非 2xx → ctor 决定语义：CaptureFailed → E_CAPTURE_FAILED。
        let err = require_2xx(500, "boom".to_string(), "/x", CapError::CaptureFailed).unwrap_err();
        assert_eq!(err.code(), "E_CAPTURE_FAILED");
        // InputFailed → E_INPUT_FAILED（有状态写路径）。
        let err2 =
            require_2xx(400, "bad".to_string(), "/wda/keys", CapError::InputFailed).unwrap_err();
        assert_eq!(err2.code(), "E_INPUT_FAILED");
    }

    // ---- L2 分级：连接不通（无 live WDA）→ E_UNAVAILABLE（连接拒绝即证，无需拉起 WDA）----
    // 127.0.0.1:59999 无监听 → loopback 即时 RST → 传输失败 → L2 探 /status 亦拒 → E_UNAVAILABLE。

    #[test]
    fn get_status_unreachable_maps_to_unavailable() {
        let wda = WdaClient::new("http://127.0.0.1:59999".to_string());
        let err = wda.get_status().unwrap_err();
        assert_eq!(err.code(), "E_UNAVAILABLE", "/status 本身即探针，不通直判不可达");
    }

    #[test]
    fn stateful_call_unreachable_maps_to_unavailable() {
        // 有状态端点：ensure_session 建 session 传输失败 → map_transport_failure 探 /status 亦拒 → E_UNAVAILABLE。
        let wda = WdaClient::new("http://127.0.0.1:59999".to_string());
        let err = wda.press_button("home").unwrap_err();
        assert_eq!(err.code(), "E_UNAVAILABLE");
    }
}

// ===== TASK-006：screenshot PNG 管线 + device-scale 自推导离线单测（无需 live WDA）=====
// 独立测试模块（与 contract_matrix/wda_tests 并存，避免并行编辑同一 mod 冲突）。PNG 管线经 png crate
// 合成→解码闭环离线自证（RGBA 逐字节还原 + RGB 补 alpha）；device-scale 推导为纯函数（native_w÷logical_w）
// 离线算得（828/414=2.0、1170/390=3.0），无网络依赖；zoom 数据路径经合成帧裁剪离线自证。
#[cfg(test)]
mod screen_tests {
    use super::wda::derive_device_scale;
    use super::{decode_png_rgba, SCREENSHOT_MAX_DIM};
    use base64::engine::general_purpose::STANDARD;
    use base64::Engine as _;

    /// 合成 RGBA8 渐变缓冲（w*h*4），像素 = [x%256, y%256, 128, 255]（同 screen.rs synth_rgba 语义）。
    fn synth_rgba(w: u32, h: u32) -> Vec<u8> {
        let mut v = Vec::with_capacity((w as usize) * (h as usize) * 4);
        for y in 0..h {
            for x in 0..w {
                v.extend_from_slice(&[(x % 256) as u8, (y % 256) as u8, 128, 255]);
            }
        }
        v
    }

    /// 用 png crate 把裸像素编码为 PNG 字节（模拟 WDA /screenshot 出的 PNG）。
    fn encode_png(pixels: &[u8], w: u32, h: u32, color: png::ColorType) -> Vec<u8> {
        let mut out = Vec::new();
        {
            let mut enc = png::Encoder::new(&mut out, w, h);
            enc.set_color(color);
            enc.set_depth(png::BitDepth::Eight);
            let mut writer = enc.write_header().unwrap();
            writer.write_image_data(pixels).unwrap();
        }
        out
    }

    #[test]
    fn screenshot_png_decode_to_encode_scaled_pipeline() {
        // 合成 RGBA → PNG 编码 → decode_png_rgba 还原 RGBA8+dims → encode_scaled 产 WebP（RIFF/WEBP 头）。
        let (w, h) = (120u32, 80u32);
        let rgba = synth_rgba(w, h);
        let png_bytes = encode_png(&rgba, w, h, png::ColorType::Rgba);
        let (decoded, dw, dh) = decode_png_rgba(&png_bytes).unwrap();
        assert_eq!((dw, dh), (w, h), "解码还原原生尺寸");
        assert_eq!(decoded.len(), (w * h * 4) as usize, "RGBA8：4 字节/像素");
        assert_eq!(decoded, rgba, "RGBA 像素逐字节还原");
        // 复用平台无关 encode_scaled 流水线出 WebP（base64 解码含 RIFF....WEBP 容器头）。
        let r = crate::screen::encode_scaled(decoded, dw, dh, SCREENSHOT_MAX_DIM, crate::screen::WEBP_QUALITY)
            .unwrap();
        assert_eq!(r.mime, "image/webp");
        let bytes = STANDARD.decode(&r.image_base64).unwrap();
        assert_eq!(&bytes[0..4], b"RIFF");
        assert_eq!(&bytes[8..12], b"WEBP");
    }

    #[test]
    fn decode_png_rgb_fills_opaque_alpha() {
        // RGB PNG（无 alpha 通道）→ decode 统一补 alpha=255 到 RGBA8（risks[0]：非 RGBA8 源亦归一）。
        let (w, h) = (4u32, 3u32);
        let mut rgb = Vec::new();
        for i in 0..(w * h) {
            rgb.extend_from_slice(&[i as u8, 10, 20]);
        }
        let png_bytes = encode_png(&rgb, w, h, png::ColorType::Rgb);
        let (decoded, dw, dh) = decode_png_rgba(&png_bytes).unwrap();
        assert_eq!((dw, dh), (w, h));
        assert_eq!(decoded.len(), (w * h * 4) as usize, "RGB 补 alpha 后为 RGBA8");
        assert_eq!(&decoded[0..4], &[0, 10, 20, 255], "首像素 RGB 保留、alpha 补 255");
        assert!(decoded.chunks_exact(4).all(|p| p[3] == 255), "全部 alpha == 255");
    }

    #[test]
    fn device_scale_derived_2x_and_3x() {
        // 运行时自推导（native_w ÷ logical_w），机型无关、非硬编码：
        // @2x 模拟器 iPhone11：828 native ÷ 414 logical = 2.0。
        let s2 = derive_device_scale(828, 414.0).unwrap();
        assert!((s2 - 2.0).abs() < 1e-9, "@2x scale = {s2}");
        // @3x 真机：1170 ÷ 390 = 3.0。
        let s3 = derive_device_scale(1170, 390.0).unwrap();
        assert!((s3 - 3.0).abs() < 1e-9, "@3x scale = {s3}");
        // 逻辑宽异常（≤0）→ None（不产 inf/NaN scale 污染坐标）。
        assert_eq!(derive_device_scale(828, 0.0), None);
        assert_eq!(derive_device_scale(828, -1.0), None);
    }

    #[test]
    fn zoom_crops_native_png_frame() {
        // zoom 数据路径（无网络）：合成 RGBA → PNG → decode 原生帧 → crop_rgba 裁剪 → encode_scaled
        // 不降采样（u32::MAX）。验证裁剪落在原生 PNG 帧上（同 android 原生帧裁剪语义）。
        let (w, h) = (10u32, 8u32);
        let png_bytes = encode_png(&synth_rgba(w, h), w, h, png::ColorType::Rgba);
        let (full, iw, ih) = decode_png_rgba(&png_bytes).unwrap();
        assert_eq!((iw, ih), (w, h));
        // 裁 (2,1) 起 3x2 区域；synth 像素 = [x,y,128,255]，故左上 = (2,1)。
        let cropped = crate::screen::crop_rgba(&full, iw, ih, 2, 1, 3, 2).unwrap();
        assert_eq!(cropped.len(), (3 * 2 * 4) as usize);
        assert_eq!(&cropped[0..4], &[2, 1, 128, 255]);
        let r = crate::screen::encode_scaled(cropped, 3, 2, u32::MAX, crate::screen::WEBP_QUALITY).unwrap();
        assert_eq!((r.meta.native_w, r.meta.native_h), (3, 2));
        assert!((r.meta.scale - 1.0).abs() < 1e-9, "zoom 不降采样 scale=1.0");
    }
}

// ===== TASK-007：input 域坐标校正 + W3C /actions 手势构建器 + call-time 限制离线单测（无需 live WDA）=====
// 独立测试模块（与 contract_matrix/wda_tests/screen_tests 并存，避免并行编辑同一 mod 冲突——TASK-008
// a11y 另起模块）。坐标除法 / actions body 形状为纯函数离线算得（钉死逻辑点值）；早返回（Middle / 修饰
// 组合 / 空 type / move_mouse）在 WDA 调用前分流，无网络依赖；type 无焦点错误分类经合成 WDA 错误文本自证。
#[cfg(test)]
mod input_tests {
    use super::*;

    /// 构造被测 driver（udid / wda_url 任意；早返回路径不触网）。
    fn drv() -> IosDriver {
        IosDriver::new("input-udid".to_string(), DEFAULT_WDA_URL.to_string())
    }

    // ---------- 坐标校正：native ÷ device-scale = WDA 逻辑点（§3.2 devil #1）----------

    #[test]
    fn to_logical_divides_by_device_scale() {
        // criteria：828÷2=414、1170÷3=390（机型无关，禁硬编码）。
        assert_eq!(to_logical(828, 2.0), 414, "@2x 屏宽");
        assert_eq!(to_logical(1170, 3.0), 390, "@3x 屏宽");
        // 中心点钉死值：native (414,896) @2x → 逻辑 (207,448)。
        assert_eq!(to_logical(414, 2.0), 207);
        assert_eq!(to_logical(896, 2.0), 448);
        // 四舍五入到整点（415÷2=207.5→208）。
        assert_eq!(to_logical(415, 2.0), 208);
        // scale ≤0 兜底原样返回（纯函数防御，不产 inf/NaN）。
        assert_eq!(to_logical(414, 0.0), 414);
        assert_eq!(to_logical(414, -2.0), 414);
    }

    // ---------- W3C /actions body 形状：pointer 链含逻辑点坐标，非 /wda/tap（规避 #798）----------

    #[test]
    fn w3c_actions_body_shapes() {
        // tap：move(207,448,0) → down → up（native 中心 414,896 @2x → 逻辑 207,448）。
        let tap = tap_actions(to_logical(414, 2.0), to_logical(896, 2.0));
        let v: serde_json::Value = serde_json::from_str(&tap).unwrap();
        let ptr = &v["actions"][0];
        assert_eq!(ptr["type"], "pointer");
        assert_eq!(ptr["parameters"]["pointerType"], "touch");
        let steps = ptr["actions"].as_array().unwrap();
        assert_eq!(steps.len(), 3, "tap = move+down+up");
        assert_eq!(steps[0]["type"], "pointerMove");
        assert_eq!(steps[0]["x"], 207, "逻辑点 x 已除 scale");
        assert_eq!(steps[0]["y"], 448, "逻辑点 y 已除 scale");
        assert_eq!(steps[1]["type"], "pointerDown");
        assert_eq!(steps[2]["type"], "pointerUp");
        // body 不含 /wda/tap 端点痕迹（手势一律 W3C actions）。
        assert!(!tap.contains("/wda/tap"));

        // long press（Right）：move → down → pause(600) → up。
        let lp = long_press_actions(207, 448, LONG_PRESS_MS);
        let v: serde_json::Value = serde_json::from_str(&lp).unwrap();
        let steps = v["actions"][0]["actions"].as_array().unwrap().clone();
        assert_eq!(steps.len(), 4, "长按 = move+down+pause+up");
        assert_eq!(steps[2]["type"], "pause");
        assert_eq!(steps[2]["duration"], 600);

        // drag（swipe）：move(from) → down → move(to,300) → up。
        // native (100,200)→(300,400) @2x → 逻辑 (50,100)→(150,200)。
        let dg = swipe_actions(
            to_logical(100, 2.0),
            to_logical(200, 2.0),
            to_logical(300, 2.0),
            to_logical(400, 2.0),
            DRAG_DURATION_MS,
        );
        let v: serde_json::Value = serde_json::from_str(&dg).unwrap();
        let steps = v["actions"][0]["actions"].as_array().unwrap().clone();
        assert_eq!(steps.len(), 4, "swipe = move+down+move+up");
        assert_eq!(steps[0]["x"], 50, "起点逻辑点 x");
        assert_eq!(steps[0]["y"], 100, "起点逻辑点 y");
        assert_eq!(steps[1]["type"], "pointerDown");
        assert_eq!(steps[2]["type"], "pointerMove");
        assert_eq!(steps[2]["x"], 150, "终点逻辑点 x");
        assert_eq!(steps[2]["y"], 200, "终点逻辑点 y");
        assert_eq!(steps[2]["duration"], DRAG_DURATION_MS);
        assert_eq!(steps[3]["type"], "pointerUp");

        // scroll：Down amount=3 → native 位移 (0,-300)；anchor native (414,896) @2x → 逻辑起 (207,448)、
        // 终 native (414,596) → 逻辑 (207,298)。方向同 android（Down=上滑）。
        let (dx, dy) = scroll_delta(ScrollDirection::Down, 3);
        assert_eq!((dx, dy), (0, -300), "Down 反向量位移（native）");
        let sc = swipe_actions(
            to_logical(414, 2.0),
            to_logical(896, 2.0),
            to_logical(414 + dx, 2.0),
            to_logical(896 + dy, 2.0),
            SCROLL_DURATION_MS,
        );
        let v: serde_json::Value = serde_json::from_str(&sc).unwrap();
        let steps = v["actions"][0]["actions"].as_array().unwrap().clone();
        assert_eq!(steps[0]["x"], 207, "scroll 起点逻辑点 x");
        assert_eq!(steps[0]["y"], 448, "scroll 起点逻辑点 y");
        assert_eq!(steps[2]["x"], 207, "scroll 终点逻辑点 x");
        assert_eq!(steps[2]["y"], 298, "scroll 终点逻辑点 y");
    }

    // ---------- scroll 四向量语义（同 android，反向量）----------

    #[test]
    fn scroll_delta_four_directions() {
        assert_eq!(scroll_delta(ScrollDirection::Down, 3), (0, -300));
        assert_eq!(scroll_delta(ScrollDirection::Up, 3), (0, 300));
        assert_eq!(scroll_delta(ScrollDirection::Left, 3), (300, 0));
        assert_eq!(scroll_delta(ScrollDirection::Right, 3), (-300, 0));
        // amount 缩放距离；amount≤0 兜底 1 步（不产零位移 swipe）。
        assert_eq!(scroll_delta(ScrollDirection::Down, 1), (0, -100));
        assert_eq!(scroll_delta(ScrollDirection::Down, 0), (0, -100));
    }

    // ---------- 硬件键映射：snake_case → WDA camelCase pressButton 名 ----------

    #[test]
    fn press_button_name_maps_hardware_keys() {
        assert_eq!(press_button_name("home"), Some("home"));
        assert_eq!(press_button_name("volume_up"), Some("volumeUp"));
        assert_eq!(press_button_name("volume_down"), Some("volumeDown"));
        // Android 特有软键无 iOS 硬件按钮语义 → None（上层判 Unsupported）。
        assert_eq!(press_button_name("enter"), None);
        assert_eq!(press_button_name("back"), None);
        assert_eq!(press_button_name("tab"), None);
    }

    // ---------- type 无 firstResponder 错误分类（devil #9，非静默丢弃）----------

    #[test]
    fn map_keys_error_enriches_no_focus() {
        // WDA 无键盘焦点报错（FBKeyboard 签名）→ 前缀明确提示且保留原文。
        let wda_err = CapError::InputFailed(
            "WDA /wda/keys returned HTTP 500: Keyboard is not present".to_string(),
        );
        match map_keys_error(wda_err) {
            CapError::InputFailed(msg) => {
                assert!(msg.contains("no firstResponder"), "含明确焦点提示: {msg}");
                assert!(msg.contains("Keyboard is not present"), "保留 WDA 原文: {msg}");
            }
            other => panic!("expected InputFailed, got {}", other.code()),
        }
        // 非焦点类 InputFailed 原样透传（不误加提示）。
        match map_keys_error(CapError::InputFailed(
            "WDA /wda/keys returned HTTP 400: bad value".to_string(),
        )) {
            CapError::InputFailed(msg) => {
                assert!(!msg.contains("no firstResponder"), "非焦点失败不加提示: {msg}")
            }
            other => panic!("expected InputFailed, got {}", other.code()),
        }
        // indicates_no_focus 签名判定。
        assert!(indicates_no_focus("keyboard is not present"));
        assert!(indicates_no_focus("element has no keyboard focus"));
        assert!(indicates_no_focus("no first responder"));
        assert!(!indicates_no_focus("http 400 bad request"));
    }

    // ---------- driver 早返回（call-time 限制，WDA 调用前分流，无 live WDA）----------

    #[tokio::test]
    async fn click_middle_unsupported() {
        let d = drv();
        let err = d
            .click(Coordinate { x: 207, y: 448 }, MouseButton::Middle)
            .await
            .unwrap_err();
        assert_eq!(err.code(), "E_UNSUPPORTED", "中键无 iOS 触摸语义");
    }

    #[tokio::test]
    async fn key_chord_unsupported() {
        let d = drv();
        // 修饰组合 → E_UNSUPPORTED（call-time，WDA 调用前分流）。
        assert_eq!(
            d.key("ctrl+c".to_string()).await.unwrap_err().code(),
            "E_UNSUPPORTED"
        );
        // 未知键（无硬件按钮映射）→ E_UNSUPPORTED。
        assert_eq!(
            d.key("f13".to_string()).await.unwrap_err().code(),
            "E_UNSUPPORTED"
        );
    }

    #[tokio::test]
    async fn empty_type_ok() {
        let d = drv();
        // 空文本短路 Ok（WDA 调用前返回，不触网）。
        assert!(d.type_text(String::new()).await.is_ok());
    }

    #[tokio::test]
    async fn wait_sleeps_and_acks() {
        let d = drv();
        // 平台无关 sleep（0ms 即返回，不触网）。
        assert!(d.wait(0).await.is_ok());
    }
}
