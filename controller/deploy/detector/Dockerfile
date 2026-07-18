# AURA M9 — OmniParser detector 服务镜像（GPU 主线 + CPU fp32 变体，单镜像 device 自适应）。
#
# 基座 = pytorch/pytorch CUDA runtime：自带匹配 CUDA 的 torch/torchvision（GPU 主线 Q2；
# 无 GPU 时同一镜像内 torch 自动落 CPU，app.py 强制 float32）。requirements.txt 不列
# torch，防 pip 拉 CPU 轮子覆盖基座 CUDA 版。
#
# 权重绝不 COPY 进镜像（C2 / AGPL 策略 c：AURA 不分发 icon_detect 的 AGPL 权重）：
# 运行期从 /weights 挂载（k3s 场景挂 detector-weights PVC，见 WEIGHTS.md；本地冒烟
# docker run -v <weights-dir>:/weights:ro）。镜像内亦无任何 hf/huggingface 下载步骤
# ——目标集群出网被劫持，运行期下载是死路（见 weights-pvc.yaml 注释）。
ARG BASE=pytorch/pytorch:2.13.0-cuda12.6-cudnn9-runtime
FROM ${BASE}

# ultralytics 依赖 opencv-python（非 headless），需系统 libGL/libglib
RUN apt-get update \
    && apt-get install -y --no-install-recommends libgl1 libglib2.0-0 \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app
COPY requirements.txt .
# 基座 Python 为 externally-managed（PEP 668）；容器即隔离环境，直接安装到系统 site
ENV PIP_BREAK_SYSTEM_PACKAGES=1
RUN pip install --no-cache-dir -r requirements.txt

COPY app.py .

# 权重挂载点（运行期只读挂入；镜像内此目录为空）
ENV AURA_WEIGHTS_DIR=/weights
# 服务无 AURA_DETECTOR_TOKEN 会拒绝启动（C1 bearer 主防线，无鉴权不裸奔）
EXPOSE 8080
CMD ["python", "app.py"]
