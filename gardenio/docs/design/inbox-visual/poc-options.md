# Inbox-primary visual POCs — options to vet the concepts

> **Status:** review/proposal. Not committing to build until operator picks an
> option. Sibling to [`mockups.md`](mockups.md) (the static SVG spec).
>
> **Scope:** the `/inbox` (inbox-primary constellation) page — referred to as
> "the bookmark page" in [Slack thread 1778004435](#) (assumed; awaiting
> operator confirmation).
>
> **Tied to:** ka-0000002831 (closed visual-design epic), ka-0000005952
> (closed v1 plan), ka-0000006157 (closed inbox-primary epic),
> ka-0000003625 (Option B operator FK — load-bearing upstream).

## 1. What's already in the repo

| Artifact | Form | Answers |
| --- | --- | --- |
| `gardenio/docs/research/inbox-constellation-design.md` (3,951 lines) | prose + ASCII | **behavior** (predicate, sector arcs, hydrate caps, keyboard model) |
| `gardenio/docs/design/inbox-visual/mockups.md` + 5 SVGs | static SVG | **layout** (palette, spacing, contrast, empty / default / overflow / peek / palette-ref) |
| `gardenio/plans/inbox-v1-option-b-sequencing.md` | sequencing | **build path** (gates G0–G5, Option B substrate, cutover) |

The SVG mockups landed via MR for ka-0000003214 on 2026-05-05. They're
the right fidelity for a *spec*, but they leave open the questions in
the "Reviewer asks" sub-sections of `mockups.md` §§2–5 — every one of
those questions is about something a static SVG can't show:

- Does the left rail at 380 px *actually breathe* at 1400 px wide,
  1024 px wide, 414 px narrow?
- Are the +N more sentinels distinct from real nodes when you can't
  hover them?
- Does the peek panel's connector line read as anchor or noise *after*
  you click a node?
- Does keyboard nav (j/k/Enter/space) *feel* right, or trip up at
  sector boundaries?
- Does the deferred row's 0.7 opacity read as "deferred" or "broken"
  at typical screen brightness?

These are perception + interaction questions. They want a thing the
reviewer can poke, not a picture.

## 2. POC ladder (cheapest → highest fidelity)

The four options below are independent — each can ship without the
later ones. Recommended order is annotated at the end.

### POC-1 — Static gallery page (~1 hour)

Single HTML page that embeds all five SVGs at native pixel scale, with
anchor links between them, served from the running `beads-viewer` at a
dev-gated route (e.g. `/preview/inbox-mockups`, only enabled when
`BEADS_VIEWER_PREVIEW=1`).

**What it answers:**
- "Do the SVGs actually render legibly at real DPR on a real browser?"
  (GitLab's SVG preview pixelates everything; iOS Safari and Firefox
  occasionally drop SVG fonts.)
- "Can we scroll the four states in one tab?" — discoverability for
  async reviewers.

**What it doesn't:** anything about interaction, keyboard, or real
data density. It's a smoke check on the SVGs themselves.

**Effort:** 1 file (`templates/preview_inbox_mockups.html`), 1 handler
(`preview.go`, 30 lines), 0 view-model changes.

### POC-2 — Live prototype with fixture data (~1–2 days) ★ recommended core

A real `/inbox` template rendered by the real viewer process, but with
**hardcoded JSON fixtures** instead of database calls. Reuses the
existing `constellation.tmpl.html` as a sub-template so the palette,
node radii, edge styles, and sector arcs come from the actual
`constellation.go` primitives — no parallel rendering pipeline.

**Fixture set:**
- `fixtures/inbox-empty.json` — single intake, zero descendants.
- `fixtures/inbox-default.json` — 8-mine list + selected intake with
  ~6 descendants (matches `mockups.md` §3).
- `fixtures/inbox-overflow.json` — 22-bead operator-queue with the
  +N more sentinels exercising the action / intermediate caps
  (matches `mockups.md` §4).
- `fixtures/inbox-peek.json` — same as overflow + a focused node with
  full peek payload (matches `mockups.md` §5).

**Route:** `GET /preview/inbox?fixture=default|empty|overflow|peek`,
dev-only, no operator-FK predicate, no Option B dependency. Behind the
same `BEADS_VIEWER_PREVIEW` gate as POC-1.

**What it answers:**
- All five "Reviewer asks" lists in `mockups.md`, because the reviewer
  can *click peek*, *press j/k*, *resize the window*, *zoom 200%*, and
  *open in mobile Safari* — none of which an SVG can show.
- The §6 "deliberately doesn't pin" list (sector boundaries, center
  radius, chip palette, orphan-actions style, empty-state wording) —
  each becomes a one-line CSS or constant change to A/B against.
- Whether the existing constellation primitives need any tweaks
  *before* v1 ships (the actual deliverable of the visual-design
  epic ka-0000002831).

**What it doesn't:** correctness of the operator-FK predicate, real
data shape, real performance with the production hydrate budget.
Those are gate G1 + G2 work in the v1 plan — out of scope for a
visual POC.

**Effort:** 1 new template (`inbox.tmpl.html`, ~150 lines reusing
`constellation.tmpl.html` partial), 1 view-model adapter
(`inbox_preview.go`, ~80 lines: JSON file → constellationView), 4
fixture files (~150 lines each). Zero schema work, zero migrations,
zero new daemon endpoints.

**Why this is the meat of the POC effort:** once it lands, every
follow-up question about the design becomes a 1-line tweak instead of
a re-render of a hand-drawn SVG. It also doubles as the v1 template
once the substrate gates close — so it's not throwaway.

**Implementation note (umbrella rule):** the v1 epic is locked to
research/design until operator green-lights build steps. POC-2 is
*template + fixture render*, no DB schema, no new endpoint that hits
the daemon. It is research-quality (you could merge it and never
build v1, and nothing in production behaves differently). Operator
should still pre-approve before MR opens — see §4.

### POC-3 — A/B variant matrix (~half day on top of POC-2)

Once POC-2 is in place, ship 2–4 alternate renders behind query
params:

- `?variant=action-right` — sector boundaries swapped to put `ACTION`
  on the right (closer to reading direction).
- `?variant=blue-chips` — selected filter chip uses `#1d6bbd` instead
  of golden, to test whether status (`in_progress`) and selection
  carry the same color cleanly.
- `?variant=center-48` — center radius bumped to 48 px to stress-test
  visual weight at the larger viewBox.
- `?variant=orphan-divider` — orphan-actions section as a divider
  line instead of a pink fill.

Each variant is a 1-line CSS + 1 constant change; all share the same
fixture data, so the only thing the reviewer compares is the visual
delta. This converts the §6 open-questions list from prose into a
clickable triage.

**What it answers:** every item on the `mockups.md` §6 "deliberately
doesn't pin" list, in one review pass.

**Effort:** ~half day on top of POC-2. Pure variant thunks; no new
template or view-model.

### POC-4 — Playwright screenshot matrix (~half day)

Use Playwright (already in the agent image) to render POC-2 at a
matrix of viewport sizes × fixture states × variants, save as PNGs to
`gardenio/docs/design/inbox-visual/screenshots/`, and embed them in
`mockups.md` as a follow-up section.

**Matrix:**
- Viewports: 1400×900 (desktop), 1024×768 (laptop), 414×896 (mobile).
- Fixtures: empty / default / overflow / peek-open.
- Variants: golden vs. POC-3 alternatives.

**What it answers:** the async-review case — reviewer can scan a PNG
gallery in 3 minutes instead of spinning up the viewer. Also captures
"what we shipped at the visual-spec freeze" as a permanent reference.

**Effort:** ~half day. One Playwright spec under `tests/visual/`,
~100 lines, runs in CI on a label.

## 3. Recommended sequencing

```
POC-1  ─►  POC-2  ─►  POC-4  ─►  POC-3 (if §6 still feels open)
1 hr      1–2 days   half-day    half-day
```

- **POC-1 first** as a smoke check — confirms SVG rendering on real
  browsers before we burn time on POC-2's view-model wiring.
- **POC-2 next** as the load-bearing artifact — answers the
  "Reviewer asks" questions and survives as the v1 template.
- **POC-4 right after** — generates the async-review gallery while
  POC-2 is fresh in cache.
- **POC-3 only if needed** — if POC-2 + POC-4 settle the §6
  open-questions list without the operator wanting to A/B, skip it.

If only one POC fits the budget: **do POC-2**. Everything else is
amplification of it.

## 4. Open decisions before a build starts

The umbrella epic ka-0000005972 is locked to research/design. Before
opening an MR for any POC above, get operator confirmation on:

1. **Does a dev-gated `/preview/inbox` route count as design or
   implementation?** It produces no new daemon traffic, no schema, no
   user-visible production behavior, but it does land Go + HTML in
   `gardenio/controller/cmd/beads-viewer/`. Recommend: design,
   conditional on the `BEADS_VIEWER_PREVIEW` env flag staying off in
   prod values.yaml.
2. **Fixtures: synthesized or sampled?** Synthesized fixtures are
   safer (no operator names / titles leak). Sampled-from-prod fixtures
   are more realistic. Recommend: synthesized, modeled on the
   `mockups.md` numbers (8 mine / 22 overflow / 4 raised).
3. **Where to host POC-2 for review?** Options: gardenio-rwx (staging,
   already has the viewer), a one-off ephemeral via the
   ephemeral-rwx-smoketest infra (ka-0000002007), or local-only via
   `make run-beads-viewer`. Recommend: gardenio-rwx behind the env
   flag — cheapest, async-reviewable, and doesn't block on the
   ephemeral umbrella.

## 5. What this doc deliberately doesn't propose

- **Building v1.** That's the v1 plan-of-record
  (`plans/inbox-v1-option-b-sequencing.md`); it's blocked on Option B
  substrate gates, which are independent of the visual spec.
- **Re-opening the closed visual-design epic ka-0000002831.** The
  static SVGs there are the *spec*; the POCs above *vet* the spec.
- **A new SVG round.** If POC-2 surfaces a layout flaw, we update
  `mockups.md` SVGs to match — but the test of the design isn't more
  pictures, it's a real DOM the operator can poke.

## Pointers

- `gardenio/docs/design/inbox-visual/mockups.md` — visual spec.
- `gardenio/docs/research/inbox-constellation-design.md` — behavior spec.
- `gardenio/plans/inbox-v1-option-b-sequencing.md` — build sequence.
- `gardenio/controller/cmd/beads-viewer/constellation.go` — primitives.
- `gardenio/docs/beads-viewer-dev.md` — local dev loop.
