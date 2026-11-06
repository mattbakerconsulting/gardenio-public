# type-badges — before/after

Per-type colored chips next to bead types in list views (project, search,
agents) and in the chip styles surfaced via `chip-type-*` classes from
`layout.html`. Captured with `make screenshot-beads-viewer` (Playwright,
`gardenio/tests/beads-viewer-screenshots/`) at the harness's pinned
viewports — desktop 1280x800 and narrow 480x800. Implementation lives in
commit 3cf2539c on `feat/beads-viewer-type-badges`.

The two harness pages that don't render bead-row type chips (`advice` —
saved-views index, `bead-show` — single-bead detail) intentionally show
no diff; pages with bead-row lists (`project`, `search`, `agents`,
`agents-filtered`, `costs`, `index`) all show the new colored chip.

> The per-page before/after screenshot tables (desktop + narrow) that
> previously followed were removed from the repo in the de-bloat
> (ka-0000000431, MR !5183) and are no longer committed. Regenerate them
> on demand with `make screenshot-beads-viewer`; the prose above records
> what the comparison covered.
