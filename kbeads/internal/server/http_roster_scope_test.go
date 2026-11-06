package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"gardenio/kbeads/internal/model"
	"gardenio/kbeads/internal/presence"
)

// Cross-project read-authz regression for GET /v1/agents/roster
// (handleAgentRoster). Both of its hand-built ListBeads filters — the per-actor
// in_progress task lookup and the unclaimed in_progress scan — must flow through
// s.scopeListFilter, so a project-bound fleet token / restricted OIDC user does
// not learn the ID/title/priority of an in_progress bead in a project it cannot
// read. This is the last unremediated member of the ka-0000008132 read-authz
// family (ka-0000005405); the sibling handlers were patched earlier.

// seedRosterScopeBeads seeds presence for two actors and four in_progress beads:
// projA-claimed, projB-claimed (cross-project), projA-unclaimed, projB-unclaimed
// (cross-project). Returns the two actor names.
func seedRosterScopeBeads(srv *BeadsServer, ms *mockStore) (actorA, actorB string) {
	actorA, actorB = "actor-a", "actor-b"

	srv.Presence.RecordHookEvent(presence.HookEvent{Actor: actorA, HookType: "PostToolUse", SessionID: "sa"})
	srv.Presence.RecordHookEvent(presence.HookEvent{Actor: actorB, HookType: "PostToolUse", SessionID: "sb"})

	ms.beads["gd-a-task"] = &model.Bead{ID: "gd-a-task", Title: "projA task", Status: model.StatusInProgress, Assignee: actorA, Fleet: "default"}
	ms.labels["gd-a-task"] = []string{"project:projA"}

	ms.beads["gd-b-task"] = &model.Bead{ID: "gd-b-task", Title: "projB SECRET task", Status: model.StatusInProgress, Assignee: actorB, Fleet: "default"}
	ms.labels["gd-b-task"] = []string{"project:projB"}

	ms.beads["gd-a-orphan"] = &model.Bead{ID: "gd-a-orphan", Title: "projA orphan", Status: model.StatusInProgress, Priority: 1, Fleet: "default"}
	ms.labels["gd-a-orphan"] = []string{"project:projA"}

	ms.beads["gd-b-orphan"] = &model.Bead{ID: "gd-b-orphan", Title: "projB SECRET orphan", Status: model.StatusInProgress, Priority: 2, Fleet: "default"}
	ms.labels["gd-b-orphan"] = []string{"project:projB"}

	return actorA, actorB
}

type rosterScopeResp struct {
	Actors []struct {
		Actor     string `json:"actor"`
		TaskID    string `json:"task_id"`
		TaskTitle string `json:"task_title"`
	} `json:"actors"`
	UnclaimedTasks []struct {
		ID    string `json:"id"`
		Title string `json:"title"`
	} `json:"unclaimed_tasks"`
}

func decodeRosterScope(t *testing.T, rec *httptest.ResponseRecorder) rosterScopeResp {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("roster GET = %d: %s", rec.Code, rec.Body.String())
	}
	var resp rosterScopeResp
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode roster: %v", err)
	}
	return resp
}

// assertNoProjB fails if any projB bead ID surfaced in either the actors' task
// lookups or the unclaimed_tasks list.
func assertNoProjB(t *testing.T, resp rosterScopeResp) {
	t.Helper()
	for _, a := range resp.Actors {
		if a.TaskID == "gd-b-task" {
			t.Errorf("cross-project leak: actor %q surfaced projB task %q (%q)", a.Actor, a.TaskID, a.TaskTitle)
		}
	}
	for _, u := range resp.UnclaimedTasks {
		if u.ID == "gd-b-orphan" {
			t.Errorf("cross-project leak: unclaimed_tasks surfaced projB orphan %q (%q)", u.ID, u.Title)
		}
	}
}

func TestScopeAuthz_AgentRoster_DeniesCrossProjectOIDC(t *testing.T) {
	srv, ms, _ := newTestServer()
	seedRosterScopeBeads(srv, ms)

	// OIDC user restricted to projA.
	makeUserBead(ms, "gd-user-roster", "restricted@example.com", "projA", "crew")
	req := httptest.NewRequest(http.MethodGet, "/v1/agents/roster?stale_threshold_secs=600", nil)
	ctx := oidcCtx("restricted@example.com", []string{RoleOperator})
	ctx = WithFleet(ctx, "default")
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	srv.handleAgentRoster(rec, req)

	resp := decodeRosterScope(t, rec)
	assertNoProjB(t, resp)

	// The in-scope orphan must still be visible (gate must not over-restrict).
	found := false
	for _, u := range resp.UnclaimedTasks {
		if u.ID == "gd-a-orphan" {
			found = true
		}
	}
	if !found {
		t.Errorf("in-scope projA orphan missing from unclaimed_tasks: %+v", resp.UnclaimedTasks)
	}
}

func TestScopeAuthz_AgentRoster_DeniesCrossProjectToken(t *testing.T) {
	srv, ms, _ := newTestServer()
	seedRosterScopeBeads(srv, ms)

	// Project-bound fleet token scoped to projA.
	req := httptest.NewRequest(http.MethodGet, "/v1/agents/roster?stale_threshold_secs=600", nil)
	ctx := WithTokenProject(req.Context(), "projA")
	ctx = withAuthMethod(ctx, authMethodStatic)
	ctx = WithFleet(ctx, "default")
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	srv.handleAgentRoster(rec, req)

	resp := decodeRosterScope(t, rec)
	assertNoProjB(t, resp)
}

func TestScopeAuthz_AgentRoster_AllowsUnrestricted(t *testing.T) {
	srv, ms, _ := newTestServer()
	seedRosterScopeBeads(srv, ms)

	// Unrestricted static/primary token: fleet-wide roster preserved.
	req := httptest.NewRequest(http.MethodGet, "/v1/agents/roster?stale_threshold_secs=600", nil)
	ctx := withAuthMethod(req.Context(), authMethodStatic)
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	srv.handleAgentRoster(rec, req)

	resp := decodeRosterScope(t, rec)

	// Both cross-project orphans must be present for an unrestricted caller.
	var seenA, seenB bool
	for _, u := range resp.UnclaimedTasks {
		switch u.ID {
		case "gd-a-orphan":
			seenA = true
		case "gd-b-orphan":
			seenB = true
		}
	}
	if !seenA || !seenB {
		t.Errorf("unrestricted caller should see both orphans, got %+v", resp.UnclaimedTasks)
	}
}
