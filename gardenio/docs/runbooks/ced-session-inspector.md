# Loop Runbook: Real-Data Session Inspector (CED Rubric)

| | |
|---|---|
| Trigger bead | ka-0000004081 (`trigger:schedule`) |
| Anchor epic | ka-0000003297 (inspection); remediation epic ka-0000004371 |
| Cadence | every 6h (`13 */6 * * *`); squawks route to #gardenio-auto via the schedule channel stamp |
| Role | auto-inspector |
| Loop bounds (machine-enforced gardener fields) | run_budget 120 (~30 days at 4×/day, then operator re-authorization), no_progress_for 3, consecutive_failures 5, wall-clock 720h; single run 30m |
| Formula | ced-session-inspector ka-0000004287 (6 molecule steps) |

## 1. Condition to keep true

The fleet's beads-data-plane quality is under continuous, OSHA-style
inspection: every ~6h, ONE random recent REAL agent session gets graded on the
Correctness / Efficiency / Discoverability ("CED", plus Advice/Context) rubric,
and every data-plane defect found is filed as a deduped bug AND driven to
remediation. The highest-value output is data-plane-tagged DISCOVERABILITY
findings — real ergonomics defects in beads search/list/advice surfacing (the
"perfect serendipity" the operator wants) — and landed fixes for them.

### Absence is not evidence — the two absence-unfalsifiable dimensions

The rubric lives on the formula bead (ka-0000004287); this note is the durable
rationale behind two guardrails baked into its `judge` and `file-bugs` steps.
Both were added after a CED cycle filed a false-positive pair against an
*exemplary* session (ka-0000001943 / ka-0000006883, session ka-0000003562):

- **Advice/Context is INJECTED, not FETCHED.** Advice matched to an agent is
  rendered straight into the cacheable system-prompt body by `gb prime`
  (controller `buildSystemPromptBody` → `outputAdvice`,
  `controller/cmd/gb/prime.go`). An agent CONSULTS its advice by reading its own
  system prompt — **not** by running `gb advice show`. So the absence of a
  `gb advice show` invocation in a transcript is the *expected, healthy* state
  and is never evidence that guidance went unconsulted or unused.
- **Discoverability is falsifiable only from a POSITIVE observable.** A search
  that returned nothing when the artifact demonstrably existed
  (`needed_artifact_missed`), or a duplicate re-created after a failed search
  (`gave_up_or_duplicated_after_search`). The mere *non-appearance* of
  `kd search` / `kd list --filter` / `gb advice show` is not a defect — an agent
  handed a complete, root-caused work order correctly performs **zero**
  discovery (searching would be redundant, which the Efficiency dimension itself
  penalizes).

A session with no discovery activity and no missed/duplicated artifact scores
these two dimensions at baseline (7), files nothing, and is a *clean* cycle. A
finding inferred purely from transcript absence is a false positive that refiles
every tick against well-behaved sessions.

**FOCUS — plan gardeners and crews (operator ask, Matt, 2026-07-20).** The
sampled session is drawn from the loop-workhorse roles where the fleet's
troubles concentrate today: `plan-gardener`, `gardener` / `gardener-*` (bugfix,
release-owner, remediate-gardeners, …), and `crew` / `crew-*` (fable,
ultralite, …). This is a sampling BIAS, not a hard gate: if the focus pool is
empty after coverage screening on a given tick, the pick step falls back to any
real session so the cycle still produces a graded sample (note the fallback).
The rubric, the hard caps, and the machine-enforced gardener bounds are
unchanged — only the worksite pool narrowed.

This is a STANDING, PERPETUAL inspector, not a finite epic-completion gardener.
An empty child frontier between cycles is the NORMAL, HEALTHY resting state —
fresh work is re-poured each tick. (Treating "nothing open under my epic" as
done is the exact misread that STALE-STOPped the loop on 2026-06-28; revived
with revisions via decision ka-0000005530.)

## 2. Drift signal

- **Frozen corpus** — `gb audit list` returns no recent sessions. The corpus
  is LIVE as of 2026-07 (bead-namespaced S3 reader fix ka-0000001976 closed
  freeze bug ka-0000002331); if it re-freezes, that IS the drift.
  Three consecutive barren cycles auto-escalates to the operator via the
  `no_progress_for` wedge-detector — do NOT keep spinning silently against a
  dead corpus (the historical ka-0000002331 failure mode burned ~30 days).
- Repeated hard caps on the same rubric dimension across sessions — a
  systemic data-plane defect not yet remediated.
- Molecule steps left open/claimed after a cycle — the despawn gate will
  reject `gb stop` with "claimed work pending".

## 3. Restoring evidence

- A healthy cycle = one real session sampled at random (ITER-seeded, no
  cherry-picking), fetched, measured with OBJECTIVE counters, and judged with
  the rubric's HARD CAPS — even if zero bugs are filed. "Corpus healthy,
  session graded clean, nothing to flag" is a productive cycle.
- No Discoverability or Advice/Context bug is filed on transcript ABSENCE alone
  (see "Absence is not evidence" in §1) — those two dimensions score baseline 7
  unless a POSITIVE observable fired (a missed/duplicated artifact, a
  broken/stale pre-prompt command, `dead_weight_fraction` over threshold).
- Filed bugs are deduped against open `kind:ced-audit` bugs (comment +
  `duplicates()` instead of re-filing) and parented to ka-0000003297.
- Remediation is REAL only when each data-plane bug has a scoped fix-task
  parented to ka-0000004371 (blocking the bug, embedding a concrete work
  order: repro + fix sites file:line + verify steps) with crew dispatched.
  Reproduce + confirm root cause before classing — filed hypotheses can be
  wrong.
- All 6 molecule step beads closed as finished.

## 4. Autonomous-vs-approval boundary

Autonomous:
- Pour the formula, sample, grade, file deduped bugs, publish the roll-up doc
  bead + one-line squawk.
- Dispatch `dataplane-fixer` crew on clean-code-MR-class data-plane bugs
  (`gb spawn --task <fix-task-id> --role dataplane-fixer`); gardenio
  self-merge applies (advice ka-0000002230), so no operator gate for in-repo
  data-plane fixes. The role's playbook is advice labeled
  `role:dataplane-fixer`.
- File a DESIGN task (no auto-spawned crew) for larger-investigation bugs.

Requires a human:
- Anything only a human can do → `gb decision` / `gb escalation` + `gb yield`.
- Re-authorization when `run_budget` exhausts.

Never:
- **Observe-only on the inspected agent and the wider fleet** — the remit
  (expanded 2026-06-25 per Matt) is: OBSERVE the inspected agent, never act on
  it directly; but DO drive remediation of the DATA-PLANE defects filed
  (find → file → dispatch-fix). Agent-behavior findings stay observe-only.
- Never double-dispatch — the bead-driven remediation gardener (ka-0000007256)
  may also wake; check before spawning.
- Never disable the own trigger; only the operator does that.

## 5. Durable state

- Bugs under ka-0000003297 (`kind:ced-audit`) — the finding history and the
  dedup corpus.
- Fix-tasks under remediation epic ka-0000004371.
- One roll-up doc bead per cycle on the inspection epic.
- Molecule beads poured per-cycle from formula ka-0000004287 (closed as steps
  finish).
- Machine-enforced gardener fields on the trigger (`run_budget`,
  `no_progress_for`, `progress_score` history) — the wedge-detector's memory.
  Do not rely on prose caps alone.

## Cycle playbook

1. **Pour** the CED rubric formula for this cycle:

   ```sh
   ITER=$(date +%Y-%m-%d-%H%M)
   kd formula apply ka-0000004287 --var PARENT_EPIC=ka-0000003297 --var ITERATION=$ITER --var SESSION_ID= --assignee <your-agent-name>
   ```

   (Leave SESSION_ID empty so the pick step chooses a random recent real
   session.)

2. **Work the 6 molecule steps inline, in dependency order** — each step
   description IS the checklist:
   - **pick-session**: `gb audit list --max-sessions 300 --json`; KEEP only
     the focus roles (`plan-gardener`, `gardener` / `gardener-*`, `crew` /
     `crew-*`), then exclude synthetic `auto-*` roles, the inspector itself,
     and bead_id-less sessions; pick ONE at random (vary by ITER; rotate
     across plan-gardener/gardener/crew over ticks). If the focus pool is empty
     after coverage screening, fall back to any real session and note it. If
     the list returns NO recent sessions at all, the corpus has re-frozen:
     record `progress_score=0`, squawk the symptom, and stop.
   - **fetch-and-measure**: `gb audit fetch <id>`; emit OBJECTIVE counters
     only (no scores).
   - **judge**: score C/E/D 0-10 with the HARD CAPS in the step; tag each
     finding agent-behavior vs data-plane.
   - **file-bugs**: one bug per dimension ≤6, parented to ka-0000003297,
     deduped against open `kind:ced-audit` bugs.
   - **report**: one roll-up doc bead on the epic + a one-line `gb squawk`.
   - **dispatch-remediation**: per data-plane bug — triage to a fix class
     (clean code MR / docs-advice / larger-investigation), create the scoped
     fix-task under ka-0000004371, dispatch `dataplane-fixer` crew (DESIGN
     task instead for larger-investigation). Guard against double-dispatch
     with ka-0000007256.

   Close each step bead as it finishes.

## Wrap-up contract

END EVERY CYCLE with `gb stop --wrapup` carrying a REQUIRED `progress_score`.
First `kd close` (or `kd unclaim`) every bead still claimed — molecule steps
AND any cycle/tracking bead — else the despawn gate rejects `gb stop` and
burns a retry turn. The score drives the `no_progress_for` wedge-detector:

- `progress_score=1` (or higher) if a real session was sampled + graded —
  EVEN IF zero bugs were filed. A quiet corpus must NOT read as no-progress.
- `progress_score=0` ONLY if no session could be sampled+graded (corpus
  frozen / `gb audit list` empty / audit reader broken). Three consecutive
  0-cycles escalates to the operator.

Example healthy wrap-up:

```sh
gb stop --wrapup '{"tldr":"CED cycle: graded session X, filed N data-plane bugs, dispatched M fixer crew","accomplishments":"...","progress_score":1,"progress_summary":"sampled+graded 1 real session; corpus live"}'
```

If genuinely wedged on something only a human can do, file a `gb decision` or
`gb escalation` and `gb yield`. You are respawned next cron tick.

## Provenance

Migrated from the inline prompt of trigger ka-0000004081 on 2026-07-19
(task ka-0000007116, epic ka-0000007152, W3 gardener-runbooks workstream).
The trigger prompt is now a pointer to this file; scheduling mechanics and the
machine-enforced gardener stop-fields stay on the trigger bead.
