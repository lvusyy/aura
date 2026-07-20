//! aura-node：AURA 节点单二进制。
//!
//! 单 tokio runtime + CLI 选传输（stdio | http）。stdio 日志强制 stderr、关闭 ANSI，
//! 以保 stdout 协议流洁净（仅 JSON-RPC）。全 crate 不向标准输出打印，日志一律走 stderr。

use std::net::SocketAddr;

use anyhow::Result;
use clap::{Parser, Subcommand, ValueEnum};
use tracing_subscriber::layer::SubscriberExt;
use tracing_subscriber::util::SubscriberInitExt;
use tracing_subscriber::{fmt, EnvFilter};

use aura_platform::{build_driver, DriverKind};

mod service;
mod transport;

use transport::AuraTools;

/// 顶层驱动选择（clap [`ValueEnum`]）：映射到 [`aura_platform::DriverKind`]。clap 依赖留在
/// aura-node 侧，不下沉 aura-platform（保持平台层无 CLI 依赖）。
#[derive(Clone, Copy, Debug, ValueEnum)]
enum DriverArg {
    /// 桌面驱动（Windows/Linux/macOS 原生），默认。
    Desktop,
    /// Android 驱动（经 adb shell out 远程设备）。
    Android,
    /// iOS 驱动（经 WDA HTTP 远程设备；模拟器 / 真机双后端）。
    Ios,
}

impl From<DriverArg> for DriverKind {
    fn from(arg: DriverArg) -> Self {
        match arg {
            DriverArg::Desktop => DriverKind::Desktop,
            DriverArg::Android => DriverKind::Android,
            DriverArg::Ios => DriverKind::Ios,
        }
    }
}

/// AURA 节点：MCP 双传输（stdio 供本地直连 / http 供远程控制面）。
#[derive(Parser)]
#[command(name = "aura-node", version, about = "AURA 节点：MCP 双传输（stdio | http）")]
struct Cli {
    /// 驱动选择：`desktop`（默认，桌面原生）| `android`（经 adb shell out 远程设备）|
    /// `ios`（经 WDA HTTP 远程设备）。驱动实现运行时选择（编译期→运行时，Locked-4/5）；
    /// 未指定即 desktop，行为与既往完全一致（回归保险）。
    #[arg(long, value_enum, default_value = "desktop")]
    driver: DriverArg,

    /// 设备序列号；由 `--driver android`（`adb -s <serial>` 设备选择）与 `--driver ios`
    /// （承载 udid）消费，desktop 忽略。与 grpc/transport 选项正交（un-gated）。
    #[arg(long)]
    serial: Option<String>,

    /// iOS WDA HTTP 端点；仅 `--driver ios` 消费（desktop/android 忽略）。缺省单实例固定 8100；
    /// 池化多实例经此参传每实例端口（udid→端口翻译归 provisioning 层，§3.1）。缺省值须与
    /// aura-platform `DEFAULT_WDA_URL` 保持一致（两层各自缺省，跨 crate 无法共享常量）。
    #[arg(long, default_value = "http://127.0.0.1:8100")]
    wda_url: String,

    /// gRPC 反连选项（仅 --features grpc 编译）：设 --controller 即与 MCP 传输并存反连控制面，
    /// 未设则不启动反连、行为与 M1 完全一致。
    #[cfg(feature = "grpc")]
    #[command(flatten)]
    reverse: transport::grpc_reverse::ReverseOpts,

    #[command(subcommand)]
    transport: TransportCmd,
}

#[derive(Subcommand)]
enum TransportCmd {
    /// stdio 传输：被 MCP 客户端作为子进程拉起，stdout 专供 JSON-RPC。
    Stdio,
    /// Streamable HTTP 传输：常驻监听，单端点 /mcp。
    Http {
        /// 监听地址（host:port）。
        #[arg(long, default_value = "0.0.0.0:7100")]
        bind: SocketAddr,
    },
    /// 设备接入（M12 TASK-006，feature enroll）：genkey+CSR → /v1/enroll 换 per-node 证书落盘。
    /// 一次性子命令，跑完即退（不启动 MCP/反连服务面）。安装器（TASK-009）调用的核心腿。
    #[cfg(feature = "enroll")]
    Enroll(transport::enroll::EnrollArgs),
    /// 证书续签（M12，feature enroll）：现 per-node 证书 mTLS → /v1/renew 换新。一次性子命令，跑完即退。
    #[cfg(feature = "enroll")]
    Renew(transport::enroll::RenewArgs),
    /// 服务生命周期管理（M16 T1.5，非门控）：查状态 / 重启（跨平台统一入口 + self-update 后验证）。
    /// 一次性子命令，跑完即退（不启动 driver/反连服务面）。安装归 install.sh/install.ps1 单一源，不重复。
    Service(service::ServiceArgs),
}

/// 构造 OTLP tracing 层（仅 `--features otel`）。
///
/// `AURA_OTLP_ENDPOINT` 已配置则建 OTLP/gRPC(tonic) span exporter + `SdkTracerProvider`（batch 上报）
/// 桥接 `tracing-opentelemetry` 层；未配置或 exporter 初始化失败则返回 `None`——降级为仅 stderr
/// （零 OTLP 开销，M1/M2 行为不变）。控制面 collector 地址与节点约定同为 `AURA_OTLP_ENDPOINT`
/// （形如 `http://collector-host:4317`，OTLP/gRPC）。诊断日志走 stderr（stdout 仍专供协议流）。
#[cfg(feature = "otel")]
fn otel_layer<S>() -> Option<Box<dyn tracing_subscriber::Layer<S> + Send + Sync>>
where
    S: tracing::Subscriber + for<'span> tracing_subscriber::registry::LookupSpan<'span> + Send + Sync,
{
    use opentelemetry::trace::TracerProvider as _;
    use opentelemetry_otlp::{SpanExporter, WithExportConfig};
    use opentelemetry_sdk::trace::SdkTracerProvider;
    use opentelemetry_sdk::Resource;
    use tracing_subscriber::Layer as _;

    // 未配置端点 = 完全禁用（零开销降级）。空串按未配置处理。
    let endpoint = std::env::var("AURA_OTLP_ENDPOINT")
        .ok()
        .filter(|s| !s.is_empty())?;

    // OTLP/gRPC(tonic) span exporter。初始化失败不 fatal：观测尽力而为，降级仅 stderr。
    let exporter = match SpanExporter::builder()
        .with_tonic()
        .with_endpoint(endpoint.clone())
        .build()
    {
        Ok(e) => e,
        Err(e) => {
            eprintln!("aura-node: OTLP exporter init failed ({e}); tracing falls back to stderr only");
            return None;
        }
    };

    // batch 上报 + service.name 资源标识（collector 侧按服务名归类 node span）。
    let provider = SdkTracerProvider::builder()
        .with_batch_exporter(exporter)
        .with_resource(Resource::builder().with_service_name("aura-node").build())
        .build();
    let tracer = provider.tracer("aura-node");
    // 置全局 provider 保活（batch 后台线程周期 flush；进程常驻，随进程生命周期上报）。
    opentelemetry::global::set_tracer_provider(provider);
    eprintln!("aura-node: OTLP tracing enabled -> {endpoint}");

    Some(tracing_opentelemetry::layer().with_tracer(tracer).boxed())
}

#[tokio::main]
async fn main() -> Result<()> {
    // 日志强制 stderr + 无 ANSI：stdio 传输 stdout 必须洁净（无横幅/颜色，仅协议流）。
    // Registry 分层：fmt 层（stderr）恒在；OTLP 层仅 --features otel 且 AURA_OTLP_ENDPOINT 配置时叠加，
    // 未配置降级仅 stderr（M1/M2 行为不变，stdout 仍洁净）。
    let env_filter = EnvFilter::try_from_default_env().unwrap_or_else(|_| EnvFilter::new("info"));
    let fmt_layer = fmt::layer().with_writer(std::io::stderr).with_ansi(false);
    let subscriber = tracing_subscriber::registry().with(env_filter).with(fmt_layer);
    #[cfg(feature = "otel")]
    subscriber.with(otel_layer()).init();
    #[cfg(not(feature = "otel"))]
    subscriber.init();

    let cli = Cli::parse();

    // M16 T1.5 service 子命令（非门控）：查状态 / 重启，不需 driver/反连服务面，driver 装配前短路跑完即退。
    if let TransportCmd::Service(ref args) = cli.transport {
        return service::run(args.clone());
    }

    // M12 一次性子命令（feature enroll，TASK-006）：enroll/renew 换证不需 driver/反连服务面，在 driver/
    // tools 装配前短路跑完即退——避免为一次性换证起完整节点运行时。ref 借用后 clone 参数，不消费 cli.transport
    // （下方服务面 match 仍可移动）。
    #[cfg(feature = "enroll")]
    match cli.transport {
        TransportCmd::Enroll(ref args) => return transport::enroll::run_enroll(args.clone()),
        TransportCmd::Renew(ref args) => return transport::enroll::run_renew(args.clone()),
        _ => {}
    }

    // 运行时按 --driver 选择驱动实现（默认 desktop 回归零破坏）；--serial 由 Android/iOS 臂消费，
    // --wda-url 仅 iOS 臂消费（desktop/android 忽略）。
    let driver = build_driver(cli.driver.into(), cli.serial, Some(cli.wda_url));

    // gRPC 反连配置先解析：其 data_dir（`--data-dir` CLI）注入 AuraTools，使录屏落盘路径与旁路上传
    // 读取路径同源（消除 record_tools 既往独立 env 解析的双解析漂移）。未启用 grpc 时用 env 缺省。
    #[cfg(feature = "grpc")]
    let mut reverse_cfg = cli.reverse.into_config()?;
    // M14：http 传输并存时回填 MCP 网关自环端点——控制面网关的代理请求经此送达本机 rmcp 面。
    // 通配绑定（0.0.0.0/[::]）走 IPv4 环回；绑定具体地址（含 127.0.0.1 与显式网卡 IP）时自环
    // 用该地址本身——否则 `--bind <LAN-IP>` 形态下 127.0.0.1 不在监听面、网关恒 502。
    // stdio 模式保持 None：无本地 /mcp，代理请求回 503（fail loud）。
    #[cfg(feature = "grpc")]
    if let (Some(cfg), TransportCmd::Http { bind }) = (reverse_cfg.as_mut(), &cli.transport) {
        let ip = if bind.ip().is_unspecified() {
            // 按地址族取环回：[::] 通配在 IPv6-only 栈上不听 127.0.0.1，须回 [::1]。
            match bind.ip() {
                std::net::IpAddr::V4(_) => std::net::IpAddr::from(std::net::Ipv4Addr::LOCALHOST),
                std::net::IpAddr::V6(_) => std::net::IpAddr::from(std::net::Ipv6Addr::LOCALHOST),
            }
        } else {
            bind.ip()
        };
        cfg.mcp_loopback = Some(std::net::SocketAddr::new(ip, bind.port()));
    }
    #[cfg(feature = "grpc")]
    let tools = match reverse_cfg.as_ref() {
        Some(cfg) => AuraTools::new(driver).with_data_dir(cfg.data_dir()),
        None => AuraTools::new(driver),
    };
    #[cfg(not(feature = "grpc"))]
    let tools = AuraTools::new(driver);

    // M13 直连 agent 观测通道：仅在反连配置存在时创建（观测事件经反连流上报 controller）——sink 交 http
    // 传输中间件采集、rx 交反连 drainer 上报。无反连（纯 MCP 节点）时 sink 为 None，观测中间件不挂、
    // 行为与既往零变化。有界 cap（1024）防事件积压致内存无界增长（满即丢，best-effort）。
    #[cfg(feature = "grpc")]
    let (activity_sink, activity_rx) = match reverse_cfg.as_ref() {
        Some(_) => {
            let (sink, rx) = transport::agent_obs::channel(1024);
            (Some(sink), Some(rx))
        }
        None => (None, None),
    };
    #[cfg(not(feature = "grpc"))]
    let activity_sink: Option<transport::agent_obs::ActivitySink> = None;

    // gRPC 反连（M2）：配置了 --controller 时 spawn 反连任务，与 MCP 传输并存。
    // 反连任务内部自带断流重连（退避 + 抖动）永不退出；无 --controller（或未启用 grpc feature）
    // 时不 spawn，行为与 M1 完全一致（向后兼容）。AuraTools 廉价 Clone（Arc 组）共享同一 driver。
    // M13：activity_rx 一并交 run——反连 drainer 消费观测事件转 AgentActivity 帧上报。
    #[cfg(feature = "grpc")]
    if let Some(reverse_cfg) = reverse_cfg {
        let tools_grpc = tools.clone();
        tokio::spawn(async move {
            transport::grpc_reverse::run(tools_grpc, reverse_cfg, activity_rx).await;
        });
    }

    // 优雅退出（M16 T1.4）：装 SIGTERM/SIGINT（Windows Ctrl-C/Ctrl-Break）handler。容器内
    // aura-node 常为 PID1，无 handler 时对 SIGTERM 免疫（内核不为 PID1 投默认处置）→ 只能 SIGKILL
    // 硬杀（selkies 实测 kill 无效、pod 不优雅停止），亦阻塞 self-update 自重启。收到信号即 flush
    // 在录会话后干净退出（退出码 0）；serve 正常返回（如 stdio 客户端断开）时行为与既往一致。
    let shutdown_tools = tools.clone();
    let serve = async move {
        match cli.transport {
            TransportCmd::Stdio => transport::mcp_stdio::serve(tools).await,
            TransportCmd::Http { bind } => {
                transport::mcp_http::serve(tools, bind, activity_sink).await
            }
            // enroll/renew 已在 driver 装配前短路返回（feature enroll）；此处不可达，仅为 match 穷尽。
            #[cfg(feature = "enroll")]
            TransportCmd::Enroll(_) | TransportCmd::Renew(_) => {
                unreachable!("enroll/renew handled before driver setup")
            }
            // service 已在 driver 装配前短路返回；此处不可达，仅为 match 穷尽。
            TransportCmd::Service(_) => unreachable!("service handled before driver setup"),
        }
    };
    tokio::select! {
        r = serve => r,
        _ = shutdown_signal() => {
            tracing::info!("shutdown signal received; flushing recordings and exiting cleanly");
            shutdown_tools.flush_recordings_on_shutdown().await;
            Ok(())
        }
    }
}

/// 关停信号：Unix SIGTERM/SIGINT，Windows Ctrl-C/Ctrl-Break。任一到达即 resolve，交主循环
/// select! 触发优雅退出。容器内 PID1 对默认信号处置免疫，须显式装 handler 才能 `kill`/`docker stop`
/// 优雅停止（M16 T1.4）。handler 安装失败属进程级致命配置错误，直接 panic 于启动期暴露而非静默降级。
async fn shutdown_signal() {
    #[cfg(unix)]
    {
        use tokio::signal::unix::{signal, SignalKind};
        let mut term = signal(SignalKind::terminate()).expect("install SIGTERM handler");
        let mut interrupt = signal(SignalKind::interrupt()).expect("install SIGINT handler");
        tokio::select! {
            _ = term.recv() => tracing::info!("received SIGTERM"),
            _ = interrupt.recv() => tracing::info!("received SIGINT"),
        }
    }
    #[cfg(windows)]
    {
        use tokio::signal::windows;
        let mut ctrl_c = windows::ctrl_c().expect("install Ctrl-C handler");
        let mut ctrl_break = windows::ctrl_break().expect("install Ctrl-Break handler");
        tokio::select! {
            _ = ctrl_c.recv() => tracing::info!("received Ctrl-C"),
            _ = ctrl_break.recv() => tracing::info!("received Ctrl-Break"),
        }
    }
    // 其余平台（理论上不达）：永挂，退化为无信号优雅退出（serve 仍可正常返回）。
    #[cfg(not(any(unix, windows)))]
    {
        std::future::pending::<()>().await;
    }
}
