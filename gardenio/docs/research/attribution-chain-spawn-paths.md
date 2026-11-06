# Attribution Chain Across Spawn Paths

**Bead:** ka-0000007523 (parent epic: ka-0000005305)
**Author:** gardenio-research-attribution-chain-rq6
**Date:** 2026-05-13

Matt's framing: operatorship should flow `human → schedule/trigger → agent task → spawned tasks`.
This report walks every spawn path in the controller + beads daemon, identifies where
the chain is intact and where it breaks, and recommends fix shapes.

## TL;DR

Three of four spawn paths now push `operated_by` correctly to both the task bead and
the agent bead (Slack /spawn, scheduled, trigger-fire). The gardener → crew path
propagates `operated_by` via the `BEADS_OPERATED_BY` env var and `--operated-by` flag
default, but does **not** propagate `slack_user_id` and does **not** auto-add a
parent-child dep from crew-created beads to the gardener's task. `user_bead_id`
(FK to a real user bead) is **not** propagated through any spawn path — it is only
populated on the user bead itself at provisioning time. The two systemic breaks are
(a) `kd create` does not auto-link the new bead to `$BOAT_TASK_ID` as a parent-child
dep, and (b) the agent runtime has no env-var carrying `user_bead_id` even when
`operated_by` is set.

## How attribution flows today

### Two transport layers

1. **`X-Operated-By` HTTP header** (server-side authoritative).
   - Set by `beadsapi.WithOperator(ctx, op)` and read back via `OperatorFromContext`
     (`gardenio/controller/internal/beadsapi/client.go:188-203`).
   - The header is stamped on every outbound request
     (`client.go:1135-1139`) and the kbeads daemon writes it to the
     `beads.operated_by` column on create (immutable thereafter).
   - `kd`/`gb` CLI: `--operated-by` flag, defaulting to `$BEADS_OPERATED_BY`
     (`kbeads/cmd/kd/main.go:43-50,149`).
   - **This is the load-bearing path for agent-created beads.**

2. **`Bead.Fields["operated_by"]` JSON field** (push-at-spawn, used by R2 §3
   resolver fallback and for `slack_user_id` inheritance).
   - Pushed explicitly by spawn paths into the agent bead's `Fields` blob so
     `podmanager.operatedByFromMetadata` can read it and inject
     `BEADS_OPERATED_BY` into the agent pod env
     (`gardenio/controller/internal/podmanager/manager.go:758-762`).
   - `Fields["slack_user_id"]` rides alongside as a legacy fallback for the
     resolver and as the input to bot DM notifications.

### Schema fields

Source: `kbeads/internal/model/bead.go:125-206`, `kbeads/internal/server/config.go:214`.

- `beads.operated_by` (TEXT, immutable, set from header at create) — opaque
  typed id: `slack:U…`, `oidc:email`, `jira:accountID`, `gitlab:username`,
  `system:<source>-<role>`.
- `beads.operated_by_id` (FK, dual-write window per migration 016) — points
  at `operators(id)`.
- `beads.created_by_operator_id` (FK) — immortal originator; never changes
  on dibs handoff.
- `beads.created_by` (TEXT) — non-human actor that performed the write
  (e.g. `system:scheduler`, agent name).
- `Fields["operated_by"]`, `Fields["slack_user_id"]`, `Fields["spawned_by"]`,
  `Fields["user_bead_id"]` (user bead only) — JSON sidecar.

## Per-path walk

### 1. Slack `/spawn` → task bead + agent bead

Files:
- `gardenio/controller/internal/bridge/bot_commands.go:48` — handler entry stamps
  `ctx = beadsapi.WithOperator(ctx, b.operatorFromSlackUser(ctx, cmd.UserID))`.
- `bot_commands.go:237-244` — task bead create. No explicit `Fields` push,
  but `X-Operated-By` header rides on the request, so server stamps
  `beads.operated_by` correctly. `slack_user_id` is **not** written to the
  task bead's `Fields`.
- `bot_commands.go:528-538` — agent bead spawn. Explicitly pushes
  `operated_by`, `slack_user_id`, and `slack_spawn_channel` into
  `spawnFields`.

**Chain status:**

| Bead | `operated_by` column | `Fields[operated_by]` | `Fields[slack_user_id]` | `user_bead_id` | parent dep |
|---|---|---|---|---|---|
| Task | ✅ via header | ❌ | ❌ | ❌ | n/a |
| Agent | ✅ via header | ✅ pushed | ✅ pushed | ❌ | "assigned" dep to task (client.go:487-494) |

**Breaks:**
- Task bead lacks `Fields["slack_user_id"]`, so resolvers that only check
  `Fields` (not the column) miss it.
- Neither bead carries `user_bead_id`. There is no link from the bead to the
  provisioned user bead.

### 2. Scheduled spawn (`type=schedule` cron)

Files:
- `gardenio/controller/internal/scheduler/scheduler.go:140-148` —
  `Schedule.OperatedBy` + `SlackUserID` read from the schedule bead's
  top-level column + `Fields`.
- `scheduler.go:1140-1142` — resolves push value: prefers `OperatedBy`,
  falls back to `slack:<SlackUserID>`.
- `scheduler.go:1154-1159` — task bead `Fields["operated_by"]` + `["slack_user_id"]`.
- `scheduler.go:1185-1212` — agent bead pushes `operated_by`,
  `slack_user_id`, full schedule metadata, `spawned_by = "scheduler:<id>"`.
- Tests: `scheduler_test.go:999-1145` pin this invariant.

**Chain status:**

| Bead | `operated_by` column | `Fields[operated_by]` | `Fields[slack_user_id]` | `user_bead_id` | parent dep |
|---|---|---|---|---|---|
| Task | ✅ | ✅ pushed | ✅ pushed | ❌ | n/a |
| Agent | ✅ | ✅ pushed | ✅ pushed | ❌ | "assigned" + `schedule_id` field |

**Breaks:**
- No `user_bead_id` propagated.
- The schedule bead itself has no `user_bead_id` field on creation, so even
  if we added propagation, the source is empty.

### 3. Trigger fire (`trigger:bead`, jira/gitlab/webhook/slack-reaction triggers)

Files:
- `gardenio/controller/internal/bridge/triggers.go:47-64` —
  `ctxWithTriggerOperator` stamps either `trig.CreatedBy` (if typed) or
  `system:<source>-trigger` sentinel.
- `triggers.go:646-690` — `actionSpawnAgent`. Calls `createTaskBead`, then
  `SpawnAgent` with `Fields["operated_by"] = OperatorFromContext(ctx)`.
- `triggers.go:740-770` — `createTaskBead`. Also pushes
  `Fields["operated_by"]` and `trigger_id`, `trigger_name`, `trigger_created_by`.

**Chain status:**

| Bead | `operated_by` column | `Fields[operated_by]` | `Fields[slack_user_id]` | `user_bead_id` | parent dep |
|---|---|---|---|---|---|
| Task | ✅ | ✅ pushed | ❌ | ❌ | none |
| Agent | ✅ | ✅ pushed | ❌ | ❌ | "assigned" to task |

**Breaks:**
- No `slack_user_id` on either bead. For triggers whose `CreatedBy` is a
  Slack user, the typed operator id is `slack:U…` but the bare `slack_user_id`
  field is not extracted. This breaks bot-DM notification paths that read
  `Fields["slack_user_id"]`.
- No parent-child dep from the spawned task bead to the trigger bead itself
  — only `trigger_id` in `Fields`. Beads-viewer graph navigation cannot show
  "triggers that fired" as edges.

### 4. Gardener → crew (`gb spawn` from inside a gardener pod)

Files:
- `gardenio/controller/cmd/gb/spawn.go` — `spawnAndPrint` reads
  `os.Getenv("BEADS_OPERATED_BY")` (via `defaultOperatedBy`) and pushes it
  into crew spawn `Fields["operated_by"]`.
- `gardenio/controller/internal/podmanager/manager.go:239-244,758-762` —
  parent pod's `OperatedBy` (resolved from gardener agent bead's
  `Fields["operated_by"]`) is injected as `BEADS_OPERATED_BY`.
- `manager.go:758-762` test pin: `manager_build_test.go:157-188`.

**Chain status:**

| Bead | `operated_by` column | `Fields[operated_by]` | `Fields[slack_user_id]` | `user_bead_id` | parent dep |
|---|---|---|---|---|---|
| Task (if created by gardener) | ✅ via header | ❌ unless explicit | ❌ | ❌ | none unless gardener adds it |
| Crew agent | ✅ via header | ✅ pushed | ❌ | ❌ | "assigned" to task |

**Breaks:**
- `slack_user_id` is not in the gardener's pod env, so it cannot be propagated
  to crew. The resolver's typed `operated_by` id covers the audit path, but
  bot-DM notifications targeted at the original Slack user fail.
- Gardener-created task beads have no automatic parent-child dep to the
  gardener's own task (the epic). Gardeners must remember to
  `kd dep add <new-task> <epic> --type parent-child`. Many do not.

### 5. Agent-created beads via `kd create` (covers all four paths)

- `kd create` reads `--operated-by` from `$BEADS_OPERATED_BY` env, sends as
  `X-Operated-By`. Column is set server-side.
- `Fields["operated_by"]` is **not** auto-pushed by `kd create`.
- No automatic parent-child dep to `$BOAT_TASK_ID` (the agent's claimed
  task). The agent must explicitly run `kd dep add`.

This is the most common write path in the system and the source of the
"old issues default-assigned to Matt / orphaned children" pattern Matt
flagged in the epic.

## The breaks, ranked

1. **`kd create` does not auto-link to `$BOAT_TASK_ID` parent.** Every
   agent-created bead is born orphaned unless the agent remembers
   `kd dep add`. Highest-impact, highest-frequency break.
2. **`kd create` does not push `Fields["operated_by"]`.** The column is set
   from the header, but the JSON sidecar is empty, so push-at-spawn
   resolvers and beads-viewer queries that read `Fields` miss it.
3. **`user_bead_id` never propagates through spawn.** User provisioning
   creates the user bead; nothing in any spawn path stamps the new bead
   with its FK. Required to make "what did I start" filters work.
4. **`slack_user_id` lost in trigger and gardener→crew paths.** Bot-DM
   notification targeting fails for these spawns.
5. **Slack `/spawn` task bead lacks `slack_user_id` field.** Minor
   parallel to (4).
6. **No parent-child dep from trigger-spawned task → trigger bead.** Only
   `Fields["trigger_id"]`. Beads-viewer cannot render the edge.

## Recommended fix shape

Layered: server-side defaulting + spawn-time push + CLI defaults.

### Server-side: inheritance at create-time

In the kbeads create handler, when the caller does not supply
`Fields["operated_by"]` or `user_bead_id`, **default from the request
context** (header for `operated_by`; user-provision middleware-resolved
`UserBeadIDFromContext` for the FK). This collapses all the "push at every
spawn site" code into one rule. Specifically:

- If `req.Fields["operated_by"]` is empty and `X-Operated-By` header is set,
  populate the field automatically.
- Add a new `X-User-Bead-Id` header populated by the user-provisioning
  middleware. Server defaults `Fields["user_bead_id"]` from it on create.
- Add server-side **auto-parent inheritance** for agent-authored writes:
  when the actor (`X-Actor` header) matches a known agent bead and that
  agent bead has `Fields["task_id"]`, auto-add a parent-child dep from
  the new bead to that task. Make this opt-out per call
  (`X-No-Auto-Parent: 1`) for cases like the agent creating a sibling
  task or a global advice bead.

### CLI: defaults from env

- `kd create` already defaults `--operated-by` from `$BEADS_OPERATED_BY`.
- Add `--parent-task` flag defaulting to `$BOAT_TASK_ID`. When set, post-create
  call `kd dep add <new-id> <parent-task> --type parent-child` automatically.
- Add `--no-parent` opt-out for explicit unparented creates.

### Spawn-site cleanup

Once server-side inheritance lands, the per-site
`Fields["operated_by"] = OperatorFromContext(ctx)` pushes in
`triggers.go`, `bot_commands.go`, and `scheduler.go` become redundant
and can be deleted. The remaining per-site pushes are spawn-specific
metadata (`schedule_id`, `trigger_id`, `slack_spawn_channel`) — those
stay.

### `user_bead_id` end-to-end

- User provisioning (already implemented in
  `kbeads/internal/server/user_provision.go`) creates/looks-up the user
  bead and stamps `UserBeadIDFromContext`. Wire this into:
  - The X-Operated-By → X-User-Bead-Id propagation rule above.
  - The gardener pod env: inject `BEADS_USER_BEAD_ID` alongside
    `BEADS_OPERATED_BY` so `kd`/`gb` can send it on every request from
    within a gardener.
  - The scheduler: when the schedule bead has `Fields["user_bead_id"]`,
    push it into both task and agent beads. Backfill schedule beads with
    a one-time migration that resolves `OperatedBy` → user bead.

### Slack-user-id legacy fallback

Keep pushing `Fields["slack_user_id"]` at Slack-originated spawn sites
(it's the input to bot-DM targeting), but extract it server-side from a
`slack:U…` `operated_by` value as a derived field — that fixes the
trigger and gardener→crew breaks without per-site code.

## Out-of-scope for this report

- Roles/permissions model (parent epic problem area 1) — separate report.
- Legacy assignee migration (problem area 4) — separate report.
- Beads-viewer filter UI for "what did I start" (problem area 5) — separate.

## Follow-up beads (recommended)

- **task** (P1, blocks all others): server-side `Fields[operated_by]` and
  `user_bead_id` inheritance from request context at create-time.
- **task** (P1): `kd create --parent-task` defaulting from `$BOAT_TASK_ID`,
  plus `BEADS_USER_BEAD_ID` env injection in podmanager.
- **task** (P2): server-side derivation of `slack_user_id` from
  `slack:U…` operated_by.
- **task** (P2): backfill schedule beads with `user_bead_id`.
- **task** (P3): one-time migration to add parent-child deps for
  agent-orphaned beads created in the last N months, using `created_by`
  + `task_id` correlation.
