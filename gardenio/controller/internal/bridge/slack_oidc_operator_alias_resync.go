package bridge

import (
	"context"
	"strings"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"gardenio/controller/internal/beadsapi"
)

// slackOIDCResyncInterval is how often the resync job walks user beads and
// upserts operator_aliases rows so every identity a human carries on their
// user bead (slack:U…, oidc:<email>, gitlab:<username>, jira:<user-id>)
// resolves to a single operator row. Idempotent on a steady-state cluster —
// once every alias is established subsequent cycles are no-ops.
//
// Resurrects ka-0000003795 (child #2 of ka-0000007320) as ka-0000000483; the
// broadening from the original slack+oidc-only pass to every identity field
// is the ka-0000003795 follow-up.
const slackOIDCResyncInterval = 6 * time.Hour

// slackOIDCResyncPageSize bounds the list call. User beads scale with humans
// in the org (tens, not thousands), so a single page is almost always
// sufficient — pagination is here for safety, not throughput.
const slackOIDCResyncPageSize = 500

// slackOIDCAliasLinker is the small daemon surface the resync job needs. Kept
// as a local interface so the test fake is a stub, not the full BeadClient.
//
// The bridge never chooses aliases (gd-uAvleAZUvV). It tells the daemon "resync
// bead X" and the daemon reads the identity fields from that type:user bead
// itself — fields only an OIDC gardenio-admin can write — and, in the same
// way, computes the prune keep-set from the user beads. The bridge's static /
// fleet token is therefore not an authority to attach an identity to an
// operator: the daemon refuses an explicit alias list from it (403).
type slackOIDCAliasLinker interface {
	ListBeadsFiltered(ctx context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error)
	ResyncUserBeadAliases(ctx context.Context, beadID string) (*beadsapi.LinkOperatorAliasesResponse, error)
}

// slackOIDCAliasPruner is the optional revocation half of the daemon surface.
// Asserted at runtime so a daemon client without it only loses revocation.
type slackOIDCAliasPruner interface {
	PruneUserBeadAliases(ctx context.Context) (*beadsapi.PruneOperatorAliasesResponse, error)
}

// Prometheus metrics emitted by the resync loop. Registered globally against
// the controller's default registerer (see cmd/controller/main.go) so they
// show up on the /metrics endpoint.
var (
	slackOIDCResyncCoverage = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "gardenio_bridge_slack_oidc_alias_coverage",
		Help: "Number of user beads whose two-or-more identity fields (slack_id, OIDC email, gitlab, jira) are linked under a single operator row as of the last resync pass.",
	})
	slackOIDCResyncLastSuccess = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "gardenio_bridge_slack_oidc_resync_last_success_timestamp_seconds",
		Help: "UNIX timestamp of the most recent successful Slack↔OIDC operator-alias resync cycle.",
	})
	slackOIDCResyncLinksCreated = promauto.NewCounter(prometheus.CounterOpts{
		Name: "gardenio_bridge_slack_oidc_resync_aliases_inserted_total",
		Help: "Cumulative count of operator_aliases rows inserted by the Slack↔OIDC resync (new slack-id alias attached to an OIDC operator).",
	})
	slackOIDCResyncRefused = promauto.NewCounter(prometheus.CounterOpts{
		Name: "gardenio_bridge_slack_oidc_resync_collisions_refused_total",
		Help: "Cumulative count of user beads whose alias set the daemon refused (409) because an alias already points at a different operator. The resync never merges operators; resolve by hand with an OIDC gardenio-admin.",
	})
	slackOIDCResyncPruned = promauto.NewCounter(prometheus.CounterOpts{
		Name: "gardenio_bridge_slack_oidc_resync_aliases_pruned_total",
		Help: "Cumulative count of user-bead-bridge alias rows deleted because the identity is no longer on any user bead.",
	})
	slackOIDCResyncMerges = promauto.NewCounter(prometheus.CounterOpts{
		Name: "gardenio_bridge_slack_oidc_resync_operators_merged_total",
		Help: "Cumulative count of operator rows merged into the OIDC operator by the Slack↔OIDC resync.",
	})
)

// startSlackOIDCOperatorAliasResync periodically walks user beads and ensures
// every identity a bead carries (slack, oidc email, gitlab, jira) points at a
// single operator row. A bead needs at least two identities for there to be
// anything to link.
//
// Distinct from startSlackOperatorResync, which refreshes display/email on
// the existing slack-aliased operator rows from their user beads. This one
// closes the gap that beads-viewer hit when a human's identity rows (e.g.
// slack:U… and oidc:<email>, or a gitlab:<username> on top) were present but
// unlinked.
func (b *Bot) startSlackOIDCOperatorAliasResync(ctx context.Context) {
	if b.daemon == nil {
		return
	}
	logger := b.logger.With("job", "slack_oidc_operator_alias_resync")

	// Stagger the initial pass so it does not contend with the
	// slack_operator_resync warmup (which fires at +2m).
	first := time.NewTimer(3 * time.Minute)
	defer first.Stop()
	select {
	case <-ctx.Done():
		return
	case <-first.C:
	}
	b.runSlackOIDCOperatorAliasResyncCycle(ctx, b.daemon, logger)

	ticker := time.NewTicker(slackOIDCResyncInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			b.runSlackOIDCOperatorAliasResyncCycle(ctx, b.daemon, logger)
		}
	}
}

// runSlackOIDCOperatorAliasResyncCycle is one pass of the resync loop.
// Factored out so tests can drive it without spinning up a ticker.
func (b *Bot) runSlackOIDCOperatorAliasResyncCycle(
	ctx context.Context,
	daemon slackOIDCAliasLinker,
	logger interface {
		Info(msg string, args ...any)
		Warn(msg string, args ...any)
	},
) {
	start := time.Now()
	var processed, linked, alreadyLinked, merges, skipped, errs, refused, pruned atomic.Int64

	offset := 0
	coverage := int64(0)
	for {
		if ctx.Err() != nil {
			return
		}
		result, err := daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
			Types:  []string{"user"},
			Kinds:  []string{"config"},
			Limit:  slackOIDCResyncPageSize,
			Offset: offset,
		})
		if err != nil {
			logger.Warn("list user beads failed", "err", err)
			return
		}
		if len(result.Beads) == 0 {
			break
		}
		for _, bead := range result.Beads {
			if ctx.Err() != nil {
				return
			}
			processed.Add(1)
			resp, err := daemon.ResyncUserBeadAliases(ctx, bead.ID)
			if beadsapi.IsAliasCollision(err) {
				// An alias on this bead already belongs to a different
				// operator. The loop never merges (that rewrites audit rows
				// across operators): leave both as they are for an admin.
				refused.Add(1)
				slackOIDCResyncRefused.Inc()
				logger.Warn("resync of user bead aliases refused: an alias already points at a different operator; merge needs an OIDC gardenio-admin",
					"bead_id", bead.ID)
				continue
			}
			if err != nil {
				errs.Add(1)
				logger.Warn("resync of user bead aliases failed", "bead_id", bead.ID, "err", err)
				continue
			}
			if resp.NothingToLink {
				// Fewer than two linkable identities on the bead.
				skipped.Add(1)
				continue
			}
			coverage++
			if resp.AlreadyLinked {
				alreadyLinked.Add(1)
				continue
			}
			if resp.AliasesInserted > 0 {
				linked.Add(1)
				slackOIDCResyncLinksCreated.Add(float64(resp.AliasesInserted))
			}
			if n := len(resp.MergedFromIDs); n > 0 {
				merges.Add(int64(n))
				slackOIDCResyncMerges.Add(float64(n))
			}
		}
		if len(result.Beads) < slackOIDCResyncPageSize {
			break
		}
		offset += len(result.Beads)
	}

	if ctx.Err() != nil {
		return
	}
	if processed.Load() > 0 {
		pruned.Store(b.pruneRemovedAliases(ctx, daemon, logger))
	}
	slackOIDCResyncCoverage.Set(float64(coverage))
	slackOIDCResyncLastSuccess.Set(float64(time.Now().Unix()))

	logger.Info(
		"slack↔oidc operator alias resync cycle complete",
		"processed", processed.Load(),
		"linked", linked.Load(),
		"already_linked", alreadyLinked.Load(),
		"merges", merges.Load(),
		"skipped", skipped.Load(),
		"refused", refused.Load(),
		"pruned", pruned.Load(),
		"errors", errs.Load(),
		"coverage", coverage,
		"duration_ms", time.Since(start).Milliseconds(),
	)
}

// pruneRemovedAliases revokes the alias rows this job wrote
// (source=user-bead-bridge) for identities no user bead carries any more.
// Without it nothing ever deletes operator_aliases rows, so an identity
// removed from a bead would resolve to its operator forever. Called only after
// a complete scan. The daemon computes the keep-set from the user beads
// itself; the bridge cannot supply one (a dummy keep-list from a machine token
// used to delete every bridge alias), and the daemon prunes nothing when it
// finds no alias on any user bead. Returns the number of rows deleted.
func (b *Bot) pruneRemovedAliases(
	ctx context.Context,
	daemon slackOIDCAliasLinker,
	logger interface {
		Info(msg string, args ...any)
		Warn(msg string, args ...any)
	},
) int64 {
	pruner, ok := daemon.(slackOIDCAliasPruner)
	if !ok {
		return 0
	}
	resp, err := pruner.PruneUserBeadAliases(ctx)
	if err != nil {
		logger.Warn("prune removed operator aliases failed", "err", err)
		return 0
	}
	if resp.Count > 0 {
		slackOIDCResyncPruned.Add(float64(resp.Count))
		kinds := make([]string, 0, len(resp.Deleted))
		for _, d := range resp.Deleted {
			kinds = append(kinds, d.Kind)
		}
		logger.Info("revoked operator aliases no longer on any user bead",
			"count", resp.Count, "kinds", strings.Join(kinds, "+"))
	}
	return int64(resp.Count)
}
