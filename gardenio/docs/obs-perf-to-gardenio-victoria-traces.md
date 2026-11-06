# gardenio VictoriaTraces — store, perf ingest transport, retention budget

> Design / ADR for epic [[ka-0000000598]], design task [[ka-0000001643]].
> Owner request (Matt Baker, Slack thread 1781616614, 2026-06-16): get **traces**
> into the gardenio observability stack — add a VictoriaTraces pod, ingest perf
> traces into it, and (fast-follow) instrument gardenio itself.
> Sibling precedent: `obs-perf-to-gardenio-vm-federation.md` (perf **metrics** →
> gardenio vmsingle) and `obs-w1-site-remote-write.md` (the W1 push receiver).
> Rollout: e2e gardenio first, then the new prod gardenio — mirror the metrics
> epic's e2e→prod gate discipline.
>
> **Scope: design only.** This doc decides the topology. No chart change is made
> here — the VT-pod scaffold ([[ka-0000002444]]), ingest ([[ka-0000004875]]),
> instrumentation ([[ka-0000005802]]) and Grafana ([[ka-0000005235]]) tasks
> implement against these decisions, gated on the confirm-transport decision.

## TL;DR

1. **Store** — add an in-chart `victoria-traces` StatefulSet, default-off
   (`vtsingle.enabled: false`), mirroring the `vmsingle` template shape (gp3
   PVC, ClusterIP Service, securityContext, nodeSelector/tolerations helpers).
   Image `victoriametrics/victoria-traces:v0.8.0` (the version perf runs today).
   A single-node VT serves **OTLP/HTTP ingest *and* the Jaeger/VT select API on
   one HTTP port `:10428`**; OTLP/gRPC `:4317` is optional and off by default.
2. **Transport — RECOMMEND PULL/RELAY.** A small in-chart OTLP **relay**
   (otel-collector) tails the perf VT Jaeger API and replays spans OTLP→the
   in-chart VT. This is fully self-service inside the gardenio chart and the
   gardenio account, mirroring why the metrics epic chose pull over push. PUSH
   (perf otel-collector second exporter → cross-account gardenio VT ingress) is
   the documented fallback if perf ever firewalls its public VT.
3. **Retention / PVC** — `retentionPeriod: 7d` (match perf), **20Gi** gp3 PVC.
   Live perf VT holds ~657 MB on disk for a ~6-day window including k6 load
   tests; 20Gi is ~30× headroom for load-test bursts plus gardenio's own spans.
4. **Grafana datasource** — **Jaeger** datasource type pointed at the VT
   service's `/select/jaeger` API. No plugin install needed (Jaeger is a
   core Grafana datasource); VT implements the Jaeger query API natively.

```text
perf VictoriaTraces (perf.acme.example/victoria-traces, acct 234567890123)
  /select/jaeger/api  ──basic_auth, PULL──▶  gardenio otel-relay (NEW, in-chart)
                                               │ OTLP export
                                               ▼
                                             gardenio-victoria-traces  :10428
                                               │  /insert/opentelemetry/v1/traces
                                               ▼  (gardenio's own spans land here too)
                                             gardenio Grafana — Jaeger datasource
                                               → http://gardenio-victoria-traces:10428/select/jaeger
```

## 1. Source reality (probed live 2026-06-16)

`GET https://perf.acme.example/victoria-traces/...` with
`VICTORIA_TRACES_PERF_USERNAME:VICTORIA_TRACES_PERF_PASSWORD` (advice
[[ka-0000000864]]):

| Probe | Result |
|---|---|
| `/select/jaeger/api/services` | **200** → `keycloak`, `site-api`, `sponsor-api`, `study-web` (ns `k6-perf`, tenant `k6`) |
| `/insert/opentelemetry/v1/traces` (GET) | 500 (expects POST body) — ingest path **exists on the same `:10428` host** |
| `/metrics` → `vm_app_version` | `victoria-traces ... v0.8.0` |
| `/metrics` → `flag{name="retentionPeriod"}` | **`7d`** (`is_set=true`) |
| `/metrics` → `flag{name="storageDataPath"}` | `/victoria-traces-data` |
| `/metrics` → `vt_data_size_bytes{type="storage"}` | **656,958,623 (~657 MB)** on disk |
| `/metrics` → `vt_bytes_ingested_total{...otlphttp...}` | 10,110,792,460 (~10.1 GB OTLP wire) |
| `/metrics` → `vt_rows_ingested_total{...otlphttp...}` | 6,057,171 spans |
| `/metrics` → `vm_app_uptime_seconds` | 500,935 (~5.8 days) |
| `/metrics` → `process_resident_memory_bytes` | 227,930,112 (~228 MB RSS) |

**Reads from this:**
- The perf VT is a **single-node VictoriaTraces v0.8.0**, retention **7d**, and
  the public host fronts both `/insert/opentelemetry/v1/traces` (ingest) and
  `/select/jaeger/api/*` (query) — so a single in-chart VT replica reproduces
  the same surface.
- **Disk compression ≈ 15:1** (10.1 GB OTLP wire → ~657 MB on disk) over a
  ~6-day window that *included* k6 load tests. That ~657 MB is the real-world
  high-water mark for a 7-day retention of the perf trace stream.

## 2. Store — in-chart VictoriaTraces StatefulSet

Mirror the `vmsingle` template shape (`templates/vmsingle/{statefulset,service}.yaml`,
`values.yaml` `vmsingle:` block). New tree `templates/vtsingle/` + `vtsingle:`
values key, default-off.

| Aspect | vmsingle (today) | vtsingle (proposed) |
|---|---|---|
| Image | `victoria-metrics:v1.108.1` | `victoria-traces:v0.8.0` |
| Kind | StatefulSet, replicas 1 | StatefulSet, replicas 1 |
| HTTP port | `:8428` (`-httpListenAddr`) | `:10428` (default; OTLP/HTTP ingest **and** Jaeger select) |
| OTLP/gRPC | n/a | `:4317`, **optional, default off** (`-otlpGRPCListenAddr`, needs explicit enable) |
| Data path | `/victoria-metrics-data` | `/victoria-traces-data` |
| Retention flag | `-retentionPeriod=90d` | `-retentionPeriod=7d` |
| PVC | gp3, 10Gi, RWO, `volumeClaimTemplates` | gp3, **20Gi**, RWO, `volumeClaimTemplates` |
| Health probe | `/health` on http | `/health` on `:10428` |
| securityContext | runAsNonRoot 65534, RO rootfs, drop ALL | identical |
| Scheduling | `gardenio.nodeSelector`/`gardenio.tolerations` helpers | identical |
| Ephemeral fallback | `persistence.enabled=false` → emptyDir | identical (throwaway test ns) |

**Port note (corrects the task brief's "OTLP 4318/4317, select 10428"):** that
split is the *cluster*/ECS-collector convention. A **single-node** VictoriaTraces
exposes one HTTP listener `:10428` that serves **both** OTLP/HTTP (`POST
/insert/opentelemetry/v1/traces`) **and** the Jaeger/native select API
(`/select/jaeger/...`, `/select/vmui`). Verified live against perf. OTLP/gRPC on
`:4317` is a *separate, optional* listener you must turn on with
`-otlpGRPCListenAddr`. So the Service exposes `:10428` (named `http`) by default,
and `:4317` (named `otlp-grpc`) only when `vtsingle.otlpGRPC.enabled`. gardenio
self-instrumentation (Go OTEL SDK, fast-follow) will use OTLP/HTTP to `:10428`,
so gRPC stays off for v1.

**Disk-cap safety flags (v0.8.0 has these — verified in perf `/metrics`):**
`-retention.maxDiskSpaceUsageBytes` and `-retention.maxDiskUsagePercent` are
available. Wire `-retention.maxDiskSpaceUsageBytes` to ~80% of the PVC so a
runaway k6 load test trims oldest partitions instead of filling the volume and
wedging the pod. Defaults are `0` (disabled) on perf today; gardenio should set
it because the gardenio VT is shared with gardenio's own spans.

## 3. Transport — PULL/RELAY (recommended) vs PUSH

The task names two ways to get perf spans into the gardenio VT. Same trade-off
the metrics epic resolved ([[ka-0000004560]] chose pull-federation): the
**substance** is a real copy in gardenio; the question is wire direction and who
must change.

| | **PULL / RELAY (recommended)** | **PUSH (fallback)** |
|---|---|---|
| Mechanism | New in-chart otel-collector (or small relay) reads perf VT `/select/jaeger/api` over the public `perf.acme.example` host with basic auth, exports OTLP → in-chart VT `:10428` | Add a **second OTLP exporter** on the *perf* env otel-collectors pointing at a gardenio VT public ingress (cross-account into acct 234567890123), mirroring the vmsingle **W1** remote-write receiver (Traefik IngressRoute + basicAuth + IP allow-list) |
| Who changes | **gardenio chart only** — entirely self-service | **perf-side terraform** (human lands TF in the perf/site account; gardenio never writes perf TF) **+** a new public write ingress on gardenio |
| New attack surface | none new (gardenio egresses out; perf VT already public+authed) | **public unauthenticated-TSDB-behind-basic-auth write endpoint** on gardenio (same risk class as W1) |
| Auth | reuses perf VT basic-auth creds via an ExternalSecret (see §3.1) | needs an ingest-credential ExternalSecret (`vtsingleIngest`, mirror `vmsingleIngest`) **and** perf-side exporter creds |
| Coupling to perf | reads a stable public query API; resilient to perf collector config | depends on perf otel-collector keeping a second exporter healthy |
| Failure blast radius | gardenio-local; if relay dies, perf is untouched | a gardenio ingress outage backs up / drops perf's export pipeline |
| Backfill | relay can re-pull a time window from perf's 7d retention | push is fire-and-forget; a gardenio outage loses spans in the gap |

### Recommendation: PULL/RELAY

Rationale, in priority order:

1. **Self-service, no perf-side change.** PUSH requires a human to land
   terraform in the perf account (gardenio never writes perf TF — same constraint
   that pushed the metrics epic to pull). PULL lives entirely in this chart and
   the gardenio account, so the epic can ship e2e without a cross-team TF gate.
2. **No new public write surface on gardenio.** VictoriaTraces, like vmsingle,
   has no native auth and a destructive admin/insert surface. PUSH means
   standing up a W1-style public ingest IngressRoute in front of an
   unauthenticated TSDB — acceptable but heavy, and a standing liability. PULL
   adds zero inbound surface: gardenio egresses to perf's already-public,
   already-authenticated query API.
3. **Decoupled failure modes + backfill.** A relay pulling perf's 7d-retained
   API can re-pull after a gardenio outage; a push exporter drops spans during a
   gardenio ingress blip and can back-pressure perf's own pipeline.
4. **Consistency with the sibling epic.** Operators already reason about
   "gardenio pulls perf" for metrics; traces following the same direction keeps
   one mental model and one credentials pattern.

**When PUSH wins (documented fallback):** if perf ever firewalls its public VT
query API, or if the relay's pull cadence proves too lossy for high-volume k6
bursts (OTLP push is lower-latency and lossless under load), flip to PUSH. The
W1 receiver scaffolding (`vmsingle.ingest.*` IngressRoute + basicAuth +
ipWhitelist + rateLimit middlewares) is the proven template to copy as
`vtsingle.ingest.*`. Keep that path designed-but-disabled.

> **Two-step reachability when you flip PUSH on (bug ka-0000004920).** On
> prod-gardenio-eks the shared `web`/`websecure` entrypoints are served only by
> the *internal*-scheme `traefik` NLB behind AWS Verified Access, so an
> out-of-cluster collector cannot reach them. Both halves must change together:
> 1. `vtsingle.ingest.entryPoints: ["webhooks"]` — bind the internet-facing edge.
> 2. Add the ingest host to terraform `webhook_edge_hostname` — external-dns
>    sources only Service/Ingress, never IngressRoute CRDs, so an entrypoint flip
>    alone resolves NXDOMAIN. The host must also be covered by a cert Traefik
>    serves (the route pins one via `tls.secretName`).
>
> `webhooks` is internet-facing and VictoriaTraces has no auth of its own — keep
> `ingest.auth.enabled=true` and turn on `ingest.ipWhitelist` before binding it.

#### Relay mechanism detail

VictoriaTraces has **no built-in "pull-and-replay"** like vmagent's `/federate`
scrape — `/federate` is a metrics concept. So the relay is a thin component, two
viable shapes:

- **(a) otel-collector with a Jaeger-query-style source** — a collector pod
  configured to read perf's Jaeger API and OTLP-export to the in-chart VT. Most
  idiomatic; reuses the OTEL ecosystem the gardenio instrumentation work will pull
  in anyway.
- **(b) tiny custom poller** — a sidecar that paginates perf
  `/select/jaeger/api/traces?service=...` on an interval and POSTs OTLP to the
  in-chart VT `:10428/insert/opentelemetry/v1/traces`. Minimal deps, but bespoke.

**Recommend (a)** and let the ingest implementation task ([[ka-0000004875]])
pick the exact collector image/config; the store + transport *direction* is what
this design fixes. (Open follow-up for the gardener: confirm OTEL Collector has a
receiver that reads the VT/Jaeger query API directly — if not, (b) is the
fallback shape. Either way the wire direction is PULL.)

> **RESOLVED → shape (b), the tiny poller** ([[ka-0000004875]]). The §3 spike
> confirmed the OTEL Collector has **no receiver that pulls from a Jaeger query
> API** — its `jaeger` receiver is push (gRPC/thrift) only — so shape (a) is not
> buildable. Implemented as the **`perf-traces-relay` subcommand of the
> controller binary** (`controller/internal/perftracesrelay/`,
> `controller/cmd/controller/perf-traces-relay`), reusing the existing controller
> image rather than adding a new one. The relay polls
> `/select/jaeger/api/{services,traces}` on the perf VT, converts Jaeger JSON →
> OTLP protobuf, and POSTs to the in-chart vtsingle
> `:10428/insert/opentelemetry/v1/traces`. Chart wiring: a default-off
> `vtsingle.relay` Deployment (`templates/vtsingle/relay-deployment.yaml`) gated
> on `vtsingle.enabled` + `vtsingle.relay.enabled`; source basic-auth via the
> `externalSecrets.perfTracesIngest` ExternalSecret (§3.1, mounted as files).

### 3.1 Auth — reuse the perfVmFederation ExternalSecret pattern

Mirror `externalSecrets.perfVmFederation` ([[ka-0000004560]] / `external-secrets.yaml`):
add `externalSecrets.perfTracesIngest` materializing a K8s Secret (default name
`gardenio-perf-traces`) with `username`/`password` keys from the prod SM store
(the same `VICTORIA_TRACES_PERF_*` basic-auth pair perf already accepts),
mounted on the relay pod as files it reads for the perf source's basic_auth.
Gated on `vtsingle.enabled` + relay enabled + a non-empty `remoteRef`; leave
`remoteRef` empty to skip (operator provisions out-of-band). Category `rotated`.

```yaml
externalSecrets:
  perfTracesIngest:
    remoteRef: ""               # SM key holding the perf VT basic-auth pair
    usernameProperty: "username"
    passwordProperty: "password"
    secretName: "gardenio-perf-traces"
```

The ExternalSecret resource is a near-verbatim copy of the perfVmFederation
block in `templates/external-secrets.yaml` (username/password template,
`creationPolicy: Owner`, `rotated` refreshInterval).

## 4. Retention period + PVC size

- **Retention: `7d`.** Match the perf VT source — there's no value retaining
  gardenio's copy longer than perf keeps the originals, and traces are
  high-cardinality, low-shelf-life. (vmsingle keeps 90d for metrics; traces are
  deliberately shorter.)
- **PVC: `20Gi` gp3, RWO** (via `volumeClaimTemplates`, mirroring vmsingle).

Sizing math, grounded in the live probe (§1):

| Input | Value |
|---|---|
| perf VT on-disk size, ~6-day window **incl. k6 load tests** | ~657 MB |
| → projected steady-state 7d perf trace footprint | **< 1 GB** |
| gardenio self-instrumentation spans (fast-follow, controller+kbeads) | budget ~1–2 GB/7d (greenfield, low volume vs k6) |
| Disk-cap trim threshold (`-retention.maxDiskSpaceUsageBytes`) | ~16Gi (≈80% of 20Gi) |

**20Gi gives ~30× headroom over the observed ~657 MB high-water mark.** It
absorbs (a) a much heavier-than-observed k6 load-test burst, (b) gardenio's own
spans once instrumented, and (c) the disk-cap safety margin — while staying a
small, cheap gp3 volume. The disk-cap flag means the pod self-trims oldest
partitions before the PVC fills, so the only failure mode is "oldest spans
drop early," never "pod wedged on a full disk."

## 5. Grafana datasource — Jaeger type

Add a second datasource to `templates/grafana/configmap.yaml` (today it
provisions only `VictoriaMetrics`/prometheus → vmsingle):

```yaml
- name: VictoriaTraces
  type: jaeger
  access: proxy
  url: http://gardenio-victoria-traces:10428/select/jaeger
  uid: victoriatraces
  jsonData:
    tracesToMetrics:
      datasourceUid: <vmsingle-uid>   # optional: span → RED metrics correlation
```

**Why `jaeger`, not a VT-native plugin:**
- VictoriaTraces **implements the Jaeger query API natively** (verified:
  `/select/jaeger/api/services` returns the Jaeger services payload). Grafana's
  `jaeger` datasource is a **core, built-in** type — no `GF_INSTALL_PLUGINS`,
  no image change, no plugin sidecar. The gardenio Grafana image
  (`grafana/grafana:11.4.0`) ships it.
- There is no separate first-class "VictoriaTraces" Grafana datasource plugin to
  install; the supported integration path *is* the Jaeger API. Choosing `jaeger`
  is the VT-native choice, not a compromise.
- It slots into the existing provisioning ConfigMap with one extra list entry,
  gated on `vtsingle.enabled`, exactly like the metrics datasource is gated on
  `grafana.enabled`.

A sample trace dashboard ([[ka-0000005235]]) can use the Jaeger datasource's
service/operation explorer; the `tracesToMetrics` link lets operators jump from
a span to the perf RED metrics already federated into gardenio-vmsingle
([[ka-0000004560]]).

## 6. Rollout

Mirror the metrics epic's gate discipline:
1. **e2e gardenio first** — enable `vtsingle` + relay, provision the perf-traces
   ExternalSecret, validate: perf services appear in the gardenio Grafana Jaeger
   datasource (`keycloak/site-api/sponsor-api/study-web`), spans queryable,
   disk-cap holding under a k6 run.
2. **prod gardenio** — auto-activates on first deploy once the new prod gardenio
   exists (same pattern the metrics overlay uses).

## 7. Open items for the gardener → Matt

1. **Confirm transport = PULL/RELAY** (this doc's recommendation) vs PUSH.
   This is the one gating decision; ingest/scaffold/instrument tasks depend on it.
2. **Relay component choice** (otel-collector reading Jaeger API vs tiny poller)
   — resolvable inside the ingest task once direction is fixed; surfaced here so
   the gardener knows there's a small spike if OTEL Collector lacks a
   Jaeger-query *receiver*.
3. **gardenio self-instrumentation sequencing** — stub the VT pod + perf ingest
   first, instrument gardenio (controller + kbeads OTEL SDK) as a fast-follow
   (epic's stated preference). This doc assumes that order.

---

*Grounded in: advice [[ka-0000000864]] (perf VT endpoints + `$VICTORIA_TRACES_PERF_*`),
sibling metrics design `obs-perf-to-gardenio-vm-federation.md` ([[ka-0000004560]]),
W1 receiver `obs-w1-site-remote-write.md`, and live probes of
`perf.acme.example/victoria-traces` on 2026-06-16.*
