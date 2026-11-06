# Design: `kd agent start --k8s`

**Task:** ka-0000000289
**Author:** kbeads-dev
**Date:** 2026-02-24
**Status:** Draft — awaiting approval

---

## Goal

Replace `/entrypoint.sh` (687 lines of bash) in the gardenio agent image with a
`kd agent start --k8s` Go subcommand. The Go binary becomes PID 1 in the K8s
pod. `entrypoint.sh` is deleted.

**Why:** bash is hard to test, hard to debug, and accumulates ad-hoc fixes.
Go gives us proper error handling, goroutines instead of subshells, unit tests,
and a single binary that works the same locally and in K8s.

---

## Scope

### What the current `entrypoint.sh` does

| # | Subsystem | ~Lines | Complexity |
|---|-----------|--------|------------|
| 1 | Read `/etc/platform-version` → `BEADS_PLATFORM_VERSION` | 5 | trivial |
| 2 | `git config --global` (user, safe.directory) | 5 | trivial |
| 3 | Git credentials (`GIT_USERNAME`/`GIT_TOKEN` → `.git-credentials`) | 8 | trivial |
| 4 | Workspace git init, or stale-branch reset on restart | 22 | low |
| 5 | Daemon config (write `.beads/config.yaml` if `BEADS_DAEMON_HOST` set) | 10 | trivial |
| 6 | PVC persistence: `mkdir .state/{claude,coop}`, symlink `~/.claude` | 12 | trivial |
| 7 | Claude credential provisioning (5-priority cascade) | 30 | medium |
| 8 | `export XDG_STATE_HOME` | 2 | trivial |
| 9 | Dev-tools `PATH` (`/usr/local/go/bin`) | 4 | trivial |
| 10 | Write `~/.claude/settings.json` (permissions + LSP plugin detection) | 18 | low |
| 11 | Hook materialization (`kd setup claude`) | 10 | trivial — already Go |
| 12 | Write `CLAUDE.md` if absent; append dev-tools section if missing | 40 | low |
| 13 | Skip onboarding wizard (write `~/.claude.json`) | 1 | trivial |
| 14 | `auto_bypass_startup` goroutine — poll coop API, dismiss prompts | 60 | medium |
| 15 | `inject_initial_prompt` goroutine — wait for idle, send nudge | 35 | low |
| 16 | OAuth credential refresh loop (every 5 min) | 90 | medium |
| 17 | `monitor_agent_exit` goroutine — detect exited state, shut down coop | 15 | low |
| 18 | Coopmux register on start / deregister on exit | 55 | low |
| 19 | Signal forwarding (`SIGTERM`/`SIGINT` → coop PID) | 12 | low |
| 20 | Restart loop: session resume, stale-log detection, max-restarts | 55 | medium |

**Total:** ~687 lines bash → ~680 lines Go (net zero, but testable).

---

## Proposed file structure

```text
cmd/kd/
  agent_start.go           existing: --local, --docker (no change)
  agent_start_k8s.go       NEW: --k8s flag wiring, top-level setup + restart loop (~200 lines)
  agent_k8s_workspace.go   NEW: git, PVC symlinks, credentials file, settings.json, CLAUDE.md (~150 lines)
  agent_k8s_creds.go       NEW: 5-priority credential cascade + OAuth refresh goroutine (~150 lines)
  agent_k8s_mux.go         NEW: coopmux register/deregister (~80 lines)
  agent_k8s_lifecycle.go   NEW: startup bypass, initial nudge, exit monitor (~100 lines)
```

Tests live alongside each file (`_test.go`).

---

## New flag

```
kd agent start --k8s [flags]

  --workspace string      workspace path (default: /home/agent/workspace)
  --coop-port int         coop HTTP port (default: 8080)
  --coop-health-port int  coop health port (default: 9090)
  --max-restarts int      max consecutive restart attempts (default: 10)
  --command string        command to run via coop
                          (default: "claude --dangerously-skip-permissions",
                           env: BOAT_COMMAND)
```

All `BOAT_*` and `BEADS_*` env vars continue to be injected by the K8s pod
manager — no Helm/manifests change.

---

## Detailed subsystem design

### 1–5. Workspace setup (`agent_k8s_workspace.go`)

```go
func setupWorkspace(cfg k8sCfg) error {
    // 1. platform version
    if data, _ := os.ReadFile("/etc/platform-version"); len(data) > 0 {
        os.Setenv("BEADS_PLATFORM_VERSION", strings.TrimSpace(string(data)))
    }
    // 2. git global config
    runGit("config", "--global", "user.name", cfg.authorName)
    runGit("config", "--global", "user.email", cfg.role+"@gardenio.local")
    runGit("config", "--global", "--add", "safe.directory", "*")
    // 3. git credentials
    if cfg.gitUser != "" && cfg.gitToken != "" {
        writeGitCredentials(cfg.gitUser, cfg.gitToken)
    }
    // 4. workspace git init or stale-branch reset
    if _, err := os.Stat(filepath.Join(cfg.workspace, ".git")); os.IsNotExist(err) {
        runGitIn(cfg.workspace, "init", "-q")
    } else {
        resetStaleBranch(cfg.workspace)
    }
    // 5. daemon config
    if cfg.daemonHost != "" {
        writeDaemonConfig(cfg.workspace, cfg.daemonHost, cfg.daemonHTTPPort)
    }
    return nil
}
```

`resetStaleBranch` checks `git branch --show-current`; if not main/master,
does `git checkout -- . && git clean -fd && git checkout main`.

### 6–9. PVC persistence + env (`agent_k8s_workspace.go`)

```go
func setupPVC(cfg k8sCfg) error {
    stateDir := filepath.Join(cfg.workspace, ".state")
    claudeState := filepath.Join(stateDir, "claude")
    coopState   := filepath.Join(stateDir, "coop")
    os.MkdirAll(claudeState, 0o755)
    os.MkdirAll(coopState,   0o755)

    claudeDir := filepath.Join(homeDir, ".claude")
    if !isMountpoint(claudeDir) {   // skip if K8s subPath already mounted
        os.RemoveAll(claudeDir)
        os.Symlink(claudeState, claudeDir)
    }

    os.Setenv("XDG_STATE_HOME", stateDir)

    // dev tools PATH
    goBin := "/usr/local/go/bin"
    if _, err := os.Stat(goBin); err == nil {
        os.Setenv("PATH", goBin+":"+os.Getenv("PATH"))
    }
    return nil
}
```

`isMountpoint` checks `/proc/mounts` for the path (avoids `mountpoint` binary
dependency).

### 7. Claude credential provisioning (`agent_k8s_creds.go`)

Five-priority cascade — first match wins:

```
1. PVC creds exist ($CLAUDE_STATE/.credentials.json)         → use as-is
2. K8s secret mount (/tmp/claude-credentials/credentials.json) → cp to PVC
3. CLAUDE_CODE_OAUTH_TOKEN env set                            → coop auto-writes
4. ANTHROPIC_API_KEY env set                                  → API-key mode
5. COOP_MUX_URL set → POST /api/v1/credentials/distribute     → write to PVC
```

Returns a `credMode` enum (`PVC`, `Secret`, `OAuthEnv`, `APIKey`, `MuxFetch`,
`None`) that the OAuth refresh goroutine uses to decide whether to run.

### 10–13. Claude settings + CLAUDE.md (`agent_k8s_workspace.go`)

Settings JSON is built in Go using `encoding/json` — no `jq` subprocess.
LSP plugin detection: `exec.LookPath("gopls")`, `exec.LookPath("rust-analyzer")`.

CLAUDE.md write uses a Go template rendered once on first start; dev-tools
section appended only if the guard string `## Development Tools` is absent.

Onboarding skip: `os.WriteFile("~/.claude.json", onboardingJSON, 0o600)`.

### 14. `auto_bypass_startup` goroutine (`agent_k8s_lifecycle.go`)

Polls `GET http://localhost:{coopPort}/api/v1/agent` every 2s for up to 60s.
Handles three cases via `net/http`:
- Screen contains "Resume Session" → `POST /api/v1/input/keys {"keys":["Escape"]}`
- Screen contains "Detected a custom API key" → `POST /api/v1/input/keys {"keys":["Up","Return"]}`
- `prompt.type == "setup"` → `POST /api/v1/agent/respond {"option":2}`

Context-cancelled when main goroutine exits.

### 15. `inject_initial_prompt` goroutine (`agent_k8s_lifecycle.go`)

Waits for `agent.state == "idle"` (polls every 2s, up to 120s), then
`POST /api/v1/agent/nudge {"message":"Check kd ready for your workflow steps and begin working."}`.
Skips if agent reaches `"working"` before idle.

### 16. OAuth credential refresh loop (`agent_k8s_creds.go`)

```go
func oauthRefreshLoop(ctx context.Context, credsFile string, credMode credMode) {
    if credMode == APIKey {
        return  // no OAuth credentials to refresh
    }
    time.Sleep(30 * time.Second)  // let Claude start first
    ticker := time.NewTicker(5 * time.Minute)
    defer ticker.Stop()
    consecutiveFails := 0
    for {
        select {
        case <-ctx.Done(): return
        case <-ticker.C:
            if err := maybeRefreshOAuth(credsFile); err != nil {
                consecutiveFails++
                if consecutiveFails >= 5 { /* check agent state, maybe fatal */ }
            } else {
                consecutiveFails = 0
            }
        }
    }
}
```

`maybeRefreshOAuth` reads `claudeAiOauth.expiresAt`, skips if > 1h remaining
or if it's a coop-managed sentinel (>= 10^12 ms), otherwise POSTs to
`https://platform.claude.com/v1/oauth/token` with `grant_type=refresh_token`.
Uses `encoding/json` throughout — no `jq`.

OAuth client ID (`9d1c250a-e61b-44d9-88ed-5944d1962f5e`) is a package-level
constant; not configurable (same as current bash).

### 17. `monitor_agent_exit` goroutine (`agent_k8s_lifecycle.go`)

Polls `GET /api/v1/agent` every 5s after 10s initial delay. When
`agent.state == "exited"`, posts `POST /api/v1/shutdown` and returns.
Context-cancelled on SIGTERM so it doesn't leak.

### 18. Coopmux registration (`agent_k8s_mux.go`)

```go
type muxClient struct { url, authToken string }

func (m *muxClient) Register(ctx context.Context, sessionID, coopURL, role, agent, pod, ip string) error
func (m *muxClient) Deregister(ctx context.Context, sessionID string) error
```

Both methods use `net/http` with a 10s timeout. `Deregister` is called from a
`defer` in the main function (with a background context, not the cancelled one).

### 19–20. Signal handling + restart loop (`agent_start_k8s.go`)

```go
func runK8s(cmd *cobra.Command, args []string) error {
    ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
    defer stop()

    // One-time setup (runs before first start and is idempotent on restart).
    if err := setupWorkspace(cfg); err != nil { return err }
    if err := setupPVC(cfg);       err != nil { return err }
    provisionCredentials(cfg)
    writeClaudeSettings(cfg)
    runSetupClaude(ctx, cfg.workspace, cfg.role)   // kd setup claude
    writeClaudeMD(cfg)
    writeOnboardingSkip()

    // Register with mux (deregister deferred).
    mux := newMuxClient(cfg)
    sessionID := cfg.hostname
    coopURL := fmt.Sprintf("http://%s:%d", cfg.podIP, cfg.coopPort)
    mux.Register(ctx, sessionID, coopURL, cfg.role, cfg.agent, cfg.hostname, cfg.podIP)
    defer mux.Deregister(context.Background(), sessionID)

    // Start OAuth refresh goroutine (survives restarts).
    go oauthRefreshLoop(ctx, credsFile, credMode)

    // Restart loop.
    maxRestarts := cfg.maxRestarts
    restarts := 0
    for {
        if restarts >= maxRestarts {
            return fmt.Errorf("max restarts (%d) reached", maxRestarts)
        }
        cleanStalePipes(coopStateDir)
        resumeFlag := findResumeSession(claudeStateDir, cfg.sessionResume)

        start := time.Now()
        exitCode, err := runCoopOnce(ctx, cfg, resumeFlag)
        elapsed := time.Since(start)

        if ctx.Err() != nil { return nil }  // SIGTERM — clean exit

        if exitCode != 0 && resumeFlag != "" {
            retireStaleSession(resumeFlag)
        }
        if elapsed >= 30*time.Second { restarts = 0 }
        restarts++
        log.Printf("[kd agent start] coop exited %d after %s (restart %d/%d)", exitCode, elapsed.Round(time.Second), restarts, maxRestarts)
        time.Sleep(2 * time.Second)
    }
}
```

`runCoopOnce` starts coop as `exec.CommandContext`, launches
`auto_bypass_startup` + `inject_initial_prompt` + `monitor_agent_exit`
goroutines, then `cmd.Wait()`s.

---

## Testing strategy

| File | Test approach |
|------|---------------|
| `agent_k8s_workspace.go` | Unit tests with `t.TempDir()` — git init, symlink, creds write |
| `agent_k8s_creds.go` | Unit tests for cascade logic; mock OAuth endpoint via `httptest.NewServer` |
| `agent_k8s_mux.go` | Unit tests with mock mux server (`httptest`) |
| `agent_k8s_lifecycle.go` | Integration tests with mock coop API (`httptest`) for bypass/nudge/monitor |
| `agent_start_k8s.go` | End-to-end: `kd agent start --k8s` in a Docker container (manual/CI) |

---

## gardenio repo changes (separate PR)

1. `images/agent/Dockerfile` — change `CMD ["/entrypoint.sh"]` to `CMD ["kd", "agent", "start", "--k8s"]`
2. `images/agent/hooks/` — remove `stop-gate.sh`, `prime.sh`, `check-mail.sh`, `drain-queue.sh` (replaced by `kd hook` subcommands)
3. `images/agent/entrypoint.sh` — **delete**

---

## Open questions

1. **`--k8s` flag vs auto-detect**: Should `--k8s` be explicit (as proposed) or
   auto-detected when `BOAT_ROLE` is set? Explicit is clearer; auto-detect is
   less ceremony. Preference?

2. **Restart count reset**: Current bash resets count when `elapsed >= 30s`.
   Keep that heuristic, or reset only on clean exit (code 0)?

3. **`isMountpoint` implementation**: Use `/proc/mounts` scan (no external
   binary) or call `syscall.Statfs` + compare device IDs? `/proc/mounts` is
   simpler and sufficient.

---

## Implementation order

1. `agent_k8s_workspace.go` + tests (stateless, no network)
2. `agent_k8s_creds.go` + tests (mock OAuth server)
3. `agent_k8s_mux.go` + tests (mock mux server)
4. `agent_k8s_lifecycle.go` + tests (mock coop server)
5. `agent_start_k8s.go` — wire all together, signal + restart loop
6. gardenio PR: Dockerfile CMD + delete entrypoint.sh + delete hook scripts
