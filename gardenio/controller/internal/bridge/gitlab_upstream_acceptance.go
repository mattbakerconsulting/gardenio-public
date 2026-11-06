package bridge

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// UpstreamAcceptanceBeadClient is the narrow bead-daemon surface the
// reverse-provenance acceptance reconciler needs. beadsapi.Client satisfies it.
type UpstreamAcceptanceBeadClient interface {
	// GetBead fetches a single bead by ID (used to read the just-closed MR
	// mirror bead's mr_url / mr_outcome / labels).
	GetBead(ctx context.Context, beadID string) (*beadsapi.BeadDetail, error)
	// ProducingTaskBeadsForMR walks the reverse `produces` edges out of the MR
	// bead and returns the task beads that produced it — for an upstream-mirror
	// bead these are the peel-request beads (linked at `gb mr link` time, FW-5).
	ProducingTaskBeadsForMR(ctx context.Context, mrBeadID string) ([]*beadsapi.BeadDetail, error)
	// UpdateBeadFields stamps acceptance/rejection fields on a peel-request bead.
	UpdateBeadFields(ctx context.Context, beadID string, fields map[string]string) error
	// AddComment posts the human-readable acceptance/rejection note.
	AddComment(ctx context.Context, beadID, author, text string) error
}

// upstreamAcceptanceAuthor is the service identity stamped on reconciler
// comments. Mirrors the "gitlab-bridge" author used elsewhere in this package.
const upstreamAcceptanceAuthor = "gitlab-bridge"

// UpstreamAcceptanceReconciler emits the R5 reverse-provenance acceptance
// signal (E-PROV FW-3, ka-0000002343). When one of our peel Draft MRs reaches a
// terminal state on the real monorepo, the upstream-mirror MR bead is closed
// (by the FW-2 webhook merge path or the scoped backstop poller). This
// reconciler then walks the reverse `produces` edge from that MR bead to the
// peel-request bead(s) and stamps the acceptance outcome:
//
//   - merged   → upstream_accepted_at / upstream_mr_url / upstream_merged_by +
//     an acceptance comment.
//   - closed   → upstream_rejected_at / upstream_mr_url + a rejection comment.
//
// Invariants (R2 artifact /bundle/ka-0000002869 §7 + §10 item 18):
//   - It NEVER closes any bead. The peel-request stays open (it is a durable
//     provenance anchor), and — critically — the dark work beads that the
//     peeled slice was extracted from MUST NOT be auto-closed: dark lives ahead
//     of the peeled slice, so an upstream merge of one slice says nothing about
//     the rest of the dark work. This reconciler only ever UpdateBeadFields +
//     AddComment.
//   - It is read-only against upstream: it acts purely on bead state the FW-2
//     close already produced. It never calls a GitLab API and never fires the
//     upstream Jira write path (honors read-only-mirror invariant ka-0000003234).
//   - It is SCOPED to upstream-mirror beads (scopeLabel, default "upstream:true"
//     from FW-5). A normal dark/prod MR merge — whose producing beads are real
//     work beads, not peel-requests — must never be stamped with acceptance
//     fields. An empty scopeLabel disables the guard (tests / single-host).
//   - It is idempotent: a peel-request already stamped for this outcome is
//     skipped, so a webhook + backstop double-delivery stamps once and comments
//     once.
type UpstreamAcceptanceReconciler struct {
	daemon UpstreamAcceptanceBeadClient
	logger *slog.Logger
	// scopeLabel restricts reconciliation to MR beads carrying this label
	// (the FW-5 upstream-mirror marker, default "upstream:true"). Empty = act on
	// every closed MR bead handed to Reconcile (tests / single-host).
	scopeLabel string
	// now is injectable for deterministic tests; defaults to time.Now.
	now func() time.Time
}

// UpstreamAcceptanceReconcilerConfig configures the reconciler.
type UpstreamAcceptanceReconcilerConfig struct {
	Daemon     UpstreamAcceptanceBeadClient
	Logger     *slog.Logger
	ScopeLabel string // default "upstream:true"; empty disables the scope guard
}

// NewUpstreamAcceptanceReconciler constructs the reconciler. ScopeLabel
// defaults to "upstream:true" when unset; pass an explicit empty string only to
// disable the guard.
func NewUpstreamAcceptanceReconciler(cfg UpstreamAcceptanceReconcilerConfig) *UpstreamAcceptanceReconciler {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &UpstreamAcceptanceReconciler{
		daemon:     cfg.Daemon,
		logger:     logger,
		scopeLabel: cfg.ScopeLabel,
		now:        time.Now,
	}
}

// Reconcile stamps the reverse-provenance acceptance signal onto the
// peel-request bead(s) that produced the just-closed upstream-mirror MR bead.
//
// mrBeadID is the closed type=merge-request bead. mergedBy is the resolved
// identity of the human who merged (webhook actor / GitLab merged_by); it is
// empty for a closed-without-merge outcome. Reconcile re-reads the MR bead to
// determine the terminal outcome (mr_outcome) and to apply the scope guard, so
// callers do not need to pass the outcome.
//
// Best-effort: all failures are logged and skipped — a stamping miss must never
// break the MR-close path that invoked it.
func (r *UpstreamAcceptanceReconciler) Reconcile(ctx context.Context, mrBeadID, mergedBy string) {
	if r == nil || mrBeadID == "" {
		return
	}

	mrBead, err := r.daemon.GetBead(ctx, mrBeadID)
	if err != nil || mrBead == nil {
		r.logger.Warn("upstream acceptance: failed to fetch MR bead",
			"mr_bead", mrBeadID, "error", err)
		return
	}

	// Scope guard: only upstream-mirror beads carry the FW-5 marker. A normal
	// dark/prod MR close lands here too (the webhook merge path is host-blind),
	// and its producing beads are real work beads — stamping them with upstream
	// acceptance would be wrong. Skip silently.
	if r.scopeLabel != "" && !hasLabel(mrBead.Labels, r.scopeLabel) {
		r.logger.Debug("upstream acceptance: MR bead not in scope, skipping",
			"mr_bead", mrBeadID, "scope_label", r.scopeLabel)
		return
	}

	mrURL := mrBead.Fields["mr_url"]
	outcome := mrBead.Fields["mr_outcome"]
	accepted := outcome == "merged"

	producers, err := r.daemon.ProducingTaskBeadsForMR(ctx, mrBeadID)
	if err != nil {
		r.logger.Warn("upstream acceptance: failed to walk producing beads",
			"mr_bead", mrBeadID, "error", err)
		return
	}
	if len(producers) == 0 {
		r.logger.Info("upstream acceptance: closed upstream-mirror bead has no producing peel-request",
			"mr_bead", mrBeadID, "mr_url", mrURL, "outcome", outcome)
		return
	}

	stampedAt := r.now().UTC().Format(time.RFC3339)
	stamped := 0
	for _, p := range producers {
		if p == nil || p.ID == "" {
			continue
		}
		// Idempotency: skip a peel-request already stamped for this outcome so a
		// webhook + backstop double-delivery does not re-stamp or re-comment.
		if accepted && p.Fields["upstream_accepted_at"] != "" {
			continue
		}
		if !accepted && p.Fields["upstream_rejected_at"] != "" {
			continue
		}

		fields := map[string]string{}
		if mrURL != "" {
			fields["upstream_mr_url"] = mrURL
		}
		var comment string
		if accepted {
			fields["upstream_accepted_at"] = stampedAt
			if mergedBy != "" {
				fields["upstream_merged_by"] = mergedBy
			}
			comment = formatAcceptanceComment(mrURL, mergedBy, stampedAt)
		} else {
			fields["upstream_rejected_at"] = stampedAt
			comment = formatRejectionComment(mrURL, stampedAt)
		}

		// Stamp fields — NEVER close. The peel-request stays open as a durable
		// provenance anchor and the dark work beads behind it are untouched.
		if uerr := r.daemon.UpdateBeadFields(ctx, p.ID, fields); uerr != nil {
			r.logger.Error("upstream acceptance: failed to stamp peel-request bead",
				"peel_request", p.ID, "mr_bead", mrBeadID, "error", uerr)
			continue
		}
		if cerr := r.daemon.AddComment(ctx, p.ID, upstreamAcceptanceAuthor, comment); cerr != nil {
			r.logger.Warn("upstream acceptance: failed to comment on peel-request bead",
				"peel_request", p.ID, "mr_bead", mrBeadID, "error", cerr)
		}
		stamped++
		r.logger.Info("upstream acceptance: stamped peel-request bead",
			"peel_request", p.ID, "mr_bead", mrBeadID, "mr_url", mrURL,
			"outcome", outcome, "merged_by", mergedBy)
	}

	r.logger.Debug("upstream acceptance reconcile complete",
		"mr_bead", mrBeadID, "producers", len(producers), "stamped", stamped, "accepted", accepted)
}

// formatAcceptanceComment renders the human-readable acceptance note posted on
// the peel-request bead when its upstream MR merged.
func formatAcceptanceComment(mrURL, mergedBy, at string) string {
	by := mergedBy
	if by == "" {
		by = "(unknown)"
	}
	return fmt.Sprintf(
		"✅ Upstream accepted — the peeled slice merged on the real monorepo.\n\n"+
			"- Upstream MR: %s\n- Merged by: %s\n- Accepted at: %s\n\n"+
			"This is the R5 reverse-provenance acceptance signal. The dark work behind "+
			"this slice is intentionally left open — dark runs ahead of the peeled slice.",
		fallbackDash(mrURL), by, at)
}

// formatRejectionComment renders the note posted when the upstream MR was
// closed without merging.
func formatRejectionComment(mrURL, at string) string {
	return fmt.Sprintf(
		"🚫 Upstream rejected — the peel MR was closed on the real monorepo without merging.\n\n"+
			"- Upstream MR: %s\n- Closed at: %s\n\n"+
			"The peel-request stays open for a follow-up slice; the dark work behind it is untouched.",
		fallbackDash(mrURL), at)
}

func fallbackDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}
