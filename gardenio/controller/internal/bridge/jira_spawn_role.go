// Package bridge provides the JIRA spawn-role resolver.
//
// Both JiraMentionHandler and JiraAgentResolver need to decide what role to
// give agents they spawn for JIRA events. Per decision ka-0000000381 / ka-0000007882,
// JIRA mentions are treated as a special case of triggers: the role is read
// from the matching JIRA trigger bead for the target project, same source of
// truth as the JQL trigger engine uses. There is no hardcoded fallback —
// spawns abort if no enabled spawn_agent trigger is configured for the project.
package bridge

import (
	"context"
	"fmt"
	"log/slog"

	"gardenio/controller/internal/beadsapi"
)

// jiraTriggerLister is the minimal interface needed to look up JIRA trigger beads.
type jiraTriggerLister interface {
	ListBeadsFiltered(ctx context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error)
}

// resolveJiraSpawnRole picks the agent role for a JIRA-event-spawned agent by
// consulting the same trigger beads the JQL poller uses. It returns the role
// of the first enabled spawn_agent trigger whose project matches. If no match
// is found (or lookup fails), it returns an error — callers must abort the
// spawn rather than pick a hardcoded default.
func resolveJiraSpawnRole(ctx context.Context, daemon jiraTriggerLister, project string, logger *slog.Logger) (string, error) {
	return resolveJiraSpawnRoleHinted(ctx, daemon, project, "", logger)
}

// resolveJiraSpawnRoleHinted is resolveJiraSpawnRole with a mention-time role
// hint (design-doc F-E, epic ka-0000007401). When preferredRole is non-empty
// (an operator wrote "@gardenio --role <name> …" on the ticket), it is honored
// ONLY if some enabled spawn_agent trigger for this project already advertises
// that role — deny-by-default, exactly like --fresh and the JQL/spawn gates: an
// operator can pick among the roles the project is already configured to spawn,
// not conjure a new one. An unknown/unconfigured hint is ignored with a log
// line and resolution falls back to the project's default (first enabled)
// trigger role — the identical behavior to a mention with no hint at all.
//
// preferredRole == "" reproduces the original first-enabled behavior exactly,
// so an un-hinted mention is byte-for-byte unchanged.
func resolveJiraSpawnRoleHinted(ctx context.Context, daemon jiraTriggerLister, project, preferredRole string, logger *slog.Logger) (string, error) {
	result, err := daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"trigger"},
		Statuses: []string{"open"},
		Labels:   []string{"trigger:jira"},
		Limit:    100,
	})
	if err != nil {
		return "", fmt.Errorf("jira spawn-role: trigger lookup failed for project %q: %w", project, err)
	}
	// First pass: honor a valid hint. We only accept a role this project is
	// already configured to spawn, so the hint can never widen the spawn
	// surface — it only re-selects among the project's enabled trigger roles.
	defaultRole := ""
	for _, bead := range result.Beads {
		if bead.Fields["enabled"] == "false" {
			continue
		}
		if bead.Fields["action"] != "spawn_agent" {
			continue
		}
		if bead.Fields["project"] != project {
			continue
		}
		role := bead.Fields["role"]
		if role == "" {
			continue
		}
		if defaultRole == "" {
			defaultRole = role // first enabled — the un-hinted default
		}
		if preferredRole != "" && role == preferredRole {
			if logger != nil {
				logger.Info("jira spawn-role: honoring mention --role hint",
					"project", project, "role", preferredRole)
			}
			return role, nil
		}
	}
	if defaultRole == "" {
		return "", fmt.Errorf("jira spawn-role: no enabled spawn_agent trigger bead configured for project %q", project)
	}
	if preferredRole != "" && logger != nil {
		// Hint named a role this project is not configured to spawn — ignore it
		// and fall back to the default, matching an un-hinted mention.
		logger.Info("jira spawn-role: mention --role hint not a configured spawn role for project; using default",
			"project", project, "requested_role", preferredRole, "default_role", defaultRole)
	}
	return defaultRole, nil
}
