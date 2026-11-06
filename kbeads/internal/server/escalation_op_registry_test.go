package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"gardenio/kbeads/internal/model"
)

// TestOperationRegistryWellFormed: each type and alias is in one class only,
// and an alias names a type of its own class.
func TestOperationRegistryWellFormed(t *testing.T) {
	seen := map[string]string{}
	classes := map[string]bool{}
	add := func(name, class string) {
		if name != normalizeOperationType(name) {
			t.Errorf("%q in class %s is not normalized", name, class)
		}
		if prev, dup := seen[name]; dup {
			t.Errorf("%q is in class %s and class %s", name, prev, class)
		}
		seen[name] = class
	}
	for _, c := range operationClasses {
		if classes[c.Name] {
			t.Errorf("class %s is declared twice", c.Name)
		}
		classes[c.Name] = true
		for _, typ := range c.Types {
			add(typ, c.Name)
		}
		for a, typ := range c.Aliases {
			add(a, c.Name)
			if operationTypeClass[typ] != c.Name || operationTypeAlias[typ] != "" {
				t.Errorf("alias %q -> %q: the target is not a type of class %s", a, typ, c.Name)
			}
		}
	}
	if n := len(operationClasses); n < 20 || n > 30 {
		t.Errorf("%d classes; P7.4 folds the seen types into about 25", n)
	}
}

// TestOperationRegistryAliasesKeepSemantics: create rewrites an alias to its
// type, so the two must agree on every set that decides what may happen to an
// escalation rather than where it is routed.
func TestOperationRegistryAliasesKeepSemantics(t *testing.T) {
	checks := map[string]func(string) bool{
		"floor":        func(op string) bool { return escalationFloorOperation(op) != "" },
		"replay":       isServerReplayOperation,
		"replayUnfit":  isReplayUnfitOperation,
		"beadClose":    func(op string) bool { return beadCloseOperationTypes[op] },
		"coordinator":  func(op string) bool { return coordinatorRefusedOperations[op] },
		"scoped":       func(op string) bool { return scopedApprovableOperations[op] },
		"verdict":      isVerdictOperation,
		"steward":      func(op string) bool { return stewardDelegableMerges[op] },
		"deskAudience": func(op string) bool { return defaultEscalationAudience(op) == audienceDesk },
		"helmDeploy":   func(op string) bool { return op == helmDeployOperation },
		"beadOnly":     isBeadOnlyOperation,
	}
	// pr_review is a request for a review verdict, so it becomes one
	// (merge desk ruling on PR #2018, gd-OXdyPW15fC); it stays an operator
	// escalation that no steward may approve.
	intended := map[string]string{"pr_review": "verdict"}
	if got := operationTypeAlias["pr_review"]; got != "github_pr_review" {
		t.Errorf("pr_review aliases %q, want github_pr_review", got)
	}
	for a, typ := range operationTypeAlias {
		for name, check := range checks {
			if check(a) != check(typ) && intended[a] != name {
				t.Errorf("alias %q -> %q changes %s (%v -> %v)", a, typ, name, check(a), check(typ))
			}
		}
	}
}

// TestOperationRegistryCoversCodeTypes: every type the daemon names in code
// is registered, so enforce never refuses an escalation the code files or
// reads.
func TestOperationRegistryCoversCodeTypes(t *testing.T) {
	var named []string
	for _, set := range []map[string]bool{verdictOperationTypes, serverReplayOperationTypes, escalationFloorOperationTypes, beadCloseOperationTypes, stewardDelegableMerges, coordinatorRefusedOperations, scopedApprovableOperations} {
		for op := range set {
			named = append(named, op)
		}
	}
	for _, r := range defaultEscalationPolicy().Rules {
		named = append(named, r.When.OperationTypes...)
	}
	// general is the bridge's PromoteDecisionToEscalation default; infrastructure_change is gardenio/docs/project-infra-escalation-chain.md's.
	named = append(named, helmDeployOperation, "manual_operator_action", "gardener_cadence_reseal", "jira_promote", "general", "misc", "k8s_rbac_grant", "infrastructure_change")
	for _, op := range named {
		if operationClassOf(op) == "" {
			t.Errorf("%q is named in code but not registered", op)
		}
	}
}

func TestApplyOperationTypeRegistry(t *testing.T) {
	cases := []struct {
		mode, filed, wantType, action string
		refused                       bool
	}{
		{"", " Kubectl_Apply ", "k8s_apply", "aliased", false},
		{"warn", "github_pr_merge", "github_pr_merge", "registered", false},
		{"warn", "GitHub_PR_Merge", "github_pr_merge", "aliased", false},
		{"warn", "frobnicate", "frobnicate", "unregistered", false},
		{"enforce", "frobnicate", "frobnicate", "refused", true},
		{"enforce", "bead_close", "kbeads_bead_close", "aliased", false},
		{"off", "helm_upgrade", "helm_upgrade", "", false},
		{"bogus", "frobnicate", "frobnicate", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.mode+"/"+tc.filed, func(t *testing.T) {
			t.Setenv("BEADS_OPERATION_TYPE_GATE", tc.mode)
			operationTypeGateTotal.Reset()
			raw, _ := json.Marshal(map[string]string{"operation_type": tc.filed})
			b := &model.Bead{Type: model.TypeEscalation, Fields: raw}
			ctx := withWarnings(context.Background())
			s, _, _ := newTestServer()
			err := s.applyOperationTypeRegistry(ctx, b)
			if (err != nil) != tc.refused {
				t.Fatalf("err = %v, want refused=%v", err, tc.refused)
			}
			if tc.refused && !strings.Contains(err.Error(), "release_roll: helm_deploy") {
				t.Errorf("refusal %q does not list the registered types", err)
			}
			if got, _, _ := exactFieldString(b.Fields, "operation_type"); got != tc.wantType {
				t.Errorf("operation_type = %q, want %q", got, tc.wantType)
			}
			if tc.action == "" {
				if n := testutil.CollectAndCount(operationTypeGateTotal); n != 0 {
					t.Errorf("gate off counted %d series", n)
				}
				return
			}
			class := operationClassOf(tc.wantType)
			if got := testutil.ToFloat64(operationTypeGateTotal.WithLabelValues(class, tc.action)); got != 1 {
				t.Errorf("counter{%q,%q} = %v, want 1", class, tc.action, got)
			}
			if warned := len(warningsFromContext(ctx)) > 0; warned != (tc.action == "unregistered") {
				t.Errorf("warned = %v for action %s", warned, tc.action)
			}
		})
	}
}

func TestEscalationPolicyMatchesOperationClass(t *testing.T) {
	rolls := escalationPolicyWhen{OperationClasses: []string{"release_roll"}}
	for op, want := range map[string]bool{"helm_deploy": true, "helm_upgrade": true, "MERGE_WINDOW": true, "k8s_apply": false, "frobnicate": false} {
		if got := rolls.matches(escalationFacts{OperationType: op}); got != want {
			t.Errorf("operation_classes [release_roll] matches %q = %v, want %v", op, got, want)
		}
	}
	// operation_types matches the filed spelling only: an alias joins a
	// type's rule through operation_classes, never by itself.
	applies := escalationPolicyWhen{OperationTypes: []string{"k8s_apply"}}
	for op, want := range map[string]bool{"k8s_apply": true, " K8S_Apply": true, "k8s_deploy": false, "kubectl_apply": false} {
		if got := applies.matches(escalationFacts{OperationType: op}); got != want {
			t.Errorf("operation_types [k8s_apply] matches %q = %v, want %v", op, got, want)
		}
	}
	if !(escalationPolicyWhen{OperationClasses: []string{"k8s_apply"}}).matches(escalationFacts{OperationType: "k8s_deploy"}) {
		t.Error("operation_classes [k8s_apply] does not match its alias k8s_deploy")
	}
	if rolls.empty() {
		t.Error("a when with only operation_classes is not the catch-all")
	}
}

func TestValidateEscalationPolicy_OperationClasses(t *testing.T) {
	policy := func(classes ...string) json.RawMessage {
		p := defaultEscalationPolicy()
		rule := escalationPolicyRule{When: escalationPolicyWhen{OperationClasses: classes}, Approvers: []string{"operator"}, ExecuteBy: "coordinator:laptop"}
		p.Rules = append([]escalationPolicyRule{rule}, p.Rules...)
		raw, _ := json.Marshal(p)
		return raw
	}
	if reason := validateEscalationPolicy(policy("release_roll", "prod_read")); reason != "" {
		t.Fatalf("registered classes refused: %s", reason)
	}
	if reason := validateEscalationPolicy(policy("helm_deploy")); !strings.Contains(reason, `operation class "helm_deploy" is not registered`) {
		t.Fatalf("a type named as a class: reason %q", reason)
	}
}

// TestEscalationCreate_StoresRegisteredType: create stores an alias's type
// when the policy routes both by the same rule, and keeps the filed spelling
// when the type has a rule of its own, so the alias never changes who
// approves (k8s_deploy must not reach a k8s_apply rule).
func TestEscalationCreate_StoresRegisteredType(t *testing.T) {
	_, ms, h := newTestServer()
	create := func(op string) *model.Bead {
		t.Helper()
		rec := doJSON(t, h, "POST", "/v1/beads", map[string]any{
			"title":  "apply " + op,
			"type":   "escalation",
			"kind":   "data",
			"labels": []string{"project:gardenio"},
			"fields": map[string]any{"operation_type": op, "operation_spec": map[string]any{"filing": op}},
		})
		requireStatus(t, rec, 201)
		var created model.Bead
		decodeJSON(t, rec, &created)
		return ms.beads[created.ID]
	}
	check := func(b *model.Bead, wantType, wantRule string) {
		t.Helper()
		if got, _, _ := exactFieldString(b.Fields, "operation_type"); got != wantType {
			t.Errorf("stored operation_type = %q, want %q", got, wantType)
		}
		if got, _, _ := exactFieldString(b.Fields, "policy_rule"); got != wantRule {
			t.Errorf("policy_rule = %q, want %q", got, wantRule)
		}
	}
	check(create("kubectl_apply"), "k8s_apply", "escalation#5") // D7's catch-all either way

	pol, _ := json.Marshal(map[string]any{
		"rules": []any{
			map[string]any{"when": map[string]any{"operation_types": []string{"k8s_apply"}}, "approvers": []string{"coordinator:laptop", "operator"}, "execute_by": "coordinator:laptop"},
			map[string]any{"when": map[string]any{}, "approvers": []string{"operator"}, "execute_by": "operator"},
		},
		"coordinators": map[string]any{"laptop": map[string]any{"principals": []string{"machine:gardenio-laptop-operator"}}},
	})
	ms.beads["gd-pol-apply"] = &model.Bead{ID: "gd-pol-apply", Kind: model.KindConfig, Type: model.BeadType(escalationPolicyType), Title: "escalation policy", Status: model.StatusOpen, Fields: pol}
	operationTypeGateTotal.Reset()
	check(create("k8s_deploy"), "k8s_deploy", "escalation#2")
	if got := testutil.ToFloat64(operationTypeGateTotal.WithLabelValues("k8s_apply", "kept")); got != 1 {
		t.Errorf("counter{k8s_apply,kept} = %v, want 1", got)
	}
	check(create("k8s_apply"), "k8s_apply", "escalation#1")
}
