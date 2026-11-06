# Gardenio platform health page

> Operator dashboard for the first 30 seconds of triage.

The platform health page lives at `GET /health` on the
[beads-viewer](./beads-viewer-dev.md) htmx app and renders seven tiles fed
by snapshot endpoints in `kbeads` and the `gardenio` controller. Each tile
answers one question the operator asks daily and links to a deeper view.

- Surface: htmx beads-viewer (`GET /health`).
- Refresh: per-tile `hx-get` polling at 30 s.
- Tracking epic: [`ka-0000000152`](https://beads.gardenio.e2e.example.com/beads/ka-0000000152).
- Design doc: [`docs/research/beads-viewer-health-page.md`](./research/beads-viewer-health-page.md).

## The seven tiles

Each tile renders one of three states:

- **Green** — operating normally.
- **Amber** — degraded but still serving (signal worth watching, not paging).
- **Red** — broken or stalled (operator should investigate now).

A tile that fails to fetch shows an explicit error state with the most
recent error message, not a green fallback.

| # | Tile | Question | Green | Amber | Red |
|---|---|---|---|---|---|
| 1 | Fleet liveness | Are agents running and progressing? | All agents `Running` and reconciler caught up. | Any deferred agents > 0 OR reconciler lag > 30 s. | Reconciler stalled OR > 5 % agents in `Failed`/`Unknown`. |
| 2 | Decision queue | Any operator-blocking decisions waiting? | 0 open decisions. | 1–4 open OR oldest open > 1 h. | 5+ open OR oldest open > 4 h. |
| 3 | Event throughput | Is bead-event traffic normal? | Within ±50 % of trailing-1 h median. | Outside that band but non-zero. | Zero events for ≥ 5 min during a working day. |
| 4 | Spend rate | Are agents burning normal $/h? | Within ±50 % of trailing-1 h median. | Outside that band. | 4× median or any single agent > $20/h. |
| 5 | Account health | Are credential pools healthy? | Every account fresh and below quota. | Any account ≥ 80 % quota OR last rotation > 24 h ago. | Any account locked, expired, or `last_event_at` (slack-bridge) > 60 s stale. |
| 6 | Daemon + bridge | Is the platform itself OK? | `kbeads /v1/health` 200, slack-bridge consuming events, fleet-collector fresh. | Any subsystem stale within tolerance. | `kbeads` down, slack-bridge stalled (no NATS event for > 60 s), or fleet-collector > 5× TTL stale. |
| 7 | Recent incidents | Did anything just break? | No release bugs, agent crashes, or stuck gates in 24 h. | 1–2 incidents in 24 h. | 3+ incidents OR any active stuck gate > 1 h. |

Tile 6 reads slack-bridge `last_event_at` from `GET /healthz` (see below).
The 60 s amber/red threshold matches the [research doc §5
recommendation](./research/beads-viewer-health-page.md).

## How to interpret amber and red

- **Amber on tile 1, 2, or 4** is usually self-healing — note it and check
  again in 30 s before acting.
- **Red on tile 6** (daemon + bridge) is the only state that almost always
  warrants paging. The platform itself is unhealthy and other tiles will
  start to lie.
- **Red on tile 7** does not necessarily indicate an active outage — recent
  incidents stay listed for 24 h. Click through to the linked beads to see
  status.

## Drill-downs

Each tile is clickable. Drill-downs go to the existing operator views
(`#agents`, `#decisions`, `#release-health`, etc.) rather than tile-specific
pages — keep one source of truth for each underlying dataset.

## slack-bridge `/healthz` and `last_event_at`

The slack-bridge process exposes its NATS-consumer freshness via the
existing `GET /healthz` endpoint. The response body shape:

```json
{
  "status": "ok",
  "version": "2026.120.3",
  "last_event_at": "2026-04-30T16:30:00.123456Z"
}
```

- `last_event_at` is the wall-clock UTC time at which the bridge most
  recently consumed a NATS message — either a hook event (`hooks.>`) or a
  coop message-forward event (`coop.events.messages.>`).
- The field is **omitted** when the bridge has not yet seen any NATS event
  (cold start before the first message arrives, or NATS connection failed).
  Treat omitted as "unknown, not OK" on tile 6.
- The timestamp is RFC 3339 in UTC. Compare against the response `Date`
  header (or wall-clock now) to compute the staleness window.
- The `status` and `version` fields keep their existing semantics — adding
  `last_event_at` does not change the 200/non-200 behaviour of `/healthz`.

Implementation lives in
[`gardenio/controller/internal/bridge/hookstream.go`](../controller/internal/bridge/hookstream.go)
(`HookStream.LastEventAt`) and
[`gardenio/controller/cmd/slack-bridge/helpers.go`](../controller/cmd/slack-bridge/helpers.go)
(`registerHealthHandlers`).

## See also

- [`docs/research/beads-viewer-health-page.md`](./research/beads-viewer-health-page.md) — design doc, tile rationale, data sources.
- Epic [`ka-0000000152`](https://beads.gardenio.e2e.example.com/beads/ka-0000000152) — tracking for the htmx implementation.
- Predecessor [`ka-0000008186`](https://beads.gardenio.e2e.example.com/beads/ka-0000008186) — research rescue task.
