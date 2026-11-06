package bridge

import (
	"context"
	"regexp"
	"strings"
)

// ticketKeyRe matches JIRA-style ticket keys embedded in free text, e.g.
// "FSM-1234", "PE-7083", "DEVOPS-42". The project prefix is two or more
// uppercase letters/digits (so "PE" matches but a stray single letter does
// not); the suffix is one or more digits. Word boundaries keep it from
// matching mid-token noise.
var ticketKeyRe = regexp.MustCompile(`\b[A-Z][A-Z0-9]+-\d+\b`)

// cardSubjectInputs bundles the spawn-context signals used to derive the
// canonical subject shown as an agent card's title. Every field is optional;
// deriveCardSubject resolves them in priority order.
type cardSubjectInputs struct {
	TriggerText string // trigger match key / title or initial bead description
	Prompt      string // agent spawn prompt
	JiraKey     string // linked jira_key field on the agent bead
	BeadID      string // task / parent bead id (ka-XXX)
	Slug        string // slack message slug / agent name fallback
}

// deriveCardSubject returns the canonical subject for an agent card title,
// resolving the first available tier in priority order:
//
//  1. an FSM-/PE-/DEVOPS-style ticket key found in the trigger text or prompt
//  2. the linked JIRA key field
//  3. the task / parent bead id
//  4. the slack message slug (agent name fallback)
//
// The agent slug remains the bead identifier used in URLs and kd commands,
// but is only the title when no stronger subject is available. Returns "" only
// when every tier is empty.
func deriveCardSubject(in cardSubjectInputs) string {
	if key := firstTicketKey(in.TriggerText, in.Prompt); key != "" {
		return key
	}
	if k := strings.TrimSpace(in.JiraKey); k != "" {
		return k
	}
	if in.BeadID != "" {
		return in.BeadID
	}
	return in.Slug
}

// firstTicketKey returns the first FSM-/PE-/DEVOPS-style ticket key found
// scanning texts in order, or "" if none match.
func firstTicketKey(texts ...string) string {
	for _, t := range texts {
		if m := ticketKeyRe.FindString(t); m != "" {
			return m
		}
	}
	return ""
}

// agentCardSubject gathers spawn-context signals for an agent and derives the
// canonical card subject. The cached trigger context is the strongest, no-cost
// signal (jira/gitlab triggers carry the ticket key as MatchKey); the agent
// bead supplies the explicit jira_key field, spawn prompt, and task id.
func (b *Bot) agentCardSubject(ctx context.Context, agent string, trigger triggerCardInfo) string {
	in := cardSubjectInputs{
		Slug:        extractAgentName(agent),
		TriggerText: strings.TrimSpace(trigger.MatchKey + " " + trigger.Title),
	}
	if b != nil && b.daemon != nil {
		if detail, err := b.daemon.FindAgentBead(ctx, agent); err == nil && detail != nil {
			in.Prompt = detail.Fields["prompt"]
			in.JiraKey = detail.Fields["jira_key"]
			in.BeadID = detail.Fields["task_id"]
		}
	}
	// Fall back to the trigger bead id when the agent bead carries no task id.
	if in.BeadID == "" {
		in.BeadID = trigger.ID
	}
	return deriveCardSubject(in)
}
