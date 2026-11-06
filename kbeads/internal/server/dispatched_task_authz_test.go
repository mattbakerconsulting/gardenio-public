package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"gardenio/kbeads/internal/model"
)

// gd-GrJRP7kLBY: a crew dispatched onto a task it did not file may finish it,
// the controller may claim a never-assigned bug for that crew, and a plan's
// gardener may reflag its crews' handles.

// dispatchFixture seeds, all filed by the owner in project gardenio: task
// gd-t with a child gd-tc, epic gd-e with a child gd-ec, bug gd-b (open,
// unassigned), and task gd-x in project other.
func dispatchFixture(ms *mockStore) {
	issue := func(id string, typ model.BeadType, project, parent string) {
		ms.beads[id] = &model.Bead{ID: id, Kind: model.KindIssue, Type: typ, Title: id, Status: model.StatusOpen,
			CreatedBy: "owner@example.com", Fields: json.RawMessage(`{}`)}
		ms.labels[id] = []string{"project:" + project}
		if parent != "" {
			ms.deps[id] = append(ms.deps[id], &model.Dependency{BeadID: id, DependsOnID: parent, Type: model.DepParentChild})
		}
	}
	issue("gd-t", model.TypeTask, "gardenio", "")
	issue("gd-tc", model.TypeBug, "gardenio", "gd-t")
	issue("gd-e", model.TypeEpic, "gardenio", "")
	issue("gd-ec", model.TypeTask, "gardenio", "gd-e")
	issue("gd-b", model.TypeBug, "gardenio", "")
	issue("gd-x", model.TypeTask, "other", "")
	issue("gd-live", model.TypeTask, "gardenio", "")
	ms.beads["gd-live"].Assignee = "crew-live"
	issue("gd-human", model.TypeTask, "gardenio", "")
	ms.beads["gd-human"].Assignee = "someone@example.com"
	issue("gd-dead", model.TypeTask, "gardenio", "")
	ms.beads["gd-dead"].Assignee = "crew-dead"
	agentBead := func(id, name string, status model.Status) {
		ms.beads[id] = &model.Bead{ID: id, Kind: model.KindConfig, Type: "agent", Title: name, Status: status,
			Fields: json.RawMessage(`{"agent":"` + name + `","project":"gardenio"}`)}
	}
	agentBead("gd-live-agent", "crew-live", model.StatusOpen)
	agentBead("gd-dead-agent", "crew-dead", model.StatusClosed)
}

func TestDispatchedTaskRow(t *testing.T) {
	s, ms, _ := newTestServer()
	dispatchFixture(ms)
	crew := planAgent(ms, "gd-a1", "crew-a", "crew", map[string]string{"task_id": "gd-t"})
	onEpic := planAgent(ms, "gd-a2", "crew-e", "crew", map[string]string{"task_id": "gd-e"})
	onOther := planAgent(ms, "gd-a3", "crew-x", "crew", map[string]string{"task_id": "gd-x"})
	other := planAgent(ms, "gd-a4", "crew-b", "crew", map[string]string{"task_id": "gd-b"})
	closed := planAgent(ms, "gd-a5", "crew-gone", "crew", map[string]string{"task_id": "gd-t"})
	ms.beads["gd-a5"].Status = model.StatusClosed
	wrongProject := WithTokenProject(agentFleetCtx("gd-a1", "crew-a"), "other")
	// Any agent may spawn a crew with --task <any task in its project>, so a
	// task_id naming a held task is the forged-at-spawn case.
	onLive := planAgent(ms, "gd-a6", "crew-f1", "crew", map[string]string{"task_id": "gd-live"})
	onHuman := planAgent(ms, "gd-a7", "crew-f2", "crew", map[string]string{"task_id": "gd-human"})
	onDead := planAgent(ms, "gd-a8", "crew-heir", "crew", map[string]string{"task_id": "gd-dead"})
	static := WithTokenProject(grantStaticCtx("crew-a"), "gardenio")
	plainFleet := WithTokenProject(WithFleet(WithAuthIdentity(context.Background(), AuthIdentity{Type: "fleet", Subject: "crew-a"}), "default"), "gardenio")
	machine := machineCtx("gardenio-accounts", "gardenio-machine-accounts")

	for _, tc := range []struct {
		name, action, bead, want string
		ctx                      context.Context
	}{
		{"own task: update", "update", "gd-t", "dispatched-task", crew},
		{"own task: close", "close", "gd-t", "dispatched-task", crew},
		{"own task's child", "update", "gd-tc", "dispatched-task", crew},
		{"own task: never delete", "delete", "gd-t", "", crew},
		{"another agent's task", "update", "gd-b", "", crew},
		{"another agent on this task's child", "update", "gd-tc", "", other},
		{"a closed agent's task", "update", "gd-t", "", closed},
		{"token for another project", "update", "gd-t", "", wrongProject},
		{"task in another project than the agent", "update", "gd-x", "", onOther},
		{"an epic is not a dispatched task", "update", "gd-e", "", onEpic},
		{"an epic's children are left to plan scope", "update", "gd-ec", "", onEpic},
		{"forged at spawn: a task a live agent holds", "close", "gd-live", "", onLive},
		{"forged at spawn: a task a human holds", "update", "gd-human", "", onHuman},
		{"a task a gone agent held", "update", "gd-dead", "dispatched-task", onDead},
		{"a static token", "update", "gd-t", "", static},
		{"a fleet token with no agent identity", "update", "gd-t", "", plainFleet},
		{"a machine principal", "close", "gd-t", "", machine},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bead, _ := ms.GetBead(context.Background(), tc.bead)
			checkOwnershipRow(t, s, tc.ctx, tc.action, bead, tc.want)
		})
	}
}

// The row trusts task_id, so an agent token may not re-point it.
func TestDispatchedTaskRow_TaskIDFixedAtSpawn(t *testing.T) {
	s, ms, _ := newTestServer()
	crew := planAgent(ms, "gd-a1", "crew-a", "crew", map[string]string{"task_id": "gd-t"})
	agent := ms.beads["gd-a1"]
	if d := s.planScopeBindingPatchDenial(crew, "update", agent, json.RawMessage(`{"task_id":"gd-b"}`)); d == nil {
		t.Fatal("an agent token changed its own task_id")
	}
	if d := s.planScopeBindingPatchDenial(crew, "update", agent, json.RawMessage(`{"task_id":"gd-t"}`)); d != nil {
		t.Fatalf("echoing the stored task_id was refused: %v", d)
	}
}

// The same refusals through the HTTP routes: re-pointing task_id by PATCH or a
// field endpoint, and a forged-at-spawn crew closing a held task.
func TestDispatchedTaskRow_HTTP(t *testing.T) {
	s, ms, _ := newTestServer()
	dispatchFixture(ms)
	planAgent(ms, "gd-a1", "crew-a", "crew", map[string]string{"task_id": "gd-t"})
	planAgent(ms, "gd-a6", "crew-f1", "crew", map[string]string{"task_id": "gd-live"})
	mux := http.NewServeMux()
	mux.HandleFunc("PATCH /v1/beads/{id}", requireRole(RoleOperator, s.handleUpdateBead))
	mux.HandleFunc("POST /v1/beads/{id}/close", requireRole(RoleOperator, s.handleCloseBead))
	mux.HandleFunc("POST /v1/beads/{id}/fields/{field}/edit", requireRole(RoleOperator, s.handleEditBeadSubField))
	as := func(beadID, name string) func(*http.Request) {
		return func(r *http.Request) {
			agentToken(beadID, name)(r)
			*r = *r.WithContext(WithTokenProject(r.Context(), "gardenio"))
		}
	}
	for _, tc := range []struct {
		name, method, path string
		body               map[string]any
		mutate             func(*http.Request)
		want               int
	}{
		{"re-point task_id by PATCH", "PATCH", "/v1/beads/gd-a1", map[string]any{"fields": map[string]any{"task_id": "gd-live"}}, as("gd-a1", "crew-a"), http.StatusForbidden},
		{"re-point task_id by a field endpoint", "POST", "/v1/beads/gd-a1/fields/task_id/edit", map[string]any{"op": "set", "key": "k", "value": "gd-live"}, as("gd-a1", "crew-a"), http.StatusForbidden},
		{"forged at spawn: close a held task", "POST", "/v1/beads/gd-live/close", map[string]any{}, as("gd-a6", "crew-f1"), http.StatusForbidden},
		{"own task: claim", "PATCH", "/v1/beads/gd-t", map[string]any{"status": "in_progress", "assignee": "crew-a"}, as("gd-a1", "crew-a"), http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if rec := doFilerJSON(mux, tc.method, tc.path, tc.body, tc.mutate); rec.Code != tc.want {
				t.Fatalf("code = %d, want %d: %s", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
	if got := fieldsStringValue(ms.beads["gd-a1"].Fields, "task_id"); got != "gd-t" {
		t.Fatalf("task_id = %q after the refused writes, want gd-t", got)
	}
}

func TestDispatchClaim(t *testing.T) {
	ctrl := machineCtx("gardenio-controller", "gardenio-machine-controller-ownership")
	str := func(v string) *string { return &v }
	claim := func(assignee string, fields string) *updateBeadInput {
		return &updateBeadInput{Status: str("in_progress"), Assignee: str(assignee), Fields: json.RawMessage(fields)}
	}
	for _, tc := range []struct {
		name  string
		ctx   context.Context
		bead  string
		agent map[string]string // the dispatched agent's extra fields; nil = none
		setup func(*mockStore)
		in    *updateBeadInput
		want  bool
	}{
		{name: "claims an unassigned bug for its single-claim agent", want: true},
		{name: "clears a reap record as it claims", in: claim("crew-a", `{"reaped_assignee":null}`), want: true},
		{name: "a non-machine caller", ctx: agentFleetCtx("gd-a1", "crew-a")},
		{name: "a machine without the grant", ctx: machineCtx("gardenio-canary")},
		{name: "a task is Ownership, not DispatchClaim", bead: "gd-t", agent: map[string]string{"task_id": "gd-t"}},
		{name: "no single-claim opt-in", agent: map[string]string{"task_id": "gd-b"}},
		{name: "the agent was dispatched elsewhere", agent: map[string]string{"task_id": "gd-t", "claim_task_at_spawn": "true"}},
		{name: "the agent is in another project", agent: map[string]string{"task_id": "gd-b", "claim_task_at_spawn": "true", "project": "other"}},
		{name: "the agent is gone", setup: func(ms *mockStore) { ms.beads["gd-a1"].Status = model.StatusClosed }},
		{name: "the bug is assigned", setup: func(ms *mockStore) { ms.beads["gd-b"].Assignee = "crew-live" }},
		{name: "the bug is deferred", setup: func(ms *mockStore) { ms.beads["gd-b"].Status = model.StatusDeferred }},
		{name: "a human assignee", in: claim("someone@example.com", "")},
		{name: "a patch that sets another field", in: claim("crew-a", `{"x":"y"}`)},
		{name: "a patch that changes the title", in: &updateBeadInput{Status: str("in_progress"), Assignee: str("crew-a"), Title: str("t")}},
		{name: "a close is not a claim", in: &updateBeadInput{Status: str("closed"), Assignee: str("crew-a")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, ms, _ := newTestServer()
			dispatchFixture(ms)
			agent := tc.agent
			if agent == nil {
				agent = map[string]string{"task_id": "gd-b", "claim_task_at_spawn": "true"}
			}
			planAgent(ms, "gd-a1", "crew-a", "crew", agent)
			if tc.setup != nil {
				tc.setup(ms)
			}
			ctx, bead, in := tc.ctx, tc.bead, tc.in
			if ctx == nil {
				ctx = ctrl
			}
			if bead == "" {
				bead = "gd-b"
			}
			if in == nil {
				in = claim("crew-a", "")
			}
			b, _ := ms.GetBead(context.Background(), bead)
			if got := s.machineMayClaimDispatchedTask(ctx, b, in); got != tc.want {
				t.Fatalf("machineMayClaimDispatchedTask = %v, want %v", got, tc.want)
			}
		})
	}
}

// The update path admits the controller's claim, and only the claim.
func TestDispatchClaim_UpdatePath(t *testing.T) {
	s, ms, _ := newTestServer()
	dispatchFixture(ms)
	planAgent(ms, "gd-a1", "crew-a", "crew", map[string]string{"task_id": "gd-b", "claim_task_at_spawn": "true"})
	ctrl := machineCtx("gardenio-controller", "gardenio-machine-controller-ownership")
	title, status, assignee := "renamed", "in_progress", "crew-a"
	if _, err := s.updateBead(ctrl, "gd-b", updateBeadInput{Title: &title}); err == nil {
		t.Fatal("the controller retitled a bug it does not own")
	}
	if _, err := s.updateBead(ctrl, "gd-b", updateBeadInput{Status: &status, Assignee: &assignee}); err != nil {
		t.Fatalf("claim refused: %v", err)
	}
	if b := ms.beads["gd-b"]; b.Assignee != "crew-a" || b.Status != model.StatusInProgress {
		t.Fatalf("bug = %s/%s, want crew-a/in_progress", b.Assignee, b.Status)
	}
}

func TestReflagByPlanGardener(t *testing.T) {
	s, ms, _ := newTestServer()
	planScopeFixture(ms)
	gardener := planAgent(ms, "gd-a-gardener", "gardenio-plan-gardener-for-gd-plan-x1", "plan-gardener", map[string]string{"trigger_id": "gd-trig"})
	crew := planAgent(ms, "gd-a-crew", "crew-other", "crew", nil)
	decision := func(plan string) *model.Bead {
		raw, _ := json.Marshal(map[string]string{"prompt": "?", "requesting_plan": plan})
		return &model.Bead{ID: "gd-d", Kind: model.KindData, Type: model.TypeDecision, Status: model.StatusOpen,
			Priority: 2, CreatedBy: "crew-gone", Fields: raw, Labels: []string{"project:gardenio"}}
	}
	p1 := 1
	bump := &updateBeadInput{Priority: &p1}
	title := "x"
	for _, tc := range []struct {
		name string
		ctx  context.Context
		bead *model.Bead
		in   *updateBeadInput
		want bool
	}{
		{"the plan's gardener bumps its crew's decision", gardener, decision("gd-plan"), bump, true},
		{"another plan's decision", gardener, decision("gd-plan2"), bump, false},
		{"a crew that is not the gardener", crew, decision("gd-plan"), bump, false},
		{"no requesting_plan", gardener, decision(""), bump, false},
		{"not a reflag patch", gardener, decision("gd-plan"), &updateBeadInput{Title: &title}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := s.authorizeReflagBySeat(tc.ctx, tc.bead, tc.in); got != tc.want {
				t.Fatalf("authorizeReflagBySeat = %v, want %v", got, tc.want)
			}
		})
	}
}
