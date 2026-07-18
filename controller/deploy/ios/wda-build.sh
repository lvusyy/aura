#!/usr/bin/env bash
# AURA M7 — WDA 禁签构建封装（模拟器免签；一次构建产 prebuilt runner，供快循环复用）。
# 参数化 TASK-001 验证过的 build-for-testing；prebuilt Runner.app 绝对路径写 stdout
# （供 wda-launch.sh 消费）。构建慢（分钟级），仅一次性/014 周期重签时调用。
#
# 用法: wda-build.sh <UDID> [DERIVED_DATA] [WDA_SRC]
#   UDID          -destination "id=<UDID>"（构建目标模拟器）
#   DERIVED_DATA  derivedDataPath（默认 $HOME/wda_dd）
#   WDA_SRC       WDA 源码目录（默认 $HOME/wda，内含 WebDriverAgent.xcodeproj）
# 退出码: 0=成功 1=用法错误 2=前置缺失 3=构建后 Runner.app 缺失
set -euo pipefail

log() { echo "[wda-build] $*" >&2; }
die() { code=$1; shift; log "ERROR: $*"; exit "$code"; }

UDID=${1:-${WDA_UDID:-}}
DERIVED_DATA=${2:-${WDA_DERIVED_DATA:-$HOME/wda_dd}}
WDA_SRC=${3:-${WDA_SRC:-$HOME/wda}}

[ -n "$UDID" ] || die 1 "用法: wda-build.sh <UDID> [DERIVED_DATA] [WDA_SRC]（UDID 必填）"
command -v xcodebuild >/dev/null 2>&1 || die 2 "xcodebuild 未找到（需 Xcode）"
[ -e "$WDA_SRC/WebDriverAgent.xcodeproj" ] || die 2 "WDA 工程缺失: $WDA_SRC/WebDriverAgent.xcodeproj"

log "禁签 build-for-testing → destination id=$UDID, derivedData=$DERIVED_DATA"
# 模拟器无需签名/描述文件：CODE_SIGNING_* 全关。构建日志走 stderr，stdout 只留 Runner.app 路径。
xcodebuild build-for-testing \
  -project "$WDA_SRC/WebDriverAgent.xcodeproj" -scheme WebDriverAgentRunner \
  -destination "id=$UDID" -derivedDataPath "$DERIVED_DATA" \
  CODE_SIGN_IDENTITY="" CODE_SIGNING_REQUIRED=NO CODE_SIGNING_ALLOWED=NO \
  COMPILER_INDEX_STORE_ENABLE=NO GCC_TREAT_WARNINGS_AS_ERRORS=0 >&2

RUNNER_APP="$DERIVED_DATA/Build/Products/Debug-iphonesimulator/WebDriverAgentRunner-Runner.app"
[ -d "$RUNNER_APP" ] || die 3 "构建完成但 Runner.app 缺失: $RUNNER_APP"
log "prebuilt runner: $RUNNER_APP"
printf '%s\n' "$RUNNER_APP"
