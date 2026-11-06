# Local agent runbook

`gb agent start --local` runs a fully-registered gardenio agent on your laptop: a
real coop+claude session, registered with the kbeads daemon and coopmux, showing
up in `gb agent roster` just like a cluster pod. This runbook covers the daily-
driver flow, the coop attach/detach model, and common failure modes.

For the design background see
[`gardenio/docs/local-agent-epic-brief.md`](local-agent-epic-brief.md).

## Prerequisites

- `gb`, `kd`, `coop` in `$PATH`. For claudeless-based e2e runs also `claudeless`.
- A reachable kbeads daemon and valid credentials. The **recommended** way for a
  human driving `gb`/`kd` from a laptop to authenticate is `gb login` (or
  `kd login`, the same thing) — a Keycloak device-grant sign-in in your own
  browser whose token both CLIs send and refresh, so you never have to copy a
  daemon bearer token out of a pod. Point at the daemon with `BEADS_HTTP_URL`
  (the session is bound to that host) and run:

  ```sh
  gb login
  ```

  See [`gb-login-runbook.md`](gb-login-runbook.md) for the full flow, refresh,
  and when you have to sign in again.

  Manual token export remains the **fallback** and the mechanism for in-cluster
  agents and CI. In that case export a valid `BEADS_AUTH_TOKEN`
  (canonical; `gb` reads this); for `kd` and the controller path, export the
  same value as the legacy `BEADS_DAEMON_TOKEN`. Setting both is the safe
  default for that fallback path.
- A `operated_by` identity the daemon will accept. A `gb login` / `kd login`
  token already carries your identity; otherwise set it explicitly. The
  daemon rejects anything that is not `slack:U<alnum>` (your Slack user
  id) or `oidc:<local>@<domain>` (an email shape) with HTTP 400
  "operator could not be resolved". Bare names, `ci:*`, `system:*`, etc.
  are refused. Example:

  ```sh
  export BEADS_OPERATED_BY="oidc:your.name@acme.example"
  # or, if you know your Slack user id:
  export BEADS_OPERATED_BY="slack:U0123ABCD"
  ```

  The e2e runner scripts (`test-local-mode.sh`, `run-shard.sh`) export an
  oidc-shape fixture default if you haven't set one — that is for CI, not a
  long-term laptop identity.
- `claude` on `$PATH` for real agent sessions (not required for `--test-scenario`).

## Security posture before distribution

The local-agent path inherits the same VPN-gated posture as any gardenio agent.
Before handing `gb agent start --local` to teammates you should wait on two
epics to land:

- `ka-0000003552` — rogue-agent containment (ownership enforcement, tight
  `write_policy`, scoped trigger authz)
- `ka-0000005919` — internet-safe daemon (per-identity tokens with TTL, rate
  limits, NATS subject scoping)

Until those ship, treat local-mode as author-and-maintainer territory: you can
confidently drive your own agents against the daemon, but do not advertise it
broadly. This is informational guidance — there is no hard bead block.

## Daily-driver launch

The canonical invocation:

```sh
gb agent start --local \
  --role=crew \
  --project=gardenio
```

(`gb runner join` starts it with `--runner`, which also makes it run the local
escalations approved for that machine; see
[Escalation round-trip](#escalation-round-trip-local-mode).)

What it does:

1. Picks a free local port and starts a `coop` daemon on it. Coop wraps the
   agent command (`claude --dangerously-skip-permissions` by default) inside
   a terminal session you can attach to.
2. Materializes workspace `.claude/` config via `gb setup claude --local`
   so the agent sees the usual CLAUDE.md and hook config. **In local mode the
   write surface is confined to `<workdir>/`** — see
   [Files written by `gb setup claude`](#files-written-by-gb-setup-claude) for the
   exact paths. Your operator-level `~/.claude/` is never touched.
3. Registers an agent bead with the daemon. On exit the bead is closed and
   coop is terminated.
4. Attaches your TTY to the coop session so you see claude's output and can
   type prompts directly. With `--runner`, the mail subscriber also hands the
   session the escalations the daemon approves for that runner.

Useful flags:

| Flag | What |
|---|---|
| `--name=<n>` | Agent name. Default: `local-<hostname>`. |
| `--role=<role>` | Registers the agent under this role (e.g. `crew`, `gardener`). |
| `--project=<p>` | Labels the agent bead `project:<p>`. |
| `--task=<bead-id>` | Pre-assigns a task. Nudges the agent to `kd claim <id>`. |
| `--runner=<id>` | The runner bead this agent runs on (set by `gb runner join`); runs its approved local escalations. |
| `--watch-escalations` | Deprecated, does nothing. |
| `--dir=<path>` | Working directory. Default: `$PWD`. |
| `--test-scenario=<name>` | Run a claudeless scenario instead of real claude. |
| `--command=<cmd>` | Override the agent command. Rarely needed. |

When you want to hand a task to a local agent the short form is:

```sh
kd claim --unassign kd-<id>           # make sure nobody else owns it
gb agent start --local --task kd-<id>
```

## A tenant's agent (`gl agent start`)

A tenant runs an agent for their land with `gl`, the tenant CLI
(`lands/gl`), not with `gb agent start` directly. The runtime is the same as
a land pod's: `kd` and `gb` under a per-agent land token at the land door;
`gl` owns only the launch (Door D.16, gd-6yW5zvZSbz).

Install: `gl`, `gb`, `kd` and `coop` on `PATH` (coop as in Prerequisites),
and `claude` signed in. The tenant's own `claude` pays for the agent's model
use (decision gd-09hIjRBKvf); no LiteLLM key is minted and the land's purse
is untouched.

```sh
gl login --land acme
gl agent start --ticket <ticket-id>   # --name, --dir as with gb
gl agent list                         # the land's laptop agents
gl agent stop <agent-id>              # its token stops at once
```

What `gl agent start` does:

1. Checks `gb` and `coop` are on `PATH`, then calls the Lands API
   `createAgent` on the tenant's own sign-in. The land writes the agent bead
   (`role=lands-gardener`, `execution_target=laptop`, `spawn_source=lands_api`,
   `operated_by` the tenant), counts it against `max_live_agents`, and answers
   the agent's token `agent-<bead id>` bound to the land (24h, with a refresh),
   and its door. The minted role holds the lands-tenant tier whatever the
   tenant's own role.
2. Runs `gb --http-url https://<edge>/land/<land> agent start --local
   --agent-id <bead> --name <name> --role lands-gardener --project <land>`
   with the agent's token in a 0600 file named by `GARDENIO_TOKEN_FILE` (also
   in `GARDENIO_TOKEN`, for a kd or gb too old to read the file) and
   `GARDENIO_NO_LOGIN_SESSION=1`, after dropping every `BEADS_*`, `KBEADS_*`,
   `KD_*`, `BOAT_*`, `GB_*` and `COOP_*` variable and `GARDENIO_TOKEN` and
   `GARDENIO_TOKEN_FILE` from the tenant's environment.
3. `gb` adopts the bead instead of creating one, and runs coop and claude
   as for any local agent. The session primes as `lands-gardener` (`gb prime`
   through the door). `/v1/configs` is not on the door, so `gb setup claude`
   writes its defaults.
   While `gb` runs, `gl` renews the token past two thirds of its life
   (`refreshAgent` at the door, on the agent's own token; on the tenant's
   sign-in only if the land refuses that, after a sleep past its end) and
   rewrites the file whole (temp file, then rename). The refresh is spent
   once and lives only in `gl`'s memory (gd-Vbvd3EgqTI). kd and gb read the
   file ahead of every token variable; gb reads it again on each request,
   so its long-lived clients follow the renewal. A set but unreadable file
   stops kd and gb rather than letting them send another credential.
4. When `gb` exits, `gl` calls `revokeAgent` on the tenant's sign-in: the
   token dies and the bead closes. If that fails, `gl` prints the
   `gl agent stop` to run.

**`--agent-id`.** `gb agent start --local --agent-id <bead> --name <name>`
runs as an agent bead that already exists: nothing is created, superseded or
closed, since whoever registered it stops it. It needs the agent's own token
(`--token`, `GARDENIO_TOKEN_FILE` or `GARDENIO_TOKEN`; never `remotes.toml`
or a login session) and
refuses to start unless `GARDENIO_NO_LOGIN_SESSION=1`.

**Why the switch matters.** The session's `kd` and `gb` share the tenant's
home, and both send a cached `gb login`/`gl login` session for the daemon
they talk to *before* any token in the environment. The tenant's land
session (`~/.local/state/beads/oidc-login-land-<land>.json`) is bound to the
very door the agent uses, so without `GARDENIO_NO_LOGIN_SESSION=1` the agent
would act as its tenant (gd-QV2WuUjely). With it, `oidclogin.Bearer` reads
no session at all.

**Presence, hooks and mail on the door** (gd-2iqSr9ANOH). The door never
serves the hook routes (`/v1/hooks/*`): emit takes its actor from the body
and runs git and advice hooks in the daemon. Instead the door records the
agent's presence from its own requests, under the name its token was minted
for, and `GET /v1/agents/roster` through a door lists only that land's
agents. On a land door gb sends SessionStart and the 5-minute heartbeat as
`GET /v1/me`, `gb hook stop-gate` and `gb bus emit` allow at once (the
decision gate means nothing to a tenant's agent), and `gb hook relay` sends
nothing. The daemon's event stream is off the door and the land feed carries
no mail, so the mail subscriber drains the agent's mail every 30 seconds
instead (nudges and `/kill` arrive within that). No reaper closes a land
agent for a stale heartbeat: it has no runner, so the controller's stale
reaper always holds it.

Still not on the door: coopmux registration and local escalations (the
door serves no `/v1/escalations/{id}`; gb warns and starts no executor). The token is not refreshed during
a session; start the agent again after about a day.

## Coop attach / detach model

Coop is the terminal multiplexer. The workflow is:

- **Attach** — `gb agent start` attaches for you on launch. You can reach an
  existing session with `coop attach http://127.0.0.1:<port>` (the port is
  printed at startup).
- **Detach** — press `Ctrl-\`. The coop session keeps running in the
  background; claude keeps working. The agent bead stays registered.
- **Re-attach** — run `coop attach <url>` again. The scrollback is preserved.
- **Shutdown** — stop the agent cleanly by re-attaching and exiting claude, or
  by sending `SIGTERM` to the `gb agent start` process (the agent bead closes
  as part of cleanup).

Detach/re-attach is useful when you want to close your laptop lid briefly, or
switch terminal windows without losing the session.

## Escalation round-trip (local mode)

An agent started with `--runner` (as `gb runner join` does) runs the local
escalations approved for that runner: the daemon mails it, and its mail
subscriber hands the mail to the escalation executor, which nudges coop. The
sequence:

1. An escalation is filed with `--execution-target local --target-runner <runner bead id>`
   (the machine, from `gb runner status`), or `--target-owner oidc:<you>` to let the daemon
   pick your one online runner at approval. The daemon refuses a local escalation that
   names neither (unless `BEADS_ESCALATION_LOCAL_TARGET=warn`), or one whose
   `target_runner` is not an open, unrevoked runner bead. `gb escalation create` refuses
   the first case itself, and a runner flag with no `--execution-target` implies `local`.
   A crew asking the operator or the laptop coordinator to act leaves
   `--execution-target` unset: the escalation policy routes it (`execute_by=coordinator:laptop`).
2. The runner's owner approves via Slack or `gb escalation approve`; any other approver,
   a coordinator lane included, gets a 403. The daemon claims the local dispatch, stamps
   the agent joined on the runner (`runtime_id`) as `executor`, and mails it
   `control:escalation-approved` (parked as `runner:<id>` when no agent is joined).
   The Slack card and the approver's DM say where it will run: the runner's
   hostname and owner.
3. The mail subscriber of the agent on that runner receives the mail (a parked
   mail is re-addressed to the agent on its next drain), and the executor nudges
   coop with the escalation prompt, records `prompt_delivered_at`, and closes the
   mail. That mail is the only trigger: nothing else on the laptop or in the
   cluster runs a local escalation. An agent started without `--runner` runs none.
4. Claude picks up the nudge on its next turn and reports with
   `gb escalation close`, which succeeds on this agent's own token (it is the
   executor) or on an operator's `kd login`.

If step 3 stalls, check the mail subscriber and executor logs printed on stderr and confirm
`BEADS_HTTP_URL` and `BEADS_AUTH_TOKEN` (plus `BEADS_DAEMON_TOKEN` for the
legacy fallback) are set in the environment that started `gb`.

## Claudeless scenarios (no real claude)

For e2e runs or local experimentation without hitting the Anthropic API, use
claudeless scenario TOMLs in `gardenio/tests/e2e/claudeless/local-*.toml`:

```sh
gb agent start --local \
  --test-scenario=local-lifecycle \
  --role=crew
```

Bare names resolve under `gardenio/tests/e2e/claudeless/`. Paths ending in
`.toml` are used verbatim. The runner script
`gardenio/tests/e2e/scripts/test-local-mode.sh` exercises the full matrix of
local-mode scenarios (basic lifecycle, task pre-assignment, yield/close,
sigterm, orphan cleanup, prime-fetch) and is the CI surface.

## Files written by `gb setup claude`

`gb setup claude` (invoked automatically by `gb agent start --local` and
explicitly via `gb setup claude --local`) has two distinct write surfaces
depending on the mode. The `--local` flag is what keeps a laptop run from
clobbering the operator's own Claude Code home.

### K8s agent pod mode (default — no `--local`)

In the agent pod, `$HOME` is the agent's own home (`/home/agent`), so
writing under `~/.claude/` is the correct behaviour.

| File | Source |
|---|---|
| `~/.claude/settings.json` | `claude-settings` config bead (user-level Claude Code settings) |
| `~/.claude/stop-gate-text.md` | `claude-instructions` config bead (`stop_gate_blocked` field) |
| `<workdir>/.claude/settings.local.json` | workspace hooks (resolved from `claude-hooks` config beads) |
| `<workdir>/.mcp.json` | `claude-mcp` config bead (or fallback playwright-mcp on `$PATH`) |
| `<workdir>/CLAUDE.md` | `claude-instructions` config bead (`claude_md` field) |

### Local mode (`gb agent start --local` / `gb setup claude --local`)

In local mode `$HOME` is the operator's own home, so the setup path is
diverted entirely into `<workdir>/`. Nothing under `~/.claude/` is created
or modified.

| File | Source |
|---|---|
| `<workdir>/.claude/settings.local.json` | `claude-settings` config bead (user-level layer) **and** workspace hooks (`claude-hooks` config beads), merged by top-level key; keys of your own in the file are kept |
| `<workdir>/.claude/stop-gate-text.md` | `claude-instructions` config bead (`stop_gate_blocked` field) |
| `<workdir>/.mcp.json` | `claude-mcp` config bead (or fallback playwright-mcp on `$PATH`) |
| `<workdir>/CLAUDE.md` | `claude-instructions` config bead (`claude_md` field) |

Local mode never writes `<workdir>/.claude/settings.json`. Repos commit that
file, and overwriting it dropped the repo's own hooks and left the tracked file
modified (gd-wKXxD4qXp6). Claude Code merges the repo's `settings.json`,
`settings.local.json` and your user-level settings, so the repo's hooks still
run next to the agent's.

The startup log will say `[setup] merged user-level settings into
<workdir>/.claude/settings.local.json (local mode: .claude/settings.json left
untouched)` instead of the pod-mode `[setup] wrote user settings to
~/.claude/settings.json`. That line is the clearest in-terminal confirmation
that `--local` took effect.

If the repo tracks `.claude/settings.local.json` anyway (it is normally
gitignored), setup warns that the write will show in `git status`. Do not
commit it; `git update-index --skip-worktree .claude/settings.local.json`
keeps it out of your commits.

If you are running `gb setup claude` directly (not through `gb agent
start`) you must pass `--local` explicitly:

```sh
gb setup claude --local --role=crew --workspace=$PWD
```

## Troubleshooting

### `coop health check: … timed out`

Coop failed to start or bind. Common causes:

- `coop` binary not on `$PATH`. Install from the coop crate or grab it from
  the gardenio agent image build (`docker.yml` `build-coop` task).
- Port conflict. Re-run — `gb agent start` picks a free port each time, but
  an eager health-probe port can still clash. Check `/tmp/coop-*.log` if
  coop writes one.

### Agent bead stuck in `spawning`

This is the SIGKILL sad path (tracked in `ka-0000000375`). Until that reaper
ships, re-run `gb agent start` so the cleanup path closes the stale bead, or
close the bead manually with `kd close kd-<id>`.

### `setup claude` complains about missing config beads

Run in a checkout of the monorepo (or any dir with a `.claude/` directory the
agent can materialize into). In fresh dirs `gb agent start` falls back to
`runSetupClaudeDefaults`, which only writes the workspace hooks — not the full
config. That is fine for quick experiments but not for daily driving.

### `claude` refuses to run with `--dangerously-skip-permissions`

Either downgrade to a safer flag by setting `--command="claude ..."`, or
install claude somewhere the flag is accepted. Claudeless scenarios are a
drop-in substitute while you work around this.

### `400 operator could not be resolved`

The daemon's `sanitizeOperatedByHeader`
(`kbeads/internal/server/audit.go`) rejected your `BEADS_OPERATED_BY` value.
Only `slack:U<alnum>` and `oidc:<local>@<domain>` shapes pass. Bare names,
`ci:*`, `system:*`, `fleet:*`, and other sentinels are refused at the
header boundary. Export a valid shape (see Prerequisites) and re-run.

### Escalation watcher: `subscribe failed: 401`

Your credentials are wrong or expired. If you authenticated with `gb login`
(the recommended human path), read the line above the 401: "Keycloak ended the
login session" means re-run `gb login`; "the session is kept, retry in a
moment" means Keycloak is restarting and a retry is enough.

If you are on the manual-token fallback: `BEADS_AUTH_TOKEN` (or the legacy
`BEADS_DAEMON_TOKEN` fallback) is wrong or expired. `gb` reads
`BEADS_AUTH_TOKEN` first and falls back to `BEADS_DAEMON_TOKEN`; `kd` reads
`BEADS_DAEMON_TOKEN`. **Note:** `BEADS_TOKEN` is not read by either client — if
you exported only that, you'll see `401` with no actionable hint. Export a fresh
token under both env-var names and re-run. On cluster-hosted daemons the token
lives in a K8s secret `kd-beads-token` in the namespace;
`kubectl get secret kd-beads-token -o jsonpath='{.data.token}' | base64 -d`
fetches it — this is the agent/CI path, not the recommended human daily driver.

## CI / e2e

`test-local-mode.sh` is wired into the RWX e2e shard runner as the
`local-mode` module. `.rwx/e2e.yml` installs `coop` (via `crane export`
of `gardenio/coop:latest`) and `claudeless` (pinned upstream release) into
each shard so the module exercises the full scenario matrix in CI. The
`coop`/`claudeless` skip path the script still carries is a defensive
fallback for laptop runs that lack those binaries on PATH.

To run the module locally against a port-forwarded daemon:

```sh
kubectl -n gardenio-rwx port-forward svc/gardenio-rwx-beads 19099:8080 &
TOKEN=$(kubectl -n gardenio-rwx get secret kd-beads-token \
  -o jsonpath='{.data.token}' | base64 -d)
BEADS_HTTP_URL=http://localhost:19099 \
  BEADS_AUTH_TOKEN=$TOKEN \
  BEADS_DAEMON_TOKEN=$TOKEN \
  ./gardenio/tests/e2e/scripts/test-local-mode.sh
```

## References

- [`local-agent-epic-brief.md`](local-agent-epic-brief.md) — design,
  threat model, decision log.
- `gardenio/controller/cmd/gb/agent_start.go` — the local spawn implementation.
- `gardenio/tests/e2e/scripts/test-local-mode.sh` — scenario runner.
- `gardenio/tests/e2e/claudeless/local-*.toml` — scenario definitions.
