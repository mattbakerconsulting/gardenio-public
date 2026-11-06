// Package bridge — bead→ticket scoping guard.
//
// An agent drives work through a bead. A Jira-sourced bead is BOUND to exactly
// one Jira issue, recorded in its jira_key field (and mirrored as a jira:<KEY>
// label). The bridge must never let a write triggered by one bead land on a
// *different* ticket than the one that bead is bound to — otherwise a bead that
// (by bug, race, or crafted field) names an arbitrary target key could make the
// bridge comment on, transition, link, or reassign a ticket the acting agent
// has no claim to.
//
// This guard makes that invariant explicit and testable: before any outbound
// mutation (AddComment / TransitionIssue / AddRemoteLink / AssignIssue) the
// caller checks that the TARGET issue key equals the bead's BOUND jira_key.
// Where the target is derived from the binding itself (today's sync-back path)
// this is a belt-and-suspenders assertion; where the target comes from a
// separate source (the poller compares the polled issue key against the bound
// bead's jira_key) it has real teeth.
//
// Scoping is orthogonal to the project gate (jira_project_gate.go): the project
// gate decides "may the bridge write to project X at all"; the scope guard
// decides "is THIS write aimed at the bead's own ticket". A write must pass
// both.
package bridge

import "strings"

// boundJiraKeyFromFields extracts the Jira issue key a bead is bound to from its
// field map, preferring the explicit jira_key field and falling back to a
// jira:<KEY> label (the poller sets both; older beads may carry only the
// label). Returns "" when the bead carries no binding.
func boundJiraKeyFromFields(fields map[string]string, labels []string) string {
	if k := strings.TrimSpace(fields["jira_key"]); k != "" {
		return k
	}
	for _, label := range labels {
		// jira-label:<x> are imported Jira *labels*, not the issue key — skip.
		if strings.HasPrefix(label, "jira:") && !strings.HasPrefix(label, "jira-label:") {
			if k := strings.TrimSpace(strings.TrimPrefix(label, "jira:")); k != "" {
				return k
			}
		}
	}
	return ""
}

// jiraWriteInScope reports whether an outbound write aimed at targetKey is in
// scope for a bead bound to boundKey. It is fail-closed: a write is in scope
// only when both keys are non-empty and name the same issue (case-insensitive,
// matching Jira's case-insensitive issue keys). An empty bound key (the bead is
// not bound to any ticket) or an empty target key denies the write — the bridge
// must never write to a ticket a bead does not own.
func jiraWriteInScope(boundKey, targetKey string) bool {
	boundKey = strings.TrimSpace(boundKey)
	targetKey = strings.TrimSpace(targetKey)
	if boundKey == "" || targetKey == "" {
		return false
	}
	return strings.EqualFold(boundKey, targetKey)
}
