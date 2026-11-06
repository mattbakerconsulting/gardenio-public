package beadsapi

import (
	"encoding/json"
	"strconv"
	"strings"
)

// Escalation audience (gd-bmOiteQi36): who an open escalation is FOR, which
// decides where it is shown and who is notified, never who may approve it
// (kbeads' approve authz does not read it). The owner sees owner-audience
// escalations as full cards and is pinged for them; desk and operator ones are
// "in hand": the merge desk and the operators work them in their own channels,
// and the owner's surfaces fold them into one line.
//
// Every surface (gb, the Lands Commons, beads-viewer, the slack bridge) reads
// an escalation's audience through EscalationAudience, so the defaults and the
// forced-owner rule live here once.
const (
	AudienceOwner    = "owner"    // product or policy choices, owner-only actions, money, credentials
	AudienceOperator = "operator" // infra: helm, RBAC, deploy values, AWS, Keycloak, live checks
	AudienceDesk     = "desk"     // the merge desk: PR merges under the merge policy
)

// Audiences are the valid audience values, owner first.
var Audiences = []string{AudienceOwner, AudienceOperator, AudienceDesk}

// ValidAudience reports whether a is one of Audiences.
func ValidAudience(a string) bool {
	switch a {
	case AudienceOwner, AudienceOperator, AudienceDesk:
		return true
	}
	return false
}

// deskOperationTypes are the operation types the merge desk handles. Agents
// file GitHub PR merges under gitlab_mr_merge (the name predates the GitHub
// move); the other spellings appear in older escalations.
var deskOperationTypes = map[string]bool{
	"gitlab_mr_merge": true,
	"github_pr_merge": true,
	"gitlab_merge":    true,
}

// DefaultEscalationAudience is the audience an escalation of operationType
// gets when its filer names none: desk for a merge, operator for everything
// else, unknown types included. No operation type defaults to owner: the
// filer says --audience owner, or the forced-owner rule applies.
func DefaultEscalationAudience(operationType string) string {
	if deskOperationTypes[strings.ToLower(strings.TrimSpace(operationType))] {
		return AudienceDesk
	}
	return AudienceOperator
}

// EscalationAudienceForced reports whether an escalation must reach the owner
// whatever its audience field says: it needs credentials
// (credential_requirements is non-empty), its sensitivity is secret, or its
// approvers name a person (only they can approve it, so folding it away would
// leave it to stall). Hiding those from the owner must be impossible, so no
// filer choice or re-route overrides it. kbeads applies the same rule when it
// stamps audience at create (escalation_audience_authz.go), plus what this side
// cannot see: an agent spawn whose role (read from operation_spec) only an
// admin, or a person a spawn_policy rule names, can approve; and it freezes
// sensitivity and credential_requirements after filing so the rule cannot be
// undone.
//
// It also reads the escalation-routing D3 floor: operator_only=true (kbeads
// stamps it on a floor escalation) and the floor operation types, so an
// escalation filed before the route stamp reads the same, and a PR merge the
// merge desk named an operator class (operator_class, kbeads'
// operatorClassMergeReason, P10.F4 gd-QiDj0JvkFr).
func EscalationAudienceForced(fields map[string]string) bool {
	if strings.EqualFold(strings.TrimSpace(fields["sensitivity"]), "secret") || strings.TrimSpace(fields["operator_only"]) == "true" {
		return true
	}
	if escalationFloorOperation(fields["operation_type"]) {
		return true
	}
	if deskOperationTypes[strings.ToLower(strings.TrimSpace(fields["operation_type"]))] && strings.TrimSpace(fields["operator_class"]) != "" {
		return true
	}
	return credentialRequirementsSet(fields["credential_requirements"]) || approversNamePerson(fields["approvers"])
}

// escalationFloorOperationTypes mirrors kbeads' owner floor
// (escalation_audience_authz.go escalationFloorOperationTypes, P10.F
// gd-JJpjOjyvIL): secrets and credentials, global roles and identity,
// gardenio's own policy, money and billing, irreversible data deletion and
// publishing outside the farm. kbeads
// resolves an alias to its type; this copy spells the aliases out.
// kbeads' TestEscalationFloorMatchesReadSide parses this map and
// escalationFloorPrefixes, so the two cannot drift.
var escalationFloorOperationTypes = map[string]bool{
	// Secrets and credentials.
	"credential_rotation": true, "credential_provisioning": true, "secret_write": true, "k8s_secret_seed": true,
	"litellm_key_rotation": true, "litellm_virtual_key_mount": true, "vault_provisioning": true,
	"project_secrets_and_credentials": true, "helm_values_secret_change": true, "aws_ses_smtp_k8s_secret_deploy_values": true,
	"credential_provision": true, "secret_provision": true, "k8s_secret_create": true,
	// Global roles and identity.
	"grant": true, "kbeads_project_access": true, "kbeads_role_grant": true, "capability_grant": true,
	"kbeads_fleet_token_admin": true, "agent_token_scope": true, "role_write": true, "kbeads_role_update": true,
	"create_role": true, "role_scaffold": true, "k8s_rbac": true, "k8s_rbac_grant": true, "aws_iam": true,
	"keycloak_client": true, "keycloak_idp_rollout": true, "kcadm_write": true, "keycloak_group_membership": true,
	"gardenio_user_capability": true, "user_identity_pin": true, "ziti_identity_management": true,
	"github_token_scope": true, "github_app_permissions": true, "slack_app_scope": true,
	"kbeads_bead_access": true, "beads_project_access": true, "agent_bead_access": true, "kbeads_machine_grant": true,
	"fleet_token_audit": true, "agent_token_scope_fix": true, "role_bead_update": true, "kd_update_role": true,
	"aws_ses_iam_dns": true, "keycloak_admin_permissions": true, "github_permissions": true, "github_pull_request": true,
	"github_pr": true, "github_app_registration": true, "github_app_config": true, "github_app_settings": true,
	// gardenio's own policy.
	"escalation_policy_write": true, "spawn_policy_write": true, "kbeads_authz_change": true,
	"github_repo_settings": true, "github_ruleset_bypass": true, "trigger_reseal": true, "trigger_prompt_install": true,
	"kbeads_authz_policy": true, "beads_rbac": true, "gardener_cadence_reseal": true,
	"trigger_prompt_reseal": true, "trigger_prompt_install_reseal": true, "trigger_prompt_amend": true,
	// Money and billing.
	"billing_change": true, "spend_limit_change": true, "budget_change": true, "litellm_budget_change": true,
	// Irreversible data deletion, and publishing outside the farm.
	"data_deletion": true, "terraform_destroy": true, "db_data_delete": true, "storage_delete": true,
	"data_delete": true, "bucket_delete": true, "volume_delete": true, "pvc_delete": true, "s3_delete": true,
	"git_push_public": true,
}

// escalationFloorPrefixes mirrors kbeads' escalationFloorPrefixes.
var escalationFloorPrefixes = []string{"credential_", "secret_", "billing_"}

// escalationFloorOperation reports whether op is on the owner floor.
func escalationFloorOperation(op string) bool {
	op = strings.ToLower(strings.TrimSpace(op))
	if escalationFloorOperationTypes[op] {
		return true
	}
	for _, p := range escalationFloorPrefixes {
		if strings.HasPrefix(op, p) {
			return true
		}
	}
	return false
}

// approversNamePerson reports whether an approvers value (a JSON list) names a
// person: an isLegacyPersonApprover entry. Lane refs and team: are not people.
func approversNamePerson(raw string) bool {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return false
	}
	var list []string
	if json.Unmarshal([]byte(raw), &list) != nil {
		list = []string{raw}
	}
	for _, a := range list {
		if isLegacyPersonApprover(a) {
			return true
		}
	}
	return false
}

// isLegacyPersonApprover mirrors kbeads (escalation_lane.go): oidc:, slack: or
// a bare email is a person; operator, coordinator:, steward:, land-owner:,
// requester, server and team: are not.
func isLegacyPersonApprover(entry string) bool {
	e := strings.TrimSpace(entry)
	switch {
	case strings.HasPrefix(e, "oidc:") && len(e) > len("oidc:"), strings.HasPrefix(e, "slack:") && len(e) > len("slack:"):
		return true
	case strings.Contains(e, "@") && !strings.ContainsAny(e, " :"):
		return true
	}
	return false
}

// credentialRequirementsSet reports whether a credential_requirements value
// asks for anything. Empty, null, [] and {} ask for nothing; any other value,
// including one that does not parse, counts as asking (fail toward the owner).
func credentialRequirementsSet(raw string) bool {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return false
	}
	var v any
	if json.Unmarshal([]byte(raw), &v) != nil {
		return true
	}
	switch t := v.(type) {
	case nil:
		return false
	case []any:
		return len(t) > 0
	case map[string]any:
		return len(t) > 0
	case string:
		return strings.TrimSpace(t) != ""
	}
	return true
}

// EscalationAudience is an escalation's effective audience, read off its
// fields: owner when the forced-owner rule applies, else the audience field
// when it holds a valid value, else the default for its operation_type.
func EscalationAudience(fields map[string]string) string {
	if EscalationAudienceForced(fields) {
		return AudienceOwner
	}
	if a := strings.ToLower(strings.TrimSpace(fields["audience"])); ValidAudience(a) {
		return a
	}
	return DefaultEscalationAudience(fields["operation_type"])
}

// EscalationForOwner reports whether an escalation's effective audience is the
// owner: the owner's surfaces show it in full and the owner is notified.
func EscalationForOwner(fields map[string]string) bool {
	return EscalationAudience(fields) == AudienceOwner
}

// InHand counts escalations that are not for the owner, by audience, for the
// folded "In hand: N (desk X, operator Y, land owners Z)" line.
type InHand struct {
	Desk     int
	Operator int
	// Lands counts owner-audience escalations held by a land's owner that the
	// viewer does not own (escalation-routing D9): theirs, not this viewer's.
	Lands int
}

// AddLand counts one escalation another land's owner holds.
func (h *InHand) AddLand() { h.Lands++ }

// Add counts one escalation of audience a; owner-audience ones are not in hand.
func (h *InHand) Add(a string) {
	switch a {
	case AudienceDesk:
		h.Desk++
	case AudienceOperator:
		h.Operator++
	}
}

// Total is how many escalations are in hand.
func (h InHand) Total() int { return h.Desk + h.Operator + h.Lands }

// Line is the folded line's words, e.g. "In hand: 19 (desk 17, operator 2)",
// or "" when nothing is in hand.
func (h InHand) Line() string {
	if h.Total() == 0 {
		return ""
	}
	var parts []string
	if h.Desk > 0 {
		parts = append(parts, "desk "+strconv.Itoa(h.Desk))
	}
	if h.Operator > 0 {
		parts = append(parts, "operator "+strconv.Itoa(h.Operator))
	}
	if h.Lands > 0 {
		parts = append(parts, "land owners "+strconv.Itoa(h.Lands))
	}
	return "In hand: " + strconv.Itoa(h.Total()) + " (" + strings.Join(parts, ", ") + ")"
}
