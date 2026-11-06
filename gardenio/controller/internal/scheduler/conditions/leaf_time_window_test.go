package conditions

import (
	"context"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// withFrozenClock runs fn with the time_window leaf's clock pinned to now.
func withFrozenClock(now time.Time, fn func()) {
	prev := timeWindowNowFunc
	timeWindowNowFunc = func() time.Time { return now }
	defer func() { timeWindowNowFunc = prev }()
	fn()
}

// mustUTC parses an RFC3339 instant or fails the test setup.
func mustUTC(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("bad test timestamp %q: %v", s, err)
	}
	return ts
}

// quietHoursLeaf is the exact gardenio release quiet-hours gate: allow firing
// OUTSIDE PST business hours (07:00-18:00, weekdays), in America/Los_Angeles.
func quietHoursLeaf() Leaf {
	return Leaf{
		Type:     "time_window",
		Timezone: "America/Los_Angeles",
		Window:   "07:00-18:00",
		Days:     []string{"Mon", "Tue", "Wed", "Thu", "Fri"},
		Op:       "outside",
	}
}

func TestTimeWindow_QuietHours_PDT(t *testing.T) {
	l := quietHoursLeaf()
	cases := []struct {
		name    string
		utc     string // instant in UTC
		allowed bool   // expected: firing allowed (outside business hours)?
	}{
		// PDT (UTC-7) is in effect in July/August.
		// 2026-08-13 is a Thursday.
		{"thu 10:00 PDT = business hours", "2026-08-13T17:00:00Z", false},      // 10:00 PDT
		{"thu 06:59 PDT = just before open", "2026-08-13T13:59:00Z", true},     // 06:59 PDT
		{"thu 07:00 PDT = open edge (blocked)", "2026-08-13T14:00:00Z", false}, // 07:00 PDT
		{"thu 17:59 PDT = last blocked minute", "2026-08-14T00:59:00Z", false}, // 17:59 PDT
		{"thu 18:00 PDT = close edge (allowed)", "2026-08-14T01:00:00Z", true}, // 18:00 PDT
		{"thu 23:00 PDT = evening allowed", "2026-08-14T06:00:00Z", true},      // 23:00 PDT
		{"thu 02:00 PDT = overnight allowed", "2026-08-13T09:00:00Z", true},    // 02:00 PDT
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withFrozenClock(mustUTC(t, tc.utc), func() {
				got, err := evalTimeWindow(l)
				if err != nil {
					t.Fatalf("evalTimeWindow error: %v", err)
				}
				if got != tc.allowed {
					t.Errorf("allowed = %v, want %v (%s)", got, tc.allowed, tc.name)
				}
			})
		})
	}
}

func TestTimeWindow_QuietHours_PST_DSTBoundaryCorrect(t *testing.T) {
	// The whole reason this leaf lives controller-side: prove the SAME wall-clock
	// hour maps to different UTC offsets across the DST boundary, and the gate
	// tracks the real zone (not a fixed offset, not a silent-UTC shell gate).
	l := quietHoursLeaf()

	// Winter: PST (UTC-8). 2026-01-15 is a Thursday. 10:00 PST = 18:00 UTC.
	withFrozenClock(mustUTC(t, "2026-01-15T18:00:00Z"), func() {
		got, err := evalTimeWindow(l)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if got {
			t.Errorf("winter 10:00 PST: allowed=true, want false (business hours)")
		}
	})
	// The same 18:00 UTC in SUMMER is 11:00 PDT — still business hours, still
	// blocked — but the point is the offset differs. Cross-check a summer
	// instant that is 18:00 UTC = 11:00 PDT.
	withFrozenClock(mustUTC(t, "2026-07-16T18:00:00Z"), func() { // Thursday
		got, err := evalTimeWindow(l)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if got {
			t.Errorf("summer 11:00 PDT: allowed=true, want false (business hours)")
		}
	})
	// A UTC instant that is business-hours in winter but off-hours in summer,
	// proving the offset is really tracked. 2026-01-15 01:30 UTC = 2026-01-14
	// 17:30 PST (Wed, business hours, blocked). 2026-07-16 01:30 UTC =
	// 2026-07-15 18:30 PDT (Wed, after close, allowed).
	withFrozenClock(mustUTC(t, "2026-01-15T01:30:00Z"), func() {
		got, _ := evalTimeWindow(l)
		if got {
			t.Errorf("winter 17:30 PST: allowed=true, want false")
		}
	})
	withFrozenClock(mustUTC(t, "2026-07-16T01:30:00Z"), func() {
		got, _ := evalTimeWindow(l)
		if !got {
			t.Errorf("summer 18:30 PDT: allowed=false, want true")
		}
	})
}

func TestTimeWindow_Weekends_AlwaysAllowed(t *testing.T) {
	l := quietHoursLeaf()
	// 2026-08-15 is a Saturday, 2026-08-16 a Sunday. 10:00 PDT = 17:00 UTC.
	for _, utc := range []string{"2026-08-15T17:00:00Z", "2026-08-16T17:00:00Z"} {
		withFrozenClock(mustUTC(t, utc), func() {
			got, err := evalTimeWindow(l)
			if err != nil {
				t.Fatalf("err: %v", err)
			}
			if !got {
				t.Errorf("weekend %s: allowed=false, want true (weekends release freely)", utc)
			}
		})
	}
}

func TestTimeWindow_InsideOp(t *testing.T) {
	l := quietHoursLeaf()
	l.Op = "inside"
	// Thu 10:00 PDT = inside business hours ⇒ inside op is TRUE.
	withFrozenClock(mustUTC(t, "2026-08-13T17:00:00Z"), func() {
		got, err := evalTimeWindow(l)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if !got {
			t.Errorf("inside op, business hours: got false, want true")
		}
	})
	// Thu 23:00 PDT = outside ⇒ inside op is FALSE.
	withFrozenClock(mustUTC(t, "2026-08-14T06:00:00Z"), func() {
		got, _ := evalTimeWindow(l)
		if got {
			t.Errorf("inside op, off hours: got true, want false")
		}
	})
}

func TestTimeWindow_DefaultOpIsOutside(t *testing.T) {
	l := quietHoursLeaf()
	l.Op = ""                                                    // default
	withFrozenClock(mustUTC(t, "2026-08-13T17:00:00Z"), func() { // 10:00 PDT
		got, err := evalTimeWindow(l)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if got {
			t.Errorf("default op (outside), business hours: got true, want false")
		}
	})
}

func TestTimeWindow_WrappingWindow(t *testing.T) {
	// Overnight window 22:00-06:00, every day, op "inside" — exercises the
	// midnight-crossing branch on both sides.
	l := Leaf{Type: "time_window", Timezone: "UTC", Window: "22:00-06:00", Op: "inside"}
	cases := []struct {
		utc    string
		inside bool
	}{
		{"2026-08-13T23:00:00Z", true},  // pre-midnight segment
		{"2026-08-13T02:00:00Z", true},  // post-midnight segment
		{"2026-08-13T21:59:00Z", false}, // before open
		{"2026-08-13T06:00:00Z", false}, // at close edge
		{"2026-08-13T12:00:00Z", false}, // midday
	}
	for _, tc := range cases {
		withFrozenClock(mustUTC(t, tc.utc), func() {
			got, err := evalTimeWindow(l)
			if err != nil {
				t.Fatalf("err: %v", err)
			}
			if got != tc.inside {
				t.Errorf("wrapping %s: inside=%v, want %v", tc.utc, got, tc.inside)
			}
		})
	}
}

func TestTimeWindow_WrappingWindow_DayScopedToOpenDay(t *testing.T) {
	// A Fri 22:00-06:00 window (weekend-eve). The post-midnight tail on Sat
	// morning must still count even though Sat is out of scope, because the
	// window OPENED on Friday.
	l := Leaf{Type: "time_window", Timezone: "UTC", Window: "22:00-06:00",
		Days: []string{"Fri"}, Op: "inside"}
	// 2026-08-14 is a Friday. Sat 2026-08-15 02:00 UTC is the tail of Friday's
	// window.
	withFrozenClock(mustUTC(t, "2026-08-15T02:00:00Z"), func() { // Sat 02:00
		got, err := evalTimeWindow(l)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if !got {
			t.Errorf("Sat 02:00 tail of Fri window: inside=false, want true")
		}
	})
	// Sat's OWN 23:00 must NOT count (Sat is out of scope, Sun tail only).
	withFrozenClock(mustUTC(t, "2026-08-15T23:00:00Z"), func() { // Sat 23:00
		got, _ := evalTimeWindow(l)
		if got {
			t.Errorf("Sat 23:00 (not a Fri): inside=true, want false")
		}
	})
}

func TestTimeWindow_Errors(t *testing.T) {
	cases := []struct {
		name string
		leaf Leaf
	}{
		{"bad timezone", Leaf{Type: "time_window", Timezone: "Nowhere/Nope", Window: "07:00-18:00"}},
		{"empty window", Leaf{Type: "time_window", Timezone: "UTC", Window: ""}},
		{"no separator", Leaf{Type: "time_window", Timezone: "UTC", Window: "0700-1800"}},
		{"bad hour", Leaf{Type: "time_window", Timezone: "UTC", Window: "25:00-26:00"}},
		{"bad minute", Leaf{Type: "time_window", Timezone: "UTC", Window: "07:99-18:00"}},
		{"unknown op", Leaf{Type: "time_window", Timezone: "UTC", Window: "07:00-18:00", Op: "maybe"}},
		{"unknown day", Leaf{Type: "time_window", Timezone: "UTC", Window: "07:00-18:00", Days: []string{"Funday"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withFrozenClock(mustUTC(t, "2026-08-13T17:00:00Z"), func() {
				_, err := evalTimeWindow(tc.leaf)
				if err == nil {
					t.Errorf("%s: expected error, got nil (gate must fail closed)", tc.name)
				}
			})
		})
	}
}

func TestTimeWindow_ShorthandDays(t *testing.T) {
	// "weekdays" shorthand equals the explicit Mon-Fri set.
	l := Leaf{Type: "time_window", Timezone: "America/Los_Angeles", Window: "07:00-18:00",
		Days: []string{"weekdays"}, Op: "outside"}
	withFrozenClock(mustUTC(t, "2026-08-13T17:00:00Z"), func() { // Thu 10:00 PDT
		got, err := evalTimeWindow(l)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if got {
			t.Errorf("weekdays shorthand, Thu business hours: allowed=true, want false")
		}
	})
	withFrozenClock(mustUTC(t, "2026-08-15T17:00:00Z"), func() { // Sat 10:00 PDT
		got, _ := evalTimeWindow(l)
		if !got {
			t.Errorf("weekdays shorthand, Sat: allowed=false, want true")
		}
	})
}

// TestTimeWindow_ThroughEvaluate proves the leaf is reachable through the
// public Evaluate entrypoint with a nil BeadClient — the exact Env shape a
// schedule trigger's condition evaluation and a cron preflight both use for a
// clock-only gate.
func TestTimeWindow_ThroughEvaluate_NilBeadClient(t *testing.T) {
	expr := Expression{Leaf: quietHoursLeaf()}
	withFrozenClock(mustUTC(t, "2026-08-13T17:00:00Z"), func() { // Thu 10:00 PDT
		got, err := Evaluate(context.Background(), expr, Env{ /* Bead: nil */ })
		if err != nil {
			t.Fatalf("Evaluate error: %v", err)
		}
		if got {
			t.Errorf("Evaluate business hours: matched=true, want false (suppress fire)")
		}
	})
	withFrozenClock(mustUTC(t, "2026-08-14T06:00:00Z"), func() { // Thu 23:00 PDT
		got, err := Evaluate(context.Background(), expr, Env{})
		if err != nil {
			t.Fatalf("Evaluate error: %v", err)
		}
		if !got {
			t.Errorf("Evaluate off-hours: matched=false, want true (allow fire)")
		}
	})
}

// TestTimeWindow_LegacyConditionRoundTrip proves the legacy Condition path
// (the one the trigger:schedule source actually uses) carries the time_window
// fields through ToExpression.
func TestTimeWindow_LegacyConditionRoundTrip(t *testing.T) {
	raw := `[{"type":"time_window","timezone":"America/Los_Angeles","window":"07:00-18:00","days":["Mon","Tue","Wed","Thu","Fri"],"op":"outside"}]`
	conds, err := ParseConditions(raw)
	if err != nil {
		t.Fatalf("ParseConditions: %v", err)
	}
	if len(conds) != 1 {
		t.Fatalf("got %d conditions, want 1", len(conds))
	}
	expr := conds[0].ToExpression()
	if expr.Leaf.Type != "time_window" || expr.Leaf.Window != "07:00-18:00" {
		t.Fatalf("lowered leaf lost fields: %+v", expr.Leaf)
	}
	// The legacy EvaluateConditions requires a non-nil client (prod always
	// passes the daemon); the time_window leaf never touches it, so a stub that
	// would panic if queried proves the leaf never reaches the store.
	client := &stubBeadClient{fn: func(beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
		t.Fatalf("time_window leaf must not query the bead store")
		return nil, nil
	}}
	withFrozenClock(mustUTC(t, "2026-08-13T17:00:00Z"), func() { // Thu 10:00 PDT
		ok, err := EvaluateConditions(context.Background(), client, conds, "gardenio")
		if err != nil {
			t.Fatalf("EvaluateConditions: %v", err)
		}
		if ok {
			t.Errorf("legacy condition business hours: met=true, want false")
		}
	})
}
