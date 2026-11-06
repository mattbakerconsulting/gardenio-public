package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"gardenio/kbeads/internal/model"
)

// Residuals of the scoped-principal work (gd-6ULnmLcF6X).

// A scoped principal may unlink only beads both of whose ends are in its
// projects, as it may link only those. A target it cannot see unlinks as a
// missing one does: 200, nothing removed (gd-Mpy0lOLACZ).
func TestScopedWrites_UnlinkAcrossProjectsDenied(t *testing.T) {
	f := newScopedFixture(t)
	f.seedTyped("cl-2", model.TypeTask, "project:claimlayer")
	for _, edge := range []struct {
		from, to string
		want     int
	}{
		{beadClaimlayer, beadGardenio, http.StatusOK},       // target out of project: as missing
		{beadClaimlayer, beadNoLabel, http.StatusOK},        // target unlabeled: as missing
		{beadGardenio, beadClaimlayer, http.StatusNotFound}, // source out of project: as missing
	} {
		f.ms.deps[edge.from] = nil
		f.ms.deps[edge.from] = append(f.ms.deps[edge.from], &model.Dependency{BeadID: edge.from, DependsOnID: edge.to, Type: "blocks"})
		path := "/v1/beads/" + edge.from + "/dependencies?type=blocks&depends_on_id=" + edge.to
		if c, b := f.do(t, tokScopedWriter, http.MethodDelete, path, nil); c != edge.want {
			t.Errorf("unlink %s -> %s: %d %s, want %d", edge.from, edge.to, c, b, edge.want)
		}
		if len(f.ms.deps[edge.from]) != 1 {
			t.Errorf("unlink %s -> %s: a denied unlink still removed the edge", edge.from, edge.to)
		}
	}
	// Positive controls: same-project edge as the scoped caller, cross-project as an admin.
	f.ms.deps[beadClaimlayer] = []*model.Dependency{{BeadID: beadClaimlayer, DependsOnID: "cl-2", Type: "blocks"}}
	if c, b := f.do(t, tokScopedWriter, http.MethodDelete, "/v1/beads/cl-1/dependencies?type=blocks&depends_on_id=cl-2", nil); c != http.StatusOK {
		t.Errorf("unlink within project: %d %s, want 200", c, b)
	}
	f.ms.deps[beadClaimlayer] = []*model.Dependency{{BeadID: beadClaimlayer, DependsOnID: beadGardenio, Type: "blocks"}}
	if c, b := f.do(t, tokOwnerAdmin, http.MethodDelete, "/v1/beads/cl-1/dependencies?type=blocks&depends_on_id=gd-1", nil); c != http.StatusOK {
		t.Errorf("admin unlink across projects: %d %s, want 200", c, b)
	}
}

// Scrub requests are a taint-scrubbing workflow, not a project write: every
// scrub-request route refuses a scoped principal, even one holding the
// operator role, whatever the request names.
func TestScopedWrites_ScrubRequestsRefused(t *testing.T) {
	f := newScopedFixture(t)
	f.seedTyped("cl-scrub", model.TypeScrubRequest, "project:claimlayer")
	body := map[string]any{"taint": "t", "kind": "detaint", "reason": "r"}
	for _, tc := range []struct{ name, method, path string }{
		{"create", http.MethodPost, "/v1/scrub-requests"},
		{"approve", http.MethodPost, "/v1/scrub-requests/cl-scrub/approve"},
		{"reject", http.MethodPost, "/v1/scrub-requests/cl-scrub/reject"},
		{"attest", http.MethodPost, "/v1/scrub-requests/cl-scrub/attest"},
	} {
		if c, b := f.do(t, tokScopedWriter, tc.method, tc.path, body); c != http.StatusForbidden {
			t.Errorf("scoped %s scrub request: %d %s, want 403", tc.name, c, b)
		}
	}
	if got := f.ms.beads["cl-scrub"].Status; got != model.StatusOpen {
		t.Errorf("scrub request status %s after refused calls, want open", got)
	}
}

// requesting_agent is what the replay attributes taints to: no non-admin
// caller may rewrite it through the field endpoints or /result.
func TestEscalation_RequestingAgentLockedOnFieldEndpoints(t *testing.T) {
	f := newScopedFixture(t)
	f.seedEscalation("op-esc", "", "create", taskSpec("claimlayer"), map[string]any{"requesting_agent": "gd-agent-real"})

	for _, c := range []struct {
		name, path string
		body       any
	}{
		{"increment", "/v1/beads/op-esc/increment", map[string]float64{"requesting_agent": 1}},
		{"append", "/v1/beads/op-esc/fields/requesting_agent/append", map[string]any{"element": "a"}},
		{"reorder", "/v1/beads/op-esc/fields/requesting_agent/reorder", map[string]any{"expected": []string{}, "next": []string{}}},
		{"edit", "/v1/beads/op-esc/fields/requesting_agent/edit", map[string]any{"op": "set", "key": "x", "value": "y"}},
		{"Edit mixed case", "/v1/beads/op-esc/fields/Requesting_Agent/edit", map[string]any{"op": "set", "key": "x", "value": "y"}},
	} {
		code, body := f.do(t, tokOperator, http.MethodPost, c.path, c.body)
		if code != http.StatusBadRequest || !strings.Contains(body, "immutable on an escalation") {
			t.Errorf("%s on requesting_agent: %d %s, want 400 immutable", c.name, code, body)
		}
	}
	code, body := f.do(t, tokOperator, http.MethodPost, "/v1/escalations/op-esc/result", map[string]any{
		"result_status": "success", "fields": map[string]string{"requesting_agent": "gd-agent-clean"},
	})
	// P7 Task 10: an operator's /result carries no fields at all.
	if code != http.StatusBadRequest || !strings.Contains(body, "only from a bridge") {
		t.Errorf("/result rewriting requesting_agent: %d %s, want 400 only from a bridge", code, body)
	}
	var fields map[string]any
	_ = json.Unmarshal(f.ms.beads["op-esc"].Fields, &fields)
	if fields["requesting_agent"] != "gd-agent-real" {
		t.Errorf("requesting_agent is %v after refused writes, want gd-agent-real", fields["requesting_agent"])
	}
	// An unrelated field is still writable by the same caller.
	if c, b := f.do(t, tokOperator, http.MethodPost, "/v1/beads/op-esc/fields/notes/edit", map[string]any{"op": "set", "key": "x", "value": "y"}); c == http.StatusBadRequest && strings.Contains(b, "immutable") {
		t.Errorf("unrelated field refused as immutable: %d %s", c, b)
	}
}

// A reject is attributed to the authenticated caller, not to a rejected_by in
// the body.
func TestRejectEscalation_RejectedByIsServerDerived(t *testing.T) {
	f := newScopedFixture(t)
	f.seedEscalation("op-esc", "", "manual_operator_action", map[string]any{"pr": "#1"}, nil)

	code, body := f.do(t, tokOperator, http.MethodPost, "/v1/escalations/op-esc/reject", map[string]any{"reason": "no", "rejected_by": "ceo@example.com"})
	f.wantStatus(t, "reject", code, body, http.StatusOK)
	if got := f.ms.beads["op-esc"].ClosedBy; got != "op@example.com" {
		t.Errorf("closed_by %q, want the authenticated caller op@example.com", got)
	}
	for _, b := range f.ms.beads {
		if b.Type == "audit" && strings.Contains(string(b.Fields), "ceo@example.com") {
			t.Errorf("audit entry carries the spoofed rejector: %s", b.Fields)
		}
	}
}

// The scoped allowlist folds the operation case, so the privileged-spawn check
// must see a "Create" the same way: a spawn cannot dodge it by spelling.
func TestApprove_ScopedHumanCannotApprovePrivilegedSpawnWithCasedOp(t *testing.T) {
	f := newScopedFixture(t)
	makeRoleBead(f.ms, "gd-role-infra-admin", "infra-admin", true)
	spec := map[string]any{"type": "agent", "title": "a", "labels": []string{"project:claimlayer"},
		"fields": map[string]any{"project": "claimlayer", "agent": "a", "role": "infra-admin"}}
	f.seedEscalation("cl-spawn", "project:claimlayer", " Create ", spec, nil)

	c, b := f.approve(t, tokScopedWriter, "cl-spawn")
	f.wantStatus(t, "scoped approve of a cased-op privileged spawn", c, b, http.StatusForbidden)
	if got := f.escalationStatus("cl-spawn"); got != model.StatusOpen {
		t.Errorf("escalation status %s, want open", got)
	}
	if _, isSpawn, _ := escalationSpawnTarget(parseSpawnEscalationFields(f.ms.beads["cl-spawn"])); !isSpawn {
		t.Errorf("escalationSpawnTarget missed a spawn spelled with a cased operation_type")
	}
}

// A denied reject is audited as a reject, not an approve.
func TestRejectEscalation_DeniedIsAuditedAsReject(t *testing.T) {
	f := newScopedFixture(t)
	f.seedEscalation("cl-merge", "project:claimlayer", "merge_window", map[string]any{"pr": "#1"}, nil)

	c, b := f.reject(t, tokScopedWriter, "cl-merge")
	f.wantStatus(t, "scoped reject of a non-allowlisted op", c, b, http.StatusForbidden)
	var actions []string
	for _, bead := range f.ms.beads {
		if bead.Type != "audit" {
			continue
		}
		var fl struct{ Action, Outcome string }
		_ = json.Unmarshal(bead.Fields, &fl)
		if fl.Outcome == "deny" {
			actions = append(actions, fl.Action)
		}
	}
	if len(actions) != 1 || actions[0] != "reject" {
		t.Errorf("denial audited as %v, want [reject]", actions)
	}
}
