//! 直连 MCP agent 活动观测（M13）。
//!
//! 外部 coding agent（Claude Code / Codex / Gemini / OpenCode）直连节点 `/mcp`（Streamable HTTP）的
//! 活动，此前 controller/console 完全不可见（区别于经 controller 转发的 DispatchTool 审计路径）。本模块
//! 在 `/mcp` axum 层加一道观测中间件：采集每次 POST（仅 POST——GET/DELETE 在无状态模式恒 405，属
//! 客户端探测非调用）的 JSON-RPC 方法 / 工具名 / 客户端标识（initialize clientInfo）/ 时延 /
//! 传输层结果 / peer，经有界通道投递给 grpc 反连侧 drainer 批量上报 controller。
//!
//! 设计纪律：观测与工具执行链正交、best-effort——通道满即丢弃，绝不阻塞 MCP 请求；无反连（纯 MCP 节点）
//! 时 sink 为 None、中间件不挂，行为与既往零变化。请求体仅在 content-length ≤ 上限时缓冲细析，超限/无
//! content-length 原样透传（不破坏大请求，记粗粒度事件）。
//!
//! 另附可选访问令牌门槛（AURA_MCP_TOKEN）：设置即要求 `/mcp` 请求带 `Authorization: Bearer <token>`，
//! 未设保持现状开放接入（向后兼容）。生产节点监听 0.0.0.0 时可据此收口直连面。

use std::net::SocketAddr;
use std::sync::Arc;
use std::time::{Instant, SystemTime, UNIX_EPOCH};

use axum::body::Body;
use axum::extract::{ConnectInfo, Request, State};
use axum::http::{header, Method, StatusCode};
use axum::middleware::Next;
use axum::response::{IntoResponse, Response};
use tokio::sync::mpsc;

/// 单次 MCP 交互事件（中间件采集，grpc 反连侧 drainer 批量转 `pb::AgentCallEvent` 上报）。
/// 传输层结果 `ok`（HTTP 2xx）——工具层错误码（E_COORD_OOB 等）在响应体信封内、体积可含截图不细析，
/// v1 观测取传输层成功即可回答「接入是否通、调用是否在流动」；工具级成败经既有 tasks/traces 面。
#[derive(Clone, Debug)]
pub struct AgentEvent {
    pub peer: String,
    pub method: String,
    pub tool: String,
    pub client_name: String,
    pub client_version: String,
    pub protocol_version: String,
    pub duration_ms: i64,
    pub ok: bool,
    pub ts_unix_ms: i64,
}

/// 观测事件投递端（有界通道；满/无接收端即静默丢弃）。廉价 Clone（内含 `mpsc::Sender`），供中间件按请求持有。
#[derive(Clone)]
pub struct ActivitySink {
    tx: mpsc::Sender<AgentEvent>,
}

impl ActivitySink {
    /// 非阻塞投递（观测尽力而为——通道满即丢弃，绝不阻塞 MCP 请求处理路径）。
    fn record(&self, ev: AgentEvent) {
        let _ = self.tx.try_send(ev);
    }
}

/// 建观测通道：投递端注入中间件，接收端交 grpc 反连侧 drainer。cap 有界，防事件积压致内存无界增长。
/// `allow(dead_code)`：接收端 drainer 仅 grpc 门控编译，默认特性（无 grpc）`cargo check` 下本函数无调用方。
#[allow(dead_code)]
pub fn channel(cap: usize) -> (ActivitySink, mpsc::Receiver<AgentEvent>) {
    let (tx, rx) = mpsc::channel(cap);
    (ActivitySink { tx }, rx)
}

/// 请求体细析上限：超此不解析 JSON-RPC（file_push 大内容经直连 MCP 罕见），原样透传 + 记粗粒度事件
/// （method 空）——观测降级不阻断请求。
const MAX_INSPECT_BYTES: usize = 1024 * 1024;

/// `/mcp` 观测中间件（`from_fn_with_state` 注入 `ActivitySink`）。缓冲小请求体解析 JSON-RPC 提取
/// 方法/工具/客户端，计时执行内层服务，按传输层结果记事件。大体/无 content-length 请求不细析、原样透传。
///
/// 仅观测 POST：stateless Streamable HTTP 下 JSON-RPC 只经 POST 承载；客户端可选的 GET SSE 探测
/// （官方 TS SDK 在 initialized 后必发一次）与 DELETE 会话终止在无状态模式恒 405——若记为「失败调用」
/// 纯属噪声（每次接入虚增一条 ok=false，短会话错误率严重虚高）。非 POST 原样透传、不记事件。
pub async fn observe(
    State(sink): State<ActivitySink>,
    ConnectInfo(peer): ConnectInfo<SocketAddr>,
    request: Request,
    next: Next,
) -> Response {
    if request.method() != Method::POST {
        return next.run(request).await;
    }
    let (parts, body) = request.into_parts();
    let content_len = parts
        .headers
        .get(header::CONTENT_LENGTH)
        .and_then(|v| v.to_str().ok())
        .and_then(|s| s.parse::<usize>().ok());

    // 仅缓冲 content-length 明确且 ≤ 上限的请求体做解析；否则不消费 body、原样透传（绝不破坏大/流式请求）。
    let inspectable = matches!(content_len, Some(n) if n <= MAX_INSPECT_BYTES);
    let (meta, request) = if inspectable {
        match axum::body::to_bytes(body, MAX_INSPECT_BYTES).await {
            Ok(bytes) => {
                let meta = parse_jsonrpc(&bytes);
                (meta, Request::from_parts(parts, Body::from(bytes)))
            }
            // 理论不可达（已按 content-length 限长）；兜底以空体重建避免 panic，事件记空 meta。
            Err(_) => (RpcMeta::default(), Request::from_parts(parts, Body::empty())),
        }
    } else {
        (RpcMeta::default(), Request::from_parts(parts, body))
    };

    let start = Instant::now();
    let response = next.run(request).await;
    let ok = response.status().is_success();

    sink.record(AgentEvent {
        peer: peer.to_string(),
        method: meta.method,
        tool: meta.tool,
        client_name: meta.client_name,
        client_version: meta.client_version,
        protocol_version: meta.protocol_version,
        duration_ms: start.elapsed().as_millis() as i64,
        ok,
        ts_unix_ms: now_unix_ms(),
    });
    response
}

/// 从 JSON-RPC 请求体提取的观测元数据（解析失败/大体透传时全空）。
#[derive(Default)]
struct RpcMeta {
    method: String,
    tool: String,
    client_name: String,
    client_version: String,
    protocol_version: String,
}

/// 解析 JSON-RPC 请求体提取观测字段：`method` 恒取；`tools/call` 取 `params.name`；`initialize` 取
/// `clientInfo.name/version` + `protocolVersion`。非 JSON / 无 method 返回空 meta（事件仍记录，method 空）。
fn parse_jsonrpc(bytes: &[u8]) -> RpcMeta {
    let v: serde_json::Value = match serde_json::from_slice(bytes) {
        Ok(v) => v,
        Err(_) => return RpcMeta::default(),
    };
    let method = v
        .get("method")
        .and_then(|m| m.as_str())
        .unwrap_or_default()
        .to_string();
    let params = v.get("params");
    let mut meta = RpcMeta {
        method: method.clone(),
        ..Default::default()
    };
    match method.as_str() {
        "tools/call" => {
            meta.tool = params
                .and_then(|p| p.get("name"))
                .and_then(|n| n.as_str())
                .unwrap_or_default()
                .to_string();
        }
        "initialize" => {
            if let Some(p) = params {
                meta.protocol_version = p
                    .get("protocolVersion")
                    .and_then(|x| x.as_str())
                    .unwrap_or_default()
                    .to_string();
                if let Some(ci) = p.get("clientInfo") {
                    meta.client_name = ci
                        .get("name")
                        .and_then(|x| x.as_str())
                        .unwrap_or_default()
                        .to_string();
                    meta.client_version = ci
                        .get("version")
                        .and_then(|x| x.as_str())
                        .unwrap_or_default()
                        .to_string();
                }
            }
        }
        _ => {}
    }
    meta
}

/// 当前 Unix 毫秒时间戳（`AgentEvent.ts_unix_ms`）。
fn now_unix_ms() -> i64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.as_millis() as i64)
        .unwrap_or(0)
}

/// 可选访问令牌门槛中间件（`from_fn_with_state` 注入期望 token）。校验 `Authorization: Bearer <token>`，
/// 不匹配返回 401。仅当 `AURA_MCP_TOKEN` 配置时挂载（见 [`super::mcp_http::serve`]）；未配置则不挂、
/// 保持开放接入（向后兼容）。常量时间比较防时序侧信道（长度不等即拒，短路仅泄漏长度，可接受）。
pub async fn require_token(
    State(expected): State<Arc<String>>,
    request: Request,
    next: Next,
) -> Response {
    let provided = request
        .headers()
        .get(header::AUTHORIZATION)
        .and_then(|v| v.to_str().ok())
        .and_then(|s| s.strip_prefix("Bearer "));
    match provided {
        Some(tok) if constant_time_eq(tok.as_bytes(), expected.as_bytes()) => next.run(request).await,
        _ => (StatusCode::UNAUTHORIZED, "unauthorized").into_response(),
    }
}

/// 常量时间字节比较（token 校验不以内容短路，防时序侧信道；长度不等直接拒）。
fn constant_time_eq(a: &[u8], b: &[u8]) -> bool {
    if a.len() != b.len() {
        return false;
    }
    let mut diff = 0u8;
    for (x, y) in a.iter().zip(b.iter()) {
        diff |= x ^ y;
    }
    diff == 0
}

#[cfg(test)]
mod tests {
    use super::*;

    /// initialize 帧提取 clientInfo + protocolVersion。
    #[test]
    fn parse_initialize_extracts_client_info() {
        let body = br#"{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","clientInfo":{"name":"claude-code","version":"1.2.3"}}}"#;
        let m = parse_jsonrpc(body);
        assert_eq!(m.method, "initialize");
        assert_eq!(m.client_name, "claude-code");
        assert_eq!(m.client_version, "1.2.3");
        assert_eq!(m.protocol_version, "2025-06-18");
        assert!(m.tool.is_empty());
    }

    /// tools/call 帧提取工具名。
    #[test]
    fn parse_tools_call_extracts_tool() {
        let body = br#"{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"click","arguments":{"coordinate":[1,2]}}}"#;
        let m = parse_jsonrpc(body);
        assert_eq!(m.method, "tools/call");
        assert_eq!(m.tool, "click");
        assert!(m.client_name.is_empty());
    }

    /// 非 JSON 请求体返回空 meta（不 panic）。
    #[test]
    fn parse_non_json_yields_empty() {
        let m = parse_jsonrpc(b"not json at all");
        assert!(m.method.is_empty());
        assert!(m.tool.is_empty());
    }

    /// 常量时间比较：等值真、异值/异长假。
    #[test]
    fn constant_time_eq_matches() {
        assert!(constant_time_eq(b"secret", b"secret"));
        assert!(!constant_time_eq(b"secret", b"secreu"));
        assert!(!constant_time_eq(b"secret", b"sec"));
    }
}
