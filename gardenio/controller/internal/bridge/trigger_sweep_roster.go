package bridge

import (
	"context"

	"gardenio/controller/internal/beadsapi"
)

// sweepRosterLimit bounds the open-trigger roster the per-cycle sweeps share.
// The live trigger fleet is tens of beads; the cap only guards against a
// runaway query. It replaced the four sweeps' equal per-sweep limits.
const sweepRosterLimit = 500

// sweepRosterQuery is the open-trigger roster the liveness, check-liveness,
// pouring-stall and dark-lane sweeps each read every Evaluate cycle. None of
// them reads a trigger's description, notes or fields.prompt (they read
// enabled / last_checked / last_run / action / conditions / the gardener
// block and write only their own stamp keys), so the projection drops them:
// prompt alone was a third of the open-trigger roster's bytes on 2026-10-23
// (gd-zXdZq4Traf).
func sweepRosterQuery() beadsapi.ListBeadsQuery {
	return beadsapi.ListBeadsQuery{
		Types:      []string{"trigger"},
		Statuses:   []string{"open"},
		Limit:      sweepRosterLimit,
		Omit:       []string{"description", "notes"},
		OmitFields: []string{"prompt"},
	}
}

// sweepRosterMemo holds one Evaluate cycle's open-trigger roster so the four
// sweeps that run back to back after the source passes share one read
// instead of four identical ones. It is set by Evaluate and only ever touched
// on Evaluate's goroutine.
type sweepRosterMemo struct {
	res *beadsapi.ListBeadsResult
}

// openTriggerRoster returns the open-trigger roster for a sweep: this cycle's
// shared read when Evaluate has a memo in place, else a fresh one (a sweep
// called on its own, as in tests). An error is not memoized, so each sweep
// still retries on its own after a failed read, as before.
func (e *TriggerEngine) openTriggerRoster(ctx context.Context) (*beadsapi.ListBeadsResult, error) {
	m := e.sweepRoster
	if m != nil && m.res != nil {
		return m.res, nil
	}
	res, err := e.daemon.ListBeadsFiltered(ctx, sweepRosterQuery())
	if err == nil && m != nil {
		m.res = res
	}
	return res, err
}

// leanAgentOmit / leanAgentOmitFields project an agent-bead listing for the
// engine's per-gardener lease and iteration reads, which use ids, timestamps
// and runtime fields only: an agent's fields.prompt was 71% of its bytes on
// 2026-10-23 (gd-zXdZq4Traf). wrapup stays — run-log reconcile reads it.
var (
	leanAgentOmit       = []string{"description", "notes"}
	leanAgentOmitFields = []string{"prompt"}
)
