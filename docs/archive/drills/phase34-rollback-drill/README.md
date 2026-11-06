# Phase 3.4 — Rollback drill toolkit (task ka-0000006773)

Two soak-safe rollback drills run on the **live** canonical-prod cluster
(account 123456789012, VPC 10.60.0.0/16) **while the 3.2 load soak
(ka-0000000656) is still running** — without disturbing it.

## Why this is soak-safe (the central constraint)

The 3.2 soak depends on the prod `gardenio` helm release (its beads daemon) and
its `slo-collect` reads **cluster-wide** SLO aggregates from `gardenio-vmsingle`:
`gardenio_reconcile_total`, `gardenio_metrics_scrape_errors_total`,
`aws_rds_database_connections_*`. So the drill is engineered to touch **none** of
those:

| Drill | What it touches | What it must NEVER touch |
|---|---|---|
| 3.4a bad-deploy | an **isolated** helm release `phase34-drill` emitting only `phase34_drill_up`, plus its own `phase34-drill-vmalert` | the prod `gardenio` release; any series the soak reads |
| 3.4b PITR | a **throwaway** RDS instance `…-postgres-drill` (`--no-multi-az --no-publicly-accessible`) | the live `canonical-prod-gardenio-postgres` the soak runs against |

`lib.sh` enforces this with `guard_not_protected_release` and
`guard_not_protected_rds` tripwires, plus `require_decision` (HARD RULE #3 — every
AWS/cluster write needs a fresh resolved `gb decision`).

## 3.4a — bad-deploy rollback (`bad-deploy-drill.sh`)

A tiny sacrificial app (`chart/`) serves `phase34_drill_up` (1=healthy, 0=broken)
and a `/ready` probe that fails when broken. A dedicated `phase34-drill-vmalert`
(same datasource `gardenio-vmsingle`, same blackhole notifier as the Tier-1 SLO
alerts — so **detection is faithful**) evaluates `PhaseThreeFourDrillDown`.

1. `helm install` healthy baseline → rev 1 (known-good), confirm green.
2. **T0**: `helm upgrade --set healthy=false` (deliberately broken release): gauge
   flips to 0 **and** the Deployment never becomes Available.
3. Poll the drill vmalert until `PhaseThreeFourDrillDown` **fires** → `T_detect`.
4. **T1**: `helm rollback` to rev 1, poll until gauge=1 and alert clears →
   `T_recover`.
5. **RTO** = `T_recover − T0`; detection latency = `T_detect − T0`.

```sh
./bad-deploy-drill.sh --context canonical-prod --i-have-a-decision <resolved-id>
```

## 3.4b — RDS PITR rollback (`pitr-drill.sh`)

Restore-to-point-in-time of the live 17.7 Multi-AZ instance **into a throwaway**,
landing in the same private subnet group + SG (reachable from the cluster for
verification), verify a sentinel point-in-time row count (RPO), record RTO, then
delete. Mirrors the Phase 1 runbook (`gardenio/docs/archive/rds-backup-restore.md`, report
ka-0000000043, observed RTO ~43 min on a db.t3.medium).

```sh
./pitr-drill.sh --i-have-a-decision <resolved-id>        # latest restorable time
# then verify RPO via an in-cluster psql pod against the drill endpoint, then:
aws rds delete-db-instance --db-instance-identifier canonical-prod-gardenio-postgres-drill \
    --skip-final-snapshot --delete-automated-backups
```

## DR targets measured against

From `docs/research/canonical-prod-dr-cost-posture.md`:

| Scenario | RPO target | RTO target |
|---|---|---|
| Bad chart/app deploy | n/a (no data loss) | minutes (rollback) |
| Accidental row/table delete (PITR) | seconds | minutes |

Phase 1 PITR actual (db.t3.medium): RTO ~43 min. The fresh cluster is db.r6g.large
Multi-AZ — this drill records the actual on prod-grade hardware. Gaps vs target
are filed as follow-up beads (acceptance criterion 3).

## HARD RULES (carried from ka-0000000765)

- Short-lived STS in agent context (≤12h); AKIA terraform exception only per
  advice ka-0000007102 (SM `gardenio/customer-poc-aws-creds`, acct 123456789012).
- A **fresh** `gb decision` gate before **every** AWS apply / data write — the
  3.4a bad-deploy, the 3.4a rollback-window, and the 3.4b PITR each gate.
- No writes until the per-apply gate resolves.
- Never cut canonical-prod traffic in a way that breaks the running soak.
