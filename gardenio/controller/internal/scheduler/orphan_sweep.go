package scheduler

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"gardenio/clicreds/beadswire"
)

// Orphan triage sweep (ka-0000002988).
//
// One-shot planning gardeners run a decomposition cycle, file gate tasks and
// child epics, then terminate — their agent bead closes. Nothing carries that
// output forward: auto-assign is opt-in/off by default, and there is no
// server-side sweep for unassigned beads (the manual `kd orphans` command is
// about git-referenced beads and is never run automatically). Continuation
// depends entirely on a separate agent later happening to `gb ready` + claim
// the work, so planning-gardener output is silently orphaned on exit.
//
// This sweep closes that gap. On a throttled cadence it scans the daemon's
// ready view (open + unblocked work) for unassigned items and surfaces them
// for intake by applying a label (default "needs-intake"). Labelling — rather
// than auto-assigning or spawning — is the deliberately conservative choice:
// it is idempotent and reversible, has no blast radius beyond a queryable
// label, and complements (rather than competes with) the existing ready-queue
// gardeners, which can consume the label. Auto-assignment to a lane gardener is
// intentionally left as a follow-up that needs more design.

// DefaultIntakeLabel is the label the sweep applies to surface otherwise
// orphaned beads (e.g. gate tasks filed by a one-shot planning
// gardener that terminated without a hand-off actor).
const DefaultIntakeLabel = "needs-intake"

// defaultOrphanMinAge is the grace period before a freshly created bead is
// considered orphaned. It gives a live gardener (or its hand-off) time to claim
// or assign its own output before the sweep surfaces it, avoiding races with
// gardeners that file then immediately assign their decomposition output.
const defaultOrphanMinAge = 15 * time.Minute

// defaultOrphanSweepInterval is how often the sweep runs, independent of the
// reconcile loop's (faster) tick.
const defaultOrphanSweepInterval = 5 * time.Minute

// defaultOrphanDeLaneCooldown is how long the sweep backs off from re-surfacing
// a bead the intake-groomer recently de-laned (removed the intake label from).
// It is deliberately longer than defaultOrphanSweepInterval and — critically —
// than any plausible groomer cadence, so a de-lane gets a real head start: a
// ready-queue dispatcher can claim the bead before the sweep would re-lane it.
// While the groomer keeps re-de-laning an unclaimed bead each cycle the signal
// stays fresh and the sweep stays out entirely; once the groomer stops managing
// it, the last signal ages past the cooldown and the sweep resumes as a
// fallback. This closes the groomer↔sweeper fight-loop (ka-0000007370) for the
// unparented case; parented work is skipped outright by the parent guard.
//
// RESONANCE HAZARD (ka-0000008193): the cooldown MUST NOT equal (or closely
// approximate) the intake-groomer cron period. The groomer's own guards
// (HasParent, HasOpenChildren) exempt parented work and live tree-roots, so for
// a parentless bead whose children are ALL CLOSED — a drained plan-of-record
// task — this cooldown is the ONLY remaining brake on the loop. When it was set to
// exactly the groomer's 30-min cadence (`3,33 * * * *`), the cooldown expired
// precisely as the next groomer cycle began: groomer de-lanes → cooldown holds
// the sweep out for one period → cooldown lapses → sweeper re-lanes → groomer
// de-lanes again, forever, burning a full groomer cycle per period on one bead.
// A de-lane is a positive triage signal and deserves a backoff FAR longer than
// one groomer period, so the sweep only resumes long after the groomer has
// clearly stopped managing the bead. 4h is ~8× the current 30-min cadence, so
// the two periods cannot phase-lock even if the cron is retuned to a faster
// cadence; TestDeLaneCooldownExceedsGroomerCadence pins this margin.
const defaultOrphanDeLaneCooldown = 4 * time.Hour

// DeLaneMarker is the comment-text prefix the intake-groomer leaves when it
// de-lanes a bead (removes the intake label). It is a FALLBACK de-lane signal
// for the cooldown guard. Match is case-insensitive on the trimmed comment body
// (see OrphanStore.LastDeLane implementations).
//
// The PRIMARY, authoritative de-lane signal is the daemon's beads.label.removed
// EVENT for the intake label: every label removal (bare `kd label remove`, the
// RemoveLabel RPC, or the HTTP DELETE) records a durable event via
// recordAndPublish → RecordEvent, independent of whether the groomer also leaves
// a comment. LastDeLane takes the later of the two signals. Relying on the
// comment alone (its former sole signal) silently defeated the cooldown for the
// groomer's canonical no-comment de-lane and re-opened the orphan fight-loop
// (ka-0000007706).
const DeLaneMarker = "intake-groomed"

// ReLaneMarker is the comment-text prefix the OrphanSweeper leaves when it
// RE-surfaces a bead that carries a prior de-lane — i.e. a bead the
// intake-groomer previously de-laned, whose de-lane cooldown has since lapsed
// (ka-0000007945). Historically the sweeper re-added the intake label in code
// and left NO comment, so its re-laning was invisible to comment-based loop
// detection: the intake-groomer runbook's RE-SWEPT branch was keyed on a
// re-sweep comment that never existed for this path. Emitting a short
// structured comment on re-lane makes the sweeper's re-laning auditable and
// restores the runbook's comment-based detection. The comment is emitted ONLY
// for re-lanes (a bead with a prior de-lane) — a first-time orphan surfacing
// stays comment-free to avoid noise. Match is case-insensitive on the trimmed
// comment body, mirroring DeLaneMarker.
const ReLaneMarker = "orphan-sweep: re-laned"

// orphanEligibleTypes is the set of bead types the sweep treats as work items.
// Infrastructure beads (agent/config/schedule/trigger/decision/advice/...) are
// never surfaced — they are not "work nobody owns". Epics are not swept: the
// sweep runs as the trigger-engine machine principal, which holds no epic
// ownership (kbeads machine_grants.go, gardenio-machine-trigger-ops), so every
// label on an epic is a 403; a parentless epic with no open children is a
// finished plan for its gardener or the operator to close, not intake work
// (gd-10swg8vnzy).
var orphanEligibleTypes = map[string]bool{
	"task":    true,
	"bug":     true,
	"feature": true,
}

// releasePolicyLabels are the labels that put a bead in the OWNED release lane.
// A bead carrying one is never an orphan, however parentless it looks.
//
// Rationale (ka-0000002438): the sweep's premise — stated in the file header —
// is that labelling is safe because ready-queue gardeners "can consume the
// label". For a release bug both halves of that premise fail:
//
//   - It is already OWNED. The operator's always-P0 release policy (decision
//     ka-0000000928) makes gardener-release-owner (trigger ka-0000006579) drive
//     the fix, with gardener-bugfix (ka-0000001007) as its explicit safety net.
//   - The consuming gardeners SKIP this label rather than consume it —
//     gardener-bugfix's prompt reads "Anything still carrying `needs-intake` is
//     NOT yours to dispatch — leave it for the groomer", and its jq candidate
//     filter excludes the label outright. So surfacing a release bug here does
//     not route it for triage, it REMOVES it from the only lane that fixes it.
//
// These bugs are filed parentless by CI auto-filers (the RWX-era release.yml,
// gardenio/scripts/verify-deploy.sh) purely because a pipeline task has no epic
// to attach to — parentlessness is an artifact of the filer, not evidence that
// nobody owns the work. Same shape as the HasOpenChildren guard below: a bead
// that only LOOKS orphaned must not be surfaced.
//
// UPDATE (ka-0000002823, operator decision ka-0000002529 → parent-at-creation):
// the release.yml filer attached each auto-filed release bug to a
// standing release-failures epic via parent_bead_id at creation, so the HasParent
// guard above short-circuits first and these bugs are no longer parentless in the
// first place. This label guard is retained deliberately as defence in depth: it
// still covers the fail-open path (the filer files parentless when the epic
// cannot be resolved), any other/older filer, and release bugs opened by hand.
var releasePolicyLabels = map[string]bool{
	"release-pipeline":        true,
	"release-pipeline-broken": true,
	"release-regression":      true,
	"release-blocker":         true,
}

// runInstanceSourceLabels are the source labels the trigger engine stamps on
// per-cycle schedule/cron/event RUN-INSTANCE fire-record beads (see
// internal/bridge/triggers.go createTaskBead and the schedule/multi trigger
// sources; mirrors statusreporter.runInstanceSourceLabels). A bead carrying one
// is NOT orphaned planning-gardener output — it is a single-shot fire record
// whose lifecycle is owned end-to-end by the stale-reaper, never the intake
// lane:
//
//   - The reaper CLOSES stranded run-instances (in_progress with a gone assignee
//     via reconcileRunInstanceTasks, and never-claimed OPEN ones via
//     reconcileOpenRunInstanceTasks). That path is the fix of record for these
//     beads (ka-0000000075, enabled by default per ka-0000006054).
//   - Surfacing them into needs-intake is actively harmful: they are filed P0 for
//     no dispatch purpose, so a labelled fire record inflates every priority-sorted
//     view an agent reads (the RDY-4 priority-inflation violation, ka-0000007106),
//     and no consuming gardener can act on a fire record anyway.
//
// The orphan sweep was catching only ~86% of this shape by accident (the ready
// view's NoOpenDeps filter admits some run-instances and not others), leaving a
// P0 residue that is invisible to the drain lane it was never meant to enter.
// Skipping the whole class here removes that inconsistency at the source: none
// enter the lane, and the reaper drains all of them (ka-0000007106).
var runInstanceSourceLabels = map[string]bool{
	"source:schedule":        true, // ScheduleTriggerSource ("Schedule: ...")
	"source:multi":           true, // MultiTriggerSource ("Multi[cron]/Multi[bead_event]: ...")
	"source:schedule-mirror": true, // schedule-mirror run instances
}

// OrphanBead is the minimal projection of a ready bead the sweep reasons about.
// It is decoupled from beadsapi.Bead so the selection policy is trivially unit
// testable without a live daemon. Beads reach the sweep via the daemon's ready
// view, which already guarantees open + unblocked, so there is no blocked flag.
type OrphanBead struct {
	ID        string
	Type      string
	Status    string
	Assignee  string
	Labels    []string
	CreatedAt time.Time
	// HasParent is true when the bead has a parent-child owning epic/plan. Such
	// a bead is NOT orphaned — its parent has a live owning gardener that carries
	// it forward — so it is never re-surfaced (ka-0000007370). Populated lazily
	// by the sweeper for the candidate set only, never in the initial ready
	// projection (parent lookups cost extra daemon round-trips).
	HasParent bool
	// HasOpenChildren is true when the bead has at least one OPEN (non-terminal)
	// parent-child child — i.e. it is a tree-ROOT with live work hanging off it.
	// A parentless root (the check is type-agnostic) is legitimately parentless
	// BY DESIGN and is NOT an orphan: re-laning it fights whoever owns the
	// subtree and re-opens the janitor↔groomer fight-loop (decision
	// ka-0000002882 → Option A; code analog of the janitor prompt fix
	// ka-0000003783). A parentless bead whose children are all closed is still
	// an orphan. Populated lazily by the sweeper for the candidate set only (an
	// extra batched daemon round-trip).
	HasOpenChildren bool
	// DeLanedAt is the timestamp of the most recent intake-groomer de-lane
	// (an "intake-groomed" comment). Zero when the bead was never de-laned.
	// Feeds the de-lane cooldown guard so a fresh de-lane wins the race against
	// the sweep. Populated lazily by the sweeper for the candidate set only.
	DeLanedAt time.Time
}

// OrphanSweepConfig tunes orphan selection.
type OrphanSweepConfig struct {
	// IntakeLabel is applied to surfaced beads. Empty ⇒ DefaultIntakeLabel.
	IntakeLabel string
	// MinAge is the grace period before a bead is eligible. <=0 ⇒ default.
	MinAge time.Duration
	// DeLaneCooldown is how long after an intake-groomer de-lane the sweep
	// leaves a bead alone (see defaultOrphanDeLaneCooldown). Zero ⇒ default; a
	// negative value disables the cooldown guard entirely (parent guard only).
	DeLaneCooldown time.Duration
}

func (c OrphanSweepConfig) withDefaults() OrphanSweepConfig {
	if strings.TrimSpace(c.IntakeLabel) == "" {
		c.IntakeLabel = DefaultIntakeLabel
	}
	if c.MinAge <= 0 {
		c.MinAge = defaultOrphanMinAge
	}
	if c.DeLaneCooldown == 0 {
		c.DeLaneCooldown = defaultOrphanDeLaneCooldown
	}
	return c
}

// SelectOrphans returns the ready beads that should be surfaced for intake:
// unassigned work items older than MinAge that don't already carry the intake
// label. Pure function — no I/O — so the routing policy is fully unit testable.
// Callers must pass beads from the daemon's ready view (already open +
// unblocked); isOrphan additionally guards on open/ready status defensively.
func SelectOrphans(beads []OrphanBead, cfg OrphanSweepConfig, now time.Time) []OrphanBead {
	cfg = cfg.withDefaults()
	var out []OrphanBead
	for _, b := range beads {
		if isOrphan(b, cfg, now) {
			out = append(out, b)
		}
	}
	return out
}

// isOrphan reports whether a single ready bead is orphaned planning-gardener
// output that the sweep should surface.
func isOrphan(b OrphanBead, cfg OrphanSweepConfig, now time.Time) bool {
	status := strings.ToLower(strings.TrimSpace(b.Status))
	if status != "" && status != "open" && status != "ready" {
		return false // defensive: only open/ready work can be orphaned
	}
	if strings.TrimSpace(b.Assignee) != "" {
		return false // someone owns it
	}
	// A bead with a parent-child owning epic/plan is NOT orphaned: its parent
	// has a live owning gardener that carries the work forward, so re-surfacing
	// it fights whoever owns the epic — and, in practice, the intake-groomer
	// that de-lanes such children. The sweep's remit (per the file header) is
	// genuinely orphaned one-shot-gardener output: parent-less work nobody owns.
	// This guard closes the groomer↔sweeper fight-loop for decomposed plan/epic
	// children (ka-0000007370).
	if b.HasParent {
		return false
	}
	// A parentless bead with at least one OPEN parent-child child is a tree-ROOT
	// by design (e.g. a plan-of-record task with child steps) — not an orphan.
	// Its subtree is live work someone owns; re-laning the root fights whoever is
	// draining it and re-opens the janitor↔groomer fight-loop. Operator decision
	// ka-0000002882 → Option A; this is the code analog of the janitor prompt fix
	// (ka-0000003783). A parentless childless task or bug is still an orphan.
	if b.HasOpenChildren {
		return false
	}
	// A bead in the owned release lane is never an orphan — surfacing it would
	// take it OUT of the lane that fixes it (see releasePolicyLabels).
	for _, l := range b.Labels {
		if releasePolicyLabels[strings.ToLower(strings.TrimSpace(l))] {
			return false
		}
	}
	// A per-cycle run-instance fire record is never an orphan — it is owned by
	// the stale-reaper, not the intake lane (see runInstanceSourceLabels). The
	// sweep was catching only a subset of these by accident, leaving a P0
	// residue outside both the labeler and the drain lane (ka-0000007106).
	for _, l := range b.Labels {
		if runInstanceSourceLabels[strings.ToLower(strings.TrimSpace(l))] {
			return false
		}
	}
	if !orphanEligibleTypes[strings.ToLower(strings.TrimSpace(b.Type))] {
		return false // infrastructure bead, not work
	}
	// Grace period: don't surface work a live gardener may still be wiring up.
	if !b.CreatedAt.IsZero() && now.Sub(b.CreatedAt) < cfg.MinAge {
		return false
	}
	// Idempotent: skip anything already surfaced.
	for _, l := range b.Labels {
		if strings.EqualFold(strings.TrimSpace(l), cfg.IntakeLabel) {
			return false
		}
	}
	// De-lane cooldown: if the intake-groomer recently removed this bead's
	// intake label, let that de-lane win the race — a ready-queue dispatcher
	// gets a head start to claim it before the sweep would re-surface it. A
	// negative cooldown disables this guard (default is defaultOrphanDeLaneCooldown).
	// Guards the unparented case of the fight-loop (ka-0000007370); the silent
	// 5-min re-lane no longer undoes the groomer's de-lane.
	if cfg.DeLaneCooldown > 0 && !b.DeLanedAt.IsZero() && now.Sub(b.DeLanedAt) < cfg.DeLaneCooldown {
		return false
	}
	return true
}

// OrphanStore is the daemon surface the sweep needs. *beadsapi.Client is
// adapted to it at the wiring site; tests supply a fake.
type OrphanStore interface {
	// ListReady returns the daemon's ready view (open + unblocked) projected
	// to OrphanBead. HasParent/HasOpenChildren/DeLanedAt are left zero here —
	// the sweeper enriches them for the (small) candidate set only, so the extra
	// round-trips ParentedBeads/HasOpenChildren/LastDeLane require are not paid
	// for the whole ready view.
	ListReady(ctx context.Context) ([]OrphanBead, error)
	// AddLabel attaches label to the bead. Must be idempotent on the daemon
	// side (adding an existing label is a no-op).
	AddLabel(ctx context.Context, beadID, label string) error
	// AddComment leaves a short structured comment on the bead. Used only to
	// record a re-lane (ReLaneMarker) when the sweeper re-surfaces a bead that
	// carries a prior de-lane, so re-lanes are auditable and comment-based loop
	// detection works (ka-0000007945). Best-effort: a failure is logged and the
	// (already-applied) label stands.
	AddComment(ctx context.Context, beadID, text string) error
	// ParentedBeads returns the subset of beadIDs that have a parent-child
	// owning epic/plan (a forward parent-child dependency edge). Batched.
	ParentedBeads(ctx context.Context, beadIDs []string) (map[string]bool, error)
	// HasOpenChildren returns the subset of beadIDs that have at least one OPEN
	// (non-terminal) parent-child child — i.e. are tree-roots with live subtree
	// work. Such a root is parentless by design and must not be surfaced
	// (ka-0000003548). Batched. IDs with no open children are absent from the
	// map (callers treat absent as false).
	HasOpenChildren(ctx context.Context, beadIDs []string) (map[string]bool, error)
	// LastDeLane returns, per bead id that has one, the timestamp of the most
	// recent intake-groomer de-lane — the later of the authoritative
	// beads.label.removed EVENT for the intake label and the legacy fallback
	// intake-groomed comment (DeLaneMarker). IDs never de-laned are absent from
	// the map. Only called when the de-lane cooldown is active.
	LastDeLane(ctx context.Context, beadIDs []string) (map[string]time.Time, error)
}

// OrphanSweeper periodically surfaces orphaned planning-gardener output by
// labelling it for intake. Throttled so it runs at most once per interval
// regardless of how often the reconcile loop ticks. Not safe for concurrent
// Tick callers (driven from the single reconcile goroutine).
type OrphanSweeper struct {
	store    OrphanStore
	logger   *slog.Logger
	cfg      OrphanSweepConfig
	interval time.Duration
	lastRun  time.Time
	// denied holds beads whose AddLabel the daemon refused with a 403. The
	// sweep never retries them for the life of the process, so an ownership gap
	// costs one denial, not one per sweep (gd-10swg8vnzy).
	denied map[string]bool
}

// NewOrphanSweeper constructs a sweeper. interval <=0 ⇒ default cadence.
func NewOrphanSweeper(store OrphanStore, logger *slog.Logger, cfg OrphanSweepConfig, interval time.Duration) *OrphanSweeper {
	if interval <= 0 {
		interval = defaultOrphanSweepInterval
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &OrphanSweeper{
		store:    store,
		logger:   logger,
		cfg:      cfg.withDefaults(),
		interval: interval,
		denied:   map[string]bool{},
	}
}

// Tick runs a sweep if at least interval has elapsed since the last run. Safe
// to call on every (fast) reconcile tick; it self-throttles to the configured
// cadence.
func (s *OrphanSweeper) Tick(ctx context.Context, now time.Time) {
	if !s.lastRun.IsZero() && now.Sub(s.lastRun) < s.interval {
		return
	}
	s.lastRun = now
	s.Sweep(ctx, now)
}

// Sweep lists ready beads, selects orphans, and labels them for intake.
// Returns the number of beads newly surfaced. A list failure is logged and
// yields 0; a per-bead label failure is logged and skipped so one bad bead
// can't stall the rest of the sweep.
//
// Selection is two-pass. The first pass applies the cheap, self-contained
// predicates (unassigned, eligible type, aged, not-already-labelled) to narrow
// the ready view to the small set the sweep would otherwise label. Only that
// candidate set is then enriched with the signals that cost extra daemon
// round-trips — parent ownership, open-children (tree-root) status, and the
// last intake-groomer de-lane — before a second pass applies the parent,
// tree-root, and de-lane-cooldown guards. This keeps the (unbatchable) comment
// lookups off the whole ready view. Enrichment failure skips the sweep rather
// than risk re-laning owned work — the fight-loop this fixes (ka-0000007370)
// and the tree-root re-laning it fixes (ka-0000003548).
func (s *OrphanSweeper) Sweep(ctx context.Context, now time.Time) int {
	beads, err := s.store.ListReady(ctx)
	if err != nil {
		s.logger.Error("orphan-sweep: list ready beads failed", "error", err)
		return 0
	}
	// Pass 1: HasParent/DeLanedAt are still zero, so the parent and cooldown
	// guards are pass-throughs here; this yields the pre-enrichment candidates.
	candidates := SelectOrphans(beads, s.cfg, now)
	if len(candidates) == 0 {
		return 0
	}

	ids := make([]string, len(candidates))
	for i := range candidates {
		ids[i] = candidates[i].ID
	}
	parented, err := s.store.ParentedBeads(ctx, ids)
	if err != nil {
		s.logger.Warn("orphan-sweep: parent lookup failed; skipping sweep to avoid re-laning owned work", "error", err)
		return 0
	}
	withOpenChildren, err := s.store.HasOpenChildren(ctx, ids)
	if err != nil {
		s.logger.Warn("orphan-sweep: open-children lookup failed; skipping sweep to avoid re-laning tree-roots", "error", err)
		return 0
	}
	var delaned map[string]time.Time
	if s.cfg.DeLaneCooldown > 0 {
		delaned, err = s.store.LastDeLane(ctx, ids)
		if err != nil {
			s.logger.Warn("orphan-sweep: de-lane lookup failed; skipping sweep to avoid re-laning groomed work", "error", err)
			return 0
		}
	}
	for i := range candidates {
		candidates[i].HasParent = parented[candidates[i].ID]
		candidates[i].HasOpenChildren = withOpenChildren[candidates[i].ID]
		if delaned != nil {
			candidates[i].DeLanedAt = delaned[candidates[i].ID]
		}
	}
	// Pass 2: the enriched fields now let the parent and cooldown guards filter.
	orphans := SelectOrphans(candidates, s.cfg, now)
	surfaced := 0
	for _, b := range orphans {
		if s.denied[b.ID] {
			continue
		}
		if err := s.store.AddLabel(ctx, b.ID, s.cfg.IntakeLabel); err != nil {
			var apiErr *beadswire.APIError
			if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusForbidden {
				s.denied[b.ID] = true
			}
			s.logger.Warn("orphan-sweep: add intake label failed",
				"bead", b.ID, "label", s.cfg.IntakeLabel, "error", err)
			continue
		}
		surfaced++
		s.logger.Info("orphan-sweep: surfaced orphaned bead for intake",
			"bead", b.ID, "type", b.Type, "label", s.cfg.IntakeLabel)
		// A bead with a prior de-lane (DeLanedAt set) that we just RE-surfaced is
		// a re-lane, not a first-time surfacing. Leave a short structured comment
		// so the re-lane is auditable and comment-based loop detection works
		// (ka-0000007945) — historically this path left NO comment, so the
		// intake-groomer runbook could not distinguish a sweeper re-lane from a
		// janitor re-sweep. First-time surfacings stay comment-free to avoid
		// noise. Best-effort: a comment failure is logged; the label already
		// stands, so it must not undo the surfacing.
		if !b.DeLanedAt.IsZero() {
			comment := ReLaneMarker + " " + s.cfg.IntakeLabel
			if err := s.store.AddComment(ctx, b.ID, comment); err != nil {
				s.logger.Warn("orphan-sweep: re-lane comment failed",
					"bead", b.ID, "error", err)
			}
		}
	}
	if surfaced > 0 {
		s.logger.Info("orphan-sweep: completed",
			"surfaced", surfaced, "candidates", len(orphans))
	}
	return surfaced
}
