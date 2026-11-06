package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gardenio/kbeads/internal/model"
)

// postEscalation files an escalation in project gardenio with fields.
func postEscalation(t *testing.T, h http.Handler, title string, fields map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	return doJSON(t, h, "POST", "/v1/beads", map[string]any{
		"title": title, "type": "escalation", "kind": "data",
		"labels": []string{"project:gardenio"}, "fields": fields,
	})
}

// requireDuplicateOf asserts rec is the P7.2 409 naming id.
func requireDuplicateOf(t *testing.T, rec *httptest.ResponseRecorder, id string) {
	t.Helper()
	requireStatus(t, rec, 409)
	var body map[string]any
	decodeJSON(t, rec, &body)
	if body["code"] != duplicateOpenErrorCode || body["existing_id"] != id {
		t.Fatalf("409 body = %v, want code %s naming %s", body, duplicateOpenErrorCode, id)
	}
	if !strings.Contains(body["error"].(string), "kd comment add "+id) {
		t.Errorf("error %q does not tell the filer to comment on %s", body["error"], id)
	}
}

// TestEscalationDedupe_ChecksReadAsks: the three checks:read asks
// (gd-Lj5RQ4qjkc, gd-GcmGV669ex, gd-nMPLxDgPhq) had one project, one
// operation_type, an empty spec and three requesting tasks; the second and
// third are refused naming the first.
func TestEscalationDedupe_ChecksReadAsks(t *testing.T) {
	_, _, h := newTestServer()
	first := createdID(t, postEscalation(t, h, "Grant reviewer GitHub token checks:read", map[string]any{
		"operation_type": "github_token_scope", "operation_spec": map[string]any{}, "requesting_task": "gd-DqUfYHQ6q9",
	}))
	requireDuplicateOf(t, postEscalation(t, h, "Grant crew GitHub token checks:read", map[string]any{
		"operation_type": "github_token_scope", "operation_spec": map[string]any{}, "requesting_task": "gd-RiDlMItXQq",
	}), first)
	requireDuplicateOf(t, postEscalation(t, h, "Grant the crew GitHub PAT Checks: read", map[string]any{
		"operation_type": "github_token_scope", "operation_spec": map[string]any{}, "requesting_task": "gd-f80Qs2RQfh",
	}), first)
}

func TestEscalationDedupe(t *testing.T) {
	spec := func(scope string) map[string]any { return map[string]any{"scope": scope, "repo": "gardenio"} }
	tests := []struct {
		name   string
		first  map[string]any
		second map[string]any
		dup    bool
	}{
		{"same spec, keys reordered", map[string]any{"operation_type": "github_token_scope", "operation_spec": spec("checks:read")},
			map[string]any{"operation_type": "github_token_scope", "operation_spec": map[string]any{"repo": "gardenio", "scope": "checks:read"}}, true},
		{"alias matches its type", map[string]any{"operation_type": "k8s_apply", "operation_spec": spec("x")},
			map[string]any{"operation_type": "kubectl_apply", "operation_spec": spec("x")}, true},
		{"same requesting_task, other spec", map[string]any{"operation_type": "helm_deploy", "operation_spec": map[string]any{"pr": 1}, "requesting_task": "gd-t1"},
			map[string]any{"operation_type": "helm_deploy", "operation_spec": map[string]any{"pr": 2}, "requesting_task": "gd-t1"}, true},
		// gb pr ready files one per PR, each with --requesting-task.
		{"merge: same requesting_task, other mr_url", map[string]any{"operation_type": "gitlab_mr_merge", "operation_spec": map[string]any{"mr_url": "https://github.com/o/r/pull/1"}, "requesting_task": "gd-t1"},
			map[string]any{"operation_type": "gitlab_mr_merge", "operation_spec": map[string]any{"mr_url": "https://github.com/o/r/pull/2"}, "requesting_task": "gd-t1"}, false},
		{"merge: same mr_url", map[string]any{"operation_type": "gitlab_mr_merge", "operation_spec": map[string]any{"mr_url": "https://github.com/o/r/pull/1"}, "requesting_task": "gd-t1"},
			map[string]any{"operation_type": "gitlab_mr_merge", "operation_spec": map[string]any{"mr_url": "https://github.com/o/r/pull/1"}, "requesting_task": "gd-t2"}, true},
		{"same spec, other target_bead_id", map[string]any{"operation_type": "close", "operation_spec": map[string]any{}, "target_bead_id": "gd-a"},
			map[string]any{"operation_type": "close", "operation_spec": map[string]any{}, "target_bead_id": "gd-b"}, false},
		{"same spec and target_bead_id", map[string]any{"operation_type": "close", "operation_spec": map[string]any{}, "target_bead_id": "gd-a"},
			map[string]any{"operation_type": "close", "operation_spec": map[string]any{}, "target_bead_id": "gd-a"}, true},
		{"other spec", map[string]any{"operation_type": "github_token_scope", "operation_spec": spec("checks:read")},
			map[string]any{"operation_type": "github_token_scope", "operation_spec": spec("actions:write")}, false},
		{"other type, same spec and task", map[string]any{"operation_type": "github_token_scope", "operation_spec": spec("x"), "requesting_task": "gd-t1"},
			map[string]any{"operation_type": "helm_deploy", "operation_spec": spec("x"), "requesting_task": "gd-t1"}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, _, h := newTestServer()
			first := createdID(t, postEscalation(t, h, "first", tc.first))
			rec := postEscalation(t, h, "second", tc.second)
			if tc.dup {
				requireDuplicateOf(t, rec, first)
			} else {
				requireStatus(t, rec, 201)
			}
		})
	}
}

// TestEscalationDedupe_XEscalateCloseTwoBeads: kd close --escalate sends the
// same body for every bead, so two denied closes on different beads are two
// escalations; a retry on the first bead gets the first one back.
func TestEscalationDedupe_XEscalateCloseTwoBeads(t *testing.T) {
	s, ms, _ := newTestServer()
	seedAgent(ms, "gd-agent-x", "crew", "gardenio")
	ctx := agentCtx("gd-agent-x", "crew-x", "gardenio")
	d := &AuthzDenial{Action: "close", BeadType: "task", Escalatable: true, Message: "ownership denied"}
	escalate := func(target string) EscalationResponse {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/v1/beads/"+target+"/close", strings.NewReader(`{}`)).WithContext(ctx)
		req.Header.Set("X-Escalate", "true")
		rec := httptest.NewRecorder()
		if !s.handleEscalation(rec, req, d, json.RawMessage(`{}`), target) {
			t.Fatalf("handleEscalation(%s) returned false", target)
		}
		requireStatus(t, rec, http.StatusAccepted)
		var resp EscalationResponse
		decodeJSON(t, rec, &resp)
		return resp
	}
	a := escalate("gd-a")
	b := escalate("gd-b")
	if a.EscalationID == "" || a.EscalationID == b.EscalationID {
		t.Fatalf("closes on gd-a and gd-b got escalations %q and %q, want two", a.EscalationID, b.EscalationID)
	}
	if got, _, _ := exactFieldString(ms.beads[b.EscalationID].Fields, "target_bead_id"); got != "gd-b" {
		t.Errorf("second escalation target_bead_id = %q, want gd-b", got)
	}
	if again := escalate("gd-a"); again.EscalationID != a.EscalationID {
		t.Errorf("retry on gd-a got %q, want the open %q", again.EscalationID, a.EscalationID)
	}
}

func TestEscalationDedupe_OnlyOpenInProjectUnlessForced(t *testing.T) {
	fields := map[string]any{"operation_type": "github_token_scope", "operation_spec": map[string]any{}}
	_, ms, h := newTestServer()
	first := createdID(t, postEscalation(t, h, "first", fields))

	other := doJSON(t, h, "POST", "/v1/beads", map[string]any{
		"title": "other project", "type": "escalation", "kind": "data",
		"labels": []string{"project:elsewhere"}, "fields": fields,
	})
	requireStatus(t, other, 201)

	forced := doJSON(t, h, "POST", "/v1/beads", map[string]any{
		"title": "forced", "type": "escalation", "kind": "data", "force": true,
		"labels": []string{"project:gardenio"}, "fields": fields,
	})
	requireStatus(t, forced, 201)

	ms.beads[first].Status = model.StatusClosed
	var f model.Bead
	decodeJSON(t, forced, &f)
	ms.beads[f.ID].Status = model.StatusClosed
	requireStatus(t, postEscalation(t, h, "after close", fields), 201)
}

// postDecision files a decision in project gardenio on behalf of agentID.
func postDecision(t *testing.T, h http.Handler, prompt, agentID string) *httptest.ResponseRecorder {
	t.Helper()
	return doJSON(t, h, "POST", "/v1/beads", map[string]any{
		"title": "ruling?", "type": "decision", "kind": "data",
		"labels": []string{"project:gardenio"},
		"fields": map[string]any{
			"prompt":                   prompt,
			"options":                  []map[string]any{{"id": "a", "label": "Merge"}, {"id": "b", "label": "Hold"}},
			"requesting_agent_bead_id": agentID,
		},
	})
}

func TestDecisionDedupe(t *testing.T) {
	_, ms, h := newTestServer()
	seat := func(id, key string) {
		f, _ := json.Marshal(map[string]any{"seat_key": key})
		ms.beads[id] = &model.Bead{ID: id, Type: "agent", Kind: model.KindConfig, Status: model.StatusOpen, Fields: f}
	}
	seat("gd-desk-1", "gardenio/reviewer/merge-desk")
	seat("gd-desk-2", "gardenio/reviewer/merge-desk")
	seat("gd-crew-1", "gardenio/crew/someone")

	first := createdID(t, postDecision(t, h, "PR #2029 needs a ruling: merge or hold?", "gd-desk-1"))
	// The next firing of the same gardener, prompt re-spaced and re-cased.
	requireDuplicateOf(t, postDecision(t, h, "pr #2029 needs a ruling:  merge or hold?", "gd-desk-2"), first)
	requireStatus(t, postDecision(t, h, "PR #2029 needs a ruling: merge or hold?", "gd-crew-1"), 201)
	requireStatus(t, postDecision(t, h, "PR #2030 needs a ruling: merge or hold?", "gd-desk-2"), 201)
}
