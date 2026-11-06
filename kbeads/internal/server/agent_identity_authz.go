package server

import (
	"context"
	"encoding/json"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"gardenio/kbeads/internal/model"
)

// agentK8sIdentityFields are the type:agent fields that would pick the pod's
// Kubernetes identity: service_account names the ServiceAccount, and
// needs_k8s_api=true mounts its API token. A pod's identity must come only from
// config an admin wrote (the role bead, the project bead, the chart default), so
// writing either on an agent bead is an authority grant, not a profile edit.
// The controller also refuses to read them off an agent bead
// (cmd/controller/podspec_k8s_identity.go); this gate stops them being stored.
//
// The values that only narrow (needs_k8s_api=false, an empty service_account)
// are not gated: they can lower a pod's authority, never raise it.
var agentK8sIdentityFields = map[string]bool{
	"service_account": true,
	"needs_k8s_api":   true,
}

// configK8sIdentityFields are the same grants on the beads the controller DOES
// read a pod's identity from: type:role and type:project. machineAllowedWrites
// lets the shared static token create/update project beads and create role
// beads for unrelated plumbing (gb project create, gb pool enable, /schedule),
// so without a field-level gate that token could write the very config the
// controller trusts. privileged is the role flag that routes a spawn through
// the escalation path (spawn_authz.go roleIsPrivileged).
var configK8sIdentityFields = map[string]bool{
	"service_account": true,
	"needs_k8s_api":   true,
	"privileged":      true,
	// ownership_grants holds the role's grant-table rows (ownership_grants.go),
	// which widen what every agent holding the role may write, so it is an
	// authority grant gated the same way. machineAllowedWrites lets a machine
	// token create role beads.
	roleOwnershipGrantsField: true,
	// The pod-spec keys the controller turns into containers, mounts and
	// sandbox choice (gd-ekYHj07AtJ). They are the same class of grant as the
	// ones above: docker=true adds a privileged DinD sidecar, secrets[] mounts
	// Secrets as env, image / runtime_class / channel_runtime_class pick the
	// container and sandbox, and ziti_* hands the pod overlay-network access.
	// machineAllowedWrites lets a machine token create/update these beads for
	// unrelated plumbing, so without this the token could shape the pod itself.
	"docker":                true,
	"secrets":               true,
	"image":                 true,
	"runtime_class":         true,
	"channel_runtime_class": true,
	"ziti_access":           true,
	"ziti_service_id":       true,
	"ziti_bind":             true,
	// capability_tiers is the set privileged is derived from (gd-Bcqj2r4m7E),
	// so it is gated the same way. On a role, every change to it counts, a
	// removal included: a restricted tier such as lands-tenant narrows a role,
	// so dropping one widens it (changedAgentIdentityFields).
	model.RoleCapabilityTiersField: true,
	// machine_client picks the Keycloak machine client whose secret the
	// controller mounts into the role's pods (escalation routing Task 15,
	// C5), so a machine token naming one would choose who holds it.
	"machine_client": true,
	// verifier=true runs the role's pods as the chart's read-only verifier
	// ServiceAccount with its API token (P9.2, gd-6ujfeTZfNL). It needs no
	// privileged flag, since the SA's reads are fixed in the chart and hold no
	// secrets, but who gets a cluster token is still an admin's call.
	"verifier": true,
}

// podIdentityKeys is every key gated on any bead type; it is the cheap
// pre-filter for the field-mutation endpoints.
var podIdentityKeys = map[string]bool{
	"service_account":              true,
	"needs_k8s_api":                true,
	"privileged":                   true,
	roleOwnershipGrantsField:       true,
	"docker":                       true,
	"secrets":                      true,
	"image":                        true,
	"runtime_class":                true,
	"channel_runtime_class":        true,
	"ziti_access":                  true,
	"ziti_service_id":              true,
	"ziti_bind":                    true,
	model.RoleCapabilityTiersField: true,
	"machine_client":               true,
	"verifier":                     true,
}

// podIdentityFieldsFor returns the gated key set for a bead type, or nil for a
// type that carries no pod identity.
func podIdentityFieldsFor(t model.BeadType) map[string]bool {
	switch t {
	case "agent":
		return agentK8sIdentityFields
	case "role", "project":
		return configK8sIdentityFields
	}
	return nil
}

// agentIdentityFieldGrants reports whether value would raise the pod's identity
// or capabilities for field: any non-empty service_account, secrets, image,
// runtime class or ziti service, or a boolean flag (needs_k8s_api, privileged,
// docker, ziti_access, ...) that is true.
func agentIdentityFieldGrants(field string, value any) bool {
	switch field {
	case "service_account", "secrets", "image", "runtime_class", "channel_runtime_class", "ziti_service_id", "machine_client", roleOwnershipGrantsField:
		if str, ok := value.(string); ok {
			// A JSON-encoded empty list/object clears the field; it grants nothing.
			if t := strings.TrimSpace(str); t == "[]" || t == "{}" || t == "null" {
				return false
			}
		}
		return !isEmptyFieldValue(value)
	case "needs_k8s_api", "privileged", "docker", "ziti_access", "ziti_bind", "verifier":
		switch t := value.(type) {
		case bool:
			return t
		case string:
			b, err := strconv.ParseBool(strings.TrimSpace(t))
			return err == nil && b
		}
	}
	return false
}

// changedAgentIdentityFields returns the sorted gated keys that a create
// (existing == nil) or update (existing = the stored fields) would set to a
// granting value different from what is stored. An unchanged value echoed back
// by a read-modify-write client (beadsapi.Client.UpdateBeadFields) does not
// count, so an agent bead that already carries one of these before this gate
// shipped stays updatable; a narrowing value never counts.
func changedAgentIdentityFields(gated map[string]bool, patch, existing json.RawMessage) []string {
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
		if !gated[k] {
			continue
		}
		if k == model.RoleCapabilityTiersField {
			if capabilityTiersChanged(pv, e, existing != nil) {
				changed = append(changed, k)
			}
			continue
		}
		if !agentIdentityFieldGrants(k, pv) {
			continue
		}
		if ev, had := e[k]; had && reflect.DeepEqual(pv, ev) {
			continue
		}
		changed = append(changed, k)
	}
	sort.Strings(changed)
	return changed
}

// capabilityTiersChanged reports whether a capability_tiers value is an
// authority change. On create (isUpdate false) only a non-empty set counts: the
// daemon stamps an empty set on every role it creates, and an empty set holds
// no tier. On update any change to the set counts, an emptied one included,
// because removing a restricted tier widens the role. A value that does not
// parse as a set counts, so a malformed write never slips past as a no-op; the
// schema check then refuses it. The comparison is on the set, so an unchanged
// set echoed back in a different order is not a change.
func capabilityTiersChanged(patchValue any, existing map[string]any, isUpdate bool) bool {
	next, ok := model.ParseCapabilityTiers(patchValue)
	if !ok {
		return true
	}
	if !isUpdate {
		return len(next) > 0
	}
	prior, ok := model.ParseCapabilityTiers(existing[model.RoleCapabilityTiersField])
	if !ok {
		return true
	}
	return !reflect.DeepEqual(next, prior)
}

// authorizeAgentIdentityFields is the field-level gate on type:agent, type:role
// and type:project writes.
// patch is the incoming fields (create body or update patch); existing is the
// stored fields on update, nil on create. See denyAgentIdentityFields.
func (s *BeadsServer) authorizeAgentIdentityFields(ctx context.Context, action string, beadType model.BeadType, patch, existing json.RawMessage) *AuthzDenial {
	gated := podIdentityFieldsFor(beadType)
	if gated == nil {
		return nil
	}
	return s.denyAgentIdentityFields(ctx, action, beadType, changedAgentIdentityFields(gated, patch, existing))
}

// denyAgentIdentityFields is the single decision point: given the identity
// fields a write would set on an agent, role or project bead, it returns a 403 denial unless
// the caller is an OIDC gardenio-admin, an auth-disabled daemon, or an approved
// escalation replay (which runs under withMachineWrite and carries an
// operator's approval). A static or fleet token (every agent pod) and a v1
// edge envelope, which carries no role (gd-AvSKOUyqRz), are refused.
//
// The controller, which owns pod specs, never writes either field on an agent
// bead: it only reads them from role and project beads. If it ever has to echo
// one back unchanged in a read-modify-write, changedAgentIdentityFields lets
// that through.
func (s *BeadsServer) denyAgentIdentityFields(ctx context.Context, action string, beadType model.BeadType, changed []string) *AuthzDenial {
	if len(changed) == 0 || isMachineWrite(ctx) {
		return nil
	}
	switch authMethodFromContext(ctx) {
	case authMethodNone:
		return nil
	case authMethodOIDC:
		if hasMinimumRole(RolesFromContext(ctx), RoleAdmin) {
			return nil
		}
	}
	d := &AuthzDenial{
		Action:       action,
		BeadType:     string(beadType),
		RequiredRole: RoleAdmin,
		Escalatable:  true,
		Message: "insufficient permissions: " + action + " on type " + string(beadType) + " setting " +
			strings.Join(changed, ", ") + " requires " + RoleAdmin +
			"; a pod's ServiceAccount, API token, write authority, sidecars, secrets, image and sandbox come from admin-written role and project config, never from the caller",
	}
	s.populateAdvisoryText(ctx, d, "A pod's Kubernetes identity comes only from admin-written role and project config or the chart default; setting it on a "+string(beadType)+" bead needs "+RoleAdmin+".")
	return d
}

// authorizeAgentBeadFieldWrite gates the single-field mutation endpoints and
// the extra-fields merge on an agent, role or project bead, and (through
// authorizeChannelBindingKeyWrite) a channel's Slack binding and (through
// authorizeLandSwitchKeyWrite) a project's land kind and freeze. Only the key is
// known, not the resulting value, so touching a gated key at all is denied for
// a non-admin. Other bead types are untouched.
func (s *BeadsServer) authorizeAgentBeadFieldWrite(ctx context.Context, action string, bead *model.Bead, keys ...string) *AuthzDenial {
	if bead == nil {
		return nil
	}
	if denial := spawnLineageSealDenial(action, bead.Type, keys...); denial != nil {
		return denial
	}
	if denial := impersonatedByKeyDenial(action, bead.Type, keys...); denial != nil {
		return denial
	}
	if denial := autonomousFieldWriteDenial(ctx, action, bead, keys...); denial != nil {
		return denial
	}
	if denial := agentParkFieldKeyDenial(ctx, action, bead, keys...); denial != nil {
		return denial
	}
	if denial := s.authorizeChannelBindingKeyWrite(ctx, action, bead, keys...); denial != nil {
		return denial
	}
	if denial := s.authorizeLandSwitchKeyWrite(ctx, action, bead, keys...); denial != nil {
		return denial
	}
	if denial := s.wishHeadKeyDenial(ctx, action, bead, keys...); denial != nil {
		return denial
	}
	gated := podIdentityFieldsFor(bead.Type)
	if gated == nil {
		return nil
	}
	var changed []string
	for _, k := range keys {
		if gated[k] {
			changed = append(changed, k)
		}
	}
	sort.Strings(changed)
	return s.denyAgentIdentityFields(ctx, action, bead.Type, changed)
}
