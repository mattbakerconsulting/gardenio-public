package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"gardenio/controller/internal/beadsapi"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
)

// TestRunSalvage_FailureCommentsTask: when a dirty worktree cannot be persisted
// (S3 client init fails), the worked task gets a durable FAILED breadcrumb —
// preStop stderr is discarded, so this comment is the only audit channel.
func TestRunSalvage_FailureCommentsTask(t *testing.T) {
	root := t.TempDir()
	work := newOriginWithClone(t, root)
	dirtyUp(t, work)

	// Comments keyed by request path: Tier 3 also posts its diff comment on the
	// AGENT bead during this run (the origin here rejects push options, so the
	// scratch-ref push fails over to the comment), and since gd-nKOOfEvKLt it
	// does so AFTER Tier 1 — so "the last comment" is no longer the one under test.
	comments := map[string]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/comments") {
			var body struct {
				Text string `json:"text"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			comments[r.URL.Path] = body.Text
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	c, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	oldDaemon := daemon
	daemon = c
	t.Cleanup(func() { daemon = oldDaemon })

	salvageTestEnv(t)
	t.Setenv("BOAT_TASK_ID", "gd-worked2")
	// Force newS3Client to fail: point the config loader at an error.
	orig := awsConfigLoader
	t.Cleanup(func() { awsConfigLoader = orig })
	awsConfigLoader = func(ctx context.Context, _ ...func(*awsconfig.LoadOptions) error) (aws.Config, error) {
		return aws.Config{}, fmt.Errorf("forced s3 config failure")
	}

	// runSalvage returns the whole-run error (caller logs it); the failure
	// comment must still have been posted.
	_ = runSalvage(context.Background(), work, "unit_test", salvageOpts{})
	gotText, ok := comments["/v1/beads/gd-worked2/comments"]
	if !ok {
		t.Fatalf("no failure comment posted to gd-worked2; comments posted to %v", commentPaths(comments))
	}
	if !strings.Contains(gotText, "FAILED") {
		t.Errorf("failure comment missing FAILED marker: %q", gotText)
	}
}

// commentPaths lists the request paths a comment-recording fake saw, for
// failure messages.
func commentPaths(comments map[string]string) []string {
	paths := make([]string, 0, len(comments))
	for p := range comments {
		paths = append(paths, p)
	}
	return paths
}

// TestRunSalvage_CommentsWorkedTask: with a daemon + BOAT_TASK_ID set, a
// successful salvage leaves an audit comment on the worked task.
func TestRunSalvage_CommentsWorkedTask(t *testing.T) {
	root := t.TempDir()
	work := newOriginWithClone(t, root)
	dirtyUp(t, work)

	// Keyed by request path for the same reason as TestRunSalvage_FailureCommentsTask:
	// Tier 3's agent-bead diff comment now lands after Tier 1's worked-task comment.
	comments := map[string]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/comments") {
			var body struct {
				Text string `json:"text"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			comments[r.URL.Path] = body.Text
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)

	c, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	oldDaemon := daemon
	daemon = c
	t.Cleanup(func() { daemon = oldDaemon })

	salvageTestEnv(t)
	t.Setenv("BOAT_TASK_ID", "gd-worked1")
	_, _ = newMemS3(t)

	if err := runSalvage(context.Background(), work, "unit_test", salvageOpts{}); err != nil {
		t.Fatalf("runSalvage: %v", err)
	}
	text, ok := comments["/v1/beads/gd-worked1/comments"]
	if !ok {
		t.Fatalf("no comment posted to the worked task gd-worked1; comments posted to %v", commentPaths(comments))
	}
	if !strings.Contains(text, "salvaged to") {
		t.Errorf("comment text missing salvage pointer: %q", text)
	}
	if !strings.Contains(text, "gb salvage restore") {
		t.Errorf("comment text missing restore recipe: %q", text)
	}
}

// ── Tier-2 guarded quarantine push tests ────────────────────────────────────

// leakMarker is the token the fake gitleaks below treats as a secret. Kept out
// of any real secret shape so it can never itself trip a real scanner.
const leakMarker = "FAKE-GITLEAKS-LEAK-MARKER"

// withFakeGitleaks installs a hermetic `gitleaks` shim on PATH so the push-tier
// tests are deterministic regardless of whether the real binary is present in
// the test container. The shim mirrors the real CLI contract this code relies
// on: it parses `--log-opts <range>` and the trailing repo path, runs
// `git -C <path> log -p <range>`, and exits 1 (leak) iff leakMarker appears in
// the transferred diff — else 0 (clean). This exercises the exact range/path we
// pass, not a canned answer.
func withFakeGitleaks(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	script := "#!/usr/bin/env bash\n" +
		"set -e\n" +
		"range=\"\"; prev=\"\"; path=\"\"\n" +
		"for a in \"$@\"; do\n" +
		"  if [ \"$prev\" = \"--log-opts\" ]; then range=\"$a\"; fi\n" +
		"  path=\"$a\"; prev=\"$a\"\n" +
		"done\n" +
		"if git -C \"$path\" log -p $range 2>/dev/null | grep -q '" + leakMarker + "'; then exit 1; fi\n" +
		"exit 0\n"
	bin := filepath.Join(dir, "gitleaks")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// newPushableOriginWithClone is newOriginWithClone plus receive-side push-option
// support on the origin (so `git push -o ci.skip` is accepted) and a checked-out
// feature branch with an unpushed tracked commit + a tracked edit, so there is a
// real tracked delta beyond origin/main to mirror.
func newPushableOriginWithClone(t *testing.T, root string) (work, origin string) {
	t.Helper()
	work = newOriginWithClone(t, root)
	origin = filepath.Join(root, "origin")
	// Accept push options (-o ci.skip) on a non-bare receiving repo.
	svGit(t, origin, "config", "receive.advertisePushOptions", "true")
	// Also stop the non-bare origin from rejecting a push to the currently
	// checked-out branch — we never push main, but be defensive.
	svGit(t, origin, "config", "receive.denyCurrentBranch", "ignore")
	return work, origin
}

// dirtyTracked makes a tracked delta beyond origin/main: an unpushed commit on a
// feature branch plus a tracked-file edit. No untracked files (those are covered
// separately).
func dirtyTracked(t *testing.T, path string) {
	t.Helper()
	svGit(t, path, "checkout", "-q", "-b", "feature")
	if err := os.WriteFile(filepath.Join(path, "feature.txt"), []byte("feature work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	svGit(t, path, "add", "-A")
	svGit(t, path, "commit", "-qm", "unpushed feature work")
	svAppend(t, filepath.Join(path, "base.txt"), "tracked wip edit\n")
}

// salvageBranches lists refs/heads/salvage/* present in the origin repo.
func salvageBranches(t *testing.T, origin string) []string {
	t.Helper()
	out := svGit(t, origin, "for-each-ref", "--format=%(refname:short)", "refs/heads/salvage/")
	if strings.TrimSpace(out) == "" {
		return nil
	}
	return strings.Split(out, "\n")
}

func TestSalvagePushEnvEnabled(t *testing.T) {
	for _, tc := range []struct {
		val  string
		want bool
	}{
		{"1", true}, {"true", true}, {"TRUE", true}, {"Yes", true}, {"on", true},
		{"", false}, {"0", false}, {"false", false}, {"nope", false}, {" ", false},
	} {
		t.Setenv("GARDENIO_SALVAGE_PUSH", tc.val)
		if got := salvagePushEnvEnabled(); got != tc.want {
			t.Errorf("salvagePushEnvEnabled(%q) = %v, want %v", tc.val, got, tc.want)
		}
	}
}

func TestSalvagePushRef(t *testing.T) {
	if got := salvagePushRef("gd-agent1", "work"); got != "salvage/gd-agent1/work" {
		t.Errorf("salvagePushRef(agent,name) = %q", got)
	}
	if got := salvagePushRef("", "work"); got != "salvage/work" {
		t.Errorf("salvagePushRef(no-agent,name) = %q", got)
	}
	// The salvage/ prefix is what structurally prevents ever targeting main.
	for _, r := range []string{salvagePushRef("gd-a", "work"), salvagePushRef("", "x")} {
		if !strings.HasPrefix(r, "salvage/") {
			t.Errorf("push ref %q must start with salvage/", r)
		}
		if r == "main" || r == "master" {
			t.Errorf("push ref %q must never be a mainline branch", r)
		}
	}
}

// TestPushSalvage_HappyPath: a clean tracked delta is mirrored to
// salvage/<agent-bead>/work on the remote, containing the tracked edit but NOT
// any untracked file. The agent's real branch pointer is untouched.
func TestPushSalvage_HappyPath(t *testing.T) {
	root := t.TempDir()
	work, origin := newPushableOriginWithClone(t, root)
	dirtyTracked(t, work)
	// An untracked novel-named file that must NEVER reach the remote.
	if err := os.WriteFile(filepath.Join(work, "novel-secret.txt"), []byte(leakMarker+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	branchBefore := svGit(t, work, "rev-parse", "feature")

	salvageTestEnv(t)
	withFakeGitleaks(t)

	targets := discoverSalvageTargets(context.Background(), work)
	if len(targets) == 0 {
		t.Fatal("no targets discovered")
	}
	pushSalvageTargets(context.Background(), targets)

	got := salvageBranches(t, origin)
	want := "salvage/gd-agent1/work"
	if len(got) != 1 || got[0] != want {
		t.Fatalf("salvage branches = %v, want [%s]", got, want)
	}
	// The pushed tree must contain the tracked edit and exclude the untracked
	// (novel-secret) file — the leak-surface invariant of the tracked-only push.
	tree := svGit(t, origin, "ls-tree", "-r", "--name-only", want)
	if !strings.Contains(tree, "feature.txt") || !strings.Contains(tree, "base.txt") {
		t.Errorf("pushed tree missing tracked files: %q", tree)
	}
	if strings.Contains(tree, "novel-secret.txt") {
		t.Errorf("pushed tree LEAKED untracked file novel-secret.txt: %q", tree)
	}
	// Real branch pointer untouched.
	if after := svGit(t, work, "rev-parse", "feature"); after != branchBefore {
		t.Errorf("feature branch moved: %s -> %s", branchBefore, after)
	}
}

// TestPushSalvage_LeakRefused: a tracked edit containing a secret is refused —
// no salvage branch is pushed. (The S3 bundle path is exercised elsewhere.)
func TestPushSalvage_LeakRefused(t *testing.T) {
	root := t.TempDir()
	work, origin := newPushableOriginWithClone(t, root)
	dirtyTracked(t, work)
	// Put the marker in a TRACKED file so it lands in the tracked-only snapshot.
	svAppend(t, filepath.Join(work, "base.txt"), "token="+leakMarker+"\n")

	salvageTestEnv(t)
	withFakeGitleaks(t)

	targets := discoverSalvageTargets(context.Background(), work)
	pushSalvageTargets(context.Background(), targets)

	if got := salvageBranches(t, origin); len(got) != 0 {
		t.Fatalf("leak-positive target was pushed: %v", got)
	}
}

// TestPushSalvage_GitleaksMissing: with no gitleaks on PATH the tier fails
// closed — nothing is pushed even though the delta is clean.
func TestPushSalvage_GitleaksMissing(t *testing.T) {
	root := t.TempDir()
	work, origin := newPushableOriginWithClone(t, root)
	dirtyTracked(t, work)

	salvageTestEnv(t)
	// PATH with git available but NO gitleaks → exec.LookPath("gitleaks")
	// fails while the salvage code (and the test's own git calls) still work.
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Skipf("git not on PATH: %v", err)
	}
	pathDir := t.TempDir()
	if err := os.Symlink(gitBin, filepath.Join(pathDir, "git")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", pathDir)

	targets := discoverSalvageTargets(context.Background(), work)
	pushSalvageTargets(context.Background(), targets)

	if got := salvageBranches(t, origin); len(got) != 0 {
		t.Fatalf("push happened without a gitleaks gate: %v", got)
	}
}

// TestPushSalvage_UntrackedOnlyNoPush: when the only dirt is untracked files,
// there is no tracked delta beyond origin/main, so nothing is pushed (and the
// untracked file certainly is not mirrored).
func TestPushSalvage_UntrackedOnlyNoPush(t *testing.T) {
	root := t.TempDir()
	work, origin := newPushableOriginWithClone(t, root)
	// Stay on main at origin/main; only add an untracked file (no tracked edit,
	// no unpushed commit).
	if err := os.WriteFile(filepath.Join(work, "scratch.txt"), []byte("scratch\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	salvageTestEnv(t)
	withFakeGitleaks(t)

	targets := discoverSalvageTargets(context.Background(), work)
	pushSalvageTargets(context.Background(), targets)

	if got := salvageBranches(t, origin); len(got) != 0 {
		t.Fatalf("untracked-only worktree was pushed: %v", got)
	}
}

// mrURLDaemon wires a fake beads daemon whose every GET returns a bead carrying
// an mr_url (the worked task "already has an MR"), and swallows PATCH/POST, into
// the package-global `daemon` for the test's duration.
func mrURLDaemon(t *testing.T) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"id":"gd-worked1","fields":{"mr_url":"https://gitlab.com/x/y/-/merge_requests/1"}}`))
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	c, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	oldDaemon := daemon
	daemon = c
	t.Cleanup(func() { daemon = oldDaemon })
}

// TestPushSalvage_MRPresentStillPushes: a worked-task mr_url must NOT suppress
// the Tier-2 push (gd-Jpw0LU1Rgu). The MR branch only holds committed, pushed
// work; the dirty tracked edit exists nowhere else. See
// TestSalvageTiers_MRPresentConsistent for the Tier-2/Tier-3 pairing.
func TestPushSalvage_MRPresentStillPushes(t *testing.T) {
	root := t.TempDir()
	work, origin := newPushableOriginWithClone(t, root)
	dirtyTracked(t, work)
	mrURLDaemon(t)

	salvageTestEnv(t)
	t.Setenv("BOAT_TASK_ID", "gd-worked1")
	withFakeGitleaks(t)

	targets := discoverSalvageTargets(context.Background(), work)
	pushSalvageTargets(context.Background(), targets)

	if got := salvageBranches(t, origin); len(got) != 1 {
		t.Fatalf("Tier 2 must push despite an open MR on the worked task, got branches %v", got)
	}
}

// TestSalvageTiers_MRPresentConsistent locks in the mr_url-skip consistency
// fix: with an mr_url on the worked task, Tier 2 (refs/heads/salvage/...) and
// Tier 3 (refs/gardenio/wip/...) BOTH still preserve the same dirty target, and
// both carry the same tracked-only snapshot tree. Before gd-Jpw0LU1Rgu only
// Tier 2 skipped on mr_url, losing the dirty tree whenever Tier 3 was not also
// running.
func TestSalvageTiers_MRPresentConsistent(t *testing.T) {
	root := t.TempDir()
	work, origin := newPushableOriginWithClone(t, root)
	dirtyTracked(t, work)
	mrURLDaemon(t)

	salvageTestEnv(t)
	t.Setenv("BOAT_TASK_ID", "gd-worked1")
	withFakeGitleaks(t)

	targets := discoverSalvageTargets(context.Background(), work)
	pushSalvageTargets(context.Background(), targets)
	preserveReapScratch(context.Background(), targets, "gd-agent1")

	tier2 := salvageBranches(t, origin)
	tier3 := reapScratchRefs(t, origin)
	if len(tier2) != 1 || len(tier3) != 1 {
		t.Fatalf("with an mr_url both tiers must preserve: tier2=%v tier3=%v", tier2, tier3)
	}
	t2tree := svGit(t, origin, "rev-parse", "refs/heads/"+tier2[0]+"^{tree}")
	t3tree := svGit(t, origin, "rev-parse", tier3[0]+"^{tree}")
	if t2tree != t3tree {
		t.Errorf("tiers pushed different snapshots: tier2 tree %s, tier3 tree %s", t2tree, t3tree)
	}
}

// TestPushTrackedSnapshot_Sentinels covers the benign-skip and fail-closed
// outcomes of the helper both tiers share, so a change to one tier's guard
// cannot silently diverge from the other.
func TestPushTrackedSnapshot_Sentinels(t *testing.T) {
	salvageTestEnv(t)
	withFakeGitleaks(t)
	bin, binErr := exec.LookPath("gitleaks")
	if binErr != nil {
		t.Fatal(binErr)
	}
	ctx := context.Background()
	spec := func(dst string) trackedPushSpec {
		return trackedPushSpec{gitleaksBin: bin, dstRef: dst}
	}

	t.Run("no origin", func(t *testing.T) {
		dir := t.TempDir()
		svGit(t, dir, "init", "-q", "-b", "main")
		svGit(t, dir, "commit", "-q", "--allow-empty", "-m", "c")
		err := pushTrackedSnapshot(ctx, salvageTarget{Name: "w", Path: dir}, spec("refs/gardenio/wip/x"))
		if !errors.Is(err, errPushNoOrigin) {
			t.Fatalf("err = %v, want errPushNoOrigin", err)
		}
	})
	t.Run("unborn HEAD", func(t *testing.T) {
		dir := t.TempDir()
		svGit(t, dir, "init", "-q", "-b", "main")
		svGit(t, dir, "remote", "add", "origin", t.TempDir())
		err := pushTrackedSnapshot(ctx, salvageTarget{Name: "w", Path: dir}, spec("refs/gardenio/wip/x"))
		if !errors.Is(err, errPushUnbornHead) {
			t.Fatalf("err = %v, want errPushUnbornHead", err)
		}
	})
	t.Run("untracked-only with requireTrackedChange", func(t *testing.T) {
		work, _ := newPushableOriginWithClone(t, t.TempDir())
		if err := os.WriteFile(filepath.Join(work, "u.txt"), []byte("u\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		targets := discoverSalvageTargets(ctx, work)
		sp := spec("refs/gardenio/wip/x")
		sp.requireTrackedChange = true
		err := pushTrackedSnapshot(ctx, targets[0], sp)
		if !errors.Is(err, errPushNoTracked) {
			t.Fatalf("err = %v, want errPushNoTracked", err)
		}
	})
	t.Run("untracked-only with requireAnchorDelta", func(t *testing.T) {
		work, origin := newPushableOriginWithClone(t, t.TempDir())
		if err := os.WriteFile(filepath.Join(work, "u.txt"), []byte("u\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		targets := discoverSalvageTargets(ctx, work)
		sp := spec("refs/heads/salvage/x")
		sp.requireAnchorDelta = true
		err := pushTrackedSnapshot(ctx, targets[0], sp)
		if !errors.Is(err, errPushNoDelta) {
			t.Fatalf("err = %v, want errPushNoDelta", err)
		}
		if got := salvageBranches(t, origin); len(got) != 0 {
			t.Fatalf("pushed despite no delta: %v", got)
		}
	})
	t.Run("gitleaks missing fails closed", func(t *testing.T) {
		work, origin := newPushableOriginWithClone(t, t.TempDir())
		dirtyTracked(t, work)
		targets := discoverSalvageTargets(ctx, work)
		sp := spec("refs/gardenio/wip/x")
		sp.gitleaksErr = exec.ErrNotFound
		err := pushTrackedSnapshot(ctx, targets[0], sp)
		if !errors.Is(err, errGitleaksMissing) {
			t.Fatalf("err = %v, want errGitleaksMissing", err)
		}
		if got := reapScratchRefs(t, origin); len(got) != 0 {
			t.Fatalf("pushed without a leak gate: %v", got)
		}
	})
	t.Run("pushes tracked-only snapshot to dstRef", func(t *testing.T) {
		work, origin := newPushableOriginWithClone(t, t.TempDir())
		dirtyTracked(t, work)
		if err := os.WriteFile(filepath.Join(work, "untracked.txt"), []byte("secret-ish\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		targets := discoverSalvageTargets(ctx, work)
		if err := pushTrackedSnapshot(ctx, targets[0], spec("refs/gardenio/wip/x")); err != nil {
			t.Fatal(err)
		}
		files := svGit(t, origin, "ls-tree", "-r", "--name-only", "refs/gardenio/wip/x")
		if strings.Contains(files, "untracked.txt") {
			t.Errorf("untracked file leaked into the pushed snapshot: %s", files)
		}
		if !strings.Contains(svGit(t, origin, "show", "refs/gardenio/wip/x:base.txt"), "tracked wip edit") {
			t.Errorf("tracked edit missing from pushed snapshot")
		}
	})
}

// TestCommitWorktreeSnapshot_AddFlag locks in the one snapshot recipe behind
// both Tier 1 (-A: untracked included) and Tier 2/3 (-u: tracked only), and
// that neither disturbs the real index/HEAD.
func TestCommitWorktreeSnapshot_AddFlag(t *testing.T) {
	ctx := context.Background()
	work, _ := newPushableOriginWithClone(t, t.TempDir())
	if err := os.WriteFile(filepath.Join(work, "untracked.txt"), []byte("u\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	svAppend(t, filepath.Join(work, "base.txt"), "edit\n")
	head, ok := worktreeHead(ctx, work)
	if !ok {
		t.Fatal("no HEAD")
	}
	statusBefore := svGit(t, work, "status", "--porcelain")

	all, err := commitWorktreeSnapshot(ctx, work, head, true, "-A", "m")
	if err != nil {
		t.Fatal(err)
	}
	tracked, err := snapshotTrackedOnly(ctx, work, "work", head)
	if err != nil {
		t.Fatal(err)
	}
	if got := svGit(t, work, "ls-tree", "-r", "--name-only", all); !strings.Contains(got, "untracked.txt") {
		t.Errorf("-A snapshot missing untracked file: %s", got)
	}
	if got := svGit(t, work, "ls-tree", "-r", "--name-only", tracked); strings.Contains(got, "untracked.txt") {
		t.Errorf("-u snapshot contains untracked file: %s", got)
	}
	for name, sha := range map[string]string{"-A": all, "-u": tracked} {
		if parent := svGit(t, work, "rev-parse", sha+"^"); parent != head {
			t.Errorf("%s snapshot parent = %s, want HEAD %s", name, parent, head)
		}
	}
	if after := svGit(t, work, "status", "--porcelain"); after != statusBefore {
		t.Errorf("snapshot disturbed the real index/worktree: before %q after %q", statusBefore, after)
	}
}

// TestPushSalvage_Idempotent: two consecutive push runs both succeed (force
// push), leaving exactly one salvage branch.
func TestPushSalvage_Idempotent(t *testing.T) {
	root := t.TempDir()
	work, origin := newPushableOriginWithClone(t, root)
	dirtyTracked(t, work)

	salvageTestEnv(t)
	withFakeGitleaks(t)

	targets := discoverSalvageTargets(context.Background(), work)
	pushSalvageTargets(context.Background(), targets)
	// Second edit + second run → force-updates the same ref.
	svAppend(t, filepath.Join(work, "base.txt"), "more wip\n")
	targets2 := discoverSalvageTargets(context.Background(), work)
	pushSalvageTargets(context.Background(), targets2)

	if got := salvageBranches(t, origin); len(got) != 1 {
		t.Fatalf("expected exactly one salvage branch after re-run, got %v", got)
	}
}
