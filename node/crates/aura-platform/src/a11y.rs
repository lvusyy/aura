//! A11yDriver 平台实现：Windows UI Automation / Linux AT-SPI；macOS defer（E_UNSUPPORTED）。
//!
//! 采集后端只此一层分平台（`#[cfg]` 门控），capability 层保持平台无关：
//!   - Windows：`uiautomation`（IUIAutomation 的安全封装，复刻本层既有高层平台封装惯例
//!     windows-capture / xcap / enigo，将 COM 收敛在封装内）。UIAutomation COM 对象 `!Send`，
//!     故起一条独占 OS 线程持有之，异步方法经通道投递请求 / 收回执——复刻 M1 输入注入（enigo）
//!     专用线程先例（见 input.rs / ARCHITECTURE §3 决策 3）。
//!   - Linux：`atspi`（zbus AT-SPI2）。走 D-Bus 无障碍总线，天然异步，直接在 tokio 上 await。
//!   - macOS 等：明确 defer 至 M4（AXUIElement），返回 E_UNSUPPORTED。
//!
//! 过滤语义（Locked-6，默认浅层防树爆 R-4）：`depth` 限后代层数、`max_nodes` 限节点总数
//! （命中即 `truncated=true`）、`role` 剪枝保留匹配节点及其祖先路径、`root` 选子树根。
//! 跨平台节点统一形状 role/name/bounds/value/children（bounds/value 平台不可得时省略）。

use async_trait::async_trait;

use aura_capability::{A11yDriver, A11yParams, A11yTree, CapError};

use crate::PlatformDriver;

#[cfg(any(windows, target_os = "linux"))]
use aura_capability::A11yNode;

#[async_trait]
impl A11yDriver for PlatformDriver {
    async fn get_a11y_tree(&self, params: A11yParams) -> Result<A11yTree, CapError> {
        backend::get_a11y_tree(params).await
    }
}

// ===== 平台无关的遍历预算 / role 剪枝（win + linux 复用，mac stub 不需要）=====

/// 遍历预算：剩余可创建节点数 + 是否已截断（命中 depth / max_nodes 上限）。
#[cfg(any(windows, target_os = "linux"))]
struct Budget {
    remaining: u32,
    truncated: bool,
}

/// 按角色剪枝：保留角色匹配的节点及其祖先路径（有匹配后代的节点亦保留），其余剪除。
/// role 为 None / 空串时调用方跳过本函数（不过滤）。
#[cfg(any(windows, target_os = "linux"))]
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
        let children = root.find_all(TreeScope::Children, &cond).unwrap_or_default();
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

        let children = element.find_all(TreeScope::Children, cond).unwrap_or_default();
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
                None => return None,
            };
            let builder = AccessibleProxy::builder(conn.connection())
                .destination(obj_name)
                .and_then(|b| b.path(obj.path()));
            let proxy = match builder {
                Ok(b) => match b.cache_properties(CacheProperties::No).build().await {
                    Ok(p) => p,
                    Err(_) => return None,
                },
                Err(_) => return None,
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

            let children = proxy.get_children().await.unwrap_or_default();
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

// ===== 其余平台（macOS 等）：明确 defer，返回 E_UNSUPPORTED =====

#[cfg(not(any(windows, target_os = "linux")))]
mod backend {
    use aura_capability::{A11yParams, A11yTree, CapError};

    /// macOS 等：get_a11y_tree 明确 defer 至 M4（AXUIElement），返回 E_UNSUPPORTED 语义。
    pub(super) async fn get_a11y_tree(_params: A11yParams) -> Result<A11yTree, CapError> {
        Err(CapError::Unsupported(
            "get_a11y_tree is not supported on this platform (macOS a11y deferred to M4)".to_string(),
        ))
    }
}

#[cfg(test)]
mod tests {
    #[cfg(any(windows, target_os = "linux"))]
    use super::*;

    /// role 剪枝保留匹配节点及其祖先路径；无匹配的旁支被剪除。
    #[cfg(any(windows, target_os = "linux"))]
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

    /// 无任何匹配 → 整棵剪空（返回 None）。
    #[cfg(any(windows, target_os = "linux"))]
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
