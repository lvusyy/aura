//! InputDriver 平台实现：以 enigo 模拟鼠标 / 键盘 / 滚动 / 拖拽。
//!
//! enigo 的 `Enigo` 为 `!Send`（Windows `SendInput` / macOS `CGEvent` 有线程亲和要求），
//! 不能跨 `.await` 或跨线程搬运。故此处启动一条长驻的**独占 OS 线程**持有 `Enigo`，
//! 异步方法经 tokio `mpsc` 投递命令、`oneshot` 收回执（见 ARCHITECTURE §3 决策 3）。
//! `PlatformDriver` 是单元结构体（lib.rs 定义，本任务不可加字段），故通道句柄置于模块级
//! `OnceLock` 单例，首次调用惰性启动线程，进程内全程唯一。

use std::sync::OnceLock;
use std::time::Duration;

use async_trait::async_trait;
use enigo::{
    Axis, Button, Coordinate as EnigoCoord, Direction, Enigo, Key, Keyboard, Mouse, Settings,
};
use tokio::sync::{mpsc, oneshot};

use aura_capability::{Ack, CapError, Coordinate, InputDriver, MouseButton, ScrollDirection};

use crate::PlatformDriver;

/// 独占输入线程的命令通道容量（在途命令上限，超出则 `send` 背压等待）。
const INPUT_CHANNEL_CAP: usize = 64;

/// type 预热后到发送真实文本之间的稳定延迟：等 X11 keymap / XTEST 通道 + 焦点就绪。
const TYPE_WARMUP_SETTLE: Duration = Duration::from_millis(50);

/// type 输入期间每键最小延迟（毫秒）：给 X11 keymap 逐字符重映射（XChangeKeyboardMapping）
/// 足够的传播时间。enigo Linux 默认 12ms，此处抬高留余量，输入结束后恢复原值。
const TYPE_KEY_DELAY_MS: u32 = 20;

/// 单条输入命令：由异步方法构造，送入独占线程对 `Enigo` 执行。
enum InputCommand {
    Click {
        x: i32,
        y: i32,
        button: MouseButton,
    },
    Type {
        text: String,
    },
    Key {
        keys: String,
    },
    Scroll {
        x: i32,
        y: i32,
        direction: ScrollDirection,
        amount: i32,
    },
    Drag {
        from: (i32, i32),
        to: (i32, i32),
    },
    MoveMouse {
        x: i32,
        y: i32,
    },
}

/// 命令执行回执发送端：独占线程执行后回送 `Result<(), CapError>`。
type Reply = oneshot::Sender<Result<(), CapError>>;

/// 全局输入线程发送端（进程内唯一）。首次访问惰性启动独占 `Enigo` 的 OS 线程。
static INPUT_TX: OnceLock<mpsc::Sender<(InputCommand, Reply)>> = OnceLock::new();

/// 获取（或惰性启动）输入线程发送端。
fn input_tx() -> &'static mpsc::Sender<(InputCommand, Reply)> {
    INPUT_TX.get_or_init(|| {
        let (tx, rx) = mpsc::channel::<(InputCommand, Reply)>(INPUT_CHANNEL_CAP);
        std::thread::Builder::new()
            .name("aura-input".into())
            .spawn(move || input_thread_main(rx))
            .expect("spawn aura-input thread");
        tx
    })
}

/// 独占输入线程主循环：持有 `!Send` 的 `Enigo`，串行执行收到的命令。
/// 用 `blocking_recv`（本线程非异步上下文），`Enigo` 全程不跨线程、不跨 `.await`。
fn input_thread_main(mut rx: mpsc::Receiver<(InputCommand, Reply)>) {
    let mut enigo = match Enigo::new(&Settings::default()) {
        Ok(e) => e,
        Err(err) => {
            // 后端不可用（如 Linux 无 X 授权 / 缺显示服务器）：排空后续命令并回错，
            // 避免调用方永久挂起。log 用英文。
            eprintln!("aura-input: enigo backend unavailable: {err}");
            let msg = format!("enigo backend unavailable: {err}");
            while let Some((_cmd, reply)) = rx.blocking_recv() {
                let _ = reply.send(Err(CapError::InputFailed(msg.clone())));
            }
            return;
        }
    };
    while let Some((cmd, reply)) = rx.blocking_recv() {
        let _ = reply.send(apply(&mut enigo, cmd));
    }
}

/// 在独占线程内对 `Enigo` 执行单条命令（同步）。坐标越界→`CoordOob`，注入失败→`InputFailed`。
fn apply(enigo: &mut Enigo, cmd: InputCommand) -> Result<(), CapError> {
    // 尽力获取主显示器尺寸用于越界校验；多显示器 / 后端不支持时退化为仅校验负坐标。
    let display = enigo.main_display().ok();
    match cmd {
        InputCommand::Click { x, y, button } => {
            let (x, y) = native_to_input(x, y);
            check_bounds(display, x, y)?;
            enigo.move_mouse(x, y, EnigoCoord::Abs).map_err(input_err)?;
            enigo
                .button(map_button(button), Direction::Click)
                .map_err(input_err)
        }
        InputCommand::Type { text } => type_text_reliable(enigo, &text),
        InputCommand::Key { keys } => {
            let (mods, main) = parse_combo(&keys)?;
            press_combo(enigo, &mods, main)
        }
        InputCommand::Scroll {
            x,
            y,
            direction,
            amount,
        } => {
            let (x, y) = native_to_input(x, y);
            check_bounds(display, x, y)?;
            enigo.move_mouse(x, y, EnigoCoord::Abs).map_err(input_err)?;
            let (axis, length) = scroll_vector(direction, amount);
            enigo.scroll(length, axis).map_err(input_err)
        }
        InputCommand::Drag { from, to } => {
            let from = native_to_input(from.0, from.1);
            let to = native_to_input(to.0, to.1);
            check_bounds(display, from.0, from.1)?;
            check_bounds(display, to.0, to.1)?;
            enigo
                .move_mouse(from.0, from.1, EnigoCoord::Abs)
                .map_err(input_err)?;
            enigo
                .button(Button::Left, Direction::Press)
                .map_err(input_err)?;
            enigo
                .move_mouse(to.0, to.1, EnigoCoord::Abs)
                .map_err(input_err)?;
            enigo
                .button(Button::Left, Direction::Release)
                .map_err(input_err)
        }
        InputCommand::MoveMouse { x, y } => {
            let (x, y) = native_to_input(x, y);
            check_bounds(display, x, y)?;
            enigo.move_mouse(x, y, EnigoCoord::Abs).map_err(input_err)
        }
    }
}

/// 可靠文本输入：修复 Linux X11 上 enigo `text` **首字符丢失**。
///
/// 根因：enigo X11 后端把 Unicode 字符动态重映射到空闲 keycode（`XChangeKeyboardMapping`）
/// 再按键。真实文本的**头几个事件**若落在 XTEST 通道 / keymap 冷启动 + 焦点未就绪的时间窗内会被
/// 丢弃（实测最多丢 9 字符，且被动 wait 越长丢越少——因为 type 自身的首批事件正是冷事件）。
///
/// 策略（三管齐下，均限于 type 路径，不影响其他方法）：
/// 1. 预热：先发一个无害的 Shift 按下+释放（不产生任何字符），让冷启动事件被它吸收、通道就绪；
/// 2. 稳定：短暂 sleep 等 keymap / 焦点通道落定（独占线程内串行，几十 ms 可接受）；
/// 3. 抬高每键延迟输入正文，给逐字符 keymap 重映射足够传播时间，输入后恢复原延迟。
fn type_text_reliable(enigo: &mut Enigo, text: &str) -> Result<(), CapError> {
    if text.is_empty() {
        return Ok(());
    }
    // 1) 预热：Shift tap 是唯一不产生字符又能激活 XTEST/keymap 通道的按键（Alt 会激活菜单、
    //    Ctrl 会触发快捷键，故选 Shift）。失败忽略——预热本身不是关键路径。
    let _ = enigo.key(Key::Shift, Direction::Press);
    let _ = enigo.key(Key::Shift, Direction::Release);
    // 2) 稳定窗口。
    std::thread::sleep(TYPE_WARMUP_SETTLE);
    // 3) 逐字符输入 + 每字符间延迟，给逐字符 keymap 重映射足够传播时间。
    //    用手动 sleep 而非 enigo.set_delay/delay：后两者仅 X11 后端提供，Windows/macOS 后端无此方法
    //    （enigo 跨平台 API 不一致，直接调会导致 Windows 编译 E0599 method not found）。
    for ch in text.chars() {
        enigo.text(&ch.to_string()).map_err(input_err)?;
        std::thread::sleep(Duration::from_millis(TYPE_KEY_DELAY_MS as u64));
    }
    Ok(())
}

/// enigo 输入错误 → 统一注入失败（E_INPUT_FAILED）。
/// 说明：能力层错误枚举无 `InputBlocked` 变体，注入被拒统一归 `InputFailed`。
fn input_err<E: std::fmt::Display>(e: E) -> CapError {
    CapError::InputFailed(e.to_string())
}

/// 领域鼠标按键 → enigo 按键。
fn map_button(b: MouseButton) -> Button {
    match b {
        MouseButton::Left => Button::Left,
        MouseButton::Right => Button::Right,
        MouseButton::Middle => Button::Middle,
    }
}

/// 滚动方向 + 步进量 → (enigo 轴, 带符号步进量)。
/// enigo 约定：正数向下 / 向右（0.6.1 已统一 Windows 滚轮方向，无需手动取反）；
/// 步进单位为一格（15 度点动）。
fn scroll_vector(direction: ScrollDirection, amount: i32) -> (Axis, i32) {
    match direction {
        ScrollDirection::Up => (Axis::Vertical, -amount),
        ScrollDirection::Down => (Axis::Vertical, amount),
        ScrollDirection::Left => (Axis::Horizontal, -amount),
        ScrollDirection::Right => (Axis::Horizontal, amount),
    }
}

/// 坐标越界检查：负坐标必为越界；上界按主显示器尺寸（M1 单显示器假设）。
/// `display` 为 `None`（无法查询显示器尺寸）时仅校验负坐标，不阻断执行。
///
/// 注意：多显示器扩展桌面下副屏坐标可能超过主屏尺寸而被误判越界；M1 聚焦主显示器，
/// 完整多屏边界需与 screen 域 `list_displays` 对接（见任务 summary）。
fn check_bounds(display: Option<(i32, i32)>, x: i32, y: i32) -> Result<(), CapError> {
    if x < 0 || y < 0 {
        return Err(CapError::CoordOob);
    }
    if let Some((w, h)) = display {
        if x >= w || y >= h {
            return Err(CapError::CoordOob);
        }
    }
    Ok(())
}

/// 节点传入的输入坐标是**原生像素**（native = 截图采集空间，`to_native` 按 screenshot scale 由
/// display 回映射所得）。多数平台输入后端坐标系即原生像素（Windows `SendInput` 绝对坐标 /
/// Linux X11 `XTEST`），恒等直用；**唯 macOS** 的 enigo/CGEvent 以**逻辑点**（logical points）为
/// 坐标系——Retina 下逻辑点 = 物理像素 / backing scale factor。若把物理像素坐标直接喂 enigo，坐标
/// 被放大 backing 倍：点击落到约 backing× 位置，边缘更直接被 `check_bounds`（上界取 enigo
/// `main_display` = 逻辑尺寸）判越界 E_COORD_OOB。故 macOS 在越界校验与注入前先按 backing scale
/// 把物理像素折算回逻辑点。
///
/// 见 ISS-20260716-001：mac 3024x1964 面板 xcap 采集报物理 3024（screenshot scale 2.3625），
/// 而 enigo `main_display` 报逻辑 1512（CoreGraphics backing 2.0），两坐标空间差 backing=2.0——
/// 有效点击范围原塌缩为 display 空间一半（[0,640)x[0,416)），折算后恢复全幅。
#[cfg(target_os = "macos")]
fn native_to_input(x: i32, y: i32) -> (i32, i32) {
    native_to_input_scaled(x, y, macos_backing_scale())
}

/// 非 macOS：输入坐标系即原生像素，恒等映射（Windows/Linux 采集空间与注入空间一致，行为零变）。
#[cfg(not(target_os = "macos"))]
#[inline]
fn native_to_input(x: i32, y: i32) -> (i32, i32) {
    (x, y)
}

/// 物理像素 → 逻辑点：按 backing scale 折算（纯逻辑，scale 显式入参便于单测）。四舍五入与
/// `to_native` 的 `scale_coord` 一致。backing 非有限 / 非正时恒等兜底，避免除零并安全降级为改前
/// 行为；backing=1.0（非 Retina）走除法即等于恒等（除以 1 无损），无需特判。
#[cfg(target_os = "macos")]
fn native_to_input_scaled(x: i32, y: i32, backing_scale: f64) -> (i32, i32) {
    if backing_scale.is_finite() && backing_scale > 0.0 {
        (
            (x as f64 / backing_scale).round() as i32,
            (y as f64 / backing_scale).round() as i32,
        )
    } else {
        (x, y)
    }
}

/// 主显示器 backing scale factor（CoreGraphics 逻辑点↔物理像素比，Retina 常为 2.0）。
/// 批F：mac 迁出 xcap（虚拟显示器失明）后改经 `screen::backend` 的 CG Online 直调取值
/// ——与采集枚举同源单一口径，虚拟显示器（合盖无头常驻主屏）亦可见。查询失败兜底 1.0
/// （恒等，安全降级）。不缓存以便运行期改分辨率/缩放后自然跟随（避免陈旧值）。
#[cfg(target_os = "macos")]
fn macos_backing_scale() -> f64 {
    crate::screen::backend::primary_backing_scale()
}

/// 解析 P0 契约的组合键字符串（如 "ctrl+c"、"enter"、"alt+f4"）为（修饰键序列, 主键）。
/// 按 '+' 分割：末位为主键，其余为修饰键；大小写与空白由 [`parse_key`] 归一化。
fn parse_combo(keys: &str) -> Result<(Vec<Key>, Key), CapError> {
    let parts: Vec<String> = keys
        .split('+')
        .map(|s| s.trim().to_string())
        .filter(|s| !s.is_empty())
        .collect();
    parse_combo_parts(&parts)
}

/// 从已分割的键名部件解析组合键。兼容旧的 `Vec<String>`（如 `["ctrl","c"]`）数组契约的内部入口。
fn parse_combo_parts(parts: &[String]) -> Result<(Vec<Key>, Key), CapError> {
    let (main, mods) = parts
        .split_last()
        .ok_or_else(|| CapError::InvalidArg("empty key combination".to_string()))?;
    let main_key = parse_key(main)?;
    let mod_keys = mods
        .iter()
        .map(|m| parse_key(m))
        .collect::<Result<Vec<_>, _>>()?;
    Ok((mod_keys, main_key))
}

/// 单个键名 → enigo `Key`。内部归一化（trim + 小写），故对两个 `parse_combo*` 入口均大小写不敏感。
/// 仅使用跨平台 `Key` 变体（不含 macOS 门控的 `Insert` 等），保证三平台编译一致；
/// 未识别的单字符回退 `Key::Unicode`。
fn parse_key(token: &str) -> Result<Key, CapError> {
    let norm = token.trim().to_ascii_lowercase();
    let key = match norm.as_str() {
        "ctrl" | "control" => Key::Control,
        "alt" | "option" => Key::Alt,
        "shift" => Key::Shift,
        "cmd" | "command" | "meta" | "super" | "win" | "windows" => Key::Meta,
        "enter" | "return" => Key::Return,
        "tab" => Key::Tab,
        "esc" | "escape" => Key::Escape,
        "space" | "spacebar" => Key::Space,
        "backspace" => Key::Backspace,
        "delete" | "del" => Key::Delete,
        "up" => Key::UpArrow,
        "down" => Key::DownArrow,
        "left" => Key::LeftArrow,
        "right" => Key::RightArrow,
        "home" => Key::Home,
        "end" => Key::End,
        "pageup" | "pgup" => Key::PageUp,
        "pagedown" | "pgdn" => Key::PageDown,
        "capslock" => Key::CapsLock,
        "f1" => Key::F1,
        "f2" => Key::F2,
        "f3" => Key::F3,
        "f4" => Key::F4,
        "f5" => Key::F5,
        "f6" => Key::F6,
        "f7" => Key::F7,
        "f8" => Key::F8,
        "f9" => Key::F9,
        "f10" => Key::F10,
        "f11" => Key::F11,
        "f12" => Key::F12,
        _ => {
            // 单字符回退为 Unicode 键（字母 / 数字 / 符号）。
            let mut chars = norm.chars();
            match (chars.next(), chars.next()) {
                (Some(c), None) => Key::Unicode(c),
                _ => return Err(CapError::InvalidArg(format!("unknown key: {token}"))),
            }
        }
    };
    Ok(key)
}

/// 按下修饰键 → 点击主键 → 逆序释放修饰键。
/// 主键失败仍释放修饰键；修饰键按下中途失败则回滚已按下者，避免修饰键卡住。
fn press_combo(enigo: &mut Enigo, mods: &[Key], main: Key) -> Result<(), CapError> {
    let mut pressed = 0usize;
    for &k in mods {
        if let Err(e) = enigo.key(k, Direction::Press) {
            for &pk in mods[..pressed].iter().rev() {
                let _ = enigo.key(pk, Direction::Release);
            }
            return Err(input_err(e));
        }
        pressed += 1;
    }
    let main_res = enigo.key(main, Direction::Click).map_err(input_err);
    for &k in mods.iter().rev() {
        let _ = enigo.key(k, Direction::Release);
    }
    main_res
}

/// 投递命令到独占输入线程并等待回执，收敛为 `Ack`。
async fn dispatch(cmd: InputCommand) -> Result<Ack, CapError> {
    let (reply_tx, reply_rx) = oneshot::channel();
    input_tx()
        .send((cmd, reply_tx))
        .await
        .map_err(|_| CapError::InputFailed("input thread stopped".to_string()))?;
    match reply_rx.await {
        Ok(result) => result.map(|_| Ack::ok()),
        Err(_) => Err(CapError::InputFailed("input thread dropped reply".to_string())),
    }
}

#[async_trait]
impl InputDriver for PlatformDriver {
    async fn click(&self, at: Coordinate, button: MouseButton) -> Result<Ack, CapError> {
        dispatch(InputCommand::Click {
            x: at.x,
            y: at.y,
            button,
        })
        .await
    }

    async fn type_text(&self, text: String) -> Result<Ack, CapError> {
        dispatch(InputCommand::Type { text }).await
    }

    async fn key(&self, keys: String) -> Result<Ack, CapError> {
        dispatch(InputCommand::Key { keys }).await
    }

    async fn scroll(
        &self,
        at: Coordinate,
        direction: ScrollDirection,
        amount: i32,
    ) -> Result<Ack, CapError> {
        dispatch(InputCommand::Scroll {
            x: at.x,
            y: at.y,
            direction,
            amount,
        })
        .await
    }

    async fn drag(&self, from: Coordinate, to: Coordinate) -> Result<Ack, CapError> {
        dispatch(InputCommand::Drag {
            from: (from.x, from.y),
            to: (to.x, to.y),
        })
        .await
    }

    async fn move_mouse(&self, to: Coordinate) -> Result<Ack, CapError> {
        dispatch(InputCommand::MoveMouse { x: to.x, y: to.y }).await
    }

    async fn wait(&self, duration_ms: u64) -> Result<Ack, CapError> {
        // wait 不走输入线程：纯延时，直接 tokio sleep，不占用独占线程。
        tokio::time::sleep(Duration::from_millis(duration_ms)).await;
        Ok(Ack::ok())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    // --- key 解析：P0 单字符串 "ctrl+c" → Ctrl + C ---

    #[test]
    fn parse_ctrl_c_into_modifier_and_main() {
        let (mods, main) = parse_combo("ctrl+c").unwrap();
        assert_eq!(mods.len(), 1);
        assert!(matches!(mods[0], Key::Control));
        assert!(matches!(main, Key::Unicode('c')));
    }

    #[test]
    fn parse_single_named_key() {
        let (mods, main) = parse_combo("enter").unwrap();
        assert!(mods.is_empty());
        assert!(matches!(main, Key::Return));
    }

    #[test]
    fn parse_multi_modifier_combo() {
        let (mods, main) = parse_combo("ctrl+shift+t").unwrap();
        assert_eq!(mods.len(), 2);
        assert!(matches!(mods[0], Key::Control));
        assert!(matches!(mods[1], Key::Shift));
        assert!(matches!(main, Key::Unicode('t')));
    }

    #[test]
    fn parse_is_case_insensitive_and_trims() {
        let (mods, main) = parse_combo(" ALT + F4 ").unwrap();
        assert!(matches!(mods[0], Key::Alt));
        assert!(matches!(main, Key::F4));
    }

    #[test]
    fn parse_empty_combo_is_invalid_arg() {
        assert!(matches!(parse_combo(""), Err(CapError::InvalidArg(_))));
        assert!(matches!(parse_combo("+"), Err(CapError::InvalidArg(_))));
    }

    #[test]
    fn vec_string_compat_entry() {
        // 内部兼容旧数组契约 ["ctrl","c"]。
        let parts = vec!["ctrl".to_string(), "c".to_string()];
        let (mods, main) = parse_combo_parts(&parts).unwrap();
        assert!(matches!(mods[0], Key::Control));
        assert!(matches!(main, Key::Unicode('c')));
    }

    // --- scroll 方向映射 ---

    #[test]
    fn scroll_direction_maps_to_axis_and_sign() {
        assert!(matches!(
            scroll_vector(ScrollDirection::Up, 3),
            (Axis::Vertical, -3)
        ));
        assert!(matches!(
            scroll_vector(ScrollDirection::Down, 3),
            (Axis::Vertical, 3)
        ));
        assert!(matches!(
            scroll_vector(ScrollDirection::Left, 5),
            (Axis::Horizontal, -5)
        ));
        assert!(matches!(
            scroll_vector(ScrollDirection::Right, 5),
            (Axis::Horizontal, 5)
        ));
    }

    // --- 坐标越界 → E_COORD_OOB ---

    #[test]
    fn negative_coordinate_is_out_of_bounds() {
        assert!(matches!(check_bounds(None, -1, 5), Err(CapError::CoordOob)));
        assert!(matches!(
            check_bounds(Some((100, 100)), 5, -1),
            Err(CapError::CoordOob)
        ));
    }

    #[test]
    fn coordinate_beyond_display_is_out_of_bounds() {
        assert!(matches!(
            check_bounds(Some((100, 100)), 100, 50),
            Err(CapError::CoordOob)
        ));
        assert!(matches!(
            check_bounds(Some((100, 100)), 50, 100),
            Err(CapError::CoordOob)
        ));
        assert_eq!(CapError::CoordOob.code(), "E_COORD_OOB");
    }

    #[test]
    fn coordinate_within_display_is_ok() {
        assert!(check_bounds(Some((100, 100)), 0, 0).is_ok());
        assert!(check_bounds(Some((100, 100)), 99, 99).is_ok());
        // 无显示器尺寸信息时仅拦负坐标，正坐标放行（best-effort）。
        assert!(check_bounds(None, 5000, 5000).is_ok());
    }

    // --- macOS 物理像素 → 逻辑点折算（ISS-20260716-001）---

    #[cfg(target_os = "macos")]
    #[test]
    fn native_to_input_divides_physical_by_backing_scale() {
        // backing 2.0（Retina）：3024x1964 面板中心物理 [1512,980] → 逻辑 [756,490]（enigo 命中真中心）。
        assert_eq!(native_to_input_scaled(1512, 980, 2.0), (756, 490));
        // 改前塌缩边界：display 空间近满幅对应物理 3022 → 逻辑 1511（< 逻辑宽 1512，不再 E_COORD_OOB）。
        assert_eq!(native_to_input_scaled(3022, 1962, 2.0), (1511, 981));
        // 四舍五入与 to_native 一致：物理 755 / 2.0 = 377.5 → 378。
        assert_eq!(native_to_input_scaled(755, 755, 2.0), (378, 378));
        // backing 1.0（非 Retina）恒等——与 Win/Linux 同形入参语义一致。
        assert_eq!(native_to_input_scaled(640, 415, 1.0), (640, 415));
        // 异常 backing（<=0 / 非有限）兜底恒等，安全降级为改前行为。
        assert_eq!(native_to_input_scaled(100, 100, 0.0), (100, 100));
        assert_eq!(native_to_input_scaled(100, 100, -2.0), (100, 100));
        assert_eq!(native_to_input_scaled(100, 100, f64::NAN), (100, 100));
    }

    // --- 鼠标按键映射 ---

    #[test]
    fn map_button_covers_all_variants() {
        assert!(matches!(map_button(MouseButton::Left), Button::Left));
        assert!(matches!(map_button(MouseButton::Right), Button::Right));
        assert!(matches!(map_button(MouseButton::Middle), Button::Middle));
    }
}
