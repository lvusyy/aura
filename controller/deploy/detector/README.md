# AURA detector — OmniParser 视觉检测独立服务（M9-P1）

外置 HTTP 服务：截图进 → UI 元素框 + 功能语义 caption 出。AURA 自有代码（Rust node /
Go controller）经网络消费本服务（arm's length，AGPL 隔离三层之 a），绝不 import 其栈。

**本文件是 `/detect`、`/healthz` 与 `/weights` 契约的单一真源**，供 detector client
（T7）与 k3s 部署（T6）消费。权重供给流程（PVC 灌注）见同目录 `WEIGHTS.md`（T3 交付）。

## 安全模型（C1，CVE-2025-55322）

- OmniParser 官方 `omniparserserver`/omnitool 存在未授权 RCE（**CVE-2025-55322**，
  v2.0.1 修复，根因 = 0.0.0.0 裸奔无鉴权）。本服务**不使用官方 server 代码**——自研
  最小 FastAPI（app.py，仅 /detect + /healthz），无执行面，天然不含该漏洞攻击面。
- **bearer 鉴权为主防线**：`AURA_DETECTOR_TOKEN` 未设置时服务拒绝启动（退出码 2）；
  `/detect` 无 token 或错 token 一律 401（常量时间比较）。服务自身从不依赖网络姿态。
- 部署侧姿态（T6 定案，C1）：consumer（controller）是集群外 PVE VM，ClusterIP 对它
  不可达，故对外暴露 = **NodePort + bearer**（bearer 是 load-bearing 防线，401 探针
  进验收）；**禁 hostNetwork**——bearer 服务直绑宿主 0.0.0.0 会精确复现
  CVE-2025-55322 的暴露形态，detector 明确不循 coturn/selkies 的 hostNetwork 先例。
- 权重 revision pin：HF `microsoft/OmniParser-v2.0` @ `6600256cb0f1b07651e3bc86166196307bad7e2d`
  （V2 checkpoints 终版；服务代码与权重解耦，CVE 属官方 server 代码而非权重）。

## AGPL 边界（C2，策略 c）

`icon_detect` 权重经 ultralytics YOLO 传染 AGPL-3.0。**权重绝不 COPY 进镜像、绝不由
AURA 分发**：镜像仅代码 + 依赖，运行期从 `/weights` 挂载（用户自 stage，见
WEIGHTS.md）。ultralytics（AGPL 库）收敛在本容器内，容器边界即 license 边界。

## HTTP 契约

### `POST /detect`

请求：

| 项 | 值 |
|----|-----|
| Header | `Authorization: Bearer <token>`（必填，缺失/错误 → 401） |
| Header | `Content-Type: image/png` \| `image/jpeg` \| `image/webp`（其他 → 415） |
| Body | 图像原始字节（非 multipart、非 base64） |

响应 200（`application/json`）：

```json
{
  "elements": [
    {
      "bbox": [x, y, w, h],
      "type": "icon",
      "caption": "close button",
      "confidence": 0.93
    }
  ],
  "image_w": 1280,
  "image_h": 720
}
```

- **`bbox` 为输入图像像素空间**（D11）：`[x, y, w, h]`，`(x, y)` = 左上角，浮点像素。
  服务内部已把检测框映射回输入图原始分辨率——消费方（T7 融合）零歧义，直接与
  a11y 树坐标同基对齐，不做任何缩放换算。
- `image_w` / `image_h` = 输入图真实分辨率（坐标对齐基准）。
- `type` = 检测头类名。OmniParser v2 `icon_detect` 为单类权重，当前恒为 `"icon"`
  （可交互元素）；换权重后为对应类集，消费方勿硬编码假设。
- `caption` = Florence-2 生成的功能语义描述（英文短语，如 "settings gear icon"）。
- `confidence` = 检测置信度 0–1。`elements` 按检测器输出序，无排序保证。
- 错误：`401 {"detail":"unauthorized"}`；`415`（Content-Type 不支持）；`400`（图像
  字节不可解码）。
- 并发语义：服务内部单 worker 串行推理队列（单帧推理已饱和算力，排队而非并行）；
  客户端超时应覆盖「队列等待 + 单帧推理」总时长。

### `GET /healthz`

就绪探针，无鉴权（仅暴露就绪状态）。模型 + 权重加载完成前服务不应答请求（ASGI
lifespan 语义），**能返回 200 即全就绪**：

```json
{"status": "ok", "device": "cuda", "dtype": "torch.float16", "florence_via": "transformers-native"}
```

## 延迟预期

| 路径 | dtype | 单帧（1080p 级） |
|------|-------|------------------|
| GPU（CUDA 可用，主线） | fp16 | ~1s |
| CPU（无 CUDA，自动降级） | **float32 强制** | 15–25s |

CPU 路径**强制 float32**（`app.py` 双保险：`torch_dtype=float32` + `.float()`）——
默认 fp16 权重在 CPU 上算子支持极差，会挂死 >1h 无输出（research-omniparser §3.2），
运维会误判为部署故障。此为硬红线，勿"优化"掉。

## 服务范围：OCR 未启用

本服务 = YOLO 检测 + Florence caption 两腿，**不含 OCR**（EasyOCR/PaddleOCR 未安装，
文本语义由融合层的 a11y 腿承担）。若未来启用 OCR：其权重（Apache-2.0，无 AGPL 顾虑）
**必须烤进镜像**，禁止依赖运行期下载——本环境出网是分域名/分时段的 fake-IP 透明代理
（间歇可通 ≠ 可靠），EasyOCR/Paddle 默认启动时下载模型（craft_mlt_25k.pth 等）会在
封锁时段让容器重启即起不来。凡可预置的资源一律预置；AGPL 的 YOLO 权重仍走 PVC，
边界不变。

## 权重挂载契约：`/weights`

镜像不含权重。运行期挂载目录结构（**T3 stage 脚本按此灌 PVC**）：

```
/weights/
  icon_detect/
    model.pt                      # YOLOv8 检测头（AGPL-3.0），40,623,819 bytes
    model.yaml
    train_args.yaml
  icon_caption_florence/          # 必须为自足 HF 快照（离线加载，见下）
    model.safetensors             # Florence-2 微调权重（MIT，float32），1,083,916,964 bytes
    config.json
    generation_config.json
    preprocessor_config.json      # ↓ 以下 6 项来自 microsoft/Florence-2-base（MIT）
    tokenizer.json
    tokenizer_config.json
    vocab.json
    configuration_florence2.py    # ↓ .py 三件 = 离线 remote-code 兜底路径
    modeling_florence2.py
    processing_florence2.py
```

**自足快照要求（硬约束，staging 时执行）**：HF 仓 `OmniParser-v2.0/icon_caption` 只有
权重 + config，**没有** processor/tokenizer 文件，且其 `config.json` 的 `auto_map` 指向
外部仓 `microsoft/Florence-2-base-ft`——目标集群出网被劫持（DNS→198.18.0.86），运行期
任何 HF 回源都是死路。staging 必须做两件事：

1. 把 Florence-2-base 的 processor 配置与 modeling .py（上表 6+3 项）合入
   `icon_caption_florence/`；
2. **净化 `config.json` 的 `auto_map` 仓前缀**：`"microsoft/Florence-2-base-ft--configuration_florence2.Florence2Config"`
   → `"configuration_florence2.Florence2Config"`（实测：带仓前缀时 transformers 无视
   目录内同名 .py、强行回源 HF，离线环境启动即死）。

app.py 以 `local_files_only=True` 加载：优先 transformers 原生 florence2 类（零远程
代码），失败自动落到目录内本地 .py（仍离线；transformers 4.x 上 OmniParser 旧 config
走此路径，与官方 OmniParser 姿态一致）。

**T6 部署前断言**（权重 PVC 由 WEIGHTS.md 流程供给；2026-07-12 已灌注并按本契约
补齐自足快照，SHA256SUMS 覆盖全部 16 文件）：

```bash
kubectl -n aura get pvc detector-weights   # STATUS = Bound
# pod 内（或宿主 local-path PV 目录）：清单完整性 + 内容一致性一步验证
sha256sum -c /weights/SHA256SUMS           # 16 条目全 OK（含 processor 与 .py）
```

## 环境变量

| 变量 | 默认 | 说明 |
|------|------|------|
| `AURA_DETECTOR_TOKEN` | （无） | **必填**。缺失即拒绝启动 |
| `AURA_WEIGHTS_DIR` | `/weights` | 权重挂载点 |
| `AURA_DETECTOR_BIND` | `0.0.0.0` | 容器内监听地址（bearer 为主防线；部署侧勿外露端口） |
| `AURA_DETECTOR_PORT` | `8080` | 监听端口 |
| `AURA_BOX_THRESHOLD` | `0.05` | YOLO 置信度阈值（OmniParser 官方默认） |

## 构建与本地冒烟

```bash
# 构建（控制面 VM docker，复刻 redroid/build-sidecar.sh 拓扑）
bash controller/deploy/detector/build.sh              # 仅 build + tag
EXPORT=1 bash controller/deploy/detector/build.sh     # + docker save -> k3s ctr import

# 本地冒烟（挂本地权重目录；无 GPU 机器自动走 CPU fp32）
docker run -d --name detector -p 18081:8080 \
  -v /path/to/weights:/weights:ro \
  -e AURA_DETECTOR_TOKEN=<token> \
  docker.io/library/aura-detector:m9

curl -s http://127.0.0.1:18081/healthz                       # 就绪后 200
curl -s -X POST http://127.0.0.1:18081/detect \
  -H "Authorization: Bearer <token>" \
  -H "Content-Type: image/png" \
  --data-binary @screenshot.png                              # -> elements[]
curl -s -o /dev/null -w '%{http_code}' -X POST \
  http://127.0.0.1:18081/detect \
  -H "Content-Type: image/png" --data-binary @screenshot.png # -> 401（无 token）
```

## k3s 部署（T6）

```bash
bash controller/deploy/detector/deploy.sh                 # GPU 主线（验收路径）
VARIANT=cpu bash controller/deploy/detector/deploy.sh     # CPU 回退变体
```

deploy.sh 一站式：镜像导入（containerd 缺失时 VM docker save → 宿主 scp → `k3s ctr
images import`）→ 部署前断言（PVC Bound + 宿主侧 `sha256sum -c SHA256SUMS` 16 条全
OK）→ bearer Secret `detector-token` 幂等确保 → apply → rollout 等就绪。

| 项 | 值 |
|----|-----|
| 命名空间 | `aura`（default ns 不碰） |
| GPU manifest | `detector-deploy.yaml` — **`runtimeClassName: nvidia` 必填**（非默认 runtime，缺失则 pod 正常调度但 CUDA 静默失败、torch 落 CPU）+ `nvidia.com/gpu: 1` + nodeSelector |
| CPU manifest | `detector-deploy-cpu.yaml` — 无 runtime/gpu limits，`CUDA_VISIBLE_DEVICES=""` 强制 CPU（app.py 随之 fp32）。与 GPU 同名互替，一次只跑一个 |
| Service | NodePort **30808**（30080 被既有 jumpserver 占用）；`http://<k3s-host>:30808`，controller VM（<controller-host>）同 22.x 段直达 |
| bearer token | Secret `detector-token`；读值 `kubectl -n aura get secret detector-token -o jsonpath='{.data.token}' \| base64 -d`（controller 侧同源注入） |
| GPU 生效断言 | `/healthz` 返回 `"device": "cuda"` + `/detect` 单帧 ~1s（**Running ≠ CUDA 就绪**，必须真推理验证） |

**当前集群现状（2026-07-12 T6 实测）**：`runtimeClassName: nvidia` 链路已验证生效
（GPU pod healthz `device=cuda` fp16、模型 3.19GB 上卡），但宿主两卡显存被常驻
llama-server 占死（GPU 0 剩 380MiB / GPU 1 剩 1.78GB，均 < detector ~4.2GB 需求）
→ /detect 推理 CUDA OOM。**现以 CPU 变体运行**；llama-server 让位后重新
`bash deploy.sh` 即切回 GPU。CPU 实测延迟 **~44s/帧**（1440x1024，64C 宿主与 8C VM
同速——瓶颈是 Florence 自回归解码串行性，加核无益；15-25s 的研究预估过乐观）。
**消费方（T7/T8）超时预算须按 ≥60s/帧 设定**，直至 GPU 就位（~1s/帧）。

## 版本锁定

`requirements.txt` 顶层列包名，torch/torchvision 由基座提供（`Dockerfile` ARG
`BASE=pytorch/pytorch:2.13.0-cuda12.6-cudnn9-runtime`，CUDA 12.6 兼容宿主
driver 595 / RTX 2080 Ti sm_75 + RTX 3080 sm_86）。关键约束
**`transformers==4.49.0`**：5.13.1 与 4.57.x 均与 Florence-2 remote code / OmniParser
旧 config 不兼容（实测三轮：5.x 原生类缺 `florence2_language`、remote code 报
`forced_bos_token_id`；4.56+ attention 重构缺 `_supports_sdpa`）。构建实际解析版本快照见
`evidence/m9/detector-build.txt`（pip freeze 摘录）；升级依赖须重跑冒烟闭环
（healthz + 真图 /detect + 401）。
