package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
)

// --- controller ReclaimOwnership (gd-RZJk5gspFv, decision gd-mM3IFDGoTq
// "narrow"): the stale reaper may UPDATE, never close, a bug/feature/chore/epic
// whose assignee is a gone agent. --------------------------------------------

// putAgentBead adds a type=agent bead named name with the given status.
func putAgentBead(ms *mockStore, id, name string, status model.Status, updated time.Time) {
	ms.beads[id] = &model.Bead{
		ID: id, Title: name, Type: "agent", Kind: model.KindConfig, Status: status,
		Fields: json.RawMessage(`{"agent":"` + name + `"}`), UpdatedAt: updated,
	}
}

func reclaimServer(t *testing.T) (*BeadsServer, *mockStore) {
	t.Helper()
	srv, ms, _ := testCtx(t)
	now := time.Now()
	putAgentBead(ms, "gd-ag-dead", "dead-crew", model.StatusClosed, now)
	putAgentBead(ms, "gd-ag-live", "live-crew", model.StatusInProgress, now)
	// Respawned under the same name: the old bead is closed, the new one is
	// active, and the closed one is the more recently updated.
	putAgentBead(ms, "gd-ag-re1", "respawned-crew", model.StatusOpen, now.Add(-time.Hour))
	putAgentBead(ms, "gd-ag-re2", "respawned-crew", model.StatusClosed, now)
	// A decision-resume agent bead titled with a human's email (gd-9H3WCBTnmq).
	putAgentBead(ms, "gd-ag-human", "owner@example.com", model.StatusClosed, now)
	return srv, ms
}

func reclaimBead(id string, typ model.BeadType, assignee string) *model.Bead {
	return &model.Bead{
		ID: id, Type: typ, Kind: model.KindIssue, Status: model.StatusInProgress,
		CreatedBy: "oidc:owner@example.com", Assignee: assignee,
	}
}

func TestAuthorizeOwnership_Machine_ControllerReclaim(t *testing.T) {
	srv, ms := reclaimServer(t)
	ctrl := machineCtx("gardenio-controller", "gardenio-operator", "gardenio-machine-controller-ownership")

	for _, typ := range []model.BeadType{model.TypeBug, model.TypeFeature, model.TypeChore, model.TypeEpic} {
		b := reclaimBead("gd-r-"+string(typ), typ, "dead-crew")
		if d := srv.authorizeOwnership(ctrl, "update", b); d != nil {
			t.Errorf("controller must update a gone crew's %s, got %+v", typ, d)
		}
		for _, action := range []string{"close", "delete", "dep-add"} {
			if d := srv.authorizeOwnership(ctrl, action, b); d == nil {
				t.Errorf("reclaim is update-only: %s on a gone crew's %s must be denied", action, typ)
			}
		}
	}

	for _, c := range []struct {
		name string
		bead *model.Bead
	}{
		{"active agent bead", reclaimBead("gd-r1", model.TypeBug, "live-crew")},
		{"respawned: an active bead beside a closed one", reclaimBead("gd-r2", model.TypeBug, "respawned-crew")},
		{"email assignee with a closed agent bead", reclaimBead("gd-r3", model.TypeBug, "owner@example.com")},
		{"assignee with no agent bead", reclaimBead("gd-r4", model.TypeBug, "thread-1792632756-481359")},
		{"no assignee", reclaimBead("gd-r5", model.TypeBug, "")},
		{"type outside the grant", reclaimBead("gd-r6", "dm", "dead-crew")},
	} {
		if d := srv.authorizeOwnership(ctrl, "update", c.bead); d == nil {
			t.Errorf("%s: controller update must be denied", c.name)
		} else if d.CallerSubject != "machine:gardenio-controller" {
			t.Errorf("%s: CallerSubject=%q", c.name, d.CallerSubject)
		}
	}

	// Another machine principal, and the controller without the role, get none
	// of it.
	dead := reclaimBead("gd-r7", model.TypeBug, "dead-crew")
	for name, ctx := range map[string]context.Context{
		"trigger-engine without the role": machineCtx("gardenio-trigger-engine", "gardenio-operator"),
		"controller without the role":     machineCtx("gardenio-controller", "gardenio-operator", "gardenio-machine-accounts"),
	} {
		if d := srv.authorizeOwnership(ctx, "update", dead); d == nil {
			t.Errorf("%s must be denied", name)
		}
	}

	// A lookup error fails closed.
	ms.findAgentByNameErr = errors.New("db down")
	if d := srv.authorizeOwnership(ctrl, "update", dead); d == nil {
		t.Error("an agent lookup error must deny the reclaim")
	}
}

// A non-machine auth method never picks up a reclaim grant, and task is the
// controller's full Ownership, not ReclaimOwnership.
func TestMachineGrantsReclaimOwnership_OnlyForMachineMethod(t *testing.T) {
	srv, _ := reclaimServer(t)
	ctx := WithMachineRoles(withAuthMethod(context.Background(), authMethodOIDC), []string{"gardenio-machine-controller-ownership"})
	ctx = WithAuthIdentity(ctx, AuthIdentity{Type: "machine", Subject: "machine:c"})
	if d := srv.authorizeOwnership(ctx, "update", reclaimBead("gd-b", model.TypeBug, "dead-crew")); d == nil {
		t.Fatal("non-machine auth method must not hold a reclaim grant")
	}
	ctrl := machineCtx("c", "gardenio-machine-controller-ownership")
	if adm, ok := srv.evaluateOwnershipGrants(ctrl, "update", reclaimBead("gd-b", model.TypeBug, "dead-crew")); !ok || adm.grant != "machine:controller-ownership/reclaim" {
		t.Fatalf("controller-ownership must grant reclaim on bug, got %q %v", adm.grant, ok)
	}
	if adm, ok := srv.evaluateOwnershipGrants(ctrl, "update", reclaimBead("gd-t", model.TypeTask, "dead-crew")); !ok || adm.grant != "machine:controller-ownership/ownership" {
		t.Fatalf("task is full Ownership, not ReclaimOwnership, got %q %v", adm.grant, ok)
	}
}

// The reaper's real writes over HTTP: the reopen PATCH (status open, assignee
// cleared, reopen counter) lands on a gone crew's bug; a PATCH to closed and
// the close endpoint are refused, and so is the reopen while the crew lives.
func TestMachineGrants_ControllerReclaim_HTTP(t *testing.T) {
	h := newMachineHarness(t, []string{"gardenio-controller"})
	ms := newMockStore()
	srv := NewBeadsServer(ms, &events.NoopPublisher{})
	srv.userProvisioner = nil
	srv.SetFleetAuth(h.fc)
	now := time.Now()
	putAgentBead(ms, "gd-ag-dead", "dead-crew", model.StatusClosed, now)
	putAgentBead(ms, "gd-ag-live", "live-crew", model.StatusInProgress, now)
	bug := func(id, assignee string) {
		ms.beads[id] = &model.Bead{
			ID: id, Title: "a bug", Type: model.TypeBug, Kind: model.KindIssue,
			Status: model.StatusInProgress, CreatedBy: "oidc:owner@example.com", Assignee: assignee,
			Labels: []string{"project:gardenio"}, Fields: json.RawMessage(`{}`),
		}
	}
	bug("gd-bug-dead", "dead-crew")
	bug("gd-bug-dead2", "dead-crew")
	bug("gd-bug-live", "live-crew")
	handler := srv.NewHTTPHandler(h.verifier, withTestToken(h.fc, machineStaticToken))
	tok := h.saToken(t, "gardenio-controller", "gardenio-operator", "gardenio-machine-controller-ownership")
	reopen := map[string]any{"status": "open", "assignee": "", "fields": map[string]any{"reopen_attempts": "1"}}

	if rec := machineDo(t, handler, tok, http.MethodPatch, "/v1/beads/gd-bug-dead", reopen); rec.Code != http.StatusOK {
		t.Fatalf("reopen of a gone crew's bug: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := ms.beads["gd-bug-dead"]; got.Status != model.StatusOpen || got.Assignee != "" {
		t.Fatalf("bug after reopen: status=%s assignee=%q", got.Status, got.Assignee)
	}

	if rec := machineDo(t, handler, tok, http.MethodPatch, "/v1/beads/gd-bug-live", reopen); rec.Code != http.StatusForbidden {
		t.Fatalf("reopen while the crew is live: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := machineDo(t, handler, tok, http.MethodPatch, "/v1/beads/gd-bug-dead2", map[string]any{"status": "closed"}); rec.Code != http.StatusForbidden {
		t.Fatalf("PATCH to closed: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := machineDo(t, handler, tok, http.MethodPost, "/v1/beads/gd-bug-dead2/close", map[string]any{}); rec.Code != http.StatusForbidden {
		t.Fatalf("close endpoint: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := ms.beads["gd-bug-dead2"]; got.Status != model.StatusInProgress {
		t.Fatalf("bug must stay in_progress, got %s", got.Status)
	}
}

// --- the reclaim kept across the reap (gd-8Irb2vDFf7): once the reaper has
// cleared a gone crew's assignee and stamped reaped_assignee, the controller
// may still UPDATE the bead, so it can claim it for the crew sent back to it. -

// reapedBead is a bug/feature/chore/epic the reaper reopened off reaped: no
// assignee, reaped_assignee naming the crew that held it.
func reapedBead(id string, typ model.BeadType, reaped string) *model.Bead {
	b := reclaimBead(id, typ, "")
	b.Status = model.StatusOpen
	b.Fields = json.RawMessage(`{"` + reapedAssigneeField + `":"` + reaped + `"}`)
	return b
}

func TestAuthorizeOwnership_Machine_ControllerReclaimReaped(t *testing.T) {
	srv, ms := reclaimServer(t)
	ctrl := machineCtx("gardenio-controller", "gardenio-operator", "gardenio-machine-controller-ownership")

	for _, typ := range []model.BeadType{model.TypeBug, model.TypeFeature, model.TypeChore, model.TypeEpic} {
		b := reapedBead("gd-rr-"+string(typ), typ, "dead-crew")
		if d := srv.authorizeOwnership(ctrl, "update", b); d != nil {
			t.Errorf("controller must update a %s reaped off a gone crew, got %+v", typ, d)
		}
		for _, action := range []string{"close", "delete", "dep-add"} {
			if d := srv.authorizeOwnership(ctrl, action, b); d == nil {
				t.Errorf("reclaim is update-only: %s on a reaped %s must be denied", action, typ)
			}
		}
	}

	// A resume crew minted under the dead crew's name is live when its task
	// is claimed for it; the closed bead beside it still admits the claim.
	if d := srv.authorizeOwnership(ctrl, "update", reapedBead("gd-rr-re", model.TypeBug, "respawned-crew")); d != nil {
		t.Errorf("reaped off a crew now resumed under its name: controller update must be allowed, got %+v", d)
	}

	assigned := reapedBead("gd-rr-a", model.TypeBug, "dead-crew")
	assigned.Assignee = "someone@example.com"
	for _, c := range []struct {
		name string
		bead *model.Bead
	}{
		{"no reaped_assignee", reclaimBead("gd-rr1", model.TypeBug, "")},
		{"reaped_assignee names an agent that never died", reapedBead("gd-rr2", model.TypeBug, "live-crew")},
		{"reaped_assignee names a human", reapedBead("gd-rr3", model.TypeBug, "owner@example.com")},
		{"reaped_assignee names no agent", reapedBead("gd-rr4", model.TypeBug, "thread-1792632756-481359")},
		{"reaped_assignee left on a bead someone has since claimed", assigned},
		{"type outside the grant", reapedBead("gd-rr6", "dm", "dead-crew")},
	} {
		if d := srv.authorizeOwnership(ctrl, "update", c.bead); d == nil {
			t.Errorf("%s: controller update must be denied", c.name)
		}
	}

	if d := srv.authorizeOwnership(machineCtx("gardenio-trigger-engine", "gardenio-operator"), "update", reapedBead("gd-rr7", model.TypeBug, "dead-crew")); d == nil {
		t.Error("another machine principal must not hold the reaped reclaim")
	}

	ms.findAgentByNameErr = errors.New("db down")
	if d := srv.authorizeOwnership(ctrl, "update", reapedBead("gd-rr8", model.TypeBug, "dead-crew")); d == nil {
		t.Error("an agent lookup error must deny the reaped reclaim")
	}
}

// The case gd-8Irb2vDFf7 was filed for, over HTTP: a crew working a bug a plan
// gardener filed dies, the reaper reopens the bug with reaped_assignee, and a
// crew is sent back to it. Before the controller's claim the crew's own update
// is refused (the 403 the resume crew hit); the controller's ClaimSpawnTask
// PATCH lands and deletes reaped_assignee; then the crew may update and close
// its bug.
func TestMachineGrants_ControllerClaimsReapedBug_ResumeCrewCloses(t *testing.T) {
	h := newMachineHarness(t, []string{"gardenio-controller"})
	ms := newMockStore()
	srv := NewBeadsServer(ms, &events.NoopPublisher{})
	srv.userProvisioner = nil
	srv.SetFleetAuth(h.fc)
	now := time.Now()
	putAgentBead(ms, "gd-ag-dead", "dead-crew", model.StatusClosed, now)
	putAgentBead(ms, "gd-ag-resume", "resume-crew", model.StatusOpen, now)
	ms.beads["gd-bug"] = &model.Bead{
		ID: "gd-bug", Title: "a bug", Type: model.TypeBug, Kind: model.KindIssue,
		Status: model.StatusOpen, CreatedBy: "gardenio-plan-gardener-for-gd-x-axap",
		Labels: []string{"project:gardenio"}, Fields: json.RawMessage(`{"` + reapedAssigneeField + `":"dead-crew"}`),
	}
	handler := srv.NewHTTPHandler(h.verifier, withTestToken(h.fc, machineStaticToken))
	tok := h.saToken(t, "gardenio-controller", "gardenio-operator", "gardenio-machine-controller-ownership")
	crew := fleetCtx("resume-crew")

	if d := srv.authorizeOwnership(crew, "update", ms.beads["gd-bug"]); d == nil {
		t.Fatal("before the controller's claim the resume crew does not own the bug")
	}

	claim := map[string]any{"status": "in_progress", "assignee": "resume-crew", "fields": map[string]any{reapedAssigneeField: nil}}
	if rec := machineDo(t, handler, tok, http.MethodPatch, "/v1/beads/gd-bug", claim); rec.Code != http.StatusOK {
		t.Fatalf("controller claim of a reaped bug: status=%d body=%s", rec.Code, rec.Body.String())
	}
	got := ms.beads["gd-bug"]
	if got.Status != model.StatusInProgress || got.Assignee != "resume-crew" {
		t.Fatalf("bug after claim: status=%s assignee=%q", got.Status, got.Assignee)
	}
	if strings.Contains(string(got.Fields), reapedAssigneeField) {
		t.Fatalf("the claim must delete %s, fields=%s", reapedAssigneeField, got.Fields)
	}

	for _, action := range []string{"update", "close"} {
		if d := srv.authorizeOwnership(crew, action, got); d != nil {
			t.Errorf("resume crew %s of its claimed bug: %+v", action, d)
		}
	}

	// The record is spent: the controller cannot reclaim the bug a second time.
	if rec := machineDo(t, handler, tok, http.MethodPatch, "/v1/beads/gd-bug", map[string]any{"assignee": "other-crew"}); rec.Code != http.StatusForbidden {
		t.Fatalf("second claim while the resume crew holds it: status=%d body=%s", rec.Code, rec.Body.String())
	}
}
