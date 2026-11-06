package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

func TestParseSimpleOption_ThreeParts(t *testing.T) {
	opt, err := parseSimpleOption("yes:Deploy now:plan")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if opt["id"] != "yes" {
		t.Errorf("id = %v, want yes", opt["id"])
	}
	if opt["short"] != "Deploy now" {
		t.Errorf("short = %v, want 'Deploy now'", opt["short"])
	}
	if opt["artifact_type"] != "plan" {
		t.Errorf("artifact_type = %v, want plan", opt["artifact_type"])
	}
}

func TestParseSimpleOption_TwoParts_DefaultsToReport(t *testing.T) {
	opt, err := parseSimpleOption("skip:Skip this")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if opt["id"] != "skip" {
		t.Errorf("id = %v, want skip", opt["id"])
	}
	if opt["short"] != "Skip this" {
		t.Errorf("short = %v, want 'Skip this'", opt["short"])
	}
	if opt["artifact_type"] != "report" {
		t.Errorf("artifact_type = %v, want report", opt["artifact_type"])
	}
}

func TestParseSimpleOption_OnePart_UsesAsID(t *testing.T) {
	opt, err := parseSimpleOption("approve")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if opt["id"] != "approve" {
		t.Errorf("id = %v, want approve", opt["id"])
	}
	if opt["artifact_type"] != "report" {
		t.Errorf("artifact_type = %v, want report", opt["artifact_type"])
	}
}

func TestParseSimpleOption_EmptyArtifactType_DefaultsToReport(t *testing.T) {
	opt, err := parseSimpleOption("yes:Approve:")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if opt["artifact_type"] != "report" {
		t.Errorf("artifact_type = %v, want report", opt["artifact_type"])
	}
}

func TestParseSimpleOption_InvalidArtifactType(t *testing.T) {
	_, err := parseSimpleOption("yes:Approve:invalid")
	if err == nil {
		t.Fatal("expected error for invalid artifact_type")
	}
}

func TestParseSimpleOption_AllValidArtifactTypes(t *testing.T) {
	for _, at := range []string{"report", "plan", "checklist", "diff-summary", "epic", "bug", "tldr", "bundle"} {
		opt, err := parseSimpleOption("id:label:" + at)
		if err != nil {
			t.Errorf("unexpected error for artifact_type %q: %v", at, err)
		}
		if got := opt["artifact_type"]; got != at {
			t.Errorf("artifact_type = %v, want %v", got, at)
		}
	}
}

// ── decision create: artifact_params round-trip ──────────────────────────

// TestDecisionCreate_JSONOptions_ArtifactParamsRoundTrips verifies that the
// --options JSON parser tolerates an arbitrary `artifact_params` object on
// each option and preserves it verbatim through create → read. The field is
// consumed by downstream code (length validator, renderer, prime injection);
// the create path must not strip or reshape it.
func TestDecisionCreate_JSONOptions_ArtifactParamsRoundTrips(t *testing.T) {
	tests := []struct {
		name        string
		optionsJSON string
		wantParams  map[string]any // nil = option should have no artifact_params
	}{
		{
			name:        "tldr with max_chars and min_chars",
			optionsJSON: `[{"id":"done","short":"Summary","label":"Summary","artifact_type":"tldr","artifact_params":{"max_chars":400,"min_chars":40}}]`,
			wantParams:  map[string]any{"max_chars": float64(400), "min_chars": float64(40)},
		},
		{
			name:        "plan with nested array and bool params",
			optionsJSON: `[{"id":"a","short":"A","label":"Plan A","artifact_type":"plan","artifact_params":{"sections":["goal","risks"],"strict":true}}]`,
			wantParams:  map[string]any{"sections": []any{"goal", "risks"}, "strict": true},
		},
		{
			name:        "no artifact_params present",
			optionsJSON: `[{"id":"skip","short":"Skip","label":"Skip","artifact_type":"report"}]`,
			wantParams:  nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var storedOptions string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodPost && r.URL.Path == "/v1/beads":
					body, _ := io.ReadAll(r.Body)
					var req struct {
						Fields json.RawMessage `json:"fields"`
					}
					if err := json.Unmarshal(body, &req); err != nil {
						t.Fatalf("parse create body: %v", err)
					}
					var fields map[string]json.RawMessage
					if err := json.Unmarshal(req.Fields, &fields); err != nil {
						t.Fatalf("parse fields: %v", err)
					}
					// The daemon stores field values as strings; mimic that.
					storedOptions = string(fields["options"])
					_ = json.NewEncoder(w).Encode(map[string]string{"id": "gd-stored"})

				case r.Method == http.MethodGet && r.URL.Path == "/v1/beads/gd-stored":
					bead := map[string]any{
						"id":     "gd-stored",
						"type":   "decision",
						"status": "open",
						"fields": map[string]string{"options": storedOptions},
					}
					_ = json.NewEncoder(w).Encode(bead)

				default:
					http.NotFound(w, r)
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

			cmd := decisionCreateCmd
			cmd.SetContext(context.Background())
			_ = cmd.Flags().Set("prompt", "test prompt")
			_ = cmd.Flags().Set("options", tc.optionsJSON)
			_ = cmd.Flags().Set("no-wait", "true")
			t.Cleanup(func() {
				cmd.Flags().Set("prompt", "")
				cmd.Flags().Set("options", "")
				cmd.Flags().Set("no-wait", "false")
			})

			if err := cmd.RunE(cmd, nil); err != nil {
				t.Fatalf("create: %v", err)
			}

			// Read back: simulate `kd show <id> --json`. We go through the
			// same HTTP GET path an operator would hit.
			bead, err := daemon.GetBead(context.Background(), "gd-stored")
			if err != nil {
				t.Fatalf("GetBead: %v", err)
			}
			raw, ok := bead.Fields["options"]
			if !ok || raw == "" {
				t.Fatalf("options field missing from stored bead, got fields=%v", bead.Fields)
			}
			var opts []map[string]any
			if err := json.Unmarshal([]byte(raw), &opts); err != nil {
				t.Fatalf("parse stored options: %v (raw=%q)", err, raw)
			}
			if len(opts) != 1 {
				t.Fatalf("expected 1 option, got %d", len(opts))
			}
			params, hasParams := opts[0]["artifact_params"]
			if tc.wantParams == nil {
				if hasParams {
					t.Errorf("artifact_params should be absent, got %v", params)
				}
				return
			}
			if !hasParams {
				t.Fatalf("artifact_params missing after round-trip (options=%v)", opts[0])
			}
			gotMap, ok := params.(map[string]any)
			if !ok {
				t.Fatalf("artifact_params = %T, want map[string]any", params)
			}
			if !reflect.DeepEqual(gotMap, tc.wantParams) {
				t.Errorf("artifact_params = %#v, want %#v", gotMap, tc.wantParams)
			}
		})
	}
}

// ── decision cancel tests ────────────────────────────────────────────────

type decisionDaemonStub struct {
	mu      sync.Mutex
	beads   []map[string]any
	cancels []cancelCall
	failIDs map[string]bool
}

type cancelCall struct {
	ID   string
	Body map[string]any
}

func (s *decisionDaemonStub) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/beads":
			s.mu.Lock()
			resp := map[string]any{"beads": s.beads, "total": len(s.beads)}
			s.mu.Unlock()
			_ = json.NewEncoder(w).Encode(resp)

		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/cancel"):
			// /v1/decisions/{id}/cancel
			id := strings.TrimPrefix(r.URL.Path, "/v1/decisions/")
			id = strings.TrimSuffix(id, "/cancel")
			body, _ := io.ReadAll(r.Body)
			var parsed map[string]any
			_ = json.Unmarshal(body, &parsed)
			s.mu.Lock()
			failed := s.failIDs[id]
			s.cancels = append(s.cancels, cancelCall{ID: id, Body: parsed})
			s.mu.Unlock()
			if failed {
				http.Error(w, `{"error":"boom"}`, http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})

		default:
			http.NotFound(w, r)
		}
	})
}

func setupDecisionDaemon(t *testing.T, stub *decisionDaemonStub) {
	t.Helper()
	srv := httptest.NewServer(stub.handler())
	t.Cleanup(srv.Close)

	c, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	oldDaemon := daemon
	daemon = c
	t.Cleanup(func() { daemon = oldDaemon })
}

func makeDecisionBead(id string, createdAgo time.Duration, fields map[string]string) map[string]any {
	fieldsRaw, _ := json.Marshal(fields)
	return map[string]any{
		"id":         id,
		"title":      "decision " + id,
		"type":       "decision",
		"kind":       "data",
		"status":     "open",
		"fields":     json.RawMessage(fieldsRaw),
		"created_at": time.Now().Add(-createdAgo).UTC().Format(time.RFC3339),
		"updated_at": time.Now().UTC().Format(time.RFC3339),
	}
}

func TestDecisionCancel_SingleID(t *testing.T) {
	stub := &decisionDaemonStub{}
	setupDecisionDaemon(t, stub)

	cmd := decisionCancelCmd
	cmd.SetContext(context.Background())
	cmd.SetArgs([]string{"gd-abc"})
	_ = cmd.Flags().Set("reason", "triage cleanup")
	t.Cleanup(func() { cmd.Flags().Set("reason", "") })

	if err := cmd.RunE(cmd, []string{"gd-abc"}); err != nil {
		t.Fatalf("RunE returned error: %v", err)
	}
	if len(stub.cancels) != 1 {
		t.Fatalf("got %d cancel calls, want 1", len(stub.cancels))
	}
	if stub.cancels[0].ID != "gd-abc" {
		t.Errorf("cancel id = %q, want gd-abc", stub.cancels[0].ID)
	}
	if r, _ := stub.cancels[0].Body["reason"].(string); r != "triage cleanup" {
		t.Errorf("cancel reason = %q, want 'triage cleanup'", r)
	}
}

func TestDecisionCancel_MultipleIDs(t *testing.T) {
	stub := &decisionDaemonStub{}
	setupDecisionDaemon(t, stub)

	cmd := decisionCancelCmd
	cmd.SetContext(context.Background())
	_ = cmd.Flags().Set("reason", "")
	ids := []string{"gd-1", "gd-2", "gd-3"}
	if err := cmd.RunE(cmd, ids); err != nil {
		t.Fatalf("RunE returned error: %v", err)
	}
	if len(stub.cancels) != 3 {
		t.Fatalf("got %d cancel calls, want 3", len(stub.cancels))
	}
	got := []string{stub.cancels[0].ID, stub.cancels[1].ID, stub.cancels[2].ID}
	for i, want := range ids {
		if got[i] != want {
			t.Errorf("cancel[%d] = %q, want %q", i, got[i], want)
		}
	}
}

func TestDecisionCancel_RejectsIDsWithAll(t *testing.T) {
	stub := &decisionDaemonStub{}
	setupDecisionDaemon(t, stub)

	cmd := decisionCancelCmd
	cmd.SetContext(context.Background())
	_ = cmd.Flags().Set("all", "true")
	_ = cmd.Flags().Set("filter-no-options", "true")
	t.Cleanup(func() {
		cmd.Flags().Set("all", "false")
		cmd.Flags().Set("filter-no-options", "false")
	})

	if err := cmd.RunE(cmd, []string{"gd-abc"}); err == nil {
		t.Fatal("expected error when combining IDs with --all, got nil")
	}
}

func TestDecisionCancel_FiltersRequireAll(t *testing.T) {
	stub := &decisionDaemonStub{}
	setupDecisionDaemon(t, stub)

	cmd := decisionCancelCmd
	cmd.SetContext(context.Background())
	_ = cmd.Flags().Set("filter-no-options", "true")
	t.Cleanup(func() { cmd.Flags().Set("filter-no-options", "false") })

	if err := cmd.RunE(cmd, nil); err == nil {
		t.Fatal("expected error when filters used without --all, got nil")
	}
}

func TestDecisionCancel_AllRequiresFilter(t *testing.T) {
	stub := &decisionDaemonStub{}
	setupDecisionDaemon(t, stub)

	cmd := decisionCancelCmd
	cmd.SetContext(context.Background())
	_ = cmd.Flags().Set("all", "true")
	t.Cleanup(func() { cmd.Flags().Set("all", "false") })

	if err := cmd.RunE(cmd, nil); err == nil {
		t.Fatal("expected error when --all used without filter flag, got nil")
	}
}

func TestDecisionCancel_NoArgs(t *testing.T) {
	stub := &decisionDaemonStub{}
	setupDecisionDaemon(t, stub)

	cmd := decisionCancelCmd
	cmd.SetContext(context.Background())
	if err := cmd.RunE(cmd, nil); err == nil {
		t.Fatal("expected error when called with no args and no flags, got nil")
	}
}

func TestDecisionCancel_BulkFilterNoOptions(t *testing.T) {
	stub := &decisionDaemonStub{
		beads: []map[string]any{
			makeDecisionBead("gd-with-opts", 1*time.Hour, map[string]string{"options": `[{"id":"a"}]`, "prompt": "p"}),
			makeDecisionBead("gd-no-opts", 1*time.Hour, map[string]string{"prompt": "p"}),
		},
	}
	setupDecisionDaemon(t, stub)

	cmd := decisionCancelCmd
	cmd.SetContext(context.Background())
	_ = cmd.Flags().Set("all", "true")
	_ = cmd.Flags().Set("filter-no-options", "true")
	t.Cleanup(func() {
		cmd.Flags().Set("all", "false")
		cmd.Flags().Set("filter-no-options", "false")
	})

	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatalf("RunE returned error: %v", err)
	}
	if len(stub.cancels) != 1 {
		t.Fatalf("got %d cancel calls, want 1", len(stub.cancels))
	}
	if stub.cancels[0].ID != "gd-no-opts" {
		t.Errorf("cancel id = %q, want gd-no-opts", stub.cancels[0].ID)
	}
}

func TestDecisionCancel_BulkOlderThan(t *testing.T) {
	stub := &decisionDaemonStub{
		beads: []map[string]any{
			makeDecisionBead("gd-old", 48*time.Hour, map[string]string{"prompt": "p"}),
			makeDecisionBead("gd-new", 30*time.Minute, map[string]string{"prompt": "p"}),
		},
	}
	setupDecisionDaemon(t, stub)

	cmd := decisionCancelCmd
	cmd.SetContext(context.Background())
	_ = cmd.Flags().Set("all", "true")
	_ = cmd.Flags().Set("older-than", "24h")
	t.Cleanup(func() {
		cmd.Flags().Set("all", "false")
		cmd.Flags().Set("older-than", "0")
	})

	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatalf("RunE returned error: %v", err)
	}
	if len(stub.cancels) != 1 {
		t.Fatalf("got %d cancel calls, want 1", len(stub.cancels))
	}
	if stub.cancels[0].ID != "gd-old" {
		t.Errorf("cancel id = %q, want gd-old", stub.cancels[0].ID)
	}
}

func TestDecisionCancel_DryRun(t *testing.T) {
	stub := &decisionDaemonStub{
		beads: []map[string]any{
			makeDecisionBead("gd-a", 1*time.Hour, map[string]string{"prompt": "p"}),
			makeDecisionBead("gd-b", 1*time.Hour, map[string]string{"prompt": "p"}),
		},
	}
	setupDecisionDaemon(t, stub)

	cmd := decisionCancelCmd
	cmd.SetContext(context.Background())
	_ = cmd.Flags().Set("all", "true")
	_ = cmd.Flags().Set("older-than", "1s")
	_ = cmd.Flags().Set("dry-run", "true")
	t.Cleanup(func() {
		cmd.Flags().Set("all", "false")
		cmd.Flags().Set("older-than", "0")
		cmd.Flags().Set("dry-run", "false")
	})

	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatalf("RunE returned error: %v", err)
	}
	if len(stub.cancels) != 0 {
		t.Fatalf("dry-run should not call cancel, got %d calls", len(stub.cancels))
	}
}

func TestDecisionCancel_FailurePropagates(t *testing.T) {
	stub := &decisionDaemonStub{
		failIDs: map[string]bool{"gd-bad": true},
	}
	setupDecisionDaemon(t, stub)

	cmd := decisionCancelCmd
	cmd.SetContext(context.Background())
	err := cmd.RunE(cmd, []string{"gd-ok", "gd-bad"})
	if err == nil {
		t.Fatal("expected error when one ID fails, got nil")
	}
	if len(stub.cancels) != 2 {
		t.Fatalf("got %d cancel calls, want 2", len(stub.cancels))
	}
}

// ── decision reflag tests ────────────────────────────────────────────────

// reflagDaemonStub implements the GET / PATCH / POST comment endpoints needed
// to exercise decisionReflagCmd end-to-end against an httptest server.
type reflagDaemonStub struct {
	mu       sync.Mutex
	bead     map[string]any
	patches  []map[string]any
	comments []map[string]any
}

func (s *reflagDaemonStub) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/beads/") && !strings.Contains(r.URL.Path, "/comments"):
			s.mu.Lock()
			defer s.mu.Unlock()
			_ = json.NewEncoder(w).Encode(s.bead)

		case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/v1/beads/"):
			body, _ := io.ReadAll(r.Body)
			var parsed map[string]any
			_ = json.Unmarshal(body, &parsed)
			s.mu.Lock()
			s.patches = append(s.patches, parsed)
			// Apply patch to in-memory bead so subsequent GET reflects state.
			if pri, ok := parsed["priority"].(float64); ok {
				s.bead["priority"] = int(pri)
			}
			if labels, ok := parsed["labels"].([]any); ok {
				s.bead["labels"] = labels
			}
			if fields, ok := parsed["fields"]; ok {
				s.bead["fields"] = fields
			}
			s.mu.Unlock()
			w.WriteHeader(http.StatusOK)

		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments"):
			body, _ := io.ReadAll(r.Body)
			var parsed map[string]any
			_ = json.Unmarshal(body, &parsed)
			s.mu.Lock()
			s.comments = append(s.comments, parsed)
			s.mu.Unlock()
			w.WriteHeader(http.StatusOK)

		default:
			http.NotFound(w, r)
		}
	})
}

func setupReflagDaemon(t *testing.T, stub *reflagDaemonStub) {
	t.Helper()
	srv := httptest.NewServer(stub.handler())
	t.Cleanup(srv.Close)
	c, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	oldDaemon := daemon
	daemon = c
	t.Cleanup(func() { daemon = oldDaemon })
}

func makeReflagBead(id string, priority int, fields map[string]any, labels []string) map[string]any {
	if fields == nil {
		fields = map[string]any{}
	}
	fieldsRaw, _ := json.Marshal(fields)
	labelsAny := make([]any, len(labels))
	for i, l := range labels {
		labelsAny[i] = l
	}
	return map[string]any{
		"id":       id,
		"title":    "decision " + id,
		"type":     "decision",
		"kind":     "data",
		"status":   "open",
		"priority": priority,
		"labels":   labelsAny,
		"fields":   json.RawMessage(fieldsRaw),
	}
}

// resetReflagFlags resets the reflag command's flag state across tests.
// Cobra commands are global singletons in this package, so a previous test's
// flag set bleeds into the next case unless we explicitly clear it.
func resetReflagFlags(t *testing.T) {
	t.Helper()
	_ = decisionReflagCmd.Flags().Set("reason", "")
	_ = decisionReflagCmd.Flags().Set("priority", "-1")
	// Clear the "Changed" bookkeeping by recreating the flag default.
	decisionReflagCmd.ResetFlags()
	decisionReflagCmd.Flags().String("reason", "", "reason for reflag (recorded as audit comment and included in Slack repost)")
	decisionReflagCmd.Flags().Int("priority", -1, "set priority explicitly (0=critical .. 4=backlog) instead of bumping by one toward P0")
}

func TestDecisionReflag_BumpsPriorityByOne(t *testing.T) {
	stub := &reflagDaemonStub{
		bead: makeReflagBead("gd-r1", 3, nil, nil),
	}
	setupReflagDaemon(t, stub)
	resetReflagFlags(t)

	cmd := decisionReflagCmd
	cmd.SetContext(context.Background())
	if err := cmd.RunE(cmd, []string{"gd-r1"}); err != nil {
		t.Fatalf("RunE returned error: %v", err)
	}

	// Find the priority patch.
	var newPri int = -1
	for _, p := range stub.patches {
		if v, ok := p["priority"].(float64); ok {
			newPri = int(v)
		}
	}
	if newPri != 2 {
		t.Errorf("priority after reflag = %d, want 2 (3 → 2)", newPri)
	}
	if len(stub.comments) != 1 {
		t.Errorf("got %d audit comments, want 1", len(stub.comments))
	}
}

func TestDecisionReflag_CapsAtP0(t *testing.T) {
	stub := &reflagDaemonStub{
		bead: makeReflagBead("gd-r2", 0, map[string]any{"reflag_count": "3"}, []string{"escalated"}),
	}
	setupReflagDaemon(t, stub)
	resetReflagFlags(t)

	cmd := decisionReflagCmd
	cmd.SetContext(context.Background())
	if err := cmd.RunE(cmd, []string{"gd-r2"}); err != nil {
		t.Fatalf("RunE returned error: %v", err)
	}

	// At P0, no priority patch should be sent (newPriority == oldPriority).
	for _, p := range stub.patches {
		if _, ok := p["priority"]; ok {
			t.Errorf("priority patch sent at P0 cap; got %v", p["priority"])
		}
	}
	// reflag_count should still bump in the fields patch. UpdateBeadFields
	// preserves the JSON type of the existing value: when the stub seeds
	// reflag_count as the string "3", the merged value is sent as "4".
	foundCountBump := false
	for _, p := range stub.patches {
		fields, ok := p["fields"].(map[string]any)
		if !ok {
			continue
		}
		switch v := fields["reflag_count"].(type) {
		case float64:
			if v == 4 {
				foundCountBump = true
			}
		case string:
			if v == "4" {
				foundCountBump = true
			}
		}
	}
	if !foundCountBump {
		t.Errorf("expected reflag_count to bump from 3 to 4 in patches; got %#v", stub.patches)
	}
}

func TestDecisionReflag_AddsEscalatedLabelOnceAtP0P1(t *testing.T) {
	stub := &reflagDaemonStub{
		bead: makeReflagBead("gd-r3", 2, nil, nil),
	}
	setupReflagDaemon(t, stub)
	resetReflagFlags(t)

	cmd := decisionReflagCmd
	cmd.SetContext(context.Background())
	if err := cmd.RunE(cmd, []string{"gd-r3"}); err != nil {
		t.Fatalf("RunE returned error: %v", err)
	}

	// Should have a label patch including "escalated" since 2 → 1.
	foundLabelPatch := false
	for _, p := range stub.patches {
		if labels, ok := p["labels"].([]any); ok {
			for _, l := range labels {
				if s, _ := l.(string); s == "escalated" {
					foundLabelPatch = true
				}
			}
		}
	}
	if !foundLabelPatch {
		t.Errorf("expected 'escalated' label patch when priority drops to P1; got patches %#v", stub.patches)
	}
}

func TestDecisionReflag_DoesNotEscalateAtP2OrBelow(t *testing.T) {
	stub := &reflagDaemonStub{
		bead: makeReflagBead("gd-r4", 4, nil, nil),
	}
	setupReflagDaemon(t, stub)
	resetReflagFlags(t)

	cmd := decisionReflagCmd
	cmd.SetContext(context.Background())
	if err := cmd.RunE(cmd, []string{"gd-r4"}); err != nil {
		t.Fatalf("RunE returned error: %v", err)
	}

	// 4 → 3, still above P1, so no label patch should appear.
	for _, p := range stub.patches {
		if _, ok := p["labels"]; ok {
			t.Errorf("unexpected label patch at P3: %v", p["labels"])
		}
	}
}

func TestDecisionReflag_ExplicitPriorityFlag(t *testing.T) {
	stub := &reflagDaemonStub{
		bead: makeReflagBead("gd-r5", 3, nil, nil),
	}
	setupReflagDaemon(t, stub)
	resetReflagFlags(t)

	cmd := decisionReflagCmd
	cmd.SetContext(context.Background())
	if err := cmd.Flags().Set("priority", "0"); err != nil {
		t.Fatal(err)
	}
	if err := cmd.RunE(cmd, []string{"gd-r5"}); err != nil {
		t.Fatalf("RunE returned error: %v", err)
	}
	var newPri int = -1
	for _, p := range stub.patches {
		if v, ok := p["priority"].(float64); ok {
			newPri = int(v)
		}
	}
	if newPri != 0 {
		t.Errorf("--priority=0 should jump to P0; got %d", newPri)
	}
}

func TestDecisionReflag_RejectsClosedDecision(t *testing.T) {
	stub := &reflagDaemonStub{
		bead: makeReflagBead("gd-r6", 2, nil, nil),
	}
	stub.bead["status"] = "closed"
	setupReflagDaemon(t, stub)
	resetReflagFlags(t)

	cmd := decisionReflagCmd
	cmd.SetContext(context.Background())
	err := cmd.RunE(cmd, []string{"gd-r6"})
	if err == nil {
		t.Fatal("expected error reflagging closed decision; got nil")
	}
}

func TestDecisionReflag_RejectsNonDecisionBead(t *testing.T) {
	stub := &reflagDaemonStub{
		bead: makeReflagBead("gd-r7", 2, nil, nil),
	}
	stub.bead["type"] = "task"
	setupReflagDaemon(t, stub)
	resetReflagFlags(t)

	cmd := decisionReflagCmd
	cmd.SetContext(context.Background())
	err := cmd.RunE(cmd, []string{"gd-r7"})
	if err == nil {
		t.Fatal("expected error reflagging non-decision bead; got nil")
	}
}

func TestDecisionReflag_RejectsPriorityOutOfRange(t *testing.T) {
	stub := &reflagDaemonStub{
		bead: makeReflagBead("gd-r8", 2, nil, nil),
	}
	setupReflagDaemon(t, stub)
	resetReflagFlags(t)

	cmd := decisionReflagCmd
	cmd.SetContext(context.Background())
	if err := cmd.Flags().Set("priority", "9"); err != nil {
		t.Fatal(err)
	}
	err := cmd.RunE(cmd, []string{"gd-r8"})
	if err == nil {
		t.Fatal("expected error for out-of-range priority; got nil")
	}
}

// ── decision create dedup tests (ka-0000003825) ──────────────────────────

// dedupDaemonStub serves a configurable list of open decisions on GET /v1/beads
// and accepts POST /v1/beads as a no-op create. Tracks whether create was
// invoked so tests can assert the dedup gate fired (or didn't).
type dedupDaemonStub struct {
	openDecisions []map[string]any
	created       bool
}

func (s *dedupDaemonStub) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/beads":
			resp := map[string]any{"beads": s.openDecisions, "total": len(s.openDecisions)}
			_ = json.NewEncoder(w).Encode(resp)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/beads":
			s.created = true
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "gd-new"})
		default:
			http.NotFound(w, r)
		}
	})
}

func setupDedupDaemon(t *testing.T, stub *dedupDaemonStub) {
	t.Helper()
	srv := httptest.NewServer(stub.handler())
	t.Cleanup(srv.Close)
	c, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	oldDaemon := daemon
	daemon = c
	t.Cleanup(func() { daemon = oldDaemon })
}

// resetCreateFlags wipes mutated flags between subtests so state doesn't bleed
// across test cases. decisionCreateCmd is a package-global cobra.Command.
func resetCreateFlags(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		_ = decisionCreateCmd.Flags().Set("prompt", "")
		_ = decisionCreateCmd.Flags().Set("options", "")
		_ = decisionCreateCmd.Flags().Set("option", "")
		_ = decisionCreateCmd.Flags().Set("no-wait", "false")
		_ = decisionCreateCmd.Flags().Set("allow-duplicate", "false")
		_ = decisionCreateCmd.Flags().Set("force", "false")
	})
}

func makeOpenDecision(id, prompt string) map[string]any {
	fieldsRaw, _ := json.Marshal(map[string]string{"prompt": prompt})
	return map[string]any{
		"id":     id,
		"title":  prompt,
		"type":   "decision",
		"kind":   "data",
		"status": "open",
		"fields": json.RawMessage(fieldsRaw),
	}
}

func TestDecisionCreate_RefusesDuplicate(t *testing.T) {
	stub := &dedupDaemonStub{
		openDecisions: []map[string]any{
			// Trailing "?" is stripped by the bucket-key normaliser, so this
			// shares a bucket with the new prompt below.
			makeOpenDecision("gd-existing", "Merge MR !2130 into main?"),
		},
	}
	setupDedupDaemon(t, stub)
	resetCreateFlags(t)

	cmd := decisionCreateCmd
	cmd.SetContext(context.Background())
	// Trailing "!!!" is stripped → same bucket as the existing open decision.
	_ = cmd.Flags().Set("prompt", "Merge MR !2130 into main!!!")
	_ = cmd.Flags().Set("option", "yes:Approve:plan")
	_ = cmd.Flags().Set("no-wait", "true")

	err := cmd.RunE(cmd, nil)
	if err == nil {
		t.Fatal("expected dup error; got nil")
	}
	if !strings.Contains(err.Error(), "gd-existing") {
		t.Errorf("error should name existing decision id; got %v", err)
	}
	if stub.created {
		t.Error("create must not be invoked when dup is detected")
	}
}

func TestDecisionCreate_AllowsNonDuplicate(t *testing.T) {
	stub := &dedupDaemonStub{
		openDecisions: []map[string]any{
			makeOpenDecision("gd-existing", "Merge MR !2130 into main?"),
		},
	}
	setupDedupDaemon(t, stub)
	resetCreateFlags(t)

	cmd := decisionCreateCmd
	cmd.SetContext(context.Background())
	_ = cmd.Flags().Set("prompt", "Deploy hotfix to staging immediately?")
	_ = cmd.Flags().Set("option", "yes:Deploy:plan")
	_ = cmd.Flags().Set("no-wait", "true")

	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatalf("create should succeed for distinct prompt; got %v", err)
	}
	if !stub.created {
		t.Error("create must be invoked when no dup exists")
	}
}

func TestDecisionCreate_AllowDuplicateBypass(t *testing.T) {
	stub := &dedupDaemonStub{
		openDecisions: []map[string]any{
			makeOpenDecision("gd-existing", "Merge MR !2130 into main?"),
		},
	}
	setupDedupDaemon(t, stub)
	resetCreateFlags(t)

	cmd := decisionCreateCmd
	cmd.SetContext(context.Background())
	_ = cmd.Flags().Set("prompt", "Merge MR !2130 into main?")
	_ = cmd.Flags().Set("option", "yes:Approve:plan")
	_ = cmd.Flags().Set("no-wait", "true")
	_ = cmd.Flags().Set("allow-duplicate", "true")

	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatalf("create should succeed with --allow-duplicate; got %v", err)
	}
	if !stub.created {
		t.Error("create must be invoked when --allow-duplicate is set, even on dup")
	}
}
