# Coop

Coop is a terminal session manager for AI coding agents. It wraps agent CLIs in a PTY (or
compatibility layer), monitors their state, and exposes a gRPC API for orchestration.

## Build & Test

```bash
make check    # fmt + clippy + quench + build + test (run this before completing work)
make ci       # full pre-release (adds e2e, audit, deny)
cargo test    # unit tests only
```

**CI is GitHub Actions** (the old `coop/.rwx/` and root `.rwx/` pipelines were deleted
2026-09-16; there is no `rwx` CLI step any more). Coop's signal comes from the repo-root
workflows; just open a PR or push:

- `.github/workflows/ci.yml` `build + test (coop)` — on PRs and pushes to `main` that
  touch `coop/**`: `cargo fmt --check`, `clippy -D warnings`, `cargo build --workspace`,
  `cargo test --workspace`.
- `.github/workflows/checks.yml` — `cargo audit + deny (coop)` (advisory, same path gate)
  and the root `quench check --ci`.
- `.github/workflows/e2e.yml` `coop-e2e (mux + fidelity)` — Playwright against a debug
  `coopmux`. Optional: runs on an `e2e` PR label, the nightly cron, or
  `gh workflow run e2e.yml -f suites=coop`.
- `.github/workflows/images.yml` builds the `coopmux` and `coop-claude` images and
  publishes them to ECR on a calver tag.

**Gotcha — you cannot run `make try-claudeless` yourself**: it opens a browser terminal.
When debugging claudeless scenarios (`crates/cli/tests/scenarios/*.toml`) for hook
detection, state transitions, or TUI rendering, ask the human to run it and report what
they see.

## Code Conventions

- **License**: BUSL-1.1, Copyright Matt Baker <fimbaz@gmail.com>. Every source file needs
  the SPDX header.
- **No unwrap/expect/panic in source**: clippy denies `unwrap_used`, `expect_used`, `panic`
  (and quench enforces `\.unwrap\(\)` = forbid outside tests). Use `?`, `anyhow::bail!`, or
  `.ok()`. Tests use `-> anyhow::Result<()>`.
- **Unsafe**: forbidden workspace-wide.
- **Rust 1.92+**: native `async fn` in traits — no `async_trait` macro.
- Unit tests in `*_tests.rs` with `#[cfg(test)] #[path = "..."] mod tests;`.

## Architecture

- `run::prepare()` sets up the full session (driver, backend, servers) and returns a
  `PreparedSession` with access to `Store` before the session loop starts. `run::run()` is
  the thin wrapper that calls `prepare().run()`.
- **Claude driver detection has five tiers**, hooks primary: Tier 1 hook FIFO
  (`hook_detect.rs`), Tier 2 session log (`log_watch.rs`), Tier 3 stdout JSONL
  (`stdout_detect.rs`), Tier 4 process monitor, Tier 5 screen parse. `composite.rs` resolves
  by tier priority.
- Session artifacts (FIFO pipe, settings) live at
  `$XDG_STATE_HOME/coop/sessions/<session-id>/` for debugging and recovery.
- Integration tests use **claudeless** (scenario-driven Claude CLI mock): call
  `run::prepare()`, subscribe to state broadcasts, spawn the session, `wait_for` expected
  states, then cancel shutdown. Keep agent-specific code in `driver/<agent>/`; `run.rs` and
  `session.rs` stay agent-agnostic.

### Credential flow (coopmux)

The gardenio controller is the single source of truth — full architecture in
`gardenio/CLAUDE.md` → **Credential Architecture**. The coop-side facts that live nowhere
else:

- A session reports its `account_name` on register (HTTP or NATS), seeding
  `assigned_account` on the `SessionEntry` — the single account-identity handshake point.
- The health poller reads `account_name` from each health response to track profile
  switches (failover/rotation).
- coopmux's one account-bead write (marking a throttled account `status=exhausted`,
  `credential/failover.rs`) runs as coopmux's own **machine principal** when
  `BEADS_OIDC_TOKEN_URL`, `BEADS_OIDC_CLIENT_ID` and `BEADS_OIDC_CLIENT_SECRET_FILE` are all
  set (`crates/mux/src/machine_token.rs`, the Rust mirror of the controller's
  `beadsapi/machine_token.go`: client-credentials grant, half-life refresh, secret file
  re-read, fail closed with `auth_unavailable`, no static fallback). Beads reads use it too
  (static `BEADS_TOKEN` only when it is not configured). coopmux never writes the
  `config:auth` bead: the controller records the failover (gd-HYilfX1Bk8). Chart switch: `coopmux.machinePrincipal.enabled`.

## Directory Structure

```toc
crates/cli/                   # Single crate (binary + lib)
├── src/main.rs               # CLI, startup
├── src/run.rs                # prepare() + run() session entrypoint
├── src/backend/              # Backend trait (NativePty forkpty+exec, tmux adapter, nbio)
├── src/transport/            # Router builder + Store (http/, ws.rs, grpc/ under here)
└── src/driver/               # AgentState, Detector, tier detectors, claude/ codex/ gemini/ unknown/
crates/mux/                   # coopmux multi-session dashboard
crates/web/                   # Web UI (Vite + React) → single-file dist/{terminal,mux}.html (committed)
tests/specs/                  # Binary smoke tests (spawn real coop, exercise transports)
tests/e2e/                    # Playwright mux dashboard tests (mock coop servers)
```

## Working Style

- When multiple approaches exist or intent is unclear, prefer the smallest reversible step
  and note the open question in your work log — do **not** call `AskUserQuestion`/blocking
  prompts; they hang the agent session (see the global workflow rules).
- Prefer end-to-end testing through the real `run()` codepath over manual library wiring;
  tests should be trivial to read.

## Commits

Conventional commit format: `type(scope): description` (feat, fix, chore, docs, test,
refactor).

> **Gotcha:** `crates/web/dist/terminal.html` and `crates/web/dist/mux.html` are always
> rebuilt together — commit both even if only one page changed.

## Landing the Plane

1. `make check` — all fmt, clippy, quench, build, and test steps must pass.
2. New source files have SPDX license headers.
3. Commit with conventional commit format (both `dist/*.html` if either changed).
