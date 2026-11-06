package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// setupAdviceForDaemon points the global daemon at a fake server whose advice
// catalog exercises the --for delivery matcher: a global bead, a project+role
// scoped bead, a different-role bead, and a different-project bead. The
// agents-by-name endpoint 404s so EnrichAgentSubscriptions fails open (the
// path a bare-role or project/role delivery-sim naturally takes when no live
// agent bead exists for the context).
func setupAdviceForDaemon(t *testing.T) {
	t.Helper()

	type wireBead struct {
		ID     string   `json:"id"`
		Title  string   `json:"title"`
		Type   string   `json:"type"`
		Status string   `json:"status"`
		Labels []string `json:"labels"`
	}
	wire := []wireBead{
		{ID: "gd-global", Title: "global advice", Type: "advice", Status: "open", Labels: []string{"global"}},
		{ID: "gd-mbaker", Title: "mbaker-assistant personality", Type: "advice", Status: "open",
			Labels: []string{"g0:project:monorepo", "g0:role:mbaker-assistant"}},
		{ID: "gd-crewrole", Title: "crew role advice", Type: "advice", Status: "open",
			Labels: []string{"role:crew"}},
		{ID: "gd-otherproj", Title: "other project+role", Type: "advice", Status: "open",
			Labels: []string{"g0:project:gardenio", "g0:role:mbaker-assistant"}},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/beads", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"beads": wire, "total": len(wire)})
	})
	// Enrichment lookup: no live agent bead for these contexts → 404 → fail open.
	mux.HandleFunc("/v1/agents-by-name/", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatalf("beadsapi.New: %v", err)
	}
	old := daemon
	daemon = c
	t.Cleanup(func() { daemon = old })

	adviceListCmd.SetContext(context.Background())
	t.Cleanup(func() {
		_ = adviceListCmd.Flags().Set("for", "")
		_ = adviceListCmd.Flags().Set("show-undelivered", "false")
		jsonOutput = false
	})
}

// TestAdviceListFor_DeliverySimulation is the regression guard for
// ka-0000002346: `gb advice list --for <context>` must run the argument through
// the REAL delivery matcher, not append it verbatim as a label. The old code
// did `labels = append(labels, forAgent)`, so a role or agent-ID argument (never
// a literal advice label) matched nothing and always printed "No advice beads
// found" — which also silently broke the `gb advice list --for <agentID>`
// command the prime truncation footer advertises.
func TestAdviceListFor_DeliverySimulation(t *testing.T) {
	t.Run("project/role agent-ID form matches the project+role bead", func(t *testing.T) {
		setupAdviceForDaemon(t)
		if err := adviceListCmd.Flags().Set("for", "monorepo/mbaker-assistants"); err != nil {
			t.Fatal(err)
		}
		out := string(captureStdout(t, func() error {
			return adviceListCmd.RunE(adviceListCmd, nil)
		}))
		if !strings.Contains(out, "gd-mbaker") {
			t.Errorf("--for monorepo/mbaker-assistants did not surface the project+role bead (the ka-0000002346 bug):\n%s", out)
		}
		if !strings.Contains(out, "gd-global") {
			t.Errorf("--for should still deliver global advice:\n%s", out)
		}
		// A different project's same-role bead must NOT match (project gate).
		if strings.Contains(out, "gd-otherproj") {
			t.Errorf("--for monorepo/... leaked gardenio-scoped advice gd-otherproj:\n%s", out)
		}
	})

	t.Run("bare role matches global + role, not project-scoped", func(t *testing.T) {
		setupAdviceForDaemon(t)
		if err := adviceListCmd.Flags().Set("for", "crew"); err != nil {
			t.Fatal(err)
		}
		out := string(captureStdout(t, func() error {
			return adviceListCmd.RunE(adviceListCmd, nil)
		}))
		if !strings.Contains(out, "gd-crewrole") {
			t.Errorf("--for crew did not surface role:crew advice:\n%s", out)
		}
		if !strings.Contains(out, "gd-global") {
			t.Errorf("--for crew did not surface global advice:\n%s", out)
		}
		// mbaker-assistant / project-scoped beads must not match a bare crew.
		if strings.Contains(out, "gd-mbaker") {
			t.Errorf("--for crew leaked mbaker-assistant advice:\n%s", out)
		}
	})

	t.Run("legacy raw-label behavior is gone", func(t *testing.T) {
		setupAdviceForDaemon(t)
		// "mbaker-assistant" is a role token, NOT a literal advice label. The old
		// code appended it as a label and matched nothing; the fix resolves it to
		// role:mbaker-assistant subscriptions and matches the role's advice.
		if err := adviceListCmd.Flags().Set("for", "mbaker-assistant"); err != nil {
			t.Fatal(err)
		}
		out := string(captureStdout(t, func() error {
			return adviceListCmd.RunE(adviceListCmd, nil)
		}))
		if strings.Contains(out, "No advice beads found") {
			t.Errorf("--for mbaker-assistant still returns nothing (regression to the ka-0000002346 bug):\n%s", out)
		}
		if !strings.Contains(out, "gd-global") {
			t.Errorf("--for mbaker-assistant should at least deliver global advice:\n%s", out)
		}
	})
}
