package beadsapi

import "strconv"

// Reopen-attempt accounting fields (ka-0000002985 DEFECT 3) — stamped on an
// ISSUE task bead each time an involuntary agent death returns it to the ready
// queue. Both reopen paths read and increment them:
//   - the in-pod graceful path (cmd/gb hook.go reopenOrphanedClaimedTask), and
//   - the controller SIGKILL backstop (statusreporter stalereaper
//     reopenStrandedIssueTasks).
//
// They live on the ISSUE task bead — which is the SAME bead across the
// successive AGENT beads (fresh bead id per respawn) that work one task_id — so
// the count survives respawns and both paths converge on one running total.
const (
	// ReopenAttemptsField holds the running count of involuntary-death reopens
	// of this task, as a decimal string.
	ReopenAttemptsField = "reopen_attempts"
	// ReopenCappedField is set to "true" when the cap was reached and the task
	// was PARKED (status=blocked) instead of reopened, so a viewer/query can tell
	// a human-parked cap from an ordinary block.
	ReopenCappedField = "reopen_capped"
	// ReopenCapReasonField records the last involuntary-death reason that drove
	// the task to the cap, for the audit trail.
	ReopenCapReasonField = "reopen_cap_reason"
	// ReapedAssigneeField names the gone agent whose assignee a reopen, park or
	// half-reap clear just removed. It carries the controller's reclaim grant
	// across the clear: kbeads lets the controller claim an unassigned
	// bug/feature/chore/epic for a respawned crew only while this names an
	// agent that has died (gd-8Irb2vDFf7). ClaimSpawnTask deletes it as it
	// claims.
	ReapedAssigneeField = "reaped_assignee"
)

// MaxReopenAttempts is the default cap on how many times an involuntary-death
// reopen returns the SAME issue task to the ready queue before it is PARKED for a
// human instead of auto-dispatched again.
//
// The 2026-08 site-deployment incident reopened 3 work items ~137 times total
// with no cap and no backoff: every fresh agent hit the same un-runnable
// condition (a project-wide credential failure) and died the same way, and the
// scheduler re-dispatched each time. A bounded number of retries absorbs a
// transient kill (a node drain, a one-off account rotation); beyond it,
// re-dispatch is just burning spend on work that cannot succeed as specified,
// and the correct move is to STOP auto-dispatching and surface it to a human.
const MaxReopenAttempts = 10

// ParseReopenAttempts reads the reopen_attempts field from a bead's fields,
// treating an absent or malformed value as 0. A missing counter is a task that
// has never been reopened, i.e. attempt 0.
func ParseReopenAttempts(fields map[string]string) int {
	if fields == nil {
		return 0
	}
	n, err := strconv.Atoi(fields[ReopenAttemptsField])
	if err != nil || n < 0 {
		return 0
	}
	return n
}
