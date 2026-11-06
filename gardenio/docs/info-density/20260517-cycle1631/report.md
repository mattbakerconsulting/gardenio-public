# Info-Density Audit — Cycle 20260517-cycle1631

**Target**: https://beads-viewer.gardenio.e2e.example.com/inbox  
**Epic**: ka-0000005534 · **Molecule**: ka-0000000947  
**Operator**: Matt Baker  
**Auditor**: gardenio-beadsviewer-infodensity-audit-f8z (crew)

## Method

Playwright-driven screenshots of `/inbox` for all 6 registered skins
(`baseline`, `minimal-refined`, `dense-admin`, `docs-notion`,
`dark-terminal`, `tufte`) at five viewport widths (480 / 768 / 960 /
1280 / 1600 px, viewport height fixed at 900). For each (skin, width)
combination we capture:

| Metric | Definition |
|---|---|
| `total_rows`     | Count of bead-row elements (`[data-bead-id]`) on the page. |
| `rows_above_fold`| Rows whose bounding rect intersects the 900px viewport. |
| `total_lines`    | Sum across rows of `round(row_height / line_height)`. |
| `bytes_per_row`  | Page HTML byte size / `total_rows`. |
| `wrap_ratio`     | `total_lines / total_rows` (≈ 1.0 = no wrap, >> 1.0 = heavy wrap). |

Skins are switched by setting `localStorage["beads-viewer:skin"]` and
reloading (matches the in-page switcher flow in `templates/layout.html`).

## Verdict summary (1280px reference, 21 rows of data)

| Skin              | Rows above fold | Wrap ratio | Bytes/row | Verdict |
|---|---:|---:|---:|---|
| `baseline`        |  7 | 4.00 | 8160 | **OK**     |
| `minimal-refined` |  4 | 4.33 | 8161 | **SPARSE** |
| `dense-admin`     | 10 | 4.67 | 8161 | **DENSE**  |
| `docs-notion`     |  5 | 4.14 | 8161 | **SPARSE** |
| `dark-terminal`   |  7 | 4.86 | 8161 | **OK**     |
| `tufte`           |  5 | 4.62 | 8160 | **SPARSE** |

(Verdict thresholds: ≥8 rows above the fold at 1280 = DENSE; 6–7 = OK;
≤5 = SPARSE. Rendering surface only; the row payload — pinned /
forwarded / flagged signals — is identical across skins.)

## Findings

- **`baseline` is a sane default** — fits 7 rows above the fold at the
  reference width with the lowest wrap-ratio of any non-dense skin
  (4.00). Chips fit on a single line at ≥768px; only at 480 do the
  type/forward chips wrap to a second line.

- **`minimal-refined` is SPARSE across all widths.** Generous vertical
  padding on the global nav (wraps into two rows at every width tested)
  plus the filter/control band push the row stream below the fold —
  only 4 rows visible at 1280, only 2 at 480. No wrap-ratio penalty
  vs. baseline, so the spew is pure vertical whitespace, not chip
  reflow.

- **`dense-admin` is DENSE and earns the name.** 10 rows above the fold
  at 1280, 12 at 1600. Nav collapses to a single row across all widths;
  filter band is tightened. Trade-off: at 1280 some long chip strings
  overflow horizontally (visible on the screenshot — bead-id chips clip
  past the right edge); this is a known cost of the dense layout.

- **`docs-notion` is SPARSE.** Notion-style padding gives every row a
  body-text feel: only 5 rows above the fold at all widths ≥768. The
  forwarded / pinned glyphs are visually de-emphasized.

- **`dark-terminal` is OK.** Same row count as baseline above the fold
  (7 at 1280), with the highest wrap-ratio (4.86) of the OK tier —
  chips wrap a second line at 480/768. No data-surface drops.

- **`tufte` is SPARSE on wide viewports.** Tufte's signature wide
  right-margin (the sidenote gutter) consumes ≈40% of the canvas at
  ≥1280px. Only 5 rows above the fold across all widths; the wide
  viewports buy zero additional density. The wide gutter is the design
  intent but reads as wasted space on a list-of-21-items screen.

## Hard-rule check

| Rule | Status |
|---|---|
| Rendering layer only, no data-surface changes | ✅ — same 21 rows surfaced across every skin × width. |
| Do not drop pinned / forwarded / flagged signals | ✅ — pin glyph + forwarded glyph present in every screenshot. |
| Feature branch + MR for code change | ✅ — docs-only branch + MR. |
| Never push to main | ✅ — feature branch only. |

## Bug bead

A bug bead has been filed against epic `ka-0000005534` for the three
SPARSE skins (`minimal-refined`, `docs-notion`, `tufte`); see linked
issue. Recommended remediation lives at the skin's CSS block in
`gardenio/controller/cmd/beads-viewer/templates/layout.html` — tighten
nav vertical padding, halve inbox row vertical padding, and (for
`tufte`) cap the right-margin gutter at ≈25% with a hard max of 240px
so wide viewports buy more list density.

## Artefacts

- Screenshots: `inbox/<skin>/<width>.png` — 6 skins × 5 widths = 30 PNGs.
- Raw metrics: `inbox/metrics.json` — machine-readable, same values
  as the table above plus the `row_selector` used.
