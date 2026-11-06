package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/bridgetokenrotator"
	"gardenio/controller/internal/config"
)

// Machine principal, PR 4b (gd-s7aPIf0Bgp, design D3): the controller's token
// minting — the bridge fleet-token rotator and per-agent token provisioning on
// the pod and MicroVM paths — runs on machineDaemon, the client authenticated
// as the Keycloak service account of client gardenio-controller (role
// gardenio-machine-token-mint). Every other call stays on daemon, the static
// token. These tests stand up two recording daemons that capture the bearer on
// each request and prove the /v1/fleet-tokens traffic carries the machine
// token while an unrelated write carries the static one.
//
// main.go wires machineDaemon into bridgetokenrotator.New, provisionAgentToken
// and resolveMicrovmBeadsToken; the functions' bodies are untouched (the
// per-agent-token paths are PR #273's, which 4b must not change).

const (
	tokenMintStaticBearer  = "Bearer static-token"
	tokenMintMachineBearer = "Bearer machine-token"
)

// bearerLog records "METHOD /path" plus the Authorization header for every
// request a stub daemon receives.
type bearerLog struct {
	mu    sync.Mutex
	calls []bearerCall
}

type bearerCall struct {
	route  string // "METHOD /path"
	bearer string // Authorization header verbatim
}

func (l *bearerLog) all() []bearerCall {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]bearerCall(nil), l.calls...)
}

// matching returns the calls whose route starts with prefix.
func (l *bearerLog) matching(prefix string) []bearerCall {
	var out []bearerCall
	for _, c := range l.all() {
		if strings.HasPrefix(c.route, prefix) {
			out = append(out, c)
		}
	}
	return out
}

// tokenMintStub answers the fleet-token endpoints the controller's minting
// paths use and {} for anything else, recording every request's bearer.
func tokenMintStub(l *bearerLog, fleets []map[string]any) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		l.mu.Lock()
		l.calls = append(l.calls, bearerCall{route: r.Method + " " + r.URL.Path, bearer: r.Header.Get("Authorization")})
		l.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/fleet-tokens":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"refresh_token":            "rt-fixture",
				"refresh_token_expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano),
			})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/fleet-tokens":
			_ = json.NewEncoder(w).Encode(map[string]any{"fleets": fleets})
		default:
			_, _ = w.Write([]byte("{}"))
		}
	})
}

// twoBearerDaemons returns (daemon, its log, machineDaemon, its log): a
// static-token client and a machine-token client, each against its own
// recording stub.
func twoBearerDaemons(t *testing.T, fleets []map[string]any) (*beadsapi.Client, *bearerLog, *beadsapi.Client, *bearerLog) {
	t.Helper()
	dl, ml := &bearerLog{}, &bearerLog{}
	dsrv := httptest.NewServer(tokenMintStub(dl, fleets))
	t.Cleanup(dsrv.Close)
	msrv := httptest.NewServer(tokenMintStub(ml, fleets))
	t.Cleanup(msrv.Close)
	dc, err := beadsapi.New(beadsapi.Config{HTTPAddr: dsrv.URL, Token: "static-token"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dc.Close() })
	mc, err := beadsapi.New(beadsapi.Config{HTTPAddr: msrv.URL, Token: "machine-token"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mc.Close() })
	return dc, dl, mc, ml
}

// assertTokenMintSplit is the shared verdict: every /v1/fleet-tokens call
// landed on the machine daemon with the machine bearer, none on the static
// daemon; the unrelated write landed on the static daemon with the static
// bearer and never reached the machine daemon.
func assertTokenMintSplit(t *testing.T, dl, ml *bearerLog, wantMintCalls int) {
	t.Helper()
	mint := ml.matching("POST /v1/fleet-tokens")
	if len(mint) != wantMintCalls {
		t.Fatalf("machine daemon received %d POST /v1/fleet-tokens, want %d: %+v", len(mint), wantMintCalls, ml.all())
	}
	for _, c := range ml.all() {
		if !strings.HasPrefix(c.route, "GET /v1/fleet-tokens") && !strings.HasPrefix(c.route, "POST /v1/fleet-tokens") {
			t.Errorf("machine daemon received a non-token-mint call %q: %+v", c.route, ml.all())
		}
		if c.bearer != tokenMintMachineBearer {
			t.Errorf("machine daemon call %q carried %q, want the machine token", c.route, c.bearer)
		}
	}
	if got := dl.matching("POST /v1/fleet-tokens"); len(got) != 0 {
		t.Errorf("static daemon received token-mint calls: %+v", got)
	}
	if got := dl.matching("GET /v1/fleet-tokens"); len(got) != 0 {
		t.Errorf("static daemon received token-list calls: %+v", got)
	}
	other := dl.matching("PATCH /v1/beads/")
	if len(other) == 0 {
		t.Fatalf("static daemon received no unrelated write: %+v", dl.all())
	}
	for _, c := range other {
		if c.bearer != tokenMintStaticBearer {
			t.Errorf("static daemon call %q carried %q, want the static token", c.route, c.bearer)
		}
	}
}

// Pod path: provisionAgentToken registers the per-agent fleet token through
// machineDaemon; a bead write on daemon keeps the static token.
func TestMachinePrincipal_ProvisionAgentTokenMintsViaMachineDaemon(t *testing.T) {
	dc, dl, mc, ml := twoBearerDaemons(t, nil)
	k8s := fake.NewSimpleClientset()

	secretName, _, err := provisionAgentToken(context.Background(), discardLogger(), mc, k8s, "gd-agent1", "crew-agent1", "gardenio", "ns", nil)
	if err != nil {
		t.Fatalf("provisionAgentToken: %v", err)
	}
	if secretName != agentTokenSecretName("gd-agent1") {
		t.Fatalf("secret name = %q", secretName)
	}
	if err := dc.UpdateBeadFields(context.Background(), "gd-agent1", map[string]string{"agent_state": "spawning"}); err != nil {
		t.Fatalf("UpdateBeadFields: %v", err)
	}
	assertTokenMintSplit(t, dl, ml, 1)
}

// MicroVM path: resolveMicrovmBeadsToken registers through machineDaemon in
// per-agent-token mode.
func TestMachinePrincipal_ResolveMicrovmBeadsTokenMintsViaMachineDaemon(t *testing.T) {
	dc, dl, mc, ml := twoBearerDaemons(t, nil)
	cfg := &config.Config{PerAgentTokens: true}

	access, refresh := resolveMicrovmBeadsToken(context.Background(), discardLogger(), mc, cfg, "gd-vm1", "crew-vm1", "gardenio")
	if access == "" || refresh != "rt-fixture" {
		t.Fatalf("resolveMicrovmBeadsToken = (%q, %q), want a minted access token and the fixture refresh", access, refresh)
	}
	if err := dc.UpdateBeadFields(context.Background(), "gd-vm1", map[string]string{"agent_state": "spawning"}); err != nil {
		t.Fatalf("UpdateBeadFields: %v", err)
	}
	assertTokenMintSplit(t, dl, ml, 1)
}

// Bridge fleet-token rotator: Check lists and Rotate registers through
// machineDaemon (the BridgeClient main.go hands bridgetokenrotator.New).
func TestMachinePrincipal_BridgeRotatorMintsViaMachineDaemon(t *testing.T) {
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	fleets := []map[string]any{{
		"name":       "slack-bridge",
		"project":    "gardenio",
		"subject":    "bridge:slack",
		"expires_at": now.Add(24 * time.Hour).UTC().Format(time.RFC3339Nano),
	}}
	dc, dl, mc, ml := twoBearerDaemons(t, fleets)
	k8s := fake.NewSimpleClientset(
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "bridge-fleet-tokens", Namespace: "ns"},
			Data: map[string][]byte{
				"slack-bridge":         []byte("old-access"),
				"slack-bridge-refresh": []byte("old-refresh"),
				"fleet-map":            []byte("old-access:slack-bridge"),
			},
		},
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "slack-bridge", Namespace: "ns"}},
	)
	r, err := bridgetokenrotator.New(bridgetokenrotator.Config{
		Enabled:   true,
		Namespace: "ns",
		Fleets:    []bridgetokenrotator.BridgeSpec{{Fleet: "slack-bridge"}},
	}, mc, k8s, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	r.SetNowFunc(func() time.Time { return now })

	cands, err := r.Check(context.Background())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if len(cands) != 1 {
		t.Fatalf("candidates = %+v, want the 24h-remaining fleet", cands)
	}
	if err := r.Rotate(context.Background(), cands[0]); err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	if err := dc.UpdateBeadFields(context.Background(), "gd-x", map[string]string{"agent_state": "idle"}); err != nil {
		t.Fatalf("UpdateBeadFields: %v", err)
	}
	if got := ml.matching("GET /v1/fleet-tokens"); len(got) != 1 || got[0].bearer != tokenMintMachineBearer {
		t.Errorf("rotator list on machine daemon = %+v, want one call with the machine token", got)
	}
	assertTokenMintSplit(t, dl, ml, 1)
}
