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

// TestDecisionCreate_StampsOwnerSeatAndGardener proves the durable-lane ownership
// stamp (ka-0000005031): when the creating session has a resolvable agent bead
// carrying a seat_key + gardener_id, `gb decision create` reads them off that
// agent bead and writes owner_seat / owner_gardener onto the new decision. These
// are the durable keys — created_by is only the ephemeral agent-instance name
// that changes on every respawn, so ownership must not be inferred from it.
func TestDecisionCreate_StampsOwnerSeatAndGardener(t *testing.T) {
	t.Setenv("KD_AGENT_ID", "gd-agent-lane")

	var createFields map[string]json.RawMessage
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/beads/gd-agent-lane" && r.Method == http.MethodGet:
			// The spawning agent bead: carries the durable lane keys.
			w.WriteHeader(200)
			json.NewEncoder(w).Encode(map[string]any{
				"id":     "gd-agent-lane",
				"type":   "agent",
				"status": "open",
				"fields": map[string]string{
					"seat_key":    "gardenio/crew/gardenio-stamp-durable-lane",
					"gardener_id": "gd-gardener-real",
				},
			})
		case r.URL.Path == "/v1/beads" && r.Method == http.MethodPost:
			var req struct {
				Fields json.RawMessage `json:"fields"`
			}
			json.NewDecoder(r.Body).Decode(&req)
			json.Unmarshal(req.Fields, &createFields)
			w.WriteHeader(201)
			json.NewEncoder(w).Encode(map[string]string{"id": "gd-dec-owner"})
		default:
			// Empty list for the dedup check (GET /v1/beads).
			w.WriteHeader(200)
			json.NewEncoder(w).Encode(map[string]any{"beads": []any{}, "total": 0})
		}
	}))
	defer srv.Close()

	oldDaemon := daemon
	daemon, _ = beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	defer func() { daemon = oldDaemon }()

	cmd := decisionCreateCmd
	cmd.SetContext(context.Background())
	cmd.Flags().Set("prompt", "owner-stamp decision")
	cmd.Flags().Set("option", "a:Option A:report")
	cmd.Flags().Set("no-wait", "true")
	defer clearDecisionCreateFlags(cmd)

	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := string(createFields["owner_seat"]); got != `"gardenio/crew/gardenio-stamp-durable-lane"` {
		t.Errorf("owner_seat = %s, want the agent bead seat_key", got)
	}
	if got := string(createFields["owner_gardener"]); got != `"gd-gardener-real"` {
		t.Errorf("owner_gardener = %s, want the agent bead gardener_id", got)
	}
}

// TestDecisionCreate_OwnerGardenerDerivedWhenUnset proves that a legacy agent
// bead carrying no explicit gardener_id still yields a non-empty owner_gardener:
// DeriveGardenerID resolves the same synthetic owner the at-spawn stamp would
// have assigned (here synthetic-gardener:<spawn_source>), so ownership is never
// left blank when the seat_key is present.
func TestDecisionCreate_OwnerGardenerDerivedWhenUnset(t *testing.T) {
	t.Setenv("KD_AGENT_ID", "gd-agent-legacy")

	var createFields map[string]json.RawMessage
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/beads/gd-agent-legacy" && r.Method == http.MethodGet:
			w.WriteHeader(200)
			json.NewEncoder(w).Encode(map[string]any{
				"id":     "gd-agent-legacy",
				"type":   "agent",
				"status": "open",
				"fields": map[string]string{
					"seat_key":     "gardenio/crew/legacy-lane",
					"spawn_source": "slack-thread",
				},
			})
		case r.URL.Path == "/v1/beads" && r.Method == http.MethodPost:
			var req struct {
				Fields json.RawMessage `json:"fields"`
			}
			json.NewDecoder(r.Body).Decode(&req)
			json.Unmarshal(req.Fields, &createFields)
			w.WriteHeader(201)
			json.NewEncoder(w).Encode(map[string]string{"id": "gd-dec-legacy"})
		default:
			w.WriteHeader(200)
			json.NewEncoder(w).Encode(map[string]any{"beads": []any{}, "total": 0})
		}
	}))
	defer srv.Close()

	oldDaemon := daemon
	daemon, _ = beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	defer func() { daemon = oldDaemon }()

	cmd := decisionCreateCmd
	cmd.SetContext(context.Background())
	cmd.Flags().Set("prompt", "legacy owner-stamp decision")
	cmd.Flags().Set("option", "a:Option A:report")
	cmd.Flags().Set("no-wait", "true")
	defer clearDecisionCreateFlags(cmd)

	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := string(createFields["owner_seat"]); got != `"gardenio/crew/legacy-lane"` {
		t.Errorf("owner_seat = %s, want the agent bead seat_key", got)
	}
	if got := string(createFields["owner_gardener"]); got != `"synthetic-gardener:slack-thread"` {
		t.Errorf("owner_gardener = %s, want the source-derived synthetic gardener", got)
	}
}

// TestDecisionCreate_OwnerStampBestEffortOnLookupFailure proves the stamp is
// best-effort: when the agent bead cannot be fetched, the decision is still
// created and simply carries no owner fields rather than failing.
func TestDecisionCreate_OwnerStampBestEffortOnLookupFailure(t *testing.T) {
	t.Setenv("KD_AGENT_ID", "gd-agent-missing")

	var createFields map[string]json.RawMessage
	created := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/v1/beads/") && r.Method == http.MethodGet:
			// Agent bead lookup fails.
			http.NotFound(w, r)
		case r.URL.Path == "/v1/beads" && r.Method == http.MethodPost:
			created = true
			var req struct {
				Fields json.RawMessage `json:"fields"`
			}
			json.NewDecoder(r.Body).Decode(&req)
			json.Unmarshal(req.Fields, &createFields)
			w.WriteHeader(201)
			json.NewEncoder(w).Encode(map[string]string{"id": "gd-dec-nofail"})
		default:
			w.WriteHeader(200)
			json.NewEncoder(w).Encode(map[string]any{"beads": []any{}, "total": 0})
		}
	}))
	defer srv.Close()

	oldDaemon := daemon
	daemon, _ = beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	defer func() { daemon = oldDaemon }()

	cmd := decisionCreateCmd
	cmd.SetContext(context.Background())
	cmd.Flags().Set("prompt", "best-effort owner decision")
	cmd.Flags().Set("option", "a:Option A:report")
	cmd.Flags().Set("no-wait", "true")
	defer clearDecisionCreateFlags(cmd)

	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatalf("create must not fail on owner-lookup failure: %v", err)
	}
	if !created {
		t.Fatal("decision was not created")
	}
	if _, ok := createFields["owner_seat"]; ok {
		t.Errorf("owner_seat should be absent when the agent bead is unresolvable, got %s", createFields["owner_seat"])
	}
}
