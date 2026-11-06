package main

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/oidcauth"
)

// maxDecisionReflagReasonLen caps the optional reason posted with a reflag.
// Same ceiling as the resolve response / dismiss reason.
const maxDecisionReflagReasonLen = 10_000

// handleReflagDecision reflags an open decision from the interaction page:
// it bumps priority one step toward P0, records reflag_count/reflagged_at/
// reflagged_by (+ optional reflag_reason), adds the "escalated" label at
// P0/P1, and drops an audit comment. This mirrors `gb decision reflag`
// (runReflag in cmd/gb/decision.go) so the CLI and the web surface produce
// the same bead mutation.
//
// On success it re-renders the decision-detail context sidebar fragment so the
// reflag count / age / priority update in place without a full page reload.
// Errors re-render the same fragment with an inline error banner.
func (s *Server) handleReflagDecision(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "Missing bead id", http.StatusBadRequest)
		return
	}
	email := oidcauth.EmailFromContext(r.Context())
	if email == "" {
		// Defense-in-depth: the OIDC middleware should already block this.
		http.Error(w, "Unauthenticated", http.StatusUnauthorized)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad form", http.StatusBadRequest)
		return
	}
	reason := strings.TrimSpace(r.PostForm.Get("reason"))
	if len(reason) > maxDecisionReflagReasonLen {
		s.renderReflagError(w, r, id, email, "Reason is too long (max 10000 characters).", http.StatusRequestEntityTooLarge)
		return
	}

	bead, err := s.daemon.GetBead(r.Context(), id)
	if err != nil {
		var apiErr *beadsapi.APIError
		if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
			http.Error(w, "Bead not found", http.StatusNotFound)
			return
		}
		s.logger.Error("getting bead for reflag", "id", id, "error", err)
		http.Error(w, "Failed to load bead", http.StatusInternalServerError)
		return
	}
	if bead.Type != "decision" {
		http.Error(w, "Bead is not a decision", http.StatusBadRequest)
		return
	}
	if bead.Status == "closed" || bead.Status == "resolved" {
		// A resolved decision cannot be reflagged (the CLI refuses reflag on a
		// closed bead). Surface a friendly inline message.
		s.renderReflagError(w, r, id, email, "Decision is already resolved — reflag has no effect.", http.StatusConflict)
		return
	}

	if err := s.reflagDecisionBead(r, bead, email, reason); err != nil {
		s.logger.Error("reflagging decision", "id", id, "actor", email, "error", err)
		s.renderReflagError(w, r, id, email, "Failed to reflag decision.", http.StatusBadGateway)
		return
	}

	// Re-fetch so the bumped priority + reflag fields flow into the fragment.
	updated, err := s.daemon.GetBead(r.Context(), id)
	if err != nil {
		s.logger.Warn("re-fetching bead after reflag", "id", id, "error", err)
		updated = bead
	}
	s.renderDecisionContextFragment(w, r, updated, email, "")
}

// reflagDecisionBead applies the priority bump + reflag bookkeeping fields +
// escalated label + audit comment. It is the shared body between the HTTP
// handler and any future callers, mirroring runReflag's field set exactly so a
// web reflag is indistinguishable from a CLI reflag downstream.
//
// The three writes (priority, fields, label) are separate daemon round-trips
// because the beadsapi UpdateBead body ignores Fields (they go through the
// dedicated field-patch endpoint). A partial failure is reported to the caller;
// the audit comment is best-effort and never unwinds the reflag.
func (s *Server) reflagDecisionBead(r *http.Request, bead *beadsapi.BeadDetail, actor, reason string) error {
	ctx := r.Context()
	oldPriority := bead.Priority
	// Default reflag: bump one step toward P0, capped at 0 (critical). The web
	// surface does not expose an explicit target priority — that CLI escape
	// hatch stays CLI-only to keep the operator button single-purpose.
	newPriority := max(oldPriority-1, 0)

	count := 0
	if v := strings.TrimSpace(bead.Fields["reflag_count"]); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			count = n
		}
	}
	count++
	now := time.Now().UTC().Format(time.RFC3339)

	fields := map[string]any{
		"reflag_count": strconv.Itoa(count),
		"reflagged_at": now,
		"reflagged_by": actor,
	}
	if reason != "" {
		fields["reflag_reason"] = reason
	}

	if newPriority != oldPriority {
		if err := s.daemon.UpdateBead(ctx, bead.ID, beadsapi.UpdateBeadRequest{
			Priority: &newPriority,
		}); err != nil {
			return fmt.Errorf("updating priority: %w", err)
		}
	}
	if err := s.daemon.UpdateBeadFieldsTyped(ctx, bead.ID, fields); err != nil {
		return fmt.Errorf("updating reflag fields: %w", err)
	}

	// At P0/P1, add the "escalated" label so the Slack bridge re-posts an
	// ESCALATED notification (deduped per-bead with a TTL bridge-side).
	if newPriority <= 1 && !slices.Contains(bead.Labels, "escalated") {
		labels := append([]string(nil), bead.Labels...)
		labels = append(labels, "escalated")
		if err := s.daemon.UpdateBead(ctx, bead.ID, beadsapi.UpdateBeadRequest{
			Labels: &labels,
		}); err != nil {
			// Non-fatal: the priority bump + fields already landed.
			s.logger.Warn("adding escalated label on reflag", "id", bead.ID, "error", err)
		}
	}

	// Audit comment (best-effort — a comment failure must not unwind the reflag).
	auditMsg := fmt.Sprintf("reflagged by %s (via viewer): P%d → P%d (count=%d)", actor, oldPriority, newPriority, count)
	if reason != "" {
		auditMsg += "\nreason: " + reason
	}
	if err := s.daemon.AddComment(ctx, bead.ID, actor, auditMsg); err != nil {
		s.logger.Warn("recording reflag audit comment", "id", bead.ID, "error", err)
	}
	return nil
}

// renderReflagError re-renders the decision-context fragment with an inline
// error banner on the reflag control.
func (s *Server) renderReflagError(w http.ResponseWriter, r *http.Request, id, email, msg string, status int) {
	if respondAction(w, r, id, actionFailure(msg, status)) {
		return
	}
	bead, err := s.daemon.GetBead(r.Context(), id)
	if err != nil {
		http.Error(w, msg, status)
		return
	}
	w.WriteHeader(status)
	s.renderDecisionContextFragment(w, r, bead, email, msg)
}

// renderDecisionContextFragment renders the #decision-context sidebar as a
// standalone htmx fragment so a reflag re-render swaps just the context block
// (reflag count / age / priority / inline error) rather than the whole page.
func (s *Server) renderDecisionContextFragment(w http.ResponseWriter, r *http.Request, bead *beadsapi.BeadDetail, email, reflagErr string) {
	view := s.buildDecisionDetailView(r.Context(), bead, email, reflagErr)
	s.renderPartial(w, r, "templates/partials/decision_context.html", "decision_context", map[string]any{
		"DecisionDetail": view,
		"AuthorEmail":    email,
	})
}
