package hooks

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
	"gardenio/kbeads/internal/store"
)

// Trigger constants match advice hook_trigger field values.
const (
	TriggerSessionEnd    = "session-end"
	TriggerBeforeCommit  = "before-commit"
	TriggerBeforePush    = "before-push"
	TriggerBeforeHandoff = "before-handoff"
)

// OnFailure constants match advice hook_on_failure field values.
const (
	OnFailureBlock  = "block"
	OnFailureWarn   = "warn"
	OnFailureIgnore = "ignore"
)

// SessionEvent is the payload published when a session lifecycle event occurs.
// Agents emit these via the bus (e.g., NATS topic "beads.session.end").
type SessionEvent struct {
	AgentID string `json:"agent_id"`
	Trigger string `json:"trigger"`
	CWD     string `json:"cwd,omitempty"`
}

// HookResponse is returned from HandleSessionEvent with the aggregated result.
type HookResponse struct {
	Block    bool     `json:"block,omitempty"`
	Reason   string   `json:"reason,omitempty"`
	Warnings []string `json:"warnings,omitempty"`
}

// adviceFields extracts hook fields from a bead's Fields JSON.
type adviceFields struct {
	HookCommand          string   `json:"hook_command"`
	HookTrigger          string   `json:"hook_trigger"`
	HookTimeout          int      `json:"hook_timeout"`
	HookOnFailure        string   `json:"hook_on_failure"`
	Subscriptions        []string `json:"subscriptions"`
	SubscriptionsExclude []string `json:"subscriptions_exclude"`
}

func parseAdviceFields(raw json.RawMessage) (adviceFields, error) {
	var f adviceFields
	if len(raw) == 0 {
		return f, nil
	}
	err := json.Unmarshal(raw, &f)
	return f, err
}

// Handler processes session lifecycle events and executes matching advice hooks.
type Handler struct {
	store  store.Store
	logger *slog.Logger
}

// NewHandler creates a hook handler backed by the given store.
func NewHandler(s store.Store, logger *slog.Logger) *Handler {
	return &Handler{store: s, logger: logger}
}

// HandleSessionEvent fetches advice beads matching the agent and trigger,
// then executes their hook commands. Returns a HookResponse indicating
// whether execution should be blocked, with any warnings.
func (h *Handler) HandleSessionEvent(ctx context.Context, event SessionEvent) HookResponse {
	var resp HookResponse

	if event.AgentID == "" || event.Trigger == "" {
		return resp
	}

	// Fetch all open advice beads.
	beads, _, err := h.store.ListBeads(ctx, model.BeadFilter{
		Type:   []model.BeadType{model.TypeAdvice},
		Status: []model.Status{model.StatusOpen},
	})
	if err != nil {
		h.logger.Error("hooks: failed to list advice beads", "err", err)
		return resp
	}

	// Build agent subscriptions for matching, enriched with agent bead config.
	agentSubs := model.BuildAgentSubscriptions(event.AgentID, nil)
	agentSubs = h.enrichAgentSubscriptions(ctx, event.AgentID, agentSubs)

	for _, bead := range beads {
		fields, err := parseAdviceFields(bead.Fields)
		if err != nil {
			h.logger.Warn("hooks: bad fields on advice bead", "id", bead.ID, "err", err)
			continue
		}

		// Must have a hook command and matching trigger.
		if fields.HookCommand == "" || fields.HookTrigger != event.Trigger {
			continue
		}

		// Check subscription matching via labels.
		if !model.MatchesSubscriptions(bead.Labels, agentSubs) {
			continue
		}

		// Execute the hook.
		env := map[string]string{"KD_AGENT": event.AgentID}
		result := Execute(ctx, fields.HookCommand, fields.HookTimeout, event.CWD, env)

		if result.Err != nil {
			switch fields.HookOnFailure {
			case OnFailureBlock:
				resp.Block = true
				resp.Reason = fmt.Sprintf("Advice hook blocked: %s\nCommand: %s\nError: %s\nOutput: %s",
					bead.Title, fields.HookCommand, result.Err, result.Output)
				return resp // Stop on first block.
			case OnFailureWarn:
				resp.Warnings = append(resp.Warnings,
					fmt.Sprintf("Advice hook warning: %s — %s (exit: %s)", bead.Title, result.Output, result.Err))
			default:
				// ignore
			}
		}

		h.logger.Info("hooks: executed advice hook",
			"id", bead.ID, "trigger", event.Trigger, "ok", result.Err == nil)
	}

	return resp
}

// enrichAgentSubscriptions looks up the agent bead and adds/removes custom
// advice subscriptions from the agent's configuration fields.
func (h *Handler) enrichAgentSubscriptions(ctx context.Context, agentID string, subs []string) []string {
	parts := strings.Split(agentID, "/")
	agentName := parts[len(parts)-1]

	agentBeads, _, err := h.store.ListBeads(ctx, model.BeadFilter{
		Type:   []model.BeadType{"agent"},
		Status: []model.Status{model.StatusOpen, model.StatusInProgress},
		Fields: map[string]string{"agent": agentName},
		Limit:  1,
	})
	if err != nil {
		h.logger.Warn("hooks: failed to look up agent bead for advice subscriptions", "agent_id", agentID, "err", err)
		return subs
	}
	if len(agentBeads) == 0 {
		return subs
	}

	var af struct {
		AdviceSubscriptions        []string `json:"advice_subscriptions"`
		AdviceSubscriptionsExclude []string `json:"advice_subscriptions_exclude"`
	}
	if err := json.Unmarshal(agentBeads[0].Fields, &af); err != nil {
		h.logger.Warn("hooks: failed to parse agent fields for advice subscriptions", "agent_id", agentID, "err", err)
		return subs
	}

	subs = append(subs, af.AdviceSubscriptions...)

	if len(af.AdviceSubscriptionsExclude) > 0 {
		excludeSet := make(map[string]bool, len(af.AdviceSubscriptionsExclude))
		for _, exc := range af.AdviceSubscriptionsExclude {
			excludeSet[exc] = true
		}
		filtered := subs[:0]
		for _, sub := range subs {
			if !excludeSet[sub] {
				filtered = append(filtered, sub)
			}
		}
		subs = filtered
	}

	return subs
}

// StartSubscriber listens for session lifecycle events on the event bus and
// runs matching advice hooks. It blocks until ctx is cancelled.
func (h *Handler) StartSubscriber(ctx context.Context, sub events.Subscriber) error {
	// Subscribe to all session events via wildcard.
	ch, cancel, err := sub.Subscribe("beads.session.>")
	if err != nil {
		return fmt.Errorf("hooks: subscribe: %w", err)
	}
	defer cancel()

	h.logger.Info("hooks: subscriber started")

	for {
		select {
		case <-ctx.Done():
			h.logger.Info("hooks: subscriber stopping")
			return nil
		case raw, ok := <-ch:
			if !ok {
				h.logger.Info("hooks: subscription channel closed")
				return nil
			}

			var event SessionEvent
			if err := json.Unmarshal(raw, &event); err != nil {
				h.logger.Warn("hooks: bad event payload", "err", err)
				continue
			}

			resp := h.HandleSessionEvent(ctx, event)
			if resp.Block {
				h.logger.Warn("hooks: blocked by advice hook", "reason", resp.Reason)
			}
			for _, w := range resp.Warnings {
				h.logger.Warn("hooks: " + w)
			}
		}
	}
}
