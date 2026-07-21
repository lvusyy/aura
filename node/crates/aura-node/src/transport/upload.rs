//! 大产物旁路上传 HTTP 客户端（feature grpc 门控）。
//!
//! >16MB 产物（录屏 / 大 file_pull）绕开 gRPC 双向流 16MB 内联上限：节点收控制面下发的
//! `UploadUrlGrant`（预签名 PUT URL）后，经本模块直连对象存储 HTTP PUT 上传，回 `UploadComplete`。
//!
//! 传输选用 hyper-util 既有依赖链（随 tonic grpc feature 入树），不新引 TLS provider，
//! 与 tonic `tls-ring` 无 rustls CryptoProvider 冲突。当前仅支持 `http://` 预签名端点
//! （对象存储内网明文端点，M3 旁路轨道与冒烟足够）；`https://` 端点留待后续 hyper-rustls(ring) 接线。

use std::net::IpAddr;
use std::path::Path;
use std::time::Duration;

use anyhow::{anyhow, bail, Context, Result};
use http_body_util::{BodyExt, Full};
use hyper::body::Bytes;
use hyper::{Method, Request};
use hyper_util::client::legacy::Client;
use hyper_util::rt::TokioExecutor;

// 源地址绑定 connector 单点构造（两建连点共用清单，grpc_reverse.rs 定义——契约 §5.2 单点钉死）。
use super::grpc_reverse::local_bound_connector;

/// 经预签名 URL 将本地文件 HTTP PUT 上传对象存储，返回对象 ETag（去引号）。
///
/// 读整文件入内存后单次 PUT（M3 轨道产物 16MB 量级可接受；流式大上传留待后续）。
/// 读取用 `tokio::fs::read`（异步，workspace tokio 已启 `fs` feature）：~120MB 录屏产物读盘
/// 不阻塞 tokio worker 线程（原同步阻塞读会占住 worker，拖累并发工具执行，ISS-007）。
/// `local_addr`：出站源 IP 绑定（--local-addr，多宿主机确定性选源）；None 由内核路由选源。
pub async fn put_file(presigned_url: &str, path: &Path, local_addr: Option<IpAddr>) -> Result<String> {
    let body = tokio::fs::read(path)
        .await
        .with_context(|| format!("read upload file {}", path.display()))?;
    put_bytes(presigned_url, body, local_addr).await
}

/// 单次旁路上传的硬超时上限：网络故障（对象存储不读 body 而挂起）时快速失败，
/// 不无限持有连接。重试策略由消费侧（TASK-010）在此之上决定。
const PUT_TIMEOUT: Duration = Duration::from_secs(300);

/// 单次制品下载硬超时（M16 self-update 下载腿；与 PUT_TIMEOUT 同数系——控制面等待窗 330s 据此推导）。
const GET_TIMEOUT: Duration = Duration::from_secs(300);

/// 经预签名 URL GET 下载对象到本地文件（M16 self-update 下载腿），返回字节数。
/// 整体收内存后落盘（制品 ~20-40MB，与 put 腿同哲学）；http-only 约束同 PUT 腿（scheme 关把守）。
pub async fn get_file(presigned_url: &str, dest: &Path, local_addr: Option<IpAddr>) -> Result<u64> {
    let uri: hyper::Uri = presigned_url.parse().context("parse presigned url")?;
    if uri.scheme_str() != Some("http") {
        bail!(
            "only http presigned endpoints are supported on the release download path (got {:?}); \
             configure the object store with a plaintext node-reachable endpoint",
            uri.scheme_str()
        );
    }

    let req = Request::builder()
        .method(Method::GET)
        .uri(uri)
        .body(Full::new(Bytes::new()))
        .context("build GET request")?;
    let client: Client<_, Full<Bytes>> =
        Client::builder(TokioExecutor::new()).build(local_bound_connector(local_addr));

    // 建连→响应头→完整体单伞硬超时：头/体各套 300s 会叠加至 600s，越过控制面 330s 等待窗——单伞
    // 保证节点侧先于控制面兜底判死，不出现「控制面已报失败、节点稍后仍换刀」的状态分裂（对象存储
    // 半途挂起同样快速失败；download 腿无重试，失败上抛 SelfUpdateResult）。
    let (status, body) = tokio::time::timeout(GET_TIMEOUT, async {
        let resp = client.request(req).await.context("send GET request")?;
        let status = resp.status();
        let body = resp.into_body().collect().await.context("read GET body")?.to_bytes();
        Ok::<_, anyhow::Error>((status, body))
    })
    .await
    .map_err(|_| anyhow!("presigned GET timed out after {}s", GET_TIMEOUT.as_secs()))??;
    if !status.is_success() {
        return Err(anyhow!("presigned GET failed: HTTP {}", status.as_u16()));
    }

    if let Some(parent) = dest.parent() {
        tokio::fs::create_dir_all(parent)
            .await
            .with_context(|| format!("create staging dir {}", parent.display()))?;
    }
    tokio::fs::write(dest, &body)
        .await
        .with_context(|| format!("write staged file {}", dest.display()))?;
    Ok(body.len() as u64)
}

/// 经预签名 URL PUT 一段字节，返回对象 ETag（去引号）。非 2xx 响应即 Err（含状态码）。
pub async fn put_bytes(presigned_url: &str, body: Vec<u8>, local_addr: Option<IpAddr>) -> Result<String> {
    let uri: hyper::Uri = presigned_url.parse().context("parse presigned url")?;
    if uri.scheme_str() != Some("http") {
        bail!(
            "only http presigned endpoints are supported on the bypass upload path (got {:?}); \
             configure the object store with a plaintext node-reachable endpoint",
            uri.scheme_str()
        );
    }

    // Full<Bytes> 为已知长度 body，hyper 据此自动设置 Content-Length（对象存储 PUT 需之）。
    let req = Request::builder()
        .method(Method::PUT)
        .uri(uri)
        .body(Full::new(Bytes::from(body)))
        .context("build PUT request")?;

    // 明文连接器（不引 TLS provider，与 tonic tls-ring 隔离）：本腿 http-only 由上方 scheme 关把守，
    // 共用 local_bound_connector 统一源地址绑定与设置清单（enforce_http(false) 对本腿无害）。
    let client: Client<_, Full<Bytes>> =
        Client::builder(TokioExecutor::new()).build(local_bound_connector(local_addr));

    // 硬超时：对象存储不读 body 而挂起时快速失败，避免连接无限泄漏（冒烟期真实踩中跨网段丢包挂起）。
    let resp = tokio::time::timeout(PUT_TIMEOUT, client.request(req))
        .await
        .map_err(|_| anyhow!("presigned PUT timed out after {}s", PUT_TIMEOUT.as_secs()))?
        .context("send PUT request")?;
    let status = resp.status();
    let etag = resp
        .headers()
        .get(hyper::header::ETAG)
        .and_then(|v| v.to_str().ok())
        .unwrap_or("")
        .trim_matches('"')
        .to_string();

    // 收尽响应体以释放连接（预签名 PUT 响应体通常为空）。
    let _ = resp.into_body().collect().await;

    if !status.is_success() {
        return Err(anyhow!("presigned PUT failed: HTTP {}", status.as_u16()));
    }
    Ok(etag)
}

#[cfg(test)]
mod tests {
    use super::*;

    /// https 端点在当前旁路轨道上明确拒绝（HTTP-only，避免静默走错 TLS provider）。
    #[tokio::test]
    async fn https_endpoint_is_rejected() {
        let err = put_bytes("https://example.com/o", vec![1, 2, 3], None)
            .await
            .unwrap_err();
        assert!(err.to_string().contains("only http"));
    }

    /// 非法 URL 直接 Err（不 panic）。
    #[tokio::test]
    async fn malformed_url_errors() {
        assert!(put_bytes("not a url", vec![1], None).await.is_err());
    }

    /// SC-1 (b)：upload 腿 --local-addr 源绑定——accept 侧 peer IP 即出站源 IP。
    /// 假 listener 不回 HTTP 响应，put_bytes 会阻塞等响应：断言在 accept 侧，任务 abort 收尾。
    #[tokio::test]
    async fn upload_leg_binds_local_source_addr() {
        // try-bind 探测（比平台 cfg 假设诚实）：mac 无 lo0 alias 时 127.0.0.2 不可绑，跳过。
        if std::net::TcpListener::bind(("127.0.0.2", 0)).is_err() {
            eprintln!("skip: 127.0.0.2 not bindable on this host");
            return;
        }
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let port = listener.local_addr().unwrap().port();

        let put = tokio::spawn(async move {
            let _ = put_bytes(
                &format!("http://127.0.0.1:{port}/k"),
                vec![1, 2, 3],
                Some("127.0.0.2".parse().unwrap()),
            )
            .await;
        });

        let (_stream, peer) = tokio::time::timeout(Duration::from_secs(5), listener.accept())
            .await
            .expect("accept should fire within 5s")
            .unwrap();
        assert_eq!(peer.ip(), "127.0.0.2".parse::<IpAddr>().unwrap());
        put.abort();
    }

    /// 冒烟（默认忽略）：真实链路 PUT——控制面签发的预签名 URL 经 env 传入，PUT 本地 >16MB 文件。
    /// 运行：
    ///   AURA_SMOKE_PUT_URL=<presigned> AURA_SMOKE_FILE=/path/to/big.bin \
    ///     cargo test -p aura-node --features grpc -- --ignored --nocapture smoke_put_file
    #[tokio::test]
    #[ignore]
    async fn smoke_put_file() {
        let url = std::env::var("AURA_SMOKE_PUT_URL").expect("AURA_SMOKE_PUT_URL");
        let file = std::env::var("AURA_SMOKE_FILE").expect("AURA_SMOKE_FILE");
        let path = std::path::PathBuf::from(&file);
        let size = std::fs::metadata(&path).expect("stat smoke file").len();
        assert!(
            size > 16 * 1024 * 1024,
            "smoke file must exceed the 16MB gRPC inline cap, got {size} bytes"
        );
        let etag = put_file(&url, &path, None).await.expect("presigned PUT");
        eprintln!("SMOKE-PUT-OK size={size} etag={etag}");
        assert!(!etag.is_empty(), "expected non-empty ETag from object store");
    }
}
