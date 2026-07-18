//! 构建脚本：Windows 目标嵌入 Per-Monitor V2 DPI manifest；grpc feature 开启时用 tonic-build 生成反连 client。
//!
//! PMv2 须在任何 HWND 创建前生效——manifest 由加载器在进程启动即应用，天然满足。
//! 声明后进程看到各屏原生物理像素，截图与 SendInput 坐标 1:1（XGA 缩放回映射的前提）。
//! 仅 Windows(MSVC) 生效：经 link.exe `/MANIFEST:EMBED` + `/MANIFESTINPUT` 与 rustc
//! 默认 manifest（longPathAware 等）由 mt.exe 合并嵌入，避免 RT_MANIFEST 资源 ID 冲突。
//! 不设 requestedExecutionLevel（asInvoker）；High 完整性由 W1 schtasks /rl HIGHEST 提供，避双 UAC。
//! 三平台原生构建（不交叉编译，决策4），故 build.rs 的 `#[cfg(windows)]` 即目标平台门控。

fn main() {
    #[cfg(windows)]
    {
        embed_dpi_manifest();
    }

    compile_grpc_proto();
}

/// grpc feature 开启时，用 tonic-build 从仓库根 proto 生成反连 client 代码。
///
/// build script 编译时不带包 feature 的 cfg，无法用 `#[cfg(feature = "grpc")]`；
/// Cargo 为已激活 feature 设置环境变量 `CARGO_FEATURE_<NAME>`，故读运行期
/// `CARGO_FEATURE_GRPC` 判断。grpc 关闭（M1 默认）时直接返回、不触碰 proto，
/// 保证 M1 无 proto/无 protoc 亦能正常构建。
fn compile_grpc_proto() {
    if std::env::var_os("CARGO_FEATURE_GRPC").is_none() {
        return;
    }

    // build.rs 的 CWD 为本 crate 根（node/crates/aura-node/），proto 在仓库根（上溯 3 级）。
    let proto = "../../../proto/aura/v1/node.proto";
    let proto_root = "../../../proto";

    // proto 变更触发重新生成。
    println!("cargo:rerun-if-changed={proto}");

    // 节点侧只作为反连 client 拨出，无需 server 代码（build_server(false)）。
    //
    // build_transport(false) 关键：本 service 的 rpc 名为 Connect，生成的 client RPC 方法为
    // `connect(&mut self, ...)`；而 tonic 默认还会在 `impl Client<Channel>` 生成便捷构造器
    // `connect(dst)`，两者同名同类型 → E0592 重复定义编译失败。关掉 transport 便捷构造器即消解冲突，
    // 我们本就手工建 Channel + `Client::new(channel)`，不依赖该便捷构造器。
    //
    // 注意：生成依赖 protoc（prost-build 需系统 protoc 或 PROTOC 环境变量），
    // 远程构建机须预装 protobuf-compiler。
    tonic_prost_build::configure()
        .build_server(false)
        .build_client(true)
        .build_transport(false)
        .compile_protos(&[proto], &[proto_root])
        .expect("failed to compile aura.v1 proto for grpc reverse client");
}

/// 经 MSVC 链接器把 PMv2 manifest 合并进可执行文件嵌入 manifest。
#[cfg(windows)]
fn embed_dpi_manifest() {
    let manifest =
        std::path::Path::new(env!("CARGO_MANIFEST_DIR")).join("aura-node.exe.manifest");
    println!("cargo:rerun-if-changed={}", manifest.display());
    // link.exe 合并本 manifest 与 rustc 默认 manifest；仅作用于 bin 目标。
    println!("cargo:rustc-link-arg-bins=/MANIFEST:EMBED");
    println!("cargo:rustc-link-arg-bins=/MANIFESTINPUT:{}", manifest.display());
}
