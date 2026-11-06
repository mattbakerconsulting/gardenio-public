package bridge

import (
	"context"
	"slices"
)

// PromptRenderer renders a trigger's prompt at fire time, for a trigger whose
// prompt must carry live state rather than the state it was bound with: a
// land's steward is rendered with the land's purse, plans and people at each
// wake (internal/steward, Lands Track B §4.3), and with the message that woke
// it (ev).
type PromptRenderer func(ctx context.Context, trig TriggerDef, ev FireEvent) (string, error)

// FireEvent is what woke a fire, as a renderer reads it: the multi-trigger
// entry's label and, for a bead event, the bead and what happened to it. A
// field is "" when the fire has none (a cron entry has no bead; a
// single-source trigger has no entry).
type FireEvent struct {
	Entry     string
	BeadID    string
	EventType string
}

// fireEventOf reads a match's FireEvent from what multiDecisionToMatch
// records on it.
func fireEventOf(m TriggerMatch) FireEvent {
	return FireEvent{
		Entry:     m.Fields["multi_entry_label"],
		BeadID:    m.TemplateVars["{{event_bead_id}}"],
		EventType: m.TemplateVars["{{event_type}}"],
	}
}

// RegisterPromptRenderer renders the prompt of every trigger carrying label
// with r. Call it before the engine runs, as RegisterSource.
func (e *TriggerEngine) RegisterPromptRenderer(label string, r PromptRenderer) {
	if e.promptRenderers == nil {
		e.promptRenderers = map[string]PromptRenderer{}
	}
	e.promptRenderers[label] = r
}

// firePrompt is the prompt a fire spawns with: the registered renderer's for
// a trigger carrying its label, or the stored prompt. A renderer that fails or
// renders nothing falls back to the stored prompt, so a failed read never
// blocks a wake.
func (e *TriggerEngine) firePrompt(ctx context.Context, trig TriggerDef, m TriggerMatch) string {
	for label, r := range e.promptRenderers {
		if !slices.Contains(trig.Labels, label) {
			continue
		}
		p, err := r(ctx, trig, fireEventOf(m))
		if err != nil || p == "" {
			e.logger.Warn("triggers: fire-time prompt render failed, spawning with the stored prompt",
				"trigger", trig.ID, "renderer", label, "error", err)
			return trig.Prompt
		}
		return p
	}
	return trig.Prompt
}
