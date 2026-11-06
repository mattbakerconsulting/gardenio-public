package metrics

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"gardenio/controller/internal/advice"
	"gardenio/controller/internal/beadsapi"
)

// errFake is the daemon failure injected via mockQuerier.beadErr.
var errFake = errors.New("daemon unavailable")

func adviceCatalogBead(id string, descLen int, labels []string) *beadsapi.BeadDetail {
	return &beadsapi.BeadDetail{
		ID:          id,
		Title:       "advice " + id,
		Description: strings.Repeat("x", descLen),
		Type:        "advice",
		Status:      "open",
		Labels:      labels,
	}
}

// agentBeadDetail builds an agent-type bead carrying the project/role fields the
// collector's fleet archetype derivation reads (advice.ArchetypesFromAgentBeads).
func agentBeadDetail(id, project, role string) *beadsapi.BeadDetail {
	return &beadsapi.BeadDetail{
		ID:     id,
		Title:  "agent " + id,
		Type:   "agent",
		Status: "closed",
		Fields: map[string]string{"project": project, "role": role},
	}
}

// TestCollectAdviceCatalog_PopulatesGauges runs the collector against a small
// corpus and pins the per-(project,role,scope) bytes/bead-count series. The
// corpus derives one archetype (gardenio/gardener); the opted-in broadcast bead
// counts under scope=project, the role-gated bead under scope=role, and the
// denied bare-project bead (no opt-in) must not count at all — mirroring the
// Phase-3 delivery deny (ka-0000007790).
func TestCollectAdviceCatalog_PopulatesGauges(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	c := New(&mockQuerier{
		beads: []*beadsapi.BeadDetail{
			adviceCatalogBead("gd-glob", 400, []string{"global"}),
			adviceCatalogBead("gd-bare", 1000, []string{"project:gardenio", "scope:project-broadcast"}),
			adviceCatalogBead("gd-capt", 2000, []string{"g0:project:gardenio", "g0:role:gardener"}),
			// Hippo mirror: dropped by the shared delivery predicate, must not count.
			adviceCatalogBead("gd-hippo", 5000, []string{"project:gardenio", "source:hippo-memory"}),
			// Denied broadcast: bare-project without the opt-in, dropped from the
			// catalog but visible in the broadcast gauge below.
			adviceCatalogBead("gd-denied", 7000, []string{"project:gardenio"}),
		},
		// The real fleet ran exactly one archetype: gardenio/gardener.
		agentBeadDetails: []*beadsapi.BeadDetail{
			agentBeadDetail("gd-ag1", "gardenio", "gardener"),
		},
	}, logger, 30*time.Second)

	// The archetype grid refreshes asynchronously (ka-0000006669); prime it
	// synchronously here so this test pins the grid→gauge audit mapping, not the
	// async timing (covered by the dedicated priming tests below).
	c.refreshFleetArchetypes(context.Background())
	c.collectAdviceCatalog(context.Background())

	want := `
# HELP gardenio_advice_catalog_bytes Matched advice-catalog description bytes per (project, role) archetype, by targeting scope. Simulated through the real delivery pipeline (MatchesSubscriptions + supersession). Divide by 4 for ~tokens; the Part-A budget is 18k tokens/archetype (ka-0000003733).
# TYPE gardenio_advice_catalog_bytes gauge
gardenio_advice_catalog_bytes{project="gardenio",role="gardener",scope="global"} 400
gardenio_advice_catalog_bytes{project="gardenio",role="gardener",scope="project"} 1000
gardenio_advice_catalog_bytes{project="gardenio",role="gardener",scope="role"} 2000
`
	if err := testutil.CollectAndCompare(c.adviceCatalogBytes, strings.NewReader(want), "gardenio_advice_catalog_bytes"); err != nil {
		t.Errorf("unexpected advice_catalog_bytes series: %v", err)
	}

	wantBeads := `
# HELP gardenio_advice_catalog_beads Matched advice-bead count per (project, role) archetype, by targeting scope. Same simulation as gardenio_advice_catalog_bytes.
# TYPE gardenio_advice_catalog_beads gauge
gardenio_advice_catalog_beads{project="gardenio",role="gardener",scope="global"} 1
gardenio_advice_catalog_beads{project="gardenio",role="gardener",scope="project"} 1
gardenio_advice_catalog_beads{project="gardenio",role="gardener",scope="role"} 1
`
	if err := testutil.CollectAndCompare(c.adviceCatalogBeads, strings.NewReader(wantBeads), "gardenio_advice_catalog_beads"); err != nil {
		t.Errorf("unexpected advice_catalog_beads series: %v", err)
	}

	wantBroadcast := `
# HELP gardenio_advice_broadcast_beads Open bare-project broadcast advice beads per project, by delivery status (delivered = scope:project-broadcast opt-in, denied = dropped by delivery). Broadcast beads reach every agent in the project (ka-0000003733 Phase 3).
# TYPE gardenio_advice_broadcast_beads gauge
gardenio_advice_broadcast_beads{project="gardenio",status="delivered"} 1
gardenio_advice_broadcast_beads{project="gardenio",status="denied"} 1
`
	if err := testutil.CollectAndCompare(c.adviceBroadcastBeads, strings.NewReader(wantBroadcast), "gardenio_advice_broadcast_beads"); err != nil {
		t.Errorf("unexpected advice_broadcast_beads series: %v", err)
	}
}

// TestCollectAdviceCatalog_ResetClearsRetiredArchetypes asserts a shrunk fleet
// drops stale series on the next collection — an archetype the fleet stops
// running must disappear from the dashboard, not linger at its old value.
func TestCollectAdviceCatalog_ResetClearsRetiredArchetypes(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	q := &mockQuerier{
		beads: []*beadsapi.BeadDetail{
			adviceCatalogBead("gd-capt", 2000, []string{"g0:project:gardenio", "g0:role:gardener"}),
			adviceCatalogBead("gd-crew", 800, []string{"g0:project:gardenio", "g0:role:crew"}),
		},
		// The fleet ran both archetypes: gardenio/gardener and gardenio/crew.
		agentBeadDetails: []*beadsapi.BeadDetail{
			agentBeadDetail("gd-ag-capt", "gardenio", "gardener"),
			agentBeadDetail("gd-ag-crew", "gardenio", "crew"),
		},
	}
	c := New(q, logger, 30*time.Second)

	// Prime the grid synchronously (the scrape-path refresh is async, ka-0000006669).
	c.refreshFleetArchetypes(context.Background())
	c.collectAdviceCatalog(context.Background())
	if n := testutil.CollectAndCount(c.adviceCatalogBytes); n != 2 {
		t.Fatalf("first collect: %d series, want 2 (gardener role + crew role)", n)
	}

	// The fleet stops running the crew archetype; its series must vanish. Expire
	// the fleet-archetype cache and re-prime so the collect re-derives from the
	// shrunk agent fixture instead of serving the 2-archetype grid it just cached.
	q.agentBeadDetails = q.agentBeadDetails[:1]
	c.fleetArchetypesAt = time.Time{}
	c.refreshFleetArchetypes(context.Background())
	c.collectAdviceCatalog(context.Background())
	if n := testutil.CollectAndCount(c.adviceCatalogBytes); n != 1 {
		t.Errorf("after shrink: %d series, want 1", n)
	}
}

// TestCollectAdviceCatalog_Error asserts a daemon failure increments the
// scrape-error counter and leaves previous series intact (no partial reset).
func TestCollectAdviceCatalog_Error(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	q := &mockQuerier{
		beads: []*beadsapi.BeadDetail{
			adviceCatalogBead("gd-capt", 2000, []string{"g0:project:gardenio", "g0:role:gardener"}),
		},
		agentBeadDetails: []*beadsapi.BeadDetail{
			agentBeadDetail("gd-ag-capt", "gardenio", "gardener"),
		},
	}
	c := New(q, logger, 30*time.Second)
	// Prime the grid synchronously (the scrape-path refresh is async) so the
	// first collect publishes the gardener series this test expects to survive.
	c.refreshFleetArchetypes(context.Background())
	c.collectAdviceCatalog(context.Background())

	before := testutil.ToFloat64(c.scrapeErrors)
	q.beadErr = errFake
	c.collectAdviceCatalog(context.Background())
	if got := testutil.ToFloat64(c.scrapeErrors); got != before+1 {
		t.Errorf("scrapeErrors = %v, want %v", got, before+1)
	}
	if n := testutil.CollectAndCount(c.adviceCatalogBytes); n != 1 {
		t.Errorf("error collect wiped previous series: %d, want 1", n)
	}
}

// gatedQuerier wraps a mockQuerier and blocks every agent-window (archetype)
// query on a release channel, so a test can prove the archetype walk runs OFF
// the scrape goroutine: if deriveFleetArchetypes blocked on it, the test would
// deadlock. Non-agent queries pass through immediately.
type gatedQuerier struct {
	*mockQuerier
	release  chan struct{}
	mu       sync.Mutex
	agentReq int
}

func (g *gatedQuerier) ListBeadsFiltered(ctx context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	if slices.Contains(q.Types, "agent") {
		g.mu.Lock()
		g.agentReq++
		g.mu.Unlock()
		<-g.release // block until the test releases the walk
	}
	return g.mockQuerier.ListBeadsFiltered(ctx, q)
}

func (g *gatedQuerier) agentReqCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.agentReq
}

// TestDeriveFleetArchetypes_ColdScrapeDoesNotBlock is the step-3 regression for
// ka-0000006669: the FIRST scrape after a restart is a guaranteed cache miss, and
// the archetype walk must run asynchronously so that cold scrape returns
// immediately instead of doing the agent-bead fetch inline (the inline fetch is
// what OOMKilled the 256Mi controller). The gated querier blocks the walk
// forever; if deriveFleetArchetypes waited on it this test would hang.
func TestDeriveFleetArchetypes_ColdScrapeDoesNotBlock(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	g := &gatedQuerier{
		mockQuerier: &mockQuerier{
			agentBeadDetails: []*beadsapi.BeadDetail{agentBeadDetail("gd-ag", "gardenio", "gardener")},
		},
		release: make(chan struct{}),
	}
	c := New(g, logger, 30*time.Second)

	done := make(chan struct{}, 1)
	go func() {
		// Cold scrape: the returned grid is nil (not primed yet) — what matters
		// is that this returns at all while the walk is blocked.
		c.deriveFleetArchetypes(context.Background())
		done <- struct{}{}
	}()

	select {
	case <-done:
		// Cold scrape returned without blocking on the (still-blocked) walk.
	case <-time.After(2 * time.Second):
		close(g.release) // unblock the goroutine so the test can exit
		t.Fatal("deriveFleetArchetypes blocked on the archetype walk — cold scrape must return immediately (ka-0000006669)")
	}

	// A background walk must have been kicked off (the single-flight guard). It
	// runs on its own goroutine, so poll briefly for it to reach the gate.
	walkStarted := false
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
		if g.agentReqCount() > 0 {
			walkStarted = true
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	close(g.release) // let the background walk complete regardless
	if !walkStarted {
		t.Fatal("no background archetype walk was started on cache miss")
	}
}

// TestDeriveFleetArchetypes_AsyncPrimePopulatesCache asserts the background walk
// actually populates the grid: the first (cold) scrape returns nil, and once the
// async refresh completes a subsequent scrape sees the derived archetypes.
func TestDeriveFleetArchetypes_AsyncPrimePopulatesCache(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	q := &mockQuerier{
		agentBeadDetails: []*beadsapi.BeadDetail{
			agentBeadDetail("gd-ag1", "gardenio", "gardener"),
			agentBeadDetail("gd-ag2", "monorepo", "crew"),
		},
	}
	c := New(q, logger, 30*time.Second)

	// Cold miss: returns nil immediately, kicks off the async walk.
	if got := c.deriveFleetArchetypes(context.Background()); got != nil {
		t.Fatalf("cold scrape = %v, want nil (grid not primed yet)", got)
	}

	// Wait for the background refresh to populate the cache.
	var got []advice.Archetype
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
		got = c.deriveFleetArchetypes(context.Background())
		if len(got) == 2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if len(got) != 2 {
		t.Fatalf("after async prime: %v, want 2 archetypes (gardenio/gardener, monorepo/crew)", got)
	}
}

// corpusCountQuerier answers the two count-only churn queries collectAdviceCorpus
// issues (created-in-window and closed-in-window) with fixed Totals, keyed on
// whether the query carries a ClosedAfter filter. The open-pool snapshot is
// passed to collectAdviceCorpus directly, so this querier only needs to serve
// the count paths.
type corpusCountQuerier struct {
	mockQuerier
	createdTotal int
	closedTotal  int
}

func (q *corpusCountQuerier) ListBeadsFiltered(_ context.Context, query beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	if !query.ClosedAfter.IsZero() {
		return &beadsapi.ListBeadsResult{Total: q.closedTotal}, nil
	}
	return &beadsapi.ListBeadsResult{Total: q.createdTotal}, nil
}

func TestCollectAdviceCorpus_PublishesPoolAndChurn(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	c := New(&corpusCountQuerier{createdTotal: 110, closedTotal: 40}, logger, 30*time.Second)

	expired := adviceCatalogBead("gd-exp", 100, nil)
	expired.Fields = map[string]string{"expires_at": "2000-01-01T00:00:00Z"}
	fixture := adviceCatalogBead("gd-fix", 100, []string{"fixture:e2e:advice"})
	hippo := adviceCatalogBead("gd-hip", 100, []string{"source:hippo-memory"})

	openAdvice := []*beadsapi.BeadDetail{
		adviceCatalogBead("gd-new", 100, nil),
		adviceCatalogBead("gd-old", 100, nil),
		expired, fixture, hippo,
	}
	// gd-new supersedes gd-old — both real and open, so gd-old is compaction backlog.
	openAdvice[0].Description = "Better rule. supersedes(gd-old)"

	c.collectAdviceCorpus(context.Background(), openAdvice)

	// Real open pool = gd-new + gd-old (expired/fixture/hippo excluded).
	if got := testutil.ToFloat64(c.adviceOpenTotal); got != 2 {
		t.Errorf("advice_open_total = %v, want 2", got)
	}
	if got := testutil.ToFloat64(c.adviceSupersededOpen); got != 1 {
		t.Errorf("advice_superseded_open = %v, want 1", got)
	}
	if got := testutil.ToFloat64(c.adviceCreated30d); got != 110 {
		t.Errorf("advice_created_30d = %v, want 110", got)
	}
	if got := testutil.ToFloat64(c.adviceClosed30d); got != 40 {
		t.Errorf("advice_closed_30d = %v, want 40", got)
	}
}
