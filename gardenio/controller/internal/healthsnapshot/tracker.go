// Package healthsnapshot collects controller-side health signals (pod counts
// by phase, recent reaper actions, recent rotation events, last reconcile
// times) and serves them as a single bounded JSON payload at
// GET /healthz/snapshot. Designed to back the gardenio health page in the
// htmx beads-viewer (epic ka-0000000152).
//
// The JSON shape mirrors the R5 chart-catalog contract per tile
// ({data, spec, meta}) so that when the chart catalog backend lands the
// page can swap to chart_id reads without reshaping the wire format.
package healthsnapshot

import (
	"sync"
	"time"
)

const (
	// MaxRecentReaps caps the number of reap events kept in memory.
	MaxRecentReaps = 20
	// MaxRecentRotations caps the number of rotation events kept in memory.
	MaxRecentRotations = 50
)

// ReapEvent is one stale-reaper pass.
type ReapEvent struct {
	Timestamp time.Time `json:"timestamp"`
	Checked   int       `json:"checked"`
	Reaped    int       `json:"reaped"`
}

// RotationEvent is one account rotation / throttle / warming notification.
// Mirrors the subset of fields the rotation_notifier publishes that an
// operator would want to see at a glance.
type RotationEvent struct {
	Timestamp   time.Time `json:"timestamp"`
	Event       string    `json:"event"`
	Project     string    `json:"project,omitempty"`
	FromAccount string    `json:"from_account,omitempty"`
	ToAccount   string    `json:"to_account,omitempty"`
	Reason      string    `json:"reason,omitempty"`
}

// ReconcileEvent records one reconcile pass.
type ReconcileEvent struct {
	StartedAt   time.Time `json:"started_at"`
	CompletedAt time.Time `json:"completed_at"`
	DurationMs  int64     `json:"duration_ms"`
	Error       string    `json:"error,omitempty"`
}

// Tracker is an in-memory ring-buffer for reaper actions, rotation events,
// and reconcile timings. Safe for concurrent use.
type Tracker struct {
	mu              sync.Mutex
	recentReaps     []ReapEvent
	recentRotations []RotationEvent
	lastReconcile   *ReconcileEvent
	lastReconcileOK *ReconcileEvent
	now             func() time.Time
}

// NewTracker returns a Tracker that uses time.Now.
func NewTracker() *Tracker {
	return &Tracker{now: time.Now}
}

// RecordReap appends a reap event with the current timestamp.
func (t *Tracker) RecordReap(checked, reaped int) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.recentReaps = appendCapped(t.recentReaps, ReapEvent{
		Timestamp: t.now(),
		Checked:   checked,
		Reaped:    reaped,
	}, MaxRecentReaps)
}

// RecordRotation appends a rotation event with the current timestamp.
func (t *Tracker) RecordRotation(event, project, fromAccount, toAccount, reason string) {
	if t == nil || event == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.recentRotations = appendCapped(t.recentRotations, RotationEvent{
		Timestamp:   t.now(),
		Event:       event,
		Project:     project,
		FromAccount: fromAccount,
		ToAccount:   toAccount,
		Reason:      reason,
	}, MaxRecentRotations)
}

// RecordReconcile records the latest reconcile pass. err may be nil for a
// successful pass. Updates lastReconcile always; lastReconcileOK only on
// success — so operators can see both "have we run lately?" and "have we
// run *successfully* lately?".
func (t *Tracker) RecordReconcile(start, end time.Time, err error) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	evt := &ReconcileEvent{
		StartedAt:   start,
		CompletedAt: end,
		DurationMs:  end.Sub(start).Milliseconds(),
	}
	if err != nil {
		evt.Error = err.Error()
	}
	t.lastReconcile = evt
	if err == nil {
		t.lastReconcileOK = evt
	}
}

// RecentReaps returns a copy of the recent reap events, oldest first.
func (t *Tracker) RecentReaps() []ReapEvent {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]ReapEvent, len(t.recentReaps))
	copy(out, t.recentReaps)
	return out
}

// RecentRotations returns a copy of the recent rotation events, oldest first.
func (t *Tracker) RecentRotations() []RotationEvent {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]RotationEvent, len(t.recentRotations))
	copy(out, t.recentRotations)
	return out
}

// LastReconcile returns the most recent reconcile pass (success or failure),
// or nil if none has been recorded.
func (t *Tracker) LastReconcile() *ReconcileEvent {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.lastReconcile == nil {
		return nil
	}
	cp := *t.lastReconcile
	return &cp
}

// LastSuccessfulReconcile returns the most recent reconcile pass that
// completed without error, or nil if none has been recorded.
func (t *Tracker) LastSuccessfulReconcile() *ReconcileEvent {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.lastReconcileOK == nil {
		return nil
	}
	cp := *t.lastReconcileOK
	return &cp
}

func appendCapped[T any](buf []T, v T, max int) []T {
	buf = append(buf, v)
	if len(buf) > max {
		buf = buf[len(buf)-max:]
	}
	return buf
}
