package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"gardenio/kbeads/internal/model"
)

// Land steward delegation (escalation-routing D8, Task 11) on the scoped
// fixture: land claimlayer is a tenant land with a steward and a gardener,
// each on its own per-agent token. tokScopedOwner owns claimlayer.

const (
	tokClStewardAgent  = "cl-steward-1-tok"
	tokClGardenerAgent = "cl-gardener-1-tok"
	tokAcmeSteward     = "acme-steward-1-tok"
	tokAcmeGardener    = "acme-gardener-1-tok"
)

// seedTenantLand stores land's project bead with land_kind=tenant and, when
// approves is non-nil, a land_steward_approves list (seeded as the endpoint
// would leave it, past the PATCH guard).
func (f *scopedFixture) seedTenantLand(land string, approves []string) {
	fields := map[string]any{"land_kind": "tenant"}
	if approves != nil {
		fields[landStewardApprovesField] = approves
	}
	raw, _ := json.Marshal(fields)
	id := "proj-" + land
	f.seedTyped(id, model.TypeProject, "")
	f.ms.beads[id].Title = land
	f.ms.beads[id].Kind = model.KindConfig
	f.ms.beads[id].Fields = raw
}

// seedLandAgent seeds an agent bead with role and project, and registers its
// per-agent token (the fleet name carries the bead id, agentBeadIDFromContext).
// A steward or lands-gardener role bead holds the lands-tenant tier, as live.
func (f *scopedFixture) seedLandAgent(id, role, land, tok string) {
	if role == landsStewardRole || role == "lands-gardener" {
		makeTierRoleBead(f.ms, "gd-role-"+role, role, `{"capability_tiers":["lands-tenant"]}`)
	}
	seedAgent(f.ms, id, role, land)
	f.fc.RegisterTokenFull(tok, agentFleetPrefix+id, land, nil, nil, time.Time{}, id, false)
}

// seedStewardLands seeds claimlayer and acme as tenant lands, each with a
// steward and a gardener.
func (f *scopedFixture) seedStewardLands(claimlayerApproves []string) {
	f.seedTenantLand("claimlayer", claimlayerApproves)
	f.seedTenantLand("acme", []string{"github_pr_merge"})
	f.seedLandAgent("cl-steward-1", "steward", "claimlayer", tokClStewardAgent)
	f.seedLandAgent("cl-gardener-1", "lands-gardener", "claimlayer", tokClGardenerAgent)
	f.seedLandAgent("acme-steward-1", "steward", "acme", tokAcmeSteward)
	f.seedLandAgent("acme-gardener-1", "lands-gardener", "acme", tokAcmeGardener)
}

// fileEscalation files an escalation in land through the daemon's create path
// as tok, so the server stamps requesting_agent and the route. It returns the
// new escalation's id.
func (f *scopedFixture) fileEscalation(t *testing.T, tok, land string, fields map[string]any) string {
	t.Helper()
	if _, ok := fields["operation_spec"]; !ok {
		f.filed++
		fields["operation_spec"] = map[string]any{"pr": fmt.Sprintf("#%d", f.filed)}
	}
	code, body := f.do(t, tok, http.MethodPost, "/v1/beads", map[string]any{
		"title": "merge PR #1", "type": "escalation", "kind": "data",
		"labels": []string{"project:" + land}, "fields": fields,
	})
	if code != http.StatusCreated {
		t.Fatalf("file escalation in %s as %s: %d %s", land, tok, code, body)
	}
	var b model.Bead
	if err := json.Unmarshal([]byte(body), &b); err != nil || b.ID == "" {
		t.Fatalf("decode created escalation: %v %s", err, body)
	}
	return b.ID
}

func mergeFields() map[string]any { return map[string]any{"operation_type": "github_pr_merge"} }

func (f *scopedFixture) field(t *testing.T, id, key string) string {
	t.Helper()
	v := escField(t, f.ms, id, key)
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	default:
		raw, _ := json.Marshal(x)
		return string(raw)
	}
}

func (f *scopedFixture) delegate(t *testing.T, tok, land string, ops []string) (int, string) {
	t.Helper()
	return f.do(t, tok, http.MethodPost, "/v1/lands/"+land+"/steward-delegation", map[string]any{"operation_types": ops})
}

func (f *scopedFixture) stewardApproves(t *testing.T, land string) []string {
	t.Helper()
	var fields struct {
		Approves []string `json:"land_steward_approves"`
	}
	_ = json.Unmarshal(f.ms.beads["proj-"+land].Fields, &fields)
	return fields.Approves
}

// Review Focus 6. Positive control in the same test: 403 before, 200 after.
func TestStewardApprove_RequiresLandDelegation(t *testing.T) {
	f := newScopedFixture(t)
	f.seedStewardLands(nil)

	esc := f.fileEscalation(t, tokClGardenerAgent, "claimlayer", mergeFields())
	if got := f.field(t, esc, "held_by"); got != "land-owner:claimlayer" {
		t.Fatalf("held_by = %q before delegation, want land-owner:claimlayer", got)
	}
	if got := f.field(t, esc, "audience"); got != "owner" {
		t.Fatalf("audience = %q before delegation, want owner (D11: the land's owner)", got)
	}
	c, b := f.approve(t, tokClStewardAgent, esc)
	f.wantStatus(t, "steward approve before delegation", c, b, http.StatusForbidden)
	requireUnapproved(t, f.ms, esc)

	c, b = f.delegate(t, tokScopedOwner, "claimlayer", []string{"github_pr_merge"})
	f.wantStatus(t, "owner delegates merges", c, b, http.StatusOK)

	esc2 := f.fileEscalation(t, tokClGardenerAgent, "claimlayer", mergeFields())
	if got := f.field(t, esc2, "held_by"); got != "steward:claimlayer" {
		t.Fatalf("held_by after delegation = %q, want steward:claimlayer", got)
	}
	if got := f.field(t, esc2, "audience"); got != "desk" {
		t.Fatalf("audience after delegation = %q, want desk (D11: steward)", got)
	}
	c, b = f.approve(t, tokClStewardAgent, esc2)
	f.wantStatus(t, "steward approve after delegation", c, b, http.StatusOK)
	if f.field(t, esc2, "approval_method") != "coordinator" || f.field(t, esc2, "approved_as") != "steward:claimlayer" {
		t.Fatalf("approval stamp = %q/%q", f.field(t, esc2, "approval_method"), f.field(t, esc2, "approved_as"))
	}
	if got := f.field(t, esc2, "held_by"); got != "requester" {
		t.Fatalf("held_by after approve = %q, want requester (execute_by)", got)
	}
}

func TestStewardApprove_RefusesOwnRequest(t *testing.T) {
	f := newScopedFixture(t)
	f.seedStewardLands([]string{"github_pr_merge"})

	esc := f.fileEscalation(t, tokClStewardAgent, "claimlayer", mergeFields())
	if got := f.field(t, esc, "held_by"); got != "steward:claimlayer" {
		t.Fatalf("held_by = %q, want steward:claimlayer", got)
	}
	c, b := f.approve(t, tokClStewardAgent, esc)
	f.wantStatus(t, "steward approves its own request", c, b, http.StatusForbidden)
	requireUnapproved(t, f.ms, esc)

	// Control: the land owner approves the same escalation.
	c, b = f.approve(t, tokScopedOwner, esc)
	f.wantStatus(t, "land owner approves the steward's request", c, b, http.StatusOK)
	f.wantLandOwnerApproval(t, esc)
}

func TestStewardApprove_RefusesOtherLand(t *testing.T) {
	f := newScopedFixture(t)
	f.seedStewardLands([]string{"github_pr_merge"})

	esc := f.fileEscalation(t, tokAcmeGardener, "acme", mergeFields())
	if got := f.field(t, esc, "held_by"); got != "steward:acme" {
		t.Fatalf("held_by = %q, want steward:acme", got)
	}
	c, b := f.approve(t, tokClStewardAgent, esc)
	f.wantStatus(t, "claimlayer steward approves an acme escalation", c, b, http.StatusNotFound) // another land's: as missing
	requireUnapproved(t, f.ms, esc)

	// Control: acme's own steward.
	c, b = f.approve(t, tokAcmeSteward, esc)
	f.wantStatus(t, "acme steward approves", c, b, http.StatusOK)
}

func TestStewardApprove_RefusesOperatorOnly(t *testing.T) {
	f := newScopedFixture(t)
	f.seedStewardLands([]string{"github_pr_merge"})

	secret := mergeFields()
	secret["sensitivity"] = "secret"
	esc := f.fileEscalation(t, tokClGardenerAgent, "claimlayer", secret)
	for k, want := range map[string]string{"operator_only": "true", "held_by": "operator", "audience": "owner"} {
		if got := f.field(t, esc, k); got != want {
			t.Fatalf("%s = %q on a secret merge, want %q (D3, the one floor)", k, got, want)
		}
	}
	c, b := f.approve(t, tokClStewardAgent, esc)
	f.wantStatus(t, "steward approves an operator-only merge", c, b, http.StatusForbidden)
	requireUnapproved(t, f.ms, esc)

	// Control: the same steward on a non-secret merge.
	ok := f.fileEscalation(t, tokClGardenerAgent, "claimlayer", mergeFields())
	c, b = f.approve(t, tokClStewardAgent, ok)
	f.wantStatus(t, "steward approves a plain merge", c, b, http.StatusOK)
}

func TestLandOwnerApprove_TenantMerge(t *testing.T) {
	f := newScopedFixture(t)
	f.seedStewardLands(nil)

	esc := f.fileEscalation(t, tokClGardenerAgent, "claimlayer", mergeFields())
	// Refusal sibling: a member of the land, not its owner.
	c, b := f.approve(t, tokScopedWriter, esc)
	f.wantStatus(t, "member approves a tenant merge", c, b, http.StatusForbidden)
	requireUnapproved(t, f.ms, esc)

	c, b = f.approve(t, tokScopedOwner, esc)
	f.wantStatus(t, "land owner approves a tenant merge", c, b, http.StatusOK)
	f.wantLandOwnerApproval(t, esc)
}

// wantLandOwnerApproval checks a land owner's approval on the human path,
// which Task 4 left unchanged: approved_by is the owner and approved_as stays
// unset (approved_as names a coordinator or steward lane). held_by moved to
// execute_by, the requester that merges (D7 rule 2).
func (f *scopedFixture) wantLandOwnerApproval(t *testing.T, esc string) {
	t.Helper()
	for k, want := range map[string]string{"approved_by": "clo@example.com", "approved_as": "", "held_by": "requester"} {
		if got := f.field(t, esc, k); got != want {
			t.Errorf("%s = %q after the land owner's approval, want %q", k, got, want)
		}
	}
	if got := f.escalationStatus(esc); got != model.StatusInProgress {
		t.Errorf("status = %s after approval, want in_progress", got)
	}
}

// The carve-out needs the land-owner lane in the stamped approvers: an owner
// is still refused a merge whose approvers do not name it.
func TestLandOwnerApprove_NeedsLandOwnerLane(t *testing.T) {
	f := newScopedFixture(t)
	f.seedStewardLands(nil)
	f.seedEscalation("cl-desk-merge", "project:claimlayer", "github_pr_merge", map[string]any{"pr": "#1"},
		map[string]any{"approvers": []string{"coordinator:merge-desk", "operator"}, "held_by": "coordinator:merge-desk"})
	c, b := f.approve(t, tokScopedOwner, "cl-desk-merge")
	f.wantStatus(t, "owner approves a merge not routed to land-owner", c, b, http.StatusForbidden)
	requireUnapproved(t, f.ms, "cl-desk-merge")

	// Control: the same escalation, approvers naming the land's owner lane.
	f.seedEscalation("cl-owner-merge", "project:claimlayer", "github_pr_merge", map[string]any{"pr": "#1"},
		map[string]any{"approvers": []string{"steward:claimlayer", "land-owner:claimlayer"}, "held_by": "land-owner:claimlayer"})
	c, b = f.approve(t, tokScopedOwner, "cl-owner-merge")
	f.wantStatus(t, "owner approves a land-owner merge", c, b, http.StatusOK)
}

func TestStewardDelegation_OwnerOnly(t *testing.T) {
	f := newScopedFixture(t)
	f.seedStewardLands(nil)
	f.seedTenantLand("other", nil)
	ops := []string{"github_pr_merge"}

	for _, c := range []struct{ name, tok, land string }{
		{"member", tokScopedWriter, "claimlayer"},
		{"guest", tokScopedGuest, "claimlayer"},
		{"land principal", tokLandClaimlayer, "claimlayer"},
		{"owner of another land", tokScopedOwner, "other"},
		{"steward", tokClStewardAgent, "claimlayer"},
		{"operator (not admin)", tokOperator, "claimlayer"},
		{"fleet bridge", tokFleetBridge, "claimlayer"},
		{"shared static token", scopedAdminToken, "claimlayer"},
	} {
		code, body := f.delegate(t, c.tok, c.land, ops)
		f.wantStatus(t, c.name+" delegates", code, body, http.StatusForbidden)
	}
	if got := f.stewardApproves(t, "claimlayer"); got != nil {
		t.Fatalf("land_steward_approves = %v after refused writes", got)
	}
	if got := f.stewardApproves(t, "other"); got != nil {
		t.Fatalf("other's land_steward_approves = %v after a refused write", got)
	}

	// Controls: the land's owner, and a gardenio-admin.
	code, body := f.delegate(t, tokScopedOwner, "claimlayer", ops)
	f.wantStatus(t, "owner delegates", code, body, http.StatusOK)
	if got := f.stewardApproves(t, "claimlayer"); !reflect.DeepEqual(got, ops) {
		t.Fatalf("land_steward_approves = %v, want %v", got, ops)
	}
	code, body = f.delegate(t, tokOwnerAdmin, "other", ops)
	f.wantStatus(t, "admin delegates", code, body, http.StatusOK)
}

func TestStewardDelegation_RejectsFloorTypes(t *testing.T) {
	f := newScopedFixture(t)
	f.seedStewardLands(nil)

	for _, ops := range [][]string{
		{"github_pr_merge", "credential_rotation"},
		{"secret_write"},
		{"credential_x"},
		{"escalation_policy_write"},
		{"github_pr_merge", "github_pr_merge"},
	} {
		code, body := f.delegate(t, tokScopedOwner, "claimlayer", ops)
		f.wantStatus(t, fmt.Sprintf("delegate %q", ops), code, body, http.StatusBadRequest)
	}
	if got := f.stewardApproves(t, "claimlayer"); got != nil {
		t.Fatalf("land_steward_approves = %v after refused writes", got)
	}

	// Control.
	code, body := f.delegate(t, tokScopedOwner, "claimlayer", []string{"github_pr_merge", "gitlab_mr_merge"})
	f.wantStatus(t, "delegate both merges", code, body, http.StatusOK)
	if got := f.stewardApproves(t, "claimlayer"); !slices.Equal(got, []string{"github_pr_merge", "gitlab_mr_merge"}) {
		t.Fatalf("land_steward_approves = %v", got)
	}
}

func TestStewardDelegation_RejectsUnknownType(t *testing.T) {
	f := newScopedFixture(t)
	f.seedStewardLands(nil)

	// Unknown, scoped-unapprovable, and not normalized.
	for _, op := range []string{"drop_database", "helm_deploy", "grant", " GitHub_PR_Merge "} {
		code, body := f.delegate(t, tokScopedOwner, "claimlayer", []string{op})
		f.wantStatus(t, "delegate "+op, code, body, http.StatusBadRequest)
	}
	if got := f.stewardApproves(t, "claimlayer"); got != nil {
		t.Fatalf("land_steward_approves = %v after refused writes", got)
	}
	// Control: the scoped-approvable replays are delegable.
	code, body := f.delegate(t, tokScopedOwner, "claimlayer", []string{"create", "update", "close"})
	f.wantStatus(t, "delegate create/update/close", code, body, http.StatusOK)
}

func TestStewardDelegation_TenantLandsOnly(t *testing.T) {
	f := newScopedFixture(t)
	f.seedTyped("proj-gardenio", model.TypeProject, "")
	f.ms.beads["proj-gardenio"].Title = "gardenio"
	f.ms.beads["proj-gardenio"].Fields = json.RawMessage(`{"land_kind":"operator"}`)

	code, body := f.delegate(t, tokOwnerAdmin, "gardenio", []string{"github_pr_merge"})
	f.wantStatus(t, "delegate on an operator land", code, body, http.StatusBadRequest)
	code, body = f.delegate(t, tokOwnerAdmin, "nosuchland", []string{"github_pr_merge"})
	f.wantStatus(t, "delegate on a missing land", code, body, http.StatusNotFound)

	// Control: a tenant land.
	f.seedTenantLand("claimlayer", nil)
	code, body = f.delegate(t, tokOwnerAdmin, "claimlayer", []string{"github_pr_merge"})
	f.wantStatus(t, "delegate on a tenant land", code, body, http.StatusOK)
}

// A revoke is an empty list; it does not re-route open escalations, but the
// steward's next approve is refused at once (membership is read at approve
// time).
func TestStewardDelegation_RevokeRefusesNextApprove(t *testing.T) {
	f := newScopedFixture(t)
	f.seedStewardLands([]string{"github_pr_merge"})

	esc := f.fileEscalation(t, tokClGardenerAgent, "claimlayer", mergeFields())
	if got := f.field(t, esc, "held_by"); got != "steward:claimlayer" {
		t.Fatalf("held_by = %q, want steward:claimlayer", got)
	}
	code, body := f.delegate(t, tokScopedOwner, "claimlayer", []string{})
	f.wantStatus(t, "owner revokes", code, body, http.StatusOK)
	if got := f.stewardApproves(t, "claimlayer"); got == nil || len(got) != 0 {
		t.Fatalf("land_steward_approves = %v after revoke, want []", got)
	}
	code, body = f.approve(t, tokClStewardAgent, esc)
	f.wantStatus(t, "steward approve after revoke", code, body, http.StatusForbidden)
	requireUnapproved(t, f.ms, esc)

	// Control: re-grant, and the same escalation is approvable again.
	code, body = f.delegate(t, tokScopedOwner, "claimlayer", []string{"github_pr_merge"})
	f.wantStatus(t, "owner re-grants", code, body, http.StatusOK)
	code, body = f.approve(t, tokClStewardAgent, esc)
	f.wantStatus(t, "steward approve after re-grant", code, body, http.StatusOK)
}

func TestLandStewardApprovesField_NotWritableByPatch(t *testing.T) {
	f := newScopedFixture(t)
	registerUntrustedInput(t, f.ms)
	f.seedTenantLand("claimlayer", nil)
	id := "proj-claimlayer"

	code, body := f.do(t, tokOwnerAdmin, http.MethodPatch, "/v1/beads/"+id, map[string]any{"fields": map[string]any{
		landStewardApprovesField: []string{"github_pr_merge"},
	}})
	f.wantStatus(t, "admin PATCH", code, body, http.StatusBadRequest)
	if !strings.Contains(body, "steward-delegation") {
		t.Errorf("admin PATCH: body %s does not name the endpoint", body)
	}
	// A case variant, which landDelegatesToSteward's decode would read, is
	// refused too (the type check refuses case-folded names first).
	code, body = f.do(t, tokOwnerAdmin, http.MethodPatch, "/v1/beads/"+id, map[string]any{"fields": map[string]any{
		"Land_Steward_Approves": []string{"github_pr_merge"},
	}})
	f.wantStatus(t, "admin PATCH case variant", code, body, http.StatusBadRequest)
	code, body = f.do(t, tokOwnerAdmin, http.MethodPost, "/v1/beads/"+id+"/fields/"+landStewardApprovesField+"/append",
		map[string]any{"value": "github_pr_merge"})
	f.wantStatus(t, "admin field append", code, body, http.StatusBadRequest)
	code, body = f.do(t, tokOwnerAdmin, http.MethodPost, "/v1/beads", map[string]any{
		"type": "project", "title": "newland", "labels": []string{"project:gardenio"},
		"fields": map[string]any{"land_kind": "tenant", landStewardApprovesField: []string{"github_pr_merge"}},
	})
	f.wantStatus(t, "admin create with the field", code, body, http.StatusBadRequest)
	if got := f.stewardApproves(t, "claimlayer"); got != nil {
		t.Fatalf("land_steward_approves = %v after refused writes", got)
	}

	// Control: the same admin PATCHes another project field.
	code, body = f.do(t, tokOwnerAdmin, http.MethodPatch, "/v1/beads/"+id, map[string]any{"fields": map[string]any{"budget_usd": 10}})
	f.wantStatus(t, "admin PATCH budget_usd", code, body, http.StatusOK)

	// A PATCH that carries the stored value unchanged (read-modify-write) is
	// not a write of the field.
	code, body = f.delegate(t, tokOwnerAdmin, "claimlayer", []string{"github_pr_merge"})
	f.wantStatus(t, "admin delegates", code, body, http.StatusOK)
	code, body = f.do(t, tokOwnerAdmin, http.MethodPatch, "/v1/beads/"+id, map[string]any{"fields": map[string]any{
		"budget_usd": 20, landStewardApprovesField: []string{"github_pr_merge"},
	}})
	f.wantStatus(t, "admin PATCH carrying the unchanged list", code, body, http.StatusOK)
}

func TestEscalationFacts_LandOnlyForTenantKind(t *testing.T) {
	ms := newMockStore()
	s := NewBeadsServer(ms, nil)
	esc := &model.Bead{ID: "e1", Type: model.TypeEscalation, Labels: []string{"project:claimlayer"},
		Fields: json.RawMessage(`{"operation_type":"github_pr_merge"}`)}
	ctx := context.Background()

	ms.beads["proj-claimlayer"] = &model.Bead{ID: "proj-claimlayer", Type: model.TypeProject, Title: "claimlayer", Status: model.StatusOpen,
		Fields: json.RawMessage(`{"land_kind":"operator","land_default_channel":"ch-general"}`)}
	if got := s.escalationFactsFor(ctx, esc).Land; got != "" {
		t.Fatalf("operator land with land_default_channel: Land = %q, want empty", got)
	}
	if r := evaluateEscalationPolicy(defaultEscalationPolicy(), s.escalationFactsFor(ctx, esc)); r.Rule == "escalation#2" {
		t.Fatalf("operator land matched the tenant-land rule: %+v", r)
	}

	// Control: the same project as a tenant land.
	ms.beads["proj-claimlayer"].Fields = json.RawMessage(`{"land_kind":"tenant","land_default_channel":"ch-general"}`)
	if got := s.escalationFactsFor(ctx, esc).Land; got != "claimlayer" {
		t.Fatalf("tenant land: Land = %q, want claimlayer", got)
	}
}

// A real steward holds the lands-tenant tier, which refuses every escalation
// verb (Track B §5.1); the amended row lets it approve a merge in its own land
// and nothing else.
func TestStewardApprove_TenantTierCarveOut(t *testing.T) {
	f := newScopedFixture(t)
	f.seedStewardLands([]string{"github_pr_merge", "create"})
	for _, role := range []string{"steward", "lands-gardener"} {
		raw, _ := json.Marshal(map[string]any{model.RoleCapabilityTiersField: []string{model.CapabilityTierLandsTenant}})
		f.ms.beads["role-"+role] = &model.Bead{ID: "role-" + role, Type: "role", Kind: model.KindConfig, Title: role, Status: model.StatusOpen, Fields: raw}
	}
	// Filed as the operator's tooling would before 10.1 lets a gardener file:
	// stamped as the create path stamps a gardener's merge.
	routed := func(id, op string) {
		f.seedEscalation(id, "project:claimlayer", op, map[string]any{"pr": "#1"}, map[string]any{
			"approvers": []string{"steward:claimlayer", "land-owner:claimlayer"}, "held_by": "steward:claimlayer",
			"execute_by": "requester", "operator_only": false, "policy_rule": "escalation#2",
			"requesting_agent": "cl-gardener-1", "requesting_agent_verified": "true",
		})
	}
	routed("cl-merge-rej", "github_pr_merge")
	code, body := f.reject(t, tokClStewardAgent, "cl-merge-rej")
	f.wantStatus(t, "tenant steward rejects", code, body, http.StatusForbidden)

	routed("cl-create", "create")
	code, body = f.approve(t, tokClStewardAgent, "cl-create")
	f.wantStatus(t, "tenant steward approves a non-merge", code, body, http.StatusForbidden)
	requireUnapproved(t, f.ms, "cl-create")

	// The gardener is tenant-tier and not a steward.
	routed("cl-merge-gard", "github_pr_merge")
	code, body = f.approve(t, tokClGardenerAgent, "cl-merge-gard")
	f.wantStatus(t, "tenant gardener approves", code, body, http.StatusForbidden)
	requireUnapproved(t, f.ms, "cl-merge-gard")

	// Control: the tenant-tier steward approves the delegated merge.
	routed("cl-merge", "github_pr_merge")
	code, body = f.approve(t, tokClStewardAgent, "cl-merge")
	f.wantStatus(t, "tenant steward approves a delegated merge", code, body, http.StatusOK)
	if got := f.field(t, "cl-merge", "approved_as"); got != "steward:claimlayer" {
		t.Fatalf("approved_as = %q", got)
	}
}

// createLand creates land's project bead through the daemon's create path as
// an admin, as the provisioner does, and returns its fields.
func (f *scopedFixture) createLand(t *testing.T, land string, fields map[string]any) map[string]json.RawMessage {
	t.Helper()
	if fields["land_kind"] == landKindTenant {
		registerUntrustedInput(t, f.ms)
	}
	code, body := f.do(t, tokOwnerAdmin, http.MethodPost, "/v1/beads", map[string]any{
		"type": "project", "kind": "config", "title": land, "labels": []string{"project:" + land}, "fields": fields,
	})
	f.wantStatus(t, "create land "+land, code, body, http.StatusCreated)
	var b model.Bead
	if err := json.Unmarshal([]byte(body), &b); err != nil {
		t.Fatalf("decode created land: %v %s", err, body)
	}
	got := map[string]json.RawMessage{}
	_ = json.Unmarshal(f.ms.beads[b.ID].Fields, &got)
	return got
}

// Owner decision gd-fBSlZfEvmo: a new tenant land delegates its PR merges to
// its steward. No creator can supply the key (the create guard refuses it),
// so the daemon stamps it; an operator land and a plain project get none.
func TestTenantLandCreate_StampsStewardApprovesDefault(t *testing.T) {
	f := newScopedFixture(t)

	got := f.createLand(t, "newland", map[string]any{"land_kind": "tenant", "land_slack_mirror": "off"})
	if v := string(got[landStewardApprovesField]); v != `["github_pr_merge"]` {
		t.Fatalf("tenant land land_steward_approves = %s, want [\"github_pr_merge\"]", v)
	}
	if err := validateLandStewardApproves(defaultLandStewardApproves); err != nil {
		t.Fatalf("the default is not a list the owner endpoint accepts: %v", err)
	}

	for land, fields := range map[string]map[string]any{
		"oplandx":  {"land_kind": "operator"},
		"plainprj": {},
	} {
		if v, ok := f.createLand(t, land, fields)[landStewardApprovesField]; ok {
			t.Errorf("%s: land_steward_approves = %s, want no key", land, v)
		}
	}
}

// Acceptance for gd-oIa8qydN1w: on a land created with the default, a
// lands-gardener's merge escalation is the steward's to approve (method
// coordinator, as steward:<land>) and is refused for another land's steward
// and for the land principal, whose approve is a 403.
func TestTenantLandDefault_StewardApprovesMerges(t *testing.T) {
	f := newScopedFixture(t)
	f.createLand(t, "claimlayer", map[string]any{"land_kind": "tenant"})
	f.seedTenantLand("acme", nil)
	f.seedLandAgent("cl-steward-1", "steward", "claimlayer", tokClStewardAgent)
	f.seedLandAgent("cl-gardener-1", "lands-gardener", "claimlayer", tokClGardenerAgent)
	f.seedLandAgent("acme-steward-1", "steward", "acme", tokAcmeSteward)

	esc := f.fileEscalation(t, tokClGardenerAgent, "claimlayer", mergeFields())
	if got := f.field(t, esc, "held_by"); got != "steward:claimlayer" {
		t.Fatalf("held_by = %q, want steward:claimlayer", got)
	}
	canApprove := func(tok string) (int, map[string]any) {
		t.Helper()
		code, body := f.do(t, tok, http.MethodPost, "/v1/escalations/"+esc+"/can-approve", map[string]any{})
		var r map[string]any
		_ = json.Unmarshal([]byte(body), &r)
		return code, r
	}
	if code, r := canApprove(tokClStewardAgent); code != http.StatusOK || r["allowed"] != true || r["method"] != "coordinator" || r["as"] != "steward:claimlayer" {
		t.Fatalf("own steward can-approve = %d %v, want allowed as steward:claimlayer by coordinator", code, r)
	}
	// Another land's steward is refused: its token does not reach the bead's
	// project (403), or the dry run says no.
	if code, r := canApprove(tokAcmeSteward); code == http.StatusOK && r["allowed"] == true {
		t.Fatalf("acme's steward can-approve = %d %v, want refused", code, r)
	}

	c, b := f.approve(t, tokLandClaimlayer, esc)
	f.wantStatus(t, "land principal approves its land's merge", c, b, http.StatusForbidden)
	c, b = f.approve(t, tokAcmeSteward, esc)
	f.wantStatus(t, "acme steward approves a claimlayer merge", c, b, http.StatusNotFound) // another land's: as missing
	requireUnapproved(t, f.ms, esc)

	// Control: the land's own steward.
	c, b = f.approve(t, tokClStewardAgent, esc)
	f.wantStatus(t, "claimlayer steward approves", c, b, http.StatusOK)
	if f.field(t, esc, "approval_method") != "coordinator" || f.field(t, esc, "approved_as") != "steward:claimlayer" {
		t.Fatalf("approval stamp = %q/%q", f.field(t, esc, "approval_method"), f.field(t, esc, "approved_as"))
	}
}

// The default is a create-time stamp only: an owner's explicit [] survives a
// later PATCH of another field, and the steward stays refused.
func TestTenantLandDefault_ExplicitEmptyKept(t *testing.T) {
	f := newScopedFixture(t)
	f.createLand(t, "claimlayer", map[string]any{"land_kind": "tenant"})
	c, b := f.delegate(t, tokScopedOwner, "claimlayer", []string{})
	f.wantStatus(t, "owner revokes", c, b, http.StatusOK)

	var id string
	for k, bead := range f.ms.beads {
		if bead.Type == model.TypeProject && bead.Title == "claimlayer" {
			id = k
		}
	}
	c, b = f.do(t, tokOwnerAdmin, http.MethodPatch, "/v1/beads/"+id, map[string]any{"fields": map[string]any{"budget_usd": 10}})
	f.wantStatus(t, "admin PATCH budget_usd", c, b, http.StatusOK)
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(f.ms.beads[id].Fields, &fields)
	if v, ok := fields[landStewardApprovesField]; !ok || string(v) != "[]" {
		t.Fatalf("land_steward_approves = %s (present %v) after a PATCH, want the owner's []", v, ok)
	}
}
