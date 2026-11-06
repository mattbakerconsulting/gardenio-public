package bypass

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitFixture builds a throwaway git repo under t.TempDir() and exposes
// helpers for adding commits. Each commit carries the supplied message
// verbatim, so callers control trailer placement exactly.
type gitFixture struct {
	t    *testing.T
	path string
}

func newGitFixture(t *testing.T) *gitFixture {
	t.Helper()
	dir := t.TempDir()
	f := &gitFixture{t: t, path: dir}
	f.run("git", "init", "--quiet", "--initial-branch=main")
	// Local identity + commit settings so commits work in CI sandboxes
	// without a global git config.
	f.run("git", "config", "user.email", "fixture@example.test")
	f.run("git", "config", "user.name", "Fixture Author")
	f.run("git", "config", "commit.gpgsign", "false")
	// Disable trailer auto-injection from hooks if any.
	f.run("git", "config", "core.hooksPath", "/dev/null")
	return f
}

func (f *gitFixture) run(args ...string) string {
	f.t.Helper()
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = f.path
	out, err := cmd.CombinedOutput()
	if err != nil {
		f.t.Fatalf("%s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// commit writes filename with content and creates a commit with msg.
// Returns the new HEAD SHA.
func (f *gitFixture) commit(filename, content, msg string) string {
	f.t.Helper()
	full := filepath.Join(f.path, filename)
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		f.t.Fatalf("write %s: %v", filename, err)
	}
	f.run("git", "add", filename)
	// Use -F - via stdin to preserve multi-line messages exactly.
	// Force author/committer via env so any ambient GIT_AUTHOR_* /
	// GIT_COMMITTER_* in the test environment can't override our
	// fixture identity.
	cmd := exec.Command("git", "commit", "--quiet", "--allow-empty-message", "-F", "-")
	cmd.Dir = f.path
	cmd.Stdin = strings.NewReader(msg)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=Fixture Author",
		"GIT_AUTHOR_EMAIL=fixture@example.test",
		"GIT_COMMITTER_NAME=Fixture Author",
		"GIT_COMMITTER_EMAIL=fixture@example.test",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		f.t.Fatalf("git commit: %v\n%s", err, out)
	}
	return f.run("git", "rev-parse", "HEAD")
}

func TestParseFromCommitRange_AllValidReasons(t *testing.T) {
	for _, r := range AllReasons() {
		t.Run(string(r), func(t *testing.T) {
			f := newGitFixture(t)
			from := f.commit("seed.txt", "seed\n", "chore: seed\n")
			to := f.commit("a.txt", "a\n",
				"feat: do thing\n\nBody.\n\nCoverage-Bypass: "+string(r)+"\n")

			got, err := ParseFromCommitRange(f.path, from, to)
			if err != nil {
				t.Fatalf("ParseFromCommitRange: %v", err)
			}
			if got == nil {
				t.Fatal("expected non-nil Bypass")
			}
			if got.Reason != r {
				t.Errorf("Reason = %q, want %q", got.Reason, r)
			}
			if got.CommitSHA != to {
				t.Errorf("CommitSHA = %q, want %q", got.CommitSHA, to)
			}
			if !strings.Contains(got.Author, "Fixture Author") {
				t.Errorf("Author = %q, want to contain Fixture Author", got.Author)
			}
			if got.RawTrailer != "Coverage-Bypass: "+string(r) {
				t.Errorf("RawTrailer = %q", got.RawTrailer)
			}
			if got.FollowUp.Reason != r {
				t.Errorf("FollowUp.Reason = %q, want %q", got.FollowUp.Reason, r)
			}
		})
	}
}

func TestParseFromCommitRange_UnknownReason_FailsClosed(t *testing.T) {
	f := newGitFixture(t)
	from := f.commit("seed.txt", "seed\n", "chore: seed\n")
	_ = f.commit("a.txt", "a\n", "feat: thing\n\nCoverage-Bypass: feels-cute\n")
	to := f.run("git", "rev-parse", "HEAD")

	got, err := ParseFromCommitRange(f.path, from, to)
	if got != nil {
		t.Fatalf("expected nil Bypass on unknown reason, got %+v", got)
	}
	if !errors.Is(err, ErrUnknownReason) {
		t.Fatalf("err = %v, want ErrUnknownReason", err)
	}
}

func TestParseFromCommitRange_EmptyRange_SameSHA(t *testing.T) {
	f := newGitFixture(t)
	sha := f.commit("seed.txt", "seed\n", "chore: seed\n\nCoverage-Bypass: refactor\n")

	got, err := ParseFromCommitRange(f.path, sha, sha)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil for empty range (from==to), got %+v", got)
	}
}

func TestParseFromCommitRange_EmptyFromSHA_InitialSeed(t *testing.T) {
	// Initial seed: no baseline bead yet. The ratchet job seeds the
	// baseline rather than attempting a bypass, so an empty fromSHA
	// returns (nil, nil) — not an error. The function should not even
	// shell out to git for this case (no repo lookup required).
	got, err := ParseFromCommitRange("/nonexistent/path", "", "deadbeef")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil, got %+v", got)
	}
}

func TestParseFromCommitRange_EmptyToSHA_IsError(t *testing.T) {
	// Caller misuse: $CI_COMMIT_SHA missing. Fail loudly so the
	// ratchet job files a `tooling-broken` synthetic.
	got, err := ParseFromCommitRange("/tmp", "abc", "")
	if got != nil {
		t.Fatalf("expected nil Bypass, got %+v", got)
	}
	if err == nil {
		t.Fatal("expected error for empty toSHA")
	}
}

func TestParseFromCommitRange_NoTrailersInRange(t *testing.T) {
	f := newGitFixture(t)
	from := f.commit("seed.txt", "seed\n", "chore: seed\n")
	to := f.commit("a.txt", "a\n", "feat: ordinary commit\n\nNo trailer body.\n")

	got, err := ParseFromCommitRange(f.path, from, to)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil Bypass, got %+v", got)
	}
}

func TestParseFromCommitRange_MultipleCommits_LastWriteWins(t *testing.T) {
	// Mirrors the W2b last-write-wins semantics across multiple
	// commits in the merged range.
	f := newGitFixture(t)
	from := f.commit("seed.txt", "seed\n", "chore: seed\n")
	_ = f.commit("a.txt", "a\n", "feat: first\n\nCoverage-Bypass: refactor\n")
	to := f.commit("b.txt", "b\n", "feat: second\n\nCoverage-Bypass: emergency\n")

	got, err := ParseFromCommitRange(f.path, from, to)
	if err != nil {
		t.Fatalf("ParseFromCommitRange: %v", err)
	}
	if got == nil {
		t.Fatal("expected non-nil Bypass")
	}
	if got.Reason != ReasonEmergency {
		t.Errorf("Reason = %q, want emergency (last-write-wins)", got.Reason)
	}
	if got.CommitSHA != to {
		t.Errorf("CommitSHA = %q, want %q", got.CommitSHA, to)
	}
}

func TestParseFromCommitRange_InvalidRepoPath_Errors(t *testing.T) {
	got, err := ParseFromCommitRange(t.TempDir(), "deadbeef", "cafebabe")
	if got != nil {
		t.Fatalf("expected nil Bypass, got %+v", got)
	}
	if err == nil {
		t.Fatal("expected error for non-git path")
	}
}

func TestParseFromCommitRange_FromSHA_IsExclusive(t *testing.T) {
	// fromSHA is the baseline (previous green main). Its trailer must
	// NOT appear in the parsed range — the range is fromSHA..toSHA
	// exclusive-inclusive.
	f := newGitFixture(t)
	from := f.commit("seed.txt", "seed\n", "chore: seed\n\nCoverage-Bypass: emergency\n")
	to := f.commit("a.txt", "a\n", "feat: ordinary\n\nNo trailer.\n")

	got, err := ParseFromCommitRange(f.path, from, to)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil Bypass (fromSHA trailer must be excluded), got %+v", got)
	}
}
