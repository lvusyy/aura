#!/usr/bin/env bash
# 生成 AURA 控制面自签证书链：CA + server（:7443/:18080）+ 通用 node 客户端证书。
#
# M2 简化：节点侧用一张“通用节点证书”（CN=aura-node）证明“可信节点”即可通过 mTLS，
# 逻辑 node_id 身份由应用层 Register + 控制面分配 UUID 承接（禁机器指纹）。
#
# M12（TASK-006）在线签发：controller 持 CA 私钥（AURA_CA_KEY_PATH 指向本 ca.key，0600）在
# /v1/enroll 收节点 CSR 在线签 per-node 证书（CN=node-id，可单独吊销/轮换），私钥永不离节点。
# 本脚本产的 CA（ca.crt/ca.key）即在线签发素材：
#   - ca.crt 复用为 server mTLS 信任根（既有）+ enroll 响应回传节点信任根（新增用途，CA 未变）；
#   - ca.key 交付控制面（AURA_CA_KEY_PATH，两副本 240/225 同源分发），驱动在线签发；
#   - 通用 node 证书（CN=aura-node）保留兼容 existing 节点（Locked-7）——per-node 为新增接入路径，
#     不破坏旧；通用证书不入 node_certs 台账，吊销校验「未命中即放行」不误伤。
# 红线：ca.key 0600 / 非 git（.gitignore *.key）/ 两副本同源（运维分发，非代码入库）。
#
# ===== 通用节点证书退役计划与吊销 SOP（批E D1）=====
# 通用证书（CN=aura-node）共享一把私钥、不入 node_certs 台账 → 不可单独吊销、泄漏即全体节点信道
# 暴露。工作树内的分发副本（dist/certs/node.key|node.crt）已于批E 移除——凭据一律经运维通道分发，
# 不驻源码树；新节点接入一律走 enrollment（console「添加设备」一键命令，per-node cert）。
# 存量通用证书节点：滚更窗口逐台 `aura-node enroll` 换 per-node cert 后，通用证书按下法整体吊销。
# 吊销 SOP（RevocationMiddleware 按 cert_fp 反查 node_certs.revoked，未命中放行——插入吊销行即命中拒绝）：
#   FP=$(openssl x509 -in node.crt -outform DER | sha256sum | cut -d' ' -f1)
#   docker exec aura-postgres-1 psql -U aura -d aura -c "
#     INSERT INTO node_certs (node_id, serial, cert_fp, not_after, revoked)
#     VALUES ('00000000-0000-0000-0000-000000000000', 'generic-retired', '${FP}', now(), true)
#     ON CONFLICT (node_id, serial) DO UPDATE SET revoked = true;"
# 吊销后仍持通用证书的节点反连即 403（先确认全体节点已换 per-node cert，否则整批离线）。
#
# 用法：bash controller/deploy/gen-certs.sh
# 产物：controller/deploy/certs/{ca.crt,ca.key,server.crt,server.key,node.crt,node.key}
set -euo pipefail

DIR="$(cd "$(dirname "$0")" && pwd)/certs"
mkdir -p "$DIR"
cd "$DIR"

DAYS=365
CTRL_IP="10.0.0.10"   # 控制面 VM 静态 IP（server 证书 SAN）

# 1) 自签 CA
openssl genrsa -out ca.key 4096
openssl req -x509 -new -nodes -key ca.key -sha256 -days "$DAYS" \
  -subj "/O=AURA/CN=AURA Root CA" \
  -addext "basicConstraints=critical,CA:TRUE" \
  -addext "keyUsage=critical,keyCertSign,cRLSign" \
  -out ca.crt

# 2) server 证书（两端口共用；SAN 含 IP + DNS，节点按 IP 拨号需 IP SAN）
openssl genrsa -out server.key 2048
openssl req -new -key server.key -subj "/O=AURA/CN=aura-controller" -out server.csr
cat > server.ext <<EOF
basicConstraints=CA:FALSE
keyUsage=critical,digitalSignature,keyEncipherment
extendedKeyUsage=serverAuth
subjectAltName=IP:${CTRL_IP},DNS:aura-controller
EOF
openssl x509 -req -in server.csr -CA ca.crt -CAkey ca.key -CAcreateserial \
  -days "$DAYS" -sha256 -extfile server.ext -out server.crt

# 3) node 客户端证书（M2 通用节点证书，CN/SAN=aura-node）
openssl genrsa -out node.key 2048
openssl req -new -key node.key -subj "/O=AURA/CN=aura-node" -out node.csr
cat > node.ext <<EOF
basicConstraints=CA:FALSE
keyUsage=critical,digitalSignature,keyEncipherment
extendedKeyUsage=clientAuth
subjectAltName=DNS:aura-node
EOF
openssl x509 -req -in node.csr -CA ca.crt -CAkey ca.key -CAcreateserial \
  -days "$DAYS" -sha256 -extfile node.ext -out node.crt

# 清理中间产物
rm -f server.csr node.csr server.ext node.ext ca.srl

# 私钥 0600（仅所有者可读）：ca.key 尤其是签发 CA 私钥红线（AURA_CA_KEY_PATH 指向本文件在线签发）。
chmod 600 ca.key server.key node.key

echo "certs generated in ${DIR}:"
echo "  ca.crt ca.key server.crt server.key node.crt node.key"
echo
echo "M12 device enrollment (TASK-006): deliver ca.key to the control plane and point AURA_CA_KEY_PATH at it,"
echo "e.g.  AURA_CA_KEY_PATH=${DIR}/ca.key  (0600, non-git; distribute the SAME ca.key to both replicas 240/225)."
echo "Leaving AURA_CA_KEY_PATH unset keeps the signing surface (/v1/enroll,/v1/renew) disabled — controller"
echo "still runs; existing nodes keep using the generic node cert (CN=aura-node) unchanged (Locked-7)."
