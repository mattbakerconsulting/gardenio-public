// Package bridge — engine-owned escalation re-notification cadence
// (ka-0000004385).
//
// EscalationPingbackLoop periodically sweeps open escalations and stamps
// pingback requests on the ones the operator has not acted on, following an
// exponential-backoff ladder: first re-notify ~12h after the original Slack
// post, again at ~24h, then a named-human-queue alert at ~48h, then doubling
// (96h, 192h, ...) so a genuinely-stuck escalation stays warm without
// unbounded spam. The loop only stamps bead fields — the Escalations SSE
// watcher picks up the update and does the actual Slack posting via the
// pingback_requested_at / pingback_notified_at handshake, so the cadence
// engine is fully decoupled from Slack and restart-safe (all state lives on
// the bead, none in memory).
//
// This replaces the ad-hoc gardener behavior of hand-rolling Slack nudges
// (13+ manual pingbacks on ka-0000003694) and the silent-staleness failure
// mode (2-day-old P0 nobody re-surfaced).
package bridge

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
)

const (
	// defaultPingbackSweepInterval is how often the loop re-evaluates open
	// escalations. The coarse thresholds (hours) make a tight sweep pointless.
	defaultPingbackSweepInterval = 15 * time.Minute

	// pingbackQueueThreshold is the stall age past which the operator queue
	// channel is alerted (the "named human queue" upper bound).
	pingbackQueueThreshold = 48 * time.Hour

	// maxAutoPingbacks caps the backoff ladder. With thresholds at
	// 12h, 24h, 48h, 96h, 192h, 384h, ... step 10 is ~256 days — far past
	// the point where a human must have intervened.
	maxAutoPingbacks = 10

	// pingbackSweepPageSize is the per-request page size for the sweep's
	// exhaustion walk, and pingbackSweepMaxPages bounds it so a runaway
	// population can never spin the loop forever. 10 × 200 = 2000 open
	// escalations, an order of magnitude above the observed 168.
	pingbackSweepPageSize = 200
	pingbackSweepMaxPages = 10
)

// autoPingbackThreshold returns the stall age at which auto-pingback step n
// (1-based) fires: 12h, 24h, 48h, then doubling.
func autoPingbackThreshold(n int) time.Duration {
	if n < 1 {
		return 0
	}
	return 12 * time.Hour << (n - 1)
}

// autoPingbackStepsPassed returns how many ladder steps a given stall age has
// crossed, capped at maxAutoPingbacks.
func autoPingbackStepsPassed(age time.Duration) int {
	steps := 0
	for steps < maxAutoPingbacks && age >= autoPingbackThreshold(steps+1) {
		steps++
	}
	return steps
}

// EscalationPingbackLoop drives the automated re-notification cadence.
type EscalationPingbackLoop struct {
	daemon   BeadClient
	logger   *slog.Logger
	interval time.Duration

	// now is injectable for tests; defaults to time.Now.
	now func() time.Time
}

// EscalationPingbackLoopConfig configures an EscalationPingbackLoop.
type EscalationPingbackLoopConfig struct {
	Daemon   BeadClient
	Logger   *slog.Logger
	Interval time.Duration // ≤ 0 → defaultPingbackSweepInterval
}

// NewEscalationPingbackLoop constructs the cadence loop.
func NewEscalationPingbackLoop(cfg EscalationPingbackLoopConfig) *EscalationPingbackLoop {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	interval := cfg.Interval
	if interval <= 0 {
		interval = defaultPingbackSweepInterval
	}
	return &EscalationPingbackLoop{
		daemon:   cfg.Daemon,
		logger:   logger,
		interval: interval,
		now:      time.Now,
	}
}

// Run sweeps immediately, then on every tick until ctx is canceled.
func (l *EscalationPingbackLoop) Run(ctx context.Context) {
	l.logger.Info("escalation pingback loop starting", "interval", l.interval)
	l.sweepOnce(ctx)

	ticker := time.NewTicker(l.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			l.logger.Info("escalation pingback loop stopping")
			return
		case <-ticker.C:
			l.sweepOnce(ctx)
		}
	}
}

// sweepOnce evaluates every open, unapproved escalation against the ladder.
//
// The fetch pages to exhaustion (ka-0000005723). A single capped call silently
// drops every escalation past the cap, and because /v1/beads returns
// newest-first that truncation lands on the OLDEST rows — precisely the ones
// the backoff ladder exists to keep warm. Measured 2026-08-21: 168 open
// escalations against the former Limit:200, growing ~+13/week, so the cap was
// weeks from biting with no error, no failed sweep, and no queue-visible
// symptom. Sorting ascending by created_at additionally makes the walk stable:
// concurrently-created escalations append past the current offset window
// instead of shifting an already-read page.
func (l *EscalationPingbackLoop) sweepOnce(ctx context.Context) {
	seen := make(map[string]struct{})
	for page := 0; page < pingbackSweepMaxPages; page++ {
		res, err := l.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
			Types:    []string{"escalation"},
			Statuses: []string{"open", "in_progress"},
			Sort:     "created_at",
			Limit:    pingbackSweepPageSize,
			Offset:   page * pingbackSweepPageSize,
		})
		if err != nil {
			l.logger.Warn("escalation pingback sweep: list failed", "error", err, "page", page)
			return
		}
		for _, b := range res.Beads {
			if _, dup := seen[b.ID]; dup {
				continue
			}
			seen[b.ID] = struct{}{}
			l.evaluateBead(ctx, b)
		}
		if len(res.Beads) < pingbackSweepPageSize {
			// Short page → the daemon returned every remaining match.
			return
		}
	}
	// Ran the page cap without a short page: the open-escalation population
	// exceeds what one sweep can walk. Unlike the silent truncation this
	// replaces, say so — the next sweep re-walks from the start, so the tail
	// is delayed rather than lost, but a sustained warning means the ceiling
	// needs raising.
	l.logger.Warn("escalation pingback sweep: page cap reached, tail not evaluated this sweep",
		"max_pages", pingbackSweepMaxPages, "page_size", pingbackSweepPageSize, "evaluated", len(seen))
}

// evaluateBead fires at most ONE ladder step per sweep for one escalation.
// After bridge downtime the count jumps straight to the current step rather
// than replaying every missed re-notify — one catch-up post, not a burst.
func (l *EscalationPingbackLoop) evaluateBead(ctx context.Context, b *beadsapi.BeadDetail) {
	fields := b.Fields
	if fields == nil {
		fields = map[string]string{}
	}

	// Approved escalations are moving through execution — nothing to warm.
	if fields["approved_by"] != "" {
		return
	}
	// Only an owner-audience escalation is warmed (#756 audience, which follows
	// held_by, D11). An in-hand one is a coordinator's queue; it reaches the
	// owner by hand-up (Task 5) or the idle hand-up.
	if !beadsapi.EscalationForOwner(fields) {
		return
	}
	// Flagged-for-review escalations (ka-0000002563) already carry the ONE
	// actionable "MR landed, verify the apply step" pingback the review flag
	// requested. Re-nagging them would resurrect exactly the stale-text spam the
	// flag exists to stop, so the auto-backoff ladder stops climbing here until a
	// human clears the review (closes the bead or drops the mr_landed latch).
	if fields[mrLandedField] == "true" {
		return
	}
	// E2E fixtures must not spam production channels.
	if isE2EEscalation(b.CreatedBy, b.Labels) {
		return
	}

	anchor := pingbackAnchor(fields, b.CreatedAt)
	if anchor.IsZero() {
		return
	}
	age := l.now().Sub(anchor)

	want := autoPingbackStepsPassed(age)
	have := 0
	if v := fields["auto_pingback_count"]; v != "" {
		have, _ = strconv.Atoi(v)
	}
	if want <= have {
		return
	}

	now := l.now().UTC().Format(time.RFC3339)
	total := 0
	if v := fields["pingback_count"]; v != "" {
		total, _ = strconv.Atoi(v)
	}

	update := map[string]string{
		"auto_pingback_count":   strconv.Itoa(want),
		"pingback_requested_at": now,
		"pingback_count":        strconv.Itoa(total + 1),
		"pingback_source":       "auto",
		"pingback_by":           "escalation-pingback-loop",
		"pingback_reason": fmt.Sprintf(
			"auto re-notify #%d: awaiting operator action for %s",
			want, formatStallDuration(age)),
	}

	// Past the queue threshold, also request the named-human-queue alert
	// (once — the requested_at stamp is only written while empty).
	if age >= pingbackQueueThreshold && fields["queue_alert_requested_at"] == "" {
		update["queue_alert_requested_at"] = now
	}

	if err := l.daemon.UpdateBeadFields(ctx, b.ID, update); err != nil {
		l.logger.Warn("escalation pingback sweep: stamp failed",
			"id", b.ID, "error", err)
		return
	}

	l.logger.Info("auto-pingback requested",
		"id", b.ID,
		"step", want,
		"stall_age", age.Round(time.Minute),
		"queue_alert", update["queue_alert_requested_at"] != "")
}

// pingbackAnchor returns the stall clock's zero point: notified_at (when the
// operator was first shown the card) when present, falling back to the bead's
// creation time for escalations that predate notified_at stamping; a later
// handed_up_at moves it forward.
func pingbackAnchor(fields map[string]string, createdAt time.Time) time.Time {
	anchor := createdAt
	if v := fields["notified_at"]; v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			anchor = t
		}
	}
	// A hand-up restarts the clock: the hours a coordinator held it are not
	// hours the human ignored it.
	if t, err := time.Parse(time.RFC3339, fields["handed_up_at"]); err == nil && t.After(anchor) {
		anchor = t
	}
	return anchor
}

// isE2EEscalation mirrors the CLI-side isE2EFixture heuristics plus the
// e2e:true label used by decision watchers.
func isE2EEscalation(createdBy string, labels []string) bool {
	if hasLabel(labels, "e2e:true") {
		return true
	}
	switch createdBy {
	case "escalation-analyze", "e2e-agent-v2":
		return true
	}
	return strings.HasPrefix(createdBy, "e2e-")
}

// formatStallDuration renders a stall age like "26h" or "3d2h" for the
// auto-pingback reason line.
func formatStallDuration(d time.Duration) string {
	hours := int(d.Hours())
	if hours < 48 {
		return fmt.Sprintf("%dh", hours)
	}
	days := hours / 24
	rem := hours % 24
	if rem == 0 {
		return fmt.Sprintf("%dd", days)
	}
	return fmt.Sprintf("%dd%dh", days, rem)
}
