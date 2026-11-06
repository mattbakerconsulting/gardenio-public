package bridge

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// iterCacheDaemon is a gardenerDaemon that filters ListBeadsFiltered the way
// the beads daemon does for the iteration reads: type, statuses, field
// equality, updated_after as the wire sends it (RFC3339, whole seconds,
// strictly greater), created_at sort in either direction (id breaks ties) and
// limit. It records every query.
type iterCacheDaemon struct {
	gardenerDaemon
	beads    []*beadsapi.BeadDetail
	queries  []beadsapi.ListBeadsQuery
	failNext bool
}

func (d *iterCacheDaemon) ListBeadsFiltered(_ context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	d.queries = append(d.queries, q)
	if d.failNext {
		d.failNext = false
		return nil, errors.New("daemon blip")
	}
	var after time.Time
	if !q.UpdatedAfter.IsZero() {
		after, _ = time.Parse(time.RFC3339, q.UpdatedAfter.UTC().Format(time.RFC3339))
	}
	var out []*beadsapi.BeadDetail
	for _, b := range d.beads {
		if len(q.Types) > 0 && !slices.Contains(q.Types, b.Type) {
			continue
		}
		if len(q.Statuses) > 0 && !slices.Contains(q.Statuses, b.Status) {
			continue
		}
		match := true
		for k, v := range q.Fields {
			if b.Fields[k] != v {
				match = false
			}
		}
		if !match || (!after.IsZero() && !b.UpdatedAt.After(after)) {
			continue
		}
		out = append(out, b)
	}
	switch q.Sort {
	case "created_at", "-created_at":
		desc := q.Sort == "-created_at"
		slices.SortStableFunc(out, func(a, b *beadsapi.BeadDetail) int {
			c := a.CreatedAt.Compare(b.CreatedAt)
			if desc {
				c = -c
			}
			if c != 0 {
				return c
			}
			return cmp.Compare(a.ID, b.ID)
		})
	}
	if q.Limit > 0 && len(out) > q.Limit {
		out = out[:q.Limit]
	}
	return &beadsapi.ListBeadsResult{Beads: out, Total: len(out)}, nil
}

func iterBead(id, status string, created, updated time.Time) *beadsapi.BeadDetail {
	return &beadsapi.BeadDetail{
		ID: id, Type: "agent", Status: status,
		Fields:    map[string]string{"trigger_id": "gd-g", "schedule_id": "gd-g"},
		CreatedAt: created, UpdatedAt: updated,
	}
}

func iterIDs(beads []*beadsapi.BeadDetail) []string {
	ids := make([]string, 0, len(beads))
	for _, b := range beads {
		ids = append(ids, b.ID)
	}
	return ids
}

type iterCacheClock struct{ t time.Time }

func (c *iterCacheClock) now() time.Time { return c.t }

func newIterCacheFixture() (*iterCacheDaemon, *closedIterationCache, *iterCacheClock) {
	clock := &iterCacheClock{t: time.Date(2026, 10, 24, 7, 0, 0, 400_000_000, time.UTC)}
	return &iterCacheDaemon{}, newClosedIterationCache(clock.now), clock
}

// The first read is the full closed list; later cycles read only the delta,
// which carries no status filter so a reopened iteration leaves the set.
func TestClosedIterationCache_DeltaAfterFullList(t *testing.T) {
	d, c, clock := newIterCacheFixture()
	t0 := clock.t
	d.beads = []*beadsapi.BeadDetail{
		iterBead("gd-a", "closed", t0.Add(-3*time.Hour), t0.Add(-3*time.Hour)),
		iterBead("gd-b", "closed", t0.Add(-2*time.Hour), t0.Add(-2*time.Hour)),
		iterBead("gd-live", "in_progress", t0.Add(-time.Hour), t0.Add(-time.Minute)),
	}
	ctx, logger := context.Background(), slog.Default()

	if got := iterIDs(c.closedIterations(ctx, d, logger, "gd-g")); !slices.Equal(got, []string{"gd-b", "gd-a"}) {
		t.Fatalf("first read: got %v, want [gd-b gd-a]", got)
	}
	for _, q := range d.queries {
		if !slices.Equal(q.Statuses, []string{"closed"}) || !q.UpdatedAfter.IsZero() || q.Sort != "-created_at" {
			t.Fatalf("first read must be the full closed list, got %+v", q)
		}
	}
	if len(d.queries) != 2 {
		t.Fatalf("first read: %d queries, want one per owner key", len(d.queries))
	}

	// Next cycle: gd-live closes, gd-a is reopened.
	clock.t = t0.Add(30 * time.Second)
	d.beads[2].Status, d.beads[2].UpdatedAt = "closed", clock.t.Add(-5*time.Second)
	d.beads[0].Status, d.beads[0].UpdatedAt = "open", clock.t.Add(-4*time.Second)
	d.queries = nil

	if got := iterIDs(c.closedIterations(ctx, d, logger, "gd-g")); !slices.Equal(got, []string{"gd-live", "gd-b"}) {
		t.Fatalf("after delta: got %v, want [gd-live gd-b]", got)
	}
	if len(d.queries) != 2 {
		t.Fatalf("delta: %d queries, want 2", len(d.queries))
	}
	for _, q := range d.queries {
		if len(q.Statuses) != 0 {
			t.Errorf("delta must not filter on status (a reopened iteration would never leave the set): %+v", q.Statuses)
		}
		if want := t0.Add(-iterationCursorSlack); !q.UpdatedAfter.Equal(want) {
			t.Errorf("delta updated_after = %v, want the previous read's start minus the slack (%v)", q.UpdatedAfter, want)
		}
	}
}

// The cursor is stepped back from the previous read's start, not set to it:
// updated_at is stamped before commit, so a bead stamped just before a read
// can become visible only after it. updated_after also goes over the wire in
// whole seconds and filters strictly (gd-o7niMQlohc). The slack covers both.
func TestClosedIterationCache_LateCommitIsNotSkipped(t *testing.T) {
	d, c, clock := newIterCacheFixture()
	t0 := clock.t // 07:00:00.4
	ctx, logger := context.Background(), slog.Default()
	_ = c.closedIterations(ctx, d, logger, "gd-g")

	// Stamped 06:59:58.4, committed after the first read ran.
	d.beads = append(d.beads, iterBead("gd-late", "closed", t0.Add(-time.Minute), t0.Add(-2*time.Second)))
	clock.t = t0.Add(30 * time.Second)
	if got := iterIDs(c.closedIterations(ctx, d, logger, "gd-g")); !slices.Equal(got, []string{"gd-late"}) {
		t.Fatalf("got %v, want [gd-late]", got)
	}
}

// A failed delta keeps the last known set rather than emptying it.
func TestClosedIterationCache_DeltaErrorKeepsSet(t *testing.T) {
	d, c, clock := newIterCacheFixture()
	d.beads = []*beadsapi.BeadDetail{iterBead("gd-a", "closed", clock.t.Add(-time.Hour), clock.t.Add(-time.Hour))}
	ctx, logger := context.Background(), slog.Default()
	_ = c.closedIterations(ctx, d, logger, "gd-g")

	clock.t = clock.t.Add(30 * time.Second)
	d.failNext = true
	if got := iterIDs(c.closedIterations(ctx, d, logger, "gd-g")); !slices.Equal(got, []string{"gd-a"}) {
		t.Fatalf("got %v, want [gd-a]", got)
	}
}

// The set is re-listed in full every iterationFullRelistEvery, and at once
// when a delta fills its page.
func TestClosedIterationCache_FullRelist(t *testing.T) {
	d, c, clock := newIterCacheFixture()
	ctx, logger := context.Background(), slog.Default()
	_ = c.closedIterations(ctx, d, logger, "gd-g")

	fullLists := func() int {
		n := 0
		for _, q := range d.queries {
			if slices.Equal(q.Statuses, []string{"closed"}) {
				n++
			}
		}
		return n
	}

	d.queries = nil
	clock.t = clock.t.Add(iterationFullRelistEvery)
	_ = c.closedIterations(ctx, d, logger, "gd-g")
	if got := fullLists(); got != 2 {
		t.Fatalf("after %v: %d full lists, want 2", iterationFullRelistEvery, got)
	}

	// A delta that comes back full re-lists that key.
	start := clock.t
	for i := range gardenerIterationFinalizeLimit {
		at := start.Add(time.Duration(i) * time.Millisecond)
		d.beads = append(d.beads, iterBead(fmt.Sprintf("gd-n%03d", i), "closed", at, at.Add(time.Second)))
	}
	d.queries = nil
	clock.t = start.Add(30 * time.Second)
	got := c.closedIterations(ctx, d, logger, "gd-g")
	if n := fullLists(); n != 2 {
		t.Fatalf("saturated delta: %d full lists, want 2 (one per key)", n)
	}
	if len(got) != gardenerIterationFinalizeLimit {
		t.Fatalf("got %d iterations, want %d", len(got), gardenerIterationFinalizeLimit)
	}
}

// Like the full list, the result is the newest gardenerIterationFinalizeLimit
// by created_at: a delta's new iteration joins it and pushes out the oldest, so
// a gardener past the limit keeps reconciling its latest runs (gd-Ahi1H2jR9a).
func TestClosedIterationCache_BoundMatchesFullList(t *testing.T) {
	d, c, clock := newIterCacheFixture()
	base := clock.t.Add(-48 * time.Hour)
	for i := range gardenerIterationFinalizeLimit {
		at := base.Add(time.Duration(i) * time.Minute)
		d.beads = append(d.beads, iterBead(fmt.Sprintf("gd-o%03d", i), "closed", at, at))
	}
	ctx, logger := context.Background(), slog.Default()
	_ = c.closedIterations(ctx, d, logger, "gd-g")

	d.beads = append(d.beads, iterBead("gd-newest", "closed", clock.t, clock.t))
	clock.t = clock.t.Add(30 * time.Second)
	got := c.closedIterations(ctx, d, logger, "gd-g")
	if len(got) != gardenerIterationFinalizeLimit || got[0].ID != "gd-newest" || slices.Contains(iterIDs(got), "gd-o000") {
		t.Fatalf("got %d iterations starting %s; want the newest %d, gd-newest first, without gd-o000", len(got), got[0].ID, gardenerIterationFinalizeLimit)
	}
	full, ok := listClosedIterations(ctx, d, logger, "gd-g", "trigger_id")
	if !ok || !slices.Equal(iterIDs(got), iterIDs(full)) {
		t.Fatalf("cached view after a delta differs from the full list")
	}

	// Callers sort the slice in place; that must not reach the cache.
	slices.Reverse(got)
	clock.t = clock.t.Add(30 * time.Second)
	if again := c.closedIterations(ctx, d, logger, "gd-g"); again[0].ID != "gd-newest" {
		t.Fatalf("cache order changed by a caller's sort: first is %s", again[0].ID)
	}
}

// evalOne hands the reconcilers a daemon whose gardenerClosedIterations is the
// cache; an engine built without NewTriggerEngine lists directly.
func TestTriggerEngine_IterationDaemonUsesCache(t *testing.T) {
	e := NewTriggerEngine(TriggerEngineConfig{Daemon: &mockTriggerDaemon{}})
	if _, ok := e.iterationDaemon().(closedIterationSource); !ok {
		t.Fatal("NewTriggerEngine: iteration daemon does not serve from the cache")
	}
	bare := &TriggerEngine{daemon: &mockTriggerDaemon{}}
	if _, ok := bare.iterationDaemon().(closedIterationSource); ok {
		t.Fatal("engine without a cache must list iterations directly")
	}
}
