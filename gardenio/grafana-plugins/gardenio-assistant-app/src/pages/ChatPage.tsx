import React, { Suspense } from 'react';
import { mcp } from '@grafana/llm';
import { PluginPage } from '@grafana/runtime';
import { Alert, ErrorBoundary, LoadingPlaceholder } from '@grafana/ui';

import { ChatPanel } from '../components/Chat/ChatPanel';
import { testIds } from '../components/testIds';
import { APP_NAME, APP_VERSION } from '../constants';

/**
 * The gardenio assistant nav page, served at /a/gardenio-assistant-app/chat
 * (i.e. /grafana/a/gardenio-assistant-app/chat behind the subpath).
 *
 * Wiring follows the official grafana-llm-app how-to, with ONE deliberate
 * deviation (ka-0000000293):
 *  - `mcp.MCPClientProvider` creates and caches a single MCP client over the
 *    StreamableHTTP transport. We pass `mcpAppName={APP_NAME}` so the transport
 *    URL resolves to THIS plugin's OWN resource handler
 *    (/api/plugins/gardenio-assistant-app/resources/mcp/grafana) instead of
 *    grafana-llm-app's EMBEDDED mcp-grafana server.
 *
 *    WHY: grafana-llm-app 1.0.8 embeds mcp-grafana v0.11.3, whose server-to-
 *    server Grafana API calls resolve the target from Grafana's app_url
 *    (== GF_SERVER_ROOT_URL == the EXTERNAL, edge-auth'd unified host) and
 *    authenticate with the plugin managed SA. Off Grafana Cloud those outbound
 *    calls therefore EGRESS to Traefik + AVA edge-auth and get 403 — so the
 *    embedded MCP grounding + dashboard-create path is broken in any deployment
 *    whose root_url must stay external for the browser SPA (i.e. ours). The URL
 *    is inseparable from root_url (Grafana derives app_url from it) and v1.0.8
 *    exposes no override, so the embedded server cannot be repointed in-cluster.
 *
 *    This plugin's backend resource handler (pkg/plugin) reverse-proxies
 *    /resources/mcp/grafana to an IN-CLUSTER write-capable mcp-grafana sidecar,
 *    forwarding this plugin's own Grafana-minted managed-SA token + the
 *    in-cluster Grafana URL as X-Grafana-API-Key / X-Grafana-URL headers. So the
 *    same browser->plugin-resource path the embedded server uses now lands on an
 *    mcp-grafana that talks to Grafana over the in-cluster Service — fixing both
 *    grounding READ and dashboard CREATE while keeping ALL traffic in-cluster
 *    (zero off-cluster egress).
 *  - Suspense handles the async client connect; ErrorBoundary surfaces failures
 *    (e.g. the backend / sidecar unavailable) without crashing nav.
 */
interface ChatPageProps {
  /**
   * Per-env opt-in for dashboard/folder writes (plugin jsonData
   * `enableDashboardWrites`, default false). Threaded from App down into the
   * chat surface (C8, ka-0000005589).
   */
  enableDashboardWrites?: boolean;
}

function ChatPage({ enableDashboardWrites = false }: ChatPageProps) {
  return (
    <PluginPage>
      <div data-testid={testIds.chatPage.container}>
        <Suspense
          fallback={
            <LoadingPlaceholder
              text="Connecting to the gardenio assistant…"
              data-testid={testIds.chatPage.connecting}
            />
          }
        >
          <ErrorBoundary>
            {({ error }) => {
              if (error) {
                return (
                  <Alert
                    title="Assistant unavailable"
                    severity="warning"
                    data-testid={testIds.chatPage.disabled}
                  >
                    Could not connect to the assistant&apos;s MCP server. Ensure the grafana-llm-app
                    plugin (≥ 0.22) is installed and enabled (the LLM transport), and that the
                    in-cluster mcp-grafana sidecar this plugin proxies to is reachable. Details:{' '}
                    {error.message}
                  </Alert>
                );
              }
              return (
                <mcp.MCPClientProvider
                  appName={APP_NAME}
                  appVersion={APP_VERSION}
                  // Route MCP at THIS plugin's resource handler (in-cluster
                  // sidecar), NOT grafana-llm-app's edge-bound embedded server.
                  // mcpAppPath defaults to /mcp/grafana, matching the backend
                  // route in pkg/plugin/resources.go (ka-0000000293).
                  mcpAppName={APP_NAME}
                >
                  <ChatPanel enableDashboardWrites={enableDashboardWrites} />
                </mcp.MCPClientProvider>
              );
            }}
          </ErrorBoundary>
        </Suspense>
      </div>
    </PluginPage>
  );
}

export default ChatPage;
