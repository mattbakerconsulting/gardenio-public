package server

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"

	"gardenio/kbeads/internal/model"
)

// Auto-inherit defaults Fields[operated_by], Fields[user_bead_id], and
// Fields[slack_user_id] on a bead at create-time from sanitized request-
// context values (and the final operated_by) when the caller omits them.
// This eliminates the per-site Fields[] pushes in gardenio spawn handlers
// (triggers.go, bot_commands.go, scheduler.go) — see research ka-0000007523
// (Break #2, #3, #4, #5) and tasks ka-0000006800 + ka-0000005990.
//
// Sources:
//
//   - Fields[operated_by] defaults from operatedByHeaderFromContext, the
//     sanitized X-Operated-By header value stamped by the static-token auth
//     interceptor. OIDC and anonymous callers are unaffected — the existing
//     resolver still populates the beads.operated_by column for them via
//     resolveAuditOperatorRequired, but the JSON sidecar is not auto-filled
//     because the OIDC subject is not carried in a stable header value.
//
//   - Fields[user_bead_id] defaults from UserBeadIDFromContext. The
//     user-provisioning middleware populates it for OIDC requests; the
//     static-token interceptor populates it from the new X-User-Bead-Id
//     request header so agent pods can propagate the upstream user bead.
//
//   - Fields[slack_user_id] is derived from the final Fields[operated_by]
//     value (whether caller-supplied or just inherited) when it matches
//     "slack:U<alnum>". This fixes the trigger and gardener→crew breaks
//     (#4) and the Slack /spawn task bead (#5) without per-site code, and
//     keeps bot-DM notification targeting working for those spawns.
//
// The X-No-Auto-Inherit: 1 request header (read by the HTTP handler and the
// gRPC interceptor) sets noAutoInheritKey in ctx; applyAutoInherit then
// returns the caller's Fields untouched. Use this when a caller deliberately
// wants the legacy "Fields empty unless explicitly set" behavior — e.g.
// a backfill job that must not invent attribution.

type noAutoInheritKey struct{}

// WithNoAutoInherit returns a context that signals the create handler MUST
// NOT auto-default Fields[operated_by] or Fields[user_bead_id]. Calling with
// v=false returns ctx unchanged.
func WithNoAutoInherit(ctx context.Context, v bool) context.Context {
	if !v {
		return ctx
	}
	return context.WithValue(ctx, noAutoInheritKey{}, true)
}

// NoAutoInheritFromContext reports whether the X-No-Auto-Inherit opt-out is
// set on the request context.
func NoAutoInheritFromContext(ctx context.Context) bool {
	v, _ := ctx.Value(noAutoInheritKey{}).(bool)
	return v
}

// sanitizeUserBeadIDHeader validates an X-User-Bead-Id header value. The
// expected shape is a nanoid-style bead id ("ka-0000001713") emitted by
// internal/idgen. Anything outside `[A-Za-z0-9-]{1,64}` is rejected so a
// malformed header cannot smuggle control characters into Fields.
func sanitizeUserBeadIDHeader(raw string) (string, bool) {
	s := strings.TrimSpace(raw)
	if s == "" || len(s) > 64 {
		return "", false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '-':
		default:
			return "", false
		}
	}
	return s, true
}

// applyAutoInherit fills in Fields[operated_by] and Fields[user_bead_id] on
// the incoming raw fields JSON when the caller omitted them and the request
// context carries the corresponding value. Returns the (possibly-updated)
// fields JSON. Idempotent: if both fields are already populated, returns
// the input unchanged. The X-No-Auto-Inherit opt-out short-circuits to a
// no-op so legacy/backfill flows can preserve the previous semantics.
//
// A fields payload that is not a JSON object (array, scalar, malformed)
// is returned unchanged — downstream validation surfaces the right error
// for those shapes; this helper does not invent a body where one was
// already malformed.
func applyAutoInherit(ctx context.Context, fields json.RawMessage, tc *model.TypeConfig) json.RawMessage {
	if NoAutoInheritFromContext(ctx) {
		return fields
	}
	operatedBy := operatedByHeaderFromContext(ctx)
	userBeadID := UserBeadIDFromContext(ctx)

	// Resolve type-allowed fields up front. Auto-inherit must never invent
	// fields that ValidateTypeFields will then reject — AllowAdditionalFields
	// types accept anything, so we keep the value through.
	allowOperatedBy := tc == nil || tc.AllowAdditionalFields || typeHasField(tc, "operated_by")
	allowUserBeadID := tc == nil || tc.AllowAdditionalFields || typeHasField(tc, "user_bead_id")
	allowSlackUserID := tc == nil || tc.AllowAdditionalFields || typeHasField(tc, "slack_user_id")
	if !allowOperatedBy {
		operatedBy = ""
	}
	if !allowUserBeadID {
		userBeadID = ""
	}

	// Fast path: nothing from ctx to inherit AND no chance of deriving
	// slack_user_id (type discards it anyway). Slack derivation can still
	// fire from a caller-supplied Fields[operated_by], so we only skip the
	// parse when the result would be filtered out.
	if operatedBy == "" && userBeadID == "" && !allowSlackUserID {
		return fields
	}

	// Decode preserving original key order is not required — Fields is a
	// JSON object whose key order is not part of the contract. A map round
	// trip is sufficient and matches how updateBead merges Fields.
	m := make(map[string]any)
	trimmed := bytes.TrimSpace(fields)
	if len(trimmed) > 0 && string(trimmed) != "null" {
		if err := json.Unmarshal(trimmed, &m); err != nil {
			return fields
		}
		// json.Unmarshal of a JSON value that is not an object (e.g. array,
		// scalar) leaves m nil. Return the input unchanged in that case —
		// downstream validation surfaces the right error.
		if m == nil {
			return fields
		}
	}

	changed := false
	if operatedBy != "" && isMissingOrEmptyString(m, "operated_by") {
		m["operated_by"] = operatedBy
		changed = true
	}
	if userBeadID != "" && isMissingOrEmptyString(m, "user_bead_id") {
		m["user_bead_id"] = userBeadID
		changed = true
	}

	// Derive Fields[slack_user_id] from the final operated_by value when it
	// matches "slack:U…" and slack_user_id is empty. The slack: prefix is
	// stamped by the spawner (X-Operated-By header or per-site Fields push),
	// so the derived id is always a well-formed Slack user id.
	if allowSlackUserID && isMissingOrEmptyString(m, "slack_user_id") {
		if op, _ := m["operated_by"].(string); op != "" {
			if uid, ok := extractSlackUserID(op); ok {
				m["slack_user_id"] = uid
				changed = true
			}
		}
	}

	if !changed {
		return fields
	}
	out, err := json.Marshal(m)
	if err != nil {
		return fields
	}
	return out
}

// extractSlackUserID parses a "slack:U<alnum>" operated_by value into its
// Slack user id portion. Returns ("", false) for operated_by values that
// are not in this exact shape — "oidc:…", "system:legacy-pre-attribution",
// "slack:" (empty subject), "slack:bogus" (non-U-prefixed), or anything
// with non-alphanumeric subject characters.
//
// Slack user ids are uppercase-alphanumeric tokens starting with U
// (workspace-user prefix). Length is capped at 64 chars defensively so a
// malformed operated_by header that slipped past sanitization cannot
// smuggle giant values into Fields[slack_user_id].
func extractSlackUserID(operatedBy string) (string, bool) {
	const prefix = "slack:"
	if !strings.HasPrefix(operatedBy, prefix) {
		return "", false
	}
	rest := operatedBy[len(prefix):]
	if rest == "" || rest[0] != 'U' || len(rest) > 64 {
		return "", false
	}
	for _, r := range rest {
		switch {
		case r >= '0' && r <= '9',
			r >= 'A' && r <= 'Z':
		default:
			return "", false
		}
	}
	return rest, true
}

// typeHasField reports whether the type config declares a field with the
// given name. Returns false when tc is nil so the caller can treat "no
// config" as "no inheritance allowed".
func typeHasField(tc *model.TypeConfig, name string) bool {
	if tc == nil {
		return false
	}
	for i := range tc.Fields {
		if tc.Fields[i].Name == name {
			return true
		}
	}
	return false
}

// isMissingOrEmptyString reports true when key is absent from m, or its value
// is the empty string. Any non-empty value (including a non-string type) is
// treated as caller-supplied and left untouched — idempotency guarantee.
func isMissingOrEmptyString(m map[string]any, key string) bool {
	v, ok := m[key]
	if !ok {
		return true
	}
	s, isStr := v.(string)
	return isStr && s == ""
}
