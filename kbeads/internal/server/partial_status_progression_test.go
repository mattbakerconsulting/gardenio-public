package server

import (
	"testing"

	"gardenio/kbeads/internal/model"
)

// TestReadyQueue_PartialBlockerUnblocksDownstream asserts that the /v1/ready
// blocker walk treats a 'partial' blocker as terminal-for-progression, exactly
// like 'closed'. A downstream step blocked only by a 'partial' step must
// surface as ready, while a step blocked by an OPEN step must not. This is the
// engine-level guarantee behind first-class 'partial' (ka-0000007305 §2.3):
// a partial step unblocks its dependents so the success path proceeds without
// waiting on the failed descendants it flagged.
func TestReadyQueue_PartialBlockerUnblocksDownstream(t *testing.T) {
	_, ms, h := newTestServer()

	// downstream-after-partial is blocked only by a partial (terminal) step.
	ms.beads["gd-partial-step"] = &model.Bead{ID: "gd-partial-step", Title: "Step with some failures", Status: model.StatusPartial}
	ms.beads["gd-after-partial"] = &model.Bead{ID: "gd-after-partial", Title: "Downstream of partial", Status: model.StatusOpen}
	ms.deps["gd-after-partial"] = []*model.Dependency{
		{BeadID: "gd-after-partial", DependsOnID: "gd-partial-step", Type: model.DepBlocks},
	}

	// downstream-after-closed is the closed-blocker control: must also be ready.
	ms.beads["gd-closed-step"] = &model.Bead{ID: "gd-closed-step", Title: "Cleanly closed step", Status: model.StatusClosed}
	ms.beads["gd-after-closed"] = &model.Bead{ID: "gd-after-closed", Title: "Downstream of closed", Status: model.StatusOpen}
	ms.deps["gd-after-closed"] = []*model.Dependency{
		{BeadID: "gd-after-closed", DependsOnID: "gd-closed-step", Type: model.DepBlocks},
	}

	// downstream-after-open is the negative control: blocked by an OPEN step,
	// so it must NOT be ready.
	ms.beads["gd-open-step"] = &model.Bead{ID: "gd-open-step", Title: "Still-active step", Status: model.StatusOpen}
	ms.beads["gd-after-open"] = &model.Bead{ID: "gd-after-open", Title: "Downstream of open", Status: model.StatusOpen}
	ms.deps["gd-after-open"] = []*model.Dependency{
		{BeadID: "gd-after-open", DependsOnID: "gd-open-step", Type: model.DepBlocks},
	}

	rec := doJSON(t, h, "GET", "/v1/ready", nil)
	requireStatus(t, rec, 200)
	var result struct {
		Beads []*model.Bead `json:"beads"`
		Total int           `json:"total"`
	}
	decodeJSON(t, rec, &result)

	ready := make(map[string]bool, len(result.Beads))
	for _, b := range result.Beads {
		ready[b.ID] = true
	}

	if !ready["gd-after-partial"] {
		t.Errorf("gd-after-partial (blocked only by a PARTIAL step) should be ready — partial is terminal-for-progression; got beads=%v", ready)
	}
	if !ready["gd-after-closed"] {
		t.Errorf("gd-after-closed (blocked only by a CLOSED step) should be ready; got beads=%v", ready)
	}
	if ready["gd-after-open"] {
		t.Errorf("gd-after-open (blocked by an OPEN step) must NOT be ready; got beads=%v", ready)
	}
}
