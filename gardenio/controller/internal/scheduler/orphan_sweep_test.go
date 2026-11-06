package scheduler

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"testing"
	"time"

	"gardenio/clicreds/beadswire"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestSelectOrphans(t *testing.T) {
	now := time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC)
	old := now.Add(-1 * time.Hour) // older than the 15m grace period
	fresh := now.Add(-1 * time.Minute)
	cfg := OrphanSweepConfig{} // defaults: needs-intake, 15m grace

	cases := []struct {
		name string
		bead OrphanBead
		want bool
	}{
		{
			name: "open unassigned old task is orphaned",
			bead: OrphanBead{ID: "gd-1", Type: "task", Status: "open", CreatedAt: old},
			want: true,
		},
		{
			name: "open unassigned old feature is orphaned",
			bead: OrphanBead{ID: "gd-2", Type: "feature", Status: "open", CreatedAt: old},
			want: true,
		},
		{
			name: "ready status counts as open",
			bead: OrphanBead{ID: "gd-3", Type: "bug", Status: "ready", CreatedAt: old},
			want: true,
		},
		{
			name: "empty status (ready view omits it) is treated as open",
			bead: OrphanBead{ID: "gd-3b", Type: "task", Status: "", CreatedAt: old},
			want: true,
		},
		{
			name: "assigned bead is not orphaned",
			bead: OrphanBead{ID: "gd-4", Type: "task", Status: "open", Assignee: "someone", CreatedAt: old},
			want: false,
		},
		{
			name: "in_progress bead is not orphaned",
			bead: OrphanBead{ID: "gd-6", Type: "task", Status: "in_progress", CreatedAt: old},
			want: false,
		},
		{
			name: "closed bead is not orphaned",
			bead: OrphanBead{ID: "gd-7", Type: "task", Status: "closed", CreatedAt: old},
			want: false,
		},
		{
			name: "infrastructure agent bead is never surfaced",
			bead: OrphanBead{ID: "gd-8", Type: "agent", Status: "open", CreatedAt: old},
			want: false,
		},
		{
			name: "schedule bead is never surfaced",
			bead: OrphanBead{ID: "gd-9", Type: "schedule", Status: "open", CreatedAt: old},
			want: false,
		},
		{
			name: "too-young bead is in the grace period",
			bead: OrphanBead{ID: "gd-10", Type: "task", Status: "open", CreatedAt: fresh},
			want: false,
		},
		{
			name: "already-labelled bead is skipped (idempotent)",
			bead: OrphanBead{ID: "gd-11", Type: "task", Status: "open", CreatedAt: old, Labels: []string{"needs-intake"}},
			want: false,
		},
		{
			name: "already-labelled is case-insensitive",
			bead: OrphanBead{ID: "gd-12", Type: "task", Status: "open", CreatedAt: old, Labels: []string{"Needs-Intake"}},
			want: false,
		},
		{
			name: "zero CreatedAt bypasses grace check and is orphaned",
			bead: OrphanBead{ID: "gd-13", Type: "feature", Status: "OPEN", CreatedAt: time.Time{}},
			want: true,
		},
		{
			// ka-0000007370 fix (1): a bead with a parent-child owning epic/plan
			// has a live owning gardener and is NOT orphaned — the groomer↔sweeper
			// fight-loop case (ka-0000005640 was parented → open epic).
			name: "parented bead is not surfaced",
			bead: OrphanBead{ID: "gd-14", Type: "task", Status: "open", CreatedAt: old, HasParent: true},
			want: false,
		},
		{
			// ka-0000007370 fix (2): a bead the groomer de-laned within the
			// cooldown window is left alone so the de-lane wins the race.
			name: "recently de-laned bead is not surfaced (cooldown active)",
			bead: OrphanBead{ID: "gd-15", Type: "task", Status: "open", CreatedAt: old, DeLanedAt: now.Add(-5 * time.Minute)},
			want: false,
		},
		{
			// The cooldown is a *recency* guard: a de-lane older than the cooldown
			// (the default 4h — deliberately far longer than the ~30m groomer
			// cadence, ka-0000008193) no longer suppresses surfacing: the fallback
			// once the groomer has clearly stopped managing the bead.
			name: "stale de-lane past the cooldown is surfaced again",
			bead: OrphanBead{ID: "gd-16", Type: "task", Status: "open", CreatedAt: old, DeLanedAt: now.Add(-5 * time.Hour)},
			want: true,
		},
		{
			// The complement of the parent/cooldown cases: genuinely orphaned,
			// unparented, never-de-laned one-shot-gardener output STILL surfaces.
			name: "unparented never-de-laned orphan is still surfaced",
			bead: OrphanBead{ID: "gd-17", Type: "task", Status: "open", CreatedAt: old, HasParent: false},
			want: true,
		},
		{
			// ka-0000003548 fix: a parentless ROOT EPIC with at least one open
			// parent-child child is a tree-root by design — NOT an orphan. This is
			// the case the janitor↔groomer fight-loop kept re-laning every 5 min.
			name: "parentless epic with open children (tree-root) is not surfaced",
			bead: OrphanBead{ID: "gd-18", Type: "epic", Status: "open", CreatedAt: old, HasParent: false, HasOpenChildren: true},
			want: false,
		},
		{
			// gd-10swg8vnzy: epics are never swept. The trigger-engine principal
			// cannot label one (every attempt is a 403), and a parentless epic
			// with no open children is a finished plan, not intake work.
			name: "parentless childless epic is not surfaced",
			bead: OrphanBead{ID: "gd-19", Type: "epic", Status: "open", CreatedAt: old, HasParent: false, HasOpenChildren: false},
			want: false,
		},
		{
			// A parentless childless bug is a plain orphan — unchanged behaviour,
			// the tree-root skip does not over-reach to leaf work items.
			name: "parentless childless bug is still surfaced",
			bead: OrphanBead{ID: "gd-20", Type: "bug", Status: "open", CreatedAt: old, HasParent: false, HasOpenChildren: false},
			want: true,
		},
		{
			// The tree-root skip is type-agnostic: any parentless bead with a live
			// subtree is a root, even a non-epic (e.g. a plan-of-record task with
			// child steps). This guards against a future regression that narrows
			// the skip to type=epic.
			name: "parentless task with open children (tree-root) is not surfaced",
			bead: OrphanBead{ID: "gd-21", Type: "task", Status: "open", CreatedAt: old, HasParent: false, HasOpenChildren: true},
			want: false,
		},
		{
			// ka-0000002438: a release-policy bug is filed parentless by a CI
			// auto-filer (no epic to attach to) but is already OWNED by the
			// release lane. Surfacing it into needs-intake takes it OUT of the
			// lane that fixes it — gardener-bugfix's candidate filter excludes
			// needs-intake outright — so it must never be swept.
			name: "parentless release-pipeline bug is not surfaced (owned lane)",
			bead: OrphanBead{ID: "gd-22", Type: "bug", Status: "open", CreatedAt: old, Labels: []string{"project:gardenio", "release-pipeline", "release:2026.208.12"}},
			want: false,
		},
		{
			name: "release-blocker bug is not surfaced",
			bead: OrphanBead{ID: "gd-23", Type: "bug", Status: "open", CreatedAt: old, Labels: []string{"release-blocker"}},
			want: false,
		},
		{
			name: "release-regression bug is not surfaced",
			bead: OrphanBead{ID: "gd-24", Type: "bug", Status: "open", CreatedAt: old, Labels: []string{"release-regression"}},
			want: false,
		},
		{
			name: "release-pipeline-broken bug is not surfaced",
			bead: OrphanBead{ID: "gd-25", Type: "bug", Status: "open", CreatedAt: old, Labels: []string{"release-pipeline-broken"}},
			want: false,
		},
		{
			// Label matching is case-insensitive, consistent with the intake-label
			// and type checks.
			name: "release-policy label match is case-insensitive",
			bead: OrphanBead{ID: "gd-26", Type: "bug", Status: "open", CreatedAt: old, Labels: []string{"Release-Pipeline"}},
			want: false,
		},
		{
			// The exemption is keyed on the POLICY labels only. A bare
			// "release:<version>" provenance label carries no ownership meaning
			// (no gardener rule matches it), so such a bead is still a plain
			// orphan — this is exactly the pre-ka-0000003811 state of the
			// auto-filers and must keep surfacing.
			name: "bare release:<version> provenance label is still surfaced",
			bead: OrphanBead{ID: "gd-27", Type: "bug", Status: "open", CreatedAt: old, Labels: []string{"project:gardenio", "release:2026.208.12"}},
			want: true,
		},
		{
			// Don't over-reach: a label that merely starts with "release" but is
			// not a policy label must not exempt the bead.
			name: "unrelated release-ish label does not exempt",
			bead: OrphanBead{ID: "gd-28", Type: "bug", Status: "open", CreatedAt: old, Labels: []string{"release-notes"}},
			want: true,
		},
		{
			// ka-0000007106: a per-cycle schedule run-instance fire record is owned
			// by the stale-reaper, never the intake lane. Surfacing it stamps a P0
			// bead into a lane no consuming gardener can act on — the priority-
			// inflation leak. (Shape of gd-5ZplF33f9d / gd-RzzLVQLLNf.)
			name: "source:schedule run-instance is not surfaced",
			bead: OrphanBead{ID: "gd-29", Type: "task", Status: "open", CreatedAt: old, Labels: []string{"project:gardenio", "source:schedule", "trigger:ka-0000000336"}},
			want: false,
		},
		{
			// The Multi[cron]/Multi[bead_event] fire-record shape — 5 of the bug's
			// 8 missed beads (gd-CqPQYZJjPT etc.) carried source:multi.
			name: "source:multi run-instance is not surfaced",
			bead: OrphanBead{ID: "gd-30", Type: "task", Status: "open", CreatedAt: old, Labels: []string{"project:gardenio", "source:multi", "trigger:ka-0000006579", "multi-entry:2"}},
			want: false,
		},
		{
			name: "source:schedule-mirror run-instance is not surfaced",
			bead: OrphanBead{ID: "gd-31", Type: "task", Status: "open", CreatedAt: old, Labels: []string{"source:schedule-mirror", "trigger:gd-abc"}},
			want: false,
		},
		{
			// Case-insensitive, consistent with every other label check.
			name: "run-instance source label match is case-insensitive",
			bead: OrphanBead{ID: "gd-32", Type: "task", Status: "open", CreatedAt: old, Labels: []string{"Source:Multi"}},
			want: false,
		},
		{
			// Don't over-reach: a non-run-instance source label (e.g. a
			// gitlab/jira/webhook-sourced work bead) is still a legitimate orphan.
			// Only the schedule/multi/schedule-mirror fire records are reaper-owned.
			name: "unrelated source label is still surfaced",
			bead: OrphanBead{ID: "gd-33", Type: "task", Status: "open", CreatedAt: old, Labels: []string{"source:gitlab"}},
			want: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := SelectOrphans([]OrphanBead{tc.bead}, cfg, now)
			gotSelected := len(got) == 1
			if gotSelected != tc.want {
				t.Fatalf("SelectOrphans(%+v) selected=%v, want %v", tc.bead, gotSelected, tc.want)
			}
		})
	}
}

func TestSelectOrphansCustomLabel(t *testing.T) {
	now := time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC)
	old := now.Add(-1 * time.Hour)
	cfg := OrphanSweepConfig{IntakeLabel: "triage", MinAge: time.Minute}

	beads := []OrphanBead{
		{ID: "gd-a", Type: "task", Status: "open", CreatedAt: old},                                   // selected
		{ID: "gd-b", Type: "task", Status: "open", CreatedAt: old, Labels: []string{"triage"}},       // already labelled
		{ID: "gd-c", Type: "task", Status: "open", CreatedAt: old, Labels: []string{"needs-intake"}}, // default label irrelevant now
	}
	got := SelectOrphans(beads, cfg, now)
	if len(got) != 2 {
		t.Fatalf("expected 2 orphans (gd-a, gd-c), got %d: %+v", len(got), got)
	}
}

// fakeStore is an in-memory OrphanStore for sweeper tests.
type fakeStore struct {
	beads     []OrphanBead
	added     map[string][]string // beadID -> labels added
	listErr   error
	addErrFor map[string]error
	addCalls  []string // beadID of every AddLabel call, including failed ones
	// parented is the set of bead IDs ParentedBeads should report as having a
	// parent-child owning epic/plan.
	parented map[string]bool
	// openChildren is the set of bead IDs HasOpenChildren should report as
	// tree-roots with at least one open parent-child child.
	openChildren map[string]bool
	// delaned maps bead IDs to the timestamp LastDeLane should report.
	delaned map[string]time.Time
	// comments records the re-lane audit comments AddComment left, keyed by bead
	// ID. commentErrFor forces AddComment to fail for a given bead.
	comments      map[string][]string
	commentErrFor map[string]error
	// parentErr / childErr / delaneErr force the corresponding enrichment lookup
	// to fail.
	parentErr error
	childErr  error
	delaneErr error
	// parentCalls / childCalls / delaneCalls record the IDs each enrichment
	// lookup was asked about, so tests can assert enrichment is bounded to the
	// candidate set (not the whole ready view).
	parentCalls [][]string
	childCalls  [][]string
	delaneCalls [][]string
}

func newFakeStore(beads ...OrphanBead) *fakeStore {
	return &fakeStore{beads: beads, added: map[string][]string{}, addErrFor: map[string]error{}}
}

func (f *fakeStore) ListReady(_ context.Context) ([]OrphanBead, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.beads, nil
}

func (f *fakeStore) ParentedBeads(_ context.Context, beadIDs []string) (map[string]bool, error) {
	f.parentCalls = append(f.parentCalls, append([]string(nil), beadIDs...))
	if f.parentErr != nil {
		return nil, f.parentErr
	}
	out := map[string]bool{}
	for _, id := range beadIDs {
		if f.parented[id] {
			out[id] = true
		}
	}
	return out, nil
}

func (f *fakeStore) HasOpenChildren(_ context.Context, beadIDs []string) (map[string]bool, error) {
	f.childCalls = append(f.childCalls, append([]string(nil), beadIDs...))
	if f.childErr != nil {
		return nil, f.childErr
	}
	out := map[string]bool{}
	for _, id := range beadIDs {
		if f.openChildren[id] {
			out[id] = true
		}
	}
	return out, nil
}

func (f *fakeStore) LastDeLane(_ context.Context, beadIDs []string) (map[string]time.Time, error) {
	f.delaneCalls = append(f.delaneCalls, append([]string(nil), beadIDs...))
	if f.delaneErr != nil {
		return nil, f.delaneErr
	}
	out := map[string]time.Time{}
	for _, id := range beadIDs {
		if t, ok := f.delaned[id]; ok {
			out[id] = t
		}
	}
	return out, nil
}

func (f *fakeStore) AddComment(_ context.Context, beadID, text string) error {
	if err := f.commentErrFor[beadID]; err != nil {
		return err
	}
	if f.comments == nil {
		f.comments = map[string][]string{}
	}
	f.comments[beadID] = append(f.comments[beadID], text)
	return nil
}

func (f *fakeStore) AddLabel(_ context.Context, beadID, label string) error {
	f.addCalls = append(f.addCalls, beadID)
	if err := f.addErrFor[beadID]; err != nil {
		return err
	}
	f.added[beadID] = append(f.added[beadID], label)
	// Reflect the label back onto the in-memory bead so a subsequent sweep
	// sees it as already surfaced (mirrors daemon idempotency).
	for i := range f.beads {
		if f.beads[i].ID == beadID {
			f.beads[i].Labels = append(f.beads[i].Labels, label)
		}
	}
	return nil
}

func (f *fakeStore) addedIDs() []string {
	ids := make([]string, 0, len(f.added))
	for id := range f.added {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func TestOrphanSweeperSweepLabelsOrphans(t *testing.T) {
	now := time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC)
	old := now.Add(-1 * time.Hour)
	store := newFakeStore(
		OrphanBead{ID: "gd-orphan-1", Type: "task", Status: "open", CreatedAt: old},
		OrphanBead{ID: "gd-orphan-2", Type: "bug", Status: "open", CreatedAt: old},
		OrphanBead{ID: "gd-owned", Type: "task", Status: "open", Assignee: "x", CreatedAt: old},
		OrphanBead{ID: "gd-agent", Type: "agent", Status: "open", CreatedAt: old},
	)
	s := NewOrphanSweeper(store, quietLogger(), OrphanSweepConfig{}, time.Minute)

	surfaced := s.Sweep(context.Background(), now)
	if surfaced != 2 {
		t.Fatalf("expected 2 surfaced, got %d", surfaced)
	}
	want := []string{"gd-orphan-1", "gd-orphan-2"}
	got := store.addedIDs()
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("labelled %v, want %v", got, want)
	}
	for _, id := range want {
		if labels := store.added[id]; len(labels) != 1 || labels[0] != DefaultIntakeLabel {
			t.Fatalf("bead %s got labels %v, want [%s]", id, labels, DefaultIntakeLabel)
		}
	}
}

func TestOrphanSweeperSweepIdempotent(t *testing.T) {
	now := time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC)
	old := now.Add(-1 * time.Hour)
	store := newFakeStore(
		OrphanBead{ID: "gd-orphan-1", Type: "task", Status: "open", CreatedAt: old},
	)
	s := NewOrphanSweeper(store, quietLogger(), OrphanSweepConfig{}, time.Minute)

	if got := s.Sweep(context.Background(), now); got != 1 {
		t.Fatalf("first sweep surfaced %d, want 1", got)
	}
	// Second sweep at a later time: the bead now carries the label, so it must
	// not be surfaced again.
	if got := s.Sweep(context.Background(), now.Add(2*time.Minute)); got != 0 {
		t.Fatalf("second sweep surfaced %d, want 0 (idempotent)", got)
	}
	if labels := store.added["gd-orphan-1"]; len(labels) != 1 {
		t.Fatalf("bead labelled %d times, want exactly 1", len(labels))
	}
}

func TestOrphanSweeperSweepListError(t *testing.T) {
	store := newFakeStore()
	store.listErr = errors.New("boom")
	s := NewOrphanSweeper(store, quietLogger(), OrphanSweepConfig{}, time.Minute)
	if got := s.Sweep(context.Background(), time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC)); got != 0 {
		t.Fatalf("sweep on list error surfaced %d, want 0", got)
	}
}

func TestOrphanSweeperSweepLabelErrorIsSkipped(t *testing.T) {
	now := time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC)
	old := now.Add(-1 * time.Hour)
	store := newFakeStore(
		OrphanBead{ID: "gd-bad", Type: "task", Status: "open", CreatedAt: old},
		OrphanBead{ID: "gd-good", Type: "task", Status: "open", CreatedAt: old},
	)
	store.addErrFor["gd-bad"] = errors.New("nope")
	s := NewOrphanSweeper(store, quietLogger(), OrphanSweepConfig{}, time.Minute)

	// One bead fails to label; the other still succeeds.
	if got := s.Sweep(context.Background(), now); got != 1 {
		t.Fatalf("surfaced %d, want 1 (bad bead skipped)", got)
	}
	if _, ok := store.added["gd-good"]; !ok {
		t.Fatalf("gd-good should have been labelled despite gd-bad failing")
	}
}

func TestOrphanSweeperTickThrottle(t *testing.T) {
	now := time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC)
	old := now.Add(-1 * time.Hour)
	store := newFakeStore(
		OrphanBead{ID: "gd-orphan-1", Type: "task", Status: "open", CreatedAt: old},
	)
	s := NewOrphanSweeper(store, quietLogger(), OrphanSweepConfig{}, 5*time.Minute)

	// First tick runs the sweep.
	s.Tick(context.Background(), now)
	if len(store.added) != 1 {
		t.Fatalf("first tick should have swept; added=%v", store.added)
	}
	// A fresh orphan appears, but a tick within the interval is throttled.
	store.beads = append(store.beads, OrphanBead{ID: "gd-orphan-2", Type: "task", Status: "open", CreatedAt: old})
	s.Tick(context.Background(), now.Add(1*time.Minute))
	if _, ok := store.added["gd-orphan-2"]; ok {
		t.Fatalf("tick within interval should be throttled, but gd-orphan-2 was labelled")
	}
	// After the interval elapses, the next tick sweeps again.
	s.Tick(context.Background(), now.Add(6*time.Minute))
	if _, ok := store.added["gd-orphan-2"]; !ok {
		t.Fatalf("tick after interval should sweep and label gd-orphan-2")
	}
}

// TestOrphanSweeperSweepSkipsParented is the end-to-end guard for
// ka-0000007370 fix (1): a parented bead survives pass-1 base selection but is
// dropped in pass-2 once ParentedBeads reports it as owned, so it is never
// re-laned; its unparented sibling still surfaces.
func TestOrphanSweeperSweepSkipsParented(t *testing.T) {
	now := time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC)
	old := now.Add(-1 * time.Hour)
	store := newFakeStore(
		OrphanBead{ID: "gd-parented", Type: "task", Status: "open", CreatedAt: old},
		OrphanBead{ID: "gd-orphan", Type: "task", Status: "open", CreatedAt: old},
	)
	store.parented = map[string]bool{"gd-parented": true}
	s := NewOrphanSweeper(store, quietLogger(), OrphanSweepConfig{}, time.Minute)

	if got := s.Sweep(context.Background(), now); got != 1 {
		t.Fatalf("surfaced %d, want 1 (parented bead skipped)", got)
	}
	if _, ok := store.added["gd-parented"]; ok {
		t.Fatalf("parented bead must not be surfaced")
	}
	if _, ok := store.added["gd-orphan"]; !ok {
		t.Fatalf("unparented orphan must still be surfaced")
	}
}

// TestOrphanSweeperSweepSkipsTreeRoots is the end-to-end guard for
// ka-0000003548: a parentless ROOT task with open children survives pass-1 base
// selection but is dropped in pass-2 once HasOpenChildren reports it as a
// tree-root, so it is never re-laned; a parentless CHILDLESS task and a plain
// leaf orphan still surface. This is the code analog of the janitor prompt fix
// (ka-0000003783) and closes the last re-opener of the janitor↔groomer fight-loop.
func TestOrphanSweeperSweepSkipsTreeRoots(t *testing.T) {
	now := time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC)
	old := now.Add(-1 * time.Hour)
	store := newFakeStore(
		OrphanBead{ID: "gd-root-task", Type: "task", Status: "open", CreatedAt: old},
		OrphanBead{ID: "gd-childless-task", Type: "task", Status: "open", CreatedAt: old},
		OrphanBead{ID: "gd-leaf-orphan", Type: "bug", Status: "open", CreatedAt: old},
	)
	store.openChildren = map[string]bool{"gd-root-task": true}
	s := NewOrphanSweeper(store, quietLogger(), OrphanSweepConfig{}, time.Minute)

	if got := s.Sweep(context.Background(), now); got != 2 {
		t.Fatalf("surfaced %d, want 2 (tree-root task skipped)", got)
	}
	if _, ok := store.added["gd-root-task"]; ok {
		t.Fatalf("parentless task with open children (tree-root) must not be surfaced")
	}
	if _, ok := store.added["gd-childless-task"]; !ok {
		t.Fatalf("parentless childless task must still be surfaced (it is a legitimate orphan)")
	}
	if _, ok := store.added["gd-leaf-orphan"]; !ok {
		t.Fatalf("parentless childless leaf orphan must still be surfaced")
	}
}

// TestOrphanSweeperSweepOpenChildrenLookupErrorSkipsSweep: an open-children
// lookup failure must skip the sweep entirely rather than risk re-laning a
// tree-root (the same fail-safe as the parent/de-lane lookups — a lookup error
// must never re-open the fight-loop).
func TestOrphanSweeperSweepOpenChildrenLookupErrorSkipsSweep(t *testing.T) {
	now := time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC)
	old := now.Add(-1 * time.Hour)
	store := newFakeStore(
		OrphanBead{ID: "gd-orphan", Type: "task", Status: "open", CreatedAt: old},
	)
	store.childErr = errors.New("daemon down")
	s := NewOrphanSweeper(store, quietLogger(), OrphanSweepConfig{}, time.Minute)

	if got := s.Sweep(context.Background(), now); got != 0 {
		t.Fatalf("surfaced %d, want 0 on open-children-lookup error", got)
	}
	if len(store.added) != 0 {
		t.Fatalf("nothing should be labelled when open-children lookup fails; added=%v", store.added)
	}
}

// TestOrphanSweeperSweepHonorsDeLaneCooldown is the end-to-end guard for
// ka-0000007370 fix (2): a bead the groomer de-laned within the cooldown is
// left alone; one whose de-lane predates the cooldown surfaces again.
func TestOrphanSweeperSweepHonorsDeLaneCooldown(t *testing.T) {
	now := time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC)
	old := now.Add(-1 * time.Hour)
	store := newFakeStore(
		OrphanBead{ID: "gd-fresh-delane", Type: "task", Status: "open", CreatedAt: old},
		OrphanBead{ID: "gd-stale-delane", Type: "task", Status: "open", CreatedAt: old},
	)
	store.delaned = map[string]time.Time{
		"gd-fresh-delane": now.Add(-10 * time.Minute), // within the 4h default cooldown
		"gd-stale-delane": now.Add(-5 * time.Hour),    // past the 4h cooldown
	}
	s := NewOrphanSweeper(store, quietLogger(), OrphanSweepConfig{}, time.Minute)

	if got := s.Sweep(context.Background(), now); got != 1 {
		t.Fatalf("surfaced %d, want 1 (fresh de-lane suppressed, stale surfaced)", got)
	}
	if _, ok := store.added["gd-fresh-delane"]; ok {
		t.Fatalf("bead de-laned within cooldown must not be re-surfaced")
	}
	if _, ok := store.added["gd-stale-delane"]; !ok {
		t.Fatalf("bead de-laned before the cooldown must surface again")
	}
}

// TestOrphanSweeperSweepEnrichmentBoundedToCandidates asserts the expensive
// enrichment lookups run only over the pass-1 candidate set, never the whole
// ready view — the (unbatchable) LastDeLane comment fetch must not fan out
// across assigned/labelled/young beads that base selection already rejected.
func TestOrphanSweeperSweepEnrichmentBoundedToCandidates(t *testing.T) {
	now := time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC)
	old := now.Add(-1 * time.Hour)
	fresh := now.Add(-1 * time.Minute)
	store := newFakeStore(
		OrphanBead{ID: "gd-candidate", Type: "task", Status: "open", CreatedAt: old},
		OrphanBead{ID: "gd-assigned", Type: "task", Status: "open", Assignee: "x", CreatedAt: old},
		OrphanBead{ID: "gd-labelled", Type: "task", Status: "open", CreatedAt: old, Labels: []string{DefaultIntakeLabel}},
		OrphanBead{ID: "gd-young", Type: "task", Status: "open", CreatedAt: fresh},
	)
	s := NewOrphanSweeper(store, quietLogger(), OrphanSweepConfig{}, time.Minute)

	if got := s.Sweep(context.Background(), now); got != 1 {
		t.Fatalf("surfaced %d, want 1", got)
	}
	wantIDs := []string{"gd-candidate"}
	if len(store.parentCalls) != 1 || !equalStrSlices(store.parentCalls[0], wantIDs) {
		t.Fatalf("ParentedBeads called with %v, want a single call for %v", store.parentCalls, wantIDs)
	}
	if len(store.childCalls) != 1 || !equalStrSlices(store.childCalls[0], wantIDs) {
		t.Fatalf("HasOpenChildren called with %v, want a single call for %v", store.childCalls, wantIDs)
	}
	if len(store.delaneCalls) != 1 || !equalStrSlices(store.delaneCalls[0], wantIDs) {
		t.Fatalf("LastDeLane called with %v, want a single call for %v", store.delaneCalls, wantIDs)
	}
}

// TestOrphanSweeperSweepSkipsEnrichmentWhenNoCandidates confirms the sweep
// short-circuits before any enrichment round-trip when pass-1 yields nothing.
func TestOrphanSweeperSweepSkipsEnrichmentWhenNoCandidates(t *testing.T) {
	now := time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC)
	old := now.Add(-1 * time.Hour)
	store := newFakeStore(
		OrphanBead{ID: "gd-assigned", Type: "task", Status: "open", Assignee: "x", CreatedAt: old},
	)
	s := NewOrphanSweeper(store, quietLogger(), OrphanSweepConfig{}, time.Minute)

	if got := s.Sweep(context.Background(), now); got != 0 {
		t.Fatalf("surfaced %d, want 0", got)
	}
	if len(store.parentCalls) != 0 || len(store.delaneCalls) != 0 {
		t.Fatalf("no enrichment expected with zero candidates; parent=%v delane=%v", store.parentCalls, store.delaneCalls)
	}
}

// TestOrphanSweeperSweepParentLookupErrorSkipsSweep: a parent-lookup failure
// must skip the sweep entirely rather than risk re-laning owned work (the
// fail-safe behaviour that protects against reopening the fight-loop).
func TestOrphanSweeperSweepParentLookupErrorSkipsSweep(t *testing.T) {
	now := time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC)
	old := now.Add(-1 * time.Hour)
	store := newFakeStore(
		OrphanBead{ID: "gd-orphan", Type: "task", Status: "open", CreatedAt: old},
	)
	store.parentErr = errors.New("daemon down")
	s := NewOrphanSweeper(store, quietLogger(), OrphanSweepConfig{}, time.Minute)

	if got := s.Sweep(context.Background(), now); got != 0 {
		t.Fatalf("surfaced %d, want 0 on parent-lookup error", got)
	}
	if len(store.added) != 0 {
		t.Fatalf("nothing should be labelled when parent lookup fails; added=%v", store.added)
	}
}

// TestOrphanSweeperSweepDeLaneLookupErrorSkipsSweep: likewise a de-lane lookup
// failure skips the sweep rather than re-lane groomed work.
func TestOrphanSweeperSweepDeLaneLookupErrorSkipsSweep(t *testing.T) {
	now := time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC)
	old := now.Add(-1 * time.Hour)
	store := newFakeStore(
		OrphanBead{ID: "gd-orphan", Type: "task", Status: "open", CreatedAt: old},
	)
	store.delaneErr = errors.New("comments unavailable")
	s := NewOrphanSweeper(store, quietLogger(), OrphanSweepConfig{}, time.Minute)

	if got := s.Sweep(context.Background(), now); got != 0 {
		t.Fatalf("surfaced %d, want 0 on de-lane-lookup error", got)
	}
	if len(store.added) != 0 {
		t.Fatalf("nothing should be labelled when de-lane lookup fails; added=%v", store.added)
	}
}

// TestOrphanSweeperSweepCooldownDisabledSkipsDeLaneLookup: a negative
// DeLaneCooldown disables the cooldown guard, so LastDeLane is never called and
// a recently de-laned bead is surfaced (parent guard remains active).
func TestOrphanSweeperSweepCooldownDisabledSkipsDeLaneLookup(t *testing.T) {
	now := time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC)
	old := now.Add(-1 * time.Hour)
	store := newFakeStore(
		OrphanBead{ID: "gd-fresh-delane", Type: "task", Status: "open", CreatedAt: old},
	)
	store.delaned = map[string]time.Time{"gd-fresh-delane": now.Add(-1 * time.Minute)}
	s := NewOrphanSweeper(store, quietLogger(), OrphanSweepConfig{DeLaneCooldown: -1}, time.Minute)

	if got := s.Sweep(context.Background(), now); got != 1 {
		t.Fatalf("surfaced %d, want 1 (cooldown disabled)", got)
	}
	if len(store.delaneCalls) != 0 {
		t.Fatalf("LastDeLane must not be called when cooldown is disabled; calls=%v", store.delaneCalls)
	}
}

// TestOrphanSweepConfigDefaults pins the cooldown default sentinel behaviour:
// zero ⇒ default, negative ⇒ preserved (disabled), positive ⇒ preserved.
func TestOrphanSweepConfigDefaults(t *testing.T) {
	if got := (OrphanSweepConfig{}).withDefaults().DeLaneCooldown; got != defaultOrphanDeLaneCooldown {
		t.Errorf("zero cooldown → %s, want default %s", got, defaultOrphanDeLaneCooldown)
	}
	if got := (OrphanSweepConfig{DeLaneCooldown: -1}).withDefaults().DeLaneCooldown; got != -1 {
		t.Errorf("negative cooldown → %s, want preserved (-1ns, disabled)", got)
	}
	if got := (OrphanSweepConfig{DeLaneCooldown: time.Hour}).withDefaults().DeLaneCooldown; got != time.Hour {
		t.Errorf("explicit cooldown → %s, want 1h preserved", got)
	}
}

// TestDeLaneCooldownExceedsGroomerCadence is the regression guard for
// ka-0000008193. The de-lane cooldown is the ONLY brake left on the
// groomer↔sweeper loop for a parentless bead whose children are all closed (the
// HasParent and HasOpenChildren guards do not cover a drained epic). If the
// cooldown equals — or merely approximates — the intake-groomer cron period,
// the cooldown lapses exactly as the next groomer cycle begins and the two
// processes phase-lock into a permanent re-lane ping-pong. The cooldown must
// therefore stay a comfortable MULTIPLE of any plausible groomer cadence so the
// periods can never resonate, even if the cron is retuned faster.
func TestDeLaneCooldownExceedsGroomerCadence(t *testing.T) {
	// The intake-groomer trigger (ka-0000006210) fires on `3,33 * * * *` — a
	// 30-minute cadence. This is the period the cooldown must not resonate with.
	const groomerCadence = 30 * time.Minute
	// Require a healthy margin (≥4×), not just strict inequality: a cooldown a
	// hair longer than one period still lets the two phase-drift into alignment
	// over time, and leaves no headroom if the cron is retuned to a faster tick.
	const minMargin = 4
	if defaultOrphanDeLaneCooldown < groomerCadence*minMargin {
		t.Fatalf("defaultOrphanDeLaneCooldown=%s must be at least %d× the groomer cadence %s (=%s) to avoid the re-lane resonance of ka-0000008193; got a margin of %.1f×",
			defaultOrphanDeLaneCooldown, minMargin, groomerCadence, groomerCadence*minMargin,
			float64(defaultOrphanDeLaneCooldown)/float64(groomerCadence))
	}
}

// TestOrphanSweeperReLaneEmitsAuditComment: a bead with a prior de-lane whose
// cooldown has lapsed is re-laned AND gets a structured re-lane comment
// (ka-0000007945), so the re-lane is auditable and comment-based loop detection
// works. A first-time (never-de-laned) orphan surfaced in the same sweep stays
// comment-free.
func TestOrphanSweeperReLaneEmitsAuditComment(t *testing.T) {
	now := time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC)
	old := now.Add(-1 * time.Hour)
	store := newFakeStore(
		OrphanBead{ID: "gd-relane", Type: "task", Status: "open", CreatedAt: old},
		OrphanBead{ID: "gd-firsttime", Type: "bug", Status: "open", CreatedAt: old},
	)
	// gd-relane was de-laned well past the 4h cooldown, so it re-surfaces; it is a
	// RE-lane and must carry the audit comment. gd-firsttime was never de-laned.
	store.delaned = map[string]time.Time{"gd-relane": now.Add(-5 * time.Hour)}
	s := NewOrphanSweeper(store, quietLogger(), OrphanSweepConfig{}, time.Minute)

	if got := s.Sweep(context.Background(), now); got != 2 {
		t.Fatalf("surfaced %d, want 2", got)
	}
	// Both beads get the intake label.
	if !equalStrSlices(store.added["gd-relane"], []string{DefaultIntakeLabel}) {
		t.Fatalf("gd-relane labels = %v, want [%s]", store.added["gd-relane"], DefaultIntakeLabel)
	}
	if !equalStrSlices(store.added["gd-firsttime"], []string{DefaultIntakeLabel}) {
		t.Fatalf("gd-firsttime labels = %v, want [%s]", store.added["gd-firsttime"], DefaultIntakeLabel)
	}
	// Only the RE-laned bead gets a comment.
	wantComment := ReLaneMarker + " " + DefaultIntakeLabel
	if got := store.comments["gd-relane"]; !equalStrSlices(got, []string{wantComment}) {
		t.Fatalf("gd-relane comments = %v, want [%q]", got, wantComment)
	}
	if got := store.comments["gd-firsttime"]; len(got) != 0 {
		t.Fatalf("first-time surfacing must not be commented; got %v", got)
	}
}

// TestOrphanSweeperReLaneCommentBestEffort: a comment failure must not undo the
// surfacing — the label already stands and the bead still counts as surfaced.
func TestOrphanSweeperReLaneCommentBestEffort(t *testing.T) {
	now := time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC)
	old := now.Add(-1 * time.Hour)
	store := newFakeStore(
		OrphanBead{ID: "gd-relane", Type: "task", Status: "open", CreatedAt: old},
	)
	store.delaned = map[string]time.Time{"gd-relane": now.Add(-5 * time.Hour)}
	store.commentErrFor = map[string]error{"gd-relane": errors.New("comment endpoint down")}
	s := NewOrphanSweeper(store, quietLogger(), OrphanSweepConfig{}, time.Minute)

	if got := s.Sweep(context.Background(), now); got != 1 {
		t.Fatalf("surfaced %d, want 1 despite comment failure", got)
	}
	if !equalStrSlices(store.added["gd-relane"], []string{DefaultIntakeLabel}) {
		t.Fatalf("gd-relane labels = %v, want [%s]", store.added["gd-relane"], DefaultIntakeLabel)
	}
}

func equalStrSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestOrphanSweeperSweepNeverLabelsEpic: a parentless, childless, aged,
// unassigned epic is not labelled, so the trigger-engine principal (no epic
// ownership) sends no AddLabel that the daemon would refuse (gd-10swg8vnzy).
func TestOrphanSweeperSweepNeverLabelsEpic(t *testing.T) {
	now := time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC)
	old := now.Add(-1 * time.Hour)
	store := newFakeStore(
		OrphanBead{ID: "gd-finished-plan", Type: "epic", Status: "open", CreatedAt: old},
		OrphanBead{ID: "gd-leaf-orphan", Type: "task", Status: "open", CreatedAt: old},
	)
	s := NewOrphanSweeper(store, quietLogger(), OrphanSweepConfig{}, time.Minute)

	if got := s.Sweep(context.Background(), now); got != 1 {
		t.Fatalf("surfaced %d, want 1 (epic skipped)", got)
	}
	if !equalStrSlices(store.addCalls, []string{"gd-leaf-orphan"}) {
		t.Fatalf("AddLabel calls %v, want only [gd-leaf-orphan]", store.addCalls)
	}
}

// TestOrphanSweeperSweepSkipsForbiddenBead: a bead whose AddLabel the daemon
// refused with a 403 is not retried on later sweeps, so an ownership gap costs
// one denial per process, not one per sweep (gd-10swg8vnzy). Other errors are
// still retried.
func TestOrphanSweeperSweepSkipsForbiddenBead(t *testing.T) {
	now := time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC)
	old := now.Add(-1 * time.Hour)
	store := newFakeStore(
		OrphanBead{ID: "gd-forbidden", Type: "task", Status: "open", CreatedAt: old},
		OrphanBead{ID: "gd-flaky", Type: "bug", Status: "open", CreatedAt: old},
	)
	store.addErrFor["gd-forbidden"] = fmt.Errorf("adding label: %w",
		&beadswire.APIError{StatusCode: http.StatusForbidden, Message: "escalation_required"})
	store.addErrFor["gd-flaky"] = errors.New("connection reset")
	s := NewOrphanSweeper(store, quietLogger(), OrphanSweepConfig{}, time.Minute)

	s.Sweep(context.Background(), now)
	s.Sweep(context.Background(), now.Add(10*time.Minute))
	want := []string{"gd-forbidden", "gd-flaky", "gd-flaky"}
	if !equalStrSlices(store.addCalls, want) {
		t.Fatalf("AddLabel calls %v, want %v", store.addCalls, want)
	}
}
