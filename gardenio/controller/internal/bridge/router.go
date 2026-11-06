// Router resolves Slack channels for agent identities using pattern matching.
//
// Resolution priority:
//  1. Exact override (break-out channel for specific agent)
//  2. Pattern match (wildcard patterns sorted by specificity)
//  3. Default channel
//
// Agent identity format: "project/role/name" (e.g., "gardenio/crew/test-bot")
// Patterns support wildcards: "*" matches any single segment.
package bridge

import (
	"strings"
	"sync"
)

// RouterConfig holds channel routing configuration.
type RouterConfig struct {
	// DefaultChannel is the fallback channel when no pattern matches.
	DefaultChannel string `json:"default_channel"`

	// Channels maps agent patterns to Slack channel IDs.
	// Patterns support wildcards: "*" matches any single segment.
	// Examples:
	//   "gardenio/crew/*"  → all crew agents in gardenio
	//   "*/polecats/*"    → all polecats across projects
	//   "beads/*"         → all agents in beads project
	Channels map[string]string `json:"channels,omitempty"`

	// Overrides maps exact agent identities to dedicated channel IDs.
	// These take precedence over pattern matching. Created via "Break Out" button.
	Overrides map[string]string `json:"overrides,omitempty"`
}

// Router resolves Slack channels for agent identities.
type Router struct {
	mu       sync.RWMutex
	config   RouterConfig
	patterns []compiledPattern
}

// compiledPattern is a pre-processed pattern for faster matching.
type compiledPattern struct {
	original string   // Original pattern string
	segments []string // Pattern split by "/"
	channel  string   // Target channel ID
}

// RouteResult contains the resolved channel information.
type RouteResult struct {
	ChannelID string // Slack channel ID
	MatchedBy string // Pattern that matched (for logging)
	IsDefault bool   // True if using default channel
}

// NewRouter creates a new channel router from config.
func NewRouter(cfg RouterConfig) *Router {
	r := &Router{config: cfg}
	r.compilePatterns()
	return r
}

// Resolve finds the appropriate Slack channel for an agent identity.
// Agent format: "project/role/name" (e.g., "gardenio/crew/test-bot")
// Priority: 1. Exact override, 2. Pattern match, 3. Default channel
func (r *Router) Resolve(agent string) RouteResult {
	r.mu.RLock()
	defer r.mu.RUnlock()

	// Check overrides first (exact agent match, highest priority).
	if r.config.Overrides != nil {
		if ch, ok := r.config.Overrides[agent]; ok {
			return RouteResult{ChannelID: ch, MatchedBy: "(override)"}
		}
	}

	// Try each pattern in priority order.
	agentSegments := strings.Split(agent, "/")
	for _, p := range r.patterns {
		if matchPattern(p.segments, agentSegments) {
			return RouteResult{ChannelID: p.channel, MatchedBy: p.original}
		}
	}

	// Fall back to default channel.
	return RouteResult{
		ChannelID: r.config.DefaultChannel,
		MatchedBy: "(default)",
		IsDefault: true,
	}
}

// ResolveIdentity resolves an agent the bridge knows by its short name, with
// the project and role cached for it. Patterns are written against the full
// "project/role/name" identity, so with both known it tries an override on
// the full identity, then an override on the short name (the key a break-out
// channel was stored under), then the patterns against the full identity.
// With either unknown, or a name that is already a path, it is Resolve(name),
// so a short name then matches only a short-name override or a one-segment
// pattern (gd-H74pBA3OyK).
func (r *Router) ResolveIdentity(project, role, name string) RouteResult {
	if project == "" || role == "" || strings.Contains(name, "/") {
		return r.Resolve(name)
	}
	r.mu.RLock()
	ch, ok := r.config.Overrides[name]
	r.mu.RUnlock()
	identity := project + "/" + role + "/" + name
	res := r.Resolve(identity)
	if ok && res.MatchedBy != "(override)" {
		return RouteResult{ChannelID: ch, MatchedBy: "(override)"}
	}
	return res
}

// HasOverride returns true if the agent has a dedicated channel override.
func (r *Router) HasOverride(agent string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.config.Overrides == nil {
		return false
	}
	_, ok := r.config.Overrides[agent]
	return ok
}

// AddOverride sets a dedicated channel override for an agent.
func (r *Router) AddOverride(agent, channelID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.config.Overrides == nil {
		r.config.Overrides = make(map[string]string)
	}
	r.config.Overrides[agent] = channelID
}

// RemoveOverride removes a dedicated channel override for an agent.
func (r *Router) RemoveOverride(agent string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.config.Overrides != nil {
		delete(r.config.Overrides, agent)
	}
}

// GetAgentByChannel returns the agent for a channel ID (reverse override lookup).
func (r *Router) GetAgentByChannel(channelID string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for agent, ch := range r.config.Overrides {
		if ch == channelID {
			return agent
		}
	}
	return ""
}

// compilePatterns pre-processes patterns for efficient matching.
// Patterns are sorted by specificity: more segments > fewer wildcards > alphabetical.
func (r *Router) compilePatterns() {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.patterns = make([]compiledPattern, 0, len(r.config.Channels))
	for pattern, channel := range r.config.Channels {
		r.patterns = append(r.patterns, compiledPattern{
			original: pattern,
			segments: strings.Split(pattern, "/"),
			channel:  channel,
		})
	}

	// Sort by specificity (bubble sort — small N).
	for i := 0; i < len(r.patterns); i++ {
		for j := i + 1; j < len(r.patterns); j++ {
			if patternMoreSpecific(r.patterns[j], r.patterns[i]) {
				r.patterns[i], r.patterns[j] = r.patterns[j], r.patterns[i]
			}
		}
	}
}

// patternMoreSpecific returns true if a should be matched before b (higher priority).
// Priority order: 1. More segments, 2. Fewer wildcards, 3. Alphabetical.
func patternMoreSpecific(a, b compiledPattern) bool {
	if len(a.segments) != len(b.segments) {
		return len(a.segments) > len(b.segments)
	}
	aw := countWildcards(a.segments)
	bw := countWildcards(b.segments)
	if aw != bw {
		return aw < bw
	}
	return a.original < b.original
}

// countWildcards counts "*" segments in a pattern.
func countWildcards(segments []string) int {
	n := 0
	for _, s := range segments {
		if s == "*" {
			n++
		}
	}
	return n
}

// matchPattern checks if an agent matches a pattern.
// Segments must be the same length. "*" matches any value.
func matchPattern(pattern, agent []string) bool {
	if len(pattern) != len(agent) {
		return false
	}
	for i, p := range pattern {
		if p != "*" && p != agent[i] {
			return false
		}
	}
	return true
}
