package main

// init_agent_salvage_restore.go — auto-restore salvaged WIP on agent respawn
// (epic ka-0000005916 Track B, step B2 ka-0000003478).
//
// `gb salvage` (salvage.go) bundles a dying agent's uncommitted / committed-but-
// unpushed WIP to S3 on the pod preStop path. Until now that was a WRITE-ONLY
// safety net: `gb salvage restore` (salvage_restore.go) existed but was
// MANUAL-only, and no respawn path ever fetched the bundle. A successor pod
// re-clones fresh (the crew EmptyDir is wiped on delete), so the salvaged commits
// sat in S3 unused while the resumed agent redid the same edits from the restored
// Claude transcript's memory — exactly the patrick-incident loss this epic fixes.
//
// autoRestoreSalvageOnRespawn closes that loop. Called ONCE per pod boot from
// runInitLoop, BEFORE coop launches, it best-effort (time-boxed, always-continue)
// lists the S3 salvage prefix for this agent, downloads each `.bundle`, fetches
// its refs/salvage/* into the matching local repo, and drops a recovery note into
// the agent's system prompt so the agent inspects + cherry-picks the salvaged
// work instead of starting over.
//
// It is gated on s3ResumeIntended() (an S3 bucket AND an agent-scoped session
// key) — the same gate coop's session resume uses — so it is a quiet no-op on a
// fresh spawn with no prior life, and it never mutates any branch/HEAD (refs land
// only under refs/salvage/*, same as the manual restore).
//
// The S3 prefix it reads is the SAME per-agent-bead prefix `gb salvage` wrote on
// the preStop path: agentS3SessionKey() resolves the canonical key (the one
// resolveCanonicalS3Key adopted from the worked task's resume_s3_key breadcrumb),
// so a chain of involuntary respawns all point at the ORIGINAL salvage location.

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// autoRestoreSalvageTimeout bounds the whole respawn auto-restore so a slow S3
// list/download or a stuck git fetch can never delay coop launch past a few
// seconds. Overridable for tests.
var autoRestoreSalvageTimeout = 20 * time.Second

// salvageRestoreNoteEnv names the env var carrying the path to the salvage
// restore note written by writeSalvageRestoreNote. It is read by
// outputSalvageRestoreNote when `gb prime --system-prompt` builds the cached
// system-prompt body, so the note reaches the agent on its first turn and
// survives compaction. Unset on a fresh spawn (the common case) → no note.
const salvageRestoreNoteEnv = "BOAT_SALVAGE_RESTORE_NOTE"

// salvageRestoreNoteFileName is the note file written under <workdir>/.gb.
const salvageRestoreNoteFileName = "salvage-restore-note.md"

// autoRestoreSalvageOnRespawn fetches any S3 salvage bundles for this agent into
// the local repo(s) and leaves a recovery note in the system prompt. Best-effort:
// every failure is logged and swallowed, and the function always returns cleanly
// so a restore hiccup never blocks the agent from launching.
func autoRestoreSalvageOnRespawn(ctx context.Context, cfg initConfig) {
	// Gate: only when an S3 resume is intended (bucket + agent-scoped key). This
	// is the same gate coop's session resume uses, so a fresh first-life spawn
	// (no prior transcript, no salvage) is a quiet no-op.
	if !s3ResumeIntended() {
		return
	}
	bucket := s3Bucket()
	if bucket == "" {
		return // defensive; s3ResumeIntended already implies a bucket.
	}

	rctx, cancel := context.WithTimeout(ctx, autoRestoreSalvageTimeout)
	defer cancel()

	agentBead := agentS3SessionKey()
	// The preStop salvage leaves COOP_S3_SESSION_KEY unset, so it keys under
	// <prefix>/<agentBead>/<agentBead>/salvage/. gb init's own env likewise has no
	// COOP_S3_SESSION_KEY (that is added only to the coop CHILD env by
	// buildCoopEnv), so session resolves to agentBead here too — matching prefixes.
	session := os.Getenv("COOP_S3_SESSION_KEY")
	if session == "" {
		session = agentBead
	}
	prefix := salvagePrefix(s3Prefix(), agentBead, session)

	client, err := newS3Client(rctx)
	if err != nil {
		logInit("auto-restore salvage: s3 client init failed: %v (skipping)", err)
		return
	}

	// List every .bundle under the salvage prefix.
	var keys []string
	paginator := s3.NewListObjectsV2Paginator(client, &s3.ListObjectsV2Input{
		Bucket: aws.String(bucket),
		Prefix: aws.String(prefix + "/"),
	})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(rctx)
		if err != nil {
			logInit("auto-restore salvage: list s3://%s/%s/ failed: %v (skipping)", bucket, prefix, err)
			return
		}
		for _, obj := range page.Contents {
			if obj.Key != nil && strings.HasSuffix(*obj.Key, ".bundle") {
				keys = append(keys, *obj.Key)
			}
		}
	}
	if len(keys) == 0 {
		return // nothing salvaged for this agent — quiet no-op.
	}

	tmpDir, err := os.MkdirTemp("", "salvage-restore-*")
	if err != nil {
		logInit("auto-restore salvage: temp dir failed: %v (skipping)", err)
		return
	}
	// The bundles are left on the ephemeral pod fs (small) so the agent can
	// re-inspect them; the note points at `gb salvage restore` for the full set.

	coopWorkdir := resolveCoopWorkdir(initToK8sConfig(cfg))

	var fetched []string
	for _, key := range keys {
		if rctx.Err() != nil {
			logInit("auto-restore salvage: deadline reached after restoring %d ref(s)", len(fetched))
			break
		}
		name := strings.TrimSuffix(filepath.Base(key), ".bundle")
		repo := salvageRestoreTargetRepo(coopWorkdir, name)
		if repo == "" {
			logInit("auto-restore salvage: no local repo present for target %q — skipping", name)
			continue
		}
		local := filepath.Join(tmpDir, filepath.Base(key))
		if err := salvageDownload(rctx, client, bucket, key, local); err != nil {
			logInit("auto-restore salvage: download %s failed: %v", key, err)
			continue
		}
		if err := fetchSalvageBundle(rctx, repo, local); err != nil {
			logInit("auto-restore salvage: fetch %q into %s failed: %v", name, repo, err)
			continue
		}
		// gb salvage always names the ref refs/salvage/<target> (bundleAndUpload),
		// so the fetched ref name is derivable without re-querying git — and stays
		// accurate even when several bundles land in the same primary repo.
		fetched = append(fetched, "refs/salvage/"+name)
	}

	if len(fetched) == 0 {
		logInit("auto-restore salvage: found %d bundle(s) but restored no refs", len(keys))
		return
	}
	logInit("auto-restore salvage: restored %d salvage ref(s) from s3://%s/%s/: %s",
		len(fetched), bucket, prefix, strings.Join(fetched, ", "))

	writeSalvageRestoreNote(coopWorkdir, agentBead, fetched)
}

// salvageRestoreTargetRepo resolves the local repo a salvage bundle named `name`
// should be fetched into, or "" when no matching repo is present in this pod.
//
//   - "repos-<x>"  → the nested reference repo <x>, checked in the sibling layout
//     (<workspace>/../repos/<x>, the standard crew layout) and the nested layout
//     (<workspace>/repos/<x>), mirroring discoverNestedRepoTargets. "" when the
//     repo is not present (a nested repo the successor did not re-clone).
//   - anything else ("work", a per-bead worktree id) → the primary checkout. Its
//     salvage bundle is anchored on the durable remote (origin/main) the fresh
//     clone already has, so refs/salvage/* land cleanly there for inspection.
func salvageRestoreTargetRepo(coopWorkdir, name string) string {
	if nested, ok := strings.CutPrefix(name, "repos-"); ok && nested != "" {
		for _, cand := range []string{
			filepath.Join(filepath.Dir(coopWorkdir), "repos", nested),
			filepath.Join(coopWorkdir, "repos", nested),
		} {
			if dirIsGitRepo(cand) {
				return cand
			}
		}
		return ""
	}
	if dirIsGitRepo(coopWorkdir) {
		return coopWorkdir
	}
	return ""
}

// dirIsGitRepo reports whether dir carries a .git entry (dir or gitlink file).
func dirIsGitRepo(dir string) bool {
	if dir == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil
}

// fetchSalvageBundle fetches refs/salvage/* out of a downloaded bundle into repo.
// Never touches any branch/HEAD — only refs/salvage/*. Returns an error on a
// fetch failure (e.g. a missing prerequisite this fresh clone lacks), annotated
// with the missing sha when git bundle verify can name it.
func fetchSalvageBundle(ctx context.Context, repo, bundlePath string) error {
	if _, err := gitCtx(ctx, repo, "fetch", bundlePath, "refs/salvage/*:refs/salvage/*"); err != nil {
		if missing := salvageBundleMissingPrereqIn(ctx, repo, bundlePath); missing != "" {
			return fmt.Errorf("bundle needs commit %s which this clone lacks: %w", missing, err)
		}
		return err
	}
	return nil
}

// salvageBundleMissingPrereqIn returns the first prerequisite sha `git bundle
// verify` reports as missing from repo, or "" if the bundle verifies (or verify
// itself errors). Like bundleMissingPrereq (salvage_restore.go) but runs the
// verify inside a specific repo rather than the process cwd, since auto-restore
// fetches into repos other than cwd.
func salvageBundleMissingPrereqIn(ctx context.Context, repo, bundlePath string) string {
	out, err := gitCtx(ctx, repo, "bundle", "verify", bundlePath)
	if err == nil {
		return "" // verifies cleanly — the fetch failed for some other reason.
	}
	for line := range strings.SplitSeq(err.Error()+"\n"+out, "\n") {
		f := strings.Fields(strings.TrimSpace(line))
		if len(f) >= 1 && len(f[0]) >= 7 && isHexSHA(f[0]) {
			return f[0]
		}
	}
	return ""
}

// writeSalvageRestoreNote writes the recovery note under <workdir>/.gb and
// exports salvageRestoreNoteEnv with its path so outputSalvageRestoreNote emits
// it in the cached system prompt. Best-effort: a write/env failure is logged and
// swallowed (the refs are already fetched; the agent can still find them with
// `git log --all | grep salvage`). No-op when there is nothing to restore.
func writeSalvageRestoreNote(workdir, agentBead string, refs []string) {
	if workdir == "" || len(refs) == 0 {
		return
	}
	dir := filepath.Join(workdir, ".gb")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		logInit("auto-restore salvage: mkdir %s: %v (note not written)", dir, err)
		return
	}
	path := filepath.Join(dir, salvageRestoreNoteFileName)

	var b strings.Builder
	b.WriteString("## Salvaged work restored from a prior life\n\n")
	b.WriteString("A previous incarnation of this agent was killed mid-work and its ")
	b.WriteString("uncommitted / committed-but-unpushed WIP was salvaged to S3. On this ")
	b.WriteString("respawn I fetched that WIP back into your local repo(s) under ")
	b.WriteString("`refs/salvage/*` — these refs are NOT on any branch and nothing is ")
	b.WriteString("checked out, so your working tree is unchanged.\n\n")
	b.WriteString("Restored salvage ref(s):\n")
	for _, r := range refs {
		fmt.Fprintf(&b, "- `%s`\n", r)
	}
	b.WriteString("\n**Before redoing this work from scratch, inspect and recover it:**\n")
	b.WriteString("```\n")
	b.WriteString("git log --oneline --all | grep salvage   # find the salvaged commits\n")
	b.WriteString("git show <ref>                            # inspect a salvage ref\n")
	b.WriteString("git checkout <ref> -- <path>              # restore specific files, or\n")
	b.WriteString("git cherry-pick <sha>                     # replay a salvaged commit onto your branch\n")
	b.WriteString("```\n")
	if agentBead != "" {
		fmt.Fprintf(&b, "\n(Full bundle + manifest also recoverable with `gb salvage restore --agent-bead %s`.)\n", agentBead)
	}

	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		logInit("auto-restore salvage: write note %s: %v", path, err)
		return
	}
	if err := os.Setenv(salvageRestoreNoteEnv, path); err != nil {
		logInit("auto-restore salvage: setenv %s: %v", salvageRestoreNoteEnv, err)
	}
	logInit("auto-restore salvage: wrote restore note %s (%d ref(s))", path, len(refs))
}

// outputSalvageRestoreNote emits the salvaged-WIP recovery note when a prior
// incarnation's work was auto-restored into this pod's repo(s) on respawn
// (epic ka-0000005916 step B2). autoRestoreSalvageOnRespawn writes the note file
// and exports salvageRestoreNoteEnv with its path; this is a no-op when the env
// var is unset (the common fresh-spawn case), so it never touches the
// system-prompt budget for an agent that had nothing to restore.
func outputSalvageRestoreNote(w io.Writer) {
	path := os.Getenv(salvageRestoreNoteEnv)
	if path == "" {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 {
		return
	}
	fmt.Fprintln(w)
	fmt.Fprint(w, string(data))
	fmt.Fprintln(w)
}
