# R4 — Operator-ownership audit trail

- **Bead:** ka-0000001481
- **Parent epic:** ka-0000003404 (operated_by transitivity, "dibs" rule, audit trail)
- **Sibling research:** R1 inventory (`operated-by-transitivity-r1-inventory.md`),
  R3 dibs rule (`operated-by-transitivity-r3-dibs.md`).
- **Status:** Research only — ADR-style recommendation. No schema migration
  lands until the parent epic's decision checkpoint approves it.

## 1. Context and problem statement

Matt's framing:

> "we must ensure we use the 'dibs' method where the last modifier of an
> object becomes its operator; we need to track operator ownership so we can
> present an audit trail; **a dedicated table in beads may be in the offing**".

R3 specifies the *behavior* (`DibsRule`, §4 of the R3 doc): on every successful
mutation, the server resolves a new operator and either no-ops or flips
`beads.operated_by`. R3 also requires that **every** flip — and every
attempted flip, including transparent sentinel mutations that never take
dibs — be recorded somewhere durable so the viewer's "ownership" tab and a
future `--ever-operated-by` query can reconstruct the timeline.

The question for R4 is: **where does that record live?**

Today the only candidate substrate is `mutation_history` (migration 008), to
which migration 012 added an `operated_by` column. R3 §6 leaves the choice
between (a) reusing it and (b) standing up a dedicated table explicitly
deferred to this doc.

This doc enumerates the candidate substrates, defines the query patterns the
viewer and CLI will need, scores each option against them, and recommends a
path forward. A B-option migration is sketched per the task brief.

## 2. Constraints from prior decisions

The following are inputs, not knobs:

1. **R3 dibs rule appends a `field='operated_by'` row to `mutation_history`
   on every flip and on every transparent attempt.** That is the rule's
   definition (R3 §4 branch 8). The audit substrate inherits whatever shape
   we give that append.
2. **Migration 012 already added `operated_by` to `mutation_history`.** Every
   row in the table — not just dibs flips — carries the resolved human
   operator. That makes mutation_history a *superset* of the dibs timeline.
3. **`beads.operated_by` is the live "current dibs holder".** The audit
   substrate is for *history*, not the current value. The sticky-vs-flipping
   semantics belong on the bead row (R3 §4); audit answers "who held it at
   time T" and "who has ever held it".
4. **No backfill of pre-cutover history is in scope.** Per the parent epic,
   R7's narrowing pass is the only retroactive fill. Anything older than
   migration 012 will read as `''` and is excluded from these queries by the
   partial index `WHERE operated_by <> ''`.
5. **R3 §6 also flags a possible new column `beads.operated_by_set_at
   TIMESTAMPTZ`.** That column is needed for the 60s debounce check and is
   independent of the audit-substrate question; this doc treats it as a
   separate (orthogonal) ask. The recommendation below adds it regardless.

## 3. Query patterns the substrate must support

These are the consumers, ordered roughly by hot-path cost. Each is named so
later sections can refer to them.

| # | Name | Consumer | Shape | Hotness |
|---|------|----------|-------|---------|
| Q1 | **bead → ownership transitions** | viewer "ownership" tab; `kd ownership <bead>` | `WHERE bead_id = $1 AND field = 'operated_by' ORDER BY created_at` | warm — opened on demand per-bead |
| Q2 | **bead → current operator** | every bead read (chip, list row, detail header) | `SELECT operated_by FROM beads WHERE id = $1` | very hot — already covered by `beads.operated_by` |
| Q3 | **operator → currently-owned beads** | `kd list -f operated_by=me`; viewer "my beads" | `SELECT … FROM beads WHERE operated_by = $1 AND fleet = $2 AND status IN (…)` | hot — covered by `idx_beads_operated_by_fleet_status` (migration 012) |
| Q4 | **operator → ever-owned beads** | `--ever-operated-by=me` toggle (R3 §5.5); audit reports | `SELECT DISTINCT bead_id FROM <audit> WHERE operated_by = $1 AND field = 'operated_by'` | warm — opt-in, not on default list path |
| Q5 | **bead → most recent dibs flip timestamp** | dibs rule's debounce check (R3 §4 branch 7a) | `SELECT MAX(created_at) FROM <audit> WHERE bead_id = $1 AND field = 'operated_by'` | very hot — runs inside the write path |
| Q6 | **operator → activity stream** | "what has Matt touched today?" timeline | `WHERE operated_by = $1 AND created_at > now() - interval '1 day' ORDER BY created_at DESC` | warm — dashboard query, paginated |
| Q7 | **bead → full attempted-flip log incl. transparent sentinels** | viewer activity feed (R3 §5.5: sentinels appear in activity, not in transitions) | `WHERE bead_id = $1 AND field = 'operated_by' ORDER BY created_at` (same shape as Q1, but no class filter) | warm |
| Q8 | **fleet-wide operator handoff stats** | "how often does dibs flip?" health metric | `SELECT bead_id, COUNT(DISTINCT operated_by) FROM <audit> WHERE field = 'operated_by' GROUP BY bead_id` | cold — periodic report |

Q5 is the load-bearing latency-critical one: the dibs resolver must answer
it on every write that the dibs rule even *considers*. Q1, Q4, Q7 are
read-side and can tolerate a single-digit-ms partial-index scan.

## 4. Candidate substrates

### Option A — Reuse `mutation_history`, filter by `field='operated_by'`

```sql
-- Existing (migration 008 + 012):
mutation_history (
  id            BIGSERIAL PRIMARY KEY,
  bead_id       TEXT NOT NULL,
  actor         TEXT NOT NULL DEFAULT '',          -- agent / bridge subject
  field         TEXT NOT NULL,                     -- 'operated_by', 'status', etc.
  old_value     TEXT NOT NULL DEFAULT '',
  new_value     TEXT NOT NULL DEFAULT '',
  mutation_type TEXT NOT NULL,                     -- create | update | delete
  created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  operated_by   TEXT NOT NULL DEFAULT ''
);
```

**Reads** (all add `AND field = 'operated_by'`):

- Q1 / Q7: `WHERE bead_id = $1 AND field = 'operated_by' ORDER BY created_at`
- Q4: `SELECT DISTINCT bead_id FROM mutation_history WHERE operated_by = $1 AND field = 'operated_by'`
- Q5: `SELECT MAX(created_at) FROM mutation_history WHERE bead_id = $1 AND field = 'operated_by'`
- Q6: existing partial index `idx_mutation_history_operated_by_created_at` already supports operator+time range.

**Writes:** R3 already appends a `field='operated_by'` row on every flip and
every transparent attempt. The dibs resolver and the existing per-field
mutation_history writer can share one append site, or the resolver can emit
a second row (the existing field-write row + a new `field='operated_by'`
row). Either way, no new write path; the resolver already has to touch this
table per R3 §4 branch 8.

**Pros:**
- Zero schema churn beyond the indexes below.
- Single source of truth for *all* per-field history — no risk of
  audit-substrate drift from mutation_history.
- The full attempted-flip log (Q7, including sentinel attempts) is already
  there — sentinels are an `operated_by` value, not a separate row kind.
- The R7 backfill story is the same one for the column itself; nothing new
  to plan.
- Replays / debug tooling that already iterate `mutation_history` see dibs
  events without a join.

**Cons:**
- Q1/Q5/Q7 all have `field='operated_by'` as a filter on top of `bead_id`.
  The existing `idx_mutation_history_bead_id` is `(bead_id, created_at)`
  with no field predicate — Q1 scans every per-field row for the bead.
  A partial index is needed (see §7).
- Q4's `SELECT DISTINCT bead_id` over the operator-side is a sort or hash
  on top of the existing partial index. Tolerable for a non-default toggle
  but not free.
- Storing dibs *and* the underlying field change can produce two
  mutation_history rows per flip — a `field='status'` row and a
  `field='operated_by'` row. Easy to confuse in tooling that counts rows;
  manageable but worth a doc comment.

### Option B — Dedicated `bead_operator_history` table

```sql
CREATE TABLE bead_operator_history (
  id              BIGSERIAL PRIMARY KEY,
  bead_id         TEXT NOT NULL,
  operator_id     TEXT NOT NULL,                   -- 'slack:U…' / 'oidc:…' / sentinels
  became_owner_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  prev_operator   TEXT NOT NULL DEFAULT '',        -- for "X → Y at T"
  source_event_id BIGINT,                          -- FK-ish into events.id (advisory)
  source_kind     TEXT NOT NULL,                   -- 'dibs-flip' | 'sentinel-attempt' | 'create' | 'refresh'
  actor           TEXT NOT NULL DEFAULT '',        -- agent / bridge subject (mirror of mutation_history.actor)
  fleet           TEXT NOT NULL DEFAULT ''         -- hot path for fleet-scoped queries
);

CREATE INDEX idx_boh_bead_at        ON bead_operator_history(bead_id, became_owner_at);
CREATE INDEX idx_boh_operator_at    ON bead_operator_history(operator_id, became_owner_at)
                                       WHERE operator_id <> '' AND source_kind = 'dibs-flip';
CREATE INDEX idx_boh_fleet_op_at    ON bead_operator_history(fleet, operator_id, became_owner_at)
                                       WHERE operator_id <> '' AND source_kind = 'dibs-flip';
```

**Reads:**

- Q1: `WHERE bead_id=$1 AND source_kind='dibs-flip' ORDER BY became_owner_at`
  — covered by `idx_boh_bead_at` after a small filter.
- Q4: `SELECT DISTINCT bead_id FROM bead_operator_history WHERE operator_id=$1 AND source_kind='dibs-flip'`
  — covered by `idx_boh_operator_at`.
- Q5: `SELECT MAX(became_owner_at) FROM bead_operator_history WHERE bead_id=$1 AND source_kind='dibs-flip'`
  — covered by `idx_boh_bead_at` (descending scan).
- Q7: same shape as Q1 but without the `source_kind` filter — still covered.
- Q8: covered by `idx_boh_bead_at` (group by bead_id).

**Writes:** dual-write. The dibs resolver appends a row to
`bead_operator_history` AND a `field='operated_by'` row to
`mutation_history`. Or: drop the mutation_history append for `operated_by`
specifically, and authoritatively use `bead_operator_history`. Both options
have downsides:

- Dual-write costs an extra INSERT inside the dibs hot path. Two indexed
  inserts per dibs-touching mutation. Manageable; the mutation_history
  insert is already there.
- Single-write (drop from mutation_history) is *cleaner* but breaks the
  mental model that mutation_history is the union of all per-field changes.
  Tools that walk mutation_history would have to join `bead_operator_history`.

**Pros:**
- Tight indexes that match each query 1:1 — no `field='operated_by'` filter
  on top of a wider scan.
- `prev_operator`, `source_kind`, `source_event_id` are first-class columns,
  not derived. The viewer's "X → Y at T" rendering is a single column read
  rather than a window function over `(old_value, new_value)` rows.
- `fleet` denormalised → fleet-scoped operator queries (Q4 with a fleet
  filter) get a 3-column index in the partial set, matching the
  `idx_beads_operated_by_fleet_status` shape from migration 012.
- Future-proof: when we add per-bead "claim count" or "shared-operator"
  semantics, a dedicated table absorbs new columns without weighing down
  the wider mutation_history schema.

**Cons:**
- New migration, new index strategy, new code path. Small but real ongoing
  maintenance burden.
- Dual-write or split-of-truth (see above) — neither is free.
- Risk of drift: a mutation_history row exists with `field='operated_by'` but
  no `bead_operator_history` row, or vice versa. Requires either a
  transactional invariant (both writes in the same tx) or a periodic
  reconciler. Both are doable; both are testable; both are *more code*.
- Backfill from existing mutation_history is a 30-line SQL job, but it must
  run carefully (large table, partial-index plan).

### Option C — Postgres `VIEW` over `mutation_history`

```sql
CREATE VIEW bead_operator_history AS
SELECT
  id,
  bead_id,
  operated_by  AS operator_id,
  created_at   AS became_owner_at,
  old_value    AS prev_operator,
  actor,
  CASE
    WHEN mutation_type = 'create'                                        THEN 'create'
    WHEN operated_by = old_value                                         THEN 'refresh'
    WHEN operated_by LIKE 'system:%' OR operated_by LIKE 'fleet:%'       THEN 'sentinel-attempt'
    WHEN operated_by = ''                                                THEN 'unknown-attempt'
    ELSE                                                                      'dibs-flip'
  END AS source_kind
FROM mutation_history
WHERE field = 'operated_by';
```

**Reads:** same query *shapes* as Option B, but the planner must rewrite
through the view down to the underlying `mutation_history` predicates.
Performance is identical to Option A — the view adds no physical structure.

**Pros:**
- Zero schema churn (no migration beyond CREATE VIEW), but the *interface*
  is the same as the dedicated table from B. Tooling can target the view
  today and the table tomorrow without churn.
- A clean SQL surface that hides the `field='operated_by'` filter and the
  classification logic; it stops every reader from reinventing the same
  WHERE clause.
- Free upgrade path: B's migration replaces the view with a real table; the
  outward shape stays.

**Cons:**
- Materialization is the catch. A regular VIEW doesn't add indexes — Q1, Q4,
  Q5 still scan `mutation_history` filtered by `field='operated_by'`. The
  partial indexes from §7 below are required regardless.
- A `MATERIALIZED VIEW` would help, but materialized views need explicit
  refresh — bad fit for a hot write path. Skip.
- Doesn't add `prev_operator`, `source_kind`, `source_event_id`, or `fleet`
  as first-class columns — only computed expressions. Tools wanting the
  string `'dibs-flip'` get a CASE expression in their plans.

## 5. Scoring

Score 1 (bad) → 5 (excellent). Equal weights; the recommendation in §6
weights based on which scores matter most.

| Criterion | A reuse | B dedicated | C view |
|-----------|:-------:|:-----------:|:------:|
| Schema-churn cost | 5 (new partial indexes only) | 2 (new table + indexes + dual-write) | 5 (CREATE VIEW only) |
| Q1/Q5/Q7 hot-path latency | 4 (partial index covers it) | 5 (1:1 index match) | 4 (same as A) |
| Q4 ever-owned latency | 4 | 5 | 4 |
| Read-side ergonomics (tools/SQL) | 3 (every reader writes the filter) | 5 (named columns) | 5 (named view) |
| Drift risk | 5 (single source) | 3 (dual-write or split-of-truth) | 5 (just a projection) |
| Future-proofing (new fields) | 3 (must widen mutation_history) | 5 (own schema) | 3 |
| Backfill / R7 alignment | 5 (already aligned) | 3 (replicate from existing rows) | 5 |
| Operational debug story | 5 (one table to grep) | 3 (two tables to join) | 4 (view + base table) |
| **Total** | **34** | **31** | **35** |

C edges A on ergonomics; B loses on churn and drift but wins on per-column
modeling.

## 6. Recommendation

**Adopt Option C as the public interface, backed by Option A as the storage
layer.** Concretely:

1. **No new table.** Append the dibs trail to `mutation_history` (as R3 §4
   branch 8 already does).
2. **Add the partial indexes from §7** so Q1, Q4, Q5, Q7 are O(log n) on
   the partial.
3. **Create a `bead_operator_history` VIEW** (the SQL in §4 Option C) so
   server code, the viewer's "ownership" tab, and any future `kd ownership`
   command read a stable, named projection. This pays the ergonomics cost
   once, in one place.
4. **Add the orthogonal `beads.operated_by_set_at TIMESTAMPTZ`** column for
   the dibs debounce check (R3 §4 branch 7a). The column is the dibs
   resolver's hot path; it removes Q5 from the per-write critical path
   entirely. The audit *also* satisfies Q5 via the view, but the bead-row
   column is faster and matches R3's recommendation.
5. **Hold Option B in reserve.** If telemetry shows mutation_history scans
   becoming a hot spot — for example, an `--ever-operated-by` query that
   the viewer actually runs frequently and that grows large — graduate to
   Option B by replacing the VIEW with a real table; outward shape stays
   the same. The B migration is sketched in §8.

Why not B today: the dual-write / split-of-truth penalty is real and the
performance benefit is hypothetical. Mutation_history is already the
canonical per-field history substrate, and *every* dibs-relevant write
already touches it under R3. Inventing a parallel table now optimises a
query mix nobody has measured yet, and adds drift risk to the dibs hot
path.

Why not A alone: tools and people will keep re-inventing
`WHERE field='operated_by'` and the sentinel-classification CASE. Writing
that once, as a view, is cheap and locks the contract.

This recommendation is a one-way door only in the *direction we want*: A+C
today, easy promotion to B later. Going the other way (start with B,
discover we should have just used A) is the costly mistake.

## 7. Indexes that R4 actually requires (whichever option lands)

Migration 012 already added:

- `idx_mutation_history_operated_by_created_at` on
  `mutation_history(operated_by, created_at)` partial `WHERE operated_by <> ''`.

That covers Q6 (operator activity over time). It does NOT cover Q1, Q5, Q7
(bead-side history of `operated_by` flips) without scanning every per-field
row for the bead and filtering, nor Q4 (DISTINCT bead_id per operator) at
its preferred shape.

R4 needs (under Option A or C):

```sql
-- D1: bead-side history (Q1, Q5, Q7).
CREATE INDEX idx_mutation_history_bead_operated_by
  ON mutation_history(bead_id, created_at)
  WHERE field = 'operated_by';

-- D2: operator-side ever-owned (Q4).
CREATE INDEX idx_mutation_history_op_dibs
  ON mutation_history(operated_by, bead_id)
  WHERE field = 'operated_by' AND operated_by <> '';
```

Both are partial indexes restricted to dibs-relevant rows, so they stay
small even as `mutation_history` grows with non-operator field changes.

Under Option B these are unnecessary; the dedicated-table indexes in §4 cover
the same access patterns.

The orthogonal column for the dibs hot-path debounce check:

```sql
ALTER TABLE beads
  ADD COLUMN operated_by_set_at TIMESTAMPTZ NOT NULL DEFAULT NOW();
```

`operated_by_set_at` is set to `NOW()` whenever the dibs resolver flips
`operated_by` (branches 5/6/7b in R3 §4). The default `NOW()` makes existing
rows coherent at migration time without a backfill — every bead is treated
as if its current operator was set at migration time, which is conservative
(no spurious flips inside the debounce window after the migration lands).

## 8. Sketched migration if/when we adopt Option B

This is the *hold-in-reserve* migration to graduate from A+C to B if
telemetry justifies it. Not for this epic. Sketched only because the task
brief asks for it.

```sql
-- 014_bead_operator_history.up.sql
CREATE TABLE bead_operator_history (
  id              BIGSERIAL PRIMARY KEY,
  bead_id         TEXT NOT NULL,
  operator_id     TEXT NOT NULL,
  became_owner_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  prev_operator   TEXT NOT NULL DEFAULT '',
  source_event_id BIGINT,
  source_kind     TEXT NOT NULL,
  actor           TEXT NOT NULL DEFAULT '',
  fleet           TEXT NOT NULL DEFAULT ''
);

CREATE INDEX idx_boh_bead_at
  ON bead_operator_history(bead_id, became_owner_at);

CREATE INDEX idx_boh_operator_at
  ON bead_operator_history(operator_id, became_owner_at)
  WHERE operator_id <> '' AND source_kind = 'dibs-flip';

CREATE INDEX idx_boh_fleet_op_at
  ON bead_operator_history(fleet, operator_id, became_owner_at)
  WHERE operator_id <> '' AND source_kind = 'dibs-flip';

-- Backfill from mutation_history.
-- Joins beads to lift fleet onto each row.
INSERT INTO bead_operator_history
  (bead_id, operator_id, became_owner_at, prev_operator, source_kind, actor, fleet)
SELECT
  mh.bead_id,
  mh.operated_by,
  mh.created_at,
  mh.old_value,
  CASE
    WHEN mh.mutation_type = 'create'                                       THEN 'create'
    WHEN mh.operated_by = mh.old_value                                     THEN 'refresh'
    WHEN mh.operated_by LIKE 'system:%' OR mh.operated_by LIKE 'fleet:%'   THEN 'sentinel-attempt'
    WHEN mh.operated_by = ''                                               THEN 'unknown-attempt'
    ELSE                                                                        'dibs-flip'
  END,
  mh.actor,
  COALESCE(b.fleet, '')
FROM mutation_history mh
LEFT JOIN beads b ON b.id = mh.bead_id
WHERE mh.field = 'operated_by';

-- Replace the view with a select against the new table (or drop it and
-- repoint readers).
DROP VIEW IF EXISTS bead_operator_history_view;

-- Server-side: dibs resolver dual-writes (or single-writes against this
-- table and stops emitting the field='operated_by' row to mutation_history;
-- this is a code-level decision documented in the impl epic).
```

```sql
-- 014_bead_operator_history.down.sql
DROP TABLE IF EXISTS bead_operator_history;
-- The view would be re-created if it had been dropped above.
```

Notes on the migration plan:

- Backfill is a single `INSERT ... SELECT`. On the production beads DB it is
  bounded by the number of `field='operated_by'` rows in mutation_history,
  which is small relative to total mutation_history volume. It can run
  online if we use `INSERT ... SELECT` in chunks of 10k bead_ids;
  `pg_repack` is not needed.
- `source_event_id BIGINT` is left nullable because the existing
  mutation_history rows pre-date the linkage to `events.id` and there is no
  reliable join. New writes (post-migration-014) populate it from the
  surrounding event-emission path.
- Fleet denormalisation costs a `LEFT JOIN beads` at backfill time; for new
  rows the dibs resolver already has the fleet on the calling context, so
  no join.
- Drift detection: a periodic check `SELECT COUNT(*) FROM mutation_history
  mh WHERE mh.field='operated_by' AND NOT EXISTS (SELECT 1 FROM
  bead_operator_history boh WHERE boh.bead_id = mh.bead_id AND
  boh.became_owner_at = mh.created_at)` flags missed dual-writes. Run it as
  a monitoring probe, not a hot-path check.

## 9. Worked examples of the recommended queries

Under the recommended A+C shape, with the `bead_operator_history` view from
§4 Option C and the partial indexes from §7.

```sql
-- Q1: bead → ownership transitions (viewer ownership tab).
SELECT operator_id, became_owner_at, prev_operator, source_kind, actor
FROM bead_operator_history
WHERE bead_id = $1
  AND source_kind = 'dibs-flip'
ORDER BY became_owner_at;

-- Q4: operator → ever-owned beads (`--ever-operated-by`).
SELECT DISTINCT bead_id
FROM bead_operator_history
WHERE operator_id = $1
  AND source_kind = 'dibs-flip';

-- Q5: bead → most recent dibs flip (NOT used in the hot path —
-- the bead row's operated_by_set_at is. Kept for tooling parity.)
SELECT MAX(became_owner_at)
FROM bead_operator_history
WHERE bead_id = $1
  AND source_kind = 'dibs-flip';

-- Q7: bead → all attempted flips, including sentinels (activity feed).
SELECT operator_id, became_owner_at, prev_operator, source_kind, actor
FROM bead_operator_history
WHERE bead_id = $1
ORDER BY became_owner_at;
```

The dibs resolver's debounce check (R3 §4 branch 7a) does NOT use any of
these — it reads `beads.operated_by_set_at` from the same row it's about
to update. That keeps the dibs hot path to a single-row read.

## 10. Open questions for the parent decision checkpoint

These belong in `gb decision create` against ka-0000003404, not in this
doc:

1. **Single-write vs dual-write under Option B.** If/when we graduate to B,
   does the dibs resolver stop emitting the `field='operated_by'` row to
   `mutation_history`, or does it dual-write? Recommend: dual-write for one
   release window (catches drift), then drop the mutation_history append in
   a follow-on migration. Allows a fast rollback by re-pointing the view
   at mutation_history.
2. **Source-kind enum membership.** The CASE in §4 Option C lists
   `create | refresh | sentinel-attempt | unknown-attempt | dibs-flip`. R3's
   decision table also identifies `dibs-flip` as a discrete outcome. Are
   `create` rows interesting to the viewer's "ownership" tab? Recommend:
   yes — show "originally created by X" alongside the first dibs flip.
3. **Should reads-without-flip emit a row?** R3 says no (branch 3 of §4: read
   ops do not pass through the rule). The audit substrate inherits that —
   it never sees a row for a pure read. Confirm.
4. **`prev_operator` on `create` rows.** Should it be `''` or NULL? The
   table column has `DEFAULT ''` matching project convention. View should
   emit `''` for consistency. Confirm the viewer renders `''` as
   "originally created by X" rather than "X → X".
5. **Does the `kd CLI` get an `ownership` subcommand now or later?** R3 §8
   recommends viewer-only initially. R4 affirms: ship the view, ship the
   index, do not ship `kd ownership` until an audit user actually asks for
   it. (Cheap to add later; the view is the contract.)

## 11. What this doc is not

- Not a server patch. The dibs resolver, the view DDL, and the index DDL
  all belong to the implementation epic that follows the parent decision.
- Not a backfill plan. R7's narrowing is the only retroactive fill; this
  doc only sketches the B-migration backfill from existing mutation_history
  if/when B lands.
- Not a test plan. Per the parent epic's hard rule, no test code or test
  design lives here.
- Not the dibs rule itself — that's R3.

## 12. Links

- Parent epic: ka-0000003404
- This task: ka-0000001481
- Sibling research:
  - R1 inventory: `gardenio/docs/research/operated-by-transitivity-r1-inventory.md`
  - R3 dibs rule: `gardenio/docs/research/operated-by-transitivity-r3-dibs.md`
- Schema:
  - `kbeads/internal/store/postgres/migrations/008_mutation_history.up.sql`
    (introduces `mutation_history`)
  - `kbeads/internal/store/postgres/migrations/012_human_attribution.up.sql`
    (adds `operated_by` to beads/comments/deps/events/mutation_history; existing
    partial indexes I1–I4 listed in the migration cover Q3 and Q6)
- Prior research plan: ka-0000003603 (R1-R10, closed). Implementation epic
  that introduced `operated_by`: ka-0000001133.
