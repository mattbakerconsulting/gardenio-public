package server

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"

	beadsv1 "gardenio/kbeads/gen/beads/v1"
	"gardenio/kbeads/internal/model"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const tokFrozenLandAgent = "claimlayer-frozen-agent-tok"

// A frozen land's agents stop writing too (gd-qpFpWcWucD): a per-agent token
// bound to the land was not a scoped principal, so frozenLandDenial let it
// file tasks, comments and decisions through the whole freeze. Now everything
// it writes is refused but gb stop's own writes, an update and a close of its
// agent bead. The same writes in the thawed land are taken, so the freeze is
// what refuses them.
func TestFrozenLandRefusesLandAgentWrites(t *testing.T) {
	refused := []frozenWriteRoute{
		{"create task", http.MethodPost, "/v1/beads", map[string]any{
			"title": "more work", "type": "task", "labels": []string{"project:claimlayer"},
		}, nil},
		{"file decision", http.MethodPost, "/v1/beads", map[string]any{
			"title": "which way?", "type": "decision", "labels": []string{"project:claimlayer"},
			"fields": map[string]any{"prompt": "which way?", "options": []map[string]any{{"id": "a", "label": "A"}, {"id": "b", "label": "B"}}},
		}, nil},
		{"comment", http.MethodPost, "/v1/beads/fz-task/comments", map[string]any{"text": "still here"}, nil},
		{"comment on own agent bead", http.MethodPost, "/v1/beads/cl-frozen-agent/comments", map[string]any{"text": "still here"}, nil},
	}
	seed := func(t *testing.T, frozen string) *scopedFixture {
		f := newScopedFixture(t)
		f.seedFrozenWriteTargets(t)
		f.seedLandProject(t, frozen)
		f.seedLandAgent("cl-frozen-agent", "crew", "claimlayer", tokFrozenLandAgent)
		f.ms.beads["fz-task"].CreatedBy = "cl-frozen-agent"
		f.ms.beads["cl-frozen-agent"].Fields = []byte(`{"agent":"cl-frozen-agent","role":"crew","project":"claimlayer","agent_state":"working"}`)
		return f
	}
	for _, r := range refused {
		t.Run(r.name, func(t *testing.T) {
			f := seed(t, "true")
			if code, body := f.do(t, tokFrozenLandAgent, r.method, r.path, r.body); code != http.StatusForbidden || !strings.Contains(body, "land claimlayer is frozen") {
				t.Fatalf("frozen land: %d %s, want 403 naming the freeze", code, body)
			}
			f = seed(t, "false")
			if code, body := f.do(t, tokFrozenLandAgent, r.method, r.path, r.body); code == http.StatusForbidden {
				t.Errorf("thawed land: %d %s, want it not refused", code, body)
			}
		})
	}

	// gb stop: the wrap-up and agent_state on its own agent bead, then the close.
	f := seed(t, "true")
	if code, body := f.do(t, tokFrozenLandAgent, http.MethodPatch, "/v1/beads/cl-frozen-agent", map[string]any{
		"fields": map[string]any{"agent_state": "stopping"},
	}); code != http.StatusOK {
		t.Errorf("own agent bead update in a frozen land: %d %s, want 200", code, body)
	}
	if code, body := f.do(t, tokFrozenLandAgent, http.MethodPost, "/v1/beads/cl-frozen-agent/close", map[string]any{}); code != http.StatusOK {
		t.Errorf("own agent bead close in a frozen land: %d %s, want 200", code, body)
	}
}

// The agent's own land is checked whatever labels the write carries, and the
// lookup fails closed for it as for a member. An agent token bound to no land
// (the operator's fleet) is not held to a land it is not in.
func TestFrozenLandDenial_LandAgentToken(t *testing.T) {
	f := newScopedFixture(t)
	f.seedLandProject(t, "true")
	agent := WithFleet(WithAuthIdentity(withAuthMethod(context.Background(), authMethodStatic),
		AuthIdentity{Type: "fleet", Subject: "cl-agent"}), agentFleetPrefix+"cl-agent")

	bound := WithTokenProject(agent, "claimlayer")
	if d := frozenLandDenial(bound, f.ms, "create", model.TypeTask, nil); d == nil || d.RequiredScope != "claimlayer" {
		t.Errorf("label-less write by a frozen land's agent: %+v, want refused naming claimlayer", d)
	}
	if d := frozenLandDenial(bound, f.ms, "update", typeAgent, []string{"project:claimlayer"}); d != nil {
		t.Errorf("agent bead update (gb stop) in a frozen land: %+v, want nil", d)
	}
	if d := frozenLandDenial(bound, f.ms, "create", typeAgent, []string{"project:claimlayer"}); d == nil {
		t.Error("agent bead create (a spawn) in a frozen land: nil, want refused")
	}
	if d := frozenLandDenial(WithTokenProject(agent, "claimlayer"), failingProjectStore{f.ms}, "create", model.TypeTask, nil); d == nil {
		t.Error("a failed lookup for an agent token: nil, want refused")
	}
	if d := frozenLandDenial(agent, f.ms, "create", model.TypeTask, []string{"project:gardenio"}); d != nil {
		t.Errorf("unbound agent token outside the frozen land: %+v, want nil", d)
	}
}

// The purse watcher parks an agent once its grace from purse_nudged_at runs
// out, so an agent that wrote the stamp (into the future, or cleared each
// pass) was never parked (gd-qpFpWcWucD). The park fields are the
// controller's: an agent token may not set them on its own agent bead by
// PATCH, by the per-field endpoint, or on a create, while it still writes
// agent_state and echoes the stored stamp back unchanged. The controller's
// token writes them.
func TestAgentParkFields_ControllerOnly(t *testing.T) {
	seed := func(t *testing.T) *scopedFixture {
		f := newScopedFixture(t)
		f.seedLandProject(t, "false")
		f.seedLandAgent("cl-park-agent", "crew", "claimlayer", tokFrozenLandAgent)
		f.ms.beads["cl-park-agent"].Fields = []byte(`{"agent":"cl-park-agent","role":"crew","project":"claimlayer","agent_state":"working","purse_nudged_at":"2026-11-02T10:00:00Z"}`)
		return f
	}
	for _, c := range []struct {
		name   string
		fields map[string]any
	}{
		{"future stamp", map[string]any{"purse_nudged_at": "2099-01-01T00:00:00Z"}},
		{"cleared stamp", map[string]any{"purse_nudged_at": ""}},
		{"park reason", map[string]any{"park_reason": "land_frozen"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := seed(t)
			code, body := f.do(t, tokFrozenLandAgent, http.MethodPatch, "/v1/beads/cl-park-agent", map[string]any{"fields": c.fields})
			if code != http.StatusForbidden || !strings.Contains(body, "written only by the controller") {
				t.Fatalf("agent token PATCH %v: %d %s, want 403 naming the controller", c.fields, code, body)
			}
			if code, body := f.do(t, scopedAdminToken, http.MethodPatch, "/v1/beads/cl-park-agent", map[string]any{"fields": c.fields}); code != http.StatusOK {
				t.Errorf("controller PATCH %v: %d %s, want 200", c.fields, code, body)
			}
		})
	}

	f := seed(t)
	if code, body := f.do(t, tokFrozenLandAgent, http.MethodPatch, "/v1/beads/cl-park-agent", map[string]any{
		"fields": map[string]any{"agent_state": "idle", "purse_nudged_at": "2026-11-02T10:00:00Z"},
	}); code != http.StatusOK {
		t.Errorf("agent_state with the stamp echoed unchanged: %d %s, want 200", code, body)
	}
	if code, body := f.do(t, tokFrozenLandAgent, http.MethodPost, "/v1/beads/cl-park-agent/fields/purse_nudged_at/edit",
		map[string]any{"op": "set", "key": "x", "value": "y"}); code != http.StatusForbidden || !strings.Contains(body, "written only by the controller") {
		t.Errorf("agent token per-field edit of the stamp: %d %s, want 403", code, body)
	}
	if code, body := f.do(t, tokFrozenLandAgent, http.MethodPost, "/v1/beads", map[string]any{
		"title": "cl-park-child", "type": "agent", "labels": []string{"project:claimlayer"},
		"fields": map[string]any{"agent": "cl-park-child", "role": "crew", "project": "claimlayer", "purse_nudged_at": "2099-01-01T00:00:00Z"},
	}); code != http.StatusForbidden || !strings.Contains(body, "written only by the controller") {
		t.Errorf("agent token creates an agent with a future stamp: %d %s, want 403", code, body)
	}
}

// In a frozen land the gb stop exemption is a way out only: the agent token
// may set agent_state stopping on its agent bead, but not paused or
// spawning, and may not blank its agent name, since the watcher parks only
// running agents it can name (gd-qpFpWcWucD). The thawed land takes the same
// patches.
func TestFrozenLandAgentMayOnlyStop(t *testing.T) {
	seed := func(t *testing.T, frozen string) *scopedFixture {
		f := newScopedFixture(t)
		f.seedLandProject(t, frozen)
		f.seedLandAgent("cl-frozen-agent", "crew", "claimlayer", tokFrozenLandAgent)
		f.ms.beads["cl-frozen-agent"].Fields = []byte(`{"agent":"cl-frozen-agent","role":"crew","project":"claimlayer","agent_state":"working"}`)
		return f
	}
	for _, fields := range []map[string]any{
		{"agent_state": "paused"},
		{"agent_state": "spawning"},
		{"agent": "someone-else"},
	} {
		f := seed(t, "true")
		if code, body := f.do(t, tokFrozenLandAgent, http.MethodPatch, "/v1/beads/cl-frozen-agent", map[string]any{"fields": fields}); code != http.StatusForbidden || !strings.Contains(body, "may only stop") {
			t.Errorf("frozen land, %v: %d %s, want 403", fields, code, body)
		}
		f = seed(t, "false")
		if code, body := f.do(t, tokFrozenLandAgent, http.MethodPatch, "/v1/beads/cl-frozen-agent", map[string]any{"fields": fields}); code == http.StatusForbidden {
			t.Errorf("thawed land, %v: %d %s, want it not refused", fields, code, body)
		}
	}
	f := seed(t, "true")
	if code, body := f.do(t, tokFrozenLandAgent, http.MethodPatch, "/v1/beads/cl-frozen-agent", map[string]any{
		"fields": map[string]any{"agent_state": "stopping", "agent": "cl-frozen-agent"},
	}); code != http.StatusOK {
		t.Errorf("frozen land, stopping with the name echoed: %d %s, want 200", code, body)
	}
}

// The per-field endpoints are held to the same way out as PATCH: in a frozen
// land the agent token may not touch agent_state, agent or project on its
// agent bead through edit, append, reorder or increment, since an append of
// paused to agent_state leaves it unparkable (gd-qpFpWcWucD). The thawed land
// takes the same writes.
func TestFrozenLandAgentFieldRoutesRefused(t *testing.T) {
	seed := func(t *testing.T, frozen string) *scopedFixture {
		f := newScopedFixture(t)
		f.seedLandProject(t, frozen)
		f.seedLandAgent("cl-frozen-agent", "crew", "claimlayer", tokFrozenLandAgent)
		f.ms.beads["cl-frozen-agent"].Fields = []byte(`{"agent":"cl-frozen-agent","role":"crew","project":"claimlayer","agent_state":"working"}`)
		return f
	}
	for _, r := range []struct {
		name, path string
		body       map[string]any
	}{
		{"edit append agent_state", "/v1/beads/cl-frozen-agent/fields/agent_state/edit", map[string]any{"op": "append", "element": "paused"}},
		{"edit remove agent", "/v1/beads/cl-frozen-agent/fields/agent/edit", map[string]any{"op": "remove", "element": "cl-frozen-agent"}},
		{"append agent_state", "/v1/beads/cl-frozen-agent/fields/agent_state/append", map[string]any{"element": "paused"}},
		{"reorder agent_state", "/v1/beads/cl-frozen-agent/fields/agent_state/reorder", map[string]any{"expected": []string{"working"}, "next": []string{"paused"}}},
		{"append agent", "/v1/beads/cl-frozen-agent/fields/agent/append", map[string]any{"element": "someone-else"}},
		{"increment agent_state", "/v1/beads/cl-frozen-agent/increment", map[string]any{"agent_state": 1}},
	} {
		t.Run(r.name, func(t *testing.T) {
			f := seed(t, "true")
			if code, body := f.do(t, tokFrozenLandAgent, http.MethodPost, r.path, r.body); code != http.StatusForbidden || !strings.Contains(body, "may only stop") {
				t.Fatalf("frozen land: %d %s, want 403 naming the freeze", code, body)
			}
			f = seed(t, "false")
			if code, body := f.do(t, tokFrozenLandAgent, http.MethodPost, r.path, r.body); code == http.StatusForbidden {
				t.Errorf("thawed land: %d %s, want it not refused", code, body)
			}
		})
	}
}

// The purse watcher lists a land's agents by the project:<land> label, so in a
// frozen land the agent token may not drop it from its agent bead, by DELETE
// or by a PATCH whose labels leave it out (gd-qpFpWcWucD). Other labels still
// come and go. A thawed land refuses it too: no tenant agent strips a project
// label (gd-U9AV7jG8hF).
func TestFrozenLandAgentKeepsLandLabel(t *testing.T) {
	seed := func(t *testing.T, frozen string) *scopedFixture {
		f := newScopedFixture(t)
		f.seedLandProject(t, frozen)
		f.seedLandAgent("cl-frozen-agent", "crew", "claimlayer", tokFrozenLandAgent)
		f.ms.beads["cl-frozen-agent"].Fields = []byte(`{"agent":"cl-frozen-agent","role":"crew","project":"claimlayer","agent_state":"working"}`)
		f.ms.labels["cl-frozen-agent"] = []string{"project:claimlayer", "role:crew"}
		f.ms.beads["cl-frozen-agent"].Labels = f.ms.labels["cl-frozen-agent"]
		return f
	}
	for _, r := range []struct {
		name, method, path string
		body               map[string]any
	}{
		{"delete label", http.MethodDelete, "/v1/beads/cl-frozen-agent/labels/project:claimlayer", nil},
		{"patch labels without it", http.MethodPatch, "/v1/beads/cl-frozen-agent", map[string]any{"labels": []string{"role:crew"}}},
		{"patch labels empty", http.MethodPatch, "/v1/beads/cl-frozen-agent", map[string]any{"labels": []string{}}},
	} {
		t.Run(r.name, func(t *testing.T) {
			f := seed(t, "true")
			if code, body := f.do(t, tokFrozenLandAgent, r.method, r.path, r.body); code != http.StatusForbidden || !strings.Contains(body, "label project:claimlayer") {
				t.Fatalf("frozen land: %d %s, want 403 naming the label", code, body)
			}
			if !slices.Contains(f.ms.labels["cl-frozen-agent"], "project:claimlayer") {
				t.Errorf("frozen land: labels %v, want project:claimlayer kept", f.ms.labels["cl-frozen-agent"])
			}
			f = seed(t, "false")
			if code, body := f.do(t, tokFrozenLandAgent, r.method, r.path, r.body); code != http.StatusForbidden || !strings.Contains(body, "tenant agent may not remove the label project:claimlayer") {
				t.Errorf("thawed land: %d %s, want 403 naming the label", code, body)
			}
		})
	}

	f := seed(t, "true")
	if code, body := f.do(t, tokFrozenLandAgent, http.MethodDelete, "/v1/beads/cl-frozen-agent/labels/role:crew", nil); code == http.StatusForbidden {
		t.Errorf("frozen land, another label: %d %s, want it not refused", code, body)
	}

	// The gRPC RemoveLabel runs the same check.
	ctx := WithTokenProject(WithFleet(WithAuthIdentity(withAuthMethod(context.Background(), authMethodStatic),
		AuthIdentity{Type: "fleet", Subject: "cl-frozen-agent"}), agentFleetPrefix+"cl-frozen-agent"), "claimlayer")
	if _, err := f.srv.RemoveLabel(ctx, &beadsv1.RemoveLabelRequest{BeadId: "cl-frozen-agent", Label: "project:claimlayer"}); status.Code(err) != codes.PermissionDenied || !strings.Contains(err.Error(), "label project:claimlayer") {
		t.Errorf("gRPC RemoveLabel of the land label in a frozen land: %v, want PermissionDenied naming the label", err)
	}
	if d := frozenAgentLabelDenial(ctx, failingProjectStore{f.ms}, f.ms.beads["cl-frozen-agent"], nil); d == nil {
		t.Error("frozenAgentLabelDenial, failed lookup: nil, want refused")
	}
}
