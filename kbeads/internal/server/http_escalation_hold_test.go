package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"gardenio/kbeads/internal/model"
)

func TestClaimEscalation_OnlyHolderAndStatusUnchanged(t *testing.T) {
	s, ms := newLaneTestServer()
	seedRoutedEscalation(ms, "gd-c1", "gardenio", mergeDeskEscalation("gd-crew1"))

	// Refusal: a crew agent is not in coordinator:merge-desk.
	rec := callEscalationVerb(t, agentCtx("gd-crew1", "crew-a", "gardenio"), s.handleClaimEscalation, "gd-c1", "claim", map[string]any{})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-holder claim: got %d %s, want 403", rec.Code, rec.Body.String())
	}
	if escField(t, ms, "gd-c1", "claimed_by") != nil {
		t.Fatal("refused claim stamped claimed_by")
	}

	// Positive control: the reviewer holds it.
	rec = callEscalationVerb(t, agentCtx("gd-rev1", "reviewer-a", "gardenio"), s.handleClaimEscalation, "gd-c1", "claim", map[string]any{})
	if rec.Code != http.StatusOK {
		t.Fatalf("holder claim: got %d %s", rec.Code, rec.Body.String())
	}
	if got := escField(t, ms, "gd-c1", "claimed_by"); got != "gd-rev1" {
		t.Fatalf("claimed_by = %v, want the reviewer's agent bead id", got)
	}
	at, _ := escField(t, ms, "gd-c1", "claimed_at").(string)
	if _, err := time.Parse(time.RFC3339, at); err != nil || at[len(at)-1] != 'Z' {
		t.Fatalf("claimed_at = %q, want RFC3339 UTC", at)
	}
	// CONTRACT ISSUE 28: gardenhub reads in_progress as approved.
	if ms.beads["gd-c1"].Status != model.StatusOpen {
		t.Fatalf("claim changed status to %s", ms.beads["gd-c1"].Status)
	}
	if got := escField(t, ms, "gd-c1", "held_by"); got != "coordinator:merge-desk" {
		t.Fatalf("claim changed held_by to %v", got)
	}

	// A second holder cannot steal the claim.
	seedAgent(ms, "gd-rev2", "reviewer", "gardenio")
	rec = callEscalationVerb(t, agentCtx("gd-rev2", "reviewer-b", "gardenio"), s.handleClaimEscalation, "gd-c1", "claim", map[string]any{})
	if rec.Code != http.StatusConflict {
		t.Fatalf("second claim: got %d, want 409", rec.Code)
	}
}

func TestClaimEscalation_UnstampedIsOperatorHeld(t *testing.T) {
	s, ms := newLaneTestServer()
	seedRoutedEscalation(ms, "gd-c2", "gardenio", map[string]any{"operation_type": "helm_deploy"})
	if rec := callEscalationVerb(t, agentCtx("gd-rev1", "reviewer-a", "gardenio"), s.handleClaimEscalation, "gd-c2", "claim", map[string]any{}); rec.Code != http.StatusForbidden {
		t.Fatalf("coordinator claim of an unstamped escalation: got %d, want 403", rec.Code)
	}
	if rec := callEscalationVerb(t, operatorCtx("owner@example.com"), s.handleClaimEscalation, "gd-c2", "claim", map[string]any{}); rec.Code != http.StatusOK {
		t.Fatalf("operator claim of an unstamped escalation: got %d %s, want 200", rec.Code, rec.Body.String())
	}
}

func TestHandUpEscalation(t *testing.T) {
	s, ms := newLaneTestServer()
	seedTenantLand(ms, "acme", nil)
	seedRoutedEscalation(ms, "gd-h1", "gardenio", mergeDeskEscalation("gd-crew1"))

	if rec := callEscalationVerb(t, agentCtx("gd-rev1", "reviewer-a", "gardenio"), s.handleHandUpEscalation, "gd-h1", "hand-up", map[string]any{}); rec.Code != http.StatusBadRequest {
		t.Fatalf("hand-up without reason: got %d, want 400", rec.Code)
	}
	if rec := callEscalationVerb(t, agentCtx("gd-crew1", "crew-a", "gardenio"), s.handleHandUpEscalation, "gd-h1", "hand-up", map[string]any{"reason": "x"}); rec.Code != http.StatusForbidden {
		t.Fatalf("non-holder hand-up: got %d, want 403", rec.Code)
	}
	if got := escField(t, ms, "gd-h1", "held_by"); got != "coordinator:merge-desk" {
		t.Fatalf("refused hand-up moved held_by to %v", got)
	}

	// Positive control.
	rec := callEscalationVerb(t, agentCtx("gd-rev1", "reviewer-a", "gardenio"), s.handleHandUpEscalation, "gd-h1", "hand-up", map[string]any{"reason": "touches deploy-values"})
	if rec.Code != http.StatusOK {
		t.Fatalf("holder hand-up: got %d %s", rec.Code, rec.Body.String())
	}
	for k, want := range map[string]any{
		"held_by": "operator", "audience": "owner", "handed_up_by": "gd-rev1", "hand_up_reason": "touches deploy-values",
		"pingback_source": "hand-up", "pingback_reason": "touches deploy-values", "pingback_count": "1",
	} {
		if got := escField(t, ms, "gd-h1", k); got != want {
			t.Errorf("%s = %v, want %v", k, got, want)
		}
	}
	for _, k := range []string{"handed_up_at", "held_at", "pingback_requested_at"} {
		v, _ := escField(t, ms, "gd-h1", k).(string)
		if ts, err := time.Parse(time.RFC3339, v); err != nil || ts.Location() != time.UTC {
			t.Errorf("%s = %q, want RFC3339 UTC", k, v)
		}
	}

	// Top of the chain: the operator has nothing above.
	if rec := callEscalationVerb(t, operatorCtx("owner@example.com"), s.handleHandUpEscalation, "gd-h1", "hand-up", map[string]any{"reason": "x"}); rec.Code != http.StatusConflict {
		t.Fatalf("operator hand-up: got %d, want 409", rec.Code)
	}

	// Tenant land: requester-held goes to the land owner, not the operator.
	f := map[string]any{"operation_type": "github_pr_merge", "approvers": []string{"steward:acme", "land-owner:acme"}, "execute_by": "requester", "held_by": "requester", "requesting_agent": "gd-crew1", "requesting_agent_verified": "true"}
	seedRoutedEscalation(ms, "gd-h2", "acme", f)
	rec = callEscalationVerb(t, agentCtx("gd-crew1", "crew-a", "acme"), s.handleHandUpEscalation, "gd-h2", "hand-up", map[string]any{"reason": "CI is red"})
	if rec.Code != http.StatusOK {
		t.Fatalf("tenant hand-up: got %d %s", rec.Code, rec.Body.String())
	}
	if got := escField(t, ms, "gd-h2", "held_by"); got != "land-owner:acme" {
		t.Fatalf("tenant hand-up held_by = %v, want land-owner:acme", got)
	}
	if got := escField(t, ms, "gd-h2", "audience"); got != "owner" {
		t.Fatalf("tenant hand-up audience = %v, want owner (D11: the land's owner)", got)
	}
}

// TestRouteEscalation_MovesHeldBy pins D11 rule 3: #756's route endpoint
// moves held_by to a lane of the target audience, so a route is never
// display-only (Review Focus 4).
func TestRouteEscalation_MovesHeldBy(t *testing.T) {
	s, ms := newLaneTestServer()
	route := func(id, to string) *httptest.ResponseRecorder {
		return callEscalationVerb(t, operatorCtx("owner@example.com"), s.handleRouteEscalation, id, "audience", map[string]any{"audience": to, "reason": "test"})
	}
	desk := mergeDeskEscalation("gd-crew1")
	desk["audience"] = "desk"
	seedRoutedEscalation(ms, "gd-r1", "gardenio", desk)

	// The laptop cannot approve while the desk holds it.
	if c := s.checkMayApprove(machineCtx("gardenio-laptop-operator"), ms.beads["gd-r1"]); c.Allowed {
		t.Fatalf("laptop allowed before the route: %+v", c)
	}
	// desk → operator: the first staffed operator-audience approver lane.
	if rec := route("gd-r1", "operator"); rec.Code != http.StatusOK {
		t.Fatalf("route to operator: %d %s", rec.Code, rec.Body.String())
	}
	if h, a := escField(t, ms, "gd-r1", "held_by"), escField(t, ms, "gd-r1", "audience"); h != "coordinator:laptop" || a != "operator" {
		t.Fatalf("after route to operator: held_by=%v audience=%v", h, a)
	}
	if c := ms.comments["gd-r1"]; len(c) != 1 || !strings.Contains(c[0].Text, "desk → operator (coordinator:laptop) by") {
		t.Fatalf("audit comment = %+v, want the lane named", c)
	}
	if c := s.checkMayApprove(machineCtx("gardenio-laptop-operator"), ms.beads["gd-r1"]); !c.Allowed {
		t.Fatalf("laptop refused after the route (positive control): %+v", c)
	}
	// operator → owner: held by the operator lane; the laptop is out again.
	if rec := route("gd-r1", "owner"); rec.Code != http.StatusOK {
		t.Fatalf("route to owner: %d %s", rec.Code, rec.Body.String())
	}
	if h, a := escField(t, ms, "gd-r1", "held_by"), escField(t, ms, "gd-r1", "audience"); h != "operator" || a != "owner" {
		t.Fatalf("after route to owner: held_by=%v audience=%v", h, a)
	}
	if c := s.checkMayApprove(machineCtx("gardenio-laptop-operator"), ms.beads["gd-r1"]); c.Allowed {
		t.Fatalf("laptop allowed on an owner-held escalation: %+v", c)
	}

	// No desk approver: the desk coordinator holds it without approving.
	seedRoutedEscalation(ms, "gd-r2", "gardenio", map[string]any{"operation_type": "helm_deploy", "approvers": []string{"operator"}, "execute_by": "coordinator:laptop", "held_by": "coordinator:laptop", "audience": "operator", "policy_rule": "escalation#5"})
	if rec := route("gd-r2", "desk"); rec.Code != http.StatusOK {
		t.Fatalf("route helm_deploy to desk: %d %s", rec.Code, rec.Body.String())
	}
	if h := escField(t, ms, "gd-r2", "held_by"); h != "coordinator:merge-desk" {
		t.Fatalf("held_by = %v, want coordinator:merge-desk (holds without approving)", h)
	}
	if c := s.checkMayApprove(agentCtx("gd-rev1", "reviewer-a", "gardenio"), ms.beads["gd-r2"]); c.Allowed {
		t.Fatalf("the desk approved an escalation it holds but is not an approver of: %+v", c)
	}

	// Approved: only owner.
	appr := mergeDeskEscalation("gd-crew1")
	appr["audience"], appr["approved_by"], appr["approved_at"], appr["approval_method"] = "desk", "owner@example.com", "2026-10-21T00:00:00Z", "web_ui"
	seedRoutedEscalation(ms, "gd-r3", "gardenio", appr)
	if rec := route("gd-r3", "operator"); rec.Code != http.StatusConflict {
		t.Fatalf("route an approved escalation to operator: got %d, want 409", rec.Code)
	}
	if rec := route("gd-r3", "owner"); rec.Code != http.StatusOK {
		t.Fatalf("route an approved escalation to owner (positive control): %d %s", rec.Code, rec.Body.String())
	}

	// Unstamped: audience only, as #756 does; the sweep picks the lane later.
	seedRoutedEscalation(ms, "gd-r4", "gardenio", map[string]any{"operation_type": "helm_deploy", "audience": "operator"})
	if rec := route("gd-r4", "owner"); rec.Code != http.StatusOK {
		t.Fatalf("route unstamped: %d %s", rec.Code, rec.Body.String())
	}
	if h, a := escField(t, ms, "gd-r4", "held_by"), escField(t, ms, "gd-r4", "audience"); h != nil || a != "owner" {
		t.Fatalf("unstamped route: held_by=%v audience=%v, want unset/owner", h, a)
	}
}

func TestListEscalations_HeldByFilter(t *testing.T) {
	s, ms := newLaneTestServer()
	seedRoutedEscalation(ms, "gd-l-desk", "gardenio", mergeDeskEscalation("gd-crew1"))
	seedRoutedEscalation(ms, "gd-l-op", "gardenio", map[string]any{"operation_type": "helm_deploy", "held_by": "operator", "approvers": []string{"operator"}})
	seedRoutedEscalation(ms, "gd-l-legacy", "gardenio", map[string]any{"operation_type": "helm_deploy"})
	list := func(ctx context.Context, q string) map[string]bool {
		t.Helper()
		r := httptest.NewRequest(http.MethodGet, "/v1/escalations?"+q, nil).WithContext(ctx)
		rec := httptest.NewRecorder()
		s.handleListEscalations(rec, r)
		if rec.Code != http.StatusOK {
			t.Fatalf("list %s: %d %s", q, rec.Code, rec.Body.String())
		}
		var resp struct {
			Escalations []struct {
				Issue model.Bead `json:"issue"`
			} `json:"escalations"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &resp)
		ids := map[string]bool{}
		for _, e := range resp.Escalations {
			ids[e.Issue.ID] = true
		}
		return ids
	}
	op := operatorCtx("owner@example.com")
	if got := list(op, "held_by=operator"); !got["gd-l-op"] || !got["gd-l-legacy"] || got["gd-l-desk"] {
		t.Fatalf("held_by=operator = %v, want gd-l-op and the unstamped gd-l-legacy (Global Constraint 7)", got)
	}
	if got := list(op, "held_by=coordinator:merge-desk,coordinator:laptop"); len(got) != 1 || !got["gd-l-desk"] {
		t.Fatalf("held_by=coordinator:* = %v", got)
	}
	if got := list(agentCtx("gd-rev1", "reviewer-a", "gardenio"), "held_by=@me"); len(got) != 1 || !got["gd-l-desk"] {
		t.Fatalf("reviewer @me = %v, want only the merge-desk escalation", got)
	}
	if got := list(op, "held_by=@me"); !got["gd-l-op"] || !got["gd-l-legacy"] || got["gd-l-desk"] {
		t.Fatalf("operator @me = %v", got)
	}
	// Positive control: no filter lists all three.
	if got := list(op, ""); len(got) != 3 {
		t.Fatalf("unfiltered = %v, want 3", got)
	}
	r := httptest.NewRequest(http.MethodGet, "/v1/escalations?held_by=bogus", nil).WithContext(op)
	rec := httptest.NewRecorder()
	s.handleListEscalations(rec, r)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad ref: got %d, want 400", rec.Code)
	}
}

// TestListEscalations_OperationTypeAndLabelFilters covers the merge desk's one
// list call (gd-CndLtkZ5h0): operation_type (one value in SQL, several in Go),
// label (all of) and exclude_label (any of), and that they stay inside the
// caller's read scope.
func TestListEscalations_OperationTypeAndLabelFilters(t *testing.T) {
	s, ms := newLaneTestServer()
	seedRoutedEscalation(ms, "gd-f-gh", "gardenio", mergeDeskEscalation("gd-crew1"))
	gl := mergeDeskEscalation("gd-crew1")
	gl["operation_type"] = "gitlab_mr_merge"
	seedRoutedEscalation(ms, "gd-f-gl", "gardenio", gl)
	seen := seedRoutedEscalation(ms, "gd-f-seen", "gardenio", mergeDeskEscalation("gd-crew1"))
	seen.Labels = append(seen.Labels, "merge-desk:seen")
	ms.labels[seen.ID] = seen.Labels
	draft := seedRoutedEscalation(ms, "gd-f-draft", "gardenio", mergeDeskEscalation("gd-crew1"))
	draft.Labels = append(draft.Labels, "merge-desk:draft", "merge-desk:seen")
	ms.labels[draft.ID] = draft.Labels
	seedRoutedEscalation(ms, "gd-f-helm", "gardenio", map[string]any{"operation_type": "helm_deploy"})
	seedRoutedEscalation(ms, "gd-f-other", "otherproj", mergeDeskEscalation("gd-crew9"))

	list := func(ctx context.Context, q string) []string {
		t.Helper()
		r := httptest.NewRequest(http.MethodGet, "/v1/escalations?"+q, nil).WithContext(ctx)
		rec := httptest.NewRecorder()
		s.handleListEscalations(rec, r)
		if rec.Code != http.StatusOK {
			t.Fatalf("list %s: %d %s", q, rec.Code, rec.Body.String())
		}
		var resp struct {
			Escalations []struct {
				Issue model.Bead `json:"issue"`
			} `json:"escalations"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &resp)
		var ids []string
		for _, e := range resp.Escalations {
			ids = append(ids, e.Issue.ID)
		}
		slices.Sort(ids)
		return ids
	}
	agent := agentCtx("gd-rev1", "reviewer-a", "gardenio")
	for _, tc := range []struct {
		q    string
		want []string
	}{
		{"operation_type=helm_deploy", []string{"gd-f-helm"}},
		{"operation_type=github_pr_merge,gitlab_mr_merge&exclude_label=merge-desk:seen", []string{"gd-f-gh", "gd-f-gl"}},
		{"operation_type=github_pr_merge&exclude_label=merge-desk:seen", []string{"gd-f-gh"}},
		{"label=merge-desk:draft", []string{"gd-f-draft"}},
		{"label=merge-desk:seen&exclude_label=merge-desk:draft", []string{"gd-f-seen"}},
		// gd-f-other (otherproj) never leaks into a gardenio agent's list.
		{"operation_type=github_pr_merge,gitlab_mr_merge", []string{"gd-f-draft", "gd-f-gh", "gd-f-gl", "gd-f-seen"}},
	} {
		if got := list(agent, tc.q); !slices.Equal(got, tc.want) {
			t.Errorf("%s = %v, want %v", tc.q, got, tc.want)
		}
	}
	// The Go-side operation_type pass still honours limit.
	if got := list(agent, "operation_type=github_pr_merge,gitlab_mr_merge&limit=1"); len(got) != 1 {
		t.Errorf("limit=1 = %v, want 1 row", got)
	}
}

func TestEscalationRouteSweep_IdleHandUp(t *testing.T) {
	s, ms := newLaneTestServer()
	markLaneSeen(ms, "gardenio-laptop-operator", time.Now())
	old := time.Now().UTC().Add(-7 * time.Hour).Format(time.RFC3339)
	idle := mergeDeskEscalation("gd-crew1")
	idle["held_at"] = old
	seedRoutedEscalation(ms, "gd-idle", "gardenio", idle)
	claimed := mergeDeskEscalation("gd-crew1")
	claimed["held_at"] = old
	claimed["claimed_by"] = "gd-rev1"
	seedRoutedEscalation(ms, "gd-claimed", "gardenio", claimed)
	seedRoutedEscalation(ms, "gd-op-old", "gardenio", map[string]any{"operation_type": "helm_deploy", "held_by": "operator", "held_at": old, "approvers": []string{"operator"}})
	fresh := mergeDeskEscalation("gd-crew1")
	fresh["held_at"] = time.Now().UTC().Format(time.RFC3339)
	seedRoutedEscalation(ms, "gd-fresh", "gardenio", fresh)
	noClock := mergeDeskEscalation("gd-crew1")
	seedRoutedEscalation(ms, "gd-noclock", "gardenio", noClock)

	t.Setenv("BEADS_ESCALATION_LANE_IDLE_HANDUP", "6h")
	if err := s.sweepUnroutedEscalationsLocked(context.Background()); err != nil {
		t.Fatal(err)
	}
	// P10.F2: off the floor, the next staffed coordinator approver takes it.
	if got := escField(t, ms, "gd-idle", "held_by"); got != "coordinator:laptop" {
		t.Fatalf("idle coordinator-held: held_by = %v, want coordinator:laptop", got)
	}
	if got := escField(t, ms, "gd-idle", "handed_up_by"); got != "system:escalation-route" {
		t.Fatalf("handed_up_by = %v", got)
	}
	if got := escField(t, ms, "gd-idle", "hand_up_reason"); got != "no coordinator:merge-desk activity for 6h0m0s; to coordinator:laptop" {
		t.Fatalf("hand_up_reason = %v", got)
	}
	// Positive controls: claimed, operator-held and fresh escalations are untouched.
	for _, id := range []string{"gd-claimed", "gd-fresh"} {
		if got := escField(t, ms, id, "held_by"); got != "coordinator:merge-desk" {
			t.Errorf("%s: held_by = %v, want untouched", id, got)
		}
	}
	if got := escField(t, ms, "gd-op-old", "handed_up_by"); got != nil {
		t.Errorf("operator-held escalation was handed up: %v", got)
	}
	// No held_at: the clock starts, nothing is handed up.
	if got := escField(t, ms, "gd-noclock", "held_by"); got != "coordinator:merge-desk" {
		t.Errorf("gd-noclock handed up with no clock")
	}
	if at, _ := escField(t, ms, "gd-noclock", "held_at").(string); at == "" {
		t.Error("gd-noclock held_at not started")
	}

	// 0 disables.
	again := mergeDeskEscalation("gd-crew1")
	again["held_at"] = old
	seedRoutedEscalation(ms, "gd-off", "gardenio", again)
	t.Setenv("BEADS_ESCALATION_LANE_IDLE_HANDUP", "0")
	_ = s.sweepUnroutedEscalationsLocked(context.Background())
	if got := escField(t, ms, "gd-off", "held_by"); got != "coordinator:merge-desk" {
		t.Errorf("idle hand-up ran with BEADS_ESCALATION_LANE_IDLE_HANDUP=0")
	}
}

// TestEscalationRouteSweep_IdleHandUpForward pins P10.F2: the idle sweep moves
// an item off the floor forward through its staffed coordinator approvers and
// stops at operator; everything else still goes straight to operator.
func TestEscalationRouteSweep_IdleHandUpForward(t *testing.T) {
	s, ms := newLaneTestServer()
	markLaneSeen(ms, "gardenio-laptop-operator", time.Now())
	t.Setenv("BEADS_ESCALATION_LANE_IDLE_HANDUP", "6h")
	old := time.Now().UTC().Add(-7 * time.Hour).Format(time.RFC3339)
	seed := func(id string, extra map[string]any) {
		f := mergeDeskEscalation("gd-crew1")
		f["held_at"] = old
		for k, v := range extra {
			f[k] = v
		}
		seedRoutedEscalation(ms, id, "gardenio", f)
	}
	sweep := func() {
		t.Helper()
		if err := s.sweepUnroutedEscalationsLocked(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	seed("gd-chain", nil)
	seed("gd-floor", map[string]any{"sensitivity": "secret"})
	seed("gd-approved", map[string]any{"approved_at": old})
	seed("gd-unstaffed", map[string]any{"approvers": []string{"coordinator:merge-desk", "coordinator:cluster", "operator"}})
	seed("gd-holdonly", map[string]any{"operation_type": "create", "approvers": []string{"operator"}, "execute_by": "server", "held_by": "coordinator:laptop"})
	sweep()
	if got, aud := escField(t, ms, "gd-chain", "held_by"), escField(t, ms, "gd-chain", "audience"); got != "coordinator:laptop" || aud != "operator" {
		t.Fatalf("desk idle: held_by = %v audience = %v, want coordinator:laptop / operator", got, aud)
	}
	for _, id := range []string{"gd-floor", "gd-unstaffed", "gd-holdonly"} {
		if got := escField(t, ms, id, "held_by"); got != "operator" {
			t.Errorf("%s: held_by = %v, want operator", id, got)
		}
	}
	// An approved merge stays with the desk that records it, re-notified
	// (gd-YnsSlh9239).
	if got := escField(t, ms, "gd-approved", "held_by"); got != "coordinator:merge-desk" {
		t.Errorf("gd-approved: held_by = %v, want coordinator:merge-desk", got)
	}

	// The laptop idles too: nothing staffed after it but operator, and there it stops.
	for _, want := range []string{"1", "2", "2"} {
		if got := escField(t, ms, "gd-chain", "pingback_count"); got != want {
			t.Fatalf("pingback_count = %v, want %s", got, want)
		}
		_ = s.mergeBeadFields(context.Background(), "gd-chain", map[string]any{"held_at": old})
		sweep()
	}
	if got := escField(t, ms, "gd-chain", "held_by"); got != "operator" {
		t.Fatalf("laptop idle: held_by = %v, want operator", got)
	}
	if got := escField(t, ms, "gd-chain", "hand_up_reason"); got != "no coordinator:laptop activity for 6h0m0s" {
		t.Fatalf("laptop idle: hand_up_reason = %v", got)
	}
}

// escField reads one field of a seeded escalation; nil when it is unset.
func escField(t *testing.T, ms *mockStore, id, key string) any {
	t.Helper()
	b, ok := ms.beads[id]
	if !ok {
		t.Fatalf("escalation %s not seeded", id)
	}
	var f map[string]any
	if err := json.Unmarshal(b.Fields, &f); err != nil {
		t.Fatalf("escalation %s fields: %v", id, err)
	}
	return f[key]
}
