// Package bridge provides the squawk message watcher.
//
// Squawk watches the kbeads SSE event stream for closed message beads with the
// "squawk" (or "say") label and projects them into the originating agent's
// thread through the conversation Router.
// This gives agents a simple way to post informational updates without
// knowing anything about Slack.
package bridge

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"gardenio/controller/internal/conversation"
)

// SquawkConfig holds configuration for the Squawk watcher.
type SquawkConfig struct {
	Daemon BeadClient
	Bot    *Bot
	Logger *slog.Logger
}

// Squawk watches for closed message beads and relays them to Slack.
type Squawk struct {
	daemon BeadClient
	bot    *Bot
	logger *slog.Logger

	mu   sync.Mutex
	seen map[string]bool // dedup on SSE reconnect
}

// NewSquawk creates a new squawk message watcher.
func NewSquawk(cfg SquawkConfig) *Squawk {
	return &Squawk{
		daemon: cfg.Daemon,
		bot:    cfg.Bot,
		logger: cfg.Logger,
		seen:   make(map[string]bool),
	}
}

// RegisterHandlers registers SSE event handlers on the given stream for
// message bead closed events.
func (s *Squawk) RegisterHandlers(stream *SSEStream) {
	stream.On("beads.bead.closed", s.handleClosed)
	s.logger.Info("squawk watcher registered SSE handlers",
		"topics", []string{"beads.bead.closed"})
}

func (s *Squawk) handleClosed(ctx context.Context, data []byte) {
	bead := ParseBeadEvent(data)
	if bead == nil {
		return
	}

	// Only handle message beads with the "squawk" label.
	// Also accept legacy "say" label for backward compatibility.
	if bead.Type != "message" || (!hasLabel(bead.Labels, "squawk") && !hasLabel(bead.Labels, "say")) {
		return
	}

	// Dedup on SSE reconnect.
	if s.alreadySeen(bead.ID) {
		return
	}

	// Extract the source agent from labels or fields.
	agent := extractSquawkAgent(*bead)
	if agent == "" {
		s.logger.Warn("squawk bead has no source agent",
			"id", bead.ID)
		return
	}

	// Get the message text from fields (falls back to title).
	text := bead.Fields["text"]
	if text == "" {
		text = bead.Title
	}

	s.logger.Info("squawk message received",
		"id", bead.ID,
		"agent", agent,
		"text_length", len(text))

	// Format and post to the agent's Slack thread.
	if s.bot != nil && !s.bot.notifConfig.SuppressSquawk {
		agentLink := s.bot.agentThreadLink(agent)
		formatted := fmt.Sprintf(":speech_balloon: %s: %s", agentLink, text)

		// For thread-bound agents, update the spawn confirmation message
		// in-place with the first squawk instead of posting a separate reply.
		// Otherwise the squawk bead is projected into the agent's thread
		// through the conversation Router, which stamps the Slack ref back
		// on it.
		if !s.bot.tryUpdateSpawnMessage(ctx, agent, formatted) {
			s.bot.routeAgentThreadMessage(ctx, agent, s.bot.landOf(bead.Labels, agent), conversation.Message{
				BeadID: bead.ID,
				Text:   formatted,
				Author: agentPrincipal(agent),
			})
		}
	}
}

// extractSquawkAgent extracts the source agent name from a squawk bead.
// Checks fields first, then falls back to from: label.
func extractSquawkAgent(bead BeadEvent) string {
	if agent := bead.Fields["source_agent"]; agent != "" {
		return extractAgentName(agent)
	}
	for _, label := range bead.Labels {
		if strings.HasPrefix(label, "from:") {
			return extractAgentName(strings.TrimPrefix(label, "from:"))
		}
	}
	return extractAgentName(bead.CreatedBy)
}

// alreadySeen returns true if this bead has already been processed.
func (s *Squawk) alreadySeen(beadID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.seen[beadID] {
		return true
	}
	s.seen[beadID] = true
	return false
}
