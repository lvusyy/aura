//! 传输适配层：AuraTools（driver 薄适配）+ 三域工具子路由合并 + 双传输入口。

use std::collections::HashMap;
use std::path::PathBuf;
use std::sync::atomic::{AtomicU32, AtomicU64, Ordering};
use std::sync::{Arc, Mutex};

use rmcp::{tool_handler, ServerHandler};

use aura_capability::{CapError, Coordinate, DeviceDriver, Envelope, ErrObj};

use record_tools::RecordSession;

pub mod screen_tools;
pub mod input_tools;
pub mod proc_tools;
pub mod a11y_tools;
pub mod assert_tools;
pub mod record_tools;
pub mod audio_tools;
pub mod mcp_stdio;
pub mod mcp_http;

/// 直连 MCP agent 活动观测（M13，非门控）：`/mcp` axum 观测中间件 + 可选访问令牌门槛 + 事件通道。
/// 采集端（中间件）非门控恒编译；上报端（drainer 转 pb::AgentActivity 帧）在 grpc_reverse（grpc 门控）。
pub mod agent_obs;

/// 基础设施形态探测（M12 批D）：runtime_kind/infra_host 宿主事实采集（Register 自报，fleet 定位）。
/// 平台无关无门控（探测子进程/文件读全 best-effort），grpc_reverse::run() 启动时一次消费。
pub mod infra;

/// gRPC 反连传输（M2，feature grpc 门控）：节点主动拨出、mTLS 双向流、复用工具执行核。
/// M1 default 关，tonic/prost 零依赖入树。
#[cfg(feature = "grpc")]
pub mod grpc_reverse;

/// gRPC 反连工具分发单一注册表（feature grpc 门控）：TOOL_NAMES 单一源 + dispatch 派发 +
/// MCP ToolRouter 集合相等断言。新原语经此单点加（1 free fn + 1 注册表项），MCP/gRPC 同源。
#[cfg(feature = "grpc")]
pub mod tool_dispatch;

/// 大产物旁路上传 HTTP 客户端（feature grpc 门控，G-5）：收 UploadUrlGrant 后经预签名 PUT URL
/// 直连对象存储上传，绕开双向流 16MB 内联上限。deps 随 grpc feature（hyper-util 链）入树。
#[cfg(feature = "grpc")]
pub mod upload;

/// 设备接入 enroll/renew 客户端（feature enroll 门控，M12 TASK-006）：一次性 CLI 生成密钥对+CSR
/// （私钥不离节点）换 per-node 证书。rcgen 产 CSR + reqwest HTTPS 换证，随 enroll feature 入树。
/// 与 grpc 正交（enroll 是反连前置的 bootstrap；grpc 是换证后的反连服务面）——生产节点两 feature 并开。
#[cfg(feature = "enroll")]
pub mod enroll;

/// 节点契约版本（**单点真源**，格式 Free）。双面同源暴露（Locked-7）：
/// - local 面：下方 `ServerHandler::get_info` override 的 `server_info.version`；
/// - fleet 面：gRPC 反连 `Register.contract_version` 上报（`grpc_reverse.rs`，供 controller
///   `NodeInfo.contract_version` 回填 + 版本偏斜告警，消费在 TASK-005）。
/// 定义于本**非门控**模块而非 grpc 门控的 `tool_dispatch`：`get_info` 恒编译（MCP 无 grpc 亦在），
/// 常量若随 `tool_dispatch` 门控则 default（无 grpc）构建引用不到；`tool_dispatch` 经
/// `pub(crate) use super::CONTRACT_VERSION` 再导出供 gRPC 面引用，保「全仓单一 const 定义」。
pub(crate) const CONTRACT_VERSION: &str = "aura.v1/2026-07";

/// MCP 工具集适配器：持有能力层 driver 与当前显示器状态，是 driver 的薄适配（thin adapter）。
///
/// 三域工具经各自 `#[tool_router]` 子路由（screen_tools/input_tools/proc_tools）挂载，
/// 最终在此合并进单一 `ServerHandler`。`Clone` 廉价（仅克隆三个 Arc），供 HTTP 传输按请求工厂复用、
/// 共享同一 driver、current_display 与 current_scale。
#[derive(Clone)]
pub struct AuraTools {
    pub driver: Arc<dyn DeviceDriver>,
    pub current_display: Arc<AtomicU32>,
    /// 最近一次 screenshot 的 display→native 缩放系数（f64 位模式存于 AtomicU64）。
    /// screenshot 成功后写入本次 `meta.scale`；input 域坐标（click/drag/move_mouse/scroll 锚点）
    /// 乘此系数由 display 空间回映射到原生像素再执行。默认 1.0（未截图时等同原生坐标，安全）。
    pub current_scale: Arc<AtomicU64>,
    /// 活动录屏会话态（rec_id → 会话元数据，TASK-010）。start_recording 登记、stop_recording 取出，
    /// 承载对象存储 key/落盘路径 供 resource_link 构造。平台层持采集/编码态，本表仅节点侧映射。
    /// `Clone` 时共享同一 Arc（多传输/多请求并发安全，Mutex 串行化会话增删）。
    /// pub(crate)：会话态为节点内部实现（RecordSession 亦 pub(crate)），非对外 API 面。
    pub(crate) recordings: Arc<Mutex<HashMap<String, RecordSession>>>,
    /// 节点数据目录（`<data_dir>/artifacts/<key>` 为产物落盘根）。由 `--data-dir` CLI 参数经
    /// [`AuraTools::with_data_dir`] 注入，与 gRPC 反连的 [`grpc_reverse::ReverseConfig::data_dir`]
    /// 同源，保证录屏落盘路径（record_tools）与旁路上传（grant 处理器）读取路径一致；未注入时
    /// 回退 [`default_data_dir`]（env 解析，纯 MCP/无反连场景，M1 行为）。
    pub(crate) data_dir: PathBuf,
}

impl AuraTools {
    pub fn new(driver: Arc<dyn DeviceDriver>) -> Self {
        Self {
            driver,
            current_display: Arc::new(AtomicU32::new(0)),
            current_scale: Arc::new(AtomicU64::new(1.0_f64.to_bits())),
            recordings: Arc::new(Mutex::new(HashMap::new())),
            data_dir: default_data_dir(),
        }
    }

    /// 注入节点数据目录（`--data-dir` CLI 参数解析而来），覆盖 [`AuraTools::new`] 的 env 缺省。
    /// 与 gRPC 反连配置 [`grpc_reverse::ReverseConfig::data_dir`] 同源：main.rs 装配反连时以同一
    /// data_dir 注入，使录屏落盘（record_tools）与旁路上传读取（grpc_reverse）解析到同一
    /// `<data_dir>/artifacts`，消除既往「双解析漂移」隐患。
    pub(crate) fn with_data_dir(mut self, data_dir: PathBuf) -> Self {
        self.data_dir = data_dir;
        self
    }

    /// 记录最近一次截图的缩放系数（display→native 回映射用）。
    /// 仅接受正有限值，异常值忽略以免污染后续坐标映射。
    pub(crate) fn set_scale(&self, scale: f64) {
        if scale.is_finite() && scale > 0.0 {
            self.current_scale.store(scale.to_bits(), Ordering::Relaxed);
        }
    }

    /// 当前缩放系数（默认 1.0）。
    pub(crate) fn scale(&self) -> f64 {
        f64::from_bits(self.current_scale.load(Ordering::Relaxed))
    }

    /// display 空间坐标 → 原生像素坐标（按当前 scale 回映射）。
    /// 模型按 display（缩放后）坐标操作，input 执行前须回映射到原生像素。
    pub(crate) fn to_native(&self, x: i32, y: i32) -> Coordinate {
        let s = self.scale();
        let native = Coordinate {
            x: scale_coord(x, s),
            y: scale_coord(y, s),
        };
        // debug 级观察点（默认静默、走 stderr）：验证坐标已按 scale 回映射。
        tracing::debug!(
            display_x = x,
            display_y = y,
            scale = s,
            native_x = native.x,
            native_y = native.y,
            "input coordinate mapped display->native"
        );
        native
    }

    /// 工具边界统一封装：执行 driver future，捕获 panic，将 `Result<T, CapError>` 收敛为
    /// `{ok, data, error}` 信封。全部工具 body 统一经此产出 Envelope
    /// （TASK-007 审计 panic/错误边界，不重写此逻辑）。
    ///
    /// `#[instrument]` 为每次工具执行开 per-tool span `tool.exec`（MCP/反连两路统一覆盖）。反连路
    /// 该 span 嵌套于 grpc_reverse 的 `tool.request{task_id}` 之下——令 node span 挂上控制面 task_id；
    /// otel 层启用时经 OTLP 上报，未启用则仅为 stderr 结构化上下文（不产生新输出，M1/M2 行为不变）。
    #[tracing::instrument(skip_all, name = "tool.exec")]
    pub(crate) async fn guard<T, F>(&self, fut: F) -> Envelope<T>
    where
        F: std::future::Future<Output = Result<T, CapError>>,
    {
        use futures::future::FutureExt;
        use std::panic::AssertUnwindSafe;

        match AssertUnwindSafe(fut).catch_unwind().await {
            Ok(Ok(data)) => Envelope::ok(data),
            Ok(Err(e)) => Envelope::error(ErrObj::from(e)),
            Err(_) => Envelope::error(ErrObj {
                code: "E_INTERNAL".to_string(),
                message: "tool handler panicked".to_string(),
            }),
        }
    }
}

/// display 空间坐标分量 → 原生像素（乘 scale 四舍五入）。
/// 纯映射逻辑抽出为自由函数，便于单测直接覆盖（无需构造 driver）。
pub(crate) fn scale_coord(v: i32, scale: f64) -> i32 {
    (v as f64 * scale).round() as i32
}

/// 节点默认数据目录：`$AURA_DATA_DIR` 优先，否则 `~/.aura`（Unix `$HOME` / Windows `%USERPROFILE%`）。
/// 与 gRPC 反连 [`grpc_reverse::ReverseConfig`] 的 data_dir 解析同一语义，作为 [`AuraTools::new`] 未显式
/// 注入 `--data-dir` 时的兜底（纯 MCP 传输、无反连场景）。无 home 环境变量时退回相对 `.aura`（best-effort）。
pub(crate) fn default_data_dir() -> PathBuf {
    if let Some(dir) = std::env::var_os("AURA_DATA_DIR") {
        return PathBuf::from(dir);
    }
    std::env::var_os("HOME")
        .or_else(|| std::env::var_os("USERPROFILE"))
        .map(|home| PathBuf::from(home).join(".aura"))
        .unwrap_or_else(|| PathBuf::from(".aura"))
}

/// 高危工具 `_meta`：标注 `anthropic/requiresUserInteraction=true`。
/// 即便客户端处于 bypass 模式也强制人工确认（RBAC 埋点，M2 补细粒度 scope）。
/// 由 kill_process / run_command 的 `#[tool(meta = ...)]` 注入 tools/list。
pub(crate) fn requires_user_interaction_meta() -> rmcp::model::Meta {
    let mut meta = rmcp::model::Meta::new();
    meta.insert(
        "anthropic/requiresUserInteraction".to_string(),
        serde_json::Value::Bool(true),
    );
    meta
}

#[cfg(test)]
mod tests {
    use super::*;

    /// Task B 回映射：display 空间坐标 × scale → 原生像素。
    #[test]
    fn scale_coord_maps_display_to_native() {
        // display 640 × scale 1.148 = 734.72 → 四舍五入 735（截图缩放回映射）。
        assert_eq!(scale_coord(640, 1.148), 735);
        // scale=1.0（未截图缺省）等同原生坐标，坐标不变。
        assert_eq!(scale_coord(640, 1.0), 640);
        // 原点在任意 scale 下仍为原点。
        assert_eq!(scale_coord(0, 1.148), 0);
    }

    /// scale 以 f64 位模式存于 AtomicU64，往返无损。
    #[test]
    fn scale_bits_roundtrip_preserves_value() {
        let s = 1.148_f64;
        assert_eq!(f64::from_bits(s.to_bits()), s);
    }

    /// requires_user_interaction_meta 注入 anthropic/requiresUserInteraction=true。
    #[test]
    fn requires_user_interaction_meta_sets_flag() {
        let meta = requires_user_interaction_meta();
        assert_eq!(
            meta.get("anthropic/requiresUserInteraction"),
            Some(&serde_json::Value::Bool(true))
        );
    }
}

// 七 sub-router 合并进单一 ServerHandler。
// 各 *_tools.rs 以 #[tool_router(router = <域>_router, vis = "pub(crate)")] 生成关联函数
// Self::<域>_router() -> ToolRouter<Self>；此处用 `+`（ToolRouter 实现 Add）合并各域，
// 由 #[tool_handler] 生成 call_tool / list_tools / get_tool / get_info（enable_tools）。
// 注意：整个合并表达式必须加外层括号。#[tool_handler] 会把 router 表达式原样拼进
// `#router.call(tcc)` / `#router.list_all()` / `#router.get(name)`，方法调用优先级高于 `+`，
// 不加括号会解析成 `a + b + c.call(tcc)`（末项变 Result）导致类型不匹配。
#[tool_handler(router = (Self::screen_router() + Self::input_router() + Self::proc_router() + Self::a11y_router() + Self::assert_router() + Self::record_router() + Self::audio_router()))]
impl ServerHandler for AuraTools {
    /// 契约版本 local 面（Locked-7）：手写 `get_info` 暴露 `server_info.version = CONTRACT_VERSION`
    /// （单点真源）+ instructions。`#[tool_handler]` 经 `has_method` 检出本手写 `get_info` 即跳过自动
    /// 生成（仅补 `call_tool`/`get_tool`），故与宏共存零冲突。`enable_tools()` 与宏默认一致——工具能力
    /// 广告不回退，M1 initialize 响应零破坏。
    fn get_info(&self) -> rmcp::model::ServerInfo {
        rmcp::model::ServerInfo::new(
            rmcp::model::ServerCapabilities::builder().enable_tools().build(),
        )
        .with_server_info(rmcp::model::Implementation::new("aura-node", CONTRACT_VERSION))
        .with_instructions(
            "AURA remote-control node: screen, input, process/file, a11y, assert and recording tools (contract aura.v1).".to_string(),
        )
    }

    /// 能力子集三面之一——MCP 面（Locked-6 / D5）：手写 `list_tools` 按 `driver.supports_tool` 过滤
    /// 七 sub-router 合并的全 21 工具广告面，与 `Register.tools`（grpc_reverse.rs）**同源同谓词**过滤，
    /// 保三面一致（linux desktop 全 21 / win·mac desktop 20（剔 audio_inject，plan D6）/ android 17 /
    /// ios 12）。`#[tool_handler]` 经 `has_method` 检出本手写 `list_tools` 即跳过自动生成；
    /// `call_tool`/`get_tool` 仍由宏补——**dispatch/校验超集网不动**，被剔工具 call-time
    /// 走 `E_UNSUPPORTED`/`E_NO_AUDIO_DEV`，仅广告面收窄。合并表达式与宏 `router` 参数同构（七 sub-router，外层括号见宏注释）。
    async fn list_tools(
        &self,
        _request: Option<rmcp::model::PaginatedRequestParams>,
        _context: rmcp::service::RequestContext<rmcp::RoleServer>,
    ) -> Result<rmcp::model::ListToolsResult, rmcp::ErrorData> {
        let router = Self::screen_router()
            + Self::input_router()
            + Self::proc_router()
            + Self::a11y_router()
            + Self::assert_router()
            + Self::record_router()
            + Self::audio_router();
        let tools = router
            .list_all()
            .into_iter()
            .filter(|t| self.driver.supports_tool(t.name.as_ref()))
            .collect();
        Ok(rmcp::model::ListToolsResult {
            tools,
            next_cursor: None,
            meta: None,
        })
    }
}
