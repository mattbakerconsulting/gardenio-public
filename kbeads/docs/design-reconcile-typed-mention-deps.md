# Design: Reconcile-on-Reextract Auto-Delete of Stale Typed-Mention Dep Rows

**Status**: Implemented (guarded, opt-in)
**Bead**: ka-0000006581 (Proposal B)
**Follows**: ka-0000007756 / MR !5201 (`kd reparent` + dep-edge audit — Proposals A & C)
**Author**: gardenio-proposal-b-reconcileonreextract-evt
**See also**: [`research-typed-mention-syntax.md`](research-typed-mention-syntax.md),
[`design-bead-references.md`](design-bead-references.md),
[`operator-reextract-references.md`](operator-reextract-references.md)

## Problem

Typed mentions (`parent: ka-X`, `child-of(ka-X)`) in a bead's text auto-materialize
a `deps` row via `bulkInsertTypedDeps` — but **additively only**. Removing the
mention never removes the dep row. This is the "re-parenting is a fussy multi-step
dance" footgun that motivated the whole epic: an author who deletes `parent: ka-OLD`
from a description still leaves the parent-child edge behind, and the *next*
write (which re-runs the parser, `ON CONFLICT DO NOTHING`) does not clean it up.

`kd reparent` (Proposal A) papers over this by *orchestrating* the removal from the
CLI. Proposal B makes edge removal **automatic**: on the write path,
`replaceBeadReferences` reconciles parser-created dep rows against the current text,
deleting the ones whose typed mention no longer exists in any live source field.

## Why this was deferred (the hazards)

This is behavior-changing and genuinely risky. Four hazards, each captured on the
bead and each addressed by a guardrail below:

1. **Silent edge removal on every write.** Once reconcile is live, any
   description/notes edit can remove a load-bearing edge based purely on text
   state. Gardener `loop_end` child-counts, epic-close cascades, and `gb ready`
   blocking all key off dep edges — an accidental deletion wedges automation.
2. **The `created_by` marker is not proof of parser-only.**
   `bulkInsertTypedDeps` stamps `created_by = 'kbeads/references/typed-mention'`,
   but that records only who created the row **first**. Because both the parser
   path and manual `kd dep add` use `ON CONFLICT (bead_id, depends_on_id, type)
   DO NOTHING`, a manual add *after* a parser row no-ops and **leaves the parser
   marker in place**. A `created_by`-only reconcile could therefore delete an
   edge a human also asserted manually.
3. **Comment-sourced mentions are immutable.** With
   `BEADS_REFERENCES_EXTRACT_COMMENTS=1`, dep rows are also minted from comment
   text (`replaceCommentReferences`). Comments cannot be edited, so a
   comment-sourced edge must never be treated as "removable by editing the
   description".
4. **Inverse verbs & multi-source dep rows.** A `blocks`/`blocked-by` dep row is
   **multi-source**: the row `(X, B, blocks)` is produced by *either* B's
   `blocks(ka-X)` *or* X's `blocked-by(ka-B)`. Reconciling from a single bead's
   write cannot see the other endpoint's text, so it cannot safely decide the row
   is stale.

## Design

### Scope: parent-child edges only

Reconcile is **restricted to `parent-child` dep rows** derived from the `parent` /
`child-of` verbs. This is the exact edge the epic targets (re-parenting), and it
is the **only** dep-row shape whose sole possible parser source is the source
bead's own text:

> For a row `(bead_id=B, depends_on_id=X, type=parent-child)`, the only verbs that
> emit `type=parent-child` are `parent`/`child-of`, and `depRowForTypedVerb` sets
> `bead_id = source` for both. So **no other bead's text can produce this row** —
> it originates solely from B's `parent:`/`child-of:` mentions of X. This makes
> single-bead reconcile unambiguous (resolves hazard 4 for this edge class).

`blocks`/`blocked-by` rows are **explicitly excluded** from auto-delete: they are
multi-source (both endpoints' text can produce the same row), so a single-bead
write cannot determine staleness without reading the other endpoint's live text.
They keep today's additive-only behavior. (Future work: a two-endpoint reconcile
could cover them; out of scope here.)

### The reconcile step

A new `reconcileTypedMentionDeps(ctx, db, b, extractComments)` runs in the **same
transaction** as the bead write, right after the additive `replaceBeadReferences`:

1. Compute `liveParents` = the set of parent-edge targets named by B's **live**
   text — `title ∪ description ∪ notes`, via
   `references.ParentEdgeTargetsExcept(field, "")` (the same authoritative helper
   `kd reparent` uses).
2. When comment extraction is enabled, **union in** the parent-edge targets from
   every one of B's live comments (read from the `comments` table in-tx). This
   preserves comment-sourced edges (resolves hazard 3).
3. `DELETE FROM deps WHERE bead_id = B AND type = 'parent-child' AND created_by =
   'kbeads/references/typed-mention' AND depends_on_id <> ALL(liveParents)`.
   When `liveParents` is empty, all parser-created parent-child rows for B are
   removed.

Because the additive insert only *adds* rows in `liveParents` and reconcile only
*deletes* rows **not** in `liveParents`, the two operate on disjoint sets and
their ordering is irrelevant.

### Guardrail: manual adds claim the row (resolves hazard 2)

`queryAddDependency`'s conflict clause changes from `DO NOTHING` to a **scoped
claim**:

```sql
ON CONFLICT (bead_id, depends_on_id, type) DO UPDATE
    SET created_by = EXCLUDED.created_by,
        operated_by_id = COALESCE(EXCLUDED.operated_by_id, deps.operated_by_id)
    WHERE deps.created_by = 'kbeads/references/typed-mention'
```

When a human/API `kd dep add` targets an edge that currently exists as a
**parser** row, the row is re-stamped to the manual actor — converting it to a
manual-owned edge that reconcile will never touch. Rows already owned by a human
(`WHERE` is false) are untouched, so this is a no-op for every case except the
exact dual-assertion hazard. The parser path (`bulkInsertTypedDeps`) keeps
`DO NOTHING`, so the parser never overwrites a human claim.

*Residual (documented):* a legacy row where a manual add no-op'd **before** this
change carries the parser marker with no way to recover the lost claim
retroactively. Reconcile is opt-in and off by default, and the claim is applied
on the next manual add; the residual is bounded and acceptable.

### Guardrail: opt-in flag

Reconcile is gated behind `BEADS_REFERENCES_RECONCILE_DEPS` (default **off**). No
deployment silently starts deleting edges on write until an operator opts in
(resolves hazard 1). The claim-on-conflict change is unconditional — it only ever
*protects* edges, so it is safe to ship on by default.

### Atomicity

Reconcile runs inside the bead-write transaction (`CreateBead`/`UpdateBead` and
the `kd admin reextract-references` per-bead tx), so the edge delete commits or
rolls back atomically with the text change that caused it.

## Guardrail → hazard matrix

| Hazard | Guardrail |
|---|---|
| 1. Silent removal on every write | `BEADS_REFERENCES_RECONCILE_DEPS` opt-in, default off |
| 2. `created_by` marker not proof | manual `kd dep add` claims the row (`DO UPDATE … WHERE created_by = marker`) |
| 3. Comment mentions immutable | desired set unions live comment parent-edge targets when comment extraction on |
| 4. Multi-source `blocks` rows | scope reconcile to `parent-child` only (single-source by construction) |

## Test matrix

- Parser-created parent-child row, mention removed → **deleted**.
- Parser-created parent-child row, mention still present → **kept**.
- Manual `kd dep add` row (`created_by` = actor), no mention → **survives**.
- Dual-assertion: parser row + later manual add → row is claimed → **survives**
  after mention removal.
- Comment-sourced parent mention (extraction on), not in description → **kept**.
- `blocks`/`blocked-by` parser row, mention removed → **kept** (out of scope).
- Present-in-title-not-description → **kept** (title is a live field).
- Flag off → **no delete** (byte-identical to pre-B behavior).
