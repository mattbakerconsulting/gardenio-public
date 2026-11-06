package main

import (
	"context"
	"encoding/json"
	"errors"
	"gardenio/controller/internal/beadsapi"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
)

// ── Tier-3 reap-scratch preservation tests (gd-PF42yiyH1d) ─────────────────

// reapScratchRefs lists refs/gardenio/wip/* present in the origin repo.
func reapScratchRefs(t *testing.T, origin string) []string {
	t.Helper()
	out := svGit(t, origin, "for-each-ref", "--format=%(refname)", reapScratchRefNamespace)
	if strings.TrimSpace(out) == "" {
		return nil
	}
	return strings.Split(out, "\n")
}

// fieldPatchDaemonCalls holds what fieldPatchDaemon's fake server observed.
// Its fields are mutated in place by the server's handler goroutine(s), so
// callers must read them only after the triggering call (preserveReapScratch
// / runSalvage) has returned — never captured by value beforehand.
type fieldPatchDaemonCalls struct {
	PatchedBead string
	Fields      map[string]any
	CommentBead string
	CommentText string
}

// fieldPatchDaemon spins up a fake beads HTTP daemon that records the last
// PATCH .../v1/beads/<id> fields payload and the last POST .../comments body,
// and wires it into the package-global `daemon` for the test's duration.
func fieldPatchDaemon(t *testing.T) *fieldPatchDaemonCalls {
	t.Helper()
	calls := &fieldPatchDaemonCalls{Fields: map[string]any{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/beads/"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"id":"x","fields":{}}`))
		case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/v1/beads/"):
			calls.PatchedBead = strings.TrimPrefix(r.URL.Path, "/v1/beads/")
			var body struct {
				Fields json.RawMessage `json:"fields"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			_ = json.Unmarshal(body.Fields, &calls.Fields)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/comments"):
			calls.CommentBead = strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/beads/"), "/comments")
			var body struct {
				Text string `json:"text"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			calls.CommentText = body.Text
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		default:
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	t.Cleanup(srv.Close)
	c, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	oldDaemon := daemon
	daemon = c
	t.Cleanup(func() { daemon = oldDaemon })
	return calls
}

func TestReapScratchRef(t *testing.T) {
	if got := reapScratchRef("gd-agent1", "work"); got != "refs/gardenio/wip/gd-agent1/work" {
		t.Errorf("reapScratchRef(agent,name) = %q", got)
	}
	if got := reapScratchRef("", "work"); got != "refs/gardenio/wip/work" {
		t.Errorf("reapScratchRef(no-agent,name) = %q", got)
	}
	// Must never land in refs/heads/ or refs/tags/ — the two namespaces the
	// default branch/tag UI and `git branch -r`/`git tag` walk.
	for _, r := range []string{reapScratchRef("gd-a", "work"), reapScratchRef("", "x")} {
		if strings.HasPrefix(r, "refs/heads/") || strings.HasPrefix(r, "refs/tags/") {
			t.Errorf("scratch ref %q must not live under refs/heads/ or refs/tags/", r)
		}
		if !strings.HasPrefix(r, reapScratchRefNamespace) {
			t.Errorf("scratch ref %q must start with %s", r, reapScratchRefNamespace)
		}
	}
}

// TestPreserveReapScratch_PushHappyPath: a clean tracked delta is pushed to
// refs/gardenio/wip/<agent-bead>/work and the outcome is recorded as
// wip_reap_ref on the agent bead — no diff comment.
func TestPreserveReapScratch_PushHappyPath(t *testing.T) {
	root := t.TempDir()
	work, origin := newPushableOriginWithClone(t, root)
	dirtyTracked(t, work)

	salvageTestEnv(t)
	withFakeGitleaks(t)
	calls := fieldPatchDaemon(t)

	targets := discoverSalvageTargets(context.Background(), work)
	if len(targets) == 0 {
		t.Fatal("no targets discovered")
	}
	preserveReapScratch(context.Background(), targets, "gd-agent1")

	wantRef := "refs/gardenio/wip/gd-agent1/work"
	got := reapScratchRefs(t, origin)
	if len(got) != 1 || got[0] != wantRef {
		t.Fatalf("reap scratch refs = %v, want [%s]", got, wantRef)
	}

	if calls.PatchedBead != "gd-agent1" {
		t.Errorf("field patch went to %q, want gd-agent1", calls.PatchedBead)
	}
	if calls.Fields["wip_reap_ref"] != wantRef {
		t.Errorf("wip_reap_ref = %v, want %q", calls.Fields["wip_reap_ref"], wantRef)
	}
	if _, ok := calls.Fields["wip_reap_comment"]; ok {
		t.Errorf("wip_reap_comment should not be set on a successful push, got %v", calls.Fields["wip_reap_comment"])
	}
	if calls.CommentBead != "" {
		t.Errorf("no diff comment expected on a successful push, got one on %q", calls.CommentBead)
	}
}

// TestPreserveReapScratch_LeakFallsBackToComment: a tracked edit containing a
// secret is refused for the ref push; the diff-comment fallback ALSO refuses
// to post the leak — a bead comment is Dolt-durable and readable by every
// holder of the shared beads token, a WIDER audience than the git ref it
// stands in for, so the fallback runs its own gitleaks gate and withholds the
// diff on a hit, posting a "WIP withheld" notice (naming the file, never the
// secret) instead. wip_reap_comment (not wip_reap_ref) is still set, since a
// withheld notice still counts as "preserved on reap" for the death report.
func TestPreserveReapScratch_LeakFallsBackToComment(t *testing.T) {
	root := t.TempDir()
	work, origin := newPushableOriginWithClone(t, root)
	dirtyTracked(t, work)
	svAppend(t, filepath.Join(work, "base.txt"), "token="+leakMarker+"\n")
	// Also dirty an untracked file and a rename, to exercise reapDiffFiles'
	// path parsing beyond the single-tracked-edit case: a "?? new.txt" line,
	// and a "R  feature.txt -> renamed.txt" line, whose withheld notice must
	// name "renamed.txt" (the new path), not "feature.txt".
	if err := os.WriteFile(filepath.Join(work, "new.txt"), []byte("untracked wip\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	svGit(t, work, "mv", "feature.txt", "renamed.txt")

	salvageTestEnv(t)
	withFakeGitleaks(t)
	calls := fieldPatchDaemon(t)

	targets := discoverSalvageTargets(context.Background(), work)
	preserveReapScratch(context.Background(), targets, "gd-agent1")

	if got := reapScratchRefs(t, origin); len(got) != 0 {
		t.Fatalf("leak-positive target must not be pushed as a scratch ref, got %v", got)
	}
	if calls.CommentBead != "gd-agent1" {
		t.Errorf("diff comment went to %q, want gd-agent1", calls.CommentBead)
	}
	if strings.Contains(calls.CommentText, leakMarker) {
		t.Errorf("diff comment must NOT contain the leaked content, got: %q", calls.CommentText)
	}
	if !strings.Contains(calls.CommentText, "WIP withheld") {
		t.Errorf("diff comment must explain the withheld diff, got: %q", calls.CommentText)
	}
	if !strings.Contains(calls.CommentText, "base.txt") {
		t.Errorf("withheld notice should still name the affected file, got: %q", calls.CommentText)
	}
	if !strings.Contains(calls.CommentText, "new.txt") {
		t.Errorf("withheld notice should name the untracked file, got: %q", calls.CommentText)
	}
	if !strings.Contains(calls.CommentText, "renamed.txt") {
		t.Errorf("withheld notice should name a rename by its new path, got: %q", calls.CommentText)
	}
	if strings.Contains(calls.CommentText, "feature.txt") {
		t.Errorf("withheld notice should not name a rename by its old path, got: %q", calls.CommentText)
	}
	if calls.Fields["wip_reap_comment"] != "work" {
		t.Errorf("wip_reap_comment = %v, want %q", calls.Fields["wip_reap_comment"], "work")
	}
	if _, ok := calls.Fields["wip_reap_ref"]; ok {
		t.Errorf("wip_reap_ref should not be set when the push was refused, got %v", calls.Fields["wip_reap_ref"])
	}
}

// TestPreserveReapScratch_GitleaksMissingFallsBackToComment: with no gitleaks
// on PATH the ref push fails closed, and the diff-comment fallback ALSO fails
// closed — it cannot prove the diff secret-free either, so it withholds the
// diff and posts a "WIP withheld" notice rather than an unscanned diff.
func TestPreserveReapScratch_GitleaksMissingFallsBackToComment(t *testing.T) {
	root := t.TempDir()
	work, origin := newPushableOriginWithClone(t, root)
	dirtyTracked(t, work)

	salvageTestEnv(t)
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Skipf("git not on PATH: %v", err)
	}
	pathDir := t.TempDir()
	if err := os.Symlink(gitBin, filepath.Join(pathDir, "git")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", pathDir)
	calls := fieldPatchDaemon(t)

	targets := discoverSalvageTargets(context.Background(), work)
	preserveReapScratch(context.Background(), targets, "gd-agent1")

	if got := reapScratchRefs(t, origin); len(got) != 0 {
		t.Fatalf("push happened without a gitleaks gate: %v", got)
	}
	if calls.CommentBead != "gd-agent1" {
		t.Errorf("diff comment went to %q, want gd-agent1", calls.CommentBead)
	}
	if !strings.Contains(calls.CommentText, "WIP withheld") {
		t.Errorf("comment must explain the withheld diff, got: %q", calls.CommentText)
	}
	if strings.Contains(calls.CommentText, "feature work") {
		t.Errorf("comment must NOT contain unscanned diff content, got: %q", calls.CommentText)
	}
	if calls.Fields["wip_reap_comment"] != "work" {
		t.Errorf("wip_reap_comment = %v, want %q", calls.Fields["wip_reap_comment"], "work")
	}
}

// TestPreserveReapScratch_UntrackedOnlyFallsBackToComment: dirt that is only
// an untracked file has nothing for the tracked-only ref push to carry, so it
// falls straight to the diff-comment fallback — which DOES include the
// untracked file's content.
func TestPreserveReapScratch_UntrackedOnlyFallsBackToComment(t *testing.T) {
	root := t.TempDir()
	work, origin := newPushableOriginWithClone(t, root)
	if err := os.WriteFile(filepath.Join(work, "scratch.txt"), []byte("untracked-only-wip\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	salvageTestEnv(t)
	withFakeGitleaks(t)
	calls := fieldPatchDaemon(t)

	targets := discoverSalvageTargets(context.Background(), work)
	preserveReapScratch(context.Background(), targets, "gd-agent1")

	if got := reapScratchRefs(t, origin); len(got) != 0 {
		t.Fatalf("untracked-only target must not be pushed as a scratch ref, got %v", got)
	}
	if calls.CommentBead != "gd-agent1" {
		t.Errorf("diff comment went to %q, want gd-agent1", calls.CommentBead)
	}
	if !strings.Contains(calls.CommentText, "untracked-only-wip") {
		t.Errorf("diff comment missing the untracked content: %q", calls.CommentText)
	}
	if calls.Fields["wip_reap_comment"] != "work" {
		t.Errorf("wip_reap_comment = %v, want %q", calls.Fields["wip_reap_comment"], "work")
	}
}

// TestPreserveReapScratch_CleanTreeUnpushedCommitPushFailure_DiffNotEmpty is
// the regression guard for the clean-tree-but-unpushed-commit push-failure
// bug: a target with a CLEAN working tree but a committed-and-unpushed HEAD
// (bug ka-0000005216's shape) whose scratch-ref push fails (e.g. a transient
// remote error) must still produce a non-empty diff comment. Pre-fix,
// commentReapDiff diffed the Tier-1 snapshot against its own immediate parent
// (the unpushed commit itself) — since the working tree is clean that diff is
// EMPTY, so the fallback silently preserved nothing even though a whole
// commit of real work was about to evaporate. The fix diffs the durable
// remote anchor against the snapshot instead, so the unpushed commit's
// content is captured.
func TestPreserveReapScratch_CleanTreeUnpushedCommitPushFailure_DiffNotEmpty(t *testing.T) {
	root := t.TempDir()
	work, _ := newPushableOriginWithClone(t, root)
	commitUnpushedCleanTree(t, work)
	// Break the push (not the local remote-tracking ref, which is already
	// fetched and resolves anchorSHA without network) so pushReapScratchRef
	// fails and the diff-comment fallback runs.
	svGit(t, work, "remote", "set-url", "origin", filepath.Join(root, "does-not-exist"))

	salvageTestEnv(t)
	withFakeGitleaks(t)
	calls := fieldPatchDaemon(t)

	targets := discoverSalvageTargets(context.Background(), work)
	preserveReapScratch(context.Background(), targets, "gd-agent1")

	if calls.CommentBead != "gd-agent1" {
		t.Fatalf("diff comment went to %q, want gd-agent1 (push failure must still be recorded)", calls.CommentBead)
	}
	if !strings.Contains(calls.CommentText, "committed work") {
		t.Errorf("diff comment must contain the unpushed commit's content, got: %q", calls.CommentText)
	}
	if strings.Contains(calls.CommentText, "UNRECOVERABLE") {
		t.Errorf("a real diff was available; must not report total failure, got: %q", calls.CommentText)
	}
	if calls.Fields["wip_reap_comment"] != "work" {
		t.Errorf("wip_reap_comment = %v, want %q", calls.Fields["wip_reap_comment"], "work")
	}
}

// TestCommentReapDiff_NoDeltaRecordsFailure unit-tests commentReapDiff's
// residual "nothing could be captured" branch directly: an (artificial)
// target whose anchor and snapshot trees are identical yields an empty diff,
// which must still produce an "UNRECOVERABLE" comment rather than silently
// returning false and leaving the loss unrecorded.
func TestCommentReapDiff_NoDeltaRecordsFailure(t *testing.T) {
	root := t.TempDir()
	work := newOriginWithClone(t, root)
	head := svGit(t, work, "rev-parse", "HEAD")

	salvageTestEnv(t)
	withFakeGitleaks(t)
	calls := fieldPatchDaemon(t)
	gitleaksBin, err := exec.LookPath("gitleaks")
	if err != nil {
		t.Fatal(err)
	}

	target := salvageTarget{Name: "work", Path: work, AnchorSHA: head, SalvageSHA: head}
	if !commentReapDiff(context.Background(), gitleaksBin, nil, "gd-agent1", target) {
		t.Fatal("commentReapDiff must still post a comment when nothing could be captured")
	}
	if calls.CommentBead != "gd-agent1" {
		t.Errorf("comment went to %q, want gd-agent1", calls.CommentBead)
	}
	if !strings.Contains(calls.CommentText, "UNRECOVERABLE") {
		t.Errorf("comment should record the failure, got: %q", calls.CommentText)
	}
}

// TestRunSalvage_MissingBucketDirtyTreeStillPreserves is the acceptance test
// for gd-PF42yiyH1d: a crew pod with NEITHER an S3 bucket NOR the push tier
// configured must still not lose a dirty tree on reap — Tier 3 runs
// unconditionally.
func TestRunSalvage_MissingBucketDirtyTreeStillPreserves(t *testing.T) {
	root := t.TempDir()
	work, origin := newPushableOriginWithClone(t, root)
	dirtyTracked(t, work)

	salvageTestEnv(t)
	t.Setenv("COOP_S3_BUCKET", "") // no S3 sink
	withFakeGitleaks(t)
	calls := fieldPatchDaemon(t)

	if err := runSalvage(context.Background(), work, "unit_test", salvageOpts{}); err != nil {
		t.Fatalf("runSalvage: %v", err)
	}

	wantRef := "refs/gardenio/wip/gd-agent1/work"
	if got := reapScratchRefs(t, origin); len(got) != 1 || got[0] != wantRef {
		t.Fatalf("reap scratch refs = %v, want [%s]", got, wantRef)
	}
	if calls.Fields["wip_reap_ref"] != wantRef {
		t.Errorf("wip_reap_ref = %v, want %q", calls.Fields["wip_reap_ref"], wantRef)
	}
}

// TestRunSalvage_CleanTreeNoScratchRefNoComment is the clean-tree acceptance
// test: no dirty target is discovered, so preserveReapScratch is never
// invoked — no scratch ref, no comment, no field write.
func TestRunSalvage_CleanTreeNoScratchRefNoComment(t *testing.T) {
	root := t.TempDir()
	work, origin := newPushableOriginWithClone(t, root)
	// Deliberately clean: no dirtyUp/dirtyTracked call.

	salvageTestEnv(t)
	withFakeGitleaks(t)
	calls := fieldPatchDaemon(t)

	if err := runSalvage(context.Background(), work, "unit_test", salvageOpts{}); err != nil {
		t.Fatalf("runSalvage: %v", err)
	}

	if got := reapScratchRefs(t, origin); len(got) != 0 {
		t.Fatalf("clean tree must not create a scratch ref, got %v", got)
	}
	if calls.PatchedBead != "" {
		t.Errorf("clean tree must not write wip_reap_* fields, but patched %q", calls.PatchedBead)
	}
	if calls.CommentBead != "" {
		t.Errorf("clean tree must not post a diff comment, but posted to %q", calls.CommentBead)
	}
}

// ── Tier ordering (gd-nKOOfEvKLt) ───────────────────────────────────────────

// TestRunSalvage_BucketSetBundleRunsBeforeScratchRef pins the tier order from
// gd-nKOOfEvKLt (PR #10 review item 3): with an S3 bucket configured, the
// Tier-1 bundle is uploaded BEFORE Tier 3 pushes its scratch ref, so a slow
// push or a full-history gitleaks scan can never starve the higher-fidelity
// bundle of the preStop grace budget. Both still happen — Tier 3 stays
// unconditional, it just runs on the budget the bundle leaves behind.
func TestRunSalvage_BucketSetBundleRunsBeforeScratchRef(t *testing.T) {
	root := t.TempDir()
	work, origin := newPushableOriginWithClone(t, root)
	dirtyTracked(t, work)

	salvageTestEnv(t) // COOP_S3_BUCKET=test-bucket
	withFakeGitleaks(t)
	calls := fieldPatchDaemon(t)
	mem, _ := newMemS3(t)

	// Snapshot the origin's scratch refs at the moment of the FIRST S3 PUT: had
	// Tier 3 run first, refs/gardenio/wip/... would already exist. The hook runs
	// on the fake server's goroutine, so it records and never calls t.Fatal.
	var (
		mu             sync.Mutex
		putSeen        bool
		refsAtFirstPut string
		refsErr        error
	)
	mem.onPut = func(string) {
		mu.Lock()
		defer mu.Unlock()
		if putSeen {
			return
		}
		putSeen = true
		out, err := exec.Command("git", "-C", origin, "for-each-ref", "--format=%(refname)", reapScratchRefNamespace).CombinedOutput()
		refsAtFirstPut, refsErr = strings.TrimSpace(string(out)), err
	}

	if err := runSalvage(context.Background(), work, "unit_test", salvageOpts{}); err != nil {
		t.Fatalf("runSalvage: %v", err)
	}

	mu.Lock()
	seen, refs, rerr := putSeen, refsAtFirstPut, refsErr
	mu.Unlock()
	if !seen {
		t.Fatalf("Tier-1 bundle never reached S3; keys=%v", mem.keys())
	}
	if rerr != nil {
		t.Fatalf("listing scratch refs during the first PUT: %v (%s)", rerr, refs)
	}
	if refs != "" {
		t.Fatalf("Tier 3 pushed %q BEFORE the Tier-1 bundle upload; the bundle must get first call on the budget", refs)
	}

	// Both tiers completed.
	if _, ok := mem.get("coop/sessions/gd-agent1/gd-agent1/salvage/work.bundle"); !ok {
		t.Fatalf("bundle not uploaded; keys=%v", mem.keys())
	}
	wantRef := "refs/gardenio/wip/gd-agent1/work"
	if got := reapScratchRefs(t, origin); len(got) != 1 || got[0] != wantRef {
		t.Fatalf("reap scratch refs after run = %v, want [%s]", got, wantRef)
	}
	if calls.Fields["wip_reap_ref"] != wantRef {
		t.Errorf("wip_reap_ref = %v, want %q", calls.Fields["wip_reap_ref"], wantRef)
	}
}

// TestRunSalvage_S3ClientInitFailsStillPreserves guards the error path the
// reorder created: Tier 3 (and Tier 2 when enabled) now run AFTER the Tier-1
// bundle, so a bucket that is configured but unusable (credential resolution
// fails, so no S3 client can be built) must not short-circuit them. The run
// still reports the S3 error to the caller, but the scratch ref, its
// wip_reap_ref field and the Tier-2 salvage branch all exist.
func TestRunSalvage_S3ClientInitFailsStillPreserves(t *testing.T) {
	root := t.TempDir()
	work, origin := newPushableOriginWithClone(t, root)
	dirtyTracked(t, work)

	salvageTestEnv(t) // COOP_S3_BUCKET=test-bucket
	withFakeGitleaks(t)
	calls := fieldPatchDaemon(t)
	orig := awsConfigLoader
	t.Cleanup(func() { awsConfigLoader = orig })
	awsConfigLoader = func(context.Context, ...func(*awsconfig.LoadOptions) error) (aws.Config, error) {
		return aws.Config{}, errors.New("no credential source")
	}

	err := runSalvage(context.Background(), work, "unit_test", salvageOpts{push: true})
	if err == nil || !strings.Contains(err.Error(), "s3 client") {
		t.Fatalf("runSalvage error = %v, want the s3 client init failure", err)
	}

	wantRef := "refs/gardenio/wip/gd-agent1/work"
	if got := reapScratchRefs(t, origin); len(got) != 1 || got[0] != wantRef {
		t.Fatalf("Tier 3 must still run when S3 is down: scratch refs = %v, want [%s]", got, wantRef)
	}
	if calls.Fields["wip_reap_ref"] != wantRef {
		t.Errorf("wip_reap_ref = %v, want %q", calls.Fields["wip_reap_ref"], wantRef)
	}
	if got := salvageBranches(t, origin); len(got) != 1 {
		t.Fatalf("Tier 2 must still run when S3 is down: salvage branches = %v, want exactly one", got)
	}
}
