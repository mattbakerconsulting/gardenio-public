package server

import (
	"bytes"
	"encoding/json"
	"reflect"
)

// fieldsDelta compares two Fields blobs key by key and returns the pair of RFC
// 7386 merge patches between them: forward turns before into after, reverse
// turns after back into before. Each holds only the top-level keys that
// changed. A key the update added is null in reverse, and a key it removed is
// null in forward. ok is false when either side is not a JSON object (or is
// unparseable), so the caller can fall back to the whole blob.
//
// This is what mutation_history and the persisted bead.updated event store
// for a fields update (gd-1F8R0SQtiQ). A trigger's fields blob carries its
// whole prompt, and a run only bumps a few counters. Storing the full blob
// twice per update made history about 96% of the beads database.
func fieldsDelta(before, after []byte) (forward, reverse map[string]any, ok bool) {
	oldF, ok1 := decodeFieldsObject(before)
	newF, ok2 := decodeFieldsObject(after)
	if !ok1 || !ok2 {
		return nil, nil, false
	}
	forward, reverse = map[string]any{}, map[string]any{}
	for k, nv := range newF {
		ov, had := oldF[k]
		if had && reflect.DeepEqual(ov, nv) {
			continue
		}
		forward[k] = nv
		reverse[k] = ov // nil when the key is new
	}
	for k, ov := range oldF {
		if _, has := newF[k]; !has {
			forward[k] = nil
			reverse[k] = ov
		}
	}
	return forward, reverse, true
}

// decodeFieldsObject decodes a Fields blob into a map, keeping numbers as
// json.Number so a re-marshal never rounds a large integer. An empty blob
// is an empty object.
func decodeFieldsObject(raw []byte) (map[string]any, bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return map[string]any{}, true
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil || m == nil {
		return nil, false
	}
	return m, true
}

// fieldsBytes returns the raw JSON of a Fields value as recordUpdates sees
// it: the pre-update snapshot is a string, the post-update one a
// json.RawMessage.
func fieldsBytes(v any) ([]byte, bool) {
	switch t := v.(type) {
	case nil:
		return nil, true
	case string:
		return []byte(t), true
	case json.RawMessage:
		return t, true
	case []byte:
		return t, true
	}
	return nil, false
}

// fieldsHistoryValues renders the old_value and new_value of a field='fields'
// mutation_history row as the reverse and forward merge patches of the
// update. changed is false when no key changed, so the caller writes no row.
// When either side is not a JSON object it falls back to the whole values,
// with the new value as JSON text rather than fmt's byte-list rendering of a
// json.RawMessage.
func fieldsHistoryValues(oldVal, newVal any) (oldStr, newStr string, changed bool) {
	ob, okO := fieldsBytes(oldVal)
	nb, okN := fieldsBytes(newVal)
	if okO && okN {
		if forward, reverse, ok := fieldsDelta(ob, nb); ok {
			if len(forward) == 0 {
				return "", "", false
			}
			return marshalDelta(reverse), marshalDelta(forward), true
		}
		return string(ob), string(nb), true
	}
	return fmtValue(oldVal), fmtValue(newVal), true
}

func marshalDelta(m map[string]any) string {
	b, err := json.Marshal(m)
	if err != nil {
		return ""
	}
	return string(b)
}
