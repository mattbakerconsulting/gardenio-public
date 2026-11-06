# Project view with epic sequencer (open green / closed red)

Tracking epic: **ka-0000003558** (ON-ICE — do not start implementation; this
doc is research + scoping only).
Sibling epics in same Slack batch:
- ka-0000005836 — sequencer ↔ report linking + decision-block visibility.
- ka-0000008153 — agent artifact bundles + multi-document viewer.

Source ask: Matt Baker, 2026-05-04 Slack thread (idea frozen on the epic).
Operator follow-up captured on the epic comment:
> (1) sequencer must allow ONE primary item to be selected; when selected,
> its deps and related beads fan out from it. (2) fan-out depth must be
> controllable by bead type and status.

## What "sequencer" means here

There is **no existing `/sequencer` route** in beads-viewer today. The
closest surfaces in `gardenio/controller/cmd/beads-viewer/` are:

| Surface | File | What it does |
|---|---|---|
| Project view `/p/{project}` | `server.go:handleProject` + `templates/project.html` | Per-project bead list with type-chip strip, status/sort dropdowns, bulk-action bar. Pure list — no spatial layout. |
| Constellation (bead-show) | `constellation.go` + `templates/partials/constellation.html` | SVG orbital diagram for ONE bead: similar beads orbit right; forward deps above; reverse deps below; transitive ring at level 2..3. Already has fan-out depth slider (`TreeLevel`), already filters closed agent/session beads, already renders typed-mention edges. |
| Beadsberg ticker (planned) | `gardenio/plans/beadsberg-ticker-view.md` | Two-pane density firehose ("Bloomberg terminal for beads"). Different shape — temporal stream, not spatial layout. |

The epic asks for a **new top-down project surface** that arranges epics
along an axis and conveys per-epic status at a glance via color. The
operator follow-up adds a single-primary-select fan-out — which is exactly
the constellation pattern, applied at the project level. The natural
implementation strategy is therefore:

> A new beads-viewer route (working name **`/p/{project}/sequencer`**)
> that renders epics as a horizontal/grid lane with status colors, and
> when an epic is clicked, fans out into a constellation-style SVG of
> that epic's deps + related beads, with depth controlled by type/status
> filters.

Not a mode of the existing project list — the spatial layout, the
single-primary-select interaction, and the fan-out SVG are all distinct
enough from the list view that grafting them into `project.html` would
be more painful than a new template. **Recommendation: new view.**

## Design questions and proposed answers

The doc is opinionated where the right answer is clear. Where the call
needs operator input, the question is flagged **OPEN** and the candidate
options are listed.

### 1. What does "closed" mean for an epic?

**Proposed: an epic is "closed" iff `status == "closed"`.** "Non-open"
(closed + cancelled + duplicate, etc.) collapses too many distinct
end-states into red; a duplicate or cancelled epic should not look the
same as a delivered one.

**Status palette (proposed):**

| Status | Color | Notes |
|---|---|---|
| `open` (no children, or all children open) | green | "in flight" |
| `in_progress` | green (brighter / pulsing) | actively claimed |
| `closed` | red | delivered. Not a "bad" red — paint as a calm crimson. |
| `cancelled` / `duplicate` | grey | de-emphasized, deprioritized in sort |
| Mixed-status (see §2) | gradient | see below |

This matches the epic title literally ("open green / closed red") while
keeping room for the mixed-status case operators care about.

### 2. Color scheme for partially-done epics

Most epics-of-interest are **open epics with a mix of open and closed
children**. Painting them flat-green hides progress; flat-red lies. Two
candidate schemes:

**Option A — Two-tone fill bar (recommended).** Render each epic as a
horizontal pill. Fill the pill green from the left in proportion to
`closed_children / total_children`, the rest red(open) or grey(other).
Numerically, it's a `<linearGradient>` with a hard stop. Operators get
"this epic is 70% done" at a glance.

**Option B — Hue interpolation.** Single-colored pill that fades from
green (0% done) to red (100% done). Loses crispness; harder to read for
colorblind users. Reject.

**Option C — Discrete buckets.** {empty / 0-25% / 25-75% / 75-100% / done}
with named CSS classes. Less informative than the two-tone bar but
cheaper to render and a11y-friendly. Acceptable fallback.

**Recommendation: Option A** with a tooltip showing the fraction. Provide
Option C as a graceful degradation when CSS gradients are disabled (high
contrast mode).

**Open question OPEN-1:** does "closed child" count direct children only,
or transitive descendants? Direct children is simpler and matches what
operators actually look at on a per-epic basis. Recommend direct
children; revisit if the trees turn out to be deeper than expected.

### 3. Sort order

Three axes are useful, none is universally right:

- **Created date.** Stable. Easy to scan chronologically. Doesn't
  surface urgency.
- **Priority (P0 → P4).** Surfaces what matters; ties are common.
- **Deadline / due date.** kbeads has `due` field but it's sparsely
  populated. Not a viable default.
- **Activity (most-recently-updated).** Highlights live work; demotes
  paused-but-important epics.

**Recommendation: default to `(priority asc, created_at desc)`** (high
priority first, then most recent within a priority bucket). Expose a
`?sort=` query param matching the existing project page (`priority`,
`created_at`, `updated_at`, plus a new `progress` mapped to the
closed/total ratio for "what's almost done first").

### 4. New view vs mode of existing view

**Recommendation: NEW view at `/p/{project}/sequencer`.** Reasons:

- The list view is highly optimized for filter-and-bulk-act; the
  sequencer is optimized for at-a-glance status + drill-into-one. They
  reward different rendering strategies.
- Single-primary-select fan-out (§5) requires a per-epic constellation-
  style SVG that has no analogue in the list view.
- A new template lets us keep the URL stable for bookmarking the
  sequencer mode without leaking sequencer-only query params back into
  the list.

A "Sequencer" tab can live alongside the existing chip strip on
`/p/{project}` so the surfaces feel adjacent, not orphan.

### 5. Single-primary-select fan-out + depth control

This is the operator follow-up. Two collapsing levels of UI:

**Default render** (no primary selected): grid/strip of epic pills
sorted per §3, each colored per §2. Clicking an epic does a soft swap
into the selected state.

**Selected render** (one primary): the chosen epic moves to a focused
position (top-left, or a dedicated "focus card" column), and a
constellation-style SVG fans out alongside it, populated with that
epic's:
- Forward deps (what this epic blocks on / depends on).
- Reverse deps (children, mention-children).
- Typed-mention edges (the existing `ka-0000005941` work renders these
  in constellation already — reuse).

**Depth control by bead type + status** (operator's explicit ask):

| Control | Affects | Default |
|---|---|---|
| **Depth slider** (1–3) | How many tree levels of forward/reverse deps to render. Reuse `constellationMaxTreeLevel = 3` from `constellation.go`. | 1 |
| **Type checklist** | Which child types to fan out to (`task`, `epic`, `bug`, `decision`, `report`, `bundle`, `agent`, `schedule`, …). | `task,epic,bug,decision`; hide `agent`/`session` by default (existing `ShowClosedAgents` toggle). |
| **Status checklist** | Which child statuses to keep. | `open,in_progress`; toggle to include `closed`. |

Implementation hint: the existing `constellationView` and
`constellationFanoutPerParent` already encode "level 2/3 fanout cap per
parent". The new sequencer can lean on `constellation.go` and pass an
extra `(types, statuses)` filter into the hydration step rather than
inventing a new SVG renderer.

**Recommendation: lift constellation rendering into a shared helper**
so bead-show and the sequencer share the same SVG/legend/layout code.
Today `constellation.go` is hard-bound to the bead-show handler; a
small refactor (`buildConstellationView(rootBead, opts)`) lets the
sequencer call it directly with `opts.Types`, `opts.Statuses`,
`opts.TreeLevel`. This is the largest implementation cost in the epic
and is what makes the operator follow-up cheap to satisfy. **Filed as
child task §AC-3.**

**Open question OPEN-2:** when no primary is selected, should the grid
still show a low-resolution preview of "this epic has 3 open decisions
and 1 ready report"? Two options:
- **Pure colored pills.** Cheaper to render, less information. Ship this
  for v1.
- **Decorated pills** with corner badges (link to §6). More information,
  more code. Defer to v2 or fold into ka-0000005836 badge work.

Recommend v1 = pure pills; pick up decorated pills only after the badge
shape lands in ka-0000005836 so the sequencer reuses the same primitive.

### 6. Integration with sibling epics

#### ka-0000005836 (sequencer ↔ report linking + decision-block visibility)

The sibling epic owns the **badge primitive** ("this bead has a report
ready" / "this bead is blocked on a decision") and the corresponding
list filters. The sequencer will consume those badges:

- **Default render:** epic pills get a small corner glyph if any
  descendant bead matches `has_report` or `blocked_on_decision`.
- **Selected render:** constellation nodes (children, deps) get the
  same badge in their orbit position.

**Coordination contract:** the sibling epic must publish a stable
**predicate** for each badge (likely a server-side helper of the form
`func beadHasReport(b Bead) bool` and `func beadBlockedOnDecision(b Bead) bool`,
or a SQL view). The sequencer consumes those predicates and must not
re-implement the detection logic. Filed as a cross-epic acceptance
criterion below (AC-EXT-1).

Mention: see ka-0000005836 scoping task ka-0000005458 (currently
in-progress) for the canonical predicate decision.

#### ka-0000008153 (agent artifact bundles + multi-document viewer)

The sibling epic owns the **bundle target** — the URL an operator clicks
to read everything an agent produced for an epic. The sequencer
consumes that:

- **Selected render:** the focused epic's focus-card includes a "Read
  bundle" link if the epic (or any descendant) has a bundle attached.
- **Constellation:** bundle beads (whatever the eventual bead type /
  representation) render as a distinct node shape, click → bundle
  viewer route from ka-0000008153.

**Coordination contract:** ka-0000008153 must define how a bundle is
discovered from an epic (parent-child, typed mention, label). The
sequencer queries that and renders. Mention: see ka-0000008153 scoping
task ka-0000003253 (currently in-progress).

### 7. URL shape and filter parameters

```
GET /p/{project}/sequencer
    ?primary=ka-XXX           # selected primary epic, optional
    &depth=N                  # 1..3, default 1
    &types=task,epic,decision # CSV; default open-types-with-bias
    &statuses=open,closed     # CSV; default "open,in_progress"
    &sort=priority|created_at|updated_at|progress
    &include_closed_epics=0|1 # default 0
```

URL design notes:
- Mirror the existing `/p/{project}` query param style for consistency.
- `primary` is a deep-linkable handle; pasting a sequencer URL into
  Slack should reproduce the exact view.
- HTMX patterns: clicking an epic does an `hx-get` with `?primary=…`
  and `hx-push-url=true`, swapping the focus card and constellation
  in-place.

### 8. Performance and limits

The existing constellation hydrates up to ~30–40 nodes (forward + reverse
+ outer + similar) with a 600ms timeout, 6 workers, 3× hydrate
multiplier (see `constellationHydrateMultiplier` in `constellation.go`).
The sequencer reuses this. The unselected grid is cheap: one
`ListBeadsFiltered(type=epic, project=…, limit=…)` call plus one
aggregate "closed children count per epic" query.

**Cap proposal:** at most 100 epics on the unselected grid (matches
`projectListLimit` in `views.go`). Beyond that, a `?page=` cursor or a
"refine the project label" banner.

The closed-children count is the only new aggregate. Two implementation
options:

- **A. SQL.** Add a daemon endpoint
  `GET /v1/beads/aggregate-children-by-status?parents=kd-…,kd-…`
  returning `{ka-XXX: {open: 3, closed: 7, in_progress: 1}}`. One round
  trip per page render. **Recommended.**
- **B. N hydrate calls.** Reuse existing per-bead lookup — N+1 problem
  at scale. Reject for the unselected grid. Acceptable for the
  selected/expanded view since it's bounded by depth.

**OPEN-3:** the daemon currently exposes parent-child fan-in via
`kd dep list` style endpoints; check whether a single aggregate exists
or needs a new daemon endpoint. Filed as a scoping spike under AC-2.

### 9. Accessibility

- Color is informative, not load-bearing — pair color with text label
  ("12/15 closed", "0/4 closed"). Existing a11y baseline doc:
  `gardenio/docs/beads-viewer-a11y-baseline.md`.
- Single-primary-select must be keyboard-navigable (focus ring on each
  pill, Enter to select, Escape to deselect).
- Constellation SVG uses `<title>` per node already; keep that.

### 10. Out of scope (explicitly)

- Real-time updates / SSE flicker on the sequencer. Static page render
  with manual refresh is enough for v1; live update is a follow-up
  bead.
- Cross-project sequencer ("all projects at once"). Project-scoped only.
- Mobile / phone layout. Desktop-first; rely on existing breakpoint
  CSS.
- Editing operations (drag-to-reorder, set priority from the
  sequencer). Read-only at first; edits stay on the bead-show page.

## Acceptance criteria — child task breakdown

Each AC below is a single-purpose child task that should be filed
**parented to ka-0000003558**, **labeled `on-ice`**, and at
**priority=4**. They are NOT to be picked up by the gardener until the
operator unfreezes the parent epic.

### AC-1 — Refactor constellation into a reusable builder

Extract `gardenio/controller/cmd/beads-viewer/constellation.go` such
that `buildConstellationView(rootBead, opts ConstellationOpts)` is
callable from both the bead-show handler and a future sequencer
handler. `opts` adds `Types []string`, `Statuses []string`,
`TreeLevel int`, `ShowClosedAgents bool` — most of which already exist
in some form. **Acceptance:** existing constellation tests still pass;
new public helper has its own unit test against a fake `beadsapi`.

### AC-2 — Daemon endpoint: aggregate children by status

Add an endpoint to kbeads that, given N parent IDs, returns a
per-parent histogram of child statuses. Used by AC-4 to color the
sequencer grid in O(1) round trips. **Acceptance:** endpoint covered
by Go-level test; CLI affordance optional. Single page-render call
covers up to 100 parents.

### AC-3 — Sequencer route + template (default/unselected render)

`GET /p/{project}/sequencer` route in beads-viewer. Lists epics for the
project as colored pills (per §2 Option A), sorted per §3.
**Acceptance:** template + handler + Go test; renders `<= 100 epics;
truncation banner above. No primary-selected logic yet (AC-5).

### AC-4 — Two-tone progress fill on epic pills

CSS + template support for the two-tone progress bar. Driven by AC-2's
aggregate. **Acceptance:** visual regression test (Playwright snapshot
under `gardenio/controller/cmd/beads-viewer/templates/static/...` test
harness, or screenshot diff if no snapshot harness exists yet) for
0%, 50%, 100%, mixed-with-cancelled.

### AC-5 — Primary-select interaction + URL params

Wire `?primary=ka-XXX` into the sequencer handler; render a focus card
+ embedded constellation SVG (via AC-1) for the selected epic. HTMX
deep-link; `?primary=…` survives reload. **Acceptance:** Go test for
handler; Playwright (or equivalent) keyboard test for tab/Enter/Escape.

### AC-6 — Type and status fan-out filters

`?types=…&statuses=…&depth=…` query params reach into the constellation
opts (AC-1). Default `types=task,epic,decision`, `statuses=open,in_progress`.
**Acceptance:** Go test asserts the constellation render shrinks/grows
appropriately when params change.

### AC-7 — Badge consumption from ka-0000005836

Once ka-0000005836 publishes the `has_report` / `blocked_on_decision`
predicates (or query API), wire them into the sequencer focus card and
constellation nodes. **Blocked-by:** ka-0000005836 must land its
predicate primitive first. **Acceptance:** badge appears on selected
epic + descendants; toggling a fixture child to "has report" flips the
glyph; integration test.

### AC-8 — Bundle link consumption from ka-0000008153

Focus card surfaces a "Read bundle" link if the epic / descendants have
a bundle. **Blocked-by:** ka-0000008153 must define bundle discovery
from an epic. **Acceptance:** focus card contains link iff fixture
bundle exists; click navigates to bundle viewer route from
ka-0000008153.

### AC-9 — Tab affordance from `/p/{project}` to `/p/{project}/sequencer`

Add a "Sequencer" tab/link to the project list page so the surfaces
are adjacent. **Acceptance:** link present; round-trips back; selected
state highlights when on the sequencer.

### AC-10 — Docs + a11y check

Update `gardenio/docs/beads-viewer-a11y-baseline.md` with the
sequencer's keyboard model + color contrast. Update
`gardenio/CLAUDE.md` if the sequencer warrants a top-level mention.
**Acceptance:** docs updated; a11y baseline run on the new page.

### AC-EXT-1 (cross-epic acceptance) — Predicate contract with ka-0000005836

Document on the sibling epic (or a shared `gardenio/docs/` reference)
that `has_report(bead) bool` and `blocked_on_decision(bead) bool` are
the **canonical** predicates the sequencer will call. Sibling epic
owns the implementation; sequencer must not duplicate detection. Filed
as a comment cross-link rather than an issue under either epic.

## Open questions to flag for operator

- **OPEN-1:** Direct children only, or transitive descendants, for the
  closed-children count?
- **OPEN-2:** v1 unselected grid: pure colored pills, or decorated with
  badges from ka-0000005836?
- **OPEN-3:** New daemon endpoint vs. extension of existing
  parent-child API for the aggregate (AC-2)?

If any of these blocks scoping when the epic unfreezes, file a
`gb decision create` against the unfrozen epic. While the epic is
on-ice, leave them as open questions in this doc.

## Summary

- **New view** at `/p/{project}/sequencer` — not a mode of the project list.
- **Color = status**, with a two-tone progress fill for mixed-status epics.
- **Single-primary-select** lifts constellation to the project level via
  a shared `buildConstellationView` helper (AC-1) — biggest engineering
  cost but unlocks every operator-asked feature.
- **Type + status fan-out depth** are query params on the constellation
  opts; mostly free once AC-1 lands.
- **Sibling integration** (ka-0000005836 badges, ka-0000008153 bundles)
  is contract-level — sequencer consumes predicates, doesn't reinvent.
- **10 child tasks** (AC-1..AC-10) plus 1 cross-epic ACK — to file under
  ka-0000003558, all priority=4, label `on-ice`, until operator
  unfreezes.
