package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gardenio/kbeads/internal/model"
)

// putRuling stores a decision answered by fields, in project gardenio.
func putRuling(ms *mockStore, id string, status model.Status, fields map[string]any) {
	raw, _ := json.Marshal(fields)
	ms.beads[id] = &model.Bead{ID: id, Title: "ruling " + id, Type: model.TypeDecision, Kind: model.KindData,
		Status: status, Fields: raw}
	ms.labels[id] = []string{"project:gardenio"}
}

// ownerRuling is a decision the owner answered with chosen, whose chosen
// option authorizes op on target ("" for none) with spec (nil for none).
func ownerRuling(chosen, op, target string, spec map[string]any) map[string]any {
	return map[string]any{"chosen": chosen, "resolver_subject": "owner@example.com", "resolver_admin": true,
		"options": []map[string]any{
			{"id": chosen, "label": "yes", "operation_type": op, "target_bead_id": target, "operation_spec": spec},
			{"id": "no", "label": "no"},
		}}
}

// fileCitingEscalation POSTs an escalation of op on target with spec, citing
// gd-ruling, as mutate's caller (nil: the test server's default).
func fileCitingEscalation(h http.Handler, op, target string, spec map[string]any, mutate func(*http.Request)) *httptest.ResponseRecorder {
	fields := map[string]any{"operation_type": op, "operation_spec": spec, implementsDecisionField: "gd-ruling"}
	if target != "" {
		fields["target_bead_id"] = target
	}
	return postJSON(h, "/v1/beads", map[string]any{
		"title": "per ruling", "type": "escalation", "kind": "data",
		"labels": []string{"project:gardenio"}, "fields": fields,
	}, mutate)
}

func putTarget(ms *mockStore, id string) {
	ms.beads[id] = &model.Bead{ID: id, Title: "old", Type: "task", Kind: model.KindIssue, Status: model.StatusOpen}
	ms.labels[id] = []string{"project:gardenio"}
}

// TestRulingApproval_BudgetRaiseAfterExtend: the owner answered "extend" on a
// budget decision; the budget-raise escalation citing it is born approved, by
// the owner, with no second click (P7.5 acceptance).
func TestRulingApproval_BudgetRaiseAfterExtend(t *testing.T) {
	_, ms, h := newTestServer()
	putRuling(ms, "gd-ruling", model.StatusClosed, ownerRuling("extend", "gardener_run_budget", "", map[string]any{"trigger": "gd-trig", "max_runs": 20}))

	rec := doJSON(t, h, "POST", "/v1/beads", map[string]any{
		"title": "raise the gardener run budget", "type": "escalation", "kind": "data",
		"labels": []string{"project:gardenio"},
		"fields": map[string]any{
			"operation_type":        "gardener_run_budget",
			"operation_spec":        map[string]any{"chosen": "extend", "trigger": "gd-trig", "max_runs": 20},
			implementsDecisionField: "gd-ruling",
		},
	})
	requireStatus(t, rec, 201)
	var created map[string]any
	decodeJSON(t, rec, &created)
	esc := ms.beads[created["id"].(string)]
	if esc.Status != model.StatusInProgress {
		t.Errorf("status = %s, want in_progress", esc.Status)
	}
	f := escalationFields(t, esc)
	if f["approved_by"] != "owner@example.com" || f["approval_method"] != approvalMethodDecision || f["approved_at"] == nil {
		t.Errorf("approval = %v/%v/%v, want owner@example.com/decision/set", f["approved_by"], f["approval_method"], f["approved_at"])
	}
}

// TestRulingApproval_ReplaysServerDispatch: a replayable escalation citing a
// ruling (chosen in a list) is replayed at create, as a human approve would.
func TestRulingApproval_ReplaysServerDispatch(t *testing.T) {
	_, ms, h := newTestServer()
	putRuling(ms, "gd-ruling", model.StatusClosed, ownerRuling("all", "update", "gd-target", map[string]any{"title": "new"}))
	putTarget(ms, "gd-target")

	rec := doJSON(t, h, "POST", "/v1/beads", map[string]any{
		"title": "retitle per ruling", "type": "escalation", "kind": "data",
		"labels": []string{"project:gardenio"},
		"fields": map[string]any{
			"operation_type":        "update",
			"target_bead_id":        "gd-target",
			"operation_spec":        map[string]any{"title": "new", "chosen": []string{"some", "all"}},
			implementsDecisionField: "gd-ruling",
		},
	})
	requireStatus(t, rec, 201)
	if got := ms.beads["gd-target"].Title; got != "new" {
		t.Errorf("target title = %q, want the replayed update", got)
	}
}

// TestRulingApproval_RefusesBadCitation: a citation that does not hold is
// refused at create, saying why.
func TestRulingApproval_RefusesBadCitation(t *testing.T) {
	cases := []struct {
		name   string
		status model.Status
		fields map[string]any
		labels []string
		spec   map[string]any
		want   string
	}{
		{"open", model.StatusOpen, ownerRuling("extend", "gardener_run_budget", "", nil), nil, map[string]any{"chosen": "extend"}, "is not closed"},
		{"not admin", model.StatusClosed, map[string]any{"chosen": "extend", "resolver_subject": "machine:bridge", "resolver_admin": false}, nil, map[string]any{"chosen": "extend"}, "gardenio-admin"},
		{"not cited", model.StatusClosed, ownerRuling("extend", "gardener_run_budget", "", nil), nil, map[string]any{"chosen": "drop"}, "not in operation_spec.chosen"},
		{"no chosen", model.StatusClosed, ownerRuling("extend", "gardener_run_budget", "", nil), nil, map[string]any{"max_runs": 20}, "not in operation_spec.chosen"},
		{"other project", model.StatusClosed, ownerRuling("extend", "gardener_run_budget", "", nil), []string{"project:other"}, map[string]any{"chosen": "extend"}, "this escalation's project"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, ms, h := newTestServer()
			putRuling(ms, "gd-ruling", tc.status, tc.fields)
			if tc.labels != nil {
				ms.labels["gd-ruling"] = tc.labels
			}
			rec := doJSON(t, h, "POST", "/v1/beads", map[string]any{
				"title": "raise budget", "type": "escalation", "kind": "data",
				"labels": []string{"project:gardenio"},
				"fields": map[string]any{
					"operation_type":        "gardener_run_budget",
					"operation_spec":        tc.spec,
					implementsDecisionField: "gd-ruling",
				},
			})
			requireStatus(t, rec, 400)
			if !strings.Contains(rec.Body.String(), tc.want) {
				t.Errorf("body %q does not say %q", rec.Body.String(), tc.want)
			}
		})
	}
}

// TestRulingApproval_FloorLeftForPerson: a D3 floor escalation citing a valid
// ruling is filed but stays unapproved.
func TestRulingApproval_FloorLeftForPerson(t *testing.T) {
	_, ms, h := newTestServer()
	putRuling(ms, "gd-ruling", model.StatusClosed, ownerRuling("all", "secret_write", "", map[string]any{"name": "x"}))

	rec := doJSON(t, h, "POST", "/v1/beads", map[string]any{
		"title": "write the secret", "type": "escalation", "kind": "data",
		"labels": []string{"project:gardenio"},
		"fields": map[string]any{
			"operation_type":        "secret_write",
			"operation_spec":        map[string]any{"chosen": "all", "name": "x"},
			implementsDecisionField: "gd-ruling",
		},
	})
	requireStatus(t, rec, 201)
	var created map[string]any
	decodeJSON(t, rec, &created)
	esc := ms.beads[created["id"].(string)]
	if f := escalationFields(t, esc); esc.Status != model.StatusOpen || f["approved_at"] != nil {
		t.Errorf("floor escalation status=%s approved_at=%v, want open and unapproved", esc.Status, f["approved_at"])
	}
}

// TestRulingApproval_RefusesOutOfScope: a ruling approves only the
// operation_type, target_bead_id and operation_spec its chosen option names,
// once. Each refusal leaves nothing filed and the target untouched.
func TestRulingApproval_RefusesOutOfScope(t *testing.T) {
	agent := withMethod(identityMutator(AuthIdentity{Type: "fleet", Subject: "crew-a"}, agentFleetPrefix+"gd-crew"), authMethodStatic)
	update := map[string]any{"title": "new", "chosen": "all"}
	shown := map[string]any{"title": "new"}
	cases := []struct {
		name, op, target string
		ruling           map[string]any
		spec             map[string]any
		want             string
	}{
		{"wrong target", "update", "gd-other", ownerRuling("all", "update", "gd-target", shown), update, `authorizes target_bead_id \"gd-target\"`},
		{"untargeted ruling, targeted ask", "update", "gd-target", ownerRuling("all", "update", "", shown), update, "authorizes target_bead_id"},
		{"wrong operation_type", "close", "gd-target", ownerRuling("all", "update", "gd-target", shown), update, `authorizes operation_type \"update\"`},
		// The desk's scenario: ruling 6 (gd-7qUpu15BBK, chosen=all) names no
		// operation, so citing it on an update of any bead approves nothing.
		{"option names no operation", "update", "gd-target", map[string]any{"chosen": "all", "resolver_subject": "owner@example.com", "resolver_admin": true,
			"options": []map[string]any{{"id": "all", "label": "adopt all six"}}}, update, "names no operation_type"},
		// Operator ruling gd-u2EEbUjh2o: the owner approves the change they
		// were shown, so the filer may not pick the fields.
		{"option names no spec", "update", "gd-target", ownerRuling("all", "update", "gd-target", nil), update, "names no operation_spec"},
		{"different spec", "update", "gd-target", ownerRuling("all", "update", "gd-target", map[string]any{"title": "shown"}), update, "different operation_spec"},
		{"spec with an extra field", "update", "gd-target", ownerRuling("all", "update", "gd-target", shown),
			map[string]any{"title": "new", "description": "unshown", "chosen": "all"}, "different operation_spec"},
	}
	for _, tc := range cases {
		for _, caller := range []struct {
			name   string
			mutate func(*http.Request)
		}{{"default", nil}, {"agent token", agent}} {
			t.Run(tc.name+"/"+caller.name, func(t *testing.T) {
				_, ms, h := newTestServer()
				putRuling(ms, "gd-ruling", model.StatusClosed, tc.ruling)
				putTarget(ms, "gd-target")
				putTarget(ms, "gd-other")
				before := len(ms.beads)
				rec := fileCitingEscalation(h, tc.op, tc.target, tc.spec, caller.mutate)
				if rec.Code == 201 || !strings.Contains(rec.Body.String(), tc.want) {
					t.Fatalf("create = %d %s, want a refusal saying %s", rec.Code, rec.Body.String(), tc.want)
				}
				if len(ms.beads) != before || ms.beads["gd-target"].Title != "old" || ms.beads["gd-other"].Title != "old" {
					t.Errorf("a refused citation filed or replayed something")
				}
			})
		}
	}
}

// TestRulingApproval_SingleUse: a second escalation citing a ruling another
// escalation already cites is refused at create; and if two slip past create
// together, the post-insert re-check approves neither twice.
func TestRulingApproval_SingleUse(t *testing.T) {
	spec := map[string]any{"chosen": "extend", "max_runs": 20}
	ruling := ownerRuling("extend", "gardener_run_budget", "", map[string]any{"max_runs": 20})
	_, ms, h := newTestServer()
	putRuling(ms, "gd-ruling", model.StatusClosed, ruling)
	requireStatus(t, fileCitingEscalation(h, "gardener_run_budget", "", spec, nil), 201)
	rec := fileCitingEscalation(h, "gardener_run_budget", "", spec, nil)
	requireStatus(t, rec, 400)
	if !strings.Contains(rec.Body.String(), "a ruling approves one escalation") {
		t.Errorf("body %q does not say the ruling is spent", rec.Body.String())
	}

	// A racing create: its twin is already inserted when it is approved.
	srv, ms, _ := newTestServer()
	putRuling(ms, "gd-ruling", model.StatusClosed, ruling)
	raw, _ := json.Marshal(map[string]any{"operation_type": "gardener_run_budget", "operation_spec": spec, implementsDecisionField: "gd-ruling"})
	for _, id := range []string{"gd-esc-a", "gd-esc-b"} {
		ms.beads[id] = &model.Bead{ID: id, Title: id, Type: model.TypeEscalation, Kind: model.KindData, Status: model.StatusOpen, Fields: raw}
		ms.labels[id] = []string{"project:gardenio"}
	}
	if got := srv.maybeApproveByRuling(t.Context(), ms.beads["gd-esc-a"]); got != nil {
		t.Errorf("approved %s while %s cites the same ruling", got.ID, "gd-esc-b")
	}
	if f := escalationFields(t, ms.beads["gd-esc-a"]); f["approved_at"] != nil {
		t.Errorf("gd-esc-a approved_at = %v, want unapproved", f["approved_at"])
	}
}

// TestRulingApproval_ForgedResolverStampRefused: resolver_admin is the
// server's stamp. An agent that writes it on create or PATCH gets no admin
// ruling, so citing that decision approves nothing.
func TestRulingApproval_ForgedResolverStampRefused(t *testing.T) {
	forged := ownerRuling("extend", "gardener_run_budget", "", map[string]any{})
	srv, ms, h := newTestServer()
	dec, err := srv.createBead(agentFleetCtx("gd-crew", "crew-a"), createBeadInput{
		Title: "extend?", Type: "decision", Kind: "data", Labels: []string{"project:gardenio"},
		Fields: decisionFields(t, forged),
	})
	if err != nil {
		t.Fatalf("create decision: %v", err)
	}
	ms.beads["gd-ruling"] = ms.beads[dec.ID]
	ms.labels["gd-ruling"] = []string{"project:gardenio"}
	ms.beads["gd-ruling"].Status = model.StatusClosed
	rec := fileCitingEscalation(h, "gardener_run_budget", "", map[string]any{"chosen": "extend"}, nil)
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "gardenio-admin") {
		t.Fatalf("cite a create-forged stamp = %d %s, want 400 not answered by a gardenio-admin", rec.Code, rec.Body.String())
	}

	// A PATCH adding the stamp to an unstamped decision is refused.
	prior := json.RawMessage(`{"prompt":"extend?","chosen":"extend"}`)
	patched, _ := json.Marshal(map[string]any{"prompt": "extend?", "chosen": "extend", "resolver_subject": "owner@example.com", "resolver_admin": true})
	if err := checkDecisionRequestingAgentImmutable(&model.Bead{Type: model.TypeDecision, Fields: patched}, prior); err == nil {
		t.Error("a PATCH wrote resolver_admin on a decision")
	}
}
