package poolmanager

import (
	"context"
	"testing"
	"time"

	"log/slog"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/config"
)

// microvmRecycleConfig is recycleTestConfig plus a MicroVM lifetime cap, so the
// derived recycle age (half the cap) is deterministic in tests. The pool's
// current image is `image`; stamp MicroVM fixtures with the SAME source_image so
// the stale-image recycler is a no-op and only the age recycler acts.
func microvmRecycleConfig(image string, maxAgeOverride time.Duration, capSeconds int) *config.Config {
	cfg := recycleTestConfig(image)
	cfg.PrewarmedMicrovmMaxAge = maxAgeOverride
	cfg.MicrovmMaximumDurationSeconds = capSeconds
	return cfg
}

// mvAgent builds a lambda_microvm prewarmed member of the given age, stamped
// with the pool's current image so it never trips the stale-image recycler.
func mvAgent(id, image, state string, age time.Duration) prewarmedAgent {
	return prewarmedAgent{
		ID:              id,
		State:           state,
		SourceImage:     image,
		ExecutionTarget: "lambda_microvm",
		CreatedAt:       time.Now().Add(-age),
	}
}

// TestRecycleAgedMicrovm_ClosesAgedMicrovmOnly: only lambda_microvm members past
// the recycle age are closed; a fresh MicroVM member and an aged K8s member
// (execution_target empty) survive untouched.
func TestRecycleAgedMicrovm_ClosesAgedMicrovmOnly(t *testing.T) {
	cur := "agent:2026.160.7"
	h := &recycleHarness{}
	srv := h.server(t)
	defer srv.Close()
	daemon, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	// Explicit 20m cap → aged = created ≥20m ago.
	pm := New(daemon, microvmRecycleConfig(cur, 20*time.Minute, 3600), slog.Default())
	rec := &fakeRecorder{}
	pm.SetCycleRecorder(rec)

	agents := []prewarmedAgent{
		mvAgent("mv-aged", cur, "prewarmed", 30*time.Minute),
		mvAgent("mv-fresh", cur, "prewarmed", 2*time.Minute),
		// Aged K8s member (no execution_target) — must NOT be age-recycled.
		{ID: "k8s-aged", State: "prewarmed", SourceImage: cur, CreatedAt: time.Now().Add(-3 * time.Hour)},
	}
	survivors := pm.recycleAgedMicrovmAgents(context.Background(), poolKey{"gardenio", "thread"}, agents)

	if len(h.closed) != 1 || h.closed[0] != "mv-aged" {
		t.Fatalf("expected only mv-aged recycled, got %v", h.closed)
	}
	if !contains(ids(survivors), "mv-fresh") || !contains(ids(survivors), "k8s-aged") {
		t.Errorf("expected mv-fresh + k8s-aged to survive, got %v", ids(survivors))
	}
	if len(survivors) != 2 {
		t.Errorf("expected 2 survivors, got %v", ids(survivors))
	}
	if rec.total != 1 {
		t.Errorf("expected recorder total 1, got %d", rec.total)
	}
}

// TestRecycleAgedMicrovm_NeverTouchesAssigned: an assigned/working MicroVM
// member (or one carrying a thread/task) is real work and is never age-recycled,
// even when well past the recycle age.
func TestRecycleAgedMicrovm_NeverTouchesAssigned(t *testing.T) {
	cur := "agent:2026.160.7"
	h := &recycleHarness{}
	srv := h.server(t)
	defer srv.Close()
	daemon, _ := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	pm := New(daemon, microvmRecycleConfig(cur, 10*time.Minute, 3600), slog.Default())

	old := time.Now().Add(-2 * time.Hour)
	agents := []prewarmedAgent{
		{ID: "mv-working", State: "working", ExecutionTarget: "lambda_microvm", SourceImage: cur, CreatedAt: old},
		{ID: "mv-assigning", State: "assigning", ExecutionTarget: "lambda_microvm", SourceImage: cur, CreatedAt: old},
		{ID: "mv-has-thread", State: "prewarmed", ExecutionTarget: "lambda_microvm", SourceImage: cur, CreatedAt: old, HasThread: true},
		{ID: "mv-has-task", State: "idle", ExecutionTarget: "lambda_microvm", SourceImage: cur, CreatedAt: old, HasTask: true},
	}
	survivors := pm.recycleAgedMicrovmAgents(context.Background(), poolKey{"gardenio", "thread"}, agents)

	if len(h.closed) != 0 {
		t.Fatalf("expected 0 closed (none eligible), got %v", h.closed)
	}
	if len(survivors) != 4 {
		t.Errorf("expected all 4 to survive, got %v", ids(survivors))
	}
}

// TestRecycleAgedMicrovm_RateCapped: drainRate bounds how many aged members
// drain per pass, oldest-first, so a burst of aged members does not empty the
// pool in one tick.
func TestRecycleAgedMicrovm_RateCapped(t *testing.T) {
	cur := "agent:2026.160.7"
	h := &recycleHarness{}
	srv := h.server(t)
	defer srv.Close()
	daemon, _ := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	pm := New(daemon, microvmRecycleConfig(cur, 10*time.Minute, 3600), slog.Default())

	var agents []prewarmedAgent
	for i := range 5 {
		// All aged (30..50m), distinct ages so oldest-first is deterministic.
		agents = append(agents, mvAgent(string(rune('a'+i)), cur, "prewarmed", time.Duration(50-i)*time.Minute))
	}
	survivors := pm.recycleAgedMicrovmAgents(context.Background(), poolKey{"gardenio", "thread"}, agents)

	// drainRate defaults to 2 → 2 closed, oldest-first (a,b), 3 survive.
	if len(h.closed) != 2 || len(survivors) != 3 {
		t.Fatalf("expected 2 closed / 3 survive, got %d closed %d survive", len(h.closed), len(survivors))
	}
	if !contains(h.closed, "a") || !contains(h.closed, "b") {
		t.Errorf("expected oldest a,b closed first, got %v", h.closed)
	}
}

// TestRecycleAgedMicrovm_DerivesAgeFromCap: with no explicit knob, the recycle
// age is half the effective MaximumDurationInSeconds. A member older than half
// the cap is recycled; a member younger than half survives.
func TestRecycleAgedMicrovm_DerivesAgeFromCap(t *testing.T) {
	cur := "agent:2026.160.7"
	h := &recycleHarness{}
	srv := h.server(t)
	defer srv.Close()
	daemon, _ := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	// cap=3600s → recycle age = 30m. No explicit override.
	pm := New(daemon, microvmRecycleConfig(cur, 0, 3600), slog.Default())

	agents := []prewarmedAgent{
		mvAgent("past-half", cur, "prewarmed", 31*time.Minute),
		mvAgent("under-half", cur, "prewarmed", 29*time.Minute),
	}
	survivors := pm.recycleAgedMicrovmAgents(context.Background(), poolKey{"gardenio", "thread"}, agents)

	if len(h.closed) != 1 || h.closed[0] != "past-half" {
		t.Fatalf("expected only past-half recycled at half-cap age, got %v", h.closed)
	}
	if len(survivors) != 1 || survivors[0].ID != "under-half" {
		t.Errorf("expected under-half to survive, got %v", ids(survivors))
	}
}

// TestRecycleAgedMicrovm_AlwaysOnFromCompiledDefault: even with neither the knob
// nor a config cap set, age-recycling for an ENABLED MicroVM pool stays active,
// deriving from the compiled default cap (3600s → 30m). A MicroVM member must
// never be allowed to approach its wall-clock cap while parked, so once a pool
// is enabled the recycle age is always positive — the pool-level cost guard
// (PrewarmedPoolConfig.Enabled, default off) is the on/off switch, not this.
func TestRecycleAgedMicrovm_AlwaysOnFromCompiledDefault(t *testing.T) {
	cur := "agent:2026.160.7"
	h := &recycleHarness{}
	srv := h.server(t)
	defer srv.Close()
	daemon, _ := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	// No knob, no config cap → recycle age = half the compiled default = 30m.
	pm := New(daemon, microvmRecycleConfig(cur, 0, 0), slog.Default())
	want := time.Duration(defaultMicrovmMaximumDurationSeconds) * time.Second / 2
	if got := pm.microvmRecycleAge(); got != want {
		t.Fatalf("expected compiled-default recycle age %s, got %s", want, got)
	}

	agents := []prewarmedAgent{
		mvAgent("ancient", cur, "prewarmed", 24*time.Hour),
		mvAgent("fresh", cur, "prewarmed", 1*time.Minute),
	}
	survivors := pm.recycleAgedMicrovmAgents(context.Background(), poolKey{"gardenio", "thread"}, agents)
	if len(h.closed) != 1 || h.closed[0] != "ancient" {
		t.Fatalf("expected ancient recycled under compiled-default age, got %v", h.closed)
	}
	if len(survivors) != 1 || survivors[0].ID != "fresh" {
		t.Errorf("expected fresh retained, got %v", ids(survivors))
	}
}

// TestRecycleAgedMicrovm_MissingTimestampNeverRecycled: a member with no
// parseable created_at reads as zero age and is never age-recycled (the
// stale-image recycler owns never-stamped members).
func TestRecycleAgedMicrovm_MissingTimestampNeverRecycled(t *testing.T) {
	cur := "agent:2026.160.7"
	h := &recycleHarness{}
	srv := h.server(t)
	defer srv.Close()
	daemon, _ := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	pm := New(daemon, microvmRecycleConfig(cur, 10*time.Minute, 3600), slog.Default())

	agents := []prewarmedAgent{
		{ID: "no-ts", State: "prewarmed", ExecutionTarget: "lambda_microvm", SourceImage: cur},
	}
	survivors := pm.recycleAgedMicrovmAgents(context.Background(), poolKey{"gardenio", "thread"}, agents)
	if len(h.closed) != 0 {
		t.Fatalf("expected 0 closed for missing timestamp, got %v", h.closed)
	}
	if len(survivors) != 1 {
		t.Errorf("expected the member retained, got %v", ids(survivors))
	}
}

// TestMicrovmRecycleAge_ExplicitKnobWins asserts the explicit
// PrewarmedMicrovmMaxAge overrides the cap-derived default verbatim, and that a
// zero knob falls back to half the effective cap.
func TestMicrovmRecycleAge_ExplicitKnobWins(t *testing.T) {
	cur := "agent:2026.160.7"
	daemon, _ := beadsapi.New(beadsapi.Config{HTTPAddr: "http://unused"})

	// Explicit knob wins over the cap-derived default.
	pm := New(daemon, microvmRecycleConfig(cur, 7*time.Minute, 3600), slog.Default())
	if got := pm.microvmRecycleAge(); got != 7*time.Minute {
		t.Errorf("explicit knob: got %s, want 7m", got)
	}

	// No knob, custom cap → half the cap.
	pm = New(daemon, microvmRecycleConfig(cur, 0, 1800), slog.Default())
	if got := pm.microvmRecycleAge(); got != 15*time.Minute {
		t.Errorf("cap-derived: got %s, want 15m (half of 1800s)", got)
	}

	// No knob, no cap set → half the compiled default (3600s → 30m).
	pm = New(daemon, microvmRecycleConfig(cur, 0, 0), slog.Default())
	want := time.Duration(defaultMicrovmMaximumDurationSeconds) * time.Second / 2
	if got := pm.microvmRecycleAge(); got != want {
		t.Errorf("default-cap: got %s, want %s", got, want)
	}
}
