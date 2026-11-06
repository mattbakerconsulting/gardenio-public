# Minimal identity-only GardenerSpec backfill shape (behavior-preserving)

**Task:** [ka-0000007784] · **Epic:** [ka-0000000380] (Backfill GardenerSpec engine
blobs onto legacy spawn_agent gardeners) · **Written:** 2026-08-06

> **DESIGN ONLY.** This document defines the exact `gardener` blob to backfill and
> proves it is behavior-preserving against the live engine. It mutates no gardener
> bead. Enumeration is [ka-0000000351] (manifest
> `gardenio/docs/gardener-spec-less-manifest-2026-06-15.md`); the repeatable
> procedure/script is [ka-0000007453]; the execute-and-verify run is
> [ka-0000001186]. Read those in that order.

---

## 1. The problem, precisely

A legacy gardener is a `type=trigger` (or `type=schedule`) bead with
`role:gardener` and `action=spawn_agent` but **no `gardener` GardenerSpec blob**. The
engine only bumps run counters when `trig.Gardener != nil`
(`gardenerPostFire`, `gardener_evaluator.go:1267`; `gardenerAgentTracker`,
`:1439`), so a spec-less gardener never accrues a native tally. beads-viewer
papers over this by DERIVING the count from iteration agent beads at render time
(`enrichGardenerRunCounts`, `gardeners_enrich.go:66`, bug [ka-0000005661]) — a
display-only repair that stays load-bearing forever.

The fix is to write a minimal `gardener` blob onto each legacy gardener so (a) the
historical tally is seeded and (b) the engine tracks future runs natively.

**The whole risk lives in one line.** `evalOne` gates the entire gardener machine
on a single nil check:

```go
// triggers.go:2240
if trig.Gardener != nil {
    if skip, reason := gardenerPreFireCheck(ctx, e.daemon, e.logger, trig); skip {
        return
    }
    if gardenerEvaluateExitCondition(ctx, e.daemon, e.logger, trig) {
        return
    }
}
```

A spec-less trigger bypasses `gardenerPreFireCheck` **entirely** — none of the
run_budget / stop / concurrency / dormancy / crashloop gates run. Writing *any*
non-nil blob flips this trigger through the full gate stack on the next reconcile.

So "behavior-preserving" is **not** only "don't set run_budget/stop." It is: *for
every gate that newly activates when `Gardener != nil`, prove that an identity-only
blob makes it a no-op that fires exactly as the spec-less trigger did.* Sections 4
and 5 do that gate-by-gate. Two of those gates are non-obvious traps (§5); the
shape in §3 is built to defuse both.

---

## 2. The shape (copy-paste reference)

The blob is **identity + counters only**. Every field is either an identity
string or an engine-owned counter seeded to its already-true value. No field that
carries loop semantics is present.

```json
{
  "name":              "<schedule_name, else cleaned trigger title>",
  "terminal_goal":     "<best-effort one-liner from title/prompt>",
  "runs_spawned":      <derived spawned count>,
  "runs_completed":    <derived completed count>,
  "last_agent_id":     "<head of the life-thread, newest keyed agent bead>",
  "last_checked_agent": "<SAME value as last_agent_id>"
}
```

That is the entire blob. Six keys. Concretely, for `ka-0000003150` (Gardener Page,
55/55, head `ka-0000001396`):

```json
{
  "name":               "Soup-to-nuts gardener: Gardener Page (epic ka-0000001579)",
  "terminal_goal":      "Drive epic ka-0000001579 (Gardener Page) to done.",
  "runs_spawned":       55,
  "runs_completed":     55,
  "last_agent_id":      "ka-0000001396",
  "last_checked_agent": "ka-0000001396"
}
```

### 2.1 Field-by-field

| Field | Required | Source | Why |
|---|---|---|---|
| `name` | yes | `schedule_name` field; else the trigger title with the `Gardener:`/`Scheduled:` prefix stripped (mirror `deriveAgentName` framing, `triggers.go:3686`) | Identity only — read by `gb gardener status` and the viewer card. Never gates firing. |
| `terminal_goal` | yes | best-effort one-liner from the title/prompt (name the anchor epic when the title carries `(epic kd-…)` / `(kd-…)`) | Identity/observability. The **master gate** for premature-stop resume, but with no stop spec present it is never consulted (§4). |
| `runs_spawned` | yes | `deriveGardenerRunCount` spawned (`gardeners_enrich.go:115`) | Seeds the historical tally so the viewer stops deriving. Engine-owned; `gardenerPostFire` increments from here. |
| `runs_completed` | yes | `deriveGardenerRunCount` completed | Same, for the completed tally. |
| `last_agent_id` | yes | newest-`created_at` keyed (`trigger_id`/`schedule_id`) agent bead | Head of the life-thread. Populates `LineageRoot` idempotently and the "last run" viewer peek. |
| `last_checked_agent` | yes | **equal to `last_agent_id`** | **De-dup guard (§5.2).** Without it, `gardenerAgentTracker` re-credits the head agent on the next tick — a phantom `+1` to `runs_completed`. |

Seed `runs_spawned` / `runs_completed` / `last_agent_id` from
`deriveGardenerRunCount` — the **exact** function the viewer already displays — so
the seeded native tally equals the number operators already see. No new number is
invented; the backfill freezes the current derived value into the blob.

### 2.2 Counters as top-level fields too

`gardenerPostFire` and `gardenerAgentTracker` write the counters BOTH into the blob
AND up to the registered INTEGER-typed top-level fields `runs_spawned` /
`runs_completed` / `last_agent_id` via `UpdateBeadFieldsTyped`
(`gardener_evaluator.go:1301`, `:1566`; the top-level mirror exists because bug
[ka-0000007976] froze counters when they were sent as strings). The backfill write
should mirror the same shape: set the blob **and** the three typed top-level
fields in one write, integers as JSON numbers (never strings, or kbeads
400-rejects the whole PATCH and silently drops the blob with it). The blob is the
source of truth `ParseTriggerDef` reads (`triggers.go:3883`); the top-level fields
are the external-observer mirror.

---

## 3. What is DELIBERATELY absent, and why each omission is safe

The GardenerSpec struct (`triggers.go:500`) has ~40 fields. The identity-only shape
uses 6. Every omitted field is safe because **the engine treats its zero/empty
value as "no limit / not configured / legacy behavior"** — which is exactly what a
spec-less trigger already does. This is the omitted-field safety proof the task
asks for.

| Omitted field | Zero value | Engine reads zero/empty as | Evidence |
|---|---|---|---|
| `run_budget` | `0` | **no run ceiling.** Gate 1 is `RunBudget > 0 && …` — a `0` never trips it. | `gardener_evaluator.go:250` |
| `max_runs` | `0` | inert. Only meaningful inside a `stop.loop_end` spec, which is absent. | (stop gate skipped, §4) |
| `stop` | `""` (nil `RawMessage`) | **no stop conditions.** `len(capSpec.Stop) > 0` is false → the entire stop-eval block (`ParseStopSpec` → `EvaluateGardenerStopSpec`) is skipped. | `gardener_evaluator.go:342` |
| `exit_condition` | `""` | inert. `gardenerEvaluateExitCondition` returns false immediately on empty. | `gardener_hooks.go:335` |
| `on_exit` | `""` | never consulted without a stop/exit match. | — |
| `progress` | `""` | **no progress tracking.** `gardenerDiscoverProgress` (`gardener_hooks.go:80`) and `gardenerFinalizeRunsFromIterations` (`:171`) both no-op on empty Progress. Critical — see §5.1. | `gardener_hooks.go:80,171` |
| `retry_on_failure` / `max_retries` | `false`/`0` | no retry policy. Legacy behavior. | — |
| `run_timeout` / `timeout` | `""` | no per-run/loop timeout. | — |
| `resume_budget` / `run_budget_refill` | `0` | "use default" — but only reached FROM a premature stop, which never fires with no stop spec + no positive run_budget (§4). Dead code path here. | `gardener_evaluator.go:452` |
| `max_second_lives` / `second_life_count` | `0` | second-life adjudicator only runs on a gardener CLOSED on max_runs; this gardener never closes on max_runs. | `gardener_second_life.go` |
| `agent_daily_budget_usd` | `0` | per-agent token guard; dark-gardeners only, and "0 ⇒ default" only enforced for `IsDarkGardener`. None of the target set is dark (§5.3). | `gardener_evaluator.go:207` |
| `one_spawn_at_a_time` | `false` | gate 2 (`OneSpawnAtATime && LastAgentID != ""`) short-circuits on the false flag. | `gardener_evaluator.go:258` |
| `max_concurrent_runs` | (top-level field, not in blob) | **LEAVE UNSET ⇒ default cap 1** — this is the trap in §5.1. Handled by NOT lowering `max_concurrent`. | `gardener_evaluator.go:1116` |
| `standing` | `false` | not standing. Legacy gardeners that ARE standing already carry the `perpetual:`/`standing:` label, which `gardenerIsStanding` reads independently of the blob (`standing_gardener.go:68`), so standing-ness is preserved with or without this field. | `standing_gardener.go` |
| `assign_bead` | `""` | not subtree-pinned. Keeps per-fire task-close semantics as-is (§5.4). | `gardener_hooks.go:56` |
| `chain` | (separate `chain` field, not in blob) | no succession heir. Untouched by this backfill. | `triggers.go:3894` |
| all `dormant_*` | `false`/`""` | not dormant. Proactive-dormancy gates re-derive from the live frontier each tick, not from these fields (§5.5). | `gardener_evaluator.go:222` |
| `wall_clock_since` / `runs_finalized_after` | zero time | never revived / no finalize baseline. Legacy behavior (credit all). | `gardener_evaluator.go:648,668` |
| `run_log` / `progress_state` / `consecutive_failures` / `lineage_root` / `resumes_without_progress` / `last_resume_*` | empty/`0` | engine-owned runtime state; starts empty and the engine fills it forward. Seeding them adds risk for zero identity/tally benefit. | — |

**Rule of thumb the shape encodes:** seed only what is *retrospectively true and
observable* (identity + historical counters + life-thread head). Leave every field
that *forward-controls the loop* at its zero value, because a spec-less trigger's
loop is already uncontrolled (it just fires on cron), and zero = uncontrolled.

---

## 4. Why the stop / budget / resume machine stays dormant

The premature-stop → resume-or-escalate machine ([ka-0000005353]) is the scariest
thing a GardenerSpec can wake up. With the identity-only shape it is provably
unreachable:

- **Gate 1 (run_budget):** `capSpec.RunBudget > 0 && RunsSpawned >= RunBudget`.
  `RunBudget == 0` ⇒ false, always. Never enters `gardenerResumeOrEscalate`.
  (`gardener_evaluator.go:250`)
- **Gate 3 (stop spec):** guarded by `len(capSpec.Stop) > 0`. Empty ⇒ the whole
  `ParseStopSpec`/`EvaluateGardenerStopSpec` block is skipped, so no `max_runs`,
  `wall_clock`, `consecutive_failures`, `no_progress_for`, or `goal_satisfied`
  kind can match. (`gardener_evaluator.go:342`)
- **Exit condition:** empty ⇒ `gardenerEvaluateExitCondition` returns false.
  (`gardener_hooks.go:335`)

Because neither premature entry point can fire, `terminal_goal`, `resume_budget`,
`run_budget_refill`, `max_second_lives`, `on_exit`, and the entire `WallClockSince`
/ `RunsFinalizedAfter` revival apparatus are inert. `terminal_goal` is stored for
identity/observability only; it is a *gate input* solely inside the resume path,
which is dead here. This is the core behavior-preservation guarantee: **the
gardener still just fires on its cron and spawns, exactly as before.**

One caveat, log-only: `warnUnboundedGardener` (`triggers.go:1906`) fires a
one-shot WARN log for any non-nil Gardener that lacks a positive `run_budget` AND a
bounded `hard_stop` wall_clock (`ValidateGardenerSpec`, `gardener_bounds_guard.go:189`).
The identity-only blob is "unbounded" by that definition, so each backfilled
gardener logs one warn-with-grace line and **keeps firing**. This is intended
warn-with-grace for live gardeners that predate the bounds guard — it changes no
firing behavior. (New gardeners are fail-closed at authoring time in
`cmd/gb/{gardener,trigger}.go`; the backfill is explicitly the grandfathered path.)

---

## 5. The five gates that DO newly activate — and why the shape neutralizes each

These are the non-obvious ones. A spec-less trigger skipped all of them; the blob
turns them on. Each must be a no-op for the shape to be behavior-preserving.

### 5.1 Concurrency cap (gate 2a) — THE headline trap

`gardenerAtConcurrentRunsCap` (`gardener_evaluator.go:1141`) runs for **every**
non-nil Gardener and defers the fire when live agents ≥ the effective cap. The cap
is `gardenerMaxConcurrentRuns` (`:1116`):

```go
func gardenerMaxConcurrentRuns(trig TriggerDef) int {
    if trig.MaxConcurrentRuns > 0 { return trig.MaxConcurrentRuns }
    return 1   // ← unset defaults to 1
}
```

`MaxConcurrentRuns` is the top-level `max_concurrent_runs` field
(`triggers.go:3829`), **not** in the blob, and it is unset on every legacy gardener
(verified across the manifest sample). So the moment a blob lands, a gardener that
was firing under the spec-less path's `max_concurrent` lease (default **3**,
`triggers.go:3828`) is suddenly capped at **1 live run**.

For the ~1-cadence single-run gardeners this is invisible (they were effectively
serial anyway). But the manifest has gardeners at `max_concurrent=3`
(`ka-0000003150`, `ka-0000000599`, `ka-0000007084`, `ka-0000000196`, …). For those,
blob-only backfill silently changes 3→1 — a real firing-behavior regression.

**Resolution (behavior-preserving):** when a target gardener's `max_concurrent > 1`,
the backfill MUST also set `max_concurrent_runs` to that same value, so the newly
active gardener concurrency cap reproduces the pre-blob effective concurrency.
Gardeners already at `max_concurrent=1` need nothing (cap 1 == default 1). This is a
mechanical, per-gardener, derived value — NOT an operator opt-in — because its
purpose is to *preserve* behavior, not change it.

> Procedure note for [ka-0000007453]/[ka-0000001186]: read `max_concurrent` per
> gardener; if `> 1`, include `max_concurrent_runs=<max_concurrent>` in the same
> typed write as the blob. Confirm in the dry-run diff.

### 5.2 Agent-tracker double-count — the counter trap

`gardenerAgentTracker` (`gardener_evaluator.go:1438`) runs at the top of `evalOne`
whenever `trig.Gardener != nil && trig.Gardener.LastAgentID != ""`
(`triggers.go:2178`). Seeding `last_agent_id` (required by the task) satisfies that
guard. The tracker then:

1. de-dups on `capSpec.LastCheckedAgent == capSpec.LastAgentID` (`:1448`);
2. if not de-duped and the head agent is closed, appends a RunLog entry AND
   **bumps `RunsCompleted`** (`:1500`).

So a blob with `last_agent_id` set but `last_checked_agent` empty would, on the
very first reconcile, re-process the already-counted head agent: `runs_completed`
goes to `seeded+1` and a duplicate RunLog entry lands. The seeded tally would be
wrong by one on day one.

**Resolution:** seed `last_checked_agent = last_agent_id`. The de-dup at `:1448`
then short-circuits immediately, the head agent is never re-credited, and the
seeded tally is stable. This is why `last_checked_agent` is a **required** field in
§2 even though the task text lists only the other five — it is the guard that makes
seeding `last_agent_id` safe.

### 5.3 Dark-gardener blast-radius caps (gate 0b)

`evaluateDarkMRCaps` / `enforceDarkAgentDailyBudget` (`gardener_evaluator.go:186`)
run only when `IsDarkGardener(trig.Labels, trig.Project)` is true
(`dark_gardener.go:44` → `beadsapi.IsDark`). None of the target set carries a
dark label / dark project. **Verify per gardener in the procedure** (a stray dark
label would newly subject the gardener to MR-rate and per-agent-token caps); for the
enumerated set this gate is a no-op.

### 5.4 Per-run task-bead close (`gardenerClosePerRunTaskBead`)

Runs when `Gardener != nil && LastAgentID != ""` and `AssignBead == ""`
(`gardener_hooks.go:52`). It closes the *per-run task bead* of the **head agent**
only, and only if that task is still open and its `schedule_id` matches the gardener
(`per_run_task_close.go:44`). For a mature legacy gardener the head agent's run is
long finished and its per-run task already closed, so `task.Status == "closed"` ⇒
no-op (`:58`). Worst case (head agent's task somehow still open) it closes exactly
that one stale task — the same close the scheduler would have done. Idempotent and
bounded; no cadence effect. Leaving `assign_bead` empty preserves the spec-less
"one task per fire" model. **Do not set `assign_bead`** — doing so would flip the
gardener to subtree mode and change `fireAction`'s task-creation branch
(`triggers.go:2826`).

### 5.5 Proactive dormancy (gate 2c) & frontier gates

`gardenerMRMergeBlockActive` and `gardenerFrontierFullyWedged`
(`gardener_evaluator.go:323`) run for any non-nil Gardener. Both are **fail-safe
toward NOT parking** and re-derive from the LIVE frontier each tick, not from any
blob field:

- MR-merge block only engages for a gardener that **declared**
  `stop.loop_end.kind == "mr_merged"` (`gardenerMRMergeBlockActive`) — absent here,
  so it is skipped.
- Operator-wedge park requires ALL of: a resolvable open frontier epic, an open
  operator escalation/decision on it, AND a positively-proven zero actionable-child
  count (`gardener_block_on_mr.go:188`). A legacy gardener resolves its frontier via
  the `gardener-epic:<id>` label (`gardenerFrontierEpic`, `:750`) — many targets
  carry it. So this gate **can** legitimately park a backfilled gardener that is, in
  fact, fully wedged behind an open operator blocker with no actionable work.

Is that a behavior change? Technically yes — the spec-less trigger would have
spawned an agent that immediately discovered "nothing to do, waiting on a human"
and closed. The blob lets the engine skip that wasteful spawn and park instead,
auto-resuming the instant the frontier unwedges. This is **strictly an
improvement and is the designed intent** of proactive dormancy ([ka-0000007387]):
it fires only on a positively-proven wedge, fail-safes to firing on any
uncertainty, and never closes the gardener. It does not change *whether* the gardener
makes progress — only whether it burns a pod to re-derive a block it cannot clear.
The design accepts this as the one sanctioned, safe, and beneficial divergence; the
verify step ([ka-0000001186]) should confirm on a sample that non-wedged gardeners
still fire on cadence.

---

## 6. Confirmation checklist (satisfies the acceptance criteria)

Verified by reading `gardener_evaluator.go` + `gardener_hooks.go` + `triggers.go` at
the cited lines:

- [x] A spec with only the §2 fields still **bumps counters**: `gardenerPostFire`
  increments `RunsSpawned` from the seed (`:1271`); `gardenerAgentTracker` bumps
  `RunsCompleted` on the NEXT genuinely-new closed agent (`:1500`), while the
  seeded `last_checked_agent` blocks re-crediting the head (§5.2).
- [x] It does **not trip exit/budget logic**: run_budget gate needs `RunBudget>0`
  (§4); stop gate needs `len(Stop)>0` (§4); exit needs non-empty `ExitCondition`
  (§4). All zero/empty ⇒ all dead.
- [x] Firing cadence is preserved: cron/source path is unchanged; the only newly
  active gate that could throttle is the concurrency cap, neutralized by §5.1.
- [x] Counter double-count avoided by the `last_checked_agent` seed (§5.2).
- [x] Historical tally seeded from the SAME `deriveGardenerRunCount` the viewer
  displays, so the native tally equals the current derived value (no jump).
- [x] Standing-ness, dark-ness, chaining, and subtree-mode are all read from
  labels / separate fields / omitted-and-zero, so none changes (§3, §5.3, §5.4).

## 7. Explicit non-goals (operator opt-in only)

The backfill MUST NOT introduce, without a per-gardener operator opt-in:

- `run_budget`, `max_runs`, `stop`, `exit_condition`, `on_exit` — any loop budget
  or exit condition. A spec-less spawn_agent trigger has NO loop budget today;
  adding one changes firing behavior (it would eventually close a gardener that
  currently fires forever). This is the KEY RISK the epic calls out.
- `assign_bead` — would flip the gardener to subtree mode (§5.4).
- `standing` — read from labels; setting it in the blob is redundant and risks
  drift from the label the guard actually reads.

`max_concurrent_runs` (§5.1) is the one non-identity field the backfill sets, and
ONLY to *preserve* the pre-blob effective concurrency (mirror `max_concurrent` when
`>1`) — it is behavior-preserving, not a new limit, so it is not an opt-in.

---

## 8. Cross-references

- Struct: `GardenerSpec`, `gardenio/controller/internal/bridge/triggers.go:500`
- Pre-fire gate stack: `gardenerPreFireCheck`, `gardener_evaluator.go:142`
- Counter bump (spawn): `gardenerPostFire`, `gardener_evaluator.go:1266`
- Counter bump (complete) + de-dup: `gardenerAgentTracker`, `gardener_evaluator.go:1438`
- Concurrency cap: `gardenerMaxConcurrentRuns` / `gardenerAtConcurrentRunsCap`, `gardener_evaluator.go:1116,1141`
- Viewer derivation being replaced: `deriveGardenerRunCount`, `cmd/beads-viewer/gardeners_enrich.go:115`
- Manifest (target set + seed values): `gardenio/docs/gardener-spec-less-manifest-2026-06-15.md`
- Bounds warn-with-grace: `warnUnboundedGardener`, `triggers.go:1906`
