package server

import (
	"encoding/json"
	"strings"
	"testing"
)

// docFormat extracts the `format` field from a persisted bead's Fields JSON.
func docFormat(t *testing.T, fields json.RawMessage) string {
	t.Helper()
	if len(fields) == 0 {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal(fields, &m); err != nil {
		t.Fatalf("unmarshal fields %q: %v", string(fields), err)
	}
	s, _ := m["format"].(string)
	return s
}

// TestCreateBead_DocDefaultsFormatMd is the core regression for ka-0000006824:
// a type=doc bead created with NO format field must succeed (no "format: is
// required" 400) and persist format=md. Before this fix a bare
// `kd create x --type=doc` returned HTTP 400.
func TestCreateBead_DocDefaultsFormatMd(t *testing.T) {
	s, ms, _ := newTestServer()
	bead, err := s.createBead(ctxAsTestSubject(), createBeadInput{
		Title: "doc with no format", Type: "doc",
	})
	if err != nil {
		t.Fatalf("createBead(type=doc, no format) error: %v", err)
	}
	stored := ms.beads[bead.ID]
	if stored == nil {
		t.Fatalf("bead %s not persisted", bead.ID)
	}
	if got := docFormat(t, stored.Fields); got != "md" {
		t.Errorf("format = %q; want %q (default)", got, "md")
	}
}

// TestCreateBead_DocDefaultsFormatMd_WithOtherFields verifies the default is
// injected without clobbering caller-supplied sibling fields (headline,
// content) when format is the only missing required field.
func TestCreateBead_DocDefaultsFormatMd_WithOtherFields(t *testing.T) {
	s, ms, _ := newTestServer()
	bead, err := s.createBead(ctxAsTestSubject(), createBeadInput{
		Title:  "doc with content, no format",
		Type:   "doc",
		Fields: json.RawMessage(`{"headline":"Hi","content":"body"}`),
	})
	if err != nil {
		t.Fatalf("createBead error: %v", err)
	}
	stored := ms.beads[bead.ID]
	if stored == nil {
		t.Fatalf("bead %s not persisted", bead.ID)
	}
	if got := docFormat(t, stored.Fields); got != "md" {
		t.Errorf("format = %q; want %q (default)", got, "md")
	}
	var m map[string]any
	if err := json.Unmarshal(stored.Fields, &m); err != nil {
		t.Fatalf("unmarshal stored fields: %v", err)
	}
	if m["headline"] != "Hi" {
		t.Errorf("headline = %v; want Hi (sibling field clobbered)", m["headline"])
	}
	if m["content"] != "body" {
		t.Errorf("content = %v; want body (sibling field clobbered)", m["content"])
	}
}

// TestCreateBead_DocPreservesExplicitFormat ensures the default never masks an
// explicit (valid) format — e.g. format=diff must round-trip unchanged.
func TestCreateBead_DocPreservesExplicitFormat(t *testing.T) {
	s, ms, _ := newTestServer()
	bead, err := s.createBead(ctxAsTestSubject(), createBeadInput{
		Title:  "doc with explicit format",
		Type:   "doc",
		Fields: json.RawMessage(`{"format":"diff"}`),
	})
	if err != nil {
		t.Fatalf("createBead(format=diff) error: %v", err)
	}
	stored := ms.beads[bead.ID]
	if stored == nil {
		t.Fatalf("bead %s not persisted", bead.ID)
	}
	if got := docFormat(t, stored.Fields); got != "diff" {
		t.Errorf("format = %q; want %q (explicit value must be preserved)", got, "diff")
	}
}

// TestCreateBead_DocRejectsBadExplicitFormat ensures the default does NOT mask
// an invalid explicit value: format=markdown (not in the enum) must still 400
// with the "must be one of" enum error, exactly as before the fix.
func TestCreateBead_DocRejectsBadExplicitFormat(t *testing.T) {
	s, _, _ := newTestServer()
	_, err := s.createBead(ctxAsTestSubject(), createBeadInput{
		Title:  "doc with bad format",
		Type:   "doc",
		Fields: json.RawMessage(`{"format":"markdown"}`),
	})
	if err == nil {
		t.Fatal("expected inputError for format=markdown, got nil")
	}
	if !strings.Contains(err.Error(), "must be one of") {
		t.Errorf("error = %v; want enum 'must be one of' message", err)
	}
}

// TestCreateBead_DocEmptyFormatDefaults covers the explicit-empty-string case:
// `-f format=` should be treated as absent and default to md, not 400.
func TestCreateBead_DocEmptyFormatDefaults(t *testing.T) {
	s, ms, _ := newTestServer()
	bead, err := s.createBead(ctxAsTestSubject(), createBeadInput{
		Title:  "doc with empty format",
		Type:   "doc",
		Fields: json.RawMessage(`{"format":""}`),
	})
	if err != nil {
		t.Fatalf("createBead(format=\"\") error: %v", err)
	}
	stored := ms.beads[bead.ID]
	if stored == nil {
		t.Fatalf("bead %s not persisted", bead.ID)
	}
	if got := docFormat(t, stored.Fields); got != "md" {
		t.Errorf("format = %q; want %q (empty must default)", got, "md")
	}
}

// TestCreateBead_NonDocUnaffected guards the blast radius: a non-doc type that
// has no `format` field must not gain one from this default.
func TestCreateBead_NonDocUnaffected(t *testing.T) {
	s, ms, _ := newTestServer()
	bead, err := s.createBead(ctxAsTestSubject(), createBeadInput{
		Title: "ordinary task", Type: "task",
	})
	if err != nil {
		t.Fatalf("createBead(type=task) error: %v", err)
	}
	stored := ms.beads[bead.ID]
	if stored == nil {
		t.Fatalf("bead %s not persisted", bead.ID)
	}
	if got := docFormat(t, stored.Fields); got != "" {
		t.Errorf("task gained format = %q; want empty (non-doc must be untouched)", got)
	}
}

// bigDocBody mimics the 4.5KB markdown payload from the ka-0000002161 incident:
// enough prose to clear the description-payload mis-author threshold.
func bigDocBody() string {
	return "# FSM Triage Report Card\n\n" + strings.Repeat("| row | data | that is real markdown |\n", 30)
}

// TestCreateBead_DocRejectsBodyInDescription is the core regression for
// ka-0000003524: a type=doc whose body was written to -d/--description with NO
// fields.content must be REJECTED at write time (not silently accepted to render
// as a blank card on /bundle).
func TestCreateBead_DocRejectsBodyInDescription(t *testing.T) {
	s, _, _ := newTestServer()
	_, err := s.createBead(ctxAsTestSubject(), createBeadInput{
		Title:       "FSM Triage Report Card",
		Type:        "doc",
		Description: bigDocBody(),
	})
	if err == nil {
		t.Fatal("expected rejection for doc body in -d with no fields.content, got nil")
	}
	if !strings.Contains(err.Error(), "fields.content") {
		t.Errorf("error must name fields.content, got: %v", err)
	}
}

// TestCreateBead_DocRejectsBodyAfterTypedParentMention reproduces the exact
// incident shape: a leading "parent: gd-XXXX" typed mention followed by the body.
func TestCreateBead_DocRejectsBodyAfterTypedParentMention(t *testing.T) {
	s, _, _ := newTestServer()
	_, err := s.createBead(ctxAsTestSubject(), createBeadInput{
		Title:       "FSM Triage Report Card",
		Type:        "doc",
		Description: "parent: gd-9U5vctvVO7\n\n" + bigDocBody(),
	})
	if err == nil {
		t.Fatal("expected rejection when body follows typed parent mention, got nil")
	}
}

// TestCreateBead_DocAcceptsBodyInContent proves the correct authoring path still
// works: body in fields.content, even with a typed-mention description.
func TestCreateBead_DocAcceptsBodyInContent(t *testing.T) {
	s, ms, _ := newTestServer()
	body, _ := json.Marshal(map[string]any{"format": "md", "content": bigDocBody()})
	bead, err := s.createBead(ctxAsTestSubject(), createBeadInput{
		Title:       "FSM Triage Report Card",
		Type:        "doc",
		Description: "parent(gd-9U5vctvVO7)",
		Fields:      body,
	})
	if err != nil {
		t.Fatalf("createBead with body in fields.content should succeed, got %v", err)
	}
	if ms.beads[bead.ID] == nil {
		t.Fatalf("bead %s not persisted", bead.ID)
	}
}

// TestCreateBead_DocAcceptsBareParentMentionDescription proves the create-then-
// fill flow (gb bundle add writes a bare typed mention, no content yet) is not
// rejected.
func TestCreateBead_DocAcceptsBareParentMentionDescription(t *testing.T) {
	s, _, _ := newTestServer()
	_, err := s.createBead(ctxAsTestSubject(), createBeadInput{
		Title:       "placeholder doc",
		Type:        "doc",
		Description: "parent(gd-9U5vctvVO7)",
	})
	if err != nil {
		t.Fatalf("bare typed-mention description must be accepted, got %v", err)
	}
}

// TestUpdateBead_DocRejectsBodyMovedToDescription covers the description-only
// PATCH path: setting a doc's description to a body while it still has no content
// must be rejected, even though `fields` is untouched.
func TestUpdateBead_DocRejectsBodyMovedToDescription(t *testing.T) {
	s, _, _ := newTestServer()
	bead, err := s.createBead(ctxAsTestSubject(), createBeadInput{
		Title: "placeholder doc", Type: "doc",
	})
	if err != nil {
		t.Fatalf("seed createBead error: %v", err)
	}
	desc := bigDocBody()
	_, err = s.updateBead(ctxAsTestSubject(), bead.ID, updateBeadInput{Description: &desc})
	if err == nil {
		t.Fatal("expected rejection for description-only doc body PATCH, got nil")
	}
	if !strings.Contains(err.Error(), "fields.content") {
		t.Errorf("error must name fields.content, got: %v", err)
	}
}

// TestUpdateBead_DocContentFillFromFieldsSucceeds proves the documented fix path
// works: `kd update <doc> -f content=@file` on an existing blank doc.
func TestUpdateBead_DocContentFillFromFieldsSucceeds(t *testing.T) {
	s, _, _ := newTestServer()
	bead, err := s.createBead(ctxAsTestSubject(), createBeadInput{
		Title: "placeholder doc", Type: "doc",
	})
	if err != nil {
		t.Fatalf("seed createBead error: %v", err)
	}
	fill, _ := json.Marshal(map[string]any{"content": bigDocBody()})
	if _, err := s.updateBead(ctxAsTestSubject(), bead.ID, updateBeadInput{Fields: fill}); err != nil {
		t.Fatalf("filling fields.content on a blank doc must succeed, got %v", err)
	}
}

// TestUpdateBead_DocUnrelatedPatchOnLegacyBlankDocNotWedged guards the blast
// radius: a legacy blank doc carrying a big description must not be wedged by an
// unrelated PATCH (e.g. status) that does not touch the description.
func TestUpdateBead_DocUnrelatedPatchOnLegacyBlankDocNotWedged(t *testing.T) {
	s, ms, _ := newTestServer()
	// Seed a legacy blank doc directly in the store, bypassing the create guard,
	// to simulate a doc created before this fix landed.
	bead, err := s.createBead(ctxAsTestSubject(), createBeadInput{Title: "legacy doc", Type: "doc"})
	if err != nil {
		t.Fatalf("seed createBead error: %v", err)
	}
	ms.beads[bead.ID].Description = bigDocBody()
	// An unrelated PATCH (owner) must succeed — it does not write the description.
	owner := "someone"
	if _, err := s.updateBead(ctxAsTestSubject(), bead.ID, updateBeadInput{Owner: &owner}); err != nil {
		t.Fatalf("unrelated PATCH on legacy blank doc must not be wedged, got %v", err)
	}
}
