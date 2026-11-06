# Loop Runbook: Triage-Intake Janitor (sweep-IN)

| | |
|---|---|
| Trigger bead | ka-0000003783 (`trigger:schedule`) |
| Anchor epic | ka-0000007143 (bead triage-intake lane; complement of the sweep-OUT groomer ka-0000006210 / [`intake-groomer.md`](./intake-groomer.md)) |
| Cadence | every 30m (`17,47 * * * *`), cooldown 5m, max_concurrent 1; pre-flight fires only while the gardenio `needs-intake` lane holds an open work bead |
| Role | gardener |

The janitor is the **sweep-IN** half of the two-lane triage-intake system.
It applies the `needs-intake` label to genuinely orphaned/abandoned gardenio
work beads (parentless leaves with no live hand-off actor) so the sweep-OUT
groomer (ka-0000006210) and the ready-queue dispatchers can pick them up. Its
mission is defined by anchor epic ka-0000007143; it is a STANDING gardener that
goes dormant between sweeps and does NOT self-complete on a momentarily-empty
queue.

> **Dormancy gate (why it must stay asleep on a triaged lane).** The pre-flight
> `bead_query` counts **open gardenio beads carrying the `needs-intake` label**
> — the janitor's own lane — not "any open gardenio bead". This is the fix
> tracked by ka-0000005015. The prior gate,
> `{types:[task,bug,epic,feature], labels:[project:gardenio], statuses:[open,in_progress]}`,
> counted **every** open gardenio work bead — ~965 at time of writing — so it
> was **always true** and the cron fired unconditionally every 30m whether or
> not anything needed sweeping. Its final six live runs swept 0,1,0,0,0,0 beads
> — empty wakes burning an Opus pod per tick (D3 compute-economy / D6
> loop-hygiene drag; scorecard ka-0000005368, grade C). Gating on `needs-intake`
> makes the effective count fall to 0 when the lane is empty, so the gardener
> stays dormant instead of waking to re-confirm there is nothing to do. This
> mirrors the proven complement-groomer shape (ka-0000006210), whose gate has
> gated on `needs-intake` since it was authored.

## The corrected pre-flight condition

```json
[{
  "type": "bead_query",
  "op": ">",
  "value": "0",
  "project": "gardenio",
  "labels": ["needs-intake"],
  "types": ["task", "bug", "epic", "feature"],
  "statuses": ["open"]
}]
```

- **`labels: ["needs-intake"]`** — the lane membership. `needs-intake` is the
  label the OrphanSweeper (`gardenio/controller/internal/scheduler/orphan_sweep.go`,
  `DefaultIntakeLabel`) applies and the whole triage-intake system keys on. A
  count `> 0` means the lane holds untriaged work; `== 0` means dormant.
- **`project: "gardenio"`** — scope. The evaluator propagates this as a
  `project:gardenio` label filter (ka-0000003939); without it a `> 0` gate would
  count beads in **every** project and fire even when gardenio's own lane is
  empty, the exact bug ka-0000000231 / ka-0000003939 guard against.
- **`types` spans task/bug/epic/feature** and **`statuses` is `open`** — a
  `needs-intake` bead is actionable while open regardless of kind. The gate
  logic (see below) intentionally applies NO `kind` filter (ka-0000006847), so
  a `type=task`/`kind=data` needs-intake bead still counts.

## Verifying the gate logic

The gate is evaluated by the shared conditions leaf, not by gardener-specific
code. `bead_query` dispatches to `evalBeadQuery` →`evalBeadQueryCount` in
[`gardenio/controller/internal/scheduler/conditions/leaf_daemon.go`](../../controller/internal/scheduler/conditions/leaf_daemon.go):
it runs `ListBeadsFiltered` with the leaf's `Types`/`Statuses`/`Labels`
(project prepended as `project:<name>`) and compares `result.Total` against the
leaf's `op`/`value`. So `labels: ["needs-intake"], op: ">", value: "0"` fires
iff at least one open gardenio needs-intake work bead exists.

Reproduce the effective count by hand (the number the gate compares against):

```sh
kd list --status open -l needs-intake -l project:gardenio \
  --type task --type bug --type epic --type feature --limit 5000 --json \
  | jq length
```

Compare against the always-true predecessor (drop `-l needs-intake`, add
`--status in_progress`) to see the magnitude of the empty-wake problem the fix
removes.

## Relationship to the groomer (the fight-loop guard)

The janitor sweeps `needs-intake` **in**; the groomer de-lanes it **out**. Run
naively against the same beads they oscillate (the janitor↔groomer fight-loop,
decisions ka-0000008119 / ka-0000002231 — the same parentless beads re-labelled
up to 16×/day). Two guards keep them apart:

- The OrphanSweeper's **de-lane cooldown** backs off from re-surfacing a bead
  the groomer recently de-laned (`orphan_sweep.go`, `defaultOrphanDeLaneCooldown`),
  and its **parent / open-children** discriminator skips parented work and live
  tree-roots outright.
- Any janitor revival should additionally carry a **recently-groomed skip-rule**
  (skip beads carrying an `intake-groomed:` marker) so a revived sweep cannot
  restart the churn — see revival decision ka-0000007871.

## Lifecycle

Standing sweeper: park between sweeps (`gb stop`), wake on the cron. Do NOT
self-complete on a momentarily-empty lane — an empty intake queue means park,
not stop (dormancy is automatic via the corrected bead-query pre-flight).
Bounded by `run_budget` + `max_runs` + `hard_stop` wall-clock; self-blocks on
`no_progress_for/3` or when anchor epic ka-0000007143 closes.

> **Revival is a separate operator decision.** As of 2026-07-15 this trigger is
> **closed + `enabled: false`** (self-closed on `loop_end/max_runs` while
> enabled — the closed-enabled dark class, bug ka-0000004633). Landing this gate
> does NOT revive it; reopen + enable is gated on decision ka-0000007871, which
> also requires resetting `run_budget`/`max_runs` (runs_spawned=60 already
> exceeds max_runs=50) and adding the recently-groomed skip-rule.

## Provenance

Pre-flight gate corrected from the always-true "any open gardenio bead exists"
predicate to the `needs-intake` lane gate on 2026-08-12 (task ka-0000005015,
scorecard ka-0000005368, revival precondition per ka-0000007871). The condition
change is applied to the live trigger bead ka-0000003783; scheduling mechanics
stay on the trigger bead. This runbook is the sweep-IN companion to the
sweep-OUT [`intake-groomer.md`](./intake-groomer.md).
