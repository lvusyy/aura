// 设备操作台画布工具（纯函数，无 React 依赖）：DispatchTool 信封编解码 + webp 截图直绘 canvas +
// canvas 点击回映射 XGA(display) 坐标。与 m6-e2e screenshot/click 管线同款（tools/m6-e2e.py:280-301）。

// AURA 工具信封：节点 ToolResponse.json_envelope 的 JSON 结构（DispatchTool 原样回传）。
// 与 m6-e2e dispatch_env 同构：{ok, data, error}；error.code 为 E_BUSY/E_UNSUPPORTED 等标准码。
export interface AuraEnvelope {
  ok: boolean | null;
  data?: Record<string, unknown>;
  error?: { code?: string; message?: string } | null;
}

// 截图帧：从 screenshot 信封 data 提取的绘制 + 坐标映射所需字段。
// displayW/displayH 为 XGA 缩放图尺寸（= 图片固有像素 = click 坐标系）；scale = native/display（节点侧 to_native 用）。
export interface ScreenFrame {
  mime: string;
  imageBase64: string;
  displayW: number;
  displayH: number;
  scale: number;
}

// DispatchTool json_args 为 proto bytes（Uint8Array）：JSON.stringify → UTF-8 编码。
// 注意：非 base64——connect 二进制传输把 bytes 直接编码，与 m6-e2e REST base64 路径不同。
export function encodeToolArgs(args: unknown): Uint8Array {
  return new TextEncoder().encode(JSON.stringify(args));
}

// DispatchTool json_envelope 为 proto bytes：UTF-8 解码 → JSON.parse。解析失败回 ok:null 兜底（不抛，供调用方判错）。
export function decodeEnvelope(bytes: Uint8Array): AuraEnvelope {
  try {
    return JSON.parse(new TextDecoder().decode(bytes)) as AuraEnvelope;
  } catch {
    return { ok: null };
  }
}

// 信封错误摘要：error.code 优先（E_BUSY 等），退 message，再退通用文案。
export function envError(env: AuraEnvelope): string {
  return env.error?.code || env.error?.message || "工具调用失败";
}

// 从 screenshot 信封提取 ScreenFrame。缺 image_base64 时回 null（非截图帧/错误帧）。
// meta 键为 snake_case（节点原始信封，未过 proto 转换）：仿 m6-e2e:283-294 取 scale/display_w/display_h。
export function parseScreenshotEnvelope(env: AuraEnvelope): ScreenFrame | null {
  const data = env.data;
  if (!data || typeof data !== "object") {
    return null;
  }
  const imageBase64 = data["image_base64"];
  if (typeof imageBase64 !== "string" || imageBase64.length === 0) {
    return null;
  }
  const meta = (data["meta"] ?? {}) as Record<string, unknown>;
  const mime = typeof data["mime"] === "string" ? (data["mime"] as string) : "image/webp";
  // display_w/h 缺省兜底（仿 m6-e2e:294 dw||720/dh||1280，此处取 XGA 常见值）；scale 缺省 1。
  const displayW = numOr(meta["display_w"], 1024);
  const displayH = numOr(meta["display_h"], 768);
  const scale = numOr(meta["scale"], 1);
  return { mime, imageBase64, displayW, displayH, scale };
}

// 数值兜底：非正/非有限一律取 fallback（尺寸/scale 必为正数）。
function numOr(v: unknown, fallback: number): number {
  const n = typeof v === "number" ? v : typeof v === "string" ? Number(v) : NaN;
  return Number.isFinite(n) && n > 0 ? n : fallback;
}

// 将 webp base64 帧绘制到 canvas：canvas 内部像素缓冲设为 display 尺寸（1:1 XGA），异步等图解码后 drawImage。
// 返回 Promise 在绘制完成/失败后 resolve（失败静默，保留上一帧、下一帧重试）。
export function drawFrameToCanvas(canvas: HTMLCanvasElement, frame: ScreenFrame): Promise<void> {
  return new Promise((resolve) => {
    const img = new Image();
    img.onload = () => {
      // canvas 缓冲 = display 尺寸；图片固有像素即 display（XGA 缩放图），drawImage 铺满 1:1 无重采样。
      canvas.width = frame.displayW;
      canvas.height = frame.displayH;
      const ctx = canvas.getContext("2d");
      if (ctx) {
        ctx.drawImage(img, 0, 0, frame.displayW, frame.displayH);
      }
      resolve();
    };
    img.onerror = () => resolve(); // webp 解码失败静默（浏览器兼容兜底，内网 Chromium 假定 OK）
    img.src = `data:${frame.mime};base64,${frame.imageBase64}`;
  });
}

// 清空 canvas（切节点时抹掉上一节点残帧）。
export function clearCanvas(canvas: HTMLCanvasElement): void {
  const ctx = canvas.getContext("2d");
  if (ctx) {
    ctx.clearRect(0, 0, canvas.width, canvas.height);
  }
}

// canvas 点击 → XGA(display) 坐标：按点击在渲染区的比例映射到 displayW×displayH 网格。
// click 工具入参即 display 坐标系（节点侧 to_native 回映射原生像素执行），与 m6-e2e:298 click{coordinate:[cx,cy]} 一致。
// 注：m6-e2e:292 的 nx/scale 是「原生 a11y bounds → display」；本处输入已是 display 分辨率截图上的点击，
// 故按渲染比例投影到 display 网格即得 display 坐标（与 scale 无关），XGA 管线终点契约一致。
export function canvasClickToDisplay(
  canvas: HTMLCanvasElement,
  clientX: number,
  clientY: number,
  frame: ScreenFrame,
): [number, number] {
  const rect = canvas.getBoundingClientRect();
  // rect 为 CSS 渲染尺寸（可能被容器 max-width 缩放）；比例 × display 尺寸 = display 坐标，独立于渲染缩放。
  const fx = rect.width > 0 ? (clientX - rect.left) / rect.width : 0;
  const fy = rect.height > 0 ? (clientY - rect.top) / rect.height : 0;
  const cx = clamp(Math.round(fx * frame.displayW), 0, frame.displayW - 1);
  const cy = clamp(Math.round(fy * frame.displayH), 0, frame.displayH - 1);
  return [cx, cy];
}

function clamp(v: number, lo: number, hi: number): number {
  return v < lo ? lo : v > hi ? hi : v;
}
