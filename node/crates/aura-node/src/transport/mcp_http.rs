//! Streamable HTTP 传输：单端点 /mcp，无状态 + JSON 直返（便于远程 MCP 客户端 / curl）。
//!
//! M13：叠加两道可选 axum 中间件——① 直连 agent 活动观测（有反连 sink 时挂，采集上报 controller）；
//! ② 访问令牌门槛（AURA_MCP_TOKEN 配置时挂，要求 Bearer）。两者均不改 MCP 协议面，纯外层拦截。

use std::net::SocketAddr;
use std::sync::Arc;

use anyhow::Result;
use rmcp::transport::streamable_http_server::session::local::LocalSessionManager;
use rmcp::transport::streamable_http_server::{StreamableHttpServerConfig, StreamableHttpService};

use super::agent_obs;
use super::AuraTools;

/// 以 Streamable HTTP 传输启动 MCP 服务，挂在 axum `/mcp` 端点。
///
/// `activity`：直连 agent 观测事件投递端（Some=经 grpc 反连上报 controller；None=纯 MCP 节点无反连，
/// 观测中间件不挂、行为与既往零变化）。
pub async fn serve(
    tools: AuraTools,
    bind: SocketAddr,
    activity: Option<agent_obs::ActivitySink>,
) -> Result<()> {
    // 无状态业务层（SUMMARY 决策 2）：stateful_mode=false + json_response=true，
    // 每个 POST 独立返回 application/json，天然兼容规范无状态演进、便于 curl 冒烟。
    // disable_allowed_hosts：AURA 节点设计为远程访问（0.0.0.0 监听），关闭回环 Host 白名单
    // （默认仅 loopback，会拒绝远端 Host），访问控制交由下方可选 AURA_MCP_TOKEN 门槛 / 反连通道。
    let config = StreamableHttpServerConfig::default()
        .with_stateful_mode(false)
        .with_json_response(true)
        .disable_allowed_hosts();

    let service: StreamableHttpService<AuraTools, LocalSessionManager> =
        StreamableHttpService::new(move || Ok(tools.clone()), Default::default(), config);

    let mut app = axum::Router::new().nest_service("/mcp", service);

    // 中间件挂载顺序：先加 observe（内层）、后加 require_token（外层）——令令牌门槛先于观测执行，
    // 被拒请求不计入 agent 活动（非真实接入）。axum `.layer` 后加者更外层。

    // ① 观测中间件（有反连 sink 时挂）：采集直连 agent 活动经反连流上报 controller。
    if let Some(sink) = activity {
        app = app.layer(axum::middleware::from_fn_with_state(sink, agent_obs::observe));
    }

    // ② 可选访问令牌门槛（AURA_MCP_TOKEN 配置且非空时挂）：/mcp 要求 `Authorization: Bearer <token>`；
    // 未配置保持现状开放接入（向后兼容，零行为变化）。
    if let Some(token) = std::env::var("AURA_MCP_TOKEN").ok().filter(|s| !s.is_empty()) {
        app = app.layer(axum::middleware::from_fn_with_state(
            Arc::new(token),
            agent_obs::require_token,
        ));
        tracing::info!("aura-node /mcp access token gate enabled (AURA_MCP_TOKEN set)");
    }

    let listener = tokio::net::TcpListener::bind(bind).await?;
    tracing::info!("aura-node http transport listening on http://{bind}/mcp");
    // ConnectInfo：观测中间件取 peer 地址需 into_make_service_with_connect_info 注入连接信息扩展。
    axum::serve(
        listener,
        app.into_make_service_with_connect_info::<SocketAddr>(),
    )
    .await?;
    Ok(())
}
