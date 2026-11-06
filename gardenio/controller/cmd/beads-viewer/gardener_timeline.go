package main

import (
	"fmt"
	"sort"
	"strconv"

	"gardenio/controller/internal/beadsapi"
)

// Gardener life-thread timeline (epic ka-0000001579, Q4 / ka-0000004816).
//
// A gardener (a type=trigger bead carrying trigger:schedule + role:gardener)
// spawns one agent bead per iteration. stampGardenerLoopContext
// (internal/bridge/triggers.go) stamps each iteration bead with:
//
//	schedule_id              = the gardener trigger bead's ID (the FK)
//	schedule_prior_agent_id  = the PREVIOUS iteration's agent bead ID
//	schedule_runs_spawned    = a snapshot counter at spawn time
//	schedule_max_runs        = the gardener's MaxRuns budget
//	schedule_terminal_goal   = the gardener's terminal goal (per-run snapshot)
//
// This file reconstructs the ordered "life-thread" of those iterations.
//
// Ordering authority (design doc ka-0000003219 §5; trigger-path tier ka-0000006929):
//
//  1. PRIMARY — the schedule_prior_agent_id CHAIN. It is the gardener's own
//     causal record: each iteration points back at its predecessor. We index
//     iterations by ID, pick the head (the gardener's LastAgentID, or the run
//     nothing points at), walk schedule_prior_agent_id backward to the root,
//     then number root-forward 1..N.
//  2. SECONDARY — the schedule_key FIRE-SLOT order. Bridge trigger-path
//     gardeners (a plain role:gardener cron trigger with no GardenerSpec block)
//     never run stampGardenerLoopContext, so they carry NO
//     schedule_prior_agent_id chain — only a per-fire schedule_key (a
//     minute-granularity fire-slot like "2026-06-02T07:11", stamped at spawn
//     by trigger_source_schedule.go). That key IS a genuine, monotonic,
//     lexically-sortable run order. When the causal chain is unavailable or
//     broken but EVERY iteration carries a schedule_key, we sort by it and
//     treat the result as a real order — the run numbers are trustworthy, just
//     sourced from the fire schedule rather than the back-pointer chain. These
//     inferred cards are labeled "(by fire order)", distinct from the heuristic
//     "(by time)" reserved for the created_at fallback (design doc §2.2/§8).
//  3. FALLBACK — created_at ascending. Both the chain and the schedule_key
//     order can be unavailable (a missed run, a manually edited/deleted bead, a
//     gardener restart that resets LastAgentID, or a mixed set where some
//     iterations lack schedule_key). On that fallback we sort the WHOLE set by
//     created_at ascending, number sequentially, and label every affected card
//     "(by time)" so operators know the order is heuristic.
//
// schedule_runs_spawned is DISPLAY-ONLY ("of N" enrichment) and is never the
// sort key: it is a spawn-time snapshot that resets/duplicates across gardener
// restarts.
//
// The latest iteration is usually still running (status != closed). It renders
// at the top of the (newest-first) thread, marked IsLive, with the label
// "Iteration N (in progress)" — it does not depend on a final run count.

// gardenerIterationView is one run in a gardener's life-thread. It EMBEDS
// agentRowView so each card renders identically to an /agents card, then
// overlays the schedule_* context stamped by stampGardenerLoopContext plus the
// derived run number / label.
//
// NOTE: this is the shared iteration view-model for the Gardener Page epic. The
// gardener header view-model (gardenerView) and its page handler land in the
// sibling children (ka-0000005461 / ka-0000001207); they build on this struct.
type gardenerIterationView struct {
	agentRowView // by value: ID, Name, Role, Status, Uptime, WrapUpSummary, ...

	// RunNumber is the canonical 1-based position in the gardener's life,
	// computed root-forward from the schedule_prior_agent_id chain (or from
	// created_at order when the chain breaks). Stable per card regardless of
	// thread display order.
	RunNumber int

	// RunsSpawned is the schedule_runs_spawned snapshot at spawn (display
	// only; never used for ordering).
	RunsSpawned int
	// MaxRuns is the schedule_max_runs budget snapshot ("of N" in the label).
	MaxRuns int
	// PriorAgentID is schedule_prior_agent_id (the chain back-pointer).
	PriorAgentID string
	// TerminalGoal is schedule_terminal_goal (per-run snapshot of the goal).
	TerminalGoal string
	// ScheduleKey is schedule_key — the per-fire slot stamped on trigger-path
	// spawns (e.g. "2026-06-02T07:11"). It is the primary component of the
	// stateless `/runs` pagination cursor (design doc §7/§8): the older-runs
	// tail is paged in bounded chunks keyed on (schedule_key, id) with no
	// server-side state. May be empty on legacy iterations — the cursor then
	// resolves by exact bead-id position (with id as the resync tie-break), so
	// paging stays correct even without a fire-slot key.
	ScheduleKey string

	// IsLive is true when the iteration is not closed — render an
	// "in progress" badge instead of a fixed "of N".
	IsLive bool

	// DidNotRun is true when this iteration bead is a SPAWN SHELL that never
	// actually ran: the reconciler auto-closed it with agent_state="expired"
	// because it sat in the coop queue past CoopQueueExpiryTimeout and never
	// claimed a pod (reconciler.expireDeferredBeads). It is a real type=agent
	// bead — so it survives the type filter and would otherwise be counted as a
	// run — but it has no session, no tool calls, and no wrap-up. It is NOT a
	// run (bug ka-0000006805: "Iterations N" must not count never-ran spawns),
	// so the count helpers (runIterationCount) exclude it and the card renders a
	// muted "did not run" ribbon instead of an outcome. Distinct from IsLive: a
	// did-not-run shell is always closed.
	DidNotRun bool

	// Basis records HOW this card's run order was derived — the causal chain
	// (genuine, no ribbon hint), the schedule_key fire-slot (inferred but real,
	// "by fire order"), or the created_at fallback (heuristic, "(by time)").
	// Per design doc §2.2/§8 this is carried PER ITERATION so a single broken
	// edge no longer stamps the whole thread "(by time)".
	Basis orderBasis
	// ByTime is true only when this card fell all the way to the created_at
	// heuristic (Basis == basisTime). Retained as the "ordering is NOT genuine"
	// discriminator for the template CSS hook and existing call sites; a
	// fire-slot ordering is genuine, so it is ByTime == false.
	ByTime bool
	// RunLabel is the human ribbon, e.g. "Iteration 3 of 10", "Iteration 7
	// (in progress)", "Iteration 3 (by fire order)", "Iteration 3 of 10 (by time)".
	RunLabel string
}

// ByFireOrder reports whether this card's order was recovered from the
// schedule_key fire-slot tier (basisFireKey). Template hook: fire-slot cards get
// a distinct, non-alarming ribbon style and tooltip — the order is genuine, just
// inferred from the fire schedule rather than the causal chain.
func (v gardenerIterationView) ByFireOrder() bool { return v.Basis == basisFireKey }

// orderBasis enumerates the authority that produced an iteration's run order.
// It is the per-iteration replacement for the old whole-thread byTime bool
// (design doc §2.2/§8): one broken chain edge must not relabel every card.
type orderBasis int

const (
	// basisChain — the schedule_prior_agent_id causal chain. Genuine order; no
	// ribbon hint.
	basisChain orderBasis = iota
	// basisFireKey — the schedule_key fire-slot order (trigger-path gardeners
	// with no chain). Inferred from the fire schedule but a real run order, so
	// labeled "(by fire order)" rather than the heuristic "(by time)".
	basisFireKey
	// basisTime — the created_at last-resort heuristic. Labeled "(by time)".
	basisTime
)

// Stamped field keys (must match stampGardenerLoopContext in
// internal/bridge/triggers.go).
const (
	fieldSchedulePriorAgentID = "schedule_prior_agent_id"
	fieldScheduleRunsSpawned  = "schedule_runs_spawned"
	fieldScheduleMaxRuns      = "schedule_max_runs"
	fieldScheduleTerminalGoal = "schedule_terminal_goal"

	// Owner back-pointers stamped on a gardener's spawned agent iterations. The
	// two spawn paths diverge: the bridge trigger path stamps trigger_id (the
	// trigger bead's own ID — see actionSpawnAgent extraFields in
	// internal/bridge/triggers.go), the legacy scheduler path stamps
	// schedule_id. gardenerIterations must match on BOTH, mirroring the dual-key
	// ownership check in reconciler.closeOrphanedMirrorTask
	// (internal/reconciler/reconciler.go).
	fieldTriggerID  = "trigger_id"
	fieldScheduleID = "schedule_id"

	// fieldScheduleKey is the per-fire match key stamped on every schedule
	// trigger-path spawn (triggerSourceFieldKey("schedule") in
	// internal/bridge/triggers.go). Its value is a minute-granularity fire
	// slot, e.g. "2026-06-02T07:11" (trigger_source_schedule.go fireKey). It is
	// the SECONDARY ordering authority for gardeners that carry no
	// schedule_prior_agent_id chain.
	fieldScheduleKey = "schedule_key"

	// fieldAgentState is the agent lifecycle enum stamped on every agent bead
	// (bridge/init.go: idle|spawning|working|done|failed|rate_limited|
	// budget_exceeded, plus "yielding" from the scheduler and "expired" written
	// by the reconciler). We read it to detect never-ran spawn shells.
	fieldAgentState = "agent_state"

	// agentStateExpired is the agent_state the reconciler stamps when it
	// auto-closes a queued spawn that exceeded CoopQueueExpiryTimeout before
	// ever claiming a pod (reconciler.expireDeferredBeads, reconciler.go). Such
	// a bead is a real type=agent record but represents a spawn that NEVER RAN —
	// no session, no tool calls, no wrap-up. It must not be counted as a run
	// (bug ka-0000006805).
	agentStateExpired = "expired"
)

// iterationDidNotRun reports whether an iteration agent bead is a never-ran
// spawn SHELL rather than a real run. The signature is the reconciler's expiry
// stamp: a closed agent bead carrying agent_state="expired" (queued past the
// coop expiry timeout, auto-closed before it ever claimed a pod —
// reconciler.expireDeferredBeads). These beads survive the type=agent filter
// the count paths use, so without this discriminator a gardener that merely had
// a few spawns expire in the queue shows an inflated "Iterations N" (bug
// ka-0000006805). A live/working/done bead is a real run and returns false.
func iterationDidNotRun(b *beadsapi.BeadDetail) bool {
	if b == nil {
		return false
	}
	return b.Fields[fieldAgentState] == agentStateExpired
}

// orderGardenerIterations reconstructs and numbers a gardener's life-thread from
// its raw iteration beads. lastAgentID is the gardener's GardenerSpec.LastAgentID
// (the engine's pointer at the most recent run; may be empty or stale). build
// turns one iteration bead into its agent-card view-model — injected so the
// ordering logic stays a pure, Server-free, testable function (the production
// caller passes s.agentRowFromBeadCtx).
//
// The returned slice is NEWEST-FIRST (highest RunNumber, i.e. the live/last
// run, on top), as operators care about the current run first.
func orderGardenerIterations(beads []*beadsapi.BeadDetail, lastAgentID string, build func(*beadsapi.BeadDetail) agentRowView) []gardenerIterationView {
	if len(beads) == 0 {
		return nil
	}

	byID := make(map[string]*beadsapi.BeadDetail, len(beads))
	for _, b := range beads {
		if b == nil || b.ID == "" {
			continue
		}
		// Last write wins on duplicate IDs (defensive; the daemon returns a set).
		byID[b.ID] = b
	}

	ordered, basis := chainOrder(byID, lastAgentID)

	out := make([]gardenerIterationView, 0, len(ordered))
	for i, b := range ordered {
		row := build(b)
		iter := gardenerIterationView{
			agentRowView: row,
			RunNumber:    i + 1, // root-forward 1..N
			RunsSpawned:  parseScheduleInt(b.Fields, fieldScheduleRunsSpawned),
			MaxRuns:      parseScheduleInt(b.Fields, fieldScheduleMaxRuns),
			PriorAgentID: b.Fields[fieldSchedulePriorAgentID],
			TerminalGoal: b.Fields[fieldScheduleTerminalGoal],
			ScheduleKey:  b.Fields[fieldScheduleKey],
			IsLive:       b.Status != "closed",
			DidNotRun:    iterationDidNotRun(b),
			Basis:        basis,
			ByTime:       basis == basisTime,
		}
		iter.RunLabel = runLabel(iter.RunNumber, iter.MaxRuns, iter.IsLive, iter.Basis)
		out = append(out, iter)
	}

	// Newest-first: highest RunNumber (the live/last run) on top.
	for l, r := 0, len(out)-1; l < r; l, r = l+1, r-1 {
		out[l], out[r] = out[r], out[l]
	}
	return out
}

// chainOrder returns the iteration beads ordered ROOT-FIRST (run 1 .. run N),
// plus the orderBasis tier the order was derived from: basisChain (the causal
// schedule_prior_agent_id chain), basisFireKey (the schedule_key fire-slot
// recovery), or basisTime (the created_at heuristic last resort).
func chainOrder(byID map[string]*beadsapi.BeadDetail, lastAgentID string) (ordered []*beadsapi.BeadDetail, basis orderBasis) {
	size := len(byID)
	if size == 0 {
		return nil, basisChain
	}

	// referenced[id] is true when some iteration's prior points at id, i.e. id
	// is NOT the latest run. The head is the run nothing points at.
	referenced := make(map[string]bool, size)
	dangling := false
	for _, b := range byID {
		prior := b.Fields[fieldSchedulePriorAgentID]
		if prior == "" {
			continue
		}
		if _, ok := byID[prior]; ok {
			referenced[prior] = true
		} else {
			// A non-empty prior pointing outside the set means the real
			// predecessor is missing — the causal chain is broken.
			dangling = true
		}
	}

	var head string
	switch {
	case lastAgentID != "" && byID[lastAgentID] != nil && !referenced[lastAgentID]:
		// The engine's pointer is present and is a genuine head (nothing
		// points at it) — trust it.
		head = lastAgentID
	default:
		// Otherwise the head is the unique run with no incoming chain edge.
		for id := range byID {
			if !referenced[id] {
				if head != "" {
					// More than one head ⇒ a fork/break; no unique chain.
					goto fallback
				}
				head = id
			}
		}
	}

	if head == "" || dangling {
		goto fallback
	}

	// Walk schedule_prior_agent_id backward from the head to the root.
	{
		seen := make(map[string]bool, size)
		walk := make([]*beadsapi.BeadDetail, 0, size)
		cur := head
		for {
			b := byID[cur]
			if b == nil || seen[cur] {
				// Missing node or a cycle ⇒ broken chain.
				goto fallback
			}
			seen[cur] = true
			walk = append(walk, b)
			prior := b.Fields[fieldSchedulePriorAgentID]
			if prior == "" {
				break // reached the root
			}
			if _, ok := byID[prior]; !ok {
				// Dangling predecessor ⇒ broken chain.
				goto fallback
			}
			cur = prior
		}
		if len(walk) != size {
			// The chain didn't cover every iteration (a fork or an
			// off-chain run) ⇒ fall back.
			goto fallback
		}
		// walk is head→root; reverse to root→head so run numbers go 1..N.
		for l, r := 0, len(walk)-1; l < r; l, r = l+1, r-1 {
			walk[l], walk[r] = walk[r], walk[l]
		}
		return walk, basisChain
	}

fallback:
	all := make([]*beadsapi.BeadDetail, 0, size)
	for _, b := range byID {
		all = append(all, b)
	}

	// SECONDARY tier — schedule_key fire-slot order. Trigger-path gardeners
	// carry no prior-agent chain but every iteration carries a schedule_key
	// (a monotonic per-fire slot). If EVERY iteration has one, that order is
	// genuine (not heuristic), so sort by it and report basisFireKey. A single
	// missing key means we cannot trust the fire-slot order over the whole set,
	// so we drop to the created_at heuristic below.
	allHaveScheduleKey := true
	for _, b := range all {
		if b.Fields[fieldScheduleKey] == "" {
			allHaveScheduleKey = false
			break
		}
	}
	if allHaveScheduleKey {
		// Lexical sort is correct: fireKey is a fixed-width RFC3339-prefix
		// ("2006-01-02T15:04") so byte order == chronological order. Ties
		// (two fires in the same minute) break by created_at, then ID, for a
		// deterministic result.
		sort.SliceStable(all, func(i, j int) bool {
			ki, kj := all[i].Fields[fieldScheduleKey], all[j].Fields[fieldScheduleKey]
			if ki != kj {
				return ki < kj
			}
			ti, tj := all[i].CreatedAt, all[j].CreatedAt
			if !ti.Equal(tj) {
				return ti.Before(tj)
			}
			return all[i].ID < all[j].ID
		})
		return all, basisFireKey
	}

	// TERTIARY tier — created_at ascending over the whole set; ties broken by
	// ID for a deterministic order. This is the heuristic last resort, so the
	// cards are marked "(by time)".
	sort.SliceStable(all, func(i, j int) bool {
		ti, tj := all[i].CreatedAt, all[j].CreatedAt
		if ti.Equal(tj) {
			return all[i].ID < all[j].ID
		}
		return ti.Before(tj)
	})
	return all, basisTime
}

// runLabel composes the per-card ribbon. Examples:
//
//	closed, chain, MaxRuns 10 : "Iteration 3 of 10"
//	closed, chain, no budget  : "Iteration 3"
//	live, chain               : "Iteration 7 (in progress)"
//	closed, fire-key          : "Iteration 3 of 10 (by fire order)"
//	live, fire-key            : "Iteration 7 (in progress) (by fire order)"
//	closed, time fallback     : "Iteration 3 of 10 (by time)"
//	live, time fallback       : "Iteration 7 (in progress) (by time)"
//
// Only INFERRED orders carry a basis hint: the schedule_key fire-slot recovery
// reads "(by fire order)" (a real, trustworthy run order sourced from the fire
// schedule), and the created_at last resort reads "(by time)" (heuristic). A
// genuine causal chain (basisChain) gets no hint.
//
// NOTE: labeled "Iteration N" (not "Run #N") until R2 true-chain numbering
// lands (ka-0000005750): "Run #N" implied a position in the full spawn
// sequence the current chain reconstruction cannot honor.
func runLabel(runNumber, maxRuns int, isLive bool, basis orderBasis) string {
	label := fmt.Sprintf("Iteration %d", runNumber)
	if isLive {
		label += " (in progress)"
	} else if maxRuns > 0 {
		label += fmt.Sprintf(" of %d", maxRuns)
	}
	switch basis {
	case basisFireKey:
		label += " (by fire order)"
	case basisTime:
		label += " (by time)"
	}
	return label
}

// parseScheduleInt reads a non-negative integer custom field, returning 0 when
// absent or unparseable.
func parseScheduleInt(fields map[string]string, key string) int {
	if fields == nil {
		return 0
	}
	n, err := strconv.Atoi(fields[key])
	if err != nil || n < 0 {
		return 0
	}
	return n
}
