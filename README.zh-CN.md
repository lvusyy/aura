# AURA — Agent-User Remote Automation

[English](README.md) | **简体中文**

**给 AI coding agent 一双真实的眼睛和手。**

AURA 是一套自托管基础设施,让 coding agent(Claude Code、Codex CLI、Gemini CLI 等)通过 [MCP](https://modelcontextprotocol.io) 远程操控真实/虚拟测试机——截图 → 点击 → 输入 → 读取 → 验证——发现单元测试和代码审查在结构上无法覆盖的 UI、交互与体验类 bug。

```
┌──────────────┐   MCP (stdio / Streamable HTTP)   ┌─────────────────────────┐
│ Coding agent │ ────────────────────────────────► │ aura-node (Rust, 1 bin) │
│ Claude Code  │                                   │ screenshot / input      │
│ Codex / …    │        direct, zero-infra         │ a11y tree / assert      │
└──────┬───────┘                                   │ files / process / rec   │
       │ REST / console                            └───────────┬─────────────┘
       ▼                                             mTLS gRPC │ reverse conn
┌─────────────────────────────────────────────┐                │ (NAT-friendly)
│ aura-controller (Go, HA-ready)              │ ◄──────────────┘
│ scheduler · fleet registry · enrollment     │
│ recordings · traces · web console (embedded)│   + PostgreSQL / Redis / MinIO
└─────────────────────────────────────────────┘
```

## 为什么需要 AURA

coding agent 很会写代码、跑测试,但「这个应用对人类真的可用吗」活在屏幕的另一侧:布局错乱、点不动的按钮、焦点陷阱、窗口缩放后流程断裂。AURA 补上这个闭环——给 agent 一个**用户视角的测试席位**:一台它能看见、能操作的真实桌面或移动环境,并配以结构化验证(accessibility 树 + 断言),而不是靠猜。

## 亮点

- **节点单二进制。** `aura-node`(Rust)内嵌 MCP server,双传输并存:`stdio` 供本地子进程拉起,无状态 **Streamable HTTP**(`/mcp`)供远程 agent 直连。零运行时依赖。
- **21 个工具。** 覆盖截图、鼠标/键盘/文本注入、accessibility 树读取、断言(text / a11y / image)、文件上传下载、进程与命令控制、录屏、音频——按平台在调用时动态广告。
- **5 个平台。** Windows、Linux(X11)、macOS 真机;Android 经 [Redroid](https://github.com/remote-android/redroid-doc) on Kubernetes;iOS 模拟器经 WebDriverAgent。统一能力契约,按平台裁剪子集。
- **坐标对 agent 准确。** 截图按 XGA 缩放交付 + 点击坐标回映射,遵循 Anthropic computer-use 最佳实践——这是决定 agent 点击能否落准的关键细节。
- **两种部署形态。**
  - *直连:* agent 指向单节点 `/mcp`,几分钟开始测试。
  - *集群:* `aura-controller`(Go)提供舰队管理、调度、环境置备(Proxmox VE / Kubernetes)、产物与录屏存储、trace 回放、Web 管理台——节点**主动外连**控制面(mTLS gRPC 反向长连接),NAT 后的测试机开箱即用。
- **舰队运维。** 一键设备接入(CSR → per-node 证书,私钥不出节点)、舰队面板、任务历史、录屏流式回放、控制面 HA 双副本、Prometheus 指标 + OpenTelemetry 追踪。
- **直连观测。** 节点把直连 agent 的每次 MCP 调用回报控制面,管理台可见谁在测什么,按 agent 客户端聚合,并内置各家接入指引。
- **访问控制可伸缩。** 单节点默认开放接入(实验室零摩擦);设 `AURA_MCP_TOKEN` 即要求 `/mcp` 携带 bearer token。控制面 REST 有三档令牌(admin / ops / 只读)。详见 `controller/deploy/ENV.md`。

## 支持的 coding agent

各家经核实的配置指引已内置于 Web 管理台(接入观测页):

| Agent | 接入方式 |
|---|---|
| Claude Code | `claude mcp add --transport http aura http://<node>:7100/mcp` |
| Codex CLI | `codex mcp add aura --url http://<node>:7100/mcp` |
| Gemini CLI | `settings.json` → `"type": "http"` |
| OpenCode | `opencode.json` → `"type": "remote"` |
| OpenClaw | `openclaw.json` → `"transport": "streamable-http"` |
| Cline | `"type": "streamableHttp"`(≥ 3.17.14) |
| Hermes Agent | `~/.hermes/config.yaml` → `mcp_servers` |
| CodeBuddy | `codebuddy mcp add-json …` |
| Kimi Code | `kimi mcp add -t http …` / `/mcp-config` |
| Grok Build | `.grok/config.toml` → `[mcp_servers.aura]` |
| Pi | 官方无 MCP → 走 `auractl` CLI |

其他任何支持 Streamable HTTP(或拉起 stdio server)的 MCP 客户端同样适用。

## 快速上手——单节点 5 分钟

**方式一:下载预编译二进制(推荐)**

从 [Releases](https://github.com/lvusyy/aura/releases) 下载对应平台的 `aura-node`(Windows x64 / Linux x64 / macOS arm64),解压即用,跳到第 2 步。

**方式二:源码构建**(需 Rust ≥ 1.95 与 `protoc`)

```bash
cd node
# feature flags 很重要——编入反连/设备接入/OTel 面;裸构建只有 stdio/http
cargo build --release -p aura-node --features grpc,enroll,otel
```

**接下来:**

```bash
# 2. 在测试机上以 Streamable HTTP 提供 MCP 服务
./aura-node http --bind 0.0.0.0:7100

# 3. 在工作机把 agent 接上来,以 Claude Code 为例:
claude mcp add --transport http aura http://<测试机>:7100/mcp

# …或者作为本地 stdio 子进程运行(不开端口):
claude mcp add aura -- /path/to/aura-node stdio
```

然后让 agent 用 `screenshot`、点击、输入、`assert` 走查你的应用。

可选访问令牌:节点环境设 `AURA_MCP_TOKEN=<secret>` 启动,agent 侧加 `--header "Authorization: Bearer <secret>"`(或对应客户端的等价配置)。超出可信实验室网段时,请用反向代理给 `/mcp` 加 TLS,或将其保持在私有网络/VPN 内。

## 集群部署

1. **底座服务** — `controller/deploy/compose.yml` 拉起 PostgreSQL、Redis、MinIO(记得改占位口令)。
2. **证书** — `controller/deploy/gen-certs.sh` 生成 mTLS gRPC 面的 CA 与 server 证书(修改 `CTRL_IP`)。
3. **控制面** — `cd controller && go build ./cmd/aura-controller`(或直接用 Releases 里的 Linux 预编译二进制)。全部配置走环境变量,权威清单见 [`controller/deploy/ENV.md`](controller/deploy/ENV.md)。
4. **管理台** — `cd console && npm install && npm run generate && npm run build`,然后重新构建控制面(产物经 `go:embed` 内嵌;预编译二进制已内嵌)。
5. **节点** — 用 `controller/deploy/install/install.sh`(Linux/macOS)或 `install.ps1`(Windows)安装,或手动接入:`aura-node enroll` 完成 CSR 设备接入,之后节点持 per-node 证书反连控制面。管理台的设备接入页会为你生成一键安装命令。

可选组件的参考清单在 `controller/deploy/` 下:Redroid Android 环境(`redroid/`)、Selkies WebRTC 容器桌面(`selkies/`)、coturn(`turn/`)、基于 OmniParser 的视觉检测服务(`detector/`,用视觉检出的 UI 元素增强 accessibility 树)。

## 仓库布局

```
node/        Rust workspace — aura-node 二进制、平台驱动、能力契约
controller/  Go 控制面 — gateway/scheduler/registry/storage + 内嵌管理台
console/     Web 管理台源码(React + Ant Design + Vite;构建产物落入 controller)
proto/       唯一 proto 源(buf;Go/TS 生成物已入库,Rust 绑定由 tonic-build 构建期生成)
```

修改 `proto/aura/v1/*.proto` 后重新生成协议代码:`cd proto && buf generate`(用 BSR remote plugins,版本锁定在 `buf.gen.yaml`)。

## 平台注意事项

- **Windows** — `aura-node` 须运行在交互式登录会话(不能作为服务):session 0 里截图/注入 API 不可用。录屏走 Windows Graphics Capture。
- **Linux** — X11 是受支持的默认(Wayland 有意标记 experimental:portal 授权弹窗与 compositor 碎片化)。无头机器配 Xvfb 或虚拟显示器工作良好。
- **macOS** — 给二进制授予屏幕录制 + 辅助功能(TCC)权限;录屏与注入受这两项权限门控。
- **Android** — Kubernetes 上的 Redroid 容器 + `aura-node` sidecar(`controller/deploy/redroid/`);输入/采集经 adb。
- **iOS** — 经 WebDriverAgent 驱动模拟器;录屏有意排除在 iOS 能力子集之外。

## 一段话说清访问模型

节点的本职是操控**可丢弃的测试机,而非生产主机**——请把每个节点当作任意代码执行面来隔离(VM、VLAN、防火墙)。控制面 ↔ 节点走 per-node 证书 mTLS。REST/管理台面要求 bearer token(三档)。节点直连面(`/mcp`)默认开放以保实验室易用性,设 `AURA_MCP_TOKEN` 即门控;涉敏场景请置于 TLS 或私有网络之后。集群形态下所有派发的工具调用均有审计日志。

## 项目状态

活跃开发中,已在一个真实混合舰队(Windows / Linux / macOS / Android / iOS 模拟器)上持续使用。API 仍可能调整;proto 契约有版本管理,迄今变更均为 additive。欢迎 Issue 与 PR。

## 双语文档

`README.md`(英文)与 `README.zh-CN.md`(本文)互为镜像,**修改任一必须同步另一份**(CI 会检查)。

## License

见 [LICENSE](LICENSE)(Apache-2.0)。
