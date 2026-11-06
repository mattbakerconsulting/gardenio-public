package plugin

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/grafana/grafana-plugin-sdk-go/backend"
)

// config is the non-secret backend configuration provisioned via the plugin's
// jsonData (helm configmap.yaml -> apps[].jsonData). Both URLs are in-cluster
// Service addresses; neither is a secret. The managed-SA token is NOT here —
// Grafana delivers it separately via PluginAppClientSecret.
type config struct {
	// MCPUpstreamURL is the in-cluster base URL of the write-capable mcp-grafana
	// sidecar (e.g. http://gardenio-mcp-grafana-write:8000). The /mcp endpoint
	// path is appended by the proxy. Trailing slash trimmed.
	MCPUpstreamURL string
	// GrafanaInClusterURL is the in-cluster Grafana base URL the sidecar should
	// call (e.g. http://gardenio-grafana:3000/grafana). Forwarded as X-Grafana-URL
	// so the sidecar never resolves Grafana's external app_url. Trailing slash
	// trimmed.
	GrafanaInClusterURL string
}

// jsonDataShape mirrors the provisioned jsonData keys. enableDashboardWrites is
// read by the FRONTEND (AppRootProps meta.jsonData) and is irrelevant to the
// backend proxy, so it is intentionally not consumed here.
type jsonDataShape struct {
	MCPUpstreamURL      string `json:"mcpUpstreamURL"`
	GrafanaInClusterURL string `json:"grafanaInClusterURL"`
}

func loadConfig(settings backend.AppInstanceSettings) (config, error) {
	var jd jsonDataShape
	if len(settings.JSONData) > 0 {
		if err := json.Unmarshal(settings.JSONData, &jd); err != nil {
			return config{}, fmt.Errorf("parsing plugin jsonData: %w", err)
		}
	}
	return config{
		MCPUpstreamURL:      strings.TrimRight(jd.MCPUpstreamURL, "/"),
		GrafanaInClusterURL: strings.TrimRight(jd.GrafanaInClusterURL, "/"),
	}, nil
}
