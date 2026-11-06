package main

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

var testNow = time.Date(2026, 11, 4, 1, 2, 3, 0, time.UTC)

// pairMerges is the PR pairs the scan merged against each other, as "a-b",
// read from the recorded merge-tree calls whose sides are both PR heads.
func pairMerges(g *recordingGit, heads map[string]int) []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []string
	for _, argv := range g.calls {
		if !slices.Contains(argv, "merge-tree") {
			continue
		}
		a, okA := heads[argv[len(argv)-2]]
		b, okB := heads[argv[len(argv)-1]]
		if okA && okB {
			out = append(out, fmt.Sprintf("%d-%d", a, b))
		}
	}
	slices.Sort(out)
	return out
}

func TestOverlapSameLineAndDisjoint(t *testing.T) {
	r := newConflictRepo(t)
	base := r.commit("main", "", "init", map[string]string{"a.txt": lines(20, nil), "b.txt": "b\n", "c.txt": "c\n"})
	r.pushMain()
	heads := map[string]int{}
	for n, files := range map[int]map[string]string{
		1: {"a.txt": lines(20, map[int]string{5: "pr1 line 5"})},
		2: {"a.txt": lines(20, map[int]string{5: "pr2 line 5"}), "d.txt": "pr2\n"},
		3: {"b.txt": "pr3\n"},
		4: {"c.txt": "pr4\n"},
		5: {"a.txt": lines(20, map[int]string{15: "pr5 line 15"})},
	} {
		branch := fmt.Sprintf("pr%d", n)
		heads[r.commit(branch, base, branch, files)] = n
		r.pushPR(n, branch)
	}

	d := &conflictFakeBeads{}
	mr := map[int]map[string]string{}
	for n := 1; n <= 5; n++ {
		mr[n] = d.add(fmt.Sprintf("gd-mr%d", n), "merge-request", conflictPRURL(n)).Fields
	}
	task1 := d.add("gd-task1", "task", conflictPRURL(1))
	s, g := newTestScanner(t, r, d, prsOf(1, 2, 3, 4, 5))
	scan(t, s)

	want := map[int]string{
		1: `[{"pr":2,"files":["a.txt"]}]`,
		2: `[{"pr":1,"files":["a.txt"]}]`,
	}
	for n := 1; n <= 5; n++ {
		if got := mr[n][fieldOverlaps]; got != want[n] {
			t.Errorf("PR %d mr_overlaps = %q, want %q", n, got, want[n])
		}
	}
	if got := task1.Fields[fieldOverlaps]; got != want[1] {
		t.Errorf("task bead mr_overlaps = %q, want %q", got, want[1])
	}
	// Only pairs sharing a file are merged; PR 5 shares a.txt but merges
	// cleanly, and PRs 3 and 4 touch files no other PR does.
	if got, want := pairMerges(g, heads), []string{"1-2", "1-5", "2-5"}; !slices.Equal(got, want) {
		t.Errorf("pair merges = %v, want %v", got, want)
	}
	if got := testutil.ToFloat64(s.metrics.overlapPairs); got != 1 {
		t.Errorf("overlap pairs gauge = %v, want 1", got)
	}
}

func TestOverlapMechanicalFilesExcluded(t *testing.T) {
	const budget = "gardenio/controller/internal/gardenhub/testdata/jsbudget/page.txt"
	r := newConflictRepo(t)
	base := r.commit("main", "", "init", map[string]string{budget: "100\n", "a.txt": lines(20, nil)})
	r.pushMain()
	heads := map[string]int{}
	for n, files := range map[int]map[string]string{
		// 1 and 2 conflict only on the budget file: no merge at all.
		1: {budget: "101\n"},
		2: {budget: "102\n"},
		// 3 and 4 share a.txt, which merges cleanly, and conflict on the
		// budget file: merged, but not an overlap.
		3: {budget: "103\n", "a.txt": lines(20, map[int]string{1: "pr3"})},
		4: {budget: "104\n", "a.txt": lines(20, map[int]string{20: "pr4"})},
	} {
		branch := fmt.Sprintf("pr%d", n)
		heads[r.commit(branch, base, branch, files)] = n
		r.pushPR(n, branch)
	}

	d := &conflictFakeBeads{}
	for n := 1; n <= 4; n++ {
		d.add(fmt.Sprintf("gd-mr%d", n), "merge-request", conflictPRURL(n))
	}
	s, g := newTestScanner(t, r, d, prsOf(1, 2, 3, 4))
	scan(t, s)

	for _, b := range d.beads {
		if v := b.Fields[fieldOverlaps]; v != "" {
			t.Errorf("%s mr_overlaps = %q, want empty (mechanical conflicts only)", b.ID, v)
		}
	}
	if got, want := pairMerges(g, heads), []string{"3-4"}; !slices.Equal(got, want) {
		t.Errorf("pair merges = %v, want %v", got, want)
	}
}

// TestOverlapWritesOnlyOnChange is the control for write-on-change of
// mr_overlaps: an unchanged second scan writes nothing, and a PR that closes
// takes its overlap off the other PR and off its own beads.
func TestOverlapWritesOnlyOnChange(t *testing.T) {
	r := newConflictRepo(t)
	base := r.commit("main", "", "init", map[string]string{"a.txt": lines(20, nil)})
	r.pushMain()
	r.commit("pr1", base, "pr1", map[string]string{"a.txt": lines(20, map[int]string{3: "pr1"})})
	r.pushPR(1, "pr1")
	r.commit("pr2", base, "pr2", map[string]string{"a.txt": lines(20, map[int]string{3: "pr2"})})
	r.pushPR(2, "pr2")

	d := &conflictFakeBeads{}
	mr1 := d.add("gd-mr1", "merge-request", conflictPRURL(1))
	mr2 := d.add("gd-mr2", "merge-request", conflictPRURL(2))
	s, _ := newTestScanner(t, r, d, prsOf(1, 2))
	scan(t, s)
	if d.writes != 2 || mr1.Fields[fieldOverlaps] == "" || mr2.Fields[fieldOverlaps] == "" {
		t.Fatalf("first scan: writes %d, overlaps %q / %q", d.writes, mr1.Fields[fieldOverlaps], mr2.Fields[fieldOverlaps])
	}

	scan(t, s)
	if d.writes != 2 {
		t.Errorf("second scan with no change wrote %d times", d.writes-2)
	}

	s.prs = prsOf(1)
	scan(t, s)
	if v := mr1.Fields[fieldOverlaps]; v != "" {
		t.Errorf("PR 1 mr_overlaps after PR 2 closed = %q, want empty", v)
	}
	if v := mr2.Fields[fieldOverlaps]; v != "" {
		t.Errorf("closed PR 2 mr_overlaps = %q, want empty", v)
	}
	writes := d.writes
	scan(t, s)
	if d.writes != writes {
		t.Errorf("scan after the clear wrote %d times", d.writes-writes)
	}
}

func TestOverlapConflictingPRIsCleared(t *testing.T) {
	cur := map[string]string{fieldMergeState: mergeClean, fieldOverlaps: `[{"pr":2,"files":["a"]}]`}
	p := conflictPatch(cur, conflictResult{state: mergeConflicting, files: []string{"a"}, overlapsKnown: true}, testNow)
	if v, ok := p[fieldOverlaps]; !ok || v != "" {
		t.Errorf("mr_overlaps not cleared on a conflicting PR: %q (present %v)", v, ok)
	}
	if u := conflictPatch(cur, conflictResult{state: mergeClean}, testNow); u != nil {
		t.Errorf("a result without overlaps wrote %v", u)
	}
}

func TestOverlapsJSON(t *testing.T) {
	if got := overlapsJSON(nil); got != "" {
		t.Errorf("no overlaps = %q, want empty", got)
	}
	var many []string
	for i := 11; 0 < i; i-- {
		many = append(many, fmt.Sprintf("f%02d", i))
	}
	got := overlapsJSON([]prOverlap{{PR: 9, Files: []string{"b", "a"}}, {PR: 3, Files: many}})
	var ov []prOverlap
	if err := json.Unmarshal([]byte(got), &ov); err != nil {
		t.Fatal(err)
	}
	if len(ov) != 2 || ov[0].PR != 3 || ov[1].PR != 9 {
		t.Fatalf("entries not sorted by PR: %s", got)
	}
	if f := ov[0].Files; len(f) != maxOverlapFiles || f[0] != "f01" || f[len(f)-1] != "…+2 more" {
		t.Errorf("capped files = %v", f)
	}
	if !strings.Contains(got, `"files":["a","b"]`) {
		t.Errorf("files not sorted: %s", got)
	}
}
