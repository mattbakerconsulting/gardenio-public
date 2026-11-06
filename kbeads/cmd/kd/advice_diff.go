package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"gardenio/kbeads/internal/client"
	"gardenio/kbeads/internal/model"
)

// printAdviceDiff fetches all advice beads and prints any that become newly
// relevant when the claimed bead's labels are added to the agent's subscriptions.
// This ensures agents see role- or project-specific advice they didn't get at
// session start.
func printAdviceDiff(ctx context.Context, claimedBead *model.Bead) {
	baseSubs := buildAgentSubscriptions()
	if len(baseSubs) == 0 {
		return // not running as an agent — skip
	}

	// Enrich subscriptions with the claimed bead's targeting labels.
	enriched := make([]string, len(baseSubs))
	copy(enriched, baseSubs)
	for _, label := range claimedBead.Labels {
		if isTargetingLabel(label) {
			enriched = append(enriched, label)
		}
	}

	// If no new labels, no diff possible.
	if len(enriched) == len(baseSubs) {
		return
	}

	allAdvice, err := fetchAllAdvice(ctx)
	if err != nil || len(allAdvice) == 0 {
		return
	}

	baseSet := makeSet(baseSubs)
	enrichedSet := makeSet(enriched)

	var diff []*model.Bead
	for _, advice := range allAdvice {
		matchesEnriched := matchesSubscriptions(advice.Labels, enrichedSet)
		matchesBase := matchesSubscriptions(advice.Labels, baseSet)
		if matchesEnriched && !matchesBase {
			diff = append(diff, advice)
		}
	}

	if len(diff) == 0 {
		return
	}

	fmt.Fprintf(os.Stderr, "\n📋 %d new advice item(s) from claimed bead's labels:\n\n", len(diff))
	for _, a := range diff {
		scope, target := categorizeScope(a.Labels)
		header := buildScopeHeader(scope, target)
		fmt.Fprintf(os.Stderr, "  [%s] %s\n", header, a.Title)
		if a.Description != "" && a.Description != a.Title {
			// Print first 3 lines of description as preview.
			lines := strings.SplitN(a.Description, "\n", 4)
			limit := len(lines)
			if limit > 3 {
				limit = 3
			}
			for _, line := range lines[:limit] {
				fmt.Fprintf(os.Stderr, "    %s\n", line)
			}
			if len(lines) > 3 {
				fmt.Fprintf(os.Stderr, "    ...\n")
			}
		}
	}
	fmt.Fprintln(os.Stderr)
}

// buildAgentSubscriptions creates the agent's base subscription labels from env vars.
func buildAgentSubscriptions() []string {
	var subs []string
	subs = append(subs, "global")

	if agent := os.Getenv("BOAT_AGENT"); agent != "" {
		subs = append(subs, "agent:"+agent)
	}
	if project := agentProject(); project != "" {
		subs = append(subs, "project:"+project)
	}
	if role := os.Getenv("BOAT_ROLE"); role != "" {
		subs = append(subs, "role:"+role)
		if singular := singularize(role); singular != role {
			subs = append(subs, "role:"+singular)
		}
	}
	return subs
}

// fetchAllAdvice retrieves all open advice beads.
func fetchAllAdvice(ctx context.Context) ([]*model.Bead, error) {
	resp, err := beadsClient.ListBeads(ctx, &client.ListBeadsRequest{
		Type:   []string{"advice"},
		Status: []string{"open"},
		Limit:  500,
	})
	if err != nil {
		return nil, err
	}
	return resp.Beads, nil
}

// matchesSubscriptions checks if an advice bead should be delivered to an agent
// based on the agent's subscription labels.
func matchesSubscriptions(adviceLabels []string, subSet map[string]bool) bool {
	// Check required labels: project:X and agent:X must be in subscriptions.
	for _, l := range adviceLabels {
		clean := stripGroupPrefix(l)
		if strings.HasPrefix(clean, "project:") && !subSet[clean] {
			return false
		}
		if strings.HasPrefix(clean, "agent:") && !subSet[clean] {
			return false
		}
	}

	groups := parseGroups(adviceLabels)
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

// parseGroups extracts group numbers from label prefixes.
// Labels with gN: prefix are grouped together (AND within group).
// Labels without prefix are treated as separate groups (OR behavior).
func parseGroups(labels []string) map[int][]string {
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
		groups[nextUnprefixed] = append(groups[nextUnprefixed], label)
		nextUnprefixed++
	}
	return groups
}

// stripGroupPrefix removes the gN: prefix from a label if present.
func stripGroupPrefix(label string) string {
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

// categorizeScope determines the targeting scope from advice labels.
func categorizeScope(labels []string) (scope, target string) {
	for _, l := range labels {
		clean := stripGroupPrefix(l)
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

// buildScopeHeader returns a human-readable header for the scope.
func buildScopeHeader(scope, target string) string {
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

// isTargetingLabel returns true if the label is a targeting label.
func isTargetingLabel(label string) bool {
	clean := stripGroupPrefix(label)
	switch {
	case clean == "global":
		return true
	case strings.HasPrefix(clean, "project:"):
		return true
	case strings.HasPrefix(clean, "role:"):
		return true
	case strings.HasPrefix(clean, "agent:"):
		return true
	}
	return false
}

// singularize converts a plural role name to singular by stripping a trailing "s".
func singularize(plural string) string {
	if s, ok := strings.CutSuffix(plural, "s"); ok {
		return s
	}
	return plural
}

// makeSet creates a string set from a slice.
func makeSet(items []string) map[string]bool {
	s := make(map[string]bool, len(items))
	for _, item := range items {
		s[item] = true
	}
	return s
}
