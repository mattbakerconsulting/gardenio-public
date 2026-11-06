package scheduler

import (
	"context"
	"log/slog"
	"strings"
	"time"
)

// Auto-intake sweep (ka-0000001368).
//
// The orphan sweep (orphan_sweep.go) is the sweep-IN half of the intake
// pipeline: it APPLIES the needs-intake label to genuinely orphaned,
// PARENTLESS one-shot-gardener output so nothing runs it unattended. The
// intended sweep-OUT half was an agent groomer (gardener-intake groomer,
// trigger ka-0000006210): a gardener that periodically stripped needs-intake
// from code-ready PLAN/EPIC CHILDREN so they became dispatchable without a
// human intake pass. That groomer is a live agent lineage — it goes dark, gets
// reaped, and stalls (it closed hard_stopped, leaving the lane dark, decision
// ka-0000002833). When it is dark, EVERY open child across the active /plans
// plans sits idle behind the needs-intake HARD-exclude, throttling fleet
// throughput — the exact pattern this feature was filed against.
//
// This sweep is the deterministic, agent-free complement: a server-side pass in
// trigger-engine that, on a throttled cadence, DE-LANES (strips needs-intake
// from) beads that pass a conservative safe-to-promote rubric. Because it is
// code — not a spawned agent — it cannot go dark, be reaped, or stall the way
// the groomer did: as long as trigger-engine runs, the sweep-OUT half runs.
//
// Disjoint-by-construction with the orphan sweep. The two halves can never
// fight over the same bead (the janitor↔groomer fight-loop, ka-0000007370)
// because they operate on DISJOINT populations keyed on the SAME discriminator:
//
//   - The orphan sweep only LABELS work with NO parent (its HasParent guard
//     skips parented beads outright).
//   - The auto-intake sweep only DE-LANES work WITH a parent (a plan/epic
//     child) that ALSO satisfies the rest of the rubric.
//
// A bead is therefore eligible for exactly one of the two sweeps at any moment,
// so there is no bead the orphan sweep re-labels moments after this sweep
// strips it. (Parentless orphans stay human-gated — the operator, not this
// sweep, promotes them.)
//
// Safe-to-promote rubric (all must hold), mirroring the task's acceptance list
// and the retired groomer's charter:
//
//   (a) carries the intake label (needs-intake) — nothing else is a candidate;
//   (b) type is a code/docs/test WORK type (task/bug/feature) — infra beads and
//       container epics are never auto-promoted;
//   (c) parented by an epic/plan (HasParent) — this is the plan-child signal
//       AND the disjointness guard against the orphan sweep;
//   (d) dependency-unblocked — guaranteed by sourcing candidates from the
//       daemon's no_open_deps ready view (a parent-child edge to an open epic
//       is NOT a blocking dep, so plan children still appear);
//   (e) unassigned — a bead someone already owns needs no de-lane;
//   (f) NOT Slack-thread-sourced — a bead the label reached via a human Slack
//       thread stays human-owned; only labels applied by the generic automated
//       sweep are promoted (see slackSourceLabelSubstrings).
//
// Stripping the label (rather than swapping it for a "triaged" marker) is the
// minimal reversible action: it is exactly the operation the ready-queue
// dispatch filter keys off, it is idempotent (re-removing an absent label is a
// daemon no-op), and it needs no new label vocabulary. A human can always
// re-apply needs-intake to re-gate a bead.

// autoIntakePromotableTypes is the set of bead types the auto-intake sweep will
// de-lane. Only concrete code/docs/test WORK belongs here: epics are excluded
// deliberately — a container epic carrying needs-intake is a scoping unit, not a
// unit of dispatchable code work. It matches orphanEligibleTypes, which also
// leaves epics out (gd-10swg8vnzy).
var autoIntakePromotableTypes = map[string]bool{
	"task":    true,
	"bug":     true,
	"feature": true,
}

// slackSourceLabelSubstrings are label fragments that mark a bead whose
// intake gating originated from a human Slack thread / mention rather than the
// generic automated orphan sweep. Any bead carrying such a label is left
// human-owned and is NEVER auto-promoted, honouring the task's rubric point (d)
// ("NOT source:slack / thread / slack-thread"). Matched case-insensitively as a
// substring of the trimmed label so both the "source:slack…" family and the
// slack-bridge conversation labels (slack-thread-reply / slack-mention /
// slack-thread-message; see beads-viewer chat_message.go) are covered by one
// list without coupling to that package.
var slackSourceLabelSubstrings = []string{
	"source:slack",
	"slack-thread",
	"slack-mention",
}

// isSlackSourced reports whether any of a bead's labels marks it as
// Slack-thread-sourced (and therefore human-owned, never auto-promoted).
func isSlackSourced(labels []string) bool {
	for _, l := range labels {
		ll := strings.ToLower(strings.TrimSpace(l))
		for _, sub := range slackSourceLabelSubstrings {
			if strings.Contains(ll, sub) {
				return true
			}
		}
	}
	return false
}

// defaultAutoIntakeMinAge is the grace period before a freshly-labelled bead is
// eligible for de-lane. It gives a human (or a downstream triage step) a window
// to re-gate or re-scope a bead the orphan sweep just surfaced before this sweep
// promotes it, and — combined with the orphan sweep's own 15-min surfacing
// grace — makes an immediate label→de-lane round-trip impossible. Keyed on the
// intake label's own age (LabeledAt), not the bead's creation time: what matters
// is how long the bead has carried needs-intake, not how old the bead is.
const defaultAutoIntakeMinAge = 30 * time.Minute

// defaultAutoIntakeSweepInterval is how often the auto-intake sweep runs,
// independent of the (faster) reconcile / poll tick. Matches the retired
// groomer's ~30-min cadence: brisk enough that a code-ready plan child is
// dispatchable within one governor cycle, slow enough that the candidate scan
// is negligible load.
const defaultAutoIntakeSweepInterval = 5 * time.Minute

// IntakeBead is the minimal projection of a needs-intake bead the auto-intake
// sweep reasons about. Decoupled from beadsapi.Bead so the promotion policy is
// trivially unit-testable without a live daemon.
type IntakeBead struct {
	ID       string
	Type     string
	Status   string
	Assignee string
	Labels   []string
	// HasParent is true when the bead has a parent-child owning epic/plan — the
	// plan-child signal AND the disjointness guard against the orphan sweep. A
	// needs-intake bead with NO parent is a genuine parentless orphan and stays
	// human-gated; only PARENTED plan/epic children are auto-promoted. Populated
	// lazily by the sweeper for the candidate set only (an extra batched daemon
	// round-trip), so the cheap self-contained predicates filter first.
	HasParent bool
	// LabeledAt is the timestamp the intake label was most recently added to the
	// bead (the beads.label.added event for the intake label). Zero when unknown
	// — treated as "old enough" so a missing signal never permanently pins a
	// bead in the lane. Feeds the min-age grace guard. Populated lazily for the
	// candidate set only.
	LabeledAt time.Time
}

// AutoIntakeSweepConfig tunes auto-intake promotion.
type AutoIntakeSweepConfig struct {
	// IntakeLabel is the label the sweep strips. Empty ⇒ DefaultIntakeLabel
	// (shared with the orphan sweep so the two halves key off the same label).
	IntakeLabel string
	// MinAge is the grace period since the intake label was applied before a
	// bead is eligible for de-lane. <=0 ⇒ defaultAutoIntakeMinAge. A negative
	// value disables the grace guard entirely.
	MinAge time.Duration
}

func (c AutoIntakeSweepConfig) withDefaults() AutoIntakeSweepConfig {
	if strings.TrimSpace(c.IntakeLabel) == "" {
		c.IntakeLabel = DefaultIntakeLabel
	}
	if c.MinAge == 0 {
		c.MinAge = defaultAutoIntakeMinAge
	}
	return c
}

// SelectPromotable returns the needs-intake beads that satisfy the
// safe-to-promote rubric and should be de-laned. Pure function — no I/O — so the
// promotion policy is fully unit-testable. Callers pass beads carrying the
// intake label; isPromotable applies every rubric guard.
func SelectPromotable(beads []IntakeBead, cfg AutoIntakeSweepConfig, now time.Time) []IntakeBead {
	cfg = cfg.withDefaults()
	var out []IntakeBead
	for _, b := range beads {
		if isPromotable(b, cfg, now) {
			out = append(out, b)
		}
	}
	return out
}

// passesCheapPredicates reports whether a bead satisfies the self-contained
// rubric guards that need NO enrichment (open, unassigned, promotable type,
// carries the intake label, not Slack-sourced). These are exactly the guards
// the sweep's PASS 1 applies to narrow the intake view before paying for the
// parent + label-age lookups. Split out from the parent/grace guards precisely
// because the two-pass pattern requires pass 1 to run WITHOUT the enrichment
// fields set — unlike the orphan sweep, whose guards are all "reject if X" and
// so pass through when X is the unenriched zero value.
func passesCheapPredicates(b IntakeBead, cfg AutoIntakeSweepConfig) bool {
	status := strings.ToLower(strings.TrimSpace(b.Status))
	if status != "" && status != "open" && status != "ready" {
		return false // defensive: only open/ready work is dispatchable
	}
	if strings.TrimSpace(b.Assignee) != "" {
		return false // (e) already owned — no de-lane needed
	}
	// (b) only concrete code/docs/test WORK types are auto-promoted; container
	// epics and infra beads are never touched.
	if !autoIntakePromotableTypes[strings.ToLower(strings.TrimSpace(b.Type))] {
		return false
	}
	// (a) must actually carry the intake label — nothing else is a candidate.
	// Also makes the de-lane idempotent: a bead already stripped is skipped.
	if !hasLabel(b.Labels, cfg.IntakeLabel) {
		return false
	}
	// (f) Slack-thread-sourced beads stay human-owned.
	if isSlackSourced(b.Labels) {
		return false
	}
	return true
}

// isPromotable reports whether a single needs-intake bead satisfies the FULL
// safe-to-promote rubric, including the guards that depend on enrichment
// (parent ownership, label age). This is the complete policy the sweep's PASS 2
// applies after enrichment, and the pure policy unit tests assert against. See
// the file header for the rubric and its rationale.
func isPromotable(b IntakeBead, cfg AutoIntakeSweepConfig, now time.Time) bool {
	if !passesCheapPredicates(b, cfg) {
		return false
	}
	// (c) parented by an epic/plan. This is BOTH the plan-child signal and the
	// disjointness guard against the orphan sweep (which only labels PARENTLESS
	// work) — so the two halves can never fight over the same bead.
	if !b.HasParent {
		return false
	}
	// Grace: don't promote a bead the orphan sweep only just surfaced; give a
	// human / triage step a window to re-gate it first. A zero LabeledAt is
	// treated as old enough so a missing signal never pins a bead forever.
	if cfg.MinAge > 0 && !b.LabeledAt.IsZero() && now.Sub(b.LabeledAt) < cfg.MinAge {
		return false
	}
	return true
}

// hasLabel reports whether labels contains target (case-insensitive, trimmed).
func hasLabel(labels []string, target string) bool {
	for _, l := range labels {
		if strings.EqualFold(strings.TrimSpace(l), strings.TrimSpace(target)) {
			return true
		}
	}
	return false
}

// AutoIntakeStore is the daemon surface the auto-intake sweep needs.
// *beadsapi.Client is adapted to it at the wiring site; tests supply a fake.
type AutoIntakeStore interface {
	// ListIntake returns the daemon's ready view (open + unblocked) restricted
	// to beads carrying the intake label, projected to IntakeBead. HasParent /
	// LabeledAt are left zero here — the sweeper enriches them for the (small)
	// candidate set only, so the extra round-trips ParentedBeads / LabelAddedAt
	// require are not paid for the whole intake view.
	ListIntake(ctx context.Context) ([]IntakeBead, error)
	// RemoveLabel strips label from the bead. Must be idempotent on the daemon
	// side (removing an absent label is a no-op).
	RemoveLabel(ctx context.Context, beadID, label string) error
	// ParentedBeads returns the subset of beadIDs that have a parent-child
	// owning epic/plan (a forward parent-child dependency edge). Batched.
	ParentedBeads(ctx context.Context, beadIDs []string) (map[string]bool, error)
	// LabelAddedAt returns, per bead id, the timestamp the intake label was most
	// recently ADDED (the latest beads.label.added event for the intake label).
	// IDs with no such event are absent from the map (treated as zero ⇒ old
	// enough). Only called when the min-age grace guard is active.
	LabelAddedAt(ctx context.Context, beadIDs []string) (map[string]time.Time, error)
}

// AutoIntakeSweeper periodically de-lanes code-ready plan/epic children by
// stripping the intake label so they become dispatchable without a human intake
// pass. Throttled so it runs at most once per interval regardless of how often
// the poll loop ticks. Not safe for concurrent Tick callers (driven from a
// single goroutine).
type AutoIntakeSweeper struct {
	store    AutoIntakeStore
	logger   *slog.Logger
	cfg      AutoIntakeSweepConfig
	interval time.Duration
	lastRun  time.Time
}

// NewAutoIntakeSweeper constructs a sweeper. interval <=0 ⇒ default cadence.
func NewAutoIntakeSweeper(store AutoIntakeStore, logger *slog.Logger, cfg AutoIntakeSweepConfig, interval time.Duration) *AutoIntakeSweeper {
	if interval <= 0 {
		interval = defaultAutoIntakeSweepInterval
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &AutoIntakeSweeper{
		store:    store,
		logger:   logger,
		cfg:      cfg.withDefaults(),
		interval: interval,
	}
}

// Tick runs a sweep if at least interval has elapsed since the last run. Safe
// to call on every (fast) poll tick; it self-throttles to the configured
// cadence.
func (s *AutoIntakeSweeper) Tick(ctx context.Context, now time.Time) {
	if !s.lastRun.IsZero() && now.Sub(s.lastRun) < s.interval {
		return
	}
	s.lastRun = now
	s.Sweep(ctx, now)
}

// Sweep lists needs-intake beads, selects the safe-to-promote subset, and
// strips the intake label from each. Returns the number of beads de-laned. A
// list failure is logged and yields 0; a per-bead label-remove failure is
// logged and skipped so one bad bead can't stall the rest of the sweep.
//
// Selection is two-pass, mirroring the orphan sweep. The first pass applies the
// cheap self-contained predicates (open, unassigned, promotable type, carries
// the label, not Slack-sourced) to narrow the intake view to the small set the
// sweep would otherwise de-lane. Only that candidate set is enriched with the
// signals that cost extra daemon round-trips — parent ownership and the label's
// add time — before a second pass applies the parent and grace guards. This
// keeps the parent lookup and (unbatchable) event lookups off the whole intake
// view. Enrichment failure skips the sweep rather than risk promoting an
// unparented orphan the orphan sweep owns.
func (s *AutoIntakeSweeper) Sweep(ctx context.Context, now time.Time) int {
	beads, err := s.store.ListIntake(ctx)
	if err != nil {
		s.logger.Error("auto-intake-sweep: list intake beads failed", "error", err)
		return 0
	}
	// Pass 1: apply only the cheap self-contained predicates (the parent and
	// grace guards need enrichment that has not been fetched yet), narrowing the
	// intake view to the small candidate set the sweep would otherwise de-lane.
	var candidates []IntakeBead
	for _, b := range beads {
		if passesCheapPredicates(b, s.cfg) {
			candidates = append(candidates, b)
		}
	}
	if len(candidates) == 0 {
		return 0
	}

	ids := make([]string, len(candidates))
	for i := range candidates {
		ids[i] = candidates[i].ID
	}
	parented, err := s.store.ParentedBeads(ctx, ids)
	if err != nil {
		s.logger.Warn("auto-intake-sweep: parent lookup failed; skipping sweep to avoid promoting unparented orphans", "error", err)
		return 0
	}
	var labeledAt map[string]time.Time
	if s.cfg.MinAge > 0 {
		labeledAt, err = s.store.LabelAddedAt(ctx, ids)
		if err != nil {
			s.logger.Warn("auto-intake-sweep: label-age lookup failed; skipping sweep to respect the promotion grace window", "error", err)
			return 0
		}
	}
	for i := range candidates {
		candidates[i].HasParent = parented[candidates[i].ID]
		if labeledAt != nil {
			candidates[i].LabeledAt = labeledAt[candidates[i].ID]
		}
	}
	// Pass 2: the enriched fields now let the parent and grace guards filter.
	promotable := SelectPromotable(candidates, s.cfg, now)
	deLaned := 0
	for _, b := range promotable {
		if err := s.store.RemoveLabel(ctx, b.ID, s.cfg.IntakeLabel); err != nil {
			s.logger.Warn("auto-intake-sweep: remove intake label failed",
				"bead", b.ID, "label", s.cfg.IntakeLabel, "error", err)
			continue
		}
		deLaned++
		autoIntakeDeLanedTotal.Inc()
		s.logger.Info("auto-intake-sweep: de-laned code-ready plan child for dispatch",
			"bead", b.ID, "type", b.Type, "label", s.cfg.IntakeLabel)
	}
	// Publish the residual lane-depth metric the feature asked for: how many
	// beads still carry the intake label after this pass (the lane's steady-state
	// backlog). Set every sweep so a stuck lane is graphable even when nothing
	// was de-laned. len(beads)-deLaned is the post-sweep count without a second
	// list round-trip.
	remaining := len(beads) - deLaned
	if remaining < 0 {
		remaining = 0
	}
	autoIntakeLaneDepth.Set(float64(remaining))
	if deLaned > 0 {
		s.logger.Info("auto-intake-sweep: completed",
			"de_laned", deLaned, "candidates", len(promotable), "lane_depth", remaining)
	}
	return deLaned
}
