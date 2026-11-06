package main

// Per-row enrichment for the /gardeners index: the actual last-run projection
// (IsRunning / inline TLDR / real outcome / completion time) and the inline
// needs-attention triage signal (ka-0000003950). Both run as bounded batches
// after the rows are projected from their gardener beads (loadGardenerRows).
//
// Inline needs-attention triage signal (ka-0000003950):
//
// On-call triage previously required drilling into /gardener/{id} to learn WHY a
// gardener needs attention. This surfaces that "why" inline on the index row —
// the gardener's open decision prompt, or its last run's blocker — GATED to the
// needs-attention slice so the dormant tail stays uncluttered.
//
// The candidate sources are discovered cheaply during the existing row load:
//   - attentionDecisionID is seeded in gardenerRowFromBead from the daemon's
//     BlockedOnDecision predicate (one opt-in column on the list query), then
//     overridden in applyLastRun by the last-run agent's own decision_bead_id.
//   - lastRunBlocker is the last run's wrap-up `blockers` text (applyLastRun).
//
// deriveAttentionSignal projects those into the exported Attention* chip fields,
// and enrichAttentionDecisionPrompts upgrades a decision chip's placeholder to
// the decision's real prompt via one bounded batch over just the attention rows.

import (
	"context"
	"strings"
	"sync"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// gardenerRunCountFanout bounds the concurrent iteration-count queries the
// zero-tally derivation issues. The zero-tally subset is small (the legacy
// spawn_agent / bare-trigger gardeners, ~21 today — bug ka-0000005661), so a
// modest fan-out drains it in one wave without flooding the daemon.
const gardenerRunCountFanout = 8

// enrichGardenerRunCounts repairs the Runs column for legacy spawn_agent /
// bare-trigger gardeners (bug ka-0000005661). Those gardeners carry no GardenerSpec
// `gardener` blob, so RunsSpawned/RunsCompleted decode to zero even when their
// life-thread holds dozens-to-hundreds of real runs — the index rendered "0/0"
// for ~21 healthy gardeners, making them look like they never ran.
//
// For the ZERO-TALLY subset only (rows where both spec counters are zero — a
// real engine tally is left untouched), this derives the count from the
// gardener's iteration agent beads via the daemon's CountOnly fast path
// (SELECT COUNT(*), no row bodies):
//
//   - spawned   = COUNT(agent beads owned by this gardener)
//   - completed = COUNT(those that are closed)
//
// Ownership is the same dual-key union gardenerIterations uses: an iteration
// carries the gardener id under trigger_id (bridge path) OR schedule_id (legacy
// scheduler path). The affected legacy set carries exactly ONE of the two keys
// per iteration (a bridge-trigger gardener's runs carry trigger_id; a legacy
// scheduler gardener's carry schedule_id), so the two key-counts never overlap.
// deriveGardenerRunCount probes the gardener's OWN primitive key first and stops
// the moment that key yields a run count, since the other key is then
// guaranteed zero for this single-keyed set — halving the round-trips the
// original MAX-of-both-keys loop issued.
//
// The work is bounded: only zero-tally rows are queried, fanned out at
// gardenerRunCountFanout. A query error leaves the row at 0/0 (the pre-fix
// display) rather than failing the page. Rows that genuinely have no iterations
// stay 0/0 and unflagged.
func (s *Server) enrichGardenerRunCounts(ctx context.Context, rows []gardenerRowView) {
	// Collect the indices of zero-tally rows that have an id to query.
	idxs := make([]int, 0)
	for i := range rows {
		if rows[i].RunsSpawned == 0 && rows[i].RunsCompleted == 0 && strings.TrimSpace(rows[i].ID) != "" {
			idxs = append(idxs, i)
		}
	}
	if len(idxs) == 0 {
		return
	}

	sem := make(chan struct{}, gardenerRunCountFanout)
	var wg sync.WaitGroup
	for _, i := range idxs {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			spawned, completed, ok := s.deriveGardenerRunCount(ctx, rows[i].ID, rows[i].Kind)
			if !ok || spawned == 0 {
				return // leave 0/0 — no findable iterations, or query failed
			}
			rows[i].RunsSpawned = spawned
			rows[i].RunsCompleted = completed
			rows[i].RunsDerived = true
			// A derived count is also the most honest source for the outcome
			// chip on these spec-less rows: a completed run means the loop has
			// closed at least one iteration. The engine-counter derivation
			// (gardenerLastRunOutcome) read 0 completed and returned "" — keep
			// that only when the real last-run enrichment hasn't already set a
			// truthful outcome. enrichGardenerLastRuns runs first and wins when it
			// has a last-agent bead; this just lifts the no-data "" to "ok" when
			// there is a closed iteration but no last-agent pointer.
			if rows[i].LastRunOutcome == "" && completed > 0 {
				rows[i].LastRunOutcome = "ok"
			}
		}(i)
	}
	wg.Wait()
}

// deriveGardenerRunCount counts a gardener's iteration agent beads via the daemon
// CountOnly fast path over the gardener's ownership key (trigger_id for a
// type=trigger primitive, schedule_id for a legacy type=schedule one), the way
// gardenerIterations does. Returns (spawned, completed, ok); ok is false only
// when every underlying count query errored — a partial result (one key served,
// one errored) still returns ok with the served key's counts so a single
// transient failure does not zero out a real count.
//
// Fan-out reduction (bug ka-0000008005): the legacy zero-tally set carries
// EXACTLY ONE of the two keys per iteration — trigger_id when the bridge trigger
// path spawned it, schedule_id when the legacy scheduler did — so the two
// key-counts never overlap. We probe the gardener's OWN primitive key first
// (kind ⇒ key) and stop the moment it yields runs; the other key is then
// guaranteed zero for this single-keyed set, so its 3 CountOnly probes are pure
// waste. Only when the primary key is genuinely empty (0 findable runs) do we
// fall back to the other key, as a defensive guard for a mis-keyed row. This
// takes the common case from 6 round-trips per row (both keys × total/closed/
// expired) down to 3.
func (s *Server) deriveGardenerRunCount(ctx context.Context, gardenerID, kind string) (spawned, completed int, ok bool) {
	anyOK := false
	for _, key := range runCountKeyOrder(kind) {
		total, terr := s.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
			Types:     []string{"agent"},
			Fields:    map[string]string{key: gardenerID},
			CountOnly: true,
		})
		if terr != nil {
			s.logger.Error("gardeners: derive run count (total)", "gardener", gardenerID, "key", key, "error", terr)
			continue
		}
		// The primary key served but found no iterations under it. Fall through
		// to the fallback key WITHOUT issuing this key's closed/expired probes —
		// they would count the same empty set. anyOK is already true (the total
		// query succeeded), so a gardener with genuinely zero runs still returns
		// ok=true and stays 0/0 rather than looking like a query failure.
		if total.Total == 0 {
			anyOK = true
			continue
		}
		closed, cerr := s.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
			Types:     []string{"agent"},
			Statuses:  []string{"closed"},
			Fields:    map[string]string{key: gardenerID},
			CountOnly: true,
		})
		if cerr != nil {
			s.logger.Error("gardeners: derive run count (closed)", "gardener", gardenerID, "key", key, "error", cerr)
			continue
		}
		// Never-ran spawn shells (agent_state="expired") are real type=agent
		// beads but NOT runs — the reconciler auto-closed them in the coop queue
		// before they claimed a pod (bug ka-0000006805). The Fields filter is
		// AND-equality with no "!=", so we count the expired shells separately
		// and subtract them from both the total and the closed tally (a shell is
		// always closed) so the index Runs column matches the detail page's
		// run-only IterationCount. A count error here leaves the shell count at
		// zero — the pre-fix (slightly inflated) behaviour — rather than
		// failing the whole row.
		expired, eerr := s.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
			Types:     []string{"agent"},
			Fields:    map[string]string{key: gardenerID, fieldAgentState: agentStateExpired},
			CountOnly: true,
		})
		expiredN := 0
		if eerr != nil {
			s.logger.Error("gardeners: derive run count (expired)", "gardener", gardenerID, "key", key, "error", eerr)
		} else {
			expiredN = expired.Total
		}
		anyOK = true
		// Subtract the never-ran shells (clamped at 0). This key owns the whole
		// single-keyed set, so its count IS the run count — no cross-key MAX is
		// needed once the owning key is found. Break so the fallback key's probes
		// never run.
		spawned = max(total.Total-expiredN, 0)
		completed = max(closed.Total-expiredN, 0)
		break
	}
	return spawned, completed, anyOK
}

// runCountKeyOrder returns the ownership-field probe order for a gardener of the
// given index Kind: its own primitive's key first, the other as a defensive
// fallback. A type=trigger gardener's iterations carry trigger_id (the bridge
// spawn path); a legacy type=schedule gardener's carry schedule_id. Probing the
// gardener's own key first lets deriveGardenerRunCount short-circuit after one key
// for the common single-keyed case (bug ka-0000008005).
func runCountKeyOrder(kind string) [2]string {
	if kind == "schedule" {
		return [2]string{fieldScheduleID, fieldTriggerID}
	}
	return [2]string{fieldTriggerID, fieldScheduleID}
}

// enrichGardenerLastRuns batch-fetches every row's last-agent bead
// (GardenerSpec.LastAgentID) in ONE daemon.BatchGetBeads call and derives the
// inline last-run projection from it: IsRunning, the one-line LastRunTLDR, the
// real LastRunOutcome (from the actual wrap-up, replacing the engine-counter
// chip), and LastCompletedAt (the run-centric secondary sort key). The gardener
// set is operator-bounded, so this is a single bounded batch regardless of how
// many runs each gardener has accumulated.
//
// Fallback chain (per row):
//   - empty LastAgentID, or the bead is missing from the batch result, or the
//     batch call fails entirely: the row keeps the engine-counter outcome chip
//     (gardenerLastRunOutcome, already set in gardenerRowFromBead) and sorts as a
//     no-completed-run row (zero LastCompletedAt). No inline TLDR.
//   - last-agent bead present + closed: IsRunning=false, real outcome + inline
//     TLDR, LastCompletedAt = the bead's UpdatedAt (completion time).
//   - last-agent bead present + not closed: IsRunning=true; the inline TLDR (if
//     any wrap-up exists yet) still shows, but LastCompletedAt stays zero so the
//     row sorts by the running-first key, not by completion.
func (s *Server) enrichGardenerLastRuns(ctx context.Context, rows []gardenerRowView, now time.Time) {
	ids := make([]string, 0, len(rows))
	seen := make(map[string]bool, len(rows))
	for i := range rows {
		if id := strings.TrimSpace(rows[i].lastAgentID); id != "" && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return
	}

	beads, err := s.daemon.BatchGetBeads(ctx, ids)
	if err != nil {
		// Degrade to the engine-counter outcome already on each row — the index
		// still renders, just without the inline last-run peek.
		s.logger.Error("gardeners: batch last-run beads", "error", err)
		return
	}

	for i := range rows {
		id := strings.TrimSpace(rows[i].lastAgentID)
		if id == "" {
			continue
		}
		b := beads[id]
		if b == nil {
			continue // bead pruned/unfetchable — keep the engine-counter fallback
		}
		s.applyLastRun(&rows[i], b, now)
	}
}

// applyLastRun overlays one gardener row with its last-agent bead's actual
// last-run result. Split out from enrichGardenerLastRuns so the projection (the
// IsRunning / TLDR / outcome / completion-time derivation) is unit-testable
// without a daemon.
func (s *Server) applyLastRun(row *gardenerRowView, b *beadsapi.BeadDetail, now time.Time) {
	closed := statusOpenSortKey(b.Status) != 0
	row.IsRunning = !closed
	row.lastActivityAt = b.UpdatedAt

	// Refine the overdue/silent liveness verdict (bug ka-0000006095 / A1) now
	// that we hold the last-agent bead: its last_progress_at is the gardener's
	// most recent genuine-progress heartbeat, the input the bead-surface seed in
	// gardenerRowFromBead lacked. Re-derive so the silent-gardener signal (still
	// spawning, progress frozen) can fire, and re-fold into NeedsAttention.
	if lp := parseRFC3339OrZero(strings.TrimSpace(b.Fields["last_progress_at"])); !lp.IsZero() {
		in := row.livenessInput
		in.LastProgress = lp
		row.Liveness = deriveGardenerLiveness(in, now)
		if row.Status == "active" && row.Liveness.NeedsAttention() {
			row.NeedsAttention = true
		}
	}

	// C3 (bug ka-0000001009): a live run's cost is not yet folded into the
	// gardener_spend_usd roll-up (total_cost_usd is written only at session
	// close), so the maintained figure UNDER-reports while a run is in flight.
	// Flag it so the chip can mark itself approximate + annotate "excludes the
	// in-flight run" — but only when there IS a rolled-up figure to qualify.
	if !closed && row.HasSpend {
		row.SpendExcludesLiveRun = true
	}

	// Live-agent overlay (maps2 v2, ka-0000000595). The last-agent bead IS the
	// gardener's currently-live agent when the run is in flight; project its
	// identity + liveness off this same bead so the /maps card can name and link
	// the working agent without a second daemon round-trip. Liveness reuses the
	// /agents predicate (agent_state ∈ agentStateAlive AND not stale) rather than
	// the bare !closed run status, so a wedged/closed shell never reads as "live".
	if name := strings.TrimSpace(b.Fields["agent"]); name != "" {
		row.LiveAgentName = name
	} else {
		row.LiveAgentName = strings.TrimSpace(b.Title)
	}
	if row.LiveAgentName != "" {
		row.LiveAgentHref = s.basePath + "/" + b.ID
		row.LiveAgentAlive = isAgentStateIn(b.Fields["agent_state"], agentStateAlive) && !isAgentStale(b, now)
	}

	// Live-context occupancy + turns (maps2 v2 cost/context chips, ka-0000001668;
	// turns triad, ka-0000001199). Reuse the /agents usage projection off this
	// same already-fetched bead — no extra fan-out, buildAgentUsageView is pure
	// over b.Fields. We take only the context + turns halves here; per-gardener
	// COST is the controller-maintained gardener_spend_usd roll-up
	// (applyGardenerSpend), not this single run's cost.
	//
	//   - CONTEXT stays gated on LiveAgentAlive here. buildAgentUsageView itself
	//     now returns a closed agent's FROZEN last reading (ka-0000003171) so the
	//     agent surfaces can show a completed run's rollup, but it flags that with
	//     ContextAtExit and the caller is expected to qualify the chip. The
	//     gardener card's mini-bar has no such qualifier, so rendering the frozen
	//     value here would read as a live occupancy — keep the explicit gate
	//     rather than inheriting a silently-unqualified figure.
	//   - TURNS survive after close: buildAgentUsageView's live→total fallback
	//     reads the advancing live_turns while alive and the authoritative
	//     total_request_count once closed, so the last-run round-trip count keeps
	//     showing on a finished gardener. Hence HasTurns is projected
	//     unconditionally from the same call.
	usage := buildAgentUsageView(b.Fields, row.LiveAgentAlive)
	if usage.HasContext && row.LiveAgentAlive {
		row.HasContext = true
		row.ContextLabel = usage.ContextLabel
		row.ContextPct = usage.ContextPct
		row.ContextTitle = usage.ContextTitle
	}
	if usage.HasTurns {
		row.HasTurns = true
		row.TurnsLabel = usage.TurnsLabel
	}

	wrapup := b.Fields["wrapup"]
	tldr, acc := parseWrapUpTLDRAndAccomplishments(wrapup)

	// Inline-attention sources (ka-0000003950). The last-run agent's own
	// decision_bead_id is the most precise "blocked on THIS decision" pointer —
	// prefer it over the gardener-bead predicate seeded in gardenerRowFromBead.
	// The wrap-up blocker is the fallback "why it needs attention" signal when
	// the gardener is not parked on a decision. Both are projected into the
	// rendered chip by deriveAttentionSignal, gated to NeedsAttention rows.
	if dec := strings.TrimSpace(b.Fields["decision_bead_id"]); dec != "" {
		row.attentionDecisionID = dec
	}
	row.lastRunBlocker = extractWrapupBlockers(wrapup)

	if closed {
		// Real last-run outcome from the wrap-up, replacing the engine-counter
		// chip. agentRunOutcome returns "ok" | "fail" | "unknown"; map "fail" to
		// the row's "failed" vocabulary and treat "unknown" (no wrap-up) as "ok"
		// — a clean close with no blockers reads as ok, matching the drawer's
		// RunOK glyph (gardener_invocations.go).
		switch agentRunOutcome(wrapup) {
		case "fail":
			row.LastRunOutcome = "failed"
		default:
			row.LastRunOutcome = "ok"
		}
		row.LastCompletedAt = b.UpdatedAt
		row.LastCompletedAgo = fmtTimeAgo(b.UpdatedAt, now)
		row.LastCompletedAtRFC = rfcTime(b.UpdatedAt)
	}

	// Inline one-line headline: prefer the wrap-up tldr, backstop with the
	// accomplishments line, then a derived outcome (MRs/beads/terminal phrase),
	// then the task title — mirroring invocationHeadline in the drawer so the
	// peek and the expanded view agree. Clamp to one line for the row.
	headline := invocationHeadline(
		tldr,
		acc,
		derivedRunOutcome(0, wrapUpBeadsClosedCount(wrapup), agentRunOutcome(wrapup)),
		b.Fields["task_title"],
	)
	row.LastRunTLDR = snippet(headline, gardenerInlineTLDRLimit)
}

// gardenerInlineTLDRLimit clamps the inline last-run headline to a single,
// glanceable line on the row. The full TLDR + accomplishments stay available in
// the expand drawer (gardener_invocations.go).
const gardenerInlineTLDRLimit = 140

// gardenerAttentionSignalLimit clamps the inline needs-attention one-liner to a
// single, glanceable line on the index row. The full decision prompt / blocker
// stays available on the /gardener/{id} detail page.
const gardenerAttentionSignalLimit = 120

// deriveAttentionSignal projects a row's discovered open-decision id and
// last-run blocker into the exported Attention* chip fields, GATED to the
// needs-attention slice (ka-0000003950). A non-attention row is left untouched
// so the dormant tail carries no inline noise.
//
// Precedence: an open DECISION (the operator has a concrete pick to make) wins
// over a blocker, since it is the more actionable triage signal. The decision
// id is surfaced as a link; the prompt text is best-effort (filled by
// enrichAttentionDecisionPrompts when the daemon can serve it, otherwise the
// chip shows a stable placeholder + the id). The blocker fallback shows the
// wrap-up text inline.
func deriveAttentionSignal(row *gardenerRowView) {
	if !row.NeedsAttention {
		return
	}
	if id := strings.TrimSpace(row.attentionDecisionID); id != "" {
		row.AttentionKind = "decision"
		row.AttentionDecisionID = id
		// Prompt text is enriched separately; default to a stable label so the
		// chip is never empty even before/without the prompt fetch.
		if row.AttentionSignal == "" {
			row.AttentionSignal = "blocked on an open decision"
		}
		return
	}
	if blk := strings.TrimSpace(row.lastRunBlocker); blk != "" {
		row.AttentionKind = "blocker"
		row.AttentionSignal = snippet(blk, gardenerAttentionSignalLimit)
	}
}

// enrichAttentionDecisionPrompts batch-fetches the decision beads pointed at by
// needs-attention rows, overlays each row's inline chip with the decision's real
// prompt text (ka-0000003950), and — critically — DOWNGRADES rows whose decision
// has already been resolved/dismissed back to their blocker fallback. It is a
// single bounded BatchGetBeads over only the attention slice's decision ids, so
// it adds at most one daemon round-trip regardless of fleet size. A failed/absent
// fetch leaves the stable "blocked on an open decision" placeholder
// deriveAttentionSignal already set, so the chip is never empty.
//
// Why the status check (ka-0000007231): the decision attention id has two
// sources. gardenerRowFromBead seeds it from the daemon's blocked_on_decision
// predicate, which is already status=open only (kbeads queries.go), so a closed
// decision never appears there. But applyLastRun OVERRIDES it from the last-run
// agent's raw fields.decision_bead_id, which is a plain pointer with no liveness
// check — when the agent yielded on a decision the operator later dismissed (or
// answered), that field still points at the now-closed decision. Without this
// guard the row renders "blocked on an open decision" linking to a dead
// decision. We mirror deriveAttentionSignal's precedence: a closed decision is
// no longer actionable, so fall back to the last-run blocker (or clear the chip).
func (s *Server) enrichAttentionDecisionPrompts(ctx context.Context, rows []gardenerRowView) {
	ids := make([]string, 0)
	seen := make(map[string]bool)
	for i := range rows {
		if rows[i].AttentionKind == "decision" && rows[i].AttentionDecisionID != "" && !seen[rows[i].AttentionDecisionID] {
			seen[rows[i].AttentionDecisionID] = true
			ids = append(ids, rows[i].AttentionDecisionID)
		}
	}
	if len(ids) == 0 {
		return
	}
	beads, err := s.daemon.BatchGetBeads(ctx, ids)
	if err != nil {
		s.logger.Error("gardeners: batch attention-decision beads", "error", err)
		return
	}
	for i := range rows {
		if rows[i].AttentionKind != "decision" {
			continue
		}
		b := beads[rows[i].AttentionDecisionID]
		if b == nil {
			continue // keep the placeholder
		}
		// A resolved/dismissed decision is no longer a live operator pick. Drop
		// the stale decision chip and fall back to the blocker signal so the row
		// stops linking to a dead decision (ka-0000007231).
		if statusOpenSortKey(b.Status) != 0 {
			downgradeDecisionToBlocker(&rows[i])
			continue
		}
		if prompt := strings.TrimSpace(b.Fields["prompt"]); prompt != "" {
			rows[i].AttentionSignal = snippet(prompt, gardenerAttentionSignalLimit)
		}
	}
}

// downgradeDecisionToBlocker clears a row's decision-attention chip and re-derives
// its inline signal from the last-run blocker (if any). Used when the decision a
// row was parked on has since closed, so the chip no longer advertises a stale
// "blocked on an open decision" link (ka-0000007231). Mirrors the blocker branch
// of deriveAttentionSignal.
func downgradeDecisionToBlocker(row *gardenerRowView) {
	row.AttentionDecisionID = ""
	if blk := strings.TrimSpace(row.lastRunBlocker); blk != "" {
		row.AttentionKind = "blocker"
		row.AttentionSignal = snippet(blk, gardenerAttentionSignalLimit)
		return
	}
	row.AttentionKind = ""
	row.AttentionSignal = ""
}

// enrichGardenerGoalStates re-derives each row's GoalState LIVE from its
// goal-target bead's CURRENT status (bug ka-0000007772), correcting the stale
// close-time snapshot that GoalState was seeded with in gardenerRowFromBead.
//
// The bug: the goal-achievement badge was computed once at gardener-close from
// the terminated slot/kind and never re-evaluated. When the target epic's
// status flipped afterward this produced FALSE-NEGATIVES ("goal not reached"
// though the epic CLOSED) and FALSE-POSITIVES ("GOAL ACHIEVED" though the epic
// is OPEN). The fix reads the target epic's OWN status as authoritative —
// never crediting "achieved" while the epic is itself open, regardless of any
// 0-subtree count (reversed parent-child edges can zero a subtree CTE while the
// epic is open; the ka-0000000398 data bug).
//
// Cost: ONE batched BatchGetBeads over the distinct target ids across all rows
// (no N+1) — the gardener set is operator-bounded (capped at gardenersListLimit).
// Rows with no resolvable target, or whose target fetch misses, keep the
// snapshot GoalState already set, so a daemon hiccup never flips a badge to a
// wrong-but-confident verdict.
func (s *Server) enrichGardenerGoalStates(ctx context.Context, rows []gardenerRowView) {
	ids := make([]string, 0, len(rows))
	seen := make(map[string]bool, len(rows))
	for i := range rows {
		if id := strings.TrimSpace(rows[i].targetEpicID); id != "" && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return
	}
	beads, err := s.daemon.BatchGetBeads(ctx, ids)
	if err != nil {
		// Degrade to the snapshot GoalState already on each row — the index
		// still renders, just without the live re-derive.
		s.logger.Error("gardeners: batch goal-target beads", "error", err)
		return
	}
	for i := range rows {
		id := strings.TrimSpace(rows[i].targetEpicID)
		if id == "" {
			continue
		}
		tb := beads[id]
		if tb == nil {
			continue // target pruned/unfetchable — keep the snapshot fallback
		}
		if live, ok := gardenerLiveGoalState(tb.Status, rows[i].IsClosed); ok {
			rows[i].GoalState = live
		}
	}
}
