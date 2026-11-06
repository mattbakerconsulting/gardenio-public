# Closed-corpus histogram — Matt-operated beads (read-only audit)

**Bead:** ka-0000001651 (parent: ka-0000005305)
**Source heuristics:** `gardenio/docs/research/legacy-operated-by-matt-cleanup.md` §2 / §6
**Date:** 2026-05-13
**Corpus snapshot:** `kd list --operated-by-alias slack:U0OPERATOR1 --status closed --all-projects --all-types --limit 5000 --json` against gardenio-prod
**Status:** read-only audit — **no writes to `beads.operated_by_id`**.

## TL;DR

The closed Matt-operated corpus is **1825 beads** (matches §1 of the report's
"~1806" rounded estimate, with a small drift from new closures since the report
was written). Categorisation per §2 of `legacy-operated-by-matt-cleanup.md`:

| Category | Count | Distinct rebind targets |
|----------|-------|--------------------------|
| **A** — slack-thread non-Matt   |     0 | 0 |
| **B** — cron / schedule author  |    22 | 0 resolved (22 unresolved — need schedule parent-walk) |
| **C** — agent-spawned children  |   311 | n/a (parent-walk) |
| **D** — keep — genuinely Matt   | 1 492 | n/a |
| Other (unclassified)            |     0 | n/a |
| **Total**                       | **1 825** | |

**Key finding.** The closed corpus is dominated by category **D (1 492 / 1 825 = 81.8 %)** —
genuinely Matt's own work. Category A is empty in the closed corpus: every
thread-* agent's `slack_user_id` resolves to Matt (`U0OPERATOR1`), and every
static-spawned agent with a populated `slack_user_id` is also Matt. This is the
opposite shape from the open-corpus §1.1 breakdown (where A is non-trivial),
because in the open set the few non-Matt-thread spawns linger but their thread
agents have since been closed and their beads are the long-tail of `closed_at` —
their child work isn't in the Matt-operated bucket in the first place.

**Implication.** If a future operator decision (§2.E) widens scope to rewrite
closed-state attribution, the actual cleanup yield is small: at most **22 + 311
= 333** rows (B + C) would change operator, and most of those would cascade to
Matt anyway once category C resolves via parent-walk (most of those parents are
themselves Matt-operated gardeners). The cost-benefit of a closed-corpus rewrite
remains low; the migration banner (ka-0000004244) + V-R2 chip remains the
right primary UX for archived browsing.

## 1. Corpus shape

**Total**: 1 825 beads.

By kind:

| Kind | Count |
|---|---|
| `data`   | 982 |
| `issue`  | 432 |
| `config` | 411 |

By type (top 8):

| Type | Count |
|---|---|
| `message`       | 418 |
| `session`       | 402 |
| `agent`         | 396 |
| `task`          | 368 |
| `decision`      | 121 |
| `bug`           |  37 |
| `report`        |  25 |
| `epic`          |  25 |

By `created_by` bucket:

| Bucket | Count |
|---|---|
| `thread-*` (Slack-thread agents) | 1 137 |
| `static` (scheduler-spawned)     |   374 |
| `gardenio-*` (agent slug)         |   121 |
| `monorepo-*` (agent slug)        |   114 |
| `sched-*` (gardener agent slug)   |    59 |
| `mr-*` (MR-bound agent slug)     |    20 |
| (other / unmatched)              |     0 |

## 2. Categorization

Each closed Matt-operated bead is classified using the deterministic rules from
§2 of `legacy-operated-by-matt-cleanup.md`. The categoriser is ordered so the
strongest signal wins (e.g. `type=schedule` is checked before `created_by`
pattern, since the schedule bead's identity matters more than how it was filed).

### Category A — slack-thread non-Matt (rebind to triggerer)

- **Targets.** `created_by` starts with `thread-*`, and the thread agent's
  `Fields["slack_user_id"]` resolves to a Slack user other than Matt.
  Alternative path: `created_by = "static"` for a re-resumed thread agent
  whose `Fields["slack_user_id"]` is non-Matt.
- **Count: 0.** Every `thread-*` agent slug present in the corpus resolves to
  `slack_user_id = U0OPERATOR1`. Every `static`-spawned agent with a populated
  `slack_user_id` resolves to Matt. There are **zero rebind-A candidates** in
  the closed Matt-operated corpus.
- **Distinct rebind targets: 0.**

> **Why A is empty here while §1.1 of the report shows A ≈ 50 in the open
> corpus.** The open corpus has 78 `static` rows with unresolved-schedule-author
> and ~50 rebind-A candidates; in the closed corpus, every thread agent that
> was ever Matt-operated has its `slack_user_id` literally equal to Matt's UID.
> Non-Matt thread agents would have been bound to *their* user from the outset
> and therefore would not appear in this operator-filtered slice. The reverse
> is also true: any current open-A rows will close into the closed-A bucket
> only if (a) they were Matt-bound at creation and (b) the future backfill
> migrates them in flight.

### Category B — cron / schedule author (rebind to schedule's author)

- **Targets.** `created_by = "static"` for `task`, `gate`, or `human_survey`
  beads (and rare `agent` beads with no `slack_user_id`), where the schedule
  bead's `operated_by_id` is the rebind target.
- **Count: 22.**
  - `static-task-unresolved`: 12
  - `static-gate-unresolved`: 8
  - `static-agent-unresolved`: 2
- **Distinct rebind targets: 0 (resolved); 22 unresolved.**

This read-only audit deliberately does **not** perform the schedule-parent walk
required to resolve each unresolved row to its schedule author. The walk is the
job of the real `kd admin backfill-operated-by-legacy` command (P1 follow-up
per §6). The §2.B caveat notes most gardeners are Matt-authored, so this
category would mostly resolve back to Matt (i.e. net D) in a real backfill.

In the closed corpus the unresolved set is only **22 rows** total — small
enough that even per-row parent-walk would be cheap if the operator decides
to widen scope.

### Category C — agent-spawned children (parent-walk)

- **Targets.** `created_by` matches an agent slug pattern (`sched-*`,
  `gardenio-*`, `monorepo-*`, `kbeads-*`, `mr-*`). Rebind target = parent's
  `operated_by_id`.
- **Count: 311.**

Breakdown by agent-prefix:

| Prefix | Count |
|---|---|
| `gardenio-*` | 120 |
| `monorepo-*`| 113 |
| `sched-*`   |  58 |
| `mr-*`      |  20 |

Distinct rebind targets: **n/a** — resolution is iterative parent-walk (§2.C),
not a fixed enumeration. In practice the parents are themselves overwhelmingly
Matt-operated gardeners, so the cascade in this slice would net most of these
rows back to Matt as well; the residual "actually-not-Matt" yield is bounded
above by the number of non-Matt-authored gardeners in the system (currently
near zero per §2.B).

### Category D — keep — genuinely Matt

- **Count: 1 492.**

Breakdown by signal:

| Sub-path | Count |
|---|---|
| `thread-matt` (thread-* agent's `slack_user_id == U0OPERATOR1`) | 1 125 |
| `static-agent-matt` (re-resumed thread agent, slack_user_id == Matt) | 345 |
| `schedule` (type=schedule, Matt-authored) | 15 |
| `survey-rater-matt` (human_survey rater = Matt) | 7 |
| (n.b. zero `Matthew.Baker` direct-CLI hits in the closed corpus — that bucket exists open-side but has closed-tail zero) | 0 |

These rows stay attributed to Matt under any scope of backfill. They are
**81.8 %** of the closed corpus.

### Other / unclassified

- **Count: 0.** Every `created_by` value matches one of the prefix patterns;
  no `jira-*`, `gitlab-*`, `scheduler:*`, or other unanticipated source
  appears in the Matt-operated closed set.

## 3. Rebind-target detail

### A targets (would be applied by a backfill if scope widened)

None.

### B targets (would be applied by a backfill if scope widened)

None resolved from this audit. 22 rows would need per-row parent-walk during
a real apply.

### C targets

Not enumerated; resolution is via the same iterative parent-walk used by the
open-corpus backfill (§2.C), capped at `--max-iterations=5`.

## 4. Methodology

1. Dump the corpus once:
   ```
   kd list --operated-by-alias slack:U0OPERATOR1 \
           --status closed --all-projects --all-types \
           --limit 5000 --json > closed-matt-corpus.json
   ```
2. Build an in-corpus index of `agent` beads keyed by `title` (= agent slug),
   recording each agent's `Fields["slack_user_id"]`.
3. For each bead, apply this ordered rule chain (first match wins):
   1. `type == "schedule"` → **D** (`schedule`)
   2. `created_by == "Matthew.Baker"` → **D** (`matt-cli`)
   3. `created_by` starts with `thread-` → look up the agent's
      `slack_user_id`. If non-empty and ≠ Matt → **A** with that user as
      target. Else → **D** (`thread-matt`).
   4. `created_by == "static"`:
       - `type == "agent"`:
           - `slack_user_id ≠ Matt` (non-empty) → **A**
           - `slack_user_id == Matt` → **D**
           - missing → **B** (unresolved, requires schedule walk)
       - `type == "human_survey"`:
           - `rater_slack_user ≠ Matt` (non-empty) → **A**
           - `rater_slack_user == Matt` → **D**
           - missing → **B** (unresolved)
       - other types (task, gate) → **B** (unresolved)
   5. `created_by` matches `^(sched|gardenio|monorepo|kbeads|mr)-` → **C**
   6. fallback → `OTHER`
4. Aggregate counts and distinct rebind-target sets.

Source script and full per-row classification are attached to the bundle
(`categorize.py`, `categorized-rows.json`, `summary.json`).

## 5. Caveats & limitations

- **Closed-corpus only.** Open beads are out of scope for this audit; see
  ka-0000001880 / ka-0000004658 for the open-corpus backfill plan.
- **No schedule-author resolution.** Category B is reported as `unresolved`
  rather than walked to the source schedule's `operated_by_id`. The 22
  unresolved rows would each be a single SQL JOIN in a real apply.
- **Static-agent `slack_user_id` mostly Matt.** Of 396 `agent`-type beads,
  346 carry `slack_user_id = U0OPERATOR1` and 50 have no `slack_user_id`
  field. The 50 no-slack-uid agents are downstream agent-spawned slugs
  (e.g. `monorepo-e2e-rebase-and-j9u`) whose creator is itself an agent
  slug; they are classified by §2.C path, not the static-agent path.
- **Drift since report.** Report §1 says 1 806 closed-Matt beads; live count
  at audit time is 1 825 (≈19 net adds since 2026-05-13 17:55 UTC). The
  drift does not change the qualitative picture.

## 6. Recommendation

This is a read-only audit; the decision-to-widen sits with the operator. The
data argues:

- **Default: do not widen.** Closed corpus is 81.8 % genuinely-Matt; a rewrite
  would change ≤ 333 rows in the best case, and most of those would still net
  back to Matt after parent-walk. The migration banner + V-R2 chip already
  solve the archived-browsing UX. Cost (audit-trail invalidation,
  `mutation_history` churn, operator-chip drift on closed UI surfaces) >
  benefit.
- **If specific archived workflows need attribution** (e.g. spend reporting
  per `timeseries-spend-attribution.md`, or a departing-engineer cleanup),
  file targeted per-row beads; the same `kd admin
  backfill-operated-by-legacy --target=...` tool from §3 of the report can
  operate row-by-row.

No follow-up bead is filed; this audit closes with the histogram as the
deliverable.

## 7. Artifacts

- `README.md` — this report (markdown).
- `summary.json` — machine-readable per-category counts and breakdowns.
- `categorize.py` — the categorisation script (read-only; reads a `kd list
  --json` dump, writes JSON+markdown).
- `categorized-rows.json` — per-row classification for all 1 825 beads.

## 8. Cross-links

- Source heuristics: `gardenio/docs/research/legacy-operated-by-matt-cleanup.md`
  §2 (categorisation), §6 (this audit task as P3 follow-up), §2.E
  (closed-as-stale non-goal).
- Parent epic: ka-0000005305.
- Open-corpus backfill plan: ka-0000001880 (command implementation),
  ka-0000004658 (apply run).
- Migration banner (UX escape hatch for archived browsing): ka-0000004244.
- Sibling owner backfill (must run after open-corpus backfill): ka-0000007029.
