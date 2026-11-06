package pausegate

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// fakeGateClient is a minimal in-memory Client: it records opens and serves
// Find off a seeded set of open gate beads.
type fakeGateClient struct {
	open      map[string]*beadsapi.BeadDetail // id -> bead
	listErr   error
	createErr error
	nextID    int
	created   []beadsapi.CreateBeadRequest
}

func newFakeGateClient() *fakeGateClient {
	return &fakeGateClient{open: map[string]*beadsapi.BeadDetail{}}
}

func (f *fakeGateClient) seed(id, title string) {
	f.open[id] = &beadsapi.BeadDetail{ID: id, Title: title, Type: "gate", Status: "open"}
}

func (f *fakeGateClient) ListBeadsFiltered(_ context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	var out []*beadsapi.BeadDetail
	for _, b := range f.open {
		// Mimic the daemon: Search is a substring prefilter, Titles is exact,
		// and Limit cuts the page. Type is asserted so a wrong query surfaces.
		if len(q.Types) != 1 || q.Types[0] != "gate" {
			continue
		}
		if q.Search != "" && !strings.Contains(b.Title, q.Search) {
			continue
		}
		if len(q.Titles) > 0 && !slices.Contains(q.Titles, b.Title) {
			continue
		}
		out = append(out, b)
	}
	// The daemon orders a search page newest first; seeding order stands in
	// for age, so later seeds lead the page.
	slices.SortFunc(out, func(a, b *beadsapi.BeadDetail) int { return strings.Compare(b.ID, a.ID) })
	if q.Limit > 0 && len(out) > q.Limit {
		out = out[:q.Limit]
	}
	return &beadsapi.ListBeadsResult{Beads: out, Total: len(out)}, nil
}

func (f *fakeGateClient) CreateBead(_ context.Context, req beadsapi.CreateBeadRequest) (string, error) {
	if f.createErr != nil {
		return "", f.createErr
	}
	f.created = append(f.created, req)
	f.nextID++
	id := "gate-new"
	f.seed(id, req.Title)
	return id, nil
}

func TestFind_ExactTitleMatch(t *testing.T) {
	f := newFakeGateClient()
	f.seed("g1", GardenersPauseTitle)
	f.seed("g2", SchedulerPauseTitle)

	got, err := Find(context.Background(), f, GardenersPauseTitle)
	if err != nil {
		t.Fatalf("Find err: %v", err)
	}
	if got == nil || got.ID != "g1" {
		t.Fatalf("Find(%q) = %+v, want g1", GardenersPauseTitle, got)
	}
}

func TestFind_NoMatchReturnsNil(t *testing.T) {
	f := newFakeGateClient()
	f.seed("g2", SchedulerPauseTitle)

	got, err := Find(context.Background(), f, GardenersPauseTitle)
	if err != nil {
		t.Fatalf("Find err: %v", err)
	}
	if got != nil {
		t.Fatalf("Find with no gardeners-pause gate = %+v, want nil", got)
	}
}

func TestFind_IgnoresFuzzyTitlePrefixMatch(t *testing.T) {
	// A gate whose title merely CONTAINS the query (the daemon Search is fuzzy)
	// must not be returned — only an exact title is the engaged gate.
	f := newFakeGateClient()
	f.seed("g3", GardenersPauseTitle+"-experimental")

	got, err := Find(context.Background(), f, GardenersPauseTitle)
	if err != nil {
		t.Fatalf("Find err: %v", err)
	}
	if got != nil {
		t.Fatalf("Find matched a fuzzy title %q, want nil", f.open["g3"].Title)
	}
}

// Other open gates that mention the title must not hide the engaged one: a
// short fuzzy page filled with them read as "not engaged", and the
// fail-closed lands-pause switch let tenant spawns through (gd-i74esUHCTg).
func TestFind_NotCrowdedOutByGatesMentioningTitle(t *testing.T) {
	f := newFakeGateClient()
	f.seed("g0", LandsPauseTitle)
	for i := 1; i <= 8; i++ {
		f.seed(fmt.Sprintf("g%d", i), fmt.Sprintf("%s drill %d", LandsPauseTitle, i))
	}

	got, err := Find(context.Background(), f, LandsPauseTitle)
	if err != nil {
		t.Fatalf("Find err: %v", err)
	}
	if got == nil || got.ID != "g0" {
		t.Fatalf("Find(%q) = %+v, want g0 behind 8 gates that mention it", LandsPauseTitle, got)
	}
}

func TestFind_PropagatesError(t *testing.T) {
	f := newFakeGateClient()
	f.listErr = errors.New("boom")
	if _, err := Find(context.Background(), f, GardenersPauseTitle); err == nil {
		t.Fatal("Find should propagate the list error")
	}
}

func TestOpen_CreatesGateBead(t *testing.T) {
	f := newFakeGateClient()
	id, err := Open(context.Background(), f, GardenersPauseTitle, "oidc:matt@x")
	if err != nil {
		t.Fatalf("Open err: %v", err)
	}
	if id == "" {
		t.Fatal("Open returned empty id")
	}
	if len(f.created) != 1 {
		t.Fatalf("Open created %d beads, want 1", len(f.created))
	}
	req := f.created[0]
	if req.Title != GardenersPauseTitle || req.Type != "gate" || req.CreatedBy != "oidc:matt@x" {
		t.Fatalf("Open created wrong bead: %+v", req)
	}
	// The newly opened gate must be findable — Open→Find round-trips.
	got, err := Find(context.Background(), f, GardenersPauseTitle)
	if err != nil || got == nil {
		t.Fatalf("Find after Open = (%+v, %v), want the new gate", got, err)
	}
}

// TestTitles_MatchOperatorContract pins the well-known strings — the fleet has
// live gate beads keyed on these exact titles, and the fire-time reads
// (bridge.gardenersPauseEngaged / triggers.loadGateState) match on them. A change
// here silently orphans every open gate, so guard the literals.
func TestTitles_MatchOperatorContract(t *testing.T) {
	if GardenersPauseTitle != "gardeners-pause" {
		t.Fatalf("GardenersPauseTitle = %q, want gardeners-pause", GardenersPauseTitle)
	}
	if SchedulerPauseTitle != "scheduler-pause" {
		t.Fatalf("SchedulerPauseTitle = %q, want scheduler-pause", SchedulerPauseTitle)
	}
}
