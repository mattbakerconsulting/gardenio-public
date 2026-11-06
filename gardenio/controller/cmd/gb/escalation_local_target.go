package main

import (
	"errors"
	"strings"
)

// errLocalTargetNoRunner is gb's refusal of --execution-target local with no
// runner named (gd-iNgsblfqCx). The daemon refuses the same create with a 400
// (checkEscalationTargets, BEADS_ESCALATION_LOCAL_TARGET unset or enforce);
// gb says it first, and says what a crew asking the laptop coordinator to act
// should file instead.
const errLocalTargetNoRunner = `--execution-target local runs the escalation on one named machine, so it needs --target-runner <runner bead id> (gb runner status) or --target-owner oidc:<subject> (that owner's one online runner, resolved at approval).

Asking the operator or the laptop coordinator to act for you? Leave --execution-target unset: the escalation policy routes it (execute_by=coordinator:laptop) and the coordinator runs it where it can.`

// resolveEscalationLocalTarget settles where a new escalation runs from the
// three placement flags (gd-iNgsblfqCx). A runner flag with no
// --execution-target implies local, since a runner is only read for a local
// dispatch (escalationNamesRunner). Local with no runner named is refused. A
// --target-owner without the oidc: prefix gets it, because a runner's owner
// is stored as oidc:<subject> and the daemon matches it exactly.
func resolveEscalationLocalTarget(execTarget, targetRunner, targetOwner string) (target, owner string, implied bool, err error) {
	owner = strings.TrimSpace(targetOwner)
	if owner != "" && !strings.HasPrefix(owner, "oidc:") {
		owner = "oidc:" + owner
	}
	named := targetRunner != "" || owner != ""
	switch {
	case execTarget == "" && named:
		return "local", owner, true, nil
	case execTarget == "local" && !named:
		return "", "", false, errors.New(errLocalTargetNoRunner)
	}
	return execTarget, owner, false, nil
}
