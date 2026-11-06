package beadsapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// --- UpdateBeadFields tests ---

func TestUpdateBeadFields_MergesFields(t *testing.T) {
	var putBody map[string]json.RawMessage
	callCount := 0

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		switch {
		case r.Method == http.MethodGet:
			// Return bead with existing fields.
			bead := beadJSON{
				ID:     "bd-merge",
				Fields: json.RawMessage(`{"existing":"keep","overwrite":"old"}`),
			}
			_ = json.NewEncoder(w).Encode(bead)

		case r.Method == http.MethodPatch:
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &putBody)
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	err := c.UpdateBeadFields(context.Background(), "bd-merge", map[string]string{
		"overwrite": "new",
		"added":     "fresh",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if callCount != 2 {
		t.Fatalf("expected 2 HTTP calls (GET + PATCH), got %d", callCount)
	}

	// Verify the PATCH body contains ONLY the changed keys. The server applies
	// additive field-merge, so untouched fields ("existing") must be omitted —
	// re-sending them would clobber concurrent writers (ka-0000004616).
	fieldsRaw, ok := putBody["fields"]
	if !ok {
		t.Fatal("expected 'fields' key in PATCH body")
	}
	var merged map[string]string
	if err := json.Unmarshal(fieldsRaw, &merged); err != nil {
		t.Fatalf("failed to unmarshal merged fields: %v", err)
	}

	if _, present := merged["existing"]; present {
		t.Errorf("expected untouched field 'existing' to be OMITTED from patch, got %q", merged["existing"])
	}
	if merged["overwrite"] != "new" {
		t.Errorf("expected overwritten field updated, got %s", merged["overwrite"])
	}
	if merged["added"] != "fresh" {
		t.Errorf("expected new field added, got %s", merged["added"])
	}
}

func TestUpdateBeadFields_HandlesNilExistingFields(t *testing.T) {
	var putBody map[string]json.RawMessage

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet:
			// Return bead with no fields (nil Fields).
			bead := beadJSON{ID: "bd-nil"}
			_ = json.NewEncoder(w).Encode(bead)

		case r.Method == http.MethodPatch:
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &putBody)
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	err := c.UpdateBeadFields(context.Background(), "bd-nil", map[string]string{
		"new_field": "value",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	fieldsRaw := putBody["fields"]
	var merged map[string]string
	if err := json.Unmarshal(fieldsRaw, &merged); err != nil {
		t.Fatalf("failed to unmarshal fields: %v", err)
	}
	if merged["new_field"] != "value" {
		t.Errorf("expected new_field=value, got %s", merged["new_field"])
	}
}

func TestUpdateBeadFields_PreservesExistingTypes(t *testing.T) {
	var patchBody []byte

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet:
			// Server returns mr_merged as boolean true and stop_requested as string.
			bead := beadJSON{
				ID:     "bd-types",
				Fields: json.RawMessage(`{"mr_merged":true,"stop_requested":"false","existing":"keep"}`),
			}
			_ = json.NewEncoder(w).Encode(bead)

		case r.Method == http.MethodPatch:
			patchBody, _ = io.ReadAll(r.Body)
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	err := c.UpdateBeadFields(context.Background(), "bd-types", map[string]string{
		"mr_merged":      "false",  // existing boolean — should stay boolean
		"stop_requested": "true",   // existing string — should stay string
		"new_field":      "true",   // new field — should be string (no coercion)
		"mr_state":       "merged", // new field — should be string
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var body map[string]json.RawMessage
	if err := json.Unmarshal(patchBody, &body); err != nil {
		t.Fatalf("failed to unmarshal PATCH body: %v", err)
	}

	var fields map[string]any
	if err := json.Unmarshal(body["fields"], &fields); err != nil {
		t.Fatalf("failed to unmarshal fields: %v", err)
	}

	// mr_merged was boolean in existing — update should preserve boolean type.
	if v, ok := fields["mr_merged"].(bool); !ok || v {
		t.Errorf("expected mr_merged=false (bool), got %v (%T)", fields["mr_merged"], fields["mr_merged"])
	}
	// stop_requested was string in existing — should stay string.
	if v, ok := fields["stop_requested"].(string); !ok || v != "true" {
		t.Errorf("expected stop_requested=\"true\" (string), got %v (%T)", fields["stop_requested"], fields["stop_requested"])
	}
	// new_field is new — should be string (no coercion for new fields).
	if v, ok := fields["new_field"].(string); !ok || v != "true" {
		t.Errorf("expected new_field=\"true\" (string), got %v (%T)", fields["new_field"], fields["new_field"])
	}
	// mr_state is new — should be string.
	if v, ok := fields["mr_state"].(string); !ok || v != "merged" {
		t.Errorf("expected mr_state=\"merged\" (string), got %v (%T)", fields["mr_state"], fields["mr_state"])
	}
	// existing was NOT in the update set — it must be omitted from the partial
	// patch so the server's additive merge leaves it (and concurrent writes) alone.
	if _, present := fields["existing"]; present {
		t.Errorf("expected untouched field 'existing' to be OMITTED from patch, got %v", fields["existing"])
	}
}

func TestUpdateBeadFields_PreservesNumericTypes(t *testing.T) {
	var patchBody []byte

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet:
			// Server returns rotation_count as a number and priority as a number.
			bead := beadJSON{
				ID:     "bd-nums",
				Fields: json.RawMessage(`{"rotation_count":3,"priority":2,"name":"test"}`),
			}
			_ = json.NewEncoder(w).Encode(bead)

		case r.Method == http.MethodPatch:
			patchBody, _ = io.ReadAll(r.Body)
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	err := c.UpdateBeadFields(context.Background(), "bd-nums", map[string]string{
		"rotation_count": "4",       // existing number — should stay number
		"name":           "updated", // existing string — should stay string
		"new_count":      "10",      // new field — should be string (no coercion)
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var body map[string]json.RawMessage
	if err := json.Unmarshal(patchBody, &body); err != nil {
		t.Fatalf("failed to unmarshal PATCH body: %v", err)
	}

	var fields map[string]any
	if err := json.Unmarshal(body["fields"], &fields); err != nil {
		t.Fatalf("failed to unmarshal fields: %v", err)
	}

	// rotation_count was numeric in existing — update should preserve numeric type.
	if v, ok := fields["rotation_count"].(float64); !ok || v != 4 {
		t.Errorf("expected rotation_count=4 (number), got %v (%T)", fields["rotation_count"], fields["rotation_count"])
	}
	// priority was NOT in the update set — must be omitted from the partial patch.
	if _, present := fields["priority"]; present {
		t.Errorf("expected untouched field 'priority' to be OMITTED from patch, got %v", fields["priority"])
	}
	// name was string in existing — should stay string.
	if v, ok := fields["name"].(string); !ok || v != "updated" {
		t.Errorf("expected name=\"updated\" (string), got %v (%T)", fields["name"], fields["name"])
	}
	// new_count is new — should be string (no coercion for new fields).
	if v, ok := fields["new_count"].(string); !ok || v != "10" {
		t.Errorf("expected new_count=\"10\" (string), got %v (%T)", fields["new_count"], fields["new_count"])
	}
}

// TestUpdateBeadFields_NeverSeededNumericSentAsString is the regression guard
// for ka-0000008019. UpdateBeadFields only coerces a string value to a JSON
// number when the field is ALREADY present with a numeric type. For a
// never-seeded field (nothing in the GET response) there is no prior type, so
// the value goes on the wire as a JSON STRING. When the daemon schema declares
// that field integer/float (as it does for context_tokens / live_cost_usd),
// the string fails validation ("must be an integer") and the whole PATCH is
// rejected — which is why live usage never landed on any agent bead. Callers
// writing numeric first-writes MUST use UpdateBeadFieldsTyped instead; this
// test pins the string-map footgun so the reason is discoverable.
func TestUpdateBeadFields_NeverSeededNumericSentAsString(t *testing.T) {
	var patchBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet:
			// Bead has NO context_tokens yet — the first-ever write case.
			bead := beadJSON{ID: "bd-boot", Fields: json.RawMessage(`{"agent":"a1"}`)}
			_ = json.NewEncoder(w).Encode(bead)
		case r.Method == http.MethodPatch:
			patchBody, _ = io.ReadAll(r.Body)
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	if err := c.UpdateBeadFields(context.Background(), "bd-boot", map[string]string{
		"context_tokens": "86243",
	}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var body map[string]json.RawMessage
	if err := json.Unmarshal(patchBody, &body); err != nil {
		t.Fatalf("failed to unmarshal PATCH body: %v", err)
	}
	var fields map[string]any
	if err := json.Unmarshal(body["fields"], &fields); err != nil {
		t.Fatalf("failed to unmarshal fields: %v", err)
	}
	// The footgun: a never-seeded numeric field goes out as a STRING, which a
	// schema-validating daemon rejects. If this ever changes to a float64, the
	// string-map path became safe for first-writes and reportLiveUsage could use
	// it again — update the comment on reporter.go:reportLiveUsage accordingly.
	if _, isStr := fields["context_tokens"].(string); !isStr {
		t.Errorf("expected never-seeded context_tokens to be sent as a STRING (the bug), got %T (%v)", fields["context_tokens"], fields["context_tokens"])
	}
}

// --- UpdateBeadFieldsTyped tests ---

func TestUpdateBeadFieldsTyped_SendsBooleanProperly(t *testing.T) {
	var patchBody []byte

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet:
			bead := beadJSON{
				ID:     "bd-typed",
				Fields: json.RawMessage(`{"agent":"test-agent","gate_satisfied_by":"ready"}`),
			}
			_ = json.NewEncoder(w).Encode(bead)

		case r.Method == http.MethodPatch:
			patchBody, _ = io.ReadAll(r.Body)
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	err := c.UpdateBeadFieldsTyped(context.Background(), "bd-typed", map[string]any{
		"stop_requested": true,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var body map[string]json.RawMessage
	if err := json.Unmarshal(patchBody, &body); err != nil {
		t.Fatalf("failed to unmarshal PATCH body: %v", err)
	}

	var fields map[string]any
	if err := json.Unmarshal(body["fields"], &fields); err != nil {
		t.Fatalf("failed to unmarshal fields: %v", err)
	}

	// stop_requested should be boolean true, not string "true".
	if v, ok := fields["stop_requested"].(bool); !ok || !v {
		t.Errorf("expected stop_requested=true (bool), got %v (%T)", fields["stop_requested"], fields["stop_requested"])
	}
	// Untouched fields must be OMITTED from the partial patch — the server's
	// additive merge leaves them (and concurrent writes) alone (ka-0000004616).
	if _, present := fields["agent"]; present {
		t.Errorf("expected untouched field 'agent' to be OMITTED from patch, got %v", fields["agent"])
	}
	if _, present := fields["gate_satisfied_by"]; present {
		t.Errorf("expected untouched field 'gate_satisfied_by' to be OMITTED from patch, got %v", fields["gate_satisfied_by"])
	}
}

func TestUpdateBeadFieldsTyped_IntegerRotationCount(t *testing.T) {
	var patchBody []byte

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet:
			// Bead has no rotation_count field yet.
			bead := beadJSON{
				ID:     "bd-account",
				Fields: json.RawMessage(`{"status":"active"}`),
			}
			_ = json.NewEncoder(w).Encode(bead)

		case r.Method == http.MethodPatch:
			patchBody, _ = io.ReadAll(r.Body)
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	err := c.UpdateBeadFieldsTyped(context.Background(), "bd-account", map[string]any{
		"status":         "exhausted",
		"rotation_count": 1, // must be JSON integer, not string
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var body map[string]json.RawMessage
	if err := json.Unmarshal(patchBody, &body); err != nil {
		t.Fatalf("failed to unmarshal PATCH body: %v", err)
	}

	var fields map[string]any
	if err := json.Unmarshal(body["fields"], &fields); err != nil {
		t.Fatalf("failed to unmarshal fields: %v", err)
	}

	// rotation_count must be a JSON number (float64 from json.Unmarshal), not a string.
	if v, ok := fields["rotation_count"].(float64); !ok || v != 1 {
		t.Errorf("expected rotation_count=1 (number), got %v (%T)", fields["rotation_count"], fields["rotation_count"])
	}
	if v, ok := fields["status"].(string); !ok || v != "exhausted" {
		t.Errorf("expected status=\"exhausted\", got %v (%T)", fields["status"], fields["status"])
	}
}

// --- UpdateBeadNotes tests ---

func TestUpdateBeadNotes_SendsCorrectBody(t *testing.T) {
	var gotMethod string
	var gotPath string
	var gotBody map[string]string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	err := c.UpdateBeadNotes(context.Background(), "bd-notes1", "coop_url: http://coop:9090\npod_name: agent-0")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if gotMethod != http.MethodPatch {
		t.Errorf("expected PATCH, got %s", gotMethod)
	}
	if gotPath != "/v1/beads/bd-notes1" {
		t.Errorf("expected path /v1/beads/bd-notes1, got %s", gotPath)
	}
	if gotBody["notes"] != "coop_url: http://coop:9090\npod_name: agent-0" {
		t.Errorf("expected notes body, got %v", gotBody)
	}
}

// --- UpdateAgentState tests ---

func TestUpdateAgentState_SetsFieldViaUpdateBeadFields(t *testing.T) {
	var putBody map[string]json.RawMessage

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet:
			bead := beadJSON{
				ID:     "bd-state1",
				Fields: json.RawMessage(`{"project":"town"}`),
			}
			_ = json.NewEncoder(w).Encode(bead)

		case r.Method == http.MethodPatch:
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &putBody)
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	err := c.UpdateAgentState(context.Background(), "bd-state1", "running")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	fieldsRaw := putBody["fields"]
	var merged map[string]string
	if err := json.Unmarshal(fieldsRaw, &merged); err != nil {
		t.Fatalf("failed to unmarshal fields: %v", err)
	}
	if merged["agent_state"] != "running" {
		t.Errorf("expected agent_state=running, got %s", merged["agent_state"])
	}
	// project was NOT in the update set — must be omitted from the partial patch.
	if _, present := merged["project"]; present {
		t.Errorf("expected untouched field 'project' to be OMITTED from patch, got %s", merged["project"])
	}
}

// --- Error handling tests ---

func TestAPIError_404(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "bead not found"})
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	_, err := c.GetBead(context.Background(), "nonexistent")
	if err == nil {
		t.Fatal("expected error for 404")
	}

	apiErr, ok := err.(*APIError)
	// The error is wrapped by GetBead, so unwrap it.
	if !ok {
		// Check if it's wrapped.
		var inner *APIError
		if unwrapped, ok2 := err.(interface{ Unwrap() error }); ok2 {
			inner, _ = unwrapped.Unwrap().(*APIError)
		}
		if inner == nil {
			t.Fatalf("expected *APIError, got %T: %v", err, err)
		}
		apiErr = inner
	}

	if apiErr.StatusCode != 404 {
		t.Errorf("expected status 404, got %d", apiErr.StatusCode)
	}
	if apiErr.Message != "bead not found" {
		t.Errorf("expected message 'bead not found', got %s", apiErr.Message)
	}
}

func TestAPIError_500WithJSONError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "internal server error"})
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	_, err := c.GetBead(context.Background(), "bd-500")
	if err == nil {
		t.Fatal("expected error for 500")
	}
	if !strings.Contains(err.Error(), "internal server error") {
		t.Errorf("expected error message to contain 'internal server error', got: %v", err)
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("expected error to mention status 500, got: %v", err)
	}
}

func TestAPIError_500WithPlainTextBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("something went wrong"))
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	_, err := c.GetBead(context.Background(), "bd-plain")
	if err == nil {
		t.Fatal("expected error for 500")
	}
	if !strings.Contains(err.Error(), "something went wrong") {
		t.Errorf("expected plain text body in error, got: %v", err)
	}
}

func TestAPIError_ErrorStringFormat(t *testing.T) {
	e := &APIError{StatusCode: 422, Message: "invalid fields"}
	got := e.Error()
	want := "HTTP 422: invalid fields"
	if got != want {
		t.Errorf("expected %q, got %q", want, got)
	}
}

func TestDoJSON_204NoContent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	err := c.UpdateBeadNotes(context.Background(), "bd-204", "notes")
	if err != nil {
		t.Fatalf("204 No Content should not be an error, got: %v", err)
	}
}

func TestListAgentBeads_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("service unavailable"))
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	_, err := c.ListAgentBeads(context.Background())
	if err == nil {
		t.Fatal("expected error for 503")
	}
	if !strings.Contains(err.Error(), "listing agent beads") {
		t.Errorf("expected wrapped error from ListAgentBeads, got: %v", err)
	}
}

func TestUpdateBeadFields_GetFailsPropagatesError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "not found"})
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	err := c.UpdateBeadFields(context.Background(), "bd-missing", map[string]string{"k": "v"})
	if err == nil {
		t.Fatal("expected error when GET fails during field update")
	}
	if !strings.Contains(err.Error(), "reading bead") {
		t.Errorf("expected 'reading bead' in error, got: %v", err)
	}
}

func TestUpdateBeadFields_SkipsNoOpPatch(t *testing.T) {
	patchCalled := false

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet:
			// Return bead with fields that already match the update.
			bead := beadJSON{
				ID:     "bd-noop",
				Fields: json.RawMessage(`{"jira_xlinks":"blocks:PE-100,relates:PE-200"}`),
			}
			_ = json.NewEncoder(w).Encode(bead)

		case r.Method == http.MethodPatch:
			patchCalled = true
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	// Update with the same value that's already stored.
	err := c.UpdateBeadFields(context.Background(), "bd-noop", map[string]string{
		"jira_xlinks": "blocks:PE-100,relates:PE-200",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if patchCalled {
		t.Error("expected PATCH to be skipped when fields are unchanged, but it was called")
	}
}

func TestUpdateBeadFieldsTyped_SkipsNoOpPatch(t *testing.T) {
	patchCalled := false

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet:
			bead := beadJSON{
				ID:     "bd-noop-typed",
				Fields: json.RawMessage(`{"jira_has_comments":true}`),
			}
			_ = json.NewEncoder(w).Encode(bead)

		case r.Method == http.MethodPatch:
			patchCalled = true
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	err := c.UpdateBeadFieldsTyped(context.Background(), "bd-noop-typed", map[string]any{
		"jira_has_comments": true,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if patchCalled {
		t.Error("expected PATCH to be skipped when typed fields are unchanged, but it was called")
	}
}

// TestUpdateBeadFields_OnlyPatchesChangedKeys is the regression guard for the
// trigger-engine lost-update race (ka-0000004616 / ka-0000005761). The trigger
// engine stamps `last_checked` on an *enabled* trigger bead. If it re-sends the
// whole fields blob, a concurrent operator write of `enabled=false` is silently
// clobbered back to true — "trigger re-disabled: expected false, got true". The
// fix sends ONLY the changed key, leaving the server's additive merge to keep
// the operator's concurrent `enabled` write intact.
func TestUpdateBeadFields_OnlyPatchesChangedKeys(t *testing.T) {
	var patchFields map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			// Engine GET'd the bead while it was still enabled=true.
			bead := beadJSON{
				ID:     "bd-trigger",
				Fields: json.RawMessage(`{"enabled":true,"source":"bead-state","action":"create_bead"}`),
			}
			_ = json.NewEncoder(w).Encode(bead)
		case http.MethodPatch:
			body, _ := io.ReadAll(r.Body)
			var parsed map[string]json.RawMessage
			_ = json.Unmarshal(body, &parsed)
			_ = json.Unmarshal(parsed["fields"], &patchFields)
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	// The engine stamps only last_checked.
	err := c.UpdateBeadFields(context.Background(), "bd-trigger", map[string]string{
		"last_checked": "2026-06-14T00:00:00Z",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// The PATCH must carry ONLY last_checked — never enabled/source/action.
	if _, present := patchFields["enabled"]; present {
		t.Errorf("LOST-UPDATE BUG: patch re-sent 'enabled' (would clobber a concurrent disable), fields=%v", patchFields)
	}
	if _, present := patchFields["source"]; present {
		t.Errorf("expected untouched 'source' omitted from patch, fields=%v", patchFields)
	}
	if v, ok := patchFields["last_checked"].(string); !ok || v != "2026-06-14T00:00:00Z" {
		t.Errorf("expected last_checked in patch, got %v", patchFields["last_checked"])
	}
	if len(patchFields) != 1 {
		t.Errorf("expected exactly 1 key in patch (last_checked), got %d: %v", len(patchFields), patchFields)
	}
}

func TestCloseBead_CloseFailsPropagatesError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// POST /close returns 500.
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("boom"))
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	err := c.CloseBead(context.Background(), "bd-fail", map[string]string{"k": "v"})
	if err == nil {
		t.Fatal("expected error when close request fails")
	}
	if !strings.Contains(err.Error(), "closing bead") {
		t.Errorf("expected 'closing bead' in error, got: %v", err)
	}
}

// --- parseNotes tests ---

func TestParseNotes_ParsesKeyValueLines(t *testing.T) {
	notes := "coop_url: http://coop:9090\npod_name: agent-hq-0\n"
	m := ParseNotes(notes)
	if m["coop_url"] != "http://coop:9090" {
		t.Errorf("expected coop_url, got %v", m)
	}
	if m["pod_name"] != "agent-hq-0" {
		t.Errorf("expected pod_name, got %v", m)
	}
}

func TestParseNotes_HandlesEmptyString(t *testing.T) {
	m := ParseNotes("")
	if m != nil {
		t.Errorf("expected nil for empty notes, got %v", m)
	}
}

func TestParseNotes_SkipsBlankLines(t *testing.T) {
	notes := "key1: val1\n\n\nkey2: val2\n"
	m := ParseNotes(notes)
	if len(m) != 2 {
		t.Errorf("expected 2 entries, got %d: %v", len(m), m)
	}
}

func TestParseNotes_HandlesColonInValue(t *testing.T) {
	notes := "url: http://host:8080/path"
	m := ParseNotes(notes)
	if m["url"] != "http://host:8080/path" {
		t.Errorf("expected URL value preserved, got %s", m["url"])
	}
}

func TestParseNotes_TrimsWhitespace(t *testing.T) {
	notes := "  key  :  value  "
	m := ParseNotes(notes)
	if m["key"] != "value" {
		t.Errorf("expected trimmed key/value, got key=%q value=%q", "key", m["key"])
	}
}

func TestParseNotes_NoColonLinesIgnored(t *testing.T) {
	notes := "no-colon-here\nkey: val"
	m := ParseNotes(notes)
	if len(m) != 1 {
		t.Errorf("expected 1 entry, got %d: %v", len(m), m)
	}
	if m["key"] != "val" {
		t.Errorf("expected key=val, got %v", m)
	}
}
