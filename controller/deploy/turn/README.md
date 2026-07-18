# AURA TURN（coturn）— 实时流备用 relay

M8-P2 实时流架构的 **fallback 组件**。第三方 harness 进程供给：coturn 属部署 harness 拥有的进程，
**非 AURA 编排面代码**（业务层不 import、不直连；若日后经编排注入 Selkies，走 `SELKIES_TURN_*` env）。

## 定位（装而不 gate）

流架构主线 = Selkies `hostNetwork` ICE 直连。UDP 直连通路已端到端实测「通」
（见 `.workflow/scratch/20260711-plan-M8P2-streaming-fusion/evidence/spike-verdict.md`：**GATE=DIRECT_OK**，
0% 丢包，宿主 tcpdump 双向复核，假阳性排除）。本 coturn 常备但**闲置**，仅当某设备/网络下
UDP 直连不可用时启用，**不 gate SC-4 验收**（用户 R-4 裁决）。

## 资产

| 文件 | 作用 |
|---|---|
| `turnserver.conf` | coturn 最小配置（单一权威源；research §3）。realm=aura.internal，relay 段 49160-49200，`use-auth-secret` REST 鉴权，`relay-ip=<host-tailscale-ip>` advertise |
| `coturn-deploy.yaml` | k3s Pod（`hostNetwork`，`aura` ns，镜像 coturn/coturn:4.14）。ConfigMap 由 turnserver.conf 生成，secret 经 k8s Secret 注入 |
| `udp-echo.yaml` | UDP spike echo 端点（可测后删）。spike 实测实际用宿主裸进程，本 manifest 为 k8s 等价形态 |

## 部署

```bash
# 1) 随机密钥（留存 evidence，不入库）
SECRET=$(openssl rand -hex 16)

# 2) ConfigMap（turnserver.conf 为单一源）+ Secret
kubectl -n aura create configmap coturn-config \
    --from-file=turnserver.conf=controller/deploy/turn/turnserver.conf
kubectl -n aura create secret generic coturn-secret \
    --from-literal=static-auth-secret=$SECRET

# 3) 部署
kubectl apply -f controller/deploy/turn/coturn-deploy.yaml
kubectl -n aura logs coturn        # 启动 banner：Coturn-4.14.0 / realm / relay address

# 镜像拉取失败回退：docker.1ms.run/coturn/coturn:4.14 后 retag，或 k3s ctr images import
```

## 自测（TURN Allocate，REST 时限凭证）

宿主无 `turnutils_uclient`。REST 凭证客户端自算：

```
username = <到期 unix 时间戳>              # 例 now + 3600
password = base64(HMAC-SHA1(secret, username))
```

Allocate 成功应返回 `XOR-RELAYED-ADDRESS = <host-tailscale-ip>:<49160-49200>`。
实测记录见 evidence/`TASK-001-turn-allocate.log`。

## Selkies 注入（fallback 启用时）

```
SELKIES_TURN_HOST=<host-tailscale-ip>
SELKIES_TURN_PORT=3478
SELKIES_TURN_PROTOCOL=udp          # 客户端腿被段间 ACL 封 UDP 时改 tcp + conf 放开 no-udp
SELKIES_TURN_SHARED_SECRET=<coturn static-auth-secret>
```

## 注意

- **绝不启用 `no-udp-relay`**（会断内网 relay→媒体对端的 UDP）。research 综述硬约束。
- relay 段每并发占一端口（49160-49200 = 41 并发上限）；如需更多并发放宽 `max-port`。
- TLS/DTLS 默认关（无证书；Tailscale/WireGuard 已提供传输层加密）。需公网 TURNS 时挂证书并放开 `tls-listening-port`。
