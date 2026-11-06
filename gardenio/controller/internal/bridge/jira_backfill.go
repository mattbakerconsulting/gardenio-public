package bridge

// jira_backfill.go — the one-shot FSM→GBFSM backfill job, DRY-RUN / LOG-ONLY
// slice (epic ka-0000002637, Phase 5a ka-0000000123; the live run is Phase 6
// ka-0000000885 — Phase 5 ka-0000006784, which originally owned it, is now closed).
//
// # What this is
//
// Existing FSM tickets triaged before the 1:1 reshape have no GBFSM counterpart.
// Phase 5 seeds ONE GBFSM ticket per in-scope FSM ticket so the 1:1 correspondence
// is complete, not just forward-going. This file is the AUTHOR-AHEAD first step of
// that job: it enumerates the in-scope FSM backlog READ-ONLY and, for each FSM
// ticket, computes what the merged Phase 2 writer WOULD upsert — and LOGS it.
//
// # Why it is safe to land now (no Phase-4 / no-pipeline dependency)
//
// Phase 5's own description authorizes the dry-run first ("dry-run first (log-only),
// then live once rules (Phase 4) are green"). This slice is LOG-ONLY and makes ZERO
// Jira contact of any kind:
//
//   - It does NOT search GBFSM, create, or edit — it never calls a JiraClient
//     mutation (nor even the read-only SearchGBFSM). The only JiraClient method it
//     touches is BrowseURL, which builds a browse-URL STRING locally (no HTTP).
//   - It reuses JiraSync.buildGBFSMTicket — the SAME pure builder the live upsert
//     uses — so the dry-run preview is exactly the ticket the live run would upsert,
//     with no second, drift-prone code path. buildGBFSMTicket performs no I/O.
//   - The LIVE run (dry-run → live flip) is explicitly DEFERRED to Phase 6
//     (ka-0000000885), which is gated on Phase 4 operator rules + a green E2E
//     pipeline. Run() refuses to proceed when DryRun is false so a premature live
//     flip fails loud, here.
//
// # Scope (do NOT widen)
//
// The in-scope FSM backlog is the set of FSM-sourced beads the triage pipeline
// already materialized: kind=issue, carrying source:jira, whose BOUND Jira key is an
// FSM key (jiraProjectKey(jira_key)==FSM). The backfill does not run its own JQL and
// PLANS a ticket only for FSM-bound beads; dark-mirrored beads (source:jira-dark) are
// excluded exactly as the sync-back path excludes them — they never flow upstream.
//
// # Scoping by the BOUND KEY, not the jira_project field (ka-0000006784)
//
// The FSM population is identified by the same signal the LIVE sync path uses to
// recognize an FSM bead — its bound Jira issue key (jiraKeyFromBead → jira_key field
// / jira:<KEY> label), whose project prefix is FSM. It is deliberately NOT scoped on
// the daemon `jira_project` FIELD: only the POLLER stamps jira_project
// (jira_poller.go), and FSM does not enter this fleet via the poller — FSM tickets
// are materialized by a `spawn_agent` JIRA trigger (trigger_source_jira.go), which
// stamps jira_key / jira:<KEY> but NOT jira_project. Scoping on jira_project=FSM
// therefore matched ZERO real FSM beads (all 160+ carry the FSM key but no
// jira_project field) and silently under-covered the entire backlog. Scoping on the
// bound-key prefix is robust to that ingestion asymmetry and mirrors live-path
// identification exactly, so the preview covers the true FSM backlog.

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"gardenio/controller/internal/beadsapi"
)

// GBFSMBackfillDaemon is the minimal beads-daemon surface the backfill needs: a
// READ-ONLY paginated list of beads. Kept small so the enumeration is unit-testable
// with a fake and so the backfill can NEVER mutate a bead (it has no write method).
type GBFSMBackfillDaemon interface {
	ListBeadsFiltered(ctx context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error)
}

// fsmBackfillProjectKey is the FSM Jira project the backfill scopes to — the
// customer service desk whose backlog is being back-seeded. Matches the sync-back
// path's fsmProjectKey ("FSM"). An in-scope bead is recognized by its BOUND Jira
// key's project prefix (jiraProjectKey(jira_key)==FSM), NOT a jira_project field —
// see the "Scoping by the BOUND KEY" note above (ka-0000006784).
const fsmBackfillProjectKey = fsmProjectKey

// backfillPageSize is the per-request page size for the read-only enumeration.
// The backlog is bounded (a service desk, not the whole of Jira), but paginate
// anyway so a large backlog is walked completely rather than silently truncated.
const backfillPageSize = 500

// fsmSourcedBeadStatuses is the authoritative set of statuses an FSM-sourced
// (source:jira, kind=issue) bead can hold — the population oracle shared by the
// JIRA poller's catch-up (jira_poller.go CatchUp) and this backfill's enumeration.
// Matching it makes the backlog the backfill previews EXACTLY the population the
// poller materialized. Do NOT narrow it to jira_sync.go CatchUp's 3-status set:
// that query is a project-agnostic dedup-marker pre-load with different
// completeness needs, and dropping deferred/blocked would silently under-cover the
// backlog — a blocked or deferred FSM ticket would get no GBFSM counterpart,
// defeating the epic's goal of a COMPLETE 1:1 correspondence (ka-0000002637).
// Both call sites reference THIS variable, so the two cannot drift.
var fsmSourcedBeadStatuses = []string{"open", "in_progress", "closed", "deferred", "blocked"}

// GBFSMBackfillConfig configures the one-shot backfill.
type GBFSMBackfillConfig struct {
	// Sync is the wired JiraSync whose buildGBFSMTicket + gates + issue-type /
	// labels-field / version-field ids define the GBFSM ticket shape. Reusing it
	// guarantees the preview equals what the live upsert would write. Required.
	Sync *JiraSync
	// Daemon is the read-only beads source. Required.
	Daemon GBFSMBackfillDaemon
	// Logger receives the per-ticket dry-run plan + the roll-up. Required.
	Logger *slog.Logger
	// DryRun MUST be true in this slice — the only mode wired here. false returns
	// an error from Run (the live flip is deferred to Phase 6, ka-0000000885, gated on
	// Phase 4 + a green E2E pipeline). Default false ⇒ Run is inert unless a caller
	// explicitly opts into dry-run, so the surface is off on merge.
	DryRun bool
	// PageSize overrides backfillPageSize (tests). <=0 uses the default.
	PageSize int
}

// GBFSMBackfill enumerates the in-scope FSM backlog and logs the GBFSM ticket each
// FSM ticket WOULD upsert. It never writes to Jira and never mutates a bead.
type GBFSMBackfill struct {
	sync     *JiraSync
	daemon   GBFSMBackfillDaemon
	logger   *slog.Logger
	dryRun   bool
	pageSize int
}

// NewGBFSMBackfill builds a backfill from config, applying defaults.
func NewGBFSMBackfill(cfg GBFSMBackfillConfig) *GBFSMBackfill {
	pageSize := cfg.PageSize
	if pageSize <= 0 {
		pageSize = backfillPageSize
	}
	return &GBFSMBackfill{
		sync:     cfg.Sync,
		daemon:   cfg.Daemon,
		logger:   cfg.Logger,
		dryRun:   cfg.DryRun,
		pageSize: pageSize,
	}
}

// BackfillStats is the roll-up tally of one backfill pass, returned for logging
// and test assertions.
type BackfillStats struct {
	Scanned        int // source:jira issue beads examined
	Planned        int // FSM-bound beads that produced a would-upsert GBFSM ticket
	SkippedKey     int // skipped: no resolvable Jira target key (unbound bead)
	SkippedDark    int // skipped: source:jira-dark (never flows upstream)
	SkippedProject int // skipped: bound key is not an FSM key (e.g. PE-/DEVOPS-)
}

// Run performs the DRY-RUN backfill: enumerate the in-scope FSM backlog read-only
// and LOG the GBFSM ticket each FSM ticket would upsert. It makes ZERO Jira calls.
//
// Fail-loud on a live flip: this slice wires ONLY the dry-run, so Run returns an
// error when DryRun is false — the live run is deferred to Phase 6 (ka-0000000885),
// gated on Phase 4 operator rules + a green E2E pipeline. It also errors on a
// missing collaborator rather than silently no-op'ing.
func (b *GBFSMBackfill) Run(ctx context.Context) (BackfillStats, error) {
	var stats BackfillStats
	if b.sync == nil || b.daemon == nil || b.logger == nil {
		return stats, fmt.Errorf("gbfsm-backfill: Sync, Daemon and Logger are required")
	}
	if !b.dryRun {
		return stats, fmt.Errorf("gbfsm-backfill: only DRY-RUN is wired in this slice (Phase 5a) — " +
			"the live backfill run is deferred to Phase 6 (ka-0000000885), gated on Phase 4 rules + a green E2E pipeline")
	}

	// Honestly log the outbox readiness at preview time. The dry-run is decoupled
	// from JIRA_OUTBOX_ENABLED on purpose (an operator can preview BEFORE arming),
	// so surface whether the surface it previews is actually wired: with no
	// Analysis issue type configured the LIVE run would create nothing, and with
	// the outbox toggle off the LIVE run would skip. Neither affects the dry-run
	// (which never writes), but the operator should see the state they'd flip into.
	b.logger.Info("gbfsm-backfill: starting DRY-RUN (log-only, zero Jira writes)",
		"fsm_project", fsmBackfillProjectKey,
		"outbox_analysis_type_configured", b.sync.analysisIssueType != "",
		"outbox_writable_now", b.sync.allowProject(outboxProjectKey),
		"page_size", b.pageSize)

	offset := 0
	for {
		// Enumerate ALL FSM-sourced issue beads (kind=issue, source:jira). The FSM
		// project scoping is applied per-bead in planOne via the bound Jira key's
		// prefix — NOT a jira_project field filter, which the trigger ingestion path
		// never stamps (see the "Scoping by the BOUND KEY" note above, ka-0000006784).
		// A non-FSM-bound bead (PE-/DEVOPS-) is tallied Scanned but SkippedProject.
		page, err := b.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
			Kinds:    []string{"issue"},
			Statuses: fsmSourcedBeadStatuses,
			Labels:   []string{"source:jira"},
			Limit:    b.pageSize,
			Offset:   offset,
		})
		if err != nil {
			return stats, fmt.Errorf("gbfsm-backfill: listing FSM backlog (offset %d): %w", offset, err)
		}
		if page == nil || len(page.Beads) == 0 {
			break
		}

		for _, detail := range page.Beads {
			stats.Scanned++
			b.planOne(detail, &stats)
		}

		// Advance until a short page (fewer than requested) signals the last page.
		if len(page.Beads) < b.pageSize {
			break
		}
		offset += len(page.Beads)
	}

	b.logger.Info("gbfsm-backfill: DRY-RUN complete (no Jira writes performed)",
		"scanned", stats.Scanned, "planned", stats.Planned,
		"skipped_no_key", stats.SkippedKey, "skipped_dark", stats.SkippedDark,
		"skipped_non_fsm", stats.SkippedProject)
	return stats, nil
}

// planOne computes and logs the would-upsert GBFSM ticket for a single FSM bead.
// It performs NO write: buildGBFSMTicket is a pure builder and this only logs.
func (b *GBFSMBackfill) planOne(detail *beadsapi.BeadDetail, stats *BackfillStats) {
	bead := beadDetailToEvent(detail)
	if bead == nil {
		return
	}

	// Exclude dark-mirrored beads exactly as the sync-back path does — they mirror
	// the in-gardenio "dark" universe and must never flow upstream to real Jira.
	if hasLabel(bead.Labels, jiraDarkLabel) {
		stats.SkippedDark++
		return
	}

	// The FSM target key is the bead's bound Jira issue — DATA only, exactly the
	// fsm_target_key the live upsert carries. An unbound bead has nothing to seed.
	fsmTargetKey := boundJiraKeyFromFields(bead.Fields, bead.Labels)
	if strings.TrimSpace(fsmTargetKey) == "" {
		stats.SkippedKey++
		b.logger.Warn("gbfsm-backfill: skipping bead with no bound jira_key",
			"bead", bead.ID)
		return
	}

	// FSM-project scoping is applied HERE, on the bound key's project prefix — the
	// same signal the live sync path uses to recognize an FSM bead — rather than via
	// a daemon jira_project field filter the trigger ingestion path never stamps
	// (ka-0000006784). A source:jira issue bead bound to a non-FSM ticket (PE-/DEVOPS-,
	// materialized by the poller into the SAME daemon) is out of scope: tally it and
	// skip. This keeps the backfill scoped to the FSM backlog without a field the
	// FSM population does not carry.
	if !strings.EqualFold(jiraProjectKey(fsmTargetKey), fsmBackfillProjectKey) {
		stats.SkippedProject++
		return
	}

	// Gate the classification under the canonical triageRole. The GBFSM outbox
	// exists solely to carry the fsm-triage classification (labels + version), and
	// the LIVE path fires syncGBFSMTicket ONLY when role == triageRole. A historical
	// backlog bead may have lost its fsm-triage assignee (agent despawned, bead
	// closed), so resolving the role from the current assignee would fail-closed and
	// silently drop the very classification the backfill exists to seed. triageRole
	// is what the live run would gate this write under once re-triaged, so it is the
	// faithful preview role.
	role := triageRole
	// The SAME pure builder the live upsert uses (JiraSync.buildGBFSMTicket): the
	// preview is exactly the GBFSM ticket the live run would upsert. No Jira I/O.
	ticket := b.sync.buildGBFSMTicket(bead, fsmTargetKey, role)

	stats.Planned++
	b.logger.Info("gbfsm-backfill: WOULD upsert GBFSM ticket (dry-run — not written)",
		"bead", bead.ID,
		"fsm_target_key", ticket.FSMTargetKey,
		"role", role,
		"issue_type", ticket.IssueTypeID,
		"summary", ticket.Summary,
		"labels", ticket.Labels,
		"affects_version", ticket.Version,
		"dedup_key", ticket.DedupKey(),
		"outbox_project", outboxProjectKey)
}
