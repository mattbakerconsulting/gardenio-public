package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
	"strings"

	"gardenio/kbeads/internal/model"
)

// This file implements the three taints ADR §6.3 "Approval" corrections for
// the shared handleApproveEscalation handler (gd-731gBeGgl7, per the
// shared-handler decision gd-CxfzKo5VOJ: one fix for every escalation
// consumer — spawn today, scrub-request and others later — rather than a
// per-domain carve-out):
//
//  1. approved_by is SERVER-DERIVED from the authenticated identity, never
//     trusted from the request body (AUDIT-2026-04-18 §C3), mirroring the
//     decisionAttribution precedent already used for decisions.
//  2. Any caller with no OIDC-derived human identity behind it is refused
//     outright — closing the gap decision gd-CxfzKo5VOJ named: today a bare
//     static/fleet token reaches handleApproveEscalation with zero identity
//     check because requireRole short-circuits for non-OIDC auth methods.
//  3. Four-eyes: when the escalation's policy demands it
//     (requires_four_eyes), the resolved approver must not be the same
//     operator as the escalation's requester, compared AFTER alias
//     resolution (operator_aliases), mirroring write_authz.go's
//     callerIsBeadOperator pattern.

// escalationApprover resolves the OIDC-derived human identity permitted to
// approve an escalation. It returns ("", false) when no such identity
// resolves, which the handler turns into a 403 instead of falling back to a
// spoofable service actor or an unauthenticated request body value the way
// decisionAttribution does for decisions — this handler is OIDC only.
//
// Exactly one accepted path: AuthIdentityFromContext.Type is "oidc" or
// "edge" — the caller (or the gardenio-edge-auth proxy in front of it,
// stamped as OIDC-equivalent per the additive-enforcement decision
// ka-0000001641) verified the human's own OIDC / oauth2-proxy session
// directly. Subject is the human's bare email.
//
// Prior to gd-ZjiSd9Kzio round 3 this also accepted a second path: any
// caller (AuthIdentityFromContext.Type anything else — static, fleet, "",
// anonymous) that threaded a human via X-Operated-By in "oidc:<email>" form.
// That path was decision gd-CxfzKo5VOJ correction 2's deliberate accommodation
// for beads-viewer's web UI approve button, which authenticates to the
// daemon with the primary static token and relays the operator's own OIDC
// login this way (ka-0000001759). The round-3 merge-desk review on
// gd-ZjiSd9Kzio flagged it as reachable by ANY holder of that same shared
// static token — every agent pod, not just beads-viewer — because kbeads has
// no way to tell beads-viewer's relay apart from a spoofed X-Operated-By
// header sent by an agent approving (and, via replayEscalation's
// withMachineWrite, fully executing) its own privilege escalation. Unlike
// the enumerated requireRoleMachineAllowed routes, there is no per-identity
// signal here to allow-list beads-viewer specifically (it is not
// fleet-scoped — see gardenio/helm/gardenio/templates/beads-viewer/
// deployment.yaml, which sources the same beads.tokenSecret every agent pod
// uses, unlike slack-bridge/gitlab-bridge/jira-bridge's distinguishable
// bridgeTokenSecretName fleet tokens), so the fallback is removed rather
// than narrowed. Restoring a relay path requires beads-viewer to present a
// distinguishable, genuinely-authenticated credential — either a
// fleet-scoped bridge token (mirroring the three bridges above, narrowly
// allow-listed here the way requireRoleMachineAllowed enumerates routes) or,
// preferably, the per-request user-bearer path already scaffolded in
// beadsapi (WithUserBearer/doJSONAsUser, used today by CreateTaint) so the
// daemon authenticates the actual human's own OIDC token instead of a
// header claim over a shared secret. Tracked as a follow-up in
// gd-J8Fet2YH6y; until then beads-viewer's approve/reject buttons 403 with
// "escalation approval requires an OIDC-authenticated identity" the same as
// any other non-OIDC caller.
//
// PAGE SESSION ONLY (P10.B gd-4mEAhIE01U, owner decision gd-CJUlVSMzEe). An
// "oidc" identity approves only when its token's azp (authorized-party) claim
// is the edge forward-auth client (SetForwardAuthClients): the browser/phone
// session that renders the escalation's page. An empty or unreadable azp, any
// other client, and an empty list are all refused. In particular the `kd
// login` / `gl` device-grant client (DeviceGrantClientID) never approves: a
// device-code sign-in can be started by any process that can run kd, so an
// agent may hold a person's CLI token, and kd/gl approve hand the person a
// link to the page instead. This replaced the BEADS_ESCALATION_APPROVER_CLIENTS
// allowlist once its only other entry, gardenio-slack-link, was dropped
// (P10.S gd-RMmWllb8PS); the escalation-routing plan's P10.B section says why.
//
// "edge" identities (the gardenio-edge-auth envelope) carry no azp. Only a
// verified v2 envelope approves: EdgeAuthMiddleware stamps it authMethodOIDC
// with its signed roles, and it must carry at least gardenio-operator. A
// verified v1 envelope (authMethodEdge) names any operator its signer likes
// and carries no role, and a burn-in-trusted envelope that did not verify is
// pinned to viewer; neither approves, so neither can satisfy an approvers
// list or a four-eyes check under someone else's name (gd-dMof1PW1oJ).
func (s *BeadsServer) escalationApprover(ctx context.Context) (string, bool) {
	id := AuthIdentityFromContext(ctx)
	if id.Subject == "" {
		return "", false
	}
	switch id.Type {
	case "edge":
		if authMethodFromContext(ctx) == authMethodOIDC && hasMinimumRole(RolesFromContext(ctx), RoleOperator) {
			return id.Subject, true
		}
	case "oidc":
		if forwardAuthClientTrusted(s.forwardAuthClients, id.ClientID) {
			return id.Subject, true
		}
	}
	return "", false
}

// SetForwardAuthClients sets the OAuth client IDs (azp) of the edge
// forward-auth proxy. Only their tokens may take an identity from the
// X-Auth-Request-Email header (when the token has no email claim) or groups
// from X-Auth-Request-Groups; blank entries are dropped and an empty list
// trusts the headers for no token.
func (s *BeadsServer) SetForwardAuthClients(clients []string) {
	m := make(map[string]struct{}, len(clients))
	for _, c := range clients {
		if c = strings.TrimSpace(c); c != "" {
			m[c] = struct{}{}
		}
	}
	s.forwardAuthClients = m
}

// escalationRequiresFourEyes reports whether the escalation bead's policy
// demands requester != approver (correction 3). It is declared as a
// per-bead boolean (requires_four_eyes, config.go type:escalation) rather
// than resolved from a global policy engine — the spawn_policy/scrub_policy
// evaluator that will eventually stamp this automatically is explicit
// future work (gd-sGW5Y1B8sg, epic gd-qbBJGHiGHo phase 2); this task only
// enforces the check once the field is set.
func escalationRequiresFourEyes(b *model.Bead) bool {
	if b == nil || len(b.Fields) == 0 {
		return false
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(b.Fields, &fields); err != nil {
		return false
	}
	raw, ok := fields["requires_four_eyes"]
	if !ok {
		return false
	}
	var v bool
	_ = json.Unmarshal(raw, &v)
	return v
}

// escalationApprovers returns the escalation bead's approvers list (config.go
// type:escalation "approvers" field), the taints ADR §8 team-based approver
// gate: entries are stored as "team:<team bead id>" — never a slug — the
// same convention scrub_policy.approvers already uses (see
// scrubApproverInApprovers, http_scrub_requests.go), so this bead-shaped
// field is a wholly generic escalation-approve control, not spawn-specific.
// An escalation with no approvers key set at all is unrestricted by this
// check (nil, nil) — the field is populated by whatever wrote the escalation
// (today: a manual stamp; eventually the spawn_policy evaluator,
// gd-sGW5Y1B8sg phase 2), and its absence must not retroactively lock out
// approval for every escalation that predates it.
//
// An approvers key that IS present but fails to parse as a []string (e.g. a
// string or object instead of an array) returns a non-nil error rather than
// silently treating the gate as unset: this field exists to restrict who can
// approve, so a value the server cannot understand must deny approval, not
// fail open (AUDIT-2026-04-18 §C3 follow-up).
func escalationApprovers(b *model.Bead) ([]string, error) {
	if b == nil || len(b.Fields) == 0 {
		return nil, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(b.Fields, &fields); err != nil {
		return nil, nil
	}
	raw, ok := fields["approvers"]
	if !ok {
		return nil, nil
	}
	var approvers []string
	if err := json.Unmarshal(raw, &approvers); err != nil {
		return nil, err
	}
	return approvers, nil
}

// escalationFourEyesViolation reports whether approver resolves to the same
// operator as the escalation bead's recorded requester (bead.OperatedBy /
// OperatedByID, stamped from the identity that created the escalation —
// handleEscalation in write_authz.go). Resolution mirrors
// callerIsBeadOperator: a byte-equal fast path, then a canonical
// operators(id) comparison via LookupOperatorAlias so e.g. a direct OIDC
// approver (bare email) correctly matches a requester recorded as
// "slack:U…" once the two identities are linked to one operator row.
//
// An unresolvable side (no alias row for either party) returns false rather
// than blocking: four-eyes exists to stop one human from being both
// requester and approver, and a requester that never resolves to any human
// operator (a bare autonomous agent/fleet identity) cannot BE that human, so
// there is nothing to enforce against.
func (s *BeadsServer) escalationFourEyesViolation(ctx context.Context, bead *model.Bead, approver string) bool {
	if approver == "" || bead == nil {
		return false
	}
	if bead.OperatedBy != "" && bead.OperatedBy == approver {
		return true
	}

	approverKind, approverSubject := splitOperator(approver)
	if approverKind == "" {
		approverKind, approverSubject = "oidc", approver
	}
	approverOp, err := s.store.LookupOperatorAlias(ctx, approverKind, approverSubject)
	if err != nil || approverOp == 0 {
		return false
	}

	requesterOp := s.beadOperatorID(ctx, bead)
	return requesterOp != 0 && requesterOp == approverOp
}

// escalationApproverListed reports whether the directly authenticated caller
// satisfies an approvers list. Entries are "team:<team bead id>" (membership
// via TeamsOfDirect) or a human identity: "oidc:<email>", "slack:<id>", or a
// bare OIDC email. A human entry matches when it names the caller's own
// identity (case-insensitive for oidc) or resolves, through
// LookupOperatorAlias, to the same operator row as the caller. Both
// resolutions use the DIRECT identity only, never the X-Operated-By relay,
// for the reason TeamsOfDirect documents. An empty list is not satisfied: the
// caller decides whether "no approvers" means unrestricted.
//
// A lane ref (escalation-routing D2) is never matched as an identity. The
// create-time route stamp (escalation_policy.go) writes approvers such as
// ["coordinator:merge-desk","operator"]. The approve handler goes through
// humanSatisfiesApprovers (escalation_lane_members.go), which sends lane refs
// to callerInLane and only legacy entries here. For the other callers (the
// spawn_policy rule approvers), the operator lane is satisfied by a
// gardenio-admin on their own sign-in (D2, CONTRACT ISSUE 6) and every other
// lane by no human here. Unparseable entries are matched as identities, as
// before.
func (s *BeadsServer) escalationApproverListed(ctx context.Context, approvedBy string, approvers []string) (bool, error) {
	var humans []string
	hasTeam := false
	for _, a := range approvers {
		if lane, err := parseLaneRef(a); err == nil && lane.Kind != laneLegacy {
			if lane.Kind == laneOperator && callerMayApproveAsAdmin(ctx) {
				return true, nil
			}
			continue
		}
		if strings.HasPrefix(a, "team:") {
			hasTeam = true
		} else if a = strings.TrimSpace(a); a != "" {
			humans = append(humans, a)
		}
	}
	if hasTeam {
		teams, err := s.TeamsOfDirect(ctx)
		if err != nil {
			return false, err
		}
		if scrubApproverInApprovers(teams, approvers) {
			return true, nil
		}
	}
	if len(humans) == 0 {
		return false, nil
	}

	callerKind, callerSubject := operatorAliasForIdentity(AuthIdentityFromContext(ctx))
	if callerKind == "" {
		return false, nil
	}
	var callerOp int64
	if op, err := s.store.LookupOperatorAlias(ctx, callerKind, canonicalAliasSubject(callerKind, callerSubject)); err == nil {
		callerOp = op
	}
	for _, h := range humans {
		kind, subject := splitOperator(h)
		if kind == "" {
			kind, subject = "oidc", h
		}
		subject = canonicalAliasSubject(kind, subject)
		if kind == callerKind && subject == canonicalAliasSubject(callerKind, callerSubject) {
			return true, nil
		}
		if callerOp == 0 {
			continue
		}
		if op, err := s.store.LookupOperatorAlias(ctx, kind, subject); err == nil && op == callerOp {
			return true, nil
		}
	}
	return false, nil
}

// callerMayApproveAsAdmin reports whether the approving caller holds
// gardenio-admin. An OIDC identity must carry the role; auth-disabled mode
// keeps its legacy authority, as requireRole does. A v1 edge envelope carries
// no role and is not an admin (gd-AvSKOUyqRz).
func callerMayApproveAsAdmin(ctx context.Context) bool {
	switch authMethodFromContext(ctx) {
	case authMethodOIDC:
		return hasMinimumRole(RolesFromContext(ctx), RoleAdmin)
	case authMethodNone:
		return true
	}
	return false
}

// escalationSpawnTarget derives, from the escalation's own operation_spec, the
// agent role the replay would grant: an agent create's fields.role, or an
// update's fields.role (an agent re-role). The spawn_role field is NOT
// consulted: it is requester-writable metadata for the inbox, and keying the
// approver gate on it let a hand-filed escalation (no spawn_role, or one
// PATCHed off) skip the gate entirely (gd-4gdUwaWr6i).
//
// isSpawn is false for an escalation that grants no role. When isSpawn is true
// and err is non-nil the role could not be resolved (undecodable spec, role
// missing or not a string) and the caller must refuse: a privileged replay
// whose role is unknown cannot be vetted. The decode mirrors replayEscalation's
// so both see the same role.
func escalationSpawnTarget(f spawnEscalationFields) (role string, isSpawn bool, err error) {
	switch normalizeOperationType(f.OperationType) {
	case "create":
		var spec createBeadInput
		if json.Unmarshal(f.OperationSpec, &spec) != nil {
			return "", true, errors.New("operation_spec is not a decodable create")
		}
		if spec.Type != "agent" {
			return "", false, nil
		}
		role, _, rerr := exactFieldString(spec.Fields, agentRoleField)
		if rerr != nil || strings.TrimSpace(role) == "" {
			return "", true, errors.New("agent create has no resolvable role")
		}
		return role, true, nil
	case "update":
		var spec updateBeadInput
		if json.Unmarshal(f.OperationSpec, &spec) != nil {
			return "", true, errors.New("operation_spec is not a decodable update")
		}
		role, present, rerr := exactFieldString(spec.Fields, agentRoleField)
		if !present && rerr == nil {
			return "", false, nil
		}
		if rerr != nil || strings.TrimSpace(role) == "" {
			return "", true, errors.New("role update has no resolvable role")
		}
		return role, true, nil
	}
	return "", false, nil
}

// spawnApprovalDenial enforces who may approve a privileged-agent-spawn
// escalation, resolved LIVE from the spawn_policy bead and the role beads at
// approval time rather than only from the approvers stamped at filing: an
// escalation is an ordinary bead that its requester can write, so a stamped
// approvers / requires_four_eyes / spawn_role alone could be removed by the
// very operator it is meant to bind (gd-5o6PxGfSa7, gd-4gdUwaWr6i). The role
// comes from operation_spec (escalationSpawnTarget), never the spawn_role
// field.
//
//   - the first-matching human rule names approvers: only those approve (an
//     admin who is not listed is refused, as is the requester's own operator
//     per four-eyes elsewhere);
//   - that rule's four_eyes is enforced here too, not only when stamped;
//   - when the role is privileged (privileged=true or a privileged
//     ownership_grants row on a role bead, or a never-auto role), the approver must ALSO be a
//     gardenio-admin on an uncapped client, whether or not a rule names them:
//     gardenio-operator is not enough to grant cluster-admin, and a token whose
//     client has a role ceiling (gardenio-slack-link) is demoted before this
//     check, so a listed owner approving through Slack is refused.
//
// An escalation granting no role, and a spawn of an unprivileged role under no
// approvers-bearing rule, returns "" (unrestricted here). A spawn whose role
// cannot be resolved is refused; a lookup error fails closed.
func (s *BeadsServer) spawnApprovalDenial(ctx context.Context, bead *model.Bead, approvedBy string) (string, error) {
	f := parseSpawnEscalationFields(bead)
	role, isSpawn, rerr := escalationSpawnTarget(f)
	if !isSpawn {
		return "", nil
	}
	if rerr != nil {
		return "approval refused: the role this escalation would grant cannot be resolved from its operation_spec", nil
	}
	// Resolve privilege first: a privileged grant needs an UNCAPPED admin
	// whatever a spawn_policy rule says. RolesFromContext is already after the
	// client role ceiling (clientRoleCeilingVerifier), so a gardenio-slack-link
	// token is at most an operator here and cannot approve cluster-admin even
	// when the rule names its owner (operator review of PR #374, B1).
	privileged := isNeverAutoApproveRole(role)
	if !privileged {
		var err error
		if privileged, err = roleIsPrivileged(ctx, s.store, role); err != nil {
			return "", err
		}
	}
	if privileged && !callerMayApproveAsAdmin(ctx) {
		return "approving a privileged-role spawn requires gardenio-admin on an uncapped client", nil
	}
	if rule := s.matchingHumanSpawnRule(ctx, role, f.OperationSpec, bead.CreatedBy); rule != nil {
		if rule.FourEyes && s.escalationFourEyesViolation(ctx, bead, approvedBy) {
			return "four-eyes required: approver must differ from the requester", nil
		}
		if len(rule.Approvers) > 0 {
			ok, err := s.escalationApproverListed(ctx, approvedBy, rule.Approvers)
			if err != nil {
				return "", err
			}
			if !ok {
				return "approval requires being one of this spawn_policy rule's approvers", nil
			}
		}
	}
	return "", nil
}

// Server-stamped escalation fields (gd-4gdUwaWr6i). handleEscalation stamps
// spawn_role, approvers and requires_four_eyes from the denial and the live
// spawn_policy, and operation_type/operation_spec/target_bead_id are what the
// approver reviewed; none may be set or changed by a non-admin caller, or the
// requester could write its own approval terms. requesting_agent is immutable
// too (the replay attributes taints to it) but is not in the stamped list: the
// create path has its own stamp, escalation_requesting_agent.go. Listing it
// here closes the field endpoints and /result, which never reach that PATCH
// guard. escalationRouteFields (escalation_lane.go) are server-only too:
// routing (held_by, execute_by, operator_only, policy_rule), approval
// provenance (approved_*, approval_method) and claim/hand-up stamps.
var (
	escalationStampedFields   = append([]string{"spawn_role", "approvers", "requires_four_eyes", "executor_mail_id", escalationRequestingAgentVerifiedField, escalationRequestingAgentProjectField}, escalationRouteFields...)
	escalationImmutableFields = append([]string{"spawn_role", "approvers", "requires_four_eyes", "requesting_agent", "executor_mail_id", escalationRequestingAgentVerifiedField, escalationRequestingAgentProjectField, "operation_type", "operation_spec", "target_bead_id", "execution_target", "target_runner", "target_owner", "requesting_runner", implementsDecisionField}, escalationRouteFields...)
)

type serverStampedEscalationKey struct{}

// withServerStampedEscalation marks ctx as the daemon's own escalation filing
// (handleEscalation), which may carry the stamped fields.
func withServerStampedEscalation(ctx context.Context) context.Context {
	return context.WithValue(ctx, serverStampedEscalationKey{}, true)
}

// escalationFieldsExempt reports whether the caller may write the stamped
// fields: the daemon itself (server-stamped filing, machine replay) or a
// gardenio-admin.
func escalationFieldsExempt(ctx context.Context) bool {
	if v, _ := ctx.Value(escalationRouteKey{}).(bool); v {
		return true // the route endpoint (escalation_audience_authz.go) writes only server-computed fields
	}
	if v, _ := ctx.Value(serverStampedEscalationKey{}).(bool); v {
		return true
	}
	return isMachineWrite(ctx) || callerMayApproveAsAdmin(ctx)
}

// escalationFieldValues returns every key of a blob that matches one of names
// case-insensitively, keyed by its EXACT spelling. The approve path decodes
// operation_spec & co. with encoding/json's case-insensitive struct match, so
// "Operation_Spec" is as live as "operation_spec"; keeping each spelling as its
// own entry means any variant that appears, changes or disappears shows in a
// before/after comparison, with no dependence on map iteration order picking
// one of two colliding variants.
func escalationFieldValues(fields json.RawMessage, names []string) map[string]any {
	out := map[string]any{}
	var m map[string]any
	if len(fields) == 0 || json.Unmarshal(fields, &m) != nil {
		return out
	}
	for k, v := range m {
		for _, n := range names {
			if strings.EqualFold(k, n) {
				out[k] = v
			}
		}
	}
	return out
}

// escalationImmutableFieldKey reports whether key names, in any case, one of
// the approval-relevant escalation fields.
func escalationImmutableFieldKey(key string) bool {
	for _, n := range escalationImmutableFields {
		if strings.EqualFold(key, n) {
			return true
		}
	}
	return false
}

// checkEscalationFieldEndpointWrite refuses a field-level write endpoint
// (/fields/{f}/edit|append|reorder, /increment) that targets an approval-
// relevant field of an escalation, for a non-admin caller. Those endpoints go
// straight to the store and so never reach checkEscalationApprovalFieldsImmutable
// (the PATCH / gRPC update guard).
func checkEscalationFieldEndpointWrite(ctx context.Context, bead *model.Bead, fieldKeys ...string) error {
	if bead == nil || bead.Type != model.TypeEscalation || escalationFieldsExempt(ctx) {
		return nil
	}
	for _, k := range fieldKeys {
		if escalationImmutableFieldKey(k) {
			return inputError(k + " is immutable on an escalation after create")
		}
	}
	return nil
}

// replayWritePolicyDenial vets, at approve time, the write an escalation's
// operation_spec would replay, as if the APPROVER made it. The replay itself
// runs withMachineWrite (the approval is the authorization), so without this
// any gardenio-operator could approve a hand-filed escalation that creates or
// updates an admin-only type (role, spawn_policy, team, user, config,
// credential, project, ...) and so write what their own role cannot (gd-pUyCv4FSv5).
//
// The approver's roles must satisfy the target type's write_policy.min_role.
// create targets spec.type; update and close target the existing bead's type.
// Escalations with no replayable write (merge_window, manual_operator_action,
// grant, or none) have nothing to vet. Auth-disabled mode keeps the legacy
// authority authorizeWriteTitled gives it; a v1 edge envelope carries no role, so it
// cannot approve a replay onto a type with a write_policy (gd-AvSKOUyqRz).
func (s *BeadsServer) replayWritePolicyDenial(ctx context.Context, bead *model.Bead) (string, error) {
	f := parseSpawnEscalationFields(bead)
	var target model.BeadType
	switch normalizeOperationType(f.OperationType) {
	case "create":
		var spec createBeadInput
		if json.Unmarshal(f.OperationSpec, &spec) != nil {
			// Not replayable either; the replay fails on the same decode.
			return "", nil
		}
		target = model.BeadType(spec.Type)
		if canonical, ok := model.TypeAliases[target]; ok {
			target = canonical
		}
	case "update", "close":
		var targetID string
		var raw struct {
			TargetBeadID string `json:"target_bead_id"`
		}
		if json.Unmarshal(bead.Fields, &raw) == nil {
			targetID = raw.TargetBeadID
		}
		if targetID == "" {
			return "", nil
		}
		tb, err := s.store.GetBead(ctx, targetID)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && tb == nil) {
			return "", nil
		}
		if err != nil {
			return "", err
		}
		target = tb.Type
	default:
		return "", nil
	}
	if target == "" {
		return "", nil
	}
	if authMethodFromContext(ctx) == authMethodNone {
		return "", nil
	}
	tc, err := s.resolveTypeConfig(ctx, target)
	if err != nil {
		return "", err
	}
	if tc == nil || tc.WritePolicy == nil || tc.WritePolicy.MinRole == "" {
		return "", nil
	}
	if !hasMinimumRole(RolesFromContext(ctx), tc.WritePolicy.MinRole) {
		return "approving this " + f.OperationType + " of a " + string(target) + " bead requires " + tc.WritePolicy.MinRole + " (the replay would write a type your own role cannot)", nil
	}
	return "", nil
}

// checkEscalationStampedFieldsOnCreate refuses a type=escalation create from
// a non-admin caller that carries a server-stamped field.
func checkEscalationStampedFieldsOnCreate(ctx context.Context, bead *model.Bead) error {
	if escalationFieldsExempt(ctx) {
		return nil
	}
	for k := range escalationFieldValues(bead.Fields, escalationStampedFields) {
		return inputError(k + " is stamped by the server on an escalation and cannot be set by the caller")
	}
	return nil
}

// checkEscalationApprovalFieldsImmutable refuses a PATCH from a non-admin
// caller that adds, changes or removes any approval-relevant field of an
// escalation. bead.Fields holds the POST-merge state, priorFields the state
// before the PATCH, and priorStatus the status before the PATCH.
func checkEscalationApprovalFieldsImmutable(ctx context.Context, bead *model.Bead, priorFields json.RawMessage, priorStatus model.Status) error {
	if bead.Type != model.TypeEscalation || escalationFieldsExempt(ctx) {
		return nil
	}
	prior := escalationFieldValues(priorFields, escalationImmutableFields)
	current := escalationFieldValues(bead.Fields, escalationImmutableFields)
	if !reflect.DeepEqual(prior, current) && !filerMayAmendMergeSpec(ctx, bead, priorStatus, prior, current) {
		return inputError("approval and routing fields (spawn_role, approvers, requires_four_eyes, requesting_agent, operation_type, operation_spec, target_bead_id, held_by, held_at, execute_by, operator_only, policy_rule, approved_*, approval_method, claimed_*, handed_up_*, hand_up_reason, executor_mail_id, execution_target, target_runner, target_owner, requesting_runner, implements_decision) are immutable on an escalation after create")
	}
	return nil
}

// filerAmendableSpecTypes are the operation types whose operation_spec the
// filer may amend before approval. Only the merge types: their spec names the
// PR the desk merges, and the desk reads that PR live when it acts, so no
// replayed write, spawn role or close grant hangs off it.
var filerAmendableSpecTypes = map[string]bool{"gitlab_mr_merge": true, "github_pr_merge": true}

// filerMayAmendMergeSpec reports whether a PATCH whose only change to the
// immutable fields is operation_spec (exact spelling) is the filer correcting
// an open, unclaimed, never-handed-up, unapproved merge escalation
// (gd-3VaIxgZfDp). Filers named the wrong PR (gb escalation create took the
// requesting task's last-linked PR, #966 for an escalation about #964) and,
// with the spec immutable, could only withdraw and refile. Everyone else, and
// the filer once the escalation is claimed, handed up, approved or no longer
// open (before or after this PATCH), stays refused.
func filerMayAmendMergeSpec(ctx context.Context, bead *model.Bead, priorStatus model.Status, prior, current map[string]any) bool {
	if bead == nil || bead.Status != model.StatusOpen || priorStatus != model.StatusOpen {
		return false
	}
	if len(prior) != len(current) {
		return false
	}
	for k, v := range current {
		pv, ok := prior[k]
		if !ok {
			return false
		}
		if k != "operation_spec" && !reflect.DeepEqual(pv, v) {
			return false
		}
	}
	if _, ok := current["operation_spec"]; !ok {
		return false
	}
	f := parseFieldsToStringMap(bead.Fields)
	if !filerAmendableSpecTypes[f["operation_type"]] || f["spawn_role"] != "" || f["claimed_by"] != "" || f["claimed_at"] != "" {
		return false
	}
	// A hand-up clears claimed_by (applyHandUp) after the desk reviewed the
	// spec, and the holder above approves what that review described, so a
	// handed-up escalation stays fixed even though it reads unclaimed again.
	if f["handed_up_by"] != "" || f["handed_up_at"] != "" || f["hand_up_reason"] != "" {
		return false
	}
	if escalationHasServerApproval(bead) {
		return false
	}
	return callerIsEscalationFiler(ctx, bead, f)
}

// callerIsEscalationFiler reports whether the caller's credential, never a
// header, identifies the escalation's filer. The static token is refused
// outright: every agent pod holds it, and its subject is the X-Actor header,
// which any pod can set to another crew's name. A per-agent token must be the
// requesting_agent the create guard stamped from that same kind of credential
// (requesting_agent_verified). Other credential-bearing identities (an OIDC
// human, the edge envelope, a machine client, a service fleet token) must be
// the escalation's created_by.
func callerIsEscalationFiler(ctx context.Context, bead *model.Bead, f map[string]string) bool {
	if agentID, ok := agentBeadIDFromContext(ctx); ok {
		return f[escalationRequestingAgentVerifiedField] == "true" && f["requesting_agent"] == agentID
	}
	id := AuthIdentityFromContext(ctx)
	switch id.Type {
	case "oidc", "edge", "machine", "fleet":
		return id.Subject != "" && id.Subject == bead.CreatedBy
	}
	return false
}
