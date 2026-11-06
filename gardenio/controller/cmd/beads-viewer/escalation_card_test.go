package main

import (
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// realSpec mirrors the operation_spec shape on gd-2HYAp93i4I — the escalation
// that prompted the operator report.
const realSpec = `{"project":"Acme/Platform/site-deployment","mr_iid":1274,` +
	`"mr_url":"https://gitlab.com/Acme/Platform/site-deployment/-/merge_requests/1274",` +
	`"branch":"agent/gd-abc-fix","squash":true}`

func escBead(status string, fields map[string]string) *beadsapi.BeadDetail {
	return &beadsapi.BeadDetail{
		ID:     "gd-2HYAp93i4I",
		Title:  "Merge site-deployment!1274",
		Type:   "escalation",
		Status: status,
		Fields: fields,
	}
}

func TestBuildEscalationView_MRTargetIsClickable(t *testing.T) {
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	v := buildEscalationView(escBead("open", map[string]string{
		"operation_type":   "gitlab_mr_merge",
		"operation_spec":   realSpec,
		"context":          "CI is green; agents may self-merge gardenio MRs but this one is **site-deployment**.",
		"sensitivity":      "internal",
		"requesting_agent": "thread-123",
		"requesting_task":  "gd-task1",
	}), nil, "", now)
	if v == nil {
		t.Fatal("buildEscalationView returned nil")
	}
	if len(v.Targets) != 1 {
		t.Fatalf("Targets = %d, want 1: %+v", len(v.Targets), v.Targets)
	}
	tgt := v.Targets[0]
	if tgt.Kind != "mr" {
		t.Errorf("target Kind = %q, want mr", tgt.Kind)
	}
	if tgt.Label != "site-deployment!1274" {
		t.Errorf("target Label = %q, want site-deployment!1274", tgt.Label)
	}
	if tgt.Href != "https://gitlab.com/Acme/Platform/site-deployment/-/merge_requests/1274" {
		t.Errorf("target Href = %q", tgt.Href)
	}
	if tgt.Note != "agent/gd-abc-fix" {
		t.Errorf("target Note = %q, want the branch", tgt.Note)
	}
	if !v.Open {
		t.Error("open escalation should report Open")
	}
	if !v.HasSpec || !strings.Contains(string(v.SpecHTML), "field-json") {
		t.Errorf("operation_spec did not expand: %s", v.SpecHTML)
	}
	if !strings.Contains(string(v.ContextHTML), "site-deployment") {
		t.Errorf("context missing: %s", v.ContextHTML)
	}
	if v.OperationLabel != "Gitlab MR merge" {
		t.Errorf("OperationLabel = %q", v.OperationLabel)
	}
}

func TestBuildEscalationView_ApprovedAndRejected(t *testing.T) {
	now := time.Now()
	approved := buildEscalationView(escBead("closed", map[string]string{
		"operation_type":  "gitlab_mr_merge",
		"approved_by":     "gd-user1",
		"approved_at":     "2026-08-27T10:00:00Z",
		"approval_method": "web_ui",
		"result_status":   "success",
		"executor":        "daemon",
	}), nil, "", now)
	if !approved.Approved || approved.Rejected {
		t.Errorf("approved escalation misclassified: %+v", approved)
	}
	if approved.Open {
		t.Error("closed+approved escalation must not be Open")
	}

	rejected := buildEscalationView(escBead("closed", map[string]string{
		"operation_type":   "gitlab_mr_merge",
		"rejected_by":      "gd-user1",
		"rejection_reason": "wrong branch",
	}), nil, "", now)
	if !rejected.Rejected {
		t.Errorf("rejected escalation misclassified: %+v", rejected)
	}
	if rejected.Open {
		t.Error("rejected escalation must not be Open")
	}
	if rejected.RejectionReason != "wrong branch" {
		t.Errorf("RejectionReason = %q", rejected.RejectionReason)
	}
}

// TestBuildEscalationView_TailExcludesPrimaryFields asserts a field rendered in
// a dedicated slot is not ALSO dumped in the "other fields" tail.
func TestBuildEscalationView_TailExcludesPrimaryFields(t *testing.T) {
	v := buildEscalationView(escBead("open", map[string]string{
		"operation_type": "gitlab_mr_merge",
		"operation_spec": realSpec,
		"context":        "why",
		"notified_at":    "2026-08-27T10:00:00Z",
		"blank":          "   ",
	}), nil, "", time.Now())
	for _, row := range v.Fields {
		if escalationPrimaryFields[row.Key] {
			t.Errorf("primary field %q leaked into the tail", row.Key)
		}
		if row.Key == "blank" {
			t.Error("blank field should be dropped from the tail")
		}
	}
	if len(v.Fields) != 1 || v.Fields[0].Key != "notified_at" {
		t.Fatalf("tail = %+v, want just notified_at", v.Fields)
	}
	if !strings.Contains(string(v.Fields[0].Value), "field-time") {
		t.Errorf("tail timestamp not humanized: %s", v.Fields[0].Value)
	}
}

func TestEscalationTargetsFromSpec_NonMRAndGarbage(t *testing.T) {
	if got := escalationTargetsFromSpec("not json"); got != nil {
		t.Errorf("garbage spec produced targets: %+v", got)
	}
	got := escalationTargetsFromSpec(`{"dashboard_url":"https://grafana.example.com/d/abc","note":"look here"}`)
	if len(got) != 1 || got[0].Kind != "link" || got[0].Label != "Dashboard URL" {
		t.Fatalf("non-MR URL target = %+v", got)
	}
}

func TestMRTargetLabel_FallsBackToHref(t *testing.T) {
	if got := mrTargetLabel(map[string]any{}, "https://gitlab.com/x/-/merge_requests/7"); got != "https://gitlab.com/x/-/merge_requests/7" {
		t.Errorf("label fallback = %q", got)
	}
}

func TestBuildEscalationView_NilBead(t *testing.T) {
	if v := buildEscalationView(nil, nil, "", time.Now()); v != nil {
		t.Errorf("nil bead should yield nil view, got %+v", v)
	}
}

// spawnCreateSpec mirrors operation_spec on a spawn escalation: the raw
// CreateBeadRequest body for the denied agent-bead create (write_authz.go
// handleEscalation copies requestBody verbatim), title = agent name, project
// nested under fields.
const spawnCreateSpec = `{"title":"gardenio-gardener-for-gd-example","type":"agent",` +
	`"fields":{"project":"gardenio","role":"gardener"}}`

func TestBuildEscalationView_SpawnRequest(t *testing.T) {
	bead := escBead("open", map[string]string{
		"operation_type":   "create",
		"operation_spec":   spawnCreateSpec,
		"context":          "role gardener is privileged; a non-human caller must escalate",
		"requesting_agent": "gardenio-gardener-for-gd-parent",
		"spawn_role":       "gardener",
	})
	bead.OperatedBy = "oidc:matthewbaker1110@gmail.com"
	bead.OperatedByDisplay = "Matthew Baker"
	bead.CreatedBy = "gardenio-gardener-for-gd-parent"

	v := buildEscalationView(bead, nil, "", time.Now())
	if !v.IsSpawn {
		t.Fatal("spawn_role set should mark IsSpawn true")
	}
	if v.SpawnRole != "gardener" {
		t.Errorf("SpawnRole = %q, want gardener", v.SpawnRole)
	}
	if v.SpawnProject != "gardenio" {
		t.Errorf("SpawnProject = %q, want gardenio", v.SpawnProject)
	}
	if v.SpawnAgentName != "gardenio-gardener-for-gd-example" {
		t.Errorf("SpawnAgentName = %q", v.SpawnAgentName)
	}
	if v.SpawnNamespace != "" {
		t.Errorf("SpawnNamespace = %q, want empty (no namespace in spec)", v.SpawnNamespace)
	}
	if v.PolicyRule != "" {
		t.Errorf("PolicyRule = %q, want empty until a rule matches", v.PolicyRule)
	}
	if v.RequesterChainRootHTML == "" {
		t.Error("RequesterChainRootHTML should render the chain-root human chip")
	}
	if !strings.Contains(string(v.RequesterChainRootHTML), "Matthew Baker") {
		t.Errorf("RequesterChainRootHTML missing chain-root human: %s", v.RequesterChainRootHTML)
	}
	for _, row := range v.Fields {
		if row.Key == "spawn_role" || row.Key == "policy_rule" {
			t.Errorf("spawn field %q leaked into the generic tail", row.Key)
		}
	}
}

func TestBuildEscalationView_SpawnRequestPolicyRuleMatched(t *testing.T) {
	v := buildEscalationView(escBead("open", map[string]string{
		"operation_type": "create",
		"operation_spec": spawnCreateSpec,
		"spawn_role":     "gardener",
		"policy_rule":    "allow-gardener-in-gardenio",
	}), nil, "", time.Now())
	if v.PolicyRule != "allow-gardener-in-gardenio" {
		t.Errorf("PolicyRule = %q", v.PolicyRule)
	}
}

func TestBuildEscalationView_NonSpawnEscalationHasNoSpawnFields(t *testing.T) {
	v := buildEscalationView(escBead("open", map[string]string{
		"operation_type": "gitlab_mr_merge",
		"operation_spec": realSpec,
	}), nil, "", time.Now())
	if v.IsSpawn {
		t.Error("MR-merge escalation with no spawn_role should not be IsSpawn")
	}
	if v.RequesterChainRootHTML != "" {
		t.Error("non-spawn escalation should not populate RequesterChainRootHTML")
	}
}

func TestParseSpawnCreateSpec(t *testing.T) {
	project, agent, ns := parseSpawnCreateSpec(spawnCreateSpec)
	if project != "gardenio" || agent != "gardenio-gardener-for-gd-example" || ns != "" {
		t.Errorf("parseSpawnCreateSpec = (%q, %q, %q)", project, agent, ns)
	}
	if p, a, n := parseSpawnCreateSpec("not json"); p != "" || a != "" || n != "" {
		t.Errorf("garbage spec produced (%q, %q, %q)", p, a, n)
	}
	nsSpec := `{"title":"a","fields":{"project":"p","target_namespace":"ns-1"}}`
	if _, _, n := parseSpawnCreateSpec(nsSpec); n != "ns-1" {
		t.Errorf("namespace = %q, want ns-1", n)
	}
}
