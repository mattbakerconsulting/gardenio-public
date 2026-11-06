package server

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"testing"

	"gardenio/kbeads/internal/model"
)

func TestAuthorizeWrite_StaticTokenBypass(t *testing.T) {
	srv, _, _ := testCtx(t)
	ctx := staticCtx()

	denial := srv.authorizeWrite(ctx, "create", "agent", []string{"project:gardenio"})
	if denial != nil {
		t.Fatalf("unscoped static token should bypass write policy, got: %v", denial)
	}
}

// TestAuthorizeWrite_StaticToken_AdminGatedTypeDenied covers gd-ZjiSd9Kzio:
// the shared primary static token is no longer admin everywhere. An unscoped
// static token creating a type:team bead (min_role: gardenio-admin) must be
// denied exactly as a roleless OIDC caller would be. Only types/actions with
// no entry in machineAllowedWrites belong here — user/account/role/config/
// jira-poller create is now allow-listed and covered by
// TestAuthorizeWrite_StaticToken_MachineAllowlistBypasses instead.
func TestAuthorizeWrite_StaticToken_AdminGatedTypeDenied(t *testing.T) {
	srv, _, _ := testCtx(t)
	ctx := staticCtx()

	types := []model.BeadType{"team", "rig", "convoy", "audit", "credential", "fleet"}
	for _, bt := range types {
		t.Run(string(bt), func(t *testing.T) {
			d := srv.authorizeWrite(ctx, "create", bt, nil)
			if d == nil {
				t.Fatalf("static token should be denied on admin-gated type %q", bt)
			}
			if d.RequiredRole != RoleAdmin {
				t.Errorf("RequiredRole=%q, want %q", d.RequiredRole, RoleAdmin)
			}
		})
	}
}

// TestAuthorizeWrite_StaticToken_MachineAllowlistBypasses covers
// gd-ZjiSd9Kzio's review requirement: every (action, type) pair in
// machineAllowedWrites is a real, deployed non-OIDC call site (gb/kd CLI,
// bridges, controller reconcilers) and must NOT be denied for an unscoped
// static token, even though the type is otherwise admin-gated.
func TestAuthorizeWrite_StaticToken_MachineAllowlistBypasses(t *testing.T) {
	srv, _, _ := testCtx(t)

	for key := range machineAllowedWrites {
		t.Run(key.Action+"/"+string(key.BeadType), func(t *testing.T) {
			ctx := staticCtx()
			if d := srv.authorizeWrite(ctx, key.Action, key.BeadType, nil); d != nil {
				t.Fatalf("allow-listed (%s, %s) should bypass write policy for a static token, got: %v", key.Action, key.BeadType, d)
			}
		})
	}
}

// TestMachineAllowedWrites_ExactSet pins machineAllowedWrites to the reviewed
// set (gd-whES7ZdkAy). A later addition or deletion is a visible diff here, so
// it cannot slip in without the evidence the list's doc comment requires: an
// addition needs a concrete non-OIDC call site, a deletion needs every
// remaining caller to hold a machine grant for the pair (machine_grants.go).
// As built, no entry meets that deletion bar yet: each is kept by a gb/kd
// agent-pod caller on the shared static token (until P7 per-agent tokens), a
// human-driven service write on a fleet or static token, or a chart-only
// component with no machine principal. The reason per entry is on
// machineAllowedWrites and in machine-principal-design.md §12.
func TestMachineAllowedWrites_ExactSet(t *testing.T) {
	want := []string{
		"close/advice",
		"close/report",
		"comment/project",
		"create/account",
		"create/advice",
		"create/config",
		"create/jira-poller",
		"create/report",
		"create/role",
		"create/user",
		"update/config",
		"update/project",
		"update/report",
		"update/user",
	}
	var got []string
	for k := range machineAllowedWrites {
		got = append(got, k.Action+"/"+string(k.BeadType))
	}
	sort.Strings(got)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("machineAllowedWrites changed.\n got: %v\nwant: %v\n"+
			"Update this list only with the evidence machineAllowedWrites' doc comment requires.", got, want)
	}
	// account/update is never allow-listed: it is held only through the
	// gardenio-machine-accounts grant, so the static token stays denied.
	if isMachineAllowedWrite("update", "account") {
		t.Fatal("update/account must never be in machineAllowedWrites")
	}
	// project/create was removed (gd-WCM9rfOL8L): a project bead with no
	// land_kind is an operator project with fleet-App GitHub tokens, so only an
	// admin and the Lands provisioner (tenant lands only) create one.
	if isMachineAllowedWrite("create", "project") {
		t.Fatal("create/project must never be in machineAllowedWrites")
	}
}

// TestAuthorizeWrite_StaticToken_MachineAllowlistIsNarrow guards against the
// allow-list silently widening to cover an action it was never reviewed
// for: an action NOT listed in machineAllowedWrites for a given type must
// still deny a static token, even when a different action on that same type
// is allow-listed (e.g. advice/create is allowed but advice/delete is not).
func TestAuthorizeWrite_StaticToken_MachineAllowlistIsNarrow(t *testing.T) {
	srv, _, _ := testCtx(t)
	ctx := staticCtx()

	cases := []struct {
		action   string
		beadType model.BeadType
	}{
		{"delete", "user"},
		{"close", "user"},
		{"update", "account"},
		{"delete", "account"},
		{"update", "role"},
		{"delete", "role"},
		{"delete", "config"},
		{"delete", "project"},
		{"comment", "report"},
		{"update", "jira-poller"},
		{"delete", "advice"},
		{"delete", "report"},
		{"create", "team"},
	}
	for _, c := range cases {
		t.Run(c.action+"/"+string(c.beadType), func(t *testing.T) {
			d := srv.authorizeWrite(ctx, c.action, c.beadType, nil)
			if d == nil {
				t.Fatalf("non-allow-listed (%s, %s) should still be denied for a static token", c.action, c.beadType)
			}
			if d.RequiredRole != RoleAdmin {
				t.Errorf("RequiredRole=%q, want %q", d.RequiredRole, RoleAdmin)
			}
		})
	}
}

// TestAuthorizeWrite_StaticToken_ProjectScopedTypeBypassed guards against a
// regression: project_scoped types (gate, decision, ...) have no resolvable
// operator identity for a static-token caller (Subject is the literal
// "static", not an operator), so project scoping stays bypassed for them —
// only min_role tightened. Tightening project scoping too would deny routine
// controller/agent traffic against these types, a much larger and unrelated
// change (see authorizeWrite's doc comment).
func TestAuthorizeWrite_StaticToken_ProjectScopedTypeBypassed(t *testing.T) {
	srv, _, _ := testCtx(t)
	ctx := staticCtx()

	types := []model.BeadType{"gate", "decision"}
	for _, bt := range types {
		t.Run(string(bt), func(t *testing.T) {
			if d := srv.authorizeWrite(ctx, "create", bt, []string{"project:gardenio"}); d != nil {
				t.Fatalf("static token should still bypass project_scoped type %q, got: %v", bt, d)
			}
		})
	}
}

// TestAuthorizeWrite_AuthDisabled_Bypasses pins gd-ZjiSd9Kzio round 3 item
// 2: authMethodNone (a daemon with no auth configured at all — dev/test
// mode, stamped by AuthInterceptor/AuthMiddleware when there is no static
// token, no fleet tokens, and no OIDC verifier) bypasses write policy even
// on an admin-gated type. Contrast with
// TestAuthorizeWrite_StaticToken_AdminGatedTypeDenied, which pins the
// opposite case this task exists to fix: a REAL static token against a
// daemon that DOES have auth configured is denied on an admin-gated type.
func TestAuthorizeWrite_AuthDisabled_Bypasses(t *testing.T) {
	srv, _, _ := testCtx(t)
	ctx := withAuthMethod(context.Background(), authMethodNone)

	if d := srv.authorizeWrite(ctx, "create", "team", nil); d != nil {
		t.Fatalf("auth-disabled daemon should bypass write policy on admin-gated type, got: %v", d)
	}
}

// TestAuthorizeWrite_MachineWrite_Bypasses covers the narrow, code-only
// escape hatch for genuine machine-origin writes (audit_log.go's
// recordAuditEntry, http_escalations.go's replayEscalation): a ctx marked
// withMachineWrite bypasses write policy even on an admin-gated type,
// unlike a bare static token (TestAuthorizeWrite_StaticToken_AdminGatedTypeDenied).
func TestAuthorizeWrite_MachineWrite_Bypasses(t *testing.T) {
	srv, _, _ := testCtx(t)
	ctx := withMachineWrite(staticCtx())

	if d := srv.authorizeWrite(ctx, "create", "audit", nil); d != nil {
		t.Fatalf("withMachineWrite ctx should bypass write policy on admin-gated type, got: %v", d)
	}
}

// TestAuthorizeWrite_SystemOperatorHeader_DoesNotBypass is a regression
// guard for the spoofing risk identified in gd-ZjiSd9Kzio: WithSystemOperator
// (once settable from the caller-controlled X-System-Operator header on the
// static-token branch, retired in gd-ke6PC7bRto) must never be treated as an
// authorization bypass signal, even though WithSystemOperator(ctx, "machine")
// collides byte-for-byte with the operatorMachine sentinel WithMachineOperator
// writes to the same context key. Only withMachineWrite (a distinct, header-
// unreachable key) may bypass authorizeWrite.
func TestAuthorizeWrite_SystemOperatorHeader_DoesNotBypass(t *testing.T) {
	srv, _, _ := testCtx(t)
	ctx := WithSystemOperator(staticCtx(), "machine")

	d := srv.authorizeWrite(ctx, "create", "team", nil)
	if d == nil {
		t.Fatalf("X-System-Operator: machine must not bypass write policy on an admin-gated type")
	}
	if d.RequiredRole != RoleAdmin {
		t.Errorf("RequiredRole=%q, want %q", d.RequiredRole, RoleAdmin)
	}
}

func TestAuthorizeWrite_ScopedStaticToken_TypeAllowed(t *testing.T) {
	srv, _, _ := testCtx(t)
	ctx := staticCtx()
	ctx = WithTokenTypeScope(ctx, []string{"agent", "config"})

	denial := srv.authorizeWrite(ctx, "create", "agent", nil)
	if denial != nil {
		t.Fatalf("scoped token should allow agent type, got: %v", denial)
	}
}

func TestAuthorizeWrite_ScopedStaticToken_TypeDenied(t *testing.T) {
	srv, _, _ := testCtx(t)
	ctx := staticCtx()
	ctx = WithTokenTypeScope(ctx, []string{"agent", "config"})

	denial := srv.authorizeWrite(ctx, "create", "task", nil)
	if denial == nil {
		t.Fatal("scoped token should deny task type")
	}
	if denial.BeadType != "task" {
		t.Errorf("denial.BeadType=%q, want task", denial.BeadType)
	}
	if !strings.Contains(denial.Message, "token scope denied") {
		t.Errorf("expected token scope denial message, got: %s", denial.Message)
	}
}

func TestAuthorizeWrite_ScopedStaticToken_OpAllowed(t *testing.T) {
	srv, _, _ := testCtx(t)
	ctx := staticCtx()
	ctx = WithTokenOpScope(ctx, []string{"create", "update"})

	denial := srv.authorizeWrite(ctx, "create", "agent", nil)
	if denial != nil {
		t.Fatalf("scoped token should allow create op, got: %v", denial)
	}
}

func TestAuthorizeWrite_ScopedStaticToken_OpDenied(t *testing.T) {
	srv, _, _ := testCtx(t)
	ctx := staticCtx()
	ctx = WithTokenOpScope(ctx, []string{"create"})

	denial := srv.authorizeWrite(ctx, "close", "agent", nil)
	if denial == nil {
		t.Fatal("scoped token should deny close op")
	}
	if !strings.Contains(denial.Message, "operation close not permitted") {
		t.Errorf("expected op scope denial message, got: %s", denial.Message)
	}
}

func TestAuthorizeWrite_ScopedStaticToken_BothScopes(t *testing.T) {
	srv, _, _ := testCtx(t)
	ctx := staticCtx()
	ctx = WithTokenTypeScope(ctx, []string{"agent"})
	ctx = WithTokenOpScope(ctx, []string{"create"})

	// Both match.
	if d := srv.authorizeWrite(ctx, "create", "agent", nil); d != nil {
		t.Fatalf("should allow when both scopes match, got: %v", d)
	}

	// Type mismatch.
	if d := srv.authorizeWrite(ctx, "create", "task", nil); d == nil {
		t.Fatal("should deny when type doesn't match")
	}

	// Op mismatch.
	if d := srv.authorizeWrite(ctx, "update", "agent", nil); d == nil {
		t.Fatal("should deny when op doesn't match")
	}
}

func TestAuthorizeWrite_ScopedStaticToken_WildcardType(t *testing.T) {
	srv, _, _ := testCtx(t)
	ctx := staticCtx()
	ctx = WithTokenTypeScope(ctx, []string{"*"})
	ctx = WithTokenOpScope(ctx, []string{"create"})

	denial := srv.authorizeWrite(ctx, "create", "anything", nil)
	if denial != nil {
		t.Fatalf("wildcard type scope should allow any type, got: %v", denial)
	}
}

func TestCheckTokenScopes_NoScopes(t *testing.T) {
	ctx := context.Background()
	if d := checkTokenScopes(ctx, "create", "agent"); d != nil {
		t.Fatalf("no scopes should return nil, got: %v", d)
	}
}

func TestCheckTokenScopes_TypeOnly(t *testing.T) {
	ctx := WithTokenTypeScope(context.Background(), []string{"agent"})
	if d := checkTokenScopes(ctx, "create", "agent"); d != nil {
		t.Fatalf("matching type should return nil, got: %v", d)
	}
	if d := checkTokenScopes(ctx, "create", "task"); d == nil {
		t.Fatal("non-matching type should return denial")
	}
}

func TestCheckTokenScopes_OpOnly(t *testing.T) {
	ctx := WithTokenOpScope(context.Background(), []string{"create", "update"})
	if d := checkTokenScopes(ctx, "update", "anything"); d != nil {
		t.Fatalf("matching op should return nil, got: %v", d)
	}
	if d := checkTokenScopes(ctx, "close", "anything"); d == nil {
		t.Fatal("non-matching op should return denial")
	}
}

func TestAuthorizeWrite_AdminBypass(t *testing.T) {
	srv, _, _ := testCtx(t)
	ctx := oidcCtx("admin@example.com", []string{RoleAdmin})

	denial := srv.authorizeWrite(ctx, "create", "agent", []string{"project:gardenio"})
	if denial != nil {
		t.Fatalf("admin should bypass write policy, got: %v", denial)
	}
}

func TestAuthorizeWrite_MinRoleDenied(t *testing.T) {
	srv, _, _ := testCtx(t)
	// User type has min_role: gardenio-admin in builtins.
	ctx := oidcCtx("operator@example.com", []string{RoleOperator})

	denial := srv.authorizeWrite(ctx, "create", "user", nil)
	if denial == nil {
		t.Fatal("operator should be denied write on user type (requires admin)")
	}
	if denial.RequiredRole != RoleAdmin {
		t.Errorf("denial.RequiredRole=%q, want %q", denial.RequiredRole, RoleAdmin)
	}
	if denial.Escalatable {
		t.Error("user type should not be escalatable")
	}
	if denial.AdvisoryText == "" {
		t.Error("denial should include advisory text")
	}
	if !strings.Contains(denial.AdvisoryText, "create") {
		t.Error("advisory text should mention the denied action")
	}
	if !strings.Contains(denial.AdvisoryText, "user") {
		t.Error("advisory text should mention the bead type")
	}
}

// TestAuthorizeWrite_TeamMinRoleDenied covers the ka-ORaCUV7C6g acceptance
// criterion: creating a type:team bead as a non-admin OIDC identity is
// rejected (ADR docs/adr/taints.md §8.1 — admin-only writes, same family as
// type:project/type:account).
func TestAuthorizeWrite_TeamMinRoleDenied(t *testing.T) {
	srv, _, _ := testCtx(t)
	ctx := oidcCtx("operator@example.com", []string{RoleOperator})

	denial := srv.authorizeWrite(ctx, "create", "team", nil)
	if denial == nil {
		t.Fatal("operator should be denied write on team type (requires admin)")
	}
	if denial.RequiredRole != RoleAdmin {
		t.Errorf("denial.RequiredRole=%q, want %q", denial.RequiredRole, RoleAdmin)
	}
}

func TestAuthorizeWrite_TeamMinRoleAllowed(t *testing.T) {
	srv, _, _ := testCtx(t)
	ctx := oidcCtx("admin@example.com", []string{RoleAdmin})

	denial := srv.authorizeWrite(ctx, "create", "team", nil)
	if denial != nil {
		t.Fatalf("admin should be allowed write on team type, got: %v", denial)
	}
}

func TestAuthorizeWrite_MinRoleAllowed(t *testing.T) {
	srv, _, _ := testCtx(t)
	ctx := oidcCtx("admin@example.com", []string{RoleAdmin})

	denial := srv.authorizeWrite(ctx, "create", "user", nil)
	if denial != nil {
		t.Fatalf("admin should be allowed write on user type, got: %v", denial)
	}
}

func TestAuthorizeWrite_ProjectScopedAllowed(t *testing.T) {
	srv, ms, _ := testCtx(t)
	makeUserBead(ms, "gd-user1", "alice@example.com", "gardenio,monorepo", "crew")
	ctx := oidcCtx("alice@example.com", []string{RoleOperator})

	denial := srv.authorizeWrite(ctx, "create", "agent", []string{"project:gardenio"})
	if denial != nil {
		t.Fatalf("should allow write for authorized project, got: %v", denial)
	}
}

func TestAuthorizeWrite_ProjectScopedDenied(t *testing.T) {
	srv, ms, _ := testCtx(t)
	makeUserBead(ms, "gd-user2", "bob@example.com", "monorepo", "crew")
	ctx := oidcCtx("bob@example.com", []string{RoleOperator})

	denial := srv.authorizeWrite(ctx, "create", "agent", []string{"project:gardenio"})
	if denial == nil {
		t.Fatal("should deny write for unauthorized project")
	}
	if denial.RequiredScope != "gardenio" {
		t.Errorf("denial.RequiredScope=%q, want gardenio", denial.RequiredScope)
	}
	if !denial.Escalatable {
		t.Error("agent type should be escalatable")
	}
	// Escalatable denial should include --escalate hint.
	if !strings.Contains(denial.AdvisoryText, "--escalate") {
		t.Errorf("escalatable denial should mention --escalate, got: %s", denial.AdvisoryText)
	}
}

func TestAuthorizeWrite_CustomAdvisoryTemplate(t *testing.T) {
	srv, ms, _ := testCtx(t)
	makeUserBead(ms, "gd-user-custom", "custom@example.com", "monorepo", "crew")

	// Set custom advisory text template.
	ms.configs["authz-denied-text"] = &model.Config{
		Key:   "authz-denied-text",
		Value: json.RawMessage(`{"template":"CUSTOM: {action} on {type} blocked. {reason} {escalate_hint}"}`),
	}

	ctx := oidcCtx("custom@example.com", []string{RoleOperator})
	denial := srv.authorizeWrite(ctx, "create", "agent", []string{"project:gardenio"})
	if denial == nil {
		t.Fatal("expected denial")
	}
	if !strings.HasPrefix(denial.AdvisoryText, "CUSTOM:") {
		t.Errorf("expected custom template, got: %s", denial.AdvisoryText)
	}
}

func TestAuthorizeWrite_ProjectFromFields(t *testing.T) {
	srv, ms, _ := testCtx(t)
	makeUserBead(ms, "gd-user3", "carol@example.com", "monorepo", "crew")
	ctx := oidcCtx("carol@example.com", []string{RoleOperator})

	// Agent bead with project in fields, not labels.
	fields := json.RawMessage(`{"project":"gardenio","role":"crew","agent":"test"}`)
	labels := labelsForAuthz(nil, fields)
	denial := srv.authorizeWrite(ctx, "create", "agent", labels)
	if denial == nil {
		t.Fatal("should deny write when project comes from fields and user lacks access")
	}
}

func TestAuthorizeWrite_NoWritePolicy(t *testing.T) {
	srv, _, _ := testCtx(t)
	// Task type has no write_policy — should pass for any operator.
	ctx := oidcCtx("operator@example.com", []string{RoleOperator})

	denial := srv.authorizeWrite(ctx, "create", "task", nil)
	if denial != nil {
		t.Fatalf("task type has no write policy, should allow, got: %v", denial)
	}
}

func TestAuthorizeWrite_NoProjectLabel(t *testing.T) {
	srv, ms, _ := testCtx(t)
	makeUserBead(ms, "gd-user4", "dave@example.com", "gardenio", "crew")
	ctx := oidcCtx("dave@example.com", []string{RoleOperator})

	// Agent type is project_scoped but no project label — skip project check.
	denial := srv.authorizeWrite(ctx, "create", "agent", nil)
	if denial != nil {
		t.Fatalf("should allow when no project label present, got: %v", denial)
	}
}

func TestAuthorizeWriteForBead(t *testing.T) {
	srv, ms, _ := testCtx(t)
	makeUserBead(ms, "gd-user5", "eve@example.com", "monorepo", "crew")
	ctx := oidcCtx("eve@example.com", []string{RoleOperator})

	bead := &model.Bead{
		ID:     "gd-test1",
		Type:   "agent",
		Kind:   "config",
		Labels: []string{"project:gardenio"},
	}

	denial := srv.authorizeWriteForBead(ctx, "update", bead)
	if denial == nil {
		t.Fatal("should deny update on agent bead for unauthorized project")
	}
	if denial.Action != "update" {
		t.Errorf("denial.Action=%q, want update", denial.Action)
	}
}

func TestAuthorizeWrite_WildcardProject(t *testing.T) {
	srv, ms, _ := testCtx(t)
	makeUserBead(ms, "gd-user6", "frank@example.com", "*", "")
	ctx := oidcCtx("frank@example.com", []string{RoleOperator})

	denial := srv.authorizeWrite(ctx, "create", "agent", []string{"project:any-project"})
	if denial != nil {
		t.Fatalf("wildcard project should allow any project, got: %v", denial)
	}
}

func TestHandleEscalation_CreatesBeadOn202(t *testing.T) {
	_, ms, h := newTestServer()
	makeUserBead(ms, "gd-user-esc", "bob@example.com", "monorepo", "crew")

	// Create an agent-type bead that bob doesn't have project access to.
	// We need to inject OIDC auth context — since the test server has no auth,
	// requests go through as static token (bypass). Instead, test via a direct
	// HTTP request with X-Escalate header and verify the escalation flow.
	//
	// With no auth middleware, the server treats requests as static token which
	// bypasses write policy. So we test the 403 → escalation path indirectly
	// via the unit-level authorizeWrite tests above. Here we verify the
	// handleEscalation mechanics by testing the create endpoint with an
	// already-denied type (user type requires admin).

	// For coverage: test that a normal create (no OIDC) succeeds.
	rec := doJSON(t, h, "POST", "/v1/beads", map[string]any{
		"title":      "Test user bead",
		"type":       "user",
		"created_by": "admin@example.com",
	})
	requireStatus(t, rec, 201)

	// Verify the escalation bead was NOT created (no denial for static token).
	escalationCount := 0
	for _, b := range ms.beads {
		if b.Type == "escalation" {
			escalationCount++
		}
	}
	if escalationCount != 0 {
		t.Errorf("expected 0 escalation beads for static token, got %d", escalationCount)
	}
}

func TestEscalationResponse_Fields(t *testing.T) {
	resp := EscalationResponse{
		EscalationID: "gd-esc-test",
		Message:      "pending",
		DeniedAction: "create",
		BeadType:     "agent",
	}
	data, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]string
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed["escalation_id"] != "gd-esc-test" {
		t.Errorf("escalation_id=%q, want gd-esc-test", parsed["escalation_id"])
	}
	if parsed["denied_action"] != "create" {
		t.Errorf("denied_action=%q, want create", parsed["denied_action"])
	}
}

func staticCtx() context.Context {
	return withAuthMethod(context.Background(), authMethodStatic)
}

// --- Advisory text template tests ---

func TestPopulateAdvisoryText_DefaultTemplate(t *testing.T) {
	srv, _, _ := testCtx(t)
	d := &AuthzDenial{
		Action:      "create",
		BeadType:    "agent",
		Escalatable: false,
	}
	srv.populateAdvisoryText(context.Background(), d, "Requires role admin.")

	if d.AdvisoryText == "" {
		t.Fatal("advisory text should not be empty")
	}
	if !strings.Contains(d.AdvisoryText, "create") {
		t.Error("should contain the action")
	}
	if !strings.Contains(d.AdvisoryText, "agent") {
		t.Error("should contain the bead type")
	}
	if !strings.Contains(d.AdvisoryText, "Requires role admin.") {
		t.Error("should contain the reason")
	}
	// Non-escalatable: should NOT contain --escalate hint.
	if strings.Contains(d.AdvisoryText, "--escalate") {
		t.Error("non-escalatable denial should not mention --escalate")
	}
}

func TestPopulateAdvisoryText_AllPlaceholders(t *testing.T) {
	srv, _, _ := testCtx(t)
	d := &AuthzDenial{
		Action:      "update",
		BeadType:    "config",
		Escalatable: true,
	}
	srv.populateAdvisoryText(context.Background(), d, "Not authorized for project gardenio.")

	if !strings.Contains(d.AdvisoryText, "update") {
		t.Error("should substitute {action}")
	}
	if !strings.Contains(d.AdvisoryText, "config") {
		t.Error("should substitute {type}")
	}
	if !strings.Contains(d.AdvisoryText, "Not authorized for project gardenio.") {
		t.Error("should substitute {reason}")
	}
	if !strings.Contains(d.AdvisoryText, "--escalate") {
		t.Error("escalatable denial should include --escalate hint")
	}
	// No unresolved placeholders should remain.
	for _, ph := range []string{"{action}", "{type}", "{reason}", "{escalate_hint}"} {
		if strings.Contains(d.AdvisoryText, ph) {
			t.Errorf("unresolved placeholder %q in advisory text: %s", ph, d.AdvisoryText)
		}
	}
}

func TestPopulateAdvisoryText_EmptyTemplate_FallsBack(t *testing.T) {
	srv, ms, _ := testCtx(t)
	// Set an empty template — should fall back to default.
	ms.configs["authz-denied-text"] = &model.Config{
		Key:   "authz-denied-text",
		Value: json.RawMessage(`{"template":""}`),
	}

	d := &AuthzDenial{
		Action:      "delete",
		BeadType:    "formula",
		Escalatable: false,
	}
	srv.populateAdvisoryText(context.Background(), d, "Permission denied.")

	if d.AdvisoryText == "" {
		t.Fatal("should fall back to default template, not produce empty text")
	}
	if !strings.Contains(d.AdvisoryText, "delete") {
		t.Error("fallback template should substitute {action}")
	}
}

func TestPopulateAdvisoryText_MalformedConfig_FallsBack(t *testing.T) {
	srv, ms, _ := testCtx(t)
	// Set malformed JSON — should fall back to default.
	ms.configs["authz-denied-text"] = &model.Config{
		Key:   "authz-denied-text",
		Value: json.RawMessage(`not-json`),
	}

	d := &AuthzDenial{
		Action:      "create",
		BeadType:    "task",
		Escalatable: false,
	}
	srv.populateAdvisoryText(context.Background(), d, "Bad config.")

	if d.AdvisoryText == "" {
		t.Fatal("should fall back to default on malformed config")
	}
	if !strings.Contains(d.AdvisoryText, "create") {
		t.Error("fallback should still substitute placeholders")
	}
}

func TestPopulateAdvisoryText_CustomTemplate_MissingPlaceholders(t *testing.T) {
	srv, ms, _ := testCtx(t)
	// Custom template that only uses {action} — other placeholders are absent.
	ms.configs["authz-denied-text"] = &model.Config{
		Key:   "authz-denied-text",
		Value: json.RawMessage(`{"template":"Denied: {action}. Contact admin."}`),
	}

	d := &AuthzDenial{
		Action:      "create",
		BeadType:    "agent",
		Escalatable: true,
	}
	srv.populateAdvisoryText(context.Background(), d, "Some reason.")

	if !strings.Contains(d.AdvisoryText, "Denied: create.") {
		t.Errorf("should use custom template, got: %s", d.AdvisoryText)
	}
	// Since {reason} and {escalate_hint} aren't in the template, they should
	// not appear (no leftover placeholders, no injected content).
	if strings.Contains(d.AdvisoryText, "Some reason.") {
		t.Error("reason should not appear when {reason} placeholder is absent from template")
	}
}

func TestPopulateAdvisoryText_EscalateHint_ContainsAction(t *testing.T) {
	srv, _, _ := testCtx(t)
	d := &AuthzDenial{
		Action:      "close",
		BeadType:    "agent",
		Escalatable: true,
	}
	srv.populateAdvisoryText(context.Background(), d, "Not allowed.")

	// The escalate hint itself should contain the action (kd close ... --escalate).
	if !strings.Contains(d.AdvisoryText, "kd close") {
		t.Errorf("escalate hint should mention 'kd close', got: %s", d.AdvisoryText)
	}
}

// TestAuthorizeWrite_HardenedTypes_OperatorDenied asserts that an OIDC operator
// cannot write any of the high-leverage types newly policied in B6
// (ka-0000001888). Admin-gated types must deny regardless of project; project-
// scoped types deny when the user lacks access to the bead's project label.
func TestAuthorizeWrite_HardenedTypes_OperatorDenied(t *testing.T) {
	srv, ms, _ := testCtx(t)
	// Operator user scoped only to project "monorepo".
	makeUserBead(ms, "gd-user-op", "op@example.com", "monorepo", "crew")
	ctx := oidcCtx("op@example.com", []string{RoleOperator})

	cases := []struct {
		beadType    model.BeadType
		labels      []string
		wantRole    string
		escalatable bool
	}{
		// Admin-gated types (no project scope).
		{"role", nil, RoleAdmin, false},
		{"rig", nil, RoleAdmin, false},
		{"convoy", nil, RoleAdmin, false},
		{"config", nil, RoleAdmin, false},
		{"audit", nil, RoleAdmin, false},
		{"jira-poller", nil, RoleAdmin, false},
		// Admin-gated but escalatable.
		{"advice", nil, RoleAdmin, true},
		{"report", nil, RoleAdmin, true},
		// Project-scoped escalatable — target a project the user lacks access to.
		{"decision", []string{"project:gardenio"}, "", true},
		{"gate", []string{"project:gardenio"}, "", true},
		{"formula", []string{"project:gardenio"}, "", true},
		{"trigger", []string{"project:gardenio"}, "", true},
	}

	for _, tc := range cases {
		t.Run(string(tc.beadType), func(t *testing.T) {
			d := srv.authorizeWrite(ctx, "create", tc.beadType, tc.labels)
			if d == nil {
				t.Fatalf("operator should be denied on type %q", tc.beadType)
			}
			if tc.wantRole != "" && d.RequiredRole != tc.wantRole {
				t.Errorf("RequiredRole=%q, want %q", d.RequiredRole, tc.wantRole)
			}
			if d.Escalatable != tc.escalatable {
				t.Errorf("Escalatable=%v, want %v", d.Escalatable, tc.escalatable)
			}
			if d.AdvisoryText == "" {
				t.Error("denial should include advisory text")
			}
		})
	}
}

// TestAuthorizeWrite_HardenedTypes_AdminAllowed asserts that an admin
// bypasses the write_policy for every type newly policied in B6.
func TestAuthorizeWrite_HardenedTypes_AdminAllowed(t *testing.T) {
	srv, _, _ := testCtx(t)
	ctx := oidcCtx("admin@example.com", []string{RoleAdmin})

	types := []model.BeadType{
		"role", "rig", "convoy", "config", "audit", "jira-poller",
		"advice", "report",
		"decision", "gate", "formula", "trigger",
	}

	for _, bt := range types {
		t.Run(string(bt), func(t *testing.T) {
			if d := srv.authorizeWrite(ctx, "create", bt, []string{"project:gardenio"}); d != nil {
				t.Fatalf("admin should be allowed on type %q, got: %v", bt, d)
			}
		})
	}
}

// TestAuthorizeWrite_HardenedTypes_ProjectScoped_AuthorizedAllowed asserts
// that operators WITH access to the target project can write the
// project-scoped hardened types (decision/gate/formula/trigger).
func TestAuthorizeWrite_HardenedTypes_ProjectScoped_AuthorizedAllowed(t *testing.T) {
	srv, ms, _ := testCtx(t)
	makeUserBead(ms, "gd-user-gb-op", "gb-op@example.com", "gardenio", "crew")
	ctx := oidcCtx("gb-op@example.com", []string{RoleOperator})

	types := []model.BeadType{"decision", "gate", "formula", "trigger"}
	for _, bt := range types {
		t.Run(string(bt), func(t *testing.T) {
			if d := srv.authorizeWrite(ctx, "create", bt, []string{"project:gardenio"}); d != nil {
				t.Fatalf("authorized operator should write %q in own project, got: %v", bt, d)
			}
		})
	}
}
