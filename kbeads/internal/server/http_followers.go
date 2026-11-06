package server

import (
	"encoding/json"
	"net/http"
	"strconv"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
)

// addFollowerRequest is the JSON body for POST /v1/beads/{id}/followers.
//
// OperatorID is the operators(id) FK of the operator who should start
// following this bead. Note is an optional human-readable string the
// forwarder can attach (e.g. "fyi — you owned this last quarter").
//
// forwarded_by is intentionally NOT accepted from the body: it is always
// derived from the request's auth identity (resolveAuditOperator) so a
// caller cannot impersonate another operator as the forwarder. This
// mirrors the comment handler's "ignore req.Author" rule (ka-0000001391).
type addFollowerRequest struct {
	OperatorID int64  `json:"operator_id"`
	Note       string `json:"note"`
}

// addFollowerResponse mirrors the BeadFollower row written, less the
// joined display fields (which are populated only by ListFollowers).
type addFollowerResponse struct {
	BeadID      string `json:"bead_id"`
	OperatorID  int64  `json:"operator_id"`
	ForwardedBy *int64 `json:"forwarded_by,omitempty"`
	Note        string `json:"note,omitempty"`
}

// handleAddFollower handles POST /v1/beads/{id}/followers. Idempotent —
// re-forwarding the same (bead, operator) pair refreshes forwarded_by + note.
func (s *BeadsServer) handleAddFollower(w http.ResponseWriter, r *http.Request) {
	beadID := r.PathValue("id")
	if beadID == "" {
		writeError(w, http.StatusBadRequest, "id is required")
		return
	}

	bodyBytes, ok := readBoundedBody(w, r)
	if !ok {
		return
	}
	var req addFollowerRequest
	if err := json.Unmarshal(bodyBytes, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.OperatorID <= 0 {
		writeError(w, http.StatusBadRequest, "operator_id is required")
		return
	}

	if s.loadBeadForWrite(w, r, beadID, "update", bodyBytes) == nil {
		return
	}

	// forwarded_by is auth-derived. resolveAuditOperator returns nil when
	// the request is anonymous or the resolver has nothing to attribute —
	// a NULL forwarded_by is legitimate (matches the system-insert path).
	_, forwardedBy, _ := s.resolveAuditOperator(r.Context())

	if err := s.store.AddFollower(r.Context(), beadID, req.OperatorID, forwardedBy, req.Note); err != nil {
		writeServerError(r.Context(), w, "handleAddFollower", err, "failed to add follower")
		return
	}

	s.recordAndPublish(r.Context(), events.TopicFollowerAdded, beadID, actorFromContext(r.Context()), events.FollowerAdded{
		BeadID:      beadID,
		OperatorID:  req.OperatorID,
		ForwardedBy: forwardedBy,
		Note:        req.Note,
		Fleet:       FleetFromContext(r.Context()),
	})

	writeJSON(w, http.StatusCreated, addFollowerResponse{
		BeadID:      beadID,
		OperatorID:  req.OperatorID,
		ForwardedBy: forwardedBy,
		Note:        req.Note,
	})
}

// handleRemoveFollower handles DELETE /v1/beads/{id}/followers/{operator_id}.
// Idempotent — a miss returns 204 just like a hit so callers can blindly unfollow.
func (s *BeadsServer) handleRemoveFollower(w http.ResponseWriter, r *http.Request) {
	beadID := r.PathValue("id")
	if beadID == "" {
		writeError(w, http.StatusBadRequest, "id is required")
		return
	}
	operatorIDStr := r.PathValue("operator_id")
	if operatorIDStr == "" {
		writeError(w, http.StatusBadRequest, "operator_id is required")
		return
	}
	operatorID, err := strconv.ParseInt(operatorIDStr, 10, 64)
	if err != nil || operatorID <= 0 {
		writeError(w, http.StatusBadRequest, "operator_id must be a positive integer")
		return
	}

	if s.loadBeadForWrite(w, r, beadID, "update", nil) == nil {
		return
	}

	removed, err := s.store.RemoveFollower(r.Context(), beadID, operatorID)
	if err != nil {
		writeServerError(r.Context(), w, "handleRemoveFollower", err, "failed to remove follower")
		return
	}

	if removed {
		s.recordAndPublish(r.Context(), events.TopicFollowerRemoved, beadID, actorFromContext(r.Context()), events.FollowerRemoved{
			BeadID:     beadID,
			OperatorID: operatorID,
			Fleet:      FleetFromContext(r.Context()),
		})
	}

	w.WriteHeader(http.StatusNoContent)
}

// handleListFollowers handles GET /v1/beads/{id}/followers. Returns rows
// joined with operators so callers get canonical (kind, subject, display)
// for both the follower and the forwarder without a second round trip.
func (s *BeadsServer) handleListFollowers(w http.ResponseWriter, r *http.Request) {
	beadID := r.PathValue("id")
	if beadID == "" {
		writeError(w, http.StatusBadRequest, "id is required")
		return
	}
	if !s.authorizeSubresourceRead(w, r, beadID, map[string]any{"followers": []*model.BeadFollower{}}) {
		return
	}

	followers, err := s.store.ListFollowers(r.Context(), beadID)
	if err != nil {
		writeServerError(r.Context(), w, "handleListFollowers", err, "failed to list followers")
		return
	}
	if followers == nil {
		followers = []*model.BeadFollower{}
	}

	writeJSON(w, http.StatusOK, map[string]any{"followers": followers})
}

// listFollowedBeadIDsMaxLimit caps the per-request fetch size for
// /v1/operators/{id}/followed-beads. The inbox handler passes a cap matching
// inboxListLimit (500); this is the daemon-side ceiling that protects the
// handler from a pathological caller.
const listFollowedBeadIDsMaxLimit = 1000

// handleListFollowedBeadIDs handles GET /v1/operators/{id}/followed-beads.
// Returns the bead IDs the operator follows, ordered by follow created_at DESC.
// Drives the inbox-query UNION (ka-0000007204): callers feed the IDs into the
// existing list path to hydrate the right side of the OR predicate
// (operated_by_id = $me) ∪ (operator follows bead).
//
// limit is bounded by listFollowedBeadIDsMaxLimit; values ≤ 0 fall back to
// no caller-side cap (the daemon ceiling still applies).
func (s *BeadsServer) handleListFollowedBeadIDs(w http.ResponseWriter, r *http.Request) {
	operatorIDStr := r.PathValue("id")
	if operatorIDStr == "" {
		writeError(w, http.StatusBadRequest, "id is required")
		return
	}
	operatorID, err := strconv.ParseInt(operatorIDStr, 10, 64)
	if err != nil || operatorID <= 0 {
		writeError(w, http.StatusBadRequest, "id must be a positive integer")
		return
	}

	limit := listFollowedBeadIDsMaxLimit
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, "limit must be a non-negative integer")
			return
		}
		if n == 0 || n > listFollowedBeadIDsMaxLimit {
			n = listFollowedBeadIDsMaxLimit
		}
		limit = n
	}

	ids, err := s.store.ListFollowedBeadIDs(r.Context(), operatorID, limit)
	if err != nil {
		writeServerError(r.Context(), w, "handleListFollowedBeadIDs", err, "failed to list followed bead ids")
		return
	}
	if ids == nil {
		ids = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"bead_ids": ids})
}
