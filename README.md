# AURA — Agent-User Remote Automation

**English** | [简体中文](README.zh-CN.md)

**Give AI coding agents real eyes and hands on real devices.**

AURA is self-hosted infrastructure that lets coding agents (Claude Code, Codex CLI, Gemini CLI, …) remotely drive real or virtual test machines over [MCP](https://modelcontextprotocol.io) — screenshot → click → type → read back → verify — to catch the UI, interaction and UX bugs that unit tests and code review structurally cannot see.

```
┌──────────────┐   MCP (stdio / Streamable HTTP)   ┌─────────────────────────┐
│ Coding agent │ ────────────────────────────────► │ aura-node (Rust, 1 bin) │
│ Claude Code  │                                   │ screenshot / input      │
│ Codex / …    │        direct, zero-infra         │ a11y tree / assert      │
└──────┬───────┘                                   │ files / process / rec   │
       │ REST / console                            └───────────┬─────────────┘
       ▼                                             mTLS gRPC │ reverse conn
┌─────────────────────────────────────────────┐                │ (NAT-friendly)
│ aura-controller (Go, HA-ready)              │ ◄──────────────┘
│ scheduler · fleet registry · enrollment     │
│ recordings · traces · web console (embedded)│   + PostgreSQL / Redis / MinIO
└─────────────────────────────────────────────┘
```

## Why

Coding agents are good at writing code and running tests — but "does the app actually work for a human" lives on the other side of the screen: layout glitches, dead buttons, focus traps, broken flows after a resize. AURA closes that loop by giving the agent a **user-perspective test seat**: a real desktop or mobile environment it can see and operate, with structured verification (accessibility tree + assertions) instead of guesswork.

## Highlights

- **One binary per node.** `aura-node` (Rust) embeds an MCP server with both transports: `stdio` for local child-process use and stateless **Streamable HTTP** (`/mcp`) for remote agents. No runtime dependencies.
- **21 tools** across screenshot, mouse/keyboard/text injection, accessibility-tree read, assertions (text / a11y / image), file push/pull, process & command control, screen recording, audio — advertised per-platform at call time.
- **5 platforms.** Windows, Linux (X11), macOS on real machines; Android via [Redroid](https://github.com/remote-android/redroid-doc) on Kubernetes; iOS Simulator via WebDriverAgent. One capability contract, per-platform subsets.
- **Agent-accurate coordinates.** Screenshots are delivered XGA-scaled with click-coordinate back-mapping, following Anthropic's computer-use guidance — the detail that decides whether clicks land.
- **Two deployment shapes.**
  - *Direct:* point your agent at a single node's `/mcp` and start testing in minutes.
  - *Cluster:* `aura-controller` (Go) adds fleet management, scheduling, environment provisioning (Proxmox VE / Kubernetes), artifact & recording storage, trace replay, and a web console — nodes dial **out** to the controller over mTLS gRPC, so test machines behind NAT just work.
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

Nodes are meant to control **disposable test machines, not production hosts** — treat every node as an arbitrary-code-execution surface and isolate it accordingly (VM, VLAN, firewall). Controller ↔ node traffic is mTLS with per-node certificates. The REST/console plane requires bearer tokens (three tiers). Direct node access (`/mcp`) is open by default for lab ergonomics and gated by `AURA_MCP_TOKEN` when set; put it behind TLS or a private network for anything sensitive. All dispatched tool calls are audit-logged in the cluster shape.

## Status

Actively developed and used against a real mixed fleet (Windows / Linux / macOS / Android / iOS-sim). APIs may still move; the proto contract is versioned and changes have been additive so far. Issues and PRs welcome.

## Bilingual docs

`README.md` (English) and [`README.zh-CN.md`](README.zh-CN.md) (简体中文) mirror each other — **edit one, sync the other** (enforced by CI).

## License

See [LICENSE](LICENSE) (Apache-2.0).
