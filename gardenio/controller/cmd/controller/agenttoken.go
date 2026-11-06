// Per-agent scoped beads daemon tokens.
//
// When PER_AGENT_TOKENS is enabled, each agent gets a unique fleet token
// scoped to its project instead of sharing the global BEADS_DAEMON_TOKEN.
// This prevents cross-project data access if an agent token is leaked.
//
// Flow:
//  1. On agent spawn: generate UUID token → register as fleet token → create K8s Secret
//  2. Agent pod reads token from its dedicated K8s Secret via secretKeyRef
//  3. Beads daemon validates the token and enforces project scope on all requests
//  4. On agent done/kill/stop: revoke fleet token → delete K8s Secret
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/podmanager"
	"gardenio/controller/internal/reconciler"
)

// defaultPATTokenTTL is the default expiration window for per-agent fleet
// tokens issued by the controller. The controller's revoke-on-done path stays
// primary; TTL is the safety net for failed revocation. Per ADR §6.2 / §6.3
// (gardenio/docs/research/c1-token-ttl-refresh-design.md), 24h is generous
// enough that a normal pod lifecycle never bumps into it.
const defaultPATTokenTTL = 24 * time.Hour

// defaultPATRefreshTTL bounds the companion refresh token's lifetime. Long
// enough that a multi-day agent run survives any number of access-token TTL
// crossings; bounded so a leaked refresh isn't valid forever. Matches the
// ADR §6.3 recommendation of 30 days for agent refreshes.
const defaultPATRefreshTTL = 30 * 24 * time.Hour

// agentTokenBridgeName returns the bridge-token identity (fleet attribution
// name) for a per-agent token. Uses the bead ID as the canonical identifier.
func agentTokenBridgeName(beadID string) string {
	return "agent-" + beadID
}

// agentTokenSubject returns the audit subject the daemon attributes to
// requests authenticated by a per-agent token: the agent's own name, the same
// value agents send today as X-Actor, so every ownership gate that compares the
// actor with a bead's assignee keeps working (P7 Task 1). Empty stays empty.
func agentTokenSubject(agentName string) string {
	return agentName
}

// agentTokenSecretName returns the K8s Secret name for a per-agent token. Bead
// ids are mixed-case (gd-F149B6RdbT) but a Secret name is a DNS-1123 subdomain
// (lowercase only), so the id is lowercased; the exact id stays in the
// gardenio.io/bead label, which is what the orphan-sweep janitor reads back.
func agentTokenSecretName(beadID string) string {
	return "agent-token-" + strings.ToLower(beadID)
}

// agentTokenNameRE is the shape an agent name must have to be minted a
// per-agent token: lowercase alphanumerics and '-' only. The daemon turns the
// name into the token's audit subject and some authorization gates compare a
// fleet subject against well-known service names ("bridge:acme-scif" carries
// SCIF approval authority), so a name with ':' or '@' must never reach the mint.
var agentTokenNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,127}$`)

// agentTokenBeadIDRE is the shape a bead id must have to become part of a fleet
// name and a Secret name.
var agentTokenBeadIDRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,63}$`)

// validateAgentTokenIdentity refuses to mint for an agent name or bead id whose
// shape could be read as anything other than an ordinary agent. An empty agent
// name is allowed (the daemon falls back to the fleet name as the subject).
func validateAgentTokenIdentity(beadID, agentName string) error {
	if !agentTokenBeadIDRE.MatchString(beadID) {
		return fmt.Errorf("refusing per-agent token: bead id %q is not [A-Za-z0-9-]", beadID)
	}
	if agentName != "" && !agentTokenNameRE.MatchString(agentName) {
		return fmt.Errorf("refusing per-agent token: agent name %q is not [a-z0-9-]", agentName)
	}
	return nil
}

// generateToken creates a cryptographically random 32-byte hex token.
func generateToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generating random token: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// provisionAgentToken generates a per-agent token, registers it with the beads
// daemon as a project-scoped fleet token, creates a K8s Secret, and returns
// the secret name to use as DaemonTokenSecret in the pod spec.
//
// created reports whether this call created the Secret. When the Secret already
// exists (a redelivered spawn, a stuck-restart recreate, a drift recreate whose
// delete raced the revoke) its token and refresh_token are replaced with the
// freshly minted pair: leaving the old pair would hand the next pod a token the
// daemon may no longer hold. The old token is not revoked here, because a live
// pod from the earlier spawn may still be using it; revoke-on-done (which
// revokes by fleet name) and the 24h TTL retire it.
//
// tm (may be nil) records the issuance outcome on
// gardenio_agent_token_provisions_total: status=error on any failure,
// status=success once the upstream registration succeeds and the K8s Secret
// exists (the IsAlreadyExists short-circuit counts as success — the token is
// usable). No raw token bytes or provider id ever reach a metric label.
func provisionAgentToken(ctx context.Context, logger *slog.Logger, daemon *beadsapi.Client, k8sClient kubernetes.Interface, beadID, agentName, project, namespace string, tm *tokenMetrics) (secretName string, created bool, err error) {
	if err := validateAgentTokenIdentity(beadID, agentName); err != nil {
		tm.recordProvision(tokenProviderBeads, tokenProvisionError)
		return "", false, err
	}
	token, err := generateToken()
	if err != nil {
		tm.recordProvision(tokenProviderBeads, tokenProvisionError)
		return "", false, err
	}

	fleet := agentTokenBridgeName(beadID)
	secretName = agentTokenSecretName(beadID)

	// Register with beads daemon (project-scoped) with a default 24h TTL as a
	// safety net against failed revoke-on-done paths. We also mint a paired
	// refresh token (defaultPATRefreshTTL) so the agent's long-lived clients
	// can survive an access-token TTL expiry mid-run without a respawn.
	// Subject is the agent name so daemon-side audit attribution is
	// unambiguous instead of falling back to the fleet name (C1.4).
	subject := agentTokenSubject(agentName)
	// Agent PAT tokens are ordinary fleet identities — never privileged.
	issued, err := daemon.RegisterBridgeTokenWithRefresh(ctx, token, fleet, project, subject, defaultPATTokenTTL, defaultPATRefreshTTL, false)
	if err != nil {
		tm.recordProvision(tokenProviderBeads, tokenProvisionError)
		return "", false, fmt.Errorf("registering fleet token: %w", err)
	}

	// Create K8s Secret with the access + refresh token pair. The refresh is
	// stored alongside the access token under a separate data key so that
	// (a) consumers that only need the access token (BEADS_DAEMON_TOKEN env)
	// don't accidentally pick up the refresh, and (b) podmanager can mount
	// each into a distinct env var.
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: namespace,
			Labels: map[string]string{
				podmanager.LabelApp: podmanager.LabelAppValue,
				"gardenio.io/type":  "agent-token",
				// provider disambiguates per-provider tokens (ADR §4.2). Today
				// only the beads-daemon token is issued; gitlab/github are T6.
				"gardenio.io/provider": tokenProviderBeads,
				// bead lets the orphan-sweep janitor map a Secret back to its
				// owning agent bead without parsing the Secret name.
				"gardenio.io/bead":      beadID,
				podmanager.LabelProject: project,
			},
		},
		Type: corev1.SecretTypeOpaque,
		StringData: map[string]string{
			"token":         token,
			"refresh_token": issued.RefreshToken,
		},
	}
	secrets := k8sClient.CoreV1().Secrets(namespace)
	created = true
	if _, err := secrets.Create(ctx, secret, metav1.CreateOptions{}); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			// Roll back fleet token on secret creation failure.
			_ = daemon.RevokeBridgeToken(ctx, fleet)
			tm.recordProvision(tokenProviderBeads, tokenProvisionError)
			return "", false, fmt.Errorf("creating agent token secret: %w", err)
		}
		// Re-mint: the Secret from the earlier mint still holds the OLD pair.
		// Replace it so the pod we are about to create reads the token the
		// daemon just registered.
		created = false
		if _, uerr := secrets.Update(ctx, secret, metav1.UpdateOptions{}); uerr != nil {
			// Leave the Secret and the fleet's earlier token alone: they may
			// belong to a live pod. The token registered above expires on its TTL.
			tm.recordProvision(tokenProviderBeads, tokenProvisionError)
			return "", false, fmt.Errorf("updating agent token secret: %w", uerr)
		}
	}

	tm.recordProvision(tokenProviderBeads, tokenProvisionSuccess)
	logger.Info("provisioned per-agent token",
		"bead", beadID, "fleet", fleet, "secret", secretName, "project", project, "created", created)
	return secretName, created, nil
}

// fleetTokenRevoker is the slice of the beads client that revokeAgentToken
// needs. *beadsapi.Client satisfies it; the orphan-sweep janitor and tests can
// pass a narrower fake without dragging in the whole HTTP client.
type fleetTokenRevoker interface {
	RevokeBridgeToken(ctx context.Context, fleet string) error
}

// revokeAgentToken revokes a per-agent fleet token and deletes its K8s Secret.
// Errors are logged but not returned (best-effort cleanup).
//
// revType is the revocation-type label (despawn|stuck_restart|async_janitor)
// and doneAt is the AgentDone reference point for the revocation-latency
// histogram — the despawn-event receipt time for the synchronous paths, or the
// bead's closed_at for the janitor path. A zero doneAt records the count only.
// tm may be nil (no-op). The revocation counter is always incremented, even
// when upstream revoke or Secret delete fails, because this best-effort path
// always reaches the "we attempted revocation" point (the janitor retries the
// upstream side on the next sweep).
func revokeAgentToken(ctx context.Context, logger *slog.Logger, daemon fleetTokenRevoker, k8sClient kubernetes.Interface, beadID, namespace string, tm *tokenMetrics, revType string, doneAt time.Time) {
	fleet := agentTokenBridgeName(beadID)
	secretName := agentTokenSecretName(beadID)

	if err := daemon.RevokeBridgeToken(ctx, fleet); err != nil {
		logger.Warn("failed to revoke fleet token", "fleet", fleet, "error", err)
	}

	if err := k8sClient.CoreV1().Secrets(namespace).Delete(ctx, secretName, metav1.DeleteOptions{}); err != nil {
		if !apierrors.IsNotFound(err) {
			logger.Warn("failed to delete agent token secret", "secret", secretName, "error", err)
		}
	}

	tm.recordRevocation(tokenProviderBeads, revType, doneAt)
	logger.Info("revoked per-agent token", "bead", beadID, "fleet", fleet, "secret", secretName,
		"revocation_type", revType)
}

// releaseMintedAgentToken undoes provisionAgentToken when the spawn fails after
// the mint (the GitHub App credential was refused, or CreateAgentPod errored).
// Without it the fleet token stays valid until its 24h TTL or the janitor sweep
// with no pod to use it. It acts only when this spawn created the Secret: a
// pre-existing Secret was refreshed in place and may belong to a live pod,
// whose token revoking by fleet name would cut. No-op when per-agent tokens are
// off or the bead id is empty. Uses a context that survives cancellation of the
// failed spawn.
func releaseMintedAgentToken(ctx context.Context, logger *slog.Logger, daemon fleetTokenRevoker, k8sClient kubernetes.Interface, beadID, namespace string, tm *tokenMetrics, minted bool) {
	if !minted || beadID == "" {
		return
	}
	revokeAgentToken(context.WithoutCancel(ctx), logger, daemon, k8sClient, beadID, namespace, tm, revocationTypeDespawn, time.Time{})
}

// reconcilerAgentTokenApplier adapts provisionAgentToken to the reconciler's
// pre-create seam, so a pod the reconciler builds (spot-loss recreate, image
// update, a spawn made while the controller was down) mounts its own per-agent
// token instead of the shared one applyCommonConfig sets. machineDaemon mints
// (the controller's own principal, as at the event door); daemon revokes.
func reconcilerAgentTokenApplier(logger *slog.Logger, machineDaemon *beadsapi.Client, daemon fleetTokenRevoker, k8sClient kubernetes.Interface, tm *tokenMetrics) reconciler.AgentTokenApplier {
	return reconciler.AgentTokenApplier{
		Apply: func(ctx context.Context, spec *podmanager.AgentPodSpec, beadID, agentName string) (bool, error) {
			if beadID == "" {
				return false, nil
			}
			secretName, minted, err := provisionAgentToken(ctx, logger, machineDaemon, k8sClient, beadID, agentName, spec.Project, spec.Namespace, tm)
			if err != nil {
				return false, err
			}
			spec.DaemonTokenSecret = secretName
			return minted, nil
		},
		Release: func(ctx context.Context, spec podmanager.AgentPodSpec, beadID string) {
			releaseMintedAgentToken(ctx, logger, daemon, k8sClient, beadID, spec.Namespace, tm, true)
		},
	}
}
