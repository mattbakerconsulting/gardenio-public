package server

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"strings"

	"gardenio/kbeads/internal/model"
	"gardenio/kbeads/internal/store"
)

// approvedSpawnReplayKey marks a context as the replay of an agent create that
// an operator approved through an escalation. authorizeAgentSpawn treats it as
// the human authorization the privileged-role gate otherwise demands.
type approvedSpawnReplayKey struct{}

type approvedSpawnReplay struct {
	EscalationID string
	ApprovedBy   string
}

// withApprovedSpawnReplay stamps ctx as the replay of escalationID, approved by
// approvedBy. Only replayEscalation may call this; it is what lets a
// privileged-role spawn that was denied as escalatable actually run once the
// escalation is approved (gd-zErNIecGIc).
func withApprovedSpawnReplay(ctx context.Context, escalationID, approvedBy string) context.Context {
	return context.WithValue(ctx, approvedSpawnReplayKey{}, approvedSpawnReplay{EscalationID: escalationID, ApprovedBy: approvedBy})
}

// approvedSpawnReplayFromContext reports whether ctx is an approved-escalation
// replay, and of which escalation.
func approvedSpawnReplayFromContext(ctx context.Context) (escalationID, approvedBy string, ok bool) {
	v, ok := ctx.Value(approvedSpawnReplayKey{}).(approvedSpawnReplay)
	if !ok || v.EscalationID == "" {
		return "", "", false
	}
	return v.EscalationID, v.ApprovedBy, true
}

// spawnPermissions holds the parsed spawn authorization fields from a user bead.
type spawnPermissions struct {
	AllowedProjects []string
	AllowedRoles    []string
}

// authorizeAgentSpawn checks whether the current caller is allowed to create
// an agent bead with the given project and role.
//
// Non-OIDC callers (static token, fleet token — every agent today) bypass the
// OIDC allowed_projects/allowed_roles rules below entirely, but are instead
// gated on privileged-role spawn: see authorizeNonHumanSpawn.
//
// OIDC-authenticated humans keep the allowed_projects/allowed_roles rules on
// their user bead, unaffected by the privileged-role gate (gd-AWTT7AroK4,
// design doc gd-sGW5Y1B8sg §2).
func authorizeAgentSpawn(ctx context.Context, s store.Store, fields json.RawMessage) error {
	return authorizeAgentRole(ctx, s, "create", fields)
}

// authorizeAgentRole is authorizeAgentSpawn for either way an agent bead's role
// can be set: a create (action "create") or an update that sets or changes the
// role on an existing agent bead (action "update", gd-Yhbc5wdjwj). Both must
// clear the identical gate, so the denial a non-human caller gets, and the
// escalation it becomes, only differ in the action it replays.
func authorizeAgentRole(ctx context.Context, s store.Store, action string, fields json.RawMessage) error {
	// Read project and role by their exact keys, the way the controller does.
	// A struct decode matches keys case-insensitively, so an agent create
	// carrying "Role" next to "role" could show this gate one value and the
	// controller another; refuse such keys outright, for every caller.
	if variants := caseVariantKeys(fields, agentRoleField, "project"); len(variants) > 0 {
		return inputError("agent fields must spell role and project exactly; refused key(s): " + strings.Join(variants, ", "))
	}
	role, _, err := exactFieldString(fields, agentRoleField)
	if err != nil {
		return inputError("agent field role must be a string")
	}
	agentFields := struct{ Project, Role string }{projectFromFields(fields), role}

	// A member's own-machine agent (lands_agents.go) is written server-side
	// after landAgentMayStart, and only as lands-gardener in the land the
	// mint was for: the member holds no operator role, so the human path
	// below would refuse it.
	if land, ok := landAgentMintFromContext(ctx); ok {
		if action == "create" && isMachineWrite(ctx) && agentFields.Role == landsGardenerRole && agentFields.Project == land {
			return nil
		}
		return inputError("spawn denied: a member's own-machine agent is " + landsGardenerRole + " in the land it was minted for")
	}

	// A land principal takes the human path below, where actionTable
	// refuses it spawn (tierLand), not the service-token path.
	if !roleBearing(authMethodFromContext(ctx)) {
		// An agent token behind the land door spawns nothing
		// (land_door_agent.go), whatever it may do in-cluster.
		if d := landDoorAgentSpawnDenial(ctx, action); d != nil {
			return d
		}
		// A lands-tenant agent (Lands Track B §5.1) spawns only
		// lands-gardener, as the steward, in its own project. It runs before
		// every exemption below: a tenant never holds an approved escalation
		// (it may not file one), and a continuation or lineage marker must not
		// widen what the tenant itself may spawn.
		if err := landsTenantSpawnDenial(ctx, s, action, agentFields.Role, agentFields.Project); err != nil {
			return err
		}
		// An approved escalation replaying the very create this gate denied
		// is the one non-OIDC path that may spawn a privileged role: the
		// operator's approval IS the authorization (gd-zErNIecGIc). The
		// marker is set only by replayEscalation, after handleApproveEscalation
		// has authorized the approver, and it bypasses nothing but this gate.
		if esc, approvedBy, ok := approvedSpawnReplayFromContext(ctx); ok {
			slog.Info("spawn authorization: privileged-role gate satisfied by approved escalation replay",
				"role", agentFields.Role, "escalation", esc, "approved_by", approvedBy)
			return nil
		}
		// A create the trigger-engine makes for an admin-authored trigger is
		// authorized as that admin (spawn_lineage_authz.go). The marker is
		// computed server-side by createBead; updates never carry it.
		if lin, ok := triggerLineageSpawnFromContext(ctx); ok && action == "create" {
			slog.Info("spawn authorization: privileged-role gate satisfied by trigger lineage",
				"role", agentFields.Role, "project", agentFields.Project,
				"trigger_lineage", lin.TriggerID, "operated_by", lin.Author)
			return nil
		}
		// A same-role continuation of an existing agent bead, made by a caller
		// that may speak for it, inherits that bead's authorization
		// (spawn_continuation_authz.go). Computed server-side by createBead.
		if cont, ok := sameRoleContinuationFromContext(ctx); ok && action == "create" {
			slog.Info("spawn authorization: privileged-role gate satisfied by same-role continuation",
				"role", agentFields.Role, "project", agentFields.Project,
				"prior", cont.PriorBeadID, "caller", cont.Caller)
			return nil
		}
		return authorizeNonHumanSpawn(ctx, s, action, agentFields.Role)
	}

	// Admin role bypasses spawn restrictions.
	roles := RolesFromContext(ctx)
	if hasMinimumRole(roles, RoleAdmin) {
		return nil
	}

	// Viewers cannot spawn agents at all.
	if !hasMinimumRole(roles, RoleOperator) {
		return inputError("insufficient permissions: spawning agents requires operator role")
	}

	// A scoped principal's spawn project must be one of its token projects. The
	// user bead's allowed_projects is not consulted and an empty or missing
	// project is refused, not treated as unrestricted.
	if d := scopeFromContext(ctx); !d.Unrestricted && !d.allows(agentFields.Project) {
		return inputError("spawn denied: not authorized for project " + agentFields.Project)
	}
	// Owners and members spawn in their land; a guest does not (Q8c,
	// gd-fPXoeHDQS3). actionTable holds the split.
	if d := scopedTierDenial(ctx, actionSpawn, typeAgent, []string{"project:" + agentFields.Project}); d != nil {
		return inputError("spawn denied: " + d.Message)
	}

	// Look up the user's permissions from their user bead.
	id := AuthIdentityFromContext(ctx)
	email := id.Subject
	if email == "" {
		return inputError("cannot authorize spawn: no user identity")
	}

	perms, err := lookupSpawnPermissions(ctx, s, email)
	if errors.Is(err, errAmbiguousUserBead) {
		slog.Warn("spawn authorization: email is carried by more than one user bead", "email", email)
		return inputError("cannot authorize spawn: your email is on more than one user bead; ask an admin to fix the duplicate")
	}
	if err != nil {
		slog.Warn("spawn authorization: failed to look up user permissions", "email", email, "error", err)
		// Fail closed — deny if we can't verify permissions.
		return inputError("cannot authorize spawn: failed to verify user permissions")
	}
	if perms == nil {
		slog.Warn("spawn authorization: no user bead found", "email", email)
		return inputError("not authorized")
	}

	// Check project authorization.
	if agentFields.Project != "" && len(perms.AllowedProjects) > 0 {
		if !containsOrWildcard(perms.AllowedProjects, agentFields.Project) {
			return inputError("spawn denied: not authorized for project " + agentFields.Project)
		}
	}

	// Check role authorization.
	// Every element of a comma-joined role must be allowed, the same split the
	// controller resolves role beads by (parseRoleList).
	if len(perms.AllowedRoles) > 0 {
		for _, r := range parseRoleList(agentFields.Role) {
			if !containsOrWildcard(perms.AllowedRoles, r) {
				return inputError("spawn denied: not authorized for role " + r)
			}
		}
	}

	return nil
}

// authorizeNonHumanSpawn is the privileged-role spawn gate for a non-OIDC
// caller (static token with an agent subject, fleet token). When role names a
// privileged type:role bead, the create is denied as escalatable so the
// caller's `--escalate` retry becomes an escalation bead
// (write_authz.go handleEscalation) carrying spawn_role for the inbox
// (gd-AWTT7AroK4, design doc gd-sGW5Y1B8sg §2, §3). The primary static token
// is denied on the same footing — there is no header or other override that
// bypasses this for a non-OIDC caller (gd-HWWEVbCy8N: every agent pod holds
// the same primary token as the caller this gate exists to constrain, so a
// header-based override was itself the bypass). A human who needs a
// privileged spawn goes through the OIDC escalation-approve step instead
// (gd-731gBeGgl7). A role with no matching privileged type:role bead —
// including every non-privileged role — is unaffected.
func authorizeNonHumanSpawn(ctx context.Context, s store.Store, action, role string) error {
	if len(parseRoleList(role)) == 0 {
		return nil
	}

	privileged, err := roleIsPrivileged(ctx, s, role)
	if err != nil {
		// Fail closed on a lookup error (gd-jTIz9PSHlK): a role whose
		// privilege cannot be resolved is treated as privileged, so a caller
		// that times its spawn with a store error cannot skip the escalation
		// gate. The denial is the same escalatable one, so a retry once the
		// store recovers goes through and `--escalate` still reaches an
		// operator.
		slog.Warn("spawn authorization: failed to resolve role privilege; failing closed", "role", role, "error", err)
		d := &AuthzDenial{
			Action:      action,
			BeadType:    "agent",
			Escalatable: true,
			SpawnRole:   role,
			Message:     "spawn denied: could not verify whether role " + role + " is privileged; retry, or escalate",
		}
		populateAdvisoryTextForStore(ctx, s, d,
			"The daemon could not resolve whether role "+role+" is privileged, so it refused the spawn rather than risk skipping operator approval.")
		return d
	}
	if !privileged {
		return nil
	}

	d := &AuthzDenial{
		Action:      action,
		BeadType:    "agent",
		Escalatable: true,
		SpawnRole:   role,
		Message:     "spawn denied: role " + role + " is privileged; a non-human caller must escalate",
	}
	populateAdvisoryTextForStore(ctx, s, d,
		"Role "+role+" is privileged and requires operator approval for a non-human caller.")
	return d
}

// roleIsPrivileged reports whether ANY element of role (parseRoleList: split on
// commas, trim) names a type:role bead that holds the privileged capability
// tier OR a privileged grant-table row (roleGrantsPrivileged). The tier is read
// from capability_tiers by roleHoldsTier (gd-Pcyz0ftcTg), which still counts the
// legacy privileged flag and treats an unparseable set as privileged. Role beads
// are keyed by Title (validateRoleFieldsImpl, beadsapi.ListRoleBeads); a role
// with no matching bead — a builtin or project-default role with no dedicated
// bead — is treated as not privileged.
//
// A privileged row (ownershipGrantPrivileged: raised by its writer, all types,
// delete, or a whole type with no label or condition) lets every agent holding
// the role mutate beads it did not create, so a spawn into such a role, or
// re-roling an agent bead into one, is the same authority grant as a
// privileged role and must escalate for a non-human caller (gd-kLRIbndGNp).
// A narrow row (one type and label, or a caller-bound condition) does not count
// (gd-Hhp3DsjOgL): a crew can spawn a child into the herder role and pick up
// its reach, which is small and undone by the next reply to a wish it closed.
// A role that needs a row whose close cannot be undone must raise the row.
// An element-wise check is what keeps "crew,infra", " infra" and "infra " from
// slipping past an exact-string compare while the controller still resolves
// them to the infra role bead (gd-Yhbc5wdjwj).
func roleIsPrivileged(ctx context.Context, s store.Store, role string) (bool, error) {
	elems := parseRoleList(role)
	if len(elems) == 0 {
		return false, nil
	}
	beads, err := roleBeadsByTitle(ctx, s, elems)
	if err != nil {
		return false, err
	}
	for _, b := range beads {
		if roleHoldsTier(b.Fields, model.CapabilityTierPrivileged) {
			return true, nil
		}
		if roleGrantsPrivileged(b) {
			return true, nil
		}
	}
	return false, nil
}

// roleBeadsByTitle returns the type:role beads whose title is exactly one of
// names, looked up by the store (BeadFilter.Titles) rather than by listing every
// role bead and matching in Go. The lookup is unlimited on purpose: an exact
// title matches a handful of beads, and a LIMIT over an unordered list could
// silently drop the one bead carrying a grant once the type outgrew it.
//
// Role beads carry no project: label and the controller resolves a role by its
// title alone (beadsapi.ListRoleBeads folds duplicates by name, across
// projects), so a role name is one global namespace and so is this lookup:
// ANY matching bead with the field set grants it. That is deliberately the
// conservative reading for an authority grant. A project that wants its own
// variant must give it a distinct role name.
func roleBeadsByTitle(ctx context.Context, s store.Store, names []string) ([]*model.Bead, error) {
	beads, _, err := s.ListBeads(ctx, model.BeadFilter{
		Type:   []model.BeadType{"role"},
		Titles: names,
	})
	return beads, err
}

// agentRoleFieldChange reports the new role string when an update's fields patch
// sets or changes `role` on an existing agent bead, and ok=false when the
// patch leaves the role alone (no role key, or the parsed role list is
// unchanged — a read-modify-write client echoing the stored value back, or
// only re-spacing it). Clearing the role is a change but never grants anything.
func agentRoleFieldChange(patch, existing json.RawMessage) (newRole string, ok bool) {
	if len(patch) == 0 {
		return "", false
	}
	newRole, present, err := exactFieldString(patch, agentRoleField)
	if err != nil || !present {
		// A non-string role is refused up front by authorizeAgentRoleUpdate.
		return "", false
	}
	return newRole, !roleListsEqual(parseRoleList(newRole), parseRoleList(roleFromFields(existing)))
}

// authorizeAgentRoleUpdate gates an update to an existing agent bead that sets
// or changes its `role` through the same privileged-role gate as a spawn
// (authorizeAgentRole): a non-human caller is denied as escalatable when the
// new role names a privileged role, an OIDC operator is held to their
// allowed_roles, an OIDC gardenio-admin passes. A patch that leaves role alone
// (agent_state, notes, ...) is not gated here. Without this a pod's role — and
// with it the ServiceAccount the controller resolves from it on the next
// (re)start — could be rewritten to infra after a perfectly ordinary spawn.
func authorizeAgentRoleUpdate(ctx context.Context, s store.Store, bead *model.Bead, patch json.RawMessage) error {
	if bead == nil || bead.Type != "agent" {
		return nil
	}
	if variants := caseVariantKeys(patch, agentRoleField, "project"); len(variants) > 0 {
		return inputError("agent fields must spell role and project exactly; refused key(s): " + strings.Join(variants, ", "))
	}
	if _, _, err := exactFieldString(patch, agentRoleField); err != nil {
		return inputError("agent field role must be a string")
	}
	if err := authorizeAgentProjectUpdate(ctx, bead, patch); err != nil {
		return err
	}
	newRole, changed := agentRoleFieldChange(patch, bead.Fields)
	if !changed {
		return nil
	}
	gated, _ := json.Marshal(map[string]string{
		"project": projectFromFields(bead.Fields),
		"role":    newRole,
	})
	return authorizeAgentRole(ctx, s, "update", gated)
}

// agentProjectFieldChange reports whether an update's fields patch sets or
// changes `project` on an existing agent bead. An echo of the stored value is not
// a change.
func agentProjectFieldChange(patch, existing json.RawMessage) bool {
	newProject, present, err := exactFieldString(patch, "project")
	if err != nil || !present {
		return err != nil
	}
	return newProject != projectFromFields(existing)
}

// authorizeAgentProjectUpdate refuses a non-human caller changing `project` on an
// existing agent bead. The project is fixed when the agent is spawned (the spawn
// gate checked it then), and the same-role continuation gate trusts it on the
// prior bead: an agent that could rewrite its own bead's project could continue
// into any project's privileged lane (gd-VuYwGGYLLT). OIDC sessions and approved
// escalation replays keep the right; a machine principal and a v1 edge envelope
// (gd-AvSKOUyqRz) are held to it like the static token (nothing in the
// controller rewrites an agent bead's project).
func authorizeAgentProjectUpdate(ctx context.Context, bead *model.Bead, patch json.RawMessage) error {
	if bead == nil || bead.Type != "agent" {
		return nil
	}
	if !agentProjectFieldChange(patch, bead.Fields) || isMachineWrite(ctx) {
		return nil
	}
	switch authMethodFromContext(ctx) {
	case authMethodOIDC, authMethodNone:
		return nil
	}
	return inputError("agent field project is fixed at spawn and cannot be changed by this caller")
}

// errAmbiguousUserBead is lookupSpawnPermissions' error when more than one user
// bead carries the email. Every caller must fail closed on it: which bead's
// allowed_projects/allowed_roles applied would otherwise depend on row order.
var errAmbiguousUserBead = errors.New("more than one user bead carries this email")

// lookupSpawnPermissions finds the user bead for the given email and parses
// spawn permission fields. Returns nil if no user bead exists, and
// errAmbiguousUserBead if more than one does.
func lookupSpawnPermissions(ctx context.Context, s store.Store, email string) (*spawnPermissions, error) {
	bead, err := userBeadByEmail(ctx, s, email)
	if err != nil || bead == nil {
		return nil, err
	}

	perms := &spawnPermissions{}
	if len(bead.Fields) > 0 {
		var raw map[string]json.RawMessage
		if json.Unmarshal(bead.Fields, &raw) == nil {
			perms.AllowedProjects = parseStringList(raw["allowed_projects"])
			perms.AllowedRoles = parseStringList(raw["allowed_roles"])
		}
	}
	return perms, nil
}

// userBeadByEmail returns the one type:user config bead whose email field
// matches email case-insensitively (an email address is matched the way the
// operator aliases canonicalise it, canonicalAliasSubject), nil if none does,
// and errAmbiguousUserBead if more than one does — the same refusal the bridge
// makes for a slack_id on two user beads. Every user bead is listed and matched
// here rather than by the store's exact-case JSONB field filter: a LIMIT 1 over
// that filter missed a mixed-case bead and silently picked one of two.
func userBeadByEmail(ctx context.Context, s store.Store, email string) (*model.Bead, error) {
	want := strings.TrimSpace(email)
	if want == "" {
		return nil, nil
	}
	beads, _, err := s.ListBeads(ctx, model.BeadFilter{
		Type: []model.BeadType{"user"},
		Kind: []model.Kind{"config"},
	})
	if err != nil {
		return nil, err
	}
	var match *model.Bead
	for _, b := range beads {
		if !strings.EqualFold(strings.TrimSpace(parseFieldsToStringMap(b.Fields)["email"]), want) {
			continue
		}
		if match != nil {
			return nil, errAmbiguousUserBead
		}
		match = b
	}
	return match, nil
}

// parseStringList parses a JSON field value as either a JSON array of strings
// or a comma-separated string. Returns nil if the value is empty or unparseable.
func parseStringList(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	// Try JSON array first.
	var arr []string
	if json.Unmarshal(raw, &arr) == nil {
		return arr
	}
	// Fall back to comma-separated string.
	var s string
	if json.Unmarshal(raw, &s) == nil && s != "" {
		parts := make([]string, 0)
		for _, p := range splitComma(s) {
			if p != "" {
				parts = append(parts, p)
			}
		}
		return parts
	}
	return nil
}

// splitComma splits a string by commas and trims whitespace.
func splitComma(s string) []string {
	var result []string
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			result = append(result, part)
		}
	}
	return result
}

// containsOrWildcard returns true if vals contains item or "*".
func containsOrWildcard(vals []string, item string) bool {
	for _, v := range vals {
		if v == "*" || v == item {
			return true
		}
	}
	return false
}

// agentRoleField is the agent-bead field whose value picks the role beads, and
// so the ServiceAccount and secrets, the controller resolves for the pod.
const agentRoleField = "role"

// denyAgentRoleKeyWrite gates the single-field mutation endpoints (increment,
// append, reorder, edit) and the extra-fields merge on close when they name the
// `role` key of an agent bead (gd-Yhbc5wdjwj). Those write through store
// primitives keyed on the bead id, bypassing updateBead's role gate, and only
// the key — not the resulting value — is known here, so the privileged-role
// check cannot run: touching the key at all needs an OIDC caller, as for the
// user identity fields. Machine-write contexts (an approved escalation replay)
// pass. Never escalatable: there is no value to show an approver.
func (s *BeadsServer) denyAgentRoleKeyWrite(ctx context.Context, action string, keys []string) *AuthzDenial {
	if !(slices.Contains(keys, agentRoleField) || slices.Contains(keys, "project")) || isMachineWrite(ctx) {
		return nil
	}
	switch authMethodFromContext(ctx) {
	case authMethodOIDC, authMethodNone:
		return nil
	}
	d := &AuthzDenial{
		Action:       action,
		BeadType:     "agent",
		RequiredRole: RoleAdmin,
		Message: "insufficient permissions: " + action + " on type agent touching field role through a field-mutation endpoint requires " +
			RoleAdmin + " (OIDC); set role with an update, which escalates for a privileged role (project is fixed at spawn)",
	}
	s.populateAdvisoryText(ctx, d, "An agent's role selects its pod's ServiceAccount.")
	return d
}
