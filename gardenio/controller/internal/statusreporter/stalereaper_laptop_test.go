package statusreporter

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// laptopFixture serves the agent list, GET /v1/beads/{id} for runner and
// agent beads, and records PATCH field updates and closes per bead. A runner
// id in failRunners answers 500, the daemon blip runnerStatusFor must not
// read as a gone runner.
type laptopFixture struct {
	mu          sync.Mutex
	agents      []beadEntry
	runners     map[string]beadEntry
	failRunners map[string]bool
	patches     map[string]map[string]string
	closed      map[string]bool
}

func (f *laptopFixture) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/beads":
			_ = json.NewEncoder(w).Encode(struct {
				Beads []beadEntry `json:"beads"`
			}{f.agents})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/beads/"):
			id := strings.TrimPrefix(r.URL.Path, "/v1/beads/")
			if f.failRunners[id] {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			if b, ok := f.runners[id]; ok {
				_ = json.NewEncoder(w).Encode(b)
				return
			}
			for _, a := range f.agents {
				if a.ID == id {
					_ = json.NewEncoder(w).Encode(a)
					return
				}
			}
			w.WriteHeader(http.StatusNotFound)
		case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/v1/beads/"):
			id := strings.TrimPrefix(r.URL.Path, "/v1/beads/")
			var body struct {
				Fields map[string]any `json:"fields"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if f.patches[id] == nil {
				f.patches[id] = map[string]string{}
			}
			for k, v := range body.Fields {
				f.patches[id][k] = fmt.Sprint(v)
			}
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/close"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/beads/"), "/close")
			f.closed[id] = true
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost:
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

func laptopAgent(id, name, state, runnerID string, updatedAgo time.Duration) beadEntry {
	fields := map[string]string{"agent": name, "role": "local", "project": "pet", "agent_state": state, "execution_target": "laptop"}
	if runnerID != "" {
		fields["runtime_id"] = runnerID
	}
	f, _ := json.Marshal(fields)
	return beadEntry{ID: id, Title: name, Type: "agent", Kind: "config", Status: "open", Fields: f, UpdatedAt: time.Now().Add(-updatedAgo).UTC().Format(time.RFC3339)}
}

func runnerEntry(id, state string, seenAgo time.Duration) beadEntry {
	f, _ := json.Marshal(map[string]string{"hostname": "laptop", "owner": "oidc:a@example.com", "state": state, "last_seen_at": time.Now().Add(-seenAgo).UTC().Format(time.RFC3339)})
	return beadEntry{ID: id, Title: "laptop", Type: "runner", Kind: "config", Status: "open", Fields: f}
}

func newLaptopFixture(agents []beadEntry, runners map[string]beadEntry) *laptopFixture {
	return &laptopFixture{
		agents:      agents,
		runners:     runners,
		failRunners: map[string]bool{},
		patches:     map[string]map[string]string{},
		closed:      map[string]bool{},
	}
}

func newLaptopReaper(t *testing.T, fx *laptopFixture) (*StaleReaper, *mockPodManager) {
	t.Helper()
	srv := httptest.NewServer(fx.handler())
	t.Cleanup(srv.Close)
	daemon, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	pods := &mockPodManager{}
	return NewStaleReaper(daemon, pods, "gardenio", 2*time.Hour, staleReaperLogger()), pods
}

// A revoked or missing runner closes its laptop agent; an away runner (an
// explicit away, or a heartbeat older than runnerAwayAfter) pauses it.
func TestStaleReaper_LaptopAgent_RevokedRunnerCloses_AwayPauses(t *testing.T) {
	fx := newLaptopFixture(
		[]beadEntry{
			laptopAgent("gd-a1", "local-stale-beat", "working", "gd-r-stale", 3*time.Hour),
			laptopAgent("gd-a2", "local-revoked", "working", "gd-r-revoked", 3*time.Hour),
			laptopAgent("gd-a3", "local-gone", "working", "gd-r-missing", 3*time.Hour),
			laptopAgent("gd-a4", "local-left", "idle", "gd-r-away", time.Minute),
		},
		map[string]beadEntry{
			"gd-r-stale":   runnerEntry("gd-r-stale", "online", 10*time.Minute),
			"gd-r-revoked": runnerEntry("gd-r-revoked", "revoked", time.Second),
			"gd-r-away":    runnerEntry("gd-r-away", "away", time.Second),
		},
	)
	r, pods := newLaptopReaper(t, fx)
	stats := r.Reap(context.Background())

	fx.mu.Lock()
	defer fx.mu.Unlock()
	for _, id := range []string{"gd-a1", "gd-a4"} {
		if fx.patches[id]["agent_state"] != "paused" {
			t.Errorf("%s: agent with an away runner must be paused, patches=%v", id, fx.patches[id])
		}
		if fx.closed[id] {
			t.Errorf("%s: a paused laptop agent must not be closed", id)
		}
	}
	if stats.Paused != 2 {
		t.Errorf("stats.Paused = %d, want 2", stats.Paused)
	}
	for _, id := range []string{"gd-a2", "gd-a3"} {
		if !fx.closed[id] {
			t.Errorf("%s: agent whose runner is revoked or missing must be closed", id)
		}
	}
	if stats.Reaped != 2 {
		t.Errorf("stats.Reaped = %d, want 2", stats.Reaped)
	}
	if len(pods.deleted) != 0 {
		t.Errorf("a laptop agent has no pod to delete, deleted=%v", pods.deleted)
	}
}

// A paused agent resumes when its runner is back, and one whose runner stays
// away is held however old its bead is (D5: no timeout).
func TestStaleReaper_LaptopAgent_PausedResumesWhenRunnerReturns_NeverTimesOut(t *testing.T) {
	fx := newLaptopFixture(
		[]beadEntry{
			laptopAgent("gd-a1", "local-back", "paused", "gd-r1", 3*time.Hour),
			laptopAgent("gd-a2", "local-still-away", "paused", "gd-r2", 48*time.Hour),
		},
		map[string]beadEntry{
			"gd-r1": runnerEntry("gd-r1", "online", time.Second),
			"gd-r2": runnerEntry("gd-r2", "away", time.Second),
		},
	)
	r, _ := newLaptopReaper(t, fx)
	stats := r.Reap(context.Background())

	fx.mu.Lock()
	defer fx.mu.Unlock()
	if fx.patches["gd-a1"]["agent_state"] != "working" || stats.Resumed != 1 {
		t.Fatalf("paused agent with its runner back must resume: patches=%v resumed=%d", fx.patches["gd-a1"], stats.Resumed)
	}
	if _, touched := fx.patches["gd-a2"]; touched || fx.closed["gd-a2"] || stats.Reaped != 0 {
		t.Fatalf("a paused laptop agent never times out (D5): patches=%v closed=%v reaped=%d", fx.patches["gd-a2"], fx.closed["gd-a2"], stats.Reaped)
	}
}

// Left to gb: an online runner's working agent (no staleness reap, however
// old), an agent with no runtime_id, a stopping agent whose runner is away,
// and any agent whose runner cannot be read this pass.
func TestStaleReaper_LaptopAgent_LeftAlone(t *testing.T) {
	fx := newLaptopFixture(
		[]beadEntry{
			laptopAgent("gd-a1", "local-online", "working", "gd-r1", 48*time.Hour),
			laptopAgent("gd-a2", "local-unjoined", "working", "", 48*time.Hour),
			laptopAgent("gd-a3", "local-stopping", "stopping", "gd-r2", 48*time.Hour),
			laptopAgent("gd-a4", "local-blip", "working", "gd-r3", 48*time.Hour),
		},
		map[string]beadEntry{
			"gd-r1": runnerEntry("gd-r1", "online", time.Second),
			"gd-r2": runnerEntry("gd-r2", "away", time.Second),
		},
	)
	fx.failRunners["gd-r3"] = true
	r, _ := newLaptopReaper(t, fx)
	stats := r.Reap(context.Background())

	fx.mu.Lock()
	defer fx.mu.Unlock()
	if len(fx.patches) != 0 || len(fx.closed) != 0 {
		t.Fatalf("these laptop agents must be left alone: patches=%v closed=%v", fx.patches, fx.closed)
	}
	if stats.Reaped != 0 || stats.Paused != 0 || stats.Resumed != 0 {
		t.Fatalf("stats = %+v, want no reap, pause or resume", stats)
	}
}
