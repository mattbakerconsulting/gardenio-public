package server

import (
	"context"
	"encoding/json"
	"reflect"
	"sort"
	"strings"

	"gardenio/kbeads/internal/model"
)

// userIdentityFields are the type:user fields that decide WHO a person is and
// what they may do: the controller bridge's alias resync turns the identity
// subset into operator_aliases rows (so a value here makes that identity
// resolve to the person's operator), and the rest are the person's access
// grants. Writing any of them is an authority grant, not a profile edit
// (gd-uAvleAZUvV).
//
// The machine allow-list in machineAllowedWrites admits {create,user} and
// {update,user} for the shared static token and fleet tokens so beads-viewer's
// self-service writes (status_message, preferences, profile_prefs, inbox
// last-visit stamp, display_name, ...) keep working. Those callers are NOT
// trusted with this field set: the shared static token is held by every agent
// pod, so letting it write `email` or `slack_id` onto the owner's user bead
// let any agent attach its own identity to the owner's operator. Changing one
// of these needs an OIDC gardenio-admin (or an operator-approved escalation
// replay, which runs under withMachineWrite).
var userIdentityFields = map[string]bool{
	"email":            true,
	"email_aliases":    true,
	"slack_id":         true,
	"slack_user_id":    true,
	"gitlab_username":  true,
	"github_username":  true,
	"jira_user_id":     true,
	"jira_user":        true,
	"allowed_roles":    true,
	"allowed_projects": true,
	"capabilities":     true,
	"auth_methods":     true,

	// The forge's numeric user ids pin who a gitlab/github login is: the
	// bridge trusts a bead matching the id over one matching the login, so
	// writing one is an authority grant like the username (gd-yrnMQbmLgp).
	"gitlab_user_id": true,
	"github_user_id": true,
}

// isGatedUserField reports whether a type:user field key is one only an OIDC
// gardenio-admin may write through the generic paths: an identity field, a
// terms-acceptance field (http_user_terms.go), which the person writes for
// themselves through POST /v1/users/me/terms, or read_cursors
// (http_read_cursors.go), which the person writes for themselves through POST
// /v1/users/me/read-cursors. Without the gate the static token every agent
// pod holds could mark any person's channels read, skipping that route's
// size cap and id checks. Standing consent (userStandingConsentFields) is
// gated the same way: the person sets it through POST
// /v1/users/me/consent-push, so an agent cannot consent for them
// (gd-QV2WuUjely). So is welcome_tour_seen_at (http_welcome_tour.go), which
// the person stamps through POST /v1/users/me/welcome-tour.
func isGatedUserField(k string) bool {
	return userIdentityFields[k] || userTermsFields[k] || k == readCursorsField || userStandingConsentFields[k] || k == userWelcomeTourField
}

// changedUserIdentityFields returns the sorted identity-field keys that a
// create (existing == nil) or update (existing = the stored fields) would set
// to a value different from what is stored. Unchanged keys echoed back by a
// read-modify-write client (beadsapi.Client.UpdateBeadFields) do not count,
// and neither does a create that leaves a key empty.
func changedUserIdentityFields(patch, existing json.RawMessage) []string {
	if len(patch) == 0 {
		return nil
	}
	var p, e map[string]any
	if err := json.Unmarshal(patch, &p); err != nil {
		return nil
	}
	if len(existing) > 0 {
		_ = json.Unmarshal(existing, &e)
	}
	var changed []string
	for k, pv := range p {
		if !isGatedUserField(k) {
			continue
		}
		ev, had := e[k]
		if had && reflect.DeepEqual(pv, ev) {
			continue
		}
		if !had && isEmptyFieldValue(pv) {
			continue
		}
		changed = append(changed, k)
	}
	sort.Strings(changed)
	return changed
}

func isEmptyFieldValue(v any) bool {
	switch t := v.(type) {
	case nil:
		return true
	case string:
		return strings.TrimSpace(t) == ""
	case []any:
		return len(t) == 0
	case map[string]any:
		return len(t) == 0
	}
	return false
}

// authorizeUserIdentityFields is the field-level gate on type:user writes.
// patch is the incoming fields (create body or update patch); existing is the
// stored fields on update, nil on create. It only restricts non-OIDC callers
// (static / fleet token): an OIDC caller already passed min_role
// gardenio-admin in authorizeWriteTitled, and withMachineWrite (escalation replay)
// carries an operator's approval.
//
// This is the whole-body form used by create/update (where the values are in
// hand and an echoed unchanged value is tolerated). The single-field mutation
// endpoints use authorizeUserBeadFieldWrite, which funnels into the same
// denyUserIdentityFields so there is ONE definition of "who may touch an
// identity field".
func (s *BeadsServer) authorizeUserIdentityFields(ctx context.Context, action string, beadType model.BeadType, patch, existing json.RawMessage) *AuthzDenial {
	if beadType != "user" {
		return nil
	}
	return s.denyUserIdentityFields(ctx, action, changedUserIdentityFields(patch, existing))
}

// denyUserIdentityFields is the single decision point: given the identity
// fields a write would change on a type:user bead, it returns a denial unless
// the caller is an OIDC gardenio-admin (or an approved escalation replay).
func (s *BeadsServer) denyUserIdentityFields(ctx context.Context, action string, changed []string) *AuthzDenial {
	if len(changed) == 0 || isMachineWrite(ctx) {
		return nil
	}
	switch authMethodFromContext(ctx) {
	case authMethodNone:
		return nil
	case authMethodOIDC:
		// A signed-in caller must hold gardenio-admin HERE, not just pass the
		// type:user write_policy: that policy is a stored config, and a
		// weakened copy (gd-32dXqWb95Y) would otherwise let any signed-in
		// person re-point another person's slack_id / email_aliases at
		// themselves. Self-service linking goes through
		// /v1/identity-links/slack, which does not use this gate.
		if hasMinimumRole(RolesFromContext(ctx), RoleAdmin) {
			return nil
		}
	}
	d := &AuthzDenial{
		Action:       action,
		BeadType:     "user",
		RequiredRole: RoleAdmin,
		Escalatable:  true,
		Message: "insufficient permissions: " + action + " on type user touching identity, terms, read-cursor or standing-consent field(s) " +
			strings.Join(changed, ", ") + " requires " + RoleAdmin + " (OIDC)",
	}
	s.populateAdvisoryText(ctx, d, "Identity fields on a user bead grant access, and terms acceptance, read positions and standing consent are recorded only by the person; all need "+RoleAdmin+".")
	return d
}

// authorizeUserBeadFieldWrite gates the single-field mutation endpoints
// (increment, append, reorder, edit) and the extra-fields merge on close. They
// write through store primitives keyed on the bead id, bypassing updateBead,
// so before gd-uAvleAZUvV's follow-up a static token could append to
// email_aliases and get 200. For a type:user bead it runs the same write
// policy as PATCH (min_role + the machine allow-list) and then refuses any
// identity field key from a non-OIDC caller. Only the key is known here (not
// the resulting value), so touching an identity key at all is denied; there is
// no "echo of the stored value" case for these primitives. Other bead types
// are untouched.
func (s *BeadsServer) authorizeUserBeadFieldWrite(ctx context.Context, action string, bead *model.Bead, keys ...string) *AuthzDenial {
	if bead != nil && bead.Type == "agent" {
		if denial := s.denyAgentRoleKeyWrite(ctx, action, keys); denial != nil {
			return denial
		}
		return s.planScopeBindingKeyDenial(ctx, action, keys)
	}
	if bead == nil || bead.Type != "user" {
		return nil
	}
	if denial := s.authorizeWriteForBead(ctx, "update", bead); denial != nil {
		return denial
	}
	var changed []string
	for _, k := range keys {
		if isGatedUserField(k) {
			changed = append(changed, k)
		}
	}
	sort.Strings(changed)
	return s.denyUserIdentityFields(ctx, action, changed)
}

func mapKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

// authorizeSetConfig is the gate on SetConfig (PUT /v1/configs/{key}). The
// route is requireRoleMachineAllowed, so the shared static token every agent
// pod holds reaches it (the beads-viewer e2e seed registers custom type:*
// rows with it). That must not extend to rewriting a type config that carries
// authorization: an agent could replace type:user with a write_policy lacking
// min_role and so unlock identity-field writes for any signed-in caller
// (gd-32dXqWb95Y). A non-OIDC caller is therefore refused on a type:* key
// that is builtin, already stored with a write_policy, or being written with
// one; registering a plain custom type is unaffected. OIDC callers are
// role-gated (admin) by the route; escalation replay and no-auth deployments
// keep their authority. A v1 edge envelope is gated like the static token
// (gd-AvSKOUyqRz).
func (s *BeadsServer) authorizeSetConfig(ctx context.Context, config *model.Config) *AuthzDenial {
	if isMachineWrite(ctx) || !strings.HasPrefix(config.Key, "type:") {
		return nil
	}
	switch authMethodFromContext(ctx) {
	case authMethodOIDC, authMethodNone:
		return nil
	}
	if _, builtin := builtinConfigs[config.Key]; !builtin &&
		!typeConfigHasWritePolicy(config.Value) && !s.storedTypeConfigHasWritePolicy(ctx, config.Key) {
		return nil
	}
	d := &AuthzDenial{
		Action:       "update",
		BeadType:     "config",
		RequiredRole: RoleAdmin,
		Escalatable:  true,
		Message: "insufficient permissions: set config " + config.Key +
			" changes a type's authorization policy and requires " + RoleAdmin + " (OIDC)",
	}
	s.populateAdvisoryText(ctx, d, "Type configs that carry write policy need "+RoleAdmin+".")
	return d
}

func typeConfigHasWritePolicy(value json.RawMessage) bool {
	var tc struct {
		WritePolicy *json.RawMessage `json:"write_policy"`
	}
	return json.Unmarshal(value, &tc) == nil && tc.WritePolicy != nil && string(*tc.WritePolicy) != "null"
}

func (s *BeadsServer) storedTypeConfigHasWritePolicy(ctx context.Context, key string) bool {
	stored, err := s.store.GetConfig(ctx, key)
	return err == nil && stored != nil && typeConfigHasWritePolicy(stored.Value)
}
