package model

import (
	"fmt"
	"strings"
)

// MatchesSubscriptions checks if an advice bead should be delivered to an agent
// based on the agent's subscription labels.
//
// Matching rules:
//   - If advice has project:X label, agent MUST be subscribed to project:X
//     (required match).
//   - If advice has agent:X label, agent MUST be subscribed to agent:X.
//   - If advice has any role:X label(s), the agent MUST be subscribed to at
//     least one of them (OR among role labels).
//   - For other labels: AND within groups (gN: prefix), OR across groups.
//
// This prevents project-scoped advice from leaking to other projects via
// role matches, and prevents role:gardener advice from leaking to crew agents
// via a bare project label OR-group (ka-0000001253).
func MatchesSubscriptions(adviceLabels, subscriptions []string) bool {
	subSet := make(map[string]bool, len(subscriptions))
	for _, s := range subscriptions {
		subSet[s] = true
	}

	// Required-match pre-filter: project:/agent: labels on the advice
	// must appear in the agent's subscriptions. role: labels gate
	// collectively (OR): if any role label is present, at least one must
	// be in the agent's subscriptions.
	hasRoleLabel := false
	roleMatched := false
	for _, l := range adviceLabels {
		clean := StripGroupPrefix(l)
		switch {
		case strings.HasPrefix(clean, "project:"):
			if !subSet[clean] {
				return false
			}
		case strings.HasPrefix(clean, "agent:"):
			if !subSet[clean] {
				return false
			}
		case strings.HasPrefix(clean, "role:"):
			hasRoleLabel = true
			if subSet[clean] {
				roleMatched = true
			}
		}
	}
	if hasRoleLabel && !roleMatched {
		return false
	}

	// Parse label groups for AND/OR matching.
	groups := ParseGroups(adviceLabels)

	// OR across groups: if any group fully matches, advice applies.
	for _, groupLabels := range groups {
		if len(groupLabels) == 0 {
			continue
		}
		allMatch := true
		for _, label := range groupLabels {
			if !subSet[label] {
				allMatch = false
				break
			}
		}
		if allMatch {
			return true
		}
	}
	return false
}

// ParseGroups extracts group numbers from label prefixes.
// Labels with gN: prefix are grouped together (AND within group).
// Labels without prefix are treated as separate groups (backward compat - OR behavior).
func ParseGroups(labels []string) map[int][]string {
	groups := make(map[int][]string)
	nextUnprefixed := 1000

	for _, label := range labels {
		if strings.HasPrefix(label, "g") {
			idx := strings.Index(label, ":")
			if idx > 1 {
				var groupNum int
				if _, err := fmt.Sscanf(label[:idx], "g%d", &groupNum); err == nil {
					groups[groupNum] = append(groups[groupNum], label[idx+1:])
					continue
				}
			}
		}
		// No valid gN: prefix — treat as its own group (OR behavior).
		groups[nextUnprefixed] = append(groups[nextUnprefixed], label)
		nextUnprefixed++
	}
	return groups
}

// StripGroupPrefix removes the gN: prefix from a label if present.
// "g0:role:polecat" → "role:polecat", "global" → "global".
func StripGroupPrefix(label string) string {
	if len(label) >= 3 && label[0] == 'g' {
		for i := 1; i < len(label); i++ {
			if label[i] == ':' && i > 1 {
				return label[i+1:]
			}
			if label[i] < '0' || label[i] > '9' {
				break
			}
		}
	}
	return label
}

// BuildAgentSubscriptions creates auto-subscription labels for an agent.
// It always includes "global" and "agent:<agentID>", plus project/role
// labels parsed from the agent ID (format: project/role_plural/name).
func BuildAgentSubscriptions(agentID string, extra []string) []string {
	subs := make([]string, 0, len(extra)+4)
	subs = append(subs, extra...)
	subs = append(subs, "global")
	subs = append(subs, "agent:"+agentID)

	parts := strings.Split(agentID, "/")
	if len(parts) >= 1 && parts[0] != "" {
		subs = append(subs, "project:"+parts[0])
	}
	if len(parts) >= 2 {
		rolePlural := parts[1]
		subs = append(subs, "role:"+rolePlural)
		if roleSingular := singularize(rolePlural); roleSingular != rolePlural {
			subs = append(subs, "role:"+roleSingular)
		}
	}
	return subs
}

// singularize converts a plural role name to singular by stripping a trailing "s".
func singularize(plural string) string {
	if s, ok := strings.CutSuffix(plural, "s"); ok {
		return s
	}
	return plural
}
