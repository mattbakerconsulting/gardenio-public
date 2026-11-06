package bridge

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// pastLastChecked returns a non-zero LastChecked timestamp that bypasses the
// engine's runtime-bootstrap guard (which suppresses first-evaluation firing
// for brand-new triggers). Most unit tests want to test the steady-state
// "N matches → N actions" behavior without the bootstrap concern.
func pastLastChecked() time.Time { return time.Now().UTC().Add(-1 * time.Hour) }

// mockTriggerSource implements TriggerSource for testing the framework.
type mockTriggerSource struct {
	name     string
	triggers []TriggerDef
	matches  map[string][]TriggerMatch // trigger ID → matches
	keys     map[string][]string       // trigger ID → catch-up keys
}

func (s *mockTriggerSource) Name() string { return s.name }

func (s *mockTriggerSource) LoadTriggers(_ context.Context, _ TriggerBeadClient) ([]TriggerDef, error) {
	return s.triggers, nil
}

func (s *mockTriggerSource) Evaluate(_ context.Context, trig TriggerDef) ([]TriggerMatch, error) {
	if s.matches != nil {
		return s.matches[trig.ID], nil
	}
	return nil, nil
}

func (s *mockTriggerSource) CatchUpKeys(_ context.Context, _ TriggerBeadClient, triggerID string) ([]string, error) {
	if s.keys != nil {
		return s.keys[triggerID], nil
	}
	return nil, nil
}

// TestTriggerEngine_MultiSource tests the engine with multiple registered sources.
func TestTriggerEngine_MultiSource(t *testing.T) {
	daemon := &mockTriggerDaemon{}

	engine := NewTriggerEngine(TriggerEngineConfig{
		Daemon: daemon,
		Logger: slog.Default(),
	})

	source1 := &mockTriggerSource{
		name: "alpha",
		triggers: []TriggerDef{
			{ID: "t1", Title: "Alpha Trigger", Source: "alpha", Action: TriggerActionNotify, MaxConcurrent: 10, Prompt: "Got {{key}}", LastChecked: pastLastChecked()},
		},
		matches: map[string][]TriggerMatch{
			"t1": {{Key: "A1", Title: "Alpha match", TemplateVars: map[string]string{"{{key}}": "A1"}}},
		},
	}
	source2 := &mockTriggerSource{
		name: "beta",
		triggers: []TriggerDef{
			{ID: "t2", Title: "Beta Trigger", Source: "beta", Action: TriggerActionCreateBead, MaxConcurrent: 10, Project: "test", LastChecked: pastLastChecked()},
		},
		matches: map[string][]TriggerMatch{
			"t2": {{Key: "B1", Title: "Beta match", Fields: map[string]string{"beta_id": "B1"}}},
		},
	}

	engine.RegisterSource(source1)
	engine.RegisterSource(source2)

	var (
		mu       sync.Mutex
		notified bool
	)
	engine.notifier = func(_ context.Context, _, _ string, _ string) {
		mu.Lock()
		notified = true
		mu.Unlock()
	}

	engine.Evaluate(context.Background())

	mu.Lock()
	if !notified {
		t.Error("expected notification from alpha source")
	}
	mu.Unlock()

	daemon.mu.Lock()
	if len(daemon.createdBeads) != 1 {
		t.Errorf("expected 1 created bead from beta source, got %d", len(daemon.createdBeads))
	}
	daemon.mu.Unlock()

	// Verify dedup: second evaluation should produce nothing new.
	daemon.mu.Lock()
	daemon.createdBeads = nil
	daemon.mu.Unlock()
	notified = false

	engine.Evaluate(context.Background())

	daemon.mu.Lock()
	if len(daemon.createdBeads) != 0 {
		t.Errorf("expected 0 beads on second eval (dedup), got %d", len(daemon.createdBeads))
	}
	daemon.mu.Unlock()
}

// TestTriggerEngine_CatchUp tests catch-up across multiple sources.
func TestTriggerEngine_CatchUp(t *testing.T) {
	daemon := &mockTriggerDaemon{}

	engine := NewTriggerEngine(TriggerEngineConfig{
		Daemon: daemon,
		Logger: slog.Default(),
	})

	source := &mockTriggerSource{
		name: "test",
		triggers: []TriggerDef{
			{ID: "t1", Title: "Test Trigger", Source: "test", Action: TriggerActionNotify, MaxConcurrent: 10},
		},
		matches: map[string][]TriggerMatch{
			"t1": {{Key: "K1", Title: "Already known"}},
		},
		keys: map[string][]string{
			"t1": {"K1"},
		},
	}

	engine.RegisterSource(source)
	engine.CatchUp(context.Background())

	// K1 should be in seen set.
	seen := engine.SeenKeys("t1")
	if !seen["K1"] {
		t.Error("expected K1 in seen set after catch-up")
	}

	// Evaluate should not fire for K1.
	var notifyCount int
	engine.notifier = func(_ context.Context, _, _ string, _ string) {
		notifyCount++
	}
	engine.Evaluate(context.Background())

	if notifyCount != 0 {
		t.Errorf("expected 0 notifications after catch-up, got %d", notifyCount)
	}
}

// TestTriggerEngine_MaxConcurrent tests that max_concurrent limits matches.
func TestTriggerEngine_MaxConcurrent(t *testing.T) {
	daemon := &mockTriggerDaemon{}

	engine := NewTriggerEngine(TriggerEngineConfig{
		Daemon: daemon,
		Logger: slog.Default(),
	})

	source := &mockTriggerSource{
		name: "test",
		triggers: []TriggerDef{
			{ID: "t1", Title: "Limited", Source: "test", Action: TriggerActionCreateBead, MaxConcurrent: 2, Project: "test", LastChecked: pastLastChecked()},
		},
		matches: map[string][]TriggerMatch{
			"t1": {
				{Key: "M1", Title: "Match 1"},
				{Key: "M2", Title: "Match 2"},
				{Key: "M3", Title: "Match 3"},
				{Key: "M4", Title: "Match 4"},
			},
		},
	}

	engine.RegisterSource(source)
	engine.Evaluate(context.Background())

	daemon.mu.Lock()
	if len(daemon.createdBeads) != 2 {
		t.Errorf("expected 2 beads (max_concurrent=2), got %d", len(daemon.createdBeads))
	}
	daemon.mu.Unlock()

	// Second eval: 2 more.
	engine.Evaluate(context.Background())

	daemon.mu.Lock()
	if len(daemon.createdBeads) != 4 {
		t.Errorf("expected 4 total beads after second eval, got %d", len(daemon.createdBeads))
	}
	daemon.mu.Unlock()
}

// TestTriggerEngine_LiveConcurrencyLease is the regression test for
// ka-0000003120: max_concurrent on a spawn-producing trigger must be a LIVE
// lease, not a per-batch cap. An agent already alive from a PRIOR fire (or a
// sibling source of the same multi-entry trigger) must count against the cap,
// so a max_concurrent=1 trigger does NOT spawn a second instance while the
// first is still running.
func TestTriggerEngine_LiveConcurrencyLease(t *testing.T) {
	daemon := &mockTriggerDaemon{
		// One agent already alive, spawned by this very trigger.
		beads: []*beadsapi.BeadDetail{
			{
				ID:     "gd-alive-agent",
				Type:   "agent",
				Status: "in_progress",
				Fields: map[string]string{"spawned_by": "trigger:t1"},
			},
		},
	}

	engine := NewTriggerEngine(TriggerEngineConfig{
		Daemon: daemon,
		Logger: slog.Default(),
	})

	source := &mockTriggerSource{
		name: "test",
		triggers: []TriggerDef{
			{
				ID: "t1", Title: "Single-threaded", Source: "test",
				Action: TriggerActionSpawnAgent, Role: "crew", Project: "myproj",
				Prompt: "do work", MaxConcurrent: 1, LastChecked: pastLastChecked(),
			},
		},
		matches: map[string][]TriggerMatch{
			"t1": {{Key: "M1", Title: "Match 1"}},
		},
	}

	engine.RegisterSource(source)
	engine.Evaluate(context.Background())

	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	if len(daemon.spawnedNames) != 0 {
		t.Errorf("expected 0 spawns (1 agent already alive, max_concurrent=1), got %d",
			len(daemon.spawnedNames))
	}
}

// TestTriggerEngine_LiveConcurrencyLeaseFiresWhenIdle verifies the lease does
// NOT over-suppress: with no live agents from this trigger, a max_concurrent=1
// spawn trigger fires exactly once. (A different trigger's live agent must not
// count against this trigger's lease.)
func TestTriggerEngine_LiveConcurrencyLeaseFiresWhenIdle(t *testing.T) {
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			// Alive, but spawned by a DIFFERENT trigger — must not count.
			{
				ID:     "gd-other-agent",
				Type:   "agent",
				Status: "in_progress",
				Fields: map[string]string{"spawned_by": "trigger:other"},
			},
		},
	}

	engine := NewTriggerEngine(TriggerEngineConfig{
		Daemon: daemon,
		Logger: slog.Default(),
	})

	source := &mockTriggerSource{
		name: "test",
		triggers: []TriggerDef{
			{
				ID: "t1", Title: "Single-threaded", Source: "test",
				Action: TriggerActionSpawnAgent, Role: "crew", Project: "myproj",
				Prompt: "do work", MaxConcurrent: 1, LastChecked: pastLastChecked(),
			},
		},
		matches: map[string][]TriggerMatch{
			"t1": {
				{Key: "M1", Title: "Match 1"},
				{Key: "M2", Title: "Match 2"},
			},
		},
	}

	engine.RegisterSource(source)
	engine.Evaluate(context.Background())

	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	if len(daemon.spawnedNames) != 1 {
		t.Errorf("expected exactly 1 spawn (no live agents from t1, max_concurrent=1), got %d",
			len(daemon.spawnedNames))
	}
}

// TestTriggerEngine_PerTriggerCooldown verifies that per-trigger
// cooldown_minutes is honored in evalOne, and that the 30s package floor
// still applies when CooldownMinutes is unset (zero).
func TestTriggerEngine_PerTriggerCooldown(t *testing.T) {
	daemon := &mockTriggerDaemon{}

	engine := NewTriggerEngine(TriggerEngineConfig{
		Daemon: daemon,
		Logger: slog.Default(),
	})

	now := time.Now().UTC()
	source := &mockTriggerSource{
		name: "test",
		triggers: []TriggerDef{
			// 20-minute per-trigger cooldown, last fired 5 minutes ago → suppressed.
			{ID: "t-cool-recent", Title: "Cool Recent", Source: "test", Action: TriggerActionNotify,
				MaxConcurrent: 10, CooldownMinutes: 20, LastChecked: now.Add(-5 * time.Minute)},
			// 20-minute per-trigger cooldown, last fired 25 minutes ago → fires.
			{ID: "t-cool-elapsed", Title: "Cool Elapsed", Source: "test", Action: TriggerActionNotify,
				MaxConcurrent: 10, CooldownMinutes: 20, LastChecked: now.Add(-25 * time.Minute)},
			// No per-trigger cooldown, last fired 10s ago → suppressed by 30s floor.
			{ID: "t-floor-recent", Title: "Floor Recent", Source: "test", Action: TriggerActionNotify,
				MaxConcurrent: 10, CooldownMinutes: 0, LastChecked: now.Add(-10 * time.Second)},
			// No per-trigger cooldown, last fired 60s ago → fires (30s floor cleared).
			{ID: "t-floor-elapsed", Title: "Floor Elapsed", Source: "test", Action: TriggerActionNotify,
				MaxConcurrent: 10, CooldownMinutes: 0, LastChecked: now.Add(-60 * time.Second)},
		},
		matches: map[string][]TriggerMatch{
			"t-cool-recent":   {{Key: "K1", Title: "match"}},
			"t-cool-elapsed":  {{Key: "K2", Title: "match"}},
			"t-floor-recent":  {{Key: "K3", Title: "match"}},
			"t-floor-elapsed": {{Key: "K4", Title: "match"}},
		},
	}

	engine.RegisterSource(source)

	fired := map[string]int{}
	var firedMu sync.Mutex
	engine.notifier = func(_ context.Context, title, _ string, _ string) {
		firedMu.Lock()
		defer firedMu.Unlock()
		fired[title]++
	}

	engine.Evaluate(context.Background())

	firedMu.Lock()
	defer firedMu.Unlock()
	if fired["Cool Recent"] != 0 {
		t.Errorf("Cool Recent (20m cooldown, 5m ago) should be suppressed, fired %d times", fired["Cool Recent"])
	}
	if fired["Cool Elapsed"] != 1 {
		t.Errorf("Cool Elapsed (20m cooldown, 25m ago) should fire once, fired %d times", fired["Cool Elapsed"])
	}
	if fired["Floor Recent"] != 0 {
		t.Errorf("Floor Recent (30s floor, 10s ago) should be suppressed, fired %d times", fired["Floor Recent"])
	}
	if fired["Floor Elapsed"] != 1 {
		t.Errorf("Floor Elapsed (30s floor, 60s ago) should fire once, fired %d times", fired["Floor Elapsed"])
	}
}

// TestTriggerEngine_IgnoreCooldown proves the test-only cooldown-bypass seam
// (ka-0000007982): a trigger whose last_checked is inside the cooldown window is
// cooldown-skipped by default, but fires when IgnoreCooldown is set for it — and
// the bypass is scoped to exactly that trigger, so a sibling inside its own
// cooldown window stays suppressed. This is the seam cmd/trigger-probe uses so a
// foreign last_checked stamp (written by a live trigger-engine sharing the e2e
// daemon) cannot cooldown-starve the probe's single deliberate fire.
func TestTriggerEngine_IgnoreCooldown(t *testing.T) {
	now := time.Now().UTC()
	makeSource := func() *mockTriggerSource {
		return &mockTriggerSource{
			name: "test",
			triggers: []TriggerDef{
				// 30s floor, last checked 5s ago → inside the cooldown window.
				{ID: "t-probe", Title: "Probe", Source: "test", Action: TriggerActionNotify,
					MaxConcurrent: 10, CooldownMinutes: 0, LastChecked: now.Add(-5 * time.Second)},
				// Sibling, also inside its cooldown window → must NOT be bypassed.
				{ID: "t-other", Title: "Other", Source: "test", Action: TriggerActionNotify,
					MaxConcurrent: 10, CooldownMinutes: 0, LastChecked: now.Add(-5 * time.Second)},
			},
			matches: map[string][]TriggerMatch{
				"t-probe": {{Key: "K1", Title: "match"}},
				"t-other": {{Key: "K2", Title: "match"}},
			},
		}
	}

	run := func(t *testing.T, ignore bool) map[string]int {
		t.Helper()
		engine := NewTriggerEngine(TriggerEngineConfig{Daemon: &mockTriggerDaemon{}, Logger: slog.Default()})
		engine.RegisterSource(makeSource())
		fired := map[string]int{}
		var mu sync.Mutex
		engine.notifier = func(_ context.Context, title, _ string, _ string) {
			mu.Lock()
			defer mu.Unlock()
			fired[title]++
		}
		if ignore {
			engine.IgnoreCooldown("t-probe")
		}
		engine.Evaluate(context.Background())
		mu.Lock()
		defer mu.Unlock()
		return fired
	}

	// Baseline: without the seam, the fresh last_checked suppresses the fire.
	base := run(t, false)
	if base["Probe"] != 0 {
		t.Errorf("without IgnoreCooldown, Probe (5s ago, 30s floor) should be cooldown-suppressed, fired %d", base["Probe"])
	}

	// With the seam: the named trigger fires despite being inside the window,
	// while the sibling (also inside its window) stays suppressed.
	got := run(t, true)
	if got["Probe"] != 1 {
		t.Errorf("with IgnoreCooldown(t-probe), Probe should bypass the cooldown and fire once, fired %d", got["Probe"])
	}
	if got["Other"] != 0 {
		t.Errorf("IgnoreCooldown must be scoped to t-probe; sibling t-other (5s ago) should stay suppressed, fired %d", got["Other"])
	}
}

// TestEffectiveCooldown tests the per-trigger cooldown calculation.
func TestEffectiveCooldown(t *testing.T) {
	tests := []struct {
		name     string
		minutes  int
		expected time.Duration
	}{
		{"zero falls back to 30s floor", 0, 30 * time.Second},
		{"1 minute exceeds floor", 1, 1 * time.Minute},
		{"20 minutes per plan A.3", 20, 20 * time.Minute},
		{"negative falls back to 30s floor", -5, 30 * time.Second},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := effectiveCooldown(TriggerDef{CooldownMinutes: tc.minutes})
			if got != tc.expected {
				t.Errorf("effectiveCooldown(%d) = %v, want %v", tc.minutes, got, tc.expected)
			}
		})
	}
}

// TestTriggerEngine_EvaluateSource tests evaluating a single source by name.
func TestTriggerEngine_EvaluateSource(t *testing.T) {
	daemon := &mockTriggerDaemon{}

	engine := NewTriggerEngine(TriggerEngineConfig{
		Daemon: daemon,
		Logger: slog.Default(),
	})

	var alphaFired, betaFired int
	alpha := &mockTriggerSource{
		name:     "alpha",
		triggers: []TriggerDef{{ID: "a1", Title: "A", Source: "alpha", Action: TriggerActionNotify, MaxConcurrent: 10, LastChecked: pastLastChecked()}},
		matches:  map[string][]TriggerMatch{"a1": {{Key: "X1", Title: "X"}}},
	}
	beta := &mockTriggerSource{
		name:     "beta",
		triggers: []TriggerDef{{ID: "b1", Title: "B", Source: "beta", Action: TriggerActionNotify, MaxConcurrent: 10, LastChecked: pastLastChecked()}},
		matches:  map[string][]TriggerMatch{"b1": {{Key: "Y1", Title: "Y"}}},
	}

	engine.RegisterSource(alpha)
	engine.RegisterSource(beta)
	engine.notifier = func(_ context.Context, title, _ string, _ string) {
		if title == "A" {
			alphaFired++
		} else if title == "B" {
			betaFired++
		}
	}

	// Only evaluate alpha.
	engine.EvaluateSource(context.Background(), "alpha")

	if alphaFired != 1 {
		t.Errorf("expected alpha to fire once, got %d", alphaFired)
	}
	if betaFired != 0 {
		t.Errorf("expected beta to not fire, got %d", betaFired)
	}
}

// TestExpandTemplate tests the generic template expansion function.
func TestExpandTemplate(t *testing.T) {
	tests := []struct {
		name     string
		tmpl     string
		vars     map[string]string
		expected string
	}{
		{
			"basic substitution",
			"Hello {{name}}, your ID is {{id}}",
			map[string]string{"{{name}}": "Alice", "{{id}}": "42"},
			"Hello Alice, your ID is 42",
		},
		{
			"empty template",
			"",
			map[string]string{"{{x}}": "y"},
			"",
		},
		{
			"no vars",
			"Static text",
			nil,
			"Static text",
		},
		{
			"missing var left as-is",
			"Hello {{name}}",
			map[string]string{"{{other}}": "val"},
			"Hello {{name}}",
		},
		{
			"multiple occurrences",
			"{{x}} and {{x}} again",
			map[string]string{"{{x}}": "foo"},
			"foo and foo again",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ExpandTemplate(tt.tmpl, tt.vars)
			if got != tt.expected {
				t.Errorf("ExpandTemplate() = %q, want %q", got, tt.expected)
			}
		})
	}
}

// TestParseTriggerDef tests parsing common fields from a bead.
func TestParseTriggerDef(t *testing.T) {
	createdAt := time.Date(2026, 4, 10, 8, 30, 0, 0, time.UTC)
	bead := &beadsapi.BeadDetail{
		ID:        "gd-test-1",
		Title:     "Test Trigger",
		CreatedAt: createdAt,
		Fields: map[string]string{
			"action":                   "notify",
			"role":                     "crew",
			"project":                  "gardenio",
			"prompt":                   "Do the thing",
			"formula_id":               "gd-formula-1",
			"formula_vars":             `{"key":"val"}`,
			"cross_pour_upstream_step": "{{bead_id}}",
			"max_concurrent":           "5",
			"cooldown_minutes":         "10",
			"enabled":                  "true",
			"last_checked":             "2026-04-12T00:00:00Z",
		},
	}

	def := ParseTriggerDef(bead, "test")

	if def.ID != "gd-test-1" {
		t.Errorf("ID = %q", def.ID)
	}
	if def.Source != "test" {
		t.Errorf("Source = %q", def.Source)
	}
	if def.Action != TriggerActionNotify {
		t.Errorf("Action = %q", def.Action)
	}
	if def.Role != "crew" {
		t.Errorf("Role = %q", def.Role)
	}
	if def.Project != "gardenio" {
		t.Errorf("Project = %q", def.Project)
	}
	if def.MaxConcurrent != 5 {
		t.Errorf("MaxConcurrent = %d", def.MaxConcurrent)
	}
	if def.CooldownMinutes != 10 {
		t.Errorf("CooldownMinutes = %d", def.CooldownMinutes)
	}
	// The cross-pour anchor is parsed verbatim (template expansion happens later,
	// at fire time in actionApplyFormula) — decision ka-0000007596.
	if def.CrossPourUpstreamStep != "{{bead_id}}" {
		t.Errorf("CrossPourUpstreamStep = %q, want {{bead_id}}", def.CrossPourUpstreamStep)
	}
	if def.LastChecked.IsZero() {
		t.Error("LastChecked should not be zero")
	}
	// CreatedAt is the wall_clock baseline for gardener-shaped triggers
	// (ka-0000006967). It must be copied verbatim from the bead; before this
	// was plumbed it stayed zero and wall_clock was a silent no-op.
	if !def.CreatedAt.Equal(createdAt) {
		t.Errorf("CreatedAt = %v, want %v", def.CreatedAt, createdAt)
	}
}

// TestHasNonEmptyTriggers guards the predicate that the mislabel guardrail
// (bug ka-0000001688) keys off: a bead is a MULTI-trigger bead iff its
// `triggers` field parses to a non-empty array. It must use the SAME parser the
// engine uses at fire time (scheduler.ParseTriggers) so the authoring-time label
// decision matches runtime dispatch.
func TestHasNonEmptyTriggers(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want bool
	}{
		{"empty string", "", false},
		{"whitespace only", "  \n\t ", false},
		{"empty array", "[]", false},
		{"empty array with spaces", "  [ ]  ", false},
		{"malformed json", "{not-an-array", false},
		{"object not array", `{"kind":"cron"}`, false},
		{"single bead_event entry", `[{"kind":"bead_event","events":["closed"]}]`, true},
		{"multi entry", `[{"kind":"cron","cron":"0 9 * * *"},{"kind":"manual"}]`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := HasNonEmptyTriggers(tc.raw); got != tc.want {
				t.Errorf("HasNonEmptyTriggers(%q) = %v, want %v", tc.raw, got, tc.want)
			}
		})
	}
}

// TestTriggerSourceLabelForFields is the core of the mislabel guardrail: a
// trigger:schedule bead with a non-empty triggers[] must be promoted to
// trigger:multi; every other combination passes through unchanged.
func TestTriggerSourceLabelForFields(t *testing.T) {
	const nonEmpty = `[{"kind":"bead_event","events":["closed"]}]`
	cases := []struct {
		name        string
		sourceLabel string
		triggers    string
		want        string
	}{
		{"schedule + non-empty triggers => multi", "trigger:schedule", nonEmpty, "trigger:multi"},
		{"schedule + empty triggers stays schedule", "trigger:schedule", "[]", "trigger:schedule"},
		{"schedule + no triggers stays schedule", "trigger:schedule", "", "trigger:schedule"},
		{"schedule + malformed triggers stays schedule", "trigger:schedule", "{bad", "trigger:schedule"},
		{"already multi stays multi", "trigger:multi", nonEmpty, "trigger:multi"},
		{"non-schedule source never rewritten", "trigger:gitlab", nonEmpty, "trigger:gitlab"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := TriggerSourceLabelForFields(tc.sourceLabel, tc.triggers); got != tc.want {
				t.Errorf("TriggerSourceLabelForFields(%q, %q) = %q, want %q",
					tc.sourceLabel, tc.triggers, got, tc.want)
			}
		})
	}
}

// TestParseTriggerDef_BeadTypeFields tests parsing of the create_bead
// override fields: bead_type, bead_priority, parent, extra_labels. See
// ka-0000003075.
func TestParseTriggerDef_BeadTypeFields(t *testing.T) {
	bead := &beadsapi.BeadDetail{
		ID:    "gd-bt-1",
		Title: "Bug Filer",
		Fields: map[string]string{
			"action":        "create_bead",
			"bead_type":     "bug",
			"bead_priority": "0",
			"parent":        "gd-Epic9",
			"extra_labels":  "kind:slack-feedback,area:bridge",
		},
	}

	def := ParseTriggerDef(bead, "webhook")

	if def.BeadType != "bug" {
		t.Errorf("BeadType = %q, want bug", def.BeadType)
	}
	if def.BeadPriority == nil || *def.BeadPriority != 0 {
		t.Errorf("BeadPriority = %v, want 0", def.BeadPriority)
	}
	if def.ParentBead != "gd-Epic9" {
		t.Errorf("ParentBead = %q, want gd-Epic9", def.ParentBead)
	}
	if len(def.ExtraLabels) != 2 || def.ExtraLabels[0] != "kind:slack-feedback" {
		t.Errorf("ExtraLabels = %v", def.ExtraLabels)
	}
}

// TestParseTriggerDef_WakeIntent verifies the wake_intent field is parsed
// verbatim (and trimmed) into TriggerDef.WakeIntent — the intent half of the
// wake-intent audit (Slack thread ka-0000007909).
func TestParseTriggerDef_WakeIntent(t *testing.T) {
	bead := &beadsapi.BeadDetail{
		ID:    "gd-wi-1",
		Title: "Hourly gardener",
		Fields: map[string]string{
			"cron":        "0 * * * *",
			"wake_intent": "  Weekdays 9am PT, plus whenever open P0s > 0  ",
		},
	}
	def := ParseTriggerDef(bead, "schedule")
	if def.WakeIntent != "Weekdays 9am PT, plus whenever open P0s > 0" {
		t.Errorf("WakeIntent = %q, want trimmed intent", def.WakeIntent)
	}

	// Absent field -> empty (reconciler falls back to mechanical liveness).
	plain := &beadsapi.BeadDetail{ID: "gd-wi-2", Title: "Plain", Fields: map[string]string{}}
	if got := ParseTriggerDef(plain, "schedule").WakeIntent; got != "" {
		t.Errorf("WakeIntent = %q, want empty when absent", got)
	}
}

// TestParseTriggerDef_SlackChannel verifies the top-level slack_channel field
// is parsed (and trimmed) onto TriggerDef so a plain schedule trigger can
// declare a non-project spawn channel without a gardener block (ka-0000003647).
func TestParseTriggerDef_SlackChannel(t *testing.T) {
	bead := &beadsapi.BeadDetail{
		ID:    "gd-sc-1",
		Title: "Daily standup poster",
		Fields: map[string]string{
			"cron":          "30 15 * * 1-5",
			"action":        "spawn_agent",
			"slack_channel": "  C0DAILY0STANDUP  ",
		},
	}
	def := ParseTriggerDef(bead, "schedule")
	if def.SlackChannel != "C0DAILY0STANDUP" {
		t.Errorf("SlackChannel = %q, want trimmed channel", def.SlackChannel)
	}

	// Absent field -> empty (spawn falls back to the project default channel).
	plain := &beadsapi.BeadDetail{ID: "gd-sc-2", Title: "Plain", Fields: map[string]string{}}
	if got := ParseTriggerDef(plain, "schedule").SlackChannel; got != "" {
		t.Errorf("SlackChannel = %q, want empty when absent", got)
	}
}

// TestParseTriggerDef_BeadTypeDefaults verifies the new fields stay empty/nil
// when absent so the create_bead action keeps its task default.
func TestParseTriggerDef_BeadTypeDefaults(t *testing.T) {
	bead := &beadsapi.BeadDetail{ID: "gd-bt-2", Title: "Plain", Fields: map[string]string{}}
	def := ParseTriggerDef(bead, "webhook")
	if def.BeadType != "" {
		t.Errorf("BeadType = %q, want empty", def.BeadType)
	}
	if def.BeadPriority != nil {
		t.Errorf("BeadPriority = %v, want nil", def.BeadPriority)
	}
	if def.ParentBead != "" {
		t.Errorf("ParentBead = %q, want empty", def.ParentBead)
	}
	if len(def.ExtraLabels) != 0 {
		t.Errorf("ExtraLabels = %v, want empty", def.ExtraLabels)
	}
}

// TestParseTriggerDef_Defaults tests default values when fields are empty.
func TestParseTriggerDef_Defaults(t *testing.T) {
	bead := &beadsapi.BeadDetail{
		ID:     "gd-test-2",
		Title:  "Minimal Trigger",
		Fields: map[string]string{},
	}

	def := ParseTriggerDef(bead, "minimal")

	if def.Action != TriggerActionSpawnAgent {
		t.Errorf("default action should be spawn_agent, got %q", def.Action)
	}
	if def.MaxConcurrent != 3 {
		t.Errorf("default max_concurrent should be 3, got %d", def.MaxConcurrent)
	}
	if def.CooldownMinutes != 5 {
		t.Errorf("default cooldown_minutes should be 5, got %d", def.CooldownMinutes)
	}
	if !def.Enabled {
		t.Error("default enabled should be true")
	}
}

// TestParseTriggerDef_Triggers tests that the optional `triggers` JSON array
// field (Phase 3b/A4 multi-source fan-in) populates def.Triggers. Empty or
// invalid JSON falls back to nil without dropping the bead.
func TestParseTriggerDef_Triggers(t *testing.T) {
	t.Run("valid array", func(t *testing.T) {
		bead := &beadsapi.BeadDetail{
			ID:    "gd-trig-1",
			Title: "Multi-source",
			Fields: map[string]string{
				"triggers": `[
				  {"kind":"cron","cron":"*/5 * * * *","timezone":"UTC"},
				  {"kind":"bead_event","events":["bead.closed"]}
				]`,
			},
		}
		def := ParseTriggerDef(bead, "test")
		if got := len(def.Triggers); got != 2 {
			t.Fatalf("Triggers len = %d, want 2", got)
		}
		if def.Triggers[0].Kind != "cron" || def.Triggers[0].Cron != "*/5 * * * *" {
			t.Errorf("Triggers[0] = %+v", def.Triggers[0])
		}
		if def.Triggers[1].Kind != "bead_event" || len(def.Triggers[1].Events) != 1 {
			t.Errorf("Triggers[1] = %+v", def.Triggers[1])
		}
	})

	t.Run("invalid JSON falls back to nil", func(t *testing.T) {
		bead := &beadsapi.BeadDetail{
			ID:    "gd-trig-2",
			Title: "Bad triggers",
			Fields: map[string]string{
				"triggers": `not-valid-json`,
			},
		}
		def := ParseTriggerDef(bead, "test")
		if def.Triggers != nil {
			t.Errorf("Triggers should be nil on parse error, got %+v", def.Triggers)
		}
	})

	t.Run("empty field yields nil", func(t *testing.T) {
		bead := &beadsapi.BeadDetail{
			ID:     "gd-trig-3",
			Title:  "No triggers",
			Fields: map[string]string{},
		}
		def := ParseTriggerDef(bead, "test")
		if def.Triggers != nil {
			t.Errorf("Triggers should be nil for empty field, got %+v", def.Triggers)
		}
	})
}

// TestParseTriggerDef_Gardener tests that the optional `gardener` JSON block
// (Phase 3b/A3 gardener-loop) populates def.Gardener. Empty or invalid JSON
// leaves Gardener nil.
func TestParseTriggerDef_Gardener(t *testing.T) {
	t.Run("valid gardener block", func(t *testing.T) {
		bead := &beadsapi.BeadDetail{
			ID:    "gd-cap-1",
			Title: "Gardener",
			Fields: map[string]string{
				"gardener": `{
				  "name":"deploy-cap",
				  "terminal_goal":"ship release",
				  "run_budget":10,
				  "max_runs":5,
				  "one_spawn_at_a_time":true,
				  "stop":{"kind":"bead_closed","bead_id":"gd-epic-1"},
				  "runs_spawned":3
				}`,
			},
		}
		def := ParseTriggerDef(bead, "test")
		if def.Gardener == nil {
			t.Fatal("Gardener should be non-nil")
		}
		if def.Gardener.Name != "deploy-cap" {
			t.Errorf("Gardener.Name = %q", def.Gardener.Name)
		}
		if def.Gardener.TerminalGoal != "ship release" {
			t.Errorf("Gardener.TerminalGoal = %q", def.Gardener.TerminalGoal)
		}
		if def.Gardener.RunBudget != 10 || def.Gardener.MaxRuns != 5 {
			t.Errorf("budget/max = %d/%d", def.Gardener.RunBudget, def.Gardener.MaxRuns)
		}
		if !def.Gardener.OneSpawnAtATime {
			t.Error("OneSpawnAtATime should be true")
		}
		if def.Gardener.RunsSpawned != 3 {
			t.Errorf("RunsSpawned = %d", def.Gardener.RunsSpawned)
		}
		if len(def.Gardener.Stop) == 0 {
			t.Error("Stop raw JSON should be preserved")
		}
	})

	t.Run("invalid JSON leaves Gardener nil", func(t *testing.T) {
		bead := &beadsapi.BeadDetail{
			ID:    "gd-cap-2",
			Title: "Bad gardener",
			Fields: map[string]string{
				"gardener": `{not valid`,
			},
		}
		def := ParseTriggerDef(bead, "test")
		if def.Gardener != nil {
			t.Errorf("Gardener should be nil on parse error, got %+v", def.Gardener)
		}
	})

	t.Run("empty field yields nil Gardener", func(t *testing.T) {
		bead := &beadsapi.BeadDetail{
			ID:     "gd-cap-3",
			Title:  "No gardener",
			Fields: map[string]string{},
		}
		def := ParseTriggerDef(bead, "test")
		if def.Gardener != nil {
			t.Errorf("Gardener should be nil for empty field, got %+v", def.Gardener)
		}
	})
}

// TestTriggerEngine_SpawnAction tests the spawn_agent action through the engine.
func TestTriggerEngine_SpawnAction(t *testing.T) {
	daemon := &mockTriggerDaemon{}

	engine := NewTriggerEngine(TriggerEngineConfig{
		Daemon: daemon,
		Logger: slog.Default(),
	})

	source := &mockTriggerSource{
		name: "test",
		triggers: []TriggerDef{
			{
				ID: "t1", Title: "Spawn Test", Source: "test",
				Action: TriggerActionSpawnAgent, Role: "crew", Project: "myproj",
				Prompt: "Work on {{item_key}}: {{item_title}}", MaxConcurrent: 10,
				LastChecked: pastLastChecked(),
			},
		},
		matches: map[string][]TriggerMatch{
			"t1": {
				{
					Key:   "ITEM-1",
					Title: "Fix the widget",
					Fields: map[string]string{
						"item_key": "ITEM-1",
					},
					Labels: []string{"item:ITEM-1"},
					TemplateVars: map[string]string{
						"{{item_key}}":   "ITEM-1",
						"{{item_title}}": "Fix the widget",
					},
				},
			},
		},
	}

	engine.RegisterSource(source)
	engine.Evaluate(context.Background())

	daemon.mu.Lock()
	defer daemon.mu.Unlock()

	if len(daemon.createdBeads) != 1 {
		t.Fatalf("expected 1 created bead, got %d", len(daemon.createdBeads))
	}
	if len(daemon.spawnedNames) != 1 {
		t.Fatalf("expected 1 spawned agent, got %d", len(daemon.spawnedNames))
	}
	if daemon.spawnedRoles[0] != "crew" {
		t.Errorf("agent role = %q", daemon.spawnedRoles[0])
	}
	if daemon.lastPrompt != "Work on ITEM-1: Fix the widget" {
		t.Errorf("agent prompt = %q", daemon.lastPrompt)
	}
}

// TestTriggerEngine_PreFireDedupRefreshSuppressesDuplicate is the regression
// test for ka-0000000151: when a prior tick's fireAction partial-failed
// (e.g. createTaskBead succeeded but SpawnAgent errored), the in-memory
// seenSet was never stamped, so subsequent ticks would re-create duplicate
// task beads for the same match Key. The pre-fire dedup refresh now consults
// source.CatchUpKeys before firing, so any persisted catch-up key for an
// upcoming match suppresses the re-fire.
func TestTriggerEngine_PreFireDedupRefreshSuppressesDuplicate(t *testing.T) {
	daemon := &mockTriggerDaemon{}

	engine := NewTriggerEngine(TriggerEngineConfig{
		Daemon: daemon,
		Logger: slog.Default(),
	})

	source := &mockTriggerSource{
		name: "test",
		triggers: []TriggerDef{
			{
				ID: "t1", Title: "Spawn Test", Source: "test",
				Action: TriggerActionSpawnAgent, Role: "crew", Project: "myproj",
				Prompt: "do work", MaxConcurrent: 10,
				LastChecked: pastLastChecked(),
			},
		},
		matches: map[string][]TriggerMatch{
			"t1": {{Key: "FIRE-1", Title: "fire 1"}},
		},
		// CatchUpKeys reports FIRE-1 already persisted (orphan task bead from
		// a prior partial-failed fire). The pre-fire refresh must seed this
		// into seenSet and suppress the re-fire.
		keys: map[string][]string{
			"t1": {"FIRE-1"},
		},
	}

	engine.RegisterSource(source)
	engine.Evaluate(context.Background())

	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	if len(daemon.createdBeads) != 0 {
		t.Errorf("expected 0 created beads (orphan-suppressed), got %d", len(daemon.createdBeads))
	}
	if len(daemon.spawnedNames) != 0 {
		t.Errorf("expected 0 spawned agents (orphan-suppressed), got %d", len(daemon.spawnedNames))
	}
}

// TestTriggerEngine_PreFireDedupRefreshNoOpWhenKeysDisjoint asserts the
// refresh path is a no-op when persisted catch-up keys don't overlap with
// the upcoming match set — the fire still happens.
func TestTriggerEngine_PreFireDedupRefreshNoOpWhenKeysDisjoint(t *testing.T) {
	daemon := &mockTriggerDaemon{}

	engine := NewTriggerEngine(TriggerEngineConfig{
		Daemon: daemon,
		Logger: slog.Default(),
	})

	source := &mockTriggerSource{
		name: "test",
		triggers: []TriggerDef{
			{
				ID: "t1", Title: "Spawn Test", Source: "test",
				Action: TriggerActionSpawnAgent, Role: "crew", Project: "myproj",
				Prompt: "do work", MaxConcurrent: 10,
				LastChecked: pastLastChecked(),
			},
		},
		matches: map[string][]TriggerMatch{
			"t1": {{Key: "FIRE-2", Title: "fire 2"}},
		},
		// Older fires persisted but none for FIRE-2 — refresh seeds them
		// into seenSet but FIRE-2 still fires.
		keys: map[string][]string{
			"t1": {"FIRE-0", "FIRE-1"},
		},
	}

	engine.RegisterSource(source)
	engine.Evaluate(context.Background())

	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	if len(daemon.createdBeads) != 1 {
		t.Errorf("expected 1 created bead (disjoint keys → fire), got %d", len(daemon.createdBeads))
	}
	if len(daemon.spawnedNames) != 1 {
		t.Errorf("expected 1 spawned agent (disjoint keys → fire), got %d", len(daemon.spawnedNames))
	}
}

// testFormulaBead returns a minimal formula bead suitable for apply_formula
// tests. It declares one variable {{KEY}} (or {{BEAD}}) and one step whose
// title substitutes that variable, so callers can assert that template
// expansion ran end-to-end.
func testFormulaBead(id, varName string) *beadsapi.BeadDetail {
	stepsJSON := fmt.Sprintf(`[{"id":"s1","title":"Do thing for {{%s}}"}]`, varName)
	varsJSON := fmt.Sprintf(`[{"name":%q}]`, varName)
	return &beadsapi.BeadDetail{
		ID:    id,
		Type:  "formula",
		Title: "Test Formula",
		Fields: map[string]string{
			"vars":  varsJSON,
			"steps": stepsJSON,
		},
	}
}

// TestTriggerEngine_FormulaAction tests the apply_formula action: a fired
// trigger should instantiate the formula directly (molecule + step beads),
// not create a placeholder task with `pending_formula` set (D6 / ka-0000006284
// removed that runner) — and spawn a DRIVER agent targeting the molecule so
// the poured work is never inert (ka-0000000070, the FLAT gardener stall).
func TestTriggerEngine_FormulaAction(t *testing.T) {
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{testFormulaBead("gd-formula-1", "KEY")},
	}

	engine := NewTriggerEngine(TriggerEngineConfig{
		Daemon: daemon,
		Logger: slog.Default(),
	})

	source := &mockTriggerSource{
		name: "test",
		triggers: []TriggerDef{
			{
				ID: "t1", Title: "Formula Test", Source: "test",
				Action: TriggerActionApplyFormula, Project: "myproj",
				FormulaID: "gd-formula-1", FormulaVars: `{"KEY":"{{item_key}}"}`,
				MaxConcurrent: 10,
				LastChecked:   pastLastChecked(),
			},
		},
		matches: map[string][]TriggerMatch{
			"t1": {
				{
					Key:          "ITEM-2",
					Title:        "Apply formula",
					TemplateVars: map[string]string{"{{item_key}}": "ITEM-2"},
				},
			},
		},
	}

	engine.RegisterSource(source)
	engine.Evaluate(context.Background())

	daemon.mu.Lock()
	defer daemon.mu.Unlock()

	// 1 molecule + 1 step = 2 CreateBead calls.
	if len(daemon.createdBeads) != 2 {
		t.Fatalf("expected 2 created beads (molecule + 1 step), got %d", len(daemon.createdBeads))
	}
	mol := daemon.createdBeads[0]
	if mol.Type != "molecule" {
		t.Errorf("first created bead type = %q, want molecule", mol.Type)
	}
	step := daemon.createdBeads[1]
	if step.Title != "Do thing for ITEM-2" {
		t.Errorf("step title = %q, template substitution failed", step.Title)
	}
	// Driver spawn: one agent, targeting the molecule bead, carrying the
	// spawned_by field the max_concurrent live lease keys on.
	if len(daemon.spawnedNames) != 1 {
		t.Fatalf("expected 1 spawned driver for formula action, got %d", len(daemon.spawnedNames))
	}
	if daemon.spawnedTaskIDs[0] != "gd-trigger-bead-1" {
		t.Errorf("driver taskID = %q, want molecule gd-trigger-bead-1", daemon.spawnedTaskIDs[0])
	}
	if daemon.spawnedRoles[0] != "crew" {
		t.Errorf("driver role = %q, want crew (default)", daemon.spawnedRoles[0])
	}
	if got := daemon.spawnedFields[0]["spawned_by"]; got != "trigger:t1" {
		t.Errorf("driver spawned_by = %q, want trigger:t1", got)
	}
	if got := daemon.spawnedFields[0]["single_claim_task"]; got != "1" {
		t.Errorf("driver single_claim_task = %q, want 1", got)
	}
	// The match key is stamped under the generic, schema-registered source_key
	// field for an unregistered source ("test"), NOT an invalid "<source>_key"
	// like "test_key" that the type:agent schema would 400 on (ka-0000001885).
	if got := daemon.spawnedFields[0]["source_key"]; got != "ITEM-2" {
		t.Errorf("driver source_key = %q, want ITEM-2", got)
	}
	if _, bad := daemon.spawnedFields[0]["test_key"]; bad {
		t.Errorf("driver stamped invalid unregistered field test_key: %v", daemon.spawnedFields[0])
	}

	// Non-gardener spawn-producing fire stamps last_run so the trigger no
	// longer reads as frozen while pouring on cadence (ka-0000001622).
	var sawLastRun bool
	for i, id := range daemon.updatedIDs {
		if id == "t1" && daemon.updatedFields[i]["last_run"] != "" {
			sawLastRun = true
			if got := daemon.updatedFields[i]["last_agent_id"]; got != "gd-agent-1" {
				t.Errorf("last_agent_id = %q, want gd-agent-1", got)
			}
		}
	}
	if !sawLastRun {
		t.Errorf("expected last_run stamped on trigger t1 after fire (updates=%v)", daemon.updatedFields)
	}
}

// TestTriggerEngine_FormulaAction_BeadSourceKeyField is the direct regression
// for ka-0000001885: a BEAD-source apply_formula trigger. Before the fix,
// triggerSourceFieldKey("bead") returned "bead_key" — an unregistered
// type:agent field — so the driver spawn 400'd "bead_key: unknown field" and
// the poured molecule was left driver-less on every fire. The driver match key
// must now land under the generic, schema-registered "source_key" field and
// never under "bead_key".
func TestTriggerEngine_FormulaAction_BeadSourceKeyField(t *testing.T) {
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{testFormulaBead("gd-formula-bead", "KEY")},
	}
	engine := NewTriggerEngine(TriggerEngineConfig{
		Daemon: daemon,
		Logger: slog.Default(),
	})

	source := &mockTriggerSource{
		name: "bead",
		triggers: []TriggerDef{
			{
				ID: "t-bead", Title: "Bead Formula", Source: "bead",
				Action: TriggerActionApplyFormula, Project: "myproj",
				FormulaID: "gd-formula-bead", FormulaVars: `{"KEY":"{{item_key}}"}`,
				MaxConcurrent: 10,
				LastChecked:   pastLastChecked(),
			},
		},
		matches: map[string][]TriggerMatch{
			"t-bead": {
				{
					Key:          "gd-src-99",
					Title:        "Apply formula on bead",
					TemplateVars: map[string]string{"{{item_key}}": "gd-src-99"},
				},
			},
		},
	}

	engine.RegisterSource(source)
	engine.Evaluate(context.Background())

	daemon.mu.Lock()
	defer daemon.mu.Unlock()

	if len(daemon.spawnedFields) != 1 {
		t.Fatalf("expected 1 spawned driver for bead-source formula action, got %d", len(daemon.spawnedFields))
	}
	f := daemon.spawnedFields[0]
	if got := f["source_key"]; got != "gd-src-99" {
		t.Errorf("driver source_key = %q, want gd-src-99", got)
	}
	if _, bad := f["bead_key"]; bad {
		t.Errorf("driver stamped invalid unregistered field bead_key (the ka-0000001885 bug): %v", f)
	}
}

// TestTriggerEngine_FormulaAction_DriverSpawnFailureKeepsPour asserts the
// non-fatal contract: when the driver spawn fails AFTER a successful pour,
// the fire itself still succeeds (match marked seen) so the next evaluation
// cycle does NOT re-pour a duplicate molecule — molecules carry no per-fire
// dedup key that CatchUpKeys could recover, so a failed fire here would
// re-pour forever. The orphaned molecule is rescued by the
// poured-but-unworked liveness path (ka-0000000957), not by refiring.
func TestTriggerEngine_FormulaAction_DriverSpawnFailureKeepsPour(t *testing.T) {
	daemon := &mockTriggerDaemon{
		beads:    []*beadsapi.BeadDetail{testFormulaBead("gd-formula-sf", "KEY")},
		spawnErr: fmt.Errorf("boom: no spawn capacity"),
	}
	engine := NewTriggerEngine(TriggerEngineConfig{
		Daemon: daemon,
		Logger: slog.Default(),
	})

	source := &mockTriggerSource{
		name: "test",
		triggers: []TriggerDef{
			{
				ID: "t-sf", Title: "Spawn-fail formula", Source: "test",
				Action: TriggerActionApplyFormula, Project: "gardenio",
				FormulaID: "gd-formula-sf", FormulaVars: `{"KEY":"{{item_key}}"}`,
				MaxConcurrent: 10,
				LastChecked:   pastLastChecked(),
			},
		},
		matches: map[string][]TriggerMatch{
			"t-sf": {
				{
					Key:          "ITEM-9",
					Title:        "Apply formula",
					TemplateVars: map[string]string{"{{item_key}}": "ITEM-9"},
				},
			},
		},
	}

	engine.RegisterSource(source)
	engine.Evaluate(context.Background())

	countMolecules := func() int {
		daemon.mu.Lock()
		defer daemon.mu.Unlock()
		n := 0
		for _, b := range daemon.createdBeads {
			if b.Type == "molecule" {
				n++
			}
		}
		return n
	}

	// Pour happened (molecule + step), even though the spawn failed.
	if got := countMolecules(); got != 1 {
		t.Fatalf("expected 1 molecule despite spawn failure, got %d", got)
	}

	// The match must be marked seen so a second cycle does not re-pour.
	if !engine.SeenKeys("t-sf")["ITEM-9"] {
		t.Error("match not marked seen after spawn-failed pour — next cycle would duplicate the molecule")
	}
	engine.Evaluate(context.Background())
	if got := countMolecules(); got != 1 {
		t.Errorf("second evaluation re-poured: %d molecules, want 1", got)
	}
}

// TestTriggerEngine_FormulaAction_RoleAndAssignedAgentOverrides asserts that
// the driver honors the trigger's `role` field and the formula's
// `assigned_agent` pinned identity (the same override the Slack pour path
// honors in handleFormulaPour).
func TestTriggerEngine_FormulaAction_RoleAndAssignedAgentOverrides(t *testing.T) {
	formula := testFormulaBead("gd-formula-ov", "KEY")
	formula.Fields["assigned_agent"] = "flat-driver"
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{formula},
	}
	engine := NewTriggerEngine(TriggerEngineConfig{
		Daemon: daemon,
		Logger: slog.Default(),
	})

	source := &mockTriggerSource{
		name: "test",
		triggers: []TriggerDef{
			{
				ID: "t-ov", Title: "Override formula", Source: "test",
				Action: TriggerActionApplyFormula, Project: "gardenio",
				Role:      "gardener",
				FormulaID: "gd-formula-ov", FormulaVars: `{"KEY":"{{item_key}}"}`,
				MaxConcurrent: 10,
				LastChecked:   pastLastChecked(),
			},
		},
		matches: map[string][]TriggerMatch{
			"t-ov": {
				{
					Key:          "ITEM-5",
					Title:        "Apply formula",
					TemplateVars: map[string]string{"{{item_key}}": "ITEM-5"},
				},
			},
		},
	}

	engine.RegisterSource(source)
	engine.Evaluate(context.Background())

	daemon.mu.Lock()
	defer daemon.mu.Unlock()

	if len(daemon.spawnedNames) != 1 {
		t.Fatalf("expected 1 spawned driver, got %d", len(daemon.spawnedNames))
	}
	if daemon.spawnedNames[0] != "flat-driver" {
		t.Errorf("driver name = %q, want assigned_agent override flat-driver", daemon.spawnedNames[0])
	}
	if daemon.spawnedRoles[0] != "gardener" {
		t.Errorf("driver role = %q, want trigger role gardener", daemon.spawnedRoles[0])
	}
}

// TestTriggerEngine_FormulaAction_LiveDriverDefersNextPour asserts that the
// max_concurrent live lease — which fireAction already counted apply_formula
// toward — actually bites now that the action spawns: with max_concurrent=1
// and a live driver from a prior pour (agent bead carrying
// spawned_by=trigger:<id>), a new match must NOT pour a second molecule.
// This is the idempotency half of ka-0000000070: pours don't stack while a
// driver is still working.
func TestTriggerEngine_FormulaAction_LiveDriverDefersNextPour(t *testing.T) {
	liveDriver := &beadsapi.BeadDetail{
		ID:     "gd-agent-live",
		Type:   "agent",
		Status: "in_progress",
		Title:  "gardenio-prior-driver-a1b2",
		Fields: map[string]string{"spawned_by": "trigger:t-lease"},
	}
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{testFormulaBead("gd-formula-ls", "KEY"), liveDriver},
	}
	engine := NewTriggerEngine(TriggerEngineConfig{
		Daemon: daemon,
		Logger: slog.Default(),
	})

	source := &mockTriggerSource{
		name: "test",
		triggers: []TriggerDef{
			{
				ID: "t-lease", Title: "Leased formula", Source: "test",
				Action: TriggerActionApplyFormula, Project: "gardenio",
				FormulaID: "gd-formula-ls", FormulaVars: `{"KEY":"{{item_key}}"}`,
				MaxConcurrent: 1,
				LastChecked:   pastLastChecked(),
			},
		},
		matches: map[string][]TriggerMatch{
			"t-lease": {
				{
					Key:          "ITEM-7",
					Title:        "Apply formula",
					TemplateVars: map[string]string{"{{item_key}}": "ITEM-7"},
				},
			},
		},
	}

	engine.RegisterSource(source)
	engine.Evaluate(context.Background())

	daemon.mu.Lock()
	defer daemon.mu.Unlock()

	for _, b := range daemon.createdBeads {
		if b.Type == "molecule" {
			t.Errorf("poured a molecule while a live driver holds the lease (created=%q)", b.Title)
		}
	}
	if len(daemon.spawnedNames) != 0 {
		t.Errorf("spawned %d agents while lease is held, want 0", len(daemon.spawnedNames))
	}
}

// TestTriggerEngine_FormulaAction_FireTimeISOAlias asserts that a trigger
// authoring formula_vars with {{fire_time_iso}} — the widely-used alias that
// no source ever published — resolves to the source's {{fire_time}} value at
// the fireAction chokepoint instead of passing through as a literal
// (ka-0000006473: 180+ stranded "({{fire_time_iso}})" step beads).
func TestTriggerEngine_FormulaAction_FireTimeISOAlias(t *testing.T) {
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{testFormulaBead("gd-formula-ft", "ITERATION")},
	}
	engine := NewTriggerEngine(TriggerEngineConfig{
		Daemon: daemon,
		Logger: slog.Default(),
	})

	source := &mockTriggerSource{
		name: "schedule",
		triggers: []TriggerDef{
			{
				ID: "t-ft", Title: "CDQR Audit Gardener", Source: "schedule",
				Action: TriggerActionApplyFormula, Project: "gardenio",
				FormulaID:     "gd-formula-ft",
				FormulaVars:   `{"ITERATION":"{{fire_time_iso}}"}`,
				MaxConcurrent: 10,
				LastChecked:   pastLastChecked(),
			},
		},
		matches: map[string][]TriggerMatch{
			"t-ft": {
				{
					Key:   "2026-07-02T07:47",
					Title: "Schedule fire",
					// Schedule source publishes {{fire_time}} but not
					// {{fire_time_iso}} — the exact prod shape.
					TemplateVars: map[string]string{
						"{{fire_time}}": "2026-07-02T07:47:00Z",
					},
				},
			},
		},
	}

	engine.RegisterSource(source)
	engine.Evaluate(context.Background())

	daemon.mu.Lock()
	defer daemon.mu.Unlock()

	if len(daemon.createdBeads) != 2 {
		t.Fatalf("expected 2 created beads (molecule + step), got %d (fire should succeed, not be rejected)", len(daemon.createdBeads))
	}
	step := daemon.createdBeads[1]
	if step.Title != "Do thing for 2026-07-02T07:47:00Z" {
		t.Errorf("step title = %q, want fire time substituted for {{fire_time_iso}}", step.Title)
	}
}

// TestTriggerEngine_FormulaAction_FireTimeFallbackForEventSources asserts that
// event-driven sources that publish no fire time at all (bead, jira, gitlab)
// still resolve {{fire_time}} / {{fire_time_iso}} — ensureFireTimeVars fills
// them with the evaluation time so a fire-time placeholder never survives as
// a literal.
func TestTriggerEngine_FormulaAction_FireTimeFallbackForEventSources(t *testing.T) {
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{testFormulaBead("gd-formula-ev", "ITERATION")},
	}
	engine := NewTriggerEngine(TriggerEngineConfig{
		Daemon: daemon,
		Logger: slog.Default(),
	})

	source := &mockTriggerSource{
		name: "bead",
		triggers: []TriggerDef{
			{
				ID: "t-ev", Title: "Event formula", Source: "bead",
				Action: TriggerActionApplyFormula, Project: "gardenio",
				FormulaID:     "gd-formula-ev",
				FormulaVars:   `{"ITERATION":"{{fire_time_iso}}"}`,
				MaxConcurrent: 10,
				LastChecked:   pastLastChecked(),
			},
		},
		matches: map[string][]TriggerMatch{
			"t-ev": {
				{
					Key:          "gd-src-9:closed",
					Title:        "Bead closed",
					TemplateVars: map[string]string{"{{bead_id}}": "gd-src-9"},
				},
			},
		},
	}

	engine.RegisterSource(source)
	engine.Evaluate(context.Background())

	daemon.mu.Lock()
	defer daemon.mu.Unlock()

	if len(daemon.createdBeads) != 2 {
		t.Fatalf("expected 2 created beads, got %d", len(daemon.createdBeads))
	}
	step := daemon.createdBeads[1]
	if strings.Contains(step.Title, "{{") {
		t.Errorf("step title %q still contains an unexpanded placeholder", step.Title)
	}
	// The fallback stamps an RFC3339 UTC timestamp.
	stamp := strings.TrimPrefix(step.Title, "Do thing for ")
	if _, err := time.Parse(time.RFC3339, stamp); err != nil {
		t.Errorf("step title %q: expected RFC3339 fire-time fallback, parse error: %v", step.Title, err)
	}
}

// TestTriggerEngine_FormulaAction_RejectsUnresolvedVars asserts the guard: a
// formula_vars value that still contains a {{...}} placeholder after template
// expansion (an unknown var the source never published) must fail the fire
// instead of baking the literal into molecule applied_vars and step titles
// (ka-0000006473).
func TestTriggerEngine_FormulaAction_RejectsUnresolvedVars(t *testing.T) {
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{testFormulaBead("gd-formula-bad", "ITERATION")},
	}
	engine := NewTriggerEngine(TriggerEngineConfig{
		Daemon: daemon,
		Logger: slog.Default(),
	})

	source := &mockTriggerSource{
		name: "schedule",
		triggers: []TriggerDef{
			{
				ID: "t-bad", Title: "Typo'd vars", Source: "schedule",
				Action: TriggerActionApplyFormula, Project: "gardenio",
				FormulaID: "gd-formula-bad",
				// {{fire_thyme}} is not published by any source and has no
				// alias — must be rejected, not passed through.
				FormulaVars:   `{"ITERATION":"{{fire_thyme}}"}`,
				MaxConcurrent: 10,
				LastChecked:   pastLastChecked(),
			},
		},
		matches: map[string][]TriggerMatch{
			"t-bad": {
				{
					Key:          "2026-07-02T08:00",
					Title:        "Schedule fire",
					TemplateVars: map[string]string{"{{fire_time}}": "2026-07-02T08:00:00Z"},
				},
			},
		},
	}

	engine.RegisterSource(source)
	engine.Evaluate(context.Background())

	daemon.mu.Lock()
	defer daemon.mu.Unlock()

	// No molecule and no step beads — the fire must fail. (The audit event
	// bead recording the error outcome is created with type=event; filter to
	// molecule/task creates only.)
	for _, b := range daemon.createdBeads {
		if b.Type == "molecule" || b.Type == "task" {
			t.Errorf("guard failed: created %s bead %q despite unresolved placeholder", b.Type, b.Title)
		}
	}
}

// TestEnsureFireTimeVars covers the alias/fallback matrix directly.
func TestEnsureFireTimeVars(t *testing.T) {
	now := time.Date(2026, 7, 2, 8, 0, 0, 0, time.UTC)

	t.Run("aliases fire_time_iso to existing fire_time", func(t *testing.T) {
		m := TriggerMatch{TemplateVars: map[string]string{"{{fire_time}}": "2026-07-02T07:47:00Z"}}
		ensureFireTimeVars(&m, now)
		if got := m.TemplateVars["{{fire_time_iso}}"]; got != "2026-07-02T07:47:00Z" {
			t.Errorf("fire_time_iso = %q, want aliased fire_time", got)
		}
	})

	t.Run("fills both from now when absent", func(t *testing.T) {
		m := TriggerMatch{TemplateVars: map[string]string{"{{bead_id}}": "gd-x"}}
		ensureFireTimeVars(&m, now)
		want := "2026-07-02T08:00:00Z"
		if got := m.TemplateVars["{{fire_time}}"]; got != want {
			t.Errorf("fire_time = %q, want %q", got, want)
		}
		if got := m.TemplateVars["{{fire_time_iso}}"]; got != want {
			t.Errorf("fire_time_iso = %q, want %q", got, want)
		}
	})

	t.Run("nil TemplateVars map is initialized", func(t *testing.T) {
		m := TriggerMatch{}
		ensureFireTimeVars(&m, now)
		if m.TemplateVars["{{fire_time}}"] == "" {
			t.Error("fire_time not filled on nil map")
		}
	})

	t.Run("existing values win", func(t *testing.T) {
		m := TriggerMatch{TemplateVars: map[string]string{
			"{{fire_time}}":     "a",
			"{{fire_time_iso}}": "b",
		}}
		ensureFireTimeVars(&m, now)
		if m.TemplateVars["{{fire_time}}"] != "a" || m.TemplateVars["{{fire_time_iso}}"] != "b" {
			t.Errorf("existing values overwritten: %v", m.TemplateVars)
		}
	})
}

// TestFindUnresolvedVars covers the guard helper.
func TestFindUnresolvedVars(t *testing.T) {
	got := findUnresolvedVars(map[string]string{
		"OK":        "2026-07-02T07:47:00Z",
		"BAD":       "{{fire_time_iso}}",
		"EMBED":     "prefix {{thing}} suffix",
		"EMPTY":     "",
		"NOT_A_VAR": "{{not closed",
	})
	want := []string{`BAD="{{fire_time_iso}}"`, `EMBED="prefix {{thing}} suffix"`}
	if len(got) != len(want) {
		t.Fatalf("findUnresolvedVars = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("findUnresolvedVars[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestTriggerEngine_FormulaAction_PropagatesSourceBeadLineage asserts that the
// apply_formula action labels the molecule with project and stamps the source
// bead via `triggered-by` deps. With D6 (ka-0000006284) the trigger now
// creates the molecule directly — lineage flows through graph edges, not
// through `pending_formula` placeholder fields.
func TestTriggerEngine_FormulaAction_PropagatesSourceBeadLineage(t *testing.T) {
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{testFormulaBead("gd-formula-x", "BEAD")},
	}
	engine := NewTriggerEngine(TriggerEngineConfig{
		Daemon: daemon,
		Logger: slog.Default(),
	})

	source := &mockTriggerSource{
		name: "bead",
		triggers: []TriggerDef{
			{
				ID: "t-lin", Title: "Apply formula on incoming bead",
				Source: "bead", Action: TriggerActionApplyFormula,
				Project:       "gardenio",
				FormulaID:     "gd-formula-x",
				FormulaVars:   `{"BEAD":"{{bead_id}}"}`,
				MaxConcurrent: 10,
				LastChecked:   pastLastChecked(),
			},
		},
		matches: map[string][]TriggerMatch{
			"t-lin": {
				{
					Key:    "gd-src-1:created",
					Title:  "Incoming bead",
					Labels: []string{"source-bead:gd-src-1"},
					Fields: map[string]string{
						"source_bead_id": "gd-src-1",
						"source_event":   "created",
						"bead_event_key": "gd-src-1:created",
					},
					TemplateVars: map[string]string{"{{bead_id}}": "gd-src-1"},
				},
			},
		},
	}

	engine.RegisterSource(source)
	engine.Evaluate(context.Background())

	daemon.mu.Lock()
	defer daemon.mu.Unlock()

	// 1 molecule + 1 step = 2 created beads.
	if len(daemon.createdBeads) != 2 {
		t.Fatalf("expected 2 created beads (molecule + step), got %d", len(daemon.createdBeads))
	}
	mol := daemon.createdBeads[0]
	hasLabel := func(want string) bool {
		for _, l := range mol.Labels {
			if l == want {
				return true
			}
		}
		return false
	}
	if !hasLabel("project:gardenio") {
		t.Errorf("molecule missing project label (labels=%v)", mol.Labels)
	}

	// triggered-by lineage: molecule → trigger bead AND molecule → source bead.
	const molID = "gd-trigger-bead-1"
	wantTargets := map[string]bool{"t-lin": false, "gd-src-1": false}
	for _, d := range daemon.addedDeps {
		if d.BeadID == molID && d.DepType == "triggered-by" {
			if _, ok := wantTargets[d.DependsOnID]; ok {
				wantTargets[d.DependsOnID] = true
			}
		}
	}
	for target, seen := range wantTargets {
		if !seen {
			t.Errorf("missing triggered-by dep from molecule to %q (deps=%+v)", target, daemon.addedDeps)
		}
	}
}

// TestTriggerEngine_FormulaAction_WritesTriggeredByDeps asserts that the
// apply_formula trigger action (epic ka-0000007507 D1/D4/D6) stamps explicit
// `triggered-by` lineage edges from the molecule bead onto BOTH the
// trigger-definition bead AND the source bead that fired the match. With D6
// (ka-0000006284) the molecule is created directly by the trigger, so the
// edges land on the molecule (not a placeholder task bead).
func TestTriggerEngine_FormulaAction_WritesTriggeredByDeps(t *testing.T) {
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{testFormulaBead("gd-formula-x", "BEAD")},
	}
	engine := NewTriggerEngine(TriggerEngineConfig{
		Daemon: daemon,
		Logger: slog.Default(),
	})

	source := &mockTriggerSource{
		name: "bead",
		triggers: []TriggerDef{
			{
				ID: "t-lin-dep", Title: "Apply formula on incoming bead",
				Source: "bead", Action: TriggerActionApplyFormula,
				Project:       "gardenio",
				FormulaID:     "gd-formula-x",
				FormulaVars:   `{"BEAD":"{{bead_id}}"}`,
				MaxConcurrent: 10,
				LastChecked:   pastLastChecked(),
			},
		},
		matches: map[string][]TriggerMatch{
			"t-lin-dep": {
				{
					Key:   "gd-src-2:created",
					Title: "Incoming bead",
					Fields: map[string]string{
						"source_bead_id": "gd-src-2",
						"source_event":   "created",
					},
					TemplateVars: map[string]string{"{{bead_id}}": "gd-src-2"},
				},
			},
		},
	}

	engine.RegisterSource(source)
	engine.Evaluate(context.Background())

	daemon.mu.Lock()
	defer daemon.mu.Unlock()

	// 1 molecule + 1 step.
	if len(daemon.createdBeads) != 2 {
		t.Fatalf("expected 2 created beads (molecule + step), got %d", len(daemon.createdBeads))
	}
	// The molecule is created first by instantiateFormula, so its ID is the
	// first synthetic ID handed out by mockTriggerDaemon.CreateBead.
	const molID = "gd-trigger-bead-1"

	wantDeps := map[string]bool{
		"t-lin-dep": false, // trigger bead
		"gd-src-2":  false, // source bead
	}
	for _, d := range daemon.addedDeps {
		// Ignore the parent-child edge that links step → molecule; this test
		// only cares about triggered-by lineage from the molecule.
		if d.DepType != "triggered-by" {
			continue
		}
		if d.BeadID != molID {
			t.Errorf("unexpected triggered-by from bead %q (want molecule %q): %+v", d.BeadID, molID, d)
			continue
		}
		if d.CreatedBy != "trigger:t-lin-dep" {
			t.Errorf("created_by = %q, want %q (dep=%+v)", d.CreatedBy, "trigger:t-lin-dep", d)
		}
		if _, ok := wantDeps[d.DependsOnID]; !ok {
			t.Errorf("unexpected dep target %q (dep=%+v)", d.DependsOnID, d)
			continue
		}
		wantDeps[d.DependsOnID] = true
	}
	for target, seen := range wantDeps {
		if !seen {
			t.Errorf("missing triggered-by dep on %q (deps=%+v)", target, daemon.addedDeps)
		}
	}
}

// TestTriggerEngine_FormulaAction_NoSourceBeadIDStillWritesTriggerDep covers
// the no-source-bead path (e.g. JIRA / GitLab triggers that don't emit a
// `source_bead_id` field). The trigger-bead lineage edge must still land on
// the molecule. With D6 (ka-0000006284) we also get parent-child edges from
// each step bead to the molecule; this test counts only the triggered-by
// deps.
func TestTriggerEngine_FormulaAction_NoSourceBeadIDStillWritesTriggerDep(t *testing.T) {
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{testFormulaBead("gd-formula-jira", "KEY")},
	}
	engine := NewTriggerEngine(TriggerEngineConfig{
		Daemon: daemon,
		Logger: slog.Default(),
	})

	source := &mockTriggerSource{
		name: "jira",
		triggers: []TriggerDef{
			{
				ID: "t-jira", Title: "JIRA-driven formula",
				Source: "jira", Action: TriggerActionApplyFormula,
				Project:       "gardenio",
				FormulaID:     "gd-formula-jira",
				FormulaVars:   `{"KEY":"{{jira_key}}"}`,
				MaxConcurrent: 10,
				LastChecked:   pastLastChecked(),
			},
		},
		matches: map[string][]TriggerMatch{
			"t-jira": {
				{
					Key:          "ACME-1",
					Title:        "JIRA issue",
					Fields:       map[string]string{"jira_key": "ACME-1"},
					TemplateVars: map[string]string{"{{jira_key}}": "ACME-1"},
				},
			},
		},
	}

	engine.RegisterSource(source)
	engine.Evaluate(context.Background())

	daemon.mu.Lock()
	defer daemon.mu.Unlock()

	const molID = "gd-trigger-bead-1"
	var triggeredBy []triggerMockDep
	for _, d := range daemon.addedDeps {
		if d.DepType == "triggered-by" {
			triggeredBy = append(triggeredBy, d)
		}
	}
	if len(triggeredBy) != 1 {
		t.Fatalf("expected exactly 1 triggered-by dep (trigger bead only), got %d: %+v",
			len(triggeredBy), triggeredBy)
	}
	d := triggeredBy[0]
	if d.BeadID != molID || d.DependsOnID != "t-jira" {
		t.Errorf("dep = %+v, want bead=%q depends_on=t-jira", d, molID)
	}
}

// TestTriggerSourceFieldKey covers the agent-bead match-key field name chosen
// per trigger source. Registered sources (jira/gitlab/schedule/schedule-mirror/
// multi) keep their dedicated `<source>_key` field — including hyphenated names
// like "schedule-mirror" that must sanitize to "schedule_mirror_key", NOT the
// invalid "schedule-mirror_key" (ka-0000000883). Every OTHER source (bead,
// account, slack-reaction, dynamic webhook names) maps to the generic,
// always-valid "source_key" field — before ka-0000001885 these produced
// UNREGISTERED fields like "bead_key" that 400'd every bead-source spawn.
func TestTriggerSourceFieldKey(t *testing.T) {
	tests := []struct {
		source string
		want   string
	}{
		// Registered sources keep their dedicated key field.
		{"jira", "jira_key"},
		{"gitlab", "gitlab_key"},
		{"schedule", "schedule_key"},
		{"schedule-mirror", "schedule_mirror_key"},
		{"multi", "multi_key"},
		// Unregistered sources fall back to the generic source_key. "bead" is
		// the exact source that surfaced the bug (ka-0000001885).
		{"bead", "source_key"},
		{"account", "source_key"},
		{"slack-reaction", "source_key"},
		{"alert", "source_key"}, // operator-named webhook source
		{"a-b-c", "source_key"},
		{"", "source_key"},
	}
	identRE := regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	for _, tt := range tests {
		t.Run(tt.source, func(t *testing.T) {
			got := triggerSourceFieldKey(tt.source)
			if got != tt.want {
				t.Errorf("triggerSourceFieldKey(%q) = %q, want %q", tt.source, got, tt.want)
			}
			if !identRE.MatchString(got) {
				t.Errorf("triggerSourceFieldKey(%q) = %q is not a valid identifier", tt.source, got)
			}
		})
	}
}

func TestSanitizeTriggerAgentName(t *testing.T) {
	tests := []struct {
		name       string
		project    string
		matchKey   string
		wantPrefix string // prefix before the random suffix
	}{
		{
			"pipeline key with project path",
			"gardenio",
			"Acme/Platform/gardenio/pipeline/12345",
			"gardenio-pipeline-12345-",
		},
		{
			"MR key with sigil",
			"monorepo",
			"Acme/Platform/monorepo!42",
			"monorepo-platform-monorepo-mr-42-",
		},
		{
			"simple JIRA key",
			"gardenio",
			"PE-1234",
			"gardenio-pe-1234-",
		},
		{
			"no project",
			"",
			"Acme/Platform/gardenio/pipeline/99",
			"trigger-pipeline-99-",
		},
		{
			"short key",
			"myproj",
			"ITEM-1",
			"myproj-item-1-",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sanitizeTriggerAgentName(tt.project, tt.matchKey)

			// Check it starts with the expected prefix.
			if !hasPrefix(got, tt.wantPrefix) {
				t.Errorf("sanitizeTriggerAgentName(%q, %q) = %q, want prefix %q", tt.project, tt.matchKey, got, tt.wantPrefix)
			}

			// Check K8s name validity: only lowercase alphanumeric and hyphens.
			for _, c := range got {
				if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-') {
					t.Errorf("invalid char %q in name %q", string(c), got)
				}
			}

			// Check it doesn't contain consecutive hyphens.
			if containsDoubleDash(got) {
				t.Errorf("name %q contains consecutive hyphens", got)
			}

			// Check reasonable length (should fit in K8s pod name).
			if len(got) > 63 {
				t.Errorf("name %q is %d chars, exceeds 63", got, len(got))
			}
		})
	}
}

func hasPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}

func containsDoubleDash(s string) bool {
	for i := 1; i < len(s); i++ {
		if s[i] == '-' && s[i-1] == '-' {
			return true
		}
	}
	return false
}

// TestSanitizeTriggerAgentName_LongKey tests truncation of very long keys.
func TestSanitizeTriggerAgentName_LongKey(t *testing.T) {
	longKey := "very/long/project/path/with/many/segments/pipeline/9999999999"
	name := sanitizeTriggerAgentName("gardenio", longKey)

	// Total should be reasonable for K8s pod naming.
	if len(name) > 63 {
		t.Errorf("name %q is %d chars, exceeds 63", name, len(name))
	}

	// Should still contain the pipeline ID.
	if !containsSubstring(name, "9999999999") {
		t.Errorf("name %q should contain pipeline ID", name)
	}
}

// TestTriggerAgentName_PrefersTitle verifies the new title-based slug path:
// when the trigger title carries semantic context, that beats the opaque
// match key (e.g. schedule-mirror's "0:0:2026-..." tuple).
func TestTriggerAgentName_PrefersTitle(t *testing.T) {
	tests := []struct {
		name       string
		trig       TriggerDef
		matchKey   string
		wantPrefix string
	}{
		{
			"gardener framing stripped + trailing kd ref stripped",
			TriggerDef{Project: "gardenio", Title: "Gardener: AWS POC litellm enablement (ka-0000004609)"},
			"0:0:2026-05-27T01:19",
			"gardenio-aws-poc-litellm-enablement-",
		},
		{
			"scheduled framing stripped + cadence parens stripped",
			TriggerDef{Project: "gardenio", Title: "Scheduled: Mezmo Alert Custodian (hourly)"},
			"0:0:2026-05-27T01:19",
			"gardenio-mezmo-alert-custodian-",
		},
		{
			"jira-style title beats jira key",
			TriggerDef{Project: "gardenio", Title: "JIRA PE Triage"},
			"PE-1234",
			"gardenio-jira-pe-triage-",
		},
		{
			"empty title falls back to match key slug",
			TriggerDef{Project: "monorepo"},
			"Acme/Platform/monorepo!42",
			"monorepo-platform-monorepo-mr-42-",
		},
		{
			"title-only + no project",
			TriggerDef{Title: "GitLab Pipeline 12345"},
			"Acme/Platform/gardenio/pipeline/12345",
			"trigger-gitlab-pipeline-12345-",
		},
		{
			"all-symbol title falls back to match key",
			TriggerDef{Project: "gardenio", Title: "(((**)))"},
			"PE-1234",
			"gardenio-pe-1234-",
		},
		{
			"empty title and empty match key",
			TriggerDef{Project: "gardenio"},
			"",
			"gardenio-trigger-",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := triggerAgentName(tt.trig, tt.matchKey)

			if !hasPrefix(got, tt.wantPrefix) {
				t.Errorf("triggerAgentName(%+v, %q) = %q, want prefix %q", tt.trig, tt.matchKey, got, tt.wantPrefix)
			}
			for _, c := range got {
				if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-') {
					t.Errorf("invalid char %q in name %q", string(c), got)
				}
			}
			if containsDoubleDash(got) {
				t.Errorf("name %q contains consecutive hyphens", got)
			}
			if len(got) > 63 {
				t.Errorf("name %q is %d chars, exceeds 63", got, len(got))
			}
		})
	}
}

// TestSlugifyTriggerTitle covers the framing-stripping rules in isolation.
func TestSlugifyTriggerTitle(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"", ""},
		{"Gardener: AWS POC litellm enablement (ka-0000004609)", "aws-poc-litellm-enablement"},
		{"Scheduled: Mezmo Alert Custodian (hourly)", "mezmo-alert-custodian"},
		{"scheduled:   Another Thing  ", "another-thing"},
		{"ScheduleMirror: Gardener: nested framing (gd-x)", "nested-framing"},
		{"plain title with no framing", "plain-title-with-no-framing"},
		{"(((**)))", ""},
		{"Title with mid (paren) and trailing (gd-x)", "title-with-mid-paren-and-trailin"},
		{"a very long title that exceeds the 32 character budget and gets truncated", "a-very-long-title-that-exceeds-t"},
	}
	for _, tt := range tests {
		got := slugifyTriggerTitle(tt.in)
		if got != tt.want {
			t.Errorf("slugifyTriggerTitle(%q) = %q, want %q", tt.in, got, tt.want)
		}
		if len(got) > 32 {
			t.Errorf("slugifyTriggerTitle(%q) = %q exceeds 32 chars", tt.in, got)
		}
	}
}

func containsSubstring(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// TestTriggerEngine_PruneStaleTriggers tests that seen entries for removed
// triggers are cleaned up after evaluation.
func TestTriggerEngine_PruneStaleTriggers(t *testing.T) {
	daemon := &mockTriggerDaemon{}

	engine := NewTriggerEngine(TriggerEngineConfig{
		Daemon: daemon,
		Logger: slog.Default(),
	})

	// Seed seen state for two triggers: one that will remain active and one
	// that will be stale (not returned by LoadTriggers).
	engine.SeedSeen("t-active", "K1")
	engine.SeedSeen("t-active", "K2")
	engine.SeedSeen("t-stale", "K3")
	engine.SeedSeen("t-stale", "K4")
	engine.SeedSeen("t-stale", "K5")

	// Register a source that only knows about t-active.
	source := &mockTriggerSource{
		name: "test",
		triggers: []TriggerDef{
			{ID: "t-active", Title: "Active", Source: "test", Action: TriggerActionNotify, MaxConcurrent: 10, LastChecked: pastLastChecked()},
		},
	}
	engine.RegisterSource(source)

	// Evaluate triggers pruning.
	engine.Evaluate(context.Background())

	// t-active should still have its seen keys.
	if seen := engine.SeenKeys("t-active"); len(seen) != 2 {
		t.Errorf("expected 2 seen keys for t-active, got %d", len(seen))
	}

	// t-stale should be pruned.
	if seen := engine.SeenKeys("t-stale"); seen != nil {
		t.Errorf("expected nil seen keys for t-stale, got %d entries", len(seen))
	}
}

// TestTriggerEngine_SeenMapSize tests the observability method.
func TestTriggerEngine_SeenMapSize(t *testing.T) {
	engine := NewTriggerEngine(TriggerEngineConfig{
		Logger: slog.Default(),
	})

	engine.SeedSeen("t1", "A")
	engine.SeedSeen("t1", "B")
	engine.SeedSeen("t2", "C")

	triggers, keys := engine.SeenMapSize()
	if triggers != 2 {
		t.Errorf("expected 2 triggers, got %d", triggers)
	}
	if keys != 3 {
		t.Errorf("expected 3 keys, got %d", keys)
	}
}

// failingSource is a trigger source that returns errors.
type failingSource struct{}

func (s *failingSource) Name() string { return "failing" }
func (s *failingSource) LoadTriggers(_ context.Context, _ TriggerBeadClient) ([]TriggerDef, error) {
	return nil, fmt.Errorf("load failed")
}
func (s *failingSource) Evaluate(_ context.Context, _ TriggerDef) ([]TriggerMatch, error) {
	return nil, fmt.Errorf("eval failed")
}
func (s *failingSource) CatchUpKeys(_ context.Context, _ TriggerBeadClient, _ string) ([]string, error) {
	return nil, fmt.Errorf("catch-up failed")
}

// TestTriggerEngine_SourceError tests that source errors don't crash the engine.
func TestTriggerEngine_SourceError(t *testing.T) {
	daemon := &mockTriggerDaemon{}

	engine := NewTriggerEngine(TriggerEngineConfig{
		Daemon: daemon,
		Logger: slog.Default(),
	})

	engine.RegisterSource(&failingSource{})

	// Should not panic.
	engine.Evaluate(context.Background())
	engine.CatchUp(context.Background())
}

// TestTriggerEngine_CatchUpTrigger tests single-trigger catch-up.
func TestTriggerEngine_CatchUpTrigger(t *testing.T) {
	daemon := &mockTriggerDaemon{}

	engine := NewTriggerEngine(TriggerEngineConfig{
		Daemon: daemon,
		Logger: slog.Default(),
	})

	source := &mockTriggerSource{
		name: "test",
		keys: map[string][]string{
			"t-specific": {"K1", "K2", "K3"},
		},
	}
	engine.RegisterSource(source)

	engine.CatchUpTrigger(context.Background(), "t-specific")

	seen := engine.SeenKeys("t-specific")
	if len(seen) != 3 {
		t.Errorf("expected 3 seen keys, got %d", len(seen))
	}
	for _, k := range []string{"K1", "K2", "K3"} {
		if !seen[k] {
			t.Errorf("missing key %s in seen set", k)
		}
	}
}

// TestTriggerEngine_GlobalPauseGate verifies that an open triggers-pause gate
// bead halts all trigger evaluation regardless of per-trigger or per-project
// enabled state.
func TestTriggerEngine_GlobalPauseGate(t *testing.T) {
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			{ID: "gate-1", Type: "gate", Status: "open", Title: triggersPauseTitle},
		},
	}

	engine := NewTriggerEngine(TriggerEngineConfig{
		Daemon: daemon,
		Logger: slog.Default(),
	})

	var fired int
	source := &mockTriggerSource{
		name: "test",
		triggers: []TriggerDef{
			{ID: "t1", Title: "T", Source: "test", Action: TriggerActionNotify, MaxConcurrent: 10, LastChecked: pastLastChecked()},
		},
		matches: map[string][]TriggerMatch{
			"t1": {{Key: "K1", Title: "match"}},
		},
	}
	engine.RegisterSource(source)
	engine.notifier = func(_ context.Context, _, _ string, _ string) { fired++ }

	engine.Evaluate(context.Background())

	if fired != 0 {
		t.Errorf("expected 0 notifications when globally paused, got %d", fired)
	}
}

// TestTriggerEngine_SchedulerPauseGatesScheduleSources verifies that an open
// scheduler-pause gate bead (created by `/schedule off`) halts evaluation of
// schedule-flavored trigger sources (trigger:schedule and schedule-mirror)
// while leaving other sources (e.g. jira/gitlab/webhook) free to fire. This
// is the "stop ticking on crons when schedules are dis[abled] with /schedule
// off" contract from gd-IGZ8Mr8c6r.
func TestTriggerEngine_SchedulerPauseGatesScheduleSources(t *testing.T) {
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			{ID: "gate-1", Type: "gate", Status: "open", Title: schedulerPauseTitleForTriggers},
		},
	}

	engine := NewTriggerEngine(TriggerEngineConfig{
		Daemon: daemon,
		Logger: slog.Default(),
	})

	scheduleSrc := &mockTriggerSource{
		name: "schedule",
		triggers: []TriggerDef{
			{ID: "sched-1", Title: "Schedule Trigger", Source: "schedule", Action: TriggerActionNotify, MaxConcurrent: 10, LastChecked: pastLastChecked()},
		},
		matches: map[string][]TriggerMatch{"sched-1": {{Key: "S1", Title: "schedule match"}}},
	}
	mirrorSrc := &mockTriggerSource{
		name: "schedule-mirror",
		triggers: []TriggerDef{
			{ID: "mirror-1", Title: "Mirror Trigger", Source: "schedule-mirror", Action: TriggerActionNotify, MaxConcurrent: 10, LastChecked: pastLastChecked()},
		},
		matches: map[string][]TriggerMatch{"mirror-1": {{Key: "M1", Title: "mirror match"}}},
	}
	otherSrc := &mockTriggerSource{
		name: "jira",
		triggers: []TriggerDef{
			{ID: "jira-1", Title: "Jira Trigger", Source: "jira", Action: TriggerActionNotify, MaxConcurrent: 10, LastChecked: pastLastChecked()},
		},
		matches: map[string][]TriggerMatch{"jira-1": {{Key: "J1", Title: "jira match"}}},
	}
	engine.RegisterSource(scheduleSrc)
	engine.RegisterSource(mirrorSrc)
	engine.RegisterSource(otherSrc)

	var fired []string
	engine.notifier = func(_ context.Context, title, _ string, _ string) { fired = append(fired, title) }

	engine.Evaluate(context.Background())

	if len(fired) != 1 || fired[0] != "Jira Trigger" {
		t.Errorf("expected only Jira trigger to fire while scheduler-pause gate is open, got %v", fired)
	}
}

// TestTriggerEngine_ProjectSchedulesDisabledGatesScheduleSources verifies that
// a project with schedules_enabled=false skips schedule-flavored triggers for
// that project, while other projects' schedule triggers still fire. Mirrors
// the per-project semantics already in place for triggers_enabled but applied
// through the `/schedule off <project>` controls.
func TestTriggerEngine_ProjectSchedulesDisabledGatesScheduleSources(t *testing.T) {
	daemon := &mockTriggerDaemon{
		projects: map[string]beadsapi.ProjectInfo{
			"locked":  {Name: "locked", TriggersEnabled: true, SchedulesEnabled: false},
			"running": {Name: "running", TriggersEnabled: true, SchedulesEnabled: true},
		},
	}

	engine := NewTriggerEngine(TriggerEngineConfig{
		Daemon: daemon,
		Logger: slog.Default(),
	})

	scheduleSrc := &mockTriggerSource{
		name: "schedule",
		triggers: []TriggerDef{
			{ID: "sched-locked", Title: "Locked Schedule", Source: "schedule", Action: TriggerActionNotify, MaxConcurrent: 10, Project: "locked", LastChecked: pastLastChecked()},
			{ID: "sched-running", Title: "Running Schedule", Source: "schedule", Action: TriggerActionNotify, MaxConcurrent: 10, Project: "running", LastChecked: pastLastChecked()},
		},
		matches: map[string][]TriggerMatch{
			"sched-locked":  {{Key: "L1", Title: "locked match"}},
			"sched-running": {{Key: "R1", Title: "running match"}},
		},
	}
	engine.RegisterSource(scheduleSrc)

	var fired []string
	engine.notifier = func(_ context.Context, title, _ string, _ string) { fired = append(fired, title) }

	engine.Evaluate(context.Background())

	if len(fired) != 1 || fired[0] != "Running Schedule" {
		t.Errorf("expected only Running Schedule to fire while schedules disabled for `locked`, got %v", fired)
	}
}

// TestTriggerEngine_SchedulerPauseDoesNotGateNonScheduleSources verifies that
// the scheduler-pause gate is a no-op for non-schedule sources (jira, gitlab,
// webhook, etc.). `/schedule off` is scoped to the schedule subsystem; other
// trigger sources are managed by `/triggers off`.
func TestTriggerEngine_SchedulerPauseDoesNotGateNonScheduleSources(t *testing.T) {
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			{ID: "gate-1", Type: "gate", Status: "open", Title: schedulerPauseTitleForTriggers},
		},
	}

	engine := NewTriggerEngine(TriggerEngineConfig{
		Daemon: daemon,
		Logger: slog.Default(),
	})

	source := &mockTriggerSource{
		name: "webhook",
		triggers: []TriggerDef{
			{ID: "wh-1", Title: "Webhook Trigger", Source: "webhook", Action: TriggerActionNotify, MaxConcurrent: 10, LastChecked: pastLastChecked()},
		},
		matches: map[string][]TriggerMatch{"wh-1": {{Key: "W1", Title: "webhook match"}}},
	}
	engine.RegisterSource(source)

	var fired int
	engine.notifier = func(_ context.Context, _, _ string, _ string) { fired++ }

	engine.Evaluate(context.Background())

	if fired != 1 {
		t.Errorf("expected webhook trigger to fire despite scheduler-pause gate, got %d notifications", fired)
	}
}

// TestTriggerEngine_NotifyRoutesSlackChannel verifies actionNotify threads the
// trigger's SlackChannel through to the notifier's channel argument, so a
// notify trigger carrying `slack_channel` posts to that channel instead of the
// default (ka-0000008204). A trigger with no SlackChannel passes "" (default).
func TestTriggerEngine_NotifyRoutesSlackChannel(t *testing.T) {
	engine := NewTriggerEngine(TriggerEngineConfig{
		Daemon: &mockTriggerDaemon{},
		Logger: slog.Default(),
	})

	source := &mockTriggerSource{
		name: "test",
		triggers: []TriggerDef{
			{ID: "routed", Title: "Routed", Source: "test", Action: TriggerActionNotify, MaxConcurrent: 10, SlackChannel: "C073CG65QUD", LastChecked: pastLastChecked()},
			{ID: "default", Title: "Default", Source: "test", Action: TriggerActionNotify, MaxConcurrent: 10, LastChecked: pastLastChecked()},
		},
		matches: map[string][]TriggerMatch{
			"routed":  {{Key: "R1", Title: "routed match"}},
			"default": {{Key: "D1", Title: "default match"}},
		},
	}
	engine.RegisterSource(source)

	got := map[string]string{} // trigger title -> channel arg
	var mu sync.Mutex
	engine.notifier = func(_ context.Context, title, _ string, channel string) {
		mu.Lock()
		defer mu.Unlock()
		got[title] = channel
	}

	engine.Evaluate(context.Background())

	if got["Routed"] != "C073CG65QUD" {
		t.Errorf("Routed channel = %q, want C073CG65QUD", got["Routed"])
	}
	if got["Default"] != "" {
		t.Errorf("Default channel = %q, want empty (default channel)", got["Default"])
	}
}

// TestTriggerEngine_ProjectDisabled verifies that triggers for a project with
// triggers_enabled=false are skipped, while other projects' triggers still fire.
func TestTriggerEngine_ProjectDisabled(t *testing.T) {
	daemon := &mockTriggerDaemon{
		projects: map[string]beadsapi.ProjectInfo{
			"locked":  {Name: "locked", TriggersEnabled: false},
			"running": {Name: "running", TriggersEnabled: true},
		},
	}

	engine := NewTriggerEngine(TriggerEngineConfig{
		Daemon: daemon,
		Logger: slog.Default(),
	})

	source := &mockTriggerSource{
		name: "test",
		triggers: []TriggerDef{
			{ID: "t1", Title: "Locked trigger", Source: "test", Action: TriggerActionNotify, MaxConcurrent: 10, Project: "locked", LastChecked: pastLastChecked()},
			{ID: "t2", Title: "Running trigger", Source: "test", Action: TriggerActionNotify, MaxConcurrent: 10, Project: "running", LastChecked: pastLastChecked()},
		},
		matches: map[string][]TriggerMatch{
			"t1": {{Key: "L1", Title: "locked match"}},
			"t2": {{Key: "R1", Title: "running match"}},
		},
	}
	engine.RegisterSource(source)

	var fired []string
	engine.notifier = func(_ context.Context, title, _ string, _ string) { fired = append(fired, title) }

	engine.Evaluate(context.Background())

	if len(fired) != 1 || fired[0] != "Running trigger" {
		t.Errorf("expected only running trigger to fire, got %v", fired)
	}
}

// TestTriggerEngine_BlockedByOpenDecision verifies that a trigger whose
// blocked_by list references an open decision bead is skipped, and that
// closing the referenced bead lets the next evaluation proceed.
//
// Acceptance criteria from ka-0000006545:
//   - Trigger with blocked_by=<open-decision> is skipped at evaluation time.
//   - Trigger with blocked_by=<closed-decision> proceeds.
//   - Multiple blocked_by entries: blocked if ANY are open.
//   - Closing the decision causes the next evaluation to proceed.
//   - last_checked is NOT updated while blocked (no controller-restart cooldown).
func TestTriggerEngine_BlockedByOpenDecision(t *testing.T) {
	openDecision := &beadsapi.BeadDetail{
		ID: "gd-decOPEN", Type: "decision", Status: "open", Title: "open decision",
	}
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{openDecision},
	}

	engine := NewTriggerEngine(TriggerEngineConfig{
		Daemon: daemon,
		Logger: slog.Default(),
	})

	source := &mockTriggerSource{
		name: "test",
		triggers: []TriggerDef{{
			ID:            "t1",
			Title:         "Blocked Trigger",
			Source:        "test",
			Action:        TriggerActionNotify,
			MaxConcurrent: 10,
			LastChecked:   pastLastChecked(),
			BlockedBy:     []string{"gd-decOPEN"},
		}},
		matches: map[string][]TriggerMatch{
			"t1": {{Key: "K1", Title: "should-not-fire"}},
		},
	}
	engine.RegisterSource(source)

	var fired int
	engine.notifier = func(_ context.Context, _, _ string, _ string) { fired++ }

	engine.Evaluate(context.Background())

	if fired != 0 {
		t.Errorf("expected 0 notifications while blocked by open decision, got %d", fired)
	}

	// last_checked MUST NOT be updated — the trigger should resume on the
	// next cycle as soon as the decision closes, without waiting for the
	// engine's cooldown to expire on a restart.
	daemon.mu.Lock()
	if len(daemon.updatedIDs) != 0 {
		t.Errorf("expected no UpdateBeadFields calls while blocked, got %v", daemon.updatedIDs)
	}
	daemon.mu.Unlock()

	// Now close the decision and re-evaluate — the trigger should fire.
	daemon.mu.Lock()
	openDecision.Status = "closed"
	daemon.mu.Unlock()

	engine.Evaluate(context.Background())

	if fired != 1 {
		t.Errorf("expected 1 notification after decision closed, got %d", fired)
	}
}

// TestTriggerEngine_BlockedByClosedDecision verifies that a trigger with a
// blocked_by entry pointing at a CLOSED decision is not blocked.
func TestTriggerEngine_BlockedByClosedDecision(t *testing.T) {
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			{ID: "gd-decCLOSED", Type: "decision", Status: "closed", Title: "resolved"},
		},
	}

	engine := NewTriggerEngine(TriggerEngineConfig{
		Daemon: daemon,
		Logger: slog.Default(),
	})

	source := &mockTriggerSource{
		name: "test",
		triggers: []TriggerDef{{
			ID:            "t1",
			Title:         "Cleared Trigger",
			Source:        "test",
			Action:        TriggerActionNotify,
			MaxConcurrent: 10,
			LastChecked:   pastLastChecked(),
			BlockedBy:     []string{"gd-decCLOSED"},
		}},
		matches: map[string][]TriggerMatch{
			"t1": {{Key: "K1", Title: "should-fire"}},
		},
	}
	engine.RegisterSource(source)

	var fired int
	engine.notifier = func(_ context.Context, _, _ string, _ string) { fired++ }

	engine.Evaluate(context.Background())

	if fired != 1 {
		t.Errorf("expected 1 notification with closed blocked_by, got %d", fired)
	}
}

// TestTriggerEngine_BlockedByAnyOpen verifies that the blocked_by gate is
// permissive at the entry level: if ANY referenced bead is still open, the
// trigger is blocked, even when others are closed.
func TestTriggerEngine_BlockedByAnyOpen(t *testing.T) {
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			{ID: "gd-decA", Type: "decision", Status: "closed", Title: "A"},
			{ID: "gd-decB", Type: "decision", Status: "open", Title: "B"},
			{ID: "gd-decC", Type: "decision", Status: "closed", Title: "C"},
		},
	}

	engine := NewTriggerEngine(TriggerEngineConfig{
		Daemon: daemon,
		Logger: slog.Default(),
	})

	source := &mockTriggerSource{
		name: "test",
		triggers: []TriggerDef{{
			ID:            "t1",
			Title:         "Multi-blocked",
			Source:        "test",
			Action:        TriggerActionNotify,
			MaxConcurrent: 10,
			LastChecked:   pastLastChecked(),
			BlockedBy:     []string{"gd-decA", "gd-decB", "gd-decC"},
		}},
		matches: map[string][]TriggerMatch{
			"t1": {{Key: "K1", Title: "should-not-fire"}},
		},
	}
	engine.RegisterSource(source)

	var fired int
	engine.notifier = func(_ context.Context, _, _ string, _ string) { fired++ }

	engine.Evaluate(context.Background())

	if fired != 0 {
		t.Errorf("expected 0 notifications while any blocked_by is open, got %d", fired)
	}
}

// TestParseTriggerDef_BlockedBy verifies the schema parses blocked_by from
// either a JSON array or a CSV string.
func TestParseTriggerDef_BlockedBy(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want []string
	}{
		{"empty", "", nil},
		{"single", "gd-decA", []string{"gd-decA"}},
		{"csv", "gd-decA,gd-decB,gd-decC", []string{"gd-decA", "gd-decB", "gd-decC"}},
		{"csv-with-spaces", "gd-decA, gd-decB , gd-decC", []string{"gd-decA", "gd-decB", "gd-decC"}},
		{"json-array", `["gd-decA","gd-decB"]`, []string{"gd-decA", "gd-decB"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bead := &beadsapi.BeadDetail{
				ID:     "t-x",
				Type:   "trigger",
				Status: "open",
				Title:  "T",
				Fields: map[string]string{
					"action":     "notify",
					"enabled":    "true",
					"blocked_by": tc.raw,
				},
			}
			def := ParseTriggerDef(bead, "test")
			if len(def.BlockedBy) != len(tc.want) {
				t.Fatalf("BlockedBy len=%d want %d (got %v)", len(def.BlockedBy), len(tc.want), def.BlockedBy)
			}
			for i, want := range tc.want {
				if def.BlockedBy[i] != want {
					t.Errorf("BlockedBy[%d]=%q want %q", i, def.BlockedBy[i], want)
				}
			}
		})
	}
}

// TestTriggerEngine_BlockedByInProgressDecision verifies that a referenced
// bead in status=in_progress also blocks the trigger — not just status=open.
// isOpenStatus is intentionally permissive about both spellings so an
// operator can grab a decision (which moves it to in_progress) without the
// trigger silently resuming behind their back.
func TestTriggerEngine_BlockedByInProgressDecision(t *testing.T) {
	cases := []struct {
		name   string
		status string
	}{
		{"in_progress", "in_progress"},
		{"in-progress", "in-progress"},
		{"inprogress", "inprogress"},
		{"uppercase open", "OPEN"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			daemon := &mockTriggerDaemon{
				beads: []*beadsapi.BeadDetail{
					{ID: "gd-dec1", Type: "decision", Status: tc.status, Title: "claimed"},
				},
			}
			engine := NewTriggerEngine(TriggerEngineConfig{
				Daemon: daemon,
				Logger: slog.Default(),
			})
			source := &mockTriggerSource{
				name: "test",
				triggers: []TriggerDef{{
					ID:            "t1",
					Title:         "Blocked",
					Source:        "test",
					Action:        TriggerActionNotify,
					MaxConcurrent: 10,
					LastChecked:   pastLastChecked(),
					BlockedBy:     []string{"gd-dec1"},
				}},
				matches: map[string][]TriggerMatch{"t1": {{Key: "K1", Title: "x"}}},
			}
			engine.RegisterSource(source)
			var fired int
			engine.notifier = func(_ context.Context, _, _ string, _ string) { fired++ }

			engine.Evaluate(context.Background())

			if fired != 0 {
				t.Errorf("status=%q: expected 0 notifications (blocked), got %d", tc.status, fired)
			}
		})
	}
}

// TestTriggerEngine_BlockedByEscalation verifies that the gate is type-
// agnostic: an open bead of type=escalation blocks just like a decision.
// The implementation only inspects status, never type.
func TestTriggerEngine_BlockedByEscalation(t *testing.T) {
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			{ID: "gd-esc1", Type: "escalation", Status: "open", Title: "open escalation"},
		},
	}
	engine := NewTriggerEngine(TriggerEngineConfig{
		Daemon: daemon,
		Logger: slog.Default(),
	})
	source := &mockTriggerSource{
		name: "test",
		triggers: []TriggerDef{{
			ID:            "t1",
			Title:         "Blocked by escalation",
			Source:        "test",
			Action:        TriggerActionNotify,
			MaxConcurrent: 10,
			LastChecked:   pastLastChecked(),
			BlockedBy:     []string{"gd-esc1"},
		}},
		matches: map[string][]TriggerMatch{"t1": {{Key: "K1", Title: "x"}}},
	}
	engine.RegisterSource(source)
	var fired int
	engine.notifier = func(_ context.Context, _, _ string, _ string) { fired++ }

	engine.Evaluate(context.Background())

	if fired != 0 {
		t.Errorf("expected 0 notifications while open escalation blocks, got %d", fired)
	}
}

// TestTriggerEngine_BlockedByLookupErrorIsDowngraded verifies that when
// GetBead errors (e.g. the daemon hiccups or the referenced bead was
// deleted), the lookup is downgraded to "not pending" so a brief outage
// does not silently freeze a trigger.
//
// The mockTriggerDaemon's GetBead returns an error for unknown bead IDs,
// so a blocked_by entry pointing at a non-existent ID exercises this path.
func TestTriggerEngine_BlockedByLookupErrorIsDowngraded(t *testing.T) {
	daemon := &mockTriggerDaemon{
		// No beads registered — GetBead returns "not found" for any ID.
	}
	engine := NewTriggerEngine(TriggerEngineConfig{
		Daemon: daemon,
		Logger: slog.Default(),
	})
	source := &mockTriggerSource{
		name: "test",
		triggers: []TriggerDef{{
			ID:            "t1",
			Title:         "Phantom block",
			Source:        "test",
			Action:        TriggerActionNotify,
			MaxConcurrent: 10,
			LastChecked:   pastLastChecked(),
			BlockedBy:     []string{"gd-deleted"},
		}},
		matches: map[string][]TriggerMatch{"t1": {{Key: "K1", Title: "x"}}},
	}
	engine.RegisterSource(source)
	var fired int
	engine.notifier = func(_ context.Context, _, _ string, _ string) { fired++ }

	engine.Evaluate(context.Background())

	if fired != 1 {
		t.Errorf("expected 1 notification when blocked_by lookup errors (downgraded to not-pending), got %d", fired)
	}
}

// TestTriggerEngine_BlockedByEmptyEntriesSkipped verifies that whitespace
// and empty entries in BlockedBy are skipped without spurious lookups —
// this matters because parseStringList can yield ["gd-decA", ""] from a
// trailing comma, and the engine must not treat the empty string as a
// blocking reference.
func TestTriggerEngine_BlockedByEmptyEntriesSkipped(t *testing.T) {
	daemon := &mockTriggerDaemon{}
	engine := NewTriggerEngine(TriggerEngineConfig{
		Daemon: daemon,
		Logger: slog.Default(),
	})
	trig := TriggerDef{
		ID:        "t1",
		BlockedBy: []string{"", "   ", "\t"},
	}

	pending := engine.triggerBlockedBy(context.Background(), trig)
	if len(pending) != 0 {
		t.Errorf("expected no pending entries for whitespace-only BlockedBy, got %v", pending)
	}
}

// TestIsOpenStatus consolidates the open-status detection table so future
// status string additions stay in sync with what the gate actually treats
// as blocking.
func TestIsOpenStatus(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"open", true},
		{"OPEN", true},
		{"  open  ", true},
		{"in_progress", true},
		{"in-progress", true},
		{"inprogress", true},
		{"In_Progress", true},
		{"closed", false},
		{"completed", false},
		{"deleted", false},
		{"", false},
		{"unknown", false},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			if got := isOpenStatus(c.in); got != c.want {
				t.Errorf("isOpenStatus(%q) = %v, want %v", c.in, got, c.want)
			}
		})
	}
}

// TestCtxWithTriggerOperator_PrefersOperatedBy verifies that the typed
// server-resolved OperatedBy is preferred over the (potentially untyped)
// CreatedBy when stamping the X-Operated-By header for daemon writes. This
// is the path exercised by the e2e trigger-engine-smoke test, where the
// trigger bead is created with KD_ACTOR=e2e-rwx (untyped) but the server
// has populated operated_by from BEADS_OPERATED_BY=oidc:e2e-shard-0@example.com.
// See ka-0000006816.
func TestCtxWithTriggerOperator_PrefersOperatedBy(t *testing.T) {
	trig := TriggerDef{
		Source:     "schedule",
		CreatedBy:  "e2e-rwx", // untyped
		OperatedBy: "oidc:e2e-shard-0@example.com",
	}
	ctx := ctxWithTriggerOperator(context.Background(), trig)
	got := beadsapi.OperatorFromContext(ctx)
	want := "oidc:e2e-shard-0@example.com"
	if got != want {
		t.Errorf("OperatorFromContext = %q, want %q", got, want)
	}
}

// TestCtxWithTriggerOperator_FallbackToCreatedBy verifies CreatedBy is used
// when OperatedBy is unset but CreatedBy is typed.
func TestCtxWithTriggerOperator_FallbackToCreatedBy(t *testing.T) {
	trig := TriggerDef{
		Source:    "jira",
		CreatedBy: "jira:JIRA-123",
	}
	ctx := ctxWithTriggerOperator(context.Background(), trig)
	got := beadsapi.OperatorFromContext(ctx)
	if got != "jira:JIRA-123" {
		t.Errorf("OperatorFromContext = %q, want jira:JIRA-123", got)
	}
}

// TestCtxWithTriggerOperator_NoTypedOperatorPassthrough verifies that when
// neither OperatedBy nor CreatedBy is typed, ctx is returned unchanged so the
// caller's fleet-token identity (audit.go fleet branch — ka-0000005187 /
// decision ka-0000001054) flows through to audit attribution. The previous
// `system:<source>-trigger` sentinel was rejected by the kbeads operator
// sanitizer (ka-0000002601) and caused jira-bridge spawn_agent failures.
func TestCtxWithTriggerOperator_NoTypedOperatorPassthrough(t *testing.T) {
	trig := TriggerDef{
		Source:    "schedule",
		CreatedBy: "e2e-rwx", // untyped
	}
	ctx := ctxWithTriggerOperator(context.Background(), trig)
	got := beadsapi.OperatorFromContext(ctx)
	if got != "" {
		t.Errorf("OperatorFromContext = %q, want \"\" (passthrough to fleet auth)", got)
	}
}

// TestCtxWithTriggerOperator_DoesNotOverride verifies an existing operator on
// the context wins over any value derived from the trigger def.
func TestCtxWithTriggerOperator_DoesNotOverride(t *testing.T) {
	parent := beadsapi.WithOperator(context.Background(), "slack:U12345")
	trig := TriggerDef{
		OperatedBy: "oidc:other@example.com",
		CreatedBy:  "jira:JIRA-1",
	}
	ctx := ctxWithTriggerOperator(parent, trig)
	got := beadsapi.OperatorFromContext(ctx)
	if got != "slack:U12345" {
		t.Errorf("OperatorFromContext = %q, want slack:U12345 (preserved)", got)
	}
}

// TestCtxWithTriggerOperatorOrDefault_StampsDefaultForSentinelAuthor is the
// fsm-triage regression (ka-0000006023): a trigger whose only attribution is a
// SENTINEL (operated_by=system:legacy-pre-attribution) is a typed operator, so
// ctxWithTriggerOperator places it on ctx — yet it is not human, so without the
// default-operator fallback the spawned agent boots unattributed and every
// kbeads write fail-closes. With a configured default, the helper must replace
// the sentinel with the human.
func TestCtxWithTriggerOperatorOrDefault_StampsDefaultForSentinelAuthor(t *testing.T) {
	engine := NewTriggerEngine(TriggerEngineConfig{
		DefaultOperator: "oidc:matthew.baker@acme.example",
	})
	trig := TriggerDef{
		Source:     "jira",
		CreatedBy:  "thread-1777560916-494139", // an agent (untyped)
		OperatedBy: "system:legacy-pre-attribution",
	}
	ctx := engine.ctxWithTriggerOperatorOrDefault(context.Background(), trig)
	got := beadsapi.OperatorFromContext(ctx)
	if got != "oidc:matthew.baker@acme.example" {
		t.Errorf("OperatorFromContext = %q, want the default human operator", got)
	}
}

// TestCtxWithTriggerOperatorOrDefault_PreservesHumanAuthor verifies the default
// never overrides a real human author resolved from the trigger bead.
func TestCtxWithTriggerOperatorOrDefault_PreservesHumanAuthor(t *testing.T) {
	engine := NewTriggerEngine(TriggerEngineConfig{
		DefaultOperator: "oidc:matthew.baker@acme.example",
	})
	trig := TriggerDef{
		Source:     "jira",
		OperatedBy: "slack:U0OPERATOR1",
	}
	ctx := engine.ctxWithTriggerOperatorOrDefault(context.Background(), trig)
	got := beadsapi.OperatorFromContext(ctx)
	if got != "slack:U0OPERATOR1" {
		t.Errorf("OperatorFromContext = %q, want the trigger's human operator preserved", got)
	}
}

// TestCtxWithTriggerOperatorOrDefault_NoDefaultIsPassthrough verifies that with
// no default configured the helper is exactly ctxWithTriggerOperator — a
// sentinel author flows through unchanged (zero behavior change when the knob
// is unset), preserving the documented fleet-token auto-attribution path.
func TestCtxWithTriggerOperatorOrDefault_NoDefaultIsPassthrough(t *testing.T) {
	engine := NewTriggerEngine(TriggerEngineConfig{}) // no DefaultOperator, env unset in test
	trig := TriggerDef{
		Source:     "jira",
		OperatedBy: "system:legacy-pre-attribution",
	}
	ctx := engine.ctxWithTriggerOperatorOrDefault(context.Background(), trig)
	got := beadsapi.OperatorFromContext(ctx)
	if got != "system:legacy-pre-attribution" {
		t.Errorf("OperatorFromContext = %q, want sentinel passthrough (no default configured)", got)
	}
}

// TestNewTriggerEngine_DropsNonHumanDefaultOperator verifies a misconfigured
// non-human DefaultOperator is dropped at construction so the engine can never
// stamp an operator the daemon rejects (ka-0000002128).
func TestNewTriggerEngine_DropsNonHumanDefaultOperator(t *testing.T) {
	engine := NewTriggerEngine(TriggerEngineConfig{
		DefaultOperator: "fleet:jira-bridge", // non-human
	})
	if engine.defaultOperator != "" {
		t.Errorf("defaultOperator = %q, want \"\" (non-human dropped)", engine.defaultOperator)
	}
}

// TestNewTriggerEngine_DefaultOperatorFromEnv verifies the env fallback is read
// and validated when no explicit config override is given.
func TestNewTriggerEngine_DefaultOperatorFromEnv(t *testing.T) {
	t.Setenv("TRIGGER_ENGINE_DEFAULT_OPERATOR", "oidc:env-human@example.com")
	engine := NewTriggerEngine(TriggerEngineConfig{})
	if engine.defaultOperator != "oidc:env-human@example.com" {
		t.Errorf("defaultOperator = %q, want the env value", engine.defaultOperator)
	}

	t.Setenv("TRIGGER_ENGINE_DEFAULT_OPERATOR", "jira:5557") // non-human
	engine = NewTriggerEngine(TriggerEngineConfig{})
	if engine.defaultOperator != "" {
		t.Errorf("defaultOperator = %q, want \"\" (non-human env value dropped)", engine.defaultOperator)
	}
}

// TestParseTriggerDef_OperatedBy verifies the OperatedBy field is populated
// from bead.OperatedBy.
func TestParseTriggerDef_OperatedBy(t *testing.T) {
	bead := &beadsapi.BeadDetail{
		ID:         "gd-trig-op",
		Title:      "Operator-stamped trigger",
		CreatedBy:  "e2e-rwx",
		OperatedBy: "oidc:e2e-shard-0@example.com",
		Fields: map[string]string{
			"action":  "create_bead",
			"project": "gardenio",
		},
	}
	def := ParseTriggerDef(bead, "schedule")
	if def.OperatedBy != "oidc:e2e-shard-0@example.com" {
		t.Errorf("OperatedBy = %q, want oidc:e2e-shard-0@example.com", def.OperatedBy)
	}
	if def.CreatedBy != "e2e-rwx" {
		t.Errorf("CreatedBy = %q, want e2e-rwx", def.CreatedBy)
	}
}

// TestTriggerEngine_GCSeenKeys_TTLEviction verifies that inner dedup keys are
// aged out of an ACTIVE trigger's seen set once older than triggerSeenKeyTTL,
// so a long-lived bead-state trigger cannot grow its seen map unbounded
// (ka-0000003049). pruneStaleTriggers only reclaims whole defunct triggers; the
// inner set needs its own eviction.
func TestTriggerEngine_GCSeenKeys_TTLEviction(t *testing.T) {
	now := time.Now().UTC()
	clock := func() time.Time { return now }

	engine := NewTriggerEngine(TriggerEngineConfig{
		Logger: slog.Default(),
		Clock:  clock,
	})

	engine.SeedSeen("t1", "old-key")
	engine.SeedSeen("t1", "fresh-key")

	// First sweep: stamps first-observation for both keys at `now`, evicts
	// nothing (nothing is older than the TTL yet).
	engine.gcSeenKeys()
	if got := len(engine.SeenKeys("t1")); got != 2 {
		t.Fatalf("after first gc, seen keys = %d, want 2", got)
	}

	// Backdate only "old-key"'s first-observation past the TTL horizon.
	engine.mu.Lock()
	engine.seenAt["t1"]["old-key"] = now.Add(-triggerSeenKeyTTL - time.Minute)
	engine.mu.Unlock()

	// Advance the clock past the GC throttle so the next sweep runs.
	now = now.Add(triggerSeenGCInterval + time.Second)
	engine.gcSeenKeys()

	seen := engine.SeenKeys("t1")
	if seen["old-key"] {
		t.Errorf("old-key should have been TTL-evicted, still present")
	}
	if !seen["fresh-key"] {
		t.Errorf("fresh-key should survive (within TTL), was evicted")
	}
}

// TestTriggerEngine_GCSeenKeys_Throttled verifies the sweep respects
// triggerSeenGCInterval: a second call inside the interval is a no-op even if a
// key is past its TTL, so the O(total keys) walk cannot run every ~30s cycle.
func TestTriggerEngine_GCSeenKeys_Throttled(t *testing.T) {
	now := time.Now().UTC()
	clock := func() time.Time { return now }

	engine := NewTriggerEngine(TriggerEngineConfig{
		Logger: slog.Default(),
		Clock:  clock,
	})
	engine.SeedSeen("t1", "k")

	engine.gcSeenKeys() // stamps k at now, sets lastSeenGC=now
	// Backdate k well past the TTL.
	engine.mu.Lock()
	engine.seenAt["t1"]["k"] = now.Add(-triggerSeenKeyTTL - time.Hour)
	engine.mu.Unlock()

	// Second call WITHOUT advancing the clock: throttled, so k survives.
	engine.gcSeenKeys()
	if !engine.SeenKeys("t1")["k"] {
		t.Errorf("k evicted by a throttled sweep; expected the sweep to no-op")
	}

	// Advance past the interval: now the sweep runs and evicts k.
	now = now.Add(triggerSeenGCInterval + time.Second)
	engine.gcSeenKeys()
	if engine.SeenKeys("t1")["k"] != false {
		t.Errorf("k should be evicted after the throttle window elapsed")
	}
}

// TestTriggerEngine_GCSeenKeys_HardCap verifies the per-trigger safety cap:
// when a single trigger's inner set exceeds triggerSeenKeyMaxPerTrigger, the
// oldest-observed keys are evicted down to the cap even before the TTL, bounding
// worst-case memory between sweeps for a firehose trigger.
func TestTriggerEngine_GCSeenKeys_HardCap(t *testing.T) {
	now := time.Now().UTC()
	clock := func() time.Time { return now }

	engine := NewTriggerEngine(TriggerEngineConfig{
		Logger: slog.Default(),
		Clock:  clock,
	})

	over := triggerSeenKeyMaxPerTrigger + 100
	for i := 0; i < over; i++ {
		engine.SeedSeen("firehose", fmt.Sprintf("k-%d", i))
	}

	engine.gcSeenKeys()

	if got := len(engine.SeenKeys("firehose")); got != triggerSeenKeyMaxPerTrigger {
		t.Errorf("after hard-cap gc, seen keys = %d, want %d", got, triggerSeenKeyMaxPerTrigger)
	}
}

// TestTriggerEngine_GCSeenKeys_ReclaimsEmptyTrigger verifies that a trigger
// whose inner set fully drains is removed from both seen and seenAt, so a
// burst-then-idle trigger frees its map entirely.
func TestTriggerEngine_GCSeenKeys_ReclaimsEmptyTrigger(t *testing.T) {
	now := time.Now().UTC()
	clock := func() time.Time { return now }

	engine := NewTriggerEngine(TriggerEngineConfig{
		Logger: slog.Default(),
		Clock:  clock,
	})
	engine.SeedSeen("burst", "only-key")

	engine.gcSeenKeys() // stamp
	engine.mu.Lock()
	engine.seenAt["burst"]["only-key"] = now.Add(-triggerSeenKeyTTL - time.Minute)
	engine.mu.Unlock()

	now = now.Add(triggerSeenGCInterval + time.Second)
	engine.gcSeenKeys()

	engine.mu.Lock()
	_, seenExists := engine.seen["burst"]
	_, atExists := engine.seenAt["burst"]
	engine.mu.Unlock()
	if seenExists || atExists {
		t.Errorf("drained trigger not reclaimed: seen=%v seenAt=%v", seenExists, atExists)
	}
}

// TestTriggerEngine_PruneStaleTriggers_ClearsSeenAt verifies pruneStaleTriggers
// drops the seenAt shadow alongside seen for a defunct trigger, so the GC
// bookkeeping cannot outlive the trigger it tracks.
func TestTriggerEngine_PruneStaleTriggers_ClearsSeenAt(t *testing.T) {
	engine := NewTriggerEngine(TriggerEngineConfig{
		Logger: slog.Default(),
	})
	engine.SeedSeen("t-stale", "k1")
	engine.gcSeenKeys() // populate seenAt["t-stale"]

	engine.pruneStaleTriggers(map[string]bool{}) // nothing active

	engine.mu.Lock()
	_, atExists := engine.seenAt["t-stale"]
	engine.mu.Unlock()
	if atExists {
		t.Errorf("seenAt[t-stale] should be cleared by pruneStaleTriggers")
	}
}

// TestTriggerGate_ScheduleOffGatesGardenersWhateverSource pins gd-gCN4m4DYxc:
// `/schedule off <project>` (schedules_enabled=false) and the scheduler-pause
// gate silence a gardener trigger whatever source loads it, so an event-mode
// plan gardener (trigger:multi, PR #1907) stops like a trigger:schedule one.
// Non-gardener multi triggers and a land's steward keep their old gating: only
// triggers-pause / triggers_enabled.
func TestTriggerGate_ScheduleOffGatesGardenersWhateverSource(t *testing.T) {
	planGardener := TriggerDef{
		ID: "gd-plan", Source: "multi", Project: "locked",
		Labels: []string{"trigger:multi", "role:gardener", "plan-gardener"}, Gardener: &GardenerSpec{},
	}
	otherProjectGardener := planGardener
	otherProjectGardener.ID, otherProjectGardener.Project = "gd-plan-2", "running"
	multiNotGardener := TriggerDef{ID: "gd-multi", Source: "multi", Project: "locked", Labels: []string{"trigger:multi"}}
	stewardTrig := TriggerDef{
		ID: "gd-steward", Source: "multi", Project: "locked",
		Labels: []string{"trigger:multi", "role:gardener", stewardMarkerLabel}, Gardener: &GardenerSpec{},
	}
	jiraGardener := TriggerDef{ID: "gd-jira", Source: "jira", Project: "locked", Gardener: &GardenerSpec{}}
	scheduleTrig := TriggerDef{ID: "gd-sched", Source: "schedule", Project: "locked"}

	projectOff := triggerGateState{projectSchedulesDisabled: map[string]bool{"locked": true}}
	globalOff := triggerGateState{schedulerPaused: true}
	triggersOff := triggerGateState{projectDisabled: map[string]bool{"locked": true}}
	none := triggerGateState{}

	for _, tc := range []struct {
		name   string
		gate   triggerGateState
		source string
		trig   TriggerDef
		want   bool
	}{
		{"project off: event-mode plan gardener", projectOff, "multi", planGardener, true},
		{"project off: another project's gardener", projectOff, "multi", otherProjectGardener, false},
		{"project off: non-gardener multi trigger", projectOff, "multi", multiNotGardener, false},
		{"project off: steward", projectOff, "multi", stewardTrig, false},
		{"project off: gardener on another source", projectOff, "jira", jiraGardener, true},
		{"project off: schedule trigger", projectOff, "schedule", scheduleTrig, true},
		{"scheduler-pause: event-mode plan gardener", globalOff, "multi", planGardener, true},
		{"scheduler-pause: non-gardener multi trigger", globalOff, "multi", multiNotGardener, false},
		{"scheduler-pause: steward", globalOff, "multi", stewardTrig, false},
		{"triggers off: steward still gated", triggersOff, "multi", stewardTrig, true},
		{"triggers off: non-gardener multi trigger still gated", triggersOff, "multi", multiNotGardener, true},
		{"no gate: event-mode plan gardener", none, "multi", planGardener, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.gate.skipTrigger(scheduleSourceNames[tc.source], tc.trig); got != tc.want {
				t.Errorf("skipTrigger(%s, %s) = %v, want %v", tc.source, tc.trig.ID, got, tc.want)
			}
		})
	}
}
