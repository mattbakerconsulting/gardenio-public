package bridge

import (
	"context"
	"errors"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// slackOperatorResyncInterval is how often the resync job walks the alias
// table. The job is idempotent (the daemon leaves updated_at alone on a no-op
// write), so a low cadence is fine — the goal is to catch user-bead renames
// within a day, not within a minute.
const slackOperatorResyncInterval = 6 * time.Hour

// slackOperatorIdentityClient is the small admin surface this job needs from
// the beads daemon. Defining a local interface (rather than expanding
// BeadClient) keeps blast radius small and lets the test fake be a 2-method
// stub.
type slackOperatorIdentityClient interface {
	ListSlackOperatorAliases(ctx context.Context) (*beadsapi.ListSlackOperatorAliasesResponse, error)
	ResyncOperatorIdentity(ctx context.Context, operatorID int64) (*beadsapi.UpdateOperatorIdentityResponse, error)
}

// startSlackOperatorResync periodically walks operator_aliases (kind=slack)
// and asks the daemon to refresh each operator's display/email from the
// person's type:user bead.
//
// This job used to copy the Slack users.info profile name and email into the
// operator row. A Slack profile is the person's own to edit, and the bridge's
// machine token could write any value onto any operator, so the daemon now
// refuses explicit values from machine tokens and derives them itself
// (gd-iRFGU6xSK7).
//
// ADR docs/adr/human-attribution-identity-v2.md §7 follow-up #1.
func (b *Bot) startSlackOperatorResync(ctx context.Context) {
	if b.daemon == nil {
		return
	}
	logger := b.logger.With("job", "slack_operator_resync")

	// Run an initial pass shortly after startup so a freshly-deployed
	// bridge populates identity within minutes (not hours), but stagger
	// off the connect path so we do not contend with backfill traffic.
	first := time.NewTimer(2 * time.Minute)
	defer first.Stop()
	select {
	case <-ctx.Done():
		return
	case <-first.C:
	}
	b.runSlackOperatorResyncCycle(ctx, b.daemon, logger)

	ticker := time.NewTicker(slackOperatorResyncInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			b.runSlackOperatorResyncCycle(ctx, b.daemon, logger)
		}
	}
}

// runSlackOperatorResyncCycle is one pass of the resync loop, factored out so
// tests can drive it without spinning up a ticker. Each operator is resynced
// once even when several slack aliases point at it.
func (b *Bot) runSlackOperatorResyncCycle(
	ctx context.Context,
	daemon slackOperatorIdentityClient,
	logger interface {
		Info(msg string, args ...any)
		Warn(msg string, args ...any)
	},
) {
	resp, err := daemon.ListSlackOperatorAliases(ctx)
	if err != nil {
		logger.Warn("list slack operator aliases failed", "err", err)
		return
	}
	var processed, updated, noUserBead, updateErrs, missing int
	seen := make(map[int64]bool, len(resp.Aliases))
	start := time.Now()
	for _, alias := range resp.Aliases {
		if ctx.Err() != nil {
			return
		}
		if seen[alias.OperatorID] {
			continue
		}
		seen[alias.OperatorID] = true
		processed++
		updResp, err := daemon.ResyncOperatorIdentity(ctx, alias.OperatorID)
		switch {
		case err != nil:
			updateErrs++
		case updResp == nil:
			missing++
		case updResp.NoUserBead:
			noUserBead++
		case !updResp.Updated:
			missing++
		default:
			updated++
		}
	}
	if errors.Is(ctx.Err(), context.Canceled) {
		return
	}
	logger.Info(
		"slack operator resync cycle complete",
		"aliases", len(resp.Aliases),
		"processed", processed,
		"updated", updated,
		"no_user_bead", noUserBead,
		"update_errors", updateErrs,
		"missing_operators", missing,
		"duration_ms", time.Since(start).Milliseconds(),
	)
}
