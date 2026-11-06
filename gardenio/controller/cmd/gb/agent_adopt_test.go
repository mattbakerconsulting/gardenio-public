package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

func TestLabelSetContains(t *testing.T) {
	labels := []string{"project:gardenio", " Needs-Gardener "}
	if !labelSetContains(labels, "needs-gardener") {
		t.Error("expected case-insensitive/trimmed match for needs-gardener")
	}
	if labelSetContains(labels, "role:crew") {
		t.Error("did not expect a match for role:crew")
	}
	if labelSetContains(nil, "x") {
		t.Error("nil label set should never match")
	}
}

func TestDisplayGardener(t *testing.T) {
	if got := displayGardener(""); got != "(unset)" {
		t.Errorf("displayGardener(\"\") = %q, want (unset)", got)
	}
	if got := displayGardener("  "); got != "(unset)" {
		t.Errorf("displayGardener(blank) = %q, want (unset)", got)
	}
	if got := displayGardener("gd-cap"); got != "gd-cap" {
		t.Errorf("displayGardener(gd-cap) = %q, want gd-cap", got)
	}
}

// adoptFakeDaemon records the mutations gb agent adopt makes.
type adoptFakeDaemon struct {
	mu            sync.Mutex
	beadType      string
	beadLabels    []string
	beadFields    map[string]any
	patchedFields map[string]any
	removedLabels []string
	comments      []string
}

func (f *adoptFakeDaemon) server(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/beads/gd-agent1", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			f.mu.Lock()
			resp := map[string]any{
				"id":     "gd-agent1",
				"type":   f.beadType,
				"status": "open",
				"labels": f.beadLabels,
				"fields": f.beadFields,
			}
			f.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)
		case http.MethodPatch:
			var body struct {
				Fields json.RawMessage `json:"fields"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.mu.Lock()
			_ = json.Unmarshal(body.Fields, &f.patchedFields)
			f.mu.Unlock()
			w.WriteHeader(http.StatusOK)
		default:
			http.Error(w, "unexpected", http.StatusMethodNotAllowed)
		}
	})
	mux.HandleFunc("/v1/beads/gd-agent1/labels/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			http.Error(w, "unexpected", http.StatusMethodNotAllowed)
			return
		}
		label := strings.TrimPrefix(r.URL.Path, "/v1/beads/gd-agent1/labels/")
		f.mu.Lock()
		f.removedLabels = append(f.removedLabels, label)
		f.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/v1/beads/gd-agent1/comments", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Text string `json:"text"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.comments = append(f.comments, body.Text)
		f.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// runAdopt wires the fake daemon in, resets the adopt flags, and runs the
// command with the given gardener flag.
func runAdopt(t *testing.T, f *adoptFakeDaemon, gardener string) error {
	t.Helper()
	srv := f.server(t)
	c, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatalf("beadsapi.New: %v", err)
	}
	old := daemon
	daemon = c
	t.Cleanup(func() { daemon = old })

	_ = agentAdoptCmd.Flags().Set("gardener", gardener)
	_ = agentAdoptCmd.Flags().Set("label", "needs-gardener")
	t.Cleanup(func() {
		_ = agentAdoptCmd.Flags().Set("gardener", "")
		_ = agentAdoptCmd.Flags().Set("label", "needs-gardener")
	})
	agentAdoptCmd.SetContext(context.Background())
	// Suppress command output during tests.
	agentAdoptCmd.SetOut(&strings.Builder{})
	agentAdoptCmd.SetErr(&strings.Builder{})
	return agentAdoptCmd.RunE(agentAdoptCmd, []string{"gd-agent1"})
}

func TestAgentAdopt_RewritesGardenerAndClearsLabel(t *testing.T) {
	f := &adoptFakeDaemon{
		beadType:   "agent",
		beadLabels: []string{"project:gardenio", "needs-gardener"},
		beadFields: map[string]any{"gardener_id": "synthetic-gardener:slack-thread"},
	}
	if err := runAdopt(t, f, "gd-realgardener"); err != nil {
		t.Fatalf("adopt failed: %v", err)
	}
	if got := f.patchedFields["gardener_id"]; got != "gd-realgardener" {
		t.Fatalf("gardener_id patched to %v, want gd-realgardener", got)
	}
	if len(f.removedLabels) != 1 || f.removedLabels[0] != "needs-gardener" {
		t.Fatalf("expected needs-gardener removed, got %v", f.removedLabels)
	}
	if len(f.comments) != 1 || !strings.Contains(f.comments[0], "gd-realgardener") {
		t.Fatalf("expected audit comment mentioning gd-realgardener, got %v", f.comments)
	}
}

func TestAgentAdopt_RejectsNonAgentBead(t *testing.T) {
	f := &adoptFakeDaemon{
		beadType:   "task",
		beadFields: map[string]any{},
	}
	err := runAdopt(t, f, "gd-realgardener")
	if err == nil || !strings.Contains(err.Error(), "not an agent") {
		t.Fatalf("expected non-agent rejection, got %v", err)
	}
	if f.patchedFields != nil {
		t.Fatal("must not patch a non-agent bead")
	}
}

func TestAgentAdopt_AlreadyOwnedIsNoop(t *testing.T) {
	f := &adoptFakeDaemon{
		beadType:   "agent",
		beadLabels: []string{"project:gardenio"},
		beadFields: map[string]any{"gardener_id": "gd-realgardener"},
	}
	if err := runAdopt(t, f, "gd-realgardener"); err != nil {
		t.Fatalf("adopt failed: %v", err)
	}
	if f.patchedFields != nil {
		t.Fatal("already-owned agent must not be re-patched")
	}
	if len(f.comments) != 0 {
		t.Fatalf("already-owned no-op must not comment, got %v", f.comments)
	}
}

func TestAgentAdopt_EmptyGardenerRejected(t *testing.T) {
	f := &adoptFakeDaemon{beadType: "agent", beadFields: map[string]any{}}
	err := runAdopt(t, f, "   ")
	if err == nil || !strings.Contains(err.Error(), "--gardener is required") {
		t.Fatalf("expected empty-gardener rejection, got %v", err)
	}
}
