package main

import (
	"context"
	"net/http"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/oidcauth"
)

// handlePeelCatalog renders /peel-catalog — the R4 pull-on-demand "menu" of
// peel-eligible merged monorepo-dark MRs (design doc ka-0000006592 §4.2). It is
// a READ surface: it fetches the scored dark MR beads, ranks the eligible ones
// by peel_score, and renders each with size / risk / test-coverage /
// provenance and a "request peel" affordance. Browsing opens no upstream MR —
// the write half (a peel-request decision bead) is a separate operator-gated
// POST (handleRequestPeel).
func (s *Server) handlePeelCatalog(w http.ResponseWriter, r *http.Request) {
	beads, truncated, err := s.fetchPeelCatalogMRs(r)
	view := &peelCatalogView{
		BasePath:   s.basePath,
		CanRequest: canRequestPeel(r.Context()),
		Truncated:  truncated,
		Limit:      peelCatalogListLimit,
	}
	data := map[string]any{
		"Catalog": view,
	}
	if err != nil {
		s.logger.Error("listing peel-catalog MRs", "error", err)
		data["Error"] = "Failed to load the peel catalog."
		s.render(w, r, "peel_catalog.html", data)
		return
	}

	view.Eligible, view.Parked = buildPeelCatalogRows(beads)
	s.render(w, r, "peel_catalog.html", data)
}

// fetchPeelCatalogMRs pulls the scored dark MR beads the catalog ranks. It
// selects type=merge-request beads carrying ANY of the peel classification
// labels (eligible or parked) so a single query returns both menu sections;
// the eligible/parked split is decided per-bead by buildPeelCatalogRows. The
// dark discriminator is not re-applied here because the peel:* labels are only
// ever written by the dark forward-engineering gardener — a non-dark MR never
// carries them.
//
// The bool return reports whether the fetch hit the cap (drives the truncation
// banner).
func (s *Server) fetchPeelCatalogMRs(r *http.Request) ([]*beadsapi.BeadDetail, bool, error) {
	// A peel is scored on a MERGED dark MR, whose canonical bead is
	// status=closed (config.go type:merge-request). Include the live statuses
	// too so a slice scored before its bead closed still appears.
	res, err := s.daemon.ListBeadsFiltered(r.Context(), beadsapi.ListBeadsQuery{
		Types:     []string{"merge-request"},
		Statuses:  []string{"open", "in_progress", "closed", "blocked", "deferred"},
		LabelsAny: []string{peelCatalogLabelEligible, peelCatalogLabelParked},
		Limit:     peelCatalogListLimit,
	})
	if err != nil {
		return nil, false, err
	}
	truncated := len(res.Beads) >= peelCatalogListLimit
	return res.Beads, truncated, nil
}

// canRequestPeel reports whether the request's viewer may file a peel request,
// mirroring the operatorOnly gate on POST /peel-catalog/{id}/request so the UI
// only offers a button that will actually work. It must be authenticated AND
// (when OIDC roles are present) hold at least the operator role. When no roles
// are in context it passes through — matching oidcauth.RequireRole, which only
// enforces RBAC for OIDC-authenticated users (internal/dev callers allowed).
func canRequestPeel(ctx context.Context) bool {
	if oidcauth.EmailFromContext(ctx) == "" {
		return false
	}
	roles := oidcauth.RolesFromContext(ctx)
	if len(roles) == 0 {
		return true
	}
	return oidcauth.HasMinimumRole(roles, oidcauth.RoleOperator)
}
