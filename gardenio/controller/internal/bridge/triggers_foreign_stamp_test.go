package bridge

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// Out-of-band last_checked detector (ka-0000006237 Layer 1b, epic
// ka-0000005107). During the 2026-07-01 outage a sandbox jira-bridge stamped
// prod trigger beads' last_checked ~10s before every prod poll; the prod
// engine cooldown-skipped 414 cycles in a row with no signal. These tests pin
// the detector that turns that condition into a WARN on the FIRST starved
// cycle.

func foreignDetectorEngine() *TriggerEngine {
	return NewTriggerEngine(TriggerEngineConfig{
		Daemon: &mockTriggerDaemon{},
		Logger: slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
	})
}

func TestNoteForeignLastChecked_FirstObservationNotForeign(t *testing.T) {
	e := foreignDetectorEngine()
	trig := TriggerDef{ID: "t1", LastChecked: time.Now().UTC()}
	if foreign, _ := e.noteForeignLastChecked(trig); foreign {
		t.Error("first observation must not be flagged foreign")
	}
}

func TestNoteForeignLastChecked_AdvanceWithoutSelfStampIsForeign(t *testing.T) {
	e := foreignDetectorEngine()
	t0 := time.Now().UTC().Add(-20 * time.Minute)
	e.noteForeignLastChecked(TriggerDef{ID: "t1", LastChecked: t0})

	// Next cycle: last_checked advanced 10 minutes but THIS engine never
	// stamped — the sandbox signature.
	t1 := t0.Add(10 * time.Minute)
	foreign, prev := e.noteForeignLastChecked(TriggerDef{ID: "t1", LastChecked: t1})
	if !foreign {
		t.Error("out-of-band advance must be flagged foreign")
	}
	if !prev.Equal(t0) {
		t.Errorf("prev = %v, want %v", prev, t0)
	}
}

func TestNoteForeignLastChecked_OwnStampNotForeign(t *testing.T) {
	e := foreignDetectorEngine()
	t0 := time.Now().UTC().Add(-20 * time.Minute)
	e.noteForeignLastChecked(TriggerDef{ID: "t1", LastChecked: t0})

	// This engine stamps (evalOne fired / no-new-matches path).
	if err := e.stampLastChecked(context.Background(), "t1"); err != nil {
		t.Fatalf("stampLastChecked: %v", err)
	}

	// Next cycle the engine re-loads its own stamp (RFC3339 = second
	// precision, so the loaded value can differ from the recorded stamp by
	// sub-second truncation).
	e.mu.Lock()
	self := e.selfStamped["t1"]
	e.mu.Unlock()
	loaded := self.Truncate(time.Second)
	if foreign, _ := e.noteForeignLastChecked(TriggerDef{ID: "t1", LastChecked: loaded}); foreign {
		t.Error("the engine's own stamp echoing back must not be flagged foreign")
	}
}

func TestNoteForeignLastChecked_NoAdvanceNotForeign(t *testing.T) {
	e := foreignDetectorEngine()
	t0 := time.Now().UTC().Add(-5 * time.Minute)
	e.noteForeignLastChecked(TriggerDef{ID: "t1", LastChecked: t0})
	if foreign, _ := e.noteForeignLastChecked(TriggerDef{ID: "t1", LastChecked: t0}); foreign {
		t.Error("unchanged last_checked must not be flagged foreign")
	}
}

func TestStampLastChecked_WritesFieldAndRecordsSelf(t *testing.T) {
	daemon := &mockTriggerDaemon{}
	e := NewTriggerEngine(TriggerEngineConfig{
		Daemon: daemon,
		Logger: slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
	})

	if err := e.stampLastChecked(context.Background(), "t1"); err != nil {
		t.Fatalf("stampLastChecked: %v", err)
	}
	if len(daemon.updatedIDs) != 1 || daemon.updatedIDs[0] != "t1" {
		t.Fatalf("updatedIDs = %v, want [t1]", daemon.updatedIDs)
	}
	if daemon.updatedFields[0]["last_checked"] == "" {
		t.Error("last_checked field not written")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.selfStamped["t1"].IsZero() {
		t.Error("selfStamped not recorded")
	}
}

// TestEvalOne_CooldownSkipWarnsOnForeignAdvance pins the wire-in: a trigger
// in cooldown whose last_checked advanced out-of-band logs the starvation
// WARN on the very next evaluation cycle.
func TestEvalOne_CooldownSkipWarnsOnForeignAdvance(t *testing.T) {
	var logBuf bytes.Buffer
	daemon := &mockTriggerDaemon{}
	e := NewTriggerEngine(TriggerEngineConfig{
		Daemon: daemon,
		Logger: slog.New(slog.NewTextHandler(&logBuf, nil)),
	})
	src := &mockTriggerSource{name: "jira"}

	// Cycle 1: last_checked recent (cooldown-skips), observed.
	t0 := time.Now().UTC().Add(-30 * time.Second)
	e.evalOne(context.Background(), src, TriggerDef{
		ID: "t1", Title: "starved", CooldownMinutes: 10, LastChecked: t0,
	})
	if strings.Contains(logBuf.String(), "out-of-band") {
		t.Fatal("first observation must not WARN")
	}

	// Cycle 2: a foreign writer advanced last_checked; still inside cooldown.
	logBuf.Reset()
	t1 := time.Now().UTC().Add(-10 * time.Second)
	e.evalOne(context.Background(), src, TriggerDef{
		ID: "t1", Title: "starved", CooldownMinutes: 10, LastChecked: t1,
	})
	out := logBuf.String()
	if !strings.Contains(out, "out-of-band") {
		t.Errorf("expected out-of-band WARN, got logs:\n%s", out)
	}
	if !strings.Contains(out, "level=WARN") {
		t.Errorf("out-of-band advance must log at WARN, got:\n%s", out)
	}
	// The cooldown skip must still happen (no evaluation side effects).
	if len(daemon.updatedIDs) != 0 {
		t.Errorf("cooldown-skipped trigger must not be stamped, got updates: %v", daemon.updatedIDs)
	}
}
