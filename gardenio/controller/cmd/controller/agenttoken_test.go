package main

import (
	"context"
	"log/slog"
	"testing"

	"k8s.io/client-go/kubernetes/fake"

	"gardenio/controller/internal/config"
	"gardenio/controller/internal/podmanager"
	"gardenio/controller/internal/subscriber"
)

// createCountingManager records CreateAgentPod calls so a refused spawn can be
// asserted to have created no pod.
type createCountingManager struct {
	mockPodManager
	created int
}

func (m *createCountingManager) CreateAgentPod(_ context.Context, _ podmanager.AgentPodSpec) error {
	m.created++
	return nil
}

func TestAgentTokenFleetName(t *testing.T) {
	got := agentTokenBridgeName("gd-abc123")
	want := "agent-gd-abc123"
	if got != want {
		t.Errorf("agentTokenBridgeName() = %q, want %q", got, want)
	}
}

func TestAgentTokenSecretName(t *testing.T) {
	got := agentTokenSecretName("gd-abc123")
	want := "agent-token-gd-abc123"
	if got != want {
		t.Errorf("agentTokenSecretName() = %q, want %q", got, want)
	}
}

func TestGenerateToken(t *testing.T) {
	token, err := generateToken()
	if err != nil {
		t.Fatalf("generateToken() error: %v", err)
	}
	// 32 bytes → 64 hex chars.
	if len(token) != 64 {
		t.Errorf("generateToken() length = %d, want 64", len(token))
	}
	// Ensure uniqueness (probabilistic but practically guaranteed).
	token2, err := generateToken()
	if err != nil {
		t.Fatalf("generateToken() second call error: %v", err)
	}
	if token == token2 {
		t.Error("generateToken() returned duplicate tokens")
	}
}

func TestAgentTokenSubject_IsAgentName(t *testing.T) {
	if got := agentTokenSubject("gardenio-crew-x1"); got != "gardenio-crew-x1" {
		t.Fatalf("agentTokenSubject = %q, want the agent name", got)
	}
	if got := agentTokenSubject(""); got != "" {
		t.Fatalf("empty agent name must stay empty, got %q", got)
	}
}

// TestProvisionAgentToken_FailureRefusesSpawn: with PerAgentTokens on and the
// daemon refusing to register the token, the spawn handler returns an error and
// creates no pod — it must not fall back to the shared token.
func TestProvisionAgentToken_FailureRefusesSpawn(t *testing.T) {
	srv := mockFleetTokenServer(t, true)
	defer srv.Close()

	mgr := &createCountingManager{}
	// The project cache knows gardenio as an operator land (as refreshProjectCache
	// does at startup), so the Lands spawn wall makes no daemon read.
	cfg := &config.Config{Namespace: "gardenio", PerAgentTokens: true,
		ProjectCache: map[string]config.ProjectCacheEntry{"gardenio": {}}}
	event := subscriber.Event{
		Type:      subscriber.AgentSpawn,
		Project:   "gardenio",
		Role:      "crew",
		AgentName: "gardenio-crew-x1",
		BeadID:    "gd-AGENT1",
	}
	// The failing daemon doubles as machineDaemon: #326 routes the per-agent
	// token mint through the controller's own principal.
	failing := newClient(t, srv)
	err := handleEvent(
		context.Background(), slog.Default(), cfg, event, mgr,
		nil, fake.NewSimpleClientset(), failing, failing, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
	)
	if err == nil {
		t.Fatal("handleEvent returned nil, want a per-agent token provisioning error")
	}
	if mgr.created != 0 {
		t.Fatalf("created %d pods, want 0 (spawn must be refused)", mgr.created)
	}
}

// TestResolveMicrovmBeadsToken_PerAgentFailureFailsClosed: a mint failure under
// PerAgentTokens must not hand the satellite the shared BEADS_DAEMON_TOKEN.
func TestResolveMicrovmBeadsToken_PerAgentFailureFailsClosed(t *testing.T) {
	t.Setenv("BEADS_DAEMON_TOKEN", "shared-daemon-token")
	srv := mockFleetTokenServer(t, true)
	defer srv.Close()

	cfg := &config.Config{PerAgentTokens: true}
	access, refresh := resolveMicrovmBeadsToken(context.Background(), discardLogger(), newClient(t, srv), cfg, "gd-A", "crew-a", "gardenio")
	if access != "" || refresh != "" {
		t.Fatalf("got (%q,%q), want empty pair so the caller refuses the launch", access, refresh)
	}
}
