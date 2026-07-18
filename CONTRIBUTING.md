# Contributing to AURA

Thanks for your interest! This page covers the essentials. 欢迎贡献!以下是要点。

## Bilingual docs rule / 双语文档规则

`README.md` (English) and `README.zh-CN.md` (简体中文) are mirrors. **Any PR that changes one must change the other in the same PR** — CI fails otherwise. Keep the section structure identical; translate meaning, not word-for-word.

`README.md` 与 `README.zh-CN.md` 互为镜像:**改动其一必须在同一 PR 内同步另一份**(否则 CI 失败)。保持章节结构一致,译意不逐字。

## Toolchain / 工具链

| Component | Requirement |
|---|---|
| `node/` (Rust) | Rust ≥ 1.95, `protoc` on PATH (or `PROTOC` env) |
| `controller/` (Go) | Go ≥ 1.25 |
| `console/` (TS) | Node.js ≥ 20 |
| `proto/` | [buf](https://buf.build) CLI (only when editing `.proto`) |

## Build & test / 构建与测试

```bash
# Node — feature flags matter; a bare build omits reverse-connect/enroll/OTel
cd node && cargo build --release -p aura-node --features grpc,enroll,otel
cargo test --workspace

# Controller
cd controller && go build ./... && go test ./...

# Console
cd console && npm install && npm run typecheck && npm run build
```

Platform-specific driver code (`node/crates/aura-platform`) only fully compiles on its target OS; CI covers the Go/TS surfaces, please build Rust changes on the OS you are targeting. Rust 平台驱动代码须在目标 OS 上构建验证。

## Protocol changes / 协议变更

`proto/aura/v1/*.proto` is the single source of truth. After editing:

```bash
cd proto && buf generate   # regenerates controller/gen + console/src/gen (committed)
```

Generated Go/TS code is committed; Rust bindings are generated at build time by `tonic-build`. Keep changes **additive** (new fields/RPCs, no renumbering) — the fleet upgrade story depends on it. proto 变更须保持 additive(只加字段/RPC,不改号)。

## Commit style / 提交风格

`type: short description` — types: `feat` / `fix` / `refactor` / `docs` / `test` / `chore`. English or Chinese both fine. 中英文提交信息皆可。

## Code style / 代码风格

- Rust: `rustfmt` grouping (std → external → crate); platform code gated by `#[cfg(target_os = "...")]` under `platform/`.
- Go: `goimports` ordering (stdlib → external → internal).
- Match the comment language and density of the file you are editing. 注释语言与所在文件保持一致。
