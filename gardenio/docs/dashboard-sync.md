# Grafana dashboard delivery — how JSON reaches live Grafana

**TL;DR:** dashboards ship in the **grafana-bridge image** (`/dashboards/*.json`),
NOT in the Helm chart. On startup the bridge backfills each JSON into a
`type=dashboard` bead (idempotent on uid) and its SSE/catch-up sync provisions a
per-uid `grafana_dashboard="1"` ConfigMap at runtime; the Grafana sidecar
live-reloads it within ~30s. A dashboard-JSON edit reaches Grafana on the next
**grafana-bridge image roll** (or immediately, if you edit the dashboard bead) —
no full chart release required.

## Why the JSON left the chart (ka-0000002799 / ka-0000003724)

Dashboards used to ship as `grafana_dashboard="1"` ConfigMaps rendered by chart
templates via `.Files.Get "dashboards/*.json"`. Helm serialized **both** copies
into every per-revision release-history Secret:

- `chart.files` (the raw JSON, ~296KB) **and**
- the rendered manifest ConfigMaps (~299KB).

After gzip that ~200KB double-store pushed the release Secret
`sh.helm.release.v1.gardenio.vN` past the Kubernetes **1,048,576-byte** hard
limit, and **deploy-prod failed** (release `2026.211.1`, Secret v362), stranding
prod. The double-store is inherent to Helm — anything Helm renders a ConfigMap
from is stored once as chart content and again as the rendered object — so the
only durable fix was to take the dashboards out of the chart entirely. See the
operator decision ka-0000003578.

## How dashboards flow into Grafana now

```text
gardenio/helm/gardenio/dashboards/*.json          (source of truth, in git)
        │  copied into the grafana-bridge image at BUILD time
        │  (.rwx/docker.yml push-grafana-bridge; .helmignore'd OUT of the chart)
        ▼
grafana-bridge image  /dashboards/*.json         (baked; NOT in the release Secret)
        │  runBackfill (GRAFANA_BACKFILL=true, GRAFANA_DASHBOARDS_DIR=/dashboards)
        │  idempotent on dashboard_uid
        ▼
type=dashboard beads  (+ type=doc model docs)    (in the beads DB, not the Secret)
        │  GrafanaSync: SSE (bead create/update) + startup catch-up
        ▼
grafana-dashboard-<uid> ConfigMap  (managed-by=grafana-bridge, created at RUNTIME)
        │  kiwigrid/k8s-sidecar, METHOD=WATCH (K8s watch API)
        ▼
/tmp/dashboards/*.json  (emptyDir in the Grafana pod)
        │  Grafana file provisioning provider, updateIntervalSeconds: 30
        ▼
live Grafana  (visible/updated within ~30s of the ConfigMap change)
```

The bridge's `ChartOwnsDashboard` guard (ka-0000002774) still exists: if some
chart still shipped a `grafana_dashboard` ConfigMap for a uid, the bridge would
defer to it to avoid Grafana's duplicate-uid provider lockout. With the chart
ConfigMaps gone, the guard finds no chart owner and the bridge is the sole
provisioner — no code change was needed to the guard.

## Enabling it (per environment)

Dashboards render **only** where the grafana-bridge and the Grafana sidecar are
both on:

```yaml
grafana:
  sidecar:
    enabled: true          # loads grafana_dashboard ConfigMaps into Grafana
grafanaBridge:
  enabled: true
  backfill:
    enabled: true          # import the baked /dashboards JSON as beads on startup
```

Set these in the environment's deploy values; for prod that is
`deploy-values/gardenio-k3s.yaml`.

> **Deprecated no-op gates:** `agents.metrics.grafanaDashboard.enabled`,
> `monitoring.monorepoDashboards.enabled`,
> `vtsingle.grafanaDashboard.enabled` / `.monorepoDistributedTraces`, and
> `agents.metrics.prometheusRule.vpaGuardrails.grafanaDashboard.enabled` no
> longer render anything (their ConfigMap templates were removed), and the keys
> are gone from `values.yaml` (gd-R0jeUZAl3F). An overlay that still sets them is
> ignored; migrate overlays to the `grafanaBridge` block above.

## Editing / adding a dashboard

- **Edit an existing dashboard's JSON in git** (`gardenio/helm/gardenio/dashboards/<uid>.json`):
  it reaches Grafana on the next grafana-bridge image roll — the startup backfill
  refreshes the bead's model doc, and the sync re-provisions the ConfigMap. (A
  main-push builds the image; a deploy rolls the bridge pod.)
- **Edit a live dashboard as a bead** (`gb grafana put <file.json>` or the
  Grafana read-back path): the SSE sync provisions it within ~30s without any
  image roll.
- **Add a brand-new dashboard:** drop `<uid>.json` under `dashboards/` (it gets
  baked into the image and backfilled) — the bridge creates the bead and the
  ConfigMap on next startup. No enable-flag wiring per dashboard any more.
- **Catalog-generated dashboard** (the `/costs` panels): do NOT hand-edit the
  model — edit the `ChartDef` and regenerate. See the next section.

## Catalog-generated dashboards (single source of truth)

Some dashboards are **generated from the kbeads chart catalog**, not authored as
standalone JSON. The `/costs` candidate dashboard `gardenio-costs-candidate`
(bead `ka-0000005371`, panels `fleet-spend-rate` + `agent-spend-over-time`) is
the first: its Grafana model is produced deterministically from the `ChartDef`
entries in `kbeads/internal/server/charts_catalog.yaml` (embedded and loaded by
`charts_catalog.go`), so the catalog is the
**single source of truth** for those panels' queries, axes, units, and legends.

This replaces the old **lockstep** rule (retired here, epic ka-0000002763,
decision ka-0000007963): a series change no longer has to be applied by hand to
both the catalog AND the Grafana model — you edit the `ChartDef` and regenerate.
The pipeline is `GeneratePanel` → `AssembleDashboard`
(`charts_grafana_dashboard.go`) → the `kd admin gen-dashboards` writer, and the
`grafana:dashboard-model` doc it writes is the same one the grafana-bridge syncs
(see the flow diagram above), so a one-line catalog edit propagates to the live
`/costs` panel with no manual JSON edit.

### Workflow: edit `ChartDef` → regenerate → commit

1. **Edit the `ChartDef`** entry in `kbeads/internal/server/charts_catalog.yaml` —
   add an `extra_series`, change a `query_template`, `y_axis_index`,
   `y_format`/`y_title`, `mark`, `title`/`subtitle`, etc. Shared PromQL fragments
   stay Go constants in `charts_catalog.go`, referenced from the YAML as `${name}`.
2. **Regenerate the committed golden** and eyeball the diff (this is what makes a
   generated-model change a deliberate, reviewed act):

   ```sh
   GB_UPDATE_GOLDEN=1 go test ./kbeads/internal/server/ -run 'TestDashboardModel_Golden|TestChartCatalog_Golden'
   git diff kbeads/internal/server/testdata/
   ```

3. **Commit** the catalog change **together with** the regenerated
   `<uid>.golden.json` and `chart_catalog.golden.txt`. CI's `TestDashboardModel_Golden` (offline, hermetic) reds
   the MR if the catalog and the golden drift — so you can never merge one
   without the other.
4. **Push to the live model doc** so the running Grafana panel reflects the
   catalog. On a deploy this is run for you; to reconcile immediately against the
   live daemon:

   ```sh
   kd admin gen-dashboards                        # write all catalog dashboards
   kd admin gen-dashboards gardenio-costs-candidate # or one uid
   kd admin gen-dashboards --dry-run              # preview without writing
   ```

   The writer is idempotent (byte-stable) and nudges the dashboard bead so the
   grafana-bridge re-syncs; the live panel updates within ~30s (sidecar reload).

### Two drift guards

- **Offline golden** — `TestDashboardModel_Golden`
  (`kbeads/internal/server/charts_grafana_dashboard_golden_test.go`): runs in CI
  with no daemon, pins the full generated model per uid to a checked-in
  `testdata/dashboards/<uid>.golden.json`, and fails on any catalog/generator
  change whose golden was not regenerated (or a stale golden left behind). This
  is the CI gate.
- **Live** — `kd admin gen-dashboards --check`: reads the live model docs and
  exits non-zero if any has drifted from what the catalog assembles (a catalog
  edit that was committed but never pushed with `gen-dashboards`). Needs a
  running daemon; wire it as a scheduled alert. It never writes.

> **Adding another catalog-generated dashboard:** register its uid in the
> generator (`DashboardUIDs()` / the catalog assembly), create the
> `type=dashboard` bead + its `grafana:dashboard-model` doc, then
> `GB_UPDATE_GOLDEN=1 go test … -run TestDashboardModel_Golden` to mint the
> golden. From then on it follows the same edit-`ChartDef`→regenerate→commit
> loop above — no hand-authored JSON.

## The transition deploy (one-time gap)

On the single release that removes the chart ConfigMaps, live Grafana briefly
has no dashboards until the rolled bridge pod backfills + syncs (seconds to
~1min). This is unavoidable: you cannot pre-create bridge ConfigMaps while the
chart ConfigMaps still carry the same uids (that is the duplicate-uid lockout the
guard prevents). After the transition the bridge-written ConfigMaps and their
beads persist across deploys (they are not chart-owned, so `helm upgrade` never
deletes them), so subsequent deploys have **no** gap.

## History

The old decoupled-sync path (`scripts/sync-dashboards.sh`, `.rwx/dashboards-sync.yml`,
the `rwx-dashboards-sync` CI job) existed to re-apply the chart-rendered
dashboard ConfigMaps between releases (ka-0000003544), because main-push does not
deploy. It was removed with the chart ConfigMaps (ka-0000002799): the bridge's
SSE/catch-up sync now closes that same accuracy gap directly, and there are no
chart ConfigMaps left to re-apply.
