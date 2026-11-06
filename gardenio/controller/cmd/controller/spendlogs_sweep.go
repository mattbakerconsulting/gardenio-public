package main

import (
	"context"
	"log/slog"
	"strconv"
	"time"

	"gardenio/controller/internal/beadsapi"
	gardeniometrics "gardenio/controller/internal/metrics"
	"gardenio/controller/internal/spendlogs"
)

// spendLogsCostField is the agent-bead field carrying the NEW, stateless
// per-agent cost (C3, plan ka-0000003747 / ka-0000000774, operator decision
// ka-0000006903 "replace-now"): the agent's lifetime spend recomputed each
// sweep directly from the LiteLLM /spend/logs request rows — no spawn
// baseline, no settlement watermark, re-derivable after the fact. Written
// ALONGSIDE total_cost_usd (the old stateful path's figure) during the
// transition; the P10 cross-check pair (xcheck_sweep.go) compares the two and
// is the safety gate C4 watches before cutting consumers over and deleting
// the baseline/watermark machinery.
const spendLogsCostField = "spend_logs_cost_usd"

// Spend-logs sweep tuning.
const (
	// spendLogsSweepInterval is the default recompute cadence (config
	// SpendLogsSweepInterval, env GARDENIO_SPENDLOGS_SWEEP_INTERVAL; 0
	// disables). A full-window /spend/logs sweep is heavy relative to the
	// other pollers (~130 pages / ~100 MB per fleet-day of rows at probe-time
	// volume, ka-0000001899), so this runs on a slower cadence than the
	// 5-minute gauge pollers; the freshness bar is "trend the old-vs-new
	// parity over days", not live display.
	spendLogsSweepInterval = 30 * time.Minute

	// spendLogsSweepLookback is the default projection window length (config
	// SpendLogsSweepLookback, env GARDENIO_SPENDLOGS_SWEEP_LOOKBACK). Only
	// agent beads CREATED inside the window are attributed — that is the
	// population whose whole life the projection covers, so the recomputed
	// figure is a true lifetime total rather than a silent partial sum. 48h
	// covers virtually every crew agent (typical life < 2h) and matches the
	// settlement sweep's recovery window; long-lived respawning agents
	// (gardeners) age past it AND past /spend/logs row retention, so they stay
	// on the old path until C4 decides their windowed semantics.
	spendLogsSweepLookback = 48 * time.Hour

	// spendLogsFlushLag is how far the window's TO edge trails now. LiteLLM's
	// spend writer flushes rows in batches with up to ~a minute of lag
	// (internal/spendlogs package doc), so a window ending at now would
	// systematically miss the freshest rows; they are picked up by the next
	// sweep instead.
	spendLogsFlushLag = 2 * time.Minute

	// spendLogsListPageLimit bounds each daemon list page when enumerating
	// tracked agent beads; spendLogsMaxTrackedBeads caps the total walked.
	// Unlike the xcheck scans, a truncated population here is per-agent safe
	// (each agent's figure is independent), so the sweep processes what it
	// fetched and logs the truncation instead of skipping the cycle.
	spendLogsListPageLimit   = 500
	spendLogsMaxTrackedBeads = 20000

	// spendLogsWriteEpsilon suppresses no-op bead PATCHes: the field is only
	// rewritten when it moved by more than this (sub-cent) amount, so an idle
	// agent's bead is not touched every cycle.
	spendLogsWriteEpsilon = 1e-6
)

// spendLogsSweep is the NEW primary per-agent attribution path (C3): a
// periodic, stateless recompute of per-agent cost from LiteLLM /spend/logs,
// replacing — after the C4 cutover — the stateful spawn-baseline/close-delta/
// watermark pipeline in reconcile_session_spend.go. Each cycle it:
//
//  1. Lists the agent beads created inside the lookback window — the tracked
//     population, exactly the beads whose whole life the projection window
//     covers (enforced again client-side; a bead predating the window is
//     SKIPPED, never booked a partial figure).
//  2. Runs one spendlogs.Project over the window across every LiteLLM proxy —
//     /spend/logs has no server-side tag filter, so one agent costs the same
//     as the whole fleet; project once, read many (ka-0000001899).
//  3. Publishes gardenio_agent_spend_logs_usd{agent,project,role,model,account}
//     for every tracked agent with in-window rows (Reset+Set: series age out
//     with their agents) and writes the same figure onto the agent bead's
//     spend_logs_cost_usd field alongside the old path's total_cost_usd.
//
// The old stateful path keeps running untouched in this step; P10 in the
// x-check sweep compares the two per-agent figures and gates the C4 cutover.
//
// What this fixes over the stateful path (the fragile states from decision
// ka-0000006903): a missed spawn baseline no longer unattributes the session
// (there is no baseline); a never-closed session's spend is visible while the
// agent is still running (no close event needed); and a lost write self-heals
// on the next cycle because every cycle re-derives from the source of truth.
type spendLogsSweep struct {
	daemon *beadsapi.Client
	client *spendlogs.Client
	// globalURL is the controller-global LiteLLM URL; the swept proxy set is
	// this plus every account-bead base_url (spendlogs.ProxyURLs).
	globalURL string
	metrics   *gardeniometrics.SpendMetrics
	// resolveLabels supplies the {model,account} labels the agent bead does
	// not carry — the SAME resolver the old counter uses (main.go), so old
	// and new series for one agent land on identical label sets and can be
	// overlaid directly. Optional; nil resolves both to "".
	resolveLabels spendLabelResolver
	logger        *slog.Logger

	interval time.Duration
	lookback time.Duration

	// now is injectable so tests pin the projection window.
	now func() time.Time
}

func newSpendLogsSweep(daemon *beadsapi.Client, client *spendlogs.Client, globalURL string, metrics *gardeniometrics.SpendMetrics, interval, lookback time.Duration, logger *slog.Logger) *spendLogsSweep {
	if interval <= 0 {
		interval = spendLogsSweepInterval
	}
	if lookback <= 0 {
		lookback = spendLogsSweepLookback
	}
	return &spendLogsSweep{
		daemon:    daemon,
		client:    client,
		globalURL: globalURL,
		metrics:   metrics,
		logger:    logger.With("component", "spendlogs-sweep"),
		interval:  interval,
		lookback:  lookback,
		now:       time.Now,
	}
}

// RunLoop recomputes the projection every interval until ctx is cancelled,
// with one immediate pass on start.
func (s *spendLogsSweep) RunLoop(ctx context.Context) {
	s.logger.Info("spend-logs sweep starting (stateless per-agent attribution, C3)",
		"interval", s.interval, "lookback", s.lookback)
	s.syncOnce(ctx)

	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			s.syncOnce(ctx)
		case <-ctx.Done():
			return
		}
	}
}

// syncOnce runs one full projection cycle. On a projection or listing error
// it returns WITHOUT touching the gauge — the previous cycle's series stay up
// (one stale cycle beats a spurious fleet-wide dip that would dent every
// increase() window), and the bead fields are simply a cycle older.
func (s *spendLogsSweep) syncOnce(ctx context.Context) {
	to := s.now().Add(-spendLogsFlushLag)
	from := to.Add(-s.lookback)

	tracked, truncated, err := s.trackedAgentBeads(ctx, from)
	if err != nil {
		s.logger.Warn("spend-logs sweep: listing tracked agent beads failed; keeping previous series", "error", err)
		return
	}
	if truncated {
		// Per-agent figures are independent, so a truncated population still
		// yields correct values for the beads fetched — process them, but say
		// so (the beads beyond the cap keep their previous series absent).
		s.logger.Warn("spend-logs sweep: tracked population truncated at cap; beads beyond it are not attributed this cycle",
			"cap", spendLogsMaxTrackedBeads)
	}
	if len(tracked) == 0 {
		// No agents born inside the window — an honest empty snapshot.
		s.metrics.SetSpendLogsCosts(nil)
		s.logger.Debug("spend-logs sweep: no tracked agent beads in window", "from", from, "to", to)
		return
	}

	proxies := spendlogs.ProxyURLs(ctx, s.daemon, s.globalURL)
	proj, err := s.client.Project(ctx, proxies, from, to)
	if err != nil {
		s.logger.Warn("spend-logs sweep: projection failed; keeping previous series", "error", err)
		return
	}

	// The projection attributes by the agent: tag, which carries only the
	// NAME. Two tracked beads sharing one name (observed live: a respawn
	// pattern re-minting a bead under an unchanged pod name) are
	// indistinguishable in the rows — booking the per-name sum to each would
	// double-count it, in both the gauge and P10's side B. A collided name is
	// ambiguous, so every bead carrying it is skipped this cycle (logged);
	// this is the name-reuse contamination class the C4 verification list
	// tracks, surfaced instead of silently double-booked.
	nameCount := make(map[string]int, len(tracked))
	for _, agent := range tracked {
		if agent.name != "" {
			nameCount[agent.name]++
		}
	}

	costs := make([]gardeniometrics.SpendLogsCost, 0, len(tracked))
	attributed, fieldWrites := 0, 0
	for _, agent := range tracked {
		agentName := agent.name
		if agentName == "" {
			continue
		}
		if nameCount[agentName] > 1 {
			s.logger.Warn("spend-logs sweep: agent name shared by multiple tracked beads; skipping (ambiguous attribution)",
				"agent", agentName, "beads", nameCount[agentName])
			continue
		}
		// Re-check coverage client-side: the daemon query already filtered on
		// created_after, but a lenient daemon (or clock skew at the boundary)
		// must not slip a partially-covered bead into a "lifetime" figure.
		if agent.createdAt.Before(from) {
			continue
		}
		usd := proj.AgentUSD(agentName)
		if usd <= 0 {
			// No billable in-window rows (yet). Leave both the series and the
			// field absent — "not derived" — rather than booking a 0 the old
			// path would never write either.
			continue
		}
		model, account := "", ""
		if s.resolveLabels != nil {
			model, account = s.resolveLabels(agent.project, agent.role)
		}
		costs = append(costs, gardeniometrics.SpendLogsCost{
			Agent:   agentName,
			Project: agent.project,
			Role:    agent.role,
			Model:   model,
			Account: account,
			USD:     usd,
		})
		attributed++
		if s.writeBeadField(ctx, agent, usd) {
			fieldWrites++
		}
	}
	s.metrics.SetSpendLogsCosts(costs)
	s.logger.Debug("spend-logs sweep complete",
		"tracked", len(tracked), "attributed", attributed, "field_writes", fieldWrites,
		"window_total_usd", proj.TotalUSD, "requests", proj.Requests, "proxies", len(proj.Proxies))
}

// writeBeadField books the recomputed lifetime figure onto the agent bead's
// spend_logs_cost_usd field, skipping the PATCH when the stored value already
// matches (idle agents' beads are not churned every cycle). Best-effort: a
// failed write is logged and self-heals next cycle — every cycle re-derives
// the full figure from the source of truth, which is the whole point of the
// stateless path. Returns true when a write was issued.
func (s *spendLogsSweep) writeBeadField(ctx context.Context, agent trackedAgent, usd float64) bool {
	if prev, err := strconv.ParseFloat(agent.prevCost, 64); err == nil {
		delta := usd - prev
		if delta < spendLogsWriteEpsilon && delta > -spendLogsWriteEpsilon {
			return false
		}
	}
	if err := s.daemon.UpdateBeadFieldsTyped(ctx, agent.id, map[string]any{spendLogsCostField: usd}); err != nil {
		s.logger.Warn("spend-logs sweep: bead field write failed",
			"agent", agent.name, "bead", agent.id, "error", err)
		return false
	}
	return true
}

// trackedAgent is the fixed-size projection of an agent bead the sweep actually
// consumes: the bead id, its creation time, the four label/name fields, and the
// previously-stored cost (for no-op-write suppression). A full BeadDetail is
// ~7.7 KiB — dominated by the agent's spawn prompt carried in Fields — while
// the sweep only ever reads these six values, so reducing each page to this
// struct and dropping the BeadDetail rows is what keeps peak memory to one page
// instead of the whole 48h window (the fetch-many/use-one fix for ka-0000001774,
// the structural twin of the fleet-archetype OOM ka-0000006669).
type trackedAgent struct {
	id        string
	name      string
	project   string
	role      string
	createdAt time.Time
	prevCost  string
}

// spendLogsSweepMaxPages caps the paged walk so a daemon that ignores offset
// (or a fleet that grows without bound) cannot spin forever. spendLogsMaxTracked
// Beads / spendLogsListPageLimit pages, matching the old accumulate-to-cap bound.
const spendLogsSweepMaxPages = spendLogsMaxTrackedBeads / spendLogsListPageLimit

// trackedAgentBeads pages through the agent beads created at or after
// createdAfter — any status: a closed bead keeps refreshing to its final
// figure until it ages out of the window, which is exactly what P10 compares
// against its settled total_cost_usd. Each page is folded into the compact
// trackedAgent slice and the heavyweight BeadDetail rows are dropped before the
// next fetch, so peak memory is one page (~500 beads) rather than the whole
// window — the fix for ka-0000001774 (the old loop appended every full page into
// one slice, making the paging cosmetic and the ceiling ~150 MiB at the cap).
// Returns the reduced agents, whether the walk was truncated at
// spendLogsMaxTrackedBeads, and any listing error.
func (s *spendLogsSweep) trackedAgentBeads(ctx context.Context, createdAfter time.Time) ([]trackedAgent, bool, error) {
	var out []trackedAgent
	q := beadsapi.ListBeadsQuery{
		Types:        []string{"agent"},
		CreatedAfter: createdAfter,
		// created_at is immutable, so it is the only sort that gives a stable
		// page sequence while new agent beads are minted underneath the walk;
		// duplicates across a page boundary are harmless — a per-name collision
		// only skips the name, never double-books it (see syncOnce).
		Sort:    "created_at",
		Limit:   spendLogsListPageLimit,
		NoTotal: true, // pages end on a short page; Total is never read
	}.OnlyFields("agent", "project", "role", spendLogsCostField)
	for page := range spendLogsSweepMaxPages {
		q.Offset = page * spendLogsListPageLimit
		res, err := s.daemon.ListBeadsFiltered(ctx, q)
		if err != nil {
			return nil, false, err
		}
		if res == nil || len(res.Beads) == 0 {
			return out, false, nil
		}
		// Reduce this page to the fixed-size projection and let the GC reclaim
		// the full BeadDetail rows before the next fetch — never hold more than
		// one page of them.
		for _, b := range res.Beads {
			if b == nil {
				continue
			}
			out = append(out, trackedAgent{
				id:        b.ID,
				name:      b.Fields["agent"],
				project:   b.Fields["project"],
				role:      b.Fields["role"],
				createdAt: b.CreatedAt,
				prevCost:  b.Fields[spendLogsCostField],
			})
		}
		if len(res.Beads) < spendLogsListPageLimit {
			return out, false, nil
		}
	}
	return out, true, nil
}
