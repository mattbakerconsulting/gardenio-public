package server

// Tests for the ack-only / no-fork decision guard. See decision_validation.go
// and ka-0000002237 for the rules.

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestDecisionValidate_ZeroOptionsRejected(t *testing.T) {
	cases := []json.RawMessage{
		nil,
		json.RawMessage(`{}`),
		json.RawMessage(`{"options":[]}`),
		json.RawMessage(`{"prompt":"yes?"}`),
	}
	for i, fields := range cases {
		r := validateDecisionFields(fields)
		if r.RejectReason == "" {
			t.Errorf("case %d: expected reject for zero options, got accept", i)
		}
		if !strings.Contains(r.RejectReason, "no options") {
			t.Errorf("case %d: reject reason = %q, want contains 'no options'", i, r.RejectReason)
		}
		if r.Hint == "" {
			t.Errorf("case %d: expected hint, got empty", i)
		}
		// ka-0000007734: the zero-options hint must point a done/no-op agent at
		// the WRAP-UP done-path ('gb stop --wrapup'), not at a bare 'gb stop'.
		// A bare stop is itself gated on a decision bead, so recommending it
		// here closed a circular loop with no clean exit. Pin both halves.
		if !strings.Contains(r.Hint, "gb stop --wrapup") {
			t.Errorf("case %d: hint should recommend 'gb stop --wrapup' (the done-path that clears the stop gate), got: %q", i, r.Hint)
		}
		if strings.Contains(r.Hint, "gb squawk") {
			t.Errorf("case %d: hint must not recommend the looping bare-stop path (was 'gb squawk + gb stop'), got: %q", i, r.Hint)
		}
	}
}

func TestDecisionValidate_SingleAckOptionRejected(t *testing.T) {
	for _, id := range []string{"ack", "done", "continue", "acknowledge", "wrap", "noop", "ACK", "  done  "} {
		// Re-encode with the literal id to test trim/case handling.
		raw, _ := json.Marshal(map[string]any{
			"options": []map[string]any{
				{"id": id, "short": "OK", "artifact_type": "report"},
			},
		})
		r := validateDecisionFields(raw)
		if r.RejectReason == "" {
			t.Errorf("id=%q: expected reject for single ack-only option", id)
		}
		if !strings.Contains(r.RejectReason, "no fork") {
			t.Errorf("id=%q: reject reason = %q, want contains 'no fork'", id, r.RejectReason)
		}
	}
}

func TestDecisionValidate_SingleNonAckOptionAccepted(t *testing.T) {
	// A single non-ack option is unusual but not in the reserved list; the
	// guard should not reject it. (Operator may still want a fork, but that
	// is not our call here — we only catch the well-known anti-patterns.)
	fields := json.RawMessage(`{"options":[{"id":"approve","short":"Approve","artifact_type":"plan"}]}`)
	r := validateDecisionFields(fields)
	if r.RejectReason != "" {
		t.Errorf("expected accept for single non-ack option, got reject: %s", r.RejectReason)
	}
}

func TestDecisionValidate_ValidMultiOptionAccepted(t *testing.T) {
	fields := json.RawMessage(`{"options":[
		{"id":"merge","short":"Merge","artifact_type":"diff-summary"},
		{"id":"split","short":"Split","artifact_type":"plan"},
		{"id":"defer","short":"Defer","artifact_type":"bug"}
	]}`)
	r := validateDecisionFields(fields)
	if r.RejectReason != "" {
		t.Errorf("expected accept for valid multi-option decision, got reject: %s", r.RejectReason)
	}
	if r.Warning != "" {
		t.Errorf("expected no warning for varied artifact_types, got: %s", r.Warning)
	}
}

func TestDecisionValidate_AllTldrSoftWarn(t *testing.T) {
	fields := json.RawMessage(`{"options":[
		{"id":"a","short":"A","artifact_type":"tldr"},
		{"id":"b","short":"B","artifact_type":"TLDR"}
	]}`)
	r := validateDecisionFields(fields)
	if r.RejectReason != "" {
		t.Errorf("all-tldr should warn, not reject; got reject: %s", r.RejectReason)
	}
	if r.Warning == "" {
		t.Errorf("expected warning for all-tldr decision, got none")
	}
	if !strings.Contains(r.Warning, "tldr") {
		t.Errorf("warning should mention tldr; got: %s", r.Warning)
	}
}

func TestDecisionValidate_MixedTldrNoWarn(t *testing.T) {
	fields := json.RawMessage(`{"options":[
		{"id":"a","short":"A","artifact_type":"tldr"},
		{"id":"b","short":"B","artifact_type":"plan"}
	]}`)
	r := validateDecisionFields(fields)
	if r.RejectReason != "" {
		t.Errorf("mixed tldr should accept, got reject: %s", r.RejectReason)
	}
	if r.Warning != "" {
		t.Errorf("mixed tldr should not warn, got: %s", r.Warning)
	}
}

func TestDecisionValidate_ErrorMessageIncludesHint(t *testing.T) {
	r := validateDecisionFields(nil)
	msg := r.errorMessage()
	if !strings.Contains(msg, r.RejectReason) {
		t.Errorf("error message missing reject reason: %s", msg)
	}
	if !strings.Contains(msg, "—") {
		t.Errorf("error message missing hint separator: %s", msg)
	}
	if !strings.Contains(msg, "--force") {
		t.Errorf("error message should mention --force override: %s", msg)
	}
}

// ── HTTP-level integration: zero-options rejected unless force=true ─────────

func TestDecisionGuard_HTTPRejectsZeroOptions(t *testing.T) {
	_, _, h := newTestServer()
	rec := doJSON(t, h, "POST", "/v1/beads", map[string]any{
		"title": "ack-only decision",
		"type":  "decision",
		"kind":  "data",
		"fields": map[string]any{
			"prompt": "are we done?",
		},
	})
	if rec.Code != 400 {
		t.Fatalf("expected 400 for zero-options decision, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "no options") {
		t.Errorf("expected error body to mention 'no options', got: %s", rec.Body.String())
	}
}

func TestDecisionGuard_HTTPRejectsSingleAck(t *testing.T) {
	_, _, h := newTestServer()
	rec := doJSON(t, h, "POST", "/v1/beads", map[string]any{
		"title": "ack-only decision",
		"type":  "decision",
		"kind":  "data",
		"fields": map[string]any{
			"prompt": "done?",
			"options": []map[string]any{
				{"id": "ack", "short": "Ack", "artifact_type": "tldr"},
			},
		},
	})
	if rec.Code != 400 {
		t.Fatalf("expected 400 for single-ack decision, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "no fork") {
		t.Errorf("expected error body to mention 'no fork', got: %s", rec.Body.String())
	}
}

func TestDecisionGuard_HTTPForceBypass(t *testing.T) {
	_, _, h := newTestServer()
	rec := doJSON(t, h, "POST", "/v1/beads", map[string]any{
		"title": "force-bypassed decision",
		"type":  "decision",
		"kind":  "data",
		"force": true,
		"fields": map[string]any{
			"prompt": "ok?",
			"options": []map[string]any{
				{"id": "ack", "short": "Ack", "artifact_type": "tldr"},
			},
		},
	})
	requireStatus(t, rec, 201)
}

func TestDecisionGuard_HTTPAcceptsValidDecision(t *testing.T) {
	_, _, h := newTestServer()
	rec := doJSON(t, h, "POST", "/v1/beads", map[string]any{
		"title": "real decision",
		"type":  "decision",
		"kind":  "data",
		"fields": map[string]any{
			"prompt": "merge or split?",
			"options": []map[string]any{
				{"id": "merge", "short": "Merge", "artifact_type": "diff-summary"},
				{"id": "split", "short": "Split", "artifact_type": "plan"},
			},
		},
	})
	requireStatus(t, rec, 201)
}

// ── Rule 0: option SHAPE (ka-0000006724) ───────────────────────────────────

// TestDecisionValidate_StringArrayRejected locks the exact bug that made
// ka-0000000201 render blank: options stored as a JSON array of plain
// strings. A string-array decodes into N zero-value structs (len==3, error
// discarded), so the len==0 zero-options rule never fires — the shape check
// must catch it explicitly.
func TestDecisionValidate_StringArrayRejected(t *testing.T) {
	fields := json.RawMessage(`{"options":["Dual-accept","Big-bang","Hybrid"]}`)
	r := validateDecisionFields(fields)
	if r.RejectReason == "" {
		t.Fatalf("expected reject for string-array options, got accept")
	}
	if !strings.Contains(r.RejectReason, "object") {
		t.Errorf("reject reason should mention 'object' shape; got: %s", r.RejectReason)
	}
	if !strings.Contains(r.Hint, "id") {
		t.Errorf("hint should point at the id-bearing object shape; got: %s", r.Hint)
	}
}

func TestDecisionValidate_ShapeRejections(t *testing.T) {
	cases := []struct {
		name   string
		fields string
	}{
		{"string array", `{"options":["a","b"]}`},
		{"number array", `{"options":[1,2,3]}`},
		{"options is object not array", `{"options":{"id":"a"}}`},
		{"options is bare string", `{"options":"yes"}`},
		{"object missing id", `{"options":[{"short":"A","artifact_type":"plan"}]}`},
		{"object blank id", `{"options":[{"id":"  ","short":"A"}]}`},
		{"mixed object+string", `{"options":[{"id":"a","short":"A","artifact_type":"plan"},"oops"]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := validateDecisionFields(json.RawMessage(tc.fields))
			if r.RejectReason == "" {
				t.Errorf("expected reject for %s, got accept (fields=%s)", tc.name, tc.fields)
			}
		})
	}
}

func TestDecisionValidate_ShapeAcceptsValidObjects(t *testing.T) {
	// Object-array with ids must NOT trip the shape check (artifact_type is
	// not required by the guard — only id-bearing object shape is).
	cases := []string{
		`{"options":[{"id":"a","short":"A","artifact_type":"plan"},{"id":"b","short":"B","artifact_type":"report"}]}`,
		`{"options":[{"id":"a"},{"id":"b"}]}`,
	}
	for _, fields := range cases {
		r := validateDecisionFields(json.RawMessage(fields))
		if r.RejectReason != "" {
			t.Errorf("expected accept for valid object-array, got reject: %s (fields=%s)", r.RejectReason, fields)
		}
	}
}

func TestDecisionValidate_ShapeIgnoresEmptyAndAbsent(t *testing.T) {
	// Empty/absent options are Rule 1's concern, not a shape error — they
	// must still reject (with the no-options reason), not the shape reason.
	for _, fields := range []string{`{"options":[]}`, `{}`} {
		r := validateDecisionFields(json.RawMessage(fields))
		if r.RejectReason == "" {
			t.Errorf("expected reject (no options) for %s", fields)
		}
		if !strings.Contains(r.RejectReason, "no options") {
			t.Errorf("expected 'no options' reason for %s, got: %s", fields, r.RejectReason)
		}
	}
}

func TestDecisionGuard_HTTPRejectsStringArray(t *testing.T) {
	_, _, h := newTestServer()
	rec := doJSON(t, h, "POST", "/v1/beads", map[string]any{
		"title": "string-array decision",
		"type":  "decision",
		"kind":  "data",
		"fields": map[string]any{
			"prompt":  "which migration strategy?",
			"options": []string{"Dual-accept", "Big-bang", "Hybrid"},
		},
	})
	if rec.Code != 400 {
		t.Fatalf("expected 400 for string-array options, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "object") {
		t.Errorf("expected error body to mention 'object' shape, got: %s", rec.Body.String())
	}
}

func TestDecisionGuard_HTTPForceBypassesShape(t *testing.T) {
	_, _, h := newTestServer()
	rec := doJSON(t, h, "POST", "/v1/beads", map[string]any{
		"title": "force string-array",
		"type":  "decision",
		"kind":  "data",
		"force": true,
		"fields": map[string]any{
			"prompt":  "which?",
			"options": []string{"a", "b", "c"},
		},
	})
	requireStatus(t, rec, 201)
}
