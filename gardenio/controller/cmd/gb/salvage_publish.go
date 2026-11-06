package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// salvageWorkedTaskID resolves the worked task bead to comment on: BOAT_TASK_ID
// if set, else the agent's single in-progress claimed task. Returns "" when
// neither resolves (or the daemon is unreachable).
func salvageWorkedTaskID(ctx context.Context, explicit string) string {
	if explicit != "" {
		return explicit
	}
	if daemon != nil && actor != "" && actor != "unknown" {
		if task, err := daemon.ListAssignedTask(ctx, actor); err == nil && task != nil {
			return task.ID
		}
	}
	return ""
}

// commentSalvageFailed leaves a durable breadcrumb on the worked task when
// salvage found dirty work but could not persist ANY of it. preStop stderr is
// discarded, so this comment is the only audit channel for a total failure.
func commentSalvageFailed(ctx context.Context, dirtyTargets int, reason string) {
	if daemon == nil || dirtyTargets == 0 {
		return
	}
	taskID := salvageWorkedTaskID(ctx, os.Getenv("BOAT_TASK_ID"))
	if taskID == "" {
		return
	}
	comment := fmt.Sprintf(
		"⚠️ Salvage ATTEMPTED but FAILED: %d dirty worktree(s) could not be persisted to S3 (%s). "+
			"Uncommitted WIP may be lost on this pod teardown.", dirtyTargets, reason)
	if err := daemon.AddComment(ctx, taskID, "gardenio-salvage", comment); err != nil {
		fmt.Fprintf(os.Stderr, "[salvage] failure comment failed: %v\n", err)
	}
}

// commentSalvagedTask leaves a best-effort audit comment on the worked task
// bead pointing at the S3 salvage location and the restore command. Any daemon
// failure (unreachable on a teardown path, missing task id, unknown actor) is
// swallowed — the durable bundle is already written.
func commentSalvagedTask(ctx context.Context, m *salvageManifest) {
	if daemon == nil {
		return
	}
	taskID := salvageWorkedTaskID(ctx, m.TaskBead)
	if taskID == "" {
		return
	}
	var names []string
	for _, t := range m.Targets {
		if t.BundleKey != "" {
			names = append(names, t.Name)
		}
	}
	comment := fmt.Sprintf(
		"Killed mid-work (%s). Uncommitted WIP for %d worktree(s) [%s] salvaged to `%s`.\n\n"+
			"Recover with:\n```\n%s\n```",
		m.Reason, len(names), strings.Join(names, ", "), m.Prefix, m.Recover)
	if err := daemon.AddComment(ctx, taskID, "gardenio-salvage", comment); err != nil {
		fmt.Fprintf(os.Stderr, "[salvage] task comment failed: %v\n", err)
	}
}

// ── Shared tracked-only snapshot push (Tier 2 + Tier 3) ─────────────────────

// Benign "nothing to push" outcomes of pushTrackedSnapshot. Tier 2 treats them
// as a silent per-target skip; Tier 3 treats them as "push not possible" and
// falls back to the diff-comment path.
var (
	errPushNoOrigin   = errors.New("no origin remote")
	errPushUnbornHead = errors.New("unborn HEAD")
	errPushNoTracked  = errors.New("no tracked change to carry (untracked-only dirt)")
	errPushNoDelta    = errors.New("no tracked delta beyond the durable remote anchor")
)

// errGitleaksMissing is returned (wrapped) by gitleaksScan when the gitleaks
// binary could not be located: the load-bearing leak gate is absent, so
// anything that would leave the pod unscanned must be refused.
var errGitleaksMissing = errors.New("gitleaks not found on PATH")

// trackedPushSpec parameterises the one tracked-only snapshot push both
// preservation tiers run (pushTrackedSnapshot). Everything that differs between
// Tier 2 (refs/heads/salvage/..., opt-in) and Tier 3 (refs/gardenio/wip/...,
// unconditional) is a field here; everything else is shared.
type trackedPushSpec struct {
	gitleaksBin string
	gitleaksErr error  // exec.LookPath error for gitleaksBin; non-nil ⇒ fail closed
	dstRef      string // FULL destination ref, e.g. refs/heads/salvage/<bead>/<name>

	// requireTrackedChange (Tier 3) pre-checks, before snapshotting, that the
	// target's porcelain dirt or unpushed HEAD carries something a tracked-only
	// snapshot would capture (reapTargetHasTrackedChange).
	requireTrackedChange bool
	// requireAnchorDelta (Tier 2) post-checks that the snapshot differs from the
	// durable remote anchor; a branch identical to origin would be pointless.
	// Skipped when no anchor resolves (the remote state is then unknowable).
	requireAnchorDelta bool
}

// pushTrackedSnapshot is the shared body of the Tier-2 and Tier-3 pushes. For
// one target it: requires an origin remote and a born HEAD; builds a
// tracked-files-only snapshot (read-tree HEAD → add -u, NEVER -A → write-tree →
// commit-tree -p HEAD, so untracked novel-named files — the residual leak
// vector — are excluded by construction, independent of the gitleaks gate);
// resolves the durable remote anchor; gates the exact range that would transfer
// through gitleaks (FAIL CLOSED: a leak, a scan error, or a missing binary
// refuses); and force-pushes the snapshot to spec.dstRef with -o ci.skip and no
// MR. Returns the anchor-delta/benign-skip sentinels above, or a wrapped error
// when a guard tripped or git failed.
func pushTrackedSnapshot(ctx context.Context, t salvageTarget, spec trackedPushSpec) error {
	if _, err := gitCtx(ctx, t.Path, "remote", "get-url", "origin"); err != nil {
		return errPushNoOrigin
	}
	head, hasHead := worktreeHead(ctx, t.Path)
	if !hasHead {
		return errPushUnbornHead
	}
	if spec.requireTrackedChange && !reapTargetHasTrackedChange(ctx, t) {
		return errPushNoTracked
	}
	if spec.gitleaksErr != nil {
		return fmt.Errorf("%w: %v", errGitleaksMissing, spec.gitleaksErr)
	}

	salvageSHA, err := snapshotTrackedOnly(ctx, t.Path, t.Name, head)
	if err != nil {
		return fmt.Errorf("tracked-only snapshot: %w", err)
	}

	// Anchor on the first durable remote ref so the delta check and the gitleaks
	// scan cover only what a fresh clone lacks.
	_, anchorSHA := durableAnchor(ctx, t.Path)
	if spec.requireAnchorDelta && anchorSHA != "" {
		// `git diff --quiet` exits 0 (err == nil) when there is no delta.
		if _, err := gitCtx(ctx, t.Path, "diff", "--quiet", anchorSHA, salvageSHA); err == nil {
			return errPushNoDelta
		}
	}
	if err := gitleaksGate(ctx, spec.gitleaksBin, t.Path, anchorSHA, salvageSHA); err != nil {
		return fmt.Errorf("gitleaks gate refused push: %w", err)
	}

	// Explicit refspec (never bare), force (+) for idempotent re-runs, and
	// -o ci.skip + no MR so the push triggers zero CI.
	if _, err := gitCtx(ctx, t.Path, "push", "--no-verify", "-o", "ci.skip", "origin", "+"+salvageSHA+":"+spec.dstRef); err != nil {
		return fmt.Errorf("git push: %w", err)
	}
	return nil
}

// ── Tier-2: guarded quarantine push ─────────────────────────────────────────
//
// pushSalvageTargets mirrors each dirty target to a guarded salvage branch on
// the git remote, on top of the durable S3 bundle. Every guard fails CLOSED —
// if any precondition is unmet or any check errors, that target is skipped and
// the S3 bundle stands. The guards, in order:
//  1. gitleaks binary present: it is the load-bearing leak gate; absence
//     refuses every push (never push unscanned content).
//  2. per target: has an origin remote, has a born HEAD, and the tracked-only
//     snapshot differs from the durable remote anchor.
//  3. per target: a gitleaks scan of the EXACT commit range that would
//     transfer is clean (nonzero exit — a leak OR a scan error — refuses).
//
// Only then is the tracked-files-only snapshot force-pushed to an explicit
// refs/heads/salvage/<agent-bead>/<target> refspec (never bare, never main)
// with -o ci.skip and no merge request, so zero CI minutes are spent.
//
// There is deliberately NO "worked task already has an mr_url ⇒ skip" guard
// (removed by gd-Jpw0LU1Rgu; Tier 3 never had one). An mr_url says the
// COMMITTED work is on the MR branch; it says nothing about uncommitted edits
// or commits made since the last push, which are exactly what salvage exists to
// save. The push targets refs/heads/salvage/..., never the MR branch, so there
// is no race to avoid either.
func pushSalvageTargets(ctx context.Context, targets []salvageTarget) {
	// Guard 1 — gitleaks is the leak gate. If it is absent we FAIL CLOSED and
	// push nothing: the S3 bundle already captured the WIP durably, so refusing
	// an unscanned push loses nothing but the (redundant) remote mirror.
	gitleaksBin, err := exec.LookPath("gitleaks")
	if err != nil {
		fmt.Fprintf(os.Stderr, "[salvage] push: gitleaks not found on PATH; refusing quarantine push (fail-closed)\n")
		return
	}

	agentBead := agentS3SessionKey()
	var pushed []string
	for _, t := range targets {
		select {
		case <-ctx.Done():
			fmt.Fprintf(os.Stderr, "[salvage] push: deadline reached; skipping remaining targets\n")
			return
		default:
		}
		ref, err := pushSalvageTarget(ctx, gitleaksBin, agentBead, t)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[salvage] push %s: %v\n", t.Name, err)
			continue
		}
		if ref != "" {
			pushed = append(pushed, ref)
		}
	}
	if len(pushed) > 0 {
		fmt.Fprintf(os.Stderr, "[salvage] pushed %d quarantine branch(es): %s\n", len(pushed), strings.Join(pushed, ", "))
		commentSalvagePushed(ctx, pushed)
	}
}

// pushSalvageTarget force-pushes one target's gitleaks-clean tracked-only
// snapshot to refs/heads/salvage/<agent-bead>/<name> via pushTrackedSnapshot.
// Returns the pushed ref (relative to refs/heads/), "" when the target is
// skipped (no remote / unborn HEAD / no tracked delta), or an error when a
// guard tripped.
func pushSalvageTarget(ctx context.Context, gitleaksBin, agentBead string, t salvageTarget) (string, error) {
	ref := salvagePushRef(agentBead, t.Name)
	err := pushTrackedSnapshot(ctx, t, trackedPushSpec{
		gitleaksBin:        gitleaksBin,
		dstRef:             "refs/heads/" + ref,
		requireAnchorDelta: true,
	})
	switch {
	case err == nil:
		return ref, nil
	case errors.Is(err, errPushNoOrigin), errors.Is(err, errPushUnbornHead), errors.Is(err, errPushNoDelta):
		return "", nil // nothing to mirror; the Tier-1 bundle / Tier-3 scratch ref stand
	default:
		return "", err
	}
}

// snapshotTrackedOnly captures one worktree's TRACKED-file edits (never
// untracked files) into a detached salvage commit, without touching the real
// index/HEAD/branch. It is commitWorktreeSnapshot with `add -u`, so the
// resulting tree can only ever contain paths git already tracks — the invariant
// that makes the Tier-2/Tier-3 push safe to mirror to a remote. Requires a born
// HEAD (the caller guarantees it).
func snapshotTrackedOnly(ctx context.Context, path, name, head string) (string, error) {
	msg := fmt.Sprintf("wip(salvage-push): %s tracked-only", name)
	return commitWorktreeSnapshot(ctx, path, head, true, "-u", msg)
}

// gitleaksGate runs `gitleaks git` over the commit range that a push of
// salvageSHA would transfer and returns nil ONLY when the scan is clean. When
// anchorSHA is set it scans anchorSHA..salvageSHA (the delta a fresh clone
// lacks); otherwise it scans the full history of salvageSHA. It fails closed:
// any nonzero exit — a leak found OR a gitleaks/git error — returns an error so
// the caller refuses the push. --redact keeps any matched secret out of logs.
func gitleaksGate(ctx context.Context, gitleaksBin, path, anchorSHA, salvageSHA string) error {
	logOpts := salvageSHA
	if anchorSHA != "" {
		logOpts = anchorSHA + ".." + salvageSHA
	}
	cmd := exec.CommandContext(ctx, gitleaksBin, "git",
		"--no-banner", "--redact", "--log-opts", logOpts, path)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		// Do NOT echo gitleaks output (even redacted) into the error that may
		// reach a bead comment; a bare signal is enough to refuse.
		return fmt.Errorf("scan of range %s not clean (%v)", logOpts, err)
	}
	return nil
}

// salvagePushRef returns the salvage branch name (relative to refs/heads/) for a
// target: salvage/<agent-bead>/<name>, or salvage/<name> when no agent bead is
// in scope. The salvage/ prefix guarantees the push can never target main.
func salvagePushRef(agentBead, name string) string {
	if agentBead == "" {
		return "salvage/" + name
	}
	return "salvage/" + agentBead + "/" + name
}

// commentSalvagePushed leaves a best-effort audit comment on the worked task
// pointing at the pushed quarantine branch(es) and the fetch recipe. Swallows
// any daemon failure — the branches are already on the remote.
func commentSalvagePushed(ctx context.Context, refs []string) {
	if daemon == nil || len(refs) == 0 {
		return
	}
	taskID := salvageWorkedTaskID(ctx, os.Getenv("BOAT_TASK_ID"))
	if taskID == "" {
		return
	}
	comment := fmt.Sprintf(
		"Also mirrored WIP (tracked files only, gitleaks-clean, `-o ci.skip`, no MR) to %d quarantine branch(es): %s.\n\n"+
			"Recover with:\n```\ngit fetch origin '%s'\ngit checkout %s\n```",
		len(refs), strings.Join(refs, ", "), refs[0], refs[0])
	if err := daemon.AddComment(ctx, taskID, "gardenio-salvage", comment); err != nil {
		fmt.Fprintf(os.Stderr, "[salvage] push comment failed: %v\n", err)
	}
}
