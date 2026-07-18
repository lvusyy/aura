//! assert 域工具（1 个：assert）+ assert_router + baseline 存取（M11 REC-4）。
//!
//! 入参复用 capability 层 [`AssertParams`]（自带 JsonSchema），MCP schema 与能力层同源；
//! gRPC 反连侧经 `tool_dispatch` 注册表派发同一执行核，MCP/gRPC 两侧工具集由集合相等断言强制同步。
//! Locked-7：断言不成立（passed:false）是正常数据，信封仍 `ok:true`，无新错误码。
//!
//! baseline 存储（plan D9）：读写全部落本传输层（capability 纯函数层零 IO——`eval_image_assert`
//! 与 `AssertDriver` 默认方法零改），路径 `<data_dir>/baselines/<key>.webp` 复用 data_dir 单源
//! （[`AuraTools::with_data_dir`]，`<data_dir>/artifacts` 先例）。[`resolve_baseline`] 为 MCP 面
//! 与 gRPC 面（`tool_dispatch::dispatch_assert`）共用的预处理函数——双路 baseline 语义一致。

use std::path::{Path, PathBuf};

use base64::engine::general_purpose::STANDARD;
use base64::Engine as _;
use rmcp::handler::server::wrapper::Parameters;
use rmcp::{tool, tool_router, Json};

use aura_capability::{AssertParams, AssertResult, CapError, Envelope, ScreenshotOpts};

use super::AuraTools;

/// assert 的 baseline 预处理（MCP 与 gRPC 反连两侧共用，plan D9 双路一致）。三分支：
///   ① `save_baseline=true`：`baseline_key` 必填（缺 → E_INVALID_ARG）→ 截当前帧（缺省编码，
///      与 assert image 比对帧同管线）→ 写 `<data_dir>/baselines/<key>.webp` → 短路返回
///      `Some(AssertResult{passed:true, detail:"baseline saved: <key>"})`，不做 diff；
///   ② `baseline_key` 给定且 `reference_image_base64` 缺省：读盘 base64 填入参考图（文件缺失 →
///      E_INVALID_ARG "baseline not found: <key>"，结构化错误禁 panic）→ 返回 `None` 继续走
///      既有 driver.assert；
///   ③ 两者皆缺省：直通返回 `None`（既有调用 wire 零变）。
/// key 经白名单 `[A-Za-z0-9._-]+` 校验（无路径分隔符，防路径穿越）。
pub(crate) async fn resolve_baseline(
    tools: &AuraTools,
    params: &mut AssertParams,
) -> Result<Option<AssertResult>, CapError> {
    if params.save_baseline == Some(true) {
        let key = params.baseline_key.as_deref().ok_or_else(|| {
            CapError::InvalidArg("save_baseline requires 'baseline_key'".to_string())
        })?;
        let shot = tools
            .driver
            .screenshot(None, ScreenshotOpts::default())
            .await?;
        save_baseline_file(&tools.data_dir, key, &shot.image_base64)?;
        return Ok(Some(AssertResult {
            passed: true,
            detail: format!("baseline saved: {key}"),
            matched: None,
            diff_score: None,
        }));
    }
    if params.reference_image_base64.is_none() {
        if let Some(key) = params.baseline_key.as_deref() {
            params.reference_image_base64 = Some(load_baseline_file(&tools.data_dir, key)?);
        }
    }
    Ok(None)
}

/// baseline 键白名单：`[A-Za-z0-9._-]+`（非空、无路径分隔符 / 无空白）。`.`/`-` 无分隔符语义，
/// 拼接 `<key>.webp` 后不可能逃出 baselines 目录（穿越需要 `/` 或 `\`，均不在白名单）。
fn valid_baseline_key(key: &str) -> bool {
    !key.is_empty()
        && key
            .chars()
            .all(|c| c.is_ascii_alphanumeric() || matches!(c, '.' | '_' | '-'))
}

/// baseline 落盘路径 `<data_dir>/baselines/<key>.webp`（key 已过白名单）。
fn baseline_path(data_dir: &Path, key: &str) -> PathBuf {
    data_dir.join("baselines").join(format!("{key}.webp"))
}

/// 保存 baseline：key 白名单校验 → 建目录 → base64 解码写 WebP 二进制（真 .webp 文件，运维可
/// 直接查看）。截图产物 base64 理论恒合法，解码失败按参数错误兜底（结构化非 panic）。
fn save_baseline_file(data_dir: &Path, key: &str, image_base64: &str) -> Result<(), CapError> {
    if !valid_baseline_key(key) {
        return Err(CapError::InvalidArg(format!(
            "invalid baseline_key {key:?}: allowed charset [A-Za-z0-9._-]"
        )));
    }
    let path = baseline_path(data_dir, key);
    if let Some(parent) = path.parent() {
        std::fs::create_dir_all(parent)
            .map_err(|e| CapError::FileError(format!("create baselines dir failed: {e}")))?;
    }
    let bytes = STANDARD
        .decode(image_base64.trim())
        .map_err(|e| CapError::InvalidArg(format!("invalid baseline image base64: {e}")))?;
    std::fs::write(&path, bytes)
        .map_err(|e| CapError::FileError(format!("write baseline {key} failed: {e}")))
}

/// 读取 baseline：key 白名单校验 → 读 WebP 二进制 → base64（回填 `reference_image_base64`）。
/// 文件缺失 → E_INVALID_ARG "baseline not found: <key>"（结构化，禁 panic）；其余 IO → E_FILE_FAILED。
fn load_baseline_file(data_dir: &Path, key: &str) -> Result<String, CapError> {
    if !valid_baseline_key(key) {
        return Err(CapError::InvalidArg(format!(
            "invalid baseline_key {key:?}: allowed charset [A-Za-z0-9._-]"
        )));
    }
    let path = baseline_path(data_dir, key);
    match std::fs::read(&path) {
        Ok(bytes) => Ok(STANDARD.encode(bytes)),
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => Err(CapError::InvalidArg(format!(
            "baseline not found: {key}"
        ))),
        Err(e) => Err(CapError::FileError(format!(
            "read baseline {key} failed: {e}"
        ))),
    }
}

#[tool_router(router = assert_router, vis = "pub(crate)")]
impl AuraTools {
    /// 对界面 / 文本状态做确定性断言（text 纯文本比较 / a11y 无障碍树谓词 / image 区域视觉 diff）。
    ///
    /// MCP 注解：readOnlyHint=true（观察判定；save_baseline 仅写节点自身 data_dir 缓存，无设备副作用）。
    /// 断言不成立以 passed=false 数据返回（信封仍 ok:true），供 agent 自行分支，闭合
    /// 『观察 → 动作 → 断言』确定性环。baseline 预处理经 [`resolve_baseline`]（与 gRPC 面同函数）。
    #[tool(
        description = "Assert UI/text state deterministically. mode=text compares the provided 'actual' text against 'expect'; mode=a11y matches a node in the accessibility tree by name/role/value; mode=image diffs a screenshot region against a reference image (WebP), where 'region' [x1,y1,x2,y2] selects the compared area (default full frame), 'method' is pixel or ssim, and it passes when diff_score <= threshold (default 0.05). The reference comes from 'reference_image_base64' or from a stored baseline via 'baseline_key' (missing baseline is a structured error); set save_baseline=true with baseline_key to capture the current frame as the named baseline instead of diffing. A failed assertion returns passed=false as data (envelope ok:true), never an error.",
        annotations(read_only_hint = true)
    )]
    async fn assert(
        &self,
        Parameters(p): Parameters<AssertParams>,
    ) -> Json<Envelope<AssertResult>> {
        let mut p = p;
        Json(
            self.guard(async {
                // baseline save/引用预处理（三分支见 resolve_baseline）；save 分支短路不做 diff。
                if let Some(saved) = resolve_baseline(self, &mut p).await? {
                    return Ok(saved);
                }
                self.driver.assert(p).await
            })
            .await,
        )
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use aura_capability::{
        A11yField, AssertMode, DiffMethod, MatchType, TOOL_NAMES,
    };

    /// 构造 image 形态 AssertParams（baseline 面测试用；其余通用字段取占位缺省）。
    fn image_params(
        reference_image_base64: Option<String>,
        baseline_key: Option<&str>,
        save_baseline: Option<bool>,
    ) -> AssertParams {
        AssertParams {
            mode: AssertMode::Image,
            expect: String::new(),
            match_type: MatchType::Contains,
            present: true,
            actual: None,
            field: A11yField::Name,
            query: Default::default(),
            reference_image_base64,
            region: None,
            threshold: 0.05,
            method: DiffMethod::Pixel,
            baseline_key: baseline_key.map(str::to_string),
            save_baseline,
        }
    }

    /// 唯一命名临时 data_dir（测试隔离；尾部清理）。
    fn temp_data_dir(tag: &str) -> PathBuf {
        let ns = std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .map(|d| d.as_nanos())
            .unwrap_or(0);
        std::env::temp_dir().join(format!("aura-baseline-test-{tag}-{ns}-{}", std::process::id()))
    }

    fn tools_with(data_dir: PathBuf) -> AuraTools {
        AuraTools::new(aura_platform::build_driver(
            aura_platform::DriverKind::Desktop,
            None,
            None,
        ))
        .with_data_dir(data_dir)
    }

    /// key 白名单：合法字符集通过；空串 / 路径分隔符 / 空白 / 穿越序列拒绝。
    #[test]
    fn baseline_key_whitelist() {
        for ok in ["k1", "login-page.v2", "A_b-3.webp"] {
            assert!(valid_baseline_key(ok), "{ok} 应合法");
        }
        for bad in ["", "a/b", "a\\b", "../evil", "a b", "键"] {
            assert!(!valid_baseline_key(bad), "{bad:?} 应拒绝");
        }
    }

    /// baseline 存取往返（判据：save→文件在→引用断言走读盘路径）：save_baseline_file 写盘 →
    /// 文件存在（真 .webp 二进制）→ resolve_baseline 分支②读盘回填 reference_image_base64 且与
    /// 原 base64 一致、返回 None（继续走既有 driver.assert）。
    #[tokio::test]
    async fn baseline_save_then_reference_roundtrip() {
        let dir = temp_data_dir("roundtrip");
        let b64 = STANDARD.encode(b"RIFFfakewebpbytes");
        save_baseline_file(&dir, "k-rt", &b64).unwrap();
        assert!(dir.join("baselines").join("k-rt.webp").is_file(), "baseline 文件须落盘");

        let tools = tools_with(dir.clone());
        let mut p = image_params(None, Some("k-rt"), None);
        let short_circuit = resolve_baseline(&tools, &mut p).await.unwrap();
        assert!(short_circuit.is_none(), "引用分支不短路，继续走 driver.assert");
        assert_eq!(p.reference_image_base64.as_deref(), Some(b64.as_str()), "读盘回填参考图");
        let _ = std::fs::remove_dir_all(&dir);
    }

    /// 引用不存在的 key → E_INVALID_ARG 含 "baseline not found"（结构化错误，非 panic）。
    #[tokio::test]
    async fn baseline_missing_key_structured_error() {
        let dir = temp_data_dir("missing");
        let tools = tools_with(dir.clone());
        let mut p = image_params(None, Some("no-such-key"), None);
        let err = resolve_baseline(&tools, &mut p).await.unwrap_err();
        assert_eq!(err.code(), "E_INVALID_ARG");
        assert!(err.to_string().contains("baseline not found"), "err={err}");
        let _ = std::fs::remove_dir_all(&dir);
    }

    /// key 含路径穿越 / 分隔符 → E_INVALID_ARG（读写两路同拦）。
    #[tokio::test]
    async fn baseline_key_path_traversal_rejected() {
        let dir = temp_data_dir("traversal");
        let tools = tools_with(dir.clone());
        for bad in ["../evil", "a/b", "a\\b"] {
            let mut p = image_params(None, Some(bad), None);
            let err = resolve_baseline(&tools, &mut p).await.unwrap_err();
            assert_eq!(err.code(), "E_INVALID_ARG", "key={bad:?}");
            let err = save_baseline_file(&dir, bad, "QQ==").unwrap_err();
            assert_eq!(err.code(), "E_INVALID_ARG", "save key={bad:?}");
        }
        let _ = std::fs::remove_dir_all(&dir);
    }

    /// save_baseline=true 缺 baseline_key → E_INVALID_ARG（校验先于截图，无显示环境亦可测）。
    #[tokio::test]
    async fn save_baseline_without_key_invalid_arg() {
        let dir = temp_data_dir("nokey");
        let tools = tools_with(dir.clone());
        let mut p = image_params(None, None, Some(true));
        let err = resolve_baseline(&tools, &mut p).await.unwrap_err();
        assert_eq!(err.code(), "E_INVALID_ARG");
        assert!(err.to_string().contains("baseline_key"), "err={err}");
        let _ = std::fs::remove_dir_all(&dir);
    }

    /// 直通分支：baseline 两参数皆缺省 → 返回 None 且参数零变（既有调用 wire 零变）。
    #[tokio::test]
    async fn baseline_passthrough_when_unset() {
        let dir = temp_data_dir("passthrough");
        let tools = tools_with(dir.clone());
        let mut p = image_params(Some("QUJDRA==".to_string()), None, None);
        let r = resolve_baseline(&tools, &mut p).await.unwrap();
        assert!(r.is_none());
        assert_eq!(p.reference_image_base64.as_deref(), Some("QUJDRA=="), "显式参考图原样保留");
        let _ = std::fs::remove_dir_all(&dir);
    }

    /// 显式 reference_image_base64 优先于 baseline_key（分支②仅在参考图缺省时读盘）。
    #[tokio::test]
    async fn explicit_reference_wins_over_baseline_key() {
        let dir = temp_data_dir("priority");
        save_baseline_file(&dir, "k-pri", &STANDARD.encode(b"stored")).unwrap();
        let tools = tools_with(dir.clone());
        let mut p = image_params(Some("RVhQTElDSVQ=".to_string()), Some("k-pri"), None);
        let r = resolve_baseline(&tools, &mut p).await.unwrap();
        assert!(r.is_none());
        assert_eq!(
            p.reference_image_base64.as_deref(),
            Some("RVhQTElDSVQ="),
            "显式参考图不被 baseline 覆盖"
        );
        let _ = std::fs::remove_dir_all(&dir);
    }

    /// M11 工具面守卫辅助断言：canonical TOOL_NAMES 已扩容至 21 且含 audio_inject（与
    /// tool_dispatch 集合相等守卫互为对偶，此处从 assert/baseline 侧钉计数）。
    #[test]
    fn tool_names_count_21_and_contains_audio_inject() {
        assert_eq!(TOOL_NAMES.len(), 21, "M11 工具面扩容 20→21");
        assert!(TOOL_NAMES.contains(&"audio_inject"));
    }
}
