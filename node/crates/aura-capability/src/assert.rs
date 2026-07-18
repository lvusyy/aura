//! assert 原语：对界面 / 文本状态做确定性断言。**失败即数据（Locked-7）**。
//!
//! 三形态：
//!   - `text`：纯逻辑，对入参 `actual` 文本与 `expect` 按 contains/equals 比较（平台无关，无后端）。
//!   - `a11y`：复用 [`A11yDriver::get_a11y_tree`]（TASK-005）取树，判定是否存在字段匹配 `expect` 的节点。
//!   - `image`：复用 [`ScreenDriver::screenshot`]（M1）取当前帧，对指定 region 与调用方参考图（base64
//!     WebP）做像素 / 结构 diff（TASK-011，stretch）。**无 baseline 落盘**——参考图仅来自入参，
//!     diff 在解码后的 RGB 裸字节层进行；`diff_score <= threshold` 判 `passed`。
//!
//! 语义关键（Locked-7）：断言不成立（passed=false）是**正常数据**，经 `Ok(AssertResult)` 返回，
//! 由上层统一信封走 `ok:true` 分支；只有参数非法（如 text 形态缺 `actual`、image 形态缺参考图 /
//! 解码失败 / 区域越界）/ 取树失败等**真实错误**才落 [`CapError`]（复用既有 E_ 码，**绝不新增
//! E_ASSERT**）。agent 据 `passed` 自行分支，闭合『观察 → 动作 → 断言』确定性环。
//!
//! 平台无关：逻辑全部在本能力层，平台层仅需空 impl 并入 DeviceDriver bound（无新后端）。

use async_trait::async_trait;
use base64::prelude::{Engine as _, BASE64_STANDARD};

use crate::a11y::A11yDriver;
use crate::screen::ScreenDriver;
use crate::types::{
    A11yField, A11yNode, AssertMode, AssertParams, AssertResult, CapError, DiffMethod, MatchType,
};

/// 断言能力：text 纯文本比较 / a11y 无障碍树谓词 / image 区域视觉 diff。默认方法即全部实现
/// （平台无关），平台驱动空 impl（`impl AssertDriver for PlatformDriver {}`）即获得本能力。
/// image 形态复用 [`ScreenDriver`] 截图管线，故并入其为 supertrait bound（平台驱动本已实现）。
#[async_trait]
pub trait AssertDriver: A11yDriver + ScreenDriver {
    /// 对目标状态求断言。断言成立性以 [`AssertResult::passed`] 数据返回（成功/失败均 `Ok`）；
    /// 仅参数非法 / 取树 / 截图失败落 [`CapError`]（既有 E_ 码，不新增 E_ASSERT）。
    async fn assert(&self, params: AssertParams) -> Result<AssertResult, CapError> {
        eval_assert(self, params).await
    }
}

/// 断言求值核（平台无关）。抽为泛型自由函数：trait 默认方法薄委派于此。
/// image 形态仅一行截图后委派纯函数 [`eval_image_assert`]，diff 逻辑无 driver 依赖便于单测。
pub async fn eval_assert<D: A11yDriver + ScreenDriver + ?Sized>(
    driver: &D,
    params: AssertParams,
) -> Result<AssertResult, CapError> {
    match params.mode {
        AssertMode::Text => assert_text(&params),
        AssertMode::A11y => {
            let tree = driver.get_a11y_tree(params.query.clone()).await?;
            Ok(assert_a11y(&params, &tree.nodes))
        }
        AssertMode::Image => {
            // 复用 M1 既有截图管线（WebP / 缩放）取当前帧；region 与截图同 display 像素空间。
            let shot = driver.screenshot(None, crate::screen::ScreenshotOpts::default()).await?;
            eval_image_assert(&shot.image_base64, &params)
        }
    }
}

/// text 形态：对入参 `actual` 与 `expect` 按 `match_type` 比较，`present` 决定期望成立性。
/// `actual` 缺省 → E_INVALID_ARG（text 形态必须提供被断言文本，属参数错误而非断言失败）。
fn assert_text(params: &AssertParams) -> Result<AssertResult, CapError> {
    let actual = params
        .actual
        .as_deref()
        .ok_or_else(|| CapError::InvalidArg("assert text mode requires 'actual'".to_string()))?;
    let hit = text_matches(actual, &params.expect, params.match_type);
    let passed = hit == params.present;
    Ok(AssertResult {
        passed,
        detail: format!(
            "text assert: {}expect {:?} (match={:?}); hit={hit}, passed={passed}",
            if params.present { "" } else { "NOT " },
            params.expect,
            params.match_type,
        ),
        matched: hit.then(|| actual.to_string()),
        diff_score: None,
    })
}

/// a11y 形态：在树中 DFS 查找字段匹配 `expect` 的节点；`present` 决定期望成立性。
fn assert_a11y(params: &AssertParams, nodes: &[A11yNode]) -> AssertResult {
    let matched = find_matching_node(nodes, &params.expect, params.match_type, params.field);
    let found = matched.is_some();
    let passed = found == params.present;
    AssertResult {
        passed,
        detail: format!(
            "a11y assert: {}expect {:?} on field {:?} (match={:?}); found={found}, passed={passed}",
            if params.present { "" } else { "NOT " },
            params.expect,
            params.field,
            params.match_type,
        ),
        matched,
        diff_score: None,
    }
}

/// 文本匹配（Unicode 大小写无关）：contains 子串 / equals 全等（忽略首尾空白）。
/// CJK 无大小写，`to_lowercase` 为恒等，故对 Windows 本地化中文 role/name 亦即精确匹配。
fn text_matches(haystack: &str, needle: &str, match_type: MatchType) -> bool {
    match match_type {
        MatchType::Contains => haystack.to_lowercase().contains(&needle.to_lowercase()),
        MatchType::Equals => haystack.trim().to_lowercase() == needle.trim().to_lowercase(),
    }
}

/// DFS 查首个字段匹配节点，命中返回其摘要（role/name[/value]）。
fn find_matching_node(
    nodes: &[A11yNode],
    expect: &str,
    match_type: MatchType,
    field: A11yField,
) -> Option<String> {
    for node in nodes {
        if node_field_matches(node, expect, match_type, field) {
            return Some(node_summary(node));
        }
        if let Some(s) = find_matching_node(&node.children, expect, match_type, field) {
            return Some(s);
        }
    }
    None
}

/// 单节点按指定字段判匹配（Any = name/role/value 任一命中，规避 Windows role 本地化）。
fn node_field_matches(node: &A11yNode, expect: &str, mt: MatchType, field: A11yField) -> bool {
    let matches = |s: &str| text_matches(s, expect, mt);
    match field {
        A11yField::Name => matches(&node.name),
        A11yField::Role => matches(&node.role),
        A11yField::Value => node.value.as_deref().is_some_and(matches),
        A11yField::Any => {
            matches(&node.name) || matches(&node.role) || node.value.as_deref().is_some_and(matches)
        }
    }
}

/// 命中节点摘要串，供 agent 分支与留证。
fn node_summary(node: &A11yNode) -> String {
    match &node.value {
        Some(v) => format!("role={:?} name={:?} value={:?}", node.role, node.name, v),
        None => format!("role={:?} name={:?}", node.role, node.name),
    }
}

// ===== image 形态：区域视觉 diff（无 baseline，参考图仅来自入参 reference_image_base64）=====

/// 解码后的 RGB 图（若源含 alpha 则剥离归一为 RGB）。裁剪与 diff 均在此裸字节层进行。
struct RgbImage {
    w: usize,
    h: usize,
    /// 长度 = w*h*3（逐像素 R,G,B）。
    data: Vec<u8>,
}

/// image 形态求值核（**纯函数，无 driver 依赖**，便于单测同图 / 异图 / 区域裁剪三路径）：
/// 解码当前帧与参考图（base64 WebP）→ 裁剪同一 region → 按 method 计 diff_score → passed=score<=threshold。
/// **失败即数据（Locked-7）**：passed 真假均 `Ok`；仅缺参考图 / 解码失败 / 区域越界落既有 E_INVALID_ARG。
pub fn eval_image_assert(
    current_image_base64: &str,
    params: &AssertParams,
) -> Result<AssertResult, CapError> {
    let reference_b64 = params.reference_image_base64.as_deref().ok_or_else(|| {
        CapError::InvalidArg("assert image mode requires 'reference_image_base64'".to_string())
    })?;
    let current = decode_webp_rgb(current_image_base64)?;
    let reference = decode_webp_rgb(reference_b64)?;
    // 缺省 region = 当前帧全图；否则用入参 [x1,y1,x2,y2]（同 zoom 契约）。
    let region = params
        .region
        .unwrap_or([0, 0, current.w as i32, current.h as i32]);
    let cur_crop = crop_rgb(&current, region)?;
    let ref_crop = crop_rgb(&reference, region)?;
    // 两裁剪区域尺寸必一致（同 region）；防御性校验（参考图小于 region 时 crop 已先报越界）。
    if cur_crop.w != ref_crop.w || cur_crop.h != ref_crop.h {
        return Err(CapError::InvalidArg(format!(
            "cropped size mismatch: current {}x{} vs reference {}x{}",
            cur_crop.w, cur_crop.h, ref_crop.w, ref_crop.h
        )));
    }
    let score = match params.method {
        DiffMethod::Pixel => pixel_diff_score(&cur_crop, &ref_crop),
        DiffMethod::Ssim => ssim_distance(&cur_crop, &ref_crop),
    };
    let passed = score <= params.threshold;
    Ok(AssertResult {
        passed,
        detail: format!(
            "image assert: method={:?} region=[{},{},{},{}] diff_score={:.6} threshold={:.6}; passed={passed}",
            params.method, region[0], region[1], region[2], region[3], score, params.threshold,
        ),
        matched: None,
        diff_score: Some(score),
    })
}

/// base64 WebP → RGB 裸字节（源含 alpha 则剥离）。解码失败 / 尺寸异常落 E_INVALID_ARG。
/// 复用 aura-platform 截图管线同款 `webp` crate（libwebp），与 M1 编码侧对称，无新重依赖。
fn decode_webp_rgb(image_base64: &str) -> Result<RgbImage, CapError> {
    let bytes = BASE64_STANDARD
        .decode(image_base64.trim())
        .map_err(|e| CapError::InvalidArg(format!("invalid base64 image: {e}")))?;
    let decoded = webp::Decoder::new(&bytes)
        .decode()
        .ok_or_else(|| CapError::InvalidArg("failed to decode WebP image".to_string()))?;
    let w = decoded.width() as usize;
    let h = decoded.height() as usize;
    let px_count = w.checked_mul(h).unwrap_or(0);
    if px_count == 0 {
        return Err(CapError::InvalidArg("decoded image has zero size".to_string()));
    }
    let raw: &[u8] = &decoded;
    // 通道数由字节长度反推（不依赖 layout 判定）；RGBA 剥 alpha 归一为 RGB。
    let channels = raw.len() / px_count;
    let data = match channels {
        3 => raw.to_vec(),
        4 => raw.chunks_exact(4).flat_map(|p| [p[0], p[1], p[2]]).collect(),
        other => {
            return Err(CapError::InvalidArg(format!(
                "unexpected channel count {other} in decoded image"
            )))
        }
    };
    Ok(RgbImage { w, h, data })
}

/// 裁剪 region=[x1,y1,x2,y2]（含左上、不含右下）。非法 / 越界落 E_INVALID_ARG。
fn crop_rgb(img: &RgbImage, region: [i32; 4]) -> Result<RgbImage, CapError> {
    let [x1, y1, x2, y2] = region;
    if x1 < 0 || y1 < 0 || x2 <= x1 || y2 <= y1 {
        return Err(CapError::InvalidArg(format!(
            "invalid region {region:?}: require 0<=x1<x2 and 0<=y1<y2"
        )));
    }
    let (x1, y1, x2, y2) = (x1 as usize, y1 as usize, x2 as usize, y2 as usize);
    if x2 > img.w || y2 > img.h {
        return Err(CapError::InvalidArg(format!(
            "region {region:?} exceeds image {}x{}",
            img.w, img.h
        )));
    }
    let (cw, ch) = (x2 - x1, y2 - y1);
    let mut data = Vec::with_capacity(cw * ch * 3);
    for y in y1..y2 {
        let start = (y * img.w + x1) * 3;
        data.extend_from_slice(&img.data[start..start + cw * 3]);
    }
    Ok(RgbImage { w: cw, h: ch, data })
}

/// 像素差率：各通道 |a-b| 之和 / (字节数 * 255)，归一 [0,1]（0=完全一致）。同尺寸由调用方保证。
fn pixel_diff_score(a: &RgbImage, b: &RgbImage) -> f64 {
    if a.data.is_empty() {
        return 0.0;
    }
    let sum: u64 = a
        .data
        .iter()
        .zip(&b.data)
        .map(|(&x, &y)| u64::from((x as i16 - y as i16).unsigned_abs()))
        .sum();
    sum as f64 / (a.data.len() as f64 * 255.0)
}

/// 结构相似性距离：1 - 全局 SSIM（luma 通道，Wang et al. 2004 常数），clamp[0,1]（0=结构一致）。
/// 全局单窗口 SSIM（非滑窗）：对区域一致性判定足够，避免引入卷积重依赖。同尺寸由调用方保证。
fn ssim_distance(a: &RgbImage, b: &RgbImage) -> f64 {
    let la = luma(a);
    let lb = luma(b);
    let n = la.len() as f64;
    if n == 0.0 {
        return 0.0;
    }
    let (mut ma, mut mb) = (0.0f64, 0.0f64);
    for (&x, &y) in la.iter().zip(&lb) {
        ma += x;
        mb += y;
    }
    ma /= n;
    mb /= n;
    let (mut va, mut vb, mut cov) = (0.0f64, 0.0f64, 0.0f64);
    for (&x, &y) in la.iter().zip(&lb) {
        va += (x - ma) * (x - ma);
        vb += (y - mb) * (y - mb);
        cov += (x - ma) * (y - mb);
    }
    va /= n;
    vb /= n;
    cov /= n;
    let c1 = (0.01 * 255.0f64).powi(2);
    let c2 = (0.03 * 255.0f64).powi(2);
    let ssim =
        ((2.0 * ma * mb + c1) * (2.0 * cov + c2)) / ((ma * ma + mb * mb + c1) * (va + vb + c2));
    (1.0 - ssim).clamp(0.0, 1.0)
}

/// RGB → luma（Rec.601：0.299R+0.587G+0.114B）逐像素灰度序列。
fn luma(img: &RgbImage) -> Vec<f64> {
    img.data
        .chunks_exact(3)
        .map(|p| 0.299 * p[0] as f64 + 0.587 * p[1] as f64 + 0.114 * p[2] as f64)
        .collect()
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::envelope::Envelope;

    fn params_text(
        actual: Option<&str>,
        expect: &str,
        present: bool,
        mt: MatchType,
    ) -> AssertParams {
        AssertParams {
            mode: AssertMode::Text,
            expect: expect.to_string(),
            match_type: mt,
            present,
            actual: actual.map(|s| s.to_string()),
            field: A11yField::Name,
            query: Default::default(),
            reference_image_base64: None,
            region: None,
            threshold: 0.05,
            method: DiffMethod::Pixel,
            baseline_key: None,
            save_baseline: None,
        }
    }

    fn params_a11y(expect: &str, present: bool, field: A11yField, mt: MatchType) -> AssertParams {
        AssertParams {
            mode: AssertMode::A11y,
            expect: expect.to_string(),
            match_type: mt,
            present,
            actual: None,
            field,
            query: Default::default(),
            reference_image_base64: None,
            region: None,
            threshold: 0.05,
            method: DiffMethod::Pixel,
            baseline_key: None,
            save_baseline: None,
        }
    }

    /// 构造 image 断言入参（其余通用字段取占位缺省，image 形态忽略之）。
    fn params_image(
        reference_image_base64: Option<String>,
        region: Option<[i32; 4]>,
        threshold: f64,
        method: DiffMethod,
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
            region,
            threshold,
            method,
            baseline_key: None,
            save_baseline: None,
        }
    }

    /// 合成 w×h 的 WebP(lossless) base64，像素由 `fill(x,y)->[r,g,b]` 决定（alpha=255）。
    /// lossless 保证解码逐像素还原，diff 数学可确定性断言（不受编码量化噪声干扰）。
    fn webp_b64(w: u32, h: u32, fill: impl Fn(u32, u32) -> [u8; 3]) -> String {
        let mut rgba = Vec::with_capacity((w * h * 4) as usize);
        for y in 0..h {
            for x in 0..w {
                let [r, g, b] = fill(x, y);
                rgba.extend_from_slice(&[r, g, b, 255]);
            }
        }
        let mem = webp::Encoder::from_rgba(&rgba, w, h).encode_lossless();
        BASE64_STANDARD.encode(&*mem)
    }

    fn leaf(role: &str, name: &str) -> A11yNode {
        A11yNode {
            role: role.into(),
            name: name.into(),
            bounds: None,
            value: None,
            children: vec![],
        }
    }

    /// text 匹配：contains 大小写无关子串 / equals 忽略首尾空白全等。
    #[test]
    fn text_matches_contains_and_equals() {
        assert!(text_matches("Hello World", "world", MatchType::Contains));
        assert!(!text_matches("Hello World", "xyz", MatchType::Contains));
        assert!(text_matches("  OK  ", "ok", MatchType::Equals));
        assert!(!text_matches("OK button", "ok", MatchType::Equals));
    }

    /// 判据5（text）：passed=true 与 passed=false 两路径均 Ok 且经 Envelope::ok → ok:true。
    #[test]
    fn text_assert_both_paths_ok_envelope() {
        // 命中 → passed=true
        let pass = assert_text(&params_text(
            Some("login succeeded"),
            "succeeded",
            true,
            MatchType::Contains,
        ))
        .unwrap();
        assert!(pass.passed);
        let v = serde_json::to_value(Envelope::ok(pass)).unwrap();
        assert_eq!(v["ok"], true);
        assert_eq!(v["data"]["passed"], true);

        // 未命中 → passed=false（仍为 Ok / ok:true，失败即数据）
        let fail = assert_text(&params_text(
            Some("login succeeded"),
            "denied",
            true,
            MatchType::Contains,
        ))
        .unwrap();
        assert!(!fail.passed);
        let v = serde_json::to_value(Envelope::ok(fail)).unwrap();
        assert_eq!(v["ok"], true);
        assert_eq!(v["data"]["passed"], false);
    }

    /// text 反向断言：present=false 时未命中即成立。
    #[test]
    fn text_assert_negative_present() {
        let r = assert_text(&params_text(
            Some("all clear"),
            "error",
            false,
            MatchType::Contains,
        ))
        .unwrap();
        assert!(r.passed);
        assert!(r.matched.is_none());
    }

    /// text 缺 actual → E_INVALID_ARG（参数错误落既有 E_ 码，非新增 E_ASSERT，非断言失败）。
    #[test]
    fn text_assert_missing_actual_is_invalid_arg() {
        let err = assert_text(&params_text(None, "x", true, MatchType::Contains)).unwrap_err();
        assert_eq!(err.code(), "E_INVALID_ARG");
    }

    /// 判据5（a11y）：存在元素 passed=true、不存在元素 passed=false，两路径均经 Envelope::ok → ok:true。
    #[test]
    fn a11y_assert_both_paths_ok_envelope() {
        let tree = vec![A11yNode {
            role: "frame".into(),
            name: "aura-e2e.txt - gedit".into(),
            bounds: None,
            value: None,
            children: vec![leaf("push button", "Save")],
        }];
        // 存在（子节点 name="Save"）→ passed=true
        let hit = assert_a11y(
            &params_a11y("Save", true, A11yField::Name, MatchType::Contains),
            &tree,
        );
        assert!(hit.passed);
        assert!(hit.matched.is_some());
        let v = serde_json::to_value(Envelope::ok(hit)).unwrap();
        assert_eq!(v["ok"], true);
        assert_eq!(v["data"]["passed"], true);

        // 不存在 → passed=false（ok:true，失败即数据）
        let miss = assert_a11y(
            &params_a11y("NoSuchElementZZZ", true, A11yField::Name, MatchType::Contains),
            &tree,
        );
        assert!(!miss.passed);
        assert!(miss.matched.is_none());
        let v = serde_json::to_value(Envelope::ok(miss)).unwrap();
        assert_eq!(v["ok"], true);
        assert_eq!(v["data"]["passed"], false);
    }

    /// a11y field=Any 命中 role（应对 Windows role 本地化：name 不含但 role 含即命中）。
    #[test]
    fn a11y_assert_field_any_matches_localized_role() {
        let tree = vec![leaf("按钮", "开始")]; // Windows 本地化 role="按钮"(Button)
        let by_name = assert_a11y(
            &params_a11y("按钮", true, A11yField::Name, MatchType::Contains),
            &tree,
        );
        assert!(!by_name.passed); // name="开始" 不含 "按钮"
        let by_any = assert_a11y(
            &params_a11y("按钮", true, A11yField::Any, MatchType::Contains),
            &tree,
        );
        assert!(by_any.passed); // Any 经 role="按钮" 命中
    }

    // ===== image 形态：区域视觉 diff（TASK-011）=====

    /// 判据4（image 同区域相同图）：同图 diff_score≈0 → passed=true，且经 Envelope::ok → ok:true。
    #[test]
    fn image_assert_identical_passes() {
        let img = webp_b64(16, 16, |_, _| [10, 200, 90]);
        let r = eval_image_assert(
            &img,
            &params_image(Some(img.clone()), None, 0.01, DiffMethod::Pixel),
        )
        .unwrap();
        assert!(r.passed, "identical image must pass, score={:?}", r.diff_score);
        assert!(r.diff_score.unwrap() <= 1e-6);
        let v = serde_json::to_value(Envelope::ok(r)).unwrap();
        assert_eq!(v["ok"], true);
        assert_eq!(v["data"]["passed"], true);
    }

    /// 判据4（image 差异超阈）：显著不同图 diff_score 高 → passed=false，仍 ok:true（失败即数据，Locked-7）。
    #[test]
    fn image_assert_different_fails() {
        let current = webp_b64(16, 16, |_, _| [255, 0, 0]); // 纯红
        let reference = webp_b64(16, 16, |_, _| [0, 0, 255]); // 纯蓝
        let r = eval_image_assert(
            &current,
            &params_image(Some(reference), None, 0.05, DiffMethod::Pixel),
        )
        .unwrap();
        assert!(!r.passed, "different image must fail, score={:?}", r.diff_score);
        assert!(r.diff_score.unwrap() > 0.05);
        let v = serde_json::to_value(Envelope::ok(r)).unwrap();
        assert_eq!(v["ok"], true, "fail path still ok:true (failure is data)");
        assert_eq!(v["data"]["passed"], false);
    }

    /// 判据4（区域裁剪正确）：参考图左半同色、右半异色——比左半(相同区)passed、比右半(相异区)failed，
    /// 证明 region 精确定位裁剪目标像素（而非误比全图）。
    #[test]
    fn image_assert_region_crop_targets_correct_pixels() {
        // current 全白；reference 左半(x<4)白、右半黑（8 宽 4 高）。
        let current = webp_b64(8, 4, |_, _| [255, 255, 255]);
        let reference = webp_b64(8, 4, |x, _| if x < 4 { [255, 255, 255] } else { [0, 0, 0] });
        // 左半 [0,0,4,4]：current 白 vs reference 白 → 一致 → passed。
        let left = eval_image_assert(
            &current,
            &params_image(Some(reference.clone()), Some([0, 0, 4, 4]), 0.01, DiffMethod::Pixel),
        )
        .unwrap();
        assert!(left.passed, "left region identical must pass, score={:?}", left.diff_score);
        // 右半 [4,0,8,4]：current 白 vs reference 黑 → 相异（差率≈1）→ failed。
        let right = eval_image_assert(
            &current,
            &params_image(Some(reference), Some([4, 0, 8, 4]), 0.01, DiffMethod::Pixel),
        )
        .unwrap();
        assert!(!right.passed, "right region differs must fail, score={:?}", right.diff_score);
        assert!(right.diff_score.unwrap() > 0.9);
    }

    /// image ssim 形态：同图 SSIM 距离≈0 → passed（验证 method=ssim 路径）。
    #[test]
    fn image_assert_ssim_identical_passes() {
        let img = webp_b64(16, 16, |x, y| [(x * 8) as u8, (y * 8) as u8, 128]);
        let r = eval_image_assert(
            &img,
            &params_image(Some(img.clone()), None, 0.01, DiffMethod::Ssim),
        )
        .unwrap();
        assert!(r.passed, "identical ssim must pass, score={:?}", r.diff_score);
    }

    /// image 缺参考图 → E_INVALID_ARG（参数错误落既有码，非断言失败，非 E_ASSERT）。
    #[test]
    fn image_assert_missing_reference_is_invalid_arg() {
        let current = webp_b64(4, 4, |_, _| [1, 2, 3]);
        let err = eval_image_assert(&current, &params_image(None, None, 0.05, DiffMethod::Pixel))
            .unwrap_err();
        assert_eq!(err.code(), "E_INVALID_ARG");
    }

    /// image region 越界 → E_INVALID_ARG。
    #[test]
    fn image_assert_region_out_of_bounds_is_invalid_arg() {
        let img = webp_b64(4, 4, |_, _| [1, 2, 3]);
        let err = eval_image_assert(
            &img,
            &params_image(Some(img.clone()), Some([0, 0, 99, 99]), 0.05, DiffMethod::Pixel),
        )
        .unwrap_err();
        assert_eq!(err.code(), "E_INVALID_ARG");
    }
}
