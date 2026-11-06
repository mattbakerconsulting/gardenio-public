package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"gardenio/kbeads/internal/model"
)

// addLabelEscalation is an approved kbeads_admin_write add_label escalation
// held and executed by lane, idle for 7h.
func addLabelEscalation(lane string, beads ...string) map[string]any {
	f := laneEscalation("kbeads_admin_write", lane, true)
	f["operation_spec"] = map[string]any{"action": "add_label", "beads": beads, "label": "project:gardenio"}
	return f
}

// seedLabelled seeds a task carrying labels.
func seedLabelled(ms *mockStore, id string, labels ...string) {
	ms.beads[id] = &model.Bead{ID: id, Kind: model.KindIssue, Type: model.TypeTask, Title: id, Status: model.StatusOpen, Labels: labels}
	ms.labels[id] = labels
}

// TestIdleSweep_SatisfiedAddLabelCloses pins gd-rnQLRY3bly: an approved
// add_label escalation whose beads all carry the label already is closed as
// a success by the sweep, with no pingback, and its opted-in requesting task
// closes with it. A partly satisfied spec, or one naming a missing bead, is
// re-notified as before.
func TestIdleSweep_SatisfiedAddLabelCloses(t *testing.T) {
	t.Setenv("BEADS_ESCALATION_LANE_IDLE_HANDUP", "6h")
	s, ms := newRetargetTestServer(true)
	seedLabelled(ms, "gd-t1", "project:gardenio")
	seedLabelled(ms, "gd-t2", "project:gardenio", "plan-step")
	seedLabelled(ms, "gd-t3", "project:other")

	done := addLabelEscalation(clusterCoordinatorLane, "gd-t1", "gd-t2")
	done["requesting_task"], done["close_requesting_task"] = "gd-req", true
	seedRoutedEscalation(ms, "gd-done", "gardenio", done)
	seedLabelled(ms, "gd-req", "project:gardenio")
	linkBlocks(ms, "gd-req", "gd-done")
	seedRoutedEscalation(ms, "gd-partial", "gardenio", addLabelEscalation(clusterCoordinatorLane, "gd-t1", "gd-t3"))
	seedRoutedEscalation(ms, "gd-missing", "gardenio", addLabelEscalation(clusterCoordinatorLane, "gd-t1", "gd-gone"))
	free := laneEscalation("kbeads_admin_write", clusterCoordinatorLane, true)
	free["operation_spec"] = "add project:gardenio to gd-t1"
	seedRoutedEscalation(ms, "gd-free", "gardenio", free)

	if err := s.sweepUnroutedEscalationsLocked(context.Background()); err != nil {
		t.Fatal(err)
	}

	if got := ms.beads["gd-done"].Status; got != model.StatusClosed {
		t.Fatalf("gd-done: status = %q, want closed", got)
	}
	assertEscFields(t, ms, "gd-done", map[string]any{"result_status": "success", "executor": escalationRouteSweepActor})
	if r, _ := escField(t, ms, "gd-done", "result").(map[string]any); r["output"] != "already satisfied: gd-t1, gd-t2 carry project:gardenio; no write needed" {
		t.Errorf("gd-done: result = %v", r)
	}
	if got := escField(t, ms, "gd-done", "pingback_count"); got != nil {
		t.Errorf("gd-done: pingback_count = %v, want no pingback", got)
	}
	if got := ms.beads["gd-req"].Status; got != model.StatusClosed {
		t.Errorf("gd-req: status = %q, want closed with its escalation", got)
	}
	for _, id := range []string{"gd-partial", "gd-missing", "gd-free"} {
		if got := ms.beads[id].Status; got == model.StatusClosed {
			t.Errorf("%s: closed, want it kept", id)
		}
		assertEscFields(t, ms, id, map[string]any{"pingback_source": "idle", "result_status": nil})
	}
}

// TestRerouteEscalations_SatisfiedCloses: the admin reroute closes a stranded
// escalation whose operation is already satisfied instead of moving it, and
// a dry run only reports it.
func TestRerouteEscalations_SatisfiedCloses(t *testing.T) {
	s, ms := newRetargetTestServer(true)
	seedLabelled(ms, "gd-t1", "project:gardenio")
	seedLabelled(ms, "gd-t3")
	stranded := func(beads ...string) map[string]any {
		f := addLabelEscalation("coordinator:laptop", beads...)
		f["held_by"] = "operator"
		return f
	}
	seedRoutedEscalation(ms, "gd-s-done", "gardenio", stranded("gd-t1"))
	seedRoutedEscalation(ms, "gd-s-partial", "gardenio", stranded("gd-t1", "gd-t3"))

	dry := callEscalationVerb(t, operatorCtx("owner@example.com"), s.handleRerouteEscalations, "", "reroute", map[string]any{"approved_unexecuted": true, "dry_run": true})
	if dry.Code != http.StatusOK {
		t.Fatalf("dry run: got %d %s", dry.Code, dry.Body.String())
	}
	if got := ms.beads["gd-s-done"].Status; got == model.StatusClosed {
		t.Fatal("dry run closed gd-s-done")
	}

	rec := callEscalationVerb(t, operatorCtx("owner@example.com"), s.handleRerouteEscalations, "", "reroute", map[string]any{"approved_unexecuted": true})
	if rec.Code != http.StatusOK {
		t.Fatalf("reroute: got %d %s", rec.Code, rec.Body.String())
	}
	if got := rerouted(t, rec.Body.Bytes()); len(got) != 1 || got[0].ID != "gd-s-partial" {
		t.Fatalf("rerouted = %+v, want only gd-s-partial", got)
	}
	if got := ms.beads["gd-s-done"].Status; got != model.StatusClosed {
		t.Fatalf("gd-s-done: status = %q, want closed", got)
	}
	assertEscFields(t, ms, "gd-s-done", map[string]any{"result_status": "success", "pingback_count": nil})
	assertEscFields(t, ms, "gd-s-partial", map[string]any{"execute_by": clusterCoordinatorLane, "pingback_source": "reroute"})
}

// TestHandUp_SatisfiedAddLabelCloses pins gd-Jzu7QMQnZS: a lane's explicit
// hand-up of an approved add_label escalation that is already satisfied
// closes it as a success instead of landing it with the operator; a partly
// satisfied one still hands up.
func TestHandUp_SatisfiedAddLabelCloses(t *testing.T) {
	s, ms := newRetargetTestServer(true)
	seedLabelled(ms, "gd-t1", "project:gardenio")
	seedLabelled(ms, "gd-t3", "project:other")
	done := laneEscalation("kbeads_admin_write", clusterCoordinatorLane, true)
	done["operation_spec"] = map[string]any{"action": "add_label", "bead_ids": []string{"gd-t1"}, "label": "project:gardenio"}
	seedRoutedEscalation(ms, "gd-done", "gardenio", done)
	seedRoutedEscalation(ms, "gd-partial", "gardenio", addLabelEscalation(clusterCoordinatorLane, "gd-t1", "gd-t3"))
	handUp := func(id string) {
		t.Helper()
		rec := callEscalationVerb(t, machineCtx(clusterClient), s.handleHandUpEscalation, id, "hand-up", map[string]any{"reason": "needs refiling"})
		if rec.Code != http.StatusOK {
			t.Fatalf("%s hand-up: got %d %s", id, rec.Code, rec.Body.String())
		}
	}

	handUp("gd-done")
	if got := ms.beads["gd-done"].Status; got != model.StatusClosed {
		t.Fatalf("gd-done: status = %q, want closed", got)
	}
	assertEscFields(t, ms, "gd-done", map[string]any{"result_status": "success", "held_by": clusterCoordinatorLane, "hand_up_reason": nil})

	handUp("gd-partial")
	if got := ms.beads["gd-partial"].Status; got == model.StatusClosed {
		t.Fatal("gd-partial: closed, want it handed up")
	}
	assertEscFields(t, ms, "gd-partial", map[string]any{"held_by": "operator", "hand_up_reason": "needs refiling", "result_status": nil})
}

// TestAddLabelSatisfied_BeadIDs: the filing convention's bead_ids is read,
// ahead of the older beads key.
func TestAddLabelSatisfied_BeadIDs(t *testing.T) {
	ms := newMockStore()
	seedLabelled(ms, "gd-t1", "project:gardenio")
	seedLabelled(ms, "gd-t3")
	get := func(id string) *model.Bead { return ms.beads[id] }
	for _, tc := range []struct {
		name string
		spec string
		want bool
	}{
		{"bead_ids", `{"bead_ids":["gd-t1"],"label":"project:gardenio"}`, true},
		{"beads", `{"beads":["gd-t1"],"label":"project:gardenio"}`, true},
		{"bead_ids wins", `{"bead_ids":["gd-t3"],"beads":["gd-t1"],"label":"project:gardenio"}`, false},
		{"bead_ids unsatisfied", `{"bead_ids":["gd-t1","gd-t3"],"label":"project:gardenio"}`, false},
	} {
		var spec map[string]json.RawMessage
		if err := json.Unmarshal([]byte(tc.spec), &spec); err != nil {
			t.Fatal(err)
		}
		if _, got := addLabelSatisfied(spec, get); got != tc.want {
			t.Errorf("%s: satisfied = %v, want %v", tc.name, got, tc.want)
		}
	}
}
