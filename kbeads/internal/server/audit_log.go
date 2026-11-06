package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
)

// auditEntry holds the parameters for an authorization audit trail entry.
type auditEntry struct {
	Actor        string // who performed the action
	Action       string // create, update, close, delete, spawn, approve, reject
	Resource     string // target bead ID or resource identifier
	ResourceType string // bead type of the target
	Outcome      string // "allow" or "deny"
	Reason       string // human-readable reason
	AuthMethod   string // oidc, static, fleet, machine, anonymous
	SourceIP     string // client IP
	EscalationID string // linked escalation bead ID (if any)
}

// auditRecorderIdentityType is the AuthIdentity.Type of the server writing its
// own audit record. It matches none of the credential classes, so no agent,
// fleet, static-token or OIDC step in createBead keys off it.
const auditRecorderIdentityType = "server"

// recordAuditEntry creates an audit bead. Failures are logged and counted
// (gardenio_audit_write_failures_total) but never propagated — audit recording
// is best-effort.
//
// The audit bead itself is a system-generated record describing the caller's
// action, not a write performed on the caller's behalf. Running the caller's
// authz against "create audit" would mean every denial path that tries to log
// itself gets silently dropped (audit write_policy requires admin). The bead
// is therefore written from a clean server context (auditRecorderContext);
// ctx is used only for logging. entry.Actor is recorded as CreatedBy and in
// the actor field, so the caller's identity is still recorded truthfully.
func (s *BeadsServer) recordAuditEntry(ctx context.Context, entry auditEntry) {
	fields := map[string]any{
		"actor":   entry.Actor,
		"action":  entry.Action,
		"outcome": entry.Outcome,
	}
	if entry.Resource != "" {
		fields["resource"] = entry.Resource
	}
	if entry.ResourceType != "" {
		fields["resource_type"] = entry.ResourceType
	}
	if entry.Reason != "" {
		fields["reason"] = entry.Reason
	}
	if entry.AuthMethod != "" {
		fields["auth_method"] = entry.AuthMethod
	}
	if entry.SourceIP != "" {
		fields["source_ip"] = entry.SourceIP
	}
	if entry.EscalationID != "" {
		fields["escalation_id"] = entry.EscalationID
	}

	fieldsJSON, err := json.Marshal(fields)
	if err != nil {
		auditWriteFailuresTotal.WithLabelValues(auditWriteFailMarshal).Inc()
		slog.WarnContext(ctx, "audit: failed to marshal fields", "error", err)
		return
	}

	title := "authz:" + entry.Outcome + ":" + entry.Action
	if entry.ResourceType != "" {
		title += ":" + entry.ResourceType
	}

	_, err = s.createBead(auditRecorderContext(entry.Actor), createBeadInput{
		Title:     title,
		Type:      "audit",
		Labels:    []string{"auto:authz-audit"},
		CreatedBy: entry.Actor,
		Fields:    fieldsJSON,
	})
	if err != nil {
		reason := auditWriteFailureReason(err)
		auditWriteFailuresTotal.WithLabelValues(reason).Inc()
		slog.WarnContext(ctx, "audit: failed to create audit bead",
			"action", entry.Action,
			"actor", entry.Actor,
			"outcome", entry.Outcome,
			"reason", reason,
			"error", err,
		)
		return
	}
	auditWritesTotal.Inc()
}

// auditRecorderContext returns the context the server writes an audit bead
// from. It is built from scratch, not derived from the request ctx
// (gd-mRYje6R9TQ): a derived ctx carried the caller's credential state into
// the write. A project-bound fleet token's TokenProject refused the audit
// bead, which has no project label, so the denial went unrecorded; a
// per-agent token's identity made createBead stamp the agent's project label
// on it, filing the server's record into the tenant project that was just
// denied. Starting empty drops the token project, the type and op scopes, the
// agent identity and every header-derived value (X-Operated-By, owner
// override, auto-inherit) at once, including any added later. It is also
// detached from request cancellation, so a client that hangs up on its 403
// cannot cancel the record of it.
//
// This is a TRUE machine-origin write (the server recording its own authz
// decisions, no human behind it): withMachineWrite makes authorizeWriteTitled
// bypass write policy on type=audit (min_role admin) (gd-ZjiSd9Kzio — a bare
// withAuthMethod(authMethodStatic) switch used to be enough, back when
// authorizeWriteTitled bypassed min_role for every non-OIDC caller; it is not
// anymore, and must not be relied on here). With no caller identity to
// resolve, WithMachineOperator binds the operator FK to the single
// allowlisted system:machine sink (ka-0000006012). The actor is the identity
// Subject, which is what createBead records as CreatedBy.
func auditRecorderContext(actor string) context.Context {
	ctx := WithAuthIdentity(context.Background(), AuthIdentity{Type: auditRecorderIdentityType, Subject: actor})
	ctx = withAuthMethod(ctx, authMethodStatic)
	ctx = WithMachineOperator(ctx)
	return withMachineWrite(ctx)
}

// auditDenial records an audit entry for an authorization denial from an HTTP
// request context. Extracts actor identity and source IP from the request.
func (s *BeadsServer) auditDenial(r *http.Request, d *AuthzDenial, resource string) {
	// Increment the denial counter before the audit bead write and the 403.
	// Reason is derived from the structured denial; bead_type is allowlisted.
	recordAuthzDenial(r.Context(), d)

	id := AuthIdentityFromContext(r.Context())
	s.recordAuditEntry(r.Context(), auditEntry{
		Actor:        id.Subject,
		Action:       d.Action,
		Resource:     resource,
		ResourceType: d.BeadType,
		Outcome:      "deny",
		Reason:       d.Message,
		AuthMethod:   id.Type,
		SourceIP:     clientIP(r, s.trustedProxies),
	})
}

// auditEscalationCreated records an audit entry when an escalation bead is
// created from a denied operation.
func (s *BeadsServer) auditEscalationCreated(r *http.Request, d *AuthzDenial, escalationID, resource string) {
	id := AuthIdentityFromContext(r.Context())
	s.recordAuditEntry(r.Context(), auditEntry{
		Actor:        id.Subject,
		Action:       d.Action,
		Resource:     resource,
		ResourceType: d.BeadType,
		Outcome:      "deny",
		Reason:       d.Message + " (escalation created)",
		AuthMethod:   id.Type,
		SourceIP:     clientIP(r, s.trustedProxies),
		EscalationID: escalationID,
	})
}

// auditEscalationApproval records an audit entry for escalation approval.
func (s *BeadsServer) auditEscalationApproval(ctx context.Context, escalationID, approvedBy string) {
	s.recordAuditEntry(ctx, auditEntry{
		Actor:        approvedBy,
		Action:       "approve",
		Resource:     escalationID,
		ResourceType: "escalation",
		Outcome:      "allow",
		Reason:       "escalation approved",
	})
}

// auditEscalationCoordinatorApproval records a coordinator-lane approval (D4):
// the principal and the lane it approved as.
func (s *BeadsServer) auditEscalationCoordinatorApproval(ctx context.Context, escalationID, approvedBy, as string) {
	s.recordAuditEntry(ctx, auditEntry{
		Actor:        approvedBy,
		Action:       "approve",
		Resource:     escalationID,
		ResourceType: "escalation",
		Outcome:      "allow",
		Reason:       "escalation approved by coordinator lane " + as,
	})
}

// auditEscalationRejection records an audit entry for escalation rejection.
func (s *BeadsServer) auditEscalationRejection(ctx context.Context, escalationID, rejectedBy, reason string) {
	s.recordAuditEntry(ctx, auditEntry{
		Actor:        rejectedBy,
		Action:       "reject",
		Resource:     escalationID,
		ResourceType: "escalation",
		Outcome:      "deny",
		Reason:       reason,
	})
}
