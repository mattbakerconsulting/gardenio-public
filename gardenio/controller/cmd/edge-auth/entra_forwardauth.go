package main

import (
	"errors"
	"net/http"

	"gardenio/controller/internal/edgeauth"
)

// handleEntraForwardauth is the strict delegated-token gate used only by the
// private Ziti /beads direct-bearer route. Traefik preserves the original
// Authorization header after this 2xx response, so beads-viewer independently
// re-verifies the same token before serving the request. Browser-cookie traffic
// stays on the existing oauth2-proxy route and never calls this endpoint.
func (s *server) handleEntraForwardauth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	auditID := newAuditID(s.now())
	rec := s.baseAuditRecord(r, auditEndpointEntraForwardauth, "", auditID)
	rec.Metadata.AuthMode = auditAuthModeEntra

	if s.entraVerifier == nil {
		s.failAudit(rec, auditDetailDenyEntraForwardauthOff)
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"entra_forwardauth_not_configured"}`))
		return
	}

	id, err := s.entraVerifier.verify(r.Context(), r)
	switch {
	case err == nil:
		// continue below
	case errors.Is(err, errEntraMissing), errors.Is(err, errEntraInvalid):
		s.failAudit(rec, auditDetailDenyInvalidEntra)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid_entra_token"}`))
		return
	case errors.Is(err, errEntraForbidden):
		s.failAudit(rec, auditDetailDenyEntraForbidden)
		s.metrics.record403(auditEndpointEntraForwardauth, auditDetailDenyEntraForbidden)
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"entra_forbidden"}`))
		return
	default:
		s.logger.Warn("edge-auth: Entra ForwardAuth verifier unavailable", "error", err)
		s.failAudit(rec, auditDetailDenyEntraUnavailable)
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"entra_unavailable"}`))
		return
	}

	if id.Email == "" {
		s.failAudit(rec, auditDetailDenyInvalidEntra)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid_entra_token"}`))
		return
	}

	w.Header().Set(headerAuditID, auditID)
	s.okAudit(rec, &edgeauth.Identity{Email: id.Email, Name: id.Email})
	w.WriteHeader(http.StatusAccepted)
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}
