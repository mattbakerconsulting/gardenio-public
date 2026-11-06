package beadsapi

import (
	"fmt"
	"regexp"
	"strings"
)

// CI-wait field keys (plan gardenio/plans/wake-on-ci-completion.md §2.1, task
// gd-AwuRDezONo). A CI wait is NOT a bead of its own: `gb wait ci` stamps these
// fields on the CALLING agent's own bead (operator steer: no new bead type), so
// one agent holds at most one wait and a second call replaces the first. The
// in-pod writer (cmd/gb wait_ci.go), the stop gate (cmd/gb stop.go) and the
// bridge resolver (P2) all key on these names, so they live here in the shared
// package rather than as drifting copies. Every key is registered on type:agent
// in BOTH kbeads internal/server/config.go and internal/bridge/init.go.
const (
	// FieldCIWaitState is the wait lifecycle:
	// pending → resolved | timed_out → consumed, or pending | resolved |
	// timed_out → cancelled. Besides the key it is the only field the resolver
	// filters on.
	FieldCIWaitState = "ci_wait_state"
	// FieldCIWaitKey is "OWNER/REPO@<40-char sha>" (CIWaitKey): one scalar so a
	// webhook event resolves its waiters with a single exact-match lookup.
	FieldCIWaitKey = "ci_wait_key"
	// FieldCIWaitWorkflows is an optional comma-separated workflow-name filter;
	// empty means every workflow run for the sha.
	FieldCIWaitWorkflows = "ci_wait_workflows"
	// FieldCIWaitTask is the claimed task the wait parks (defaults to the agent
	// bead's task_id). Same role as requesting_task on a decision/escalation.
	FieldCIWaitTask = "ci_wait_task"
	// FieldCIWaitDeadline is the RFC3339 time at which the wait resolves as
	// timed_out (and still wakes the agent) if CI never concludes.
	FieldCIWaitDeadline = "ci_wait_deadline"
	// FieldCIWaitResult is the JSON per-workflow
	// [{name, run_id, run_attempt, conclusion}] written at resolution.
	FieldCIWaitResult = "ci_wait_result"
	// FieldCIWaitLedger is the resolver's own state: the JSON run ledger
	// (internal/ciwait.Ledger) for the wait's sha, mirrored onto the bead so a
	// bridge restart does not forget runs it already saw (task gd-WKrO492yYP).
	// Only the bridge resolver writes it, and the ledger carries its own key,
	// so a replaced wait never reads a stale one; that is why it is not in
	// CIWaitFields, which `gb wait ci` rewrites on every new wait.
	FieldCIWaitLedger = "ci_wait_ledger"

	// CIWaitStatePending: the agent is parked; nothing has concluded yet.
	CIWaitStatePending = "pending"
	// CIWaitStateResolved: every awaited run concluded (red or green).
	CIWaitStateResolved = "resolved"
	// CIWaitStateTimedOut: the deadline passed before CI concluded.
	CIWaitStateTimedOut = "timed_out"
	// CIWaitStateConsumed: the agent read the result (`gb wait ci --result`).
	CIWaitStateConsumed = "consumed"
	// CIWaitStateCancelled: the wait was dropped before it was read, by
	// `gb wait ci --cancel`, because its task closed, or because no CI will
	// ever run on its sha (gd-lHWcyCRj1Q). ci_wait_result holds the reason.
	// Like consumed it neither parks a stop nor wakes the agent.
	CIWaitStateCancelled = "cancelled"
)

// CIWaitFields lists every ci_wait_* key, in the order the plan documents them.
// The schema registration tests iterate it so a new key cannot be added here
// without being registered on type:agent.
var CIWaitFields = []string{
	FieldCIWaitState,
	FieldCIWaitKey,
	FieldCIWaitWorkflows,
	FieldCIWaitTask,
	FieldCIWaitDeadline,
	FieldCIWaitResult,
}

var (
	ciWaitRepoRe = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
	ciWaitSHARe  = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

// CIWaitKey validates repo (OWNER/REPO) and sha and returns the canonical
// FieldCIWaitKey value "OWNER/REPO@<sha>". The sha must be the FULL 40-hex
// commit sha (case-folded to lower): the Actions API matches head_sha exactly,
// so an abbreviated sha silently returns zero runs and the wait would never
// resolve (working-rules advice gd-oI2AIaSzjF).
func CIWaitKey(repo, sha string) (string, error) {
	repo = strings.TrimSpace(repo)
	sha = strings.ToLower(strings.TrimSpace(sha))
	if !ciWaitRepoRe.MatchString(repo) {
		return "", fmt.Errorf("repo %q must be OWNER/REPO", repo)
	}
	if !ciWaitSHARe.MatchString(sha) {
		return "", fmt.Errorf("sha %q must be the full 40-character commit sha (an abbreviated sha matches no runs)", sha)
	}
	return repo + "@" + sha, nil
}

// CIWait is the ci_wait_* view of an agent bead's fields.
type CIWait struct {
	State     string
	Key       string
	Workflows string
	Task      string
	Deadline  string
	Result    string
}

// CIWaitFromFields reads the ci_wait_* fields off an agent bead's field map.
func CIWaitFromFields(fields map[string]string) CIWait {
	return CIWait{
		State:     fields[FieldCIWaitState],
		Key:       fields[FieldCIWaitKey],
		Workflows: fields[FieldCIWaitWorkflows],
		Task:      fields[FieldCIWaitTask],
		Deadline:  fields[FieldCIWaitDeadline],
		Result:    fields[FieldCIWaitResult],
	}
}

// Pending reports whether the wait still awaits CI.
func (w CIWait) Pending() bool { return w.State == CIWaitStatePending }

// Concluded reports whether CI concluded (or the deadline passed) and the agent
// has not yet read the result.
func (w CIWait) Concluded() bool {
	return w.State == CIWaitStateResolved || w.State == CIWaitStateTimedOut
}
