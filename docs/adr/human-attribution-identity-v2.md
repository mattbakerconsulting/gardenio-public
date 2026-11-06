# ADR: Human-attribution identity shape v2 (`operators` FK + aliases)

- **Bead:** ka-0000006334
- **Status:** Proposed — research only, no schema/code lands until human green-light on this ADR
- **Author:** gardenio-draft-adr-humanattributionidentityv2-hzq
- **Date:** 2026-05-04
- **Supersedes:** `docs/adr/human-attribution-identity.md` (R2 ADR — Option A, free-form `operated_by TEXT`)
- **Supersedes decision:** ka-0000001224 (R10 "approve-full" of Option A)
- **Umbrella:** ka-0000003625 — "Pivot operated_by to Option B (operators FK + aliases) — unwind R2 Option A"
- **Scope checklist:** ka-0000005927 on decision ka-0000001784
- **Related (paused, will be re-scoped onto Option B):** ka-0000001133 (P-series implementation), ka-0000007565 (transitivity design), ka-0000003639 (transitivity implementation, in-progress)

---

## 1. Context

### 1.1 Option A is shipped and behaving as designed

Migration 012 (`kbeads/internal/store/postgres/migrations/012_human_attribution.up.sql`) is deployed in prod and gardenio-rwx. It adds:

- `operated_by TEXT NOT NULL DEFAULT ''` on `beads`, `comments`, `deps`, `events`, `mutation_history`
- `operated_by_display TEXT` on `beads`
- Four partial indexes keyed on `WHERE operated_by <> ''`

Migration 014 (`014_operated_by_dibs.up.sql`) layers a `bead_operator_history` view + dibs indexes on top of that TEXT column.

The resolver (`kbeads/internal/server/audit.go:160-210`) is wired and active behind `BEADS_OPERATED_BY_ENABLED`. Stored values today look like `slack:U0OPERATOR1` (Slack), `oidc:matt@acme.example` (CLI), `system:scheduler`, `fleet:gardenio`. The bridge `WithOperator` plumbing fans out from `gardenio/controller/internal/bridge/bot.go:779`, `gitlab_mentions.go:175,295`, `jira_mentions.go:193`, `gitlab_sync.go:91,234,375,671,713,801` and similar.

### 1.2 The gap that R2 explicitly deferred

R2 ADR §2 enumerated three operator types — Slack human, direct-CLI human, scheduled agent — and §5 acknowledged that **a Slack-triggered bead and a CLI-triggered bead by the same human store as different `operated_by` values** under Option A:

> The cross-provider unification benefit of Option B (one operator, many aliases) is the real argument against Option A. The counter-argument: today we don't have authoritative cross-provider mappings; inventing them in v1 risks conflation. The right time to add an `operators` table is **after** we have a working `operated_by TEXT` column populated for some months […].
> — `docs/adr/human-attribution-identity.md` §4

R2 deferred unification to a future ADR and proposed `operated_by_display` as the cosmetic stand-in. In practice `operated_by_display` was never populated by the resolver; the only display-name resolution that exists today is the Slack-only LRU cache in `bot_human_survey_rating.go:91-113` (used by the rating flow, not by the audit resolver).

The result observable in prod: when Matt Baker (Slack `U0OPERATOR1`) drives a bead from Slack and then runs `kd` from a laptop, the audit trail shows two distinct opaque IDs (`slack:U0OPERATOR1` and `oidc:matt@acme.example`). Filters like `kd list --operated-by=oidc:matt@acme.example` miss his Slack-driven work; updating his email is impossible (it would have to be backfilled across every audit-bearing row).

### 1.3 Why we are pivoting now, not "in some months"

Matt directed the pivot in the 2026-05-04 Slack thread (umbrella bead ka-0000003625). Three things change the calculus relative to R2's "wait and see" reasoning:

1. **The `operators` side-table evolution path R2 sketched is not free.** R2 §5 ("a future ADR can add an `operators` lookup later") implied additive layering. In practice every resolver, every audit-write callsite, and the dibs view (014) would still need to flip from "write TEXT" to "look up id, write FK", so there is no painless future migration — it is the same migration whether we run it now or in six months.
2. **Adding `operators` as a side-relation while keeping `operated_by TEXT` as the source of truth duplicates state.** Both columns would need to stay in sync; the FK becomes lossy whenever the resolver writes a sentinel/empty.
3. **The decision-point is "are these two opaque IDs the same human?"** With a TEXT column we are guessing every time we want to render or filter. With an aliases table we record the answer once.

R10 (`ka-0000001224`) was an "approve-full" of Option A. This ADR supersedes that decision; the implementation epics it spawned (ka-0000001133, ka-0000007565, ka-0000003639) pause and re-scope onto the schema described below.

### 1.4 What is in scope vs. out of scope (locked by ka-0000005927)

**In scope for this ADR:**

- New tables `operators` + `operator_aliases`.
- Replace `operated_by TEXT` with `operated_by_id BIGINT REFERENCES operators(id)` on every audit-bearing table (`beads`, `comments`, `deps`, `events`, `mutation_history`).
- Pre-seeded sentinel rows for `system:*` / `fleet:*` operators so the FK is never NULL on a known-autonomous write.
- **Retroactive rebind** of every existing row carrying a TEXT value (e.g. `slack:U0OPERATOR1`) — migration walks the corpus, upserts `operators`/`operator_aliases`, rewrites the row to FK. No abandoned TEXT data.
- Re-do / supersede migration 012. Spec the migration path from currently deployed Option A state to Option B end state.
- Resolver (controller + bridge `WithOperator` + jira/gitlab bridges + kd CLI) writes operator id, not opaque text.
- Beads-viewer + bridge plumbing changes that flow naturally from the schema change.

**Out of scope, deferred to follow-up beads:**

- A periodic Slack `users.info` resync job to refresh `operators.display` / `operators.email` after pivot. Captured below in §7.
- Beads-viewer UI redesign for a dedicated "people" surface.
- Multi-tenant scoping of operators (today every operator is global).
- Cross-provider unification of pre-existing opaque ids that have **no** authoritative mapping (e.g. fusing two `slack:U…` ids that both map to the same human but where neither has logged in via OIDC). The migration in §4 captures observed mappings only; manual consolidation lands in a follow-up.

---

## 2. Decision

**Adopt Option B: operators table + operator_aliases table + `operated_by_id BIGINT REFERENCES operators(id)` on every audit-bearing table.**

This supersedes Option A (`operated_by TEXT`) as the source of truth for human attribution. The TEXT column does not survive the migration; every existing value is re-bound to a FK before the column is dropped.

The decision is driven by:

1. **Cross-provider unification is the actual feature.** The motivating use case ("show me everything Matt did, regardless of which surface he used") is a JOIN against `operator_aliases`, not a string predicate. Storing an opaque per-surface ID is a workaround.
2. **Single source of truth for operator metadata.** Updating Matt's email is one row in `operators`; under Option A it is a fan-out backfill across five tables.
3. **Referential integrity.** Sentinel + agent-bead-fallback paths guarantee non-empty operator strings today; an FK with `NOT NULL` makes that guarantee structural rather than convention.
4. **Aligns with the `WithOperator` resolver model already in place.** The resolver becomes "look up or insert by `(kind, subject)`, return id" — the same shape as today's "produce string", with one extra DB round-trip on the cold path. The hot path (sentinel rows, already-seen aliases) is a single indexed lookup.

The cost — a one-shot migration that touches every audit-bearing row, plus follow-on work in the resolver, dibs view (014), proto, and read paths — is bounded and front-loaded. Option A would defer the same cost into a later "Option B but for real this time" migration plus the cleanup of any half-state in the meantime.

---

## 3. Schema

### 3.1 New tables

```sql
-- operators: one row per known human OR sentinel autonomous identity.
-- Pre-seeded with system:* / fleet:* sentinels at migration time.
CREATE TABLE operators (
    id          BIGSERIAL PRIMARY KEY,
    kind        TEXT NOT NULL,            -- 'slack', 'oidc', 'gitlab', 'jira', 'system', 'fleet'
    subject     TEXT NOT NULL,            -- the canonical subject for this kind:
                                          --   slack: U-id (e.g. 'U0OPERATOR1')
                                          --   oidc:  email (e.g. 'matt@acme.example')
                                          --   gitlab/jira: account id / username
                                          --   system/fleet: name (e.g. 'scheduler', 'gardenio')
    display     TEXT NOT NULL DEFAULT '', -- human-readable label, refreshed lazily
    email       TEXT NOT NULL DEFAULT '', -- best-known email; '' when unknown
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    -- The (kind, subject) tuple is the canonical identity within a kind;
    -- aliases that are *also* canonical for some other kind get their own
    -- row in operator_aliases, not in operators.
    UNIQUE (kind, subject)
);

-- operator_aliases: every (kind, subject) the system has ever seen for an
-- operator, including its canonical (kind, subject). Lookups go
-- alias → operator_id, never bead → string predicate.
--
-- Examples:
--   operator 42 (kind=oidc, subject=matt@acme.example):
--     - alias (slack, U0OPERATOR1)
--     - alias (oidc,  matt@acme.example)         -- mirrors the canonical row
--     - alias (gitlab, matthewbaker)
CREATE TABLE operator_aliases (
    operator_id  BIGINT NOT NULL REFERENCES operators(id) ON DELETE CASCADE,
    kind         TEXT NOT NULL,
    subject      TEXT NOT NULL,
    source       TEXT NOT NULL DEFAULT '', -- where we learned the mapping:
                                           --   'migration', 'slack-resync',
                                           --   'oidc-login', 'manual', etc.
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    -- A given (kind, subject) maps to at most one operator. If we later
    -- discover this is wrong (two humans share an alias), the consolidation
    -- task moves the alias row, not duplicates it.
    PRIMARY KEY (kind, subject)
);

CREATE INDEX idx_operator_aliases_operator_id ON operator_aliases(operator_id);
```

Pre-seeded rows (inserted by the up migration after table creation):

| `id` | `kind`   | `subject`           | `display` | `email` | Notes |
|------|----------|---------------------|-----------|---------|-------|
| 1    | `system` | `scheduler`         | ''        | ''      | Cron-driven autonomous agents |
| 2    | `system` | `gitlab-sync`       | ''        | ''      | GitLab poller / pipeline / approval — same family as today's `system:gitlab-*` strings (`gitlab_sync.go:91,375`) |
| 3    | `system` | `gitlab-poller`     | ''        | ''      | (kept distinct because today's resolver writes distinct strings; consolidating is a follow-up) |
| 4    | `system` | `gitlab-pipeline`   | ''        | ''      | |
| 5    | `system` | `gitlab-approval`   | ''        | ''      | |
| 6    | `system` | `gitlab-webhook`    | ''        | ''      | |
| 7    | `system` | `gitlab-mr-review`  | ''        | ''      | |
| 8    | `fleet`  | `gardenio`           | ''        | ''      | Fleet/static-token fallback |

The exact sentinel set is the union of `system:*` / `fleet:*` strings observable in the existing data plus the explicit hard-coded callsites in the bridge. The migration §4 enumerates by walking distinct values.

### 3.2 FK column on every audit-bearing table

```sql
-- One column per audit-bearing table (matches the set in migration 012).
ALTER TABLE beads             ADD COLUMN operated_by_id BIGINT REFERENCES operators(id);
ALTER TABLE comments          ADD COLUMN operated_by_id BIGINT REFERENCES operators(id);
ALTER TABLE deps              ADD COLUMN operated_by_id BIGINT REFERENCES operators(id);
ALTER TABLE events            ADD COLUMN operated_by_id BIGINT REFERENCES operators(id);
ALTER TABLE mutation_history  ADD COLUMN operated_by_id BIGINT REFERENCES operators(id);
```

`operated_by_id` is initially NULLABLE because the migration runs in phases (§4). After backfill completes, the up migration re-asserts `NOT NULL` on every column except the historical-data-only edge cases captured in §4.5.

Indexes (mirror migration 012 partial-index plan but on FK):

```sql
CREATE INDEX idx_beads_operated_by_id
    ON beads(operated_by_id)
    WHERE operated_by_id IS NOT NULL;

CREATE INDEX idx_beads_operated_by_id_fleet_status
    ON beads(fleet, operated_by_id, status)
    WHERE operated_by_id IS NOT NULL;

CREATE INDEX idx_events_operated_by_id_created_at
    ON events(operated_by_id, created_at)
    WHERE operated_by_id IS NOT NULL;

CREATE INDEX idx_mutation_history_operated_by_id_created_at
    ON mutation_history(operated_by_id, created_at)
    WHERE operated_by_id IS NOT NULL;

-- D2-equivalent for dibs hot path (migration 014's idx_mutation_history_op_dibs).
CREATE INDEX idx_mutation_history_op_id_dibs
    ON mutation_history(operated_by_id, bead_id)
    WHERE field = 'operated_by_id' AND operated_by_id IS NOT NULL;
```

### 3.3 Touched columns elsewhere

- `beads.operated_by_display TEXT` (migration 012) — **dropped**. Display becomes a JOIN against `operators.display`. Beads-viewer reads via the JOIN; SSE event payloads embed a denormalized `display` snapshot only when the consumer requests it (R6 follow-up).
- `mutation_history.operated_by_set_at TIMESTAMPTZ` (migration 014) — **kept**. The dibs timestamp is orthogonal to the identity shape.
- `bead_operator_history` view (migration 014) — **rewritten** to project `operated_by_id`; the field-name string the view filters on changes from `'operated_by'` to `'operated_by_id'` (or we keep both during the cutover; see §4.4).
- `dibs.go` queries `SELECT operated_by FROM beads WHERE id = $1` and `UPDATE beads SET operated_by = $2` — both rewrite to use `operated_by_id`.

### 3.4 Proto / wire shape

Today's proto (`kbeads/proto/beads/v1/types.proto`) carries `operated_by` (field 22) and `operated_by_display` (field 23) as TEXT strings on every type. Two wire options for v2:

- **(W1, recommended)** Add new `int64 operated_by_id` fields alongside the existing `operated_by` strings; keep the strings on the wire indefinitely as a server-side denormalized convenience. Client-side filtering by id is new; client-side filtering by string is unchanged. New `Operator` message type carries `id`, `kind`, `subject`, `display`, `email`. `ListBeads.operated_by_id_in` joins are server-side.
- **(W2, deferred)** Replace string fields with the `Operator` message embedded directly. Heavier wire footprint, breaks all existing readers. Defer.

Recommend W1 for at-rest pivot; W2 is a separate deprecation epic if we ever decide we want it.

`X-Operated-By` header semantics (`kbeads/internal/client/http.go:761`, gRPC `x-operated-by` metadata) keep the same string shape — clients still send `slack:U…` / `oidc:…` strings. The server resolves to id at the audit boundary. This means **no client-side CLI changes are required to ship the schema pivot**; CLI changes only land when we stop trusting strings (§5.4).

---

## 4. Migration plan

### 4.1 Constraints

- **Beads DB is load-bearing** (rule 5 in the canonical-prod role banner; same applies at platform level). The migration runs while the daemon is up; we cannot afford a multi-hour write outage.
- Mutation history is append-only and enormous relative to the other audit tables. Backfill must be batched.
- Migration 012 is already applied. We are not "rewinding" 012; we are **adding** schema and **rebinding** data, then dropping the old TEXT columns once dual-write is healthy.
- Migration 014 (dibs) layers on 012's TEXT column. The dibs view and indexes have to flip atomically with the resolver flip.

### 4.2 Sequence overview

The migration is split into three numbered Postgres migration files plus an admin-driven backfill:

| # | Migration / step | What it does |
|---|------------------|--------------|
| 015 | `015_operators_tables.up.sql` | Create `operators`, `operator_aliases`, indexes; pre-seed sentinel rows; **add nullable `operated_by_id` columns** on the five audit tables; create the new partial indexes (still nullable). |
| — | `kd admin backfill-operators` (offline-callable, batched) | Walk distinct `operated_by` TEXT values, upsert `operators`/`operator_aliases`, rewrite each audit row's `operated_by_id`. Idempotent: every batch carries `WHERE operated_by_id IS NULL` predicates. Driven by an admin command parallel to today's `kd admin backfill-operated-by` (`kbeads/cmd/kd/admin_backfill_operated_by.go`). |
| 016 | `016_operators_cutover.up.sql` | After backfill is verified: enforce `NOT NULL` on `operated_by_id` (except where §4.5 carves an exception), rewrite `bead_operator_history` view to project the FK, replace the dibs partial index. |
| 017 | `017_drop_operated_by_text.up.sql` | Drop `beads.operated_by_display`, drop `operated_by` TEXT columns and their partial indexes from migration 012, drop `mutation_history.operated_by` from 012. Keeps `mutation_history.operated_by_set_at` (014). |

Migration 012's down-path semantics are preserved by the supersession: 015's down recreates 012's columns empty, 016's down restores nullable, 017's down restores the TEXT columns and re-derives them by reverse JOIN. Reversibility is best-effort — the canonical operational stance is "roll forward".

### 4.3 Backfill algorithm

The admin command is the central piece; the migrations only set up the schema for it.

```
for each (kind, subject) ∈ {distinct prefix-decoded operated_by TEXT values}:
    upsert into operators(kind, subject) -- ON CONFLICT (kind, subject) DO NOTHING
    upsert into operator_aliases(kind, subject) -- alias mirrors canonical
        with operator_id from the upserted row
        source = 'migration'

for each table T in (beads, comments, deps, events, mutation_history):
    UPDATE T SET operated_by_id = a.operator_id
      FROM operator_aliases a
     WHERE T.operated_by_id IS NULL
       AND T.operated_by <> ''
       AND prefix(T.operated_by) = a.kind
       AND suffix(T.operated_by) = a.subject
     -- batched by id range (ctid range for mutation_history) to keep
     -- the transaction size bounded; commit per batch.

-- Final pass: empty-string TEXT rows are NOT bound to any operator.
-- These are pre-Option-A rows where the resolver had nothing to write.
-- See §4.5 for the policy.
```

**Prefix decode** is the same logic that today's resolver writes: split on the first `:`, the LHS is `kind` (`slack`, `oidc`, `system`, `fleet`, `gitlab`, `jira`), the RHS is `subject`. Anything that does not match this shape is logged and quarantined for manual review (we do not expect any).

**Cross-kind alias detection** during migration: when the same human appears under multiple kinds the migration cannot tell automatically. Two safe rules:

1. If a Slack user has logged into the OIDC IdP and we have an authoritative mapping (e.g. via the existing `bot_human_survey_rating.go` LRU cache snapshot, or a CSV of known mappings), apply it: both `(slack, U…)` and `(oidc, email)` aliases point to the same operator row.
2. Otherwise the migration creates one operator per `(kind, subject)` and leaves consolidation for a follow-up "operator merge" admin command. **No conflation in v1.**

This matches the ka-0000005927 scope decision ("retroactive rebind, definitely" — but only against authoritative mappings).

### 4.4 Resolver dual-write window

Between 015 and 017 the daemon writes to **both** `operated_by_id` and the legacy `operated_by` TEXT column. The resolver helper grows a "look up or insert by `(kind, subject)`, return (id, prefixed_string)" entry point; both the FK and the TEXT column are populated on the same write path. Read paths prefer the FK and fall back to TEXT only for code that has not been migrated yet.

The dual-write window is bounded by the deploys of the resolver change and the read-path cutover. Order of operations:

1. Deploy daemon with 015 applied; resolver dual-writes; reads still use TEXT.
2. Run backfill until `operated_by` TEXT and `operated_by_id` agree on every row (verifier query).
3. Apply 016; flip read paths to FK (proto stays additive, see §3.4).
4. After ≥1 cycle of green production traffic, apply 017 and remove TEXT-only code paths.

If anything goes wrong during 015 → 016, we roll back to TEXT-as-source-of-truth without data loss because the TEXT column never stopped being written.

### 4.5 Empty-string and pre-cutover rows

Rows where `operated_by = ''` correspond to writes that happened before Option A flipped on (or while the flag was off). Two policies considered:

- **(P1) NULL in the FK column.** Cleanest semantically: "we do not know who operated this bead." Requires `operated_by_id` to remain NULLABLE on the audit tables forever, OR we backfill all of them to a sentinel.
- **(P2) Backfill empty rows to a single `system:legacy-pre-attribution` sentinel.** Lets us declare `NOT NULL` on `operated_by_id`. Loses the distinction between "we genuinely do not know" and "this was a known-autonomous write".

Recommendation: **P1 for `mutation_history` and `events` (history-only tables; preserving "unknown" is a feature)**, **P2 for `beads`, `comments`, `deps` (live-state tables; the new sentinel makes downstream queries simpler)**. The migration spec calls this out in 016.

### 4.6 Performance budget

Rough sizing (order-of-magnitude) before benchmarking on a snapshot:

- `mutation_history` is the largest table by row count; backfill in batches of 50k–100k rows with a brief sleep between batches. The resolver's existing `BEADS_OPERATED_BY_ENABLED` flag stays ON throughout; nothing in the runtime path scans the TEXT column.
- `beads`, `comments`, `deps`, `events` are small enough to backfill in single-table loops.
- The new partial indexes are created CONCURRENTLY where supported (Postgres 12+ — already required).

Acceptance: backfill completes in < 60 minutes against a dump matching current prod size; daemon stays up the whole time. Numbers TBD on a real benchmark — the implementation epic captures this as an exit criterion.

### 4.7 What this re-does about migration 012

Migration 012 stays in the migrations directory (it has already run in prod; we cannot un-run it). The story is:

- 012 added `operated_by TEXT` and indexes.
- 015–017 add the FK schema, rebind data, and drop everything 012 added.

The R2 ADR was correct that 012 is a small, reversible change relative to a from-zero design — but **forward-only** the path is 012 → 015 → 016 → 017, not "delete 012 and start over". We cannot delete a deployed migration; we can only land a new one that supersedes it.

---

## 5. Resolver changes

### 5.1 Server (`kbeads/internal/server/audit.go`)

Replace `operatedByFromContext` and `resolveOperatedByPure` with an FK-shaped resolver:

- Same input contract (read OIDC subject, X-Operated-By header, agent-bead fallback).
- Same string ladder produces a `(kind, subject)` tuple instead of a prefixed string.
- New helper `resolveOperator(ctx)` returns `(operatorID int64, source string, err error)`.
- `ON CONFLICT DO UPDATE … RETURNING id` upsert against `operators` + `operator_aliases` on cold-path resolution; hot path is a `SELECT id FROM operator_aliases WHERE kind=$1 AND subject=$2` — single index hit.
- `applyOperatedByHeader` still sanitizes; the only difference is the eventual write target.
- Dual-write window: existing `operatedByFromContext` keeps returning the prefixed string for callers that have not migrated yet; new write paths take the resolved id directly.

The R9 §4.4 source-label set (`oidc_subject`, `static_header`, `agent_bead_fallback`, `empty`) carries over verbatim. M2/M3/M4 metrics still fire on the resolution attempt.

`static_header` is a header-provenance label, not an auth-type label: it is emitted for both `id.Type=static` and `id.Type=fleet` calls that carry `X-Operated-By`. Use `gardenio_beads_requests_by_auth_type_total{auth_type,method_class}` (gd-M4pFzv6bUq) to measure static-token use. The `fleet_header` / `static_header` source split was left out: the new counter answers the question, and splitting the source would change the label vocabulary the dashboards already read.

### 5.2 Slack bridge (`gardenio/controller/internal/bridge/`)

`beadsapi.WithOperator` continues to take a string (today: `slack:U0OPERATOR1`, `oidc:foo@bar`, etc.). The bridge does not need a code change for the schema pivot itself — the daemon's resolver translates the string at the audit boundary.

A future cleanup folds the bridge's internal "build the operator string" sites (e.g. `operatorFromSlackUser`, `gitlab_mentions.go:175`, `jira_mentions.go:193`) into a typed `Operator` carrier that ships the kind + subject explicitly. That is in §7 follow-ups, not blocking this ADR.

The Slack `users.info` resync (out of scope per ka-0000005927) writes into `operators.display` / `operators.email` once it lands; today's `bot_human_survey_rating.go` cache becomes the seed for the first resync pass.

### 5.3 Jira / GitLab bridges

Same story as Slack: today they call `beadsapi.WithOperator(ctx, "gitlab:matthewbaker")` etc. After the pivot, the daemon resolves that string to an operator id at write time. Pre-seeded `system:gitlab-*` sentinels (§3.1) cover the autonomous writes.

`gitlab_agent_resolver.go:225-229` pulls `operated_by` off the task bead's `Fields` JSON and re-stamps it — that field stays a string for now; the operator resolution happens at the daemon side.

### 5.4 `kd` CLI (`kbeads/cmd/kd/`)

- `--operated-by` flag (`main.go:149`) keeps its string shape (`slack:U…` / `oidc:…`). Resolution happens server-side.
- New filter `kd list --operated-by-alias=oidc:matt@acme.example` joins through `operator_aliases` so a single value matches every bead operated by that human regardless of which surface stamped it. The existing `--operated-by=…` filter becomes "exact alias-string match" for back-compat.
- New `kd operator show <id-or-alias>` to inspect an operator and its aliases. Read-only; mutations land later under `kd admin operator merge` (§7).
- `kd admin backfill-operated-by` (the existing command) is renamed `kd admin backfill-operators` and re-targeted at populating `operator_aliases` from agent-bead fields. The TEXT-column backfill it does today becomes obsolete after 017.

### 5.5 Beads-viewer

- Reads display name via JOIN. The constellation/owner views currently keying off `operated_by` strings switch to operator id; aliasing happens transparently.
- An ownership tab now shows "all aliases for this operator" — small additive change.

---

## 6. Rollout sequence

```
[Phase 0] Land this ADR. Operator green-light required before any code below.

[Phase 1] Schema introduction (015)
  - Apply 015_operators_tables.up.sql in gardenio-rwx.
  - operator_aliases pre-seeded for sentinels only.
  - operated_by_id columns nullable; resolver still writes TEXT only.
  - No behavior change for any existing query.

[Phase 2] Resolver dual-write
  - Deploy controller/daemon with FK-aware resolver.
  - Resolver writes BOTH operated_by_id (via upsert) AND operated_by TEXT.
  - Read paths still use TEXT.
  - Smoke test via gardenio-rwx; verify counts and sample matches.

[Phase 3] Backfill (kd admin backfill-operators)
  - Run against gardenio-rwx; verify zero rows have populated TEXT but NULL FK.
  - Run against prod beads DB during a low-traffic window. Bound: < 60 min.

[Phase 4] Cutover (016)
  - Apply 016_operators_cutover.up.sql.
  - operated_by_id NOT NULL where applicable.
  - Read paths flip to FK; bead_operator_history view rewritten.
  - Dibs hot path now keys off operated_by_id.

[Phase 5] Soak (≥ 1 release cycle in prod)
  - Watch dibs metrics, audit query latency, resolver M2/M3/M4 panels.
  - Verify Slack bridge + Jira/GitLab bridges still produce coherent operator
    rows (no surprise sentinels, no rejected aliases).

[Phase 6] Drop legacy (017)
  - Apply 017_drop_operated_by_text.up.sql.
  - Remove all TEXT-only code paths.
  - Beads-viewer / SSE consumers stop reading the TEXT field; W1 keeps the
    string on the wire for a bounded deprecation window if needed.

[Phase 7] Follow-up beads (out of scope here)
  - users.info resync job populates operators.display / operators.email.
  - Operator merge admin command consolidates known-same-human aliases.
  - W2 wire-shape decision (drop TEXT from proto entirely).
```

Each phase is a separate operator-approved checkpoint. Phases 1–4 are reversible; phase 6 is the point of no return.

---

## 7. Out-of-scope / follow-ups

These are tracked here so the implementation epic does not lose them; each lands as its own bead under the umbrella once this ADR is approved.

1. **Slack `users.info` resync job.** Periodic job that walks `operator_aliases WHERE kind='slack'`, calls `users.info`, refreshes `operators.display` / `operators.email`. Seeded by the existing in-memory cache in `bot_human_survey_rating.go:91-113`. Expected cadence: hourly with rate-limit awareness. Bead title: "Slack operator resync job". *Amended 2026-10-21 (gd-iRFGU6xSK7):* the job no longer copies the Slack profile. A Slack profile is the person's own to edit and the bridge's machine token could write any value onto any operator, so `POST /v1/admin/operators/{id}/identity` refuses explicit values from machine tokens; the bridge sends `from_user_bead` and the daemon derives display (`display_name`, else title) and email from the one user bead whose aliases resolve to the operator. Explicit values need an OIDC gardenio-admin.
2. **OIDC ↔ Slack alias linking on login.** When a Slack user runs `kd` for the first time with an OIDC token, the resolver should (a) look up an existing operator by `(oidc, email)`, (b) if found and there is also an unlinked `(slack, U…)` operator that the bridge has previously stamped on this same human's beads, propose merging them. Today the bridge already calls `users.info` and gets the email; that mapping lands in `operator_aliases` automatically as the bridge reuses the same resolver. The merge proposal is the human-in-the-loop part — fits a `gb decision` shape.
3. **`kd admin operator merge`.** Manual consolidation tool for the cases where the migration created two operators that should be one. Audit-logged; reversible.
4. **W2 wire-shape pivot.** Remove `operated_by`/`operated_by_display` strings from proto entirely and have clients read `Operator.id` + denormalized `display` snapshots. Coordinated proto bump across kbeads, gardenio controller, beads-viewer.
5. **Multi-tenant scoping.** When tenant isolation lands, operators acquire a `tenant_id` and the unique key becomes `(tenant_id, kind, subject)`. Out of scope until tenancy lands.
6. **Beads-viewer "people" surface.** A dedicated view that shows an operator and all their beads/comments/deps regardless of alias. Naturally falls out of the FK; UX work is its own bead.
7. **Re-evaluate paused implementation epics.** ka-0000001133 (P-series implementation, paused), ka-0000007565 (transitivity design, paused), ka-0000003639 (transitivity implementation, in-progress) all rebase onto Option B writes. Each gets a follow-up bead summarizing what carries over (the dibs rule, the column on `mutation_history`, the view) vs. what gets rewritten (every TEXT-string write site).
8. **R10 supersession housekeeping.** Mark ka-0000001224 ("approve-full" of Option A) as superseded with a comment pointing to this ADR + umbrella bead.

---

## 8. References

- `docs/adr/human-attribution-identity.md` — superseded R2 ADR (Option A).
- `docs/adr/human-attribution-propagation.md` — R3 propagation ADR; unchanged by this pivot (the propagation models are independent of identity shape).
- `kbeads/internal/store/postgres/migrations/012_human_attribution.up.sql` — current TEXT schema.
- `kbeads/internal/store/postgres/migrations/014_operated_by_dibs.up.sql` — dibs view + indexes layered on TEXT.
- `kbeads/internal/server/audit.go` — current resolver (`operatedByFromContext`, `resolveOperatedBy`, `operatedByFromAgentBead`).
- `kbeads/internal/store/postgres/dibs.go` — `GetBeadOperator` / `UpdateBeadOperator` TEXT queries that flip to FK.
- `kbeads/cmd/kd/admin_backfill_operated_by.go` — current backfill command shape; renamed and re-targeted in §5.4.
- `gardenio/controller/internal/bridge/bot.go:779`, `bot_commands.go:48,530`, `bot_mentions.go:35`, `bot_reactions.go:91` — Slack `WithOperator` callsites.
- `gardenio/controller/internal/bridge/gitlab_mentions.go:175,295`, `gitlab_sync.go:91,234,375,671,713,801`, `gitlab_agent_resolver.go:225-229` — GitLab callsites.
- `gardenio/controller/internal/bridge/jira_mentions.go:193` — Jira callsite.
- `gardenio/controller/internal/bridge/bot_human_survey_rating.go:79-113` — Slack email cache that seeds the v2 alias data.
- Umbrella bead: ka-0000003625.
- Scope checklist: ka-0000005927 on decision ka-0000001784.
- Superseded R10 decision: ka-0000001224.
