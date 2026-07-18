//! screen 域工具（4 个）+ screen_router。body 委派 driver，TASK-004 填全实现细节。
//!
//! M11 REC-1+REC-2：screenshot/zoom 支持 `output:"json"|"image"`（缺省 json，B 方案 9 约束）与
//! 高保真参数 `quality`/`max_dim`（additive，serde default）。缺省路径 wire 四要素
//! （text/structuredContent/isError/outputSchema）与 rmcp `Json<T>` wrapper 历史产出逐字节一致
//! （P0，见 [`envelope_to_result`] 与 cfg(test) snapshot 测试族）。

use std::sync::atomic::Ordering;

use rmcp::handler::server::wrapper::Parameters;
use rmcp::model::{CallToolResult, ContentBlock};
use rmcp::{tool, tool_router, Json};
use schemars::JsonSchema;
use serde::Deserialize;

use aura_capability::{DisplayInfo, Envelope, Region, ScreenshotOpts, ScreenshotResult};

use super::AuraTools;

/// WebP 质量下限（出域静默 clamp，护栏语义沿 RecordParams fps 先例：软请求不报错）。
const QUALITY_MIN: f32 = 10.0;
/// WebP 质量上限。
const QUALITY_MAX: f32 = 100.0;
/// `output=image` 时 max_dim 上限：同帧 base64 双份（structuredContent + image block）在 wire 上
/// 并存，1600 防 MCP 消息体积失控（Locked-4 组合护栏，main 2026-07-14 认可值）。
const MAX_DIM_CAP_IMAGE: u32 = 1600;
/// 缺省（json）输出时 max_dim 上限：4096 系显式 opt-in 高保真上限（缺省 1280 不受影响，
/// arch spec『截图 WebP ≤1280px』语义由缺省值继续满足）。
const MAX_DIM_CAP_JSON: u32 = 4096;

/// screenshot 入参：可选显示器 id（缺省用当前显示器）+ M11 additive 三字段（全 serde default，
/// 不传时 wire 与历史逐字节一致）。
#[derive(Debug, Deserialize, JsonSchema)]
pub struct ScreenshotParams {
    /// 目标显示器 id；缺省则截当前显示器。
    #[serde(default)]
    pub display: Option<u32>,
    /// 输出形态："json"（缺省，单条 text JSON 信封）| "image"（在 text 之后追加 MCP image
    /// content block，供视觉客户端直接渲染；JSON 信封完整保留）。
    #[serde(default)]
    pub output: Option<String>,
    /// WebP 编码质量（10–100，缺省 80）。出域静默 clamp。
    #[serde(default)]
    pub quality: Option<f32>,
    /// 缩放长边上限 px（缺省 1280/XGA）。上限：output=image 时 1600，否则 4096（静默 clamp）。
    #[serde(default)]
    pub max_dim: Option<u32>,
}

/// zoom 入参：放大区域，P0 契约 [x1,y1,x2,y2]（左上 + 右下两角）+ M11 additive 三字段。
#[derive(Debug, Deserialize, JsonSchema)]
pub struct ZoomParams {
    /// 放大区域 [x1,y1,x2,y2]（左上角 + 右下角，像素）。
    pub region: [i32; 4],
    /// 输出形态："json"（缺省）| "image"（追加 image content block）。语义同 screenshot。
    #[serde(default)]
    pub output: Option<String>,
    /// WebP 编码质量（10–100，缺省 80）。出域静默 clamp。
    #[serde(default)]
    pub quality: Option<f32>,
    /// 缩放长边上限 px；zoom 缺省不降采样（保留原生分辨率观察小字），显式传值时同 clamp 规则。
    #[serde(default)]
    pub max_dim: Option<u32>,
}

/// switch_display 入参：目标显示器 id。
#[derive(Debug, Deserialize, JsonSchema)]
pub struct SwitchDisplayParams {
    /// 目标显示器 id。
    pub display: u32,
}

/// output 参数 → 是否 image 模式（仅 "image" 字面命中；未知值按缺省 json 容错，additive 语义）。
pub(crate) fn is_image_output(output: Option<&str>) -> bool {
    output == Some("image")
}

/// 护栏 clamp（Locked-4）：quality 收敛 [10,100]；max_dim 按输出形态封顶（image 1600 / json 4096）。
/// 只净化**显式传入**的值——`None` 原样透传，缺省语义由 driver 层单源决定（screenshot 1280 /
/// zoom 不降采样），保证不传参的 wire 与历史逐字节一致。触发时静默 clamp 并留日志痕。
/// MCP（screen_tools）与 gRPC（tool_dispatch）两路 handler 共用本函数，参数语义两面一致。
pub(crate) fn clamp_opts(quality: Option<f32>, max_dim: Option<u32>, image_mode: bool) -> ScreenshotOpts {
    let quality = quality.map(|q| {
        let c = q.clamp(QUALITY_MIN, QUALITY_MAX);
        if c != q {
            tracing::info!(requested = q, clamped = c, "screenshot quality out of range, clamped");
        }
        c
    });
    let cap = if image_mode { MAX_DIM_CAP_IMAGE } else { MAX_DIM_CAP_JSON };
    let max_dim = max_dim.map(|d| {
        let c = d.min(cap);
        if c != d {
            tracing::info!(requested = d, clamped = c, cap, "screenshot max_dim over cap, clamped");
        }
        c
    });
    ScreenshotOpts { quality, max_dim }
}

/// screenshot/zoom 的 outputSchema（P0 约束【5】）：与改前 rmcp 宏对 `Json<Envelope<ScreenshotResult>>`
/// 返回类型的自动生成值**同源同函数**（`schema_for_output`，内建 per-type 缓存）。宏对返回
/// `CallToolResult` 的 handler 不做 schema 推导（ANL-010，rmcp-macros-2.1.0/src/tool.rs:26-77 仅识别
/// `Json<T>`/`Result<Json<T>,E>`），故经 `#[tool(output_schema = ...)]` attribute 显式保留。
fn screenshot_output_schema() -> std::sync::Arc<rmcp::model::JsonObject> {
    rmcp::handler::server::tool::schema_for_output::<Envelope<ScreenshotResult>>()
        .expect("Envelope<ScreenshotResult> output schema must stay valid (valid pre-M11)")
}

/// 把 Envelope 组装为 CallToolResult，缺省（json）路径与 rmcp `Json<T>` wrapper 历史产出逐字节
/// 等价（P0 约束【1】）：与 SDK 同一序列化链 `serde_json::to_value` → `CallToolResult::structured`
/// （其 text = `Value::to_string()`，键序、isError=false 恒定语义均由同链保证，勿改为
/// `serde_json::to_string(&env)` 直接序列化——键序不同）。
///
/// image 模式（约束【2】【3】）：仅当信封携带成功数据时在 text 之后**追加** image block
/// （content = [text, image]，text/structuredContent 完整保留含 data.image_base64——双份 base64
/// 系 B 方案既定代价，护栏 clamp 兜底）；ok=false 时无 data 可渲染，形态与缺省路径一致
/// （约束【4】：结构化错误 envelope，isError 不置 true——失败即数据惯例）。
fn envelope_to_result(env: &Envelope<ScreenshotResult>, image_mode: bool) -> CallToolResult {
    // 序列化理论不失败（Envelope 全具名字段）；极端兜底给结构化错误信封值，形态惯例不破。
    let value = serde_json::to_value(env).unwrap_or_else(|e| {
        serde_json::json!({
            "ok": false,
            "error": { "code": "E_INTERNAL", "message": format!("envelope serialization failed: {e}") }
        })
    });
    let mut result = CallToolResult::structured(value);
    if image_mode {
        if let Some(data) = env.data.as_ref() {
            result
                .content
                .push(ContentBlock::image(data.image_base64.clone(), data.mime.clone()));
        }
    }
    result
}

#[tool_router(router = screen_router, vis = "pub(crate)")]
impl AuraTools {
    /// 截取屏幕，缩放至 XGA/1280、WebP 编码，返回 base64 + 元数据。
    ///
    /// MCP 注解：readOnlyHint=true（纯观察，无副作用）。
    /// 副作用（仅进程内状态）：成功后记录本次 `meta.scale` 供 input 域坐标回映射。
    #[tool(
        description = "Capture the screen, scaled to XGA/1280 and WebP-encoded",
        output_schema = screenshot_output_schema(),
        annotations(read_only_hint = true)
    )]
    async fn screenshot(&self, Parameters(p): Parameters<ScreenshotParams>) -> CallToolResult {
        let display = p
            .display
            .unwrap_or_else(|| self.current_display.load(Ordering::Relaxed));
        let image_mode = is_image_output(p.output.as_deref());
        let opts = clamp_opts(p.quality, p.max_dim, image_mode);
        let env = self.guard(self.driver.screenshot(Some(display), opts)).await;
        // 记录本次 display→native 缩放系数，供 input 域坐标回映射；
        // 失败信封无 data 时保持既有 scale（不污染后续坐标映射）。
        if let Some(result) = env.data.as_ref() {
            self.set_scale(result.meta.scale);
        }
        envelope_to_result(&env, image_mode)
    }

    /// 放大屏幕指定区域（[x1,y1,x2,y2]）并截取。
    #[tool(
        description = "Zoom into a screen region [x1,y1,x2,y2] and capture it",
        output_schema = screenshot_output_schema()
    )]
    async fn zoom(&self, Parameters(p): Parameters<ZoomParams>) -> CallToolResult {
        let region = Region {
            x1: p.region[0],
            y1: p.region[1],
            x2: p.region[2],
            y2: p.region[3],
        };
        let image_mode = is_image_output(p.output.as_deref());
        let opts = clamp_opts(p.quality, p.max_dim, image_mode);
        let env = self.guard(self.driver.zoom(region, opts)).await;
        envelope_to_result(&env, image_mode)
    }

    /// 列出所有显示器。
    ///
    /// MCP 注解：readOnlyHint=true（只读枚举，无副作用）。
    #[tool(
        description = "List all displays",
        annotations(read_only_hint = true)
    )]
    async fn list_displays(&self) -> Json<Envelope<Vec<DisplayInfo>>> {
        Json(self.guard(self.driver.list_displays()).await)
    }

    /// 切换当前活动显示器（display 为 list_displays 返回的 0 基序号）。
    #[tool(description = "Switch the current active display")]
    async fn switch_display(
        &self,
        Parameters(p): Parameters<SwitchDisplayParams>,
    ) -> Json<Envelope<DisplayInfo>> {
        let env = self.guard(self.driver.switch_display(p.display)).await;
        // 仅当目标显示器有效（driver 校验通过）时更新当前显示器，
        // 避免无效序号污染后续缺省截图（screenshot 缺省用 current_display）。
        if env.ok {
            self.current_display.store(p.display, Ordering::Relaxed);
        }
        Json(env)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use aura_capability::{ErrObj, ScreenshotMeta};
    use rmcp::handler::server::tool::IntoCallToolResult;

    fn synth_env_ok() -> Envelope<ScreenshotResult> {
        Envelope::ok(ScreenshotResult {
            image_base64: "QUJDRA==".to_string(),
            mime: "image/webp".to_string(),
            meta: ScreenshotMeta {
                native_w: 1600,
                native_h: 900,
                display_w: 1280,
                display_h: 720,
                scale: 1.25,
            },
        })
    }

    fn synth_env_err() -> Envelope<ScreenshotResult> {
        Envelope::error(ErrObj {
            code: "E_CAPTURE_FAILED".to_string(),
            message: "boom".to_string(),
        })
    }

    /// P0 约束【1】【6】snapshot：缺省（json）路径四要素与 rmcp `Json<T>` wrapper 现产出逐字节等价。
    /// 期望值由 SDK 原实现（`Json(env).into_call_tool_result()`）活体生成——非硬编码基线，任何
    /// 序列化链偏差（键序 / isError / structuredContent 形态）都在结构与字节双断言下失配。
    #[test]
    fn mcp_screenshot_default_wire_snapshot_four_elements() {
        for env in [synth_env_ok(), synth_env_err()] {
            let expected = Json(env.clone())
                .into_call_tool_result()
                .expect("rmcp Json wrapper baseline");
            let rebuilt = envelope_to_result(&env, false);
            // 要素一~三（text/structuredContent/isError）：结构等价 + wire 字节等价双断言。
            assert_eq!(rebuilt, expected, "重建须与 rmcp Json wrapper 产出全字段一致");
            assert_eq!(
                serde_json::to_vec(&rebuilt).unwrap(),
                serde_json::to_vec(&expected).unwrap(),
                "wire 序列化字节须逐字节一致"
            );
            // 三要素显式钉死（防 SDK 侧未来漂移让对拍双双漂走）。
            assert_eq!(rebuilt.content.len(), 1, "缺省路径单条 text content");
            let text = rebuilt.content[0].as_text().expect("content[0] 必须是 text");
            assert_eq!(text.text, serde_json::to_value(&env).unwrap().to_string());
            assert_eq!(
                rebuilt.structured_content,
                Some(serde_json::to_value(&env).unwrap())
            );
            assert_eq!(rebuilt.is_error, Some(false), "成功失败信封 isError 恒 false");
        }
        // 要素四 outputSchema（P0 约束【5】，ANL-010 宏坑防线）：screenshot/zoom 广告面必须保留
        // 与改前宏产出一致的 schema（同源函数 schema_for_output::<Envelope<ScreenshotResult>>）。
        let expected_schema =
            rmcp::handler::server::tool::schema_for_output::<Envelope<ScreenshotResult>>()
                .expect("Envelope<ScreenshotResult> schema 恒有效");
        let router = AuraTools::screen_router();
        for name in ["screenshot", "zoom"] {
            let tool = router
                .list_all()
                .into_iter()
                .find(|t| t.name == name)
                .expect("tool advertised");
            assert_eq!(
                tool.output_schema,
                Some(expected_schema.clone()),
                "{name} outputSchema 缺失或漂移（rmcp 宏对 CallToolResult 返回不自动生成——须显式保留）"
            );
        }
    }

    /// 约束【2】【3】：output=image 时 content[0]=text（完整保留）+ content[1]=image（追加不替换），
    /// mime 透传自信封数据。
    #[test]
    fn mcp_screenshot_output_image_content_order() {
        let env = synth_env_ok();
        let r = envelope_to_result(&env, true);
        assert_eq!(r.content.len(), 2, "image 模式 content = [text, image]");
        assert!(r.content[0].as_text().is_some(), "text 第一（约束【3】）");
        let img = r.content[1].as_image().expect("content[1] 必须是 image");
        assert_eq!(img.data, env.data.as_ref().unwrap().image_base64);
        assert_eq!(img.mime_type, "image/webp");
        // 追加不替换（约束【2】）：text/structuredContent/isError 与缺省路径完全一致。
        let base = envelope_to_result(&env, false);
        assert_eq!(r.content[0], base.content[0]);
        assert_eq!(r.structured_content, base.structured_content);
        assert_eq!(r.is_error, base.is_error);
    }

    /// 约束【4】：ok=false 保持结构化错误 envelope，isError 不置 true（失败即数据）；
    /// image 模式下错误信封无 data 可渲染，不追加 image block。
    #[test]
    fn mcp_screenshot_error_envelope_not_iserror() {
        let env = synth_env_err();
        for image_mode in [false, true] {
            let r = envelope_to_result(&env, image_mode);
            assert_eq!(r.content.len(), 1, "错误信封恒单条 text（无 image 可追加）");
            assert_ne!(r.is_error, Some(true), "isError 不置 true");
            let v: serde_json::Value =
                serde_json::from_str(&r.content[0].as_text().unwrap().text).unwrap();
            assert_eq!(v["ok"], false, "结构化错误 envelope 完整保留");
            assert_eq!(v["error"]["code"], "E_CAPTURE_FAILED");
        }
    }

    /// 护栏 clamp（Locked-4）：quality 出域收 [10,100]；max_dim 按输出形态封顶
    /// （image 1600 / json 4096）；未显式传参不注入值（缺省语义由 driver 层单源决定）。
    #[test]
    fn quality_max_dim_guardrail_clamp() {
        // 出域值收敛。
        let o = clamp_opts(Some(5.0), Some(4000), true);
        assert_eq!(o.quality, Some(10.0));
        assert_eq!(o.max_dim, Some(1600), "image 档上限 1600");
        let o = clamp_opts(Some(150.0), Some(8192), false);
        assert_eq!(o.quality, Some(100.0));
        assert_eq!(o.max_dim, Some(4096), "json 档上限 4096");
        // 域内值原样透传。
        let o = clamp_opts(Some(55.5), Some(1024), true);
        assert_eq!(o.quality, Some(55.5));
        assert_eq!(o.max_dim, Some(1024));
        // 缺省不注入（None 透传——screenshot 走 1280、zoom 走不降采样，均由 driver 层决定）。
        let o = clamp_opts(None, None, true);
        assert_eq!(o.quality, None);
        assert_eq!(o.max_dim, None);
        // output 判定：仅 "image" 字面命中，未知值按缺省 json 容错。
        assert!(is_image_output(Some("image")));
        assert!(!is_image_output(Some("json")));
        assert!(!is_image_output(Some("IMAGE")));
        assert!(!is_image_output(None));
    }
}
