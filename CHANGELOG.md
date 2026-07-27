# Changelog

All notable changes to this project are documented in this file.
The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project adheres to [Semantic Versioning](https://semver.org/).

本文件记录项目的重要变更,格式遵循 Keep a Changelog,版本号遵循语义化版本。

## [0.3.1] - 2026-07-27

### Fixed

- **Windows screen capture robustness**: on a headless machine whose interactive session lost its display surface (e.g. the remote client disconnected), WGC capture failed permanently with `ItemConvertFailed` and a node process restart did not recover it. The node now detects that failure and self-heals by reattaching its own session to the console — where a persistent virtual display can live — then retries the capture once (throttled to 30 s). `list_displays` no longer collapses when a single monitor carries a non-standard device name: per-monitor `index`/`name` resolution degrades gracefully and unreadable monitors are skipped instead of failing the whole enumeration (previously `invalid digit found in string`).
- Windows 无头机截图健壮性:交互会话失去显示面(如远程连接断开)后 WGC 永久失败、进程重启亦不自愈——节点现检测该故障并自愈(把自身会话接管到 console,配合常驻虚拟显示器)后重试一次;`list_displays` 不再因单个显示器设备名非标准而整体失败。
- **`assert(mode=a11y)` false negatives**: the accessibility lookup reused the same shallow defaults as `get_a11y_tree` (depth 3 / 200 nodes), so a control deeper in the tree — e.g. a browser address bar `EditText` at depth 5 — reported `found=false` and misled agents into concluding their input had not landed. Assert now traverses deeply by default (depth 32 / 5000 nodes): the shallow default exists to keep a whole tree from flooding the agent's context, and assert returns only a verdict plus the matched node, so that constraint does not apply there (`get_a11y_tree` itself is unchanged). When a lookup misses **and** the tree was truncated, the result now says the search was incomplete instead of reporting a bare `found=false`.
- `assert(mode=a11y)` 假阴性:取树沿用了 get_a11y_tree 的浅层缺省(深度 3),深层控件(如浏览器地址栏)被判"不存在",agent 据此误判输入未生效。assert 改为缺省深遍历(浅层是为「整树回 agent」防上下文树爆而设,assert 只回判定+命中节点,该约束在此不成立;get_a11y_tree 自身不变);未命中且树被截断时如实告知搜索不完整,不再静默。
- `run_command` output on non-UTF-8 Windows consoles is now decoded with the console code page (GBK on Chinese Windows) instead of being mangled by a lossy UTF-8 read.
- Windows 中文控制台下 `run_command` 输出不再乱码(按控制台代码页解码)。

## [0.3.0] - 2026-07-21

### Added

- **Node self-update & fleet rollout**: upload per-platform release artifacts to the controller (`POST /v1/releases`, admin bearer, MinIO-backed with server-side sha256 registration; `auractl release upload/list`), then roll the fleet with `auractl rollout --version V --all|--nodes …` — serial canary-first, halting on the first failure. The node downloads over a zone-aware presigned URL, enforces sha256, sanity-probes the staged binary (`--version`), swaps atomically in the install directory (an `.old` self-heal slot is kept; any pre-swap failure leaves the running binary untouched), reports the result and restarts in place — Unix `exec` self-replacement covers systemd/launchd/container-PID1/bare processes, Windows re-launches through the node's scheduled task. Nodes self-report `host_platform` (`{os}-{arch}`) so artifacts match the binary's actual host (an Android node's binary runs on a linux-x86_64 host); the console fleet page shows an "update available" drift tag per node. `install.sh` now places the binary inside the data dir (writable by the node user — required for the atomic swap) with a `/usr/local/bin` symlink for manual CLI use.
- 节点 **self-update 舰队滚更**:制品上传控制面登记(`auractl release upload`),`auractl rollout` 串行金丝雀逐台滚更、失败即停;节点侧 sha256 强校验+sanity 探针+原子换刀(.old 自愈位,换刀前失败不动现网二进制)+原地重启,覆盖 systemd/launchd/容器 PID1/裸进程/Windows 计划任务全形态;console 节点卡片显示「可更新」版本漂移标签。
- **Access tokens & per-project isolation**: API tokens are first-class DB entities — created from a new console page, sha256-hashed at rest, plaintext shown exactly once, each carrying a scope tier (admin/ops/read-only) and an optional project binding. Nodes carry a `project` attribute (assignable at enrollment); a project-bound token sees and controls only its project's nodes across dispatch, MCP gateway, fleet views and node addressing. Env-token deployments keep working unchanged.
- **访问令牌实体化+多租户项目隔离**:令牌入库(哈希存储、明文一次性展示)、绑定档位与项目视界,派发/网关/舰队视图/节点寻址全面按项目隔离;纯 env 令牌部署行为不变。
- **Graceful node shutdown**: SIGTERM / console control events finalize and upload in-flight recordings before exit — container PID1 and service stops included.
- **`aura-node service status|restart`** subcommands (systemd / launchd / Windows scheduled task aware).
- **`run_command` `detach` mode**: launch GUI apps that must outlive the tool call (previously reaped by the 30 s kill-on-drop guard).
- **End-to-end test suite** in CI: a real controller + node + MinIO exercising enroll → dispatch → gateway → project isolation → UI loop → self-update.
- Console: OS-specific platform icons (Windows/Linux/macOS/K8s) on the fleet page.
- Hardening: MCP gateway per-node in-flight gate; CI gained a Rust build/test job.

### Fixed

- Adversarial review round on the self-update path: the Windows relauncher now recognizes `cmd`-wrapped scheduled tasks (the exact form `install.ps1` registers — the executable only appears in the task's arguments), waits for the task to leave `Running` before starting it, and verifies the node process actually appeared before using the direct-launch fallback (prevents duplicate instances); arguments containing spaces are pre-quoted for the fallback. The release download leg uses a single umbrella timeout (header and body previously timed out independently, worst case exceeding the controller's wait window and splitting CLI-reported state from node reality). Late `SelfUpdateResult` frames are matched by version echo so a stale reply cannot resolve the wrong request.
- `aura-node service status` no longer breaks on localized Windows (parses PowerShell `State` instead of locale-dependent text); e2e harness temp-dir leak fixed; detached `run_command` children are explicitly reaped.

## [0.2.0] - 2026-07-19

### Added

- **MCP gateway** on the controller (`POST /v1/mcp/<node-id>`, TLS + admin-scope bearer): coding agents now reach any internal node's MCP surface through the controller as the single exposed entry point. Raw JSON-RPC is forwarded over the node's outbound mTLS reverse stream to its local MCP server — protocol semantics are byte-identical to direct access, and test machines need no inbound exposure at all (bind the node's `/mcp` to loopback). HA-aware (requests forward to the node's owner replica); gateway calls are audit-logged and appear in the agent observability page; the console's Agents page shows a copy-ready gateway URL per node. Direct node access remains as a same-segment lab shortcut.
- 控制面新增 **MCP 网关**(`/v1/mcp/<节点ID>`):agent 经控制面单一入口访问内网节点,测试机零暴露;协议语义与直连字节级一致;HA 感知、有审计与观测;管理台逐节点给出可复制网关 URL。

### Fixed

- Node loopback proxy hardening (adversarial review round): the proxied `/mcp` response body is capped at 12 MB — below the 16 MB reverse-stream frame limit — so oversized responses fail loud with 502 instead of breaking the node's whole reverse connection; the 120 s proxy timeout now covers body reads as well, so a hung or never-ending response can no longer leak node-side tasks.
- Gateway: the HA cross-replica forwarding leg now honors the same 150 s end-to-end timeout as the local path, and audit records are emitted for every outcome (403/404/405/413/forward failures included).
- Node loopback address derivation: binding `[::]` now proxies via the IPv6 loopback, and binding a specific LAN address proxies to that address (previously always `127.0.0.1`, which broke the gateway for non-loopback binds).
- CORS: `Mcp-Protocol-Version` added to allowed headers for cross-origin browser MCP clients.

## [0.1.0] - 2026-07-18

First public release. 首次公开发布。

### Added

- **`aura-node`** (Rust, single static binary): embedded MCP server with dual transports — `stdio` and stateless Streamable HTTP (`/mcp`); 21 tools covering screenshot, mouse/keyboard/text injection, accessibility-tree read, assertions (text/a11y/image), file push/pull, process & command control, screen recording and audio; per-platform capability advertisement at call time; XGA screenshot scaling with click-coordinate back-mapping.
- **Platform drivers**: Windows, Linux (X11), macOS, Android (Redroid via adb), iOS Simulator (WebDriverAgent) — one capability contract, per-platform subsets.
- **`aura-controller`** (Go): fleet registry, scheduler, task dispatch; nodes reverse-connect over mTLS gRPC (NAT-friendly); environment provisioning for Proxmox VE and Kubernetes; PostgreSQL/Redis/MinIO backing stores with graceful in-memory degradation; audit logging; Prometheus metrics + OpenTelemetry tracing; HA dual-replica with owner routing and cross-replica forwarding.
- **Device onboarding**: one-command enrollment — CSR-based per-node certificates, private keys never leave the node; certificate renewal and revocation; 30-day offline node reaping.
- **Recording & replay**: screen recording with streaming playback (HTTP Range), MinIO lifecycle retention; trace capture and `auractl replay`.
- **Visual fusion**: optional OmniParser-based detector service augmenting the accessibility tree with vision-detected UI elements.
- **Web console** (React + Ant Design, embedded via `go:embed`): fleet dashboard with infrastructure labels, task history, recording playback, orchestration wall, live desktop entry (Selkies WebRTC), device onboarding page.
- **Direct-access observability**: per-call MCP activity of directly-connected agents reported back to the controller; console page with per-agent-client breakdowns and verified access guides for 11 coding agents (Claude Code, Codex CLI, Gemini CLI, OpenCode, OpenClaw, Cline, Hermes, CodeBuddy, Kimi Code, Grok Build, Pi via `auractl`).
- **Access control**: three-tier bearer tokens on the REST plane (admin/ops/read-only); optional `AURA_MCP_TOKEN` gate on the node `/mcp` endpoint (open by default for lab use).
- **`auractl`** CLI: task dispatch, environment lifecycle, artifact fetch, trace replay against the controller REST plane.
- Prebuilt binaries: `aura-node` (Windows x64 / Linux x64 / macOS arm64), `aura-controller` + `auractl` (Linux x64, console embedded), `auractl` (Windows x64).

[0.3.1]: https://github.com/lvusyy/aura/releases/tag/v0.3.1
[0.3.0]: https://github.com/lvusyy/aura/releases/tag/v0.3.0
[0.2.0]: https://github.com/lvusyy/aura/releases/tag/v0.2.0
[0.1.0]: https://github.com/lvusyy/aura/releases/tag/v0.1.0
