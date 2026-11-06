# Beads-Viewer Style Exploration

This directory holds before/after screenshots for the first style-exploration
MR on `beads-viewer` (see ka-0000005252 under umbrella epic ka-0000000694).

Each candidate is one commit on the MR branch. The branch carries:

```
docs/design/beads-viewer/style-exploration/
├── _baseline/                # BEFORE screenshots captured from main
├── 01-minimal-refined/       # candidate 1
├── 02-dense-admin/           # candidate 2
├── 03-docs-notion/           # candidate 3
├── 04-dark-terminal/         # candidate 4
└── 05-tufte/                 # candidate 5 (wildcard)
```

Each candidate directory contains `desktop/` (1280×800) and `narrow/`
(480×800) PNGs for the four canonical pages: `index`, `project`,
`bead-show`, `search`.

Captured via the Playwright harness at
`gardenio/tests/beads-viewer-screenshots/` (ka-0000001180) against
beads-viewer booted locally per `gardenio/docs/beads-viewer-dev.md`
(ka-0000007138).

## Not a merge candidate

This MR is for picking, not landing. The follow-up MR lands the chosen style
(or a Frankenstein of pieces).
