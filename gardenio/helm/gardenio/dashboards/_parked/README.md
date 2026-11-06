# Parked dashboards

Dashboard JSON models in this directory are **intentionally excluded** from the
live gardenio Grafana suite and from the daily Grafana GRADE audit. They live
here — one directory below `dashboards/` — so they drop out of every surface
that keys off the top-level `dashboards/*.json` glob:

- **Image bake** — `gardenio/images/grafana-bridge/Dockerfile` (built by `images.yml`) copies
  `gardenio/helm/gardenio/dashboards/*.json` into the grafana-bridge image at
  `/dashboards`; a subdirectory is not matched by that glob.
- **Runtime backfill** — `grafana-bridge` `runBackfill` (`os.ReadDir` + `e.IsDir()`
  skip) only imports top-level `*.json`, so a parked board is never provisioned
  as a `type=dashboard` bead / runtime ConfigMap.
- **Grafana GRADE** — the audit enumerates `helm/gardenio/dashboards/*.json`
  (top level only), so a parked board is no longer scored.

`.helmignore` already ignores `dashboards/` (including this subdir), so parked
JSON is not packaged into the Helm release Secret either.

Parking is **git-reversible**: to revive a board, `git mv` it back up one level
into `dashboards/` and re-add its entry to the `keyToFile` map in
`tests/grafana_fleet_lifecycle_dashboard_test.go`.

## Currently parked

### `dark-drift.json` (uid `gitlab-dark-drift`)

Parked 2026-08-24 per decision **ka-0000003993** (task **ka-0000005317**).

The `gitlab_dark_*` metric family (ahead_commits, diverged_files,
conflict_topics/budget, drift_alert, peelability_rate, provenance_rate, and
their `*_slo` floors) plus the Mirror-sync `kube_job_status_failed` panel is
emitted **only** by `deploy-values/gitlab-dark/drift/drift-metrics.sh`. That
entire `deploy-values/gitlab-dark/` tree is a committed **DESIGN artifact that
is NOT APPLIED** — no exporter runs anywhere, so `gitlab_dark_*` has **zero
series** in `gardenio-vmsingle`. Neither *federate* nor *repoint* is executable:
there is no live source `/federate` endpoint and no other Grafana already
scraping these series.

Keeping the board in the graded suite made it re-flag as the fleet's lowest
board (R=1) every day for a structural reason it could never fix. It is parked
here instead.

**Un-park the day the exporter is actually deployed** (`deploy-values/gitlab-dark/`
applied to a live cluster and emitting). At that point Option A becomes a clean
sibling of the existing `perfVmFederation` / `devVmFederation` jobs: add a
`gitlab-dark-federation` entry to `vmagent.extraScrapeConfigs` (values.yaml)
pulling the source `/federate`, stamping `source="gitlab-dark"`, with a `keep`
allow-list bounded to the ~15 `gitlab_dark_*` names +
`kube_job_status_failed{namespace="gitlab-dark"}`. Decide alongside the
vpa-right-sizing sibling (ka-0000000341), same federation lane.
