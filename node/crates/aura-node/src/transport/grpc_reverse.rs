//! gRPC 反连传输（M2，feature grpc 门控）。
//!
//! 节点主动拨出、建立到控制面 aura-controller 的 mTLS gRPC 双向流（假设节点在 NAT/防火墙后，
//! 控制面从不回连）：首帧 Register 上报身份 + 工具清单 → 收 RegisterAck 持久化控制面分配的 node_id
//! → 循环收 ToolRequest 复用 MCP 侧同一工具执行核执行、回 ToolResponse → 周期 Heartbeat。
//! 断流后指数退避 + 抖动重连并携已分配 node_id 重注册，永不退出。
//!
//! 哑管道：gRPC 层只透传 JSON 信封（json_args / json_envelope 为 bytes 原样搬运），
//! 工具语义契约由 MCP JSON schema 单一承载，工具分发经 [`super::tool_dispatch`] 单一注册表
//! （TOOL_NAMES 单一源 + dispatch），本模块只做超时/连接/重连编排。

use std::net::{IpAddr, SocketAddr};
use std::path::{Path, PathBuf};
use std::sync::{Arc, Mutex};
use std::time::{Duration, SystemTime, UNIX_EPOCH};

use anyhow::{anyhow, Result};
use futures::channel::mpsc;
use futures::{SinkExt, StreamExt};
use tracing::Instrument;

// 工具名清单与派发核收敛进 tool_dispatch 单一注册表（单一源）：本模块只引用不再手写。
use super::tool_dispatch::{dispatch, error_envelope, CONTRACT_VERSION, TOOL_NAMES};
use super::agent_obs;
use super::AuraTools;

use pb::node_control_client::NodeControlClient;
use pb::{
    controller_to_node, node_to_controller, AgentActivity, AgentCallEvent, Heartbeat,
    NodeToController, Register, ToolResponse, UploadComplete, UploadFailed,
};
use hyper_util::client::legacy::connect::HttpConnector;
use tonic::transport::{Certificate, Channel, ClientTlsConfig, Endpoint, Identity};

/// tonic-build 从 aura.v1 proto 生成的类型（消息 + NodeControl client）。
/// 生成代码含 ControllerAdmin client 等节点未用项，`allow(dead_code)` 抑制 bin crate 死代码告警。
pub mod pb {
    #![allow(clippy::all, dead_code)]
    tonic::include_proto!("aura.v1");
}

// 工具名清单 TOOL_NAMES 与 gRPC 派发 dispatch 已收敛进 `super::tool_dispatch` 单一注册表
// （单一源：Register 上报与派发同源，编译期断言测试保证与 MCP 集合相等，杜绝静默 E_UNSUPPORTED）。

// ===== CLI 选项与运行配置 =====

/// gRPC 反连 CLI 选项（clap flatten 进顶层 Cli）。
/// 设 `--controller` 即启用反连，未设则不启动反连、行为与 M1 完全一致。
#[derive(clap::Args, Clone, Debug)]
pub struct ReverseOpts {
    /// 控制面地址 host:port（设置即启用 gRPC 反连；未设置则行为同 M1）。
    /// ca/cert/key/tls_domain 的必填性由 into_config 校验（fail fast），不用 clap requires_all。
    #[arg(long)]
    pub controller: Option<String>,
    /// mTLS CA 证书路径（PEM）。
    #[arg(long)]
    pub ca: Option<PathBuf>,
    /// mTLS 客户端证书路径（PEM）。
    #[arg(long)]
    pub cert: Option<PathBuf>,
    /// mTLS 客户端私钥路径（PEM）。
    #[arg(long)]
    pub key: Option<PathBuf>,
    /// 服务端 cert SAN 对齐的域名（TLS 校验用）。
    #[arg(long)]
    pub tls_domain: Option<String>,
    /// 一次性 bootstrap/enrollment token（随 Register 上报）。
    #[arg(long)]
    pub token: Option<String>,
    /// 用户标签（M12 舰队分组/筛选）：随 Register 上报作 label 引导值；缺省空。console 编辑为权威源
    /// （UpdateNodeMeta 改后重连不覆盖——controller 仅首注册取本引导值）。
    #[arg(long)]
    pub label: Option<String>,
    /// 用户位置（M12 舰队展示）：随 Register 上报作 location 引导值；缺省空。console 编辑权威（同 label）。
    #[arg(long)]
    pub location: Option<String>,
    /// 网络域显式标签（M12/T07 presigned 分派）：设置即跳过 AURA_MINIO_PROBE TCP 探测，直接以此值上报
    /// network_zone（controller 按域签发 presigned 端点）。未设则回落探测（AURA_MINIO_PROBE 候选可达域）。
    #[arg(long)]
    pub network_domain: Option<String>,
    /// 节点数据目录（node_id 持久化于 <data-dir>/node_id）；默认 $AURA_DATA_DIR 或 ~/.aura。
    #[arg(long)]
    pub data_dir: Option<PathBuf>,
    /// 出站源 IP 绑定（多宿主机确定性选源，SC-1）：gRPC 反连与旁路上传两建连点同时生效；
    /// 未设置则由内核路由选源（行为与既往完全一致）。
    #[arg(long)]
    pub local_addr: Option<IpAddr>,
}

/// 反连运行配置（`--controller` 存在时由 [`ReverseOpts`] 解析而来）。
pub struct ReverseConfig {
    /// 控制面地址 host:port。
    pub controller: String,
    /// mTLS CA / 客户端证书 / 私钥路径。
    pub ca: PathBuf,
    pub cert: PathBuf,
    pub key: PathBuf,
    /// 服务端 cert SAN 域名。
    pub tls_domain: String,
    /// bootstrap token（可空）。
    pub token: Option<String>,
    /// 用户标签/位置（M12，随 Register 上报作引导值；缺省 None → 上报空串，console 编辑权威）。
    pub label: Option<String>,
    pub location: Option<String>,
    /// 网络域显式标签（--network-domain；None=回落 AURA_MINIO_PROBE 探测可达域，T07 presigned 分派）。
    pub network_domain: Option<String>,
    /// node_id 持久化文件路径。
    pub node_id_path: PathBuf,
    /// h2 keepalive 间隔（≥30s）。
    pub keepalive: Duration,
    /// 应用层心跳周期。
    pub heartbeat: Duration,
    /// gRPC 编解码消息上限（字节）。
    pub max_msg_bytes: usize,
    /// 出站源 IP 绑定（--local-addr；None=内核路由选源，行为与既往一致）。
    pub local_addr: Option<IpAddr>,
}

impl ReverseOpts {
    /// 设置了 `--controller` 则解析为反连配置，否则 `None`（纯 M1，不启动反连）。
    /// controller 存在时 ca/cert/key/tls_domain 均为必填，缺任一即返回 Err（fail fast）。
    pub fn into_config(self) -> Result<Option<ReverseConfig>> {
        let Some(controller) = self.controller else {
            return Ok(None);
        };
        let ca = self.ca.ok_or_else(|| anyhow!("--ca is required with --controller"))?;
        let cert = self
            .cert
            .ok_or_else(|| anyhow!("--cert is required with --controller"))?;
        let key = self
            .key
            .ok_or_else(|| anyhow!("--key is required with --controller"))?;
        let tls_domain = self
            .tls_domain
            .ok_or_else(|| anyhow!("--tls-domain is required with --controller"))?;
        let data_dir = match self.data_dir {
            Some(d) => d,
            None => default_data_dir()
                .ok_or_else(|| anyhow!("cannot resolve data dir; pass --data-dir"))?,
        };
        Ok(Some(ReverseConfig {
            controller,
            ca,
            cert,
            key,
            tls_domain,
            token: self.token,
            label: self.label,
            location: self.location,
            network_domain: self.network_domain,
            node_id_path: data_dir.join("node_id"),
            // Locked：keepalive≥30s + keep_alive_while_idle；心跳 15s；消息上限 16MB。
            keepalive: Duration::from_secs(30),
            heartbeat: Duration::from_secs(15),
            max_msg_bytes: 16 * 1024 * 1024,
            local_addr: self.local_addr,
        }))
    }
}

/// 默认节点数据目录：`$AURA_DATA_DIR` 优先，否则 `~/.aura`
/// （Unix `$HOME` / Windows `%USERPROFILE%`）。不引第三方 crate，跨平台读环境变量解析 home。
fn default_data_dir() -> Option<PathBuf> {
    if let Some(dir) = std::env::var_os("AURA_DATA_DIR") {
        return Some(PathBuf::from(dir));
    }
    std::env::var_os("HOME")
        .or_else(|| std::env::var_os("USERPROFILE"))
        .map(|home| PathBuf::from(home).join(".aura"))
}

impl ReverseConfig {
    /// 节点数据目录（node_id 持久化路径 `<data_dir>/node_id` 上溯一级）。产物暂存/落盘均以此为根。
    /// 与 `AuraTools` 注入的 data_dir 同源（main.rs 以本值注入 `AuraTools::with_data_dir`），保证
    /// 录屏落盘路径（record_tools）与旁路上传读取路径一致。
    pub fn data_dir(&self) -> PathBuf {
        self.node_id_path
            .parent()
            .map(Path::to_path_buf)
            .unwrap_or_else(|| PathBuf::from("."))
    }
}

/// 旁路上传产物暂存路径：`<data_dir>/artifacts/<key>`（data_dir 见 `ReverseConfig::data_dir`）。
/// 与 record_tools 的 `AuraTools::artifact_output_path` 同源：录屏落盘处即本处 PUT 读取处，
/// 由 main.rs 以同一 data_dir 注入 AuraTools 保证（消除既往双解析漂移）。
fn artifact_staging_path(cfg: &ReverseConfig, key: &str) -> PathBuf {
    cfg.data_dir().join("artifacts").join(key)
}

// ===== 反连主循环 =====

/// gRPC 反连主循环：拨出 → 注册 → 收发工具调用 → 断流退避重连（永不退出）。
///
/// node_id 跨重连共享：首次为空 → RegisterAck 分配后持久化 → 重连复用（禁机器指纹）。
/// 曾成功建流则重置退避；反复失败则退避指数增长至上限、叠加抖动打散重连时序。
///
/// `activity_rx`：直连 MCP agent 观测事件接收端（M13，Some=http 传输并存时由 mcp_http 中间件投递）。
/// run 内 spawn 一个跨重连长驻 drainer：批量转 `AgentActivity` 帧、经「当前活跃反连出站」共享句柄
/// （`uplink`）投递给控制面。drainer 生命周期独立于单次连接——重连时 connect_once 更新 uplink 指向新出站。
pub async fn run(
    tools: AuraTools,
    cfg: ReverseConfig,
    activity_rx: Option<tokio::sync::mpsc::Receiver<agent_obs::AgentEvent>>,
) {
    let node_id = Arc::new(Mutex::new(load_node_id(&cfg.node_id_path)));

    // 当前活跃反连的出站发送端共享句柄（M13 观测上报）：connect_once 建连时置 Some(tx.clone())、断连清 None。
    // drainer 据此把 agent 活动帧投给当下连接；无连接期投递丢弃（best-effort，有界不积压）。
    let uplink: Arc<Mutex<Option<mpsc::Sender<NodeToController>>>> = Arc::new(Mutex::new(None));
    if let Some(rx) = activity_rx {
        let uplink_drain = uplink.clone();
        tokio::spawn(async move { drain_activity(rx, uplink_drain).await });
    }

    // 网络域探测（T07/REC-6）：启动时一次解析——`--network-domain` 显式标签优先，否则 AURA_MINIO_PROBE
    // 候选 TCP dial 探测首个可达域（lan 优先 jump 兜底）。节点网段固定，探测一次跨重连复用（免每次重连
    // 重付 dial 延迟）；空=未配置/均不可达→上报空 zone，controller 回落默认端点（兼容既有单端点部署）。
    let network_zone = resolve_network_zone(&cfg).await;
    if network_zone.is_empty() {
        tracing::debug!("no network zone resolved; controller will presign against default endpoint");
    } else {
        tracing::info!(zone = %network_zone, "resolved network zone for presigned dispatch");
    }

    // M12 批B：系统辨识 os_version 启动时一次采集（宿主/设备 OS 稳定，跨重连复用免每次 spawn 子进程；
    // 区别于每次重连现采的 ip_address——ip 可能随网络变化，取即时值）。空=采集失败/未知平台（前端不显）。
    // M12 批C：driver 覆写优先（platform() 同款派生模式）——AndroidDriver 经 adb getprop 报被控设备版本
    // （Some，含失败显式空，不回落宿主值错报）；桌面驱动默认 None → 回落既有宿主采集，行为零漂移。
    let os_version = match tools.driver.os_version().await {
        Some(v) => v,
        None => node_os_version(),
    };
    // M12 批D：基础设施标注启动时一次采集（形态/宿主稳定同 os_version 口径）。runtime_kind/infra_host
    // 宿主事实走 infra 探测；attached 派生服务标识 env AURA_ATTACHED 显式声明优先（部署面权威），
    // 否则 driver 配置拼装（AndroidDriver=redroid@serial），桌面驱动默认空=自身即设备。
    let infra = super::infra::detect(&node_hostname());
    let attached = std::env::var("AURA_ATTACHED")
        .ok()
        .filter(|v| !v.is_empty())
        .or_else(|| tools.driver.attached())
        .unwrap_or_default();
    tracing::info!(
        os_version = %os_version,
        runtime_kind = %infra.runtime_kind,
        infra_host = %infra.infra_host,
        attached = %attached,
        "collected node identity facts for fleet display"
    );
    let reported = SelfReported {
        network_zone,
        os_version,
        runtime_kind: infra.runtime_kind,
        infra_host: infra.infra_host,
        attached,
    };

    let initial_backoff = Duration::from_secs(1);
    let max_backoff = Duration::from_secs(30);
    let mut backoff = initial_backoff;

    loop {
        match connect_once(&tools, &cfg, &node_id, &reported, &uplink).await {
            Ok(()) => {
                tracing::warn!("reverse stream closed, reconnecting");
                backoff = initial_backoff;
            }
            Err(e) => {
                // {:#} 单行展开 anyhow cause 链（F3）：仅顶层 Display 会吞掉根因（如 TLS/IO 底层错误）。
                tracing::warn!(error = %format!("{:#}", e), "reverse connection failed, backing off");
            }
        }
        let delay = backoff + jitter_for(backoff);
        tokio::time::sleep(delay).await;
        backoff = (backoff * 2).min(max_backoff);
    }
}

/// 启动时一次采集、跨重连复用的自报事实（Register 帧组装消费；ip_address 除外——每次重连现采）。
/// 聚合替代散参：network_zone（T07 探测）/os_version（批B/C driver 优先）/runtime_kind+infra_host
/// （批D infra 探测）/attached（批D env 优先 driver 回落）。
struct SelfReported {
    network_zone: String,
    os_version: String,
    runtime_kind: String,
    infra_host: String,
    attached: String,
}

/// 单次反连生命周期：建 mTLS channel → 首帧 Register → 收发帧直至流断。
/// 返回 `Ok(())` 表示控制面关闭了入站流（正常重连）；`Err` 表示连接/传输错误（退避重连）。
async fn connect_once(
    tools: &AuraTools,
    cfg: &ReverseConfig,
    node_id: &Arc<Mutex<String>>,
    reported: &SelfReported,
    uplink: &Arc<Mutex<Option<mpsc::Sender<NodeToController>>>>,
) -> Result<()> {
    let channel = build_channel(cfg).await?;
    let mut client = NodeControlClient::new(channel)
        .max_decoding_message_size(cfg.max_msg_bytes)
        .max_encoding_message_size(cfg.max_msg_bytes);

    // 出站流：futures mpsc 的 Receiver 直接实现 Stream，作为 Connect 请求流（无需 tokio-stream）。
    let (mut tx, rx) = mpsc::channel::<NodeToController>(64);

    // 首帧必为 Register（先入队再开流，保证第一帧）。node_id 空=首次；非空=重连携已分配 UUID。
    let register = NodeToController {
        payload: Some(node_to_controller::Payload::Register(Register {
            node_id: node_id.lock().unwrap().clone(),
            // platform 由 driver 派生（DeviceDriver::platform()：默认 desktop / AndroidDriver 覆写
            // android），非 host cfg!(target_os)——node 宿主 OS ≠ 被控设备平台；controller 纯透传。
            // dyn DeviceDriver 经 vtable 调用其方法，无需 use 该 trait 入作用域。
            platform: tools.driver.platform().to_string(),
            token: cfg.token.clone().unwrap_or_default(),
            // 能力子集三面之一——fleet 面（Locked-6 / D5）：按 driver.supports_tool 过滤广告面，
            // 与 MCP list_tools（transport/mod.rs）同源同谓词（linux desktop 全 21 / win·mac
            // desktop 20（剔 audio_inject，M11 plan D6）/ android 17 / ios 12）。
            // dispatch 超集网不动（被剔工具 call-time 走 E_UNSUPPORTED），过滤只影响 Register 广告面。
            tools: TOOL_NAMES
                .iter()
                .copied()
                .filter(|&t| tools.driver.supports_tool(t))
                .map(|s| s.to_string())
                .collect(),
            // M6 契约版本 fleet 面上报（Locked-7）：填 CONTRACT_VERSION 单点真源（tool_dispatch 再导出，
            // 定义于 transport/mod.rs）；controller 侧回填 NodeInfo.contract_version + 偏斜告警（TASK-005）。
            contract_version: CONTRACT_VERSION.to_string(),
            // M12 可读元数据自报（additive，controller 侧 grpc.go 读→registry→nodes 表落库）：
            //   name         = 宿主 hostname（可读辨识替裸 UUID；非机器指纹，PVE clone 趋同时 label 区分）；
            //   label/location = 用户 --label/--location 引导值（缺省空；console 编辑为权威源，重连不覆盖）；
            //   network_zone = 探测的可达 MinIO 网络域（T07 presigned 分派用）：run() 启动时一次探测跨重连复用，
            //                  空=未配置/均不可达→controller 回落默认端点（兼容既有）。
            name: node_hostname(),
            label: cfg.label.clone().unwrap_or_default(),
            location: cfg.location.clone().unwrap_or_default(),
            network_zone: reported.network_zone.clone(),
            // M12 批B（节点元数据扩展，additive 自报）：os_version=宿主简短系统版本（run() 一次采集传入）；
            //   ip_address=主内网网卡 IP（现采：--local-addr 显式源优先，否则 UDP-connect controller 取源 IP）。
            //   controller 仅内存会话回填 NodeInfo 供 console 渐进披露（ip 敏感不入库；未滚更节点空串前端不显）。
            os_version: reported.os_version.clone(),
            ip_address: node_ip_address(cfg),
            // M12 批D（基础设施标注，additive 自报）：runtime_kind/infra_host=infra 探测（k8s/container/
            //   vm/baremetal + 宿主链）；attached=派生服务标识（env AURA_ATTACHED 优先，driver 配置回落）。
            //   controller 落 nodes 表（离线定位是本组字段核心场景，区别于 os/ip 的 session-only）。
            runtime_kind: reported.runtime_kind.clone(),
            infra_host: reported.infra_host.clone(),
            attached: reported.attached.clone(),
            // 批E（滚更可见性，additive 自报）：二进制版本编译期定值（CARGO_PKG_VERSION），controller
            // 落 nodes 表——版本偏斜自此可从 console/API 盘点（此前仅 auractl stderr 可见）。
            node_version: env!("CARGO_PKG_VERSION").to_string(),
        })),
    };
    tx.send(register)
        .await
        .map_err(|_| anyhow!("outbound channel closed before register"))?;

    // M13：把本次连接出站端登记为观测上报 uplink（drainer 据此投 AgentActivity 帧）。必须在 Register
    // 入队之后发布——出站 channel FIFO，早于此发布则 drainer 的观测帧可抢占队首，controller 以
    // "first frame must be Register" 拒连（收帧前的观测事件丢弃即可，best-effort）。_guard Drop 时
    // （本函数任意路径返回=连接结束）清 None，避免 drainer 向已断出站投递。
    *uplink.lock().unwrap() = Some(tx.clone());
    let _uplink_guard = UplinkGuard(uplink.clone());

    // 开双向流（rx 为请求流）。client.connect 是 NodeControl.Connect RPC 方法（非 transport 便捷构造器，
    // 后者已由 build.rs 的 build_transport(false) 关闭以避 E0592 同名冲突）。tonic 0.14 client streaming API 待远程构建验证。
    let response = client.connect(rx).await?;
    let mut inbound = response.into_inner();

    // 心跳任务：周期 Send Heartbeat；AbortOnDrop 保证本次连接结束时回收（不泄漏）。
    let _heartbeat = AbortOnDrop(spawn_heartbeat(tx.clone(), node_id.clone(), cfg.heartbeat));

    // 收控制面下发帧。
    while let Some(frame) = inbound.next().await {
        let frame = frame?; // 传输错误 → 退避重连
        match frame.payload {
            Some(controller_to_node::Payload::RegisterAck(ack)) => {
                if !ack.accepted {
                    return Err(anyhow!("registration rejected: {}", ack.message));
                }
                if !ack.node_id.is_empty() {
                    let changed = {
                        let mut guard = node_id.lock().unwrap();
                        if *guard != ack.node_id {
                            *guard = ack.node_id.clone();
                            true
                        } else {
                            false
                        }
                    };
                    // 仅在分配值变化时落盘（锁外 IO，不持锁）。
                    if changed {
                        persist_node_id(&cfg.node_id_path, &ack.node_id);
                    }
                    tracing::info!(node_id = %ack.node_id, "registered with controller");
                }
            }
            Some(controller_to_node::Payload::ToolRequest(req)) => {
                // 每请求独立 spawn：慢工具不阻塞收帧/心跳。控制面 per-node 串行下发，
                // 节点侧 AuraTools 廉价 Clone（三 Arc）并发安全。
                let tools = tools.clone();
                let mut resp_tx = tx.clone();
                // per-tool 执行 span：携控制面下发的 task_id 与工具名（哑管道既有字段，零 proto 变更）。
                // 令 node span 与控制面 span 同 task_id 关联，三跳 trace(REST→gRPC→node)可拼接；
                // guard 的 tool.exec 子 span 继承本 task_id 上下文。otel 未启用时仅作 stderr 结构化上下文。
                // trace_id = M6 录制会话 id（node.proto ToolRequest.trace_id，非 OTel trace id）：
                // 非空才记录（GAP-4 node leg，录制归属可从节点侧日志/trace 反查，非录制请求免噪）。
                let span = tracing::info_span!(
                    "tool.request",
                    task_id = %req.task_id,
                    tool = %req.tool,
                    trace_id = tracing::field::Empty
                );
                if !req.trace_id.is_empty() {
                    span.record("trace_id", tracing::field::display(&req.trace_id));
                }
                tokio::spawn(
                    async move {
                        let json_envelope = execute_with_deadline(
                            &tools,
                            &req.tool,
                            &req.json_args,
                            req.deadline_ms,
                        )
                        .await;
                        let resp = NodeToController {
                            payload: Some(node_to_controller::Payload::ToolResponse(ToolResponse {
                                task_id: req.task_id,
                                json_envelope,
                            })),
                        };
                        // 出站已关闭（连接断裂）则丢弃：控制面会在重连后按需重发。
                        let _ = resp_tx.send(resp).await;
                    }
                    .instrument(span),
                );
            }
            Some(controller_to_node::Payload::UploadUrlGrant(grant)) => {
                // 大产物旁路上传（G-5）：经预签名 PUT URL 直连对象存储上传，绕开双向流 16MB 内联上限，
                // 完成回 UploadComplete。每授权独立 spawn：大文件上传不阻塞收帧/心跳。
                let mut resp_tx = tx.clone();
                let staging = artifact_staging_path(cfg, &grant.key);
                // 出站源绑定与 gRPC 腿同源（--local-addr）：Option<IpAddr> 为 Copy，直接进闭包。
                let local_addr = cfg.local_addr;
                let span = tracing::info_span!("upload.grant", key = %grant.key);
                tokio::spawn(
                    async move {
                        let key = grant.key.clone();
                        match super::upload::put_file(&grant.presigned_url, &staging, local_addr).await
                        {
                            Ok(etag) => {
                                tracing::info!(key = %key, etag = %etag, "bypass upload complete");
                                let done = NodeToController {
                                    payload: Some(node_to_controller::Payload::UploadComplete(
                                        UploadComplete { key, etag },
                                    )),
                                };
                                let _ = resp_tx.send(done).await;
                            }
                            Err(e) => {
                                // 上传失败：告警 + best-effort 回 UploadFailed 帧（T10，UploadComplete
                                // 对偶）——控制面据此提前唤醒 awaitUpload 即时降级，免等满兜底窗。
                                // 上行流可能恰已断（PUT 失败常伴网络故障）：发送失败仅告警不重试，
                                // 帧丢失时控制面 awaitUploadTimeout 兜底独立生效（两案纵深）。
                                tracing::warn!(key = %key, error = %format!("{:#}", e), "bypass upload failed");
                                if resp_tx.send(upload_failed_frame(key, &e)).await.is_err() {
                                    tracing::warn!("upload failure frame dropped: outbound stream closed");
                                }
                            }
                        }
                    }
                    .instrument(span),
                );
            }
            None => { /* 空 payload：忽略未知/占位帧 */ }
        }
    }

    // inbound 结束（控制面关闭流）→ 正常重连。
    Ok(())
}

/// 构造旁路上传失败帧（UploadComplete 对偶帧，T10）：key 回声授权、error 取 anyhow cause 链
/// 单行展开（{:#}，仅顶层 Display 会吞根因）。控制面以该文本 resolve 等待方并记降级日志。
fn upload_failed_frame(key: String, err: &anyhow::Error) -> NodeToController {
    NodeToController {
        payload: Some(node_to_controller::Payload::UploadFailed(UploadFailed {
            key,
            error: format!("{:#}", err),
        })),
    }
}

// ===== M13 直连 agent 活动上报 =====

/// uplink 句柄 Drop 守卫：连接结束（connect_once 任意路径返回）时清 None，避免 drainer 向已断出站投递。
struct UplinkGuard(Arc<Mutex<Option<mpsc::Sender<NodeToController>>>>);

impl Drop for UplinkGuard {
    fn drop(&mut self) {
        *self.0.lock().unwrap() = None;
    }
}

/// agent 活动 drainer（run 内一次性 spawn，跨重连长驻）：从观测通道收事件，批量（≤50 条或每 2s）转
/// AgentActivity 帧经当前活跃反连 uplink 投递。best-effort：无连接期/出站满即丢（观测不阻断、不积压）。
async fn drain_activity(
    mut rx: tokio::sync::mpsc::Receiver<agent_obs::AgentEvent>,
    uplink: Arc<Mutex<Option<mpsc::Sender<NodeToController>>>>,
) {
    const BATCH_MAX: usize = 50;
    let mut batch: Vec<AgentCallEvent> = Vec::new();
    let mut ticker = tokio::time::interval(Duration::from_secs(2));
    ticker.tick().await; // 跳过 interval 立即触发的首拍
    loop {
        tokio::select! {
            maybe = rx.recv() => match maybe {
                Some(ev) => {
                    batch.push(to_pb_event(ev));
                    if batch.len() >= BATCH_MAX {
                        flush_activity(&mut batch, &uplink);
                    }
                }
                // 投递端全部 drop（进程收尾 / 该节点无 http 传输）：冲刷残余后退出。
                None => {
                    flush_activity(&mut batch, &uplink);
                    break;
                }
            },
            _ = ticker.tick() => flush_activity(&mut batch, &uplink),
        }
    }
}

/// 冲刷一批 agent 活动事件为单个 AgentActivity 帧，经当前活跃反连出站投递（try_send 非阻塞）。
/// 无活跃连接 / 出站满即丢弃本批（std::mem::take 已清空 batch）——观测 best-effort，不阻塞、不无界积压。
fn flush_activity(
    batch: &mut Vec<AgentCallEvent>,
    uplink: &Arc<Mutex<Option<mpsc::Sender<NodeToController>>>>,
) {
    if batch.is_empty() {
        return;
    }
    let frame = NodeToController {
        payload: Some(node_to_controller::Payload::AgentActivity(AgentActivity {
            events: std::mem::take(batch),
        })),
    };
    if let Some(tx) = uplink.lock().unwrap().as_mut() {
        let _ = tx.try_send(frame); // 满/断即丢（观测帧不重传，控制面容缺）
    }
    // uplink 为 None（无活跃连接）：frame 连同事件丢弃（batch 已 take 清空）。
}

/// 观测事件（采集层 [`agent_obs::AgentEvent`]）→ 传输层 [`AgentCallEvent`]。
/// transport 恒 "http"（stdio 本地开发无反连流承载、不观测；见 agent_obs 模块注释）。
fn to_pb_event(ev: agent_obs::AgentEvent) -> AgentCallEvent {
    AgentCallEvent {
        peer: ev.peer,
        method: ev.method,
        tool: ev.tool,
        client_name: ev.client_name,
        client_version: ev.client_version,
        protocol_version: ev.protocol_version,
        duration_ms: ev.duration_ms,
        ok: ev.ok,
        transport: "http".to_string(),
        ts_unix_ms: ev.ts_unix_ms,
    }
}

/// 构造带可选源地址绑定的 HTTP connector（两建连点共用：gRPC 反连 + 旁路上传 upload.rs）。
/// 设置清单复刻 tonic 0.14 connect() 内部行为——enforce_http(false) 必设：HttpConnector 默认拒
/// 非 http scheme，漏设则 https://{controller} URI 在 connector 层首连即炸（M-6，生产 mTLS 面）。
/// 单点钉死设置清单防两建连点漂移（同 M8 SPA base 契约单点教训）。
pub(crate) fn local_bound_connector(local: Option<IpAddr>) -> HttpConnector {
    let mut c = HttpConnector::new();
    c.enforce_http(false); // 必设（M-6）
    c.set_nodelay(true); // 复刻 tonic 默认
    if let Some(ip) = local {
        c.set_local_address(Some(ip));
    }
    c
}

/// 建立到控制面的 mTLS gRPC Channel（tonic 0.14）。
/// keepalive≥30s + keep_alive_while_idle：驱动 h2 ping 探活，规避 NAT 后半开连接。
/// max_decoding/encoding_message_size 显式设 16MB 上限（小文件内联；大文件走旁路是别的 task）。
async fn build_channel(cfg: &ReverseConfig) -> Result<Channel> {
    let ca = std::fs::read(&cfg.ca)?;
    let cert = std::fs::read(&cfg.cert)?;
    let key = std::fs::read(&cfg.key)?;

    // mTLS：CA 校验服务端 + 客户端证书自证；domain_name 对齐服务端 cert SAN。
    // tonic 0.14 TLS 经 tls-ring feature（rustls + ring provider，跨平台无系统依赖）。
    // ClientTlsConfig builder 方法名（ca_certificate/identity/domain_name）待远程构建验证。
    let tls = ClientTlsConfig::new()
        .ca_certificate(Certificate::from_pem(ca))
        .identity(Identity::from_pem(cert, key))
        .domain_name(cfg.tls_domain.clone());

    let endpoint = Endpoint::from_shared(format!("https://{}", cfg.controller))?
        .tls_config(tls)?
        .http2_keep_alive_interval(cfg.keepalive)
        .keep_alive_while_idle(true)
        .keep_alive_timeout(Duration::from_secs(20))
        .connect_timeout(Duration::from_secs(10));

    // --local-addr 出站选源（SC-1）：未设走既有 connect() 原路径（现网节点全部不带参滚动，
    // 行为零变化红线）；设了则以源地址绑定的 connector 建连——TLS wrap 由 endpoint 层在
    // connector 之外叠加（tonic 自身 connect() 即同构），mTLS/keepalive/connect_timeout 不受影响。
    match cfg.local_addr {
        None => Ok(endpoint.connect().await?),
        Some(ip) => Ok(endpoint
            .connect_with_connector(local_bound_connector(Some(ip)))
            .await?),
    }
}

/// 心跳任务：周期 Send Heartbeat（携当前 node_id）。出站关闭即停止。
fn spawn_heartbeat(
    mut tx: mpsc::Sender<NodeToController>,
    node_id: Arc<Mutex<String>>,
    interval: Duration,
) -> tokio::task::JoinHandle<()> {
    tokio::spawn(async move {
        let mut ticker = tokio::time::interval(interval);
        ticker.tick().await; // 跳过 interval 立即触发的首拍
        loop {
            ticker.tick().await;
            let id = node_id.lock().unwrap().clone();
            let frame = NodeToController {
                payload: Some(node_to_controller::Payload::Heartbeat(Heartbeat {
                    node_id: id,
                    ts_unix_ms: now_unix_ms(),
                })),
            };
            if tx.send(frame).await.is_err() {
                break; // 出站关闭：连接已断，停止心跳
            }
        }
    })
}

/// JoinHandle 包装：Drop 时 abort 关联任务，确保本次连接结束时心跳任务被回收（不泄漏）。
struct AbortOnDrop(tokio::task::JoinHandle<()>);

impl Drop for AbortOnDrop {
    fn drop(&mut self) {
        self.0.abort();
    }
}

/// 按 deadline_ms 在节点侧强制工具超时（控制面另有兜底 timer）。deadline_ms<=0 视为无超时。
async fn execute_with_deadline(
    tools: &AuraTools,
    tool: &str,
    json_args: &[u8],
    deadline_ms: i64,
) -> Vec<u8> {
    if deadline_ms > 0 {
        let dur = Duration::from_millis(deadline_ms as u64);
        match tokio::time::timeout(dur, dispatch(tools, tool, json_args)).await {
            Ok(bytes) => bytes,
            Err(_) => error_envelope("E_TIMEOUT", "tool execution exceeded deadline"),
        }
    } else {
        dispatch(tools, tool, json_args).await
    }
}

// ===== 小工具 =====

/// 当前 Unix 毫秒时间戳（Heartbeat.ts_unix_ms）。
fn now_unix_ms() -> i64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.as_millis() as i64)
        .unwrap_or(0)
}

/// 宿主 hostname（Register.name 自报，M12 可读辨识替裸 UUID）。gethostname 跨平台读宿主名（无系统
/// 依赖，Cargo.lock 既有）；非机器指纹（PVE clone 禁用 machine-id），趋同时由用户 `--label` 区分。
/// 非法 UTF-8 经 to_string_lossy 兜底；controller 侧 name 空则 COALESCE 保留现值、fleet 回落 UUID 展示，
/// 不阻断反连（best-effort 辅助辨识维度）。
fn node_hostname() -> String {
    gethostname::gethostname().to_string_lossy().into_owned()
}

/// 宿主简短系统版本（Register.os_version 自报，M12 批B 辨识增强）：cfg(target_os) 分平台无依赖采集，
/// 归一化为简短可辨串（「Windows 11」/「Ubuntu 22.04」/「macOS 26」）——不含 build/codename/内核号。
/// 采集失败/未知平台回空串（controller 回填空，前端不显，best-effort 辅助辨识；非机器指纹）。
fn node_os_version() -> String {
    #[cfg(target_os = "linux")]
    {
        os_version_linux()
    }
    #[cfg(target_os = "windows")]
    {
        os_version_windows()
    }
    #[cfg(target_os = "macos")]
    {
        os_version_macos()
    }
    #[cfg(not(any(target_os = "linux", target_os = "windows", target_os = "macos")))]
    {
        String::new()
    }
}

/// Linux：读 /etc/os-release 的 NAME + VERSION_ID → 「Ubuntu 22.04」。缺 VERSION_ID 回落 NAME，均缺回空。
#[cfg(target_os = "linux")]
fn os_version_linux() -> String {
    let content = match std::fs::read_to_string("/etc/os-release") {
        Ok(c) => c,
        Err(_) => return String::new(),
    };
    let (mut name, mut version) = (String::new(), String::new());
    for line in content.lines() {
        if let Some(v) = line.strip_prefix("NAME=") {
            name = v.trim_matches('"').to_string();
        } else if let Some(v) = line.strip_prefix("VERSION_ID=") {
            version = v.trim_matches('"').to_string();
        }
    }
    match (name.is_empty(), version.is_empty()) {
        (false, false) => format!("{name} {version}"),
        (false, true) => name,
        _ => String::new(),
    }
}

/// Windows：`cmd /c ver` 输出 `Microsoft Windows [<版本/Version> 10.0.BUILD.rev]` → 取 build（第三点分段），
/// ≥22000 归「Windows 11」否则「Windows 10」（build 号是 Win10/11 唯一可靠区分，产品名/主次版本号不可靠）。
/// **语言无关**（团队警告：中文 Windows 输出「[版本 …]」而非「[Version …]」，cmd OEM 码页经 from_utf8_lossy
/// 成 mojibake）：不依赖 "Version"/"版本" 字面——逐 whitespace token 剥非数字首尾噪声（[ ] 及紧贴的版本/mojibake），
/// 命中「数.数.数…」即取第三段 build（ASCII 数字段不受编码影响，实测 252.20 中文机「版本 10.0.19045.4780」→19045）。
#[cfg(target_os = "windows")]
fn os_version_windows() -> String {
    let out = match std::process::Command::new("cmd").args(["/c", "ver"]).output() {
        Ok(o) => o,
        Err(_) => return String::new(),
    };
    windows_version_from_ver(&String::from_utf8_lossy(&out.stdout))
}

/// 从 `ver` 输出文本解析简短 Windows 版本（**语言无关纯函数**，非生产 cfg-gated 便于跨平台单测中文/英文/
/// mojibake 输入）：逐 whitespace token 剥非数字首尾噪声（[ ] 及紧贴的「版本」/mojibake），命中「数.数.BUILD…」
/// 取第三段 build——≥22000→「Windows 11」否则「Windows 10」；无数字版本 token→「Windows」兜底。cfg(windows)
/// 生产调用 + cfg(test) 单测覆盖，Linux 非测试构建排除（无 dead_code）。
#[cfg(any(target_os = "windows", test))]
fn windows_version_from_ver(text: &str) -> String {
    let build = text.split_whitespace().find_map(|tok| {
        let t = tok.trim_matches(|c: char| !c.is_ascii_digit());
        let parts: Vec<&str> = t.split('.').collect();
        if parts.len() >= 3 && parts[0].parse::<u32>().is_ok() && parts[1].parse::<u32>().is_ok() {
            parts[2].parse::<u32>().ok()
        } else {
            None
        }
    });
    match build {
        Some(b) if b >= 22000 => "Windows 11".to_string(),
        Some(_) => "Windows 10".to_string(),
        None => "Windows".to_string(),
    }
}

/// macOS：`sw_vers -productVersion` → 「26.0」→ 「macOS 26」（取主版本号；mac 本轮不滚更，代码仍写全）。
#[cfg(target_os = "macos")]
fn os_version_macos() -> String {
    let out = match std::process::Command::new("sw_vers")
        .arg("-productVersion")
        .output()
    {
        Ok(o) => o,
        Err(_) => return String::new(),
    };
    let ver = String::from_utf8_lossy(&out.stdout).trim().to_string();
    if ver.is_empty() {
        return String::new();
    }
    let major = ver.split('.').next().unwrap_or(&ver);
    format!("macOS {major}")
}

/// 节点主内网 IP（Register.ip_address 自报，M12 批B）：`--local-addr` 显式源优先（多宿主确定性）；否则
/// UDP-connect 到 controller 地址读本地 socket 源 IP（无实际发包，仅令内核按路由选定到达 controller 的源
/// 网卡 IP）。失败回空串。controller 仅内存会话回填 NodeInfo（敏感，不入库）。
fn node_ip_address(cfg: &ReverseConfig) -> String {
    if let Some(addr) = cfg.local_addr {
        return addr.to_string();
    }
    let sock = match std::net::UdpSocket::bind("0.0.0.0:0") {
        Ok(s) => s,
        Err(_) => return String::new(),
    };
    if sock.connect(cfg.controller.as_str()).is_err() {
        return String::new();
    }
    match sock.local_addr() {
        Ok(a) => a.ip().to_string(),
        Err(_) => String::new(),
    }
}

/// 解析节点可达的 MinIO 网络域（T07/REC-6）：`--network-domain` 显式标签优先（直用免探测）；否则读
/// `AURA_MINIO_PROBE`（`zone:host:port,zone:host:port` 逗号分隔，冒号首分隔 zone 与 endpoint）逐项 TCP
/// dial（2s 超时），首个可达域即上报域（列表顺序即优先级：lan 优先 jump 兜底）。均不可达/未配置返回空串
///（controller 回落默认端点，兼容未分派部署）。honor `--local-addr` 出站源（多宿主机从对应网卡探测）。
async fn resolve_network_zone(cfg: &ReverseConfig) -> String {
    if let Some(domain) = cfg.network_domain.as_deref() {
        if !domain.is_empty() {
            return domain.to_string();
        }
    }
    let probe = std::env::var("AURA_MINIO_PROBE").unwrap_or_default();
    probe_first_reachable_zone(&probe, cfg.local_addr).await
}

/// 从 `AURA_MINIO_PROBE` 候选列表探测首个可达域（顺序即优先级：lan 优先 jump 兜底）；均不可达/空返回
/// 空串。env 读取（resolve_network_zone）与探测算法分离，便于确定性单测（免 env 全局态竞态）。
async fn probe_first_reachable_zone(probe: &str, local_addr: Option<IpAddr>) -> String {
    for (zone, endpoint) in parse_probe_candidates(probe) {
        if probe_reachable(&endpoint, local_addr).await {
            tracing::info!(zone = %zone, endpoint = %endpoint, "minio endpoint reachable, selecting network zone");
            return zone;
        }
        tracing::debug!(zone = %zone, endpoint = %endpoint, "minio endpoint unreachable, trying next candidate");
    }
    String::new()
}

/// 解析 `AURA_MINIO_PROBE`（`zone:host:port,...`）为 (zone, endpoint) 候选列表（顺序即探测优先级）。
/// 冒号首分隔：zone 取首段、endpoint 取其余（endpoint 的 host:port 自身含冒号，故 split_once 非 split）。
/// 空项/无冒号/空 zone/空 endpoint 跳过（宽松解析：误配单项忽略不阻断反连）。
fn parse_probe_candidates(s: &str) -> Vec<(String, String)> {
    s.split(',')
        .filter_map(|item| {
            let (zone, endpoint) = item.trim().split_once(':')?;
            let zone = zone.trim();
            let endpoint = endpoint.trim();
            if zone.is_empty() || endpoint.is_empty() {
                return None;
            }
            Some((zone.to_string(), endpoint.to_string()))
        })
        .collect()
}

/// TCP 探测 endpoint（host:port）2s 内可建连即视为可达（不发任何字节，仅握手）。honor `--local-addr`
/// 出站源绑定（与 gRPC 反连 / 旁路上传腿同源选源，多宿主机确定性从对应网卡探测）；DNS 解析/连接失败/
/// 超时均视为不可达（返 false，探测下一候选）。
async fn probe_reachable(endpoint: &str, local_addr: Option<IpAddr>) -> bool {
    const PROBE_TIMEOUT: Duration = Duration::from_secs(2);
    let addrs = match tokio::net::lookup_host(endpoint).await {
        Ok(it) => it,
        Err(_) => return false,
    };
    for addr in addrs {
        let dial = async {
            match local_addr {
                None => tokio::net::TcpStream::connect(addr).await,
                Some(ip) => connect_bound(ip, addr).await,
            }
        };
        if matches!(tokio::time::timeout(PROBE_TIMEOUT, dial).await, Ok(Ok(_))) {
            return true;
        }
    }
    false
}

/// 以指定源 IP 绑定后连 target（多宿主机确定性选源，与 gRPC/上传腿 `--local-addr` 同义）。
/// 源与目标地址族不一致（如源 v4 目标 v6）时 bind 失败返 Err，由调用方视作该候选不可达。
async fn connect_bound(local: IpAddr, target: SocketAddr) -> std::io::Result<tokio::net::TcpStream> {
    let socket = match target {
        SocketAddr::V4(_) => tokio::net::TcpSocket::new_v4()?,
        SocketAddr::V6(_) => tokio::net::TcpSocket::new_v6()?,
    };
    socket.bind(SocketAddr::new(local, 0))?;
    socket.connect(target).await
}

/// 退避抖动 [0, backoff/2)：无第三方 rand，用系统时间纳秒低位派生。仅打散重连时序，非加密用途。
fn jitter_for(backoff: Duration) -> Duration {
    let half = backoff / 2;
    if half.is_zero() {
        return Duration::ZERO;
    }
    let nanos = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.subsec_nanos())
        .unwrap_or(0);
    let frac = (nanos % 1_000_000) as u128;
    Duration::from_millis(((half.as_millis() * frac) / 1_000_000) as u64)
}

/// 读持久化的 node_id（不存在或读失败返回空串 = 首次注册）。
fn load_node_id(path: &Path) -> String {
    std::fs::read_to_string(path)
        .map(|s| s.trim().to_string())
        .unwrap_or_default()
}

/// 持久化控制面分配的 node_id（失败仅告警，不中断反连；重连时以内存值兜底）。
fn persist_node_id(path: &Path, node_id: &str) {
    if let Some(parent) = path.parent() {
        if let Err(e) = std::fs::create_dir_all(parent) {
            tracing::warn!(error = %e, "failed to create node data dir");
            return;
        }
    }
    if let Err(e) = std::fs::write(path, node_id) {
        tracing::warn!(error = %e, "failed to persist node_id");
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    // 工具名/入参解析/error_envelope 的单测随实现迁移至 `super::tool_dispatch`；
    // 集合相等断言（MCP ToolRouter == TOOL_NAMES）亦在该模块，替代旧 len==16 计数测试。

    /// 抖动不超过 backoff/2。
    #[test]
    fn jitter_never_exceeds_half_backoff() {
        let j = jitter_for(Duration::from_secs(4));
        assert!(j < Duration::from_secs(2));
    }

    /// 零退避的抖动为零（消除边界）。
    #[test]
    fn jitter_zero_backoff_is_zero() {
        assert_eq!(jitter_for(Duration::ZERO), Duration::ZERO);
    }

    /// data_dir 同源回归：record_tools 的 `AuraTools::artifact_output_path` 与本模块
    /// `artifact_staging_path` 对同一 data_dir + key 必须解析到同一路径（录屏落盘 == 旁路上传读取，
    /// 杜绝双解析漂移）。
    #[test]
    fn record_and_staging_paths_share_data_dir() {
        let data_dir = PathBuf::from("/var/lib/aura");
        let key = "recordings/deadbeef.mp4";

        // grpc 侧暂存路径（经 ReverseConfig::data_dir，即 node_id_path 上溯一级）。
        let cfg = ReverseConfig {
            controller: "controller:7443".to_string(),
            ca: PathBuf::from("ca.pem"),
            cert: PathBuf::from("cert.pem"),
            key: PathBuf::from("key.pem"),
            tls_domain: "controller".to_string(),
            token: None,
            label: None,
            location: None,
            network_domain: None,
            node_id_path: data_dir.join("node_id"),
            keepalive: Duration::from_secs(30),
            heartbeat: Duration::from_secs(15),
            max_msg_bytes: 16 * 1024 * 1024,
            local_addr: None,
        };
        let staging = artifact_staging_path(&cfg, key);

        // record 侧落盘路径（AuraTools 注入同一 data_dir）。
        let tools = AuraTools::new(aura_platform::build_driver(
            aura_platform::DriverKind::Desktop,
            None,
            None,
        ))
        .with_data_dir(data_dir.clone());
        let output = tools.artifact_output_path(key);

        assert_eq!(staging, output, "录屏落盘路径必须与旁路暂存路径同源");
        assert_eq!(staging, data_dir.join("artifacts").join(key));
    }

    /// T10：PUT 失败路径发帧——帧形态（oneof=UploadFailed、key 回声、error 为 anyhow cause 链
    /// 单行展开）与上行发送链路（接收端收到同帧）双断言；sender 即闭包实际所用的 futures mpsc 通道。
    #[tokio::test]
    async fn upload_failed_frame_is_sent_on_put_error() {
        let err = anyhow!("connection reset").context("presigned PUT failed");
        let frame = upload_failed_frame("artifacts/rec-t10.mp4".to_string(), &err);

        let (mut tx, mut rx) = mpsc::channel::<NodeToController>(1);
        tx.send(frame).await.expect("send upload-failed frame");
        let got = rx.next().await.expect("frame delivered on outbound channel");
        match got.payload {
            Some(node_to_controller::Payload::UploadFailed(uf)) => {
                assert_eq!(uf.key, "artifacts/rec-t10.mp4", "key 必须回声授权中的 key");
                assert!(
                    uf.error.contains("presigned PUT failed") && uf.error.contains("connection reset"),
                    "error 必须为 cause 链单行展开，got: {}",
                    uf.error
                );
            }
            other => panic!("expected UploadFailed payload, got {other:?}"),
        }
    }

    /// SC-1 (a)：gRPC 腿 --local-addr 源绑定——accept 侧 peer IP 即出站源 IP。
    /// h2 前言对假 listener 必失败：TCP 建连时 accept 已发生，断言在 accept 侧，client 错误忽略。
    #[tokio::test]
    async fn grpc_leg_binds_local_source_addr() {
        // try-bind 探测（比平台 cfg 假设诚实）：mac 无 lo0 alias 时 127.0.0.2 不可绑，跳过。
        if std::net::TcpListener::bind(("127.0.0.2", 0)).is_err() {
            eprintln!("skip: 127.0.0.2 not bindable on this host");
            return;
        }
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let port = listener.local_addr().unwrap().port();

        let endpoint = Endpoint::from_shared(format!("http://127.0.0.1:{port}")).unwrap();
        let connect = tokio::spawn(async move {
            let _ = endpoint
                .connect_with_connector(local_bound_connector(Some("127.0.0.2".parse().unwrap())))
                .await;
        });

        let (_stream, peer) = tokio::time::timeout(Duration::from_secs(5), listener.accept())
            .await
            .expect("accept should fire within 5s")
            .unwrap();
        assert_eq!(peer.ip(), "127.0.0.2".parse::<IpAddr>().unwrap());
        connect.abort();
    }

    /// SC-1 (c)：https + tls_config 组合经自定义 connector 不被 scheme 关拒（M-6 盲区补测）。
    /// accept 触发 = TCP 已达 = enforce_http(false) 生效；对假 listener 的 TLS 握手必失败——预期，
    /// 断言在 accept 侧。对照：漏设 enforce_http 时 connector 在任何 TCP 连接之前拒 URI，accept 永不触发。
    #[tokio::test]
    async fn https_uri_passes_custom_connector_with_tls_config() {
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let port = listener.local_addr().unwrap().port();

        let endpoint = Endpoint::from_shared(format!("https://127.0.0.1:{port}"))
            .unwrap()
            .tls_config(ClientTlsConfig::new().domain_name("localhost"))
            .unwrap();
        let connect = tokio::spawn(async move {
            let _ = endpoint
                .connect_with_connector(local_bound_connector(None))
                .await;
        });

        let accepted = tokio::time::timeout(Duration::from_secs(5), listener.accept()).await;
        assert!(
            accepted.is_ok(),
            "TCP accept 必须触发：enforce_http(false) 放行 https URI 过 connector scheme 关"
        );
        connect.abort();
    }

    /// SC-1 (d)：CLI 贯通回归——不传 --local-addr 时配置为 None（build_channel 走既有 connect()
    /// 分支，行为零变化红线）；传参则 clap → into_config → ReverseConfig 全链贯通。
    #[test]
    fn local_addr_cli_wiring_default_none() {
        #[derive(clap::Parser)]
        struct TestCli {
            #[command(flatten)]
            reverse: ReverseOpts,
        }
        use clap::Parser as _;

        let base = [
            "t", "--controller", "c:7443", "--ca", "ca.pem", "--cert", "cert.pem", "--key",
            "key.pem", "--tls-domain", "c", "--data-dir", "aura-test-dir",
        ];
        let cfg = TestCli::try_parse_from(base)
            .unwrap()
            .reverse
            .into_config()
            .unwrap()
            .unwrap();
        assert!(cfg.local_addr.is_none(), "不传参必须保持 None（既有 .connect() 分支）");

        let mut with_flag = base.to_vec();
        with_flag.extend(["--local-addr", "192.168.252.11"]);
        let cfg = TestCli::try_parse_from(with_flag)
            .unwrap()
            .reverse
            .into_config()
            .unwrap()
            .unwrap();
        assert_eq!(cfg.local_addr, Some("192.168.252.11".parse().unwrap()));
    }

    /// M12：node_hostname 自报宿主名（gethostname，Register.name 据此替裸 UUID）。构建机宿主名恒非空
    /// （gethostname 在真实主机返有效名）；断言非空 + 两次读稳定，不硬编码具体名（跨机异）。
    #[test]
    fn node_hostname_is_reported_and_stable() {
        let h1 = node_hostname();
        let h2 = node_hostname();
        assert!(!h1.is_empty(), "gethostname 应返非空宿主名（Register.name 自报源）");
        assert_eq!(h1, h2, "同主机两次读 hostname 应稳定");
    }

    /// M12 批B：Windows `ver` 版本解析**语言无关**（团队警告的中文 Windows 坑）——不依赖 "Version"/"版本"
    /// 字面，只抓「数.数.BUILD」数字段取 build。覆盖英文/中文/mojibake/无版本 token；build≥22000=Win11。
    #[test]
    fn windows_version_parse_is_language_agnostic() {
        // 英文
        assert_eq!(
            windows_version_from_ver("Microsoft Windows [Version 10.0.22631.4460]"),
            "Windows 11"
        );
        assert_eq!(
            windows_version_from_ver("Microsoft Windows [Version 10.0.19045.4780]"),
            "Windows 10"
        );
        // 中文（实测 252.20 中文 Win10）——「版本」而非「Version」
        assert_eq!(
            windows_version_from_ver("Microsoft Windows [版本 10.0.19045.4780]"),
            "Windows 10"
        );
        assert_eq!(
            windows_version_from_ver("Microsoft Windows [版本 10.0.22000.100]"),
            "Windows 11"
        );
        // mojibake（cmd OEM 码页经 from_utf8_lossy，「版本」→替换字符）——数字段仍 ASCII 完好
        assert_eq!(
            windows_version_from_ver("Microsoft Windows [\u{fffd}\u{fffd} 10.0.19045.4780]"),
            "Windows 10"
        );
        // 无空格粘连边缘（版本号紧贴前缀）
        assert_eq!(
            windows_version_from_ver("[版本10.0.22631.1]"),
            "Windows 11"
        );
        // 无版本 token → 兜底
        assert_eq!(windows_version_from_ver("garbage no version"), "Windows");
        assert_eq!(windows_version_from_ver(""), "Windows");
    }

    /// M12：--label/--location CLI 贯通 into_config——不传 → None（上报空串，console 编辑权威）；
    /// 传参 → ReverseConfig.label/location 全链贯通（Register 首帧作引导值上报）。
    #[test]
    fn label_location_cli_wiring() {
        #[derive(clap::Parser)]
        struct TestCli {
            #[command(flatten)]
            reverse: ReverseOpts,
        }
        use clap::Parser as _;

        let base = [
            "t", "--controller", "c:7443", "--ca", "ca.pem", "--cert", "cert.pem", "--key",
            "key.pem", "--tls-domain", "c", "--data-dir", "aura-test-dir",
        ];
        let cfg = TestCli::try_parse_from(base)
            .unwrap()
            .reverse
            .into_config()
            .unwrap()
            .unwrap();
        assert!(
            cfg.label.is_none() && cfg.location.is_none(),
            "不传 --label/--location 时为 None（上报空串，console 编辑权威）"
        );

        let mut with = base.to_vec();
        with.extend(["--label", "prod-desk", "--location", "rack-3"]);
        let cfg = TestCli::try_parse_from(with)
            .unwrap()
            .reverse
            .into_config()
            .unwrap()
            .unwrap();
        assert_eq!(cfg.label.as_deref(), Some("prod-desk"));
        assert_eq!(cfg.location.as_deref(), Some("rack-3"));
    }

    /// T07：AURA_MINIO_PROBE 解析——zone:host:port 冒号首分隔（endpoint 自身含冒号），保序。
    #[test]
    fn parse_probe_candidates_splits_zone_and_endpoint() {
        let got = parse_probe_candidates("lan:192.168.22.240:9000,jump:100.78.22.127:9000");
        assert_eq!(
            got,
            vec![
                ("lan".to_string(), "192.168.22.240:9000".to_string()),
                ("jump".to_string(), "100.78.22.127:9000".to_string()),
            ]
        );
    }

    /// T07：解析宽松跳过误配项——空项 / 无冒号 / 空 zone(:x) / 空 endpoint(x:) 忽略，合法项按序保留
    /// （顺序即探测优先级），单项误配不阻断反连。
    #[test]
    fn parse_probe_candidates_skips_malformed_keeps_order() {
        let got = parse_probe_candidates(
            " lan:10.0.0.1:9000 , , nocolon , :noendpoint , lan2: , jump:10.0.0.2:9000 ",
        );
        assert_eq!(
            got,
            vec![
                ("lan".to_string(), "10.0.0.1:9000".to_string()),
                ("jump".to_string(), "10.0.0.2:9000".to_string()),
            ]
        );
    }

    /// T07：probe_reachable 对活 listener 探测可达（仅 TCP 握手，不发字节）。
    #[tokio::test]
    async fn probe_reachable_detects_live_listener() {
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let addr = listener.local_addr().unwrap();
        assert!(
            probe_reachable(&addr.to_string(), None).await,
            "本地活 listener 应探测可达"
        );
    }

    /// T07：probe_first_reachable_zone 选首个可达域——活 listener 置首即选中并短路，第二候选
    /// （TEST-NET-1 不可路由 192.0.2.0/24，RFC 5737）不触达（否则 2s 超时）。验证顺序即优先级。
    #[tokio::test]
    async fn probe_first_reachable_zone_selects_first_live() {
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let addr = listener.local_addr().unwrap();
        let probe = format!("lan:{},jump:192.0.2.1:9000", addr);
        assert_eq!(probe_first_reachable_zone(&probe, None).await, "lan");
    }

    /// T07：无候选（空 AURA_MINIO_PROBE）→ 空 zone（controller 回落默认端点，兼容既有单端点）。
    #[tokio::test]
    async fn probe_first_reachable_zone_empty_when_no_candidates() {
        assert_eq!(probe_first_reachable_zone("", None).await, "");
    }

    /// T07：`--network-domain` 显式标签短路探测——resolve_network_zone 直接返回标签，不读 env、不 dial。
    #[tokio::test]
    async fn resolve_network_zone_explicit_domain_short_circuits() {
        let cfg = ReverseConfig {
            controller: "c:7443".to_string(),
            ca: PathBuf::from("ca.pem"),
            cert: PathBuf::from("cert.pem"),
            key: PathBuf::from("key.pem"),
            tls_domain: "c".to_string(),
            token: None,
            label: None,
            location: None,
            network_domain: Some("jump".to_string()),
            node_id_path: PathBuf::from("nid"),
            keepalive: Duration::from_secs(30),
            heartbeat: Duration::from_secs(15),
            max_msg_bytes: 16 * 1024 * 1024,
            local_addr: None,
        };
        assert_eq!(
            resolve_network_zone(&cfg).await,
            "jump",
            "显式 --network-domain 直用免探测"
        );
    }

    /// M12/T07：--network-domain CLI 贯通 into_config——不传 → None（回落 AURA_MINIO_PROBE 探测）；
    /// 传参 → ReverseConfig.network_domain 贯通（显式覆盖探测）。
    #[test]
    fn network_domain_cli_wiring() {
        #[derive(clap::Parser)]
        struct TestCli {
            #[command(flatten)]
            reverse: ReverseOpts,
        }
        use clap::Parser as _;

        let base = [
            "t", "--controller", "c:7443", "--ca", "ca.pem", "--cert", "cert.pem", "--key",
            "key.pem", "--tls-domain", "c", "--data-dir", "aura-test-dir",
        ];
        let cfg = TestCli::try_parse_from(base)
            .unwrap()
            .reverse
            .into_config()
            .unwrap()
            .unwrap();
        assert!(
            cfg.network_domain.is_none(),
            "不传 --network-domain → None（回落 AURA_MINIO_PROBE 探测）"
        );

        let mut with = base.to_vec();
        with.extend(["--network-domain", "jump"]);
        let cfg = TestCli::try_parse_from(with)
            .unwrap()
            .reverse
            .into_config()
            .unwrap()
            .unwrap();
        assert_eq!(cfg.network_domain.as_deref(), Some("jump"));
    }
}
