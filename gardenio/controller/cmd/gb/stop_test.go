package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// checkClaimedWork is claimedWorkGate with no held PRs, returning only the
// gate's error: the shape most claimed-work gate tests assert on.
func checkClaimedWork(ctx context.Context, agentID string) error {
	_, err := claimedWorkGate(ctx, agentID, nil)
	return err
}

// initGitRepo creates a bare remote + clone with one initial commit.
// Returns (workdir, cleanup).
func initGitRepo(t *testing.T) (string, func()) {
	t.Helper()
	tmp := t.TempDir()

	bare := filepath.Join(tmp, "remote.git")
	work := filepath.Join(tmp, "work")

	run := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%v failed: %v\n%s", args, err, out)
		}
	}

	// Create a bare remote and clone it.
	run(tmp, "git", "init", "--bare", "-b", "main", bare)
	run(tmp, "git", "clone", bare, work)

	// Initial commit so main branch exists.
	f := filepath.Join(work, "README.md")
	if err := os.WriteFile(f, []byte("init\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(work, "git", "add", "README.md")
	run(work, "git", "commit", "-m", "initial commit")
	run(work, "git", "push", "-u", "origin", "main")

	return work, func() {} // t.TempDir handles cleanup
}

func TestCurrentBranch(t *testing.T) {
	work, cleanup := initGitRepo(t)
	defer cleanup()

	got := currentBranch(work)
	if got != "main" {
		t.Errorf("currentBranch = %q, want %q", got, "main")
	}

	// Switch to a feature branch.
	cmd := exec.Command("git", "-C", work, "checkout", "-b", "feat/test")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git checkout: %v\n%s", err, out)
	}
	got = currentBranch(work)
	if got != "feat/test" {
		t.Errorf("currentBranch = %q, want %q", got, "feat/test")
	}
}

func TestCurrentBranch_NotARepo(t *testing.T) {
	got := currentBranch(t.TempDir())
	if got != "unknown" {
		t.Errorf("currentBranch(non-repo) = %q, want %q", got, "unknown")
	}
}

func TestCheckDelivery_NothingToDeliver(t *testing.T) {
	work, cleanup := initGitRepo(t)
	defer cleanup()

	t.Setenv("WORKSPACE", work)

	// Everything pushed — checkDelivery should pass.
	if err := checkDelivery(); err != nil {
		t.Errorf("checkDelivery() = %v, want nil (all pushed)", err)
	}
}

func TestCheckDelivery_UnpushedCommitsOnMain(t *testing.T) {
	work, cleanup := initGitRepo(t)
	defer cleanup()

	t.Setenv("WORKSPACE", work)

	// Make an unpushed commit on main.
	f := filepath.Join(work, "new.txt")
	if err := os.WriteFile(f, []byte("change\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = work
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
	}
	run("git", "-C", work, "add", "new.txt")
	run("git", "-C", work, "commit", "-m", "local only")

	err := checkDelivery()
	if err == nil {
		t.Fatal("checkDelivery() = nil, want error for unpushed commits on main")
	}
	if got := err.Error(); !strings.Contains(got, "unpushed") {
		t.Errorf("error = %q, want it to mention 'unpushed'", got)
	}
}

func TestCheckDelivery_UnpushedBranch(t *testing.T) {
	work, cleanup := initGitRepo(t)
	defer cleanup()

	t.Setenv("WORKSPACE", work)

	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = work
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
	}

	// Create a feature branch with a commit, don't push.
	run("git", "-C", work, "checkout", "-b", "feat/unpushed")
	f := filepath.Join(work, "feat.txt")
	if err := os.WriteFile(f, []byte("feature\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("git", "-C", work, "add", "feat.txt")
	run("git", "-C", work, "commit", "-m", "feature work")

	err := checkDelivery()
	if err == nil {
		t.Fatal("checkDelivery() = nil, want error for unpushed branch")
	}
	if got := err.Error(); !strings.Contains(got, "feat/unpushed") {
		t.Errorf("error = %q, want it to mention branch name", got)
	}
}

func TestCheckDelivery_PushedBranch(t *testing.T) {
	work, cleanup := initGitRepo(t)
	defer cleanup()

	t.Setenv("WORKSPACE", work)

	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = work
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
	}

	// Create and push a feature branch.
	run("git", "-C", work, "checkout", "-b", "feat/pushed")
	f := filepath.Join(work, "feat.txt")
	if err := os.WriteFile(f, []byte("feature\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("git", "-C", work, "add", "feat.txt")
	run("git", "-C", work, "commit", "-m", "feature work")
	run("git", "-C", work, "push", "-u", "origin", "feat/pushed")

	// All pushed — should pass.
	if err := checkDelivery(); err != nil {
		t.Errorf("checkDelivery() = %v, want nil (branch pushed)", err)
	}
}

func TestCheckDelivery_NotAGitRepo(t *testing.T) {
	t.Setenv("WORKSPACE", t.TempDir())

	// No git repo — should pass silently.
	if err := checkDelivery(); err != nil {
		t.Errorf("checkDelivery() = %v, want nil (not a git repo)", err)
	}
}

func TestProcessWrapUp_ValidJSON(t *testing.T) {
	raw := `{"accomplishments":"Closed 3 bugs","blockers":"API key pending","handoff_notes":"Check PR #42"}`

	w, comment, err := processWrapUp(raw)
	if err != nil {
		t.Fatalf("processWrapUp: %v", err)
	}
	if w.Accomplishments != "Closed 3 bugs" {
		t.Errorf("Accomplishments = %q, want %q", w.Accomplishments, "Closed 3 bugs")
	}
	if w.Blockers != "API key pending" {
		t.Errorf("Blockers = %q, want %q", w.Blockers, "API key pending")
	}
	if comment == "" {
		t.Error("comment should not be empty")
	}
	if !strings.Contains(comment, "Closed 3 bugs") {
		t.Errorf("comment missing accomplishments: %q", comment)
	}
}

func TestProcessWrapUp_InvalidJSON(t *testing.T) {
	_, _, err := processWrapUp("not json")
	if err == nil {
		t.Fatal("processWrapUp should fail on invalid JSON")
	}
	if !strings.Contains(err.Error(), "invalid --wrapup JSON") {
		t.Errorf("error = %q, want it to mention 'invalid --wrapup JSON'", err.Error())
	}
	// The Expected-format hint must teach every agent-facing field with its
	// shape — ka-0000006605: the hardcoded hint omitted pull_requests and an
	// agent burned 3 stop attempts on a bare-string value.
	if !strings.Contains(err.Error(), `"pull_requests":["...", ...]`) {
		t.Errorf("error = %q, want hint to show pull_requests as an array", err.Error())
	}
	for _, field := range []string{"tldr", "accomplishments", "blockers", "handoff_notes"} {
		if !strings.Contains(err.Error(), `"`+field+`"`) {
			t.Errorf("error = %q, want hint to mention %q", err.Error(), field)
		}
	}
}

func TestProcessWrapUp_CoercesBareStringPullRequests(t *testing.T) {
	raw := `{"tldr":"x","accomplishments":"y","pull_requests":"https://gitlab.com/a/b/-/merge_requests/1"}`

	w, comment, err := processWrapUp(raw)
	if err != nil {
		t.Fatalf("processWrapUp should coerce a bare-string pull_requests, got: %v", err)
	}
	want := []string{"https://gitlab.com/a/b/-/merge_requests/1"}
	if len(w.PullRequests) != 1 || w.PullRequests[0] != want[0] {
		t.Errorf("PullRequests = %v, want %v", w.PullRequests, want)
	}
	if !strings.Contains(comment, want[0]) {
		t.Errorf("comment missing coerced PR URL: %q", comment)
	}
}

func TestProcessWrapUp_RejectsNonStringNonArrayPullRequests(t *testing.T) {
	// Only the bare-string mistake is forgiven; other type errors stay fatal.
	_, _, err := processWrapUp(`{"accomplishments":"y","pull_requests":42}`)
	if err == nil {
		t.Fatal("processWrapUp should reject a numeric pull_requests")
	}
	if !strings.Contains(err.Error(), "invalid --wrapup JSON") {
		t.Errorf("error = %q, want it to mention 'invalid --wrapup JSON'", err.Error())
	}
}

func TestCoerceWrapUpJSON(t *testing.T) {
	tests := []struct {
		name     string
		raw      string
		wantWarn bool
	}{
		{"bare string coerced", `{"pull_requests":"https://x/mr/1"}`, true},
		{"array untouched", `{"pull_requests":["https://x/mr/1"]}`, false},
		{"absent untouched", `{"accomplishments":"y"}`, false},
		{"non-object untouched", `not json`, false},
		{"wrong type untouched", `{"pull_requests":42}`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, warnings := coerceWrapUpJSON(tt.raw)
			if tt.wantWarn {
				if len(warnings) != 1 {
					t.Fatalf("warnings = %v, want exactly 1", warnings)
				}
				var w WrapUp
				if err := json.Unmarshal([]byte(out), &w); err != nil {
					t.Fatalf("coerced output does not unmarshal: %v", err)
				}
				if len(w.PullRequests) != 1 {
					t.Errorf("PullRequests = %v, want 1 entry", w.PullRequests)
				}
			} else {
				if out != tt.raw {
					t.Errorf("output = %q, want input passed through %q", out, tt.raw)
				}
				if len(warnings) != 0 {
					t.Errorf("warnings = %v, want none", warnings)
				}
			}
		})
	}
}

func TestProcessWrapUp_EmptyObject(t *testing.T) {
	w, _, err := processWrapUp("{}")
	if err != nil {
		t.Fatalf("processWrapUp: %v", err)
	}
	if w.Accomplishments != "" {
		t.Errorf("Accomplishments = %q, want empty", w.Accomplishments)
	}
}

func TestFsyncPath_FileAndDir(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "a.jsonl")
	if err := os.WriteFile(path, []byte(`{"ok":1}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := fsyncPath(path, os.O_RDWR); err != nil {
		t.Errorf("fsyncPath(file) = %v, want nil", err)
	}
	if err := fsyncPath(tmp, os.O_RDONLY); err != nil {
		t.Errorf("fsyncPath(dir) = %v, want nil", err)
	}
}

func TestFsyncPath_Missing(t *testing.T) {
	err := fsyncPath(filepath.Join(t.TempDir(), "nope.jsonl"), os.O_RDWR)
	if err == nil {
		t.Error("fsyncPath(missing) = nil, want open error")
	}
}

func TestSyncAgentSessions_SyncsJSONLFiles(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	proj := filepath.Join(home, ".claude", "projects", "-tmp-work")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(proj, "session.jsonl")
	if err := os.WriteFile(keep, []byte(`{"turn":1}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	skip := filepath.Join(proj, "notes.txt")
	if err := os.WriteFile(skip, []byte("ignore\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Must not panic or block on typical input; sleeps 2s on success path.
	syncAgentSessions()

	// File still exists and is readable.
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("session file missing after fsync: %v", err)
	}
}

func TestSyncAgentSessions_NoProjectsDir(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // no .claude/projects
	// Should return without error and without sleeping (no files synced).
	syncAgentSessions()
}

func TestProcessWrapUp_WithBeadsAndPRs(t *testing.T) {
	raw := `{"accomplishments":"work done","beads_closed":["gd-1","gd-2"],"pull_requests":["https://github.com/org/repo/pull/1"]}`

	w, comment, err := processWrapUp(raw)
	if err != nil {
		t.Fatalf("processWrapUp: %v", err)
	}
	if len(w.BeadsClosed) != 2 {
		t.Errorf("BeadsClosed = %v, want 2 entries", w.BeadsClosed)
	}
	if len(w.PullRequests) != 1 {
		t.Errorf("PullRequests = %v, want 1 entry", w.PullRequests)
	}
	if !strings.Contains(comment, "gd-1") {
		t.Errorf("comment missing bead ID: %q", comment)
	}
}

// TestDecisionGateRequired pins the ka-0000007734 fix: a clean DONE stop (a
// valid --wrapup with accomplishments present, no open in-progress work) must
// clear the decision-bead gate WITHOUT requiring a decision and WITHOUT
// --force. The pre-fix gate fired on every non-force stop, bouncing a no-op
// cycle forever between 'gb stop' and the decision-options validator.
func TestDecisionGateRequired(t *testing.T) {
	cases := []struct {
		name   string
		force  bool
		wrapup *WrapUp
		want   bool // true => gate fires (decision bead required)
	}{
		// Clean DONE stop: valid wrap-up, no force -> gate does NOT fire.
		{"done wrapup, no force", false, &WrapUp{Accomplishments: "shipped MR 42"}, false},
		// The no-op gardener case from the bug: accomplishments describe a
		// legitimate no-op cycle -> gate does NOT fire.
		{"no-op cycle wrapup", false, &WrapUp{Accomplishments: "release cycle: no cut, main +0"}, false},
		// Bare stop (nil wrap-up): gate STILL fires so silent exits are caught.
		{"nil wrapup", false, nil, true},
		// Wrap-up present but accomplishments empty: not a done stop, gate fires.
		{"empty accomplishments", false, &WrapUp{TLDR: "x"}, true},
		// Whitespace-only accomplishments is not "present".
		{"whitespace accomplishments", false, &WrapUp{Accomplishments: "   "}, true},
		// --force always clears the gate, regardless of wrap-up.
		{"force, nil wrapup", true, nil, false},
		{"force, done wrapup", true, &WrapUp{Accomplishments: "x"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := decisionGateRequired(c.force, c.wrapup); got != c.want {
				t.Errorf("decisionGateRequired(force=%v, wrapup=%+v) = %v, want %v",
					c.force, c.wrapup, got, c.want)
			}
		})
	}
}

// TestIsDoneWrapUp covers the accomplishments-presence predicate that drives
// the clean-done carve-out.
func TestIsDoneWrapUp(t *testing.T) {
	if isDoneWrapUp(nil) {
		t.Error("nil wrap-up should not be a done wrap-up")
	}
	if isDoneWrapUp(&WrapUp{}) {
		t.Error("empty wrap-up should not be a done wrap-up")
	}
	if isDoneWrapUp(&WrapUp{Accomplishments: "  \t \n "}) {
		t.Error("whitespace-only accomplishments should not be a done wrap-up")
	}
	if !isDoneWrapUp(&WrapUp{Accomplishments: "did the thing"}) {
		t.Error("wrap-up with accomplishments should be a done wrap-up")
	}
}

// TestRequireDecisionBead_HintDoesNotLoopBackToBareStop pins the
// discoverability half of ka-0000007734: when the gate DOES fire (blocked /
// no-wrapup stop with no decision bead), the remediation must not send the
// agent back to the very command that landed here. It must (a) name the
// done-path (a wrap-up) and (b) name 'gb decision create' (with options) for
// the blocked case — never bare 'kd create --type decision' as the sole escape.
func TestRequireDecisionBead_HintDoesNotLoopBackToBareStop(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"beads": []map[string]any{{
				"id": "gd-D1", "type": "decision", "created_by": "someone-else",
				"status": "open",
			}},
		})
	}))
	defer srv.Close()

	oldDaemon := daemon
	daemon, _ = beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	defer func() { daemon = oldDaemon }()

	// Capture stderr where the remediation hint is written.
	oldStderr := os.Stderr
	rPipe, wPipe, _ := os.Pipe()
	os.Stderr = wPipe

	err := requireDecisionBead(context.Background(), "alice")

	wPipe.Close()
	os.Stderr = oldStderr
	var buf bytes.Buffer
	_, _ = io.Copy(&buf, rPipe)
	hint := buf.String()

	if err == nil {
		t.Fatal("expected error when alice has no decision bead")
	}
	// The done-path (wrap-up) must be offered.
	if !strings.Contains(hint, "--wrapup") {
		t.Errorf("hint should point at the --wrapup done-path, got:\n%s", hint)
	}
	// The blocked-path must use 'gb decision create' with options, not a bare
	// 'kd create --type decision' that the validator rejects for a no-op.
	if !strings.Contains(hint, "gb decision create") {
		t.Errorf("hint should recommend 'gb decision create' for the blocked case, got:\n%s", hint)
	}
	if !strings.Contains(hint, "--options") {
		t.Errorf("hint should spell out the --options requirement, got:\n%s", hint)
	}
	// Regression guard: the old self-contradictory hint hard-coded
	// 'kd create "<...>" --type decision' as THE fix. That command HTTP-400s
	// for a no-op cycle (no options), closing the loop. It must be gone.
	if strings.Contains(hint, "--type decision") {
		t.Errorf("hint must not hard-code the looping 'kd create --type decision' remediation, got:\n%s", hint)
	}
}

// --- --successor flag (task ka-0000002751) ---

// TestStopSuccessorFlagRegistered proves the CLI actually exposes --successor
// with the documented default and usage, so the flag is wired end-to-end.
func TestStopSuccessorFlagRegistered(t *testing.T) {
	f := stopCmd.Flags().Lookup("successor")
	if f == nil {
		t.Fatal("gb stop should register a --successor flag")
	}
	if f.DefValue != "" {
		t.Errorf("--successor default = %q, want empty (== none)", f.DefValue)
	}
	for _, v := range []string{"none", "same", "new"} {
		if !strings.Contains(f.Usage, v) {
			t.Errorf("--successor usage should mention %q: %q", v, f.Usage)
		}
	}
}

// TestApplySuccessorFlag covers the flag<->JSON merge contract:
//   - no flag: raw --wrapup passes through untouched
//   - flag + no --wrapup: a minimal {"successor":...} object is synthesized
//   - flag + --wrapup without successor: flag injected
//   - flag + --wrapup WITH successor: JSON wins (flag ignored)
//   - unknown flag: rejected at parse
func TestApplySuccessorFlag(t *testing.T) {
	t.Run("no flag passes through", func(t *testing.T) {
		raw := `{"accomplishments":"x"}`
		out, err := applySuccessorFlag(raw, "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if out != raw {
			t.Errorf("out = %q, want passthrough %q", out, raw)
		}
	})

	t.Run("flag with no wrapup synthesizes object", func(t *testing.T) {
		out, err := applySuccessorFlag("", "new")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		var w WrapUp
		if err := json.Unmarshal([]byte(out), &w); err != nil {
			t.Fatalf("synthesized wrap-up does not unmarshal: %v (%s)", err, out)
		}
		if w.Successor != SuccessorNew {
			t.Errorf("Successor = %q, want %q", w.Successor, SuccessorNew)
		}
	})

	t.Run("flag injected when wrapup omits successor", func(t *testing.T) {
		out, err := applySuccessorFlag(`{"accomplishments":"x"}`, "same")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		var w WrapUp
		if err := json.Unmarshal([]byte(out), &w); err != nil {
			t.Fatalf("merged wrap-up does not unmarshal: %v (%s)", err, out)
		}
		if w.Accomplishments != "x" {
			t.Errorf("Accomplishments = %q, want preserved", w.Accomplishments)
		}
		if w.Successor != SuccessorSame {
			t.Errorf("Successor = %q, want %q", w.Successor, SuccessorSame)
		}
	})

	t.Run("wrapup JSON wins over flag", func(t *testing.T) {
		// Capture the conflict warning on stderr.
		oldStderr := os.Stderr
		rPipe, wPipe, _ := os.Pipe()
		os.Stderr = wPipe
		out, err := applySuccessorFlag(`{"accomplishments":"x","successor":"new"}`, "same")
		wPipe.Close()
		os.Stderr = oldStderr
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, rPipe)

		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		var w WrapUp
		if err := json.Unmarshal([]byte(out), &w); err != nil {
			t.Fatalf("does not unmarshal: %v", err)
		}
		if w.Successor != SuccessorNew {
			t.Errorf("Successor = %q, want %q (JSON authoritative)", w.Successor, SuccessorNew)
		}
		if !strings.Contains(buf.String(), "--wrapup value wins") {
			t.Errorf("expected a conflict warning, got: %q", buf.String())
		}
	})

	t.Run("unknown flag rejected", func(t *testing.T) {
		_, err := applySuccessorFlag("", "bogus")
		if err == nil {
			t.Fatal("applySuccessorFlag should reject an unknown --successor value")
		}
		if !strings.Contains(err.Error(), "invalid --successor flag") {
			t.Errorf("error = %q, want it to mention 'invalid --successor flag'", err.Error())
		}
	})
}

// TestApplyGoalUnachievableFlag mirrors TestApplySuccessorFlag for the
// --goal-unachievable convenience flag (P3, ka-0000005843): false is a no-op,
// true synthesizes/merges the JSON field, and an explicit JSON value wins.
func TestApplyGoalUnachievableFlag(t *testing.T) {
	t.Run("false passes through unchanged", func(t *testing.T) {
		raw := `{"accomplishments":"x"}`
		out, err := applyGoalUnachievableFlag(raw, false)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if out != raw {
			t.Errorf("out = %q, want passthrough %q", out, raw)
		}
	})

	t.Run("true with no wrapup synthesizes object", func(t *testing.T) {
		out, err := applyGoalUnachievableFlag("", true)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		var w WrapUp
		if err := json.Unmarshal([]byte(out), &w); err != nil {
			t.Fatalf("synthesized wrap-up does not unmarshal: %v (%s)", err, out)
		}
		if !w.GoalUnachievable {
			t.Errorf("GoalUnachievable = false, want true")
		}
	})

	t.Run("true injected when wrapup omits field", func(t *testing.T) {
		out, err := applyGoalUnachievableFlag(`{"accomplishments":"x"}`, true)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		var w WrapUp
		if err := json.Unmarshal([]byte(out), &w); err != nil {
			t.Fatalf("merged wrap-up does not unmarshal: %v (%s)", err, out)
		}
		if w.Accomplishments != "x" {
			t.Errorf("Accomplishments = %q, want preserved", w.Accomplishments)
		}
		if !w.GoalUnachievable {
			t.Errorf("GoalUnachievable = false, want true")
		}
	})

	t.Run("wrapup JSON wins over flag", func(t *testing.T) {
		oldStderr := os.Stderr
		rPipe, wPipe, _ := os.Pipe()
		os.Stderr = wPipe
		out, err := applyGoalUnachievableFlag(`{"accomplishments":"x","goal_unachievable":false}`, true)
		wPipe.Close()
		os.Stderr = oldStderr
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, rPipe)

		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		var w WrapUp
		if err := json.Unmarshal([]byte(out), &w); err != nil {
			t.Fatalf("does not unmarshal: %v", err)
		}
		if w.GoalUnachievable {
			t.Errorf("GoalUnachievable = true, want false (JSON authoritative)")
		}
		if !strings.Contains(buf.String(), "--wrapup value wins") {
			t.Errorf("expected a conflict warning, got: %q", buf.String())
		}
	})
}

// TestProcessWrapUp_SuccessorValidatedAndReconciled ties the pieces together at
// the processWrapUp boundary: a valid successor is canonicalized and persisted;
// an unknown one is fatal; and the goal_satisfied=no + none contradiction is
// coerced back to none (dropped to empty via omitempty) with a warning.
func TestProcessWrapUp_SuccessorValidatedAndReconciled(t *testing.T) {
	t.Run("valid successor canonicalized", func(t *testing.T) {
		w, _, err := processWrapUp(`{"accomplishments":"x","successor":"New"}`)
		if err != nil {
			t.Fatalf("processWrapUp: %v", err)
		}
		if w.Successor != SuccessorNew {
			t.Errorf("Successor = %q, want %q", w.Successor, SuccessorNew)
		}
	})

	t.Run("unknown successor is fatal", func(t *testing.T) {
		_, _, err := processWrapUp(`{"accomplishments":"x","successor":"maybe"}`)
		if err == nil {
			t.Fatal("processWrapUp should reject an unknown successor")
		}
		if !strings.Contains(err.Error(), "invalid --wrapup successor") {
			t.Errorf("error = %q, want it to mention 'invalid --wrapup successor'", err.Error())
		}
	})

	t.Run("goal=no + none coerced to empty with warning", func(t *testing.T) {
		oldStderr := os.Stderr
		rPipe, wPipe, _ := os.Pipe()
		os.Stderr = wPipe
		w, _, err := processWrapUp(`{"accomplishments":"x","goal_satisfied":"no","successor":"none"}`)
		wPipe.Close()
		os.Stderr = oldStderr
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, rPipe)

		if err != nil {
			t.Fatalf("processWrapUp: %v", err)
		}
		if w.Successor != "" {
			t.Errorf("Successor = %q, want empty (coerced none)", w.Successor)
		}
		if !strings.Contains(buf.String(), "no respawn will") {
			t.Errorf("expected contradiction warning, got: %q", buf.String())
		}
	})

	t.Run("goal=no + new passes through", func(t *testing.T) {
		w, _, err := processWrapUp(`{"accomplishments":"x","goal_satisfied":"no","successor":"new"}`)
		if err != nil {
			t.Fatalf("processWrapUp: %v", err)
		}
		if w.Successor != SuccessorNew {
			t.Errorf("Successor = %q, want %q (legitimate continuation)", w.Successor, SuccessorNew)
		}
	})
}

func TestFreezeEligible(t *testing.T) {
	t.Setenv("BOAT_FREEZE_ELIGIBLE", "true")
	if !freezeEligible() {
		t.Errorf("freezeEligible() = false, want true when BOAT_FREEZE_ELIGIBLE=true")
	}
	// Any value other than the exact "true" is not eligible.
	for _, v := range []string{"", "false", "1", "TRUE", "yes"} {
		t.Setenv("BOAT_FREEZE_ELIGIBLE", v)
		if freezeEligible() {
			t.Errorf("freezeEligible() = true for BOAT_FREEZE_ELIGIBLE=%q, want false", v)
		}
	}
}

// TestRenderStopGateRejection_BatchesAllFailures is the core regression lock
// for ka-0000005647: the two defects the audited gardener hit one-per-invocation
// (claimed work pending, then wrap-up missing "tldr") must be reported in ONE
// rejection listing BOTH, under a single header, with a single --force escape.
func TestRenderStopGateRejection_BatchesAllFailures(t *testing.T) {
	claimed := fmt.Errorf("claimed work pending: gd-PPqzhPOlLr — close ('kd close gd-PPqzhPOlLr') or unclaim ('kd unclaim gd-PPqzhPOlLr')")
	wrapup := fmt.Errorf("wrap-up incomplete: required field \"tldr\" is empty")

	msg := renderStopGateRejection([]error{claimed, wrapup})

	// Exactly one "Cannot stop" header — a single rejection, not two.
	if n := strings.Count(msg, "Cannot stop"); n != 1 {
		t.Errorf("want exactly 1 'Cannot stop' header (one rejection), got %d:\n%s", n, msg)
	}
	// The count reflects both gates.
	if !strings.Contains(msg, "2 despawn gate(s)") {
		t.Errorf("header should report 2 gates, got:\n%s", msg)
	}
	// Both defects are enumerated in one message.
	if !strings.Contains(msg, "[1]") || !strings.Contains(msg, "[2]") {
		t.Errorf("both gates should be numbered [1] and [2], got:\n%s", msg)
	}
	if !strings.Contains(msg, "gd-PPqzhPOlLr") {
		t.Errorf("rejection should name the claimed bead, got:\n%s", msg)
	}
	if !strings.Contains(msg, "tldr") {
		t.Errorf("rejection should name the missing tldr field, got:\n%s", msg)
	}
	// A single consolidated --force escape, not one per gate.
	if n := strings.Count(msg, "--force"); n != 1 {
		t.Errorf("want exactly 1 '--force' escape note, got %d:\n%s", n, msg)
	}
}

// TestRenderStopGateRejection_Empty proves no failures render an empty string,
// so a clean stop proceeds (the caller treats "" as "no rejection").
func TestRenderStopGateRejection_Empty(t *testing.T) {
	if got := renderStopGateRejection(nil); got != "" {
		t.Errorf("renderStopGateRejection(nil) = %q, want empty", got)
	}
	if got := renderStopGateRejection([]error{}); got != "" {
		t.Errorf("renderStopGateRejection([]) = %q, want empty", got)
	}
}

// TestRenderStopGateRejection_Single covers the single-failure case: still one
// header, one numbered item, one --force note.
func TestRenderStopGateRejection_Single(t *testing.T) {
	msg := renderStopGateRejection([]error{fmt.Errorf("only one gate")})
	if !strings.Contains(msg, "1 despawn gate(s)") {
		t.Errorf("want '1 despawn gate(s)' header, got:\n%s", msg)
	}
	if !strings.Contains(msg, "[1] only one gate") {
		t.Errorf("want the single failure numbered [1], got:\n%s", msg)
	}
	if strings.Contains(msg, "[2]") {
		t.Errorf("single failure must not render a [2] item, got:\n%s", msg)
	}
}

// TestCheckClaimedWork_Batched proves the claimed-work gate returns a concise
// headline naming the bead (for the batched rejection) while still printing the
// full close/unclaim remediation to stderr, and returns nil when no work is
// claimed (fail-open) so it never wedges a stop on its own.
func TestCheckClaimedWork_Batched(t *testing.T) {
	t.Run("claimed work -> headline names the bead", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"beads": []map[string]any{{
					"id": "gd-CLAIMED01", "title": "some in-progress task",
					"status": "in_progress", "kind": "issue",
				}},
			})
		}))
		defer srv.Close()
		swapDaemon(t, srv.URL)

		oldActor := actor
		actor = "alice"
		t.Cleanup(func() { actor = oldActor })

		// Capture the stderr remediation.
		oldStderr := os.Stderr
		rPipe, wPipe, _ := os.Pipe()
		os.Stderr = wPipe
		err := checkClaimedWork(context.Background(), "gd-AGENT01")
		wPipe.Close()
		os.Stderr = oldStderr
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, rPipe)
		stderr := buf.String()

		if err == nil {
			t.Fatal("checkClaimedWork returned nil while a bead was claimed; want an error")
		}
		if !strings.Contains(err.Error(), "gd-CLAIMED01") {
			t.Errorf("headline error should name the claimed bead, got: %q", err.Error())
		}
		// Full remediation still reaches stderr.
		for _, want := range []string{"kd close gd-CLAIMED01", "kd unclaim gd-CLAIMED01", "--force"} {
			if !strings.Contains(stderr, want) {
				t.Errorf("stderr remediation missing %q, got:\n%s", want, stderr)
			}
		}
	})

	t.Run("no claimed work -> nil (fail-open)", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{"beads": []map[string]any{}})
		}))
		defer srv.Close()
		swapDaemon(t, srv.URL)

		if err := checkClaimedWork(context.Background(), "gd-AGENT01"); err != nil {
			t.Errorf("no claimed work should return nil, got: %v", err)
		}
	})
}

// parkFixture serves the three reads the park exemption makes — the claimed-work
// list, each task's dependencies, and each handle bead — from in-memory maps, so
// a case declares only the graph it cares about. deps maps task id -> handle ids
// linked by a "blocks" edge; handles maps handle id -> its bead JSON.
func parkFixture(t *testing.T, claimed []map[string]any, deps map[string][]string, handles map[string]map[string]any) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case strings.HasSuffix(path, "/dependencies"):
			id := strings.TrimSuffix(strings.TrimPrefix(path, "/v1/beads/"), "/dependencies")
			rows := []map[string]any{}
			for _, h := range deps[id] {
				rows = append(rows, map[string]any{
					"bead_id": id, "depends_on_id": h, "type": "blocks",
				})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"dependencies": rows})
		case strings.HasPrefix(path, "/v1/beads/"):
			id := strings.TrimPrefix(path, "/v1/beads/")
			bead, ok := handles[id]
			if !ok {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			_ = json.NewEncoder(w).Encode(bead)
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"beads": claimed})
		}
	}))
	t.Cleanup(srv.Close)
	swapDaemon(t, srv.URL)
}

// TestCheckClaimedWork_ParkedBehindOwnHandle covers the park exemption
// (ka-0000004133): a claimed task blocked by an OPEN escalation/decision that
// THIS agent filed is the sanctioned "done but the close is gated on an
// operator action" path, so it must not trip the gate — while every other
// shape (no handle, closed handle, another agent's handle, a second abandoned
// claim) still must.
func TestCheckClaimedWork_ParkedBehindOwnHandle(t *testing.T) {
	oldActor := actor
	actor = "alice"
	t.Cleanup(func() { actor = oldActor })

	claimed := []map[string]any{{
		"id": "gd-PARKED01", "title": "fix lives on an unmerged MR",
		"status": "in_progress", "kind": "issue", "assignee": "alice",
	}}

	// Ownership is recorded by agent-BEAD id, and escalations/decisions use
	// different field keys for it — both must satisfy the exemption.
	for _, tc := range []struct {
		name  string
		bead  map[string]any
		agent string
	}{
		{
			name: "escalation via requesting_agent",
			bead: map[string]any{
				"id": "gd-HANDLE01", "type": "escalation", "status": "open",
				"fields": map[string]string{"requesting_agent": "gd-AGENT01"},
			},
			agent: "gd-AGENT01",
		},
		{
			name: "decision via requesting_agent_bead_id",
			bead: map[string]any{
				"id": "gd-HANDLE01", "type": "decision", "status": "open",
				"fields": map[string]string{"requesting_agent_bead_id": "gd-AGENT01"},
			},
			agent: "gd-AGENT01",
		},
		{
			// The handle records no agent-bead id at all; created_by is the
			// only ownership signal left.
			name: "escalation via created_by actor name",
			bead: map[string]any{
				"id": "gd-HANDLE01", "type": "escalation", "status": "open",
				"created_by": "alice",
			},
			agent: "gd-AGENT01",
		},
	} {
		t.Run(tc.name+" -> gate satisfied", func(t *testing.T) {
			parkFixture(t, claimed,
				map[string][]string{"gd-PARKED01": {"gd-HANDLE01"}},
				map[string]map[string]any{"gd-HANDLE01": tc.bead})

			if err := checkClaimedWork(context.Background(), tc.agent); err != nil {
				t.Errorf("task parked behind its own open handle must not trip the gate, got: %v", err)
			}
		})
	}

	// Regression guard: the exemption must stay an exemption, not a bypass.
	for _, tc := range []struct {
		name    string
		deps    map[string][]string
		handles map[string]map[string]any
	}{
		{
			name: "no handle at all (plain abandoned claim)",
			deps: map[string][]string{},
		},
		{
			name: "handle already closed — nothing left to respawn on",
			deps: map[string][]string{"gd-PARKED01": {"gd-HANDLE01"}},
			handles: map[string]map[string]any{"gd-HANDLE01": {
				"id": "gd-HANDLE01", "type": "escalation", "status": "closed",
				"fields": map[string]string{"requesting_agent": "gd-AGENT01"},
			}},
		},
		{
			name: "another agent's escalation is a real blocker, not a park",
			deps: map[string][]string{"gd-PARKED01": {"gd-HANDLE01"}},
			handles: map[string]map[string]any{"gd-HANDLE01": {
				"id": "gd-HANDLE01", "type": "escalation", "status": "open",
				"created_by": "bob",
				"fields":     map[string]string{"requesting_agent": "gd-OTHER99"},
			}},
		},
		{
			// Status is allow-listed, not closed-excluded: an unrecognized
			// (e.g. future terminal) status must fail closed rather than
			// silently grant a permanent exemption.
			name: "handle in an unknown/terminal status",
			deps: map[string][]string{"gd-PARKED01": {"gd-HANDLE01"}},
			handles: map[string]map[string]any{"gd-HANDLE01": {
				"id": "gd-HANDLE01", "type": "escalation", "status": "superseded",
				"fields": map[string]string{"requesting_agent": "gd-AGENT01"},
			}},
		},
		{
			name: "blocks-edge to a non-handle bead (a blocking task)",
			deps: map[string][]string{"gd-PARKED01": {"gd-TASK99"}},
			handles: map[string]map[string]any{"gd-TASK99": {
				"id": "gd-TASK99", "type": "task", "status": "open",
				"created_by": "alice",
			}},
		},
	} {
		t.Run(tc.name+" -> gate still fires", func(t *testing.T) {
			parkFixture(t, claimed, tc.deps, tc.handles)

			err := checkClaimedWork(context.Background(), "gd-AGENT01")
			if err == nil {
				t.Fatal("gate must still fire; got nil (exemption widened into a bypass)")
			}
			if !strings.Contains(err.Error(), "gd-PARKED01") {
				t.Errorf("headline should name the unparked bead, got: %q", err.Error())
			}
		})
	}

	// awaits_release parks a merged task behind its release (gd-ZM7NEBPAqZ):
	// no handle is needed, and the decoded bool still reads as the opt-in.
	t.Run("awaits_release task is parked behind its release", func(t *testing.T) {
		parkFixture(t, []map[string]any{{
			"id": "gd-SHIPS01", "title": "merged, waiting for its tag", "status": "in_progress",
			"kind": "issue", "assignee": "alice", "fields": map[string]any{"awaits_release": true},
		}}, nil, nil)
		if err := checkClaimedWork(context.Background(), "gd-AGENT01"); err != nil {
			t.Fatalf("an awaits_release task must not trip the gate: %v", err)
		}
	})

	// A parked FIRST claim must not mask a still-abandoned SECOND one — the
	// reason the gate reads every claimed bead rather than just Beads[0].
	t.Run("parked claim does not mask a second abandoned claim", func(t *testing.T) {
		parkFixture(t,
			[]map[string]any{
				{"id": "gd-PARKED01", "title": "parked", "status": "in_progress", "kind": "issue"},
				{"id": "gd-LOOSE02", "title": "genuinely abandoned", "status": "in_progress", "kind": "issue"},
			},
			map[string][]string{"gd-PARKED01": {"gd-HANDLE01"}},
			map[string]map[string]any{"gd-HANDLE01": {
				"id": "gd-HANDLE01", "type": "escalation", "status": "open",
				"fields": map[string]string{"requesting_agent": "gd-AGENT01"},
			}})

		err := checkClaimedWork(context.Background(), "gd-AGENT01")
		if err == nil {
			t.Fatal("a second unparked claim must still trip the gate; got nil")
		}
		if !strings.Contains(err.Error(), "gd-LOOSE02") {
			t.Errorf("headline should name the ABANDONED bead, got: %q", err.Error())
		}
	})
}

// TestCheckClaimedWork_ParkedBehindSiblingHandle covers the re-spawn widening
// (ka-0000007825): the exemption keys on the parked TASK (`requesting_task`),
// not on the filing agent, so a re-spawn continuing the same worked bead is
// exempt even though the handle was filed by an EARLIER lineage sibling — a
// different agent bead AND a different actor. Before this, such a re-spawn was
// forced to `gb stop --force`, overriding every other gate too.
func TestCheckClaimedWork_ParkedBehindSiblingHandle(t *testing.T) {
	// This session is actor "…-x78" / agent bead gd-BEADX78; the handle was
	// filed by sibling "…-7le" / gd-BEAD7LE. Mirrors the real repro session.
	oldActor := actor
	actor = "gardenio-review-x78"
	t.Cleanup(func() { actor = oldActor })

	claimed := []map[string]any{{
		"id": "gd-WORKED01", "title": "fix lives on an unmerged MR",
		"status": "in_progress", "kind": "issue", "assignee": "gardenio-review-x78",
	}}

	for _, tc := range []struct {
		name string
		bead map[string]any
	}{
		{
			name: "escalation filed by a lineage sibling",
			bead: map[string]any{
				"id": "gd-HANDLE01", "type": "escalation", "status": "open",
				"created_by": "gardenio-review-7le",
				"fields": map[string]string{
					"requesting_task":  "gd-WORKED01",
					"requesting_agent": "gd-BEAD7LE",
				},
			},
		},
		{
			name: "decision filed by a lineage sibling",
			bead: map[string]any{
				"id": "gd-HANDLE01", "type": "decision", "status": "open",
				"created_by": "gardenio-review-7le",
				"fields": map[string]string{
					"requesting_task":          "gd-WORKED01",
					"requesting_agent_bead_id": "gd-BEAD7LE",
				},
			},
		},
	} {
		t.Run(tc.name+" -> gate satisfied without --force", func(t *testing.T) {
			parkFixture(t, claimed,
				map[string][]string{"gd-WORKED01": {"gd-HANDLE01"}},
				map[string]map[string]any{"gd-HANDLE01": tc.bead})

			// Agent bead id deliberately differs from the handle's recorded
			// requesting_agent: only the task match can carry this.
			if err := checkClaimedWork(context.Background(), "gd-BEADX78"); err != nil {
				t.Errorf("re-spawn parked behind a sibling's handle must not trip the gate, got: %v", err)
			}
		})
	}

	// The widening must not become a bypass: requesting_task is only honored
	// when it names THIS task, and the handle must still be open.
	for _, tc := range []struct {
		name   string
		handle map[string]any
	}{
		{
			// A sibling's handle parking a DIFFERENT task, merely blocking
			// ours, is a real blocker — not our park.
			name: "requesting_task names a different task",
			handle: map[string]any{
				"id": "gd-HANDLE01", "type": "escalation", "status": "open",
				"created_by": "gardenio-review-7le",
				"fields": map[string]string{
					"requesting_task":  "gd-OTHERWORK",
					"requesting_agent": "gd-BEAD7LE",
				},
			},
		},
		{
			name: "sibling handle already closed — nothing left to respawn on",
			handle: map[string]any{
				"id": "gd-HANDLE01", "type": "escalation", "status": "closed",
				"created_by": "gardenio-review-7le",
				"fields": map[string]string{
					"requesting_task":  "gd-WORKED01",
					"requesting_agent": "gd-BEAD7LE",
				},
			},
		},
		{
			// requesting_task must not turn a non-handle bead into a park.
			name: "non-handle bead carrying requesting_task",
			handle: map[string]any{
				"id": "gd-HANDLE01", "type": "task", "status": "open",
				"created_by": "gardenio-review-7le",
				"fields":     map[string]string{"requesting_task": "gd-WORKED01"},
			},
		},
	} {
		t.Run(tc.name+" -> gate still fires", func(t *testing.T) {
			parkFixture(t, claimed,
				map[string][]string{"gd-WORKED01": {"gd-HANDLE01"}},
				map[string]map[string]any{"gd-HANDLE01": tc.handle})

			err := checkClaimedWork(context.Background(), "gd-BEADX78")
			if err == nil {
				t.Fatal("gate must still fire; got nil (exemption widened into a bypass)")
			}
			if !strings.Contains(err.Error(), "gd-WORKED01") {
				t.Errorf("headline should name the unparked bead, got: %q", err.Error())
			}
		})
	}
}

// TestShouldSuspend covers the suspend-vs-done decision matrix. SUSPEND only
// when the agent is freeze-eligible AND neither --done nor --clean was passed;
// a runc (non-eligible) agent always takes the terminal done path unchanged.
func TestShouldSuspend(t *testing.T) {
	cases := []struct {
		name     string
		eligible bool
		done     bool
		clean    bool
		want     bool
	}{
		{"freeze-eligible, plain stop -> suspend", true, false, false, true},
		{"freeze-eligible, --done -> terminal", true, true, false, false},
		{"freeze-eligible, --clean -> terminal", true, false, true, false},
		{"freeze-eligible, --done --clean -> terminal", true, true, true, false},
		{"runc, plain stop -> terminal (unchanged)", false, false, false, false},
		{"runc, --done -> terminal", false, true, false, false},
		{"runc, --clean -> terminal", false, false, true, false},
	}

	// shouldSuspend reads package-level flag vars + the env signal; save/restore.
	origDone, origClean := stopDone, stopClean
	t.Cleanup(func() { stopDone, stopClean = origDone, origClean })

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			stopDone = c.done
			stopClean = c.clean
			if c.eligible {
				t.Setenv("BOAT_FREEZE_ELIGIBLE", "true")
			} else {
				t.Setenv("BOAT_FREEZE_ELIGIBLE", "")
			}
			if got := shouldSuspend(); got != c.want {
				t.Errorf("shouldSuspend() = %v, want %v", got, c.want)
			}
		})
	}
}

// ── gb stop parks an open PR behind a merge escalation (gd-x7HUu7s3gD) ─────

// prParkFakeDaemon serves the agent bead, its claimed tasks and the open
// escalations the dedupe lists.
type prParkFakeDaemon struct {
	beads       map[string]*beadsapi.BeadDetail
	claimed     []*beadsapi.BeadDetail
	escalations []*beadsapi.BeadDetail
}

func (f *prParkFakeDaemon) GetBead(_ context.Context, id string) (*beadsapi.BeadDetail, error) {
	if b, ok := f.beads[id]; ok {
		return b, nil
	}
	return nil, fmt.Errorf("bead %s not found", id)
}

func (f *prParkFakeDaemon) ListAssignedTasks(_ context.Context, _ string) ([]*beadsapi.BeadDetail, error) {
	return f.claimed, nil
}

func (f *prParkFakeDaemon) ListBeadsFiltered(_ context.Context, _ beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	return &beadsapi.ListBeadsResult{Beads: f.escalations, Total: len(f.escalations)}, nil
}

// fakePRPark fakes gh (prs maps a PR URL to its `gh pr view` JSON; the
// current branch has no PR) and gb (each call is recorded and answers with a
// fresh escalation id).
func fakePRPark(t *testing.T, prs map[string]string) *[][]string {
	t.Helper()
	oldGH, oldGB := prGHRun, prGBRun
	t.Cleanup(func() { prGHRun, prGBRun = oldGH, oldGB })
	prGHRun = func(_ context.Context, args ...string) (string, error) {
		if len(args) < 3 || args[0] != "pr" || args[1] != "view" {
			return "", fmt.Errorf("unexpected gh %v", args)
		}
		if out, ok := prs[args[2]]; ok {
			return out, nil
		}
		return "", fmt.Errorf("no pull requests found for branch")
	}
	var calls [][]string
	prGBRun = func(_ context.Context, args ...string) (string, error) {
		calls = append(calls, args)
		return "escalation_id=gd-NEWESC\n", nil
	}
	return &calls
}

const (
	parkPRURL = "https://github.com/acme/app/pull/42"
	parkAgent = "gd-agent"
	parkTask  = "gd-task"
)

func parkPRJSON(state string, draft bool) string {
	return fmt.Sprintf(`{"number":42,"url":%q,"title":"Fix the thing","state":%q,"isDraft":%t,"headRefName":"agent/fix","headRefOid":"abc123"}`,
		parkPRURL, state, draft)
}

// parkDaemon is an agent assigned parkTask, which records parkPRURL.
func parkDaemon(escalations ...*beadsapi.BeadDetail) *prParkFakeDaemon {
	task := &beadsapi.BeadDetail{ID: parkTask, Kind: "issue", Status: "in_progress", Fields: map[string]string{"mr_url": parkPRURL}}
	return &prParkFakeDaemon{
		beads: map[string]*beadsapi.BeadDetail{
			parkAgent: {ID: parkAgent, Fields: map[string]string{"task_id": parkTask}},
			parkTask:  task,
		},
		claimed:     []*beadsapi.BeadDetail{task},
		escalations: escalations,
	}
}

func hasArgPair(args []string, flag, value string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag && args[i+1] == value {
			return true
		}
	}
	return false
}

func TestParkOpenPRs_OpenWithEscalationFilesNothing(t *testing.T) {
	calls := fakePRPark(t, map[string]string{parkPRURL: parkPRJSON("OPEN", false)})
	d := parkDaemon(&beadsapi.BeadDetail{
		ID: "gd-esc", Type: "escalation", Status: "open",
		Fields: map[string]string{"operation_type": "gitlab_mr_merge", "operation_spec": `{"mr_url":"` + parkPRURL + `"}`},
	})
	results := parkOpenPRs(context.Background(), d, parkAgent, "crew-a", nil)
	if len(results) != 0 || len(*calls) != 0 {
		t.Fatalf("PR already in the desk queue: want nothing filed, got results %+v, gb calls %v", results, *calls)
	}
}

func TestParkOpenPRs_OpenWithoutEscalationFilesOne(t *testing.T) {
	calls := fakePRPark(t, map[string]string{parkPRURL: parkPRJSON("OPEN", false)})
	// An open merge escalation for a different PR does not count.
	d := parkDaemon(&beadsapi.BeadDetail{
		ID: "gd-other", Type: "escalation", Status: "open",
		Fields: map[string]string{"operation_type": "gitlab_mr_merge", "operation_spec": `{"mr_url":"https://github.com/acme/app/pull/7"}`},
	})
	results := parkOpenPRs(context.Background(), d, parkAgent, "crew-a", &WrapUp{PullRequests: []string{parkPRURL}})
	if len(results) != 1 || !results[0].Filed || results[0].EscalationID != "gd-NEWESC" || results[0].Task != parkTask {
		t.Fatalf("want one filed escalation parking %s, got %+v", parkTask, results)
	}
	if len(*calls) != 1 {
		t.Fatalf("want one gb call, got %v", *calls)
	}
	args := (*calls)[0]
	if args[0] != "escalation" || args[1] != "create" ||
		!hasArgPair(args, "--operation-type", "gitlab_mr_merge") ||
		!hasArgPair(args, "--requesting-task", parkTask) ||
		!strings.Contains(strings.Join(args, " "), "--close-on-execute") ||
		!hasArgPair(args, "--operation-spec", `{"mr_url":"`+parkPRURL+`"}`) {
		t.Fatalf("escalation argv = %v", args)
	}
}

func TestParkOpenPRs_MergedPRFilesNothing(t *testing.T) {
	calls := fakePRPark(t, map[string]string{parkPRURL: parkPRJSON("MERGED", false)})
	results := parkOpenPRs(context.Background(), parkDaemon(), parkAgent, "crew-a", &WrapUp{PullRequests: []string{parkPRURL}})
	if len(results) != 0 || len(*calls) != 0 {
		t.Fatalf("merged PR: want nothing filed, got results %+v, gb calls %v", results, *calls)
	}
}

func TestParkOpenPRs_DraftGoesThroughPRReady(t *testing.T) {
	calls := fakePRPark(t, map[string]string{parkPRURL: parkPRJSON("OPEN", true)})
	results := parkOpenPRs(context.Background(), parkDaemon(), parkAgent, "crew-a", nil)
	if len(results) != 1 || !results[0].Filed {
		t.Fatalf("want the draft readied and filed, got %+v", results)
	}
	want := []string{"pr", "ready", parkPRURL, "--task", parkTask}
	if got := (*calls)[0]; strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("gb argv = %v, want %v", got, want)
	}
}

// A task that holds its PR (gd-L9Bfqv2Ndn) keeps its draft a draft: the park
// runs no gb at all and reports the PR as held, so the gate can let it pass.
func TestParkOpenPRs_HeldTaskLeavesDraftAlone(t *testing.T) {
	calls := fakePRPark(t, map[string]string{parkPRURL: parkPRJSON("OPEN", true)})
	d := parkDaemon()
	d.beads[parkTask].Labels = []string{"project:gardenio", "pr:hold"}
	results := parkOpenPRs(context.Background(), d, parkAgent, "crew-a", &WrapUp{PullRequests: []string{parkPRURL}})
	if len(*calls) != 0 {
		t.Fatalf("held PR: want no gb call (no pr ready, no escalation), got %v", *calls)
	}
	if len(results) != 1 || results[0].Filed || results[0].Task != parkTask || !strings.Contains(results[0].Held, "pr:hold label") {
		t.Fatalf("want one held result for %s, got %+v", parkTask, results)
	}
	if got := heldPRTasks(results); got[parkTask] == "" {
		t.Errorf("heldPRTasks = %v, want %s", got, parkTask)
	}
	if note := prParkNote(results); !strings.Contains(note, "left "+parkPRURL+" as it is") {
		t.Errorf("note = %q", note)
	}
}

// A hold on a task whose PR is merged or closed holds nothing: the PR is
// skipped as before and the task is not reported as held, so the claimed-work
// gate still tells the crew to close it.
func TestParkOpenPRs_HoldOnMergedPRIsNotHeld(t *testing.T) {
	for _, state := range []string{"MERGED", "CLOSED"} {
		t.Run(state, func(t *testing.T) {
			calls := fakePRPark(t, map[string]string{parkPRURL: parkPRJSON(state, false)})
			d := parkDaemon()
			d.beads[parkTask].Labels = []string{"pr:hold"}
			results := parkOpenPRs(context.Background(), d, parkAgent, "crew-a", nil)
			if len(results) != 0 || len(*calls) != 0 {
				t.Fatalf("%s PR on a held task: want no result and no gb call, got %+v, calls %v", state, results, *calls)
			}
			if got := heldPRTasks(results); len(got) != 0 {
				t.Errorf("heldPRTasks = %v, want empty", got)
			}
		})
	}
}

// Without a hold a draft is readied as before, and the stop output says so.
func TestParkOpenPRs_ReadiedDraftIsNoted(t *testing.T) {
	calls := fakePRPark(t, map[string]string{parkPRURL: parkPRJSON("OPEN", true)})
	results := parkOpenPRs(context.Background(), parkDaemon(), parkAgent, "crew-a", nil)
	if len(results) != 1 || !results[0].Filed || !results[0].Readied || results[0].Held != "" || len(*calls) != 1 {
		t.Fatalf("want the draft readied and filed, got %+v, gb calls %v", results, *calls)
	}
	if note := prParkNote(results); !strings.Contains(note, "marked draft "+parkPRURL+" ready") {
		t.Errorf("note should say the draft was readied, got %q", note)
	}
}

func TestParkOpenPRs_UnreadablePRIsReported(t *testing.T) {
	calls := fakePRPark(t, map[string]string{})
	results := parkOpenPRs(context.Background(), parkDaemon(), parkAgent, "crew-a", nil)
	if len(results) != 1 || results[0].Filed || results[0].Err == nil || len(*calls) != 0 {
		t.Fatalf("gh failure: want one unfiled result with an error and no gb call, got %+v, %v", results, *calls)
	}
}

func TestPRParkEnabled(t *testing.T) {
	t.Setenv("BOAT_ROLE", "crew")
	t.Setenv("GB_STOP_PR_PARK", "")
	if !prParkEnabled(false, nil, nil) {
		t.Error("crew stop: want enabled")
	}
	if prParkEnabled(true, nil, nil) {
		t.Error("--force: want disabled")
	}
	if prParkEnabled(false, fmt.Errorf("unpushed"), nil) {
		t.Error("unpushed head: want disabled")
	}
	if prParkEnabled(false, nil, &WrapUp{GoalUnachievable: true}) {
		t.Error("goal unachievable: want disabled")
	}
	t.Setenv("BOAT_ROLE", "reviewer")
	if prParkEnabled(false, nil, nil) {
		t.Error("reviewer: want disabled")
	}
	t.Setenv("BOAT_ROLE", "crew")
	t.Setenv("GB_STOP_PR_PARK", "off")
	if prParkEnabled(false, nil, nil) {
		t.Error("kill switch: want disabled")
	}
}

func TestApplyPRParkToWrapUp(t *testing.T) {
	w := &WrapUp{HandoffNotes: "Rebased on main.", PullRequests: []string{"https://github.com/acme/app/pull/7"}}
	applyPRParkToWrapUp(w, []prParkResult{{URL: parkPRURL, Task: parkTask, EscalationID: "gd-NEWESC", Filed: true}})
	if !strings.HasPrefix(w.HandoffNotes, "Rebased on main.\n") ||
		!strings.Contains(w.HandoffNotes, "gb stop filed merge escalation gd-NEWESC for "+parkPRURL) {
		t.Errorf("handoff_notes = %q", w.HandoffNotes)
	}
	if len(w.PullRequests) != 2 || w.PullRequests[1] != parkPRURL {
		t.Errorf("pull_requests = %v", w.PullRequests)
	}
}
