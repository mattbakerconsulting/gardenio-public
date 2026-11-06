package beadsapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"sort"
	"sync/atomic"
	"testing"
	"time"
)

// fakeTunableServer returns an httptest.Server whose /v1/beads endpoint
// serves the beads in `seed`. The callCount pointer is incremented on every
// request so tests can assert poll cadence. The returned pointer is safe to
// update concurrently — tests swap it between polls to simulate bead changes.
func fakeTunableServer(t *testing.T, seed *atomic.Pointer[[]beadJSON]) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	calls := &atomic.Int64{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/v1/beads" {
			http.NotFound(w, r)
			return
		}
		beads := seed.Load()
		if beads == nil {
			beads = &[]beadJSON{}
		}
		// The client filters by type/kind/status/labels server-side. We
		// emulate the label filter because scope-resolution tests rely on
		// server-side filtering being honored (otherwise every bead would
		// match every component).
		labelFilter := ""
		if q, err := url.ParseQuery(r.URL.RawQuery); err == nil {
			labelFilter = q.Get("labels")
		}
		var matched []beadJSON
		for _, b := range *beads {
			if labelFilter == "" || containsStr(b.Labels, labelFilter) {
				matched = append(matched, b)
			}
		}
		_ = json.NewEncoder(w).Encode(listBeadsResponse{Beads: matched, Total: len(matched)})
	}))
	t.Cleanup(srv.Close)
	return srv, calls
}

func containsStr(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

func tunableBead(id, title string, labels []string, fields map[string]string) beadJSON {
	raw, _ := json.Marshal(fields)
	return beadJSON{
		ID:     id,
		Title:  title,
		Type:   "config",
		Kind:   "config",
		Status: "open",
		Labels: labels,
		Fields: raw,
	}
}

// --- unit: precedence order (bead > env > compiled) ---------------------

func TestGet_BeadBeatsEnvAndCompiled(t *testing.T) {
	seed := &atomic.Pointer[[]beadJSON]{}
	seed.Store(&[]beadJSON{
		tunableBead("bd-1", "tunables",
			[]string{"component:controller"},
			map[string]string{"COOP_MAX_PODS": "42"}),
	})
	srv, _ := fakeTunableServer(t, seed)

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	w, err := NewConfigWatcher(c, "controller",
		WithPollInterval(10*time.Millisecond),
		WithLookupEnv(func(k string) (string, bool) {
			if k == "COOP_MAX_PODS" {
				return "7", true
			}
			return "", false
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if err := w.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer w.Stop()

	if got := w.GetInt("COOP_MAX_PODS", "COOP_MAX_PODS", 1); got != 42 {
		t.Fatalf("GetInt: expected bead=42, got %d", got)
	}
}

func TestGet_EnvBeatsCompiled_WhenNoBead(t *testing.T) {
	seed := &atomic.Pointer[[]beadJSON]{}
	seed.Store(&[]beadJSON{})
	srv, _ := fakeTunableServer(t, seed)

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	w, err := NewConfigWatcher(c, "controller",
		WithLookupEnv(func(k string) (string, bool) {
			if k == "COOP_SYNC_INTERVAL" {
				return "90s", true
			}
			return "", false
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := w.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer w.Stop()

	got := w.GetDuration("COOP_SYNC_INTERVAL", "COOP_SYNC_INTERVAL", 60*time.Second)
	if got != 90*time.Second {
		t.Fatalf("GetDuration: expected env=90s, got %v", got)
	}
}

func TestGet_CompiledWhenNoBeadAndNoEnv(t *testing.T) {
	seed := &atomic.Pointer[[]beadJSON]{}
	seed.Store(&[]beadJSON{})
	srv, _ := fakeTunableServer(t, seed)

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	w, err := NewConfigWatcher(c, "controller",
		WithLookupEnv(func(k string) (string, bool) { return "", false }),
	)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := w.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer w.Stop()

	if got := w.GetString("MISSING", "MISSING_ENV", "default"); got != "default" {
		t.Fatalf("GetString: expected default, got %q", got)
	}
	if got := w.GetBool("MISSING_BOOL", "MISSING_ENV", true); got != true {
		t.Fatalf("GetBool: expected true, got %v", got)
	}
}

// --- unit: parse failure falls back to compiled -------------------------

func TestGet_ParseFailureFallsBackToCompiled(t *testing.T) {
	seed := &atomic.Pointer[[]beadJSON]{}
	seed.Store(&[]beadJSON{
		tunableBead("bd-1", "tunables",
			[]string{"component:controller"},
			map[string]string{"COOP_MAX_PODS": "not-a-number"}),
	})
	srv, _ := fakeTunableServer(t, seed)

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	w, err := NewConfigWatcher(c, "controller",
		WithLookupEnv(func(k string) (string, bool) { return "", false }),
	)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := w.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer w.Stop()

	if got := w.GetInt("COOP_MAX_PODS", "COOP_MAX_PODS", 99); got != 99 {
		t.Fatalf("GetInt: expected compiled=99 on parse failure, got %d", got)
	}
}

// --- unit: scope precedence (component < role < project < project+role) ---

func TestResolveBeadValues_ScopePrecedence(t *testing.T) {
	beads := []*BeadDetail{
		{Title: "tunables", Labels: []string{"component:controller"},
			Fields: map[string]string{"KEY": "component-only", "BASE": "base"}},
		{Title: "tunables", Labels: []string{"component:controller", "role:gardener"},
			Fields: map[string]string{"KEY": "role-scoped"}},
		{Title: "tunables", Labels: []string{"component:controller", "project:gardenio"},
			Fields: map[string]string{"KEY": "project-scoped"}},
		{Title: "tunables", Labels: []string{"component:controller", "project:gardenio", "role:gardener"},
			Fields: map[string]string{"KEY": "project+role"}},
	}
	out := resolveBeadValues(beads, "controller", "gardenio", "gardener")
	if out["KEY"] != "project+role" {
		t.Fatalf("KEY: expected project+role wins, got %q", out["KEY"])
	}
	if out["BASE"] != "base" {
		t.Fatalf("BASE: expected inherited from component-only, got %q", out["BASE"])
	}
}

func TestResolveBeadValues_WrongScopeFiltered(t *testing.T) {
	beads := []*BeadDetail{
		{Title: "tunables", Labels: []string{"component:controller", "project:other"},
			Fields: map[string]string{"KEY": "other-project"}},
		{Title: "tunables", Labels: []string{"component:controller"},
			Fields: map[string]string{"KEY": "component-only"}},
	}
	out := resolveBeadValues(beads, "controller", "gardenio", "")
	if out["KEY"] != "component-only" {
		t.Fatalf("KEY: expected component-only (other-project bead filtered), got %q", out["KEY"])
	}
}

func TestResolveBeadValues_SkipsWrongComponentOrTitle(t *testing.T) {
	beads := []*BeadDetail{
		{Title: "tunables", Labels: []string{"component:slack-bridge"},
			Fields: map[string]string{"KEY": "wrong-component"}},
		{Title: "auth", Labels: []string{"component:controller"},
			Fields: map[string]string{"KEY": "wrong-title"}},
	}
	out := resolveBeadValues(beads, "controller", "", "")
	if _, ok := out["KEY"]; ok {
		t.Fatalf("KEY should be absent when no matching bead, got map: %v", out)
	}
}

// --- unit: change notification ------------------------------------------

func TestSubscribe_EmitsChangeOnValueUpdate(t *testing.T) {
	seed := &atomic.Pointer[[]beadJSON]{}
	seed.Store(&[]beadJSON{
		tunableBead("bd-1", "tunables",
			[]string{"component:controller"},
			map[string]string{"KEY": "v1"}),
	})
	srv, _ := fakeTunableServer(t, seed)

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	w, err := NewConfigWatcher(c, "controller",
		WithPollInterval(5*time.Millisecond),
		WithLookupEnv(func(string) (string, bool) { return "", false }),
	)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	ch := w.Subscribe()
	if err := w.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer w.Stop()

	// Drain the initial-load event for KEY=v1 (prev was empty).
	select {
	case first := <-ch:
		if first.Key != "KEY" || first.OldValue != "" || first.NewValue != "v1" {
			t.Fatalf("unexpected initial change: %+v", first)
		}
	case <-ctx.Done():
		t.Fatalf("timed out waiting for initial ConfigChange")
	}

	// Swap in a new bead with a different value.
	seed.Store(&[]beadJSON{
		tunableBead("bd-1", "tunables",
			[]string{"component:controller"},
			map[string]string{"KEY": "v2"}),
	})

	select {
	case change := <-ch:
		if change.Key != "KEY" || change.OldValue != "v1" || change.NewValue != "v2" {
			t.Fatalf("unexpected change: %+v", change)
		}
	case <-ctx.Done():
		t.Fatalf("timed out waiting for ConfigChange")
	}
}

func TestSubscribe_EmitsAdditionAndDeletion(t *testing.T) {
	seed := &atomic.Pointer[[]beadJSON]{}
	seed.Store(&[]beadJSON{
		tunableBead("bd-1", "tunables",
			[]string{"component:controller"},
			map[string]string{"A": "1"}),
	})
	srv, _ := fakeTunableServer(t, seed)

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	w, err := NewConfigWatcher(c, "controller",
		WithPollInterval(5*time.Millisecond),
		WithLookupEnv(func(string) (string, bool) { return "", false }),
	)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	ch := w.Subscribe()
	if err := w.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer w.Stop()

	// Add a key and remove the existing one in the same refresh.
	seed.Store(&[]beadJSON{
		tunableBead("bd-1", "tunables",
			[]string{"component:controller"},
			map[string]string{"B": "2"}),
	})

	var seen []ConfigChange
	deadline := time.After(1500 * time.Millisecond)
loop:
	for len(seen) < 2 {
		select {
		case c := <-ch:
			seen = append(seen, c)
		case <-deadline:
			break loop
		}
	}
	if len(seen) < 2 {
		t.Fatalf("expected 2 changes (add + delete), got %d: %+v", len(seen), seen)
	}
	keys := []string{seen[0].Key, seen[1].Key}
	sort.Strings(keys)
	if !reflect.DeepEqual(keys, []string{"A", "B"}) {
		t.Fatalf("expected changes for keys [A B], got %v", keys)
	}
}

// --- integration-flavored: full lifecycle over a real HTTP server -------

func TestConfigWatcher_Integration_FullLifecycle(t *testing.T) {
	seed := &atomic.Pointer[[]beadJSON]{}
	seed.Store(&[]beadJSON{
		tunableBead("bd-1", "tunables",
			[]string{"component:controller"},
			map[string]string{
				"COOP_MAX_PODS":      "20",
				"COOP_SYNC_INTERVAL": "45s",
				"FEATURE_X":          "true",
			}),
	})
	srv, calls := fakeTunableServer(t, seed)

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	w, err := NewConfigWatcher(c, "controller",
		WithPollInterval(10*time.Millisecond),
		WithProject("gardenio"),
		WithLookupEnv(func(k string) (string, bool) {
			// Env provides a fallback for a key the bead does not carry.
			if k == "FALLBACK_ONLY" {
				return "env-wins", true
			}
			return "", false
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := w.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer w.Stop()

	// Initial values come from the bead.
	if got := w.GetInt("COOP_MAX_PODS", "COOP_MAX_PODS", 1); got != 20 {
		t.Fatalf("COOP_MAX_PODS: expected 20, got %d", got)
	}
	if got := w.GetDuration("COOP_SYNC_INTERVAL", "COOP_SYNC_INTERVAL", time.Second); got != 45*time.Second {
		t.Fatalf("COOP_SYNC_INTERVAL: expected 45s, got %v", got)
	}
	if got := w.GetBool("FEATURE_X", "FEATURE_X", false); !got {
		t.Fatalf("FEATURE_X: expected true, got false")
	}
	if got := w.GetString("FALLBACK_ONLY", "FALLBACK_ONLY", "compiled"); got != "env-wins" {
		t.Fatalf("FALLBACK_ONLY: expected env-wins, got %q", got)
	}

	// Add a project-scoped override that should beat the component bead.
	seed.Store(&[]beadJSON{
		tunableBead("bd-1", "tunables",
			[]string{"component:controller"},
			map[string]string{"COOP_MAX_PODS": "20"}),
		tunableBead("bd-2", "tunables",
			[]string{"component:controller", "project:gardenio"},
			map[string]string{"COOP_MAX_PODS": "77"}),
	})

	// Wait for the refresh to pick up the change.
	deadline := time.After(2 * time.Second)
	for {
		if w.GetInt("COOP_MAX_PODS", "COOP_MAX_PODS", 0) == 77 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("project-scoped override not observed; current value=%d", w.GetInt("COOP_MAX_PODS", "COOP_MAX_PODS", 0))
		case <-time.After(10 * time.Millisecond):
		}
	}

	if calls.Load() < 2 {
		t.Fatalf("expected at least 2 HTTP calls (initial + poll), got %d", calls.Load())
	}

	// Snapshot returns a safe copy.
	snap := w.Snapshot()
	if snap["COOP_MAX_PODS"] != "77" {
		t.Fatalf("snapshot COOP_MAX_PODS: expected 77, got %q", snap["COOP_MAX_PODS"])
	}
	snap["COOP_MAX_PODS"] = "tampered"
	if w.GetInt("COOP_MAX_PODS", "COOP_MAX_PODS", 0) != 77 {
		t.Fatalf("watcher value mutated via snapshot")
	}
}

// --- unit: ready signal -------------------------------------------------

func TestStart_ReadyClosesAfterFirstLoad(t *testing.T) {
	seed := &atomic.Pointer[[]beadJSON]{}
	seed.Store(&[]beadJSON{})
	srv, _ := fakeTunableServer(t, seed)

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	w, err := NewConfigWatcher(c, "controller")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := w.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer w.Stop()

	select {
	case <-w.Ready():
	case <-time.After(time.Second):
		t.Fatalf("Ready channel did not close after Start")
	}
}

// --- unit: constructor validation ---------------------------------------

func TestNewConfigWatcher_RequiresClientAndComponent(t *testing.T) {
	if _, err := NewConfigWatcher(nil, "x"); err == nil {
		t.Fatal("expected error for nil client")
	}
	if _, err := NewConfigWatcher(&Client{}, ""); err == nil {
		t.Fatal("expected error for empty component")
	}
}

// --- unit: Stop is idempotent -------------------------------------------

func TestStop_Idempotent(t *testing.T) {
	seed := &atomic.Pointer[[]beadJSON]{}
	seed.Store(&[]beadJSON{})
	srv, _ := fakeTunableServer(t, seed)

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	w, err := NewConfigWatcher(c, "controller")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_ = w.Start(ctx)
	w.Stop()
	w.Stop() // must not panic
}
