package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// Machine principal split (gd-s7aPIf0Bgp): every account-bead WRITE goes
// through the `accounts` client (the controller's own Keycloak service account
// in production) while reads stay on `daemon` (the static token). These tests
// stand up two recording daemons and prove each account writer sends its PATCH
// to accounts and its list to daemon — never the other way round.

// routeLog records "METHOD /path" for every request a stub daemon receives.
type routeLog struct {
	mu    sync.Mutex
	calls []string
}

func (l *routeLog) wrap(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		l.mu.Lock()
		l.calls = append(l.calls, r.Method+" "+r.URL.Path)
		l.mu.Unlock()
		h.ServeHTTP(w, r)
	})
}

func (l *routeLog) count(method, pathPrefix string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, c := range l.calls {
		if strings.HasPrefix(c, method+" "+pathPrefix) {
			n++
		}
	}
	return n
}

func (l *routeLog) countExact(call string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, c := range l.calls {
		if c == call {
			n++
		}
	}
	return n
}

func (l *routeLog) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.calls...)
}

// twoDaemons serves handler behind two independent recording servers and
// returns a beadsapi client for each: (daemon, accounts).
func twoDaemons(t *testing.T, daemonH, accountsH http.Handler) (*beadsapi.Client, *routeLog, *beadsapi.Client, *routeLog) {
	t.Helper()
	dl, al := &routeLog{}, &routeLog{}
	dsrv := httptest.NewServer(dl.wrap(daemonH))
	t.Cleanup(dsrv.Close)
	asrv := httptest.NewServer(al.wrap(accountsH))
	t.Cleanup(asrv.Close)
	dc, err := beadsapi.New(beadsapi.Config{HTTPAddr: dsrv.URL, Token: "static-token"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dc.Close() })
	ac, err := beadsapi.New(beadsapi.Config{HTTPAddr: asrv.URL, Token: "machine-token"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ac.Close() })
	return dc, dl, ac, al
}

// assertSplit is the shared verdict: list on daemon only, PATCH on accounts only.
func assertSplit(t *testing.T, dl, al *routeLog) {
	t.Helper()
	if dl.count("PATCH", "/v1/beads/") != 0 {
		t.Errorf("daemon (static token) received an account PATCH: %v", dl.all())
	}
	if al.count("PATCH", "/v1/beads/") == 0 {
		t.Errorf("accounts (machine principal) received no PATCH: accounts=%v daemon=%v", al.all(), dl.all())
	}
	if dl.countExact("GET /v1/beads") == 0 {
		t.Errorf("daemon received no list call: %v", dl.all())
	}
	if al.countExact("GET /v1/beads") != 0 {
		t.Errorf("accounts received a list call: %v", al.all())
	}
}

func TestMachinePrincipal_SpendSyncWritesViaAccounts(t *testing.T) {
	bead := makeDaemonBead("bd-acct", "prod-account", "vk-1")
	daemon := &daemonStub{beads: []map[string]any{bead}}
	accounts := &daemonStub{beads: []map[string]any{bead}}
	dc, dl, ac, al := twoDaemons(t, daemon.handler(), accounts.handler())

	litellmSrv := httptest.NewServer(litellmRouter(map[string]float64{"vk-1": 12.5}, nil))
	t.Cleanup(litellmSrv.Close)

	ss := newSpendSync(dc, ac, litellmSrv.URL, "test-master-key", slog.Default())
	ss.now = fixedClock()
	ss.syncAll(context.Background())

	if got := accounts.getUpdates(); len(got) != 1 || got[0].BeadID != "bd-acct" {
		t.Fatalf("accounts stub updates = %+v, want one for bd-acct", got)
	}
	if got := daemon.getUpdates(); len(got) != 0 {
		t.Fatalf("daemon stub must see no updates, got %+v", got)
	}
	assertSplit(t, dl, al)
}

func TestMachinePrincipal_RateLimitProbeWritesViaAccounts(t *testing.T) {
	bead := makeDaemonBead("bd-acct", "prod-account", "")
	daemon := &daemonStub{beads: []map[string]any{bead}}
	accounts := &daemonStub{beads: []map[string]any{bead}}
	dc, dl, ac, al := twoDaemons(t, daemon.handler(), accounts.handler())

	p := newRateLimitProbe(dc, ac, nil, "gardenio", nil, nil, testProbeLogger())
	p.handleResult(context.Background(), "prod-account", beadsapi.AccountInfo{Name: "prod-account"}, probeResult{
		Status:              "allowed",
		Utilization5h:       0.1,
		Utilization7d:       0.2,
		RepresentativeClaim: "five_hour",
		HTTPStatus:          200,
		CheckedAt:           time.Now(),
	})

	if got := accounts.getUpdates(); len(got) != 1 || got[0].Fields["probe_auth_status"] != "ok" {
		t.Fatalf("accounts stub updates = %+v, want one probe update", got)
	}
	assertSplit(t, dl, al)

	// The terminal-auth path (probe_auth_status=rejected) takes the same route.
	p2 := newRateLimitProbe(dc, ac, nil, "gardenio", nil, nil, testProbeLogger())
	p2.handleResult(context.Background(), "prod-account", beadsapi.AccountInfo{Name: "prod-account"}, probeResult{
		HTTPStatus: 401,
		ProbeError: errors.New("401 unauthorized"),
		CheckedAt:  time.Now(),
	})
	if got := accounts.getUpdates(); len(got) != 2 || got[1].Fields["probe_auth_status"] != "rejected" {
		t.Fatalf("accounts stub updates after 401 = %+v", got)
	}
	if got := daemon.getUpdates(); len(got) != 0 {
		t.Fatalf("daemon stub must see no updates, got %+v", got)
	}
}

func TestMachinePrincipal_AuthFailoverWritesViaAccounts(t *testing.T) {
	store := newMockStore(
		account("gd-p", "primary", "myproj", map[string]any{
			"provider": "anthropic", "fallback": "backup", "status": "active",
			"throttle_status": "rejected", "utilization_7d": "1.00",
		}),
		account("gd-b", "backup", "myproj", map[string]any{
			"provider": "anthropic", "status": "active",
		}),
	)
	dsrv := mockDaemonServer(t, store)
	t.Cleanup(dsrv.Close)
	asrv := mockDaemonServer(t, store)
	t.Cleanup(asrv.Close)
	dc, dl, ac, al := twoDaemons(t, dsrv.Config.Handler, asrv.Config.Handler)

	f := newAuthFailover(dc, ac, testFailoverLogger(), 0, 0)
	if err := f.Handle(context.Background(), "myproj", "gd-agent1"); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got := store.get("gd-p").Fields["status"]; got != "exhausted" {
		t.Fatalf("primary status = %v, want exhausted", got)
	}
	assertSplit(t, dl, al)

	// The sweep-recovery write (status back to active) also goes via accounts.
	// (mockStore.patchFields replaces the whole map, as the daemon's typed
	// update does, so restate every field the sweep reads.)
	store.patchFields("gd-p", map[string]any{
		"provider": "anthropic", "fallback": "backup", "status": "exhausted",
		"cooldown_until":   time.Now().Add(-time.Hour).UTC().Format(time.RFC3339),
		"throttle_status":  "allowed",
		"probe_checked_at": time.Now().UTC().Format(time.RFC3339),
	})
	before := al.count("PATCH", "/v1/beads/")
	f.SweepStaleExhausted(context.Background())
	if al.count("PATCH", "/v1/beads/") <= before {
		t.Errorf("sweep recovery did not PATCH via accounts: %v", al.all())
	}
	if dl.count("PATCH", "/v1/beads/") != 0 {
		t.Errorf("sweep recovery PATCHed via daemon: %v", dl.all())
	}
}

// With no accounts client (nil) every constructor falls back to daemon, which
// is exactly today's single-client behavior.
func TestMachinePrincipal_NilAccountsFallsBackToDaemon(t *testing.T) {
	bead := makeDaemonBead("bd-acct", "prod-account", "vk-1")
	daemon := &daemonStub{beads: []map[string]any{bead}}
	srv := httptest.NewServer(daemon.handler())
	t.Cleanup(srv.Close)
	dc, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dc.Close() })
	if ss := newSpendSync(dc, nil, "http://127.0.0.1:1", "k", slog.Default()); ss.accounts != dc {
		t.Error("spendSync.accounts must default to daemon")
	}
	if p := newRateLimitProbe(dc, nil, nil, "ns", nil, nil, testProbeLogger()); p.accounts != dc {
		t.Error("rateLimitProbe.accounts must default to daemon")
	}
	if f := newAuthFailover(dc, nil, testFailoverLogger(), 0, 0); f.accounts != dc {
		t.Error("authFailover.accounts must default to daemon")
	}
}
