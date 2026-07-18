# Selkies GPU-less Linux container desktop on k3s (AURA M8-P2, SC-3)

Streams a Selkies WebRTC Linux desktop from the k3s host `user@<k3s-host>`
(node `<k3s-host>`, Ubuntu 24.04) and — in the sidecar form — drives that same
desktop from the AURA control plane (`aura-node --driver desktop`). Delivers SC-3:
**browser watches the live desktop stream + in-container screenshot→click closed loop.**

Design decisions: **no GPU** (Xvfb software render, `SELKIES_ENCODER=x264enc`, and a
GLX-disabling workaround — see [§GPU-less](#gpuless)); **hostNetwork** so WebRTC ICE host
candidates include the host's Tailscale IP (TASK-001 gate = `DIRECT_OK` carries the media)
with Selkies' ICE pointed at our coturn (the image's default external ICE servers are
unreachable here — see [§TURN](#turn)); **non-privileged**; `/dev/shm` as
`emptyDir{medium:Memory}` (the default 64 MB crashes Chromium). Isolated to the **`aura`**
namespace (Locked-10). SAFETY: the `default`-ns VMs and existing KubeVirt VMIs are never
touched, and the KubeVirt VM-desktop screenshot/console path is unchanged.

## Files

| Repo file                   | Purpose                                                              |
|-----------------------------|---------------------------------------------------------------------|
| `selkies-pod.yaml`          | STREAMING-ONLY Selkies pod (stream, no aura-node) — minimal fallback |
| `selkies-node-sidecar.yaml` | Selkies + aura-node sidecar in ONE pod — the SC-3 deliverable        |
| `entrypoint-desktop.sh`     | node-sidecar entrypoint: wait for Selkies X socket, then start node  |

Both pods are `hostNetwork` and bind the same host ports — **run ONE at a time** (see
[§Coexistence](#coexistence)).

## Ports (two-tier — verified from the image)

`hostNetwork: true` means **every** container listen binds a host port directly, so all
three Selkies ports must be free. Selkies is two-tier: nginx fronts the public endpoint and
reverse-proxies the internal gstreamer app.

| Env var                      | Value  | Bind      | Role                                             |
|------------------------------|--------|-----------|--------------------------------------------------|
| `NGINX_PORT`                 | `8082` | 0.0.0.0   | **PUBLIC** signaling/web (basic auth); browser + probe + curl hit this |
| `SELKIES_PORT`               | `8083` | localhost | internal gstreamer app (nginx-proxied)           |
| `SELKIES_METRICS_HTTP_PORT`  | `8084` | localhost | internal metrics (nginx `/metrics`)              |

> **Why not the defaults:** nginx defaults to `8080` and gstreamer to `8081` — **both are
> occupied** on this host. All three are overridden to the free 8082/8083/8084 block. The
> console (`selkiesLaunch.tsx`) and the readiness probe both target the public `8082`.

WebRTC **media** uses ephemeral high UDP ports (webrtcbin), distinct from coturn's relay
range `49160-49200`. Host LISTEN set already in use: `22 53 80 2222 3478 5037 5901-5903
6443 6444 7120 8080 8081 9428 9998 9999 18080` (+ redroid adb `5555`); `7443` lives on the
control-plane VM, not here.

## Signaling auth (basic auth)

nginx enforces HTTP basic auth on the public port using an htpasswd built from the pod env:

- `SELKIES_ENABLE_BASIC_AUTH=true`, `SELKIES_BASIC_AUTH_USER=aura`,
  `SELKIES_BASIC_AUTH_PASSWORD=aura-selkies`.

The browser gets a native basic-auth prompt on first open. This is separate from any TURN
auth (see [§TURN](#turn)).

## Browser requirement (H.264)

Selkies streams **H.264** (`x264enc`). Open the stream in a browser whose WebRTC stack
decodes H.264 — **Chrome or Edge** (also Safari). Firefox depends on an OS H.264 codec and
is not guaranteed. Use a Chromium-based browser over Tailscale.

> The same **Chrome/Edge** requirement applies to the other live-stream path — the Redroid
> **Tango** console stream (`../redroid/`), which decodes H.264 via **WebCodecs** on a canvas.
> Note (TASK-003 R2): Playwright's *bundled* Chromium ships **without** the proprietary H.264
> codec, so WebCodecs `VideoDecoder` reports it unsupported and the Tango canvas stays blank —
> use a real Chrome/Edge install, not headless bundled Chromium, to watch either live stream.

## <a name="image"></a>§image — image selection & mirror fallback

Pinned image: `ghcr.io/selkies-project/nvidia-egl-desktop:24.04` (the egl-desktop image
auto-falls-back to llvmpipe when no GPU / no nvidia runtime is present). It is **already
pre-pulled** into this k3s containerd, and `imagePullPolicy: IfNotPresent` uses the resident
copy — no pull needed for a normal deploy.

If you ever need to (re)acquire it and ghcr is throttled, any ONE path suffices (k3s uses
its bundled containerd; namespace `k8s.io`):

```bash
IMG=ghcr.io/selkies-project/nvidia-egl-desktop:24.04

# Path 1 — direct (this k3s redirects docker.io via a pull-through mirror; ghcr is direct):
sudo k3s ctr -n k8s.io images pull "$IMG"

# Path 2 — CN mirror, then retag to the pod's ghcr reference (docker.1ms.run works 2026-07;
#          docker.m.daocloud.io is dead/403):
sudo k3s ctr -n k8s.io images pull docker.1ms.run/selkies-project/nvidia-egl-desktop:24.04
sudo k3s ctr -n k8s.io images tag  docker.1ms.run/selkies-project/nvidia-egl-desktop:24.04 "$IMG"

# Path 3 — offline tar import (on a host with working Docker/registry access):
#   docker pull "$IMG" && docker save "$IMG" -o selkies.tar ; scp selkies.tar user@<k3s-host>: ; then:
sudo k3s ctr -n k8s.io images import selkies.tar

sudo k3s crictl images | grep nvidia-egl-desktop   # confirm resident
```

## Prerequisites (already in place on this host)

- `aura` namespace (see `../redroid/namespace.yaml`).
- Secret `aura-node-certs` (`ca.crt`/`node.crt`/`node.key`) — the same secret the redroid
  sidecar uses. Recreate if absent:
  ```bash
  kubectl -n aura create secret generic aura-node-certs \
    --from-file=ca.crt=~/aura-m5/certs/ca.crt \
    --from-file=node.crt=~/aura-m5/certs/node.crt \
    --from-file=node.key=~/aura-m5/certs/node.key
  ```
- Image `docker.io/library/aura-node-sidecar:m6` in containerd (desktop-capable aura-node).
- coturn pod + secret `coturn-secret` — **required** for Selkies' ICE here (both pods
  reference it via `secretKeyRef`); see `../turn/` and [§TURN](#turn).

## Deploy — streaming-only (stream, no closed loop)

```bash
kubectl -n aura apply -f selkies-pod.yaml
kubectl -n aura get pod selkies -w                 # wait Running + Ready (readiness on 8082)
```

Verify the public endpoint (basic-auth 401 without creds still proves nginx is serving):

```bash
curl -sfi -u aura:aura-selkies http://<host-tailscale-ip>:8082/ | head -1   # -> 200 OK
```

Then open `http://<host-tailscale-ip>:8082/` in Chrome/Edge over Tailscale, enter `aura` /
`aura-selkies` at the basic-auth prompt, and the desktop streams.

## Deploy — sidecar (stream + closed loop) — the SC-3 form

The sidecar pod adds an `aura-node --driver desktop` container that shares Selkies' X socket
(`/tmp/.X11-unix` emptyDir + `DISPLAY=:20`) and reverse-connects to the control plane. Its
entrypoint is mounted from a configMap (the m6 image bakes only the android entrypoint):

```bash
# 1) configMap from the repo entrypoint (source of truth; re-create to update it):
kubectl -n aura create configmap selkies-node-entrypoint \
  --from-file=entrypoint-desktop.sh=controller/deploy/selkies/entrypoint-desktop.sh \
  --dry-run=client -o yaml | kubectl apply -f -

# 2) only one Selkies at a time — remove the streaming-only pod first if it is up:
kubectl -n aura delete pod selkies --ignore-not-found --wait=true

# 3) apply the sidecar pod:
kubectl -n aura apply -f selkies-node-sidecar.yaml
kubectl -n aura get pod selkies-node-sidecar -w    # -> 2/2 Running (selkies + node-sidecar)
```

Verify the closed loop from the control-plane VM
(`ssh -J user@<k3s-host> user@<controller-host>`, then `source ~/aura-env`):

```bash
auractl node list                                  # -> a desktop node, online
# dispatch a screenshot then a click against that node id (see evidence/ for the recorded run)
```

The browser stream works identically to the streaming-only form (same Selkies container).

## <a name="gpuless"></a>§GPU-less — the Xvfb GLX crash workaround

With no GPU, this image's Xvfb **segfaults on startup**: it boots with `+extension GLX
+iglx`, which dlopens Mesa's `swrast_dri.so → libdril_dri.so` (the unified software-DRI
loader) and crashes during GLX init (verified on this host; even a minimal `Xvfb -screen 0
1280x720x24` crashes, and forcing `GALLIUM_DRIVER=llvmpipe` does not help). The image
hardcodes the Xvfb flags, so both pods instead set **`LIBGL_DRIVERS_PATH=/opt/no-dri`** (an
empty `emptyDir`): GLX finds no DRI driver and self-disables gracefully, and Xvfb comes up as
a 2D software display (RANDR/RENDER/XTEST intact). GLX is not needed here — aura-node captures
via xcb `GetImage` + injects via XTEST, Selkies captures the 2D framebuffer, and Chromium
uses its own SwiftShader. Without this, no X server → no stream and no closed loop.

## <a name="turn"></a>§TURN — Selkies ICE → our coturn (REQUIRED here)

TASK-001's spike gate is `DIRECT_OK`: the browser reaches the host's Tailscale IP directly
over UDP (`evidence/spike-verdict.md`), and that direct host-candidate path carries the WebRTC
**media**. But the Selkies image's **default ICE servers are unreachable in this air-gapped
segment** — it defaults to `stun.l.google.com:19302` + the openrelay TURN server, and its
own bundled turnserver collides with our coturn on host `:3478`. The result: webrtcbin stalls
on ICE gathering, never sends an offer, and the browser never creates a PeerConnection (no
picture). So **both pods point Selkies' ICE at our coturn** (deployed in TASK-001, reachable
on the Tailscale IP). This also makes the entrypoint **skip its bundled turnserver** (dodging
the `:3478` clash). ICE then gathers `host` + `srflx` candidates and connects; the direct host
candidate is preferred for media (DIRECT_OK preserved), with coturn relay as the fallback.

Both `selkies-pod.yaml` and `selkies-node-sidecar.yaml` already carry this in the **selkies**
container `env` — the shared secret comes from the `coturn-secret` k8s secret via
`secretKeyRef` (kept out of the repo; the plaintext lives only in
`evidence/TASK-001-coturn-deploy.log`):

```yaml
        - name: SELKIES_STUN_HOST
          value: "<host-tailscale-ip>"
        - name: SELKIES_STUN_PORT
          value: "3478"
        - name: SELKIES_TURN_HOST
          value: "<host-tailscale-ip>"      # coturn advertises relay-ip <host-tailscale-ip> (Tailscale)
        - name: SELKIES_TURN_PORT
          value: "3478"
        - name: SELKIES_TURN_PROTOCOL
          value: "udp"                # switch to "tcp" if a client leg's UDP is ACL-blocked
                                      # (and set no-udp in turnserver.conf accordingly)
        - name: SELKIES_TURN_SHARED_SECRET
          valueFrom:
            secretKeyRef: { name: coturn-secret, key: static-auth-secret }
```

Selkies computes REST credentials from the shared secret client-side
(`username=<expiry-unix>`, `password=base64(HMAC-SHA1(secret, username))`); coturn was
verified end-to-end in TASK-001 (Allocate SUCCESS, `XOR-RELAYED-ADDRESS` in `49160-49200`).
Basic-auth (signaling) and TURN auth are independent.

## <a name="coexistence"></a>Coexistence — run ONE at a time

`selkies-pod.yaml` and `selkies-node-sidecar.yaml` are both `hostNetwork` and both bind host
`8082/8083/8084`. Run **either** the streaming-only pod **or** the sidecar pod, never both.
The sidecar form is the SC-3 deliverable; the streaming-only pod is the minimal fallback.

## Teardown (aura ns only)

```bash
kubectl -n aura delete pod selkies-node-sidecar --ignore-not-found
kubectl -n aura delete pod selkies --ignore-not-found
kubectl -n aura delete configmap selkies-node-entrypoint --ignore-not-found
# leaves the aura namespace, aura-node-certs, coturn, and all VMIs intact
```

## Boundary (what this does NOT touch)

- **EnvProvider / `provider.go` unchanged.** `EnvProvider` is VMI-centric (K8sProvisioner
  builds KubeVirt VMIs); Selkies is a plain harness-level Pod (redroid precedent), so there
  is no provider flavor and `provider.go` is not modified (YAGNI — a Pod-GVR adapter would be
  over-design here).
- **KubeVirt VM desktops unchanged** — they keep the existing screenshot/console path; this
  work adds a parallel container-desktop path and regresses neither.
