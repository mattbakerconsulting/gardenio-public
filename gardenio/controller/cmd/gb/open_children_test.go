package main

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// TestFilterOpenParentChildren_UmbrellaCase covers the ka-0000003110 fix:
// gardener-bugfix must skip bugs that have open parent-child children. The
// poster child is ka-0000002333, whose reverse-deps include ka-0000003384
// (Stage 3 epic, open) plus a constellation of `assigned`-type dep rows
// (one per crew that ever claimed the bug). Only the parent-child rows are
// the umbrella signal; assigned rows must be ignored.
func TestFilterOpenParentChildren_UmbrellaCase(t *testing.T) {
	// Real-shape fixture lifted from `kd children ka-0000002333 --json` so
	// the test stays anchored to the bug it's protecting against.
	deps := []beadsapi.Dependency{
		// assigned crew rows — must be ignored regardless of status.
		{BeadID: "gd-Ml1LoPuGcC", DependsOnID: "ka-0000002333", Type: "assigned"},
		{BeadID: "ka-0000007408", DependsOnID: "ka-0000002333", Type: "parent-child"}, // W-E-2 task, open
		{BeadID: "gd-U8PF4v4S8V", DependsOnID: "ka-0000002333", Type: "assigned"},
		{BeadID: "ka-0000003384", DependsOnID: "ka-0000002333", Type: "parent-child"}, // Stage 3 epic, open
		{BeadID: "ka-0000002789", DependsOnID: "ka-0000002333", Type: "parent-child"}, // task, closed
		{BeadID: "gd-0Iw6xyjp6I", DependsOnID: "ka-0000002333", Type: "parent-child"}, // W-E-4 task, closed
		{BeadID: "ka-0000002895", DependsOnID: "ka-0000002333", Type: "parent-child"}, // W-B-7 task, in_progress
	}
	info := map[string]childInfo{
		"ka-0000007408": {Type: "task", Status: "open"},
		"ka-0000003384": {Type: "epic", Status: "open"},
		"ka-0000002789": {Type: "task", Status: "closed"},
		"gd-0Iw6xyjp6I": {Type: "task", Status: "closed"},
		"ka-0000002895": {Type: "task", Status: "in_progress"},
	}

	got := filterOpenParentChildren(deps, info)
	want := []string{"ka-0000007408", "ka-0000003384", "ka-0000002895"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("filterOpenParentChildren = %v, want %v", got, want)
	}
}

func TestFilterOpenParentChildren_NoChildren(t *testing.T) {
	if got := filterOpenParentChildren(nil, nil); len(got) != 0 {
		t.Fatalf("expected empty for nil input, got %v", got)
	}
}

// TestFilterOpenParentChildren_AllClosed — a bug whose parent-child children
// are all closed is NOT an umbrella; the bug itself is dispatchable.
func TestFilterOpenParentChildren_AllClosed(t *testing.T) {
	deps := []beadsapi.Dependency{
		{BeadID: "gd-a", DependsOnID: "gd-root", Type: "parent-child"},
		{BeadID: "gd-b", DependsOnID: "gd-root", Type: "parent-child"},
	}
	info := map[string]childInfo{
		"gd-a": {Type: "task", Status: "closed"},
		"gd-b": {Type: "bug", Status: "closed"},
	}
	if got := filterOpenParentChildren(deps, info); len(got) != 0 {
		t.Fatalf("expected empty for all-closed children, got %v", got)
	}
}

// TestFilterOpenParentChildren_UnknownStatusExcluded — if we couldn't fetch
// a child's status, treat it as unknown and exclude. Prefer one extra crew
// dispatch over silently skipping a real bug.
func TestFilterOpenParentChildren_UnknownStatusExcluded(t *testing.T) {
	deps := []beadsapi.Dependency{
		{BeadID: "gd-a", DependsOnID: "gd-root", Type: "parent-child"},
		{BeadID: "gd-b", DependsOnID: "gd-root", Type: "parent-child"},
	}
	// gd-a: type known but status empty (fetch failure sentinel);
	// gd-b: never populated (also unknown). Both must be excluded.
	info := map[string]childInfo{"gd-a": {Type: "task", Status: ""}}
	if got := filterOpenParentChildren(deps, info); len(got) != 0 {
		t.Fatalf("expected empty for unknown-status children, got %v", got)
	}
}

// TestFilterOpenParentChildren_NonParentChildIgnored — only parent-child edges
// count. blocks/related/duplicates/etc. are not umbrella signals.
func TestFilterOpenParentChildren_NonParentChildIgnored(t *testing.T) {
	deps := []beadsapi.Dependency{
		{BeadID: "gd-x", DependsOnID: "gd-root", Type: "blocks"},
		{BeadID: "gd-y", DependsOnID: "gd-root", Type: "related"},
		{BeadID: "gd-z", DependsOnID: "gd-root", Type: "duplicates"},
	}
	info := map[string]childInfo{
		"gd-x": {Type: "bug", Status: "open"},
		"gd-y": {Type: "task", Status: "open"},
		"gd-z": {Type: "epic", Status: "open"},
	}
	if got := filterOpenParentChildren(deps, info); len(got) != 0 {
		t.Fatalf("expected empty when only non-parent-child edges, got %v", got)
	}
}

// TestFilterOpenParentChildren_SessionChildExcluded is the ka-0000001519
// regression: ka-0000003962's sole open parent-child child gd-XaoxLSFtlY is a
// type=session, kind=data agent-session record — NOT staged acceptance.
// Counting it made a real leaf bug look like an umbrella and suppressed
// dispatch forever. A session child must never appear in the open set.
func TestFilterOpenParentChildren_SessionChildExcluded(t *testing.T) {
	deps := []beadsapi.Dependency{
		{BeadID: "gd-XaoxLSFtlY", DependsOnID: "ka-0000003962", Type: "parent-child"},
	}
	info := map[string]childInfo{
		// session:gardenio-coop-emits-no-5bs:2026-07-25T05:35:45Z
		"gd-XaoxLSFtlY": {Type: "session", Status: "open"},
	}
	if got := filterOpenParentChildren(deps, info); len(got) != 0 {
		t.Fatalf("session child must be excluded, got %v", got)
	}
}

// TestFilterOpenParentChildren_BundleChildExcluded — narrative bundle children
// (gd-PFbH0QUN6m etc.) are type=bundle, kind=issue. Because they are kind=issue
// (not kind=data), a plain kind filter would NOT exclude them — only the type
// allowlist does. They are auto-created narrative docs, not staged acceptance.
func TestFilterOpenParentChildren_BundleChildExcluded(t *testing.T) {
	deps := []beadsapi.Dependency{
		{BeadID: "gd-PFbH0QUN6m", DependsOnID: "gd-2nMut27bMJ", Type: "parent-child"},
	}
	info := map[string]childInfo{
		"gd-PFbH0QUN6m": {Type: "bundle", Status: "open"},
	}
	if got := filterOpenParentChildren(deps, info); len(got) != 0 {
		t.Fatalf("bundle child must be excluded, got %v", got)
	}
}

// TestFilterOpenParentChildren_MixedRealAndNoise — a bug with BOTH a real open
// staged child (type=bug) and session/bundle noise children must still report
// as an umbrella (return only the real child). This is the true-positive half
// that the fix must not break: ka-0000006967 -> gd-Eat2FNTD8s (bug) is a
// legitimate umbrella per the bug report's evidence table.
func TestFilterOpenParentChildren_MixedRealAndNoise(t *testing.T) {
	deps := []beadsapi.Dependency{
		{BeadID: "gd-sess", DependsOnID: "gd-root", Type: "parent-child"},
		{BeadID: "gd-Eat2FNTD8s", DependsOnID: "gd-root", Type: "parent-child"},
		{BeadID: "gd-bundle", DependsOnID: "gd-root", Type: "parent-child"},
	}
	info := map[string]childInfo{
		"gd-sess":       {Type: "session", Status: "open"},
		"gd-Eat2FNTD8s": {Type: "bug", Status: "open"},
		"gd-bundle":     {Type: "bundle", Status: "open"},
	}
	got := filterOpenParentChildren(deps, info)
	want := []string{"gd-Eat2FNTD8s"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("filterOpenParentChildren = %v, want %v", got, want)
	}
}

// fakeFetcher implements beadFetcher for the openParentChildren test —
// records call order so we can verify we only fetch parent-child rows.
type fakeFetcher struct {
	deps      []beadsapi.Dependency
	beads     map[string]*beadsapi.BeadDetail
	depErr    error
	beadErrs  map[string]error
	beadCalls []string
}

func (f *fakeFetcher) GetReverseDependencies(_ context.Context, _ string) ([]beadsapi.Dependency, error) {
	return f.deps, f.depErr
}

func (f *fakeFetcher) GetBead(_ context.Context, beadID string) (*beadsapi.BeadDetail, error) {
	f.beadCalls = append(f.beadCalls, beadID)
	if err, ok := f.beadErrs[beadID]; ok {
		return nil, err
	}
	return f.beads[beadID], nil
}

// TestOpenParentChildren_QueriesOnlyParentChild verifies the wrapper only
// fetches beads for parent-child rows — the ka-0000002333 fixture has 27+
// reverse-dep rows but only ~9 are parent-child. N+1 is bad enough on the
// real shape; we don't want to make it (3+N).
func TestOpenParentChildren_QueriesOnlyParentChild(t *testing.T) {
	f := &fakeFetcher{
		deps: []beadsapi.Dependency{
			{BeadID: "gd-assigned1", DependsOnID: "gd-root", Type: "assigned"},
			{BeadID: "gd-child1", DependsOnID: "gd-root", Type: "parent-child"},
			{BeadID: "gd-assigned2", DependsOnID: "gd-root", Type: "assigned"},
			{BeadID: "gd-child2", DependsOnID: "gd-root", Type: "parent-child"},
		},
		beads: map[string]*beadsapi.BeadDetail{
			"gd-child1": {Type: "task", Status: "open"},
			"gd-child2": {Type: "task", Status: "closed"},
		},
	}
	got, _, err := openParentChildren(context.Background(), f, "gd-root")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"gd-child1"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("openParentChildren = %v, want %v", got, want)
	}
	wantCalls := []string{"gd-child1", "gd-child2"}
	if !reflect.DeepEqual(f.beadCalls, wantCalls) {
		t.Fatalf("bead calls = %v, want %v", f.beadCalls, wantCalls)
	}
}

// TestOpenParentChildren_SessionChildSuppressed is the end-to-end ka-0000001519
// regression through the daemon-fetching wrapper: a bug whose only open
// parent-child child is a type=session record must report NO open children, so
// gardener-bugfix dispatches it instead of skipping it forever.
func TestOpenParentChildren_SessionChildSuppressed(t *testing.T) {
	f := &fakeFetcher{
		deps: []beadsapi.Dependency{
			{BeadID: "gd-XaoxLSFtlY", DependsOnID: "ka-0000003962", Type: "parent-child"},
		},
		beads: map[string]*beadsapi.BeadDetail{
			"gd-XaoxLSFtlY": {Type: "session", Kind: "data", Status: "open"},
		},
	}
	got, _, err := openParentChildren(context.Background(), f, "ka-0000003962")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("session-only child must yield no open children (dispatchable), got %v", got)
	}
}

// TestOpenParentChildren_DaemonError surfaces a reverse-deps fetch failure as
// an error rather than silently returning empty (which would let an umbrella
// bug through).
func TestOpenParentChildren_DaemonError(t *testing.T) {
	f := &fakeFetcher{depErr: errors.New("boom")}
	if _, _, err := openParentChildren(context.Background(), f, "gd-root"); err == nil {
		t.Fatal("expected error when GetReverseDependencies fails")
	}
}

// TestOpenParentChildren_BeadFetchFailExcluded — if GetBead errors, we treat
// that child as unknown (exclude from the open set). Documented trade-off:
// prefer false negatives (one extra dispatch) over false positives (silently
// skipping a real bug).
func TestOpenParentChildren_BeadFetchFailExcluded(t *testing.T) {
	f := &fakeFetcher{
		deps: []beadsapi.Dependency{
			{BeadID: "gd-flaky", DependsOnID: "gd-root", Type: "parent-child"},
		},
		beadErrs: map[string]error{"gd-flaky": errors.New("transient")},
	}
	got, _, err := openParentChildren(context.Background(), f, "gd-root")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty on bead fetch failure, got %v", got)
	}
}

// TestOpenParentChildren_UnreadableChildReported: a child the daemon answers
// 404 for (outside the caller's project scope) is reported as unreadable, not
// dropped, and stays out of the open set so stdout keeps its umbrella meaning
// (gd-BYCG4IGJB2).
func TestOpenParentChildren_UnreadableChildReported(t *testing.T) {
	f := &fakeFetcher{
		deps: []beadsapi.Dependency{
			{BeadID: "gd-open", DependsOnID: "gd-plan", Type: "parent-child"},
			{BeadID: "gd-hidden", DependsOnID: "gd-plan", Type: "parent-child"},
			{BeadID: "gd-flaky", DependsOnID: "gd-plan", Type: "parent-child"},
		},
		beads: map[string]*beadsapi.BeadDetail{"gd-open": {Type: "task", Status: "open"}},
		beadErrs: map[string]error{
			"gd-hidden": errors.New("getting bead gd-hidden: HTTP 404: bead not found"),
			"gd-flaky":  errors.New("transient"),
		},
	}
	open, unreadable, err := openParentChildren(context.Background(), f, "gd-plan")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !reflect.DeepEqual(open, []string{"gd-open"}) {
		t.Fatalf("open = %v, want [gd-open]", open)
	}
	if !reflect.DeepEqual(unreadable, []string{"gd-hidden"}) {
		t.Fatalf("unreadable = %v, want [gd-hidden]", unreadable)
	}
	var buf bytes.Buffer
	printUnreadableChildren(&buf, "", unreadable)
	if got, want := buf.String(), "gd-hidden (unreadable: outside your project scope)\n"; got != want {
		t.Fatalf("printed %q, want %q", got, want)
	}
}
