package plugin

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/grafana/grafana-plugin-sdk-go/backend"
)

// CheckHealth reports the backend's readiness to proxy MCP traffic. It surfaces
// configuration gaps (missing upstream URL or managed-SA token) and probes the
// in-cluster mcp-grafana sidecar's /healthz so the Grafana plugin health page
// reflects real reachability rather than just "the binary started".
func (a *App) CheckHealth(ctx context.Context, _ *backend.CheckHealthRequest) (*backend.CheckHealthResult, error) {
	if a.mcpUpstreamURL == "" {
		return &backend.CheckHealthResult{
			Status:  backend.HealthStatusError,
			Message: "mcpUpstreamURL is not configured (set jsonData.mcpUpstreamURL)",
		}, nil
	}
	if a.saToken == "" {
		return &backend.CheckHealthResult{
			Status:  backend.HealthStatusError,
			Message: "no managed service-account token (externalServiceAccounts must be enabled and plugin.json iam present)",
		}, nil
	}

	healthURL := a.mcpUpstreamURL + "/healthz"
	reqCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, healthURL, nil)
	if err != nil {
		return &backend.CheckHealthResult{
			Status:  backend.HealthStatusError,
			Message: fmt.Sprintf("building sidecar health request: %v", err),
		}, nil
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return &backend.CheckHealthResult{
			Status:  backend.HealthStatusError,
			Message: fmt.Sprintf("mcp-grafana sidecar unreachable at %s: %v", healthURL, err),
		}, nil
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return &backend.CheckHealthResult{
			Status:  backend.HealthStatusError,
			Message: fmt.Sprintf("mcp-grafana sidecar health %s returned HTTP %d", healthURL, resp.StatusCode),
		}, nil
	}

	return &backend.CheckHealthResult{
		Status:  backend.HealthStatusOk,
		Message: "MCP proxy ready (in-cluster mcp-grafana reachable)",
	}, nil
}
