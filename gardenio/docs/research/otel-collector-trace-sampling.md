# otel-collector: trace sampling design

**Bead**: [[ka-0000004073]] — otel-collector: add tail/probabilistic sampler to traces pipeline
**Status**: design — *no implementation until human approval*
**Related**:
- [[ka-0000004759]] — deep-dive on the 1060 spans/sec offered load (closed)
- [[ka-0000006795]] — sibling: study-web browser-side click-span rate-limit / sampling
- [[ka-0000002683]] — sibling: Keycloak sampler ratio + path filter
- [[ka-0000007133]] — parent investigation
- site-deployment MR !868 — VictoriaTraces sizing bump that surfaced the volume problem

## Goal

Cut "OK" trace volume reaching VictoriaTraces by **80–90%** while preserving 100% of error traces and 100% of latency outliers, without sacrificing module portability for greenfield consumers.

## Context (from ka-0000004759)

The otel-collector traces pipeline today is `[memory_limiter, resource, batch]` only — there is **no sampling layer** in `terraform/new-modules/ecs-services/observability/otel-collector.tf:165-176`. Every span emitted by backend services (sponsor-api, site-api, ~10–30 spans amplification per request from FastAPI + SQLAlchemy + HTTPX auto-instrumentation), browsers (study-web fetch / XHR / click / error / lifecycle spans), and Keycloak (`parentbased_always_on`, no path filter) is forwarded unsampled. MR !868 fixed the *capacity* shortfall; this doc proposes a sampling layer to address the *volume* shortfall.

This doc covers **only** the otel-collector tail-sampling layer. Browser-side and Keycloak-source sampling are tracked in sibling beads ka-0000006795 and ka-0000002683 respectively, and remain valuable independent of the collector-side decision documented here. The two layers compose: source-side reduction first, then tail-decision filtering on what makes it to the collector.

## 1. Sampler choice — tail_sampling vs probabilistic_sampler

The OTel Collector ships two relevant processors. The decision is a tradeoff between fidelity-at-decision-time and memory cost.

### `probabilistic_sampler`

- **What**: Hashes `trace_id` (or HTTP attribute) and keeps a fixed fraction.
- **Pros**:
  - Stateless. No per-trace buffering. Memory cost is essentially zero — a single probabilistic decision per span.
  - Simple config (`sampling_percentage: 15`).
  - Coherent across a distributed trace if all collectors hash the same `trace_id` (same root → same decision).
- **Cons** — **the disqualifying ones for our use case**:
  - **Cannot keep all error traces**. Errors are a span-level / trace-level signal that is not known until the trace ends. Probabilistic sampling discards based on `trace_id` alone.
  - **Cannot keep all latency outliers**. Same reason — duration is unknown until the root span ends.
  - We would lose ~85% of error traces, which is exactly the data we need most.
- **When it would fit**: If we wanted a flat 10–20% sample with zero memory cost and no error-bias requirement.

### `tail_sampling` (recommended)

- **What**: Buffers complete trace groups (all spans sharing a `trace_id`) for `decision_wait`. At decision time, evaluates a list of policies; first match wins, OR-of-policies semantics. Drops the group if no policy matches.
- **Pros**:
  - **Trace-level signals available at decision time**: status code, latency, attributes, span count.
  - Composable rule list: `status_code: ERROR`, `latency: 1000ms`, `probabilistic: 15` — express "100% errors + 100% outliers + 10–20% of the rest" exactly as proposed.
  - Predictable shape: defined by policy; not a stochastic miss for errors.
- **Cons**:
  - **Memory cost is real** (sized in section 3 below): holds spans for `decision_wait`.
  - **Trace coherence requires every span of a given trace to land at the same collector instance** before the decision is made. Today our otel-collector runs as a single ECS service per environment, so this is satisfied; if we ever scale out behind a load-balancer that does not pin by `trace_id` (e.g. round-robin gRPC), we will need a `loadbalancing` exporter in front. Flagged as a follow-up risk for the rollout plan, not a blocker today.
  - Slightly later visibility: traces appear in VictoriaTraces ~`decision_wait` after the root span ends.

### Decision

**`tail_sampling`**. The acceptance criteria explicitly require error-bias and latency-bias, which `probabilistic_sampler` cannot provide. The memory cost is bounded and sizable against current prod allocations (section 3). `probabilistic_sampler` is retained as a fallback if tail_sampling memory pressure proves unmanageable on prod (section 5: rollout).

## 2. Sample ratios and policies

The proposed `tail_sampling` config is OR-semantics: a trace is kept if **any** policy fires. Order of policies does not affect retention but affects which `policy.name` label appears on `processor_tail_sampling_count_traces_sampled` metrics, so we order by importance for observability:

```yaml
tail_sampling:
  decision_wait: 10s
  num_traces: 50000
  expected_new_traces_per_sec: 1500
  policies:
    - name: errors
      type: status_code
      status_code: { status_codes: [ERROR] }

    - name: latency-outliers
      type: latency
      latency: { threshold_ms: 1000 }

    - name: ok-baseline
      type: probabilistic
      probabilistic: { sampling_percentage: 15 }
```

### Per-policy intent

| Policy | Type | Setting | Intent | Expected volume |
|---|---|---|---|---|
| `errors` | `status_code` | `[ERROR]` | Keep every failing trace. Errors are the high-signal subset; this is the data we need to debug regressions. | Today errors are <2% of trace volume per ka-0000004759 signal patterns; we expect ~20 traces/sec retained out of ~1060 offered. |
| `latency-outliers` | `latency` | `threshold_ms: 1000` | Keep every trace whose root span exceeds 1s. Captures perf regressions even when the trace status is OK. Threshold is initial; tune after one week of histograms. | Estimated 1–5% of traces; depends heavily on per-tenant workload. |
| `ok-baseline` | `probabilistic` | `15%` | Keep a representative sample of healthy traces for capacity planning, flame-graph baselines, and unknown-unknown investigations. | 15% × ~1040 OK traces/sec ≈ 156 traces/sec. |

### Why 15% (within the 10–20% band)

- **10%** is the floor that still gives operators *some* baseline trace to compare against an error trace from the same endpoint. Below 10%, "show me a successful login flow next to this failing one" frequently misses.
- **20%** is the ceiling justified by VictoriaTraces' new sizing under MR !868. Above that, we are not meaningfully reducing offered load.
- **15%** is the midpoint and is round enough that "we keep ~1 in 7 OK traces" is easy to communicate. We default to 15% in the prod tfvars overlay; per-tenant overlays may dial down to 10% if their span amplification is unusually high (chatty SQLAlchemy footprint).

### Aggregate retention math

Starting offered load: ~1060 spans/sec across all sources (per ka-0000004759: ~129 spans/sec shared-prod-Keycloak, plus 100–400 spans/sec/tenant across active tenants). Trace count is lower than span count because a typical trace contains 10–30 spans (FastAPI server + SQLAlchemy + HTTPX, plus paired browser-side spans). Estimated trace rate: ~50–100 traces/sec offered. Retained:

- ~2% errors (kept) +
- ~3% latency outliers (kept; partial overlap with errors) +
- ~15% of remaining ~95% OK ≈ ~14% (kept)

≈ **~18–19% retention overall**, i.e. ~80% volume reduction. This is the intended outcome.

### Latency threshold provenance

`1000ms` is a starting point, not a tuned value. Two reasons:

1. It comfortably exceeds the p95 of healthy FastAPI request latency in our current stack (typical p95 is <500ms for non-aggregating endpoints, <800ms for joined queries against test data).
2. It is short enough to flag interactive jank (browser perception of "slow" is roughly 1s).

Tune in production after one week by reading `histogram_quantile(0.95, sum by (le) (rate(http_server_duration_seconds_bucket[5m])))` from the spanmetrics-derived metrics, raising the threshold to ~p98–p99 if 5% of healthy traffic is being dragged into the slow bucket.

## 3. Sizing knobs against prod cpu=2048 / memory=4096

### Knobs that matter

| Knob | Default | Proposed prod | Reasoning |
|---|---|---|---|
| `decision_wait` | `30s` | `10s` | Reduces in-memory residency 3×. Most of our traces complete in <2s (FastAPI request lifecycle); 10s is comfortably long enough that root spans have arrived for >99% of traces. Operator visibility cost: traces appear in VictoriaTraces 10s late, which is acceptable for debugging. |
| `num_traces` | `50000` | `50000` | Cap on simultaneously buffered traces. At ~1500 new traces/sec arrival × 10s wait = ~15000 active traces, so 50000 is ~3× headroom. |
| `expected_new_traces_per_sec` | unset | `1500` | Hint for sizing internal data structures. We are at ~1060 spans/sec offered; that maps to ~50–100 traces/sec, but conservatively we set the hint at 1500 to absorb Keycloak-burst patterns. |

### Memory cost estimate

Rough sizing for memory pressure on the otel-collector ECS task (cpu=2048, memory=4096):

- **Per-span overhead in the tail-sampling buffer**: ~1.5–2 KB (proto-decoded `Span` + per-trace policy metadata + indexes). Source: OTel Collector benchmarks and operator reports for ratios of `num_traces × avg_span_per_trace`.
- **Active trace memory** at steady state: 15000 active traces × 20 spans/trace × 2 KB = **~600 MB**. With burst headroom (`num_traces=50000`): worst case ~2 GB.
- **Existing collector overhead** (memory_limiter, batch, exporters, OS, runtime): ~500 MB — 1 GB.
- **Total worst-case**: ~2.5–3 GB of 4 GB. Leaves ~1 GB margin.

This is tight but workable. Two safety nets:

1. The `memory_limiter` processor stays first in the pipeline. If memory crosses its `limit_mib` threshold it sheds load (drops new spans) before the tail_sampling buffer can OOM the task. Configure `limit_mib: 3072`, `spike_limit_mib: 512` to leave room for the tail-sampling working set.
2. If steady-state memory exceeds ~70% sustained, reduce `decision_wait` to `5s` first, then `num_traces` to `25000`. Both are non-disruptive ECS task variable changes.

### CPU cost

Tail sampling adds a per-trace decision evaluation. At ~50–100 traces/sec decision rate, this is <5% CPU; negligible against the existing batch-processor CPU floor. CPU is not the constraint.

### What we will NOT need to bump (initial)

- `cpu=2048` and `memory=4096` are sufficient for the proposed config. If memory pressure shows otherwise in observation week 1, the next step is a memory bump (`memory=6144`) before tuning down `num_traces`, since task-resize is reversible and ratio-tune is the bigger-blast-radius change.

## 4. Module portability rule

**The module's default MUST remain 100% sample (no tail_sampling processor).** Sampling is an opt-in via the prod tfvars overlay.

### Why

Aligns with the project's "portability first; acme-specific in overlays" core rule. Reasons specific to sampling:

1. **Greenfield consumers may want full fidelity.** A small deployment with low span volume gains nothing from sampling and loses debug data. Their VictoriaTraces sizing may not be the constraint; their operator-debugging UX is.
2. **Sampling ratios are workload-dependent.** A consumer running a write-heavy ingest pipeline has a different error/OK ratio than our auth-heavy / browser-heavy mix. Defaults that work for us are not defaults for them.
3. **Latency thresholds are tenant-specific.** `1000ms` is right for our interactive web app; a backend batch consumer might want `30s` or no threshold at all.

### Shape of the overlay

Module exposes a structured variable, defaulting to disabled:

```hcl
variable "trace_sampling" {
  description = "Optional tail_sampling configuration for the otel-collector traces pipeline. When null, no sampler is added (100% pass-through)."
  type = object({
    enabled                       = optional(bool, false)
    decision_wait_seconds         = optional(number, 10)
    num_traces                    = optional(number, 50000)
    expected_new_traces_per_sec   = optional(number, 1500)
    error_policy_enabled          = optional(bool, true)
    latency_threshold_ms          = optional(number, 1000)
    ok_baseline_percentage        = optional(number, 15)
  })
  default = { enabled = false }
}
```

The collector config template gates the `tail_sampling` processor block on `var.trace_sampling.enabled`; when false, the traces pipeline reverts to today's `[memory_limiter, resource, batch]`. Module unit tests cover both the enabled and disabled rendering.

The site-deployment caller for prod sets `trace_sampling = { enabled = true, ok_baseline_percentage = 15 }` in the tfvars overlay; per-tenant overlays may further tune.

### What stays out of the module

- No hardcoded latency thresholds tuned to study-web.
- No Keycloak-specific path filters (those belong in the shared-prod overlay alongside the work in ka-0000002683).
- No tenant-list switches — the module knows about a single deployment.

## 5. Rollout plan

Per-tenant first, shared-prod last. Rationale: blast radius is smaller per-tenant; shared-prod hosts Keycloak which is the fattest single trace source (~129 spans/sec, ka-0000004759) so it both benefits most and stresses the collector hardest. Validating on a per-tenant collector lets us catch memory-pressure surprises before they hit shared-prod.

### Phase 0 — module change (gated)

After this design is approved:

1. Land the module change (variable + conditional processor block) with `enabled = false` everywhere. Module behavior is unchanged. This unblocks the rollout without committing to a behavior change.
2. site-deployment tfvars overlay change is a separate MR per phase below.

### Phase 1 — single per-tenant pilot (1 week observation)

1. Pick a representative tenant collector (medium span volume, both authenticated and anonymous traffic). Set `trace_sampling.enabled = true` with the proposed defaults.
2. Apply during a quiet window. Watch for:
   - `otelcol_processor_tail_sampling_count_traces_sampled{policy=...}` per policy (sanity-check the policy mix).
   - `otelcol_processor_tail_sampling_new_trace_id_received` vs `_sampling_decisions_made` — closing the gap means decisions are firing.
   - Collector task `MemoryUtilization` (CloudWatch) — should stay below 70% sustained.
   - VictoriaTraces ingest rate from this tenant should drop by ~80%.
3. Verify error/latency retention with the queries in section 6.
4. If memory pressure shows: lower `decision_wait` to 5s, then `num_traces` to 25000.

### Phase 2 — broader per-tenant rollout (1–2 weeks)

Roll out to remaining per-tenant collectors in batches. Each batch waits one full business cycle (24h) before the next, so unusual workload patterns surface.

### Phase 3 — shared-prod (Keycloak-heavy)

shared-prod is last because:

- It is the highest-volume single collector (~129 spans/sec is mostly Keycloak, ka-0000004759).
- The tail_sampling memory profile under Keycloak's burstier patterns (token-refresh storms at session-expiry boundaries) is the worst-case test.
- It can compose with Keycloak source-side sampling once ka-0000002683 lands. Order matters: if ka-0000002683 lands first, the offered load to the collector drops 10×, and the tail-sampling memory budget becomes trivial. If tail-sampling lands first, ka-0000002683's eventual landing is purely additive savings. Either order is safe; we do not block on the other.

### Rollback

Sampling is reversible by setting `trace_sampling.enabled = false` in the offending overlay and re-applying. ECS task variable change re-renders the collector config; no state migration. Rollback should complete within one ECS deployment cycle (~3–5 minutes).

If we determine `tail_sampling` is too memory-intensive *categorically* (not just at one tenant), the fallback is to swap to `probabilistic_sampler` at 10–15%, accepting the loss of error-bias. That is a worse outcome than the current state for debugging, so we exhaust tail_sampling tuning first.

## 6. Verification

Three confirmation classes, each with explicit queries / steps. None require manual visual inspection beyond the dashboard checks.

### 6.1 Confirm error traces are still kept

**Synthetic**: After enabling sampling on the pilot tenant, hit an endpoint that we know produces an error trace (e.g. POST to a route with an invalid payload triggering a validation error → `status: error` on the FastAPI server span). Within `decision_wait + 5s`, the trace should appear in VictoriaTraces.

**Bulk**: Compare error trace count before/after via VictoriaTraces query:

```
sum(rate(traces_count{status="error", deployment_environment="<tenant>"}[15m]))
```

Pre-rollout vs post-rollout should be **statistically indistinguishable** — error-rate is workload-driven, not sample-driven. If post-rollout error count drops more than ~5%, the `errors` policy is misfiring (most likely candidate: status_code attribute name mismatch). Investigate before proceeding.

### 6.2 Confirm latency outliers are still kept

**Synthetic**: Hit a known-slow endpoint (or use a sleep-injecting test endpoint) that produces a >1s root span. Trace should appear in VictoriaTraces.

**Bulk**: Verify the `latency-outliers` policy is firing via collector metrics:

```
rate(otelcol_processor_tail_sampling_count_traces_sampled{policy="latency-outliers"}[5m])
```

Should be >0 in any window with active interactive traffic. Cross-check against derived spanmetrics: the count of traces with `duration > 1s` in spanmetrics should approximately equal the latency-outliers retention rate.

### 6.3 Confirm OK-trace volume drops

**Volume**: VictoriaTraces ingest rate (bytes or trace-count, both work) for the pilot tenant should drop by ~80% within an hour of rollout.

```
sum(rate(victoriatraces_ingested_traces_total{tenant="<tenant>"}[5m]))
```

Pre-rollout vs post-rollout: expect ratio of ~0.18–0.22 (i.e. retained fraction matches the section-2 math).

**Policy mix**: The collector emits per-policy decision counters. Compute the policy distribution:

```
sum by (policy) (rate(otelcol_processor_tail_sampling_count_traces_sampled[5m]))
```

Expected approximate split: `errors` ~2%, `latency-outliers` ~3%, `ok-baseline` ~95% of *retained* traces. Drift from this distribution suggests a policy is mis-configured or a workload pattern has shifted; either is a useful signal.

### 6.4 Health of the collector itself

Throughout, watch for tail-sampling-induced health regressions:

- `otelcol_processor_tail_sampling_sampling_trace_dropped_too_early` — non-zero means traces hit `num_traces` cap before decision. Bump `num_traces` or shorten `decision_wait`.
- `otelcol_processor_refused_spans` — non-zero with `processor=memory_limiter` means we are shedding load. Bump task memory.
- ECS task `MemoryUtilization` sustained >70% — see knob tuning above.

## Open questions for the operator

These are deliberately punted to the operator-approval step rather than decided here:

1. **Pilot-tenant choice**: which tenant collector do we use as the Phase-1 pilot? Recommend a medium-volume tenant with mixed authenticated and anonymous traffic. Operator picks based on which tenant they want to absorb 1 week of "traces appear ~10s late" UX.
2. **Coordination with ka-0000002683 and ka-0000006795**: do we want to land Keycloak source-side sampling and study-web browser-side rate-limiting *first* (reducing offered load before adding the tail-sampling memory cost), or in parallel? This doc is ordering-agnostic but flags the ordering decision.
3. **Latency threshold revisit cadence**: who owns tuning `latency_threshold_ms` after the first week of histograms? Suggest filing a beads task at rollout time, owned by whoever runs the dashboards.

## Summary

- Use **`tail_sampling`**, not `probabilistic_sampler`. Probabilistic cannot keep all errors.
- Three policies, OR-of-policies semantics: `errors` (status_code: ERROR, 100%), `latency-outliers` (latency > 1000ms, 100%), `ok-baseline` (probabilistic 15%).
- Sizing: `decision_wait: 10s`, `num_traces: 50000`, `expected_new_traces_per_sec: 1500`. Fits within current prod cpu=2048/memory=4096 with ~1 GB margin; tighten knobs before bumping memory.
- Module default: sampling **disabled** (100% pass-through). Prod tfvars overlay opts in.
- Rollout: per-tenant pilot → broader per-tenant → shared-prod last. Reversible at the tfvars layer.
- Verification: error retention via synthetic + bulk count diff; outlier retention via collector metrics + synthetic; volume drop via VictoriaTraces ingest. Health watched via tail-sampling and memory_limiter counters.

No implementation lands until operator approval per ka-0000004073 acceptance criteria.
