// Package bridge — minimal Grafana HTTP API read client.
//
// The beads→Grafana write path (grafana_configmap.go) needs no Grafana auth —
// it only touches the k8s API and lets the sidecar provision. The reverse
// direction (Grafana→beads read-back, W2-impl-b) DOES need to read dashboard
// JSON out of Grafana, so this client wraps the two read endpoints the
// read-back poll uses:
//
//	GET /api/search?type=dash-db         → list dashboards (uid + title)
//	GET /api/dashboards/uid/<uid>        → fetch one dashboard's JSON model
//
// Auth is a Grafana service-account token (Authorization: Bearer …), injected
// via k8s secret (W2 design doc §7 path (a)). The bridge talks to the in-cluster
// Grafana Service directly, behind the AVA auth-proxy edge.
//
// Design: gardenio/docs/research/obs-w2-grafana-bridge.md §2, §4b, §7.
package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// GrafanaReader is the read surface the read-back poll targets. Defined as an
// interface so the poller can be unit-tested with a fake Grafana.
type GrafanaReader interface {
	// Search lists dashboards Grafana knows about (type=dash-db).
	Search(ctx context.Context) ([]GrafanaDashboardRef, error)
	// DashboardByUID fetches one dashboard's JSON model and its Grafana
	// version int. Returns (nil, 0, nil) when the uid is not found (404) so
	// callers can treat "deleted in Grafana" distinctly from a transport
	// error.
	DashboardByUID(ctx context.Context, uid string) (model []byte, version int, err error)
}

// GrafanaDashboardRef is one entry from GET /api/search.
type GrafanaDashboardRef struct {
	UID   string `json:"uid"`
	Title string `json:"title"`
	Type  string `json:"type"`
}

// GrafanaClient is an HTTP client for the Grafana read endpoints.
type GrafanaClient struct {
	baseURL string
	token   string
	http    *http.Client
	logger  *slog.Logger
}

// GrafanaClientConfig configures a GrafanaClient.
type GrafanaClientConfig struct {
	// BaseURL is the Grafana root, e.g. http://gardenio-grafana:3000 or a
	// sub-path root (…/grafana). Trailing slash is trimmed.
	BaseURL string
	// Token is the Grafana service-account token (Bearer). May be empty when
	// Grafana runs with anonymous/auth-proxy access for in-cluster reads.
	Token  string
	Client *http.Client
	Logger *slog.Logger
}

// NewGrafanaClient constructs a GrafanaClient. A nil http client defaults to a
// 15s-timeout client; a nil logger defaults to slog.Default.
func NewGrafanaClient(cfg GrafanaClientConfig) *GrafanaClient {
	c := cfg.Client
	if c == nil {
		c = &http.Client{Timeout: 15 * time.Second}
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &GrafanaClient{
		baseURL: strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/"),
		token:   strings.TrimSpace(cfg.Token),
		http:    c,
		logger:  logger,
	}
}

// newRequest builds a GET request with the Bearer auth header (when set).
func (g *GrafanaClient) newRequest(ctx context.Context, path string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.baseURL+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if g.token != "" {
		req.Header.Set("Authorization", "Bearer "+g.token)
	}
	return req, nil
}

// Search returns all dash-db dashboards Grafana knows about.
func (g *GrafanaClient) Search(ctx context.Context) ([]GrafanaDashboardRef, error) {
	req, err := g.newRequest(ctx, "/api/search?type=dash-db&limit=5000")
	if err != nil {
		return nil, fmt.Errorf("grafana search: build request: %w", err)
	}
	resp, err := g.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("grafana search: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("grafana search: status %d: %s", resp.StatusCode, snippet(body))
	}
	var refs []GrafanaDashboardRef
	if err := json.Unmarshal(body, &refs); err != nil {
		return nil, fmt.Errorf("grafana search: decode: %w", err)
	}
	return refs, nil
}

// dashboardByUIDResponse is the GET /api/dashboards/uid/<uid> envelope. The
// dashboard model lives under .dashboard; .meta.version is the authoritative
// server-side version (the model's own version can lag).
type dashboardByUIDResponse struct {
	Meta struct {
		Version int `json:"version"`
	} `json:"meta"`
	Dashboard json.RawMessage `json:"dashboard"`
}

// DashboardByUID fetches one dashboard's JSON model and Grafana version.
// A 404 returns (nil, 0, nil) so callers distinguish "not in Grafana" from a
// transport error.
func (g *GrafanaClient) DashboardByUID(ctx context.Context, uid string) ([]byte, int, error) {
	req, err := g.newRequest(ctx, "/api/dashboards/uid/"+uid)
	if err != nil {
		return nil, 0, fmt.Errorf("grafana get uid %q: build request: %w", uid, err)
	}
	resp, err := g.http.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("grafana get uid %q: %w", uid, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if resp.StatusCode == http.StatusNotFound {
		return nil, 0, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, 0, fmt.Errorf("grafana get uid %q: status %d: %s", uid, resp.StatusCode, snippet(body))
	}
	var env dashboardByUIDResponse
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, 0, fmt.Errorf("grafana get uid %q: decode: %w", uid, err)
	}
	if len(env.Dashboard) == 0 {
		return nil, 0, fmt.Errorf("grafana get uid %q: empty dashboard in response", uid)
	}
	version := env.Meta.Version
	if version == 0 {
		// Fall back to the version embedded in the model.
		if meta, merr := ParseDashboardMeta(env.Dashboard); merr == nil {
			version = meta.Version
		}
	}
	return env.Dashboard, version, nil
}

// snippet trims a response body for inclusion in an error message.
func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		return s[:200] + "…"
	}
	return s
}
