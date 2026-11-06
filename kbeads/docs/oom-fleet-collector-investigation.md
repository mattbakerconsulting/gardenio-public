# kbeads OOM investigation — fleet collector dominance

Bead: ka-0000004022.
Date: 2026-04-30.

## Symptom recap

`gardenio-beads` pod with `beads.embedding.enabled=true` OOMKilled every ~6 min on
4Gi limit; t+35s memory hit 2948Mi (72% of 4Gi). 12 restarts in 72 min.
Workaround: 4Gi → 8Gi (MR 1857). Live state: ~27K live beads + 50K+ event beads.

## Findings

### Cause of the ~3Gi startup spike: `fleetCollector.fetchSnapshot`

`internal/server/http_metrics.go:126` is registered on the metrics handler
(`internal/server/http_metrics.go:247`) and runs on every Prometheus scrape
behind a 30s TTL cache. Prometheus annotations are wired in
`gardenio/helm/gardenio/templates/beads/deployment.yaml:24-26`, so the first
scrape lands within ~15-30s of pod start.

Per scrape it issues two unbounded reads:

```go
// http_metrics.go:135
agentBeads, _, err := c.store.ListBeads(ctx, model.BeadFilter{
    Type:  []model.BeadType{"agent"},
    Limit: 10000,
})
// http_metrics.go:194
allBeads, _, err := c.store.ListBeads(ctx, model.BeadFilter{
    Limit: 50000,
})
```

`queryListBeads` (`internal/store/postgres/queries.go:143`) selects
`beadColumns` — full `description`, `notes`, `fields`, etc. — and follows up
with `queryGetLabelsForBeads` for every returned ID. With ~27K rows averaging
several KB each (issue descriptions plus JSON `fields`), the second call alone
materialises hundreds of MB of `*model.Bead` graph in Go heap, plus the label
join. Combined with the agent-only fetch and the 30s cache that pins the
snapshot, steady-state heap sits in the ~1-3GB range, with each scrape
producing a fresh transient peak before the previous snapshot is GC'd.

This matches the observed t+35s ≈ 2948Mi spike: container start → first
Prometheus scrape → fetchSnapshot allocates ~2.5GB in `*Bead` + label slices →
`runtime` doesn't return memory to the OS until well after the next GC pass.

### Why the embedding=true correlation is most likely coincidental

`embedding.NewEmbedder(cfg)` (`internal/embedding/factory.go`) constructs a
stateless HTTP client struct. `BeadsServer.SetEmbedder` just stashes the
reference. No vectors are loaded into RAM, no startup backfill kicks off, no
new goroutine starts. The OpenAI-compatible embedder holds only a 60s-timeout
`http.Client` and a 1024-element scratch buffer per request. There is no
embedding-related path that allocates per-bead memory at startup.

`SetEmbeddingStatsProvider` is *never called* in `cmd/kd/serve.go`, so
`s.embedStats` stays nil and `RegisterEmbedding` falls back to
`NoopStatsProvider`. Nothing on the embedding registration path scans the
bead table.

JetStream replay was also ruled out: kbeads only *publishes* to JetStream;
the only NATS subscriber (`internal/hooks/handler.go`, subject
`beads.session.>`) is plain core NATS (no replay), and the streams declared
in `internal/eventbus/streams.go` are subscribed to only by *external*
consumers (the gardenio controller, slack-bridge).

The most likely story: the embedding rollout (ka-0000000853) was deployed
during a window when the cumulative bead count had crossed the threshold
where fetchSnapshot's transient allocation no longer fit under 4Gi. The flag
itself is a red herring; what matters is the bead corpus size.

### What's growing post-startup at ~100Mi/min — open

Not yet root-caused. Suspects worth confirming with a heap profile (pprof is
already wired at `MetricsAddr/debug/pprof/heap` via `cmd/kd/serve.go:312-316`):

- `fleetCollector.snapshot` is replaced (not appended) on each refresh, but
  the previous snapshot's `*Bead` slice references rooted in maps may keep
  the old generation pinned through scrape boundaries until two GC cycles
  fully clear it. With scrape-every-30s and a slow GC pacer, this could look
  like steady linear growth.
- `sseHub` ring buffer is capped at 1000 events (`internal/server/sse.go:18`)
  — bounded, eliminated.
- Audit retention runs every 24h with `Limit: 500` — eliminated.
- NATS JetStream client has bounded per-stream config (10K msgs / 100MB) —
  externally managed, not in kbeads heap.

### Embedding warmup question — N/A

Question 3 from the bead ("should embedding warmup be bounded or lazy?") does
not apply: there is no embedding warmup at startup today. Vectors live only in
pgvector and are fetched per-query.

## Proposed fix path

### Fix A (recommended): SQL-side aggregation in fetchSnapshot

Replace the two `ListBeads` calls with single-row aggregation queries that
never pull bead row data into Go memory:

```sql
-- replaces allBeads loop
SELECT type, status, COUNT(*) AS n
FROM beads
WHERE deleted_at IS NULL
GROUP BY type, status;

-- replaces agentBeads loop (cost / sessions / active by project label)
SELECT
    COALESCE(SUBSTRING(label FROM '^project:(.+)$'), 'unknown') AS project,
    SUM(CASE WHEN status IN ('open','in_progress') THEN 1 ELSE 0 END) AS active,
    SUM(COALESCE((b.fields->>'total_cost_usd')::float, 0)) AS cost_usd,
    SUM(COALESCE((b.fields->>'session_count')::float, 0)) AS sessions
FROM beads b
LEFT JOIN labels l ON l.bead_id = b.id AND l.label LIKE 'project:%'
WHERE b.type = 'agent' AND b.deleted_at IS NULL
GROUP BY project;
```

This collapses 27K+10K row scans + N+1 label joins into two single-pass
aggregations; per-scrape Go heap drops from hundreds of MB to a few KB.

Trade-off: the metric-collection path becomes SQL-shaped (need new store
methods `FleetTypeStatusCounts`, `FleetAgentRollup`). Tests use sqlmock so
the new methods are easy to cover. (`FleetAgentRollup` was later split into
`FleetActiveAgentsByProject` and `FleetAgentLifetimeRollup`, gd-ipZ10t3oTo.)

### Fix B (defensive, complementary): cap the per-scrape budget

Even after Fix A, audit other Prometheus collectors for unbounded scans.
Add a `ListBeads`-misuse `quench` rule that flags any `Limit >= 10000` outside
admin/migration paths.

### Fix C (instrumentation, ship alongside Fix A): live heap profile capture

pprof endpoints already serve at `MetricsAddr`. Add a runbook entry to the
gardenio operator docs:

```sh
kubectl -n gardenio port-forward deploy/gardenio-beads 9091:9091
go tool pprof -alloc_space http://localhost:9091/debug/pprof/heap
```

This is what we should run on the live 8Gi pod *before* Fix A ships, to
confirm fetchSnapshot dominance and identify the secondary 100Mi/min source.
The 8Gi headroom gives us a comfortable window to capture without rolling
the pod.

## Recommended next steps

1. Capture a live heap profile from the (post-workaround) 8Gi pod to confirm
   fetchSnapshot dominance and identify the post-startup growth source.
2. Ship Fix A as a kbeads MR (small, isolated to two new store methods +
   `http_metrics.go` rewrite + sqlmock tests).
3. After Fix A is deployed, watch the same pod for one full prometheus
   scrape window to confirm the spike is gone; then revert the 8Gi → 4Gi
   limit in a follow-up MR (separate commit, separate window).
4. File a separate task for the post-startup growth investigation if the
   heap profile points at something other than fleetCollector residue.
