# AURA M9 / Jev Upgrade — OmniParser + Local Jev SystemOne detector service
# (D3 external service / C1 bearer token gate / C2 weights mounted from PVC).
#
# Supports:
# 1. GET  /healthz        — readiness + device/dtype + capabilities + caption_mode
# 2. POST /detect         — fast YOLO geometry detection (?caption=0 default) or +Florence caption (?caption=1)
# 3. POST /v1/systemone   — Jev System One compatible primitive endpoint (noul / choice / score / box)
# 4. POST /select         — alias of /v1/systemone
import asyncio
import base64
import io
import logging
import math
import os
import re
import secrets
import sys
import time
from concurrent.futures import ThreadPoolExecutor
from contextlib import asynccontextmanager
from pathlib import Path

log = logging.getLogger("aura.detector")
logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(name)s %(message)s")

TOKEN = os.environ.get("AURA_DETECTOR_TOKEN", "")
WEIGHTS_DIR = Path(os.environ.get("AURA_WEIGHTS_DIR", "/weights"))
BIND = os.environ.get("AURA_DETECTOR_BIND", "0.0.0.0")
PORT = int(os.environ.get("AURA_DETECTOR_PORT", "8080"))
BOX_THRESHOLD = float(os.environ.get("AURA_BOX_THRESHOLD", "0.05"))
CAPTION_MODE = os.environ.get("AURA_CAPTION_MODE", "none").strip().lower()
MAX_QUEUE = int(os.environ.get("AURA_MAX_QUEUE", "4"))
MAX_NEW_TOKENS = 20
TOP_K_ICON_CAPTIONS = int(os.environ.get("AURA_TOPK_ICON_CAPTIONS", "16"))

if not TOKEN:
    log.error("AURA_DETECTOR_TOKEN is not set; refusing to start without bearer token configured")
    sys.exit(2)


class _Models:
    yolo = None
    florence = None
    processor = None
    device = "cpu"
    dtype = None
    florence_via = ""
    caption_batch = 1


M = _Models()
EXECUTOR = ThreadPoolExecutor(max_workers=1)
QUEUE_SEM = None


def _load_models():
    import torch
    from ultralytics import YOLO
    from transformers import AutoModelForCausalLM, AutoProcessor

    M.device = "cuda" if torch.cuda.is_available() else "cpu"
    M.dtype = torch.float16 if M.device == "cuda" else torch.float32
    M.caption_batch = 32 if M.device == "cuda" else 8

    det = WEIGHTS_DIR / "icon_detect" / "model.pt"
    cap = WEIGHTS_DIR / "icon_caption_florence"
    if not det.is_file() or not (cap / "model.safetensors").is_file():
        log.error(
            "weights not found under %s (need icon_detect/model.pt + icon_caption_florence/); see README.md",
            WEIGHTS_DIR,
        )
        sys.exit(3)

    log.info("loading icon_detect YOLO from %s", det)
    M.yolo = YOLO(str(det))

    log.info("loading icon_caption_florence from %s (device=%s dtype=%s)", cap, M.device, M.dtype)
    common = dict(local_files_only=True, torch_dtype=M.dtype)
    try:
        M.florence = AutoModelForCausalLM.from_pretrained(
            cap, trust_remote_code=False, attn_implementation="sdpa", **common
        )
        M.processor = AutoProcessor.from_pretrained(cap, trust_remote_code=False, local_files_only=True)
        M.florence_via = "transformers-native"
    except Exception as e:
        log.warning("native florence2 load failed (%s); falling back to in-dir remote code", e)
        M.florence = AutoModelForCausalLM.from_pretrained(cap, trust_remote_code=True, **common)
        M.processor = AutoProcessor.from_pretrained(cap, trust_remote_code=True, local_files_only=True)
        M.florence_via = "local-remote-code"
    M.florence = M.florence.to(M.device)
    if M.device == "cpu":
        M.florence = M.florence.float()
    M.florence.eval()
    log.info("models ready (device=%s dtype=%s florence_via=%s caption_mode=%s)", M.device, M.dtype, M.florence_via, CAPTION_MODE)


def _caption(crops):
    """Batch-generate semantic captions for cropped icon regions."""
    import torch

    if not crops or M.florence is None:
        return [""] * len(crops)
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


def _yolo_boxes(img):
    """Run YOLO detection and return list of (x1, y1, x2, y2, conf, cls_name)."""
    w, h = img.size
    res = M.yolo.predict(img, conf=BOX_THRESHOLD, device=M.device, verbose=False)[0]
    names = res.names or {}
    boxes = res.boxes.xyxy.cpu().numpy().tolist()
    confs = res.boxes.conf.cpu().numpy().tolist()
    clses = res.boxes.cls.int().cpu().numpy().tolist()

    kept = []
    for (x1, y1, x2, y2), conf, cls in zip(boxes, confs, clses):
        x1, y1 = max(0.0, x1), max(0.0, y1)
        x2, y2 = min(float(w), x2), min(float(h), y2)
        if x2 - x1 < 3 or y2 - y1 < 3:
            continue
        kept.append((x1, y1, x2, y2, float(conf), str(names.get(cls, "icon"))))
    return kept


def _infer(img, with_caption=False):
    """Synchronous /detect inference: YOLO geometry + optional Florence captioning."""
    w, h = img.size
    kept = _yolo_boxes(img)
    if with_caption and kept:
        crops = [img.crop((int(x1), int(y1), int(x2), int(y2))) for (x1, y1, x2, y2, _, _) in kept]
        captions = _caption(crops)
    else:
        captions = [""] * len(kept)

    elements = [
        {
            "bbox": [round(x1, 1), round(y1, 1), round(x2 - x1, 1), round(y2 - y1, 1)],
            "type": cls_name,
            "caption": cap,
            "confidence": round(conf, 4),
        }
        for (x1, y1, x2, y2, conf, cls_name), cap in zip(kept, captions)
    ]
    return {
        "elements": elements,
        "image_w": w,
        "image_h": h,
        "caption_mode": "florence" if with_caption else "none",
    }


_LOC_RE = re.compile(r"<loc_(\d+)><loc_(\d+)><loc_(\d+)><loc_(\d+)>")
_BBOX_HINT_RE = re.compile(r"@\(\s*(-?\d+(?:\.\d+)?)\s*,\s*(-?\d+(?:\.\d+)?)\s*,\s*(\d+(?:\.\d+)?)\s*,\s*(\d+(?:\.\d+)?)\s*\)")


def _iou_xyxy(a, b):
    ix1 = max(a[0], b[0])
    iy1 = max(a[1], b[1])
    ix2 = min(a[2], b[2])
    iy2 = min(a[3], b[3])
    iw = max(0.0, ix2 - ix1)
    ih = max(0.0, iy2 - iy1)
    inter = iw * ih
    if inter <= 0.0:
        return 0.0
    ua = max(0.0, a[2] - a[0]) * max(0.0, a[3] - a[1])
    ub = max(0.0, b[2] - b[0]) * max(0.0, b[3] - b[1])
    denom = ua + ub - inter
    return inter / denom if denom > 0.0 else 0.0


_GENERIC_ROLE_TOKENS = {
    "button", "push", "icon", "label", "frame", "window", "pane", "layered",
    "item", "menu", "text", "box", "check", "indicator", "control", "widget", "element",
}


def _tokenize(s: str) -> list[str]:
    s = re.sub(r"([a-z])([A-Z0-9])", r"\1 \2", s or "")
    s = re.sub(r"([0-9])([a-zA-Z])", r"\1 \2", s)
    return [tok for tok in re.split(r"[\s_\-/,:;.\[\]()@\"']+", s.lower()) if tok]


def _text_similarity(target: str, text: str) -> float:
    t = (target or "").strip().lower()
    s = (text or "").strip().lower()
    if not t or not s:
        return 0.0
    if t == s:
        return 1.0
    t_tokens = _tokenize(target)
    s_tokens = _tokenize(text)
    if not t_tokens or not s_tokens:
        return 0.0
    content_t = [tok for tok in t_tokens if tok not in _GENERIC_ROLE_TOKENS]
    if t in s:
        return 0.85 + 0.15 * min(1.0, len(t) / max(1, len(s)))
    if s in t and len(s) >= 3 and s not in _GENERIC_ROLE_TOKENS and (len(s) / max(1, len(t))) >= 0.35:
        return 0.72

    content_hits = 0.0
    total_hits = 0.0
    for tok in t_tokens:
        is_content = tok not in _GENERIC_ROLE_TOKENS
        matched = 0.0
        if tok in s_tokens:
            matched = 1.0
        elif len(tok) >= 3 and any(
            (tok in st or st in tok) and (min(len(tok), len(st)) / max(len(tok), len(st)) >= 0.5)
            for st in s_tokens
            if len(st) >= 3
        ):
            matched = 0.65
        total_hits += matched
        if is_content:
            content_hits += matched

    if content_t and content_hits <= 0.0:
        return 0.0
    if content_t:
        return min(0.84, (0.8 * (content_hits / len(content_t)) + 0.2 * (total_hits / len(t_tokens))))
    return min(0.75, total_hits / len(t_tokens))


def _ground_phrase_florence(img, target: str):
    """Run single-pass Florence-2 phrase grounding if supported by checkpoint, with hallucination guard."""
    import torch

    if img is None or M.florence is None or not target:
        return None
    w, h = img.size
    if w <= 0 or h <= 0:
        return None
    prompt = f"<CAPTION_TO_PHRASE_GROUNDING>{target}"
    try:
        inputs = M.processor(text=[prompt], images=[img], return_tensors="pt")
        inputs = {k: v.to(M.device) for k, v in inputs.items()}
        if M.device == "cuda":
            inputs["pixel_values"] = inputs["pixel_values"].to(M.dtype)
        with torch.inference_mode():
            gen = M.florence.generate(
                input_ids=inputs["input_ids"],
                pixel_values=inputs["pixel_values"],
                max_new_tokens=32,
                num_beams=1,
                do_sample=False,
            )
        raw = M.processor.batch_decode(gen, skip_special_tokens=False)[0]
        cand_box = None
        m = _LOC_RE.search(raw)
        if m:
            x1 = float(int(m.group(1))) / 1000.0 * w
            y1 = float(int(m.group(2))) / 1000.0 * h
            x2 = float(int(m.group(3))) / 1000.0 * w
            y2 = float(int(m.group(4))) / 1000.0 * h
            if x2 > x1 + 2 and y2 > y1 + 2:
                cand_box = (x1, y1, x2, y2)
        elif hasattr(M.processor, "post_process_generation"):
            parsed = M.processor.post_process_generation(
                raw, task="<CAPTION_TO_PHRASE_GROUNDING>", image_size=(w, h)
            )
            g = parsed.get("<CAPTION_TO_PHRASE_GROUNDING>", {})
            bboxes = g.get("bboxes") or []
            if bboxes:
                bx = bboxes[0]
                if len(bx) == 4 and bx[2] > bx[0] + 2 and bx[3] > bx[1] + 2:
                    cand_box = (float(bx[0]), float(bx[1]), float(bx[2]), float(bx[3]))
        if cand_box is not None:
            x1, y1, x2, y2 = cand_box
            area_ratio = ((x2 - x1) * (y2 - y1)) / float(w * h)
            # Reject degenerate full-screen / background container boxes returned when target is absent
            if area_ratio > 0.60 or area_ratio < 0.0001:
                return None
            return (x1, y1, x2, y2, 0.86)
    except Exception as e:
        log.debug("phrase grounding pass fallback: %s", e)
    return None


def _extract_state_inputs(payload: dict):
    """Extract (PIL.Image or None, state_text, target_hint) from SystemOne payload."""
    from PIL import Image

    state = payload.get("state")
    img = None
    texts = []
    if isinstance(state, str):
        texts.append(state)
    elif isinstance(state, list):
        for item in state:
            if not isinstance(item, dict):
                continue
            itype = item.get("type", "")
            if itype == "text":
                texts.append(str(item.get("text", "")))
            elif itype == "image":
                src = item.get("source") or {}
                b64 = src.get("data") or item.get("data") or ""
                if b64:
                    raw = base64.b64decode(b64)
                    im = Image.open(io.BytesIO(raw))
                    im.load()
                    img = im.convert("RGB")
            elif itype == "image_url":
                url = (item.get("image_url") or {}).get("url", "")
                if ";base64," in url:
                    b64 = url.split(";base64,", 1)[1]
                    raw = base64.b64decode(b64)
                    im = Image.open(io.BytesIO(raw))
                    im.load()
                    img = im.convert("RGB")
    target = str(payload.get("target") or "").strip()
    if not target:
        questions = payload.get("questions") or {}
        for q in questions.values():
            if isinstance(q, dict):
                qtext = str(q.get("question") or "")
                qm = re.search(r"['\"「]([^'\"」]{1,120})['\"」]", qtext)
                if qm:
                    target = qm.group(1).strip()
                    break
                if qtext:
                    target = qtext.strip()
                    break
    return img, "\n".join(texts), target


def _parse_choice_bbox(choice_str: str):
    """Parse @(x,y,w,h) from a candidate choice string if present."""
    m = _BBOX_HINT_RE.search(choice_str or "")
    if not m:
        return None
    x, y, w, h = float(m.group(1)), float(m.group(2)), float(m.group(3)), float(m.group(4))
    return (x, y, x + w, y + h)


def _strip_choice_meta(choice_str: str) -> str:
    """Extract clean semantic text from '0: [button] Submit @(100,200,80,32)'."""
    s = re.sub(r"^\s*\d+\s*:\s*", "", choice_str or "")
    s = _BBOX_HINT_RE.sub("", s)
    s = re.sub(r"^\s*\[[^\]]*\]\s*", "", s)
    return s.strip()


def _systemone_infer(payload: dict) -> dict:
    """Execute Jev System One primitives (noul, choice, score, box) on local GPU."""
    t0 = time.perf_counter()
    img, state_text, target = _extract_state_inputs(payload)
    questions = payload.get("questions") or {}

    yolo_kept = _yolo_boxes(img) if img is not None else []
    grounded = _ground_phrase_florence(img, target) if (img is not None and target) else None
    if grounded is not None and yolo_kept:
        gx1, gy1, gx2, gy2, gconf = grounded
        best_iou, best_box = 0.0, None
        for (x1, y1, x2, y2, yconf, _) in yolo_kept:
            iou = _iou_xyxy((gx1, gy1, gx2, gy2), (x1, y1, x2, y2))
            if iou > best_iou:
                best_iou = iou
                best_box = (x1, y1, x2, y2, max(gconf, yconf))
        if best_box is not None and best_iou >= 0.25:
            grounded = best_box

    # Gather all choices from any choice question to share candidate scoring with noul/score/box
    all_choices = []
    for q in questions.values():
        if isinstance(q, dict) and q.get("type") == "choice":
            all_choices = [str(c) for c in (q.get("choices") or [])]
            break

    choice_scores = []
    if all_choices:
        for idx, cstr in enumerate(all_choices):
            clean_label = _strip_choice_meta(cstr)
            sim = _text_similarity(target, clean_label)
            if sim == 0.0:
                sim = _text_similarity(target, cstr) * 0.6
            cbbox = _parse_choice_bbox(cstr)
            choice_scores.append([idx, cstr, cbbox, sim, clean_label])

    best_lex = max((item[3] for item in choice_scores), default=0.0)

    # Verify phrase-grounding box and caption top-K unlabeled/small candidate crops in ONE GPU batch when lexical match is weak
    if best_lex < 0.65 and img is not None:
        w, h = img.size
        batch_crops = []
        batch_tags = []  # ("grounded", -1) or ("choice", idx)

        if grounded is not None:
            gx1 = max(0, min(w - 1, int(grounded[0])))
            gy1 = max(0, min(h - 1, int(grounded[1])))
            gx2 = max(gx1 + 1, min(w, int(grounded[2])))
            gy2 = max(gy1 + 1, min(h, int(grounded[3])))
            if gx2 - gx1 >= 3 and gy2 - gy1 >= 3:
                batch_crops.append(img.crop((gx1, gy1, gx2, gy2)))
                batch_tags.append(("grounded", -1))

        # Prioritize unlabeled visual candidates (clean_label == "") and smaller specific controls over large container boxes
        def _cand_area(cb):
            if cb is None:
                return 1e12
            return max(1.0, (cb[2] - cb[0]) * (cb[3] - cb[1]))

        candidate_order = sorted(
            choice_scores,
            key=lambda it: (0 if it[4] == "" else 1, _cand_area(it[2]), -it[3]),
        )
        for item in candidate_order:
            idx, cstr, cbbox, sim, clean_label = item
            if cbbox is None or len(batch_crops) >= max(TOP_K_ICON_CAPTIONS, 12) + 1:
                continue
            x1 = max(0, min(w - 1, int(cbbox[0])))
            y1 = max(0, min(h - 1, int(cbbox[1])))
            x2 = max(x1 + 1, min(w, int(cbbox[2])))
            y2 = max(y1 + 1, min(h, int(cbbox[3])))
            bw, bh = x2 - x1, y2 - y1
            if bw < 3 or bh < 3 or (bw * bh) > 0.30 * (w * h):
                continue
            batch_crops.append(img.crop((x1, y1, x2, y2)))
            batch_tags.append(("choice", idx))

        if batch_crops:
            caps = _caption(batch_crops)
            grounded_verified = False
            for (tag_kind, tag_idx), cap in zip(batch_tags, caps):
                cap_sim = _text_similarity(target, cap)
                if tag_kind == "grounded":
                    if cap_sim >= 0.30:
                        grounded_verified = True
                        grounded = (grounded[0], grounded[1], grounded[2], grounded[3], max(0.75, cap_sim))
                elif tag_kind == "choice" and cap_sim > choice_scores[tag_idx][3]:
                    choice_scores[tag_idx][3] = cap_sim
            if grounded is not None and not grounded_verified:
                # Check if grounded overlaps any candidate that now has >=0.35 similarity
                overlaps_match = any(
                    it[2] is not None and it[3] >= 0.35 and _iou_xyxy((grounded[0], grounded[1], grounded[2], grounded[3]), it[2]) >= 0.25
                    for it in choice_scores
                )
                if not overlaps_match:
                    grounded = None

    # Apply verified grounded box IoU boost to choice_scores
    if grounded is not None and choice_scores:
        for item in choice_scores:
            cbbox = item[2]
            if cbbox is not None:
                iou = _iou_xyxy((grounded[0], grounded[1], grounded[2], grounded[3]), cbbox)
                if iou >= 0.25:
                    item[3] = max(item[3], iou * 0.9)

    # Specificity / containment refinement: when an enclosing container box and an inner control box
    # both match the target (because the container crop also includes the inner control's text/icon),
    # prefer the tighter inner control unless the target explicitly asks for a container.
    target_toks = set(_tokenize(target))
    wants_container = bool(target_toks & {"window", "dialog", "frame", "panel", "screen", "container"})
    if not wants_container and len(choice_scores) > 1:
        n_cands = len(choice_scores)
        contained_by = [False] * n_cands
        contains_inner = [False] * n_cands
        for i in range(n_cands):
            bi = choice_scores[i][2]
            si = choice_scores[i][3]
            if bi is None or si < 0.35:
                continue
            area_i = max(1.0, (bi[2] - bi[0]) * (bi[3] - bi[1]))
            for j in range(n_cands):
                if i == j:
                    continue
                bj = choice_scores[j][2]
                sj = choice_scores[j][3]
                if bj is None or sj < 0.35:
                    continue
                area_j = max(1.0, (bj[2] - bj[0]) * (bj[3] - bj[1]))
                if area_i >= 1.8 * area_j:
                    ix1 = max(bi[0], bj[0])
                    iy1 = max(bi[1], bj[1])
                    ix2 = min(bi[2], bj[2])
                    iy2 = min(bi[3], bj[3])
                    inter = max(0.0, ix2 - ix1) * max(0.0, iy2 - iy1)
                    if inter / area_j >= 0.75:
                        contains_inner[i] = True
                        contained_by[j] = True
        for i in range(n_cands):
            if contains_inner[i] and not contained_by[i]:
                choice_scores[i][3] = round(choice_scores[i][3] * 0.65, 4)
            elif contained_by[i]:
                choice_scores[i][3] = round(min(0.99, choice_scores[i][3] + 0.05), 4)

    # If no choices were supplied (or box-only query) and grounded is still None, caption top-K YOLO boxes
    best_yolo_box = grounded
    if best_yolo_box is None and img is not None and yolo_kept and target:
        w, h = img.size
        top_kept = sorted(yolo_kept, key=lambda b: b[4], reverse=True)[:TOP_K_ICON_CAPTIONS]
        crops = [img.crop((int(b[0]), int(b[1]), int(b[2]), int(b[3]))) for b in top_kept]
        caps = _caption(crops)
        best_sim = 0.0
        for (x1, y1, x2, y2, yconf, _), cap in zip(top_kept, caps):
            sim = _text_similarity(target, cap)
            if sim > best_sim:
                best_sim = sim
                best_yolo_box = (x1, y1, x2, y2, round(0.5 * sim + 0.5 * yconf, 4))
        if best_sim < 0.35:
            best_yolo_box = None

    # Compute overall existence confidence (noul)
    max_choice_sim = max((item[3] for item in choice_scores), default=0.0)
    state_sim = _text_similarity(target, state_text) if state_text else 0.0
    if max_choice_sim > 0.0:
        existence_prob = min(0.99, max(0.05, 0.15 + 0.84 * max_choice_sim))
    elif best_yolo_box is not None:
        existence_prob = min(0.96, max(0.20, float(best_yolo_box[4])))
    elif state_sim > 0.0:
        existence_prob = min(0.95, max(0.10, state_sim))
    else:
        existence_prob = 0.08

    answers = {}
    for qid, q in questions.items():
        if not isinstance(q, dict):
            continue
        qtype = q.get("type", "")
        if qtype == "noul":
            p_yes = round(float(existence_prob), 4)
            answers[qid] = {
                "type": "noul",
                "noul": p_yes,
                "probabilities": {"yes": p_yes, "no": round(1.0 - p_yes, 4)},
            }
        elif qtype == "score":
            answers[qid] = {
                "type": "score",
                "score": round(float(existence_prob), 4),
            }
        elif qtype == "choice":
            qchoices = [str(c) for c in (q.get("choices") or [])]
            if not qchoices:
                answers[qid] = {"type": "choice", "choice": "", "choice_index": -1, "confidence": 0.0, "probabilities": {}}
                continue
            # Use precomputed choice_scores if same list, else score on the fly
            raw_scores = []
            for idx, cstr in enumerate(qchoices):
                if idx < len(choice_scores) and choice_scores[idx][1] == cstr:
                    s = choice_scores[idx][3]
                else:
                    s = _text_similarity(target, _strip_choice_meta(cstr))
                raw_scores.append(s)
            # Calibrated softmax with temperature
            temp = 0.15
            max_s = max(raw_scores)
            exps = [math.exp((s - max_s) / temp) for s in raw_scores]
            total_exp = sum(exps) or 1.0
            probs = [e / total_exp for e in exps]

            def _choice_tie_key(i: int):
                cb = choice_scores[i][2] if i < len(choice_scores) else None
                area = max(1.0, (cb[2] - cb[0]) * (cb[3] - cb[1])) if cb is not None else 1e12
                return (round(raw_scores[i], 3), -area, probs[i])

            best_idx = max(range(len(qchoices)), key=_choice_tie_key)
            # Calibrate confidence so poor lexical matches don't claim 99% confidence
            best_raw = raw_scores[best_idx]
            calibrated_conf = round(float(probs[best_idx]) * min(1.0, max(0.2, best_raw + 0.15)), 4)
            prob_map = {qchoices[i]: round(float(probs[i]), 4) for i in range(len(qchoices))}
            answers[qid] = {
                "type": "choice",
                "choice": qchoices[best_idx],
                "choice_index": best_idx,
                "confidence": calibrated_conf,
                "probabilities": prob_map,
            }
        elif qtype == "box":
            w, h = img.size if img is not None else (1.0, 1.0)
            chosen_box = best_yolo_box
            if chosen_box is None and choice_scores:
                best_item = max(
                    choice_scores,
                    key=lambda it: (
                        round(it[3], 3),
                        -max(1.0, (it[2][2] - it[2][0]) * (it[2][3] - it[2][1])) if it[2] is not None else -1e12,
                    ),
                )
                if best_item[2] is not None and best_item[3] >= 0.3:
                    bx = best_item[2]
                    chosen_box = (bx[0], bx[1], bx[2], bx[3], best_item[3])
            if chosen_box is not None and w > 0 and h > 0:
                x1, y1, x2, y2, bconf = chosen_box
                answers[qid] = {
                    "type": "box",
                    "box": {
                        "top": round(max(0.0, min(1.0, y1 / h)), 4),
                        "bottom": round(max(0.0, min(1.0, y2 / h)), 4),
                        "left": round(max(0.0, min(1.0, x1 / w)), 4),
                        "right": round(max(0.0, min(1.0, x2 / w)), 4),
                    },
                    "confidence": round(float(bconf), 4),
                }
            else:
                answers[qid] = {
                    "type": "box",
                    "box": {"top": 0.0, "bottom": 0.0, "left": 0.0, "right": 0.0},
                    "confidence": 0.0,
                }

    elapsed_ms = round((time.perf_counter() - t0) * 1000.0, 1)
    return {
        "answers": answers,
        "model": "jev-local-florence2-yolo",
        "device": M.device,
        "latency_ms": elapsed_ms,
    }


# ---- HTTP Layer ----
from fastapi import FastAPI, Request  # noqa: E402
from fastapi.responses import JSONResponse  # noqa: E402


@asynccontextmanager
async def lifespan(_app):
    global QUEUE_SEM
    QUEUE_SEM = asyncio.Semaphore(MAX_QUEUE)
    _load_models()
    yield


app = FastAPI(lifespan=lifespan)


def _check_bearer(request: Request) -> bool:
    auth = request.headers.get("authorization", "")
    return auth.startswith("Bearer ") and secrets.compare_digest(auth[7:], TOKEN)


@app.get("/healthz")
async def healthz():
    return {
        "status": "ok",
        "device": M.device,
        "dtype": str(M.dtype),
        "florence_via": M.florence_via,
        "caption_mode": CAPTION_MODE,
        "model": "jev-local-florence2-yolo",
        "capabilities": ["detect", "noul", "choice", "score", "box"],
    }


@app.post("/detect")
async def detect(request: Request):
    if not _check_bearer(request):
        return JSONResponse({"detail": "unauthorized"}, status_code=401)

    ctype = request.headers.get("content-type", "").split(";")[0].strip().lower()
    if ctype not in ("image/png", "image/jpeg", "image/webp"):
        return JSONResponse(
            {"detail": f"unsupported content-type {ctype!r}; use image/png, image/jpeg or image/webp"},
            status_code=415,
        )

    cap_param = request.query_params.get("caption", "").strip().lower()
    if cap_param in ("1", "true", "florence"):
        with_caption = True
    elif cap_param in ("0", "false", "none"):
        with_caption = False
    else:
        with_caption = CAPTION_MODE == "florence"

    body = await request.body()
    try:
        from PIL import Image

        img = Image.open(io.BytesIO(body))
        img.load()
        img = img.convert("RGB")
    except Exception:
        return JSONResponse({"detail": "invalid image payload"}, status_code=400)

    if QUEUE_SEM.locked():
        return JSONResponse({"detail": "detector queue saturated"}, status_code=429)

    async with QUEUE_SEM:
        loop = asyncio.get_running_loop()
        result = await loop.run_in_executor(EXECUTOR, _infer, img, with_caption)
    return JSONResponse(result)


@app.post("/v1/systemone")
@app.post("/select")
async def systemone(request: Request):
    if not _check_bearer(request):
        return JSONResponse({"detail": "unauthorized"}, status_code=401)

    try:
        payload = await request.json()
    except Exception:
        return JSONResponse({"detail": "invalid json payload"}, status_code=400)

    if not isinstance(payload, dict) or "questions" not in payload:
        return JSONResponse({"detail": "missing questions field in payload"}, status_code=400)

    if QUEUE_SEM.locked():
        return JSONResponse({"detail": "detector queue saturated"}, status_code=429)

    async with QUEUE_SEM:
        loop = asyncio.get_running_loop()
        result = await loop.run_in_executor(EXECUTOR, _systemone_infer, payload)
    return JSONResponse(result)


if __name__ == "__main__":
    import uvicorn

    uvicorn.run(app, host=BIND, port=PORT)
