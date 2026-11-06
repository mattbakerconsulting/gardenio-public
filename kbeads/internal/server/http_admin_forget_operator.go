package server

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"gardenio/kbeads/internal/store"
)

// OperatorForgetGuardErrorCode is the stable `code` on the 409 body returned
// when ForgetOperator refuses because the operator owns more audit history than
// the guard threshold and `confirm` was not set. A client can branch on this
// code (and read audit_rows / threshold) instead of matching the message
// string.
const OperatorForgetGuardErrorCode = "operator_forget_guard"

// ForgetOperatorRequest is the JSON body for
// POST /v1/admin/operators/{id}/forget. All fields are optional: an empty body
// forgets with no reason, no confirm, and the default audit-history guard.
type ForgetOperatorRequest struct {
	// Reason is an optional free-text justification recorded as
	// operators.forgotten_reason (e.g. an erasure-request or ticket ref).
	Reason string `json:"reason,omitempty"`
	// Confirm bypasses the large-audit-history guard.
	Confirm bool `json:"confirm,omitempty"`
	// Threshold overrides the audit-history guard row count; <=0 uses the
	// store default.
	Threshold int `json:"threshold,omitempty"`
}

// ForgetOperatorResponse mirrors store.ForgetOperatorResult over the wire.
type ForgetOperatorResponse struct {
	OperatorID         int64     `json:"operator_id"`
	AlreadyForgotten   bool      `json:"already_forgotten"`
	ForgottenAt        time.Time `json:"forgotten_at"`
	SlugCleared        bool      `json:"slug_cleared"`
	SlugAliasesRetired int64     `json:"slug_aliases_retired"`
	AuditRows          int64     `json:"audit_rows"`
}

// handleForgetOperator handles POST /v1/admin/operators/{id}/forget. Admin-only.
// It soft-deletes the operator (forgotten_at + null slug) as the API primitive
// behind `kd admin forget-operator` (ka-0000005150), recording the acting
// principal (server-derived, never client-supplied) and reason. Returns 404 for
// an unknown operator and 409 (with OperatorForgetGuardErrorCode) when the
// operator owns a large audit history and `confirm` was not set.
func (s *BeadsServer) handleForgetOperator(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "invalid operator id")
		return
	}

	var req ForgetOperatorRequest
	if !decodeJSONBodyOptional(w, r, &req) {
		return
	}

	result, err := s.store.ForgetOperator(r.Context(), id, store.ForgetOperatorOptions{
		// Actor is resolved from the authenticated identity — the audit
		// attribution must not be spoofable by a client-supplied field (§C3).
		Actor:               actorFromContext(r.Context()),
		Reason:              req.Reason,
		Confirm:             req.Confirm,
		AuditGuardThreshold: req.Threshold,
	})
	if err != nil {
		var guard *store.OperatorForgetGuardError
		if errors.As(err, &guard) {
			writeJSON(w, http.StatusConflict, map[string]any{
				"error":      guard.Error(),
				"code":       OperatorForgetGuardErrorCode,
				"audit_rows": guard.AuditRows,
				"threshold":  guard.Threshold,
			})
			return
		}
		var notFound *store.OperatorNotFoundError
		if errors.As(err, &notFound) {
			writeError(w, http.StatusNotFound, notFound.Error())
			return
		}
		writeServerError(r.Context(), w, "handleForgetOperator", err, "failed to forget operator")
		return
	}

	writeJSON(w, http.StatusOK, &ForgetOperatorResponse{
		OperatorID:         result.ID,
		AlreadyForgotten:   result.AlreadyForgotten,
		ForgottenAt:        result.ForgottenAt,
		SlugCleared:        result.SlugCleared,
		SlugAliasesRetired: result.SlugAliasesRetired,
		AuditRows:          result.AuditRows,
	})
}
