package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/bridge"
)

// ── gitlab_mr_merge capability guard (ka-0000004913) ────────────────────────
//
// An escalation is a request for a human to do something the agent CANNOT do.
// A `gitlab_mr_merge` escalation for an MR the agent can merge itself is
// therefore not a request — it is noise, and it costs an operator a Slack ping
// plus a queue slot that never drains.
//
// This was not a theoretical concern. Measured 2026-08-16 across the whole open
// queue: of the 40 unique gardenio MRs carrying an open merge escalation, 39 were
// ALREADY MERGED, every one of them by the group bot's own auto-merge — i.e. the
// filing agent had merge rights the entire time and the human was never in the
// loop. Those 40 escalations were a third of all merge-escalation inflow and had
// to be swept by hand.
//
// A legitimate merge escalation is the mirror image: a protected branch whose
// merge_access_levels grant merge ONLY to a role the bot does not hold, so GitLab
// reports `user.can_merge=false` and `PUT /merge_requests/:iid/merge` genuinely
// refuses. The guard lets exactly that case (can_merge=false) through untouched.
//
// A SECOND legitimate class: the branch protection DOES grant the bot merge, so
// can_merge=true, but the MR carries a blocker only a human with more authority
// can clear. That is the whole non-gardenio fleet, and reading can_merge alone
// got it exactly backwards (bug ka-0000005160).
//
// MEASURED 2026-08-27 on example-org/monorepo. Protected `main` grants
// merge to BOTH {40, "Humans" group} AND {30, "Developers + Maintainers"}, and
// the bot is access_level 30 — so authorization genuinely clears (a bogus-SHA
// `PUT /merge` probe returned HTTP 409 "SHA does not match HEAD", not 401/403)
// and GitLab reports user.can_merge=TRUE. But monorepo also carries approval
// rules — {All Members, any_approver, 1} and {platform users only, regular, 1} —
// invisible to can_merge:
//
//	monorepo !2646 -> can_merge=true, merge_status=can_be_merged,
//	                  detailed_merge_status=not_approved, approvals_left=1
//	monorepo !2643 -> identical
//
// The bot cannot self-approve, so that approval IS a real operator gate. An
// earlier revision of this comment concluded from the 409 alone that "the guard
// CORRECTLY refuses a monorepo gitlab_mr_merge escalation" — that probe tested
// authorization only and never asked whether the MR was mergeable. gardenio
// carries ZERO approval rules, which is why the difference never showed up there.
//
// The consequence was a DEADLOCK with mr_dispatch_gate.go, which already encodes
// the right model (dispatchGateSelfMergeableRepos = {gardenio}) and tells an
// agent working a monorepo MR that it needs an operator — while this guard
// refused to let it file one, printing "YOU CAN MERGE monorepo!2646 YOURSELF".
// 121 of 127 open fleet MRs are non-gardenio, so that stranded essentially the
// entire gardener-delivery merge queue.
//
// So the guard asks GitLab TWO questions on a single-MR fetch: `user.can_merge`
// (does my role permit merging this branch?) and `detailed_merge_status` (is
// anything blocking it, and can I clear that myself?). It refuses only when the
// first is true AND the second names no operator-only blocker.
//
// Design rule: FAIL OPEN. Every unknown — no token, no parseable target, an API
// error, a nil permission block — proceeds with the escalation. A guard that
// wrongly blocks a legitimate escalation is far worse than one that lets noise
// through, because the blocked case is a human waiting on work that never gets
// asked for. Only a positive, unambiguous "yes, this token can merge this MR
// right now" refuses.
//
// KNOWN LIMITATION — can_merge reflects ROLE, not TOKEN SCOPE. Verified against
// gitlab.com on 2026-08-16: a `read_api`-only PAT belonging to a Maintainer still
// reports `user.can_merge=true`, because GitLab answers from the user's project
// role and protected-branch access and never consults the token's scopes. So the
// guard can refuse an agent whose role permits the merge but whose token cannot
// perform it. That is why the refusal message hands back the exact glab command
// and tells the agent to escalate with the REAL error if it fails — which is the
// behaviour we want anyway (advice ka-0000000752: attempt the privileged action
// first, read the actual error, then escalate). --skip-merge-capability-check is
// the explicit bypass for that case.

// skipMergeCapabilityCheck is bound to --skip-merge-capability-check, the
// escape hatch for the case the guard gets wrong: GitLab reporting can_merge
// where the merge in fact fails. Kept so a mistaken guard can never permanently
// block a legitimate ask.
var skipMergeCapabilityCheck bool

// mergeGuardDecision is the outcome of evaluating a would-be gitlab_mr_merge
// escalation against live GitLab state.
type mergeGuardDecision struct {
	// Refuse is true when the escalation should NOT be created.
	Refuse bool
	// Reason is the operator/agent-facing explanation. Non-empty when Refuse
	// is true, or when proceeding with a caveat worth printing to stderr.
	Reason string
	// Warn is true when the guard could not reach a verdict and is letting the
	// escalation through — printed as a WARNING, never fatal.
	Warn bool
}

// mergeTarget is the project/MR a gitlab_mr_merge escalation is about.
type mergeTarget struct {
	ProjectPath string
	IID         int
	BaseURL     string // derived from an MR URL in the spec, when present
}

// parseMergeTarget pulls the target MR out of an --operation-spec object.
//
// The key spellings below are not hypothetical variants — they are the ones
// agents actually wrote across the open queue: `mr_iid`, `merge_request_iid`,
// `mr_url`, and `url`. A spec that names none of them yields ok=false and the
// guard fails open rather than guessing from the title.
func parseMergeTarget(specObj any) (mergeTarget, bool) {
	spec, ok := specObj.(map[string]any)
	if !ok {
		return mergeTarget{}, false
	}

	// A full MR URL is the richest form: it carries host + project + iid, so it
	// also gives us a base URL for self-hosted GitLab instances.
	for _, key := range []string{"mr_url", "url", "merge_request_url"} {
		raw, _ := spec[key].(string)
		if raw == "" {
			continue
		}
		if ref := bridge.ParseMRURL(raw); ref != nil {
			return mergeTarget{
				ProjectPath: ref.ProjectPath,
				IID:         ref.IID,
				BaseURL:     baseURLFromGitLabURL(raw),
			}, true
		}
	}

	project, _ := spec["project"].(string)
	if project == "" {
		return mergeTarget{}, false
	}
	for _, key := range []string{"mr_iid", "merge_request_iid", "iid"} {
		if iid, ok := numericField(spec[key]); ok {
			return mergeTarget{ProjectPath: project, IID: iid}, true
		}
	}
	return mergeTarget{}, false
}

// numericField reads an IID that may arrive as a JSON number (float64 after a
// generic unmarshal) or as a string, since both spellings appear in the wild.
func numericField(v any) (int, bool) {
	switch t := v.(type) {
	case float64:
		if t <= 0 {
			return 0, false
		}
		return int(t), true
	case int:
		if t <= 0 {
			return 0, false
		}
		return t, true
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(t))
		if err != nil || n <= 0 {
			return 0, false
		}
		return n, true
	}
	return 0, false
}

// evaluateMergeGuard turns a fetched MR (or a fetch failure) into a decision.
// Split out from the network call so the policy is unit-testable without GitLab.
func evaluateMergeGuard(target mergeTarget, mr *bridge.GitLabMR, fetchErr error) mergeGuardDecision {
	if fetchErr != nil {
		return mergeGuardDecision{Warn: true, Reason: fmt.Sprintf(
			"could not verify merge capability for %s!%d (%v) — creating the escalation anyway",
			target.ProjectPath, target.IID, fetchErr)}
	}
	if mr == nil {
		return mergeGuardDecision{Warn: true, Reason: fmt.Sprintf(
			"GitLab returned no merge request for %s!%d — creating the escalation anyway",
			target.ProjectPath, target.IID)}
	}

	// An MR that is already merged or closed needs nothing from an operator.
	// This is the duplicate-handle case: 17% of the open queue was a second
	// escalation filed against an MR that had already landed (ka-0000006698).
	switch mr.State {
	case "merged":
		return mergeGuardDecision{Refuse: true, Reason: fmt.Sprintf(
			"%s!%d is ALREADY MERGED — there is nothing for an operator to do.\n"+
				"If follow-up work remains, file it as a task rather than a merge escalation.",
			target.ProjectPath, target.IID)}
	case "closed":
		return mergeGuardDecision{Refuse: true, Reason: fmt.Sprintf(
			"%s!%d is CLOSED (not merged) — reopen it, or file a task, rather than escalating a merge.",
			target.ProjectPath, target.IID)}
	}

	// nil User means the permission block was absent (older GitLab, a list-shaped
	// payload, a stubbed client). Unknown is not "not allowed" — proceed.
	if mr.User == nil {
		return mergeGuardDecision{Warn: true, Reason: fmt.Sprintf(
			"GitLab did not report merge permissions for %s!%d — creating the escalation anyway",
			target.ProjectPath, target.IID)}
	}

	// can_merge=false is the classic legitimate escalation: the branch grants
	// merge only to a role the bot does not hold. Nothing more to check.
	if !mr.User.CanMerge {
		return mergeGuardDecision{}
	}

	// can_merge=true says only "my ROLE permits merging this branch". Before
	// telling the agent to do it itself, check whether anything is blocking the
	// MR that the agent cannot clear on its own (bug ka-0000005160).
	if blocker, ok := operatorOnlyMergeBlockers[mr.DetailedMergeStatus]; ok {
		return mergeGuardDecision{Warn: true, Reason: fmt.Sprintf(
			"%s!%d reports user.can_merge=true, but detailed_merge_status=%s — %s.\n"+
				"That is a REAL operator gate, not over-escalation, so this escalation is being created.\n"+
				"can_merge answers 'does my role permit merging this branch', NOT 'is this MR mergeable':\n"+
				"approval rules, requested changes and security policies are invisible to it.",
			target.ProjectPath, target.IID, mr.DetailedMergeStatus, blocker)}
	}

	// can_merge=true and no operator-only blocker: the agent either can merge
	// right now, or is blocked by something it must clear itself (rebase, CI,
	// conflicts, unresolved threads, its own draft flag). Either way a human has
	// nothing to do. Name the status so the agent knows which.
	return mergeGuardDecision{Refuse: true, Reason: fmt.Sprintf(
		"YOU CAN MERGE %s!%d YOURSELF — GitLab reports user.can_merge=true for this token%s,\n"+
			"so an escalation would ask a human for something you are already allowed to do.\n\n"+
			"user.can_merge is NOT merely 'push access' and NOT a project-role guess: GitLab\n"+
			"evaluates it for THIS token against the project role AND the target branch's\n"+
			"protected-branch merge_access_levels. A true value therefore already answers the\n"+
			"question 'may I merge protected main in this project'. Do not talk yourself out of\n"+
			"it — that exact rationalization ('the capability guard reports the push-access\n"+
			"can_merge, not protected-main merge authority') is how this escalation class kept\n"+
			"reaching operators who had nothing to do (ka-0000007570).\n\n"+
			"This guard already checked for the blockers a human WOULD have to clear\n"+
			"(%s) and found none — so any remaining blocker is\n"+
			"yours to clear: rebase, wait for CI, fix conflicts, resolve threads, un-draft.\n\n"+
			"Do this instead:\n"+
			"  glab api -X PUT projects/%s/merge_requests/%d/merge \\\n"+
			"    -f squash=true -f should_remove_source_branch=true -f merge_when_pipeline_succeeds=true\n\n"+
			"If that call actually fails, escalate with the REAL error in --context, or re-run with\n"+
			"--skip-merge-capability-check to bypass this guard.",
		target.ProjectPath, target.IID, detailedStatusSuffix(mr.DetailedMergeStatus),
		operatorOnlyMergeBlockerList(),
		pathEncode(target.ProjectPath), target.IID)}
}

// operatorOnlyMergeBlockers maps a GitLab `detailed_merge_status` to a plain
// description of why an AGENT cannot clear it. These are the states where
// can_merge=true and the MR is still un-mergeable by the bot alone, so a
// gitlab_mr_merge escalation is legitimate and must not be refused.
//
// Deliberately narrow. Everything absent from this map — ci_still_running,
// need_rebase, conflict, draft_status, discussions_not_resolved, checking,
// unchecked, preparing, mergeable, and the empty string (older GitLab that does
// not return the field at all) — is either already mergeable or blocked by
// something the agent itself must fix, and still earns the refusal.
//
// gardenio carries ZERO approval rules and no security policies, so no gardenio
// MR can land in this map: the standing self-merge rule (advice ka-0000002230)
// is unaffected by this carve-out.
var operatorOnlyMergeBlockers = map[string]string{
	"not_approved":      "the MR needs an approval, and the bot cannot approve its own MR",
	"requested_changes": "a reviewer has requested changes, which only that reviewer can withdraw",
	"policies_denied":   "a merge-request approval policy is denying the merge",
}

// operatorOnlyMergeBlockerList renders the map's keys in a stable order for the
// refusal message, so the agent can see exactly which states were checked.
func operatorOnlyMergeBlockerList() string {
	keys := make([]string, 0, len(operatorOnlyMergeBlockers))
	for k := range operatorOnlyMergeBlockers {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return "detailed_merge_status=" + strings.Join(keys, "/")
}

// detailedStatusSuffix appends the observed detailed_merge_status to the refusal
// so the verdict is auditable against live GitLab state. Empty when GitLab did
// not report the field.
func detailedStatusSuffix(status string) string {
	if status == "" {
		return ""
	}
	return " (detailed_merge_status=" + status + ")"
}

// pathEncode percent-encodes a project path for use as a GitLab :id path
// segment (group/sub/repo -> group%2Fsub%2Frepo), matching what the suggested
// glab command needs.
func pathEncode(p string) string {
	return strings.ReplaceAll(p, "/", "%2F")
}

// checkMergeEscalationCapability runs the guard for a gitlab_mr_merge escalation.
// Returns a non-nil error ONLY when the escalation should be refused; any
// warning is printed by the caller. A nil GitLab client (no token / no base URL)
// short-circuits to "proceed", keeping the guard inert wherever it cannot see.
//
// requestingTask is the --requesting-task bead id (may be empty). It is the
// second place the guard looks for a target MR: when the spec names none, a
// gitlab_mr_merge escalation almost always still points at an MR via the
// requesting bead's `mr_url` field (that is how `gb mr link` and the whole
// escalation-on-open-MR workflow record it). Recovering the target from there
// closes the bypass in ka-0000003288 — stripping --operation-spec used to make
// the guard fail open silently, which made deleting information the cheapest way
// past a refusal.
func checkMergeEscalationCapability(ctx context.Context, specObj any, requestingTask string) mergeGuardDecision {
	target, ok := parseMergeTarget(specObj)
	if !ok {
		// No machine-readable target in the spec. Before failing open, try to
		// recover the MR from the requesting task's mr_url — a spec-less merge
		// escalation still names its MR there in the common case.
		recovered, rok := recoverTargetFromRequestingTask(ctx, requestingTask)
		if !rok {
			// The spec does name its PR, just not a GitLab one: a GitHub PR
			// (gardenio's home since 2026-09). This GitLab-only guard has nothing
			// to check, and the "names no MR … add mr_url" warning below would be
			// false — it is exactly what made crews re-file every merge
			// escalation with an mr_url they had already given (gd-f7GvZ7qahY).
			if spec, _ := specObj.(map[string]any); specPRURL(spec) != "" {
				return mergeGuardDecision{}
			}
			// Genuinely no target anywhere. Warn (not silent) so the gap is at
			// least visible in the record — a gitlab_mr_merge escalation with no
			// discoverable MR cannot be machine-linked to what it is asking for.
			return mergeGuardDecision{Warn: true, Reason: "gitlab_mr_merge escalation names no MR " +
				"(spec has no mr_url/project+iid, and --requesting-task carries no mr_url) — " +
				"creating it anyway, but it cannot be checked for self-mergeability or linked to its MR.\n" +
				"Add the MR to --operation-spec (e.g. {\"mr_url\":\"…\"}) so the guard and queue tooling can see it."}
		}
		target = recovered
	}

	client, err := newMRGitLabClient(target.BaseURL, "https://gitlab.com")
	if err != nil || client == nil {
		return mergeGuardDecision{Warn: true, Reason: fmt.Sprintf(
			"no GitLab client available to verify merge capability for %s!%d — creating the escalation anyway",
			target.ProjectPath, target.IID)}
	}

	mr, fetchErr := client.GetMergeRequestByPath(ctx, target.ProjectPath, target.IID)
	return evaluateMergeGuard(target, mr, fetchErr)
}

// recoverTargetFromRequestingTask looks up the requesting task and pulls a merge
// target out of its `mr_url` field. This is the fallback the guard uses when the
// --operation-spec named no MR: a gitlab_mr_merge escalation is filed against an
// open MR, and that MR is recorded on the requesting bead (via `gb mr link`),
// so the information is almost always present even when the spec omits it.
//
// It fails soft in every direction — empty id, no daemon, fetch error, absent or
// unparseable mr_url all yield ok=false and let the caller fall through to the
// visible fail-open warning. It NEVER blocks on its own; only a subsequent live
// can_merge=true / already-merged verdict does.
func recoverTargetFromRequestingTask(ctx context.Context, requestingTask string) (mergeTarget, bool) {
	if requestingTask == "" || daemon == nil {
		return mergeTarget{}, false
	}
	bead, err := daemon.GetBead(ctx, requestingTask)
	if err != nil || bead == nil {
		return mergeTarget{}, false
	}
	rawURL := mrURLFromBead(bead)
	if !mrURLIsParseable(rawURL) {
		return mergeTarget{}, false
	}
	return targetFromMRURL(rawURL), true
}

// mrURLFromBead reads a merge-request URL off a bead's fields, tolerating the
// two spellings that appear in the wild: `mr_url` (the canonical field set by
// `gb mr link`) and `merge_request_url`.
func mrURLFromBead(bead *beadsapi.BeadDetail) string {
	if bead == nil || bead.Fields == nil {
		return ""
	}
	for _, key := range []string{"mr_url", "merge_request_url"} {
		if v := strings.TrimSpace(bead.Fields[key]); v != "" {
			return v
		}
	}
	return ""
}

// targetFromMRURL turns an MR URL into a mergeTarget, or the zero value if it
// does not parse. Pairs with mrURLIsParseable so callers can distinguish
// "no url" from "url but unparseable".
func targetFromMRURL(rawURL string) mergeTarget {
	ref := bridge.ParseMRURL(rawURL)
	if ref == nil {
		return mergeTarget{}
	}
	return mergeTarget{
		ProjectPath: ref.ProjectPath,
		IID:         ref.IID,
		BaseURL:     baseURLFromGitLabURL(rawURL),
	}
}

// mrURLIsParseable reports whether rawURL is a GitLab MR URL the guard can act
// on. A non-empty but unparseable url (e.g. a shortlink or a typo) is treated
// as "no target" so the guard fails open visibly rather than fetching garbage.
func mrURLIsParseable(rawURL string) bool {
	return bridge.ParseMRURL(rawURL) != nil
}

// decodeOperationSpec re-parses the already-validated --operation-spec JSON into
// a generic map for the guard. The create path holds it as `any` after
// json.Unmarshal, so this is a no-op passthrough for that shape and a tolerant
// fallback for a raw string.
func decodeOperationSpec(v any) any {
	if s, ok := v.(string); ok {
		var out any
		if err := json.Unmarshal([]byte(s), &out); err == nil {
			return out
		}
		return nil
	}
	return v
}
