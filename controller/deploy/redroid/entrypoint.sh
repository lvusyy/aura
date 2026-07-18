#!/bin/sh
# AURA node-sidecar entrypoint: wait for the in-pod Redroid adb endpoint to come up
# and finish booting, then hand off (exec) to aura-node in android-driver mode with
# an mTLS gRPC reverse connection to the control plane.
#
# Endpoint stability (D3): the device is always localhost:5555 — the co-located
# Redroid container in the same pod netns — never a bare PodIP.
set -eu

SERIAL="${AURA_SERIAL:-localhost:5555}"
CONTROLLER="${AURA_CONTROLLER:?AURA_CONTROLLER required (set in pod env)}"
TLS_DOMAIN="${AURA_TLS_DOMAIN:-aura-controller}"
CA="${AURA_CA:-/etc/aura/certs/ca.crt}"
CERT="${AURA_CERT:-/etc/aura/certs/node.crt}"
KEY="${AURA_KEY:-/etc/aura/certs/node.key}"
DATA_DIR="${AURA_DATA_DIR:-/var/lib/aura}"

echo "[entrypoint] waiting for Redroid adb endpoint ${SERIAL} (same-pod localhost) ..."
# Redroid boots after the sidecar starts; poll adb connect. AdbCli (TASK-003) also
# auto-reconnects on device offline/not-found, so this is best-effort readiness.
i=0
until adb connect "${SERIAL}" 2>/dev/null | grep -qE "connected to|already connected"; do
  i=$((i + 1))
  [ "$i" -ge 150 ] && { echo "[entrypoint] adb connect still pending after ~5m; proceeding"; break; }
  sleep 2
done
adb -s "${SERIAL}" wait-for-device 2>/dev/null || true
# Gate on the boot property value (getprop exits 0 regardless of value).
i=0
until [ "$(adb -s "${SERIAL}" shell getprop sys.boot_completed 2>/dev/null | tr -d '\r')" = "1" ]; do
  i=$((i + 1))
  [ "$i" -ge 180 ] && { echo "[entrypoint] sys.boot_completed wait timed out; proceeding (AdbCli retries)"; break; }
  sleep 2
done
echo "[entrypoint] adb ready (Android $(adb -s "${SERIAL}" shell getprop ro.build.version.release 2>/dev/null | tr -d '\r')); starting aura-node --driver android"

# Global options precede the transport subcommand (aura-node [OPTIONS] <COMMAND>).
# `http` keeps the node resident as a daemon; the mTLS gRPC reverse connection to the
# controller engages via --controller (TASK-006 node.log: http transport + registered).
# The local /mcp endpoint (default 0.0.0.0:7100) is unused by the control-plane loop.
exec aura-node \
  --driver android --serial "${SERIAL}" \
  --controller "${CONTROLLER}" --tls-domain "${TLS_DOMAIN}" \
  --ca "${CA}" --cert "${CERT}" --key "${KEY}" \
  --data-dir "${DATA_DIR}" \
  http --bind 0.0.0.0:7100
