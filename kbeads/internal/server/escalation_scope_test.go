package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"gardenio/kbeads/internal/model"
)

// Scoped approvals (gd-tUi5IuttJn): a scoped principal resolves an escalation
// only in its own project, only for an allowlisted operation, and never a
// spawn of a privileged role; its replay is held to the scoped write rules.

// seedEscalation stores an escalation bead with the given project label (""
// for none), operation_type and operation_spec. extra is merged into fields.
func (f *scopedFixture) seedEscalation(id, label, opType string, spec map[string]any, extra map[string]any) {
	fields := map[string]any{"operation_type": opType, "operation_spec": spec}
	for k, v := range extra {
		fields[k] = v
	}
	raw, _ := json.Marshal(fields)
	f.seedTyped(id, model.TypeEscalation, label)
	f.ms.beads[id].Kind = "data"
	f.ms.beads[id].Fields = raw
}

func taskSpec(project string) map[string]any {
	return map[string]any{"type": "task", "title": "replayed", "labels": []string{"project:" + project}}
}

func (f *scopedFixture) approve(t *testing.T, token, id string) (int, string) {
	t.Helper()
	return f.do(t, token, http.MethodPost, "/v1/escalations/"+id+"/approve", map[string]any{})
}

func (f *scopedFixture) reject(t *testing.T, token, id string) (int, string) {
	t.Helper()
	return f.do(t, token, http.MethodPost, "/v1/escalations/"+id+"/reject", map[string]any{"reason": "no"})
}

func (f *scopedFixture) wantStatus(t *testing.T, what string, got int, body string, want int) {
	t.Helper()
	if got != want {
		t.Errorf("%s: %d %s, want %d", what, got, body, want)
	}
}

func (f *scopedFixture) escalationStatus(id string) model.Status { return f.ms.beads[id].Status }

func (f *scopedFixture) hasBeadTitled(title string) bool {
	for _, b := range f.ms.beads {
		if b.Title == title {
			return true
		}
	}
	return false
}

func TestApprove_ScopedHumanApprovesOwnProjectEscalation(t *testing.T) {
	f := newScopedFixture(t)
	f.seedEscalation("cl-esc", "project:claimlayer", "create", taskSpec("claimlayer"), nil)

	c, b := f.approve(t, tokScopedWriter, "cl-esc")
	f.wantStatus(t, "approve own-project allowlisted escalation", c, b, http.StatusOK)
	if !f.hasBeadTitled("replayed") {
		t.Errorf("approved create did not replay")
	}
	if got := f.escalationStatus("cl-esc"); got != model.StatusClosed {
		t.Errorf("escalation status %s, want closed", got)
	}
}

func TestApprove_ScopedHumanCannotApproveOutOfProject(t *testing.T) {
	f := newScopedFixture(t)
	f.seedEscalation("gd-esc", "project:gardenio", "create", taskSpec("gardenio"), nil)
	f.seedEscalation("nl-esc", "", "create", taskSpec("claimlayer"), nil)

	// Out of its projects, the escalation answers as a missing one (gd-7APTAIhdC5).
	if c, b := f.get(t, tokScopedWriter, "/v1/escalations/gd-esc"), ""; c.StatusCode != http.StatusNotFound {
		t.Errorf("read out-of-project escalation: %d %s, want 404", c.StatusCode, b)
	}
	for _, id := range []string{"gd-esc", "nl-esc"} {
		c, b := f.approve(t, tokScopedWriter, id)
		f.wantStatus(t, "approve "+id, c, b, http.StatusNotFound)
		if got := f.escalationStatus(id); got != model.StatusOpen {
			t.Errorf("%s status %s, want open", id, got)
		}
	}
	if f.hasBeadTitled("replayed") {
		t.Errorf("a refused approval replayed its write")
	}
	// Positive control: the owner approves the gardenio one.
	c, b := f.approve(t, tokOwnerAdmin, "gd-esc")
	f.wantStatus(t, "owner approves gardenio escalation", c, b, http.StatusOK)
}

func TestApprove_ScopedHumanCannotApprovePrivilegedSpawn(t *testing.T) {
	f := newScopedFixture(t)
	makeRoleBead(f.ms, "gd-role-infra-admin", "infra-admin", true)
	makeRoleBead(f.ms, "gd-role-crew", "crew", false)
	spec := func(role string) map[string]any {
		return map[string]any{"type": "agent", "title": "a", "labels": []string{"project:claimlayer"},
			"fields": map[string]any{"project": "claimlayer", "agent": "a", "role": role}}
	}
	f.seedEscalation("cl-spawn", "project:claimlayer", "create", spec("infra-admin"), nil)
	// spawn_role is requester-writable metadata: a missing or benign one must not help.
	f.seedEscalation("cl-spawn-benign", "project:claimlayer", "create", spec("infra-admin"), map[string]any{"spawn_role": "crew"})
	f.seedEscalation("cl-rerole", "project:claimlayer", "update", map[string]any{"fields": map[string]any{"role": "infra-admin"}},
		map[string]any{"target_bead_id": "cl-1"})

	for _, id := range []string{"cl-spawn", "cl-spawn-benign", "cl-rerole"} {
		c, b := f.approve(t, tokScopedWriter, id)
		f.wantStatus(t, "scoped approve "+id, c, b, http.StatusForbidden)
		c, b = f.reject(t, tokScopedWriter, id)
		f.wantStatus(t, "scoped reject "+id, c, b, http.StatusForbidden)
		if got := f.escalationStatus(id); got != model.StatusOpen {
			t.Errorf("%s status %s, want open", id, got)
		}
	}
	// Positive control: the owner approves the privileged spawn.
	c, b := f.approve(t, tokOwnerAdmin, "cl-spawn")
	f.wantStatus(t, "owner approves privileged spawn", c, b, http.StatusOK)
}

func TestApprove_ScopedHumanCannotApproveNonAllowlistedOperation(t *testing.T) {
	f := newScopedFixture(t)
	ops := []string{"helm_apply", "helm_deploy", "manual_operator_action", "gitlab_mr_merge", "merge_window", "aws_iam", "grant", "something_new"}
	for _, op := range ops {
		id := "cl-" + op
		f.seedEscalation(id, "project:claimlayer", op, map[string]any{"pr": "#1"}, nil)
		c, b := f.approve(t, tokScopedWriter, id)
		f.wantStatus(t, "scoped approve "+op, c, b, http.StatusForbidden)
		if got := f.escalationStatus(id); got != model.StatusOpen {
			t.Errorf("%s status %s, want open", id, got)
		}
		// Positive control: the owner approves the same escalation.
		c, b = f.approve(t, tokOwnerAdmin, id)
		if op != "grant" { // a grant needs a well-formed spec; the control is the others
			f.wantStatus(t, "owner approve "+op, c, b, http.StatusOK)
		}
	}
}

func TestApprove_ScopedReplayChecksWritePolicy(t *testing.T) {
	f := newScopedFixture(t)
	f.seedTyped("gd-target", model.TypeTask, "project:gardenio")
	f.seedTyped("cl-target", model.TypeTask, "project:claimlayer")
	cases := map[string]struct {
		op     string
		spec   map[string]any
		target string
	}{
		"create-out-of-project":  {"create", taskSpec("gardenio"), ""},
		"create-no-project":      {"create", map[string]any{"type": "task", "title": "replayed"}, ""},
		"create-fields-project":  {"create", map[string]any{"type": "task", "title": "replayed", "labels": []string{"project:claimlayer"}, "fields": map[string]any{"project": "gardenio"}}, ""},
		"create-admin-only-type": {"create", map[string]any{"type": "team", "title": "replayed", "labels": []string{"project:claimlayer"}, "fields": map[string]any{"team": "t"}}, ""},
		"create-agent":           {"create", map[string]any{"type": "agent", "title": "replayed", "labels": []string{"project:claimlayer"}, "fields": map[string]any{"project": "claimlayer", "agent": "a", "role": "crew"}}, ""},
		"update-out-of-project":  {"update", map[string]any{"title": "replayed"}, "gd-target"},
		"update-relabel-out":     {"update", map[string]any{"labels": []string{"project:gardenio"}}, "cl-target"},
		"close-out-of-project":   {"close", map[string]any{}, "gd-target"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			extra := map[string]any{}
			if c.target != "" {
				extra["target_bead_id"] = c.target
			}
			id := "esc-" + name
			f.seedEscalation(id, "project:claimlayer", c.op, c.spec, extra)
			code, body := f.approve(t, tokScopedWriter, id)
			f.wantStatus(t, name, code, body, http.StatusForbidden)
			if got := f.escalationStatus(id); got != model.StatusOpen {
				t.Errorf("escalation status %s, want open", got)
			}
			if f.hasBeadTitled("replayed") {
				t.Errorf("refused replay still wrote a bead")
			}
			if got := f.ms.beads["gd-target"].Status; got != model.StatusOpen {
				t.Errorf("gd-target status %s: the close replay ran", got)
			}
		})
	}

	// Positive controls: in-project update and close replay.
	f.seedEscalation("esc-upd", "project:claimlayer", "update", map[string]any{"title": "retitled"}, map[string]any{"target_bead_id": "cl-target"})
	c, b := f.approve(t, tokScopedWriter, "esc-upd")
	f.wantStatus(t, "in-project update replay", c, b, http.StatusOK)
	if got := f.ms.beads["cl-target"].Title; got != "retitled" {
		t.Errorf("cl-target title %q, want retitled", got)
	}
	f.seedEscalation("esc-close", "project:claimlayer", "close", map[string]any{}, map[string]any{"target_bead_id": "cl-target"})
	c, b = f.approve(t, tokScopedWriter, "esc-close")
	f.wantStatus(t, "in-project close replay", c, b, http.StatusOK)
	if got := f.ms.beads["cl-target"].Status; got != model.StatusClosed {
		t.Errorf("cl-target status %s, want closed", got)
	}
}

func TestReject_ScopedHumanOwnProjectOnly(t *testing.T) {
	f := newScopedFixture(t)
	f.seedEscalation("cl-ok", "project:claimlayer", "create", taskSpec("claimlayer"), nil)
	f.seedEscalation("gd-esc", "project:gardenio", "create", taskSpec("gardenio"), nil)
	f.seedEscalation("cl-helm", "project:claimlayer", "helm_deploy", map[string]any{}, nil)

	// gd-esc is out of its projects, so it answers as missing (gd-7APTAIhdC5).
	for id, want := range map[string]int{"gd-esc": http.StatusNotFound, "cl-helm": http.StatusForbidden} {
		c, b := f.reject(t, tokScopedWriter, id)
		f.wantStatus(t, "scoped reject "+id, c, b, want)
		if got := f.escalationStatus(id); got != model.StatusOpen {
			t.Errorf("%s status %s, want open", id, got)
		}
	}
	c, b := f.reject(t, tokScopedWriter, "cl-ok")
	f.wantStatus(t, "scoped reject own-project", c, b, http.StatusOK)
	if got := f.escalationStatus("cl-ok"); got != model.StatusClosed {
		t.Errorf("cl-ok status %s, want closed", got)
	}
	if f.hasBeadTitled("replayed") {
		t.Errorf("reject replayed the operation")
	}
	// Positive control: the owner rejects the other-project one.
	c, b = f.reject(t, tokOwnerAdmin, "gd-esc")
	f.wantStatus(t, "owner reject", c, b, http.StatusOK)
}

// The global operator path is what it was: an operator or admin approves any
// escalation, whatever its operation_type or project.
func TestApprove_OperatorUnchanged(t *testing.T) {
	f := newScopedFixture(t)
	f.seedEscalation("gd-merge", "project:gardenio", "merge_window", map[string]any{"pr": "#1"}, nil)
	f.seedEscalation("gd-create", "project:gardenio", "create", taskSpec("gardenio"), nil)
	f.seedEscalation("cl-helm", "project:claimlayer", "helm_deploy", map[string]any{}, nil)

	for _, tc := range []struct{ tok, id string }{
		{tokOperator, "gd-merge"}, {tokOperator, "gd-create"}, {tokOwnerAdmin, "cl-helm"},
	} {
		c, b := f.approve(t, tc.tok, tc.id)
		f.wantStatus(t, "approve "+tc.id, c, b, http.StatusOK)
		if got := f.escalationStatus(tc.id); got != model.StatusClosed && tc.id != "gd-merge" && tc.id != "cl-helm" {
			t.Errorf("%s status %s, want closed", tc.id, got)
		}
	}
	if !f.hasBeadTitled("replayed") {
		t.Errorf("operator approval of a create did not replay")
	}
}

func TestScopedApprovableOperations_UnknownDenied(t *testing.T) {
	for op, want := range map[string]bool{"create": true, " UPDATE ": true, "close": true, "grant": false, "": false, "brand_new": false} {
		if got := scopedApprovableOperations[normalizeOperationType(op)]; got != want {
			t.Errorf("scopedApprovableOperations[%q] = %v, want %v", op, got, want)
		}
	}
}
