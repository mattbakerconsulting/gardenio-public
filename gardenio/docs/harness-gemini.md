# Gemini harness: what runs where, and how to debug a Gemini agent

Operator and developer guide for `harness=gemini` agents. Design and decisions: [ADR 0005](../../docs/adr/0005-gemini-harness.md) (it builds on [ADR 0004](../../docs/adr/0004-codex-harness.md); read the Codex guide [harness-codex.md](harness-codex.md) first, this page only records where Gemini differs). Evidence from the pinned-CLI spike: [spike.md](../../docs/research/gemini-harness/spike.md). Task: gd-sXOmQvpFWR.

A Gemini agent is the same pod, the same beads, Slack card, decisions, escalations and stop gate as a Claude agent; only the agent CLI, its credential and its translated config differ.

**Status.** Everything below is built and tested against the pinned Gemini CLI 0.64.0 and a scripted API stand-in. It has **not** run against the live Gemini API: the live pass (gd-AkOqbNHAym) waits on an API key (escalation gd-sOYClGJw8U). The controller keeps the harness off until then: while `GEMINI_HARNESS_ENABLED` is false (the default) a spec that resolves to `gemini` is refused at spawn with a permanent `invalid agent harness: ... not enabled on this controller` reason. Items that need the live API are tagged **[LIVE]**.

## What runs where

| Piece | Where | Claude agent | Gemini agent |
|---|---|---|---|
| Harness resolution | controller, once at spawn (`resolveHarness`, `cmd/controller/podspec_harness.go`) | `claude` | agent bead `harness` > role > project > `AGENT_DEFAULT_HARNESS`; stamped on the agent bead as `resolved_harness` |
| Account | controller (`selectAccount`, `harnessProviders` in `cmd/controller/harness_providers.go`) | any provider no other harness claims | `provider=gemini`, `credential=secret:<name>#api-key`, `billing_type=metered`; a mismatch fails the spawn naming both values |
| Credentials | coop credential push | OAuth token / LiteLLM key | `GEMINI_API_KEY`, pushed by coop; an inherited key in the pod spec is stripped and never wins |
| Pod launch | `gb init` / `gb agent start` (`internal/harness`) | `claude --dangerously-skip-permissions` | `gemini --acp`, driven by coop over ACP (`coop --agent gemini`) |
| Config | `gb setup gemini` | `~/.claude/settings.json` | `~/.gemini/settings.json`, `~/.gemini/GEMINI.md` |
| Hooks | `gb hook relay` | Claude hook events | translated `settings.json` hooks, same relay, `harness: "gemini"` on every event |
| State dir | workspace PVC subPath | `~/.claude` | `~/.gemini` (= `$GEMINI_CLI_HOME/.gemini`) |
| Screen | beads-viewer / Slack | PTY screen | no TUI; coop's normalised transcript |

## Env vars in a Gemini pod

| Var | Set by | Meaning |
|---|---|---|
| `GARDENIO_HARNESS=gemini` | controller (`shapeHarnessEnv`) | Read by `gb`, the stop gate and the hook relay. An unknown value is an error, never a silent fallback to claude. |
| `GEMINI_MODEL` | controller, from the resolved model | Becomes `model.name` in `settings.json` and `session/set_model`; empty means the CLI's `auto` routing. The Claude pins (`ANTHROPIC_MODEL`, `CLAUDE_MODEL`, `ANTHROPIC_DEFAULT_*_MODEL`) are removed. |
| `GEMINI_CLI_HOME` | optional | Parent of the `.gemini` state dir. |
| `GEMINI_CLI_TRUST_WORKSPACE=true` | controller | Clears the CLI's first-run workspace-trust gate. |
| `GEMINI_AUTH_MODE=oauth` | controller | Google-account login: `gb setup gemini` pins `oauth-personal` and installs the login, and coop authenticates with it. Unset = API key. See [Google-account login](#google-account-login-subscription--free-tier-no-per-token-billing). |
| `COOP_GEMINI_APPROVAL` | optional | `deny` rejects every `session/request_permission`; default allows once (the pod is the sandbox). |

Every other `GEMINI_*` / `GOOGLE_*` variable is stripped from coop's child environment (`internal/harness.IsDeniedEnv`), so `GOOGLE_GEMINI_BASE_URL` or a stray `GEMINI_API_KEY` cannot redirect or re-bill the agent.

## The `settings.json` we write

Regenerated wholesale at each pod start by `gb setup gemini`. Pins `security.auth.selectedType=gemini-api-key`, `privacy.usageStatisticsEnabled=false`, `general.enableAutoUpdate{,Notification}=false`, `model.name=$GEMINI_MODEL`, `context.fileName=["GEMINI.md","CLAUDE.md"]` (so the shared workspace `CLAUDE.md` loads), plus translated hooks and `mcpServers` from `.mcp.json`. Override with `gemini-config` / `gemini-instructions` beads: `gemini-config` passes through an **allowlist** (`model`, `context`, `ui`, `output`), not a denylist, because most of the Gemini settings surface runs a program or redirects requests; the first-run gates cannot be unpinned. Golden: `cmd/gb/testdata/gemini_settings_crew.golden.json`.

Two things that differ from Claude and Codex and bite when debugging:

- **Hook timeouts are milliseconds** (Claude and Codex use seconds); `gb setup gemini` converts.
- **Plain hook stdout never reaches the model; only JSON `hookSpecificOutput.additionalContext` does.** The context hooks (`prime`, `load-state`, `check-mail`) are therefore skipped, not installed. Advice reaches a Gemini agent through `GEMINI.md`; mail arrives through the driver's input queue.

## Choosing Gemini

- **Role/project default:** `kd create '<role>' --type=role -l project:<p> -f harness=gemini -f account=<gemini-account>`.
- **One CLI spawn:** `gb spawn --harness gemini`.
- **One Slack thread:** `@bot --harness gemini <prompt>` or the shorthand `@bot --model gemini <prompt>`; add a model with `--harness gemini --model gemini-<id>`.
  - The channel's role must allow it: a role allows its own resolved harness plus anything in its `allowed_harnesses` field (comma list, e.g. `codex,gemini`). A default role is claude-only and the bot answers with the allowed set and how to widen it; it does not spawn.
  - The Slack model vocabulary is a per-harness table (`harnessModelProfiles`, `internal/bridge/bot_mentions_harness.go`). A Claude alias or id (`opus`, `fable`, `claude-*`) or a Codex id (`gpt-*`, `o3`, `codex-*`) is **refused** on Gemini with a message naming the right harness, and a Gemini id without `--harness gemini` is refused with the fix. A foreign id is never passed through, because the CLI does no local validation of `-m` and would send it to the API as typed.
  - **There are no short Gemini aliases yet** (`--model flash` is "Unrecognized Gemini model"). The ids the CLI lists (`gemini-3.1-pro-preview`, `gemini-3.8-flash`, `gemini-2.5-pro`, ...) are known, but which of them an API key can call, and what the API does with an unknown id (hard 404, or silent degrade as Codex does), is [LIVE]. Once the live pass answers that, add the aliases to `harnessModelProfiles[gemini].Aliases`; the resolver already consults the table and no other code changes. Full ids matching `gemini-...` work today; omit `--model` for the CLI's `auto` routing.
  - The override skips the warm pool (the pod bakes its harness at create time), is stamped on the agent bead, and survives restart and resume. A resumed thread cannot switch harness; start a new thread.

## Observability

- `gardenio_tool_latency_seconds` and `gardenio_bash_cmd_latency_seconds` carry `harness="claude|codex|gemini|unknown"`; `gardenio_tool_harness_overhead_seconds` (pushed by `gb init`) carries it too. Gemini's events are rewritten to the canonical hook vocabulary by the relay (`BeforeTool` -> `PreToolUse`, `AfterAgent` -> `Stop`), so one query covers all three harnesses: `sum by (harness) (rate(gardenio_tool_latency_seconds_count[5m]))`.
- beads-viewer shows a harness chip on `/agents` list rows for every non-claude harness (`codex`, `gemini`) and a `harness` badge on the agent detail card, from `resolved_harness`; role hubs already list the role's `harness`.
- **Context window.** Claude's window tables do not describe Gemini, so an agent with `harness=gemini` shows the raw `context_tokens` count and **no percentage** unless the agent bead carries `model_context_window` (the ACP `usage_update.size` the CLI reports; 1048576 on the spike). Nothing writes that field yet, for Codex or Gemini; until something does, "window unknown" is the honest reading.
- **Usage.** A Gemini pod has no Claude JSONL log. `gb` reads session usage from coop (`GET /api/v1/session/usage`, normalised from the ACP per-turn `usage`) and keeps a snapshot for the close-time record, exactly as for Codex. `TotalCostUSD` is 0 (ADR 0005 §5 C: no spend tracking for Gemini in v1; metered spend needs its own bead), and thinking/tool/error/compaction counts are not reported.
- **Death reports.** A Gemini session is read from coop's harness-neutral `hook_events.jsonl` / `state_events.jsonl`, not a Claude transcript (`deathtranscript.IsEventStreamHarness`). Under ACP only `BeforeAgent`, `AfterAgent`, `PreCompress` and `SessionEnd` fire as hooks, so tool calls reach those logs from the driver, not from CLI hooks; a death report for a Gemini agent may show fewer tool rows than a Codex one. [LIVE] to confirm on a real session.
- **Dashboards.** The per-harness Grafana panels need the `harness` label in the dashboard JSON, which lives in the helm lane (`gardenio/helm/**`) and is not part of this change; see the escalation on gd-sXOmQvpFWR.

## Google-account login (subscription / free tier, no per-token billing)

An account can authenticate with a Google-account login instead of `GEMINI_API_KEY`. Usage then counts against the Google account's Gemini CLI allowance (the free tier, or a Google AI Pro/Ultra or Gemini Code Assist plan) rather than the metered API. Check Google's current limits and terms: plan quotas change, and automated use of a consumer login is theirs to permit.

**Account.** `provider=gemini`, `credential=secret:<name>#oauth_creds.json`, label `project:<p>`. The `#api-key` credential (or none) stays the metered API-key path.

**How it works.**

| Step | Where |
|---|---|
| Controller mounts Secret `<name>` (key `oauth_creds.json`) read-only at `/var/run/gardenio/gemini` and sets `GEMINI_AUTH_MODE=oauth`; no `GEMINI_API_KEY`, no LiteLLM or Claude env | `applyGeminiAccountConfig` (`cmd/controller/gemini_credential.go`) |
| The spawn credential guard refuses a pod whose login Secret does not exist (transient, like the Claude/Codex guards), and `verifySecretsExist` gates pod creation on it | `credentialSecretRef`, `referencedSecretNames` |
| `gb setup gemini` pins `security.auth.selectedType=oauth-personal` and copies the mounted file to a writable `~/.gemini/oauth_creds.json` (0600); a missing or non-JSON login fails setup loudly | `installGeminiOAuthCreds` |
| coop's ACP driver calls `authenticate` with `oauth-personal` instead of `gemini-api-key` | `AuthMethod::from_env` (`driver/gemini/acp.rs`) |

There is **no keeper**: the CLI refreshes its own access token with the long-lived refresh token in `oauth_creds.json` and rewrites the file. Google does not rotate that refresh token on use, so every pod can start from the same Secret. (Codex differs: its refresh token rotates, hence the controller keeper and the placeholder.) Consequences: the Secret holds a real refresh token, so keep it out of reach of anything but the controller and agent pods; the pod keeps its own refreshed copy on the workspace PVC but is re-seeded from the Secret at each start.

**Provision it (operator, once per Google account).**

1. On a machine with a browser: `gemini`, choose *Login with Google*, finish the sign-in, quit. This writes `~/.gemini/oauth_creds.json`.
2. `kubectl -n <ns> create secret generic gardenio-gemini-login --from-file=oauth_creds.json=$HOME/.gemini/oauth_creds.json`
3. Create the account bead: `kd create --type account --title gardenio-gemini --field provider=gemini --field credential=secret:gardenio-gemini-login#oauth_creds.json --field billing_type=subscription --label project:<p>`, then point a role at it (`-f harness=gemini -f account=gardenio-gemini`).
4. Set `GEMINI_HARNESS_ENABLED=true` on the controller (still off by default; see Status).

[LIVE] not yet verified against a real login: that `authenticate {methodId:"oauth-personal"}` succeeds headless from a copied `oauth_creds.json` with no browser, that the CLI refreshes and rewrites it under ACP, and the real quota/429 behaviour on a subscription. Run these as part of the live pass (gd-AkOqbNHAym). Spend is not tracked for this path (same as the API-key path in v1).

## Debugging a Gemini agent

Start with the bead: `kd show <agent-bead>` -> `resolved_harness`, `harness`, `model`, `agent_state`, `last_session_id`. A spawn refused for a harness/account mismatch, or because `GEMINI_HARNESS_ENABLED` is off, fails the bead with the reason (`invalid agent harness: ...`); nothing is escalated, fix the role/account or enable the harness.

In the pod (you have no kubectl; `gb spawn --role infra --escalate` gets read-only cluster access):

1. **Binary and version:** `gemini --version`. The image pins the version and checksum in `images/agent/install/gemini.sh`; the spike ran 0.64.0.
2. **Auth:** coop pushes `GEMINI_API_KEY` after start, so `env | grep GEMINI` in a login shell will not show it. A real 401/403 surfaces as an `[gemini:unauthorized]` coop error; check the account's `credential` secret and `provider=gemini`.
3. **Config as Gemini sees it:** `cat ~/.gemini/settings.json ~/.gemini/GEMINI.md`. Setup logs a warning for each hook or MCP server it dropped, by name. A tool-name matcher on a tool event is refused, not installed as match-all.
4. **Gemini's own session record:** `~/.gemini/tmp/<slug>/chats/session-<minute>-<id8>.jsonl`. The file alone restores the whole history in an empty `$GEMINI_CLI_HOME` (`gemini --resume <id>`), which is what the persistence upload relies on. `session_id` in the relay events is this id.
5. **Hooks:** the relay is a one-shot process fed hook JSON on stdin. Replay a captured payload with `echo '<payload>' | gb hook relay` and compare to `cmd/gb/hook_relay_gemini_test.go`. On the bus, subscribe to `hooks.<agent>.>`; Gemini events have `harness: "gemini"`.
6. **coop's view:** `curl localhost:<port>/api/v1/agent` shows driver state (working/idle/error). `coop/permission` events record each approval answer; quota errors map to `RateLimited` so profile rotation applies.
7. **Stop gate:** `gb hook stop-gate --format gemini` emits `{"decision":"block","reason":...}` on stdout with exit 0 (Claude uses exit 2 + stderr; Codex JSON). The old `{"continue":true}` form fails open on 0.64.0. `cmd/gb/hook_stopgate_gemini_realbinary_test.go` proves a block produces a second model call.
8. **Memory:** coop caps the Node heap at `--max-old-space-size=1024` unless `NODE_OPTIONS` already sets one, and signals the whole process group at exit (the CLI re-spawns itself). An OOM-killed pod under the 2 GiB limit is [LIVE] to rule out.
9. **Input while a turn runs:** ACP replaces a running prompt when a second arrives, so coop serialises input; text sent mid-turn waits for the turn to end. Escape / Ctrl-C is `session/cancel`.

## Skills

Not translated yet: `gb setup gemini` does not link `.claude/skills/*` (Codex does). Follow-up bead.

## Not in v1 (follow-up beads)

Google-account OAuth / subscription login (non-goal, no keeper); LiteLLM routing and spend tracking for Gemini; `--format gemini` for the context hooks (`prime`, `check-mail`, `load-state`) so they can inject `additionalContext`; retiring coop's curl stop-gate shim; a headless `stream-json` per-turn backend (ACP is the supported mode); short Slack model aliases and a reported context window (both wait on the live pass); `lambda_microvm` and `--ultracode=on` (refused at spawn, as for Codex).
