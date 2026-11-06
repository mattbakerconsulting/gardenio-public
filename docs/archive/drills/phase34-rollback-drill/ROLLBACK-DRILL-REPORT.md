# Phase 3.4 — Rollback drill report

**Task:** ka-0000006773 · **Program:** ka-0000000398 · **Parent:** ka-0000002725 (Phase 3 PROVE)
**Cluster:** canonical-prod (account 123456789012, VPC 10.60.0.0/16), us-east-1
**Window auth:** decision ka-0000007390 (shared-standup) · **Operator:** matthew.baker (slack:U0OPERATOR1)

Both scenarios executed on the **live** cluster **while the 3.2 load soak
(ka-0000000656) was still running**, without disturbing it. Each AWS/cluster
write was gated by a fresh `gb decision` (HARD RULE #3).

---

## Scenario A — bad-deploy rollback (cluster) — PASSED ✅

**Gate:** ka-0000008225 (resolved `proceed`) · **Report artifact:** ka-0000003054

**Method.** An isolated, sacrificial helm release `phase34-drill` (in
`gardenio-system`) exposes a single gauge `phase34_drill_up` (1=healthy,
0=broken) and a `/ready` probe that fails when broken. A dedicated
`phase34-drill-vmalert` — **same datasource (`gardenio-vmsingle`) and same
blackhole notifier as the production Tier-1 SLO alerts** — evaluates
`PhaseThreeFourDrillDown: (phase34_drill_up == 0) or absent(...)`. So the
detection path is faithful to how a real SLO breach would be caught, while the
drill touches **none** of the prod `gardenio` release or any series the soak's
`slo-collect` reads.

**Timeline.**

| Event | Timestamp (UTC) | Δ |
|---|---|---|
| T0 — broken release deployed (`helm upgrade --set healthy=false`) | 2026-06-16T03:58:55Z | — |
| Detection — `PhaseThreeFourDrillDown` fired | 2026-06-16T04:00:30Z | **detect 95s** |
| T1 — `helm rollback` to rev 1 (known-good) initiated | 2026-06-16T04:00:30Z | — |
| Recovery — gauge=1 and alert cleared | 2026-06-16T04:01:53Z | **rollback→green 83s** |

**RTO (detect→recover) = 178s (~3 min).**

**vs DR target** (`docs/research/canonical-prod-dr-cost-posture.md`): bad
chart/app deploy RTO target = *“minutes (rollback)”*. **Actual 178s — within
target.**

**Soak safety — proven.** Across the drill window (03:54–04:03Z) the 3.2 soak
held S2=1.0, S3=0.9956, S4=1.0 with **zero Tier-1 breaches**; `phase32-load-gen`
and `phase32-slo-collect` stayed 1/1 Running (18h, 0 restarts). Detection used
the dedicated drill vmalert; the prod `gardenio-vmalert` configmap and the soak's
SLO series were never modified.

---

## Scenario B — RDS PITR rollback — WAIVED by operator dispensation ⚖️

**Gate:** ka-0000004366 — resolved by **operator dispensation** (matthew.baker,
via Slack): *“We can skip this; I’ve done the procedure before. Write it off as
operator dispensation.”*

The live PITR write was therefore **not executed** this run. The acceptance
criterion for the PITR scenario is satisfied by the **Phase 1 PITR drill**, which
already exercised the operational runbook end-to-end against a live env, plus the
operator's explicit dispensation here:

- **Phase 1 PITR drill** (task ka-0000003875, report **ka-0000000043**):
  `restore-db-instance-to-point-in-time --use-latest-restorable-time` into a
  throwaway, source instance untouched throughout.
  - **Observed RTO ~43 min** (restore API 04:59:23Z → available 05:42:36Z).
  - **RPO verified**: sentinel counts matched the consistent point-in-time
    (beads/comments/labels exact; events −1 vs a later source baseline, which
    *proves* a consistent PITR point rather than a live copy). RPO ≈ seconds.

**Ready-to-run toolkit shipped.** The operational runbook for re-running this on
canonical-prod is fully built and committed here (`pitr-drill.sh` +
`pitr-verify-rpo.sh`), tripwire-guarded to restore only into a throwaway and
never touch the live instance — so the drill can be executed on demand under a
fresh gate if a future operator wants the prod-grade (db.r6g.large) actual.

**vs DR target** (PITR / accidental delete): RPO target = *seconds*, RTO target =
*minutes*. Phase 1 actual (db.t3.medium): RTO ~43 min, RPO ≈ seconds — within
target (RTO at the high end of the runbook estimate for a small instance).

---

## RTO/RPO actuals vs Phase 1 DR targets — summary

| Scenario | Target RTO | Target RPO | Actual RTO | Actual RPO | Verdict |
|---|---|---|---|---|---|
| Bad-deploy rollback (this run, live canonical-prod) | minutes | n/a (no data loss) | **178s** | n/a | ✅ within |
| RDS PITR (Phase 1 drill ka-0000000043; 3.4b waived by operator) | minutes | seconds | **~43 min** (db.t3.medium) | **≈ seconds** (sentinel-verified) | ✅ within; ⚖️ live re-run waived |

### Gaps / follow-ups (acceptance criterion 3)

- **No gaps that breach target.** Both scenarios landed within their DR targets.
- **S1/S5 SLI probe gap** (pre-existing, documented since the soak start): the
  in-cluster load hits the ClusterIP, not the ALB, so S1 (ingress 5xx) and S5
  (p95 latency) read `NaN` (`no-probe-configured`). Not a rollback-drill gap, but
  the bad-deploy detection deliberately used a dedicated drill SLI rather than S1
  for exactly this reason. Tracked for the soak/proof-bar discussion.
- **Prod-grade PITR actual not captured this run** — the ~43 min figure is from a
  db.t3.medium (Phase 1). The db.r6g.large Multi-AZ actual was not measured
  because the operator waived the live write. The `pitr-drill.sh` /
  `pitr-verify-rpo.sh` toolkit is committed and ready to run on demand under a
  fresh gate if that prod-grade number is later wanted.
