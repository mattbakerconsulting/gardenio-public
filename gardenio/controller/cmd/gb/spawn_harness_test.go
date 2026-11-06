package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// --harness is validated before any daemon call, so a typo fails locally with
// the allowed values (gd-6SezxhSEEk).
func TestRunSpawn_BadHarnessRejectedBeforeDaemon(t *testing.T) {
	t.Setenv("BOAT_PROJECT", "gardenio")
	failOnSpawnDaemon(t)

	_, _, err := newSpawnTestCmd(t, []string{"--role", "crew", "--harness", "gpt"})
	if err == nil || !strings.Contains(err.Error(), `unknown harness "gpt"`) {
		t.Fatalf("err = %v, want unknown harness error", err)
	}
}

// --harness codex stamps harness=codex on the new agent bead (the agent-level,
// highest layer of the controller's resolution); omitting it stamps nothing.
func TestRunSpawn_HarnessStampedOnAgentBead(t *testing.T) {
	t.Setenv("BOAT_PROJECT", "gardenio")
	for _, tc := range []struct {
		name string
		argv []string
		want string
	}{
		{"codex", []string{"--role", "crew", "--harness", "codex"}, "codex"},
		{"unset", []string{"--role", "crew"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got string
			var sawCreate bool
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/agents-by-name/"):
					http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
				case r.Method == http.MethodGet && r.URL.Path == "/v1/beads":
					_ = json.NewEncoder(w).Encode(map[string]any{"beads": []any{}})
				case r.URL.Path == "/v1/beads" && r.Method == http.MethodPost:
					sawCreate = true
					var body struct {
						Fields json.RawMessage `json:"fields"`
					}
					_ = json.NewDecoder(r.Body).Decode(&body)
					var fields map[string]string
					_ = json.Unmarshal(body.Fields, &fields)
					got = fields["harness"]
					_ = json.NewEncoder(w).Encode(map[string]string{"id": "gd-HARNESS01"})
				default:
					_ = json.NewEncoder(w).Encode(map[string]string{})
				}
			}))
			t.Cleanup(srv.Close)
			c, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
			if err != nil {
				t.Fatal(err)
			}
			oldDaemon := daemon
			daemon = c
			t.Cleanup(func() { daemon = oldDaemon })

			captureSpawnStdout(t, func() {
				if _, out, err := newSpawnTestCmd(t, tc.argv); err != nil {
					t.Fatalf("spawn: %v\n%s", err, out.String())
				}
			})
			if !sawCreate {
				t.Fatal("spawn never reached the create POST")
			}
			if got != tc.want {
				t.Errorf("created bead fields.harness = %q, want %q", got, tc.want)
			}
		})
	}
}
