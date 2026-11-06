# Contributing

## License

By contributing, you acknowledge that contributions are licensed under BSL 1.1 (see LICENSE).

## Requirements

- **Rust 1.92+** — install via [rustup](https://rustup.rs/)
- **protoc** — Protocol Buffers compiler (needed by `prost-build` for gRPC codegen)
  - macOS: `brew install protobuf`
  - Debian/Ubuntu: `apt install protobuf-compiler`
- **[quench](https://github.com/alfredjeanlab/quench)** — fast linting tool for quality signals, used by `make check`
- **[claudeless](https://github.com/alfredjeanlab/claudeless)** — Claude CLI mock for integration tests and manual testing

**Optional:**

- **Docker** — for `make test-docker` and `try-docker-*` targets
- **Claude CLI** / **Gemini CLI** — only for `make try-claude` / `make try-gemini`

## Build & Test

```bash
cargo build           # build only (requires Rust + protoc)
cargo test            # unit + integration tests (integration tests need claudeless)
make check            # fmt + clippy + quench + build + test
make ci               # full pre-release (adds audit + deny)
```

## Code Conventions

- License: BUSL-1.1 — every source file needs the SPDX header
- Clippy: `unwrap_used`, `expect_used`, `panic` are denied; use `?`, `anyhow::bail!`, or `.ok()`
- Unsafe: forbidden workspace-wide
- Tests: use `-> anyhow::Result<()>` return type instead of unwrap
- Commits: conventional format — `type(scope): description`

## Context

See `CLAUDE.md` for architecture overview.
