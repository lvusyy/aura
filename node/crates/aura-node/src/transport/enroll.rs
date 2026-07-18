//! 设备接入 enroll/renew 客户端（feature enroll 门控，M12 TASK-006）。
//!
//! 一次性 CLI 腿（TASK-009 安装器调用的核心）：生成密钥对（**私钥永不离节点**，CSR 路线 Q1）→
//! 构造 PKCS#10 CSR → HTTPS POST 控制面签发面换 per-node 证书 → 落盘。两子命令：
//!   - `aura-node enroll`：POST /v1/enroll（server-TLS + 钉 CA / body 内 enroll token 认证，节点此刻无
//!     客户端证书）→ 收 {node_id, node_cert_pem, ca_cert_pem} 落盘 <data-dir>/{node.key,node.crt,ca.crt,node_id}。
//!   - `aura-node renew`：POST /v1/renew（mTLS，现 per-node 证书自证）→ 换新证书（新密钥轮换，旧证书自然过期）。
//!
//! 私钥落 <data-dir>/node.key（0600，永不上传）；enroll 后节点以 per-node cert 反连注册（grpc_reverse），
//! 现身 fleet。第三方 crypto/HTTP 隔离在本模块小函数内（rcgen CSR / reqwest HTTPS），具体 API 以构建机
//! docs.rs 为准——**待远程构建验证（T010，node sync-and-build）**，同 grpc_reverse.rs tonic 0.14 API 先例。

use std::path::{Path, PathBuf};
use std::time::Duration;

use anyhow::{anyhow, Context, Result};
use serde::{Deserialize, Serialize};

/// enroll 单次 HTTP 硬超时（换证是短交互；网络故障快速失败不无限挂）。
const ENROLL_TIMEOUT: Duration = Duration::from_secs(30);

/// `aura-node enroll` CLI 参数（clap Args）。
#[derive(clap::Args, Clone, Debug)]
pub struct EnrollArgs {
    /// 控制面 enroll 端点 host:port（HA 用 VIP:18080）。
    #[arg(long)]
    pub controller: String,
    /// 一次性 enroll token（console「添加设备」生成，随一键命令下发）。
    #[arg(long)]
    pub token: String,
    /// 平台（windows|linux|macos|android）；缺省由本机 OS 推断（一键命令按平台显式传）。
    #[arg(long)]
    pub platform: Option<String>,
    /// 用户标签（M12 舰队分组）：随 enroll 上报作 label 引导值；缺省空。
    #[arg(long)]
    pub label: Option<String>,
    /// 用户位置（M12 舰队展示）：随 enroll 上报；缺省空。
    #[arg(long)]
    pub location: Option<String>,
    /// 控制面 CA 证书路径（PEM）：TOFU pin 校验 controller server 身份（design §3.3，闭合中间人）。
    /// 安装器随附 ca.crt；enroll 以此校验后再发 CSR+token。
    #[arg(long)]
    pub ca: PathBuf,
    /// 节点数据目录（node.key/node.crt/ca.crt/node_id 落盘根）；缺省 $AURA_DATA_DIR 或 ~/.aura。
    #[arg(long)]
    pub data_dir: Option<PathBuf>,
}

/// `aura-node renew` CLI 参数（clap Args）。持现 per-node 证书走 mTLS 换新，无需 token。
#[derive(clap::Args, Clone, Debug)]
pub struct RenewArgs {
    /// 控制面 renew 端点 host:port（HA 用 VIP:7443，mTLS）。
    #[arg(long)]
    pub controller: String,
    /// 节点数据目录（读现 node.key/node.crt/ca.crt，写回新证书）；缺省同 enroll。
    #[arg(long)]
    pub data_dir: Option<PathBuf>,
}

// —— /v1/enroll、/v1/renew 契约（与 controller enroll_rest.go 同形）——————————————————————————————

#[derive(Serialize)]
struct EnrollRequest<'a> {
    token: &'a str,
    csr_pem: &'a str,
    platform: &'a str,
    hostname: &'a str,
    label: &'a str,
}

#[derive(Deserialize)]
struct EnrollResponse {
    node_id: String,
    node_cert_pem: String,
    ca_cert_pem: String,
}

#[derive(Serialize)]
struct RenewRequest<'a> {
    csr_pem: &'a str,
    platform: &'a str,
}

#[derive(Deserialize)]
struct RenewResponse {
    node_cert_pem: String,
    ca_cert_pem: String,
}

// —— 子命令入口 ————————————————————————————————————————————————————————————————

/// `aura-node enroll` 执行：genkey+CSR（私钥落盘不上传）→ POST /v1/enroll → 落盘 cert/ca/node_id。
pub fn run_enroll(args: EnrollArgs) -> Result<()> {
    let data_dir = args.data_dir.clone().unwrap_or_else(super::default_data_dir);
    std::fs::create_dir_all(&data_dir)
        .with_context(|| format!("create data dir {}", data_dir.display()))?;
    let platform = args.platform.clone().unwrap_or_else(default_platform);

    // 1) 生成密钥对 + CSR（私钥永不离节点，CSR 路线 Q1）。私钥先落盘 0600（换证成功前即持有，重试幂等）。
    let (key_pem, csr_pem) = generate_keypair_and_csr()?;
    write_private_key(&data_dir.join("node.key"), &key_pem)?;

    // 2) POST /v1/enroll（server-TLS 钉 CA + body token 认证）。
    let ca_pem = std::fs::read(&args.ca)
        .with_context(|| format!("read pinned CA {}", args.ca.display()))?;
    let req = EnrollRequest {
        token: &args.token,
        csr_pem: &csr_pem,
        platform: &platform,
        hostname: &hostname(),
        label: args.label.as_deref().unwrap_or(""),
    };
    let resp: EnrollResponse = http_post_enroll(&args.controller, &ca_pem, &req)?;

    // 3) 落盘 cert/ca/node_id（后续反连以 per-node cert 自证，grpc_reverse 读 <data-dir>/{node.crt,node.key,ca.crt,node_id}）。
    std::fs::write(data_dir.join("node.crt"), &resp.node_cert_pem).context("write node.crt")?;
    std::fs::write(data_dir.join("ca.crt"), &resp.ca_cert_pem).context("write ca.crt")?;
    std::fs::write(data_dir.join("node_id"), &resp.node_id).context("write node_id")?;
    eprintln!(
        "aura-node enrolled: node_id={} platform={} data_dir={}",
        resp.node_id,
        platform,
        data_dir.display()
    );
    Ok(())
}

/// `aura-node renew` 执行：现 per-node 证书 mTLS 自证 → 新密钥 CSR → POST /v1/renew → 换新证书落盘。
pub fn run_renew(args: RenewArgs) -> Result<()> {
    let data_dir = args.data_dir.clone().unwrap_or_else(super::default_data_dir);
    let node_key = data_dir.join("node.key");
    let node_crt = data_dir.join("node.crt");
    let ca_crt = data_dir.join("ca.crt");

    // mTLS 客户端身份 = 现 node.crt + node.key（reqwest Identity 取 cert 链 + 私钥的合并 PEM）。
    let cur_cert = std::fs::read(&node_crt)
        .with_context(|| format!("read current cert {} (enroll first?)", node_crt.display()))?;
    let cur_key = std::fs::read(&node_key)
        .with_context(|| format!("read current key {}", node_key.display()))?;
    let ca_pem = std::fs::read(&ca_crt).with_context(|| format!("read {}", ca_crt.display()))?;
    let mut identity_pem = cur_cert;
    identity_pem.extend_from_slice(b"\n");
    identity_pem.extend_from_slice(&cur_key);

    // 新密钥 + CSR（轮换密钥更安全；私钥仍不离节点）。
    let (new_key_pem, csr_pem) = generate_keypair_and_csr()?;
    let platform = default_platform();
    let req = RenewRequest { csr_pem: &csr_pem, platform: &platform };
    let resp: RenewResponse = http_post_renew(&args.controller, &ca_pem, &identity_pem, &req)?;

    // 换证落盘：新私钥（0600）+ 新证书 + 信任根。旧证书留待自然过期（或 console 吊销）。
    write_private_key(&node_key, &new_key_pem)?;
    std::fs::write(&node_crt, &resp.node_cert_pem).context("write renewed node.crt")?;
    std::fs::write(&ca_crt, &resp.ca_cert_pem).context("write ca.crt")?;
    eprintln!("aura-node cert renewed at {}", data_dir.display());
    Ok(())
}

// —— 第三方隔离：CSR 生成（rcgen）——————————————————————————————————————————————————————
// 待远程构建验证（T010）：rcgen 0.13 API（KeyPair::generate / KeyPair::serialize_pem /
// CertificateParams::new / distinguished_name / serialize_request / CertificateSigningRequest::pem）
// 以构建机 docs.rs 为准；ring 后端（与 tonic tls-ring 同 ring，不引 aws-lc）。

/// 生成 ECDSA P-256 密钥对 + PKCS#10 CSR，返回 (私钥 PEM, CSR PEM)。CN 占位「aura-node-enroll」——
/// controller 签发时覆写为分配的 node-id（不信 CSR 携带 CN，design §3.4）。
fn generate_keypair_and_csr() -> Result<(String, String)> {
    use rcgen::{CertificateParams, DistinguishedName, DnType, KeyPair};

    let key_pair = KeyPair::generate().context("generate node keypair")?;
    let key_pem = key_pair.serialize_pem();

    let mut params = CertificateParams::new(Vec::<String>::new()).context("build CSR params")?;
    let mut dn = DistinguishedName::new();
    dn.push(DnType::CommonName, "aura-node-enroll"); // 占位，controller 覆写为 node-id
    params.distinguished_name = dn;

    let csr = params
        .serialize_request(&key_pair)
        .context("serialize CSR")?;
    let csr_pem = csr.pem().context("encode CSR PEM")?;
    Ok((key_pem, csr_pem))
}

// —— 第三方隔离：HTTPS POST（reqwest blocking + rustls）—————————————————————————————————————
// 待远程构建验证（T010）：reqwest 0.12 blocking API（Client::builder / add_root_certificate /
// identity / use_rustls_tls / json / send / status / json）以构建机 docs.rs 为准。default-features=false
// 去 native-tls（不引 openssl 原生库）；rustls provider 由 reqwest 自装——enroll 为独立一次性进程，与
// grpc 服务面 tonic(tls-ring) 不同时活，无 rustls CryptoProvider 进程级冲突。

/// POST /v1/enroll（server-TLS + 钉 CA，token 在 body）。非 2xx 即 Err（含状态码 + 错误体，便于诊断
/// 401 token 无效 / 400 CSR 拒）。
fn http_post_enroll(controller: &str, ca_pem: &[u8], req: &EnrollRequest) -> Result<EnrollResponse> {
    let client = reqwest::blocking::Client::builder()
        .add_root_certificate(reqwest::Certificate::from_pem(ca_pem).context("parse pinned CA")?)
        .use_rustls_tls()
        .timeout(ENROLL_TIMEOUT)
        .build()
        .context("build enroll HTTP client")?;
    let url = format!("https://{}/v1/enroll", controller);
    let resp = client.post(&url).json(req).send().with_context(|| format!("POST {}", url))?;
    let status = resp.status();
    if !status.is_success() {
        let body = resp.text().unwrap_or_default();
        return Err(anyhow!("enroll rejected: HTTP {} {}", status.as_u16(), body.trim()));
    }
    resp.json::<EnrollResponse>().context("decode enroll response")
}

/// POST /v1/renew（mTLS：identity_pem = node.crt + node.key 合并 PEM 作客户端身份）。
fn http_post_renew(
    controller: &str,
    ca_pem: &[u8],
    identity_pem: &[u8],
    req: &RenewRequest,
) -> Result<RenewResponse> {
    let client = reqwest::blocking::Client::builder()
        .add_root_certificate(reqwest::Certificate::from_pem(ca_pem).context("parse CA")?)
        .identity(reqwest::Identity::from_pem(identity_pem).context("load client identity (node.crt+node.key)")?)
        .use_rustls_tls()
        .timeout(ENROLL_TIMEOUT)
        .build()
        .context("build renew HTTP client")?;
    let url = format!("https://{}/v1/renew", controller);
    let resp = client.post(&url).json(req).send().with_context(|| format!("POST {}", url))?;
    let status = resp.status();
    if !status.is_success() {
        let body = resp.text().unwrap_or_default();
        return Err(anyhow!("renew rejected: HTTP {} {}", status.as_u16(), body.trim()));
    }
    resp.json::<RenewResponse>().context("decode renew response")
}

// —— 小工具 ————————————————————————————————————————————————————————————————————

/// 写私钥文件（Unix 0600 仅所有者可读；Windows 靠部署目录 ACL）。私钥红线：永不上传（design §10）。
fn write_private_key(path: &Path, pem: &str) -> Result<()> {
    use std::io::Write;
    let mut opts = std::fs::OpenOptions::new();
    opts.write(true).create(true).truncate(true);
    #[cfg(unix)]
    {
        use std::os::unix::fs::OpenOptionsExt;
        opts.mode(0o600);
    }
    let mut f = opts.open(path).with_context(|| format!("open {}", path.display()))?;
    f.write_all(pem.as_bytes())
        .with_context(|| format!("write {}", path.display()))?;
    Ok(())
}

/// 本机 OS 推断平台（enroll --platform 缺省兜底）。std::env::consts::OS 返 "linux"/"macos"/"windows"/
/// "android"，与 controller platform_scope 词汇一致（一键命令按平台显式传，本兜底覆盖手动 enroll）。
fn default_platform() -> String {
    std::env::consts::OS.to_string()
}

/// 宿主 hostname（enroll payload hostname，审计事实列）。gethostname 跨平台（Cargo.lock 既有，随 enroll
/// feature 入树）；非法 UTF-8 经 to_string_lossy 兜底。
fn hostname() -> String {
    gethostname::gethostname().to_string_lossy().into_owned()
}

// —— 单测（批E C8：本文件此前零测试——密钥引导是安全红线路径）——————————————————————————
// 覆盖纯本地逻辑：CSR/私钥 PEM 结构、密钥随机性、私钥落盘 0600 权限位与覆盖写。HTTP 非 2xx 分支
// 需真 HTTPS 端点（钉 CA + mTLS），经构建机 enroll e2e 覆盖（T11 mac 实测含 401/400 路径），单测
// 不起 TLS server（测试基建成本＞收益，且 reqwest 面已按第三方隔离规约收敛在两个小函数内）。

#[cfg(test)]
mod tests {
    use super::*;

    /// 唯一临时目录（不引 tempfile 依赖：进程 id + 计数器保并发用例不撞路径）。
    fn temp_dir(tag: &str) -> PathBuf {
        use std::sync::atomic::{AtomicU32, Ordering};
        static SEQ: AtomicU32 = AtomicU32::new(0);
        let dir = std::env::temp_dir().join(format!(
            "aura-enroll-test-{}-{}-{}",
            std::process::id(),
            tag,
            SEQ.fetch_add(1, Ordering::Relaxed)
        ));
        std::fs::create_dir_all(&dir).expect("create temp dir");
        dir
    }

    /// CSR 与私钥产出为对应 PEM 结构（私钥 PKCS#8、CSR PKCS#10），CSR 不含私钥字节（私钥不离节点红线）。
    #[test]
    fn generate_keypair_and_csr_produces_pem_pair() {
        let (key_pem, csr_pem) = generate_keypair_and_csr().expect("generate");
        assert!(key_pem.contains("BEGIN PRIVATE KEY"), "key must be PKCS#8 PEM: {key_pem}");
        assert!(csr_pem.contains("BEGIN CERTIFICATE REQUEST"), "csr must be PKCS#10 PEM: {csr_pem}");
        // CSR 载荷不得夹带私钥（结构性防呆：两 PEM 块类型互斥）。
        assert!(!csr_pem.contains("PRIVATE KEY"), "csr must not embed the private key");
    }

    /// 两次生成密钥互异（随机源真在工作；恒同即密钥引导整体失效）。
    #[test]
    fn generated_keypairs_are_unique() {
        let (a, _) = generate_keypair_and_csr().expect("first");
        let (b, _) = generate_keypair_and_csr().expect("second");
        assert_ne!(a, b, "two generated private keys must differ");
    }

    /// 私钥落盘：内容往返一致 + 覆盖写 truncate（renew 换新钥不残留旧字节）。
    #[test]
    fn write_private_key_roundtrip_and_truncate() {
        let dir = temp_dir("roundtrip");
        let path = dir.join("node.key");
        write_private_key(&path, "NEW-KEY-LONG-CONTENT").expect("first write");
        write_private_key(&path, "SHORT").expect("overwrite");
        let got = std::fs::read_to_string(&path).expect("read back");
        assert_eq!(got, "SHORT", "overwrite must truncate previous content");
        std::fs::remove_dir_all(&dir).ok();
    }

    /// Unix 私钥权限位 0600（仅所有者可读写——design §10 私钥红线的落盘面）。
    #[cfg(unix)]
    #[test]
    fn write_private_key_sets_owner_only_mode() {
        use std::os::unix::fs::PermissionsExt;
        let dir = temp_dir("mode");
        let path = dir.join("node.key");
        write_private_key(&path, "SECRET").expect("write");
        let mode = std::fs::metadata(&path).expect("stat").permissions().mode() & 0o777;
        assert_eq!(mode, 0o600, "private key must be 0600, got {mode:o}");
        std::fs::remove_dir_all(&dir).ok();
    }

    /// 平台推断兜底与 controller platform_scope 词表同域（linux/macos/windows/android）。
    #[test]
    fn default_platform_is_nonempty_known_os() {
        let p = default_platform();
        assert!(!p.is_empty());
        assert_eq!(p, std::env::consts::OS);
    }
}
