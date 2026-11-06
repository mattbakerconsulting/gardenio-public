package bridge

import (
	"log/slog"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// TestBridgeActivityTypeValid pins the bounded activity_type enum on the
// bridge side. Mirrors the kbeads-side enum so the shared metric never
// disagrees on membership.
func TestBridgeActivityTypeValid(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{activityTypeSpawn, true},
		{activityTypeMutation, true},
		{activityTypeSession, true},
		{activityTypeView, true},
		{"", false},
		{"SPAWN", false},
		{"spawns", false},
		{"view-only", false},
	}
	for _, tc := range cases {
		if got := activityTypeValid(tc.in); got != tc.want {
			t.Errorf("activityTypeValid(%q) = %v; want %v", tc.in, got, tc.want)
		}
	}
}

// TestRecordOperatorActivity asserts the bridge stamps the full label set on a
// valid spawn emit, applies the empty-operator→"0" / empty-project→"unknown"
// sentinels, and rejects an out-of-enum activity_type without incrementing.
func TestRecordOperatorActivity(t *testing.T) {
	operatorActivityTotal.Reset()
	b := &Bot{logger: slog.Default()}

	// Valid spawn, attributed operator, explicit project.
	if !b.recordOperatorActivity("slack:U123", "gardenio", activityTypeSpawn, surfaceSlack) {
		t.Fatal("recordOperatorActivity returned false for a valid activity_type")
	}
	if got := testutil.ToFloat64(operatorActivityTotal.WithLabelValues(
		"slack:U123", "gardenio", activityTypeSpawn, surfaceSlack)); got != 1 {
		t.Errorf("counter for (slack:U123,gardenio,spawn,slack) = %v; want 1", got)
	}

	// Empty operator + empty project → "0" / "unknown" sentinels.
	if !b.recordOperatorActivity("", "", activityTypeSpawn, surfaceSlack) {
		t.Fatal("recordOperatorActivity returned false for empty-operator valid activity")
	}
	if got := testutil.ToFloat64(operatorActivityTotal.WithLabelValues(
		"0", "unknown", activityTypeSpawn, surfaceSlack)); got != 1 {
		t.Errorf("counter for (0,unknown,spawn,slack) = %v; want 1", got)
	}

	// Out-of-enum activity_type is rejected: no increment, returns false.
	if b.recordOperatorActivity("slack:U123", "gardenio", "bogus", surfaceSlack) {
		t.Error("recordOperatorActivity accepted an out-of-enum activity_type")
	}
	if total := testutil.CollectAndCount(operatorActivityTotal); total != 2 {
		t.Errorf("series count after rejected emit = %d; want 2 (no bogus series)", total)
	}
}
