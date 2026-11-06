# Archived top-level docs

Repository-wide docs that have been superseded or whose subject matter has
shipped. Active top-level docs live one level up in `docs/`.

For research notes specifically, see `docs/research/archive/`.

## Archived docs

- `07-beads3d-dashboard.md` — user-guide page for the retired `beads3d` 3D
  force-graph viewer (removed, ka-0000006444). Replaced by beads-viewer.
  Screenshot: `beads3d-ui-overview.png`.
- `eks-cluster-upgrade.md` — EKS upgrade runbook for `poc-gardenio-eks` and
  `prod-gardenio-eks`. Both clusters and `terraform/modules/eks` are gone
  (EKS destroyed 2026-09-16, modules deleted in PR #1681).
- `public-readiness-inventory.md` — 2026-10-31 count of placeholder and
  dead-host strings from `scripts/public-readiness-inventory.sh`, taken for
  the public-ready scrub (gd-uNpaF2zvVH). A point-in-time snapshot; its
  pattern table names the strings the public-strings guard bans.

## How to archive a doc

```sh
git mv docs/<doc>.md docs/archive/
# Then update any inbound references (grep -r '<doc>.md' .)
```
