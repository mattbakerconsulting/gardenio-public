package main

// init_agent_login_budget.go — a small consecutive-failure counter for the
// credential-not-ready-at-spawn wedge (ka-0000007536).
//
// Why this exists: coop's in-session LoginRequiredWatchdog (threshold 3) is
// per-coop-process and in-memory, so it RESETS on every restart. A pod that
// keeps dying on the STARTUP login banner (bypassStartup detects the banner and
// shuts coop down before Claude ever runs a turn) never accumulates 3
// consecutive IN-SESSION login turns, so that watchdog can never trip. The only
// remaining backstop was maxRestarts (default 10) → close_reason
// "max_restarts_reached" — but the original incident (agent mpce) froze BELOW
// that cap and never reached it, and even when reached the close reason is the
// generic max_restarts, not the credential-specific not_logged_in.
//
// This supervisor-side (restart-surviving) counter closes that gap: it counts
// CONSECUTIVE startup sessions that died on a login banner and, once the budget
// is reached, lets the loop close the bead with the distinct not_logged_in
// reason and stop the pod — deterministically, well before maxRestarts, and
// with the credential-provisioning-failure signal gb audit needs (ka-0000001328).
// Any session that does NOT die on the login banner resets the streak, so a pod
// whose credential eventually arrives (like the sibling agent a1sl, which
// self-healed on its first session) never trips it.

// startupLoginFailureBudget is the number of consecutive credential-not-ready
// (startup login-banner) sessions tolerated before the supervisor closes the
// bead not_logged_in and stops the pod. Chosen to mirror coop's in-session
// LoginRequiredWatchdog threshold (3): a transient credential-switch blip that
// clears within a session or two never trips it, but a genuinely
// unprovisioned credential converges in three short restarts instead of ten.
const startupLoginFailureBudget = 3

// loginFailureTracker counts consecutive startup sessions that died on a
// credential-not-ready login banner. It is the startup-banner analogue of
// coop's in-session watchdog, living on the gb-init side so it survives coop
// restarts (which wipe coop's in-memory counter).
type loginFailureTracker struct {
	budget int
	count  int
}

func newLoginFailureTracker(budget int) *loginFailureTracker {
	return &loginFailureTracker{budget: budget}
}

// Record registers one startup session that died on a login banner and reports
// whether the consecutive-failure budget has now been reached (so the caller
// should close the bead not_logged_in and stop the pod). Always false when the
// budget is <= 0 (disabled).
func (t *loginFailureTracker) Record() bool {
	t.count++
	return t.budget > 0 && t.count >= t.budget
}

// Reset clears the consecutive-failure streak. Called on any session that did
// NOT die on a login banner (productive work, a normal crash, a credential
// switch) so the budget only ever fires on a genuinely SUSTAINED
// credential-not-ready condition.
func (t *loginFailureTracker) Reset() { t.count = 0 }

// Count returns the current consecutive-failure count (for logging/telemetry).
func (t *loginFailureTracker) Count() int { return t.count }
