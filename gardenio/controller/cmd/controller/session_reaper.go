package main

import (
	"context"
	"log/slog"
	"time"

	"gardenio/controller/internal/beadsapi"
	gardeniometrics "gardenio/controller/internal/metrics"
)

// sessionReaper closes session beads whose agent has ended (gd-c6muILNedN).
//
// A session bead is closed in exactly one place: gb, in-pod, after coop exits
// (cmd/gb/session.go closeSessionBead). It parses the session log and writes
// the slowest-calls field before the close PATCH, so a pod terminate (gb stop,
// wall_clock, cooldown kill) whose SIGKILL lands first leaves the bead open with
// fields.status=active forever. gb's own orphan reconciler only runs on the
// next start of the SAME agent name, which cron crews and plan gardeners never
// have. Measured 2026-10-24: 3,577 open session beads, ~200 more a day, most of
// them for closed agents.
//
// The reaper is the server-side backstop. Each cycle it lists open session
// beads and the agent beads that are still live, and closes every session whose
// agent bead is closed (for longer than sessionReaperGrace) or missing. It uses
// three list reads and no per-agent fetches, so a steady-state cycle reads a few
// dozen rows.
type sessionReaper struct {
	daemon   sessionReaperDaemon
	metrics  *gardeniometrics.EventMetrics
	logger   *slog.Logger
	interval time.Duration
}

// sessionReaperDaemon is the slice of *beadsapi.Client the reaper uses.
type sessionReaperDaemon interface {
	ListBeadsFiltered(ctx context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error)
	CloseBead(ctx context.Context, beadID string, fields map[string]string) error
}

const (
	// sessionReaperGrace leaves a just-ended agent's session to gb, whose own
	// close carries the exit code and usage. gb closes within seconds of coop
	// exiting; ten minutes is far past any close that is still going to land.
	sessionReaperGrace = 10 * time.Minute

	// sessionReaperMaxCloses bounds one cycle's writes. The first cycles after
	// deploy drain the historical backlog (~3,500) at this rate instead of in
	// one burst; steady state closes a handful per cycle.
	sessionReaperMaxCloses = 500

	// sessionReaperPageSize / sessionReaperMaxPages bound one list.
	sessionReaperPageSize = 500
	sessionReaperMaxPages = 20

	// sessionStatusRetired is the type:session status the reaper writes. The
	// daemon validates the enum strictly ({active, completed, crashed,
	// retired}); "retired" says the agent ended without recording an exit,
	// and keeps these out of the crashed count that SessionStatusForExit made
	// honest (ka-0000003165).
	sessionStatusRetired = "retired"
)

func newSessionReaper(daemon *beadsapi.Client, metrics *gardeniometrics.EventMetrics, interval time.Duration, logger *slog.Logger) *sessionReaper {
	return &sessionReaper{
		daemon:   daemon,
		metrics:  metrics,
		logger:   logger.With("component", "session-reaper"),
		interval: interval,
	}
}

// RunLoop reaps once at start and then every interval until ctx is cancelled.
// A non-positive interval disables the loop.
func (r *sessionReaper) RunLoop(ctx context.Context) {
	if r.interval <= 0 {
		return
	}
	r.logger.Info("session reaper started", "interval", r.interval, "grace", sessionReaperGrace)
	r.reap(ctx, time.Now())
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.reap(ctx, time.Now())
		}
	}
}

// reap runs one cycle at the given instant (injected for tests).
func (r *sessionReaper) reap(ctx context.Context, now time.Time) {
	sessions, _, err := r.list(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"session"},
		Statuses: beadsapi.ActiveStatuses(),
		Sort:     "created_at",
	}.OnlyFields("agent_bead_id", "status", "ended_at"))
	if err != nil {
		r.logger.Warn("session reaper: listing open sessions failed", "error", err)
		return
	}
	if len(sessions) == 0 {
		r.metrics.SetOrphanSessions(0, 0)
		return
	}

	// An agent bead that is not closed is live; one closed inside the grace
	// window may still be closing its own session. A truncated list would make
	// a live agent read as ended, so either list failing or truncating skips
	// the cycle.
	live, liveTrunc, err := r.list(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"agent"},
		Statuses: beadsapi.ActiveStatuses(),
		Sort:     "created_at",
	}.OnlyFields())
	if err != nil || liveTrunc {
		r.logger.Warn("session reaper: listing live agents failed, cycle skipped", "error", err, "truncated", liveTrunc)
		return
	}
	recent, recentTrunc, err := r.list(ctx, beadsapi.ListBeadsQuery{
		Types:       []string{"agent"},
		Statuses:    []string{"closed"},
		ClosedAfter: now.Add(-sessionReaperGrace),
		Sort:        "created_at",
	}.OnlyFields())
	if err != nil || recentTrunc {
		r.logger.Warn("session reaper: listing recently closed agents failed, cycle skipped", "error", err, "truncated", recentTrunc)
		return
	}
	keep := make(map[string]bool, len(live)+len(recent))
	for _, b := range append(live, recent...) {
		if b.Type == "agent" {
			keep[b.ID] = true
		}
	}

	var orphans []*beadsapi.BeadDetail
	for _, s := range sessions {
		if s.Type != "session" || s.Status == "closed" {
			continue
		}
		if sessionOrphan(s, keep, now) {
			orphans = append(orphans, s)
		}
	}

	closed, failed := 0, 0
	endedAt := now.UTC().Format(time.RFC3339)
	for _, s := range orphans {
		if closed+failed >= sessionReaperMaxCloses {
			break
		}
		if err := r.daemon.CloseBead(ctx, s.ID, sessionCloseFields(s, endedAt)); err != nil {
			failed++
			r.logger.Warn("session reaper: close failed", "session", s.ID, "agent_bead", s.Fields["agent_bead_id"], "error", err)
			continue
		}
		closed++
	}
	r.metrics.SetOrphanSessions(len(orphans), len(orphans)-closed)
	if len(orphans) > 0 {
		r.logger.Info("session reaper: closed sessions of ended agents",
			"found", len(orphans), "closed", closed, "failed", failed, "open_sessions", len(sessions))
	}
}

// sessionOrphan reports whether an open session bead's agent has ended. keep
// holds the agent beads that are live or closed inside the grace window.
//
// A session gb already stamped ended (ended_at set, close leg failed) is an
// orphan once the stamp is older than the grace, whatever its agent. Otherwise
// it is an orphan when its agent bead is neither live nor recently closed:
// closed long enough ago, or gone. A session with no agent_bead_id, or one
// created inside the grace window, is left alone.
func sessionOrphan(s *beadsapi.BeadDetail, keep map[string]bool, now time.Time) bool {
	if ended := s.Fields["ended_at"]; ended != "" {
		t, err := time.Parse(time.RFC3339, ended)
		return err == nil && now.Sub(t) >= sessionReaperGrace
	}
	agentID := s.Fields["agent_bead_id"]
	if agentID == "" || now.Sub(s.CreatedAt) < sessionReaperGrace {
		return false
	}
	return !keep[agentID]
}

// sessionCloseFields is what the close stamps on the session bead. It fills
// ended_at and a terminal status only where gb left them unset, so an exit gb
// did record is never overwritten.
func sessionCloseFields(s *beadsapi.BeadDetail, endedAt string) map[string]string {
	fields := map[string]string{}
	if s.Fields["ended_at"] == "" {
		fields["ended_at"] = endedAt
	}
	if st := s.Fields["status"]; st == "" || st == "active" {
		fields["status"] = sessionStatusRetired
	}
	return fields
}

// list pages a query to the end, or to sessionReaperMaxPages (truncated=true).
func (r *sessionReaper) list(ctx context.Context, q beadsapi.ListBeadsQuery) (out []*beadsapi.BeadDetail, truncated bool, err error) {
	q.Limit = sessionReaperPageSize
	q.NoTotal = true // pages end on a short page; Total is never read
	for page := 0; page < sessionReaperMaxPages; page++ {
		q.Offset = page * sessionReaperPageSize
		res, err := r.daemon.ListBeadsFiltered(ctx, q)
		if err != nil {
			return out, false, err
		}
		out = append(out, res.Beads...)
		if len(res.Beads) < sessionReaperPageSize {
			return out, false, nil
		}
	}
	return out, true, nil
}
