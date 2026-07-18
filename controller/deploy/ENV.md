# aura-controller 环境变量权威清单（批E D4）

> 生成依据：全仓 `grep -rE '"AURA_[A-Z0-9_]+"' controller --include='*.go'`（2026-07-17 批E 走查）。
> 双副本部署纪律：两副本用同一 env 模板（仅 `AURA_REPLICA_ID` 与监听地址类差异化），启动日志的
> `effective config` 摘要行（main.go 打印，敏感值掩码）可直接 diff 两副本核对一致性。
> 生产实际值所在：各副本宿主的 aura-env 文件（环境私有，不入仓库）。

## 核心监听 / 身份

| 变量 | 默认 | 说明 |
|---|---|---|
| `AURA_GRPC_ADDR` | `:7443` | mTLS gRPC 监听（节点反连双向流） |
| `AURA_REST_ADDR` | `:18080` | TLS+bearer REST 监听（管理面 + console + enroll） |
| `AURA_METRICS_ADDR` | `:18090` | 明文 HTTP `/metrics`（Prometheus）；空=禁用 |
| `AURA_CERT_DIR` | `deploy/certs` | ca.crt / server.crt / server.key 目录 |
| `AURA_REPLICA_ID` | hostname | 本副本标识（HA 双副本 owner 路由；生产 `replica-1`/`replica-2`） |

## 认证令牌（批E C1 三档分级 + 凭据解耦）

| 变量 | 默认 | 说明 |
|---|---|---|
| `AURA_BEARER_TOKEN` | 空（拒绝所有请求） | admin 档全权令牌（既有单令牌部署即此档） |
| `AURA_BEARER_TOKEN_OPS` | 空（不启用该档） | ops 档：可派发常规工具；高影响工具（run_command/kill_process/file_push）拒绝 |
| `AURA_BEARER_TOKEN_RO` | 空（不启用该档） | ro 档：只读查询；一切派发/编排拒绝 |
| `AURA_FORWARD_TOKEN` | 回落 `AURA_BEARER_TOKEN` | 跨副本转发专用凭据（admin 档准入，独立轮换） |
| `AURA_TANGO_TOKEN` | 回落 `AURA_BEARER_TOKEN` | Tango 流桥专用凭据（桥自持校验，独立轮换） |

## 状态存储（未配置即纯内存降级；批E C2 启动有界重试）

| 变量 | 默认 | 说明 |
|---|---|---|
| `AURA_PG_DSN` | 空（纯内存） | PostgreSQL DSN（审计/trace/节点台账/enroll token） |
| `AURA_REDIS_ADDR` | 空（进程内） | Redis（健康 TTL / owner 路由 / 租约共享 / node 锁） |
| `AURA_TRACE_LEASE_TTL` | `30m` | 录制会话独占租约 TTL |
| `AURA_NODE_REAP_DAYS` | `30` | 长期离线僵尸节点自动遗忘阈值（天） |
| `AURA_AGENT_CALLS_RETENTION_DAYS` | `7` | 直连 MCP agent 调用流水/静默会话保留期（M13 接入观测；6h 周期后台清理，仅 PG 配置时生效） |

> **节点侧相关变量（不在本清单范畴，记于此便于查找）**：`AURA_MCP_TOKEN` —— 节点 `/mcp` 可选访问令牌
> 门槛（M13）：设置即要求 `Authorization: Bearer`，未设保持开放接入。配置处为各节点的 service env
> （如 aura-node.env），非 controller aura-env。

## 对象存储（MinIO；未配置即旁路上传/录屏面降级）

| 变量 | 默认 | 说明 |
|---|---|---|
| `AURA_MINIO_ENDPOINT` | 空（禁用） | 内部 client 端点（host:port） |
| `AURA_MINIO_ACCESS_KEY` / `AURA_MINIO_SECRET_KEY` | — | 凭据 |
| `AURA_MINIO_SECURE` | `false` | 是否 TLS |
| `AURA_MINIO_PUBLIC_ENDPOINT` | 空 | presigned URL 默认签发端点（节点可达面） |
| `AURA_MINIO_ENDPOINTS` | 空 | 按网络域分派签发端点（`lan=…,jump=…`，M12 T07） |

## HA 双副本

| 变量 | 默认 | 说明 |
|---|---|---|
| `AURA_REPLICA_PEERS` | 空（禁用转发） | 副本直连对称表 `replica-1=https://…,replica-2=https://…`（含 self，不用 VIP） |
| `AURA_PEER_TLS_SERVERNAME` | 空（按 URL host） | peer 证书 SAN 域名（IP 直连 + 域名证书时设 `aura-controller`） |

## 设备接入（M12 enroll；CA+PG 均就位才启用）

| 变量 | 默认 | 说明 |
|---|---|---|
| `AURA_CA_KEY_PATH` | 空（签发面禁用） | 签发 CA 私钥路径（0600；配置了但加载失败 → fail-closed 拒启动） |
| `AURA_CONTROLLER_ENDPOINT` | — | install_command 组装的控制面地址（console 生成一键命令用） |
| `AURA_RELEASE_HOST` | — | install_command 组装的安装脚本/二进制托管地址 |

## 环境置备（PVE / K8s 单选；两者同配拒启动）

| 变量 | 默认 | 说明 |
|---|---|---|
| `AURA_PVE_URL` / `AURA_PVE_TOKEN_ID` / `AURA_PVE_SECRET` | 空 | PVE API 端点与 token |
| `AURA_PVE_NODE` / `AURA_PVE_TEMPLATE_VMID` / `AURA_PVE_VMID_BASE` / `AURA_PVE_INSECURE` | — | PVE 置备参数 |
| `AURA_K8S_KUBECONFIG` / `AURA_K8S_NAMESPACE` / `AURA_K8S_EPHEMERAL_IMAGE` | 空 | K8s 置备参数 |

## 可观测 / 视觉融合 / 实时流

| 变量 | 默认 | 说明 |
|---|---|---|
| `AURA_OTLP_ENDPOINT` | 空（no-op） | OTLP 追踪导出端点（Jaeger :4317） |
| `AURA_DETECTOR_ENDPOINT` / `AURA_DETECTOR_TOKEN` / `AURA_DETECTOR_TIMEOUT` | 空 / — / `120s` | OmniParser 检测服务（M9 融合面；空=Unavailable） |
| `AURA_FUSION_MAX_SKEW_MS` | `500` | 融合双源采集偏斜阈值 |
| `AURA_TANGO_ADB_ADDR` / `AURA_TANGO_ADB_SERIAL` / `AURA_TANGO_ADB_BIN` | 空 / `localhost:5555` / `adb` | Tango 桥 Redroid adbd 端点（空=桥 E_UNAVAILABLE） |
| `AURA_SCRCPY_SERVER_JAR` | 空（纯隧道） | 预置 scrcpy-server jar 路径 |

## 仅测试用（生产勿配）

`AURA_TEST_PG_DSN`、`AURA_TEST_MINIO_ENDPOINT`、`AURA_TEST_MINIO_ACCESS_KEY`、
`AURA_TEST_MINIO_SECRET_KEY`、`AURA_SMOKE_DETECTOR_URL`、`AURA_SMOKE_DETECTOR_TOKEN`
——集成测试门控变量，缺省 skip 对应用例。
