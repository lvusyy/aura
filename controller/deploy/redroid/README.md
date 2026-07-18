# Redroid Android device on k3s (AURA M5)

Static deployment of a Redroid Android container on the k3s host
`user@<k3s-host>` (node `<k3s-host>`, Ubuntu 24.04 / kernel 6.8). Provides a
stable `localhost:5555` adb endpoint for `aura-node --driver android` (M5 P1).

Design decisions: binder path = **binderfs** (D1, overrides the roadmap's DKMS path —
実測 2026-07-09 作废 DKMS); deploy = **hostPort 先行** (D3, never bare PodIP); pod is
**privileged** and isolated to the **`aura`** namespace (Locked-10). SAFETY: the
`default`-ns VMs and existing `aura` VMIs are never touched.

## Files

| Repo file            | Installs to                              | Purpose                                    |
|----------------------|------------------------------------------|--------------------------------------------|
| `binder.conf`        | `/etc/modules-load.d/binder.conf`        | autoload `binder_linux` at boot            |
| `binder-options.conf`| `/etc/modprobe.d/binder-options.conf`    | `devices=binder,hwbinder,vndbinder` param  |
| `binderfs.mount`     | `/etc/systemd/system/dev-binderfs.mount` | mount binderfs at `/dev/binderfs` at boot  |
| `binderfs-perms.service` | `/etc/systemd/system/binderfs-perms.service` | chmod 0666 the binder nodes (Android opens them non-root) |
| `namespace.yaml`     | (kubectl)                                | ensure `aura` namespace                    |
| `redroid-pod.yaml`   | (kubectl)                                | privileged Redroid pod, hostPort 5555      |

> The mount unit **must** be named `dev-binderfs.mount` on the host — systemd derives
> the unit name from `Where=/dev/binderfs`. The repo keeps the source name
> `binderfs.mount`; copy it under the systemd-required name.

## 1. binderfs 固化 (host, sudo)

```bash
sudo cp binder.conf            /etc/modules-load.d/binder.conf
sudo cp binder-options.conf    /etc/modprobe.d/binder-options.conf
sudo cp binderfs.mount         /etc/systemd/system/dev-binderfs.mount
sudo cp binderfs-perms.service /etc/systemd/system/binderfs-perms.service
sudo systemctl daemon-reload
sudo systemctl enable --now dev-binderfs.mount
sudo systemctl enable --now binderfs-perms.service   # chmod 0666 the binder nodes
```

> **Why 0666:** binderfs creates the nodes `0600 root:root`, but the Redroid container
> opens `/dev/binder*` as non-root (servicemanager=`system`, HALs=uid 9999). Left at 0600
> the opens fail `Permission denied`, servicemanager never becomes context manager, and
> boot loops forever. `binderfs-perms.service` relaxes them to Android's native 0666.
> The boot chain at reboot: modules-load → modprobe `devices=` → `dev-binderfs.mount`
> (3 nodes) → `binderfs-perms.service` (0666).

Verify (reboot-persistent without rebooting the host — it is the k3s single point + jump box):

```bash
cat /etc/modules-load.d/binder.conf                       # -> binder_linux
modprobe -c | grep -i binder                              # -> options binder_linux devices=...
lsmod | grep binder_linux                                 # module loaded
cat /sys/module/binder_linux/parameters/devices           # -> binder,hwbinder,vndbinder
systemctl is-enabled dev-binderfs.mount binderfs-perms.service   # -> enabled (x2)
ls -l /dev/binderfs/binder                                # -> crw-rw-rw- (0666)
# remount cycle proves the unit alone reproduces the three nodes (run BEFORE the pod is up,
# else the container's bind-mounts hold binderfs busy):
sudo systemctl stop dev-binderfs.mount && ls /dev/binderfs/ 2>/dev/null   # (empty)
sudo systemctl start dev-binderfs.mount && ls /dev/binderfs/              # binder hwbinder vndbinder
```

## 2. Modern platform-tools (host) — keeps adb shell v2 (exit_code/stderr semantics)

```bash
sudo apt-get update && sudo apt-get install -y adb
adb version            # host tools >= 34 on Ubuntu 24.04 (shell v2)
```
Fallback (official zip): download `platform-tools-latest-linux.zip` from
`https://dl.google.com/android/repository/platform-tools-latest-linux.zip`, unzip, and
put `adb` on PATH.

## 3. Redroid image — three-way fallback (any one path suffices)

k3s uses its bundled containerd; use `k3s ctr` / `k3s crictl` (namespace `k8s.io`).
Redroid tags are `NN.0.0-latest` (rolling) or dated `NN.0.0-YYMMDD` — a **bare `NN.0.0`
does not exist**. This deploy pins `13.0.0-240527` (Android 13); `-latest` is the alt.

```bash
TAG=13.0.0-240527

# Path 1 — docker.io direct. NOTE: this k3s redirects docker.io -> docker.nju.edu.cn
#          via /etc/rancher/k3s/registries.yaml (transparent pull-through mirror):
sudo k3s ctr -n k8s.io images pull docker.io/redroid/redroid:$TAG

# Path 2 — explicit CN mirror, then retag to the pod's docker.io reference.
#          (docker.m.daocloud.io is dead/403 as of 2026-07; docker.1ms.run works):
sudo k3s ctr -n k8s.io images pull docker.1ms.run/redroid/redroid:$TAG
sudo k3s ctr -n k8s.io images tag  docker.1ms.run/redroid/redroid:$TAG \
                                   docker.io/redroid/redroid:$TAG

# Path 3 — offline tar import. Elsewhere (a host with working Docker):
#   docker pull redroid/redroid:$TAG && docker save redroid/redroid:$TAG -o redroid.tar
#   scp redroid.tar user@<k3s-host>:  ; then on the host:
sudo k3s ctr -n k8s.io images import redroid.tar

# confirm it is resident:
sudo k3s crictl images | grep redroid
```

> This host acquired the image via **Path 1** (docker.io through the NJU mirror) in ~53s.

## 4. Deploy

```bash
kubectl apply -f namespace.yaml
kubectl apply -f redroid-pod.yaml
kubectl -n aura get pod redroid -w            # wait for Running (+ Ready via boot probe)
kubectl -n aura describe pod redroid          # no binder mount errors
```

## 5. Verify adb endpoint

```bash
adb connect localhost:5555
adb -s localhost:5555 wait-for-device
adb -s localhost:5555 shell getprop sys.boot_completed        # -> 1 (boot done)
adb -s localhost:5555 shell getprop ro.build.version.release  # -> Android version (e.g. 13)
```

First cold boot with software rendering can take a few minutes; poll `sys.boot_completed`
until it returns `1`.

## adb server lifecycle (R4)

`aura-node` shells out to the host `adb`. Keep the adb server up across host restarts by
running the node under systemd with `After=dev-binderfs.mount` (and, if a dedicated adb
keep-alive is desired, an `adb start-server` oneshot ordered `After=network-online.target`).
`AdbCli` (TASK-003) additionally auto-`adb connect` on `device offline/not found`.

## Teardown (aura ns only)

```bash
kubectl -n aura delete pod redroid            # leaves the aura namespace and its VMIs intact
```

## Sidecar form (M5 末交付 / M8 Redroid-as-EnvProvider 铺路)

Second deployment form (D3): aura-node runs as a **sidecar container in the same pod**
as Redroid, sharing the pod network namespace, so it reaches the device over
**localhost:5555** (never a bare PodIP) and is fully **kubelet-managed**. This is the
endpoint-stability prerequisite for the M8 EnvProvider-ization (pod-GVR adapter deferred
to M8 per D5); the hostPort form above stays the M5 MVP baseline.

| Repo file                    | Purpose                                                         |
|------------------------------|-----------------------------------------------------------------|
| `Dockerfile.node-sidecar`    | glibc image (base `ubuntu:22.04`) = aura-node + adb             |
| `entrypoint.sh`              | wait for in-pod adb `localhost:5555` boot, then start the node  |
| `build-sidecar.sh`           | reproducible build → k3s containerd import                      |
| `redroid-sidecar-pod.yaml`   | Redroid + node-sidecar in ONE pod (aura ns)                     |

> **Base is glibc, not musl/distroless-static.** `ldd` on the release binary shows it is
> glibc-dynamically linked (libdbus-1/libxcb/libsystemd/libc — the desktop-driver deps are
> NEEDED at load time even when only the android driver runs). `ubuntu:22.04` (glibc 2.35)
> matches the 252.25 build host and ships `adb`; `debian:12-slim` was rejected — bookworm
> has no `adb` candidate.

> **两形态并存 — run ONE at a time.** The sidecar pod embeds its OWN Redroid, and the binder
> context manager is a host singleton (both forms bind-mount the same host binderfs nodes),
> so run **either** `redroid-pod.yaml` (hostPort form, node on the host) **or**
> `redroid-sidecar-pod.yaml` — never both. M5 keeps the hostPort pod resident as the baseline;
> bring the sidecar up only for the sidecar-form smoke, then restore.

### 1. Build the image (reproducible)

The k3s host runs containerd only (no docker/buildah); the control-plane VM has docker.
`build-sidecar.sh` stages the context on the VM, `docker build` + `docker save`, the k3s
host pulls the tar (same 22.x segment) and `k3s ctr -n k8s.io images import`. Idempotent —
re-running produces a bit-identical image (same tag/sha), EXIT 0. Binary provenance: reuses
the M5-verified release binary (`--features grpc`, k3s:~/aura-m5/aura-node).

```bash
bash controller/deploy/redroid/build-sidecar.sh          # -> docker.io/library/aura-node-sidecar:m5 (~36 MiB)
# knobs: TAG=, BASE=, K3S_BIN=, AURA_NODE_BIN=<local override>
```

### 2. Cert secret (mTLS reverse-connect)

```bash
kubectl -n aura create secret generic aura-node-certs \
  --from-file=ca.crt=~/aura-m5/certs/ca.crt \
  --from-file=node.crt=~/aura-m5/certs/node.crt \
  --from-file=node.key=~/aura-m5/certs/node.key
```

### 3. Deploy (swap from the hostPort form) + verify + restore

```bash
kubectl -n aura delete pod redroid --wait=true          # only one Redroid at a time
kubectl -n aura apply  -f redroid-sidecar-pod.yaml
kubectl -n aura get pod redroid-sidecar -w              # -> 2/2 Running (redroid + node-sidecar)

# node reverse-registers platform=android (control plane <controller-host>:7443):
auractl ... node list                                   # -> <id> android online
# same closed loop as the hostPort form (screenshot→a11y→click→type→assert):
python3 tools/m5-e2e.py android <NODE_ID>               # -> 子序 A 总判定: PASS

# restore the hostPort baseline:
kubectl -n aura delete pod redroid-sidecar --wait=true
kubectl -n aura delete secret aura-node-certs
kubectl -n aura apply -f redroid-pod.yaml
```

> **data_dir seam (M8).** The sidecar pins `--data-dir /var/lib/aura` (M2 lesson: no HOME in
> a container/systemd unit). Device-side large artifacts (`screenrecord`, deferred to M8) must
> be `adb pull`-bridged to that host-local dir — see the seam comment at the head of
> `node/crates/aura-platform/src/android.rs`.
