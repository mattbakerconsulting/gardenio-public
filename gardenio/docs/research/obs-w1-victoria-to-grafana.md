# W1 — Site-deployment Victoria → gardenio Grafana plumbing

> Phase 0 design doc for epic [[ka-0000006776]] (workstream W1, task [[ka-0000003078]]).
> **Status: design only.** No plumbing is applied and no chart change is merged until the
> Phase-1 decision gate ([[ka-0000004494]]) is approved. (Gate resolved `approve_all`
> 2026-06-09; this doc is the basis Phase 1 builds on.)

## 1. Problem

gardenio now runs an in-chart Grafana (`/grafana`, AVA header-auth proxy) wired to an
in-chart `vmsingle`. Today that vmsingle holds only `gardenio_*` / `beads_*` /
`kbeads_*` series scraped by the in-chart `vmagent`. The operator wants
**site-deployment (ACME, ECS) Victoria signals** — metrics, and eventually logs/traces —
to reach gardenio Grafana so we can iterate on ACME dashboards in one place.

The hard constraint (inherited from centralization epic [[ka-0000001783]]):
**config-flip, not refactor.** Site-deployment must not grow a hard dependency on the
gardenio EKS cluster being up. If gardenio is down, ACME observability keeps working
unchanged.

## 2. What already exists (substrate)

gardenio side (`gardenio/helm/gardenio/`):

- `templates/vmsingle/` — single-node VictoriaMetrics, `:8428`, `/api/v1/write` ingest.
  Values `vmsingle.*` (`values.yaml:2707`): retention + `persistence.size`.
- `templates/vmagent/` — scrapes pods with `prometheus.io/scrape=true`; remote-writes to
  vmsingle. Scrape config `templates/vmagent/configmap.yaml` already carries a **PII
  `labeldrop` backstop** (`oidc_email|slack_user_id|email|...`) and stamps
  `external_labels: {env, cluster}`.
- `templates/vmalert/` — recording rules only today (epic [[ka-0000006536]]).
- `templates/grafana/` — Grafana 11.4, sidecar (`grafana_dashboard="1"`), datasource
  auto-wires to vmsingle. Values `grafana.*` (`values.yaml:2828`).

site-deployment side (terraform/terragrunt, **not in this repo** — confirmed: no
`repos/` clone present; facts below are from the epic framing + FSM/alerting research docs
`docs/FSM_CORRELATION_CONTRACT.md`, `docs/research/canonical-prod-alerting-catalog.md`):

- Per-env VictoriaMetrics / VictoriaLogs / VictoriaTraces + `vmalert → Alertmanager`
  (`terraform/live/*/common/services/...`, `tg-templates/services/{alertmanager,vmalert}`).
- MR !879 precedent: a **second OTEL traces exporter** fanning dev traces to a devops
  VictoriaTraces — i.e. cross-env fan-out by adding an exporter, not by re-architecting.

## 3. Options for getting ACME metrics into gardenio vmsingle

Three transport shapes, evaluated against the config-flip constraint.

### Option A — vmagent remote-write fan-out (push, from site-deployment) ✅ recommended

Add a **second `-remoteWrite.url`** to each env's existing site-deployment vmagent (or a
dedicated fan-out vmagent), pointing at a gardenio-side ingest endpoint. This mirrors the
MR !879 traces-exporter precedent exactly, one signal up.

```text
site vmagent ──remote_write #1──▶ site vmsingle      (unchanged, primary)
            └─remote_write #2──▶ gardenio ingest ──▶ gardenio vmsingle
```

- **Config-flip:** ✅ adding/removing the second URL is a terragrunt var; site keeps its
  primary write. gardenio down ⇒ vmagent buffers then drops the *secondary* queue; primary
  unaffected.
- **Selectivity:** `writeRelabelConfigs` on the second write keeps only the series we want
  (e.g. `keep` a ACME allow-list), capping cardinality/cost on gardenio vmsingle.
- **Auth:** bearer/basic on the remote-write (`victoria_auth_secret_name` pattern).
- **Cons:** site-deployment owns the config; we coordinate a terragrunt change per env.

### Option B — Prometheus/VM federation (pull, from gardenio)

gardenio vmagent scrapes `/federate` (or VM `/api/v1/export`) on each site vmsingle.

- **Config-flip:** ✅ purely a gardenio-side scrape job; zero site change.
- **Cons:** gardenio must reach *into* each env (network path is gardenio→site, the harder
  direction); federation is a snapshot/scrape, lossy for counters and coarse for high-res
  series; gardenio must hold long-lived cross-env credentials. Inverts the trust direction.

### Option C — OTLP fan-out at the collector (push, OTel-native)

For traces/logs: add a second OTLP exporter on the site OTel collector → gardenio OTLP
endpoint (VictoriaTraces/VictoriaLogs ingest). This is the literal MR !879 shape.

- **Config-flip:** ✅ one exporter entry.
- **Scope:** best for **traces/logs** (phase 2), not the metrics-first MVP.

**Recommendation:** **Option A for metrics (MVP), Option C for traces/logs (later).**
Both are "add one exporter/remote-write target," both keep site-deployment authoritative,
both fail safe for gardenio outages. Reject Option B: wrong trust/network direction and
worse fidelity.

## 4. Which envs / signals first

Phased, smallest blast radius first:

| Phase | Env(s) | Signal | Rationale |
|---|---|---|---|
| 1 | `devel` (or `qa`) | metrics only | non-prod, no PHI risk, validate path + cost |
| 2 | `perf` | metrics | load-shaped data to size vmsingle cardinality |
| 3 | `prod` | metrics | only after PII/PHI sign-off + cost budget confirmed |
| 4 | per-env | logs/traces (OTLP, Option C) | separate decision; VLogs/VTraces sizing |

Metrics first because they are the lowest-PII signal and the cheapest to cap with
`writeRelabelConfigs`.

## 5. Network + auth path (ECS → EKS)

site-deployment runs on **ECS**; gardenio vmsingle runs on **EKS**. The push (Option A)
needs ECS tasks to reach an EKS-hosted ingest endpoint. Three candidate paths, in
preference order:

1. **Private (VPC peering / Transit Gateway / PrivateLink)** — ingest exposed via an
   internal NLB; ECS security group → EKS SG on `:8428`. No public exposure of the write
   API. Preferred for prod. **Open question for the gate:** which of peering/TGW/PrivateLink
   already exists between the ACME ECS VPCs and the gardenio EKS VPC. (Not discoverable from
   this repo — needs site-deployment terraform.)
2. **Public ingress + auth** — vmsingle write behind the existing AVA/ingress with
   bearer-token auth (`victoria_auth_secret_name`), TLS. Simplest for non-prod (devel/qa).
   Note: gardenio `vmagent` egress NetworkPolicy currently blocks RFC1918 ranges
   (`templates/networkpolicies/`) — that's egress; **ingest** is a separate ingress rule we
   add.
3. **OTLP via existing collector hop** — only relevant for Option C.

MVP (devel): start with path 2 (public ingress + bearer). Promote to path 1 before prod.

## 6. PII / PHI

**Do not duplicate** the PII-stripping work in [[ka-0000001783]] — compose with it.

- Primary defense is at the **source**: site-deployment's existing/centralized PII strip
  (ka-0000001783) runs before fan-out. ACME series arriving at gardenio must already be
  scrubbed.
- Defense-in-depth at gardenio ingest: extend the existing `metric_relabel_configs`
  `labeldrop` backstop (already in `vmagent/configmap.yaml`) to the ingest path / a
  relabel rule on the write endpoint, dropping any forbidden label that slips through.
- **Metrics-first** is itself a PII mitigation: metric *labels* are bounded and reviewable;
  logs/traces (phase 4) carry free-text and need the full ka-0000001783 pipeline before
  they fan out.

## 7. Retention / cardinality / cost

gardenio vmsingle is sized today for `gardenio_*`/`beads_*` only. Adding ACME series:

- **Gate inbound series** with `writeRelabelConfigs: keep` (an explicit ACME allow-list),
  not "everything." This is the single biggest cost lever.
- **Separate `env` label** already stamped — ACME series land under their own `env`, so
  dashboards and retention policy can target them without colliding with gardenio series.
- Re-size `vmsingle.persistence.size` / retention as a values change once phase-2 (`perf`)
  shows real cardinality. Track as a Phase-1 sub-task; don't pre-size blind.
- Consider a **separate vmsingle instance** (or VM cluster) for ACME data if cardinality
  dwarfs gardenio's — keeps gardenio self-monitoring isolated. Flagged as open question.

## 8. Recommended approach (summary)

1. **Metrics, push, Option A.** Add a second remote-write on the site-deployment vmagent,
   ACME-allow-listed, bearer-authed, to a gardenio vmsingle ingest endpoint.
2. **devel first**, public-ingress+bearer; promote to private path before prod.
3. **PII:** rely on ka-0000001783 at source; keep gardenio-side `labeldrop` backstop.
4. **Cost:** allow-list inbound series; re-size vmsingle after `perf` phase.
5. **Traces/logs later** via Option C (OTLP second exporter), separate decision.

This is a config-flip on both sides — no new binary, no refactor — which is why W1 is the
lowest-risk workstream and a good first Phase-1 step (see W4 synthesis).

## 9. Open questions for the decision gate

- ECS↔EKS private connectivity: peering/TGW/PrivateLink — which exists? (needs site TF)
- Single shared gardenio vmsingle vs. a dedicated ACME-data instance?
- Exact ACME metric allow-list for MVP (which `__name__` prefixes)?
- Does ka-0000001783's strip already cover everything we'd fan out, or is there a gap for
  metric labels specifically?

## 10. Related

- Epic [[ka-0000006776]] · gate [[ka-0000004494]]
- Centralization + PII: [[ka-0000001783]] (compose, do not duplicate)
- gardenio Grafana epic [[ka-0000007895]] · vmalert epic [[ka-0000006536]]
- FSM read-time federation contract: `docs/FSM_CORRELATION_CONTRACT.md`
- Sibling designs: W2 `obs-w2-grafana-bridge.md`, W3 `obs-w3-alertmanager-bridge.md`,
  W4 `obs-synthesis-rollout.md`
