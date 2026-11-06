# beads-viewer: hover popover over the constellation graph

**Status:** Plan / design — not yet implemented
**Tracking:** ka-0000001701 (this plan), epic TBD on creation
**Scope:** beads-viewer = Go htmx server-side viewer at
`gardenio/controller/cmd/beads-viewer/`. **NOT** beads3d.
**Author:** thread-1777249843-070269 (planning agent), 2026-04-27
**Origin:** Matt's Slack request 2026-04-27 — "we want the new hover popover
functionality to work when hovering over a beads graph in beads viewer; that
would be sick. Research and write an epic of tasks with some tests."

---

## TL;DR

The bead-link hover popover (ka-0000005464, plan-of-record:
`docs/design/beads-viewer/popover-preview.md`) already works for HTML `<a>`
links inside descriptions, comments, dep lists, search results, etc. It
**does not fire** when the user hovers a node in the **constellation SVG**
rendered above the bead-show page (parents, children, similar beads).

We will extend `templates/static/preview_popover.js` so the same popover
fires for SVG `<a>` elements inside `figure.constellation`. No new server
endpoint, no new template — `GET /b/{id}/preview` and the singleton
`<aside id="bead-preview-popover">` already exist and are battery-included.

The work is roughly:

1. Make `isBeadLink` / `beadIdOf` understand SVG anchors (where `a.href` is
   an `SVGAnimatedString`, not a string).
2. Make `position()` clamp against the SVG node's `getBoundingClientRect()`
   — the existing implementation already does this; we only verify it works
   for SVG anchors and add a tiny offset so the popover hovers near the
   node rather than the whole `<svg>`.
3. Add a Playwright spec covering the constellation-node hover path.
4. Add a Go test confirming the constellation template still wraps each
   node in `<a href="/kd-...">` so the JS contract holds.

---

## Why this isn't already working

`preview_popover.js` event-delegates on `document` for `mouseover` /
`focusin` and uses `e.target.closest('a')` to find the anchor. That part is
fine — `closest('a')` walks across `<g>` / `<circle>` / `<text>` and lands
on the SVG `<a>` element, which is an `SVGAElement`.

The break is here:

```js
function isBeadLink(a) {
  if (!a || !a.href) return false;          // (1) SVG: a.href is an SVGAnimatedString — truthy ✓
  try { url = new URL(a.href, location.origin); } catch (_) { return false; } // (2) ✗
  …
}
```

For an `SVGAElement`, `a.href` is an `SVGAnimatedString` whose default
string coercion returns `"[object SVGAnimatedString]"` — which is not a
parseable URL. `new URL(...)` throws, the `catch` returns `false`, and the
popover never fires for SVG anchors. Same problem in `beadIdOf`.

The fix: read the URL via `a.href.baseVal` when it exists, or via
`a.getAttribute('href')` as a fallback. Both return the plain string
`"/ka-abc123"` (or `"/<basePath>/ka-abc123"`) that the constellation
template emits.

## Constellation anchor shape (for reference)

`gardenio/controller/cmd/beads-viewer/templates/partials/constellation.html`:

```html
<g class="constellation-center">
  …
  <a href="{{.Center.Href}}">         <!-- /ka-abc123 -->
    <title>{{.Center.TitleAttr}}</title>
    <circle … />
    <text … >{{.Center.Glyph}}</text>
  </a>
  …
</g>
```

```html
{{define "constellation_node"}}
<g class="constellation-node constellation-node-{{.Role}}">
  <a href="{{.Href}}"
     hx-get="{{.Href}}"
     hx-trigger="mouseover once"
     hx-target="none"
     hx-swap="none">
    <title>{{.TitleAttr}}</title>
    <circle … />
    <text … >{{.Glyph}}</text>
  </a>
  …
</g>
{{end}}
```

`Href` is computed in `constellation.go` as `s.basePath + "/" + bead.ID` —
exactly the shape `isBeadLink` already accepts for HTML anchors. The
existing `<title>` SVG tooltip will continue to render the browser-native
tooltip. Modern browsers suppress the native tooltip while a custom
popover is visible; if not, we can drop the `<title>` for SVG `<a>` nodes
that participate in the popover, but the cheap path is to leave it.

## Goals (delta over the existing popover)

G1. Hovering any `<a>` inside `figure.constellation` (center, parent,
    child, similar) for ≥150 ms shows the same popover the rest of the
    page already shows for bead links.
G2. Cache, hide-debounce, focus, Escape, scroll/resize repositioning, and
    `bead-popover-open` coordination all behave identically — by reusing
    the existing controller, not by forking a second one.
G3. Hovering off the SVG node (or off the popover) within ~150 ms hides
    the popover. Hovering onto the popover keeps it open even though it's
    DOM-detached from the SVG.
G4. The native browser tooltip from `<title>` should not race with the
    custom popover. Browsers suppress `<title>` while focus/hover sits on
    a child of the SVG anchor; verify this is fine in CI.

## Non-goals

- Inline editing (priority/assignee) from the SVG popover — separate
  features, same as for HTML-link popover.
- A different popover style for the SVG case. Same template, same chips,
  same excerpt. Visual parity is the point.
- beads3d (3D viewer) parity. beads3d already has its own click-to-open
  detail panel; out of scope.
- Touch/long-press fallback. Out of scope per the parent plan.
- Replacing the existing `htmx hx-get … mouseover once` warm-prefetch on
  constellation nodes. That is the "click is fast" prefetch — orthogonal.
  Leave it alone.

## Acceptance criteria (verifiable)

A1. On a bead-show page that has a non-empty constellation, hovering a
    parent/child/similar circle for ≥150 ms triggers exactly one
    `GET /b/{id}/preview` request and renders the popover.
A2. The popover for the constellation center node shows the *center
    bead's* preview (we are on its show page, but the popover is the same
    fragment and that's a feature: confirms parity).
A3. Re-hovering the same node within the same page session does **not**
    re-issue the request (per-page cache hit, identical to HTML links).
A4. Hovering off the node closes the popover within ~150 ms; hovering
    onto the popover keeps it open.
A5. Tab-focusing a constellation `<a>` shows the popover; Escape hides.
A6. The viewport-flip / clamp logic positions the popover sensibly when
    the constellation node is near the right or top edge of the viewport.
A7. The native `<title>` SVG tooltip does not visibly race with the
    custom popover (no double-tooltip flash at the 150 ms mark).
A8. Existing `popover_preview.spec.js` scenarios continue to pass — no
    regressions on HTML-link popover behavior.
A9. Go unit test confirms `constellation_node` and the center anchor
    both render `href="/kd-..."` in the BasePath="" case (and
    `/<base>/kd-...` when BasePath is set).

---

## Implementation

### 1) `preview_popover.js` — extract href in an SVG-safe way

```js
// Returns the plain href string for both HTML <a> and SVG <a>.
function hrefOf(a) {
  if (!a) return '';
  // SVGAElement: a.href is SVGAnimatedString; HTMLAnchorElement: a.href is string.
  if (a.href && typeof a.href === 'object' && 'baseVal' in a.href) return a.href.baseVal;
  if (typeof a.href === 'string') return a.href;
  // Last resort — both element types respond to getAttribute('href').
  return a.getAttribute('href') || '';
}

function isBeadLink(a) {
  var raw = hrefOf(a);
  if (!raw) return false;
  var url;
  try { url = new URL(raw, location.origin); } catch (_) { return false; }
  if (url.origin !== location.origin) return false;
  var path = url.pathname;
  if (BASE && path.indexOf(BASE) === 0) path = path.slice(BASE.length);
  path = path.replace(/^\/+/, '').replace(/\/+$/, '');
  return /^kd-[A-Za-z0-9]+$/.test(path);
}

function beadIdOf(a) {
  var url = new URL(hrefOf(a), location.origin);
  var path = url.pathname;
  if (BASE && path.indexOf(BASE) === 0) path = path.slice(BASE.length);
  return path.replace(/^\/+/, '').replace(/\/+$/, '');
}
```

### 2) Positioning

`getBoundingClientRect()` works on `SVGAElement`. The existing `position()`
flips above/below and clamps within the viewport, so it Just Works for
SVG anchors too — the bounding rect is the rect of the visible SVG
content (i.e., the circle plus title). One thing to add: the constellation
nodes are small (~12 px radius) and tightly packed. To avoid the popover
covering an adjacent node, increase `GAP` from 6 to 10 px when the anchor
is inside a `figure.constellation`. Implementation:

```js
function gapFor(anchor) {
  return anchor && anchor.closest && anchor.closest('figure.constellation') ? 10 : GAP;
}
```

…and substitute `gapFor(anchor)` for the bare `GAP` in `position()`.

### 3) Suppressing the native `<title>` tooltip race

For HTML, browsers always show the popover *and* the `title=` tooltip,
which can flash at the popover's 150 ms threshold. The constellation
nodes use SVG `<title>` (which behaves the same way). The cleanest fix is
to drop the `<title>` element from constellation anchors that participate
in the popover and rely on the popover for that information. **Decision:**
keep `<title>` for accessibility (screen readers read it) and add a CSS
rule that hides it visually when the popover is open. Actually, screen
readers don't render `<title>` as a sighted-tooltip, so there is no visual
race for AT users — only mouse users. Empirically Firefox/Chrome suppress
the native tooltip for ~250–500 ms after a mouse enter, which is past our
150 ms hover-intent. **Decision:** verify in a Playwright test that no
double-tooltip is visible at the popover's render time. If it flashes,
follow-up: remove `<title>` from the constellation anchors and rely on
the popover + `aria-label`.

### 4) Layout / wiring

No changes. `preview_popover.js` is already loaded once via `layout.html`.
The behavior change is entirely inside that file.

### 5) Non-bead anchors inside the constellation

The legend uses `<text>` and shapes but no `<a>` elements, so there's
nothing else inside `figure.constellation` for the popover to misfire on.
Verified by reading `constellation.html` — only the center and node
templates wrap content in `<a>`.

---

## Testing

### Playwright e2e — new spec `tests/popover_constellation.spec.js`

Use the existing harness (`gardenio/tests/beads-viewer-e2e/`). Pattern off
of `popover_preview.spec.js`. Seed setup: ensure the seed has a bead
whose constellation includes at least one `forward`, one `reverse`, and
one `similar` node. If the existing seed doesn't, extend `seed/`.

Scenarios:

- `popover_appears_on_constellation_node_hover` — hover a similar-bead
  circle, assert popover is visible within 500 ms with the right
  `data-bead-id`.
- `popover_appears_on_constellation_center` — hover the center disk,
  assert popover for the current bead is shown.
- `popover_dismisses_when_leaving_constellation_node` — hover off the
  node and off the popover; assert hidden within 500 ms.
- `popover_persists_when_moved_onto_popover_from_constellation` — hover
  parent node → drift onto popover → popover stays open.
- `popover_constellation_dismisses_on_escape` — hover, press Escape.
- `popover_constellation_caches_within_page` — hover same node twice;
  count exactly one `/b/{id}/preview` request.
- `popover_constellation_keyboard_focus_shows_preview` — Tab to a
  constellation `<a>`, assert popover.
- `popover_constellation_no_native_tooltip_race` — at the popover's
  rendered moment, assert that no `<title>`-derived browser tooltip is
  visible (heuristic: assert popover element is the topmost paint of
  type "tooltip" or assert `getBoundingClientRect` of the SVG `<title>`
  is irrelevant — Playwright cannot directly read native tooltips, so
  the check is "popover visible AND no test failure due to flake at 150
  ms"; if this proves flaky on CI, fall back to verifying the popover's
  computed `z-index` is greater than the SVG container's, and remove the
  `<title>` element in a follow-up).

### Go unit test — `constellation_test.go`

The constellation file already has tests; we add focused assertions:

- `TestConstellation_NodeTemplateHasBeadAnchor` — render the partial
  with a known forward node (`Href: "/ka-foo"`), assert the output
  contains `href="/ka-foo"`.
- `TestConstellation_CenterTemplateHasBeadAnchor` — same for the center.
- `TestConstellation_RespectsBasePath` — render with `BasePath="/v"` and
  a node with `Href="/v/ka-foo"`, assert `href="/v/ka-foo"` survives
  template execution. (The popover relies on this exact shape.)

These tests freeze the JS↔template contract: if a future refactor of
`constellation.html` strips `<a>` wrappers or moves to data attributes,
the unit test fails before the Playwright test does — saving CI time.

### Existing tests we must not break

- `popover_preview.spec.js` — must still pass identically.
- `bead_show_test.go`, `htmx_test.go` — unchanged.
- Existing `constellation*_test.go` — unchanged.

---

## Implementation breakdown — child tasks

The gardener (whoever picks this up) should work the tasks in order.
Each is a single feature branch + MR.

1. **JS: SVG-safe href extraction + constellation gap.**
   - Edit `preview_popover.js` (`hrefOf`, update `isBeadLink`,
     `beadIdOf`, `position` to call `gapFor`).
   - Acceptance: manual smoke shows popover firing on parents/children/
     similar circles; existing HTML-link tests still pass.

2. **Tests: Go template contract.**
   - Add `TestConstellation_NodeTemplateHasBeadAnchor`,
     `TestConstellation_CenterTemplateHasBeadAnchor`, and
     `TestConstellation_RespectsBasePath` to `constellation_test.go` (or
     add the file if missing).
   - Acceptance: `go test ./...` passes; intentionally breaking the
     `<a>` wrapper in the partial fails the new tests.

3. **Tests: Playwright e2e for constellation popover.**
   - Add `tests/popover_constellation.spec.js` with all eight
     scenarios above.
   - If the seed lacks a non-empty constellation, extend the seed.
   - Acceptance: spec passes locally + on CI; existing
     `popover_preview.spec.js` is unchanged and still green.

4. **Polish.**
   - Tune `gapFor` constant or fall back to dropping `<title>` from
     the constellation anchors if QA flags a native-tooltip flash.
   - Verify the legend / labels / hx-prefetch behavior is unchanged.
   - Acceptance: no regressions in any other beads-viewer e2e spec.

Sequencing: 1 unblocks 3. 2 can ship in parallel with 1. 4 wraps up.

---

## Risks / open questions

- **`<title>` race.** Modern browsers usually suppress the native SVG
  tooltip while the popover is open, but this is browser-dependent.
  Mitigation: verify in CI via the constellation Playwright spec; if it
  flashes, drop `<title>` from constellation anchors and rely on the
  custom popover plus `aria-label` for screen readers.
- **htmx prefetch interaction.** Constellation nodes already carry
  `hx-trigger="mouseover once"` to warm the navigation. The popover's
  `mouseover` listener and htmx's listener coexist — both fire, both are
  idempotent (htmx swaps target none; popover dedupes via cache). No
  expected interference, but worth a smoke test in task 1.
- **Seed coverage.** The current beads-viewer e2e seed may not produce a
  rich constellation for every bead. If we have to extend the seed,
  prefer adding a minimal `forward`/`reverse`/`similar` set to one
  fixture bead rather than reshaping the whole seed.
- **Visual overlap on small viewports.** On narrow widths the
  constellation SVG shrinks; small circles + 380 px popover may push the
  popover off the node. The clamp logic handles this — verify in the
  responsive spec scope (`responsive.spec.js`).

---

## Gardener handoff checklist

- [ ] Read this doc and the parent (`docs/design/beads-viewer/popover-preview.md`).
- [ ] Pull task 1 (JS SVG-safe href + gap) and ship.
- [ ] Pull task 2 (Go template contract test) and ship.
- [ ] Pull task 3 (Playwright e2e suite for constellation) and ship.
- [ ] Pull task 4 (polish + regression sweep) and ship.
- [ ] Close epic.
