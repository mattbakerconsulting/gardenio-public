# mcp-victorialogs — VictoriaLogs MCP server for agents

Task ka-0000006956. Operator directive (Matt Baker, 2026-08-29, decision
ka-0000006492): *"Enhance to use Victorialog also since that's what we have."*

Gives crew agents read-only **log-query** tools against the logs we already run
(the in-cluster VictoriaLogs `vlsingle` store), over the same Track A MCP path
that `mcp-grafana` uses for metrics/traces.

## Why not `query_loki_logs`?

mcp-grafana's `query_loki_logs` tool requires a **`loki`-typed** Grafana
datasource. VictoriaLogs speaks **LogsQL** (`/select/logsql/...`), not the Loki
query API, so it cannot back a `loki` datasource natively. The options were:

- Stand up a real Loki — rejected, we don't run one and the operator said use
  what we have.
- Add a third-party Loki→VL shim (e.g. `loki-vl-proxy`) — rejected as a prod
  supply-chain risk.
- **Chosen:** run the *official* VictoriaLogs MCP server
  (`ghcr.io/victoriametrics/mcp-victorialogs`) pointed at `vlsingle`, exposed to
  agents as its own MCP endpoint. Uses what we have, an official image, and the
  proven `mcp-grafana` deploy shape.

## Shape

Mirrors `mcpGrafana` (Track A): one standalone Deployment + ClusterIP Service +
NetworkPolicies. Values under `mcpVictoriaLogs` (`values.yaml`), default
**`enabled: false`** — flip on per-env in deploy-values alongside
`vlsingle.enabled`.

| Piece | Where |
|---|---|
| Deployment | `templates/mcp-victorialogs/deployment.yaml` |
| Service (ClusterIP `:8081`) | `templates/mcp-victorialogs/service.yaml` |
| Helpers (`fullname`/`labels`/`selectorLabels`/`vlEntrypoint`) | `_helpers.tpl` |
| Agent → server egress allow | `networkpolicies.yaml` (agent-egress) |
| Server ingress (agents only) | `networkpolicies.yaml` (`mcp-victorialogs-ingress`) |
| Server → vlsingle + DNS egress | `default-deny-egress.yaml` (`mcp-victorialogs-egress`) |
| vlsingle ingress peer | `networkpolicies.yaml` (`vlsingle-ingress`) |

**Config (all via ENV — no args override):**
- `VL_INSTANCE_ENTRYPOINT` — auto-derives the in-chart vlsingle Service
  (`http://<release>-victoria-logs:9428`) via `gardenio.vlsingle.baseURL`;
  override with `mcpVictoriaLogs.vlEntrypoint`.
- `MCP_SERVER_MODE=http`, `MCP_LISTEN_ADDR=0.0.0.0:8081` — streamable-HTTP,
  endpoint path is a fixed `/mcp`.
- `VL_DEFAULT_TENANT_ID=0:0` — vlsingle is single-tenant.

**Security envelope:** exposes VictoriaLogs' read-only select/query APIs only
(upstream registers no write/insert tools). Non-root, read-only rootfs, no K8s
API token, no secret. VictoriaLogs has no auth of its own in-cluster, so reach
is bounded purely by NetworkPolicy: agents → server, server → `vlsingle:9428` +
DNS only.

## Enabling in an environment

1. **Chart** ships with the plumbing default-off. In the env's deploy-values set
   both `vlsingle.enabled=true` (the store must exist) and
   `mcpVictoriaLogs.enabled=true`. On prod this is `terraform apply
   -target=module.gardenio` in `composition/canonical-prod` (external repo).
2. **Wire crew agents** — only AFTER the Service is live, add a `victorialogs`
   entry to the crew claude-mcp config bead (`ka-0000006806`, labels
   `project:gardenio`/`role:crew`), which `writeMCPConfig` merges into each
   agent's `.mcp.json`:

   ```json
   {"mcpServers":{"victorialogs":{"type":"http",
     "url":"http://gardenio-mcp-victorialogs:8081/mcp"}}}
   ```

   Do NOT add this entry before the prod Service exists, or agents get a dead
   MCP endpoint on startup.

## Verifying it's live

```bash
kubectl -n <ns> get deploy,svc -l app.kubernetes.io/component=mcp-victorialogs
# readiness: the pod serves GET /health/readiness (200) in http mode
kubectl -n <ns> port-forward svc/<release>-mcp-victorialogs 8081:8081 &
curl -s localhost:8081/health/readiness
```
