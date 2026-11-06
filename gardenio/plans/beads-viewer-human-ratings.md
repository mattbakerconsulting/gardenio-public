# Beads-Viewer Human Ratings — Plan of Record

**Status:** Draft plan-of-record. T3 of epic [ka-0000007308](../).
**Predecessors:** [ka-0000000547](../) (T1 research), [ka-0000002402](../) (T2 decisions raised).
**Source documents:**
- Research notes: [`gardenio/docs/research/beads-viewer-human-ratings.md`](../docs/research/beads-viewer-human-ratings.md)
- Data-model decision: ka-0000000502
- UX-surface decision: ka-0000001726

---

## 0. Decision input — read this first

T2 (ka-0000002402) raised two decision beads to lock the design fork: **ka-0000000502** (data model) and **ka-0000001726** (UX surface + rateability scope). At the time this plan was drafted, both decision beads were still in `status=open` — the spawn directive instructed the planner to proceed against the T1 recommendations.

This plan therefore documents the T1-recommended design as the **assumed-chosen** path:

| Decision | Chosen option | Source |
|---|---|---|
| ka-0000000502 (data model) | `model-A` — new `type=rating` bead, one bead per (rater, rated-bead), `rates` dependency edge | T1 §5 / §7 recommendation |
| ka-0000001726 (UX surface) | `ux-I-wide` — top-level `/ratings` tab + per-bead inline widget; **any** bead rateable | T1 §6 / §6.4 recommendation |

If the operator picks any other option on either decision, the affected sections of this plan must be updated before implementation begins. The implementation gate (T4) is blocked until both decisions are formally resolved with `gb decision report`.

---

## 1. Scope

In scope:
- New `type=rating` first-class bead in kbeads.
- `gardenio/controller/cmd/beads-viewer/` Go htmx server: top-level `/ratings` page, per-bead rating widget, POST handler, view-model wiring.
- OIDC-gated write path; rater identity server-set from the OIDC email claim.
- Read views: recent-ratings list, per-bead aggregate (mean ± count), and a small leaderboard for `mr` and `agent-run` types.

Out of scope (call-outs in the research doc, deferred):
- iOS Gardenio parity — no existing rating UI in the iOS app (research §3); no backport this round.
- beads3d ratings overlay — separate epic if/when proven useful.
- Rating-driven advice promotion or auto-pinning — interesting, future epic.

---

## 2. Data model — `type=rating` bead

A rating is a first-class bead, not a comment kind and not an inline list. One bead per (rater, rated-bead) pair. Re-rating from the same author is **idempotent**: it updates the existing bead in place rather than creating a second bead — the daemon resolves uniqueness via the `(rater, rated_bead_id)` tuple.

### 2.1 Type config (kbeads)

Registered in `kbeads/internal/model/type_config.go` as a new `TypeConfig`:

```yaml
type: rating
title: required (kept short — "rating: <rated-id> — <rater-handle>" is the convention)
required_fields: [rated_bead_id, score]
field_defs:
  - name: rated_bead_id
    type: string
    required: true
    description: ID of the bead being rated.
  - name: score
    type: integer
    required: true
    min: 1
    max: 5
    description: Overall 1–5 rating.
  - name: axes
    type: json
    required: false
    description: |
      Optional sub-scores. Object with any of:
        correctness, scope, efficiency, communication
      each an integer 1–5. Missing axes are unrated.
  - name: tags
    type: strings
    required: false
    description: Free-form tags for filtering (e.g. "regression", "good-rationale").
  - name: rater
    type: string
    required: true
    description: |
      OIDC email of the rater. Server-set on POST — never trusted from form input.
```

Rationale (from research §5): JSONB-extensible field set lets axes evolve without migrations; we keep the row narrow and rely on `rated_bead_id` + `rater` as the logical key, enforced at the application layer (see §5.3).

### 2.2 Edge: `rates` dependency type

A `rating` bead carries one outgoing dependency of type `rates` to the rated bead:

```
rating-bead --rates--> rated-bead
```

This makes ratings show up in the rated bead's incoming-edge panel "for free" via the existing dependency-fanout in `bead_show.html`. `model.DependencyType` already supports custom labels — no new enum value, just a new string constant `rates` in `internal/model/dependency_types.go` (or wherever existing custom labels live).

### 2.3 Title/body convention

- **Title:** `rating: <short-rated-title-or-id> · <score>★` — used for list-page hooks and search.
- **Description:** the rater's free-text rationale (markdown). Optional but encouraged. Comments on the rating bead are reserved for follow-up dialogue (e.g. an operator asking the rater to clarify).

### 2.4 Why not the alternatives

- **model-B (kind=rating Comment):** Comments are scoped per-bead and do not carry typed fields cleanly; validation, list-views across all ratings, and idempotent re-rate all become ad-hoc.
- **model-C (inline `fields.ratings[]`):** Mutating the rated bead on every rating creates write contention, kills audit history, and forces every rateable type to opt in to a schema change.

Full tradeoffs in research §5.

---

## 3. Routes and templates (beads-viewer)

All routes live under the existing `gardenio/controller/cmd/beads-viewer/` server. New file: `ratings.go` (mirrors `decisions.go`).

### 3.1 New page route

| Method | Path | Handler | Template |
|---|---|---|---|
| GET | `/ratings` | `(*Server).handleRatings` | `templates/pages/ratings.html` |

Optional query params:
- `?project=<name>` — filter by `project:<name>` label (mirrors `/decisions`).
- `?type=<bead-type>` — filter by rated-bead type (server resolves rated bead and filters in-memory; capped result set keeps this cheap).
- `?rater=<email>` — filter by rater.

Rendered card per row: rated-bead title + id (linkified), score (1–5★), rater email, age, rationale snippet (240 chars).

### 3.2 New fragment routes (per-bead widget)

Mounted under `/b/{id}/...` to avoid collision with `/p/{project}` and project-scoped routes:

| Method | Path | Handler | Purpose |
|---|---|---|---|
| GET | `/b/{id}/rating` | `(*Server).handleRatingWidget` | Render the rating widget partial for this bead — used by htmx on tab activation or as part of `bead_show`. |
| POST | `/b/{id}/rating` | `(*Server).handleSubmitRating` | Create-or-update the calling user's rating for this bead. |

The POST handler returns the same widget partial (htmx `outerHTML` swap) so optimistic state, errors, and resolved rendering all flow through one fragment.

### 3.3 Per-type view — rendered on every bead page

`injectTypeView` (`type_views.go`, line ~834) gains a new switch case for the **rating** type — rendering a `ratingView` model that summarizes:
- the rated bead (linkified),
- the score with star glyphs,
- axes table when present,
- tags as chips,
- rater email.

Additionally, the **rated bead's** show page renders a sibling section "Ratings" near the dependency fan-out, populated by walking incoming `rates` edges. Implementation: lift it out of `bead_show.html` as `templates/partials/ratings_summary.html` and call it from any non-rating bead page.

### 3.4 Nav

Add a `/ratings` link to the top nav in `templates/layout.html` (lines 926–948), positioned between **Decisions** and **Advice**.

### 3.5 Rateability scope

T1 §6.4 picks **wide**: any bead is rateable. Server hardening:
- POST handler refuses if `rated_bead_id` does not exist or is itself a `rating` bead (no rating-of-rating loop).
- Self-rating is allowed: a rater may rate a bead they own, are assigned to, or originally created. (Earlier drafts gated this; the daemon does not.)

---

## 4. View models (`type_views.go`)

New struct alongside `decisionView` / `scheduleView` etc.:

```go
type ratingView struct {
    BeadID        string        // the rating bead
    RatedBeadID   string
    RatedTitle    string
    Score         int
    AxisRows      []ratingAxis  // pre-zipped name → value, only present axes
    Tags          []string
    Rater         string
    RationaleHTML template.HTML // rendered from the rating bead's description
    CreatedAgo    string
}

type ratingAxis struct {
    Name  string // correctness, scope, efficiency, communication
    Score int    // 1..5
}
```

Builder `buildRatingView(bead, renderMarkdown)` mirrors `buildDecisionView` — pure function over `bead.Fields`, no daemon I/O. The rated-bead title is fetched at the page level (one extra `GetBead` per rating on a list page; bounded by `ratingsListLimit`).

For the per-bead widget partial, an additional `ratingWidgetView` carries:
- `MyRating *ratingView` — current user's rating, nil if absent.
- `Aggregate ratingAggregate` — `Mean float64`, `Count int`, `Histogram [5]int`.
- `CanRate bool` — false when unauthenticated, or when bead is `type=rating`.
- `Error string`, `ScoreDraft int`, `RationaleDraft string` — preserved on validation failure.

---

## 5. Write path — `handleSubmitRating`

Mirrors `handleResolveDecision` in `decisions.go`. POST flow:

### 5.1 Validation

1. `r.PathValue("id")` → `rated_bead_id`. 400 if empty.
2. `oidcauth.EmailFromRequest(r)` → rater email. 401 if empty.
3. `r.ParseForm()` → 400 on failure.
4. `score` field present, parses as int in `[1, 5]`. Else inline error in widget.
5. Optional `axes_<name>` form fields — each must be `[1,5]` if set.
6. Optional `tags` (comma-separated).
7. Optional `rationale` (≤ 10_000 chars; same cap as `maxDecisionResponseLen`).

### 5.2 Authorization gates

8. `GetBead(rated_bead_id)` — 404 if missing.
9. Reject if rated bead's type is `rating` (no rate-of-rating).

### 5.3 Idempotent upsert

11. Look up existing rating: `ListBeadsFiltered(Types: ["rating"], FieldEquals: {"rated_bead_id": id, "rater": email}, Limit: 1)`.
    - If kbeads' filter API doesn't support generic field-equality yet, this becomes a small spike under T4 (see §10): either add `FieldEquals` to `ListBeadsFiltered`, or add a typed daemon endpoint `GET /v1/ratings/by-rater?bead=<id>&rater=<email>`. The plan-of-record assumes the daemon-side API exists or is added in T4 step 1.
12. If found: `UpdateBead` with new score/axes/tags/rationale. Bumps `updated_at`; existing dependency edge stays.
13. If not found: `CreateBead(type="rating", title=..., description=rationale, fields={...})`, then `AddDependency(rating_id, rated_bead_id, "rates")`.
14. On success: re-fetch the widget aggregate and render the partial as a 200 response.

### 5.4 Errors

All errors render the widget partial inline (preserving draft state), with status:
- 400 — bad form / invalid score
- 401 — unauthenticated (defense in depth; oauth2-proxy should already block)
- 404 — rated bead missing
- 502 — daemon write failed

---

## 6. Read views

### 6.1 `/ratings` list page

Top nav peer. Server query:

```go
beadsapi.ListBeadsQuery{
    Types:    []string{"rating"},
    Sort:     "-created_at",
    Limit:    200,                 // ratingsListLimit
}
```

Filters (project / type / rater) applied as label/field predicates server-side where supported, otherwise filtered post-fetch within the limit. Truncation banner mirrors `/decisions` when `Total > Limit`.

### 6.2 Per-bead "Ratings" panel

Rendered on any non-rating bead's show page. Pulls incoming `rates` edges, builds aggregate (mean, count, histogram). Hidden when count == 0 and viewer is unauthenticated (avoid drawing attention to empty panels for read-only users).

### 6.3 Aggregate-by-type leaderboard

Two small sections on `/ratings`, surfaced behind `?leaderboard=mr` / `?leaderboard=agent-run`: top 10 rated beads of that type by mean score where count ≥ 3. Implemented as in-memory aggregation over a single `Types=[rating]` fetch (capped at 200) — sufficient for early-stage volume; promoted to a daemon-side aggregate query if the cap starts truncating.

### 6.4 Search

`/search` already indexes title + description + fields, so ratings are searchable out of the box. No work needed.

---

## 7. Auth posture

| Action | Gate |
|---|---|
| GET `/ratings` | None (public read, mirrors `/decisions` posture). |
| GET `/b/{id}/rating` | None — anonymous viewers see the aggregate but not the "rate this" form. |
| POST `/b/{id}/rating` | OIDC required (`oidcauth.EmailFromRequest`); falls through to 401 if email empty. |

The rater field is **server-set** from the OIDC email claim; the form is not allowed to override it. This is the single most important security invariant of the feature — call it out in the handler comments.

---

## 8. Telemetry

Three structured log lines (existing slog logger pattern in `decisions.go`):

| Event | Level | Fields |
|---|---|---|
| `rating.submit` | info | `bead`, `rater`, `score`, `axes_count`, `update` (true on idempotent re-rate) |
| `rating.reject` | info | `bead`, `rater`, `reason` (one of: bad_score, rated_missing, rate_of_rating) |
| `rating.error` | error | `bead`, `rater`, `error` (daemon failure path) |

No metrics export in the first cut — the existing prom histograms on the daemon's HTTP layer cover request volume; product-side aggregates are read off the bead store directly.

---

## 9. Rollout

Phased to keep the operator gate in place at every step.

1. **kbeads** — register `type=rating`, add `rates` dependency-type constant, add type-config validation tests. No HTTP-API change beyond shipping the new type. Behind a default-on env flag `KBEADS_ENABLE_TYPE_RATING=1` for cheap rollback.
2. **beads-viewer** — `ratings.go` handlers, page template, widget partial, view models, nav link. No DB writes possible until kbeads is deployed.
3. **Auth check pass** — manual: confirm anonymous viewer cannot POST; confirm rater email cannot be spoofed via form.
4. **Backfill (optional)** — operator-driven: a one-shot CLI `kd rating create --bead <id> --score N --rationale '...'` for retroactive ratings (research §8 allows this). Out of scope for this plan; covered by a follow-up bead.
5. **Announcement** — `gb squawk` to the dev channel with the `/ratings` URL once both kbeads and beads-viewer are live in gardenio-rwx.

Rollback: drop the nav link + 404 the `/ratings` route in beads-viewer (one commit). The kbeads type registration is harmless at rest — leaving rating beads in the store does not affect any other bead-type view.

---

## 10. Implementation slicing for T4–T7

Concrete child-task sketch for the implementation epic. Each task ships its own MR.

| Task | What | Repos touched |
|---|---|---|
| T4-a | Register `type=rating` + `rates` dep-type in kbeads. Type-config + validation tests. | kbeads |
| T4-b | Add `FieldEquals` predicate (or `/v1/ratings/by-rater` endpoint) to support idempotent upsert. | kbeads |
| T4-c | beads-viewer: `ratings.go` handlers, view models, page + widget templates, `injectTypeView` case. | gardenio |
| T4-d | Per-bead "Ratings" panel partial + wiring in `bead_show.html`. | gardenio |
| T4-e | Tests: handler unit tests (decisions.go-style table tests), Playwright smoke for `/ratings` and submit flow. | gardenio, beads3d (no — Playwright lives in gardenio for beads-viewer; see `gardenio/docs/beads-viewer-dev.md`) |
| T4-f | E2E: rate a bead in gardenio-rwx, confirm aggregate render and idempotent re-rate. | n/a (manual + smoke) |

T5–T7 reserved for: leaderboard, optional backfill CLI, advice-promotion experiment.

---

## 11. Open questions (forwarded, not blocking the plan)

- **Decision-bead resolution.** ka-0000000502 and ka-0000001726 must be formally resolved by the operator via `gb decision report` before T4 starts. This plan is the artifact those decisions point at.
- **`FieldEquals` shape.** T4-b needs a concrete daemon-side API for `(rater, rated_bead_id)` lookup; the existing `ListBeadsQuery` does not expose generic field equality. Design fork to be raised in T4 if it isn't trivial.

---

## 12. References

- Research notes: [`gardenio/docs/research/beads-viewer-human-ratings.md`](../docs/research/beads-viewer-human-ratings.md)
- Existing patterns mirrored:
  - `gardenio/controller/cmd/beads-viewer/decisions.go` — list page + POST handler shape
  - `gardenio/controller/cmd/beads-viewer/type_views.go` — per-type view-model dispatch
  - `gardenio/controller/cmd/beads-viewer/templates/layout.html` — nav placement, six-skin compatibility
- Bead model: `kbeads/internal/model/type_config.go` — `TypeConfig`, `FieldDef`, validation
