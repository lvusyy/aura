# iOS Simulator + WDA supply (AURA M7)

Harness-layer供给脚本：把 TASK-001 锁定的 simctl 设备生命周期与 WDA 拉起沉淀为可复用
deploy 资产，镜像 M5 `controller/deploy/redroid/` 布局。**node 零拥有 WDA 进程**——
供给 / 监督 / 崩溃重拉全归 harness（与 M6 引擎“只警示不清场”哲学一致）。这些脚本被
**009 闭环**、**010 S0 复位**、**014 周期重签 cron** 三处复用同一函数快循环拉起 WDA。

设计要点：设备一律以 **UDID** 引用，**禁 `booted` 别名**（并发下非确定）；boot 走
`simctl bootstatus <UDID> -b` 阻塞至就绪，**禁裸 `boot`**（非阻塞、fastlane 因崩溃弃用）；
每实例端口经 **`SIMCTL_CHILD_USE_PORT` 进程 env**（当 xcodebuild 参数传无效）。

## Files

| Repo file           | Purpose                                                                 |
|---------------------|-------------------------------------------------------------------------|
| `sim-lifecycle.sh`  | 模拟器设备生命周期：`create`/`boot`/`erase`/`reset`/`shutdown`/`status`（UDID-only，`bootstatus -b`）。可 source（仅定义函数）或直接执行（子命令派发） |
| `wda-build.sh`      | 禁签 `build-for-testing` 封装；一次构建产 prebuilt Runner.app，路径写 stdout（分钟级，仅一次性/014 重签调用） |
| `wda-launch.sh`     | 快循环拉起：ensure-booted → install runner → `SIMCTL_CHILD_USE_PORT` launch → 轮询 `/status` 就绪。免每次 xcodebuild |
| `wda-supervise.sh`  | 崩溃监督：单次探测 `/status`，失败即调 `wda-launch.sh` 重拉一次（harness 级监督） |
| `README.md`         | 本文件：布局 + Aqua 会话前置 + S0 成本账 + 复用图                        |

## Locked environment (TASK-001 gate)

脚本的 env 默认值即锚定这套已验证参数（`evidence/wda-gate.txt`）：

| 项              | 值                                                                                  |
|-----------------|-------------------------------------------------------------------------------------|
| SIMULATOR_NAME  | `aura-ios-m7` (iPhone 11)                                                            |
| SIMULATOR_UDID  | `4103F11E-A2A8-4D28-86CA-6B5D0AC39BE4`                                               |
| RUNTIME         | iOS 26.2 (`com.apple.CoreSimulator.SimRuntime.iOS-26-2`)                             |
| WDA_VERSION     | v15.1.4 (pinned)                                                                     |
| WDA_SRC         | `~/wda`                                                                              |
| DERIVED_DATA    | `~/wda_dd`                                                                           |
| RUNNER_APP      | `~/wda_dd/Build/Products/Debug-iphonesimulator/WebDriverAgentRunner-Runner.app`      |
| BUNDLE_ID       | `com.facebook.WebDriverAgentRunner.xctrunner`                                        |
| WDA_PORT        | `8100`（`SIMCTL_CHILD_USE_PORT`；模拟器共享宿主网络命名空间，宿主 `localhost:8100` 直达，无需转发） |

## ⚠️ Aqua GUI 会话前置 (RISK-8)

**CoreSimulator 需要 Aqua(GUI) launchd 会话。** 纯 SSH 落 Background 域——无人登录控制台时
模拟器 boot 失败。规律：*有 GUI 会话（哪怕锁屏）SSH 驱动无头模拟器可用；全登出则不可用*。

修法（任一）：
- runner 用户**自动登录**（维持 Aqua 会话）— AURA mac 节点采用此路，**最稳**；
- `launchctl asuser <uid> ...` 把 simctl 命令派发进已登录 GUI 会话；
- Screen Sharing 连一次也能建会话。

> **AURA mac 节点进程须运行在（或 asuser 派发进）已登录 GUI 会话**，否则 `sim-lifecycle.sh boot`
> 会挂在 `bootstatus`。TASK-001 与本任务的 SSH 实测均在已登录会话下通过。

## Usage

脚本均 `bash`，路径全双引号。UDID 用 TASK-001 锁定值（或 `~/.aura_ios_m7_udid`）。

```bash
UDID=4103F11E-A2A8-4D28-86CA-6B5D0AC39BE4

# --- provision（首供给 / 幂等重跑）---
bash sim-lifecycle.sh boot "$UDID"     # bootstatus -b 阻塞至就绪（已 boot 即 no-op）
bash wda-launch.sh "$UDID"             # install + launch + /status 就绪；重复跑安全

# --- reset-s0（出厂重置 → 全链重供给到就绪）---
bash sim-lifecycle.sh reset "$UDID"    # erase + boot（设备侧基线）
bash wda-launch.sh "$UDID"             # erase 抹掉 runner → 此步 install 重装 → launch → /status
#   等价一行：bash sim-lifecycle.sh reset "$UDID" && bash wda-launch.sh "$UDID"

# --- supervise（崩溃恢复；cron/watchdog 周期调用）---
bash wda-supervise.sh "$UDID"          # /status 健康 no-op；失败即调 wda-launch 重拉一次

# --- teardown（省内存；保留设备与已装 runner，不删除）---
bash sim-lifecycle.sh shutdown "$UDID"

# --- build（一次性 / 014 周期重签）---
RUNNER=$(bash wda-build.sh "$UDID")    # 禁签构建，prebuilt Runner.app 路径写 stdout
```

### Env knobs

| 变量                 | 默认                                                        | 用途                          |
|----------------------|-------------------------------------------------------------|-------------------------------|
| `WDA_PORT`           | `8100`                                                      | `SIMCTL_CHILD_USE_PORT`（多实例每实例不同端口） |
| `WDA_DERIVED_DATA`   | `$HOME/wda_dd`                                              | derivedDataPath               |
| `WDA_RUNNER_APP`     | `$WDA_DERIVED_DATA/Build/Products/Debug-iphonesimulator/WebDriverAgentRunner-Runner.app` | prebuilt runner 路径 |
| `WDA_BUNDLE_ID`      | `com.facebook.WebDriverAgentRunner.xctrunner`              | runner bundle id              |
| `WDA_STATUS_TIMEOUT` | `120`                                                      | `/status` 就绪轮询上限秒（可配；健康路径实测约 5s 即返回） |
| `WDA_SRC`            | `$HOME/wda`                                                 | WDA 源码目录（wda-build）     |

### Exit codes

| 码 | 语义                                                    |
|----|---------------------------------------------------------|
| 0  | 成功 / 就绪                                             |
| 1  | 用法错误（缺 UDID / 未知子命令）                        |
| 2  | 前置缺失（xcrun/xcodebuild/runner/WDA 工程 不存在）     |
| 3  | `/status` 于 `WDA_STATUS_TIMEOUT` 内未就绪（wda-launch/supervise 透传） |

## S0 全链成本账 (erase → bootstatus → install → launch → /status)

analysis §3.4 预估 **40–70s**。mac 实测 2026-07-10（`aura-ios-m7` iOS 26.2，
Xcode 26.3 / macOS 26.5.1，见 `evidence/TASK-002-reset-s0.log` / `evidence/deploy-scripts.txt`）：

| 阶段                                   | 实测耗时 |
|----------------------------------------|----------|
| `erase`（含关机）                      | 0s（设备已关机；温设备含关机约 +1–3s） |
| `bootstatus -b`（boot 至就绪）         | 29s      |
| `install` runner + `launch`            | ≈8s      |
| `/status` 轮询至就绪                   | 11s（幂等重拉/崩溃恢复仅 2–4s） |
| **reset-s0 全链合计**                  | **48s**（落在 40–70s 预估内） |

> WDA `/status` 健康路径实测 2–11s 即 ready（首次冷拉 11s；幂等重拉 4s；崩溃恢复 2s；
> TASK-001 首拉 t=5s）。`WDA_STATUS_TIMEOUT=120` 为宽松上限（可配），风险缓解的 60s 上限
> 经 `WDA_STATUS_TIMEOUT=60` 即得——健康路径远早于任一上限返回。

## 复用图 (009 / 010 / 014)

| 复用方         | 调用                                                                   |
|----------------|------------------------------------------------------------------------|
| 009 闭环       | `wda-launch.sh` 拉起 WDA；`wda-supervise.sh` 崩溃恢复                    |
| 010 S0 复位    | `sim-lifecycle.sh reset` + `wda-launch.sh`（全链重供给到 `/status` 就绪） |
| 014 周期重签   | `wda-build.sh` 重构建 → `wda-launch.sh` 重装拉起（cron，模拟器免签本无 7 天过期，重签路径为真机预留 / 复用同一 launch 函数） |

## Teardown 边界

`sim-lifecycle.sh shutdown` **只关机、不删除**——设备与已装 runner 均保留，供下次快循环
`install+launch` 复用（`simctl listapps` 在关机设备上返回空是查询在运行服务所致，**非卸载**，
runner bundle 在盘上存活，见 `evidence/wda-gate.txt` POST-SHUTDOWN PERSISTENCE 段）。删除设备
需显式 `xcrun simctl delete <UDID>`，本套脚本不提供（避免误删 TASK-001 产物）。
