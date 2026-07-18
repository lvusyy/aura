#!/usr/bin/env bash
# AURA M7 — WDA 崩溃监督（harness 级；node 零拥有 WDA 进程）。
# 单次探测 /status：健康则 no-op；失败即调 wda-launch.sh 重拉一次。
# 与 M6 引擎“只警示不清场”哲学一致：探测→按需重拉，不做进程清场。
# 供 cron/watchdog 周期调用，或闭环/重签流程崩溃恢复复用同一 wda-launch 函数。
#
# 用法: wda-supervise.sh <UDID> [PORT]     (PORT 默认 8100)
# 退出码: 0=已健康或重拉后就绪 1=用法错误 3=重拉后仍不就绪（透传 wda-launch）
set -euo pipefail

log() { echo "[wda-supervise] $*" >&2; }
die() { code=$1; shift; log "ERROR: $*"; exit "$code"; }

UDID=${1:-${WDA_UDID:-}}
PORT=${2:-${WDA_PORT:-8100}}
STATUS_URL="http://127.0.0.1:${PORT}/status"
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

[ -n "$UDID" ] || die 1 "用法: wda-supervise.sh <UDID> [PORT]（UDID 必填）"

# 单次 /status 探测（HTTP 200 且 state=success）
if out=$(curl -sf "$STATUS_URL" 2>/dev/null) && printf '%s' "$out" | grep -Eq '"state"[[:space:]]*:[[:space:]]*"success"'; then
  log "WDA 健康 on ${STATUS_URL}（no-op）"
  exit 0
fi

log "WDA /status 探测失败 → 调 wda-launch.sh 重拉（$UDID:${PORT}）"
exec bash "$HERE/wda-launch.sh" "$UDID" "$PORT"
