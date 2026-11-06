package main

// charts_proxy.go — the read-only browser-facing proxy to the kbeads chart
// catalog (E6.1, task ka-0000005063; epic ka-0000000596). The E5 backend
// (ka-0000001540) serves the chart catalog on the kbeads HTTP daemon:
//
//	GET /v1/charts             → catalog listing  (registered chart defs)
//	GET /v1/charts/{chart_id}  → {data, spec, meta, ios_hint} render
//
// Those endpoints are gated behind the daemon's RoleViewer bearer and are not
// reachable from the browser (the daemon Service is cluster-internal and the
// bearer is server-held). So the viewer fronts a deliberately narrow proxy:
// GET /api/v1/charts and GET /api/v1/charts/{chart_id}, forwarded to the
// daemon at <beadsHTTPAddr>/v1/charts[...] with the server's daemon bearer
// attached. The browser only chooses the chart id (a path segment we
// re-encode) plus the universal query string (since/until/step/window/…);
// it can never reach another daemon path.
//
// Why proxy and not call the daemon from the browser: the daemon bearer must
// not leak to the client, the daemon Service URL is not browser-reachable, and
// keeping the chart fetch inside the already-authenticated viewer origin lets
// the panels share the viewer session. This mirrors metrics_proxy.go, which
// fronts vmsingle the same way.
//
// Degraded mode: when beadsHTTPAddr is unset (it always is in prod, but tests
// and some local-dev configs leave it blank) the handler returns 503 so the
// misconfiguration is visible rather than surfacing as an empty chart.
//
// Caching: the daemon stamps a strong ETag on each render and honours
// If-None-Match with a 304. We forward both directions verbatim so a polling
// dashboard re-validates cheaply (304, no body) instead of re-fetching the
// full payload every interval.

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// chartsProxyBasePath is the constant upstream path prefix this proxy reaches
// on the kbeads daemon. The path is never taken from the request beyond the
// single {chart_id} segment (which we re-encode), so the proxy cannot be
// redirected at another daemon endpoint.
const chartsProxyBasePath = "/v1/charts"

// chartsProxyTimeout caps an individual catalog/render fetch. A chart render
// runs one PromQL range query upstream; a fetch that approaches this ceiling
// is a struggling backend and the panel should fail fast rather than pin a
// connection.
const chartsProxyTimeout = 20 * time.Second

// handleChartsCatalog forwards GET /api/v1/charts to the daemon's
// GET /v1/charts — the registered chart definitions and their param schemas.
// Independent of VM state, so it is served even when the chart backend's
// VictoriaMetrics endpoint is unconfigured (a client can still learn what
// charts exist).
func (s *Server) handleChartsCatalog(w http.ResponseWriter, r *http.Request) {
	s.proxyChartsUpstream(w, r, chartsProxyBasePath)
}

// handleChartsRender forwards GET /api/v1/charts/{chart_id} to the daemon's
// GET /v1/charts/{chart_id} — the {data, spec, meta, ios_hint} render. The
// chart id is taken from the path, percent-encoded as a single path segment,
// and appended to the constant base path; an empty id falls through to the
// catalog endpoint upstream, so we reject it here with a 404 to keep the two
// routes distinct.
func (s *Server) handleChartsRender(w http.ResponseWriter, r *http.Request) {
	chartID := strings.TrimSpace(r.PathValue("chart_id"))
	if chartID == "" {
		http.Error(w, "missing chart id", http.StatusNotFound)
		return
	}
	// Re-encode the single dynamic segment so a crafted id (slashes, dots)
	// cannot escape the /v1/charts/ prefix and reach another daemon path.
	upstreamPath := chartsProxyBasePath + "/" + url.PathEscape(chartID)
	s.proxyChartsUpstream(w, r, upstreamPath)
}

// engagementPanels / securityPanels are the fixed sub-paths the engagement and
// security dashboard proxies accept. Bounded (not free-form) so the single
// dynamic {panel} segment can never be steered at another daemon path — the
// forwarded upstream path is always chartsProxyBasePath + a constant from these
// maps, never request text. The map value is the exact upstream leaf so a
// renamed daemon route is a one-line change here.
var (
	engagementPanels = map[string]string{
		"activity": "activity",
		"spend":    "spend",
		"sessions": "sessions",
	}
	securityPanels = map[string]string{
		"authz":     "authz",
		"decisions": "decisions",
		"token-slo": "token-slo",
	}
)

// handleChartsUniqueness forwards GET /api/v1/charts/uniqueness to the daemon's
// GET /v1/charts/uniqueness — the DAU/WAU/MAU query-time panels (kbeads
// charts_uniqueness.go). A distinct route from handleChartsRender because
// "uniqueness" is not a catalog chart id: it is a fixed daemon endpoint, and
// registering it explicitly keeps the {chart_id} render route from swallowing
// it as an unknown chart.
func (s *Server) handleChartsUniqueness(w http.ResponseWriter, r *http.Request) {
	s.proxyChartsUpstream(w, r, chartsProxyBasePath+"/uniqueness")
}

// handleChartsEngagement forwards GET /api/v1/charts/engagement/{panel} to the
// daemon's GET /v1/charts/engagement/{panel} for the operator-engagement
// dashboard (activity / spend / sessions). The {panel} segment is validated
// against the engagementPanels allow-list and the upstream path is composed
// from the constant map value, never the raw request segment, so a crafted
// panel can never reach another daemon path. Identity resolution (display name,
// redaction) happens SERVER-SIDE in the daemon; this proxy only forwards the
// already-resolved JSON — it never sees or resolves an operator_id.
func (s *Server) handleChartsEngagement(w http.ResponseWriter, r *http.Request) {
	s.proxyChartsPanel(w, r, "engagement", engagementPanels, r.PathValue("panel"))
}

// handleChartsSecurity forwards GET /api/v1/charts/security/{panel} to the
// daemon's GET /v1/charts/security/{panel} for the security-overview dashboard
// (authz / decisions / token-slo). Same bounded-panel discipline as
// handleChartsEngagement; the security panels are identity-free by design so no
// resolution is involved on either side.
func (s *Server) handleChartsSecurity(w http.ResponseWriter, r *http.Request) {
	s.proxyChartsPanel(w, r, "security", securityPanels, r.PathValue("panel"))
}

// proxyChartsPanel validates a {panel} segment against a bounded allow-list and
// forwards to <base>/<group>/<panel> on the daemon. An unknown panel is a 404
// (never forwarded), so the upstream path is always composed from a constant,
// not request text.
func (s *Server) proxyChartsPanel(w http.ResponseWriter, r *http.Request, group string, allowed map[string]string, panel string) {
	leaf, ok := allowed[strings.TrimSpace(panel)]
	if !ok {
		http.Error(w, "unknown chart panel", http.StatusNotFound)
		return
	}
	s.proxyChartsUpstream(w, r, chartsProxyBasePath+"/"+group+"/"+leaf)
}

// proxyChartsUpstream is the shared read-only forward to the kbeads daemon for
// both the catalog listing and a single render. upstreamPath is a constant or
// a re-encoded /v1/charts/{id}; only the request's query string is forwarded.
// The daemon bearer is attached server-side and never exposed to the browser.
func (s *Server) proxyChartsUpstream(w http.ResponseWriter, r *http.Request, upstreamPath string) {
	if strings.TrimSpace(s.beadsHTTPAddr) == "" {
		// No daemon wired (tests / local dev without a beads HTTP addr). Be
		// explicit so the misconfiguration is visible rather than surfacing
		// as an empty chart.
		http.Error(w, "charts proxy unavailable", http.StatusServiceUnavailable)
		return
	}

	upstream := strings.TrimRight(s.beadsHTTPAddr, "/") + upstreamPath
	if raw := r.URL.RawQuery; raw != "" {
		upstream += "?" + raw
	}
	// Defensive: a malformed daemon base should surface as a 502, not a panic
	// deeper in the transport.
	if _, err := url.Parse(upstream); err != nil {
		s.logger.Error("charts proxy bad upstream URL", "error", err)
		http.Error(w, "charts upstream misconfigured", http.StatusBadGateway)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), chartsProxyTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, upstream, nil)
	if err != nil {
		s.logger.Error("charts proxy build request", "error", err)
		http.Error(w, "charts upstream error", http.StatusBadGateway)
		return
	}
	tok, err := s.daemonBearer(ctx)
	if err != nil {
		s.logger.Error("charts proxy daemon credential", "error", err)
		http.Error(w, "charts upstream error", http.StatusBadGateway)
		return
	}
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	req.Header.Set("Accept", "application/json")
	// Forward the conditional-request validator so the daemon can answer 304.
	if inm := r.Header.Get("If-None-Match"); inm != "" {
		req.Header.Set("If-None-Match", inm)
	}

	client := s.metricsHTTP
	if client == nil {
		client = &http.Client{Timeout: chartsProxyTimeout}
	}
	resp, err := client.Do(req)
	if err != nil {
		s.logger.Error("charts proxy upstream error", "error", err)
		http.Error(w, "charts upstream error", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	// Mirror the daemon's content type and its cache validators so the
	// browser's HTTP cache can re-validate this render on the next poll.
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	} else {
		w.Header().Set("Content-Type", "application/json")
	}
	if etag := resp.Header.Get("ETag"); etag != "" {
		w.Header().Set("ETag", etag)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "" {
		w.Header().Set("Cache-Control", cc)
	}

	// A 304 carries no body — write the status and stop.
	if resp.StatusCode == http.StatusNotModified {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.WriteHeader(resp.StatusCode)
	if _, err := io.Copy(w, resp.Body); err != nil {
		// Headers already on the wire; can't promote to an HTTP error.
		s.logger.Warn("charts proxy stream interrupted", "error", err)
	}
}
