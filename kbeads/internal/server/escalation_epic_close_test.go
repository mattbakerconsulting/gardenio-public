package server

import (
	"context"
	"testing"

	"gardenio/kbeads/internal/model"
)

// Regression test for gd-0SbA6Iyclm. A plan gardener filed the merge
// escalation for one step's PR; gb escalation create defaulted
// requesting_task to the gardener's task_id, the plan epic, with
// close_requesting_task=true. When the desk merged the PR the whole plan
// closed with 7 steps still open. An epic drains through afterBeadClosed when
// its last child closes, never because an escalation executed.
func TestAfterBeadClosed_EscalationWrapper_NeverClosesEpic(t *testing.T) {
	for _, labels := range [][]string{nil, {"plan"}} {
		s, ms := newAutoCloseServer()
		ctx := context.Background()

		seedBead(ms, "gd-esc", model.TypeEscalation, model.StatusClosed, nil, map[string]any{
			"operation_type":        "gitlab_mr_merge",
			"requesting_task":       "gd-epic",
			"close_requesting_task": true,
		})
		seedBead(ms, "gd-epic", model.TypeEpic, model.StatusOpen,
			append(labels, closeOnEscalationLabel), nil)
		seedBead(ms, "gd-step", model.TypeTask, model.StatusOpen, nil, nil)
		linkParentChild(ms, "gd-step", "gd-epic")
		linkBlocks(ms, "gd-epic", "gd-esc")

		s.afterBeadClosed(ctx, ms.beads["gd-esc"])

		if got := ms.beads["gd-epic"].Status; got != model.StatusOpen {
			t.Fatalf("labels %v: epic closed by its merge escalation, status=%q", labels, got)
		}
		if got := ms.beads["gd-step"].Status; got != model.StatusOpen {
			t.Fatalf("labels %v: step status=%q, want open", labels, got)
		}
	}
}
