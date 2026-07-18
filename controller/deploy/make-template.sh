#!/usr/bin/env bash
# make-template.sh —— 在 PVE 宿主一次性制作 aura-node 模板 VM（TASK-007 checkpoint，由人执行）。
#
# 从 Ubuntu 24.04 cloud image 制作模板：装 qemu-guest-agent + 预置 aura-node 二进制 +
# systemd 反连 unit + mTLS 证书，generalize（cloud-init clean + machine-id 清零 + 删 ssh host key），
# 收尾 qm template。运行时由 controller 的 go-proxmox provisioner 从此模板 clone/snapshot/rollback。
#
# 用 virt-customize(libguestfs) 离线注入（不启动 VM，确定性强）；binary/证书为宿主本地文件参数传入。
# 存储 local-lvm(LVM-thin)；模板不设静态 IP，clone 走 cloud-init DHCP（节点主动反连控制面，无需入站寻址）。
#
# 用法：
#   bash make-template.sh \
#     --node-bin /root/aura-node \
#     --ca /root/aura-certs/ca.crt --cert /root/aura-certs/node.crt --key /root/aura-certs/node.key \
#     --controller <controller-host>:7443 [--tls-domain aura-controller] [--vmid 9100] \
#     [--img /var/lib/vz/template/iso/noble-server-cloudimg-amd64.img] \
#     [--storage local-lvm] [--bridge vmbr0]
#
# 成功末行输出：TEMPLATE-OK
set -euo pipefail

# ---- 默认值 ----
VMID=9100
IMG="/var/lib/vz/template/iso/noble-server-cloudimg-amd64.img"
STORAGE="local-lvm"
BRIDGE="vmbr0"
TLS_DOMAIN="aura-controller"   # 须匹配控制面 server 证书 SAN（gen-certs.sh: DNS:aura-controller）
NODE_BIN=""
CA=""
CERT=""
KEY=""
CONTROLLER=""

# ---- 参数解析 ----
while [[ $# -gt 0 ]]; do
  case "$1" in
    --node-bin)   NODE_BIN="$2"; shift 2 ;;
    --ca)         CA="$2"; shift 2 ;;
    --cert)       CERT="$2"; shift 2 ;;
    --key)        KEY="$2"; shift 2 ;;
    --controller) CONTROLLER="$2"; shift 2 ;;
    --tls-domain) TLS_DOMAIN="$2"; shift 2 ;;
    --vmid)       VMID="$2"; shift 2 ;;
    --img)        IMG="$2"; shift 2 ;;
    --storage)    STORAGE="$2"; shift 2 ;;
    --bridge)     BRIDGE="$2"; shift 2 ;;
    *) echo "unknown arg: $1" >&2; exit 2 ;;
  esac
done

# ---- 校验 ----
die() { echo "ERROR: $*" >&2; exit 1; }

[[ $EUID -eq 0 ]] || die "must run as root on the PVE host (qm/virt-customize need root)"
[[ -n "$NODE_BIN" && -n "$CA" && -n "$CERT" && -n "$KEY" && -n "$CONTROLLER" ]] \
  || die "missing required args: --node-bin --ca --cert --key --controller"
for f in "$NODE_BIN" "$CA" "$CERT" "$KEY" "$IMG"; do
  [[ -f "$f" ]] || die "file not found: $f"
done
command -v qm >/dev/null 2>&1 || die "qm not found (run on the PVE host)"

# controller 传的是节点侧 --controller 期望的 host:port；容错去掉可能的 scheme 前缀。
CONTROLLER="${CONTROLLER#https://}"
CONTROLLER="${CONTROLLER#http://}"

if qm status "$VMID" >/dev/null 2>&1; then
  die "VMID $VMID already exists; pick another --vmid or remove it first (qm destroy $VMID)"
fi

# ---- 确保 libguestfs 可用（virt-customize）----
if ! command -v virt-customize >/dev/null 2>&1; then
  echo "[make-template] installing libguestfs-tools ..."
  apt-get update -qq
  apt-get install -y -qq libguestfs-tools
fi
# PVE 宿主内核下 supermin 直连后端更稳（免造 appliance 内核探测问题）。
export LIBGUESTFS_BACKEND=direct

# ---- 工作镜像：复制基镜像后离线定制（不动共享基镜像）----
WORKIMG="/var/lib/vz/template/iso/aura-node-template-${VMID}.qcow2"
cleanup() { rm -f "$WORKIMG" "$UNIT_FILE" 2>/dev/null || true; }
trap cleanup EXIT
echo "[make-template] copying base image -> $WORKIMG"
cp -f "$IMG" "$WORKIMG"

# ---- systemd 反连 unit（写临时文件后 upload 进镜像）----
UNIT_FILE="$(mktemp)"
cat >"$UNIT_FILE" <<UNIT
[Unit]
Description=AURA node (reverse gRPC to controller)
After=network-online.target qemu-guest-agent.service
Wants=network-online.target

[Service]
Type=simple
# 反连拨出控制面（mTLS）+ 常驻 MCP http 传输并存；--controller 为 host:port（节点侧自加 https://）。
# --data-dir 显式指定：systemd 系统服务无 HOME，node_id 持久化目录无法回落到 ~/.aura，必须显式给。
ExecStart=/usr/local/bin/aura-node --controller ${CONTROLLER} --ca /etc/aura/ca.crt --cert /etc/aura/node.crt --key /etc/aura/node.key --tls-domain ${TLS_DOMAIN} --data-dir /var/lib/aura http --bind 0.0.0.0:7100
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
UNIT

# ---- 离线定制镜像 ----
echo "[make-template] customizing image (install agent + inject binary/certs/unit + generalize) ..."
virt-customize -a "$WORKIMG" \
  --install qemu-guest-agent \
  --mkdir /etc/aura \
  --mkdir /var/lib/aura \
  --upload "$NODE_BIN":/usr/local/bin/aura-node \
  --upload "$CA":/etc/aura/ca.crt \
  --upload "$CERT":/etc/aura/node.crt \
  --upload "$KEY":/etc/aura/node.key \
  --upload "$UNIT_FILE":/etc/systemd/system/aura-node.service \
  --chmod '0755:/usr/local/bin/aura-node' \
  --chmod '0600:/etc/aura/ca.crt' \
  --chmod '0600:/etc/aura/node.crt' \
  --chmod '0600:/etc/aura/node.key' \
  --run-command 'systemctl enable qemu-guest-agent.service aura-node.service' \
  --run-command 'cloud-init clean --logs --seed' \
  --run-command 'truncate -s 0 /etc/machine-id' \
  --run-command 'rm -f /var/lib/dbus/machine-id' \
  --run-command 'rm -f /etc/ssh/ssh_host_*'

# ---- 建 VM + 导盘 + cloud-init 驱动器 + 转模板 ----
echo "[make-template] creating VM $VMID and importing disk ..."
qm create "$VMID" --name "aura-node-template" --memory 2048 --cores 2 --cpu host \
  --net0 "virtio,bridge=${BRIDGE}" --scsihw virtio-scsi-pci --ostype l26 --agent 1
qm importdisk "$VMID" "$WORKIMG" "$STORAGE"
qm set "$VMID" --scsi0 "${STORAGE}:vm-${VMID}-disk-0" --boot order=scsi0
qm set "$VMID" --ide2 "${STORAGE}:cloudinit"
qm set "$VMID" --serial0 socket --vga serial0
# clone 走 DHCP；ciupgrade 0：clone 首启不 apt upgrade（qemu-guest-agent 已 baked）。
qm set "$VMID" --ipconfig0 ip=dhcp
qm set "$VMID" --ciupgrade 0 2>/dev/null || true
qm template "$VMID"

echo "[make-template] template ready: VMID=$VMID name=aura-node-template controller=${CONTROLLER} tls-domain=${TLS_DOMAIN}"
echo "TEMPLATE-OK"
