package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// TriggerAuthResult is the outcome of a trigger authorization check.
type TriggerAuthResult struct {
	Allowed bool
	Reason  string
}

// checkTriggerAuth verifies that a trigger is authorized to perform its action.
// Authorization checks:
//  1. Trigger must have a creator (CreatedBy field).
//  2. If AllowedProjects is set, the trigger's target project must be in the list.
//  3. If AllowedRoles is set, the trigger's target role must be in the list.
//  4. For spawn_agent actions, the target project and role must be permitted.
func checkTriggerAuth(trig TriggerDef) TriggerAuthResult {
	// Triggers without a creator are system-created and always allowed.
	if trig.CreatedBy == "" {
		return TriggerAuthResult{Allowed: true, Reason: "system trigger (no creator)"}
	}

	// If allowed_projects is set, check that the target project is in scope.
	if len(trig.AllowedProjects) > 0 && trig.Project != "" {
		if !containsOrWildcard(trig.AllowedProjects, trig.Project) {
			return TriggerAuthResult{
				Allowed: false,
				Reason:  fmt.Sprintf("project %q not in allowed_projects %v", trig.Project, trig.AllowedProjects),
			}
		}
	}

	// If allowed_roles is set, check that the target role is in scope.
	if len(trig.AllowedRoles) > 0 && trig.Role != "" {
		if !containsOrWildcard(trig.AllowedRoles, trig.Role) {
			return TriggerAuthResult{
				Allowed: false,
				Reason:  fmt.Sprintf("role %q not in allowed_roles %v", trig.Role, trig.AllowedRoles),
			}
		}
	}

	return TriggerAuthResult{Allowed: true, Reason: "authorized"}
}

// containsOrWildcard checks if the list contains the value or a wildcard "*".
func containsOrWildcard(list []string, value string) bool {
	for _, item := range list {
		if item == "*" || item == value {
			return true
		}
	}
	return false
}

// creatorSpawnScopes holds the spawn scope fields from a creator's user bead.
type creatorSpawnScopes struct {
	AllowedProjects []string
	AllowedRoles    []string
}

// recheckSpawnAuthzAtFire re-validates a spawn_agent action at fire time
// against the trigger creator's CURRENT user-bead spawn scopes. The
// AllowedProjects / AllowedRoles fields on the trigger bead are client-supplied
// advisory data (B1 finding) and MUST NOT be trusted at fire time: an operator
// could have crafted a trigger with AllowedRoles=["admin"] at creation and the
// daemon would only enforce coarse write_policy on the bead itself. This
// function replicates the kbeads authorizeAgentSpawn check at the controller,
// using the creator's user bead as the authoritative source of scopes.
//
// Scope (matches kbeads behavior):
//   - Only runs for spawn-producing actions (spawn_agent, apply_formula —
//     which spawns a molecule driver); other actions have no spawn scope.
//   - Triggers with no CreatedBy (system triggers) are allowed.
//   - Triggers whose CreatedBy is not an OIDC identity (no "@") bypass,
//     matching authorizeAgentSpawn's non-OIDC bypass for static-token callers.
//
// Fail-closed cases:
//   - CreatedBy looks OIDC but no matching user bead exists → deny (creator
//     may have been deprovisioned since the trigger was created).
//   - User bead exists but allowed_projects / allowed_roles do not include
//     the target (with "*" wildcard support) → deny.
func recheckSpawnAuthzAtFire(ctx context.Context, daemon TriggerBeadClient, trig TriggerDef) TriggerAuthResult {
	// apply_formula spawns a driver agent for the poured molecule
	// (ka-0000000070), so it carries the same spawn scope as spawn_agent.
	if trig.Action != TriggerActionSpawnAgent && trig.Action != TriggerActionApplyFormula {
		return TriggerAuthResult{Allowed: true, Reason: "not a spawn action"}
	}
	if trig.CreatedBy == "" {
		return TriggerAuthResult{Allowed: true, Reason: "system trigger (no creator)"}
	}
	if !strings.Contains(trig.CreatedBy, "@") {
		// Service / static-token identities bypass, mirroring the kbeads
		// authorizeAgentSpawn bypass for non-OIDC auth methods. These are
		// already constrained at write time by write_policy + fleet-token scopes.
		return TriggerAuthResult{Allowed: true, Reason: "non-OIDC creator bypass"}
	}

	perms, err := lookupCreatorSpawnScopes(ctx, daemon, trig.CreatedBy)
	if err != nil {
		return TriggerAuthResult{
			Allowed: false,
			Reason:  fmt.Sprintf("cannot verify creator scopes for %q: %v", trig.CreatedBy, err),
		}
	}
	if perms == nil {
		return TriggerAuthResult{
			Allowed: false,
			Reason:  fmt.Sprintf("creator %q has no user bead (deprovisioned?)", trig.CreatedBy),
		}
	}

	if trig.Project != "" && len(perms.AllowedProjects) > 0 {
		if !containsOrWildcard(perms.AllowedProjects, trig.Project) {
			return TriggerAuthResult{
				Allowed: false,
				Reason: fmt.Sprintf(
					"creator %q not authorized for project %q (user-bead allowed_projects=%v)",
					trig.CreatedBy, trig.Project, perms.AllowedProjects),
			}
		}
	}
	if trig.Role != "" && len(perms.AllowedRoles) > 0 {
		if !containsOrWildcard(perms.AllowedRoles, trig.Role) {
			return TriggerAuthResult{
				Allowed: false,
				Reason: fmt.Sprintf(
					"creator %q not authorized for role %q (user-bead allowed_roles=%v)",
					trig.CreatedBy, trig.Role, perms.AllowedRoles),
			}
		}
	}
	return TriggerAuthResult{Allowed: true, Reason: "authorized by creator scopes"}
}

// lookupCreatorSpawnScopes finds the user bead for the given email and returns
// its parsed allowed_projects / allowed_roles fields. Returns nil if no user
// bead matches (caller must treat this as a deny).
//
// The controller's ListBeadsFiltered does not support a server-side fields
// filter (unlike the daemon's own store), so we list type=user config beads
// and match client-side. The user population is small and already cached in
// UserDirectory; for the narrow fire-time authz path we fetch fresh to avoid
// staleness from scope changes not yet picked up by SSE.
func lookupCreatorSpawnScopes(ctx context.Context, daemon TriggerBeadClient, email string) (*creatorSpawnScopes, error) {
	result, err := daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"user"},
		Kinds:    []string{"config"},
		Statuses: []string{"open", "in_progress"},
		Limit:    500,
	})
	if err != nil {
		return nil, err
	}
	for _, b := range result.Beads {
		if b.Fields["email"] != email {
			continue
		}
		return &creatorSpawnScopes{
			AllowedProjects: parseStringList(b.Fields["allowed_projects"]),
			AllowedRoles:    parseStringList(b.Fields["allowed_roles"]),
		}, nil
	}
	return nil, nil
}

// parseStringList parses a JSON array or comma-separated string into a string slice.
func parseStringList(raw string) []string {
	if raw == "" {
		return nil
	}
	raw = strings.TrimSpace(raw)

	// Try JSON array first.
	if strings.HasPrefix(raw, "[") {
		var list []string
		if err := json.Unmarshal([]byte(raw), &list); err == nil && len(list) > 0 {
			return list
		}
	}

	// Fall back to comma-separated.
	parts := strings.Split(raw, ",")
	var result []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			result = append(result, p)
		}
	}
	return result
}

// TriggerAuditEntry represents a trigger fire event for the audit trail.
type TriggerAuditEntry struct {
	TriggerID    string
	TriggerTitle string
	Source       string
	Action       TriggerAction
	MatchKey     string
	MatchTitle   string
	Project      string
	Role         string
	CreatedBy    string
	Outcome      string // "allow" or "deny"
	Reason       string
	Timestamp    time.Time
}

// recordTriggerAudit creates an audit bead recording a trigger fire event.
func recordTriggerAudit(ctx context.Context, daemon TriggerBeadClient, logger *slog.Logger, entry TriggerAuditEntry) {
	fields := map[string]string{
		"trigger_id":    entry.TriggerID,
		"trigger_title": entry.TriggerTitle,
		"source":        entry.Source,
		"action":        string(entry.Action),
		"match_key":     entry.MatchKey,
		"match_title":   entry.MatchTitle,
		"project":       entry.Project,
		"role":          entry.Role,
		"created_by":    entry.CreatedBy,
		"outcome":       entry.Outcome,
		"reason":        entry.Reason,
		"timestamp":     entry.Timestamp.UTC().Format(time.RFC3339),
	}

	fieldsJSON, _ := json.Marshal(fields)

	title := fmt.Sprintf("trigger:%s %s %s", entry.Source, entry.Outcome, entry.MatchKey)

	// Audit-event beads are append-only ledger records, not work — create
	// them in status=closed so they never surface in ready/news queries
	// or inflate the open-bead count (ka-0000004100).
	_, err := daemon.CreateBead(ctx, beadsapi.CreateBeadRequest{
		Title:     title,
		Type:      "event",
		Status:    "closed",
		Labels:    []string{"auto:trigger-audit", "source:" + entry.Source, "trigger:" + entry.TriggerID},
		Fields:    json.RawMessage(fieldsJSON),
		CreatedBy: "trigger-engine",
	})
	if err != nil {
		logger.Warn("triggers: failed to record audit",
			"trigger", entry.TriggerID, "outcome", entry.Outcome, "error", err)
	}
}
