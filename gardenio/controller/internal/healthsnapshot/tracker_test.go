package healthsnapshot

import (
	"errors"
	"testing"
	"time"
)

func TestTracker_NilSafe(t *testing.T) {
	var tr *Tracker
	tr.RecordReap(1, 1)
	tr.RecordRotation("rotated", "p", "a", "b", "")
	tr.RecordReconcile(time.Now(), time.Now(), nil)
	if got := tr.RecentReaps(); got != nil {
		t.Errorf("nil tracker RecentReaps = %v, want nil", got)
	}
	if got := tr.RecentRotations(); got != nil {
		t.Errorf("nil tracker RecentRotations = %v, want nil", got)
	}
	if got := tr.LastReconcile(); got != nil {
		t.Errorf("nil tracker LastReconcile = %v, want nil", got)
	}
	if got := tr.LastSuccessfulReconcile(); got != nil {
		t.Errorf("nil tracker LastSuccessfulReconcile = %v, want nil", got)
	}
}

func TestTracker_RecordReap_Caps(t *testing.T) {
	tr := NewTracker()
	for i := range MaxRecentReaps + 5 {
		tr.RecordReap(i, i%3)
	}
	got := tr.RecentReaps()
	if len(got) != MaxRecentReaps {
		t.Fatalf("len = %d, want %d", len(got), MaxRecentReaps)
	}
	// Oldest dropped: first kept entry should have Checked == 5.
	if got[0].Checked != 5 {
		t.Errorf("oldest kept Checked = %d, want 5", got[0].Checked)
	}
}

func TestTracker_RecordRotation_FiltersEmpty(t *testing.T) {
	tr := NewTracker()
	tr.RecordRotation("", "p", "a", "b", "noop")
	if got := tr.RecentRotations(); len(got) != 0 {
		t.Errorf("empty event was recorded: %v", got)
	}
	tr.RecordRotation("rotated", "p", "a", "b", "rate_limited")
	if got := tr.RecentRotations(); len(got) != 1 || got[0].Event != "rotated" {
		t.Errorf("populated rotation not recorded correctly: %v", got)
	}
}

func TestTracker_RecordReconcile_TracksOKAndError(t *testing.T) {
	tr := NewTracker()
	t0 := time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC)
	t1 := t0.Add(50 * time.Millisecond)
	tr.RecordReconcile(t0, t1, nil)

	last := tr.LastReconcile()
	if last == nil || last.DurationMs != 50 || last.Error != "" {
		t.Fatalf("ok pass not recorded as last: %+v", last)
	}
	if ok := tr.LastSuccessfulReconcile(); ok == nil || ok.DurationMs != 50 {
		t.Fatalf("ok pass not recorded as lastOK: %+v", ok)
	}

	t2 := t1.Add(time.Second)
	t3 := t2.Add(100 * time.Millisecond)
	tr.RecordReconcile(t2, t3, errors.New("boom"))
	last = tr.LastReconcile()
	if last == nil || last.Error != "boom" {
		t.Fatalf("error pass not recorded as last: %+v", last)
	}
	// lastOK should still be the prior success.
	ok := tr.LastSuccessfulReconcile()
	if ok == nil || !ok.CompletedAt.Equal(t1) {
		t.Fatalf("lastOK should still point at the earlier success, got %+v", ok)
	}
}

func TestTracker_GettersReturnCopies(t *testing.T) {
	tr := NewTracker()
	tr.RecordReap(1, 0)
	got := tr.RecentReaps()
	got[0].Checked = 999
	if again := tr.RecentReaps(); again[0].Checked != 1 {
		t.Errorf("RecentReaps did not return a copy: got Checked=%d after mutation", again[0].Checked)
	}
}
