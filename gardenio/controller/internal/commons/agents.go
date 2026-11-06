package commons

import (
	"context"
	"strings"
	"sync"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// AgentReader is the slice of the daemon client Agents reads through.
// *beadsapi.Client satisfies it.
type AgentReader interface {
	GetAgentRoster(ctx context.Context, staleThresholdSecs int) (*beadsapi.AgentRosterResponse, error)
	ListBeadsFiltered(ctx context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error)
}

// Agents is the production AgentSource: the kbeads presence tracker's roster
// (kbeads/internal/presence), joined by name to the open agent beads. An
// agent's land is its bead's one project label; its channel is where its card
// is placed (Place, the shared card placement of spec section 2.5). Every read
// is as the system: the answer is cut per viewer by land before anyone sees it.
type Agents struct {
	Daemon AgentReader
	// Place returns the channel bead id an agent's card is placed in, "" when
	// the land has no channel for it. beads-viewer passes conversation.Place.
	// Nil places every agent in its land only.
	Place func(ctx context.Context, agent *beadsapi.BeadDetail) (string, error)

	mu   sync.Mutex
	memo map[string]placeMemo // agent bead id -> its channel
}

type placeMemo struct {
	channel string
	expires time.Time
}

const (
	// agentIdle is how long since its last hook event an agent still counts as
	// present: a working agent calls tools every few seconds.
	agentIdle = 10 * time.Minute
	// agentBeadsMax caps the open agent beads one read pulls.
	agentBeadsMax = 2000
	// placeTTL is how long an agent's placement is reused; a thread binding
	// or a channel_roles change shows within it.
	placeTTL = time.Minute
)

// Spots reads where each live agent is. It is an AgentSource.
func (a *Agents) Spots(ctx context.Context) (map[string][]Spot, error) {
	ctx = beadsapi.WithSystemRead(ctx)
	roster, err := a.Daemon.GetAgentRoster(ctx, int(agentIdle/time.Second))
	if err != nil {
		return nil, err
	}
	live := map[string]bool{}
	for _, r := range roster.Actors {
		if name := strings.TrimSpace(r.Actor); name != "" && !r.Reaped && r.IdleSecs <= agentIdle.Seconds() {
			live[name] = true
		}
	}
	out := map[string][]Spot{}
	if len(live) == 0 {
		return out, nil
	}
	res, err := a.Daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"agent"},
		Statuses: []string{"open", "in_progress"},
		Limit:    agentBeadsMax,
	})
	if err != nil {
		return nil, err
	}
	now := time.Now()
	for _, b := range res.Beads {
		if b == nil {
			continue
		}
		name := strings.TrimSpace(b.Fields["agent"])
		if name == "" {
			name = strings.TrimSpace(b.Title)
		}
		land := landOf(b.Labels)
		if !live[name] || land == "" {
			continue
		}
		who := "agent:" + name
		out[who] = append(out[who], Spot{Land: land})
		if ch := a.channel(ctx, b, now); ch != "" {
			out[who] = append(out[who], Spot{Land: land, Channel: ch})
		}
	}
	return out, nil
}

// channel returns the agent's placed channel, memoized for placeTTL. A failed
// placement is memoized as "" too, so a land with no channels costs one
// placement a minute per agent, not one per poll.
func (a *Agents) channel(ctx context.Context, b *beadsapi.BeadDetail, now time.Time) string {
	if a.Place == nil {
		return ""
	}
	a.mu.Lock()
	m, hit := a.memo[b.ID]
	a.mu.Unlock()
	if hit && now.Before(m.expires) {
		return m.channel
	}
	ch, err := a.Place(ctx, b)
	if err != nil {
		ch = ""
	}
	a.mu.Lock()
	if a.memo == nil || len(a.memo) >= landMemoCap {
		a.memo = map[string]placeMemo{}
	}
	a.memo[b.ID] = placeMemo{channel: ch, expires: now.Add(placeTTL)}
	a.mu.Unlock()
	return ch
}
