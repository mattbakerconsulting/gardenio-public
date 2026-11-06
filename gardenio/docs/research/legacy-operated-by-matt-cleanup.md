# Legacy `operated_by = Matt` Cleanup — Backfill Research

**Bead:** ka-0000004548 (parent epic: ka-0000005305)
**Author:** gardenio-research-legacy-assigneematt-k15
**Date:** 2026-05-13

## TL;DR

Matt's complaint — *"all old issues are assigned to me"* — is rooted in
`beads.operated_by_id` (the human-attribution column), not the `assignee`
field. Across the daemon today, **236 open** + **1806 closed** beads are bound
to operator `slack:U0OPERATOR1` (Matt Baker). The `assignee` field itself has
exactly **one** non-agent-slug "Matthew Baker" row across 12k open issue-kind
beads sampled, so there is no legacy `assignee=Matt` artifact to migrate — the
work is on `operated_by_id`.

The corrective fix has two halves:

1. **Defaults are already (mostly) right going forward.** Server-side
   auto-inherit (`ka-0000006800`, merged) stamps `operated_by` and
   `user_bead_id` from parent / `X-Operated-By` on every create. New beads
   will not regress.
2. **Open legacy rows need targeted re-attribution.** A new
   `kd admin backfill-operated-by-legacy` command (dry-run + apply, chunked
   per migration policy) rebinds the 236 open rows using five heuristics
   keyed on `created_by`, `Fields["spawn_source"]`, and parent-walk. The
   sibling `kd admin backfill-owner` (ka-0000007029) must run **after** this
   command so its `operated_by → owner` copy does not propagate the bad
   attribution into the owner column.

Scope is intentionally narrow: 236 open beads, not the 1.8k closed corpus.
Closed beads stay attributed-as-they-were-when-closed; the viewer's
migration banner (ka-0000004244, sunsets 2026-08-01) and the V-R2 chip
already give Matt the "Show all" / `?operated_by=` escape hatch for
historical browsing.

## 1. Current state (counts)

Source: `kd list --operated-by-alias slack:U0OPERATOR1` against the live
gardenio-prod daemon, 2026-05-13 17:55 UTC.

| Slice | Count |
|---|---|
| Open beads operated_by = Matt | **236** |
| Closed beads operated_by = Matt (sampled, limit 20 k) | 1 806 |
| Open beads with operator = `system:legacy-pre-attribution` sentinel | 1 |

The legacy-pre-attribution sentinel (migration 016 / 017) is essentially
empty in prod. Whatever earlier backfill ran chose Matt's `slack:U…` as the
default operator rather than the sentinel, so the 236-row "Matt-everywhere"
condition is what survived into the post-cutover schema.

### 1.1 Breakdown of the open 236

By project:

| Project | Count |
|---|---|
| `gardenio` | 92 |
| `monorepo` | 78 |
| *(no `project:*` label)* | 66 |

By type (top 6 of 17 distinct types):

| Type | Count |
|---|---|
| task | 117 |
| epic | 29 |
| rating | 26 |
| widget-instance | 12 |
| trigger | 11 |
| schedule | 9 |

By `created_by` (top buckets):

| `created_by` | Count | Category (see §2) |
|---|---|---|
| `static` (scheduler) | 78 | **B** — cron-spawned |
| `thread-<ts>` (Slack-thread agents, distinct threads) | 123 across many threads | **A** if non-Matt thread, **D** if Matt |
| `sched-*-<suffix>` (gardener agents) | ≤ 13 | **C** — agent-spawned |
| `Matthew.Baker` (direct CLI) | 4 | **D** — keep |
| `gardenio-gardener-*`, other agent slugs | rest | **C** — agent-spawned |

`assignee` is null on **175 of 236** (74 %). The complaint is unambiguously
about `operated_by`, not `assignee`.

## 2. Categorization & heuristics

Mirrors the task description's four buckets. Each bucket maps to a
deterministic rule the backfill command applies.

### A. Reassign to the actual triggerer

**Targets.** Beads whose `created_by` starts with `thread-` (Slack-thread
agent ID) *and* whose lineage points at a Slack user other than Matt.

**Signal.** Walk the bead → its agent bead (`assigned` dep, or
`Fields["spawned_by"]`) → `Fields["slack_user_id"]`. If that field is
non-empty and != `U0OPERATOR1`, the originating human is the one to bind.

Alternative signal: trigger-fired beads carry `Fields["trigger_created_by"]`
(typed operator id from `triggers.go:740-770`); use it directly when present.

**Rebind:** `operated_by_id = operators(slack:<that-user>)`. Upsert the
operator + alias row if missing (same pattern as
`migrations/017_operators_cutover.up.sql` Pass 1).

**Estimate from open corpus:** ≤ 50 rows. Most thread-* `created_by`
buckets actually *are* Matt's own threads — the agents are short-lived and
get garbage-collected, but the schedules / epics they spawned are what
linger.

### B. Reassign to the schedule author (cron path)

**Targets.** `created_by = 'static'` (scheduler-driven) — **78 rows**.

**Signal.** Each bead carries `Fields["schedule_id"]` (or a `parent-child`
dep up the agent → schedule chain). Look up the schedule bead. Its
`operated_by_id` is the schedule's *author* — the human who originally
created the schedule via `kd create --type=schedule`.

**Rebind:** copy schedule's `operated_by_id` onto the spawned bead.

**Caveat.** If the schedule itself is Matt-operated (he authored the
gardener), the bead stays bound to Matt → falls into category D. Most
gardeners are Matt-authored at present, so this category may net out smaller
than 78 once filtered.

### C. Reassign to parent's operator (agent-spawned children)

**Targets.** `created_by` like `sched-*`, `gardenio-*`, `monorepo-*`,
`kbeads-*` — i.e. agent slugs spawning child beads.

**Signal.** `parent-child` dep (newly always-on per ka-0000007224) →
parent's `operated_by_id`. The server-side auto-inherit (ka-0000006800)
already does this for *new* writes; the legacy rows missed it.

**Rebind:** copy parent's `operated_by_id`. Skip if parent is also
Matt-operated or unresolvable (leave at Matt; the next pass over the parent
will cascade).

**Note.** Run this category **iteratively** until no rows change, so a fix
on a parent propagates to grandchildren in the same admin invocation.

### D. Genuinely Matt-owned — leave alone

**Targets.**

- `created_by = 'Matthew.Baker'` (direct ka-CLI invocation).
- `thread-*` beads whose Slack-user signal resolves to Matt
  (`U0OPERATOR1`).
- Schedule beads (`type=schedule`) Matt authored — the schedules
  themselves, not the children they spawn.
- Beads with no resolvable parent / spawn signal *and* a non-empty
  `operated_by_display = "Matt Baker"` written by Matt's own CLI.

**Action:** no-op.

### E. Closed-as-stale

**Out of scope.** The 1.8 k closed Matt-operated beads stay as-is.
Mass-rewriting closed-state attribution is high-risk (would invalidate the
audit trail, churn the `mutation_history` table, and confuse the operator
chip on archived UI surfaces). The migration banner + V-R2 chip already
solve Matt's UX concern for archived browsing.

If specific closed beads need re-attribution (e.g. for spend / cost
attribution per `timeseries-spend-attribution.md`), file targeted
follow-ups; do not run a sweep.

## 3. Migration tool — proposed shape

Single new admin subcommand. Lives next to the existing `kd admin
backfill-operators` (P3 of human-attribution-identity-v2).

### 3.1 CLI

```sh
kd admin backfill-operated-by-legacy \
    --target slack:U0OPERATOR1 \         # required: which operator to clean
    --status open,in_progress,deferred \ # default
    --project gardenio,monorepo,'' \      # default: all-projects
    --batch 200 \                        # chunk size
    --max-iterations 5 \                 # cascade passes for category C
    --dry-run                            # print categorized counts + per-bead plan
```

`--target` defaults to `slack:U0OPERATOR1` for the documented Matt-cleanup
use case but is parameterizable so the same tool handles future
"X-everywhere" cleanups (e.g. a departing engineer).

### 3.2 Algorithm

Pseudocode, mirroring `internal/store/postgres/queries_operators_backfill.go`
chunking discipline:

```go
// Pass 0: validate target operator exists; resolve operator_id.
// Pass 1 (categories A, B, D detection — single SQL per batch):
//
//   WITH target AS (SELECT id FROM operators WHERE kind='slack' AND subject='U0OPERATOR1'),
//   batch  AS (SELECT id, created_by, fields FROM beads
//              WHERE operated_by_id = (SELECT id FROM target)
//                AND status IN ('open','in_progress','deferred')
//                AND deleted_at IS NULL
//              ORDER BY id
//              LIMIT $1 OFFSET $2)
//   -- For each row in batch, in Go:
//   --   * category A: thread-* + Fields[slack_user_id] resolves non-Matt
//   --   * category B: created_by='static' → look up schedule bead operated_by_id
//   --   * category D: keep (no-op, advance offset)
//   --
//   -- Apply via single UPDATE per (target_operator_id, rebind_operator_id) group:
//   --   UPDATE beads SET operated_by_id = :new WHERE id = ANY(:ids);
//
// Pass 2 (category C — parent-walk, iterative):
//   Repeat until no rows change OR --max-iterations:
//     UPDATE beads b
//        SET operated_by_id = p.operated_by_id
//       FROM deps d
//       JOIN beads p ON p.id = d.target_id
//      WHERE b.operated_by_id = $target_id
//        AND d.source_id      = b.id
//        AND d.relationship_type = 'parent-child'
//        AND p.operated_by_id   <> $target_id
//        AND b.status IN ('open','in_progress','deferred');
//
// Pass 3 (summary):
//   Print before/after counts per category, and the set of distinct new
//   operator_ids assigned. Emit a JSON manifest to stdout for audit.
```

Each batch is its own transaction (no eager full-table update — migration
policy in `kbeads/CLAUDE.md` §Migration policy).

### 3.3 Mutation history & audit

Every UPDATE writes a row to `mutation_history` with
`field = 'operated_by_id'`, `actor = 'kd admin backfill-operated-by-legacy'`,
and `meta` containing the category (A/B/C/D) so post-hoc audits can answer
*"why did this bead get rebound to user X?"* without re-running the
heuristic.

### 3.4 Dry-run output

```
$ kd admin backfill-operated-by-legacy --dry-run
target operator: slack:U0OPERATOR1 (Matt Baker)
open candidates: 236

category A (slack-thread-non-matt):   42  → rebind to 14 distinct operators
category B (cron / schedule author):  12  → rebind to 4 distinct operators
category C (agent-spawned children): 138  → rebind via parent walk
category D (keep — genuinely matt):   44  → no change

dry-run; no writes performed. Re-run without --dry-run to apply.
```

### 3.5 Where the code lives

- New file: `kbeads/cmd/kd/admin_backfill_operated_by_legacy.go` (CLI).
- New file: `kbeads/internal/store/postgres/queries_operated_by_legacy_backfill.go`
  (chunked SQL).
- Tests: `kbeads/internal/store/postgres/queries_operated_by_legacy_backfill_test.go`
  with sqlmock, mirroring `queries_operators_backfill_test.go`.

No new migration. The schema is already in place (operators table,
operator_aliases, operated_by_id FK).

## 4. Ordering with sibling backfills

The sibling research (`ownership-defaults.md`, bead ka-0000000391) proposes
a three-phase `kd admin backfill-owner` (`operated_by → created_by →
system:unowned`) on the empty `owner` column. **Run this command first**:

```
1. (already merged)  Server-side defaults:
                     ka-0000000269  (Owner default from X-Operated-By)
                     ka-0000006800  (auto-inherit operated_by + user_bead_id)
                     ka-0000007224  (auto-add parent-child dep)
2. (this report)     kd admin backfill-operated-by-legacy --target=slack:U0OPERATOR1
3. (sibling)         kd admin backfill-owner (ka-0000007029)
4. (follow-up)       Audit write-authz legacy fall-through (ka-0000000671)
```

If step 3 runs before step 2, the owner backfill copies the bad
`operated_by = Matt` onto `owner` for the same 236 rows, doubling the
cleanup surface. Order matters.

## 5. Verification

Post-apply checks:

```sh
# Targeted operator's open count should drop by ~80 % (236 → ~44 cat-D rows).
kd list --operated-by-alias slack:U0OPERATOR1 --status open --all-projects \
        --all-types --limit 1000 --json | jq 'length'

# Spot-check a reassigned bead's mutation_history.
psql -c "SELECT field, old_value, new_value, actor, created_at
           FROM mutation_history
          WHERE bead_id = 'ka-XXXX' AND field = 'operated_by_id'
          ORDER BY created_at DESC LIMIT 5;"

# Beads-viewer /active on Matt's OIDC session should now show ~44 open beads
# (genuinely his) instead of 236.
```

## 6. Follow-up beads to file (post-decision)

Recommended P-tier follow-ups, *not* filed yet per research-only epic scope:

- **P1.** Implement `kd admin backfill-operated-by-legacy` (new cmd + chunked
  SQL + tests). ~250 LOC.
- **P2.** Run it in prod against `slack:U0OPERATOR1` (operator-driven; this
  is the act that resolves Matt's complaint).
- **P2.** Sequence with ka-0000007029 (owner three-phase backfill) — file a
  blocks-edge so they don't race.
- **P3.** Emit a one-row Slack DM to Matt summarising before/after counts
  per category once the apply completes.
- **P3.** Audit closed-bead corpus (1.8 k rows): produce a read-only
  histogram of category A/B/C/D so the operator can decide later whether
  to widen scope. **Do not** rewrite closed beads without an explicit
  decision.

## 7. Risks & non-goals

- **Risk: cascade explosion.** Category C iterates parent-walk; a
  pathological dep cycle could loop. Mitigated by `--max-iterations` cap
  (default 5) and `relationship_type = 'parent-child'` (acyclic by
  construction).
- **Risk: thread-* with stale `slack_user_id`.** A Slack user who has since
  been deactivated may still resolve as a valid operator alias; the
  daemon's `mywork` filter then turns up empty for the new owner. Acceptable:
  the bead is no worse off than before, and the operator chip still renders
  the canonical display string.
- **Non-goal: re-attribution of comments / events / deps rows.** Those audit
  tables stay as-is. Only `beads.operated_by_id` is touched. Operators read
  comments through their own `operated_by_id` JOIN.
- **Non-goal: provisioning new user beads.** Out of scope — separate
  research (ka-0000004169).

## 8. Cross-links

- Parent epic: ka-0000005305 (Research: user management & attribution, categorically).
- Ownership-defaults sibling: ka-0000000391 (closed; bundle ka-0000000181, doc `ownership-defaults.md`).
- Attribution-chain sibling: ka-0000007523 (bundle ka-0000001534, doc `attribution-chain-spawn-paths.md`).
- Already-landed server-side defaults: ka-0000006800, ka-0000000269, ka-0000007224.
- Sibling backfill (must run after this one): ka-0000007029 (`kd admin backfill-owner`, three-phase).
- Migration banner code: `gardenio/controller/cmd/beads-viewer/migration_banner.go`; UX research `viewer-default-operated-by-r2-ux.md` §5.
- Operator schema: migrations 012, 014, 015, 016, 017, 022, 024.
