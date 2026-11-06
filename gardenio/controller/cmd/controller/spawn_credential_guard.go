package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/podmanager"
)

// claudeOAuthEnvName is the env var the agent entrypoint reads for the Claude
// Code OAuth token. applyAccountConfig injects it as a SecretEnv from the
// resolved account bead's `credential` (typically secret:gardenio-claude-oauth-<user>).
const claudeOAuthEnvName = "CLAUDE_CODE_OAUTH_TOKEN"

// spawnRefusal describes why a crew/gardener spawn was refused by the
// fail-closed credential guard. A zero value (refuse=false) means "allow".
type spawnRefusal struct {
	refuse bool
	// permanent marks a refusal that will never resolve on retry (e.g. a
	// project-less agent bead). Permanent refusals mark the agent bead failed
	// so the reconciler stops re-driving them; transient refusals (a missing
	// OAuth secret that the secretreconciler / ESO may still be syncing) leave
	// the bead spawnable so a later pass succeeds once the secret lands.
	permanent bool
	reason    string
	// harness marks a harness misconfiguration (unknown value, or a harness the
	// resolved account's provider cannot serve). The reason is written onto the
	// failed agent bead; no escalation is filed, because the fix is a bead edit
	// the reason already names, not credential provisioning.
	harness bool
}

// credentialKind names which provider credential a spec carries, for messages.
const (
	credentialKindClaudeOAuth = "Claude OAuth"
	credentialKindCodexLogin  = "Codex login (-session)"
	credentialKindGeminiLogin = "Gemini login (oauth_creds.json)"
)

// credentialSecretRef returns the K8s Secret name backing the credential this
// spec's account provider needs, which kind of credential it is, and whether
// such a credential was resolved at all: the CLAUDE_CODE_OAUTH_TOKEN SecretEnv
// for an Anthropic-style account, or the derived -session Secret mounted for a
// provider=openai (Codex subscription) account. A spec with neither (hasRef=false)
// is a project that uses direct-provider auth (Bedrock, a LiteLLM virtual key,
// etc.) rather than a per-operator subscription login — a legitimate, supported
// configuration that is NOT refused here.
func credentialSecretRef(spec podmanager.AgentPodSpec) (secretName, kind string, hasRef bool) {
	if spec.CodexAuthSecret != "" {
		return spec.CodexAuthSecret, credentialKindCodexLogin, true
	}
	if spec.GeminiOAuthSecret != "" {
		return spec.GeminiOAuthSecret, credentialKindGeminiLogin, true
	}
	for _, se := range spec.SecretEnv {
		if se.EnvName == claudeOAuthEnvName {
			return se.SecretName, credentialKindClaudeOAuth, true
		}
	}
	return "", "", false
}

// evaluateSpawnCredentials is the fail-closed guard that decides whether a
// crew/gardener pod may be spawned (ka-0000007608). It refuses two cases that
// today silently yield a loginless pod (one with no CLAUDE_CODE_OAUTH_TOKEN):
//
//  1. Empty project. applyAccountConfig keys account/credential resolution off
//     spec.Project; an empty gardenio.io/project early-returns there, injecting
//     NO OAuth SecretEnv, and verifySecretsExist then has nothing to check — so
//     a credential-less pod spawns. This is a permanent misconfiguration.
//  2. A resolved credential secret that does not exist in the pod's
//     namespace (for a tenant land, the release namespace it is copied from): the CLAUDE_CODE_OAUTH_TOKEN secret, or for a provider=openai
//     account the derived Codex -session secret (written by the keeper). This is
//     transient by default (the project-scoped ExternalSecret may still be
//     syncing from AWS Secrets Manager; the keeper may not have minted the
//     -session secret yet), so it is refused but not marked permanent.
//
// Specs with no OAuth SecretEnv at all (direct-provider auth) are allowed
// unchanged — the happy path for credentialed pods is never altered. A
// non-NotFound K8s API error while probing the secret does NOT refuse: a
// transient API blip must not fail-closed the whole fleet, and the existing
// verifySecretsExist admission gate still backstops a genuinely-missing secret
// at CreateAgentPod time.
func evaluateSpawnCredentials(ctx context.Context, k8s kubernetes.Interface, logger *slog.Logger, spec podmanager.AgentPodSpec) spawnRefusal {
	// Harness misconfiguration fails fast and readably before any credential
	// probing: a codex role on an anthropic account would otherwise boot a pod
	// that cannot authenticate.
	if spec.HarnessError != "" {
		return spawnRefusal{
			refuse:    true,
			permanent: true,
			harness:   true,
			reason:    "invalid agent harness: " + spec.HarnessError,
		}
	}
	if spec.Project == "" {
		return spawnRefusal{
			refuse:    true,
			permanent: true,
			reason: "agent spawn has no project (gardenio.io/project is empty); " +
				"cannot resolve a per-operator Claude OAuth credential — refusing to launch a loginless pod",
		}
	}

	secretName, kind, hasRef := credentialSecretRef(spec)
	if !hasRef {
		// Direct-provider auth project (no subscription credential). Allowed.
		return spawnRefusal{}
	}
	if secretName == "" {
		// An OAuth env was wired with no backing secret name — malformed
		// credential. Treat as missing (transient) rather than spawn loginless.
		return spawnRefusal{
			refuse: true,
			reason: fmt.Sprintf("%s credential has no backing secret name", kind),
		}
	}

	if k8s == nil {
		// No client to probe with — defer to verifySecretsExist at create time.
		return spawnRefusal{}
	}
	// A tenant land's credential is copied into its namespace at pod create
	// (gd-gokoYMl5px), so it is probed where it is copied from.
	ns := podmanager.LandSecretSourceNamespace(spec, secretName)
	_, err := k8s.CoreV1().Secrets(ns).Get(ctx, secretName, metav1.GetOptions{})
	if err == nil {
		return spawnRefusal{}
	}
	if apierrors.IsNotFound(err) {
		return spawnRefusal{
			refuse: true,
			reason: fmt.Sprintf("%s secret %s/%s not found — refusing to launch a loginless pod",
				kind, ns, secretName),
		}
	}
	// Non-NotFound API error — do not fail-closed on a transient blip.
	logger.Warn("spawn credential guard: error probing credential secret, allowing spawn to proceed (verifySecretsExist still backstops create)",
		"agent", spec.AgentName, "project", spec.Project,
		"secret", secretName, "namespace", ns, "error", err)
	return spawnRefusal{}
}

// buildSpawnRefusalEscalation constructs the type=escalation bead that records
// a refused loginless spawn for the operator queue. Pure (no I/O) so the field
// shape is unit-testable. Mirrors `gb escalation create`'s field schema:
// operation_type + operation_spec + context, with a project label when known.
func buildSpawnRefusalEscalation(spec podmanager.AgentPodSpec, agentBeadID, reason string) beadsapi.CreateBeadRequest {
	specObj := map[string]any{
		"agent":   spec.AgentName,
		"project": spec.Project,
		"role":    spec.Role,
		"reason":  reason,
	}
	if name, kind, ok := credentialSecretRef(spec); ok {
		specObj["credential_secret"] = name
		if kind == credentialKindClaudeOAuth {
			specObj["oauth_secret"] = name
		}
		specObj["namespace"] = spec.Namespace
	}
	fields := map[string]any{
		"operation_type": "credential_provisioning",
		"operation_spec": specObj,
		"context": fmt.Sprintf(
			"Controller refused to spawn agent %q: %s. Provision the per-operator "+
				"credential secret (gardenio-claude-oauth-<user>, or for a Codex account "+
				"the gardenio-codex-auth*-session secret the keeper writes) and/or fix the "+
				"agent bead's project, then re-spawn.", spec.AgentName, reason),
	}
	if agentBeadID != "" {
		fields["requesting_agent"] = agentBeadID
	}
	// Best-effort marshal; on error the caller falls back to nil Fields.
	fieldsJSON, _ := json.Marshal(fields)

	labels := []string{"guard:loginless-spawn"}
	if spec.Project != "" {
		labels = append(labels, "project:"+spec.Project)
	}

	title := fmt.Sprintf("Refused loginless spawn: %s", spec.AgentName)

	req := beadsapi.CreateBeadRequest{
		Title:     title,
		Type:      "escalation",
		Kind:      "data",
		Fields:    json.RawMessage(fieldsJSON),
		Labels:    labels,
		Priority:  1,
		CreatedBy: "gardenio-controller",
	}
	if agentBeadID != "" {
		// Dedup re-deliveries of the same spawn event onto one escalation.
		req.IdempotencyKey = "loginless-spawn:" + agentBeadID
	}
	return req
}

// recordSpawnRefusal surfaces a refused spawn loudly: a structured error log,
// a best-effort escalation bead for the operator, and — for permanent refusals
// — marking the agent bead failed so the reconciler stops re-driving it. Every
// downstream write is best-effort; the refusal itself (no pod created) holds
// regardless of whether the daemon writes succeed.
func recordSpawnRefusal(ctx context.Context, logger *slog.Logger, daemon *beadsapi.Client, spec podmanager.AgentPodSpec, agentBeadID string, r spawnRefusal) {
	logger.Error("refusing agent spawn — fail-closed credential guard",
		"agent", spec.AgentName, "project", spec.Project, "role", spec.Role,
		"namespace", spec.Namespace, "bead", agentBeadID,
		"permanent", r.permanent, "reason", r.reason)

	if daemon == nil {
		return
	}

	if r.permanent && agentBeadID != "" {
		if err := daemon.MarkAgentFailed(ctx, agentBeadID, r.reason); err != nil {
			logger.Warn("spawn refusal: failed to mark agent bead failed",
				"bead", agentBeadID, "error", err)
		}
	}

	if r.harness {
		return
	}

	escID, err := daemon.CreateBead(ctx, buildSpawnRefusalEscalation(spec, agentBeadID, r.reason))
	if err != nil {
		logger.Warn("spawn refusal: failed to file escalation bead",
			"agent", spec.AgentName, "bead", agentBeadID, "error", err)
		return
	}
	logger.Info("spawn refusal: filed escalation",
		"escalation", escID, "agent", spec.AgentName, "bead", agentBeadID)
}
