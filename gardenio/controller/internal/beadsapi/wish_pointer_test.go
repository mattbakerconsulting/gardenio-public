package beadsapi

import "testing"

// TestSetWishPointer_StampsBothFields is the core invariant: a non-empty head
// id writes BOTH the canonical wish-pointer field and its legacy alias to the
// SAME value. This is the property the six former open-coded call sites each had
// to uphold by hand; centralizing it here makes drift impossible.
func TestSetWishPointer_StampsBothFields(t *testing.T) {
	fields := map[string]string{}
	SetWishPointer(fields, "gd-W1sh")

	if got := fields[FieldThreadID]; got != "gd-W1sh" {
		t.Errorf("fields[%q] = %q, want %q", FieldThreadID, got, "gd-W1sh")
	}
	if got := fields[FieldSlackThreadBeadID]; got != "gd-W1sh" {
		t.Errorf("fields[%q] = %q, want %q", FieldSlackThreadBeadID, got, "gd-W1sh")
	}
	// Invariant restated as a relation: the alias always mirrors the canonical.
	if fields[FieldThreadID] != fields[FieldSlackThreadBeadID] {
		t.Errorf("alias/canonical drift: %q=%q vs %q=%q",
			FieldThreadID, fields[FieldThreadID],
			FieldSlackThreadBeadID, fields[FieldSlackThreadBeadID])
	}
}

// TestSetWishPointer_EmptyHeadIsNoop guards the head/unbound case: the thread
// head carries NO wish pointer (it IS the wish), and a bead not bound to a
// thread has no head id, so an empty head must leave the map untouched. This is
// what lets callers drop the `if headID != ""` guard.
func TestSetWishPointer_EmptyHeadIsNoop(t *testing.T) {
	fields := map[string]string{"existing": "keep"}
	SetWishPointer(fields, "")

	if _, ok := fields[FieldThreadID]; ok {
		t.Errorf("empty head stamped %q; want absent", FieldThreadID)
	}
	if _, ok := fields[FieldSlackThreadBeadID]; ok {
		t.Errorf("empty head stamped %q; want absent", FieldSlackThreadBeadID)
	}
	if got := fields["existing"]; got != "keep" {
		t.Errorf("unrelated field clobbered: fields[%q] = %q, want %q", "existing", got, "keep")
	}
}

// TestSetWishPointer_OverwritesStaleValue confirms a re-stamp with a new head id
// overwrites BOTH fields — neither field can be left carrying a stale pointer
// while the other advances.
func TestSetWishPointer_OverwritesStaleValue(t *testing.T) {
	fields := map[string]string{
		FieldThreadID:          "gd-old",
		FieldSlackThreadBeadID: "gd-old",
	}
	SetWishPointer(fields, "gd-new")

	if got := fields[FieldThreadID]; got != "gd-new" {
		t.Errorf("fields[%q] = %q, want %q", FieldThreadID, got, "gd-new")
	}
	if got := fields[FieldSlackThreadBeadID]; got != "gd-new" {
		t.Errorf("fields[%q] = %q, want %q", FieldSlackThreadBeadID, got, "gd-new")
	}
}

// TestSetWishPointer_DoesNotTouchOtherFields ensures the helper writes only the
// two wish-pointer keys and never disturbs sibling fields on the same map (the
// call sites pass a map already populated with operator/slack metadata).
func TestSetWishPointer_DoesNotTouchOtherFields(t *testing.T) {
	fields := map[string]string{
		"operated_by":          "slack:U123",
		"slack_thread_channel": "C123",
	}
	SetWishPointer(fields, "gd-W1sh")

	if got := fields["operated_by"]; got != "slack:U123" {
		t.Errorf("operated_by mutated: %q", got)
	}
	if got := fields["slack_thread_channel"]; got != "C123" {
		t.Errorf("slack_thread_channel mutated: %q", got)
	}
	if len(fields) != 4 {
		t.Errorf("field count = %d, want 4 (2 pre-existing + 2 wish-pointer)", len(fields))
	}
}
