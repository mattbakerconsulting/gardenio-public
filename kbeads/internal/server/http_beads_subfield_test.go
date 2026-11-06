package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"gardenio/kbeads/internal/model"
)

// mapFieldOf decodes the string→string map value of a bead field from the mock
// store — the assertion helper for the sub-field map-edit handler tests.
func mapFieldOf(t *testing.T, b *model.Bead, field string) map[string]string {
	t.Helper()
	out := map[string]string{}
	if b == nil {
		return out
	}
	m := map[string]json.RawMessage{}
	if len(b.Fields) > 0 {
		_ = json.Unmarshal(b.Fields, &m)
	}
	raw, ok := m[field]
	if !ok {
		return out
	}
	_ = json.Unmarshal(raw, &out)
	return out
}

// csvFieldOf decodes the CSV-string value of a bead field into its trimmed
// elements.
func csvFieldOf(t *testing.T, b *model.Bead, field string) []string {
	t.Helper()
	if b == nil {
		return nil
	}
	m := map[string]json.RawMessage{}
	if len(b.Fields) > 0 {
		_ = json.Unmarshal(b.Fields, &m)
	}
	raw, ok := m[field]
	if !ok {
		return nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil
	}
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// TestHandleEditBeadSubField_MapSetPreservesOtherKeys is the headline
// acceptance case at the HTTP layer: setting ONE key of channel_roles leaves the
// other keys intact.
func TestHandleEditBeadSubField_MapSetPreservesOtherKeys(t *testing.T) {
	_, ms, handler := newTestServer()
	ms.beads["gd-proj"] = &model.Bead{
		ID: "gd-proj", Type: model.TypeProject, Status: model.StatusOpen,
		Fields: json.RawMessage(`{"channel_roles":{"C1":"ops"},"prefix":"gb"}`),
	}

	rec := doJSON(t, handler, http.MethodPost, "/v1/beads/gd-proj/fields/channel_roles/edit",
		map[string]string{"op": "set", "key": "C2", "value": "crew"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	got := mapFieldOf(t, ms.beads["gd-proj"], "channel_roles")
	if got["C1"] != "ops" || got["C2"] != "crew" {
		t.Fatalf("channel_roles = %v, want C1=ops C2=crew", got)
	}
}

// TestHandleEditBeadSubField_ClobberContrast is the clobber REPRODUCTION: a bare
// whole-field PATCH replaces channel_roles (dropping C1), whereas the sub-field
// edit preserves it. Both run against the same seeded bead so the contrast is
// direct.
func TestHandleEditBeadSubField_ClobberContrast(t *testing.T) {
	// (a) Whole-field PATCH clobbers: setting channel_roles={"C2":"crew"} drops C1.
	_, ms, handler := newTestServer()
	ms.beads["gd-proj"] = &model.Bead{
		ID: "gd-proj", Kind: model.KindConfig, Type: model.TypeProject,
		Title: "Project: gardenio", Status: model.StatusOpen,
		Fields: json.RawMessage(`{"channel_roles":{"C1":"ops"}}`),
	}
	rec := doJSON(t, handler, http.MethodPatch, "/v1/beads/gd-proj",
		map[string]any{"fields": map[string]any{"channel_roles": map[string]string{"C2": "crew"}}})
	if rec.Code != http.StatusOK {
		t.Fatalf("patch status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := mapFieldOf(t, ms.beads["gd-proj"], "channel_roles"); got["C1"] != "" || got["C2"] != "crew" {
		t.Fatalf("PATCH channel_roles = %v; expected the clobber (C1 gone, only C2)", got)
	}

	// (b) Sub-field edit preserves: on a fresh bead, --field-set adds C2 without
	// dropping C1.
	_, ms2, handler2 := newTestServer()
	ms2.beads["gd-proj"] = &model.Bead{
		ID: "gd-proj", Type: model.TypeProject, Status: model.StatusOpen,
		Fields: json.RawMessage(`{"channel_roles":{"C1":"ops"}}`),
	}
	rec2 := doJSON(t, handler2, http.MethodPost, "/v1/beads/gd-proj/fields/channel_roles/edit",
		map[string]string{"op": "set", "key": "C2", "value": "crew"})
	if rec2.Code != http.StatusOK {
		t.Fatalf("edit status = %d, body = %s", rec2.Code, rec2.Body.String())
	}
	if got := mapFieldOf(t, ms2.beads["gd-proj"], "channel_roles"); got["C1"] != "ops" || got["C2"] != "crew" {
		t.Fatalf("edit channel_roles = %v; expected BOTH C1 and C2", got)
	}
}

// TestHandleEditBeadSubField_MapUnsetKey verifies unset-key drops only the named
// key.
func TestHandleEditBeadSubField_MapUnsetKey(t *testing.T) {
	_, ms, handler := newTestServer()
	ms.beads["gd-proj"] = &model.Bead{
		ID: "gd-proj", Type: model.TypeProject, Status: model.StatusOpen,
		Fields: json.RawMessage(`{"channel_roles":{"C1":"ops","C2":"crew"}}`),
	}

	rec := doJSON(t, handler, http.MethodPost, "/v1/beads/gd-proj/fields/channel_roles/edit",
		map[string]string{"op": "unset-key", "key": "C1"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	got := mapFieldOf(t, ms.beads["gd-proj"], "channel_roles")
	if _, ok := got["C1"]; ok || got["C2"] != "crew" {
		t.Fatalf("channel_roles = %v, want only C2=crew", got)
	}
}

// TestHandleEditBeadSubField_CSVAppendPreservesExisting is the CSV acceptance
// case at the HTTP layer.
func TestHandleEditBeadSubField_CSVAppendPreservesExisting(t *testing.T) {
	_, ms, handler := newTestServer()
	ms.beads["gd-proj"] = &model.Bead{
		ID: "gd-proj", Type: model.TypeProject, Status: model.StatusOpen,
		Fields: json.RawMessage(`{"slack_channel":"C1,C2"}`),
	}

	rec := doJSON(t, handler, http.MethodPost, "/v1/beads/gd-proj/fields/slack_channel/edit",
		map[string]string{"op": "append", "element": "C3"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	got := csvFieldOf(t, ms.beads["gd-proj"], "slack_channel")
	if len(got) != 3 {
		t.Fatalf("slack_channel = %v, want 3 entries", got)
	}
}

// TestHandleEditBeadSubField_CSVRemove verifies remove drops only the target.
func TestHandleEditBeadSubField_CSVRemove(t *testing.T) {
	_, ms, handler := newTestServer()
	ms.beads["gd-proj"] = &model.Bead{
		ID: "gd-proj", Type: model.TypeProject, Status: model.StatusOpen,
		Fields: json.RawMessage(`{"slack_channel":"C1,C2,C3"}`),
	}

	rec := doJSON(t, handler, http.MethodPost, "/v1/beads/gd-proj/fields/slack_channel/edit",
		map[string]string{"op": "remove", "element": "C2"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	got := csvFieldOf(t, ms.beads["gd-proj"], "slack_channel")
	if len(got) != 2 {
		t.Fatalf("slack_channel = %v, want [C1 C3]", got)
	}
	for _, e := range got {
		if e == "C2" {
			t.Fatalf("slack_channel still has C2: %v", got)
		}
	}
}

func TestHandleEditBeadSubField_UnknownOp400(t *testing.T) {
	_, ms, handler := newTestServer()
	ms.beads["gd-proj"] = &model.Bead{ID: "gd-proj", Type: model.TypeProject, Status: model.StatusOpen}

	rec := doJSON(t, handler, http.MethodPost, "/v1/beads/gd-proj/fields/channel_roles/edit",
		map[string]string{"op": "frobnicate", "key": "C1", "value": "x"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestHandleEditBeadSubField_CommaElement400(t *testing.T) {
	_, ms, handler := newTestServer()
	ms.beads["gd-proj"] = &model.Bead{ID: "gd-proj", Type: model.TypeProject, Status: model.StatusOpen}

	rec := doJSON(t, handler, http.MethodPost, "/v1/beads/gd-proj/fields/slack_channel/edit",
		map[string]string{"op": "append", "element": "C1,C2"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for comma element", rec.Code)
	}
}

func TestHandleEditBeadSubField_InvalidField400(t *testing.T) {
	_, ms, handler := newTestServer()
	ms.beads["gd-proj"] = &model.Bead{ID: "gd-proj", Type: model.TypeProject, Status: model.StatusOpen}

	rec := doJSON(t, handler, http.MethodPost, "/v1/beads/gd-proj/fields/channel%20roles/edit",
		map[string]string{"op": "set", "key": "C1", "value": "x"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestHandleEditBeadSubField_NotFound404(t *testing.T) {
	_, _, handler := newTestServer()
	rec := doJSON(t, handler, http.MethodPost, "/v1/beads/gd-missing/fields/channel_roles/edit",
		map[string]string{"op": "set", "key": "C1", "value": "x"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}
