package bridge

import (
	"sync"
	"time"
)

// reopenDebounceWindow is the minimum interval between actual agent restarts
// (kill + session-resume remint) for the same agent. Rapid successive reopen
// requests inside this window — a double-clicked "Restart Agent" button, an
// impatient string of `@gardenio restart` mentions — are coalesced into the ONE
// restart the leading request already kicked off, rather than each firing its
// own competing killAgent (a 30s+ graceful shutdown) + remint.
//
// Ten seconds is chosen deliberately: it comfortably covers the human
// double-click / retry window while staying short enough that a genuine,
// deliberately-spaced restart a few beats later is honored. It mirrors the
// 10s hold the respawnThreadAgent spawnInFlight guard already uses for the
// concurrent-reply race (bot_agent_kill.go), so the two coalescing windows on
// the reopen family agree.
const reopenDebounceWindow = 10 * time.Second

// reopenDebouncer coalesces rapid successive agent-restart (reopen) requests so
// only one actual kill+remint fires per window per agent. It is the first-class
// analog of conciergeDebouncer (which debounces concierge button spam), applied
// to the LIFECYCLE-ONLY reopen paths — the "Restart Agent" button and the
// `@gardenio restart` command — where a reopen carries no user message content,
// so coalescing a duplicate loses nothing.
//
// It is intentionally NOT applied to the general follow-up path
// (respawnThreadAgent), which delivers a real user message: that path is
// serialized by the spawnInFlight concurrency guard, and a time-window DROP
// there would silently swallow a distinct follow-up — a worse failure than the
// thrash this guards against.
//
// Leading-edge semantics: the first request in a quiet window fires immediately
// and stamps the clock; every request within reopenDebounceWindow of that stamp
// is coalesced (Allow returns false). This matches conciergeDebouncer.Allow.
type reopenDebouncer struct {
	mu   sync.Mutex
	last map[string]time.Time // key: agent name → last restart time
}

func newReopenDebouncer() *reopenDebouncer {
	return &reopenDebouncer{last: make(map[string]time.Time)}
}

// Allow reports whether a restart should proceed for this agent. It returns
// true (and stamps the clock) when no restart has fired for the agent within
// reopenDebounceWindow; it returns false when a recent restart already covers
// this request, so the caller should coalesce rather than fire a second
// kill+remint. A nil receiver allows unconditionally, so a Bot constructed
// without a debouncer (older test fixtures) keeps its prior behavior.
func (d *reopenDebouncer) Allow(agentName string) bool {
	if d == nil {
		return true
	}
	now := time.Now()
	d.mu.Lock()
	defer d.mu.Unlock()
	if last, ok := d.last[agentName]; ok && now.Sub(last) < reopenDebounceWindow {
		return false
	}
	d.last[agentName] = now
	return true
}

// Cleanup removes entries older than the debounce window to prevent unbounded
// growth of the map as agents come and go. Called periodically by the bot's
// Run loop, alongside conciergeDebouncer.Cleanup.
func (d *reopenDebouncer) Cleanup() {
	if d == nil {
		return
	}
	cutoff := time.Now().Add(-reopenDebounceWindow * 2)
	d.mu.Lock()
	defer d.mu.Unlock()
	for k, v := range d.last {
		if v.Before(cutoff) {
			delete(d.last, k)
		}
	}
}
