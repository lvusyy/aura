#!/bin/sh
# AURA M8-P2 — aura-node desktop-sidecar entrypoint (Selkies Linux container desktop).
#
# Runs BESIDE the Selkies container in the SAME pod. Unlike the redroid sidecar (which
# reaches an adb device over localhost:5555), the desktop driver drives the *X11 display*
# that Selkies renders + streams: aura-platform captures via xcap (core libxcb GetImage,
# no extra .so — ldd on the release binary NEEDs only libxcb/libXau/libXdmcp/libdbus) and
# injects input via enigo (pure-Rust x11rb XTEST). So the ONLY coupling is the X server:
# this container shares Selkies' X unix socket (/tmp/.X11-unix, emptyDir) + DISPLAY, and,
# if Selkies enforces MIT-MAGIC-COOKIE auth, its XAUTHORITY (shared file). No adb here.
#
# Then it hands off (exec) to aura-node --driver desktop with an mTLS gRPC reverse
# connection to the control plane — identical reverse-connect contract to the redroid
# sidecar (certs from the aura-node-certs secret at /etc/aura/certs).
set -eu

CONTROLLER="${AURA_CONTROLLER:?AURA_CONTROLLER required (set in pod env)}"
TLS_DOMAIN="${AURA_TLS_DOMAIN:-aura-controller}"
CA="${AURA_CA:-/etc/aura/certs/ca.crt}"
CERT="${AURA_CERT:-/etc/aura/certs/node.crt}"
KEY="${AURA_KEY:-/etc/aura/certs/node.key}"
DATA_DIR="${AURA_DATA_DIR:-/var/lib/aura}"
DISPLAY_VAL="${DISPLAY:-:0}"

# Derive the X socket path from DISPLAY (":0" -> /tmp/.X11-unix/X0). Selkies creates it
# after its X server boots; the shared emptyDir makes it visible here.
DPYNUM=$(printf '%s' "$DISPLAY_VAL" | sed 's/^.*://; s/\..*$//')
XSOCK="/tmp/.X11-unix/X${DPYNUM}"

echo "[entrypoint-desktop] waiting for Selkies X socket ${XSOCK} (DISPLAY=${DISPLAY_VAL}) ..."
i=0
until [ -S "$XSOCK" ]; do
  i=$((i + 1))
  [ "$i" -ge 180 ] && { echo "[entrypoint-desktop] X socket still absent after ~6m; proceeding (node registers regardless; screenshot retries)"; break; }
  sleep 2
done
# Brief settle so the X server finishes bringing up RandR/root window before first capture.
sleep 3

# X authority: if a shared XAUTHORITY was provided and exists, use it; otherwise proceed
# (Selkies image may disable access control). aura-node's xcb connect reads XAUTHORITY.
if [ -n "${XAUTHORITY:-}" ] && [ -f "${XAUTHORITY}" ]; then
  echo "[entrypoint-desktop] using shared XAUTHORITY=${XAUTHORITY}"
else
  echo "[entrypoint-desktop] no XAUTHORITY file (assuming no X access control)"
fi

echo "[entrypoint-desktop] X ready; starting aura-node --driver desktop -> controller ${CONTROLLER}"

# Global options precede the transport subcommand (aura-node [OPTIONS] <COMMAND>). desktop
# driver ignores --serial. `http` keeps the node resident; the mTLS gRPC reverse connection
# engages via --controller. Bind the (unused-by-control-plane) /mcp endpoint on loopback so
# the hostNetwork pod adds no host port footprint (control plane uses the reverse gRPC link).
export DISPLAY="$DISPLAY_VAL"
exec aura-node \
  --driver desktop \
  --controller "${CONTROLLER}" --tls-domain "${TLS_DOMAIN}" \
  --ca "${CA}" --cert "${CERT}" --key "${KEY}" \
  --data-dir "${DATA_DIR}" \
  http --bind 127.0.0.1:7100
