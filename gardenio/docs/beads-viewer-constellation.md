# beads-viewer constellation

The constellation is the SVG graph that floats above each bead's detail page in
beads-viewer. It draws three rings around the focal (center) bead:

- **parents** — explicit `depends on` rows (top arc)
- **children** — explicit `depended on by` rows (bottom-left arc)
- **similar** — semantic-similarity matches (right arc)

This document covers **how mention edges render** in the constellation: the
provenance distinctions, the styling, the data attributes the SVG carries for
downstream click-handling work, and the operator-visible toggle that hides
untyped mentions.

Source files:

| Concern              | Path                                                              |
| -------------------- | ----------------------------------------------------------------- |
| Edge construction    | `gardenio/controller/cmd/beads-viewer/constellation.go`            |
| SVG template         | `gardenio/controller/cmd/beads-viewer/templates/partials/constellation.html` |
| Toggle JS            | `gardenio/controller/cmd/beads-viewer/templates/static/constellation.js`     |
| CSS hide-rule        | `gardenio/controller/cmd/beads-viewer/templates/layout.html` (`.constellation[data-hide-untyped-mentions]`) |
| Tests                | `gardenio/controller/cmd/beads-viewer/constellation_test.go`       |

## Edge origins

Every edge in the constellation carries an `Origin` tag describing **where the
edge came from**. The same target bead can appear in more than one provenance
list (e.g. the operator wrote both an explicit `depends on` row AND a `parent:
ka-X` line in the description) — the constellation deduplicates so each
target gets one edge, and the precedence order is:

```
dep > text > mention
```

| Origin    | Source                                                     | Stroke                                | Dash  | Opacity |
| --------- | ---------------------------------------------------------- | ------------------------------------- | ----- | ------- |
| `dep`     | explicit `bead_dependencies` row                           | direction-color (#3b6fa0 / #5d8a3a)   | solid | 0.85    |
| `text`    | typed mention (`relationship_type` set in `bead_references`) | direction-color                        | `6 2` | 0.85    |
| `mention` | untyped mention (`relationship_type` NULL)                 | neutral gray `#7a7a7a`                | `3 4` | 0.55    |

Direction-color is the same palette as before: forward (this bead → parent) is
muted blue `#3b6fa0`; reverse (child → this bead) is muted green `#5d8a3a`.
The dash on text-origin edges is intentionally subtle so an operator who
wants strict parity with deps can ignore it; the closed decision
[ka-0000000923](https://beads/ka-0000000923) was "render typed mentions
equivalent to deps", and the dash satisfies that decision while still letting
sharp eyes see the provenance distinction.

## Edge data attributes

Every `<line>` in `<g class="constellation-edges">` carries the following
attributes when applicable. These are the contract that future edge-click
work (epic [ka-0000000608](https://beads/ka-0000000608)) will read:

```html
<line x1="…" y1="…" x2="…" y2="…"
      stroke="…" stroke-dasharray="…"
      data-origin="dep|text|mention"
      data-relationship-type="blocks"           ← typed only
      data-source-bead="ka-XXX"
      data-target-bead="ka-YYY">
  <title>blocks (text)</title>                  ← hover tooltip
</line>
```

Hovering an edge surfaces a short verb tooltip:

| Origin    | Verb            | Tooltip               |
| --------- | --------------- | --------------------- |
| `dep`     | `blocks`        | `blocks`              |
| `dep`     | (forward, none) | `depends on`          |
| `dep`     | (reverse, none) | `depended on by`      |
| `text`    | `parent-child`  | `parent-child (text)` |
| `text`    | (none)          | `mention (text)`      |
| `mention` | (always none)   | `mentions (text)`     |

## "Hide untyped mentions" toggle

Untyped mentions (gray dashed) can clutter the SVG when a bead is referenced
casually by many adjacent beads. The figcaption below the constellation
includes a checkbox labeled **"hide untyped mentions"**. State persists in
`localStorage` under key `constellation:hide-untyped-mentions` (value `"1"`
when hidden, key removed when visible).

Mechanism:

1. The checkbox toggles `data-hide-untyped-mentions="1"` on
   `figure.constellation`.
2. CSS in `layout.html` hides any `<line data-origin="mention">` under that
   attribute.
3. Typed-mention (`text`) edges and explicit `dep` edges are unaffected.
4. The `constellation.js` static module re-applies state on every
   `htmx:afterSwap` so partial replacements (activity feed updates etc.) keep
   the user's preference.

Orphaned nodes whose only edge was a hidden untyped mention remain in the
SVG; this is intentional — the user can still see the bead's existence even
if the connecting edge is muted out.

## Edge click → mention highlight

Clicking a `text`- or `mention`-origin edge navigates the browser to the
**source bead** with `?highlight=<target-bead-id>` (and `&rel=<verb>` for
typed mentions), then `highlight.js` flash-highlights the inline `ka-XXX`
anchor inside the description.

| Origin    | Click behavior                                                                            |
| --------- | ----------------------------------------------------------------------------------------- |
| `dep`     | no-op (dep is already listed in the deps section, no inline anchor to highlight)         |
| `text`    | `<base>/<source>?highlight=<target>&rel=<verb>` — prefers the typed anchor when matching |
| `mention` | `<base>/<source>?highlight=<target>` — falls back to any `a.mention[data-bead-id="…"]`   |

Wiring:

1. `constellation.js` installs a single delegated click handler on
   `<g class="constellation-edges">` (survives htmx swaps inside the wrapper)
   and reads `data-source-bead` / `data-target-bead` / `data-origin` /
   `data-relationship-type` off the clicked `<line>`.
2. The bead-show route renders description IDs through
   `LinkifyDescriptionHTML` (`linkify.go`), which emits anchors of the form
   `<a href="<base>/<id>" id="mention-<id>-<n>" data-bead-id="<id>" class="mention" [data-relationship-type="<verb>"]>ka-XXX</a>`.
   Each repeat of the same target gets a unique `id` so the highlight script
   can scroll to a specific occurrence.
3. `highlight.js` (loaded after `constellation.js`) parses
   `?highlight=…&rel=…` on `DOMContentLoaded`, scopes its query to
   `#description-block`, and adds `.mention-highlight` (yellow flash, ~3s)
   before scrolling the anchor into view.

Typed-verb correlation is "first verb wins per (target, source-field)" —
the source daemon already dedups at the API surface (one
`relationship_type` per `(target, field)`), and the highlight script falls
back gracefully to "any matching mention" when `?rel` doesn't line up.

Out of inline-highlight scope (intentional, tracked separately):

- Mentions in **comments** and **notes** — both load via htmx fragments and
  would need their own scoping pass. When no inline anchor is found,
  `highlight.js` scrolls the description heading into view and emits a
  `console.warn`.

## Out of scope (tracked elsewhere)

- **3D constellation.** The `beads3d/` repo is deprecated; mention-edge work
  there is bug-fix-only.

## Test surface

`constellation_test.go` covers:

- `TestEdgeStyle_*` pins the (stroke, dash, opacity) tuple per origin.
- `TestEdgeTitle` pins the `<title>` tooltip strings.
- `TestMergeTypedMentions_*` and `TestAppendUntypedMentionEdges_*` pin the
  per-target dedup ordering (deps > text > mention).
- `TestConstellation_EdgeRendersDataAttributes` is a golden-style render
  check that verifies the SVG carries every data attribute and tooltip the
  E2 click-handler will rely on.
- `TestConstellation_LegendIncludesMentionEntries` pins the new legend rows.
- `TestConstellation_HideUntypedToggleRendered` pins the checkbox UI.
