// Package advice provides subscription matching and advice listing
// for the beads advice system. Agents receive advice based on label
// matching with AND/OR group semantics.
package advice

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"gardenio/controller/internal/beadsapi"
)

// BuildAgentSubscriptions creates auto-subscription labels for an agent.
// Always includes "global" and "agent:<agentID>", plus project/role labels
// parsed from the agent ID (format: project/role_plural/name).
func BuildAgentSubscriptions(agentID string, extra []string) []string {
	subs := make([]string, 0, len(extra)+3)
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
		if roleSingular := Singularize(rolePlural); roleSingular != rolePlural {
			subs = append(subs, "role:"+roleSingular)
		}
	}
	return AppendRoleFamilySubs(subs)
}

// AppendRoleFamilySubs adds role-FAMILY anchor subscriptions so a differentiated
// role inherits the shared advice pool of its family. Today the only family is
// the gardener family: any role carrying "gardener" as a hyphen-delimited token
// (gardener-bugfix, gardener-delivery, gardener-release-owner AND the SUFFIX forms
// plan-gardener, cost-gardener, infra-gardener) also subscribes to role:gardener /
// role:gardeners, so the ~50-bead gardener doctrine pool reaches the 90% of real
// gardenio gardeners that run a differentiated role (bug ka-0000000880, operator
// decision ka-0000003366 resolved [cut-pool]).
//
// It is a token match, NOT a gardener-* prefix glob: a prefix glob silently
// leaves plan-gardener/cost-gardener/infra-gardener — real archetypes carrying
// "gardener" as a suffix — un-advised (the scoping defect flagged in
// ka-0000003366). The function is idempotent, so it is safe to call from every
// subscription builder (BuildAgentSubscriptions, EnrichAgentSubscriptions,
// ArchetypeSubscriptions) and after enrichment adds role labels from any source
// (agent-ID path segment, the bead's role field, or its role: labels).
//
// Token boundaries are deliberate: substring names like "recaptcha" or
// "gardenerly" do NOT match, only a hyphen-delimited "gardener" token does. A
// consequence is that a test/e2e variant such as gardener-e2e is also treated as
// family and inherits the gardener pool — intended (it is a gardener shape), but
// note it if a gardener-e2e archetype ever goes live, since it then pays the full
// gardener pool against its own budget.
func AppendRoleFamilySubs(subs []string) []string {
	hasAnchor := false
	isFamily := false
	for _, s := range subs {
		role, ok := strings.CutPrefix(StripGroupPrefix(s), "role:")
		if !ok {
			continue
		}
		if role == "gardener" || role == "gardeners" {
			hasAnchor = true
			continue
		}
		for tok := range strings.SplitSeq(role, "-") {
			if tok == "gardener" || tok == "gardeners" {
				isFamily = true
				break
			}
		}
	}
	if isFamily && !hasAnchor {
		subs = append(subs, "role:gardener", "role:gardeners")
	}
	return subs
}

// EnrichAgentSubscriptions looks up the agent bead from the daemon and
// adds/removes custom advice subscriptions from the agent's configuration.
//
// agentID may be either an agent name (e.g. "thread-1776-849" or
// "project/role/name") or an agent bead ID (e.g. "ka-0000005132" — the
// shape of KD_AGENT_ID as set by the controller). Bead-ID-shaped inputs
// are resolved via GetBead; names go through FindMostRecentAgentBead, which
// considers closed beads when no active match exists — necessary for the
// session-resume window when the previous session bead has closed before the
// next one is created (see ka-0000007427).
func EnrichAgentSubscriptions(ctx context.Context, daemon *beadsapi.Client, agentID string, subs []string) []string {
	var agentBead *beadsapi.BeadDetail
	var err error
	if strings.HasPrefix(agentID, "gd-") || strings.HasPrefix(agentID, "ka-") {
		// Bead-ID-shaped input (e.g. KD_AGENT_ID from the controller).
		// FindAgentBead can't find this because it indexes by the `agent`
		// field, not by bead ID. GetBead is the authoritative lookup.
		agentBead, err = daemon.GetBead(ctx, agentID)
	} else {
		parts := strings.Split(agentID, "/")
		agentName := parts[len(parts)-1]
		agentBead, err = daemon.FindMostRecentAgentBead(ctx, agentName)
	}
	if err != nil || agentBead == nil {
		// Don't fail silently: an agent that primes with no project/role
		// context loses every project-scoped advice bead, which is almost
		// always a regression rather than intent.
		if err != nil && !errors.Is(err, beadsapi.ErrAgentNotFound) {
			slog.Warn("advice: failed to enrich agent subscriptions",
				"agent_id", agentID, "error", err)
		} else {
			slog.Warn("advice: no agent bead found for enrichment — project/role advice will not be delivered",
				"agent_id", agentID)
		}
		return subs
	}

	if raw, ok := agentBead.Fields["advice_subscriptions"]; ok && raw != "" {
		var extra []string
		if json.Unmarshal([]byte(raw), &extra) == nil {
			subs = append(subs, extra...)
		}
	}

	// Derive role: subscription from the agent bead's role field.
	if role, ok := agentBead.Fields["role"]; ok && role != "" {
		subs = append(subs, "role:"+role)
	}

	// Derive role: and project: subscriptions from the agent bead's own labels.
	for _, label := range agentBead.Labels {
		if strings.HasPrefix(label, "role:") || strings.HasPrefix(label, "project:") {
			subs = append(subs, label)
		}
	}

	// Re-apply the role-family anchor now that enrichment has added the bead's
	// role field + role: labels — a differentiated gardener whose role arrives
	// only via the role field (agentID was bead-ID-shaped, so BuildAgentSubscriptions
	// saw no role path segment) gets its role:gardener anchor here. Applied BEFORE
	// the exclude filter so an operator can still opt out via
	// advice_subscriptions_exclude: ["role:gardener"].
	subs = AppendRoleFamilySubs(subs)

	if raw, ok := agentBead.Fields["advice_subscriptions_exclude"]; ok && raw != "" {
		var exclude []string
		if json.Unmarshal([]byte(raw), &exclude) == nil && len(exclude) > 0 {
			excludeSet := make(map[string]bool, len(exclude))
			for _, exc := range exclude {
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
	}

	// Defensive observability: if the final subs carry no real project: label,
	// every project-scoped advice bead will be filtered out by
	// MatchesSubscriptions. That's almost always a bug, not intent.
	if !hasRealProjectSub(subs) {
		slog.Warn("advice: enriched subs carry no project: label — project-scoped advice will not match",
			"agent_id", agentID, "subs", subs)
	}

	return subs
}

// hasRealProjectSub reports whether subs contains at least one project: label
// that looks like an actual project name (i.e., not the junk
// "project:<bead-id>" that BuildAgentSubscriptions emits when given a bead-ID-
// shaped agentID).
func hasRealProjectSub(subs []string) bool {
	for _, s := range subs {
		clean := StripGroupPrefix(s)
		name, ok := strings.CutPrefix(clean, "project:")
		if !ok || name == "" {
			continue
		}
		if strings.HasPrefix(name, "gd-") || strings.HasPrefix(name, "ka-") {
			continue
		}
		return true
	}
	return false
}

// MatchesSubscriptions checks if an advice bead should be delivered to an agent
// based on the agent's subscription labels.
func MatchesSubscriptions(adviceLabels, subscriptions []string) bool {
	subSet := make(map[string]bool, len(subscriptions))
	for _, s := range subscriptions {
		subSet[s] = true
	}

	// Check required labels: project:X and agent:X must be in subscriptions.
	// role:X is required across the advice as a whole (OR among role labels):
	// if the advice carries any role: label, the agent must subscribe to at
	// least one of them. Without this gate, an advice tagged
	// `project:gardenio, role:gardener` matches every gardenio agent — not just
	// gardeners — because the bare project label forms its own OR group that
	// any project member satisfies.
	hasRoleLabel := false
	roleMatched := false
	for _, l := range adviceLabels {
		clean := StripGroupPrefix(l)
		if strings.HasPrefix(clean, "project:") && !subSet[clean] {
			return false
		}
		if strings.HasPrefix(clean, "agent:") && !subSet[clean] {
			return false
		}
		if strings.HasPrefix(clean, "role:") {
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

// FindMatchedLabels returns the subset of advice labels that match the subscriptions.
func FindMatchedLabels(adviceLabels, subscriptions []string) []string {
	subSet := make(map[string]bool, len(subscriptions))
	for _, s := range subscriptions {
		subSet[s] = true
	}
	seen := make(map[string]bool)
	var matched []string
	for _, l := range adviceLabels {
		clean := StripGroupPrefix(l)
		if subSet[clean] && !seen[clean] {
			matched = append(matched, clean)
			seen[clean] = true
		}
	}
	return matched
}

// CategorizeScope determines the targeting scope from advice labels.
// Returns scope (global/project/role/agent) and the target value.
func CategorizeScope(labels []string) (scope, target string) {
	for _, l := range labels {
		clean := StripGroupPrefix(l)
		switch {
		case strings.HasPrefix(clean, "agent:"):
			return "agent", strings.TrimPrefix(clean, "agent:")
		case strings.HasPrefix(clean, "role:"):
			scope, target = "role", strings.TrimPrefix(clean, "role:")
		case strings.HasPrefix(clean, "project:") && scope != "role":
			scope, target = "project", strings.TrimPrefix(clean, "project:")
		case clean == "global" && scope == "":
			scope, target = "global", ""
		}
	}
	if scope == "" {
		scope = "global"
	}
	return scope, target
}

// CategorizeScopeForMatch is like CategorizeScope but, when the advice carries
// multiple role: labels (multi-role targeting), prefers the role: label that
// the agent actually matched. Without this, an advice tagged with both
// role:gardener-e2e and role:crew-e2e renders under the sibling role's header
// in a gardener-e2e agent's loadout purely because of label order — the
// gardener-e2e role-group leaks "Role: crew-e2e" entries (ka-0000001489).
func CategorizeScopeForMatch(labels, matchedLabels []string) (scope, target string) {
	for _, l := range labels {
		clean := StripGroupPrefix(l)
		if name, ok := strings.CutPrefix(clean, "agent:"); ok {
			return "agent", name
		}
	}
	for _, m := range matchedLabels {
		clean := StripGroupPrefix(m)
		if name, ok := strings.CutPrefix(clean, "role:"); ok {
			return "role", name
		}
	}
	return CategorizeScope(labels)
}

// BuildScopeHeader returns a human-readable header for the scope.
func BuildScopeHeader(scope, target string) string {
	switch scope {
	case "global":
		return "Global"
	case "project":
		return "Project: " + target
	case "role":
		return "Role: " + target
	case "agent":
		return "Agent: " + target
	default:
		return scope
	}
}

// GroupSortKey returns a sortable key for ordering scope groups.
func GroupSortKey(scope, target string) string {
	switch scope {
	case "global":
		return "0:" + target
	case "project":
		return "1:" + target
	case "role":
		return "2:" + target
	case "agent":
		return "3:" + target
	default:
		return "9:" + target
	}
}

// StripGroupPrefix removes the gN: prefix from a label if present.
// "g0:role:polecat" -> "role:polecat", "global" -> "global".
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
		// No valid gN: prefix -- treat as its own group (OR behavior).
		groups[nextUnprefixed] = append(groups[nextUnprefixed], label)
		nextUnprefixed++
	}
	return groups
}

// HasTargetingLabel checks whether any label is a targeting label
// (global, project:, role:, agent:).
func HasTargetingLabel(labels []string) bool {
	for _, l := range labels {
		l = StripGroupPrefix(l)
		switch {
		case l == "global":
			return true
		case strings.HasPrefix(l, "project:"):
			return true
		case strings.HasPrefix(l, "role:"):
			return true
		case strings.HasPrefix(l, "agent:"):
			return true
		}
	}
	return false
}

// Singularize converts a plural role name to singular by stripping a trailing "s".
func Singularize(plural string) string {
	if s, ok := strings.CutSuffix(plural, "s"); ok {
		return s
	}
	return plural
}
