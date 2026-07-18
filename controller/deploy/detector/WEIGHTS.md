# OmniParser detector weights — supply & staging (AURA M9-P1, SC-2 deploy prerequisite)

The M9 detector pod (T6) mounts its OmniParser v2 checkpoints **read-only at `/weights`
from a pre-staged PVC** — it never downloads anything at runtime. Staging that PVC is a
**hard prerequisite of SC-2**: do it once, before the detector is deployed.

> Scope note: this file owns the *weight supply* story only. The detector service/image
> itself is documented in `README.md` (T2).

## Why a pre-staged PVC (devil C2 — the only self-consistent option)

Two candidate supply paths are dead on arrival for this cluster:

1. **Bake weights into the image** — `icon_detect` is **AGPL-3.0** (inherited from
   Ultralytics YOLO). Shipping it inside an AURA image = AURA conveys AGPL artifacts,
   which violates the project's AGPL policy (c): *AURA never distributes the weights;
   the user self-stages them*.
2. **Runtime `hf download` in the pod** — this cluster's HF egress runs through an
   uncontrolled fake-IP interception layer, so the path is **unreliable by
   construction**. The full picture (all probes in `evidence/m9/weights-stage.txt`):
   - **Host level, persistent**: `huggingface.co` → `198.18.0.86` and
     `us.aws.cdn.hf.co` → `198.18.0.89` (RFC2544 range = transparent-proxy fake-IP
     mode). The old LFS CDN `cdn-lfs.huggingface.co` failed SSL outright at the devil
     C2 probe — runtime download was a hard dead end at that moment.
   - **Pod level, at stage time**: CoreDNS resolved real CloudFront IPs and a genuine
     `hf download` of `icon_detect/model.pt` (40 MB) **succeeded** — HF has since moved
     LFS blobs to the Xet CDN (`us.aws.cdn.hf.co`) and the interception layer happened
     to let it through that moment.
   - **Conclusion**: whether a runtime download works depends on the day's behavior of
     an interception layer AURA does not control, which has already blocked the blob
     path once. A detector restart during a "blocked" window would never come up.

Pre-staging from an internet-capable machine removes that dependency entirely: the
operator downloads the weights themselves (= user self-stage, AGPL-clean: AURA never
conveys them), and the cluster works deterministically and fully offline afterwards.

## What gets staged

HF repo `microsoft/OmniParser-v2.0` @ revision `6600256cb0f1b07651e3bc86166196307bad7e2d`
(main HEAD, lastModified 2025-03-28 — the authoritative V2-checkpoint state for
OmniParser v2.0.1-era deployments; v2.0.1 itself was a server-code security release,
the weight repo did not change). Directory layout follows the official README step
`mv weights/icon_caption weights/icon_caption_florence` = the T2 `/weights` contract:

| PVC path                                    | Content                  | License  | sha256 (prefix) | Size |
|---------------------------------------------|--------------------------|----------|-----------------|------|
| `/weights/icon_detect/model.pt`             | YOLOv8 icon detector     | AGPL-3.0 | `dab3d435`      | 40 MB |
| `/weights/icon_caption_florence/model.safetensors` | Florence-2 captioner | MIT      | `01b934b0`      | 1.08 GB |
| `/weights/icon_caption_florence/` (7 files) | Florence-2-base processor/tokenizer configs + 3 modeling `.py` | MIT | see `SHA256SUMS` | ~2.6 MB |
| `/weights/SHA256SUMS`                       | full manifest (16 files) | —        | —               | — |
| `/weights/REVISION`                         | provenance record        | —        | —               | — |

Each model dir keeps its upstream `LICENSE` plus config/yaml sidecars. Full sha256
manifest lives in the PVC (`SHA256SUMS`) and in `evidence/m9/weights-stage.txt`.
Total ≈ 1.12 GiB (the PVC requests 3Gi; note the Florence safetensors is float32 —
the "~300 MB total" figure circulating in research docs undercounts it).

### Self-sufficient snapshot — two hard requirements (T2 smoke findings)

`icon_caption_florence/` must load with `local_files_only=True`, fully offline. The
raw HF `icon_caption` folder does NOT satisfy that; staging enforces two extra steps
(contract source of truth: `README.md` "/weights" section; both are built into
`stage-weights.sh` pack):

1. **Merge the Florence-2-base files** — the icon_caption repo ships no
   processor/tokenizer files. 7 files come from `microsoft/Florence-2-base` (MIT;
   code/config, no weights — the AGPL boundary is untouched):
   `preprocessor_config.json`, `tokenizer.json`, `tokenizer_config.json`,
   `vocab.json`, `configuration_florence2.py`, `modeling_florence2.py`,
   `processing_florence2.py`. Verified byte-identical to the upstream HF files
   (sha256 cross-check in `evidence/m9/weights-stage.txt`, rework section).
2. **De-reference `config.json` `auto_map`** — upstream values carry a
   `microsoft/Florence-2-base-ft` repo prefix (`<repo>--<module>.<Class>`); with the
   prefix present, transformers ignores the local `.py` files and re-fetches from HF,
   so offline startup dies. Staging strips the prefix to bare local class paths
   (e.g. `configuration_florence2.Florence2Config`).

The T2 smoke-verified reference snapshot (exactly what the PVC now holds) lives on
the control-plane VM at `user@<controller-host>:/tmp/aura-detector-weights/` (tgz
alongside). If you rebuild from scratch via `stage-weights.sh` instead, re-run the
T2 smoke (`healthz` + real `/detect`) before relying on it.

**Not in this PVC:** OCR weights (EasyOCR/PaddleOCR, Apache-2.0). They carry no AGPL
concern, so if the detector image (T2) enables OCR it should bake them into the image —
a runtime OCR-weight download would hit the same egress hijack.

## How to stage (operator, once)

On an **internet-capable machine** (NOT the k3s host), from this directory:

```bash
bash stage-weights.sh          # all phases: download → pack → ship → fill
```

Phases are idempotent and individually resumable (`download|pack|ship|fill`) — safe to
re-run after an interruption; `fill` re-applies the PVC (kubectl apply is idempotent)
and overwrites the volume content (wipe-then-extract). Knobs: `K3S_HOST` (default
`user@<k3s-host>`), `REVISION`, `WORK_DIR`.

`local-path` uses `volumeBindingMode: WaitForFirstConsumer`, so the bare PVC stays
`Pending` until its first consumer — the script's short-lived `weights-stage` pod
triggers binding, fills `/weights`, verifies `sha256sum -c`, probes HF egress from
inside the pod (evidence), and is deleted. The PVC and its data persist.

## Deploy-time assertion (T6 gate)

Before deploying the detector pod, assert:

```bash
kubectl -n aura get pvc detector-weights          # -> STATUS Bound
# weights in place (via any pod mounting the claim, or the stage pod):
kubectl -n aura exec <pod> -- sh -c 'cd /weights && sha256sum -c SHA256SUMS'
# expect: all 16 entries OK (both model blobs + snapshot files + REVISION)
# self-sufficient snapshot holds (tokenizer present, no HF re-fetch references):
kubectl -n aura exec <pod> -- sh -c \
  'ls /weights/icon_caption_florence/tokenizer.json >/dev/null && \
   { grep -rn "Florence-2-base-ft--" /weights && exit 1 || echo SNAPSHOT_OK; }'
```

If the PVC is missing/unbound, the manifest check fails, or the snapshot assert
fails, the detector must not be deployed (SC-2 "detector 就位" cannot hold — a
repo-prefixed `auto_map` means the pod would try to re-fetch from HF at startup
and die offline).

## AGPL posture (policy c)

- The AGPL-3.0 `icon_detect` weights are **never baked into any AURA image and never
  distributed by AURA** — the operator downloads them directly from HuggingFace and
  stages them into their own cluster (user self-stage).
- Upstream `LICENSE` files travel with the weights into the PVC.
- The detector consumes the weights as an isolated, arm's-length HTTP service
  (see `README.md` / the M9 AGPL assessment in `evidence/m9/agpl-assessment.md`).
