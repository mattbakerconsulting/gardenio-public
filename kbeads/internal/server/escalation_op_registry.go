package server

import (
	"context"
	"log/slog"
	"os"
	"slices"
	"strings"

	"github.com/prometheus/client_golang/prometheus"

	"gardenio/kbeads/internal/model"
)

// The operation_type registry (agent-access programme P7.4, gd-OXdyPW15fC):
// the closed set of escalation operation types, folded into classes that
// escalation_policy rules match on (when.operation_classes).
//
// Types are accepted as filed. An alias is a spelling of one of its class's
// types; create rewrites it to that type. An alias never changes what the
// code does with an escalation: it and its type agree on every set that
// decides what may happen to it (TestOperationRegistryAliasesKeepSemantics),
// and create keeps the filed spelling when the active policy would route the
// type by another rule, so a rewrite never changes who approves. A spelling
// that differs in either way is a type of its class, not an alias.

type operationClass struct {
	Name    string
	Types   []string
	Aliases map[string]string // alias -> one of Types
}

var operationClasses = []operationClass{
	{Name: "merge", Types: []string{"github_pr_merge", "gitlab_mr_merge", "github_mr_merge", "gitlab_merge"}},
	{Name: "review", Types: []string{"github_pr_review", "doc_review"}, Aliases: map[string]string{"pr_review": "github_pr_review"}},
	{Name: "bead_replay", Types: []string{"create", "update", "close", "grant"}},
	// Only helm_deploy records a deployment on close (recordEscalationDeployment),
	// so its other spellings are types, not aliases.
	{Name: "release_roll", Types: []string{
		"helm_deploy", "merge_window", "release_roll", "release_cut", "helm_upgrade", "helm_release_rollout", "release_helm_upgrade",
		"release_and_helm_deploy", "k8s_helm_deploy", "helm_upgrade_and_remeasure", "k8s_node_label_and_helm_rollout", "deploy",
	}, Aliases: map[string]string{"github_pr_merge_and_deploy": "merge_window"}},
	{Name: "deploy_values", Types: []string{"helm_deploy_values"}, Aliases: map[string]string{
		"helm_values_change": "helm_deploy_values", "helm_values": "helm_deploy_values", "k8s_helm_deploy_values": "helm_deploy_values",
		"deploy_values_edit": "helm_deploy_values", "helm_values_rollout": "helm_deploy_values", "deploy_values_and_chart": "helm_deploy_values",
	}},
	{Name: "chart_edit", Types: []string{"helm_chart_change", "agent_image_change"}, Aliases: map[string]string{
		"helm_chart_edit": "helm_chart_change", "helm_dashboard_change": "helm_chart_change", "repo_edit_helm_lane": "helm_chart_change",
		"agent_image_tooling": "agent_image_change",
	}},
	{Name: "k8s_apply", Types: []string{"k8s_apply", "k8s_pod_delete"}, Aliases: map[string]string{
		"k8s_manifest_apply": "k8s_apply", "kubectl_apply": "k8s_apply", "k8s_deploy": "k8s_apply", "k8s_apply_and_rollout": "k8s_apply",
		"k8s_pod_rotate": "k8s_pod_delete",
	}},
	{Name: "k8s_rbac", Types: []string{"k8s_rbac", "k8s_rbac_grant"}},
	{Name: "edge", Types: []string{"edge_apply", "ziti_identity_management"}, Aliases: map[string]string{
		"edge_manifest_rollout": "edge_apply", "edge_public_vhost": "edge_apply", "edge_route_and_readonly_session": "edge_apply",
		"hooks_edge_cutover": "edge_apply",
	}},
	{Name: "prod_read", Types: []string{"live_verification", "k8s_exec_readonly", "k8s_pod_exec", "db_readonly_query", "local_probe", "soak_verification", "aws_read"}, Aliases: map[string]string{
		"k8s_verification": "live_verification", "live_check_runbook": "live_verification", "verification": "live_verification",
		"manual_verification": "live_verification", "edge_probe_from_outside": "live_verification", "bead_config_and_k8s_verify": "live_verification",
		"k8s_exec_readonly_sql": "k8s_exec_readonly", "laptop_local_read": "local_probe",
		"aws_verification": "aws_read", "aws_cloudwatch_read": "aws_read", "aws_cost_explorer_read": "aws_read",
		"aws_vpc_flow_logs": "aws_read", "aws_ssm_readonly": "aws_read",
	}},
	{Name: "terraform", Types: []string{"terraform_apply", "terraform_plan", "helm_values_and_terraform_apply"}, Aliases: map[string]string{
		"terraform_route53_dns_record": "terraform_apply", "terraform_helm_ziti_router_config": "terraform_apply",
	}},
	{Name: "cloud_infra", Types: []string{"aws_iam", "aws_ec2", "aws_ecr", "aws_s3_lifecycle_and_deploy", "deploy_values_and_aws", "infrastructure", "minio_bucket_quota", "ssh_file_permission"}, Aliases: map[string]string{
		"aws_ec2_node_replace": "aws_ec2", "aws_ses_iam_dns": "aws_iam", "infrastructure_change": "infrastructure",
	}},
	{Name: "database", Types: []string{"postgres_maintenance", "db_data_backfill"}},
	{Name: "credential", Types: []string{
		"credential_rotation", "credential_provisioning", "secret_write", "k8s_secret_seed", "litellm_key_rotation",
		"litellm_virtual_key_mount", "vault_provisioning", "project_secrets_and_credentials", "helm_values_secret_change",
		"aws_ses_smtp_k8s_secret_deploy_values",
	}, Aliases: map[string]string{
		"credential_provision": "credential_provisioning", "secret_provision": "secret_write", "k8s_secret_create": "k8s_secret_seed",
	}},
	{Name: "app_access", Types: []string{"github_token_scope", "github_app_permissions", "github_repo_settings", "github_repo_create", "slack_app_scope", "github_ruleset_bypass"}, Aliases: map[string]string{
		"github_permissions": "github_token_scope", "github_pull_request": "github_token_scope", "github_pr": "github_token_scope",
		"github_app_registration": "github_app_permissions", "github_app_config": "github_app_permissions",
		"github_app_settings": "github_app_permissions", "github_repo_setup": "github_repo_create",
	}},
	{Name: "github_actions", Types: []string{"github_actions_rerun", "github_workflow_dispatch", "github_workflow_edit"}, Aliases: map[string]string{
		"github_workflow_rerun": "github_actions_rerun",
	}},
	{Name: "bead_admin", Types: []string{
		"kbeads_admin_write", "kbeads_role_update", "kbeads_dep_edit", "kbeads_plan_lifecycle", "kbeads_taint_create",
		"release_record_backfill", "user_identity_pin",
	}, Aliases: map[string]string{
		"beads_admin_write": "kbeads_admin_write", "beads_admin_edit": "kbeads_admin_write", "admin_bead_write": "kbeads_admin_write",
		"bead_admin_write": "kbeads_admin_write", "kbeads_bead_update": "kbeads_admin_write", "bead_field_update": "kbeads_admin_write",
		"kbeads_config_write": "kbeads_admin_write", "beads_config_update": "kbeads_admin_write", "kbeads_label_add": "kbeads_admin_write",
		"role_bead_update": "kbeads_role_update", "kd_update_role": "kbeads_role_update",
		"kbeads_dep_add": "kbeads_dep_edit", "beads_dep_add": "kbeads_dep_edit", "beads_dep_remove": "kbeads_dep_edit",
		"beads_dep_edit": "kbeads_dep_edit", "bead_dependency_edit": "kbeads_dep_edit",
		"plan_complete": "kbeads_plan_lifecycle", "beads_plan_lifecycle": "kbeads_plan_lifecycle",
	}},
	{Name: "bead_close", Types: []string{"kbeads_bead_close", "bead_admin_close"}, Aliases: map[string]string{
		"bead_close": "kbeads_bead_close",
	}},
	{Name: "advice", Types: []string{"kbeads_advice_write"}, Aliases: map[string]string{
		"advice_update": "kbeads_advice_write", "kbeads_advice_create": "kbeads_advice_write",
		"kbeads_advice_update": "kbeads_advice_write", "kbeads_advice_relabel": "kbeads_advice_write",
	}},
	{Name: "trigger", Types: []string{"kbeads_trigger_update", "trigger_reseal", "trigger_prompt_install"}, Aliases: map[string]string{
		"kbeads_trigger_field_update": "kbeads_trigger_update", "trigger_retune": "kbeads_trigger_update", "trigger_enable": "kbeads_trigger_update",
		"kbeads_trigger_enable": "kbeads_trigger_update", "gardener_trigger_update": "kbeads_trigger_update",
		"gardener_trigger_rearm": "kbeads_trigger_update", "gardener_run_budget": "kbeads_trigger_update",
		"gardener_extend": "kbeads_trigger_update", "gardener_start": "kbeads_trigger_update",
		"gardener_cadence_reseal": "trigger_reseal",
		"trigger_prompt_reseal":   "trigger_prompt_install", "trigger_prompt_install_reseal": "trigger_prompt_install",
		"trigger_prompt_amend": "trigger_prompt_install",
	}},
	{Name: "kbeads_access", Types: []string{
		"kbeads_project_access", "kbeads_role_grant", "capability_grant", "kbeads_authz_change", "kbeads_fleet_token_admin",
		"agent_token_scope", "escalation_policy_write", "spawn_policy_write", "role_write",
	}, Aliases: map[string]string{
		"kbeads_bead_access": "kbeads_project_access", "beads_project_access": "kbeads_project_access", "agent_bead_access": "kbeads_project_access",
		"kbeads_machine_grant": "kbeads_role_grant", "kbeads_authz_policy": "kbeads_authz_change", "beads_rbac": "kbeads_authz_change",
		"fleet_token_audit": "kbeads_fleet_token_admin", "agent_token_scope_fix": "agent_token_scope",
	}},
	{Name: "agent_lifecycle", Types: []string{"agent_despawn", "role_scaffold", "create_role"}},
	{Name: "identity", Types: []string{
		"keycloak_client", "keycloak_idp_rollout", "kcadm_write", "kcadm_read_only", "keycloak_group_membership",
		"gardenio_user_capability", "lands_tenant_test_identity",
	}, Aliases: map[string]string{"keycloak_admin_permissions": "kcadm_write"}},
	{Name: "lands", Types: []string{"lands_window_mint", "lands_invite", "lands_channel_seed", "commons_channel_binding"}, Aliases: map[string]string{
		"window_mint": "lands_window_mint",
	}},
	// The owner floor's money and deletion categories (P10.F, gd-JJpjOjyvIL):
	// every type here is a D3 floor type (escalationFloorOperationTypes).
	{Name: "billing", Types: []string{"billing_change", "spend_limit_change"}, Aliases: map[string]string{
		"budget_change": "spend_limit_change", "litellm_budget_change": "spend_limit_change",
	}},
	{Name: "data_deletion", Types: []string{"data_deletion", "terraform_destroy", "db_data_delete", "storage_delete"}, Aliases: map[string]string{
		"data_delete": "data_deletion", "bucket_delete": "storage_delete", "volume_delete": "storage_delete",
		"pvc_delete": "storage_delete", "s3_delete": "storage_delete",
	}},
	{Name: "owner_action", Types: []string{
		"manual_owner_action", "manual_operator_action", "manual_intervention", "owner_device_action", "owner_browser_check",
		"manual_platform_acceptance", "commons_live_walkthrough", "jira_promote", "coopmux_account_sync", "git_push_public",
		"general", "misc",
	}, Aliases: map[string]string{"operator_manual": "manual_operator_action"}},
}

var operationTypeClass, operationTypeAlias = indexOperationClasses(operationClasses)

func indexOperationClasses(classes []operationClass) (class, alias map[string]string) {
	class, alias = map[string]string{}, map[string]string{}
	for _, c := range classes {
		for _, t := range c.Types {
			class[t] = c.Name
		}
		for a, t := range c.Aliases {
			class[a], alias[a] = c.Name, t
		}
	}
	return class, alias
}

// canonicalOperationType is the registered type op names, aliases resolved,
// or op normalized when the registry does not know it. Create stores it
// unless the policy routes it by another rule than op (aliasKeepsPolicyRule).
func canonicalOperationType(op string) string {
	op = normalizeOperationType(op)
	if t, ok := operationTypeAlias[op]; ok {
		return t
	}
	return op
}

// operationClassOf returns op's class, or "" when op is not registered.
func operationClassOf(op string) string { return operationTypeClass[normalizeOperationType(op)] }

func isOperationClass(name string) bool {
	return slices.ContainsFunc(operationClasses, func(c operationClass) bool { return c.Name == name })
}

// registeredOperationTypes is the closed set as an operator reads it: one
// "class: type, type" entry per class, aliases left out.
func registeredOperationTypes() string {
	parts := make([]string, 0, len(operationClasses))
	for _, c := range operationClasses {
		parts = append(parts, c.Name+": "+strings.Join(c.Types, ", "))
	}
	return strings.Join(parts, "; ")
}

// operationTypeGateMode is BEADS_OPERATION_TYPE_GATE: warn (the default; an
// unregistered type is filed, logged and counted), enforce (refused once a
// release shows the unregistered count at zero), off (no rewrite, no check).
// Any other value is off, as for BEADS_SHORT_TITLE_GATE.
func operationTypeGateMode() string {
	switch m := strings.ToLower(strings.TrimSpace(os.Getenv("BEADS_OPERATION_TYPE_GATE"))); m {
	case "", "warn":
		return "warn"
	case "enforce":
		return m
	}
	return "off"
}

// operationTypeGateTotal counts escalation creates by registry outcome.
// action=unregistered is the residual a flip to enforce would refuse.
// action=kept is an alias left as filed because the policy routes its type
// by another rule. Cardinality: class (the registry's classes plus "") ×
// action ∈ {registered,aliased,kept,unregistered,refused}.
var operationTypeGateTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "gardenio_beads_operation_type_gate_total",
		Help: "Escalation operation_type registry outcomes at create. class is the registry class (empty when unregistered); action ∈ {registered,aliased,kept,unregistered,refused}. action=unregistered is the residual a flip of BEADS_OPERATION_TYPE_GATE to enforce would refuse (gd-OXdyPW15fC).",
	},
	[]string{"class", "action"},
)

// applyOperationTypeRegistry rewrites an escalation's operation_type to its
// registered spelling and checks it against the registry. It runs first among
// the escalation create guards, so every later check reads the registered
// type.
func (s *BeadsServer) applyOperationTypeRegistry(ctx context.Context, bead *model.Bead) error {
	mode := operationTypeGateMode()
	if mode == "off" {
		return nil
	}
	filed, _, _ := exactFieldString(bead.Fields, "operation_type")
	op := canonicalOperationType(filed)
	class := operationClassOf(op)
	switch {
	case class != "" && op != normalizeOperationType(filed) && !s.aliasKeepsPolicyRule(ctx, bead, op):
		operationTypeGateTotal.WithLabelValues(class, "kept").Inc()
	case class != "" && op != filed:
		bead.Fields = setFieldValue(bead.Fields, "operation_type", op)
		operationTypeGateTotal.WithLabelValues(class, "aliased").Inc()
	case class != "":
		operationTypeGateTotal.WithLabelValues(class, "registered").Inc()
	case mode == "enforce":
		operationTypeGateTotal.WithLabelValues("", "refused").Inc()
		return inputError("operation_type " + strconvQuote(filed) + " is not registered; use one of " + registeredOperationTypes())
	default:
		operationTypeGateTotal.WithLabelValues("", "unregistered").Inc()
		addWarning(ctx, "operation_type "+strconvQuote(filed)+" is not registered; the policy will route it by its catch-all rule")
		slog.Warn("escalation operation_type not registered", "operation_type", filed, "actor", actorFromContext(ctx))
	}
	return nil
}

// aliasKeepsPolicyRule reports whether the active policy routes bead by the
// same rule under its filed operation_type and under op. Policy rules match
// operation_types on the filed type, so a rewrite that changed the rule
// would change who approves without an escalation_policy_write. An
// unreadable policy keeps the filed spelling.
func (s *BeadsServer) aliasKeepsPolicyRule(ctx context.Context, bead *model.Bead, op string) bool {
	p, err := s.activeEscalationPolicy(ctx)
	if err != nil {
		return false
	}
	f := s.escalationFactsFor(ctx, bead)
	g := f
	g.OperationType = op
	return evaluateEscalationPolicy(p, f).Rule == evaluateEscalationPolicy(p, g).Rule
}
