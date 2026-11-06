/**
 * Plugin `jsonData` shape (non-secret config persisted on the AppPlugin meta and
 * provisioned per-env via grafana provisioning `apps[].jsonData`).
 */
export interface GardenioAssistantJsonData {
  /**
   * Opt-in: let the assistant CREATE/UPDATE dashboards and folders through the
   * vetted write tools (update_dashboard, create_folder) — and ONLY those.
   *
   * Default FALSE: the chat box stays hard read-only unless an env explicitly
   * sets this true in the plugin's provisioned jsonData (C8, ka-0000005589;
   * epic ka-0000002011). The write surface is bounded to dashboard + folder
   * create/update; no alerting/annotations/datasource-write/admin. All LLM + MCP
   * traffic still stays on the in-cluster grafana-llm-app -> LiteLLM/Bedrock
   * path with zero off-cluster egress.
   */
  enableDashboardWrites?: boolean;
}
