# Plan: gardener-placement (work-router gardener)

**Status:** Draft v2 (awaiting operator approval before enabling).
**Tracking bead:** ka-0000005210
**Origin:** Slack ask from Matt 2026-04-29 — *"i need a new gardener that
proactive looks for unclosed and untested work, examines the schedule beads
and their gardeners, and places work with them in their epics."*

**Revision history:**
- v1 (2026-04-29 00:24 UTC): conservative, comment-only, 8/cycle cap, 3-strike
  burial. Matt feedback on MR !1796: *"too conservative; this is the kind of
  attitude that led to us having a huge open queue. review the current state
  of things and reconsider."*
- v2 (this revision): aggressive routing, no per-cycle cap, semantic-match
  fallback, crew-spawn for scoping unparented work, escalate-don't-bury.
  Explicit response to the failure mode Matt called out 2026-04-26 when he
  disabled intake/delivery/release for being "narrowly scoped and ultimately
  useless... bookkeeping cycles but little throughput" (ka-0000005320).

## Problem

The unparented, unassigned open queue in gardenio is currently **159 beads**
(snapshot 2026-04-29 00:36 UTC, `kd list --status=open` against the live
daemon). Today, three of the four gardeners that historically owned pieces of
this queue are **disabled**:

| Gardener | Status | Disabled because |
|---|---|---|
| `ka-0000001610` gardener-intake | enabled=false (2026-04-26) | "too narrowly scoped and ultimately useless" |
| `ka-0000000377` gardener-delivery | enabled=false (2026-04-26) | same |
| `ka-0000003737` gardener-release | enabled=false (2026-04-26) | same |

Of the **7 schedules currently enabled**
(`kd list --type=schedule -f enabled=true`), exactly zero have "drain the
unparented gardenio open queue" as their KPI. The queue is unowned. It grows.

This is the mode Matt called out on MR !1796: *"too conservative; this is
the kind of attitude that led to us having a huge open queue."* A router
that only routes (and routes cautiously, with caps and skip-lists) can
**only** make the disease worse — it inherits the failure mode that took
intake/delivery/release down.

## Mission

**gardener-placement is the owner of the unparented gardenio open queue.**
Its KPI is throughput, not bookkeeping:

> **Each cycle, the count of `status=open` beads with no parent and no
> assignee MUST decrease, OR the gardener MUST explain in its wrap-up why
> every remaining bead is genuinely unrouteable and has been escalated.**

A cycle that walks the queue, comments on a few things, and exits with the
queue size unchanged is a **FAILED** cycle.

The gardener is allowed to use every tool needed to move beads:

1. **Set parent** when a bead obviously belongs in a gardener-owned epic.
2. **Spawn `crew-lite`** to scope an unscoped bead (the smallest possible
   action that moves a bead out of "unparented and unscoped"). This is the
   *only* permitted crew spawn — no implementation crew, no MR-cutting crew.
3. **Comment + label** when a bead matches a lane but no gardener epic is the
   right home — let the lane gardener pull on the next cycle.
4. **Escalate via `gb decision`** when a bead has cycled twice without
   progress — operator picks a route or closes as stale. **Never bury**
   with a permanent `placement:unroutable` label.

For closed-but-untested work it produces an **action**, not just a report:
add a `needs-verification` label and a routing comment naming the lane
gardener. The lane gardener sees the label on its next cycle.

## Hard rules (out of scope)

These remain absolute:

- ❌ Does **not** open MRs, edit code, merge anything.
- ❌ Does **not** modify other gardener beads (no editing prompts, no
  flipping enabled, no closing schedules). Gardener orchestration is an
  operator decision.
- ❌ Does **not** re-parent beads that already have a `parent` — only
  unparented work is in scope.
- ❌ Does **not** invent labels not already in use, EXCEPT the two
  operationally-needed labels this gardener owns:
  - `needs-verification` — applied to closed-merged-untested beads.
  - `placement:escalated` — applied when a decision has been opened
    asking the operator to disposition the bead. (No `placement:unroutable`
    — beads do not get permanently buried.)
- ❌ Does **not** spawn crew for implementation, only for scoping
  (`crew-lite`, prompt = "scope this bead: title, AC, suggested parent
  epic, suggested labels, then `gb stop`").
- ❌ Does **not** cycle silently when the queue grows. Wrap-up must
  surface the regression.

## Gardener roster → lane map (live, not static)

Built each cycle from `kd list --type=schedule -f enabled=true`. The table
below is illustrative of the current 7-schedule fleet and is **not** the
source of truth — the gardener rebuilds it from live data every cycle.

| Schedule bead | Lane | Routing hint (parsed from prompt) |
|---|---|---|
| ka-0000003931 ephemeral-impl gardener | epic-mention looping gardeners | parent already set by @gardenio mention; placement does not touch |
| ka-0000007431 per-user gardener | beads-viewer + gardenio user-facing identity | path `beads3d/`, label hints from ka-0000003603 epic |
| ka-0000006058 gardener-release-owner | golden deploys + release-stuck | tag/release/deploy beads, `release-stuck` |
| ka-0000001612 gardener-cycle-watchdog | failed gardener cycles | meta-watchdog; placement does not feed |
| ka-0000005035 E2E gardener | un-quarantine flaky e2e modules | label `e2e`, `flaky`, `quarantined` |
| ka-0000001052 coverage gardener | DEVOPS-312 coverage | epic DEVOPS-312, label `coverage` |
| ka-0000004194 [E2E fixture] | test fixture, not a real lane | skip |

Realistically, **most beads in the unparented queue today do not match any
of these 7 lanes** — they're new research tasks, plans, follow-ups to
recently-merged MRs, etc. Gardener-placement must therefore be willing to:

- Use **semantic matching** (`kd similar <bead>`) to find the gardener whose
  recent epics/comments are nearest neighbors — if ≥3 of the top-10
  neighbors live under a single gardener's epic, set parent. Cosine
  threshold: 0.35.
- For beads with no plausible gardener home, **spawn a scoping crew**
  rather than skip. The scoping crew may itself set parent or open an
  epic if appropriate.
- For beads that resist three rounds of placement attempts, **escalate**
  via `gb decision create` — never bury.

## Routing rules (cycle algorithm)

Each cycle:

1. **Snapshot the unparented, unassigned open queue:**

       kd list --status=open --unassigned --no-parent --limit 500 --json   # the snapshot
       kd count --status=open --unassigned --no-parent                     # unparented_start (KPI)

   Both `--unassigned` (assignee IS NULL OR '') and `--no-parent` (no
   parent-child parent) ship on `kd list`/`kd count` and push both
   predicates server-side, so the snapshot and the count agree exactly.
   `kd count` needs no `--limit` and no jq, so the KPI can never truncate
   or drift from its `unparented` label — do NOT hand-roll a jq filter on
   `assignee` alone (that measures the wrong, larger set) or call
   `POST /v1/bead_query` directly.

2. **Build the live gardener → lane map** from
   `kd list --type=schedule -f enabled=true`. Parse each schedule's
   `prompt` for label/path/epic hints. Skip gardeners whose prompt cannot
   be parsed for any hint — but record the count, this is a useful signal
   that hint-parseability is a problem worth fixing.

3. **Walk the queue.** **No per-cycle cap on routing.** The cap is
   wall-clock — the gardener budgets 8 minutes total and processes beads
   in priority then age order until time is up. Aim for ≥10 beads moved
   per cycle while the queue is non-empty. For each bead:

   a. **Hard match** — bead has a label exactly matching one gardener's
      `label_hints`, AND that gardener owns exactly one matching epic →
      `kd update <bead> --parent <epic-id>` + routing comment.

   b. **Soft match** — keyword/path/title match to one gardener →
      `kd update <bead> --parent <epic-id>` (set parent, not just
      comment). Cheaper to be wrong than to skip; lane gardeners can
      punt back. Only fall back to comment-only if the gardener has no
      epic and the bead is therefore unparentable.

   c. **Semantic match** — `kd similar <bead> --limit=10`. If ≥3 of the
      top-10 nearest neighbors live under a single gardener's epic and
      the gardener's lane is plausible, set parent + routing comment
      (note "matched semantically").

   d. **No match, bead is unscoped** (no AC, vague title, < 200 chars
      description) → spawn `crew-lite` to scope, with prompt: "scope
      this bead, suggest a parent epic, then `gb stop`." Add label
      `placement:scoping`. Crew is its own bead; closes when scoped.

   e. **No match, bead is scoped** → on first cycle, add a routing
      comment "gardener-placement: no gardener owns this lane today;
      surfacing in next cycle." On second cycle with no neighbor change,
      `gb decision create` asking the operator: route to / spawn lane
      / close as stale. Tag `placement:escalated` so we don't re-decide
      on the same bead.

4. **Untested-work scan (action, not just report):**

   For closed beads merged in the last 7 days with no comment matching
   `verified|tested|smoke|dashboard|e2e|validated`:

   - Add label `needs-verification`.
   - Add a routing comment naming the lane gardener
     (`gardener-placement: closed without a verification artifact;
     <lane-gardener> please verify or write a verification follow-up.`).
   - Surface the top-10 in the wrap-up squawk for operator visibility
     too.

   Top-10 cap is on the squawk, not on labelling — every untested bead
   gets the label. This is what turns a report into routing.

5. **Wrap-up KPI:** `gb stop --wrapup` reports:

   - `unparented_start`, `unparented_end`, `delta`, `routed_hard`,
     `routed_soft`, `routed_semantic`, `scoping_spawned`, `escalated`.
   - `untested_labelled_count` (every untested bead labelled this cycle).
   - `failed_to_parse_gardener_prompts` (count of gardeners skipped because
     their prompt yielded no hints — operator should look at these).
   - If `delta >= 0` AND `escalated == 0`, the cycle is FAILED — wrap-up
     accomplishments line MUST start with "FAILED:".

## Cadence

`*/30 * * * *` (every 30 min). Off-phase from
gardener-cycle-watchdog (which runs every 30m at :00/:30 — placement
runs at :15/:45 to avoid the watchdog catching its own cycles mid-flight).
The 30m cadence is intentionally tighter than v1's 45m: the queue is
the bottleneck, faster cycles == faster drain.

**Initial enabled=false** — operator flips on after reviewing first
dry-run output (one cycle invoked manually via `gb spawn`).

## Role

`role=crew-lite` is sufficient. The gardener reads beads, parses prompts,
sets parent, adds comments, posts a squawk, spawns *only* `crew-lite`
scoping crew (≤3 per cycle to keep cluster pressure bounded), and opens
decisions. No code, no MRs, no kubectl, no implementation crew.

## Risks & mitigations

| Risk | Mitigation |
|---|---|
| Wrong-epic parentage (beads land in wrong gardener's queue) | Soft and semantic matches set parent eagerly, but lane gardeners can punt with one `kd update --parent ""` — recovery is one command. The expensive failure is the queue staying full, not transient mis-routings. |
| Heuristics drift as gardeners evolve | Routing map is rebuilt every cycle from live schedule prompts — no static table to bitrot |
| Scoping-crew runaway (cluster pressure) | Cap of 3 scoping crew per cycle. Crew is `crew-lite`, scope-only prompt, hard 5-minute wallclock |
| Gardener cycles silently when queue grows | Wrap-up MUST report `delta` and escalation count; `delta >= 0 && escalated == 0` flips wrap-up to "FAILED:" prefix; cycle-watchdog will then file a bug |
| Untested-work label spam | `needs-verification` is added at most once per bead; the gardener checks for the label before applying. Stale label is operator-surfaced, not auto-removed |
| Race with whatever consolidated successor to intake/delivery emerges | Placement only touches `--no-parent` beads; once a parent is set, placement is hands-off forever on that bead. Successor gardeners pull from their own queues, no contention |

## Decision points (operator gates)

Before enabling, the operator should weigh in on:

1. **Crew-spawn budget.** Plan currently allows ≤3 `crew-lite` scoping
   spawns per cycle. Should this be 0 (no spawning, mark unscoped beads
   for a human), 3 (default), or higher?
2. **Semantic-match cosine threshold.** Default 0.35. Operator may want
   to start at 0.5 (stricter) and loosen.
3. **Decision-escalation cadence.** Plan escalates after 2 unsuccessful
   cycles per bead. Operator may want 3 (more patience) or 1 (faster
   surfacing).
4. **Lane successor discoverability.** With intake/delivery/release
   disabled, several beads will likely escalate every cycle. Is the
   right answer to (a) tolerate this until a consolidated successor is
   built, (b) auto-close stale unrouteable beads after 14 days, or (c)
   spawn a one-off "tidy unrouteable queue" run before enabling
   placement?

## Why this is safer than v1, despite being more aggressive

v1 was conservative by construction (cap=8, comment-only soft matches,
report-only untested scan, 3-strike permanent burial). It would have
*reduced* the queue at most ~8 beads/cycle on hard matches alone — and
in practice, near zero, since most current unparented beads have no
hard label match. The v1 gardener would have produced bookkeeping
without throughput, the exact failure mode ka-0000005320 called out.

v2 produces real movement per cycle: parent set on hard, soft, or
semantic match; scoping crew spawned for unscoped; escalation for
truly stuck. The blast radius is bounded by:

- All actions are reversible (parent is one `kd update` away, label
  is one `kd label rm` away, comment is informational).
- No write surface beyond beads (no MRs, no kubectl, no other
  gardeners, no code).
- Per-cycle wrap-up names every action so the operator can audit.
- Cycle-watchdog observes FAILED-prefixed wrap-ups and surfaces them.

Reversible aggression > irreversible conservatism when the failure
mode under conservatism is the queue getting worse.

## Deliverables

1. This plan (`gardenio/plans/gardener-placement.md`).
2. Prompt file (`gardenio/plans/gardener-prompts/placement.md`).
3. Schedule bead created with `enabled=false`, prompt synced from file.
4. MR for review (this MR, !1796, will be the merge).
5. Operator decision on the 4 questions above.
6. Operator flips `enabled=true` after a manual dry-run cycle.
