# kbeads post-startup memory growth — unbounded ListBeads

Bead: ka-0000006889 (follow-on from ka-0000004022).
Date: 2026-04-30.

## Symptom recap

After raising the `gardenio-beads` memory limit 4Gi → 8Gi to absorb the
fleet-collector startup spike (ka-0000004022), the pod no longer OOMs but
memory continues to grow well past startup — observed at ~2.6Gi RSS after
15h of normal traffic. The bead description hypothesised "~100Mi/min"
linear growth and listed JetStream subscriber buffers, embedding HTTP-client
body buffers, the pgvector pool, and unbounded in-process caches as suspects.

## Findings

The dominant cost is **unbounded `GET /v1/beads`** calls served by
`handleListBeads` (`internal/server/http_beads.go`). The growth is not
strictly linear — it is a sawtooth where each call temporarily inflates
the heap to ~1Gi and Go's runtime retains the high-water mark — but the
practical effect is a steady-state RSS far above what the workload
warrants.

### Heap profile (live pod, 15h uptime)

`debug/pprof/heap` `inuse_space`:

| Frame | Share |
|---|---|
| `handleListBeads` total | 99.76% |
| `json.(*Encoder).Encode` (response serialisation) | ~576MB / call |
| `bytes.Clone` inside `scanBeadWithTotal` | ~213MB / call |
| `pq.textDecode` (lib/pq row decode) | rest |

`alloc_space` cumulative shows ListBeads as ~69% of all allocations
since process start. The handler is the single largest allocator in
the running server.

### Caller analysis

Audit logs over a 15h window show **1167 `/v1/beads` requests from the
single coopmux pod alone** — averaging one every ~46s, each 8–11s wall
time. Coopmux's account-cache refresh loop runs every 60s
(`coop/crates/mux/src/lib.rs` `spawn_account_cache_refresh`) and issues
two ListBeads calls per tick:

- `GET /v1/beads?type=account&status=open` (`coop/crates/mux/src/beads.rs:117`)
- `GET /v1/beads?type=config&status=open`  (`coop/crates/mux/src/beads.rs:152`)

Filtered, those queries each return ~9KB and complete in ~10ms. Observed
durations of 8–11s with payloads in the hundreds of MB are consistent
with the filter not being applied — the server is serving the full
unbounded bead set for every refresh.

### Why one call inflates the heap by ~1Gi

`writeJSON` (`internal/server/http.go:251-254`) uses
`json.NewEncoder(w).Encode(...)` but the encoder buffers the entire
response into an internal `bytes.Buffer` before flushing. With ~27K
beads in a populated DB, the buffered JSON is ~340MB, plus a `bytes.Clone`
copy in the row-scan path, plus `pq.textDecode` allocations. After GC,
Go retains the high-water-mark heap region.

## Fix

Apply server-side defence-in-depth: `handleListBeads` now defaults
`limit` to 1000 when the caller omits it, and clamps any caller-supplied
`limit` to a maximum of 10000. The change lives in
`internal/server/http_beads.go` and matches the existing pattern in
`http_audit.go` and `http_decisions.go`, both of which already cap their
list endpoints.

Rationale for the chosen numbers:

- **Default 1000**: Big enough that legitimate paged callers see no change
  in normal operation; small enough that an accidental unfiltered call
  returns ~1.5MB instead of ~340MB.
- **Max 10000**: Matches the existing `fleetCollector.fetchSnapshot`
  hard-coded limit (`http_metrics.go:135`). Callers that legitimately
  need more must page.

Existing in-process callers were audited before picking the numbers:

| Caller | Limit | Effect of cap |
|---|---|---|
| `cmd/kd/orphans.go` | 5000 | unchanged |
| `cmd/kd/status.go` | 0 (unlimited) | now capped at 1000 — single-screen display, fine |
| `internal/server/http_metrics.go` (fleet collector) | 10000 | unchanged |
| Coopmux account-cache refresh | filter-driven (~50 rows expected) | unchanged when filter applies; capped at 1000 when it doesn't |

## Follow-ups

1. **Coopmux filter mystery (separate bug)**: Coopmux constructs URLs
   with explicit `type=account&status=open` filters yet observed durations
   and payload sizes match unfiltered queries. Could be a `reqwest` URL
   construction bug, server-side filter parsing edge case, or auth-path
   short-circuit. Worth tracing with `tcpdump` against the beads pod
   service or by adding query-string logging to the request middleware
   (currently `http.go:357-407` logs `r.URL.Path` only). Filed as a
   follow-up bug.

2. **Streaming JSON encoding**: `writeJSON` buffers the full response.
   For large list responses, switching to a chunked encoder that writes
   directly to the `http.ResponseWriter` would cut the per-call peak
   roughly in half. Larger refactor — out of scope for this fix.

3. **Audit-middleware logging**: log the full `RawQuery` (with sensitive
   fields redacted) so future investigations don't have to guess which
   filter was applied. Currently the request middleware records only
   `path=/v1/beads`, which makes caller pattern analysis hard.

## Acceptance check

The bead's acceptance criterion was: "Steady-state RSS stable below 1Gi
after 1h with normal mutation traffic; revert chart memory limit
8Gi → 2Gi". The cap should hold steady-state under 1Gi by bounding any
one ListBeads call's heap footprint. The chart memory revert is a
separate operational follow-up — defer until at least one production
pod has run 24h on the new image.
