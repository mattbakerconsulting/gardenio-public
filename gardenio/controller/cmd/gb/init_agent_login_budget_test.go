package main

import "testing"

// The budget trips only on the Nth CONSECUTIVE login-blocked startup.
func TestLoginFailureTracker_TripsAtBudget(t *testing.T) {
	tr := newLoginFailureTracker(3)

	if tr.Record() {
		t.Fatal("budget tripped on the 1st failure (want 3)")
	}
	if tr.Record() {
		t.Fatal("budget tripped on the 2nd failure (want 3)")
	}
	if !tr.Record() {
		t.Fatal("budget did NOT trip on the 3rd consecutive failure")
	}
	if tr.Count() != 3 {
		t.Fatalf("Count() = %d, want 3", tr.Count())
	}
}

// A non-login-blocked session resets the streak — mirroring the loop's
// `else { loginFailures.Reset() }` branch — so the budget only fires on a
// genuinely sustained credential-not-ready condition.
func TestLoginFailureTracker_ResetBreaksStreak(t *testing.T) {
	tr := newLoginFailureTracker(3)

	tr.Record() // 1
	tr.Record() // 2
	tr.Reset()  // productive session / normal crash / credential switch
	if tr.Count() != 0 {
		t.Fatalf("Count() after Reset = %d, want 0", tr.Count())
	}

	// Two more failures must NOT trip — the earlier two were cleared.
	if tr.Record() {
		t.Fatal("budget tripped after reset on the 1st post-reset failure")
	}
	if tr.Record() {
		t.Fatal("budget tripped after reset on the 2nd post-reset failure")
	}
	if !tr.Record() {
		t.Fatal("budget did NOT trip on the 3rd post-reset failure")
	}
}

// The self-heal shape from the incident: agent a1sl's credential arrived on its
// first session, so it never accumulates a login failure and never trips.
func TestLoginFailureTracker_SelfHealNeverTrips(t *testing.T) {
	tr := newLoginFailureTracker(3)
	// First (and only) session became productive → the loop calls Reset(), not
	// Record(). Simulate a handful of productive sessions.
	for range 10 {
		tr.Reset()
		if tr.Count() != 0 {
			t.Fatalf("Count() = %d on a productive session, want 0", tr.Count())
		}
	}
}

// A budget of 0 disables the cap (Record never trips) — defensive, so a
// misconfigured budget can never wedge a pod closed.
func TestLoginFailureTracker_DisabledBudget(t *testing.T) {
	tr := newLoginFailureTracker(0)
	for range 100 {
		if tr.Record() {
			t.Fatal("disabled budget (0) tripped")
		}
	}
}

// The production constant mirrors coop's in-session watchdog threshold (3).
func TestStartupLoginFailureBudget_MatchesInSessionWatchdog(t *testing.T) {
	if startupLoginFailureBudget != 3 {
		t.Fatalf("startupLoginFailureBudget = %d, want 3 (mirror coop LoginRequiredWatchdog threshold)", startupLoginFailureBudget)
	}
}
