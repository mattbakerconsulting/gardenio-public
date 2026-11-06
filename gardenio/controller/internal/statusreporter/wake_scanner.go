package statusreporter

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// Wake-scanner (persistent-agents Phase 2, task ka-0000004902). The counterpart
// to the in-pod idle-TTL PARK path (cmd/gb init_agent_idle_ttl.go idleTTLDespawn):
// when a persistent agent's warm window expires, that path CLOSES its agent bead
// but stamps wake metadata — successor=same + next_respawn_at + a parked
// close_reason — so the identity is revivable rather than terminal. Nothing in
// the fleet clears that metadata or acts on it today; this scanner is what makes
// the park mean something.
//
// Each pass:
//
//  1. SCAN closed type=agent beads carrying successor=same whose next_respawn_at
//     is ≤ now (the wake is due). Server-side filtered + paged, so a large closed
//     backlog never OOMs the scanner (advice ka-0000003555).
//  2. BUDGET-gate each candidate: respawn_count < max_respawns (default
//     DefaultMaxRespawns). An exhausted budget CLEARS the wake field (so the
//     identity stops matching) and does NOT revive — the per-lineage self-wake
//     loop backstop (ka-0000001361). A malformed next_respawn_at is treated the
//     same way: cleared, never revived, so a bad stamp can't wedge the scan.
//  3. RECREATE-BY-NAME: reopen the closed bead (status=open) in ONE atomic PATCH
//     that also deletes next_respawn_at (idempotent clear), deletes the terminal
//     agent_state (so the recreated pod boots clean), and bumps respawn_count.
//     The agent bead keeps its NAME, so the reconciler's existing cold-start
//     create-missing loop recreates the pod under the same name and the pod
//     `claude --resume`s the conversation from the fsynced transcript — no new
//     spawn primitive, no new pod path.
//
// The clear is what makes the whole thing idempotent: it lands in the SAME write
// as the reopen (RFC-7386 merge on the fields object, mirroring the plan-gardener
// wake ka-0000001583), so a re-delivered or overlapping pass finds an already
// reopened bead that no longer carries next_respawn_at and skips it. A reopen
// failure leaves the wake field intact, so the bead is simply retried next pass.
//
// Reaper coexistence: a parked bead is CLOSED, so neither the stale reaper nor
// the reconciler's leaked-bead closer (both list only active statuses) ever sees
// it — the metadata rests safely on a closed bead until its wake is due. The
// reaper-exclusion this task ships (reapReason's terminal-orphan case) guards the
// opposite window: the brief interval AFTER this scanner reopens the bead but
// BEFORE the reconciler has recreated the pod, so a just-woken podless bead is
// not immediately orphan-reaped out from under the wake.

// wakeScanMaxPages bounds the paged scan of closed agent beads. Parked beads (a
// stamped next_respawn_at) are a small population, so a handful of pages is
// ample; the bound is a runaway backstop, not an expected limit.
const wakeScanMaxPages = 20

// wakeScanPageSize is the per-page row count for the closed-agent scan.
const wakeScanPageSize = 200

// WakeScanner revives parked persistent-agent identities whose wake is due. It
// owns no pod lifecycle itself: it reopens the agent bead and clears the wake
// metadata, then leaves pod recreation to the reconciler's cold-start path.
type WakeScanner struct {
	daemon *beadsapi.Client
	logger *slog.Logger

	// now is injectable for deterministic tests; defaults to time.Now.
	now func() time.Time
}

// WakeScannerStats summarizes one scan pass for periodic-sync logging.
type WakeScannerStats struct {
	// Scanned is the number of due candidates examined this pass.
	Scanned int
	// Woken is the number of parked identities reopened (wake fired).
	Woken int
	// BudgetExhausted is the number of due candidates whose respawn budget was
	// spent — cleared, not revived.
	BudgetExhausted int
	// Errors is the number of candidates that failed to reopen (retried next pass).
	Errors int
}

// NewWakeScanner constructs a wake-scanner. Returns nil when daemon is nil so
// the caller can leave the periodic hook unwired in single-binary/dev setups
// without a nil-deref, mirroring NewStaleReaper's disabled-returns-nil contract.
func NewWakeScanner(daemon *beadsapi.Client, logger *slog.Logger) *WakeScanner {
	if daemon == nil {
		return nil
	}
	return &WakeScanner{daemon: daemon, logger: logger, now: time.Now}
}

// Scan runs one wake pass and returns its stats. Best-effort throughout: a list
// or per-bead error logs and continues, never aborting the pass, so one bad bead
// can't starve the rest.
func (s *WakeScanner) Scan(ctx context.Context) WakeScannerStats {
	now := s.now()
	var stats WakeScannerStats

	for _, b := range s.dueParkedBeads(ctx, now) {
		stats.Scanned++
		s.wakeOne(ctx, b, now, &stats)
	}

	if stats.Woken > 0 || stats.BudgetExhausted > 0 || stats.Errors > 0 {
		s.logger.Info("wake scan pass complete",
			"scanned", stats.Scanned, "woken", stats.Woken,
			"budget_exhausted", stats.BudgetExhausted, "errors", stats.Errors)
	}
	return stats
}

// dueParkedBeads lists closed agent beads carrying successor=same whose
// next_respawn_at is ≤ now. The closed + successor=same + next_respawn_at
// present filter runs server-side; the ≤ now comparison is applied client-side
// because it is a relative-time predicate the fields filter cannot express.
// Paged to bound memory.
//
// The presence term matters for cost: successor=same also outlives the park on
// every closed agent that stopped with it but was never stamped (1,260 of
// ~15.5k closed agents on 2026-11-05, none stamped), and wakeIsDue skips those
// anyway. Indexed by kbeads migration 064. NoTotal because only the page is
// read (gd-lCYkpAlNas).
func (s *WakeScanner) dueParkedBeads(ctx context.Context, now time.Time) []*beadsapi.BeadDetail {
	var due []*beadsapi.BeadDetail
	for page := 0; page < wakeScanMaxPages; page++ {
		result, err := s.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
			Types:    []string{"agent"},
			Statuses: []string{"closed"},
			Fields: map[string]string{
				beadsapi.FieldSuccessor:     beadsapi.SuccessorSame,
				beadsapi.FieldNextRespawnAt: "*",
			},
			Sort:    "-updated_at",
			Limit:   wakeScanPageSize,
			Offset:  page * wakeScanPageSize,
			NoTotal: true,
		})
		if err != nil {
			s.logger.Warn("wake scan: closed-agent list failed", "error", err, "page", page)
			return due
		}
		for _, b := range result.Beads {
			// Re-check type/status/successor client-side: test stubs and lenient
			// daemons may return unfiltered rows.
			if b.Type != "agent" || b.Status != "closed" {
				continue
			}
			if b.Fields[beadsapi.FieldSuccessor] != beadsapi.SuccessorSame {
				continue
			}
			if !wakeIsDue(b.Fields[beadsapi.FieldNextRespawnAt], now) {
				continue
			}
			due = append(due, b)
		}
		if len(result.Beads) < wakeScanPageSize {
			break
		}
	}
	return due
}

// wakeIsDue reports whether a parked bead's next_respawn_at wake timestamp has
// arrived (≤ now). An empty stamp is NOT due (a successor=same bead that carries
// no wake time is not a scanner candidate — e.g. a successor=new mis-set, or a
// park mid-write). A malformed stamp IS treated as due so the budget path can
// clear it rather than let a bad value wedge the identity forever; malformedWake
// distinguishes the two for wakeOne.
func wakeIsDue(nextRespawnAt string, now time.Time) bool {
	if nextRespawnAt == "" {
		return false
	}
	t, err := time.Parse(time.RFC3339, nextRespawnAt)
	if err != nil {
		return true // malformed → surface to wakeOne, which clears it
	}
	return !t.After(now)
}

// wakeInFuture reports whether a next_respawn_at wake timestamp is present,
// parseable, and STILL IN THE FUTURE (strictly after now). Used by the stale
// reaper to exempt a parked identity that has not yet reached its wake time.
// An empty or malformed stamp is NOT "in the future" — those are handled by the
// scanner's due-path (a malformed stamp is cleared), never protected here.
func wakeInFuture(nextRespawnAt string, now time.Time) bool {
	if nextRespawnAt == "" {
		return false
	}
	t, err := time.Parse(time.RFC3339, nextRespawnAt)
	if err != nil {
		return false
	}
	return t.After(now)
}

// malformedWake reports whether a next_respawn_at value is present but unparseable.
func malformedWake(nextRespawnAt string) bool {
	if nextRespawnAt == "" {
		return false
	}
	_, err := time.Parse(time.RFC3339, nextRespawnAt)
	return err != nil
}

// wakeOne revives (or budget-clears) a single due parked bead.
func (s *WakeScanner) wakeOne(ctx context.Context, b *beadsapi.BeadDetail, now time.Time, stats *WakeScannerStats) {
	count := parseIntField(b.Fields[beadsapi.FieldRespawnCount], 0)
	max := parseIntField(b.Fields[beadsapi.FieldMaxRespawns], beadsapi.DefaultMaxRespawns)

	// Goal-unachievable backstop (P3, ka-0000005843): a bead that declared its
	// goal deterministically unachievable must NEVER be revived, no matter what
	// wake metadata survives on it. The goal-unachievable close already clears
	// successor + next_respawn_at, so such a bead should not even reach here — but
	// if a stray successor=same slipped through, clear the wake WITHOUT reviving so
	// a deterministically-failing identity can never crash-loop the pod.
	if strings.EqualFold(strings.TrimSpace(b.Fields[beadsapi.FieldGoalUnachievable]), "true") {
		s.logger.Warn("wake scan: goal_unachievable set, clearing wake without revive",
			"bead", b.ID, "agent", b.Fields["agent"])
		if s.clearWakeOnly(ctx, b.ID) {
			stats.BudgetExhausted++
		} else {
			stats.Errors++
		}
		return
	}

	// Budget gate + malformed-stamp gate: either one clears the wake metadata
	// WITHOUT reviving, so the identity stops matching and the scan converges.
	if count >= max {
		s.logger.Warn("wake scan: respawn budget exhausted, clearing wake without revive",
			"bead", b.ID, "agent", b.Fields["agent"], "respawn_count", count, "max_respawns", max)
		if s.clearWakeOnly(ctx, b.ID) {
			stats.BudgetExhausted++
		} else {
			stats.Errors++
		}
		return
	}
	if malformedWake(b.Fields[beadsapi.FieldNextRespawnAt]) {
		s.logger.Warn("wake scan: malformed next_respawn_at, clearing wake without revive",
			"bead", b.ID, "agent", b.Fields["agent"],
			"next_respawn_at", b.Fields[beadsapi.FieldNextRespawnAt])
		if s.clearWakeOnly(ctx, b.ID) {
			stats.BudgetExhausted++
		} else {
			stats.Errors++
		}
		return
	}

	// Revive: reopen + clear wake field + clear terminal agent_state + bump the
	// respawn budget, all in ONE atomic PATCH (RFC-7386 merge on `fields`; nil
	// deletes). Same shape as the plan-gardener operator-reopen wake (ka-0000001583).
	open := "open"
	if err := s.daemon.UpdateBead(ctx, b.ID, beadsapi.UpdateBeadRequest{
		Status: &open,
		// Clears next_respawn_at + agent_state, bumps respawn_count, stamps
		// wake_reason=timer (surfaced as BOAT_WAKE_REASON, ka-0000000027).
		Fields: beadsapi.ReviveAgentFields(count+1, beadsapi.WakeReasonTimer),
	}); err != nil {
		s.logger.Warn("wake scan: failed to reopen parked bead (retry next pass)",
			"bead", b.ID, "agent", b.Fields["agent"], "error", err)
		stats.Errors++
		return
	}

	s.logger.Info("wake scan: revived parked agent identity",
		"bead", b.ID, "agent", b.Fields["agent"], "project", b.Fields["project"],
		"respawn_count", count+1, "max_respawns", max,
		"was_due_at", b.Fields[beadsapi.FieldNextRespawnAt])

	// Audit comment mirrors the other lifecycle reopen paths (stale-reaper,
	// gardener rearm) so the trail reads consistently. Best-effort: a comment
	// failure does not undo the wake.
	comment := fmt.Sprintf(
		"Woken by controller wake-scanner: parked persistent identity `%s` reached its "+
			"next_respawn_at wake time — reopened (respawn %d/%d) so the reconciler recreates the "+
			"pod under the same name and `claude --resume`s the conversation. (persistent-agents "+
			"Phase 2, ka-0000004902)",
		b.Fields["agent"], count+1, max)
	if err := s.daemon.AddComment(ctx, b.ID, "gardenio", comment); err != nil {
		s.logger.Warn("wake scan: revived bead but audit comment failed", "bead", b.ID, "error", err)
	}

	stats.Woken++
}

// clearWakeOnly deletes just the wake metadata (next_respawn_at + successor) on a
// bead WITHOUT reopening it — the budget-exhausted / malformed-stamp path. The
// bead stays closed and terminal; clearing successor too means it will never be
// re-selected by the scan. Returns true on success.
func (s *WakeScanner) clearWakeOnly(ctx context.Context, beadID string) bool {
	if err := s.daemon.UpdateBead(ctx, beadID, beadsapi.UpdateBeadRequest{
		Fields: map[string]any{
			beadsapi.FieldNextRespawnAt: nil,
			beadsapi.FieldSuccessor:     nil,
		},
	}); err != nil {
		s.logger.Warn("wake scan: failed to clear wake metadata", "bead", beadID, "error", err)
		return false
	}
	return true
}

// parseIntField parses a string-coded int field, returning def on empty/malformed.
func parseIntField(v string, def int) int {
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}
