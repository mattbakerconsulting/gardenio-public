package main

// init_agent_switch_backoff.go — durable anti-flap backoff for in-session
// credential rotation.
//
// Why this exists (ka-0000005014): coop has its own anti-flap guard (a per-
// account cooldown + a switches-per-hour cap in profile.rs), but that state is
// in-memory and a credential switch is an exit(10) + restart — so every
// rotation WIPES coop's flap history. The cap therefore cannot bound the very
// thing it is meant to bound: a pod can rotate A→B→A→… faster than intended if
// each restarted coop starts with a fresh, empty switch history.
//
// gb init is the supervisor that survives those restarts, so the durable
// backoff belongs here. We count credential-switch restarts in a sliding
// window and, when they arrive faster than a threshold, sleep with escalating
// backoff before relaunching coop. This caps the rotation rate across restarts
// without blocking a single, legitimate failover.

import "time"

const (
	// switchFlapThreshold is the number of credential switches inside
	// switchFlapWindow that must occur before backoff kicks in. Below this,
	// switches restart immediately (the common single-failover case).
	switchFlapThreshold = 3
	// switchFlapWindow is the sliding window over which rapid switches are
	// counted.
	switchFlapWindow = 10 * time.Minute
	// switchBackoffBase is the first backoff applied once the threshold is
	// crossed; it doubles per excess switch up to switchBackoffMax.
	switchBackoffBase = 5 * time.Second
	// switchBackoffMax caps the per-switch backoff so a wedged account never
	// stalls the pod for longer than this between attempts.
	switchBackoffMax = 2 * time.Minute
	// switchTerminalCap is the number of NON-PRODUCTIVE credential-switch
	// restarts (short coop sessions that exit-10 without doing sustained work)
	// tolerated before the flap is declared TERMINAL. It is defense-in-depth
	// for NOVEL / unclassified 429 shapes that still classify as an
	// auth-exhaustion category and drive try_auto_rotate → exit-10 in a tight
	// loop despite the escalating backoff above (the known model-group 429 is
	// already routed to the non-rotating ErrorCategory::ModelRateLimited by
	// coop — see ka-0000001718). Above this cap gb init stops the tight
	// exit-10 → fresh-session-bead churn (~one new session every ~277s,
	// resumed=False) and parks: it relaunches coop ONCE with rotation disabled
	// so coop stays up and the 429 is handled IN PLACE by monitorExit
	// (dismiss + backoff + re-nudge, same session) instead of cold-restarting
	// forever. The counter resets on a productive session (see Reset), so this
	// is a self-healing circuit breaker, not a blind restarts=0-forever park.
	// 0 disables the terminal cap. Chosen well above switchFlapThreshold so a
	// legitimate multi-account failover burst never trips it.
	switchTerminalCap = 6
)

// switchTracker records credential-switch restart times in a sliding window
// and computes an escalating backoff once switches come too fast. It is the
// gb-init-side (restart-surviving) complement to coop's in-memory anti-flap.
//
// On top of the sliding-window backoff it maintains a monotonic
// "switches since the last productive session" counter that drives a bounded
// TERMINAL cap (see switchTerminalCap): once the flap has clearly not resolved,
// the supervisor parks instead of looping forever. The counter is reset by
// [Reset] on a productive session.
type switchTracker struct {
	threshold   int
	window      time.Duration
	base        time.Duration
	max         time.Duration
	terminalCap int
	now         func() time.Time
	switches    []time.Time
	// sinceProductive counts credential switches since the last productive
	// session. It is NOT window-pruned — it accumulates across the whole flap
	// and is cleared only by [Reset] (a productive session), so a sustained but
	// slow flap (each cycle spaced beyond switchFlapWindow) still trips the
	// terminal cap rather than resetting itself via time alone.
	sinceProductive int
}

func newSwitchTracker(threshold int, window, base, max time.Duration, terminalCap int) *switchTracker {
	return &switchTracker{
		threshold:       threshold,
		window:          window,
		base:            base,
		max:             max,
		terminalCap:     terminalCap,
		now:             time.Now,
		switches:        nil,
		sinceProductive: 0,
	}
}

// Record registers a credential-switch restart at the current time, prunes the
// window, and returns the backoff to sleep before relaunching coop. The first
// `threshold` switches in a window return 0 (restart immediately); each switch
// beyond the threshold returns base*2^(excess-1), capped at max.
//
// It also increments the monotonic since-productive counter that feeds
// [TrippedTerminal].
func (s *switchTracker) Record() time.Duration {
	now := s.now()
	cutoff := now.Add(-s.window)
	pruned := s.switches[:0]
	for _, t := range s.switches {
		if t.After(cutoff) {
			pruned = append(pruned, t)
		}
	}
	s.switches = append(pruned, now)
	s.sinceProductive++

	excess := len(s.switches) - s.threshold
	if excess <= 0 {
		return 0
	}
	// Escalate: base, 2*base, 4*base, … capped at max.
	backoff := s.base
	for i := 1; i < excess; i++ {
		backoff *= 2
		if backoff >= s.max {
			return s.max
		}
	}
	if backoff > s.max {
		backoff = s.max
	}
	return backoff
}

// TrippedTerminal reports whether the number of credential switches since the
// last productive session has reached the terminal cap — i.e. the escalating
// backoff has failed to break the flap and the supervisor should park (relaunch
// coop once with rotation disabled) instead of continuing the tight exit-10 →
// fresh-session churn. Always false when terminalCap <= 0 (cap disabled).
func (s *switchTracker) TrippedTerminal() bool {
	return s.terminalCap > 0 && s.sinceProductive >= s.terminalCap
}

// SinceProductive returns the current count of credential switches since the
// last productive session (for logging / telemetry).
func (s *switchTracker) SinceProductive() int {
	return s.sinceProductive
}

// Reset clears the flap accounting after a productive coop session: it zeroes
// both the sliding-window switch history (so backoff restarts from base) and
// the since-productive counter (so the terminal cap re-arms). This is the
// "reset on a productive session" semantic that keeps the terminal cap a
// self-healing circuit breaker rather than a permanent one-shot park.
func (s *switchTracker) Reset() {
	s.switches = nil
	s.sinceProductive = 0
}
