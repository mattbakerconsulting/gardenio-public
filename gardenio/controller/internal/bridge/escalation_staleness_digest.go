// Package bridge — engine-owned consolidated staleness digest for stale open
// P0/P1 escalations (ka-0000006001 / design ka-0000003756).
//
// EscalationStalenessDigestLoop periodically sweeps open escalations and posts
// ONE consolidated message to the operator queue channel listing every P0/P1
// that has been awaiting operator action past its priority threshold (P0 > 2h,
// P1 > 8h). It is the low-threshold, scannable-snapshot complement to the two
// per-escalation primitives that already exist:
//
//   - EscalationPingbackLoop re-notifies a SINGLE stalled escalation on a
//     12h/24h/48h+ backoff ladder (a thread broadcast per item).
//   - NotifyEscalationQueueAlert posts a SINGLE per-item alert to the queue
//     channel once an escalation crosses the 48h queue threshold.
//
// Neither answers "what is stale in the queue RIGHT NOW, at a glance." With a
// backlog of dozens of open P0/P1s, per-item pings scatter across threads; a
// consolidated digest gives the operator one line-per-escalation list to triage.
//
// WHY THIS IS NOT DUPLICATE-NOTIFY SPAM (the constraint that deferred the design
// in the first place): the digest is engine-owned and NOT agent-triggerable, so
// N parked gardeners cannot each fire it at one P0 (the 4x-PINGBACK pathology).
// Anti-spam within the loop is per-bead: an escalation re-appears in a digest at
// most once per digestReWarmInterval, tracked by a last_digested_at stamp on the
// bead — so the same standing backlog is not re-posted every sweep, and a bridge
// restart re-reads the stamps from the beads and does NOT re-fire the whole
// backlog at once (the exact restart hazard the design called out). All dedup
// state lives on the bead; none in memory.
//
// The digest is OFF by default (ESCALATION_STALE_DIGEST unset) — first release
// is opt-in, mirroring the design's "config flag defaulting OFF" constraint.
package bridge

import (
	"context"
	"log/slog"
	"sort"
	"time"

	"gardenio/controller/internal/beadsapi"
)

const (
	// defaultDigestSweepInterval is how often the loop re-evaluates open
	// escalations. The coarse thresholds (hours) make a tight sweep pointless;
	// this sits inside the design's suggested 15-30m window.
	defaultDigestSweepInterval = 20 * time.Minute

	// defaultP0StaleThreshold / defaultP1StaleThreshold are the stall ages past
	// which a P0 / P1 escalation is considered digest-worthy. Lower than the
	// pingback ladder's first rung (12h) and the queue alert (48h) so a fresh P0
	// surfaces in the consolidated view hours before either per-item primitive
	// fires. P2+ escalations are never digested.
	defaultP0StaleThreshold = 2 * time.Hour
	defaultP1StaleThreshold = 8 * time.Hour

	// defaultDigestReWarmInterval bounds how often a still-stale escalation
	// re-appears in a digest. Without it the same backlog would be re-posted
	// every sweep; with it, a standing P0 shows up, goes quiet for this window,
	// then re-warms if still unresolved. This is the per-bead anti-spam control.
	defaultDigestReWarmInterval = 6 * time.Hour

	// digestSweepPageSize / digestSweepMaxPages page the open-escalation walk to
	// exhaustion with a hard ceiling, mirroring the pingback loop (ka-0000005723):
	// a single capped call silently drops the OLDEST rows — precisely the stale
	// escalations the digest exists to surface. 10 × 200 = 2000, an order of
	// magnitude above the observed open-escalation population.
	digestSweepPageSize = 200
	digestSweepMaxPages = 10

	// digestMaxEntries caps how many escalations one digest message lists, so a
	// pathological backlog cannot produce a Slack message that exceeds block
	// limits. The overflow count is reported in the message footer.
	digestMaxEntries = 40
)

// StaleEscalationDigestEntry is one row in a consolidated staleness digest.
type StaleEscalationDigestEntry struct {
	ID             string
	Title          string
	Priority       int
	Age            time.Duration
	OperationType  string
	RequestingTask string
}

// StaleDigestNotifier posts a consolidated staleness digest to the operator
// queue channel. *Bot satisfies it via PostStaleEscalationDigest; a narrow
// interface keeps the loop testable without the whole EscalationNotifier.
type StaleDigestNotifier interface {
	PostStaleEscalationDigest(ctx context.Context, entries []StaleEscalationDigestEntry, overflow int) error
}

// EscalationStalenessDigestLoop drives the consolidated staleness digest.
type EscalationStalenessDigestLoop struct {
	daemon   BeadClient
	notifier StaleDigestNotifier
	logger   *slog.Logger

	interval    time.Duration
	p0Threshold time.Duration
	p1Threshold time.Duration
	reWarm      time.Duration

	// now is injectable for tests; defaults to time.Now.
	now func() time.Time
}

// EscalationStalenessDigestConfig configures an EscalationStalenessDigestLoop.
type EscalationStalenessDigestConfig struct {
	Daemon   BeadClient
	Notifier StaleDigestNotifier
	Logger   *slog.Logger
	Interval time.Duration // ≤ 0 → defaultDigestSweepInterval

	// Threshold overrides; ≤ 0 falls back to the package defaults. Exposed for
	// tests and future tuning without an env-var change.
	P0Threshold time.Duration
	P1Threshold time.Duration
	ReWarm      time.Duration
}

// NewEscalationStalenessDigestLoop constructs the digest loop. A nil Daemon or
// Notifier disables it (returns nil), so a slack-bridge running without a Slack
// app — or a deployment that has not opted in — is unaffected. The loop is
// nil-safe: callers `go loop.Run(ctx)` unconditionally.
func NewEscalationStalenessDigestLoop(cfg EscalationStalenessDigestConfig) *EscalationStalenessDigestLoop {
	if cfg.Daemon == nil || cfg.Notifier == nil {
		return nil
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	interval := cfg.Interval
	if interval <= 0 {
		interval = defaultDigestSweepInterval
	}
	p0 := cfg.P0Threshold
	if p0 <= 0 {
		p0 = defaultP0StaleThreshold
	}
	p1 := cfg.P1Threshold
	if p1 <= 0 {
		p1 = defaultP1StaleThreshold
	}
	reWarm := cfg.ReWarm
	if reWarm <= 0 {
		reWarm = defaultDigestReWarmInterval
	}
	return &EscalationStalenessDigestLoop{
		daemon:      cfg.Daemon,
		notifier:    cfg.Notifier,
		logger:      logger,
		interval:    interval,
		p0Threshold: p0,
		p1Threshold: p1,
		reWarm:      reWarm,
		now:         time.Now,
	}
}

// Run sweeps immediately, then on every tick until ctx is canceled. Nil-safe.
func (l *EscalationStalenessDigestLoop) Run(ctx context.Context) {
	if l == nil {
		return
	}
	l.logger.Info("escalation staleness digest loop starting",
		"interval", l.interval, "p0_threshold", l.p0Threshold,
		"p1_threshold", l.p1Threshold, "re_warm", l.reWarm)
	l.sweepOnce(ctx)

	ticker := time.NewTicker(l.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			l.logger.Info("escalation staleness digest loop stopping")
			return
		case <-ticker.C:
			l.sweepOnce(ctx)
		}
	}
}

// staleThreshold returns the digest-worthy stall age for a priority, and false
// for priorities the digest ignores (P2+).
func (l *EscalationStalenessDigestLoop) staleThreshold(priority int) (time.Duration, bool) {
	switch priority {
	case 0:
		return l.p0Threshold, true
	case 1:
		return l.p1Threshold, true
	default:
		return 0, false
	}
}

// sweepOnce collects every open P0/P1 escalation that is due for a digest this
// sweep, posts ONE consolidated message if any are due, and stamps
// last_digested_at on each included bead only after a successful post.
//
// The fetch pages to exhaustion with a hard ceiling, matching the pingback loop:
// /v1/beads returns newest-first, so a single capped call would silently drop
// the OLDEST — the stalest — escalations, precisely the rows this digest exists
// to surface. Sorting ascending by created_at also makes the walk stable under
// concurrent creates.
func (l *EscalationStalenessDigestLoop) sweepOnce(ctx context.Context) {
	now := l.now()
	var due []StaleEscalationDigestEntry
	dueIDs := make(map[string]struct{})

	for page := 0; page < digestSweepMaxPages; page++ {
		res, err := l.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
			Types:    []string{"escalation"},
			Statuses: []string{"open", "in_progress"},
			Sort:     "created_at",
			Limit:    digestSweepPageSize,
			Offset:   page * digestSweepPageSize,
		})
		if err != nil {
			l.logger.Warn("escalation staleness digest sweep: list failed", "error", err, "page", page)
			return
		}
		for _, b := range res.Beads {
			if _, dup := dueIDs[b.ID]; dup {
				continue
			}
			if entry, ok := l.evaluateBead(b, now); ok {
				dueIDs[b.ID] = struct{}{}
				due = append(due, entry)
			}
		}
		if len(res.Beads) < digestSweepPageSize {
			break
		}
		if page == digestSweepMaxPages-1 {
			l.logger.Warn("escalation staleness digest sweep: page cap reached, tail not evaluated this sweep",
				"max_pages", digestSweepMaxPages, "page_size", digestSweepPageSize)
		}
	}

	if len(due) == 0 {
		return
	}

	// Oldest-first: the most-stalled escalation heads the digest.
	sort.Slice(due, func(i, j int) bool {
		if due[i].Priority != due[j].Priority {
			return due[i].Priority < due[j].Priority
		}
		return due[i].Age > due[j].Age
	})

	overflow := 0
	if len(due) > digestMaxEntries {
		overflow = len(due) - digestMaxEntries
		due = due[:digestMaxEntries]
	}

	if err := l.notifier.PostStaleEscalationDigest(ctx, due, overflow); err != nil {
		// Post failed → do NOT stamp, so the next sweep retries the whole batch.
		l.logger.Error("escalation staleness digest: post failed, not stamping", "error", err, "entries", len(due))
		return
	}

	// Stamp last_digested_at only on the escalations actually listed (the
	// overflow tail is intentionally left unstamped so it surfaces next sweep).
	stamp := now.UTC().Format(time.RFC3339)
	for _, e := range due {
		if err := l.daemon.UpdateBeadFields(ctx, e.ID, map[string]string{"last_digested_at": stamp}); err != nil {
			// Non-fatal: a missed stamp only means this escalation re-appears in
			// the next sweep's digest sooner than the re-warm window — annoying,
			// not incorrect, and never a lost notification.
			l.logger.Warn("escalation staleness digest: failed to stamp last_digested_at", "id", e.ID, "error", err)
		}
	}

	l.logger.Info("posted escalation staleness digest", "entries", len(due), "overflow", overflow)
}

// evaluateBead decides whether one escalation belongs in this sweep's digest and,
// if so, returns its entry. Skips approved, review-flagged, E2E-fixture, and
// non-P0/P1 escalations, and any escalation digested within the re-warm window.
func (l *EscalationStalenessDigestLoop) evaluateBead(b *beadsapi.BeadDetail, now time.Time) (StaleEscalationDigestEntry, bool) {
	fields := b.Fields
	if fields == nil {
		fields = map[string]string{}
	}

	// Approved escalations are moving through execution — nothing stale to surface.
	if fields["approved_by"] != "" {
		return StaleEscalationDigestEntry{}, false
	}
	// Review-flagged escalations already carry their one actionable pingback; the
	// pingback loop stops nagging them, and so does the digest.
	if fields[mrLandedField] == "true" {
		return StaleEscalationDigestEntry{}, false
	}
	// E2E fixtures must not spam production channels.
	if isE2EEscalation(b.CreatedBy, b.Labels) {
		return StaleEscalationDigestEntry{}, false
	}

	threshold, ok := l.staleThreshold(b.Priority)
	if !ok {
		return StaleEscalationDigestEntry{}, false
	}

	anchor := pingbackAnchor(fields, b.CreatedAt)
	if anchor.IsZero() {
		return StaleEscalationDigestEntry{}, false
	}
	age := now.Sub(anchor)
	if age < threshold {
		return StaleEscalationDigestEntry{}, false
	}

	// Per-bead re-warm dedup: skip if digested within the re-warm window. A
	// restart re-reads this stamp from the bead, so the whole backlog is not
	// re-fired at once.
	if v := fields["last_digested_at"]; v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil && now.Sub(t) < l.reWarm {
			return StaleEscalationDigestEntry{}, false
		}
	}

	return StaleEscalationDigestEntry{
		ID:             b.ID,
		Title:          b.Title,
		Priority:       b.Priority,
		Age:            age,
		OperationType:  fields["operation_type"],
		RequestingTask: fields["requesting_task"],
	}, true
}
