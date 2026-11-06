package workflowhost

import (
	"testing"
	"time"
)

func mustMarshalLease(t *testing.T, l Lease) string {
	t.Helper()
	s, err := MarshalLease(l)
	if err != nil {
		t.Fatalf("MarshalLease: %v", err)
	}
	return s
}

func TestParseLease_RoundTrip(t *testing.T) {
	now := time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC)
	orig := NewLease("pod-a", "wf_123", now)
	s := mustMarshalLease(t, orig)

	got, err := ParseLease(s)
	if err != nil {
		t.Fatalf("ParseLease: %v", err)
	}
	if got == nil {
		t.Fatal("ParseLease returned nil for non-empty input")
	}
	if got.PodID != "pod-a" || got.RunID != "wf_123" {
		t.Errorf("round-trip mismatch: %+v", got)
	}
	if got.HeartbeatTS != now.Format(time.RFC3339) {
		t.Errorf("heartbeat = %q, want %q", got.HeartbeatTS, now.Format(time.RFC3339))
	}
}

func TestParseLease_Empty(t *testing.T) {
	got, err := ParseLease("")
	if err != nil {
		t.Fatalf("ParseLease(\"\"): %v", err)
	}
	if got != nil {
		t.Errorf("expected nil lease for empty string, got %+v", got)
	}
}

func TestParseLease_Malformed(t *testing.T) {
	if _, err := ParseLease("{not json"); err == nil {
		t.Fatal("expected error parsing malformed lease JSON")
	}
}

func TestLease_IsStale(t *testing.T) {
	now := time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name      string
		heartbeat string
		ttl       time.Duration
		want      bool
	}{
		{"fresh", now.Add(-1 * time.Minute).Format(time.RFC3339), DefaultLeaseTTL, false},
		{"exactly-at-ttl", now.Add(-DefaultLeaseTTL).Format(time.RFC3339), DefaultLeaseTTL, false},
		{"just-past-ttl", now.Add(-DefaultLeaseTTL - time.Second).Format(time.RFC3339), DefaultLeaseTTL, true},
		{"way-old", now.Add(-2 * time.Hour).Format(time.RFC3339), DefaultLeaseTTL, true},
		{"empty-heartbeat", "", DefaultLeaseTTL, true},
		{"garbage-heartbeat", "not-a-time", DefaultLeaseTTL, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l := Lease{PodID: "p", HeartbeatTS: tt.heartbeat}
			if got := l.IsStale(now, tt.ttl); got != tt.want {
				t.Errorf("IsStale = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDecideResume_ColdStartNoRunID(t *testing.T) {
	d := DecideResume(State{ScriptPath: "wf.js", S3URI: "s3://b/wf_1/"})
	if d.Resumable {
		t.Error("expected not resumable when runId is empty")
	}
	if d.NeedJournalRestore {
		t.Error("cold start must not request journal restore")
	}
	if d.ScriptPath != "wf.js" {
		t.Errorf("ScriptPath = %q, want carried through", d.ScriptPath)
	}
	if d.Reason == "" {
		t.Error("expected a cold-start reason")
	}
}

func TestDecideResume_ResumableWithJournal(t *testing.T) {
	d := DecideResume(State{RunID: "wf_9", ScriptPath: "wf.js", S3URI: "s3://b/wf_9/"})
	if !d.Resumable {
		t.Fatal("expected resumable when runId present")
	}
	if !d.NeedJournalRestore {
		t.Error("expected journal restore when s3 uri present")
	}
	if d.RunID != "wf_9" || d.S3URI != "s3://b/wf_9/" {
		t.Errorf("decision carried wrong handles: %+v", d)
	}
}

func TestDecideResume_ResumableNoJournal(t *testing.T) {
	// runId persisted but no s3 uri yet (same-pod retry, journal still local).
	d := DecideResume(State{RunID: "wf_9", ScriptPath: "wf.js"})
	if !d.Resumable {
		t.Fatal("expected resumable")
	}
	if d.NeedJournalRestore {
		t.Error("no s3 uri → no journal restore needed")
	}
}

func TestEvaluateLease_FreeAcquire(t *testing.T) {
	now := time.Now().UTC()
	d := EvaluateLease(nil, "pod-a", now, DefaultLeaseTTL)
	if !d.Acquired {
		t.Errorf("expected acquire on free lease: %s", d.Reason)
	}
}

func TestEvaluateLease_SelfOwnedAcquire(t *testing.T) {
	now := time.Now().UTC()
	cur := NewLease("pod-a", "wf_1", now)
	d := EvaluateLease(&cur, "pod-a", now, DefaultLeaseTTL)
	if !d.Acquired {
		t.Errorf("expected acquire on self-owned lease: %s", d.Reason)
	}
}

func TestEvaluateLease_StaleTakeover(t *testing.T) {
	now := time.Now().UTC()
	cur := NewLease("pod-a", "wf_1", now.Add(-2*time.Hour))
	d := EvaluateLease(&cur, "pod-b", now, DefaultLeaseTTL)
	if !d.Acquired {
		t.Errorf("expected stale takeover: %s", d.Reason)
	}
}

func TestEvaluateLease_LiveConflictRefused(t *testing.T) {
	now := time.Now().UTC()
	cur := NewLease("pod-a", "wf_1", now.Add(-1*time.Minute))
	d := EvaluateLease(&cur, "pod-b", now, DefaultLeaseTTL)
	if d.Acquired {
		t.Fatal("expected refusal when another pod holds a live lease")
	}
	if d.Conflict == nil || d.Conflict.PodID != "pod-a" {
		t.Errorf("expected conflict on pod-a, got %+v", d.Conflict)
	}
}
