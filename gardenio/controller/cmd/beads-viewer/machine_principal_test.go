package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// Machine principal for the viewer's background reactors (gd-Hc34EENbO9,
// decision gd-ZsfxvXRiyv "tight"): the plan-label and plan-step reactors run
// off the daemon SSE stream with no human bearer, so when the viewer has its
// own Keycloak service-account credential they attach it per call
// (beadsapi.WithMachinePrincipal). Human-driven handlers never do.

// stubTokenSource is a TokenSource that hands out one fixed token.
type stubTokenSource struct{ tok string }

func (s stubTokenSource) Token(context.Context) (string, error) { return s.tok, nil }

// The plan-label reactor's bind CreateBead carries the machine principal when
// one is configured, and nothing when it is not (today's behavior).
func TestReactToPlanLabel_MachinePrincipalOnBindWrite(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		f := newFake()
		storeCreatedBeads(f)
		f.createResp = "gd-newcap"
		f.beads["gd-plan1"] = &beadsapi.BeadDetail{
			ID: "gd-plan1", Type: "epic", Status: "in_progress",
			Title: "Ship the widget", Labels: []string{"project:gardenio", "plan"},
			OperatedBy: "oidc:owner@example.com",
		}
		s := labelReactorServer(f)
		if enabled {
			s.SetMachinePrincipal(stubTokenSource{tok: "machine-jwt"})
		}

		s.reactToPlanLabel(context.Background(), "gd-plan1")

		if got := len(gardenerCreates(f)); got != 1 {
			t.Fatalf("enabled=%v: expected one gardener create, got %d", enabled, got)
		}
		if f.createLastMachine != enabled {
			t.Errorf("enabled=%v: bind CreateBead carried a machine principal=%v", enabled, f.createLastMachine)
		}
		// The epic's human is still offered as the gardener's operator in the
		// request body; the daemon ignores the header for a machine caller and
		// attributes the trigger to machine:<clientId> (see the PR notes).
		if f.createLastOpBy != "oidc:owner@example.com" {
			t.Errorf("enabled=%v: operator on ctx = %q", enabled, f.createLastOpBy)
		}
	}
}

// The plan-step reactor's last_nudge_at stamp on the (human-owned) plan epic
// carries the machine principal — that write is why gardenio-machine-plan-reactor
// is typed to epic.
func TestReactToStepEdge_MachinePrincipalOnNudgeStamp(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		f := seedPlansFake()
		seedAssistantBead(f, "gd-asstlive", "gardenio-plan-asst-live", "gd-plan1", "working", planAssistantTestNow)
		f.beads["gd-newstep"] = &beadsapi.BeadDetail{
			ID: "gd-newstep", Type: "task", Status: "open", Title: "Ship the docs",
			Labels: []string{"project:gardenio"},
		}
		cm := &fakeScreenClient{}
		s := reactorTestServer(f, cm)
		if enabled {
			s.SetMachinePrincipal(stubTokenSource{tok: "machine-jwt"})
		}

		s.reactToStepEdge(context.Background(), "gd-newstep", "gd-plan1", "oidc:op@acme.example")

		if cm.nudgeCalls != 1 {
			t.Fatalf("enabled=%v: expected exactly one nudge, got %d", enabled, cm.nudgeCalls)
		}
		if f.updateFieldsLastID != "gd-plan1" {
			t.Fatalf("enabled=%v: last_nudge_at stamp went to %q, want gd-plan1", enabled, f.updateFieldsLastID)
		}
		if _, ok := f.updateFieldsLastVal["last_nudge_at"]; !ok {
			t.Fatalf("enabled=%v: stamp did not write last_nudge_at: %v", enabled, f.updateFieldsLastVal)
		}
		if f.updateFieldsLastMachine != enabled {
			t.Errorf("enabled=%v: nudge stamp carried a machine principal=%v", enabled, f.updateFieldsLastMachine)
		}
	}
}

// Operator review of PR #343 (blocker 1): a step added to a plan whose gardener
// is PARKED must still wake it. The wake is an UpdateBead on the agent bead and
// has no human bearer behind it (SSE origin), so with write-bearer enforcement
// on and the viewer's static token read-only it can only go out on the machine
// principal — a wake on the plain ctx would be dropped client-side
// (ErrUserBearerRequired). Pins: the wake reuses the prior bead (no create),
// flips it to in_progress with the seed as its prompt, carries the machine
// principal, and the epic stamp does too.
func TestReactToStepEdge_MachinePrincipalWakesParkedAssistant(t *testing.T) {
	f := seedPlansFake()
	seedAssistantBead(f, "gd-asstpark", "gardenio-plan-asst-park", "gd-plan1", "stopping",
		planAssistantTestNow.Add(-2*time.Hour))
	f.beads["gd-asstpark"].Status = "closed"
	f.beads["gd-newstep"] = &beadsapi.BeadDetail{
		ID: "gd-newstep", Type: "task", Status: "open", Title: "resume this",
	}
	cm := &fakeScreenClient{}
	s := reactorTestServer(f, cm)
	s.SetMachinePrincipal(stubTokenSource{tok: "machine-jwt"})

	s.reactToStepEdge(context.Background(), "gd-newstep", "gd-plan1", "oidc:op@acme.example")

	if f.createCalls != 0 {
		t.Fatalf("waking a parked gardener must not create a bead (no cold spawn as the machine); got %d CreateBead", f.createCalls)
	}
	if f.updateCalls != 1 || f.updateLastID != "gd-asstpark" {
		t.Fatalf("expected exactly one wake UpdateBead on gd-asstpark, got %d (last id %q)", f.updateCalls, f.updateLastID)
	}
	if f.updateLastReq.Status == nil || *f.updateLastReq.Status != "in_progress" {
		t.Errorf("wake did not flip the parked bead to in_progress: %+v", f.updateLastReq.Status)
	}
	if p, _ := f.updateLastReq.Fields["prompt"].(string); !strings.Contains(p, "resume this") {
		t.Errorf("wake prompt does not carry the new step: %v", f.updateLastReq.Fields["prompt"])
	}
	if !f.updateLastMachine {
		t.Error("the wake went out without the machine principal; under write-bearer enforcement it would never be sent")
	}
	if f.updateFieldsLastID != "gd-plan1" || !f.updateFieldsLastMachine {
		t.Errorf("last_nudge_at stamp: id=%q machine=%v; want gd-plan1 on the machine principal", f.updateFieldsLastID, f.updateFieldsLastMachine)
	}
}

// The reactor stays NUDGE/WAKE-ONLY on the machine principal: a plan with no
// bound gardener gets no agent bead created as machine:gardenio-beads-viewer.
func TestReactToStepEdge_MachinePrincipalNeverColdSpawns(t *testing.T) {
	f := seedPlansFake()
	f.beads["gd-newstep"] = &beadsapi.BeadDetail{
		ID: "gd-newstep", Type: "task", Status: "open", Title: "no gardener yet",
	}
	cm := &fakeScreenClient{}
	s := reactorTestServer(f, cm)
	s.SetMachinePrincipal(stubTokenSource{tok: "machine-jwt"})

	s.reactToStepEdge(context.Background(), "gd-newstep", "gd-plan1", "oidc:op@acme.example")

	if f.createCalls != 0 || f.updateCalls != 0 {
		t.Fatalf("no bound gardener: want no create/update, got create=%d update=%d", f.createCalls, f.updateCalls)
	}
}

// A human-driven write never picks up the machine principal: machineCtx is
// called only by the reactors, and the Server-level credential is not applied
// to ordinary request contexts.
func TestMachineCtx_OnlyWhenConfigured(t *testing.T) {
	s := newTestServer(t)
	if beadsapi.MachinePrincipalFromContext(s.machineCtx(context.Background())) != nil {
		t.Fatal("machineCtx without a principal must leave ctx untouched")
	}
	s.SetMachinePrincipal(stubTokenSource{tok: "machine-jwt"})
	if beadsapi.MachinePrincipalFromContext(s.machineCtx(context.Background())) == nil {
		t.Fatal("machineCtx with a principal must attach it")
	}
	// An ordinary request context (what operatorMiddleware hands a handler)
	// carries no machine principal even when one is configured.
	if beadsapi.MachinePrincipalFromContext(beadsapi.WithUserBearer(context.Background(), "user-jwt")) != nil {
		t.Fatal("a human request context must never carry the machine principal")
	}
}
