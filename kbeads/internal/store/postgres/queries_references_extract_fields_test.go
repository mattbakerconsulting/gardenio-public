package postgres

import (
	"encoding/json"
	"sort"
	"testing"
)

// TestExtractFieldsRefs_NewlinePrefixedID pins the fix for ka-0000007416: an
// id sitting immediately after a `\n` escape in the raw JSON encoding — a
// newline directly before the id in the operator's prose — must still be
// extracted. The pre-fix implementation ran the regex over the RAW JSON bytes,
// where such an id is preceded by the literal two bytes `\` `n`; the
// word-boundary anchor `\b` does not match between `n` and `k`, so the id was
// silently dropped. Extraction now runs over the DECODED string values.
func TestExtractFieldsRefs_NewlinePrefixedID(t *testing.T) {
	// The literal repro from the bead: four ids on two prose lines, the third
	// id newline-prefixed. All four must survive.
	body := "triggers (gd-jLmXPRaOxo, gd-mEIG7VGxJE,\ngd-ZoGO5BOH5n, gd-X0p8w3CBya)"
	fields, err := json.Marshal(map[string]any{"context": body})
	if err != nil {
		t.Fatalf("marshal fields: %v", err)
	}

	got := extractFieldsRefs(fields)
	want := []string{"gd-mEIG7VGxJE", "gd-jLmXPRaOxo", "gd-X0p8w3CBya", "gd-ZoGO5BOH5n"}
	if !equalSorted(got, want) {
		t.Fatalf("extractFieldsRefs dropped a newline-prefixed id:\n got:  %v\n want: %v", sortedCopy(got), want)
	}
}

// TestExtractFieldsRefs_NestedValues confirms the decoded walk still recurses
// into nested objects and arrays so ids in options[].label / prompt are
// covered — the whole-blob coverage the raw-bytes scan provided must not
// regress.
func TestExtractFieldsRefs_NestedValues(t *testing.T) {
	fields, err := json.Marshal(map[string]any{
		"prompt":  "which of gd-FNHTKRBDIQ wins?",
		"context": "parked behind\ngd-HKBGWAOTHB now",
		"options": []map[string]any{
			{"id": "a", "label": "bless gd-7YFI0RSI0U"},
			{"id": "b", "label": "reject\ngd-5VCLRIVK3G"},
		},
		"nested": map[string]any{"deep": []any{"gd-FA6DCNNHZL"}},
	})
	if err != nil {
		t.Fatalf("marshal fields: %v", err)
	}

	got := extractFieldsRefs(fields)
	want := []string{
		"gd-FNHTKRBDIQ", "gd-HKBGWAOTHB", "gd-7YFI0RSI0U",
		"gd-5VCLRIVK3G", "gd-FA6DCNNHZL",
	}
	if !equalSorted(got, want) {
		t.Fatalf("extractFieldsRefs missed a nested id:\n got:  %v\n want: %v", sortedCopy(got), want)
	}
}

// TestExtractFieldsRefs_MalformedFallsBackToRaw confirms a non-JSON blob
// degrades to the historical raw-bytes scan rather than losing every ref.
func TestExtractFieldsRefs_MalformedFallsBackToRaw(t *testing.T) {
	raw := []byte("not json but names gd-FNHTKRBDIQ here")
	got := extractFieldsRefs(raw)
	if len(got) != 1 || got[0] != "gd-FNHTKRBDIQ" {
		t.Fatalf("malformed-blob fallback failed: got %v, want [gd-FNHTKRBDIQ]", got)
	}
}

// TestExtractFieldsRefs_Empty confirms empty/nil fields yields no refs.
func TestExtractFieldsRefs_Empty(t *testing.T) {
	if got := extractFieldsRefs(nil); got != nil {
		t.Fatalf("nil fields: got %v, want nil", got)
	}
	if got := extractFieldsRefs([]byte{}); got != nil {
		t.Fatalf("empty fields: got %v, want nil", got)
	}
}

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

func equalSorted(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	as, bs := sortedCopy(a), sortedCopy(b)
	for i := range as {
		if as[i] != bs[i] {
			return false
		}
	}
	return true
}

// TestExtractFieldsRefs_SkipsReadCursors: a person's read_cursors map holds
// message ids, and as references it would put them on each message's
// mentioned-by. Other keys on the same bead still yield their refs.
func TestExtractFieldsRefs_SkipsReadCursors(t *testing.T) {
	fields, err := json.Marshal(map[string]any{
		"read_cursors":   map[string]any{"gd-ch1aaaaaaa": "gd-msg1aaaaaa", "gd-ch2aaaaaaa": "gd-msg2aaaaaa"},
		"status_message": "see gd-FNHTKRBDIQ",
	})
	if err != nil {
		t.Fatalf("marshal fields: %v", err)
	}
	got := extractFieldsRefs(fields)
	if want := []string{"gd-FNHTKRBDIQ"}; !equalSorted(got, want) {
		t.Fatalf("extractFieldsRefs = %v, want %v", sortedCopy(got), want)
	}
}
