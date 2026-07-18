#!/usr/bin/env bash
# AURA M7 — WDA 快循环拉起（harness 供给；node 零拥有 WDA 进程）。
# 确保设备 booted → install prebuilt runner → SIMCTL_CHILD_USE_PORT 进程 env 拉起
# → 轮询 /status 至就绪。免每次 xcodebuild，被 009 闭环 / 010 S0 / 014 重签复用。
# 幂等：install 覆盖安装；launch 用 --terminate-running-process；重复跑安全。
#
# 用法: wda-launch.sh <UDID> [PORT]      (PORT 默认 8100)
# 环境变量:
#   WDA_DERIVED_DATA    derivedDataPath（默认 $HOME/wda_dd）
#   WDA_RUNNER_APP      prebuilt Runner.app 路径（默认由 DERIVED_DATA 推导）
#   WDA_BUNDLE_ID       runner bundle id（默认 com.facebook.WebDriverAgentRunner.xctrunner）
#   WDA_STATUS_TIMEOUT  /status 就绪轮询上限秒（默认 120，可配）
# 退出码: 0=就绪 1=用法错误 2=前置缺失(xcrun/runner) 3=/status 超时未就绪
set -euo pipefail

log() { echo "[wda-launch] $*" >&2; }
die() { code=$1; shift; log "ERROR: $*"; exit "$code"; }

UDID=${1:-${WDA_UDID:-}}
PORT=${2:-${WDA_PORT:-8100}}
BUNDLE_ID=${WDA_BUNDLE_ID:-com.facebook.WebDriverAgentRunner.xctrunner}
DERIVED_DATA=${WDA_DERIVED_DATA:-$HOME/wda_dd}
RUNNER_APP=${WDA_RUNNER_APP:-$DERIVED_DATA/Build/Products/Debug-iphonesimulator/WebDriverAgentRunner-Runner.app}
STATUS_TIMEOUT=${WDA_STATUS_TIMEOUT:-120}
STATUS_URL="http://127.0.0.1:${PORT}/status"

# /status 就绪判定：curl 命中 HTTP 200（-f 非 200 即失败）且 body 含 state=success。
wda_ready() {
  out=$(curl -sf "$STATUS_URL" 2>/dev/null) || return 1
  printf '%s' "$out" | grep -Eq '"state"[[:space:]]*:[[:space:]]*"success"'
}

[ -n "$UDID" ] || die 1 "用法: wda-launch.sh <UDID> [PORT]（UDID 必填，一律 UDID 禁 'booted'）"
command -v xcrun >/dev/null 2>&1 || die 2 "xcrun 未找到（需 Xcode command line tools）"
[ -d "$RUNNER_APP" ] || die 2 "prebuilt Runner.app 缺失: ${RUNNER_APP}（先跑 wda-build.sh）"

# 前置：确保设备 booted（install/launch 均要求 booted）。bootstatus -b 幂等、禁裸 boot。
log "确保设备就绪: bootstatus -b $UDID"
xcrun simctl bootstatus "$UDID" -b

# 安装 prebuilt runner（幂等覆盖安装）
log "install runner → $UDID"
xcrun simctl install "$UDID" "$RUNNER_APP"

# 每实例端口经 SIMCTL_CHILD_ 进程 env（当 xcodebuild 参数传无效）；--terminate-running-process 保证幂等重拉
log "launch ${BUNDLE_ID}（SIMCTL_CHILD_USE_PORT=${PORT}，--terminate-running-process）"
env SIMCTL_CHILD_USE_PORT="$PORT" \
  xcrun simctl launch --terminate-running-process "$UDID" "$BUNDLE_ID" >&2

# 轮询 /status 至就绪（上限 STATUS_TIMEOUT）
log "轮询 $STATUS_URL 至就绪（上限 ${STATUS_TIMEOUT}s）"
start=$(date +%s)
deadline=$(( start + STATUS_TIMEOUT ))
until wda_ready; do
  now=$(date +%s)
  if [ "$now" -ge "$deadline" ]; then
    die 3 "WDA /status 于 ${STATUS_TIMEOUT}s 内未就绪（${STATUS_URL}）"
  fi
  sleep 2
done
elapsed=$(( $(date +%s) - start ))
log "WDA ready on ${STATUS_URL}（/status 就绪耗时 ${elapsed}s）"
exit 0
