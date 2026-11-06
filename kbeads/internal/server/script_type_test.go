package server

import (
	"net/http"
	"testing"
)

// TestScriptType_Schema verifies the type:script builtin declares the
// probe-execution payload fields (probe-pods plan ka-0000000544): a required
// `script` body plus the runner's activity_deadline_seconds / exit_code /
// close_reason / execution_target / project fields.
func TestScriptType_Schema(t *testing.T) {
	tc := parseBuiltinSchema(t, "script")
	if tc.Kind != "data" {
		t.Errorf("type:script kind = %q, want data (a script is a payload, not a tracked issue)", tc.Kind)
	}
	if f := fieldByName(tc, "script"); f == nil {
		t.Fatal("type:script missing the `script` field")
	} else if !f.Required {
		t.Error("type:script `script` field must be required")
	}
	for _, name := range []string{"activity_deadline_seconds", "exit_code", "close_reason", "execution_target", "project"} {
		if fieldByName(tc, name) == nil {
			t.Errorf("type:script missing field %q", name)
		}
	}
}

// TestScriptType_CreateAndClose is the end-to-end regression: creating a
// type=script bead with a probe payload must not 400, and the runner's terminal
// close (exit_code + close_reason) must be accepted too.
func TestScriptType_CreateAndClose(t *testing.T) {
	_, _, h := newTestServer()

	rec := doJSON(t, h, "POST", "/v1/beads", map[string]any{
		"title": "probe: has main moved since last release?",
		"type":  "script",
		"force": true,
		"fields": map[string]any{
			"script":                    "git ls-remote origin main | grep -q . || exit 1",
			"activity_deadline_seconds": 60,
			"execution_target":          "probe",
			"project":                   "gardenio",
		},
	})
	if rec.Code == http.StatusBadRequest {
		t.Fatalf("creating type=script 400'd (regression): %s", rec.Body.String())
	}
	requireStatus(t, rec, 201)

	var created struct {
		ID string `json:"id"`
	}
	decodeJSON(t, rec, &created)
	if created.ID == "" {
		t.Fatal("created script bead has no id")
	}

	// The probe runner stamps exit_code + close_reason on the terminal close.
	closeRec := doJSON(t, h, "POST", "/v1/beads/"+created.ID+"/close", map[string]any{
		"closed_by":    "gardenio",
		"exit_code":    1,
		"close_reason": "probe exit 1",
	})
	if closeRec.Code == http.StatusBadRequest {
		t.Fatalf("closing type=script with exit_code/close_reason 400'd (regression): %s", closeRec.Body.String())
	}
}
