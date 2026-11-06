package main

import (
	"context"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// Durable settlement sweep (A3, ka-0000007288). The close-time settlement path
// is inline-attempt + an IN-MEMORY retry queue (reconcile_session_spend.go) —
// a controller restart drops every pending retry permanently, and the ~8-minute
// retry budget is shorter than LiteLLM's worst-case spend-log indexing lag.
// Measured cost before this sweep: ~5.4% of closed agent beads never settled.
//
// The durable state is the bead itself: a closed agent bead whose settlement
// watermark (litellm_spend_close) is absent — or lags the tag's current total —
// still has unbooked spend. This sweep periodically scans recently-closed agent
// beads, publishes the backlog as gardenio_unsettled_sessions, and re-attempts
// settlement through the same settle() math the close path uses (idempotent by
// watermark, so a bead the inline path settles concurrently books nothing
// twice).
const (
	// defaultSpendSweepWindow bounds how far back the sweep looks. Old enough to
	// outlive any indexing lag or controller outage; young enough that the scan
	// stays a few thousand beads.
	defaultSpendSweepWindow = 48 * time.Hour
	// spendSweepGrace leaves the freshest closes to the inline+retry path —
	// sweeping a bead seconds after close would race the retry worker for no
	// benefit and misread indexing lag as a lost settlement.
	spendSweepGrace = 10 * time.Minute
	// spendSweepEpsilon is the tag-total-vs-watermark slack below which a bead
	// counts as settled. LiteLLM totals are dollars; half a cent is noise.
	spendSweepEpsilon = 0.005
	// spendSweepPageSize / spendSweepMaxPages bound one cycle's daemon reads.
	spendSweepPageSize = 500
	spendSweepMaxPages = 10
)

// RunSettlementSweep runs the durable settlement sweep every interval until ctx
// is cancelled. An interval <= 0 disables the sweep. Call from main in its own
// goroutine, after metrics/resolvers are wired.
func (r *sessionSpendReconciler) RunSettlementSweep(ctx context.Context, interval time.Duration) {
	if r == nil {
		return
	}
	if interval <= 0 {
		r.logger.Info("settlement sweep disabled (interval=0)")
		return
	}
	r.logger.Info("settlement sweep started", "interval", interval, "window", defaultSpendSweepWindow)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.sweepSettlements(ctx, time.Now())
		}
	}
}

// sweepSettlements runs one sweep cycle at the given wall-clock instant
// (injected for tests). It lists closed agent beads inside the window, decides
// which still carry unbooked spend, publishes the backlog gauge, and re-settles
// each candidate using one cached /spend/tags read per distinct proxy.
func (r *sessionSpendReconciler) sweepSettlements(ctx context.Context, now time.Time) {
	closed := r.listAgentBeads(ctx, []string{"closed"}, now.Add(-defaultSpendSweepWindow))
	live := r.listAgentBeads(ctx, []string{"open", "in_progress"}, time.Time{})

	// Newest bead creation per agent name, across live beads and the closed
	// window. Guards the name-reuse hazard below.
	newestByName := map[string]time.Time{}
	for _, b := range append(append([]*beadsapi.BeadDetail{}, closed...), live...) {
		if b.Title == "" {
			continue
		}
		if t, ok := newestByName[b.Title]; !ok || b.CreatedAt.After(t) {
			newestByName[b.Title] = b.CreatedAt
		}
	}

	// One /spend/tags read per distinct proxy per cycle. A nil map value marks a
	// proxy whose read failed — its beads are skipped this cycle rather than
	// misread as settled.
	tagTotals := map[string]map[string]float64{}
	totalsFor := func(baseURL string) map[string]float64 {
		if totals, ok := tagTotals[baseURL]; ok {
			return totals
		}
		entries, err := r.queryAllTagSpend(ctx, baseURL)
		if err != nil {
			r.logger.Warn("settlement sweep: /spend/tags read failed", "base_url", baseURL, "error", err)
			tagTotals[baseURL] = nil
			return nil
		}
		totals := make(map[string]float64, len(entries))
		for _, e := range entries {
			totals[e.IndividualRequestTag] = e.TotalSpend
		}
		tagTotals[baseURL] = totals
		return totals
	}

	var candidates []*beadsapi.BeadDetail
	var candidateTotals []float64
	for _, b := range closed {
		agentName := b.Title
		if agentName == "" {
			continue
		}
		closedAt, err := time.Parse(time.RFC3339, b.ClosedAt)
		if err != nil || now.Sub(closedAt) < spendSweepGrace {
			continue
		}
		// Name-reuse guard: a bead with this agent name created after this bead
		// closed owns the tag's spend since its own baseline. Settling the older
		// bead against the tag's CURRENT total would book the newer bead's spend
		// onto it — skip and let each bead settle at its own close.
		if newest, ok := newestByName[agentName]; ok && newest.After(closedAt) {
			continue
		}
		totals := totalsFor(r.baseURLFor(b.Fields["project"], b.Fields["role"]))
		if totals == nil {
			continue
		}
		total, found := totals[agentTag(agentName)]
		if !found {
			continue // tag never appeared: the bead genuinely spent nothing
		}
		baseline, baselineOK := parseBaselineField(b.Fields[spendBaselineField])
		watermark, watermarkOK := parseBaselineField(b.Fields[spendCloseField])
		if !baselineOK && !watermarkOK {
			// Unsettleable: no anchor to attribute from (see settle()'s refusal).
			// Not counted as backlog — no sweep cycle can ever clear it.
			continue
		}
		start := baseline
		if !baselineOK || (watermarkOK && watermark > start) {
			start = watermark
		}
		if total-start <= spendSweepEpsilon {
			continue // fully settled (or $0 residue)
		}
		candidates = append(candidates, b)
		candidateTotals = append(candidateTotals, total)
	}

	if r.metrics != nil {
		r.metrics.SetUnsettledSessions(len(candidates))
	}
	if len(candidates) == 0 {
		return
	}
	r.logger.Info("settlement sweep: re-settling unsettled closed beads", "count", len(candidates))
	for i, b := range candidates {
		r.settle(ctx, b, b.Title, candidateTotals[i], -1)
	}
}

// listAgentBeads pages through agent beads with the given statuses, optionally
// bounded to updated-after. Best-effort: a page error returns what was read so
// far (a partial sweep settles less this cycle, never wrongly).
func (r *sessionSpendReconciler) listAgentBeads(ctx context.Context, statuses []string, updatedAfter time.Time) []*beadsapi.BeadDetail {
	var out []*beadsapi.BeadDetail
	for page := 0; page < spendSweepMaxPages; page++ {
		result, err := r.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
			Types:        []string{"agent"},
			Statuses:     statuses,
			UpdatedAfter: updatedAfter,
			Sort:         "-updated_at",
			Limit:        spendSweepPageSize,
			Offset:       page * spendSweepPageSize,
			NoTotal:      true, // pages end on a short page; Total is never read
		})
		if err != nil {
			r.logger.Warn("settlement sweep: bead list failed", "error", err)
			return out
		}
		for _, b := range result.Beads {
			// Re-check type/status client-side: test stubs (and lenient daemons)
			// may return unfiltered rows.
			if b.Type != "agent" {
				continue
			}
			ok := false
			for _, s := range statuses {
				if b.Status == s {
					ok = true
					break
				}
			}
			if ok {
				out = append(out, b)
			}
		}
		if len(result.Beads) < spendSweepPageSize {
			return out
		}
	}
	return out
}
