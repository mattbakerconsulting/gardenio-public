# Users / People page v2 — data-layer research

**Operator:** Matt Baker (slack:U0OPERATOR1 / oidc:matthew.baker@acme.example)
**Origin:** Slack thread 1780578472.384209 — "the users page still sucks and doesn't
show user stats or disambiguate between user aliases or emails (look at prod db, think
abt cleanup and further data bugs as well as the users ergonomics)."
**Author:** thread-1780578472-384209 · 2026-06-04
**Prod source:** RDS `america-e2e…/beads` (operators, operator_aliases, beads)

## TL;DR

The `/people` directory got a render-time dedup + top-stats pass (epic ka-0000005027)
but it "still sucks" because the rot is in the **data layer**, not the template:

- **Render-time dedup can't fire for most rows.** `dedupOperators` keys on a normalized
  email or display name. In prod **52 of 77** operator rows carry a synthetic
  `…@noreply.kbeads.local` sentinel email and **45 of 77** have an empty `display`. Rows
  with no usable key never group → real duplicate humans render as separate rows anyway.
- **Per-operator stats are computed from a 5 000-bead sample** against a fleet of
  **385 809 beads / 245 889 open**. The header "open beads" total and every per-row
  count are arbitrary under-counts, not real numbers.
- **Real duplicate clusters and malformed rows exist in the table** and need a one-shot
  merge + cleanup (the `MergeOperators` / `link-aliases` primitive already exists, it has
  just never been pointed at the real clusters).
- **Ergonomics:** machine identities are intermixed with humans; `op-<id>` slugs dominate;
  aliases/emails are only surfaced when the (rarely-firing) dedup collapses a cluster.

## Prod data snapshot (2026-06-04)

```
operators: 77 total · 77 with slug · 0 forgotten
  by kind:  gitlab 23 · slack 19 · oidc 17 · system 12 · fleet 4 · agent 1 · email 1
  class:    human-ish 55 · machine 22 (system/fleet/agent/*bot*/group_/project_)
  distinct real human emails (@, non-sentinel): 25
beads: 385 809 total · 245 889 open/in_progress
operator_aliases: 103 rows
sentinel @noreply.kbeads.local emails: 52 / 77
empty display: gitlab 23 · system 12 · fleet 4 · oidc 4 · slack 4 · email 1 · agent 1 (45 total)
```

## Confirmed data bugs

### 1. Duplicate human clusters never merged
The same human exists as several operator rows that are **not** linked through
`operator_aliases` to one survivor. Worst offender — **Matt Baker = 5 rows**:

| id  | kind   | subject                      | slug             | email                              |
|-----|--------|------------------------------|------------------|------------------------------------|
| 229 | slack  | U0OPERATOR1                  | op-229           | matthew.baker@acme.example (oidc-linked) |
| 264 | slack  | matt.baker                   | op-264           | sentinel |
| 297 | slack  | Matt Baker                   | op-297           | sentinel |
| 223 | gitlab | matthewbaker                 | op-223           | sentinel |
| 321 | oidc   | matthewcbaker0122@gmail.com  | matthewcbaker0122| matthewcbaker0122@gmail.com (likely personal — confirm before merge) |

Only 229 has its slack↔oidc alias linked (`source=manual`). 264/297/223 are orphan
rows that each render as a distinct "user." The same pattern repeats for most humans:
~18 gitlab handles (al.kent, devin.hale, jackiep, sean.kennedy3, …) and the
sentinel-email slack rows are not linked to their oidc identities.

### 2. Malformed slack operator rows
Two `kind=slack` rows have a **non-U-id subject** — a display name / handle was written
into the Slack subject column by some ingestion path:

```
264 | slack | subject="matt.baker"  | 2 beads attributed
297 | slack | subject="Matt Baker"  | 1 bead attributed
```

Valid Slack subjects are `U…` ids (see `deriveOperatorEmail`). These need (a) cleanup —
merge into Matt's survivor — and (b) a write-path guard so a non-`U` slack subject is
rejected or routed to resolution instead of creating a junk operator.

### 3. `op-<id>` slug ergonomics
52 rows have `op-NNN` slugs. `ensureOperatorSlug` only derives a human-readable slug from
an oidc email; slack/gitlab humans (and the sentinel-email rows) fall back to `op-<id>`,
so the directory and `/u/{slug}` URLs are opaque for most real people.

### 4. Stats sampled, not aggregated
`lookupOperatorStats` pulls the 5 000 most-recently-updated beads (`peopleStatsBeadLimit`)
and groups them in memory. Against 245 889 open beads the per-row "open" counts and the
header "open beads" total are sampling artifacts. The fix is a `GROUP BY operated_by_id`
aggregate in the daemon (count open beads + max(updated_at) + recent-created top-N),
optionally **resolved through `operator_aliases`** so a merged human's count unions all
their identities.

## Ergonomics gaps (the "still sucks" surface)

- **No human/machine separation.** 22 system/fleet/agent/bot rows sit in the same list as
  25 humans with no grouping, filter, or visual distinction.
- **Alias/email disambiguation is invisible** unless render-dedup fires (it mostly
  doesn't). The row should always show every identity (kind + subject/email) folded into
  a human, which is exactly what the complaint asks for.
- **Slug opacity** (above) makes the directory hard to scan and the URLs unmemorable.

## Why the existing gardener didn't catch this

The People-Directory CDQR Gardener (ka-0000001894, epic ka-0000005027) is a **visual**
audit loop: it screenshots the rendered `/people` and scores dimensions. It cannot see
that the underlying operators table has unlinked duplicate rows or that stats are sampled
— those are invisible once the page renders. This epic is the **data-layer** counterpart.

## Existing primitives to reuse (no new infra needed)

- `POST /v1/admin/operators/link-aliases` (`http_admin_operator_aliases_link.go`) — picks a
  survivor (prefers oidc), repoints aliases, and calls `MergeOperators` to move audit-FK
  rows across beads/comments/deps/events/mutation_history in one tx. Idempotent.
- `store.MergeOperators` (`postgres/operators.go`) — the underlying merge.
- `gb identity link-operator` — per-caller oidc↔slack link (already used for Matt's 229).
- `gb identity seed` — seeds user beads from Slack workspace members (source of real
  display/email for sentinel rows).

## Proposed work (see child tasks of the epic)

1. **Data cleanup (one-shot):** merge the confirmed human clusters via `link-aliases`;
   resolve the 2 malformed slack rows. Operator-confirm the personal-gmail row (321)
   before merging.
2. **Write-path guard:** reject/repair non-`U` slack subjects at ingestion.
3. **Real stats:** ✅ **DONE (ka-0000001390).** Daemon `GROUP BY operated_by_id` aggregate
   (alias-aware) replacing the 5 000-bead in-memory sample. `store.OperatorDirectoryStats`
   (`postgres/operators_stats.go`) runs a full-table GROUP BY for open/in_progress count +
   `max(updated_at)` last-active, plus a windowed per-operator recent-chips query (capped
   per operator, with a hard ceiling on total rows). Exposed as
   `GET /v1/operators/stats`; the beads-viewer `lookupOperatorStats` now calls it instead of
   pulling a sample. Because `operated_by_id` is the canonical operator the write path bound
   through `operator_aliases`, every linked identity folds into one row; the viewer's
   render-time group fold still sums any not-yet-merged duplicate identities (work item 1).
4. **Slug backfill:** human-readable slugs for `op-<id>` humans.
5. **Disambiguation UI:** always render every folded identity + email per human row.
6. **Human/machine grouping** on `/people` (separate section or filter).
