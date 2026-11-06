package server

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"reflect"
	"strings"
	"time"

	"gardenio/kbeads/internal/model"
)

// Escalation audience (gd-bmOiteQi36): the audience field says who an
// escalation is for (owner | operator | desk), which the owner's surfaces and
// the slack bridge read to decide what to show in full and whom to notify.
//
// The daemon derives it at create (stampEscalationAudience) and it is then
// frozen: no generic write (PATCH, gRPC UpdateBead, the field endpoints,
// /result) may change audience, sensitivity or credential_requirements, the
// last two because the forced-owner rule reads them (#756 send-back). The one
// way to move an escalation is POST /v1/escalations/{id}/audience, for a
// gardenio-operator or admin on their own sign-in, which refuses to move a
// forced escalation off the owner. An agent cannot move its own escalation out
// of the owner's sight.
//
// audience never gates approval: handleApproveEscalation and its helpers
// (escalation_approval_authz.go) do not read it, and
// TestEscalationApprovalIgnoresAudience proves an approval comes out the same
// whatever the audience. The read side, every surface's
// gardenio/controller/internal/beadsapi/escalation_audience.go, applies the
// same defaults and forced-owner rule to escalations filed before the stamp.

const (
	escalationAudienceField = "audience"
	audienceOwner           = "owner"
	audienceOperator        = "operator"
	audienceDesk            = "desk"
)

// escalationFrozenFields are fixed when an escalation is filed: audience, and
// the two fields the forced-owner rule reads.
//
// held_by, held_at, execute_by and operator_only are frozen too
// (escalation-routing D11): audience is derived from held_by, so only the
// daemon and the route endpoint may move either. Not even an admin PATCH.
// approved_targets is frozen once the approve paths stamp it (Task 15): it is
// the write reach of the escalation's executor.
var escalationFrozenFields = []string{escalationAudienceField, "sensitivity", "credential_requirements", "held_by", "held_at", "execute_by", "operator_only", approvedTargetsField, operatorClassField}

func validEscalationAudience(a string) bool {
	return a == audienceOwner || a == audienceOperator || a == audienceDesk
}

// defaultEscalationAudience mirrors beadsapi.DefaultEscalationAudience: desk
// for a PR merge, operator for everything else.
func defaultEscalationAudience(operationType string) string {
	if isDeskMergeOperation(operationType) {
		return audienceDesk
	}
	return audienceOperator
}

// isDeskMergeOperation reports whether op is a PR merge, every spelling the
// merge desk handles (beadsapi.deskOperationTypes).
func isDeskMergeOperation(op string) bool {
	switch strings.ToLower(strings.TrimSpace(op)) {
	case "gitlab_mr_merge", "github_pr_merge", "gitlab_merge":
		return true
	}
	return false
}

// operatorClassField is the merge desk's OPERATOR-ONLY class (1)-(3)
// (docs/merge-desk.md, "Sensitive paths") on a PR merge, stamped only by the
// hand-up verb (handleHandUpEscalation). It puts the merge on the owner floor
// with operatorClassMergeReason (P10.F4, gd-QiDj0JvkFr): the desk has ruled
// that only the owner may merge it, so it is an owner item, not a desk one.
const (
	operatorClassField       = "operator_class"
	operatorClassMergeReason = "operator_class_merge"
)

// validOperatorClass reports whether c names one of the desk's OPERATOR-ONLY
// classes.
func validOperatorClass(c string) bool { return c == "1" || c == "2" || c == "3" }

// escalationFloorOperationTypes are D3's operator-only operation types, the
// owner floor (owner ruling gd-CJUlVSMzEe, 11-04; P10.F gd-JJpjOjyvIL): only
// these reach the owner, everything else is approved by a policy or
// coordinator lane. Five categories, registered types only; an alias of one is
// on the floor through canonicalOperationType. Code, not config.
// beadsapi.escalationFloorOperationTypes is the read-side copy, aliases
// spelled out; TestEscalationFloorMatchesReadSide keeps the two equal.
var escalationFloorOperationTypes = map[string]bool{
	// Secrets and credentials.
	"credential_rotation": true, "credential_provisioning": true, "secret_write": true, "k8s_secret_seed": true,
	"litellm_key_rotation": true, "litellm_virtual_key_mount": true, "vault_provisioning": true,
	"project_secrets_and_credentials": true, "helm_values_secret_change": true, "aws_ses_smtp_k8s_secret_deploy_values": true,
	// Global roles and identity: who may do what, as whom.
	"grant": true, "kbeads_project_access": true, "kbeads_role_grant": true, "capability_grant": true,
	"kbeads_fleet_token_admin": true, "agent_token_scope": true, "role_write": true, "kbeads_role_update": true,
	"create_role": true, "role_scaffold": true, "k8s_rbac": true, "k8s_rbac_grant": true, "aws_iam": true,
	// keycloak_group_membership is on the floor except for a tenant land's own
	// groups (escalationLandPeople, P10.C gd-wi44L8Qy4I), which
	// escalationForcedReason lets off.
	"keycloak_client": true, "keycloak_idp_rollout": true, "kcadm_write": true, "keycloak_group_membership": true,
	"gardenio_user_capability": true, "user_identity_pin": true, "ziti_identity_management": true,
	"github_token_scope": true, "github_app_permissions": true, "slack_app_scope": true,
	// gardenio's own policy: approval, spawn and authz rules, the repo's merge
	// rules, and the sealed prompts that tell privileged agents what to do.
	"escalation_policy_write": true, "spawn_policy_write": true, "kbeads_authz_change": true,
	"github_repo_settings": true, "github_ruleset_bypass": true, "trigger_reseal": true, "trigger_prompt_install": true,
	// Money and billing.
	"billing_change": true, "spend_limit_change": true,
	// Irreversible data deletion, and its mirror: publishing farm content
	// outside the farm, which cannot be taken back either (P10.F4, gd-QiDj0JvkFr).
	"data_deletion": true, "terraform_destroy": true, "db_data_delete": true, "storage_delete": true,
	"git_push_public": true,
}

// escalationFloorPrefixes put a type that is not registered on the floor by
// its spelling. beadsapi.escalationFloorPrefixes is the read-side copy.
var escalationFloorPrefixes = []string{"credential_", "secret_", "billing_"}

// escalationFloorOperation returns the normalized operation type when it is a
// D3 floor type, an alias of one or a floor-prefixed spelling, else "". Task
// 11's steward delegation refuses these.
func escalationFloorOperation(opType string) string {
	op := normalizeOperationType(opType)
	if escalationFloorOperationTypes[canonicalOperationType(op)] {
		return op
	}
	for _, p := range escalationFloorPrefixes {
		if strings.HasPrefix(op, p) {
			return op
		}
	}
	return ""
}

// escalationFloorReason is the floor part of escalationForcedReason, on decoded
// fields: sensitivity_secret, credential_requirements, the operation type, or
// a PR merge the desk named an operator class (operatorClassMergeReason).
func escalationFloorReason(f map[string]any) string {
	if sens, _ := f["sensitivity"].(string); strings.EqualFold(strings.TrimSpace(sens), "secret") {
		return "sensitivity_secret"
	}
	if credentialRequirementsSet(f["credential_requirements"]) {
		return "credential_requirements"
	}
	op, _ := f["operation_type"].(string)
	if r := escalationFloorOperation(op); r != "" {
		return r
	}
	if c, _ := f[operatorClassField].(string); isDeskMergeOperation(op) && strings.TrimSpace(c) != "" {
		return operatorClassMergeReason
	}
	return ""
}

// isEscalationFloorReason reports whether a forced reason makes the escalation
// operator_only with approvers=["operator"] (D3): one escalationFloorReason
// returns. The other reasons (a named person, a spawn) keep a server-stamped
// approvers list. A positive list, so spawn_policy_write is a floor reason
// while spawn_privileged is not.
func isEscalationFloorReason(reason string) bool {
	switch reason {
	case "":
		return false
	case "sensitivity_secret", "credential_requirements", operatorClassMergeReason:
		return true
	}
	return escalationFloorOperation(reason) == reason
}

// escalationForcedReason is the one floor (escalation-routing D3): why an
// escalation must reach the owner whatever its audience says, or "". It is
// #756's forced-owner rule plus D3's operation types: it is secret, needs
// credentials or is a floor operation type (isEscalationFloorReason; a
// tenant land's own people are not, escalationLandPeople, P10.C), its
// approvers name a person, or it grants an agent role that only an admin, or a
// person a spawn_policy rule names, can approve. A named approver or an
// admin-only approval is the owner's to act on, so folding it into "in hand"
// would leave it to stall. The spawn is read the way the approval gate reads
// it (spawnApprovalDenial): the role from operation_spec, never the spawn_role
// field, so a hand-filed spawn escalation is caught too. An unresolvable role
// or a lookup error fails toward the owner. escalationAudienceForced,
// stampEscalationAudience, the route endpoint and escalationRouteFieldsFor all
// read it.
func (s *BeadsServer) escalationForcedReason(ctx context.Context, bead *model.Bead) string {
	var f map[string]any
	if bead == nil || len(bead.Fields) == 0 || json.Unmarshal(bead.Fields, &f) != nil {
		return ""
	}
	if r := escalationFloorReason(f); r != "" && (r != landPeopleOperation || s.escalationLandPeople(ctx, bead) == "") {
		return r
	}
	if approversNamePerson(f["approvers"]) {
		return "approvers_name_person"
	}
	sf := parseSpawnEscalationFields(bead)
	role, isSpawn, err := escalationSpawnTarget(sf)
	switch {
	case !isSpawn:
		return ""
	case err != nil:
		return "spawn_unresolvable"
	case isNeverAutoApproveRole(role):
		return "spawn_never_auto_approve"
	}
	if privileged, err := roleIsPrivileged(ctx, s.store, role); err != nil || privileged {
		return "spawn_privileged"
	}
	if rule := s.matchingHumanSpawnRule(ctx, role, sf.OperationSpec, bead.CreatedBy); rule != nil {
		for _, a := range rule.Approvers {
			if approversNamePerson([]any{a}) {
				return "spawn_rule_names_person"
			}
		}
	}
	return ""
}

// escalationAudienceForced is #756's predicate, kept for its callers.
func (s *BeadsServer) escalationAudienceForced(ctx context.Context, bead *model.Bead) bool {
	return s.escalationForcedReason(ctx, bead) != ""
}

// credentialRequirementsSet mirrors beadsapi's rule on a decoded value:
// null, "", [] and {} ask for nothing; anything else asks, including a string
// that does not parse as JSON.
func credentialRequirementsSet(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case string:
		t = strings.TrimSpace(t)
		if t == "" {
			return false
		}
		var inner any
		if json.Unmarshal([]byte(t), &inner) != nil {
			return true
		}
		if s, ok := inner.(string); ok {
			return strings.TrimSpace(s) != ""
		}
		return credentialRequirementsSet(inner)
	case []any:
		return len(t) > 0
	case map[string]any:
		return len(t) > 0
	}
	return true
}

// approversNamePerson reports whether an approvers value names a person, the
// spawn_policy rule's named human approvers: an isLegacyPersonApprover entry.
// Lane refs and team: entries are not people.
func approversNamePerson(v any) bool {
	var list []any
	switch t := v.(type) {
	case []any:
		list = t
	case []string:
		for _, e := range t {
			list = append(list, e)
		}
	case string:
		if json.Unmarshal([]byte(t), &list) != nil {
			list = []any{t}
		}
	}
	for _, e := range list {
		if a, _ := e.(string); isLegacyPersonApprover(a) {
			return true
		}
	}
	return false
}

// stampEscalationAudience derives an escalation's audience at create, on every
// transport (createBead): owner when the forced-owner rule applies or the
// filer asked for owner, else the default for its operation_type. A filer may
// raise an escalation to the owner but not pick operator or desk, so what the
// owner sees is the daemon's call, not the agent's.
func (s *BeadsServer) stampEscalationAudience(ctx context.Context, bead *model.Bead) error {
	var f map[string]any
	if len(bead.Fields) > 0 && json.Unmarshal(bead.Fields, &f) != nil {
		return nil
	}
	// Schema validation has already refused a case-folded spelling of the key.
	asked, _ := f[escalationAudienceField].(string)
	asked = strings.ToLower(strings.TrimSpace(asked))
	if f[escalationAudienceField] != nil && !validEscalationAudience(asked) {
		return inputError("audience must be one of owner, operator, desk")
	}
	op, _ := f["operation_type"].(string)
	audience := defaultEscalationAudience(op)
	if asked == audienceOwner || s.escalationAudienceForced(ctx, bead) {
		audience = audienceOwner
	}
	bead.Fields = setFieldValue(bead.Fields, escalationAudienceField, audience)
	return nil
}

// escalationFrozenFieldsExempt: only the daemon itself (a server-stamped
// filing, an approved replay, its own machine writes) or the audience route
// writes a frozen field after create. Not an admin's generic PATCH either:
// an admin re-routes through the route endpoint like an operator.
func escalationFrozenFieldsExempt(ctx context.Context) bool {
	if v, _ := ctx.Value(serverStampedEscalationKey{}).(bool); v {
		return true
	}
	if v, _ := ctx.Value(escalationRouteKey{}).(bool); v {
		return true
	}
	return isMachineWrite(ctx)
}

type escalationRouteKey struct{}

const escalationFrozenDenied = "audience, held_by, execute_by, operator_only, sensitivity and credential_requirements are fixed when an escalation is filed, and approved_targets when it is first approved; re-route with POST /v1/escalations/{id}/audience (gb escalation route), which needs gardenio-operator or gardenio-admin on your own sign-in"

// checkEscalationFrozenFields refuses an update (PATCH or gRPC UpdateBead,
// which both reach updateBead) that adds, changes or removes a frozen field.
// bead.Fields holds the POST-merge state, priorFields the state before.
func checkEscalationFrozenFields(ctx context.Context, bead *model.Bead, priorFields []byte) error {
	if bead == nil || bead.Type != model.TypeEscalation || escalationFrozenFieldsExempt(ctx) {
		return nil
	}
	if !reflect.DeepEqual(escalationFieldValues(priorFields, escalationFrozenFields), escalationFieldValues(bead.Fields, escalationFrozenFields)) {
		return inputError(escalationFrozenDenied)
	}
	return nil
}

// checkEscalationFrozenFieldEndpoint is checkEscalationFrozenFields for the
// field-level write endpoints (/fields/{f}/edit|append|reorder, /increment)
// and /result, which go straight to the store.
func checkEscalationFrozenFieldEndpoint(ctx context.Context, bead *model.Bead, fieldKeys ...string) error {
	if bead == nil || bead.Type != model.TypeEscalation || escalationFrozenFieldsExempt(ctx) {
		return nil
	}
	for _, k := range fieldKeys {
		for _, n := range escalationFrozenFields {
			if strings.EqualFold(k, n) {
				return inputError(escalationFrozenDenied)
			}
		}
	}
	return nil
}

// callerMayRouteEscalation reports whether the caller may re-route an
// escalation: a gardenio-operator or admin on a role-bearing human identity
// (a browser session or a verified v2 edge envelope, both authMethodOIDC),
// the daemon's own machine writes, and auth-disabled mode. A v1 edge envelope
// carries no role and may not (#756 send-back); nor may a static/fleet token
// (every agent pod holds one) or a land principal (whose roles are the
// tenant's own).
func callerMayRouteEscalation(ctx context.Context) bool {
	if isMachineWrite(ctx) {
		return true
	}
	switch authMethodFromContext(ctx) {
	case authMethodNone:
		return true
	case authMethodOIDC:
		return hasMinimumRole(RolesFromContext(ctx), RoleOperator)
	}
	return false
}

const escalationRouteDenied = "re-routing an escalation (its audience) requires gardenio-operator or gardenio-admin on your own sign-in; an agent token may not"

// routeEscalationRequest is the JSON body for POST /v1/escalations/{id}/audience.
type routeEscalationRequest struct {
	Audience string `json:"audience"`
	Reason   string `json:"reason"`
}

// handleRouteEscalation handles POST /v1/escalations/{id}/audience: set an
// open escalation's audience and record who did it as a comment. An empty
// audience stamps the derived one (backfill of escalations filed before the
// create stamp). Refused to anyone callerMayRouteEscalation refuses, and for a
// forced escalation to any audience but owner. A no-change route writes
// nothing.
func (s *BeadsServer) handleRouteEscalation(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "id is required")
		return
	}
	var req routeEscalationRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	to := strings.ToLower(strings.TrimSpace(req.Audience))
	if to != "" && !validEscalationAudience(to) {
		writeError(w, http.StatusBadRequest, "audience must be one of owner, operator, desk (or empty, to stamp the derived one)")
		return
	}
	ctx := r.Context()
	if !callerMayRouteEscalation(ctx) {
		writeError(w, http.StatusForbidden, escalationRouteDenied)
		return
	}
	bead, ok := s.authorizeResolutionTarget(w, r, id, "escalation", "handleRouteEscalation")
	if !ok {
		return
	}
	if bead.Status == model.StatusClosed {
		writeError(w, http.StatusConflict, "escalation is closed")
		return
	}
	var f map[string]any
	_ = json.Unmarshal(bead.Fields, &f)
	from, _ := f[escalationAudienceField].(string)
	forced := s.escalationAudienceForced(ctx, bead)
	if to == "" {
		op, _ := f["operation_type"].(string)
		switch {
		case forced:
			to = audienceOwner
		case validEscalationAudience(from):
			to = from
		default:
			to = defaultEscalationAudience(op)
		}
	}
	if to != audienceOwner && forced {
		writeError(w, http.StatusConflict, "this escalation needs credentials, is secret, names an approver or needs an admin, so it always goes to the owner")
		return
	}
	// D11 rule 3: a stamped escalation moves held_by to a lane of the target
	// audience in the same write. An unstamped one keeps #756's behaviour
	// (audience only) and the sweep picks its lane (D11 rule 4).
	held, _ := f["held_by"].(string)
	held = strings.TrimSpace(held)
	lane := held
	if held != "" {
		if escalationHasServerApproval(bead) && to != audienceOwner {
			writeError(w, http.StatusConflict, "this escalation is approved and held by its executor; it can only be routed to the owner")
			return
		}
		p, err := s.activeEscalationPolicy(ctx)
		if err != nil {
			slog.Warn("escalation route: policy unreadable; refusing", "bead", id, "error", err)
			writeError(w, http.StatusServiceUnavailable, "could not read the escalation policy; try again")
			return
		}
		if laneAudience(held, p) != to {
			if lane, err = s.laneForAudience(ctx, bead, to, p); err != nil {
				writeError(w, http.StatusConflict, err.Error())
				return
			}
		}
	}
	if to == from && lane == held {
		writeJSON(w, http.StatusOK, map[string]any{"escalation": extractEscalationFields(bead), "issue": bead})
		return
	}
	if from == "" {
		from = "(unset)"
	}
	patch := map[string]any{escalationAudienceField: to}
	if lane != held {
		patch["held_by"], patch["held_at"] = lane, time.Now().UTC().Format(time.RFC3339)
		patch["claimed_by"], patch["claimed_at"] = "", ""
	}
	fields, _ := json.Marshal(patch)
	updated, err := s.updateBead(context.WithValue(ctx, escalationRouteKey{}, true), id, updateBeadInput{Fields: fields})
	if err != nil {
		var denial *AuthzDenial
		var ie inputError
		switch {
		case errors.As(err, &denial):
			s.auditDenial(r, denial, id)
			writeAuthzDenial(w, denial)
		case errors.As(err, &ie):
			writeError(w, http.StatusBadRequest, ie.Error())
		default:
			writeServerError(ctx, w, "handleRouteEscalation", err, "failed to re-route escalation")
		}
		return
	}
	who := actorFromContext(ctx)
	if who == "" {
		who = "unknown"
	}
	note := "Audience re-routed: " + from + " → " + to
	if lane != held {
		note += " (" + lane + ")"
	}
	note += " by " + who
	if reason := strings.TrimSpace(req.Reason); reason != "" {
		note += ": " + reason
	}
	if err := s.store.AddComment(ctx, &model.Comment{BeadID: id, Author: who, Text: note, CreatedAt: time.Now().UTC()}); err != nil {
		slog.Warn("escalation re-routed but the audit comment failed", "bead", id, "error", err)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"escalation": extractEscalationFields(updated),
		"issue":      updated,
	})
}
