package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// ── Tier-3: unconditional reap-scratch preservation ─────────────────────────
//
// preserveReapScratch (bug gd-PF42yiyH1d) runs UNCONDITIONALLY on every dirty
// target discoverSalvageTargets finds — independent of S3 bucket config,
// --push, and GARDENIO_SALVAGE_PUSH. Tier 1 (S3 bundle) and Tier 2 (opt-in
// guarded remote push) both require configuration a crew pod may not have; in
// that default configuration a reap (OOM, spot reclaim, idle-out) used to lose
// a dirty tree exactly as if `gb salvage` did not exist.
//
// Where it sits in the run (gd-nKOOfEvKLt): runSalvage calls it AFTER the
// Tier-1 S3 bundle when a bucket is configured — the bundle is the higher-
// fidelity sink (tracked + untracked, `gb salvage restore`-able), so it gets
// first call on the preStop grace budget and this tier runs on what is left,
// including when every bundle uploaded and when the S3 client could not be
// built. With no bucket it runs first, with the whole budget, as the primary
// sink. For every target it tries, in order:
//
//  1. Scratch-ref push: a TRACKED-files-only snapshot (via pushTrackedSnapshot,
//     the same push Tier 2 runs — untracked files never leave the pod via this
//     path), gated through gitleaks against the durable remote anchor,
//     force-pushed to refs/gardenio/wip/<agent-bead>/<target>. Like Tier 2 it
//     has no mr_url skip: uncommitted edits are never on an MR branch.
//  2. Diff-comment fallback: when the push is impossible for any reason (no
//     origin remote, an unborn HEAD, no tracked delta to carry, missing
//     gitleaks, a leak found, or the push itself failing), attach a diff of
//     the target — tracked AND untracked files, since this is a bead comment
//     that is never pushed anywhere, so the tracked-only leak guard the
//     scratch-ref push relies on does not apply — to the AGENT bead, truncated
//     to reapDiffCommentCap. A bead comment is Dolt-durable and readable by
//     every holder of the shared beads token — a WIDER audience than a git
//     ref — so this fallback runs its OWN gitleaks gate (over the Tier-1
//     add -A snapshot, tracked and untracked) before posting: a leak, or a
//     missing gitleaks binary, withholds the diff entirely and posts a
//     "WIP withheld" notice naming the dirty files instead.
//
// Either outcome is recorded on the agent bead as wip_reap_ref /
// wip_reap_comment, which `gb session death-report --bead <agent-bead>`
// surfaces under "Uncommitted work preserved on reap". A clean tree sets
// neither field and creates neither a ref nor a comment (preserveReapScratch
// is simply never called — runSalvage returns before discovery finds nothing).
//
// The refs/gardenio/wip/ namespace is deliberately NOT refs/heads/ or
// refs/tags/ — the only two namespaces GitHub/GitLab's default branch/tag UI,
// `git branch -r`, and `git ls-remote --heads`/`--tags` walk — so a scratch
// ref is excluded from the repo's normal branch listing by construction, and
// can never collide with a real branch name. See
// gardenio/docs/reap-scratch-ref.md for the full recovery runbook.
const reapScratchRefNamespace = "refs/gardenio/wip/"

// reapDiffCommentCap bounds the diff-comment fallback body. 32 KiB comfortably
// holds a typical WIP diff while keeping the comment payload small.
const reapDiffCommentCap = 32 * 1024

func preserveReapScratch(ctx context.Context, targets []salvageTarget, agentBead string) {
	if len(targets) == 0 {
		return
	}
	gitleaksBin, gitleaksErr := exec.LookPath("gitleaks")

	var pushedRefs []string
	var commentedTargets []string
	for _, t := range targets {
		if ctx.Err() != nil {
			break // deadline reached; whatever remains is best-effort unpreserved
		}
		ref, err := pushReapScratchRef(ctx, gitleaksBin, gitleaksErr, agentBead, t)
		if err == nil && ref != "" {
			pushedRefs = append(pushedRefs, ref)
			continue
		}
		if commentReapDiff(ctx, gitleaksBin, gitleaksErr, agentBead, t) {
			commentedTargets = append(commentedTargets, t.Name)
		}
	}

	if agentBead == "" || daemon == nil {
		return
	}
	fields := map[string]string{}
	if len(pushedRefs) > 0 {
		fields["wip_reap_ref"] = strings.Join(pushedRefs, ", ")
	}
	if len(commentedTargets) > 0 {
		fields["wip_reap_comment"] = strings.Join(commentedTargets, ", ")
	}
	if len(fields) == 0 {
		return
	}
	if err := daemon.UpdateBeadFields(ctx, agentBead, fields); err != nil {
		fmt.Fprintf(os.Stderr, "[salvage] reap-scratch field update failed: %v\n", err)
	}
}

// reapTargetHasTrackedChange reports whether a target's dirt includes anything
// a tracked-files-only snapshot would actually capture: a tracked porcelain
// line (any status other than "??"), or a committed-but-unpushed HEAD (whose
// ancestry a tracked-only snapshot still carries forward). A target whose only
// dirt is untracked files has nothing for the scratch-ref push to preserve —
// only the diff-comment fallback (tracked AND untracked) can save it.
func reapTargetHasTrackedChange(ctx context.Context, t salvageTarget) bool {
	for _, line := range t.DirtyFiles {
		if len(line) >= 2 && line[:2] != "??" {
			return true
		}
	}
	return worktreeHasUnpushedCommits(ctx, t.Path)
}

// pushReapScratchRef attempts the Tier-3 scratch-ref push for one target.
// Returns the pushed ref (e.g. "refs/gardenio/wip/gd-agent1/work") on success,
// or ("", err) when the push was not attempted or failed — the caller falls
// back to the diff-comment path in every such case.
func pushReapScratchRef(ctx context.Context, gitleaksBin string, gitleaksErr error, agentBead string, t salvageTarget) (string, error) {
	ref := reapScratchRef(agentBead, t.Name)
	if err := pushTrackedSnapshot(ctx, t, trackedPushSpec{
		gitleaksBin:          gitleaksBin,
		gitleaksErr:          gitleaksErr,
		dstRef:               ref,
		requireTrackedChange: true,
	}); err != nil {
		return "", err
	}
	return ref, nil
}

// reapScratchRef returns the full scratch-ref path for a target:
// refs/gardenio/wip/<agent-bead>/<name>, or refs/gardenio/wip/<name> when no
// agent bead is in scope.
func reapScratchRef(agentBead, name string) string {
	if agentBead == "" {
		return reapScratchRefNamespace + name
	}
	return reapScratchRefNamespace + agentBead + "/" + name
}

// gitEmptyTreeSHA is git's well-known hash of the empty tree. Diffing against
// it yields the full content of a commit, independent of any parent — used as
// the diff base when no durable remote anchor resolves (see reapDiffRange).
const gitEmptyTreeSHA = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"

// commentReapDiff attaches a diff of t (tracked AND untracked files, via the
// Tier-1 snapshot commit t.SalvageSHA which was built with `add -A`) as a
// comment on the agent bead, truncated to reapDiffCommentCap. This is a bead
// comment, never pushed anywhere, so the tracked-only leak guard the
// scratch-ref push relies on does not apply — but a Dolt-durable, widely-
// readable bead comment is a WIDER audience than the git ref it stands in
// for, so this fallback runs its own gitleaks gate over the Tier-1 snapshot
// before posting anything: a leak, or a missing gitleaks binary, withholds
// the diff and posts a "WIP withheld" notice instead (see
// commentReapWithheld). Returns true iff a comment (diff or withheld notice)
// was posted.
func commentReapDiff(ctx context.Context, gitleaksBin string, gitleaksErr error, agentBead string, t salvageTarget) bool {
	if daemon == nil || agentBead == "" || t.SalvageSHA == "" {
		return false
	}
	if gitleaksErr != nil {
		return commentReapWithheld(ctx, agentBead, t, "gitleaks not found on PATH — refusing to post an unscanned diff")
	}
	if err := gitleaksGate(ctx, gitleaksBin, t.Path, t.AnchorSHA, t.SalvageSHA); err != nil {
		return commentReapWithheld(ctx, agentBead, t, "secret detected by gitleaks scan")
	}

	diff, err := reapDiffRange(ctx, t)
	if err != nil || strings.TrimSpace(diff) == "" {
		return commentReapDiffFailed(ctx, agentBead, t, err)
	}
	full := len(diff)
	diff = truncate(diff, reapDiffCommentCap)
	truncNote := "not truncated"
	if len(diff) < full {
		truncNote = fmt.Sprintf("truncated from %d KiB to the %d KiB cap — may not `git apply` cleanly",
			(full+1023)/1024, reapDiffCommentCap/1024)
	}
	comment := fmt.Sprintf(
		"⚠️ Pod reaped with a dirty tree at `%s` (target `%s`) that could not be pushed to a scratch ref. "+
			"Diff below (tracked + untracked files, %s):\n\n```diff\n%s\n```",
		t.Path, t.Name, truncNote, diff)
	if err := daemon.AddComment(ctx, agentBead, "gardenio-salvage", comment); err != nil {
		fmt.Fprintf(os.Stderr, "[salvage] reap diff comment failed: %v\n", err)
		return false
	}
	return true
}

// reapDiffRange computes the full diff the Tier-1 snapshot t.SalvageSHA
// carries beyond the durable remote, for the diff-comment fallback. When a
// remote anchor resolved at snapshot time it diffs anchorSHA..salvageSHA —
// the accumulated delta of every unpushed commit PLUS any working-tree
// edit, not just the one commit's own parent diff (bug: a clean tree with
// committed-but-unpushed history diffed against its own immediate parent
// shows an EMPTY diff and silently drops the WIP). With no anchor it diffs
// against git's empty-tree sha so the full snapshot content is still shown.
func reapDiffRange(ctx context.Context, t salvageTarget) (string, error) {
	base := t.AnchorSHA
	if base == "" {
		base = gitEmptyTreeSHA
	}
	return gitCtx(ctx, t.Path, "diff", "--no-color", base, t.SalvageSHA)
}

// reapDiffFiles renders the dirty-file list captured at snapshot time (paths
// only, no content) for the withheld/failed notices below.
func reapDiffFiles(t salvageTarget) string {
	var names []string
	for _, line := range t.DirtyFiles {
		if name := porcelainPath(line); name != "" {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return "(unknown files)"
	}
	return strings.Join(names, ", ")
}

// porcelainPath extracts the path from one `git status --porcelain` line
// (untrimmed — see worktreeDirtyFiles): 2 status chars, a space, then the
// path, or "old -> new" for a rename/copy, in which case it returns the new
// path.
func porcelainPath(line string) string {
	if len(line) < 4 {
		return ""
	}
	path := line[3:]
	if idx := strings.Index(path, " -> "); idx >= 0 {
		path = path[idx+len(" -> "):]
	}
	return path
}

// commentReapWithheld posts a notice that a diff was intentionally NOT
// attached because it could not be proven secret-free: a leak-positive
// gitleaks scan or a missing gitleaks binary. It names the dirty files (safe
// metadata already captured in the manifest) but never the diff content.
// Returns true iff the notice was posted — this still counts as "preserved
// on reap" for wip_reap_comment/death-report purposes, so the operator knows
// a target was reaped and its diff withheld, even though the diff itself
// could not travel through Dolt.
func commentReapWithheld(ctx context.Context, agentBead string, t salvageTarget, reason string) bool {
	if daemon == nil || agentBead == "" {
		return false
	}
	comment := fmt.Sprintf(
		"⚠️ Pod reaped with a dirty tree at `%s` (target `%s`) that could not be pushed to a scratch ref. "+
			"WIP withheld: %s. Affected files: %s.",
		t.Path, t.Name, reason, reapDiffFiles(t))
	if err := daemon.AddComment(ctx, agentBead, "gardenio-salvage", comment); err != nil {
		fmt.Fprintf(os.Stderr, "[salvage] reap withheld comment failed: %v\n", err)
		return false
	}
	return true
}

// commentReapDiffFailed records that BOTH the scratch-ref push and the
// diff-comment fallback failed to preserve anything for a target, instead of
// leaving the loss silent. Still posts a comment (and so still sets
// wip_reap_comment) so the death report surfaces the total loss rather than
// showing nothing at all.
func commentReapDiffFailed(ctx context.Context, agentBead string, t salvageTarget, cause error) bool {
	if daemon == nil || agentBead == "" {
		return false
	}
	reason := "diff was empty"
	if cause != nil {
		reason = cause.Error()
	}
	comment := fmt.Sprintf(
		"⚠️ Pod reaped with a dirty tree at `%s` (target `%s`) that could not be pushed to a scratch ref, "+
			"and no diff could be captured (%s). This WIP is UNRECOVERABLE. Affected files: %s.",
		t.Path, t.Name, reason, reapDiffFiles(t))
	if err := daemon.AddComment(ctx, agentBead, "gardenio-salvage", comment); err != nil {
		fmt.Fprintf(os.Stderr, "[salvage] reap-failure comment failed: %v\n", err)
		return false
	}
	return true
}

// ── git helpers (context-bounded, no-prompt) ────────────────────────────────
