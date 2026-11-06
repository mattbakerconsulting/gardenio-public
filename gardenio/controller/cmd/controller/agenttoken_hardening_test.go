package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"gardenio/controller/internal/config"
	"gardenio/controller/internal/microvm"
	"gardenio/controller/internal/podmanager"
	"gardenio/controller/internal/subscriber"
)

// Name validation at mint (P7 review item 1): a name the daemon could read as a
// service identity must never be minted a token.
func TestValidateAgentTokenIdentity(t *testing.T) {
	for _, tc := range []struct {
		name, bead, agent string
		ok                bool
	}{
		{"ordinary", "gd-abc123", "gardenio-crew-x1", true},
		{"mixed-case bead id", "gd-F149B6RdbT", "gardenio-roll-out-peragent-zrx", true},
		{"empty agent name falls back to fleet name", "gd-abc123", "", true},
		{"scif portal impersonation", "gd-abc123", "bridge:acme-scif", false},
		{"email-shaped", "gd-abc123", "bob@example.com", false},
		{"uppercase", "gd-abc123", "Crew-X", false},
		{"underscore", "gd-abc123", "crew_x", false},
		{"space", "gd-abc123", "crew x", false},
		{"leading dash", "gd-abc123", "-crew", false},
		{"too long", "gd-abc123", strings.Repeat("a", 129), false},
		{"empty bead id", "", "crew-x", false},
		{"colon in bead id", "gd:abc", "crew-x", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateAgentTokenIdentity(tc.bead, tc.agent)
			if (err == nil) != tc.ok {
				t.Fatalf("validateAgentTokenIdentity(%q, %q) err = %v, want ok=%v", tc.bead, tc.agent, err, tc.ok)
			}
		})
	}
}

// A refused name registers nothing with the daemon and creates no Secret.
func TestProvisionAgentToken_RefusesBadNameBeforeMinting(t *testing.T) {
	var mu sync.Mutex
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()
	k8s := fake.NewSimpleClientset()

	_, _, err := provisionAgentToken(context.Background(), discardLogger(), newClient(t, srv), k8s,
		"gd-abc123", "bridge:acme-scif", "gardenio", "ns", nil)
	if err == nil {
		t.Fatal("provisionAgentToken accepted a service-shaped agent name")
	}
	mu.Lock()
	defer mu.Unlock()
	if hits != 0 {
		t.Errorf("daemon was called %d times for a refused name, want 0", hits)
	}
	if list, _ := k8s.CoreV1().Secrets("ns").List(context.Background(), metav1.ListOptions{}); len(list.Items) != 0 {
		t.Errorf("a Secret was created for a refused name: %d", len(list.Items))
	}
}

// Bead ids are mixed-case; a Secret name must be lowercase (DNS-1123).
func TestAgentTokenSecretName_LowercasesBeadID(t *testing.T) {
	if got := agentTokenSecretName("gd-F149B6RdbT"); got != "agent-token-gd-f149b6rdbt" {
		t.Fatalf("agentTokenSecretName = %q, want a lowercase DNS-1123 name", got)
	}
}

func TestProvisionAgentToken_MixedCaseBeadIDKeepsExactLabel(t *testing.T) {
	srv := mockFleetTokenServer(t, false)
	defer srv.Close()
	k8s := fake.NewSimpleClientset()
	name, _, err := provisionAgentToken(context.Background(), discardLogger(), newClient(t, srv), k8s,
		"gd-F149B6RdbT", "crew-x", "gardenio", "ns", nil)
	if err != nil {
		t.Fatal(err)
	}
	sec, err := k8s.CoreV1().Secrets("ns").Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if name != strings.ToLower(name) {
		t.Errorf("secret name %q is not lowercase", name)
	}
	if sec.Labels["gardenio.io/bead"] != "gd-F149B6RdbT" {
		t.Errorf("bead label = %q, want the exact id (the janitor maps back through it)", sec.Labels["gardenio.io/bead"])
	}
}

type recordingRevoker struct{ fleets []string }

func (r *recordingRevoker) RevokeBridgeToken(_ context.Context, fleet string) error {
	r.fleets = append(r.fleets, fleet)
	return nil
}

// Revoke on post-mint failure (review item c): when this spawn minted the
// Secret and the pod is then not created, the token and Secret are revoked.
func TestReleaseMintedAgentToken(t *testing.T) {
	srv := mockFleetTokenServer(t, false)
	defer srv.Close()
	k8s := fake.NewSimpleClientset()
	name, minted, err := provisionAgentToken(context.Background(), discardLogger(), newClient(t, srv), k8s,
		"gd-rel1", "crew-rel", "gardenio", "ns", nil)
	if err != nil || !minted {
		t.Fatalf("provision: minted=%v err=%v", minted, err)
	}

	rev := &recordingRevoker{}
	// Not minted by this spawn: leave a possibly-live agent's token alone.
	releaseMintedAgentToken(context.Background(), discardLogger(), rev, k8s, "gd-rel1", "ns", nil, false)
	if len(rev.fleets) != 0 {
		t.Fatalf("revoked %v for a token this spawn did not mint", rev.fleets)
	}
	if _, err := k8s.CoreV1().Secrets("ns").Get(context.Background(), name, metav1.GetOptions{}); err != nil {
		t.Fatalf("Secret must survive: %v", err)
	}

	releaseMintedAgentToken(context.Background(), discardLogger(), rev, k8s, "gd-rel1", "ns", nil, true)
	if len(rev.fleets) != 1 || rev.fleets[0] != "agent-gd-rel1" {
		t.Fatalf("revoked fleets = %v, want [agent-gd-rel1]", rev.fleets)
	}
	if _, err := k8s.CoreV1().Secrets("ns").Get(context.Background(), name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("Secret must be deleted, got err=%v", err)
	}
}

type failingCreateManager struct {
	mockPodManager
	created int
}

func (m *failingCreateManager) CreateAgentPod(context.Context, podmanager.AgentPodSpec) error {
	m.created++
	return errors.New("api server unavailable")
}

// End to end through the spawn door: CreateAgentPod fails after the mint, so the
// freshly minted Secret must not be left behind.
func TestHandleEvent_SpawnCreateFailureRevokesMintedToken(t *testing.T) {
	srv := mockFleetTokenServer(t, false)
	defer srv.Close()
	k8s := fake.NewSimpleClientset()
	mgr := &failingCreateManager{}
	// The project cache knows gardenio as an operator land (as refreshProjectCache
	// does at startup), so the Lands spawn wall makes no daemon read.
	cfg := &config.Config{Namespace: "gardenio", PerAgentTokens: true,
		ProjectCache: map[string]config.ProjectCacheEntry{"gardenio": {}}}
	event := subscriber.Event{
		Type: subscriber.AgentSpawn, Project: "gardenio", Role: "crew",
		AgentName: "gardenio-crew-x1", BeadID: "gd-Spawn1",
	}
	c := newClient(t, srv)
	err := handleEvent(context.Background(), discardLogger(), cfg, event, mgr,
		nil, k8s, c, c, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	if err == nil {
		t.Fatal("handleEvent returned nil, want the CreateAgentPod error")
	}
	if mgr.created != 1 {
		t.Fatalf("CreateAgentPod calls = %d, want 1", mgr.created)
	}
	if _, err := k8s.CoreV1().Secrets("gardenio").Get(context.Background(), agentTokenSecretName("gd-Spawn1"), metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("minted Secret must be revoked after a failed pod create, got err=%v", err)
	}
}

// Stuck-restart refusal (review item 3): the recreate path is fail-closed. When
// the fresh mint fails, no pod is created (never the shared token).
func TestHandleEvent_StuckRestartRefusesWhenMintFails(t *testing.T) {
	srv := mockFleetTokenServer(t, true)
	defer srv.Close()
	mgr := &createCountingManager{}
	// The recreate passes the Lands spawn wall first (gd-Ld5r7Ok2rK); a cached
	// operator land makes no daemon read there.
	cfg := &config.Config{Namespace: "gardenio", PerAgentTokens: true,
		ProjectCache: map[string]config.ProjectCacheEntry{"gardenio": {}}}
	event := subscriber.Event{
		Type: subscriber.AgentStuck, Project: "gardenio", Role: "crew",
		AgentName: "gardenio-crew-x1", BeadID: "gd-Stuck1",
		Metadata: map[string]string{"reason": "no coop seq advance", "source": "stuck-heuristic"},
	}
	c := newClient(t, srv)
	err := handleEvent(context.Background(), discardLogger(), cfg, event, mgr,
		nil, fake.NewSimpleClientset(), c, c, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "per-agent token provisioning failed") {
		t.Fatalf("handleEvent err = %v, want a per-agent token provisioning error", err)
	}
	if mgr.created != 0 {
		t.Fatalf("created %d pods, want 0 (stuck-restart must not fall back to the shared token)", mgr.created)
	}
}

// Microvm refusal (review item 3): with the flag on, a failed mint (here a
// service-shaped agent name) refuses the launch instead of delivering the
// shared token into the satellite.
func TestHandleNonK8sSpawn_MicrovmRefusesWhenPerAgentTokenNotMinted(t *testing.T) {
	t.Setenv("BEADS_DAEMON_TOKEN", "shared-daemon-token")
	srv := mockFleetTokenServer(t, false)
	defer srv.Close()
	c := newClient(t, srv)
	cfg := &config.Config{Namespace: "gardenio", PerAgentTokens: true}
	lifecycle := microvm.NewLifecycle(&microvm.Backend{}, nil, discardLogger())
	spec := podmanager.AgentPodSpec{
		AgentName: "bridge:acme-scif", Project: "gardenio", Role: "crew",
		ExecutionTarget: podmanager.ExecutionTargetLambdaMicroVM,
	}
	err := handleNonK8sSpawn(context.Background(), discardLogger(), cfg, c, c, fake.NewSimpleClientset(),
		lifecycle, nil, "gd-Mvm1", spec, nil)
	if err == nil || !strings.Contains(err.Error(), "per-agent token provisioning failed") {
		t.Fatalf("handleNonK8sSpawn err = %v, want a per-agent token provisioning refusal", err)
	}
}

func TestResolveMicrovmBeadsToken_RefusesServiceShapedName(t *testing.T) {
	t.Setenv("BEADS_DAEMON_TOKEN", "shared-daemon-token")
	srv := mockFleetTokenServer(t, false)
	defer srv.Close()
	cfg := &config.Config{PerAgentTokens: true}
	access, refresh := resolveMicrovmBeadsToken(context.Background(), discardLogger(), newClient(t, srv), cfg, "gd-A", "bridge:acme-scif", "gardenio")
	if access != "" || refresh != "" {
		t.Fatalf("got (%q,%q), want empty pair for a service-shaped name", access, refresh)
	}
}

// The reconciler hook mints through the same provisioner as the event door.
func TestReconcilerAgentTokenApplier_MintsAndReleases(t *testing.T) {
	srv := mockFleetTokenServer(t, false)
	defer srv.Close()
	k8s := fake.NewSimpleClientset()
	c := newClient(t, srv)
	rev := &recordingRevoker{}
	app := reconcilerAgentTokenApplier(discardLogger(), c, rev, k8s, nil)

	spec := podmanager.AgentPodSpec{Project: "gardenio", Namespace: "ns", DaemonTokenSecret: "kd-beads-token"}
	minted, err := app.Apply(context.Background(), &spec, "gd-Rec1", "crew-rec")
	if err != nil || !minted {
		t.Fatalf("Apply minted=%v err=%v", minted, err)
	}
	if spec.DaemonTokenSecret != agentTokenSecretName("gd-Rec1") {
		t.Fatalf("DaemonTokenSecret = %q, want the per-agent Secret", spec.DaemonTokenSecret)
	}
	app.Release(context.Background(), spec, "gd-Rec1")
	if len(rev.fleets) != 1 || rev.fleets[0] != "agent-gd-Rec1" {
		t.Fatalf("revoked = %v", rev.fleets)
	}

	// Refused name: error, spec untouched.
	spec2 := podmanager.AgentPodSpec{Project: "gardenio", Namespace: "ns", DaemonTokenSecret: "kd-beads-token"}
	if _, err := app.Apply(context.Background(), &spec2, "gd-Rec2", "bridge:acme-scif"); err == nil {
		t.Fatal("Apply accepted a service-shaped name")
	}
	if spec2.DaemonTokenSecret != "kd-beads-token" {
		t.Fatalf("a refused Apply must not touch the spec, got %q", spec2.DaemonTokenSecret)
	}
}

// gd-BDirD4F7Iq: on the controller's machine principal the process holds no
// BEADS_DAEMON_TOKEN. Shared-token mode then has nothing to deliver and the
// MicroVM launch is refused instead of starting a satellite that cannot
// authenticate.
func TestHandleNonK8sSpawn_MicrovmRefusesSharedModeWithoutStaticToken(t *testing.T) {
	t.Setenv("BEADS_DAEMON_TOKEN", "")
	srv := mockFleetTokenServer(t, false)
	defer srv.Close()
	c := newClient(t, srv)
	cfg := &config.Config{Namespace: "gardenio", PerAgentTokens: false}
	lifecycle := microvm.NewLifecycle(&microvm.Backend{}, nil, discardLogger())
	spec := podmanager.AgentPodSpec{
		AgentName: "crew-a", Project: "gardenio", Role: "crew",
		ExecutionTarget: podmanager.ExecutionTargetLambdaMicroVM,
	}
	err := handleNonK8sSpawn(context.Background(), discardLogger(), cfg, c, c, fake.NewSimpleClientset(),
		lifecycle, nil, "gd-Mvm2", spec, nil)
	if err == nil || !strings.Contains(err.Error(), "enable agents.perAgentTokens") {
		t.Fatalf("handleNonK8sSpawn err = %v, want a no-token refusal", err)
	}
}
