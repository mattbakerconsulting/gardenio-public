// Package presence tracks live agent activity for the agent roster.
//
// The PresenceTracker maintains an in-memory map of active agents,
// updated directly by the server when hook events arrive via
// POST /v1/hooks/emit. A background reaper goroutine marks idle
// agents as dead after a configurable threshold.
//
// A land agent's presence comes from its own traffic through the land door
// instead (land_door_presence.go in the server): the door never serves the
// hook routes, so those events carry the land, and its entries are kept apart
// from fleet actors of the same name.
//
// This is the kbeads equivalent of beads' eventbus.PresenceTracker,
// but simplified: instead of subscribing to NATS, the server calls
// RecordHookEvent directly. This avoids an unnecessary NATS round-trip
// since the server already receives all hook events via HTTP.
package presence

import (
	"log/slog"
	"sort"
	"sync"
	"time"
)

// Entry represents a single agent's live presence state.
type Entry struct {
	Actor               string    `json:"actor"`
	Land                string    `json:"land,omitempty"` // the land whose door the agent came through
	LastSeen            time.Time `json:"last_seen"`
	FirstSeen           time.Time `json:"first_seen"`
	LastEvent           string    `json:"last_event"`            // e.g. "PreToolUse", "PostToolUse", "Stop"
	ToolName            string    `json:"tool_name,omitempty"`   // last tool used
	SessionID           string    `json:"session_id,omitempty"`  // Claude Code session
	CWD                 string    `json:"cwd,omitempty"`         // last known working directory
	IdleSecs            float64   `json:"idle_secs"`             // seconds since last event
	EventCount          int64     `json:"event_count"`           // total events seen
	SessionDurationSecs float64   `json:"session_duration_secs"` // seconds since first event
	Reaped              bool      `json:"reaped,omitempty"`      // true if reaper marked dead
	ReapedAt            time.Time `json:"reaped_at,omitempty"`   // when reaped
}

// HookEvent is the data extracted from a hook emit request that the
// tracker needs to update presence state.
type HookEvent struct {
	Actor     string // agent name (from KD_ACTOR or resolved)
	HookType  string // "SessionStart", "Stop", "PreToolUse", "PostToolUse"
	ToolName  string // tool name from Claude Code (e.g. "Bash", "Read")
	SessionID string // Claude Code session ID
	CWD       string // working directory
	// Land is the land whose door the event came through, empty for a fleet
	// hook event. Set only by the server from the door, never from a body.
	Land string
}

// ReaperConfig configures the background dead-agent reaper.
type ReaperConfig struct {
	// DeadThreshold is how long an agent must be idle before being marked dead.
	// Default: 15 minutes.
	DeadThreshold time.Duration

	// EvictAfter is how long after being reaped before an agent is permanently
	// removed from the in-memory map. Prevents unbounded growth from ephemeral agents.
	// Default: 30 minutes.
	EvictAfter time.Duration

	// SweepInterval is how often the reaper scans for dead agents.
	// Default: 60 seconds.
	SweepInterval time.Duration

	// OnDead is called for each agent newly marked as dead.
	// Called outside the lock — safe to make blocking calls.
	OnDead func(actor, sessionID string)
}

// Tracker maintains an in-memory roster of active agents.
type Tracker struct {
	mu      sync.RWMutex
	actors  map[string]*actorState
	started time.Time

	reaperStop chan struct{}
	reaperDone chan struct{}
}

type actorState struct {
	actor      string
	land       string
	firstSeen  time.Time
	lastSeen   time.Time
	lastEvent  string
	toolName   string
	sessionID  string
	cwd        string
	eventCount int64
	reaped     bool
	reapedAt   time.Time
}

// New creates a new presence tracker.
func New() *Tracker {
	return &Tracker{
		actors:  make(map[string]*actorState),
		started: time.Now(),
	}
}

// RecordHookEvent updates the presence state for an agent based on a hook event.
// Called by the server when POST /v1/hooks/emit is received.
func (t *Tracker) RecordHookEvent(ev HookEvent) {
	if ev.Actor == "" {
		return
	}

	now := time.Now()
	t.mu.Lock()
	defer t.mu.Unlock()

	key := actorKey(ev.Land, ev.Actor)
	state, ok := t.actors[key]
	if !ok {
		state = &actorState{actor: ev.Actor, land: ev.Land, firstSeen: now}
		t.actors[key] = state
	}

	// Resurrect reaped agents that come back to life.
	if state.reaped {
		slog.Info("presence: actor resurrected", "actor", ev.Actor)
		state.reaped = false
		state.reapedAt = time.Time{}
	}

	state.lastSeen = now
	state.lastEvent = ev.HookType
	state.eventCount++

	if ev.ToolName != "" {
		state.toolName = ev.ToolName
	}
	if ev.SessionID != "" {
		state.sessionID = ev.SessionID
	}
	if ev.CWD != "" {
		state.cwd = ev.CWD
	}
}

// actorKey keys a land's actor apart from a fleet actor of the same name, so a
// land agent's door traffic never marks a fleet agent alive or tags it with
// the land (and the reverse).
func actorKey(land, actor string) string {
	if land == "" {
		return actor
	}
	return land + "\x00" + actor
}

// Roster returns a snapshot of all tracked agents, sorted by most recently active.
// staleThreshold controls how long since last event before an agent is excluded.
// Pass 0 to include all agents ever seen.
func (t *Tracker) Roster(staleThreshold time.Duration) []Entry {
	t.mu.RLock()
	defer t.mu.RUnlock()

	now := time.Now()
	entries := make([]Entry, 0, len(t.actors))

	for _, state := range t.actors {
		idle := now.Sub(state.lastSeen)
		if staleThreshold > 0 && idle > staleThreshold {
			continue
		}

		firstSeen := state.firstSeen
		if firstSeen.IsZero() {
			firstSeen = t.started
		}
		sessionDur := now.Sub(firstSeen).Seconds()

		entries = append(entries, Entry{
			Actor:               state.actor,
			Land:                state.land,
			LastSeen:            state.lastSeen,
			FirstSeen:           firstSeen,
			LastEvent:           state.lastEvent,
			ToolName:            state.toolName,
			SessionID:           state.sessionID,
			CWD:                 state.cwd,
			IdleSecs:            idle.Seconds(),
			EventCount:          state.eventCount,
			SessionDurationSecs: sessionDur,
			Reaped:              state.reaped,
			ReapedAt:            state.reapedAt,
		})
	}

	// Sort by last seen (most recent first).
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].LastSeen.After(entries[j].LastSeen)
	})

	return entries
}

// StartReaper launches a background goroutine that periodically marks idle
// agents as dead. Call Stop() to shut it down.
func (t *Tracker) StartReaper(cfg *ReaperConfig) {
	if cfg == nil {
		cfg = &ReaperConfig{}
	}
	if cfg.DeadThreshold == 0 {
		cfg.DeadThreshold = 15 * time.Minute
	}
	if cfg.EvictAfter == 0 {
		cfg.EvictAfter = 30 * time.Minute
	}
	if cfg.SweepInterval == 0 {
		cfg.SweepInterval = 60 * time.Second
	}

	t.reaperStop = make(chan struct{})
	t.reaperDone = make(chan struct{})

	go t.reapLoop(cfg)
	slog.Info("presence: reaper started",
		"dead_threshold", cfg.DeadThreshold,
		"sweep_interval", cfg.SweepInterval)
}

// Stop shuts down the reaper goroutine.
func (t *Tracker) Stop() {
	if t.reaperStop != nil {
		close(t.reaperStop)
		<-t.reaperDone
		t.reaperStop = nil
		t.reaperDone = nil
	}
}

func (t *Tracker) reapLoop(cfg *ReaperConfig) {
	defer close(t.reaperDone)

	ticker := time.NewTicker(cfg.SweepInterval)
	defer ticker.Stop()

	for {
		select {
		case <-t.reaperStop:
			return
		case <-ticker.C:
			t.sweep(cfg)
		}
	}
}

func (t *Tracker) sweep(cfg *ReaperConfig) {
	now := time.Now()

	type deadActor struct {
		name      string
		sessionID string
	}
	var newlyDead []deadActor

	t.mu.Lock()
	for key, state := range t.actors {
		if state.reaped {
			// Evict agents reaped for longer than EvictAfter.
			// Low-event agents (<10 events) are likely ephemeral — evict faster (5 min).
			evictThreshold := cfg.EvictAfter
			if state.eventCount < 10 {
				evictThreshold = 5 * time.Minute
			}
			if !state.reapedAt.IsZero() && now.Sub(state.reapedAt) > evictThreshold {
				delete(t.actors, key)
			}
			continue
		}
		idle := now.Sub(state.lastSeen)
		if idle > cfg.DeadThreshold {
			state.reaped = true
			state.reapedAt = now
			newlyDead = append(newlyDead, deadActor{name: state.actor, sessionID: state.sessionID})
		}
	}
	t.mu.Unlock()

	for _, dead := range newlyDead {
		slog.Info("presence: reaper marked agent dead",
			"actor", dead.name,
			"threshold", cfg.DeadThreshold)
		if cfg.OnDead != nil {
			cfg.OnDead(dead.name, dead.sessionID)
		}
	}
}
