# AURA — Agent-User Remote Automation

**English** | [简体中文](README.zh-CN.md)

**Give AI coding agents real eyes and hands on real devices.**

AURA is self-hosted infrastructure that lets coding agents (Claude Code, Codex CLI, Gemini CLI, …) remotely drive real or virtual test machines over [MCP](https://modelcontextprotocol.io) — screenshot → click → type → read back → verify — to catch the UI, interaction and UX bugs that unit tests and code review structurally cannot see.

```
┌──────────────┐  MCP over HTTPS + bearer (gateway)  ┌──────────────────────────────┐
│ Coding agent │ ──────────────────────────────────► │ aura-controller (Go, HA)     │
│ Claude Code  │  REST / web console                 │ MCP gateway · scheduler      │
│ Codex / …    │ ──────────────────────────────────► │ fleet · recordings · console │
└──────┬───────┘   the only exposed surface          └──────────────┬───────────────┘
       │                                                  mTLS gRPC │ reverse conn
       │ lab-only shortcut: direct MCP            (nodes dial out — │  NAT-friendly)
       │ (stdio / plain Streamable HTTP)                            ▼
       └─────────────────────────────────► ┌────────────────────────────────────────┐
                                           │ aura-node (Rust, 1 bin, private net)   │
                                           │ screenshot / input / a11y tree /       │
                                           │ assert / files / process / recording   │
                                           └────────────────────────────────────────┘
                                             + PostgreSQL / Redis / MinIO (internal)
```

Test machines and backing stores stay on a private network; the controller is the single TLS + bearer-token entry point — agents reach internal nodes through its **MCP gateway** over the nodes' outbound mTLS reverse connections, so nothing on the test network is ever exposed. Direct node access remains available as a zero-infra shortcut for same-segment lab use.

## Why

Coding agents are good at writing code and running tests — but "does the app actually work for a human" lives on the other side of the screen: layout glitches, dead buttons, focus traps, broken flows after a resize. AURA closes that loop by giving the agent a **user-perspective test seat**: a real desktop or mobile environment it can see and operate, with structured verification (accessibility tree + assertions) instead of guesswork.

## Highlights

- **One binary per node.** `aura-node` (Rust) embeds an MCP server with both transports: `stdio` for local child-process use and stateless **Streamable HTTP** (`/mcp`) for remote agents. No runtime dependencies.
- **21 tools** across screenshot, mouse/keyboard/text injection, accessibility-tree read, assertions (text / a11y / image), file push/pull, process & command control, screen recording, audio — advertised per-platform at call time.
- **5 platforms.** Windows, Linux (X11), macOS on real machines; Android via [Redroid](https://github.com/remote-android/redroid-doc) on Kubernetes; iOS Simulator via WebDriverAgent. One capability contract, per-platform subsets.
- **Agent-accurate coordinates.** Screenshots are delivered XGA-scaled with click-coordinate back-mapping, following Anthropic's computer-use guidance — the detail that decides whether clicks land.
- **Two deployment shapes.**
  - *Direct (lab):* point your agent at a single node's `/mcp` on the same network segment and start testing in minutes — zero infrastructure.
  - *Cluster (production):* `aura-controller` (Go) adds the **MCP gateway** (agents reach any internal node via `https://<controller>/v1/mcp/<node-id>` — one TLS + bearer entry, nodes stay unexposed), fleet management, scheduling, environment provisioning (Proxmox VE / Kubernetes), artifact & recording storage, trace replay, and a web console — nodes dial **out** to the controller over mTLS gRPC, so test machines behind NAT just work.
- **Batteries-included web console.** Device wall, live operate seat, task orchestration, recording playback, step-by-step replay and agent observability — embedded in the controller binary, nothing extra to deploy. See [Web console](#web-console).
- **Fleet operations.** One-command node enrollment (CSR → per-node certificate, private key never leaves the node), fleet dashboard, task history, streaming recording playback, HA dual-replica controller, Prometheus metrics + OpenTelemetry traces.
- **Direct-access observability.** Nodes report per-call MCP activity of directly-connected agents back to the controller, so the console shows who is testing what, with per-agent-client breakdowns and access guides.
- **Access control that scales down.** A single node is open by default for frictionless lab use; set `AURA_MCP_TOKEN` to require a bearer token on `/mcp`. The controller REST plane has three token tiers (admin / ops / read-only). See `controller/deploy/ENV.md`.

## Supported coding agents

Verified configuration guides for each are built into the web console (Agents page):

| Agent | Connects via |
|---|---|
| Claude Code | `claude mcp add --transport http aura http://<node>:7100/mcp` |
| Codex CLI | `codex mcp add aura --url http://<node>:7100/mcp` |
| Gemini CLI | `settings.json` → `"type": "http"` |
| OpenCode | `opencode.json` → `"type": "remote"` |
| OpenClaw | `openclaw.json` → `"transport": "streamable-http"` |
| Cline | `"type": "streamableHttp"` (≥ 3.17.14) |
| Hermes Agent | `~/.hermes/config.yaml` → `mcp_servers` |
| CodeBuddy | `codebuddy mcp add-json …` |
| Kimi Code | `kimi mcp add -t http …` / `/mcp-config` |
| Grok Build | `.grok/config.toml` → `[mcp_servers.aura]` |
| Pi | no MCP by design → use the `auractl` CLI |

Any other MCP client that speaks Streamable HTTP (or spawns a stdio server) works the same way.

## Web console

The controller ships with a full management console — React + Ant Design, embedded in the binary via `go:embed` and served at `https://<controller-host>:18080/console` with bearer-token login. Nothing extra to deploy. Page by page:

- **Fleet** — the device wall: every node is a card with a platform badge, live health state (online / unhealthy / offline) and a hover popover listing its actual tool capabilities grouped by category. Infrastructure labels distinguish bare metal, VMs, containers and Kubernetes pods, with host attribution and search. The add-device dialog generates a one-command install line for new nodes; node metadata is editable in place; deletion is guarded so a live node can't be dropped by accident.
- **Operate** — a live seat on any online node: a screenshot canvas with adjustable polling (1–10 s), click-through with XGA coordinate back-mapping, and text injection — every action answered with per-call ok/error feedback and tagged with an audit identity. Android nodes can switch the canvas to a low-latency scrcpy/WebCodecs live stream; containerized Linux desktops open a Selkies WebRTC session in a new tab.
- **Tasks** — dispatch history with cursor pagination and per-node filtering, plus the orchestration wall: fan a tool sequence out across hand-picked nodes or an entire environment group, watch pass/fail buckets fill in live, and drill down into each constituent task.
- **Agents** — observability for directly-connected MCP agents: sessions and per-call activity with per-client color coding, a 24-hour transport error rate, and built-in copy-paste access guides for all 11 supported coding agents.
- **Recordings** — screen recordings as streamable MP4 (HTTP Range, instant seeking), filterable by node, with a 30-day retention policy.
- **Replay** — structured step-by-step playback of recorded traces: every tool call with its arguments and screenshot, optionally re-dispatched live against a node with a PASS / FAIL / UNSUPPORTED verdict per step.

Recordings and Replay complement each other: one is a video of what the screen showed, the other is the structured film of what the agent actually did — and lets you run it again.

Console access follows the REST token tiers (admin / ops / read-only): a read-only token can browse but not dispatch.

## Quickstart — single node, 5 minutes

**Option A — prebuilt binary (recommended):** grab `aura-node` for your platform (Windows x64 / Linux x64 / macOS arm64) from [Releases](https://github.com/lvusyy/aura/releases), unpack, and jump to step 2.

**Option B — build from source** (requires Rust ≥ 1.95 and `protoc`):

```bash
# Feature flags matter — they compile the reverse-connect, enrollment and OTel
# surfaces; a bare build produces a stdio/http-only binary.
cd node
cargo build --release -p aura-node --features grpc,enroll,otel
```

Then:

```bash
# 2. Serve MCP over Streamable HTTP on the test machine
./aura-node http --bind 0.0.0.0:7100

# 3. Connect an agent from your workstation, e.g. Claude Code:
claude mcp add --transport http aura http://<test-machine>:7100/mcp

# …or run it as a local stdio child process instead of a server:
claude mcp add aura -- /path/to/aura-node stdio
```

Then ask the agent to `screenshot`, click, type, and `assert` its way through your app.

Optional access token: start the node with `AURA_MCP_TOKEN=<secret>` in its environment and add `--header "Authorization: Bearer <secret>"` (or your client's equivalent) on the agent side. For anything beyond a trusted lab segment, front `/mcp` with TLS (reverse proxy) or keep it on a private network/VPN.

## Cluster deployment

1. **Backing services** — `controller/deploy/compose.yml` brings up PostgreSQL, Redis and MinIO (change the placeholder passwords).
2. **Certificates** — `controller/deploy/gen-certs.sh` generates the CA and server certificates for the mTLS gRPC plane (edit `CTRL_IP`).
3. **Controller** — `cd controller && go build ./cmd/aura-controller`, or use the prebuilt Linux binary from [Releases](https://github.com/lvusyy/aura/releases). All configuration is environment variables; the authoritative list with defaults is [`controller/deploy/ENV.md`](controller/deploy/ENV.md).
4. **Console** — `cd console && npm install && npm run generate && npm run build`, then rebuild the controller (the build output is embedded via `go:embed`; the prebuilt binary ships with the console already embedded).
5. **Nodes** — install with `controller/deploy/install/install.sh` (Linux/macOS) or `install.ps1` (Windows), or enroll manually: `aura-node enroll` performs CSR-based enrollment against the controller, then the node reverse-connects with its per-node certificate. The console's onboarding page generates the one-command install line for you.

Once the controller is up, the [web console](#web-console) is at `https://<controller-host>:18080/console` — log in with a bearer token (tiers in `ENV.md`).

6. **Connect agents through the MCP gateway** — the production path. Each node's MCP surface is reachable at `https://<controller-host>:18080/v1/mcp/<node-id>` with an admin-scope bearer token; the console's Agents page shows a copy-ready URL per node. Nodes need `http --bind 127.0.0.1:7100` alongside the reverse connection (loopback is enough — the gateway rides the mTLS reverse stream, nothing on the test network is exposed). Protocol semantics are byte-identical to direct access: the gateway forwards raw JSON-RPC to the node's own MCP server. Example:

```bash
claude mcp add --transport http aura https://<controller-host>:18080/v1/mcp/<node-id> \
  --header "Authorization: Bearer <admin-token>"
```

Reference manifests for optional components live under `controller/deploy/`: Redroid Android environments (`redroid/`), Selkies WebRTC container desktops (`selkies/`), coturn (`turn/`), and the OmniParser-based visual detector service (`detector/`) that augments the accessibility tree with vision-detected UI elements.

## Repository layout

```
node/        Rust workspace — aura-node binary, platform drivers, capability contract
controller/  Go control plane — gateway/scheduler/registry/storage + embedded console
console/     Web console source (React + Ant Design + Vite; builds into controller)
proto/       Single proto source of truth (buf; Go/TS generated code is committed,
             Rust bindings are generated at build time by tonic-build)
```

Regenerating protocol code after editing `proto/aura/v1/*.proto`: `cd proto && buf generate` (uses BSR remote plugins; version-pinned in `buf.gen.yaml`).

## Platform notes

- **Windows** — run `aura-node` in an interactive logon session (not as a service): screenshot/input APIs are unavailable in session 0. Recording uses Windows Graphics Capture.
- **Linux** — X11 is the supported default (Wayland is experimental by design: portal prompts and compositor fragmentation). Headless boxes work well with Xvfb or a virtual display.
- **macOS** — grant Screen Recording + Accessibility (TCC) to the binary; recording and injection are gated by those permissions.
- **Android** — Redroid containers on Kubernetes with an `aura-node` sidecar (`controller/deploy/redroid/`); input/capture via adb.
- **iOS** — Simulator driven through WebDriverAgent; screen recording is intentionally excluded from the iOS capability subset.

## Security model in one paragraph

Nodes are meant to control **disposable test machines, not production hosts** — treat every node as an arbitrary-code-execution surface and isolate it accordingly (VM, VLAN, firewall). In the cluster shape the controller is the **only exposed surface**: agents enter through the MCP gateway / REST / console over TLS with bearer tokens (three tiers; the gateway requires admin scope), and controller ↔ node traffic is mTLS with per-node certificates over node-initiated connections — test machines accept no inbound connections at all (bind the node's `/mcp` to loopback). Direct node access is a lab convenience for trusted same-segment networks: open by default, gated by `AURA_MCP_TOKEN` when set, plaintext HTTP — keep it off untrusted networks. All dispatched and gateway calls are audit-logged in the cluster shape.

## Status

Actively developed and used against a real mixed fleet (Windows / Linux / macOS / Android / iOS-sim). APIs may still move; the proto contract is versioned and changes have been additive so far. Issues and PRs welcome.

## Bilingual docs

`README.md` (English) and [`README.zh-CN.md`](README.zh-CN.md) (简体中文) mirror each other — **edit one, sync the other** (enforced by CI).

## License

See [LICENSE](LICENSE) (Apache-2.0).
