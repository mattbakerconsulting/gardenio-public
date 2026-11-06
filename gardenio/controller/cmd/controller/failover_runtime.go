package main

import (
	"sync"
	"time"
)

// failoverState records that a project's primary credential account has been
// temporarily replaced by a fallback account. Zero value (or no entry at all)
// means the project is running on its primary — not in failover.
//
// The runtime map is the controller-process source of truth. Persistence of
// exhaustion/cooldown metadata lives on the account bead (status=exhausted,
// cooldown_until, exhausted_at) so the runtime map can be reconstructed after
// a controller restart.
type failoverState struct {
	Primary        string    // the account this project is "supposed" to use
	Current        string    // the account currently serving the project
	FailedOverAt   time.Time // first time we moved off Primary in this failover episode
	FailoverReason string    // e.g. "rate_limited"
	LastFailoverAt time.Time // most recent failover decision (for cooldown)
}

// inFailover returns true when this state represents an active failover
// (Current != Primary and both are set).
func (s failoverState) inFailover() bool {
	return s.Current != "" && s.Primary != "" && s.Current != s.Primary
}

// failoverRuntime is an in-memory per-project failover state table. Access is
// concurrency-safe via an RWMutex; callers should not retain pointers to
// failoverState after releasing the lock — the type is copied by value.
type failoverRuntime struct {
	mu    sync.RWMutex
	state map[string]failoverState // project → state
}

func newFailoverRuntime() *failoverRuntime {
	return &failoverRuntime{state: make(map[string]failoverState)}
}

// Get returns the state for a project plus a boolean indicating whether the
// project has any recorded state.
func (r *failoverRuntime) Get(project string) (failoverState, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	s, ok := r.state[project]
	return s, ok
}

// Set stores state for a project (overwriting any existing entry).
func (r *failoverRuntime) Set(project string, s failoverState) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.state[project] = s
}

// ProjectsByCurrent returns projects whose current (fallback) account equals
// the given name.
func (r *failoverRuntime) ProjectsByCurrent(account string) []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []string
	for p, s := range r.state {
		if s.Current == account {
			out = append(out, p)
		}
	}
	return out
}
