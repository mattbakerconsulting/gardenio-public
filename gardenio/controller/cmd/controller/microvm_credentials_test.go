package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"gardenio/controller/internal/config"
	"gardenio/controller/internal/microvm"
	"gardenio/controller/internal/podmanager"
)

// TestClaudeCredentialsDocument verifies the file-oriented Claude credential is a
// VALID .credentials.json document (never the bare token — that would corrupt
// ~/.credentials.json when the image writes it verbatim), and that an empty token
// yields "" so backend.go's omitempty drops the field.
func TestClaudeCredentialsDocument(t *testing.T) {
	if got := claudeCredentialsDocument(""); got != "" {
		t.Errorf("claudeCredentialsDocument(\"\") = %q, want empty", got)
	}

	body := claudeCredentialsDocument("oauth-abc123")
	if body == "" {
		t.Fatal("claudeCredentialsDocument returned empty for a non-empty token")
	}
	// Must be valid JSON with the claudeAiOauth.accessToken shape Claude Code +
	// coopmux distribute both use.
	var doc struct {
		ClaudeAiOauth struct {
			AccessToken string `json:"accessToken"`
			ExpiresAt   int64  `json:"expiresAt"`
			Scopes      []string
		} `json:"claudeAiOauth"`
	}
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("credential document is not valid JSON: %v (body=%s)", err, body)
	}
	if doc.ClaudeAiOauth.AccessToken != "oauth-abc123" {
		t.Errorf("accessToken = %q, want oauth-abc123", doc.ClaudeAiOauth.AccessToken)
	}
	if doc.ClaudeAiOauth.ExpiresAt <= 0 {
		t.Errorf("expiresAt = %d, want a far-future positive value", doc.ClaudeAiOauth.ExpiresAt)
	}
	if len(doc.ClaudeAiOauth.Scopes) == 0 {
		t.Error("scopes empty; want the claude_code inference scopes")
	}
}

// TestApplyMicrovmCredentials verifies the beads token + Claude OAuth token are
// folded into the AgentSpec so backend.go writes them into the per-run secret:
// BEADS_AUTH_TOKEN + BEADS_DAEMON_TOKEN env (both names kd/gb read),
// BEADS_REFRESH_TOKEN only when a refresh was minted, CLAUDE_CODE_OAUTH_TOKEN env
// (native Claude Code path) AND ClaudeCredentials (file path).
func TestApplyMicrovmCredentials(t *testing.T) {
	spec := &microvm.AgentSpec{BeadID: "gd-A"}
	applyMicrovmCredentials(spec, "beads-access", "beads-refresh", "claude-oauth")

	for k, want := range map[string]string{
		"BEADS_DAEMON_TOKEN":      "beads-access",
		"BEADS_AUTH_TOKEN":        "beads-access",
		"BEADS_REFRESH_TOKEN":     "beads-refresh",
		"CLAUDE_CODE_OAUTH_TOKEN": "claude-oauth",
	} {
		if got := spec.Environment[k]; got != want {
			t.Errorf("env[%q] = %q, want %q", k, got, want)
		}
	}
	if spec.ClaudeCredentials == "" || !strings.Contains(spec.ClaudeCredentials, "claude-oauth") {
		t.Errorf("ClaudeCredentials = %q, want a credentials.json document carrying the token", spec.ClaudeCredentials)
	}
}

// TestApplyMicrovmCredentialsNoRefresh: the shared-token mode has no refresh, so
// BEADS_REFRESH_TOKEN must be omitted (not set empty).
func TestApplyMicrovmCredentialsNoRefresh(t *testing.T) {
	spec := &microvm.AgentSpec{}
	applyMicrovmCredentials(spec, "shared-token", "", "")
	if spec.Environment["BEADS_AUTH_TOKEN"] != "shared-token" {
		t.Errorf("BEADS_AUTH_TOKEN = %q, want shared-token", spec.Environment["BEADS_AUTH_TOKEN"])
	}
	if _, present := spec.Environment["BEADS_REFRESH_TOKEN"]; present {
		t.Error("BEADS_REFRESH_TOKEN present with no refresh; want omitted")
	}
	// No Claude token → no CLAUDE_CODE_OAUTH_TOKEN and no ClaudeCredentials.
	if _, present := spec.Environment["CLAUDE_CODE_OAUTH_TOKEN"]; present {
		t.Error("CLAUDE_CODE_OAUTH_TOKEN present with no token; want omitted")
	}
	if spec.ClaudeCredentials != "" {
		t.Errorf("ClaudeCredentials = %q, want empty with no token", spec.ClaudeCredentials)
	}
}

// TestApplyMicrovmCredentialsEmpty: no tokens at all leaves the credential env
// keys unset (a launch with no resolvable credentials must not stamp empty
// values that would look authoritative).
func TestApplyMicrovmCredentialsEmpty(t *testing.T) {
	spec := &microvm.AgentSpec{}
	applyMicrovmCredentials(spec, "", "", "")
	for _, k := range []string{"BEADS_AUTH_TOKEN", "BEADS_DAEMON_TOKEN", "BEADS_REFRESH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN"} {
		if _, present := spec.Environment[k]; present {
			t.Errorf("env[%q] present with no credentials; want omitted", k)
		}
	}
	if spec.ClaudeCredentials != "" {
		t.Errorf("ClaudeCredentials = %q, want empty", spec.ClaudeCredentials)
	}
}

// TestResolveMicrovmClaudeOAuthToken exercises the account→secret→token
// resolution mirror of syncAccountsToCoopmux, plus every graceful-degradation
// branch (returns "" rather than blocking the launch).
func TestResolveMicrovmClaudeOAuthToken(t *testing.T) {
	ctx := context.Background()
	logger := discardLogger()

	oauthSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "gardenio-claude-oauth-gardenio2", Namespace: "gardenio"},
		Data:       map[string][]byte{"token": []byte("oauth-live-token")},
	}

	baseCfg := func() *config.Config {
		return &config.Config{
			AccountCache: map[string]config.AccountCacheEntry{
				"gardenio2": {Provider: "anthropic", Credential: "secret:gardenio-claude-oauth-gardenio2"},
				"broker":    {Provider: "anthropic", Credential: "coopmux:some-pool"},
			},
		}
	}

	t.Run("secret-backed account resolves", func(t *testing.T) {
		k8s := fake.NewSimpleClientset(oauthSecret)
		spec := podmanager.AgentPodSpec{BeadID: "gd-A", Namespace: "gardenio", ResolvedAccount: "gardenio2"}
		if got := resolveMicrovmClaudeOAuthToken(ctx, logger, baseCfg(), k8s, spec); got != "oauth-live-token" {
			t.Errorf("token = %q, want oauth-live-token", got)
		}
	})

	t.Run("no resolved account", func(t *testing.T) {
		k8s := fake.NewSimpleClientset(oauthSecret)
		spec := podmanager.AgentPodSpec{BeadID: "gd-A", Namespace: "gardenio"}
		if got := resolveMicrovmClaudeOAuthToken(ctx, logger, baseCfg(), k8s, spec); got != "" {
			t.Errorf("token = %q, want empty for unresolved account", got)
		}
	})

	t.Run("non-secret credential source", func(t *testing.T) {
		k8s := fake.NewSimpleClientset(oauthSecret)
		spec := podmanager.AgentPodSpec{BeadID: "gd-A", Namespace: "gardenio", ResolvedAccount: "broker"}
		if got := resolveMicrovmClaudeOAuthToken(ctx, logger, baseCfg(), k8s, spec); got != "" {
			t.Errorf("token = %q, want empty for coopmux: credential (controller holds no plaintext)", got)
		}
	})

	t.Run("secret missing", func(t *testing.T) {
		k8s := fake.NewSimpleClientset() // no secrets
		spec := podmanager.AgentPodSpec{BeadID: "gd-A", Namespace: "gardenio", ResolvedAccount: "gardenio2"}
		if got := resolveMicrovmClaudeOAuthToken(ctx, logger, baseCfg(), k8s, spec); got != "" {
			t.Errorf("token = %q, want empty when the secret is absent", got)
		}
	})

	t.Run("secret empty token", func(t *testing.T) {
		empty := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "gardenio-claude-oauth-gardenio2", Namespace: "gardenio"},
			Data:       map[string][]byte{"token": []byte("  ")},
		}
		k8s := fake.NewSimpleClientset(empty)
		spec := podmanager.AgentPodSpec{BeadID: "gd-A", Namespace: "gardenio", ResolvedAccount: "gardenio2"}
		if got := resolveMicrovmClaudeOAuthToken(ctx, logger, baseCfg(), k8s, spec); got != "" {
			t.Errorf("token = %q, want empty when the secret token is blank", got)
		}
	})
}

// TestApplyMicrovmNats verifies the in-VM NATS contract (task ka-0000006995):
// the pod-path trio (BEADS_NATS_URL + COOP_NATS_URL + COOP_NATS_FORWARD=1)
// re-pointed at the local Ziti proxy listener, and the agent token delivered in
// both consumable forms — COOP_NATS_TOKEN env (coop + gb hook-relay fallback)
// and the file-oriented NATSToken field (nfd!15 contract).
func TestApplyMicrovmNats(t *testing.T) {
	spec := &microvm.AgentSpec{BeadID: "gd-N"}
	applyMicrovmNats(spec, "nats-agent-token", "agent")

	for k, want := range map[string]string{
		"BEADS_NATS_URL":    "nats://127.0.0.1:14222",
		"COOP_NATS_URL":     "nats://127.0.0.1:14222",
		"COOP_NATS_FORWARD": "1",
		"COOP_NATS_TOKEN":   "nats-agent-token",
		// Username delivered with the token: multi-user server auth matches
		// user/password entries only (ka-0000000002).
		"COOP_NATS_USER": "agent",
	} {
		if got := spec.Environment[k]; got != want {
			t.Errorf("env[%q] = %q, want %q", k, got, want)
		}
	}
	if spec.NATSToken != "nats-agent-token" {
		t.Errorf("NATSToken = %q, want the token (file-oriented nfd!15 path)", spec.NATSToken)
	}
	// The pod's tmpfs token-file path must never be stamped — it does not exist
	// in the VM and would leave coop reading a phantom file every reconnect.
	if _, present := spec.Environment["COOP_NATS_TOKEN_FILE"]; present {
		t.Error("COOP_NATS_TOKEN_FILE present; the VM token is delivered inline, never by pod file path")
	}
}

// TestApplyMicrovmNatsNoToken: with no resolvable token (nats.auth disabled —
// the mode where pods equally mount no token), the local-proxy URLs are still
// stamped (the outbound-only registration relay and gb hook-relay need the
// transport) but no empty credential keys appear.
func TestApplyMicrovmNatsNoToken(t *testing.T) {
	spec := &microvm.AgentSpec{}
	applyMicrovmNats(spec, "", "")
	if got := spec.Environment["COOP_NATS_URL"]; got != "nats://127.0.0.1:14222" {
		t.Errorf("COOP_NATS_URL = %q, want the local proxy URL even without a token", got)
	}
	if got := spec.Environment["BEADS_NATS_URL"]; got != "nats://127.0.0.1:14222" {
		t.Errorf("BEADS_NATS_URL = %q, want the local proxy URL even without a token", got)
	}
	if _, present := spec.Environment["COOP_NATS_TOKEN"]; present {
		t.Error("COOP_NATS_TOKEN present with no token; want omitted")
	}
	if spec.NATSToken != "" {
		t.Errorf("NATSToken = %q, want empty with no token", spec.NATSToken)
	}
}

// TestResolveMicrovmNatsToken exercises the Secret resolution mirror of the pod
// path's applyCommonConfig (agent-user secret preferred, legacy shared-token
// secret fallback) plus every graceful-degradation branch — a missing or empty
// secret yields "" rather than blocking the launch.
func TestResolveMicrovmNatsToken(t *testing.T) {
	ctx := context.Background()
	logger := discardLogger()

	agentCreds := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "gardenio-nats-agent-creds", Namespace: "gardenio"},
		Data:       map[string][]byte{"token": []byte("restricted-agent-token\n")},
	}
	legacyCreds := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "gardenio-nats-creds", Namespace: "gardenio"},
		Data:       map[string][]byte{"token": []byte("legacy-shared-token")},
	}
	spec := podmanager.AgentPodSpec{BeadID: "gd-N", Namespace: "gardenio"}

	t.Run("agent-user secret preferred", func(t *testing.T) {
		k8s := fake.NewSimpleClientset(agentCreds, legacyCreds)
		cfg := &config.Config{NatsAgentUserSecret: "gardenio-nats-agent-creds", NatsTokenSecret: "gardenio-nats-creds"}
		if got := resolveMicrovmNatsToken(ctx, logger, cfg, k8s, spec); got != "restricted-agent-token" {
			t.Errorf("token = %q, want the trimmed restricted agent token", got)
		}
	})

	t.Run("legacy shared-token fallback", func(t *testing.T) {
		k8s := fake.NewSimpleClientset(legacyCreds)
		cfg := &config.Config{NatsTokenSecret: "gardenio-nats-creds"}
		if got := resolveMicrovmNatsToken(ctx, logger, cfg, k8s, spec); got != "legacy-shared-token" {
			t.Errorf("token = %q, want legacy-shared-token", got)
		}
	})

	t.Run("no secret configured means no-auth deployment", func(t *testing.T) {
		k8s := fake.NewSimpleClientset()
		if got := resolveMicrovmNatsToken(ctx, logger, &config.Config{}, k8s, spec); got != "" {
			t.Errorf("token = %q, want empty when NATS auth is not configured (pod parity)", got)
		}
	})

	t.Run("secret missing", func(t *testing.T) {
		k8s := fake.NewSimpleClientset() // configured but absent
		cfg := &config.Config{NatsAgentUserSecret: "gardenio-nats-agent-creds"}
		if got := resolveMicrovmNatsToken(ctx, logger, cfg, k8s, spec); got != "" {
			t.Errorf("token = %q, want empty when the secret is absent", got)
		}
	})

	t.Run("secret empty token", func(t *testing.T) {
		empty := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "gardenio-nats-agent-creds", Namespace: "gardenio"},
			Data:       map[string][]byte{"token": []byte("  ")},
		}
		k8s := fake.NewSimpleClientset(empty)
		cfg := &config.Config{NatsAgentUserSecret: "gardenio-nats-agent-creds"}
		if got := resolveMicrovmNatsToken(ctx, logger, cfg, k8s, spec); got != "" {
			t.Errorf("token = %q, want empty when the secret token is blank", got)
		}
	})
}

// TestResolveMicrovmBeadsTokenSharedMode: with per-agent tokens disabled (prod
// default), the satellite reuses the controller's own shared BEADS_DAEMON_TOKEN —
// no daemon round-trip needed.
func TestResolveMicrovmBeadsTokenSharedMode(t *testing.T) {
	t.Setenv("BEADS_DAEMON_TOKEN", "shared-daemon-token")
	cfg := &config.Config{PerAgentTokens: false}
	access, refresh := resolveMicrovmBeadsToken(context.Background(), discardLogger(), nil, cfg, "gd-A", "crew-a", "gardenio")
	if access != "shared-daemon-token" {
		t.Errorf("access = %q, want shared-daemon-token", access)
	}
	if refresh != "" {
		t.Errorf("refresh = %q, want empty in shared-token mode", refresh)
	}
}

// TestResolveMicrovmSlackBridgeToken exercises the Secret resolution mirror of
// the pod path's SLACK_BRIDGE_TOKEN env-ref (podspec.go,
// SLACK_BRIDGE_TOKEN_SECRET, key "token") plus every graceful-degradation
// branch — an unconfigured, missing, or empty secret yields "" rather than
// blocking the launch.
func TestResolveMicrovmSlackBridgeToken(t *testing.T) {
	ctx := context.Background()
	logger := discardLogger()
	spec := podmanager.AgentPodSpec{BeadID: "gd-S", Namespace: "gardenio"}

	t.Run("configured secret resolves trimmed token", func(t *testing.T) {
		bridgeCreds := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "gardenio-slack-bridge-agent-token", Namespace: "gardenio"},
			Data:       map[string][]byte{"token": []byte("bridge-bearer-token\n")},
		}
		k8s := fake.NewSimpleClientset(bridgeCreds)
		cfg := &config.Config{SlackBridgeTokenSecret: "gardenio-slack-bridge-agent-token"}
		if got := resolveMicrovmSlackBridgeToken(ctx, logger, cfg, k8s, spec); got != "bridge-bearer-token" {
			t.Errorf("token = %q, want the trimmed bridge bearer token", got)
		}
	})

	t.Run("no secret configured means no-auth bridge", func(t *testing.T) {
		k8s := fake.NewSimpleClientset()
		if got := resolveMicrovmSlackBridgeToken(ctx, logger, &config.Config{}, k8s, spec); got != "" {
			t.Errorf("token = %q, want empty when bridge agent auth is not configured (pod parity)", got)
		}
	})

	t.Run("secret missing", func(t *testing.T) {
		k8s := fake.NewSimpleClientset() // configured but absent
		cfg := &config.Config{SlackBridgeTokenSecret: "gardenio-slack-bridge-agent-token"}
		if got := resolveMicrovmSlackBridgeToken(ctx, logger, cfg, k8s, spec); got != "" {
			t.Errorf("token = %q, want empty when the secret is absent", got)
		}
	})

	t.Run("secret empty token", func(t *testing.T) {
		empty := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "gardenio-slack-bridge-agent-token", Namespace: "gardenio"},
			Data:       map[string][]byte{"token": []byte("  ")},
		}
		k8s := fake.NewSimpleClientset(empty)
		cfg := &config.Config{SlackBridgeTokenSecret: "gardenio-slack-bridge-agent-token"}
		if got := resolveMicrovmSlackBridgeToken(ctx, logger, cfg, k8s, spec); got != "" {
			t.Errorf("token = %q, want empty when the secret token is blank", got)
		}
	})
}

// TestApplyMicrovmSlackBridge verifies the slack-bridge auth delivery
// (ka-0000007184): the bearer token rides the per-run secret as
// SLACK_BRIDGE_TOKEN, and — unlike the NATS trio — SLACK_BRIDGE_URL is NOT
// stamped controller-side. The URL is image-owned: the entrypoint stamps
// 127.0.0.1:18090 only when the image's ZITI_PROXY_SERVICES carries the
// slackbridge listener, so a controller-side URL would break version skew
// against older pinned images without the listener.
func TestApplyMicrovmSlackBridge(t *testing.T) {
	spec := &microvm.AgentSpec{BeadID: "gd-S"}
	applyMicrovmSlackBridge(spec, "bridge-bearer-token")
	if got := spec.Environment["SLACK_BRIDGE_TOKEN"]; got != "bridge-bearer-token" {
		t.Errorf("SLACK_BRIDGE_TOKEN = %q, want the bridge bearer token", got)
	}
	if _, present := spec.Environment["SLACK_BRIDGE_URL"]; present {
		t.Error("SLACK_BRIDGE_URL present; the URL is image-owned (entrypoint stamps it iff the listener exists), never controller-stamped")
	}

	// No token → no key at all (pod parity: unauthenticated bridge mode), and
	// the map is left untouched.
	empty := &microvm.AgentSpec{BeadID: "gd-S"}
	applyMicrovmSlackBridge(empty, "")
	if _, present := empty.Environment["SLACK_BRIDGE_TOKEN"]; present {
		t.Error("SLACK_BRIDGE_TOKEN present with no token; want omitted")
	}
}

// TestResolveMicrovmCoopmuxToken exercises the Secret resolution mirror of the
// pod path's COOP_BROKER_TOKEN env-ref (podspec.go, COOPMUX_TOKEN_SECRET, key
// "token") plus every graceful-degradation branch — an unconfigured, missing,
// or empty secret yields "" rather than blocking the launch (ka-0000006120).
func TestResolveMicrovmCoopmuxToken(t *testing.T) {
	ctx := context.Background()
	logger := discardLogger()
	spec := podmanager.AgentPodSpec{BeadID: "gd-M", Namespace: "gardenio"}

	t.Run("configured secret resolves trimmed token", func(t *testing.T) {
		muxCreds := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "gardenio-coopmux-token", Namespace: "gardenio"},
			Data:       map[string][]byte{"token": []byte("mux-bearer-token\n")},
		}
		k8s := fake.NewSimpleClientset(muxCreds)
		cfg := &config.Config{CoopmuxTokenSecret: "gardenio-coopmux-token"}
		if got := resolveMicrovmCoopmuxToken(ctx, logger, cfg, k8s, spec); got != "mux-bearer-token" {
			t.Errorf("token = %q, want the trimmed mux bearer token", got)
		}
	})

	t.Run("no secret configured means no-auth coopmux", func(t *testing.T) {
		k8s := fake.NewSimpleClientset()
		if got := resolveMicrovmCoopmuxToken(ctx, logger, &config.Config{}, k8s, spec); got != "" {
			t.Errorf("token = %q, want empty when coopmux auth is not configured (pod parity)", got)
		}
	})

	t.Run("secret missing", func(t *testing.T) {
		k8s := fake.NewSimpleClientset() // configured but absent
		cfg := &config.Config{CoopmuxTokenSecret: "gardenio-coopmux-token"}
		if got := resolveMicrovmCoopmuxToken(ctx, logger, cfg, k8s, spec); got != "" {
			t.Errorf("token = %q, want empty when the secret is absent", got)
		}
	})

	t.Run("secret empty token", func(t *testing.T) {
		empty := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "gardenio-coopmux-token", Namespace: "gardenio"},
			Data:       map[string][]byte{"token": []byte("  ")},
		}
		k8s := fake.NewSimpleClientset(empty)
		cfg := &config.Config{CoopmuxTokenSecret: "gardenio-coopmux-token"}
		if got := resolveMicrovmCoopmuxToken(ctx, logger, cfg, k8s, spec); got != "" {
			t.Errorf("token = %q, want empty when the secret token is blank", got)
		}
	})
}

// TestApplyMicrovmCoopmux verifies the coopmux broker auth delivery
// (ka-0000006120): the bearer token rides the per-run secret under BOTH
// pod-path names (COOP_BROKER_TOKEN — the fallback every in-VM consumer
// reads — and COOP_MUX_TOKEN), and — like SLACK_BRIDGE_URL — the mux URL is
// NOT stamped controller-side (image-owned local Ziti listener).
func TestApplyMicrovmCoopmux(t *testing.T) {
	spec := &microvm.AgentSpec{BeadID: "gd-M"}
	applyMicrovmCoopmux(spec, "mux-bearer-token")
	if got := spec.Environment["COOP_BROKER_TOKEN"]; got != "mux-bearer-token" {
		t.Errorf("COOP_BROKER_TOKEN = %q, want the mux bearer token", got)
	}
	if got := spec.Environment["COOP_MUX_TOKEN"]; got != "mux-bearer-token" {
		t.Errorf("COOP_MUX_TOKEN = %q, want the mux bearer token", got)
	}
	for _, urlKey := range []string{"COOP_BROKER_URL", "COOP_MUX_URL"} {
		if _, present := spec.Environment[urlKey]; present {
			t.Errorf("%s present; the mux URL is image-owned (local Ziti listener), never controller-stamped", urlKey)
		}
	}

	// No token → no keys at all (pod parity: unauthenticated coopmux mode).
	empty := &microvm.AgentSpec{BeadID: "gd-M"}
	applyMicrovmCoopmux(empty, "")
	if _, present := empty.Environment["COOP_BROKER_TOKEN"]; present {
		t.Error("COOP_BROKER_TOKEN present with no token; want omitted")
	}
	if _, present := empty.Environment["COOP_MUX_TOKEN"]; present {
		t.Error("COOP_MUX_TOKEN present with no token; want omitted")
	}
}
