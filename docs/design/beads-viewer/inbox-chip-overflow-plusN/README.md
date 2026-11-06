# inbox-chip-overflow-plusN — screenshot gallery

Tracking bead: ka-0000005608 (epic) / ka-0000005709 (preflight).

Inbox chip-cluster overflow +N affordance work. This directory holds the
before/after screenshot pairs that accompany the styling MR.

## Layout

- `before/{desktop,narrow}/<page>.png` — captures from fresh `main` at the
  preflight commit (this branch).
- `after/{desktop,narrow}/<page>.png` — populated by the implementation MR.

Pages captured (per `gardenio/tests/beads-viewer-screenshots/` harness):
`index`, `project`, `bead-show`, `search`, `agents`, `agents-filtered`,
`costs`, `advice`, `health`, `inbox`. Viewports: desktop 1280×800, narrow
480×800.

## Reproduce

```sh
make run-beads-viewer   # in one shell (needs BEADS_HTTP_ADDR reachable)
make screenshot-beads-viewer
```
Output lands in `gardenio/tests/beads-viewer-screenshots/test-results/beads-viewer-screenshots/`.
