package poolmanager

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"log/slog"

	"gardenio/controller/internal/beadsapi"
)

// patchCapturingDaemon serves the pool's bead list/get and records the JSON body
// of the FIRST PATCH (the atomic agent_state flip in AssignPrewarmed), so a test
// can assert exactly which fields were stamped.
func patchCapturingDaemon(t *testing.T, agents []map[string]any, gotBody *map[string]any, mu *sync.Mutex) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/beads":
			beads := make([]map[string]any, len(agents))
			for i, a := range agents {
				beads[i] = map[string]any{
					"id": a["id"], "title": a["title"], "type": "agent",
					"status": "open", "fields": a["fields"],
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"beads": beads, "total": len(beads)})
		case r.Method == http.MethodGet:
			for _, a := range agents {
				if "/v1/beads/"+a["id"].(string) == r.URL.Path {
					_ = json.NewEncoder(w).Encode(map[string]any{
						"id": a["id"], "title": a["title"], "type": "agent",
						"status": "open", "fields": a["fields"],
					})
					return
				}
			}
			w.WriteHeader(http.StatusNotFound)
		case r.Method == http.MethodPatch:
			body, _ := io.ReadAll(r.Body)
			var parsed map[string]any
			_ = json.Unmarshal(body, &parsed)
			mu.Lock()
			if *gotBody == nil {
				// The daemon UpdateBeadFields PATCH nests the map under "fields".
				if f, ok := parsed["fields"].(map[string]any); ok {
					*gotBody = f
				} else {
					*gotBody = parsed
				}
			}
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
}

func warmResumeTestAgents() []map[string]any {
	return []map[string]any{
		{
			"id":    "gd-prewarm-wr",
			"title": "prewarmed-wr",
			"fields": map[string]string{
				"agent": "prewarmed-wr", "agent_state": "prewarmed",
				"mode": "crew", "role": "thread", "project": "gardenio",
			},
		},
	}
}

// TestAssignPrewarmed_StampsWarmResumeS3Key verifies the warm-pool coop-resume
// graft (ka-0000000983): when AssignRequest.ResumeS3Key is set, the assign PATCH
// stamps fields.warm_resume_s3_key so the assigned pod resumes that transcript.
func TestAssignPrewarmed_StampsWarmResumeS3Key(t *testing.T) {
	var gotBody map[string]any
	var mu sync.Mutex
	srv := patchCapturingDaemon(t, warmResumeTestAgents(), &gotBody, &mu)
	defer srv.Close()

	daemon, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	pm := New(daemon, testConfig(), slog.Default())

	if _, err := pm.AssignPrewarmed(context.Background(), AssignRequest{
		Channel:     "C-test",
		ThreadTS:    "1234.5678",
		Project:     "gardenio",
		Role:        "thread",
		ResumeS3Key: "gd-prior-transcript",
	}); err != nil {
		t.Fatalf("AssignPrewarmed returned error: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if gotBody == nil {
		t.Fatal("no PATCH body captured — AssignPrewarmed did not write to the daemon")
	}
	if got := gotBody["warm_resume_s3_key"]; got != "gd-prior-transcript" {
		t.Errorf("warm_resume_s3_key = %v, want %q", got, "gd-prior-transcript")
	}
	// The agent_state flip must still happen in the same PATCH.
	if got := gotBody["agent_state"]; got != "assigning" {
		t.Errorf("agent_state = %v, want %q", got, "assigning")
	}
}

// TestAssignPrewarmed_NoWarmResumeKeyByDefault verifies the field is inert when
// ResumeS3Key is empty — a plain warm assignment must not stamp
// warm_resume_s3_key, so the pod starts a fresh warm session (today's behavior).
func TestAssignPrewarmed_NoWarmResumeKeyByDefault(t *testing.T) {
	var gotBody map[string]any
	var mu sync.Mutex
	srv := patchCapturingDaemon(t, warmResumeTestAgents(), &gotBody, &mu)
	defer srv.Close()

	daemon, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	pm := New(daemon, testConfig(), slog.Default())

	if _, err := pm.AssignPrewarmed(context.Background(), AssignRequest{
		Channel:  "C-test",
		ThreadTS: "1234.5678",
		Project:  "gardenio",
		Role:     "thread",
		// ResumeS3Key intentionally empty.
	}); err != nil {
		t.Fatalf("AssignPrewarmed returned error: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if gotBody == nil {
		t.Fatal("no PATCH body captured — AssignPrewarmed did not write to the daemon")
	}
	if _, present := gotBody["warm_resume_s3_key"]; present {
		t.Errorf("warm_resume_s3_key must be absent on a plain assign, got %v", gotBody["warm_resume_s3_key"])
	}
}
