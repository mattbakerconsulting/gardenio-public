# W2c trace-aware panels — data-correctness audit

> Task [[ka-0000006848]] · epic [[ka-0000000756]] · rubric `gardenio/docs/data-correctness-rubric.md`.
> Audited in-cluster against live VictoriaTraces (`gardenio-victoria-traces:10428`), 2026-06-22.

## What shipped

Three beads-embeddable trace panels, served by kbeads from the **VictoriaTraces
Jaeger query API** (not span-derived PromQL — see "Architecture finding" below):

| Endpoint | Panel | acme-helm parity target |
|---|---|---|
| `GET /v1/charts/traces/service-map` | Per-service RED tier (req rate, error ratio, p50/p95 latency) across every live VT service | `victoria-traces` / service-map node tier |
| `GET /v1/charts/traces/error-correlation` | Top 4xx/5xx hot-spots by (service, operation) + fleet error totals | `error-correlation-dashboard` |
| `GET /v1/charts/traces/red?service=X` | Single-service RED detail (drill from a service-map row) | `distributed-traces-dashboard` |

Code: `kbeads/internal/server/charts_traces.go` (handlers + RED derivation),
`charts_traces_vtclient.go` (VT Jaeger client), `charts_traces_test.go` (tests).
Wiring: `BEADS_VICTORIA_TRACES_URL` (+ optional `_TOKEN`) in `cmd/kd/serve.go`.

## Architecture finding (why VT-direct, not span PromQL)

The originally-scoped path — port the acme RED PromQL (`span_calls_total`,
`span_duration_milliseconds_bucket`) as `:::grafana`/`:::chart` panels — is **not
realizable on this stack**. Probed live 2026-06-22:

1. **gardenio VM has zero `span_*` spanmetrics.** `vmsingle:8428` carries 1220
   metric names; none are span-derived. No spanmetrics connector mirrors
   spans→VM. A `:::grafana` panel on `span_calls_total` would render permanently
   empty → fails C5 (existence) / R2 (honest-empty).
2. **The relay emits no metrics.** `internal/perftracesrelay` replays raw Jaeger
   traces → VT over OTLP only.
3. **The chart-catalog backend is PromQL/VM-only** — no VT client existed.
4. **Traces are single-service.** 0 multi-service traces across all 5 services,
   spans carry no cross-service references, `/select/jaeger/api/dependencies`
   returns `[]`. A true node+edge service-**map** topology is **not computable**
   from this data.

The rubric §2 names VT as the trace **ground truth**, so deriving RED straight
from the raw server spans is both the only realizable source and the most
replay-honest one: the panel value *is* the ground-truth count.

## Honest degradations (documented, not faked)

- **Service map → service-RED tier.** Response sets `topology_available:false`
  with a `note` explaining the missing edges. Clients render the per-service RED
  tier (what each acme map node shows), not a fabricated topology. (R2/R4/C5.)
- **Server-spans only.** RED counts only `span.kind="server"` spans — a service's
  own request surface. Client/internal spans (DB `SELECT`s, outbound calls) are
  excluded; counting them would inflate the rate exactly like summing prewarmed
  agents inflates "active agents" (C2). Consequence: a service whose relayed
  traffic is all client/internal spans (sponsor-api, keycloak right now) renders
  **honest-empty** (`no_data:true`), not a misleading zero.

## Gate matrix

| Gate | Verdict | Evidence |
|---|---|---|
| **C1 Replay-match** | OK | Live replay of the panel logic vs raw VT curl agree exactly. `site-api` 1h: handler & raw both = 2 server spans, 2 errors, p95 3.1ms. Counts ARE the ground-truth span counts (no aggregation layer to drift). |
| **C2 Honest-aggregation** | OK | `isServerSpan` filter; client/internal excluded. Test `TestTracesServiceMap_REDFromServerSpans`. |
| **C3 Unit-scale** | OK | Duration µs→ms once (`/1000`); `requests_per_sec` = count/window-seconds; `error_ratio` ∈ [0,1]. JSON fields name units (`_ms`, `_per_sec`). |
| **C4 Freshness** | OK | Each render queries `[now-window, now]` live against VT; no cached/frozen series. |
| **C5 Existence-honesty** | OK | Services come from the live `/services` API; `red` 404s an unknown service (`TestTracesRED_UnknownServiceIs404`); empty service → `no_data` (`TestTracesServiceMap_HonestEmptyService`). |
| **C6 Window-integrity** | OK | Windows are a bounded enum (15m/1h/6h/24h); no rate-window-below-step artifact — RED is a direct span count over the window, not a `rate()[w]`. |
| **R1 Legible** | OK | Panels answer "how much traffic / how many errors / how slow", not raw metric names. |
| **R2 Honest-empty** | OK | `no_data` (service-map) and `empty` (error-correlation) flags; tests assert both. |
| **R3 Scale-sanity** | OK | p50 + p95 both returned so a single slow outlier doesn't crush the axis. |
| **R4 Self-locating** | OK | Every response echoes `window`, `evaluated_at`, per-service labels, and the topology `note`. |
| **R5 Drill-path** | OK | `service-map` → `red?service=X` is the aggregate→detail drill; both share one derivation path. |

## Replay command (paste to reproduce C1)

```bash
VT=http://gardenio-victoria-traces:10428
NOW=$(date +%s)000000; FROM=$(date -d '1 hour ago' +%s)000000
# raw server-span count + errors for one service (the panel's own number):
curl -fsS "$VT/select/jaeger/api/traces?service=site-api&start=$FROM&end=$NOW&limit=1000" \
  | jq '[.data[].spans[] | select(any(.tags[]; .key=="span.kind" and .value=="server"))] as $s
       | {server_spans: ($s|length),
          errors: ([$s[] | .tags[] | select(.key=="http.status_code") | .value | tostring
                    | select(test("^[45]"))] | length)}'
```

## Caveats / follow-ups

- **Truncation floor.** `traceMaxPerService=1000`. Wide busy windows can hit the
  cap; the row then carries `truncated:true` and the rate is a floor. No silent
  cap.
- **Topology gap** persists until the relay preserves cross-service span
  references (out of W2c scope). When it does, flip `topology_available`.
- **Beads-embed.** These are JSON panel endpoints (like `engagement/*`); a
  `:::chart`-style viewer embed for them is a small follow-on, not blocking the
  data-correctness acceptance this report covers.
