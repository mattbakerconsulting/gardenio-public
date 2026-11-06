# Trigger System Reference

The trigger system lets external events (JIRA issues, GitLab MRs/pipelines,
inbound webhooks, bead lifecycle transitions, account thresholds, cron
schedules, Slack reactions) fire controller actions (spawn an agent, apply a
formula, create a bead, post a notification). Triggers are described as beads
so they are versioned, auditable, and editable through the normal bead tools
(`kd`, `gb trigger`, `beads3d`, etc.).

This document is the reference for authoring and operating trigger beads. Code
lives in `gardenio/controller/internal/bridge/` — start at
`triggers.go` for the engine and `trigger_source_*.go` for per-source logic.

## Contents

- [Bead schema](#bead-schema)
- [Common fields](#common-fields)
- [Engine semantics](#engine-semantics)
  - [Wake-intent reconciliation (intent vs. mechanism)](#wake-intent-reconciliation-intent-vs-mechanism)
  - [Dedup](#dedup)
  - [Cooldown](#cooldown)
  - [max_concurrent](#max_concurrent)
  - [max_concurrent_runs (single-live-instance default for gardeners)](#max_concurrent_runs-single-live-instance-default-for-gardeners)
  - [Catch-up on restart](#catch-up-on-restart)
- [Permission model](#permission-model)
- [Actions](#actions)
  - [spawn_agent](#spawn_agent)
  - [apply_formula](#apply_formula)
  - [create_bead](#create_bead)
  - [notify](#notify)
  - [assert](#assert)
  - [wake_pr_owner](#wake_pr_owner)
- [Sources](#sources)
  - [jira](#jira)
  - [gitlab](#gitlab)
  - [webhook](#webhook)
  - [bead](#bead)
  - [account](#account)
  - [schedule](#schedule)
    - [slack_channel routing (non-project spawn channel)](#slack_channel-routing-non-project-spawn-channel)
  - [slack-reaction](#slack-reaction)
- [Declarative trigger vs LLM gardener](#declarative-trigger-vs-llm-gardener)
- [Scheduled gardeners (`type=trigger`)](#scheduled-gardeners-typetrigger)
- [Worked examples](#worked-examples)
- [Opportunistic gardener wake-condition recipes](#opportunistic-gardener-wake-condition-recipes)
- [Waking an agent when its CI concludes (`gb wait ci`)](#waking-an-agent-when-its-ci-concludes-gb-wait-ci)

## Bead schema

A trigger is a bead with:

| Attribute | Value |
|---|---|
| `type` | `trigger` |
| `kind` | `data` |
| `status` | `open` (disabled triggers are either `status=closed` or carry `enabled: "false"`) |
| Label | `trigger:<source>` — e.g. `trigger:jira`, `trigger:gitlab`, `trigger:webhook`, `trigger:bead`, `trigger:account`, `trigger:schedule`, `trigger:slack-reaction` |
| Fields | The [common fields](#common-fields) plus per-source fields |

Each source's `LoadTriggers` picks up its beads via a filtered query (`type=trigger`,
`status=open`, `labels=[trigger:<source>]`), skips any bead with `enabled: "false"`,
and parses common fields via `ParseTriggerDef`. See
`gardenio/controller/internal/bridge/triggers.go` for `ParseTriggerDef`.

## Common fields

These fields are shared by every trigger source. Field values are strings in
the bead, even for numbers and booleans.

| Field | Type | Default | Meaning |
|---|---|---|---|
| `enabled` | bool-ish | `true` | Treated as enabled unless the string is `"false"`. |
| `action` | string | `spawn_agent` | One of `spawn_agent`, `apply_formula`, `notify`, `create_bead`, `assert`, `wake_pr_owner`. |
| `role` | string | — | Agent role for `spawn_agent` (e.g. `crew`, `gardener`, `triage`). |
| `project` | string | — | Project scope. Used for created task beads and default condition scoping. |
| `prompt` | string | — | Prompt / notification body. Supports `{{var}}` template expansion. |
| `formula_id` | string | — | Formula bead ID for `apply_formula`. |
| `formula_vars` | string | — | Formula variables (template-expanded at fire time). |
| `max_concurrent` | int | `3` | Max fires per evaluation cycle. For `spawn_agent`/`apply_formula` this is a **live lease** — agents still alive from prior fires (or sibling multi-entry sources) count against it. See [max_concurrent](#max_concurrent). |
| `max_concurrent_runs` | int | gardener: `1`, else unset | Hard cap on how many agents this trigger may have **live (spawning/working) at once** — distinct from `max_concurrent` (a per-cycle fire cap). For a **gardener** (`gardener` block present) this defaults to `1`: a gardener loop is a singleton controller, so a late/catch-up cron fire cannot spawn a second gardener while a prior run is still working (ka-0000003097). A positive value raises the cap (bounded fan-out); a non-gardener trigger that leaves it unset keeps the legacy `max_concurrent` behavior. See [max_concurrent_runs](#max_concurrent_runs-single-live-instance-default-for-gardeners). |
| `lease_stale_hours` | float | `3` | Age bound on the live lease: an agent whose `updated_at` is older than this stops counting toward `max_concurrent` / `max_concurrent_runs` (and stops blocking a gardener's `one_spawn_at_a_time` gate), so a hung agent can't wedge the lane forever. Non-destructive — the agent bead is untouched. See [Lease staleness bound](#lease-staleness-bound-lease_stale_hours). |
| `cooldown_minutes` | int | `5` | Per-trigger evaluation cooldown; the engine gates on `max(30s, cooldown_minutes)`. Bypassed read-only when an event is pending (ka-0000005988) or a cron window is due (ka-0000004307) — see [Cooldown](#cooldown). |
| `last_checked` | RFC3339 | — | Managed by the engine; written after every evaluation. **Canonical liveness signal** for `type=trigger` beads — see [Liveness signals](#liveness-signals-canonical-last_checked). |
| `last_outcome` / `last_outcome_at` | string / RFC3339 | — | Managed by the engine; written in the SAME patch as `last_checked` and names WHY the most recent evaluation did what it did (`fired: spawned N`, or a decline reason). **The diagnosability answer for a dark gardener** — see [Decline reason](#decline-reason-last_outcome). |
| `last_fire_error` / `last_fire_error_at` | string / RFC3339 | — | Managed by the engine; stamped when a `fireAction` actually **RAISES** (e.g. `spawn_agent`'s `CreateBead` 400s) — a HARD failure, distinct from a decline. Because a failed fire spawns no agent, no run counter moves, so without this a gardener that MATCHES-EVERY-CYCLE-AND-FAILS is byte-indistinguishable from an idle/gated one in the data plane (bug ka-0000002991; the trap of ka-0000004529, where two agents mis-diagnosed it as a silent decline). Cleared on the next successful fire — a present value is a **live** failure. For gardeners a fire error ALSO increments `consecutive_failures` (below) so `hard_stop.consecutive_failures` engages. Surfaced prominently by `gb gardener status`. |
| `pour_stall` | `"true"` / `""` | — | Managed by the engine's pouring-stall sweep. `"true"` while an `apply_formula` trigger is pouring molecules that go unworked; cleared on recovery. Read-only for operators — see [pouring-but-never-worked sweep](#engine-side-pouring-but-never-worked-stall-sweep). |
| `pour_stall_molecules` / `pour_stall_oldest_age` / `pour_stall_detected_at` | int / duration / RFC3339 | — | Stamped alongside `pour_stall`: the inert-molecule count, how long the oldest has been quiet, and when the stall was first detected this episode. |
| `allowed_projects` | JSON array or CSV | — | Optional allowlist of target projects; `"*"` matches any. |
| `allowed_roles` | JSON array or CSV | — | Optional allowlist of target roles; `"*"` matches any. |
| `blocked_by` | JSON array or CSV | — | Bead IDs (typically `type=decision` or `type=escalation`) that pause this trigger while ANY of them is still open. See [Blocked-by decisions](#blocked-by-decisions). |
| `execution_target` | string | — | Runtime backend stamped onto every agent this trigger spawns (agent-metadata layer of the controller's `global < project < role < agent` precedence). E.g. `lambda_microvm` routes iterations to the AWS Lambda MicroVM backend without flipping the whole role/project. Unrecognized values fall back to the Kubernetes pod path controller-side. |
| `slack_channel` | string | — | Slack channel ID (e.g. `C0DAILY0STANDUP`). For **`action: spawn_agent`**: every agent this trigger spawns routes its card / thread / squawks there instead of the project's default channel (stamped onto each spawned agent bead as `schedule_slack_channel`). Works for a **plain `spawn_agent` schedule** — no gardener block needed; a gardener may set it inside its `gardener` block instead, and when both are set the gardener block wins. For **`action: notify`**: the notification posts to this channel instead of the notifier's default (ka-0000008204). Set it with `--slack-channel`. See [slack_channel routing](#slack_channel-routing-non-project-spawn-channel). |
| `wake_intent` | string | — | **Natural-language statement of WHEN this trigger should wake** — the *intent*, deliberately split from the cron/conditions *mechanism*. E.g. `"Weekdays 9am PT, plus whenever open P0s > 0."` **Never read by the fire path** — only by the periodic wake-intent reconciler, which diffs intent against mechanism + observed fires and fixes drift or files a bug. Set with `gb trigger create … --wake-intent '…'`, audit with `gb trigger reconcile`. See [Wake-intent reconciliation](#wake-intent-reconciliation-intent-vs-mechanism). |

The bead's `created_by` identifies the author. Triggers with a non-empty
creator are subject to allowlist checks; triggers with an empty creator are
treated as system-created and skip those checks (see
[Permission model](#permission-model)).

Per-source fields — JQL, project paths, webhook tokens, emojis, cron
expressions, conditions — are documented under [Sources](#sources).

## Engine semantics

`TriggerEngine` (`triggers.go`) orchestrates all sources. On every evaluation
tick it:

1. Loads trigger beads for each registered source (`source.LoadTriggers`).
2. Applies the engine-level cooldown check (`time.Since(LastChecked) < 30s` → skip).
3. Runs `source.Evaluate` to produce `TriggerMatch` values.
4. Filters matches through the per-trigger `seen` set (dedup).
5. Fires at most `max_concurrent` actions per evaluation.
6. Updates `last_checked` on the trigger bead.

### Liveness signals (canonical: `last_checked`)

> **`last_checked` is the canonical liveness signal for `type=trigger`
> beads — including gardeners. Do NOT use `last_run` to decide whether a
> trigger/gardener is alive.**

This is a spec statement, not a hint. The reasoning (B1, ka-0000002275):

- `last_checked` is stamped **unconditionally on every evaluation** of a
  trigger — on a no-match cycle, a dedup-suppressed cycle, and a fired cycle
  alike (`evalOne` in `triggers.go`). A stale `last_checked` therefore means
  the engine has stopped evaluating that trigger (disabled, source-dropped, or
  the controller is dark) — exactly the dark-gardener condition. Freshness is
  assertable declaratively via the `older_than` / `newer_than` condition
  operators (e.g. `{"field":"last_checked","op":"older_than","value":"2h"}`).
- `last_run` is **engine-managed and best-effort**. On the unified
  `type=trigger` gardener path it is written only as a *convenience mirror* of
  the last successful fire time (`gardenerPostFire`); historically it was not
  written at all, so older gardeners and any non-gardener trigger leave it empty.
  It can also legitimately be empty on a healthy gardener that has not yet
  fired (all conditions blocking). **An empty or stale `last_run` does not
  imply a dark trigger** — only a stale `last_checked` does.

Run accounting (`runs_spawned`, `runs_completed`, `consecutive_failures`,
`last_agent_id`) is authoritative inside the serialized `gardener` JSON blob,
written by `gardenerPostFire` / `gardenerAgentTracker`. As of B1 those counters
are also **mirrored up to the registered top-level fields** in the same write,
so `gb gardener status`, `beads-viewer`, and declarative `bead_query` conditions
can read them directly instead of parsing the blob. The blob remains the source
of truth; the top-level fields are eventually-consistent mirrors refreshed on
each fire / terminal-agent reconcile.

### Decline reason (`last_outcome`)

`last_checked` answers *"is the engine still evaluating this trigger?"* — but a
trigger can be evaluated every cycle and still never fire, and `last_checked`
advances **identically** whether the source returned zero matches, the seen-set
dedup-suppressed every match, or a fire actually happened. That indistinguishability
was a real diagnosability wall (ka-0000001513): a plan-gardener that never fired
passed every health check — `enabled: true`, fresh `last_checked`, cron in-window,
preflight satisfied — with the only externally visible state being `last_checked`
advancing, forcing an investigation to grep controller logs the operator cannot
reach (and, in that case, run a live cron experiment to rule out the seen-set by
hand).

`last_outcome` closes that gap. Every evaluation that stamps `last_checked` now
carries, in the **same field patch** (zero extra daemon round-trips or SSE events
over the pre-existing `last_checked` write), a short string naming what the cycle
did:

| `last_outcome` | Meaning |
|---|---|
| `fired: spawned N` | The healthy case — the engine spawned N agent(s) this cycle. |
| `no source matches (out of window / preflight not met / already fired)` | `source.Evaluate` returned **zero** matches. A schedule gardener is out of its cron window, a preflight condition is unmet, or `last_run` already covers the current window. |
| `all N match(es) deduped by seen-set (window already fired)` | The source **did** produce matches, but the engine's in-memory `seen` set already carries every key — the fire window was consumed. Distinguishes the engine-dedup case from "no source matches" (the exact split the ka-0000001513 investigation had to establish by experiment). |
| `all matches deduped by persisted state (already fired)` | Same, but caught by the pre-fire dedup refresh against persisted catch-up keys (ka-0000000151). |
| `deferred: max_concurrent lease full` | Matches existed but every one was deferred because the live-agent concurrency lease was already full — the dark-by-wedged-agent case. |
| `no spawn: fire failed: <error>` | Matches existed and every `fireAction` **RAISED**; the (truncated) error is folded inline and also persisted to `last_fire_error` — see [Fire error](#fire-error-last_fire_error). |
| `no spawn: all fire attempts failed` | Matches existed but every `fireAction` errored without a captured error (legacy fallback). |

`gb gardener status` surfaces `last_outcome` and flags any non-`fired` value, so
a gardener that is evaluated but never fires no longer reads as all-green.

### Fire error (`last_fire_error`)

`last_outcome` names a **decline** — why a trigger evaluated but chose not to
fire. It does NOT cover the case where the trigger DID try to fire and the
action **raised**: `spawn_agent`'s `CreateBead` 400ing on
`operator could not be resolved`, a formula pour failing, etc. That failure was
historically logged (`ERROR triggers: action failed …`) and then **dropped** —
`last_run` stayed null, `consecutive_failures` stayed `0`, `last_outcome` was
untouched. Because a failed fire spawns no agent, no run counter ever moves, so
a gardener that *matches every cycle and fails* is **byte-indistinguishable** in
the data plane from one that is idle or gated. That indistinguishability was a
real, expensive wall (ka-0000004529): three plan-gardeners failed this way for
hours and two agents burned full sessions mis-diagnosing it as a silent decline
— the only way to see the truth was `kubectl logs` on `gardenio-trigger-engine`.

`last_fire_error` closes that gap. On a `fireAction` error the engine stamps:

| Field | Meaning |
|---|---|
| `last_fire_error` | The (truncated, ≤500 char) error string. Cleared on the next successful fire, so a present value is a **live** failure, not a historical one. |
| `last_fire_error_at` | When the failure happened (RFC3339). |
| `consecutive_failures` | **Gardeners only:** incremented via the authoritative `gardener` blob (and mirrored to the integer top-level field). This is the sharper half of the fix — `gardenerAgentTracker` only advances the streak when it observes a *closed agent*, but a failed fire never spawns one, so before this a gardener could fail **forever** without ever tripping its own `hard_stop.consecutive_failures` self-block. The counter is NOT reset by a successful *fire* (that is not yet a successful *run*); the run-completion `done` path owns the reset. |

`gb gardener status` prints `last_fire_error` prominently (`⛔ last_fire_error: …`)
and exposes it in `--json` — it is the command an operator reaches for first, and
was previously silent on exactly this failure mode.

Two decline paths deliberately write **nothing** (not even `last_outcome`),
because their contract is *resume on the very next cycle with zero side effects*:
the **cooldown** skip (so the out-of-band-advance detector, ka-0000006237, can
attribute any `last_checked` movement to a foreign evaluator) and the **gardener
pre-fire gates** (dormancy / `run_budget` / `one_spawn_at_a_time` / stop-spec,
whose dormant-stays-resumable behavior the `DormantStays*` tests pin). Both are
independently diagnosable — cooldown from a fresh `last_checked` + `cooldown_minutes`,
dormancy from its park-markers and the blob run counters. The **`blocked_by`**
gate is likewise write-free and shows its open blocker on the `blocked_by` field.

#### Engine-side liveness heartbeat sweep (B2)

A stale `last_checked` is a usable dead-man's switch — but only if something
watches it. The trigger engine watches itself: once per `Evaluate` cycle,
**after** the source passes, `SweepGardenerLiveness` (`gardener_liveness.go`)
iterates the open `type=trigger` roster and, for every **enabled gardener**,
asserts `last_checked` is within `N×` the gardener's expected cadence
(`N = 3`). A violation emits a liveness alert — `notify` and/or `create_bead`,
**never a spawn** — so a gardener that goes dark for *any* reason (source-closed,
`enabled=false`, dropped by the scheduler) is surfaced within one cadence with
zero agents in the loop. This is the agent-free replacement for the 2-hourly
LLM fleet-liveness guard (ka-0000006629); the 5/24–5/26 incident — a refactor
that silently disabled the whole janitorial fleet for a week — is the failure
class it catches (B2, ka-0000001541; design ka-0000007560 §B2).

The engine is the correct home: it knows the full roster and runs every cycle,
so it cannot itself go dark the way a single watched gardener can (engine-down is
infra liveness, a separate concern). Cadence is derived from the gardener's cron
(gap between the two most recent fire times), or `2h` by default for an
event-driven gardener; the stale threshold is floored at `15m` so a tight cron is
not flagged on a single slow poll. The sweep skips beads that are **not** dark by
design: disabled gardeners (B3 owns the disable-transition signal), gardeners
`dormant_on_escalation` (parked behind an open operator decision), E2E fixtures,
and never-yet-evaluated gardeners (no `last_checked`). Alerts dedup **per dark
episode** — one alert when a gardener goes dark, re-armed once it recovers.

Wiring (trigger-engine binary): `GARDENER_LIVENESS_ENABLED` (default `true`),
`GARDENER_LIVENESS_NOTIFY` (default `true`), `GARDENER_LIVENESS_CREATE_BEAD`
(default `false`). The declarative CEL form of the same invariant (an
author-owned fleet watch list, B4) layers on top later; this engine pass is the
always-on floor.

##### Steady-state poll allow-list (all statuses)

The roster query above is `Statuses:[open]` by design: a **closed**+enabled
gardener has a `last_checked` frozen at its final cycle, so widening the general
sweep to closed would read ~130 retired lineages as permanently stale and flood.
But that open-only scope leaves a real blind spot the declarative assert
(ka-0000002431) was supposed to cover but can't reliably: a watched gardener that
**self-closes** on a stop condition (`max_runs` / `no_progress_for`) or is
disabled emits exactly one lifecycle event, and the event-driven assert goes
silent forever if it misses that single transition (an SSE gap, a
reconciler-watermark bootstrap, or an engine restart between the close and the
drain). A closed gardener then emits no further event *and* is outside the
open-only roster, so nobody watches it again — the exact regression in bug
ka-0000004451, where 2 of 4 watched janitor gardeners sat dark 15+ days with no
alert.

`SweepGardenerLivenessAllowList` (`gardener_liveness.go`) closes that gap with a
steady-state poll: an operator-configured allow-list of gardener ids
(`GARDENER_LIVENESS_WATCH_IDS`, comma-separated; helm
`triggerEngine.gardenerLiveness.watchIDs`) is `BatchGetBeads`-fetched every cycle
and checked across **all** statuses against the same invariant as the assert
(`status==open && enabled!=false && last_checked` fresh). Because the id list is
small, explicit, and operator-curated — not the fleet-wide closed pool — it is
safe to widen to closed/disabled here where the general sweep must not. A missing
id (deleted / typo'd) is itself flagged so a rotting watch list surfaces rather
than decaying quietly. Alerts dedup per dark episode in a **dedicated** state map
(so the general sweep's per-cycle prune of its open roster cannot wipe the
allow-list's closed-gardener entries). Empty list ⇒ no-op. This is the
poll-every-cycle parity the residual task ka-0000000484 named as the only path
that truly matches the retired guard ka-0000006629.

##### Authoring a declarative liveness assert: `last_checked`, never `last_run`

The declarative B4 form of this invariant (assert `ka-0000002431`) must score
**`last_checked` only**. `last_run` is not a liveness signal, and a CEL clause
over it cannot be made into one:

- **`last_checked` is cadence-independent.** The engine stamps it on every
  `evalOne` exit, so a *fixed* absolute window (the assert uses `3h`) is correct
  for every watched gardener regardless of its cron. That is why the two
  code-plane sweeps score it, and why they are the same invariant as the assert.
- **`last_run` advances only on a FIRE**, so any fixed window shorter than the
  gardener's cron period is violated *by construction* for part of every cycle.
  Bug ka-0000007383: the assert carried a hardcoded `last_run < 10800s` clause
  while watched gardener ka-0000007481 moved to an `0 */8 * * *` cron — it then
  read DARK for 5 of every 8 hours and sent **36 false alerts in 7h** (one per
  `last_checked` stamp, since each stamp is a fresh `updated` event with a fresh
  dedup key).
- **A stale `last_run` is usually a gardener correctly parked**, not a dark one —
  between fires, behind an unmet `bead_query` preflight, or on a cooldown skip
  (all of which report `last_outcome="no source matches …"`).
- **The one shape where a lagging `last_run` IS a defect** — `last_run` many
  cadences behind `last_checked` *while a live holder pins `max_concurrent`* — is
  already owned by `SweepDarkLane` (below), which measures the gap against the
  trigger's own parsed cron cadence. A CEL assert cannot parse a cron, so it can
  never derive that threshold; do not try to approximate it with a constant.

Rule of thumb: if a candidate clause needs to know the watched trigger's cadence,
it belongs in an engine sweep, not in a declarative assert. Use
`gb trigger test <trigger-id> --bead <bead-id>` to dry-run a changed assert
against both a known-HEALTHY and a known-DARK control before leaving it armed.

#### Engine-side "pouring-but-never-worked" stall sweep

The liveness sweep above catches a **dark** gardener — one the engine stopped
evaluating, so its `last_checked` froze. But there is an **orthogonal** failure
the FLAT gardener hit (epic ka-0000001622): the engine keeps evaluating it every
cycle (`last_checked` stays fresh, so the liveness sweep reads it as alive), yet
it delivers nothing. Its `apply_formula` action pours a molecule + step beads on
every cadence, but the driver spawn keeps failing (or the drivers make no
progress), so the poured molecules accumulate **open with unworked step beads**.
To a `last_checked` watcher it looks perfectly healthy while it stalls silently
for days.

`SweepPouringButNeverWorked` (`gardener_pouring_stall.go`) is the backstop for
that class. Once per `Evaluate` cycle, after the liveness sweep, it iterates the
open **`apply_formula`** trigger roster (plain schedule triggers *and* gardener
triggers — the FLAT gardener was a plain `apply_formula` schedule trigger) and,
for each one the engine is actively firing, walks the molecules it has poured.
A poured molecule is **inert** when its whole subtree (the molecule bead plus
its step children) has shown no activity — no claim, no status change, no close —
for longer than `N×` the trigger's cadence (`N = 3`, floored at `1h`). This
freshness-based test catches every stall shape: never-worked, one-step-then-
driver-died, and claimed-then-crashed-and-stale. A trigger is flagged when
**either** ≥ 3 molecules are inert **or** a single molecule has been quiet past a
hard-stale threshold (`6× cadence`, floored `6h` — this arm catches a trigger
that pours one big molecule per cadence and so never accumulates a count). The
flagged trigger gets an alert (`notify` and/or `create_bead`, **never a spawn**)
and a verdict stamped onto its bead (`pour_stall`, `pour_stall_molecules`,
`pour_stall_oldest_age`, `pour_stall_detected_at`) so `gb trigger show`,
`gb gardener status`, and beads-viewer surface the stall from a cheap field read
— no read surface repeats the graph walk. Alerts dedup **per stall episode** and
re-arm once the trigger recovers (its inert count drops below the threshold).

The molecule walk is heavier than the liveness sweep's single query, so per
trigger it is throttled to once per `15m` (a recheck TTL, mirroring the
cooldown-starvation detector); the cached verdict is re-stamped in between. The
sweep skips the same no-fire cases the liveness sweep does (disabled, E2E
fixtures, `dormant_on_escalation`, `blocked_by` an open decision) plus a trigger
at its live `max_concurrent` lease (its molecules are waiting on a live driver by
design), and skips the **entire** pass while a `gardeners-pause` / `triggers-pause`
gate is open (a deliberate hold must not manufacture a stall).

Wiring (trigger-engine binary): `GARDENER_POURING_STALL_ENABLED` (default
`true`), `GARDENER_POURING_STALL_NOTIFY` (default `true`),
`GARDENER_POURING_STALL_CREATE_BEAD` (default `false`). Signal owner:
ka-0000000957.

### Wake-intent reconciliation (intent vs. mechanism)

> **The gap the mechanical sweeps cannot see.** Both liveness signals above
> watch whether a trigger was *evaluated*, not whether it fired *when it was
> meant to*. A gardener whose cron window is wrong, or whose preflight condition
> is subtly always-false, still gets evaluated every cycle → `last_checked`
> stays fresh → it looks perfectly alive → and it never actually wakes. Because
> the cron **is** the intent, nothing can detect when the mechanism fails to
> achieve the intent. (Slack thread ka-0000007909.)

`wake_intent` closes that gap by splitting the two apart:

- **`wake_intent`** (this field) is a natural-language statement of *when the
  trigger should wake* — the intent. It is **never read by the fire path**; the
  engine's firing decision is unchanged.
- The **mechanism** is the existing `cron` / `timezone` / `conditions` /
  `cooldown_minutes` / `blocked_by` / `enabled` config.
- A periodic **wake-intent reconciler** — a cheap (haiku) agent on its **own**
  cron — compares the two and reconciles drift.

**Watcher ≠ watched, no infinite regress.** The reconciler wakes on its own
schedule, so a dark gardener can't hide from it; and the reconciler's *own*
liveness is covered by the engine-side `SweepGardenerLiveness`
(`gardener_liveness.go`), which runs inside the engine and cannot itself go dark.
The engine sweep is the mechanical dead-man's-switch floor; the reconciler is
the semantic "fired as intended" layer on top.

**Deterministic core — the model never guesses timestamps.** Timestamp
arithmetic is done in code by `gb trigger reconcile <id>`
(`scheduler.ReconcileFires`): it reconstructs the trigger's observed fire
history from durable bead state (children's `schedule_fire_time` + `last_run`,
no gardener self-report) and diffs it against the **unconditional** cron
schedule, emitting a JSON report of `expected` / `matched` / `missed` / `extra`
fires. The reconciler reads that report and does only the judgment a model is
good at: does `wake_intent` match the mechanism, and how should any drift be
classified? A reported *missed* fire is a **candidate** miss — the diff
deliberately ignores `conditions` / `cooldown` / `blocked_by` / `enabled`
(those are exactly the levers the reconciler reasons about), so a candidate miss
may be a legitimate gated no-fire.

**Failure taxonomy** (what the reconciler does per class):

| Class | Signal | Action |
|---|---|---|
| **fixable drift** | intent is expressible with current primitives but the mechanism is wrong (bad cron window, wrong tz, an always-false condition) | edit the trigger so mechanism matches intent; log the diff |
| **impossible** | intent can't be expressed with the primitives, or is self-contradictory (incl. a malformed cron — surfaced in the report's `note`) | file a bug bead |
| **habitual** | the same drift recurs across passes, or the trigger chronically over/under-fires | file a bug bead — a systemic signal, not a one-off fix |
| **over-firing** | observed fires ≫ intent (`extra` fires with no cron counterpart) | tighten `cooldown` / window / condition, or file a bug — "lots of unneeded gardener wakeups is bad too" |

Authoring: `gb trigger create <source> … --wake-intent 'Weekdays 9am PT, plus
whenever open P0s > 0'`. Auditing (any agent, read-only): `gb trigger reconcile
<trigger-id> --window 168h --json`. Design: `docs/wake-intent-reconciler.md`.

### Dedup

The engine tracks a per-trigger `seen` set:

```go
seen map[string]map[string]bool // trigger bead ID → set of TriggerMatch.Key
```

When `source.Evaluate` returns matches, the engine filters out any whose
`TriggerMatch.Key` is already in `seen[trigger.ID]`. After a successful
action fire the key is added to the set. The key is chosen by each source
to be stable per logical match — examples:

| Source | Key shape |
|---|---|
| `jira` | `PROJ-123` (issue key) |
| `gitlab` MR | `<project_path>!<iid>` (e.g. `Group/Repo!42`) |
| `gitlab` pipeline | `<project_path>/pipeline/<id>` |
| `webhook` | `<token>:<payload-id>` (payload id or sha256 fallback) |
| `bead` | `<bead_id>:<event>` (e.g. `ka-abc123:created`) |
| `account` spend | `spend:<account>:<field>:<threshold>` |
| `account` utilization | `util:<account>:<field>:<threshold>` |
| `account` all rate limited | `all_rate_limited:<count>` |
| `account` recovered | `recovered:<account>` |
| `schedule` | `<fire_time>` at minute granularity (`2006-01-02T15:04`) |
| `slack-reaction` | `reaction:<emoji>:<channel>:<message_ts>` |

The `seen` map is in-memory; see [Catch-up on restart](#catch-up-on-restart)
for how it is re-populated.

### Blocked-by decisions

The `blocked_by` field lets an operator pause a single trigger while one or
more decision/escalation beads are open — useful when a human is reviewing the
output of a previous fire and the trigger should not produce more work in the
meantime.

Semantics:

- The check runs **after** the engine cooldown check and **before**
  `source.Evaluate` — no upstream queries are issued while blocked.
- The gate is **permissive at the entry level**: if ANY referenced bead is
  status `open` or `in_progress`, the trigger is blocked. Lookup errors and
  missing beads are logged at WARN and treated as "not pending" so a deleted
  decision lets the trigger resume.
- While blocked, the engine **does not update `last_checked`**. The trigger
  therefore resumes on the very next evaluation tick after the referenced bead
  closes — no controller restart, no cooldown delay.
- The skipped evaluation emits a single log line:
  `triggers: blocked by open decision trigger=<id> blocked_by=<list> pending=<open-subset>`.

CLI helpers wrap the field so operators don't have to edit `Fields` JSON
directly:

```sh
gb trigger block   ka-trigABCDE --decision ka-decFGHIJ   # append
gb trigger unblock ka-trigABCDE --decision ka-decFGHIJ   # remove
gb trigger show    ka-trigABCDE                          # render current state
```

`gb trigger show` resolves each referenced bead and prints its current status
alongside the trigger fields.

### Cooldown

The engine gates each trigger's evaluation on
`effectiveCooldown = max(triggerCooldown, cooldown_minutes)`:

- **Engine floor** — `triggerCooldown = 30 * time.Second` in `triggers.go`.
  This protects against tight eval loops and rate-limits upstream queries.
- **Per-trigger `cooldown_minutes`** — a field on each trigger bead, parsed
  by `ParseTriggerDef` with a default of `5` (minutes). Raises the floor for
  triggers whose upstream queries are expensive (JIRA JQL polls etc.).

Two read-only bypasses let a due trigger through mid-cooldown:

- **Pending event** (`eventReadySource.EventReady`, bug ka-0000005988) — a
  multi-source trigger with a matching buffered bead/decision event fires it
  this cycle instead of losing it to the cycle-end drain.
- **Due cron window** (`cronDueSource.CronDue`, bug ka-0000004307) — a cron
  entry that is in-window and past `last_run` fires even mid-cooldown. The
  engine stamps `last_checked` on *every* evaluation, so evaluations recur at
  fixed wall-clock offsets; a cooldown that divides 24h evenly (e.g. 240m)
  phase-locks those offsets, and when none of them lands inside the cron's
  active window the trigger silently never fires again while `last_checked`
  stays fresh — the aliasing that kept gardener ka-0000005204 dark for 6 days.
  Seen-set dedup and `last_run` suppression still prevent re-firing a window
  that was already consumed, so the bypass cannot thundering-herd.

**Cooldown-starvation detector** (`trigger_starve.go`, Alert B of epic
ka-0000005107): the engine tracks how many consecutive cycles each trigger
ended at the cooldown gate. A streak that outlives both ~30 cycles and
max(15m, 3× the trigger's own cooldown) — a duration the engine's own
`last_checked` stamps can never produce — means some other writer keeps
re-arming the gate (e.g. a foreign-environment bridge, the 2026-07-01
outage). When the trigger's source can also confirm its query still matches
upstream items (the JIRA source checks the trigger's JQL via
approximate-count), the engine publishes
`gardenio_trigger_starved{source,trigger}=1` (plus the raw
`gardenio_trigger_cooldown_skip_streak` gauge) and logs a WARN. The
`JiraTriggerStarved` PrometheusRule
(`helm/gardenio/templates/jira-bridge/trigger-starved-alert.yaml`,
`jiraBridge.metrics.prometheusRule.triggerStarved`) pages on the gauge.

### max_concurrent

After filtering to new matches, the engine fires actions in order but stops
after `max_concurrent` successful fires in a single evaluation. Remaining new
matches are deferred — they remain **unseen**, so the next evaluation cycle
will pick them up (subject to cooldown and continued matching). Default is
`3`.

```
matches: 10 new, max_concurrent=3
cycle 1 fires 3 → seen += those 3 keys
cycle 2 fires next 3 → seen += those 3 keys
cycle 3 fires next 3 → seen += those 3 keys
cycle 4 fires last 1
```

A fire is counted only when the action succeeds (no error from
`fireAction`). Denied or failed fires do not consume the concurrency budget
and are not added to `seen`.

#### Live lease for spawn-producing actions

For `spawn_agent` and `apply_formula` actions, `max_concurrent` is a **live
lease**, not a per-batch cap. Before firing, the engine counts the agents this
trigger already has *alive* (status `open`/`in_progress`/`blocked`/`deferred`,
matched via `spawned_by: trigger:<id>`) and seeds the per-cycle fire counter
with that number. So an agent still running from a *prior* fire — or one
spawned by a **sibling source of the same multi-entry trigger** (e.g. `cron`
and `bead_event` firing near-simultaneously, each in its own evaluation) —
counts against the cap.

This makes `max_concurrent=1` mean "only one live instance at a time" rather
than "one per evaluation batch". Without it, a multi-entry trigger could spawn
N concurrent instances (one per source per fire), which defeated single-
threading for `gardener-release-owner-v3` and produced duplicate-tag races
(ka-0000003120). The lookup is fail-open: a daemon error counting live agents
is logged and treated as zero, so a transient hiccup cannot permanently wedge a
trigger. Non-spawn actions (`notify`, `create_bead`, `assert`) keep the
original per-batch semantics.

#### Lease staleness bound (`lease_stale_hours`)

The live lease is **age-bounded** so a single hung agent cannot hold it forever.
An agent counts toward `max_concurrent` only while its `updated_at` is within
`lease_stale_hours` (default **3h**); a holder that has been idle longer stops
counting, and the lane resumes firing on its next tick. The wedged agent bead is
left completely untouched — this is a **non-destructive lease-side release**, not
a kill. The skip is observable: a `WARN` log per stale holder, plus a one-shot
comment on the trigger naming the stale agent, so a recovering lane surfaces
rather than going silent.

The 3h default is chosen from the measured runtime distribution: legitimate long
trigger-spawned runs cluster at 106–124m, while the two confirmed prod outages
(bug ka-0000001095: the fleet merge lane dark 7h49m, bugfix lane dark 9h07m) ran
469m and 547m — 3h separates them cleanly. It mirrors the
`gardener-autoblock-audit.py --lease-stale-hours` detector default, so the
engine's release threshold and the twice-daily hygiene sweep's alert threshold
agree. Set `lease_stale_hours` on the trigger bead to tune per-lane (a positive
float of hours; `0`/absent/invalid falls back to the default). A zero/absent
agent `updated_at` is treated as **not** stale (fail toward keeping the lease
held, so a fresh/not-yet-stamped bead is never released).

The same bound applies symmetrically to a gardener's `one_spawn_at_a_time` gate
(`gardenerAgentStillRunning`): a stale `last_agent_id` no longer blocks the
gardener from firing. Both paths are release-only — the lease bound resumes the
lane but never touches the wedged pod, so a genuinely-hung agent keeps burning
spend until something closes it.

That destructive spend-stopper is the **reap-side runtime ceiling** (bug
ka-0000000716, extended to every agent role by gd-X2eSin2J80), enforced by the
stale-agent reaper rather than the trigger engine. Any agent whose **runtime**
(measured from `created_at`, so a churning wedge that keeps `updated_at` fresh
cannot dodge it) exceeds its role's ceiling is closed and its pod deleted —
but only when a live pod is present and the agent is **not** holding an open
merge-request bead (killing mid-MR is the irreversible case, so that run is
left for `gardener-delivery`/the operator to terminate). The ceiling is set
per role (starting points, to be tuned against Phase B age-metric data —
gd-5MTDWjwNjm — not final thresholds):

| Role | Env var | Default |
|---|---|---|
| `crew` | `STALEREAPER_RUNTIME_MAX_CREW` | 6h |
| `thread` | `STALEREAPER_RUNTIME_MAX_THREAD` | 2h |
| `plan-gardener` | `STALEREAPER_RUNTIME_MAX_PLAN_GARDENER` | 12h |

Originally scoped to trigger-spawned agents only (`spawned_by=trigger:*`,
`TRIGGER_AGENT_MAX_RUNTIME`, 6h default — strictly above the 3h lease-release
threshold and below the 469m/547m outages); now applied by role regardless of
`spawned_by`. A role absent from the map, or set to `0`, has no ceiling.
Because the kill is destructive it ships behind an operator-armed dry-run
gate: `STALEREAPER_RUNTIME_REAP` (renamed from
`STALEREAPER_TRIGGER_RUNTIME_REAP`) defaults **off** (log-only "would reap"
lines); an operator flips it (env or config bead, no restart) once the log
confirms the ceiling matches only genuine wedges. Immediately before an armed
reap fires, the reaper also files an advisory escalation bead naming the
agent, its age, session count, and held task — so a human gets a look at a
long-running agent before the fleet kills it — and never blocks or reverses
the reap.

### max_concurrent_runs (single-live-instance default for gardeners)

`max_concurrent` is a *per-evaluation* fire cap; it does not by itself make a
gardener a singleton. A gardener that leaves `one_spawn_at_a_time` unset and keeps
the `gb gardener start` default `max_concurrent=3` can therefore have **up to
three copies of the same controller running at once** — which is exactly how two
Test-Health Director gardeners ran concurrently off one schedule trigger when a
late/catch-up cron fire spawned a second run 18 min after a catch-up run for the
previous weekday was still `agent_state=working` (bug ka-0000003097).

`max_concurrent_runs` is the hard single-live-instance gate that closes this. In
`gardenerPreFireCheck` (`gardener_evaluator.go`), before a gardener fires, the engine
counts the agents this trigger already has **live** — non-terminal bead status
(`open` covers `agent_state=spawning`, `in_progress` covers `working`), matched
via `spawned_by: trigger:<id>` and pruned by the same `lease_stale_hours` bound
above — and **defers the fire** (stays open + enabled, retries next tick) while
that count is at the cap. The cap is:

- **gardener, unset** → **1** (`gardenerMaxConcurrentRuns`). A gardener loop is a
  singleton controller; one live run per trigger is the safe default, and it
  applies to *every* gardener regardless of the `max_concurrent` value stored on
  its bead — so gardeners authored before this fix (carrying the old default 3)
  are protected with no data migration.
- **gardener, positive** → honored verbatim. An explicit escape hatch for a
  gardener that genuinely wants bounded fan-out (e.g. an `assign_bead` subtree
  gardener working N children concurrently).
- **non-gardener** (no `gardener` block) → **unaffected**: the gate is a no-op, and
  those triggers keep the legacy `max_concurrent` live-lease semantics (a generic
  `spawn_agent` trigger firing on N distinct matches still fans out to
  `max_concurrent`).

This is stricter than `one_spawn_at_a_time` in two ways that mattered for the
incident: it is **on by default** (no opt-in field), and it counts **all** live
agents from the trigger rather than a single `last_agent_id` pointer, so a prior
run the gardener's blob no longer points at still blocks a new spawn. Like every
other gardener gate it is **fail-open** on a daemon read error (treated as "under
cap") so a transient hiccup cannot wedge the lane.

**On catch-up fires for gardeners.** A missed weekday tick should simply be
**skipped, not replayed**. A gardener re-derives current state on every run, so a
catch-up run for yesterday's slot does no useful work the next scheduled run
would not — it is pure duplication. `max_concurrent_runs` addresses the *harm*
(two controllers mutating the same epic concurrently) by collapsing an overlap
into **sequential** runs: the catch-up fire is deferred until the prior run
terminates. It does **not** by itself suppress the redundant catch-up run — the
hour-granularity `CronExpr.InWindow` + `expr.Prev` semantics (which fire once
anywhere in the active hour and, at the top of the window, resolve `Prev` to the
previous day's un-run slot) still produce it. Fully suppressing catch-up would
change fire semantics for **all** schedule triggers, not just gardeners, so it is
deliberately left as a separate operator decision rather than bundled into this
single-live-instance fix.

### Catch-up on restart

The `seen` map is in-memory, so after a controller restart it would be empty
and every still-open match would re-fire. To prevent that, each source
implements `CatchUpKeys(ctx, daemon, triggerID)` which returns the dedup keys
for beads created by that trigger previously. The engine calls this once per
trigger during `TriggerEngine.CatchUp` on startup and before the first
evaluation.

The convention is: every created task bead carries a `trigger:<trigger-id>`
label, plus a source-specific key field (e.g. `jira_key`, `gitlab_key`,
`webhook_event_key`, `bead_event_key`, `account_event_key`,
`schedule_fire_key`, `reaction_key`). `CatchUpKeys` lists beads with
`labels=[trigger:<triggerID>]`, `kinds=[issue]` and extracts the key field.

## Permission model

`checkTriggerAuth` (`trigger_permissions.go`) gates every fire. Rules:

1. If `TriggerDef.CreatedBy == ""` the trigger is treated as **system-created**
   and is unconditionally allowed. This lets bootstrap triggers shipped with
   the platform run without author attribution.
2. Otherwise, if `allowed_projects` is non-empty and `trig.Project != ""`,
   the project must appear in the list (or the list must contain `"*"`).
3. Otherwise, if `allowed_roles` is non-empty and `trig.Role != ""`, the role
   must appear in the list (or the list must contain `"*"`).
4. Otherwise the trigger is authorized.

Allowlist inputs are parsed by `parseStringList`, which accepts either a JSON
array (`["gardenio","coop"]`) or a comma-separated string (`gardenio,coop`).

Every fire by a user-created trigger writes an audit bead
(`type=event`, labels `[auto:trigger-audit, source:<source>, trigger:<id>]`)
with fields including `outcome` (`allow`, `deny`, `error`), `reason`,
`match_key`, `project`, `role`, and `created_by`. System-created triggers
(empty `created_by`) skip audit to avoid bead-spam in the common case.

## Actions

Fields below are in addition to the [common fields](#common-fields).

### spawn_agent

Creates a task bead from the match and spawns an agent that claims it.

| Field | Required | Notes |
|---|---|---|
| `role` | yes | Agent role (e.g. `crew`). Must pass `allowed_roles` if set. |
| `project` | recommended | Project for the task bead and the agent. |
| `prompt` | recommended | Prompt for the spawned agent; `{{vars}}` are expanded from the match. If empty, defaults to `Work on <Key>: <Title>`. |

The agent is named `trigger-<match-key>-<4-char-suffix>` and carries extra
fields including `trigger_id`, `trigger_title`, `source`, and
`spawned_by: "trigger:<trigger-id>"`, plus `<source>_key: <match-key>`.

### apply_formula

Instantiates the named formula directly — the same outcome as
`kd formula apply`, with the trigger and (when present) source bead recorded
as `triggered-by` lineage on the resulting molecule. The action creates the
molecule bead first, then one bead per step with `parent-child` and `blocks`
edges wired per the formula's `depends_on` graph — and finally **spawns a
driver agent** targeting the molecule, mirroring the Slack `/formula pour`
path. Without the driver the pour was inert: step beads with an open
parent-child dep to their molecule are invisible to `gb ready`, so molecules
accumulated unworked on every fire (ka-0000000070, the FLAT gardener stall).

| Field | Required | Notes |
|---|---|---|
| `formula_id` | yes | Formula bead ID (e.g. `ka-0000000359`). |
| `formula_vars` | optional | JSON object string of variables, template-expanded against the match's vars before parsing. |
| `project` | recommended | Applied to the molecule and inherited by steps unless a step overrides it. Also the driver agent's project. |
| `role` | optional | Driver agent role; defaults to `crew`. |
| `prompt` | optional | Prompt for the driver agent (`{{vars}}` expanded from the match). Empty means the agent works from the molecule alone. |

**Driver agent.** The driver is spawned with the molecule as its task bead
and carries the same extra fields as `spawn_agent` drivers (`trigger_id`,
`trigger_title`, `source`, `spawned_by: "trigger:<id>"`, `<source>_key`),
so the [`max_concurrent` live lease](#live-lease-for-spawn-producing-actions)
counts drivers still working a prior pour — a new cron window defers instead
of stacking molecules. The molecule is single-claim work: near-simultaneous
fires cannot put two drivers on the same molecule. The agent name honors the
formula's `assigned_agent` field when set (same override as the Slack pour
path), otherwise it derives from the trigger title. A **failed driver spawn
does not fail the fire**: the pour stands (re-firing would duplicate the
molecule, which has no per-fire dedup key), an ERROR log names the orphaned
molecule, and the poured-but-unworked liveness check is the backstop.

Like `spawn_agent`, the action re-validates the trigger creator's spawn
scopes at fire time and runs the human-operator gate *before* pouring — in
enforce mode an unattributed trigger refuses the whole fire rather than
pouring a driverless molecule.

After a successful fire the engine stamps `last_run` (and `last_agent_id`)
on non-gardener spawn-producing triggers, so a plain `apply_formula` schedule
trigger no longer reads as frozen while firing on cadence.

**Unresolved-placeholder guard.** After template expansion, any
`formula_vars` value that still contains a `{{...}}` placeholder fails the
fire with an error naming the offending var(s) and the template vars the
source actually publishes. Without this guard, a placeholder the source
never published (a typo, or an unsupported alias) would be substituted
*literally* into the molecule's `applied_vars` and every step title,
stranding a batch of unclaimable template beads on every fire
(ka-0000006473). Fire-time placeholders never trip the guard — see
[Fire-time variables](#fire-time-variables).

#### Fire-time variables

Before any action dispatch (all sources, all actions), the engine
guarantees two fire-time template vars on the match:

- `{{fire_time}}` — RFC3339 UTC fire time. Clock-driven sources
  (`schedule`, `multi`, `schedule-mirror`) publish the actual cron
  fire-time; for event-driven sources that publish none, the engine fills
  in the evaluation time.
- `{{fire_time_iso}}` — alias for `{{fire_time}}`. Historically this
  spelling was widely authored in `formula_vars` but never published by
  any source, so the literal string leaked into molecule vars and step
  titles (ka-0000006473); it is now first-class.

Source-published values always win — the engine only fills gaps.

Emitted side-effects: a molecule bead is created with `formula_id` and
`applied_vars` fields, followed by one step bead per active step (after
condition evaluation), each linked via `parent-child` to the molecule.
A `triggered-by` dep is stamped from the molecule to the trigger bead and
to the source bead surfaced in `match.fields["source_bead_id"]` (when set).
Finally an agent bead is created for the driver (task = the molecule).

Historical note: an earlier version of this action created a placeholder
task bead with `pending_formula` / `pending_formula_vars` fields, intended
to be picked up by a separate runner that was never implemented. D6
(ka-0000006284, epic ka-0000007507) removed both the field and the
indirection in favour of direct instantiation.

#### Trigger lineage on molecules (`--trigger-bead` / `--source-bead`)

`kd formula apply`, `kd formula pour`, and `kd formula wisp` accept
`--trigger-bead <id>` and `--source-bead <id>` (both repeatable) to record
**why** the molecule was created. Each id becomes a `triggered-by`
dependency from the molecule to that bead (per D1 / ka-0000005967), and
the full lists are also persisted on the molecule's `trigger_bead_ids` /
`source_bead_ids` fields so they are queryable without traversing deps.

- `--trigger-bead`: the trigger configuration bead that fired (e.g. the
  `apply_formula` trigger row).
- `--source-bead`: the input bead that matched the trigger (e.g. an
  incoming work bead from the `bead` source).

Manual example:

```sh
kd formula apply ka-formula-X --var x=y --trigger-bead ka-trig-1
kd formula apply ka-formula-X --var x=y --source-bead ka-input-2
```

The Slack `/formula pour` handler and the `apply_formula` trigger action
pass these flags automatically (D3 ka-0000000425 / D4 ka-0000006943) so
every bridge-spawned molecule carries an explicit lineage edge instead of
relying on auto-parent against the bridge actor.

### create_bead

Creates a bead without spawning an agent. Use this for triggers whose
downstream consumer is another system (e.g. a formula runner, a manual
workflow, or a teammate polling `gb ready`).

| Field | Required | Notes |
|---|---|---|
| `project` | recommended | Label applied to the created bead. |
| `bead_type` | optional | Type of the filed bead. One of `task` (default), `bug`, `feature`, `chore`. An unknown type is rejected with a clear error so a trigger cannot mint arbitrary types. |
| `bead_priority` | optional | Integer priority (0–4) stamped on the filed bead. Omitted leaves the server default. |
| `parent` | optional | Bead ID wired as a `parent-child` dependency (filed bead → parent) after creation — file a child of an existing epic. A dependency failure is logged, not fatal. |
| `extra_labels` | optional | Additional labels appended to the filed bead, beyond the `source:`/`trigger:`/`project:` labels always added. JSON array or comma-separated string. |

Author the `create_bead` shaping with `gb trigger create bead --action create_bead
--bead-type bug --bead-priority 1 --parent ka-EPIC --bead-label needs-intake …`.
The same four flags apply to an assert trigger's `create_bead` leg
(`gb trigger create assert … --on-violation create_bead|both`). `--bead-priority`
distinguishes P0 (explicitly passed) from "leave the server default" (omitted), so
a filed bug can carry priority 0. These fields are part of the builtin
`type:trigger` schema (ka-0000002642) — before that fix they 400'd at create time
even though `createTaskBead` already read them.

The filed bead's **title is copied from the source bead** (prefixed with the
match key, e.g. `[ka-SRC:created] <source title>`); there is no per-action title
or body template. Provenance is carried automatically: the filed bead gets the
`source-bead:<sourceID>` label and a `source_bead_id` field pointing back at the
bead that fired the trigger, so a draining gardener can read the original one hop
away.

#### Worked example: Slack-feedback → bug (the standing `feedback→bug` pipeline)

The always-on agent-feedback write path (`POST /api/slack/agent-feedback`,
ka-0000002210) emits a stable `type=rating` bead — a `feedback` field, a
`kind:slack-feedback` label, `source=slack-feedback-only`, `score=0` — decoupled
from the dark human-survey UX. A single declarative `create_bead` trigger turns
each such feedback bead into an **open `type=bug`** that existing scheduled
gardeners drain via `gb ready` (no dedicated gardener, no code deploy — epic
ka-0000006893, design ka-0000007307). This is the live trigger `ka-0000002876`;
recreate it with:

```bash
gb trigger create bead \
  --title 'Slack-feedback → bug (feedback rating → open type=bug for gardener drain)' \
  --event-types created \
  --condition 'type:==:rating' \
  --condition 'feedback:present' \
  --condition 'label:kind:slack-feedback:present' \
  --action create_bead \
  --bead-type bug --bead-priority 2 \
  --parent ka-0000006893 \
  --bead-label 'kind:slack-feedback' \
  --agent-project gardenio --cooldown 30s
```

Why each clause:

- `--event-types created` — the daemon upserts feedback on `(rater,
  rated_bead_id)`, so a unique feedback surfaces exactly one `created` event;
  repeated feedback updates the same bead rather than minting a new one. Matching
  only `created` files one bug per distinct feedback.
- `label:kind:slack-feedback:present` — excludes the legacy modal star-ratings
  (which lack this label), so only always-on feedback beads convert. (The
  operator-aware condition parser folds the two extra colons into the field
  operand: `label:kind:slack-feedback` + op `present`.)
- `feedback:present` — a `type=rating` row with no `feedback` field (a bare star
  score) does not become a bug.
- `--bead-type bug --bead-priority 2` — files an open P2 bug the existing bug
  gardeners already drain.
- `--parent ka-0000006893` — wires each filed bug under the feedback→bug pipeline
  epic for narrative context (in addition to the automatic `source-bead:` /
  `source_bead_id` provenance).

#### Worked example: Slack-feedback → gardener (immediate dedicated triage)

The `feedback→bug` trigger above gives feedback *durable capture* — the filed
bug drains through the normal gardener queue on the next cron tick. When feedback
should also get **immediate, dedicated triage** rather than waiting for that
tick, a **second** trigger fires on the *same* feedback event with
`action=spawn_agent role=gardener`. This is the explicit feedback-gardener (task
ka-0000002332, design ka-0000007307 option B); the two triggers are complementary
halves of one pipeline, not alternatives.

The two triggers share identical `type=rating` + `feedback:present` +
`label:kind:slack-feedback:present` conditions and both fire on `created`, so a
single incoming feedback bead:

1. is turned into an open P2 `type=bug` by `ka-0000002876` (`create_bead`), and
2. spawns a dedicated gardener via `ka-0000004923` (`spawn_agent`) that
   **adopts that just-filed bug** — refines it into a well-formed bug and
   dispatches crew or merges — instead of double-filing.

Because `spawn_agent` cannot be authored with a gardener self-block through the
`gb trigger create bead` flags, this trigger is a hand-authored `type=trigger`
bead carrying a nested `gardener` blob. Its shape (the live trigger is
`ka-0000004923`, created **disabled** pending operator enable):

- **Conditions / event**: identical to the `feedback→bug` trigger —
  `type:==:rating`, `feedback:present`, `label:kind:slack-feedback:present`,
  `event_types=created`.
- **Action**: `spawn_agent`, `role=gardener`, `project=gardenio`.
- **Prompt**: `A Slack-agent text feedback arrived: {{feedback}} on
  {{source_bead_id}}. Turn it into a well-formed bug (parent/label it
  kind:slack-feedback under epic ka-0000006893), then dispatch crew or merge as
  appropriate. Provenance: {{source_bead_id}}.` — plus an explicit
  *adopt-don't-double-file* note pointing at the co-firing `ka-0000002876` bug.
- **Herd guards**: `max_concurrent=1`, `cooldown_minutes=2`. The engine seen-set
  already fires exactly-once per unique feedback (the daemon upserts feedback on
  `(rater, rated_bead_id)`), so one unique feedback ⇒ one `created` event ⇒ one
  gardener spawn.
- **Self-block (advice ka-0000000974)**: a nested `gardener` blob with
  `standing:true` (+ a `perpetual:true` label), `run_budget:400`, a `hard_stop`
  of `consecutive_failures:5` **and** `wall_clock:720h` (the backstop axis), plus
  a `loop_end` `no_progress_for:{runs:3}` backed by a `progress` spec (the
  progress-aware self-block axis). Verify both axes with
  `gb gardener status ka-0000004923 --json | jq '.has_backstop,.self_block'` →
  `true`, `"ok"`.

  > **Why NO `loop_end` `bead_query`.** A standing event-responder must **not**
  > carry a `loop_end` `bead_query` pointed at the pipeline epic. `bead_query` is
  > a *terminal* stop kind (`stopKindIsTerminal`), so it would **close** the
  > gardener the instant the epic's open-work count hits zero — i.e. the moment
  > the last open child closes — taking the feedback-triage lane dark during
  > every quiet period (the reap that darkened the release/delivery lanes, bug
  > ka-0000006241). The correct self-block for a standing responder is
  > `no_progress_for`, which advice ka-0000000974 accepts as a valid
  > progress-aware stop. Mirror the fleet's standing gardeners `ka-0000001510`
  > (delivery) and `ka-0000003719` (release-owner).

### notify

Calls the configured `TriggerNotifier` callback. In trigger-engine, the
notifier publishes a closed `type=message` bead labeled `trigger-notice`;
slack-bridge subscribes to those beads over SSE and posts them to Slack on
its own bot credentials. trigger-engine itself holds no Slack token.

| Field | Required | Notes |
|---|---|---|
| `prompt` | recommended | Message body; `{{vars}}` are expanded. If empty, defaults to `New match for trigger "<Title>": <Key> — <Title>`. |
| `slack_channel` | no | Slack channel **ID** (e.g. `C073CG65QUD`) this notification posts to, instead of slack-bridge's default channel. The engine threads it through `TriggerNotifier`; a `trigger-notice` notifier writes it onto the notice bead's `channel` field so slack-bridge's Notice watcher posts there. Empty falls through to the default channel. Set it with `--slack-channel` on `gb trigger create`. (ka-0000008204) |

If no notifier is configured on the engine, `notify` is a no-op that logs
the message.

Per-trigger channel routing lets a pipeline-failure trigger post to a specific
ops channel (e.g. `#acme-devops`) instead of the single project default —

```bash
gb trigger create gitlab-pipeline \
  --project example-org/monorepo --status failed \
  --action notify --prompt 'Pipeline {{gitlab_pipeline_url}} failed on {{ref}}' \
  --slack-channel C073CG65QUD \
  --title 'monorepo pipeline-failure → #acme-devops'
```

#### Posting a top-level message to an arbitrary channel

The `trigger-notice` bead is also the agent-facing path for posting a
**channel-root** (top-level, not threaded) message to any channel the bot is
in — `gb squawk` only reaches your own thread, and `gb slack reply` requires a
`thread_ts`. Create a **closed** `type=message` bead labeled `trigger-notice`
with:

| Field | Notes |
|---|---|
| `text` | Message body (falls back to the bead title if empty). |
| `channel` | Target Slack channel ID (e.g. `C02P59QCRD5`). Omit to use slack-bridge's default channel. |

slack-bridge's Notice watcher (`bridge/notice.go`) reads `Fields["channel"]`
and posts on the bot's `chat:write`. The override field MUST be `channel` —
it is the only channel-ish name the `type:message` schema declares (kbeads
`config.go`); `slack_channel` is rejected at create time (ka-0000002479).

### assert

Fires an alert **because a predicate was violated** — the inverse of
`notify` / `create_bead`, which fire on a positive match. Only the
[`bead` source](#bead) produces matches for an assert trigger, and only when
its `assert` invariant is violated (see the bead source's `assert` field). No
agent is spawned, so the action bypasses the SpawnGuard.

| Field | Required | Notes |
|---|---|---|
| `assert` | yes | JSON array of `{"expr":"<CEL>"}` invariant clauses, expected to hold TRUE. The trigger fires when any clause is FALSE or fails closed (compile/eval error). See the [`bead` source](#bead). |
| `on_violation` | recommended | Which non-spawn alert(s) to fire: `notify`, `create_bead`, or `both`. Defaults to `notify`. |
| `prompt` | recommended | Alert body. In addition to the standard bead vars, `{{violated}}` (the failed clause) and `{{values}}` (the observed field values) are available. |
| `conditions` | optional | Event pre-filter (which beads to evaluate the invariant against) — these suppress on a positive match the usual way. |

Author with `gb trigger create assert --expr '<invariant>' --on-violation
notify|create_bead|both` (feature F5, ka-0000008157). Without `--on-violation`,
`gb trigger create assert` produces a legacy fire-on-match trigger instead.

### wake_pr_owner

Tells the agent that owns a conflicting pull request what conflicts and how to
fix it (Task 3.1, gd-xTJWewKG71). Only the [`bead` source](#bead) feeds it: the
match must be a work bead carrying the conflict fields the github-bridge
scanner writes. The action re-reads the bead, does nothing if it is no longer
`conflicting`, and otherwise tells the first of:

1. a live crew agent whose `task_id` is the bead: a mail nudge;
2. the gardener of the plan the bead sits under (the first ancestor labelled
   `plan`): a mail nudge when it is live, a wake in place (the `/plans` wake:
   `status=in_progress`, `session_resume`, the message as its `prompt`) when
   it is parked. The wake is gated on the SpawnGuard. A plan that never had a
   gardener is left to its gardener trigger;
3. nobody.

It always comments the message on the bead, naming who was told. Nothing in
`prompt` is used: the message is built from the bead's `mr_*` fields. A
failed nudge or wake falls through to the next target; the fire errors (and
is retried) only when nobody was told and the comment failed too. The live
trigger and its message are in [`docs/pr-conflicts.md`](../../docs/pr-conflicts.md#waking-the-prs-owner).

## Sources

Every source accepts the [common fields](#common-fields). The tables below
document **source-specific** fields, supported `event_type` values, and
template variables available to `prompt` / `formula_vars`.

### jira

Label: `trigger:jira`. Polls JIRA with a JQL query via `JiraClient.SearchIssues`.

| Field | Required | Notes |
|---|---|---|
| `jql` | yes | JIRA Query Language. Example: `project = PE AND status = "To Do" AND labels = needs-agent`. |

Match key: JIRA issue key (e.g. `PE-1234`). Dedup prevents re-firing on the
same issue.

Template variables:

- `{{jira_key}}`, `{{jira_key_lower}}`
- `{{jira_summary}}`, `{{jira_description}}`
- `{{jira_status}}`, `{{jira_type}}`, `{{jira_priority}}`
- `{{jira_reporter}}`, `{{jira_assignee}}` (defaults to `"Unassigned"`)
- `{{jira_labels}}` (comma-separated)
- `{{jira_url}}` — `https://acme.atlassian.net/browse/<key>`

Created task fields: `jira_key`, `jira_url`, `jira_status`, `jira_type`, `jira_priority`.

### gitlab

Label: `trigger:gitlab`. Polls GitLab for either merge requests or pipelines.
`event_type` selects which.

| Field | Required | Notes |
|---|---|---|
| `project_path` | yes | Namespaced GitLab project path (e.g. `example-org/monorepo`). |
| `event_type` | no | `merge_request` (default) or `pipeline`. |

`event_type=merge_request` fields:

| Field | Required | Notes |
|---|---|---|
| `state` | no | MR state filter: `opened` (default), `merged`, `closed`, `all`. |
| `gitlab_labels` | no | Comma-separated MR labels. |
| `author` | no | Filter by author username. |
| `target_branch` | no | Filter by MR target branch. |

`event_type=pipeline` fields:

| Field | Required | Notes |
|---|---|---|
| `pipeline_ref` | no | **Exact** branch or tag ref filter, threaded verbatim into GitLab's `?ref=` query param (server-side exact match). |
| `pipeline_ref_pattern` | no | Optional **Go regexp** applied to each pipeline's ref **client-side, after the API fetch and before matches are returned** — so a non-matching ref never becomes a match, never creates a task bead, and **never spawns an agent**. Complements (does not replace) the exact `pipeline_ref`: GitLab's `?ref=` is a single exact value, but merge-train pipelines run on per-MR refs (`refs/merge-requests/<N>/train`), so no single exact ref can select "all trains plus `main`" — a regexp can. Example: `^(main\|refs/merge-requests/[0-9]+/train)$`. **Fails closed:** an invalid regexp makes the trigger return an error and **zero** matches (logged loudly), exactly like a bad `pipeline_max_age` clamps to the default — a typo must not silently reopen the spawn firehose. The engine logs matched-vs-filtered **counts** on each poll so you can prove the filter is firing. Set via `gb trigger create gitlab-pipeline --ref-pattern '<regexp>'`. |
| `pipeline_status` | no | Pipeline status filter: `success`, `failed`, `running`, `canceled`, `pending`, etc. |
| `pipeline_max_age` | no | Recency window (Go duration, e.g. `2h`) threaded into GitLab's `updated_after`. Only pipelines updated within this window match. **Default `2h` when unset.** This is a restart-safe guard: GitLab returns up to 100 historical pipelines with no implicit recency bound, so a freshly-enabled (or engine-restarted) trigger whose in-memory seen-map is empty would otherwise treat every pre-existing failure as new and fire against all of them. The window caps the blast radius regardless of seen-map state — the engine's `LastChecked.IsZero()`-gated bootstrap seeding cannot help once a stale `last_checked` is present on the bead. A non-positive/unparseable value falls back to the default (there is deliberately no "unbounded" escape hatch; widen with a large duration like `720h` instead). Set via `gb trigger create gitlab-pipeline --max-age 2h`. |
| `min_duration` | no | Optional **"slow pipeline" threshold** (Go duration, e.g. `45m`). A pipeline becomes a match **only if its run duration is at/over** this — so the fleet can wake on a train that merely got **slow**, not only one that **broke**. Applied client-side, **after** the cheap `pipeline_status`/`pipeline_ref`/`pipeline_ref_pattern` filters, so a detail fetch is only spent on candidates that already matched everything else. **Gotcha it handles:** GitLab's **list** pipelines endpoint omits `duration` entirely (only the single-pipeline **detail** endpoint returns it), so the engine fetches pipeline detail per surviving candidate to get an authoritative number. **Fails closed:** an unknown/`null` duration (a still-running or skipped pipeline, or a detail-fetch error) **never** matches, and an invalid/non-positive value makes the trigger return an error and **zero** matches (logged loudly) — a typo must not turn a slow-pipeline trigger into one that fires on everything. The engine logs `duration_filtered` counts each poll. **Compose it** with `--status success --ref-pattern '^refs/merge-requests/[0-9]+/train$'` to wake **only on a slow GREEN merge train** — precisely the operator ask. Set via `gb trigger create gitlab-pipeline --min-duration 45m`. |
| `ignore_failed_statuses` | no | Optional **comma-separated list of external commit-status names** (e.g. `Meticulous Tests`). A **failed** pipeline is **suppressed** — never becomes a match, never creates a task bead, never spawns an agent — if and **only if every required (`allow_failure=false`) failed commit status on its SHA is named in this list**. A monorepo pipeline can read `failed` with **every GitLab job green** when the sole red gate is an external required commit status; when that gate is a visual-diff tool like Meticulous, the failure is expected review noise the tool already comments on, and spawning a failure analyst is pure waste. Applied client-side **after** the cheap filters, and it fetches the SHA's commit statuses (`/repository/commits/:sha/statuses`) only for candidates whose status is `failed`. **Fails SAFE, not closed** (the opposite of the filters above): a commit-status fetch error, a failure that includes any required status **not** in the list (a real CI-job failure), or **no** required commit status failed at all — none of these suppress; the pipeline still matches and an analyst still spawns. Suppression happens **only** when the named statuses fully explain the failure. Matching is case-insensitive and exact (not substring). The engine logs `status_filtered` counts each poll. |

Match key:

- MR: `<project_path>!<iid>` (e.g. `example-org/monorepo!42`).
- Pipeline: `<project_path>/pipeline/<id>`.

Template variables (MR):

- `{{gitlab_key}}`, `{{gitlab_mr_iid}}`, `{{gitlab_mr_title}}`, `{{gitlab_mr_url}}`,
  `{{gitlab_mr_state}}`
- `{{gitlab_project_path}}`, `{{gitlab_source_branch}}`, `{{gitlab_target_branch}}`,
  `{{gitlab_labels}}`, `{{gitlab_description}}`
- `{{gitlab_author}}`, `{{gitlab_author_name}}`
- `{{gitlab_pipeline_status}}`, `{{gitlab_pipeline_url}}` (head pipeline, if any)

Template variables (pipeline):

- `{{gitlab_key}}`, `{{gitlab_pipeline_id}}`, `{{gitlab_pipeline_status}}`
- `{{gitlab_pipeline_ref}}`, `{{gitlab_pipeline_sha}}`, `{{gitlab_pipeline_url}}`
- `{{gitlab_project_path}}`

### webhook

Label: `trigger:webhook`. Event-driven: a generic HTTP endpoint
(`POST /webhooks/{token}`) accepts a JSON payload, verifies HMAC, and buffers
the event. On the next evaluation cycle, all buffered events are matched
against every webhook trigger whose conditions pass.

| Field | Required | Notes |
|---|---|---|
| `webhook_token` | yes | Unique path token for this trigger's URL. Generate with `openssl rand -hex 16`. |
| `webhook_secret` | optional (strongly recommended) | HMAC-SHA256 secret. If set, requests must carry a matching `X-Hub-Signature-256`, `X-Hub-Signature`, or `X-Webhook-Signature` header (format `sha256=<hex>`). If empty, requests are accepted unverified. |
| `conditions` | optional | JSON array of condition objects (see below). Empty means "match every inbound event". |

Condition object:

```json
{"path": "alert.severity", "op": "==", "value": "critical"}
```

- `path` — dot-separated JSONPath into the payload.
- `op` — one of `==` / `eq`, `!=` / `ne`, `in`, `not_in`, `contains`, `present`, `absent`.
- `value` — string comparand. For `in` and `not_in`, a comma-separated list.

All conditions must match (logical AND).

Dedup key: `<token>:<id>`, chosen in this order:

1. GitHub `workflow_run` payloads: `workflow_run:<workflow_run.id>:<run_attempt>:<action>`.
   GitHub `check_suite` payloads: `check_suite:<check_suite.id>:<action>`. These payloads
   have no top-level `id`, so before this rule every event on a token collapsed to
   `<token>:completed` and only the first run ever fired (gd-ExCBfoRCTA). `run_attempt`
   keeps a re-run distinct; `action` keeps a run's `requested` event from consuming the
   key its `completed` event needs. A redelivery of the same event yields the same key.
2. The first non-empty of `payload.id`, `payload.event.id`, `payload.message.id`,
   `payload.incident.id`, `payload.alert.id`, `payload.delivery_id`, or
   `payload.groupKey`.
3. Any other GitHub event: `delivery=<X-GitHub-Delivery>` (the per-delivery GUID, which
   GitHub keeps when it redelivers).
4. `payload.action`.
5. Otherwise `<sha256-prefix>` of the serialized payload.

Template variables: every string / number / bool in the payload is flattened
into `{{path.to.field}}`. Captured headers are also exposed as
`{{header_content-type}}`, `{{header_user-agent}}`, `{{header_x-request-id}}`,
`{{header_x-github-event}}`, `{{header_x-github-delivery}}`, and the token is available as `{{webhook_token}}`.

Extracted title (used for the created task bead) comes from the first present
of: `incident.title`, `incident.summary`, `alert.summary`, `event.title`,
`message.title`, `message.text`, `action`, `event_type`. Falls back to
`"Webhook event"`.

> **Waking an agent on its own CI run is not a webhook trigger.** Do not write a
> `trigger:webhook` bead that matches `workflow_run`: use `gb wait ci` (see
> [Waking an agent when its CI concludes](#waking-an-agent-when-its-ci-concludes-gb-wait-ci)).

### bead

Label: `trigger:bead`. Event-driven: subscribes to SSE topics published by
kbeads via `BeadStateTriggerSource.RegisterSSE`. Coverage is split into two
buckets:

**Full-payload topics** (carry the full bead state — labels/fields available
for condition matching):

- `beads.bead.created`
- `beads.bead.updated`
- `beads.bead.closed`

**Edge-event topics** (graph mutations whose payload does not carry full bead
state — condition matching is degraded to id-only and the `present`/`absent`
operators on the `id` field):

- `beads.dependency.added`, `beads.dependency.removed` (fires for **both**
  endpoints of the edge, head and tail)
- `beads.label.added`, `beads.label.removed`
- `beads.comment.added`, `beads.comment.deleted`
- `beads.follower.added`, `beads.follower.removed`
- `beads.bead.deleted`
- `beads.bead.transferred`

| Field | Required | Notes |
|---|---|---|
| `event_types` | optional | Comma-separated list of event types to match. Allowed forms: leaf (`created`, `updated`, `closed`), short topic (`dependency.added`, `label.added`, `comment.added`, …), or full topic (`beads.dependency.added`). Empty means match all. **Prefer the short form for edge events** — the leaf alone is ambiguous (`added` matches every `*.added` topic). |
| `conditions` | optional | JSON array of conditions on the bead. |

Condition object:

```json
{"field": "status", "op": "==", "value": "in_progress"}
```

- `field` — a top-level bead field (`type`, `status`, `title`, `assignee`,
  `created_by`, `closed_by`, `id`), or a key in the bead's extensible
  `Fields` map. Special prefix `label:` checks label presence/absence — e.g.
  `{"field": "label:needs-review", "op": "present"}`. For a prefix match on
  labels, end the label key with `:` (e.g. `label:project:` matches any
  `project:<name>` label).

  > **Footgun (ka-0000002382): a `label:` condition needs `label.added` /
  > `label.removed` in `event_types`.** A label change is delivered by the
  > daemon **only** as the `beads.label.added` / `beads.label.removed` edge
  > topics — adding a label does **not** emit a `beads.bead.updated` event and
  > does not bump `updated_at`. So a trigger that gates on `label:X:present`
  > while pinning `event_types` to `updated` is **over-constrained**: the arming
  > label-add is filtered out at the event-type check *before* conditions ever
  > run, and the trigger silently never fires on the label change (this
  > dark-holed the MR-D publish-fix trigger, which advanced only when an
  > unrelated description edit happened to emit a real `updated`). Either add
  > `label.added` (and `label.removed` if an `absent` guard should re-arm on
  > removal) to `event_types`, or leave `event_types` empty (match-all).
  > `LoadTriggers` logs a one-shot WARN — *"label condition can never be woken
  > by its event_types"* — when it detects this shape.
- `op` — same operator set as the webhook source (`==`, `!=`, `in`, `not_in`,
  `contains`, `present`, `absent`), plus two diff-aware operators that match
  against the `BeadUpdated` event's `Changes` map:
  - `field_changed` (unary, no `value`) — true when the named field appears
    in `Changes` (i.e. the field rotated on this update).
  - `changed_to` — true when the named field is in `Changes` AND its new
    value matches `value`. Non-string scalars (numbers, bools) are
    stringified before comparison so authors can write
    `{"field":"priority","op":"changed_to","value":"0"}`.

  These two operators only fire on real field transitions, so a trigger
  like `{"field":"status","op":"changed_to","value":"in_progress"}` will
  not fire on a comment-add update that leaves status unchanged.

  Both top-level columns (`status`, `assignee`, …) **and** keys in the
  extensible `Fields` blob are observable. The daemon overlays a per-key
  diff of the fields blob onto the published `Changes` map, so a custom-field
  rotation surfaces under its bare key — e.g. a gardener/trigger being disabled
  is matchable as `{"field":"enabled","op":"changed_to","value":"false"}`
  (bug B3, ka-0000004185). Before this, a fields PATCH only ever published the
  opaque `fields` key, so no per-bead event marked a gardener going dark. If a
  custom field happens to share a reserved column name (e.g. a type-config
  field literally named `status`), the real column transition wins that key.
- Two **time-relative (freshness)** operators evaluate the age of a timestamp
  field — the scriptable-trigger JSON-DSL stopgap (decision ka-0000001680,
  "JSON and plan cel in parallel"), shippable ahead of the full CEL evaluator
  (F4 ka-0000008047). `field` is a bead field written in RFC3339 (e.g.
  `last_checked`); `value` is a Go duration string (`2h`, `90m`, `45s`):
  - `older_than` — true when `now - field >= value` (the field is **stale**).
    Example, the fleet-liveness shape:
    `{"field":"last_checked","op":"older_than","value":"2h"}`.
  - `newer_than` — true when `now - field < value` (the field is **fresh**) —
    the inverse of `older_than`.

  Both are **fail-closed**: a missing field, an unparseable timestamp, an
  unparseable or negative duration all evaluate to `false`, so a broken
  freshness assertion never silently passes (mirroring the `in_subgraph_of`
  fail-closed stance). The human-readable `2006-01-02 15:04:05` timestamp form
  is tolerated alongside RFC3339. These combine with the AND-only conjunction,
  so the full liveness predicate "open AND enabled AND last_checked stale" is
  expressible today as three conditions without CEL.
- An **involuntary-death classifier** operator keys off `close_reason` to fire
  on an agent that died mid-work (ka-0000002929, epic ka-0000005447
  auto-forensics):
  - `is_involuntary_death` — true when `field` (default `close_reason`)
    classifies as a **counted involuntary mid-work death**. It delegates to the
    exact same predicate that gates the `gardenio_agent_involuntary_deaths_total`
    counter (`gardeniometrics.InvoluntaryDeathFamily`), so a trigger keyed on it
    fires on precisely the population the metric counts — no drift by
    construction, and no new instrumentation. `value` optionally narrows to a
    single death **family** (`stop_gate_cooldown`, `stop_gate_escape_hatch`,
    `stop_gate_rate_limited`, `context_cancelled`, `context_cancelled_unclaimed`,
    `other`); an empty `value` matches any death. Example, the auto-forensics
    shape (fires an agent to pull the S3 recording of any crew that died
    mid-work):

    ```json
    "event_types": "closed",
    "conditions": [
      {"field":"type","op":"==","value":"agent"},
      {"field":"close_reason","op":"is_involuntary_death"}
    ]
    ```

    Match on the **`_involuntary` marker in `close_reason`, NOT the pod exit
    code** — a SIGTERM'd pod always exits `143`, so exit code cannot
    discriminate an involuntary death from a clean idle-out (that is exactly why
    the metric keys off `close_reason`). Fail-closed: an empty or unclassified
    reason (a clean `gb stop`, a graceful zero-work teardown) evaluates to
    `false`, so the trigger stays silent on healthy despawns. On a match the
    classified family is surfaced as the `{{death_family}}` template var so the
    spawned prompt can name the death shape without re-parsing `close_reason`.
- For edge-event topics, only the `id` field is available on the surfaced
  bead stub. Label/field matching for those topics requires bead hydration
  (tracked under ka-0000001427 follow-ups).
- Two **daemon-backed** operators wake the trigger on mutations anywhere in
  the connected subgraph of a root bead (GAP-3, ka-0000007345):
  - `in_subgraph_of` — `value` is the root bead id; matches when the bead
    identified by `field` (default `id`) sits in the connected subgraph
    rooted there. The subgraph is the BFS closure over forward and reverse
    dependencies (any edge type — parent-child, blocks, related, …) and
    forward and reverse typed mentions. Example:
    `{"op":"in_subgraph_of","value":"ka-demo"}` fires whenever any bead
    reachable from `ka-demo` mutates.
  - `not_in_subgraph_of` — inverse.

  The closure is computed BFS-style at evaluation time, bounded by depth
  (default 4 hops) and node count (default 1024 beads), and cached
  per-root for 30s. Bounded staleness — operators trade exact freshness for
  a cheap hot path. If the closure saturates (BFS hits the node cap), the
  operator returns false for any candidate not already in the partial
  closure; narrow the root if you hit this.

  Requires the trigger engine to wire a `SubgraphResolver` — production
  deploys do this automatically (`cmd/trigger-engine/main.go`). When the
  resolver is missing the trigger does **not** fire (fail-closed) and logs
  a warning, so a misconfigured operator does not accidentally turn into a
  fire-on-every-event hook.
- A **sandboxed CEL expression** condition (feature F4, ka-0000008047) lifts
  the AND-only / single-field / count-only limits of the operator DSL. Instead
  of `field`/`op`/`value`, the condition carries a single `expr`:

  ```json
  {"expr": "bead.type == \"bug\" && bead.priority == 0 && watched.exists(w, w.status != \"closed\")"}
  ```

  The expression is evaluated against a typed, **I/O-free** context:

  - `bead` — a map of the event bead's fields: `bead.status`, `bead.type`,
    `bead.priority` (int), `bead.assignee`, any extensible field by name
    (`bead.agent_state`), `bead.labels` (a list — quantify with
    `bead.labels.exists(l, l == "needs-review")`), and `bead["label:<name>"]`
    (a bool presence map; compare to a literal, e.g.
    `bead["label:project:gardenio"] == true`). Missing scalar fields are
    **not** pre-populated, so guard reads with `has(bead.x)`.
  - `event` — `event.type` (`"created"`/`"updated"`/`"closed"`/…) and, for
    updates, `event.changes.<field>` (the new value, stringified).
  - `now` — evaluation time as a CEL `timestamp`, captured once per Evaluate
    cycle. Time-relative checks read naturally, e.g.
    `now - timestamp(bead.last_checked) > duration("2h")` (the CEL equivalent
    of the `older_than` operator).
  - `watched` — the **explicitly-resolved**, bounded set of beads connected to
    the event bead (the same connected-subgraph closure used by
    `in_subgraph_of`: forward/reverse dependencies + typed mentions), each a
    map with the same shape as `bead`. Quantify with `.exists()` / `.all()` /
    `.size()`. The list is bounded like the subgraph resolver — a node cap
    (default 256 hydrated beads) and a 30s TTL cache — so a large graph cannot
    stall the hot path. When no watched resolver is wired, `watched` is an
    empty list (an expression that quantifies over it simply under-matches).

  Limits and guarantees:

  - **Compile-on-write**: the expression is type-checked and compiled when the
    trigger is authored (`gb trigger create assert`) and again at load time;
    a malformed or non-bool expression is **rejected at authoring time**, not
    at runtime. The compiled program is cached and reused across Evaluate
    cycles.
  - **Bounded cost**: every program carries a cel-go cost limit plus a
    per-eval timeout, so a pathological comprehension is cut off rather than
    stalling the Evaluate loop.
  - **Deterministic, no I/O**: the only inputs are the four context variables;
    `watched` is resolved up front and frozen into the activation.
  - **FAIL-CLOSED**: a compile error, eval error, cost overrun, timeout, or a
    non-bool result all evaluate to `false` — a broken assertion never
    silently fires (mirroring the `older_than` / `in_subgraph_of` stance).

  Expression conditions AND together with each other and with the operator
  DSL conditions in the same `conditions` array.

  Author with `gb trigger create assert --expr '<CEL>'` and dry-run against a
  real bead with `gb trigger test <trigger-id> --bead <bead-id>` (evaluates the
  expression without firing the action; `watched` is empty in dry-run).

- An **assert invariant** (feature F5, ka-0000008157) inverts the firing rule:
  instead of firing when a predicate matches, it fires when a predicate is
  **violated**. The trigger carries an `assert` field — a JSON array of
  `{"expr":"<CEL>"}` clauses, each evaluated against the same typed context as
  the expression conditions above — that is expected to hold TRUE. The trigger
  fires when the invariant is violated: any clause evaluates FALSE, **or** a
  clause fails closed (compile/eval error, non-bool, watched-resolution
  failure). This is the opposite of a suppress-gate condition: for an assert,
  fail-closed means *fire* (a broken assertion must not silently pass).

  ```json
  "assert": "[{\"expr\": \"bead.assignee != \\\"\\\"\"}]"
  ```

  When an assert trigger fires, the match exposes two extra template vars
  alongside the standard bead vars: `{{violated}}` (the source string of the
  clause that failed) and `{{values}}` (a `field=value` rendering of the bead
  fields the clause referenced). The action is `assert`, and the `on_violation`
  field selects the non-spawn fan-out (`notify`, `create_bead`, or `both`) —
  see the [assert action](#assert). Any `conditions` on the same trigger act as
  the **event pre-filter** (which beads to evaluate the invariant against), not
  as the firing predicate.

  Author with `gb trigger create assert --expr '<invariant>' --on-violation
  notify|create_bead|both`. Without `--on-violation`, the same subcommand
  produces a legacy fire-on-match trigger (the expression-condition shape
  above).

  Dry-run an assert trigger with `gb trigger test <trigger-id> --bead <bead-id>
  [--event updated|closed]`. The test command detects the `assert` field and
  reports the **violation** verdict — each clause shows `✓ holds` or
  `✗ VIOLATED`, and the overall result is `WOULD fire (invariant VIOLATED)` when
  any clause fails (matching the engine's fire-on-violation semantic), or
  `would NOT fire (invariant holds)` when all clauses hold. For a default
  fire-on-match trigger the same command reports the match verdict instead.
  `watched` is empty in dry-run, so quantifiers over it under-match (ka-0000002642).

All conditions must match (logical AND). `event_types` is a separate filter
applied before `conditions`.

Dedup key: `<bead_id>:<event>` where `<event>` is the leaf for the original
three topics (e.g. `ka-abc123:created`) and the short form for edge topics
(e.g. `ka-abc123:dependency.added`) to keep `*.added` events distinct from
each other. For `updated` events with a non-empty `Changes` map, the
dedup key also embeds a deterministic fingerprint of the changed fields
(e.g. `ka-abc123:updated:status=in_progress`) so distinct transitions on
the same bead each get to fire once instead of collapsing.

Template variables:

- `{{bead_id}}`, `{{bead_type}}`, `{{bead_title}}`, `{{bead_status}}`, `{{bead_assignee}}`
- `{{bead_event}}` — event name (`created`/`updated`/`closed` for full-payload
  topics; `dependency.added`/`label.added`/etc. for edge topics)
- `{{bead_labels}}` — comma-separated labels (full-payload topics only)
- `{{<field>}}` for every key in the bead's `Fields` map (full-payload topics only)
- `{{changed_<field>}}` for every key in the `BeadUpdated` `Changes` map
  (e.g. `{{changed_status}}` carries the new status value when a status
  transition fired the trigger)

### account

Label: `trigger:account`. Polls account beads (`type=account`, `status=open`)
against thresholds.

| Field | Required | Notes |
|---|---|---|
| `condition_type` | yes | One of `spend_threshold`, `utilization_threshold`, `all_rate_limited`, `account_recovered`. |
| `field` | conditional | For `spend_threshold`, the numeric field (default `total_cost_usd`). For `utilization_threshold`, the numeric field (default `utilization_5h`). |
| `threshold` | conditional | Numeric comparand for `spend_threshold` / `utilization_threshold`. |
| `op` | no | Comparison operator: `>`, `>=` (default), `<`, `<=`, `==`. |
| `account_filter` | no | Restrict to a single account by `title`. Empty = all accounts. |

Condition types:

- `spend_threshold` — fires for each account where `<field> <op> <threshold>`.
  Default field `total_cost_usd`.
- `utilization_threshold` — same shape, default field `utilization_5h`
  (use `utilization_7d` for the 7-day rolling bucket).
- `all_rate_limited` — fires once when every account (filtered by
  `account_filter`) has `throttle_status == "rejected"`.
- `account_recovered` — fires once per account when `throttle_status`
  becomes `"allowed"` and the account has been rate-limited at least once
  (`rotation_count` or `exhausted_at` set).

Dedup keys and titles are source-specific (see the
[Dedup](#dedup) table above).

Template variables:

- `{{account_name}}`, `{{account_id}}`
- `{{condition}}`, `{{field}}`, `{{value}}`
- `{{throttle_status}}`, `{{utilization_5h}}`, `{{utilization_7d}}`
- `{{total_cost_usd}}`, `{{provider}}`
- `all_rate_limited` also exposes `{{account_count}}` and `{{account_names}}`.

### schedule

Label: `trigger:schedule`. Cron-based with optional pre-flight conditions.
Uses the internal `scheduler.ParseCron` (5-field cron with standard aliases)
and respects an IANA timezone.

| Field | Required | Notes |
|---|---|---|
| `cron` | yes | 5-field cron expression (e.g. `*/30 9-17 * * 1-5`). |
| `timezone` | no | IANA timezone (e.g. `America/Los_Angeles`). Default UTC. |
| `last_run` | no (engine-managed) | Timestamp of the last successful fire. Used to detect missed fires. |
| `conditions` | no | JSON array of pre-flight checks. Fires only if all pass. |

Pre-flight condition object:

```json
{"type": "ready_queue", "op": ">", "value": "0", "project": "monorepo"}
```

Supported `type` values:

| type | Meaning | Uses |
|---|---|---|
| `ready_queue` | Count of *ready* `kind=issue` beads: open AND no open dependencies (same semantics as `gb ready`). | `op`, `value`, `project` (defaults to trigger's `project`), plus optional `labels` / `exclude_labels` filter arrays. |
| `active_agents` | Count of active `type=agent` beads. | `op`, `value`, `project`, `role`. |
| `gate` | Does a `type=gate` bead with a given title exist? | `op = present` / `absent` (default `absent`), `value = <gate title>`. |
| `bead_query` | Generic count query. | `op`, `value`, plus `types`, `statuses`, `labels`, `exclude_labels` filter arrays (`exclude_labels` drops beads carrying ANY listed label — e.g. count open MRs while skipping `gardener-delivery-skip`-flagged ones), `parent` (transitive subtree), and `exclude_blocked_by_open_types` (subtracts matched beads blocked_by an OPEN bead of a listed type — the set is data-driven, so `["escalation","decision"]` skips children waiting on the operator, and adding work types, e.g. `["escalation","decision","task","bug"]`, additionally skips children waiting on other open work. Lets a gardener loop_end stop, or a pre-flight gate go dormant, instead of firing on beads it cannot action; see Recipe D). |
| `script` | Read the verdict a **probe pod** stamped on a `type=script` bead — the exec-based pre-check the count-based leaves cannot express (main-moved-since-tag, red-RC, etc.). The leaf itself runs nothing (the `conditions` package stays pure — no `os/exec`, no network); a probe pod (`gb probe run`) runs the script out-of-band and closes the bead stamping `exit_code`. Condition is **met** (⇒ fire) when the probe exited **non-zero** ("something changed, wake the agent"), **unmet** (⇒ suppress) on a fresh exit **0** ("nothing to do"). | `script_bead` = the `type=script` bead id; `op` = `nonzero` (the only op; empty defaults to it); `value` = optional staleness window as a Go duration (default `1h`, `0` disables). **FAIL-OPEN**: a missing / open / stale / unparseable verdict returns *met* (spawns) — inverting the count leaves' fail-closed default — so a degraded probe costs one no-op run and never silently stops releases. Probe-pods plan `ka-0000000544`; wired to no trigger by default (opt-in per lane, gated on decision `ka-0000001284`). |

`op` for count-based conditions is one of `>`, `>=`, `<`, `<=`, `==` / `eq`,
`!=` / `ne`. `value` is the integer threshold as a string. The `script` leaf's
`op` is instead `nonzero` (see its row) and its `value` is a staleness window,
not a threshold.

Fire semantics: on each evaluation, the source finds the most recent cron
fire-time before now (`expr.Prev(nowLocal)`). If `last_run` is before that
fire time (or empty) **and** all conditions pass, it produces a single
`TriggerMatch` keyed on the fire-time at minute granularity
(`2006-01-02T15:04`). This gives at-most-once semantics per fire minute.

Template variables: `{{trigger_id}}`, `{{trigger_title}}`, `{{cron}}`,
`{{timezone}}`, `{{fire_time}}` (RFC3339 UTC), `{{fire_time_local}}`.
`{{fire_time_iso}}` is an engine-level alias for `{{fire_time}}` — see
[Fire-time variables](#fire-time-variables).

#### slack_channel routing (non-project spawn channel)

By default a schedule's spawned agent posts to its **project's** default Slack
channel. To route a recurring post to a **different** channel — the canonical
case is a daily-standup poster that must land in `#daily-stand-up` rather than
the project firehose — set the top-level [`slack_channel`](#common-fields)
field to the target channel **ID** (not `#name`):

```bash
gb trigger create schedule \
  --project gardenio --role crew \
  --cron '30 15 * * 1-5' --timezone America/Los_Angeles \
  --prompt 'Post the gardenio daily standup summary.' \
  --slack-channel C0DAILY0STANDUP \
  --title 'gardenio daily standup poster'
```

At fire time the engine stamps `slack_channel` onto every spawned agent bead as
`schedule_slack_channel`; the slack-bridge reads that as the agent's spawn
channel, so the agent's card, thread, and `gb squawk`s all route to the target
channel. This works for a **plain `action: spawn_agent` schedule with no
gardener block** — before, only gardeners (which carry `slack_channel` inside the
`gardener` block) could redirect their channel, so a bare schedule was stuck on
the project default (ka-0000003647).

The same `slack_channel` field (and `--slack-channel` flag) also routes
**`action: notify`** output to a specific channel — see [notify](#notify)
(ka-0000008204).

Precedence: a gardener trigger that sets `slack_channel` both at the top level
and inside its `gardener` block resolves to the **gardener block's** value. Common
rejected guesses (`unknown field`): `slack_thread_channel`, `slack_spawn_channel`
— those are agent-bead fields, not trigger fields (advice ka-0000005119).

### slack-reaction

Label: `trigger:slack-reaction`. Event-driven: the Slack bot pushes
reaction-added events via `BufferEvent`; matches by emoji, optional channel
filter, and optional message-substring match.

| Field | Required | Notes |
|---|---|---|
| `emoji` | yes | Emoji name **without** colons (e.g. `runner`, `mag`). |
| `match` | no | Substring that must appear in the reacted-to message text. |
| `channel_filter` | no | Slack channel ID to restrict matching. |

Dedup key: `reaction:<emoji>:<channel>:<message_ts>`.

Template variables:

- `{{emoji}}`, `{{channel}}`, `{{user}}`
- `{{message_ts}}`, `{{thread_ts}}` (set equal to `message_ts`)
- `{{message_text}}`
- `{{mr_iid}}` and `{{source_branch}}` extracted from the message if present.

> **Legacy compatibility.** Legacy `type=reaction-rule` beads are auto-loaded
> and translated into `slack-reaction` triggers. Their single-brace `{var}`
> placeholders (e.g. `{channel}`, `{mr_iid}`) are converted to `{{var}}`
> automatically. Prefer new-style `trigger:slack-reaction` beads; the
> `reaction-rule` path is a migration bridge only.

## Declarative trigger vs LLM gardener

Before you author a `type=trigger` bead with `action: spawn_agent`, stop and ask
whether it needs an agent at all. An LLM gardener costs a pod, a context window,
and tokens every time it fires. A declarative trigger ([`assert`](#assert) /
[`notify`](#notify) / [`create_bead`](#create_bead), or a
[`bead_query`](#recipe-d-bead_query-loop-end-self-terminate-when-work-is-drained)
wake-gate) costs a sub-millisecond evaluation on the engine hot path and spawns
nothing. Reaching for an agent when a predicate would do produces a **spandrel**:
a gardener "made entirely out of the mistakes of other parts of the system" — the
exact shape of the retired fleet-liveness guard `ka-0000006629`, which burned an
agent every 2h to check three boolean fields and was collapsed into the agent-free
assert `ka-0000002431` (epic ka-0000007837, ~360 spawns/month → 0).

### The one-question test

> **Does any single fire require an agent to *reason, judge, or change the world* —
> or does it just *evaluate a fixed predicate over bead fields and alert*?**

If the per-fire mission is "evaluate predicate P over beads → if P is violated,
notify and/or file a tracking bead," it is **declarative**. Build it with `assert`
(+ `on_violation: notify|create_bead|both`). No `spawn_agent`.

If any fire does irreducible agent work — the work below — it is an **LLM gardener**.
Keep `spawn_agent`.

| Signal a fire does real agent work (→ **keep LLM**) | Signal it is a fixed check (→ **declarative**) |
|---|---|
| Spawns or supervises crew (`gb spawn`, re-spawn stalled agents) | Counts beads matching a filter (open children, ready queue) |
| Gates an MR/artifact against a rubric (CI-green is *not* the rubric) | Asserts a field value or invariant (`status`, `enabled`, a threshold) |
| Reads and *judges* a session/transcript/screenshot | Checks freshness (`last_checked older_than 2h`) |
| Makes an operator-judgment or escalate-vs-self-resolve call | Tests a boolean/`OR`/set-quantified predicate over a `watched` set |
| Synthesizes, reasons, writes code, reconciles infra | Detects a roster-size drop or a disable/close transition |
| Drives an epic toward a goal across multiple steps | The alert body is a *template*, not a task prompt |

Mixed cases resolve by the **mission**, not the trappings: a gardener whose
`conditions`/`loop_end` use `bead_query` is still an LLM gardener if the *per-fire*
work is judgment — the predicate only gates *whether* it wakes, not *what* it does.
Conversely, `action: spawn_agent` does not make a bead a real gardener if the
spawned agent would only run a fixed check (that is the spandrel).

### Sub-case: the zombie one-shot poller

A distinct, common spandrel is the **cron poll around a one-shot condition**: a
trigger that fires a full agent every N minutes purely to ask "is X true yet? if
so, do the one-time thing, then disable myself." Even when the one-time payload is
real agent work, the *wake-gate* is a declarative predicate the engine evaluates
for free — so the steady-state of "spawn an agent to find nothing to do" is the
spandrel, even though the agent itself stays.

**Fix the wake, keep the work.** Move the gate into a
[`bead_query` loop-end](#recipe-d-bead_query-loop-end-self-terminate-when-work-is-drained)
or a [`trigger:bead` event](#recipe-a-wake-on-bead_event-triggerbead) so the
`spawn_agent` payload fires *once* when the predicate first flips true — never the
`*/15` poll. This is a **wake-gate conversion**, not a full collapse: the verdict
on the gardener stays *keep-LLM*; only its scheduling becomes declarative.

### When in doubt, prefer declarative

"We trust our users" — the [`assert`](#assert) predicate language (CEL, feature
F4) is real expression power: time-relative (`older_than`), set-quantified
(`watched.all(...)` / `.filter(...).size()`), boolean-`OR`, and field assertions
that fire *because* a predicate is FALSE. Most "watchdog," "roster check," "queue
empty," and "went dark" missions are fully expressible without an agent — see
[Detect a gardener/trigger going dark (B3)](#6-detect-a-gardenertrigger-going-dark-b3).
Hand-write an LLM gardener only when the one-question test above genuinely lands on
"yes, this fire must reason." Every spandrel you *don't* write is a pod, a context
window, and a recurring token bill the fleet never pays.

> **Sweep result (2026-06-28, task ka-0000004430).** An adversarially-verified
> audit of all 12 open `type=trigger` beads found the fleet already largely
> de-spandrelized (Phase 3b `type=schedule`→`type=trigger` unification + the C1
> collapse): 10 are genuine epic-driving / crew-supervising / rubric-gating /
> session-grading gardeners (keep-LLM); 1 (`ka-0000002431`) is already the C1
> declarative assert; 1 (`ka-0000005296`, a `probe`/`probe` test artifact firing
> 48×/day) is junk to retire; and 1 (`ka-0000004489`, "spawn R6 when R1–R5 closed")
> is a zombie poller whose wake-gate should become an event/loop-end fire while the
> synthesis payload stays an agent. The reusable output is *this heuristic* — so the
> next spandrel is caught at authoring time, not hand-written and swept later.

## Scheduled gardeners (`type=trigger`)

A **scheduled gardener** is a `type=trigger` bead that carries gardener-loop
fields (`triggers:`, `stop:`, `progress:`, `terminal_goal`, `run_budget`,
`run_log`, ...) on top of the standard trigger schema. It runs in
`gardenio/controller/internal/scheduler/` and is the canonical home for
long-running agents that own a goal, spawn one run at a time, and are
bounded by a run budget plus a stop spec. Reach for a gardener-shaped
`type=trigger` when you want prior-run summaries, a terminal goal, fan-in
over cron + bead events + decision resolutions, and explicit
yield-vs-stop accounting. The plain `trigger:schedule` source (cron +
`spawn_agent` with no gardener fields) is for stateless routing rules with
no lifecycle.

> **Phase 3b status (2026-05-27): `type=trigger` has absorbed `type=schedule`.**
> All open `type=schedule` beads were migrated to `type=trigger` in Phase 3b/C8
> (see `plans/trigger-schedule-unification.md`). Gardeners are authored as
> `type=trigger` with gardener fields. The gardener-loop fields (`triggers:`,
> `stop:`, `progress:`, `terminal_goal`, `run_budget`, `run_log`, etc.)
> live on the `type=trigger` bead.

### Wake spec: `triggers:` array (canonical)

The canonical way to express *when a gardener wakes up* is the
**`triggers:` array** field on the schedule bead. Each entry is one source
that can fire a run: `cron`, `bead_event`, `decision_resolved`, `manual`.
Multiple entries fan in — any matching event spawns a run, subject to the
schedule's match / throttle / debounce gates and stop spec.

The triggers array is parsed by `scheduler.ParseTriggers` and consumed by
the `TriggerEngine` in `internal/scheduler/`. See `gardener-loop-runbook.md`
for the three condition families (`triggers:`, `stop:`, `progress:`) and
worked examples.

#### Deprecation: legacy `cron` / `trigger_mode` / `watch_*` fields

The flat `cron`, `trigger_mode`, and `watch_*` fields predate the
`triggers:` array. The surface is **frozen**, and the scheduler does not
lower them into trigger entries (the old `LowerLegacyToTriggers` helper
had no production caller and was removed). New schedules MUST use the
`triggers:` array. The legacy fields will be removed in a future epic.

Soft enforcement (no rejections, warnings only):

- `kd create` / `kd update` print a `lint:` warning on stderr when a
  `type=trigger` bead is created or updated with `cron`, `trigger_mode`,
  or any `watch_*` field set but no `triggers:` array.
- The controller's scheduler emits a one-shot `WARN` log per bead at
  reconcile time for the same condition.

This section documents the schedule bead schema (including the
ephemeral-schedules-gardener fields shipped in 2026.118.5 — see
`docs/proposals/ephemeral-schedules-gardener-primitive.md`) and the
`gb schedule` subcommands an agent uses to self-manage its loop.

### Bead schema

| Attribute | Value |
|---|---|
| `type` | `trigger` |
| `kind` | `data` |
| `status` | `open` (closed gardeners are skipped by the scheduler) |
| Fields | See the table below |

Field values are strings in the bead, even for numbers and durations.

#### Identity & spawn fields

| Field | R/W | Default | Meaning |
|---|---|---|---|
| `name` | operator | — | Custom agent name (e.g. `monorepo-gardener`). Empty = scheduler auto-generates `sched-<title-slug>-<suffix>`. |
| `cron` | operator | — | **Deprecated** — use the `triggers:` array with a `cron` entry instead. 5-field cron expression. Required for `trigger_mode=cron`; ignored for `trigger_mode=manual`. Validated via `scheduler.ParseCron`. |
| `timezone` | operator | UTC | IANA timezone (e.g. `America/Los_Angeles`). Falls back to the project's `default_timezone` if set. |
| `project` | operator | `gardenio` | Project for the spawned agent and the per-run task bead. |
| `role` | operator | `crew` | Role for the spawned agent. |
| `prompt` | operator | — | Prompt text for the spawned agent. Required. |
| `slack_channel` | operator | — | Override Slack channel for run notifications. |
| `assign_bead` | operator | — | Pin the agent to a fixed task bead instead of creating a fresh one per run. |
| `enabled` | operator | `false` | Must be `"true"` for the scheduler to consider the schedule. |

#### Trigger mode (deprecated — use the `triggers:` array)

| Field | R/W | Default | Meaning |
|---|---|---|---|
| `trigger_mode` | operator | `cron` | **Deprecated.** Use the `triggers:` array with the appropriate entry kind instead. What fires the schedule. `cron` (or empty) = legacy cron-driven. `manual` = dormant until `gb schedule fire` runs. The cron field is unused when `manual`; cron-only fields like `timezone` are ignored in that mode. |
| `watch_bead_id` / `watch_channel` / `watch_from` / `watch_labels` / `watch_project` / `watch_role` / `watch_types` | operator | — | **Deprecated.** Use the `triggers:` array with a `bead_event` or `mail_received` entry instead. The scheduler does not lower them into trigger entries.

> **Out of scope for now.** Phase 8 (`decision_resolved`) and Phase 9
> (`bead_closed`, `mail_received`) trigger modes are described in the
> proposal but are **not implemented** as `trigger_mode` values. They
> *are* implemented as entry kinds inside the `triggers:` array — author
> them there.

#### Run-budget & terminal goal (ephemeral-schedules-gardener primitive)

All optional. Absence preserves legacy unbounded cron behavior.

| Field | R/W | Default | Meaning |
|---|---|---|---|
| `max_runs` | operator (initial) / agent via `gb schedule extend` | `0` (unlimited) | Maximum total spawns. When `runs_completed >= max_runs > 0` the scheduler auto-closes the schedule with comment `terminated: max_runs exhausted`. |
| `terminal_goal` | operator | — | Natural-language description of "done". Surfaced to the gardener via `gb prime` ("## Scheduled Loop Context") so each run sees the goal. |
| `exit_condition` | operator | — | JSON-encoded `scheduler.Condition` (same DSL as `trigger:schedule` preconditions: `ready_queue`, `active_agents`, `gate`, `bead_query`). Re-evaluated before every spawn; when it passes, the scheduler skips the spawn and auto-closes with `terminated: exit_condition met`. Fail-open on parse / evaluator errors. |
| `on_exit` | operator | `close` | Reserved. Parsed and persisted; only `close` is implemented today. |
| `run_timeout` | operator | unset | Duration string (`30m`, `2h`). When a run yields and stays in `agent_state=yielding` for longer than this, the scheduler reaps it: the agent bead is closed `agent_state=failed` with `close_reason=run_timeout in yield`, and `runs_completed` is bumped. Unset = legacy behavior (no yield reaping). |

#### Counters (scheduler-managed)

These are written by the scheduler (or `gb schedule fire` for manual runs)
on every state transition. Operators should treat them as read-only;
agents can read them via the schedule bead but should not poke them
directly.

| Field | R/W | Bumped when |
|---|---|---|
| `runs_spawned` | scheduler | A new agent has just been successfully spawned for this schedule. Bumped *after* the spawn succeeds; rejected/failed spawns do not count. |
| `runs_completed` | scheduler | The last spawned agent's bead transitions to `closed` (any `agent_state`), or a yielded run is reaped past `run_timeout`. Used by the `max_runs` auto-close. |
| `consecutive_failures` | scheduler | Last agent closed with `agent_state=failed`, or a spawn attempt errored, or a run was reaped/timed-out. Reset to 0 on a successful close. Auto-disables the schedule (`enabled=false`) when ≥ `max_retries` (default 3). |
| `last_run` | scheduler | Right after a successful spawn. Load-bearing — drives the cron `Prev(now)` vs `last_run` comparison; persisted in its own update call so a counter-schema drift can't wedge the scheduling slot. |
| `last_agent_id` | scheduler | Same call as `last_run`. The most recent spawned agent's bead ID. `gb prime` reads `schedule_prior_agent_id` (set on the *next* run's agent bead) to render the prior-run wrap-up summary. |
| `last_checked_agent` | scheduler | After `trackLastAgentStatus` processes the prior agent's terminal state. Prevents double-counting on the next reconcile. |

On the unified `type=trigger` gardener path these counters are authoritative
inside the serialized `gardener` JSON blob (`gardenerPostFire` /
`gardenerAgentTracker` in `internal/bridge/`); as of B1 (ka-0000002275) they are
also mirrored up to these registered top-level fields on each write, and
`last_run` is stamped with the fire time as a best-effort convenience mirror.
For liveness, prefer `last_checked` (see
[Liveness signals](#liveness-signals-canonical-last_checked)) — `last_run` is
not a reliable dark-gardener signal.

#### Wrap-up signals (agent → scheduler)

The gardener communicates back to the scheduler through its **stop wrap-up**
JSON (the `--wrapup` argument to `gb stop`). The scheduler reads it from
the agent bead's `wrapup` field when the agent closes.

| Wrapup key | R/W | Effect |
|---|---|---|
| `goal_reached` | agent | When `true`, the scheduler appends `terminated: goal_reached — <goal_reason>` and closes the schedule, regardless of `max_runs` or `exit_condition`. Highest-priority loop short-circuit. |
| `goal_reason` | agent | Optional human-readable reason that gets appended to the goal-reached comment. |
| `accomplishments` | agent | Surfaced verbatim in the `Run completed` schedule comment (truncated at 300 chars) and used by `gb prime` to render the **Prior-run summary** block on the next run. |

### Subcommands (`gb schedule`)

These commands are agent-facing — they let a scheduled gardener self-manage
its loop without an operator. All four operate on a schedule bead by ID,
write an audit comment, and update fields atomically.

| Subcommand | Effect | Required flag |
|---|---|---|
| `gb schedule complete <id>` | Adds a completion comment and closes the schedule. The scheduler stops considering it on subsequent ticks. Use when the gardener decides the terminal goal is reached. | `--reason '<why>'` |
| `gb schedule extend <id>` | Increments `max_runs` by N (read-modify-write so the field round-trips as a string). Lets the gardener give itself more iterations. | `--runs +N` (or `--runs N`; positive integer) |
| `gb schedule progress <id>` | Appends a timestamped `[progress]` comment without closing the schedule. Mid-run note for visibility; the schedule keeps firing. | `--note '<text>'` |
| `gb schedule fire <id>` | Force-spawns one immediate run, bypassing the cron fire-time check. Bumps `runs_spawned`, updates `last_run` + `last_agent_id`, and posts a fire-event comment. Works for both `cron` and `manual` schedules; for `manual` it is the only way to trigger a run. Refuses if the schedule is closed/disabled, has hit `max_runs`, has a passing `exit_condition`, or is at concurrency limit. | `--reason '<why>'` |

`goal_reached` is **not** a subcommand — it is a wrap-up flag set on
`gb stop`. Use `gb schedule complete` only when you want to close the
schedule mid-run (no spawn pending). When you are wrapping up the *current
run* and the goal is reached, prefer:

```bash
gb stop --wrapup '{"goal_reached":true,"goal_reason":"shipped MR !1234"}'
```

This both completes the run and short-circuits the loop in a single step.

### Authoring a bounded scheduled gardener

Use the `triggers:` array to describe wake conditions. The example below
fires every 10 minutes and is bounded by `max_runs` plus an
`exit_condition` that auto-closes when the ready queue empties:

```bash
kd create 'Drive ready queue until empty' \
  --type=trigger \
  -f triggers='[{"kind":"cron","cron":"*/10 * * * *"}]' \
  -f project=gardenio \
  -f role=gardener-delivery \
  -f prompt='Drive the gardenio ready queue. Run `gb ready --limit 5`, pick the highest-priority unblocked task, dispatch a crew or claim it yourself.' \
  -f enabled=true \
  -f max_runs=20 \
  -f terminal_goal='ready queue drained — no open gardener-shaped beads in project=gardenio for 1 cycle' \
  -f run_timeout=2h \
  -f exit_condition='{"type":"ready_queue","op":"==","value":"0","project":"gardenio"}'
```

> **Legacy flat fields.** You may still see flat `-f cron='*/10 * * * *'`
> fields without a `triggers:` array. The scheduler does not lower flat
> `cron` / `watch_*` fields into a `triggers:` entry; the surface is frozen
> and `kd` prints a deprecation lint. New gardeners MUST use the
> `triggers:` array.

Runtime behavior:

- Every 10 minutes (cron), and only while the ready queue has work
  (`exit_condition` evaluates `ready_queue == 0` → close), the scheduler
  spawns a gardener-delivery agent.
- Each run sees "## Scheduled Loop Context" via `gb prime`, including
  current run number, `max_runs`, terminal goal, and the prior run's
  wrap-up summary.
- The gardener may `gb yield` between cycles (does NOT consume a run);
  `gb stop` consumes a run.
- After 20 closed runs (or sooner if the gardener wraps up with
  `goal_reached:true`, or sooner still if the queue empties and
  `exit_condition` passes), the schedule auto-closes.

### Manual-fire variant

For schedules that should sit dormant and only run on operator demand,
use a `manual` entry in the `triggers:` array:

```bash
kd create 'Manual: re-embed everything' \
  --type=trigger \
  -f triggers='[{"kind":"manual"}]' \
  -f project=gardenio \
  -f role=crew \
  -f prompt='Run kd embed sync against the live daemon and report counts.' \
  -f enabled=true \
  -f max_runs=1

gb schedule fire <id> --reason 'after schema migration'
```

> **Legacy flat fields.** The flat `-f trigger_mode=manual` field
> (without a `triggers:` array) is the pre-3b spelling of the same
> primitive. It still works but is deprecated — see the lint
> warnings above.

The scheduler will still poll the schedule for `trackLastAgentStatus`
(so terminal state, goal_reached, max_runs, and reaping all work as
usual), but it will never spawn a run from the cron path.



Each example shows the key bead fields and the resulting runtime behavior.
Authoring tip: use `gb trigger create` (see `kd show` for its syntax) to
avoid hand-rolling JSON for common shapes.

`gb trigger create` covers all common authoring shapes; reach for `kd create`
only when you need a field that the CLI does not expose:

| Subcommand | Source label | Notes |
|---|---|---|
| `gb trigger create gitlab-pipeline` | `trigger:gitlab` | Pipeline status filter (`failed`/`success`/...). |
| `gb trigger create gitlab-mr` | `trigger:gitlab` | Merge-request state, labels, author, target branch. |
| `gb trigger create jira` | `trigger:jira` | JQL query plus optional project key. |
| `gb trigger create webhook` | `trigger:webhook` | `--token`, optional `--secret`, repeatable `--condition path:op:value`. |
| `gb trigger create bead` | `trigger:bead` | `--event-types created,updated,closed`, repeatable `--condition field:op[:value]`. Field accepts `label:<name>` (or `label:<prefix>:`) for label predicates. Operator-aware split — `field` may itself contain colons. |
| `gb trigger create schedule` | `trigger:schedule` | `--cron`, `--tz`, repeatable `--bead-query 'labels=A\|B,statuses=open,op=>,value=0'`. The cron expression is validated against `scheduler.ParseCron` at create time. |

`--bead-query` keys: `op` (default `>`), `value` (default `0`),
`labels` / `types` / `statuses` (pipe-separated for multiple values),
`project`, `role`, `type` (defaults to `bead_query`; set to `ready_queue`,
`active_agents`, or `gate` for those condition shapes). When `project=<name>`
is given without an explicit `labels=`, the project is propagated as a
`project:<name>` label so the count query stays scoped.

### 1. GitLab pipeline-failed → apply_formula (Golden Formula triage)

Fire the Golden Formula triage on every failed pipeline on `main` in
`example-org/monorepo`.

```yaml
type: trigger
kind: data
status: open
title: Golden Formula on monorepo pipeline failures
labels:
  - trigger:gitlab
  - project:monorepo
fields:
  enabled:           "true"
  event_type:        pipeline
  project_path:      example-org/monorepo
  pipeline_ref:      main
  pipeline_status:   failed
  pipeline_max_age:  2h
  action:            apply_formula
  formula_id:        ka-GoldenFormulaID
  formula_vars:      '{"pipeline_url":"{{gitlab_pipeline_url}}","sha":"{{gitlab_pipeline_sha}}"}'
  project:           monorepo
  role:              triage
  max_concurrent:    "1"
  allowed_projects:  monorepo
  allowed_roles:     triage
```

Runtime: every tick, `GitLabTriggerSource.evaluatePipelines` lists
`pipelines?ref=main&status=failed&updated_after=<now-pipeline_max_age>`. The
`updated_after` bound (defaulting to `2h`) is what keeps a freshly-enabled or
engine-restarted trigger from firing against the up-to-100 pre-existing failed
pipelines GitLab would otherwise return. Each pipeline produces a match keyed
`example-org/monorepo/pipeline/<id>`. For new matches, the engine
instantiates the golden formula directly (molecule + step beads), substituting
`{{pipeline_url}}` and `{{sha}}` from the match. `triggered-by` deps from the
molecule are stamped onto the trigger bead so the lineage is queryable
without parsing fields.

The example above pins a single exact `pipeline_ref: main`. When a trigger must
cover a *set* of refs that no single exact value can express — e.g. **every
merge-train pipeline plus `main`**, where trains run on the per-MR ref
`refs/merge-requests/<N>/train` — drop `pipeline_ref` and set
`pipeline_ref_pattern` instead:

```yaml
  pipeline_ref_pattern: '^(main|refs/merge-requests/[0-9]+/train)$'
```

The engine compiles the regexp and applies it to each fetched pipeline's ref
*before* building a match, so a non-matching ref (a plain `.../merge` or
`.../head` pipeline, or a push to a feature branch) never becomes a match,
never creates a task bead, and never spawns an agent. It fails closed on an
invalid regexp (error + zero matches, logged loudly) and logs
matched-vs-filtered counts each poll.

#### Wake only on a *slow green* merge train (`min_duration`)

Status and ref tell you a pipeline *broke*; they say nothing about one that
merely got *slow*. Add `min_duration` to fire only on pipelines whose run
duration is at/over a threshold — the operator ask that motivated this field
(thread ka-0000004950). Compose it with `status: success` and the train
`pipeline_ref_pattern` and you get exactly "wake me only on a slow GREEN train":

```yaml
  pipeline_status:      success
  pipeline_ref_pattern: '^refs/merge-requests/[0-9]+/train$'
  min_duration:         45m
```

or via the CLI:

```bash
gb trigger create gitlab-pipeline \
  --project example-org/monorepo \
  --status success \
  --ref-pattern '^refs/merge-requests/[0-9]+/train$' \
  --min-duration 45m \
  --action apply_formula --formula ka-XYZ ...
```

Because GitLab's **list** pipelines endpoint omits `duration` (only the
single-pipeline **detail** endpoint returns it), the engine fetches pipeline
detail for each candidate that already passed the cheap status+ref filters, then
applies the threshold to the authoritative run duration. A `null`/unknown
duration (still-running, skipped, or a detail-fetch error) never matches (fail
closed), so a slow-pipeline trigger fires *only* on a pipeline confirmed at/over
the threshold. The threshold is inclusive (`duration >= min_duration`), and the
poll logs a `duration_filtered` count. A baseline to pick a threshold from: on
2026-08-05 the monorepo's 16 successful merge-train pipelines ran min 24.8 / p50
29.3 / p90 53.7 / max 61.1 minutes — so `45m` fires on roughly the slowest
~10–20% of trains, a rare and genuinely interesting event rather than a firehose.

#### Don't wake on a *Meticulous-only* failure (`ignore_failed_statuses`)

A monorepo pipeline can read `failed` with **every GitLab job green** — the sole
red gate is an external *required* commit status like **Meticulous Tests** (a
visual-diff tool that posts its own MR review comment). Spawning a failure
analyst for that is pure waste: there is no CI job to diagnose, and Meticulous
already surfaced the diff (operator ruling, advice ka-0000007792). Add
`ignore_failed_statuses` so a failure explained **entirely** by the named
external gate(s) is suppressed:

```yaml
  pipeline_status:        failed
  ignore_failed_statuses: Meticulous Tests
```

The engine fetches the SHA's commit statuses (`/repository/commits/:sha/statuses`)
for each **failed** candidate and suppresses it **only if every** required
(`allow_failure=false`) failed status is named in the list. This filter fails
**SAFE**, the opposite of `min_duration`/`pipeline_ref_pattern`: if the fetch
errors, if any required failure is *not* in the list (a real CI-job break), or if
no required commit status failed at all, the pipeline still matches and an analyst
still spawns. It only ever *under*-suppresses. The poll logs a `status_filtered`
count. Multiple gates are comma-separated (`Meticulous Tests, SomeOtherGate`);
matching is case-insensitive and exact.

### 2. Webhook PagerDuty alert → spawn_agent

Page the incident-response crew when a `critical` PagerDuty incident fires.

```yaml
type: trigger
kind: data
status: open
title: PagerDuty critical → crew agent
labels:
  - trigger:webhook
  - project:ops
fields:
  enabled:         "true"
  webhook_token:   "<32 hex chars>"
  webhook_secret:  "shared-hmac-secret"
  conditions: |
    [
      {"path": "event.event_type", "op": "==", "value": "incident.triggered"},
      {"path": "incident.urgency",  "op": "==", "value": "high"}
    ]
  action:          spawn_agent
  role:            crew
  project:         ops
  prompt: |
    PagerDuty incident {{incident.id}} fired.
    Title: {{incident.title}}
    URL:   {{incident.html_url}}
    Investigate, mitigate, and post a status update in the channel.
  max_concurrent:  "1"
  allowed_projects: ops
  allowed_roles:    crew
```

PagerDuty posts to `https://<gardenio>/webhooks/3f2d9b...` with an HMAC
signature. The controller verifies the signature, parses the JSON, and
buffers the event. On the next eval, both conditions match, so the engine
creates a task bead and spawns `trigger-<token>:<payload-id>-<suffix>` as a
`crew` agent in project `ops` with the expanded prompt.

### 3. JIRA new ticket → create_bead

Funnel every new `PE` ticket into the backlog as an unassigned task. Crew
agents on the `monorepo` project pick them up from `gb ready`.

```yaml
type: trigger
kind: data
status: open
title: PE inbox → monorepo backlog
labels:
  - trigger:jira
  - project:monorepo
fields:
  enabled:         "true"
  jql:             'project = PE AND status = "To Do" AND labels = needs-agent'
  action:          create_bead
  project:         monorepo
  prompt:          |
    JIRA {{jira_key}}: {{jira_summary}}

    Status: {{jira_status}}   Reporter: {{jira_reporter}}
    URL:    {{jira_url}}

    {{jira_description}}
  max_concurrent:  "5"
  cooldown_minutes: "10"
```

Every ~30s (engine floor), the JIRA source runs the JQL and produces one
match per issue keyed by `PE-<n>`. For each new match the engine creates a
task bead titled `[PE-<n>] <summary>` with labels
`source:jira`, `trigger:<trigger-id>`, `jira:<key>`, `project:monorepo`, and
fields `jira_key`, `jira_url`, `jira_status`, `jira_type`, `jira_priority`,
`trigger_id`, `trigger_name`. No agent is spawned — the task sits in the
ready queue until a crew agent claims it.

### 4. GitLab pipeline-failed → spawn_agent (CI troubleshooter)

Spawn a crew agent to investigate and fix pipeline failures on `main`.

```yaml
type: trigger
kind: data
status: open
title: Auto-spawn agent on gardenio main pipeline failure
labels:
  - trigger:gitlab
  - project:gardenio
fields:
  enabled:           "true"
  event_type:        pipeline
  project_path:      example-org/gardenio
  pipeline_ref:      main
  pipeline_status:   failed
  action:            spawn_agent
  project:           gardenio
  role:              crew
  max_concurrent:    "1"
  cooldown_minutes:  "10"
  prompt: |
    GitLab pipeline #{{gitlab_pipeline_id}} FAILED on ref {{gitlab_pipeline_ref}}.
    Pipeline URL: {{gitlab_pipeline_url}}
    Commit SHA: {{gitlab_pipeline_sha}}

    Investigate: check failed jobs, identify root cause, fix or retry.
```

Runtime: the gitlab-bridge trigger engine polls `pipelines?ref=main&status=failed`
every 60s. Each new failed pipeline produces a match keyed
`example-org/gardenio/pipeline/<id>`. The engine creates a task bead and
spawns a crew agent named `gardenio-pipeline-<id>-<suffix>` with the expanded
prompt. `max_concurrent=1` ensures only one agent is spawned per eval cycle.

Create with `kd`:
```bash
kd create "Auto-spawn agent on main pipeline failure" \
  --type=trigger --kind=data \
  -l trigger:gitlab -l project:gardenio \
  -f event_type=pipeline \
  -f project_path=example-org/gardenio \
  -f pipeline_ref=main \
  -f pipeline_status=failed \
  -f action=spawn_agent \
  -f project=gardenio \
  -f role=crew \
  -f max_concurrent=1 \
  -f cooldown_minutes=10 \
  -f 'prompt=Pipeline #{{gitlab_pipeline_id}} failed on {{gitlab_pipeline_ref}}. URL: {{gitlab_pipeline_url}}. Investigate and fix.'
```

### 5. Pause a noisy trigger while a human decision is open

When a trigger has produced output that needs human review, gate it on the
decision bead so it stops firing more work until the operator resolves the
question.

```yaml
type: trigger
kind: data
status: open
title: PE auto-crew (paused on open ticketing-policy decision)
labels:
  - trigger:jira
  - project:gardenio
fields:
  enabled:    "true"
  jql:        "project = PE AND status = Open"
  action:     spawn_agent
  role:       crew
  project:    gardenio
  blocked_by: '["ka-decTICKETPOLICY"]'
```

Runtime: while `ka-decTICKETPOLICY` is open, every evaluation tick logs

```
triggers: blocked by open decision trigger=kd-... blocked_by=[ka-decTICKETPOLICY] pending=[ka-decTICKETPOLICY]
```

and skips `Evaluate`. `last_checked` is **not** advanced — closing the
decision causes the trigger to fire on the very next cycle (no restart, no
30s cooldown delay), so the operator can resolve the review and immediately
unblock all pending work without touching the trigger bead.

To attach a decision bead to an existing trigger from the CLI:

```sh
gb trigger block   ka-0000004221 --decision ka-decTICKETPOLICY
gb trigger show    ka-0000004221                  # confirm gate + decision state
gb trigger unblock ka-0000004221 --decision ka-decTICKETPOLICY
```

### 6. Detect a gardener/trigger going dark (B3)

Disabling a gardener (manual toggle on the `/triggers` page, or a refactor that
drops a whole source) used to produce **no signal** — detection was a human
happening to look at the page, or the 2-hourly LLM fleet-liveness guard
(ka-0000006629). Two complementary declarative nets replace that
(bug B3, ka-0000004185):

**Net 1 — per-bead disable event.** The daemon now overlays a per-field diff of
the extensible `Fields` blob onto each `bead.updated` event, so an `enabled`
flip is observable under its bare key. A `trigger:bead` watcher fires the moment
a single gardener is toggled off:

```yaml
type: trigger
kind: data
status: open
title: Alert when a janitor gardener is disabled
labels:
  - trigger:bead
  - project:gardenio
fields:
  enabled:    "true"
  event_type: updated
  conditions: >
    [{"field":"label:role:gardener","op":"present"},
     {"field":"enabled","op":"changed_to","value":"false"}]
  action:     notify
  prompt:     ":warning: gardener {{bead_id}} ({{bead_title}}) was just disabled."
```

This catches a *single* toggle but **not** a cascading disable (a whole source
dropped, or a global `/schedule off`) — that produces no per-bead `enabled`
transition at all.

**Net 2 — roster-count invariant.** A whole-source disable is caught by an
`assert` trigger that holds the roster size as an invariant. Wire the janitor
gardeners as the `watched` set of a roster root bead (the existing `watched`
resolver — bounded BFS over the connected subgraph, hydrated, TTL-cached), then
assert the count of *enabled* members against the expected roster. The instant
a cascading disable flips members to `enabled:false`, the count drops and the
invariant fails:

```sh
gb trigger create assert \
  --event-types updated \
  --expr 'watched.filter(b, b["enabled"] == "true").size() == 4' \
  --on-violation notify \
  --prompt ':rotating_light: janitor gardener roster dropped: {{violated}} ({{values}}) — expected 4 enabled'
```

This asserts on the live `enabled` field rather than label presence, so it also
catches a gardener left `open` but silently flipped to `enabled:false` — the very
5/24–5/26 dark-fleet shape (ka-0000000326). Both nets are agent-free (`assert`
bypasses the SpawnGuard), so the guard costs no pod and no tokens — unlike the
2-hourly LLM spandrel it retires.

## Opportunistic gardener wake-condition recipes

The worked examples above fire a **fresh action** (spawn / formula / bead /
notify) per upstream match — they are routing rules. This section is for the
other shape: a **scheduled gardener** (`type=trigger` with gardener-loop fields,
see [Scheduled gardeners](#scheduled-gardeners-typetrigger)) that you want to
*wake* the moment something changes instead of polling on a bare cron.

Fleet surveys keep finding the same anti-pattern: ~54 gardener-ish trigger
beads, most of them cron-only, idling between ticks while the work they care
about already changed minutes ago. The wake engine is **more capable than
live gardeners use it** — event wakes (`bead_event`, `decision_resolved`),
hybrid event+cron-floor, and edge-change wakes are all supported. The gap is
discoverability. These recipes close it.

The four building blocks, and where each lives:

| Block | Field | Lives on | Reference |
|---|---|---|---|
| `trigger:bead` event wake | `triggers:` entry, `kind: bead_event` | the gardener `type=trigger` bead | [bead source](#bead), runbook §8.2–§8.6b |
| `trigger:schedule` cron floor | `triggers:` entry, `kind: cron` + `preflight` | same bead | [schedule source](#schedule), runbook §5.4 |
| `bead_query` loop-end | `stop.loop_end.any[]`, `kind: bead_query` | same bead | runbook §2 |
| progress-aware self-block | `progress:` + `stop.loop_end` `no_progress_for` | same bead | runbook §3 |

Full payload-shape catalogue and per-recipe gotchas:
`gardenio/docs/gardener-loop-runbook.md` §8 (recipe cookbook) and §2/§3/§5.4.
Advice beads: `ka-0000005707` + `ka-0000001467`.

### The one rule that bites everyone (read first)

A `bead_event` / `decision_resolved` `match` is evaluated **only against the
event payload**, never the live bead. A field leaf naming a key the payload
doesn't carry resolves to "not present", and every non-`absent` operator over
a missing field returns `false` — so the entry **silently never fires**. This
is the #1 reason a hand-written wake condition looks right and does nothing.

Two ingestion paths feed the same engine with **different** payload shapes:

- **SSE full-payload** (`created` / `updated` / `closed`, and hydrated
  edge events): carries `id`, `type`, `status`, `assignee`, `created_by`,
  `priority`, `closed_by`, **every custom field** (`mr_state`,
  `mr_pipeline_status`, `parent`, …), and labels (via `label:<name>` leaves).
  Match here for `type` / `parent` / MR-field / label conditions.
- **NATS close + decision listeners** (`closed`, `decision_resolved`): carry
  only a fixed projection — `watch_bead_id`, `watch_type`, `watch_project`,
  `watch_role`, plus `chosen_label` / `chosen_index` / `resolved_by` for
  decisions. Match here for decision answers and role/project scoping.

Do **not** mix keys from both shapes in one `all:` block — no single event
carries both `type` and `watch_role`, so the `all:` never matches. Daemon-backed
operators (`in_subtree_of`, `in_subgraph_of`, and the `bead_query` /
`ready_queue` / `gate` *preflight* leaves) are the exception: they consult the
daemon, so they work regardless of which payload shape delivered the event.

### Recipe A: wake on `bead_event` (`trigger:bead`)

The core opportunistic primitive — wake when a bead in your scope changes.
Drop a `bead_event` entry into the gardener's `triggers:` array. Wake an epic
gardener whenever a **direct child closes**:

```json
{"kind": "bead_event",
 "events": ["closed", "updated"],
 "match": {"all": [
   {"field": "parent", "op": "==", "value": "{{assign_bead}}"}
 ]},
 "throttle": "60s"}
```

`parent` is a custom field, so this rides the **SSE full-payload** path —
match on `parent` directly. Prefer `parent ==` over `id in_subtree_of` when
you only care about direct children: it's a cheap payload compare with no
daemon ancestry walk. Use `in_subtree_of` only for the whole subtree
(grandchildren and deeper).

Two more high-value variants (full payload-shape notes in runbook §8.2–§8.3):

```json
// Wake when one of my MRs goes green (SSE full-payload; mr_* are custom fields)
{"kind": "bead_event", "events": ["updated", "closed"],
 "match": {"all": [
   {"field": "type", "op": "==", "value": "merge-request"},
   {"field": "mr_pipeline_status", "op": "==", "value": "success"},
   {"field": "created_by", "op": "==", "value": "<your-bot-name>"}
 ]}, "throttle": "120s"}

// Wake when an escalation under my epic resolves (in_subtree_of is daemon-backed)
{"kind": "bead_event", "events": ["closed"],
 "match": {"all": [
   {"field": "type", "op": "==", "value": "escalation"},
   {"field": "id", "op": "in_subtree_of", "value": "{{assign_bead}}"}
 ]}, "throttle": "60s"}
```

Edge-change wakes (`comment.added` / `dependency.added` / `label.added`) are
functional as of 2026-06-03 — name the **short topic form** in `events` (the
bare leaf `added` is ambiguous) and scope with `in_subtree_of`. See runbook
§8.6b.

`throttle` (min wall-clock between fires) and `debounce` (coalesce a burst)
keep a comment/label storm from waking you on every frame. `max_concurrent`
defaults to `1` for gardeners — a yielded run is woken by the next fire, not
run in parallel.

### Recipe B: `decision_resolved` wake

Wake when a blocking `gb decision` your crew raised is answered. Use the
first-class `decision_resolved` kind and match the keys the NATS decision
payload **actually carries** (`watch_project`, `watch_role`, `watch_bead_id`,
`chosen_label`, `chosen_index`, `resolved_by`):

```json
{"kind": "decision_resolved",
 "match": {"all": [
   {"field": "watch_project", "op": "==", "value": "gardenio"},
   {"field": "watch_bead_id", "op": "in_subtree_of", "value": "{{assign_bead}}"}
 ]}}
```

Branch on the operator's pick with `{"field": "chosen_label", "op": "==",
"value": "ship-it"}`. **Correction to older guidance:** the decision payload
does **not** carry `raised_by_agent.task_id` — that key is never populated, so
a match on it silently never fires. Scope via `watch_bead_id` +
`in_subtree_of` instead (daemon-backed, so it works on the projection shape).

### Recipe C: hybrid event + cron floor (the recommended default)

Pure event wakes are fast but not guaranteed: a controller restart, a dropped
SSE frame, or a payload that didn't carry the field you matched on all mean a
missed wake. The recommended shape for almost every opportunistic gardener
pairs a **`bead_event` entry** (fast response) with a **`cron` entry carrying
a `preflight`** (slow catch-up floor that's skipped when there's genuinely
nothing to do):

```json
[
  {"kind": "bead_event",
   "events": ["closed", "updated"],
   "match": {"all": [
     {"field": "parent", "op": "==", "value": "{{assign_bead}}"}
   ]},
   "throttle": "60s"},
  {"kind": "cron",
   "cron": "17 */6 * * *",
   "timezone": "America/Los_Angeles",
   "preflight": {"all": [
     {"type": "bead_query", "op": ">", "value": "0",
      "parent": "{{assign_bead}}",
      "types": ["task", "bug", "epic"],
      "statuses": ["open", "in_progress", "blocked"]}
   ]}}
]
```

The `cron` entry is a `trigger:schedule`-style fire with a `bead_query`
preflight: every 6 hours it re-checks "is there still open work under my
epic?" and only spawns a run if the answer is yes — so a missed event still
gets picked up within the floor interval, but a quiet gardener stays dormant.

Keep the cron floor whenever **any** of these holds: event delivery isn't
guaranteed; the match predicate depends on a field that may not be on the
payload; or the gardener verifies a sliding window of state rather than
reacting to one event. If none apply — you care about exactly one bead-id
closing and the loop ends after that — drop the cron entry and ship the pure
event form.

### Recipe D: `bead_query` loop-end (self-terminate when work is drained)

An opportunistic gardener that wakes on events must also know when to **stop**.
The `bead_query` loop-end condition closes the schedule cleanly the moment its
scope of work empties — no idle ticks against a finished epic:

```json
{
  "loop_end": {"any": [
    {"kind": "bead_query", "op": "==", "value": "0",
     "types": ["task", "bug", "epic"],
     "parent": "{{assign_bead}}",
     "status": ["open", "in_progress", "blocked"]},
    {"kind": "specific_bead_closed", "bead_id": "{{assign_bead}}"},
    {"kind": "wall_clock", "duration": "168h"}
  ]},
  "on_loop_end": "close"
}
```

`loop_end` is evaluated **before each fire and at the end of each run**, so a
gardener that wakes on a child-closed event, drains the last open child, and
re-evaluates will find `bead_query == 0` and close itself. The
`specific_bead_closed` and `wall_clock` entries are belt-and-suspenders: close
if the epic itself closes, and a hard schedule-wide ceiling so a wedged loop
can't run forever. **Closed-root short-circuit:** when the named `parent` root
is itself `closed`, the subtree count is forced to `0` — so a closed root epic
is the operator's "done" signal even if stray follow-on work was grafted under
it (runbook §2).

**Externally-blocked exclusion** (`exclude_blocked_by_open_types`): a gardener
whose only open child is *legitimately blocked* on an operator escalation or an
unresolved decision would otherwise loop forever — the child is `open`, so the
count never drops to `0`, and the gardener reads as STALE-STOP (ka-0000002198).
Opt a `bead_query` loop-end into the exclusion so those children are subtracted
from the count:

```json
{"kind": "bead_query", "op": "==", "value": "0",
 "types": ["task", "bug", "epic"],
 "parent": "{{assign_bead}}",
 "status": ["open", "in_progress", "blocked"],
 "exclude_blocked_by_open_types": ["escalation", "decision"]}
```

A matched child is excluded only when it has a **forward blocking dep**
(`blocks` / `blocked-by` / `depends-on`, the same allow-list as `gb ready`)
whose target bead is **open** (`open`/`in_progress`/`deferred`) **and** whose
type is in the configured set. So a child blocked on an open `escalation`
(e.g. via `gb escalation create --requesting-task`) is treated as
"waiting on the operator", not "work to do", and the gardener can stop; the
moment the operator closes that escalation the child counts again. Empty/omitted
keeps the legacy count-only behaviour.

**The type set is data-driven — extend it to whatever "waiting, not
actionable" means for your gate.** The filter builds its `wanted` set straight
from the configured array (`subtractExternallyBlocked`, leaf_daemon.go), so
listing more types just excludes more blocked children — there is no code path
that special-cases `escalation`/`decision`. A groomer/de-lane pre-flight, for
example, cannot action a child whose blocker is an open **task** or **bug** any
more than one blocked on an open escalation — in every case the child correctly
stays parked per the runbook's BLOCKED branch. Such a gate should therefore
declare `"exclude_blocked_by_open_types": ["escalation","decision","task","bug"]`
so it counts only genuinely-groomable beads and goes dormant on a fully-blocked
(or fully-triaged) lane instead of waking a pod every tick to re-confirm there
is nothing to do. This is exactly the intake-groomer trigger fix
(ka-0000007887): the effective count went 15 → 0 on a lane where all 15 beads
were blocked or already triaged.

**Fail-safe — the filter only ever REDUCES the count, and never drops work.**
It is *count-reducing only*: it subtracts a matched bead solely when it can
*positively prove* that bead is blocked_by an open bead of a listed type, and it
never returns below zero. Every uncertainty leaves the count untouched — the
client lacks the batch capability, the matched set exceeds the 500-row scan cap,
a dependency or blocker fetch errors — so the gate over-counts (keeps looping /
keeps firing) rather than under-counting. Two consequences worth internalising:

- **It cannot skip actionable work.** The excluded beads stay open, keep their
  `needs-intake`/lane membership, and re-enter the count the instant their
  blocker closes. Subtraction changes only whether the *gardener wakes*, never
  what work exists — a mis-subtraction parks the gardener, it does not lose a
  bead.
- **It is a no-op above the scan cap.** Because the exclusion needs to inspect
  every matched row's deps, it only runs when the whole matched set fits in one
  `beadQueryBlockedScanCap` (500-row) page. If the matched set is larger the
  filter excludes nothing (`len(result.Beads) < result.Total` bail). So a
  gate that relies on the exclusion **must also be scoped small enough to stay
  under the cap** — e.g. project-scoped via the `project` key
  (ka-0000003939), not counting a label fleet-wide. An unscoped gate whose
  matched set is >500 rows silently ignores the exclusion entirely.

> **Mandatory self-block.** Per project advice (`ka-0000000974`), every gardener
> MUST declare at least one intelligent auto-block — a `run_budget` plus a
> progress-aware or `bead_query` `loop_end` — so it terminates when it stops
> making progress, not just when a pod crashes or a multi-week `wall_clock`
> elapses. A bare-cron gardener with no `loop_end` is the anti-pattern this
> section exists to retire.

### Recipe E: progress-aware self-block (wedge detection)

`bead_query` loop-end catches "work is done". It does **not** catch "the loop
is spinning but nothing is moving" — a gardener that wakes, dispatches a crew
that bounces, and re-wakes forever. Progress-aware self-block adds wedge
detection. Configure a `progress:` block that defines what counts as forward
motion, then reference it from a `no_progress_for` loop-end entry:

```json
// progress: field
{
  "events": [
    {"kind": "child_closed",      "parent": "{{assign_bead}}"},
    {"kind": "mr_merged",         "author_label": "gardener:{{schedule_id}}"},
    {"kind": "decision_resolved", "raised_by_agent_subtree_of": "{{assign_bead}}"}
  ],
  "windows": [
    {"name": "last_3_runs", "scope": "last_n_runs", "n": 3}
  ],
  "surface": {"in_prime": true, "in_wrapup": true}
}
```

```json
// stop.loop_end.any[] — add alongside the bead_query entry from Recipe D
{"kind": "no_progress_for", "runs": 3}
```

`no_progress_for: {runs: 3}` fires `loop_end` when the last 3 closed runs all
report `progress_score == 0`. With `surface.in_wrapup: true`, `gb stop`
**rejects** a wrap-up missing `progress_score` (int) and `progress_summary`
(string), so the gardener is forced to self-attest each run:

| Score | Meaning |
|---|---|
| `0` | Did nothing material (yielded, woke, nothing actionable). |
| `1` | Incremental (dispatched a crew, resolved one decision). |
| `≥2` | Substantive (closed a child, merged an MR, hit a milestone). |

With `surface.in_prime: true`, each run sees a `## Loop Progress` block at the
top of `gb prime` showing per-window counters and a drain-rate estimate — the
same counters that feed `no_progress_for`. Together, Recipes D + E give a
gardener two independent ways to end: **done** (`bead_query == 0`) and **stuck**
(`no_progress_for`).

### Putting it together: a fully opportunistic epic gardener

Recipes A–E composed onto one `type=trigger` bead — wakes on child closes and
decision answers, floor-ticks every 6h only while work remains, and ends on
either drained-work or no-progress:

```yaml
type: trigger
title: "Gardener for ka-EPICID (opportunistic)"
fields:
  enabled:     "true"
  role:        gardener
  project:     gardenio
  assign_bead: ka-EPICID
  prompt:      "<gardener playbook prompt>"
  run_budget:  "40"
  triggers: |
    [
      {"kind":"bead_event","events":["closed","updated"],
       "match":{"all":[{"field":"parent","op":"==","value":"{{assign_bead}}"}]},
       "throttle":"60s"},
      {"kind":"decision_resolved",
       "match":{"all":[
         {"field":"watch_bead_id","op":"in_subtree_of","value":"{{assign_bead}}"}
       ]}},
      {"kind":"cron","cron":"17 */6 * * *","timezone":"America/Los_Angeles",
       "preflight":{"all":[
         {"type":"bead_query","op":">","value":"0",
          "parent":"{{assign_bead}}","types":["task","bug","epic"],
          "statuses":["open","in_progress","blocked"]}
       ]}},
      {"kind":"manual","label":"operator-fire"}
    ]
  stop: |
    {
      "loop_end": {"any": [
        {"kind":"bead_query","op":"==","value":"0",
         "types":["task","bug","epic"],"parent":"{{assign_bead}}",
         "status":["open","in_progress","blocked"]},
        {"kind":"specific_bead_closed","bead_id":"{{assign_bead}}"},
        {"kind":"no_progress_for","runs":3},
        {"kind":"max_runs"},
        {"kind":"wall_clock","duration":"168h"}
      ]},
      "run_end": {"any": [
        {"kind":"yielded_then_woken_then_idle"},
        {"kind":"no_actionable_state"}
      ]},
      "hard_stop": {"any": [{"kind":"consecutive_failures","value":5}]},
      "on_loop_end": "close"
    }
  progress: |
    {
      "events": [
        {"kind":"child_closed","parent":"{{assign_bead}}"},
        {"kind":"decision_resolved","raised_by_agent_subtree_of":"{{assign_bead}}"},
        {"kind":"mr_merged","author_label":"gardener:{{schedule_id}}"}
      ],
      "windows": [{"name":"last_3_runs","scope":"last_n_runs","n":3}],
      "surface": {"in_prime":true,"in_wrapup":true}
    }
```

Runtime: the gardener sleeps until a child closes, a scoped decision resolves,
or the 6-hour floor ticks (and even then only if open work remains). It wakes,
acts, self-attests `progress_score`, and `gb stop`s. It closes itself when the
epic's work is drained (`bead_query == 0`), the epic bead closes, three runs
make no progress, the run budget is spent, or the 7-day ceiling hits —
whichever comes first. No idle ticks against a finished or wedged epic.

> **Schema caveat.** The rich inline `triggers:` / `stop:` / `progress:` shape
> is the engine's full capability. Depending on the live `type:trigger` config
> schema (config bead `ka-0000001965`), bead creation may reject some keys;
> `gb gardener start` emits the bare firing shape (flat `cron` + `conditions`
> preflight + a `gardener-epic:<id>` label, with the budget/goal folded into the
> prompt) until the schema is widened. See the note in
> [Scheduled gardeners §5.1](#scheduled-gardeners-typetrigger) and runbook §5.1.

## Waking an agent when its CI concludes (`gb wait ci`)

An agent that has pushed and needs CI's verdict does not poll, and it does not need a
trigger bead. It parks on the run and stops:

```bash
git push
gb wait ci --repo mattbakerconsulting/gardenio --sha "$(git rev-parse HEAD)"
gb stop --wrapup '...'
```

When every workflow run for that sha has concluded, red or green, the agent is resumed in
place (same name, same claimed task, same transcript) with `wake_reason=ci`. Its first
step is `gb wait ci --result`, which prints each workflow's conclusion and marks the wait
consumed. Plan of record: [`plans/wake-on-ci-completion.md`](../plans/wake-on-ci-completion.md)
(epic `gd-j4OrJOprh9`).

This is deliberately not a `trigger:webhook` bead. A webhook trigger spawns a fresh agent
per event and knows nothing about who is waiting; this path writes only to the bead of the
agent that asked, and never takes an agent name from a payload.

### What `gb wait ci` records

The wait is a set of fields on the calling agent's own bead. One wait per agent: a second
`gb wait ci` replaces the first.

| Field | Meaning |
|---|---|
| `ci_wait_state` | `pending`, then `resolved` or `timed_out`, then `consumed` once `gb wait ci --result` has read it. `cancelled` (reason in `ci_wait_result`) when `gb wait ci --cancel` drops it, its task closes, or no CI will run on its sha. |
| `ci_wait_key` | `OWNER/REPO@<40-char sha>`, the lookup key for deliveries and polls. |
| `ci_wait_workflows` | Optional comma-separated filter from `--workflow` (repeatable). Empty means every run on the sha. |
| `ci_wait_task` | The claimed task the wait parks. Defaults to the agent bead's `task_id`; `--requesting-task` overrides it. |
| `ci_wait_deadline` | RFC3339, now + `--timeout` (default `2h`). At the deadline the wait resolves `timed_out` and still wakes the agent. |
| `ci_wait_result` | JSON list of `{name, run_id, run_attempt, conclusion}`, written when the wait concludes. |
| `ci_wait_ledger` | The resolver's mirror of the runs it has seen for the sha, so a bridge restart keeps them. |

Two inputs bite people:

- **`--sha` must be the full 40-character sha.** An abbreviated sha matches no runs, so the
  wait only ever ends at its deadline. `gb wait ci` rejects anything shorter.
- **`--repo` must be the repository's current `OWNER/NAME`.** GitHub reports
  `repository.full_name` in its deliveries, and the key only folds case. A pre-transfer
  name (`joyousaversion/gardenio`, before the move to `mattbakerconsulting/gardenio`) never
  matches a webhook delivery, so such a wait is resolved only by the poll fallback or the
  25-minute timer below. Read it with `gh repo view --json nameWithOwner -q .nameWithOwner`
  or `git remote get-url origin`.

### What `gb stop` does with a pending wait

`gb stop` treats `ci_wait_task` as parked: the claimed-work gate lets that task stay
`in_progress`, with no handle bead and no `blocks` edge. The agent bead closes with
`successor=same` and `close_reason=ci_wait_parked`, the dormant-identity marker the
event-wake path reopens in place. The stop also stamps `next_respawn_at` 25 minutes out
(clamped to the deadline), so the controller's wake-scanner revives the agent even if the
resolver below is not running. If the wait has already concluded when the agent stops,
`gb stop` refuses and says to run `gb wait ci --result` first.

| Command | Use |
|---|---|
| `gb wait ci --status` | Read-only: state, key, deadline, result. |
| `gb wait ci --result` | Print the conclusions and mark the wait consumed. On a still-pending wait it asks GitHub (`gh api`) for the sha's runs first; if they are still running it says so, and the agent runs `gb stop` again. |
| `gb wait ci --cancel` | Drop a pending wait (`cancelled`): `gb stop` no longer parks its task and nothing wakes the agent for it. `--result` and `gb stop` cancel a pending wait on their own once its task is closed, or once its sha has no runs and every pull request carrying it has moved its head past it or closed (the merge desk merged `main` into the branch, gd-lHWcyCRj1Q). |

### Who resolves the wait: github-bridge

The resolver lives in `cmd/github-bridge` (`ciwait.go`, `ciwait_poll.go`, `ciwake.go`; pure
core in `internal/ciwait`). It is on whenever the bridge has a beads daemon
(`BEADS_HTTP_ADDR`); `CI_WAIT_RESOLVER=off` turns it off.

1. **Deliveries.** Every verified `workflow_run` delivery goes into an in-memory ledger per
   sha, deduped on `workflow_run.id` + `run_attempt`. The ledger is per sha, not per waiter,
   because the agent pushes before it runs `gb wait ci`: the `requested` and `in_progress`
   deliveries usually arrive before any waiter exists.
2. **Sweep** (every `CI_WAIT_SWEEP_INTERVAL`, default `15s`). Lists agent beads with
   `ci_wait_state=pending` in any status (the waiter is usually closed), merges each one's
   `ci_wait_ledger` with the in-memory ledger, and evaluates it.
3. **Quiesce rule.** A wait resolves when every awaited run is `completed` and the ledger has
   not changed for `CI_WAIT_QUIESCE` (default `60s`), because a push can create runs lazily
   (a later workflow, a re-run that bumps `run_attempt`, a `workflow_run`-chained workflow).
   `cancelled`, `skipped` and `action_required` count as concluded and are reported verbatim.
4. **Resolution.** One PATCH writes `ci_wait_state` and `ci_wait_result`. Then the bridge
   wakes the agent through `internal/agentwake` (the same wake the decision and escalation
   respawners use): it reopens a dormant identity in place, thaws a frozen one, or spawns a
   fresh agent with the conclusions in its prompt. It comments the red and green runs on
   `ci_wait_task`. A laptop agent is never woken as a pod; its runner's next session reads
   the wait itself.
5. **Poll fallback** (needs the GitHub App on the bridge). GitHub does not retry a failed
   delivery, so the bridge also lists `GET /repos/{r}/actions/runs?head_sha=` per waiter:
   first after `CI_WAIT_POLL_GRACE` (default `2m`), then every `CI_WAIT_POLL_INTERVAL`
   (default `5m`), plus one confirming poll before a wait resolves on a ledger no poll has
   seen. Polls feed the same ledger and the same rule. Without an App the resolver is
   webhook-only and logs `poll_fallback=false` at start.
6. **Deadline.** A wait still pending at `ci_wait_deadline` resolves `timed_out`, including
   one that never saw a run, and wakes the agent.

Every layer has a backstop: a missed delivery is caught by the poll; a bridge that is down
is covered by the stop's 25-minute `next_respawn_at` timer, after which
`gb wait ci --result` reads GitHub itself; a failed wake leaves the wait resolved and
unconsumed, and the timer still revives the agent.

Enabling it in a cluster (deploy values, secrets, pointing the GitHub App's webhook at the
bridge) is in the
[github-bridge runbook](runbooks/github-bridge.md#wake-on-ci-gb-wait-ci). Until that is
done only the timer runs, so a parked agent wakes every 25 minutes rather than about a
minute after CI concludes.

### Diagnosing a wait that did not wake

| Symptom | Look at |
|---|---|
| Agent never woke | `kd show <agent-bead> --json \| jq '.fields \| with_entries(select(.key \| startswith("ci_wait")))'`. `pending` with an empty `ci_wait_ledger` means no delivery or poll matched the key: check the sha length and the repo name. |
| Woke only after 25 minutes | The resolver is not running or not receiving deliveries. Check the github-bridge `ci wait resolver enabled` start log and `gardenio_github_webhook_deliveries_total{outcome="accepted"}`. |
| `resolved` but the agent is still closed | A failed wake: look for `ci wait: wake failed` in the github-bridge log. The timer backstop revives it. |
| `timed_out` with an empty result | No run was ever seen for the key before the deadline. Same causes as the first row. |

## See also

- `triggers.go` — engine, action dispatch, common field parsing
- `trigger_permissions.go` — auth, allowlists, audit bead format
- `trigger_source_*.go` — per-source implementations
- `gardenio/CLAUDE.md` — component overview
- `scheduler` package — cron parser used by the `schedule` source
