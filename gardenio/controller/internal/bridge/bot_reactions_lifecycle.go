// bot_reactions_lifecycle.go adds/removes emoji reactions on Slack messages
// to reflect agent lifecycle state transitions.
//
// Thread-bound agents react to the user's originating message (thread parent).
// Card-threaded agents react to the agent status card.
//
// Lifecycle: seen (spawn) → working → done / failed. The emoji names come from
// the channel's card theme (Theme.Reaction): Garden is seedling → leaves →
// deciduous_tree / wilted_flower; Business keeps white_circle →
// hourglass_flowing_sand → white_check_mark / x.
package bridge

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/slack-go/slack"
)

// reactionTarget identifies a Slack message to react on.
type reactionTarget struct {
	Channel   string
	Timestamp string
}

// ReactionLifecycle manages outgoing reaction emoji on Slack messages
// to indicate agent state transitions.
type ReactionLifecycle struct {
	api    *slack.Client
	logger *slog.Logger

	// theme resolves the card theme for a channel (Bot.themeFor in
	// production). Nil means Garden.
	theme func(ctx context.Context, channel string) Theme

	mu      sync.Mutex
	current map[string]string         // agent → current emoji (for swap)
	targets map[string]reactionTarget // agent → message to react on

	// Circuit breaker: stops calling Slack API after detecting missing_scope.
	disabled atomic.Bool
}

// NewReactionLifecycle creates a new reaction lifecycle manager.
func NewReactionLifecycle(api *slack.Client, logger *slog.Logger) *ReactionLifecycle {
	return &ReactionLifecycle{
		api:     api,
		logger:  logger,
		current: make(map[string]string),
		targets: make(map[string]reactionTarget),
	}
}

// SetTarget registers the Slack message that reactions should be placed on
// for the given agent. Must be called before any state transitions.
func (r *ReactionLifecycle) SetTarget(agent string, channel, timestamp string) {
	r.mu.Lock()
	r.targets[agent] = reactionTarget{Channel: channel, Timestamp: timestamp}
	r.mu.Unlock()
}

// TransitionState updates the reaction on the agent's target message
// to reflect the new lifecycle state. Removes the previous reaction
// (if any) and adds the new one.
func (r *ReactionLifecycle) TransitionState(agent, state string) {
	key := stateToReaction(state)
	if key == "" {
		return
	}

	r.mu.Lock()
	target, ok := r.targets[agent]
	r.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Resolve the emoji name from the target channel's theme. With no target
	// there is nothing to react on, but current is still tracked below.
	th := Garden
	if ok && r.theme != nil {
		th = r.theme(ctx, target.Channel)
	}
	emoji := th.Reaction(key)

	r.mu.Lock()
	prev := r.current[agent]
	r.current[agent] = emoji
	// Clean up tracking for terminal states.
	if state == "done" || state == "failed" {
		delete(r.current, agent)
		delete(r.targets, agent)
	}
	r.mu.Unlock()

	if !ok {
		return
	}

	// Circuit breaker: skip API calls if reactions scope is missing.
	if r.disabled.Load() {
		return
	}

	// Remove previous reaction if different.
	if prev != "" && prev != emoji {
		if err := r.api.RemoveReactionContext(ctx, prev, slack.ItemRef{
			Channel:   target.Channel,
			Timestamp: target.Timestamp,
		}); err != nil {
			if r.tripped(err) {
				return
			}
			// Not fatal — reaction may have been manually removed.
			r.logger.Debug("reaction-lifecycle: failed to remove previous reaction",
				"agent", agent, "emoji", prev, "error", err)
		}
	}

	// Add new reaction.
	if err := r.api.AddReactionContext(ctx, emoji, slack.ItemRef{
		Channel:   target.Channel,
		Timestamp: target.Timestamp,
	}); err != nil {
		if r.tripped(err) {
			return
		}
		r.logger.Debug("reaction-lifecycle: failed to add reaction",
			"agent", agent, "emoji", emoji, "error", err)
	} else {
		r.logger.Info("reaction-lifecycle: set reaction",
			"agent", agent, "state", state, "emoji", emoji,
			"channel", target.Channel)
	}
}

// tripped checks if the error is a missing_scope error and trips the
// circuit breaker if so. Returns true if the breaker was tripped.
func (r *ReactionLifecycle) tripped(err error) bool {
	if err == nil {
		return false
	}
	if !strings.Contains(err.Error(), "missing_scope") {
		return false
	}
	if r.disabled.CompareAndSwap(false, true) {
		r.logger.Warn("reaction-lifecycle: reactions:write scope missing — disabling reactions until restart. " +
			"Reinstall the Slack app to grant the scope (Slack App Settings > Install App > Reinstall to Workspace)")
	}
	return true
}

// Cleanup removes any tracked state for an agent without modifying Slack.
// Use when the agent has already been cleaned up elsewhere.
func (r *ReactionLifecycle) Cleanup(agent string) {
	r.mu.Lock()
	delete(r.current, agent)
	delete(r.targets, agent)
	r.mu.Unlock()
}

// stateToReaction maps agent lifecycle states to Theme.Reaction keys.
func stateToReaction(state string) string {
	switch state {
	case "spawning":
		return ReactionSeen
	case "working":
		return ReactionWorking
	case "done":
		return ReactionDone
	case "failed":
		return ReactionFailed
	default:
		return ""
	}
}
