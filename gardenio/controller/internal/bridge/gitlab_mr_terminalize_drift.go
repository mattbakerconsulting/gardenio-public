package bridge

// gitlab_mr_terminalize_drift.go — reconcile sweep that TERMINALIZES stale
// non-terminal MR beads: type=merge-request beads still carrying
// mr_state=opened whose merge request has actually merged/closed on GitLab
// (ka-0000001293, the residual half of ka-0000001223 / ka-0000002394).
//
// This is the exact inverse of gitlab_mr_reopen_drift.go. That sweep enumerates
// GitLab's currently-OPEN MRs and re-opens closed issue beads whose MR is still
// open; by construction it never touches a bead whose MR has merged/closed
// (that direction is explicitly out of its scope, see its header). So a real
// gap remained: nothing ever walked the non-terminal MR beads and terminalized
// the ones GitLab says are done.
//
// The measured drift (2026-08-18, ka-0000001293): of the 888 type=merge-request
// beads carrying mr_state=opened, the OLDEST 60 by updated_at were ~70% already
// terminal on GitLab (27 closed, 15 merged), while the NEWEST 60 were ~0% drift.
// So the drift is a HISTORICAL BACKLOG concentrated in beads last touched months
// ago, not an ongoing leak — the bridge's live webhook/poll path keeps current
// MRs correct; what never happened is a backfill over the beads that went stale
// while sync was down or before they were linked. Also drifted: some beads whose
// lifecycle terminalized (status=closed) while their mr_state field never did.
//
// Why this direction is the per-bead re-fetch (unlike the reopen sweep's cheap
// open-MR scan): the drift set is exactly the beads GitLab has FORGOTTEN in its
// recent-changes window — an MR merged months ago is not in any bounded
// ListMergedMRs window, so a windowed group scan structurally cannot find it.
// The only way to learn a stale bead's true state is to ask GitLab about that
// specific MR. To keep that bounded (an unbounded walk is 888 GitLab
// round-trips), the sweep is:
//   - STALE-GATED: only beads whose updated_at is older than StaleAge are
//     candidates. Fresh MRs are owned by the live path; re-fetching them here
//     would put load on GitLab for beads that are already correct.
//   - FAN-OUT CAPPED: at most MaxPerScan beads re-fetched per pass, oldest-first.
//     As terminal beads drain (their mr_state flips off `opened`, and open-status
//     beads close), the oldest-first window slides forward, so the backlog drains
//     monotonically over several passes instead of in one unbounded burst.
//
// Safety mirrors the reopen sweep — a spurious terminalize is churn, a missed
// one is the bug:
//   - Every terminalize RE-VERIFIES the MR against GitLab immediately before
//     writing (the candidate query's mr_state is exactly the field the origin
//     bug proved untrustworthy — advice ka-0000000519). Only a fresh, positive
//     `merged`/`closed` justifies a write; a read failure or a still-`opened`
//     state aborts that bead, so the sweep can never terminalize on its own
//     stale read.
//   - It only writes the terminal direction: it stamps mr_state/mr_outcome/
//     mr_merged and closes the bead. A still-`opened` MR is left entirely alone
//     (the reopen sweep owns the reopen direction), so the two sweeps compose
//     rather than fight.

import (
	"context"
	"log/slog"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// mrTerminalizeDaemon is the subset of the beads client the sweep needs. Kept
// small so the drift logic is unit-testable with a fake.
type mrTerminalizeDaemon interface {
	// ListBeadsFiltered selects the stale non-terminal MR bead candidates.
	ListBeadsFiltered(ctx context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error)
	// UpdateBeadFields stamps drifted mr_* fields on an already-closed bead.
	UpdateBeadFields(ctx context.Context, beadID string, fields map[string]string) error
	// CloseBead closes an open bead and stamps the mr_* fields atomically.
	CloseBead(ctx context.Context, beadID string, fields map[string]string) error
}

// mrTerminalizeGitLab is the subset of the GitLab client the sweep needs: a
// per-MR re-fetch to learn the authoritative state.
type mrTerminalizeGitLab interface {
	GetMergeRequestByPath(ctx context.Context, projectPath string, mrIID int) (*GitLabMR, error)
}

// MRTerminalizeDriftConfig configures the terminalize reconcile sweep.
type MRTerminalizeDriftConfig struct {
	GitLab     mrTerminalizeGitLab
	Daemon     mrTerminalizeDaemon
	Logger     *slog.Logger
	Interval   time.Duration // how often to scan; default 1h
	StaleAge   time.Duration // only beads not updated within this window; default 7d
	MaxPerScan int           // cap on beads re-fetched per pass; default 200
	DryRun     bool          // when true, log intended terminalizes but do not write
}

// MRTerminalizeDriftReconciler periodically terminalizes stale non-terminal MR
// beads whose merge request has actually merged/closed on GitLab, draining the
// historical drift backlog the live webhook/poll path never backfilled.
type MRTerminalizeDriftReconciler struct {
	gitlab     mrTerminalizeGitLab
	daemon     mrTerminalizeDaemon
	logger     *slog.Logger
	interval   time.Duration
	staleAge   time.Duration
	maxPerScan int
	dryRun     bool
}

// NewMRTerminalizeDriftReconciler builds a sweep from config, applying defaults.
func NewMRTerminalizeDriftReconciler(cfg MRTerminalizeDriftConfig) *MRTerminalizeDriftReconciler {
	interval := cfg.Interval
	if interval <= 0 {
		interval = time.Hour
	}
	staleAge := cfg.StaleAge
	if staleAge <= 0 {
		staleAge = 7 * 24 * time.Hour
	}
	maxPerScan := cfg.MaxPerScan
	if maxPerScan <= 0 {
		maxPerScan = 200
	}
	return &MRTerminalizeDriftReconciler{
		gitlab:     cfg.GitLab,
		daemon:     cfg.Daemon,
		logger:     cfg.Logger,
		interval:   interval,
		staleAge:   staleAge,
		maxPerScan: maxPerScan,
		dryRun:     cfg.DryRun,
	}
}

// terminalizeStats is the per-scan outcome tally, also returned for tests.
type terminalizeStats struct {
	candidates   int // stale non-terminal MR beads examined this pass
	skippedNoRef int // skipped: bead has no parseable mr_url
	stillOpen    int // skipped: re-verify says the MR is still opened/locked
	gitlabErrors int // per-bead GitLab re-fetch errors
	terminalized int // beads terminalized this pass (fields stamped)
	closed       int // of the terminalized, how many were also closed (were open)
	writeErrors  int // daemon write errors
}

// Run starts the sweep loop, scanning immediately then every interval. It
// blocks until ctx is canceled.
func (r *MRTerminalizeDriftReconciler) Run(ctx context.Context) error {
	r.logger.Info("starting MR-terminalize drift sweep",
		"interval", r.interval, "stale_age", r.staleAge, "max_per_scan", r.maxPerScan, "dry_run", r.dryRun)

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

// scan performs one bounded pass over the oldest stale non-terminal MR beads.
func (r *MRTerminalizeDriftReconciler) scan(ctx context.Context) terminalizeStats {
	var s terminalizeStats

	// Candidate set: type=merge-request beads still carrying mr_state=opened,
	// not touched within StaleAge, oldest-first, capped at MaxPerScan. The stale
	// gate excludes the live-managed set (newest beads are ~0% drift); oldest-
	// first + the cap bounds each pass and slides the window forward as terminal
	// beads drain out of the mr_state=opened filter. Statuses spans open AND
	// closed: a bead can be status=closed while its mr_state field never
	// terminalized (measured), and that field still needs repair.
	res, err := r.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:         []string{"merge-request"},
		Statuses:      []string{"open", "in_progress", "blocked", "deferred", "closed"},
		Fields:        map[string]string{"mr_state": "opened"},
		UpdatedBefore: time.Now().Add(-r.staleAge),
		Sort:          "updated_at", // ascending: oldest (most-drifted) first
		Limit:         r.maxPerScan,
	})
	if err != nil {
		s.gitlabErrors++ // count as an error so a broken pass is never silently "clean"
		r.logger.Error("MR-terminalize sweep: candidate list failed", "error", err)
		return s
	}
	if res != nil {
		for _, b := range res.Beads {
			r.processBead(ctx, b, &s)
		}
	}

	r.logger.Info("MR-terminalize drift sweep done",
		"candidates", s.candidates,
		"skipped_no_ref", s.skippedNoRef,
		"still_open", s.stillOpen,
		"gitlab_errors", s.gitlabErrors,
		"terminalized", s.terminalized,
		"closed", s.closed,
		"write_errors", s.writeErrors,
		"dry_run", r.dryRun)
	return s
}

// processBead re-verifies one candidate bead against GitLab and terminalizes it
// if — and only if — GitLab reports the MR merged or closed.
func (r *MRTerminalizeDriftReconciler) processBead(ctx context.Context, b *beadsapi.BeadDetail, s *terminalizeStats) {
	s.candidates++

	ref := ParseMRURL(b.Fields["mr_url"])
	if ref == nil {
		s.skippedNoRef++
		r.logger.Debug("MR-terminalize sweep: unparseable mr_url — skipping",
			"bead", b.ID, "mr_url", b.Fields["mr_url"])
		return
	}

	// Re-verify against GitLab. The candidate query's mr_state is exactly the
	// field the origin bug proved untrustworthy, so we NEVER act on it — only a
	// fresh, positive terminal state justifies a write.
	fresh, err := r.gitlab.GetMergeRequestByPath(ctx, ref.ProjectPath, ref.IID)
	if err != nil {
		s.gitlabErrors++
		r.logger.Warn("MR-terminalize sweep: re-verify fetch failed — not terminalizing",
			"bead", b.ID, "mr_url", b.Fields["mr_url"], "error", err)
		return
	}

	terminal := fresh.State == "merged" || fresh.State == "closed"
	if !terminal {
		// Still opened/locked on GitLab: leave it entirely alone. A closed bead
		// whose MR is genuinely still open is the reopen sweep's domain, not ours.
		s.stillOpen++
		return
	}

	updates := terminalFieldUpdates(fresh.State)
	shouldClose := b.Status != "closed"

	if r.dryRun {
		s.terminalized++
		if shouldClose {
			s.closed++
		}
		r.logger.Info("MR-terminalize sweep: would terminalize (dry-run)",
			"bead", b.ID, "mr_url", b.Fields["mr_url"],
			"fresh_state", fresh.State, "would_close", shouldClose, "updates", updates)
		return
	}

	if shouldClose {
		if err := r.daemon.CloseBead(ctx, b.ID, updates); err != nil {
			s.writeErrors++
			r.logger.Error("MR-terminalize sweep: close+stamp failed",
				"bead", b.ID, "mr_url", b.Fields["mr_url"], "error", err)
			return
		}
		s.closed++
	} else {
		if err := r.daemon.UpdateBeadFields(ctx, b.ID, updates); err != nil {
			s.writeErrors++
			r.logger.Error("MR-terminalize sweep: field stamp failed",
				"bead", b.ID, "mr_url", b.Fields["mr_url"], "error", err)
			return
		}
	}
	s.terminalized++
	r.logger.Info("MR-terminalize sweep: terminalized stale MR bead",
		"bead", b.ID, "mr_url", b.Fields["mr_url"], "fresh_state", fresh.State, "closed", shouldClose)
}

// terminalFieldUpdates maps a terminal GitLab MR state to the mr_* field patch
// the bead should carry: the same outcome semantics the webhook path stamps via
// UpsertMergeRequestBead.
func terminalFieldUpdates(state string) map[string]string {
	if state == "merged" {
		return map[string]string{
			"mr_state":   "merged",
			"mr_outcome": "merged",
			"mr_merged":  "true",
		}
	}
	// closed
	return map[string]string{
		"mr_state":   "closed",
		"mr_outcome": "closed",
		"mr_merged":  "false",
	}
}
