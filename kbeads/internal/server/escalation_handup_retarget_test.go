package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"gardenio/kbeads/internal/model"
)

// newRetargetTestServer is newLaneTestServer with Task 15's policy: the
// cluster coordinator lane and its C3 rule. staffCluster adds its client to
// BEADS_ESCALATION_COORDINATOR_CLIENTS.
func newRetargetTestServer(staffCluster bool) (*BeadsServer, *mockStore) {
	s, ms := newLaneTestServer()
	clients := []string{"gardenio-laptop-operator"}
	if staffCluster {
		clients = append(clients, clusterClient)
	}
	s.SetEscalationCoordinatorClients(clients)
	pol, _ := json.Marshal(map[string]any{
		"rules": []any{
			map[string]any{"when": map[string]any{"operation_types": []string{"bead_close", "kbeads_admin_write"}}, "approvers": []string{"operator"}, "execute_by": clusterCoordinatorLane, "hold_by": clusterCoordinatorLane},
			map[string]any{"when": map[string]any{}, "approvers": []string{"operator"}, "execute_by": "coordinator:laptop", "hold_by": "coordinator:laptop"},
		},
		"coordinators": map[string]any{
			"laptop":  map[string]any{"principals": []string{"machine:gardenio-laptop-operator"}, "audience": "operator"},
			"cluster": map[string]any{"principals": []string{"machine:" + clusterClient}, "audience": "operator"},
		},
	})
	ms.beads["gd-pol"] = &model.Bead{ID: "gd-pol", Kind: model.KindConfig, Type: model.BeadType(escalationPolicyType), Title: "escalation policy", Status: model.StatusOpen, Fields: pol}
	return s, ms
}

// laneEscalation is an escalation held and executed by lane, idle for 7h.
func laneEscalation(op, lane string, approved bool) map[string]any {
	f := map[string]any{
		"operation_type": op, "approvers": []string{"operator"},
		"execute_by": lane, "held_by": lane, "operator_only": false,
		"held_at": time.Now().UTC().Add(-7 * time.Hour).Format(time.RFC3339),
	}
	if approved {
		f["approved_at"] = time.Now().UTC().Add(-8 * time.Hour).Format(time.RFC3339)
		f["approved_by"] = "owner@example.com"
	}
	return f
}

func assertEscFields(t *testing.T, ms *mockStore, id string, want map[string]any) {
	t.Helper()
	for k, v := range want {
		if got := escField(t, ms, id, k); got != v {
			t.Errorf("%s: %s = %v, want %v", id, k, got, v)
		}
	}
}

// TestIdleHandUp_RetargetsExecuteBy pins P7.1: the idle sweep that hands an
// escalation up to the operator also moves execute_by off the lane that
// idled, so an approved one is claimable by a lane after one sweep.
func TestIdleHandUp_RetargetsExecuteBy(t *testing.T) {
	t.Setenv("BEADS_ESCALATION_LANE_IDLE_HANDUP", "6h")
	s, ms := newRetargetTestServer(true)
	seedRoutedEscalation(ms, "gd-beadonly", "gardenio", laneEscalation("bead_close", "coordinator:laptop", true))
	floor := laneEscalation("bead_close", "coordinator:laptop", true)
	floor["sensitivity"] = "secret"
	seedRoutedEscalation(ms, "gd-floor", "gardenio", floor)
	seedRoutedEscalation(ms, "gd-helm", "gardenio", laneEscalation("helm_deploy", "coordinator:laptop", true))
	seedRoutedEscalation(ms, "gd-unapproved", "gardenio", laneEscalation("bead_close", "coordinator:laptop", false))
	seedRoutedEscalation(ms, "gd-cluster-idle", "gardenio", laneEscalation("bead_close", clusterCoordinatorLane, true))
	replay := laneEscalation("close", "coordinator:laptop", false)
	replay["execute_by"] = "server"
	seedRoutedEscalation(ms, "gd-replay", "gardenio", replay)
	merge := mergeDeskEscalation("gd-crew1")
	merge["held_at"], merge["approved_at"] = replay["held_at"], time.Now().UTC().Format(time.RFC3339)
	seedRoutedEscalation(ms, "gd-merge", "gardenio", merge)

	if err := s.sweepUnroutedEscalationsLocked(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Approved bead-only: the cluster lane executes and now holds it.
	assertEscFields(t, ms, "gd-beadonly", map[string]any{"execute_by": clusterCoordinatorLane, "held_by": clusterCoordinatorLane, "audience": "operator", "handed_up_by": escalationRouteSweepActor})
	if r, _ := escField(t, ms, "gd-beadonly", "hand_up_reason").(string); !strings.HasSuffix(r, "; execution moved from coordinator:laptop to coordinator:cluster") {
		t.Errorf("gd-beadonly hand_up_reason = %q", r)
	}
	if rec := callEscalationVerb(t, machineCtx(clusterClient), s.handleClaimEscalation, "gd-beadonly", "claim", map[string]any{}); rec.Code != http.StatusOK {
		t.Fatalf("cluster claim after one sweep: got %d %s", rec.Code, rec.Body.String())
	}

	// Floor (a bead-only type marked secret) and a non-bead-only type have no
	// other machine lane: approved, they stay with the staffed lane that
	// executes them and it is re-notified (gd-YnsSlh9239), rather than
	// handing a person an approval they already gave.
	for _, id := range []string{"gd-floor", "gd-helm"} {
		assertEscFields(t, ms, id, map[string]any{"execute_by": "coordinator:laptop", "held_by": "coordinator:laptop", "pingback_source": "idle", "pingback_count": "1"})
		if got := escField(t, ms, id, "handed_up_by"); got != nil {
			t.Errorf("%s: handed up by %v, want re-notified in place", id, got)
		}
		at, _ := escField(t, ms, id, "held_at").(string)
		if heldAt, err := time.Parse(time.RFC3339, at); err != nil || time.Since(heldAt) > time.Minute {
			t.Errorf("%s: held_at = %q, want the clock restarted", id, at)
		}
	}
	if rec := callEscalationVerb(t, machineCtx(clusterClient), s.handleClaimEscalation, "gd-floor", "claim", map[string]any{}); rec.Code != http.StatusForbidden {
		t.Errorf("cluster claim of a floor item: got %d, want 403", rec.Code)
	}

	// Unapproved: the operator holds it for approval; approval hands it to
	// the cluster lane (stampEscalationHolder reads execute_by).
	assertEscFields(t, ms, "gd-unapproved", map[string]any{"execute_by": clusterCoordinatorLane, "held_by": "operator", "audience": "owner"})

	// Approved on the cluster lane, with no other machine lane: it stays and
	// the cluster lane is re-notified.
	assertEscFields(t, ms, "gd-cluster-idle", map[string]any{"execute_by": clusterCoordinatorLane, "held_by": clusterCoordinatorLane, "pingback_source": "idle"})

	// A server replay keeps execute_by=server and goes up. A merge (a verdict
	// record) keeps the desk that records it; this policy has no desk lane,
	// so it goes up too (IdleHandUpForward has a staffed desk keep it).
	assertEscFields(t, ms, "gd-replay", map[string]any{"execute_by": "server", "held_by": "operator"})
	assertEscFields(t, ms, "gd-merge", map[string]any{"execute_by": "coordinator:merge-desk", "held_by": "operator"})
}

// TestIdleHandUp_UnapprovedKeepsStaffedExecutor is gd-YnsSlh9239: the cluster
// lane idled on an unapproved bead_close, the sweep handed it up for approval
// and moved its execution to operator, so the owner's approval spawned no
// coordinator. Now the hand-up keeps execute_by on the staffed lane, and an
// unstaffed one still goes to operator.
func TestIdleHandUp_UnapprovedKeepsStaffedExecutor(t *testing.T) {
	t.Setenv("BEADS_ESCALATION_LANE_IDLE_HANDUP", "6h")
	s, ms := newRetargetTestServer(true)
	seedRoutedEscalation(ms, "gd-ask", "gardenio", laneEscalation("bead_close", clusterCoordinatorLane, false))
	if err := s.sweepUnroutedEscalationsLocked(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertEscFields(t, ms, "gd-ask", map[string]any{"execute_by": clusterCoordinatorLane, "held_by": "operator", "audience": "owner", "hand_up_reason": "no coordinator:cluster activity for 6h0m0s"})

	s, ms = newRetargetTestServer(false)
	seedRoutedEscalation(ms, "gd-ask", "gardenio", laneEscalation("bead_close", clusterCoordinatorLane, false))
	if err := s.sweepUnroutedEscalationsLocked(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertEscFields(t, ms, "gd-ask", map[string]any{"execute_by": "operator", "held_by": "operator"})
}

// TestIdleHandUp_UnstaffedClusterGoesToOperator is the staffing control:
// the same approved bead-only item, with the cluster client not admitted, is
// not moved to the cluster lane. It stays with the laptop lane, which is
// staffed (gd-YnsSlh9239).
func TestIdleHandUp_UnstaffedClusterGoesToOperator(t *testing.T) {
	t.Setenv("BEADS_ESCALATION_LANE_IDLE_HANDUP", "6h")
	s, ms := newRetargetTestServer(false)
	seedRoutedEscalation(ms, "gd-beadonly", "gardenio", laneEscalation("bead_close", "coordinator:laptop", true))
	if err := s.sweepUnroutedEscalationsLocked(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertEscFields(t, ms, "gd-beadonly", map[string]any{"execute_by": "coordinator:laptop", "held_by": "coordinator:laptop", "pingback_source": "idle"})
}

// TestManualHandUp_KeepsExecuteBy: a lane that hands up for approval (the
// cluster coordinator's C6 flow) keeps executing; only the sweep re-targets.
func TestManualHandUp_KeepsExecuteBy(t *testing.T) {
	s, ms := newRetargetTestServer(true)
	f := laneEscalation("bead_close", clusterCoordinatorLane, false)
	f["held_at"] = time.Now().UTC().Format(time.RFC3339)
	seedRoutedEscalation(ms, "gd-ask", "gardenio", f)
	rec := callEscalationVerb(t, machineCtx(clusterClient), s.handleHandUpEscalation, "gd-ask", "hand-up", map[string]any{"reason": "needs your approval"})
	if rec.Code != http.StatusOK {
		t.Fatalf("hand-up: got %d %s", rec.Code, rec.Body.String())
	}
	assertEscFields(t, ms, "gd-ask", map[string]any{"execute_by": clusterCoordinatorLane, "held_by": "operator", "hand_up_reason": "needs your approval"})
}

// TestRerouteEscalations_ApprovedUnexecuted pins the P7.1 backfill: the same
// rule over the escalations a hand-up stranded before it, admin only.
func TestRerouteEscalations_ApprovedUnexecuted(t *testing.T) {
	s, ms := newRetargetTestServer(true)
	stranded := func(op string) map[string]any {
		f := laneEscalation(op, "coordinator:laptop", true)
		f["held_by"] = "operator"
		return f
	}
	seedRoutedEscalation(ms, "gd-s-beadonly", "gardenio", stranded("bead_close"))
	// Handed up by a person or the lane itself: its own call, it stays.
	seedRoutedEscalation(ms, "gd-s-helm", "gardenio", stranded("helm_deploy"))
	// Handed up by the idle sweep (gd-YnsSlh9239): back to the laptop lane.
	swept := stranded("helm_deploy")
	swept["handed_up_by"] = escalationRouteSweepActor
	swept["hand_up_reason"] = "no coordinator:laptop activity for 6h0m0s"
	seedRoutedEscalation(ms, "gd-s-swept", "gardenio", swept)
	// Execution moved to operator by the sweep: back to the cluster lane.
	moved := stranded("bead_close")
	moved["execute_by"] = "operator"
	moved["handed_up_by"] = escalationRouteSweepActor
	moved["hand_up_reason"] = "no coordinator:cluster activity for 6h0m0s; execution moved from coordinator:cluster to operator"
	seedRoutedEscalation(ms, "gd-s-moved", "gardenio", moved)
	unapproved := stranded("bead_close")
	delete(unapproved, "approved_at")
	seedRoutedEscalation(ms, "gd-s-unapproved", "gardenio", unapproved)
	claimed := stranded("bead_close")
	claimed["claimed_by"] = "owner@example.com"
	seedRoutedEscalation(ms, "gd-s-claimed", "gardenio", claimed)
	laptopHeld := laneEscalation("bead_close", "coordinator:laptop", true)
	seedRoutedEscalation(ms, "gd-s-laptop-held", "gardenio", laptopHeld)

	body := map[string]any{"approved_unexecuted": true}
	if rec := callEscalationVerb(t, agentCtx("gd-crew1", "crew-a", "gardenio"), s.handleRerouteEscalations, "", "reroute", body); rec.Code != http.StatusForbidden {
		t.Fatalf("agent reroute: got %d, want 403", rec.Code)
	}
	if rec := callEscalationVerb(t, operatorCtx("owner@example.com"), s.handleRerouteEscalations, "", "reroute", map[string]any{}); rec.Code != http.StatusBadRequest {
		t.Fatalf("reroute without approved_unexecuted: got %d, want 400", rec.Code)
	}

	dry := callEscalationVerb(t, operatorCtx("owner@example.com"), s.handleRerouteEscalations, "", "reroute", map[string]any{"approved_unexecuted": true, "dry_run": true})
	if dry.Code != http.StatusOK {
		t.Fatalf("dry run: got %d %s", dry.Code, dry.Body.String())
	}
	if got := rerouted(t, dry.Body.Bytes()); len(got) != 3 {
		t.Fatalf("dry run rerouted = %+v, want 3", got)
	}
	if got := escField(t, ms, "gd-s-beadonly", "execute_by"); got != "coordinator:laptop" {
		t.Fatalf("dry run wrote execute_by = %v", got)
	}

	rec := callEscalationVerb(t, operatorCtx("owner@example.com"), s.handleRerouteEscalations, "", "reroute", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("reroute: got %d %s", rec.Code, rec.Body.String())
	}
	got := map[string]bool{}
	for _, r := range rerouted(t, rec.Body.Bytes()) {
		got[r.ID] = true
	}
	if len(got) != 3 || !got["gd-s-beadonly"] || !got["gd-s-swept"] || !got["gd-s-moved"] {
		t.Fatalf("rerouted = %v, want gd-s-beadonly, gd-s-swept and gd-s-moved", got)
	}
	assertEscFields(t, ms, "gd-s-beadonly", map[string]any{"execute_by": clusterCoordinatorLane, "held_by": clusterCoordinatorLane, "audience": "operator", "pingback_source": "reroute"})
	assertEscFields(t, ms, "gd-s-swept", map[string]any{"execute_by": "coordinator:laptop", "held_by": "coordinator:laptop", "audience": "operator", "pingback_source": "reroute"})
	assertEscFields(t, ms, "gd-s-moved", map[string]any{"execute_by": clusterCoordinatorLane, "held_by": clusterCoordinatorLane, "audience": "operator"})
	assertEscFields(t, ms, "gd-s-helm", map[string]any{"execute_by": "coordinator:laptop", "held_by": "operator"})
	if rec := callEscalationVerb(t, machineCtx(clusterClient), s.handleClaimEscalation, "gd-s-moved", "claim", map[string]any{}); rec.Code != http.StatusOK {
		t.Fatalf("cluster claim of the restored item: got %d %s", rec.Code, rec.Body.String())
	}
	if rec := callEscalationVerb(t, machineCtx(clusterClient), s.handleClaimEscalation, "gd-s-beadonly", "claim", map[string]any{}); rec.Code != http.StatusOK {
		t.Fatalf("cluster claim after reroute: got %d %s", rec.Code, rec.Body.String())
	}
	// Controls: unapproved, claimed and still-laptop-held items are untouched.
	for _, id := range []string{"gd-s-unapproved", "gd-s-claimed", "gd-s-laptop-held"} {
		if got := escField(t, ms, id, "execute_by"); got != "coordinator:laptop" {
			t.Errorf("%s: execute_by = %v, want untouched", id, got)
		}
	}
}

func rerouted(t *testing.T, body []byte) []reroutedEscalation {
	t.Helper()
	var resp struct {
		Rerouted []reroutedEscalation `json:"rerouted"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatal(err)
	}
	return resp.Rerouted
}

// TestRouteToOperator_PrefersExecutorCoordinator pins D11 rule 3 once the
// operator audience has two coordinators (gd-OBwxpCcMBY): with no staffed
// operator-audience approver, the escalation lands on its execute_by lane,
// then its rule's hold_by, never "cluster" just because it sorts first.
func TestRouteToOperator_PrefersExecutorCoordinator(t *testing.T) {
	s, ms := newRetargetTestServer(true)
	route := func(id string) {
		t.Helper()
		if rec := callEscalationVerb(t, operatorCtx("owner@example.com"), s.handleRouteEscalation, id, "audience", map[string]any{"audience": "operator", "reason": "test"}); rec.Code != http.StatusOK {
			t.Fatalf("route %s to operator: %d %s", id, rec.Code, rec.Body.String())
		}
	}
	ownerHeld := func(op, executeBy, rule string) map[string]any {
		return map[string]any{"operation_type": op, "approvers": []string{"operator"}, "execute_by": executeBy, "held_by": "operator", "audience": "owner", "operator_only": false, "policy_rule": rule}
	}
	seedRoutedEscalation(ms, "gd-rt-catch", "gardenio", ownerHeld("helm_deploy", "coordinator:laptop", "escalation#2"))
	seedRoutedEscalation(ms, "gd-rt-c3", "gardenio", ownerHeld("bead_close", clusterCoordinatorLane, "escalation#1"))
	// execute_by names no coordinator: the stamped rule's hold_by decides.
	seedRoutedEscalation(ms, "gd-rt-hold", "gardenio", ownerHeld("helm_deploy", "server", "escalation#2"))
	for _, id := range []string{"gd-rt-catch", "gd-rt-c3", "gd-rt-hold"} {
		route(id)
	}
	assertEscFields(t, ms, "gd-rt-catch", map[string]any{"held_by": "coordinator:laptop", "audience": "operator"})
	assertEscFields(t, ms, "gd-rt-c3", map[string]any{"held_by": clusterCoordinatorLane, "audience": "operator"})
	assertEscFields(t, ms, "gd-rt-hold", map[string]any{"held_by": "coordinator:laptop", "audience": "operator"})
}

// TestRouteSweep_KeptOperatorAudiencePrefersExecutor is D11 rule 4 with two
// operator-audience coordinators (gd-OBwxpCcMBY): an unstamped escalation
// whose #756 audience is operator, on rules without hold_by, is held by the
// coordinator of the route being stamped, not the first in name order.
func TestRouteSweep_KeptOperatorAudiencePrefersExecutor(t *testing.T) {
	s, ms := newRetargetTestServer(true)
	pol, _ := json.Marshal(map[string]any{
		"rules": []any{
			map[string]any{"when": map[string]any{"operation_types": []string{"bead_close"}}, "approvers": []string{"operator"}, "execute_by": clusterCoordinatorLane},
			map[string]any{"when": map[string]any{}, "approvers": []string{"operator"}, "execute_by": "coordinator:laptop"},
		},
		"coordinators": map[string]any{
			"laptop":  map[string]any{"principals": []string{"machine:gardenio-laptop-operator"}, "audience": "operator"},
			"cluster": map[string]any{"principals": []string{"machine:" + clusterClient}, "audience": "operator"},
		},
	})
	ms.beads["gd-pol"].Fields = pol
	seedRoutedEscalation(ms, "gd-sw-catch", "gardenio", map[string]any{"operation_type": "helm_deploy", "audience": "operator"})
	seedRoutedEscalation(ms, "gd-sw-c3", "gardenio", map[string]any{"operation_type": "bead_close", "audience": "operator"})
	if err := s.sweepUnroutedEscalationsLocked(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertEscFields(t, ms, "gd-sw-catch", map[string]any{"held_by": "coordinator:laptop", "audience": "operator", "execute_by": "coordinator:laptop"})
	assertEscFields(t, ms, "gd-sw-c3", map[string]any{"held_by": clusterCoordinatorLane, "audience": "operator", "execute_by": clusterCoordinatorLane})
}
