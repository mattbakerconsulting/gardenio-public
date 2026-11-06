package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

const (
	resolveJSBudget = "gardenio/controller/internal/gardenhub/testdata/jsbudget/pr-a.txt"
	resolveGolden   = "gardenio/controller/internal/gardenhub/testdata/golden/porch.png"
)

// fakePRDetails serves PR details; after, when set, runs once the detail is
// read (a crew push racing the resolver).
type fakePRDetails struct {
	details map[int]prDetail
	after   func()
	reads   int
}

func (f *fakePRDetails) PRDetail(_ context.Context, _ string, n int) (prDetail, error) {
	f.reads++
	d, ok := f.details[n]
	if !ok {
		return prDetail{}, fmt.Errorf("PR %d not found", n)
	}
	if f.after != nil {
		f.after()
		f.after = nil
	}
	return d, nil
}

type fakeComments struct {
	mu    sync.Mutex
	posts []string
}

func (c *fakeComments) HasMarker(context.Context, prRef, string) (bool, error) { return false, nil }

func (c *fakeComments) Post(_ context.Context, _ prRef, body string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.posts = append(c.posts, body)
	return nil
}

type resolveFixture struct {
	r        *conflictRepo
	scanner  *conflictScanner
	git      *recordingGit
	res      *mechanicalResolver
	prs      *fakePRDetails
	comments *fakeComments
	reg      *prometheus.Registry
}

func newResolveFixture(t *testing.T, r *conflictRepo, d *conflictFakeBeads, open fixedPRs) *resolveFixture {
	t.Helper()
	s, g := newTestScanner(t, r, d, open)
	f := &resolveFixture{r: r, scanner: s, git: g, prs: &fakePRDetails{details: map[int]prDetail{}}, comments: &fakeComments{}, reg: prometheus.NewRegistry()}
	res, err := newMechanicalResolver("o/r", s.mirror, f.prs, f.comments, "", f.reg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	s.resolver = res
	f.res = res
	return f
}

// openPR registers PR n as an open same-repo PR on branch at head.
func (f *resolveFixture) openPR(n int, branch, head string) {
	f.r.git(f.r.work, "push", "-q", "-f", "origin", branch+":refs/heads/"+branch)
	f.r.pushPR(n, branch)
	f.prs.details[n] = prDetail{State: "open", HeadRef: branch, HeadSHA: head, HeadRepo: "o/r"}
}

func (f *resolveFixture) originRef(ref string) string {
	return f.r.git(f.r.origin, "rev-parse", ref)
}

// count reads the counter of result and reason ("" unless aborted).
func (f *resolveFixture) count(result, reason string) float64 {
	return testutil.ToFloat64(f.res.results.WithLabelValues(result, reason))
}

// jsbudgetConflict commits main and PR branch "agent/a" (PR 1) both appending
// to the same jsbudget entry file, and returns the PR head.
func jsbudgetConflict(r *conflictRepo, extra map[string]string) string {
	base := r.commit("main", "", "init", map[string]string{resolveJSBudget: "a.js 10\nb.js 20\n", "page.go": lines(10, nil)})
	r.commit("main", "", "main grows", map[string]string{resolveJSBudget: "a.js 10\nb.js 20\nmain.js 30\n"})
	r.pushMain()
	files := map[string]string{resolveJSBudget: "a.js 10\nb.js 20\npr.js 40\n", "pr.go": "package pr\n"}
	for k, v := range extra {
		files[k] = v
	}
	return r.commit("agent/a", base, "pr", files)
}

func TestResolveJSBudgetUnion(t *testing.T) {
	r := newConflictRepo(t)
	head := jsbudgetConflict(r, nil)
	d := &conflictFakeBeads{}
	d.add("gd-mr1", "merge-request", conflictPRURL(1))
	f := newResolveFixture(t, r, d, prsOf(1))
	f.openPR(1, "agent/a", head)

	scan(t, f.scanner)

	got := f.originRef("refs/heads/agent/a")
	if got == head {
		t.Fatal("the branch was not pushed")
	}
	if parents := f.r.git(r.origin, "rev-list", "--parents", "-n1", got); parents != got+" "+head+" "+f.originRef("refs/heads/main") {
		t.Errorf("parents = %q, want the PR head then main", parents)
	}
	if body := f.r.git(r.origin, "show", got+":"+resolveJSBudget); body != "a.js 10\nb.js 20\nmain.js 30\npr.js 40" {
		t.Errorf("union = %q", body)
	}
	if body := f.r.git(r.origin, "show", got+":pr.go"); body != "package pr" {
		t.Errorf("the PR's other change was lost: %q", body)
	}
	if author := f.r.git(r.origin, "log", "-1", "--format=%an <%ae>|%s", got); author != defaultResolveAuthor+"|merge main: resolve mechanical conflicts ("+resolveJSBudget+")" {
		t.Errorf("author|subject = %q", author)
	}
	if out := f.r.git(r.origin, "merge-tree", "--write-tree", "--name-only", "main", got); strings.Contains(out, "\n") {
		t.Errorf("merge-tree against main not clean:\n%s", out)
	}
	if n := f.count(resolveOK, ""); n != 1 {
		t.Errorf("ok = %v, want 1", n)
	}
	if len(f.comments.posts) != 1 || !strings.Contains(f.comments.posts[0], "`"+resolveJSBudget+"`: union of lines") {
		t.Errorf("comments = %q", f.comments.posts)
	}

	// The same main push: no second bot merge, not even for a new head.
	f.prs.details[1] = prDetail{State: "open", HeadRef: "agent/a", HeadSHA: got, HeadRepo: "o/r"}
	if res := f.res.Resolve(context.Background(), prsOf(1)[0], f.originRef("refs/heads/main"), got); res != "" {
		t.Errorf("second resolve on the same main = %q, want skipped", res)
	}
}

func TestResolveHandConflictAbortsWithNoPush(t *testing.T) {
	r := newConflictRepo(t)
	base := r.commit("main", "", "init", map[string]string{resolveJSBudget: "a.js 10\n", "page.go": lines(10, nil)})
	r.commit("main", "", "main", map[string]string{resolveJSBudget: "a.js 10\nmain.js 30\n", "page.go": lines(10, map[int]string{3: "main 3"})})
	r.pushMain()
	head := r.commit("agent/a", base, "pr", map[string]string{resolveJSBudget: "a.js 10\npr.js 40\n", "page.go": lines(10, map[int]string{3: "pr 3"})})
	f := newResolveFixture(t, r, &conflictFakeBeads{}, prsOf(1))
	f.openPR(1, "agent/a", head)
	if err := f.scanner.mirror.ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := f.scanner.mirror.fetch(context.Background()); err != nil {
		t.Fatal(err)
	}

	res := f.res.Resolve(context.Background(), prsOf(1)[0], f.originRef("refs/heads/main"), head)

	if res != resolveAborted {
		t.Errorf("result = %q, want aborted", res)
	}
	if n := f.count(resolveAborted, reasonHandConflict); n != 1 {
		t.Errorf("aborted{reason=%s} = %v, want 1", reasonHandConflict, n)
	}
	if got := f.originRef("refs/heads/agent/a"); got != head {
		t.Errorf("branch moved to %s on an abort", got)
	}
	for _, c := range f.git.calls {
		if slices.Contains(c, "push") || slices.Contains(c, "commit-tree") {
			t.Errorf("an aborted resolve ran %q", c)
		}
	}
	if len(f.comments.posts) != 0 {
		t.Errorf("comments on an abort: %q", f.comments.posts)
	}
}

// TestResolveLeaseLostOverwritesNothing is the control: a crew push between
// the scan and the bot's push must survive.
func TestResolveLeaseLostOverwritesNothing(t *testing.T) {
	r := newConflictRepo(t)
	head := jsbudgetConflict(r, nil)
	f := newResolveFixture(t, r, &conflictFakeBeads{}, prsOf(1))
	f.openPR(1, "agent/a", head)
	var crew string
	f.prs.after = func() {
		crew = r.commit("agent/a", "", "crew pushes", map[string]string{"crew.go": "package crew\n"})
		r.git(r.work, "push", "-q", "origin", "agent/a:refs/heads/agent/a")
	}

	scan(t, f.scanner)

	if crew == "" {
		t.Fatal("the race never ran")
	}
	if got := f.originRef("refs/heads/agent/a"); got != crew {
		t.Errorf("branch = %s, want the crew push %s kept", got, crew)
	}
	if n := f.count(resolveLeaseLost, ""); n != 1 {
		t.Errorf("lease_lost = %v, want 1", n)
	}
	if len(f.comments.posts) != 0 {
		t.Errorf("comments after a lost lease: %q", f.comments.posts)
	}
}

func TestResolveSkipsForkAndOptOut(t *testing.T) {
	for name, d := range map[string]prDetail{
		"fork":         {State: "open", HeadRef: "agent/a", HeadRepo: "someone/r"},
		"deleted fork": {State: "open", HeadRef: "agent/a"},
		"no-bot-merge": {State: "open", HeadRef: "agent/a", HeadRepo: "o/r", Labels: []string{"bug", noBotMergeLabel}},
	} {
		t.Run(name, func(t *testing.T) {
			r := newConflictRepo(t)
			head := jsbudgetConflict(r, nil)
			f := newResolveFixture(t, r, &conflictFakeBeads{}, prsOf(1))
			f.openPR(1, "agent/a", head)
			d.HeadSHA = head
			f.prs.details[1] = d

			scan(t, f.scanner)

			if f.prs.reads != 1 {
				t.Errorf("PR detail reads = %d, want 1", f.prs.reads)
			}
			if got := f.originRef("refs/heads/agent/a"); got != head {
				t.Errorf("branch pushed to %s", got)
			}
			for _, c := range f.git.calls {
				if slices.Contains(c, "push") {
					t.Errorf("skipped PR ran %q", c)
				}
			}
		})
	}
}

func TestResolveGoldenTakesMain(t *testing.T) {
	r := newConflictRepo(t)
	jsbudgetConflict(r, nil)
	r.commit("main", "", "main golden", map[string]string{resolveGolden: "main png\n"})
	r.pushMain()
	head := r.commit("agent/a", "", "pr golden", map[string]string{resolveGolden: "pr png\n"})
	f := newResolveFixture(t, r, &conflictFakeBeads{}, prsOf(1))
	f.openPR(1, "agent/a", head)

	scan(t, f.scanner)

	got := f.originRef("refs/heads/agent/a")
	if body := f.r.git(r.origin, "show", got+":"+resolveGolden); body != "main png" {
		t.Errorf("golden = %q, want main's", body)
	}
	if len(f.comments.posts) != 1 || !strings.Contains(f.comments.posts[0], "`"+resolveGolden+"`: main's version") ||
		!strings.Contains(f.comments.posts[0], "Goldens never go in a feature PR") {
		t.Errorf("comments = %q", f.comments.posts)
	}
}

func TestResolveTokenNeverInArgv(t *testing.T) {
	const readSecret, pushSecret = "ghs_readSECRET", "ghs_pushSECRET"
	r := newConflictRepo(t)
	head := jsbudgetConflict(r, nil)
	f := newResolveFixture(t, r, &conflictFakeBeads{}, prsOf(1))
	f.openPR(1, "agent/a", head)
	f.scanner.mirror.askpass = "/bin/false"
	f.scanner.mirror.token = func(context.Context) (string, error) { return readSecret, nil }
	f.res.pushToken = func(context.Context) (string, error) { return pushSecret, nil }

	scan(t, f.scanner)

	if n := f.count(resolveOK, ""); n != 1 {
		t.Fatalf("ok = %v, want 1: the control must reach the push", n)
	}
	sawPush := false
	for i, argv := range f.git.calls {
		for _, a := range argv {
			if strings.Contains(a, readSecret) || strings.Contains(a, pushSecret) {
				t.Errorf("token in argv: %q", argv)
			}
		}
		if slices.Contains(argv, "push") {
			sawPush = slices.Contains(f.git.envs[i], askpassTokenEnv+"="+pushSecret)
			if slices.Contains(f.git.envs[i], askpassTokenEnv+"="+readSecret) {
				t.Error("the push carried the read token")
			}
		}
	}
	if !sawPush {
		t.Error("the push token never reached git push's environment")
	}
}

// TestResolveReadOnlyTempDir is gd-qDrQYJB726: the bridge pod's root
// filesystem is read-only, so a temporary index under os.TempDir() aborted
// every live attempt. Only the mirror's volume may be written.
func TestResolveReadOnlyTempDir(t *testing.T) {
	r := newConflictRepo(t)
	head := jsbudgetConflict(r, nil)
	f := newResolveFixture(t, r, &conflictFakeBeads{}, prsOf(1))
	f.openPR(1, "agent/a", head)
	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "absent"))

	scan(t, f.scanner)

	if n := f.count(resolveOK, ""); n != 1 {
		t.Errorf("ok = %v, want 1 with no writable TMPDIR (aborted build_error = %v)", n, f.count(resolveAborted, reasonBuildError))
	}
	if left, _ := filepath.Glob(filepath.Join(f.scanner.mirror.dir, "resolve-*")); len(left) != 0 {
		t.Errorf("temporary index left in the mirror: %q", left)
	}
}

// TestResolveFetchesOnlyConflictedBlobs: building the merge reads the blobs of
// the conflicted files and no other, so the blobless mirror stays blobless
// (gd-qDrQYJB726: write-tree without --missing-ok fetched all of main's).
func TestResolveFetchesOnlyConflictedBlobs(t *testing.T) {
	r := newConflictRepo(t)
	head := jsbudgetConflict(r, nil)
	f := newResolveFixture(t, r, &conflictFakeBeads{}, prsOf(1))
	f.openPR(1, "agent/a", head)

	scan(t, f.scanner)

	if n := f.count(resolveOK, ""); n != 1 {
		t.Fatalf("ok = %v, want 1", n)
	}
	unrelated := f.originRef("refs/heads/main:page.go")
	missing := f.r.git(f.scanner.mirror.dir, "rev-list", "--objects", "--missing=print", "refs/heads/main")
	if !strings.Contains(missing, "?"+unrelated) {
		t.Errorf("the blob of page.go, which neither side changed, was fetched into the mirror")
	}
}

func TestResolveEveryMechanicalPatternHasARule(t *testing.T) {
	for _, pat := range MechanicalConflictPaths {
		if resolveRules[pat] == "" {
			t.Errorf("MechanicalConflictPaths entry %q has no resolveRules entry", pat)
		}
	}
	if len(resolveRules) != len(MechanicalConflictPaths) {
		t.Errorf("resolveRules has %d entries for %d mechanical patterns", len(resolveRules), len(MechanicalConflictPaths))
	}
	if ruleFor("page.go") != "" || ruleFor(resolveJSBudget) != ruleUnion || ruleFor(resolveGolden) != ruleTakeMain ||
		ruleFor("scripts/ratchets/line-budget.d/pr-a.tsv") != ruleUnion {
		t.Error("ruleFor maps a path to the wrong rule")
	}
}

func TestResolveUnionLines(t *testing.T) {
	for _, tc := range []struct{ main, pr, want string }{
		{"a\nb\n", "a\nc\n", "a\nb\nc\n"},
		{"a\nb", "b\na\n", "a\nb\n"},
		{"", "x\n", "x\n"},
		{"a\n", "c\nb\nc\n", "a\nc\nb\n"},
	} {
		if got := string(unionLines([]byte(tc.main), []byte(tc.pr))); got != tc.want {
			t.Errorf("unionLines(%q, %q) = %q, want %q", tc.main, tc.pr, got, tc.want)
		}
	}
}

func TestResolveSubjectCapsFiles(t *testing.T) {
	got := resolveSubject([]string{"a", "b", "c", "d", "e"})
	if want := "merge main: resolve mechanical conflicts (a, b, c, +2 more)"; got != want {
		t.Errorf("subject = %q, want %q", got, want)
	}
	if _, err := newMechanicalResolver("o/r", &gitMirror{dir: filepath.Join(t.TempDir(), "m")}, nil, nil, "no email", nil, slog.Default()); err == nil {
		t.Error("an author without an email was accepted")
	}
}
