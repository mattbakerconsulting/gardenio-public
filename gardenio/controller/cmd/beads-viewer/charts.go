package main

// /charts — the fleet Observability Hub. Originally (inline-charts→Grafana
// deprecation, epic ka-0000006518) this embedded a single hardcoded dashboard
// (uid gardenio-charts-candidate) as a fixed 5-panel slice. As of the
// Observability Hub (epic ka-0000005362, W2 task ka-0000000773) the page
// instead enumerates EVERY type=dashboard bead at request time — see
// dashboard_hub.go (buildDashboardHub) for the enumeration + grouping + cache,
// and templates/charts.html / W3 for the grouped-card layout. `gb grafana put`
// of a new dashboard bead now makes it appear with no code change.
//
// The viewer never sees or chooses a query — it only points a sandboxed iframe
// at the operator-set Grafana base URL plus a charset-validated uid and a
// numeric panel id (grafanaembed.HTML, internal/grafanaembed). Degraded mode: when
// GRAFANA_BASE_URL is unset, each embed renders grafanaembed.HTML's quiet
// link-card fallback rather than an iframe pointed nowhere, and the "Open in
// Grafana ↗" header link is suppressed.
//
// The former per-bead charts panel (the "fleet-wide" Grafana embeds injected
// into every agent/account/project bead page) was removed in ka-0000005018 —
// operators read the dedicated /charts hub and /costs pages instead.

import "net/http"

// handleCharts renders the Observability Hub: every type=dashboard bead,
// enumerated at request time and grouped into per-project sections of cards
// (each a live hero solo-panel embed + deep links). Backed by a short-TTL cache
// (dashboard_hub.go). When GRAFANA_BASE_URL is unset the hero embeds degrade to
// quiet link cards and the header "Open in Grafana ↗" link is suppressed.
func (s *Server) handleCharts(w http.ResponseWriter, r *http.Request) {
	hub := s.dashboardHub(r.Context())
	s.render(w, r, "charts.html", map[string]any{
		"Hub": hub,
		// GrafanaBaseURL gates the header "Open in Grafana ↗" link: rendered
		// only when Grafana is configured, so non-Grafana envs show no dead
		// link (task ka-0000006706, epic ka-0000007895).
		"GrafanaBaseURL": s.grafanaBaseURL,
		// The garden's Costs panel draws the costs dashboards this hub lists
		// (gd-0WYDQUDLFf); the hub stays, this is a cross-link.
		"GardenCostsHref": s.gardenCostsHref(r),
	})
}

// gardenCostsHref is the garden's fleet Costs panel (?costs=fleet, WB-17), linked
// in the /charts header for a viewer who reaches /garden, or "" for one who
// does not (/costs has had its own "See it in the garden" header link since
// phase 2). The old pages stay (owner, 2026-10-27, gd-0WYDQUDLFf): the link
// is a cross-reference between two views of the same charts, not a step of
// garden_retire.go's retirement.
func (s *Server) gardenCostsHref(r *http.Request) string {
	if !s.reachesGarden(r) {
		return ""
	}
	return gardenPanelURL(s.basePath, nil, "costs", "fleet")
}
