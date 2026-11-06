# V-R1: Inventory — which beads-viewer surfaces should default-filter `operated_by=me`

**Status:** research only — no implementation.
**Parent epic:** ka-0000002754 (research-only).
**Sibling docs:** R2 (UX/override semantics), R3 (server propagation/caching), R4 (migration sunset).
**Related prior work:**

- `docs/research/human-attribution-ux.md` (R8) — chip rendering states; `me` server-side alias semantics; "my beads" hero link.
- `gardenio/controller/cmd/beads-viewer/operator_chip.go` — `resolveOperatedByParam` already maps `me` / `null` / `(none)`.
- `gardenio/controller/cmd/beads-viewer/active.go::handleActive` and `search.go::handleSearch` — only two surfaces that read `?operated_by=` today.

## Why this exists

Matt asked (Slack, 2026-05-03):
> "make the operated by filter the default for a beads viewer user (all beads created so far are by me for migration purposes)"

Before we can decide *how* to default the filter (R2) or *where* to thread the default through the request path (R3), we need to enumerate every viewer surface and decide whether the default should apply to each one. This doc is that inventory.

## Methodology

Walked the route table at `gardenio/controller/cmd/beads-viewer/server.go` (registerRoutes around line 410 onwards). For each top-level GET that renders a list / page (not an htmx fragment for an already-authorized bead), I asked four questions:

1. **Does the daemon call accept `OperatedBy`/`OperatedByIsNull`?** (i.e. is filtering even possible at this layer?)
2. **Is the page list-shaped (filterable) or detail/structural?**
3. **What is the user's expectation when they land cold?** (Daily-driver attention queue → "my work" wins. Lookup / structural / global → unfiltered wins.)
4. **What is the cheapest escape hatch back to the unfiltered view?**

Out of scope here: htmx partials hung off `/b/{id}/...`, POST mutators, `/healthz`/`/readyz`, static assets. The `?project=`, `?status=`, `?type=` query-string compositions are R3 territory — this doc records only whether the surface should default-on, not the exact composition rule.

## The current state

Only `/active` and `/search` consult `resolveOperatedByParam` today (`active.go:78`, `search.go:54`). Both default to *no filter* unless the user explicitly passes `?operated_by=`. Every other surface currently ignores `operated_by` entirely — either because the daemon list call doesn't pass it through, or because the page is structurally global (e.g. `/health` tiles, `/projects` index).

`MyWorkSummaryMode` (active.go:84) is already wired as a template flag set when `?operated_by=me` resolves to the viewer. R8 §5.4 calls this out explicitly. So `/active` is the closest existing analogue of "default-on" — the plumbing is there, just not the cold-load default.

## The inventory table

Surfaces are grouped by category. **Default behavior** is the proposal *assuming Phase 1* (post-dibs/transitivity, when the corpus is no longer ~100% Matt). Phase 0 caveats are folded into R4. **Escape hatch** is the URL-only override discussed in R2; persistence/cookies are out of scope here.

### Daily-driver / list views

| Surface | Default behavior | Rationale | Escape hatch |
|---|---|---|---|
| `/active` (`handleActive`) | **default-on** (`operated_by=me`) | Daily-driver attention queue. The whole point of the page is "what is happening that I should care about right now" — the unfiltered firehose dilutes that signal. The `MyWorkSummaryMode` template branch already exists, so the rendering is ready. | URL: `?operated_by=` (empty = "all"); `?operated_by=null` ("unowned"); `?operated_by=<id>` ("that person"). Header chip in R2 must surface the active default and offer a one-click "show all". |
| `/search` (`handleSearch`) | **default-off** (no filter) | Search is a lookup tool. A user searching `q="canonical-prod"` expects global recall, not a silently-narrowed result. Default-on here would cause "I know this bead exists but search can't find it" surprise. The R8 chip already provides a `[☐ me]` toggle on the form (R8 §6 form sketch), which is the right opt-in path. | Form checkbox `[☐ me]` (R8 §6) sets `?operated_by=me`. URL override identical to `/active`. |
| `/active?project=<name>` | **default-on** (inherits `/active`) | Project-scoped Active is still the daily-driver lens, just narrowed. AND-composing `project` and `operated_by=me` is the expected reading. | Same as `/active`. |
| `/decisions` (`handleDecisions`) | **page-specific: default-off**, but expose `[☐ me]` chip | Decision queue is short and operator-attention-dense; today the daemon call does not even pass `OperatedBy`. Most decisions are *for* a human to act on regardless of who *requested* them. Defaulting-on would hide the very items that the page exists to surface. Worth offering a "decisions I requested" toggle, but not as the default. | URL `?operated_by=me` once R3 plumbs it through `handleDecisions`. |
| `/ratings` (`handleRatings`) | **default-off** | Page already supports `?rater=`, which is the *more correct* "my ratings" filter (rater identity is recorded separately). `operated_by` here would conflate "I rated this" with "I operated the bead I rated", which are semantically different. | Use `?rater=<email>` (existing); no `operated_by` plumbing required. |
| `/agents` (`handleAgents`) | **default-off** | `/agents` is the fleet-wide live-state dashboard. Operators need to see other people's pods to spot stuck/stale ones — that's the page's whole job (cf. `gb news`). Defaulting to "my agents only" defeats the point. | `?project=<name>` (existing) is the typical narrowing; an `?operated_by=me` chip is fine to add but never as default. |
| `/advice` (`handleAdvice`) | **default-off** | Advice beads are a knowledge base; almost everything is `global` or `project:<x>`. Authorship by `operated_by` is incidental. The page's existing `?scope=` (all\|global\|project:\|role:\|agent:) is the right primary axis. | `?scope=` is the canonical narrowing; do not add `operated_by` default here. |
| `/schedules` (`handleSchedules`) | **default-off** | Schedule beads are infrastructure (cron jobs). They are typically not human-operated at all; their `created_by` is `sched-…` and `operated_by` is empty (the "scheduler" chip Kind in `operator_chip.go:42`). A `me` filter would hide every row. | n/a — keep page global. |
| `/costs` (`handleCosts`) | **default-off** | Cost dashboards are aggregate / infra. `me` is meaningless at this layer. | n/a. |
| `/health` and `/health/tile/*` (`handleHealth*`) | **default-off** | Operations dashboard. Tiles intentionally aggregate across the fleet (open beads, gardeners, bridges, incidents). The whole point is system-wide visibility. | n/a. |
| `/views` index (`handleViewsList`) | **default-off** for the *index*, but already sorts owned-by-me first (`OwnedByMe` in `views.go:229`). | The index is browsing all saved views (mine first, others below). Defaulting to "only mine" hides shared team views, which is part of why saved views exist. | The existing top-of-list ordering plus an `?owner=me` chip would be more discoverable than auto-filtering. |
| `/views/{id}` (`handleViewShow`) | **default-off — must respect the saved spec verbatim** | Saved views are user-authored queries; injecting an implicit `operated_by=me` would silently mutate someone else's saved query. If a view's author wants self-scope, they save `operated_by=me` *into the spec* (`savedViewSpec.toQuery`, `views.go:54` — would need a new field). The R3 default-injection logic must skip this code path. | View author edits their own spec. R2 should mention this exception explicitly. |
| `/p/{project}` (`handleProject`) | **default-off** | Project view is structural — it shows the bead-graph of a project, not the viewer's slice of it. The chip strip already segments by `?type=`; `operated_by=me` overlays would fight with that mental model. Adding an opt-in chip is fine, but default-on would be a regression for the "show me everything in the canonical-prod epic" use case. | Add `?operated_by=me` chip to the strip in a follow-on; URL override always available. |
| `/` index (`handleIndex`) | **default-off** | Landing page lists projects + global stats. No bead-list to filter. | n/a. |

### Bead-detail / mutator surfaces

| Surface | Default behavior | Rationale |
|---|---|---|
| `/{id}` (`handleBeadShow`) | **n/a — single bead** | Detail page. No list filter applies. Operator chip on the bead itself is rendered via `beadOperatorChip` (R8). |
| `/b/{id}/activity` (`handleBeadActivity`) | **n/a — single-bead history** | Shows events/comments for one bead. The `operated_by` value is a per-event annotation, not a list filter. |
| `/b/{id}/comments`, `/b/{id}/deps`, `/b/{id}/preview`, `/b/{id}/rating`, all `/b/{id}/*/edit` and POST handlers | **n/a — bead-scoped fragments** | Same reasoning. No list filter to default. |
| `/agents/{id}/prime`, `/agents/{id}/screen`, `/agents/{id}/events`, all POST `/agents/{id}/*` | **n/a — agent-scoped** | Single-agent fragments. |
| `/cmdk/suggest` | **default-off** | Type-ahead autocomplete; needs global recall, same logic as `/search`. |
| `/quick-spawn` (`handleQuickSpawn*`) | **n/a — form** | Spawn a new agent against a bead; not a list view. |
| `/settings`, `/settings/edit`, `/settings/view` | **n/a — per-user prefs** | The settings page is *about* the viewer; it is implicitly self-scoped. |
| `/login`, `/healthz`, `/readyz`, `/assets/static/*` | **n/a — infra** | No filtering at all. |

### Surfaces that don't currently exist but were called out in the task

- **`/projects`** — there is no `/projects` route today. `/{$}` (`handleIndex`) is the closest analogue and lists projects. If a future `/projects` index emerges, follow `/active` for project-scoped variants and follow `handleIndex` for the global list (default-off).
- **Activity / timeline** — there is no global `/activity` page; only per-bead `/b/{id}/activity`. If a future global activity feed lands, treat it like `/active` (default-on, daily driver) but verify with a per-page explicit decision — recency feeds tend to want global recall.
- **Jacks** — no `/jacks` route today. If/when added, default-off until the page's purpose is clarified.

## Summary of defaults

- **Default-on (one and only one surface):** `/active` (and its `?project=` variant). This is the daily-driver page and the most-requested surface in Matt's note.
- **Default-off but offer an explicit `me` toggle:** `/search`, `/decisions`, `/p/{project}`. Each gets a chip / checkbox in R2's UX layer; URL override is the contract.
- **Default-off and don't even add a `me` toggle:** `/agents`, `/advice`, `/schedules`, `/costs`, `/health*`, `/views`, `/views/{id}`, `/`, `/cmdk/suggest`, `/ratings` (which has `?rater=` instead).
- **N/A:** every `/b/{id}/...`, `/agents/{id}/...`, `/login`, `/settings*`, `/healthz`, `/readyz`, `/assets/static/*`.

## Open questions surfaced (forwarded to R2 / R3 / R4)

These are *not* decided here — they exist so the next epic in the chain owns them.

1. **Saved-view exception (R2 + R3).** When a default-on surface is reached *via* a saved view spec, the spec wins. R3 must thread the default-injection step *outside* `savedViewSpec.toQuery`. Today there is no `OperatedBy` field on `savedViewSpec` — adding one is an R3 follow-on.
2. **`/active?project=<name>` composition (R3).** Is the default applied before or after `?project=` is parsed? Either order works; needs to be written down.
3. **Decisions chip vs. default (R2).** "Decisions I requested" is a real workflow but not the page's primary axis. R2 should weigh whether the chip lives on `/decisions` itself or under a future `/active?type=decision` view.
4. **Phase-0 visibility (R4).** Until the corpus diversifies, `/active` default-on for non-Matt OIDC users will look near-empty. Whether to gate the default on a cardinality threshold or ship with the migration banner is R4's call.
5. **`?operated_by=` empty-string semantics (R2).** `resolveOperatedByParam` returns `("", false)` for empty *and* for `null` (with the caller checking the literal string for the IS-NULL branch). R2 must lock the contract: empty = "all", `null` = "unowned", `me` = viewer.
6. **`/views` injection skip (R3).** `handleViewShow` calls `s.daemon.ListBeadsFiltered(r.Context(), spec.toQuery())` — R3's default-injection layer must be a wrapper applied at the *handler* level (active, search, …) and **not** at the daemon-client layer, so saved views remain spec-faithful.

## Links

- Bead: ka-0000000145 (this task)
- Parent epic: ka-0000002754
- Decision checkpoint: filed by epic owner after R1–R4 land.
- R8 prior art: `docs/research/human-attribution-ux.md` (note: lives at repo root `docs/`, not `gardenio/docs/`)
- Code touchpoints: `gardenio/controller/cmd/beads-viewer/active.go`, `search.go`, `operator_chip.go`, `views.go`, `server.go`.
