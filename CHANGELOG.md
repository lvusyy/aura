# Changelog

All notable changes to this project are documented in this file.
The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project adheres to [Semantic Versioning](https://semver.org/).

本文件记录项目的重要变更,格式遵循 Keep a Changelog,版本号遵循语义化版本。

## [0.1.0] - 2026-07-18

First public release. 首次公开发布。

### Added

- **`aura-node`** (Rust, single static binary): embedded MCP server with dual transports — `stdio` and stateless Streamable HTTP (`/mcp`); 21 tools covering screenshot, mouse/keyboard/text injection, accessibility-tree read, assertions (text/a11y/image), file push/pull, process & command control, screen recording and audio; per-platform capability advertisement at call time; XGA screenshot scaling with click-coordinate back-mapping.
- **Platform drivers**: Windows, Linux (X11), macOS, Android (Redroid via adb), iOS Simulator (WebDriverAgent) — one capability contract, per-platform subsets.
- **`aura-controller`** (Go): fleet registry, scheduler, task dispatch; nodes reverse-connect over mTLS gRPC (NAT-friendly); environment provisioning for Proxmox VE and Kubernetes; PostgreSQL/Redis/MinIO backing stores with graceful in-memory degradation; audit logging; Prometheus metrics + OpenTelemetry tracing; HA dual-replica with owner routing and cross-replica forwarding.
- **Device onboarding**: one-command enrollment — CSR-based per-node certificates, private keys never leave the node; certificate renewal and revocation; 30-day offline node reaping.
- **Recording & replay**: screen recording with streaming playback (HTTP Range), MinIO lifecycle retention; trace capture and `auractl replay`.
- **Visual fusion**: optional OmniParser-based detector service augmenting the accessibility tree with vision-detected UI elements.
- **Web console** (React + Ant Design, embedded via `go:embed`): fleet dashboard with infrastructure labels, task history, recording playback, orchestration wall, live desktop entry (Selkies WebRTC), device onboarding page.
- **Direct-access observability**: per-call MCP activity of directly-connected agents reported back to the controller; console page with per-agent-client breakdowns and verified access guides for 11 coding agents (Claude Code, Codex CLI, Gemini CLI, OpenCode, OpenClaw, Cline, Hermes, CodeBuddy, Kimi Code, Grok Build, Pi via `auractl`).
- **Access control**: three-tier bearer tokens on the REST plane (admin/ops/read-only); optional `AURA_MCP_TOKEN` gate on the node `/mcp` endpoint (open by default for lab use).
- **`auractl`** CLI: task dispatch, environment lifecycle, artifact fetch, trace replay against the controller REST plane.
- Prebuilt binaries: `aura-node` (Windows x64 / Linux x64 / macOS arm64), `aura-controller` + `auractl` (Linux x64, console embedded), `auractl` (Windows x64).

[0.1.0]: https://github.com/lvusyy/aura/releases/tag/v0.1.0
