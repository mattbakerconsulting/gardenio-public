package scheduler

import (
	"context"
	"errors"
	"sort"
	"testing"
	"time"
)

func TestSelectPromotable(t *testing.T) {
	now := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	old := now.Add(-1 * time.Hour)     // older than the 30m grace
	fresh := now.Add(-1 * time.Minute) // inside the grace window
	cfg := AutoIntakeSweepConfig{}     // defaults: needs-intake, 30m grace

	cases := []struct {
		name string
		bead IntakeBead
		want bool
	}{
		{
			name: "parented unassigned code-ready task is promotable",
			bead: IntakeBead{ID: "gd-1", Type: "task", Status: "open", Labels: []string{"needs-intake"}, HasParent: true, LabeledAt: old},
			want: true,
		},
		{
			name: "bug is promotable",
			bead: IntakeBead{ID: "gd-2", Type: "bug", Status: "open", Labels: []string{"needs-intake"}, HasParent: true, LabeledAt: old},
			want: true,
		},
		{
			name: "feature is promotable",
			bead: IntakeBead{ID: "gd-3", Type: "feature", Status: "open", Labels: []string{"needs-intake"}, HasParent: true, LabeledAt: old},
			want: true,
		},
		{
			name: "ready status counts as open",
			bead: IntakeBead{ID: "gd-4", Type: "task", Status: "ready", Labels: []string{"needs-intake"}, HasParent: true, LabeledAt: old},
			want: true,
		},
		{
			name: "empty status (ready view omits it) is treated as open",
			bead: IntakeBead{ID: "gd-4b", Type: "task", Status: "", Labels: []string{"needs-intake"}, HasParent: true, LabeledAt: old},
			want: true,
		},
		{
			name: "epic is NOT promotable (container, not code work)",
			bead: IntakeBead{ID: "gd-5", Type: "epic", Status: "open", Labels: []string{"needs-intake"}, HasParent: true, LabeledAt: old},
			want: false,
		},
		{
			name: "parentless bead is NOT promotable (orphan-sweep territory)",
			bead: IntakeBead{ID: "gd-6", Type: "task", Status: "open", Labels: []string{"needs-intake"}, HasParent: false, LabeledAt: old},
			want: false,
		},
		{
			name: "assigned bead is NOT promotable",
			bead: IntakeBead{ID: "gd-7", Type: "task", Status: "open", Assignee: "someone", Labels: []string{"needs-intake"}, HasParent: true, LabeledAt: old},
			want: false,
		},
		{
			name: "in_progress bead is NOT promotable",
			bead: IntakeBead{ID: "gd-8", Type: "task", Status: "in_progress", Labels: []string{"needs-intake"}, HasParent: true, LabeledAt: old},
			want: false,
		},
		{
			name: "closed bead is NOT promotable",
			bead: IntakeBead{ID: "gd-9", Type: "task", Status: "closed", Labels: []string{"needs-intake"}, HasParent: true, LabeledAt: old},
			want: false,
		},
		{
			name: "bead without the intake label is NOT promotable",
			bead: IntakeBead{ID: "gd-10", Type: "task", Status: "open", Labels: []string{"other"}, HasParent: true, LabeledAt: old},
			want: false,
		},
		{
			name: "source:slack bead stays human-owned",
			bead: IntakeBead{ID: "gd-11", Type: "task", Status: "open", Labels: []string{"needs-intake", "source:slack"}, HasParent: true, LabeledAt: old},
			want: false,
		},
		{
			name: "slack-thread-reply bead stays human-owned",
			bead: IntakeBead{ID: "gd-12", Type: "task", Status: "open", Labels: []string{"needs-intake", "slack-thread-reply"}, HasParent: true, LabeledAt: old},
			want: false,
		},
		{
			name: "slack-mention bead stays human-owned",
			bead: IntakeBead{ID: "gd-13", Type: "bug", Status: "open", Labels: []string{"needs-intake", "slack-mention"}, HasParent: true, LabeledAt: old},
			want: false,
		},
		{
			name: "freshly-labelled bead is inside the grace window",
			bead: IntakeBead{ID: "gd-14", Type: "task", Status: "open", Labels: []string{"needs-intake"}, HasParent: true, LabeledAt: fresh},
			want: false,
		},
		{
			name: "zero LabeledAt bypasses grace and is promotable",
			bead: IntakeBead{ID: "gd-15", Type: "task", Status: "OPEN", Labels: []string{"needs-intake"}, HasParent: true, LabeledAt: time.Time{}},
			want: true,
		},
		{
			name: "label match is case-insensitive",
			bead: IntakeBead{ID: "gd-16", Type: "task", Status: "open", Labels: []string{"Needs-Intake"}, HasParent: true, LabeledAt: old},
			want: true,
		},
		{
			name: "source:slack match is case-insensitive substring",
			bead: IntakeBead{ID: "gd-17", Type: "task", Status: "open", Labels: []string{"needs-intake", "Source:Slack-Thread"}, HasParent: true, LabeledAt: old},
			want: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := SelectPromotable([]IntakeBead{tc.bead}, cfg, now)
			gotSelected := len(got) == 1
			if gotSelected != tc.want {
				t.Fatalf("SelectPromotable(%+v) selected=%v, want %v", tc.bead, gotSelected, tc.want)
			}
		})
	}
}

func TestSelectPromotableCustomLabel(t *testing.T) {
	now := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	old := now.Add(-1 * time.Hour)
	cfg := AutoIntakeSweepConfig{IntakeLabel: "triage", MinAge: time.Minute}

	beads := []IntakeBead{
		{ID: "gd-a", Type: "task", Status: "open", Labels: []string{"triage"}, HasParent: true, LabeledAt: old},       // selected
		{ID: "gd-b", Type: "task", Status: "open", Labels: []string{"needs-intake"}, HasParent: true, LabeledAt: old}, // default label irrelevant now
		{ID: "gd-c", Type: "task", Status: "open", Labels: []string{"triage"}, HasParent: false, LabeledAt: old},      // parentless
	}
	got := SelectPromotable(beads, cfg, now)
	if len(got) != 1 || got[0].ID != "gd-a" {
		t.Fatalf("expected [gd-a], got %+v", got)
	}
}

// TestAutoIntakeDisjointFromOrphanSweep pins the disjointness invariant that
// makes the two halves fight-loop-proof: the orphan sweep only LABELS parentless
// work, the auto-intake sweep only DE-LANES parented work, so no bead is
// eligible for both at once. A parented needs-intake task is promotable here but
// never orphaned there; a parentless task is the reverse.
func TestAutoIntakeDisjointFromOrphanSweep(t *testing.T) {
	now := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	old := now.Add(-1 * time.Hour)

	parented := IntakeBead{ID: "gd-child", Type: "task", Status: "open", Labels: []string{"needs-intake"}, HasParent: true, LabeledAt: old}
	if got := SelectPromotable([]IntakeBead{parented}, AutoIntakeSweepConfig{}, now); len(got) != 1 {
		t.Fatalf("parented needs-intake task should be promotable, got %+v", got)
	}
	// The orphan sweep's guard: a parented bead is never an orphan.
	orphanView := OrphanBead{ID: "gd-child", Type: "task", Status: "open", CreatedAt: old, HasParent: true}
	if got := SelectOrphans([]OrphanBead{orphanView}, OrphanSweepConfig{}, now); len(got) != 0 {
		t.Fatalf("parented bead must not be an orphan, got %+v", got)
	}

	// A parentless needs-intake task: NOT promotable (auto-intake skips it), and
	// it already carries the label so the orphan sweep skips it as idempotent —
	// it stays human-gated, promoted by neither sweep.
	parentless := IntakeBead{ID: "gd-loose", Type: "task", Status: "open", Labels: []string{"needs-intake"}, HasParent: false, LabeledAt: old}
	if got := SelectPromotable([]IntakeBead{parentless}, AutoIntakeSweepConfig{}, now); len(got) != 0 {
		t.Fatalf("parentless bead must not be promotable, got %+v", got)
	}
}

// fakeIntakeStore is an in-memory AutoIntakeStore for sweeper tests.
type fakeIntakeStore struct {
	beads     []IntakeBead
	removed   map[string][]string // beadID -> labels removed
	listErr   error
	removeErr map[string]error
	// parented is the set of bead IDs ParentedBeads should report as having a
	// parent-child owning epic/plan.
	parented map[string]bool
	// labeledAt maps bead IDs to the timestamp LabelAddedAt should report.
	labeledAt map[string]time.Time
	// parentErr / labelAgeErr force the corresponding enrichment lookup to fail.
	parentErr   error
	labelAgeErr error
	// parentCalls / labelAgeCalls record the IDs each enrichment lookup was
	// asked about, so tests can assert enrichment is bounded to the candidate
	// set (not the whole intake view).
	parentCalls   [][]string
	labelAgeCalls [][]string
}

func newFakeIntakeStore(beads ...IntakeBead) *fakeIntakeStore {
	return &fakeIntakeStore{beads: beads, removed: map[string][]string{}, removeErr: map[string]error{}}
}

func (f *fakeIntakeStore) ListIntake(_ context.Context) ([]IntakeBead, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.beads, nil
}

func (f *fakeIntakeStore) ParentedBeads(_ context.Context, beadIDs []string) (map[string]bool, error) {
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

func (f *fakeIntakeStore) LabelAddedAt(_ context.Context, beadIDs []string) (map[string]time.Time, error) {
	f.labelAgeCalls = append(f.labelAgeCalls, append([]string(nil), beadIDs...))
	if f.labelAgeErr != nil {
		return nil, f.labelAgeErr
	}
	out := map[string]time.Time{}
	for _, id := range beadIDs {
		if t, ok := f.labeledAt[id]; ok {
			out[id] = t
		}
	}
	return out, nil
}

func (f *fakeIntakeStore) RemoveLabel(_ context.Context, beadID, label string) error {
	if err := f.removeErr[beadID]; err != nil {
		return err
	}
	f.removed[beadID] = append(f.removed[beadID], label)
	// Reflect the removal back onto the in-memory bead so a subsequent sweep
	// sees it as already de-laned (mirrors daemon idempotency).
	for i := range f.beads {
		if f.beads[i].ID != beadID {
			continue
		}
		kept := f.beads[i].Labels[:0:0]
		for _, l := range f.beads[i].Labels {
			if l != label {
				kept = append(kept, l)
			}
		}
		f.beads[i].Labels = kept
	}
	return nil
}

func (f *fakeIntakeStore) removedIDs() []string {
	ids := make([]string, 0, len(f.removed))
	for id := range f.removed {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func TestAutoIntakeSweeperDeLanesPromotable(t *testing.T) {
	now := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	old := now.Add(-1 * time.Hour)
	store := newFakeIntakeStore(
		IntakeBead{ID: "gd-child-1", Type: "task", Status: "open", Labels: []string{"needs-intake"}},
		IntakeBead{ID: "gd-child-2", Type: "bug", Status: "open", Labels: []string{"needs-intake"}},
		IntakeBead{ID: "gd-loose", Type: "task", Status: "open", Labels: []string{"needs-intake"}}, // parentless
		IntakeBead{ID: "gd-owned", Type: "task", Status: "open", Assignee: "x", Labels: []string{"needs-intake"}},
	)
	store.parented = map[string]bool{"gd-child-1": true, "gd-child-2": true}
	store.labeledAt = map[string]time.Time{"gd-child-1": old, "gd-child-2": old}
	s := NewAutoIntakeSweeper(store, quietLogger(), AutoIntakeSweepConfig{}, time.Minute)

	deLaned := s.Sweep(context.Background(), now)
	if deLaned != 2 {
		t.Fatalf("expected 2 de-laned, got %d", deLaned)
	}
	want := []string{"gd-child-1", "gd-child-2"}
	got := store.removedIDs()
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("de-laned %v, want %v", got, want)
	}
	for _, id := range want {
		if labels := store.removed[id]; len(labels) != 1 || labels[0] != DefaultIntakeLabel {
			t.Fatalf("bead %s removed labels %v, want [%s]", id, labels, DefaultIntakeLabel)
		}
	}
}

func TestAutoIntakeSweeperGraceWindow(t *testing.T) {
	now := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	store := newFakeIntakeStore(
		IntakeBead{ID: "gd-fresh", Type: "task", Status: "open", Labels: []string{"needs-intake"}},
	)
	store.parented = map[string]bool{"gd-fresh": true}
	// Labelled only 1 minute ago — inside the 30m default grace window.
	store.labeledAt = map[string]time.Time{"gd-fresh": now.Add(-1 * time.Minute)}
	s := NewAutoIntakeSweeper(store, quietLogger(), AutoIntakeSweepConfig{}, time.Minute)

	if got := s.Sweep(context.Background(), now); got != 0 {
		t.Fatalf("fresh bead in grace window should not be de-laned, got %d", got)
	}
	// After the grace window elapses it becomes promotable.
	later := now.Add(31 * time.Minute)
	if got := s.Sweep(context.Background(), later); got != 1 {
		t.Fatalf("aged bead should be de-laned, got %d", got)
	}
}

func TestAutoIntakeSweeperIdempotent(t *testing.T) {
	now := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	old := now.Add(-1 * time.Hour)
	store := newFakeIntakeStore(
		IntakeBead{ID: "gd-child-1", Type: "task", Status: "open", Labels: []string{"needs-intake"}},
	)
	store.parented = map[string]bool{"gd-child-1": true}
	store.labeledAt = map[string]time.Time{"gd-child-1": old}
	s := NewAutoIntakeSweeper(store, quietLogger(), AutoIntakeSweepConfig{}, time.Minute)

	if got := s.Sweep(context.Background(), now); got != 1 {
		t.Fatalf("first sweep de-laned %d, want 1", got)
	}
	// Second sweep: the label is gone, so nothing to de-lane.
	if got := s.Sweep(context.Background(), now.Add(2*time.Minute)); got != 0 {
		t.Fatalf("second sweep de-laned %d, want 0 (idempotent)", got)
	}
	if labels := store.removed["gd-child-1"]; len(labels) != 1 {
		t.Fatalf("bead de-laned %d times, want exactly 1", len(labels))
	}
}

func TestAutoIntakeSweeperListError(t *testing.T) {
	store := newFakeIntakeStore()
	store.listErr = errors.New("boom")
	s := NewAutoIntakeSweeper(store, quietLogger(), AutoIntakeSweepConfig{}, time.Minute)
	if got := s.Sweep(context.Background(), time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)); got != 0 {
		t.Fatalf("sweep on list error de-laned %d, want 0", got)
	}
}

// TestAutoIntakeSweeperParentErrorSkips pins the fail-safe: a parent-lookup
// failure skips the whole sweep rather than risk promoting an unparented orphan.
func TestAutoIntakeSweeperParentErrorSkips(t *testing.T) {
	now := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	store := newFakeIntakeStore(
		IntakeBead{ID: "gd-child-1", Type: "task", Status: "open", Labels: []string{"needs-intake"}},
	)
	store.parentErr = errors.New("daemon down")
	s := NewAutoIntakeSweeper(store, quietLogger(), AutoIntakeSweepConfig{}, time.Minute)
	if got := s.Sweep(context.Background(), now); got != 0 {
		t.Fatalf("sweep on parent-lookup error de-laned %d, want 0", got)
	}
	if len(store.removed) != 0 {
		t.Fatalf("no bead should be de-laned on parent-lookup error, removed %v", store.removed)
	}
}

func TestAutoIntakeSweeperLabelAgeErrorSkips(t *testing.T) {
	now := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	store := newFakeIntakeStore(
		IntakeBead{ID: "gd-child-1", Type: "task", Status: "open", Labels: []string{"needs-intake"}},
	)
	store.parented = map[string]bool{"gd-child-1": true}
	store.labelAgeErr = errors.New("events unavailable")
	s := NewAutoIntakeSweeper(store, quietLogger(), AutoIntakeSweepConfig{}, time.Minute)
	if got := s.Sweep(context.Background(), now); got != 0 {
		t.Fatalf("sweep on label-age error de-laned %d, want 0", got)
	}
}

// TestAutoIntakeSweeperLabelAgeSkippedWhenGraceDisabled pins that a negative
// MinAge disables the grace guard AND its (per-bead) label-age lookup entirely.
func TestAutoIntakeSweeperLabelAgeSkippedWhenGraceDisabled(t *testing.T) {
	now := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	store := newFakeIntakeStore(
		IntakeBead{ID: "gd-child-1", Type: "task", Status: "open", Labels: []string{"needs-intake"}},
	)
	store.parented = map[string]bool{"gd-child-1": true}
	// Force a failure if LabelAddedAt is ever called — it must not be, since the
	// grace guard is disabled.
	store.labelAgeErr = errors.New("should not be called")
	s := NewAutoIntakeSweeper(store, quietLogger(), AutoIntakeSweepConfig{MinAge: -1}, time.Minute)
	if got := s.Sweep(context.Background(), now); got != 1 {
		t.Fatalf("with grace disabled the bead should be de-laned, got %d", got)
	}
	if len(store.labelAgeCalls) != 0 {
		t.Fatalf("LabelAddedAt must not be called when grace is disabled, calls=%v", store.labelAgeCalls)
	}
}

func TestAutoIntakeSweeperRemoveErrorIsSkipped(t *testing.T) {
	now := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	old := now.Add(-1 * time.Hour)
	store := newFakeIntakeStore(
		IntakeBead{ID: "gd-bad", Type: "task", Status: "open", Labels: []string{"needs-intake"}},
		IntakeBead{ID: "gd-good", Type: "task", Status: "open", Labels: []string{"needs-intake"}},
	)
	store.parented = map[string]bool{"gd-bad": true, "gd-good": true}
	store.labeledAt = map[string]time.Time{"gd-bad": old, "gd-good": old}
	store.removeErr = map[string]error{"gd-bad": errors.New("nope")}
	s := NewAutoIntakeSweeper(store, quietLogger(), AutoIntakeSweepConfig{}, time.Minute)

	if got := s.Sweep(context.Background(), now); got != 1 {
		t.Fatalf("one bead should de-lane despite the other failing, got %d", got)
	}
	if labels := store.removed["gd-good"]; len(labels) != 1 {
		t.Fatalf("gd-good should be de-laned once, got %v", labels)
	}
}

// TestAutoIntakeSweeperEnrichmentBoundedToCandidates pins that the expensive
// parent + label-age lookups are only issued for the small post-pass-1
// candidate set, never the whole intake view.
func TestAutoIntakeSweeperEnrichmentBoundedToCandidates(t *testing.T) {
	now := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	old := now.Add(-1 * time.Hour)
	store := newFakeIntakeStore(
		IntakeBead{ID: "gd-cand", Type: "task", Status: "open", Labels: []string{"needs-intake"}},
		IntakeBead{ID: "gd-epic", Type: "epic", Status: "open", Labels: []string{"needs-intake"}},                 // filtered pass 1 (type)
		IntakeBead{ID: "gd-owned", Type: "task", Status: "open", Assignee: "x", Labels: []string{"needs-intake"}}, // filtered pass 1 (assignee)
	)
	store.parented = map[string]bool{"gd-cand": true}
	store.labeledAt = map[string]time.Time{"gd-cand": old}
	s := NewAutoIntakeSweeper(store, quietLogger(), AutoIntakeSweepConfig{}, time.Minute)

	if got := s.Sweep(context.Background(), now); got != 1 {
		t.Fatalf("expected 1 de-laned, got %d", got)
	}
	if len(store.parentCalls) != 1 || len(store.parentCalls[0]) != 1 || store.parentCalls[0][0] != "gd-cand" {
		t.Fatalf("parent lookup should be bounded to [gd-cand], got %v", store.parentCalls)
	}
	if len(store.labelAgeCalls) != 1 || len(store.labelAgeCalls[0]) != 1 || store.labelAgeCalls[0][0] != "gd-cand" {
		t.Fatalf("label-age lookup should be bounded to [gd-cand], got %v", store.labelAgeCalls)
	}
}

func TestAutoIntakeSweeperTickThrottles(t *testing.T) {
	now := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	old := now.Add(-1 * time.Hour)
	store := newFakeIntakeStore(
		IntakeBead{ID: "gd-child-1", Type: "task", Status: "open", Labels: []string{"needs-intake"}},
	)
	store.parented = map[string]bool{"gd-child-1": true}
	store.labeledAt = map[string]time.Time{"gd-child-1": old}
	s := NewAutoIntakeSweeper(store, quietLogger(), AutoIntakeSweepConfig{}, 5*time.Minute)

	s.Tick(context.Background(), now)
	// A second tick well within the interval must not run a sweep (no new
	// removals; gd-child-1 already de-laned once).
	s.Tick(context.Background(), now.Add(time.Minute))
	if labels := store.removed["gd-child-1"]; len(labels) != 1 {
		t.Fatalf("throttled tick should not re-sweep; removals=%v", labels)
	}
}
