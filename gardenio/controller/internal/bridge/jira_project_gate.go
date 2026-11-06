// Package bridge — coordinated outbound-Jira project gate.
//
// Every outbound Jira write the bridge makes is scoped to a Jira project key
// (the "PE" in "PE-1234"). Whether the bridge may write to a project is a
// deny-by-default decision: the project must be on an allow-list and must not
// be on a hard block-list. FSM is the customer-facing service desk and is the
// one project whose write-back the operator wants to flip atomically.
//
// # Why a single coordinated toggle (ka-0000001679)
//
// The plan (MR !4882, Matt Baker) is to stand up a separate *sandbox* Jira
// bridge that owns the FSM finite-state-machine (FSM) sync path. The cutover
// constraint is hard: the moment the sandbox bridge starts syncing FSM, the
// prod bridge must STOP — at the same instant, via one switch — or both
// bridges write FSM and customers get duplicate tickets/comments. A per-path
// flag would let the four write paths (jira_sync, jira_mentions,
// jira_squawk_forward, jira_poller) drift out of step; the audit (ka-0000001081)
// already found one such drift (the poller leak, ka-0000008042). So FSM
// write-back is governed by exactly ONE switch — fsmSyncEnabled — and a single
// *ProjectSyncGate value is shared by all four paths. Flip it and all four flip
// together.
//
// # Default-safe / additive
//
// fsmSyncEnabled defaults to false. With FSM disabled the gate's verdict is
// byte-for-byte the legacy deny-by-default behavior (FSM is in the block-list
// and never in the allow-list), so this change is purely additive: prod, which
// already hard-blocks FSM, sees no behavior change. The toggle only ever
// *grants* FSM write-back, and only when an operator explicitly opts in.
//
// # Rollback
//
// The toggle is wired from the JIRA_FSM_SYNC_ENABLED env var (default false).
// To roll back a sandbox cutover, set JIRA_FSM_SYNC_ENABLED=false (or unset it)
// on the prod jira-bridge Deployment and restart — prod resumes blocking FSM.
// Because it is one env var feeding one gate shared by all four paths, the
// rollback is atomic and total: there is no path that can keep writing FSM
// after the flip.
package bridge

import (
	"maps"
	"strings"
)

// ProjectSyncGate decides whether the bridge may make an outbound write to a
// given Jira issue's project. It is deny-by-default: a project must be on the
// allow-list and absent from the block-list, EXCEPT that FSM — the customer
// service desk — is governed solely by fsmSyncEnabled so it can be cut over
// atomically (see package doc). The zero value denies everything; construct via
// NewProjectSyncGate or DefaultProjectSyncGate.
//
// SUPERSEDED (ka-0000007252, then epic ka-0000002067): the live jira-bridge
// consults the config-driven WritePolicyStore (jira_write_policy.go), whose
// writable set is {PE, DEVOPS, GBFSM}. FSM was removed from that policy
// permanently — the bot holds no FSM write permission; FSM-bound writes flow
// through GBFSM outbox intents. This type + the blockedJiraProjects/
// allowedJiraProjects/fsmProjectKey constants below are DEAD IN THE PRODUCTION
// DECISION PATH; they are retained ONLY as the nil-WritePolicy fallback used by
// legacy/sibling unit tests (which still exercise the old FSM-toggle gate
// directly). Physically deleting them — and the JIRA_FSM_SYNC_ENABLED toggle
// path — is a fast-follow once the outbox is validated by E2E gate
// ka-0000000600. The fallback keeps FSM DENIED by default (FSM off), so it can
// never leak an FSM write in production even though the type still models one.
type ProjectSyncGate struct {
	// blocked is the hard block-list. A project here is denied regardless of
	// the allow-list — with the one exception of FSM, whose verdict is taken
	// over by fsmSyncEnabled.
	blocked map[string]bool
	// allowed is the allow-list. Deny-by-default means a project must appear
	// here to be writable.
	allowed map[string]bool
	// fsmSyncEnabled is the single coordinated toggle for FSM write-back. When
	// false (default) FSM is denied; when true FSM is permitted. This is the
	// only knob that lets FSM through, and the same gate value is shared by all
	// four outbound paths so the flip is atomic (ka-0000001679).
	fsmSyncEnabled bool
}

// fsmProjectKey is the customer-facing service-desk project governed by the
// coordinated toggle.
const fsmProjectKey = "FSM"

// NewProjectSyncGate builds a gate from explicit allow/block sets and the FSM
// toggle. The maps are copied so later mutation of the caller's maps cannot
// change the gate's verdicts. nil maps are treated as empty (deny-by-default).
func NewProjectSyncGate(blocked, allowed map[string]bool, fsmSyncEnabled bool) *ProjectSyncGate {
	return &ProjectSyncGate{
		blocked:        copyBoolSet(blocked),
		allowed:        copyBoolSet(allowed),
		fsmSyncEnabled: fsmSyncEnabled,
	}
}

// DefaultProjectSyncGate returns the production gate over the compiled-in
// allow/block sets (allow={PE,DEVOPS}, block={FSM}). fsmSyncEnabled wires the
// JIRA_FSM_SYNC_ENABLED operator toggle. With fsmSyncEnabled=false the verdict
// is identical to the legacy syncAllowedProject logic, so this is a drop-in.
func DefaultProjectSyncGate(fsmSyncEnabled bool) *ProjectSyncGate {
	return NewProjectSyncGate(blockedJiraProjects, allowedJiraProjects, fsmSyncEnabled)
}

// AllowProject reports whether the bridge may write to the project that owns
// jiraKey. It is the single chokepoint all four outbound paths consult.
//
// Verdict order:
//   - empty / malformed key ⇒ deny.
//   - FSM ⇒ governed SOLELY by fsmSyncEnabled (block/allow lists ignored for
//     FSM), so the operator's single toggle decides it.
//   - any other project ⇒ deny-by-default: must be on the allow-list and not on
//     the block-list.
func (g *ProjectSyncGate) AllowProject(jiraKey string) bool {
	if g == nil {
		// A nil gate is the safe-by-default value: fall back to the legacy
		// deny-by-default logic with FSM blocked.
		return DefaultProjectSyncGate(false).AllowProject(jiraKey)
	}
	project := jiraProjectKey(strings.TrimSpace(jiraKey))
	if project == "" {
		return false
	}
	if project == fsmProjectKey {
		return g.fsmSyncEnabled
	}
	if g.blocked[project] {
		return false
	}
	return g.allowed[project]
}

// FSMSyncEnabled reports whether the coordinated FSM toggle is on (for
// diagnostics / startup logging).
func (g *ProjectSyncGate) FSMSyncEnabled() bool {
	if g == nil {
		return false
	}
	return g.fsmSyncEnabled
}

// copyBoolSet returns a shallow copy of a string→bool set (nil-safe).
func copyBoolSet(in map[string]bool) map[string]bool {
	out := make(map[string]bool, len(in))
	maps.Copy(out, in)
	return out
}
