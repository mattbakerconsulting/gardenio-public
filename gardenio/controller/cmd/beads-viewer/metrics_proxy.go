package main

// /api/v1/query_range read-only proxy to an in-cluster VictoriaMetrics
// vmsingle (ka-0000002747).
//
// The /charts tab (lean graphs-in-beads-viewer slice, epic ka-0000003954)
// renders a handful of time-series panels backed by an in-cluster vmsingle.
// Rather than expose vmsingle to the browser directly — which would require
// its own ingress + auth, and leak the raw upstream — the viewer fronts a
// single, deliberately narrow proxy: GET /api/v1/query_range, forwarded to
// `<upstream>/api/v1/query_range` and nothing else.
//
// Why so narrow:
//   - Read-only: only the GET range-query endpoint is reachable. vmsingle's
//     write path (/api/v1/import, /api/v1/write), delete API, and the
//     instant-query/labels/series surface are all unreachable through this
//     proxy. The route registration itself is `GET /api/v1/query_range`, so
//     a hand-crafted POST or a path like /api/v1/admin/tsdb/... never reaches
//     the handler.
//   - Allowlist by construction: there is no path passthrough. The upstream
//     path is a constant; the only thing forwarded from the client is the
//     query string (query=, start=, end=, step=). An attacker cannot pivot
//     the proxy at another upstream path.
//
// Why proxy and not redirect: the panels run inside the already-authenticated
// viewer origin; a redirect to the cluster-internal vmsingle Service URL is
// not reachable from the browser, and a public vmsingle ingress is exactly
// what this slice is avoiding. One in-cluster hop is cheap for the small
// JSON range payloads the panels request.
//
// Degraded mode: when no upstream is configured (local dev / tests without a
// vmsingle), the handler returns 503 so an operator sees the misconfiguration
// rather than a confusing empty graph. main wires the upstream from
// VMSINGLE_QUERY_URL, defaulting to the in-cluster gardenio-vmsingle Service.

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// defaultVMSingleQueryURL is the in-cluster VictoriaMetrics vmsingle base URL.
// vmsingle serves its HTTP API (including /api/v1/query_range) on :8428; the
// Service name matches the Helm chart's `gardenio.vmsingle.fullname`
// (`<release>-vmsingle`, default release `gardenio`). Operators override the
// whole base via VMSINGLE_QUERY_URL when the Service name or port differs.
const defaultVMSingleQueryURL = "http://gardenio-vmsingle:8428"

// vmsingleQueryRangePath is the single upstream path this proxy will ever
// reach. It is a constant, never derived from the request — that constancy
// is the allowlist.
const vmsingleQueryRangePath = "/api/v1/query_range"

// metricsProxyTimeout caps an individual range-query fetch. Range queries
// over modest windows return quickly; a query that approaches this ceiling
// is either a pathological time range or a struggling upstream, and either
// way the panel should fail fast rather than pin a connection.
const metricsProxyTimeout = 20 * time.Second

// metricsProxyCacheControl keeps range responses briefly cacheable but
// private — the data is not user-specific, but these URLs are gated on the
// viewer's session and a short max-age lets a refreshing dashboard reuse a
// just-fetched window without re-hitting vmsingle.
const metricsProxyCacheControl = "private, max-age=15"

// SetMetricsProxy wires the upstream vmsingle base URL used by the
// /api/v1/query_range proxy. An empty/blank upstream leaves the handler in
// degraded mode (503). Called once from main; idempotent (last write wins).
func (s *Server) SetMetricsProxy(upstreamBaseURL string) {
	s.metricsUpstream = strings.TrimRight(strings.TrimSpace(upstreamBaseURL), "/")
	if s.metricsHTTP == nil {
		s.metricsHTTP = &http.Client{Timeout: metricsProxyTimeout}
	}
}

// handleMetricsQueryRange forwards a range query to vmsingle's
// /api/v1/query_range. It is read-only and allowlisted by construction: the
// route is registered for GET only, and the upstream path is the constant
// vmsingleQueryRangePath — only the request's query string is forwarded.
func (s *Server) handleMetricsQueryRange(w http.ResponseWriter, r *http.Request) {
	if s.metricsUpstream == "" {
		// No upstream wired (local dev / tests without a vmsingle, or
		// VMSINGLE_QUERY_URL unset). Be explicit so the misconfiguration
		// is visible rather than surfacing as an empty graph.
		http.Error(w, "metrics proxy unavailable", http.StatusServiceUnavailable)
		return
	}

	// A `query` parameter is required by vmsingle; reject early with a 400
	// so a malformed panel request doesn't burn an upstream round-trip.
	if strings.TrimSpace(r.URL.Query().Get("query")) == "" {
		http.Error(w, "missing query parameter", http.StatusBadRequest)
		return
	}

	// Build the upstream URL from the constant base+path plus the client's
	// raw query string. The path is never taken from the request, so the
	// proxy cannot be redirected at another vmsingle endpoint.
	upstream := s.metricsUpstream + vmsingleQueryRangePath
	if raw := r.URL.RawQuery; raw != "" {
		upstream += "?" + raw
	}
	// Defensive: a malformed upstream base (bad VMSINGLE_QUERY_URL) should
	// surface as a 502, not a panic deeper in the transport.
	if _, err := url.Parse(upstream); err != nil {
		s.logger.Error("metrics proxy bad upstream URL", "error", err)
		http.Error(w, "metrics upstream misconfigured", http.StatusBadGateway)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), metricsProxyTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, upstream, nil)
	if err != nil {
		s.logger.Error("metrics proxy build request", "error", err)
		http.Error(w, "metrics upstream error", http.StatusBadGateway)
		return
	}
	req.Header.Set("Accept", "application/json")

	client := s.metricsHTTP
	if client == nil {
		client = &http.Client{Timeout: metricsProxyTimeout}
	}
	resp, err := client.Do(req)
	if err != nil {
		s.logger.Error("metrics proxy upstream error", "error", err)
		http.Error(w, "metrics upstream error", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	// Mirror vmsingle's content type (application/json) and status. vmsingle
	// returns a structured JSON error body with a 4xx/5xx on a bad query; we
	// pass it through so the panel can surface the upstream's own message.
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	} else {
		w.Header().Set("Content-Type", "application/json")
	}
	if resp.StatusCode == http.StatusOK {
		w.Header().Set("Cache-Control", metricsProxyCacheControl)
	}
	w.WriteHeader(resp.StatusCode)
	if _, err := io.Copy(w, resp.Body); err != nil {
		// Headers already on the wire; can't promote to an HTTP error.
		s.logger.Warn("metrics proxy stream interrupted", "error", err)
	}
}

// metricsQueryURLFromEnv returns the configured vmsingle base URL, defaulting
// to the in-cluster Service. Mirrors the image proxy's env-derivation style.
func metricsQueryURLFromEnv(getenv func(string) string) string {
	if u := strings.TrimSpace(getenv("VMSINGLE_QUERY_URL")); u != "" {
		return u
	}
	return defaultVMSingleQueryURL
}
