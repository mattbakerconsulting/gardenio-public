# IDP transport-architecture decision (ADR)

**Status:** Accepted (locked) · **Date:** 2026-08-02 · **Phase:** 0 — Foundations

How the EKS control-plane **audit log** (already emitted to CloudWatch) is bridged
into the alertable VictoriaMetrics/VictoriaLogs observability plane for the
intrusion-detection program (IDP) that remediates audit finding **F1**
(`ka-0000000578`, *EKS API public endpoint open to `0.0.0.0/0` on
`prod-gardenio-eks`*).

This ADR is the in-repo engineering record for the transport fork. The full
architecture narrative, detections catalog, and RC1-safety analysis live in the
**design of record** — read that first if you need the *why*:

- **Design of record (authoritative):** `ka-0000003550` · bundle `ka-0000007676`
  (<https://gardenio.example.com/beads/bundle/ka-0000007676>)
- **Operator decision (the fork, filed for the record):** `ka-0000000935`
- **Plan (`/plans`):** `ka-0000007128` — Phase 0 → Phase 1 metrics-first → Phase 2
  VictoriaLogs plane → Phase 3 harden & validate
- **This foundations epic:** `ka-0000000178`
- **Operator runbooks:** [`intrusion-detection-runbook.md`](./intrusion-detection-runbook.md)
  (umbrella), [`eks-api-intrusion-runbook.md`](./archive/eks-api-intrusion-runbook.md)
  (Phase-1 alert-group card)

---

## Context

Everything the IDP alerts on already exists in the EKS **audit** log group
(`/aws/eks/prod-gardenio-eks/cluster`, CloudWatch, 30 d). The **gap is transport**:
nothing carries those records from CloudWatch into anything the alerting plane can
evaluate.

Two grounding facts constrain the choice:

- The alert → Slack path is already proven and live: `PrometheusRule`/`VMRule` →
  `gardenio-vmalert` → `gardenio-alertmanager` → Slack. (An earlier draft listed
  `alertmanager-bridge` in this chain; Alertmanager posts to Slack directly, and that
  bridge was deleted 2026-10-31, gd-Y4ZCGr2m2t.)
- VictoriaMetrics (`vmsingle`/`vmagent`/`vmalert`) + VictoriaTraces run;
  **VictoriaLogs was not deployed** — so the log-native detections need a new store
  either way.

**Already decided upstream (NOT this ADR):** we do **not** touch
`endpoint_public_access_cidrs`. Narrowing that CIDR froze golden promotion for
three tags (RC1, `ka-0000004055` / `ka-0000002512`). The IDP is a *detective*
control whose known-good egress set is a **soft alerting input**, so a wrong entry
degrades to a false-positive alert — never a locked-out CI runner. See the design
of record for the full RC1-safety argument.

## The fork — how to bridge CloudWatch → alertable plane

| Option | Transport | New stateful infra | Time to first alert | End state |
|---|---|---|---|---|
| **opt1 — metrics-first, two-phase** *(chosen)* | CloudWatch **metric filters** → cloudwatch-exporter (IRSA) → existing `vmsingle` for Phase 1; VictoriaLogs raw-log path added in Phase 2 | **none** for Phase 1 (reuses `vmsingle`); `vlsingle` in Phase 2 | **shortest** — a paged control ships in the first iteration | metric alerts **+** LogsQL detections + forensics |
| opt2 — logs-first via Vector | Deploy VictoriaLogs **now** + a Vector deployment (native `aws_cloudwatch_logs` source → VictoriaLogs sink); all detections authored as LogsQL from day one | `vlsingle` + Vector, before any control exists | slower — no alert until the whole log pipeline + a new store are up | same |
| opt3 — logs-first via Kinesis Firehose | CloudWatch **subscription filter** → Kinesis Firehose → transform → VictoriaLogs `/insert/jsonline` | `vlsingle` + Firehose + transform + its IAM | slowest — heaviest new infra before first alert | same |

All three converge on the *same* end state (metric alerts + LogsQL detections +
forensics); they differ only in **order** and **first infra stood up**.

## Decision

**opt1 — metrics-first, two-phase. Locked.**

Rationale:

1. **Fastest paged detective control in front of F1.** Phase 1 reuses the entire
   proven alert path and adds **zero new stateful infra** — CloudWatch metric
   filters over the audit group emit per-event counts, the in-chart
   cloudwatch-exporter pulls them into the existing `vmsingle`, and a new
   `PrometheusRule` fires detections #1 (anonymous / 401-403 reach), #2
   (auth-failure spike), #4 (sensitive verbs) through the live
   vmalert → Alertmanager → bridge → Slack path.
2. **Still delivers the operator's "victoriametrics logs" vision.** Phase 2 then
   stands up `VictoriaLogs (vlsingle)` and the raw-log shipper for the richer
   log-native detections (#3 unknown-source-IP, #5 impossible-travel, #6
   enumeration) + forensics — the logs-first end state, reached second instead of
   first.
3. **Lowest risk per increment.** No component blocks the first alert; each phase
   is independently shippable and reviewable.

opt2/opt3 are logs-first: a cleaner single-system end state, but **no alert fires
until a brand-new store and a full log pipeline are stood up** — strictly slower to
close the detection gap, with more new infra ahead of the first control. Both are
subsumed by opt1's Phase 2 (opt2's Vector source and opt3's Firehose remain the two
candidate transports *within* Phase 2 — see "Phase-2 sub-decision" below).

## Consequences — IAM (the load-bearing prerequisite)

The IAM surface is **split by phase**, each grant least-privilege and bound to one
ServiceAccount by an IRSA `StringEquals` trust on a single subject:

| Phase | Workload | Actions | Resource | Terraform |
|---|---|---|---|---|
| **1 (live)** | cloudwatch-exporter pod | `cloudwatch:GetMetricStatistics`, `cloudwatch:ListMetrics` | `*` (CloudWatch read actions have no resource-level ARN) | `cloudwatch_exporter_irsa.tf` (merged, `ka-0000002106`) |
| **1 (apply)** | terraform (operator identity) | `logs:PutMetricFilter` on the audit group | audit log group | metric-filter resources (`ka-0000005580`) — created *by* the apply, no in-cluster role |
| **2** | audit-log shipper pod | `logs:FilterLogEvents`, `logs:GetLogEvents`, `logs:DescribeLogStreams` | **`/aws/eks/prod-gardenio-eks/cluster` only** (+ its `:*` stream glob) | `idp_log_reader_irsa.tf` (this MR — count-gated, disabled by default) |

Key facts:

- **Phase 1 needs no `logs:*` at all.** The exporter reads *metrics*, not log
  events. This is why the fast path could ship without ever touching the
  log-read grant.
- **Phase 2 is the only workload in the whole IDP that reads raw audit log
  events.** Those three `logs:*` verbs are exactly the ones the shared
  `gardenio-prod-agent` role is **denied** on the audit group today (proven live in
  the design of record) — so the grant is a genuine, scoped addition, not a
  widening of an existing broad role.
- The Phase-2 role is drafted now (`idp_log_reader_irsa.tf`) so the grant is not a
  late surprise, but it is **count-gated off by default** — a default
  `terraform plan` creates **zero** resources. Phase 2 activates it in one reviewed
  apply by setting `idp_log_reader_irsa_enabled = true` and pinning
  `idp_log_reader_sa_name` to the shipper's ServiceAccount.

## Phase-2 sub-decision — RESOLVED (transport within Phase 2)

The *transport within Phase 2* was originally framed as Vector native
`aws_cloudwatch_logs` source (opt2) vs CloudWatch subscription-filter → Firehose
(opt3) → VictoriaLogs. **Building it (task `ka-0000007889`) surfaced a
load-bearing correction to that framing:**

> **Vector has no `aws_cloudwatch_logs` *source*.** Vector only ships a CloudWatch
> *sink* (writes **to** CloudWatch); its sole way to *ingest* CloudWatch logs is
> the **push** model — a CloudWatch subscription-filter → Kinesis Firehose →
> Vector's `aws_kinesis_firehose` HTTP source. So "opt2 = Vector native pull
> source" was never a real option; a Vector-based transport collapses into opt3's
> Firehose infra.

Meanwhile the log-read IAM this ADR drafted (`idp_log_reader_irsa.tf`) grants the
**pull** verbs — `logs:FilterLogEvents` + `GetLogEvents` + `DescribeLogStreams` —
which a Firehose/push transport does **not** use (Firehose needs a
subscription-filter + `firehose:PutRecord` + an S3 dead-letter bucket). So the
already-merged IAM foundation had **already committed to a pull transport**.

A pull shipper is therefore the transport. Two pull options were weighed:

- **An OpenTelemetry Collector (contrib) with `awscloudwatchreceiver`.**
  Off-the-shelf and matches the pull IAM, BUT its pipeline **cannot host the
  source-IP log-field contract** (below): that contract must **expand one
  multi-IP audit record into N rows** — one per source IP — which OTTL cannot do,
  and it is proven end-to-end against VictoriaLogs' `/insert/jsonline`, not the
  OTLP endpoint an OTel exporter uses. Rejected on that mismatch.
- **A purpose-built Go shipper (chosen).** `gardenio/controller/cmd/idp-log-shipper`
  polls the audit group via `FilterLogEvents` (exactly the three granted verbs,
  named group so no `DescribeLogGroups`), runs each record through
  `internal/auditxform.EnrichJSONLine` — the *source of truth* for the log-field
  contract, built and e2e-proven in `ka-0000006176` explicitly so "the transport
  build consumes it" — and POSTs the enriched newline-delimited JSON to
  VictoriaLogs' `/insert/jsonline`. This is the only option that satisfies the
  contract (scalar `client_ip` + per-IP expansion) *and* the pull IAM, with **no
  Firehose / S3 / subscription-filter infra** and no OTTL limitation. It ships at
  the fleet calver as a monorepo image (`.rwx/docker.yml` `push-idp-log-shipper`),
  so there is no upstream tag or mirror entry to track.

Chart: the off-by-default `idpLogShipper` component
(`templates/idp-log-shipper/`, values `idpLogShipper.*`) — a Deployment + SA whose
config is passed by env var; enabled in the same reviewed Phase-2 apply that flips
`idp_log_reader_irsa_enabled=true` and `vlsingle.enabled=true`. The shipper's
ServiceAccount is `gardenio-idp-log-shipper` — the `var.idp_log_reader_sa_name`
default the IRSA trust already pins, so no terraform change is needed to bind them.

Firehose (opt3) remains the fallback **only** if a push/durability requirement
(e.g. cross-account delivery, or buffering audit through S3) later outweighs the
pull shipper's simplicity; it would need new IAM (subscription-filter +
Firehose + S3) on top of — not instead of — the pull grant.

## The source-IP log-field contract (resolved — `ka-0000006176`)

One field-shaping question is **transport-agnostic** and had to be settled before
either candidate transport could satisfy detection #3: how the audit record's
`sourceIPs` (a `[]string`) reaches VictoriaLogs as something `ipv4_range(<CIDR>)`
can filter.

Measured at the VictoriaLogs `/insert` boundary and reproduced against the
chart-pinned VictoriaLogs `v1.52.0` (bundle `ka-0000002825`,
`internal/auditxform/auditxform_e2e_test.go`): a `[]string` stores as ONE opaque
JSON-encoded STRING — `["203.0.113.7"]` lands as the literal text
`"[\"203.0.113.7\"]"` — so `ipv4_range` cannot see the IP inside the brackets and
detection #3's known-good-egress subtraction (`-sourceIPs:ipv4_range(<CIDR>)`) is
a **no-op**. Nested OBJECTS, by contrast, flatten to clean dot-path scalars
(`objectRef.name`, `responseStatus.code`).

**Contract (whichever transport is chosen, it MUST implement this):** before
insert, emit a SCALAR `client_ip` per source IP, and EXPAND a multi-IP proxy
chain to one record per source IP so the allowlist is evaluated independently per
IP (the runbook §6 "any source IP outside the set" semantics). Leave the original
`sourceIPs` array untouched — consumers keep both the raw forensic form and the
scalar queryable form. Fields emitted: `client_ip` (scalar, `""` when the record
had no source IP — a fail-safe that pages rather than silently subtracting),
`client_ip_index`, `client_ip_count`.

The reference implementation lives in `controller/internal/auditxform` (unit- +
e2e-tested against real VictoriaLogs). It was also exposed as a stdin→stdout
JSONL filter for a Vector `exec` transform or a Firehose transform-Lambda; that
binary was deleted on 2026-11-02 (gd-jOl9itDY0D) with the EKS log shipping it
served. The chart's
`eksApiIntrusionLogs.fields.sourceIP` now defaults to `client_ip` (config-only,
no LogsQL rewrite), so the detections consume the scalar the moment the transport
publishes it.

## Status of the operator decision

`ka-0000000935` records the fork for the operator. The fleet has **de facto locked
opt1**: Phase 1 (metric filters, exporter IRSA, `PrometheusRule`, dead-man's-switch,
VictoriaLogs store, LogsQL detections) is already built and merged on the opt1
architecture. This ADR ratifies that at the engineering level; the operator
decision remains the human-facing record and can be resolved to `metrics-first`
to close the loop.
