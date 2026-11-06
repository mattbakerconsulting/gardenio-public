// Tests for `gb trigger block`/`unblock`/`show` (trigger_block.go).
//
// These tests stand up an httptest server that emulates the beads daemon for
// the read/modify/write cycle inside mutateBlockedBy and runTriggerShow:
//   - GET  /v1/beads/<id>   →  return the bead JSON (used by both commands)
//   - PATCH /v1/beads/<id>  →  capture the merged fields, ack with 200
//
// The CLI commands round-trip blocked_by through parseBlockedByField, so the
// server records the raw stored string and we assert on parsed values.

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

	"gardenio/controller/internal/beadsapi"
)

// triggerBeadFixture is a minimal in-memory representation of a bead used by
// the test daemon. It mirrors the shape the real /v1/beads/<id> endpoint
// returns — only the fields the CLI reads or writes.
type triggerBeadFixture struct {
	ID     string
	Type   string
	Status string
	Title  string
	Labels []string
	Fields map[string]string
}

type triggerDaemonStub struct {
	mu       sync.Mutex
	beads    map[string]*triggerBeadFixture
	patches  []patchCall
	missing  map[string]bool // IDs to return 404 for
	getErrs  map[string]bool // IDs to return 500 for on GET
	patchErr map[string]bool // IDs to return 500 for on PATCH
}

type patchCall struct {
	ID     string
	Fields map[string]any
}

func (s *triggerDaemonStub) handler(t *testing.T) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// All trigger CLI paths look like /v1/beads/<id>
		if !strings.HasPrefix(r.URL.Path, "/v1/beads/") {
			http.NotFound(w, r)
			return
		}
		id := strings.TrimPrefix(r.URL.Path, "/v1/beads/")

		s.mu.Lock()
		bead, ok := s.beads[id]
		missing := s.missing[id]
		getErr := s.getErrs[id]
		patchErr := s.patchErr[id]
		s.mu.Unlock()

		switch r.Method {
		case http.MethodGet:
			if getErr {
				http.Error(w, `{"error":"boom"}`, http.StatusInternalServerError)
				return
			}
			if missing || !ok {
				http.NotFound(w, r)
				return
			}
			fieldsRaw, _ := json.Marshal(bead.Fields)
			resp := map[string]any{
				"id":     bead.ID,
				"type":   bead.Type,
				"status": bead.Status,
				"title":  bead.Title,
				"labels": bead.Labels,
				"fields": json.RawMessage(fieldsRaw),
			}
			_ = json.NewEncoder(w).Encode(resp)

		case http.MethodPatch:
			if patchErr {
				http.Error(w, `{"error":"boom"}`, http.StatusInternalServerError)
				return
			}
			if !ok {
				http.NotFound(w, r)
				return
			}
			body, _ := io.ReadAll(r.Body)
			var req struct {
				Fields json.RawMessage `json:"fields"`
			}
			if err := json.Unmarshal(body, &req); err != nil {
				t.Errorf("patch body parse: %v", err)
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			var merged map[string]any
			if err := json.Unmarshal(req.Fields, &merged); err != nil {
				t.Errorf("patch fields parse: %v", err)
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			s.mu.Lock()
			s.patches = append(s.patches, patchCall{ID: id, Fields: merged})
			// Reflect the merge back into the in-memory bead so subsequent
			// GETs see the new value. The CLI writes blocked_by as a JSON
			// array string OR an empty string; preserve whatever shape we
			// received.
			if v, ok := merged["blocked_by"]; ok {
				switch tv := v.(type) {
				case string:
					bead.Fields["blocked_by"] = tv
				case nil:
					bead.Fields["blocked_by"] = ""
				default:
					raw, _ := json.Marshal(tv)
					bead.Fields["blocked_by"] = string(raw)
				}
			}
			s.mu.Unlock()
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})

		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
}

func setupTriggerDaemon(t *testing.T, stub *triggerDaemonStub) {
	t.Helper()
	srv := httptest.NewServer(stub.handler(t))
	t.Cleanup(srv.Close)

	c, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatalf("beadsapi.New: %v", err)
	}
	oldDaemon := daemon
	daemon = c
	t.Cleanup(func() { daemon = oldDaemon })
}

func newTriggerStub(beads ...*triggerBeadFixture) *triggerDaemonStub {
	m := make(map[string]*triggerBeadFixture, len(beads))
	for _, b := range beads {
		if b.Fields == nil {
			b.Fields = map[string]string{}
		}
		m[b.ID] = b
	}
	return &triggerDaemonStub{beads: m}
}

// extractStoredBlockedBy parses the raw blocked_by field value the CLI wrote
// into the bead. The CLI marshals as a JSON array of strings, OR an empty
// string when the list became empty.
func extractStoredBlockedBy(t *testing.T, raw any) []string {
	t.Helper()
	if raw == nil {
		return nil
	}
	s, ok := raw.(string)
	if !ok {
		t.Fatalf("blocked_by field is %T, expected string (raw=%v)", raw, raw)
	}
	if strings.TrimSpace(s) == "" {
		return nil
	}
	var out []string
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		t.Fatalf("unmarshalling blocked_by=%q: %v", s, err)
	}
	return out
}

// ── parseBlockedByField unit tests ─────────────────────────────────────────

func TestParseBlockedByField_Shapes(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"empty", "", nil},
		{"whitespace only", "   ", nil},
		{"single id", "gd-decA", []string{"gd-decA"}},
		{"csv", "gd-decA,gd-decB,gd-decC", []string{"gd-decA", "gd-decB", "gd-decC"}},
		{"csv with spaces", " gd-decA , gd-decB ,gd-decC ", []string{"gd-decA", "gd-decB", "gd-decC"}},
		{"csv empty entries dropped", "gd-decA,,gd-decB,  ,gd-decC", []string{"gd-decA", "gd-decB", "gd-decC"}},
		{"json array", `["gd-decA","gd-decB"]`, []string{"gd-decA", "gd-decB"}},
		{"json array with spaces", `[" gd-decA ", "gd-decB"]`, []string{"gd-decA", "gd-decB"}},
		{"json array empty entries dropped", `["gd-decA","","gd-decB"]`, []string{"gd-decA", "gd-decB"}},
		// Malformed JSON falls back to CSV: the brackets become part of the
		// first/last entry but the function still returns non-empty parts.
		{"malformed json falls back to csv", `[gd-decA,gd-decB`, []string{"[gd-decA", "gd-decB"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseBlockedByField(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("parseBlockedByField(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// ── gb trigger block ───────────────────────────────────────────────────────

func TestTriggerBlock_AppendsToEmpty(t *testing.T) {
	stub := newTriggerStub(&triggerBeadFixture{
		ID: "gd-trig1", Type: "trigger", Status: "open", Title: "T",
	})
	setupTriggerDaemon(t, stub)

	cmd := triggerBlockCmd
	cmd.SetContext(context.Background())
	_ = cmd.Flags().Set("decision", "gd-decA")
	t.Cleanup(func() { _ = cmd.Flags().Set("decision", "") })

	if err := cmd.RunE(cmd, []string{"gd-trig1"}); err != nil {
		t.Fatalf("RunE: %v", err)
	}

	stub.mu.Lock()
	defer stub.mu.Unlock()
	if len(stub.patches) != 1 {
		t.Fatalf("patches = %d, want 1", len(stub.patches))
	}
	got := extractStoredBlockedBy(t, stub.patches[0].Fields["blocked_by"])
	want := []string{"gd-decA"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("blocked_by after block = %v, want %v", got, want)
	}
}

func TestTriggerBlock_AppendsToExisting(t *testing.T) {
	stub := newTriggerStub(&triggerBeadFixture{
		ID: "gd-trig1", Type: "trigger", Status: "open", Title: "T",
		Fields: map[string]string{"blocked_by": `["gd-decA"]`},
	})
	setupTriggerDaemon(t, stub)

	cmd := triggerBlockCmd
	cmd.SetContext(context.Background())
	_ = cmd.Flags().Set("decision", "gd-decB")
	t.Cleanup(func() { _ = cmd.Flags().Set("decision", "") })

	if err := cmd.RunE(cmd, []string{"gd-trig1"}); err != nil {
		t.Fatalf("RunE: %v", err)
	}

	stub.mu.Lock()
	defer stub.mu.Unlock()
	if len(stub.patches) != 1 {
		t.Fatalf("patches = %d, want 1", len(stub.patches))
	}
	got := extractStoredBlockedBy(t, stub.patches[0].Fields["blocked_by"])
	want := []string{"gd-decA", "gd-decB"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("blocked_by after block = %v, want %v", got, want)
	}
}

func TestTriggerBlock_DeduplicatesRepeatedDecision(t *testing.T) {
	// Calling `block` twice with the same decision must not produce duplicate
	// entries — mutateBlockedBy filters out the target ID before appending.
	// The merged fields end up identical to what's already stored, so the
	// underlying UpdateBeadFieldsTyped fast-path skips the PATCH entirely;
	// what we verify is that the post-state list is still the single entry.
	stub := newTriggerStub(&triggerBeadFixture{
		ID: "gd-trig1", Type: "trigger", Status: "open", Title: "T",
		Fields: map[string]string{"blocked_by": `["gd-decA"]`},
	})
	setupTriggerDaemon(t, stub)

	cmd := triggerBlockCmd
	cmd.SetContext(context.Background())
	_ = cmd.Flags().Set("decision", "gd-decA")
	t.Cleanup(func() { _ = cmd.Flags().Set("decision", "") })

	if err := cmd.RunE(cmd, []string{"gd-trig1"}); err != nil {
		t.Fatalf("RunE: %v", err)
	}

	stub.mu.Lock()
	defer stub.mu.Unlock()
	// Either 0 patches (no-op fast-path) or 1 patch with the same list — both
	// are correct as long as the stored list is exactly ["gd-decA"].
	if len(stub.patches) > 1 {
		t.Fatalf("patches = %d, want 0 or 1", len(stub.patches))
	}
	stored := stub.beads["gd-trig1"].Fields["blocked_by"]
	var got []string
	if err := json.Unmarshal([]byte(stored), &got); err != nil {
		t.Fatalf("unmarshalling stored blocked_by=%q: %v", stored, err)
	}
	want := []string{"gd-decA"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("blocked_by after dup block = %v, want %v (no duplicates)", got, want)
	}
}

func TestTriggerBlock_RequiresDecisionFlag(t *testing.T) {
	stub := newTriggerStub(&triggerBeadFixture{
		ID: "gd-trig1", Type: "trigger", Status: "open", Title: "T",
	})
	setupTriggerDaemon(t, stub)

	cmd := triggerBlockCmd
	cmd.SetContext(context.Background())
	_ = cmd.Flags().Set("decision", "")
	t.Cleanup(func() { _ = cmd.Flags().Set("decision", "") })

	err := cmd.RunE(cmd, []string{"gd-trig1"})
	if err == nil || !strings.Contains(err.Error(), "--decision") {
		t.Fatalf("expected --decision required error, got %v", err)
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if len(stub.patches) != 0 {
		t.Errorf("no PATCH should be issued without --decision, got %d", len(stub.patches))
	}
}

func TestTriggerBlock_RejectsWrongBeadType(t *testing.T) {
	stub := newTriggerStub(&triggerBeadFixture{
		ID: "gd-task1", Type: "task", Status: "open", Title: "T",
	})
	setupTriggerDaemon(t, stub)

	cmd := triggerBlockCmd
	cmd.SetContext(context.Background())
	_ = cmd.Flags().Set("decision", "gd-decA")
	t.Cleanup(func() { _ = cmd.Flags().Set("decision", "") })

	err := cmd.RunE(cmd, []string{"gd-task1"})
	if err == nil || !strings.Contains(err.Error(), "expected type=trigger") {
		t.Fatalf("expected wrong-type error, got %v", err)
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if len(stub.patches) != 0 {
		t.Errorf("no PATCH should be issued for wrong type, got %d", len(stub.patches))
	}
}

func TestTriggerBlock_TriggerNotFound(t *testing.T) {
	stub := newTriggerStub()
	setupTriggerDaemon(t, stub)

	cmd := triggerBlockCmd
	cmd.SetContext(context.Background())
	_ = cmd.Flags().Set("decision", "gd-decA")
	t.Cleanup(func() { _ = cmd.Flags().Set("decision", "") })

	err := cmd.RunE(cmd, []string{"gd-missing"})
	if err == nil {
		t.Fatal("expected error for missing trigger, got nil")
	}
}

// ── gb trigger unblock ─────────────────────────────────────────────────────

func TestTriggerUnblock_RemovesExisting(t *testing.T) {
	stub := newTriggerStub(&triggerBeadFixture{
		ID: "gd-trig1", Type: "trigger", Status: "open", Title: "T",
		Fields: map[string]string{"blocked_by": `["gd-decA","gd-decB","gd-decC"]`},
	})
	setupTriggerDaemon(t, stub)

	cmd := triggerUnblockCmd
	cmd.SetContext(context.Background())
	_ = cmd.Flags().Set("decision", "gd-decB")
	t.Cleanup(func() { _ = cmd.Flags().Set("decision", "") })

	if err := cmd.RunE(cmd, []string{"gd-trig1"}); err != nil {
		t.Fatalf("RunE: %v", err)
	}

	stub.mu.Lock()
	defer stub.mu.Unlock()
	if len(stub.patches) != 1 {
		t.Fatalf("patches = %d, want 1", len(stub.patches))
	}
	got := extractStoredBlockedBy(t, stub.patches[0].Fields["blocked_by"])
	want := []string{"gd-decA", "gd-decC"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("blocked_by after unblock = %v, want %v", got, want)
	}
}

func TestTriggerUnblock_LastEntryClearsField(t *testing.T) {
	// Removing the only entry collapses the field to an empty string so the
	// engine's parseStringList returns nil and the gate is fully disabled.
	stub := newTriggerStub(&triggerBeadFixture{
		ID: "gd-trig1", Type: "trigger", Status: "open", Title: "T",
		Fields: map[string]string{"blocked_by": `["gd-decA"]`},
	})
	setupTriggerDaemon(t, stub)

	cmd := triggerUnblockCmd
	cmd.SetContext(context.Background())
	_ = cmd.Flags().Set("decision", "gd-decA")
	t.Cleanup(func() { _ = cmd.Flags().Set("decision", "") })

	if err := cmd.RunE(cmd, []string{"gd-trig1"}); err != nil {
		t.Fatalf("RunE: %v", err)
	}

	stub.mu.Lock()
	defer stub.mu.Unlock()
	if len(stub.patches) != 1 {
		t.Fatalf("patches = %d, want 1", len(stub.patches))
	}
	stored, ok := stub.patches[0].Fields["blocked_by"]
	if !ok {
		t.Fatalf("blocked_by field missing from patch fields=%v", stub.patches[0].Fields)
	}
	s, isStr := stored.(string)
	if !isStr || s != "" {
		t.Errorf("blocked_by after final unblock = %v (%T), want \"\"", stored, stored)
	}
}

func TestTriggerUnblock_NoOpWhenMissing(t *testing.T) {
	// Unblocking a decision that isn't currently in the list is allowed and
	// leaves the surviving entries intact.
	stub := newTriggerStub(&triggerBeadFixture{
		ID: "gd-trig1", Type: "trigger", Status: "open", Title: "T",
		Fields: map[string]string{"blocked_by": `["gd-decA","gd-decB"]`},
	})
	setupTriggerDaemon(t, stub)

	cmd := triggerUnblockCmd
	cmd.SetContext(context.Background())
	_ = cmd.Flags().Set("decision", "gd-decX")
	t.Cleanup(func() { _ = cmd.Flags().Set("decision", "") })

	if err := cmd.RunE(cmd, []string{"gd-trig1"}); err != nil {
		t.Fatalf("RunE: %v", err)
	}

	stub.mu.Lock()
	defer stub.mu.Unlock()
	// The UpdateBeadFieldsTyped fast-path skips the PATCH when merged fields
	// equal the existing fields. Removing an absent entry yields the same
	// list, so we expect zero PATCH calls.
	if len(stub.patches) != 0 {
		got := extractStoredBlockedBy(t, stub.patches[0].Fields["blocked_by"])
		want := []string{"gd-decA", "gd-decB"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("blocked_by after no-op unblock = %v, want %v (unchanged)", got, want)
		}
	}
}

func TestTriggerUnblock_RequiresDecisionFlag(t *testing.T) {
	stub := newTriggerStub(&triggerBeadFixture{
		ID: "gd-trig1", Type: "trigger", Status: "open",
	})
	setupTriggerDaemon(t, stub)

	cmd := triggerUnblockCmd
	cmd.SetContext(context.Background())
	_ = cmd.Flags().Set("decision", "")
	t.Cleanup(func() { _ = cmd.Flags().Set("decision", "") })

	err := cmd.RunE(cmd, []string{"gd-trig1"})
	if err == nil || !strings.Contains(err.Error(), "--decision") {
		t.Fatalf("expected --decision required error, got %v", err)
	}
}

// ── gb trigger show ────────────────────────────────────────────────────────

func TestTriggerShow_NoBlockedBy(t *testing.T) {
	stub := newTriggerStub(&triggerBeadFixture{
		ID: "gd-trig1", Type: "trigger", Status: "open", Title: "T",
		Labels: []string{"trigger:jira", "project:gardenio"},
		Fields: map[string]string{"action": "notify", "enabled": "true"},
	})
	setupTriggerDaemon(t, stub)

	cmd := triggerShowCmd
	cmd.SetContext(context.Background())
	var out strings.Builder
	cmd.SetOut(&out)
	cmd.SetErr(&out)

	if err := cmd.RunE(cmd, []string{"gd-trig1"}); err != nil {
		t.Fatalf("RunE: %v", err)
	}

	if !strings.Contains(out.String(), "blocked_by: (none)") {
		t.Errorf("show output missing (none) marker for empty blocked_by:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "gd-trig1") {
		t.Errorf("show output missing trigger ID:\n%s", out.String())
	}
}

func TestTriggerShow_ReportsReferenceStatuses(t *testing.T) {
	// Two references: one open (blocking), one closed (resolved). The show
	// command must report each with the correct status marker.
	stub := newTriggerStub(
		&triggerBeadFixture{
			ID: "gd-trig1", Type: "trigger", Status: "open", Title: "T",
			Fields: map[string]string{"blocked_by": `["gd-decOPEN","gd-decCLOSED"]`},
		},
		&triggerBeadFixture{ID: "gd-decOPEN", Type: "decision", Status: "open", Title: "open"},
		&triggerBeadFixture{ID: "gd-decCLOSED", Type: "decision", Status: "closed", Title: "closed"},
	)
	setupTriggerDaemon(t, stub)

	cmd := triggerShowCmd
	cmd.SetContext(context.Background())
	var out strings.Builder
	cmd.SetOut(&out)
	cmd.SetErr(&out)

	if err := cmd.RunE(cmd, []string{"gd-trig1"}); err != nil {
		t.Fatalf("RunE: %v", err)
	}

	got := out.String()
	for _, want := range []string{
		"gd-decOPEN",
		"[open]",
		"gd-decCLOSED",
		"[closed]",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("show output missing %q:\n%s", want, got)
		}
	}
}

func TestTriggerShow_PouringStallBanner(t *testing.T) {
	// A trigger the pouring-stall sweep flagged carries the stamped pour_stall*
	// fields; `gb trigger show` must surface them as a first-class warning
	// banner (not buried in the alphabetical "other fields" dump).
	stub := newTriggerStub(&triggerBeadFixture{
		ID: "gd-trigPOUR", Type: "trigger", Status: "open", Title: "FLAT gardener",
		Labels: []string{"trigger:schedule", "project:gardenio"},
		Fields: map[string]string{
			"action":                 "apply_formula",
			"enabled":                "true",
			"pour_stall":             "true",
			"pour_stall_molecules":   "13",
			"pour_stall_oldest_age":  "72h0m0s",
			"pour_stall_detected_at": "2026-07-11T00:00:00Z",
		},
	})
	setupTriggerDaemon(t, stub)

	cmd := triggerShowCmd
	cmd.SetContext(context.Background())
	var out strings.Builder
	cmd.SetOut(&out)
	cmd.SetErr(&out)

	if err := cmd.RunE(cmd, []string{"gd-trigPOUR"}); err != nil {
		t.Fatalf("RunE: %v", err)
	}
	got := out.String()
	for _, want := range []string{"POURING-BUT-NEVER-WORKED", "13 inert", "72h0m0s"} {
		if !strings.Contains(got, want) {
			t.Errorf("show output missing %q:\n%s", want, got)
		}
	}
	// The stamped fields must NOT be double-printed in the "other fields" dump.
	if strings.Contains(got, "    pour_stall:") {
		t.Errorf("pour_stall field should be skipped in the other-fields catch-all:\n%s", got)
	}
}

func TestTriggerShow_LookupErrorIsReported(t *testing.T) {
	// A referenced bead that 404s is reported with a "?" status and the
	// error text so the operator can see why the lookup failed — the gate
	// itself treats the bead as not-blocking, but show should still surface
	// the failure rather than silently dropping it.
	stub := newTriggerStub(&triggerBeadFixture{
		ID: "gd-trig1", Type: "trigger", Status: "open", Title: "T",
		Fields: map[string]string{"blocked_by": `["gd-vHZfCm9vAu"]`},
	})
	setupTriggerDaemon(t, stub)

	cmd := triggerShowCmd
	cmd.SetContext(context.Background())
	var out strings.Builder
	cmd.SetOut(&out)
	cmd.SetErr(&out)

	if err := cmd.RunE(cmd, []string{"gd-trig1"}); err != nil {
		t.Fatalf("RunE: %v", err)
	}

	got := out.String()
	if !strings.Contains(got, "gd-vHZfCm9vAu") {
		t.Errorf("show output missing referenced bead id:\n%s", got)
	}
	if !strings.Contains(got, "[?]") {
		t.Errorf("show output missing [?] marker for lookup failure:\n%s", got)
	}
	if !strings.Contains(got, "error:") {
		t.Errorf("show output missing error annotation:\n%s", got)
	}
}

func TestTriggerShow_TriggerNotFound(t *testing.T) {
	stub := newTriggerStub()
	setupTriggerDaemon(t, stub)

	cmd := triggerShowCmd
	cmd.SetContext(context.Background())
	var out strings.Builder
	cmd.SetOut(&out)
	cmd.SetErr(&out)

	err := cmd.RunE(cmd, []string{"gd-missing"})
	if err == nil {
		t.Fatal("expected error for missing trigger, got nil")
	}
}

// ── sortedFieldKeys helper ─────────────────────────────────────────────────

func TestSortedFieldKeys(t *testing.T) {
	in := map[string]string{"c": "3", "a": "1", "b": "2"}
	got := sortedFieldKeys(in)
	want := []string{"a", "b", "c"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("sortedFieldKeys = %v, want %v", got, want)
	}
}
