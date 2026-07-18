# AURA M9 — OmniParser detector 独立服务（D3 外置服务 / C1 bearer 主防线 / C2 权重不进镜像）。
#
# 自研最小 FastAPI 服务，不使用 OmniParser 仓库的 omniparserserver/omnitool 代码
# （规避 CVE-2025-55322 的未鉴权执行面；权重 checkpoints 与服务代码解耦）。
# 权重从挂载路径 $AURA_WEIGHTS_DIR（默认 /weights）离线加载（local_files_only），
# 目录契约见同目录 README.md——icon_caption_florence 必须是自足 HF 快照
# （含 processor/tokenizer 配置；auto_map 的外部仓引用在离线环境不可达）。
#
# device 自适应（Q2）：CUDA 可用 → GPU fp16（~1s/帧）；否则 CPU 强制 float32
# （fp16-on-CPU 算子支持极差会挂死 >1h，research-omniparser §3.2 红线）。
# 推理为单 worker 串行队列（ThreadPoolExecutor(1)）：单帧已饱和算力，并发无益。
import asyncio
import io
import logging
import os
import secrets
import sys
from concurrent.futures import ThreadPoolExecutor
from contextlib import asynccontextmanager
from pathlib import Path

log = logging.getLogger("aura.detector")
logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(name)s %(message)s")

TOKEN = os.environ.get("AURA_DETECTOR_TOKEN", "")
WEIGHTS_DIR = Path(os.environ.get("AURA_WEIGHTS_DIR", "/weights"))
BIND = os.environ.get("AURA_DETECTOR_BIND", "0.0.0.0")  # 容器内需非回环；bearer 为主防线（C1）
PORT = int(os.environ.get("AURA_DETECTOR_PORT", "8080"))
BOX_THRESHOLD = float(os.environ.get("AURA_BOX_THRESHOLD", "0.05"))  # OmniParser 官方默认
MAX_NEW_TOKENS = 20  # 官方 caption 生成长度

if not TOKEN:
    # 无 token 即拒绝启动：绝不允许无鉴权裸奔（CVE-2025-55322 教训，C1）
    log.error("AURA_DETECTOR_TOKEN is not set; refusing to start an unauthenticated service")
    sys.exit(2)

# ---- 模型状态（lifespan 启动时加载；加载完成前 ASGI 不接请求，/healthz 通即就绪） ----
class _Models:
    yolo = None
    florence = None
    processor = None
    device = "cpu"
    dtype = None
    florence_via = ""
    caption_batch = 1

M = _Models()
EXECUTOR = ThreadPoolExecutor(max_workers=1)  # 串行推理队列


def _load_models():
    import torch
    from ultralytics import YOLO
    from transformers import AutoModelForCausalLM, AutoProcessor

    M.device = "cuda" if torch.cuda.is_available() else "cpu"
    # fp16 仅限 GPU；CPU 强制 float32（fp16-on-CPU 挂死红线）
    M.dtype = torch.float16 if M.device == "cuda" else torch.float32
    M.caption_batch = 32 if M.device == "cuda" else 8

    det = WEIGHTS_DIR / "icon_detect" / "model.pt"
    cap = WEIGHTS_DIR / "icon_caption_florence"
    if not det.is_file() or not (cap / "model.safetensors").is_file():
        log.error("weights not found under %s (need icon_detect/model.pt + icon_caption_florence/); see README.md", WEIGHTS_DIR)
        sys.exit(3)

    log.info("loading icon_detect YOLO from %s", det)
    M.yolo = YOLO(str(det))

    log.info("loading icon_caption_florence from %s (device=%s dtype=%s)", cap, M.device, M.dtype)
    common = dict(local_files_only=True, torch_dtype=M.dtype)
    try:
        # 主路径：transformers 原生 florence2（≥4.55），零远程代码
        M.florence = AutoModelForCausalLM.from_pretrained(cap, trust_remote_code=False, attn_implementation="sdpa", **common)
        M.processor = AutoProcessor.from_pretrained(cap, trust_remote_code=False, local_files_only=True)
        M.florence_via = "transformers-native"
    except Exception as e:
        # 兜底：目录内本地 modeling/processing .py（仍离线，不触网）
        log.warning("native florence2 load failed (%s); falling back to in-dir remote code", e)
        M.florence = AutoModelForCausalLM.from_pretrained(cap, trust_remote_code=True, **common)
        M.processor = AutoProcessor.from_pretrained(cap, trust_remote_code=True, local_files_only=True)
        M.florence_via = "local-remote-code"
    M.florence = M.florence.to(M.device)
    if M.device == "cpu":
        M.florence = M.florence.float()  # 双保险：强制 float32
    M.florence.eval()
    log.info("models ready (florence via %s)", M.florence_via)


def _caption(crops):
    """对裁剪块批量生成功能语义 caption（官方 <CAPTION> prompt）。"""
    import torch

    texts = []
    for i in range(0, len(crops), M.caption_batch):
        batch = crops[i : i + M.caption_batch]
        inputs = M.processor(text=["<CAPTION>"] * len(batch), images=batch, return_tensors="pt", padding=True)
        inputs = {k: v.to(M.device) for k, v in inputs.items()}
        if M.device == "cuda":
            inputs["pixel_values"] = inputs["pixel_values"].to(M.dtype)
        with torch.inference_mode():
            gen = M.florence.generate(
                input_ids=inputs["input_ids"],
                pixel_values=inputs["pixel_values"],
                max_new_tokens=MAX_NEW_TOKENS,
                num_beams=1,
                do_sample=False,
            )
        texts += [t.strip() for t in M.processor.batch_decode(gen, skip_special_tokens=True)]
    return texts


def _infer(img):
    """同步推理：YOLO 检测 + Florence caption。在单线程 EXECUTOR 中运行。"""
    w, h = img.size
    res = M.yolo.predict(img, conf=BOX_THRESHOLD, device=M.device, verbose=False)[0]
    names = res.names or {}
    # ultralytics xyxy 已是输入图像像素坐标（自动映射回原图），无需反归一化
    boxes = res.boxes.xyxy.cpu().numpy().tolist()
    confs = res.boxes.conf.cpu().numpy().tolist()
    clses = res.boxes.cls.int().cpu().numpy().tolist()

    kept, crops = [], []
    for (x1, y1, x2, y2), conf, cls in zip(boxes, confs, clses):
        x1, y1 = max(0.0, x1), max(0.0, y1)
        x2, y2 = min(float(w), x2), min(float(h), y2)
        if x2 - x1 < 3 or y2 - y1 < 3:  # 过滤退化框（Florence 空裁剪会炸）
            continue
        kept.append((x1, y1, x2, y2, conf, cls))
        crops.append(img.crop((int(x1), int(y1), int(x2), int(y2))))

    captions = _caption(crops) if crops else []
    elements = [
        {
            "bbox": [round(x1, 1), round(y1, 1), round(x2 - x1, 1), round(y2 - y1, 1)],
            "type": str(names.get(cls, "icon")),
            "caption": cap,
            "confidence": round(float(conf), 4),
        }
        for (x1, y1, x2, y2, conf, cls), cap in zip(kept, captions)
    ]
    return {"elements": elements, "image_w": w, "image_h": h}


# ---- HTTP 层 ----
from fastapi import FastAPI, Request  # noqa: E402
from fastapi.responses import JSONResponse  # noqa: E402


@asynccontextmanager
async def lifespan(_app):
    _load_models()
    yield


app = FastAPI(lifespan=lifespan)


@app.get("/healthz")
async def healthz():
    # lifespan 完成前 ASGI 不接请求：能应答即模型+权重加载完成
    return {"status": "ok", "device": M.device, "dtype": str(M.dtype), "florence_via": M.florence_via}


@app.post("/detect")
async def detect(request: Request):
    auth = request.headers.get("authorization", "")
    # 常量时间比较，防时序侧信道（C1）
    if not (auth.startswith("Bearer ") and secrets.compare_digest(auth[7:], TOKEN)):
        return JSONResponse({"detail": "unauthorized"}, status_code=401)

    ctype = request.headers.get("content-type", "").split(";")[0].strip().lower()
    if ctype not in ("image/png", "image/jpeg", "image/webp"):
        return JSONResponse({"detail": f"unsupported content-type {ctype!r}; use image/png, image/jpeg or image/webp"}, status_code=415)

    body = await request.body()
    try:
        from PIL import Image

        img = Image.open(io.BytesIO(body))
        img.load()
        img = img.convert("RGB")
    except Exception:
        return JSONResponse({"detail": "invalid image payload"}, status_code=400)

    loop = asyncio.get_running_loop()
    result = await loop.run_in_executor(EXECUTOR, _infer, img)
    return JSONResponse(result)


if __name__ == "__main__":
    import uvicorn

    uvicorn.run(app, host=BIND, port=PORT)
