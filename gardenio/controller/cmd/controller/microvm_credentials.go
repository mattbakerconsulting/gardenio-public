// MicroVM credential delivery (bug ka-0000007063, epic ka-0000005010).
//
// The Kubernetes pod path delivers two credential classes that the runtime-
// neutral AgentSpec / spec.Env DO NOT carry, so they never reached a
// lambda_microvm satellite:
//
//  1. The beads daemon auth token. On K8s, podmanager injects BEADS_DAEMON_TOKEN
//     + BEADS_AUTH_TOKEN (+ BEADS_REFRESH_TOKEN) as Secret env-refs
//     (internal/podmanager/manager.go). Without it the in-VM kd/gb cannot
//     authenticate writes → cannot set agent_state, claim, comment, or close
//     (C7 item 4).
//  2. The Claude OAuth credential. On K8s the account-bead path injects
//     CLAUDE_CODE_OAUTH_TOKEN from the account's `secret:<name>` credential
//     (cmd/controller/podspec.go). Without it the in-VM `claude` cannot run
//     (C7 item 3).
//
// This file sources both controller-side and folds them into the runtime-neutral
// microvm.AgentSpec so backend.go writes them ONLY into the per-run, task-scoped
// Secrets Manager document — never the run-hook payload, image layers, terraform
// state, or logs (the least-privilege invariant in internal/microvm/runtime.go).
package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/config"
	"gardenio/controller/internal/microvm"
	"gardenio/controller/internal/podmanager"
)

// resolveMicrovmBeadsToken returns the (access, refresh) beads daemon token to
// deliver into a MicroVM satellite so its in-VM kd/gb can authenticate writes.
//
// It mirrors the Kubernetes pod path's two token modes:
//   - PerAgentTokens ON: mint a fresh project-scoped fleet token for this agent
//     bead (same registration provisionAgentToken performs) and return the
//     access+refresh pair. Unlike the pod path it does NOT create a K8s Secret —
//     a MicroVM cannot mount one; the value rides the per-run Secrets Manager
//     document instead. A leaked/un-revoked token self-expires at the 24h TTL
//     safety net (defaultPATTokenTTL); teardown revokes it eagerly.
//   - PerAgentTokens OFF (prod default today): reuse the controller's own shared
//     BEADS_DAEMON_TOKEN — exactly the token K8s pods receive from the shared
//     BEADS_TOKEN_SECRET (both resolve to the same `kd-beads-token` value).
//
// With PerAgentTokens ON a mint failure returns ("", ""): falling back to the
// shared token would hand the satellite the primary credential, so the caller
// refuses the launch (P7 Task 1).
//
// A shared refresh token is not available (the controller holds only the static
// access token), so refresh is empty in that mode; the access token's own TTL
// governs. The returned values are never logged.
func resolveMicrovmBeadsToken(ctx context.Context, logger *slog.Logger, daemon *beadsapi.Client, cfg *config.Config, beadID, agentName, project string) (access, refresh string) {
	if cfg.PerAgentTokens && beadID != "" {
		// A microvm agent name can carry ':' or '@' (e.g. "bridge:acme-scif"),
		// which the daemon would read as a service identity. Refuse to mint.
		if err := validateAgentTokenIdentity(beadID, agentName); err != nil {
			logger.Error("lambda_microvm: refusing per-agent beads token", "bead", beadID, "error", err)
			return "", ""
		}
		token, err := generateToken()
		if err != nil {
			logger.Error("lambda_microvm: failed to generate per-agent beads token",
				"bead", beadID, "error", err)
			return "", ""
		}
		fleet := agentTokenBridgeName(beadID)
		subject := agentTokenSubject(agentName)
		// Ordinary fleet identity, never privileged — same call the pod path makes.
		issued, err := daemon.RegisterBridgeTokenWithRefresh(ctx, token, fleet, project, subject, defaultPATTokenTTL, defaultPATRefreshTTL, false)
		if err != nil {
			logger.Error("lambda_microvm: failed to register per-agent beads token",
				"bead", beadID, "fleet", fleet, "error", err)
			return "", ""
		}
		logger.Info("lambda_microvm: provisioned per-agent beads token",
			"bead", beadID, "fleet", fleet, "project", project)
		return token, issued.RefreshToken
	}
	// Shared-token mode: the controller's own admin token, identical to what K8s
	// pods mount from BEADS_TOKEN_SECRET when per-agent tokens are disabled.
	// With the controller on its machine principal (gd-BDirD4F7Iq) the process
	// holds no such token: this returns "" and handleNonK8sSpawn refuses the
	// launch rather than start a satellite that cannot authenticate (and never
	// hands it the machine client's credential). Enable agents.perAgentTokens.
	return strings.TrimSpace(os.Getenv("BEADS_DAEMON_TOKEN")), ""
}

// revokeMicrovmBeadsToken revokes the per-agent fleet token minted for a MicroVM
// satellite on teardown. No-op when per-agent tokens are disabled (the shared
// token is never per-agent and must not be revoked). Best-effort: the 24h TTL is
// the backstop if the revoke fails. Unlike revokeAgentToken it does not touch a
// K8s Secret — the MicroVM path never created one.
func revokeMicrovmBeadsToken(ctx context.Context, logger *slog.Logger, daemon *beadsapi.Client, cfg *config.Config, beadID string) {
	if !cfg.PerAgentTokens || beadID == "" {
		return
	}
	fleet := agentTokenBridgeName(beadID)
	if err := daemon.RevokeBridgeToken(ctx, fleet); err != nil {
		logger.Warn("lambda_microvm: failed to revoke per-agent beads token (TTL backstops)",
			"bead", beadID, "fleet", fleet, "error", err)
		return
	}
	logger.Info("lambda_microvm: revoked per-agent beads token", "bead", beadID, "fleet", fleet)
}

// microvmNatsProxyURL is the in-VM NATS endpoint: the MicroVM image's local
// Ziti proxy listener (nfd!15 proxy-listener contract — Beads 18080, Coopmux
// 19800, NATS 14222, LiteLLM 14000, Slack bridge 18090; see
// microvmClusterInternalEnvKeys in main.go). Traffic to 127.0.0.1:14222 rides the Ziti overlay to the in-cluster
// NATS server; the NATS protocol's token auth passes through the TCP tunnel
// unchanged. The pod path's cluster-DNS value (nats://gardenio-nats:4222) is
// unresolvable inside the VM, which is why the translation strips it and this
// constant re-points the same env keys at the overlay listener.
const microvmNatsProxyURL = "nats://127.0.0.1:14222"

// resolveMicrovmNatsToken returns the NATS auth token to deliver into a MicroVM
// satellite, resolved from the SAME K8s Secret the pod path projects as a token
// file (podspec.go applyCommonConfig): the subject-isolation agent-user secret
// (NATS_AGENT_USER_SECRET, the restricted publish-only token) when set,
// otherwise the legacy shared-token secret (NATS_TOKEN_SECRET). Returns ""
// when neither is configured — the deployment runs NATS without client auth
// (nats.auth.enabled=false), and pods equally mount no token in that mode, so
// an empty result IS pod parity, not a degradation. Missing/empty Secrets warn
// and return "" rather than blocking the launch (matches the Claude OAuth
// resolver's graceful degradation). The value is never logged.
func resolveMicrovmNatsToken(ctx context.Context, logger *slog.Logger, cfg *config.Config, k8sClient kubernetes.Interface, spec podmanager.AgentPodSpec) string {
	secretName := cfg.NatsAgentUserSecret
	if secretName == "" {
		secretName = cfg.NatsTokenSecret
	}
	if secretName == "" {
		return ""
	}
	secret, err := k8sClient.CoreV1().Secrets(spec.Namespace).Get(ctx, secretName, metav1.GetOptions{})
	if err != nil {
		logger.Warn("lambda_microvm: NATS token secret not readable; in-VM NATS will connect without credentials",
			"bead", spec.BeadID, "secret", secretName, "error", err)
		return ""
	}
	token := strings.TrimSpace(string(secret.Data["token"]))
	if token == "" {
		logger.Warn("lambda_microvm: NATS token secret has empty token; in-VM NATS will connect without credentials",
			"bead", spec.BeadID, "secret", secretName)
		return ""
	}
	return token
}

// applyMicrovmNats folds the in-VM NATS contract into the runtime-neutral
// AgentSpec (task ka-0000006995): the SAME contract the Kubernetes pod path
// wires in applyCommonConfig — BEADS_NATS_URL + COOP_NATS_URL +
// COOP_NATS_FORWARD=1 plus the agent NATS token — with the two VM-specific
// substitutions the substrate forces:
//
//   - URLs point at the local Ziti proxy listener (microvmNatsProxyURL) instead
//     of cluster DNS, so pub/sub rides the overlay. Stamped even when no token
//     resolved: without BEADS_NATS_URL the in-VM `gb hook-relay` loses its
//     direct-NATS fallback, and COOP_NATS_URL is what gives the outbound-only
//     registration relay (microvmOutboundOnlyEnv) a transport to ride.
//   - The token is delivered inline instead of via COOP_NATS_TOKEN_FILE: the
//     pod's tmpfs Secret mount does not exist in a VM, so the value rides the
//     per-run task-scoped secret in two consumable forms — the COOP_NATS_TOKEN
//     env var (the documented file-beats-env fallback both coop's
//     nats_token_resolved() and gb hook-relay's natsToken() read) AND the
//     file-oriented AgentSpec.NATSToken field (the nfd!15 credential-delivery
//     contract; the image writes it to the UID-1000 filesystem with restrictive
//     mode). One credential, two forms, same secret — no added exposure, and
//     backend.go keeps both out of the run-hook payload.
//
// Per-run/per-subject credentials are NOT minted here: the deployed NATS server
// supports only the static two-user token model (infra/agent subject isolation,
// helm templates/nats/statefulset.yaml), so the restricted agent-class token is
// the least privilege available today. Per-agent NATS identity is the C6 design
// (docs/research/c6-nats-subject-scoping-design.md), deferred with the per-run
// Ziti identity work (ka-0000001924).
func applyMicrovmNats(spec *microvm.AgentSpec, natsToken, natsUser string) {
	if spec.Environment == nil {
		spec.Environment = map[string]string{}
	}
	spec.Environment["BEADS_NATS_URL"] = microvmNatsProxyURL
	spec.Environment["COOP_NATS_URL"] = microvmNatsProxyURL
	spec.Environment["COOP_NATS_FORWARD"] = "1"
	if natsToken != "" {
		spec.Environment["COOP_NATS_TOKEN"] = natsToken
		spec.NATSToken = natsToken
		// Multi-user server auth (subject isolation) matches user/password
		// entries only — deliver the (non-secret) username so the token is
		// sent as that user's password (ka-0000000002).
		if natsUser != "" {
			spec.Environment["COOP_NATS_USER"] = natsUser
		}
	}
}

// microvmNatsUser returns the NATS username a MicroVM's delivered token
// authenticates as. Mirrors the pod path (podspec.go): only the restricted
// agent-user secret implies multi-user server auth, where the username is
// required for the token to match a users[] entry (ka-0000000002). Legacy
// single-token deployments (NatsTokenSecret) return "" → bare token auth.
func microvmNatsUser(cfg *config.Config) string {
	if cfg.NatsAgentUserSecret != "" {
		return "agent"
	}
	return ""
}

// resolveMicrovmSlackBridgeToken returns the shared slack-bridge bearer token to
// deliver into a MicroVM satellite so its in-VM `gb slack` calls authenticate to
// the bridge's /api/slack/* agent endpoints, resolved from the SAME K8s Secret
// the pod path injects as a SLACK_BRIDGE_TOKEN env-ref (podspec.go,
// SLACK_BRIDGE_TOKEN_SECRET, key "token"). Returns "" when the secret is not
// configured — the bridge then runs without agent-token auth and pods equally
// carry no token, so an empty result IS pod parity. Missing/empty Secrets warn
// and return "" rather than blocking the launch (matches the NATS resolver's
// graceful degradation). The value is never logged.
func resolveMicrovmSlackBridgeToken(ctx context.Context, logger *slog.Logger, cfg *config.Config, k8sClient kubernetes.Interface, spec podmanager.AgentPodSpec) string {
	if cfg.SlackBridgeTokenSecret == "" {
		return ""
	}
	secret, err := k8sClient.CoreV1().Secrets(spec.Namespace).Get(ctx, cfg.SlackBridgeTokenSecret, metav1.GetOptions{})
	if err != nil {
		logger.Warn("lambda_microvm: slack-bridge token secret not readable; in-VM gb slack will call the bridge without a bearer token",
			"bead", spec.BeadID, "secret", cfg.SlackBridgeTokenSecret, "error", err)
		return ""
	}
	token := strings.TrimSpace(string(secret.Data["token"]))
	if token == "" {
		logger.Warn("lambda_microvm: slack-bridge token secret has empty token; in-VM gb slack will call the bridge without a bearer token",
			"bead", spec.BeadID, "secret", cfg.SlackBridgeTokenSecret)
		return ""
	}
	return token
}

// applyMicrovmSlackBridge folds the slack-bridge auth token into the
// runtime-neutral AgentSpec (task ka-0000007184) so it rides the per-run
// task-scoped secret as SLACK_BRIDGE_TOKEN — the env var gb's slackHTTPClient
// reads for its Authorization bearer header.
//
// Deliberately NOT stamped here: SLACK_BRIDGE_URL. Unlike the NATS trio, the
// bridge URL is image-owned — the image entrypoint stamps
// http://127.0.0.1:18090 only when its baked ZITI_PROXY_SERVICES set carries
// the gardenio-prod-slackbridge listener (the same self-synchronizing pattern
// as ANTHROPIC_BASE_URL/litellm). Stamping the URL controller-side would break
// version skew: a pinned image without the listener would see a dead
// 127.0.0.1:18090 (connection refused) instead of the legible
// "SLACK_BRIDGE_URL is not set". The token alone is inert without a URL, so
// delivering it to older images is harmless.
func applyMicrovmSlackBridge(spec *microvm.AgentSpec, bridgeToken string) {
	if bridgeToken == "" {
		return
	}
	if spec.Environment == nil {
		spec.Environment = map[string]string{}
	}
	spec.Environment["SLACK_BRIDGE_TOKEN"] = bridgeToken
}

// resolveMicrovmCoopmuxToken returns the shared coopmux broker bearer token to
// deliver into a MicroVM satellite, resolved from the SAME K8s Secret the pod
// path injects as COOP_BROKER_TOKEN / COOP_MUX_TOKEN env-refs (podspec.go,
// COOPMUX_TOKEN_SECRET, key "token"). In-VM consumers: gb init's
// waitForCoopmuxCredentials poll (GET /api/v1/credentials/status), gb's mux
// registration client, and coop's mux_client — all reach coopmux through the
// image's local Ziti listener (127.0.0.1:19800) and fall back to
// COOP_BROKER_TOKEN for auth. Without the token every status poll 401s and the
// credential wait burns its full 90s timeout before starting coop anyway — the
// dominant term in MicroVM time-to-working-agent (ka-0000006120). Returns ""
// when the secret is not configured — coopmux then runs without bearer auth and
// pods equally carry no token, so an empty result IS pod parity. Missing/empty
// Secrets warn and return "" rather than blocking the launch (matches the
// slack-bridge resolver's graceful degradation). The value is never logged.
func resolveMicrovmCoopmuxToken(ctx context.Context, logger *slog.Logger, cfg *config.Config, k8sClient kubernetes.Interface, spec podmanager.AgentPodSpec) string {
	if cfg.CoopmuxTokenSecret == "" {
		return ""
	}
	secret, err := k8sClient.CoreV1().Secrets(spec.Namespace).Get(ctx, cfg.CoopmuxTokenSecret, metav1.GetOptions{})
	if err != nil {
		logger.Warn("lambda_microvm: coopmux token secret not readable; in-VM coopmux calls will 401 and the credential wait will burn its full timeout",
			"bead", spec.BeadID, "secret", cfg.CoopmuxTokenSecret, "error", err)
		return ""
	}
	token := strings.TrimSpace(string(secret.Data["token"]))
	if token == "" {
		logger.Warn("lambda_microvm: coopmux token secret has empty token; in-VM coopmux calls will 401 and the credential wait will burn its full timeout",
			"bead", spec.BeadID, "secret", cfg.CoopmuxTokenSecret)
		return ""
	}
	return token
}

// applyMicrovmCoopmux folds the coopmux broker bearer token into the
// runtime-neutral AgentSpec so it rides the per-run task-scoped secret under
// BOTH names the pod path injects (podspec.go): COOP_BROKER_TOKEN — the common
// fallback every in-VM consumer reads (gb init's credential wait, gb's mux
// client, coop's mux_client) — and COOP_MUX_TOKEN for exact pod parity.
//
// Deliberately NOT stamped here: COOP_BROKER_URL / COOP_MUX_URL. Like
// SLACK_BRIDGE_URL, the mux URL is image-owned — the entrypoint points it at
// the local Ziti coopmux listener (127.0.0.1:19800); the pod path's
// cluster-DNS value is stripped by microvmClusterInternalEnvKeys.
func applyMicrovmCoopmux(spec *microvm.AgentSpec, coopmuxToken string) {
	if coopmuxToken == "" {
		return
	}
	if spec.Environment == nil {
		spec.Environment = map[string]string{}
	}
	spec.Environment["COOP_BROKER_TOKEN"] = coopmuxToken
	spec.Environment["COOP_MUX_TOKEN"] = coopmuxToken
}

// resolveMicrovmClaudeOAuthToken returns the raw Claude OAuth access token for a
// MicroVM satellite, resolved from the SAME source the K8s account path bakes
// into CLAUDE_CODE_OAUTH_TOKEN: the resolved account bead's `secret:<name>`
// credential, read from the K8s Secret's `token` key (identical to
// syncAccountsToCoopmux's resolution). Returns "" (and logs a warning) when the
// account is unresolved, has no credential, uses a non-`secret:` credential
// source, or the Secret is missing/empty — the satellite then simply has no
// Claude credential rather than blocking the launch. The value is never logged.
func resolveMicrovmClaudeOAuthToken(ctx context.Context, logger *slog.Logger, cfg *config.Config, k8sClient kubernetes.Interface, spec podmanager.AgentPodSpec) string {
	account := spec.ResolvedAccount
	if account == "" {
		logger.Warn("lambda_microvm: no resolved account for agent; in-VM claude will have no OAuth credential",
			"bead", spec.BeadID, "project", spec.Project)
		return ""
	}
	acct, ok := cfg.AccountCache[account]
	if !ok || acct.Credential == "" {
		logger.Warn("lambda_microvm: resolved account has no credential; in-VM claude will have no OAuth credential",
			"bead", spec.BeadID, "account", account)
		return ""
	}
	credType, credValue, found := strings.Cut(acct.Credential, ":")
	if !found || credType != "secret" {
		// coopmux:/env: sources are not resolvable to a plaintext token here (the
		// controller never holds them). Only secret:-backed accounts can be
		// materialized into the per-run document.
		logger.Warn("lambda_microvm: account credential is not secret-backed; cannot deliver Claude OAuth to VM",
			"bead", spec.BeadID, "account", account, "credential_type", credType)
		return ""
	}
	secret, err := k8sClient.CoreV1().Secrets(spec.Namespace).Get(ctx, credValue, metav1.GetOptions{})
	if err != nil {
		logger.Warn("lambda_microvm: Claude OAuth secret not found; in-VM claude will have no OAuth credential",
			"bead", spec.BeadID, "account", account, "secret", credValue, "error", err)
		return ""
	}
	token := strings.TrimSpace(string(secret.Data["token"]))
	if token == "" {
		logger.Warn("lambda_microvm: Claude OAuth secret has empty token; in-VM claude will have no OAuth credential",
			"bead", spec.BeadID, "account", account, "secret", credValue)
		return ""
	}
	return token
}

// claudeCredentialsDocument builds the `.credentials.json` body Claude Code reads
// from a raw OAuth access token, matching coop's write_credentials_file and the
// coopmux distribute endpoint shape ({"claudeAiOauth":{"accessToken":…}}). The
// MicroVM image writes AgentSpec.ClaudeCredentials verbatim to the UID-1000
// runtime filesystem (nfd!15 credential-delivery contract), so it must be a
// VALID JSON document — never the bare token, which would corrupt
// ~/.claude/.credentials.json. expiresAt is set far in the future because the
// broker (not Claude Code) owns token refresh. Returns "" for an empty token so
// backend.go's omitempty drops the field entirely.
func claudeCredentialsDocument(oauthToken string) string {
	if oauthToken == "" {
		return ""
	}
	doc := map[string]any{
		"claudeAiOauth": map[string]any{
			"accessToken":  oauthToken,
			"refreshToken": "",
			"expiresAt":    int64(9_999_999_999_999),
			"scopes": []string{
				"user:inference",
				"user:profile",
				"user:sessions:claude_code",
				"user:mcp_servers",
			},
		},
	}
	body, err := json.Marshal(doc)
	if err != nil {
		// map[string]any of strings/int/[]string cannot fail to marshal; guard
		// only to satisfy the error return contract.
		return ""
	}
	return string(body)
}

// applyMicrovmCredentials folds the sourced beads token + Claude OAuth token into
// the runtime-neutral AgentSpec so backend.go writes them ONLY into the per-run
// task-scoped Secrets Manager document (never the run-hook payload). Pure and
// side-effect-free for unit testing; the sourcing (K8s Secret reads, token
// minting) happens in the caller.
//
// Beads token → BEADS_AUTH_TOKEN + BEADS_DAEMON_TOKEN env (the two names the kd
// CLI + gb read), plus BEADS_REFRESH_TOKEN when a refresh was minted.
//
// Claude OAuth is delivered TWO ways on the same secret for robustness:
//   - environment[CLAUDE_CODE_OAUTH_TOKEN] = raw token. Claude Code reads this
//     env var natively and the image already materializes `environment` as
//     process env, so this is the reliable path (identical value+mechanism to
//     the K8s account path).
//   - AgentSpec.ClaudeCredentials = a valid `.credentials.json` document. The
//     file-oriented nfd!15 contract path, written to the UID-1000 filesystem with
//     restrictive mode.
//
// Both ride the same per-run secret, so this is one credential in two consumable
// forms — no additional exposure surface.
func applyMicrovmCredentials(spec *microvm.AgentSpec, beadsAccessToken, beadsRefreshToken, claudeOAuthToken string) {
	if spec.Environment == nil {
		spec.Environment = map[string]string{}
	}
	if beadsAccessToken != "" {
		spec.Environment["BEADS_DAEMON_TOKEN"] = beadsAccessToken
		spec.Environment["BEADS_AUTH_TOKEN"] = beadsAccessToken
		if beadsRefreshToken != "" {
			spec.Environment["BEADS_REFRESH_TOKEN"] = beadsRefreshToken
		}
	}
	if claudeOAuthToken != "" {
		spec.Environment["CLAUDE_CODE_OAUTH_TOKEN"] = claudeOAuthToken
		spec.ClaudeCredentials = claudeCredentialsDocument(claudeOAuthToken)
	}
}
