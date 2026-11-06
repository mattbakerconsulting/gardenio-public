package beadsapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fixedTokenSource returns one token, or an error, on every call.
type fixedTokenSource struct {
	tok string
	err error
}

func (f fixedTokenSource) Token(context.Context) (string, error) { return f.tok, f.err }

func newRecordingClient(t *testing.T, enforce bool) (*Client, *recordingDaemon) {
	t.Helper()
	d := &recordingDaemon{}
	srv := httptest.NewServer(d.handler())
	t.Cleanup(srv.Close)
	c, err := New(Config{HTTPAddr: srv.URL, Token: "static-read-token"})
	if err != nil {
		t.Fatal(err)
	}
	c.SetRequireUserBearerForWrites(enforce)
	return c, d
}

func lastAuth(t *testing.T, d *recordingDaemon) string {
	t.Helper()
	reqs := d.requests()
	if len(reqs) == 0 {
		t.Fatal("no request reached the daemon")
	}
	return reqs[len(reqs)-1].Header.Get("Authorization")
}

// Under write-bearer enforcement a write with no bearer fails closed, a write
// on a machine-principal ctx goes out with the machine token, a read on that
// ctx does too, and a plain read stays on the client's own token.
func TestWireCredential_MachinePrincipalUnderEnforcement(t *testing.T) {
	c, d := newRecordingClient(t, true)
	ctx := context.Background()

	err := c.doJSON(ctx, http.MethodPost, "/v1/beads", map[string]any{"title": "x"}, nil)
	if !errors.Is(err, ErrUserBearerRequired) {
		t.Fatalf("bearer-less write under enforcement: err=%v, want ErrUserBearerRequired", err)
	}
	if n := len(d.requests()); n != 0 {
		t.Fatalf("bearer-less write sent %d requests; want 0", n)
	}

	mctx := WithMachinePrincipal(ctx, fixedTokenSource{tok: "machine-jwt"})
	if err := c.doJSON(mctx, http.MethodPost, "/v1/beads", map[string]any{"title": "x"}, nil); err != nil {
		t.Fatalf("machine write: %v", err)
	}
	if got := lastAuth(t, d); got != "Bearer machine-jwt" {
		t.Errorf("machine write Authorization=%q", got)
	}
	if err := c.doJSON(mctx, http.MethodGet, "/v1/beads/gd-1", nil, nil); err != nil {
		t.Fatalf("machine read: %v", err)
	}
	if got := lastAuth(t, d); got != "Bearer machine-jwt" {
		t.Errorf("machine read Authorization=%q (reads on a reactor ctx act as the machine too)", got)
	}
	if err := c.doJSON(ctx, http.MethodGet, "/v1/beads/gd-1", nil, nil); err != nil {
		t.Fatalf("plain read: %v", err)
	}
	if got := lastAuth(t, d); got != "Bearer static-read-token" {
		t.Errorf("plain read Authorization=%q", got)
	}
}

// The human wins: a ctx carrying both a user bearer and a machine principal
// writes as the user.
func TestWireCredential_UserBearerBeatsMachinePrincipal(t *testing.T) {
	c, d := newRecordingClient(t, true)
	ctx := WithMachinePrincipal(WithUserBearer(context.Background(), "user-jwt"), fixedTokenSource{tok: "machine-jwt"})
	if err := c.doJSON(ctx, http.MethodPatch, "/v1/beads/gd-1", map[string]any{"title": "y"}, nil); err != nil {
		t.Fatal(err)
	}
	if got := lastAuth(t, d); got != "Bearer user-jwt" {
		t.Errorf("Authorization=%q, want the user's bearer", got)
	}
}

// Without enforcement the machine principal still applies (so a dev run with
// the BEADS_OIDC_* env set exercises the same path), and a plain write stays
// on the client's own token exactly as before.
func TestWireCredential_MachinePrincipalWithoutEnforcement(t *testing.T) {
	c, d := newRecordingClient(t, false)
	if err := c.doJSON(WithMachinePrincipal(context.Background(), fixedTokenSource{tok: "machine-jwt"}), http.MethodPost, "/v1/beads", map[string]any{}, nil); err != nil {
		t.Fatal(err)
	}
	if got := lastAuth(t, d); got != "Bearer machine-jwt" {
		t.Errorf("machine write Authorization=%q", got)
	}
	if err := c.doJSON(context.Background(), http.MethodPost, "/v1/beads", map[string]any{}, nil); err != nil {
		t.Fatal(err)
	}
	if got := lastAuth(t, d); got != "Bearer static-read-token" {
		t.Errorf("plain write Authorization=%q", got)
	}
}

// A machine token fetch failure fails the call closed: no request is sent and
// the static token is never substituted. The error carries the source's text
// (auth_unavailable from the real machineTokenSource) and never a secret.
func TestWireCredential_MachineFetchFailureFailsClosed(t *testing.T) {
	c, d := newRecordingClient(t, true)
	ctx := WithMachinePrincipal(context.Background(), fixedTokenSource{err: errors.New(machineAuthUnavailable + ": token endpoint 503")})
	err := c.doJSON(ctx, http.MethodPost, "/v1/beads", map[string]any{}, nil)
	if err == nil || !strings.Contains(err.Error(), machineAuthUnavailable) {
		t.Fatalf("err=%v, want auth_unavailable", err)
	}
	if n := len(d.requests()); n != 0 {
		t.Fatalf("failed machine fetch still sent %d requests; want 0 (no static fallback)", n)
	}
	// An empty token is treated the same way.
	err = c.doJSON(WithMachinePrincipal(context.Background(), fixedTokenSource{}), http.MethodPost, "/v1/beads", map[string]any{}, nil)
	if err == nil || !strings.Contains(err.Error(), machineAuthUnavailable) {
		t.Fatalf("empty token: err=%v, want auth_unavailable", err)
	}
	if n := len(d.requests()); n != 0 {
		t.Fatalf("empty machine token still sent %d requests; want 0", n)
	}
}

// The real machineTokenSource end to end: a fixture token endpoint, a
// recording daemon, and the client proving which credential carries which call.
func TestWireCredential_RealMachineTokenSource(t *testing.T) {
	_, tokenSrv, tok := newFixtureEndpoint(t, 1800)
	secretFile := writeSecretFile(t, t.TempDir(), "s3cr3t-value")
	// The fixture endpoint only knows the controller client; the client id is
	// irrelevant to what this test proves (which credential carries the call).
	setMachineEnv(t, tokenSrv.URL+"/token", "gardenio-controller", secretFile)
	ts, ok, err := MachineTokenSourceFromEnv(context.Background(), nil)
	if err != nil || !ok {
		t.Fatalf("MachineTokenSourceFromEnv: ok=%v err=%v", ok, err)
	}
	c, d := newRecordingClient(t, true)
	if err := c.doJSON(WithMachinePrincipal(context.Background(), ts), http.MethodPost, "/v1/beads", map[string]any{}, nil); err != nil {
		t.Fatal(err)
	}
	if got := lastAuth(t, d); got != "Bearer "+tok {
		t.Errorf("Authorization=%q, want the fetched machine token", got)
	}
	for _, r := range d.requests() {
		if strings.Contains(r.Header.Get("Authorization"), "s3cr3t-value") {
			t.Fatal("client secret leaked onto the daemon wire")
		}
	}
}
