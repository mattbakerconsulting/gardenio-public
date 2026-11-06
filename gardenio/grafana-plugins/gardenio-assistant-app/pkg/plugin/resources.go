package plugin

import (
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	"github.com/grafana/grafana-plugin-sdk-go/backend/log"
)

// MCP endpoint paths. The frontend's @grafana/llm MCPClientProvider, given
// mcpAppName=gardenio-assistant-app, targets
// /api/plugins/gardenio-assistant-app/resources/mcp/grafana — so the resource
// route the SDK strips down to is exactly "/mcp/grafana".
const (
	mcpResourcePath = "/mcp/grafana"
	// sidecarEndpointPath is the mcp-grafana streamable-HTTP endpoint
	// (chart mcpGrafanaWrite.endpointPath, pinned to /mcp).
	sidecarEndpointPath = "/mcp"
)

// mcp-grafana reads the target Grafana base URL + bearer from these request
// headers, falling back to its own env only when they are absent
// (mcp-grafana mcpgrafana.go ExtractGrafanaInfoFromHeaders). We always stamp
// them so the sidecar talks to the IN-CLUSTER Grafana as THIS plugin's managed
// SA, regardless of how the sidecar pod is otherwise configured.
const (
	headerGrafanaURL    = "X-Grafana-URL"
	headerGrafanaAPIKey = "X-Grafana-API-Key"
)

// grafanaHeaderPrefix matches EVERY header mcp-grafana inspects to resolve the
// Grafana target + identity (X-Grafana-URL, X-Grafana-API-Key,
// X-Grafana-Service-Account-Token, X-Grafana-Org-Id, X-Grafana-Id, and any
// future X-Grafana-* the sidecar version may honour). We strip the whole prefix
// rather than enumerate names, so a sidecar bump that starts reading a new
// X-Grafana-* header can never silently let a client override identity. NB
// mcp-grafana 0.17.0's urlAndAPIKeyFromHeaders prefers X-Grafana-Service-
// Account-Token OVER X-Grafana-API-Key — an enumerate-by-name list that missed
// it would let an authenticated chat user supply that header and make the
// write-capable sidecar act as the caller's token instead of the bounded
// managed SA (confused-deputy). The prefix strip closes that whole class.
const grafanaHeaderPrefix = "X-Grafana-"

// extraClientIdentityHeaders are the non-X-Grafana- request headers mcp-grafana
// also honours for identity and which the browser must not control: Basic/Bearer
// Authorization and the on-behalf-of access token. The inbound call already
// carries the Grafana plugin-resource auth, which is irrelevant to (and must not
// leak into) the sidecar's own SA-bearer auth.
var extraClientIdentityHeaders = []string{
	"Authorization",
	"X-Access-Token",
}

// registerRoutes wires the CallResource mux. The ONLY route is the MCP
// reverse-proxy; everything else 404s (the chat box's LLM traffic goes through
// grafana-llm-app's own resource proxy, not this backend).
func (a *App) registerRoutes(mux *http.ServeMux) {
	mux.HandleFunc(mcpResourcePath, a.handleMCPProxy)
	// Tolerate a trailing slash on the streamable-HTTP path.
	mux.HandleFunc(mcpResourcePath+"/", a.handleMCPProxy)
}

// handleMCPProxy reverse-proxies an MCP streamable-HTTP request to the in-cluster
// write-capable mcp-grafana sidecar, injecting the in-cluster Grafana URL + this
// plugin's managed-SA token as the headers mcp-grafana authenticates from.
//
// Streaming: mcp-grafana's streamable-HTTP transport may respond with an
// event-stream (chunked) body. ReverseProxy with FlushInterval=-1 flushes after
// every read, and the SDK's httpadapter responseWriter turns each Flush into a
// CallResourceResponse chunk back to the browser — so SSE frames are delivered
// incrementally rather than buffered to end-of-response.
func (a *App) handleMCPProxy(w http.ResponseWriter, req *http.Request) {
	if a.mcpUpstreamURL == "" {
		http.Error(w, "gardenio-assistant-app: mcpUpstreamURL is not configured (set jsonData.mcpUpstreamURL)", http.StatusBadGateway)
		return
	}

	target, err := url.Parse(a.mcpUpstreamURL + sidecarEndpointPath)
	if err != nil {
		http.Error(w, fmt.Sprintf("gardenio-assistant-app: bad mcpUpstreamURL: %v", err), http.StatusInternalServerError)
		return
	}

	proxy := &httputil.ReverseProxy{
		// Flush every write so streamable-HTTP / SSE frames are not buffered.
		FlushInterval: -1,
		Director: func(r *http.Request) {
			r.URL.Scheme = target.Scheme
			r.URL.Host = target.Host
			r.URL.Path = target.Path
			r.Host = target.Host

			// Strip EVERY client-supplied identity header before re-stamping the
			// two the backend controls, so the browser can never repoint the
			// Grafana target, switch org, inject/override the bearer (incl. the
			// X-Grafana-Service-Account-Token the sidecar prefers), pass an
			// on-behalf-of id token, or smuggle Basic/Bearer auth. This backend is
			// the SOLE authority for who the sidecar authenticates as. Stripping
			// the whole X-Grafana-* prefix (not an enumerated name list) keeps
			// this robust across sidecar version bumps.
			for h := range r.Header {
				if strings.HasPrefix(http.CanonicalHeaderKey(h), grafanaHeaderPrefix) {
					r.Header.Del(h)
				}
			}
			for _, h := range extraClientIdentityHeaders {
				r.Header.Del(h)
			}
			if a.grafanaInClusterURL != "" {
				r.Header.Set(headerGrafanaURL, a.grafanaInClusterURL)
			}
			if a.saToken != "" {
				r.Header.Set(headerGrafanaAPIKey, a.saToken)
			}
		},
		ErrorHandler: func(rw http.ResponseWriter, _ *http.Request, perr error) {
			log.DefaultLogger.Error("gardenio-assistant-app: MCP proxy error", "err", perr, "upstream", target.String())
			http.Error(rw, fmt.Sprintf("gardenio-assistant-app: MCP upstream unreachable: %v", perr), http.StatusBadGateway)
		},
	}

	proxy.ServeHTTP(w, req)
}
