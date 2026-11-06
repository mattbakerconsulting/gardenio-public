package server

import (
	"encoding/json"
	"slices"
	"strings"

	"gardenio/kbeads/internal/model"
)

func normOp(op string) string { return strings.ToLower(strings.TrimSpace(op)) }

// Verdict records (spec D9, owner question Q1): a reviewer's verdict on a PR,
// MR, or document. Never executed, never dispatched, never cascade.
var verdictOperationTypes = map[string]bool{
	"github_pr_merge": true, "gitlab_mr_merge": true, "github_pr_review": true, "doc_review": true,
}

func isVerdictOperation(op string) bool { return verdictOperationTypes[normOp(op)] }

// Bead-only operations (escalation routing Task 15, C2; P9.5 gd-q4ebFp0p4f):
// the approved work is only daemon writes to beads off the D3 floor, so
// coordinator:cluster may execute it. They are every type and alias of the
// registry classes the policy sends to that lane (D7 rule 4: bead_admin,
// bead_close), so plan lifecycle and dependency edits execute where they are
// routed, plus two spellings the registry does not know. A floor type in those
// classes (kbeads_role_update, user_identity_pin) is never bead-only. A filer
// can still pick the wrong type; what bounds the executor is its write grant
// (escalationExecutorGrant: approved_targets, executor types), not this name.
var beadOnlyOperationClasses = []string{"bead_admin", "bead_close"}

var beadOnlyUnregisteredTypes = map[string]bool{"release_record": true, "bead_label": true}

func isBeadOnlyOperation(op string) bool {
	op = normOp(op)
	if escalationFloorOperation(op) != "" {
		return false
	}
	return beadOnlyUnregisteredTypes[op] || slices.Contains(beadOnlyOperationClasses, operationClassOf(op))
}

// escalationExecutorTypes are the bead types an approval may stamp into
// approved_targets, and so the only types the escalation-executor grant
// reaches. Never config, role, agent, trigger, escalation, decision, policy,
// user, account or project beads (Task 15, C4).
var escalationExecutorTypes = map[model.BeadType]bool{
	model.TypeTask: true, model.TypeBug: true, model.TypeFeature: true, model.TypeChore: true, model.TypeEpic: true,
}

// escalationCloseOnlyTypes are the bead types an approval stamps into
// approved_targets only when the escalation is a bead-close type
// (beadCloseOperationTypes), and which the escalation-executor grant then
// admits for close alone: a plan's or report's narrative bundle, closed with
// the work it narrates. Without them every bundle close was handed up to the
// owner (gd-dNYrSP9KkW).
var escalationCloseOnlyTypes = map[model.BeadType]bool{model.TypeBundle: true}

// Operation types the daemon itself replays on approval (replayEscalation).
var serverReplayOperationTypes = map[string]bool{"create": true, "update": true, "close": true, "grant": true}

func isServerReplayOperation(op string) bool { return serverReplayOperationTypes[normOp(op)] }

// Replay-unfit types (decision gd-UQTnDZATTU, owner question Q6): no executor
// may run them from an escalation; a human runs them outside gardenio.
func isReplayUnfitOperation(op string) bool {
	op = normOp(op)
	return strings.HasPrefix(op, "terraform_") || strings.HasPrefix(op, "aws_") ||
		op == "coopmux_account_sync" || op == "git_push_public"
}

// escalationStringFields returns the string-valued fields of b.
func escalationStringFields(b *model.Bead) map[string]string {
	out := map[string]string{}
	if b == nil || len(b.Fields) == 0 {
		return out
	}
	var m map[string]any
	if json.Unmarshal(b.Fields, &m) != nil {
		return out
	}
	for k, v := range m {
		if s, ok := v.(string); ok {
			out[k] = s
		}
	}
	return out
}

// resultRenderField: the only keys a bridge may land through /result's
// "fields" (the escalation reaper's render and witness trail).
func resultRenderField(key string) bool {
	return key == "reaped" || strings.HasPrefix(key, "reaped_") ||
		strings.HasPrefix(key, "mr_") || strings.HasPrefix(key, "approvals_")
}
