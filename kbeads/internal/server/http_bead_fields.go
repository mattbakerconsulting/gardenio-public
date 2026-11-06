package server

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strings"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
	"gardenio/kbeads/internal/store"
)

// The per-field write endpoints (increment, array append/reorder, sub-field
// edit) and the shared gate they all call first, tokenAuthorizedForBead.

// fieldEndpointDenial is the write_policy-side gates a single-field
// endpoint must run itself: the tenant-agent type gate and the restrict taint
// (typeWriteDenial) and the land freeze (land_frozen.go).
func (s *BeadsServer) fieldEndpointDenial(ctx context.Context, existing *model.Bead) *AuthzDenial {
	if denial := s.typeWriteDenial(ctx, "update", existing.Type); denial != nil {
		return denial
	}
	return frozenLandDenial(ctx, s.store, "update", existing.Type, existing.Labels)
}

// fieldEndpointKeyError refuses keys a single-field endpoint may not write
// because a fields PATCH keeps them in step with something else: a role's
// tier pair (roleTierFieldEndpointError) and fields.project
// (projectFieldEndpointError).
func (s *BeadsServer) fieldEndpointKeyError(ctx context.Context, existing *model.Bead, keys ...string) error {
	if err := roleTierFieldEndpointError(existing, keys...); err != nil {
		return err
	}
	return s.projectFieldEndpointError(ctx, existing, keys...)
}

// tokenAuthorizedForBead fetches the bead by id and verifies the caller's
// project scope includes the bead's project, mirroring handleEditBeadSubField.
// It runs BOTH cross-project gates the canonical PATCH write path
// (handleUpdateBead -> updateBead) enforces: the project-bound fleet-token gate
// (beadMatchesTokenProject) and the OIDC-user project gate (oidcAllowedProjects
// / beadMatchesOIDCProjects). It writes the appropriate error response (404 on
// missing bead / fetch error, 403 on a project-scope mismatch) and returns
// false when the caller must stop; a true return means the caller may proceed
// with the mutation. It exists so the field-mutation endpoints (increment /
// append / reorder) enforce the same cross-project write-authz gate as the
// .../edit sibling without duplicating the fetch-and-check block. handler names
// the calling handler for error logs.
//
// It is also the single gate for identity fields on type:user beads: fieldKeys
// names the field(s) the caller is about to write, and
// authorizeUserBeadFieldWrite refuses a non-OIDC caller touching one of
// userIdentityFields (403 AuthzDenial). Every handler that writes a field
// through a store primitive MUST call this with its keys; the
// TestFieldMutationEndpoints_UserIdentityGate table and
// TestStoreFieldWriters_AreGated source scan enforce that.
func (s *BeadsServer) tokenAuthorizedForBead(w http.ResponseWriter, r *http.Request, id, handler string, fieldKeys ...string) bool {
	existing, err := s.store.GetBead(r.Context(), id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "bead not found")
			return false
		}
		writeServerError(r.Context(), w, handler, err, "failed to fetch bead")
		return false
	}
	if existing == nil {
		writeError(w, http.StatusNotFound, "bead not found")
		return false
	}
	// The token and OIDC gates below refuse every bead the caller cannot
	// read; such a bead answers as missing (beadHiddenFromCaller).
	if s.beadHiddenFromCaller(r.Context(), existing) {
		writeError(w, http.StatusNotFound, "bead not found")
		return false
	}
	if !beadMatchesTokenProject(r.Context(), existing.Labels) {
		writeError(w, http.StatusForbidden, "token not authorized for this bead's project")
		return false
	}
	if allowed := oidcAllowedProjects(r.Context(), s.store); allowed != nil {
		if !beadMatchesOIDCProjects(existing.Labels, allowed) {
			writeError(w, http.StatusForbidden, "not authorized to write beads in this project")
			return false
		}
	}
	// A scoped principal is confined to its allowlisted bead types (and its
	// projects) on every write route, not just PATCH; same shape as
	// handleCloseBead. Authz runs before field validation.
	if denial := scopedWriteDenial(r.Context(), "update", existing.Type, existing.Labels); denial != nil {
		s.auditDenial(r, denial, id)
		writeAuthzDenial(w, denial)
		return false
	}
	// These routes never reach write_policy, so the tenant-agent type gate,
	// the restrict taint and the land freeze are checked here
	// (fieldEndpointDenial).
	if denial := s.fieldEndpointDenial(r.Context(), existing); denial != nil {
		s.auditDenial(r, denial, id)
		writeAuthzDenial(w, denial)
		return false
	}
	if err := checkEscalationFieldEndpointWrite(r.Context(), existing, fieldKeys...); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return false
	}
	// Spec D8 (P7 Task 11): no escalation decision or filing field, for any
	// caller (escalation_field_guard.go).
	if err := checkEscalationFieldWrite(r.Context(), existing, fieldKeys...); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return false
	}
	if err := checkEscalationFrozenFieldEndpoint(r.Context(), existing, fieldKeys...); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return false
	}
	if err := checkLandStewardApprovesFieldEndpoint(r.Context(), existing, fieldKeys...); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return false
	}
	if s.ledgerFieldEndpointDenied(w, r, "update", existing) {
		return false
	}
	if denial := s.authorizeUserBeadFieldWrite(r.Context(), "update", existing, fieldKeys...); denial != nil {
		s.auditDenial(r, denial, id)
		writeAuthzDenial(w, denial)
		return false
	}
	if denial := s.authorizeAgentFieldEndpoint(r.Context(), existing, fieldKeys...); denial != nil {
		s.auditDenial(r, denial, id)
		writeAuthzDenial(w, denial)
		return false
	}
	if err := s.fieldEndpointKeyError(r.Context(), existing, fieldKeys...); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return false
	}
	if denial := s.authorizePermissionFieldWrite(r.Context(), existing); denial != nil {
		s.auditDenial(r, denial, id)
		writeAuthzDenial(w, denial)
		return false
	}
	if denial := denyDecisionStampedKeyWrite("update", existing, fieldKeys); denial != nil {
		s.auditDenial(r, denial, id)
		writeAuthzDenial(w, denial)
		return false
	}
	if denial := commonsMessageFieldKeysDenial(r.Context(), "update", existing, fieldKeys...); denial != nil {
		s.auditDenial(r, denial, id)
		writeAuthzDenial(w, denial)
		return false
	}
	if denial := messageAuthorFieldKeysDenial(r.Context(), "update", existing, fieldKeys...); denial != nil {
		s.auditDenial(r, denial, id)
		writeAuthzDenial(w, denial)
		return false
	}
	if denial := messageGitHubAuthorKeyDenial("update", existing, fieldKeys...); denial != nil {
		s.auditDenial(r, denial, id)
		writeAuthzDenial(w, denial)
		return false
	}
	if denial := s.authorizeDecisionAnswerFields(r.Context(), "update", existing, fieldKeys...); denial != nil {
		s.auditDenial(r, denial, id)
		writeAuthzDenial(w, denial)
		return false
	}
	// Field validation after authz: refuse a key that collides under case
	// folding (field_key_case.go).
	if err := s.fieldKeyCaseError(r.Context(), existing, fieldKeyWrites(fieldKeys)); err != nil {
		writeError(w, http.StatusBadRequest, "invalid fields: "+err.Error())
		return false
	}
	// The caller is about to write an answer through a field endpoint: stamp
	// who first. A write that then fails leaves a stamp that refuses the old
	// answer, which fails closed.
	if writesDecisionAnswer(existing, fieldKeys) {
		if err := s.mergeBeadFields(r.Context(), id, s.decisionResolverStamp(r.Context())); err != nil {
			writeServerError(r.Context(), w, handler, err, "failed to stamp the decision's resolver")
			return false
		}
	}
	// A field write by an impersonated session names the admin behind it
	// (impersonation.go), stamped first like the resolver above.
	if err := s.stampImpersonatedByOn(r.Context(), id); err != nil {
		writeServerError(r.Context(), w, handler, err, "failed to stamp impersonated_by")
		return false
	}
	return true
}

// handleIncrementBeadFields handles POST /v1/beads/{id}/increment.
// Atomically adds numeric deltas to the bead's JSONB fields. The request body
// is a map of field names to numeric deltas. This avoids the read-modify-write
// race condition that occurs when using GET + PATCH for counter-style fields.
func (s *BeadsServer) handleIncrementBeadFields(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "id is required")
		return
	}

	var deltas map[string]float64
	if !decodeJSONBody(w, r, &deltas) {
		return
	}
	if len(deltas) == 0 {
		writeError(w, http.StatusBadRequest, "at least one field delta is required")
		return
	}
	for key := range deltas {
		if !store.ValidFieldKey(key) {
			writeError(w, http.StatusBadRequest, "invalid field name: must match [A-Za-z_][A-Za-z0-9_]*")
			return
		}
	}

	// Token-project scope: reject before mutating, mirroring the PATCH path and
	// the .../edit sibling. The store UPDATE is keyed on id alone, so without
	// this a project-scoped token could increment another project's bead.
	if !s.tokenAuthorizedForBead(w, r, id, "handleIncrementBeadFields", mapKeys(deltas)...) {
		return
	}

	bead, err := s.store.IncrementBeadFields(r.Context(), id, deltas)
	if err != nil {
		if errors.Is(err, store.ErrInvalidFieldKey) {
			writeError(w, http.StatusBadRequest, "invalid field name: must match [A-Za-z_][A-Za-z0-9_]*")
			return
		}
		writeServerError(r.Context(), w, "handleIncrementBeadFields", err, "internal server error")
		return
	}
	if bead == nil {
		writeError(w, http.StatusNotFound, "bead not found")
		return
	}

	s.recordAndPublish(r.Context(), events.TopicBeadUpdated, bead.ID, "", events.BeadUpdated{
		Bead: bead,
	})
	for k, v := range deltas {
		s.recordMutation(r.Context(), bead.ID, actorFromContext(r.Context()), "fields."+k, "", fmtValue(v), model.MutationTypeUpdate)
	}

	writeJSON(w, http.StatusOK, bead)
}

// appendArrayFieldRequest is the body of POST /v1/beads/{id}/fields/{field}/append.
type appendArrayFieldRequest struct {
	Element string `json:"element"`
}

// handleAppendBeadArrayField handles POST /v1/beads/{id}/fields/{field}/append.
// Atomically appends `element` to the string-array JSONB field `field`
// (phase_order / step_order), skipping the write if it is already present
// (append-if-absent, idempotent). This is the GROW primitive that lets the
// browser and the CLI both add a phase/step without a read-modify-write window
// where one writer clobbers the other (ka-0000000988). The reordered array is
// written by the CAS sibling below.
func (s *BeadsServer) handleAppendBeadArrayField(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	field := r.PathValue("field")
	if id == "" || field == "" {
		writeError(w, http.StatusBadRequest, "id and field are required")
		return
	}
	if !store.ValidFieldKey(field) {
		writeError(w, http.StatusBadRequest, "invalid field name: must match [A-Za-z_][A-Za-z0-9_]*")
		return
	}

	var req appendArrayFieldRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.Element == "" {
		writeError(w, http.StatusBadRequest, "element is required")
		return
	}

	// Token-project scope: reject before mutating, mirroring the PATCH path and
	// the .../edit sibling. The store UPDATE is keyed on id alone, so without
	// this a project-scoped token could grow another project's array field.
	if !s.tokenAuthorizedForBead(w, r, id, "handleAppendBeadArrayField", field) {
		return
	}

	bead, changed, err := s.store.AppendBeadArrayField(r.Context(), id, field, req.Element)
	if err != nil {
		if errors.Is(err, store.ErrInvalidFieldKey) {
			writeError(w, http.StatusBadRequest, "invalid field name: must match [A-Za-z_][A-Za-z0-9_]*")
			return
		}
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "bead not found")
			return
		}
		writeServerError(r.Context(), w, "handleAppendBeadArrayField", err, "internal server error")
		return
	}
	if bead == nil {
		writeError(w, http.StatusNotFound, "bead not found")
		return
	}

	// Only publish/record when the append actually changed the array — an
	// already-present element is a genuine no-op (no updated_at bump) and must
	// not pollute the activity feed.
	if changed {
		s.recordAndPublish(r.Context(), events.TopicBeadUpdated, bead.ID, "", events.BeadUpdated{
			Bead: bead,
		})
		s.recordMutation(r.Context(), bead.ID, actorFromContext(r.Context()), "fields."+field, "", req.Element, model.MutationTypeUpdate)
	}

	writeJSON(w, http.StatusOK, bead)
}

// casArrayFieldRequest is the body of POST /v1/beads/{id}/fields/{field}/reorder.
// `expected` is the array the caller read before permuting; `next` is the array
// it wants to write. The write lands only if the field still equals `expected`.
type casArrayFieldRequest struct {
	Expected []string `json:"expected"`
	Next     []string `json:"next"`
}

// handleReorderBeadArrayField handles POST /v1/beads/{id}/fields/{field}/reorder.
// It compare-and-swaps the string-array JSONB field `field` from `expected` to
// `next`: the write lands ONLY IF the field still equals `expected`. This is the
// REORDER primitive — a reorder is a blind full-array rewrite that under naive
// read-modify-write silently drops a concurrent append, so the CAS guard rejects
// a stale rewrite with 409 Conflict and the caller re-reads + retries against the
// fresh array (ka-0000000988).
func (s *BeadsServer) handleReorderBeadArrayField(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	field := r.PathValue("field")
	if id == "" || field == "" {
		writeError(w, http.StatusBadRequest, "id and field are required")
		return
	}
	if !store.ValidFieldKey(field) {
		writeError(w, http.StatusBadRequest, "invalid field name: must match [A-Za-z_][A-Za-z0-9_]*")
		return
	}

	var req casArrayFieldRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}

	// Token-project scope: reject before mutating, mirroring the PATCH path and
	// the .../edit sibling. The store CAS is keyed on id alone, so without this
	// a project-scoped token could rewrite another project's array field.
	if !s.tokenAuthorizedForBead(w, r, id, "handleReorderBeadArrayField", field) {
		return
	}

	bead, err := s.store.CASBeadArrayField(r.Context(), id, field, req.Expected, req.Next)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrInvalidFieldKey):
			writeError(w, http.StatusBadRequest, "invalid field name: must match [A-Za-z_][A-Za-z0-9_]*")
		case errors.Is(err, store.ErrCASConflict):
			writeError(w, http.StatusConflict, "compare-and-swap conflict: bead was modified concurrently; re-read and retry")
		case errors.Is(err, sql.ErrNoRows):
			writeError(w, http.StatusNotFound, "bead not found")
		default:
			writeServerError(r.Context(), w, "handleReorderBeadArrayField", err, "internal server error")
		}
		return
	}
	if bead == nil {
		writeError(w, http.StatusNotFound, "bead not found")
		return
	}

	s.recordAndPublish(r.Context(), events.TopicBeadUpdated, bead.ID, "", events.BeadUpdated{
		Bead: bead,
	})
	s.recordMutation(r.Context(), bead.ID, actorFromContext(r.Context()), "fields."+field, strings.Join(req.Expected, ","), strings.Join(req.Next, ","), model.MutationTypeUpdate)

	writeJSON(w, http.StatusOK, bead)
}

// editSubFieldRequest is the body of POST /v1/beads/{id}/fields/{field}/edit.
// `op` selects the aggregate shape and the mutation: "set"/"unset-key" edit ONE
// key of a JSON-map field (channel_roles), "append"/"remove" edit ONE element of
// a CSV field (slack_channel). `key`+`value` apply to the map ops; `element`
// applies to the CSV ops.
type editSubFieldRequest struct {
	Op      string `json:"op"`
	Key     string `json:"key"`
	Value   string `json:"value"`
	Element string `json:"element"`
}

// handleEditBeadSubField handles POST /v1/beads/{id}/fields/{field}/edit.
// It edits ONE key of a JSON-map field or ONE element of a CSV field WITHOUT
// clobbering the field's other keys/elements or the bead's other fields — the
// non-clobbering counterpart to a bare PATCH `-f field=…`, which replaces the
// whole aggregate and silently drops entries other agents/operators set
// (ka-0000007767). The store primitives run a read-modify-write in one
// transaction under SELECT … FOR UPDATE, so concurrent writers to different
// keys/elements never lose an update. It is operator-gated (like PATCH) and
// additionally rejects a token not scoped to the bead's project.
func (s *BeadsServer) handleEditBeadSubField(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	field := r.PathValue("field")
	if id == "" || field == "" {
		writeError(w, http.StatusBadRequest, "id and field are required")
		return
	}
	if !store.ValidFieldKey(field) {
		writeError(w, http.StatusBadRequest, "invalid field name: must match [A-Za-z_][A-Za-z0-9_]*")
		return
	}

	var req editSubFieldRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}

	// Token-project scope: reject before mutating, mirroring the PATCH path.
	if !s.tokenAuthorizedForBead(w, r, id, "handleEditBeadSubField", field) {
		return
	}

	var (
		bead    *model.Bead
		changed bool
		err     error
		mutVal  string // describes the sub-field edit for the mutation-history row
	)
	switch req.Op {
	case "set":
		mutVal = "set " + req.Key + "=" + req.Value
		bead, changed, err = s.store.MergeBeadMapField(r.Context(), id, field, req.Op, req.Key, req.Value)
	case "unset-key":
		mutVal = "unset-key " + req.Key
		bead, changed, err = s.store.MergeBeadMapField(r.Context(), id, field, req.Op, req.Key, req.Value)
	case "append", "remove":
		mutVal = req.Op + " " + req.Element
		bead, changed, err = s.store.EditBeadCSVField(r.Context(), id, field, req.Op, req.Element)
	default:
		writeError(w, http.StatusBadRequest, `op must be one of "set", "unset-key" (JSON-map field) or "append", "remove" (CSV field)`)
		return
	}
	if err != nil {
		switch {
		case errors.Is(err, store.ErrInvalidFieldKey):
			writeError(w, http.StatusBadRequest, "invalid field name: must match [A-Za-z_][A-Za-z0-9_]*")
		case errors.Is(err, store.ErrInvalidSubFieldEdit):
			writeError(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, sql.ErrNoRows):
			writeError(w, http.StatusNotFound, "bead not found")
		default:
			writeServerError(r.Context(), w, "handleEditBeadSubField", err, "internal server error")
		}
		return
	}
	if bead == nil {
		writeError(w, http.StatusNotFound, "bead not found")
		return
	}

	// Only publish/record on a real change — an idempotent no-op (set to the
	// same value, unset of an absent key, append of a present element, remove of
	// an absent one) must not bump updated_at or pollute the activity feed.
	if changed {
		s.recordAndPublish(r.Context(), events.TopicBeadUpdated, bead.ID, "", events.BeadUpdated{
			Bead: bead,
		})
		s.recordMutation(r.Context(), bead.ID, actorFromContext(r.Context()), "fields."+field, "", mutVal, model.MutationTypeUpdate)
	}

	writeJSON(w, http.StatusOK, bead)
}
