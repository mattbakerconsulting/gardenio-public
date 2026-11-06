package server

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// TestActivityTypeValid pins the bounded activity_type enum. The set is the
// single guard against label-cardinality blowup, so any drift in membership
// must be a deliberate edit to activityTypeValid (and this table).
func TestActivityTypeValid(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{activityTypeSpawn, true},
		{activityTypeMutation, true},
		{activityTypeSession, true},
		{activityTypeView, true},
		{"", false},
		{"SPAWN", false},      // case-sensitive
		{"mutations", false},  // near-miss
		{"delete", false},     // plausible-but-out-of-set
		{"slack:U123", false}, // operator-shaped junk
	}
	for _, tc := range cases {
		if got := activityTypeValid(tc.in); got != tc.want {
			t.Errorf("activityTypeValid(%q) = %v; want %v", tc.in, got, tc.want)
		}
	}
}

// TestOperatorIDLabel covers the NULL→"0" sentinel and ordinary FK rendering.
func TestOperatorIDLabel(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0"}, // NULL / unattributable operator
		{1, "1"},
		{42, "42"},
	}
	for _, tc := range cases {
		if got := operatorIDLabel(tc.in); got != tc.want {
			t.Errorf("operatorIDLabel(%d) = %q; want %q", tc.in, got, tc.want)
		}
	}
}

// TestRecordOperatorActivity asserts the full label set is stamped on a valid
// emit, that an out-of-enum activity_type is rejected (no increment), and that
// the NULL-operator / empty-project sentinels are applied.
func TestRecordOperatorActivity(t *testing.T) {
	operatorActivityTotal.Reset()

	// Valid mutation, attributed operator, explicit project.
	if !recordOperatorActivity(7, "gardenio", activityTypeMutation, surfaceKbeads) {
		t.Fatal("recordOperatorActivity returned false for a valid activity_type")
	}
	if got := testutil.ToFloat64(operatorActivityTotal.WithLabelValues(
		"7", "gardenio", activityTypeMutation, surfaceKbeads)); got != 1 {
		t.Errorf("counter for (7,gardenio,mutation,kbeads) = %v; want 1", got)
	}

	// NULL operator (id 0) + empty project → "0" / "unknown" sentinels.
	if !recordOperatorActivity(0, "", activityTypeMutation, surfaceKbeads) {
		t.Fatal("recordOperatorActivity returned false for null-operator valid activity")
	}
	if got := testutil.ToFloat64(operatorActivityTotal.WithLabelValues(
		"0", "unknown", activityTypeMutation, surfaceKbeads)); got != 1 {
		t.Errorf("counter for (0,unknown,mutation,kbeads) = %v; want 1", got)
	}

	// Out-of-enum activity_type is rejected: no increment, returns false.
	if recordOperatorActivity(7, "gardenio", "bogus", surfaceKbeads) {
		t.Error("recordOperatorActivity accepted an out-of-enum activity_type")
	}
	if total := testutil.CollectAndCount(operatorActivityTotal); total != 2 {
		t.Errorf("series count after rejected emit = %d; want 2 (no bogus series)", total)
	}
}
