package beadsapi

import "testing"

// TestDefaultEscalationAudience is the defaulting table: merges go to the
// desk, everything else (infra and unknown types alike) to the operators, and
// nothing defaults to the owner.
func TestDefaultEscalationAudience(t *testing.T) {
	for op, want := range map[string]string{
		"gitlab_mr_merge":         AudienceDesk,
		"github_pr_merge":         AudienceDesk,
		"gitlab_merge":            AudienceDesk,
		" GitLab_MR_Merge ":       AudienceDesk,
		"helm_chart_change":       AudienceOperator,
		"k8s_rbac":                AudienceOperator,
		"deploy_values_and_aws":   AudienceOperator,
		"keycloak_client":         AudienceOperator,
		"aws_iam":                 AudienceOperator,
		"aws_s3_bucket":           AudienceOperator,
		"live_check_runbook":      AudienceOperator,
		"commons_channel_binding": AudienceOperator,
		"manual_operator_action":  AudienceOperator,
		"something_new":           AudienceOperator,
		"":                        AudienceOperator,
	} {
		if got := DefaultEscalationAudience(op); got != want {
			t.Errorf("DefaultEscalationAudience(%q) = %q, want %q", op, got, want)
		}
	}
}

// escalationAudienceCases is TestEscalationAudience's table: the filer's or
// router's field wins over the default, an invalid field falls back to the
// default, and the forced-owner rule beats both.
var escalationAudienceCases = []struct {
	name   string
	fields map[string]string
	want   string
	forced bool
}{
	{"merge defaults to desk", map[string]string{"operation_type": "gitlab_mr_merge"}, AudienceDesk, false},
	{"infra defaults to operator", map[string]string{"operation_type": "helm_chart_change"}, AudienceOperator, false},
	{"filer says owner", map[string]string{"operation_type": "gitlab_mr_merge", "audience": "owner"}, AudienceOwner, false},
	{"routed to desk", map[string]string{"operation_type": "k8s_apply", "audience": "desk"}, AudienceDesk, false},
	{"field case and space", map[string]string{"operation_type": "k8s_apply", "audience": " Owner "}, AudienceOwner, false},
	{"invalid field falls back", map[string]string{"operation_type": "gitlab_mr_merge", "audience": "nobody"}, AudienceDesk, false},
	{"credentials force owner over desk", map[string]string{
		"operation_type": "gitlab_mr_merge", "audience": "desk",
		"credential_requirements": `[{"type":"github_sudo"}]`,
	}, AudienceOwner, true},
	{"credentials force owner over the default", map[string]string{
		"operation_type": "aws_iam", "credential_requirements": `[{"type":"aws_root"}]`,
	}, AudienceOwner, true},
	{"credential object forces owner", map[string]string{
		"operation_type": "aws_iam", "credential_requirements": `{"type":"vault"}`,
	}, AudienceOwner, true},
	{"unparseable credentials force owner", map[string]string{
		"operation_type": "aws_iam", "audience": "operator", "credential_requirements": `not json`,
	}, AudienceOwner, true},
	{"secret forces owner over operator", map[string]string{
		"operation_type": "k8s_rbac", "audience": "operator", "sensitivity": "secret",
	}, AudienceOwner, true},
	{"SECRET in any case", map[string]string{
		"operation_type": "k8s_rbac", "sensitivity": "SECRET",
	}, AudienceOwner, true},
	{"sensitive is not forced", map[string]string{
		"operation_type": "k8s_apply", "sensitivity": "sensitive",
	}, AudienceOperator, false},
	{"empty credential list is not forced", map[string]string{
		"operation_type": "gitlab_mr_merge", "credential_requirements": `[]`,
	}, AudienceDesk, false},
	{"null credentials are not forced", map[string]string{
		"operation_type": "gitlab_mr_merge", "credential_requirements": `null`,
	}, AudienceDesk, false},
	{"empty credential object is not forced", map[string]string{
		"operation_type": "gitlab_mr_merge", "credential_requirements": ` {} `,
	}, AudienceDesk, false},
	{"approvers naming a person force owner", map[string]string{
		"operation_type": "create", "audience": "operator", "approvers": `["oidc:owner@example.com"]`,
	}, AudienceOwner, true},
	{"a person among teams forces owner", map[string]string{
		"operation_type": "create", "approvers": `["team:ops","owner@example.com"]`,
	}, AudienceOwner, true},
	{"team-only approvers are not forced", map[string]string{
		"operation_type": "create", "approvers": `["team:ops"]`,
	}, AudienceOperator, false},
	{"lane-ref approvers are not people", map[string]string{
		"operation_type": "gitlab_mr_merge", "approvers": `["coordinator:merge-desk","operator"]`,
	}, AudienceDesk, false},
	{"slack approver is a person", map[string]string{
		"operation_type": "create", "approvers": `["slack:U0ABC"]`,
	}, AudienceOwner, true},
	{"empty approvers are not forced", map[string]string{
		"operation_type": "create", "approvers": `[]`,
	}, AudienceOperator, false},
	// escalation-routing D3: the floor operation types and the stamped
	// operator_only force the owner too.
	{"floor operation type forces owner", map[string]string{
		"operation_type": "credential_rotation",
	}, AudienceOwner, true},
	{"floor prefix forces owner over desk", map[string]string{
		"operation_type": "Secret_Write", "audience": "desk",
	}, AudienceOwner, true},
	{"operator_only forces owner", map[string]string{
		"operation_type": "helm_deploy", "operator_only": "true",
	}, AudienceOwner, true},
	// P10.F (gd-JJpjOjyvIL): the owner floor's five categories.
	{"role grant is on the floor", map[string]string{
		"operation_type": "k8s_rbac", "audience": "desk",
	}, AudienceOwner, true},
	{"floor alias is on the floor", map[string]string{
		"operation_type": "github_permissions",
	}, AudienceOwner, true},
	{"money is on the floor", map[string]string{
		"operation_type": "billing_ci_nodes",
	}, AudienceOwner, true},
	{"deletion is on the floor", map[string]string{
		"operation_type": "terraform_destroy",
	}, AudienceOwner, true},
	{"a trigger field update is not", map[string]string{
		"operation_type": "kbeads_trigger_update",
	}, AudienceOperator, false},
	{"helm_deploy alone is not forced", map[string]string{
		"operation_type": "helm_deploy",
	}, AudienceOperator, false},
	{"operator_only false is not forced", map[string]string{
		"operation_type": "helm_deploy", "operator_only": "false",
	}, AudienceOperator, false},
	// P10.F4 (gd-QiDj0JvkFr): a merge the desk named an operator class, and
	// publishing outside the farm.
	{"operator-class merge is on the floor", map[string]string{
		"operation_type": "gitlab_mr_merge", "audience": "desk", "operator_class": "2",
	}, AudienceOwner, true},
	{"operator_class off a merge is not", map[string]string{
		"operation_type": "helm_deploy", "operator_class": "2",
	}, AudienceOperator, false},
	{"public push is on the floor", map[string]string{
		"operation_type": "git_push_public",
	}, AudienceOwner, true},
}

// TestEscalationAudience covers the effective audience over
// escalationAudienceCases.
func TestEscalationAudience(t *testing.T) {
	for _, c := range escalationAudienceCases {
		t.Run(c.name, func(t *testing.T) {
			if got := EscalationAudience(c.fields); got != c.want {
				t.Errorf("EscalationAudience = %q, want %q", got, c.want)
			}
			if got := EscalationAudienceForced(c.fields); got != c.forced {
				t.Errorf("EscalationAudienceForced = %v, want %v", got, c.forced)
			}
			if got := EscalationForOwner(c.fields); got != (c.want == AudienceOwner) {
				t.Errorf("EscalationForOwner = %v, want %v", got, c.want == AudienceOwner)
			}
		})
	}
}

func TestInHandLine(t *testing.T) {
	var h InHand
	if got := h.Line(); got != "" {
		t.Errorf("empty Line = %q, want \"\"", got)
	}
	for _, a := range []string{AudienceDesk, AudienceDesk, AudienceOperator, AudienceOwner} {
		h.Add(a)
	}
	if got, want := h.Line(), "In hand: 3 (desk 2, operator 1)"; got != want {
		t.Errorf("Line = %q, want %q", got, want)
	}
	if got, want := (InHand{Operator: 2}).Line(), "In hand: 2 (operator 2)"; got != want {
		t.Errorf("Line = %q, want %q", got, want)
	}
	var l InHand
	l.Add(AudienceDesk)
	l.AddLand()
	l.AddLand()
	if got, want := l.Line(), "In hand: 3 (desk 1, land owners 2)"; got != want {
		t.Errorf("Line with land owners = %q, want %q", got, want)
	}
}
