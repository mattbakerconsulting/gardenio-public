package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"gardenio/controller/internal/advice"
	"gardenio/controller/internal/beadsapi"
)

// adviceSweeperClient is the subset of the beads daemon client used by the
// advice sweeper. Narrowing the surface lets tests inject a fake without
// reimplementing the full HTTP client.
type adviceSweeperClient interface {
	// ListAllBeadsFiltered PAGES the fetch so an open-advice pool larger than a
	// single page is swept in full. A single capped fetch (the old Limit:500)
	// would leave expired advice beyond the cap open forever — the sweep half of
	// the F10 silent-truncation bug (ka-0000005760).
	ListAllBeadsFiltered(ctx context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error)
	CloseBead(ctx context.Context, beadID string, fields map[string]string) error
}

// adviceSweeper periodically compacts the open advice pool. Two passes run on
// every sweep:
//
//   - EXPIRY: closes open advice whose expires_at timestamp is in the past.
//   - SUPERSESSION: closes open advice that another open advice bead explicitly
//     supersedes ("supersedes ka-XXX"). These beads are already hidden from
//     agent loadouts by ApplySupersessions at read time, but stay open and keep
//     consuming the advice-pool cap (R1) and byte budget until closed. Retiring
//     them is the compaction cadence R3 (ka-0000006409) stands up to hold corpus
//     growth beyond bare expiry.
//
// Closing (not deleting) preserves history while removing the bead from advice
// read paths (gb advice list, gb prime).
type adviceSweeper struct {
	daemon adviceSweeperClient
	// compaction gates the supersession-close pass; the expiry pass always runs.
	compaction bool
	interval   time.Duration
	logger     *slog.Logger
	now        func() time.Time
}

func newAdviceSweeper(daemon adviceSweeperClient, interval time.Duration, compaction bool, logger *slog.Logger) *adviceSweeper {
	return &adviceSweeper{
		daemon:     daemon,
		compaction: compaction,
		interval:   interval,
		logger:     logger.With("component", "advice-sweeper"),
		now:        time.Now,
	}
}

// RunLoop sweeps immediately then every interval until ctx is cancelled.
// An interval of 0 returns without running (sweeper disabled).
func (s *adviceSweeper) RunLoop(ctx context.Context) {
	if s.interval <= 0 {
		s.logger.Info("advice sweeper disabled (interval=0)")
		return
	}
	s.logger.Info("advice sweeper starting", "interval", s.interval)

	s.sweep(ctx)

	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.sweep(ctx)
		}
	}
}

// sweep lists open advice beads and closes any that are expired or explicitly
// superseded by another open advice bead. Errors on individual beads are logged
// and do not abort the sweep.
func (s *adviceSweeper) sweep(ctx context.Context) {
	result, err := s.daemon.ListAllBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"advice"},
		Statuses: []string{"open"},
	})
	if err != nil {
		s.logger.Warn("advice sweep: failed to list open advice beads", "error", err)
		return
	}
	if len(result.Beads) == 0 {
		return
	}

	// Compute the supersession-compaction set from the whole open snapshot once,
	// before we start closing anything, so the graph is evaluated against a
	// consistent view (SupersededOpenIDs itself excludes expired beads as
	// superseders — an about-to-be-swept expired bead never retires a peer).
	// Skipped entirely when compaction is disabled, so only expiry runs.
	var superseded map[string]string
	if s.compaction {
		superseded = advice.SupersededOpenIDs(result.Beads)
	}

	closedExpired, closedSuperseded := 0, 0
	for _, bead := range result.Beads {
		var reason string
		switch {
		case advice.IsExpired(bead):
			expiresAt := bead.Fields["expires_at"]
			reason = fmt.Sprintf("auto-closed: advice expiration reached at %s", expiresAt)
		case superseded[bead.ID] != "":
			reason = fmt.Sprintf("auto-closed: superseded by %s (advice compaction, R3 ka-0000006409)", superseded[bead.ID])
		default:
			continue
		}
		if err := s.daemon.CloseBead(ctx, bead.ID, map[string]string{
			"close_reason": reason,
			"closed_at":    s.now().UTC().Format(time.RFC3339),
		}); err != nil {
			s.logger.Warn("advice sweep: failed to close bead",
				"id", bead.ID, "reason", reason, "error", err)
			continue
		}
		if superseder := superseded[bead.ID]; superseder != "" && !advice.IsExpired(bead) {
			closedSuperseded++
			s.logger.Info("advice sweep: closed superseded advice",
				"id", bead.ID, "superseded_by", superseder)
		} else {
			closedExpired++
			s.logger.Info("advice sweep: closed expired advice",
				"id", bead.ID, "expires_at", bead.Fields["expires_at"])
		}
	}

	if closedExpired+closedSuperseded > 0 {
		s.logger.Info("advice sweep complete",
			"scanned", len(result.Beads),
			"closed_expired", closedExpired,
			"closed_superseded", closedSuperseded)
	}
}
