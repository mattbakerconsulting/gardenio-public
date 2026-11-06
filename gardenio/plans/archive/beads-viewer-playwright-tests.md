# beads-viewer thorough Playwright behavioral test suite — plan of record

**Status:** scoping (proposed work, no implementation yet)
**Owner (gardener):** beads-viewer gardener — schedule `ka-0000002931`
**Target component:** `gardenio/controller/cmd/beads-viewer/` (Go htmx server)
**Source:** Slack 2026-04-25, Matt: *"scope new work for beads viewer gardener: thorough playwright tests!"*

## Why now

Phase 2 of the iOS-parity epic (`ka-0000002538`) shipped a wide additive
surface in beads-viewer: per-type detail templates, comment write path,
`/active`, `/agents`, `/advice`, `/decisions`, `/schedules`, `/costs`,
keyboard shortcuts, htmx partial swaps, drafts. Coverage today is:

- Go unit tests per handler (`*_test.go`) — solid, but request/response
  level only, not browser behavior.
- `gardenio/tests/beads-viewer-screenshots/` — Playwright **screenshot
  harness only**, captures canonical pages at two viewports for MR visual
  review. Not behavioral, not assertive, not a CI gate.

There is **no behavioral browser-level coverage** for: htmx swaps, comment
form submission, draft persistence, keyboard shortcuts, search debounce,
type/status filter chips, decision resolve flow, per-type detail
content correctness, navigation/active-tab state, or accessibility.

A regression in any of these surfaces (e.g. a stray `hx-target` typo, a
form action rename, a keyboard binding collision) ships green. Time to
build a proper behavioral Playwright suite, modeled on the maturity of
`beads3d/tests/`.

## Out of scope

- Visual regression / pixel-diffing (keep the existing screenshot harness
  separate; this suite asserts behavior, not pixels).
- Load / scalability testing.
- Cross-browser matrix beyond chromium (start chromium-only, add Firefox
  + WebKit only if a reported bug demands it).
- beads3d (different component, has its own suite).

## Reuse, don't reinvent

Reuse the existing `gardenio/tests/beads-viewer-screenshots/` directory's
config and `webServer:`-style auto-boot pattern, OR colocate a new
`gardenio/tests/beads-viewer-e2e/` directory if mixing screenshot config
with behavioral config gets gnarly. **Decision deferred to Phase 1
(T1)** — research task, gardener picks.

The screenshot harness already solves: pinned viewports, `BEADS_VIEWER_URL`
env knob, `make screenshot-beads-viewer` Makefile target, RWX-friendly
chromium install. Mirror that.

## Phased breakdown

Each phase is a child of the new epic. Phase ordering matters: Phase 1
(foundations) blocks all others.

### Phase 1 — foundations

**T1: harness scaffold + auto-boot.** Pick a directory layout
(reuse vs. colocate vs. sibling). Wire a `webServer:` block in
playwright.config that boots `make run-beads-viewer` (or equivalent)
against an in-memory or local-port-forwarded beads daemon. Goal: `make
test-beads-viewer-e2e` from a clean checkout boots everything and runs
green. Document in `gardenio/docs/beads-viewer-dev.md`.

**T2: deterministic fixtures.** beads-viewer talks to a live daemon. For
the harness we need a known data set: either (a) seed a throwaway daemon
in `webServer:` with a fixture script, or (b) point at the in-cluster
daemon and use `SCREENSHOT_PROJECT` / `SCREENSHOT_BEAD_ID`-style env
pinning the screenshot harness already uses. Decide and document. Tests
must be reproducible across runs — no flakes from drifting bead data.

### Phase 2 — smoke + nav

**T3: route smoke.** Every route returns 200, renders the layout, and
contains the expected page-level landmark (`h1`, nav, footer):
`/`, `/search`, `/active`, `/decisions`, `/agents`, `/advice`,
`/schedules`, `/costs`, `/p/{project}`, `/{id}`. One `test.describe`
per route, < 50 lines total. This is the safety net that catches
"renamed a template, server panics on render" regressions.

**T4: navigation + active-tab + breadcrumbs.** Click each top nav tab
from the index, assert URL change and active-tab styling. Click a bead
from a project list, assert breadcrumb back to project. Verify back
button restores scroll/state.

### Phase 3 — per-type detail templates

**T5: schedule, agent, advice.** For each type, render a known bead and
assert the type-specific fields are present and correctly formatted:
- `schedule`: cron expr rendered, next-fire timestamp present, enabled
  badge reflects state.
- `agent`: roster fields, current task link if set.
- `advice`: scope label (project vs. global), body markdown rendered.

**T6: decision, formula, mr, generic.** Same pattern:
- `decision`: options list, resolution state, resolve button visible
  only when unresolved.
- `formula`: ordered step list, current-step indicator.
- `mr`: pipeline status badge, MR URL link, branch name.
- `generic`: fallback for `task`, `epic`, `bug`, `chore` — title, body,
  metadata block.

### Phase 4 — interactions

**T7: comment write path.** POST `/b/{id}/comments`. Assert: optimistic
render before round-trip; success replaces optimistic node; failure (5xx
mock) shows inline error and preserves draft text; empty submit is
rejected client-side; multi-line markdown round-trips.

**T8: htmx partial swaps.** Click "load more comments" / deps swap
triggers. Assert: response is a fragment (not full page), target swap
happens, no full reload (track `navigationStart` timestamp). Catch
`hx-target` / `hx-swap` typos.

**T9: decision resolve flow.** Open a decision bead, click an option,
submit any required artifact, assert resolved state renders and the
unresolved CTA is gone.

### Phase 5 — search + filters

**T10: search.** Type into search box, assert debounce (no request <
N ms after keystroke), then result list renders, empty query shows
empty state, no-match query shows "no results", click result navigates
to bead.

**T11: type/status/label filters.** On `/p/{project}`: click type chip,
assert URL `?type=` updates and result list filters; multi-select
status chips apply both; clear button resets.

### Phase 6 — keyboard + drafts

**T12: keyboard shortcuts.** From `templates/static/keys.js`: `?` opens
help overlay; `Esc` closes; `g a/s/d/c` navigates to /agents, /schedules,
/decisions, /costs; `/` focuses search; `j/k` moves bead-list cursor;
`c` focuses comment input on detail; `Enter` while in input does NOT
trigger a shortcut.

**T13: drafts persistence.** From `templates/static/drafts.js`: type
into a comment box, navigate away, navigate back — draft restored.
Submit comment — draft cleared. Multiple beads have independent drafts
(localStorage keyed by bead id).

### Phase 7 — cross-cutting

**T14: auth middleware.** With `BEADS_VIEWER_DEV_AUTH_BYPASS=1`, all
routes accessible. With bypass off and no token, protected routes
redirect; OIDC start endpoint reachable. (Token path testing belongs
to Go integration tests — Playwright covers the redirect behavior.)

**T15: markdown + linkify.** Bead body with code fences, lists, links,
and `ka-XXXX` references renders correctly: code blocks have monospace,
list items have bullets, `ka-XXXX` references are turned into links to
the bead by `linkify.go`.

**T16: a11y baseline.** Run `axe-core` against every canonical page,
fail on **serious** + **critical** violations only (skip moderate/minor
to avoid bikeshed). Document baseline; ratchet over time.

**T17: responsive.** Mobile viewport (375×667): top nav collapses
correctly, two-pane layout stacks, comment form remains usable, no
horizontal overflow on canonical pages.

### Phase 8 — CI integration

**T18: wire into CI.** Add a `test-beads-viewer-e2e` RWX task in
`.rwx/*.yml`, gated to run on changes touching
`gardenio/controller/cmd/beads-viewer/**` and the test directory itself.
Cache npm + chromium between runs. Target wall time < 3 min.

## Risks & mitigations

- **Daemon dependency.** The viewer is useless without a beads daemon.
  Either ship a tiny in-process fixture daemon (preferred for hermetic
  CI) or use port-forward in dev + a CI-only seeded daemon. T2 decides.
- **htmx flakiness.** htmx swaps are async; tests must wait on
  network-idle or specific `hx-after-swap` events, not arbitrary
  sleeps. Document the wait pattern in T1's harness scaffold.
- **Drift.** Tests against live daemon data drift over time. Pin
  fixtures by id (existing screenshot harness uses `SCREENSHOT_BEAD_ID`)
  or seed a per-run dataset.
- **Scope creep into beads3d.** This plan does **not** touch beads3d.
  The gardener owns Go htmx beads-viewer only.

## Acceptance for the epic as a whole

- `make test-beads-viewer-e2e` passes from a clean checkout in < 3 min.
- All 8 phases shipped (T1–T18).
- CI gate runs on every viewer-touching MR.
- Gardener wrap-up confirms no behavioral regression has slipped through
  in 4 consecutive cycles after phase 8 lands.

## Decision points the gardener will hit

1. **T1 directory layout** — reuse screenshot harness vs. new sibling
   dir. Recommend new sibling: `gardenio/tests/beads-viewer-e2e/` to
   keep behavioral config (CI-gated, parallelizable, fail-loud) cleanly
   separated from screenshot config (advisory, single worker, never
   gates).
2. **T2 fixtures** — in-process daemon vs. seeded real daemon. Recommend
   seeded real daemon, pinned bead ids, because the wire protocol IS
   part of what we're testing. In-process fixture risks divergence.
3. **T16 a11y threshold** — recommend serious+critical only at first;
   broaden after baseline stabilizes.

These three forks should be raised as `gb decision create` checkpoints
when their respective tasks start, not pre-decided here.

## Bead layout

- Epic: "beads-viewer thorough Playwright behavioral test suite"
  (parent: `ka-0000002538` iOS-parity Phase 2 — sibling work, not blocked)
- Children: T1–T18, sequenced via `addBlockedBy` so foundations land
  before everything else.

The gardener (`ka-0000002931`) picks tasks off the epic each cycle,
spawns crew per task, never self-merges if the MR is style-sensitive
(per existing rules).
