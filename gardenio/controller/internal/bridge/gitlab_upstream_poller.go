package bridge

import (
	"context"
	"log/slog"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// GitLabUpstreamBeadClient is the narrow bead-daemon surface the scoped
// upstream backstop poller needs. It is a subset of GitLabBeadClient plus the
// scoped MR-bead listing so the poller can be unit-tested with a tiny mock.
type GitLabUpstreamBeadClient interface {
	// ListOpenMergeRequestBeads returns open type=merge-request beads scoped to
	// the given label (empty = all). The poller passes the upstream-mirror
	// marker so it only reconciles human-owned upstream MR mirror beads.
	ListOpenMergeRequestBeads(ctx context.Context, label string) ([]*beadsapi.BeadDetail, error)
	UpsertMergeRequestBead(ctx context.Context, f beadsapi.MergeRequestBeadFields, closed bool) (string, error)
}

// GitLabUpstreamPoller is the missed-webhook BACKSTOP for upstream peel MRs —
// NOT the primary path. The real-monorepo `merge_request` webhook (registered
// against GitLabWebhookHandlerWithConfig) is the primary signal: it is fully
// payload-driven, so it closes the mirror bead + stamps mr_outcome=merged
// without any host-pinned GitLab client. This poller exists only to recover
// the rare merge whose webhook never arrived (delivery failure, bridge
// downtime).
//
// It is deliberately SCOPED, unlike GitLabPoller (which scans every merged MR
// in a whole group): it enumerates only the small set of OPEN upstream-mirror
// beads (label-filtered, e.g. "upstream:true" from FW-5) and, for each, does a
// single per-MR GET against the real monorepo. This keeps it cheap and means
// it touches nothing but human-owned peel MRs — honoring the read-only-mirror
// invariant (ka-0000003234): it only pulls state FROM upstream, never pushes.
//
// Design source of truth: R2 artifact /bundle/ka-0000002869 §7 ("Reverse
// provenance") + §10 item 17 (FW-2). Sub-epic E-PROV (ka-0000001443).
type GitLabUpstreamPoller struct {
	gitlab       *GitLabClient
	daemon       GitLabUpstreamBeadClient
	logger       *slog.Logger
	pollInterval time.Duration
	// scopeLabel filters the mirror beads this poller reconciles. The
	// upstream-mirror marker (FW-5, e.g. "upstream:true"); empty = all open MR
	// beads (used only by tests / single-host deployments without the marker).
	scopeLabel string
	// host, when set, scopes reconciliation to mirror beads whose mr_url lives
	// on this exact GitLab host — the upstream host. This guards the
	// dual-GitLab world (one beads daemon shared by a dark-pointed and an
	// upstream-pointed bridge): the upstream poller must never GET a dark MR
	// URL against the upstream API. Empty = match every host.
	host string
	// acceptance, when set, emits the R5 reverse-provenance acceptance signal
	// (E-PROV FW-3, ka-0000002343) after the backstop closes a mirror bead: it
	// stamps the producing peel-request bead(s) and never closes any dark work
	// bead. The backstop passes an empty mergedBy — the merger identity is only
	// on the webhook payload, not the scoped per-MR GET, and a re-poll must not
	// clobber a webhook-stamped upstream_merged_by (empty fields are skipped by
	// UpdateBeadFields). nil disables stamping from the backstop. See the
	// reconciler's idempotency guard: a webhook that already stamped wins.
	acceptance *UpstreamAcceptanceReconciler
}

// GitLabUpstreamPollerConfig holds configuration for the scoped upstream
// backstop poller.
type GitLabUpstreamPollerConfig struct {
	GitLab       *GitLabClient
	Daemon       GitLabUpstreamBeadClient
	Logger       *slog.Logger
	PollInterval time.Duration
	ScopeLabel   string
	Host         string
	// Acceptance is the FW-3 reverse-provenance reconciler. Optional; when set,
	// the backstop stamps the peel-request bead(s) after closing a mirror bead.
	Acceptance *UpstreamAcceptanceReconciler
}

// NewGitLabUpstreamPoller creates a new scoped upstream backstop poller.
func NewGitLabUpstreamPoller(cfg GitLabUpstreamPollerConfig) *GitLabUpstreamPoller {
	interval := cfg.PollInterval
	if interval <= 0 {
		interval = 10 * time.Minute
	}
	return &GitLabUpstreamPoller{
		gitlab:       cfg.GitLab,
		daemon:       cfg.Daemon,
		logger:       cfg.Logger,
		pollInterval: interval,
		scopeLabel:   cfg.ScopeLabel,
		host:         cfg.Host,
		acceptance:   cfg.Acceptance,
	}
}

// Run starts the polling loop. It blocks until ctx is canceled.
func (p *GitLabUpstreamPoller) Run(ctx context.Context) error {
	p.logger.Info("starting scoped upstream MR backstop poller",
		"poll_interval", p.pollInterval, "scope_label", p.scopeLabel, "host", p.host)

	// Initial poll.
	p.poll(ctx)

	ticker := time.NewTicker(p.pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			p.poll(ctx)
		}
	}
}

// poll enumerates the scoped open upstream-mirror beads and reconciles each one
// whose upstream MR has reached a terminal state (merged/closed) that the
// webhook missed.
func (p *GitLabUpstreamPoller) poll(ctx context.Context) {
	beads, err := p.daemon.ListOpenMergeRequestBeads(ctx, p.scopeLabel)
	if err != nil {
		p.logger.Error("upstream backstop poll: failed to list mirror beads", "error", err)
		return
	}
	if len(beads) == 0 {
		p.logger.Debug("upstream backstop poll: no open mirror beads in scope")
		return
	}

	checked, reconciled := 0, 0
	for _, bead := range beads {
		mrURL := bead.Fields["mr_url"]
		if mrURL == "" {
			continue
		}
		ref := ParseMRURL(mrURL)
		if ref == nil {
			p.logger.Warn("upstream backstop poll: unparseable mr_url", "bead", bead.ID, "mr_url", mrURL)
			continue
		}
		// Dual-GitLab safety: never GET a foreign-host MR URL against this
		// poller's (upstream) GitLab API. Mirrors GitLabSync.hostMatches.
		if p.host != "" && ref.Host != p.host {
			continue
		}
		checked++

		mr, err := p.gitlab.GetMergeRequestByPath(ctx, ref.ProjectPath, ref.IID)
		if err != nil {
			p.logger.Warn("upstream backstop poll: failed to fetch MR",
				"bead", bead.ID, "mr_url", mrURL, "error", err)
			continue
		}
		// Only act on a terminal state the webhook should have delivered. An
		// opened/locked MR needs no backstop action.
		if mr.State != "merged" && mr.State != "closed" {
			continue
		}

		p.logger.Info("upstream backstop poll: terminal MR missed by webhook — closing mirror bead",
			"bead", bead.ID, "mr_url", mrURL, "state", mr.State)

		f := beadsapi.MergeRequestBeadFields{
			MRURL:        mr.WebURL,
			MRState:      mr.State,
			MROutcome:    inferUpstreamOutcome(mr.State),
			Title:        mr.Title,
			SourceBranch: mr.SourceBranch,
			TargetBranch: mr.TargetBranch,
		}
		if mr.Author != nil {
			f.Author = mr.Author.Username
		}
		// The mirror bead is keyed by mr_url; upsert with closed=true so the
		// bead is closed and mr_outcome is stamped. This is exactly what the
		// webhook merge path does — the backstop just reaches the same end
		// state when the webhook never fired.
		mrBeadID, uerr := p.daemon.UpsertMergeRequestBead(ctx, f, true)
		if uerr != nil {
			p.logger.Error("upstream backstop poll: failed to upsert/close mirror bead",
				"bead", bead.ID, "mr_url", mrURL, "error", uerr)
			continue
		}
		reconciled++

		// Reverse-provenance acceptance signal (E-PROV FW-3, ka-0000002343).
		// The mirror bead is now closed; stamp the producing peel-request
		// bead(s). mergedBy is empty — the scoped per-MR GET does not carry a
		// resolved human-operator id, and the reconciler's idempotency guard
		// means a webhook that already stamped (with merged_by) wins. Never
		// closes a dark work bead.
		if p.acceptance != nil && mrBeadID != "" {
			p.acceptance.Reconcile(ctx, mrBeadID, "")
		}
	}
	p.logger.Debug("upstream backstop poll complete",
		"in_scope", len(beads), "checked", checked, "reconciled", reconciled)
}

// inferUpstreamOutcome maps a GitLab MR state into the mr_outcome enum used on
// the mirror bead. Mirrors beadsapi.inferOutcome (unexported there): "merged"
// stays "merged"; "closed" (and anything else terminal) collapses to "closed".
func inferUpstreamOutcome(state string) string {
	if state == "merged" {
		return "merged"
	}
	return "closed"
}
