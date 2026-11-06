# Archived docs

Docs here are **point-in-time reports, completed migration runbooks, or
implementation status snapshots** for work that has fully landed. They are
kept for historical context but are NOT authoritative for current state.

Active operational docs and runbooks live one level up in `gardenio/docs/`.

## How to archive a doc

```sh
git mv gardenio/docs/<doc>.md gardenio/docs/archive/
# Then update any inbound references (grep -r '<doc>.md' .)
```

A doc is a candidate for archival if it is:

- A **completed migration runbook** whose migration has finished and the
  legacy primitive is disabled (not just superseded — actually retired).
- A **point-in-time audit or investigation report** whose findings have
  either been acted on or are no longer load-bearing for current planning.
- An **implementation status report** for a feature that has fully shipped,
  with no active enhancement plan referencing this doc as the source of
  truth.
- An **enablement plan** whose code has shipped and whose enablement is
  deferred indefinitely (in this case, leave a one-line stub in the
  active dir pointing here).

## Archived docs

EKS-era runbooks. Their cluster (`prod-gardenio-eks`) was destroyed on
2026-09-16 and the terraform behind them was deleted in PR #1681; prod is the
k3s box (`deploy-values/gardenio-k3s.yaml`). Archived by gd-HGe22fUCr9.

- `eks-api-intrusion-runbook.md`, `eks-api-intrusion-redteam-runbook.md`,
  `eks-api-intrusion-threshold-tuning.md`: on-call card, red-team validation
  and threshold tuning for the EKS control-plane audit alerts. The chart's
  `eksApiIntrusionAlerts` is off on k3s.
- `rds-backup-restore.md`: backup and restore of the canonical-prod RDS
  instance (`terraform/modules/rds`, deleted). k3s runs the in-box Postgres;
  see `../postgres-backup-restore.md`.
- `coverage-efs-enable.md`: turning on the EFS store for the e2e-coverage PVC
  (`terraform/modules/efs`, deleted).
