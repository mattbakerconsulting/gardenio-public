# Edge-IDP L0 transport decision — Traefik access logs → VictoriaLogs

**Status:** Accepted · **Date:** 2026-08-08 · **Layer:** L0 (telemetry) of the
edge intrusion-detection program (design of record `ka-0000007287`, epic
`ka-0000008216`, task `ka-0000001563`).

This is the in-repo engineering record for the **sink decision + volume
measurement + transport** that L0 called for. The architecture narrative and the
L1–L3 layers live in the design of record — read that first for the *why*.

---

## What L0 needs

MR `!7042` turned the Traefik edge **JSON access log** ON
(`terraform/modules/platform/traefik.tf`, `logs.access` → the container's
stdout). L0's job is to get that stream **somewhere queryable** so L1 enrichment
and L2/L3 detection have a per-request record of who touches the public edge.
Two open questions, per the acceptance criteria:

1. **Sink** — where do the logs land?
2. **Volume** — QPS / retention, measured not guessed (the design forbids
   choosing a sink or retention against a guess).

## Volume measurement (measured live, 2026-08-08)

Traefik exposes Prometheus request counters (`traefik_entrypoint_requests_total`)
even though the access-log body itself is not scraped. Measured off the live prod
Traefik pod (`traefik` namespace, `prod-gardenio-eks`):

| Metric | Value |
|---|---|
| Lifetime requests (19.2 d uptime) | 211,963 |
| **Average rate** | **0.13 req/s ≈ 11,020 req/day** |
| Live 30 s window | 0 (bursty, low) |
| Status mix | 2xx 75.1%, 3xx 12.6%, 4xx 11.5%, 5xx 0.9% |
| **Non-2xx fraction** | **24.9%** — the SIGNAL; never sampled |

Sizing (raw, uncompressed, ~700 B/record):

| Horizon | Raw bytes | % of the existing 10 Gi vlsingle PVC |
|---|---|---|
| 1 day | 7.7 MB | — |
| **30 days** | **231 MB** | **2.16 %** |

At the current rate it would take **~46× more traffic** to fill the existing
10 Gi PVC in 30 days *raw* — and VictoriaLogs typically compresses log data
10–30×. **Conclusion: no PVC or retention change is needed.** The existing
`vlsingle` sizing (`retentionPeriod: 30d`, `size: 10Gi`, in the canonical-prod
day0 overlay `victoria-logs-values.yaml`, since retired) is more than
adequate; re-confirm from the first real day of stored logs (measure-two-ways).

## Sink decision — VictoriaLogs

`gardenio-victoria-logs.gardenio-system:9428` (the in-chart `vlsingle` store,
already live and empty-but-ready). Rejected alternatives:

| Candidate | Verdict |
|---|---|
| **VictoriaLogs** *(chosen)* | Already deployed + paid for; `/insert/jsonline` accepts the Traefik JSON shape directly; it is the store the L1–L3 detections and the Grafana geomap already target. |
| stdout captured by pod-log collection | **Not available** — there is NO log-collector DaemonSet (fluent-bit/vector/promtail) in the cluster (verified live). "Captured by pod-log collection" describes infra that does not exist. |
| e2e metrics system (vmsingle) | Wrong store — metrics counters carry no client IP / path / UA; that is exactly the gap L0 exists to close. |
| Loki / S3 | New stateful infra for a stream that already has a home; strictly more work. |

## Transport decision — a small stdout streamer (`traefik-log-shipper`)

A purpose-built Go Deployment (`gardenio/controller/cmd/traefik-log-shipper`,
logic in `internal/traefikshipper`) in `gardenio-system` that:

1. opens a **follow** stream of the single Traefik pod's stdout via the
   Kubernetes pod-log API (`pods/log?follow`), re-resolving the pod by label
   selector on each reconnect;
2. keeps only JSON **access** records (positive match on
   `DownstreamStatus`/`RequestMethod`), dropping Traefik's own runtime chatter;
3. POSTs them to VictoriaLogs `/insert/jsonline`, reusing the **exact**
   `idpshipper.HTTPSink` (now `internal/vlsink`) and the sanctioned poll/stream → transform → sink shape.

Rejected transport alternatives:

- **Traefik-pod sidecar** — Traefik is a **terraform-managed cluster addon**
  (`helm_release` in `modules/platform`), not part of the gardenio app chart, and
  the `traefik` namespace has no `regcred` to pull a private gardenio image. A
  sidecar there means editing the addon's values AND provisioning pull creds in a
  second namespace — a much larger blast radius than a Deployment in
  `gardenio-system`, where `regcred` and the VictoriaLogs Service already live.
- **Node log-collector DaemonSet** — none exists; standing up a cluster-wide
  agent for one low-volume stream is disproportionate.
- **CloudWatch hop** (the `idp-log-shipper` shape) — that hop exists only because
  the EKS-audit *source* is CloudWatch. The edge source is a pod's stdout,
  already in-cluster; routing it out and back is pure overhead.

### Sampling policy (non-negotiable, `ka-0000007287#c3`)

The filter keys ONLY on "is this a JSON access record?", **never on status
code**. Non-2xx (probes, scans, auth failures) are the signal and ship in full.
A regression test (`TestNeverSampleNon2xx`) locks this in.

### At-least-once

`streamSinceSeconds` bounds the re-read window on each (re)connect so the
disconnect gap is covered; a sink failure surfaces an error (Run reconnects)
rather than dropping the batch. A bounded boundary duplicate is preferred to a
silent gap — the same stance `idpshipper` (now `vlsink`) takes.

## Verification (live prod, 2026-08-08)

Proven end-to-end against the live cluster, not just unit tests:

- **Sink half:** POSTed a synthetic Traefik record to the live
  `gardenio-victoria-logs:9428/insert/jsonline` and read it back with LogsQL —
  `request_X-Forwarded-For` preserved.
- **Transport half:** ran the compiled `traefik-log-shipper` against the live
  Traefik pod-log stream + live VictoriaLogs (in-cluster SA). Real access records
  landed and were LogsQL-queryable, with `_msg`=RequestPath, `DownstreamStatus`,
  `ClientHost`, `request_X-Real-Ip`, `request_User-Agent`, `RouterName`,
  `_time`=StartUTC, and `_stream` partitioned by `entryPointName`.

This caught one real bug the synthetic fixtures missed: a live Traefik v3 access
record ALSO carries a top-level `"level":"info"` field, so an early filter that
dropped any JSON with a `level` field would have dropped **every** real record.
Fixed to key on the positive presence of access fields; `TestAcceptLine` now
pins the real prod shape.

## Ingest field mapping

`/insert/jsonline?_stream_fields=entryPointName&_time_field=StartUTC&_msg_field=RequestPath`

- `_stream_fields=entryPointName` — low cardinality (web/websecure/ziti/webhooks),
  so streams don't explode.
- `_time_field=StartUTC` — the LogsQL `_time` window matches when the request
  happened, not when it was shipped.
- `_msg_field=RequestPath` — a bare LogsQL word-search hits the path analysts scan
  first.

## Rollout

`traefikLogShipper` ships **disabled by default** in the chart. Enabling it in
prod is a chart-values flip (no terraform change): set
`traefikLogShipper.enabled=true` alongside the already-live `vlsingle`. RBAC is a
cross-namespace Role/RoleBinding in the `traefik` namespace bound to the
`gardenio-system` ServiceAccount (read-only `pods` + `pods/log`).

**Out of scope (unchanged gates):** this does NOT enable `vmalertLogs` — that
evaluator stays gated per `prod-real.tfvars`; edge logs satisfy the store+transport
gate for EDGE detections only, and the EKS Phase-2 CloudWatch→VL transport
(`ka-0000007889`) is a separate unmet gate.
