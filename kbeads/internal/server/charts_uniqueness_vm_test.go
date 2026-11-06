package server

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// charts_uniqueness_vm_test.go — faithful correctness test against a REAL
// VictoriaMetrics, gated on BEADS_TEST_VMSELECT_URL.
//
// The DAU/WAU/MAU correctness story is a property of the PromQL engine's
// last_over_time membership semantics — it cannot be proven by a stubbed
// scalar. This test loads a hand-built fixture into a real VictoriaMetrics
// (the same v1.108.1 the helm chart pins) via /api/v1/import, then asserts
// computeUniqueness returns the EXACT distinct-operator counts the fixture is
// constructed to produce, including the cases that distinguish last_over_time
// from increase():
//
//   - op "5": a counter RESET inside the 1d window (pod restart) — must still
//     count once. increase() would mis-handle the reset.
//   - op "2": a SINGLE sample 3 days ago — must count in WAU/MAU.
//     increase() drops single-sample series entirely.
//   - op "6": TWO label series (mutation/kbeads + spawn/slack) for the SAME
//     operator today — count by(operator_id) must collapse to ONE.
//   - op "4": active 45 days ago — outside all windows, never counts.
//   - op "0": the unattributed sentinel bucket, active today — IS a counted
//     distinct operator.
//
// Expected: DAU=4 {1,5,6,0}, WAU=5 {1,2,5,6,0}, MAU=6 {1,2,3,5,6,0}.
//
// This is the same fixture and result documented in
// gardenio/docs/research/dau-wau-mau-benchmark.md, which records the run
// against a real VM (DAU=4/WAU=5/MAU=6, p99 ~3ms).
//
// To run locally:
//
//	victoria-metrics-prod -httpListenAddr=127.0.0.1:8428 &
//	BEADS_TEST_VMSELECT_URL=http://127.0.0.1:8428 go test ./internal/server/ -run VMFixture -v
func TestUniquenessVMFixture_DistinctCountsCorrect(t *testing.T) {
	base := strings.TrimRight(os.Getenv("BEADS_TEST_VMSELECT_URL"), "/")
	if base == "" {
		t.Skip("BEADS_TEST_VMSELECT_URL not set; skipping real-VictoriaMetrics correctness test")
	}

	// Evaluate at a fixed-ish "now" captured once so the import timestamps and
	// the query evaluation time share the same reference frame.
	now := time.Now().UTC()
	const day = 24 * time.Hour

	// Build the fixture as /api/v1/import JSONL. Each line is one series with
	// explicit (value,timestamp) pairs; timestamps are ms since epoch.
	type sample struct {
		ago time.Duration
		val float64
	}
	mkLine := func(op, atype, surface string, samples []sample) string {
		var vals, ts strings.Builder
		vals.WriteString("[")
		ts.WriteString("[")
		for i, s := range samples {
			if i > 0 {
				vals.WriteString(",")
				ts.WriteString(",")
			}
			fmt.Fprintf(&vals, "%g", s.val)
			fmt.Fprintf(&ts, "%d", now.Add(-s.ago).UnixMilli())
		}
		vals.WriteString("]")
		ts.WriteString("]")
		return fmt.Sprintf(
			`{"metric":{"__name__":"gardenio_operator_activity_total","operator_id":%q,"project":"gardenio","activity_type":%q,"surface":%q},"values":%s,"timestamps":%s}`,
			op, atype, surface, vals.String(), ts.String(),
		)
	}

	lines := []string{
		// op1: active today, monotonic — DAU/WAU/MAU.
		mkLine("1", "mutation", "kbeads", []sample{{2 * time.Hour, 3}, {1 * time.Hour, 5}}),
		// op2: single sample 3 days ago — WAU/MAU only (increase() would drop).
		mkLine("2", "mutation", "kbeads", []sample{{3 * day, 7}}),
		// op3: active 20 days ago — MAU only.
		mkLine("3", "spawn", "slack", []sample{{20 * day, 2}, {20*day - time.Hour, 4}}),
		// op4: active 45 days ago — outside all windows.
		mkLine("4", "mutation", "kbeads", []sample{{45 * day, 9}}),
		// op5: counter RESET inside 1d window — must still count once.
		mkLine("5", "session", "cli", []sample{{20 * time.Hour, 5}, {15 * time.Hour, 6}, {10 * time.Hour, 1}, {1 * time.Hour, 2}}),
		// op6: two label series for the same operator today — collapse to one.
		mkLine("6", "mutation", "kbeads", []sample{{2 * time.Hour, 4}}),
		mkLine("6", "spawn", "slack", []sample{{90 * time.Minute, 1}}),
		// op0: unattributed sentinel, active today — counts as distinct.
		mkLine("0", "view", "web", []sample{{30 * time.Minute, 11}}),
	}

	importVMFixture(t, base, strings.Join(lines, "\n"))

	vm := NewVMClient(base, os.Getenv("BEADS_TEST_VMSELECT_TOKEN"))
	s := NewBeadsServer(newMockStore(), nil)
	s.SetVMClient(vm)

	resp, err := s.computeUniqueness(context.Background(), now)
	if err != nil {
		t.Fatalf("computeUniqueness: %v", err)
	}
	got := map[string]int64{}
	for _, p := range resp.Panels {
		got[p.Key] = p.Count
	}
	want := map[string]int64{"dau": 4, "wau": 5, "mau": 6}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: got %d, want %d (fixture distinct-operator count). Full: %+v", k, got[k], v, got)
		}
	}
}

// importVMFixture loads JSONL into VictoriaMetrics and forces a flush so the
// data is immediately queryable.
func importVMFixture(t *testing.T, base, jsonl string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, base+"/api/v1/import", strings.NewReader(jsonl))
	if err != nil {
		t.Fatalf("build import request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("import to VM: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		t.Fatalf("VM import returned HTTP %d", resp.StatusCode)
	}
	// Best-effort force flush so imported data is searchable without waiting
	// for the background flush interval.
	flush, _ := http.NewRequest(http.MethodPost, base+"/internal/force_flush", bytes.NewReader(nil))
	if flush != nil {
		if fr, ferr := http.DefaultClient.Do(flush); ferr == nil {
			fr.Body.Close()
		}
	}
	// Small settle to let the flush land before the query.
	time.Sleep(500 * time.Millisecond)
}
