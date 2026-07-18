//! AndroidDriver：Android 移动面设备驱动（M5）。
//!
//! 经系统 `adb` shell out 控制远程 Android 设备（Redroid 容器 / 真机），驱动实现 host-agnostic
//! ——不以 `#[cfg(target_os)]` 门控（node 宿主 OS ≠ 被控设备平台），任意桌面宿主装 adb 即可跑。
//!
//! 本文件 TASK-003 落地 screen（截图 / 区域放大 / 显示枚举）与 input（点击 / 长按 / 滑动 / 键事件 /
//! 文本 / 滚动 / 拖拽 / 等待）两域；TASK-004 补齐 a11y（uiautomator dump 写设备文件再 `adb pull`
//! 避管道截断 → 手写解析 XML → A11yNode 统一形状 + 幽灵过滤 + depth/max_nodes 截断）与 process/file
//! 整域下沉（run_command→`adb shell` 设备语义、file_push/pull→`adb push/pull`、list/kill→`ps`/`kill`）。
//! 六域均经 [`AdbCli`] 窄接口 adapter 隔离系统 adb。
//!
//! 录屏（record 域）已实装：Android 设备侧 `screenrecord` 后台录制 → `pkill -INT` finalize →
//! `adb pull` 大产物桥接到**宿主**（node-local）`<data_dir>/artifacts`（data_dir 宿主钉死，见
//! grpc_reverse `ReverseConfig`），与桌面 WGC 进程内落盘语义正交；实现见 `impl RecordDriver`。

use std::collections::HashMap;
use std::path::PathBuf;
use std::process::Stdio;
use std::sync::{Mutex, MutexGuard, OnceLock, PoisonError};
use std::time::Instant;

use async_trait::async_trait;
use base64::engine::general_purpose::STANDARD;
use base64::Engine as _;

use aura_capability::{
    A11yDriver, A11yNode, A11yParams, A11yTree, Ack, AssertDriver, AudioDriver, AudioInjectParams,
    AudioInjectResult, CapError, CmdResult, Coordinate, DeviceDriver, DisplayInfo, FileResult,
    InputDriver, MouseButton, ProcessFileDriver, ProcessInfo, RecordArtifact, RecordDriver,
    RecordParams, Region, ScreenDriver, ScreenshotOpts, ScreenshotResult, ScrollDirection,
};

// ===== adb 调用常量（remap 域共享）=====

/// 单次 adb 调用超时：30s。screencap/input 均短命令，留足首次连接握手余量。
const ADB_TIMEOUT: std::time::Duration = std::time::Duration::from_secs(30);
/// 截图缩放长边上限（与桌面 `screen::MAX_DIM` 一致的 XGA 最佳实践）。
const SCREENSHOT_MAX_DIM: u32 = 1280;
/// 右键 → 同点长按时长（ms）：600ms 足以触发 Android 上下文/长按语义（Locked-6）。
const LONG_PRESS_MS: u64 = 600;
/// 拖拽 swipe 时长（ms）：300ms 触发拖拽而非快速 fling。
const DRAG_DURATION_MS: u64 = 300;
/// 滚动 swipe 时长（ms）。
const SCROLL_DURATION_MS: u64 = 300;
/// 滚动单步基距（px）：距离 = amount × 本值（amount 默认 3 → 300px）。
const SCROLL_STEP_PX: i32 = 100;
/// run_command 默认超时（ms）：30s（入参 timeout_ms 覆盖，D4 设备语义）。与桌面 process_file 一致。
const RUN_COMMAND_DEFAULT_TIMEOUT_MS: u64 = 30_000;
/// file_pull 单文件上限（字节）：10 MiB。复用桌面 process_file.rs:26 `MAX_PULL_BYTES` 语义，
/// 超限拒绝避免大文件经 base64 撑爆 MCP 响应体（大产物走 resource_link 是后续事项）。
const MAX_PULL_BYTES: u64 = 10 * 1024 * 1024;
// uiautomator dump 的设备侧落盘路径不再固定（GAP-2）：改由 `ui_dump_device_path()` 每次生成唯一
// `/sdcard/aura-ui-<pid>-<nanos>-<n>.xml`，避免并发/多 node dump 互踩固定路径；dump→pull 后设备侧
// `rm -f` 清理（见 A11yDriver::get_a11y_tree），避免 /sdcard 残骸累积。

// SEAM: data_dir=宿主（node-local）钉死——设备侧大产物（screenrecord 录屏、file_pull 文件）须经
// `adb pull` → 宿主 <data_dir>/artifacts 桥接走 G-5（录屏 output_path 由传输层注入，落宿主后旁路
// PUT 上传，Locked-7）；不假设 node 与设备共享文件系统。

// ===== AdbCli：系统 adb 窄接口 adapter（第三方依赖隔离）=====

// ADAPTER: system adb via tokio::process —— 第三方依赖（Android Debug Bridge CLI）经此窄接口隔离。
// 业务层（ScreenDriver/InputDriver impl）只调 exec_raw()（二进制）/ run_text()（文本）双面，不直接
// 拼装或解析 adb 命令细节；adb CLI 行为漂移（参数/输出格式变化）的修复收敛于本 adapter 单文件
// （规约：第三方 SDK/API 一律经窄接口 adapter 隔离消费）。零新 Cargo 依赖——adb 为外部运行时二进制。
#[derive(Debug, Clone)]
struct AdbCli {
    /// 目标设备序列号（`adb -s <serial>`）。空串 = 未指定（单设备默认，不加 -s）。
    serial: String,
}

impl AdbCli {
    fn new(serial: String) -> Self {
        AdbCli { serial }
    }

    /// 组装完整 adb argv：`-s <serial>` 前缀（serial 非空时）贯穿每次 invoke（Locked-2）+ 调用方 args。
    fn full_argv<'a>(&'a self, args: &[&'a str]) -> Vec<&'a str> {
        let mut argv = Vec::with_capacity(args.len() + 2);
        if !self.serial.is_empty() {
            argv.push("-s");
            argv.push(self.serial.as_str());
        }
        argv.extend_from_slice(args);
        argv
    }

    /// 起一次 adb 子进程并等待输出（默认 [`ADB_TIMEOUT`]）。复刻 process_file.rs run_command 脚手架。
    async fn spawn_once(&self, args: &[&str]) -> Result<std::process::Output, CapError> {
        self.spawn_with_timeout(args, ADB_TIMEOUT).await
    }

    /// 起一次 adb 子进程并等待输出，超时可定制（run_command 按入参 timeout_ms 覆盖 D4）。
    /// `tokio::process::Command` + `Stdio::piped` + `kill_on_drop(true)` + `tokio::time::timeout`。
    async fn spawn_with_timeout(
        &self,
        args: &[&str],
        timeout: std::time::Duration,
    ) -> Result<std::process::Output, CapError> {
        let argv = self.full_argv(args);
        let child = tokio::process::Command::new("adb")
            .args(&argv)
            .stdout(Stdio::piped())
            .stderr(Stdio::piped())
            // 超时时 timeout 会 drop wait future 进而 drop 子进程句柄，kill_on_drop 保证发出结束信号避免孤儿。
            .kill_on_drop(true)
            .spawn()
            .map_err(|e| CapError::ProcessError(format!("spawn adb failed: {e}")))?;
        match tokio::time::timeout(timeout, child.wait_with_output()).await {
            Ok(Ok(out)) => Ok(out),
            Ok(Err(e)) => Err(CapError::ProcessError(format!("adb wait failed: {e}"))),
            Err(_elapsed) => Err(CapError::ProcessError(format!(
                "adb command timed out after {} ms",
                timeout.as_millis()
            ))),
        }
    }

    /// 执行 adb 并做失联自愈（默认超时）。`idempotent` 决定失联重连后是否重发（GAP-1 ②）。
    /// 返回底层 Output，stdout 二进制/文本由 exec_raw/run_text 解释。
    async fn run_output(
        &self,
        args: &[&str],
        idempotent: bool,
    ) -> Result<std::process::Output, CapError> {
        self.run_output_with_timeout(args, ADB_TIMEOUT, idempotent).await
    }

    /// 失联自愈 + 定制超时：检出 stderr 为**连接层**失联即自动 `adb connect <serial>` 重连（Locked-2）。
    /// 重连后是否重发依 `idempotent`（GAP-1 ②）：幂等只读操作重发一次；非幂等操作重连但返回错误、
    /// **不重发**（避免副作用双执行，由调用方/agent 自行重试）。仅解析 stderr（文本），绝不触碰 stdout
    /// （可能是二进制帧）。
    async fn run_output_with_timeout(
        &self,
        args: &[&str],
        timeout: std::time::Duration,
        idempotent: bool,
    ) -> Result<std::process::Output, CapError> {
        let out = self.spawn_with_timeout(args, timeout).await?;
        if out.status.success() {
            return Ok(out);
        }
        // adb 把设备失联写 stderr 且退出码非 0（连接层，收窄匹配见 is_device_disconnected）。
        let stderr = std::str::from_utf8(&out.stderr).unwrap_or("");
        match reconnect_plan(
            !self.serial.is_empty(),
            is_device_disconnected(stderr),
            idempotent,
        ) {
            // 非失联 / 无 serial：原样返回首次结果（由上层按退出码判错）。
            ReconnectPlan::None => Ok(out),
            // 失联 + 幂等：`adb connect <serial>`（host:port 形态，如 Redroid localhost:5555）后重发一次。
            ReconnectPlan::RetryIdempotent => {
                let _ = self.spawn_once(&["connect", &self.serial]).await;
                self.spawn_with_timeout(args, timeout).await
            }
            // 失联 + 非幂等：重连但不重发，返回连接错误（副作用可能已落地，双执行不安全）。
            ReconnectPlan::ReconnectNoRetry => {
                let _ = self.spawn_once(&["connect", &self.serial]).await;
                Err(CapError::ProcessError(format!(
                    "adb device disconnected during non-idempotent op {args:?}; reconnected but not auto-retried: {}",
                    stderr.trim()
                )))
            }
        }
    }

    /// 二进制路径：返回 adb stdout 原始字节（screencap raw RGBA 帧）。**绝不做有损文本解码**
    /// ——二进制帧任何字符集折叠都会破坏像素字节（Locked-2/3）。非 0 退出即 CaptureFailed（仅 stderr 转文本）。
    /// `idempotent`：截图为只读操作恒 true（失联重连后可安全重发）。
    async fn exec_raw(&self, args: &[&str], idempotent: bool) -> Result<Vec<u8>, CapError> {
        let out = self.run_output(args, idempotent).await?;
        if !out.status.success() {
            let stderr = std::str::from_utf8(&out.stderr).unwrap_or("<non-utf8 stderr>");
            return Err(CapError::CaptureFailed(format!(
                "adb {args:?} failed (exit {:?}): {}",
                out.status.code(),
                stderr.trim()
            )));
        }
        Ok(out.stdout)
    }

    /// 文本路径：返回结构化 [`CmdResult`]（退出码 + stdout/stderr 文本）。供 wm size / input / ps 等文本命令用。
    /// `idempotent`（GAP-1 ②）：只读命令（wm size/ps/pull/dump）传 true 可重发；副作用命令
    /// （input/push/kill/rm）传 false，失联重连后不自动重发。
    async fn run_text(&self, args: &[&str], idempotent: bool) -> Result<CmdResult, CapError> {
        let out = self.run_output(args, idempotent).await?;
        let exit_code = out.status.code().unwrap_or(-1);
        Ok(CmdResult {
            exit_code,
            stdout: decode_text(out.stdout),
            stderr: decode_text(out.stderr),
        })
    }

    /// 文本路径 + 定制超时：run_command 按入参 timeout_ms 控制单次 adb shell 时长（D4）。
    /// `idempotent`：run_command 执行任意命令，语义非幂等 → 调用方传 false（失联不自动重发）。
    async fn run_text_timeout(
        &self,
        args: &[&str],
        timeout: std::time::Duration,
        idempotent: bool,
    ) -> Result<CmdResult, CapError> {
        let out = self.run_output_with_timeout(args, timeout, idempotent).await?;
        let exit_code = out.status.code().unwrap_or(-1);
        Ok(CmdResult {
            exit_code,
            stdout: decode_text(out.stdout),
            stderr: decode_text(out.stderr),
        })
    }
}

// ===== adb 输出解析 / argv 组装（自由函数，便纯函数单测，无需真 adb）=====

/// adb 文本路径专用字节转 String（stdout/stderr）。二进制路径 exec_raw 绝不经此，保 screencap
/// 原始帧零破坏（Locked-2）。adb 文本输出为 ASCII/UTF-8，非法字节降级空串（不静默污染像素路径）。
fn decode_text(bytes: Vec<u8>) -> String {
    String::from_utf8(bytes).unwrap_or_default()
}

/// 判定 adb stderr 是否为**连接层**失联态（触发自动重连）。unauthorized 不重连——需人工授权，重连无益。
/// 收窄匹配（GAP-1 ①）：仅锚定 adb 客户端连接层错误格式，**不**匹配命令自身输出里的泛化 "not found"
/// （如设备 sh 的 `<cmd>: not found`），避免把「命令未找到」误判为「设备失联」而触发误重连/双执行。
fn is_device_disconnected(stderr: &str) -> bool {
    let s = stderr.to_ascii_lowercase();
    // adb 设备状态失联：`error: device offline`。
    s.contains("device offline")
        // adb 无设备：`adb: no devices/emulators found`。
        || s.contains("no devices/emulators found")
        // adb connect 失败：`cannot connect to <host>:<port>` / `... Connection refused`。
        || s.contains("cannot connect to")
        || s.contains("connection refused")
        // adb 设备未找到：`error: device '<serial>' not found`——锚定前导单引号 + `' not found`，
        // 区别于 shell 命令自身的 `<cmd>: not found`（无前导单引号，属命令输出非连接层）。
        || (s.contains("device '") && s.contains("' not found"))
}

/// 失联后的重连/重发决策（纯逻辑，便离线单测；GAP-1 ②）。
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum ReconnectPlan {
    /// 未失联，或未指定 serial（无从 `adb connect`）：不重连，原样返回首次结果（由上层按退出码判错）。
    None,
    /// 失联 + 幂等只读操作：重连后安全重发一次（screencap/dump/pull/wm size/ps 等，重发无副作用）。
    RetryIdempotent,
    /// 失联 + 非幂等操作：重连但**不重发**，返回连接错误（input/push/rm/kill/force-stop/run_command——
    /// 副作用可能已在设备落地，自动重发会双执行；由调用方/agent 自行决定是否重试）。
    ReconnectNoRetry,
}

/// 依据 serial 是否指定、stderr 是否连接层失联、操作是否幂等，决定重连后是否重发（GAP-1 ②）。
fn reconnect_plan(serial_present: bool, disconnected: bool, idempotent: bool) -> ReconnectPlan {
    if !serial_present || !disconnected {
        ReconnectPlan::None
    } else if idempotent {
        ReconnectPlan::RetryIdempotent
    } else {
        ReconnectPlan::ReconnectNoRetry
    }
}

/// 解析 `adb exec-out screencap`（无 -p）raw 帧头，**反推** header 尺寸（禁硬编码 12/16B，Locked-3）。
/// 帧布局：`[w:u32 LE][h:u32 LE][format:u32 LE]( [colorspace:u32 LE] Android≥9 )` + RGBA8 像素尾。
/// 反推式：`header_size = total_len − w*h*4`（w/h 读自帧头前 8 字节，像素固定 RGBA8=4B/px）——
/// 自然覆盖 Android<9 的 12B 与 Android≥9 的 16B（含 dataspace）两 case，无版本分支硬编码。
/// 返回 `(w, h, header_size)`；帧过短 / 尺寸溢出 / 与总长不自洽（header<12 或像素超总长）→ CaptureFailed。
fn parse_screencap_header(raw: &[u8]) -> Result<(u32, u32, usize), CapError> {
    if raw.len() < 12 {
        return Err(CapError::CaptureFailed(format!(
            "screencap frame too short: {} bytes (need ≥12B header)",
            raw.len()
        )));
    }
    let w = u32::from_le_bytes([raw[0], raw[1], raw[2], raw[3]]);
    let h = u32::from_le_bytes([raw[4], raw[5], raw[6], raw[7]]);
    let total_len = raw.len();
    let pixel_bytes = (w as usize)
        .checked_mul(h as usize)
        .and_then(|wh| wh.checked_mul(4))
        .ok_or_else(|| CapError::CaptureFailed(format!("screencap dims overflow: {w}x{h}")))?;
    if pixel_bytes == 0 || pixel_bytes > total_len {
        return Err(CapError::CaptureFailed(format!(
            "screencap size mismatch: {w}x{h} needs {pixel_bytes} px bytes but frame is {total_len}"
        )));
    }
    // 反推 header：总长减去像素尾。合法值仅 12（Android<9）或 16（Android≥9），此处只做下界自洽校验。
    let header_size = total_len - pixel_bytes;
    if header_size < 12 {
        return Err(CapError::CaptureFailed(format!(
            "screencap header underflow: derived {header_size} (<12) for {w}x{h} in {total_len}B frame"
        )));
    }
    Ok((w, h, header_size))
}

/// 解析 `adb shell wm size` 输出。典型 `Physical size: 1080x2400`，改分辨率后另含 `Override size: WxH`。
/// Override 优先（反映当前生效分辨率），退化取首个含 `x` 的尺寸行。
fn parse_wm_size(out: &str) -> Result<(u32, u32), CapError> {
    let line = out
        .lines()
        .find(|l| l.contains("Override size"))
        .or_else(|| out.lines().find(|l| l.contains("Physical size")))
        .or_else(|| out.lines().find(|l| l.contains('x')))
        .ok_or_else(|| CapError::CaptureFailed(format!("cannot parse wm size: {out:?}")))?;
    // 取冒号后的 `WxH`（无冒号则整行）。
    let dims = line.rsplit(':').next().unwrap_or(line).trim();
    let (w_str, h_str) = dims
        .split_once('x')
        .ok_or_else(|| CapError::CaptureFailed(format!("no WxH in wm size line: {line:?}")))?;
    let w = w_str
        .trim()
        .parse::<u32>()
        .map_err(|_| CapError::CaptureFailed(format!("bad width in wm size: {line:?}")))?;
    let h = h_str
        .trim()
        .parse::<u32>()
        .map_err(|_| CapError::CaptureFailed(format!("bad height in wm size: {line:?}")))?;
    Ok((w, h))
}

/// 命名键 → Android KEYCODE 数字码。仅收录 M5 契约表列键；未列键返回 None（上层判 Unsupported）。
/// 数字码即 `KEYCODE_*` 常量值：`KEYCODE_ENTER`=66 / `KEYCODE_BACK`=4 / `KEYCODE_HOME`=3 /
/// `KEYCODE_TAB`=61 / `KEYCODE_DEL`=67 / `KEYCODE_MENU`=82 / `KEYCODE_VOLUME_UP`=24。
fn keycode_of(name: &str) -> Option<u32> {
    match name {
        "enter" => Some(66),
        "back" => Some(4),
        "home" => Some(3),
        "tab" => Some(61),
        "del" => Some(67),
        "menu" => Some(82),
        "volume_up" => Some(24),
        _ => None,
    }
}

/// `adb shell input text` 转义：空格 → `%s`（input text 约定），并对经设备 shell 的元字符
/// `( ) < > | ; & * ~ " ' $ ` `（含 `$` 变量/命令替换、反引号命令替换）前置反斜杠，避免被 shell
/// 解释或展开。其余字符原样。GAP-3：`$`/反引号缺转义时 `input text "price=$5"` 在设备 sh 展开面
/// 会把 `$5` 吞成空串（或反引号内容被当子命令执行），须补转义保文本原样落地。
fn escape_input_text(text: &str) -> String {
    let mut out = String::with_capacity(text.len());
    for c in text.chars() {
        match c {
            ' ' => out.push_str("%s"),
            '(' | ')' | '<' | '>' | '|' | ';' | '&' | '*' | '~' | '"' | '\'' | '$' | '`' => {
                out.push('\\');
                out.push(c);
            }
            _ => out.push(c),
        }
    }
    out
}

/// `adb exec-out screencap`：无 `-p` → raw RGBA 帧；`exec-out` 保 stdout 二进制不经行尾转换。
fn screencap_argv() -> Vec<String> {
    vec!["exec-out".into(), "screencap".into()]
}

/// `adb shell input tap <x> <y>`（click Left）。
fn tap_argv(x: i32, y: i32) -> Vec<String> {
    vec![
        "shell".into(),
        "input".into(),
        "tap".into(),
        x.to_string(),
        y.to_string(),
    ]
}

/// `adb shell input swipe <x1> <y1> <x2> <y2> <duration_ms>`（长按 / 滚动 / 拖拽共用）。
fn swipe_argv(x1: i32, y1: i32, x2: i32, y2: i32, duration_ms: u64) -> Vec<String> {
    vec![
        "shell".into(),
        "input".into(),
        "swipe".into(),
        x1.to_string(),
        y1.to_string(),
        x2.to_string(),
        y2.to_string(),
        duration_ms.to_string(),
    ]
}

/// `adb shell input keyevent <keycode>`（命名键经 [`keycode_of`] 映射后的数字码）。
fn keyevent_argv(keycode: u32) -> Vec<String> {
    vec![
        "shell".into(),
        "input".into(),
        "keyevent".into(),
        keycode.to_string(),
    ]
}

/// `adb shell input text <escaped>`（空格 → `%s`，shell 元字符转义，见 [`escape_input_text`]）。
fn text_argv(text: &str) -> Vec<String> {
    vec![
        "shell".into(),
        "input".into(),
        "text".into(),
        escape_input_text(text),
    ]
}

// ===== process/file 域 argv 组装（remap D4，自由函数便纯函数单测）=====

/// `adb shell uiautomator dump <device_path>`：写设备文件（禁管道直读避截断，Locked-8）。
fn uiautomator_dump_argv(device_path: &str) -> Vec<String> {
    vec![
        "shell".into(),
        "uiautomator".into(),
        "dump".into(),
        device_path.into(),
    ]
}

/// `adb shell rm -f <path>`：设备侧删除文件（uiautomator dump 清理，GAP-2；`-f` 避免文件不存在时报错）。
fn rm_argv(path: &str) -> Vec<String> {
    vec!["shell".into(), "rm".into(), "-f".into(), path.into()]
}

/// `adb pull <remote> <local>`：设备文件 → 宿主暂存（a11y dump / file_pull 共用，Locked-6/7）。
fn pull_argv(remote: &str, local: &str) -> Vec<String> {
    vec!["pull".into(), remote.into(), local.into()]
}

/// `adb push <local> <remote>`：宿主暂存 → 设备 /sdcard 路径（file_push，Locked-6）。
fn push_argv(local: &str, remote: &str) -> Vec<String> {
    vec!["push".into(), local.into(), remote.into()]
}

/// `adb shell ps -A`：枚举设备全部进程（list_processes remap）。
fn ps_argv() -> Vec<String> {
    vec!["shell".into(), "ps".into(), "-A".into()]
}

/// `adb shell kill <pid>`：按 pid 结束设备进程（kill_process remap，非 root 常失败 → ProcessError）。
/// 注：按包名结束用 `am force-stop <pkg>`；本 trait 只收 pid，force-stop 为 by-name 变体（M5 未用）。
fn kill_argv(pid: u32) -> Vec<String> {
    vec!["shell".into(), "kill".into(), pid.to_string()]
}

/// run_command 设备语义 argv：`adb shell [cd <cwd> &&] <cmd> <args...>`（D4，被测命令跑在设备而非宿主）。
/// cwd 经 `cd <cwd> &&` 前缀注入；各参数为独立 token（adb 侧空格拼接为设备 sh 命令行，
/// 含空格的设备侧单参数需调用方自行引号——YAGNI，D4）。
fn run_command_argv(cmd: &str, args: &[String], cwd: Option<&str>) -> Vec<String> {
    let mut v = vec!["shell".to_string()];
    if let Some(dir) = cwd.filter(|d| !d.is_empty()) {
        v.push("cd".into());
        v.push(dir.into());
        v.push("&&".into());
    }
    v.push(cmd.to_string());
    v.extend(args.iter().cloned());
    v
}

/// 宿主暂存文件路径（file_push/pull、a11y dump pull 用）。pid + 进程内原子计数保唯一，避免并发碰撞。
/// data_dir 宿主钉死（见文件头 SEAM）：暂存落 temp_dir，非设备侧。
fn host_temp_path(tag: &str) -> PathBuf {
    use std::sync::atomic::{AtomicU64, Ordering};
    static COUNTER: AtomicU64 = AtomicU64::new(0);
    let n = COUNTER.fetch_add(1, Ordering::Relaxed);
    std::env::temp_dir().join(format!("aura-android-{tag}-{}-{n}.tmp", std::process::id()))
}

/// uiautomator dump 的设备侧**唯一**落盘路径：`/sdcard/aura-ui-<pid>-<nanos>-<n>.xml`（GAP-2）。
/// 固定路径 `/sdcard/window_dump.xml` 在多 node/并发 dump 下互踩（一方 pull 到另一方半写文件）；
/// 唯一化 + dump 后设备侧 `rm -f` 清理避免 /sdcard 残骸累积。pid+nanos 跨进程唯一 + 进程内原子计数
/// 防同纳秒并发碰撞（nanos 非单调，同 host_temp_path 规约）。
fn ui_dump_device_path() -> String {
    use std::sync::atomic::{AtomicU64, Ordering};
    static COUNTER: AtomicU64 = AtomicU64::new(0);
    let n = COUNTER.fetch_add(1, Ordering::Relaxed);
    let nanos = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|d| d.as_nanos())
        .unwrap_or(0);
    format!("/sdcard/aura-ui-{}-{}-{}.xml", std::process::id(), nanos, n)
}

/// 解析 `adb shell ps -A` 输出为 [`ProcessInfo`] 列表（纯函数便测）。
/// toybox ps 列随机型浮动，故按表头列名定位索引：`PID`→pid，`RSS`(KB)→memory_bytes(×1024)，
/// `NAME`/`CMD`→name；`ps -A` 无 %CPU 列 → cpu=0（best-effort，列缺置 0）。无法解析 pid 的行跳过。
fn parse_ps(out: &str) -> Vec<ProcessInfo> {
    let mut lines = out.lines().filter(|l| !l.trim().is_empty());
    let header = match lines.next() {
        Some(h) => h,
        None => return Vec::new(),
    };
    let cols: Vec<&str> = header.split_whitespace().collect();
    let idx = |name: &str| cols.iter().position(|c| c.eq_ignore_ascii_case(name));
    let pid_idx = idx("PID");
    let rss_idx = idx("RSS");
    let name_idx = idx("NAME").or_else(|| idx("CMD")).or_else(|| idx("COMMAND"));
    let mut procs = Vec::new();
    for line in lines {
        let toks: Vec<&str> = line.split_whitespace().collect();
        let pid = match pid_idx
            .and_then(|i| toks.get(i))
            .and_then(|s| s.parse::<u32>().ok())
        {
            Some(p) => p,
            None => continue,
        };
        // NAME 为最后一列：从 name_idx 到行末 join（兼容极少数含空格的进程名）。
        let name = name_idx
            .and_then(|i| toks.get(i..))
            .map(|rest| rest.join(" "))
            .unwrap_or_default();
        let memory_bytes = rss_idx
            .and_then(|i| toks.get(i))
            .and_then(|s| s.parse::<u64>().ok())
            .map(|kb| kb * 1024)
            .unwrap_or(0);
        procs.push(ProcessInfo {
            pid,
            name,
            cpu: 0.0,
            memory_bytes,
        });
    }
    procs
}

// ===== uiautomator XML 手写解析（零新依赖，Free：结构固定 node 元素 + 固定属性）=====

/// 轻量 XML 元素：uiautomator dump 输出为 `<hierarchy>` 根 + 嵌套 `<node ...>`，属性均双引号。
/// 手写解析规避引入 quick-xml（不写占位版本号 Free）；仅需 tag 名 + 属性 + 子节点。
#[derive(Debug)]
struct XmlEl {
    tag: String,
    attrs: Vec<(String, String)>,
    children: Vec<XmlEl>,
}

impl XmlEl {
    /// 按名取属性值（线性扫描，node 属性数固定小）。
    fn attr(&self, key: &str) -> Option<&str> {
        self.attrs
            .iter()
            .find(|(k, _)| k == key)
            .map(|(_, v)| v.as_str())
    }
}

/// XML 实体解码：`&amp; &lt; &gt; &quot; &apos;` + 数字实体 `&#NN; / &#xHH;`（uiautomator 转义集）。
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
            break; // 非引号值：uiautomator 恒双引号，异常则停止
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

/// 手写扫描 uiautomator dump XML → 元素树。识别开始/自闭合/结束标签与 `<?xml?>`/`<!--?-->` 声明，
/// 忽略标签间空白文本（uiautomator 无有意义文本节点）。属性值内 `<>&"` 已实体化，故按 `>` 扫描安全。
fn parse_uiautomator_xml(input: &str) -> Result<XmlEl, CapError> {
    let bytes = input.as_bytes();
    let n = bytes.len();
    let mut i = 0usize;
    let mut stack: Vec<XmlEl> = Vec::new();
    let mut root: Option<XmlEl> = None;
    let bad = |m: &str| CapError::CaptureFailed(format!("uiautomator XML parse: {m}"));
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

/// 解析 uiautomator `bounds="[l,t][r,b]"` → `[x=l, y=t, w=r−l, h=b−t]`。
/// 解析失败或零/负面积（幽灵）→ None。
fn parse_bounds(s: &str) -> Option<[i32; 4]> {
    let s = s.trim().strip_prefix('[')?;
    let (first, second) = s.split_once("][")?;
    let second = second.strip_suffix(']')?;
    let (l, t) = first.split_once(',')?;
    let (r, b) = second.split_once(',')?;
    let l: i32 = l.trim().parse().ok()?;
    let t: i32 = t.trim().parse().ok()?;
    let r: i32 = r.trim().parse().ok()?;
    let b: i32 = b.trim().parse().ok()?;
    let (w, h) = (r - l, b - t);
    if w <= 0 || h <= 0 {
        return None; // 零/负面积 = 幽灵
    }
    Some([l, t, w, h])
}

/// 遍历预算：剩余可创建节点数 + 是否已截断（命中 depth / max_nodes 上限）。
/// 与桌面 a11y.rs `Budget` 同语义（平台无关，此处随 android 单文件本地化，不跨文件复用私有项）。
struct Budget {
    remaining: u32,
    truncated: bool,
}

/// 将 uiautomator XmlEl 映射为 [`A11yNode`]（幽灵过滤 + depth/max_nodes 预算截断，与桌面语义一致）。
/// 幽灵判定（Locked-8）：`visible-to-user="false"` 或 bounds 缺失/非法/零面积——丢弃节点及其子树，
/// 不占预算（幽灵不应挤占 max_nodes）。映射：class→role、text 优先空则 content-desc→name、
/// bounds→[x,y,w,h]、resource-id→value。
fn map_a11y_node(el: &XmlEl, depth: u32, budget: &mut Budget) -> Option<A11yNode> {
    if budget.remaining == 0 {
        budget.truncated = true;
        return None;
    }
    let visible = el.attr("visible-to-user").map(|v| v != "false").unwrap_or(true);
    let bounds = el.attr("bounds").and_then(parse_bounds);
    if !visible || bounds.is_none() {
        return None; // 幽灵：丢弃节点 + 子树，不占预算
    }
    budget.remaining -= 1;
    let role = el.attr("class").unwrap_or("unknown").to_string();
    let text = el.attr("text").unwrap_or("");
    let name = if !text.is_empty() {
        text.to_string()
    } else {
        el.attr("content-desc").unwrap_or("").to_string()
    };
    let value = el
        .attr("resource-id")
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
            if let Some(cn) = map_a11y_node(child, depth - 1, budget) {
                node.children.push(cn);
            }
        }
    }
    Some(node)
}

/// 按角色剪枝：保留角色匹配的节点及其祖先路径（有匹配后代的节点亦保留），其余剪除。
/// role 为 None / 空串时调用方跳过本函数（与桌面 a11y.rs `prune_by_role` 同语义）。
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

/// 深度优先查找首个 `focused="true"` 节点（root="focus" 选择器用）。
fn find_focused(el: &XmlEl) -> Option<&XmlEl> {
    if el.attr("focused") == Some("true") {
        return Some(el);
    }
    for c in &el.children {
        if let Some(f) = find_focused(c) {
            return Some(f);
        }
    }
    None
}

/// 由 uiautomator XML 根 + [`A11yParams`] 构建 [`A11yTree`]（纯函数便测）。
/// 顶层 nodes = 选定根的直接子节点（root="focus"/"focused" → focused 元素，否则 hierarchy 根），
/// 各按 depth/max_nodes 预算遍历；role 指定则按角色剪枝。与桌面 a11y.rs `build_tree` 同形。
fn build_a11y_tree(xml_root: &XmlEl, params: &A11yParams) -> A11yTree {
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
        if let Some(n) = map_a11y_node(child, params.depth, &mut budget) {
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

/// Android 设备驱动。经 [`AdbCli`] adapter shell out 系统 adb 控制目标设备（`serial` 选择）。
/// `Debug` 便于反连侧结构化日志。
#[derive(Debug)]
pub struct AndroidDriver {
    adb: AdbCli,
}

impl AndroidDriver {
    /// 以设备序列号构造。空串表示未指定设备（AdbCli 按单设备默认处理，不加 `-s`）。
    pub fn new(serial: String) -> Self {
        AndroidDriver {
            adb: AdbCli::new(serial),
        }
    }

    /// 便捷：owned argv → 二进制路径（screencap）。截图只读 → **幂等**（失联重连后可安全重发，GAP-1 ②）。
    async fn adb_raw(&self, argv: &[String]) -> Result<Vec<u8>, CapError> {
        let refs: Vec<&str> = argv.iter().map(String::as_str).collect();
        self.adb.exec_raw(&refs, /* idempotent */ true).await
    }

    /// 便捷：owned argv → 文本路径，成功退出码折叠为 [`Ack`]（非 0 退出 → InputFailed）。
    /// input tap/swipe/text/keyevent 均为**非幂等**副作用操作 → idempotent=false（失联重连后不自动重发，
    /// 避免双执行；调用方/agent 可自行重试，GAP-1 ②）。
    async fn adb_ack(&self, argv: &[String]) -> Result<Ack, CapError> {
        let refs: Vec<&str> = argv.iter().map(String::as_str).collect();
        let res = self.adb.run_text(&refs, /* idempotent */ false).await?;
        if res.exit_code == 0 {
            Ok(Ack::ok())
        } else {
            Err(CapError::InputFailed(format!(
                "adb {argv:?} exited {}: {}",
                res.exit_code,
                res.stderr.trim()
            )))
        }
    }

    /// 设备侧删除文件（uiautomator dump 清理，GAP-2）：`adb shell rm -f <path>`。rm 为非幂等副作用
    /// （idempotent=false，失联不自动重发）；清理失败非致命（不影响已成功的 dump→pull 主链），故忽略结果。
    async fn device_rm(&self, device_path: &str) {
        let argv = rm_argv(device_path);
        let refs: Vec<&str> = argv.iter().map(String::as_str).collect();
        let _ = self.adb.run_text(&refs, /* idempotent */ false).await;
    }
}

#[async_trait]
impl ScreenDriver for AndroidDriver {
    async fn screenshot(
        &self,
        _display: Option<u32>,
        opts: ScreenshotOpts,
    ) -> Result<ScreenshotResult, CapError> {
        // Android 单显示（M5）：display 忽略。`adb exec-out screencap` 取 raw RGBA 帧（绝不经文本解码）。
        let raw = self.adb_raw(&screencap_argv()).await?;
        let (w, h, header_size) = parse_screencap_header(&raw)?;
        let pixels = raw[header_size..].to_vec(); // 像素尾，长度 = w*h*4
        // CPU 密集的缩放/编码收敛进 spawn_blocking，复用桌面 XGA + WebP 流水线（零新 decoder 依赖）。
        tokio::task::spawn_blocking(move || {
            crate::screen::encode_scaled(
                pixels,
                w,
                h,
                opts.max_dim.unwrap_or(SCREENSHOT_MAX_DIM),
                opts.quality.unwrap_or(crate::screen::WEBP_QUALITY),
            )
        })
        .await
        .map_err(|e| {
            CapError::CaptureFailed(format!("android screenshot encode task join failed: {e}"))
        })?
    }

    async fn zoom(&self, region: Region, opts: ScreenshotOpts) -> Result<ScreenshotResult, CapError> {
        // P0 契约 region=[x1,y1,x2,y2] → 裁剪所需 (x,y,w,h)（crop_rgba 实收 xywh）。
        let (x, y, w, h) = crate::screen::region_to_xywh(region)?;
        let raw = self.adb_raw(&screencap_argv()).await?;
        let (iw, ih, header_size) = parse_screencap_header(&raw)?;
        let full = raw[header_size..].to_vec();
        tokio::task::spawn_blocking(move || -> Result<ScreenshotResult, CapError> {
            let cropped = crate::screen::crop_rgba(&full, iw, ih, x, y, w, h)?;
            // 缺省不降采样（max_dim = u32::MAX）保留原生分辨率以观察小字；显式传 max_dim 时以参数为准。
            crate::screen::encode_scaled(
                cropped,
                w,
                h,
                opts.max_dim.unwrap_or(u32::MAX),
                opts.quality.unwrap_or(crate::screen::WEBP_QUALITY),
            )
        })
        .await
        .map_err(|e| CapError::CaptureFailed(format!("android zoom encode task join failed: {e}")))?
    }

    async fn list_displays(&self) -> Result<Vec<DisplayInfo>, CapError> {
        // Android 单显示：`adb shell wm size` → 单元素 Vec<DisplayInfo>。wm size 只读 → 幂等（GAP-1 ②）。
        let res = self.adb.run_text(&["shell", "wm", "size"], /* idempotent */ true).await?;
        if res.exit_code != 0 {
            return Err(CapError::CaptureFailed(format!(
                "adb shell wm size exited {}: {}",
                res.exit_code,
                res.stderr.trim()
            )));
        }
        let (w, h) = parse_wm_size(&res.stdout)?;
        Ok(vec![DisplayInfo {
            id: 0,
            name: "android-display-0".to_string(),
            x: 0,
            y: 0,
            width: w,
            height: h,
            is_primary: true,
            scale: 1.0,
        }])
    }

    async fn switch_display(&self, display: u32) -> Result<DisplayInfo, CapError> {
        // Android 单显示：0 → 返回该唯一显示；非 0 → Unsupported（无多显示切换语义）。
        if display != 0 {
            return Err(CapError::Unsupported(format!(
                "android has a single display; switch_display({display}) unsupported"
            )));
        }
        self.list_displays()
            .await?
            .into_iter()
            .next()
            .ok_or_else(|| CapError::CaptureFailed("android display 0 not found".to_string()))
    }
}

#[async_trait]
impl InputDriver for AndroidDriver {
    async fn click(&self, at: Coordinate, button: MouseButton) -> Result<Ack, CapError> {
        // 收 native 像素（dispatch 已 pre-apply to_native，Locked-6），直接喂 adb。
        let argv = match button {
            // Left → 轻点。
            MouseButton::Left => tap_argv(at.x, at.y),
            // Right → 同点长按 600ms（Android 无右键，长按替代上下文菜单语义）。
            MouseButton::Right => swipe_argv(at.x, at.y, at.x, at.y, LONG_PRESS_MS),
            // Middle → 无对应 Android input 语义。
            MouseButton::Middle => {
                return Err(CapError::Unsupported(
                    "middle click has no Android input equivalent".to_string(),
                ))
            }
        };
        self.adb_ack(&argv).await
    }

    async fn type_text(&self, text: String) -> Result<Ack, CapError> {
        if text.is_empty() {
            return Ok(Ack::ok()); // 空文本无操作，视作成功（避免空 argv 传 adb）。
        }
        self.adb_ack(&text_argv(&text)).await
    }

    async fn key(&self, keys: String) -> Result<Ack, CapError> {
        let norm = keys.trim().to_ascii_lowercase();
        // 修饰符组合（如 "ctrl+c"）无单 keyevent 表达 → Unsupported（Locked-6）。
        if norm.contains('+') {
            return Err(CapError::Unsupported(format!(
                "key chord '{keys}' unsupported on Android (no modifier keyevent composition)"
            )));
        }
        match keycode_of(&norm) {
            Some(code) => self.adb_ack(&keyevent_argv(code)).await,
            None => Err(CapError::Unsupported(format!(
                "unknown key '{keys}' has no Android KEYCODE mapping"
            ))),
        }
    }

    async fn scroll(
        &self,
        at: Coordinate,
        direction: ScrollDirection,
        amount: i32,
    ) -> Result<Ack, CapError> {
        // 以 anchor 为起点沿「反向量」swipe：手指移动方向与内容滚动方向相反。
        // Down=上滑（手指上移露出下方内容）/ Up=下滑 / Left=右滑 / Right=左滑。amount 缩放距离（默认 3）。
        let distance = amount.max(1) * SCROLL_STEP_PX;
        let (dx, dy) = match direction {
            ScrollDirection::Down => (0, -distance),
            ScrollDirection::Up => (0, distance),
            ScrollDirection::Left => (distance, 0),
            ScrollDirection::Right => (-distance, 0),
        };
        let argv = swipe_argv(at.x, at.y, at.x + dx, at.y + dy, SCROLL_DURATION_MS);
        self.adb_ack(&argv).await
    }

    async fn drag(&self, from: Coordinate, to: Coordinate) -> Result<Ack, CapError> {
        // 拖拽 = 持续 300ms 的 swipe（够触发长按拖拽而非快速 fling）。
        let argv = swipe_argv(from.x, from.y, to.x, to.y, DRAG_DURATION_MS);
        self.adb_ack(&argv).await
    }

    async fn move_mouse(&self, _to: Coordinate) -> Result<Ack, CapError> {
        // Android 触摸屏无「悬停指针」语义——move_mouse 无对应操作（Locked-6）。
        Err(CapError::Unsupported(
            "move_mouse has no equivalent on Android touch input".to_string(),
        ))
    }

    async fn wait(&self, duration_ms: u64) -> Result<Ack, CapError> {
        // 平台无关：本地 sleep，不经 adb。
        tokio::time::sleep(std::time::Duration::from_millis(duration_ms)).await;
        Ok(Ack::ok())
    }
}

// ProcessFileDriver 整域下沉（D4）：run_command/file/proc 全走设备语义（adb），data_dir 宿主钉死
// （见文件头 SEAM）。命令跑在被测设备而非 node 宿主 shell；file_push/pull ↔ /sdcard 保 push 后 run 一致性。
#[async_trait]
impl ProcessFileDriver for AndroidDriver {
    async fn list_processes(&self) -> Result<Vec<ProcessInfo>, CapError> {
        // `adb shell ps -A` → 按表头列名解析（cpu 列不可得置 0，best-effort）。
        let argv = ps_argv();
        let refs: Vec<&str> = argv.iter().map(String::as_str).collect();
        // ps -A 只读枚举 → 幂等（GAP-1 ②）。
        let res = self.adb.run_text(&refs, /* idempotent */ true).await?;
        if res.exit_code != 0 {
            return Err(CapError::ProcessError(format!(
                "adb shell ps -A exited {}: {}",
                res.exit_code,
                res.stderr.trim()
            )));
        }
        Ok(parse_ps(&res.stdout))
    }

    async fn kill_process(&self, pid: u32) -> Result<Ack, CapError> {
        // 按 pid：`adb shell kill <pid>`（非 root 常失败 → ProcessError，不假 PASS）。
        let argv = kill_argv(pid);
        let refs: Vec<&str> = argv.iter().map(String::as_str).collect();
        // kill 有副作用 → 非幂等（GAP-1 ②，失联重连后不自动重发）。
        let res = self.adb.run_text(&refs, /* idempotent */ false).await?;
        if res.exit_code == 0 {
            Ok(Ack::ok())
        } else {
            Err(CapError::ProcessError(format!(
                "adb shell kill {pid} exited {}: {}",
                res.exit_code,
                res.stderr.trim()
            )))
        }
    }

    async fn file_push(
        &self,
        remote_path: String,
        content_base64: String,
    ) -> Result<FileResult, CapError> {
        // base64 解码 → 宿主暂存 → `adb push` → 设备 /sdcard 路径（Locked-6）。宿主暂存无论成败清理。
        let bytes = STANDARD
            .decode(content_base64.as_bytes())
            .map_err(|e| CapError::InvalidArg(format!("invalid base64 content: {e}")))?;
        let tmp = host_temp_path("push");
        tokio::fs::write(&tmp, &bytes).await.map_err(|e| {
            CapError::FileError(format!("write host temp {} failed: {e}", tmp.display()))
        })?;
        let tmp_str = tmp.to_string_lossy().into_owned();
        let argv = push_argv(&tmp_str, &remote_path);
        let refs: Vec<&str> = argv.iter().map(String::as_str).collect();
        // push 写设备文件有副作用 → 非幂等（GAP-1 ②）。
        let res = self.adb.run_text(&refs, /* idempotent */ false).await;
        let _ = tokio::fs::remove_file(&tmp).await;
        let res = res?;
        if res.exit_code != 0 {
            return Err(CapError::FileError(format!(
                "adb push to {remote_path} exited {}: {}",
                res.exit_code,
                res.stderr.trim()
            )));
        }
        // push 语义：不回带内容。size 取本地解码字节数（与设备落盘一致）。
        Ok(FileResult {
            path: remote_path,
            size_bytes: bytes.len() as u64,
            content_base64: None,
        })
    }

    async fn file_pull(&self, remote_path: String) -> Result<FileResult, CapError> {
        // `adb pull` → 宿主暂存 → 读文件 → 10MiB 上限校验 → base64 回带（宿主暂存无论成败清理）。
        let tmp = host_temp_path("pull");
        let tmp_str = tmp.to_string_lossy().into_owned();
        let argv = pull_argv(&remote_path, &tmp_str);
        let refs: Vec<&str> = argv.iter().map(String::as_str).collect();
        // pull 只读回带（读设备文件到宿主，不改设备态）→ 幂等（GAP-1 ②）。
        let res = self.adb.run_text(&refs, /* idempotent */ true).await?;
        if res.exit_code != 0 {
            let _ = tokio::fs::remove_file(&tmp).await;
            return Err(CapError::FileError(format!(
                "adb pull {remote_path} exited {}: {}",
                res.exit_code,
                res.stderr.trim()
            )));
        }
        let meta = tokio::fs::metadata(&tmp).await.map_err(|e| {
            CapError::FileError(format!("stat pulled temp {} failed: {e}", tmp.display()))
        })?;
        if meta.len() > MAX_PULL_BYTES {
            let _ = tokio::fs::remove_file(&tmp).await;
            return Err(CapError::InvalidArg(format!(
                "file too large to pull: {} bytes exceeds {MAX_PULL_BYTES} limit",
                meta.len()
            )));
        }
        let bytes = tokio::fs::read(&tmp).await.map_err(|e| {
            CapError::FileError(format!("read pulled temp {} failed: {e}", tmp.display()))
        })?;
        let _ = tokio::fs::remove_file(&tmp).await;
        let content = STANDARD.encode(&bytes);
        Ok(FileResult {
            path: remote_path,
            size_bytes: bytes.len() as u64,
            content_base64: Some(content),
        })
    }

    async fn run_command(
        &self,
        cmd: String,
        args: Vec<String>,
        timeout_ms: Option<u64>,
        cwd: Option<String>,
    ) -> Result<CmdResult, CapError> {
        // 设备语义：`adb -s <serial> shell [cd <cwd> &&] <cmd> <args...>`（D4，非宿主 shell）。
        let argv = run_command_argv(&cmd, &args, cwd.as_deref());
        let refs: Vec<&str> = argv.iter().map(String::as_str).collect();
        let timeout =
            std::time::Duration::from_millis(timeout_ms.unwrap_or(RUN_COMMAND_DEFAULT_TIMEOUT_MS));
        // run_command 执行任意命令，语义非幂等 → 失联重连后不自动重发（GAP-1 ②）。
        self.adb.run_text_timeout(&refs, timeout, /* idempotent */ false).await
    }
}

#[async_trait]
impl A11yDriver for AndroidDriver {
    async fn get_a11y_tree(&self, params: A11yParams) -> Result<A11yTree, CapError> {
        // 设备侧路径唯一化（GAP-2）：避免固定 /sdcard/window_dump.xml 在并发/多 node dump 下互踩；
        // dump→pull→设备侧 rm 清理，避免 /sdcard 残骸累积。
        let device_path = ui_dump_device_path();
        // 1) uiautomator dump 写设备文件（禁管道直读避截断，Locked-8）。dump 为只读快照 → 幂等（GAP-1 ②）。
        let dump_av = uiautomator_dump_argv(&device_path);
        let dump_refs: Vec<&str> = dump_av.iter().map(String::as_str).collect();
        let dumped = self.adb.run_text(&dump_refs, /* idempotent */ true).await?;
        // uiautomator 成功写 stdout "UI hierchary dumped to: ..."；空树/挂起以非 0 或缺文件暴露（不假 PASS）。
        if dumped.exit_code != 0 {
            self.device_rm(&device_path).await; // 清理可能的半成品文件（GAP-2）
            return Err(CapError::CaptureFailed(format!(
                "uiautomator dump exited {}: {}",
                dumped.exit_code,
                dumped.stderr.trim()
            )));
        }
        // 2) adb pull 设备文件 → 宿主暂存，再读**文件**（非 adb stdout，杜绝管道截断，Locked-8）。pull 幂等。
        let tmp = host_temp_path("uidump");
        let tmp_str = tmp.to_string_lossy().into_owned();
        let pull_av = pull_argv(&device_path, &tmp_str);
        let pull_refs: Vec<&str> = pull_av.iter().map(String::as_str).collect();
        let pulled = self.adb.run_text(&pull_refs, /* idempotent */ true).await?;
        if pulled.exit_code != 0 {
            let _ = tokio::fs::remove_file(&tmp).await;
            self.device_rm(&device_path).await; // 失败也清理设备侧文件（GAP-2）
            return Err(CapError::CaptureFailed(format!(
                "adb pull ui dump exited {}: {}",
                pulled.exit_code,
                pulled.stderr.trim()
            )));
        }
        // 3) 设备侧 rm 清理（内容已到宿主暂存，GAP-2 避免 /sdcard 残骸）。清理失败非致命。
        self.device_rm(&device_path).await;
        let xml_bytes = tokio::fs::read(&tmp).await.map_err(|e| {
            CapError::CaptureFailed(format!("read pulled ui dump {} failed: {e}", tmp.display()))
        })?;
        let _ = tokio::fs::remove_file(&tmp).await;
        let xml = String::from_utf8(xml_bytes)
            .map_err(|e| CapError::CaptureFailed(format!("ui dump not valid utf-8: {e}")))?;
        // 4) 手写解析 XML → 幽灵过滤 + depth/max_nodes 截断 + role 剪枝 → A11yTree（纯函数）。
        let root = parse_uiautomator_xml(&xml)?;
        Ok(build_a11y_tree(&root, &params))
    }
}

// AssertDriver 为平台无关组合能力（text 纯逻辑 + a11y/image 复用本 driver 的 get_a11y_tree/screenshot），
// 默认方法即全部实现；空 impl 并入 DeviceDriver bound（同 PlatformDriver）。TASK-004 后 get_a11y_tree
// 与 screenshot 均实装，text/a11y/image 三形态均可真实断言（不假 PASS）；契约矩阵组合验收见 TASK-005。
impl AssertDriver for AndroidDriver {}

// ===== record 域：设备侧 screenrecord + adb pull 桥接 =====
//
// Android 录屏走设备原生 `screenrecord`：start 在设备后台起录（`--time-limit` 自终止兜底），
// stop 经设备侧 `pkill -INT screenrecord` 令其 finalize MP4 trailer（moov）后 `adb pull` 回宿主
// output_path（传输层注入，<data_dir>/artifacts/<key> 同源，走 G-5 旁路 PUT 上传）。与桌面 WGC
// 进程内落盘正交：产物落设备 /sdcard 再桥接回宿主。会话态进程内持有（rec_id → 设备路径+宿主路径+
// 起录时刻），复刻 record.rs 桌面 backend 的 OnceLock 惰性单例先例。screenrecord 无 fps 控制
// （按设备刷新率采集），故 RecordParams.fps 不透传；bitrate 固定安全值，时长按 max_duration_secs 封顶。

/// screenrecord 码率（bps）：2.5 Mbps，与桌面 record.rs `BITRATE` 对齐——redroid 软编下文字仍清晰、
/// 不撑爆 /sdcard，体积较原 4 Mbps 再省约 38%。
const REC_BITRATE_BPS: u32 = 2_500_000;
/// 录屏时长缺省（秒）——与 record.rs 桌面 DEFAULT_DURATION_SECS 对齐。
const REC_DEFAULT_DURATION_SECS: u64 = 30;
/// 录屏时长封顶（秒）：screenrecord 历史 180s 上限，取其为安全上限（redroid 亦稳）。
const REC_MAX_DURATION_SECS: u64 = 180;
/// stop 后轮询等待 screenrecord 退出（finalize moov）的步进 + 封顶窗口。
const REC_FINALIZE_POLL: std::time::Duration = std::time::Duration::from_millis(150);
const REC_FINALIZE_DEADLINE: std::time::Duration = std::time::Duration::from_secs(4);

/// 活动录屏会话（rec_id → 设备路径 + 宿主落盘路径 + 起录时刻 + 请求时长封顶）。纯数据：
/// screenrecord 在设备侧独立运行，无本地子进程句柄需持有（stop 经设备侧 pkill 终止）。
struct AndroidRecSession {
    device_path: String,
    output_path: PathBuf,
    started: Instant,
    duration_secs: u64,
}

/// 进程内录屏会话注册表（rec_id → 活动会话）。复刻 record.rs / a11y UIA_TX 的 OnceLock 惰性单例先例。
static REC_SESSIONS: OnceLock<Mutex<HashMap<String, AndroidRecSession>>> = OnceLock::new();

/// 取会话注册表锁（毒锁不二次 panic，同 record.rs sessions_guard 惯例——表无跨字段不变量，
/// 中毒源 panic 已被工具边界记录，续用内层数据安全）。
fn rec_sessions_guard() -> MutexGuard<'static, HashMap<String, AndroidRecSession>> {
    REC_SESSIONS
        .get_or_init(|| Mutex::new(HashMap::new()))
        .lock()
        .unwrap_or_else(PoisonError::into_inner)
}

/// 录屏时长封顶（纯函数，单测面）：clamp 到 [1, REC_MAX_DURATION_SECS]，缺省 REC_DEFAULT_DURATION_SECS。
/// 语义对齐 record.rs 桌面 clamp_params 的时长面。
fn clamp_rec_duration(params: &RecordParams) -> u64 {
    params
        .max_duration_secs
        .unwrap_or(REC_DEFAULT_DURATION_SECS)
        .clamp(1, REC_MAX_DURATION_SECS)
}

/// 会话设备侧落盘路径（纯函数，单测面）：rec_id 唯一 → 并发/多 node 不互踩；stop 后 rm 清理防
/// /sdcard 残骸。仅取字母数字/短横/下划线防路径注入，兜底非空。
fn rec_device_path(rec_id: &str) -> String {
    let safe: String = rec_id
        .chars()
        .filter(|c| c.is_ascii_alphanumeric() || *c == '-' || *c == '_')
        .collect();
    let safe = if safe.is_empty() { "rec".to_string() } else { safe };
    format!("/sdcard/aura-rec-{safe}.mp4")
}

#[async_trait]
impl RecordDriver for AndroidDriver {
    /// 起设备侧 screenrecord 后台录制：stdio 重定向 + `&` 令 adb shell 即返回（screenrecord 独立于
    /// adb 连接运行）；`--time-limit` 为自终止兜底。起录后 pidof 校验进程已在，否则回收会话并报
    /// CaptureFailed（screenrecord 缺失/忙经 stdio 重定向不可见，pidof 兜底防静默空录）。
    async fn start_recording(
        &self,
        rec_id: String,
        params: RecordParams,
        output_path: PathBuf,
    ) -> Result<(), CapError> {
        if rec_sessions_guard().contains_key(&rec_id) {
            return Err(CapError::InvalidArg(format!(
                "recording session '{rec_id}' already active"
            )));
        }
        let duration_secs = clamp_rec_duration(&params);
        let device_path = rec_device_path(&rec_id);

        // 清理同名残留（上轮异常遗留），避免 screenrecord 拒写已存在文件。
        let _ = self
            .adb
            .run_text(&["shell", &format!("rm -f {device_path}")], /* idempotent */ false)
            .await;

        let launch = format!(
            "screenrecord --time-limit {duration_secs} --bit-rate {REC_BITRATE_BPS} {device_path} >/dev/null 2>&1 &"
        );
        self.adb
            .run_text(&["shell", &launch], /* idempotent */ false)
            .await?;

        // 校验录制真起（screenrecord 缺失/起录失败经 stdio 重定向不可见，pidof 兜底）。
        tokio::time::sleep(std::time::Duration::from_millis(400)).await;
        let alive = self
            .adb
            .run_text(&["shell", "pidof screenrecord"], /* idempotent */ true)
            .await
            .map(|r| !r.stdout.trim().is_empty())
            .unwrap_or(false);
        if !alive {
            let _ = self
                .adb
                .run_text(&["shell", &format!("rm -f {device_path}")], false)
                .await;
            return Err(CapError::CaptureFailed(
                "screenrecord failed to start on device (missing binary or busy)".to_string(),
            ));
        }

        rec_sessions_guard().insert(
            rec_id,
            AndroidRecSession {
                device_path,
                output_path,
                started: Instant::now(),
                duration_secs,
            },
        );
        Ok(())
    }

    /// 停录：设备侧 `pkill -INT screenrecord` → 轮询至进程退出（moov 落盘）→ `adb pull` 回宿主
    /// output_path → 设备侧 `rm` 清理 → stat 宿主产物构造 [`RecordArtifact`]。会话不存在
    /// （未 start 或已 stop）→ E_INVALID_ARG。
    async fn stop_recording(&self, rec_id: String) -> Result<RecordArtifact, CapError> {
        let session = rec_sessions_guard()
            .remove(&rec_id)
            .ok_or_else(|| CapError::InvalidArg(format!("no active recording session '{rec_id}'")))?;
        let elapsed = session.started.elapsed();

        // 令设备侧 screenrecord 收 SIGINT，finalize MP4 trailer（未捕获则产物缺 moov 不可播）。
        let _ = self
            .adb
            .run_text(&["shell", "pkill -INT screenrecord"], /* idempotent */ false)
            .await;

        // 轮询等待 screenrecord 退出（trailer 写完再 pull），封顶 REC_FINALIZE_DEADLINE。
        let deadline = Instant::now() + REC_FINALIZE_DEADLINE;
        loop {
            let gone = self
                .adb
                .run_text(&["shell", "pidof screenrecord"], /* idempotent */ true)
                .await
                .map(|r| r.stdout.trim().is_empty())
                .unwrap_or(true);
            if gone || Instant::now() >= deadline {
                break;
            }
            tokio::time::sleep(REC_FINALIZE_POLL).await;
        }

        // 宿主 output 父目录兜底创建（传输层通常已建；防守）。
        if let Some(parent) = session.output_path.parent() {
            let _ = std::fs::create_dir_all(parent);
        }
        let out_str = session.output_path.display().to_string();
        let pull = self
            .adb
            .run_text(&["pull", &session.device_path, &out_str], /* idempotent */ true)
            .await?;
        // 无论 pull 结果，清理设备侧产物防 /sdcard 残骸累积。
        let _ = self
            .adb
            .run_text(&["shell", &format!("rm -f {}", session.device_path)], false)
            .await;
        if pull.exit_code != 0 {
            return Err(CapError::CaptureFailed(format!(
                "adb pull recording failed (exit {}): {}",
                pull.exit_code,
                pull.stderr.trim()
            )));
        }

        let meta = std::fs::metadata(&session.output_path).map_err(|e| {
            CapError::CaptureFailed(format!("recording output not found after pull: {e}"))
        })?;
        let size_bytes = meta.len();
        if size_bytes == 0 {
            return Err(CapError::CaptureFailed(
                "recording produced an empty file".to_string(),
            ));
        }
        Ok(RecordArtifact {
            mime: "video/mp4".to_string(),
            size_bytes,
            duration_ms: elapsed.as_millis() as u64,
            // screenrecord 不报帧数（无 progress 通路）；informational 字段留 0。
            frame_count: 0,
            // 起录到 stop 已达/超请求时长 → screenrecord 大概率已按 --time-limit 自截（部分产物语义）。
            truncated: elapsed.as_secs() >= session.duration_secs,
        })
    }
}

/// audio 域（M11）：Android 无宿主侧音频注入通路（设备音频经 adb 无 null-sink 语义），矩阵
/// Unsupported——call-time 恒 E_UNSUPPORTED（广告面 supports_tool 同步剔除，dispatch 超集网不动）。
#[async_trait]
impl AudioDriver for AndroidDriver {
    async fn audio_inject(
        &self,
        _params: AudioInjectParams,
    ) -> Result<AudioInjectResult, CapError> {
        Err(CapError::Unsupported(
            "audio_inject is not supported on android (no host-side virtual audio sink path)"
                .to_string(),
        ))
    }
}

/// 聚合 DeviceDriver：七子 trait 全实现后并入；`platform()` 覆写为 `"android"`（默认 `"desktop"`），
/// 传输层 Register 帧据此派生 platform（非 host `cfg!(target_os)`，node 宿主 OS ≠ 被控设备平台）。
#[async_trait]
impl DeviceDriver for AndroidDriver {
    fn platform(&self) -> &'static str {
        "android"
    }

    /// 被控设备系统版本（M12 批C，platform() 同款驱动派生）：adb `getprop ro.build.version.release`
    /// → 主版本号（如 "13"）→ 归一「Android 13」。任何失败（adb 不在/设备失联/空输出）返回
    /// `Some("")` 显式空——前端不显，但**绝不**回落宿主采集（节点程序跑 Linux 宿主，宿主
    /// os-release 值对 android 节点是错报，正是本钩子要修的语义坑）。getprop 只读幂等（失联重连
    /// 后可安全重发，同 screencap 口径）。
    async fn os_version(&self) -> Option<String> {
        let release = match self
            .adb
            .run_text(
                &["shell", "getprop", "ro.build.version.release"],
                /* idempotent */ true,
            )
            .await
        {
            Ok(r) if r.exit_code == 0 => r.stdout.trim().to_string(),
            _ => String::new(),
        };
        if release.is_empty() {
            Some(String::new())
        } else {
            Some(format!("Android {release}"))
        }
    }

    /// 派生服务标识（M12 批D）：serial 配置自动拼「redroid@<serial>」（生产 android 节点驱动的是
    /// Redroid 容器，见 deploy/redroid）。serial 空（单设备默认，未指定目标）无从标识 → None
    /// （宁空勿猜）。部署面另有 env AURA_ATTACHED 覆写通路（传输层优先），非 redroid 的 adb 目标
    /// （真机等）经 env 声明真实形态。
    fn attached(&self) -> Option<String> {
        if self.adb.serial.is_empty() {
            None
        } else {
            Some(format!("redroid@{}", self.adb.serial))
        }
    }

    /// Android 广告面剔除 2 个无对应语义的工具（D5 + M11）：`move_mouse`（触摸屏无悬停指针）+
    /// `audio_inject`（M11：无宿主侧虚拟音频注入通路，矩阵 Unsupported）。`start_recording`/
    /// `stop_recording` 已实装（设备侧 screenrecord + adb pull 桥接，见 RecordDriver impl），进广告面。
    /// 其余 19 工具进广告面（21 − 2）。
    /// 注意：被剔 2 工具在 dispatch 超集网仍恒可派发，call-time 返 `E_UNSUPPORTED`（tool_dispatch 全臂
    /// 不动）；本谓词仅收窄 Register.tools/list_tools 广告面。`middle-click`/`key-chords` **不**剔——
    /// `click`/`key` 工具名整体仍支持，仅特定入参（中键 / 修饰组合）走 call-time `E_UNSUPPORTED`。
    fn supports_tool(&self, tool: &str) -> bool {
        !matches!(tool, "move_mouse" | "audio_inject")
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// 合成 screencap raw 帧（[w][h][format]( [colorspace] ) + w*h*4 像素尾），供反推 parser 单测。
    /// `header_len` 取 12（Android<9）或 16（Android≥9），像素值无关取递增填充。
    fn synth_screencap(w: u32, h: u32, header_len: usize) -> Vec<u8> {
        let mut v = Vec::new();
        v.extend_from_slice(&w.to_le_bytes());
        v.extend_from_slice(&h.to_le_bytes());
        v.extend_from_slice(&0u32.to_le_bytes()); // format
        if header_len == 16 {
            v.extend_from_slice(&0u32.to_le_bytes()); // colorspace/dataspace（Android≥9）
        }
        let px = (w as usize) * (h as usize) * 4;
        v.extend((0..px).map(|i| (i % 256) as u8));
        v
    }

    /// `&[&str]` → `Vec<String>`，便于与 argv builder 结果断言。
    fn sv(parts: &[&str]) -> Vec<String> {
        parts.iter().map(|s| s.to_string()).collect()
    }

    // ---- screencap header 反推（12B / 16B 双 case，Locked-3）----

    #[test]
    fn screencap_header_12b_reverse_derived() {
        let frame = synth_screencap(3, 2, 12);
        let (w, h, hs) = parse_screencap_header(&frame).unwrap();
        assert_eq!((w, h, hs), (3, 2, 12));
        assert_eq!(frame.len() - hs, (3 * 2 * 4) as usize, "pixel tail = w*h*4");
    }

    #[test]
    fn screencap_header_16b_reverse_derived() {
        // Android≥9：16B header（含 dataspace）。反推公式须自然得出 16，不硬编码。
        let frame = synth_screencap(4, 5, 16);
        let (w, h, hs) = parse_screencap_header(&frame).unwrap();
        assert_eq!((w, h, hs), (4, 5, 16));
        assert_eq!(frame.len() - hs, (4 * 5 * 4) as usize);
    }

    #[test]
    fn screencap_header_rejects_short_frame() {
        assert!(parse_screencap_header(&[0u8; 8]).is_err());
    }

    #[test]
    fn screencap_header_rejects_size_mismatch() {
        // 破坏像素尾长度：w*h*4 大于剩余帧长 → 反推越界。
        let mut frame = synth_screencap(4, 5, 16);
        frame.truncate(20);
        assert!(parse_screencap_header(&frame).is_err());
    }

    // ---- adb argv builder（tap/swipe/text/keyevent/screencap 参数向量断言）----

    #[test]
    fn screencap_argv_is_exec_out_screencap() {
        assert_eq!(screencap_argv(), sv(&["exec-out", "screencap"]));
    }

    #[test]
    fn tap_argv_builds_input_tap() {
        assert_eq!(tap_argv(120, 340), sv(&["shell", "input", "tap", "120", "340"]));
    }

    #[test]
    fn swipe_argv_builds_input_swipe_with_duration() {
        assert_eq!(
            swipe_argv(10, 20, 30, 40, 600),
            sv(&["shell", "input", "swipe", "10", "20", "30", "40", "600"])
        );
    }

    #[test]
    fn keyevent_argv_builds_input_keyevent() {
        assert_eq!(keyevent_argv(66), sv(&["shell", "input", "keyevent", "66"]));
    }

    #[test]
    fn text_argv_escapes_and_builds_input_text() {
        assert_eq!(text_argv("a b"), sv(&["shell", "input", "text", "a%sb"]));
        // GAP-3：含 $ 文本经 text_argv 落地为已转义 argv（防设备 sh 展开）。
        assert_eq!(
            text_argv("price=$5"),
            sv(&["shell", "input", "text", "price=\\$5"])
        );
    }

    // ---- keycode 映射表（命名键 → KEYCODE 数字码）----

    #[test]
    fn keycode_of_maps_named_keys() {
        assert_eq!(keycode_of("enter"), Some(66));
        assert_eq!(keycode_of("back"), Some(4));
        assert_eq!(keycode_of("home"), Some(3));
        assert_eq!(keycode_of("tab"), Some(61));
        assert_eq!(keycode_of("del"), Some(67));
        assert_eq!(keycode_of("menu"), Some(82));
        assert_eq!(keycode_of("volume_up"), Some(24));
        assert_eq!(keycode_of("f13"), None, "未列键 → None → 上层 Unsupported");
    }

    // ---- input text 转义（空格→%s + shell 元字符）----

    #[test]
    fn escape_input_text_space_and_shell_meta() {
        assert_eq!(escape_input_text("hello world"), "hello%sworld");
        assert_eq!(escape_input_text("a&b|c;d"), "a\\&b\\|c\\;d");
        assert_eq!(escape_input_text("(x)*~"), "\\(x\\)\\*\\~");
        assert_eq!(escape_input_text("plain"), "plain");
        // GAP-3：$（变量/命令替换）与反引号（命令替换）必须转义，防设备 sh 展开吞字符/执行子命令。
        assert_eq!(escape_input_text("price=$5"), "price=\\$5");
        assert_eq!(escape_input_text("$HOME"), "\\$HOME");
        assert_eq!(escape_input_text("a`b`c"), "a\\`b\\`c");
    }

    // ---- wm size 解析 ----

    #[test]
    fn parse_wm_size_reads_physical() {
        assert_eq!(
            parse_wm_size("Physical size: 1080x2400\n").unwrap(),
            (1080, 2400)
        );
    }

    #[test]
    fn parse_wm_size_prefers_override() {
        let out = "Physical size: 1080x2400\nOverride size: 720x1600\n";
        assert_eq!(parse_wm_size(out).unwrap(), (720, 1600));
    }

    #[test]
    fn parse_wm_size_rejects_garbage() {
        assert!(parse_wm_size("no dimensions here").is_err());
    }

    // ---- AdbCli.full_argv：-s serial 贯穿（Locked-2）----

    #[test]
    fn full_argv_injects_serial_dash_s() {
        let cli = AdbCli::new("localhost:5555".to_string());
        assert_eq!(
            cli.full_argv(&["shell", "input", "tap", "1", "2"]),
            vec!["-s", "localhost:5555", "shell", "input", "tap", "1", "2"]
        );
    }

    #[test]
    fn full_argv_omits_serial_when_empty() {
        let cli = AdbCli::new(String::new());
        assert_eq!(cli.full_argv(&["shell", "wm", "size"]), vec!["shell", "wm", "size"]);
    }

    // ---- 失联检测（触发 adb connect 重连）----

    #[test]
    fn detects_device_disconnected_stderr() {
        // 连接层失联 → true（锚定 adb 客户端错误格式）。
        assert!(is_device_disconnected("error: device offline"));
        assert!(is_device_disconnected("error: device 'emulator-5554' not found"));
        assert!(is_device_disconnected("error: device 'localhost:5555' not found"));
        assert!(is_device_disconnected("adb: no devices/emulators found"));
        assert!(is_device_disconnected("cannot connect to 127.0.0.1:5555"));
        assert!(is_device_disconnected("failed to connect: Connection refused"));
        // 收窄（GAP-1 ①）：命令自身输出里的泛化 "not found" 不得误判为设备失联（否则误重连 + 双执行）。
        assert!(!is_device_disconnected("/system/bin/sh: foo: not found"));
        assert!(!is_device_disconnected("ls: bar.txt: No such file or directory"));
        assert!(!is_device_disconnected("Error: object not found in registry"));
        assert!(!is_device_disconnected("Success"));
    }

    // ---- 失联重连/重发决策（GAP-1 ②：非幂等失联不自动重发，幂等重发）----

    #[test]
    fn reconnect_plan_idempotent_retries_after_reconnect() {
        // 幂等只读操作失联 → 重连后重发（screencap/dump/pull/ps/wm size 等）。
        assert_eq!(reconnect_plan(true, true, true), ReconnectPlan::RetryIdempotent);
    }

    #[test]
    fn reconnect_plan_non_idempotent_reconnects_without_retry() {
        // 非幂等副作用操作失联 → 重连但不重发（input/push/kill/rm/run_command），避免双执行。
        assert_eq!(
            reconnect_plan(true, true, false),
            ReconnectPlan::ReconnectNoRetry
        );
    }

    #[test]
    fn reconnect_plan_no_reconnect_when_connected_or_no_serial() {
        // 未失联 → 不重连（无论幂等性）。
        assert_eq!(reconnect_plan(true, false, true), ReconnectPlan::None);
        assert_eq!(reconnect_plan(true, false, false), ReconnectPlan::None);
        // 无 serial → 无从 adb connect，不重连。
        assert_eq!(reconnect_plan(false, true, false), ReconnectPlan::None);
    }

    // ---- remap Unsupported 三面（Middle click / key 组合 / move_mouse），无需 adb（早返回）----

    #[tokio::test]
    async fn middle_click_is_unsupported() {
        let d = AndroidDriver::new("x".to_string());
        let err = d
            .click(Coordinate { x: 1, y: 2 }, MouseButton::Middle)
            .await
            .unwrap_err();
        assert_eq!(err.code(), "E_UNSUPPORTED");
    }

    #[tokio::test]
    async fn key_chord_is_unsupported() {
        let d = AndroidDriver::new("x".to_string());
        let err = d.key("ctrl+c".to_string()).await.unwrap_err();
        assert_eq!(err.code(), "E_UNSUPPORTED");
    }

    #[tokio::test]
    async fn unknown_key_is_unsupported() {
        let d = AndroidDriver::new("x".to_string());
        let err = d.key("printscreen".to_string()).await.unwrap_err();
        assert_eq!(err.code(), "E_UNSUPPORTED");
    }

    #[tokio::test]
    async fn move_mouse_is_unsupported() {
        let d = AndroidDriver::new("x".to_string());
        let err = d.move_mouse(Coordinate { x: 1, y: 2 }).await.unwrap_err();
        assert_eq!(err.code(), "E_UNSUPPORTED");
    }

    #[tokio::test]
    async fn switch_display_nonzero_is_unsupported() {
        let d = AndroidDriver::new("x".to_string());
        let err = d.switch_display(1).await.unwrap_err();
        assert_eq!(err.code(), "E_UNSUPPORTED");
    }

    #[tokio::test]
    async fn empty_type_text_is_ok_without_adb() {
        // 空文本短路成功，不 spawn adb（构建机无 adb 亦绿）。
        let d = AndroidDriver::new("x".to_string());
        assert!(d.type_text(String::new()).await.is_ok());
    }

    #[tokio::test]
    async fn wait_sleeps_without_adb() {
        let d = AndroidDriver::new("x".to_string());
        assert!(d.wait(1).await.is_ok());
    }

    #[test]
    fn platform_is_android() {
        assert_eq!(AndroidDriver::new("x".to_string()).platform(), "android");
    }

    /// M12 批C：os_version 覆写在 adb 不可用/设备不存在时返回 `Some("")` 显式空——绝不 `None`
    /// （None 令传输层回落宿主采集，android 节点会错报宿主系统版本，正是覆写要修的语义坑）。
    /// 不依赖 adb 是否安装：无 adb → run_text Err；有 adb → 假 serial 非 0 退出，两路同归显式空。
    #[tokio::test]
    async fn os_version_unavailable_is_explicit_empty_not_none() {
        let d = AndroidDriver::new("aura-test-no-such-device".to_string());
        assert_eq!(d.os_version().await, Some(String::new()));
    }

    /// M12 批D：attached 从 serial 配置拼装（redroid@<serial>）；serial 空（单设备默认）无从标识
    /// 返回 None（宁空勿猜），交传输层 env AURA_ATTACHED 覆写通路。
    #[test]
    fn attached_derives_from_serial() {
        assert_eq!(
            AndroidDriver::new("localhost:5555".to_string()).attached(),
            Some("redroid@localhost:5555".to_string())
        );
        assert_eq!(AndroidDriver::new(String::new()).attached(), None);
    }

    // ================= TASK-004：a11y XML 解析 + process/file remap 纯函数单测 =================

    /// uiautomator dump 样例（含幽灵元素：零面积 ghost + visible-to-user=false hidden；
    /// focused 节点带子节点便于 root="focus" 断言；content-desc 含 `&amp;` 实体便于解码断言）。
    const UIA_XML_FIXTURE: &str = r#"<?xml version='1.0' encoding='UTF-8' standalone='yes' ?>
<hierarchy rotation="0">
  <node index="0" text="" resource-id="" class="android.widget.FrameLayout" package="pkg" content-desc="" bounds="[0,0][720,1280]" visible-to-user="true">
    <node index="0" text="Hello" resource-id="com.app:id/title" class="android.widget.TextView" content-desc="" bounds="[10,20][210,80]" visible-to-user="true" focused="false" />
    <node index="1" text="" resource-id="com.app:id/ghost" class="android.widget.View" content-desc="" bounds="[0,0][0,0]" visible-to-user="true" />
    <node index="2" text="" resource-id="com.app:id/hidden" class="android.widget.Button" content-desc="Hidden" bounds="[10,90][210,150]" visible-to-user="false" />
    <node index="3" text="" resource-id="com.app:id/input" class="android.widget.EditText" content-desc="Search &amp; go" bounds="[10,160][210,220]" visible-to-user="true" focused="true">
      <node index="0" text="typed" resource-id="" class="android.widget.TextView" content-desc="" bounds="[12,162][208,218]" visible-to-user="true" />
    </node>
  </node>
</hierarchy>"#;

    // ---- 手写 XML 解析结构 ----

    #[test]
    fn parse_xml_builds_nested_tree() {
        let root = parse_uiautomator_xml(UIA_XML_FIXTURE).unwrap();
        assert_eq!(root.tag, "hierarchy");
        assert_eq!(root.children.len(), 1, "hierarchy 下单 window 节点");
        let frame = &root.children[0];
        assert_eq!(frame.tag, "node");
        assert_eq!(frame.attr("class"), Some("android.widget.FrameLayout"));
        assert_eq!(frame.children.len(), 4, "title/ghost/hidden/input 四子（解析不过滤）");
        // input（末子）自身带一个 typed 子节点。
        assert_eq!(frame.children[3].children.len(), 1);
    }

    #[test]
    fn parse_xml_rejects_malformed() {
        assert!(parse_uiautomator_xml("<node>").is_err(), "未闭合标签");
        assert!(parse_uiautomator_xml("no tags at all").is_err(), "空文档");
        assert!(parse_uiautomator_xml("</node>").is_err(), "孤立结束标签");
    }

    // ---- 幽灵过滤 + 统一形状映射（Locked-8）----

    #[test]
    fn a11y_tree_filters_ghosts_and_maps_shape() {
        let root = parse_uiautomator_xml(UIA_XML_FIXTURE).unwrap();
        let tree = build_a11y_tree(&root, &A11yParams::default());
        assert!(!tree.truncated, "depth 3 足够容纳全树");
        assert_eq!(tree.nodes.len(), 1, "顶层 = hierarchy 直接子（window）");
        let frame = &tree.nodes[0];
        assert_eq!(frame.role, "android.widget.FrameLayout", "class→role");
        assert_eq!(
            frame.children.len(),
            2,
            "零面积 ghost + invisible hidden 均被过滤，仅 title/input 存活"
        );
        let title = &frame.children[0];
        assert_eq!(title.role, "android.widget.TextView");
        assert_eq!(title.name, "Hello", "text→name");
        assert_eq!(title.value.as_deref(), Some("com.app:id/title"), "resource-id→value");
        assert_eq!(title.bounds, Some([10, 20, 200, 60]), "[l,t][r,b]→[x,y,w,h]");
        let input = &frame.children[1];
        assert_eq!(input.name, "Search & go", "text 空回退 content-desc + 实体解码");
        assert_eq!(input.value.as_deref(), Some("com.app:id/input"));
        assert_eq!(input.children.len(), 1);
        assert_eq!(input.children[0].name, "typed");
    }

    // ---- depth / max_nodes 截断标志（truncated 语义保持）----

    #[test]
    fn a11y_tree_depth_zero_truncates() {
        let root = parse_uiautomator_xml(UIA_XML_FIXTURE).unwrap();
        let params = A11yParams {
            depth: 0,
            ..A11yParams::default()
        };
        let tree = build_a11y_tree(&root, &params);
        assert!(tree.truncated, "depth 0 但 window 有子 → 截断");
        assert_eq!(tree.nodes.len(), 1);
        assert!(tree.nodes[0].children.is_empty(), "depth 0 不含后代");
    }

    #[test]
    fn a11y_tree_max_nodes_truncates() {
        let root = parse_uiautomator_xml(UIA_XML_FIXTURE).unwrap();
        let params = A11yParams {
            max_nodes: 2,
            ..A11yParams::default()
        };
        let tree = build_a11y_tree(&root, &params);
        assert!(tree.truncated, "预算 2 < 存活节点数 → 截断");
    }

    // ---- role 剪枝（保留匹配节点 + 祖先路径）----

    #[test]
    fn a11y_tree_prunes_by_role() {
        let root = parse_uiautomator_xml(UIA_XML_FIXTURE).unwrap();
        let params = A11yParams {
            role: Some("android.widget.EditText".to_string()),
            ..A11yParams::default()
        };
        let tree = build_a11y_tree(&root, &params);
        assert_eq!(tree.nodes.len(), 1, "FrameLayout 作 EditText 祖先保留");
        let frame = &tree.nodes[0];
        assert_eq!(frame.children.len(), 1, "非匹配的 title 旁支剪除");
        assert_eq!(frame.children[0].role, "android.widget.EditText");
    }

    // ---- root="focus" 选焦点子树 ----

    #[test]
    fn a11y_tree_focus_selects_focused_subtree() {
        let root = parse_uiautomator_xml(UIA_XML_FIXTURE).unwrap();
        let params = A11yParams {
            root: Some("focus".to_string()),
            ..A11yParams::default()
        };
        let tree = build_a11y_tree(&root, &params);
        assert_eq!(tree.nodes.len(), 1, "焦点=input，其直接子=typed");
        assert_eq!(tree.nodes[0].name, "typed");
    }

    // ---- bounds 解析（[l,t][r,b]→[x,y,w,h]，零/负面积=幽灵）----

    #[test]
    fn parse_bounds_maps_and_rejects_ghosts() {
        assert_eq!(parse_bounds("[10,20][210,80]"), Some([10, 20, 200, 60]));
        assert_eq!(parse_bounds("[0,0][720,1280]"), Some([0, 0, 720, 1280]));
        assert_eq!(parse_bounds("[0,0][0,0]"), None, "零面积");
        assert_eq!(parse_bounds("[5,5][3,10]"), None, "负宽");
        assert_eq!(parse_bounds("garbage"), None);
    }

    // ---- XML 实体解码 ----

    #[test]
    fn xml_unescape_decodes_entities() {
        assert_eq!(xml_unescape("a &amp; b"), "a & b");
        assert_eq!(xml_unescape("&lt;x&gt;"), "<x>");
        assert_eq!(xml_unescape("&quot;q&quot;"), "\"q\"");
        assert_eq!(xml_unescape("&#65;&#x42;"), "AB", "数字实体");
        assert_eq!(xml_unescape("plain text"), "plain text");
    }

    // ---- ps -A 解析（按表头列名定位，cpu 缺置 0）----

    const PS_FIXTURE: &str = "USER            PID   PPID     VSZ    RSS WCHAN            ADDR S NAME
root              1      0   10943   2712 0                0   S init
u0_a123        4567    321 1234567  55000 0                0   S com.app.foo
garbage row without a numeric pid col
";

    #[test]
    fn parse_ps_extracts_pid_name_rss() {
        let procs = parse_ps(PS_FIXTURE);
        assert_eq!(procs.len(), 2, "无法解析 pid 的行跳过");
        assert_eq!(procs[0].pid, 1);
        assert_eq!(procs[0].name, "init");
        assert_eq!(procs[0].memory_bytes, 2712 * 1024, "RSS(KB)×1024");
        assert_eq!(procs[0].cpu, 0.0, "ps -A 无 CPU 列 → 0");
        assert_eq!(procs[1].pid, 4567);
        assert_eq!(procs[1].name, "com.app.foo");
        assert_eq!(procs[1].memory_bytes, 55000 * 1024);
    }

    #[test]
    fn parse_ps_empty_is_empty() {
        assert!(parse_ps("").is_empty());
    }

    // ---- process/file remap argv 组装（设备语义 D4 / /sdcard Locked-6）----

    #[test]
    fn uiautomator_dump_argv_writes_device_file() {
        // 设备路径唯一化（GAP-2）：aura-ui- 前缀 + .xml 后缀，argv 原样承载该唯一路径。
        let p = ui_dump_device_path();
        assert!(
            p.starts_with("/sdcard/aura-ui-") && p.ends_with(".xml"),
            "唯一化设备路径: {p}"
        );
        assert_eq!(
            uiautomator_dump_argv(&p),
            sv(&["shell", "uiautomator", "dump", p.as_str()])
        );
    }

    #[test]
    fn ui_dump_device_path_is_unique_per_call() {
        // 两次调用路径不同（含唯一后缀），避免并发 dump 互踩固定路径（GAP-2）。
        let a = ui_dump_device_path();
        let b = ui_dump_device_path();
        assert!(a.starts_with("/sdcard/aura-ui-") && a.ends_with(".xml"));
        assert_ne!(a, b, "每次 dump 路径唯一: {a} vs {b}");
    }

    #[test]
    fn rm_argv_builds_shell_rm_f() {
        // 设备侧清理 argv（GAP-2）：adb shell rm -f <path>。
        assert_eq!(
            rm_argv("/sdcard/aura-ui-1-2-3.xml"),
            sv(&["shell", "rm", "-f", "/sdcard/aura-ui-1-2-3.xml"])
        );
    }

    #[test]
    fn pull_push_argv_map_to_sdcard() {
        assert_eq!(
            pull_argv("/sdcard/x.xml", "/tmp/y"),
            sv(&["pull", "/sdcard/x.xml", "/tmp/y"])
        );
        assert_eq!(
            push_argv("/tmp/y", "/sdcard/x.bin"),
            sv(&["push", "/tmp/y", "/sdcard/x.bin"])
        );
    }

    #[test]
    fn ps_and_kill_argv_build() {
        assert_eq!(ps_argv(), sv(&["shell", "ps", "-A"]));
        assert_eq!(kill_argv(4567), sv(&["shell", "kill", "4567"]));
    }

    #[test]
    fn run_command_argv_device_shell_semantics() {
        // 无 cwd：adb shell <cmd> <args...>。
        assert_eq!(
            run_command_argv("ls", &sv(&["-la", "/data"]), None),
            sv(&["shell", "ls", "-la", "/data"])
        );
        // 有 cwd：cd <cwd> && 前缀注入。
        assert_eq!(
            run_command_argv("ls", &sv(&["-la"]), Some("/data/local/tmp")),
            sv(&["shell", "cd", "/data/local/tmp", "&&", "ls", "-la"])
        );
        // 空 cwd 视同无 cwd（不注入空 cd）。
        assert_eq!(
            run_command_argv("id", &[], Some("")),
            sv(&["shell", "id"])
        );
    }
}

// ===== TASK-005：21 方法 triage 契约测试矩阵（6 direct / 13 remap / 2 unsupported）=====

/// 21 方法 triage 契约矩阵：按 driver 契约三分类逐方法断言其行为，纯函数 / argv / 早返回 Unsupported
/// 层面验证（不依赖真 adb、无 MockDriver），作为 gate E2E（TASK-006）前的 driver 契约级验收。
///
///   - direct（6）：screenshot / zoom / type / drag / wait / assert —— Android 原生等价操作，
///     argv 或行为与桌面契约语义等价（截图 raw 帧、swipe 手势、本地 sleep、平台无关 assert 组合）。
///   - remap（13）：list_displays / switch_display / click / key / scroll / list_processes /
///     kill_process / file_push / file_pull / run_command / get_a11y_tree / start_recording /
///     stop_recording —— 桌面语义映射为 adb argv 向量或等价输出形状（wm size / input / ps / kill /
///     push / pull / uiautomator dump / 设备侧 screenrecord + adb pull 桥接）。
///   - unsupported（2）：move_mouse（clean：触屏无 hover 指针语义，永久拒绝）+ audio_inject
///     （M11 clean：无宿主侧虚拟音频注入通路）。
///
/// triage 计数 6/13/2 与 Locked-6 原定『6 direct / 13 remap / 1 unsupported』的差异来由：M11 工具面
/// 扩容 20→21，新工具 audio_inject 判 clean-unsupported（unsupported 1→2、remap 13 不变）。record
/// 双方法（start/stop_recording）以设备侧 screenrecord + adb pull 桥接实装，归 remap（与 Locked-6 原判一致）。
///
/// 集合相等（不破 Locked-6）：矩阵覆盖的 21 方法名与 `aura_capability::TOOL_NAMES` canonical 集合
/// 相等（GAP-7 单源下沉：真源 = aura-node `tool_registry!` 注册表，canonical 经 aura-node 侧
/// `canonical_tool_names_in_capability_equal_registry` 断言与之钉死，本侧编译期直引 canonical，
/// 旧 cfg(test) 字面镜像已删）；MCP 面一致性另由 `mcp_router_names_equal_tool_names_registry` 守卫。
#[cfg(test)]
mod contract_matrix {
    use super::*;
    use std::collections::HashSet;

    use aura_capability::{A11yField, AssertMode, AssertParams, DiffMethod, MatchType, TOOL_NAMES};

    /// 方法 triage 分类（driver 契约三分）。
    #[derive(Debug, Clone, Copy, PartialEq, Eq)]
    enum Triage {
        /// 语义等价：Android 原生等价操作，argv/行为与桌面契约一致。
        Direct,
        /// 映射：桌面语义 remap 为 Android adb argv 向量或等价输出形状。
        Remap,
        /// 不支持：clean（永久无等价）或 deferred（record 域 defer M8）。
        Unsupported,
    }

    /// 21 方法 triage 表：对外工具名（顺序对照 `aura_capability::TOOL_NAMES` canonical）→ 分类。
    const TRIAGE_MATRIX: &[(&str, Triage)] = &[
        ("screenshot", Triage::Direct),
        ("zoom", Triage::Direct),
        ("list_displays", Triage::Remap),
        ("switch_display", Triage::Remap),
        ("click", Triage::Remap),
        ("type", Triage::Direct),
        ("key", Triage::Remap),
        ("scroll", Triage::Remap),
        ("drag", Triage::Direct),
        ("move_mouse", Triage::Unsupported),
        ("wait", Triage::Direct),
        ("list_processes", Triage::Remap),
        ("kill_process", Triage::Remap),
        ("file_push", Triage::Remap),
        ("file_pull", Triage::Remap),
        ("run_command", Triage::Remap),
        ("get_a11y_tree", Triage::Remap),
        ("assert", Triage::Direct),
        ("start_recording", Triage::Remap),
        ("stop_recording", Triage::Remap),
        ("audio_inject", Triage::Unsupported),
    ];

    /// 构造被测 driver（serial 任意；矩阵不发起真 adb 调用，direct/remap 走纯函数、unsupported 走早返回）。
    fn drv() -> AndroidDriver {
        AndroidDriver::new("matrix-serial:5555".to_string())
    }

    /// `&[&str]` → `Vec<String>`，便于与 argv builder 结果断言。
    fn sv(parts: &[&str]) -> Vec<String> {
        parts.iter().map(|s| s.to_string()).collect()
    }

    /// 合成 screencap raw 帧（`[w][h][format]( [colorspace] )` + w*h*4 像素尾），供 direct 截图链断言。
    fn synth_screencap(w: u32, h: u32, header_len: usize) -> Vec<u8> {
        let mut v = Vec::new();
        v.extend_from_slice(&w.to_le_bytes());
        v.extend_from_slice(&h.to_le_bytes());
        v.extend_from_slice(&0u32.to_le_bytes());
        if header_len == 16 {
            v.extend_from_slice(&0u32.to_le_bytes());
        }
        v.extend((0..(w as usize * h as usize * 4)).map(|i| (i % 256) as u8));
        v
    }

    /// 构造 text 形态 AssertParams（其余形态专属字段取缺省，text 形态忽略）。
    fn text_assert(actual: &str, expect: &str, present: bool) -> AssertParams {
        AssertParams {
            mode: AssertMode::Text,
            expect: expect.to_string(),
            match_type: MatchType::Contains,
            present,
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

    /// 小型 uiautomator dump fixture：FrameLayout > TextView("Hello")，供 get_a11y_tree/assert 组合断言。
    const UIA_XML: &str = r#"<?xml version='1.0' encoding='UTF-8'?>
<hierarchy rotation="0">
  <node class="android.widget.FrameLayout" text="" content-desc="" resource-id="" bounds="[0,0][720,1280]" visible-to-user="true">
    <node class="android.widget.TextView" text="Hello" content-desc="" resource-id="com.app:id/title" bounds="[10,20][210,80]" visible-to-user="true" />
  </node>
</hierarchy>"#;

    // ---------- 矩阵完整性守卫：集合相等 + triage 分区计数 ----------

    /// 集合相等（不破 Locked-6）：矩阵 20 方法名与 canonical `aura_capability::TOOL_NAMES` 集合相等，
    /// 不缺不多、无重复（GAP-7：canonical 与 tool_registry! 真源由 aura-node 侧断言钉死，此处漂移即红）。
    #[test]
    fn matrix_covers_exactly_tool_names_set_equal() {
        let matrix: HashSet<&str> = TRIAGE_MATRIX.iter().map(|(n, _)| *n).collect();
        let tool_names: HashSet<&str> = TOOL_NAMES.iter().copied().collect();
        assert_eq!(TRIAGE_MATRIX.len(), 21, "矩阵 21 条目");
        assert_eq!(matrix.len(), 21, "21 方法名无重复");
        assert_eq!(matrix, tool_names, "矩阵方法集必须与 TOOL_NAMES 集合相等（不缺不多）");
    }

    /// triage 分区计数：6 direct / 13 remap / 2 unsupported，总和 21。record 双方法实装后归 remap
    /// （设备侧 screenrecord + adb pull ≠ 桌面 WGC 进程内落盘，属 remap 语义），unsupported 仅剩
    /// move_mouse（触屏无 hover clean）+ M11 audio_inject（无宿主音频注入通路 clean）。
    #[test]
    fn triage_partition_counts_6_direct_13_remap_2_unsupported() {
        let n = |t: Triage| TRIAGE_MATRIX.iter().filter(|(_, x)| *x == t).count();
        let (direct, remap, unsupported) =
            (n(Triage::Direct), n(Triage::Remap), n(Triage::Unsupported));
        assert_eq!(
            (direct, remap, unsupported),
            (6, 13, 2),
            "6 direct / 13 remap / 2 unsupported（record 双方法实装归 remap + move_mouse/audio_inject clean）"
        );
        assert_eq!(direct + remap + unsupported, 21);
    }

    // ---------- direct（6）：语义等价 argv / 行为 ----------

    /// screenshot（direct）：`adb exec-out screencap` raw 帧 → header 反推 → 像素尾 = w*h*4。
    #[test]
    fn direct_screenshot_semantics() {
        assert_eq!(screencap_argv(), sv(&["exec-out", "screencap"]));
        let frame = synth_screencap(4, 5, 16);
        let (w, h, hs) = parse_screencap_header(&frame).unwrap();
        assert_eq!((w, h, hs), (4, 5, 16));
        assert_eq!(frame.len() - hs, (4 * 5 * 4) as usize, "像素尾 = w*h*4");
    }

    /// zoom（direct）：screencap 全屏帧 → region 两角转 xywh → 内存裁剪（截图的区域裁剪语义等价）。
    #[test]
    fn direct_zoom_semantics() {
        assert_eq!(screencap_argv(), sv(&["exec-out", "screencap"]));
        let frame = synth_screencap(4, 4, 16);
        let (iw, ih, hs) = parse_screencap_header(&frame).unwrap();
        let pixels = frame[hs..].to_vec();
        let (x, y, w, h) =
            crate::screen::region_to_xywh(Region { x1: 0, y1: 0, x2: 2, y2: 2 }).unwrap();
        let cropped = crate::screen::crop_rgba(&pixels, iw, ih, x, y, w, h).unwrap();
        assert_eq!(cropped.len(), (2 * 2 * 4) as usize, "裁剪区 = w*h*4");
    }

    /// type（direct）：文本经 `adb shell input text`（空格→%s + shell 元字符转义）；空文本短路成功。
    #[tokio::test]
    async fn direct_type_text_semantics() {
        assert_eq!(text_argv("a b"), sv(&["shell", "input", "text", "a%sb"]));
        // 空文本无操作即成功（不 spawn adb，构建机无设备亦绿）。
        assert!(drv().type_text(String::new()).await.is_ok());
    }

    /// drag（direct）：拖拽 = 持续 DRAG_DURATION_MS 的 `input swipe`（触屏拖拽手势等价）。
    #[test]
    fn direct_drag_semantics() {
        assert_eq!(
            swipe_argv(10, 20, 30, 40, DRAG_DURATION_MS),
            sv(&["shell", "input", "swipe", "10", "20", "30", "40", "300"])
        );
    }

    /// wait（direct）：平台无关本地 sleep，不经 adb → 恒成功。
    #[tokio::test]
    async fn direct_wait_semantics() {
        assert!(drv().wait(0).await.is_ok());
    }

    /// assert（direct）：平台无关组合能力（空 `impl AssertDriver` 默认方法即全部实现）。text 纯逻辑；
    /// a11y 复用 get_a11y_tree（其纯核 build_a11y_tree 对 fixture 树产出 assert 消费的统一形状）。
    #[tokio::test]
    async fn direct_assert_composition_text_and_a11y() {
        let d = drv();
        // text 形态：经 AssertDriver 默认方法组合直接可用，命中/未命中两路径均 Ok（失败即数据 Locked-7）。
        let pass = d
            .assert(text_assert("deploy succeeded", "succeeded", true))
            .await
            .unwrap();
        assert!(pass.passed, "assert(text) 命中 → passed=true");
        let fail = d
            .assert(text_assert("deploy succeeded", "denied", true))
            .await
            .unwrap();
        assert!(!fail.passed, "assert(text) 未命中 → passed=false");
        // a11y 形态复用 get_a11y_tree：其纯核对 fixture 树产出 assert 谓词消费的统一 A11yNode 形状。
        let tree = build_a11y_tree(&parse_uiautomator_xml(UIA_XML).unwrap(), &A11yParams::default());
        assert_eq!(
            tree.nodes[0].children[0].name, "Hello",
            "assert(a11y) 将在 get_a11y_tree 产出的树上匹配节点"
        );
    }

    // ---------- remap（11）：adb argv 向量 / remap 输出形状 ----------

    /// list_displays（remap）：`adb shell wm size` → 单元素 DisplayInfo（解析 WxH，Override 优先）。
    #[test]
    fn remap_list_displays_wm_size() {
        assert_eq!(parse_wm_size("Physical size: 720x1280\n").unwrap(), (720, 1280));
        assert_eq!(
            parse_wm_size("Physical size: 1080x2400\nOverride size: 720x1600\n").unwrap(),
            (720, 1600)
        );
    }

    /// switch_display（remap）：display 0 → 唯一显示（委派 list_displays）；非 0 → Unsupported（无多显示切换）。
    #[tokio::test]
    async fn remap_switch_display_single_display() {
        let err = drv().switch_display(1).await.unwrap_err();
        assert_eq!(err.code(), "E_UNSUPPORTED", "非 0 display → 无多显示切换语义");
    }

    /// click（remap）：Left → `input tap`；Right → 同点长按 `input swipe` 600ms（Middle 分支见 branch_ 测试）。
    #[test]
    fn remap_click_left_to_tap() {
        assert_eq!(tap_argv(120, 340), sv(&["shell", "input", "tap", "120", "340"]));
        assert_eq!(
            swipe_argv(120, 340, 120, 340, LONG_PRESS_MS),
            sv(&["shell", "input", "swipe", "120", "340", "120", "340", "600"]),
            "Right → 同点长按 600ms"
        );
    }

    /// key（remap）：命名键 → KEYCODE 数字码 → `input keyevent`（chord/未知键分支见 branch_ 测试）。
    #[test]
    fn remap_key_named_to_keycode() {
        assert_eq!(keycode_of("enter"), Some(66));
        assert_eq!(keyevent_argv(66), sv(&["shell", "input", "keyevent", "66"]));
    }

    /// scroll（remap）：以 anchor 为起点沿「反向量」swipe（Down=上滑露出下方内容）。断言其委派的 swipe 形状。
    #[test]
    fn remap_scroll_reverse_vector_swipe() {
        // anchor(200,400) + amount 3 → distance = 3×SCROLL_STEP_PX = 300；Down → dy=-300 → 终点 (200,100)。
        assert_eq!(
            swipe_argv(200, 400, 200, 400 - 3 * SCROLL_STEP_PX, SCROLL_DURATION_MS),
            sv(&["shell", "input", "swipe", "200", "400", "200", "100", "300"])
        );
    }

    /// list_processes（remap）：`adb shell ps -A` → 按表头列名解析 ProcessInfo（RSS×1024，cpu 缺置 0）。
    #[test]
    fn remap_list_processes_ps() {
        assert_eq!(ps_argv(), sv(&["shell", "ps", "-A"]));
        let procs = parse_ps("USER PID PPID VSZ RSS WCHAN ADDR S NAME\nroot 1 0 100 2712 0 0 S init\n");
        assert_eq!(procs.len(), 1);
        assert_eq!((procs[0].pid, procs[0].name.as_str()), (1, "init"));
        assert_eq!(procs[0].memory_bytes, 2712 * 1024, "RSS(KB)×1024");
    }

    /// kill_process（remap）：`adb shell kill <pid>`。
    #[test]
    fn remap_kill_process() {
        assert_eq!(kill_argv(4567), sv(&["shell", "kill", "4567"]));
    }

    /// file_push（remap）：宿主暂存 → `adb push <local> <remote>`（→ /sdcard）。
    #[test]
    fn remap_file_push() {
        assert_eq!(
            push_argv("/tmp/y", "/sdcard/x.bin"),
            sv(&["push", "/tmp/y", "/sdcard/x.bin"])
        );
    }

    /// file_pull（remap）：`adb pull <remote> <local>` → 宿主暂存 → base64 回带。
    #[test]
    fn remap_file_pull() {
        assert_eq!(
            pull_argv("/sdcard/x.xml", "/tmp/y"),
            sv(&["pull", "/sdcard/x.xml", "/tmp/y"])
        );
    }

    /// run_command（remap）：设备语义 `adb shell [cd <cwd> &&] <cmd> <args...>`（D4，命令跑在设备而非宿主）。
    #[test]
    fn remap_run_command_device_shell() {
        assert_eq!(
            run_command_argv("ls", &sv(&["-la"]), None),
            sv(&["shell", "ls", "-la"])
        );
        assert_eq!(
            run_command_argv("ls", &sv(&["-la"]), Some("/data/local/tmp")),
            sv(&["shell", "cd", "/data/local/tmp", "&&", "ls", "-la"])
        );
    }

    /// get_a11y_tree（remap）：`uiautomator dump` 写设备文件 → `adb pull` → 手写解析 → A11yTree 统一形状。
    #[test]
    fn remap_get_a11y_tree_dump_pull_parse() {
        // GAP-2：设备路径唯一化（aura-ui- 前缀），dump/pull argv 原样承载唯一路径。
        let dev = ui_dump_device_path();
        assert!(dev.starts_with("/sdcard/aura-ui-") && dev.ends_with(".xml"));
        assert_eq!(
            uiautomator_dump_argv(&dev),
            sv(&["shell", "uiautomator", "dump", dev.as_str()])
        );
        assert_eq!(pull_argv(&dev, "/tmp/d")[0], "pull");
        let tree = build_a11y_tree(&parse_uiautomator_xml(UIA_XML).unwrap(), &A11yParams::default());
        assert_eq!(tree.nodes[0].role, "android.widget.FrameLayout", "class→role");
        assert_eq!(tree.nodes[0].children[0].name, "Hello", "text→name");
    }

    // ---------- unsupported（2）：move_mouse / audio_inject clean（record 双方法已实装，见下 record 段）----------

    /// move_mouse → CapError::Unsupported（clean unsupported：触屏无 hover 指针语义，永久拒绝）。
    #[tokio::test]
    async fn unsupported_move_mouse_clean() {
        let err = drv().move_mouse(Coordinate { x: 1, y: 2 }).await.unwrap_err();
        assert_eq!(err.code(), "E_UNSUPPORTED");
    }

    // ---------- record（remap，已实装）：纯逻辑 + 无 adb 早返回面（真采集经 adb screenrecord，gate E2E 覆盖）----------

    /// 时长封顶纯函数：缺省 30s，clamp 到 [1, 180]。
    #[test]
    fn rec_duration_clamps() {
        let mk = |d: Option<u64>| RecordParams { fps: None, max_duration_secs: d, display: None };
        assert_eq!(clamp_rec_duration(&mk(None)), REC_DEFAULT_DURATION_SECS);
        assert_eq!(clamp_rec_duration(&mk(Some(0))), 1, "下限 1");
        assert_eq!(clamp_rec_duration(&mk(Some(9999))), REC_MAX_DURATION_SECS, "上限封顶 180");
        assert_eq!(clamp_rec_duration(&mk(Some(60))), 60);
    }

    /// 设备落盘路径纯函数：rec_id 唯一映射，非法字符剔除防路径注入，空 id 兜底非空。
    #[test]
    fn rec_device_path_sanitizes() {
        assert_eq!(rec_device_path("abc-123"), "/sdcard/aura-rec-abc-123.mp4");
        assert_eq!(rec_device_path("../../etc/x"), "/sdcard/aura-rec-etcx.mp4", "剔 / 与 .");
        assert_eq!(rec_device_path(""), "/sdcard/aura-rec-rec.mp4", "空 id 兜底非空");
    }

    /// stop_recording 无活动会话 → E_INVALID_ARG（会话表判定先于任何 adb 调用，无需真设备）。
    #[tokio::test]
    async fn stop_recording_missing_session_invalid_arg() {
        let err = drv().stop_recording("never-started".to_string()).await.unwrap_err();
        assert_eq!(err.code(), "E_INVALID_ARG");
    }

    /// audio_inject → CapError::Unsupported（M11 clean：无宿主侧虚拟音频注入通路，对齐矩阵语义）。
    #[tokio::test]
    async fn unsupported_audio_inject_clean() {
        let err = drv()
            .audio_inject(AudioInjectParams { wav_base64: Some("QQ==".to_string()), file: None })
            .await
            .unwrap_err();
        assert_eq!(err.code(), "E_UNSUPPORTED");
    }

    // ---------- 分支级 unsupported 附断言：click Middle / key 修饰组合 ----------

    /// click Middle → CapError::Unsupported（分支级：Android input 无中键语义）。
    #[tokio::test]
    async fn branch_click_middle_unsupported() {
        let err = drv()
            .click(Coordinate { x: 1, y: 2 }, MouseButton::Middle)
            .await
            .unwrap_err();
        assert_eq!(err.code(), "E_UNSUPPORTED");
    }

    /// key ctrl+c 修饰组合 → CapError::Unsupported（分支级：无单 keyevent 表达修饰组合键）。
    #[tokio::test]
    async fn branch_key_chord_unsupported() {
        let err = drv().key("ctrl+c".to_string()).await.unwrap_err();
        assert_eq!(err.code(), "E_UNSUPPORTED");
    }
}
