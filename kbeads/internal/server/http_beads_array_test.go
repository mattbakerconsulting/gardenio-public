package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"gardenio/kbeads/internal/model"
)

// arrayFieldOf decodes the string-array value of a bead field from the mock
// store — the assertion helper for the append/reorder handler tests.
func arrayFieldOf(t *testing.T, b *model.Bead, field string) []string {
	t.Helper()
	if b == nil {
		return nil
	}
	m := map[string]any{}
	if len(b.Fields) > 0 {
		_ = json.Unmarshal(b.Fields, &m)
	}
	raw, ok := m[field].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		out = append(out, v.(string))
	}
	return out
}

func TestHandleAppendBeadArrayField_Appends(t *testing.T) {
	_, ms, handler := newTestServer()
	ms.beads["gd-plan"] = &model.Bead{
		ID: "gd-plan", Type: model.TypeEpic, Status: model.StatusOpen,
		Fields: json.RawMessage(`{"phase_order":["gd-a"]}`),
	}

	rec := doJSON(t, handler, http.MethodPost, "/v1/beads/gd-plan/fields/phase_order/append",
		map[string]string{"element": "gd-b"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	got := arrayFieldOf(t, ms.beads["gd-plan"], "phase_order")
	if len(got) != 2 || got[0] != "gd-a" || got[1] != "gd-b" {
		t.Fatalf("phase_order = %v, want [gd-a gd-b]", got)
	}
}

func TestHandleAppendBeadArrayField_IdempotentNoOp(t *testing.T) {
	_, ms, handler := newTestServer()
	ms.beads["gd-plan"] = &model.Bead{
		ID: "gd-plan", Type: model.TypeEpic, Status: model.StatusOpen,
		Fields: json.RawMessage(`{"phase_order":["gd-a"]}`),
	}

	rec := doJSON(t, handler, http.MethodPost, "/v1/beads/gd-plan/fields/phase_order/append",
		map[string]string{"element": "gd-a"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	got := arrayFieldOf(t, ms.beads["gd-plan"], "phase_order")
	if len(got) != 1 {
		t.Fatalf("phase_order = %v, want unchanged [gd-a]", got)
	}
}

func TestHandleAppendBeadArrayField_EmptyElement400(t *testing.T) {
	_, ms, handler := newTestServer()
	ms.beads["gd-plan"] = &model.Bead{ID: "gd-plan", Type: model.TypeEpic, Status: model.StatusOpen}

	rec := doJSON(t, handler, http.MethodPost, "/v1/beads/gd-plan/fields/phase_order/append",
		map[string]string{"element": ""})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestHandleAppendBeadArrayField_InvalidField400(t *testing.T) {
	_, ms, handler := newTestServer()
	ms.beads["gd-plan"] = &model.Bead{ID: "gd-plan", Type: model.TypeEpic, Status: model.StatusOpen}

	// A non-identifier field key (contains a space) is rejected before the store.
	rec := doJSON(t, handler, http.MethodPost, "/v1/beads/gd-plan/fields/phase%20order/append",
		map[string]string{"element": "gd-b"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestHandleAppendBeadArrayField_NotFound404(t *testing.T) {
	_, _, handler := newTestServer()
	rec := doJSON(t, handler, http.MethodPost, "/v1/beads/gd-missing/fields/phase_order/append",
		map[string]string{"element": "gd-b"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestHandleReorderBeadArrayField_Swaps(t *testing.T) {
	_, ms, handler := newTestServer()
	ms.beads["gd-plan"] = &model.Bead{
		ID: "gd-plan", Type: model.TypeEpic, Status: model.StatusOpen,
		Fields: json.RawMessage(`{"phase_order":["gd-a","gd-b"]}`),
	}

	rec := doJSON(t, handler, http.MethodPost, "/v1/beads/gd-plan/fields/phase_order/reorder",
		map[string]any{"expected": []string{"gd-a", "gd-b"}, "next": []string{"gd-b", "gd-a"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	got := arrayFieldOf(t, ms.beads["gd-plan"], "phase_order")
	if len(got) != 2 || got[0] != "gd-b" || got[1] != "gd-a" {
		t.Fatalf("phase_order = %v, want [gd-b gd-a]", got)
	}
}

func TestHandleReorderBeadArrayField_StaleExpected409(t *testing.T) {
	_, ms, handler := newTestServer()
	// Current value has a 3rd element the reorder caller never saw — its
	// `expected` snapshot is stale, so the CAS must reject with 409.
	ms.beads["gd-plan"] = &model.Bead{
		ID: "gd-plan", Type: model.TypeEpic, Status: model.StatusOpen,
		Fields: json.RawMessage(`{"phase_order":["gd-a","gd-b","gd-c"]}`),
	}

	rec := doJSON(t, handler, http.MethodPost, "/v1/beads/gd-plan/fields/phase_order/reorder",
		map[string]any{"expected": []string{"gd-a", "gd-b"}, "next": []string{"gd-b", "gd-a"}})
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body = %s", rec.Code, rec.Body.String())
	}
	// The conflicting write must NOT have landed.
	got := arrayFieldOf(t, ms.beads["gd-plan"], "phase_order")
	if len(got) != 3 {
		t.Fatalf("phase_order = %v, want unchanged 3-element array", got)
	}
}

func TestHandleReorderBeadArrayField_NotFound404(t *testing.T) {
	_, _, handler := newTestServer()
	rec := doJSON(t, handler, http.MethodPost, "/v1/beads/gd-missing/fields/phase_order/reorder",
		map[string]any{"expected": []string{}, "next": []string{"gd-a"}})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}
