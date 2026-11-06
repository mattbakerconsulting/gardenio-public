# inbox-chip-overflow-plusN — before/after

Tighten the chip cluster on `/inbox` (and other bead-row pages) so a row
with many chips collapses overflow into a compact `+N` affordance instead
of wrapping into a tall stack. Implementation lives on
`feat/ka-0000004013-inbox-chip-overflow-plusN` (merged via dc2df8155 /
47b15f597) and is tracked by ka-0000004013 under epic ka-0000005608.

Captured with `make screenshot-beads-viewer` (Playwright,
`gardenio/tests/beads-viewer-screenshots/`) at the harness's pinned
viewports — desktop 1280×800 and narrow 480×800. Pages without dense
chip clusters (`advice`, `bead-show`, `costs`, `health`, single-bead
focus pages) will show little or no visual delta; pages that render
bead-row lists with chips (`inbox`, `index`, `project`, `search`,
`agents`, `agents-filtered`) carry the change.

> The per-page before/after screenshot tables (desktop + narrow) that
> previously followed were removed from the repo in the de-bloat
> (ka-0000000431, MR !5183) and are no longer committed. Regenerate them
> on demand with `make screenshot-beads-viewer`; the prose above records
> what the comparison covered.
