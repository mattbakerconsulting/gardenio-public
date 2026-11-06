# ADR: Human-attribution identity shape (`operated_by`)

- **Bead:** ka-0000001306 (R2 of ka-0000003603)
- **Status:** Proposed — research only, no schema/code lands until ka-0000003603 R10 resolves
- **Author:** gardenio-r2-identityshape-adr-ayu
- **Date:** 2026-04-29
- **Supersedes:** none
- **Related:** R1 inventory (ka-0000001488, in-progress); R3 propagation (ka-0000005586, queued); parent plan `docs/research/human-attribution-operated-by.md`

---

## 1. Context

The parent plan (`docs/research/human-attribution-operated-by.md`, Section 3 question 1) frames the question this ADR resolves:

> Identity shape. Is `operated_by` a `TEXT` (free-form, like `created_by`)? A foreign key into a new `humans` / `operators` table? An opaque IdP subject?

Today every audit-shaped column on every bead-related table is `TEXT` populated server-side from the authenticated identity. There is **no first-class link from a bead to the human operator** who caused it to exist; the only human-aware datum in the system is the raw Slack ID stuffed into `Fields["slack_user_id"]` on the agent bead at spawn (`gardenio/controller/internal/bridge/bot_commands.go:116`, `:531`; `bot_concierge.go:197`; `bot_agent_kill.go:199`).

Before introducing a new column it is worth being explicit about how the existing identity columns behave so we do not accidentally diverge. The list below is the canonical set of audit fields we are extending alongside, with file:line citations.

| Surface | Column / field | Type | Reference |
|---|---|---|---|
| `beads.created_by` | TEXT | server-set | `kbeads/internal/store/postgres/migrations/001_initial.up.sql:14` |
| `beads.closed_by` | TEXT | server-set | `kbeads/internal/store/postgres/migrations/001_initial.up.sql:17` |
| `beads.owner` / `beads.assignee` | TEXT | server-set | `kbeads/internal/store/postgres/migrations/001_initial.up.sql:11-12` |
| `deps.created_by` | TEXT | server-set | `kbeads/internal/store/postgres/migrations/001_initial.up.sql:42` |
| `comments.author` | TEXT | server-set | `kbeads/internal/store/postgres/migrations/001_initial.up.sql:50` |
| `events.actor` | TEXT | server-set | `kbeads/internal/store/postgres/migrations/001_initial.up.sql:68` |
| `Bead.created_by` (proto) | string field 13 | wire | `kbeads/proto/beads/v1/types.proto:21` |
| `Bead.assignee` / `owner` (proto) | string fields 10, 11 | wire | `kbeads/proto/beads/v1/types.proto:18-19` |
| `Dependency.created_by` (proto) | string field 5 | wire | `kbeads/proto/beads/v1/types.proto:38` |
| `Comment.author` (proto) | string field 3 | wire | `kbeads/proto/beads/v1/types.proto:46` |
| `Event.actor` (proto) | string field 4 | wire | `kbeads/proto/beads/v1/types.proto:56` |
| `actorFromContext` | server-derived TEXT | trust boundary | `kbeads/internal/server/audit.go:47-49` |
| Static-token override | `X-Actor` header / `x-actor` gRPC metadata | client-supplied; sanitized before write | `kbeads/internal/server/audit.go:51-69` |
| CLI `--actor` / `KD_ACTOR` | env / flag | client | `kbeads/cmd/kd/main.go:18`, `:25` |

The trust model worth preserving verbatim: `actorFromContext` is the single point that resolves audit identity from the authenticated context, and it deliberately ignores client-supplied actor strings except for static-token callers — see the comment at `kbeads/internal/server/audit.go:33-46`. **Whatever shape we pick for `operated_by`, the server-side write path must hook into the same function, not bypass it.**

The R3 ADR will decide propagation (inherit-from-spawning-agent vs per-request header vs session-scoped). This ADR must not foreclose any of those — the chosen identity type has to be settable by all three propagation models without changing what is stored on the row.

---

## 2. Operator types we must represent

The plan calls out three first-class operator types that the chosen identity shape must handle (Section 3 question 5 of the parent doc, restated as concrete examples). Any option below is judged in part on how cleanly it represents each one:

1. **Slack human** — a person who typed in Slack. The bridge already has the raw Slack user ID `U…` (see `bot_commands.go:116`). Display name is resolved separately by `resolveSlackUser()` in the bridge.
2. **Direct-CLI human** — a person running `kd` from a laptop, authenticated by an OIDC bearer token. The server already pulls the email from the OIDC subject via the `IdentityVerifier` path (`kbeads/internal/server/audit.go:75-84`).
3. **Scheduled / autonomous agent with no human operator** — e.g. `sched-gardener-cycle-watchdog-…`. There is no human to attribute to. The chosen shape must let us record this fact unambiguously (not just "NULL — nobody knows", which is also a real state during backfill).

A fourth, less common case worth keeping in mind: **agent-on-behalf-of-system-account** (e.g. a controller-internal token operating as `fleet:gardenio`). This already shows up in `AuthIdentity.Type == "fleet"` (`kbeads/internal/server/audit.go:8-15`); it is functionally a sub-case of (3) — no human operator.

---

## 3. Options

### Option A — Free-form TEXT, same shape as `created_by`

Add `operated_by TEXT` (and likely `operated_by_display TEXT` for denormalized display info) on every audit-bearing table; populate server-side from `actorFromContext` extended with a parallel "operator" resolver. Stored values are stable opaque IDs from the originating identity provider, with a sentinel for the no-operator case.

**Concrete stored values:**

| Operator type | `operated_by` | `operated_by_display` |
|---|---|---|
| Slack human (Matt Baker, Slack `U0OPERATOR2`) | `slack:U0OPERATOR2` | `mbaker` |
| Direct-CLI human (OIDC email) | `oidc:matt@acme.example` | `matt@acme.example` |
| Scheduled agent, no operator | `system:scheduler` | `(empty)` |
| Fleet/system token | `fleet:gardenio` | `(empty)` |
| Pre-cutover bead (backfill blank) | `''` (empty string, matches existing TEXT-default convention) | `(empty)` |

The `<provider>:<subject>` prefix is the existing convention used for OIDC subjects elsewhere in the bridge and avoids ambiguity between "Slack U-ID `Uxxx`" and a hypothetical OIDC sub that happens to look like one. It costs four bytes per row and keeps the TEXT searchable with prefix matches (`operated_by LIKE 'slack:%'`).

**Storage shape:** `operated_by TEXT NOT NULL DEFAULT ''` on `beads`, `events`, `comments`, `deps` (R3 may narrow this to a subset; not foreclosed). One b-tree index on `beads.operated_by` plus a partial index `WHERE operated_by <> ''`; no per-row FK.

**Pros:**
- Mirrors `created_by` exactly — same DDL, same nullability convention (empty string default), same proto field type. No new join in any existing query.
- Backfill is a `UPDATE … SET operated_by = …` — no rowset constraint.
- Sentinels (`system:scheduler`, `fleet:gardenio`) compose with the same column; no "this column is NULL because of FK semantics" branch.
- `kd list -f operated_by=slack:U0OPERATOR2` is a single column predicate; the existing `Filter` machinery (`kbeads/internal/model`) only needs a new field.
- Does not foreclose adding a `humans` lookup table later for display/email/avatar — that table can be a side-table joined opportunistically by beads-viewer without changing what is *stored on the bead*.

**Cons:**
- Display info is denormalized; refreshing Matt Baker's email or display-name globally requires either backfill or always joining a side table.
- No referential integrity — typos / drift would not be caught by the DB. Mitigated by the fact that the value is always server-set from `actorFromContext`, never written by clients.
- Doesn't enforce that any particular ID space is well-formed; we rely on `sanitizeActorHeader` (`kbeads/internal/server/audit.go:58-69`) and the OIDC verifier for hygiene.

### Option B — FK to a new `humans` / `operators` table

Add tables `operators(id BIGSERIAL PRIMARY KEY, kind TEXT, subject TEXT, display TEXT, email TEXT, …)` plus `operator_aliases(operator_id, kind, subject)` for the Slack-ID ↔ OIDC-email mapping. Replace `operated_by TEXT` with `operated_by_id BIGINT REFERENCES operators(id)` on every audit table.

**Concrete stored values:**

| Operator type | Row in `operators` | `operated_by_id` |
|---|---|---|
| Slack human | `(id=42, kind='slack', subject='U0OPERATOR2', display='mbaker', email='matt@acme.example')` | `42` |
| Direct-CLI human | same row 42 (linked via `operator_aliases`) | `42` |
| Scheduled agent | `(id=1, kind='system', subject='scheduler', display='', email=NULL)` (sentinel pre-seeded) | `1` |
| Pre-cutover bead | NULL | NULL |

**Pros:**
- Single source of truth for operator metadata. Updating Matt's email is one row.
- Cross-provider unification: one `operators` row, multiple `operator_aliases` (Slack `U…`, OIDC email, future Teams/Discord), so a Slack-triggered bead and a CLI-triggered bead by the same person resolve to the same operator. This is the strongest argument for FK.
- Referential integrity: a bead cannot reference a non-existent operator.
- Natural home for future fields (avatar URL, tenant ID, deactivation flag) without bloating bead rows.

**Cons:**
- Big migration: every audit-bearing table grows a column AND every row needs a write to a new operators table. The plan's R7 backfill cost goes up linearly because we now create operators-table rows in addition to populating `operated_by_id`.
- Every list/filter query needs a join to surface a human-readable identity. `kd list` adds a join. SSE event payloads need to either denormalize the join or expose only the FK and force consumers to join.
- `Fields["slack_user_id"]` data is mostly `U…` strings without an OIDC link; the first time we fold those into an `operators` row, we'd be guessing whether two Slack users with no OIDC alias are the "same human." That's a conflation problem: the FK shape pushes us to make that decision up-front, even though we don't yet have authoritative data.
- Coupling problem: every place today that writes a TEXT actor (`actorFromContext` callers, SSE publishers, mutation-history rows in `008_mutation_history.up.sql`) has to learn about operators-table semantics. We are picking up R3, R4, R5, R6 work for this ADR.
- Schema is harder to evolve: changing the operator key from "Slack ID" to "Slack workspace + ID" later means migrating every dependent row.

### Option C — IdP-subject-only (always OIDC sub)

Always store the OIDC subject — a single canonical identity space. `operated_by TEXT NOT NULL` containing the OIDC `sub` claim (or email if that's what `IdentityVerifier` returns).

**Concrete stored values:**

| Operator type | `operated_by` |
|---|---|
| Slack human | requires Slack-ID → OIDC-sub resolution at ingest. No-op if the user has never logged in via OIDC; we'd have to invent a fake OIDC sub or fall back to a sentinel (defeats the purpose). |
| Direct-CLI human | `matt@acme.example` (or OIDC sub) |
| Scheduled agent | sentinel (`system:scheduler`) — but now we have a column where most values are OIDC subs and the scheduler one isn't, which is exactly the discriminator-prefix problem Option A solves with the `<provider>:` prefix. |
| Pre-cutover bead | NULL or empty — depends on whether we can resolve historic Slack IDs to OIDC subs (often no). |

**Pros:**
- One ID space, in theory globally unique and stable.
- Works trivially for the direct-CLI case — that identity is *already* the OIDC sub today.

**Cons (the deal-breakers):**
- The Slack-bridge case currently has the Slack ID and **does not have the OIDC sub** at spawn time (`bot_commands.go:116` writes `slack_user_id` directly). Forcing an OIDC mapping at spawn means either:
  - the bridge has to call an IdP lookup endpoint at spawn time (new dependency, new failure mode, unclear what to do when the user has never logged in via OIDC), or
  - we drop attribution entirely for un-mapped users (worst possible behavior — the rule is *the user typed a thing in Slack and we lost track of them*).
- "OIDC sub or sentinel" collapses back to Option A but with a worse default: most Slack users will never authenticate via OIDC.
- Forecloses any future federation against Teams/Discord/etc. until we build IdP federation first — Option A representations would still work.

---

## 4. Decision

**Recommend Option A — free-form TEXT with stable opaque IDs and provider prefixes.**

Rationale, in priority order:

1. **It is the smallest possible delta from `created_by`.** R3 has not yet decided propagation. Whatever propagation model wins, "set a TEXT column from `actorFromContext`" works without new infrastructure. An FK change locks in not just the identity shape but also an operator-table API surface — too much for one ADR to commit to.
2. **It does not foreclose Option B as an evolution.** The natural upgrade path is: keep `operated_by TEXT`, *add* an `operators` lookup table later as a side-relation keyed on `(provider, subject)`. Reads that need rich operator metadata join opportunistically; reads that only need attribution use the column directly. This is the inverse of starting with FK and trying to remove it later, which is a real migration.
3. **It handles the no-operator case cleanly with a sentinel** (`system:scheduler`, `fleet:gardenio`). Option B forces us to either pre-seed sentinel rows or use NULL with special meaning; both are worse than a typed-prefix string.
4. **It matches the trust model of the existing column.** `created_by` is server-set TEXT from `actorFromContext`. `operated_by` should be too — a parallel server-side resolver that sits next to `actorFromContext` and reads the same authenticated context. No new spoofing surface.
5. **The display-info concern is real but solvable without an FK.** Add an optional `operated_by_display TEXT` column denormalized at write time, refreshed on a periodic Slack-name resync (the bridge already calls `resolveSlackUser()`). Stale display strings are an acceptable trade-off for an audit field that prioritizes "who actually did this" over "what is their current display name."

The cross-provider unification benefit of Option B (one operator, many aliases) is the real argument against Option A. The counter-argument: today we don't have authoritative cross-provider mappings; inventing them in v1 risks conflation. The right time to add an `operators` table is **after** we have a working `operated_by TEXT` column populated for some months, when we can audit who has multiple identities and consolidate with confidence.

---

## 5. Consequences

### Storage

- One new TEXT column on `beads`, `events`, `comments`, `deps` (R4 will refine — possibly only `beads` + `events`). At ~32 bytes typical (`slack:U0OPERATOR2`, ~17 chars; OIDC emails commonly under 40), the per-row overhead is negligible vs. existing `created_by`/`actor` columns of the same shape.
- Optional second `operated_by_display TEXT` column on `beads` only, denormalized for UI use. Empty string default; refreshed by a future Slack/IdP resync job.
- One b-tree index on `beads.operated_by`. Selectivity is high enough (per-human filtering is the main use case) that a partial index `WHERE operated_by <> ''` is reasonable to keep the index small post-backfill.

### Query ergonomics

- `kd list -f operated_by=slack:U0OPERATOR2`: single column predicate, parallel to existing `--owner` / `--assignee` filters in the existing model `Filter` (`kbeads/internal/model`).
- "All beads operated by anyone" / "all beads operated by no one": `WHERE operated_by <> ''` and `WHERE operated_by = ''` respectively. Sentinel-aware filters (`operated_by LIKE 'system:%'`) are trivial.
- SSE event payloads grow by one TEXT field; consumers that don't care can ignore it; consumers that do (beads-viewer) can render it directly without a fetch.

### Denormalization cost

- `operated_by_display` is denormalized. Stale display names are visible in old beads if a user changes their Slack display name. Acceptable — this is an audit field, not a profile lookup. Beads-viewer can fall back to the live Slack name service when the bead is open in the UI.
- If we later add an `operators` side-table, beads-viewer can prefer a join lookup and treat `operated_by_display` as a fallback for offline / batch contexts.

### Future tenant scoping

- Provider-prefix scheme (`slack:`, `oidc:`, `system:`, `fleet:`) extends naturally to a tenant prefix (`tenant:acme/slack:U…`) when multi-tenant isolation lands. No schema change required to support that — it's a string-format extension.
- An FK upgrade later can layer on top: `operators(id, tenant_id, kind, subject, …)` with `operated_by_id` added alongside `operated_by` and the latter retained for backwards compat / audit. This is strictly easier than removing an FK.

### Trust / audit

- `operated_by` writes follow the same path as `created_by`: server-resolved from authenticated context, never client-supplied. R3 / R5 will decide whether the resolver reads from a spawn-time bead field (inherit) vs. a per-request header (sanitized like `X-Actor`) vs. a session env var. Option A is compatible with all three because in every case the resolver is server-side and the column is TEXT.
- Spoofing surface is identical to `created_by`'s. The only new client-surface in propagation models that sends a header (model b) would be sanitized through the same `sanitizeActorHeader` shape (`kbeads/internal/server/audit.go:58-69`), and would still only be honored for static-token callers — same rule as today.

### Backfill (R7-bound; not decided here)

- Trivial in shape: `UPDATE beads SET operated_by = 'slack:' || (fields->>'slack_user_id') WHERE fields ? 'slack_user_id' AND operated_by = ''`. Recursive descent for child beads is the harder part and belongs in R7. The shape choice does not change that complexity; an FK shape would add operator-table inserts on top.

### What this does NOT decide

- **Which tables get the column.** Likely `beads` + `events`; possibly `comments` and `deps`. Defer to R4.
- **How values flow from operator → agent → child beads.** Defer to R3.
- **Backfill correctness.** Defer to R7.
- **Beads-viewer UI presentation.** Defer to R8.

---

## 6. Recommendation interaction with R3

R3 will pick one of:

- (a) **Inherit from spawning agent** — server reads spawning agent's bead `operated_by` and copies. Works with TEXT directly: read column, write column.
- (b) **Per-request header** — agent sends `X-Operated-By`. Works with TEXT: sanitize, write.
- (c) **Session-scoped** — env var on agent pod, CLI wrapper attaches. Devolves to (b) at the wire level. Works with TEXT.

Each model's wire and write logic is identical regardless of FK vs TEXT — but FK adds a "look up operator id, insert if missing" step on every audit write that TEXT does not. That extra step is what locks in conflation decisions early. Option A keeps R3 cheap.

---

## 7. Open follow-ups

These belong to other research beads, not this ADR; surfaced here so they aren't lost:

- **R4 (schema sketch, kd-…):** define exact migration DDL, index plan, NOT NULL vs default-empty-string per column; decide whether `comments` and `deps` get the column or inherit via parent bead.
- **R6 (API surface):** add `operated_by` to proto `Bead` / `Comment` / `Event` (new field numbers), add filter to gRPC `ListBeads`, add `--operated-by` to `kd list`. New filter field should accept a provider-prefixed value like the column.
- **R7 (backfill):** the recursive-descent algorithm for propagating an agent bead's `slack_user_id` to its descendants. Bound by R3 — if propagation is "inherit from spawning agent", backfill is the same algorithm running over historic data.
- **R10 (decision checkpoint):** gardener packages this ADR + R3 + R4 as a `gb decision` for human approval.

A future ADR (post-R10, post-implementation) should revisit FK introduction once we have ~3-6 months of `operated_by` data and can audit cross-provider conflation safely.

---

## 8. References

- `docs/research/human-attribution-operated-by.md` — parent plan, especially Section 3 question 1, Section 4 R2, Section 6 file-line index.
- `kbeads/internal/store/postgres/migrations/001_initial.up.sql` — beads/comments/deps/events table definitions.
- `kbeads/internal/server/audit.go` — `actorFromContext` (`:47-49`), `sanitizeActorHeader` (`:58-69`), `verifyAndIdentify` (`:75-84`), `AuthIdentity` shape (`:8-15`).
- `kbeads/proto/beads/v1/types.proto` — `Bead.created_by` (`:21`), `Comment.author` (`:46`), `Event.actor` (`:56`), `Dependency.created_by` (`:38`).
- `kbeads/cmd/kd/main.go:18,25` — CLI `--actor` / `KD_ACTOR` resolution.
- `gardenio/controller/internal/bridge/bot_commands.go:116,531`, `bot_concierge.go:197`, `bot_agent_kill.go:199` — places that write `Fields["slack_user_id"]` on agent beads at spawn.
- `gardenio/controller/internal/bridge/bot_decisions.go:37,48` — read site for `slack_user_id` (decision routing). Today this is the only attribution use.
