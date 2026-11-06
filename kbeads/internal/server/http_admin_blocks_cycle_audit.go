package server

import "net/http"

// handleAuditBlocksCycles handles GET /v1/admin/audit-blocks-cycles. It scans
// the whole blocks graph and returns every cycle (strongly-connected tangle)
// present — the read-only counterpart to the write-time blocks-cycle guard.
// Admin-only. See ka-0000007984 and the `kd admin audit-blocks-cycles` driver.
func (s *BeadsServer) handleAuditBlocksCycles(w http.ResponseWriter, r *http.Request) {
	audit, err := s.AuditBlocksCycles(r.Context())
	if err != nil {
		writeServerError(r.Context(), w, "handleAuditBlocksCycles", err, "failed to audit blocks cycles")
		return
	}
	writeJSON(w, http.StatusOK, audit)
}
