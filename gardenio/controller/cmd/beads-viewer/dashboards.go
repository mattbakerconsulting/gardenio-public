package main

// dashboards.go — the operator-engagement + security dashboards page
// (task ka-0000004892, epic ka-0000001609; customer-1 telemetry plan §1 / §5).
//
// This is the native-in-viewer counterpart to /charts (which fronts Grafana).
// The page is a thin shell: it renders the panel scaffolding + tab bar and
// links the vendored ECharts library plus the dashboards driver
// (templates/static/dashboards.js). Every number on the page is fetched
// client-side from the daemon's engagement/security/uniqueness chart endpoints
// through the read-only viewer proxy (charts_proxy.go):
//
//	GET /api/v1/charts/uniqueness            → DAU/WAU/MAU
//	GET /api/v1/charts/engagement/activity   → activity-by-surface + top operators
//	GET /api/v1/charts/engagement/spend      → per-operator spend
//	GET /api/v1/charts/engagement/sessions   → per-operator session-duration quantiles
//	GET /api/v1/charts/security/authz        → authz-denial breakdown
//	GET /api/v1/charts/security/decisions    → decision lifecycle + escalations
//	GET /api/v1/charts/security/token-slo    → token-revocation SLO compliance
//
// PRIVACY CONTRACT (plan §4, the whole reason this page exists in the viewer
// rather than Grafana): operator identities are resolved SERVER-SIDE in the
// daemon. Each metric-label row already carries a pre-resolved `display` name;
// the web tier NEVER resolves operator_id, and the daemon never sends email /
// slack_user_id in a metric label. When an operator has exercised the
// right-to-be-forgotten, the row carries `forgotten:true` and the driver badges
// it as redacted rather than showing any residual identity.

import "net/http"

// handleDashboards renders the engagement + security dashboards shell. The two
// dashboards share one page with a client-side section tab bar (Engagement /
// Security); the window selector (1d/7d/30d) is shared across every panel that
// accepts a window. All data loads client-side, so the handler itself is a
// pure render with no daemon round-trip — the panels degrade to their own
// loading/empty/error states when the charts backend is unconfigured (proxy
// 503), exactly like /charts' catalog panels.
func (s *Server) handleDashboards(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, "dashboards.html", nil)
}
