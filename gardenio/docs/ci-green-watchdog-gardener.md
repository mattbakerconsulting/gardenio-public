# CI-Green Watchdog Gardener

> Anchor epic: **ka-0000003884** — "Keep gardenio CI green (MR + main pipelines)"
> Origin: Slack ask from Matt Baker 2026-06-08 (research bead ka-0000003666).

## Why this gardener exists

Investigation on 2026-06-08 answered Matt's question — *whose job is it to keep
MR CI green, and why aren't they doing it?* — as follows:

- **Open-MR pipelines were actually green** (9 of 10 open MRs passed).
- The real pain was **main-push pipelines: 9 of the last 10 RED**, every one
  failing in the same job `rwx-deploy-rwx` (the staging deploy), never in
  build/lint/test.
- **Root cause:** the shared `aws-load-balancer-controller` (in the `operators`
  namespace, NOT in this repo) was **OOMKilled / CrashLoopBackOff for 22+ days**
  — memory limit was 128Mi and the leader replica needs ~128Mi just to warm its
  informer cache, so it died (exit 137) on every restart. With the controller
  down, its mutating webhook `mservice.elbv2.k8s.aws` had **zero endpoints**, so
  every helm deploy that creates/mutates a Service 500ed with
  `no endpoints available for service "aws-load-balancer-webhook-service"`.
- **Verdict: it was nobody's job.** No CI-health gardener existed. The failure
  was a *shared cluster-infra operator silently wedged* — exactly the class of
  problem no per-MR author would ever notice, because their MR pipelines were
  green.

So the answer is: **make it somebody's job.** This gardener is that somebody.

## Remit

Keep gardenio CI green across **both** MR pipelines and main-push pipelines.
Each cycle:

1. **Scan** the latest pipeline status for `main` and for every open MR in
   `example-org/gardenio`.
2. **Triage** each red pipeline into one of three classes:
   - **flaky / transient** → never retrigger, rerun or push an empty commit to a
     PR (owner rule 2026-10-31, root CLAUDE.md "CI on pull requests"); lost
     runners are re-run by `retry-lost-runners.yml`. On `main`, a re-run of the
     failed job is fine. File a flaky-test bug if it recurs.
   - **real app/test regression** → identify the authoring branch/agent, file a
     `gb decision` (or comment on the owning bead) so the author fixes it. Do
     NOT fix other people's feature regressions silently.
   - **shared-infra wedge** (deploy/webhook/operator/cluster) → this is the
     gardener's *own* job. Remediate within authz (restart/right-size the wedged
     operator), confirm CI recovers, and **file a durable-fix bug** so the live
     patch lands in the chart source (live `kubectl patch` on a Helm-managed
     release reverts on the next `helm upgrade`).
3. **Log** what was found and done as a comment on the epic, so trends
   (which job fails most, MTTR, recurring infra wedges) accumulate.

## Triage rubric (which class?)

| Symptom in the failed job trace | Class | Action |
|---|---|---|
| `build` / `lint` / `test` job failed with a code/compile/assertion error | real regression | find author, gb decision / comment on owning bead |
| `rwx-deploy-rwx` fails with `no endpoints available for service "aws-load-balancer-webhook-service"` | infra wedge | check `operators` ns LB controller (`kubectl get pods -n operators \| grep load-balancer`); if OOMKilled/CrashLoop, right-size mem + confirm endpoints; file durable bug |
| `rwx-deploy-rwx` fails with `UPGRADE FAILED: another operation in progress` | infra wedge (helm) | see advice ka-0000005959 / helm-state escalations ka-0000005363 |
| RWX dispatch shows `Dispatching … RunID` then RWX run fails inside a task | real (RWX-side) | `rwx logs <task-id>`; read the actual task error before classifying |
| Same test passes on a re-run with no code change | flaky | re-run on `main` only, never on a PR; if it recurs ≥2×, file a flaky-test bug |

**Do NOT pattern-match `RWX_ACCESS_TOKEN` as a vault expiry** (advice
ka-0000005959) — that secret name always appears in deploy-rwx YAML.

## Known shared-infra checks (the part no MR author watches)

```sh
# LB controller health (the 2026-06-08 culprit)
kubectl get pods -n operators -l app.kubernetes.io/name=aws-load-balancer-controller
kubectl get endpoints -n operators aws-load-balancer-webhook-service   # must be non-empty
# if OOMKilled: the deploy memory limit is too low
kubectl get deploy -n operators operators-aws-load-balancer-controller \
  -o jsonpath='{.spec.template.spec.containers[0].resources}'
```

If endpoints are empty and pods are OOMKilled, right-size the limit (the
2026-06-08 fix bumped 128Mi → 512Mi) and confirm both replicas go `1/1` and the
webhook gets endpoints. Always file the durable-fix bug (ka-0000007877 is the
template) — the chart lives in a acme-infra/terraform repo, not here.

## Gardener loop configuration

Authored via `gb gardener start ka-0000003884` with:

- **cron**: `*/20 * * * *` (every 20 min) — main pipelines run on every push and
  take ~10–15 min; 20 min catches a red within one cycle without thrashing.
- **preflight**: only do real work when there is actually a red pipeline OR the
  epic has open children; otherwise the run is a cheap no-op.
- **max-runs / loop-wall-clock**: standing gardener, but bounded per the
  mandatory-self-block advice (ka-0000000974). Use a generous `max-runs` and a
  `no_progress_for` guard rather than letting it loop unbounded.
- **run-wall-clock**: 30m per run is plenty for a scan+triage+remediate cycle.

This is a **continual** gardener (CI is never "done"), so unlike an epic-drainer
its loop_end is operator-driven (`manual`) plus the self-block guards, not
"all children closed."
