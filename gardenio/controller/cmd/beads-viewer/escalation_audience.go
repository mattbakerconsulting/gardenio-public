package main

import (
	"errors"
	"net/http"
	"strings"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/oidcauth"
)

// Escalation audience on the escalation page (gd-bmOiteQi36): the card says
// who the escalation is for (owner, operator or desk, the shared rules in
// internal/beadsapi/escalation_audience.go) and, for a gardenio-operator or
// admin, offers a re-route. The daemon is the authority on who may re-route:
// the write goes to POST /v1/escalations/{id}/audience on the viewer's own
// bearer, and kbeads refuses anyone below gardenio-operator. Approval is untouched: CanApprove never reads audience.

// audienceView fills the audience slots of an escalation card.
func audienceView(v *escalationView, f map[string]string) {
	v.Audience = beadsapi.EscalationAudience(f)
	v.AudienceForced = beadsapi.EscalationAudienceForced(f)
	v.AudienceDefault = !v.AudienceForced && !beadsapi.ValidAudience(strings.ToLower(strings.TrimSpace(f["audience"])))
	for _, a := range beadsapi.Audiences {
		if a != v.Audience {
			v.RouteTo = append(v.RouteTo, a)
		}
	}
}

// viewerMayRouteEscalation reports whether the viewer is a gardenio-operator
// or admin, who may re-route an escalation.
func viewerMayRouteEscalation(r *http.Request) bool {
	return oidcauth.EmailFromContext(r.Context()) != "" &&
		oidcauth.HasMinimumRole(oidcauth.RolesFromContext(r.Context()), oidcauth.RoleOperator)
}

// handleRouteEscalation handles POST /b/{id}/escalation/route: re-route the
// escalation through the daemon's route endpoint (which records who did it as
// a comment) and re-render the card.
func (s *Server) handleRouteEscalation(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "Missing bead id", http.StatusBadRequest)
		return
	}
	email := oidcauth.EmailFromContext(r.Context())
	if email == "" {
		http.Error(w, "Unauthenticated", http.StatusUnauthorized)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad form", http.StatusBadRequest)
		return
	}
	to := strings.ToLower(strings.TrimSpace(r.PostForm.Get("audience")))
	if !beadsapi.ValidAudience(to) {
		s.renderEscalationError(w, r, id, email, "Pick owner, operator or desk.", http.StatusBadRequest)
		return
	}
	if !viewerMayRouteEscalation(r) {
		s.renderEscalationError(w, r, id, email, "Only a gardenio-operator or admin can re-route an escalation.", http.StatusForbidden)
		return
	}
	bead, err := s.daemon.GetBead(r.Context(), id)
	if err != nil {
		var apiErr *beadsapi.APIError
		if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
			http.Error(w, "Bead not found", http.StatusNotFound)
			return
		}
		s.logger.Error("getting bead for escalation route", "id", id, "error", err)
		http.Error(w, "Failed to load bead", http.StatusInternalServerError)
		return
	}
	if bead.Type != "escalation" {
		http.Error(w, "Bead is not an escalation", http.StatusBadRequest)
		return
	}
	if beadsapi.EscalationAudienceForced(bead.Fields) && to != beadsapi.AudienceOwner {
		s.renderEscalationError(w, r, id, email, "This escalation needs credentials, is secret or names an approver, so it always goes to the owner.", http.StatusConflict)
		return
	}
	from := beadsapi.EscalationAudience(bead.Fields)
	if _, err := s.daemon.RouteEscalation(r.Context(), id, beadsapi.RouteEscalationRequest{Audience: to}); err != nil {
		var apiErr *beadsapi.APIError
		if errors.As(err, &apiErr) && apiErr.StatusCode >= 400 && apiErr.StatusCode < 500 {
			s.logger.Warn("escalation route refused by daemon", "id", id, "actor", email, "error", err)
			s.renderEscalationError(w, r, id, email, escalationRefusalMessage("re-route", apiErr), http.StatusForbidden)
			return
		}
		s.logger.Error("escalation route failed", "id", id, "actor", email, "error", err)
		s.renderEscalationError(w, r, id, email, "Failed to re-route this escalation.", http.StatusBadGateway)
		return
	}
	s.logger.Info("escalation re-routed from bead page", "id", id, "from", from, "to", to, "actor", email)
	updated, ferr := s.daemon.GetBead(r.Context(), id)
	if ferr != nil {
		updated = bead
	}
	s.renderEscalationPartial(w, r, updated, email, "", http.StatusOK)
}
