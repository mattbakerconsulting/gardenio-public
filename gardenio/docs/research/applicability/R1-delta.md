# R1 applicability delta — VictoriaMetrics deploy/ops vs tip-of-main helm chart

**Bead:** ka-0000006042 (R1 applicability) · **Parent:** ka-0000001798 (E0) · **Umbrella:** ka-0000007704
**Source research:** ka-0000000179 — `gardenio/docs/research/timeseries-victoriametrics-ops.md` (committed 2026-04-25)
**Re-verified against:** monorepo `main` @ `755d74f69` (2026-05-29), `acme-helm-chart` @ `d97e8ea` (2026-05-18)
**Verdict:** ✅ **Core recommendation still holds. Enable-storage path is unchanged and short.** One material drift (a *new, parallel* VM-operator scrape model + a storage-class naming error in the note) and three small follow-up clarifications. No blocker to E1.

---

## 1. Claims that still hold (re-verified line-by-line against templates today)

| Claim in the 2026-04-25 note | Status today | Evidence |
|---|---|---|
| Chart ships hand-written vmsingle + vmagent + grafana templates; no subchart, no vmoperator CRDs pulled in | ✅ Holds | `templates/vmsingle/{statefulset,service}.yaml`, `templates/vmagent/{deployment,configmap,rbac,service}.yaml`, `templates/grafana/{deployment,service,configmap,ingressroute,pvc,rbac}.yaml` all present |
| All gated by `*.enabled: false`, disabled in **every** environment | ✅ Holds | `values.yaml`: `vmsingle.enabled:false` (L2073), `vmagent.enabled:false` (L2111), `grafana.enabled:false` (L2149). No env in `acme-helm-chart/charts/gardenio/values/*` sets any to true |
| vmsingle StatefulSet, 1 replica, PVC, retention from `vmsingle.retentionPeriod`, image `victoriametrics/victoria-metrics:v1.108.1` | ✅ Holds | `vmsingle/statefulset.yaml`; image tag `v1.108.1` (values L2077) |
| vmsingle Service ClusterIP `:8428` | ✅ Holds | `vmsingle/service.yaml` |
| vmagent Deployment, 1 replica, K8s pod SD, remote-writes to vmsingle | ✅ Holds | `vmagent/deployment.yaml` args `-promscrape.config` + `-remoteWrite.url` defaulting to `gardenio.vmsingle.writeURL` |
| vmagent ConfigMap scrapes pods with `prometheus.io/scrape=true` in release ns | ✅ Holds | `vmagent/configmap.yaml` `kubernetes-pods` job, ns-scoped to `.Release.Namespace` |
| vmagent RBAC = ServiceAccount + Role for pod SD | ✅ Holds | `vmagent/rbac.yaml` (Role: pods/endpoints/services get/list/watch) |
| vmagent Service ClusterIP `:8429` | ✅ Holds | `vmagent/service.yaml` |
| Grafana datasource pre-wired to vmsingle `:8428` | ✅ Holds | `grafana/configmap.yaml` L16 `http://<vmsingle-fullname>:8428` |
| Grafana IngressRoute with Keycloak forwardAuth / IP whitelist / rate limit | ✅ Holds | `grafana/ingressroute.yaml`; values `grafana.ingress.{ipWhitelist,rateLimit}` (L2201–2212) |
| NetworkPolicy carve-outs gated by `vmagent.enabled` / `vmsingle.enabled` | ✅ Holds | `networkpolicies/networkpolicies.yaml`: vmagent→beads:9091 (L69–77), vmagent→controller:9092 (L398–405), vmsingle ingress from vmagent(write)+grafana(query):8428 (L933–966), vmagent/grafana egress blocks present |
| Helpers `gardenio.vmsingle.fullname/labels/selectorLabels/writeURL`, `gardenio.vmagent.*` | ✅ Holds | `_helpers.tpl` L596–676; `writeURL` = `http://<fullname>:8428/api/v1/write` |
| Prebuilt dashboards shipped | ✅ Holds **+1** | `dashboards/{cost-overview,kbeads-embedding}.json` **plus a new `operated-by.json`** added since the note |
| Producer: controller `/metrics` on `:9092`, pod annotated `prometheus.io/scrape=true`, `port=9092` | ✅ Holds | `controller/deployment.yaml` L25–26, L98 |
| Producer: `gb session` push → `$VMAGENT_URL/api/v1/import/prometheus` | ✅ Holds | `controller/cmd/gb/session_metrics_push.go` L29, L42; reads `VMAGENT_URL`, silently skips if unset |
| `vmsingle.persistence.storageClass:""` → inherit `global.storageClass`; size default 10Gi | ✅ Holds | values L2084–2087; statefulset volumeClaimTemplate inherits `global.storageClass` when component SC empty |
| Remote-write metric-name **keep-list relabel is NOT yet applied** (flagged as a small followup in the note) | ✅ Holds (still absent) | `vmagent/configmap.yaml` has no `__name__` keep filter — the §11/§4 `(gardenio\|beads\|kbeads\|…)_.*` keep relabel is still a follow-up, exactly as the note predicted |

**Bottom line on §1:** every structural claim about the templates, the `*.enabled` gating, the producer side, and the write URL is **still accurate**. The note's "enabling vmsingle and vmagent in one environment is the v1 ship" remains true.

---

## 2. Drift since 2026-04-25

### 2a. NEW — a second, parallel scrape model: `monitoring.victoriaMetrics` + VMServiceScrape CRDs (material)

Commit `d69f5296b` *feat(gardenio/helm): add monitoring.victoriaMetrics toggle for VMServiceScrape (ka-0000005115)* (**2026-05-24**, ~1 month after the note) added an **alternative** ingest model the research note never mentions:

- New values block `monitoring.victoriaMetrics.{enabled:false, interval:30s}` (values L2042–2053).
- New CRD templates `operator.victoriametrics.com/v1beta1 VMServiceScrape`, emitted only when `monitoring.victoriaMetrics.enabled=true`:
  - `templates/controller/vmservicescrape.yaml` (agents `/metrics` :9092)
  - `templates/beads/vmservicescrape.yaml` (beads `/metrics` :9091)
  - `templates/nats/vmservicescrape.yaml` (:8222 monitoring)
  - `templates/keycloak-runtime/vmservicescrape.yaml` (management port)
- Motivation (per the template doc-comments): the **customer-poc-aws POC** runs an **externally-installed vm-operator** (`operator.victoriametrics.com`) *without* prometheus-operator, so it needs VMServiceScrape objects rather than the chart's own vmagent or a ServiceMonitor.

**Why this matters for R1/E1:** there are now **two non-overlapping ways** to get gardenio metrics into VictoriaMetrics, and they answer different deployment questions:

| Path | What it is | When to use | E1 relevance |
|---|---|---|---|
| **A. In-chart vmagent + vmsingle** (the note's recommendation) | Self-contained: chart deploys its own vmagent (pod SD scrape) → its own vmsingle store | Self-contained env with **no** external VM/Prom operator. This is the umbrella-epic v1 "enable long-retention storage" goal | **This is E1.** `vmsingle.enabled + vmagent.enabled` |
| **B. `monitoring.victoriaMetrics.enabled` VMServiceScrape** | Emits scrape-target CRDs for an **externally-installed vm-operator** to consume; **no vmsingle, no storage** | Cluster already runs vm-operator/vmcluster (customer-poc POC) and you want it to scrape gardenio | **Not E1.** It produces scrape configs for someone else's store; it does not stand up the long-retention store the epic wants |

The two are explicitly designed to coexist (`monitoring.victoriaMetrics` doc-comment: "emitted in parallel with the ServiceMonitors — both can be enabled simultaneously"). **E1 = path A.** Do not confuse `monitoring.victoriaMetrics.enabled` with the storage-enable flags — it does the opposite of what E1 needs (delegates to an external store instead of standing one up).

### 2b. CORRECTION — storage-class naming in the note is wrong (the note says `gardenio-storage`; the real default is `gp2`)

The 2026-04-25 note repeatedly asserts the chart default StorageClass is **`gardenio-storage`** (gp3, ebs.csi.aws.com, WaitForFirstConsumer, expandable). **That name does not exist anywhere in the chart** (`grep -rn gardenio-storage gardenio/helm/` → nothing). Actual state today:

- `global.storageClass: "gp2"` (values L498) — **gp2, not gp3** — is what every PVC inherits when its component SC is empty.
- There **is** an optional chart-managed StorageClass, but it is named **`gardenio-gp3`** (values L571–580), `type: gp3`, `WaitForFirstConsumer`, `Delete`, `allowVolumeExpansion: true` — **and it is `create: false` by default** (not rendered).
- Every `acme-helm-chart` env (`gardenio`, `-rwx`, `-next`, `-test`, `-e2e`) pins component PVCs to **`storageClass: "gp2"`**. No env creates or uses `gardenio-gp3`.

So the note's storage section is directionally right (gp3 is available, expandable, WaitForFirstConsumer) but **factually wrong on two points**: (1) the name is `gardenio-gp3` not `gardenio-storage`; (2) it is **not** the active default — `gp2` is, and `gardenio-gp3` is not even created. This changes the E1 storage recipe (see §3).

### 2c. Minor — extra dashboard added

`dashboards/operated-by.json` now ships alongside `cost-overview.json` and `kbeads-embedding.json`. Non-blocking; the Grafana sidecar will provision all three.

### 2d. No drift on producer metrics, ports, network policy, helpers, image tag

Re-verified: controller `:9092` + scrape annotations, beads `:9091`, `gb session` push envelope, `VMAGENT_URL` plumbing, vmsingle/vmagent helpers, write URL, all NetworkPolicy carve-outs, and image tag `v1.108.1` are **unchanged** since the note. The keep-list relabel is still un-applied (correctly predicted as a followup).

---

## 3. Shortest path NOW to the v1 deliverable (enable long-retention storage = E1)

The path is still **a wrapper-chart values-only change in `acme-helm-chart`** — the gardenio repo needs **zero** changes to ship E1. The only delta vs. the note is the **storage-class line**.

Pick a target env (note recommends `gardenio-rwx` first — operator-facing, validates forwardAuth). Add under the `gardenio:` subchart key in `acme-helm-chart/charts/gardenio/values/gardenio-rwx.yaml`:

```yaml
vmsingle:
  enabled: true
  retentionPeriod: "1y"          # note's recommendation; chart default is 90d
  persistence:
    enabled: true
    size: 50Gi                   # note's recommendation; chart default is 10Gi
    storageClass: "gp2"          # ← CORRECTED. Match the env convention (gp2),
                                 #   NOT the note's non-existent "gardenio-storage".
                                 #   To use gp3 you must FIRST flip storageClass.create=true
                                 #   (renders the gardenio-gp3 SC) then set this to "gardenio-gp3".

vmagent:
  enabled: true                  # remoteWriteURL "" auto-wires to in-chart vmsingle

controller:
  env:
    VMAGENT_URL: "http://gardenio-vmagent.gardenio.svc:8429"   # enables gb-session push path

# Optional, dev/rwx only — operator dashboards:
grafana:
  enabled: true
  sidecar:
    enabled: true                # auto-loads the 3 shipped dashboards
  ingress:
    enabled: true
    host: "gardenio-grafana.<env>.acme.example"
    ipWhitelist: { enabled: true, sourceRange: ["<office-cidr>"] }
```

**Critical correction vs the note:** do **not** set `storageClass: ""` expecting `gardenio-storage`/gp3 — `""` inherits `global.storageClass` which is **`gp2`**. That is fine and is the lowest-friction choice. If gp3 is genuinely wanted (cheaper $/GiB, better baseline IOPS), the chart must first render the SC via `storageClass.create: true` (it is `create:false` today) and then pin `vmsingle.persistence.storageClass: "gardenio-gp3"`. That is a **two-line** change, still values-only, still no gardenio-repo edit.

**Sequence to E1 (unchanged from the note's §6 except storage):**
1. `T-vm-5` — enable vmsingle+vmagent in `gardenio-test` (smoke the writer/scraper). Values-only.
2. `T-vm-6` — enable in `gardenio-rwx` + Grafana. Validates forwardAuth + dashboards.
3. `T-vm-7` — enable in `gardenio-prod` (defer in-chart Grafana; register vmsingle datasource in ACME Grafana).
4. `T-vm-8` — add the `__name__` keep-list relabel to `vmagent/configmap.yaml` (the one still-pending gardenio-repo change; one stanza, non-blocking — apply at or just after first enable).

E1 itself ("enable storage") is steps 1–2. **No code, no new templates, no image bump.** A `helm template` + `helm lint` on the values overlay is the entire pre-flight.

---

## 4. Updated decision-point recommendation

The note's five open decisions (§5) are **still the right forks**, with these updates:

1. **PVC size (25/50/100 Gi):** unchanged — recommend **50 Gi**. Footprint math (~1.6 GiB/yr) still holds; no new producers added.
2. **Retention (90d/1y/2y):** unchanged — recommend **1y**.
3. **Prod Grafana (in-chart vs ACME-shared):** unchanged — in-chart for dev/rwx, ACME-shared in prod.
4. **`gardenio_beads_event_total` counter:** unchanged — still **absent**; still recommend instrumenting it (~30 lines in `controller/internal/metrics/metrics.go`). This is the one umbrella-v1 metric not yet emitted.
5. **vmagent keep-list relabel:** unchanged — recommend adopt at enable time.

**NEW decision the operator must now make (introduced by drift 2a):**

> **D-R1-6 — Which VM ingest model does gardenio's own long-retention store use: in-chart vmagent (path A) or the new `monitoring.victoriaMetrics` VMServiceScrape model (path B)?**
> **Recommendation: path A (in-chart vmagent + vmsingle) for the umbrella-epic v1 deliverable.** Path B does **not** stand up a store — it only emits scrape-target CRDs for an *externally-installed* vm-operator (added for the customer-poc POC, ka-0000005115). For gardenio's own 1-year-retention store on the e2e cluster (which has no gardenio-owned vm-operator), path A is the only one that delivers E1. Path B remains the correct answer **only** in a cluster that already runs vm-operator/vmcluster and you want *that* store to scrape gardenio (the POC case). They coexist; this is a per-environment choice, not a global one.

**NEW correction the operator/gardener must propagate (drift 2b):** the storage-class recipe in the source note (`gardenio-storage`) is wrong. The implementation values must use **`gp2`** (env default) or explicitly create+use **`gardenio-gp3`**. Flag this so a copy-paste of the note's §4 snippet doesn't ship a dangling `storageClass` reference.

---

## 5. Verdict (one-liner)

**Applicable, ship-ready, short path intact.** The in-chart vmsingle/vmagent/grafana scaffolding the R1 note described is fully present and still disabled everywhere; E1 is a values-only enable in `acme-helm-chart` (start `gardenio-rwx`). Two corrections to carry forward: (1) storage class is `gp2`/`gardenio-gp3`, **not** the note's non-existent `gardenio-storage`; (2) a new parallel `monitoring.victoriaMetrics` VMServiceScrape model exists for external-vm-operator clusters (customer-poc POC) and must **not** be mistaken for the storage-enable path. New decision **D-R1-6** disambiguates the two ingest models.
