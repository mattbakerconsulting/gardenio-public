package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// conflictRepo is a fixture: a working repo whose main and PR branches are
// pushed to a bare origin that serves partial clones over file://.
type conflictRepo struct {
	t      *testing.T
	work   string
	origin string
	home   string
}

func newConflictRepo(t *testing.T) *conflictRepo {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	r := &conflictRepo{t: t, work: t.TempDir(), origin: t.TempDir(), home: t.TempDir()}
	r.git(r.work, "init", "-q", "--initial-branch=main")
	r.git(r.origin, "init", "-q", "--bare", "--initial-branch=main")
	r.git(r.origin, "config", "uploadpack.allowFilter", "true")
	r.git(r.origin, "config", "uploadpack.allowAnySHA1InWant", "true")
	r.git(r.work, "remote", "add", "origin", r.origin)
	return r
}

func (r *conflictRepo) git(dir string, args ...string) string {
	r.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "HOME="+r.home, "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// commit writes files on branch (created from start when new) and returns the
// commit sha.
func (r *conflictRepo) commit(branch, start, msg string, files map[string]string) string {
	r.t.Helper()
	switch {
	case start != "":
		r.git(r.work, "checkout", "-q", "-B", branch, start)
	case exec.Command("git", "-C", r.work, "rev-parse", "--verify", "-q", "HEAD").Run() == nil:
		r.git(r.work, "checkout", "-q", branch)
	}
	for name, body := range files {
		p := filepath.Join(r.work, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			r.t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			r.t.Fatal(err)
		}
	}
	r.git(r.work, "add", "-A")
	r.git(r.work, "commit", "-q", "-m", msg)
	return r.git(r.work, "rev-parse", "HEAD")
}

func (r *conflictRepo) pushMain() {
	r.git(r.work, "push", "-q", "-f", "origin", "main:refs/heads/main")
}

func (r *conflictRepo) pushPR(n int, branch string) {
	r.git(r.work, "push", "-q", "-f", "origin", fmt.Sprintf("%s:refs/pull/%d/head", branch, n))
}

// lines is a file of n numbered lines with line i replaced by repl.
func lines(n int, repl map[int]string) string {
	var b strings.Builder
	for i := 1; i <= n; i++ {
		if s, ok := repl[i]; ok {
			b.WriteString(s)
		} else {
			fmt.Fprintf(&b, "line %d", i)
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// recordingGit runs real git and records every argv and environment.
type recordingGit struct {
	mu    sync.Mutex
	calls [][]string
	envs  [][]string
}

func (g *recordingGit) Run(ctx context.Context, dir string, env []string, args ...string) ([]byte, int, error) {
	g.mu.Lock()
	g.calls = append(g.calls, append([]string{"git"}, args...))
	g.envs = append(g.envs, env)
	g.mu.Unlock()
	return execGit{}.Run(ctx, dir, env, args...)
}

// conflictFakeBeads is an in-memory daemon holding beads by mr_url.
type conflictFakeBeads struct {
	mu     sync.Mutex
	beads  []*beadsapi.BeadDetail
	writes int
}

func (d *conflictFakeBeads) add(id, typ, url string) *beadsapi.BeadDetail {
	b := &beadsapi.BeadDetail{ID: id, Type: typ, Status: "open", Fields: map[string]string{"mr_url": url}}
	d.beads = append(d.beads, b)
	return b
}

func (d *conflictFakeBeads) ListBeadsFiltered(_ context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []*beadsapi.BeadDetail
	for _, b := range d.beads {
		if b.Fields["mr_url"] == q.Fields["mr_url"] {
			c := *b
			c.Fields = map[string]string{}
			for k, v := range b.Fields {
				c.Fields[k] = v
			}
			out = append(out, &c)
		}
	}
	return &beadsapi.ListBeadsResult{Beads: out}, nil
}

func (d *conflictFakeBeads) UpdateBeadFields(_ context.Context, id string, f map[string]string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.writes++
	for _, b := range d.beads {
		if b.ID == id {
			for k, v := range f {
				b.Fields[k] = v
			}
		}
	}
	return nil
}

type fixedPRs []openPR

func (p fixedPRs) OpenPRs(context.Context, string) ([]openPR, error) { return p, nil }

func conflictPRURL(n int) string { return fmt.Sprintf("https://github.com/o/r/pull/%d", n) }

func prsOf(ns ...int) fixedPRs {
	var out fixedPRs
	for _, n := range ns {
		out = append(out, openPR{Number: n, URL: conflictPRURL(n), Base: "main"})
	}
	return out
}

func newTestScanner(t *testing.T, r *conflictRepo, d *conflictFakeBeads, prs openPRLister) (*conflictScanner, *recordingGit) {
	t.Helper()
	g := &recordingGit{}
	m := &gitMirror{dir: filepath.Join(t.TempDir(), "mirror"), remote: "file://" + r.origin, git: g}
	s := newConflictScanner("o/r", m, d, prs, 0, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.now = func() time.Time { return time.Date(2026, 11, 4, 1, 2, 3, 0, time.UTC) }
	return s, g
}

func scan(t *testing.T, s *conflictScanner) {
	t.Helper()
	if err := s.ScanAll(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestConflictScanCleanAndSameLine(t *testing.T) {
	r := newConflictRepo(t)
	base := r.commit("main", "", "init", map[string]string{"a.txt": lines(20, nil), "b.txt": "b\n"})
	r.pushMain()
	r.commit("pr1", base, "pr1", map[string]string{"b.txt": "b changed\n"})
	r.pushPR(1, "pr1")
	r.commit("pr2", base, "pr2", map[string]string{"a.txt": lines(20, map[int]string{5: "pr2 line 5"})})
	r.pushPR(2, "pr2")
	r.commit("main", "", "main edits line 5", map[string]string{"a.txt": lines(20, map[int]string{5: "main line 5"})})
	r.pushMain()

	d := &conflictFakeBeads{}
	clean := d.add("gd-mr1", "merge-request", conflictPRURL(1))
	mr2 := d.add("gd-mr2", "merge-request", conflictPRURL(2))
	task2 := d.add("gd-task2", "task", conflictPRURL(2))
	s, _ := newTestScanner(t, r, d, prsOf(1, 2))
	scan(t, s)

	if got := clean.Fields[fieldMergeState]; got != mergeClean {
		t.Errorf("PR 1 state = %q, want clean", got)
	}
	// The mirror is blobless: blobs merge-tree never needed stay missing.
	missing := r.git(s.mirror.dir, "rev-list", "--objects", "--missing=print", "refs/heads/main")
	if !strings.Contains(missing, "\n?") {
		t.Errorf("mirror holds every blob; the partial-clone filter did not apply:\n%s", missing)
	}
	mainSHA := r.git(r.work, "rev-parse", "main")
	for _, b := range []*beadsapi.BeadDetail{mr2, task2} {
		want := map[string]string{
			fieldMergeState:    mergeConflicting,
			fieldConflictFiles: `["a.txt"]`,
			fieldConflictKind:  conflictHand,
			fieldConflictSince: "2026-11-04T01:02:03Z",
			fieldConflictBase:  mainSHA,
		}
		for k, v := range want {
			if b.Fields[k] != v {
				t.Errorf("%s %s = %q, want %q", b.ID, k, b.Fields[k], v)
			}
		}
		if c := b.Fields[fieldConflictCause]; c != "" {
			t.Errorf("%s cause = %q, want empty (never seen clean)", b.ID, c)
		}
	}
}

func TestConflictKindMechanical(t *testing.T) {
	const golden = "gardenio/controller/internal/gardenhub/testdata/golden/x.png"
	r := newConflictRepo(t)
	base := r.commit("main", "", "init", map[string]string{golden: "v0", "page.go": "package p\n"})
	r.pushMain()
	r.commit("pr1", base, "pr1", map[string]string{golden: "pr1"})
	r.pushPR(1, "pr1")
	r.commit("pr2", base, "pr2", map[string]string{golden: "pr2", "page.go": "package p // pr2\n"})
	r.pushPR(2, "pr2")
	r.commit("main", "", "rebless", map[string]string{golden: "main", "page.go": "package p // main\n"})
	r.pushMain()

	d := &conflictFakeBeads{}
	mr1 := d.add("gd-mr1", "merge-request", conflictPRURL(1))
	mr2 := d.add("gd-mr2", "merge-request", conflictPRURL(2))
	s, _ := newTestScanner(t, r, d, prsOf(1, 2))
	scan(t, s)

	if got := mr1.Fields[fieldConflictKind]; got != conflictMechanical {
		t.Errorf("golden-only conflict kind = %q, want mechanical (files %s)", got, mr1.Fields[fieldConflictFiles])
	}
	if got := mr2.Fields[fieldConflictKind]; got != conflictHand {
		t.Errorf("golden + page.go conflict kind = %q, want hand", got)
	}
	if got, want := mr2.Fields[fieldConflictFiles], `["`+golden+`","page.go"]`; got != want {
		t.Errorf("files = %s, want %s", got, want)
	}
}

func TestConflictCauseIsTheIntroducingCommit(t *testing.T) {
	r := newConflictRepo(t)
	base := r.commit("main", "", "init", map[string]string{"a.txt": lines(40, nil), "z.txt": "z\n"})
	r.pushMain()
	r.commit("pr5", base, "pr5", map[string]string{"a.txt": lines(40, map[int]string{3: "pr line 3"})})
	r.pushPR(5, "pr5")

	d := &conflictFakeBeads{}
	mr := d.add("gd-mr5", "merge-request", conflictPRURL(5))
	s, _ := newTestScanner(t, r, d, prsOf(5))
	scan(t, s)
	if mr.Fields[fieldMergeState] != mergeClean {
		t.Fatalf("first scan state = %q, want clean", mr.Fields[fieldMergeState])
	}

	r.commit("main", "", "chore: unrelated before (#76)", map[string]string{"z.txt": "z1\n"})
	cause := r.commit("main", "", "fix: edit line 3 (#77)", map[string]string{"a.txt": lines(40, map[int]string{3: "main line 3"})})
	r.commit("main", "", "fix: edit line 30 (#78)", map[string]string{"a.txt": lines(40, map[int]string{3: "main line 3", 30: "main line 30"})})
	r.commit("main", "", "chore: unrelated after (#79)", map[string]string{"z.txt": "z2\n"})
	r.pushMain()
	scan(t, s)

	if got, want := mr.Fields[fieldConflictCause], cause+" #77"; got != want {
		t.Errorf("cause = %q, want %q", got, want)
	}
}

func TestConflictCauseThroughMergeCommit(t *testing.T) {
	r := newConflictRepo(t)
	base := r.commit("main", "", "init", map[string]string{"a.txt": lines(10, nil)})
	r.pushMain()
	r.commit("pr5", base, "pr5", map[string]string{"a.txt": lines(10, map[int]string{2: "pr"})})
	r.pushPR(5, "pr5")

	d := &conflictFakeBeads{}
	mr := d.add("gd-mr5", "merge-request", conflictPRURL(5))
	s, _ := newTestScanner(t, r, d, prsOf(5))
	scan(t, s)

	cause := r.commit("feat", base, "edit line 2", map[string]string{"a.txt": lines(10, map[int]string{2: "feat"})})
	r.git(r.work, "checkout", "-q", "main")
	r.git(r.work, "merge", "-q", "--no-ff", "-m", "Merge pull request #81 from o/feat", "feat")
	r.pushMain()
	scan(t, s)

	if got, want := mr.Fields[fieldConflictCause], cause+" #81"; got != want {
		t.Errorf("cause = %q, want %q", got, want)
	}
}

// TestConflictScanWritesOnlyOnChange is the control for write-on-change: a
// second scan with nothing changed, and a third after an unrelated main push
// (only mr_conflict_base would differ), write nothing.
func TestConflictScanWritesOnlyOnChange(t *testing.T) {
	r := newConflictRepo(t)
	base := r.commit("main", "", "init", map[string]string{"a.txt": lines(20, nil), "z.txt": "z\n"})
	r.pushMain()
	r.commit("pr1", base, "pr1", map[string]string{"z.txt": "pr\n"})
	r.pushPR(1, "pr1")
	r.commit("pr2", base, "pr2", map[string]string{"a.txt": lines(20, map[int]string{1: "pr"})})
	r.pushPR(2, "pr2")
	r.commit("main", "", "main", map[string]string{"a.txt": lines(20, map[int]string{1: "main"})})
	r.pushMain()

	d := &conflictFakeBeads{}
	d.add("gd-mr1", "merge-request", conflictPRURL(1))
	mr2 := d.add("gd-mr2", "merge-request", conflictPRURL(2))
	d.add("gd-task2", "task", conflictPRURL(2))
	s, _ := newTestScanner(t, r, d, prsOf(1, 2))
	scan(t, s)
	if d.writes != 3 {
		t.Fatalf("first scan writes = %d, want 3", d.writes)
	}
	firstBase := mr2.Fields[fieldConflictBase]

	scan(t, s)
	if d.writes != 3 {
		t.Errorf("second scan with no change wrote %d times", d.writes-3)
	}

	r.commit("main", "", "unrelated", map[string]string{"b.txt": "new\n"})
	r.pushMain()
	scan(t, s)
	if d.writes != 3 {
		t.Errorf("scan after an unrelated main push wrote %d times; base alone is not a change", d.writes-3)
	}
	if mr2.Fields[fieldConflictBase] != firstBase {
		t.Errorf("base rewritten alone: %s -> %s", firstBase, mr2.Fields[fieldConflictBase])
	}
}

func TestConflictScanMissingRefIsUnknown(t *testing.T) {
	r := newConflictRepo(t)
	r.commit("main", "", "init", map[string]string{"a.txt": "a\n"})
	r.pushMain()

	d := &conflictFakeBeads{}
	mr := d.add("gd-mr9", "merge-request", conflictPRURL(9))
	s, _ := newTestScanner(t, r, d, prsOf(9))
	scan(t, s)
	if got := mr.Fields[fieldMergeState]; got != mergeUnknown {
		t.Errorf("state = %q, want unknown", got)
	}

	// The single-PR path: the head cannot be fetched at all.
	mr.Fields[fieldMergeState] = ""
	if err := s.ScanOne(context.Background(), openPR{Number: 9, URL: conflictPRURL(9), Base: "main"}); err != nil {
		t.Fatal(err)
	}
	if got := mr.Fields[fieldMergeState]; got != mergeUnknown {
		t.Errorf("ScanOne state = %q, want unknown", got)
	}
}

func TestConflictScanPRWithoutBeadIsCounted(t *testing.T) {
	r := newConflictRepo(t)
	base := r.commit("main", "", "init", map[string]string{"a.txt": "a\n"})
	r.pushMain()
	r.commit("pr1", base, "pr1", map[string]string{"b.txt": "b\n"})
	r.pushPR(1, "pr1")

	d := &conflictFakeBeads{}
	s, _ := newTestScanner(t, r, d, prsOf(1))
	scan(t, s)
	if d.writes != 0 {
		t.Errorf("writes = %d, want 0", d.writes)
	}
}

func TestConflictScanTokenNeverInArgv(t *testing.T) {
	const secret = "ghs_conflictScanSecretToken123"
	r := newConflictRepo(t)
	base := r.commit("main", "", "init", map[string]string{"a.txt": lines(5, nil)})
	r.pushMain()
	r.commit("pr1", base, "pr1", map[string]string{"a.txt": lines(5, map[int]string{1: "pr"})})
	r.pushPR(1, "pr1")
	r.commit("main", "", "main", map[string]string{"a.txt": lines(5, map[int]string{1: "main"})})
	r.pushMain()

	d := &conflictFakeBeads{}
	d.add("gd-mr1", "merge-request", conflictPRURL(1))
	s, g := newTestScanner(t, r, d, prsOf(1))
	s.mirror.askpass = "/bin/false"
	s.mirror.token = func(context.Context) (string, error) { return secret, nil }
	scan(t, s)
	if err := s.ScanOne(context.Background(), openPR{Number: 1, URL: conflictPRURL(1), Base: "main"}); err != nil {
		t.Fatal(err)
	}

	sawEnv := false
	for i, argv := range g.calls {
		for _, a := range argv {
			if strings.Contains(a, secret) {
				t.Errorf("token in argv: %q", argv)
			}
		}
		for _, e := range g.envs[i] {
			sawEnv = sawEnv || e == askpassTokenEnv+"="+secret
		}
	}
	if !sawEnv {
		t.Error("token never reached git's environment; the control is not exercising the credential path")
	}
	if len(g.calls) < 4 {
		t.Errorf("only %d git calls recorded", len(g.calls))
	}
}

func TestConflictAskpass(t *testing.T) {
	env := map[string]string{askpassEnv: "1", askpassTokenEnv: "tok"}
	for prompt, want := range map[string]string{
		"Username for 'https://github.com': ":                "x-access-token\n",
		"Password for 'https://x-access-token@github.com': ": "tok\n",
	} {
		var b bytes.Buffer
		if !runAskpass([]string{"bridge", prompt}, func(k string) string { return env[k] }, &b) {
			t.Fatal("askpass mode not detected")
		}
		if b.String() != want {
			t.Errorf("%q -> %q, want %q", prompt, b.String(), want)
		}
	}
	if runAskpass([]string{"bridge"}, func(string) string { return "" }, io.Discard) {
		t.Error("askpass mode without the env var")
	}
}

func TestConflictFilesCapped(t *testing.T) {
	var files []string
	for i := 0; i < 60; i++ {
		files = append(files, fmt.Sprintf("f%02d", i))
	}
	var got []string
	if err := json.Unmarshal([]byte(conflictFilesJSON(files)), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != maxConflictFiles || got[0] != "f00" || got[len(got)-1] != "…+11 more" {
		t.Errorf("capped list = %d entries, first %q, last %q", len(got), got[0], got[len(got)-1])
	}
}

func TestConflictPatchClearsOnClean(t *testing.T) {
	cur := map[string]string{
		fieldMergeState: mergeConflicting, fieldConflictFiles: `["a"]`, fieldConflictKind: conflictHand,
		fieldConflictSince: "t", fieldConflictBase: "b", fieldConflictCause: "c",
	}
	p := conflictPatch(cur, conflictResult{state: mergeClean, base: "b2"}, time.Now())
	for _, f := range []string{fieldConflictFiles, fieldConflictKind, fieldConflictSince, fieldConflictBase, fieldConflictCause} {
		if v, ok := p[f]; !ok || v != "" {
			t.Errorf("%s not cleared: %q (present %v)", f, v, ok)
		}
	}
	if p[fieldMergeState] != mergeClean {
		t.Errorf("state = %q", p[fieldMergeState])
	}
	if u := conflictPatch(cur, conflictResult{state: mergeUnknown, base: "b2"}, time.Now()); len(u) != 1 || u[fieldMergeState] != mergeUnknown {
		t.Errorf("unknown patch = %v, want only the state", u)
	}
}

func TestConflictMechanicalPaths(t *testing.T) {
	for p, want := range map[string]bool{
		"gardenio/controller/internal/gardenhub/testdata/golden/a/b.png": true,
		"gardenio/controller/internal/gardenhub/testdata/jsbudget/x.txt": true,
		"gardenio/controller/internal/gardenhub/testdata/jsbudget/x.go":  false,
		"scripts/ratchets/line-budget.d/gd-x.tsv":                        true,
		"scripts/ratchets/line-budget.d/sub/gd-x.tsv":                    false,
		"gardenio/controller/internal/gardenhub/page.go":                 false,
	} {
		if got := isMechanicalPath(p); got != want {
			t.Errorf("isMechanicalPath(%q) = %v, want %v", p, got, want)
		}
	}
}

func TestConflictHandleQueues(t *testing.T) {
	s := newConflictScanner("O/R", &gitMirror{}, &conflictFakeBeads{}, fixedPRs{}, 0, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := context.Background()
	_ = s.Handle(ctx, Event{Type: "push", Repo: "o/r", Payload: []byte(`{"ref":"refs/heads/feature"}`)})
	_ = s.Handle(ctx, Event{Type: "push", Repo: "x/y", Payload: []byte(`{"ref":"refs/heads/main"}`)})
	if len(s.pushes) != 0 {
		t.Fatal("a non-main or foreign push queued a scan")
	}
	_ = s.Handle(ctx, Event{Type: "push", Repo: "o/r", Payload: []byte(`{"ref":"refs/heads/main"}`)})
	_ = s.Handle(ctx, Event{Type: "push", Repo: "o/r", Payload: []byte(`{"ref":"refs/heads/main"}`)})
	if len(s.pushes) != 1 {
		t.Errorf("main pushes queued %d, want 1 (coalesced)", len(s.pushes))
	}
	pr := `{"pull_request":{"number":7,"html_url":"u","state":"open","base":{"ref":"main"}}}`
	_ = s.Handle(ctx, Event{Type: "pull_request", Action: "synchronize", Repo: "o/r", Payload: []byte(pr)})
	_ = s.Handle(ctx, Event{Type: "pull_request", Action: "labeled", Repo: "o/r", Payload: []byte(pr)})
	if len(s.single) != 1 {
		t.Fatalf("single queue = %d, want 1", len(s.single))
	}
	if got := <-s.single; got.Number != 7 || got.URL != "u" || got.Base != "main" {
		t.Errorf("queued %+v", got)
	}
}
