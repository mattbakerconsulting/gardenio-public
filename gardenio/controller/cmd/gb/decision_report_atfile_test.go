package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// TestDecisionReport_ContentAtFileStoresBody is the ka-0000003861 regression
// guard: `gb decision report <id> --content @file` must store the file's BODY
// in the report bead's content field, not the literal "@/path" pointer. Before
// the fix the pointer was persisted verbatim, silently shipping an empty
// artifact with a zero exit and a "Report submitted" success line.
func TestDecisionReport_ContentAtFileStoresBody(t *testing.T) {
	dir := t.TempDir()
	body := "## Root cause\n\nThe flag was stored verbatim.\n\n## Fix\n\n@file expansion.\n"
	path := filepath.Join(dir, "report.md")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write report body: %v", err)
	}

	var createdFields map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/beads" && r.Method == http.MethodPost:
			var req beadsapi.CreateBeadRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			_ = json.Unmarshal(req.Fields, &createdFields)
			w.WriteHeader(201)
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "gd-report-1"})
		case strings.HasSuffix(r.URL.Path, "/dependencies") && r.Method == http.MethodGet:
			// resolveOriginatingTask: decision has no parent-child edge.
			w.WriteHeader(200)
			_ = json.NewEncoder(w).Encode(map[string]any{"dependencies": []any{}})
		case strings.HasSuffix(r.URL.Path, "/dependencies") && r.Method == http.MethodPost:
			w.WriteHeader(201)
			_ = json.NewEncoder(w).Encode(map[string]string{})
		case strings.HasSuffix(r.URL.Path, "/close") && r.Method == http.MethodPost:
			w.WriteHeader(200)
			_ = json.NewEncoder(w).Encode(map[string]string{})
		case strings.HasPrefix(r.URL.Path, "/v1/beads/") && r.Method == http.MethodGet:
			// GetBead on the decision: a decision requiring no artifact, so the
			// artifact_status advance + gate paths stay out of the way.
			w.WriteHeader(200)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "gd-dec-1", "type": "decision", "status": "open"})
		default:
			w.WriteHeader(200)
			_ = json.NewEncoder(w).Encode(map[string]any{})
		}
	}))
	defer srv.Close()

	oldDaemon := daemon
	daemon, _ = beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	defer func() { daemon = oldDaemon }()

	// KD_AGENT_ID unset → the gate-satisfy branch is skipped, keeping the test
	// focused on the create path. Guard against a leaked env from the harness.
	t.Setenv("KD_AGENT_ID", "")
	_ = os.Unsetenv("KD_AGENT_ID")

	cmd := decisionReportCmd
	cmd.SetContext(context.Background())
	if err := cmd.Flags().Set("content", "@"+path); err != nil {
		t.Fatalf("set --content: %v", err)
	}
	defer func() { _ = cmd.Flags().Set("content", "") }()

	if err := cmd.RunE(cmd, []string{"gd-dec-1"}); err != nil {
		t.Fatalf("RunE: %v", err)
	}

	if createdFields == nil {
		t.Fatal("no report bead was created")
	}
	got, _ := createdFields["content"].(string)
	if got != body {
		t.Errorf("report content = %q, want the file body %q (not the literal @path)", got, body)
	}
	if strings.HasPrefix(got, "@") {
		t.Errorf("report content is a literal pointer %q — @file expansion did not run", got)
	}
}

// TestDecisionReport_ContentMissingAtFileFailsLoud proves a typo'd/missing path
// fails LOUD instead of shipping the dangling pointer as the artifact body.
func TestDecisionReport_ContentMissingAtFileFailsLoud(t *testing.T) {
	dir := t.TempDir()

	created := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/beads" && r.Method == http.MethodPost {
			created = true
		}
		w.WriteHeader(200)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "gd-x", "type": "decision", "status": "open"})
	}))
	defer srv.Close()

	oldDaemon := daemon
	daemon, _ = beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	defer func() { daemon = oldDaemon }()

	cmd := decisionReportCmd
	cmd.SetContext(context.Background())
	if err := cmd.Flags().Set("content", "@"+filepath.Join(dir, "gone.md")); err != nil {
		t.Fatalf("set --content: %v", err)
	}
	defer func() { _ = cmd.Flags().Set("content", "") }()

	err := cmd.RunE(cmd, []string{"gd-x"})
	if err == nil {
		t.Fatal("expected an error for --content @missing-file, got nil")
	}
	if !strings.Contains(err.Error(), "--content") {
		t.Errorf("error %q should name the --content flag", err.Error())
	}
	if created {
		t.Error("report bead must not be created when --content @file fails to read")
	}
}
