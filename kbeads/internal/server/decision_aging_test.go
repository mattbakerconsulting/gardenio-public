package server

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"gardenio/kbeads/internal/model"
)

// seedDecision inserts a type=decision bead with the given fields and an
// UpdatedAt of `ageDays` days ago (also used as CreatedAt so a never-touched
// decision reads created==updated).
func seedDecision(ms *mockStore, id string, ageDays int, fields map[string]any) {
	ts := time.Now().UTC().Add(-time.Duration(ageDays) * 24 * time.Hour)
	b := &model.Bead{
		ID:        id,
		Kind:      model.KindIssue,
		Type:      model.TypeDecision,
		Title:     id,
		Status:    model.StatusOpen,
		CreatedAt: ts,
		UpdatedAt: ts,
	}
	if fields != nil {
		raw, _ := json.Marshal(fields)
		b.Fields = raw
	}
	ms.beads[id] = b
}

// optsWithDefaultFlag builds an options array where the option with id==dflt
// carries "default": true.
func optsWithDefaultFlag(dflt string, ids ...string) []map[string]any {
	out := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		o := map[string]any{"id": id, "label": id, "artifact_type": "report"}
		if id == dflt {
			o["default"] = true
		}
		out = append(out, o)
	}
	return out
}

// plainOpts builds an options array with no default flag on any option.
func plainOpts(ids ...string) []map[string]any {
	return optsWithDefaultFlag("", ids...)
}

func TestDefaultOptionFor(t *testing.T) {
	tests := []struct {
		name   string
		fields map[string]any
		wantID string
		wantOK bool
	}{
		{name: "nil fields", fields: nil, wantOK: false},
		{
			name:   "options without any default",
			fields: map[string]any{"options": plainOpts("a", "b", "c")},
			wantOK: false,
		},
		{
			name:   "explicit default_option matching an option",
			fields: map[string]any{"options": plainOpts("a", "b"), "default_option": "b"},
			wantID: "b", wantOK: true,
		},
		{
			name:   "explicit default_option not matching any option is ignored",
			fields: map[string]any{"options": plainOpts("a", "b"), "default_option": "zzz"},
			wantOK: false,
		},
		{
			name:   "option flagged default:true",
			fields: map[string]any{"options": optsWithDefaultFlag("b", "a", "b", "c")},
			wantID: "b", wantOK: true,
		},
		{
			name: "explicit default_option wins over default:true flag",
			fields: map[string]any{
				"options":        optsWithDefaultFlag("c", "a", "b", "c"),
				"default_option": "a",
			},
			wantID: "a", wantOK: true,
		},
		{
			name:   "plain string-array options yield no default",
			fields: map[string]any{"options": []string{"a", "b"}},
			wantOK: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := &model.Bead{ID: "gd-x", Type: model.TypeDecision}
			if tt.fields != nil {
				raw, _ := json.Marshal(tt.fields)
				b.Fields = raw
			}
			gotID, gotOK := defaultOptionFor(b)
			if gotOK != tt.wantOK || gotID != tt.wantID {
				t.Fatalf("defaultOptionFor = (%q, %v), want (%q, %v)", gotID, gotOK, tt.wantID, tt.wantOK)
			}
		})
	}
}

func TestParseAgingOptions_DoubleEncoded(t *testing.T) {
	// Options stored as a JSON STRING containing the array (the shape
	// extractDecisionFields passes through verbatim).
	inner, _ := json.Marshal(optsWithDefaultFlag("b", "a", "b"))
	encoded, _ := json.Marshal(string(inner)) // a JSON string
	opts := parseAgingOptions(encoded)
	if len(opts) != 2 {
		t.Fatalf("expected 2 options from double-encoded, got %d", len(opts))
	}
	if !opts[1].Default || opts[1].ID != "b" {
		t.Fatalf("expected option b to carry default:true, got %+v", opts)
	}
}

func TestAgingOverrideDays(t *testing.T) {
	tests := []struct {
		name   string
		fields map[string]any
		want   int
	}{
		{name: "absent", fields: map[string]any{"options": plainOpts("a")}, want: 0},
		{name: "positive", fields: map[string]any{"auto_default_after_days": 30}, want: 30},
		{name: "zero ignored", fields: map[string]any{"auto_default_after_days": 0}, want: 0},
		{name: "negative ignored", fields: map[string]any{"auto_default_after_days": -5}, want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := &model.Bead{ID: "gd-x"}
			raw, _ := json.Marshal(tt.fields)
			b.Fields = raw
			if got := agingOverrideDays(b); got != tt.want {
				t.Fatalf("agingOverrideDays = %d, want %d", got, tt.want)
			}
		})
	}
}

// TestSweepStaleDecisions_AutoDefaultsOnlyOptedInStale is the core acceptance
// test: a stale decision that declared a default is auto-defaulted; a stale one
// without a default is left untouched (no ping); a fresh opted-in one is not
// yet touched.
func TestSweepStaleDecisions_AutoDefaultsOnlyOptedInStale(t *testing.T) {
	t.Setenv("BEADS_DECISION_AGING_ENABLED", "1")
	s, ms := newAutoCloseServer()
	ctx := context.Background()

	// Stale (20d) + declared default → should auto-default.
	seedDecision(ms, "gd-stale-default", 20, map[string]any{
		"prompt":         "proceed?",
		"options":        plainOpts("yes", "no"),
		"default_option": "no",
	})
	// Stale (20d), no declared default → must be left alone (no ping, no close).
	seedDecision(ms, "gd-stale-nodefault", 20, map[string]any{
		"prompt":  "which approach?",
		"options": plainOpts("a", "b"),
	})
	// Fresh (2d) + declared default → below threshold, untouched.
	seedDecision(ms, "gd-fresh-default", 2, map[string]any{
		"prompt":  "proceed?",
		"options": optsWithDefaultFlag("skip", "go", "skip"),
	})

	s.sweepStaleDecisions(ctx)

	// Opted-in stale → closed, chosen recorded, auto_defaulted marker set.
	got := ms.beads["gd-stale-default"]
	if got.Status != model.StatusClosed {
		t.Fatalf("gd-stale-default status = %s, want closed", got.Status)
	}
	if got.ClosedBy != decisionAgingActor {
		t.Fatalf("gd-stale-default closedBy = %q, want %q", got.ClosedBy, decisionAgingActor)
	}
	var f map[string]any
	_ = json.Unmarshal(got.Fields, &f)
	if f["chosen"] != "no" {
		t.Fatalf("gd-stale-default chosen = %v, want \"no\"", f["chosen"])
	}
	if f["auto_defaulted"] != true {
		t.Fatalf("gd-stale-default auto_defaulted = %v, want true", f["auto_defaulted"])
	}
	if f["responded_by"] != decisionAgingActor {
		t.Fatalf("gd-stale-default responded_by = %v, want %q", f["responded_by"], decisionAgingActor)
	}

	// Un-opted-in stale → untouched.
	if ms.beads["gd-stale-nodefault"].Status != model.StatusOpen {
		t.Fatalf("gd-stale-nodefault should be left open, got %s", ms.beads["gd-stale-nodefault"].Status)
	}
	// Fresh opted-in → untouched (below the UpdatedBefore cutoff).
	if ms.beads["gd-fresh-default"].Status != model.StatusOpen {
		t.Fatalf("gd-fresh-default should be left open, got %s", ms.beads["gd-fresh-default"].Status)
	}
}

// TestSweepStaleDecisions_SkipsWhenAnotherReplicaHoldsTheLock pins the
// cross-replica serialization from the Phase 2 2-replica spike
// (gd-XxrObXvc8r): when decisionAgingAdvisoryLockKey is already held (by a
// concurrent tick, standing in for another replica's overlapping sweep),
// this replica's sweep must skip its body entirely rather than double-run
// the auto-default.
func TestSweepStaleDecisions_SkipsWhenAnotherReplicaHoldsTheLock(t *testing.T) {
	t.Setenv("BEADS_DECISION_AGING_ENABLED", "1")
	s, ms := newAutoCloseServer()
	ctx := context.Background()

	seedDecision(ms, "gd-stale-default", 20, map[string]any{
		"prompt":         "proceed?",
		"options":        plainOpts("yes", "no"),
		"default_option": "no",
	})

	ms.advisoryLocksMu.Lock()
	if ms.advisoryLocks == nil {
		ms.advisoryLocks = map[int64]bool{}
	}
	ms.advisoryLocks[decisionAgingAdvisoryLockKey] = true
	ms.advisoryLocksMu.Unlock()

	s.sweepStaleDecisions(ctx)

	if got := ms.beads["gd-stale-default"].Status; got != model.StatusOpen {
		t.Fatalf("sweep ran despite the advisory lock being held: status = %s, want open", got)
	}
}

// TestSweepStaleDecisions_DisabledIsNoop verifies the policy ships dark: with
// the flag unset, even a stale opted-in decision is untouched.
func TestSweepStaleDecisions_DisabledIsNoop(t *testing.T) {
	// Ensure the env is empty for this test.
	t.Setenv("BEADS_DECISION_AGING_ENABLED", "")
	s, ms := newAutoCloseServer()

	seedDecision(ms, "gd-stale-default", 30, map[string]any{
		"options":        plainOpts("yes", "no"),
		"default_option": "no",
	})

	s.sweepStaleDecisions(context.Background())

	if ms.beads["gd-stale-default"].Status != model.StatusOpen {
		t.Fatalf("disabled policy must not close anything; got %s", ms.beads["gd-stale-default"].Status)
	}
}

// TestSweepStaleDecisions_PerDecisionExtension verifies a decision can ask to
// wait LONGER than the global floor via auto_default_after_days.
func TestSweepStaleDecisions_PerDecisionExtension(t *testing.T) {
	t.Setenv("BEADS_DECISION_AGING_ENABLED", "1")
	s, ms := newAutoCloseServer()
	ctx := context.Background()

	// 16d old, but asks to wait 30d → still too fresh, must be untouched.
	seedDecision(ms, "gd-extend-waiting", 16, map[string]any{
		"options":                 plainOpts("yes", "no"),
		"default_option":          "no",
		"auto_default_after_days": 30,
	})
	// 40d old, asks to wait 30d → past its own window, must auto-default.
	seedDecision(ms, "gd-extend-ripe", 40, map[string]any{
		"options":                 plainOpts("yes", "no"),
		"default_option":          "no",
		"auto_default_after_days": 30,
	})

	s.sweepStaleDecisions(ctx)

	if ms.beads["gd-extend-waiting"].Status != model.StatusOpen {
		t.Fatalf("gd-extend-waiting within its extended window should be open, got %s",
			ms.beads["gd-extend-waiting"].Status)
	}
	if ms.beads["gd-extend-ripe"].Status != model.StatusClosed {
		t.Fatalf("gd-extend-ripe past its extended window should be closed, got %s",
			ms.beads["gd-extend-ripe"].Status)
	}
}

// TestSweepStaleDecisions_RunsCloseCascade verifies an auto-defaulted decision
// parks the work it was gating (afterBeadClosed → maybeParkDecisionGatedBeads),
// so the gated bead does not thrash back into the ready lane (ka-0000005620).
func TestSweepStaleDecisions_RunsCloseCascade(t *testing.T) {
	t.Setenv("BEADS_DECISION_AGING_ENABLED", "1")
	s, ms := newAutoCloseServer()
	ctx := context.Background()

	seedDecision(ms, "gd-gate", 20, map[string]any{
		"options":        plainOpts("yes", "no"),
		"default_option": "no",
	})
	// A gated bead blocks→ the decision (the edge --requesting-task writes).
	seedBead(ms, "gd-gated-work", model.TypeTask, model.StatusOpen, nil, nil)
	linkBlocks(ms, "gd-gated-work", "gd-gate")

	s.sweepStaleDecisions(ctx)

	if ms.beads["gd-gate"].Status != model.StatusClosed {
		t.Fatalf("decision should be closed, got %s", ms.beads["gd-gate"].Status)
	}
	if ms.beads["gd-gated-work"].Status != model.StatusBlocked {
		t.Fatalf("gated work should be parked blocked, got %s", ms.beads["gd-gated-work"].Status)
	}
}

func TestDecisionAgingDays_Default(t *testing.T) {
	t.Setenv("BEADS_DECISION_AGING_DAYS", "")
	if got := decisionAgingDays(); got != decisionAgingDefaultDays {
		t.Fatalf("default days = %d, want %d", got, decisionAgingDefaultDays)
	}
	t.Setenv("BEADS_DECISION_AGING_DAYS", "7")
	if got := decisionAgingDays(); got != 7 {
		t.Fatalf("configured days = %d, want 7", got)
	}
	t.Setenv("BEADS_DECISION_AGING_DAYS", "0")
	if got := decisionAgingDays(); got != decisionAgingDefaultDays {
		t.Fatalf("0 (invalid) should floor to default %d, got %d", decisionAgingDefaultDays, got)
	}
	t.Setenv("BEADS_DECISION_AGING_DAYS", "notanumber")
	if got := decisionAgingDays(); got != decisionAgingDefaultDays {
		t.Fatalf("invalid should floor to default %d, got %d", decisionAgingDefaultDays, got)
	}
}
