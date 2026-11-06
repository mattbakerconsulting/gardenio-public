package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"time"
	"unicode"

	"google.golang.org/grpc"

	"gardenio/kbeads/internal/model"
)

// Writes made while impersonating (plan gd-0SUIdKXWV3 step 5, gd-L8LeYDLACc).
//
// One-click Become signs an admin's browser in as a tenant through Keycloak's
// own impersonation, so the session really is the tenant's: created_by,
// author and operated_by stay the tenant. Keycloak records the admin behind it
// in two session notes, which the gardenio-traefik-forwardauth client's
// impersonator / impersonator_id mappers put in every token the session gets
// (deploy-values/gardenio-k3s.yaml). On any other session both are absent.
//
// The daemon reads the "impersonator" claim from the token it verified
// (ImpersonationExtractor, oidcAuthIdentity), never from a header or a request
// field, and:
//
//   - stamps fields.impersonated_by=<admin username> on every bead the session
//     creates, updates or closes, and on every field-endpoint write
//     (stampImpersonatedBy, stampImpersonatedByOn). It is the last impersonated
//     write and stays on the bead, so it does not say who wrote the bead;
//   - stamps fields.created_impersonated_by on the beads the session creates,
//     and on no later write (stampImpersonatedCreate): the authorship mark the
//     Commons shows reads it, so an impersonated close or reaction never
//     relabels a bead its author wrote themselves;
//   - stamps comments.impersonated_by on every comment it adds;
//   - refuses either field from a client on every field-write path
//     (impersonatedByPatchDenial, impersonatedByKeyDenial, and mergeBeadFields
//     for every extra-field merge). The one value a whole-body write may carry
//     is the one the daemon would stamp anyway (the session's own admin, and
//     created_impersonated_by only on a create), or the value already stored,
//     echoed back unchanged by a read-modify-write client;
//   - writes one type=audit bead per impersonated session, on its first read
//     or write, whose resource is the tenant's user bead: who (impersonated_by),
//     whom (subject), when (created_at) and from which client (client_id,
//     source_ip, user_agent) (auditImpersonatedSession).
//
// impersonated_by is a universal field (config.go universalFields), so the
// stamp validates on every typed bead and a later update grandfathers it.

const (
	impersonatedByField = "impersonated_by"
	// createdImpersonatedByField is impersonated_by as the bead's create set
	// it. No later write changes it.
	createdImpersonatedByField = "created_impersonated_by"
	// impersonatorMaxBytes caps the claim. A Keycloak username is at most 255
	// characters.
	impersonatorMaxBytes = 255
	// impersonatorUnreadable stands in for an impersonator claim that is
	// present but not a plain string: the session is still marked as
	// impersonated, rather than read as the tenant's own.
	impersonatorUnreadable = "unknown-impersonator"
	// impersonationAuditLabel marks the per-session audit beads.
	impersonationAuditLabel = "auto:impersonation-audit"
	// impersonationAuditAction is the audit bead's action.
	impersonationAuditAction = "impersonate"
	// impersonationSessionCap bounds impersonationSessionSet. Past it the set
	// starts over; the idempotency key still holds each session to one bead.
	impersonationSessionCap = 4096
)

// parseImpersonatorClaim reads the impersonator claim: the admin's username,
// trimmed. An absent, null or empty claim is no impersonator. A claim that is
// present but not a string, holds a control character or is over-long reads
// as impersonatorUnreadable.
func parseImpersonatorClaim(raw json.RawMessage) string {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return ""
	}
	var name string
	if err := json.Unmarshal(raw, &name); err != nil {
		return impersonatorUnreadable
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	if len(name) > impersonatorMaxBytes || strings.IndexFunc(name, unicode.IsControl) >= 0 {
		return impersonatorUnreadable
	}
	return name
}

// impersonatedByFromContext is the admin behind the caller's session, or ""
// when the caller is not an impersonated OIDC session.
func impersonatedByFromContext(ctx context.Context) string {
	if authMethodFromContext(ctx) != authMethodOIDC {
		return ""
	}
	return AuthIdentityFromContext(ctx).ImpersonatedBy
}

// isServerOwnWrite reports whether ctx is the daemon writing its own record
// (auditRecorderContext), the one writer that names impersonated_by itself.
func isServerOwnWrite(ctx context.Context) bool {
	return AuthIdentityFromContext(ctx).Type == auditRecorderIdentityType
}

// isImpersonationField reports whether k names a server-owned impersonation
// field, in any letter case.
func isImpersonationField(k string) bool {
	return strings.EqualFold(k, impersonatedByField) || strings.EqualFold(k, createdImpersonatedByField)
}

type impersonationStampKey struct{}

// withImpersonationStamp marks ctx as stampImpersonatedByOn's own merge, the
// one extra-field merge mergeBeadFields lets write impersonated_by.
func withImpersonationStamp(ctx context.Context) context.Context {
	return context.WithValue(ctx, impersonationStampKey{}, true)
}

func isImpersonationStamp(ctx context.Context) bool {
	v, _ := ctx.Value(impersonationStampKey{}).(bool)
	return v
}

func impersonatedByDeny(action string, beadType model.BeadType) *AuthzDenial {
	return &AuthzDenial{
		Action:   action,
		BeadType: string(beadType),
		Message:  impersonatedByField + " and " + createdImpersonatedByField + " are stamped by the server from a verified impersonated session's token; they cannot be set directly",
	}
}

// impersonatedByKeyDenial refuses the field endpoints and extra-field merges
// that touch impersonated_by or created_impersonated_by in any letter case:
// they write no whole value to compare, and the daemon stamps the fields
// itself.
func impersonatedByKeyDenial(action string, beadType model.BeadType, keys ...string) *AuthzDenial {
	for _, k := range keys {
		if isImpersonationField(k) {
			return impersonatedByDeny(action, beadType)
		}
	}
	return nil
}

// impersonatedByPatchDenial refuses a create's fields or an update's patch
// that sets impersonated_by or created_impersonated_by (in any letter case) to
// anything but the session's own admin or, on an update, the value already
// stored. An update may not give created_impersonated_by the session's admin:
// only a create says who wrote the bead. existing is nil on a create.
func impersonatedByPatchDenial(ctx context.Context, action string, beadType model.BeadType, patch, existing json.RawMessage) *AuthzDenial {
	if len(patch) == 0 || isServerOwnWrite(ctx) {
		return nil
	}
	var prior map[string]any
	for k, v := range fieldsAsMap(patch) {
		if !isImpersonationField(k) {
			continue
		}
		if k == impersonatedByField || k == createdImpersonatedByField {
			ownStampable := k == impersonatedByField || action == "create"
			if s, ok := v.(string); ok && s != "" && ownStampable && s == impersonatedByFromContext(ctx) {
				continue
			}
			if prior == nil {
				prior = fieldsAsMap(existing)
			}
			if ev, had := prior[k]; had && reflect.DeepEqual(v, ev) {
				continue
			}
		}
		return impersonatedByDeny(action, beadType)
	}
	return nil
}

// stampImpersonatedBy stamps bead.Fields with the session's admin. A no-op
// outside an impersonated session. Runs on the final fields of a create or
// update.
func stampImpersonatedBy(ctx context.Context, bead *model.Bead) {
	imp := impersonatedByFromContext(ctx)
	if imp == "" || bead == nil {
		return
	}
	if v, _ := fieldsAsMap(bead.Fields)[impersonatedByField].(string); v == imp {
		return
	}
	bead.Fields = setField(bead.Fields, impersonatedByField, imp)
}

// stampImpersonatedCreate stamps a bead the session creates with its admin, as
// both the last impersonated write and the bead's author. A no-op outside an
// impersonated session.
func stampImpersonatedCreate(ctx context.Context, bead *model.Bead) {
	stampImpersonatedBy(ctx, bead)
	imp := impersonatedByFromContext(ctx)
	if imp == "" || bead == nil {
		return
	}
	if v, _ := fieldsAsMap(bead.Fields)[createdImpersonatedByField].(string); v == imp {
		return
	}
	bead.Fields = setField(bead.Fields, createdImpersonatedByField, imp)
}

// stampImpersonatedByOn stamps the stored bead id with the session's admin,
// for the write paths that change a bead without a whole-body update (close,
// the field endpoints). A no-op outside an impersonated session.
func (s *BeadsServer) stampImpersonatedByOn(ctx context.Context, id string) error {
	imp := impersonatedByFromContext(ctx)
	if imp == "" {
		return nil
	}
	return s.mergeBeadFields(withImpersonationStamp(ctx), id, map[string]any{impersonatedByField: imp})
}

// impersonationSessionSet is the impersonated sessions this replica has
// written the audit bead for, so the hot path skips the daemon round trip. It
// only saves work: the bead's idempotency key is what holds a session to one
// bead across replicas and restarts.
type impersonationSessionSet struct {
	mu   sync.Mutex
	seen map[string]struct{}
}

// claim reports whether key is new, and marks it seen.
func (s *impersonationSessionSet) claim(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.seen[key]; ok {
		return false
	}
	if s.seen == nil || len(s.seen) >= impersonationSessionCap {
		s.seen = make(map[string]struct{})
	}
	s.seen[key] = struct{}{}
	return true
}

// release forgets key, so a failed audit write is tried again.
func (s *impersonationSessionSet) release(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.seen, key)
}

// impersonationSessionKey is the idempotency key of id's session audit bead:
// the Keycloak session, or, for a token with no sid, the admin, the tenant
// and the day.
func impersonationSessionKey(id AuthIdentity, now time.Time) string {
	if id.SessionID != "" {
		return "impersonation-session:" + id.SessionID
	}
	return "impersonation-session:" + id.ImpersonatedBy + ":" + id.Subject + ":" + now.UTC().Format(time.DateOnly)
}

// auditImpersonatedSession writes the session's audit bead the first time the
// session is seen. Best-effort: a failure is logged and tried again on the
// session's next request, and never fails the request.
func (s *BeadsServer) auditImpersonatedSession(ctx context.Context, sourceIP, userAgent string) {
	if impersonatedByFromContext(ctx) == "" {
		return
	}
	id := AuthIdentityFromContext(ctx)
	key := impersonationSessionKey(id, time.Now())
	if !s.impersonationAudited.claim(key) {
		return
	}
	userBead := UserBeadIDFromContext(ctx)
	if userBead == "" && id.Subject != "" {
		if b, err := userBeadByEmail(ctx, s.store, id.Subject); err == nil && b != nil {
			userBead = b.ID
		}
	}
	fields := map[string]any{
		"actor":             id.ImpersonatedBy,
		"action":            impersonationAuditAction,
		"outcome":           "allow",
		"reason":            id.ImpersonatedBy + " signed in as " + id.Subject,
		"auth_method":       id.Type,
		impersonatedByField: id.ImpersonatedBy,
		"subject":           id.Subject,
	}
	for k, v := range map[string]string{
		"resource": userBead, "session_id": id.SessionID, "client_id": id.ClientID,
		"source_ip": sourceIP, "user_agent": clipUserAgent(userAgent),
	} {
		if v != "" {
			fields[k] = v
		}
	}
	if userBead != "" {
		fields["resource_type"] = "user"
	}
	fieldsJSON, err := json.Marshal(fields)
	if err != nil {
		s.impersonationAudited.release(key)
		return
	}
	_, err = s.createBead(auditRecorderContext(id.ImpersonatedBy), createBeadInput{
		Title:          "impersonation: " + id.ImpersonatedBy + " as " + id.Subject,
		Type:           "audit",
		Labels:         []string{impersonationAuditLabel},
		CreatedBy:      id.ImpersonatedBy,
		Fields:         fieldsJSON,
		IdempotencyKey: key,
	})
	if err != nil {
		s.impersonationAudited.release(key)
		reason := auditWriteFailureReason(err)
		auditWriteFailuresTotal.WithLabelValues(reason).Inc()
		slog.WarnContext(ctx, "audit: failed to record impersonated session",
			"impersonated_by", id.ImpersonatedBy, "subject", id.Subject, "reason", reason, "error", err)
		return
	}
	auditWritesTotal.Inc()
}

// clipUserAgent caps a User-Agent header at 255 bytes, on a rune boundary.
func clipUserAgent(ua string) string {
	const max = 255
	if len(ua) <= max {
		return ua
	}
	return strings.ToValidUTF8(ua[:max], "")
}

// impersonationAuditMiddleware writes an impersonated session's audit bead on
// its first HTTP request, read or write. It runs below auth and user
// provisioning, so the tenant's user bead is on the context.
func (s *BeadsServer) impersonationAuditMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if impersonatedByFromContext(r.Context()) != "" {
			s.auditImpersonatedSession(r.Context(), clientIP(r, s.trustedProxies), r.UserAgent())
		}
		next.ServeHTTP(w, r)
	})
}

// impersonationAuditInterceptor is impersonationAuditMiddleware for gRPC.
func (s *BeadsServer) impersonationAuditInterceptor(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	if impersonatedByFromContext(ctx) != "" {
		s.auditImpersonatedSession(ctx, "", "")
	}
	return handler(ctx, req)
}
