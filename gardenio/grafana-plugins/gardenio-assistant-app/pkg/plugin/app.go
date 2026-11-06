// Package plugin implements the gardenio-assistant-app Grafana app backend.
//
// The backend is a thin CallResource reverse-proxy. It forwards the chat box's
// MCP requests (browser -> /api/plugins/gardenio-assistant-app/resources/mcp/grafana)
// to an IN-CLUSTER, write-capable mcp-grafana sidecar, stamping the plugin's own
// Grafana-minted managed service-account token and the in-cluster Grafana URL as
// the X-Grafana-API-Key / X-Grafana-URL headers mcp-grafana reads per request.
//
// WHY THIS EXISTS (root cause, ka-0000000293):
//
//	grafana-llm-app 1.0.8 embeds mcp-grafana v0.11.3, whose embedded server
//	(pkg/mcp/http_server.go) resolves its server-to-server Grafana API target as
//	cfg.AppURL() and authenticates with cfg.PluginAppClientSecret(). cfg.AppURL()
//	is Grafana's app_url, which is DERIVED FROM server.root_url and cannot be
//	split from it. In this deployment root_url MUST be the external, AWS-Verified-
//	Access edge-auth'd unified host (the browser SPA needs it), so the embedded
//	server's outbound calls EGRESS to the edge and get 403 (the edge expects an
//	AVA OIDC browser session, not a managed-SA bearer). v1.0.8 exposes no override
//	(GF_APP_URL is fallback-only in the SDK) and is the latest release, and the
//	plugin is Grafana-root-signed, so the embedded server cannot be repointed.
//
//	This plugin's frontend therefore points @grafana/llm's MCPClientProvider at
//	THIS plugin (mcpAppName=gardenio-assistant-app). The browser hits the SAME
//	plugin-resource transport it would use for the embedded server — already
//	inside the edge-auth'd /grafana scope, so the user's identity is unchanged —
//	but this backend reverse-proxies it to an in-cluster mcp-grafana that reaches
//	Grafana over the cluster Service. That fixes BOTH grounding READ and dashboard
//	CREATE while keeping every hop in-cluster (zero off-cluster egress). The LLM
//	completion path (grafana-llm-app's openai proxy) is untouched.
package plugin

import (
	"context"
	"net/http"
	"strings"

	"github.com/grafana/grafana-plugin-sdk-go/backend"
	"github.com/grafana/grafana-plugin-sdk-go/backend/instancemgmt"
	"github.com/grafana/grafana-plugin-sdk-go/backend/log"
	"github.com/grafana/grafana-plugin-sdk-go/backend/resource/httpadapter"
)

// PluginVersion is the plugin version, set by main from the Mage-injected
// linker var so logs/telemetry can report it.
var PluginVersion = "development"

// Ensure App implements the SDK interfaces it actually needs. We implement
// CallResourceHandler (the reverse-proxy) and CheckHealthHandler (so Grafana's
// plugin health page reflects sidecar reachability). We deliberately do NOT
// implement QueryData/StreamHandler — this backend serves no datasource queries.
var (
	_ backend.CallResourceHandler   = (*App)(nil)
	_ backend.CheckHealthHandler    = (*App)(nil)
	_ instancemgmt.InstanceDisposer = (*App)(nil)
)

// App is the gardenio-assistant-app backend instance.
type App struct {
	backend.CallResourceHandler

	// mcpUpstreamURL is the in-cluster base URL of the write-capable mcp-grafana
	// sidecar (e.g. http://gardenio-mcp-grafana-write:8000), from jsonData.
	// The /mcp endpoint path is appended by the proxy route.
	mcpUpstreamURL string

	// grafanaInClusterURL is the in-cluster Grafana base URL the sidecar should
	// call (e.g. http://gardenio-grafana:3000/grafana), from jsonData. Forwarded
	// as X-Grafana-URL so the sidecar NEVER resolves the external app_url.
	grafanaInClusterURL string

	// saToken is this plugin's Grafana-minted managed service-account token
	// (cfg.PluginAppClientSecret), scoped by plugin.json `iam` to dashboards +
	// folders + datasource reads ONLY. Forwarded as X-Grafana-API-Key so the
	// sidecar acts as this SA. Never leaves the cluster.
	saToken string
}

// NewApp builds an App from the plugin's provisioned settings + the managed-SA
// token Grafana shares with the plugin on each instance.
func NewApp(ctx context.Context, settings backend.AppInstanceSettings) (instancemgmt.Instance, error) {
	var app App

	cfg, err := loadConfig(settings)
	if err != nil {
		return nil, err
	}
	app.mcpUpstreamURL = cfg.MCPUpstreamURL
	app.grafanaInClusterURL = cfg.GrafanaInClusterURL

	// The Grafana-minted managed-SA token (plugin.json `iam`). Off Grafana Cloud
	// this is the only identity the proxied MCP tools run as. A warning (not a
	// hard error) keeps the instance up so CheckHealth can report the misconfig.
	gcfg := backend.GrafanaConfigFromContext(ctx)
	if gcfg != nil {
		app.saToken, err = gcfg.PluginAppClientSecret()
		if err != nil {
			log.DefaultLogger.Warn("gardenio-assistant-app: no managed SA token; MCP proxy will be unauthenticated", "err", err)
		}
		// Fall back to Grafana's own app URL only if the overlay did not pin an
		// in-cluster URL — but log loudly, because the external app_url is the
		// exact value that breaks the embedded server.
		if app.grafanaInClusterURL == "" {
			if appURL, aerr := gcfg.AppURL(); aerr == nil {
				log.DefaultLogger.Warn("gardenio-assistant-app: grafanaInClusterURL unset; falling back to app_url (likely external — set jsonData.grafanaInClusterURL)", "appURL", appURL)
				app.grafanaInClusterURL = strings.TrimRight(appURL, "/")
			}
		}
	}

	mux := http.NewServeMux()
	app.registerRoutes(mux)
	app.CallResourceHandler = httpadapter.New(mux)

	return &app, nil
}

// Dispose is called by the SDK when the instance is replaced. The proxy holds no
// long-lived resources, so this is a no-op.
func (a *App) Dispose() {}
