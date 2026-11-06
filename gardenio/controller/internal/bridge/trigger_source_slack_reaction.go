package bridge

import (
	"context"
	"log/slog"
	"strings"
	"sync"

	"gardenio/controller/internal/beadsapi"
)

// SlackReactionTriggerSource implements TriggerSource for Slack reaction triggers.
// It is event-driven: the Bot pushes reaction events via BufferEvent, and Evaluate
// matches them against loaded trigger definitions.
//
// This source also loads legacy "reaction-rule" beads and converts them to the
// unified trigger format, providing a migration path from the standalone reaction
// handler to the shared trigger engine.
type SlackReactionTriggerSource struct {
	logger *slog.Logger

	// skipLegacy, when true, makes LoadTriggers skip the legacy reaction-rule
	// auto-conversion. Set when the source runs inside the slack-bridge Bot,
	// which still serves legacy reaction-rule beads via bot_reactions.go — so
	// the engine must handle only new-style trigger:slack-reaction beads to
	// avoid double-spawning the same legacy rule.
	skipLegacy bool

	mu      sync.Mutex
	pending []slackReactionEvent // buffered events waiting for evaluation

	// notify is signaled (non-blocking) whenever a new event is buffered, so
	// an eval loop can evaluate immediately instead of waiting for its tick.
	notify chan struct{}
}

// slackReactionEvent is a buffered Slack reaction event with pre-fetched context.
type slackReactionEvent struct {
	User        string
	Emoji       string
	Channel     string
	MessageTS   string
	MessageText string
}

// slackReactionTriggerData holds reaction-specific fields parsed from a trigger bead.
type slackReactionTriggerData struct {
	// Emoji is the emoji name to match (without colons, e.g. "runner").
	Emoji string
	// Match is an optional substring that must appear in the reacted-to message.
	Match string
	// ChannelFilter is an optional channel ID to restrict matching to.
	ChannelFilter string
	// Legacy indicates this was converted from a reaction-rule bead.
	Legacy bool
}

// NewSlackReactionTriggerSource creates a new Slack reaction trigger source.
func NewSlackReactionTriggerSource(logger *slog.Logger) *SlackReactionTriggerSource {
	if logger == nil {
		logger = slog.Default()
	}
	return &SlackReactionTriggerSource{
		logger: logger,
		notify: make(chan struct{}, 1),
	}
}

// SkipLegacy disables loading + auto-converting legacy reaction-rule beads.
// The slack-bridge Bot calls this because it serves legacy reaction-rule beads
// through its own bot_reactions.go path; leaving auto-conversion on would let
// the engine spawn a second agent for the same legacy rule. Returns the source
// for fluent construction.
func (s *SlackReactionTriggerSource) SkipLegacy() *SlackReactionTriggerSource {
	s.skipLegacy = true
	return s
}

// Notify returns a channel that receives a signal whenever a new reaction
// event is buffered. An eval loop selects on it to evaluate immediately.
func (s *SlackReactionTriggerSource) Notify() <-chan struct{} {
	return s.notify
}

func (s *SlackReactionTriggerSource) Name() string { return "slack-reaction" }

// SkipRuntimeBootstrap opts this source out of the engine's runtime-bootstrap
// guard. Slack reactions are event-driven: Evaluate drains a buffer of reaction
// events that have arrived since the last call — every match is a genuinely
// fresh event, not historical state. Suppressing the first eval would drop
// legitimate reactions that arrived right after a trigger was created.
func (s *SlackReactionTriggerSource) SkipRuntimeBootstrap() bool { return true }

// BufferEvent adds a Slack reaction event to the pending buffer.
// Called by the Bot when a reaction_added event is received.
// The message text should be pre-fetched before calling this method.
func (s *SlackReactionTriggerSource) BufferEvent(evt slackReactionEvent) {
	s.mu.Lock()
	s.pending = append(s.pending, evt)
	s.mu.Unlock()

	// Signal an eval loop (if any) to evaluate immediately. Non-blocking: the
	// buffered channel coalesces bursts into a single pending wake-up.
	if s.notify != nil {
		select {
		case s.notify <- struct{}{}:
		default:
		}
	}
}

// BufferReaction is an exported wrapper around BufferEvent for out-of-package
// callers (the trigger-probe e2e driver) that need to inject a synthetic
// reaction_added event without access to the unexported slackReactionEvent
// type. Production code keeps using BufferEvent directly from inside the Bot.
func (s *SlackReactionTriggerSource) BufferReaction(user, emoji, channel, messageTS, messageText string) {
	s.BufferEvent(slackReactionEvent{
		User:        user,
		Emoji:       emoji,
		Channel:     channel,
		MessageTS:   messageTS,
		MessageText: messageText,
	})
}

// DrainPending clears the pending event buffer. Call after all triggers
// have been evaluated in a cycle to prevent unbounded growth.
//
// Prefer DrainFirst in the eval loop: DrainPending blind-clears the WHOLE
// buffer, so any reaction appended by a concurrent BufferEvent after Evaluate
// snapshotted s.pending but before this call is discarded without ever being
// matched. DrainPending remains for the one caller that drains a buffer it
// knows is quiescent.
func (s *SlackReactionTriggerSource) DrainPending() {
	s.mu.Lock()
	s.pending = s.pending[:0]
	s.mu.Unlock()
}

// DrainFirst removes exactly the first n buffered events — the prefix an
// eval cycle actually evaluated (snapshot PendingCount() BEFORE evaluating,
// pass it here AFTER). s.pending is append-only FIFO, so dropping the first n
// removes the evaluated events while preserving any tail appended concurrently
// during evaluation. Those tail events stay buffered for the next cycle (the
// coalesced Notify signal from their BufferEvent re-wakes the loop), so a
// reaction that lands mid-cycle is evaluated next pass instead of being
// silently dropped. n<=0 is a no-op; n>=len clears the whole buffer.
func (s *SlackReactionTriggerSource) DrainFirst(n int) {
	if n <= 0 {
		return
	}
	s.mu.Lock()
	if n >= len(s.pending) {
		s.pending = s.pending[:0]
	} else {
		// Copy the tail down rather than reslicing, so the drained head does
		// not pin backing-array memory and len shrinks to the tail size.
		s.pending = append(s.pending[:0], s.pending[n:]...)
	}
	s.mu.Unlock()
}

// PendingCount returns the number of buffered events (for testing/monitoring).
func (s *SlackReactionTriggerSource) PendingCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.pending)
}

// LoadTriggers fetches both new-style trigger:slack-reaction beads and legacy
// reaction-rule beads, converting both into unified TriggerDef format.
func (s *SlackReactionTriggerSource) LoadTriggers(ctx context.Context, daemon TriggerBeadClient) ([]TriggerDef, error) {
	var triggers []TriggerDef

	// Load new-style trigger beads with label "trigger:slack-reaction".
	newResult, err := daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"trigger"},
		Statuses: []string{"open"},
		Labels:   []string{"trigger:slack-reaction"},
		Limit:    100,
	})
	if err != nil {
		return nil, err
	}

	for _, bead := range newResult.Beads {
		if bead.Fields["enabled"] == "false" {
			continue
		}
		emoji := bead.Fields["emoji"]
		if emoji == "" {
			s.logger.Warn("slack-reaction trigger: missing emoji, skipping",
				"trigger", bead.ID)
			continue
		}
		t := ParseTriggerDef(bead, "slack-reaction")
		t.SourceData = slackReactionTriggerData{
			Emoji:         emoji,
			Match:         bead.Fields["match"],
			ChannelFilter: bead.Fields["channel_filter"],
		}
		triggers = append(triggers, t)
	}

	// Load legacy reaction-rule beads and auto-convert — unless skipLegacy is
	// set (the slack-bridge Bot serves those rules via its own bot_reactions.go
	// path, so converting them here would double-spawn).
	if s.skipLegacy {
		return triggers, nil
	}

	legacyResult, err := daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"reaction-rule"},
		Statuses: []string{"open"},
		Limit:    100,
	})
	if err != nil {
		s.logger.Warn("slack-reaction trigger: failed to load legacy reaction-rule beads",
			"error", err)
		return triggers, nil // return what we have from new-style triggers
	}

	for _, bead := range legacyResult.Beads {
		emoji := bead.Fields["emoji"]
		if emoji == "" {
			continue
		}
		t := convertLegacyReactionRule(bead)
		triggers = append(triggers, t)
	}

	return triggers, nil
}

// convertLegacyReactionRule converts a legacy reaction-rule bead to a TriggerDef.
func convertLegacyReactionRule(bead *beadsapi.BeadDetail) TriggerDef {
	role := bead.Fields["role"]
	if role == "" {
		role = "crew"
	}
	return TriggerDef{
		ID:            bead.ID,
		Title:         bead.Title,
		Source:        "slack-reaction",
		Action:        TriggerActionSpawnAgent,
		Role:          role,
		Project:       bead.Fields["project"],
		Prompt:        convertLegacyPromptTemplate(bead.Fields["prompt"]),
		MaxConcurrent: 3,
		Enabled:       true,
		SourceData: slackReactionTriggerData{
			Emoji:  bead.Fields["emoji"],
			Match:  bead.Fields["match"],
			Legacy: true,
		},
	}
}

// convertLegacyPromptTemplate converts {placeholder} syntax from legacy
// reaction rules to {{placeholder}} syntax used by the trigger engine.
func convertLegacyPromptTemplate(prompt string) string {
	if prompt == "" {
		return ""
	}
	// Replace single-brace {var} with double-brace {{var}} for ExpandTemplate.
	// Only convert known reaction-specific variables.
	for _, v := range []string{
		"channel", "thread_ts", "message_ts", "user",
		"emoji", "message_text", "mr_iid", "source_branch",
	} {
		prompt = strings.ReplaceAll(prompt, "{"+v+"}", "{{"+v+"}}")
	}
	return prompt
}

// Evaluate drains pending reaction events and returns matches for the given trigger.
func (s *SlackReactionTriggerSource) Evaluate(_ context.Context, trig TriggerDef) ([]TriggerMatch, error) {
	data, ok := trig.SourceData.(slackReactionTriggerData)
	if !ok {
		return nil, nil
	}

	// Read pending events (don't clear — other triggers need them too).
	s.mu.Lock()
	events := make([]slackReactionEvent, len(s.pending))
	copy(events, s.pending)
	s.mu.Unlock()

	var matches []TriggerMatch
	for _, evt := range events {
		if !matchReactionEvent(data, evt) {
			continue
		}
		matches = append(matches, reactionEventToMatch(evt))
	}
	return matches, nil
}

// matchReactionEvent checks if a buffered reaction event matches the trigger criteria.
func matchReactionEvent(data slackReactionTriggerData, evt slackReactionEvent) bool {
	// Emoji must match.
	if evt.Emoji != data.Emoji {
		return false
	}
	// Channel filter (optional).
	if data.ChannelFilter != "" && evt.Channel != data.ChannelFilter {
		return false
	}
	// Message text pattern (optional).
	if data.Match != "" && !strings.Contains(evt.MessageText, data.Match) {
		return false
	}
	return true
}

// reactionEventToMatch converts a reaction event to a TriggerMatch with template vars.
func reactionEventToMatch(evt slackReactionEvent) TriggerMatch {
	// Dedup key: unique per emoji + channel + message.
	key := "reaction:" + evt.Emoji + ":" + evt.Channel + ":" + evt.MessageTS

	vars := map[string]string{
		"{{emoji}}":        evt.Emoji,
		"{{channel}}":      evt.Channel,
		"{{message_ts}}":   evt.MessageTS,
		"{{thread_ts}}":    evt.MessageTS,
		"{{user}}":         evt.User,
		"{{message_text}}": evt.MessageText,
	}

	// Extract MR IID from message text.
	if m := mrIIDPattern.FindStringSubmatch(evt.MessageText); len(m) > 1 {
		vars["{{mr_iid}}"] = m[1]
	}
	// Extract branch name from message text.
	if m := branchPattern.FindStringSubmatch(evt.MessageText); len(m) > 1 {
		vars["{{source_branch}}"] = m[1]
	}

	fields := map[string]string{
		"reaction_key":     key,
		"reaction_emoji":   evt.Emoji,
		"reaction_channel": evt.Channel,
		"reaction_ts":      evt.MessageTS,
		"reaction_user":    evt.User,
	}

	return TriggerMatch{
		Key:          key,
		Title:        "Reaction :" + evt.Emoji + ": on message",
		Fields:       fields,
		Labels:       []string{"source:slack-reaction"},
		TemplateVars: vars,
	}
}

// CatchUpKeys returns dedup keys for existing beads created by this trigger.
func (s *SlackReactionTriggerSource) CatchUpKeys(ctx context.Context, daemon TriggerBeadClient, triggerID string) ([]string, error) {
	result, err := daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Kinds:    []string{"issue"},
		Statuses: []string{"open", "in_progress", "closed"},
		Labels:   []string{"trigger:" + triggerID},
		Limit:    500,
	})
	if err != nil {
		return nil, err
	}

	var keys []string
	for _, bead := range result.Beads {
		if key := bead.Fields["reaction_key"]; key != "" {
			keys = append(keys, key)
		}
	}
	return keys, nil
}
