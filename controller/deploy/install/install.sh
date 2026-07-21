#!/bin/sh
# controller/deploy/install/install.sh
# AURA 一键接入安装器（Linux / macOS）—— M12 TASK-009。
#
# 全新设备一条命令接入舰队（console「添加设备」拿到）：
#   curl -fsSL https://<release-host>/install.sh | sh -s -- --token <TOKEN> --controller <VIP:7443>
# 可选参数：--label "工位A-桌面" --location "SH-IDC" --data-dir <dir>
#
# 7 步流程（install-command-spec §3）：
#   1 检测平台     uname -s/-m → linux/darwin + amd64/arm64
#   2 拉二进制     GET release URL → /usr/local/bin/aura-node（chmod 0755）
#   3+4 enroll     aura-node enroll：本地生成密钥+CSR（私钥永不离节点）→ POST /v1/enroll 带 token+CSR
#                  → controller CA 签 per-node cert → 落盘 <data-dir>/{node.key,node.crt,ca.crt,node_id}
#   5 装证书       enroll 已 co-locate 落 <data-dir>（无独立步骤）
#   6 装服务       Linux systemd unit / macOS launchd LaunchAgent（反连参数模板化）
#   7 起服务反连   per-node cert mTLS 拨 :7443 → Register → 现身 fleet
#
# node enroll/renew 子命令是 TASK-006 交付（node/crates/aura-node，feature enroll），本脚本调用。
# 真机一键接入实证归 TASK-011（≥1 平台全新设备现身 fleet）；mac 真机验收 deferred（停机）。
# release URL 二进制分发归 TASK-012（GitHub public release），本脚本占位 <release-host>，T011/T012 对齐真实 URL。
set -eu

# ============================ 默认值 / 可覆盖配置 ============================
TOKEN=""
CONTROLLER=""
LABEL=""
LOCATION=""
DATA_DIR=""

# release 分发基址（占位——T011/T012 对齐 GitHub release 真实 URL；可经 --release-base / $AURA_RELEASE_BASE 覆盖）。
RELEASE_BASE="${AURA_RELEASE_BASE:-https://<release-host>}"
# CA pin 源（缺省派生自 release 基址；节点首触 enroll 时无 ca.crt，以此校验 controller server 身份闭合中间人）。
CA_URL="${AURA_CA_URL:-}"
# 反连稳态参数（与 make-template.sh / aura-node.service 同款）。
TLS_DOMAIN="${AURA_TLS_DOMAIN:-aura-controller}"
HTTP_BIND="${AURA_HTTP_BIND:-0.0.0.0:7100}"
# enroll REST bootstrap 端口（install-command-spec §2：由 --controller 的 HOST 派生 HOST:18080）。
ENROLL_PORT="${AURA_ENROLL_PORT:-18080}"
# 二进制安装路径：缺省在 DATA_DIR 解析后派生为 ${DATA_DIR}/bin/aura-node（M16 self-update 布局，
# 见平台检测段注释）；可经 $AURA_BIN_PATH 显式覆盖。
BIN_PATH="${AURA_BIN_PATH:-}"

die()  { echo "ERROR: $*" >&2; exit 1; }
info() { echo "[install] $*" >&2; }
warn() { echo "[install] WARNING: $*" >&2; }

# ============================ 参数解析 ============================
while [ $# -gt 0 ]; do
  case "$1" in
    --token)        TOKEN="$2"; shift 2 ;;
    --controller)   CONTROLLER="$2"; shift 2 ;;
    --label)        LABEL="$2"; shift 2 ;;
    --location)     LOCATION="$2"; shift 2 ;;
    --data-dir)     DATA_DIR="$2"; shift 2 ;;
    --release-base) RELEASE_BASE="$2"; shift 2 ;;
    --ca-url)       CA_URL="$2"; shift 2 ;;
    -h|--help)
      echo "usage: install.sh --token <TOKEN> --controller <HOST:7443> [--label L] [--location L] [--data-dir D]" >&2
      exit 0 ;;
    *) die "unknown arg: $1 (see --help)" ;;
  esac
done

[ -n "$TOKEN" ]      || die "--token is required"
[ -n "$CONTROLLER" ] || die "--controller is required (HOST:7443)"
command -v curl >/dev/null 2>&1 || die "curl not found (required to fetch binary + CA)"
# 批E C4：占位基址从 warn 升级为强制拒绝——占位形态下 curl 必然失败且报错晦涩（DNS 解析
# <release-host>），部署期直接 fail-fast 指认配置缺口。真实分发（T09 临时 HTTP / 未来 release 托管）
# 一律显式传 --release-base 或 $AURA_RELEASE_BASE。
case "$RELEASE_BASE" in
  *"<release-host>"*) die "RELEASE_BASE 仍为占位 <release-host>：以 --release-base 或 \$AURA_RELEASE_BASE 指定真实分发地址（如临时 HTTP 托管 http://<build-host>:18888）" ;;
esac

# ============================ 提权助手（非 root 走 sudo）============================
SUDO=""
if [ "$(id -u)" -ne 0 ]; then
  if command -v sudo >/dev/null 2>&1; then
    SUDO="sudo"
  else
    die "need root or sudo to install binary/service"
  fi
fi
# run_priv / write_priv：SUDO 为空时直接执行，避免调用空串命令。
run_priv()   { if [ -n "$SUDO" ]; then "$SUDO" "$@"; else "$@"; fi }
write_priv() { if [ -n "$SUDO" ]; then "$SUDO" tee "$1" >/dev/null; else tee "$1" >/dev/null; fi }

# ============================ 装服务函数（定义先于调用；读全局变量 CALL 时求值）============================

# ---- Linux systemd（aura-node.service.tmpl 底稿）----
install_service_systemd() {
  service_user="${SUDO_USER:-$(id -un)}"
  # 桌面会话 env 固化（screenshot/input 依赖，spec §4）：从活跃会话抄 DISPLAY/XAUTHORITY/XDG_RUNTIME_DIR。
  run_priv mkdir -p /etc/aura
  {
    echo "# AURA node desktop session env（install.sh 从活跃会话抄现值；screenshot/input 依赖）"
    if [ -n "${DISPLAY:-}" ];         then echo "DISPLAY=${DISPLAY}"; fi
    if [ -n "${XAUTHORITY:-}" ];      then echo "XAUTHORITY=${XAUTHORITY}"; fi
    if [ -n "${XDG_RUNTIME_DIR:-}" ]; then echo "XDG_RUNTIME_DIR=${XDG_RUNTIME_DIR}"; fi
  } | write_priv /etc/aura/aura-node.env
  if [ -z "${DISPLAY:-}" ]; then
    warn "未捕获 DISPLAY（无活跃图形会话?）——screenshot/input_inject 需手动补 /etc/aura/aura-node.env 的 DISPLAY/XAUTHORITY（spec §4 会话 env 债）；反连注册不受影响"
  fi

  exec_line="${BIN_PATH} --driver desktop --controller ${CONTROLLER} --ca ${DATA_DIR}/ca.crt --cert ${DATA_DIR}/node.crt --key ${DATA_DIR}/node.key --tls-domain ${TLS_DOMAIN} --data-dir ${DATA_DIR}${EXEC_META} http --bind ${HTTP_BIND}"
  info "install systemd unit /etc/systemd/system/aura-node.service (User=${service_user})"
  cat <<EOF | write_priv /etc/systemd/system/aura-node.service
[Unit]
Description=AURA node (reverse gRPC to controller)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=${service_user}
EnvironmentFile=-/etc/aura/aura-node.env
ExecStart=${exec_line}
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF
  run_priv systemctl daemon-reload
  run_priv systemctl enable --now aura-node
  info "systemd: aura-node enabled + started（systemctl status aura-node 查状态）"
}

# ---- macOS launchd LaunchAgent（aura-node.plist.tmpl 底稿；真机验收 deferred）----
install_service_launchd() {
  agent_dir="${HOME}/Library/LaunchAgents"
  plist="${agent_dir}/com.aura.node.plist"
  mkdir -p "$agent_dir"
  info "install launchd LaunchAgent ${plist}（mac 真机验收 deferred）"
  # LaunchAgent 落用户家目录，无需 sudo（跑用户 GUI 会话得屏幕录制上下文）。
  # ProgramArguments 逐 token（launchd 不解析 shell 引号）；--label/--location 若有则单独 token 追加。
  {
    cat <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>com.aura.node</string>
    <key>ProgramArguments</key>
    <array>
        <string>${BIN_PATH}</string>
        <string>--driver</string><string>desktop</string>
        <string>--controller</string><string>${CONTROLLER}</string>
        <string>--ca</string><string>${DATA_DIR}/ca.crt</string>
        <string>--cert</string><string>${DATA_DIR}/node.crt</string>
        <string>--key</string><string>${DATA_DIR}/node.key</string>
        <string>--tls-domain</string><string>${TLS_DOMAIN}</string>
        <string>--data-dir</string><string>${DATA_DIR}</string>
EOF
    if [ -n "$LABEL" ];    then printf '        <string>--label</string><string>%s</string>\n' "$LABEL"; fi
    if [ -n "$LOCATION" ]; then printf '        <string>--location</string><string>%s</string>\n' "$LOCATION"; fi
    cat <<EOF
        <string>http</string>
        <string>--bind</string><string>${HTTP_BIND}</string>
    </array>
    <key>RunAtLoad</key><true/>
    <key>KeepAlive</key><true/>
    <key>StandardOutPath</key><string>${DATA_DIR}/aura-node.log</string>
    <key>StandardErrorPath</key><string>${DATA_DIR}/aura-node.log</string>
</dict>
</plist>
EOF
  } > "$plist"
  uid="$(id -u)"
  launchctl bootstrap "gui/${uid}" "$plist" 2>/dev/null || launchctl load "$plist" 2>/dev/null || warn "launchctl 装载失败（mac 停机 deferred，T11+ 真机核对）"
  launchctl kickstart -k "gui/${uid}/com.aura.node" 2>/dev/null || true
  warn "macOS TCC：屏幕录制/辅助功能权限须在系统设置手动授予 ${BIN_PATH}（真机 T11+ 处理）"
}

# ============================ 1) 检测平台 ============================
os="$(uname -s)"
arch="$(uname -m)"
case "$os" in
  Linux)  plat_asset="linux";  plat_enroll="linux" ;;
  Darwin) plat_asset="darwin"; plat_enroll="macos" ;;  # asset 用 darwin；enroll --platform 用 macos（std::env::consts::OS 词汇，对齐 enroll.rs 默认）
  *) die "unsupported OS: $os (installer 仅 Linux/macOS；Windows 走 install.ps1)" ;;
esac
case "$arch" in
  x86_64|amd64)  arch_asset="amd64" ;;
  aarch64|arm64) arch_asset="arm64" ;;
  *) die "unsupported arch: $arch (支持 amd64/arm64)" ;;
esac
ASSET="aura-node-${plat_asset}-${arch_asset}"
info "platform: os=${os} arch=${arch} -> asset=${ASSET} enroll-platform=${plat_enroll}"

# 平台缺省 data-dir（install-command-spec §2）。
if [ -z "$DATA_DIR" ]; then
  case "$plat_asset" in
    linux)  DATA_DIR="/var/lib/aura" ;;
    darwin) DATA_DIR="/usr/local/var/aura" ;;
  esac
fi

# M16 self-update 布局：二进制装进数据目录（随后 chown 归服务用户）——self-update 原子换刀需要节点
# 进程对二进制所在目录可写；旧 /usr/local/bin（root 属主）布局下非 root 服务无法 rename 替换。
# /usr/local/bin/aura-node 保留 symlink 供人工 CLI（enroll/service 子命令），换刀换真身 symlink 恒有效。
if [ -z "$BIN_PATH" ]; then BIN_PATH="${DATA_DIR}/bin/aura-node"; fi

# ============================ 2) 拉二进制 ============================
BIN_URL="${RELEASE_BASE}/${ASSET}"
info "fetch binary: ${BIN_URL} -> ${BIN_PATH}"
run_priv mkdir -p "$(dirname "$BIN_PATH")"
tmpbin="$(mktemp)"
tmpca=""
trap 'rm -f "$tmpbin" "${tmpca:-}"' EXIT
curl -fsSL "$BIN_URL" -o "$tmpbin" || die "download binary failed: ${BIN_URL}"
run_priv install -m 0755 "$tmpbin" "$BIN_PATH"
# 便捷 symlink（best-effort）：真身在数据目录（self-update 换真身），symlink 供 PATH 内人工调用。
if [ "$BIN_PATH" != "/usr/local/bin/aura-node" ]; then
  run_priv ln -sf "$BIN_PATH" /usr/local/bin/aura-node || warn "symlink /usr/local/bin/aura-node failed（不影响服务）"
fi

# ============================ 3+4) enroll（genkey+CSR → 换 per-node cert）============================
run_priv mkdir -p "$DATA_DIR"

# CA pin：pre-placed 优先（<data-dir>/ca.crt 已存则复用，airgap/自定义信任根友好），否则从 release 拉。
# enroll.rs --ca 必填：节点以此 pin 校验 controller :18080 server-TLS（TOFU，闭合中间人，design §3.3）。
CA_PIN="${DATA_DIR}/ca.crt"
if run_priv test -f "$CA_PIN"; then
  info "CA pin 复用已存在 ${CA_PIN}"
else
  if [ -z "$CA_URL" ]; then CA_URL="${RELEASE_BASE}/ca.crt"; fi
  info "fetch CA pin: ${CA_URL} -> ${CA_PIN}"
  tmpca="$(mktemp)"
  curl -fsSL "$CA_URL" -o "$tmpca" || die "download CA pin failed: ${CA_URL}（可预置 ${CA_PIN} 或传 --ca-url）"
  run_priv install -m 0644 "$tmpca" "$CA_PIN"
fi

# enroll 端点：由 --controller 的 HOST 派生 HOST:18080（install-command-spec §2）。
# 注：${CONTROLLER%%:*} 剥离 :port 取 HOST，对 IPv4/hostname 正确；IPv6 字面量（含多冒号）不支持，
#     此类环境请预置 --ca-url 与端点或后续 T011 对齐（LAN IPv4 为目标形态）。
CTRL_HOST="${CONTROLLER%%:*}"
ENROLL_EP="${CTRL_HOST}:${ENROLL_PORT}"
info "enroll endpoint: ${ENROLL_EP} (derived from controller HOST)"

# 构造 enroll argv（POSIX set -- 增量拼接，稳健处理 --label/--location 含空格值）。
set -- enroll \
  --controller "$ENROLL_EP" \
  --token "$TOKEN" \
  --platform "$plat_enroll" \
  --ca "$CA_PIN" \
  --data-dir "$DATA_DIR"
if [ -n "$LABEL" ];    then set -- "$@" --label "$LABEL"; fi
if [ -n "$LOCATION" ]; then set -- "$@" --location "$LOCATION"; fi
info "aura-node enroll（私钥本地生成不出节点，换 per-node cert 落 ${DATA_DIR}）..."
run_priv "$BIN_PATH" "$@" || die "enroll failed（token 无效/过期/耗尽?→401；CSR 拒?→400；网络/CA pin?→检查 ${ENROLL_EP} 可达与 ${CA_PIN} 匹配）"

# enroll 经 run_priv（sudo）落 root-owned 凭据（node.key 0600 root）；但反连服务以非 root 用户跑
# （macOS launchd 用户 GUI 会话得屏幕录制上下文 / Linux systemd User=${service_user} 得桌面会话），
# 该用户读不了 root 的 node.key → 启动即 EX_CONFIG（launchd exit 78 / systemd 循环重启）。
# 修：sudo 提权装过时把 DATA_DIR chown 给服务用户，令反连服务可读自身凭据（凭据仍 0600，仅换属主）。
SVC_USER="${SUDO_USER:-$(id -un)}"
if [ -n "$SUDO" ]; then
  info "chown ${DATA_DIR} -> ${SVC_USER}（反连服务以非 root 用户跑，须可读 node.key）"
  run_priv chown -R "$SVC_USER" "$DATA_DIR"
fi

# ============================ 6) 装服务 + 7) 起服务反连 ============================
# 反连 ExecStart 元数据（per-node cert 自证，无 token）；--label/--location 作 Register 引导值（console 可后编）。
EXEC_META=""
if [ -n "$LABEL" ];    then EXEC_META="${EXEC_META} --label \"${LABEL}\""; fi
if [ -n "$LOCATION" ]; then EXEC_META="${EXEC_META} --location \"${LOCATION}\""; fi

case "$plat_asset" in
  linux)  install_service_systemd ;;
  darwin) install_service_launchd ;;
esac

info "DONE：aura-node 已接入（enroll 换 per-node cert + 装服务 + 反连）。真机现身 fleet 实证归 TASK-011。"
