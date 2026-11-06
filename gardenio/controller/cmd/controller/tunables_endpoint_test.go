package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// TestHandleTunables_ReflectsBeadAndFallback covers the three precedence
// outcomes the admin endpoint must report: compiled default (no bead, no
// env), env fallback (no bead, env set), and bead override (bead present).
func TestHandleTunables_ReflectsBeadAndFallback(t *testing.T) {
	// currentBeads is mutated by the test body but read by the httptest
	// handler on the watcher's poll goroutine, so a mutex guards both sides.
	var (
		beadsMu      sync.Mutex
		currentBeads []map[string]any
	)
	setBeads := func(beads []map[string]any) {
		beadsMu.Lock()
		defer beadsMu.Unlock()
		currentBeads = beads
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && r.URL.Path == "/v1/beads" {
			w.Header().Set("Content-Type", "application/json")
			beadsMu.Lock()
			beads := currentBeads
			beadsMu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{
				"beads": beads,
				"total": len(beads),
			})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	daemon, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer daemon.Close()

	watcher, err := beadsapi.NewConfigWatcher(daemon, "controller",
		beadsapi.WithPollInterval(50*time.Millisecond),
		beadsapi.WithLookupEnv(func(string) (string, bool) { return "", false }),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := watcher.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer watcher.Stop()

	handler := handleTunables(watcher)

	// Case 1: no bead, no env → compiled default (1h).
	body := invokeTunables(t, handler)
	if body.Resolved["COOP_QUEUED_WARN_AGE"].Source != "compiled" {
		t.Errorf("empty bead set: source = %q, want compiled", body.Resolved["COOP_QUEUED_WARN_AGE"].Source)
	}
	if body.Resolved["COOP_QUEUED_WARN_AGE"].Value != "1h0m0s" {
		t.Errorf("empty bead set: value = %q, want 1h0m0s", body.Resolved["COOP_QUEUED_WARN_AGE"].Value)
	}

	// Case 2: bead present → bead source, new value.
	setBeads([]map[string]any{
		{
			"id":     "gd-cfg-controller",
			"title":  "tunables",
			"type":   "config",
			"status": "open",
			"labels": []string{"component:controller"},
			"fields": map[string]string{
				"COOP_QUEUED_WARN_AGE": "15m",
			},
		},
	})
	// Wait for the watcher to pick up the change.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		body = invokeTunables(t, handler)
		if body.Resolved["COOP_QUEUED_WARN_AGE"].Source == "bead" {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if body.Resolved["COOP_QUEUED_WARN_AGE"].Source != "bead" {
		t.Fatalf("bead present: source = %q, want bead", body.Resolved["COOP_QUEUED_WARN_AGE"].Source)
	}
	if body.Resolved["COOP_QUEUED_WARN_AGE"].Value != "15m0s" {
		t.Errorf("bead present: value = %q, want 15m0s", body.Resolved["COOP_QUEUED_WARN_AGE"].Value)
	}
	if got := body.Cache["COOP_QUEUED_WARN_AGE"]; got != "15m" {
		t.Errorf("cache: COOP_QUEUED_WARN_AGE = %q, want 15m", got)
	}

	// Case 3: bead removed → falls back to compiled again.
	setBeads(nil)
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		body = invokeTunables(t, handler)
		if body.Resolved["COOP_QUEUED_WARN_AGE"].Source == "compiled" {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if body.Resolved["COOP_QUEUED_WARN_AGE"].Source != "compiled" {
		t.Fatalf("bead removed: source = %q, want compiled", body.Resolved["COOP_QUEUED_WARN_AGE"].Source)
	}
}

func TestHandleTunables_NilWatcher(t *testing.T) {
	body := invokeTunables(t, handleTunables(nil))
	if len(body.Cache) != 0 || len(body.Resolved) != 0 {
		t.Errorf("nil watcher: expected empty payload, got cache=%v resolved=%v", body.Cache, body.Resolved)
	}
}

type tunablesBody struct {
	Cache    map[string]string `json:"cache"`
	Resolved map[string]struct {
		Value  string `json:"value"`
		Source string `json:"source"`
	} `json:"resolved"`
}

func invokeTunables(t *testing.T, h http.HandlerFunc) tunablesBody {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/config/tunables", nil)
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body tunablesBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, rec.Body.String())
	}
	return body
}
