package server

import (
	"context"
	"strings"
	"testing"

	"gardenio/kbeads/internal/model"
)

// Regression tests for gd-MgicCcY2rj: close-on-escalation-execute closed
// nothing when the requesting task had no blocks edge, and a desk that wanted
// the task kept open could only say so in a second write that lost the race.

// fleetActorCtx is an agent on a fleet token acting as name.
func fleetActorCtx(name string) context.Context {
	return WithAuthIdentity(context.Background(), AuthIdentity{Type: "fleet", Subject: name})
}

// seedOptedInMerge seeds a closed merge escalation that opted in to closing
// gd-req, and gd-req itself, created by crew-a, with no blocks edge between
// them: the shape gd-wQqqAnagJp and gd-QFxiQF1DWs had when a thread agent's
// edge write was refused on ownership.
func seedOptedInMerge(ms *mockStore, extra map[string]any) {
	fields := map[string]any{
		"operation_type":        "gitlab_mr_merge",
		"requesting_task":       "gd-req",
		"close_requesting_task": true,
	}
	for k, v := range extra {
		fields[k] = v
	}
	seedBead(ms, "gd-esc", model.TypeEscalation, model.StatusClosed, nil, fields)
	seedBead(ms, "gd-req", model.TypeTask, model.StatusInProgress, nil, nil)
	ms.beads["gd-req"].CreatedBy = "crew-a"
}

func TestAfterBeadClosed_EscalationWrapper_NoEdgeClosesWhenResolverMayClose(t *testing.T) {
	s, ms := newAutoCloseServer()
	seedOptedInMerge(ms, nil)

	s.afterBeadClosed(fleetActorCtx("crew-a"), ms.beads["gd-esc"])

	if got := ms.beads["gd-req"].Status; got != model.StatusClosed {
		t.Fatalf("requesting task with no blocks edge: status=%q, want closed", got)
	}
	if ms.beads["gd-req"].ClosedBy != autoCloseActor {
		t.Fatalf("closed_by=%q, want %q", ms.beads["gd-req"].ClosedBy, autoCloseActor)
	}
}

// The field alone is no authority: requesting_task is editable by anyone on
// the shared queue, so a resolver who may not close the task closes nothing.
func TestAfterBeadClosed_EscalationWrapper_NoEdgeLeavesTaskWhenResolverMayNot(t *testing.T) {
	s, ms := newAutoCloseServer()
	seedOptedInMerge(ms, nil)

	s.afterBeadClosed(fleetActorCtx("crew-stranger"), ms.beads["gd-esc"])

	if got := ms.beads["gd-req"].Status; got == model.StatusClosed {
		t.Fatalf("requesting task closed for a resolver that may not close it")
	}
	cs := ms.comments["gd-req"]
	if len(cs) == 0 || !strings.Contains(cs[0].Text, "not auto-closed") {
		t.Fatalf("expected a not-auto-closed comment on the task, got %v", cs)
	}
}

// The spawn-policy sweeper closes an auto-approved escalation on a
// context.Background()-derived ctx (autoApproveSpawnEscalation ->
// replayEscalation -> afterBeadClosed). authorizeOwnership passes an empty
// subject, so without its own check anyone who could edit requesting_task on
// a pending spawn escalation could have the sweeper close any task.
func TestAfterBeadClosed_EscalationWrapper_NoEdgeRefusesIdentitylessResolver(t *testing.T) {
	s, ms := newAutoCloseServer()
	seedOptedInMerge(ms, nil)

	s.afterBeadClosed(context.Background(), ms.beads["gd-esc"])

	if got := ms.beads["gd-req"].Status; got == model.StatusClosed {
		t.Fatalf("requesting task closed with no resolver identity on the context")
	}
	cs := ms.comments["gd-req"]
	if len(cs) == 0 || !strings.Contains(cs[0].Text, "not auto-closed") {
		t.Fatalf("expected a not-auto-closed comment on the task, got %v", cs)
	}
}

// A daemon serving with auth disabled has no subject on any request, so the
// identity check does not apply there.
func TestAfterBeadClosed_EscalationWrapper_NoEdgeClosesWhenAuthDisabled(t *testing.T) {
	s, ms := newAutoCloseServer()
	seedOptedInMerge(ms, nil)
	ctx := withAuthMethod(WithAuthIdentity(context.Background(), AuthIdentity{Type: "none"}), authMethodNone)

	s.afterBeadClosed(ctx, ms.beads["gd-esc"])

	if got := ms.beads["gd-req"].Status; got != model.StatusClosed {
		t.Fatalf("auth disabled: status=%q, want closed", got)
	}
}

func TestAfterBeadClosed_EscalationWrapper_NoEdgeNeedsOptIn(t *testing.T) {
	s, ms := newAutoCloseServer()
	seedOptedInMerge(ms, map[string]any{"close_requesting_task": false})

	s.afterBeadClosed(fleetActorCtx("crew-a"), ms.beads["gd-esc"])

	if got := ms.beads["gd-req"].Status; got == model.StatusClosed {
		t.Fatalf("requesting task closed by an escalation that did not opt in")
	}
}

func TestAfterBeadClosed_EscalationWrapper_ResultMergedFalseClosesNothing(t *testing.T) {
	for _, merged := range []any{false, "false"} {
		s, ms := newAutoCloseServer()
		seedOptedInMerge(ms, map[string]any{"result": map[string]any{"merged": merged}})
		linkBlocks(ms, "gd-req", "gd-esc")

		s.afterBeadClosed(context.Background(), ms.beads["gd-esc"])

		if got := ms.beads["gd-req"].Status; got == model.StatusClosed {
			t.Fatalf("merged=%v: requesting task closed by a merge that did not merge", merged)
		}
	}
}

func TestEscalationResultSaysNotMerged(t *testing.T) {
	for raw, want := range map[string]bool{
		``:                        false,
		`not json`:                false,
		`{"output":"merged"}`:     false,
		`{"merged":true}`:         false,
		`{"merged":"true"}`:       false,
		`{"merged":false}`:        true,
		`{"merged":" False "}`:    true,
		`{"verdict":"mergeable"}`: false,
		`["merged",false]`:        false,
	} {
		if got := escalationResultSaysNotMerged(raw); got != want {
			t.Errorf("escalationResultSaysNotMerged(%q) = %v, want %v", raw, got, want)
		}
	}
}

// The gd-Ma2u0JfCFE race: the desk merged #1417, whose requesting task's fix
// rode #1344, closed the escalation success and removed the opt-in a second
// later; the auto-close had already fired. keep_requesting_task withdraws the
// opt-in in the close itself.
func TestCaptureEscalationResult_KeepRequestingTask(t *testing.T) {
	for _, keep := range []bool{true, false} {
		_, ms, h := newTestServer()
		seedBead(ms, "gd-esc", model.TypeEscalation, model.StatusOpen, nil, map[string]any{
			"operation_type":        "gitlab_mr_merge",
			"requesting_task":       "gd-req",
			"close_requesting_task": true,
		})
		seedBead(ms, "gd-req", model.TypeTask, model.StatusInProgress, nil, nil)
		linkBlocks(ms, "gd-req", "gd-esc")

		rec := doJSON(t, h, "POST", "/v1/escalations/gd-esc/result", map[string]any{
			"executor":             "merge-desk",
			"result_status":        "success",
			"result":               map[string]any{"merged": true},
			"keep_requesting_task": keep,
		})
		requireStatus(t, rec, 200)

		if got := ms.beads["gd-esc"].Status; got != model.StatusClosed {
			t.Fatalf("keep=%v: escalation status=%q, want closed", keep, got)
		}
		taskClosed := ms.beads["gd-req"].Status == model.StatusClosed
		if keep && taskClosed {
			t.Fatalf("keep_requesting_task=true: requesting task was closed")
		}
		if !keep && !taskClosed {
			t.Fatalf("keep_requesting_task=false: requesting task left %q, want closed", ms.beads["gd-req"].Status)
		}
		if keep && parseFieldsToStringMap(ms.beads["gd-esc"].Fields)[closeRequestingTaskField] != "false" {
			t.Fatalf("keep_requesting_task=true: close_requesting_task not withdrawn on the escalation: %s", ms.beads["gd-esc"].Fields)
		}
	}
}
