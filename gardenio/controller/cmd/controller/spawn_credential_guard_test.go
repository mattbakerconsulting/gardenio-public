package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"gardenio/controller/internal/podmanager"
)

var errSyntheticAPI = errors.New("synthetic API error")

func guardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// oauthSpec returns a credentialed crew spec referencing the given OAuth secret.
func oauthSpec(project, namespace, secret string) podmanager.AgentPodSpec {
	return podmanager.AgentPodSpec{
		Project:   project,
		Mode:      "crew",
		Role:      "crew",
		AgentName: "test-agent",
		Namespace: namespace,
		SecretEnv: []podmanager.SecretEnvSource{
			{EnvName: claudeOAuthEnvName, SecretName: secret, SecretKey: "token"},
		},
	}
}

// codexSpec returns a credentialed spec for a provider=openai account: no
// CLAUDE_CODE_OAUTH_TOKEN, just the derived -session Secret to mount.
func codexSpec(project, namespace, session string) podmanager.AgentPodSpec {
	return podmanager.AgentPodSpec{
		Project:         project,
		Mode:            "crew",
		Role:            "crew",
		AgentName:       "codex-agent",
		Namespace:       namespace,
		CodexAuthSecret: session,
	}
}

func TestCredentialSecretRef_Codex(t *testing.T) {
	name, kind, ok := credentialSecretRef(codexSpec("gardenio", "gardenio", "gardenio-codex-auth-session"))
	if !ok || name != "gardenio-codex-auth-session" || kind != credentialKindCodexLogin {
		t.Errorf("credentialSecretRef = (%q, %q, %v)", name, kind, ok)
	}
}

// TestEvaluateSpawnCredentials: the fail-closed spawn guard refuses an empty
// project or a missing credential secret, so no loginless pod spawns
// (ka-0000007608).
func TestEvaluateSpawnCredentials(t *testing.T) {
	const ns = "gardenio"
	const secret = "gardenio-claude-oauth-matt"

	tests := []struct {
		name          string
		spec          podmanager.AgentPodSpec
		seedSecret    bool // create the OAuth secret in the fake namespace
		wantRefuse    bool
		wantPermanent bool
	}{
		{
			name:          "refuse: empty project",
			spec:          oauthSpec("", ns, secret),
			wantRefuse:    true,
			wantPermanent: true, // permanent misconfiguration
		},
		{
			name:       "refuse: oauth secret referenced but missing",
			spec:       oauthSpec("gardenio", ns, secret),
			seedSecret: false,
			wantRefuse: true,
			// transient: the ExternalSecret may still be syncing.
			wantPermanent: false,
		},
		{
			name:       "allow: oauth secret referenced and present",
			spec:       oauthSpec("gardenio", ns, secret),
			seedSecret: true,
			wantRefuse: false,
		},
		{
			name:       "refuse: codex -session secret referenced but missing",
			spec:       codexSpec("gardenio", ns, "gardenio-codex-auth-session"),
			wantRefuse: true,
			// transient: the keeper may not have minted the -session secret yet.
			wantPermanent: false,
		},
		{
			name:       "allow: codex -session secret present",
			spec:       codexSpec("gardenio", ns, "gardenio-codex-auth-session"),
			seedSecret: true,
			wantRefuse: false,
		},
		{
			name: "allow: direct-provider auth (no oauth SecretEnv)",
			spec: podmanager.AgentPodSpec{
				Project:   "gardenio",
				Mode:      "crew",
				Role:      "crew",
				AgentName: "bedrock-agent",
				Namespace: ns,
			},
			wantRefuse: false,
		},
		{
			name: "refuse: empty project even with no oauth ref",
			spec: podmanager.AgentPodSpec{
				Project:   "",
				Mode:      "crew",
				Role:      "crew",
				AgentName: "orphan",
				Namespace: ns,
			},
			wantRefuse:    true,
			wantPermanent: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := fake.NewClientset()
			if tt.seedSecret {
				if name, _, ok := credentialSecretRef(tt.spec); ok {
					_, err := client.CoreV1().Secrets(tt.spec.Namespace).Create(
						context.Background(),
						&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: tt.spec.Namespace}},
						metav1.CreateOptions{},
					)
					if err != nil {
						t.Fatalf("seeding secret: %v", err)
					}
				}
			}

			got := evaluateSpawnCredentials(context.Background(), client, guardLogger(), tt.spec)
			if got.refuse != tt.wantRefuse {
				t.Fatalf("refuse = %v, want %v (reason=%q)", got.refuse, tt.wantRefuse, got.reason)
			}
			if tt.wantRefuse {
				if got.reason == "" {
					t.Errorf("expected a non-empty refusal reason")
				}
				if got.permanent != tt.wantPermanent {
					t.Errorf("permanent = %v, want %v", got.permanent, tt.wantPermanent)
				}
			}
		})
	}
}

// TestEvaluateSpawnCredentials_APIErrorAllows verifies a non-NotFound API error
// while probing the secret does NOT fail-closed (a transient API blip must not
// wedge the fleet; verifySecretsExist still backstops at create time).
func TestEvaluateSpawnCredentials_APIErrorAllows(t *testing.T) {
	client := fake.NewClientset()
	client.PrependReactor("get", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errSyntheticAPI
	})
	got := evaluateSpawnCredentials(context.Background(), client, guardLogger(),
		oauthSpec("gardenio", "gardenio", "gardenio-claude-oauth-matt"))
	if got.refuse {
		t.Fatalf("expected allow on transient API error, got refuse (reason=%q)", got.reason)
	}
}

func TestOAuthSecretRef(t *testing.T) {
	// Present.
	name, _, ok := credentialSecretRef(oauthSpec("gardenio", "gardenio", "gardenio-claude-oauth-matt"))
	if !ok || name != "gardenio-claude-oauth-matt" {
		t.Errorf("credentialSecretRef = (%q, %v), want (gardenio-claude-oauth-matt, true)", name, ok)
	}
	// Absent.
	_, _, ok = credentialSecretRef(podmanager.AgentPodSpec{Project: "gardenio"})
	if ok {
		t.Errorf("credentialSecretRef on spec with no OAuth SecretEnv = true, want false")
	}
}

func TestBuildSpawnRefusalEscalation(t *testing.T) {
	spec := oauthSpec("gardenio", "gardenio", "gardenio-claude-oauth-matt")
	req := buildSpawnRefusalEscalation(spec, "crew-gardenio-crew-test-agent", "missing secret")

	if req.Type != "escalation" {
		t.Errorf("Type = %q, want escalation", req.Type)
	}
	if req.IdempotencyKey != "loginless-spawn:crew-gardenio-crew-test-agent" {
		t.Errorf("IdempotencyKey = %q, want loginless-spawn:crew-gardenio-crew-test-agent", req.IdempotencyKey)
	}
	if !hasLabel(req.Labels, "project:gardenio") || !hasLabel(req.Labels, "guard:loginless-spawn") {
		t.Errorf("labels = %v, want project:gardenio + guard:loginless-spawn", req.Labels)
	}

	var fields map[string]any
	if err := json.Unmarshal(req.Fields, &fields); err != nil {
		t.Fatalf("unmarshalling fields: %v", err)
	}
	if fields["operation_type"] != "credential_provisioning" {
		t.Errorf("operation_type = %v, want credential_provisioning", fields["operation_type"])
	}
	spc, ok := fields["operation_spec"].(map[string]any)
	if !ok {
		t.Fatalf("operation_spec not an object: %T", fields["operation_spec"])
	}
	if spc["oauth_secret"] != "gardenio-claude-oauth-matt" {
		t.Errorf("operation_spec.oauth_secret = %v, want gardenio-claude-oauth-matt", spc["oauth_secret"])
	}
	if fields["requesting_agent"] != "crew-gardenio-crew-test-agent" {
		t.Errorf("requesting_agent = %v, want crew-gardenio-crew-test-agent", fields["requesting_agent"])
	}
}

// buildSpawnRefusalEscalation must omit the project label when the project is
// empty (the empty-project refusal case), so it never emits a "project:" label
// with an empty value.
func TestBuildSpawnRefusalEscalation_EmptyProject(t *testing.T) {
	req := buildSpawnRefusalEscalation(
		podmanager.AgentPodSpec{Mode: "crew", Role: "crew", AgentName: "orphan", Namespace: "gardenio"},
		"crew--crew-orphan", "no project")
	if hasLabel(req.Labels, "project:") {
		t.Errorf("labels = %v, must not contain an empty project label", req.Labels)
	}
	if !hasLabel(req.Labels, "guard:loginless-spawn") {
		t.Errorf("labels = %v, want guard:loginless-spawn", req.Labels)
	}
}

func hasLabel(labels []string, want string) bool {
	return slices.Contains(labels, want)
}
