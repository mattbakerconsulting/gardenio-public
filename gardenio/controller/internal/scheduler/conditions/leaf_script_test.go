package conditions

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// stubProbeClient embeds stubBeadClient (to satisfy BeadClient) and adds
// GetBead so it exercises the "script" leaf's probe-verdict read path. A stub
// that does NOT implement GetBead exercises the fail-open "client lacks
// capability" branch (see TestScriptLeaf_ClientLacksGetBead).
type stubProbeClient struct {
	stubBeadClient
	bead     *beadsapi.BeadDetail
	err      error
	getCalls []string
}

func (s *stubProbeClient) GetBead(_ context.Context, beadID string) (*beadsapi.BeadDetail, error) {
	s.getCalls = append(s.getCalls, beadID)
	if s.err != nil {
		return nil, s.err
	}
	return s.bead, nil
}

// fixedClock is a frozen "now" the staleness tests anchor against, so a fresh
// verdict is one whose close time is at/after this minus max-age.
var fixedClock = time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)

// closedProbe builds a closed type=script bead stamped with exit_code and a
// close time, matching what `gb probe run` writes on completion.
func closedProbe(id string, exit int, closedAt time.Time) *beadsapi.BeadDetail {
	return &beadsapi.BeadDetail{
		ID:        id,
		Type:      "script",
		Status:    "closed",
		Fields:    map[string]string{"exit_code": strconv.Itoa(exit)},
		ClosedAt:  closedAt.Format(time.RFC3339),
		UpdatedAt: closedAt,
	}
}

// withFixedClock installs a frozen clock for the duration of a test.
func withFixedClock(t *testing.T, now time.Time) {
	t.Helper()
	prev := scriptNowFunc
	scriptNowFunc = func() time.Time { return now }
	t.Cleanup(func() { scriptNowFunc = prev })
}

func evalScriptLeaf(t *testing.T, leaf Leaf, client BeadClient) (bool, error) {
	t.Helper()
	return Evaluate(context.Background(), Expression{Leaf: leaf}, Env{Bead: client})
}

// The ONE suppression path: a fresh, closed probe that exited exactly 0.
func TestScriptLeaf_FreshExitZero_Suppresses(t *testing.T) {
	withFixedClock(t, fixedClock)
	c := &stubProbeClient{bead: closedProbe("gd-probe", 0, fixedClock.Add(-time.Minute))}
	got, err := evalScriptLeaf(t, Leaf{Type: "script", ScriptBead: "gd-probe"}, c)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got {
		t.Fatalf("fresh exit-0 probe must SUPPRESS (met=false); got met=true")
	}
	if len(c.getCalls) != 1 || c.getCalls[0] != "gd-probe" {
		t.Fatalf("expected exactly one GetBead(gd-probe); got %v", c.getCalls)
	}
}

// Non-zero exit ⇒ wake the gardener (met=true).
func TestScriptLeaf_ExitNonZero_Wakes(t *testing.T) {
	withFixedClock(t, fixedClock)
	for _, exit := range []int{1, 2, 137} {
		c := &stubProbeClient{bead: closedProbe("gd-probe", exit, fixedClock.Add(-time.Minute))}
		got, err := evalScriptLeaf(t, Leaf{Type: "script", ScriptBead: "gd-probe"}, c)
		if err != nil {
			t.Fatalf("exit %d: unexpected error: %v", exit, err)
		}
		if !got {
			t.Fatalf("exit %d: non-zero probe must WAKE (met=true); got met=false", exit)
		}
	}
}

// Fail-open matrix: every degraded input must return (true, nil) — spawn.
func TestScriptLeaf_FailOpen(t *testing.T) {
	withFixedClock(t, fixedClock)
	fresh := fixedClock.Add(-time.Minute)

	openBead := closedProbe("gd-probe", 0, fresh)
	openBead.Status = "open" // not yet completed

	noExit := closedProbe("gd-probe", 0, fresh)
	delete(noExit.Fields, "exit_code")

	badExit := closedProbe("gd-probe", 0, fresh)
	badExit.Fields["exit_code"] = "not-a-number"

	stale := closedProbe("gd-probe", 0, fixedClock.Add(-2*time.Hour)) // older than default 1h

	noTimestamp := &beadsapi.BeadDetail{
		ID: "gd-probe", Type: "script", Status: "closed",
		Fields: map[string]string{"exit_code": "0"},
		// no ClosedAt, zero UpdatedAt ⇒ no usable verdict time
	}

	cases := []struct {
		name   string
		leaf   Leaf
		client BeadClient
	}{
		{"empty script_bead", Leaf{Type: "script"}, &stubProbeClient{bead: closedProbe("x", 0, fresh)}},
		{"unsupported op", Leaf{Type: "script", ScriptBead: "gd-probe", Op: "zero"}, &stubProbeClient{bead: closedProbe("gd-probe", 0, fresh)}},
		{"get error", Leaf{Type: "script", ScriptBead: "gd-probe"}, &stubProbeClient{err: errors.New("boom")}},
		{"nil bead", Leaf{Type: "script", ScriptBead: "gd-probe"}, &stubProbeClient{bead: nil}},
		{"open bead (no verdict)", Leaf{Type: "script", ScriptBead: "gd-probe"}, &stubProbeClient{bead: openBead}},
		{"no exit_code", Leaf{Type: "script", ScriptBead: "gd-probe"}, &stubProbeClient{bead: noExit}},
		{"unparseable exit_code", Leaf{Type: "script", ScriptBead: "gd-probe"}, &stubProbeClient{bead: badExit}},
		{"stale verdict", Leaf{Type: "script", ScriptBead: "gd-probe"}, &stubProbeClient{bead: stale}},
		{"no verdict timestamp", Leaf{Type: "script", ScriptBead: "gd-probe"}, &stubProbeClient{bead: noTimestamp}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := evalScriptLeaf(t, tc.leaf, tc.client)
			if err != nil {
				t.Fatalf("fail-open must not error; got %v", err)
			}
			if !got {
				t.Fatalf("%s must FAIL OPEN (met=true); got met=false", tc.name)
			}
		})
	}
}

// A client that does not implement GetBead fails open.
func TestScriptLeaf_ClientLacksGetBead(t *testing.T) {
	withFixedClock(t, fixedClock)
	// stubBeadClient satisfies BeadClient but NOT scriptProbeClient.
	bc := &stubBeadClient{fn: count(0)}
	got, err := evalScriptLeaf(t, Leaf{Type: "script", ScriptBead: "gd-probe"}, bc)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !got {
		t.Fatalf("client lacking GetBead must FAIL OPEN (met=true); got met=false")
	}
}

// Op "nonzero" and empty op are equivalent.
func TestScriptLeaf_OpNonzeroExplicit(t *testing.T) {
	withFixedClock(t, fixedClock)
	c := &stubProbeClient{bead: closedProbe("gd-probe", 0, fixedClock.Add(-time.Minute))}
	got, err := evalScriptLeaf(t, Leaf{Type: "script", ScriptBead: "gd-probe", Op: "nonzero"}, c)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got {
		t.Fatalf("explicit op=nonzero, fresh exit 0 must SUPPRESS (met=false); got met=true")
	}
}

// A verdict exactly at the staleness boundary is still trusted; just past it
// fails open. Guards the boundary condition of the staleness gate.
func TestScriptLeaf_StalenessBoundary(t *testing.T) {
	withFixedClock(t, fixedClock)

	// Exactly max-age old (age == maxAge, not > maxAge): still trusted.
	atBoundary := &stubProbeClient{bead: closedProbe("gd-probe", 0, fixedClock.Add(-scriptDefaultMaxAge))}
	got, err := evalScriptLeaf(t, Leaf{Type: "script", ScriptBead: "gd-probe"}, atBoundary)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got {
		t.Fatalf("verdict exactly at max-age must still SUPPRESS (met=false); got met=true")
	}

	// One second past: fails open.
	past := &stubProbeClient{bead: closedProbe("gd-probe", 0, fixedClock.Add(-scriptDefaultMaxAge-time.Second))}
	got, err = evalScriptLeaf(t, Leaf{Type: "script", ScriptBead: "gd-probe"}, past)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !got {
		t.Fatalf("verdict past max-age must FAIL OPEN (met=true); got met=false")
	}
}

// A per-leaf Value overrides the staleness window; Value<=0 disables it.
func TestScriptLeaf_MaxAgeOverride(t *testing.T) {
	withFixedClock(t, fixedClock)

	// Custom short window: a 10-minute-old verdict is stale under a 5m window.
	shortWin := &stubProbeClient{bead: closedProbe("gd-probe", 0, fixedClock.Add(-10*time.Minute))}
	got, err := evalScriptLeaf(t, Leaf{Type: "script", ScriptBead: "gd-probe", Value: "5m"}, shortWin)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !got {
		t.Fatalf("10m-old verdict under 5m window must FAIL OPEN (met=true); got met=false")
	}

	// Disabled staleness gate: an ancient verdict still suppresses.
	disabled := &stubProbeClient{bead: closedProbe("gd-probe", 0, fixedClock.Add(-30*24*time.Hour))}
	got, err = evalScriptLeaf(t, Leaf{Type: "script", ScriptBead: "gd-probe", Value: "0"}, disabled)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got {
		t.Fatalf("staleness gate disabled (Value=0): ancient exit-0 verdict must SUPPRESS (met=false); got met=true")
	}
}

// Staleness falls back to UpdatedAt when ClosedAt is absent/unparseable.
func TestScriptLeaf_VerdictTimeFallsBackToUpdatedAt(t *testing.T) {
	withFixedClock(t, fixedClock)
	b := &beadsapi.BeadDetail{
		ID: "gd-probe", Type: "script", Status: "closed",
		Fields:    map[string]string{"exit_code": "0"},
		ClosedAt:  "", // no close time
		UpdatedAt: fixedClock.Add(-time.Minute),
	}
	got, err := evalScriptLeaf(t, Leaf{Type: "script", ScriptBead: "gd-probe"}, &stubProbeClient{bead: b})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got {
		t.Fatalf("fresh verdict via UpdatedAt fallback must SUPPRESS (met=false); got met=true")
	}
}

// The legacy Condition shape round-trips ScriptBead through ToExpression, so
// the daemon-scoped gate (which uses []Condition) can carry a script leaf.
func TestScriptLeaf_LegacyConditionRoundTrip(t *testing.T) {
	withFixedClock(t, fixedClock)
	cond := Condition{Type: "script", ScriptBead: "gd-probe", Op: "nonzero"}
	c := &stubProbeClient{bead: closedProbe("gd-probe", 0, fixedClock.Add(-time.Minute))}
	got, err := EvaluateCondition(context.Background(), c, cond, "gardenio")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got {
		t.Fatalf("legacy Condition script leaf, fresh exit 0 must SUPPRESS (met=false); got met=true")
	}
}

// script_bead survives JSON parse/marshal round-trip on both shapes.
func TestScriptLeaf_JSONRoundTrip(t *testing.T) {
	raw := `{"type":"script","script_bead":"gd-probe","op":"nonzero"}`
	e, err := ParseExpression(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if e.ScriptBead != "gd-probe" {
		t.Fatalf("ScriptBead did not parse; got %q", e.ScriptBead)
	}
	out, err := MarshalExpression(e)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	e2, err := ParseExpression(out)
	if err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	if e2.ScriptBead != "gd-probe" {
		t.Fatalf("ScriptBead lost on round-trip; got %q", e2.ScriptBead)
	}
}
