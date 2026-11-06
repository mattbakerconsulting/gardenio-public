package main

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
	gardeniometrics "gardenio/controller/internal/metrics"
)

// xcheckSweep is the cost-audit cross-check sweep (C2, plan ka-0000003747, bundle
// ka-0000004761). On a slow cadence it recomputes a set of conservation PAIRS —
// the same quantity derived two independent ways — and publishes each as
// gardenio_xcheck_value{pair,side} + gardenio_xcheck_agreement{pair} (see
// internal/metrics/xcheck.go). The agreement score is the operator's
// "double-check principle" made into a standing signal: two independent
// derivations of one number that stay close corroborate that we are measuring
// something real; sustained divergence is a measurement bug the CostXCheckDrift
// alert (C5) fires on.
//
// Two data-source classes back the pairs, chosen so the sweep audits the
// plumbing without doubling its load:
//
//   - PromQL against vmsingle (P1, P2, P6, P9-side-A). Most quantities are
//     ALREADY published as fleet metrics; the honest "second source" is to read
//     those series back two ways rather than re-hitting LiteLLM (which would
//     double the API load and skip the emit→scrape path the pair is meant to
//     verify). Skipped wholesale when no metrics upstream is wired.
//   - Bead scans against the daemon (P4, P5, P9-side-B). The stateful-era
//     per-agent/per-session/per-gardener cost fields (settled_cost_usd,
//     litellm_spend_close/first_baseline, total_cost_usd, gardener_spend_usd) are
//     not in Prometheus, so these are summed directly off the beads. Per the plan
//     these compare stateful-era fields C4 later deletes — they are built anyway
//     because they gate that cutover.
//
// The framework is deliberately a flat registry of pairs (pairs()): each pair is
// a name + a compute closure returning both sides. Adding a pair — e.g. P10 in
// C3 (stateful settled counter vs stateless /spend/logs recompute) — is one entry
// and one compute func, no sweep-loop changes.
type xcheckSweep struct {
	daemon  *beadsapi.Client
	vm      *xcheckVMClient
	metrics *gardeniometrics.XCheckMetrics
	logger  *slog.Logger

	// gardener is reused to compute P5's live BFS side without duplicating the
	// tested transitive-rollup walk. It shares the daemon; its logger is quieted
	// under this component.
	gardener *gardenerSpendSync

	// interval is the sweep cadence (config XCheckSweepInterval; defaults to
	// xcheckSweepInterval). A non-positive value disables the loop.
	interval time.Duration

	// published tracks which pairs currently have series live, so a pair that
	// transitions from derivable to underivable gets its stale corroboration
	// score actively cleared (ClearPair) rather than left frozen — a frozen
	// "healthy" score while a source is broken is the one failure mode worse than
	// a visible gap. Only touched from the single RunLoop goroutine.
	published map[string]bool
}

// Cross-check sweep tuning.
const (
	// xcheckSweepInterval is how often the pairs are recomputed. Cost sources
	// move at most every ~5 min (the spend/tag gauges) or per session close, so a
	// 15-minute cadence keeps the corroboration scores fresh without hammering
	// either vmsingle or the daemon. Set GARDENIO_XCHECK_SWEEP_INTERVAL=0 to
	// disable the sweep.
	xcheckSweepInterval = 15 * time.Minute

	// xcheckScanPageLimit bounds each daemon list page for a bead-scan pair.
	xcheckScanPageLimit = 500

	// xcheckScanMaxBeads caps how many beads a single bead-scan side will page
	// through. A side that would exceed it is TRUNCATED, and a truncated side
	// makes its pair unavailable for that cycle (skipped, logged) rather than
	// publishing a comparison against a partial population — a half-scanned sum
	// is worse than a visible gap. Fleet bead counts sit well under this today.
	xcheckScanMaxBeads = 20000

	// xcheckMaxGardenersP5 bounds the per-gardener BFS fan-out of the P5
	// rollup-vs-tree pair. The walk processes the first N gardeners on BOTH sides
	// identically (stored field and live recompute), so a cap yields a valid
	// comparison over that subset, logged when it truncates. Gardener populations
	// are operator-bounded (~dozens), so this covers the whole set in practice.
	xcheckMaxGardenersP5 = 200
)

// Pair names — the stable {pair} label values. Kept as constants so the sweep,
// the Grafana dashboard (C5), and the beads-viewer trust strip (C6) select the
// same series; a rename here is a compile reminder across consumers that import
// them. Values mirror the plan's P1–P9 shorthand.
const (
	xcheckPairBillConservation = "bill-conservation" // P1
	xcheckPairTagConservation  = "tag-conservation"  // P2
	// P3 settled-vs-bill was RETIRED (ka-0000000543). Its side A was the at-close
	// per-agent settled COUNTER gardenio_agent_spend_usd_total, which went dead in
	// prod ($36 lifetime vs a ~$40k bill — 0.09% of it) and was confirmed
	// structurally incapable of agreeing with side B (the live account bill) at
	// ANY floor (P0 ka-0000002699). It measured the DEPRECATED stateful path that
	// P10 agent-spend-parity (below, ~0.93–0.99 healthy) replaced; per-account
	// bill conservation is already covered by P1 bill-conservation (~0.99). No new
	// pair is needed — P1 + P10 cover P3's ground honestly. Not retiring the
	// counter itself here: it still has non-xcheck consumers (spend-reconciliation
	// alert, vmalert configmap) retired separately in ka-0000005951.
	xcheckPairSessionsVsAgent = "sessions-vs-agent" // P4
	xcheckPairRollupVsTree    = "rollup-vs-tree"    // P5
	xcheckPairTokenParity     = "token-parity"      // P6
	// P7 estimate-vs-settled was RETIRED (ka-0000007296): its side A, the in-pod
	// JSONL response_cost estimate (Σ session total_cost_usd), stopped existing
	// when coop went strictly cost-free on 2026-06-06 (b44b0b3c9, ka-0000002983;
	// locked by the coop cost-free guards). The pair was added to this sweep six
	// weeks later (f1d45562c, 2026-07-19) with a producer that was already gone,
	// so total_cost_usd is present on 0 session beads and its agreement never
	// published — NO-DATA in every crosscheck.json since. settled_cost_usd is
	// still independently witnessed by P4 sessions-vs-agent (Σ session settled
	// vs Σ agent LiteLLM delta).
	xcheckPairInterruptParity  = "interrupt-parity"   // P9
	xcheckPairAgentSpendParity = "agent-spend-parity" // P10
)

// xcheckSpendParityWindow bounds the P10 population to RECENTLY-TOUCHED agent
// beads. The spend-logs sweep keeps patching a tracked bead's
// spend_logs_cost_usd while the bead's created_at is inside its lookback
// (spendlogs_sweep.go), so recently-updated ≈ actively-tracked; beads that
// aged out of tracking carry two FROZEN, agreeing values forever, and without
// this window they would accumulate and dilute the pair into insensitivity —
// a fresh divergence on live agents is exactly the signal the C4 cutover
// gate needs, so the comparison must stay dominated by them. Keep it equal to
// the sweep's default lookback (spendLogsSweepLookback).
const xcheckSpendParityWindow = 48 * time.Hour

// PromQL window literals per windowed pair. P9 uses a day (involuntary deaths
// are rare enough that a 1h window is almost always 0/0); P6 tokens use 1h
// (high-volume, so a short window still corroborates and reacts to divergence
// faster). (P3 also used the day window before it was retired, ka-0000000543.)
const (
	xcheckWindowDay  = "24h"
	xcheckWindowHour = "1h"
	// xcheckInterruptWindow is the Go-duration twin of the P9 PromQL window: the
	// bead-scan side counts agents closed within the SAME lookback so both sides
	// cover one window. Keep it equal to xcheckWindowDay.
	xcheckInterruptWindow = 24 * time.Hour
)

func newXCheckSweep(daemon *beadsapi.Client, vm *xcheckVMClient, metrics *gardeniometrics.XCheckMetrics, interval time.Duration, logger *slog.Logger) *xcheckSweep {
	l := logger.With("component", "xcheck-sweep")
	if interval <= 0 {
		interval = xcheckSweepInterval
	}
	return &xcheckSweep{
		daemon:    daemon,
		vm:        vm,
		metrics:   metrics,
		logger:    l,
		gardener:  newGardenerSpendSync(daemon, l),
		interval:  interval,
		published: make(map[string]bool),
	}
}

// RunLoop recomputes every pair each sweep interval until ctx is cancelled, with
// one immediate pass on start.
func (s *xcheckSweep) RunLoop(ctx context.Context) {
	s.logger.Info("xcheck sweep starting",
		"interval", s.interval, "metrics_upstream_wired", s.vm.vmAvailable())
	s.syncAll(ctx)

	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			s.syncAll(ctx)
		case <-ctx.Done():
			return
		}
	}
}

// syncAll computes every registered pair and publishes those with both sides
// available, clearing the series of any pair that went unavailable since last
// cycle.
func (s *xcheckSweep) syncAll(ctx context.Context) {
	pairs := s.pairs()
	published, cleared, unavailable := 0, 0, 0
	for _, p := range pairs {
		res, err := p.compute(ctx)
		if err != nil {
			s.logger.Warn("xcheck: pair compute failed", "pair", p.name, "error", err)
			s.retire(p.name)
			cleared++
			continue
		}
		if !res.haveA || !res.haveB {
			s.logger.Debug("xcheck: pair unavailable this cycle",
				"pair", p.name, "have_a", res.haveA, "have_b", res.haveB)
			if s.retire(p.name) {
				cleared++
			}
			unavailable++
			continue
		}
		s.metrics.SetPair(p.name, res.a, res.b)
		s.published[p.name] = true
		published++
		s.logger.Debug("xcheck: pair published",
			"pair", p.name, "side_a", res.a, "side_b", res.b,
			"agreement", gardeniometrics.XCheckAgreement(res.a, res.b))
	}
	s.logger.Debug("xcheck sweep complete",
		"pairs", len(pairs), "published", published, "cleared", cleared, "unavailable", unavailable)
}

// retire clears a pair's series if it was previously published, returning true
// when a clear actually happened. Idempotent for a never-published pair.
func (s *xcheckSweep) retire(name string) bool {
	if !s.published[name] {
		return false
	}
	s.metrics.ClearPair(name)
	delete(s.published, name)
	return true
}

// xcheckResult carries a pair's two independently-derived sides and whether each
// was obtained this cycle. A pair is published only when both haveA and haveB.
type xcheckResult struct {
	a, b         float64
	haveA, haveB bool
}

// bothAvail is the common case: both sides derived.
func bothAvail(a, b float64) xcheckResult {
	return xcheckResult{a: a, b: b, haveA: true, haveB: true}
}

// xcheckPair is one conservation check: a stable name and a compute closure that
// derives both sides. The registry (pairs()) is the whole framework — a new pair
// is one struct literal.
type xcheckPair struct {
	name    string
	compute func(ctx context.Context) (xcheckResult, error)
}

// pairs is the pair registry: P1, P2, P4–P6, P9, P10 (P3 retired ka-0000000543;
// P7 retired ka-0000007296).
func (s *xcheckSweep) pairs() []xcheckPair {
	return []xcheckPair{
		{xcheckPairBillConservation, s.computeBillConservation},
		{xcheckPairTagConservation, s.computeTagConservation},
		// P3 settled-vs-bill retired (ka-0000000543) — see the const block above.
		{xcheckPairSessionsVsAgent, s.computeSessionsVsAgent},
		{xcheckPairRollupVsTree, s.computeRollupVsTree},
		{xcheckPairTokenParity, s.computeTokenParity},
		// P7 estimate-vs-settled retired (ka-0000007296) — see the constant block.
		{xcheckPairInterruptParity, s.computeInterruptParity},
		{xcheckPairAgentSpendParity, s.computeAgentSpendParity},
	}
}

// --- PromQL-backed pairs ----------------------------------------------------

// vmPair derives both sides from two instant PromQL queries. Either side missing
// (no upstream, metric absent, upstream error) yields an unavailable result so
// the sweep leaves the pair absent — never a fabricated agreement.
func (s *xcheckSweep) vmPair(ctx context.Context, qa, qb string) (xcheckResult, error) {
	a, okA := s.vm.query(ctx, qa)
	b, okB := s.vm.query(ctx, qb)
	return xcheckResult{a: a, b: b, haveA: okA, haveB: okB}, nil
}

// P1 bill-conservation: Σ per-account bill (from LiteLLM /key/info, published as
// gardenio_account_cost_usd) vs attributed+unattributed (from /spend/tags +
// /global/spend, published as gardenio_agent_attributed_cost_usd +
// gardenio_unattributed_cost_usd). Both reconstruct the whole LiteLLM bill by
// construction; divergence catches proxy-merge bugs and partial reads. The
// account gauge is deduped max by(account,env) to collapse per-scrape duplicates
// (same shape the /costs headline uses).
func (s *xcheckSweep) computeBillConservation(ctx context.Context) (xcheckResult, error) {
	const sideA = `sum(max by (account, env) (gardenio_account_cost_usd))`
	const sideB = `sum(gardenio_agent_attributed_cost_usd) + sum(gardenio_unattributed_cost_usd)`
	return s.vmPair(ctx, sideA, sideB)
}

// P2 tag-conservation: Σ tags dim=role vs Σ tags dim=project. LiteLLM books the
// full request cost under EVERY tag, so sum(role:*) ≡ sum(project:*) ≡ the bill;
// divergence catches untagged requests and tag drops on one dimension.
func (s *xcheckSweep) computeTagConservation(ctx context.Context) (xcheckResult, error) {
	const sideA = `sum(gardenio_tag_cost_usd{dim="role"})`
	const sideB = `sum(gardenio_tag_cost_usd{dim="project"})`
	return s.vmPair(ctx, sideA, sideB)
}

// P3 settled-vs-bill was RETIRED (ka-0000000543). It compared
// increase(gardenio_agent_spend_usd_total[24h]) (side A) against the live account
// bill (side B). Side A's counter went dead in prod ($36 lifetime, ~0.09% of the
// ~$40k bill), so the pair read structurally low at any floor — a broken
// measurement, not a settlement collapse (the unsettled backlog sawtooths to 0
// and attribution conserves; P0 ka-0000002699). P1 bill-conservation (~0.99)
// already corroborates the bill, and P10 agent-spend-parity (~0.93–0.99) covers
// per-agent settled attribution via the stateless /spend/logs recompute — so the
// pair's ground is covered without it. Its compute closure and const are gone;
// values.yaml, the trust strip, the runbook, and the sweep doc were updated in
// lockstep.

// P6 token-parity: the gb-pushed per-session token counter (one series set at
// close, side A) vs the coop-scraped live per-session token gauges
// (input+output+cache_read+cache_write, side B). Two independent producers of the
// same token totals; divergence catches JSONL parse/dedup bugs.
//
// Why NOT increase(): side A is PUSH-ONCE-AT-CLOSE (gb POSTs
// gardenio_session_tokens_total a single time when a session ends,
// cmd/gb/session_metrics_push.go), so every series carries exactly one sample.
// increase()/rate() over a single-sample series is 0 — there is no second point
// to difference against — which pinned this pair at agreement 0.000 for its
// entire life (ka-0000002557, verified fleet-wide against vmsingle: side A
// increase[1h] ≡ 0 vs side B ~1.6e8). Both metrics are cumulative counters, so
// the honest comparison is of LEVELS, read with last_over_time over the window
// rather than a windowed delta.
//
// Population alignment: side A exists only for CLOSED sessions (keyed by
// session_id+type+agent_name); side B is scraped continuously from LIVE pods
// (keyed by agent_name, no session_id) and disappears when the pod is torn down.
// Comparing raw fleet sums would pit two disjoint populations against each other
// (a live agent contributes to B but not yet A; a long-closed agent the reverse),
// so both sides are intersected on agent_name via `and on (agent_name)` — the set
// of agents that are BOTH recently-live (in B) AND have pushed a close total (in
// A). Over that shared population the two producers should agree closely; the
// score is a corroboration trend, not a hard equality (an agent mid-session
// contributes its live-scrape level to B and its previous close level to A). The
// intersection also preserves the framework's graceful-degradation contract: if
// either producer emits nothing in the window the intersection is empty, the
// query returns no vector, and the pair is published as absent rather than a
// fabricated score. Verified against vmsingle: agreement ~0.93–0.97 in
// steady-state windows (ka-0000002557).
func (s *xcheckSweep) computeTokenParity(ctx context.Context) (xcheckResult, error) {
	// Per-agent close-total level (side A source) and live-scrape level sum
	// (side B source), each windowed with last_over_time so a single push is
	// still observed.
	const closeLevel = `sum by (agent_name) (last_over_time(gardenio_session_tokens_total[%[1]s]))`
	const liveLevel = `(sum by (agent_name) (last_over_time(gardenio_session_input_tokens_total[%[1]s])) + ` +
		`sum by (agent_name) (last_over_time(gardenio_session_output_tokens_total[%[1]s])) + ` +
		`sum by (agent_name) (last_over_time(gardenio_session_cache_read_tokens_total[%[1]s])) + ` +
		`sum by (agent_name) (last_over_time(gardenio_session_cache_write_tokens_total[%[1]s])))`

	closeQ := fmt.Sprintf(closeLevel, xcheckWindowHour)
	liveQ := fmt.Sprintf(liveLevel, xcheckWindowHour)

	// Restrict each side to agents present on BOTH sides, then sum.
	sideA := fmt.Sprintf(`sum((%[1]s) and on (agent_name) (%[2]s))`, closeQ, liveQ)
	sideB := fmt.Sprintf(`sum((%[2]s) and on (agent_name) (%[1]s))`, closeQ, liveQ)
	return s.vmPair(ctx, sideA, sideB)
}

// --- Bead-scan pairs --------------------------------------------------------

// P4 sessions-vs-agent: Σ session settled_cost_usd vs Σ agent (litellm_spend_close
// − litellm_spend_first_baseline). Both should equal the fleet's lifetime settled
// spend — the session ledger and the agent lifetime-delta are two bookings of the
// same settlements; divergence catches lost/double session settlement. A
// truncated scan on either side makes the pair unavailable (partial populations
// are not comparable).
func (s *xcheckSweep) computeSessionsVsAgent(ctx context.Context) (xcheckResult, error) {
	var sessionSettled float64
	sessCount, sessTrunc, err := s.scanBeads(ctx, beadsapi.ListBeadsQuery{Types: []string{"session"}}.OnlyFields(sessionSettledCostField), func(b *beadsapi.BeadDetail) {
		sessionSettled += parseXCheckFloat(b.Fields[sessionSettledCostField])
	})
	if err != nil {
		return xcheckResult{}, fmt.Errorf("scanning session beads: %w", err)
	}

	var agentDelta float64
	agCount, agTrunc, err := s.scanBeads(ctx, beadsapi.ListBeadsQuery{Types: []string{"agent"}}.OnlyFields(spendCloseField, spendFirstBaselineField), func(b *beadsapi.BeadDetail) {
		closeTotal := parseXCheckFloat(b.Fields[spendCloseField])
		firstBaseline := parseXCheckFloat(b.Fields[spendFirstBaselineField])
		if d := closeTotal - firstBaseline; d > 0 {
			agentDelta += d
		}
	})
	if err != nil {
		return xcheckResult{}, fmt.Errorf("scanning agent beads: %w", err)
	}

	if sessTrunc || agTrunc {
		s.logger.Warn("xcheck P4: bead scan truncated, pair skipped this cycle",
			"session_truncated", sessTrunc, "agent_truncated", agTrunc, "cap", xcheckScanMaxBeads)
		return xcheckResult{}, nil
	}
	if sessCount == 0 && agCount == 0 {
		// Empty fleet — nothing to corroborate. Publishing a 0/0 pair would read
		// as a misleadingly-healthy agreement of 1.0; leave it absent instead.
		return xcheckResult{}, nil
	}
	return bothAvail(sessionSettled, agentDelta), nil
}

// P7 estimate-vs-settled (computeEstimateVsSettled) was REMOVED (ka-0000007296).
// Its side A summed session total_cost_usd (the in-pod JSONL response_cost
// estimate), a field that no producer writes: coop went strictly cost-free on
// 2026-06-06 (b44b0b3c9, ka-0000002983), so total_cost_usd is present on 0
// session beads and the pair could never derive both sides. It published no
// score in any cycle since it was added (f1d45562c, 2026-07-19). The settled
// side remains cross-checked by P4 sessions-vs-agent and P3 settled-vs-bill,
// so no corroboration of settled_cost_usd is lost by the removal.

// P10 agent-spend-parity: Σ agent-bead total_cost_usd (the OLD stateful path:
// spawn baseline → close delta → watermark, reconcile_session_spend.go) vs Σ
// spend_logs_cost_usd (the NEW stateless path: full recompute from LiteLLM
// /spend/logs rows, spendlogs_sweep.go), summed over the SAME agents — only
// beads carrying BOTH fields contribute, so the two sides always cover one
// population. This is the replace-now transition's safety gate (decision
// ka-0000006903): C4 cuts consumers over to the new path and deletes the
// baseline/watermark machinery only after this pair's agreement holds.
//
// Two known, EXPECTED contributors to sub-1.0 agreement — both are old-path
// defects the new path exists to fix, so read the score as a trend:
//   - close-time flush lag: settlement snapshots /spend/tags at bead close,
//     but the session's last rows land up to ~1min later; settled reads
//     $0.01–0.40 LOW per agent (measured in ka-0000001899).
//   - gb-estimate residue: total_cost_usd falls back to the in-pod JSONL
//     estimate for sessions whose settlement never lands.
//
// The population is bounded to beads updated inside xcheckSpendParityWindow
// (see that constant) so frozen historical pairs cannot dilute a fresh
// divergence on live agents.
func (s *xcheckSweep) computeAgentSpendParity(ctx context.Context) (xcheckResult, error) {
	var settled, recomputed float64
	contributing := 0
	_, trunc, err := s.scanBeads(ctx, beadsapi.ListBeadsQuery{
		Types:        []string{"agent"},
		UpdatedAfter: time.Now().Add(-xcheckSpendParityWindow),
	}.OnlyFields("total_cost_usd", spendLogsCostField), func(b *beadsapi.BeadDetail) {
		oldSide := b.Fields["total_cost_usd"]
		newSide := b.Fields[spendLogsCostField]
		if oldSide == "" || newSide == "" {
			return
		}
		settled += parseXCheckFloat(oldSide)
		recomputed += parseXCheckFloat(newSide)
		contributing++
	})
	if err != nil {
		return xcheckResult{}, fmt.Errorf("scanning agent beads: %w", err)
	}
	if trunc {
		s.logger.Warn("xcheck P10: agent scan truncated, pair skipped this cycle", "cap", xcheckScanMaxBeads)
		return xcheckResult{}, nil
	}
	if contributing == 0 {
		// No agent carries both figures yet (fresh deploy, sweep disabled, or
		// an idle window) — nothing to corroborate; absent, not a hollow 0/0.
		return xcheckResult{}, nil
	}
	// side a = old stateful settled, side b = new stateless recompute, so the
	// trust strip reads a→b as "old path corroborated by the new one".
	return bothAvail(settled, recomputed), nil
}

// P5 rollup-vs-tree: Σ maintained gardener_spend_usd (the rollup field
// gardenerSpendSync writes) vs Σ live BFS recompute over each gardener's iteration
// + transitive crew tree. Divergence is rollup staleness or missed descendants.
// The two sides walk the SAME first-N gardeners, so a cap yields a valid subset
// comparison (logged); reuses agentSpendIndex.rollupGardenerSpend so the live
// side is the identical, tested walk.
func (s *xcheckSweep) computeRollupVsTree(ctx context.Context) (xcheckResult, error) {
	gardeners, err := s.gardener.listGardeners(ctx)
	if err != nil {
		return xcheckResult{}, fmt.Errorf("listing gardeners: %w", err)
	}
	idx, err := s.gardener.loadAgentSpendIndex(ctx)
	if err != nil {
		return xcheckResult{}, fmt.Errorf("listing agents: %w", err)
	}
	var stored, live float64
	truncated := false
	for i, cap := range gardeners {
		if cap == nil {
			continue
		}
		if i >= xcheckMaxGardenersP5 {
			truncated = true
			break
		}
		liveSpend, _, _ := idx.rollupGardenerSpend(cap.ID)
		stored += parseGardenerSpendCost(cap.Fields[gardenerSpendField])
		live += liveSpend
	}
	if truncated {
		s.logger.Warn("xcheck P5: gardener set truncated, comparing first N",
			"n", xcheckMaxGardenersP5, "total", len(gardeners))
	}
	if len(gardeners) == 0 {
		// No gardeners → nothing to corroborate; leave the pair absent rather than
		// publishing a hollow 0/0.
		return xcheckResult{}, nil
	}
	return bothAvail(stored, live), nil
}

// P9 interrupt-parity: windowed increase of gardenio_agent_involuntary_deaths_total
// (Prometheus, side A) vs a kbeads scan of agent beads closed in the same window
// whose close_reason counts as an involuntary death (side B). Two stores, one
// count; divergence means the deaths counter and the bead close_reason record
// disagree about how many agents died mid-work. Side A needs the metrics
// upstream; side B is a cheap recent-closes scan. Side B deliberately reuses the
// emitter's own classifier (gardeniometrics.InvoluntaryDeathFamily — the
// `_involuntary` marker families plus the unmarked context_cancelled_unclaimed
// family, C9 ka-0000001146, and the unmarked context_cancelled_claim_lookup_failed
// family, gd-TYjMaxCMZR) so the two sides count the same predicate by
// construction.
func (s *xcheckSweep) computeInterruptParity(ctx context.Context) (xcheckResult, error) {
	sideAQuery := fmt.Sprintf(`sum(increase(gardenio_agent_involuntary_deaths_total[%s]))`, xcheckWindowDay)
	deathsMetric, okA := s.vm.query(ctx, sideAQuery)

	since := time.Now().Add(-xcheckInterruptWindow)
	var beadCount float64
	_, trunc, err := s.scanBeads(ctx, beadsapi.ListBeadsQuery{
		Types:        []string{"agent"},
		Statuses:     []string{"closed"},
		UpdatedAfter: since,
	}.OnlyFields("close_reason"), func(b *beadsapi.BeadDetail) {
		if _, counted := gardeniometrics.InvoluntaryDeathFamily(b.Fields["close_reason"]); counted {
			beadCount++
		}
	})
	if err != nil {
		return xcheckResult{}, fmt.Errorf("scanning closed agent beads: %w", err)
	}
	if trunc {
		s.logger.Warn("xcheck P9: closed-agent scan truncated, side B partial; pair skipped", "cap", xcheckScanMaxBeads)
		return xcheckResult{}, nil
	}
	// The deaths counter is created lazily on the first involuntary death, so an
	// absent side-A metric is ambiguous. Resolve it by side B:
	//   - beadCount == 0 and metric absent → clean window, nothing to corroborate;
	//     leave the pair absent rather than publishing a hollow 0/0.
	//   - beadCount  > 0 and metric absent → the very divergence P9 exists to
	//     catch (beads recorded involuntary deaths the counter never emitted);
	//     surface it by treating side A as a present 0 so agreement drops.
	if !okA {
		if beadCount == 0 {
			return xcheckResult{}, nil
		}
		s.logger.Warn("xcheck P9: involuntary-death metric absent but beads show deaths — surfacing divergence",
			"bead_involuntary_count", beadCount)
		return xcheckResult{a: 0, b: beadCount, haveA: true, haveB: true}, nil
	}
	return bothAvail(deathsMetric, beadCount), nil
}

// --- shared bead-scan pager -------------------------------------------------

// scanBeads pages through beads matching q and invokes fn for each, up to
// xcheckScanMaxBeads. It returns the count visited and whether the scan was
// truncated at the cap (the caller decides how to treat a partial — every
// current caller skips the pair). A page limit is forced so callers cannot
// accidentally pull unbounded pages. Every caller narrows q with OnlyFields to
// the keys its fn reads: P4 walks every agent and session bead ever made each
// cycle (gd-6PXLuR3juD).
func (s *xcheckSweep) scanBeads(ctx context.Context, q beadsapi.ListBeadsQuery, fn func(*beadsapi.BeadDetail)) (count int, truncated bool, err error) {
	q.Limit = xcheckScanPageLimit
	q.NoTotal = true // pages end on a short page; Total is never read
	for offset := 0; offset < xcheckScanMaxBeads; offset += xcheckScanPageLimit {
		q.Offset = offset
		res, lerr := s.daemon.ListBeadsFiltered(ctx, q)
		if lerr != nil {
			return count, false, lerr
		}
		if res == nil || len(res.Beads) == 0 {
			return count, false, nil
		}
		for _, b := range res.Beads {
			if b == nil {
				continue
			}
			fn(b)
			count++
		}
		if len(res.Beads) < xcheckScanPageLimit {
			// Last (short) page — the full population fit under the cap.
			return count, false, nil
		}
	}
	// Reached the cap with a full final page: more beads remain unscanned.
	return count, true, nil
}

// parseXCheckFloat parses a USD/number cost field, returning 0 for empty,
// invalid, or negative — mirroring the viewer's parseCostField and
// gardenerSpendSync's parser so every consumer sums the same figure a card shows.
func parseXCheckFloat(sv string) float64 {
	if sv == "" {
		return 0
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(sv), 64)
	if err != nil || f < 0 {
		return 0
	}
	return f
}
