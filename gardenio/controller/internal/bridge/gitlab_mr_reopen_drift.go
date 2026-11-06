package bridge

// gitlab_mr_reopen_drift.go — reconcile sweep that re-opens closed issue beads
// whose linked MR is NOT merged (ka-0000000267, direction 2 of ka-0000003953).
//
// The close-time guard (kd close's --allow-open-mr refusal, MR !6190) prevents
// NEW open-MR-close drift, but it cannot repair beads that ALREADY drifted:
// once a bead is closed, nothing in the lane — no gardener dispatch, no
// ready-queue, no orphan sweep — ever looks at it again, so a real fix whose
// bead was closed while its MR sat unmerged is one stale branch away from
// silent deletion (the exact ~50min near-loss of MR !6183).
//
// This sweep closes that gap. On an interval it enumerates the tracked GitLab
// group's currently-`opened` MRs — GitLab is the ground truth, so we never
// trust the issue bead's own mr_state, which is stale/unset by design (advice
// ka-0000000519, bug ka-0000003953). For each open MR it finds the CLOSED issue
// bead(s) carrying that mr_url and re-opens them, leaving an explanatory comment
// so the gardener lane picks the work back up (merge the MR or abandon it
// deliberately). It does NOT merge — a reconciler must not write to main; it
// only heals the ledger so the drift becomes visible again.
//
// Why enumerate open MRs instead of scanning closed beads:
//   - It is the cheap direction. The group has O(dozens) of open MRs at any
//     time; scanning every closed bead with an mr_url would be O(thousands).
//   - It is staleness-proof. `opened` on GitLab is authoritative; the bead's
//     mr_state is exactly the field the origin bug proved untrustworthy.
//   - A deliberately-abandoned MR is `closed`/`merged` on GitLab, never
//     `opened`, so it never enters this scan — the won't-fix case is excluded
//     structurally, before any marker check.
//
// Safety is the whole point (a spurious re-open is churn, a missed one is the
// bug):
//   - Only CLOSED issue beads are re-opened; an open/in_progress bead needs no
//     repair, and a type=merge-request bead is never a re-open target.
//   - mr_close_override="true" (stamped by `kd close --allow-open-mr`) means the
//     close was deliberate — skip it, so directions 1 and 2 compose instead of
//     fighting.
//   - Every re-open re-verifies the MR against GitLab immediately before acting
//     (fresh per-MR fetch confirms state=opened); a read failure or any
//     non-opened state aborts that bead, so the sweep can never manufacture a
//     re-open from its own error.

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// mrReopenDaemon is the subset of the beads client the sweep needs. Kept small
// so the drift logic is unit-testable with a fake.
type mrReopenDaemon interface {
	// ListBeadsFiltered resolves an mr_url to the issue bead(s) carrying it.
	ListBeadsFiltered(ctx context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error)
	// UpdateBead flips a bead's status back to open (the re-open write).
	UpdateBead(ctx context.Context, beadID string, req beadsapi.UpdateBeadRequest) error
	// AddComment records why the bead was re-opened, on the bead itself.
	AddComment(ctx context.Context, beadID, author, text string) error
}

// mrReopenGitLab is the subset of the GitLab client the sweep needs: list the
// group's open MRs, and re-fetch one MR to re-verify its state before acting.
type mrReopenGitLab interface {
	ListGroupMergeRequests(ctx context.Context, groupID int, state string, updatedAfter time.Time) ([]GitLabMR, error)
	GetMergeRequestByPath(ctx context.Context, projectPath string, mrIID int) (*GitLabMR, error)
}

// reopenSweepAuthor attributes the sweep's re-open comment.
const reopenSweepAuthor = "gitlab-bridge-reconcile"

// mrCloseOverrideField is the marker `kd close --allow-open-mr` stamps on a
// bead whose still-open MR was deliberately closed over. When "true", the sweep
// leaves the bead closed (see kbeads config.go mrFields / close.go).
const mrCloseOverrideField = "mr_close_override"

// MRReopenDriftConfig configures the re-open reconcile sweep.
type MRReopenDriftConfig struct {
	GitLab   mrReopenGitLab
	Daemon   mrReopenDaemon
	Logger   *slog.Logger
	GroupID  int
	Interval time.Duration // how often to scan; default 15m
	Window   time.Duration // only consider MRs updated within this window; default 14d
	DryRun   bool          // when true, log intended re-opens but do not write
}

// MRReopenDriftReconciler periodically re-opens closed issue beads whose linked
// MR is still open (not merged), so pre-existing close-drift self-heals.
type MRReopenDriftReconciler struct {
	gitlab   mrReopenGitLab
	daemon   mrReopenDaemon
	logger   *slog.Logger
	groupID  int
	interval time.Duration
	window   time.Duration
	dryRun   bool
}

// NewMRReopenDriftReconciler builds a sweep from config, applying defaults.
func NewMRReopenDriftReconciler(cfg MRReopenDriftConfig) *MRReopenDriftReconciler {
	interval := cfg.Interval
	if interval <= 0 {
		interval = 15 * time.Minute
	}
	window := cfg.Window
	if window <= 0 {
		window = 14 * 24 * time.Hour
	}
	return &MRReopenDriftReconciler{
		gitlab:   cfg.GitLab,
		daemon:   cfg.Daemon,
		logger:   cfg.Logger,
		groupID:  cfg.GroupID,
		interval: interval,
		window:   window,
		dryRun:   cfg.DryRun,
	}
}

// reopenStats is the per-scan outcome tally, also returned for tests.
type reopenStats struct {
	openMRs    int // open MRs examined
	noBead     int // skipped: no issue bead carries this mr_url
	notClosed  int // skipped: the owning bead is already open/in_progress
	overridden int // skipped: bead carries mr_close_override=true (deliberate)
	notOpenNow int // skipped: re-verify says the MR is no longer `opened`
	reopened   int // beads re-opened this scan
	errors     int // transport / write errors
}

// Run starts the sweep loop, scanning immediately then every interval. It
// blocks until ctx is canceled. A zero group ID disables the sweep.
func (r *MRReopenDriftReconciler) Run(ctx context.Context) error {
	if r.groupID == 0 {
		r.logger.Info("MR-reopen drift sweep disabled (no group id)")
		return nil
	}
	r.logger.Info("starting MR-reopen drift sweep",
		"group_id", r.groupID, "interval", r.interval, "window", r.window, "dry_run", r.dryRun)

	r.scan(ctx)

	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			r.scan(ctx)
		}
	}
}

// ScanOnce performs exactly one reconciliation pass and returns, instead of
// looping on the interval like Run. It exists for the e2e probe
// (cmd/mr-reopen-probe, bead ka-0000006088), which drives this same code against
// the live beads daemon and a stub GitLab: the sweep's 15m interval is far
// longer than a shard budget, so a test needs a way to trigger a single pass
// deterministically. Production always uses Run.
//
// It deliberately exposes no stats: the probe asserts the sweep's OBSERVABLE
// effect (which beads came back open, and the audit comment) against the real
// daemon, rather than an internal tally that could agree with itself while the
// bead writes were wrong.
func (r *MRReopenDriftReconciler) ScanOnce(ctx context.Context) {
	r.scan(ctx)
}

// scan performs one reconciliation pass over the group's recently-updated open
// MRs.
func (r *MRReopenDriftReconciler) scan(ctx context.Context) reopenStats {
	since := time.Now().Add(-r.window)
	var s reopenStats

	// Only `opened` MRs matter: a merged MR's bead SHOULD be closed, and a
	// GitLab-`closed` (abandoned) MR is exactly the deliberate case we must not
	// re-open. So the ground-truth set is precisely the open MRs.
	mrs, err := r.gitlab.ListGroupMergeRequests(ctx, r.groupID, "opened", since)
	if err != nil {
		s.errors++
		r.logger.Error("MR-reopen sweep: list open MRs failed", "error", err)
		return s
	}

	for i := range mrs {
		r.processMR(ctx, &mrs[i], &s)
	}

	r.logger.Info("MR-reopen drift sweep done",
		"open_mrs", s.openMRs,
		"no_bead", s.noBead,
		"not_closed", s.notClosed,
		"overridden", s.overridden,
		"not_open_now", s.notOpenNow,
		"reopened", s.reopened,
		"errors", s.errors,
		"dry_run", r.dryRun)
	return s
}

// processMR handles one open MR: find the closed issue bead(s) that link it and
// re-open each one that has not been deliberately closed over.
func (r *MRReopenDriftReconciler) processMR(ctx context.Context, mr *GitLabMR, s *reopenStats) {
	s.openMRs++
	if mr.WebURL == "" {
		return
	}

	beads, err := r.issueBeadsForMR(ctx, mr.WebURL)
	if err != nil {
		s.errors++
		r.logger.Warn("MR-reopen sweep: bead lookup failed", "mr_url", mr.WebURL, "error", err)
		return
	}
	if len(beads) == 0 {
		s.noBead++
		return
	}

	for _, bead := range beads {
		r.maybeReopen(ctx, mr, bead, s)
	}
}

// maybeReopen re-opens a single owning bead if it is a closed issue bead with
// no deliberate-close marker and the MR re-verifies as still `opened`.
func (r *MRReopenDriftReconciler) maybeReopen(ctx context.Context, mr *GitLabMR, bead *beadsapi.BeadDetail, s *reopenStats) {
	// Only CLOSED beads drift. An open/in_progress bead is already visible to
	// the lane; re-opening it would be a no-op write and a spurious event.
	if bead.Status != "closed" {
		s.notClosed++
		return
	}
	// A deliberate --allow-open-mr close stamped this marker: honor it.
	if bead.Fields[mrCloseOverrideField] == "true" {
		s.overridden++
		r.logger.Debug("MR-reopen sweep: skipping deliberately-closed bead",
			"bead", bead.ID, "mr_url", mr.WebURL)
		return
	}

	// Re-verify against GitLab immediately before writing. The group listing is
	// a snapshot; between the list and now the MR may have merged or closed.
	// Only a fresh, positive `opened` justifies a re-open — so the sweep can
	// never re-open on its own stale read (advice ka-0000000519: never trust a
	// possibly-stale state for MR truth; re-fetch the authoritative one).
	ref := ParseMRURL(mr.WebURL)
	if ref == nil {
		s.errors++
		r.logger.Warn("MR-reopen sweep: unparseable MR url", "mr_url", mr.WebURL)
		return
	}
	fresh, err := r.gitlab.GetMergeRequestByPath(ctx, ref.ProjectPath, ref.IID)
	if err != nil {
		s.errors++
		r.logger.Warn("MR-reopen sweep: re-verify fetch failed — not re-opening",
			"bead", bead.ID, "mr_url", mr.WebURL, "error", err)
		return
	}
	if fresh.State != "opened" {
		s.notOpenNow++
		r.logger.Info("MR-reopen sweep: MR no longer opened on re-verify — leaving bead closed",
			"bead", bead.ID, "mr_url", mr.WebURL, "fresh_state", fresh.State)
		return
	}

	if r.dryRun {
		s.reopened++
		r.logger.Info("MR-reopen sweep: would re-open bead (dry-run)",
			"bead", bead.ID, "mr_url", mr.WebURL)
		return
	}

	// Comment first so the audit note survives even if the status write races a
	// concurrent close; a failed comment aborts the re-open so we never flip
	// status without the explanation.
	comment := formatReopenComment(mr.WebURL, fresh.State)
	if err := r.daemon.AddComment(ctx, bead.ID, reopenSweepAuthor, comment); err != nil {
		s.errors++
		r.logger.Error("MR-reopen sweep: comment failed — not re-opening",
			"bead", bead.ID, "mr_url", mr.WebURL, "error", err)
		return
	}
	openStatus := "open"
	if err := r.daemon.UpdateBead(ctx, bead.ID, beadsapi.UpdateBeadRequest{Status: &openStatus}); err != nil {
		s.errors++
		r.logger.Error("MR-reopen sweep: re-open write failed",
			"bead", bead.ID, "mr_url", mr.WebURL, "error", err)
		return
	}
	s.reopened++
	r.logger.Info("MR-reopen sweep: re-opened bead whose MR is still open",
		"bead", bead.ID, "mr_url", mr.WebURL)
}

// issueBeadsForMR returns the CLOSED issue beads (task/bug/feature/chore/epic)
// carrying mrURL in their mr_url field. It filters to closed status + the
// mr_url field server-side, and drops the canonical type=merge-request bead
// (which also carries mr_url) so only re-openable issue beads come back.
func (r *MRReopenDriftReconciler) issueBeadsForMR(ctx context.Context, mrURL string) ([]*beadsapi.BeadDetail, error) {
	res, err := r.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Statuses: []string{"closed"},
		Fields:   map[string]string{"mr_url": mrURL},
		Limit:    50,
	})
	if err != nil {
		return nil, err
	}
	if res == nil {
		return nil, nil
	}
	var out []*beadsapi.BeadDetail
	for _, b := range res.Beads {
		// Exact-match guard: the field filter is the daemon's job, but re-check
		// so a partial/loose match can never re-open the wrong bead.
		if b.Fields["mr_url"] != mrURL {
			continue
		}
		// Never re-open the MR bead itself; only the issue bead it tracks.
		if b.Type == "merge-request" {
			continue
		}
		out = append(out, b)
	}
	return out, nil
}

// formatReopenComment is the audit note left on a re-opened bead.
func formatReopenComment(mrURL, mrState string) string {
	return fmt.Sprintf(
		"Re-opened by the gitlab-bridge reconcile sweep (ka-0000000267): this bead was closed "+
			"while its merge request %s is still `%s` (not merged), so the ledger claimed a fix "+
			"shipped while its code is not on main. Merge the MR to land the fix, or close again with "+
			"`kd close --allow-open-mr` if the MR was abandoned (that stamps a marker the sweep honors).",
		mrURL, mrState)
}
