package server

import (
	"net/http"
)

// handleAcceptanceMetrics serves the three C5 acceptance signals
// (ka-0000004365) as a single JSON payload at
// `GET /v1/metrics/acceptance`. RoleViewer — the gardener pulls this
// every cycle to decide whether the operated_by multi-user readiness
// epic (ka-0000004247) is done. Returns 500 on store failure; success
// always emits all three sub-metrics (zero-population queries return
// total=0 with fraction=0, not 404).
func (s *BeadsServer) handleAcceptanceMetrics(w http.ResponseWriter, r *http.Request) {
	metrics, err := s.store.AcceptanceMetrics(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, metrics)
}
