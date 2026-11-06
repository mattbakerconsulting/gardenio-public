// Command gpx_gardenio_assistant_app is the backend binary for the
// gardenio-assistant-app Grafana app plugin (ka-0000000293, epic ka-0000002011).
//
// The backend exists for ONE purpose: a CallResource reverse-proxy that routes
// the chat box's MCP traffic to an IN-CLUSTER mcp-grafana sidecar instead of
// grafana-llm-app's EMBEDDED mcp-grafana server. The embedded server resolves
// its Grafana API target from app_url (== the external, AVA-edge-auth'd
// root_url) and 403s on every server-to-server call off Grafana Cloud — so the
// grounding READ + dashboard-CREATE path is broken in any deployment whose
// root_url must stay external for the browser SPA (ours). See pkg/plugin for
// the full root-cause writeup. The LLM completion path (grafana-llm-app's
// openai resource proxy) is unaffected and is NOT proxied here.
package main

import (
	"os"

	"github.com/grafana/gardenio-assistant-app/pkg/plugin"
	"github.com/grafana/grafana-plugin-sdk-go/backend/app"
	"github.com/grafana/grafana-plugin-sdk-go/backend/log"
)

// version is set at build time by the Mage target via `-X main.version=x.y.z`.
var version = "development"

func init() {
	plugin.PluginVersion = version
}

func main() {
	log.DefaultLogger.Info("Starting gardenio-assistant-app backend", "version", version)
	// app.Manage blocks until Grafana shuts the process down. It manages the
	// life cycle of App instances (one per plugin context), calling plugin.NewApp
	// to build each.
	if err := app.Manage("gardenio-assistant-app", plugin.NewApp, app.ManageOpts{}); err != nil {
		log.DefaultLogger.Error(err.Error())
		os.Exit(1)
	}
}
