import type { Tool as MCPTool } from '@modelcontextprotocol/sdk/types.js';

/**
 * Curated, READ-ONLY allow-list of mcp-grafana tools exposed to the assistant.
 *
 * Security envelope (epic ka-0000002011, task ka-0000006039):
 *  - All LLM + MCP traffic flows through grafana-llm-app -> the in-cluster
 *    LiteLLM/Bedrock backend. Zero off-cluster egress.
 *  - This list is the THIRD layer of a defense-in-depth read-only posture:
 *      1. The deployed mcp-grafana server runs with `--disable-write`
 *         (chart: mcpGrafana.disableWrite=true), so write tools are not even
 *         advertised.
 *      2. The Grafana service-account token it authenticates with is a scoped
 *         Viewer token, empirically incapable of writes (POST /api/folders -> 403).
 *      3. This plugin-side allow-list — we only ever hand the model tools we
 *         have explicitly vetted as read-only, regardless of what the server
 *         advertises.
 *
 * Dashboard/folder CREATE power is a SEPARATE, config-gated surface — see
 * `WRITE_TOOLS` below (C8, ka-0000005589). It is NEVER folded into this list:
 * the read-only set stays the default, and the deny heuristic below is NOT
 * relaxed — the vetted write names are admitted by `isToolAllowed` only when the
 * env has explicitly opted in, and only because they are on `WRITE_TOOLS`.
 *
 * The allow-list is intentionally generous within the read-only surface so the
 * model can ground answers richly (Prometheus/VictoriaMetrics, dashboards,
 * Loki, datasources, alerting reads, annotations, OnCall, navigation, etc.),
 * while the deny heuristic catches any write-shaped tool the server might
 * advertise in a future mcp-grafana version we have not re-vetted.
 */
export const READ_ONLY_TOOLS: readonly string[] = [
  // --- Search & dashboards (read) ---
  'search_dashboards',
  'get_dashboard_by_uid',
  'get_dashboard_panel_queries',
  'get_dashboard_property',
  'get_dashboard_summary',

  // --- Datasources (read) ---
  'list_datasources',
  'get_datasource',

  // --- Prometheus / VictoriaMetrics (read) ---
  'query_prometheus',
  'query_prometheus_histogram',
  'list_prometheus_metric_metadata',
  'list_prometheus_metric_names',
  'list_prometheus_label_names',
  'list_prometheus_label_values',

  // --- Loki (read) ---
  'query_loki_logs',
  'query_loki_stats',
  'query_loki_patterns',
  'list_loki_label_names',
  'list_loki_label_values',
  'analyze_loki_labels',

  // NOTE: alerting_manage_rules / alerting_manage_routing are intentionally
  // OMITTED. Despite the "inspect" framing, alerting_manage_rules reaches
  // create/update/delete via its `operation` parameter, so it is not wholly
  // read-only and has no place in the read-only-first slice. If alert-rule
  // grounding is wanted later, add it behind the same write-gated config as the
  // C7 dashboard-create work — never to this read-only list.

  // --- Incidents (read) ---
  'list_incidents',
  'get_incident',

  // --- OnCall (read) ---
  'list_oncall_schedules',
  'get_oncall_shift',
  'get_current_oncall_users',
  'list_oncall_teams',
  'list_oncall_users',
  'list_alert_groups',
  'get_alert_group',

  // --- Sift investigations (read) ---
  'list_sift_investigations',
  'get_sift_investigation',
  'get_sift_analysis',

  // --- Pyroscope profiling (read) ---
  'list_pyroscope_label_names',
  'list_pyroscope_label_values',
  'list_pyroscope_profile_types',
  'query_pyroscope',

  // --- Asserts (read) ---
  'get_assertions',

  // --- Annotations (read) ---
  'get_annotations',
  'get_annotation_tags',

  // --- Snapshots (read) ---
  'list_snapshots',
  'get_snapshot',

  // --- Navigation (read-only URL generation) ---
  'generate_deeplink',
];

const READ_ONLY_TOOL_SET = new Set(READ_ONLY_TOOLS);

/**
 * Curated, VETTED WRITE allow-list — the ONLY mutating tools this plugin will
 * ever expose, and only when the env has opted in (C8, ka-0000005589; epic
 * ka-0000002011). Bounded to dashboard + folder CREATE/UPDATE; deliberately NO
 * alerting, annotations, datasource-write, admin, or any other write family.
 *
 * These are the exact write tools the embedded mcp-grafana server (the one this
 * plugin's MCP client connects to) advertises once C7 (ka-0000001262) turned on
 * the `dashboard` + `folder` toolset families with the managed-SA's
 * dashboards:create + folders:create scope (create-only; NO dashboards:write /
 * folders:write — see plugin.json `iam` and ka-0000004727 for why a pure create
 * needs only `dashboards:create`@folders and `folders:create`@folders):
 *   - `update_dashboard`  — create-OR-overwrite-a-dashboard-by-UID. Upstream
 *     mcp-grafana exposes NO pure create-only dashboard tool; this single tool
 *     both creates new dashboards (omit/duplicate uid) and overwrites existing
 *     ones, so the create flow and the overwrite flow share one name. The
 *     managed SA has no dashboards:write, so an overwrite of an EXISTING
 *     dashboard is refused at the Grafana RBAC layer (CanSave -> 403); the
 *     frontend `dashboardCreateOnlyViolation` guard rejects it earlier still.
 *   - `create_folder`     — create a folder to target a new dashboard into.
 *
 * Gating (see `isToolAllowed`): a write tool is admitted ONLY when (a) it is on
 * THIS list — so the deny heuristic cannot reject our own vetted write names —
 * AND (b) the caller passes `enableWrites=true` (plugin config
 * `enableDashboardWrites`, default FALSE). An UNVETTED write tool the server
 * might advertise in a future version is NOT on this list, so the heuristic
 * still nets it even with writes enabled.
 */
export const WRITE_TOOLS: readonly string[] = [
  // --- Dashboards (create / overwrite-by-UID) ---
  'update_dashboard',
  // --- Folders (create) ---
  'create_folder',
];

const WRITE_TOOL_SET = new Set(WRITE_TOOLS);

/**
 * Substrings that mark a tool as write-shaped. Even if a tool slips past the
 * allow-list (e.g. a future server advertises a renamed write tool), any tool
 * whose name contains one of these verbs is rejected. This is a safety net, not
 * the primary control — the allow-list is.
 *
 * NB the vetted `WRITE_TOOLS` names (update_dashboard, create_folder) ARE
 * write-shaped by design; `isToolAllowed` admits them via the explicit
 * allow-list BEFORE consulting this net, so the heuristic only ever rejects
 * UNVETTED write-shaped tools.
 */
const WRITE_NAME_MARKERS: readonly string[] = [
  'create',
  'update',
  'delete',
  'remove',
  'add_',
  'set_',
  'write',
  'patch',
  'edit',
  'modify',
  'put_',
  'post_',
  'apply',
];

/** Returns true if a tool name looks write-shaped (defense-in-depth net). */
export function isWriteShaped(name: string): boolean {
  const lower = name.toLowerCase();
  return WRITE_NAME_MARKERS.some((marker) => lower.includes(marker));
}

/**
 * Returns true if a tool may be exposed to / called by the model.
 *
 * Decision order (the vetted allow-lists WIN over the heuristic):
 *   1. A vetted WRITE tool is allowed iff writes are explicitly enabled. It is
 *      admitted here — before the write-shaped net — precisely because our own
 *      write names (update_dashboard, create_folder) are write-shaped and would
 *      otherwise be rejected by the heuristic.
 *   2. Otherwise the tool must be on the read-only allow-list AND must not be
 *      write-shaped. This is what nets any UNVETTED write tool (a renamed or
 *      newly-advertised mutator) even when writes are enabled, since such a tool
 *      is not on `WRITE_TOOLS`.
 *
 * @param name        the mcp-grafana tool name
 * @param enableWrites whether the env has opted into dashboard/folder writes
 *                     (plugin config `enableDashboardWrites`, default false)
 */
export function isToolAllowed(name: string, enableWrites = false): boolean {
  if (enableWrites && WRITE_TOOL_SET.has(name)) {
    return true;
  }
  return READ_ONLY_TOOL_SET.has(name) && !isWriteShaped(name);
}

/**
 * Filter an MCP tool list down to the surface the model may use: the curated
 * read-only set always, plus the vetted write tools when `enableWrites` is set.
 * Used both to decide which tools the model may call and to display the active
 * tool surface to the user.
 *
 * @param tools        the tools the server advertises
 * @param enableWrites whether to additionally admit the vetted write tools
 */
export function curateTools(tools: MCPTool[], enableWrites = false): MCPTool[] {
  return tools.filter((tool) => isToolAllowed(tool.name, enableWrites));
}

/**
 * Back-compat alias: curate down to the read-only surface only. Equivalent to
 * `curateTools(tools, false)`. Retained so existing read-only call sites and
 * their tests keep reading clearly.
 */
export function curateReadOnlyTools(tools: MCPTool[]): MCPTool[] {
  return curateTools(tools, false);
}
