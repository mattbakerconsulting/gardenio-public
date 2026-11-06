# `/operator-inbox` v1 — Option B sequencing plan

> **Status:** plan-of-record. Plan only — no implementation lands until
> the operator green-lights specific build steps.
>
> **Bead:** [ka-0000005952](#) — closes the artifact obligation from
> promotion gate [ka-0000002778](#) (resolved `wait-option-b`,
> 2026-05-05).
>
> **Parent epic:** [ka-0000006157](#) (closed; design-complete) —
> inbox-primary constellation view.
>
> **Substrate:** [ka-0000003625](#) — Pivot operated_by to Option B
> (operators FK + aliases). v1 inbox blocks on Option B P1 + P2.
>
> **Design doc:** [`gardenio/docs/research/inbox-constellation-design.md`](../docs/research/inbox-constellation-design.md)
> (all 8 §§ on main as of MR !2123).
>
> **Shared prerequisite:** [ka-0000003787](#) — D7 `DeriveCategory` plan.

This plan converts the §§1–8 design doc into a buildable sequence.
It defines (a) what must be true upstream before the inbox's v1 query
shape is correct, (b) what code lands when, (c) where the implementation
diverges from the design doc, and (d) what fresh decisions surface
before any build starts.

The plan deliberately does **not** schedule Option B's own work
(P1/P2/backfill) — that track is owned by [ka-0000003625](#). This
plan only consumes Option B milestones as predicates.

## 1. Sequencing checklist

The build path runs in five gates. Each gate is closed by a concrete,
checkable predicate; "yes/no" answers, not subjective sign-off.
The inbox v1 cut depends on gates 1–3; gate 4 is the cutover; gate 5
is the post-cutover hygiene window before the §8.5 promotion bar can
even be evaluated.

### Gate G0 — Substrate landed (out of inbox track scope)

This gate is owned by [ka-0000003625](#) and its child tasks. The inbox
track does not drive it; the inbox track only blocks on it.

| Predicate | How to check | Owner |
| --- | --- | --- |
| **G0.a** Option B ADR `docs/adr/human-attribution-identity-v2.md` is on main | `git log -1 docs/adr/human-attribution-identity-v2.md` | [ka-0000006334](#) — closed; merged via MR !2103 (commit `e6e31d77`) ✅ |
| **G0.b** Option B P1 (operators + operator_aliases tables + FK columns + sentinel rows) merged | `kd list --label='option-b-p1'` shows `closed`; migration 015 visible in `kbeads/migrations/`; sentinel ids 1–8 present in `operators` | [ka-0000003625](#) child task (**not yet filed** — see §7 OQ-1) |
| **G0.c** Option B P2 (bridges write `operated_by_id` instead of TEXT) merged and rolled out to gardenio-rwx | `gb pipeline status`; spot-check a recent Slack/GitLab/Jira-bridged intake — `beads.operated_by_id IS NOT NULL` | [ka-0000003625](#) child task (**not yet filed** — see §7 OQ-1) |
| **G0.d** Backfill of legacy Option-A `slack:U…`/`oidc:<email>` rows complete | `SELECT count(*) FROM beads WHERE operated_by IS NOT NULL AND operated_by_id IS NULL;` returns 0 (or matches a documented non-human residual) | Same Option B child (see §5) |

**Gating predicate for inbox v1 work to start:** G0.a + G0.b are
**both** required before any inbox-side code that references
`operated_by_id` can land cleanly. G0.c is required before the
inbox query gives correct results in production (without it, fresh
intake from bridges is invisible to the FK predicate). G0.d is
required for the cutover (gate G4) — see §5.

The "≥ X% non-null" predicate the task description mentions belongs
on G0.c, not on a single inbox-side metric: under the §1.4 resolver
contract, the inbox is *correct* when every freshly-stamped human
intake carries `operated_by_id`. A practical staleness-floor for the
cutover is **`operated_by_id` non-null on ≥ 99 % of rows where
`operated_by IS NOT NULL` and `operated_by_kind = 'human'`** for the
30-day window leading into G4 — that bound is operator-confirmable;
see §7 OQ-3.

### Gate G1 — Inbox-side server prerequisites (parallelisable with G0.b/G0.c)

These are inbox-track changes that can land on main *before* G0.b/G0.c
because they are additive (new column, new filter, new index) and
do not break existing surfaces. They are blocked only on G0.a (the
ADR) so the column shapes match.

| Predicate | Spec source | Owner |
| --- | --- | --- |
| **G1.a** `category` materialized column + maintenance triggers + partial index | design doc §7.3.2 (option B "trigger-maintained materialized column"); migration `016_category_materialized.up.sql` | inbox-track child of [ka-0000006157](#) (**not yet filed** — see §3.1) |
| **G1.b** `category` filter wired through `Filter`, `ListBeadsQuery`, HTTP query parser, postgres queries | design doc §7.3.3 — four small files: `kbeads/internal/model/filter.go`, `kbeads/internal/server/http_beads.go`, `kbeads/internal/store/postgres/queries.go`, `gardenio/controller/internal/beadsapi/client_query.go` | same |
| **G1.c** `idx_beads_inbox_predicate` partial index (`(operated_by_id, urgency_score DESC, id)` WHERE `category='intake'`) | design doc §1.2.2 + §7.3.2 | same |
| **G1.d** `urgency_score` column + writer hook on bead mutate + 1h sweep | design doc §4.4.3 + §4.9 OQ-2 (open question 2) | inbox-track child (see §7 OQ-2 — **partially open**) |
| **G1.e** `created_by_operator_id` immortal column on `beads` (raise-only chip prereq) | design doc §4.3.1 + §4.7 cross-ref + bead [ka-0000006467](#) (already filed, open) | [ka-0000006467](#) |

G1.a–G1.c can all ride a single migration. G1.d and G1.e are
schema-immortal and should land their migrations *before* the v1
controller code so the `beads` row payload is stable when the
view-model code lands.

### Gate G2 — Identity resolver primitive lifted

Per design doc §1.4, the viewer-resolver is shared substrate for
`/inbox`, `/active`, `kd list --operated-by=me`, and the operator chip.
G2 lifts this out of `operator_chip.go`'s ad-hoc helpers into a
standalone primitive that both surfaces consume.

| Predicate | Spec source |
| --- | --- |
| **G2.a** `viewer.OperatorID(ctx) (int64, bool, error)` returns the `operators.id` for the OIDC subject in `ctx`, hitting `operator_aliases(kind='oidc', subject=email)` with a 60s in-memory cache invalidated on `operators.updated_at` SSE | design doc §1.4.1, §1.4.2 |
| **G2.b** Resolver returns `kind ∈ {'system','fleet'}` operator ids if the bridge ever stamped them; the inbox handler enforces the human-only contract by passing `:viewer_operator_id` from `viewer.OperatorID`, which can never return a sentinel for an OIDC subject | design doc §1.5 — the contract is "resolver only ever returns human ids" |
| **G2.c** `/active`'s identity helpers (`resolveOperatedByParam`, `resolveDefaultOperatedBy`, `operatorIDsMatchViewer` in `operator_chip.go`) delegate to the new resolver | design doc §1.4.1; round-9 review-doc note "Lift IdentityResolver as a standalone primitive" |

G2 unblocks the v1 inbox handler. G2.c is *additive* — `/active`'s
existing string-keyed helpers can stay in place at G2 land time and
flip to the FK-resolver call site after G0.c reaches gardenio-rwx
(when bridges actually populate `operated_by_id`).

### Gate G3 — Inbox v1 surface ships behind `/inbox`

This is the inbox v1 cut. It assumes G0.a + G0.b + G0.c + G1 + G2 all
landed. The §1.2 query shape is the v1 form; no v0.5 fallback.

| Predicate | Spec source |
| --- | --- |
| **G3.a** `/inbox` route registered in `server.go` per design doc §8.3.3 | §8.3.3 URL surface table |
| **G3.b** `/operator-inbox` 308 redirect to `/inbox` registered | §8.3.3 |
| **G3.c** `handleInbox` and `handleInboxBead` handlers implemented per §§4–6 | §§4–6 |
| **G3.d** Sibling `buildInboxConstellation` added in `constellation.go` (no edits to `buildConstellation`) | parent-epic constraint + §7.4.3 + §8.8.2 |
| **G3.e** Index handler answers the §1.2.1 query shape (`category='intake' AND operated_by_id = :viewer_operator_id`); chip-strip variants per §4.3.1 | §1.2 + §4.3 |
| **G3.f** Missing-identity banner per §4.6.1 (Mode A blocking, Mode B informational) | §4.6.1 |
| **G3.g** Action surface §5 — peek panel, flag toggle, reparent (new endpoint), decision resolve/dismiss, comment | §5.2–§5.7 |
| **G3.h** Orphan pseudo-row per §6 | §6 |
| **G3.i** `[Inbox · N ↗]` chrome badge added to `index.html` and `active.html` per §8.4.3 | §8.4.3 |
| **G3.j** Regression test: sentinel-operated intake (`operators.id ∈ {sentinels}`) does **not** appear in any viewer's inbox result | §1.5 test obligation |
| **G3.k** Regression test: v1 perf budget — p50 ≤ 50ms, p95 ≤ 270ms TTFB on the cold-load index against a fixture matching §7.6 stress shape | §7.7 + §7.6 |

### Gate G4 — Cutover from `/inbox`-opt-in to `/`-default (out of v1 scope)

Gated by §8.5 (design completeness + §8.5.2 K-day proof + §8.5.3 perf
budgets + §8.5.4 operator sign-off). v1 ships behind `/inbox` only;
G4 is *not* part of v1 and is filed as a follow-up decision under the
implementation epic.

### Gate G5 — `/decisions` redirect and project-list move

At promotion (G4), `/decisions` 301-redirects to `/inbox?type=decision`
and `handleIndex` moves to `/projects` for authenticated viewers
(anonymous fallback at `/` per §8.6). v1 does **not** ship these
redirects.

## 2. v1 query shape

Verbatim from design doc §1.2.1, restated here so the build target is
unambiguous:

```sql
SELECT b.*
FROM   beads b
WHERE  b.category = 'intake'
  AND  b.operated_by_id = :viewer_operator_id
ORDER  BY <ranking>          -- §4.4 default sort = urgency_score DESC, id
LIMIT  :page_size;           -- 50 cold-load
```

Three properties that must hold at v1:

1. **Single FK equality.** No `IN`-list, no `ANY(text[])`,
   no application-layer expansion. `viewer_operator_id` is a single
   `BIGINT` resolved by `viewer.OperatorID(ctx)` from §1.4.1 once per
   request.
2. **Index-served.** The query plan is an index scan on
   `idx_beads_inbox_predicate (operated_by_id, urgency_score DESC, id)
   WHERE operated_by_id IS NOT NULL AND category = 'intake'`. No heap
   sort. See §7.3.4 for the EXPLAIN shape.
3. **Sentinel-safe.** The resolver contract (§1.5) guarantees
   `:viewer_operator_id` is always a `kind='oidc'` operator id (or a
   human operator id resolved from one). It can never equal a
   `kind='system'` or `kind='fleet'` id, so sentinel-operated intake
   cannot match. The G3.j regression test exists to keep the contract
   honest.

The v0.5 path (`operated_by = ANY($1::text[])` with application-layer
identity expansion, design doc §1.3) is **explicitly out of v1 scope**.
Per the resolved promotion gate ka-0000002778 (`wait-option-b`), the
inbox waits for Option B P1+P2 rather than shipping the throwaway.
v0.5 is referenced in the design doc only as documentation of the
shape we are *not* building; this plan does not file it as a fallback
or a parallel track.

## 3. Routes / view-model deltas vs. design doc

This section identifies what the build adds, what it reuses, and where
the implementation diverges from the design doc's wording.

### 3.1 Routes added

Per design doc §8.3.3 + §5.2:

| Method | Path | Handler | Notes |
| --- | --- | --- | --- |
| `GET` | `/inbox` | `handleInbox` (new) | §4 cold-load; default chip = `mine`; §1.2 query |
| `GET` | `/inbox/{id}` | `handleInboxBead` (new) | §2 subgraph; left rail kept, right pane swapped |
| `GET` | `/inbox/orphans/{scope}` | `handleInboxOrphans` (new) | §6.6.4 pseudo-intake drill |
| `GET` | `/api/v1/inbox/{id}/subgraph` | `handleInboxSubgraphJSON` (new) | §7.3 HTMX subgraph swap (small JSON) |
| `GET` | `/operator-inbox` | `redirectPerm("/inbox")` | §8.3.3 permanent 308 alias |
| `GET` | `/operator-inbox/{rest...}` | `redirectPerm("/inbox/...")` | §8.3.3 path-preserving alias |
| `POST` | `/b/{id}/parent` | `handleSetParent` (new) | §5.5 — only new mutation endpoint |
| `POST` | `/b/{id}/flag/{flag}` | `handleFlagToggle` (new, **optional**) | §5.4.2 ergonomic helper; reuse of `/b/{id}/labels` is acceptable |

Everything else in §5.2's reuse table is already on `appMux` and the
inbox calls it as-is: `/b/{id}/preview`, `/b/{id}/labels`,
`/b/{id}/decision/resolve`, `/b/{id}/decision/dismiss`,
`/b/{id}/comments`, `/b/{id}/comments/preview`.

The only new schema-touching mutation is `POST /b/{id}/parent` —
design doc §5.5 makes the case that rewriting `parent(ka-X)` typed
mentions in the description (the existing affordance) is ergonomically
wrong for the inbox surface.

**Net new server endpoints under v1: at most 6** (4 GET + 1 mutation
`/parent` + 1 optional `/flag/{flag}`). Of these, only `/parent`
needs new authorization plumbing; the rest are read-only or wrap the
existing labels mutation.

### 3.2 View-model deltas

Per design doc §1, §4, §7.3.3:

- **`ListBeadsQuery.Categories []string`** — new field. Wired through
  `client_query.go`, `http_beads.go`, `Filter`, `postgres/queries.go`
  (§7.3.3). Mirrors existing `Types`/`Kinds`/`Statuses`/`Labels`.
- **`BeadDetail.Category string`** — already returned by D7's
  `DeriveCategory` shape per [ka-0000003787](#). Once G1.a's
  materialized column lands, the field reads from the column rather
  than being computed at `toDetail()` time, but the wire shape is
  unchanged.
- **`BeadDetail.UrgencyScore float64` (or `int`)** — new immortal
  column from G1.d. Surfaced on every list response so the `mine` chip
  can sort by it without a join.
- **`BeadDetail.CreatedByOperatorID int64`** — new immortal column
  from G1.e. Used only by the `raise-only` chip (§4.3.1).

### 3.3 Reused primitives (no edits)

Per design doc §4.7 reuse table + §8.8.2 ("`/{id}` constellation
unchanged forever"):

| Primitive | File | Inbox uses it for |
| --- | --- | --- |
| Identity resolution (post G2.c) | `gardenio/controller/cmd/beads-viewer/operator_chip.go` (`resolveOperatedByParam:120`, `resolveDefaultOperatedBy:155`, `operatorIDsMatchViewer:378`) | Chip-strip backing + missing-identity banner data |
| Constellation SVG primitives, age-opacity, status fills, edge styling, fan-out caps, hydrate worker pool | `gardenio/controller/cmd/beads-viewer/constellation.go` (1174 lines, all of `buildConstellation` and helpers) | Sibling `buildInboxConstellation` reuses every helper; *zero edits* to the existing function |
| Two-pane shell template | `templates/active.html` skeleton | `templates/inbox.html` clones the structure |
| Per-row card markup | `project_row` partial | Same row markup so `keys.js` discovers `.bead-row` |
| Keyboard navigation | `templates/static/keys.js` (j/k/Enter/gg/G/?) | Adds `g i` rebind + new `g n` (§4.5.2) |
| Chip strip CSS | `chip` / `chip-active` / `chip-disabled` classes on `active.html` | Used as-is |
| Hover preview popover | `templates/static/preview_popover.js` + `GET /b/{id}/preview` (`bead_preview.go:59`) | Peek panel renders the same fragment in a docked surface (§5.3) |
| Banner pattern (allowlist plumbing only) | `shouldRenderMigrationBanner` in `active.go` | Missing-identity banner reuses the allowlist; *no* per-viewer dismissal cookie (§4.6.3) |
| cmdk overlay | `templates/static/cmdk.js` | Untouched |

### 3.4 Sections of the design doc the build ignores

The design doc's `Out of scope` blocks and explicit deferrals translate
to the following v1 build behaviour:

- **No `kd list --operated-by=me` CLI mirror at v1** (§1.7). The
  resolver primitive (G2) is the substrate; the CLI is a follow-up
  bead, not v1.
- **No `beads.urgency_score` writer-hook beyond the strawman in §4.4.3
  (every-mutation + 1h sweep).** §7 OQ-2 is open; the build targets
  the strawman.
- **No saved-views integration on the inbox chrome** (§4.8). Saved
  views (`/views/<id>`) link out of the inbox; the inbox does not
  render saved-view chips at v1.
- **No multi-select / bulk operations at v1** (§5.9). Single-bead
  actions only.
- **No undo for decision resolve** (§5.10.3). Other destructive actions
  follow §5.10.2 patterns.

## 4. Onboarding flow

Per design doc §4.6.1 (under Option B / v1):

The viewer-resolver (§1.4) returns one of three states for each
request:

1. **Resolved.** OIDC subject maps to a single human operator id;
   inbox renders normally; banner suppressed unless §4.6.1 Mode B
   condition fires.
2. **No `operators` row** (Mode A). Resolver finds nothing in
   `operator_aliases(kind='oidc', subject=viewer_email)`. Inbox
   renders the blocking Mode-A banner; chip strip hidden; no
   index rows fetched. Banner CTA:
   ```
   gb identity add --oidc <viewer_email>
   ```
3. **Operator row exists but missing platform alias** (Mode B). The
   `operators` row is present; `operator_aliases` lacks at least one
   `kind ∈ {seen-source-set} \ {present-aliases}` for the past 30 days.
   Inbox renders normally; informational banner above the chip strip
   names the missing platform(s). Banner CTA:
   ```
   gb identity add --slack U0OPERATOR1       # or --gitlab, --jira, etc.
   ```

Both CTAs invoke the same `gb identity add` command, which under
Option B upserts a row in `operator_aliases` (creating the
`operators` row if it does not already exist). The CLI shape is the
single onboarding lever; the banner copy is the only UX surface that
prompts it.

**Reframing note.** §4.6's design-doc language is already the v1
(Option B) shape. The Round-9 framing — *"create a `user_identity`
bead via `gb identity add`"* — is referenced in the design doc only
as the v0.5 fallback (§4.6.2). This plan adopts the v1 framing
directly: there is no `user_identity` bead to create; `gb identity add`
writes to `operator_aliases`. Existing code or docs that mention
`user_identity` as the onboarding artifact need a follow-up sweep —
filed as §7 OQ-4 below if any are found.

**Banner suppression** (§4.6.3):

- Anonymous viewers never reach the handler (OIDC-gated upstream).
- Allowlisted operators (per `shouldRenderMigrationBanner`'s pattern
  in `active.go`) do not see the banner. We do **not** add a
  per-viewer dismissal cookie.

## 5. Migration / backfill story for legacy Option-A rows

This section is explicitly **cross-link only** per the task scope —
the inbox track does not own the migration. It identifies what must
happen and which Option B child task should own it.

### 5.1 What rows need migrating

When G0.b lands (operators + operator_aliases tables + FK columns +
sentinel rows), `beads.operated_by` is still populated per
migration 012 (`12_human_attribution.up.sql`) with TEXT values like:

- `slack:U0OPERATOR1` — Slack-bridge writes from Option A.
- `oidc:matt@acme.example` — OIDC CLI writes from Option A.
- `gitlab:matthewbaker` — GitLab-bridge writes.
- `jira:abc123` — Jira-bridge writes.
- `system:scheduler`, `fleet:gardenio`, … — sentinel writes.

`beads.operated_by_id` is added by Option B's migration 015 but is
`NULL` for every pre-existing row until a backfill populates it.

### 5.2 Backfill shape

The Option B ADR (`docs/adr/human-attribution-identity-v2.md` §4)
specifies a `kd admin backfill-operators` command that walks the
distinct set of `(kind, subject)` strings in the existing
`operated_by` column, upserts `operators` + `operator_aliases`
rows, then `UPDATE`s every matched bead with `operated_by_id`.

For human rows, the backfill needs the same identity-unification map
the Round-9 application-layer resolver would have synthesised (the
data the v0.5 path read from `user_identity` beads). Per design doc
§1.6.4: *"The data the v0.5 resolver reads from `user_identity` is
the same data Option B's backfill migration walks to populate
`operator_aliases`."*

For sentinel rows (`system:*`, `fleet:*`), the backfill matches
against the pre-seeded sentinel rows (ids 1–8 per ADR §3.1).

### 5.3 Owner

**Open ([§7 OQ-1](#7-open-questions)).** As of 2026-05-05 the Option B
epic [ka-0000003625](#) has no filed P1/P2/backfill child tasks —
only the merged ADR ([ka-0000006334](#)) and the rollout-flag-flip
child ([ka-0000000630](#), inherited from Option A). The backfill
owner needs to be filed under [ka-0000003625](#) before G0.d can be
checked off.

### 5.4 Cutover correctness predicate

When the backfill completes, the inbox must observe:

```sql
-- All human-stamped rows have a FK.
SELECT count(*) FROM beads
WHERE operated_by IS NOT NULL
  AND operated_by_kind = 'human'  -- or operated_by NOT LIKE 'system:%' AND NOT LIKE 'fleet:%'
  AND operated_by_id IS NULL;
-- Expect: 0, or a bounded residual operator-confirmed.
```

```sql
-- Spot check: all of Matt's rows roll up to one operator id.
SELECT operated_by_id, count(*) FROM beads
WHERE operated_by IN (
  'slack:U0OPERATOR1',
  'oidc:matt@acme.example',
  'gitlab:matthewbaker'
)
GROUP BY operated_by_id;
-- Expect: one row, count = sum of inputs.
```

Both predicates belong on the Option B backfill task's done-list, not
the inbox track's. This plan flags them so the inbox cutover (G4)
has a checkable G0.d predicate to read.

## 6. Promotion sequencing — when `/inbox` becomes default at `/`

Per design doc §8.5, promotion is gated on four legs. v1 (the scope of
this plan) ships only the opt-in surface at `/inbox`; **promotion is
out of v1 scope** but filed as a follow-up so the bar is durable.

### 6.1 Bar restatement

| Leg | Predicate | Source |
| --- | --- | --- |
| **A. Design completeness** | All §§1–8 of the design doc green-lit; all §-level Open Questions resolved; G1.a–G1.e all landed (`category` materialized, `urgency_score`, `created_by_operator_id`, partial indexes); Option B P1+P2 reached prod | §8.5.1 |
| **B. Proof-in-use** | 14-day window with ≥ 1 on-call rotation; ≥ 30 distinct triage sessions across ≥ 2 operators; bounce rate ≤ 25 %; ≥ 3 actions/session median; "would default to this" sentiment from ≥ 2 of 2 active operators; zero open `flag:operator-blocking` bugs labelled `inbox` | §8.5.2 |
| **C. Perf budgets** | p50 ≤ 50 ms; p95 ≤ 200 ms (tightened from v1's 270 ms); p99 ≤ 400 ms; index-cache hit-rate ≥ 80 %; resolver miss-rate ≤ 5 %; anonymous-render ≤ 50 ms with no DB hits beyond resolver | §8.5.3 |
| **D. Operator sign-off** | Single decision bead filed by the implementation epic with options Promote / Defer / Pass; `diff-summary` artifact for option A | §8.5.4 |

§7 OQ-5 through OQ-7 surface the operator-confirmation needs the
design doc itself flagged on §8.5.

### 6.2 Promotion code change

A single `server.go` route swap (~10 lines), per design doc §8.6:

```go
mux.HandleFunc("GET /{$}", s.handleRoot)

func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
    op := s.resolveViewerOperator(r)  // G2.a primitive
    if op == nil {
        s.handleIndex(w, r)            // anonymous / no resolved id → project list
        return
    }
    s.handleInbox(w, r, op)            // resolved id → inbox
}
```

Plus:

- `/projects` route added before the `/` swap (one release earlier),
  pointing at `handleIndex`.
- `/decisions` 301-redirects to `/inbox?type=decision` (§8.8.1).

Both are `/`-route choreography under G4/G5 and are not in the v1 cut.

### 6.3 What does **not** change at promotion

Per design doc §8.7, §8.8:

- `/active` stays at `/active` indefinitely (different audience).
- `/{id}` constellation stays unchanged forever (per-bead context view).

## 7. Open questions

The build cannot start cleanly until these resolve. Each is filed
against the right owner; the inbox track does not own any of them
unilaterally.

### OQ-1. Option B P1/P2/backfill child tasks not yet filed

[ka-0000003625](#) has only the ADR child ([ka-0000006334](#)) and
the rollout-flag-flip child ([ka-0000000630](#)). The schema migration
(P1), bridge resolver writes (P2), and `kd admin backfill-operators`
(backfill) need explicit child tasks under [ka-0000003625](#) before
G0.b/G0.c/G0.d can be evaluated. Cross-link only — this plan flags
the gap; the Option B track owner files the children.

### OQ-2. `urgency_score` writer-hook timing

Design doc §4.9 OQ-2 (open). Strawman: recompute on every bead mutate
+ 1h sweep. Build targets the strawman. Operator confirmation requested
before G1.d lands so the column doesn't churn between v1 cut and
promotion.

### OQ-3. Cutover `operated_by_id` non-null floor

The task description suggests "operated_by_id non-null on > X% of
recent rows." This plan recommends **≥ 99 %** on rows where
`operated_by IS NOT NULL` and the value is not a sentinel, in the
30 days leading into G4. Operator-confirmable, not a correctness
requirement (sentinels and edge-case rows can stay non-FK; the inbox
predicate handles them via §1.5).

### OQ-4. `user_identity` bead references in existing code/docs

Round-9 framing introduced a `user_identity` bead concept. The v1 plan
adopts the Option B framing directly — `gb identity add` writes to
`operator_aliases`, not a `user_identity` bead. A search/sweep is
needed to find any code that still references `user_identity` (e.g.
in CLI subcommands, `gb identity` plumbing, Helm values, prior
gardener prompts). Build cannot ship the v1 banner CTA until the CLI
under-the-hood writes to `operator_aliases`. Cross-link to Option B
P2 / [ka-0000000630](#) sequencing.

### OQ-5. Bounce-rate threshold for promotion (design doc §8.5.2)

25 % strawman. Operator confirmation needed before the K-day window
starts; this plan defers to design doc §8.10 OQ-2.

### OQ-6. K-day window length for promotion (design doc §8.5.2)

14 days + 1 on-call rotation strawman. Operator confirmation needed;
defers to design doc §8.10 OQ-3.

### OQ-7. Promotion sign-off mechanism (design doc §8.5.4)

Single-operator vs. per-operator-with-resolved-row. Defers to design
doc §8.10 OQ-4.

### OQ-8. Optional `/flag/{flag}` toggle endpoint

Design doc §5.2 names this as an optional ergonomic helper over the
existing `/labels` mutation. Build can defer it; the optimistic UI
(§5.4.3) works with either shape. Recommendation: defer to a follow-up;
v1 reuses `/b/{id}/labels`. Operator confirmation not strictly required.

### OQ-9. `/operator-inbox` vs `/inbox` canonical (design doc §8.10 OQ-1)

§8.3 recommends `/inbox` canonical with `/operator-inbox` as 308
alias. Parent-epic open-question prefers the inverse. Both shapes work;
this plan adopts §8.3's pick. Operator confirmation requested before
the 308 ships either way (the redirect direction is durable).

## 8. Sequencing summary diagram

```text
   [Option B track]                          [Inbox track (this plan)]
   ────────────────                          ───────────────────────────

   G0.a  ADR merged ✅
            │
            ▼
   G0.b  P1 schema (operators + aliases + FK) ──┐
            │                                   │ unblocks
            ▼                                   ▼
   G0.c  P2 bridges write FK                G1   Inbox prereqs
            │                              (cat. col + urgency + raise + index)
            ▼                                   │
   G0.d  Backfill                              ▼
            │                              G2   Resolver primitive
            └─────────────┐                     │
                          ▼                     ▼
                          └─────────────────► G3   Inbox v1 ships at /inbox
                                                  │
                                                  ▼
                                                 (dogfooding window §8.5.2)
                                                  │
                                                  ▼
                                              G4   Promotion to /
                                                  │
                                                  ▼
                                              G5   /decisions redirect, /projects move
```

## 9. Out of scope

- **Implementation.** Plan only; no code lands until operator
  green-lights specific build steps via `gb decision`.
- **Driving Option B P1/P2/backfill.** Owned by [ka-0000003625](#).
- **Modifying `/{id}` constellation.** Per parent-epic constraint and
  §8.8.2 — permanent fixture.
- **Reviving `beads3d/`.** Pinned out by parent epic; SVG primitive
  in `beads-viewer/constellation.go` is the only target.
- **v0.5 throwaway path.** Resolved promotion gate ka-0000002778
  picked `wait-option-b`; v0.5 is documented in §1.3 of the design
  doc but not built.
- **Per-team / per-org default-surface configuration.** Post-promotion
  enhancement, not v1.
- **Multi-select / bulk operations, undo for decision resolve, saved-
  views chrome integration.** Per design-doc out-of-scope blocks.
