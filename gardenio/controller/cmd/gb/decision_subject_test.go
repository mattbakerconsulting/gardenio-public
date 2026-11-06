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

// TestDecisionCreate_WritesRelatedEdgeToSubject proves the decision→subject
// link (ka-0000001617): create with --subject must POST a related-typed
// dependency ON the new decision, pointing at the subject trigger — so a
// resolved revival-review decision is walkable back to the gardener it reviewed.
func TestDecisionCreate_WritesRelatedEdgeToSubject(t *testing.T) {
	var depPath string
	var depBody map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/beads" && r.Method == http.MethodPost:
			w.WriteHeader(201)
			json.NewEncoder(w).Encode(map[string]string{"id": "gd-dec-subject"})
		case strings.HasSuffix(r.URL.Path, "/dependencies") && r.Method == http.MethodPost:
			depPath = r.URL.Path
			json.NewDecoder(r.Body).Decode(&depBody)
			w.WriteHeader(201)
			json.NewEncoder(w).Encode(map[string]string{})
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
	cmd.Flags().Set("prompt", "reaper-revival-review for gardener schedule gd-cap")
	cmd.Flags().Set("option", "revive:Revive:diff-summary")
	cmd.Flags().Set("no-wait", "true")
	cmd.Flags().Set("subject", "gd-cap")
	defer clearDecisionCreateFlags(cmd)

	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if depPath != "/v1/beads/gd-dec-subject/dependencies" {
		t.Errorf("related edge posted to %q, want it on the new decision", depPath)
	}
	if depBody["depends_on_id"] != "gd-cap" {
		t.Errorf("depends_on_id = %q, want the subject trigger id gd-cap", depBody["depends_on_id"])
	}
	if depBody["type"] != "related" {
		t.Errorf("dependency type = %q, want related", depBody["type"])
	}
}

// TestDecisionCreate_StampsSubjectField proves --subject is persisted as the
// machine-readable subject_trigger_id field on the decision bead, so C3's
// dismissal-labeling and downstream cycles have a direct field to read even if
// the graph edge is later severed.
func TestDecisionCreate_StampsSubjectField(t *testing.T) {
	var createFields map[string]json.RawMessage
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/beads" && r.Method == http.MethodPost:
			var req struct {
				Fields json.RawMessage `json:"fields"`
			}
			json.NewDecoder(r.Body).Decode(&req)
			json.Unmarshal(req.Fields, &createFields)
			w.WriteHeader(201)
			json.NewEncoder(w).Encode(map[string]string{"id": "gd-dec-subj-field"})
		case strings.HasSuffix(r.URL.Path, "/dependencies") && r.Method == http.MethodPost:
			w.WriteHeader(201)
			json.NewEncoder(w).Encode(map[string]string{})
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
	cmd.Flags().Set("prompt", "subject field-stamp decision")
	cmd.Flags().Set("option", "a:Option A:report")
	cmd.Flags().Set("no-wait", "true")
	cmd.Flags().Set("subject", "gd-subj")
	defer clearDecisionCreateFlags(cmd)

	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := string(createFields["subject_trigger_id"]); got != `"gd-subj"` {
		t.Errorf("subject_trigger_id field = %s, want \"gd-subj\"", got)
	}
}

// TestDecisionCreate_NoSubjectEdgeWithoutFlag proves the flag stays optional:
// no --subject → no subject_trigger_id field and no dependency call, so the
// existing behaviour is byte-for-byte unchanged when the flag is omitted.
func TestDecisionCreate_NoSubjectEdgeWithoutFlag(t *testing.T) {
	depCalls := 0
	var createFields map[string]json.RawMessage
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/beads" && r.Method == http.MethodPost:
			var req struct {
				Fields json.RawMessage `json:"fields"`
			}
			json.NewDecoder(r.Body).Decode(&req)
			json.Unmarshal(req.Fields, &createFields)
			w.WriteHeader(201)
			json.NewEncoder(w).Encode(map[string]string{"id": "gd-dec-nosubj"})
		case strings.HasSuffix(r.URL.Path, "/dependencies") && r.Method == http.MethodPost:
			depCalls++
			w.WriteHeader(201)
			json.NewEncoder(w).Encode(map[string]string{})
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
	cmd.Flags().Set("prompt", "no subject decision")
	cmd.Flags().Set("option", "a:Option A:report")
	cmd.Flags().Set("no-wait", "true")
	defer clearDecisionCreateFlags(cmd)

	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if depCalls != 0 {
		t.Errorf("expected no dependency calls without --subject, got %d", depCalls)
	}
	if _, ok := createFields["subject_trigger_id"]; ok {
		t.Errorf("subject_trigger_id should be absent without --subject, got %s", createFields["subject_trigger_id"])
	}
}
