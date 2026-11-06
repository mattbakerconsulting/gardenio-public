# Scheduler Over-Fire Root Cause (ka-0000002126)

Investigation of why schedule `ka-0000004432` (cron `"7,37 * * * *"`,
`America/Los_Angeles`) fired ~5×/hr instead of the configured 2×/hr.

## Symptom

Bead `ka-0000004432` fires at UTC minutes like `:31`, `:35`, `:40`, `:44`, `:50`
with consistent second offset (`:21`–`:22`). The configured cron only matches
`:07` and `:37` of each hour in LA local time. Fires are ~5× the expected
cadence.

## Root Cause

Atomic all-or-nothing bead field update fails schema validation on an unknown
field (`runs_spawned`) and `last_run` never persists. The scheduler's
"already-fired-for-this-slot" guard relies on `LastRun`, so it repeats a fire
that it believes never happened on every tick where the previous agent has
already finished.

1. `gardenio/controller/internal/scheduler/scheduler.go:296-303` writes three
   fields atomically on every spawn:

       s.daemon.UpdateBeadFields(ctx, sched.ID, map[string]string{
           "last_run":      now.UTC().Format(time.RFC3339),
           "last_agent_id": agentID,
           "runs_spawned":  strconv.Itoa(sched.RunsSpawned),
       })

2. `kbeads/internal/server/config.go:624-646` builtin `type:schedule`
   TypeConfig does **not** declare `runs_spawned`, `runs_completed`,
   `max_runs`, `terminal_goal`, `exit_condition`, `on_exit`, or `run_timeout`.
   These were introduced in commit `89ca538e` (phase 1 of the
   ephemeral-scheduled-gardeners proposal) in
   `gardenio/controller/internal/bridge/init.go:707-713`, but the matching
   kbeads builtin schema was never updated. The stored `type:schedule` config
   in the live DB is also unpatched.

3. When the scheduler calls `UpdateBeadFields` the daemon rejects the entire
   patch:

       HTTP 400: invalid fields: validation failed:
         runs_spawned: unknown field

   Controller log (pod `gardenio-agents-77bc7868b9-txklc`):

       {"time":"2026-04-24T00:20:22.239960551Z","level":"WARN",
        "msg":"failed to update schedule last_run",
        "component":"scheduler","schedule":"ka-0000004432",
        "error":"updating fields on bead ka-0000004432: HTTP 400:
                 invalid fields: validation failed: runs_spawned: unknown field"}

4. Because the write is all-or-nothing, `last_run` is **never** persisted.
   `kd show ka-0000004432 --json` still reports:

       "last_run": "2026-04-23T17:07:14Z"

   while the most recent "Run started → …" comment on the same bead is at
   `2026-04-24T01:50:22Z` — an ~8h lag that matches the last successful update
   before commit `89ca538e` landed on the controller.

5. `gardenio/controller/internal/scheduler/scheduler.go:177-194` treats the
   stuck `LastRun` as "we have not yet serviced the previous cron slot":

       lastFire := expr.Prev(nowLocal).UTC()
       ...
       if !sched.LastRun.IsZero() && !sched.LastRun.Before(lastFire) {
           continue
       }

   `Prev(nowLocal)` returns the most-recent matching minute. `LastRun` sits at
   `2026-04-23T17:07:14Z`, which is always before today's `Prev`, so the guard
   falls through and the schedule is eligible to fire every tick.

6. The scheduler ticks every 60s
   (`gardenio/controller/cmd/controller/main.go:368`,
   `scheduler.RunLoop(ctx, syncInterval)` at `main.go:480`). A fire does not
   actually happen on every tick because
   `gardenio/controller/internal/scheduler/scheduler.go:203` skips when the
   previously-spawned agent is still running. Gaps in the observed `:31 :35
   :40 :44 :50` pattern are runs of the prior agent; when one finishes the
   next tick fires a new one immediately. The constant `:21–:22` seconds
   offset is the fixed `time.NewTicker(60s)` phase from process start
   (controller pod `AGE 3h11m` at the time of capture).

## Ruled-Out Hypotheses

- **Cron parser over-matches.** `gardenio/controller/internal/scheduler/cron.go`
  parses `"7,37 * * * *"` strictly as 5 fields and `Prev()` returns exactly
  one of the two configured minutes; not the source of the extra fires.
- **Timezone / catch-up fires.** `nowLocal := now.In(loc)` and
  `expr.InWindow()` correctly constrain to the current day/hour; the parser
  does not synthesize missed fires.
- **Multi-pod leader election.** `kubectl -n gardenio get pods` shows a single
  `gardenio-agents-77bc7868b9-txklc` controller pod; `kubectl -n gardenio get
  deploy gardenio-agents -o yaml | grep replicas` is `1`. Not a race.
- **DB transaction race on `last_run`.** `UpdateBeadFields` is a single
  transactional write; no concurrent writer observed. The write itself is
  rejected, not overwritten.

## Related (Secondary) Schema Drift

`type:agent` was previously also missing the five `schedule_*` fields written
by `spawnScheduledAgent` — `schedule_id`, `schedule_title`, `schedule_cron`,
`schedule_slack_channel`, `schedule_name`. The stored config for `type:agent`
shows `updated_at: 2026-04-24T00:17:44Z`, indicating an operator patched the
stored config at that time but did not patch `type:schedule` or the
`config.go` builtins. Keeping builtins in lockstep with the controller is the
durable fix so fresh installs are not broken out of the box.

Commit `89ca538e` (ephemeral-scheduled-gardeners phase 1) also declared these
fields in `gardenio/controller/internal/bridge/init.go:128-132` and `:707-713`
for a future `SetConfig` call, but nothing propagates those declarations into
the kbeads daemon builtins at this time.

## Fix (Applied in this MR)

1. Add the seven new run-budget fields to the `type:schedule` builtin in
   `kbeads/internal/server/config.go:624-646`:

       {"name": "max_runs",       "type": "integer"},
       {"name": "runs_spawned",   "type": "integer"},
       {"name": "runs_completed", "type": "integer"},
       {"name": "terminal_goal",  "type": "string"},
       {"name": "exit_condition", "type": "string"},
       {"name": "on_exit",        "type": "string"},
       {"name": "run_timeout",    "type": "string"},

2. Add the five `schedule_*` context fields to the `type:agent` builtin in
   `kbeads/internal/server/config.go:180-184` so new installs do not rely on
   the live DB patch applied on `2026-04-24T00:17:44Z`. (These fields were
   the pre-existing fix for the `type:agent` half of the same drift.)

3. Add a `type:agent` field `schedule_prior_agent_id` (declared in the
   controller but previously also unknown to the daemon).

Once the kbeads daemon carrying these builtins is deployed, the next scheduler
tick will successfully persist `last_run`, the `LastRun.Before(lastFire)`
guard will re-engage, and the schedule will return to 2×/hr.

## Follow-Ups (Not in This MR)

- Filed as a separate bug bead: make `UpdateBeadFields` in the scheduler
  resilient to schema drift — for example, retry `last_run`/`last_agent_id`
  as a standalone call if the combined update fails with an unknown-field
  400, or split the combined update into two calls. This prevents a
  schema-mismatch on a *new* bookkeeping field from silently wedging the
  core "mark slot serviced" invariant.
- Reconsider whether controller bookkeeping counters (`runs_spawned`,
  `runs_completed`) belong on the *schedule* bead at all, or whether they
  should live on the gardener-bead hierarchy where they originate.

## Resolution (ka-0000003268)

Resilience follow-up shipped. The scheduler now splits
`last_run`/`last_agent_id` (load-bearing for cadence) from `runs_spawned`
(bookkeeping) into two separate `UpdateBeadFields` calls at
`gardenio/controller/internal/scheduler/scheduler.go:296`. A future schema
drift on any bookkeeping counter will surface as a noisy warning on the
second call instead of silently dropping the first. Regression guarded by
`TestUpdateSchedule_SplitsLoadBearingFromBookkeeping` in
`gardenio/controller/internal/scheduler/update_resilience_test.go`, which
simulates the unknown-field rejection and asserts `last_run` still
persists. See the MR linked from bead `ka-0000003268`.
