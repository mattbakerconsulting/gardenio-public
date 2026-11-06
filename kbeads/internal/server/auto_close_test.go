package server

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	beadsv1 "gardenio/kbeads/gen/beads/v1"
	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
)

// seedBead inserts a bead + its labels into the mock store.
func seedBead(ms *mockStore, id string, typ model.BeadType, status model.Status, labels []string, fields map[string]any) {
	b := &model.Bead{
		ID:     id,
		Kind:   model.KindIssue,
		Type:   typ,
		Title:  id,
		Status: status,
	}
	// Mirror how the daemon stamps Kind from the type config: data-kind types
	// carry KindData, everything else defaults to KindIssue (deliverable
	// containers like bundle/rich-report are issue-KIND but classify `artifact`
	// via isDeliverableType). Kept in sync with the built-in type configs / live rows.
	switch typ {
	case model.TypeEscalation, model.TypeDecision, model.TypeAdvice, model.TypeReport, "doc":
		b.Kind = model.KindData
	}
	if fields != nil {
		raw, _ := json.Marshal(fields)
		b.Fields = raw
	}
	ms.beads[id] = b
	if len(labels) > 0 {
		ms.labels[id] = append([]string(nil), labels...)
	}
}

// linkParentChild wires a child-first parent-child edge (child bead_id → parent
// depends_on_id), the canonical orientation the subtree CTE expects.
func linkParentChild(ms *mockStore, child, parent string) {
	ms.deps[child] = append(ms.deps[child], &model.Dependency{
		BeadID:      child,
		DependsOnID: parent,
		Type:        model.DepParentChild,
	})
}

// linkBlocks wires a blocks edge (blocked bead_id → blocker depends_on_id),
// mirroring `gb escalation create --requesting-task` (wrapper blocks→ escalation).
func linkBlocks(ms *mockStore, blocked, blocker string) {
	ms.deps[blocked] = append(ms.deps[blocked], &model.Dependency{
		BeadID:      blocked,
		DependsOnID: blocker,
		Type:        model.DepBlocks,
	})
}

func newAutoCloseServer() (*BeadsServer, *mockStore) {
	ms := newMockStore()
	return NewBeadsServer(ms, &events.NoopPublisher{}), ms
}

// TestAfterBeadClosed_NonPlanEpic_HardClosesWhenDrained is the acceptance test
// for criterion #1 (non-plan epic): when the last child of a plain epic closes
// and its MR work is merged, the epic is auto-closed.
func TestAfterBeadClosed_NonPlanEpic_HardClosesWhenDrained(t *testing.T) {
	s, ms := newAutoCloseServer()
	ctx := context.Background()

	seedBead(ms, "gd-epic", model.TypeEpic, model.StatusOpen, nil, nil)
	// One already-closed child with a MERGED MR, plus the child that just closed.
	seedBead(ms, "gd-child1", model.TypeTask, model.StatusClosed, nil, map[string]any{
		"mr_url": "https://gitlab/x/-/merge_requests/1", "mr_merged": "true",
	})
	seedBead(ms, "gd-child2", model.TypeTask, model.StatusClosed, nil, nil)
	linkParentChild(ms, "gd-child1", "gd-epic")
	linkParentChild(ms, "gd-child2", "gd-epic")

	// Simulate the close of the last child.
	closed := ms.beads["gd-child2"]
	s.afterBeadClosed(ctx, closed)

	if got := ms.beads["gd-epic"].Status; got != model.StatusClosed {
		t.Fatalf("expected epic auto-closed, got status=%q", got)
	}
	if ms.beads["gd-epic"].ClosedBy != autoCloseActor {
		t.Fatalf("expected closed_by=%q, got %q", autoCloseActor, ms.beads["gd-epic"].ClosedBy)
	}
	if len(ms.comments["gd-epic"]) == 0 {
		t.Fatalf("expected an audit comment on the auto-closed epic")
	}
}

// TestAfterBeadClosed_Epic_StaysOpenWithOpenChild verifies the drain guard: an
// epic with any remaining open/in_progress/blocked issue-kind child is NOT
// closed. This is the shared reverse-subtree enumeration doing its job.
func TestAfterBeadClosed_Epic_StaysOpenWithOpenChild(t *testing.T) {
	s, ms := newAutoCloseServer()
	ctx := context.Background()

	seedBead(ms, "gd-epic", model.TypeEpic, model.StatusOpen, nil, nil)
	seedBead(ms, "gd-child1", model.TypeTask, model.StatusClosed, nil, nil)
	seedBead(ms, "gd-child2", model.TypeTask, model.StatusOpen, nil, nil) // still open
	linkParentChild(ms, "gd-child1", "gd-epic")
	linkParentChild(ms, "gd-child2", "gd-epic")

	s.afterBeadClosed(ctx, ms.beads["gd-child1"])

	if got := ms.beads["gd-epic"].Status; got != model.StatusOpen {
		t.Fatalf("expected epic to stay open with an open child, got status=%q", got)
	}
}

// TestAfterBeadClosed_Epic_StaysOpenWithUnmergedMR verifies the merged-work
// gate: an epic whose descendants are all closed but one has an UNMERGED MR is
// NOT closed — the code is not on main yet.
func TestAfterBeadClosed_Epic_StaysOpenWithUnmergedMR(t *testing.T) {
	s, ms := newAutoCloseServer()
	ctx := context.Background()

	seedBead(ms, "gd-epic", model.TypeEpic, model.StatusOpen, nil, nil)
	seedBead(ms, "gd-child1", model.TypeTask, model.StatusClosed, nil, map[string]any{
		"mr_url": "https://gitlab/x/-/merge_requests/9", "mr_state": "opened",
	})
	linkParentChild(ms, "gd-child1", "gd-epic")

	s.afterBeadClosed(ctx, ms.beads["gd-child1"])

	if got := ms.beads["gd-epic"].Status; got != model.StatusOpen {
		t.Fatalf("expected epic to stay open with an unmerged MR, got status=%q", got)
	}
}

// TestAfterBeadClosed_Epic_UnmergedMRBeyondCap_NotClosed is the regression test
// for bug ka-0000008187. The merged-work gate (Gate C) used to enumerate closed
// descendants with a single-shot `Limit: 500` fetch and NO sort — so postgres
// applied its default `created_at DESC` order and the cap kept the 500 NEWEST
// closed descendants while SILENTLY DROPPING the oldest. An unmerged MR on one
// of those dropped (oldest) descendants never entered firstUnmergedMR, so a
// >500-closed-descendant epic auto-closed with unmerged work still on a branch.
//
// It seeds 600 closed descendants, all with merged MRs except one among the
// OLDEST — exactly the region a `Limit: 500` + implicit-DESC fetch truncates —
// and asserts the epic STAYS OPEN.
//
// This test only bites once the mock store honors Limit/Offset/Sort (it does,
// as of this change): a mock that returned every match regardless of Limit
// could not exhibit the truncation. It FAILS against the pre-fix single-shot
// `Limit: 500` code (the oldest unmerged child is dropped, so the epic closes)
// and PASSES against the paged scanClosedDescendants walk, which sees every
// descendant regardless of age.
func TestAfterBeadClosed_Epic_UnmergedMRBeyondCap_NotClosed(t *testing.T) {
	s, ms := newAutoCloseServer()
	ctx := context.Background()

	seedBead(ms, "gd-bigepic", model.TypeEpic, model.StatusOpen, nil, nil)

	// 600 closed descendants > the old 500 cap. created_at increases with the
	// index, so index 0 is the OLDEST. A pre-fix `Limit: 500` fetch (implicit
	// created_at DESC) keeps indices 100..599 (the newest 500) and drops
	// indices 0..99 — so the unmerged MR at index 10 is squarely in the dropped
	// region, invisible to the single capped fetch.
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	const total = 600
	const unmergedIdx = 10
	for i := 0; i < total; i++ {
		id := fmt.Sprintf("gd-kid-%04d", i)
		fields := map[string]any{
			"mr_url":    fmt.Sprintf("https://gitlab/x/-/merge_requests/%d", i),
			"mr_merged": "true",
		}
		if i == unmergedIdx {
			// The one descendant whose code is NOT on main yet.
			fields = map[string]any{
				"mr_url":   fmt.Sprintf("https://gitlab/x/-/merge_requests/%d", i),
				"mr_state": "opened",
			}
		}
		seedBead(ms, id, model.TypeTask, model.StatusClosed, nil, fields)
		ms.beads[id].CreatedAt = base.Add(time.Duration(i) * time.Second)
		linkParentChild(ms, id, "gd-bigepic")
	}

	// Drive the drain as if the last child just closed.
	s.afterBeadClosed(ctx, ms.beads[fmt.Sprintf("gd-kid-%04d", total-1)])

	if got := ms.beads["gd-bigepic"].Status; got != model.StatusOpen {
		t.Fatalf("expected epic to STAY open — an unmerged MR on closed descendant #%d (beyond the 500 cap) must defer the drain, got status=%q",
			unmergedIdx, got)
	}
}

// TestAfterBeadClosed_Epic_PagesClosedDescendants asserts on the REQUEST SHAPE
// of the closed-descendant walk (advice ka-0000003555: assert on the requests,
// not the result). With >500 closed descendants and every MR merged, the drain
// must fetch AT LEAST two pages with an ADVANCING Offset — proof the
// enumeration is paged, not a single capped `Limit: 500` window. A single-page
// fetch (the pre-fix shape) would leave only one closed-descendant request and
// fail this test.
func TestAfterBeadClosed_Epic_PagesClosedDescendants(t *testing.T) {
	s, ms := newAutoCloseServer()
	ctx := context.Background()

	seedBead(ms, "gd-bigepic", model.TypeEpic, model.StatusOpen, nil, nil)

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	const total = 600
	for i := 0; i < total; i++ {
		id := fmt.Sprintf("gd-kid-%04d", i)
		seedBead(ms, id, model.TypeTask, model.StatusClosed, nil, map[string]any{
			"mr_url":    fmt.Sprintf("https://gitlab/x/-/merge_requests/%d", i),
			"mr_merged": "true",
		})
		ms.beads[id].CreatedAt = base.Add(time.Duration(i) * time.Second)
		linkParentChild(ms, id, "gd-bigepic")
	}

	s.afterBeadClosed(ctx, ms.beads[fmt.Sprintf("gd-kid-%04d", total-1)])

	// The epic drains fully (all MRs merged, no live gardener, plain epic).
	if got := ms.beads["gd-bigepic"].Status; got != model.StatusClosed {
		t.Fatalf("expected epic auto-closed once all 600 descendants' MRs are merged, got status=%q", got)
	}

	// Collect the closed/partial-descendant enumeration requests and assert
	// they paged: >= 2 requests over the closed-status filter, each advancing
	// Offset by a bounded page window (never a > page-size Limit).
	var closedPageOffsets []int
	for _, f := range ms.listFilters {
		if f.ParentSubtreeID != "gd-bigepic" {
			continue
		}
		if !statusesContain(f.Status, model.StatusClosed) {
			continue // skip the open-subtree Gate-A / gardener enumerations
		}
		if f.Limit > closedKidsPageSize {
			t.Fatalf("closed-descendant page requested Limit %d > page size %d — that is the silent-truncation shape this fix removes (ka-0000008187)",
				f.Limit, closedKidsPageSize)
		}
		closedPageOffsets = append(closedPageOffsets, f.Offset)
	}
	if len(closedPageOffsets) < 2 {
		t.Fatalf("expected the closed-descendant enumeration to page (>= 2 requests) over 600 descendants, got %d request(s): %v",
			len(closedPageOffsets), closedPageOffsets)
	}
	// Offsets must advance (0, 500, ...) — a repeated 0 would be a spin, a
	// single 0 would be the un-paged single fetch.
	if closedPageOffsets[0] != 0 || closedPageOffsets[1] != closedKidsPageSize {
		t.Fatalf("expected advancing page offsets [0, %d, ...], got %v", closedKidsPageSize, closedPageOffsets)
	}
}

// statusesContain reports whether ss includes want.
func statusesContain(ss []model.Status, want model.Status) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

// TestAfterBeadClosed_ChildlessEpic_NotClosed verifies an empty container epic
// (no closed descendants at all) is left alone — auto-close only fires on an
// epic that actually held and drained real work.
func TestAfterBeadClosed_ChildlessEpic_NotClosed(t *testing.T) {
	s, ms := newAutoCloseServer()
	ctx := context.Background()

	seedBead(ms, "gd-epic", model.TypeEpic, model.StatusOpen, nil, nil)
	// A non-issue-kind child (advice) closes but is NOT an issue-kind
	// descendant, so the epic has zero closed issue-kind children.
	seedBead(ms, "gd-note", model.TypeTask, model.StatusClosed, nil, nil)
	// Deliberately NOT linked to the epic.

	s.afterBeadClosed(ctx, ms.beads["gd-note"])

	if got := ms.beads["gd-epic"].Status; got != model.StatusOpen {
		t.Fatalf("expected childless epic to stay open, got status=%q", got)
	}
}

// TestAfterBeadClosed_PlanEpic_FlaggedNotClosed is the acceptance test for
// criterion #1 (plan epic): a drained plan epic (carries fields.plan_state) is
// FLAGGED autoclose-ready + commented, but NOT closed — the operator's
// Mark-complete gate is preserved.
func TestAfterBeadClosed_PlanEpic_FlaggedNotClosed(t *testing.T) {
	s, ms := newAutoCloseServer()
	ctx := context.Background()

	seedBead(ms, "gd-plan", model.TypeEpic, model.StatusOpen, nil, map[string]any{
		"plan_state": "building",
	})
	seedBead(ms, "gd-child1", model.TypeTask, model.StatusClosed, nil, map[string]any{
		"mr_url": "https://gitlab/x/-/merge_requests/2", "mr_merged": "true",
	})
	linkParentChild(ms, "gd-child1", "gd-plan")

	s.afterBeadClosed(ctx, ms.beads["gd-child1"])

	if got := ms.beads["gd-plan"].Status; got != model.StatusOpen {
		t.Fatalf("expected plan epic to STAY open (operator gate), got status=%q", got)
	}
	if !hasLabel(ms.labels["gd-plan"], autoCloseReadyLabel) {
		t.Fatalf("expected plan epic flagged %q, labels=%v", autoCloseReadyLabel, ms.labels["gd-plan"])
	}
	if len(ms.comments["gd-plan"]) == 0 {
		t.Fatalf("expected an audit comment explaining the flag")
	}

	// Idempotency: a second drained-child close must not re-flag / re-comment.
	before := len(ms.comments["gd-plan"])
	s.afterBeadClosed(ctx, ms.beads["gd-child1"])
	if len(ms.comments["gd-plan"]) != before {
		t.Fatalf("expected no duplicate flag comment; before=%d after=%d", before, len(ms.comments["gd-plan"]))
	}
}

// TestAfterBeadClosed_CascadesToParentPlan verifies the upward cascade: closing
// the last child of a phase sub-epic auto-closes that sub-epic, which in turn
// drains and FLAGS the parent plan epic. This is the ka-0000003733 shape
// (Phase-6 epic fully closed → plan should surface as complete).
func TestAfterBeadClosed_CascadesToParentPlan(t *testing.T) {
	s, ms := newAutoCloseServer()
	ctx := context.Background()

	seedBead(ms, "gd-plan", model.TypeEpic, model.StatusOpen, nil, map[string]any{"plan_state": "building"})
	seedBead(ms, "gd-phase", model.TypeEpic, model.StatusOpen, nil, nil) // plain sub-epic
	seedBead(ms, "gd-leaf", model.TypeTask, model.StatusClosed, nil, map[string]any{
		"mr_url": "https://gitlab/x/-/merge_requests/3", "mr_merged": "true",
	})
	linkParentChild(ms, "gd-phase", "gd-plan")
	linkParentChild(ms, "gd-leaf", "gd-phase")

	s.afterBeadClosed(ctx, ms.beads["gd-leaf"])

	if got := ms.beads["gd-phase"].Status; got != model.StatusClosed {
		t.Fatalf("expected phase sub-epic auto-closed, got status=%q", got)
	}
	// The plan is a plan epic → flagged, not closed.
	if got := ms.beads["gd-plan"].Status; got != model.StatusOpen {
		t.Fatalf("expected parent plan to stay open (gated), got status=%q", got)
	}
	if !hasLabel(ms.labels["gd-plan"], autoCloseReadyLabel) {
		t.Fatalf("expected parent plan flagged %q after cascade, labels=%v", autoCloseReadyLabel, ms.labels["gd-plan"])
	}
}

// TestAfterBeadClosed_EscalationWrapper_ClosesOptedIn is the acceptance test for
// criterion #2: when an escalation closes, a task blocked-by it that carries the
// close-on-escalation-execute label is auto-closed.
func TestAfterBeadClosed_EscalationWrapper_ClosesOptedIn(t *testing.T) {
	s, ms := newAutoCloseServer()
	ctx := context.Background()

	seedBead(ms, "gd-esc", model.TypeEscalation, model.StatusClosed, nil, nil)
	seedBead(ms, "gd-wrapper", model.TypeTask, model.StatusOpen, []string{closeOnEscalationLabel}, nil)
	linkBlocks(ms, "gd-wrapper", "gd-esc") // wrapper blocks→ escalation

	s.afterBeadClosed(ctx, ms.beads["gd-esc"])

	if got := ms.beads["gd-wrapper"].Status; got != model.StatusClosed {
		t.Fatalf("expected opted-in wrapper auto-closed, got status=%q", got)
	}
	if ms.beads["gd-wrapper"].ClosedBy != autoCloseActor {
		t.Fatalf("expected closed_by=%q, got %q", autoCloseActor, ms.beads["gd-wrapper"].ClosedBy)
	}
	if len(ms.comments["gd-wrapper"]) == 0 {
		t.Fatalf("expected an audit comment on the auto-closed wrapper")
	}
}

// TestAfterBeadClosed_EscalationWrapper_LeavesPlainTask verifies the opt-in
// gate: a task blocked-by the escalation but WITHOUT the contract label is left
// open — a plain dependency is not a close-on-execute contract.
func TestAfterBeadClosed_EscalationWrapper_LeavesPlainTask(t *testing.T) {
	s, ms := newAutoCloseServer()
	ctx := context.Background()

	seedBead(ms, "gd-esc", model.TypeEscalation, model.StatusClosed, nil, nil)
	seedBead(ms, "gd-plain", model.TypeTask, model.StatusOpen, nil, nil) // no opt-in label
	linkBlocks(ms, "gd-plain", "gd-esc")

	s.afterBeadClosed(ctx, ms.beads["gd-esc"])

	if got := ms.beads["gd-plain"].Status; got != model.StatusOpen {
		t.Fatalf("expected plain blocked task to stay open, got status=%q", got)
	}
}

// TestAfterBeadClosed_EscalationWrapper_CascadesToParentEpic verifies that
// closing a wrapper task (via its escalation executing) drains the wrapper's
// parent epic — the ka-0000005629 shape.
func TestAfterBeadClosed_EscalationWrapper_CascadesToParentEpic(t *testing.T) {
	s, ms := newAutoCloseServer()
	ctx := context.Background()

	seedBead(ms, "gd-epic", model.TypeEpic, model.StatusOpen, nil, nil)
	seedBead(ms, "gd-esc", model.TypeEscalation, model.StatusClosed, nil, nil)
	seedBead(ms, "gd-wrapper", model.TypeTask, model.StatusOpen, []string{closeOnEscalationLabel}, nil)
	linkParentChild(ms, "gd-wrapper", "gd-epic")
	linkBlocks(ms, "gd-wrapper", "gd-esc")

	s.afterBeadClosed(ctx, ms.beads["gd-esc"])

	if got := ms.beads["gd-wrapper"].Status; got != model.StatusClosed {
		t.Fatalf("expected wrapper auto-closed, got status=%q", got)
	}
	if got := ms.beads["gd-epic"].Status; got != model.StatusClosed {
		t.Fatalf("expected parent epic drained + auto-closed after wrapper close, got status=%q", got)
	}
}

// TestAfterBeadClosed_EscalationWrapper_FieldOptInClosesRegardlessOfLabel
// verifies fix (a) for gd-91b7zMOR2z: an escalation carrying
// close_requesting_task=true closes its wrapper even though the wrapper
// carries none of the legacy closeOnEscalationLabel — the opt-in now lives on
// the escalation, not the task.
func TestAfterBeadClosed_EscalationWrapper_FieldOptInClosesRegardlessOfLabel(t *testing.T) {
	s, ms := newAutoCloseServer()
	ctx := context.Background()

	seedBead(ms, "gd-esc", model.TypeEscalation, model.StatusClosed, nil, map[string]any{
		"requesting_task":       "gd-wrapper",
		"close_requesting_task": true,
	})
	seedBead(ms, "gd-wrapper", model.TypeTask, model.StatusOpen, nil, nil) // no legacy label
	linkBlocks(ms, "gd-wrapper", "gd-esc")

	s.afterBeadClosed(ctx, ms.beads["gd-esc"])

	if got := ms.beads["gd-wrapper"].Status; got != model.StatusClosed {
		t.Fatalf("expected wrapper auto-closed via escalation field opt-in, got status=%q", got)
	}
}

// TestAfterBeadClosed_EscalationWrapper_LaterEscalationDoesNotInheritStaleLabel
// is the regression test for gd-91b7zMOR2z: a task once wrapped by an
// escalation filed WITH --close-on-execute (leaving the deprecated task label
// stamped) must NOT be closed by a LATER, unrelated escalation against the
// same task that was filed WITHOUT the flag — even though the stale label is
// still sitting on the task. The later escalation's own
// close_requesting_task=false field must win over the legacy label.
func TestAfterBeadClosed_EscalationWrapper_LaterEscalationDoesNotInheritStaleLabel(t *testing.T) {
	s, ms := newAutoCloseServer()
	ctx := context.Background()

	// Escalation A executed earlier with --close-on-execute; the task still
	// carries the sticky label it stamped, mirroring the observed incident.
	seedBead(ms, "gd-esc-a", model.TypeEscalation, model.StatusClosed, nil, map[string]any{
		"close_requesting_task": true,
	})
	seedBead(ms, "gd-task", model.TypeTask, model.StatusOpen, []string{closeOnEscalationLabel}, nil)
	linkBlocks(ms, "gd-task", "gd-esc-a")

	// Escalation B is filed later against the SAME task, WITHOUT the flag —
	// e.g. a review escalation carrying a conditional verdict.
	seedBead(ms, "gd-esc-b", model.TypeEscalation, model.StatusOpen, nil, map[string]any{
		"close_requesting_task": false,
	})
	linkBlocks(ms, "gd-task", "gd-esc-b")

	// B succeeds and closes.
	escB := ms.beads["gd-esc-b"]
	escB.Status = model.StatusClosed
	s.afterBeadClosed(ctx, escB)

	if got := ms.beads["gd-task"].Status; got != model.StatusOpen {
		t.Fatalf("expected task to stay open — escalation B did not opt in, got status=%q", got)
	}
}

// TestAfterBeadClosed_EscalationWrapper_GithubPRMergeNeverCascades verifies fix
// (c) for gd-91b7zMOR2z: a github_pr_merge escalation's success must never
// cascade-close its requesting task — the PR merge, not the review verdict, is
// the terminal event for that operation type — even when the escalation itself
// carries close_requesting_task=true and the wrapper carries the legacy label.
func TestAfterBeadClosed_EscalationWrapper_GithubPRMergeNeverCascades(t *testing.T) {
	s, ms := newAutoCloseServer()
	ctx := context.Background()

	seedBead(ms, "gd-esc", model.TypeEscalation, model.StatusClosed, nil, map[string]any{
		"operation_type":        "github_pr_merge",
		"close_requesting_task": true,
	})
	seedBead(ms, "gd-wrapper", model.TypeTask, model.StatusOpen, []string{closeOnEscalationLabel}, nil)
	linkBlocks(ms, "gd-wrapper", "gd-esc")

	s.afterBeadClosed(ctx, ms.beads["gd-esc"])

	if got := ms.beads["gd-wrapper"].Status; got != model.StatusOpen {
		t.Fatalf("expected github_pr_merge escalation to never cascade-close its task, got status=%q", got)
	}
}

// TestAfterBeadClosed_EscalationWrapper_VerdictsNeverCascade (P7 Task 14,
// spec D9): a review verdict's success never closes its requesting task.
func TestAfterBeadClosed_EscalationWrapper_VerdictsNeverCascade(t *testing.T) {
	for _, op := range []string{"github_pr_review", "doc_review", " Doc_Review "} {
		s, ms := newAutoCloseServer()
		seedBead(ms, "gd-esc", model.TypeEscalation, model.StatusClosed, nil, map[string]any{
			"operation_type":        op,
			"requesting_task":       "gd-req",
			"close_requesting_task": true,
			"result_status":         "success",
		})
		seedBead(ms, "gd-req", model.TypeTask, model.StatusInProgress, nil, nil)
		linkBlocks(ms, "gd-req", "gd-esc")

		s.afterBeadClosed(context.Background(), ms.beads["gd-esc"])

		if got := ms.beads["gd-req"].Status; got != model.StatusInProgress {
			t.Errorf("%q: requesting task status = %q, want in_progress", op, got)
		}
	}
}

// TestAfterBeadClosed_EscalationWrapper_ClosesOnlyRequestingTask is the
// regression test for gd-ifFUTNHbqm. Merge escalation gd-dNQTxW4wo1 (#1315,
// close_requesting_task=true) executed and every bead with a `blocks` edge on
// it closed: two other merge escalations waiting for #1315 to land, and a task
// with no opt-in label. Only the escalation's own requesting_task may close.
func TestAfterBeadClosed_EscalationWrapper_ClosesOnlyRequestingTask(t *testing.T) {
	s, ms := newAutoCloseServer()
	ctx := context.Background()

	seedBead(ms, "gd-esc", model.TypeEscalation, model.StatusClosed, nil, map[string]any{
		"operation_type":        "gitlab_mr_merge",
		"requesting_task":       "gd-req",
		"close_requesting_task": true,
	})
	seedBead(ms, "gd-req", model.TypeBug, model.StatusInProgress, nil, nil)
	// Another merge escalation, filed with --close-on-execute for its own
	// task, that waits for gd-esc to merge first.
	seedBead(ms, "gd-esc-waiting", model.TypeEscalation, model.StatusOpen, nil, map[string]any{
		"operation_type":        "gitlab_mr_merge",
		"requesting_task":       "gd-step",
		"close_requesting_task": true,
	})
	seedBead(ms, "gd-step", model.TypeTask, model.StatusInProgress, nil, nil)
	seedBead(ms, "gd-plain", model.TypeTask, model.StatusInProgress, nil, nil)
	seedBead(ms, "gd-labelled", model.TypeTask, model.StatusInProgress, []string{closeOnEscalationLabel}, nil)
	linkBlocks(ms, "gd-req", "gd-esc")
	linkBlocks(ms, "gd-esc-waiting", "gd-esc")
	linkBlocks(ms, "gd-step", "gd-esc-waiting")
	linkBlocks(ms, "gd-plain", "gd-esc")
	linkBlocks(ms, "gd-labelled", "gd-esc")

	s.afterBeadClosed(ctx, ms.beads["gd-esc"])

	if got := ms.beads["gd-req"].Status; got != model.StatusClosed {
		t.Fatalf("expected the requesting task closed, got status=%q", got)
	}
	for _, id := range []string{"gd-esc-waiting", "gd-step", "gd-plain", "gd-labelled"} {
		if got := ms.beads[id].Status; got == model.StatusClosed {
			t.Errorf("%s: closed by an escalation it only waits on (gd-ifFUTNHbqm)", id)
		}
	}
}

// TestAfterBeadClosed_EscalationWrapper_LegacyLabelOnlyOnRequestingTask
// covers the back-compat path (no close_requesting_task field): the legacy
// label still closes the requesting task, but no longer a different labelled
// bead that merely waits on the escalation.
func TestAfterBeadClosed_EscalationWrapper_LegacyLabelOnlyOnRequestingTask(t *testing.T) {
	s, ms := newAutoCloseServer()
	ctx := context.Background()

	seedBead(ms, "gd-esc", model.TypeEscalation, model.StatusClosed, nil, map[string]any{
		"requesting_task": "gd-req",
	})
	seedBead(ms, "gd-req", model.TypeTask, model.StatusOpen, []string{closeOnEscalationLabel}, nil)
	seedBead(ms, "gd-other", model.TypeTask, model.StatusOpen, []string{closeOnEscalationLabel}, nil)
	linkBlocks(ms, "gd-req", "gd-esc")
	linkBlocks(ms, "gd-other", "gd-esc")

	s.afterBeadClosed(ctx, ms.beads["gd-esc"])

	if got := ms.beads["gd-req"].Status; got != model.StatusClosed {
		t.Fatalf("expected labelled requesting task closed, got status=%q", got)
	}
	if got := ms.beads["gd-other"].Status; got != model.StatusOpen {
		t.Fatalf("expected non-requesting labelled task left open, got status=%q", got)
	}
}

// TestAfterBeadClosed_EscalationWrapper_FieldOptInWithoutRequestingTask: an
// opt-in that names no requesting task has nothing to close; it must not fall
// back to closing every blocked-by bead.
func TestAfterBeadClosed_EscalationWrapper_FieldOptInWithoutRequestingTask(t *testing.T) {
	s, ms := newAutoCloseServer()
	ctx := context.Background()

	seedBead(ms, "gd-esc", model.TypeEscalation, model.StatusClosed, nil, map[string]any{
		"close_requesting_task": true,
	})
	seedBead(ms, "gd-blocked", model.TypeTask, model.StatusOpen, []string{closeOnEscalationLabel}, nil)
	linkBlocks(ms, "gd-blocked", "gd-esc")

	s.afterBeadClosed(ctx, ms.beads["gd-esc"])

	if got := ms.beads["gd-blocked"].Status; got != model.StatusOpen {
		t.Fatalf("expected blocked task left open, got status=%q", got)
	}
}

// TestAfterBeadClosed_EscalationWrapper_NeverClosesEscalationOrDecision: even
// when named as the requesting task, an escalation or decision closes on its
// own execution or resolution.
func TestAfterBeadClosed_EscalationWrapper_NeverClosesEscalationOrDecision(t *testing.T) {
	for _, typ := range []model.BeadType{model.TypeEscalation, model.TypeDecision} {
		s, ms := newAutoCloseServer()
		ctx := context.Background()

		seedBead(ms, "gd-esc", model.TypeEscalation, model.StatusClosed, nil, map[string]any{
			"requesting_task":       "gd-req",
			"close_requesting_task": true,
		})
		seedBead(ms, "gd-req", typ, model.StatusOpen, []string{closeOnEscalationLabel}, nil)
		linkBlocks(ms, "gd-req", "gd-esc")

		s.afterBeadClosed(ctx, ms.beads["gd-esc"])

		if got := ms.beads["gd-req"].Status; got != model.StatusOpen {
			t.Fatalf("%s: expected left open, got status=%q", typ, got)
		}
	}
}

// TestAfterBeadClosed_EscalationWrapper_CommentCarriesOperator: the audit
// comment inherits the wrapper's operator, since comments.operated_by_id is
// NOT NULL and a comment without one is dropped in production (gd-ifFUTNHbqm).
func TestAfterBeadClosed_EscalationWrapper_CommentCarriesOperator(t *testing.T) {
	s, ms := newAutoCloseServer()
	ctx := context.Background()

	seedBead(ms, "gd-esc", model.TypeEscalation, model.StatusClosed, nil, map[string]any{
		"requesting_task":       "gd-req",
		"close_requesting_task": true,
	})
	seedBead(ms, "gd-req", model.TypeTask, model.StatusOpen, nil, nil)
	opID := int64(11)
	ms.beads["gd-req"].OperatedBy = "oidc:alice"
	ms.beads["gd-req"].OperatedByID = &opID
	linkBlocks(ms, "gd-req", "gd-esc")

	s.afterBeadClosed(ctx, ms.beads["gd-esc"])

	cs := ms.comments["gd-req"]
	if len(cs) == 0 {
		t.Fatalf("expected an audit comment on the closed requesting task")
	}
	if cs[0].OperatedByID == nil || *cs[0].OperatedByID != opID {
		t.Fatalf("expected comment operated_by_id=%d, got %v", opID, cs[0].OperatedByID)
	}
}

// TestAfterBeadClosed_IgnoresNonEpicParent verifies a closed task whose parent
// is another TASK (not an epic) does not attempt to close the parent — only
// epics are containers to drain.
func TestAfterBeadClosed_IgnoresNonEpicParent(t *testing.T) {
	s, ms := newAutoCloseServer()
	ctx := context.Background()

	seedBead(ms, "gd-parent-task", model.TypeTask, model.StatusOpen, nil, nil)
	seedBead(ms, "gd-sub", model.TypeTask, model.StatusClosed, nil, nil)
	linkParentChild(ms, "gd-sub", "gd-parent-task")

	s.afterBeadClosed(ctx, ms.beads["gd-sub"])

	if got := ms.beads["gd-parent-task"].Status; got != model.StatusOpen {
		t.Fatalf("expected non-epic parent left untouched, got status=%q", got)
	}
}

// TestAfterBeadClosed_DecisionGate_ParksOpenGatedBead is the acceptance test for
// the intake-thrash fix (ka-0000005620): when a decision closes, an OPEN bead
// blocked-by it via a `blocks`→decision edge is auto-parked at `blocked` so it
// does not fall back into the ready lane and thrash. This is the intake-gate
// shape — the decision carries NO `requesting_task`, i.e. the gated bead is not
// the decision's own requester (contrast ka-0000000025, which exempts that one).
func TestAfterBeadClosed_DecisionGate_ParksOpenGatedBead(t *testing.T) {
	s, ms := newAutoCloseServer()
	ctx := context.Background()

	seedBead(ms, "gd-decision", model.TypeDecision, model.StatusClosed, nil, nil)
	seedBead(ms, "gd-gated", model.TypeTask, model.StatusOpen, nil, nil)
	linkBlocks(ms, "gd-gated", "gd-decision") // gated blocks→ decision

	s.afterBeadClosed(ctx, ms.beads["gd-decision"])

	if got := ms.beads["gd-gated"].Status; got != model.StatusBlocked {
		t.Fatalf("expected gated bead auto-parked to blocked, got status=%q", got)
	}
	if len(ms.comments["gd-gated"]) == 0 {
		t.Fatalf("expected an audit comment on the parked bead")
	}
}

// TestAfterBeadClosed_DecisionGate_ParksInProgressGatedBead verifies an
// in_progress gated bead is also parked — an agent may have picked it up before
// the decision resolved; parking pulls it back out of the active lane.
func TestAfterBeadClosed_DecisionGate_ParksInProgressGatedBead(t *testing.T) {
	s, ms := newAutoCloseServer()
	ctx := context.Background()

	seedBead(ms, "gd-decision", model.TypeDecision, model.StatusClosed, nil, nil)
	seedBead(ms, "gd-gated", model.TypeTask, model.StatusInProgress, nil, nil)
	linkBlocks(ms, "gd-gated", "gd-decision")

	s.afterBeadClosed(ctx, ms.beads["gd-decision"])

	if got := ms.beads["gd-gated"].Status; got != model.StatusBlocked {
		t.Fatalf("expected in_progress gated bead parked to blocked, got status=%q", got)
	}
}

// TestAfterBeadClosed_DecisionGate_LeavesTerminalGatedBead verifies the status
// guard: a gated bead that is already closed / deferred / blocked is NOT touched
// — parking only pulls actively-surfaceable (open/in_progress) work out of the
// lane, and re-closing the decision must be idempotent.
func TestAfterBeadClosed_DecisionGate_LeavesTerminalGatedBead(t *testing.T) {
	for _, st := range []model.Status{model.StatusClosed, model.StatusDeferred, model.StatusBlocked} {
		s, ms := newAutoCloseServer()
		ctx := context.Background()

		seedBead(ms, "gd-decision", model.TypeDecision, model.StatusClosed, nil, nil)
		seedBead(ms, "gd-gated", model.TypeTask, st, nil, nil)
		linkBlocks(ms, "gd-gated", "gd-decision")

		s.afterBeadClosed(ctx, ms.beads["gd-decision"])

		if got := ms.beads["gd-gated"].Status; got != st {
			t.Fatalf("expected gated bead in status %q left untouched, got %q", st, got)
		}
	}
}

// TestAfterBeadClosed_DecisionGate_OptOutLabelReleases verifies the opt-out: a
// gated bead carrying release-on-decision-close is NOT parked — it is meant to
// release into the ready lane when its gating decision resolves.
func TestAfterBeadClosed_DecisionGate_OptOutLabelReleases(t *testing.T) {
	s, ms := newAutoCloseServer()
	ctx := context.Background()

	seedBead(ms, "gd-decision", model.TypeDecision, model.StatusClosed, nil, nil)
	seedBead(ms, "gd-gated", model.TypeTask, model.StatusOpen, []string{decisionGateReopenLabel}, nil)
	linkBlocks(ms, "gd-gated", "gd-decision")

	s.afterBeadClosed(ctx, ms.beads["gd-decision"])

	if got := ms.beads["gd-gated"].Status; got != model.StatusOpen {
		t.Fatalf("expected opt-out bead left open (released), got status=%q", got)
	}
}

// TestAfterBeadClosed_DecisionGate_IgnoresNonBlocksEdge verifies only `blocks`
// edges park: a parent-child edge from a bead to the decision (a decision
// parented under its originating task/session) must NOT park that bead.
func TestAfterBeadClosed_DecisionGate_IgnoresNonBlocksEdge(t *testing.T) {
	s, ms := newAutoCloseServer()
	ctx := context.Background()

	seedBead(ms, "gd-decision", model.TypeDecision, model.StatusClosed, nil, nil)
	seedBead(ms, "gd-related", model.TypeTask, model.StatusOpen, nil, nil)
	// A parent-child edge (related is the parent of the decision) — NOT a blocks edge.
	linkParentChild(ms, "gd-decision", "gd-related")

	s.afterBeadClosed(ctx, ms.beads["gd-decision"])

	if got := ms.beads["gd-related"].Status; got != model.StatusOpen {
		t.Fatalf("expected non-blocks-linked bead left open, got status=%q", got)
	}
}

// TestAfterBeadClosed_DecisionGate_ParksMultipleGatedBeads verifies every
// distinct bead blocked-by the decision is parked (the sibling-gate shape:
// several beads waiting on one operator sign-off).
func TestAfterBeadClosed_DecisionGate_ParksMultipleGatedBeads(t *testing.T) {
	s, ms := newAutoCloseServer()
	ctx := context.Background()

	seedBead(ms, "gd-decision", model.TypeDecision, model.StatusClosed, nil, nil)
	seedBead(ms, "gd-gated-a", model.TypeTask, model.StatusOpen, nil, nil)
	seedBead(ms, "gd-gated-b", model.TypeBug, model.StatusOpen, nil, nil)
	linkBlocks(ms, "gd-gated-a", "gd-decision")
	linkBlocks(ms, "gd-gated-b", "gd-decision")

	s.afterBeadClosed(ctx, ms.beads["gd-decision"])

	for _, id := range []string{"gd-gated-a", "gd-gated-b"} {
		if got := ms.beads[id].Status; got != model.StatusBlocked {
			t.Fatalf("expected %s parked to blocked, got status=%q", id, got)
		}
	}
}

// TestAfterBeadClosed_DecisionGate_ReleasesOwnRequestingTask is the acceptance
// test for ka-0000000025: a decision's OWN requesting task (the bead named in
// the decision's `requesting_task` field — the `--requesting-task` contract,
// advice ka-0000001746) is NOT parked when the decision resolves. Resolving that
// decision FREES the work to continue (the Slack-bridge respawn revives an agent
// to pick it up); parking it to `blocked` behind a now-CLOSED decision would
// re-hide just-unblocked work with an empty open_blockers[], invisible to every
// open-gated lane. It must stay open (or in_progress) so a lane picks it up.
func TestAfterBeadClosed_DecisionGate_ReleasesOwnRequestingTask(t *testing.T) {
	for _, st := range []model.Status{model.StatusOpen, model.StatusInProgress} {
		s, ms := newAutoCloseServer()
		ctx := context.Background()

		seedBead(ms, "gd-decision", model.TypeDecision, model.StatusClosed, nil, map[string]any{
			"requesting_task": "gd-req",
			"chosen":          "fix-now",
		})
		seedBead(ms, "gd-req", model.TypeBug, st, nil, nil)
		linkBlocks(ms, "gd-req", "gd-decision") // requesting task blocks→ its own decision

		s.afterBeadClosed(ctx, ms.beads["gd-decision"])

		if got := ms.beads["gd-req"].Status; got != st {
			t.Fatalf("expected requesting task left in %q (released, not parked), got status=%q", st, got)
		}
		if len(ms.comments["gd-req"]) != 0 {
			t.Fatalf("expected no park comment on the released requesting task, got %d", len(ms.comments["gd-req"]))
		}
	}
}

// TestAfterBeadClosed_DecisionGate_ParksOtherGatedBeadsButNotRequestingTask
// verifies the two edge shapes are disambiguated by `requesting_task`: on the
// SAME decision close, the decision's own requesting task is released while a
// DIFFERENT bead gated by the same decision (an intake-gate, ka-0000005620) is
// still parked to blocked.
func TestAfterBeadClosed_DecisionGate_ParksOtherGatedBeadsButNotRequestingTask(t *testing.T) {
	s, ms := newAutoCloseServer()
	ctx := context.Background()

	seedBead(ms, "gd-decision", model.TypeDecision, model.StatusClosed, nil, map[string]any{
		"requesting_task": "gd-req",
	})
	seedBead(ms, "gd-req", model.TypeBug, model.StatusOpen, nil, nil)
	seedBead(ms, "gd-other", model.TypeTask, model.StatusOpen, nil, nil)
	linkBlocks(ms, "gd-req", "gd-decision")
	linkBlocks(ms, "gd-other", "gd-decision")

	s.afterBeadClosed(ctx, ms.beads["gd-decision"])

	if got := ms.beads["gd-req"].Status; got != model.StatusOpen {
		t.Fatalf("expected requesting task released (open), got status=%q", got)
	}
	if got := ms.beads["gd-other"].Status; got != model.StatusBlocked {
		t.Fatalf("expected non-requesting gated bead still parked to blocked, got status=%q", got)
	}
}

// TestAfterBeadClosed_OrphanPark_ParksOpenChildOfClosedEpic is the acceptance
// test for ka-0000000504: closing an epic that still has an OPEN parent-child
// child parks that child at `blocked` (out of the ready lane) rather than
// stranding it claimable under a dead parent.
func TestAfterBeadClosed_OrphanPark_ParksOpenChildOfClosedEpic(t *testing.T) {
	s, ms := newAutoCloseServer()
	ctx := context.Background()

	seedBead(ms, "gd-epic", model.TypeEpic, model.StatusClosed, nil, nil)
	seedBead(ms, "gd-child", model.TypeTask, model.StatusOpen, nil, nil)
	linkParentChild(ms, "gd-child", "gd-epic")

	s.afterBeadClosed(ctx, ms.beads["gd-epic"])

	if got := ms.beads["gd-child"].Status; got != model.StatusBlocked {
		t.Fatalf("expected orphaned child auto-parked to blocked, got status=%q", got)
	}
	if len(ms.comments["gd-child"]) == 0 {
		t.Fatalf("expected an audit comment on the parked child")
	}
}

// TestAfterBeadClosed_OrphanPark_ParksInProgressChild verifies an in_progress
// child is also parked — an agent may already hold it when the parent is closed.
func TestAfterBeadClosed_OrphanPark_ParksInProgressChild(t *testing.T) {
	s, ms := newAutoCloseServer()
	ctx := context.Background()

	seedBead(ms, "gd-epic", model.TypeEpic, model.StatusClosed, nil, nil)
	seedBead(ms, "gd-child", model.TypeTask, model.StatusInProgress, nil, nil)
	linkParentChild(ms, "gd-child", "gd-epic")

	s.afterBeadClosed(ctx, ms.beads["gd-epic"])

	if got := ms.beads["gd-child"].Status; got != model.StatusBlocked {
		t.Fatalf("expected in_progress orphaned child parked to blocked, got status=%q", got)
	}
}

// TestAfterBeadClosed_OrphanPark_ParksChildOfClosedNonEpicParent is the second,
// compounding-gap acceptance test (ka-0000000504): a NON-epic parent (task, bug,
// molecule, …) closed over its open child must also park the child. This is the
// path maybeAutoCloseParents' `parent.Type != TypeEpic` skip could never cover;
// 43 of 56 measured stranded rows had a non-epic closed parent.
func TestAfterBeadClosed_OrphanPark_ParksChildOfClosedNonEpicParent(t *testing.T) {
	for _, pt := range []model.BeadType{model.TypeTask, model.TypeBug, model.TypeFeature} {
		s, ms := newAutoCloseServer()
		ctx := context.Background()

		seedBead(ms, "gd-parent", pt, model.StatusClosed, nil, nil)
		seedBead(ms, "gd-child", model.TypeTask, model.StatusOpen, nil, nil)
		linkParentChild(ms, "gd-child", "gd-parent")

		s.afterBeadClosed(ctx, ms.beads["gd-parent"])

		if got := ms.beads["gd-child"].Status; got != model.StatusBlocked {
			t.Fatalf("parent type %q: expected orphaned child parked to blocked, got status=%q", pt, got)
		}
	}
}

// TestAfterBeadClosed_OrphanPark_LeavesTerminalChild verifies the status guard:
// a child already closed / deferred / blocked is left untouched (parking only
// pulls actively-surfaceable open/in_progress work out of the lane, and a
// re-close must be idempotent).
func TestAfterBeadClosed_OrphanPark_LeavesTerminalChild(t *testing.T) {
	for _, st := range []model.Status{model.StatusClosed, model.StatusDeferred, model.StatusBlocked} {
		s, ms := newAutoCloseServer()
		ctx := context.Background()

		seedBead(ms, "gd-epic", model.TypeEpic, model.StatusClosed, nil, nil)
		seedBead(ms, "gd-child", model.TypeTask, st, nil, nil)
		linkParentChild(ms, "gd-child", "gd-epic")

		s.afterBeadClosed(ctx, ms.beads["gd-epic"])

		if got := ms.beads["gd-child"].Status; got != st {
			t.Fatalf("expected child in status %q left untouched, got %q", st, got)
		}
	}
}

// TestAfterBeadClosed_OrphanPark_LeavesChildWithLiveOtherParent verifies the
// multi-parent guard: a child with ANOTHER still-open parent-child parent is NOT
// orphaned — the live parent legitimately tracks it, so parking would hide real
// work.
func TestAfterBeadClosed_OrphanPark_LeavesChildWithLiveOtherParent(t *testing.T) {
	s, ms := newAutoCloseServer()
	ctx := context.Background()

	seedBead(ms, "gd-dead-epic", model.TypeEpic, model.StatusClosed, nil, nil)
	seedBead(ms, "gd-live-epic", model.TypeEpic, model.StatusOpen, nil, nil)
	seedBead(ms, "gd-child", model.TypeTask, model.StatusOpen, nil, nil)
	linkParentChild(ms, "gd-child", "gd-dead-epic")
	linkParentChild(ms, "gd-child", "gd-live-epic")

	s.afterBeadClosed(ctx, ms.beads["gd-dead-epic"])

	if got := ms.beads["gd-child"].Status; got != model.StatusOpen {
		t.Fatalf("expected child with a live other parent left open, got status=%q", got)
	}
}

// TestAfterBeadClosed_OrphanPark_ParksWhenOtherParentAlsoTerminal verifies the
// multi-parent guard's complement: when EVERY parent is terminal-for-progression
// (closed/partial), the child is genuinely orphaned and is parked.
func TestAfterBeadClosed_OrphanPark_ParksWhenOtherParentAlsoTerminal(t *testing.T) {
	s, ms := newAutoCloseServer()
	ctx := context.Background()

	seedBead(ms, "gd-dead-a", model.TypeEpic, model.StatusClosed, nil, nil)
	seedBead(ms, "gd-dead-b", model.TypeEpic, model.StatusClosed, nil, nil)
	seedBead(ms, "gd-child", model.TypeTask, model.StatusOpen, nil, nil)
	linkParentChild(ms, "gd-child", "gd-dead-a")
	linkParentChild(ms, "gd-child", "gd-dead-b")

	s.afterBeadClosed(ctx, ms.beads["gd-dead-a"])

	if got := ms.beads["gd-child"].Status; got != model.StatusBlocked {
		t.Fatalf("expected child with all-terminal parents parked to blocked, got status=%q", got)
	}
}

// TestAfterBeadClosed_OrphanPark_ParksMultipleChildren verifies every distinct
// open child of the closed parent is parked (the dominant measured shape: an
// epic declared done with several stragglers still open).
func TestAfterBeadClosed_OrphanPark_ParksMultipleChildren(t *testing.T) {
	s, ms := newAutoCloseServer()
	ctx := context.Background()

	seedBead(ms, "gd-epic", model.TypeEpic, model.StatusClosed, nil, nil)
	seedBead(ms, "gd-child-a", model.TypeTask, model.StatusOpen, nil, nil)
	seedBead(ms, "gd-child-b", model.TypeBug, model.StatusOpen, nil, nil)
	linkParentChild(ms, "gd-child-a", "gd-epic")
	linkParentChild(ms, "gd-child-b", "gd-epic")

	s.afterBeadClosed(ctx, ms.beads["gd-epic"])

	for _, id := range []string{"gd-child-a", "gd-child-b"} {
		if got := ms.beads[id].Status; got != model.StatusBlocked {
			t.Fatalf("expected %s parked to blocked, got status=%q", id, got)
		}
	}
}

// TestAfterBeadClosed_OrphanPark_IgnoresBlocksEdge verifies only parent-child
// edges park: a bead merely blocked-by the closed bead (a `blocks` dependent)
// is NOT its child and must not be parked by this walk.
func TestAfterBeadClosed_OrphanPark_IgnoresBlocksEdge(t *testing.T) {
	s, ms := newAutoCloseServer()
	ctx := context.Background()

	seedBead(ms, "gd-blocker", model.TypeTask, model.StatusClosed, nil, nil)
	seedBead(ms, "gd-dependent", model.TypeTask, model.StatusOpen, nil, nil)
	linkBlocks(ms, "gd-dependent", "gd-blocker") // dependent blocks→ blocker

	s.afterBeadClosed(ctx, ms.beads["gd-blocker"])

	if got := ms.beads["gd-dependent"].Status; got != model.StatusOpen {
		t.Fatalf("expected blocks-dependent left open (not a child), got status=%q", got)
	}
}

// TestAfterBeadClosed_OrphanPark_LeavesEscalationAndDecisionChildren is the
// acceptance test for ka-0000002225: closing a parent must NOT park its open
// escalation/decision children (they are un-answered operator questions, never
// `gb ready` work — parking silently drops them from `gb escalation list` /
// `gb decision list`), while a kind=issue child under the SAME closed parent
// still parks (the ka-0000000504 behavior must not regress).
func TestAfterBeadClosed_OrphanPark_LeavesEscalationAndDecisionChildren(t *testing.T) {
	s, ms := newAutoCloseServer()
	ctx := context.Background()

	// The signature from the bug: a scheduled gardener's ephemeral per-cycle
	// bead, closed at `gb stop`, is the sole parent of the escalation/decision
	// it just filed with `--requesting-task <cycle-bead>`.
	seedBead(ms, "gd-cycle", model.TypeTask, model.StatusClosed, nil, nil)
	seedBead(ms, "gd-h73itmoip3", model.TypeEscalation, model.StatusOpen, nil, nil)
	seedBead(ms, "gd-decision", model.TypeDecision, model.StatusOpen, nil, nil)
	seedBead(ms, "gd-issue", model.TypeTask, model.StatusOpen, nil, nil)
	linkParentChild(ms, "gd-h73itmoip3", "gd-cycle")
	linkParentChild(ms, "gd-decision", "gd-cycle")
	linkParentChild(ms, "gd-issue", "gd-cycle")

	s.afterBeadClosed(ctx, ms.beads["gd-cycle"])

	if got := ms.beads["gd-h73itmoip3"].Status; got != model.StatusOpen {
		t.Fatalf("expected escalation child left open (visible to operator), got status=%q", got)
	}
	if got := ms.beads["gd-decision"].Status; got != model.StatusOpen {
		t.Fatalf("expected decision child left open (visible to operator), got status=%q", got)
	}
	// Regression guard for ka-0000000504: a plain issue child still parks.
	if got := ms.beads["gd-issue"].Status; got != model.StatusBlocked {
		t.Fatalf("expected kind=issue child still parked to blocked, got status=%q", got)
	}
}

// TestAfterBeadClosed_OrphanPark_LeavesNonWorkChildren is the acceptance test
// for ka-0000000182: the escalation/decision carve-out was too narrow. Parking
// exists solely to pull stale claimable WORK out of `gb ready`; every
// non-issue-kind child (advice/doc/report) and every deliverable container
// (bundle/rich-report), though sometimes issue-KIND, is NOT `gb ready` work, so
// parking it to `blocked` is pure durable loss — advice is delivered by open
// status, docs/reports/bundles are receipts. None of these may be parked, while
// a plain issue-kind task under the SAME closed parent still parks.
func TestAfterBeadClosed_OrphanPark_LeavesNonWorkChildren(t *testing.T) {
	nonWork := []struct {
		id  string
		typ model.BeadType
	}{
		{"gd-advice", model.TypeAdvice},         // delivered by open status
		{"gd-doc", "doc"},                       // receipt rendered on /plans
		{"gd-report", model.TypeReport},         // deprecated deliverable
		{"gd-bundle", model.TypeBundle},         // issue-KIND deliverable container
		{"gd-auylgu466w", model.TypeRichReport}, // issue-KIND deliverable container
		{"gd-dashboard", "dashboard"},           // deliverable container
	}

	s, ms := newAutoCloseServer()
	ctx := context.Background()

	seedBead(ms, "gd-parent", model.TypeEpic, model.StatusClosed, nil, nil)
	for _, nw := range nonWork {
		seedBead(ms, nw.id, nw.typ, model.StatusOpen, nil, nil)
		linkParentChild(ms, nw.id, "gd-parent")
	}
	// Regression control: a plain issue-kind task under the same closed parent
	// still parks (ka-0000000504 behavior must not regress).
	seedBead(ms, "gd-task", model.TypeTask, model.StatusOpen, nil, nil)
	linkParentChild(ms, "gd-task", "gd-parent")

	s.afterBeadClosed(ctx, ms.beads["gd-parent"])

	for _, nw := range nonWork {
		if got := ms.beads[nw.id].Status; got != model.StatusOpen {
			t.Fatalf("type %q (%s): expected non-work child left open, got status=%q", nw.typ, nw.id, got)
		}
	}
	if got := ms.beads["gd-task"].Status; got != model.StatusBlocked {
		t.Fatalf("expected issue-kind task child still parked to blocked, got status=%q", got)
	}
}

// TestFirstUnmergedMR unit-tests the merged-work gate helper across the field
// shapes the gitlab-bridge writes.
// TestFirstUnmergedMR exercises the LEGACY-fallback path: with no canonical
// type=merge-request bead in the store, firstUnmergedMR reads the merge signal
// off the issue bead's own fields (the pre-migration behavior it still honors).
func TestFirstUnmergedMR(t *testing.T) {
	mk := func(id, url, merged, state string) *model.Bead {
		f := map[string]any{}
		if url != "" {
			f["mr_url"] = url
		}
		if merged != "" {
			f["mr_merged"] = merged
		}
		if state != "" {
			f["mr_state"] = state
		}
		raw, _ := json.Marshal(f)
		return &model.Bead{ID: id, Fields: raw}
	}

	cases := []struct {
		name string
		kids []*model.Bead
		want string
	}{
		{"no MRs at all", []*model.Bead{mk("a", "", "", ""), mk("b", "", "", "")}, ""},
		{"all merged via mr_merged", []*model.Bead{mk("a", "u", "true", ""), mk("b", "u", "true", "")}, ""},
		{"merged via mr_state", []*model.Bead{mk("a", "u", "", "merged")}, ""},
		{"one unmerged (opened)", []*model.Bead{mk("a", "u", "true", ""), mk("b", "u", "false", "opened")}, "b"},
		{"unmerged draft", []*model.Bead{mk("a", "u", "", "draft")}, "a"},
		{"nil entries skipped", []*model.Bead{nil, mk("a", "u", "true", "")}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := newAutoCloseServer() // empty store → legacy issue-bead fallback
			if got := s.firstUnmergedMR(context.Background(), tc.kids); got != tc.want {
				t.Fatalf("firstUnmergedMR = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestFirstUnmergedMR_ResolvesViaCanonicalMRBead is the regression test for
// ka-0000002273. It mirrors the gitlab-bridge ban-list: a closed child whose
// issue bead carries mr_url but NO mr_state/mr_merged (the keys the bridge
// stopped stamping), paired with a canonical type=merge-request bead that
// records the merge, must be treated as MERGED — firstUnmergedMR must resolve
// through the MR bead, not the (empty) issue-bead fields.
func TestFirstUnmergedMR_ResolvesViaCanonicalMRBead(t *testing.T) {
	// mrBead seeds a canonical type=merge-request bead keyed by url, with the
	// given merge-marker fields (mr_merged/mr_outcome/mr_state).
	mrBead := func(ms *mockStore, id, url string, fields map[string]any) {
		fields["mr_url"] = url
		seedBead(ms, id, model.BeadType("merge-request"), model.StatusClosed, nil, fields)
	}

	t.Run("merged via mr_merged on MR bead only", func(t *testing.T) {
		s, ms := newAutoCloseServer()
		// Child: mr_url present, but NO merge marker on the issue bead (the ban).
		seedBead(ms, "gd-child", model.TypeTask, model.StatusClosed, nil, map[string]any{
			"mr_url": "https://gitlab/x/-/merge_requests/1",
		})
		mrBead(ms, "gd-mr", "https://gitlab/x/-/merge_requests/1", map[string]any{"mr_merged": "true"})

		if got := s.firstUnmergedMR(context.Background(), []*model.Bead{ms.beads["gd-child"]}); got != "" {
			t.Fatalf("expected merged (via canonical MR bead), got unmerged child %q", got)
		}
	})

	t.Run("merged via mr_outcome with mr_state=opened (ka-0000005427)", func(t *testing.T) {
		s, ms := newAutoCloseServer()
		seedBead(ms, "gd-child", model.TypeTask, model.StatusClosed, nil, map[string]any{
			"mr_url": "https://gitlab/x/-/merge_requests/2",
		})
		// !5404 shape: mr_state=opened but mr_outcome=merged / mr_merged=true.
		mrBead(ms, "gd-mr", "https://gitlab/x/-/merge_requests/2", map[string]any{
			"mr_state": "opened", "mr_outcome": "merged", "mr_merged": "true",
		})

		if got := s.firstUnmergedMR(context.Background(), []*model.Bead{ms.beads["gd-child"]}); got != "" {
			t.Fatalf("expected merged via mr_outcome, got unmerged child %q", got)
		}
	})

	t.Run("genuinely unmerged MR bead still gates", func(t *testing.T) {
		s, ms := newAutoCloseServer()
		seedBead(ms, "gd-child", model.TypeTask, model.StatusClosed, nil, map[string]any{
			"mr_url": "https://gitlab/x/-/merge_requests/3",
		})
		mrBead(ms, "gd-mr", "https://gitlab/x/-/merge_requests/3", map[string]any{
			"mr_state": "opened", "mr_merged": "false",
		})

		if got := s.firstUnmergedMR(context.Background(), []*model.Bead{ms.beads["gd-child"]}); got != "gd-child" {
			t.Fatalf("expected gd-child to gate as unmerged, got %q", got)
		}
	})
}

// TestAfterBeadClosed_DrainsWhenMRMergedOnCanonicalBead is the end-to-end
// regression for ka-0000002273: a plain epic whose last child closes with
// mr_url set but no issue-bead merge marker must still auto-close, because the
// canonical MR bead records the merge. Before the fix the epic deferred forever.
func TestAfterBeadClosed_DrainsWhenMRMergedOnCanonicalBead(t *testing.T) {
	s, ms := newAutoCloseServer()
	ctx := context.Background()

	seedBead(ms, "gd-epic", model.TypeEpic, model.StatusOpen, nil, nil)
	// Closed child: mr_url set, but the bridge-banned mr_state/mr_merged are
	// ABSENT on the issue bead (exactly the prod data in the bug report).
	seedBead(ms, "gd-child", model.TypeTask, model.StatusClosed, nil, map[string]any{
		"mr_url": "https://gitlab/x/-/merge_requests/42",
	})
	linkParentChild(ms, "gd-child", "gd-epic")
	// Canonical MR bead carries the merge marker — the sole source of truth.
	seedBead(ms, "gd-mr", model.BeadType("merge-request"), model.StatusClosed, nil, map[string]any{
		"mr_url": "https://gitlab/x/-/merge_requests/42", "mr_merged": "true", "mr_outcome": "merged",
	})

	s.afterBeadClosed(ctx, ms.beads["gd-child"])

	if got := ms.beads["gd-epic"].Status; got != model.StatusClosed {
		t.Fatalf("expected epic auto-closed via canonical MR bead merge state, got status=%q", got)
	}
}

// TestAfterBeadClosed_NilAndNonIssueSafe verifies the choke point tolerates a
// nil bead and that a non-issue-kind close whose only sibling work is also
// non-issue-kind does NOT spuriously close its parent epic. Post-ka-0000001346
// the walk runs for every kind, so the epic here stays open because it holds no
// CLOSED issue-kind descendant (the childless/empty-container guard), not
// because the walk was skipped.
func TestAfterBeadClosed_NilAndNonIssueSafe(t *testing.T) {
	s, ms := newAutoCloseServer()
	ctx := context.Background()

	// nil bead must not panic.
	s.afterBeadClosed(ctx, nil)

	// A closed config-kind bead with an epic parent edge walks the parent, but
	// the epic has zero closed issue-kind descendants, so the empty-container
	// guard leaves it open.
	seedBead(ms, "gd-epic", model.TypeEpic, model.StatusOpen, nil, nil)
	agent := &model.Bead{ID: "gd-agent", Kind: model.KindConfig, Type: "agent", Status: model.StatusClosed}
	ms.beads["gd-agent"] = agent
	linkParentChild(ms, "gd-agent", "gd-epic")

	s.afterBeadClosed(ctx, agent)

	if got := ms.beads["gd-epic"].Status; got != model.StatusOpen {
		t.Fatalf("expected epic untouched by a config-kind close with no closed issue-kind work, got status=%q", got)
	}
}

// TestAfterBeadClosed_NonIssueTriggerLastChild_DrainsEpic is the regression test
// for ka-0000001346: an epic whose remaining open child is a non-issue-kind
// `type=trigger, kind=data` gardener bead (the standard gardener-under-epic
// pattern) MUST auto-close when that trigger is closed. Before the fix,
// afterBeadClosed gated the parent walk on Kind == KindIssue, so closing the
// trigger never re-checked the epic and it stayed open forever.
func TestAfterBeadClosed_NonIssueTriggerLastChild_DrainsEpic(t *testing.T) {
	s, ms := newAutoCloseServer()
	ctx := context.Background()

	seedBead(ms, "gd-epic", model.TypeEpic, model.StatusOpen, nil, nil)
	// An already-closed issue-kind child proves the epic held real work (with
	// its MR merged so the merged-work gate is satisfied).
	seedBead(ms, "gd-issue-child", model.TypeTask, model.StatusClosed, nil, map[string]any{
		"mr_url": "https://gitlab/x/-/merge_requests/7", "mr_merged": "true",
	})
	// The last remaining open child is a non-issue-kind trigger (kind=data).
	trigger := &model.Bead{ID: "gd-trigger", Kind: model.KindData, Type: "trigger", Status: model.StatusClosed}
	ms.beads["gd-trigger"] = trigger
	linkParentChild(ms, "gd-issue-child", "gd-epic")
	linkParentChild(ms, "gd-trigger", "gd-epic")

	// Simulate the close of the trigger (the last child to close).
	s.afterBeadClosed(ctx, trigger)

	if got := ms.beads["gd-epic"].Status; got != model.StatusClosed {
		t.Fatalf("expected epic auto-closed when its last non-issue-kind child closed, got status=%q", got)
	}
	if ms.beads["gd-epic"].ClosedBy != autoCloseActor {
		t.Fatalf("expected closed_by=%q, got %q", autoCloseActor, ms.beads["gd-epic"].ClosedBy)
	}
	if len(ms.comments["gd-epic"]) == 0 {
		t.Fatalf("expected an audit comment on the auto-closed epic")
	}
}

// TestAfterBeadClosed_NonIssueTriggerLastChild_FlagsPlanEpic is the plan-epic
// half of the ka-0000001346 regression: a drained PLAN epic (carries
// fields.plan_state) whose last child is a non-issue-kind trigger must be
// FLAGGED autoclose-ready (operator Mark-complete gate preserved), not closed.
func TestAfterBeadClosed_NonIssueTriggerLastChild_FlagsPlanEpic(t *testing.T) {
	s, ms := newAutoCloseServer()
	ctx := context.Background()

	seedBead(ms, "gd-plan", model.TypeEpic, model.StatusOpen, nil, map[string]any{"plan_state": "building"})
	seedBead(ms, "gd-issue-child", model.TypeTask, model.StatusClosed, nil, map[string]any{
		"mr_url": "https://gitlab/x/-/merge_requests/8", "mr_merged": "true",
	})
	trigger := &model.Bead{ID: "gd-trigger", Kind: model.KindData, Type: "trigger", Status: model.StatusClosed}
	ms.beads["gd-trigger"] = trigger
	linkParentChild(ms, "gd-issue-child", "gd-plan")
	linkParentChild(ms, "gd-trigger", "gd-plan")

	s.afterBeadClosed(ctx, trigger)

	if got := ms.beads["gd-plan"].Status; got != model.StatusOpen {
		t.Fatalf("expected plan epic to STAY open (operator gate), got status=%q", got)
	}
	if !hasLabel(ms.labels["gd-plan"], autoCloseReadyLabel) {
		t.Fatalf("expected plan epic flagged %q, labels=%v", autoCloseReadyLabel, ms.labels["gd-plan"])
	}
}

// TestAfterBeadClosed_LiveGardenerChild_DoesNotCloseEpic is the acceptance test
// for bug ka-0000001525: an epic whose last issue-kind descendant just closed
// but which still carries an OPEN + enabled trigger gardener (terminal goal
// UNMET) MUST NOT be hard-closed. The trigger is a data-kind child invisible to
// Gate A's type allowlist, so without the live-gardener guard the continual
// audit lane's epic was closed the instant its last step drained (observed
// twice in one day on the Users-CAMP lane), each time burning a gardener cycle
// to reopen. The trigger here is a GRANDCHILD (epic -> closed task -> trigger),
// matching the real lane where the reviver attached the gardener under a step.
func TestAfterBeadClosed_LiveGardenerChild_DoesNotCloseEpic(t *testing.T) {
	s, ms := newAutoCloseServer()
	ctx := context.Background()

	seedBead(ms, "gd-epic", model.TypeEpic, model.StatusOpen, nil, nil)
	// A closed issue-kind step proves the epic held (and drained) real work, and
	// is the parent the gardener trigger hangs under (grandchild of the epic).
	seedBead(ms, "gd-step", model.TypeTask, model.StatusClosed, nil, nil)
	// The live gardener: open + enabled trigger whose terminal goal is unmet (an
	// unmet goal is exactly why the trigger is still open — a met goal closes it).
	trigger := &model.Bead{ID: "gd-gardener", Kind: model.KindData, Type: "trigger", Status: model.StatusOpen}
	raw, _ := json.Marshal(map[string]any{"enabled": true, "terminal_goal": "all dims >= 8, two consecutive clean runs"})
	trigger.Fields = raw
	ms.beads["gd-gardener"] = trigger
	linkParentChild(ms, "gd-step", "gd-epic")
	linkParentChild(ms, "gd-gardener", "gd-step")

	// Simulate the close of the last issue-kind step.
	s.afterBeadClosed(ctx, ms.beads["gd-step"])

	if got := ms.beads["gd-epic"].Status; got != model.StatusOpen {
		t.Fatalf("expected epic to STAY open while a live gardener child remains, got status=%q", got)
	}
}

// TestAfterBeadClosed_DisabledGardenerChild_ClosesEpic is the negative control
// for the live-gardener guard: a trigger child whose enabled field is the literal
// "false" is inert (the trigger engine will never fire it), so it must NOT hold
// the epic open — mirroring the engine's own `enabled != "false"` liveness read.
func TestAfterBeadClosed_DisabledGardenerChild_ClosesEpic(t *testing.T) {
	s, ms := newAutoCloseServer()
	ctx := context.Background()

	seedBead(ms, "gd-epic", model.TypeEpic, model.StatusOpen, nil, nil)
	seedBead(ms, "gd-step", model.TypeTask, model.StatusClosed, nil, nil)
	trigger := &model.Bead{ID: "gd-gardener", Kind: model.KindData, Type: "trigger", Status: model.StatusOpen}
	raw, _ := json.Marshal(map[string]any{"enabled": false})
	trigger.Fields = raw
	ms.beads["gd-gardener"] = trigger
	linkParentChild(ms, "gd-step", "gd-epic")
	linkParentChild(ms, "gd-gardener", "gd-epic")

	s.afterBeadClosed(ctx, ms.beads["gd-step"])

	if got := ms.beads["gd-epic"].Status; got != model.StatusClosed {
		t.Fatalf("expected epic auto-closed (disabled trigger is inert, not a live gardener), got status=%q", got)
	}
}

// TestAfterBeadClosed_LiveScheduleChild_DoesNotCloseEpic proves the guard covers
// type=schedule gardeners too (the schedule-mirror source carries the same
// gardener field set), not just type=trigger.
func TestAfterBeadClosed_LiveScheduleChild_DoesNotCloseEpic(t *testing.T) {
	s, ms := newAutoCloseServer()
	ctx := context.Background()

	seedBead(ms, "gd-epic", model.TypeEpic, model.StatusOpen, nil, nil)
	seedBead(ms, "gd-step", model.TypeTask, model.StatusClosed, nil, nil)
	sched := &model.Bead{ID: "gd-sched", Kind: model.KindConfig, Type: "schedule", Status: model.StatusOpen}
	raw, _ := json.Marshal(map[string]any{"enabled": true})
	sched.Fields = raw
	ms.beads["gd-sched"] = sched
	linkParentChild(ms, "gd-step", "gd-epic")
	linkParentChild(ms, "gd-sched", "gd-epic")

	s.afterBeadClosed(ctx, ms.beads["gd-step"])

	if got := ms.beads["gd-epic"].Status; got != model.StatusOpen {
		t.Fatalf("expected epic to STAY open while a live schedule gardener remains, got status=%q", got)
	}
}

// TestAfterBeadClosed_LiveGardenerBeyondCap_DoesNotCloseEpic is the regression
// test for bug ka-0000005183. firstLiveGardenerChild (Gate B) used to enumerate
// the epic's OPEN trigger/schedule descendants with a single-shot `Limit: 500`
// fetch and NO sort — the same silent-truncation class as Gate C
// (ka-0000008187). With no Sort, postgres applies its default `created_at DESC`,
// so the cap keeps the 500 NEWEST open gardeners and SILENTLY DROPS the oldest.
// If the ONLY enabled (live) gardener sits among those dropped oldest,
// firstLiveGardenerChild returned "" — so the live-gardener guard read "no live
// gardener", and the epic hard-closed with a gardener still mid-loop (the exact
// failure mode of parent bug ka-0000001525).
//
// It seeds 600 OPEN gardener triggers, all DISABLED except one among the OLDEST
// (index 5 — squarely in the region a `Limit: 500` + implicit-DESC fetch
// truncates) and asserts the epic STAYS OPEN.
//
// This test only bites because the mock store honors Limit/Offset/Sort (as of
// ka-0000008187): a mock that returned every match regardless of Limit could
// not exhibit the truncation. It FAILS against the pre-fix single-shot
// `Limit: 500` Gate B (the sole live gardener is dropped, so the epic closes)
// and PASSES against the paged firstLiveGardenerChild walk, which reads every
// gardener regardless of age.
func TestAfterBeadClosed_LiveGardenerBeyondCap_DoesNotCloseEpic(t *testing.T) {
	s, ms := newAutoCloseServer()
	ctx := context.Background()

	seedBead(ms, "gd-bigepic", model.TypeEpic, model.StatusOpen, nil, nil)
	// A closed issue-kind step proves the epic held (and drained) real work so
	// the childless guard (Gate C) does not short-circuit the drain before the
	// gardener guard's verdict can hard-close the epic. No MR → nothing to gate.
	seedBead(ms, "gd-step", model.TypeTask, model.StatusClosed, nil, nil)
	linkParentChild(ms, "gd-step", "gd-bigepic")

	// 600 OPEN gardener triggers > the old 500 cap. created_at increases with the
	// index, so index 0 is the OLDEST. A pre-fix `Limit: 500` Gate B fetch
	// (implicit created_at DESC) keeps indices 100..599 (the newest 500) and
	// drops indices 0..99 — so the ONLY live gardener at index 5 is squarely in
	// the dropped region, invisible to the single capped fetch.
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	const total = 600
	const liveIdx = 5
	for i := 0; i < total; i++ {
		id := fmt.Sprintf("gd-gardener-%04d", i)
		trig := &model.Bead{ID: id, Kind: model.KindData, Type: "trigger", Status: model.StatusOpen}
		// Every gardener disabled EXCEPT the oldest live one — so a truncated
		// (newest-500) window sees only inert triggers and reads "no live gardener".
		raw, _ := json.Marshal(map[string]any{"enabled": i == liveIdx})
		trig.Fields = raw
		trig.CreatedAt = base.Add(time.Duration(i) * time.Second)
		ms.beads[id] = trig
		linkParentChild(ms, id, "gd-bigepic")
	}

	// Drive the drain as if the last issue-kind step just closed.
	s.afterBeadClosed(ctx, ms.beads["gd-step"])

	if got := ms.beads["gd-bigepic"].Status; got != model.StatusOpen {
		t.Fatalf("expected epic to STAY open — the only live gardener (index %d, among the oldest, beyond the 500 cap) must defer the drain, got status=%q",
			liveIdx, got)
	}
}

// TestAfterBeadClosed_PagesGardenerChildren asserts on the REQUEST SHAPE of the
// live-gardener enumeration (advice ka-0000003555: assert on the requests, not
// the result). With >500 OPEN gardener descendants and every one disabled (so no
// live gardener short-circuits the walk), Gate B must fetch AT LEAST two pages
// with an ADVANCING Offset — proof the enumeration is paged, not a single
// capped `Limit: 500` window. A single-page fetch (the pre-fix shape) would
// leave only one gardener request and fail this test.
func TestAfterBeadClosed_PagesGardenerChildren(t *testing.T) {
	s, ms := newAutoCloseServer()
	ctx := context.Background()

	seedBead(ms, "gd-bigepic", model.TypeEpic, model.StatusOpen, nil, nil)
	seedBead(ms, "gd-step", model.TypeTask, model.StatusClosed, nil, nil)
	linkParentChild(ms, "gd-step", "gd-bigepic")

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	const total = 600
	for i := 0; i < total; i++ {
		id := fmt.Sprintf("gd-gardener-%04d", i)
		trig := &model.Bead{ID: id, Kind: model.KindData, Type: "trigger", Status: model.StatusOpen}
		raw, _ := json.Marshal(map[string]any{"enabled": false}) // all inert
		trig.Fields = raw
		trig.CreatedAt = base.Add(time.Duration(i) * time.Second)
		ms.beads[id] = trig
		linkParentChild(ms, id, "gd-bigepic")
	}

	s.afterBeadClosed(ctx, ms.beads["gd-step"])

	// Every gardener is disabled (inert), so the guard finds no live gardener and
	// the epic drains fully (one closed step, no unmerged MR, plain epic).
	if got := ms.beads["gd-bigepic"].Status; got != model.StatusClosed {
		t.Fatalf("expected epic auto-closed once all 600 gardener triggers are disabled/inert, got status=%q", got)
	}

	// Collect the gardener enumeration requests and assert they paged: >= 2
	// requests over the trigger/schedule type filter, each advancing Offset by a
	// bounded page window (never a > page-size Limit). Gate A (open issue-kind)
	// and Gate C (closed issue-kind) use openSubtreeChildTypes, which carries no
	// trigger type — so filtering on the trigger type uniquely isolates Gate B.
	var gardenerPageOffsets []int
	for _, f := range ms.listFilters {
		if f.ParentSubtreeID != "gd-bigepic" {
			continue
		}
		if !typesContain(f.Type, model.BeadType("trigger")) {
			continue // skip the Gate-A / Gate-C issue-kind enumerations
		}
		if f.Limit > closedKidsPageSize {
			t.Fatalf("gardener page requested Limit %d > page size %d — that is the silent-truncation shape this fix removes (ka-0000005183)",
				f.Limit, closedKidsPageSize)
		}
		gardenerPageOffsets = append(gardenerPageOffsets, f.Offset)
	}
	if len(gardenerPageOffsets) < 2 {
		t.Fatalf("expected the gardener enumeration to page (>= 2 requests) over 600 open gardeners, got %d request(s): %v",
			len(gardenerPageOffsets), gardenerPageOffsets)
	}
	// Offsets must advance (0, 500, ...) — a repeated 0 would be a spin, a
	// single 0 would be the un-paged single fetch.
	if gardenerPageOffsets[0] != 0 || gardenerPageOffsets[1] != closedKidsPageSize {
		t.Fatalf("expected advancing page offsets [0, %d, ...], got %v", closedKidsPageSize, gardenerPageOffsets)
	}
}

// typesContain reports whether ts includes want.
func typesContain(ts []model.BeadType, want model.BeadType) bool {
	for _, ty := range ts {
		if ty == want {
			return true
		}
	}
	return false
}

// TestAfterBeadClosed_NoAutoCloseLabel_DoesNotCloseEpic is the acceptance test
// for the explicit opt-out half of bug ka-0000001525: an epic carrying the
// no-auto-close label is left untouched even when fully drained with no live
// gardener — a one-`kd update` escape hatch for a continual lane, mirroring how a
// plan epic's plan_state suppresses the hard close.
func TestAfterBeadClosed_NoAutoCloseLabel_DoesNotCloseEpic(t *testing.T) {
	s, ms := newAutoCloseServer()
	ctx := context.Background()

	seedBead(ms, "gd-epic", model.TypeEpic, model.StatusOpen, []string{noAutoCloseLabel}, nil)
	seedBead(ms, "gd-child1", model.TypeTask, model.StatusClosed, nil, map[string]any{
		"mr_url": "https://gitlab/x/-/merge_requests/9", "mr_merged": "true",
	})
	seedBead(ms, "gd-child2", model.TypeTask, model.StatusClosed, nil, nil)
	linkParentChild(ms, "gd-child1", "gd-epic")
	linkParentChild(ms, "gd-child2", "gd-epic")

	s.afterBeadClosed(ctx, ms.beads["gd-child2"])

	if got := ms.beads["gd-epic"].Status; got != model.StatusOpen {
		t.Fatalf("expected no-auto-close epic to STAY open, got status=%q", got)
	}
}

// TestGRPCCloseBead_CascadesThroughRealRPC exercises the choke point through the
// real CloseBead RPC (authenticated ctx), proving the wiring in beads.go fires
// end-to-end — not just the direct afterBeadClosed unit. Closing the epic's last
// child auto-closes the plain epic and emits both close events.
func TestGRPCCloseBead_CascadesThroughRealRPC(t *testing.T) {
	srv, ms, ctx := testCtxAs(t, "alice")
	ms.beads["gd-epic"] = &model.Bead{ID: "gd-epic", Title: "epic", Kind: model.KindIssue, Type: model.TypeEpic, Status: model.StatusOpen}
	ms.beads["gd-only-child"] = &model.Bead{ID: "gd-only-child", Title: "child", Kind: model.KindIssue, Type: model.TypeTask, Status: model.StatusOpen}
	linkParentChild(ms, "gd-only-child", "gd-epic")

	resp, err := srv.CloseBead(ctx, &beadsv1.CloseBeadRequest{Id: "gd-only-child"})
	if err != nil {
		t.Fatalf("CloseBead error: %v", err)
	}
	if resp.Bead.Status != "closed" {
		t.Fatalf("child status=%q, want closed", resp.Bead.Status)
	}
	if got := ms.beads["gd-epic"].Status; got != model.StatusClosed {
		t.Fatalf("expected parent epic auto-closed via RPC cascade, got status=%q", got)
	}
	// Two close events: the child (explicit) + the epic (cascade).
	closeEvents := 0
	for _, e := range ms.events {
		if e.Topic == events.TopicBeadClosed {
			closeEvents++
		}
	}
	if closeEvents != 2 {
		t.Fatalf("expected 2 bead.closed events (child + cascaded epic), got %d", closeEvents)
	}
}

// TestGRPCUpdateBead_StatusClosedCascades proves the updateBead status→closed
// path also drives the cascade (kd update <id> --status closed routes here).
func TestGRPCUpdateBead_StatusClosedCascades(t *testing.T) {
	srv, ms, ctx := testCtxAs(t, "alice")
	ms.beads["gd-epic"] = &model.Bead{ID: "gd-epic", Title: "epic", Kind: model.KindIssue, Type: model.TypeEpic, Status: model.StatusOpen}
	ms.beads["gd-kid"] = &model.Bead{ID: "gd-kid", Title: "kid", Kind: model.KindIssue, Type: model.TypeTask, Status: model.StatusInProgress}
	linkParentChild(ms, "gd-kid", "gd-epic")

	closedStatus := string(model.StatusClosed)
	_, err := srv.UpdateBead(ctx, &beadsv1.UpdateBeadRequest{Id: "gd-kid", Status: &closedStatus})
	if err != nil {
		t.Fatalf("UpdateBead error: %v", err)
	}
	if got := ms.beads["gd-epic"].Status; got != model.StatusClosed {
		t.Fatalf("expected parent epic auto-closed via UpdateBead cascade, got status=%q", got)
	}
}

// linkProduces wires an issue --produces--> merge-request edge (leaf bead_id →
// MR depends_on_id), the orientation the gitlab-bridge writes and that
// maybeCloseMergedLeaves walks in reverse from the MR bead. Mirrors how the
// evidence beads (ka-0000002438 etc.) link their merged MR beads.
func linkProduces(ms *mockStore, leaf, mr string) {
	ms.deps[leaf] = append(ms.deps[leaf], &model.Dependency{
		BeadID:      leaf,
		DependsOnID: mr,
		Type:        producesDepType,
	})
}

// seedMergedMR seeds a closed type=merge-request bead marked merged, the shape
// UpsertMergeRequestBead writes on merge. url is the canonical mr_url.
func seedMergedMR(ms *mockStore, id, url string) {
	seedBead(ms, id, mergeRequestType, model.StatusClosed, nil, map[string]any{
		"mr_url": url, "mr_merged": "true", "mr_outcome": "merged", "mr_state": "merged",
	})
}

// TestAfterBeadClosed_Leaf_ClosesOnMergedMR is the acceptance test for the
// leaf close-on-merge path (ka-0000005910), scenario (a): a leaf bug that
// produced ONE merged MR is auto-closed when that MR bead closes. This is the
// exact shape of the evidence bugs (ka-0000002438 / ka-0000001755), which sat
// open forever with a merged MR before this fix.
func TestAfterBeadClosed_Leaf_ClosesOnMergedMR(t *testing.T) {
	s, ms := newAutoCloseServer()
	ctx := context.Background()

	seedBead(ms, "gd-leaf", model.TypeBug, model.StatusOpen, nil, nil)
	seedMergedMR(ms, "gd-mr", "https://gitlab/x/-/merge_requests/6154")
	linkProduces(ms, "gd-leaf", "gd-mr")

	// The MR bead closing (on merge) is the trigger event.
	s.afterBeadClosed(ctx, ms.beads["gd-mr"])

	if got := ms.beads["gd-leaf"].Status; got != model.StatusClosed {
		t.Fatalf("expected leaf auto-closed on merged MR, got status=%q", got)
	}
	if ms.beads["gd-leaf"].ClosedBy != autoCloseActor {
		t.Fatalf("expected closed_by=%q, got %q", autoCloseActor, ms.beads["gd-leaf"].ClosedBy)
	}
	if len(ms.comments["gd-leaf"]) == 0 {
		t.Fatalf("expected an audit comment on the auto-closed leaf")
	}
}

// TestAfterBeadClosed_Leaf_TaskAndFeatureAndChoreEligible confirms the LEAF
// type allowlist: task, feature, and chore leaves close on a merged MR exactly
// like a bug. (Epics are covered by the negative test below.)
func TestAfterBeadClosed_Leaf_TaskAndFeatureAndChoreEligible(t *testing.T) {
	for _, typ := range []model.BeadType{model.TypeTask, model.TypeFeature, model.TypeChore} {
		t.Run(string(typ), func(t *testing.T) {
			s, ms := newAutoCloseServer()
			ctx := context.Background()

			seedBead(ms, "gd-leaf", typ, model.StatusOpen, nil, nil)
			seedMergedMR(ms, "gd-mr", "https://gitlab/x/-/merge_requests/700")
			linkProduces(ms, "gd-leaf", "gd-mr")

			s.afterBeadClosed(ctx, ms.beads["gd-mr"])

			if got := ms.beads["gd-leaf"].Status; got != model.StatusClosed {
				t.Fatalf("expected %s leaf auto-closed, got status=%q", typ, got)
			}
		})
	}
}

// TestAfterBeadClosed_Leaf_TwoMRsOnlyOneMerged is the acceptance test for
// scenario (b): a leaf that produced TWO MRs, only one merged, is left
// UNTOUCHED. The gate is all-MRs-merged, never first-merge-wins. This is the
// ka-0000003811 shape (produced !6154 + !6155) but with the second MR still
// open.
func TestAfterBeadClosed_Leaf_TwoMRsOnlyOneMerged(t *testing.T) {
	s, ms := newAutoCloseServer()
	ctx := context.Background()

	seedBead(ms, "gd-leaf", model.TypeBug, model.StatusOpen, nil, nil)
	seedMergedMR(ms, "gd-mr1", "https://gitlab/x/-/merge_requests/6154")
	// Second MR is still open (not merged).
	seedBead(ms, "gd-mr2", mergeRequestType, model.StatusOpen, nil, map[string]any{
		"mr_url": "https://gitlab/x/-/merge_requests/6155", "mr_state": "opened",
	})
	linkProduces(ms, "gd-leaf", "gd-mr1")
	linkProduces(ms, "gd-leaf", "gd-mr2")

	// The first MR merging fires the event; the leaf must stay open because the
	// second MR is unmerged.
	s.afterBeadClosed(ctx, ms.beads["gd-mr1"])

	if got := ms.beads["gd-leaf"].Status; got != model.StatusOpen {
		t.Fatalf("expected leaf to STAY open (one MR unmerged), got status=%q", got)
	}
}

// TestAfterBeadClosed_Leaf_TwoMRsBothMerged confirms the positive multi-MR
// case: once EVERY linked MR is merged, the leaf closes. Complements the
// scenario (b) negative test above.
func TestAfterBeadClosed_Leaf_TwoMRsBothMerged(t *testing.T) {
	s, ms := newAutoCloseServer()
	ctx := context.Background()

	seedBead(ms, "gd-leaf", model.TypeBug, model.StatusOpen, nil, nil)
	seedMergedMR(ms, "gd-mr1", "https://gitlab/x/-/merge_requests/6154")
	seedMergedMR(ms, "gd-mr2", "https://gitlab/x/-/merge_requests/6155")
	linkProduces(ms, "gd-leaf", "gd-mr1")
	linkProduces(ms, "gd-leaf", "gd-mr2")

	s.afterBeadClosed(ctx, ms.beads["gd-mr2"])

	if got := ms.beads["gd-leaf"].Status; got != model.StatusClosed {
		t.Fatalf("expected leaf closed once both MRs merged, got status=%q", got)
	}
}

// TestAfterBeadClosed_MergedMR_DoesNotCloseEpicProducer is the acceptance test
// for scenario (c) at the trigger boundary: close-on-merge is LEAF-ONLY. An
// EPIC that (unusually) produced a merged MR must NOT be closed by the
// close-on-merge path — an epic drains only via its subtree. The audit behind
// ka-0000005910 found 7 merged-MR epics that are correctly still open.
func TestAfterBeadClosed_MergedMR_DoesNotCloseEpicProducer(t *testing.T) {
	s, ms := newAutoCloseServer()
	ctx := context.Background()

	seedBead(ms, "gd-epic", model.TypeEpic, model.StatusOpen, nil, nil)
	// The epic still has an open child — its subtree is NOT drained.
	seedBead(ms, "gd-child", model.TypeTask, model.StatusOpen, nil, nil)
	linkParentChild(ms, "gd-child", "gd-epic")
	seedMergedMR(ms, "gd-mr", "https://gitlab/x/-/merge_requests/800")
	linkProduces(ms, "gd-epic", "gd-mr")

	s.afterBeadClosed(ctx, ms.beads["gd-mr"])

	if got := ms.beads["gd-epic"].Status; got != model.StatusOpen {
		t.Fatalf("expected epic to stay open (close-on-merge is leaf-only), got status=%q", got)
	}
}

// TestAfterBeadClosed_Epic_DrainUnchanged is the regression test for scenario
// (c): the pre-existing epic subtree-drain behavior is unchanged. Closing the
// last child of a plain epic whose MR work is merged still auto-closes the epic
// — the leaf close-on-merge addition did not perturb this path.
func TestAfterBeadClosed_Epic_DrainUnchanged(t *testing.T) {
	s, ms := newAutoCloseServer()
	ctx := context.Background()

	seedBead(ms, "gd-epic", model.TypeEpic, model.StatusOpen, nil, nil)
	seedBead(ms, "gd-child1", model.TypeTask, model.StatusClosed, nil, map[string]any{
		"mr_url": "https://gitlab/x/-/merge_requests/1", "mr_merged": "true",
	})
	seedBead(ms, "gd-child2", model.TypeTask, model.StatusClosed, nil, nil)
	linkParentChild(ms, "gd-child1", "gd-epic")
	linkParentChild(ms, "gd-child2", "gd-epic")

	s.afterBeadClosed(ctx, ms.beads["gd-child2"])

	if got := ms.beads["gd-epic"].Status; got != model.StatusClosed {
		t.Fatalf("expected epic drain unchanged (auto-closed), got status=%q", got)
	}
}

// TestAfterBeadClosed_Leaf_NoMRLinked verifies a closed MR bead whose producing
// leaf links NO canonical MR bead (dangling / not-yet-linked) leaves the leaf
// untouched — the gate requires >=1 linked MR.
func TestAfterBeadClosed_Leaf_NoMRLinked(t *testing.T) {
	s, ms := newAutoCloseServer()
	ctx := context.Background()

	seedBead(ms, "gd-leaf", model.TypeBug, model.StatusOpen, nil, nil)
	// The MR bead exists and merged, but the reverse edge points at a leaf that
	// has NO forward produces edge to any MR bead (edge was never written back).
	seedMergedMR(ms, "gd-mr", "https://gitlab/x/-/merge_requests/900")
	ms.deps["gd-leaf"] = nil // leaf has no produces edges of its own
	// Reverse dep from MR → leaf still present so the walk reaches the leaf.
	ms.deps["gd-mr"] = []*model.Dependency{{BeadID: "gd-leaf", DependsOnID: "gd-mr", Type: producesDepType}}

	s.afterBeadClosed(ctx, ms.beads["gd-mr"])

	if got := ms.beads["gd-leaf"].Status; got != model.StatusOpen {
		t.Fatalf("expected leaf with no linked MR bead to stay open, got status=%q", got)
	}
}

// TestAfterBeadClosed_Leaf_NoAutoCloseOptOut verifies the explicit opt-out: a
// leaf carrying the no-auto-close label is left open even when its every linked
// MR is merged — mirroring the epic hard-close escape hatch.
func TestAfterBeadClosed_Leaf_NoAutoCloseOptOut(t *testing.T) {
	s, ms := newAutoCloseServer()
	ctx := context.Background()

	seedBead(ms, "gd-leaf", model.TypeBug, model.StatusOpen, []string{noAutoCloseLabel}, nil)
	seedMergedMR(ms, "gd-mr", "https://gitlab/x/-/merge_requests/901")
	linkProduces(ms, "gd-leaf", "gd-mr")

	s.afterBeadClosed(ctx, ms.beads["gd-mr"])

	if got := ms.beads["gd-leaf"].Status; got != model.StatusOpen {
		t.Fatalf("expected no-auto-close leaf to stay open, got status=%q", got)
	}
}

// TestAfterBeadClosed_Leaf_UmbrellaWithOpenChildSkipped verifies the umbrella
// guard: a non-epic bead that (mis)uses itself as a container — has an OPEN
// issue-kind parent-child child — is NOT closed on its own MR merge, since
// closing it would abandon that open child work.
func TestAfterBeadClosed_Leaf_UmbrellaWithOpenChildSkipped(t *testing.T) {
	s, ms := newAutoCloseServer()
	ctx := context.Background()

	seedBead(ms, "gd-umbrella", model.TypeBug, model.StatusOpen, nil, nil)
	seedBead(ms, "gd-openchild", model.TypeTask, model.StatusOpen, nil, nil)
	linkParentChild(ms, "gd-openchild", "gd-umbrella")
	seedMergedMR(ms, "gd-mr", "https://gitlab/x/-/merge_requests/902")
	linkProduces(ms, "gd-umbrella", "gd-mr")

	s.afterBeadClosed(ctx, ms.beads["gd-mr"])

	if got := ms.beads["gd-umbrella"].Status; got != model.StatusOpen {
		t.Fatalf("expected umbrella bead with open child to stay open, got status=%q", got)
	}
}

// TestAfterBeadClosed_UnmergedMRClose_NoLeafClose verifies that a merge-request
// bead that closed WITHOUT merging (mr_outcome=closed) never closes its
// producing leaf — the leaf's fix did not land.
func TestAfterBeadClosed_UnmergedMRClose_NoLeafClose(t *testing.T) {
	s, ms := newAutoCloseServer()
	ctx := context.Background()

	seedBead(ms, "gd-leaf", model.TypeBug, model.StatusOpen, nil, nil)
	seedBead(ms, "gd-mr", mergeRequestType, model.StatusClosed, nil, map[string]any{
		"mr_url": "https://gitlab/x/-/merge_requests/903", "mr_outcome": "closed", "mr_state": "closed",
	})
	linkProduces(ms, "gd-leaf", "gd-mr")

	s.afterBeadClosed(ctx, ms.beads["gd-mr"])

	if got := ms.beads["gd-leaf"].Status; got != model.StatusOpen {
		t.Fatalf("expected leaf to stay open when its MR closed unmerged, got status=%q", got)
	}
}

// TestAfterBeadClosed_Leaf_CascadesToParentEpic verifies the upward cascade: a
// leaf auto-closed on merge that was the LAST open child of a plain epic drains
// that epic in turn. Proves autoCloseMergedLeaf re-enters afterBeadClosed.
func TestAfterBeadClosed_Leaf_CascadesToParentEpic(t *testing.T) {
	s, ms := newAutoCloseServer()
	ctx := context.Background()

	seedBead(ms, "gd-epic", model.TypeEpic, model.StatusOpen, nil, nil)
	seedBead(ms, "gd-leaf", model.TypeBug, model.StatusOpen, nil, nil)
	linkParentChild(ms, "gd-leaf", "gd-epic")
	seedMergedMR(ms, "gd-mr", "https://gitlab/x/-/merge_requests/904")
	linkProduces(ms, "gd-leaf", "gd-mr")

	s.afterBeadClosed(ctx, ms.beads["gd-mr"])

	if got := ms.beads["gd-leaf"].Status; got != model.StatusClosed {
		t.Fatalf("expected leaf auto-closed, got status=%q", got)
	}
	if got := ms.beads["gd-epic"].Status; got != model.StatusClosed {
		t.Fatalf("expected parent epic drained after leaf close, got status=%q", got)
	}
}

// --- Step 5: reverse-`blocks` bug close-on-fix-merge (ka-0000003106) ---
//
// The CED remediation lane files a SCOPED FIX-TASK that `blocks` a bug and links
// the MR to the fix-task; when the fix merges the fix-task closes, but the bug
// stayed open because the cascade never traversed the bug's `blocks`→fix-task
// edge. These tests cover the opt-in reverse-blocks close and its safety gates.

// seedMergedFixTask seeds a CLOSED fix-task that produced a MERGED MR — the exact
// shape the CED lane leaves behind (fix-task ka-0000002081 / MR !6950).
func seedMergedFixTask(ms *mockStore, taskID, mrID, url string) {
	seedBead(ms, taskID, model.TypeTask, model.StatusClosed, nil, nil)
	seedMergedMR(ms, mrID, url)
	linkProduces(ms, taskID, mrID)
}

// TestAfterBeadClosed_BlockedBug_ClosesOnFixMerge is the acceptance test for
// criterion #1: a bug whose only open work was a blocking fix-task that closed
// MERGED is auto-closed once the fix-task closes.
func TestAfterBeadClosed_BlockedBug_ClosesOnFixMerge(t *testing.T) {
	s, ms := newAutoCloseServer()
	ctx := context.Background()

	seedBead(ms, "gd-bug", model.TypeBug, model.StatusOpen, []string{closeOnBlockingFixMergeLabel}, nil)
	seedMergedFixTask(ms, "gd-fix", "gd-mr", "https://gitlab/x/-/merge_requests/6950")
	linkBlocks(ms, "gd-bug", "gd-fix") // bug blocks→ fix-task (bug is blocked-by fix)

	// The fix-task closing is the trigger event.
	s.afterBeadClosed(ctx, ms.beads["gd-fix"])

	if got := ms.beads["gd-bug"].Status; got != model.StatusClosed {
		t.Fatalf("expected bug auto-closed on blocking fix merge, got status=%q", got)
	}
	if ms.beads["gd-bug"].ClosedBy != autoCloseActor {
		t.Fatalf("expected closed_by=%q, got %q", autoCloseActor, ms.beads["gd-bug"].ClosedBy)
	}
	if len(ms.comments["gd-bug"]) == 0 {
		t.Fatalf("expected an audit comment on the auto-closed bug")
	}
}

// TestAfterBeadClosed_BlockedBug_LegacyMRFieldOnFixTask covers the legacy
// fallback: a fix-task carrying the merged marker on its OWN fields (no canonical
// produces→MR bead) still counts as a merged fix.
func TestAfterBeadClosed_BlockedBug_LegacyMRFieldOnFixTask(t *testing.T) {
	s, ms := newAutoCloseServer()
	ctx := context.Background()

	seedBead(ms, "gd-bug", model.TypeBug, model.StatusOpen, []string{closeOnBlockingFixMergeLabel}, nil)
	seedBead(ms, "gd-fix", model.TypeTask, model.StatusClosed, nil, map[string]any{
		"mr_url": "https://gitlab/x/-/merge_requests/1", "mr_merged": "true",
	})
	linkBlocks(ms, "gd-bug", "gd-fix")

	s.afterBeadClosed(ctx, ms.beads["gd-fix"])

	if got := ms.beads["gd-bug"].Status; got != model.StatusClosed {
		t.Fatalf("expected bug auto-closed on legacy merged fix-task, got status=%q", got)
	}
}

// TestAfterBeadClosed_BlockedBug_NoOptInLabel_StaysOpen verifies the opt-in gate:
// a bug blocked-by a merged fix-task but WITHOUT the contract label is left open —
// a plain blocked-by is overloaded (fixes vs must-precede), never a close signal.
func TestAfterBeadClosed_BlockedBug_NoOptInLabel_StaysOpen(t *testing.T) {
	s, ms := newAutoCloseServer()
	ctx := context.Background()

	seedBead(ms, "gd-bug", model.TypeBug, model.StatusOpen, nil, nil) // no opt-in label
	seedMergedFixTask(ms, "gd-fix", "gd-mr", "https://gitlab/x/-/merge_requests/6950")
	linkBlocks(ms, "gd-bug", "gd-fix")

	s.afterBeadClosed(ctx, ms.beads["gd-fix"])

	if got := ms.beads["gd-bug"].Status; got != model.StatusOpen {
		t.Fatalf("expected un-opted-in bug to stay open, got status=%q", got)
	}
}

// TestAfterBeadClosed_BlockedBug_WontfixBlocker_StaysOpen is the acceptance test
// for criterion #2: a bug whose blocking fix-task closed WONTFIX/DUPLICATE (no
// merged MR) is NOT auto-closed — it still needs a real fix.
func TestAfterBeadClosed_BlockedBug_WontfixBlocker_StaysOpen(t *testing.T) {
	s, ms := newAutoCloseServer()
	ctx := context.Background()

	seedBead(ms, "gd-bug", model.TypeBug, model.StatusOpen, []string{closeOnBlockingFixMergeLabel}, nil)
	seedBead(ms, "gd-fix", model.TypeTask, model.StatusClosed, nil, nil) // closed, no MR at all
	linkBlocks(ms, "gd-bug", "gd-fix")

	s.afterBeadClosed(ctx, ms.beads["gd-fix"])

	if got := ms.beads["gd-bug"].Status; got != model.StatusOpen {
		t.Fatalf("expected bug with wontfix blocker (no merged MR) to stay open, got status=%q", got)
	}
}

// TestAfterBeadClosed_BlockedBug_UnmergedMRBlocker_StaysOpen covers a blocker
// that closed with an UNMERGED (still-open) MR: not a merged fix, so the bug is
// left open.
func TestAfterBeadClosed_BlockedBug_UnmergedMRBlocker_StaysOpen(t *testing.T) {
	s, ms := newAutoCloseServer()
	ctx := context.Background()

	seedBead(ms, "gd-bug", model.TypeBug, model.StatusOpen, []string{closeOnBlockingFixMergeLabel}, nil)
	seedBead(ms, "gd-fix", model.TypeTask, model.StatusClosed, nil, nil)
	seedBead(ms, "gd-mr", mergeRequestType, model.StatusOpen, nil, map[string]any{
		"mr_url": "https://gitlab/x/-/merge_requests/2", "mr_state": "opened",
	})
	linkProduces(ms, "gd-fix", "gd-mr")
	linkBlocks(ms, "gd-bug", "gd-fix")

	s.afterBeadClosed(ctx, ms.beads["gd-fix"])

	if got := ms.beads["gd-bug"].Status; got != model.StatusOpen {
		t.Fatalf("expected bug with unmerged-MR blocker to stay open, got status=%q", got)
	}
}

// TestAfterBeadClosed_BlockedBug_OpenSiblingBlocker_StaysOpen verifies the
// all-blockers-closed gate: a bug with a merged fix-task AND a still-open second
// blocking fix-task is left open until every blocker closes.
func TestAfterBeadClosed_BlockedBug_OpenSiblingBlocker_StaysOpen(t *testing.T) {
	s, ms := newAutoCloseServer()
	ctx := context.Background()

	seedBead(ms, "gd-bug", model.TypeBug, model.StatusOpen, []string{closeOnBlockingFixMergeLabel}, nil)
	seedMergedFixTask(ms, "gd-fix1", "gd-mr", "https://gitlab/x/-/merge_requests/6950")
	seedBead(ms, "gd-fix2", model.TypeTask, model.StatusOpen, nil, nil) // still open
	linkBlocks(ms, "gd-bug", "gd-fix1")
	linkBlocks(ms, "gd-bug", "gd-fix2")

	s.afterBeadClosed(ctx, ms.beads["gd-fix1"])

	if got := ms.beads["gd-bug"].Status; got != model.StatusOpen {
		t.Fatalf("expected bug with an open sibling blocker to stay open, got status=%q", got)
	}
}

// TestAfterBeadClosed_BlockedBug_NoAutoCloseOptOut verifies the explicit opt-out:
// a bug carrying no-auto-close is left open even with a merged blocking fix.
func TestAfterBeadClosed_BlockedBug_NoAutoCloseOptOut(t *testing.T) {
	s, ms := newAutoCloseServer()
	ctx := context.Background()

	seedBead(ms, "gd-bug", model.TypeBug, model.StatusOpen,
		[]string{closeOnBlockingFixMergeLabel, noAutoCloseLabel}, nil)
	seedMergedFixTask(ms, "gd-fix", "gd-mr", "https://gitlab/x/-/merge_requests/6950")
	linkBlocks(ms, "gd-bug", "gd-fix")

	s.afterBeadClosed(ctx, ms.beads["gd-fix"])

	if got := ms.beads["gd-bug"].Status; got != model.StatusOpen {
		t.Fatalf("expected no-auto-close bug to stay open, got status=%q", got)
	}
}

// TestAfterBeadClosed_BlockedBug_NonBugTypeIgnored verifies the type gate: only a
// type=bug is closed via this path (a task blocked-by a merged fix is not).
func TestAfterBeadClosed_BlockedBug_NonBugTypeIgnored(t *testing.T) {
	s, ms := newAutoCloseServer()
	ctx := context.Background()

	seedBead(ms, "gd-task", model.TypeTask, model.StatusOpen, []string{closeOnBlockingFixMergeLabel}, nil)
	seedMergedFixTask(ms, "gd-fix", "gd-mr", "https://gitlab/x/-/merge_requests/6950")
	linkBlocks(ms, "gd-task", "gd-fix")

	s.afterBeadClosed(ctx, ms.beads["gd-fix"])

	if got := ms.beads["gd-task"].Status; got != model.StatusOpen {
		t.Fatalf("expected non-bug blocked bead left untouched, got status=%q", got)
	}
}

// TestAfterBeadClosed_BlockedBug_CascadesToParentEpic verifies the closed bug
// re-enters afterBeadClosed and drains its own parent epic when it was the last
// open child (acceptance #3: no regression to parent-child cascade).
func TestAfterBeadClosed_BlockedBug_CascadesToParentEpic(t *testing.T) {
	s, ms := newAutoCloseServer()
	ctx := context.Background()

	seedBead(ms, "gd-epic", model.TypeEpic, model.StatusOpen, nil, nil)
	seedBead(ms, "gd-bug", model.TypeBug, model.StatusOpen, []string{closeOnBlockingFixMergeLabel}, nil)
	linkParentChild(ms, "gd-bug", "gd-epic")
	seedMergedFixTask(ms, "gd-fix", "gd-mr", "https://gitlab/x/-/merge_requests/6950")
	linkBlocks(ms, "gd-bug", "gd-fix")

	s.afterBeadClosed(ctx, ms.beads["gd-fix"])

	if got := ms.beads["gd-bug"].Status; got != model.StatusClosed {
		t.Fatalf("expected bug auto-closed, got status=%q", got)
	}
	if got := ms.beads["gd-epic"].Status; got != model.StatusClosed {
		t.Fatalf("expected parent epic drained after bug close, got status=%q", got)
	}
}

// TestAfterBeadClosed_BlockedBug_TerminalBugSkipped verifies idempotency: an
// already-closed bug is not re-processed (no spurious re-close / comment).
func TestAfterBeadClosed_BlockedBug_TerminalBugSkipped(t *testing.T) {
	s, ms := newAutoCloseServer()
	ctx := context.Background()

	seedBead(ms, "gd-bug", model.TypeBug, model.StatusClosed, []string{closeOnBlockingFixMergeLabel}, nil)
	seedMergedFixTask(ms, "gd-fix", "gd-mr", "https://gitlab/x/-/merge_requests/6950")
	linkBlocks(ms, "gd-bug", "gd-fix")

	s.afterBeadClosed(ctx, ms.beads["gd-fix"])

	if len(ms.comments["gd-bug"]) != 0 {
		t.Fatalf("expected no comment on an already-closed bug, got %d", len(ms.comments["gd-bug"]))
	}
}

// TestAfterBeadClosed_EscalationWrapper_WaitsForEverySlice covers a task split
// into slice PRs, one --close-on-execute merge escalation each (gd-ed4jSP0ssQ):
// the first slice's merge must not close the task while another slice's
// escalation or PR is still open. Once only the executed escalation's own PR is
// left (its merge-request bead not yet caught up), the task closes.
func TestAfterBeadClosed_EscalationWrapper_WaitsForEverySlice(t *testing.T) {
	s, ms := newAutoCloseServer()
	ctx := context.Background()

	slice := func(esc, pr string, status model.Status) {
		seedBead(ms, esc, model.TypeEscalation, status, nil, map[string]any{
			"operation_type":        "gitlab_mr_merge",
			"requesting_task":       "gd-task",
			"close_requesting_task": true,
			"execution_spec":        `{"mr_url":"` + pr + `"}`,
		})
		linkBlocks(ms, "gd-task", esc)
	}
	seedBead(ms, "gd-task", model.TypeTask, model.StatusInProgress, nil, nil)
	slice("gd-esc-1", "https://github.com/o/r/pull/1842", model.StatusClosed)
	slice("gd-esc-2", "https://github.com/o/r/pull/1852", model.StatusOpen)
	for id, url := range map[string]string{"gd-mr-1": "https://github.com/o/r/pull/1842", "gd-mr-2": "https://github.com/o/r/pull/1852"} {
		seedBead(ms, id, mergeRequestType, model.StatusOpen, nil, map[string]any{"mr_url": url, "mr_state": "open"})
		linkProduces(ms, "gd-task", id)
	}

	s.afterBeadClosed(ctx, ms.beads["gd-esc-1"])
	if got := ms.beads["gd-task"].Status; got == model.StatusClosed {
		t.Fatalf("task closed while slice escalation gd-esc-2 is still open")
	}

	// gd-esc-2 is sent back (closed, no merge); its PR stays open.
	ms.beads["gd-esc-2"].Status = model.StatusClosed
	s.afterBeadClosed(ctx, ms.beads["gd-esc-1"])
	if got := ms.beads["gd-task"].Status; got == model.StatusClosed {
		t.Fatalf("task closed while slice PR #1852 is still open")
	}

	// #1852 closes unmerged: only gd-esc-1's own PR is left open.
	ms.beads["gd-mr-2"].Status = model.StatusClosed
	s.afterBeadClosed(ctx, ms.beads["gd-esc-1"])
	if got := ms.beads["gd-task"].Status; got != model.StatusClosed {
		t.Fatalf("expected task closed once only the executed escalation's own PR is open, got %q", got)
	}
}
