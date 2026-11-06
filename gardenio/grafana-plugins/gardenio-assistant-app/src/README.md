# Gardenio Assistant

A gardenio-branded chat assistant embedded in Grafana. It streams answers from
the in-cluster LiteLLM/Bedrock backend (via the Grafana LLM app) and grounds
them in your gardenio dashboards and observability data through curated
[mcp-grafana](https://github.com/grafana/mcp-grafana) tools (read-only by
default; see the security envelope below).

**All LLM and MCP traffic stays inside the cluster — zero off-cluster egress.**

## Overview

The plugin adds a single **Chat** nav page. Ask questions like:

- "Which dashboards mention cost?"
- "Show me CPU usage over the last hour."
- "What datasources are configured here?"

The assistant uses `@grafana/llm`'s `streamChatCompletions` for the completion
stream and an MCP client (StreamableHTTP transport) to call observability tools.
It runs the standard tool-calling loop: the model requests a tool, the plugin
executes it against mcp-grafana, feeds the result back, and continues until the
model answers.

### Why this plugin has a backend (the MCP transport)

The MCP client does **not** talk to grafana-llm-app's *embedded* mcp-grafana
server. It targets **this plugin's own backend** (`mcpAppName=gardenio-assistant-app`),
which reverse-proxies MCP traffic to an **in-cluster** write-capable mcp-grafana
sidecar (`mcp-grafana-write`).

The reason (ka-0000000293): grafana-llm-app 1.0.8 embeds mcp-grafana v0.11.3,
whose embedded server resolves its server-to-server Grafana API target from
Grafana's `app_url` — which equals `GF_SERVER_ROOT_URL`, the **external,
edge-auth'd** unified host in this deployment — and authenticates with the plugin
managed SA. Off Grafana Cloud those outbound calls therefore egress to the edge
proxy and get **403** (the edge expects an interactive AVA session, not a bearer
token). `app_url` is derived from `root_url` and cannot be split from it, and
v1.0.8 exposes no override, so the embedded server's MCP grounding +
dashboard-create path is broken in any deployment whose `root_url` must stay
external for the browser SPA (i.e. ours).

This plugin's backend (`pkg/plugin`) is a thin CallResource reverse-proxy. The
browser hits the same plugin-resource transport it would use for the embedded
server — already inside the edge-auth'd `/grafana` scope, so the user's identity
is unchanged — but the backend forwards it to an in-cluster mcp-grafana that
reaches Grafana over the cluster Service (`X-Grafana-URL` header), authenticated
as this plugin's **own Grafana-minted managed service account** (`X-Grafana-API-Key`
header; scoped by `plugin.json` `iam` to dashboards + folders + datasource reads,
tighter than grafana-llm-app's SA). The result: grounding READ and dashboard
CREATE both work, and every hop stays in-cluster. The LLM completion path
(grafana-llm-app's OpenAI proxy → in-cluster LiteLLM) is unchanged.

## Requirements

- Grafana **>= 11.4**.
- The [Grafana LLM app](https://grafana.com/grafana/plugins/grafana-llm-app/)
  (`grafana-llm-app`) **>= 0.22** installed and enabled, with:
  - an OpenAI-compatible provider pointed at the in-cluster LiteLLM proxy, and
  - MCP enabled (the StreamableHTTP `/mcp` endpoint).

If the LLM app is not configured, the Chat page degrades gracefully to an
informational notice instead of erroring.

## Security envelope

The assistant is **read-only by default**. Out of the box it can query and read
observability data (Prometheus/VictoriaMetrics, Loki, dashboards, datasources,
OnCall, Sift, Pyroscope, annotations, snapshots, deeplinks) but does not create
or modify any Grafana resource.

A single opt-in config flag, **`enableDashboardWrites`** (plugin `jsonData`,
default **`false`**), unlocks a bounded, vetted **dashboard + folder
create/update** surface — and nothing else. When it is on, the assistant may
call exactly two mutating tools — `update_dashboard` (create-or-overwrite a
dashboard by UID) and `create_folder` — to build dashboards and the folders that
hold them. It still cannot touch alerts, datasources, users, annotations, or any
other Grafana resource, and there is no delete tool. With the flag off (the
default), the chat box stays hard read-only.

The posture is enforced in depth:

1. **Plugin allow-list (the load-bearing control).** The plugin only ever hands
   the model a curated allow-list of read-only tools (`src/tools/readOnlyTools.ts`),
   plus a separate vetted `WRITE_TOOLS` list (`update_dashboard`, `create_folder`)
   that is admitted *only* when `enableDashboardWrites` is true. A write-name deny
   heuristic nets any unvetted write-shaped tool even when writes are enabled. The
   gate is applied both when advertising tools to the model and re-checked at
   call time in `chatEngine.ts` before any tool runs.
2. **Standalone read-only mcp-grafana (Track A).** The standalone deployed
   mcp-grafana server runs with `--disable-write` and a scoped **Viewer**
   service-account token — a separate, always-read-only identity.
3. **Embedded server toolset families.** The chat box talks to the Grafana LLM
   app's embedded mcp-grafana server, whose enabled tool families are pinned in
   the chart to reads + `dashboard` + `folder` only; the alerting, admin,
   annotations, and other mutating families are disabled at registration (the
   `datasource` family is on, but exposes only its read tools).
4. **Managed-SA RBAC is create-only (defense in depth).** The `plugin.json`
   `iam` grants the managed service account `dashboards:create` (scoped to
   folders) + `folders:create` + reads, but **not** `dashboards:write` or
   `folders:write`. In Grafana a pure create (fresh uid, `overwrite=false`,
   `dash.ID==0`) is authorized by `dashboards:create` on the target folder;
   `dashboards:write` is only evaluated when *saving an existing* dashboard
   (`CanSave`). So even if the frontend `dashboardCreateOnlyViolation` guard
   were bypassed, an overwrite of a live dashboard is refused at the RBAC layer
   (403). `folders:create` is scoped to `folders:uid:*` because Grafana 11.4's
   folder-create check (with `nestedFolders` on) requires a folder-scoped
   `folders:create` — an unscoped grant fails with `folders.forbiddenCreation`.
   Verified empirically on staging Grafana 11.4.0 + source-grounded (ka-0000004727).

The default-off, per-env `enableDashboardWrites` flip keeps read-only the
baseline and makes the bounded dashboard-write power an explicit, auditable
opt-in. See `enableDashboardWrites` in `src/types.ts` and the helm
`grafana.assistant.enableDashboardWrites` value for the wiring.
