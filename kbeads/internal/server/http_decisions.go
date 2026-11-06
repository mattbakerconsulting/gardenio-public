package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"maps"
	"net/http"
	"strconv"
	"strings"
	"time"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
)

// handleGetDecision handles GET /v1/decisions/{id}.
// Returns the bead as both a decision fields object and the full issue data,
// matching the shape expected by the kbeads3d frontend.
func (s *BeadsServer) handleGetDecision(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "id is required")
		return
	}

	bead, err := s.store.GetBead(r.Context(), id)
	if errors.Is(err, sql.ErrNoRows) || bead == nil {
		writeError(w, http.StatusNotFound, "decision not found")
		return
	}
	if err != nil {
		writeServerError(r.Context(), w, "handleGetDecision", err, "failed to get decision")
		return
	}
	// Enforce cross-project read scope (ka-0000008132) — the by-id twin of the
	// handleGetBead gate. Without it an out-of-scope OIDC user / fleet token
	// could read a decision bead's full contents through this endpoint even
	// though GET /v1/beads/{id} on the same id answers it as missing.
	if !s.authorizeBeadRead(w, r, bead, "decision not found") {
		return
	}
	if err := s.propagateReadTaint(r.Context(), bead.ID); err != nil {
		writeServerError(r.Context(), w, "handleGetDecision", err, "failed to propagate read taint")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"decision": extractDecisionFields(bead),
		"issue":    bead,
	})
}

// handleListDecisions handles GET /v1/decisions.
// Lists beads of type=decision, with optional status filter.
func (s *BeadsServer) handleListDecisions(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	filter := model.BeadFilter{
		Type: []model.BeadType{model.TypeDecision},
		Sort: "-created_at",
	}
	if v := q.Get("status"); v != "" {
		for _, st := range strings.Split(v, ",") {
			filter.Status = append(filter.Status, model.Status(st))
		}
	} else {
		filter.Status = []model.Status{model.StatusOpen, model.StatusInProgress}
	}
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			filter.Limit = n
		}
	}
	// Route through clampListBeadsLimit so this hand-built filter shares the
	// maxListBeadsLimit OOM choke point (ka-0000006889) like every other
	// ListBeads path — an unclamped limit here would let a RoleViewer
	// materialize the whole table in daemon heap.
	filter.Limit = clampListBeadsLimit(filter.Limit, 50)

	// Enforce cross-project read scope (ka-0000008132): inject the fleet-token
	// project: label (AND) and the OIDC allowed-projects labels (OR), exactly
	// as handleListBeads does, so this list-twin cannot leak out-of-scope
	// decision beads (operation context / requested_by / options).
	filter = s.scopeListFilter(r.Context(), filter)

	beads, _, err := s.store.ListBeads(r.Context(), filter)
	if err != nil {
		writeServerError(r.Context(), w, "handleListDecisions", err, "failed to list decisions")
		return
	}
	if err := s.propagateReadTaintBeads(r.Context(), beadIDsOf(beads)); err != nil {
		writeServerError(r.Context(), w, "handleListDecisions", err, "failed to propagate read taint")
		return
	}

	decisions := make([]map[string]any, 0, len(beads))
	for _, b := range beads {
		decisions = append(decisions, map[string]any{
			"decision": extractDecisionFields(b),
			"issue":    b,
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{"decisions": decisions})
}

// resolveDecisionRequest is the JSON body for POST /v1/decisions/{id}/resolve.
// RespondedBy is ignored — the server derives responder attribution from the
// authenticated context so callers cannot forge the decision's responded_by
// field (AUDIT-2026-04-18 §C3; ka-0000000687).
type resolveDecisionRequest struct {
	SelectedOption string `json:"selected_option"`
	ResponseText   string `json:"response_text"`
	RespondedBy    string `json:"responded_by,omitempty"`
}

// handleResolveDecision handles POST /v1/decisions/{id}/resolve.
// Merges response fields into the bead, then closes it.
func (s *BeadsServer) handleResolveDecision(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "id is required")
		return
	}

	var req resolveDecisionRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}

	if req.SelectedOption == "" && req.ResponseText == "" {
		writeError(w, http.StatusBadRequest, "selected_option or response_text is required")
		return
	}

	// Enforce cross-project write-authz before mutating (ka-0000002666).
	// requireRole(RoleOperator) short-circuits for non-OIDC callers, so a
	// project-bound fleet token would otherwise reach the merge + close below
	// with no project-scope check — the same 403 GET /v1/decisions/{id} returns.
	if _, ok := s.authorizeResolutionTarget(w, r, id, "decision", "handleResolveDecision"); !ok {
		return
	}

	// Attribution is derived from the authenticated identity, never from
	// the request body, to prevent audit-log spoofing (AUDIT-2026-04-18
	// §C3; ka-0000000687). The client-supplied responded_by is logged and
	// ignored. decisionAttribution layers the static-token → human-operator
	// promotion over actorFromContext so a viewer/bridge that resolves the
	// decision on a human's behalf records the human (ka-0000001759), and
	// falls back to the body only in no-auth dev/e2e mode.
	respondedBy := s.decisionAttribution(r.Context(), req.RespondedBy)
	if req.RespondedBy != "" && req.RespondedBy != respondedBy {
		slog.Warn("ignoring client-supplied responded_by on decision resolve",
			"bead", id,
			"client_supplied", req.RespondedBy,
			"derived", respondedBy,
		)
	}

	// Merge resolution fields.
	respondedAt := time.Now().UTC().Format(time.RFC3339)
	extra := map[string]any{
		"responded_at": respondedAt,
	}
	if req.SelectedOption != "" {
		extra["chosen"] = req.SelectedOption
	}
	if req.ResponseText != "" {
		extra["response_text"] = req.ResponseText
	}
	if respondedBy != "" {
		extra["responded_by"] = respondedBy
	}
	maps.Copy(extra, s.decisionResolverStamp(r.Context()))

	if err := s.mergeBeadFields(r.Context(), id, extra); err != nil {
		slog.Warn("failed to merge decision fields", "bead", id, "error", err)
	}

	closedBy := respondedBy
	bead, err := s.store.CloseBead(r.Context(), id, closedBy)
	if errors.Is(err, sql.ErrNoRows) || bead == nil {
		writeError(w, http.StatusNotFound, "decision not found")
		return
	}
	if err != nil {
		writeServerError(r.Context(), w, "handleResolveDecision", err, "failed to close decision")
		return
	}

	s.recordAndPublish(r.Context(), events.TopicBeadClosed, bead.ID, closedBy, events.BeadClosed{
		Bead:     bead,
		ClosedBy: closedBy,
		DecisionAnswer: &events.DecisionAnswer{
			Chosen:       req.SelectedOption,
			ResponseText: req.ResponseText,
			RespondedBy:  respondedBy,
			RespondedAt:  respondedAt,
		},
	})
	s.recordCloseClient(r.Context(), bead.ID, closedBy)

	// Run the daemon-side close cascade the generic close path runs. A resolved
	// decision parks the still-open work it was gating (maybeParkDecisionGatedBeads,
	// ka-0000005620); without this the resolve/cancel handlers bypass the choke
	// point and a closed decision's `blocks` edge silently frees the gated bead
	// back into the ready lane.
	s.afterBeadClosed(r.Context(), bead)

	// Privileged dibs rebind: when a human answers an agent-created decision,
	// the answering agent bead's operated_by inherits the responder so the
	// agent acts on the responder's behalf from here on (R3 §5.6.2, R2 §5.2
	// step 3, R5 §2 row S5). The rebind helper bails on an empty / sentinel
	// responder, so anonymous / fleet-attributed answers are no-ops.
	s.maybeRebindAgentOnDecisionAnswer(r.Context(), bead, closedBy)

	resp := map[string]any{
		"decision": extractDecisionFields(bead),
		"issue":    bead,
	}
	if rt := reportTypeFromLabels(bead.Labels); rt != "" {
		resp["report_required"] = true
		resp["report_type"] = rt
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleCancelDecision handles POST /v1/decisions/{id}/cancel.
func (s *BeadsServer) handleCancelDecision(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "id is required")
		return
	}

	// CanceledBy is ignored — attribution derives from the authenticated
	// context to prevent audit-log spoofing (AUDIT-2026-04-18 §C3;
	// ka-0000000687).
	var body struct {
		Reason     string `json:"reason"`
		CanceledBy string `json:"canceled_by,omitempty"`
	}
	if !decodeJSONBodyOptional(w, r, &body) {
		return
	}

	// Enforce cross-project write-authz before mutating (ka-0000002666).
	if _, ok := s.authorizeResolutionTarget(w, r, id, "decision", "handleCancelDecision"); !ok {
		return
	}

	canceledBy := s.decisionAttribution(r.Context(), body.CanceledBy)
	if body.CanceledBy != "" && body.CanceledBy != canceledBy {
		slog.Warn("ignoring client-supplied canceled_by on decision cancel",
			"bead", id,
			"client_supplied", body.CanceledBy,
			"derived", canceledBy,
		)
	}

	canceledAt := time.Now().UTC().Format(time.RFC3339)
	extra := map[string]any{
		"canceled":     true,
		"chosen":       "dismissed",
		"responded_at": canceledAt,
	}
	if body.Reason != "" {
		extra["cancel_reason"] = body.Reason
	}
	if canceledBy != "" {
		extra["canceled_by"] = canceledBy
	}
	if err := s.mergeBeadFields(r.Context(), id, extra); err != nil {
		slog.Warn("failed to merge cancel fields", "bead", id, "error", err)
	}

	closedBy := canceledBy
	bead, err := s.store.CloseBead(r.Context(), id, closedBy)
	if errors.Is(err, sql.ErrNoRows) || bead == nil {
		writeError(w, http.StatusNotFound, "decision not found")
		return
	}
	if err != nil {
		writeServerError(r.Context(), w, "handleCancelDecision", err, "failed to cancel decision")
		return
	}

	s.recordAndPublish(r.Context(), events.TopicBeadClosed, bead.ID, closedBy, events.BeadClosed{
		Bead:     bead,
		ClosedBy: closedBy,
		DecisionAnswer: &events.DecisionAnswer{
			Chosen:       "dismissed",
			ResponseText: body.Reason,
			RespondedBy:  canceledBy,
			RespondedAt:  canceledAt,
			Cancelled:    true,
		},
	})

	// A DISMISSED decision parks its gated work exactly as a resolved one does —
	// dismissal is the worst thrash case (the question was never even answered,
	// yet the gate evaporates: ka-0000004466). Run the same close cascade the
	// resolve path runs so the gated bead is parked, not silently freed
	// (ka-0000005620).
	s.afterBeadClosed(r.Context(), bead)

	writeJSON(w, http.StatusOK, map[string]any{
		"decision": extractDecisionFields(bead),
		"issue":    bead,
	})
}

// decisionAttribution resolves the operator string a decision resolve/cancel
// should record in responded_by / canceled_by (and ClosedBy). It NEVER trusts
// the request body — bodyValue is accepted only as a no-auth dev/e2e fallback,
// preserving the AUDIT §C3 anti-spoofing posture.
//
// The resolution ladder:
//
//  1. actorFromContext — the authenticated AuthIdentity.Subject. For an
//     OIDC-authenticated operator this is already the human email, so it
//     wins immediately.
//  2. When the caller is a trusted bridge (relayTrusted: a listed bridge fleet
//     subject), the human it relays in X-Operated-By wins: the Slack bridge
//     answers on its fleet token on behalf of the person who clicked. The
//     relay counts only from a bridge fleet subject (P7 Task 3); the static
//     token's relay is attribution only, so a static-token resolve stamps
//     responded_by="static" (beads-viewer now writes with the user's own
//     bearer, gd-0ORn0Mhr4I, and reaches step 1).
//  3. No-auth dev/e2e mode: actorFromContext is "", so fall back to the body
//     value (ka-0000006099) so the viewer can forward the oauth2-proxy
//     X-Auth-Request-Email through.
func (s *BeadsServer) decisionAttribution(ctx context.Context, bodyValue string) string {
	actor := actorFromContext(ctx)
	if op := s.trustedRelayOperator(ctx); op != "" && isHumanOperator(op) {
		return op
	}
	if actor != "" {
		// A human's own session, or a service actor with no trusted relay:
		// keep the authenticated identity (e.g. "static") rather than taking
		// an unverified header's word for who is behind it.
		return actor
	}
	// No-auth mode: nothing in context, fall back to the body value.
	return bodyValue
}

// extractDecisionFields extracts decision-specific fields from a bead's Fields JSON.
// Returns a map with keys like prompt, options, context, selected_option (mapped from chosen),
// requested_by, responded_by, responded_at, response_text, urgency, iteration, max_iterations.
func extractDecisionFields(b *model.Bead) map[string]any {
	dec := make(map[string]any)
	if len(b.Fields) == 0 {
		return dec
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(b.Fields, &fields); err != nil {
		return dec
	}

	// String fields — extract and assign.
	stringKeys := []string{"prompt", "context", "requested_by", "responded_by", "responded_at", "response_text", "urgency"}
	for _, k := range stringKeys {
		if raw, ok := fields[k]; ok {
			var s string
			if json.Unmarshal(raw, &s) == nil {
				dec[k] = s
			} else {
				dec[k] = strings.TrimSpace(string(raw))
			}
		}
	}

	// "chosen" in storage maps to "selected_option" in the frontend.
	if raw, ok := fields["chosen"]; ok {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			dec["selected_option"] = s
		}
	}

	// "options" — pass through as-is (may be JSON array string or raw array).
	if raw, ok := fields["options"]; ok {
		dec["options"] = json.RawMessage(raw)
	}

	// Numeric fields.
	numKeys := []string{"iteration", "max_iterations"}
	for _, k := range numKeys {
		if raw, ok := fields[k]; ok {
			var n float64
			if json.Unmarshal(raw, &n) == nil {
				dec[k] = int(n)
			}
		}
	}

	// Report requirement derived from bead labels.
	if rt := reportTypeFromLabels(b.Labels); rt != "" {
		dec["report_required"] = true
		dec["report_type"] = rt
	}

	return dec
}
