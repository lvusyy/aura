//! 节点工具单一注册表（feature grpc 门控）：MCP 与 gRPC 反连两侧工具集的单一真源。
//!
//! 背景：M2 时 gRPC 反连侧手写 `TOOL_NAMES: [&str;16]` + `dispatch_tool` 的 16 臂 match，
//! 与 MCP `#[tool]` 宏注册重复；MCP-only 新增工具会在反连侧静默 `E_UNSUPPORTED`。
//! 本模块把「工具名清单」与「gRPC 派发」收敛为单一 [`tool_registry!`] 注册表：
//! [`TOOL_NAMES`]（Register 上报 + 集合相等断言共用）与 [`dispatch`] 的 match 分支由同一份
//! 注册表一次展开，二者永不漂移。
//!
//! 新增工具（后续 a11y/assert/recording）：写 1 个 `dispatch_*` 自由函数 + 在 [`tool_registry!`]
//! 注册表加 1 条 `"name" => dispatch_fn`，gRPC 派发与 Register 清单即同步生效（单编辑点）。
//!
//! 单一源守卫：`tests` 内断言 MCP 侧 `ToolRouter`（经 list_tools/list_all 暴露）的工具名集合
//! 与 [`TOOL_NAMES`] 集合相等，任一侧漏配都在编译期测试失败（替代脆弱的 len==16 计数）。
//!
//! 哑管道：本层仅搬运 JSON 信封字节（json_args in → Envelope JSON out），工具语义契约由 MCP
//! JSON schema 单一承载，此处复用 MCP 侧同一执行核（guard + driver + to_native + scale/display 状态）。

use std::sync::atomic::Ordering;

use serde::de::DeserializeOwned;
use serde::Serialize;

use aura_capability::{
    A11yParams, AssertParams, AudioInjectParams, Envelope, ErrObj, MouseButton, RecordParams,
    Region, ScrollDirection,
};

use super::input_tools::{
    ClickParams, DragParams, KeyParams, MoveMouseParams, ScrollParams, TypeTextParams, WaitParams,
};
use super::proc_tools::{FilePullParams, FilePushParams, KillProcessParams, RunCommandParams};
use super::record_tools::StopRecordingParams;
use super::screen_tools::{ScreenshotParams, SwitchDisplayParams, ZoomParams};
use super::AuraTools;

/// 契约版本单点真源经父模块 [`super::CONTRACT_VERSION`] 再导出，与 [`TOOL_NAMES`] 契约面共址。
/// 定义置于**非门控**父模块 `transport/mod.rs`（而非本 grpc 门控模块）：`get_info` override 恒编译
/// （MCP 无 grpc 亦在），常量若随本模块门控则 default（无 grpc）构建的 get_info 引用不到；故单点置于
/// 父模块，此处再导出供 gRPC 面（`grpc_reverse.rs` 的 `Register.contract_version` 上报）经单一
/// tool_dispatch 注册表引用（Locked-7 契约版本单点双面同源）。
pub(crate) use super::CONTRACT_VERSION;

/// 解析工具入参；失败即 `return` 预编码的 E_INVALID_ARG 信封字节（哑管道：错误也走 Envelope）。
macro_rules! parse_or_return {
    ($args:expr) => {
        match parse($args) {
            Ok(p) => p,
            Err(err_bytes) => return err_bytes,
        }
    };
}

/// 工具注册表：`"对外名" => dispatch_自由函数` 的单一清单。一次展开出 [`TOOL_NAMES`]（键 slice）
/// 与 [`dispatch`]（按名路由 match）——二者同源，永不漂移。未知工具在 dispatch 内回 E_UNSUPPORTED。
macro_rules! tool_registry {
    ( $( $name:literal => $handler:path ),+ $(,)? ) => {
        /// 节点支持的工具对外名（"type" 而非内部 type_text）。由 [`tool_registry!`] 从注册表键派生，
        /// 是 Register 工具清单与 gRPC 派发的单一真源（旧 grpc_reverse 独立 `[&str;16]` 硬编码已删）。
        pub(crate) const TOOL_NAMES: &[&str] = &[ $( $name ),+ ];

        /// gRPC 反连工具派发核：按对外名路由到分发自由函数（复用 MCP 侧同一执行核语义）。
        /// 未知工具回 E_UNSUPPORTED 信封（反连侧 fallthrough，与 MCP 侧无此 schema 对齐）。
        pub(crate) async fn dispatch(tools: &AuraTools, tool: &str, json_args: &[u8]) -> Vec<u8> {
            match tool {
                $( $name => $handler(tools, json_args).await, )+
                other => error_envelope("E_UNSUPPORTED", &format!("unknown tool: {other}")),
            }
        }
    };
}

// 单一注册表：新增工具在此加 1 条即 TOOL_NAMES 与 dispatch 同步（配套 1 个 dispatch_* 自由函数）。
tool_registry! {
    "screenshot"     => dispatch_screenshot,
    "zoom"           => dispatch_zoom,
    "list_displays"  => dispatch_list_displays,
    "switch_display" => dispatch_switch_display,
    "click"          => dispatch_click,
    "type"           => dispatch_type,
    "key"            => dispatch_key,
    "scroll"         => dispatch_scroll,
    "drag"           => dispatch_drag,
    "move_mouse"     => dispatch_move_mouse,
    "wait"           => dispatch_wait,
    "list_processes" => dispatch_list_processes,
    "kill_process"   => dispatch_kill_process,
    "file_push"      => dispatch_file_push,
    "file_pull"      => dispatch_file_pull,
    "run_command"    => dispatch_run_command,
    "get_a11y_tree"  => dispatch_get_a11y_tree,
    "assert"         => dispatch_assert,
    "start_recording" => dispatch_start_recording,
    "stop_recording"  => dispatch_stop_recording,
    "audio_inject"    => dispatch_audio_inject,
}

// ===== screen 域（4）=====

/// screenshot：截屏并记录本次 scale 供 input 坐标回映射（复刻 MCP screenshot 副作用）。
/// M11：quality/max_dim 经与 MCP 面同一 clamp 护栏透传 driver（两路参数语义一致）；
/// `p.output` 在 gRPC 面**显式忽略**（P0 约束【8】：image content block 是 MCP wire 概念，
/// 反连恒走 `encode(&env)` JSON 信封字节，哑管道契约零变——本文件禁止出现 output 分支逻辑）。
async fn dispatch_screenshot(tools: &AuraTools, json_args: &[u8]) -> Vec<u8> {
    let p: ScreenshotParams = parse_or_return!(json_args);
    let display = p
        .display
        .unwrap_or_else(|| tools.current_display.load(Ordering::Relaxed));
    // gRPC 面无 image 双份 base64，护栏恒取 json 档上限（image_mode=false）。
    let opts = super::screen_tools::clamp_opts(p.quality, p.max_dim, false);
    let env = tools.guard(tools.driver.screenshot(Some(display), opts)).await;
    // 成功后记录本次 display→native 缩放系数，供 input 域坐标回映射；失败信封无 data 时保持既有 scale。
    if let Some(result) = env.data.as_ref() {
        tools.set_scale(result.meta.scale);
    }
    encode(&env)
}

/// zoom：quality/max_dim 同 clamp 透传（缺省不降采样语义在 driver 层）；output 同上显式忽略。
async fn dispatch_zoom(tools: &AuraTools, json_args: &[u8]) -> Vec<u8> {
    let p: ZoomParams = parse_or_return!(json_args);
    let region = Region {
        x1: p.region[0],
        y1: p.region[1],
        x2: p.region[2],
        y2: p.region[3],
    };
    let opts = super::screen_tools::clamp_opts(p.quality, p.max_dim, false);
    encode(&tools.guard(tools.driver.zoom(region, opts)).await)
}

async fn dispatch_list_displays(tools: &AuraTools, _json_args: &[u8]) -> Vec<u8> {
    encode(&tools.guard(tools.driver.list_displays()).await)
}

/// switch_display：仅在 driver 校验通过时更新 current_display（复刻 MCP switch_display 副作用）。
async fn dispatch_switch_display(tools: &AuraTools, json_args: &[u8]) -> Vec<u8> {
    let p: SwitchDisplayParams = parse_or_return!(json_args);
    let env = tools.guard(tools.driver.switch_display(p.display)).await;
    if env.ok {
        tools.current_display.store(p.display, Ordering::Relaxed);
    }
    encode(&env)
}

// ===== input 域（7）=====

/// click：coordinate 经 to_native 按当前 scale 由 display 空间回映射到原生像素。
async fn dispatch_click(tools: &AuraTools, json_args: &[u8]) -> Vec<u8> {
    let p: ClickParams = parse_or_return!(json_args);
    let at = tools.to_native(p.coordinate[0], p.coordinate[1]);
    let button = match p.button.as_deref() {
        Some("right") => MouseButton::Right,
        Some("middle") => MouseButton::Middle,
        _ => MouseButton::Left,
    };
    encode(&tools.guard(tools.driver.click(at, button)).await)
}

async fn dispatch_type(tools: &AuraTools, json_args: &[u8]) -> Vec<u8> {
    let p: TypeTextParams = parse_or_return!(json_args);
    encode(&tools.guard(tools.driver.type_text(p.text)).await)
}

async fn dispatch_key(tools: &AuraTools, json_args: &[u8]) -> Vec<u8> {
    let p: KeyParams = parse_or_return!(json_args);
    encode(&tools.guard(tools.driver.key(p.keys)).await)
}

async fn dispatch_scroll(tools: &AuraTools, json_args: &[u8]) -> Vec<u8> {
    let p: ScrollParams = parse_or_return!(json_args);
    let at = tools.to_native(p.coordinate[0], p.coordinate[1]);
    let direction = match p.scroll_direction.as_deref() {
        Some("up") => ScrollDirection::Up,
        Some("left") => ScrollDirection::Left,
        Some("right") => ScrollDirection::Right,
        _ => ScrollDirection::Down,
    };
    let amount = p.scroll_amount.unwrap_or(3);
    encode(&tools.guard(tools.driver.scroll(at, direction, amount)).await)
}

async fn dispatch_drag(tools: &AuraTools, json_args: &[u8]) -> Vec<u8> {
    let p: DragParams = parse_or_return!(json_args);
    let from = tools.to_native(p.from[0], p.from[1]);
    let to = tools.to_native(p.to[0], p.to[1]);
    encode(&tools.guard(tools.driver.drag(from, to)).await)
}

async fn dispatch_move_mouse(tools: &AuraTools, json_args: &[u8]) -> Vec<u8> {
    let p: MoveMouseParams = parse_or_return!(json_args);
    let to = tools.to_native(p.coordinate[0], p.coordinate[1]);
    encode(&tools.guard(tools.driver.move_mouse(to)).await)
}

async fn dispatch_wait(tools: &AuraTools, json_args: &[u8]) -> Vec<u8> {
    let p: WaitParams = parse_or_return!(json_args);
    encode(&tools.guard(tools.driver.wait(p.duration_ms)).await)
}

// ===== proc 域（5）=====

async fn dispatch_list_processes(tools: &AuraTools, _json_args: &[u8]) -> Vec<u8> {
    encode(&tools.guard(tools.driver.list_processes()).await)
}

async fn dispatch_kill_process(tools: &AuraTools, json_args: &[u8]) -> Vec<u8> {
    let p: KillProcessParams = parse_or_return!(json_args);
    encode(&tools.guard(tools.driver.kill_process(p.pid)).await)
}

async fn dispatch_file_push(tools: &AuraTools, json_args: &[u8]) -> Vec<u8> {
    let p: FilePushParams = parse_or_return!(json_args);
    encode(
        &tools
            .guard(tools.driver.file_push(p.remote_path, p.content_base64))
            .await,
    )
}

async fn dispatch_file_pull(tools: &AuraTools, json_args: &[u8]) -> Vec<u8> {
    let p: FilePullParams = parse_or_return!(json_args);
    encode(&tools.guard(tools.driver.file_pull(p.remote_path)).await)
}

async fn dispatch_run_command(tools: &AuraTools, json_args: &[u8]) -> Vec<u8> {
    let p: RunCommandParams = parse_or_return!(json_args);
    encode(
        &tools
            .guard(tools.driver.run_command(p.cmd, p.args, p.timeout_ms, p.cwd))
            .await,
    )
}

// ===== a11y 域（1）=====

/// get_a11y_tree：读取目标窗口无障碍树。入参 A11yParams（capability 层单一源）经哑管道
/// JSON → Envelope<A11yTree> JSON；复用 MCP 侧同一执行核（guard + driver）。
async fn dispatch_get_a11y_tree(tools: &AuraTools, json_args: &[u8]) -> Vec<u8> {
    let p: A11yParams = parse_or_return!(json_args);
    encode(&tools.guard(tools.driver.get_a11y_tree(p)).await)
}

// ===== assert 域（1）=====

/// assert：对界面 / 文本状态做确定性断言。入参 AssertParams（capability 单一源）经哑管道
/// JSON → Envelope<AssertResult> JSON；复用 MCP 侧同一执行核（guard + driver.assert）。
/// Locked-7：断言不成立（passed:false）是正常数据，经 guard 走信封 ok:true 分支（不落错误码）。
/// M11 REC-4：先经 `assert_tools::resolve_baseline`（与 MCP 面**同一**预处理函数，plan D9 双路
/// 一致）处理 baseline save/引用（save 短路 / 读盘回填 / 直通三分支），再走 driver.assert。
async fn dispatch_assert(tools: &AuraTools, json_args: &[u8]) -> Vec<u8> {
    let mut p: AssertParams = parse_or_return!(json_args);
    let env = tools
        .guard(async {
            if let Some(saved) = super::assert_tools::resolve_baseline(tools, &mut p).await? {
                return Ok(saved);
            }
            tools.driver.assert(p).await
        })
        .await;
    encode(&env)
}

// ===== record 域（2）=====

/// start_recording：起持续录屏会话。复用 MCP 侧同一执行核（会话登记 + driver 委派 + resource_link）。
async fn dispatch_start_recording(tools: &AuraTools, json_args: &[u8]) -> Vec<u8> {
    let p: RecordParams = parse_or_return!(json_args);
    encode(&tools.start_recording_impl(p).await)
}

/// stop_recording：停会话 finalize 并回 resource_link。复用 MCP 侧同一执行核。
async fn dispatch_stop_recording(tools: &AuraTools, json_args: &[u8]) -> Vec<u8> {
    let p: StopRecordingParams = parse_or_return!(json_args);
    encode(&tools.stop_recording_impl(p.rec_id).await)
}

// ===== audio 域（1）=====

/// audio_inject：向设备（虚拟）音频输出注入 WAV（M11 REC-3）。复用 MCP 侧同一执行核（guard +
/// driver.audio_inject）；Win/mac desktop 广告面已剔（plan D6）但 dispatch 超集网不滤，
/// call-time 走 E_NO_AUDIO_DEV 结构化降级。
async fn dispatch_audio_inject(tools: &AuraTools, json_args: &[u8]) -> Vec<u8> {
    let p: AudioInjectParams = parse_or_return!(json_args);
    encode(&tools.guard(tools.driver.audio_inject(p)).await)
}

// ===== 信封编解码（哑管道）=====

/// 序列化 Envelope 为 JSON 字节（哑管道回传体）。序列化理论不失败，失败兜底 E_INTERNAL。
fn encode<T: Serialize>(env: &Envelope<T>) -> Vec<u8> {
    serde_json::to_vec(env)
        .unwrap_or_else(|_| error_envelope("E_INTERNAL", "failed to serialize envelope"))
}

/// 构造错误信封 JSON 字节（传输/派发层错误，非工具语义错误）。
/// 亦供 grpc_reverse 侧 deadline 超时（E_TIMEOUT）复用，故 `pub(crate)`。
pub(crate) fn error_envelope(code: &str, message: &str) -> Vec<u8> {
    let env: Envelope<serde_json::Value> = Envelope::error(ErrObj {
        code: code.to_string(),
        message: message.to_string(),
    });
    // 极端兜底：手写最小错误信封，避免递归。
    serde_json::to_vec(&env).unwrap_or_else(|_| {
        br#"{"ok":false,"error":{"code":"E_INTERNAL","message":"encode failure"}}"#.to_vec()
    })
}

/// 解析工具入参。空 json_args 按 `{}` 处理（带 `#[serde(default)]` 的入参可解析）；
/// 失败返回预编码的 E_INVALID_ARG 信封字节作为 `Err`。
fn parse<P: DeserializeOwned>(json_args: &[u8]) -> std::result::Result<P, Vec<u8>> {
    let bytes = if json_args.is_empty() {
        b"{}".as_slice()
    } else {
        json_args
    };
    serde_json::from_slice(bytes)
        .map_err(|e| error_envelope("E_INVALID_ARG", &format!("invalid tool args: {e}")))
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::collections::HashSet;

    /// 单一源守卫：MCP 侧 `ToolRouter`（经 list_tools/list_all 暴露）暴露的工具名集合
    /// 必须与 [`TOOL_NAMES`] 注册表集合相等。任一侧新增/漏配（MCP `#[tool]` 或注册表）
    /// 都破坏集合相等，本编译期测试即拦截，杜绝反连侧对 MCP-only 工具静默 E_UNSUPPORTED
    /// （替代 grpc_reverse 旧的脆弱 len==16 计数测试）。
    #[test]
    fn mcp_router_names_equal_tool_names_registry() {
        // 与 mod.rs `#[tool_handler]` 合并的七 sub-router 同构。
        let router = AuraTools::screen_router()
            + AuraTools::input_router()
            + AuraTools::proc_router()
            + AuraTools::a11y_router()
            + AuraTools::assert_router()
            + AuraTools::record_router()
            + AuraTools::audio_router();
        let mcp: HashSet<String> = router
            .list_all()
            .into_iter()
            .map(|t| t.name.to_string())
            .collect();
        let registry: HashSet<String> = TOOL_NAMES.iter().map(|s| s.to_string()).collect();
        assert_eq!(mcp, registry, "MCP ToolRouter tool set must equal TOOL_NAMES");
    }

    /// GAP-7 canonical 单源守卫：aura-capability 侧 `TOOL_NAMES` canonical 清单必须与本注册表
    /// （`tool_registry!` 真源）逐字一致。aura-platform android/ios 两侧 triage 矩阵已改锚 canonical
    /// （M9 前为 cfg(test) 字面镜像），本断言把「注册表 ↔ canonical」缝成编译期闭环——向注册表 /
    /// canonical / 矩阵任一侧增删改工具名，`cargo test --workspace` 必红（SC-5 机制强制）。
    #[test]
    fn canonical_tool_names_in_capability_equal_registry() {
        use std::collections::BTreeSet;
        let registry: BTreeSet<&str> = TOOL_NAMES.iter().copied().collect();
        let canonical: BTreeSet<&str> = aura_capability::TOOL_NAMES.iter().copied().collect();
        let only_registry: Vec<_> = registry.difference(&canonical).collect();
        let only_canonical: Vec<_> = canonical.difference(&registry).collect();
        assert!(
            only_registry.is_empty() && only_canonical.is_empty(),
            "registry ↔ canonical 漂移：仅注册表有 {only_registry:?}；仅 canonical 有 {only_canonical:?}"
        );
        // 集合相等之上再钉逐字顺序与长度（canonical 文档承诺与注册表同序，重复项亦由此拦截）。
        assert_eq!(
            TOOL_NAMES,
            aura_capability::TOOL_NAMES,
            "canonical 须与 tool_registry! 注册表逐字同序"
        );
    }

    /// 正交守卫（Locked-6 / D5 + M11 plan D6）：每个 driver 的**广告子集**（TOOL_NAMES 按
    /// `supports_tool` 过滤）必须 ⊆ TOOL_NAMES，且 `AndroidDriver` 广告面精确 = 19（21 −
    /// move_mouse/audio_inject；record 双方法已实装进广告面）、`PlatformDriver`（desktop）分平台：
    /// Linux = 全 21（audio_inject pactl 主线）、Windows/macOS = 20（剔 audio_inject，call-time
    /// E_NO_AUDIO_DEV）。与上方集合相等守卫**正交并存**：后者测静态注册表 MCP==gRPC 不受过滤影响
    /// （放宽会重开 M2 缺口，保持不动），本测覆盖过滤后的广告面与 Register.tools（grpc_reverse）/
    /// MCP list_tools（transport/mod.rs）三面同源谓词。
    #[test]
    fn advertised_subset_within_tool_names_and_android_is_19() {
        use aura_platform::{build_driver, DriverKind};
        let names: HashSet<&str> = TOOL_NAMES.iter().copied().collect();
        assert_eq!(TOOL_NAMES.len(), 21, "M11 工具面扩容：全集 = 21 工具");

        // desktop：分平台广告面（plan D6）——Linux 全集 21；Win/mac 剔 audio_inject 保 20。
        let desktop = build_driver(DriverKind::Desktop, None, None);
        let desktop_adv: Vec<&str> = TOOL_NAMES
            .iter()
            .copied()
            .filter(|&t| desktop.supports_tool(t))
            .collect();
        assert!(
            desktop_adv.iter().all(|t| names.contains(t)),
            "desktop advertised subset must be ⊆ TOOL_NAMES"
        );
        let expected_desktop = if cfg!(target_os = "linux") { 21 } else { 20 };
        assert_eq!(
            desktop_adv.len(),
            expected_desktop,
            "desktop 广告面分平台：linux 21 / win·mac 20（剔 audio_inject）"
        );
        assert_eq!(
            desktop.supports_tool("audio_inject"),
            cfg!(target_os = "linux"),
            "audio_inject 广告面仅 Linux desktop 开启（Win/mac call-time E_NO_AUDIO_DEV）"
        );

        // android：覆写剔 2 → 广告面 19，仍 ⊆ TOOL_NAMES（record 双方法已实装进广告面）。
        let android = build_driver(DriverKind::Android, Some("test-serial".to_string()), None);
        let android_adv: Vec<&str> = TOOL_NAMES
            .iter()
            .copied()
            .filter(|&t| android.supports_tool(t))
            .collect();
        assert!(
            android_adv.iter().all(|t| names.contains(t)),
            "android advertised subset must be ⊆ TOOL_NAMES"
        );
        assert_eq!(
            android_adv.len(),
            19,
            "android 广告面 = 21 − 2（move_mouse/audio_inject；start/stop_recording 已实装）"
        );
        // 被剔 2 项精确断言（防误剔其他工具名）：不在广告面、但仍在 TOOL_NAMES 超集（dispatch 网不动）。
        // start_recording/stop_recording 已实装 → 反向断言其**在**广告面（防回归再被误剔）。
        for excluded in ["move_mouse", "audio_inject"] {
            assert!(
                !android.supports_tool(excluded),
                "{excluded} 须被 AndroidDriver 广告面剔除"
            );
            assert!(
                names.contains(excluded),
                "{excluded} 仍在 TOOL_NAMES 超集（call-time E_UNSUPPORTED，非从注册表删除）"
            );
        }
        for advertised in ["start_recording", "stop_recording"] {
            assert!(
                android.supports_tool(advertised),
                "{advertised} 已实装（设备侧 screenrecord + adb pull），须进 AndroidDriver 广告面"
            );
        }
        // 补集精确 = 2（广告面 + 剔除集 = 全集，无第 3 项被误剔）。
        assert_eq!(TOOL_NAMES.len() - android_adv.len(), 2);
    }

    /// ios 广告面精确 = 12（21 − 9：move_mouse/list_processes/kill_process/file_push/file_pull/
    /// run_command/start_recording/stop_recording/audio_inject，§3.5 iOS 子集 + M11，SC-4）；
    /// 剔除集精确 = 9、补集 = 9。
    /// 与上方 `advertised_subset_within_tool_names_and_android_is_19` 同构（Locked-6 / D5 三面同源谓词），
    /// aura-platform 侧 `ios::contract_matrix::advertised_subset_is_exactly_12` 为 driver 自证对偶。
    #[test]
    fn advertised_subset_ios_is_12() {
        use aura_platform::{build_driver, DriverKind};
        let names: HashSet<&str> = TOOL_NAMES.iter().copied().collect();

        // ios：覆写剔 9 → 广告面 12，仍 ⊆ TOOL_NAMES（build_driver 传 udid + WDA 端点，与反连调用点同形）。
        let ios = build_driver(
            DriverKind::Ios,
            Some("sim-udid".to_string()),
            Some("http://127.0.0.1:8100".to_string()),
        );
        let ios_adv: Vec<&str> = TOOL_NAMES
            .iter()
            .copied()
            .filter(|&t| ios.supports_tool(t))
            .collect();
        assert!(
            ios_adv.iter().all(|t| names.contains(t)),
            "ios advertised subset must be ⊆ TOOL_NAMES"
        );
        assert_eq!(
            ios_adv.len(),
            12,
            "ios 广告面 = 21 − 9（move_mouse/list_processes/kill_process/file_push/file_pull/run_command/start_recording/stop_recording/audio_inject）"
        );
        // 被剔 9 项精确断言（防误剔其他工具名）：不在广告面、但仍在 TOOL_NAMES 超集（dispatch 网不动）。
        for excluded in [
            "move_mouse",
            "list_processes",
            "kill_process",
            "file_push",
            "file_pull",
            "run_command",
            "start_recording",
            "stop_recording",
            "audio_inject",
        ] {
            assert!(
                !ios.supports_tool(excluded),
                "{excluded} 须被 IosDriver 广告面剔除"
            );
            assert!(
                names.contains(excluded),
                "{excluded} 仍在 TOOL_NAMES 超集（call-time E_UNSUPPORTED，非从注册表删除）"
            );
        }
        // 补集精确 = 9（广告面 + 剔除集 = 全集，无第 10 项被误剔）。
        assert_eq!(TOOL_NAMES.len() - ios_adv.len(), 9);
    }

    /// 空 json_args 按 {} 处理：带默认值的入参可解析。
    #[test]
    fn parse_empty_args_defaults_to_object() {
        let p: ScreenshotParams = parse(b"").expect("empty args parse to default");
        assert!(p.display.is_none());
    }

    /// 缺必填字段 → E_INVALID_ARG 信封（不 panic）。
    #[test]
    fn parse_invalid_args_returns_error_envelope() {
        let err = parse::<ClickParams>(b"{}").unwrap_err();
        let v: serde_json::Value = serde_json::from_slice(&err).unwrap();
        assert_eq!(v["ok"], false);
        assert_eq!(v["error"]["code"], "E_INVALID_ARG");
    }

    /// error_envelope 产出合法 JSON，含 code/message。
    #[test]
    fn error_envelope_is_valid_json_with_code() {
        let bytes = error_envelope("E_TIMEOUT", "deadline exceeded");
        let v: serde_json::Value = serde_json::from_slice(&bytes).unwrap();
        assert_eq!(v["ok"], false);
        assert_eq!(v["error"]["code"], "E_TIMEOUT");
        assert_eq!(v["error"]["message"], "deadline exceeded");
    }

    /// TASK-008 判据5（端到端 gRPC 派发路径）：assert(text) 命中/未命中两路径均产出 `ok:true` 信封，
    /// `data.passed` 分别为 true/false（失败即数据，Locked-7）。text 形态纯逻辑，无需显示环境，
    /// 直接经 dispatch_assert → driver.assert → guard → Envelope 完整链路验证。
    #[tokio::test]
    async fn dispatch_assert_text_both_paths_return_ok_envelope() {
        let tools = AuraTools::new(aura_platform::build_driver(
            aura_platform::DriverKind::Desktop,
            None,
            None,
        ));

        // 命中 → passed=true，信封 ok:true
        let pass = dispatch_assert(
            &tools,
            br#"{"mode":"text","actual":"deploy succeeded","expect":"succeeded"}"#,
        )
        .await;
        let v: serde_json::Value = serde_json::from_slice(&pass).unwrap();
        assert_eq!(v["ok"], true, "pass path envelope must be ok:true");
        assert_eq!(v["data"]["passed"], true);

        // 未命中 → passed=false，信封仍 ok:true（Locked-7：断言失败即数据，不落错误码）
        let fail = dispatch_assert(
            &tools,
            br#"{"mode":"text","actual":"deploy succeeded","expect":"failed"}"#,
        )
        .await;
        let v: serde_json::Value = serde_json::from_slice(&fail).unwrap();
        assert_eq!(
            v["ok"], true,
            "fail path envelope must still be ok:true (failure is data)"
        );
        assert_eq!(v["data"]["passed"], false);
    }
}
