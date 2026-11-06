package server

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	beadsv1 "gardenio/kbeads/gen/beads/v1"
	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
)

// applyMergePatch applies an RFC 7386 top-level merge patch to doc.
func applyMergePatch(t *testing.T, doc []byte, patch map[string]any) map[string]any {
	t.Helper()
	out, ok := decodeFieldsObject(doc)
	if !ok {
		t.Fatalf("decode %s", doc)
	}
	for k, v := range patch {
		if v == nil {
			delete(out, k)
			continue
		}
		out[k] = v
	}
	return out
}

func TestFieldsDelta_RoundTrip(t *testing.T) {
	before := []byte(`{"prompt": "a long prompt", "attempts": 41, "last_note": "t1", "gone": "x", "nested": {"a": 1}}`)
	after := []byte(`{"prompt":"a long prompt","attempts":42,"last_note":"t2","added":true,"nested":{"a":1}}`)

	forward, reverse, ok := fieldsDelta(before, after)
	if !ok {
		t.Fatal("fieldsDelta: not ok")
	}
	wantKeys := []string{"added", "gone", "last_note", "attempts"}
	for _, m := range []map[string]any{forward, reverse} {
		if len(m) != len(wantKeys) {
			t.Fatalf("delta keys = %v, want %v", m, wantKeys)
		}
		for _, k := range wantKeys {
			if _, has := m[k]; !has {
				t.Errorf("delta missing %q: %v", k, m)
			}
		}
	}
	if forward["gone"] != nil || reverse["added"] != nil {
		t.Errorf("removed key must be null forward and added key null reverse: forward=%v reverse=%v", forward, reverse)
	}

	wantAfter, _ := decodeFieldsObject(after)
	if got := applyMergePatch(t, before, forward); !reflect.DeepEqual(got, wantAfter) {
		t.Errorf("before+forward = %v, want %v", got, wantAfter)
	}
	wantBefore, _ := decodeFieldsObject(before)
	if got := applyMergePatch(t, after, reverse); !reflect.DeepEqual(got, wantBefore) {
		t.Errorf("after+reverse = %v, want %v", got, wantBefore)
	}
}

func TestFieldsDelta_NotAnObject(t *testing.T) {
	if _, _, ok := fieldsDelta([]byte(`[1,2]`), []byte(`{}`)); ok {
		t.Error("array before: want ok=false")
	}
	if _, _, ok := fieldsDelta([]byte(`{}`), []byte(`not json`)); ok {
		t.Error("garbage after: want ok=false")
	}
	if f, r, ok := fieldsDelta(nil, []byte(`{"a":"b"}`)); !ok || f["a"] != "b" || len(r) != 1 || r["a"] != nil {
		t.Errorf("empty before: forward=%v reverse=%v ok=%v", f, r, ok)
	}
}

func TestFieldsHistoryValues(t *testing.T) {
	oldStr, newStr, changed := fieldsHistoryValues(`{"prompt":"p","n":1}`, json.RawMessage(`{"prompt":"p","n":2}`))
	if !changed {
		t.Fatal("changed = false, want true")
	}
	if oldStr != `{"n":1}` || newStr != `{"n":2}` {
		t.Errorf("history values = %q -> %q, want {\"n\":1} -> {\"n\":2}", oldStr, newStr)
	}
	// JSONB renders with spaces; Go marshals without. Same keys and values is
	// no change, so recordUpdates writes no row.
	if _, _, changed := fieldsHistoryValues(`{"a": 1, "b": {"c": "d"}}`, json.RawMessage(`{"a":1,"b":{"c":"d"}}`)); changed {
		t.Error("reformatted blob: changed = true, want false")
	}
	// A non-object falls back to the whole values, as JSON text (not fmt's
	// byte-list rendering of a json.RawMessage).
	if o, n, _ := fieldsHistoryValues(`[1]`, json.RawMessage(`[2]`)); o != `[1]` || n != `[2]` {
		t.Errorf("fallback = %q -> %q, want [1] -> [2]", o, n)
	}
}

// TestUpdateBead_FieldsHistoryIsDelta: a fields patch on a trigger writes one
// field='fields' mutation_history row holding only the changed keys, and the
// persisted bead.updated event drops bead.fields and carries the same delta.
func TestUpdateBead_FieldsHistoryIsDelta(t *testing.T) {
	srv, ms, ctx := testCtx(t)
	const id = "gd-trg1"
	prompt := strings.Repeat("a long trigger prompt ", 200)
	ms.beads[id] = &model.Bead{
		ID:        id,
		Title:     "a trigger",
		Kind:      model.KindConfig,
		Type:      "trigger",
		Status:    model.StatusOpen,
		CreatedBy: defaultTestSubject,
		Fields:    json.RawMessage(`{"action":"notify","prompt":"` + prompt + `","attempts":41,"last_note":"t1"}`),
	}

	patch := json.RawMessage(`{"attempts":42,"last_note":"t2"}`)
	if _, err := srv.UpdateBead(ctx, &beadsv1.UpdateBeadRequest{Id: id, Fields: patch}); err != nil {
		t.Fatalf("UpdateBead: %v", err)
	}

	var row *model.MutationRow
	for _, m := range ms.mutations {
		if m.BeadID == id && m.Field == "fields" {
			if row != nil {
				t.Fatalf("more than one fields row: %+v and %+v", row, m)
			}
			row = m
		}
	}
	if row == nil {
		t.Fatal("no field='fields' mutation row recorded")
	}
	if row.OldValue != `{"attempts":41,"last_note":"t1"}` || row.NewValue != `{"attempts":42,"last_note":"t2"}` {
		t.Errorf("fields row = %q -> %q, want only the two changed keys", row.OldValue, row.NewValue)
	}

	var ev *model.Event
	for _, e := range ms.events {
		if e.BeadID == id && e.Topic == events.TopicBeadUpdated {
			ev = e
		}
	}
	if ev == nil {
		t.Fatal("no bead.updated event recorded")
	}
	if strings.Contains(string(ev.Payload), "a long trigger prompt") {
		t.Errorf("persisted payload still carries the prompt (%d bytes)", len(ev.Payload))
	}
	var got struct {
		Bead struct {
			ID     string          `json:"id"`
			Type   string          `json:"type"`
			Fields json.RawMessage `json:"fields"`
		} `json:"bead"`
		Changes       map[string]any `json:"changes"`
		FieldsOld     map[string]any `json:"fields_old"`
		FieldsOmitted bool           `json:"fields_omitted"`
	}
	if err := json.Unmarshal(ev.Payload, &got); err != nil {
		t.Fatalf("decode persisted payload: %v", err)
	}
	if got.Bead.ID != id || got.Bead.Type != "trigger" || got.Bead.Fields != nil || !got.FieldsOmitted {
		t.Errorf("persisted bead = %+v fields_omitted=%v, want id/type kept, fields dropped", got.Bead, got.FieldsOmitted)
	}
	wantFwd := map[string]any{"last_note": "t2", "attempts": float64(42)}
	if !reflect.DeepEqual(got.Changes["fields"], wantFwd) {
		t.Errorf("changes.fields = %v, want %v", got.Changes["fields"], wantFwd)
	}
	if got.Changes["attempts"] != float64(42) {
		t.Errorf("per-key overlay lost: changes = %v", got.Changes)
	}
	wantOld := map[string]any{"last_note": "t1", "attempts": float64(41)}
	if !reflect.DeepEqual(got.FieldsOld, wantOld) {
		t.Errorf("fields_old = %v, want %v", got.FieldsOld, wantOld)
	}
}

// TestPersistedEventPayload_Scope: only a trigger or agent bead.updated event
// is slimmed. Other types and topics persist the live payload byte for byte,
// and the live event itself is never changed.
func TestPersistedEventPayload_Scope(t *testing.T) {
	fields := json.RawMessage(`{"prompt":"p","n":2}`)
	mk := func(typ model.BeadType) events.BeadUpdated {
		return events.BeadUpdated{
			Bead:         &model.Bead{ID: "gd-x", Type: typ, Fields: fields},
			Changes:      map[string]any{"fields": fields},
			FieldsBefore: json.RawMessage(`{"prompt":"p","n":1}`),
		}
	}
	for _, tc := range []struct {
		name  string
		topic string
		typ   model.BeadType
		slim  bool
	}{
		{"trigger updated", events.TopicBeadUpdated, "trigger", true},
		{"agent updated", events.TopicBeadUpdated, "agent", true},
		{"task updated", events.TopicBeadUpdated, model.TypeTask, false},
		{"decision updated", events.TopicBeadUpdated, "decision", false},
		{"trigger closed", events.TopicBeadClosed, "trigger", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ev := mk(tc.typ)
			full, _ := json.Marshal(ev)
			got := persistedEventPayload(tc.topic, ev, full)
			if !tc.slim {
				if string(got) != string(full) {
					t.Errorf("payload changed: %s, want %s", got, full)
				}
				return
			}
			if string(got) == string(full) || strings.Contains(string(got), `"prompt"`) {
				t.Errorf("not slimmed: %s", got)
			}
			if string(ev.Bead.Fields) != string(fields) {
				t.Errorf("live bead mutated: fields = %s", ev.Bead.Fields)
			}
			if !strings.Contains(string(full), `"prompt"`) || strings.Contains(string(full), "fields_omitted") {
				t.Errorf("live payload changed: %s", full)
			}
		})
	}
}

// TestPersistedEventPayload_NoFieldsBefore: an emitter without the prior
// fields (an IncrementBeadFields write) still drops bead.fields from a
// trigger's persisted event and keeps whatever changes it published.
func TestPersistedEventPayload_NoFieldsBefore(t *testing.T) {
	ev := events.BeadUpdated{Bead: &model.Bead{ID: "gd-x", Type: "agent", Status: model.StatusOpen, Fields: json.RawMessage(`{"agent_state":"working"}`)}}
	full, _ := json.Marshal(ev)
	got := persistedEventPayload(events.TopicBeadUpdated, ev, full)
	var decoded events.BeadUpdated
	if err := json.Unmarshal(got, &decoded); err != nil {
		t.Fatalf("slim payload must still decode as BeadUpdated: %v", err)
	}
	if decoded.Bead == nil || decoded.Bead.ID != "gd-x" || decoded.Bead.Status != model.StatusOpen || decoded.Bead.Fields != nil {
		t.Errorf("decoded bead = %+v", decoded.Bead)
	}
}
