# Changelog

All notable changes to this project are documented in this file.
The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project adheres to [Semantic Versioning](https://semver.org/).

本文件记录项目的重要变更,格式遵循 Keep a Changelog,版本号遵循语义化版本。

## [0.4.1] - 2026-11-06

Productization, multi-project isolation closure, and hybrid semantic UI automation release.

### Added

- **Hybrid Semantic UI Automation (`locate_element` & `click_element`)** — 3-tier element localization and one-step interaction exposed across Connect-RPC, the MCP gateway (`tools/list` & `tools/call` controller augmentation), `auractl locate` / `auractl click-element`, and the Web Console: Tier 0 deterministic A11y tree matching (< 15 ms) → Tier 1 local k3s GPU YOLO/OmniParser → Tier 2 Jev System-1 (`Noul` existence probability / `Choice` candidate ranking / `Box` bounding-box grounding), with optional `--wait-ms` polling and L1 (A11y tree diff) + L2 (8×8 dHash visual diff) post-click state verification.
- **混合语义 UI 自动化 (`locate_element` 与 `click_element`)**:在 Connect-RPC、MCP 网关(`tools/list` 与 `tools/call` 控制面增强)、`auractl locate` / `auractl click-element` 及 Web 管理台全链路提供三级渐进定位与一步交互:Tier 0 原生 A11y 树确定性匹配(< 15 ms)→ Tier 1 本地 k3s GPU YOLO/OmniParser → Tier 2 Jev System-1(`Noul` 存在概率 / `Choice` 候选排序 / `Box` 坐标定位),支持 `--wait-ms` 自适应等待及 L1(A11y 树 diff)+ L2(8×8 dHash 像素 diff)点击生效校验。
- **Web Console Operate & Fleet UX upgrades** — interactive **A11y Tree Inspector** drawer with depth/keyword filtering and one-click canvas highlight/click, real-time display-coordinate HUD on canvas hover, double-click/right-click/quick-key pass-through, one-click `auractl` CLI command copy, and project/IP/OS filtering on the Fleet page; `auractl a11y <node-id>` CLI shortcut for inspecting accessibility trees.
- **Web 管理台与 CLI 易用性升级**:操作台新增交互式 **A11y 无障碍树检视器**抽屉(支持深度/关键词过滤与一键画布高亮/点击)、画布鼠标悬停实时坐标 HUD、双击/右键/快捷按键透传、一键复制 `auractl` CLI 命令;设备墙新增按项目/IP/系统版本筛选;`auractl` 新增 `a11y <node-id>` 快捷检视命令。

### Fixed

- **Multi-project isolation (`M15`) & RBAC closure** — online node `project` bindings now persist across `UpsertNode RETURNING`, `NodeSession`, `ListFleet`, and `UpdateNodeMeta`; RBAC scope guards (`ro` / `admin`) and project-access checks now cover `UpdateNodeMeta`, `DeleteNode`, `RevokeNodeCert`, `RunOrchestration` (with HA cluster-wide online node resolution), `ListTasks`, `GetTask`, `ListTraces`, `GetTrace`, `ListRecordings`, `WatchFleet` recording states, `GetDashboard`, `GetQueueDepth`, `GetArtifact`, and `/stream/tango`.
- **多项目隔离(`M15`)与 RBAC 权限收口**:修复在线节点 `project` 归属在 `UpsertNode`、`NodeSession`、`ListFleet` 与 `UpdateNodeMeta` 中的透传与保持;补齐 `UpdateNodeMeta`、`DeleteNode`、`RevokeNodeCert`、`RunOrchestration`(含 HA 跨副本在线节点解析)、`ListTasks`、`GetTask`、`ListTraces`、`GetTrace`、`ListRecordings`、`WatchFleet` 录制态、`GetDashboard`、`GetQueueDepth`、`GetArtifact` 与 `/stream/tango` 的令牌档位及项目视界校验。
- **Node & HA runtime hardening** — `enroll`/`renew` blocking HTTP runs inside `spawn_blocking` with atomic `.tmp` + rename credential persistence (`node.key` written only after server acceptance); input worker skips timed-out commands via `reply.is_closed()` and guarantees mouse button release on `Drag` error; Windows UIA `get_a11y_tree` propagates root errors and marks `truncated=true` on child walk errors; `self_update` guards `commit_pending` against concurrent rollouts and writes `pending.json` immediately before restart; `stop_recording` preserves the session on transient error; MinIO upload timeout covers response body drain; iOS WDA clears session ID conditionally (`clear_sid_if`); controller `ReapOnce`/`IsActiveInCluster` fail safe when Redis is unreachable; Console canvas guards against out-of-order async frame decode on node switch.
- **节点端与 HA 运行时稳健性加固**:`enroll`/`renew` 阻塞 HTTP 迁入 `spawn_blocking` 并采用 `.tmp` + rename 原子落盘(服务端签发成功后才写 `node.key`);输入工作线程通过 `reply.is_closed()` 丢弃已超时指令并在 `Drag` 异常时兜底释放鼠标按键;Windows UIA `get_a11y_tree` 传播根节点错误并在子节点遍历失败时标记 `truncated=true`;`self_update` 为 `commit_pending` 加 `IN_FLIGHT` 互斥且在重启前夕才写 `pending.json`;`stop_recording` 在瞬态错误时保留会话、MinIO 上传超时覆盖响应体读取、iOS WDA 条件清理失效 `sid`;控制面 `ReapOnce`/`IsActiveInCluster` 在 Redis 瞬断时保守免驱逐;前端画布修复切换节点时的异步解码乱序覆写。

## [0.4.0] - 2026-08-11

Reliability release: make "online but silently broken" impossible to miss, and make fleet rollouts safe to automate.

### Added

- **`aura-node doctor`** — one-shot capability self-check. Real-world e2e runs showed a node can stay "online" while a core capability is silently broken (Windows WGC capture failure after a lock/unlock cycle, macOS TCC permission loss, locked-session black frames) — agents can't tell, and a human had to ground-truth screenshots to find out. `doctor` probes each capability for real: display enumeration, screenshot capture with near-uniform-frame detection (catches locked/blank/no-signal screens), a11y tree read, a side-effect-free input injection probe (a single `shift` key, exercising the enigo/adb path for real), a 100 ms silent-WAV audio probe, and process listing. Results are graded `ok` / `degraded` (advertised but broken — the F1-class signal) / `warn` / `skipped` (platform matrix expectation), printed human-readable or `--json`, with exit code 1 when anything core is degraded — usable directly in scripts and CI, and as a first-contact troubleshooting tool for new deployments.
- **`aura-node doctor`**:一次性能力自检。真机 e2e 实证过节点「在线」但核心能力静默故障(WGC 截图故障、mac TCC 权限被收、锁屏黑帧),agent 无从分辨。doctor 逐项实测:显示器枚举、截图+近单色帧检测(抓锁屏/黑屏/无信号)、无障碍取树、无副作用输入探针(单发 shift,真实走注入通路)、100ms 静音 WAV 音频探针、进程枚举;分级 ok/degraded/warn/skipped,人类可读或 `--json`,存在 degraded 时退出码 1(脚本/CI 可判),也是新部署的首诊排障工具。
- **Fleet capability health** — nodes now run a non-intrusive probe subset (display / screenshot / a11y — never input or audio) on a period (`AURA_HEALTH_PROBE_INTERVAL_SECS`, default 300 s, `0` disables) and piggyback the latest results on heartbeats. The console fleet page shows an aggregate health tag per node (red "capability degraded N" / gold "notice" / green "healthy") with a per-probe diagnostic popover. A locked-screen or broken-capture node is now visibly unhealthy in the fleet view within a heartbeat cycle instead of masquerading as "online". Session-only (not persisted; offline nodes show nothing), fully backward compatible — old nodes simply don't report.
- **舰队能力健康巡检**:节点周期(缺省 300s,可调/可禁)跑非侵入探针子集(显示器/截图/无障碍,绝不含输入注入与音频),结果随心跳捎带上报;console 节点卡片显示聚合健康徽章(红「能力异常」/黄「提示」/绿「正常」)+逐项诊断 Popover。锁屏/截图故障节点在一个心跳周期内即在舰队视图现形,不再伪装「在线」。仅会话态不入库,未滚更节点不上报,完全向后兼容。
- **Self-update startup health rollback** (closes the M16 Phase 3 debt) — a swap succeeding never meant the new version was healthy: a bad release could crash-loop or come up but never reconnect, leaving the node stranded until someone SSH'd in (painful on macOS re-sign / container rebuild paths). The node now writes a pending marker at swap time; the new binary must complete a controller registration within a trial window (`AURA_SELF_UPDATE_TRIAL_SECS`, default 300 s) to commit. Crash loops (> 3 startup attempts) or a trial timeout roll back to the kept `.old` binary automatically, preserve the bad binary as `.failed` for forensics, record `rollback.json`, and restart — the node then re-registers on its previous version, which the existing rollout convergence check surfaces as a failed release. Manual binary swaps are unaffected (no marker, no trial).
- **self-update 启动健康回滚**(闭合 M16 Phase3 债):换刀成功≠新版本健康——坏版本可能崩溃循环或起而不连,节点搁浅只能人工 SSH 修(mac 重签/容器路径尤痛)。现换刀时写 pending 标记,新二进制须在试用窗(缺省 300s,可调)内完成注册才 commit;崩溃循环(启动尝试>3)或试用窗超时自动回滚 `.old`,坏二进制留 `.failed` 取证、落 `rollback.json` 记录并重启,节点以旧版本重新注册,既有 rollout 收敛检查即可看出发布失败。手动换二进制不受影响(无标记即无试用)。
- e2e: two new scenarios — capability-health heartbeat reporting, and startup-health rollback (trial-window expiry → `.old` restored, forensics kept).
- e2e 新增两场景:能力健康心跳上报;启动健康回滚(试用窗超时→回滚 .old+取证落盘)。

### Fixed

- Workspace test compile: the android/ios assert unit tests missed the 0.3.2 `AssertA11yQuery` migration and no longer compiled under `cargo test --workspace` (CI runs `-p aura-node`, which skips other crates' test code — the gap surfaced while validating this release).
- workspace 测试编译:android/ios 的 assert 单测漏掉了 0.3.2 的 `AssertA11yQuery` 迁移,`cargo test --workspace` 口径下编译失败(CI 只测 `-p aura-node`,不编译其他 crate 的测试代码;本次发布验证时暴露)。

## [0.3.2] - 2026-07-27

### Fixed

- **Windows screen capture robustness**: on a headless machine whose interactive session lost its display surface (e.g. the remote client disconnected), WGC capture failed permanently with `ItemConvertFailed` and a node process restart did not recover it. The node now detects that failure and self-heals by reattaching its own session to the console — where a persistent virtual display can live — then retries the capture once (throttled to 30 s). `list_displays` no longer collapses when a single monitor carries a non-standard device name: per-monitor `index`/`name` resolution degrades gracefully and unreadable monitors are skipped instead of failing the whole enumeration (previously `invalid digit found in string`).
- Windows 无头机截图健壮性:交互会话失去显示面(如远程连接断开)后 WGC 永久失败、进程重启亦不自愈——节点现检测该故障并自愈(把自身会话接管到 console,配合常驻虚拟显示器)后重试一次;`list_displays` 不再因单个显示器设备名非标准而整体失败。
- **`assert(mode=a11y)` false negatives**: the accessibility lookup reused the same shallow defaults as `get_a11y_tree` (depth 3 / 200 nodes), so a control deeper in the tree — e.g. a browser address bar `EditText` at depth 5 — reported `found=false` and misled agents into concluding their input had not landed. Assert now traverses deeply by default (depth 32 / 5000 nodes): the shallow default exists to keep a whole tree from flooding the agent's context, and assert returns only a verdict plus the matched node, so that constraint does not apply there (`get_a11y_tree` itself is unchanged). When a lookup misses **and** the tree was truncated, the result now says the search was incomplete instead of reporting a bare `found=false`.
- `assert(mode=a11y)` 假阴性:取树沿用了 get_a11y_tree 的浅层缺省(深度 3),深层控件(如浏览器地址栏)被判"不存在",agent 据此误判输入未生效。assert 改为缺省深遍历(浅层是为「整树回 agent」防上下文树爆而设,assert 只回判定+命中节点,该约束在此不成立;get_a11y_tree 自身不变);未命中且树被截断时如实告知搜索不完整,不再静默。
- `run_command` output on non-UTF-8 Windows consoles is now decoded with the console code page (GBK on Chinese Windows) instead of being mangled by a lossy UTF-8 read. Note that a short GBK sequence that happens to be valid UTF-8 can still decode incorrectly — automatic detection has no reliable answer for arbitrary child processes.
- Windows 中文控制台下 `run_command` 输出不再乱码(按控制台代码页解码)。注意:恰好构成合法 UTF-8 的短 GBK 序列仍可能解错——对任意子进程的编码,自动探测没有可靠解。
- **`assert(mode=a11y)` verdict soundness** (both found in pre-release review): supplying only *part* of the query object (e.g. `{"root":"focus"}`) used to reset every unspecified field to the shallow `get_a11y_tree` defaults, silently reintroducing the deep-node false negative — the deep defaults now apply per field. And a miss on a **truncated** tree is now reported as `passed=false` and flagged `INCONCLUSIVE`: a negative assertion (`present=false`) can no longer "pass" on the strength of a search that never finished, since absence cannot be proven from an incomplete tree.
- `assert(mode=a11y)` 判定收紧(两处均由发布前审查发现):只给**部分** query 字段(如 `{"root":"focus"}`)时,未指定的字段会退回浅层缺省、深层节点假阴性复发——缺省改为逐字段生效;**截断**树上未命中现返回 `passed=false` 并标记 `INCONCLUSIVE`——反向断言(`present=false`)不再靠「没搜到」判通过,不完整的搜索无法证明不存在。

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

[0.4.1]: https://github.com/lvusyy/aura/releases/tag/v0.4.1
[0.4.0]: https://github.com/lvusyy/aura/releases/tag/v0.4.0
[0.3.2]: https://github.com/lvusyy/aura/releases/tag/v0.3.2
[0.3.0]: https://github.com/lvusyy/aura/releases/tag/v0.3.0
[0.2.0]: https://github.com/lvusyy/aura/releases/tag/v0.2.0
[0.1.0]: https://github.com/lvusyy/aura/releases/tag/v0.1.0
