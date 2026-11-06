# Repeatable GardenerSpec backfill procedure

**Task:** [ka-0000007453] · **Epic:** [ka-0000000380] (Backfill GardenerSpec engine
blobs onto legacy spawn_agent gardeners) · **Written:** 2026-08-20

> Idempotent, dry-run-first procedure to seed a minimal identity-only
> `gardener` GardenerSpec blob onto each legacy spec-less gardener, so the engine
> tracks run tallies natively instead of beads-viewer deriving them forever
> (bug [ka-0000005661]). The **execute + verify on a sample** run is the sibling
> task [ka-0000001186] — this task delivers the script and its dry-run output.

## Inputs (read these first, in order)

1. **T1 manifest / target set** — [ka-0000000351],
   `gardenio/docs/gardener-spec-less-manifest-2026-06-15.md` and
   `gardenio/scripts/enumerate-legacy-gardeners.py`. The backfill script reuses that
   script's exact set logic (role:gardener by label OR field, deduped across
   type=trigger/schedule) and its `deriveGardenerRunCount` port for the counts.
2. **T2 shape / behavior-preservation proof** — [ka-0000007784],
   `gardenio/docs/gardenerspec-backfill-shape-2026-08-06.md`. Defines the exact
   6-key blob and proves, gate-by-gate against `gardener_evaluator.go`, that it
   fires exactly as the spec-less trigger did.

## The tool

`gardenio/scripts/backfill-gardenerspec.py` — idempotent, dry-run by default.

```bash
# 1. DRY-RUN (default, writes nothing): print the old-absent -> new-spec diff
#    for every spec-less type=trigger gardener, for operator review.
gardenio/scripts/backfill-gardenerspec.py

# 2. Review a single gardener, or the machine-readable plan.
gardenio/scripts/backfill-gardenerspec.py --only ka-0000003150
gardenio/scripts/backfill-gardenerspec.py --json

# 3. APPLY (idempotent). Start with ONE gardener to verify (ka-0000001186),
#    then widen. Any gardener that already has a blob is always skipped.
gardenio/scripts/backfill-gardenerspec.py --apply --only ka-0000003150
gardenio/scripts/backfill-gardenerspec.py --apply

# type=schedule legacy gardeners are opt-in:
gardenio/scripts/backfill-gardenerspec.py --include-schedules [--apply]
```

## What it writes (per gardener)

The **6-key identity-only blob** (design §2), stored as a JSON *string* in the
`gardener` field (via `-f gardener=@file` so kd does not coerce it to a nested
object — the engine reads `bead.Fields["gardener"]` as a string,
`triggers.go:4364`):

```json
{
  "name":               "<schedule_name, else cleaned trigger title>",
  "terminal_goal":      "<best-effort one-liner from the title>",
  "runs_spawned":       <derived spawned>,
  "runs_completed":     <derived completed>,
  "last_agent_id":      "<head of the life-thread>",
  "last_checked_agent": "<same as last_agent_id>"
}
```

plus the **top-level typed mirror** the engine maintains (design §2.2):
`runs_spawned` / `runs_completed` as JSON **integers** (kbeads 400-rejects them
as strings) and `last_agent_id` as a string, all in the same `kd update`. When a
gardener has `max_concurrent > 1` the script also sets `max_concurrent_runs` to
that same value — the one non-identity field — purely to **preserve** the
pre-blob effective concurrency, because any non-nil blob activates the
concurrency cap whose unset default is 1 (design §5.1).

Counters are seeded from the **same `deriveGardenerRunCount`** the viewer already
displays, so the native tally equals the number operators already see — no jump.

## Safety properties (acceptance criteria)

- **Idempotent / re-runnable** — `has_spec()` skips any gardener that already
  carries a non-empty `gardener` blob; nothing is ever overwritten. A
  fully-applied set is an all-skipped no-op. A partial apply is safe to resume.
- **Dry-run diff before writing** — no flags = no writes; prints
  `gardener field: (absent) -> <blob>` + the typed mirror for operator review.
- **Read-before-write** — even under `--apply`, each gardener is re-fetched and
  re-checked for a blob immediately before its write, so a gardener that gained a
  blob concurrently (another apply, the engine, an operator) is still skipped.
- **Fail-closed on counts** — a gardener whose run counts cannot be derived is
  reported and **skipped**, never written with a guessed tally.
- **Behavior-preserving** — only identity + already-true counters (+ concurrency
  preservation) are written; every loop-controlling field (`run_budget`,
  `max_runs`, `stop`, `exit_condition`, `on_exit`, `assign_bead`, `standing`) is
  left at its zero value per design §3/§7, so firing behavior is unchanged.

## Verify after apply (hand-off to ka-0000001186)

1. `kd show <gardener> --json | jq .fields.gardener` → the stringified blob is
   present and parses.
2. `/gardener/<id>` in beads-viewer renders a **non-derived** engine tally equal
   to the previously-derived count.
3. Watch one cron cycle: the gardener still fires on cadence (concurrency
   preserved), `runs_spawned` advances natively on the next fire, and
   `runs_completed` does **not** jump by a phantom +1 (the `last_checked_agent`
   de-dup guard, design §5.2).
