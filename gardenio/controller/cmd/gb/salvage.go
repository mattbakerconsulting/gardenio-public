package main

// salvage.go — `gb salvage`: end-of-life WIP rescue for involuntarily-killed
// agent pods (bug ka-0000000146, design bundle ka-0000007429).
//
// Crew pods use EmptyDir (gardenio defaults.go): the worktree — and any *local*
// git commit/stash in it — dies with the pod. Only S3-durable or
// remote-durable state survives. When an agent is killed mid-work by an
// involuntary infra termination (controller spec-drift drain, account
// rotation, node drain, spot reclaim, OOM), the SIGTERM path today salvages
// only coop's stdout tail — the agent's uncommitted work product evaporates.
//
// TIER-2 QUARANTINE PUSH (opt-in --push, Phase 4 / design §5): on top of the
// S3 bundle, `gb salvage --push` optionally mirrors the WIP to the git remote
// so a successor can `git fetch` it without S3 access at all. It is guarded to
// be safe-by-construction and CI-free:
//   - tracked-files-only (`git add -u`, NEVER `git add -A`) — untracked,
//     novel-named files an agent wrote into the tree (the residual leak vector,
//     since real creds live in $HOME outside the worktree) are never pushed.
//   - a `gitleaks` scan of the exact commit range that would transfer gates the
//     push and FAILS CLOSED: a leak-positive scan — or a missing gitleaks
//     binary — refuses the push (the S3 bundle is still written).
//   - an explicit `refs/heads/salvage/<agent-bead>/<target>` refspec — never a
//     bare push, never main/master.
//   - `-o ci.skip` and NO merge request, so zero CI minutes are spent: both
//     ci.yml and docker.yml gate the pipeline on branch==main, and a salvage/*
//     branch with no MR triggers nothing.
// There is no `mr_url` skip: an MR branch holds only COMMITTED, PUSHED work, so a
// worked task with an mr_url can still carry uncommitted edits or unpushed
// commits that only salvage can save (gd-Jpw0LU1Rgu).
//
// `gb salvage` closes that gap. It is invoked FIRST from the pod's preStop
// hook (podmanager/manager.go), before coop's drain + S3 finalize consume the
// 45s grace budget, so it runs at t≈0 with the worktree intact. For every
// dirty git worktree — the primary /work checkout, each per-bead worktree under
// ../worktrees/* (sibling of the primary checkout; legacy .beads/worktrees/*
// scanned too for one release), AND each nested reference repo under repos/*
// (see the nested-repo note below) — it:
//
//  1. Snapshots the full working tree (tracked edits + untracked files,
//     .gitignore honored) into a DETACHED salvage commit built in a throwaway
//     index — never touching the agent's real index, HEAD, or branch pointer.
//  2. `git bundle`s that commit — incremental against the first durable remote
//     anchor (origin/main|master|HEAD) that resolves, else a self-contained
//     full bundle — and PUTs it to
//     s3://<COOP_S3_BUCKET>/<COOP_S3_PREFIX>/<agent-bead>/<session>/salvage/<target>.bundle
//     reusing the exact newS3Client/s3Bucket/s3Prefix/agentS3SessionKey
//     plumbing the stdout-tail upload uses on this path. The KEY is a stable
//     per-AGENT-bead prefix (<prefix>/<bead>/<bead>/salvage/ on the preStop
//     path, where COOP_S3_SESSION_KEY is unset), NOT the per-spawn coop-UUID
//     leaf the stdout-tail upload keys on — so restore is predictable from just
//     the agent bead id.
//  3. Writes an aggregate salvage-manifest.json alongside the bundles so a
//     successor/operator can recover exactly (recovery recipe documented in
//     the manifest and runnable via `gb salvage restore`).
//  4. Best-effort comments on the worked task bead pointing at the S3 location
//     and the restore command.
//
// It is strictly time-boxed (context.WithTimeout, default 25s), non-fatal
// (RunE always returns nil so the preStop `|| true` is belt-and-suspenders),
// and idempotent (stable S3 keys → re-runs overwrite in place). git is invoked
// with --no-optional-locks and GIT_TERMINAL_PROMPT=0 so a stuck lock or a
// credential prompt can never hang inside the grace window.
//
// A correctness note on the bundle anchor (verified by round-trip): bundling
// `HEAD..salvage` is WRONG when HEAD is an unpushed local commit — HEAD then
// becomes a bundle *prerequisite* that no fresh clone has, so the bundle is
// unrecoverable (the WIP is exactly what we are trying to save). We anchor on a
// durable REMOTE sha (`^<anchor-sha>`) instead, or emit a self-contained full
// bundle when no remote anchor resolves.
//
// THE CLEAN-TREE-BUT-UNPUSHED-COMMIT GAP (bug ka-0000005216, Track B of plan
// ka-0000004007 — the crux of the 2026-07-22 Patrick incident, now CLOSED).
// Discovery used to gate ONLY on a dirty working tree (`git status --porcelain`
// non-empty). An agent that COMMITTED its edits and then died before pushing
// left a CLEAN tree — so discovery found nothing, and salvage wrote nothing to
// S3, even though a whole commit of real work was about to evaporate with the
// EmptyDir. That is exactly what happened to Patrick's agent: it committed its
// three !1160 edits (clean tree), was killed, and had to redo everything from
// memory on respawn. The fix: discovery now salvages a worktree when it is dirty
// OR carries an unpushed HEAD (worktreeHasUnpushedCommits: `git rev-list -n 1
// HEAD --not --remotes` returns non-empty). The primary checkout uses
// isSalvageablePrimary and per-bead/nested worktrees use salvageWorktreeHasWIP;
// both fold in the unpushed-HEAD check, so a committed-but-unpushed HEAD is
// salvaged on every target class. Restore is still `gb salvage restore`; the
// respawn-time auto-restore that fetches these bundles is Track B2
// (ka-0000003478).
//
// WHICH DEATHS REACH THIS CODE (drain-coverage note, Track B3). This runs from
// the pod preStop hook (podmanager/manager.go), which K8s fires ONLY on a
// GRACEFUL teardown — a pod delete with a termination grace period (controller
// spec-drift drain, node cordon+drain, rollout). It does NOT fire on a hard
// SIGKILL: an OOM kill (gVisor OOMs the container at its memory limit) or an
// abrupt node loss (spot reclaim without a drain, instance failure) skips
// preStop entirely, so nothing salvages those. Reducing how OFTEN those
// forceful kills happen is the sibling plan ka-0000004429 (node-drain nudge +
// churn reduction), and making them OBSERVABLE is Track D (OOMKill +
// node-lifecycle Slack alerts, ka-0000001509). Within the graceful-teardown
// class this hook covers, the clean-tree fix above closes the committed-work
// loss window; the residual uncovered window is exactly the SIGKILL class those
// complementary tracks address.
//
// A correctness note on NESTED repos (bug ka-0000001959): a crew agent's real
// edits often live in a nested working repo (a reference repo cloned at
// <workspace>/repos/<name>, or a project checked out under <workspace>/repos/*).
// Each nested repo has its OWN .git. If we only snapshot the outer workspace
// tree, git records the nested repo as a GITLINK (a bare submodule-commit
// pointer, mode 160000) — its objects are NEVER bundled, so restore fetches the
// outer refs cleanly while the actual WIP branch/commit is unrecoverable
// (`git cat-file -t <nested-sha>` ⇒ "could not get object info"). A false-
// positive success that loses data. So discoverSalvageTargets scans repos/* and
// snapshots every dirty nested repo as its OWN salvage target ("repos-<name>"),
// landing that repo's objects in their own bundle. repos/ is checked both
// alongside the workspace root (the standard crew layout: <project>/work is the
// salvage root and <project>/repos is its sibling) and directly under it.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/spf13/cobra"
)

// salvageDefaultTimeout bounds the whole salvage run. The preStop hook budgets
// ~25s for salvage before it moves on to coop drain; the command self-enforces
// the same ceiling so a hung git can never eat the K8s grace period.
const salvageDefaultTimeout = 25 * time.Second

// salvageBundleCap is the per-target bundle size ceiling. Above this the bundle
// is skipped (logged in the manifest) rather than risk a slow multi-hundred-MB
// PutObject blowing the grace window. 64 MiB comfortably holds source WIP; a
// tree that large is almost certainly build output that should not be salvaged.
const salvageBundleCap = 64 * 1024 * 1024

// salvageManifestVersion identifies the manifest schema for the restore path.
const salvageManifestVersion = 1

// salvageTarget describes one salvaged worktree in the manifest.
type salvageTarget struct {
	Name        string   `json:"name"`                   // stable identifier used in the bundle key (e.g. "work" or the bead ID)
	Path        string   `json:"path"`                   // absolute worktree path at salvage time
	Branch      string   `json:"branch"`                 // checked-out branch (or "(detached)"/"(unborn)")
	HeadSHA     string   `json:"head_sha"`               // worktree HEAD at salvage time ("" if unborn)
	AnchorRef   string   `json:"anchor_ref,omitempty"`   // durable remote ref the bundle is incremental against ("" ⇒ self-contained)
	AnchorSHA   string   `json:"anchor_sha,omitempty"`   // resolved sha of AnchorRef
	SalvageSHA  string   `json:"salvage_sha"`            // the detached salvage commit captured in the bundle
	BundleKey   string   `json:"bundle_key,omitempty"`   // S3 key of the .bundle ("" if not uploaded)
	BundleBytes int64    `json:"bundle_bytes,omitempty"` // size of the uploaded bundle
	DirtyFiles  []string `json:"dirty_files,omitempty"`  // `git status --porcelain` lines, for the human reading the manifest
	Skipped     string   `json:"skipped,omitempty"`      // non-empty reason when this target was not uploaded
}

// salvageManifest is the aggregate JSON written next to the per-target bundles.
type salvageManifest struct {
	Version    int             `json:"version"`
	Agent      string          `json:"agent"`      // KD_ACTOR / agent name
	AgentBead  string          `json:"agent_bead"` // KD_AGENT_ID
	TaskBead   string          `json:"task_bead,omitempty"`
	Session    string          `json:"session"` // COOP_S3_SESSION_KEY / agent-bead key used in the S3 path
	Prefix     string          `json:"prefix"`  // s3://<bucket>/<prefix>/<agent-bead>/<session>/salvage/
	Bucket     string          `json:"bucket"`
	Reason     string          `json:"reason"`
	CreatedRFC string          `json:"created,omitempty"` // RFC3339 stamp (best-effort; omitted when clock unavailable)
	Targets    []salvageTarget `json:"targets"`
	Recover    string          `json:"recover"` // human recovery recipe
}

var salvageCmd = &cobra.Command{
	Use:   "salvage",
	Short: "Bundle dirty worktrees to S3 before an involuntary pod kill (best-effort)",
	Long: `Salvage rescues uncommitted work from a dying agent pod.

For every dirty git worktree — the primary /work checkout, each per-bead
worktree under ../worktrees/*, AND each nested reference repo under repos/*
(which the outer checkout only records as a gitlink) — it captures the full
working tree into a detached salvage commit, writes a git bundle +
salvage-manifest.json to S3, and comments the location on the worked task. It
never mutates the agent's branch, HEAD, or index.

Designed to run FIRST in the pod preStop hook. It is strictly time-boxed,
non-fatal (always exits 0), and idempotent. When no S3 bucket is configured or
no worktree is dirty it is a quiet no-op.

With --push it ALSO mirrors each salvaged worktree to a guarded
salvage/<agent-bead>/<target> branch on the git remote (tracked files only,
behind a gitleaks scan, -o ci.skip, never main, never an MR). A leak-positive
scan, a missing gitleaks binary, or a worked task that already has an MR refuses
the push — the S3 bundle is always written regardless.

Recover with:  gb salvage restore --agent-bead <gd-…> [--session <key>]`,
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, _ []string) error {
		timeout, _ := cmd.Flags().GetDuration("timeout")
		reason, _ := cmd.Flags().GetString("reason")
		push, _ := cmd.Flags().GetBool("push")
		// The tier is also enablable fleet-wide via a truthy GARDENIO_SALVAGE_PUSH
		// env var, so an operator can flip on the guarded push at EOL by setting
		// one env var on agent pods — the preStop hook already runs `gb salvage`,
		// so no controller redeploy or preStop edit is needed to roll it out.
		if !push && salvagePushEnvEnabled() {
			push = true
		}
		workspace := salvageWorkspaceRoot(cmd)

		ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
		defer cancel()

		// Always exit 0: this runs on a teardown path where a non-zero exit
		// would only add noise to the preStop `|| true`. Errors are logged.
		if err := runSalvage(ctx, workspace, reason, salvageOpts{push: push}); err != nil {
			fmt.Fprintf(os.Stderr, "[salvage] %v\n", err)
		}
		return nil
	},
}

func init() {
	salvageCmd.Flags().Duration("timeout", salvageDefaultTimeout, "hard deadline for the whole salvage run")
	salvageCmd.Flags().String("reason", "involuntary_termination", "reason recorded in the manifest and task comment")
	salvageCmd.Flags().String("workspace", "", "workspace root to scan (default: BOAT_WORKSPACE / primary-repo layout / cwd)")
	salvageCmd.Flags().Bool("push", false, "also mirror each salvaged worktree to a guarded salvage/<agent-bead>/<target> remote branch (tracked-only, gitleaks-gated, -o ci.skip, never main, never an MR)")

	salvageCmd.AddCommand(salvageRestoreCmd)
}

// salvageOpts carries the tunable behaviour of a salvage run. It exists so the
// Tier-1 (S3 bundle) contract stays a stable no-arg default while Tier-2
// (guarded remote push) is threaded through as an opt-in.
type salvageOpts struct {
	push bool // also attempt the guarded Tier-2 quarantine push
}

// salvagePushEnvEnabled reports whether GARDENIO_SALVAGE_PUSH is set to a truthy
// value (1/true/yes/on, case-insensitive). Lets the tier be turned on fleet-wide
// via pod env without editing the preStop hook or the controller.
func salvagePushEnvEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("GARDENIO_SALVAGE_PUSH"))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// salvageWorkspaceRoot resolves the primary workspace root to scan. Precedence:
//  1. --workspace flag (tests / manual runs).
//  2. BOAT_WORKSPACE env (set on agent pods).
//  3. /home/agent/bot/<BOAT_PROJECT>/work (the standard crew layout).
//  4. cwd.
func salvageWorkspaceRoot(cmd *cobra.Command) string {
	if cmd != nil {
		if ws, _ := cmd.Flags().GetString("workspace"); ws != "" {
			return ws
		}
	}
	if ws := os.Getenv("BOAT_WORKSPACE"); ws != "" {
		return ws
	}
	if project := os.Getenv("BOAT_PROJECT"); project != "" {
		primary := filepath.Join("/home/agent/bot", project, "work")
		if st, err := os.Stat(primary); err == nil && st.IsDir() {
			return primary
		}
	}
	cwd, _ := os.Getwd()
	return cwd
}

// runSalvage discovers dirty targets, bundles each to S3, writes the manifest,
// and comments the worked task. When opts.push is set it additionally attempts
// a guarded Tier-2 quarantine push per target (see pushSalvageTargets). It
// returns an error only for a whole-run failure the caller should log;
// per-target failures are captured in the manifest and never abort the run.
//
// The push tier does NOT require an S3 bucket: when no bucket is configured but
// --push is set, the run still discovers dirty targets and attempts the guarded
// remote push (the only remaining durable sink). The S3-bundle path is skipped
// in that case.
func runSalvage(ctx context.Context, workspace, reason string, opts salvageOpts) error {
	// Clean-exit guard (bug ka-0000001049). The pod preStop hook runs `gb salvage`
	// on EVERY teardown — including the one that follows a successful `gb stop`
	// (the agent closes its own bead, the reconciler scales the pod down, and THAT
	// deletion fires this hook). A voluntarily-stopped agent has no lost WIP: its
	// workspace holds only agent-infra state (.claude/ .gb/ .mcp.json .state/ and
	// the nested read-only reference clones under repos/), which `git status`
	// reports as untracked. Salvaging it re-uploads hundreds of MB of pristine
	// clones to S3 and posts a FALSE "Killed mid-work (involuntary_termination)"
	// notice on the agent's parent epic (241 on one epic before this fix — ~89% of
	// its comment volume, and it masks the REAL involuntary-kill signal). This is a
	// clean-exit-vs-crash confusion, independent of WIP detection, so we skip the
	// whole path — S3 bundle AND push tier — when the agent exited voluntarily.
	// Fail-OPEN (see salvageIsCleanExit): a genuine involuntary kill, whose bead
	// is still open at preStop time, still salvages.
	if salvageIsCleanExit(ctx) {
		fmt.Fprintln(os.Stderr, "[salvage] agent exited voluntarily (clean gb stop/suspend); skipping salvage")
		return nil
	}

	started := time.Now()

	targets := discoverSalvageTargets(ctx, workspace)
	if len(targets) == 0 {
		return nil // no dirty worktree → nothing to salvage.
	}

	agentBead := agentS3SessionKey()

	// Tier ordering (gd-nKOOfEvKLt, PR #10 review item 3). The S3 bundle
	// (Tier 1) is the highest-fidelity sink — tracked AND untracked files,
	// round-trippable with `gb salvage restore` — so when a bucket is configured
	// it gets first call on the grace budget. Tier 3 (scratch-ref push / diff
	// comment) used to run BEFORE it and could spend the whole 15s budget on a
	// slow push or a full-history gitleaks scan, leaving the bundle to hit the
	// deadline before its first PUT. Tier 3 now runs AFTER the bundle, on
	// whatever budget remains, and FIRST — with the whole budget — only when
	// there is no bucket, where it is the primary sink. It stays unconditional
	// either way: it runs when every bundle uploaded, and when the S3 client
	// could not even be built — a crew pod must never lose a dirty tree because
	// S3 happened to be down at preStop.
	bucket := s3Bucket()
	var bundleErr error
	if bucket != "" {
		bundleErr = bundleSalvageToS3(ctx, started, bucket, reason, agentBead, targets)
	}

	// Tier 3 (gd-PF42yiyH1d): unconditional reap-scratch preservation. Runs on
	// every dirty target found above, independent of S3 bucket config, --push,
	// and GARDENIO_SALVAGE_PUSH — a crew pod with none of that configured must
	// still not lose a dirty tree on reap. See preserveReapScratch.
	preserveReapScratch(ctx, targets, agentBead)

	// Tier-2: optional guarded quarantine push. It runs with no S3 sink (the
	// remote is then the only durable destination) and when the bundle failed:
	// bundleErr only means S3 was unusable, which is exactly when a second
	// durable copy matters most.
	if opts.push {
		pushSalvageTargets(ctx, targets)
	}
	return bundleErr
}

// bundleSalvageToS3 is Tier 1: it bundles every target to
// s3://<bucket>/<prefix>/<target>.bundle, writes the aggregate manifest, pushes
// the run's metrics and comments on the worked task. It returns an error only
// when the S3 client could not be built (nothing was attempted); per-target
// upload failures are recorded in the manifest and surfaced through
// commentSalvageFailed instead, and never abort the run.
func bundleSalvageToS3(ctx context.Context, started time.Time, bucket, reason, agentBead string, targets []salvageTarget) error {
	session := os.Getenv("COOP_S3_SESSION_KEY")
	if session == "" {
		session = agentBead
	}
	prefix := salvagePrefix(s3Prefix(), agentBead, session)

	client, err := newS3Client(ctx)
	if err != nil {
		// The S3 sink is unreachable but the beads daemon may not be — leave a
		// durable failure breadcrumb on the worked task so the loss is visible
		// (preStop stderr is discarded, so a comment is the only audit channel).
		commentSalvageFailed(ctx, len(targets), fmt.Sprintf("s3 client init failed: %v", err))
		pushSalvageMetrics(ctx, salvageRunStats{
			outcome: "failed", targets: len(targets), duration: time.Since(started),
		})
		return fmt.Errorf("s3 client: %w", err)
	}

	manifest := salvageManifest{
		Version:   salvageManifestVersion,
		Agent:     envOr("KD_ACTOR", os.Getenv("BOAT_AGENT")),
		AgentBead: agentBead,
		TaskBead:  os.Getenv("BOAT_TASK_ID"),
		Session:   session,
		Prefix:    fmt.Sprintf("s3://%s/%s/", bucket, prefix),
		Bucket:    bucket,
		Reason:    reason,
		Recover:   salvageRecoverRecipe(agentBead, session),
	}
	if stamp := salvageTimestamp(); stamp != "" {
		manifest.CreatedRFC = stamp
	}

	var uploaded int
	for _, t := range targets {
		select {
		case <-ctx.Done():
			t.Skipped = "deadline reached before upload"
			manifest.Targets = append(manifest.Targets, t)
			continue
		default:
		}
		key := fmt.Sprintf("%s/%s.bundle", prefix, t.Name)
		if err := bundleAndUpload(ctx, client, bucket, key, &t); err != nil {
			t.Skipped = err.Error()
		} else {
			t.BundleKey = key
			uploaded++
		}
		manifest.Targets = append(manifest.Targets, t)
	}

	// Write the manifest even when every bundle was skipped — it records what
	// was attempted and why, which is itself useful post-mortem signal.
	if err := uploadManifest(ctx, client, bucket, prefix, &manifest); err != nil {
		fmt.Fprintf(os.Stderr, "[salvage] manifest upload failed: %v\n", err)
	}

	fmt.Fprintf(os.Stderr, "[salvage] %d/%d worktree(s) bundled to s3://%s/%s/\n",
		uploaded, len(targets), bucket, prefix)

	var bundleBytes int64
	for _, t := range manifest.Targets {
		bundleBytes += t.BundleBytes
	}
	outcome := "success"
	switch {
	case uploaded == 0:
		outcome = "failed"
	case uploaded < len(targets):
		outcome = "partial"
	}
	pushSalvageMetrics(ctx, salvageRunStats{
		outcome:  outcome,
		targets:  len(targets),
		uploaded: uploaded,
		bytes:    bundleBytes,
		duration: time.Since(started),
	})

	if uploaded > 0 {
		commentSalvagedTask(ctx, &manifest)
	} else {
		// Every target was skipped (S3 denied every PUT, all bundles hit the
		// size cap, or the deadline bit before any upload). preStop stderr is
		// discarded, so surface the failure durably on the worked task.
		commentSalvageFailed(ctx, len(targets), "every worktree was skipped (see stderr / manifest for per-target reasons)")
	}
	return nil
}

// salvageIsCleanExit reports whether the agent this pod runs exited VOLUNTARILY
// (a successful `gb stop`/`gb yield`/freeze suspend) rather than being killed
// mid-work — the discriminator for the bug ka-0000001049 false-positive.
//
// It reads the agent bead's live state, which the two exit paths leave in
// distinct shapes at preStop time:
//
//   - Voluntary `gb stop`: the agent closes its OWN bead first (stop.go →
//     CloseBead with agent_state=done). Only THEN does the reconciler scale the
//     pod down, whose deletion fires this preStop hook. So a clean stop presents
//     a bead already status=closed with agent_state=done.
//   - Voluntary freeze/suspend (`gb stop` on a freeze-eligible agent): the bead
//     is parked OPEN at agent_state=stopping (the AgentStop trigger) and the
//     controller checkpoints + tears the pod down. That teardown also fires this
//     hook, but the agent is resumable by design — nothing was lost.
//   - Involuntary kill (node drain / Spot reclaim / rollout / OOM): the pod is
//     SIGTERM'd with the bead still OPEN and agent_state=working; the involuntary
//     close happens LATER in the entrypoint restart loop after coop exits
//     (init_agent_loop.go closeAgentBeadBestEffort). So a real mid-work kill
//     presents an open, non-stopping bead here — and DOES salvage.
//
// Fail-OPEN by construction: any inability to prove a clean exit (no agent bead
// in scope, daemon unreachable, GET error, unexpected state) returns false, so a
// genuine involuntary kill is never denied salvage on account of a flaky read.
// The cost of a false negative is one skipped-but-harmless salvage of pristine
// infra state; the cost of a false positive is lost WIP — so we bias hard toward
// still-salvaging when unsure.
func salvageIsCleanExit(ctx context.Context) bool {
	if daemon == nil {
		return false
	}
	agentBead := agentS3SessionKey()
	if agentBead == "" {
		return false
	}
	// A short, independent deadline so a slow daemon on the teardown path can
	// never eat into salvage's own grace budget: fail-open (proceed to salvage)
	// rather than block.
	gctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	bead, err := daemon.GetBead(gctx, agentBead)
	if err != nil || bead == nil {
		return false
	}
	state := bead.Fields["agent_state"]
	// Terminal clean stop: the agent closed its own bead to done.
	if bead.Status == "closed" && state == "done" {
		return true
	}
	// Voluntary freeze/suspend: parked open at stopping, resumable by design.
	if bead.Status != "closed" && state == "stopping" {
		return true
	}
	return false
}

// discoverSalvageTargets walks the workspace for git worktrees carrying WIP: the
// primary checkout (unless it is a pristine main), every per-bead worktree
// under ../worktrees/* (legacy .beads/worktrees/* too for one release), and
// every nested reference repo under repos/*
// (bug ka-0000001959 — a nested repo the outer tree only records as a gitlink,
// whose objects would otherwise never be bundled). "WIP" is EITHER a dirty
// working tree (tracked edits / untracked files) OR a committed-but-not-yet-
// pushed HEAD (bug ka-0000005216): an agent that `git commit`ed its work and
// died before `git push` leaves a spotless `git status`, yet the commit lives
// only in the dying EmptyDir pod — so a clean tree alone must NOT skip a target
// that carries unpushed commits. Each returned target has its snapshot commit +
// dirty file list already captured; bundling/upload happens in the caller so a
// discovery-time git failure on one target cannot abort the rest.
func discoverSalvageTargets(ctx context.Context, workspace string) []salvageTarget {
	var out []salvageTarget

	// Primary checkout. Its stable bundle name is "work" so recovery is
	// predictable regardless of the on-disk directory name.
	if isSalvageablePrimary(ctx, workspace) {
		if t, ok := snapshotWorktree(ctx, workspace, "work"); ok {
			out = append(out, t)
		}
	}

	// Per-bead worktrees. Their directory name is the bead ID, which is a
	// stable and meaningful bundle name. Worktrees now live at the SIBLING
	// location <project>/worktrees/<bead-id> (bug ka-0000007049); the legacy
	// nested layout <workspace>/.beads/worktrees/<bead-id> is scanned too for
	// ONE release so a pod still on the old layout, or killed mid-migration,
	// still has its dirty WIP bundled. De-dup by cleaned path (a symlink or
	// identical resolution is scanned once) and by bundle name (the bead ID)
	// so the same bead cannot be bundled twice.
	seenWtDir := map[string]bool{}
	seenBead := map[string]bool{}
	for _, wtDir := range []string{
		worktreesDir(workspace),
		filepath.Join(workspace, ".beads", "worktrees"),
	} {
		clean := filepath.Clean(wtDir)
		if seenWtDir[clean] {
			continue
		}
		seenWtDir[clean] = true

		entries, err := os.ReadDir(clean)
		if err != nil {
			continue
		}
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			if e.IsDir() {
				names = append(names, e.Name())
			}
		}
		sort.Strings(names) // deterministic order for stable manifests/tests.
		for _, name := range names {
			if seenBead[name] {
				continue
			}
			path := filepath.Join(clean, name)
			if !salvageWorktreeHasWIP(ctx, path) {
				continue
			}
			if t, ok := snapshotWorktree(ctx, path, name); ok {
				seenBead[name] = true
				out = append(out, t)
			}
		}
	}

	// Nested reference repos under repos/*. On real crew pods the salvage root
	// is <project>/work and reference repos are cloned as its SIBLING at
	// <project>/repos/<name> (initclone.go). A repo checked out nested inside
	// the workspace at <workspace>/repos/<name> is possible too. Scan both so a
	// nested repo's own git objects land in their OWN bundle instead of being
	// silently reduced to an outer-tree gitlink. Bundle names are namespaced
	// "repos-<name>" to stay collision-safe against "work"/bead-id targets.
	out = append(out, discoverNestedRepoTargets(ctx, workspace)...)

	return out
}

// discoverNestedRepoTargets snapshots every dirty git repo directly under a
// repos/ directory adjacent to (sibling of) or inside the workspace root. A
// nested repo carries its own .git, so it must be bundled as its own target —
// the outer workspace tree records it only as a gitlink (mode 160000) whose
// objects no bundle of the outer tree contains (bug ka-0000001959).
func discoverNestedRepoTargets(ctx context.Context, workspace string) []salvageTarget {
	var out []salvageTarget

	// Candidate repos/ dirs, in a deterministic order. The sibling layout
	// (<workspace>/../repos) is the standard crew layout; the nested layout
	// (<workspace>/repos) covers repos checked out inside the checkout. De-dup
	// by cleaned path so a symlink or identical resolution is only scanned once.
	seen := map[string]bool{}
	reposDirs := []string{
		filepath.Join(filepath.Dir(workspace), "repos"),
		filepath.Join(workspace, "repos"),
	}
	for _, reposDir := range reposDirs {
		clean := filepath.Clean(reposDir)
		if seen[clean] {
			continue
		}
		seen[clean] = true

		entries, err := os.ReadDir(clean)
		if err != nil {
			continue
		}
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			// A repo may be a real dir or a symlink to one; stat-follow via
			// os.Stat below. Skip plain files.
			if e.IsDir() || e.Type()&os.ModeSymlink != 0 {
				names = append(names, e.Name())
			}
		}
		sort.Strings(names) // deterministic order for stable manifests/tests.
		for _, name := range names {
			path := filepath.Join(clean, name)
			// Must itself be a git worktree (own .git dir or gitlink file).
			if _, err := os.Stat(filepath.Join(path, ".git")); err != nil {
				continue
			}
			if !salvageWorktreeHasWIP(ctx, path) {
				continue
			}
			if t, ok := snapshotWorktree(ctx, path, "repos-"+name); ok {
				out = append(out, t)
			}
		}
	}
	return out
}

// salvageInfraPaths is the set of top-level agent-infra paths a provisioned
// agent workspace always carries as UNTRACKED (bug ka-0000001049). None is work
// product:
//   - .claude/    agent config
//   - .gb/        system-prompt.md + gb runtime state
//   - .mcp.json   MCP server config
//   - .state/     claude/claude-code/coop runtime state
//   - repos/      nested read-only reference clones (each already salvaged as its
//     OWN target by discoverNestedRepoTargets when genuinely dirty)
//
// A gardener workspace has no commits, so every one of these reads as untracked
// WIP forever — which made the primary checkout salvage (and post a false
// "Killed mid-work" notice) on every clean stop. isSalvageablePrimary excludes
// this set so pristine infra state alone never counts as salvageable primary
// WIP; a real tracked edit or an untracked file OUTSIDE this set still does.
var salvageInfraPaths = map[string]bool{
	".claude":   true,
	".gb":       true,
	".mcp.json": true,
	".state":    true,
	"repos":     true,
}

// isSalvageablePrimary reports whether the primary checkout is worth salvaging:
// it must be a git worktree carrying REAL WIP, which is EITHER
//   - working-tree WIP: a tracked modification (staged or unstaged) or an
//     untracked path OUTSIDE the agent-infra set (salvageInfraPaths), OR
//   - a committed-but-not-yet-pushed HEAD (bug ka-0000005216): the agent
//     `git commit`ed its work and died before `git push`, so the working tree is
//     clean yet the commit lives only in the dying pod.
//
// A checkout whose only "dirt" is pristine agent-infra state (the shape of every
// clean gardener workspace) with a fully-pushed HEAD is NOT salvageable: it
// carries no work to lose (bug ka-0000001049). A dirty main — or a main with
// unpushed commits — is still salvaged; the agent may have worked directly on
// main.
func isSalvageablePrimary(ctx context.Context, workspace string) bool {
	if _, err := os.Stat(filepath.Join(workspace, ".git")); err != nil {
		return false
	}
	return primaryHasRealWIP(ctx, workspace) || worktreeHasUnpushedCommits(ctx, workspace)
}

// primaryHasRealWIP reports whether `git status --porcelain` on the primary
// checkout shows any change that is NOT confined to the agent-infra path set.
// Tracked modifications (any porcelain status other than "??") always count;
// untracked entries ("?? path") count only when their top-level path component
// is outside salvageInfraPaths. Returns false on a git error (same fail-quiet
// contract as salvageWorktreeDirty) or when every dirty line is infra-only.
func primaryHasRealWIP(ctx context.Context, workspace string) bool {
	out, err := gitCtx(ctx, workspace, "--no-optional-locks", "status", "--porcelain")
	if err != nil {
		return false
	}
	for line := range strings.SplitSeq(out, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if salvagePorcelainLineIsRealWIP(line) {
			return true
		}
	}
	return false
}

// salvagePorcelainLineIsRealWIP classifies one `git status --porcelain` line.
// Porcelain v1 format is a 2-char status ("XY"), a space, then the path (a
// rename/copy is "R  old -> new"). Any tracked change (status != "??") is real
// WIP. An untracked line ("?? path") is real WIP only when the path's first
// component is outside the agent-infra set. Paths are unquoted best-effort; a
// line we cannot parse is treated as real WIP (fail toward salvaging).
func salvagePorcelainLineIsRealWIP(line string) bool {
	if len(line) < 4 {
		return true // too short to be a real porcelain entry; don't drop it
	}
	status := line[:2]
	path := strings.TrimSpace(line[3:])
	if status != "??" {
		return true // any tracked modification/staged change is real WIP
	}
	// Untracked: infra-only if its top-level component is in the infra set.
	return !salvagePathIsInfra(path)
}

// salvagePathIsInfra reports whether a worktree-relative path's FIRST component
// is one of the agent-infra paths. A trailing slash (git prints untracked dirs
// as "repos/") and surrounding quotes (git quotes paths with special chars) are
// stripped first. The first path component is what git reports for an untracked
// directory, so ".state/foo/bar" and "repos/" both match on their head.
func salvagePathIsInfra(path string) bool {
	path = strings.Trim(path, `"`)
	head := path
	if i := strings.IndexByte(head, '/'); i >= 0 {
		head = head[:i]
	}
	return salvageInfraPaths[head]
}

// salvageWorktreeDirty is a ctx-bounded, --no-optional-locks dirtiness probe.
// It replaces the package isDirtyWorktree (unbounded plain exec.Command, no
// --no-optional-locks) for the discovery phase so the FIRST git run per target
// can never hang past the grace window on a stuck index.lock.
func salvageWorktreeDirty(ctx context.Context, path string) bool {
	out, err := gitCtx(ctx, path, "--no-optional-locks", "status", "--porcelain")
	if err != nil {
		return false
	}
	return strings.TrimSpace(out) != ""
}

// salvageWorktreeHasWIP reports whether a worktree carries any salvageable WIP:
// a dirty working tree OR a committed-but-not-yet-pushed HEAD (bug
// ka-0000005216). It is the discovery gate for per-bead and nested-repo targets
// — a clean tree alone is no longer enough to skip one, because the agent may
// have committed its work and died before pushing. (The primary checkout uses
// the richer isSalvageablePrimary gate, which additionally ignores pristine
// agent-infra dirt.)
func salvageWorktreeHasWIP(ctx context.Context, path string) bool {
	return salvageWorktreeDirty(ctx, path) || worktreeHasUnpushedCommits(ctx, path)
}

// worktreeHasUnpushedCommits reports whether HEAD carries any commit that is not
// reachable from ANY remote-tracking ref — i.e. committed work that lives only
// in this (soon-to-die, EmptyDir) pod and would be lost on teardown (bug
// ka-0000005216). It runs `git rev-list -n 1 HEAD --not --remotes`: any output
// means at least one unpushed commit exists.
//
// It is deliberately biased toward OVER-reporting, in the same fail-toward-
// salvaging spirit as the rest of discovery:
//   - An unborn HEAD (no commits) makes rev-list error → false (nothing to lose).
//   - A crew workspace's narrow fetch refspec tracks only origin/main
//     (advice ka-0000001569), so a genuinely-pushed feature branch has no local
//     origin/<branch> ref and reads as "unpushed" here. The cost is a redundant
//     S3 bundle of already-durable work — cheap, and exactly the right bias for a
//     data-loss-prevention path. (The remote pushes cannot tell either and
//     carry no mr_url skip, so they may mirror an already-pushed branch.)
//   - A git error (stuck lock, detached-in-a-weird-state) → false, matching the
//     fail-quiet contract of salvageWorktreeDirty.
func worktreeHasUnpushedCommits(ctx context.Context, path string) bool {
	out, err := gitCtx(ctx, path, "--no-optional-locks", "rev-list", "-n", "1", "HEAD", "--not", "--remotes")
	if err != nil {
		return false
	}
	return strings.TrimSpace(out) != ""
}

// snapshotWorktree captures the full working tree of one dirty worktree into a
// detached salvage commit, WITHOUT touching the real index/HEAD/branch. It uses
// a throwaway GIT_INDEX_FILE: read-tree HEAD → add -A → write-tree →
// commit-tree -p HEAD. When HEAD is unborn (no commits yet) it builds a
// parentless commit from the staged tree. Returns (target, false) when the
// worktree is not salvageable (e.g. write-tree fails).
func snapshotWorktree(ctx context.Context, path, name string) (salvageTarget, bool) {
	t := salvageTarget{Name: name, Path: path, Branch: worktreeBranch(ctx, path)}
	t.DirtyFiles = worktreeDirtyFiles(ctx, path)

	head, hasHead := worktreeHead(ctx, path)
	t.HeadSHA = head

	session := os.Getenv("COOP_S3_SESSION_KEY")
	if session == "" {
		session = agentS3SessionKey() // preStop path leaves COOP_S3_SESSION_KEY unset; fall back to the agent bead
	}
	msg := fmt.Sprintf("wip(salvage): %s @ %s", name, session)
	// add -A: tracked edits AND untracked files (.gitignore honored) — the full
	// tree the S3 bundle and the diff-comment fallback carry.
	salvageSHA, err := commitWorktreeSnapshot(ctx, path, head, hasHead, "-A", msg)
	if err != nil {
		return t, false
	}
	t.SalvageSHA = salvageSHA

	// Choose the bundle anchor: the first durable REMOTE ref that resolves.
	// Anchoring on a remote sha keeps the bundle small AND recoverable in a
	// fresh clone. Anchoring on local HEAD would be unrecoverable when HEAD is
	// itself an unpushed commit.
	if ref, sha := durableAnchor(ctx, path); sha != "" {
		t.AnchorRef, t.AnchorSHA = ref, sha
	}
	return t, true
}

// bundleAndUpload creates the git bundle for a snapshotted target and PUTs it to
// S3 under key. It writes a temporary detached salvage ref so `git bundle` has a
// named ref to package, then deletes that ref (the bundle retains the object).
// The agent branch pointer is never touched.
func bundleAndUpload(ctx context.Context, client *s3.Client, bucket, key string, t *salvageTarget) error {
	if t.SalvageSHA == "" {
		return fmt.Errorf("no salvage commit")
	}
	ref := "refs/salvage/" + t.Name
	if _, err := gitCtx(ctx, t.Path, "update-ref", ref, t.SalvageSHA); err != nil {
		return fmt.Errorf("stage salvage ref: %w", err)
	}
	// Delete the temp ref even if the parent ctx already expired, but under a
	// FRESH short deadline: context.WithoutCancel alone strips the deadline too,
	// leaving the git subprocess with no wall-clock bound — a hang on a
	// packed-refs.lock would then outlive salvage's own 25s ceiling with only
	// the K8s SIGKILL as backstop (which also aborts coop's drain).
	defer func() {
		cctx, ccancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer ccancel()
		_, _ = gitCtx(cctx, t.Path, "update-ref", "-d", ref)
	}()

	bundle, err := os.CreateTemp("", "salvage-*.bundle")
	if err != nil {
		return fmt.Errorf("temp bundle: %w", err)
	}
	bundlePath := bundle.Name()
	bundle.Close()
	defer os.Remove(bundlePath)

	args := []string{"bundle", "create", bundlePath}
	if t.AnchorSHA != "" {
		args = append(args, "^"+t.AnchorSHA)
	}
	args = append(args, ref)
	if _, err := gitCtx(ctx, t.Path, args...); err != nil {
		return fmt.Errorf("git bundle: %w", err)
	}

	data, err := os.ReadFile(bundlePath)
	if err != nil {
		return fmt.Errorf("read bundle: %w", err)
	}
	if len(data) == 0 {
		return fmt.Errorf("empty bundle")
	}
	if int64(len(data)) > salvageBundleCap {
		return fmt.Errorf("bundle %d bytes exceeds cap %d — skipped", len(data), salvageBundleCap)
	}
	t.BundleBytes = int64(len(data))

	putCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	_, err = client.PutObject(putCtx, &s3.PutObjectInput{
		Bucket:      aws.String(bucket),
		Key:         aws.String(key),
		Body:        bytes.NewReader(data),
		ContentType: aws.String("application/x-git-bundle"),
		Metadata: map[string]string{
			"salvage-name": t.Name,
			"salvage-sha":  t.SalvageSHA,
		},
	})
	if err != nil {
		return fmt.Errorf("put bundle: %w", err)
	}
	return nil
}

// uploadManifest PUTs the aggregate manifest as salvage-manifest.json.
func uploadManifest(ctx context.Context, client *s3.Client, bucket, prefix string, m *salvageManifest) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	key := prefix + "/salvage-manifest.json"
	putCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	_, err = client.PutObject(putCtx, &s3.PutObjectInput{
		Bucket:      aws.String(bucket),
		Key:         aws.String(key),
		Body:        bytes.NewReader(data),
		ContentType: aws.String("application/json"),
	})
	return err
}
