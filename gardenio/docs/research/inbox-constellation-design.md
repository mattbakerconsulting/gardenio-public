# Inbox-constellation design

> **⚠ Superseded by [`per-user-homepage-design.md`](per-user-homepage-design.md)
> (ka-0000003587 M3 fold).** `/inbox` is now an HTTP-308 redirect to
> `/u/me?view=dashboard`; the legacy dashboard is reachable via
> `/u/me?view=constellation`. Kept for historical context; new design
> decisions belong in the per-user-homepage doc.

> **Locked to research/design.** This doc accumulates section-by-section
> drafts under the parent epic [`ka-0000006157`](#) — *"inbox-primary
> constellation view (intake-by-user index → drill to per-intake
> constellation)"*. No routes, endpoints, view-model code, or templates
> land until the operator green-lights this doc.
>
> Round-by-round rationale and bridge-source tables live in the parent
> review doc: [`escalation-beads-review-2026-05-05.md`](escalation-beads-review-2026-05-05.md)
> (rounds 8–10).

The eight sections below mirror the subtask split on `ka-0000006157`. Each
subtask agent appends its own section to this file; the structure is
otherwise empty until those drafts land.

1. **Index predicate** — what does *"intake beads operated by the current
   user"* mean? (ka-0000001371, drafted below.)
2. *Constellation subgraph* — what does *"the constellation a bead is
   part of"* expand to? (ka-0000004482.)
3. **Layout for an intake-rooted subgraph** — concentric rings by depth
   ÷ category as angular sector. (ka-0000003739, drafted below.)
4. **Index UI** — left-rail column shell, filter chips, default sort,
   keyboard model, missing-identity banner. (ka-0000001406, drafted below.)
5. **Interaction surface** — peek, flag, reparent, resolve decision,
   comment; reuse-vs-new endpoint table; keyboard model; multi-select
   deferred; undo via 30 s toast. (ka-0000002076, drafted below.)
6. **Action beads not under an intake** — pseudo-intake row in the
   index for orphan decisions / escalations. (ka-0000001922, drafted
   below.)
7. **Performance / data shape** — reuse vs. new endpoint matrix, query
   cost on the 22-bead fixture + a stress fixture, caching strategy.
   (ka-0000006979, drafted below.)
8. **Relationship to existing constellation + promotion** —
   `/inbox` as the new route (with `/operator-inbox` as a permanent
   alias), `/` unchanged at v1, four-leg promotion bar (design,
   K-day proof-in-use, perf, operator sign-off), `/active` stays,
   `/decisions` folds at promotion, `/{id}` constellation unchanged
   forever. (ka-0000001339, drafted below.)

---

## 1. Index predicate

### 1.1 Problem statement

The inbox-primary view's left rail is an index of *intake beads operated
by the current user*. The query that backs that index is, in shape,
trivial: `category=intake AND operated_by-matches-viewer`. The hard part
is the second clause — how a single human's identity expands into the
set of `operated_by` values that count as "me", given that bridges stamp
platform-prefixed strings (`slack:U0OPERATOR1`, `gitlab:matthewbaker`,
`jira:abc123`, `oidc:matt@…`) at create time.

Round 9 of the parent review framed this as an application-layer
`IdentityResolver` that expands a `user_identity` bead into a set of
operator id strings and IN-matches against `beads.operated_by TEXT`.
Round 10 superseded that: the operator pointed at
[`ka-0000003625`](#) (Pivot operated_by to Option B) and ADR
[`docs/adr/human-attribution-identity-v2.md`](../../../docs/adr/human-attribution-identity-v2.md)
(merged via MR !2103) as the substrate. Under Option B the expansion
collapses into a single FK lookup against the new `operators` table.

This section specifies the predicate at two stops on that timeline:

- **v1** (post Option B P1+P2) — the clean form, single FK equality.
- **v0.5** (pre Option B) — a throwaway interim against
  `user_identity` beads, kept only if we ship the inbox before P2
  reaches the bridges.

### 1.2 v1 predicate (post Option B P1+P2)

#### 1.2.1 Shape

```sql
SELECT b.*
FROM   beads b
WHERE  b.category = 'intake'
  AND  b.operated_by_id = :viewer_operator_id
ORDER  BY <ranking>          -- subtask 4 fills in the sort
LIMIT  :page_size;
```

`category=intake` is derived per `DeriveCategory` (D7 plan
[`ka-0000003787`](#)) and is **not** affected by the identity-shape
pivot. `operated_by_id` is the FK column added by Option B's migration
015 (`docs/adr/human-attribution-identity-v2.md` §3.2):

```sql
ALTER TABLE beads ADD COLUMN operated_by_id BIGINT REFERENCES operators(id);
```

`:viewer_operator_id` is a single `BIGINT`, resolved once per request
(see §1.4). The query is one indexed equality on a FK column — the
cheapest shape Postgres can serve.

#### 1.2.2 Index requirement

The ADR already specifies the index this query reads from
(`docs/adr/human-attribution-identity-v2.md` §3.2):

```sql
CREATE INDEX idx_beads_operated_by_id
    ON beads(operated_by_id)
    WHERE operated_by_id IS NOT NULL;
```

For the inbox surface specifically, we additionally want an index that
covers the `(operated_by_id, category)` access path so the query plan
doesn't require a heap scan to filter on category after the FK lookup:

```sql
CREATE INDEX idx_beads_inbox_predicate
    ON beads(operated_by_id, category)
    WHERE operated_by_id IS NOT NULL
      AND category = 'intake';
```

The partial index keeps the index small (only intake rows that have an
operator), which is the working set the inbox actually queries. Whether
this index lands as part of Option B P1, or as a separate inbox-side
migration, is a sequencing decision deferred to subtask 7
(performance / data shape). The predicate above is correct either
way; only the plan changes.

#### 1.2.3 Why the FK collapses Round 9's resolver

Under Option B, cross-platform unification is materialised at
*write* time, not read time. When the Slack bridge creates an intake
bead under Matt's `slack:U0OPERATOR1`, the resolver looks up
`(slack, U0OPERATOR1)` in `operator_aliases`, gets back operator id
`42`, and writes `operated_by_id = 42`. When the same Matt later runs
`kd create` over OIDC, the resolver looks up `(oidc, matt@…)` in
`operator_aliases`, gets back operator id `42` again, and writes
`operated_by_id = 42`. Both intake beads have the same FK; one
equality predicate finds them both.

The Round-9 application-layer `IdentityResolver` (expand viewer's
`user_identity` bead → set of strings → `IN` match) is doing the same
work, but at *every* read, against `operated_by TEXT`. Option B does
the work once at create. The inbox query no longer needs an
expansion step.

### 1.3 v0.5 predicate (pre Option B)

This branch is kept only as a *throwaway* path: it lets the inbox
surface ship before Option B P1+P2 land in production. The v1 swap is
mechanical — the application-layer expansion is replaced with a single
FK lookup. Nothing else in the inbox view changes. If Option B is
already in production by the time we begin implementation work, this
branch never ships.

#### 1.3.1 Shape

```sql
SELECT b.*
FROM   beads b
WHERE  b.category = 'intake'
  AND  b.operated_by = ANY($1::text[])  -- $1 = viewer's identity_ids
ORDER  BY <ranking>
LIMIT  :page_size;
```

`ANY($1::text[])` is the form already used elsewhere in `kbeads` for
"value is in this set" predicates. The text array is the post-resolution
set of operator ids that map to the current viewer.

#### 1.3.2 Identity expansion

`viewer.identity_ids` is computed application-side by an
`IdentityResolver` primitive (Round 9 §"Why this re-framing matters
beyond D8" of the review doc). The resolver:

1. Takes the OIDC subject (email) from the request context.
2. Looks up the viewer's `user_identity` bead (one bead per human,
   linking `email` ↔ `slack_id` ↔ `gitlab_username` ↔ `jira_user`).
3. Returns the set of platform-prefixed strings:
   `{"oidc:matt@…", "slack:U0OPERATOR1", "gitlab:matthewbaker",
   "jira:abc123"}`.
4. When the bead is unset or only contains the OIDC email, returns the
   single-element set `{"oidc:<email>"}`. (Subtask 4 wires the
   missing-identity banner that prompts `gb identity add` here.)

The set is small (≤ ~5 entries today; one per platform per human).
Postgres handles `ANY(text[])` well at this cardinality; we do not need
a more elaborate join.

#### 1.3.3 Index requirement

`beads(operated_by)` already has the partial index from migration 012
(`12_human_attribution.up.sql`):

```sql
CREATE INDEX idx_beads_operated_by
    ON beads(operated_by)
    WHERE operated_by IS NOT NULL;
```

For the inbox-specific access path, an additional partial index on
`(operated_by, category)` would help the same way the v1 partial does:

```sql
CREATE INDEX idx_beads_inbox_predicate_v05
    ON beads(operated_by, category)
    WHERE operated_by IS NOT NULL
      AND category = 'intake';
```

Whether to land this index for the v0.5 lifetime depends on (a) how
long v0.5 actually lives and (b) whether the existing partial is
sufficient at the data sizes we have now. This is a sequencing call,
not a correctness one — defer to subtask 7.

#### 1.3.4 Resolver caching

The expansion is per-request input → small string set. The resolver
should cache by OIDC subject for the lifetime of the
`user_identity` bead's `updated_at` (i.e. invalidate on bead-update
SSE). Cache miss is one bead read; cache hit is a string-set lookup.
Resolver lives next to `operator_chip.go` so the inbox view, `/active`,
`kd list --operated-by=me`, and the operator chip all consume the same
primitive. See §1.4.

### 1.4 Identity-resolution call site

Both v0.5 and v1 need to compute "who is the viewer, expressed as the
right shape for the predicate" once per request. The shape differs:

| Version | Output                                              | Resolver returns                                   |
| ------- | --------------------------------------------------- | -------------------------------------------------- |
| v1      | `viewer_operator_id BIGINT` (one row in `operators`) | Single FK lookup via `operator_aliases(oidc, email)` |
| v0.5    | `viewer.identity_ids []string`                       | Application-layer expansion of `user_identity` bead |

#### 1.4.1 Where it lives

The reference identity primitive in tree today is the operator chip
helper in
[`gardenio/controller/cmd/beads-viewer/operator_chip.go`](../../controller/cmd/beads-viewer/operator_chip.go).
It already:

- Resolves `?operated_by=me` against the OIDC viewer email
  (`resolveOperatedByParam`, line 120).
- Applies a default-on `oidc:<email>` filter on `/active`
  (`resolveDefaultOperatedBy`, line 155).
- Compares an `operated_by` id against the viewer email
  (`operatorIDsMatchViewer`, line 378).

The inbox view should **delegate** to this primitive, not reinvent
identity resolution. Concretely:

- The HTTP handler that serves the inbox index calls a viewer
  resolver once at request entry.
- v1: the resolver returns the viewer's operator id by joining
  `operator_aliases (kind='oidc', subject=email)`. The resolver lives
  alongside `operator_chip.go` and is shared with `/active` once
  Option B P2 lands (today `/active` works against `operated_by TEXT`
  via the `oidc:<email>` string).
- v0.5: the resolver expands the `user_identity` bead into the
  `[]string` set. Same call site; different return type.

The viewer-resolution primitive is the same lever Round 9 named: the
new constellation inbox, `/active`, `kd list --operated-by=me`, and
the operator chip all consume it. We add it here in §1.4 because the
inbox surface is the first new caller; it is *not* an inbox-only
helper.

#### 1.4.2 Caching

- **v1.** `(oidc_subject) → operator_id` is stable for the lifetime of
  the user's `operator_aliases` row. Cache by OIDC subject; invalidate
  on `operators.updated_at` change (or invalidate on every `gb identity
  add` upsert via SSE). Cache miss = one indexed lookup against
  `operator_aliases(kind, subject)` (PK).
- **v0.5.** `(oidc_subject) → []string` is stable for the lifetime of
  the user's `user_identity` bead. Cache by OIDC subject; invalidate
  on `user_identity` bead-update SSE. Cache miss = one bead read.

Both caches live in the controller process (in-memory map keyed by OIDC
subject, TTL = 60 s plus invalidation on relevant SSE events). No
separate cache service.

### 1.5 Sentinel handling — `system:*` and `fleet:*` must not appear in inbox

The system and fleet sentinels (`system:scheduler`,
`system:gitlab-sync`, `fleet:gardenio`, …) represent autonomous /
non-human writers. Under Option B they are first-class rows in
`operators` with `kind ∈ {'system', 'fleet'}` and pre-seeded ids
1–8 (per ADR §3.1). Under v0.5 they are TEXT strings prefixed
`system:` / `fleet:`.

The inbox is *the human's intake queue*. Sentinel-operated intake is
either (a) cron-driven gardener churn that never warranted a human's
attention or (b) bridge book-keeping. Both are explicitly out of scope
for this view.

The predicate handles this without a separate filter clause **as long
as the resolver only ever returns human operator ids**:

- **v1.** `operators` rows for sentinels have `kind ∈ {'system',
  'fleet'}`. The viewer-resolver looks up `(oidc, email)` and
  always returns a `kind='oidc'` operator (or one with an `oidc`
  alias). It can never return `id=1` (system:scheduler) or `id=8`
  (fleet:gardenio). `operated_by_id = :viewer_operator_id` therefore
  cannot match a sentinel-operated bead.
- **v0.5.** The expansion of `user_identity` returns
  `oidc:` / `slack:` / `gitlab:` / `jira:` strings only. There is no
  `user_identity` field that maps to `system:*` or `fleet:*`, so the
  IN-set never contains a sentinel string and the predicate cannot
  match a sentinel-operated bead.

So the inbox surface inherits sentinel exclusion from the resolver
contract, not from a `WHERE operated_by_id NOT IN (sentinel_ids)`
clause. That is the right place to enforce it: any future surface
that wants the same exclusion gets it for free by reusing the
viewer-resolver, and any surface that wants the opposite (e.g. an
audit view of "what did the scheduler raise this week") can build a
separate predicate without the resolver getting in the way.

**Test obligation.** A regression test must assert that an intake bead
with `operated_by_id = system:scheduler` (or v0.5 equivalent) does
*not* appear in the viewer's inbox query result, regardless of which
viewer is signed in. Subtask 7 owns that test fixture.

### 1.6 Cross-platform unification — worked example

The motivating use case for Option B is *"show me everything Matt
did, regardless of which surface he used"*. This is the predicate's
acceptance criterion: a Slack-triggered intake and an OIDC-CLI-triggered
intake by the same human MUST land on the same row.

#### 1.6.1 Setup

Matt has three platform identities the system has stamped at various
points:

- Slack user id `U0OPERATOR1`
- OIDC subject (email) `matt@acme.example`
- GitLab username `matthewbaker`

#### 1.6.2 Under Option B (v1)

`operators`:

| id  | kind   | subject              | display       | email                |
| --- | ------ | -------------------- | ------------- | -------------------- |
| 42  | oidc   | matt@acme.example     | Matt Baker    | matt@acme.example     |

`operator_aliases`:

| operator_id | kind   | subject               | source        |
| ----------- | ------ | --------------------- | ------------- |
| 42          | oidc   | matt@acme.example      | oidc-login    |
| 42          | slack  | U0OPERATOR1           | migration     |
| 42          | gitlab | matthewbaker          | manual        |

When the Slack bridge creates intake bead `ka-A111` under Matt's
Slack mention, the resolver looks up `(slack, U0OPERATOR1)` in
`operator_aliases`, gets `operator_id = 42`, and writes
`beads.operated_by_id = 42`.

When Matt later runs `kd create` over OIDC and creates intake bead
`ka-B222`, the resolver looks up `(oidc, matt@acme.example)` in
`operator_aliases`, gets `operator_id = 42` again, and writes
`beads.operated_by_id = 42`.

The viewer-resolver, given Matt's OIDC subject, returns
`viewer_operator_id = 42`. The inbox query:

```sql
SELECT * FROM beads
WHERE  category = 'intake'
  AND  operated_by_id = 42;
```

returns both `ka-A111` and `ka-B222`. One row in `operators`, one FK
equality, both intake beads found.

#### 1.6.3 Under v0.5 (interim)

`user_identity` bead for Matt (one bead per human):

```yaml
email:           matt@acme.example
slack_id:        U0OPERATOR1
gitlab_username: matthewbaker
```

The same Slack bridge writes `beads.operated_by = "slack:U0OPERATOR1"`
on `ka-A111`. The OIDC CLI writes
`beads.operated_by = "oidc:matt@acme.example"` on `ka-B222`.

The application-layer `IdentityResolver`, given Matt's OIDC subject,
expands the `user_identity` bead into:

```
identity_ids = [
  "oidc:matt@acme.example",
  "slack:U0OPERATOR1",
  "gitlab:matthewbaker",
]
```

The inbox query:

```sql
SELECT * FROM beads
WHERE  category = 'intake'
  AND  operated_by = ANY($1::text[]);  -- $1 = identity_ids above
```

returns both `ka-A111` and `ka-B222`. The set has three elements; the
expansion is per-request; the predicate falls back to the existing
`idx_beads_operated_by` partial index.

#### 1.6.4 Note on the v0.5 → v1 swap

The application-layer expansion under v0.5 *exactly* mirrors what
`operator_aliases` does at write time under v1. The data the v0.5
resolver reads from `user_identity` is the same data Option B's
backfill migration walks to populate `operator_aliases` (per ADR §4 —
the `kd admin backfill-operators` command is driven by the same
union of distinct `operated_by` strings the v0.5 resolver expands).
The swap from v0.5 to v1 therefore preserves inbox membership for
every viewer who has a populated `user_identity` bead today.

### 1.7 What this section does NOT specify

Out of scope for the predicate; pinned for the right subtask:

- **The sort / ranking** (`ORDER BY` clause) — subtask 4 (index UI)
  picks the default sort and any toggle.
- **Pagination** — subtask 7 (performance / data shape).
- **Filter chips** for predicate variants (mine / team / on-call /
  raise-only) — subtask 4. This section names the *base* predicate
  only.
- **`kd list --operated-by=me`** CLI mirror — Round 9 noted this as a
  candidate 9th subtask. We do not specify the CLI here; the resolver
  primitive in §1.4 is the shared substrate it would consume.
- **Missing-identity banner** UX — subtask 4. §1.3 / §1.4 only specify
  the resolver behaviour when the `user_identity` bead is unset
  (return `{"oidc:<email>"}`); the banner copy and the prompt to run
  `gb identity add` is UI surface owned by subtask 4.

### 1.8 Open question for operator review

One sequencing question this section forces but does not resolve:

> **Do we ship the inbox surface against v0.5 first, or wait for
> Option B P2 and ship straight to v1?**

Tradeoff:

- **Ship v0.5 now.** Faster path to the inbox UX. Cost: an
  application-layer resolver and a query path we will replace within
  weeks. Risk: any divergence between v0.5 expansion and v1
  `operator_aliases` content shows up as inbox membership drift on
  the day we swap.
- **Wait for v1.** No throwaway code; predicate is correct on day 1.
  Cost: inbox UX waits on Option B P1+P2 sequencing.

Recommend: wait for v1, **unless** Option B P2 sequencing slips beyond
two weeks past the inbox UI being ready, in which case ship v0.5 with
an explicit deprecation plan tied to the swap. Operator decision.

---

## 2. "Constellation a bead is part of" — subgraph predicate

> *Owning bead: `ka-0000004482`. Parent: `ka-0000006157`.*

When the operator clicks an intake row in the index, what subgraph do
we render in the right pane? This section pins the predicate so the
layout work in §3 and the perf work in §7 have a stable input.

### 2.1 Anchor selection — stay at the clicked intake

**Rule:** the anchor of the rendered subgraph is the intake bead the
operator clicked. We do **not** walk further up.

Round-8 listed two candidates: (a) walk to the topmost intake/epic
ancestor, then expand N levels down; (b) stay at the clicked intake and
expand N levels down. We pick (b).

Rationale:

- The index is filtered to `category=intake`. By D7 taxonomy
  (`ka-0000003787`, `DeriveCategory`), an intake is *either* an
  external-source artifact (Slack/Jira/GitLab/email-bridged bead) *or*
  an epic that is **not parented by another epic/intake**. So an intake
  by construction has no intake/epic parent worth walking to. Any
  upward walk either (i) terminates at the same node, or (ii) crosses
  into a non-intake parent, which violates the index→constellation
  contract ("clicking an intake renders an intake-rooted graph").
- The operator picked a row. Re-rooting somewhere they did not pick is
  a bait-and-switch. If a near-by intake is actually the right anchor,
  it sits next to the clicked one in the index and is one keyboard
  press away.
- Ancestor walking on a non-tree (deps + mentions) graph is ill-defined
  in the presence of multi-parent beads (see §2.6). Anchoring at the
  click avoids the question.

**Edge case — clicked bead is mis-labelled `category=intake` but does
have an intake parent:** treat the click as authoritative. Render
rooted at the clicked bead. Surface the misclassification by making
the parent intake (if any) reachable as a "mentioned by" reverse-edge
node, not by re-rooting. This is the same dishonest-data principle the
existing constellation applies (`category` is server-derived; the
viewer trusts it).

### 2.2 Direction of expansion — reverse-deps (descendants) only at L1

The bead-show constellation in `constellation.go` expands in **both**
directions from the center: `Forward` = "this bead depends on" (parents
of the center) and `Reverse` = "depends on this" (children of the
center). For the inbox view, the intake is by definition the root of
its work; *parents* of the intake are ancestor noise, not work the
operator drives.

**v1 expansion contract:**

- L1 = reverse-deps of the intake (children) only. Skip the
  `Forward` arc.
- L2..N = reverse-deps of L1 nodes (and so on) — i.e. transitive
  descendants, walked outward via `expandOuterLevels` reverse path.
- Forward (parent-of-intake) edges are dropped from the SVG but kept
  in the hover preview (so an operator who clicked a sub-epic-by-
  mistake can see "this is parented by `epic:X`" without re-rendering).
- Untyped mentions and typed text mentions are folded in via the
  existing `mergeTypedMentionsAsDepEdges` /
  `appendUntypedMentionEdges` helpers, same as bead-show.

This is purely a `buildConstellation` parameter knob: skip the
forward-fetching block (lines ~286, ~291–304 in `constellation.go`)
and pass `reverse`-only tree expansion to `expandOuterLevels`. No
modification to existing functions; a sibling builder
`buildInboxConstellation` may take a `direction = reverseOnly` flag.

### 2.3 Default depth N and node cap K

**v1 defaults:**

| Parameter | v1 value | Reused constant |
| --- | --- | --- |
| Tree depth `N` | **2** | bounded by `constellationMaxTreeLevel = 3` (`constellation.go:31`) |
| L1 fanout (intake → children) | **6** | `constellationMaxReverse = 6` (`constellation.go:24`) |
| L2+ fanout (per-parent) | **3** | `constellationFanoutPerParent = 3` (`constellation.go:35`) |
| Hydrate timeout | **600 ms** | `constellationHydrateTime` (`constellation.go:25`) |
| Hydrate concurrency | **6** | `constellationHydrateWorker` (`constellation.go:26`) |

**Total node ceiling** at the v1 default:

```
K_v1 = 1 (intake)
     + constellationMaxReverse                           = 6   (L1)
     + constellationMaxReverse * constellationFanoutPerParent = 18  (L2)
     = 25 nodes
```

Slider raised to N=3 (the existing `constellationMaxTreeLevel` upper
bound):

```
K_max = 25 + (constellationMaxReverse * constellationFanoutPerParent^2)
      = 25 + 54
      = 79 nodes
```

**Sanity check against the current 22-bead operator queue.** Round-4
fixtures cite a 22-bead operator queue spanning ~12 distinct intakes,
i.e. an average of <2 children per intake. At N=2 the typical render
draws `1 + ~2 + ~2 = ~5` nodes — well under K_v1 = 25. The 25-node
ceiling absorbs the worst-case intake (a "release prep" epic with ~6
direct sub-tasks each owning 2–3 decisions/escalations) without
overflowing. Larger intakes hit overflow handling (§2.5), not the
hydrate timeout.

**Why N=2 and not N=3 for v1:**

- An intake's actionable triage horizon is intake → tasks → decisions/
  escalations. That is depth 2.
- N=3 grows the SVG viewBox to 1400×1100 (`constellationLayout` case 3
  at `constellation.go:579`). For an embedded right-pane it is too
  tall on a 1080p laptop — the operator scrolls past their decision
  buttons. N=2 fits 1200×800.
- N=3 is preserved as a slider option (re-uses `clampTreeLevel`), so
  the operator can opt-in for archeology without making it the default.

### 2.4 Edge case — anchor with empty descendants

Intake with `len(reverseDeps) == 0` (e.g. a Slack-bridged complaint
not yet triaged into a task) renders as **just the center node + a
hint banner**: `"This intake has no children yet — peek to triage."`
This re-uses the existing center-only render path; no new layout
needed.

### 2.5 Overflow UX — pruning order and indicator

When the predicate-matching subgraph would exceed K_v1, the existing
fan-out caps already prune; we pin the prune order and add an
operator-visible indicator.

**Prune order (deepest first; oldest first within a level):**

1. **Deepest level first.** The `constellationFanoutPerParent = 3`
   cap (`constellation.go:35`) already truncates each parent's L2+
   children to 3. We do not raise this for v1 — the slider raise to
   N=3 is the escape hatch.
2. **Within a level, prune by recency.** Reuse the same
   `applyAgeOpacity` ordering already computed by `constellation.go`:
   sort children of a parent by last-activity descending, take the
   first 3. Stale work fades visually anyway; pruning the oldest
   keeps the constellation focused on what is moving.
3. **Within same recency, prune `closed` before `open` /
   `in_progress`.** A closed sibling is informational; an open one is
   actionable. The existing `Status` field on `constellationNode`
   gives this for free.
4. **Stable tie-break by `id` ascending** so the rendered set is
   deterministic across requests (matches the `sort.Strings(parents)`
   discipline at `constellation.go:635`).

**Operator-visible indicator:**

- Each pruned parent renders a "+N more" pip near its node label (re-
  uses the existing label slot at `IDLabelY`; no new SVG primitive).
  Hovering the pip shows the truncated child IDs in a `<title>`
  tooltip — same hover affordance as `constellationNode.TitleAttr`.
- A banner above the right pane reads `Showing X of Y beads in this
  intake. Raise depth slider, or open in bead-show for full subtree.`
  when the subgraph is truncated. The banner is the same affordance
  used by the existing depth-slider hint, just with a different copy.
- **No "load more" button in v1.** Progressive disclosure is the
  slider raise (N=2 → N=3) plus the hover pip. Adding an inline
  expand-this-parent control is a v2 iteration after we see how often
  the slider is enough.

### 2.6 Edge cases — cycles, multi-parent, cross-intake decisions

**Cycles.** The existing `expandOuterLevels` keeps a `seen` set
seeded with the center plus L1 nodes (`constellation.go:612–618`) and
skips any target already in `seen`. We reuse this verbatim. A cycle
in the dep graph (rare; usually a bug in mention-extraction) renders
as the first traversal wins; subsequent re-encounters drop the node
but **keep the edge** to the already-rendered position so the cycle
still reads visually. (This is one behavior change from bead-show,
which currently drops both — see §2.7.)

**Multi-parent beads under the same intake.** A bead `B` reachable
via two distinct paths in the intake's subtree (e.g. parented by L1
node `A1` and also referenced by L1 node `A2`) is rendered **once**
at its first-encountered position, with **two edges** drawn — one
from `A1`, one from `A2`. The current `expandOuterLevels` already
does this for nodes (via `seen`); we extend the loop to emit the
second edge instead of skipping the entry. Cost: one extra
`constellationEdge` per multi-parent target, well within the SVG
edge budget.

**Decision / escalation beads referenced by multiple intakes.** A
decision bead `D` parented under intake `I_a` but also referenced
(typed mention `relates-to`) from intake `I_b` appears in **both**
`I_a`'s and `I_b`'s constellations:

- In `I_a`: `D` is a normal descendant via parent-child dep, drawn at
  L1 or L2 per the walk.
- In `I_b`: `D` surfaces only if it is *also* in `I_b`'s descendant
  closure. If the cross-intake reference is purely via a typed/untyped
  *mention* (no parent-child dep), `D` is drawn as an edge-decorated
  mention node via the existing untyped-mention path
  (`mergeTypedMentionsAsDepEdges` / `appendUntypedMentionEdges`),
  styled with the dashed `Origin = "mention"` stroke (lines 100–126,
  741–770).
- Cross-intake co-membership is **not** badged in v1. The hover
  preview already shows the bead's full parent list, so an operator
  who needs to know "is this also in another intake?" gets it via
  hover, not via a new node decoration.

**Self-references.** `targetID == "" || targetID == parentID` is
already filtered at `constellation.go:647`. Inherited.

**Disconnected children (mention-only, no dep edge).** A task with no
deps but an untyped mention pointing at the intake renders via the
existing mention-edge path and orbits the intake at L1 with the
dashed "mention" stroke. This is identical to bead-show behaviour;
no new code path.

### 2.7 Reused constants and call sites

Single source of truth for caps remains `constellation.go`. The
inbox-constellation builder reuses the symbols below verbatim — no
new constants, no copies:

| Constant | Line | Inbox use |
| --- | --- | --- |
| `constellationMaxReverse = 6` | `constellation.go:24` | L1 children of intake |
| `constellationMaxForward = 6` | `constellation.go:23` | unused in v1 (forward dropped) |
| `constellationMaxSimilar = 8` | `constellation.go:22` | unused (similarity arc dropped) |
| `constellationMaxTreeLevel = 3` | `constellation.go:31` | upper bound on depth slider |
| `constellationFanoutPerParent = 3` | `constellation.go:35` | L2+ per-parent fanout |
| `constellationHydrateTime` | `constellation.go:25` | per-render hydrate timeout |
| `constellationHydrateWorker = 6` | `constellation.go:26` | hydrate concurrency |
| `ageOpacityFloor = 0.30` | `constellation.go:42` | age fade reused unchanged |

Reused functions (no modifications):

- `clampTreeLevel(level int) int` (`constellation.go:561–570`) — depth
  slider sanitization.
- `constellationLayout(treeLevel)` (`constellation.go:572–585`) —
  viewBox + ring radii.
- `expandOuterLevels(...)` (`constellation.go:587–716`) — walks the
  reverse-dep frontier; called with `forward` frontier empty for the
  inbox case.
- `mergeTypedMentionsAsDepEdges`, `appendUntypedMentionEdges`,
  `mentionEdge` — typed/untyped mention edge folding.
- `applyAgeOpacity` — re-used as the age-fade pass on the rendered
  set.
- `hydrateBeads`, `depNode` — bead detail fetch + node materialization.

**Behaviour delta from bead-show that needs a sibling implementation
(not a modification of the existing function):**

1. Skip the forward arc; pass an empty L1 forward frontier.
2. In the cycle path, emit the second edge to the already-seen node
   rather than dropping it (§2.6 multi-parent rule).
3. Render the "+N more" pip + truncation banner (§2.5).

These three deltas argue for a sibling `buildInboxConstellation` next
to `buildConstellation` rather than a flag on the existing one — keeps
bead-show regression risk at zero per the parent epic's "existing
constellation is not modified" constraint.

### 2.8 Predicate summary (one paragraph)

> **The constellation an intake bead `I` is part of, v1:** anchor at
> `I` itself (no upward walk). Render `I` plus the transitive
> reverse-dep / mention closure walked outward at most `N=2` levels,
> with `constellationMaxReverse=6` children at L1 and
> `constellationFanoutPerParent=3` per L1 parent at L2 (worst case
> 25 nodes). Pruning order: deepest level first → oldest within
> level → closed before open → ID-ascending tiebreak. Multi-parent
> targets render once with multiple incoming edges; cross-intake
> co-mentioned decisions appear via the existing dashed-mention edge.
> Cycles are broken by the existing `seen`-set with the additional
> behaviour that the second edge to an already-rendered node is
> emitted (not dropped). All caps and helpers are reused from
> `gardenio/controller/cmd/beads-viewer/constellation.go`; the inbox
> view ships as a sibling builder `buildInboxConstellation`, not a
> modification of `buildConstellation`.

### 2.9 Open questions punted to other subtasks

- **Layout of the rooted subgraph** (concentric rings vs. lanes vs.
  category-banded) is §3 (subtask 3). This section pins only the
  *node set* — what's drawn, not where.
- **What happens when an intake has zero matching children but plenty
  of mention references** is a layout-default question for §3.
- **Pseudo-intake rows for un-parented decisions** are §6 (subtask 6).
  This section assumes every clicked row is a real intake.
- **Whether a slider raise to N=3 needs server-side cost gating**
  (e.g. require explicit opt-in flag) is §7 (subtask 7). This section
  assumes the existing 600 ms hydrate budget is the same.


---

## 3. Layout for an intake-rooted subgraph

> **Subtask:** ka-0000003739
> **Inputs:** round-7 research epic ka-0000004309 (absorbed); round-8 plan
> in `escalation-beads-review-2026-05-05.md`; D7 plan ka-0000003787
> (`DeriveCategory`); existing primitives in `constellation.go`.

### Decision: hybrid — concentric rings by depth, category as angular sector + fill hue

For the intake-rooted subgraph rendered in the right pane of `/inbox`, the
default layout is a **hybrid**:

- **Concentric rings by depth from the intake anchor** drive *position*. The
  intake bead sits at the center; level-1 descendants sit on ring 1; deeper
  descendants on rings 2..N. Ring radii reuse `constellationLayout`'s existing
  per-tree-level table (240 / 380 / 500 in the current 1000×500 / 1200×800 /
  1400×1100 viewBoxes — see `constellation.go:576`).
- **Category drives angular sector + node color**. Each category
  (`action` / `intermediate` / `config` — `intake` is the center itself) is
  pinned to a fixed arc range so that, on every ring, action descendants
  appear in the same angular slice, intermediate in another, config in a
  third. Within a sector, nodes fan evenly via `arcAngle` and place via
  `polarPosition` (the same helpers the current view uses).
- **Edge styling, status fills, age opacity, and hydrate fan-out caps are
  reused unchanged** from `constellation.go`. No new edge stroke palette.

#### Default sector assignments (1000×500 viewBox, center=(500,250))

The 360° angular space splits into three contiguous arcs, leaving a small
guard between sectors so the eye reads the breaks:

| Category       | Arc (degrees) | Mnemonic                                       |
|----------------|---------------|------------------------------------------------|
| `action`       | −150° → −30°  | top — what the operator looks at first         |
| `intermediate` | −30° → +90°   | right — the middle of the work                  |
| `config`       | +90° → +210°  | bottom-left — supporting / scaffolding         |

(`intake` descendants are rare; per D7 rule 3 a nested intake is itself
either an epic-under-epic — already `intermediate` — or an external-source
re-entry. If an `intake` descendant does appear it is folded into the
`intermediate` sector and given the intake glyph; rationale below.)

These sector boundaries mirror the current bead-show constellation's
`forward` arc (−150° → −30°) and `reverse` arc (150° → 210°) so the muscle
memory carries over for operators who already navigate `/bead/<id>`.

#### Why hybrid (and not pure lanes or pure rings)

The two pure alternatives both lose information:

- **Pure lanes by category** (stacked horizontal bands or vertical columns,
  the round-7 epic's first sketch). Strong category legibility, but the
  *intake center* loses its meaning — there is no obvious "anchor" position
  in a banded layout, and the edges from the intake to every descendant
  cross every band, producing a hairball. Round-7 subtask 1 flagged this
  for the project-wide case; it is worse for the intake-rooted case where
  the center bead is load-bearing.
- **Pure concentric rings by depth** (the current bead-show layout, just
  re-anchored to an intake). Strong topology legibility, but every category
  signal is mediated by node color alone — and color carries `status` in the
  current palette (`statusFill`, `constellation.go:984`). Adding a second
  meaning to color either reuses the status palette (collisions: which is
  "open action" vs. "open intermediate"?) or introduces a parallel palette
  the operator must learn alongside status.
- **Hybrid** keeps depth on the radial axis (where the existing primitives
  already place it) and category on the angular axis (where the current
  view spends `forward` / `reverse`, an axis the eye is already trained to
  read as kind-of-relationship). The two channels do not collide. Status
  remains the `fill`; category is conveyed by *position*, with an optional
  decorative `category-tint` on the stroke (see "Reuse" below).

#### Center-bead semantics

Per round 8, the center *is* the selected intake from the left rail. It is
constructed via the existing `Center` field on `constellationView`
(`constellation.go:51-71`):

- Radius `42.0`, halo `2.2× radius`, golden ring (`#d4a017`).
- `Color = statusFill(intake.Status)` so the operator sees the intake's
  current status at a glance.
- `Glyph = typeGlyph(intake.Type)` (typically `E` for epic, `↗` for an MR,
  source-tagged glyphs for slack/jira/gitlab intakes — extend `typeGlyph`
  if needed; out of scope for this section).

The center stays at full opacity (`view.Center.Opacity = 1.0`,
`constellation.go:353`); the age-opacity floor is only applied to orbit
nodes. This rule carries over verbatim.

### ASCII sketch — empty-graph case (intake with no descendants)

The default selection from the index can be an intake with zero descendants
(a freshly-arrived Slack message, a Jira ticket nobody has expanded yet, a
new external-source bead). The layout must not look broken in this case.

```
                              1000
  ┌───────────────────────────────────────────────────────────────┐
  │                                                               │
  │                                                               │
  │                          ka-0000006157                        │
  │                          ┌─────────┐                          │
  │                          │   ●     │                          │  500
  │                          │  E:open │                          │
  │                          └─────────┘                          │
  │      Epic: inbox-primary constellation view (intake-…)        │
  │                                                               │
  │   action sector (top)        ·   no descendants yet           │
  │   intermediate sector (rt.)  ·   nothing to show              │
  │   config sector (bot.-left)  ·   nothing to show              │
  │                                                               │
  │      [ tip: drill into the inbox row again after the          │
  │              first agent claims this work ]                   │
  └───────────────────────────────────────────────────────────────┘
```

Concrete view-model contract for the empty case:

- `view.Center` populated (the intake bead); `Forward`, `Reverse`, `Outer`,
  `Similar` slices all empty; `Edges` empty.
- `HasForward`, `HasReverse`, `HasOuter`, `HasSimilar` all `false`;
  `HasAnything` is also false — the template branches on this to render the
  three sector caption strings ("action sector — no descendants yet",
  etc.) below the center node, in the same legend area used today by
  `constellationLegendItems`.
- Sector labels are *captions*, not nodes — they do not consume `arcAngle`
  slots and cannot be clicked. They render as static `<text>` elements
  styled to read as muted hints.
- A small one-line tip ("drill into the inbox row again after the first
  agent claims this work") sits below the legend area. This is for the
  empty-graph case only; suppressed once any descendant exists.

The empty-graph branch is the cheapest case for the hydrate path: zero
follow-up `GetBead` calls, zero hydrate timeout pressure.

### ASCII sketch — overflow case (22-bead operator queue as fixture)

The current operator queue (~22 open beads, mostly intake epics with action
and intermediate descendants — see `kd list --type=epic --status=open` for
the live shape) is the load-test fixture for this layout. A representative
single-intake subgraph rooted at, e.g., `ka-0000006157` (the parent of this
very subtask) hydrates roughly:

- 4–6 child epics / intermediate tasks (ring 1, intermediate sector)
- 2–3 escalations / decisions (ring 1, action sector)
- 1–2 config beads (schedules, advice — ring 1, config sector)
- 8–12 grandchildren on ring 2 (mostly intermediate; some action)
- 2–4 great-grandchildren on ring 3 (almost all intermediate)

That is well above the existing per-page caps (`maxForward=6`,
`maxReverse=6`, plus `fanoutPerParent=3` per L1 parent at deeper levels —
`constellation.go:22-36`). Reusing those caps without modification yields
the following overflow-friendly rendering at `treeLevel=3`
(viewBox 1400×1100):

```
                                   1400
  ┌──────────────────────────────────────────────────────────────────────────┐
  │                              ACTION sector                              │
  │                       (top arc, −150° → −30°)                           │
  │                                                                          │
  │     ●  ka-Y1tg…           ●  ka-MEm6…           ●  ka-tQH2…  (+2 more)  │
  │ (decision)               (escalation)          (epic→action)             │
  │       ╲ ── ── ── ── ── ── ── ── ── ── ── ── ── ── ── ── ╱                │
  │            ╲                                          ╱                  │
  │              ╲           ●  intake center           ╱                    │
  │                ●         ┌────────────┐           ●                      │  550
  │   (ring 2,      ╲        │ ka-uu6W…   │          ╱  (ring 1,             │
  │    grandchild)   ╲       │  E:open    │         ╱    intermediate)       │
  │     INTERMED              └────────────┘                                 │
  │                ╲                              ╱                          │
  │     ●  ──── ●  ╲          ●  ●  ●  ●        ╱  ●  ●     +6 more         │
  │   (overflow      (intermediate sector,           "+6 more" sentinel)     │
  │   expandable)     right arc, −30° → +90°)                                │
  │                                                                          │
  │              CONFIG sector (bottom-left, +90° → +210°)                   │
  │                ●  ka-BIB…              ●  ka-Gdy…       (+1 more)        │
  │              (advice/plan)             (schedule)                        │
  └──────────────────────────────────────────────────────────────────────────┘
                                  1100
```

Key overflow rules carried over from the existing constellation:

- **Per-direction caps still bound the L1 ring.** Reuse
  `constellationMaxForward = 6` / `constellationMaxReverse = 6` interpreted
  as "≤6 per category sector on ring 1". This keeps the daemon hydrate
  workload ≤ 18 nodes / hop and avoids changing the cap constants.
- **Per-parent fanout still bounds rings 2..N.** Reuse
  `constellationFanoutPerParent = 3` so a single noisy L1 parent cannot
  flood ring 2.
- **"+N more" sentinel inside each sector.** When a sector overflows its
  cap, append a single static `<text>` "+N more" node *outside* the radial
  ring (a few pixels past `levelRadii[ring]`) in the same sector arc. The
  sentinel is keyboard-focusable and clicking opens the full descendant
  list at `/<intake-id>?open=<sector>` (an existing-API list view, no new
  endpoint). Rationale: the user always sees a count; the sentinel only
  intrudes on the SVG when there is overflow.
- **Empty sectors print a caption, not a placeholder node** — same primitive
  as the empty-graph case.
- **Tree-level slider stays.** `?tree=N` (1..3) still expands deeper rings
  using `constellationLayout`'s existing radii. The `tree=1` rendering of
  the same fixture fits comfortably in the 1000×500 default viewBox; the
  `tree=3` rendering in the 1400×1100 viewBox is the worst-case load.

### Reuse of existing primitives

This layout introduces **no new SVG palettes, no new caps, no new template
helpers**. It composes existing primitives:

| Primitive                                | Source (`constellation.go`)        | Use in inbox layout                                                  |
|------------------------------------------|------------------------------------|----------------------------------------------------------------------|
| `constellationLayout(treeLevel)`         | line 576                           | viewBox + center + per-ring radii — used *as-is*                    |
| `arcAngle(i, n, start, end)`             | line 534                           | even fan-out within a sector arc                                     |
| `polarPosition(cx, cy, r, angle)`        | line 544                           | `(x, y)` for every orbit node                                        |
| `statusFill(status)`                     | line 984                           | node `Color` (status remains the fill axis)                          |
| `statusStroke(status)`                   | line 999                           | node `Stroke`                                                        |
| `typeGlyph(type)`                        | line 1017                          | single-char marker inside each disk                                  |
| `applyAgeOpacity(view, …)`               | line 347                           | unchanged — orbit-only fade, center stays at 1.0                     |
| `ageOpacityFloor` (`0.30`)               | line 42                            | unchanged                                                            |
| `mentionEdge(...)`, `edgeStyle(...)`     | line 854 / 878                     | edges between intake center and ring 1 reuse the dep / typed-mention / untyped-mention palette unchanged |
| `constellationMaxForward = 6`            | line 23                            | reinterpreted as per-sector L1 cap                                   |
| `constellationMaxReverse = 6`            | line 24                            | reinterpreted as per-sector L1 cap                                   |
| `constellationFanoutPerParent = 3`       | line 35                            | unchanged — still caps rings 2..N per parent                         |
| `constellationMaxTreeLevel = 3`          | line 31                            | unchanged                                                            |
| `hydrateBeads(ctx, ids)`                 | line 921                           | unchanged — same parallel hydrate, same 600 ms budget                |
| `constellationLegendItems()`             | line 1050                          | extend with three category-sector entries (additive, no removals)    |

**Optional decorative tint (deferred to subtask 4 / interactivity):** if
the operator wants a faint per-category visual cue beyond position, the
existing `Stroke` field can carry a category-tinted darkening of
`statusStroke` (e.g. action sectors render their stroke at full saturation;
intermediate sectors at 0.85; config at 0.7). This is a follow-up
proposal, not a blocker for v1; it does not affect layout geometry.

### Findings carried over from absorbed round-7 epic (ka-0000004309)

Round-7 subtasks 1, 4, 6, and 7 produced layout / edge / perf / promotion
findings before the round-8 pivot folded the epic into ka-0000006157. The
load-bearing items for *this* section:

- **Subtask 1 — layout**: Round-7 evaluated stacked horizontal bands and
  vertical columns against the 22-bead fixture. Both produced edge
  hairballs when a center bead emitted dependencies into every band.
  Concentric rings out-performed both on edge legibility but lost
  category signal. **Hybrid is the synthesis.** Carried forward.
- **Subtask 4 — edges in lane mode**: Round-7 considered repurposing edges
  as cross-lane traversal arrows (intake → intermediate → action flow).
  The conclusion was that edges should keep dependency semantics, because
  rewriting their meaning collides with the existing `dep` / `text` /
  `mention` provenance palette (`edgeStyle`, `constellation.go:878`). The
  inbox layout adopts that conclusion: edges keep their existing
  semantics; cross-category traversal is implicit in the angular geometry,
  not in stroke styling.
- **Subtask 6 — perf**: Round-7 noted that a project-wide lane view would
  need a constellation-specific endpoint to avoid N+1 hydrates. The
  intake-rooted subgraph defined here does **not** have that problem: it
  is bounded by the existing per-direction caps and the per-parent fanout
  cap, so it fits within the existing list/get APIs. The subgraph
  predicate (subtask 2) is the load-bearing perf question, not the
  layout. Layout-side, the worst case (`treeLevel=3`, 22-bead fixture) is
  ≤ 30 hydrates over a 600 ms budget — within the current envelope.
- **Subtask 7 — promotion criteria**: Out of scope for this section
  (subtask 8 of the parent epic). Recorded here only as a pointer: any
  promotion of the inbox view to default at `/` must demonstrate the
  hybrid layout reads cleanly on the empty-graph and overflow cases
  documented above.

### Open questions deferred to other subtasks

- **Subgraph predicate** (subtask 2): "the constellation a bead is part
  of" — walk to topmost intake/epic ancestor, expand N levels down, cap
  at K. The layout above assumes that predicate produces a tree rooted at
  the intake. If the predicate later produces a connected component
  rooted elsewhere (e.g. a child of the selected intake), the layout's
  center-anchor assumption fails and an addendum will need to land here.
- **Edge styling for cross-sector category-traversal flow**: deferred to
  subtask 4 / interactivity. v1 keeps existing `mentionEdge` styling.
- **Action beads not under an intake**: handled at the index level
  (subtask 6 of the parent epic); for the layout, an "orphan-action
  pseudo-intake" appears as just another center-anchored subgraph and
  needs no special-casing.

---

## 4. Index UI

### 4.1 Problem statement

§1 pinned the predicate that backs the inbox-primary view's index: the
set of intake beads operated by the current viewer. §2 pinned the
constellation subgraph that opens to the right of that index when a
row is clicked. §3 pinned the layout of that subgraph.

This section pins the **index UI itself** — the chrome around the
predicate result set that lives on the left of the inbox-primary view:
the shell shape (where on the page, how it relates to the
constellation pane), the filter chips that switch between predicate
variants, the sort the rows arrive in, the keyboard model, and the
banner that fires when the viewer's identity is not wired up well
enough for the predicate to return a useful result set.

It is deliberately *just* the index UI. It does not redesign per-row
rendering (which is the existing `project_row` partial reused from
`/active`), it does not specify the per-intake action surface (subtask
5), and it does not specify pagination beyond the requirement that the
sort be index-friendly (subtask 7).

### 4.2 UI shape — left-rail column

#### 4.2.1 The pick

**Left-rail column** — a persistent index occupying the left third of
the inbox-primary view, with the per-intake constellation drilling in
on the right two-thirds. Same shell as `/active`, with the constellation
pane replacing the per-project group cards.

```
┌────────────────────────────┬────────────────────────────────────────┐
│ Inbox · 24 intakes         │  ⊙ ka-A111 · "RWX cred expired"        │
│ ─────────────────────────  │                                        │
│ [mine] [team] [on-call] [+]│        (subtask 3 layout — concentric  │
│ Sort: urgency · ↓          │         rings of descendants, action   │
│ ─────────────────────────  │         sector top, intermediate right,│
│ ⊙ ka-A111 · RWX cred…  ▸  │         config bottom-left)            │
│ ⊙ ka-B222 · GitLab MR  ▸  │                                        │
│ ⊙ ka-C333 · Slack: …   ▸  │                                        │
│ …                          │                                        │
└────────────────────────────┴────────────────────────────────────────┘
```

#### 4.2.2 Why left-rail and not the alternatives

Two non-trivial alternatives were considered and rejected:

- **Sidebar pop / drawer** (index hidden behind a hamburger; click an
  intake → constellation fills the page). Rejected: the inbox-primary
  view's value is *index ↔ subgraph context-switching at speed* —
  scan the index, click an intake, scan the constellation, *go back
  to the index without losing your place and pick the next one.* A
  drawer that has to re-open every time defeats the workflow. The
  iOS analogue (`InProgressTab` → bead-detail back-stack) survives
  the round trip because the OS owns the back gesture; in a web
  viewer the index needs to stay on screen.
- **cmdk-style overlay** (Cmd-K opens an index palette; pick → the
  page navigates to that intake's constellation; overlay closes).
  Rejected as the *primary* shell for the same reason — a one-shot
  picker is fine for "jump to a known thing", but the inbox is an
  unknown-thing surface where the operator wants to *triage*, not
  *navigate*. cmdk *does* survive, but as a complement (§4.5
  keyboard model): the existing
  [`templates/static/cmdk.js`](../../controller/cmd/beads-viewer/templates/static/cmdk.js)
  bead-search palette continues to be triggered by Cmd-K from
  anywhere in the viewer; the inbox-primary view does not own it.

The left-rail shape is the same one `/active` uses for project
groups. Reusing it keeps the inbox visually continuous with the
existing viewer and lets us reuse the
[`active.html`](../../controller/cmd/beads-viewer/templates/active.html)
two-pane skeleton with the right pane swapped out for the
constellation surface.

#### 4.2.3 Width / responsiveness

- Left rail: `min-width: 320px`, `max-width: 420px`, default ~33% of
  viewport. Wider than the 280px iOS analogue because intake titles
  carry long source-prefixed strings (`Slack[#ops-platform]: …`,
  `gitlab[!2118]: …`).
- Below the existing `/active` mobile breakpoint (~768px viewport)
  the left rail collapses to a top-of-page drawer that toggles open
  by default and closes when the operator clicks an intake. The
  constellation pane then occupies the full viewport. This is a
  fallback shape — the inbox-primary view is not optimised for
  mobile and the operator base lives in desktop browsers.

Width and breakpoint specifics are non-load-bearing; the primary
contract is "left-rail column on desktop, persistent". Subtask 7
owns any further tuning if the index ends up rendering more than ~50
rows in practice.

### 4.3 Filter chips

The chips live in a single horizontal strip immediately under the
"Inbox · N intakes" header and immediately above the sort control.
They map 1:1 to predicate variants on the `category=intake` query —
exactly one chip is active at any time (radio semantics, not
checkbox). The chip strip mirrors the existing project-filter strip on
[`/active`](../../controller/cmd/beads-viewer/templates/active.html)
visually; a `chip-active` class marks the selected one.

#### 4.3.1 Chip set + predicate map

| Chip          | URL                                          | Resolver call                                                                                                          | Predicate                                                                                              |
| ------------- | -------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------ |
| **mine**      | `/inbox` (default) or `/inbox?scope=mine`    | viewer-resolver §1.4 returns `viewer_operator_id` (v1) or `identity_ids` (v0.5)                                        | §1's base predicate — `operated_by_id = :viewer_operator_id` (v1) / `operated_by = ANY($1)` (v0.5)     |
| **team**      | `/inbox?scope=team`                          | resolver returns `team_operator_ids` — viewer's `operators.team` rows (Option B `operators.team` field, P3+ ADR §3.1)  | `operated_by_id = ANY(:team_operator_ids)` (v1) / IN-set expansion of all team members' identity sets (v0.5) |
| **on-call**   | `/inbox?scope=on_call`                       | resolver returns `on_call_operator_id` from active on-call schedule (subtask 8 / 3R2-G in review doc round 3)          | `operated_by_id = :on_call_operator_id` (v1) / IN-set per-platform (v0.5)                              |
| **raise-only**| `/inbox?scope=raise_only`                    | viewer-resolver §1.4 returns viewer ids                                                                                | `operated_by_id = :viewer_operator_id AND created_by_operator_id = :viewer_operator_id` (v1) / equivalent string equality (v0.5) — strips intake the viewer *operates* but did not *raise* |

Notes per chip:

- **mine** is the default and the cold-load surface. The URL key is
  optional; an absent `?scope=` is treated as `?scope=mine` (mirrors
  `/active`'s default-on operated-by behaviour, §4.6.2 below). When
  present, `?scope=mine` is the operator's explicit choice and is
  preserved verbatim in the chip strip's switch-to links.
- **team** depends on the `operators.team` column landing under
  Option B P3 (per ADR §3.1 / §6 backlog). Until then, the chip is
  rendered but disabled with a tooltip pointing at the Option B P3
  backlog item; the inbox does *not* try to fall back to a
  `team:<name>` label sweep across `user_identity` beads.
- **on-call** depends on a separate "on-call schedule" primitive
  that does not yet exist (3R2-G in `escalation-beads-review-2026-05-05.md`
  round 3 lists this as ~2 days). The chip is rendered but disabled
  with a tooltip pointing at that backlog item. We render-disable
  rather than hide so the chip strip's information architecture is
  visible from day one — operators see what is coming.
- **raise-only** is a precision tool for triage: it strips intake
  whose `operated_by` rolled to the viewer because they statused or
  commented (i.e. `operated_by_id` set by a mutation event), leaving
  only intake the viewer originally raised. v1 needs a
  `created_by_operator_id` column on `beads` (i.e. immortal author
  alongside the per-mutation `operated_by_id`) to be cheap; that
  column is *not* in Option B's migration 015. **Until the column
  exists, the chip is disabled with a tooltip** pointing at a new
  follow-up bead (filed by this section — see §4.7 cross-references)
  that asks for `beads.created_by_operator_id` as a prerequisite.

#### 4.3.2 Why these four and not more

Subtask 4's bead description names exactly four chips. Three smell
tests confirm the set:

1. **Each chip changes the predicate, not the rendering.** Sort,
   density, and per-row decoration are not chips here — they live in
   the sort toggle (§4.4) or are out of scope (per-row rendering is
   subtask 5).
2. **Each chip names a *role* the viewer plays at scan time**
   ("things on me", "things on my team", "things on call", "things I
   raised"). Roles are stable over a session; rendering choices
   ("hide closed", "hide low priority") are per-scan and belong on a
   secondary control.
3. **No chip is a saved-view stand-in.** Saved views (`/views/<id>`)
   are the right place for "intake from the gitlab bridge with
   priority ≤ 2"; the chip strip stays small and predicate-shaped.

A "snoozed" chip was considered (per 3R2-E in the review doc round
3) and explicitly deferred: snooze is an action on a row, not a
predicate variant on the index, and lands as part of subtask 5
(interaction surface). When snooze ships, the index hides snoozed
intake by default and surfaces them via a `?include_snoozed=1` URL
flag rather than a chip.

#### 4.3.3 Layout when team / on-call / raise-only are disabled

The chip strip renders all four chips at all times. Disabled chips
get a `chip-disabled` class (existing pattern in `active.html` for
the `(none)` project chip) and a tooltip pointing at the bead that
unblocks them. Hiding them would leak implementation sequencing into
the UI; rendering-disabled keeps the IA stable and tells the
operator what is coming.

### 4.4 Default sort + toggle UX

#### 4.4.1 The pick

**Default sort: composite urgency, descending.** The composite-urgency
formula was named in `escalation-beads-review-2026-05-05.md` round 3
as part of D3-r2's resolution (3R2-B):

```
score = priority_weight(p) × age_factor(t/sla) × downstream_blocked_count
```

The exact shape of `priority_weight` and `age_factor` is itself an
open item in the review doc (round 3 §"Composite urgency score",
line 246) — not subtask 4's call to make. **What this section pins is
the *contract*:**

- The default sort is whatever the application-layer urgency function
  returns, applied identically on the inbox index and on the
  `/operator-inbox` surface from D3-r2. The two views *must* agree on
  the ranking — they are different shells over the same predicate.
- The function is implemented once in the controller (proposed home:
  alongside `operator_chip.go`) and is reused.
- The default sort direction is descending (highest urgency at the
  top of the rail).

#### 4.4.2 Single toggle: a sort dropdown, three options

Below the chip strip, one control:

```
Sort: [▼ urgency        ] ↓
       priority
       last activity
       urgency      (•)
```

- **urgency** (default) — composite-urgency, descending.
- **priority** — `priority ASC, updated_at DESC` (lowest numeric
  priority first; tiebreak on recency). This is the "P1 floor" view.
- **last activity** — `updated_at DESC` (most-recently-touched
  first). The "what just happened" view.

Direction is a separate `↓ / ↑` toggle, defaulting to descending for
all three options. Persisted in the URL as `?sort=urgency&dir=desc`.
Operator-shaped persistence (sticky-per-viewer) is deferred — no
state in cookies, the URL is the source of truth (matches
`/active`'s sort behaviour today).

#### 4.4.3 Index-friendliness obligation

Each sort option must be servable by an index without a sort step on
the heap:

- **urgency** — composite-urgency is computed at write time into
  `beads.urgency_score DOUBLE PRECISION` (proposed; subtask 7 owns
  the migration). The query is `ORDER BY urgency_score DESC, id ASC`.
  An index on `(operated_by_id, urgency_score DESC)` (partial:
  `category='intake'`) keeps the inbox query O(log N) for the
  default scope. **If the column is not landed by the time the
  inbox surface is implemented**, urgency is computed application-side
  on the page-sized result set returned by a `priority + updated_at`
  pre-sort and the sort dropdown labels the option as "urgency
  (slow path)". This is a degradation, not a correctness problem.
- **priority** — already index-friendly via `(operated_by_id,
  priority, updated_at DESC)`.
- **last activity** — already index-friendly via `(operated_by_id,
  updated_at DESC)`.

Subtask 7 owns the index choices; this section pins only that the
*default* is a single column read, not a heap sort.

#### 4.4.4 Why one toggle, not faceted controls

Three sorts is the smallest set that covers the three operator
mental models the review doc surfaces: "who is bleeding the most
right now" (urgency), "what is officially most important" (priority),
"what just changed" (last activity). Anything beyond three pushes
this surface toward the saved-views shape, which is a different
primitive.

### 4.5 Keyboard model

The existing
[`templates/static/keys.js`](../../controller/cmd/beads-viewer/templates/static/keys.js)
already implements the keyboard model the inbox-primary view needs.
**The inbox does not add a new key handler; it adds a CSS hook.**

#### 4.5.1 What keys.js already does

Per the script's header comment:

```text
j / k             - move focus down / up the visible bead list
Enter / o         - open the focused bead
gg                - jump to top of the bead list
G                 - jump to bottom of the bead list
/                 - focus the search input
Esc               - close inline-edit popover / blur input
g + a/d/s/v/c/i/h - jump to /agents /decisions /schedules /advice /costs /active home
?                 - toggle the help overlay
```

The script discovers focusable rows via the selector
`.bead-row, .advice-row, .sched-row, .search-results tbody tr`
(`keys.js` line 21, `ROW_SELECTOR`). Pressing Enter on the focused
row navigates to its `a[href]`.

#### 4.5.2 What the inbox view does

- Each row in the index rail uses the existing `.bead-row` class so
  `keys.js` discovers it as a focusable row without any change to the
  selector.
- Enter on a focused row navigates to the *constellation pane URL*
  for that intake (e.g. `/inbox/<intake-id>`), not the bead-show
  page. Achieved by the `<a href>` on the row pointing at the
  inbox-primary deep link, not at `/bead/<id>`. Plain bead-show
  navigation is still available via cmdk (Cmd-K → search → Enter)
  and via right-click → open in new tab.
- The `g + i` shortcut (currently mapped to `/active`) is rebound
  to point at `/inbox` once the inbox-primary view ships. The
  existing `g + a` for `/active` stays, since `/active` continues to
  exist as the project-grouped overview.
- A new shortcut `g + n` jumps from the constellation pane back to
  the inbox index (focuses the rail). This is the only addition to
  `keys.js`'s handler tables; the rest is selector reuse.

#### 4.5.3 What we explicitly do not add

- **Arrow-key navigation as an alternative to j/k.** The existing
  pattern is j/k only; adding arrow keys would compete with the
  browser's scroll behaviour and break parity with the rest of the
  viewer. Arrow keys remain browser-default scroll.
- **Vim-style `:` command bar.** cmdk (Cmd-K) is the existing
  command-palette primitive; a second one would split the muscle
  memory.
- **Per-row chord shortcuts** (e.g. `s` to snooze, `r` to resolve).
  Subtask 5 owns the per-intake action surface; if it adds row
  chords they extend `keys.js` once, not per-surface.

### 4.6 Missing-identity banner

When the viewer-resolver §1.4 returns a result that produces an
empty or near-empty inbox *because the viewer's identity is not
fully wired up*, the index renders a banner above the chip strip.
The banner shape differs between the v1 (Option B) and v0.5 paths.

#### 4.6.1 v1 — under Option B

Two distinguishable failure modes:

**Mode A: no `operators` row.**

The viewer-resolver looks up `(oidc, viewer_email)` in
`operator_aliases` and finds nothing. The viewer has no row in
`operators` at all — likely a brand-new human who has signed in
once via OIDC but never had an intake stamped against them.

```
┌─────────────────────────────────────────────────────────────────┐
│ ⓘ  No operator record found for matt@acme.example.               │
│                                                                 │
│    You have no `operators` row, so the inbox cannot scope to    │
│    "your" intake yet. Run:                                      │
│                                                                 │
│        gb identity add --oidc matt@acme.example                  │
│                                                                 │
│    Or have an operator run it on your behalf. Once your row     │
│    exists, intake stamped against any of your aliases will      │
│    appear here.                                                 │
└─────────────────────────────────────────────────────────────────┘
```

The inbox renders empty (no rows below the banner). The chip strip
is hidden — chip selection is meaningless without an operator id.

**Mode B: alias set is missing a platform.**

The viewer has an `operators` row, but `operator_aliases` has only
the `oidc` row. Intake stamped by the Slack bridge under
`slack:U0OPERATOR1` will not roll up to this operator id because the
alias was never recorded.

```
┌─────────────────────────────────────────────────────────────────┐
│ ⓘ  Showing 12 intakes for matt@acme.example.                     │
│                                                                 │
│    Your alias set is missing platform `slack`. Intake your      │
│    Slack identity raised will not appear here. Run:             │
│                                                                 │
│        gb identity add --slack U0OPERATOR1                      │
│                                                                 │
│    Or list aliases first: `gb identity show`.                   │
└─────────────────────────────────────────────────────────────────┘
```

The inbox renders normally below the banner. Mode B is non-blocking
— the viewer can still triage what's already there — but actively
warns about the gap.

The platform names listed in mode B are computed by intersecting the
*set of source labels seen on intake beads in the last 30 days*
(`source:slack`, `source:gitlab`, `source:jira`) with the *set of
alias kinds present on the viewer's operator row*. Any source the
system uses but the viewer has no alias for produces a warning. This
keeps the banner specific (it only suggests platforms that actually
matter for inbox membership) without hard-coding platform lists in
the UI.

The "run `gb identity add` on your behalf" copy is intentional: the
ADR allows a non-self alias upsert by an operator with the right
role, and a brand-new viewer signing in for the first time may not
yet have CLI access.

#### 4.6.2 v0.5 — pre Option B

Under v0.5 the resolver returns a `[]string` set computed from a
`user_identity` bead. The banner reframes accordingly.

**Mode A' (v0.5): no `user_identity` bead.**

```
┌─────────────────────────────────────────────────────────────────┐
│ ⓘ  No user_identity bead found for matt@acme.example.            │
│                                                                 │
│    The inbox is filtering to oidc:matt@acme.example only.        │
│    Slack / GitLab / Jira intake will not appear until you       │
│    create your identity bead. Run:                              │
│                                                                 │
│        gb identity add --oidc matt@acme.example \                │
│                       --slack U0OPERATOR1 \                     │
│                       --gitlab matthewbaker                     │
│                                                                 │
│    Or open `/identity` and edit the bead directly.              │
└─────────────────────────────────────────────────────────────────┘
```

**Mode B' (v0.5): `user_identity` bead exists but a platform is
missing.** Same trigger as v1 mode B — intersect seen `source:*`
labels with bead's recorded platforms. Banner copy points at the
`user_identity` bead, not at `gb identity add`'s alias subcommand:

```
   Your user_identity bead is missing field `slack_id`. Edit at
   /bead/<user_identity_bead_id> or run:
       gb identity add --slack U0OPERATOR1
```

The `gb identity add` CLI is the same in both worlds: it writes to
either `operator_aliases` (v1) or to the `user_identity` bead
(v0.5), transparently. The banner's onboarding copy therefore
survives the v0.5 → v1 swap unchanged.

#### 4.6.3 Banner suppression

- Suppressed when the viewer is anonymous (no OIDC subject) — the
  inbox-primary view itself is OIDC-gated upstream; an anonymous
  request never reaches the index handler.
- Suppressed for the operator(s) named in the no-banner allowlist
  used by `/active`'s migration banner
  (`shouldRenderMigrationBanner` in `active.go`) — see also the
  per-viewer dismissal cookie pattern there. We do not add a new
  dismissal mechanism for this banner; mode A is blocking (cannot
  triage without an operator record), mode B is informational and
  re-evaluated per request based on actual missing aliases.

### 4.7 Reuse summary + cross-references filed

This section deliberately defers work to existing primitives
wherever possible. The reuse table:

| What                         | Source                                                                                         | How the inbox uses it                                       |
| ---------------------------- | ---------------------------------------------------------------------------------------------- | ----------------------------------------------------------- |
| Identity resolution          | `operator_chip.go` (`resolveOperatedByParam`, `resolveDefaultOperatedBy`, viewer-resolver §1.4) | Chip-strip backing + missing-identity banner data           |
| Two-pane shell               | `templates/active.html` skeleton                                                                | Same left-rail structure with constellation pane on right   |
| Per-row card                 | `project_row` partial (used by `/active`)                                                       | Same row markup so `keys.js` discovers `.bead-row`          |
| Keyboard navigation          | `templates/static/keys.js`                                                                      | j/k/Enter/gg/G/?, plus rebound `g i` and new `g n`          |
| Chip strip CSS               | `chip` / `chip-active` / `chip-disabled` classes on `active.html`                              | Filter-chip rendering                                       |
| Sort persistence             | `?sort=` / `?dir=` URL params                                                                   | Source-of-truth pattern from `/active`                      |
| Banner pattern               | `shouldRenderMigrationBanner` + dismissal cookie pattern (`active.go`)                         | Allowlist plumbing only — no per-viewer dismissal cookie    |
| Composite urgency            | D3-r2 formula (review doc round 3, line 246)                                                    | Default sort; computed once, shared with `/operator-inbox`  |
| cmdk overlay                 | `templates/static/cmdk.js`                                                                      | Untouched; remains the global Cmd-K bead-search palette     |

Cross-references filed (or to be filed when this section lands):

- **`beads.created_by_operator_id` column** — prerequisite for the
  raise-only chip's cheap predicate. New bead: *"Add immortal
  `created_by_operator_id` column to `beads` for raise-only inbox
  chip + author-attribution surfaces"* (parent: ka-0000006157;
  blocks: ka-0000001406 raise-only chip wiring).
- **`beads.urgency_score` column + writer hook** — landed as part of
  subtask 7 (performance / data shape), since the hot path is the
  index. This section names the contract; subtask 7 owns the
  schema, the writer hook, and the recompute strategy.
- **`operators.team` column** — already on Option B's P3 backlog
  (ADR §3.1 / §6); team chip blocks on it.
- **on-call schedule primitive** — review doc round 3 / 3R2-G;
  on-call chip blocks on it. Filed under the parent escalation
  epic, not under ka-0000006157.

### 4.8 Out of scope

Pinned for the right subtask:

- **Per-row action surface** — snooze, resolve, escalate, assign:
  subtask 5.
- **Action beads not under an intake** — the constellation pane's
  fallback when an intake has no descendants worth showing: subtask 6.
- **Pagination** — the predicate's result set is small in the median
  (~24 intakes per operator today) but unbounded in principle:
  subtask 7. The contract this section pins is that *the default
  sort is index-friendly* so that the pagination strategy is "page
  by id after sort" rather than "sort all then page".
- **Promotion criteria** — when does the inbox-primary view
  supersede `/active` as the cold-load default? Subtask 8.
- **Saved views interaction** — saved views (`/views/<id>`) are not
  a chip on the inbox; the inbox is the cold-load default and saved
  views are user-created refinements that link out. The two
  surfaces share the row partial but not the chrome.

### 4.9 Open questions for operator review

1. **`beads.created_by_operator_id`?** Is the operator willing to
   pay for an immortal author column on `beads` to make the
   raise-only chip cheap, or should raise-only be deferred until an
   alternative (e.g. derive from the create-event row in the
   `events` table) is benchmarked? §4.3.1 chip wiring depends on
   this.
2. **`beads.urgency_score` write-hook timing.** The composite-urgency
   score depends on `age_factor(t/sla)`, which changes continuously
   over time. Strawman: recompute on every bead mutation + every
   1h via a sweep job. Acceptable, or do we need finer granularity?
   Subtask 7 owns the implementation; this section flags the
   sequencing question because the default sort UX depends on the
   score being fresh enough.
3. **Default chip on cold load** — `mine` is the obvious default,
   but should anonymous-with-cookie viewers (i.e. dev environments
   without auth-proxy) get `mine` (empty) or `all` (everything)?
   `/active` renders `anonymous` state in this case via the operator
   header chip. Recommend: render the missing-identity banner in
   `anonymous` mode A form pointing at sign-in, and an empty inbox.
   Operator confirmation requested.

---

## 5. Interaction surface

> *Owning bead: `ka-0000002076`. Parent: `ka-0000006157`.*

### 5.1 Problem statement

§§1–4 pin the *display* surface of the inbox-primary view: which intake
the index returns (§1), which subgraph the right pane shows when a row
is clicked (§2–§3), and the chrome around the index (§4). This section
pins the *action* surface — the smallest set of mutations the operator
can drive from the inbox-primary view without bouncing to `/{id}`,
`/decisions`, or the bead-show page.

The contract:

- The inbox-primary view is a triage cockpit. An operator should be
  able to scan, peek, mutate, and move on. Bouncing to `/{id}` is a
  loss-of-place tax that compounds across a 24-intake queue.
- "Mutate and move on" implies a small action vocabulary, not the full
  bead-show CRUD surface. Things that demand a full bead context (long
  description edits, dependency rewiring, owner reassignment, label
  set rewrites) stay on `/{id}` and the inbox links *out* to that
  page. The inbox owns only the actions that resolve a triage step.

The five actions in scope, per the bead description:

1. **Peek** — preview a node's fields and links without leaving the
   page.
2. **Raise / clear `flag:operator-blocking`** — the canonical flag the
   review doc round 3 named for "this needs a human now."
3. **Reparent into an epic** — pull a stray task or escalation under
   the right umbrella so it stops appearing as an action bead not
   under an intake (subtask 6 territory).
4. **Resolve a decision** — pick an option (and submit a free-text or
   artifact response).
5. **Comment** — leave a triage note attached to the bead.

The ordering is the cognitive frequency of these actions in a typical
session, not a UI ordering — the keyboard surface is laid out by
ergonomic adjacency, not action frequency (§5.7).

### 5.2 Per-action endpoint reuse

The viewer's existing handler set covers four of the five actions
cleanly. Reparenting is the one action whose existing endpoint is
ergonomically wrong for the inbox surface; this section names a small
new endpoint shape for it.

| Action                         | Existing endpoint                                                                       | Reuse?       | New shape (only if reuse infeasible)                                                              |
| ------------------------------ | --------------------------------------------------------------------------------------- | ------------ | ------------------------------------------------------------------------------------------------- |
| **Peek**                       | `GET /b/{id}/preview`  ([`bead_preview.go:59`](../../controller/cmd/beads-viewer/bead_preview.go))                | Reuse        | —                                                                                                 |
| **Raise/clear flag**           | `POST /b/{id}/labels`  ([`labels_edit.go:65`](../../controller/cmd/beads-viewer/labels_edit.go))                  | Reuse with caveat | Optional small `POST /b/{id}/flag/{flag}` toggle to avoid re-sending the full label set; see §5.4 |
| **Reparent into epic**         | `POST /b/{id}/description` (rewrites `parent(ka-X)` typed mention)                                                | **No**       | New: `POST /b/{id}/parent` body `parent_id=ka-X` (or empty to clear). See §5.5                    |
| **Resolve decision**           | `POST /b/{id}/decision/resolve`  ([`decisions.go:106`](../../controller/cmd/beads-viewer/decisions.go))           | Reuse        | —                                                                                                 |
| **Dismiss decision**           | `POST /b/{id}/decision/dismiss`  ([`decisions.go:185`](../../controller/cmd/beads-viewer/decisions.go))           | Reuse        | —                                                                                                 |
| **Comment (with preview)**     | `POST /b/{id}/comments` + `POST /b/{id}/comments/preview`  ([`bead_activity.go`](../../controller/cmd/beads-viewer/bead_activity.go))               | Reuse        | —                                                                                                 |

Two clarifications on the table:

- **Peek vs `/{id}`.** `GET /b/{id}/preview` already serves the
  hover-preview fragment used by bead-link popovers across the viewer.
  The inbox peek panel is the same fragment in a docked surface
  instead of a popover (§5.3). One handler, two callers.
- **Decision artifact submission.** The bead description asks "resolve
  a decision (pick option + optional artifact submission)." The
  existing resolve endpoint takes `option_id` *and* `response_text` —
  the latter is the artifact slot. `gb decision report <id>` writes
  the same `response_text` field via a CLI path; the viewer's resolve
  form already exposes both. Inbox reuses as-is. Long-form artifact
  composition (multi-paragraph plan / report) belongs on `/{id}` —
  the inbox provides the form but does not grow into a markdown
  editor.

### 5.3 Peek panel

#### 5.3.1 Trigger

- **Hover** over a constellation node ⇒ the existing hover-preview
  popover (rendered by
  [`templates/static/preview_popover.js`](../../controller/cmd/beads-viewer/templates/static/preview_popover.js)
  via `GET /b/{id}/preview`). Unchanged from bead-show.
- **Click** a constellation node ⇒ the *peek panel*: a docked
  fragment that swaps into a fixed slot in the right pane (below or
  beside the SVG, depending on layout case from §3) and persists
  across hover changes elsewhere. Click the same node again, click
  the panel's close affordance, or press `Esc` to close.
- **Index row click** ⇒ does **not** open peek; it re-renders the
  constellation (§2). Peek is a node-level interaction, not an
  intake-level one.

#### 5.3.2 Fields

The peek panel renders the bead-link preview fragment plus a small
action strip. The fragment already includes:

| Field             | Source on `BeadDetail`                                  | Why it is in peek                          |
| ----------------- | ------------------------------------------------------- | ------------------------------------------ |
| Title + ID        | `Title`, `ID`                                           | Identification                             |
| Type, status      | `Type`, `Status`                                        | What kind of work, what state              |
| Priority          | `Priority`                                              | Triage hint                                |
| Assignee, owner   | `Assignee`, `Owner`                                     | "Whose turn is it?"                        |
| Last updated (relative) | `UpdatedAt` → `humanize.Ago`                       | Staleness                                  |
| Labels (chip row) | `Labels`                                                | `flag:operator-blocking` visibility        |
| Description first paragraph | `Description` head — same as the popover fragment renders | Context without re-render          |
| Open-decision count (if epic) | descendant scan via constellation hydrate    | Pulls forward §3's existing color-fill cue |

The action strip below the fields is the per-node entry point for the
other four actions in this section: a button row keyed `[F]lag`,
`[R]eparent`, `[D]ecide` (only when the focused node is `type=decision`),
`[C]omment`. Each button posts to the corresponding endpoint and
re-renders the peek panel on success.

A "Open in `/{id}`" link (`a href="/{id}"`) in the panel header
preserves the existing escape hatch for actions outside the inbox
vocabulary.

#### 5.3.3 Why a docked panel and not a sidesheet / overlay

Two alternatives considered and rejected:

- **Floating popover** (the existing hover preview). Already used for
  hover. Rejected for click because a floater that disappears on the
  next hover prevents the operator from acting on the previewed bead
  — they cannot click a button without the popover dismissing.
- **Right-edge sidesheet** (slides in from the page edge, full-height
  modal-ish). Rejected because it occludes the constellation, which
  defeats the inbox's "scan-and-act in one viewport" contract. The
  docked-below variant keeps the SVG visible.

The docked region's exact size and placement is non-load-bearing
(§3 owns layout); the contract for §5 is "click → persistent
single-node panel that hosts the action strip."

### 5.4 Flag toggle

#### 5.4.1 The action

Toggle `flag:operator-blocking` on/off for the currently-peeked node.
On = label appears in the bead's label set; off = absent.

The flag's *meaning* is owned by the parent escalation review doc
(`escalation-beads-review-2026-05-05.md` round 3 §"Operator-blocking
flag"). This section does not redefine it; it pins the *interaction*
that raises and clears it.

#### 5.4.2 Endpoint shape

The viewer already exposes `POST /b/{id}/labels` for label rewrites
([`labels_edit.go:65`](../../controller/cmd/beads-viewer/labels_edit.go)).
That endpoint takes a *full* label set as form input — to add a flag,
the caller has to supply the existing set + the new one; to remove,
the existing set minus one.

This is workable but ergonomically wrong for an inbox click:

- The viewer would have to round-trip the bead to read its current
  labels, mutate the slice client-side, and POST back. That is two
  daemon hops per click and a TOCTOU race against any concurrent
  label edit on `/{id}`.
- The semantic intent ("toggle this one flag") is lost in a payload
  that looks like a full rewrite — making per-event auditing harder.

Two options for the inbox surface:

- **5.4.2-A — reuse `POST /b/{id}/labels`.** Viewer reads the
  current label set in the same request that renders peek (cheap;
  the daemon already returns `Labels` on `BeadDetail`), keeps it in
  the peek panel's hidden form, and submits the modified set on
  toggle. **Cost: TOCTOU race window between peek render and
  toggle.** Mitigation: defensive read-modify-write with a 409 on
  divergence, falling back to a re-fetch + re-render.
- **5.4.2-B — small new endpoint `POST /b/{id}/flag/{flag}`** with
  body `value=on|off`. Server reads current labels, idempotently
  adds/removes the named label, writes the result. Audited as a
  flag-toggle event rather than a label rewrite. **Cost: one new
  handler (~40 lines, same shape as `handleClaimBead` in
  [`bead_actions.go:143`](../../controller/cmd/beads-viewer/bead_actions.go)).**

**Recommend 5.4.2-B.** The cost is small, the audit-trail clarity is
worth it (operator-blocking flag changes are exactly the events the
escalation pipeline wants to surface), and the TOCTOU mitigation in
5.4.2-A is more code than 5.4.2-B's handler. The endpoint is
generalised across `flag/*` labels — `flag:snoozed` (subtask 5
backlog), `flag:on-call-relevant`, etc., reuse the same handler.

The `{flag}` path segment is allowlisted server-side to a fixed set
to prevent the endpoint becoming a generic label scribble surface.
v1 allowlist: `operator-blocking`. Future flags require a code change
(intentional friction).

#### 5.4.3 Optimistic UI

Toggling the flag flips the label-chip's class immediately
(`label-flag-active` ↔ `label-flag-inactive`) and sends the POST in
the background via htmx. On non-2xx, htmx swaps an inline error
banner into the peek panel's footer and reverts the chip class. This
matches the `handleResolveDecision` pattern of inline error rendering
([`decisions.go:248`](../../controller/cmd/beads-viewer/decisions.go)).

### 5.5 Reparent

#### 5.5.1 The action

Move the focused bead under a different epic. *Reparenting* in the
beads model means rewriting the `parent(ka-X)` typed mention in the
bead's description body. The viewer has no endpoint for this today —
the only way to reparent through the viewer is via
`POST /b/{id}/description`, which round-trips the entire description
and conflicts with anything else editing the body.

#### 5.5.2 Why a new endpoint is required

The existing description endpoint is wrong for the inbox in three
ways:

1. **Round-tripping the full body** is a TOCTOU race for any
   concurrent description edit, and the diff is unreviewable —
   server logs see "description rewritten" rather than "parent
   changed."
2. **The viewer has no parser for the typed-mention syntax** — that
   logic lives kbeads-side
   (`kbeads/internal/store/typed_mentions*.go`). Re-implementing the
   regex client-side risks drift from the canonical syntax (per
   `kbeads/docs/research-typed-mention-syntax.md`).
3. **Audit-trail.** A reparent event has a stable downstream
   consumer — the constellation needs to invalidate the old graph,
   the index recomputes membership. A description-update event
   buries the signal.

#### 5.5.3 Endpoint shape

```
POST /b/{id}/parent
form: parent_id=ka-XYZ            # set / move
form: parent_id=                  # clear (rare; mostly a recovery affordance)

→ 200: re-render peek panel with new parent + breadcrumb
→ 400: invalid parent_id (not a bead id, would create a cycle)
→ 404: parent bead not found
→ 409: bead already has a typed-mention description that this
        endpoint declines to rewrite (e.g. multi-parent edge case;
        operator falls back to /{id}/description). Inline error
        names the workaround.
```

The handler does the typed-mention rewrite *server-side* via the
kbeads daemon. Two implementation paths:

- **5.5.3-A — viewer-side rewrite** (same shape as
  `handleUpdateDescription` but mutating only the parent mention).
  Keeps the daemon contract unchanged. Cost: the viewer has to
  re-implement the typed-mention regex, accepting drift risk.
- **5.5.3-B — daemon-side endpoint.** Add `PATCH
  /v1/beads/{id}/parent` to kbeads itself; viewer calls through. The
  daemon owns the typed-mention regex and emits a `parent-changed`
  event. Cost: kbeads change.

**Recommend 5.5.3-B.** This subtask should *file* the kbeads endpoint
as a follow-up bead and design the viewer endpoint as a thin shim that
calls through. The kbeads change is small (~80 lines including event
emission and tests) and the typed-mention parser is single-source.

If 5.5.3-B slips, the v1 inbox can ship 5.5.3-A behind a "parent
edits trip a server-side validation pass" guard that re-parses the
rewritten description through the daemon's typed-mention extractor;
divergence rejects the write.

#### 5.5.4 Picker UX

Reparent surfaces a small typeahead picker bound to the existing
cmdk bead-search palette
([`cmdk.go`](../../controller/cmd/beads-viewer/cmdk.go) /
[`templates/static/cmdk.js`](../../controller/cmd/beads-viewer/templates/static/cmdk.js))
*scoped to `type=epic` results*. The cmdk overlay already supports
in-place picking and keyboard navigation; the inbox does not invent
a new picker. Pressing `R` on a peeked node opens cmdk in
parent-pick mode; selecting a result POSTs to `/b/{id}/parent` and
the overlay closes.

### 5.6 Resolve / dismiss decision

The viewer's existing decision endpoints are reused unchanged:

- `POST /b/{id}/decision/resolve` accepts `option_id` and
  `response_text`.
- `POST /b/{id}/decision/dismiss` accepts an optional `reason` and
  closes the bead with `fields.canceled=true`.

In the inbox, when the focused node is `type=decision`, the peek
panel's action strip surfaces both:

- A radio group of the decision's `options` (rendered from the
  bead's `fields.decision.options` JSON, same as the bead-show
  resolve form in
  [`decisions.go:262`](../../controller/cmd/beads-viewer/decisions.go)
  `renderDecisionPartial`).
- A `response_text` textarea, optional unless the chosen option's
  `artifact_type` is set (in which case the form makes it required;
  enforcement is server-side already).
- A "Dismiss" button gated by a confirm step (§5.8).

#### 5.6.1 Artifact submission

The bead description's "optional artifact submission" line refers to
the `response_text` field, which doubles as the artifact body when
the option's `artifact_type` is set (`plan`, `report`, `checklist`,
etc.). The existing resolve form covers this; inbox reuses as-is.

For artifacts that need to land *after* the decision is resolved
(rare — the operator wants to ship the artifact later), the
existing CLI path `gb decision report <id>` writes the same
`response_text` field. The inbox links *out* to that with a "submit
artifact later" affordance on the post-resolve view; it does not
itself host a post-resolve composer in v1.

#### 5.6.2 Optimistic UI

Same pattern as flag-toggle: the action strip swaps to a "resolving…"
state on submit, and on success swaps to the resolved-decision card
(re-rendered by the existing
[`renderDecisionPartial`](../../controller/cmd/beads-viewer/decisions.go)).
On non-2xx, the strip re-renders with the inline error and preserves
the operator's draft response.

### 5.7 Comment

`POST /b/{id}/comments` + `POST /b/{id}/comments/preview` are reused
unchanged. The peek panel's `[C]omment` button opens the existing
comment composer fragment inline below the action strip:

- Markdown textarea + preview toggle (existing
  [`comment_edit.go`](../../controller/cmd/beads-viewer/comment_edit.go)
  partial).
- Submit posts to `POST /b/{id}/comments`; on success the comment
  appends to a small "recent comments" list at the bottom of the
  peek panel and the textarea clears.
- The composer pre-fills with any comment-draft saved by the
  existing `templates/static/drafts.js` localStorage shim, so a
  half-typed comment survives a peek-close / peek-reopen cycle.

The inbox does *not* expose comment edit / delete (no existing
endpoints; out of scope for this section).

### 5.8 Keyboard model

The inbox-primary view runs a **two-mode** keyboard model. Mode is
implicit (defined by what is focused), not explicit (no Vim-style
modal switch).

- **Index mode** — left-rail row focus. Reuses
  [`templates/static/keys.js`](../../controller/cmd/beads-viewer/templates/static/keys.js)
  bindings: `j`/`k` move row focus, `Enter`/`o` open the row's
  constellation (right-pane swap, no navigation), `gg`/`G` jump,
  `/` focuses the inbox-search filter, `?` opens the help overlay.
- **Constellation mode** — right-pane node focus, entered when the
  operator clicks a node or presses `Tab` from index mode.

#### 5.8.1 Constellation-mode bindings

| Key  | Action                                  | Reuse / new                                    | Notes                                                                          |
| ---- | --------------------------------------- | ---------------------------------------------- | ------------------------------------------------------------------------------ |
| `h` / `l` | Move node focus across the lane / ring | New                                          | Lane semantics from §3 layout                                                  |
| `j` / `k` | Move node focus inside a lane / depth ring | New                                       | Mirrors index-mode `j`/`k`                                                     |
| `Enter`   | Toggle peek panel for focused node      | New                                            | Click-equivalent                                                               |
| `Esc`     | Close peek panel; second `Esc` returns to index mode | Reuse keys.js Esc dispatcher       | —                                                                              |
| `Tab`     | Switch between index / constellation mode | New                                          | Predictable focus pivot                                                        |
| `p`       | Focus the peek panel (after open)       | New                                            | Frees `Enter` to mean toggle                                                   |
| `f`       | Toggle `flag:operator-blocking` on the focused node | New                                | §5.4 endpoint                                                                  |
| `r`       | Open reparent picker (cmdk overlay) for focused node | New                               | §5.5; cmdk handles `Enter`/`Esc` internally                                    |
| `d`       | Open decision-resolve form (only when focused node is `type=decision`) | New              | §5.6                                                                           |
| `c`       | Focus the comment textarea on the peek panel | Reuse `c` binding, scoped to peeked node  | keys.js `case 'c'` already focuses `comment-text`; works as-is when peek is open |
| `?`       | Help overlay                            | Reuse keys.js                                 | Help overlay extends to list these new bindings                                |

`g`-prefix navigation (`gi`, `gn`, etc.) for cross-page jumps stays
on the keys.js dispatcher; subtask 4 §4.5 already rebinds `gi` →
`/inbox` and adds `gn` for the new view's namesake. This section
adds nothing new at the prefix level.

#### 5.8.2 Why these letters

- `f`, `r`, `d`, `c` are the first letters of the actions and unused
  in keys.js's existing single-key bindings (j/k/o/Enter/G//?/c/g).
- `c` is reused (it focuses the comment textarea today) and the
  semantic ("focus comment composer") is preserved — peek panel's
  composer reuses the `comment-text` element id, so keys.js's
  `case 'c'` in
  [`keys.js:321`](../../controller/cmd/beads-viewer/templates/static/keys.js)
  works without modification.
- `f` is *not* used by the existing keyboard surface (no flag-toggle
  on `/active` today). Adding it is a green-field binding.
- `Tab` is reused — keys.js does not currently bind it, so default
  browser focus-cycling stays for form fields. Constellation-mode
  `Tab` only fires when no input is focused, mirroring keys.js's
  existing `if (isTypingTarget(e.target)) return;` guard.

#### 5.8.3 Reuse vs. new code

All of the new bindings live in a sibling JS file (proposed name
`templates/static/inbox_keys.js`)
that *augments* `keys.js` rather than forking it. The augmenter
imports keys.js's helpers (`isTypingTarget`, `focusComment`) and
attaches a second `keydown` listener that handles only the
constellation-mode bindings. Index-mode bindings on `/inbox` flow
through keys.js unchanged.

### 5.9 Multi-select / bulk operations

**Defer for v1.** Rationale:

- The triage workflow this section is built for is *one-at-a-time*
  judgment ("this intake — flag, comment, move on; next intake —
  resolve, comment, move on"). Bulk operations are useful when the
  predicate has *false positives* the operator wants to clear in a
  sweep. The inbox's predicate (§1) is already tight enough that
  false positives are rare — the cold-load row count is bounded at
  ~24 in the median operator's session.
- Bulk endpoints already exist on the per-project page
  (`/p/{project}/bulk/status`, `/p/{project}/bulk/labels`,
  `/p/{project}/bulk/assignee` — see [`server.go:483-485`](../../controller/cmd/beads-viewer/server.go))
  for the cases the inbox would want; if v2 picks up bulk it can
  reuse those endpoints under `/inbox/bulk/*` aliases. The inbox UI
  does not need to re-design them.
- Multi-select chrome (checkboxes per row, sticky footer with bulk
  actions) is non-trivial keyboard surface — `x` to toggle a
  selection, `*a`/`*n` to select all/none, etc. Adding it to v1
  makes the keyboard model meaningfully heavier, and the most likely
  bulk action ("mark these N as not-mine") collapses to either
  fixing the predicate (subtask 1) or flagging-then-clearing (which
  is the §5.4 single-action shape repeated).

**v1 scope:** single-select only. The peek panel always reflects one
focused node; actions apply to that node.

**v2 scope (filed but not built):** if operators report bulk-flag /
bulk-resolve as a need after the inbox lands, a follow-up bead under
the umbrella epic adds multi-select against the §5.4/§5.6 endpoints
plus an inbox-side `/inbox/bulk/labels` proxy for the existing
project-bulk handler.

### 5.10 Confirmation and undo for destructive actions

#### 5.10.1 Categorisation

| Action                              | Reversible? | Confirmation gate     | Undo affordance                               |
| ----------------------------------- | ----------- | --------------------- | --------------------------------------------- |
| Peek                                | Trivial     | None                  | N/A                                           |
| Raise/clear flag                    | Yes (re-toggle)        | None              | Re-press `f`; toast "flag set / cleared, undo" with 30 s window |
| Reparent                            | Yes (set previous parent) | Light: typed-mention preview in peek before submit | Toast with "undo" reverts `parent_id` for 30 s |
| Resolve decision                    | **No** (resolve event is recorded; closing reopens the bead but does not unwind downstream Slack notifications) | **Hard: confirm-on-submit modal naming the chosen option** | None — the toast says "decision resolved; visible to thread"; undo is not offered |
| Dismiss decision                    | Soft (dismiss → reopen via `/b/{id}/reopen`) | Light: confirm modal with reason field | Toast with "reopen" reverts for 30 s |
| Comment                             | Soft (delete via `/{id}` only) | None                | None — the toast says "comment posted"; edit/delete is on `/{id}` |

#### 5.10.2 Patterns

- **Light confirm** — htmx-driven `hx-confirm` with a one-line
  modal, used for reparent and dismiss. The modal restates the from
  → to (reparent) or shows the dismiss reason input (dismiss). No
  blocking timeouts. Default action is the destructive one so the
  operator can `Enter` through.
- **Hard confirm** — full modal with `Cancel` / `Resolve as <option>`
  buttons, used for decision resolve. The modal restates the option
  text and the response artifact (if any). The keyboard binding (`d`)
  opens the resolve form *with* the modal pre-built; the operator
  reviews, presses `Enter` to submit, `Esc` to cancel.
- **Toast undo** — bottom-corner toast with 30 s countdown and an
  inline undo button. Implemented as an htmx OOB swap targeting a
  fixed `#toast-region` element. Pressing `u` while the toast is
  visible triggers undo. The toast self-dismisses on timeout.

The 30 s window is a hand-tuned compromise: long enough for the
operator to realise a misclick after scanning the next intake, short
enough that the undo button is not a permanent escape hatch (which
would otherwise complicate audit-trail expectations).

#### 5.10.3 Why no undo for decision resolve

A resolve event triggers downstream notifications (Slack thread
update, agent inbox poke, decision-resolution rollup). Undoing the
resolve does not unwind those side effects — at best it reopens the
bead, at worst it produces a "resolved → reopened" sequence that
confuses any downstream automation. Confirm-before-submit is the
load-bearing safety, not a post-hoc undo.

### 5.11 Reuse map

| Inbox interaction      | Existing primitive                                                                                          | Inbox use                                            |
| ---------------------- | ----------------------------------------------------------------------------------------------------------- | ---------------------------------------------------- |
| Hover preview          | `GET /b/{id}/preview` ([`bead_preview.go`](../../controller/cmd/beads-viewer/bead_preview.go))                                 | Constellation node hover, unchanged                  |
| Peek panel fragment    | `GET /b/{id}/preview` rendered into a docked slot                                                                                              | Click-target for constellation nodes                 |
| Decision form          | `renderDecisionPartial` in [`decisions.go`](../../controller/cmd/beads-viewer/decisions.go)                                  | Embedded in peek panel when focused node is decision |
| Comment composer       | [`comment_edit.go`](../../controller/cmd/beads-viewer/comment_edit.go) partial                                                                  | Embedded in peek panel via `[C]` button              |
| cmdk overlay           | [`cmdk.go`](../../controller/cmd/beads-viewer/cmdk.go) + [`templates/static/cmdk.js`](../../controller/cmd/beads-viewer/templates/static/cmdk.js)       | Reparent picker, scoped to `type=epic` results       |
| Keyboard dispatcher    | [`templates/static/keys.js`](../../controller/cmd/beads-viewer/templates/static/keys.js)                                                       | Index-mode bindings unchanged; constellation-mode in sibling file |
| Bulk endpoints         | `POST /p/{project}/bulk/{status,labels,assignee}` ([`server.go:483-485`](../../controller/cmd/beads-viewer/server.go))      | Filed but not used in v1; alias slot for v2          |
| Optimistic-UI pattern  | `handleResolveDecision` inline-error pattern ([`decisions.go:248`](../../controller/cmd/beads-viewer/decisions.go))         | Re-applied to `/b/{id}/flag/{flag}` and `/b/{id}/parent` |

#### Cross-references filed (or to be filed when this section lands)

- **`POST /b/{id}/flag/{flag}` viewer endpoint** — small new
  handler. New bead: *"Add `POST /b/{id}/flag/{flag}` toggle
  endpoint to beads-viewer for inbox flag actions"* (parent:
  ka-0000006157; depends on §5.4 design lock).
- **`PATCH /v1/beads/{id}/parent` kbeads endpoint + viewer shim
  `POST /b/{id}/parent`** — daemon-side typed-mention rewrite. New
  bead: *"Add `parent` PATCH endpoint to kbeads + reparent shim in
  beads-viewer"* (parent: ka-0000006157; depends on §5.5 design
  lock; cross-links the typed-mention research doc
  `kbeads/docs/research-typed-mention-syntax.md`).
- **`templates/static/inbox_keys.js` augmenter** — sibling JS file
  for constellation-mode bindings. Filed under §5.8; lands together
  with the `/inbox` template work in subtask 7.
- **Toast region + undo affordance** — shared chrome (the toast
  region and the 30 s undo state machine) is reusable on `/active`
  too. Filed as: *"Add toast-with-undo affordance to beads-viewer
  shell"* (parent: ka-0000006157; not a hard inbox dependency,
  could ship after v1 with a no-op undo).

### 5.12 Out of scope

Pinned for the right subtask:

- **Per-row inline actions on the index** — the index uses the
  `project_row` partial (per §4) and does not grow per-row buttons.
  Any per-row action (snooze, archive) belongs on the peek panel,
  not the row.
- **Snooze** — predicate-side handling deferred per §4.3.2 (review
  doc 3R2-E); action-side handling deferred to subtask 5 backlog.
- **Reassign owner / assignee from the inbox** — the existing
  `POST /b/{id}/owner` and `POST /b/{id}/assignee` endpoints work
  but the workflow ("change who is on this") is bead-show
  territory, not triage territory. Out of scope for v1.
- **Edit description from the inbox** — outright too heavy for the
  triage cockpit. Operator clicks "Open in `/{id}`" to escape.
- **Multi-bead reparent** — depends on multi-select (§5.9 deferred).
- **Live SSE updates of the peek panel** — when a decision is
  resolved by another operator while peeked, the panel currently
  re-renders only on the next click. Live invalidation is a perf
  / SSE concern (subtask 7) and is filed there.

### 5.13 Open questions for operator review

1. **`POST /b/{id}/flag/{flag}` vs. `POST /b/{id}/labels` reuse** —
   §5.4 recommends the new endpoint. Operator call: is the audit /
   ergonomics gain worth a new viewer handler, or do we ship v1 on
   the existing `POST /b/{id}/labels` with the TOCTOU mitigation
   and revisit only if the flag-toggle becomes a hot path?
2. **`PATCH /v1/beads/{id}/parent` kbeads endpoint** — §5.5
   recommends adding the daemon endpoint rather than rewriting the
   typed mention viewer-side. The kbeads change is small but it is
   a *new* daemon write surface; operator confirmation requested
   before this section files the kbeads bead.
3. **30 s undo window for reparent and flag toggle** — is 30 s the
   right default, or should we mirror Gmail's 5 s "send undo" /
   30 s setting model and let the operator pick? Defaulting to
   30 s with no setting on v1; a setting can land later if
   operators ask.
4. **Hard-confirm modal for decision resolve** — §5.10 names this
   as the load-bearing safety. The bead-show resolve form today
   does *not* hard-confirm — it submits inline. Should the inbox
   raise the bar (hard-confirm) and leave bead-show as inline, or
   raise both surfaces? Operator preference.
5. **Constellation-mode `Tab` to switch panes** — this is a new
   binding on a key the browser owns by default. Is the friction
   of overriding `Tab` worth it, or should we use a non-default
   chord (e.g. `g r` for "go right pane" / `g l` for "go left
   rail")? `Tab` is faster but less discoverable; `g r`/`g l` is
   slower but consistent with the existing `g`-prefix navigation.
   Recommend `Tab` with a `?`-help mention; operator confirmation
   requested.
## 6. Action beads not under an intake (orphan decisions / escalations)

> *Owning bead: `ka-0000001922`. Parent: `ka-0000006157`.*

### 6.1 Problem statement

The index defined in §1 is `category=intake AND operated_by* matches
viewer`. Drilling renders the intake's descendant subgraph (§2). Every
action the operator can take in the inbox-primary view is therefore
reachable only by going through an intake row.

But D7's `DeriveCategory` table (`ka-0000003787`, rule 1) classifies
**all** `decision`/`escalation` beads as `category=action`, regardless
of whether their parent chain reaches a `category=intake` ancestor.
And the `flag:operator-blocking` bug rule (rule 2) does the same for
some bugs. In practice this means a non-trivial slice of the action
queue lives outside any intake's descendant closure:

- Decisions raised by an agent against another *intermediate* task or
  bug (parent chain ends at an unparented `task` — D7 rule 7 routes it
  to `intermediate`, not `intake`). Common in scheduled-gardener churn:
  a gardener spawns a task, the task surfaces a decision, the task
  itself is not parented.
- Escalations filed via `kd escalation create` with no `parent`
  argument. Common in `gb decision create` from agents that do not
  know which intake to attribute the work to.
- Operator-blocking bugs filed by `kd bug` from a CLI session that did
  not specify a parent.
- Decisions whose only structural connection to an intake is a typed
  *mention* (e.g. `relates-to(ka-X)`) rather than a parent-child dep
  edge — §2 only walks parent-child + reverse-deps, so mention-only
  links don't pull a decision into an intake's subgraph.

Without a fix, these beads are **invisible** in the inbox-primary
view: the index does not list them (they are not `category=intake`),
and they do not surface from any intake's drill (no parent chain to
walk). For an inbox surface that aspires to be the cold-load default
(§8), this is unacceptable — every decision/escalation that would
have routed to the human via the legacy `/decisions` page must remain
reachable.

This section pins the resolution.

### 6.2 Defining "orphan" — what counts

An **orphan action bead** is any bead `b` satisfying:

1. `DeriveCategory(b) = action` (i.e. D7 rule 1 — `type ∈ {decision,
   escalation}` — or rule 2 — `type=bug AND label
   flag:operator-blocking`).
2. `b.status ∈ {open, in_progress}` (closed action beads are
   informational; they belong on a history surface, not the inbox).
3. The transitive parent-child walk from `b` reaches no bead `a` with
   `DeriveCategory(a) = intake`. Equivalently: no ancestor of `b`
   along `deps WHERE type='parent-child'` has `category=intake`.
4. The viewer's identity (per §1's resolver — `viewer_operator_id` in
   v1 or `identity_ids` set in v0.5) matches `b.operated_by` /
   `b.operated_by_id` under the same predicate the index uses.

A bead `b` is **not** an orphan if any ancestor in the parent-child
chain is `category=intake`, even if none of `b`, the chain, or the
ancestor are reachable in `b`'s SVG render under the §2 caps. The
discriminator is graph reachability via parent-child, not whether the
§2 fanout caps would *display* the chain — the inbox must not surface
a decision twice (once under its real intake, once as orphan).

`relates-to` / typed-mention edges are **not** counted as ancestors.
A decision that merely mentions an intake is still an orphan if its
parent chain does not reach one. This matches D7's "no parent walk
across mention edges" rule (D7 §2 footnote on rules 5–7) and keeps
the orphan set well-defined: the predicate is on `parent-child` deps
only, identical to the existing `WITH RECURSIVE subtree` query in
`kbeads/internal/store/postgres/queries.go:294`.

### 6.3 Pick — option A (pseudo-intake row)

**Pick A.** Surface orphan action beads as a synthetic **pseudo-intake
row** in the index with id `inbox:orphans:<scope>` (e.g.
`inbox:orphans:mine`, `inbox:orphans:on_call`). Clicking the row
opens a flat constellation in the right pane with the synthetic node
at the center and every orphan action bead drawn as an L1 child.

Rationale:

- **Preserves the index↔subgraph contract.** The whole shell built in
  §4 is "scan rows in the left rail, click, see a constellation in
  the right pane, go back, pick the next." A pseudo-intake row is
  *the same affordance* — the operator's eye and keyboard model do
  not change. A banner (option C) would carve out a second, smaller
  affordance the operator must learn to scan separately. A taxonomy
  fix (option B) would push the cost onto every bead-creation
  surface (CLI, Slack bridge, Jira bridge, GitLab bridge, web
  viewer) and onto a one-time backfill — see §6.8 for the cost
  estimate.
- **Composes cleanly with the chip variants from §4.3.** Each chip
  (mine / team / on-call / raise-only) gets its own
  `inbox:orphans:<chip>` row computed against the same predicate
  variant the chip pins on real intakes. No new chip semantics.
- **Composes cleanly with the layout from §3.** The right pane
  renders the synthetic node as a center bead with the §3 concentric
  rings collapsed to a single L1 ring of orphan action beads.
  Category sectors still apply: every orphan is `category=action` so
  every node lands in the action sector (top-of-ring); the ring is
  uniformly colored, which doubles as a visual signal that the user
  is viewing the orphan set, not a real intake's subgraph.
- **Reuses the §2 fan-out caps verbatim.** The "L1 children" of the
  synthetic intake are capped at `constellationMaxReverse = 6` per
  ring, with overflow handled by §2.5's pruning order and "+N more"
  pip. No new node-cap constants. Larger orphan sets paginate via the
  existing depth-slider escape hatch (raise to N=2; orphans expand
  outward as if the synthetic node had a deeper subtree — useful for
  decisions that themselves chain to sub-decisions).
- **Synthetic id is namespaced.** `inbox:orphans:*` is a viewer-side
  identifier, not a bead id. It never collides with real `kd-*`
  ids (the prefix is reserved by the URL router; see §6.6). This
  keeps the back-end honest — no synthetic row is ever written to the
  `beads` table.

### 6.4 Why not option B (taxonomy fix), why not option C (banner)

**Option B — require all action beads to have an intake ancestor.**
Tempting because it makes the inbox-primary contract trivially clean
(the orphan set is empty by construction). Rejected for v1; not
ruled out as a v2+ direction.

Costs:

- **Write-path enforcement.** Every bead-creation surface needs a new
  validation: reject `decision` / `escalation` / `flag:operator-blocking
  bug` without a parent that resolves (after walk) to a
  `category=intake` ancestor. This means CLI (`kd decision create`,
  `kd escalation create`, `gb decision create`), Slack bridge (decision
  emitted from a Slack thread without a bead context), Jira bridge,
  GitLab bridge, web viewer, and any future surface — at least 5–7
  call sites — gain a parent-resolution check on the hot creation
  path. Each one needs a graceful "couldn't find an intake" fallback
  (auto-create a synthetic intake? bounce to the operator? assign to
  a default intake?) and that fallback is itself a design fork.
- **Backfill.** A one-time migration walks every open action bead's
  parent chain and re-parents the orphans under either (a) a
  newly-created intake, (b) an existing nearby intake, or (c) a
  catch-all fallback intake. With the current ~60-bead open action
  queue this is small but *not* mechanical: option (a) clutters the
  intake index with one synthetic row per orphan; option (b) requires
  human judgment per bead; option (c) is functionally identical to
  the pseudo-intake row option A — the catch-all *is* the pseudo
  intake, just persisted as a real bead with a real id. The
  pseudo-intake row in option A is option B's catch-all without the
  migration tax.
- **Coupling to D7.** D7 rule 1 (`type∈{decision,escalation} →
  action`) would need a co-amendment that prohibits orphan decisions
  on the write path. That makes `DeriveCategory` non-pure: the
  function would have to either reject parents at write time or
  return a different category for legitimately-parentless decisions
  (e.g. `category=meta-decision`, which dilutes the action lens).

If the operator later prefers option B (e.g. because the orphan rate
in production turns out to be high and option A's pseudo row
accumulates too many beads to scan), the migration plan in §6.8
captures the steps to flip. The point of option A is that it does not
foreclose option B — both can coexist transitionally, and the
synthetic row's count badge (§6.7) makes the orphan rate observable
so the operator can decide.

**Option C — banner / separate list at the top of the inbox.**
Rejected. A banner is a different affordance from the rest of the
inbox: the operator's keyboard model (j/k through index rows, Enter
to drill, Esc to return) is built around row navigation in the left
rail. A banner sits above the rail and breaks the model. Worse, a
banner's natural shape is a 1-line summary ("12 unparented
decisions"); turning that into the same drill-into-a-constellation
affordance the rest of the inbox uses requires mounting a second list
beneath the banner, which is just option A with worse ergonomics.

If the operator wants a sticky reminder of the orphan rate (e.g. *"42
orphans, you should triage these"*), the count badge on the option A
pseudo row (§6.7) is the same signal in the same place, without the
chrome cost.

### 6.5 Sample query for the orphan set

Two query variants, one per Option B sequencing tier (matching §1.2
and §1.3). Both reuse the existing
`WITH RECURSIVE subtree` shape from
`kbeads/internal/store/postgres/queries.go:294` so no new query
primitive is needed.

The walk inverts the existing primitive: instead of "find the
descendants of `:anchor` via parent-child deps", we walk *upward*
from each candidate action bead to test whether any ancestor is
`category=intake`. The query returns the action beads whose ancestor
walk has no intake hit.

#### 6.5.1 v1 query (post Option B P1+P2)

Assumes `category` is computed server-side per D7 plan (`ka-0000003787`)
and exposed as a stamped column on the response — but the SQL still
projects category derivation inline because the column is virtual.

```sql
WITH viewer AS (
  SELECT :viewer_operator_id::int8 AS oid
),
action_candidates AS (
  -- D7 rule 1 + rule 2: action category
  SELECT b.id, b.type, b.status
  FROM   beads b, viewer
  WHERE  b.deleted_at IS NULL
    AND  b.status IN ('open', 'in_progress')
    AND  b.operated_by_id = viewer.oid
    AND  (
           b.type IN ('decision', 'escalation')
        OR (b.type = 'bug' AND b.labels ? 'flag:operator-blocking')
         )
),
ancestor AS (
  -- Walk parent-child upward from each action candidate.
  SELECT ac.id AS root_id, d.depends_on_id AS ancestor_id
  FROM   action_candidates ac
  JOIN   deps d ON d.bead_id = ac.id AND d.type = 'parent-child'
  UNION  ALL
  SELECT a.root_id, d.depends_on_id
  FROM   ancestor a
  JOIN   deps d ON d.bead_id = a.ancestor_id AND d.type = 'parent-child'
),
roots_with_intake_ancestor AS (
  -- Roots whose ancestor walk reaches at least one category=intake.
  SELECT DISTINCT a.root_id
  FROM   ancestor a
  JOIN   beads anc ON anc.id = a.ancestor_id
  LEFT   JOIN beads par ON par.id = (
           SELECT depends_on_id FROM deps
            WHERE bead_id = anc.id AND type = 'parent-child'
            LIMIT 1
         )
  WHERE  anc.deleted_at IS NULL
    AND  -- Inline category=intake test from D7 rule 4 / rule 5:
         (
           (anc.type = 'epic' AND par.id IS NULL)                                           -- rule 4
        OR (anc.type = 'epic' AND par.id IS NOT NULL AND par.type NOT IN ('epic'))          -- rule 4 (parent not epic/intermediate)
        OR EXISTS (SELECT 1 FROM jsonb_array_elements_text(anc.labels) l
                   WHERE l LIKE 'source:%')                                                 -- rule 5
         )
)
SELECT b.*
FROM   action_candidates ac
JOIN   beads b ON b.id = ac.id
WHERE  ac.id NOT IN (SELECT root_id FROM roots_with_intake_ancestor)
ORDER  BY b.priority ASC, b.updated_at DESC
LIMIT  :page_size OFFSET :page_offset;
```

Notes:

- The recursive CTE caps in PostgreSQL (default `work_mem` 4MB; no
  cycle protection in standard recursive CTEs) are tolerable because
  open action queue size today is `~60` beads and the parent-child
  graph depth maxes at 4–5 levels in practice. If the walk ever
  blows up, add `CYCLE id SET is_cycle USING path` (PG14+) — every
  prod DB the project runs on is PG15+.
- The `roots_with_intake_ancestor` subquery does the "is this
  ancestor an intake?" test inline by re-deriving D7 rules 4 and 5.
  When `DeriveCategory` lands as a stored function or a generated
  column (D7 plan §4 says it is a pure function in
  `kbeads/internal/server/category.go`), this block collapses to
  `WHERE category(anc.id) = 'intake'`. The inlined form is
  intentional — it lets the orphan query ship before the D7 plan's
  SQL exposure lands.
- D7 rule 6 (`type∈{task,bug} AND parent intermediate → intermediate`)
  and rule 7 (`type∈{task,bug} AND no parent → intermediate`) are
  *not* re-derived here because the only ancestors we care about are
  intake-classified; the moment the walk hits an intake we stop.
  Rule 8 (config-types) is irrelevant — config beads don't appear
  on the parent chain of an action bead in practice.

#### 6.5.2 v0.5 query (pre Option B)

Identical shape; the `viewer` CTE substitutes the v0.5 identity
expansion from §1.3.2:

```sql
WITH viewer AS (
  SELECT :identity_ids::text[] AS ids
),
action_candidates AS (
  SELECT b.id, b.type, b.status
  FROM   beads b, viewer
  WHERE  b.deleted_at IS NULL
    AND  b.status IN ('open', 'in_progress')
    AND  b.operated_by = ANY(viewer.ids)
    AND  (
           b.type IN ('decision', 'escalation')
        OR (b.type = 'bug' AND b.labels ? 'flag:operator-blocking')
         )
)
-- … remainder identical to §6.5.1 (ancestor walk, intake test, NOT IN).
```

Drop-in. The ancestor walk does not depend on the
`operated_by` form, so the only diff is the candidate filter. Hits
`idx_beads_operated_by` (the partial index from §1.3.3) on the
candidate scan.

#### 6.5.3 Index requirements

- **`deps(bead_id, type, depends_on_id)`** composite — already exists
  in production (used by every parent-child walk in the codebase
  including the §2 subtree query and `WITH RECURSIVE subtree` at
  `queries.go:294`). No new index.
- **`idx_beads_operated_by_id`** — pinned by §1.2.2 for v1.
- **`idx_beads_operated_by`** partial — pinned by §1.3.3 for v0.5.
- **No `idx_beads_category_action_orphan`-style synthetic index.**
  The orphan predicate is a derived join, not a column equality, so
  it cannot be indexed on a single column. The query's hot path is
  the `action_candidates` filter (≪ 100 rows in the operator's set);
  the recursive walk runs over a small candidate seed.

### 6.6 UX mock — index row + right-pane render

#### 6.6.1 Pseudo-intake row in the left rail

The synthetic row sits **last** in the left rail, after all real
intakes, with a divider above it. This placement is deliberate:

- Real intakes are the operator's primary scanning target. Putting
  the orphan row at the top would prepend a cognitive interrupt to
  the hot path. Putting it at the bottom respects the row-by-row
  scan and lets the operator triage real intakes first.
- The row carries a count badge (`42` for "42 orphan action beads
  matching the active chip"); the badge is the discoverability
  hook. If the count is `0`, the row collapses entirely (no row,
  no divider) — most chips will have empty orphan sets.

```
┌────────────────────────────┬────────────────────────────────────────┐
│ Inbox · 24 intakes         │  ⊙ ka-A111 · "RWX cred expired"        │
│ ─────────────────────────  │                                        │
│ [mine] [team] [on-call] [+]│        (subtask 3 layout)              │
│ Sort: urgency · ↓          │                                        │
│ ─────────────────────────  │                                        │
│ ⊙ ka-A111 · RWX cred…  ▸  │                                        │
│ ⊙ ka-B222 · GitLab MR  ▸  │                                        │
│ ⊙ ka-C333 · Slack: …   ▸  │                                        │
│ …                          │                                        │
│ ⊙ ka-Z999 · Jira: …    ▸  │                                        │
│ ─────────────────────────  │                                        │
│ ✦ Unparented action [42]▸  │                                        │
└────────────────────────────┴────────────────────────────────────────┘
```

Row anatomy:

- Glyph: `✦` (different from real intakes' `⊙`) so the row is
  recognisably synthetic at a glance.
- Title: `Unparented action`. Always plural; the count comes from the
  badge, not the title. Localized variant for `raise-only` chip:
  `Unparented (raised by you)`.
- Count badge: square-bracketed integer, right-justified, same color
  as the active chip (so it tracks scope changes).
- Right-arrow drill affordance same as real rows.

The row is keyboard-reachable via the same j/k navigation §4.5
defines; pressing `Enter` from this row invokes the same drill
handler as a real intake but with the synthetic id
`inbox:orphans:<scope>`.

#### 6.6.2 Right-pane render — flat orphan constellation

Clicking `Unparented action` opens this in the right pane:

```
                          ✦ Unparented action
                            (mine · 42 beads)

                     ┌──────────────────────────┐
                     │                          │
       ◇ ka-D444 ───┐│                          │┌─── ◇ ka-D451
       (decision)   ││                          ││    (decision)
                    ││                          ││
       ◇ ka-D445 ───┤│      ✦ (synthetic       │├─── ◇ ka-D452
       (escalation) ││         center node)    ││    (escalation)
                    ││                          ││
       ◇ ka-D446 ───┤│                          │├─── ⚑ ka-B447
       (decision)   ││                          ││    (bug, flagged)
                    ││                          ││
       ◇ ka-D447 ───┘│                          │└─── ◇ ka-D453
       (decision)    │                          │     (decision)
                     └──────────────────────────┘

         Showing 8 of 42 — raise depth slider for more
```

Anatomy:

- **Center node:** the synthetic `✦` glyph + label `Unparented action
  (<scope> · <total> beads)`. Not clickable. Hover preview shows
  *"these beads have no intake ancestor — drill via the row, not the
  node"*.
- **L1 ring:** action beads from the orphan set, sorted by §4.4's
  default sort (composite urgency desc, then `priority` asc, then
  `updated_at` desc). All nodes are `category=action`, so the §3
  category sector is uniformly the *action* sector — visually the
  ring fills the top arc only. This is intentional: the ring shape
  itself signals "this is the orphan set, not a regular intake."
- **Capped at `constellationMaxReverse = 6` by default** with §2.5's
  pruning order and "+N more" pip per overflow. The depth slider
  raises the cap to 18 (one ring expansion); raising past N=2 is
  out-of-scope for v1 since orphans by definition don't have an
  intake-rooted subtree to expand into.
- **Edges:** thin parent-of edges from the synthetic center to each
  ring node, no mention edges. Cycles, multi-parent, and
  cross-intake co-mention rules from §2.6 do not apply (no recursive
  walk).
- **Empty state:** when the chip yields zero orphans, the row
  collapses and the right pane never shows the orphan render. The
  operator sees no evidence of the row's existence — desired.

#### 6.6.3 Per-row peek and action

Hovering an L1 orphan node opens the same peek panel §5 (subtask 5)
defines for any action node — peek metadata, raise/clear
`flag:operator-blocking`, resolve the decision, comment, **plus** a
new action specific to this view: *"Re-parent to intake"*. The
re-parent action opens a typeahead seeded with the same chip
predicate (`mine` / `team` / `on-call`) showing intake candidates;
selecting one writes a `parent-child` dep edge from the orphan to
the chosen intake, after which the bead drops out of
`inbox:orphans:*` on the next refresh.

This action is the operator-side mitigation for option B without
forcing a write-path validation: the operator can opportunistically
re-parent decisions as they triage, draining the orphan set over
time without coordinated migration. Subtask 5 owns the action
surface; this section names the affordance.

#### 6.6.4 URL shape

```
GET  /inbox/orphans/<scope>          # right-pane render
GET  /api/v1/inbox/orphans/<scope>   # JSON (subtask 7 owns shape)
```

`<scope> ∈ {mine, team, on_call, raise_only}` mirrors the chip set
in §4.3.1. The URL stays inside the `/inbox` namespace from §4 so
deep-links survive chip changes. URL state on cold load:
`/inbox?orphans=1` is *not* a special case — the operator clicks
the synthetic row and the URL becomes `/inbox/orphans/mine`. Back
button returns to `/inbox` with the row still selected for
keyboard re-entry.

### 6.7 Cross-cutting concerns

#### 6.7.1 Sentinels (§1.5)

The orphan query inherits sentinel exclusion from §1's
viewer-resolver: the `viewer` CTE returns only human operator ids,
so a `system:scheduler`-operated orphan decision is not in the
candidate set regardless of who is signed in. Test obligation
(parallel to §1.5's): assert that an orphan decision with
`operated_by_id = system:scheduler` does not appear in any
viewer's `inbox:orphans:mine` result.

#### 6.7.2 Cross-platform unification

The same Option B FK collapse §1.6 demonstrates applies here: an
orphan escalation filed via Slack bridge (`operated_by_id = 42`,
matt's operator id) and an orphan decision created via OIDC
(`operated_by_id = 42`) both appear in matt's
`inbox:orphans:mine` because the candidate filter is FK equality
under v1 / `ANY(identity_ids)` under v0.5.

#### 6.7.3 Count badge freshness

The badge count is the same row count the right-pane query
returns. It is computed by the same query the index endpoint runs
to render the row (no separate count-only query). For chips with
small orphan sets (≪ 100), this is cheap. For larger sets the
count is capped at `99+` display-side (same convention as
inbox-row count badges on `/active`). Fresh count is updated on:

- Each chip-switch (re-runs the full predicate).
- Each SSE bead-event for a bead the viewer's predicate matches
  (same channel `/active` already subscribes to).
- Each manual `Cmd-R` / `F5`.

Subtask 7 owns the SSE delta wire shape; this section names the
contract.

#### 6.7.4 Multi-parent and cross-intake decisions

A decision parented to *both* an intake and a non-intake (rare;
multi-parent dep edges) is **not** an orphan — §6.2 rule 3 says
"reaches *no* intake ancestor"; a single intake hit clears the
orphan flag. The walk uses `EXISTS`, so any path to an intake
suffices.

A decision that is **mention-related** (`relates-to`) to an intake
but parented under non-intake beads remains an orphan (§6.2
restricts the walk to `parent-child` deps). The peek-panel
re-parent action (§6.6.3) is the mitigation: the operator
upgrades the mention to a parent-child dep when it makes sense.

#### 6.7.5 Promotion to v1 from v0.5

The v0.5→v1 swap (§1.6.4) preserves orphan membership for the same
reason it preserves intake membership: the `operated_by` →
`operated_by_id` migration is a value rewrite, not a parent-chain
rewrite, so the ancestor walk and the intake test are unchanged.

### 6.8 Migration plan if the operator overrides to option B

This subsection is recorded in case the operator picks B in §6.9.
Recommendation remains A; the plan below is the *cost*.

**Phase 1 — write-path enforcement.**

1. Define `RequireIntakeAncestor(b, parent)` predicate. Pure
   function in `kbeads/internal/server/category.go` (alongside
   `DeriveCategory` from D7 plan), returns `true` if `b` is
   action-classified AND the parent chain does not reach
   `category=intake`.
2. Wire on every bead-creation surface:
   - `kbeads/internal/server/http_beads.go` `POST /v1/beads` —
     reject with 400 `{"error": "action_bead_requires_intake_ancestor"}`
     when the predicate fires.
   - `kd decision create`, `kd escalation create`, `kd bug create
     --flag operator-blocking` — same check pre-submit, surface
     as a CLI error with `--allow-orphan` escape hatch (off by
     default, present for operator overrides).
   - Slack bridge — when an agent emits a decision in a thread
     with no bead context, walk back to the spawning bead's intake
     ancestor before submitting; if none exists, attach to the
     fallback intake (see Phase 2).
   - Jira / GitLab bridges — at-creation parent resolution; a
     bridge-created action without an intake gets the fallback
     intake as parent.
3. Stamp a `policy_version` field on `beads` so the enforcement
   point is dated; later rollbacks stay surgical.

**Phase 2 — fallback intake.**

1. Create a permanent intake bead per scope: `ka-FALLBACK-MINE`
   etc., or one global `ka-FALLBACK-INTAKE` with `operated_by`
   set per chip variant via the resolver. Recommendation: one
   global, system-operated; chip variants render under the same
   fallback when no closer ancestor exists. Risk: operator-facing
   noise as the fallback accumulates beads — mitigated by
   weekly garbage-collection (close fallback children that have
   themselves closed).
2. The fallback intake **is** the option A pseudo row, persisted.
   The migration does not save the operator the cognitive cost
   of the orphan list — it just re-shapes it as a real bead.

**Phase 3 — backfill.**

1. One-time job: walk every open action bead's parent chain.
2. If chain reaches an intake → no-op.
3. If chain ends at a non-intake → write `parent-child` dep to
   the fallback intake.
4. Estimated rows touched: ~60 (current open action queue);
   query and write are both O(N). Runtime: < 1 minute. Risk:
   none.

**Phase 4 — D7 amendment.**

D7 plan (`ka-0000003787`) gains a co-amendment that documents the
Phase-1 invariant: `category=action → exists intake ancestor`.
Future read-time consumers can rely on the invariant rather than
re-deriving it.

**Total cost.** ~1 week of focused engineering plus a 1-time
backfill plus ongoing fallback-intake hygiene. Compare with
option A's cost: a synthetic row + the §6.5 query, both lifting
existing primitives, ≈ 1 day. The 7× cost ratio is the
recommendation's foundation.

### 6.9 Open questions for operator review

1. **Confirm pick A.** §6.3 recommends the pseudo-intake row.
   §6.4 shows option B is 7× more expensive and §6.8 documents
   what flipping to B looks like if the operator prefers it. Any
   reason to escalate B to v1 anyway (e.g. an external compliance
   constraint that says "no orphan decisions"), or is A the
   pick?
2. **Pseudo-row placement.** §6.6.1 puts the row last in the
   left rail. Alternative: pin it first when its count exceeds
   a threshold (e.g. `count > 10` → top of rail with warning
   tint). Should v1 ship the static-bottom placement, or the
   dynamic-top-on-overflow placement?
3. **Cap on orphan ring.** §6.6.2 reuses
   `constellationMaxReverse = 6` for the L1 ring. If the typical
   orphan set ends up larger than expected (e.g. > 50 in normal
   operation), the depth-slider escape hatch may not be enough
   and the right pane wants pagination instead. Should this
   section declare a hard cap (e.g. K_orphan = 30, paginate
   after) or punt to subtask 7's perf work?
4. **Re-parent action default.** §6.6.3 names the
   "re-parent to intake" peek action; subtask 5 owns it. Should
   this action be on by default, or feature-flagged behind
   `feat:inbox-reparent` until the typeahead UX lands?
5. **Drop the "raise-only" orphan variant?** `inbox:orphans:raise_only`
   means *"action beads I raised that are not under any intake."*
   In practice this is the same set as `inbox:orphans:mine` for
   most operators (the resolver's mine predicate already includes
   raised-by under the v1 FK, and the raise-only variant adds
   `created_by_operator_id = viewer` — see §4.3.1 chip notes).
   Worth shipping? Recommend: ship behind the same operator
   flag as the §4.9 raise-only chip and let it inherit that
   chip's enable state.
## 7. Performance / data shape

> **Subtask:** ka-0000006979
> **Inputs:** §1 (index predicate), §2 (subgraph predicate), §3 (layout
> caps), §4 (sort + chip predicates), round-7 perf findings absorbed in
> §3, existing list/get APIs in
> [`gardenio/controller/internal/beadsapi/client_query.go`](../../controller/internal/beadsapi/client_query.go)
> and the constellation hydrate path in
> [`gardenio/controller/cmd/beads-viewer/constellation.go`](../../controller/cmd/beads-viewer/constellation.go).

### 7.1 Problem statement

§§1–4 specified the index predicate, the subgraph predicate, the
layout, and the index UI. This section answers three orthogonal
performance / data-shape questions the parent epic asks (see the
subtask 7 description on `ka-0000006979`):

1. **Index query.** Does `kd list -f category=intake -f operated_by_id=:viewer`
   work against today's HTTP/list API, or do we need a new endpoint?
2. **Subgraph hydrate.** Does the existing constellation hydrate path
   (`buildConstellation` → `hydrateBeads` → reverse-deps walk capped
   by `constellationFanoutPerParent`) suffice, or do we need a
   constellation-shaped server endpoint?
3. **Per-row decorators.** Last-activity, composite urgency,
   raise-flag state — fold into the index call, or fetch as a
   separate batch?

For each, the headline is *reuse where the existing API already does
the work; specify the smallest delta where it doesn't*. The bar for
"new endpoint" is high: every new server route adds surface area for
the controller, the bridges, and the SSE event topology to keep
consistent. We pay that cost only when the existing shape would
force an N+1 or a heap scan that breaks under a stress fixture.

### 7.2 Headline answer — reuse vs. new endpoint matrix

| Question                          | Existing API                                                                                              | Verdict                                                                                          | Smallest delta if any                                                                                            |
| --------------------------------- | --------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------ | ----------------------------------------------------------------------------------------------------------------- |
| **Inbox index query**             | `GET /v1/beads?operated_by=…&type=…&labels=…&sort=…&limit=…` (`client_query.go:36`)                       | Reuse — *once* `category` is filterable. Today the HTTP query has no `category` parameter.        | Add `category` to `ListBeadsQuery` and to the HTTP query parser. Land alongside D7's `DeriveCategory` (ka-0000003787). |
| **Subgraph hydrate (per intake)** | `GetBead`, `GetReverseDependencies`, `GetMentionedBy`, `GetMentions`, `hydrateBeads` worker pool          | Reuse verbatim. The §2 caps (1 + 6 + 18 = 25 nodes at v1) fit inside the existing 600 ms budget. | Sibling `buildInboxConstellation` re-uses every helper; no new HTTP route.                                        |
| **Per-row decorators**            | `BeadDetail.Labels`, `BeadDetail.UpdatedAt`, planned `urgency_score`, planned `created_by_operator_id`     | Fold into the index call. No batch endpoint needed.                                              | Two new immortal columns (urgency_score, created_by_operator_id) wired to the same row JSON the list returns.     |
| **Inbox decorators that need a JOIN** (e.g. "this intake has 3 open decisions under it") | None of the existing endpoints carry roll-up counts. | Defer to v2. v1 renders the count from the subgraph hydrate when the row is *expanded*, not on the row itself. | None for v1. v2 may want a `GET /v1/beads/<id>/rollup` shape, but v1 does not.                                    |

**One bullet for each row, restating the verdict in plain English:**

- **Index** — fits the existing list endpoint with one added filter
  (`category`). The other filters the chip-strip needs (`operated_by`,
  `labels`, `parent`) are already on `ListBeadsQuery` (`client_query.go:18,
  47–48, 71`).
- **Subgraph** — already fits. The subgraph predicate from §2 is
  *literally* the bead-show constellation's reverse-dep walk with the
  forward arc dropped. Same caps, same hydrate worker pool, same
  600 ms budget. Sibling builder, not new endpoint.
- **Decorators** — every per-row decoration that §4 names (urgency,
  last activity, raise-flag-from-label) is on the bead row itself.
  Folding them into the index call is a single SELECT; a separate
  batch fetch would be strictly worse (one round-trip vs. zero).

The rest of this section justifies these verdicts in detail and pins
the cost numbers.

### 7.3 Index query — fits the existing list API, with one filter delta

#### 7.3.1 What the HTTP/list API already does

The shape `GET /v1/beads?operated_by=<id>&type=<t>&labels=<l>&sort=<s>&limit=<n>`
is implemented in
[`kbeads/internal/server/http_beads.go`](../../../kbeads/internal/server/http_beads.go)
and surfaced via `ListBeadsQuery` (`client_query.go:12`). Concretely:

- `OperatedBy` is a single `<provider>:<subject>` string filter. The
  HTTP parser at `http_beads.go:91` already accepts comma-separated
  values via `filter.OperatedBys = strings.Split(v, ",")` and the
  store applies `mergeNonEmpty(filter.OperatedBy, filter.OperatedBys)`
  in the SQL `IN` (`postgres/queries.go:230`). **The v0.5 predicate
  from §1.3 (`operated_by = ANY($1::text[])`) is supported today** —
  the controller passes `"oidc:matt@…,slack:U07K…,gitlab:matthewbaker"`
  as one comma-joined string and Postgres serves the resulting `IN`
  with `idx_beads_operated_by` (migration 012).
- `Labels` accepts a comma-separated set; the SQL applies an `array
  contains` predicate. This is what the `raise-only` / `flag` UX in
  §4.3 reads — `flag:operator-blocking` sits in `beads.labels`
  already.
- `Parent` restricts to a transitive subtree (`client_query.go:24`).
  Not used by the inbox *index* (which is flat) but used as a
  fallback for the subgraph hydrate when the per-bead path is too
  expensive (see §7.4).
- `Sort` accepts `priority`, `updated_at`, `created_at`. Adding
  `urgency_score` is a one-line server change once the column lands.
- `Limit` + `Offset` are present. The inbox uses `limit=50` for the
  cold-load page and a "load more" stub on overflow (chip-strip
  scrolling is out of scope for v1 per §4.4).

The single gap: **`category` is not in `ListBeadsQuery`** (and not in
the HTTP query parser at `http_beads.go`). It does not need to be —
yet — because no other surface filters by it. The inbox is the
first one that does.

#### 7.3.2 The `category` derivation tension

D7's plan (`ka-0000003787` §6) fixes `category` as **derived-on-read**:

> Derived-on-read = no rewrite-on-reparent. A re-parented epic flips
> category on the next read. Single test: re-parent epic from
> no-parent to under-another-epic, assert `intake → intermediate` on
> next GET.

If `category` is computed at read time inside `BeadDetail.toDetail()`,
filtering by it in SQL is impossible. Two ways to resolve this without
breaking D7's "no rewrite on reparent" property:

| Option | Shape | Trade-off |
| --- | --- | --- |
| **A. Generated column.** | `ALTER TABLE beads ADD COLUMN category TEXT GENERATED ALWAYS AS (derive_category_sql(...)) STORED;` plus a partial index on `(operated_by_id, category)`. The SQL function mirrors `DeriveCategory(b, parent)`. | "No rewrite on reparent" *requires* re-deriving category for every child of a re-parented epic. A `STORED` generated column does not see parent updates — Postgres recomputes only when the row's own columns change. So the generated column lies after a parent re-parent. **Disqualified.** |
| **B. Trigger-maintained materialized column.** | `ALTER TABLE beads ADD COLUMN category TEXT NOT NULL DEFAULT '';` plus an `AFTER INSERT/UPDATE/DELETE` trigger on `deps` (parent-child edges) that recomputes `category` for the affected child + its descendants. Application also writes `category` on bead create. | Preserves D7's no-app-side-rewrite contract: the trigger does it. Cost is one extra `UPDATE beads SET category=…` per parent-child edge mutation, which is a low-frequency event compared to bead-mutations on the row itself. Index-friendly: `(operated_by_id, category)` partial behaves as a normal column. **Recommended.** |
| **C. Keep derived-on-read.** | No column on `beads`. The inbox query is `category=intake AND operated_by_id=:viewer` — but with `category` derived application-side, the SQL has to over-fetch by `operated_by_id` alone, then post-filter. | Today's `intake` row count is small (∼12 of the operator's 22 open beads, ∼0–5% of the project's bead count overall). Over-fetch is fine at this size. Breaks at 10k+ beads in scope. **Acceptable v0.5; sub-optimal v1.** |

We pick **B (trigger-maintained materialized column)** as the v1
target. C is the v0.5 fallback if the trigger and the partial index
are not yet landed when the inbox surface ships. A is disqualified by
D7's correctness requirement.

The migration looks like:

```sql
-- 016_category_materialized.up.sql (proposed; subtask 7 owns).
ALTER TABLE beads ADD COLUMN category TEXT NOT NULL DEFAULT '';

-- One-time backfill: equivalent of running DeriveCategory across the table.
UPDATE beads SET category = derive_category_sql(beads.id);

-- Trigger maintains category on bead INSERT/UPDATE and on dep INSERT/DELETE
-- (the latter covers re-parenting). The trigger recomputes for the affected
-- child + walks descendants when an *epic*'s parent changes.
CREATE TRIGGER beads_category_maintain
    AFTER INSERT OR UPDATE OF type, kind, parent_id ON beads
    FOR EACH ROW EXECUTE FUNCTION beads_recompute_category();

CREATE TRIGGER deps_category_maintain
    AFTER INSERT OR DELETE ON deps
    FOR EACH ROW WHEN (NEW.type = 'parent-child' OR OLD.type = 'parent-child')
    EXECUTE FUNCTION deps_recompute_child_category();

-- Partial index that the inbox query reads from.
CREATE INDEX idx_beads_inbox_predicate
    ON beads(operated_by_id, urgency_score DESC, id)
    WHERE operated_by_id IS NOT NULL
      AND category = 'intake';
```

The exact `derive_category_sql` shape is out of scope here — it's
D7's rule table. What matters: the trigger lets the partial index
exist, and the partial index makes the default sort (urgency,
desc) a single index walk per page.

#### 7.3.3 Filter delta on the controller / client

Two thin changes on the existing list path:

```go
// gardenio/controller/internal/beadsapi/client_query.go
type ListBeadsQuery struct {
    // ... existing fields ...
    Categories []string // Filter by category ("intake", "action", "intermediate", "config")
}

// in ListBeadsFiltered:
if len(q.Categories) > 0 {
    params.Set("category", strings.Join(q.Categories, ","))
}
```

```go
// kbeads/internal/server/http_beads.go (parser):
if v := req.URL.Query().Get("category"); v != "" {
    filter.Categories = strings.Split(v, ",")
}
```

```go
// kbeads/internal/model/filter.go:
type Filter struct {
    // ... existing ...
    Categories []string `json:"categories,omitempty"`
}
```

```go
// kbeads/internal/store/postgres/queries.go (where-clause):
if cats := mergeNonEmpty("", filter.Categories); len(cats) > 0 {
    whereClauses = append(whereClauses, "category = ANY("+nextArg()+")")
    args = append(args, pq.Array(cats))
}
```

This is a *standard* filter wiring — same shape as `Types`, `Kinds`,
`Statuses`, `Labels`. No new endpoint.

#### 7.3.4 The query plan, end to end

For the cold-load default chip (`mine`, sort=urgency desc, limit=50):

```
SELECT id, type, kind, status, priority, title, labels, updated_at,
       urgency_score, operated_by_id, ...
FROM   beads
WHERE  category = 'intake'
  AND  operated_by_id = $1            -- :viewer_operator_id
ORDER BY urgency_score DESC, id
LIMIT 50;
```

Plan: index scan on `idx_beads_inbox_predicate (operated_by_id,
urgency_score DESC, id) WHERE operated_by_id IS NOT NULL AND
category = 'intake'`. No heap sort. Page size of 50 means one index
read + one heap fetch per row. **O(log N + 50)** for the index
walk; effectively O(1) at our data size.

For the chip variants:

- `team`: same index — `operated_by_id = ANY(:team_ids)` (5–20
  members). Bitmap-OR over the same index entries. Still no heap
  sort.
- `on-call`: same index — single equality.
- `raise-only`: AND-clause `created_by_operator_id = :viewer`. The
  partial index above does not cover this column; either (a) add a
  second partial `(operated_by_id, category, created_by_operator_id)`,
  or (b) accept a heap filter on the rows the primary index already
  narrowed to (≤ N intake-by-viewer; small N, cheap filter). v1
  picks (b).

### 7.4 Subgraph hydrate — reuses existing per-bead path

The §2 predicate is exactly:

> Anchor at the clicked intake. Walk reverse-deps + folded mentions
> outward to depth `N=2` (slider up to `constellationMaxTreeLevel=3`),
> capped at `constellationMaxReverse=6` children at L1 and
> `constellationFanoutPerParent=3` per L1 parent at L2. Worst-case 25
> nodes at the v1 default; 79 nodes at the slider ceiling.

This is structurally identical to the bead-show constellation walk
(`buildConstellation` → `expandOuterLevels` →
`hydrateBeads`). The only delta is a `forwardOnly = false,
reverseOnly = true` flag and the cycle behaviour change pinned in
§2.6. **No new HTTP endpoint, no new daemon work — a sibling builder
function in `constellation.go`** is sufficient.

#### 7.4.1 Hydrate budget

| Tree depth | Worst-case nodes (per §2) | `hydrateBeads` calls | Wall-clock at 6 workers, 600 ms timeout |
| ---------- | -------------------------- | -------------------- | --------------------------------------- |
| N=1        | 1 + 6 = 7                  | 7                    | ≤ 100 ms                                |
| N=2 (v1)   | 1 + 6 + 18 = 25            | 25                   | ≤ 250 ms (well under the 600 ms cap)    |
| N=3 (slider) | 1 + 6 + 18 + 54 = 79      | 79                   | ≤ 600 ms (at the existing ceiling)      |

The bead-show constellation today operates at the same envelope —
it can hit `1 + 6 + 6 + 36 = 49 forward + reverse + L2` at the
slider's top. The inbox view is *cheaper* at the same N because
forward is dropped (§2.2).

The existing `hydrateBeads` (`constellation.go:921`) handles
fan-out concurrency and the 600 ms timeout. No code change beyond
the §2.6 cycle delta and the `forwardOnly` flag.

#### 7.4.2 The `Parent` fallback shape — and why we don't pick it

`ListBeadsQuery.Parent` (`client_query.go:24`) fetches the entire
transitive subtree under a bead. For the inbox subgraph this
produces the right *node set* (`Parent=:intake_id`) in one SQL hit:

```sql
WITH RECURSIVE subtree AS (
  SELECT bead_id FROM deps WHERE depends_on_id = :intake AND type = 'parent-child'
  UNION
  SELECT d.bead_id FROM deps d
  INNER JOIN subtree s ON d.depends_on_id = s.bead_id AND d.type = 'parent-child'
)
SELECT b.* FROM beads b WHERE b.id IN (SELECT bead_id FROM subtree) OR b.id = :intake
LIMIT 100;
```

So why not collapse 25 hydrate calls into one recursive CTE?

- The constellation **also folds in mentions** (typed + untyped) via
  `mergeTypedMentionsAsDepEdges` and `appendUntypedMentionEdges`.
  These are not `deps` rows, so a `Parent`-only subtree query would
  miss them. The existing per-bead path picks them up via
  `GetMentionedBy` / `GetMentions`. Switching to the CTE means
  re-implementing mention-folding on the server, doubling the
  surface.
- The constellation walks **per-parent caps** at depth (≤ 3 children
  per L1 parent at L2) — this is a layout decision, not a SQL one.
  Doing it server-side requires the client to specify the cap shape
  on the request, which is a layering inversion (server should not
  know the SVG cap constants).
- The 25 hydrate calls hit the daemon **in parallel via the worker
  pool**. The wall-clock is dominated by the slowest of 25, not the
  sum. At 6 workers it's ≤ 250 ms even on cold cache.

The `Parent` recursive shape stays available as a *fallback for
overflow*: when the operator slider raises N=3 and the worker-pool
walk would exceed 600 ms, the server can switch to a single CTE
fetch of the subtree-by-id, with mentions hydrated in a parallel
batch. v1 does not need this; the slider ceiling at N=3 is 79 nodes
at 6 workers = 600 ms, which is the existing budget.

#### 7.4.3 Decision: no new endpoint

The inbox subgraph hydrate is served by:

| Existing call                                           | Use                                                                         |
| ------------------------------------------------------- | --------------------------------------------------------------------------- |
| `GET /v1/beads/<id>` (`GetBead`)                        | Anchor + each L1+/L2+ node (per `hydrateBeads`'s worker pool)              |
| `GET /v1/beads/<id>/dependents` (`GetReverseDependencies`) | The L1 children walk; called per node in the BFS                          |
| `GET /v1/beads/<id>/mentioned-by` (`GetMentionedBy`)    | Untyped + typed mentions folded into the constellation                      |
| `GET /v1/beads/<id>/mentions` (`GetMentions`)           | Forward mentions (kept in the hover preview but dropped from the SVG, §2.2)|

No daemon change. The inbox surface is a sibling builder over the
same primitives.

### 7.5 Per-row decorators — fold into the list call

§4 named four decorations the inbox row needs at scan time:

1. **Title + type glyph + status fill.** Already on `BeadDetail`
   (`client.go` BeadDetail struct).
2. **Last activity (`updated_at`).** Already on `BeadDetail`.
3. **Composite urgency.** Proposed `beads.urgency_score DOUBLE
   PRECISION` column (§4.4.3). Once landed, present on the same
   `BeadDetail`. v0.5 computes it application-side from
   `priority + updated_at + open-children-count` on the page-sized
   result.
4. **Raise-flag pip (`flag:operator-blocking`).** A label. Already
   surfaced on `BeadDetail.Labels`.

None of these require a separate batch — they are all *columns on the
row the index already returns*. The index call is already a single
HTTP round-trip; folding decorators into it costs zero extra hops.

#### 7.5.1 The two columns the inbox needs landed

To make §4's chip strip and default sort cheap and correct, two new
columns on `beads` are prerequisites:

| Column                        | Purpose                                                                     | Owner                                |
| ----------------------------- | --------------------------------------------------------------------------- | ------------------------------------ |
| `urgency_score DOUBLE PRECISION` | default sort; `(operated_by_id, urgency_score DESC) WHERE category='intake'` partial index | subtask 7 follow-up bead, this section files it |
| `created_by_operator_id BIGINT` | `raise-only` chip predicate (§4.3.1); without it the chip is disabled       | filed alongside §4 cross-references; reasserted here |

Neither column is on Option B's migration 015 (per §4.3.1 / §4.7).
They are this section's *implementation prerequisites* and are filed
as follow-up beads in §7.9.

If neither column is landed when the inbox surface ships, the
degraded shapes are:

- **No `urgency_score`:** sort label reads "urgency (slow path)";
  the inbox over-fetches `limit × 3` rows by `priority + updated_at`,
  computes urgency in the controller, returns the top `limit`. Cost
  is a 3× over-fetch on the index page; still O(1) at our data size.
- **No `created_by_operator_id`:** `raise-only` chip is rendered
  disabled with a tooltip pointing at the prerequisite bead. Same
  pattern §4.3.3 uses for `team` / `on-call`.

#### 7.5.2 Decoration that requires a roll-up — defer to v2

A row-level "this intake has 3 open decisions and 1 escalation under
it" badge would require a per-row JOIN against the dep-graph. v1
does not render that — the hover-preview / subgraph render shows the
counts on intake selection, not on the row itself. v2 might want a
`GET /v1/beads/<id>/rollup` endpoint with cached counts, but v1
explicitly does not need it (§4.6 keyboard model and §4.5 row chrome
both work without it).

### 7.6 Estimated query cost — current 22-bead queue + a stress fixture

#### 7.6.1 Today's operator queue (the 22-bead fixture)

Per §2.3's sanity check: the current operator queue is ∼22 open
beads spanning ∼12 distinct intakes. Approximate distribution:

| Metric                                     | Value          |
| ------------------------------------------ | -------------- |
| Total intake beads `category='intake'`     | ∼12             |
| Inbox query hits (`category='intake' AND operated_by_id=:viewer`) | ∼6–10 |
| Avg children per intake                    | < 2            |
| Worst-case intake (release prep)           | ∼6 direct, ∼2–3 grandchildren each |
| Subgraph hydrate calls @ N=2 (worst case)  | ≤ 25            |
| Subgraph hydrate calls @ N=2 (typical)     | ≤ 5             |

**Index query cost.** Even without the partial index, today's
22-row table is served by `idx_beads_operated_by` (the partial from
migration 012) + a heap filter for `category='intake'`. Plan: index
scan returns 6–10 rows; heap-fetch each; client-side post-filter
because `category` is derived application-side today. **< 5 ms** in
the daemon, dwarfed by HTTP round-trip latency.

**Subgraph hydrate.** Worst-case 25 hydrates over 6 workers ≤ 250 ms.
Typical (5 hydrates) ≤ 50 ms.

#### 7.6.2 Stress fixture — projected steady-state load

To model what 6–24 months of growth looks like, define a stress
fixture:

| Parameter                                  | Stress fixture value |
| ------------------------------------------ | -------------------- |
| `beads` row count                          | 50,000               |
| `intake` rows                              | 5,000 (10 % of beads) |
| Distinct operators                         | 20                   |
| Intake rows operated by the median viewer  | ∼250 (5,000 ÷ 20)    |
| Intake rows operated by the worst viewer   | ∼1,500 (the on-call) |
| Avg children per intake                    | 4                    |
| 95th-percentile intake child count         | 12                   |

**Index query cost — without the partial index:**

- `WHERE operated_by_id = :viewer` returns ∼1,500 rows in the worst
  case (on-call viewer). Heap-fetch each, post-filter `category` in
  the controller, sort by urgency in the controller. **∼15 ms** for
  the SQL hit, plus an O(N) sort step in Go. Still acceptable but
  starting to amortize sort cost against page size.

**Index query cost — with the `idx_beads_inbox_predicate` partial:**

- Index walk over `(operated_by_id, urgency_score DESC) WHERE
  operated_by_id IS NOT NULL AND category = 'intake'` returns the
  top 50 rows directly. **< 2 ms** regardless of viewer. This is
  the v1 target.

**Subgraph hydrate — at the slider ceiling (N=3):**

- Largest intake fixture: 12 direct children, average 4 grandchildren
  each, depth-3 = 12 × 4 × 4 = 192 candidates → capped at
  `constellationMaxReverse(6) + 6×3(L2) + 18×3(L3) = 78` nodes by the
  existing per-parent fanout cap.
- 78 hydrates × 6 workers ≈ 13 batches × 30 ms median per call =
  ∼400 ms. Within the existing 600 ms budget but tight; if the
  fixture grows further the `Parent`-CTE fallback (§7.4.2) kicks in
  for the slider-N=3 case only.

**Conclusion:** v1 scales cleanly to a 50k-row, 20-operator fixture
*provided* the `idx_beads_inbox_predicate` partial index is landed.
Without it the worst-viewer case slides into the controller-side
sort path, which is acceptable but not the v1 target.

#### 7.6.3 What the existing perf envelope tolerates (carried over from round 7)

§3 absorbed round-7's perf finding: the bead-show constellation's
22-bead-fixture worst case is ≤ 30 hydrates over a 600 ms budget.
The inbox view's hydrate budget is *strictly smaller* at the same
fixture (forward arc dropped) and *equal* at the stress-fixture's
slider-N=3 worst case (§7.6.2). No new endpoint is required to keep
the inbox under that envelope.

### 7.7 Caching strategy

Three caches are live in this surface; each has a different scope and
TTL.

#### 7.7.1 Per-viewer index cache

**Scope:** the result of the cold-load index query — i.e. "the 50
intake rows the viewer-resolver returns for this OIDC subject, this
chip, this sort."

**Shape:** `LRU(64, ttl=5s)` keyed by
`(viewer_operator_id, scope_chip, sort_key, dir, page)`. 5 s TTL is
short enough that "I just resolved a decision" surfaces on the next
scan; long enough that a back-button keystroke chord doesn't
re-issue a SQL round-trip. The key includes `page` so paginated
"load more" doesn't poison the first page's cache slot.

**Invalidation:** on any SSE event with a payload that touches a
bead the viewer operates (`operated_by_id == viewer_operator_id`).
The SSE subscriber already exists (`internal/subscriber/`); the
inbox handler taps the same channel and clears its cache slot when
a relevant event fires. Pseudocode:

```go
func (s *Server) onBeadEvent(ev BeadEvent) {
    if ev.OperatedByID == s.viewerID(ev) || ev.Category == "intake" {
        s.indexCache.InvalidateOperator(ev.OperatedByID)
    }
}
```

**Why not longer TTL?** The index is the operator's primary scan
surface. Stale rows would mean "I clicked an intake, then five
seconds later it's still showing as un-triaged after I closed it."
5 s is the existing `/active` TTL; adopting it keeps the two
surfaces' freshness identical.

**Why a cache at all?** Without one, every keypress in the
left-rail (j/k focus moves don't re-fetch, but every cmdk-driven
filter chip swap does) would issue a SQL round-trip. With the
partial index landed each round-trip is < 2 ms — the cache is a
politeness, not a correctness, knob. Pushing the cache TTL to 0 is
acceptable; we set it at 5 s for parity with `/active` and to
absorb double-render bugs in the chip strip.

#### 7.7.2 Per-intake subgraph cache

**Scope:** the rendered subgraph view-model for a clicked intake.

**Shape:** `LRU(256, ttl=15s)` keyed by `(intake_id, slider_N,
viewer_operator_id)`. The viewer key is in there because §2.6's
multi-parent edge selection and §2.5's overflow-sort are deterministic
*within* a viewer but pick a different prune order when the viewer's
identity influences "raise-only" filtering on the children. (For v1
the children are not filtered by viewer; the key triple keeps a
hook for v2 raise-only-overlay without re-keying.)

**TTL:** 15 s. The subgraph changes less often than the index (an
intake's children don't ping-pong) so the longer TTL is safe.

**Invalidation:** SSE events for any descendant of the intake.
Implemented as a "subgraph contains" check against the cached node
set. If the SSE event's bead id is in the cached subgraph, drop
the entry.

**Why not server-side cache?** The viewer-keyed shape and the
SVG-render-time decisions (cycle behaviour, age-opacity ordering)
are controller-state concerns, not daemon-state. The daemon's
`hydrateBeads` worker pool is already cheap; caching a second time
in the daemon would duplicate without simplifying.

#### 7.7.3 Resolver cache (carried over from §1.4)

§1.4 already pinned the identity-resolver cache shape:

- v1: `(oidc_subject) → operator_id`, TTL = lifetime of
  `operator_aliases` row, invalidated by SSE on
  `operator_aliases` mutations.
- v0.5: `(oidc_subject) → []string`, TTL = lifetime of the
  `user_identity` bead, invalidated on bead-update SSE.

The inbox surface inherits this verbatim. No new cache scope is
introduced here.

#### 7.7.4 Cache layering — an inbox request, end to end

A cold-load `GET /inbox?scope=mine&sort=urgency` walks:

```
HTTP request
  -> resolver cache (1.4): viewer_operator_id  -- hit ≈ 0.05 ms; miss ≈ 1 ms
  -> index cache (7.7.1):   page-of-50 rows    -- hit ≈ 0.05 ms; miss ≈ 2 ms (partial-index walk)
  -> render row partial in template            -- ≈ 5 ms for 50 rows
  -> response
```

A click on an intake row walks:

```
HTTP request
  -> resolver cache (1.4): viewer_operator_id  -- hit ≈ 0.05 ms
  -> subgraph cache (7.7.2): view-model        -- hit ≈ 0.05 ms; miss ≈ 250 ms (25 hydrates)
  -> render constellation SVG                  -- ≈ 10 ms
  -> response
```

Cold-cache, worst-case, the entire inbox boot is **≤ 270 ms**. Hot
cache it's **≤ 15 ms**. Both are below the 300 ms perceptual budget
operators experience as "instant."

### 7.8 Open question for subtask 8 — does promotion to `/` need an API change?

Subtask 8 (`ka-0000001339`) decides whether and when the inbox
constellation becomes the default at `/`. This section pins the
*data-shape* obligations that promotion would impose:

| Promotion target            | API delta                                                                                          | Severity |
| --------------------------- | -------------------------------------------------------------------------------------------------- | -------- |
| Default at `/inbox` only    | None — every query / hydrate / cache shape pinned above is already in scope.                       | None     |
| Default at `/` as cold-load | Anonymous-mode handling: when the resolver returns no `viewer_operator_id`, render the missing-identity banner (§4.6) — no SQL change. | None     |
| Default at `/` for SSO-less dev environments | None — same as above; the empty-inbox state is the right default.                       | None     |
| Default at `/` *and* the slider raise to N=3 becomes a default | The 600 ms hydrate budget at the stress fixture's worst intake (§7.6.2: 78 nodes, ∼400 ms) is borderline. Promotion to default-N=3 would require either the `Parent`-CTE fallback (§7.4.2) or a higher worker-pool count. | Medium — affects daemon load, not API surface |
| Default at `/` *and* a project-wide lane view (the round-7 absorbed shape) replaces the per-intake constellation | This is a *different* surface — round-7 conclusion already noted that a project-wide lane needs a constellation-specific endpoint to avoid N+1 hydrates. The inbox surface as defined here does not implement that shape and its absence is the reason today's inbox fits the existing API. | High — would require new server endpoint; explicitly **out of scope** for this epic |

**Headline answer for subtask 8:** promotion of the inbox-constellation
view (as defined by §§1–7) to `/` requires *zero API changes*. The
two scenarios that would require API changes (default-N=3, project-
wide lane view) are out of scope for this epic and are noted here so
subtask 8 can rule them in or out as part of the promotion bar.

### 7.9 New work this section blocks (cross-references filed)

Subtask 7 surfaces three implementation prerequisites that need their
own beads under the parent epic:

1. **`beads.category` materialized column + trigger + partial index.**
   Migration 016 + trigger + `Filter.Categories` wiring (§7.3.2,
   §7.3.3). Owner: this section. Filed as a subtask under the
   parent epic when implementation is green-lit.
2. **`beads.urgency_score` column + write-hook.** §4.4.3 named the
   column; this section pins the partial index that consumes it
   (`(operated_by_id, urgency_score DESC) WHERE category='intake'`).
   Filed as subtask 4's follow-up; this section reasserts it.
3. **`beads.created_by_operator_id` column.** §4.3.1 names this as
   the `raise-only` chip prerequisite. Without it the chip ships
   disabled. Filed as subtask 4's follow-up; this section reasserts
   the data-shape rationale (immortal author column, partial index
   not required for v1 since the chip filter runs over the already-
   narrowed `mine` result).

These three beads are the **smallest set of server-side changes**
required to ship the inbox surface as designed. None of them is a
new HTTP endpoint; all three are column adds + index work on the
existing `beads` table.

### 7.10 Out of scope

- **A `/v1/inbox/index` or `/v1/inbox/<id>/subgraph` endpoint.** The
  existing list/get APIs serve every query this section names. New
  endpoints would be premature optimisation and add bridge-side
  surface (NATS topic shape, SSE replay rules) that is not justified
  by the data sizes.
- **A roll-up endpoint (`/v1/beads/<id>/rollup`).** Deferred to v2
  per §7.5.2.
- **Project-wide lane view as the default surface at `/`.** Explicit
  carry-over from round 7's perf finding: that shape *would* need a
  constellation-specific endpoint. It is not what this epic ships.
- **Read-replica or per-operator sharding** of the `beads` table.
  Today's data sizes don't motivate either; the partial indexes from
  §7.3 keep the primary read path under 2 ms.

### 7.11 Open questions for operator review

1. **Pick A vs. B vs. C for `category` derivation.** §7.3.2
   recommends B (trigger-maintained materialized column). A is
   disqualified by D7's no-rewrite-on-reparent rule; C is acceptable
   v0.5 but does not scale to the stress fixture. Operator
   confirmation requested before subtask 7 files the migration.
2. **`idx_beads_inbox_predicate` sequencing.** The partial index on
   `(operated_by_id, urgency_score DESC, id) WHERE category='intake'`
   depends on (1) above + the urgency_score column landing. Land
   them as one migration, or sequence the column add → trigger →
   partial index across separate migrations? Recommend one combined
   migration; the partial index is a no-op until the column exists.
3. **Subgraph cache TTL.** §7.7.2 sets it at 15 s. If operators
   typically scan an intake then click into a child for triage and
   come back, 15 s might be too short (they'll re-hydrate on
   return). Recommend 15 s as v1; raise to 30 s if telemetry shows
   high re-hit rate.

---

## 8. Relationship to existing constellation + promotion

> **Subtask:** ka-0000001339
> **Inputs:** §§1–7 (the inbox-constellation surface as designed);
> existing routes in
> [`gardenio/controller/cmd/beads-viewer/server.go`](../../controller/cmd/beads-viewer/server.go)
> (`handleIndex` at `server.go:831`, `handleActive` at `active.go:61`,
> `handleDecisions` registered at `server.go:437`, `handleBeadShow` at
> `server.go:529` for the `/{id}` bead-show page that hosts the
> existing constellation); superseded design epic
> [`ka-0000004660`](#) (operator-inbox list page) and absorbed
> research epic [`ka-0000004309`](#) (constellation-as-primary).

### 8.1 Problem statement

§§1–7 specify a complete inbox-primary constellation surface: an
index of intake beads operated by the viewer, drilling into a per-
intake constellation, with chips, sort, peek/action surface, an
orphan affordance, and a bounded perf budget. What §§1–7 deliberately
*do not* answer is where this surface lives in the URL space and what
must be true before it becomes the cold-load default at `/`.

This section pins three calls:

1. **Default route name** for the inbox-constellation surface.
   Recommend `/inbox` (with `/operator-inbox` as a permanent alias).
2. **Whether and when** the new surface replaces the existing
   project-list landing page at `/`. Recommend *not at v1*; promotion
   is gated on a concrete proof-in-use bar this section defines.
3. **What `/active`, `/decisions`, and the `/{id}` bead-show
   constellation do under the new world.** Recommend: `/active`
   stays (different audience), `/decisions` is folded as a typed
   filter on the inbox once the inbox is the default, the existing
   bead-show constellation at `/{id}` stays unchanged forever (it is
   a per-bead context view, not a per-operator triage view).

The shape of these calls is constrained by the parent epic's framing
(`ka-0000006157` — "the existing bead-show constellation in
`gardenio/controller/cmd/beads-viewer/constellation.go` is **not
modified** — this is a sibling view that reuses primitives") and by
the parent epic's open question on route stability ("Keep
`/operator-inbox` to preserve URL stability with ka-0000004660's
framing"). §§4 and 6 of this doc were drafted using `/inbox` as a
working stand-in; this section formalizes the choice and reconciles
it with the parent's prior preference.

### 8.2 The existing surfaces — what is already at the relevant URLs

Before specifying promotion, we name what is at each URL today and
what role it plays. All routes register against `appMux` in
[`server.go:432–530`](../../controller/cmd/beads-viewer/server.go).

| Route                | Handler                              | Audience / role                                                                                                           | Status under v1 |
| -------------------- | ------------------------------------ | ------------------------------------------------------------------------------------------------------------------------- | --------------- |
| `/`                  | `handleIndex` (`server.go:831`)      | Landing page; renders the list of known projects + `computeIndexStats`. Anonymous-friendly. The default cold-load surface. | **Unchanged at v1.** Promotion to inbox is gated per §8.5. |
| `/{id}`              | `handleBeadShow` (`server.go:529`)   | Per-bead deep view; hosts the existing constellation render via `buildConstellation` for the clicked bead.                | **Unchanged forever.** Different surface, different audience, kept verbatim per parent epic constraint. |
| `/active`            | `handleActive` (`active.go:61`)      | Project-grouped list of all `in_progress` work; chip strip filters by operator. Cold-load surface for "what's running."   | **Stays.** Different lens (project-by-project, all in-progress) than the inbox (intake-by-operator). |
| `/decisions`         | `handleDecisions` (`server.go:437`)  | Flat list of open decisions ranked by age. The legacy "what's blocking a human" surface.                                  | **Folds into inbox once inbox is default.** Pre-promotion: stays. Post-promotion: redirect to `/inbox?type=decision`. |
| `/agents`, `/advice`, `/views`, `/health`, `/costs`, `/ratings`, `/schedules`, `/settings`, `/p/{project}` | various                              | Typed surfaces with their own audience; not in the inbox's path.                                                          | **Unchanged.** |
| `/inbox` (new)       | proposed `handleInbox` + `handleInboxBead` | The surface §§1–7 designs.                                                                                                | **New at v1.** Sibling additive route under the inbox URL prefix. |
| `/operator-inbox` (new) | redirect to `/inbox`             | Stable alias for ka-0000004660's framing. Permanent.                                                                       | **New at v1.** 308 redirect, never removed. |

### 8.3 Default route — pick `/inbox`

#### 8.3.1 The pick

**Pick `/inbox`** as the canonical URL for the new surface, with
`/operator-inbox` registered as a permanent 308 redirect to `/inbox`.

#### 8.3.2 Why `/inbox` and not `/operator-inbox`

The parent epic's prior preference (`ka-0000006157` open questions
section) was to keep `/operator-inbox` "to preserve URL stability with
ka-0000004660's framing." Three things have changed since that note
was written:

- **ka-0000004660 is superseded.** The parent epic's own
  description names it as superseded ("Supersedes: ka-0000004660 …
  ka-0000004309. Their findings are inputs."). Nothing currently
  links to `/operator-inbox`; there is no live URL stability to
  preserve. The "stability" concern was forward-looking, not
  backward-looking.
- **Sections 4 and 6 of this doc both shipped using `/inbox`.**
  §4.3.1's chip strip uses `/inbox`, `/inbox?scope=team`,
  `/inbox?scope=on_call`, `/inbox?scope=raise_only`. §6.6.4 uses
  `/inbox/orphans/<scope>`. Renaming to `/operator-inbox` at this
  stage means a search-and-replace across two long sections plus the
  template files that subtask 4 and 6 will produce. The cost is
  small but real.
- **`/inbox` is shorter and the index is keyboard-driven.** §4.5's
  keyboard model rebinds `g i` to "go to inbox." Operators will type
  the URL when deep-linking and reach for the shortest stable form.
  `/operator-inbox` reads as a typed-surface name (analogous to
  `/operator-decisions` if such a thing existed); `/inbox` reads as
  the noun the operator already says out loud ("check the inbox").

The trade-off is small: `/inbox` is mildly more generic than
`/operator-inbox` and could collide with a hypothetical future
"system inbox" or "agent inbox" surface. We mitigate by:

- Reserving `/operator-inbox` as a permanent 308 alias. Future
  surfaces would land at `/agent-inbox`, `/system-inbox`, etc.
- Pinning the URL meaning in
  [`server.go`](../../controller/cmd/beads-viewer/server.go)'s route
  registration comment: *"`/inbox` = operator-inbox (intake-rooted
  constellation index for the signed-in viewer). For agent or system
  surfaces use `/agent-inbox`, `/system-inbox`."*

#### 8.3.3 URL surface for v1

```
GET  /inbox                              # cold-load index, default chip = mine
GET  /inbox?scope={mine|team|on_call|raise_only}   # chip variants (§4.3.1)
GET  /inbox?sort={urgency|priority|updated_at}&dir={asc|desc}  # sort (§4.4)
GET  /inbox/{id}                         # left rail = index, right pane = intake {id}'s constellation
GET  /inbox/orphans/{scope}              # orphan pseudo-row drill (§6.6.4)
GET  /api/v1/inbox/{id}/subgraph         # JSON for HTMX subgraph swap (subtask 7 owns shape)

GET  /operator-inbox                     # 308 → /inbox (permanent alias)
GET  /operator-inbox/...                 # 308 → /inbox/... (path preserved)
```

The redirect handler is a one-liner registered alongside the inbox
routes in `server.go`:

```go
mux.HandleFunc("GET /operator-inbox", redirectPerm("/inbox"))
mux.HandleFunc("GET /operator-inbox/{rest...}", redirectPerm("/inbox"))
```

### 8.4 Stays-default question — `/` is unchanged at v1

#### 8.4.1 The pick

**`/` continues to render `handleIndex` (the project list) at v1.**
The inbox-constellation surface lives at `/inbox` and is reached
either by typing the URL, by `g i` from anywhere in the viewer, or
by a top-of-page link added to `index.html` (and to `active.html`'s
chrome) that reads *"Inbox · 24 ↗"* with a count badge equal to the
viewer's `mine`-chip result.

Promotion of the inbox-constellation surface to `/` is **out of
scope for v1**. The bar for that promotion is pinned in §8.5; it
exists so the operator can decide *after dogfooding* whether to flip
the cold-load default, rather than committing to the flip up front.

#### 8.4.2 Why not promote `/` immediately

Three reasons, in declining order of weight:

- **Anonymous and dev-bypass viewers see a useful page today.**
  `handleIndex` works without an OIDC session — the project list and
  `computeIndexStats` block render for anyone who can reach the URL,
  including dev-bypass mode (`devBypassMiddleware`). The inbox-
  constellation surface is *intrinsically* identity-scoped: §1's
  resolver returns a viewer operator id; without one the surface
  renders an empty state plus the missing-identity banner (§4.6,
  §1.5). Replacing the project list with an empty inbox makes the
  page *worse* for the populations that hit it without a resolved
  identity (anyone landing for the first time, anyone without an
  `operators` row yet, anyone in dev-bypass).
- **The inbox is a triage surface, not a discovery surface.** `/`
  today is reached on cold load and from "I just want to see what's
  going on across projects" navigation. The inbox answers a *narrower*
  question ("what is on me right now"). Both questions are valid;
  collapsing them into one default loses the discovery shape.
- **Proof-in-use is not yet collected.** §8.5's bar is empirical —
  the inbox has to demonstrate that it *can* serve as the cold-load
  default before it is one. v1 ships behind `/inbox` and `g i` so
  that proof-collection can happen against real triage sessions
  without forcing every viewer (including the ones who don't yet
  have an `operators` row wired) to run the gauntlet.

#### 8.4.3 The `/` discoverability hook

Even with promotion deferred, the inbox needs to be findable from
`/`. The `index.html` template gains a single chrome change:

```
┌──────────────────────────────────────────────────────────────┐
│ beads-viewer                          [Inbox · 24 ↗]  [Active]│
│ ──────────────────────────────────────────────────────────── │
│ Projects: gardenio · cicd · …                                 │
│ Stats: …                                                     │
└──────────────────────────────────────────────────────────────┘
```

The `[Inbox · N ↗]` badge:

- Renders only when the viewer has a resolved operator id (per §1.4).
- The `N` is the same `mine`-chip count §6.7.3 computes for the
  orphan badge, fetched as a single inexpensive call to the index
  endpoint with `limit=0` (count-only flag added to `ListBeadsQuery`
  per §7.3.3).
- A click navigates to `/inbox` with the chip strip on `mine` and
  the cold-load index rendered.
- Same chrome change applied to `active.html` (the active page
  already has its own chip strip; the inbox link goes top-right
  alongside the operator chip).

This keeps `/` and `/active` as the discovery surfaces and `/inbox`
as the triage surface, with cheap navigation between.

### 8.5 The promotion bar — what must be true before `/` flips to inbox

Promotion of the inbox-constellation to `/` is a *one-line config
change* in
[`server.go:434`](../../controller/cmd/beads-viewer/server.go) — the
`/{$}` route handler swaps from `handleIndex` to a wrapper that
either renders the inbox (when the resolver returns a viewer id) or
falls back to `handleIndex` (when it does not). The technical change
is trivial. The operational decision to make it is what this section
gates.

The bar has four legs: design completeness, proof-in-use, perf, and
operator sign-off.

#### 8.5.1 Design completeness

All eight subtasks under `ka-0000006157` must be merged into this
doc, all `## Open questions for operator review` blocks resolved,
and the implementation epic that follows must have shipped to
production. Concretely:

- §§1–8 of this doc green-lit by the operator (a single decision
  bead with a `report` artifact_type that names the merged version of
  this doc by commit SHA).
- All cross-referenced server-side prerequisites landed (`category`
  materialized column + trigger + partial index per §7.3.2;
  `urgency_score` column per §4.4.3; `created_by_operator_id`
  column per §4.3.1; Option B P1+P2 of the human-attribution ADR
  for the v1 predicate per §1.2; `operators.team` for the `team`
  chip per §4.3.1 (or the chip stays disabled)).
- The `/inbox` surface ships at v1 with §§1–7 implemented.

If any of these is missing, promotion is blocked on the missing
piece, not on the size of the proof-in-use window.

#### 8.5.2 Proof-in-use — dogfooding window

The empirical bar. Before promotion, the inbox must demonstrate that
it can carry the cold-load default's role *for the operators who
will use it*. The bar is concrete and has measurable thresholds.

| Metric                                                    | Target (must hold for the K-day window)                                              | How measured                                                                                  |
| --------------------------------------------------------- | ------------------------------------------------------------------------------------ | --------------------------------------------------------------------------------------------- |
| **Window length (K)**                                     | **14 days**, contiguous, at least one full on-call rotation inside                   | Calendar; on-call rotation lookup                                                             |
| **Distinct triage sessions completed**                    | **≥ 30**, across **≥ 2 distinct operators**                                          | Session = first `/inbox` GET → ≥ 5 minutes of bounded activity → page close or 30-min idle. Bridge-side log query. |
| **Bounce rate to other views during a session**           | **≤ 25 %** of sessions navigate to `/active`, `/decisions`, or `/p/{project}` mid-session | Referer header + page-event log; `/{id}` bead-show drills do *not* count as a bounce (they're part of the inbox flow) |
| **Triage actions per session**                            | **≥ 3 median**, **≥ 8 p90**                                                          | §5's action surface emits action events; aggregate by session id                              |
| **Operator-reported "I would default to this" sentiment** | **≥ 2 of 2 active operators agree**                                                  | Decision bead with `tldr` artifact, one per operator, after the K-day window                  |
| **Zero unresolved blocker bugs filed against `/inbox`**   | **0 open `flag:operator-blocking` bugs** with `project:gardenio` and component label `inbox` | `kd list -f labels=flag:operator-blocking,inbox`                                              |

Bounce rate is the load-bearing metric. The inbox is a default-
candidate only if operators *don't* repeatedly bounce to legacy
surfaces during a triage session — that bounce pattern would mean
the inbox is missing affordances the operator's hands have learned
to reach for elsewhere. The 25 % threshold is a strawman; the parent
epic open questions block (§8.10) names it for operator confirmation.

The 14-day window is a strawman too. The minimum is "at least one
on-call rotation" because the on-call chip (§4.3.1) is the surface's
busiest-day stress test; the maximum is bounded by *not* delaying
promotion past the point where dogfooding has converged.

#### 8.5.3 Perf budgets that must hold under load

§7.7.4 pinned the perf envelope for the inbox surface in isolation:
cold-cache cold-load ≤ 270 ms, hot-cache ≤ 15 ms, subgraph hydrate
worst-case ≤ 600 ms at the slider ceiling. The promotion bar lifts
those numbers from "must hold on a developer's laptop" to "must hold
under cold-load production load":

| Budget                                       | v1 (`/inbox` opt-in) | Promotion to `/` (cold-load default)                                                         |
| -------------------------------------------- | -------------------- | -------------------------------------------------------------------------------------------- |
| p50 inbox-index render (TTFB)                | ≤ 50 ms              | **≤ 50 ms** (no regression)                                                                  |
| p95 inbox-index render                       | ≤ 270 ms             | **≤ 200 ms** — the cold-load default is hit *first* every page-load, not after an explicit nav |
| p99 inbox-index render                       | ≤ 600 ms             | **≤ 400 ms**                                                                                 |
| p95 subgraph hydrate (per-intake drill)      | ≤ 600 ms             | **≤ 600 ms** (unchanged — drilled, not cold-load)                                            |
| Cache hit rate on the index cache (§7.7.1)   | (no target)          | **≥ 80 %** measured over the K-day window                                                    |
| Resolver miss rate (§1.4 cache)              | (no target)          | **≤ 5 %** — every cold-load resolves the viewer; misses imply identity churn or a regression |
| Anonymous / no-identity render               | empty state + banner | **must render under 50 ms** (no DB hits beyond the resolver miss path)                       |

These tighter numbers force the implementation to rely on the
partial indexes from §7.3.2 (the `idx_beads_inbox_predicate` partial)
rather than the controller-side sort fallback. If the indexes have
not landed at promotion time, the bar is not met.

The anonymous-render budget is non-trivial: when promotion happens,
every unauthenticated probe (uptime checks, dev-bypass viewers)
hits the same handler. The fallback path must not trigger the
`category=intake AND operated_by_id IS NULL` query (which would scan
the partial-index-skipped portion of the table). §8.6 specifies the
fallback explicitly.

#### 8.5.4 Operator sign-off

The final lever is non-empirical: the operator pulls the trigger.
After §§8.5.1–8.5.3 are satisfied, the implementation epic files a
`gb decision` of the form:

> **Promote `/inbox` to `/`?** All four bars met (design, K-day
> proof, perf, no open blockers). Options:
>   - **A. Promote.** Cold-load default at `/` becomes the inbox-
>     constellation; project list moves to `/projects` (or stays at
>     `/` for anonymous viewers as the fallback in §8.6). Artifact:
>     `diff-summary` of the `server.go` route swap.
>   - **B. Defer.** One more dogfooding window — name the metric
>     that wasn't convincing. Artifact: `report` describing what
>     additional signal you need.
>   - **C. Pass.** Inbox stays at `/inbox` indefinitely; `/` keeps
>     `handleIndex`. Artifact: `report` documenting the call so
>     future promotion proposals don't re-litigate.

The decision is single-operator: the parent epic (`ka-0000006157`)
is locked to research/design and the operator is the only authority
that can flip the default surface for the viewer.

### 8.6 What `/` looks like *after* promotion

If the operator picks A in §8.5.4, the `/` route swap looks like:

```go
// gardenio/controller/cmd/beads-viewer/server.go (proposed at promotion)
mux.HandleFunc("GET /{$}", s.handleRoot)

func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
    op := s.resolveViewerOperator(r)
    if op == nil {
        // Anonymous / no resolved identity → fall back to the project list.
        s.handleIndex(w, r)
        return
    }
    s.handleInbox(w, r, op)
}
```

Three behaviours fall out of this shape:

- **Authenticated viewers** with a resolved operator id land on the
  inbox by default. The cold-load surface is the same shell §§1–7
  build at `/inbox`.
- **Anonymous / dev-bypass viewers** get the project list verbatim.
  The `[Inbox · N ↗]` chrome from §8.4.3 disappears for these
  viewers because there is no `N` to show.
- **`/inbox`** continues to resolve to the same `handleInbox`
  handler. Both routes converge so deep-links (and the on-call
  chip's URL state) survive promotion verbatim.

The project list moves to `/projects` (a new route registration that
points at `handleIndex`) so authenticated viewers who want the
discovery view can still get it. `/projects` is added a release
*before* the `/` swap so any saved bookmarks update.

### 8.7 Relationship to `/active`

#### 8.7.1 `/active` stays — different audience

`/active` answers a different question than the inbox: *what is in
progress across all projects right now*, grouped by project, chip-
filtered by operator. It is the operations / fleet view; the inbox
is the personal triage view.

The two surfaces share machinery (§4.7's reuse table — the
`project_row` partial, the chip CSS, the keys.js binding) but
*not* the workflow. A gardener monitoring CI doesn't want to see only
their assigned intake; they want to see every in-progress run across
the project. The inbox would force-narrow that view down to the
gardener's own beads.

`/active` therefore stays at `/active` indefinitely. It is **not**
folded into the inbox under any v1 or post-promotion plan.

#### 8.7.2 What changes about `/active` at v1

One small thing: `/active`'s chrome gains the `[Inbox · N ↗]` badge
from §8.4.3 so the inbox is reachable from the operations view
without going through `/`. Otherwise unchanged.

#### 8.7.3 What changes about `/active` at promotion

The chrome updates to reflect the new default-route world: the
top-of-page nav reads `[Inbox] · [Active] · [Projects]` (with
`[Inbox]` highlighted on its page, etc.). No structural change.

### 8.8 Relationship to `/decisions` and `/{id}` constellation

#### 8.8.1 `/decisions` — folds at promotion, not at v1

`/decisions` today renders a flat list of open decisions ranked by
age. It overlaps significantly with the inbox: every open decision
is `category=action`, and the inbox surfaces actions either via
their parent intake (drilled into the right pane) or via the orphan
pseudo-row (§6).

The fold sequencing:

- **At v1** (`/inbox` opt-in): `/decisions` is unchanged. It is
  still the canonical "show me decisions" surface for operators who
  haven't moved to the inbox. The inbox doesn't try to replace it.
- **At promotion**: `/decisions` redirects to a typed view of the
  inbox — `/inbox?type=decision` (a new chip variant or an inline
  filter the inbox renders when this query param is present). The
  redirect is **301 permanent** because promotion is an explicit
  operator decision and the rename is durable. Any external
  bookmark or Slack post that linked to `/decisions` lands on the
  filtered inbox, sees the same set of beads, and the operator's
  flow continues.
- **Long-term**: if the inbox-as-default surface holds for a second
  K-day window without bounce-back to `/decisions`, the route can
  be deleted. v1 does not commit to deletion.

#### 8.8.2 `/{id}` constellation — unchanged forever

The existing constellation render at `/{id}` (`handleBeadShow` →
`buildConstellation` in
[`constellation.go`](../../controller/cmd/beads-viewer/constellation.go),
1100 lines of layout, hydrate, edge styling, and fan-out caps) is a
**per-bead context view**, not a per-operator triage view. Its
audience is *"what does this bead's neighborhood look like"* — every
bead-show in the viewer renders one, regardless of who is signed in
or what they operate.

The inbox-constellation reuses the SVG primitives, the hydrate
worker pool, and the fan-out caps — but it does not modify them.
The parent epic pins this constraint ("the existing bead-show
constellation in `gardenio/controller/cmd/beads-viewer/constellation.go`
is **not modified** — this is a sibling view that reuses
primitives") and §§2–7 of this doc were written under it. Section 8
inherits the constraint verbatim.

Concretely:

- `buildConstellation` and its helpers stay where they are.
- The inbox surface adds a *sibling* `buildInboxConstellation`
  function in the same file (per §7.4.3), reusing every primitive.
- A `/{id}` deep-link from the inbox right pane is unchanged: it
  navigates to the existing per-bead view, with the existing
  constellation, in a new tab or by Cmd-click. The inbox does not
  re-render the bead-show layout.

There is no v1 or post-promotion scenario in which the bead-show
constellation gets removed, replaced, or restructured. It is a
permanent fixture.

#### 8.8.3 Cross-link affordances

The two surfaces benefit from cheap cross-navigation:

- The inbox right pane's per-node hover preview includes a "Open in
  bead view" link to `/{id}` — operators who want the full
  per-bead context (forward + reverse + mention edges) can jump
  there without losing inbox state.
- The bead-show page (`/{id}`) gains a small breadcrumb when the
  bead's category-derived ancestor walk (§6.5's inverted walk)
  reaches an intake the viewer operates: *"← Inbox: <intake-title>"*.
  Click returns to `/inbox/<intake-id>` with the intake selected and
  the constellation rendered. No-op when the bead is not in the
  viewer's intake-rooted set.

Both affordances are tiny chrome additions, not new surfaces. They
are filed as part of the implementation epic's first slice, not as
separate beads.

### 8.9 Relationship to the existing landing page (`/` = `handleIndex`)

`/` today renders a project list with `computeIndexStats` block.
§§8.4–8.6 have already pinned the v1 (no change) and post-promotion
(swap with anonymous fallback) shapes. This subsection just names
the affordance changes for completeness:

| Phase                | `/`                                                                  | Discovery of `/inbox`                                  |
| -------------------- | -------------------------------------------------------------------- | ------------------------------------------------------ |
| Pre-v1 (today)       | Project list                                                         | None (the surface doesn't exist)                       |
| v1 (`/inbox` opt-in) | Project list + `[Inbox · N ↗]` badge for resolved-identity viewers   | Badge from `/`, badge from `/active`, `g i` keystroke  |
| Post-promotion       | Authenticated viewer → inbox; anonymous → project list               | `[Projects ↗]` link from inbox chrome; URL `/projects` |

The `[Inbox · N ↗]` badge is the lightest discoverability hook that
moves operators onto the inbox without forcing it. If the badge
clicks turn into the dominant entry point for inbox traffic during
the K-day window (§8.5.2), that is itself a signal that promotion
will be welcome.

### 8.10 Open questions for operator review

1. **Confirm `/inbox` over `/operator-inbox`.** §8.3 recommends
   `/inbox` (with `/operator-inbox` as a permanent 308 alias). The
   parent epic's open-questions block prefers `/operator-inbox`
   "to preserve URL stability with ka-0000004660's framing" — but
   that epic is superseded and there is no live URL stability to
   preserve. Confirm the recommendation, or pick `/operator-inbox`
   as the canonical and `/inbox` as the alias (the same redirect
   shape works in either direction).
2. **Bounce-rate threshold (§8.5.2).** 25 % is a strawman.
   Is the right ceiling 25 %, 15 %, or higher (e.g. 40 %) given
   that operators may legitimately use `/active` for fleet-watch
   alongside the inbox for personal triage? The lower the
   threshold, the longer promotion waits.
3. **K-day window length (§8.5.2).** 14 days + at least one
   on-call rotation. Is one rotation enough, or should the window
   span two rotations to cover both an "easy" and "noisy" oncall
   shift?
4. **Sign-off mechanism (§8.5.4).** Single-operator decision is
   the recommendation. Should it instead require sign-off from
   each operator who will be hit by the default-flip (i.e. each
   operator with a resolved `operators` row)? The latter is more
   conservative but slower.
5. **`/decisions` redirect timing (§8.8.1).** The recommendation
   is that `/decisions` redirects to `/inbox?type=decision` *at
   promotion*, not at v1. Alternative: redirect at v1 to drive
   inbox adoption faster; risk is that operators who haven't
   internalized the inbox UX yet bounce off the redirect with
   confusion. Recommend: hold redirect until promotion.
6. **Project-list move to `/projects` (§8.6).** At promotion, the
   project list moves to `/projects` for authenticated viewers and
   stays at `/` for anonymous. Is `/projects` the right URL, or
   should it be `/p` (mirroring `/p/{project}`)? Recommendation:
   `/projects` (the singular `/p/...` is per-project, the plural
   `/projects` is the index — symmetric with `/agents` /
   `/a/{name}`).
7. **Anonymous-render budget (§8.5.3, 50 ms).** This is tighter
   than any other budget in the doc because every probe hits it.
   Is 50 ms an acceptable target, or do we need to factor in
   network jitter and set it at 100 ms? Subtask 7's perf work
   pinned the inbox itself at 270 ms cold; the *no-identity*
   fallback path is much cheaper (it short-circuits before the
   first DB hit) so 50 ms is achievable, but operator confirmation
   requested.
8. **Promotion-blocking bugs (§8.5.2).** Zero open
   `flag:operator-blocking` bugs labeled `inbox` is the bar.
   Is the project-wide gardenio blocker count also a bar (i.e.
   "no other unrelated blockers either"), or only inbox-component
   blockers?

### 8.11 Out of scope

- **Implementation of the route swap.** §8.6 sketches the handler
  shape but the actual `server.go` change, the `/projects` route
  add, and the `/decisions` redirect ship under the implementation
  epic that follows the doc green-light. This section pins the
  *contract*; subtask 9 (or its successor) ships the diff.
- **Modifying `/{id}` constellation.** Pinned by the parent epic
  and reasserted in §8.8.2. The bead-show constellation is a
  permanent sibling, not a v2 candidate.
- **A second promotion target (`/inbox` → primary navigation chip
  on `/active`).** Out of scope — `/active` is its own surface
  with its own audience (§8.7.1).
- **Reviving `beads3d/`.** Pinned out by the parent epic; this
  doc is about the SVG primitive in `beads-viewer/constellation.go`
  only.
- **Per-team or per-org default-surface configuration.** A future
  *"my team defaults to inbox at `/`, your team defaults to
  project list"* knob is interesting but is post-promotion. v1 and
  promotion both ship a single default per viewer (resolver-based,
  binary).
