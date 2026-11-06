package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
)

// revokeUserRequest is the JSON body for POST /v1/admin/revoke-user.
type revokeUserRequest struct {
	Email    string `json:"email"`
	Username string `json:"username"`
	Disable  bool   `json:"disable"` // also disable the Keycloak account
	DryRun   bool   `json:"dry_run"` // preview only, don't revoke
	Reason   string `json:"reason"`  // audit trail
}

// revokeUserResponse is returned by POST /v1/admin/revoke-user.
type revokeUserResponse struct {
	UserID          string `json:"user_id"`
	Username        string `json:"username"`
	Email           string `json:"email"`
	SessionsRevoked int    `json:"sessions_revoked"`
	UserDisabled    bool   `json:"user_disabled"`
	DryRun          bool   `json:"dry_run,omitempty"`
}

// handleRevokeUser handles POST /v1/admin/revoke-user.
// Revokes all Keycloak sessions for a user and optionally disables their account.
// Admin-only endpoint.
func (s *BeadsServer) handleRevokeUser(w http.ResponseWriter, r *http.Request) {
	if s.keycloakAdmin == nil {
		writeError(w, http.StatusServiceUnavailable,
			"keycloak admin API not configured (set BEADS_KEYCLOAK_ADMIN_URL, BEADS_KEYCLOAK_ADMIN_REALM, BEADS_KEYCLOAK_ADMIN_CLIENT_ID, BEADS_KEYCLOAK_ADMIN_CLIENT_SECRET)")
		return
	}

	var req revokeUserRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}

	if req.Email == "" && req.Username == "" {
		writeError(w, http.StatusBadRequest, "email or username is required")
		return
	}

	ctx := r.Context()
	actor := AuthIdentityFromContext(ctx).Subject

	if req.DryRun {
		// Preview mode: look up user and count sessions without revoking.
		result, sessions, err := s.previewRevocation(ctx, req)
		if err != nil {
			AttachServerError(r.Context(), "handleRevokeUser(preview)", err)
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}

		slog.Info("revoke-user dry run",
			"actor", actor,
			"user_id", result.ID,
			"email", result.Email,
			"sessions", sessions,
		)

		writeJSON(w, http.StatusOK, &revokeUserResponse{
			UserID:          result.ID,
			Username:        result.Username,
			Email:           result.Email,
			SessionsRevoked: sessions,
			DryRun:          true,
		})
		return
	}

	// Execute revocation.
	result, err := s.keycloakAdmin.RevokeUser(ctx, req.Email, req.Username, req.Disable)
	if err != nil {
		if err.Error() == "user not found" {
			writeError(w, http.StatusNotFound, "user not found in Keycloak")
			return
		}
		AttachServerError(ctx, "handleRevokeUser", err)
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("revocation failed: %v", err))
		return
	}

	slog.Warn("user sessions revoked",
		"actor", actor,
		"user_id", result.UserID,
		"email", result.Email,
		"username", result.Username,
		"sessions_revoked", result.SessionsRevoked,
		"user_disabled", result.UserDisabled,
		"reason", req.Reason,
	)

	// Record an audit event.
	s.recordAndPublish(ctx, events.TopicBeadUpdated, "", actor, map[string]any{
		"action":           "user_sessions_revoked",
		"user_id":          result.UserID,
		"email":            result.Email,
		"username":         result.Username,
		"sessions_revoked": result.SessionsRevoked,
		"user_disabled":    result.UserDisabled,
		"reason":           req.Reason,
		"timestamp":        time.Now().UTC().Format(time.RFC3339),
	})

	// Also record a dedicated audit log entry.
	auditPayload, _ := json.Marshal(map[string]any{
		"action":           "revoke_user",
		"email":            result.Email,
		"username":         result.Username,
		"user_id":          result.UserID,
		"sessions_revoked": result.SessionsRevoked,
		"user_disabled":    result.UserDisabled,
		"reason":           req.Reason,
	})
	// Best-effort observability: skip RecordEvent if the operator cannot be
	// resolved so migration-026 NOT VALID constraint on events.operated_by_id
	// never sees a NULL insert. The keycloak revocation already succeeded.
	if operatedBy, operatedByID, _, ok := s.resolveAuditOperatorRequired(ctx); ok {
		_ = s.store.RecordEvent(ctx, &model.Event{
			Topic:        "admin.revoke_user",
			Actor:        actor,
			Payload:      auditPayload,
			OperatedBy:   operatedBy,
			OperatedByID: &operatedByID,
		})
	} else {
		slog.Warn("skipping admin.revoke_user RecordEvent: operator unresolvable",
			"user_id", result.UserID)
	}

	writeJSON(w, http.StatusOK, &revokeUserResponse{
		UserID:          result.UserID,
		Username:        result.Username,
		Email:           result.Email,
		SessionsRevoked: result.SessionsRevoked,
		UserDisabled:    result.UserDisabled,
	})
}

// previewRevocation finds the user and counts sessions without revoking.
func (s *BeadsServer) previewRevocation(ctx context.Context, req revokeUserRequest) (*keycloakUser, int, error) {
	var user *keycloakUser
	var err error

	if req.Email != "" {
		user, err = s.keycloakAdmin.FindUserByEmail(ctx, req.Email)
	} else {
		user, err = s.keycloakAdmin.FindUserByUsername(ctx, req.Username)
	}
	if err != nil {
		return nil, 0, fmt.Errorf("find user: %w", err)
	}
	if user == nil {
		return nil, 0, fmt.Errorf("user not found in Keycloak")
	}

	sessions, err := s.keycloakAdmin.ListUserSessions(ctx, user.ID)
	if err != nil {
		return nil, 0, fmt.Errorf("list sessions: %w", err)
	}

	return user, len(sessions), nil
}
