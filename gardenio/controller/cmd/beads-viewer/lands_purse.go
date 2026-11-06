package main

import (
	"errors"
	"net/http"

	"gardenio/controller/internal/lands/purse"
)

// GET /api/lands/{land}/purse is a tenant land's purse (Lands Track B spec
// §3.4, step 8.2) as JSON: credits and, for the operator, the same amounts at
// cost (internal/lands/purse). It is admin-only: the purse reads account and
// credit_rate beads, which a tenant's bearer cannot, and the at-cost dollars
// are the operator's. A tenant's credits-only view rides the land header
// (step 8.8, purse.Purse.CreditsOnly).
//
// 404 when the land is not a project or is an operator land (no purse); 502
// when the daemon read fails, which a caller deciding a spawn treats as
// unknown, the wall.
func (s *Server) registerLandsPurseRoute(mux routeRegistrar) {
	mux.HandleFunc("GET /api/lands/{land}/purse", s.handleLandPurse)
}

func (s *Server) handleLandPurse(w http.ResponseWriter, r *http.Request) {
	land := r.PathValue("land")
	p, err := purse.Read(r.Context(), s.daemon, land, s.now())
	switch {
	case errors.Is(err, purse.ErrNotLand), errors.Is(err, purse.ErrOperatorLand):
		writeJSONStatus(w, http.StatusNotFound, map[string]string{"error": err.Error()})
	case err != nil:
		writeJSONStatus(w, http.StatusBadGateway, map[string]string{"error": "purse unknown: " + err.Error(), "state": purse.StateUnknown})
	default:
		w.Header().Set("Cache-Control", "no-store")
		writeJSONStatus(w, http.StatusOK, p)
	}
}
