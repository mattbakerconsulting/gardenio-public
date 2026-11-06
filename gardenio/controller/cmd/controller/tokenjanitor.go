// Async orphan-sweep janitor for per-agent tokens.
//
// The synchronous revoke-on-done path in handleEvent is primary, but it can
// miss tokens: a despawn event may be dropped, the controller may restart
// mid-revoke, or a Secret-delete may transiently fail. The janitor is the
// reconciler safety net (per-agent-tokens-lifecycle.md ADR §6.3): it lists the
// agent-token Secrets, and for any whose owning agent bead is already closed,
// it revokes the fleet token and deletes the Secret, recording the event with
// revocation_type=async_janitor.
//
// Revocation latency for the async path is measured from the bead's closed_at
// (the AgentDone reference point, telemetry-plan §1.3) to the moment the sweep
// completes the revoke — so the histogram captures how long an orphan lingered
// before the janitor caught it, on the same SLO scale as the synchronous path.
//
// Privacy invariant: the janitor identifies tokens via the gardenio.io/bead and
// gardenio.io/provider Secret labels written at provision time — never via the
// raw token bytes — and the only label that reaches a metric is the bounded
// provider enum.
package main

import (
	"context"
	"log/slog"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"gardenio/controller/internal/beadsapi"
)

// agentTokenSecretLabelSelector selects the K8s Secrets created by
// provisionAgentToken. gardenio.io/type=agent-token is set on every per-agent
// token Secret.
//
// GitHub App installation-token Secrets (gardenio.io/provider=github) share the
// type label but are owned by githubAppIssuer.sweep, which knows how to revoke
// them at GitHub; the beads sweep must leave them alone, hence the != clause
// (which also matches Secrets with no provider label at all, i.e. pre-provider
// beads tokens).
const agentTokenSecretLabelSelector = "gardenio.io/type=agent-token,gardenio.io/provider!=" + tokenProviderGithub

// janitorDaemon is the slice of the beads client the janitor needs: inspect a
// bead's status/closed_at (to decide whether a token is orphaned) and revoke
// the fleet token. *beadsapi.Client satisfies it; tests use a fake.
type janitorDaemon interface {
	GetBead(ctx context.Context, beadID string) (*beadsapi.BeadDetail, error)
	fleetTokenRevoker
}

// tokenJanitor reconciles orphaned per-agent token Secrets: those whose owning
// agent bead is closed but whose token was never revoked by the synchronous
// despawn path.
type tokenJanitor struct {
	daemon    janitorDaemon
	k8sClient kubernetes.Interface
	namespace string
	logger    *slog.Logger
	tm        *tokenMetrics

	// beadsTokens gates the beads fleet-token sweep (PER_AGENT_TOKENS). The
	// zero value (false) is only reached by tests that predate the field, so
	// newTokenJanitor defaults it to true.
	beadsTokens bool
	// github, when non-nil, additionally sweeps/refreshes GitHub App token
	// Secrets in githubNamespaces.
	github           *githubAppIssuer
	githubNamespaces []string
}

// newTokenJanitor builds a tokenJanitor. A nil daemon or k8sClient yields a
// janitor whose sweep is a no-op (defensive — the caller only constructs one
// when per-agent tokens are enabled).
func newTokenJanitor(daemon janitorDaemon, k8sClient kubernetes.Interface, namespace string, logger *slog.Logger, tm *tokenMetrics) *tokenJanitor {
	return &tokenJanitor{
		daemon:    daemon,
		k8sClient: k8sClient,
		namespace: namespace,
		logger:    logger,
		tm:        tm,

		beadsTokens: true,
	}
}

// tokenJanitorStats summarizes one sweep.
type tokenJanitorStats struct {
	Scanned int // agent-token Secrets examined
	Revoked int // orphans revoked this sweep
}

// sweep lists agent-token Secrets and revokes any whose owning bead is closed.
// It is best-effort: per-Secret failures are logged and skipped so one bad
// Secret can't stall the sweep. Returns counts for observability/logging.
func (j *tokenJanitor) sweep(ctx context.Context) tokenJanitorStats {
	var stats tokenJanitorStats
	if j == nil || j.daemon == nil || j.k8sClient == nil {
		return stats
	}
	if j.github != nil {
		gs := j.github.sweep(ctx, j.daemon, j.githubNamespaces)
		stats.Scanned += gs.Scanned
		stats.Revoked += gs.Revoked
	}
	if !j.beadsTokens {
		return stats
	}

	secrets, err := j.k8sClient.CoreV1().Secrets(j.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: agentTokenSecretLabelSelector,
	})
	if err != nil {
		j.logger.Warn("token janitor: failed to list agent-token secrets", "error", err)
		return stats
	}

	for i := range secrets.Items {
		sec := &secrets.Items[i]
		beadID := sec.Labels["gardenio.io/bead"]
		if beadID == "" {
			// Pre-label Secret (provisioned before the gardenio.io/bead label
			// existed). We can't safely map it back to a bead, so leave it for
			// the TTL safety net rather than risk revoking a live token.
			continue
		}
		stats.Scanned++

		bead, err := j.daemon.GetBead(ctx, beadID)
		if err != nil {
			j.logger.Warn("token janitor: failed to fetch owning bead",
				"bead", beadID, "error", err)
			continue
		}
		if bead == nil || bead.Status != "closed" {
			continue // agent still live — synchronous path owns revocation.
		}

		// Orphan: the bead is closed but the token Secret survives. Revoke it
		// with latency measured from the bead's closed_at.
		ns := sec.Namespace
		if ns == "" {
			ns = j.namespace
		}
		// The gardenio.io/provider label is always "beads" today; revokeAgentToken
		// records under tokenProviderBeads, the only provider the janitor sweeps.
		// T6 (gitlab/github) will grow a provider-aware revoke path keyed off the
		// label.
		revokeAgentToken(ctx, j.logger, j.daemon, j.k8sClient, beadID, ns, j.tm, revocationTypeAsyncJanitor, parseBeadCloseTime(bead.ClosedAt))
		stats.Revoked++
	}

	return stats
}

// parseBeadCloseTime parses a bead's closed_at timestamp. The daemon emits
// RFC3339; an unparseable or empty value yields the zero time, which
// recordRevocation treats as "reference point unavailable" (count only, no
// latency observation) rather than emitting a bogus latency.
func parseBeadCloseTime(closedAt string) time.Time {
	if closedAt == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, closedAt)
	if err != nil {
		return time.Time{}
	}
	return t
}
