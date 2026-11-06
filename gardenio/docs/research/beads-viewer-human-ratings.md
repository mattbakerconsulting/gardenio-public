# beads-viewer human-ratings — research note

**Bead:** ka-0000000547 (T1, research). Parent epic: ka-0000007308.
**Status:** research/design only — no code, no schema migration, no UI.
**Successor:** the T2 decision bead must pick a data-model fork and a UX fork from this doc.

This note answers the open questions in the parent epic. It surveys the current feedback signals in beads + the iOS Gardenio client, then sketches three data-model options and three UX options with tradeoffs.

---

## 1. TL;DR

- Today there is no first-class human-rating signal in beads. The closest signals — advice beads, comments, decision option resolution, MR review state, cost rows — were each designed for a different purpose and degrade as a ratings substrate.
- The iOS Gardenio client has **no ratings UI**. `BeadsTab.swift` and the in-file `BeadDetailView` are read/spawn/close only — no star rating, thumbs, score field, or rating sheet exists. There is therefore no parity constraint on the schema; we may pick what fits the htmx viewer best.
- Three data-model options worth taking to the operator: **(A) `rating` bead type**, **(B) typed comment subtype**, **(C) JSONB `ratings[]` array on the rated bead's `fields`**. Recommendation: **A**. Reasons in §5.
- Three UX options worth taking to the operator: **(I) standalone `/ratings` tab + per-bead inline widget**, **(II) per-bead inline widget only (no top-level tab)**, **(III) ratings-as-comments thread on every bead**. Recommendation: **I**, but the operator should explicitly pick scope between "rate any bead" and "rate only MR / agent-run beads" before T3 starts.
- Recommended rating schema: **1–5 stars (required) + multi-axis tag set (optional) + free-text comment (optional)**. Reasons in §6. The operator should explicitly accept or replace this in T2.
- Recommended unit: **the rated bead itself** (any bead may receive ratings; rating beads point at the rated bead via a `rates` dependency edge). Initial roll-out should encourage rating MRs (`mr` beads), agent-run gardener beads, and decision-resolution outcomes — but the schema doesn't restrict you. Reasons in §6.
- Backwards-fill: **allowed** — ratings carry their own `created_at`; rating a closed bead is fine. Operator can rate retroactively without reopening anything.
- Multi-rater: **allowed** — one rating bead per (rater, rated-bead) pair. Re-rating updates that bead in place rather than creating a new one.

The rest of this doc backs these recommendations.

---

## 2. Inventory of existing feedback signals

### 2.1 Advice beads (`type=advice`)

- **What they are.** Durable, scoped guidance the operator wants every future agent run to honor. Stored as `advice` beads with scope (global / project / role / agent / advanced) and a body (`gb advice show <id>` reads it; `gb prime` injects the curated list at session start).
- **Why they aren't ratings.** Advice is rule-shaped, prescriptive, and forward-looking ("from now on, never push to main"). It is not pinned to a specific work output. Its success metric is "future agents stop doing X", not "this MR was good." Conflating the two would either flood the advice corpus with single-incident ratings or strip ratings of their per-output specificity.
- **Where they overlap.** A *pattern* of low ratings on similar work output may legitimately motivate an advice bead. T3's plan should sketch the linkbacks from a rating to a derived advice bead, but the rating itself is not advice.

### 2.2 Comments (`Store.AddComment` / `GetComments`)

- **What they are.** Free-text notes attached to any bead. Authored via `kd comment` or the Slack bridge. Always have an actor + timestamp; no structured field.
- **Why they aren't ratings.** No score. No multi-rater aggregation. No native query surface for "MRs averaging ≥4 stars" or "agent X's last 50 outputs". Free-text is great for context, terrible for grading.
- **Where they overlap.** A rating bead **should carry** the operator's free-text justification as a comment on the rating bead. That keeps the field structured and the rationale durable, and avoids re-inventing comment storage.

### 2.3 Decision-bead resolutions

- **What they are.** A `decision` bead's options are resolved by a human picking one (`gb decision resolve <id> --option <id>`). The picked option becomes a structured field on the decision; option-level free-text feedback is allowed via `--feedback`.
- **Why they aren't ratings.** Decisions answer "which path do we take?", not "how good was this output?" The resolution is binary (one option wins) per decision. There is no notion of "this decision was resolved badly in retrospect."
- **Where they overlap.** A decision *outcome* is itself a rateable artifact. The Ratings tab should be able to record "the path we picked here turned out poorly" as a rating that points at the decision bead.

### 2.4 MR review state

- **What it is.** GitLab/GitHub MR state — approvals, comments, threaded replies, merge status. Visible to the controller via `glab` / `gh` and surfaced today on `mr` beads through their fields (`mr.url`, `mr.state`, `mr.approvals`, etc.).
- **Why it isn't ratings.** Approval is a gate signal ("ship it / hold"), not a quality grade. A merged MR may still be a 2-star MR that the team accepted because the bead was time-pressured; conversely, a closed-without-merging MR may be 5-star research that simply didn't translate into code.
- **Where it overlaps.** The Ratings tab should be biased to surface MR beads as a rateable unit, since they are the densest "concrete agent output" beads we have. The rating is **distinct from** the MR review state and lives on a different bead.

### 2.5 Cost beads

- **What they are.** Per-account / per-agent token usage and dollar spend (`type=cost-account`, `type=agent-cost`). They power the Costs tab.
- **Why they aren't ratings.** Spend is an input efficiency signal, not an output-quality signal. A 1-cent agent run can produce garbage; a $30 run can produce a brilliant doc. Conflating cost with quality would punish thoroughness.
- **Where they overlap.** A useful future analytic is "rating per dollar spent" — but that is a join of two distinct signals on the read side, not a single conflated record.

### 2.6 Composite picture

| Signal | What it grades | Structured score? | Per-output? | Multi-rater? | Comment-bearing? |
|---|---|---|---|---|---|
| Advice | future behavior | no | no (rule-scoped) | no | yes (body) |
| Comments | anything | no | yes | yes | n/a |
| Decision resolution | path choice | option-id only | yes | one decider | yes (feedback) |
| MR review state | merge gate | approve/block | yes | yes | yes (threads) |
| Cost beads | spend | yes (numeric) | yes | n/a | no |
| **Rating (proposed)** | **quality** | **yes** | **yes** | **yes** | **yes** |

A rating bead occupies the empty "structured + per-output + multi-rater + comment-bearing + about-quality" cell. Nothing today does this.

---

## 3. iOS Gardenio survey

### 3.1 Files inspected

- `usr/Gardenio/Gardenio/BeadsTab.swift` (the file the bead description names) — 757 lines, contains `BeadsTab` (list) + `BeadDetailView` (detail) + `SpawnPromptSheet` + helper views.
- `usr/Gardenio/Gardenio/BeadDetailView` — there is **no separate `BeadDetailView.swift` file**. `BeadDetailView` is defined inline inside `BeadsTab.swift` at line 299.
- All other tab files: `AdviceTab.swift`, `AdviceDetailView.swift`, `AgentsTab.swift`, `CostsTab.swift`, `DecisionsTab.swift`, `InProgressTab.swift`, plus `Models.swift`, `KbeadsClient.swift`, `KbeadsStore.swift`.

### 3.2 Search for any rating affordance

```
grep -li "rating|star|thumb|score" usr/Gardenio/Gardenio/*.swift
```

Hits are spurious — `KbeadsClient.swift` mentions HTTP status, `Models.swift` contains the `status` field, `OIDCAuthManager.swift` mentions auth status, `UsageMonitor.swift` mentions throttle status. **None of them implement a rating UI.** `BeadDetailView` only renders status / type / priority / kind chips, an assigned-agent card, the People block, description, notes, labels, fields, timestamps, and Spawn/Close action buttons. There is no star widget, no thumbs control, no `rating` field rendered, no "rate this" affordance.

### 3.3 What this means for the design

- **No parity constraint.** Whatever schema we land on does not have to match an existing iOS data structure. The iOS app's `Bead` and `BeadFields` models can absorb a new `rating` bead type or a new typed comment as a `Models.swift` change later, but that is a separate (out-of-scope) iOS epic.
- **No existing UX to copy.** We are designing the rating UX from scratch on the htmx side. The iOS team can copy us later if they want parity.
- **Nav idiom is well-defined.** iOS uses a `TabView` of named tabs (`AppTab` enum: beads/active/agents/decisions/advice/costs). The htmx viewer's nav (`gardenio/controller/cmd/beads-viewer/templates/layout.html`) mirrors that — beads / Active / Agents / Decisions / Advice / Schedules / Costs. A new `Ratings` tab fits naturally as another peer.

---

## 4. Bead-stack constraints worth knowing before T3

These are existing facts about kbeads + beads-viewer that constrain implementation in T3/T4/T5. They are **not** decisions for T2; they are the floor any plan must stand on.

- **Bead model is JSONB-extensible.** `kbeads/internal/model/bead.go:Bead` carries a `Fields json.RawMessage` field validated against a `[]FieldDef` per type (`type_config.go`). New typed fields land in `type_config` without a schema migration. This is what `decision`, `schedule`, `agent`, `cost-account`, etc. all do today.
- **New bead types are first-class.** Adding `type=rating` is a config change, not a column add — it gets a `TypeConfig` entry with `FieldDef`s for `score`, `axes`, `rated_bead_id`, etc.
- **Dependency edges are typed.** `model.DependencyType` already supports custom edge labels. A `rating` bead pointing at its target via a `rates` edge fits the existing `Store.AddDependency` API.
- **Comments are universal.** Any bead can receive comments via `Store.AddComment` regardless of type. A rating bead can carry the operator's free-text rationale as a comment without inventing a new column.
- **Auth posture in beads-viewer is OIDC-cookie.** Routes are gated by `oauth2-proxy`; the user identity flows in via `oidcauth.EmailFromContext(ctx)`. A rating's `rater` is just `EmailFromContext(ctx)` at submit time. No new auth design needed.
- **Nav is a flat strip in `layout.html`.** Adding a tab is a one-line edit + a new route + a new template; the parity-phase2 doc sketched the same path for past tabs.
- **Backfill is free.** A new bead type can be created any time and immediately points at any pre-existing bead. There is no "must rate within 7 days" constraint at the data layer; that is a UX decision, not a model decision.

---

## 5. Three data-model options

Each option is sketched as if the operator picked it. I name the schema, the read/write APIs, the migration cost, and the failure modes.

### Option A — `rating` bead type (a new type)

A rating is itself a bead. New `type=rating` registered in `type_config`. One rating bead per `(rater, rated-bead)` pair.

**Schema** (TypeConfig fields on the rating bead):

| field | type | required | notes |
|---|---|---|---|
| `rated_bead_id` | string | yes | the bead being rated; also encoded as a `rates` dependency edge for graph queries |
| `score` | int (1–5) | yes | overall star rating |
| `axes` | object | no | `{correctness:int, scope:int, efficiency:int, communication:int}`, each 1–5 — optional secondary scores |
| `tags` | string[] | no | small controlled vocab (e.g. `regression-risk`, `over-scoped`, `clean`, `concise`); operator-curated |
| `rater` | string | yes | OIDC email at submit time; auto-set, not user-editable |

The rating bead's `title` is auto-generated (`Rating: <rated-bead-title> — <score>★ by <rater>`). Free-text rationale lives as a comment on the rating bead via the existing `AddComment` path. Multi-rater is natural: each rater gets their own rating bead. Re-rating: the same rater rating the same target updates their existing rating bead in place (idempotent on `(rater, rated_bead_id)`).

**Read surfaces** are cheap — the existing list/filter daemon RPCs already support filtering by `type=rating` plus an optional `fields.rated_bead_id=…`. Aggregations (average score per rated-type, leaderboard by agent) are SQL on the bead table + JSONB extraction.

**Tradeoffs.**
- **+** Maximal alignment with our "everything is a bead" idiom. Future-extensible (a rating can be commented on, can carry its own labels, can be queried by all the tooling that already understands beads).
- **+** Backfill is trivial: rate any bead any time, no foreign-key worries.
- **+** Auth/comment/dependency machinery is reused; no new RPCs or tables.
- **−** A rating is a noun-shaped object on top of a verb-shaped action. Operators may find "where is my rating? oh, it's a bead with id `kd-…`" surprising. UX has to keep the bead-id mostly hidden.
- **−** Adds rows to the bead table. At, say, 50 ratings/day this is negligible; at 5000/day from automation it is not. The schema does not force a TTL.

### Option B — typed comment subtype

Ratings ride on the existing `Comment` machinery. A new optional `kind` field on `Comment` (or a JSONB `meta` blob) carries `kind=rating`, `score`, `axes`, `tags`. The free-text body is the rationale.

**Schema.** One column or one JSONB field added to `Comment`. Validation enforces "score is required when kind=rating; otherwise must be unset."

**Read surfaces** are awkward. Today there is no "list comments across beads" query; comments are read per-bead via `GetComments(ctx, beadID)`. A leaderboard view ("all ratings authored by Matt, last 30 days") would require a new RPC. JSONB-on-comment also splits the validation surface — bead types validate fields via `FieldDef`s; comments would need a parallel mechanism.

**Tradeoffs.**
- **+** Lightest visible surface — no new bead type, no nav tweaks needed.
- **+** Naturally per-bead: "comments + ratings" reads as one thread, ordered by time.
- **−** Loses bead-shaped queryability. Average-score-per-agent or top-rated-MRs become bespoke comment scans.
- **−** New write path / new validation path for comments. Today every comment is text-only; introducing a typed subtype has spill-over risk into Slack bridge formatting, comment HTMX partials, etc.
- **−** Backfill requires a Comment migration (not just a config addition).

### Option C — JSONB `ratings[]` array on the rated bead's `fields`

Ratings are packed inline into the rated bead's `fields.ratings` array. Each element: `{rater, score, axes?, tags?, comment?, ts}`. Updating a rating mutates the array.

**Schema.** No new bead type. No new comment kind. One JSONB array on the rated bead, validated by an extension to that type's `FieldDef`s — but to support rating *any* type, every type's `FieldDef` set would need the new array, or `FieldDef` validation has to learn a "common fields" concept.

**Read surfaces** for "ratings on bead X" are trivial — read the bead, render the array. For "all ratings authored by X" or "top-rated MRs" you must scan every bead's `fields` JSONB and unnest. Postgres can do it with a GIN index, but the query is bead-table-wide.

**Tradeoffs.**
- **+** Co-locates the rating with the rated artifact. One bead read returns artifact + ratings.
- **+** Avoids a row explosion in the bead table.
- **−** Every type now carries a `ratings[]` field. The `FieldDef` model assumes per-type fields. Common cross-type fields are not idiomatic in our store today; introducing them risks a refactor whose blast radius is bigger than the feature.
- **−** Concurrent writes to the same bead's `fields.ratings` need optimistic-concurrency or row locks. Bead writes today don't expect simultaneous rating + status edits from different paths.
- **−** "Rate-the-bead" mutations can race with "edit-the-bead" mutations from controllers. Rating is a high-frequency append; the bead row was sized for low-frequency lifecycle edits.

### Recommendation: **Option A.**

Reasons:
1. Maximal idiomatic fit — beads-the-noun is what every other tool already understands. We pay zero new abstraction for queryability.
2. Cleanest concurrency story — one bead per (rater, target). No row-locking on the rated bead.
3. Backfill, multi-rater, comment-bearing, and label-bearing all come free from the existing bead model.
4. The "but this adds rows" objection is real but small; if it ever bites, we can add a TTL/archive policy on `type=rating` without changing the read surface.

T2 should still ask the operator to confirm — Option B has merit if Matt prefers the rating to feel like a flavored comment rather than a thing-of-its-own.

---

## 6. Three UX options

Each option is sketched as a beads-viewer change. UI lives in `gardenio/controller/cmd/beads-viewer/templates/`; routes in `server.go`/`*.go` siblings. Nav is `templates/layout.html`.

### Option I — Top-level `/ratings` tab + per-bead inline widget

- **Nav.** `templates/layout.html` grows one entry: `<a href="{{.BasePath}}/ratings">Ratings</a>`, peer to Decisions/Advice/Costs.
- **`/ratings` page.** Lists the most recent rating beads (default sort: `updatedAt desc`, limit 200). Filter chips: by rater, by rated-bead type (`mr`/`task`/`agent-run`/`decision`), by score (`≥4` / `≤2`). Each row shows: stars, rated-bead title (linked), rater, ago, comment preview.
- **Detail.** Tapping a rating row is just `bead_show.html?id=<rating-id>` — but `type_views.go` switches `type=rating` to a per-type partial that renders the score widget read-only, the axes block, the rated-bead linked card, and the rationale comment thread.
- **Per-bead inline widget.** On `bead_show.html`, when the viewer is signed in and the bead is rateable (see §6.4), a compact widget renders: 1–5 stars + "open ratings" disclosure → expanded form with axes, tags, comment, submit. Existing ratings on this bead are shown above the widget as a compact "averaged 4.3 ★ across 6 raters · expand" disclosure.
- **Discoverability.** Top tab makes the surface findable. Per-bead widget makes the act of rating zero-click after navigation.
- **Cost.** One new route, one new template, one nav-line edit, one template edit on `bead_show.html`, one POST handler. Mirrors the Costs / Decisions pattern almost exactly.

### Option II — Per-bead inline widget only (no top-level tab)

- **Nav.** Unchanged. No `/ratings` route.
- **Per-bead.** Same widget as Option I. Existing ratings rendered inline.
- **Read surface.** "Recent ratings across all beads" is reachable via `/?type=rating` (the existing index already supports `type=` filtering since the parity-phase2 work). Per-rater leaderboard is reachable only via search.
- **Tradeoffs.**
  - **+** Lowest-cost ship. Almost all delta is on `bead_show.html`.
  - **+** Avoids a half-empty Ratings tab in the early days when there are 3 ratings total.
  - **−** Hides the surface — operators may forget the feature exists if it's not in the nav.
  - **−** Aggregations (top-rated MRs, agent leaderboard) are not first-class. They rely on whatever filtering the generic bead list grows.

### Option III — Ratings as a comment thread on every bead

- **Nav.** Unchanged.
- **Per-bead.** The existing comments partial gains a "Rate this" affordance at the top. Submitting posts a typed comment with `kind=rating`, score, axes; renders inline with normal comments, sorted chronologically; visually distinguished by a star prefix. No separate Ratings tab.
- **Read surface.** "Comments thread including ratings" per bead. No cross-bead aggregation page.
- **Tradeoffs.**
  - **+** Conceptually simple — there is one feedback thread per bead, ratings are just typed comments.
  - **+** Pairs naturally with Option B in the data-model section.
  - **−** Cross-bead views (leaderboard, "top-rated MRs of the week") are not ergonomic; rebuilding them requires the cross-comment query that doesn't exist today.
  - **−** Rating UX gets visually tangled with normal commentary on long threads.

### 6.4 Rateability scope

Independent of which UX option wins, T2 should choose what is rateable:

- **Wide scope: any bead.** Maximally permissive. Ratings on `schedule` beads or `cost-account` beads are weird but allowed. Operator self-restraint is the gate.
- **Curated scope: rateable types only.** A `RateableTypes` set in beads-viewer (initial: `mr`, `agent-run`/gardener, `decision`, `task`, `epic`). Other types render no widget. Easier to defend when training pipelines later read this corpus.

I lean **wide scope**. Adding a type to the rateable set later is free; subtracting is harder once operators have rated. But Option II strongly assumes wide scope (no top-level tab, so the surface *is* the per-bead widget).

### 6.5 Recommendation: **Option I.**

Reasons:
1. Discoverable. A new feedback surface lives or dies on whether operators remember it exists; a nav peer with Decisions/Advice gets eyeballs.
2. Read-side aggregations are first-class — leaderboards, score-by-rated-type, recent-low-ratings all become trivial template work later.
3. Cost is in line with the parity-phase2 budget (one tab, one detail switch). The work is well-scoped.
4. Composes cleanly with the recommended Option A data model — `type=rating` beads are the natural row type for a list page.

T2 should ask the operator to pick between **I (top tab + widget)**, **II (widget only)**, **III (comment thread)**, AND independently to pick **wide vs. curated** rateability scope.

---

## 7. Schema details (recommendation only — T2 may override)

If the operator confirms Option A + Option I, the recommended concrete schema:

```yaml
# kbeads/internal/model/type_config — proposed type
- type: rating
  required_fields: [rated_bead_id, score]
  field_defs:
    - name: rated_bead_id
      type: string
      required: true
    - name: score
      type: int
      min: 1
      max: 5
      required: true
    - name: axes
      type: object
      required: false
      schema:
        correctness: int (1-5, optional)
        scope:       int (1-5, optional)
        efficiency:  int (1-5, optional)
        communication: int (1-5, optional)
    - name: tags
      type: string[]
      required: false
    - name: rater
      type: string
      required: true   # auto-populated server-side from OIDC, not user-editable
```

Plus:
- `Rating` writes are gated to authenticated operators (existing OIDC cookie path). `rater` is set from `oidcauth.EmailFromContext(ctx)` — never trust a client-supplied value.
- A `rates` dependency edge from rating bead → rated bead supports cheap graph queries ("what ratings reference ka-XXX?").
- Free-text rationale rides as a `Comment` on the rating bead, not a field. Reuses the existing comment write path.
- Server enforces idempotence per `(rater, rated_bead_id)`: re-rating updates the existing rating bead. New `Store.UpsertRating(...)` helper or daemon route can wrap this.

---

## 8. Open questions answered (epic checklist)

| Epic question | Answer |
|---|---|
| 1. Unit of rating | Any bead (recommended). Operator may narrow to `mr`/`agent-run`/`decision`/`task`/`epic` in T2 if they want curated scope. |
| 2. Rating schema | 1–5 stars (required) + optional axes (4 sub-scores) + optional tag chips + optional comment. Confirmed by operator in T2. |
| 3. Where data lives | New `rating` bead type (Option A). Confirmed by operator in T2. |
| 4. Who can rate | Authenticated operators (OIDC). Multi-rater allowed (one rating bead per rater per target). Self-rating by agents: **disallowed** at first, revisit in a follow-up epic. |
| 5. Read surface | `/ratings` tab (recent), per-bead inline widget, linkbacks on rated bead detail page (Option I). |
| 6. Backwards-fill | Allowed. Closed beads are rateable. No reopening side-effect. |

Anything not on this list is out of scope for the parent epic and belongs in a follow-up.

---

## 9. The decision T2 must surface

T2 should `gb decision create` with two independent forks (or one decision with all combinations — the operator's preference). Recommended draft:

- **Fork 1 — data model:**
  - **A** — new `rating` bead type *(recommended)*. `artifact_type=plan`.
  - **B** — typed comment subtype. `artifact_type=plan`.
  - **C** — JSONB `ratings[]` on rated bead's fields. `artifact_type=plan`.
- **Fork 2 — UX:**
  - **I** — top-level `/ratings` tab + per-bead widget *(recommended)*. `artifact_type=plan`.
  - **II** — per-bead widget only. `artifact_type=plan`.
  - **III** — ratings-as-comments thread. `artifact_type=plan`.
- **Fork 3 — rateability scope:**
  - **wide** — any bead is rateable *(recommended)*. `artifact_type=plan`.
  - **curated** — fixed allow-list `[mr, agent-run, decision, task, epic]`. `artifact_type=plan`.

Once resolved, T3 writes `gardenio/plans/beads-viewer-human-ratings.md` capturing the chosen combination and the implementation steps, and the parent epic re-points at that plan as plan-of-record.

---

## 10. Out of scope (not for this epic)

- iOS Gardenio ratings parity (the survey above shows there is no current UI to mirror; if Matt wants iOS parity it is a separate epic).
- Training-pipeline integration that *consumes* ratings.
- Public/anonymous ratings.
- Rating-of-agents (rate work output, not personas).
- 3D-graph rating overlays in beads3d.
- Rating-driven quality dashboards (a follow-on epic can read from the surface this delivers).

---

## 11. References

- Parent epic: ka-0000007308 (canonical scope statement).
- Tracking bead: ka-0000004028 (gardener spawn).
- Source thread: Slack 2026-04-25 — Matt Baker, "new tab for beads-viewer for human ratings".
- iOS Gardenio code surveyed: `usr/Gardenio/Gardenio/BeadsTab.swift` (incl. inline `BeadDetailView` at line 299).
- beads-viewer code surveyed: `gardenio/controller/cmd/beads-viewer/{server.go,decisions.go,advice.go,costs.go,type_views.go,templates/layout.html,templates/bead_show.html}`.
- Adjacent doc: `gardenio/docs/beads-viewer-ios-parity-phase2.md` (nav + per-type rendering precedent).
- kbeads model surveyed: `kbeads/internal/model/{bead.go,comment.go,type_config.go,validate_fields.go}`.
