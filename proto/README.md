# proto/aura/v1 — AURA 传输契约（proto-first 单一源）

节点↔控制面 gRPC 双向流 + 管理面服务的唯一 proto 源。**proto-first**：接口变更先改此处再生成，禁手写平行接口。

## 设计：哑管道（dumb pipe）

gRPC 层只搬运**传输信封**，不结构化重定义 16 个 MCP 工具。工具语义契约由 MCP JSON schema（`node/crates/aura-node` 的 rmcp+schemars）单一承载：

- `ToolRequest.json_args` — MCP 工具入参 JSON，透传
- `ToolResponse.json_envelope` — `aura-capability` 的 `Envelope{ok,data,error}` JSON，原样回传

这样兑现"能力层零改动接入 gRPC"：节点 gRPC handler 复用同一工具执行核，控制面按同一 Envelope 解码。`ErrorCode` 枚举逐字镜像 `aura-capability/src/types.rs` 的 `CapError::code()`（规范源），加 M2 新增 `E_BUSY`/`E_TIMEOUT`。

## 反连模型

`NodeControl.Connect` 双向流由**节点主动拨出**建立（节点在 NAT/防火墙后，控制面从不回连）。上行首帧 `Register`（node_id 空则控制面分配 UUID），之后周期 `Heartbeat`（应用层，驱动 unhealthy 判定）+ 按需 `ToolResponse`；下行 `ToolRequest`。

## 生成

```bash
# Go（connect-go）：BSR remote plugins，输出 controller/gen/aura/v1/
cd proto && buf lint && buf generate

# Rust（tonic/prost）：由 node build.rs tonic-build 编译，无需 buf
```

## 双监听端口

- `:7443` mTLS gRPC — 节点 `NodeControl`（RequireAndVerifyClientCert）
- `:18080` TLS+bearer REST — `ControllerAdmin`（auractl/agent）
