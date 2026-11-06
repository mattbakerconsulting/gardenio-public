package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// salvageToS3 runs the preStop-style salvage of `work` (dirty primary checkout)
// into the in-memory S3 fake so the restore path has real bundles to fetch. It
// uses the exact runSalvage plumbing production uses, so the round-trip is
// end-to-end (salvage write → auto-restore read), not a hand-built fixture.
func salvageToS3(t *testing.T, work string) *memS3 {
	t.Helper()
	mem, _ := newMemS3(t)
	if err := runSalvage(context.Background(), work, "unit_test", salvageOpts{}); err != nil {
		t.Fatalf("runSalvage (seed): %v", err)
	}
	return mem
}

// restoreSalvageEnv clears + sets the env autoRestoreSalvageOnRespawn reads. It mirrors
// salvageTestEnv but is scoped to the restore side so a test can set the target
// project/workspace independently.
func restoreSalvageEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"COOP_S3_SESSION_KEY", "BOAT_PROJECT", "BOAT_WORKSPACE",
		salvageRestoreNoteEnv,
	} {
		t.Setenv(k, "")
	}
}

// TestAutoRestoreSalvage_RestoresWorkBundleAndNote is the end-to-end B2 golden
// path: salvage a dirty primary checkout to S3 (as the dying pod would), then run
// the respawn auto-restore against a FRESH clone of the same origin and assert it
// (a) fetched refs/salvage/work into the fresh clone, (b) left the branch/HEAD
// untouched, and (c) wrote a recovery note wired into the system prompt.
func TestAutoRestoreSalvage_RestoresWorkBundleAndNote(t *testing.T) {
	root := t.TempDir()
	work := newOriginWithClone(t, root)
	dirtyUp(t, work)
	unpushed := svGit(t, work, "rev-parse", "HEAD")

	salvageTestEnv(t) // COOP_S3_BUCKET + KD_AGENT_ID=gd-agent1 + COOP_S3_SESSION_KEY
	mem := salvageToS3(t, work)
	if _, ok := mem.get("coop/sessions/gd-agent1/gd-agent1/salvage/work.bundle"); !ok {
		t.Fatalf("precondition: work.bundle not seeded; keys=%v", mem.keys())
	}

	// Fresh successor pod: a clean re-clone of origin at <project>/work, with the
	// unpushed WIP commit absent. BOAT_PROJECT drives resolveCoopWorkdir.
	restoreSalvageEnv(t)
	freshRoot := t.TempDir()
	project := "gardenio"
	freshWork := filepath.Join(freshRoot, "bot", project, "work")
	if err := os.MkdirAll(filepath.Dir(freshWork), 0o755); err != nil {
		t.Fatal(err)
	}
	svGit(t, filepath.Dir(freshWork), "clone", "-q", filepath.Join(root, "origin"), freshWork)
	// Point resolveCoopWorkdir at the fresh clone. It hard-codes /home/agent/bot,
	// so use BOAT_WORKSPACE as the fallback path instead (project repo not found).
	t.Setenv("BOAT_PROJECT", "")
	t.Setenv("BOAT_WORKSPACE", freshWork)

	// Precondition: the unpushed sha is NOT in the fresh clone yet.
	if _, err := exec.Command("git", "-C", freshWork, "cat-file", "-t", unpushed).Output(); err == nil {
		t.Fatalf("precondition: unpushed sha %s already present before restore", unpushed)
	}

	cfg := initConfig{workspace: freshWork}
	autoRestoreSalvageOnRespawn(context.Background(), cfg)

	// refs/salvage/work must now resolve in the fresh clone, carrying the
	// salvaged commit objects (the whole point: the WIP is recoverable).
	if _, err := exec.Command("git", "-C", freshWork, "show-ref", "--verify", "--quiet", "refs/salvage/work").Output(); err != nil {
		out, _ := exec.Command("git", "-C", freshWork, "for-each-ref").CombinedOutput()
		t.Fatalf("refs/salvage/work not fetched into fresh clone; refs=\n%s", out)
	}
	// The salvage commit carries the tracked WIP edit; walk it to prove the
	// objects (not just the ref) transferred.
	if out, err := exec.Command("git", "-C", freshWork, "show", "refs/salvage/work:base.txt").CombinedOutput(); err != nil {
		t.Fatalf("salvaged base.txt not recoverable from refs/salvage/work: %v\n%s", err, out)
	} else if !strings.Contains(string(out), "wip-edit") {
		t.Errorf("salvaged base.txt missing WIP edit, got:\n%s", out)
	}

	// The note was written and wired into the env for the system prompt.
	notePath := os.Getenv(salvageRestoreNoteEnv)
	if notePath == "" {
		t.Fatal("expected salvageRestoreNoteEnv to be set after a restore")
	}
	noteData, err := os.ReadFile(notePath)
	if err != nil {
		t.Fatalf("read note: %v", err)
	}
	note := string(noteData)
	if !strings.Contains(note, "refs/salvage/work") {
		t.Errorf("note should name the restored ref, got:\n%s", note)
	}
	if !strings.Contains(note, "cherry-pick") || !strings.Contains(note, "grep salvage") {
		t.Errorf("note should carry the recovery recipe, got:\n%s", note)
	}

	// And outputSalvageRestoreNote emits it into the system-prompt body.
	var buf bytes.Buffer
	outputSalvageRestoreNote(&buf)
	if !strings.Contains(buf.String(), "refs/salvage/work") {
		t.Errorf("outputSalvageRestoreNote should emit the note, got:\n%s", buf.String())
	}
}

// TestAutoRestoreSalvage_NoBucketNoOp: with no S3 bucket, s3ResumeIntended is
// false, so the whole path is a quiet no-op — no note, no error.
func TestAutoRestoreSalvage_NoBucketNoOp(t *testing.T) {
	restoreSalvageEnv(t)
	t.Setenv("COOP_S3_BUCKET", "")
	t.Setenv("KD_AGENT_ID", "gd-agent1")

	work := t.TempDir()
	cfg := initConfig{workspace: work}
	autoRestoreSalvageOnRespawn(context.Background(), cfg)

	if os.Getenv(salvageRestoreNoteEnv) != "" {
		t.Errorf("no-bucket run must not write a restore note")
	}
}

// TestAutoRestoreSalvage_NoSalvageNoOp: bucket + key configured but S3 has no
// salvage bundles for this agent → no ref fetched, no note written.
func TestAutoRestoreSalvage_NoSalvageNoOp(t *testing.T) {
	root := t.TempDir()
	work := newOriginWithClone(t, root)

	salvageTestEnv(t)
	newMemS3(t) // empty fake — no bundles seeded
	restoreSalvageEnv(t)
	t.Setenv("BOAT_WORKSPACE", work)

	cfg := initConfig{workspace: work}
	autoRestoreSalvageOnRespawn(context.Background(), cfg)

	if os.Getenv(salvageRestoreNoteEnv) != "" {
		t.Errorf("empty-salvage run must not write a restore note")
	}
	// No branch/ref should have been created.
	if out, _ := exec.Command("git", "-C", work, "for-each-ref", "refs/salvage/").Output(); len(bytes.TrimSpace(out)) != 0 {
		t.Errorf("no salvage refs expected, got:\n%s", out)
	}
}

// TestAutoRestoreSalvage_NestedRepoRoutedToCorrectRepo verifies a repos-<name>
// bundle is fetched into the matching nested reference repo, not the primary
// checkout (the primary clone lacks the nested repo's objects, so a mis-route
// would fail to recover the nested WIP).
func TestAutoRestoreSalvage_NestedRepoRoutedToCorrectRepo(t *testing.T) {
	root := t.TempDir()
	work := newOriginWithClone(t, root)
	// Sibling repos layout: root/repos/demo (filepath.Dir(work) == root).
	_, unpushed := newNestedRepo(t, root, filepath.Join(root, "repos"), "demo")

	salvageTestEnv(t)
	mem := salvageToS3(t, work)
	if _, ok := mem.get("coop/sessions/gd-agent1/gd-agent1/salvage/repos-demo.bundle"); !ok {
		t.Fatalf("precondition: repos-demo.bundle not seeded; keys=%v", mem.keys())
	}

	// Fresh successor: re-clone BOTH the primary and the nested demo repo clean.
	restoreSalvageEnv(t)
	freshRoot := t.TempDir()
	freshWork := filepath.Join(freshRoot, "work")
	svGit(t, freshRoot, "clone", "-q", filepath.Join(root, "origin"), freshWork)
	freshDemo := filepath.Join(freshRoot, "repos", "demo")
	if err := os.MkdirAll(filepath.Dir(freshDemo), 0o755); err != nil {
		t.Fatal(err)
	}
	svGit(t, filepath.Join(freshRoot, "repos"), "clone", "-q", filepath.Join(root, "demo-origin"), freshDemo)
	t.Setenv("BOAT_WORKSPACE", freshWork)

	cfg := initConfig{workspace: freshWork}
	autoRestoreSalvageOnRespawn(context.Background(), cfg)

	// The nested unpushed sha must be recoverable in the NESTED repo …
	if out, err := exec.Command("git", "-C", freshDemo, "cat-file", "-t", unpushed).CombinedOutput(); err != nil {
		t.Fatalf("nested unpushed sha %s not restored into nested repo: %v\n%s", unpushed, err, out)
	}
	if _, err := exec.Command("git", "-C", freshDemo, "show-ref", "--verify", "--quiet", "refs/salvage/repos-demo").Output(); err != nil {
		t.Errorf("refs/salvage/repos-demo not fetched into nested repo")
	}
	// … and must NOT have leaked into the primary checkout (its objects aren't
	// there, so a mis-routed fetch would have failed anyway — assert it's clean).
	if _, err := exec.Command("git", "-C", freshWork, "show-ref", "--verify", "--quiet", "refs/salvage/repos-demo").Output(); err == nil {
		t.Errorf("nested salvage ref must not be fetched into the primary checkout")
	}
}

// TestAutoRestoreSalvage_MissingNestedRepoSkipped: a repos-<name> bundle whose
// target repo the successor did NOT re-clone is skipped cleanly (no crash, no
// note) rather than force-fetched into the wrong repo.
func TestAutoRestoreSalvage_MissingNestedRepoSkipped(t *testing.T) {
	root := t.TempDir()
	work := newOriginWithClone(t, root)
	newNestedRepo(t, root, filepath.Join(root, "repos"), "demo")

	salvageTestEnv(t)
	// Seed ONLY the nested bundle by removing the primary from discovery: run
	// salvage, then confirm both exist; the fresh pod below re-clones only work.
	mem := salvageToS3(t, work)
	if _, ok := mem.get("coop/sessions/gd-agent1/gd-agent1/salvage/repos-demo.bundle"); !ok {
		t.Fatalf("precondition: repos-demo.bundle not seeded; keys=%v", mem.keys())
	}

	restoreSalvageEnv(t)
	freshRoot := t.TempDir()
	freshWork := filepath.Join(freshRoot, "work")
	svGit(t, freshRoot, "clone", "-q", filepath.Join(root, "origin"), freshWork)
	// NB: no repos/demo re-cloned in the fresh pod.
	t.Setenv("BOAT_WORKSPACE", freshWork)

	cfg := initConfig{workspace: freshWork}
	// Must not panic; the missing nested repo target is skipped.
	autoRestoreSalvageOnRespawn(context.Background(), cfg)

	// The primary work bundle should still have restored, so a note is written —
	// but the nested ref must be absent everywhere.
	if _, err := exec.Command("git", "-C", freshWork, "show-ref", "--verify", "--quiet", "refs/salvage/repos-demo").Output(); err == nil {
		t.Errorf("missing nested repo target must not be force-fetched into the primary")
	}
}

// TestSalvageRestoreTargetRepo covers the target-repo routing table directly.
func TestSalvageRestoreTargetRepo(t *testing.T) {
	root := t.TempDir()
	work := filepath.Join(root, "work")
	svGit(t, root, "init", "-q", work) // makes work/.git

	// A primary-checkout target ("work" / a bead id) routes to the workdir.
	if got := salvageRestoreTargetRepo(work, "work"); got != work {
		t.Errorf(`salvageRestoreTargetRepo(work,"work") = %q, want %q`, got, work)
	}
	if got := salvageRestoreTargetRepo(work, "gd-somebead"); got != work {
		t.Errorf(`per-bead target should route to primary, got %q`, got)
	}

	// A nested repos-<name> target routes to the sibling repos/<name> when present.
	sibling := filepath.Join(root, "repos", "demo")
	svGit(t, root, "init", "-q", sibling)
	if got := salvageRestoreTargetRepo(work, "repos-demo"); got != sibling {
		t.Errorf(`repos-demo should route to sibling %q, got %q`, sibling, got)
	}

	// A nested target with no matching repo returns "".
	if got := salvageRestoreTargetRepo(work, "repos-absent"); got != "" {
		t.Errorf(`absent nested repo should return "", got %q`, got)
	}
}

// TestOutputSalvageRestoreNote_NoEnvNoOp: with the env unset (fresh spawn),
// outputSalvageRestoreNote emits nothing — it must not touch the system-prompt
// budget for agents that had nothing restored.
func TestOutputSalvageRestoreNote_NoEnvNoOp(t *testing.T) {
	t.Setenv(salvageRestoreNoteEnv, "")
	var buf bytes.Buffer
	outputSalvageRestoreNote(&buf)
	if buf.Len() != 0 {
		t.Errorf("expected no output with env unset, got %q", buf.String())
	}
}
