package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/bridge"
)

// Machine principal for slack-bridge (gd-Hc34EENbO9 component 4): the config
// seed is the only startup write; with the principal on it carries the machine
// token and only the bridge-owned titles, retried on auth_unavailable; with it
// off it is today's full EnsureConfigs on the fleet token.

type seedRecorder struct {
	mu      sync.Mutex
	auth    []string
	created []string // titles of POSTed config beads
}

func (r *seedRecorder) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	r.auth = append(r.auth, req.Header.Get("Authorization"))
	if req.Method == http.MethodPost {
		var body struct {
			Title string `json:"title"`
		}
		b, _ := io.ReadAll(req.Body)
		_ = json.Unmarshal(b, &body)
		r.created = append(r.created, body.Title)
	}
	r.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"id":"gd-cfg","beads":[]}`))
}

type fakeMachineTS struct {
	failFirst int32
	calls     atomic.Int32
}

func (f *fakeMachineTS) Token(context.Context) (string, error) {
	if f.calls.Add(1) <= f.failFirst {
		return "", fmt.Errorf("token endpoint refused: %w", beadsapi.ErrMachineAuthUnavailable)
	}
	return "machine-jwt", nil
}

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)) }

func seedClient(t *testing.T, url string) *beadsapi.Client {
	t.Helper()
	c, err := beadsapi.New(beadsapi.Config{HTTPAddr: url, Token: "fleet-static"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func TestSeedConfigsAsMachine_OwnedTitlesOnTheMachineToken(t *testing.T) {
	rec := &seedRecorder{}
	srv := httptest.NewServer(rec)
	defer srv.Close()
	if err := seedConfigsAsMachine(context.Background(), seedClient(t, srv.URL), &fakeMachineTS{}, discard()); err != nil {
		t.Fatalf("seedConfigsAsMachine: %v", err)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.auth) == 0 {
		t.Fatal("daemon saw no calls")
	}
	for i, a := range rec.auth {
		if a != "Bearer machine-jwt" {
			t.Errorf("call %d Authorization=%q, want the machine token", i, a)
		}
	}
	want := bridge.SlackBridgeConfigKeys()
	sort.Strings(want)
	got := append([]string(nil), rec.created...)
	sort.Strings(got)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("created %v, want exactly the bridge-owned titles %v", got, want)
	}
}

func TestSeedConfigsAsMachine_RetriesOnAuthUnavailableOnly(t *testing.T) {
	orig := seedSleep
	seedSleep = func(context.Context, time.Duration) error { return nil }
	defer func() { seedSleep = orig }()
	rec := &seedRecorder{}
	srv := httptest.NewServer(rec)
	defer srv.Close()
	ts := &fakeMachineTS{failFirst: 3}
	if err := seedConfigsAsMachine(context.Background(), seedClient(t, srv.URL), ts, discard()); err != nil {
		t.Fatalf("seedConfigsAsMachine: %v", err)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	// An attempt that fails part-way re-creates on the next one (the stub lists
	// nothing), so compare the set of titles, not the count.
	seen := map[string]bool{}
	for _, title := range rec.created {
		seen[title] = true
	}
	if len(seen) != len(bridge.SlackBridgeConfigKeys()) {
		t.Fatalf("created %v after the retry, want every owned title", rec.created)
	}
	for _, a := range rec.auth {
		if a != "Bearer machine-jwt" {
			t.Errorf("Authorization=%q: a call went out on a non-machine credential during retry", a)
		}
	}
}

// forbiddenThenOK answers the first n requests 403 (the role Keycloak had not
// attached yet), then behaves like seedRecorder.
type forbiddenThenOK struct {
	seedRecorder
	forbid int
	n      int
}

func (f *forbiddenThenOK) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	f.mu.Lock()
	f.n++
	deny := f.n <= f.forbid
	f.mu.Unlock()
	if deny {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	f.seedRecorder.ServeHTTP(w, req)
}

func TestSeedConfigsAsMachine_RetriesOn403(t *testing.T) {
	orig := seedSleep
	slept := 0
	seedSleep = func(context.Context, time.Duration) error { slept++; return nil }
	defer func() { seedSleep = orig }()
	h := &forbiddenThenOK{forbid: 2}
	srv := httptest.NewServer(h)
	defer srv.Close()
	if err := seedConfigsAsMachine(context.Background(), seedClient(t, srv.URL), &fakeMachineTS{}, discard()); err != nil {
		t.Fatalf("seedConfigsAsMachine: %v", err)
	}
	if slept == 0 {
		t.Fatal("a 403 must be retried with backoff, not abandoned")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	seen := map[string]bool{}
	for _, title := range h.created {
		seen[title] = true
	}
	if len(seen) != len(bridge.SlackBridgeConfigKeys()) {
		t.Fatalf("created %v after the 403 retry, want every owned title", h.created)
	}
}

func TestSeedConfigsAsMachine_OtherDaemon4xxIsNotRetried(t *testing.T) {
	orig := seedSleep
	seedSleep = func(context.Context, time.Duration) error { t.Fatal("a non-403 4xx must not retry"); return nil }
	defer func() { seedSleep = orig }()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":"bad"}`, http.StatusBadRequest)
	}))
	defer srv.Close()
	if err := seedConfigsAsMachine(context.Background(), seedClient(t, srv.URL), &fakeMachineTS{}, discard()); err != nil {
		t.Fatalf("a real 4xx is logged and non-fatal, got %v", err)
	}
}

func TestSeedConfigsAsMachine_NeverFallsBackToTheFleetToken(t *testing.T) {
	orig := seedSleep
	defer func() { seedSleep = orig }()
	ctx, cancel := context.WithCancel(context.Background())
	seedSleep = func(context.Context, time.Duration) error { cancel(); return ctx.Err() }
	rec := &seedRecorder{}
	srv := httptest.NewServer(rec)
	defer srv.Close()
	err := seedConfigsAsMachine(ctx, seedClient(t, srv.URL), &fakeMachineTS{failFirst: 1 << 30}, discard())
	if err == nil || !beadsapi.IsAuthUnavailable(err) {
		t.Fatalf("err=%v, want an abandoned auth_unavailable seed", err)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.auth) != 0 {
		t.Fatalf("daemon saw %d calls with no machine token, want 0 (no fleet-token fallback)", len(rec.auth))
	}
}

func TestSeedConfigs_WithoutMachineIsTodaysFullSeedOnTheFleetToken(t *testing.T) {
	rec := &seedRecorder{}
	srv := httptest.NewServer(rec)
	defer srv.Close()
	seedConfigs(context.Background(), seedClient(t, srv.URL), nil, discard())
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.created) <= len(bridge.SlackBridgeConfigKeys()) {
		t.Fatalf("created %d titles; the disabled path must seed the full configs() set", len(rec.created))
	}
	for i, a := range rec.auth {
		if a != "Bearer fleet-static" {
			t.Errorf("call %d Authorization=%q, want the fleet token", i, a)
		}
	}
}
