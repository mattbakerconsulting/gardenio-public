package scheduler

import (
	"context"
	"log/slog"
	"strings"
	"time"
)

// needs-gardener backstop sweep (persistent-agents Phase 3, task ka-0000005121,
// design memo ka-0000001420 §2.3 / §3.2 / §4).
//
// Gardener ownership is stamped at SPAWN (beadsapi.stampAgentGardenerID): every
// agent bead resolves a canonical gardener_id — a real gardener (trigger) bead,
// or a `synthetic-gardener:<source>` bucket for ad-hoc vectors — so the viewer's
// "Unowned" bucket is empty by construction. Enforcement is deliberately soft
// and at-spawn only; there is no write-time gate and no graph-mutating sweeper.
//
// This sweep is the OPTIONAL backstop the memo reserved for the case where the
// at-spawn stamp leaks (a spawn path that forgot to set a gardener, or a legacy
// bead written before the stamp existed). Mirroring the OrphanSweeper's
// needs-intake behaviour, it surfaces such agents by applying a QUERYABLE LABEL
// only (default "needs-gardener") — never a dependency edge. Labelling is the
// conservative choice: it is idempotent, reversible, has no blast radius beyond
// a queryable label, and never touches liveness (a labelled agent is still
// reaped normally). An operator or gardener consumes the label and reparents the
// agent with `gb agent adopt <id> --gardener <id>`, which removes the label.
//
// The sweep is OFF by default (see cmd/trigger-engine ORPHAN-style wiring): the
// at-spawn stamp already makes the leak set empty in the common case, so the
// backstop is armed only when an operator actually wants the queryable signal.

// DefaultNeedsGardenerLabel is the label the sweep applies to surface an active
// agent bead that has no real gardener (an at-spawn gardener_id stamp leak).
const DefaultNeedsGardenerLabel = "needs-gardener"

// defaultNeedsGardenerMinAge is the grace period before a freshly created agent
// bead is considered a leak. The at-spawn stamp is synchronous, so this is
// belt-and-braces: it keeps the sweep from racing any spawn path that stamps
// gardener_id a beat after the bead is created.
const defaultNeedsGardenerMinAge = 15 * time.Minute

// defaultNeedsGardenerSweepInterval is how often the sweep runs, independent of
// the reconcile loop's (faster) tick.
const defaultNeedsGardenerSweepInterval = 5 * time.Minute

// GardenerlessAgent is the minimal projection of an active agent bead the sweep
// reasons about. It is decoupled from beadsapi.AgentBead so the selection
// policy is trivially unit testable without a live daemon. HasRealGardener is
// pre-computed by the adapter (via beadsapi.IsSyntheticGardenerID) so the single
// definition of "real gardener vs synthetic/unowned" stays in beadsapi and is
// not duplicated here.
type GardenerlessAgent struct {
	ID string
	// HasRealGardener is true when the agent's derived gardener_id resolves to a
	// real gardener (trigger) bead — i.e. it is NOT a synthetic/unowned bucket.
	// An agent with a real gardener is owned and is never surfaced.
	HasRealGardener bool
	// Labels are the agent bead's current labels, used for idempotency (skip an
	// agent that already carries the needs-gardener label).
	Labels []string
	// CreatedAt is the agent bead's creation timestamp, used for the grace period.
	CreatedAt time.Time
}

// NeedsGardenerSweepConfig tunes needs-gardener selection.
type NeedsGardenerSweepConfig struct {
	// Label is applied to surfaced agents. Empty ⇒ DefaultNeedsGardenerLabel.
	Label string
	// MinAge is the grace period before an agent is eligible. <=0 ⇒ default; a
	// negative value disables the grace guard (surface immediately).
	MinAge time.Duration
}

func (c NeedsGardenerSweepConfig) withDefaults() NeedsGardenerSweepConfig {
	if strings.TrimSpace(c.Label) == "" {
		c.Label = DefaultNeedsGardenerLabel
	}
	if c.MinAge == 0 {
		c.MinAge = defaultNeedsGardenerMinAge
	}
	return c
}

// SelectGardenerless returns the active agents that should be surfaced for
// adoption: agents with no real gardener, older than MinAge, that don't already
// carry the label. Pure function — no I/O — so the routing policy is fully unit
// testable.
func SelectGardenerless(agents []GardenerlessAgent, cfg NeedsGardenerSweepConfig, now time.Time) []GardenerlessAgent {
	cfg = cfg.withDefaults()
	var out []GardenerlessAgent
	for _, a := range agents {
		if needsGardener(a, cfg, now) {
			out = append(out, a)
		}
	}
	return out
}

// needsGardener reports whether a single active agent is a gardener-ownership
// leak the sweep should surface.
func needsGardener(a GardenerlessAgent, cfg NeedsGardenerSweepConfig, now time.Time) bool {
	// Owned by a real gardener — not a leak.
	if a.HasRealGardener {
		return false
	}
	// Grace period: don't surface an agent a spawn path may still be stamping.
	// A negative MinAge disables this guard (surface immediately).
	if cfg.MinAge > 0 && !a.CreatedAt.IsZero() && now.Sub(a.CreatedAt) < cfg.MinAge {
		return false
	}
	// Idempotent: skip anything already surfaced.
	for _, l := range a.Labels {
		if strings.EqualFold(strings.TrimSpace(l), cfg.Label) {
			return false
		}
	}
	return true
}

// NeedsGardenerStore is the daemon surface the sweep needs. *beadsapi.Client is
// adapted to it at the wiring site; tests supply a fake.
type NeedsGardenerStore interface {
	// ListActiveAgents returns the active agent beads projected to
	// GardenerlessAgent (HasRealGardener pre-computed from the derived gardener_id).
	ListActiveAgents(ctx context.Context) ([]GardenerlessAgent, error)
	// AddLabel attaches label to the agent bead. Must be idempotent on the
	// daemon side (adding an existing label is a no-op).
	AddLabel(ctx context.Context, beadID, label string) error
}

// NeedsGardenerSweeper periodically surfaces gardener-ownership leaks by
// labelling the agent for adoption. Throttled so it runs at most once per
// interval regardless of how often the reconcile loop ticks. Not safe for
// concurrent Tick callers (driven from a single goroutine).
type NeedsGardenerSweeper struct {
	store    NeedsGardenerStore
	logger   *slog.Logger
	cfg      NeedsGardenerSweepConfig
	interval time.Duration
	lastRun  time.Time
}

// NewNeedsGardenerSweeper constructs a sweeper. interval <=0 ⇒ default cadence.
func NewNeedsGardenerSweeper(store NeedsGardenerStore, logger *slog.Logger, cfg NeedsGardenerSweepConfig, interval time.Duration) *NeedsGardenerSweeper {
	if interval <= 0 {
		interval = defaultNeedsGardenerSweepInterval
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &NeedsGardenerSweeper{
		store:    store,
		logger:   logger,
		cfg:      cfg.withDefaults(),
		interval: interval,
	}
}

// Tick runs a sweep if at least interval has elapsed since the last run. Safe
// to call on every (fast) reconcile tick; it self-throttles to the configured
// cadence.
func (s *NeedsGardenerSweeper) Tick(ctx context.Context, now time.Time) {
	if !s.lastRun.IsZero() && now.Sub(s.lastRun) < s.interval {
		return
	}
	s.lastRun = now
	s.Sweep(ctx, now)
}

// Sweep lists active agents, selects gardener-ownership leaks, and labels them
// for adoption. Returns the number of agents newly surfaced. A list failure is
// logged and yields 0; a per-agent label failure is logged and skipped so one
// bad agent can't stall the rest of the sweep.
func (s *NeedsGardenerSweeper) Sweep(ctx context.Context, now time.Time) int {
	agents, err := s.store.ListActiveAgents(ctx)
	if err != nil {
		s.logger.Error("needs-gardener-sweep: list active agents failed", "error", err)
		return 0
	}
	leaks := SelectGardenerless(agents, s.cfg, now)
	surfaced := 0
	for _, a := range leaks {
		if err := s.store.AddLabel(ctx, a.ID, s.cfg.Label); err != nil {
			s.logger.Warn("needs-gardener-sweep: add label failed",
				"agent", a.ID, "label", s.cfg.Label, "error", err)
			continue
		}
		surfaced++
		s.logger.Info("needs-gardener-sweep: surfaced gardenerless agent for adoption",
			"agent", a.ID, "label", s.cfg.Label)
	}
	if surfaced > 0 {
		s.logger.Info("needs-gardener-sweep: completed", "surfaced", surfaced, "candidates", len(leaks))
	}
	return surfaced
}
