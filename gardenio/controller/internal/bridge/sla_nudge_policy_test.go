package bridge

import (
	"testing"
	"time"
)

func TestSLAPolicy_RungForAge_Ladder(t *testing.T) {
	p := DefaultSLAPolicy()

	tests := []struct {
		name     string
		priority int
		age      time.Duration
		want     SLARung
	}{
		// P0: SLA=1h, rung1@15m, rung2@36m, rung3@1h.
		{"p0 just below rung1", 0, 14*time.Minute + 59*time.Second, SLARungNone},
		{"p0 at rung1", 0, 15 * time.Minute, SLARung1},
		{"p0 mid rung1", 0, 20 * time.Minute, SLARung1},
		{"p0 just below rung2", 0, 36*time.Minute - time.Second, SLARung1},
		{"p0 at rung2", 0, 36 * time.Minute, SLARung2},
		{"p0 just below rung3", 0, time.Hour - time.Second, SLARung2},
		{"p0 at rung3", 0, time.Hour, SLARung3},
		{"p0 well past rung3", 0, 5 * time.Hour, SLARung3},

		// P1: SLA=4h, rung1@1h, rung2@2.4h, rung3@4h.
		{"p1 below rung1", 1, 59 * time.Minute, SLARungNone},
		{"p1 at rung1", 1, time.Hour, SLARung1},
		{"p1 at rung2", 1, time.Duration(2.4 * float64(time.Hour)), SLARung2},
		{"p1 below rung3", 1, 4*time.Hour - time.Second, SLARung2},
		{"p1 at rung3", 1, 4 * time.Hour, SLARung3},

		// P2: SLA=24h, rung1@6h, rung2@14h24m, rung3@24h.
		{"p2 below rung1", 2, 5*time.Hour + 59*time.Minute, SLARungNone},
		{"p2 at rung1", 2, 6 * time.Hour, SLARung1},
		{"p2 at rung2", 2, 14*time.Hour + 24*time.Minute, SLARung2},
		{"p2 at rung3", 2, 24 * time.Hour, SLARung3},

		// P3: SLA=72h, rung1@18h, rung2@43h12m, rung3@72h.
		{"p3 below rung1", 3, 17*time.Hour + 59*time.Minute, SLARungNone},
		{"p3 at rung1", 3, 18 * time.Hour, SLARung1},
		{"p3 at rung2", 3, 43*time.Hour + 12*time.Minute, SLARung2},
		{"p3 at rung3", 3, 72 * time.Hour, SLARung3},

		// Unknown priority never nudges.
		{"unknown priority", 7, 1000 * time.Hour, SLARungNone},

		// Zero/negative age never nudges.
		{"zero age", 0, 0, SLARungNone},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := p.RungForAge(tt.priority, tt.age)
			if got != tt.want {
				t.Errorf("RungForAge(p%d, %s) = %v, want %v", tt.priority, tt.age, got, tt.want)
			}
		})
	}
}

func TestSLAPolicy_RungForAge_NoSLAConfigured(t *testing.T) {
	p := SLAPolicy{Rung1Fraction: 0.25, Rung2Fraction: 0.6}
	if got := p.RungForAge(0, 1000*time.Hour); got != SLARungNone {
		t.Errorf("RungForAge with empty SLAByPriority = %v, want SLARungNone", got)
	}
}

func TestSLARung_String(t *testing.T) {
	tests := []struct {
		r    SLARung
		want string
	}{
		{SLARungNone, "none"},
		{SLARung1, "rung1"},
		{SLARung2, "rung2"},
		{SLARung3, "overdue"},
	}
	for _, tt := range tests {
		if got := tt.r.String(); got != tt.want {
			t.Errorf("SLARung(%d).String() = %q, want %q", tt.r, got, tt.want)
		}
	}
}

func TestSLAPolicy_IsQuietHours(t *testing.T) {
	p := DefaultSLAPolicy()
	loc, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Skipf("tzdata not available in this environment: %v", err)
	}

	// 22:00 PT is inside the 21:00-08:00 quiet window.
	quiet := time.Date(2026, 1, 15, 22, 0, 0, 0, loc)
	got, err := p.IsQuietHours(quiet)
	if err != nil {
		t.Fatalf("IsQuietHours: %v", err)
	}
	if !got {
		t.Errorf("IsQuietHours(%v) = false, want true (inside 21:00-08:00 window)", quiet)
	}

	// 14:00 PT is outside the window.
	awake := time.Date(2026, 1, 15, 14, 0, 0, 0, loc)
	got, err = p.IsQuietHours(awake)
	if err != nil {
		t.Fatalf("IsQuietHours: %v", err)
	}
	if got {
		t.Errorf("IsQuietHours(%v) = true, want false (outside 21:00-08:00 window)", awake)
	}
}

func TestSLAPolicy_IsQuietHours_NilDisabled(t *testing.T) {
	p := SLAPolicy{SLAByPriority: map[int]time.Duration{0: time.Hour}}
	got, err := p.IsQuietHours(time.Now())
	if err != nil {
		t.Fatalf("IsQuietHours: %v", err)
	}
	if got {
		t.Errorf("IsQuietHours with nil QuietHours = true, want false")
	}
}
