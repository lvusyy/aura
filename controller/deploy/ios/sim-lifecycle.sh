#!/usr/bin/env bash
# AURA M7 — iOS 模拟器设备生命周期（simctl 编排，harness 供给层）。
# 一律以 UDID 引用设备，禁用 'booted' 别名（并发下非确定性）；boot 走
# `bootstatus -b` 阻塞至就绪，禁裸 `boot`（非阻塞、fastlane 因崩溃已弃用）。
# 可被 source（仅定义函数，无副作用）或直接执行（子命令派发）。
#
# 子命令（直接执行）:
#   create <name> <devicetype> <runtime>  幂等创建，UDID 写 stdout
#   boot <udid>                           bootstatus -b 阻塞至就绪
#   erase <udid>                          关机后出厂重置
#   reset <udid>                          erase + boot（设备侧基线复位；全链 S0 再叠加 wda-launch）
#   shutdown <udid>                       关机（teardown：保留设备与已装 runner，绝不删除）
#   status <udid>                         打印设备状态（Booted/Shutdown/...）
#
# 退出码: 0=成功 1=用法错误 2=前置缺失(xcrun)
set -euo pipefail

log() { echo "[sim-lifecycle] $*" >&2; }
die() { code=$1; shift; log "ERROR: $*"; exit "$code"; }

require_udid() {
  [ -n "${1:-}" ] || die 1 "UDID required（一律显式 UDID，禁 'booted' 别名）"
}

# 设备当前状态：取行尾最后一个括号组（Booted/Shutdown/Booting/...）；不存在返回 NotFound。
sim_state() {
  udid=$1
  line=$(xcrun simctl list devices | grep -F "$udid" | head -1)
  [ -n "$line" ] || { printf '%s\n' "NotFound"; return 0; }
  printf '%s\n' "$line" | sed -E 's/.*\(([^)]*)\)[[:space:]]*$/\1/'
}

# 幂等创建：同名设备已存在则复用其 UDID，否则新建。UDID 写 stdout。
sim_create() {
  name=$1; devicetype=$2; runtime=$3
  existing=$(xcrun simctl list devices | grep -F " $name (" | grep -oE '[0-9A-Fa-f-]{36}' | head -1 || true)
  if [ -n "$existing" ]; then
    log "sim '$name' 已存在，复用 ${existing}（幂等）"
    printf '%s\n' "$existing"
    return 0
  fi
  udid=$(xcrun simctl create "$name" "$devicetype" "$runtime")
  log "已创建 sim '$name': $udid"
  printf '%s\n' "$udid"
}

# 出厂重置（erase 要求先关机）
sim_erase() {
  udid=$1; require_udid "$udid"
  st=$(sim_state "$udid")
  case "$st" in
    Booted|Booting) log "erase 前先关机 ${udid}（当前 ${st}）"; xcrun simctl shutdown "$udid" || true ;;
  esac
  log "erasing ${udid}（出厂重置）"
  xcrun simctl erase "$udid"
}

# boot 至就绪：bootstatus -b 未 boot 则 boot 并阻塞，已 boot 立即返回（幂等）。禁裸 boot。
sim_boot() {
  udid=$1; require_udid "$udid"
  log "booting $udid via 'bootstatus -b'（阻塞至就绪）"
  xcrun simctl bootstatus "$udid" -b
}

# 关机（teardown：保留设备与已装 runner，绝不删除）
sim_shutdown() {
  udid=$1; require_udid "$udid"
  st=$(sim_state "$udid")
  if [ "$st" = "Shutdown" ]; then
    log "sim $udid 已关机（no-op，幂等）"
    return 0
  fi
  log "shutting down ${udid}（保留设备+已装 runner，不删除）"
  xcrun simctl shutdown "$udid"
}

# erase + boot：模拟器侧基线复位（S0 设备侧；WDA 侧由 wda-launch.sh 叠加）
sim_reset() {
  udid=$1; require_udid "$udid"
  sim_erase "$udid"
  sim_boot "$udid"
}

_usage() { sed -n '2,17p' "$0" >&2; }

main() {
  command -v xcrun >/dev/null 2>&1 || die 2 "xcrun 未找到（需 Xcode command line tools）"
  cmd=${1:-}
  case "$cmd" in
    create)   shift; [ $# -ge 3 ] || die 1 "用法: create <name> <devicetype> <runtime>"; sim_create "$1" "$2" "$3" ;;
    boot)     shift; sim_boot "${1:-}" ;;
    erase)    shift; sim_erase "${1:-}" ;;
    reset)    shift; sim_reset "${1:-}" ;;
    shutdown) shift; sim_shutdown "${1:-}" ;;
    status)   shift; require_udid "${1:-}"; sim_state "$1" ;;
    -h|--help|help) _usage; exit 0 ;;
    "") _usage; exit 1 ;;
    *) die 1 "未知子命令: $cmd" ;;
  esac
}

# 仅在直接执行时派发子命令；被 source 时只定义函数（无副作用）
if [ "${BASH_SOURCE[0]}" = "${0}" ]; then
  main "$@"
fi
