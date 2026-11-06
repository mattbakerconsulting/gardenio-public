package main

import (
	"context"
	"strings"
	"testing"
	"time"
)

// plans_enrich_timing_test.go — Phase 1 per-section timing (plan ka-0000002224,
// task ka-0000005453). It pins the instrumentation contract the /plans render
// leans on: the enrich pass records EACH bulk join's own wall clock (not just the
// wave aggregate), handlePlans stitches in the list-query time, and both the
// Server-Timing header and the debug LogArgs carry the full split. These are the
// signals that turn a slow /plans render from "guessed" into "measured" and make a
// reintroduced per-row fan-out show up as a NAMED join that scales with plan count.
//
// It reuses the self-contained seedManyPlansFake / countingEnrichServer harness
// from plans_enrich_measure_test.go (its own counting+latency daemon and N-plan
// seeder) so it never touches the shared seedPlansFake fixture.

// the eight bulk joins planIndexEnrichTimed times, one metric each, across the
// two concurrent waves.
var wantJoinNames = []string{
	"join_slugs", "join_assistants", "join_stepdata", "join_gardeners",
	"join_stalls", "join_escalations", "join_crews", "join_mrs",
	// join_blockers (ka-0000005352 iter-55) — the blocker-state signal, timed
	// like every other wave-2 bulk join.
	"join_blockers",
}

// TestPlansEnrichTiming_RecordsEachJoin proves the enrich pass records one timing
// entry PER bulk join (not just the two wave aggregates), so a slow render can be
// blamed on a specific join. It asserts every expected join name is present
// exactly once and the wave/assemble sequential sections are still recorded.
func TestPlansEnrichTiming_RecordsEachJoin(t *testing.T) {
	f, epics := seedManyPlansFake(12)
	s, _ := countingEnrichServer(f, 0)
	_, tm := s.planIndexEnrichTimed(context.Background(), epics)

	if tm == nil {
		t.Fatal("planIndexEnrichTimed returned nil timing")
	}
	if tm.PlanCount != 12 {
		t.Fatalf("PlanCount = %d, want 12", tm.PlanCount)
	}

	// Every join is timed exactly once.
	got := map[string]int{}
	for _, j := range tm.Joins {
		got[j.Name]++
	}
	for _, name := range wantJoinNames {
		if got[name] != 1 {
			t.Fatalf("join %q recorded %d times, want exactly 1; joins=%v", name, got[name], joinNames(tm.Joins))
		}
	}
	if len(tm.Joins) != len(wantJoinNames) {
		t.Fatalf("recorded %d joins, want %d; got %v", len(tm.Joins), len(wantJoinNames), joinNames(tm.Joins))
	}

	// The sequential sections (waves + assemble) are still recorded — the join
	// split is ADDITIVE, it does not replace the wave aggregates that sum to the
	// enrich wall clock.
	for _, want := range []string{"wave1", "wave2", "assemble"} {
		if !hasSection(tm.Sections, want) {
			t.Fatalf("missing sequential section %q; sections=%v", want, joinNames(tm.Sections))
		}
	}
}

// TestPlansEnrichTiming_JoinsAreConcurrent guards the concurrency claim that
// justifies reporting per-join times SEPARATELY from the wave total: the joins in
// a wave overlap, so each wave's wall clock is the SLOWEST branch, not the sum. We
// inject a per-round-trip latency and assert wave1 is far below the serial sum of
// its four joins — proving the timing reflects real concurrent execution (and
// that the instrumentation itself did not serialize the joins).
func TestPlansEnrichTiming_JoinsAreConcurrent(t *testing.T) {
	const rtt = 5 * time.Millisecond
	f, epics := seedManyPlansFake(12)
	s, _ := countingEnrichServer(f, rtt)
	_, tm := s.planIndexEnrichTimed(context.Background(), epics)

	var wave1 time.Duration
	for _, sec := range tm.Sections {
		if sec.Name == "wave1" {
			wave1 = sec.Dur
		}
	}
	if wave1 == 0 {
		t.Fatal("wave1 section not recorded")
	}

	// Sum the four wave-1 joins. If the joins ran serially the wave would be ~their
	// sum; because they fan out concurrently the wave is far below it. Assert the
	// wave is strictly less than the serial sum (a generous bound that still fails a
	// regression that serialized the wave).
	var wave1JoinSum time.Duration
	for _, j := range tm.Joins {
		switch j.Name {
		case "join_slugs", "join_assistants", "join_stepdata", "join_gardeners":
			wave1JoinSum += j.Dur
		}
	}
	if wave1 >= wave1JoinSum {
		t.Fatalf("wave1 wall clock %v >= sum of its joins %v — joins are not running concurrently", wave1, wave1JoinSum)
	}
	t.Logf("wave1=%v, serial sum of its 4 joins=%v (concurrency saved ~%v)", wave1, wave1JoinSum, wave1JoinSum-wave1)
}

// TestPlansEnrichTiming_HeaderCarriesListAndJoins proves the Server-Timing header
// (the operator-visible surface) carries the FULL split: the plan count, the
// stitched-in list query, the enrich total, each sequential wave, and every named
// join. This is the "per-section timing is visible" acceptance criterion.
func TestPlansEnrichTiming_HeaderCarriesListAndJoins(t *testing.T) {
	f, epics := seedManyPlansFake(12)
	s, _ := countingEnrichServer(f, 0)
	_, tm := s.planIndexEnrichTimed(context.Background(), epics)
	// Stitch in a list-query time the way handlePlans does.
	tm.ListQuery = 7 * time.Millisecond

	header := tm.ServerTimingHeader()
	for _, metric := range append([]string{"plans", "list", "enrich_total", "wave1", "wave2", "assemble"}, wantJoinNames...) {
		// Each metric renders as `<name>;...` — assert the token is present with its
		// trailing semicolon so a substring match can't spuriously hit a longer name.
		if !strings.Contains(header, metric+";") {
			t.Fatalf("Server-Timing header missing metric %q\nheader: %s", metric, header)
		}
	}

	// When the list query was NOT measured (the enrich-only callers), the header
	// omits the `list` metric rather than reporting a misleading zero.
	tm.ListQuery = 0
	if strings.Contains(tm.ServerTimingHeader(), "list;") {
		t.Fatalf("Server-Timing header should omit `list` when unmeasured\nheader: %s", tm.ServerTimingHeader())
	}
}

// TestPlansEnrichTiming_LogArgsCarryListAndJoins proves the structured debug line
// (the log-visible surface) carries the same split: plan_count, list_ms (when
// measured), total_ms, and one "<name>_ms" key per section and per join. Flat keys
// so an operator can `grep join_stepdata_ms` across renders.
func TestPlansEnrichTiming_LogArgsCarryListAndJoins(t *testing.T) {
	f, epics := seedManyPlansFake(12)
	s, _ := countingEnrichServer(f, 0)
	_, tm := s.planIndexEnrichTimed(context.Background(), epics)
	tm.ListQuery = 7 * time.Millisecond

	keys := logArgKeys(tm.LogArgs())
	for _, want := range []string{"plan_count", "list_ms", "total_ms", "wave1_ms", "wave2_ms", "assemble_ms"} {
		if _, ok := keys[want]; !ok {
			t.Fatalf("LogArgs missing key %q; keys=%v", want, keys)
		}
	}
	for _, name := range wantJoinNames {
		if _, ok := keys[name+"_ms"]; !ok {
			t.Fatalf("LogArgs missing join key %q; keys=%v", name+"_ms", keys)
		}
	}

	// list_ms is omitted when the list query was not measured.
	tm.ListQuery = 0
	if _, ok := logArgKeys(tm.LogArgs())[";list_ms"]; ok {
		t.Fatal("LogArgs should omit list_ms when the list query is unmeasured")
	}
	if _, ok := logArgKeys(tm.LogArgs())["list_ms"]; ok {
		t.Fatal("LogArgs should omit list_ms when the list query is unmeasured")
	}
}

// hasSection reports whether a (name,dur) slice carries a section by name.
func hasSection(secs []planEnrichSection, name string) bool {
	for _, s := range secs {
		if s.Name == name {
			return true
		}
	}
	return false
}

// joinNames extracts the names of a section slice for a readable failure message.
func joinNames(secs []planEnrichSection) []string {
	out := make([]string, len(secs))
	for i, s := range secs {
		out[i] = s.Name
	}
	return out
}

// logArgKeys turns a flat slog key/value slice into a set of the string keys.
func logArgKeys(args []any) map[string]struct{} {
	keys := map[string]struct{}{}
	for i := 0; i+1 < len(args); i += 2 {
		if k, ok := args[i].(string); ok {
			keys[k] = struct{}{}
		}
	}
	return keys
}
