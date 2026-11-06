# Wake a stopped agent when its CI run completes — plan of record

**Status:** plan of record, rev 3 (2026-10-30 status refresh; rev 2 of 2026-10-11 moved the wait
state onto the AGENT bead, not a new bead type — operator steer). P1 has shipped; P0 is open;
P2-P6 are not started. See the phase status below.
**Origin:** task `gd-7O9vhK7Z7m` (filed by the merge desk); Slack thread `gd-cDgwx9BdMq`.
**Goal:** an agent can `git push`, `gb wait ci`, `gb stop`, and be resumed (conversation
resumed) within ~1 min of CI concluding, with per-workflow conclusions in hand. Red and green
both wake it. A missed webhook still resolves via a poll fallback within N min.

**Phase status (2026-10-30):**

| Phase | State |
|---|---|
| P0 dedup | Open as PR #1324 (gd-ExCBfoRCTA). |
| P1 fields + `gb wait ci` + stop gate | **Shipped** in PR #1326 (9bd76ce6, gd-AwuRDezONo closed): `ci_wait_*` fields registered on `type:agent`, `WakeReasonCI`, `gb wait ci` (incl. `--timeout`, default 2h), the stop-gate park with `close_reason=ci_wait_parked`. Do not redo it. Interim follow-up gd-PwyvuF0fHp (c67f77a5): a parked stop also stamps `next_respawn_at` (25 min poll, clamped to the deadline) so the wake-scanner revives the agent before any resolver exists, and `gb wait ci --result` reads GitHub itself. |
| P2 ledger + resolver | Built on branch `agent/wake-on-ci/gd-WKrO492yYP` (gd-WKrO492yYP): pure core `controller/internal/ciwait` (ledger deduped on `workflow_run.id` + `run_attempt`, `Evaluate` with the 60 s quiesce rule), hosted in `cmd/github-bridge` (`ciwait.go`, Q4). Deliveries only touch an in-memory per-sha ledger; a 15 s sweep lists `ci_wait_state=pending` agent beads, mirrors the ledger onto `ci_wait_ledger`, and writes `ci_wait_state` + `ci_wait_result` in one PATCH. No wake yet (P3). |
| P3 wake | Built on branch `agent/wake-on-ci-p3/gd-T7aFA3tFoW` (gd-T7aFA3tFoW): the shared wake is `controller/internal/agentwake` (`Wake`: event-wake a dormant identity in place, thaw a frozen one, else `SpawnAgent`), used by the decision and escalation respawners and by github-bridge `ciwake.go`, which wakes the waiter right after `apply` writes a concluded wait (`wake_reason=ci`) and comments a nudge naming the red and green runs on `ci_wait_task`. The escalation follow-through now thaws a frozen requester too (it used to dedup into a no-op). |
| P4 poll fallback + deadline | Built on branch `agent/ci-wait-poll/gd-rntlUTWMtI` (gd-rntlUTWMtI), in `cmd/github-bridge/ciwait_poll.go`: `GET /repos/{r}/actions/runs?head_sha=` feeds the same ledger and `Evaluate`. A periodic poll per waiter (first after a 2 min grace, then every 5 min; `CI_WAIT_POLL_GRACE`, `CI_WAIT_POLL_INTERVAL`) and a confirm poll before a wait resolves when no poll has seen the ledger's last change. A wait with no runs seen at all now times out at its deadline (P2 left it pending forever). The `next_respawn_at` backstop at stop already shipped (gd-PwyvuF0fHp). The wake retry for `resolved`/`timed_out` unconsumed beads needs P3's wake and is left to it. |
| P5 docs + advice + wiring | Branch `agent/wake-on-ci/gd-8wxOTJ8dJe` (gd-8wxOTJ8dJe): `docs/triggers.md` section "Waking an agent when its CI concludes", github-bridge runbook "Wake on CI" (the k3s enable steps: hand-seeded Secrets, the `githubBridge` deploy-values block, repointing the App webhook to `https://hooks.clonk.tv/webhook`), merge desk prompt v16 (`docs/merge-desk.md`: a CI-pending PR is parked with `merge-desk:ci-wait` and the run ends on `gb wait ci` instead of being re-fired by the cron). Live delivery waits on the operator's deploy-values change and App repoint (helm_deploy escalation filed from gd-8wxOTJ8dJe). |
| P6 | Not started. |

## 1. What exists (verified in tree 2026-10-11; gaps refreshed 2026-10-30)

| Piece | Where | Gap for this feature |
|---|---|---|
| Park-behind-a-handle | `controller/cmd/gb/stop.go` `findParkHandle(ctx, taskID, agentID)`, `parkHandleTypes = {escalation, decision}` | A `blocks`-edge from the task to an OPEN handle whose `requesting_task` names the task exempts the claimed-work gate. **Closed by P1:** a pending wait on the agent's own bead is now the third exemption branch (no handle bead, no edge). |
| Respawn on resolution | `bridge/decisions.go` `respawnAgentForDecision`, `bridge/escalations.go` `respawnRequestingAgent` | Triggered by a HUMAN resolving. Both copies do the same four things: event-wake a dormant identity, thaw a frozen one, else `SpawnAgent` with a nudge prompt, carrying `plan_id`/`operated_by`/Slack fields. A third caller justifies extracting that into one `wakeRequestingAgent`. |
| Event-wake | `bridge/event_wake.go` `eventWakeDormantAgent`, `beadsapi/wake_fields.go` `WakeReason*` | Reopens a CLOSED `successor=same` agent bead IN PLACE (same name, `claude --resume`), stamps `wake_reason`, does not consume `respawn_count`. `BOAT_WAKE_REASON` reaches the pod for free. **Closed by P1:** `WakeReasonCI` exists (`beadsapi/wake_fields.go`). |
| Typed agent fields | `kbeads/internal/server/config.go`, `bridge/init.go` (subset guard test) | `type:agent` has no `allow_additional_fields`: an unregistered field 400s the whole PATCH. **Closed by P1:** the `ci_wait_*` fields are registered in both (`beadsapi/ci_wait_fields.go`). |
| Webhook intake | `bridge/trigger_source_webhook.go` `HandleWebhook` | HMAC-verified (`X-Hub-Signature-256`), captures `X-GitHub-Event`, buffers an event, but the action SPAWNS a fresh agent. Open: no resolver behind it (P2). |
| Second candidate intake | `controller/cmd/github-bridge` (`webhook.go`, `pipeline.go`; PRs #283/#285) | Already handles verified `check_suite`/`workflow_run` deliveries (stamps `mr_pipeline_status`/`mr_pipeline_url`) and dedups on `X-GitHub-Delivery`. Chart-only today: not enabled in `deploy-values/gardenio-k3s.yaml`, and its bead writes may need privileged-fleet access to PATCH agent beads. Which receiver hosts P2 is Q4. |
| Webhook dedup | `buildWebhookDedupKey` | Tries `id`, `event.id`, ..., `delivery_id`, then `action`. A GitHub `workflow_run` payload has no top-level `id`, so the key collapses to `<token>:completed` and only the first event ever fires. **Must key on `workflow_run.id` + `run_attempt`** (or `X-GitHub-Delivery`). Open: P0, PR #1324. |
| Ingress | helm `templates/trigger-engine/ingressroute.yaml` (`PathPrefix(/webhooks/)`) | Live: `deploy-values` enables it at `Host(hooks.clonk.tv) && PathPrefix(/webhooks/)`, TLS-only, rate-limited, `ipWhitelist` off on purpose (see Q2). |
| GitHub client | `bridge/github.go` | Read-only (tags/compare). No Actions API calls yet. |
| GitLab MR sync | `bridge/gitlab_sync.go` | GitLab-only, stamps status but never wakes the author. Not the path. |
| Cron re-fire | merge desk, 15 min | Works, adds latency, burns a spawn per tick. This feature replaces it for agents that know their sha. |

## 2. Design

### 2.1 The wait lives on the agent bead
`gb wait ci --repo OWNER/REPO --sha FULL_SHA [--workflow NAME ...] [--requesting-task T] [--timeout DURATION]`
stamps fields on the CALLING agent's own bead (one wait per agent; a second call replaces the
first). There is no new bead type, no handle bead and no `blocks` edge.

| Field | Meaning |
|---|---|
| `ci_wait_state` | `pending` -> `resolved` \| `timed_out` -> `consumed`. The only field the resolver filters on besides the key. |
| `ci_wait_key` | `OWNER/REPO@<40-char sha>`. One scalar so the resolver does a single server-side exact-match lookup per webhook event. |
| `ci_wait_workflows` | Optional comma-separated workflow-name filter. |
| `ci_wait_task` | The claimed task the wait parks. Defaults to the agent bead's `task_id`. Same role as `requesting_task` on a decision. |
| `ci_wait_deadline` | RFC3339: now + `--timeout` (default 2h). |
| `ci_wait_result` | JSON per-workflow `{name, run_id, run_attempt, conclusion}`, written at resolution. |

`gb stop` gains a third park branch in `findParkHandle`: the stopping agent's bead has
`ci_wait_state=pending` and `ci_wait_task == taskID`. If the state is already `resolved` or
`timed_out` the stop is REFUSED with "CI already concluded: run `gb wait ci --result`" (this
closes the race where CI finishes before the agent stops). `gb wait ci --result` prints the
result and sets `consumed`; `gb wait ci --status` is the read-only form.

On a pending wait, `gb stop` must leave the bead revivable: it closes with `successor=same`
(the existing dormant-identity marker, `isDormantParkedIdentity`) instead of terminal `none`
and `close_reason=ci_wait_parked` (`CloseReasonCIWaitParked`, `cmd/gb/stop.go`), and it takes the same freeze/suspend branch as today when the pod is freeze-eligible. The
sha must be the FULL 40-char sha: an abbreviated sha silently returns zero runs from the
Actions API (working-rules advice gd-oI2AIaSzjF).

### 2.2 Event source
A `workflow_run` (+ `check_suite`) GitHub webhook into trigger-engine (or into `cmd/github-bridge`,
which already receives these events; the receiver is open question Q4). A new bridge component,
not another `spawn_agent` trigger: on each verified event it looks up agent beads with
`ci_wait_state=pending` and `ci_wait_key=<repo>@<head_sha>` (server-side `Fields` filter,
any bead status — the waiting agent is usually CLOSED), and records the run's
`(workflow, run_id, run_attempt, status, conclusion)` into a per-key run ledger, deduped on
`workflow_run.id` + `run_attempt`. It does not use the `spawn_agent` action at all.

The ledger is per SHA, not per waiter: an agent pushes before it runs `gb wait ci`, so the
`requested`/`in_progress` deliveries for most runs arrive before any waiter exists. The bridge
keeps it in memory (6 h TTL) and mirrors it onto each waiter's `ci_wait_ledger` field, so a
restart keeps what was already seen. A delivery GitHub never sent, or one that arrived while
the bridge was down for a sha that had no waiter yet, is in no ledger: P4's Actions listing
feeds the same ledger.

### 2.3 "All runs concluded"
A wait resolves only when every run for the sha has `status=completed`. Subtleties to settle in
P2: a push can create runs lazily (a later workflow, a re-run bumping `run_attempt`, a
`workflow_run`-chained workflow), so "all" needs a quiesce window (proposed 60 s with no new
run for the sha) and, when `--workflow` is given, resolution is on exactly that set. Cancelled /
skipped / `action_required` count as concluded and are reported verbatim.

### 2.4 Resolution
In ONE atomic PATCH of the agent bead: `ci_wait_state=resolved` (or `timed_out`) and
`ci_wait_result=<json>`; then wake the agent through the shared `wakeRequestingAgent`
(extracted from `respawnAgentForDecision` / `respawnRequestingAgent`) with
`wake_reason=ci` and a nudge naming the red and green runs. That path event-wakes the dormant
identity in place, thaws a frozen one, or falls back to `SpawnAgent`. Because the identity is
reopened in place it keeps its name, its claimed task and its transcript. A duplicate delivery
finds `ci_wait_state != pending` and does nothing; a wake that fails leaves
`resolved` + unconsumed, which the sweep (2.5) retries.

### 2.5 Poll fallback
GitHub does not retry failed deliveries. A sweep (every N min, proposed 5) lists agent beads with
`ci_wait_state=pending` older than a grace period and calls
`GET /repos/{r}/actions/runs?head_sha=`; it resolves through the SAME code path as the
webhook. The sweep also retries the wake for any bead left `resolved`/`timed_out` and
unconsumed. `ci_wait_deadline` (proposed 2 h): at the deadline the wait resolves as `timed_out`
and still wakes the agent, so a lost wait can never strand a task forever. Independent
belt-and-braces to evaluate in P4: also stamp `next_respawn_at = deadline` at stop, so the
existing controller wake-scanner revives the identity even if the bridge is down. The client in `github.go` needs a token with
Actions read.

### 2.6 Security
Per-trigger secret verified with `verifyWebhookHMAC`; fail closed (no `allow_unsigned`).
A wait may only be created for a repo in the creating agent's project allowlist, and only
the agent whose bead carries the wait is woken (the resolver never takes an agent name from the
webhook payload).

## 3. Phases (each a step of the plan epic; one crew at a time per advice working rules)

- **P0 — Fix webhook dedup for GitHub events.** Make `buildWebhookDedupKey` key on
  `workflow_run.id` + `run_attempt` (and `check_suite.id`), falling back to `X-GitHub-Delivery`.
  Unit tests incl. two distinct runs on one token. Independent, small, ships alone.
- **P1 — Agent fields + `gb wait ci` + stop gate. SHIPPED (PR #1326).** Register the `ci_wait_*` fields on
  `type:agent` in kbeads `config.go` and `bridge/init.go` (subset-guard test covers both), add
  `WakeReasonCI`, add `gb wait ci` (`--repo --sha --workflow --requesting-task --timeout`
  (default 2h), `--status`, `--result`; stamps the caller's own bead; 40-char sha validation),
  add the third `findParkHandle` branch and the "already concluded" refusal, and make `gb stop`
  close with `successor=same` and `close_reason=ci_wait_parked` when a wait is pending. No event source yet; the wait just parks.
- **P2 — Run ledger + resolver.** Settle Q4 (which receiver) first. Bridge component that ingests verified `workflow_run` events,
  matches pending agent beads by `ci_wait_key`, implements the quiesce rule, writes per-workflow
  conclusions on the agent bead (`ci_wait_state=resolved`). Pure-function core with table tests for 2.3.
- **P3 — Wake.** Extract `wakeRequestingAgent` from the decision and escalation respawners
  (behavior-preserving, their tests stay green), then call it from resolution with
  `wake_reason=ci` + nudge text; dedup via `ci_wait_state`; red/green/timed_out all covered.
  Depends on P2.
- **P4 — Poll fallback + deadline.** `ListWorkflowRuns` in `github.go`, sweep, deadline path.
  Depends on P2. Shares the resolver, adds no second resolution path.
- **P5 — Docs + advice + wiring.** `docs/triggers.md` section, crew advice ("push, `gb wait ci`,
  `gb stop`; do not poll"), the operator runbook for the App webhook configuration, and moving
  the merge desk from cron re-check to `gb wait ci`. Depends on P3, P4.
- **P6 — E2E.** Replay a recorded `workflow_run` payload against a live trigger-engine and
  assert the wait resolves + agent is woken; poll-fallback e2e with the webhook suppressed.

Helm and `.github/workflows/**` are outside crew lanes (advice gd-oI2AIaSzjF). If the webhook
route or a secret mount needs a chart change, the step files the escalation rather than editing.

## 4. Open questions for the operator (decision beads, children of the epic)

- **Q1 — Handle type. RESOLVED (operator steer, 2026-10-11): neither.** No new bead type and no
  decision reuse; the wait is a set of fields on the agent bead (2.1). Trade-offs accepted:
  one pending wait per agent; the wait is invisible on the decision/escalation surfaces (it
  shows on the agent card via `wake_reason` and the `ci_wait_*` fields); new fields must be
  registered in two files. Gained: no `parkHandleTypes` change, no new TypeConfig, the existing
  event-wake path is reused, and the waiter is the lookup key.
- **Q2 — Who registers the GitHub webhook? RESOLVED (operator, 2026-10-11, gd-PdCxEi7Knl): the
  GitHub App.** Not a per-repo webhook registered by hand. The `gardenio-io` agents App
  (id 5029022, `plans/github-app-onboarding.md` P0) is configured with the trigger-engine webhook
  URL (`https://hooks.clonk.tv/webhooks/<token>`), a webhook secret, and the `workflow_run`
  (+ `check_suite`) event, and installed on `mattbakerconsulting/gardenio`. The event
  subscription and the install can only be done in the App settings UI (the API's
  `PATCH /app/hook/config` sets only URL/secret/content type), so it was an operator action:
  escalation gd-fyCexuAP9d, done 2026-10-11 (installation 170295668, set up before the repo moved
  from `joyousaversion`; P6 confirms deliveries still arrive). Consequences for the steps:
  - One App-level URL, so the trigger token is fixed per App, and the trigger sees every App
    event (`installation`, `ping`, ...). The trigger must condition on `X-GitHub-Event` and
    drop non-`workflow_run`/`check_suite` events.
  - The webhook secret is shared between the App settings and the trigger bead's
    `webhook_secret`; mint it once, store it in SSM `/gardenio/github-app/gardenio-io/webhook-secret`
    and rotate both together (`docs/runbooks/github-app-key-rotation.md`).
  - The App also covers Q3: its installation token carries `actions` (D9 ruled `write`), so the
    poll fallback can mint an Actions-read token without the crew PAT.
  - The trigger-engine ingress is live (decision gd-Bz9rkZg5Q0): `deploy-values/gardenio-k3s.yaml`
    `triggerEngine.ingress` sets `enabled: true`, `host: hooks.clonk.tv`. `ipWhitelist` is OFF
    on purpose: GitHub's `hooks` CIDRs are enforced at the node security group
    (`web_ingress_cidrs`). ServiceLB SNATs to the ClusterIP, so Traefik only sees 10.42.x
    addresses and a Traefik allowlist would block every delivery. Do not turn it on.
  - P5's runbook becomes "App webhook configuration", not "register a repo webhook". P5 and P6
    live verification wait on that escalation; P0-P4 do not (P6 can replay a recorded payload).
- **Q3 — Actions-read token for the poll fallback. ANSWERED by P4 (2026-10-31):** neither the
  crew token nor a controller token. The poll runs in `cmd/github-bridge` with its own
  installation token, minted with `githubapp.BridgeTokenPermissions()`, which carries
  `actions: read`. It needs the GitHub App configured on the bridge (no App, no poll: the
  resolver stays webhook-only and logs `poll_fallback=false`).
- **Q4 — Which receiver hosts the P2 intake? SETTLED by P2 (2026-10-31): `cmd/github-bridge`**, per
  the plan-captain steer on gd-WKrO492yYP. Its writes to agent beads need no bypass (`agent` is
  config-kind, `docs/machine-principal-inventory.md` section 4), so no privileged-fleet
  escalation is needed. Still open for P5/P6: github-bridge is chart-only (`githubBridge.enabled`
  is not set in deploy-values), so enabling it and pointing the App webhook at it is a
  deploy-values change for the operator. The core in `internal/ciwait` does no I/O, so a
  trigger-engine host would reuse it unchanged. The two candidates were:
  - trigger-engine at `hooks.clonk.tv/webhooks/<token>`: the App webhook already points here
    (gd-fyCexuAP9d) and it is the ONE public endpoint deploy-values exposes, but it needs P0's
    dedup fix and a non-`spawn_agent` handler.
  - `cmd/github-bridge` (#283/#285): already verifies `check_suite`/`workflow_run` deliveries and
    dedups on `X-GitHub-Delivery`, and the plan-captain drift note on P2 (gd-WKrO492yYP,
    2026-10-28) suggests hanging the ledger off it rather than standing up a parallel intake.
    But it is not enabled in deploy-values, the App webhook would have to be repointed (or the
    trigger-engine route forwarded), and its writes to agent beads may need privileged-fleet
    access (`docs/machine-principal-inventory.md`, github-bridge section), which is an
    escalation.
  The P2 crew settles this with the plan captain (a decision bead) before building; the run
  ledger, quiesce rule and resolver in 2.2-2.4 are the same either way.

## 5. Acceptance (from the origin task)
An agent can push, `gb wait ci`, `gb stop`, and be resumed within ~1 min of CI concluding with
the conclusions in hand. Red and green both wake it. A missed webhook resolves via the poll
fallback within N min (proposed 5 + grace).

## 6. Not verified
Whether GitHub delivers `workflow_run` for workflows triggered by `push` to a PR branch of a
private repo to a repo-level webhook without extra permissions (expected yes; P6 proves it).
Whether a freeze-eligible (runsc) agent that stops with a pending wait is thawed correctly by
`wakeRequestingAgent` when no decision bead is involved (P3 proves it with a test on the frozen
branch). Whether `gb stop` can safely stamp `successor=same` for a non-persistent role (the
wake-scanner keys on `next_respawn_at`, so it should ignore it; P1 asserts that).
