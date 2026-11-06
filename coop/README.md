# Coop

A terminal session manager for AI coding agents. Spawns agent CLIs on a PTY, classifies agent state via structured data, and serves everything over your choice of HTTP, WebSocket, or gRPC.

Coop replaces tmux/screen-based agent orchestration with a proper API. Instead of `capture-pane` and `send-keys`, consumers get structured endpoints for screen state, agent state detection, nudging idle agents, and answering prompts.

## Install

```bash
scripts/install    # builds and installs to ~/.local/bin/
```

Or build manually:

```bash
cargo build --release
```

## Usage

```bash
# Claude basics
coop --port 8080 claude --dangerously-skip-permissions

# Serve on a Unix socket
coop --socket /tmp/coop.sock -- claude

# Dumb PTY server (no driver)
coop --port 8080 -- /bin/bash

# Attach to an existing tmux session
coop --agent claude --attach tmux:my-session --port 8080

# Enable gRPC alongside HTTP
coop --port 8080 --port-grpc 9090 -- claude

# Resume a previous Claude session
coop --port 8080 --resume <session-id> -- claude
```

## API

Once coop is running, consumers interact with agents over HTTP or gRPC:

```bash
# Check agent state
curl localhost:8080/api/v1/agent

# Give the agent a task
curl -X POST localhost:8080/api/v1/agent/nudge \
  -d '{"message": "Fix the login bug"}'

# Accept a permission prompt
curl -X POST localhost:8080/api/v1/agent/respond \
  -d '{"accept": true}'

# View the terminal screen
curl localhost:8080/api/v1/screen/text

# Stream events over WebSocket
websocat ws://localhost:8080/ws?subscribe=state
```

### Endpoints

| Method | Path | Description |
|--------|------|-------------|
| GET | `/api/v1/health` | Health check |
| GET | `/api/v1/ready` | Readiness probe |
| GET | `/api/v1/screen` | Rendered screen (JSON) |
| GET | `/api/v1/screen/text` | Plain text screen |
| GET | `/api/v1/output` | Raw ring buffer |
| GET | `/api/v1/status` | Process status |
| POST | `/api/v1/input` | Send text input |
| POST | `/api/v1/input/raw` | Send raw bytes (base64) |
| POST | `/api/v1/input/keys` | Send key sequences |
| POST | `/api/v1/resize` | Resize terminal |
| POST | `/api/v1/signal` | Signal child process |
| GET | `/api/v1/agent` | Agent state + prompt context |
| POST | `/api/v1/agent/nudge` | Deliver message to idle agent |
| POST | `/api/v1/agent/respond` | Answer agent prompt |
| POST | `/api/v1/hooks/stop` | Stop hook verdict |
| POST | `/api/v1/stop/resolve` | Resolve pending stop |
| GET/PUT | `/api/v1/config/stop` | Stop configuration |
| POST | `/api/v1/hooks/start` | Start hook context injection |
| GET/PUT | `/api/v1/config/start` | Start configuration |
| POST | `/api/v1/shutdown` | Graceful shutdown |
| GET | `/ws` | WebSocket (output, screen, state, hooks, messages) |

gRPC is also available when `--port-grpc` is set, mirroring the HTTP endpoints with streaming RPCs for output, screen, and state.

## Agent Drivers

Coop uses structured data sources (not screen scraping) to classify agent state:

| Agent | Maturity | Hooks | Session log | Stdout JSONL | Process |
|-------|----------|-------|-------------|--------------|---------|
| `claude` | Beta | PostToolUse, Stop | `~/.claude/sessions/` | `--print --output-format stream-json` | Yes |
| `codex` | Alpha (dogfooded in gardenio, [ADR 0004](../docs/adr/0004-codex-harness.md)) | SessionStart, UserPromptSubmit, PreToolUse, PostToolUse, Stop (needs trusted `hooks.json`) | `$CODEX_HOME/sessions/` (rollout) | `app-server` JSON-RPC, `exec --json` | Yes (TUI only) |
| `gemini` | Pre-alpha | AfterTool, SessionEnd | `~/.gemini/tmp/` | `stream-json` | Yes |
| `unknown` | Experimental | -- | -- | -- | Yes |

**Codex** has no terminal UI to scrape (consumers render coop's normalised transcript instead of a screen; operator guide: [`gardenio/docs/harness-codex.md`](../gardenio/docs/harness-codex.md)). coop picks a mode from the command line
(`driver/codex/mod.rs`, design in `docs/adr/0004-codex-harness.md`):

| Command | Mode | How coop drives it |
|---------|------|--------------------|
| `codex app-server` | app-server | JSON-RPC client over stdio (`backend/codex.rs`). Nudge = `turn/steer` (or `turn/start` when idle), Escape / Ctrl-C = `turn/interrupt`, `--resume <session-id>` = `thread/resume`. Sessions run `approvalPolicy: never` + `danger-full-access`; any approval request is declined and reported as an `error` state. |
| `codex exec ...` | exec | One `codex exec [resume <id>] --json -` process per nudge (prompt on stdin); the thread id is learned from `thread.started`. Escape kills the running turn. |
| anything else | tui | Plain PTY, typed input, hooks + process monitor only. |

State comes from the protocol (`turn/started`, `turn/completed`, `error`) or the `--json` stream
(`turn.started`, `turn.completed`, `turn.failed`), and Codex's typed error classes map onto the
existing error categories (`unauthorized`, `rate_limited`, `budget_exceeded`, `server_error`, ...).
The screen shows a compact transcript rather than a TUI. Hook installation and trust seeding in
`$CODEX_HOME` are the pod-init step's job; coop only generates the fragment
(`COOP_CODEX_HOOKS_JSON`).

Agent states: `starting`, `working`, `idle`, `prompt`, `error`, `exited`, `unknown`. Prompt subtypes: `permission`, `plan`, `question`, `setup`.

## Development

### Requirements

- **Rust 1.92+** — install via [rustup](https://rustup.rs/)
- **protoc** — Protocol Buffers compiler (used by `prost-build` for gRPC codegen)
  - macOS: `brew install protobuf`
  - Debian/Ubuntu: `apt install protobuf-compiler`
- **[quench](https://github.com/alfredjeanlab/quench)** — fast linting tool for quality signals, used by `make check`
- **[claudeless](https://github.com/alfredjeanlab/claudeless)** — Claude CLI mock, used for integration tests and manual testing

Optional:

- **[Bun](https://bun.sh/)** — used by `tests/debug/` scripts for manual testing targets (`try-*`, `capture-*`)
- **Docker** — used for `make try-k8s` (local Kubernetes testing, pulls images from GHCR)
- **Claude CLI** / **Gemini CLI** — only needed for `make try-claude` / `make try-gemini` manual testing

### Commands

```bash
make check    # fmt + clippy + quench + build + test
make ci       # full pre-release (adds audit + deny)
cargo test    # unit tests only
```

### Manual testing

Requires [Bun](https://bun.sh/).

```bash
# Launch coop + claudeless in a browser terminal (requires claudeless)
make try-claudeless SCENARIO=crates/cli/tests/scenarios/claude_hello.toml

# Launch coop + real agent CLI in a browser terminal
make try-claude     # requires claude CLI
make try-gemini     # requires gemini CLI

# Capture claude onboarding state changes
make capture-claude                  # empty config, full onboarding
make capture-claude CONFIG=trusted   # skip to idle
```

## Performance

An idle coop-wrapped session should use well under 5% of a core. Measure it locally:

```bash
coop --agent=claude -- sleep 600 &
sleep 15 && ps -o pcpu= -p $(pgrep -f 'coop --agent=claude')
```

Watch this if you touch `driver/log_watch.rs`: its `notify` watch on the session log's
parent directory uses the default inotify mask, which includes `IN_OPEN`. Anything in the
tailer that unconditionally opens the log file to check for new data re-triggers its own
watch on every check, and the resulting wake immediately schedules another check — an
unbounded self-feedback loop bounded only by tokio scheduling overhead (observed at ~12k
wakeups/sec, ~0.7 core per pod, in gd-hMkeX9MkIT). `read_new_lines` avoids this by
`stat`-ing the file first and only opening it when the length has actually changed, since
`stat` doesn't emit `IN_OPEN`. See `unchanged_file_is_not_reopened` in
`log_watch_tests.rs` for the regression test.

## License

Licensed under the Business Source License 1.1
Copyright (c) Matt Baker <fimbaz@gmail.com>
See LICENSE for details.
