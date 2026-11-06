package server

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestCreateBead_RedactsCredentialShapesInProse is the F17 (R4-SEC-4) end-to-end
// regression: a bare provider credential echoed into a bead's description/notes
// must be scrubbed before it lands in durable storage — while the structured
// `fields` JSON (which legitimately carries a litellm_key on account beads) is
// left byte-for-byte intact.
func TestCreateBead_RedactsCredentialShapesInProse(t *testing.T) {
	s, ms, _ := newTestServer()
	bead, err := s.createBead(ctxAsTestSubject(), createBeadInput{
		Title:       "leaked sk-ant-oat01-SECRETBODY123 in title",
		Type:        "task",
		Description: "the agent printed sk-ant-oat01-AAAABBBBCCCCdddd during the run",
		Notes:       "also vk-1234567890abcdef rode along",
		// gitlab_key is a valid string field on type=task and carries a
		// credential-shaped value — it must survive the prose scrubber untouched
		// (structured credential fields are the account-bead distribution path).
		Fields: json.RawMessage(`{"gitlab_key":"glpat-1234567890abcdefABCDEF"}`), // gitleaks:allow: fake token for the redaction test
	})
	if err != nil {
		t.Fatalf("createBead error: %v", err)
	}
	stored := ms.beads[bead.ID]
	if stored == nil {
		t.Fatalf("bead %s not persisted", bead.ID)
	}

	if strings.Contains(stored.Title, "SECRETBODY123") {
		t.Errorf("title leaked credential: %q", stored.Title)
	}
	if strings.Contains(stored.Description, "AAAABBBBCCCCdddd") {
		t.Errorf("description leaked credential: %q", stored.Description)
	}
	if strings.Contains(stored.Notes, "1234567890abcdef") {
		t.Errorf("notes leaked credential: %q", stored.Notes)
	}
	if !strings.Contains(stored.Description, "<redacted>") {
		t.Errorf("description missing redaction marker: %q", stored.Description)
	}

	// Fields MUST be untouched — redacting here would corrupt the account-bead
	// credential-distribution path.
	var m map[string]any
	if err := json.Unmarshal(stored.Fields, &m); err != nil {
		t.Fatalf("unmarshal stored fields: %v", err)
	}
	if m["gitlab_key"] != "glpat-1234567890abcdefABCDEF" { // gitleaks:allow: fake token for the redaction test
		t.Errorf("gitlab_key field was altered: %v (fields must not be redacted)", m["gitlab_key"])
	}
}

// TestUpdateBead_RedactsCredentialShapesInProse mirrors the create-path guard on
// the PATCH/update path: a `kd update --description/--notes` carrying a bare key
// is scrubbed before persistence.
func TestUpdateBead_RedactsCredentialShapesInProse(t *testing.T) {
	s, ms, _ := newTestServer()
	bead, err := s.createBead(ctxAsTestSubject(), createBeadInput{
		Title: "clean task", Type: "task",
	})
	if err != nil {
		t.Fatalf("createBead error: %v", err)
	}

	leakDesc := "rotated to ghp_aaaaBBBBccccDDDDeeeeFFFF1234 for the push"
	updated, err := s.updateBead(ctxAsTestSubject(), bead.ID, updateBeadInput{
		Description: &leakDesc,
	})
	if err != nil {
		t.Fatalf("updateBead error: %v", err)
	}
	if strings.Contains(updated.Description, "aaaaBBBBccccDDDD") {
		t.Errorf("updated description leaked credential: %q", updated.Description)
	}
	if !strings.Contains(updated.Description, "<redacted>") {
		t.Errorf("updated description missing redaction marker: %q", updated.Description)
	}
	stored := ms.beads[bead.ID]
	if stored == nil || strings.Contains(stored.Description, "aaaaBBBBccccDDDD") {
		t.Errorf("persisted description leaked credential: %+v", stored)
	}
}
