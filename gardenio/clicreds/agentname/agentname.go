// Package agentname holds the agent names that both the controller and the
// beads daemon must spell the same way.
//
// An escalation executor's name is its per-agent token's subject: the bridge
// spawns it under that name (controller/internal/bridge/escalations.go) and
// the controller mints the token for it. The daemon reads the same name back
// as the binding for what that executor may do (kbeads
// escalation_close_grant.go, gd-Ti5JGqdhpd). One function, so the two sides
// cannot drift.
package agentname

import "strings"

const (
	// EscalationExecutorPrefix names the local executor of an approved
	// escalation (spawnLocalAgent).
	EscalationExecutorPrefix = "escalation-"
	// ServiceAccountExecutorPrefix names the system executor of an approved
	// execution_target=service_account escalation (triggerServiceAccount).
	ServiceAccountExecutorPrefix = "escalation-sa-"
)

// EscalationExecutor names an escalation's executor agent. Bead ids are
// mixed-case (gd-bH4HHHzucm), but the controller mints a per-agent token only
// for names matching [a-z0-9-] (agenttoken.go), and fails closed otherwise, so
// the id is lowercased.
func EscalationExecutor(prefix, beadID string) string {
	return prefix + strings.ToLower(beadID)
}
