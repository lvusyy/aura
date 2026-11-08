//! A11yDriver 平台实现：Windows UI Automation / Linux AT-SPI / macOS AXUIElement。
//!
//! 采集后端只此一层分平台（`#[cfg]` 门控），capability 层保持平台无关：
//!   - Windows：`uiautomation`（IUIAutomation 的安全封装，复刻本层既有高层平台封装惯例
//!     windows-capture / xcap / enigo，将 COM 收敛在封装内）。UIAutomation COM 对象 `!Send`，
//!     故起一条独占 OS 线程持有之，异步方法经通道投递请求 / 收回执——复刻 M1 输入注入（enigo）
//!     专用线程先例（见 input.rs / ARCHITECTURE §3 决策 3）。
//!   - Linux：`atspi`（zbus AT-SPI2）。走 D-Bus 无障碍总线，天然异步，直接在 tokio 上 await。
//!   - macOS：`ApplicationServices` (`AXUIElement` C FFI) + `CGWindowListCopyWindowInfo` 在屏窗口枚举。
//!     `AXUIElement` 坐标（`kAXPositionAttribute` / `kAXSizeAttribute`）为 CoreGraphics 逻辑点，
//!     采集时统一乘 `screen::backend::primary_backing_scale()` 折算为原生物理像素，与 `screenshot`
//!     原生尺寸及 `input::native_to_input` 逆折算严格闭环。
//!
//! 过滤语义（Locked-6，默认浅层防树爆 R-4）：`depth` 限后代层数、`max_nodes` 限节点总数
//! （命中即 `truncated=true`）、`role` 剪枝保留匹配节点及其祖先路径、`root` 选子树根。
//! 跨平台节点统一形状 role/name/bounds/value/children（bounds/value 平台不可得时省略）。

use async_trait::async_trait;

use aura_capability::{A11yDriver, A11yNode, A11yParams, A11yTree, CapError};

use crate::PlatformDriver;

#[async_trait]
impl A11yDriver for PlatformDriver {
    async fn get_a11y_tree(&self, params: A11yParams) -> Result<A11yTree, CapError> {
        backend::get_a11y_tree(params).await
    }
}

// ===== 平台无关的遍历预算 / role 剪枝 / AX 归一纯函数（三平台复用）=====

/// 遍历预算：剩余可创建节点数 + 是否已截断（命中 depth / max_nodes 上限）。
struct Budget {
    remaining: u32,
    truncated: bool,
}

/// 按角色剪枝：保留角色匹配的节点及其祖先路径（有匹配后代的节点亦保留），其余剪除。
/// role 为 None / 空串时调用方跳过本函数（不过滤）。
fn prune_by_role(node: A11yNode, role: &str) -> Option<A11yNode> {
    let A11yNode {
        role: node_role,
        name,
        bounds,
        value,
        children,
    } = node;
    let kept: Vec<A11yNode> = children
        .into_iter()
        .filter_map(|c| prune_by_role(c, role))
        .collect();
    if node_role.eq_ignore_ascii_case(role) || !kept.is_empty() {
        Some(A11yNode {
            role: node_role,
            name,
            bounds,
            value,
            children: kept,
        })
    } else {
        None
    }
}

/// 将 macOS Accessibility 原始角色（如 `"AXButton"`、`"AXStaticText"`、`"AXTextField"`）
/// 归一化为跨平台小写角色名（与 Windows/Linux/SelectorEngine `isInteractiveRole` 对齐）。
/// 纯函数置于门控外，供全平台离线单测验证。
#[allow(dead_code)]
pub(crate) fn normalize_ax_role(raw: &str) -> String {
    let trimmed = raw.trim();
    let stripped = trimmed
        .strip_prefix("AX")
        .or_else(|| trimmed.strip_prefix("ax"))
        .unwrap_or(trimmed);
    let lower = stripped.to_ascii_lowercase();
    match lower.as_str() {
        "" => "unknown".to_string(),
        "statictext" => "text".to_string(),
        "textfield" | "textarea" | "securetextfield" => "textbox".to_string(),
        "searchfield" => "searchbox".to_string(),
        "popupbutton" | "combobox" => "combobox".to_string(),
        "radiobutton" => "radio button".to_string(),
        "checkbox" => "checkbox".to_string(),
        "menuitem" => "menuitem".to_string(),
        "tabgroup" => "tab".to_string(),
        other => other.to_string(),
    }
}

/// 将 macOS `AXUIElement` 的逻辑点矩形 `(x, y, w, h)` 按 `backing_scale` 折算为原生物理像素
/// `[x, y, w, h]`。零/负面积或非有限坐标返回 `None`（幽灵框不入 IoU）。
/// 纯函数置于门控外，供全平台离线单测验证。
#[allow(dead_code)]
pub(crate) fn ax_rect_to_bounds(
    x: f64,
    y: f64,
    w: f64,
    h: f64,
    backing_scale: f64,
) -> Option<[i32; 4]> {
    if !x.is_finite() || !y.is_finite() || !w.is_finite() || !h.is_finite() || w <= 0.0 || h <= 0.0
    {
        return None;
    }
    let scale = if backing_scale.is_finite() && backing_scale > 0.0 {
        backing_scale
    } else {
        1.0
    };
    let nx = (x * scale).round() as i32;
    let ny = (y * scale).round() as i32;
    let nw = (w * scale).round() as i32;
    let nh = (h * scale).round() as i32;
    (nw > 0 && nh > 0).then_some([nx, ny, nw, nh])
}

// ===== Windows 采集后端：uiautomation（IUIAutomation 安全封装），独占 COM 线程 =====

#[cfg(windows)]
mod backend {
    use std::sync::OnceLock;

    use tokio::sync::{mpsc, oneshot};
    use uiautomation::core::{UIAutomation, UICondition, UIElement};
    use uiautomation::types::TreeScope;

    use aura_capability::{A11yNode, A11yParams, A11yTree, CapError};

    use super::{prune_by_role, Budget};

    /// 一次取树请求：入参 + 回执通道。结果为纯数据 A11yTree，可跨线程回送。
    type Job = (A11yParams, oneshot::Sender<Result<A11yTree, CapError>>);

    /// 全局 UIA 线程发送端（进程内唯一）。首次访问惰性启动独占 UIAutomation 的 OS 线程。
    static UIA_TX: OnceLock<mpsc::Sender<Job>> = OnceLock::new();

    fn uia_tx() -> &'static mpsc::Sender<Job> {
        UIA_TX.get_or_init(|| {
            let (tx, rx) = mpsc::channel::<Job>(16);
            std::thread::Builder::new()
                .name("aura-uia".into())
                .spawn(move || uia_thread_main(rx))
                .expect("spawn aura-uia thread");
            tx
        })
    }

    /// 独占 UIA 线程主循环：持有 `!Send` 的 UIAutomation（COM 在本线程初始化为 MTA），串行处理请求。
    /// COM 对象全程不跨线程、不跨 `.await`。构造失败则对每个请求回错，避免调用方永挂。
    fn uia_thread_main(mut rx: mpsc::Receiver<Job>) {
        let automation = UIAutomation::new();
        while let Some((params, reply)) = rx.blocking_recv() {
            let result = match automation.as_ref() {
                Ok(a) => build_tree(a, &params),
                Err(e) => Err(CapError::Unsupported(format!("UI Automation unavailable: {e}"))),
            };
            let _ = reply.send(result);
        }
    }

    /// 独占线程内同步构建树：选根 → 逐顶层子树遍历（depth/max_nodes 约束）→ role 剪枝。
    fn build_tree(automation: &UIAutomation, params: &A11yParams) -> Result<A11yTree, CapError> {
        let root = select_root(automation, params.root.as_deref())?;
        let cond = automation
            .create_true_condition()
            .map_err(|e| CapError::Internal(format!("create UIA condition failed: {e}")))?;

        let mut budget = Budget {
            remaining: params.max_nodes,
            truncated: false,
        };
        // 顶层 nodes = 选定根的直接子节点（各为一棵子树），depth 自其下计。
        let children = root
            .find_all(TreeScope::Children, &cond)
            .map_err(|e| CapError::Internal(format!("enumerate root a11y children failed: {e}")))?;
        let mut nodes = Vec::new();
        for child in children {
            if budget.remaining == 0 {
                budget.truncated = true;
                break;
            }
            if let Some(n) = walk(&child, params.depth, &cond, &mut budget) {
                nodes.push(n);
            }
        }

        if let Some(role) = params.role.as_deref().filter(|r| !r.is_empty()) {
            nodes = nodes.into_iter().filter_map(|n| prune_by_role(n, role)).collect();
        }

        Ok(A11yTree {
            nodes,
            truncated: budget.truncated,
        })
    }

    /// 按 root 选择器取子树根：缺省 / "root" / "desktop" = 桌面根；"focus" / "focused" = 焦点元素。
    fn select_root(automation: &UIAutomation, root: Option<&str>) -> Result<UIElement, CapError> {
        let sel = root.map(|s| s.trim().to_ascii_lowercase());
        let elem = match sel.as_deref() {
            Some("focus") | Some("focused") => automation.get_focused_element(),
            _ => automation.get_root_element(),
        };
        elem.map_err(|e| CapError::Internal(format!("select a11y root failed: {e}")))
    }

    /// 递归遍历一个 UIA 元素为 A11yNode（同步，独占线程内）。
    /// depth = 该节点下仍要包含的后代层数（0 = 仅本节点）；命中 max_nodes / depth 上限置 truncated。
    fn walk(
        element: &UIElement,
        depth: u32,
        cond: &UICondition,
        budget: &mut Budget,
    ) -> Option<A11yNode> {
        if budget.remaining == 0 {
            budget.truncated = true;
            return None;
        }
        budget.remaining -= 1;

        // role 用本地化控件类型（Result<String>，稳定可得）；name 无则空串；bounds 取包围盒 [x,y,w,h]。
        let role = element
            .get_localized_control_type()
            .unwrap_or_else(|_| "unknown".to_string());
        let name = element.get_name().unwrap_or_default();
        let bounds = element
            .get_bounding_rectangle()
            .ok()
            .map(|r| [r.get_left(), r.get_top(), r.get_width(), r.get_height()]);
        let mut node = A11yNode {
            role,
            name,
            bounds,
            // value 需 ValuePattern 属性（VARIANT）读取，M3 core best-effort 省略（跨平台形状保留字段）。
            value: None,
            children: Vec::new(),
        };

        let children = match element.find_all(TreeScope::Children, cond) {
            Ok(c) => c,
            Err(_) => {
                budget.truncated = true;
                Vec::new()
            }
        };
        if depth == 0 {
            // 到达深度上限：若确有子节点则标记截断（树未完整）。
            if !children.is_empty() {
                budget.truncated = true;
            }
        } else {
            for child in children {
                if budget.remaining == 0 {
                    budget.truncated = true;
                    break;
                }
                if let Some(cn) = walk(&child, depth - 1, cond, budget) {
                    node.children.push(cn);
                }
            }
        }
        Some(node)
    }

    /// 异步入口：投递取树请求到独占 UIA 线程并等待回执。
    pub(super) async fn get_a11y_tree(params: A11yParams) -> Result<A11yTree, CapError> {
        let (reply_tx, reply_rx) = oneshot::channel();
        uia_tx()
            .send((params, reply_tx))
            .await
            .map_err(|_| CapError::Internal("UIA thread stopped".to_string()))?;
        reply_rx
            .await
            .map_err(|_| CapError::Internal("UIA thread dropped reply".to_string()))?
    }
}

// ===== Linux 采集后端：atspi（zbus AT-SPI2），异步 D-Bus 遍历 =====

#[cfg(target_os = "linux")]
mod backend {
    use std::future::Future;
    use std::pin::Pin;

    use atspi::connection::AccessibilityConnection;
    use atspi::object_ref::ObjectRefOwned;
    use atspi::proxy::accessible::AccessibleProxy;
    use atspi::proxy::proxy_ext::ProxyExt;
    use atspi::zbus::proxy::CacheProperties;
    use atspi::CoordType;

    use aura_capability::{A11yNode, A11yParams, A11yTree, CapError};

    use super::{prune_by_role, Budget};

    /// async 递归的装箱返回类型（+ Send：反连侧 tokio::spawn 要求整链 Send）。
    type NodeFuture<'a> = Pin<Box<dyn Future<Output = Option<A11yNode>> + Send + 'a>>;

    pub(super) async fn get_a11y_tree(params: A11yParams) -> Result<A11yTree, CapError> {
        // 连接 AT-SPI 无障碍总线：AT_SPI_BUS_ADDRESS 显式地址优先——sidecar 跨容器拓扑（如
        // Selkies KDE）本容器无 session bus，AccessibilityConnection::new() 的标准发现
        // （session bus → org.a11y.Bus GetAddress）无从进行，须由部署面直接给总线地址；
        // 未设时走标准发现。总线不可用（无 at-spi2-registryd / 未启用）→ E_UNSUPPORTED。
        let conn = match std::env::var("AT_SPI_BUS_ADDRESS").ok().filter(|a| !a.is_empty()) {
            Some(addr) => {
                let parsed: atspi::zbus::Address = addr.parse().map_err(|e| {
                    CapError::Unsupported(format!("invalid AT_SPI_BUS_ADDRESS {addr:?}: {e}"))
                })?;
                AccessibilityConnection::from_address(parsed).await
            }
            None => AccessibilityConnection::new().await,
        }
        .map_err(|e| CapError::Unsupported(format!("AT-SPI accessibility bus unavailable: {e}")))?;
        // registry 根的子节点 = 总线上所有无障碍应用的根对象（顶层 nodes 起点）。
        let root = conn
            .root_accessible_on_registry()
            .await
            .map_err(|e| CapError::Internal(format!("get AT-SPI registry root failed: {e}")))?;
        let top = root
            .get_children()
            .await
            .map_err(|e| CapError::Internal(format!("get root children failed: {e}")))?;

        let mut budget = Budget {
            remaining: params.max_nodes,
            truncated: false,
        };
        let mut nodes = Vec::new();
        for child in top {
            if budget.remaining == 0 {
                budget.truncated = true;
                break;
            }
            if let Some(n) = walk(&conn, child, params.depth, &mut budget).await {
                nodes.push(n);
            }
        }

        if let Some(role) = params.role.as_deref().filter(|r| !r.is_empty()) {
            nodes = nodes.into_iter().filter_map(|n| prune_by_role(n, role)).collect();
        }

        Ok(A11yTree {
            nodes,
            truncated: budget.truncated,
        })
    }

    /// 递归遍历一个无障碍对象为 A11yNode（async 递归经 Box::pin 装箱）。
    /// depth = 该节点下仍要包含的后代层数（0 = 仅本节点）；命中 max_nodes / depth 上限置 truncated。
    fn walk<'a>(
        conn: &'a AccessibilityConnection,
        obj: ObjectRefOwned,
        depth: u32,
        budget: &'a mut Budget,
    ) -> NodeFuture<'a> {
        Box::pin(async move {
            if budget.remaining == 0 {
                budget.truncated = true;
                return None;
            }
            budget.remaining -= 1;

            // 单节点取代理失败不阻断整树（best-effort）。显式 destination+path 构造总线路由
            // proxy，不用 P2P trait 的 object_as_accessible()：其 bus-fallback 分支不设
            // destination（atspi-connection 0.14 p2p.rs），无 P2P peer 的对象所有方法调用必败；
            // 而 P2P 直连 socket（应用私有 /tmp/atspi2-*）在跨容器部署（Selkies sidecar）不可达，
            // peer 永不可得——总线路由是唯一可用且正确的路径。
            let obj_name = match obj.name() {
                Some(n) => n.to_owned(),
                None => {
                    budget.truncated = true;
                    return None;
                }
            };
            let builder = AccessibleProxy::builder(conn.connection())
                .destination(obj_name)
                .and_then(|b| b.path(obj.path()));
            let proxy = match builder {
                Ok(b) => match b.cache_properties(CacheProperties::No).build().await {
                    Ok(p) => p,
                    Err(_) => {
                        budget.truncated = true;
                        return None;
                    }
                },
                Err(_) => {
                    budget.truncated = true;
                    return None;
                }
            };
            let role = proxy
                .get_role_name()
                .await
                .unwrap_or_else(|_| "unknown".to_string());
            let name = proxy.name().await.unwrap_or_default();
            // bounds：AT-SPI Component 接口 get_extents（CoordType::Screen 屏幕坐标 = 原生像素，
            // 与截图 native 空间同源 types.rs:163）；接口不可得 / 失败 / 零负面积则 best-effort None。
            // value 仍省略（SC 不需，需 Value 接口，YAGNI；跨平台形状保留字段）。
            let bounds = component_bounds(&proxy).await;
            let mut node = A11yNode {
                role,
                name,
                bounds,
                value: None,
                children: Vec::new(),
            };

            let children = match proxy.get_children().await {
                Ok(c) => c,
                Err(_) => {
                    budget.truncated = true;
                    Vec::new()
                }
            };
            if depth == 0 {
                if !children.is_empty() {
                    budget.truncated = true;
                }
            } else {
                for child in children {
                    if budget.remaining == 0 {
                        budget.truncated = true;
                        break;
                    }
                    if let Some(cn) = walk(conn, child, depth - 1, &mut *budget).await {
                        node.children.push(cn);
                    }
                }
            }
            Some(node)
        })
    }

    /// best-effort 取 AT-SPI Component 接口的屏幕坐标 extents 为 bounds（原生像素 [x, y, w, h]）。
    /// 经 ProxyExt::proxies() 取对象接口集，仅含 Component 接口时 component() 成功；get_extents 用
    /// CoordType::Screen（屏幕像素，与截图 native 空间同源）。接口不可得 / 调用失败 / 零负面积 → None
    /// （不阻断节点，仅该字段空——同 Android/iOS「有节点无 bounds 不参与 IoU」best-effort 语义）。
    async fn component_bounds(proxy: &AccessibleProxy<'_>) -> Option<[i32; 4]> {
        let proxies = proxy.proxies().await.ok()?;
        let component = proxies.component().await.ok()?;
        let extents = component.get_extents(CoordType::Screen).await.ok()?;
        extents_to_bounds(extents)
    }

    /// 由 Component get_extents 的 (x, y, w, h) 归一为 bounds：零 / 负面积 → None（幽灵框不入 IoU）。
    /// 坐标可为负（离屏 / 多显示器），面积须正。纯函数，离线可测 best-effort 面积守卫。
    pub(super) fn extents_to_bounds(extents: (i32, i32, i32, i32)) -> Option<[i32; 4]> {
        let (x, y, w, h) = extents;
        (w > 0 && h > 0).then_some([x, y, w, h])
    }
}

// ===== macOS 采集后端：ApplicationServices AXUIElement C FFI + CGWindowList 在屏窗口枚举 =====

#[cfg(target_os = "macos")]
mod backend {
    use std::ffi::{c_char, c_void, CString};
    use std::ptr;

    use aura_capability::{A11yNode, A11yParams, A11yTree, CapError};

    use super::{ax_rect_to_bounds, normalize_ax_role, prune_by_role, Budget};

    type CFTypeRef = *const c_void;
    type CFStringRef = *const c_void;
    type CFArrayRef = *const c_void;
    type CFDictionaryRef = *const c_void;
    type CFNumberRef = *const c_void;
    type AXUIElementRef = *const c_void;
    type AXValueRef = *const c_void;

    const K_CF_STRING_ENCODING_UTF8: u32 = 0x0800_0100;
    const K_CF_NUMBER_SINT32_TYPE: i32 = 3;
    const K_AX_ERROR_SUCCESS: i32 = 0;
    const K_AX_VALUE_CGPOINT_TYPE: u32 = 1;
    const K_AX_VALUE_CGSIZE_TYPE: u32 = 2;
    /// kCGWindowListOptionOnScreenOnly (1) | kCGWindowListExcludeDesktopElements (16)
    const CG_WINDOW_LIST_OPTS: u32 = 1 | 16;

    #[repr(C)]
    #[derive(Clone, Copy, Default)]
    struct CGPoint {
        x: f64,
        y: f64,
    }

    #[repr(C)]
    #[derive(Clone, Copy, Default)]
    struct CGSize {
        width: f64,
        height: f64,
    }

    #[link(name = "CoreFoundation", kind = "framework")]
    extern "C" {
        fn CFRelease(cf: CFTypeRef);
        fn CFGetTypeID(cf: CFTypeRef) -> usize;
        fn CFStringGetTypeID() -> usize;
        fn CFArrayGetTypeID() -> usize;
        fn CFNumberGetTypeID() -> usize;
        fn CFStringCreateWithCString(
            alloc: CFTypeRef,
            c_str: *const c_char,
            encoding: u32,
        ) -> CFStringRef;
        fn CFStringGetLength(the_string: CFStringRef) -> isize;
        fn CFStringGetMaximumSizeForEncoding(length: isize, encoding: u32) -> isize;
        fn CFStringGetCString(
            the_string: CFStringRef,
            buffer: *mut c_char,
            buffer_size: isize,
            encoding: u32,
        ) -> u8;
        fn CFArrayGetCount(the_array: CFArrayRef) -> isize;
        fn CFArrayGetValueAtIndex(the_array: CFArrayRef, idx: isize) -> CFTypeRef;
        fn CFDictionaryGetValue(the_dict: CFDictionaryRef, key: CFTypeRef) -> CFTypeRef;
        fn CFNumberGetValue(
            number: CFNumberRef,
            the_type: i32,
            value_ptr: *mut c_void,
        ) -> u8;
    }

    #[link(name = "CoreGraphics", kind = "framework")]
    extern "C" {
        fn CGWindowListCopyWindowInfo(option: u32, relative_to_window: u32) -> CFArrayRef;
    }

    #[link(name = "ApplicationServices", kind = "framework")]
    extern "C" {
        fn AXIsProcessTrusted() -> u8;
        fn AXUIElementCreateSystemWide() -> AXUIElementRef;
        fn AXUIElementCreateApplication(pid: i32) -> AXUIElementRef;
        fn AXUIElementCopyAttributeValue(
            element: AXUIElementRef,
            attribute: CFStringRef,
            value: *mut CFTypeRef,
        ) -> i32;
        fn AXValueGetType(value: AXValueRef) -> u32;
        fn AXValueGetValue(value: AXValueRef, the_type: u32, value_ptr: *mut c_void) -> u8;
    }

    struct CfOwned(CFTypeRef);

    impl CfOwned {
        fn from_owned(ptr: CFTypeRef) -> Option<Self> {
            if ptr.is_null() {
                None
            } else {
                Some(Self(ptr))
            }
        }

        fn as_ptr(&self) -> CFTypeRef {
            self.0
        }
    }

    impl Drop for CfOwned {
        fn drop(&mut self) {
            if !self.0.is_null() {
                unsafe { CFRelease(self.0) };
            }
        }
    }

    fn make_cfstring(s: &str) -> Option<CfOwned> {
        let cstr = CString::new(s).ok()?;
        let ptr = unsafe {
            CFStringCreateWithCString(ptr::null(), cstr.as_ptr(), K_CF_STRING_ENCODING_UTF8)
        };
        CfOwned::from_owned(ptr)
    }

    fn cfstring_to_string(cf: CFTypeRef) -> Option<String> {
        if cf.is_null() {
            return None;
        }
        unsafe {
            if CFGetTypeID(cf) != CFStringGetTypeID() {
                return None;
            }
            let len = CFStringGetLength(cf);
            if len <= 0 {
                return Some(String::new());
            }
            let max_bytes = CFStringGetMaximumSizeForEncoding(len, K_CF_STRING_ENCODING_UTF8) + 1;
            if max_bytes <= 1 {
                return None;
            }
            let mut buf = vec![0u8; max_bytes as usize];
            let ok = CFStringGetCString(
                cf,
                buf.as_mut_ptr() as *mut c_char,
                max_bytes,
                K_CF_STRING_ENCODING_UTF8,
            );
            if ok == 0 {
                return None;
            }
            let nul_pos = buf.iter().position(|&b| b == 0).unwrap_or(buf.len());
            String::from_utf8(buf[..nul_pos].to_vec()).ok()
        }
    }

    fn copy_attr(elem: AXUIElementRef, attr_name: &str) -> Option<CfOwned> {
        let key = make_cfstring(attr_name)?;
        let mut out: CFTypeRef = ptr::null();
        let err = unsafe { AXUIElementCopyAttributeValue(elem, key.as_ptr(), &mut out) };
        if err == K_AX_ERROR_SUCCESS {
            CfOwned::from_owned(out)
        } else {
            None
        }
    }

    fn copy_attr_string(elem: AXUIElementRef, attr_name: &str) -> Option<String> {
        let val = copy_attr(elem, attr_name)?;
        cfstring_to_string(val.as_ptr())
    }

    fn copy_bounds(elem: AXUIElementRef, backing_scale: f64) -> Option<[i32; 4]> {
        let pos_val = copy_attr(elem, "AXPosition")?;
        let size_val = copy_attr(elem, "AXSize")?;
        let mut pt = CGPoint::default();
        let mut sz = CGSize::default();
        unsafe {
            if AXValueGetType(pos_val.as_ptr()) != K_AX_VALUE_CGPOINT_TYPE
                || AXValueGetValue(
                    pos_val.as_ptr(),
                    K_AX_VALUE_CGPOINT_TYPE,
                    &mut pt as *mut _ as *mut c_void,
                ) == 0
            {
                return None;
            }
            if AXValueGetType(size_val.as_ptr()) != K_AX_VALUE_CGSIZE_TYPE
                || AXValueGetValue(
                    size_val.as_ptr(),
                    K_AX_VALUE_CGSIZE_TYPE,
                    &mut sz as *mut _ as *mut c_void,
                ) == 0
            {
                return None;
            }
        }
        ax_rect_to_bounds(pt.x, pt.y, sz.width, sz.height, backing_scale)
    }

    /// 枚举当前在屏窗口所属的应用 PID（按 Z-order 由前到后去重，过滤桌面背景层 layer != 0）。
    fn onscreen_app_pids() -> Vec<i32> {
        let mut pids = Vec::new();
        let Some(info_list) =
            CfOwned::from_owned(unsafe { CGWindowListCopyWindowInfo(CG_WINDOW_LIST_OPTS, 0) })
        else {
            return pids;
        };
        let Some(k_pid) = make_cfstring("kCGWindowOwnerPID") else {
            return pids;
        };
        let Some(k_layer) = make_cfstring("kCGWindowLayer") else {
            return pids;
        };
        let count = unsafe { CFArrayGetCount(info_list.as_ptr()) };
        for i in 0..count {
            let dict = unsafe { CFArrayGetValueAtIndex(info_list.as_ptr(), i) };
            if dict.is_null() {
                continue;
            }
            let layer_ref = unsafe { CFDictionaryGetValue(dict, k_layer.as_ptr()) };
            if !layer_ref.is_null()
                && unsafe { CFGetTypeID(layer_ref) == CFNumberGetTypeID() }
            {
                let mut layer: i32 = 0;
                if unsafe {
                    CFNumberGetValue(
                        layer_ref,
                        K_CF_NUMBER_SINT32_TYPE,
                        &mut layer as *mut _ as *mut c_void,
                    )
                } != 0
                    && layer != 0
                {
                    continue;
                }
            }
            let pid_ref = unsafe { CFDictionaryGetValue(dict, k_pid.as_ptr()) };
            if pid_ref.is_null() || unsafe { CFGetTypeID(pid_ref) != CFNumberGetTypeID() } {
                continue;
            }
            let mut pid: i32 = 0;
            if unsafe {
                CFNumberGetValue(
                    pid_ref,
                    K_CF_NUMBER_SINT32_TYPE,
                    &mut pid as *mut _ as *mut c_void,
                )
            } != 0
                && pid > 0
                && !pids.contains(&pid)
            {
                pids.push(pid);
            }
        }
        pids
    }

    fn walk_ax(
        elem: AXUIElementRef,
        depth: u32,
        budget: &mut Budget,
        backing_scale: f64,
    ) -> Option<A11yNode> {
        if elem.is_null() {
            return None;
        }
        if budget.remaining == 0 {
            budget.truncated = true;
            return None;
        }
        budget.remaining -= 1;

        let raw_role = copy_attr_string(elem, "AXRole").unwrap_or_default();
        let role = normalize_ax_role(&raw_role);
        let title = copy_attr_string(elem, "AXTitle").unwrap_or_default();
        let name = if !title.trim().is_empty() {
            title
        } else {
            copy_attr_string(elem, "AXDescription").unwrap_or_default()
        };
        let value = copy_attr_string(elem, "AXValue").filter(|s| !s.is_empty());
        let bounds = copy_bounds(elem, backing_scale);

        let mut node = A11yNode {
            role,
            name,
            bounds,
            value,
            children: Vec::new(),
        };

        let children_cf = copy_attr(elem, "AXChildren").or_else(|| copy_attr(elem, "AXWindows"));
        let Some(children_arr) = children_cf else {
            return Some(node);
        };
        if unsafe { CFGetTypeID(children_arr.as_ptr()) != CFArrayGetTypeID() } {
            return Some(node);
        }
        let child_count = unsafe { CFArrayGetCount(children_arr.as_ptr()) };
        if depth == 0 {
            if child_count > 0 {
                budget.truncated = true;
            }
            return Some(node);
        }

        for idx in 0..child_count {
            if budget.remaining == 0 {
                budget.truncated = true;
                break;
            }
            let child = unsafe { CFArrayGetValueAtIndex(children_arr.as_ptr(), idx) };
            if let Some(cn) = walk_ax(child, depth - 1, budget, backing_scale) {
                node.children.push(cn);
            }
        }
        Some(node)
    }

    fn build_tree_sync(params: A11yParams) -> Result<A11yTree, CapError> {
        if unsafe { AXIsProcessTrusted() } == 0 {
            return Err(CapError::Unavailable(
                "macOS Accessibility permission (TCC) not granted for aura-node".to_string(),
            ));
        }

        let backing_scale = crate::screen::backend::primary_backing_scale();
        let mut budget = Budget {
            remaining: params.max_nodes,
            truncated: false,
        };

        let mut roots: Vec<A11yNode> = Vec::new();
        let root_spec = params.root.as_deref().unwrap_or("root");

        if root_spec == "focus" {
            if let Some(sys) = CfOwned::from_owned(unsafe { AXUIElementCreateSystemWide() }) {
                let focused = copy_attr(sys.as_ptr(), "AXFocusedApplication")
                    .or_else(|| copy_attr(sys.as_ptr(), "AXFocusedUIElement"));
                if let Some(f) = focused {
                    if let Some(n) = walk_ax(f.as_ptr(), params.depth, &mut budget, backing_scale) {
                        roots.push(n);
                    }
                }
            }
        }

        if roots.is_empty() {
            let pids = onscreen_app_pids();
            let filter_lower = (root_spec != "root" && root_spec != "focus" && !root_spec.is_empty())
                .then(|| root_spec.to_ascii_lowercase());
            for pid in pids {
                if budget.remaining == 0 {
                    budget.truncated = true;
                    break;
                }
                let Some(app) = CfOwned::from_owned(unsafe { AXUIElementCreateApplication(pid) })
                else {
                    continue;
                };
                if let Some(ref needle) = filter_lower {
                    let app_title = copy_attr_string(app.as_ptr(), "AXTitle")
                        .unwrap_or_default()
                        .to_ascii_lowercase();
                    if !app_title.contains(needle) {
                        continue;
                    }
                }
                if let Some(n) = walk_ax(app.as_ptr(), params.depth, &mut budget, backing_scale) {
                    roots.push(n);
                }
            }
        }

        let nodes = match params.role.as_deref().filter(|r| !r.is_empty()) {
            Some(role) => roots
                .into_iter()
                .filter_map(|n| prune_by_role(n, role))
                .collect(),
            None => roots,
        };

        Ok(A11yTree {
            nodes,
            truncated: budget.truncated,
        })
    }

    pub(super) async fn get_a11y_tree(params: A11yParams) -> Result<A11yTree, CapError> {
        tokio::task::spawn_blocking(move || build_tree_sync(params))
            .await
            .map_err(|e| CapError::Unavailable(format!("macos a11y task join failed: {e}")))?
    }
}

// ===== 其余平台：返回 E_UNSUPPORTED =====

#[cfg(not(any(windows, target_os = "linux", target_os = "macos")))]
mod backend {
    use aura_capability::{A11yParams, A11yTree, CapError};

    pub(super) async fn get_a11y_tree(_params: A11yParams) -> Result<A11yTree, CapError> {
        Err(CapError::Unsupported(
            "get_a11y_tree is not supported on this platform".to_string(),
        ))
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// role 剪枝保留匹配节点及其祖先路径；无匹配的旁支被剪除。
    #[test]
    fn prune_by_role_keeps_matching_subtree() {
        let leaf_match = A11yNode {
            role: "button".to_string(),
            name: "OK".to_string(),
            bounds: None,
            value: None,
            children: vec![],
        };
        let leaf_other = A11yNode {
            role: "text".to_string(),
            name: "label".to_string(),
            bounds: None,
            value: None,
            children: vec![],
        };
        let root = A11yNode {
            role: "window".to_string(),
            name: "app".to_string(),
            bounds: None,
            value: None,
            children: vec![leaf_match, leaf_other],
        };
        let pruned = prune_by_role(root, "button").expect("ancestor retained for matching descendant");
        // window（祖先）保留，其下仅剩匹配的 button，text 旁支被剪除。
        assert_eq!(pruned.role, "window");
        assert_eq!(pruned.children.len(), 1);
        assert_eq!(pruned.children[0].role, "button");
    }

    /// Linux AT-SPI extents 面积守卫：正面积过、零/负面积拒（幽灵框不入 IoU）、负坐标
    /// （离屏/多显示器）不影响判定。Component 缺失/调用失败的降级（bounds 保 None 不 panic）
    /// 由 component_bounds 的 `.ok()?` 链承载，无 panic 路径；真总线提取正确性由 Selkies e2e 覆盖。
    #[cfg(target_os = "linux")]
    #[test]
    fn extents_to_bounds_guards_zero_or_negative_area() {
        use super::backend::extents_to_bounds;
        assert_eq!(extents_to_bounds((10, 20, 300, 200)), Some([10, 20, 300, 200]));
        assert_eq!(extents_to_bounds((-5, -7, 40, 30)), Some([-5, -7, 40, 30]));
        assert_eq!(extents_to_bounds((0, 0, 0, 50)), None);
        assert_eq!(extents_to_bounds((0, 0, 50, 0)), None);
        assert_eq!(extents_to_bounds((0, 0, -10, 50)), None);
        assert_eq!(extents_to_bounds((0, 0, 50, -10)), None);
    }

    /// macOS AX 角色名归一与逻辑点→原生物理像素折算闭环单测（全平台离线可测）。
    #[test]
    fn macos_ax_role_and_bounds_scaling_invariants() {
        assert_eq!(normalize_ax_role("AXButton"), "button");
        assert_eq!(normalize_ax_role("AXStaticText"), "text");
        assert_eq!(normalize_ax_role("AXTextField"), "textbox");
        assert_eq!(normalize_ax_role("AXSearchField"), "searchbox");
        assert_eq!(normalize_ax_role("AXPopUpButton"), "combobox");
        assert_eq!(normalize_ax_role("AXCheckBox"), "checkbox");
        assert_eq!(normalize_ax_role("AXWindow"), "window");
        assert_eq!(normalize_ax_role(""), "unknown");

        // Retina 2.0x：逻辑点 (100, 200, 80, 30) -> 原生物理像素 [200, 400, 160, 60]
        assert_eq!(
            ax_rect_to_bounds(100.0, 200.0, 80.0, 30.0, 2.0),
            Some([200, 400, 160, 60])
        );
        // 零/负宽高或 NaN 拒入（防幽灵框污染 IoU）
        assert_eq!(ax_rect_to_bounds(10.0, 20.0, 0.0, 30.0, 2.0), None);
        assert_eq!(ax_rect_to_bounds(10.0, 20.0, 40.0, -5.0, 2.0), None);
        assert_eq!(ax_rect_to_bounds(f64::NAN, 20.0, 40.0, 30.0, 2.0), None);
    }

    /// 无任何匹配 → 整棵剪空（返回 None）。
    #[test]
    fn prune_by_role_drops_when_no_match() {
        let root = A11yNode {
            role: "window".to_string(),
            name: "app".to_string(),
            bounds: None,
            value: None,
            children: vec![A11yNode {
                role: "text".to_string(),
                name: "x".to_string(),
                bounds: None,
                value: None,
                children: vec![],
            }],
        };
        assert!(prune_by_role(root, "button").is_none());
    }
}
