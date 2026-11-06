# Session Lifecycle

This document covers the coop session lifecycle from spawn to exit: session
phases, state transitions, shutdown, credential switching, profile rotation,
error handling, and tuning knobs. For detection tiers, hooks, and encoding see
[01-overview.md](01-overview.md). For credential file shapes see
[02-credentials.md](02-credentials.md).


## 1. Session Phases

A coop session has three nested layers:

```diagram
Initialization               prepare config, driver, servers
└─▶ Session loop             run session → on exit, await switch or shutdown
    └─▶ Active session       multiplex output, input, detection, drain, switch
        └─▶ exit / switch
```

### Initialization

Coop loads config (`--agent-config`), handles `--resume` if present, prepares
the agent session (hooks, settings, env vars), spawns the PTY, starts transport
servers (HTTP, Unix socket, gRPC), and installs signal handlers.

### Session Loop

After the agent exits, coop waits for either a credential switch request or a
shutdown signal. On switch, it respawns with new credentials and loops back.
Transport connections survive across switches.

### Active Session

During normal operation, coop concurrently processes PTY output (updating the
screen and ring buffer), forwards API input to the PTY, runs state detection,
and handles shutdown/drain/switch signals.


## 2. Agent States

Nine states classify the agent process:

| State | Wire name | Payload | Trigger |
|-------|-----------|---------|---------|
| Starting | `starting` | -- | Initial state before first detection |
| Working | `working` | -- | Tool call, thinking, turn start |
| Idle | `idle` | -- | Turn end, idle notification, text-only output |
| Prompt | `prompt` | prompt context | Permission, plan, question, or setup dialog |
| Error | `error` | `detail` | API error detected in log or hook |
| Parked | `parked` | `reason`, `resume_at_epoch_ms` | All profiles rate-limited; waiting for cooldown |
| Restarting | `restarting` | -- | Credential switch or restart initiated |
| Exited | `exited` | `code`, `signal` | Child process terminated |
| Unknown | `unknown` | -- | State cannot be determined |

The session is **not ready** until the first transition away from `starting`.
Until then, action endpoints (`nudge`, `respond`) and `GET /api/v1/ready` return `NOT_READY` (503).
The state endpoint (`GET /api/v1/agent`) always returns 200 with the current state.

### State Diagram

```diagram
                 ┌─────────────────────────────┐
                 │          starting           │
                 └──────────┬──────────────────┘
                            │ first detection
                            ▼
              ┌─────── working ◄──────┐
              │           │           │
              │      tool call /      │
              │      thinking         │
              ▼           │           │
           prompt         │        turn start /
           (perm/plan/    │        user input
            question/     │           │
            setup)        ▼           │
              │         idle ─────────┘
              │           │
              └───────────┤
                          │
                    ┌─────┴─────┐
                    ▼           ▼
                  error       exited
                    │
                    ▼ (rate_limited + profiles)
                  parked ──▶ restarting ──▶ starting
```

## 3. Shutdown

Shutdown can be triggered by:

- **SIGTERM** or **SIGINT** to the coop process
- **`POST /api/v1/shutdown`** from the orchestrator

### Shutdown Steps

1. **If agent is idle**: send SIGHUP immediately
2. **Otherwise**: enter drain mode — send Escape every 2 seconds until the
   agent reaches idle or the drain deadline expires (`COOP_DRAIN_TIMEOUT_MS`,
   default 20s), then SIGHUP
3. Wait for the child process to exit (`COOP_SHUTDOWN_TIMEOUT_MS`, default 10s)
4. If the child does not exit in time: SIGKILL the process group

### Second Signal

If coop receives a second SIGTERM or SIGINT while already shutting down, it
exits immediately with code 130.


## 4. Credential Switch

A credential switch restarts the child process with new environment variables
while preserving all transport connections and the conversation (via `--resume`).

### Trigger

`POST /api/v1/session/switch`:

```json
{
  "credentials": {"ANTHROPIC_API_KEY": "sk-ant-..."},
  "force": false,
  "profile": "profile-2"
}
```

Only one switch can be pending at a time. If a switch is already queued, the API
returns `SWITCH_IN_PROGRESS` (409).

### Sequence

1. **Wait for idle** (or force): if agent is already `idle`, `exited`, or
   `force: true`, proceed immediately. Otherwise queue and wait for `idle`.
2. **Broadcast** `restarting` state to all subscribers
3. **SIGHUP** the child process group — child exits
4. **Respawn** a new child with the updated credentials merged into its
   environment. The session resets to `starting` (ready flag clears) and a
   new session ID is assigned.
5. **Resume** the active session with the new child process


## 5. Profile Rotation

Named credential profiles enable automatic rotation on rate-limit errors.

### Mode

Profile rotation is a process-wide concern controlled by `--profile` (env:
`COOP_PROFILE`). Two modes:

| Mode | Behavior |
|------|----------|
| `auto` (default) | Rotate to the next available profile on rate-limit errors |
| `manual` | Profiles are registered but never auto-rotated; orchestrator switches explicitly |

The mode can be toggled at runtime:

| Transport | Get | Set |
|-----------|-----|-----|
| HTTP | `GET /api/v1/session/profiles/mode` | `PUT /api/v1/session/profiles/mode` ← `{"mode":"auto\|manual"}` |
| WS | `profiles:mode` | `profiles:mode:set` ← `{"mode":"..."}` |
| gRPC | `GetProfileMode` | `SetProfileMode` |

### Registration

`POST /api/v1/session/profiles`:

```json
{
  "profiles": [
    {"name": "primary", "credentials": {"ANTHROPIC_API_KEY": "sk-1"}},
    {"name": "secondary", "credentials": {"ANTHROPIC_API_KEY": "sk-2"}}
  ]
}
```

The first profile starts as `active`.

### Rotation Trigger

When coop detects a `rate_limited` error (and mode is `auto`):

1. Mark the active profile as `rate_limited` with a cooldown timer
2. Promote any expired cooldowns back to `available`
3. Pick the next `available` profile (round-robin from after the current one)
4. Trigger a forced credential switch

### Safety Nets

Env var-tunable safety mechanisms prevent excessive switching:

| Variable | Default | Purpose |
|----------|---------|---------|
| `COOP_ROTATE_COOLDOWN_SECS` | `300` | How long a rate-limited profile stays unavailable |
| `COOP_ROTATE_MAX_PER_HOUR` | `20` | Maximum switches per hour (anti-flap cap) |

### Exhaustion and Parking

If all profiles are on cooldown, the session transitions to `parked` with
reason `"all_profiles_rate_limited"` and `resume_at_epoch_ms` set to the
earliest cooldown expiry. Coop automatically retries rotation when the first
cooldown expires.

### Profile Status Values

| Status | Meaning |
|--------|---------|
| `active` | Currently in use |
| `available` | Ready for rotation |
| `rate_limited` | On cooldown (carries `cooldown_remaining_secs`) |


## 6. Error Classification

When the agent enters an `error` state, the detail string is classified into
one of six categories (reported as `error_category` in the
[agent state API](../api/http.md#get-apiv1agent)):

| Category | Wire name | Example patterns | Automated action |
|----------|-----------|------------------|------------------|
| Unauthorized | `unauthorized` | `authentication_error`, `invalid api key`, `permission_error` | None |
| Out of credits | `out_of_credits` | `billing`, `insufficient_credits`, `payment_required` | None |
| Rate limited | `rate_limited` | `rate_limit_error`, `too many requests`, `429` | Profile rotation |
| No internet | `no_internet` | `connection refused`, `dns`, `timeout`, `econnrefused` | None |
| Server error | `server_error` | `api_error`, `overloaded`, `500`, `502`, `503` | None |
| Other | `other` | (no match) | None |

Only `rate_limited` triggers an automated action (profile rotation, if profiles
are registered). All other categories are reported to the orchestrator for
manual handling.


## 7. Groom Levels

The `--groom` flag (env: `COOP_GROOM`) controls how coop handles agent prompts
during startup and operation:

| Level | Hooks | Screen detection | Auto-dismiss | Detection |
|-------|-------|------------------|--------------|-----------|
| `auto` (default) | Injected | Active | Yes (disruptions) | All sources |
| `manual` | Injected | Active | No | All sources |
| `pristine` | Not injected | Active | No | Log watcher + screen only |

### Auto-Dismissed Prompts (groom=auto)

These are "disruption" prompts that block the agent from reaching idle:

| Prompt | Subtype | Action |
|--------|---------|--------|
| Security notes | `security_notes` | Select option 1 |
| Login success | `login_success` | Select option 1 |
| Terminal setup | `terminal_setup` | Select option 1 |
| Theme picker | `theme_picker` | Select option 1 |
| Settings error | `settings_error` | Select option 2 ("Continue without") |
| Workspace trust | `trust` (permission) | Select option 1 ("Yes, I trust") |
| Fable consent | `fable_consent` | Select option 1 ("Continue with Fable 5") |
| Fable credits | `fable_credits` | Select option 2 (decline — see below) |

Claude Code gates the first session on Fable 5 behind a one-time usage-credit
consent dialog. Left unanswered it wedges every fable-pinned agent at startup,
and it cannot be preseeded: consent persists per-org in `~/.claude.json`
(`fableOverageConsentV2`), keyed by an OAuth account UUID unknowable at
image-build time. So it is answered at the screen.

The two subtypes split on what confirming actually costs. `fable_consent`
("Continue with Fable 5") means credits are already provisioned — confirming
only records consent, so coop confirms it. Every other confirm label is
`fable_credits`, which coop *declines* instead: option 2 keeps the session alive
on another model rather than wedging, and it logs a warning. Enable usage
credits for the account out of band to run on Fable 5.

The harness picks the confirm label from the account's credit state, and there
are four branches — the last one being what an org-managed account gets:

| Credit state | Confirm label | Subtype |
|---|---|---|
| Usable | `Continue with Fable 5` | `fable_consent` |
| Off, balance remains | `Yes, re-enable and continue` | `fable_credits` |
| None, self-serve purchase | `Yes, buy usage credits` / `Buy usage credits` | `fable_credits` |
| Managed on the web or by an admin | `Set up`/`Manage usage credits on claude.ai`, `Request usage credits from your admin`, `Request more from your admin` | `fable_credits` |

That fourth branch spends nothing, but confirming only opens a URL or mails the
org's admins and then dismisses *without* making Fable usable — so it is
declined like the rest (ka-0000001616). Its labels were missing from the
detector at first, which left those screens on one signal, below threshold, and
wedged: the exact failure the detector exists to prevent.

Neither the standalone "Buy usage credits" purchase dialog nor the admin-request
confirmation is ever auto-answered.

The weekly-included-usage **limit** dialog ("You've reached your Fable 5 limit"
/ "What do you want to do?", subtype `fable_limit`, ka-0000003007) *is*
auto-answered, but only toward the STOP/WAIT option — the one that neither spends
nor upgrades — chosen by scanning the on-screen labels, not by a fixed position.
Its option set is conditional (members include "Upgrade to Max", "Add funds to
continue with usage credits", "Adjust monthly limit", "Upgrade to Team plan",
"Stop", "Wait for limit to reset", "Don't continue automatically"), and option 2
in particular walks into the card-charging purchase flow, so the answer is an
**allowlist by label**: the first "stop / wait for limit to reset / don't
continue automatically" option is selected, and if none is present the dialog is
declined with Esc. Either way the session stays alive without any spending or
upgrade decision. It is classified as `fable_limit` (not a consent subtype)
precisely so it can never route through the fixed-option auto-confirm path.

Auto-dismiss waits `COOP_GROOM_DISMISS_DELAY_MS` (default 500ms) before
sending keystrokes. The prompt state is broadcast *before* auto-dismiss so
API clients see it transparently.

### Elicitation-Only Prompts

These prompts are never auto-dismissed regardless of groom level:

- Tool permissions (`permission` without `trust` subtype)
- Plan prompts (`plan`)
- Question prompts (`question`)
- OAuth login (`setup` / `oauth_login`)
- Login method (`setup` / `login_method`)
- Startup text prompts (`startup_trust`, `startup_bypass`, `startup_login`)


## 8. Tuning Reference

All durations can be overridden via environment variables (milliseconds):

| Variable | Default | Purpose |
|----------|---------|---------|
| `COOP_DRAIN_TIMEOUT_MS` | `20000` | Graceful drain timeout (0 = immediate kill) |
| `COOP_SHUTDOWN_TIMEOUT_MS` | `10000` | Child exit wait after SIGHUP |
| `COOP_IDLE_TIMEOUT_MS` | `0` | Idle timeout before auto-shutdown (0 = disabled) |
| `COOP_NUDGE_TIMEOUT_MS` | `4000` | Wait for `working` after nudge delivery |
| `COOP_INPUT_DELAY_MS` | `200` | Base delay between message and Enter |
| `COOP_INPUT_DELAY_PER_BYTE_MS` | `1` | Extra delay per byte beyond 256 |
| `COOP_GROOM_DISMISS_DELAY_MS` | `500` | Delay before auto-dismissing prompts |
| `COOP_SCREEN_DEBOUNCE_MS` | `50` | Screen update broadcast interval |
| `COOP_SCREEN_POLL_MS` | `3000` | Screen detector poll interval |
| `COOP_LOG_POLL_MS` | `3000` | Log watcher poll interval |
| `COOP_PROCESS_POLL_MS` | `10000` | Process monitor poll interval |
| `COOP_TMUX_POLL_MS` | `1000` | Tmux adapter poll interval |
| `COOP_REAP_POLL_MS` | `50` | Child process exit check interval |


## 9. API Error Codes

Shared across HTTP, WebSocket, and gRPC transports:

| Code | HTTP | gRPC | Meaning |
|------|------|------|---------|
| `NOT_READY` | 503 | Unavailable | Agent has not completed first state transition |
| `EXITED` | 410 | NotFound | Agent process has exited |
| `AGENT_BUSY` | 409 | FailedPrecondition | Agent is not idle (for nudge/respond) |
| `NO_PROMPT` | 409 | FailedPrecondition | No active prompt to respond to |
| `SWITCH_IN_PROGRESS` | 409 | FailedPrecondition | A credential switch is already pending |
| `UNAUTHORIZED` | 401 | Unauthenticated | Missing or invalid auth token |
| `BAD_REQUEST` | 400 | InvalidArgument | Malformed request body |
| `NO_DRIVER` | 404 | Unimplemented | No driver configured for the agent type |
| `INTERNAL` | 500 | Internal | Unexpected server error |
