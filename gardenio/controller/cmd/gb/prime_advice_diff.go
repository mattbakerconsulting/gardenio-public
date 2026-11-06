package main

// prime_advice_diff.go outputs an advice diff when an agent's claimed bead
// has a role label that differs from the agent's own BOAT_ROLE. This handles
// the formula role-per-step case where the same agent continues working but
// needs to know what advice changed for the new role.

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"gardenio/controller/internal/advice"
)

// outputAdviceRoleDiff detects if the agent's claimed bead targets a different
// role and outputs the advice diff (added/removed items). This is called
// during gb prime after the normal advice output.
func outputAdviceRoleDiff(w io.Writer, agentID string) {
	agentRole := os.Getenv("BOAT_ROLE")
	if agentRole == "" || agentID == "" {
		return
	}

	ctx := context.Background()

	// Find the agent's currently claimed bead.
	agentName := agentID
	parts := strings.Split(agentID, "/")
	if len(parts) > 0 {
		agentName = parts[len(parts)-1]
	}

	task, err := daemon.ListAssignedTask(ctx, agentName)
	if err != nil || task == nil {
		return
	}

	// Check if the claimed bead has a role label different from BOAT_ROLE.
	targetRole := detectRoleMismatch(task.Labels, agentRole)
	if targetRole == "" {
		return // no mismatch — nothing to output
	}

	// Get the agent's current advice (what they already see).
	currentAdvice, _, err := advice.ListAdviceForAgent(ctx, daemon, agentID)
	if err != nil {
		return
	}

	// Compute the diff.
	diff, err := advice.DiffAdviceForRole(ctx, daemon, agentID, targetRole, currentAdvice)
	if err != nil || diff.IsEmpty() {
		return
	}

	// Output the diff section.
	fmt.Fprintf(w, "\n## Role Transition: %s → %s\n\n", agentRole, targetRole)
	fmt.Fprintf(w, "Your claimed task has `role:%s` — the following advice changes apply:\n\n", targetRole)

	if len(diff.Added) > 0 {
		fmt.Fprintf(w, "### New advice for role:%s (%d items)\n\n", targetRole, len(diff.Added))
		outputAdviceDiffItems(w, agentID, diff.Added)
	}

	if len(diff.Removed) > 0 {
		fmt.Fprintf(w, "### No longer applicable (%d items)\n\n", len(diff.Removed))
		for _, item := range diff.Removed {
			fmt.Fprintf(w, "- ~~**[%s]** %s~~\n", item.ScopeHeader, item.Bead.Title)
		}
		fmt.Fprintln(w)
	}
}

// outputAdviceDiffItems renders added advice items via the shared budget
// cascade (renderAdviceItemsWithLimits), using the tight diff-specific budgets
// (adviceDiffBudgetBytes / adviceDiffTitleBudgetBytes). The role diff is a
// supplementary delta layered on top of the already-rendered advice section, so
// it must not inherit the main path's 80 KB cap or it could blow the 32 KB
// stress ceiling when the target role's catalog is large.
func outputAdviceDiffItems(w io.Writer, agentID string, items []advice.MatchedAdvice) {
	type scopeGroup struct {
		Header string
		Items  []advice.MatchedAdvice
	}

	groupMap := make(map[string]*scopeGroup)
	for _, m := range items {
		key := m.Scope + ":" + m.ScopeTarget
		g, ok := groupMap[key]
		if !ok {
			g = &scopeGroup{Header: m.ScopeHeader}
			groupMap[key] = g
		}
		g.Items = append(g.Items, m)
	}

	var groups []*scopeGroup
	for _, g := range groupMap {
		groups = append(groups, g)
	}
	sort.Slice(groups, func(i, j int) bool {
		return groups[i].Header < groups[j].Header
	})

	var flat []adviceRenderItem
	for _, g := range groups {
		// Same deterministic intra-group priority+recency ordering as the main
		// prime path (F1, ka-0000008175) so the diff evicts under budget in the
		// same reproducible order.
		advice.SortByPriorityRecency(g.Items)
		for _, item := range g.Items {
			flat = append(flat, adviceRenderItem{Header: g.Header, Bead: item.Bead})
		}
	}
	renderAdviceItemsWithLimits(w, agentID, flat, adviceDiffBudgetBytes(), adviceDiffTitleBudgetBytes())
}

// detectRoleMismatch checks if a bead's labels contain a role:X label that
// doesn't match the agent's current role. Returns the mismatched target role
// or "" if no mismatch is found.
func detectRoleMismatch(labels []string, agentRole string) string {
	agentRoles := parseRoles(agentRole)

	for _, l := range labels {
		if strings.HasPrefix(l, "role:") {
			role := strings.TrimPrefix(l, "role:")
			if !agentRoles[role] && !agentRoles[advice.Singularize(role)] {
				return role
			}
		}
	}
	return ""
}

// parseRoles parses a comma-separated BOAT_ROLE value into a set, including
// both the literal value and its singular form.
func parseRoles(boatRole string) map[string]bool {
	roles := make(map[string]bool)
	for _, r := range strings.Split(boatRole, ",") {
		r = strings.TrimSpace(r)
		if r != "" {
			roles[r] = true
			roles[advice.Singularize(r)] = true
		}
	}
	return roles
}
