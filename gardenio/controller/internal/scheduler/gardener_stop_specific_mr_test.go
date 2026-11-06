package scheduler

import (
	"context"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// fakeMRStopDaemon implements GardenerStopDaemon PLUS the optional mrURLResolver
// capability so the specific_mr_merged evaluator can resolve a target URL to its
// mirror bead. It maps mr_url -> mirror bead (or absence) to exercise the
// not-synced / open / merged states.
type fakeMRStopDaemon struct {
	// byURL maps a canonical MR URL to its type=merge-request mirror bead. A URL
	// missing from the map resolves to (nil, nil) — "not synced yet".
	byURL map[string]*beadsapi.BeadDetail
	// urlErr, when non-nil, is returned from MRBeadByURL to exercise the
	// fail-open-on-error path.
	urlErr error
}

func (f *fakeMRStopDaemon) ListBeadsFiltered(_ context.Context, _ beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	return &beadsapi.ListBeadsResult{}, nil
}

func (f *fakeMRStopDaemon) GetBead(_ context.Context, _ string) (*beadsapi.BeadDetail, error) {
	return nil, nil
}

func (f *fakeMRStopDaemon) IsAncestor(_ context.Context, _, _ string) (bool, error) {
	return false, nil
}

func (f *fakeMRStopDaemon) MRBeadByURL(_ context.Context, mrURL string) (*beadsapi.BeadDetail, error) {
	if f.urlErr != nil {
		return nil, f.urlErr
	}
	return f.byURL[mrURL], nil
}

// daemonWithoutMRResolver implements GardenerStopDaemon but NOT mrURLResolver, so
// specific_mr_merged must fail-open to not-met.
type daemonWithoutMRResolver struct{}

func (daemonWithoutMRResolver) ListBeadsFiltered(_ context.Context, _ beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	return &beadsapi.ListBeadsResult{}, nil
}
func (daemonWithoutMRResolver) GetBead(_ context.Context, _ string) (*beadsapi.BeadDetail, error) {
	return nil, nil
}
func (daemonWithoutMRResolver) IsAncestor(_ context.Context, _, _ string) (bool, error) {
	return false, nil
}

const testMRURL = "https://gitlab.com/acme/app/-/merge_requests/42"

func specificMRStopEntry(url string) StopEntry {
	return StopEntry{Kind: StopKindSpecificMRMerged, MRURL: url}
}

// TestGardenerStop_SpecificMRMerged is the behavioral lock for the
// --stop-on-mr-merged flag (task ka-0000005598): the entry fires ONLY when the
// exact target MR's mirror bead reports merged, and fails-open (not-met) in
// every uncertain state so a gardener never mis-closes on a transient read.
func TestGardenerStop_SpecificMRMerged(t *testing.T) {
	ctx := context.Background()
	sctx := GardenerStopContext{BeadID: "gd-cap", RunsSpawned: 1}

	t.Run("merged_via_mr_outcome_fires", func(t *testing.T) {
		d := &fakeMRStopDaemon{byURL: map[string]*beadsapi.BeadDetail{
			testMRURL: {ID: "gd-mr", Type: "merge-request", Fields: map[string]string{
				"mr_url": testMRURL, "mr_outcome": "merged",
			}},
		}}
		if !EvaluateGardenerStopEntry(ctx, d, nil, sctx, specificMRStopEntry(testMRURL)) {
			t.Fatal("expected specific_mr_merged to fire when mr_outcome==merged")
		}
	})

	t.Run("merged_via_mr_state_fires", func(t *testing.T) {
		d := &fakeMRStopDaemon{byURL: map[string]*beadsapi.BeadDetail{
			testMRURL: {ID: "gd-mr", Type: "merge-request", Fields: map[string]string{
				"mr_url": testMRURL, "mr_state": "merged",
			}},
		}}
		if !EvaluateGardenerStopEntry(ctx, d, nil, sctx, specificMRStopEntry(testMRURL)) {
			t.Fatal("expected specific_mr_merged to fire when mr_state==merged")
		}
	})

	t.Run("open_mr_does_not_fire", func(t *testing.T) {
		d := &fakeMRStopDaemon{byURL: map[string]*beadsapi.BeadDetail{
			testMRURL: {ID: "gd-mr", Type: "merge-request", Fields: map[string]string{
				"mr_url": testMRURL, "mr_state": "opened", "mr_pipeline_status": "running",
			}},
		}}
		if EvaluateGardenerStopEntry(ctx, d, nil, sctx, specificMRStopEntry(testMRURL)) {
			t.Fatal("expected specific_mr_merged NOT to fire for an open MR")
		}
	})

	t.Run("not_synced_mirror_absent_does_not_fire", func(t *testing.T) {
		d := &fakeMRStopDaemon{byURL: map[string]*beadsapi.BeadDetail{}}
		if EvaluateGardenerStopEntry(ctx, d, nil, sctx, specificMRStopEntry(testMRURL)) {
			t.Fatal("expected specific_mr_merged NOT to fire when the mirror bead is absent")
		}
	})

	t.Run("empty_url_does_not_fire", func(t *testing.T) {
		d := &fakeMRStopDaemon{byURL: map[string]*beadsapi.BeadDetail{}}
		if EvaluateGardenerStopEntry(ctx, d, nil, sctx, specificMRStopEntry("")) {
			t.Fatal("expected specific_mr_merged NOT to fire for an empty mr_url")
		}
	})

	t.Run("resolver_error_fails_open", func(t *testing.T) {
		d := &fakeMRStopDaemon{urlErr: context.DeadlineExceeded}
		if EvaluateGardenerStopEntry(ctx, d, nil, sctx, specificMRStopEntry(testMRURL)) {
			t.Fatal("expected specific_mr_merged to fail-open (not-met) on a resolver error")
		}
	})

	t.Run("daemon_without_resolver_fails_open", func(t *testing.T) {
		if EvaluateGardenerStopEntry(ctx, daemonWithoutMRResolver{}, nil, sctx, specificMRStopEntry(testMRURL)) {
			t.Fatal("expected specific_mr_merged to fail-open when the daemon cannot resolve MR URLs")
		}
	})
}

// TestGardenerStop_SpecificMRMergedTerminal proves a merged target routes through
// the loop_end slot as a TERMINAL (non-premature) match, so the fire path closes
// the gardener as success rather than resume-or-escalate.
func TestGardenerStop_SpecificMRMergedTerminal(t *testing.T) {
	ctx := context.Background()
	spec := StopSpec{LoopEnd: StopSlot{Any: []StopEntry{specificMRStopEntry(testMRURL)}}}
	d := &fakeMRStopDaemon{byURL: map[string]*beadsapi.BeadDetail{
		testMRURL: {ID: "gd-mr", Type: "merge-request", Fields: map[string]string{
			"mr_url": testMRURL, "mr_outcome": "merged",
		}},
	}}
	match, fired := EvaluateGardenerStopSpec(ctx, d, nil, GardenerStopContext{BeadID: "gd-cap", RunsSpawned: 1}, spec)
	if !fired {
		t.Fatal("expected the spec to fire on a merged target MR")
	}
	if match.Slot != StopSlotLoopEnd {
		t.Fatalf("expected loop_end slot, got %q", match.Slot)
	}
	if match.Kind != StopKindSpecificMRMerged {
		t.Fatalf("expected specific_mr_merged kind, got %q", match.Kind)
	}
	if match.Premature {
		t.Fatal("expected specific_mr_merged to be TERMINAL (Premature=false) — reaching the merge is success, not exhaustion")
	}
}

// TestStopKindIsTerminal_SpecificMRMerged locks the terminal classification so a
// future refactor can't silently flip it to premature (which would re-arm the
// gardener instead of closing it as done).
func TestStopKindIsTerminal_SpecificMRMerged(t *testing.T) {
	if !stopKindIsTerminal(StopKindSpecificMRMerged) {
		t.Fatal("specific_mr_merged must be terminal (a match closes the gardener as success)")
	}
	if stopKindIsTerminal(StopKindMRMerged) {
		t.Fatal("mr_merged (the subtree park) must stay premature — it must never close a gardener")
	}
}
