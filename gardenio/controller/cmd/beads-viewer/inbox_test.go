package main

import (
	"context"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"gardenio/controller/cmd/beads-viewer/viewer"
	"gardenio/controller/internal/beadsapi"
)

// inboxFakeLookup is the AliasLookup used by the inbox regression tests.
// It maps an OIDC subject (email) to (operator_id, kind, found) triples so a
// single fake covers humans, sentinels, and unprovisioned viewers.
type inboxFakeLookup struct {
	rows map[string]struct {
		id    int64
		kind  string
		found bool
	}
}

func (f *inboxFakeLookup) LookupOperatorByAlias(_ context.Context, _, subject string) (*beadsapi.LookupOperatorByAliasResponse, error) {
	if f == nil || f.rows == nil {
		return &beadsapi.LookupOperatorByAliasResponse{Found: false}, nil
	}
	row, ok := f.rows[subject]
	if !ok {
		return &beadsapi.LookupOperatorByAliasResponse{Found: false}, nil
	}
	return &beadsapi.LookupOperatorByAliasResponse{
		OperatorID:   row.id,
		OperatorKind: row.kind,
		Found:        row.found,
	}, nil
}

// inboxFake builds a fakeDaemon pre-loaded with a mix of intake beads operated
// by humans (id 100, 101) and a sentinel system operator (id 9999). The fake's
// ListBeadsFiltered honors `q.OperatedByID` so tests can verify the predicate
// is wired through end-to-end. The §1.2.1 query (category='intake' AND
// operated_by_id = N) is applied client-side here because the fake has no SQL
// engine — but the assertion that the *handler* sets q.OperatedByID is what
// guards the regression.
func inboxFake() *fakeDaemon {
	humanID := int64(100)
	otherHumanID := int64(101)
	sentinelID := int64(9999)
	mkBead := func(id, title string, opID *int64, status string) *beadsapi.BeadDetail {
		return &beadsapi.BeadDetail{
			ID:                id,
			Title:             title,
			Type:              "task",
			Status:            status,
			Category:          "intake",
			OperatedByID:      opID,
			OperatedByDisplay: "test",
			Description:       "a sample intake",
			CreatedAt:         time.Now().Add(-1 * time.Hour),
			UpdatedAt:         time.Now(),
		}
	}
	beads := map[string]*beadsapi.BeadDetail{
		"gd-h1": mkBead("gd-h1", "human alice intake A", &humanID, "open"),
		"gd-h2": mkBead("gd-h2", "human alice intake B", &humanID, "in_progress"),
		"gd-h3": mkBead("gd-h3", "human bob intake", &otherHumanID, "open"),
		"gd-s1": mkBead("gd-s1", "sentinel-operated intake", &sentinelID, "open"),
	}
	f := &fakeDaemon{
		beads: beads,
		deps:  map[string][]beadsapi.Dependency{},
	}
	return f
}

// inboxFilteringDaemon wraps fakeDaemon to enforce the §1.2.1 predicate
// client-side: when q.Categories=[intake] and q.OperatedByID is set, only
// beads with matching Category and OperatedByID are returned. This is
// load-bearing for G3.j (sentinel exclusion happens because the handler
// sends q.OperatedByID = viewer's id, never the sentinel's).
type inboxFilteringDaemon struct {
	*fakeDaemon
}

func (d *inboxFilteringDaemon) ListBeadsFiltered(ctx context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	// Track the call on the underlying fake. handleInbox now fans its
	// branches out concurrently, so this recorder needs the same mutex
	// as the base fake's ListBeadsFiltered.
	qq := q
	d.fakeDaemon.listMu.Lock()
	d.fakeDaemon.listLastQ = &qq
	d.fakeDaemon.listAllQ = append(d.fakeDaemon.listAllQ, qq)
	d.fakeDaemon.listMu.Unlock()

	wantCategory := ""
	if len(q.Categories) > 0 {
		wantCategory = q.Categories[0]
	}
	out := make([]*beadsapi.BeadDetail, 0)
	for _, b := range d.fakeDaemon.beads {
		if b == nil {
			continue
		}
		if wantCategory != "" && b.Category != wantCategory {
			continue
		}
		if len(q.Types) > 0 && !slices.Contains(q.Types, b.Type) {
			continue
		}
		if q.OperatedByID != 0 {
			if b.OperatedByID == nil || *b.OperatedByID != q.OperatedByID {
				continue
			}
		}
		if len(q.Statuses) > 0 && !slices.Contains(q.Statuses, b.Status) {
			continue
		}
		if len(q.Types) > 0 && !slices.Contains(q.Types, b.Type) {
			continue
		}
		if len(q.Labels) > 0 {
			matchAll := true
			for _, want := range q.Labels {
				if !slices.Contains(b.Labels, want) {
					matchAll = false
					break
				}
			}
			if !matchAll {
				continue
			}
		}
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return &beadsapi.ListBeadsResult{Beads: out, Total: len(out)}, nil
}

// inboxTestEmailKey carries the viewer email across the test boundary. The
// resolver's default extractor reads from oidcauth's private context key,
// which we cannot set from outside that package — so the test injects a
// custom EmailExtractor that pulls from this key, and ServeHTTP sees the
// email after we wrap the request context.
type inboxTestEmailCtxKey struct{}

func mkInboxServer(t *testing.T, daemon beadAPI, lookup viewer.AliasLookup) *Server {
	t.Helper()
	srv := NewServer(daemon, slog.Default(), "", "", "", nil)
	// The production /inbox route 308s to /u/me?view=dashboard (M3 fold,
	// ka-0000003587). Inbox-renderer regression tests still exercise the
	// legacy handleInbox renderer (which now also backs
	// /u/me?view=constellation), so flip the legacy-route flag so the test
	// mux maps /inbox directly to that handler.
	srv.inboxLegacyRoute = true
	if lookup != nil {
		srv.SetResolver(viewer.NewResolver(lookup, viewer.WithEmailExtractor(func(ctx context.Context) string {
			v, _ := ctx.Value(inboxTestEmailCtxKey{}).(string)
			return v
		})))
	}
	return srv
}

// inboxRequest builds a request with both the X-Auth-Request-Email header
// (read by handler-side EmailFromRequest) and a context value (read by the
// test's custom resolver extractor). Both must be set for the inbox surface
// to render with a viewer identity.
func inboxRequest(method, target, email string) *http.Request {
	req := httptest.NewRequest(method, target, nil)
	req.Header.Set("X-Auth-Request-Email", email)
	ctx := context.WithValue(req.Context(), inboxTestEmailCtxKey{}, email)
	return req.WithContext(ctx)
}

func mkInboxMux(t *testing.T, daemon beadAPI, lookup viewer.AliasLookup) http.Handler {
	t.Helper()
	srv := mkInboxServer(t, daemon, lookup)
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)
	return edgeAuth(mux)
}

// TestInbox_SentinelOperatedIntakeNotInInbox is the G3.j regression test.
// A sentinel-kind operator (kind ∈ {system, fleet}) must never have its
// intakes appear in any human viewer's inbox. Two failure modes are covered:
//
//   - Bead-axis: the §1.2.1 predicate `operated_by_id = :viewer_operator_id`
//     filters out beads whose operated_by_id points at a sentinel id when
//     the viewer is a human.
//   - Viewer-axis: a viewer whose own alias points at a sentinel kind must
//     get the Mode A blocking banner and a fully-empty row list — the
//     resolver's sentinel-safety contract refuses to let a sentinel scope
//     itself.
//
// Both axes guard the same incident class: a sentinel never shadows a real
// operator's inbox.
func TestInbox_SentinelOperatedIntakeNotInInbox(t *testing.T) {
	lookup := &inboxFakeLookup{
		rows: map[string]struct {
			id    int64
			kind  string
			found bool
		}{
			"alice@example.com":    {id: 100, kind: "human", found: true},
			"sentinel@example.com": {id: 9999, kind: "system", found: true},
		},
	}
	daemon := &inboxFilteringDaemon{fakeDaemon: inboxFake()}

	t.Run("human viewer sees only their own intakes", func(t *testing.T) {
		mux := mkInboxMux(t, daemon, lookup)
		req := inboxRequest(http.MethodGet, "/inbox", "alice@example.com")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
		}
		body := w.Body.String()
		if !strings.Contains(body, "gd-h1") || !strings.Contains(body, "gd-h2") {
			t.Errorf("inbox missing alice's intakes (gd-h1/gd-h2)\n%s", body)
		}
		if strings.Contains(body, "gd-h3") {
			t.Errorf("inbox leaked bob's intake (gd-h3) to alice")
		}
		if strings.Contains(body, "gd-s1") {
			t.Errorf("REGRESSION G3.j: sentinel-operated intake (gd-s1) appeared in alice's inbox\n%s", body)
		}

		// Verify the handler issued the §1.2.1 predicate. The handler may
		// fan out into multiple ListBeadsFiltered calls (intake list +
		// orphan probe + adjacent rows); we only require that *some* call
		// carries Categories=[intake] AND OperatedByID=alice's id.
		var sawIntakePredicate bool
		for _, q := range daemon.fakeDaemon.listAllQ {
			if len(q.Categories) == 1 && q.Categories[0] == "intake" && q.OperatedByID == 100 {
				sawIntakePredicate = true
				break
			}
		}
		if !sawIntakePredicate {
			t.Errorf("expected at least one ListBeadsFiltered call with Categories=[intake] AND OperatedByID=100; got %d calls: %+v",
				len(daemon.fakeDaemon.listAllQ), daemon.fakeDaemon.listAllQ)
		}
	})

	t.Run("sentinel viewer gets Mode A banner and zero rows", func(t *testing.T) {
		// Reset the call log so this assertion is independent.
		daemon.fakeDaemon.listAllQ = nil
		daemon.fakeDaemon.listLastQ = nil

		mux := mkInboxMux(t, daemon, lookup)
		req := inboxRequest(http.MethodGet, "/inbox", "sentinel@example.com")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
		}
		body := w.Body.String()
		if !strings.Contains(body, "No operators row found") {
			t.Errorf("expected Mode A banner in body for sentinel viewer\n%s", body)
		}
		// Mode A must short-circuit BEFORE the inbox listing query runs —
		// otherwise a sentinel id would scope the predicate.
		for _, q := range daemon.fakeDaemon.listAllQ {
			if len(q.Categories) > 0 && q.Categories[0] == "intake" {
				t.Errorf("REGRESSION G3.j: sentinel viewer triggered intake listing (operated_by_id=%d)", q.OperatedByID)
			}
		}
	})
}

// inboxRoundTripCounter wraps inboxFilteringDaemon and ticks a total on EVERY
// read method the /inbox render path can invoke — including the per-bead fan-out
// risks (GetBead, GetDependencies, ListFollowedBeadIDs). Ticking on all of them
// is what makes an O(N) regression detectable: if a future change re-introduces
// a per-bead hydration loop in the index path, its round-trip total starts
// scaling with the fixture size and TestInbox_PerfBudget's invariance assertion
// reds. Concurrent because fetchInboxStreamSources fans its four kind-queries
// out under an errgroup.
type inboxRoundTripCounter struct {
	*inboxFilteringDaemon
	mu        sync.Mutex
	total     int
	perMethod map[string]int
}

func newInboxRoundTripCounter(inner *inboxFilteringDaemon) *inboxRoundTripCounter {
	return &inboxRoundTripCounter{inboxFilteringDaemon: inner, perMethod: map[string]int{}}
}

func (c *inboxRoundTripCounter) tick(method string) {
	c.mu.Lock()
	c.total++
	c.perMethod[method]++
	c.mu.Unlock()
}

func (c *inboxRoundTripCounter) roundTrips() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.total
}

func (c *inboxRoundTripCounter) breakdown() map[string]int {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]int, len(c.perMethod))
	maps.Copy(out, c.perMethod)
	return out
}

func (c *inboxRoundTripCounter) ListBeadsFiltered(ctx context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	c.tick("ListBeadsFiltered")
	return c.inboxFilteringDaemon.ListBeadsFiltered(ctx, q)
}

func (c *inboxRoundTripCounter) GetBead(ctx context.Context, id string) (*beadsapi.BeadDetail, error) {
	c.tick("GetBead")
	return c.inboxFilteringDaemon.GetBead(ctx, id)
}

func (c *inboxRoundTripCounter) GetDependencies(ctx context.Context, id string) ([]beadsapi.Dependency, error) {
	c.tick("GetDependencies")
	return c.inboxFilteringDaemon.GetDependencies(ctx, id)
}

func (c *inboxRoundTripCounter) GetReverseDependencies(ctx context.Context, id string) ([]beadsapi.Dependency, error) {
	c.tick("GetReverseDependencies")
	return c.inboxFilteringDaemon.GetReverseDependencies(ctx, id)
}

func (c *inboxRoundTripCounter) BatchGetBeads(ctx context.Context, ids []string) (map[string]*beadsapi.BeadDetail, error) {
	c.tick("BatchGetBeads")
	return c.inboxFilteringDaemon.BatchGetBeads(ctx, ids)
}

func (c *inboxRoundTripCounter) BatchGetDependencies(ctx context.Context, ids []string, direction string) (map[string][]beadsapi.Dependency, error) {
	c.tick("BatchGetDependencies")
	return c.inboxFilteringDaemon.BatchGetDependencies(ctx, ids, direction)
}

func (c *inboxRoundTripCounter) FindAgentBeadsByNames(ctx context.Context, names, statuses []string) (map[string]*beadsapi.BeadDetail, error) {
	c.tick("FindAgentBeadsByNames")
	return c.inboxFilteringDaemon.FindAgentBeadsByNames(ctx, names, statuses)
}

func (c *inboxRoundTripCounter) ListFollowedBeadIDs(ctx context.Context, operatorID int64, limit int) ([]string, error) {
	c.tick("ListFollowedBeadIDs")
	return c.inboxFilteringDaemon.ListFollowedBeadIDs(ctx, operatorID, limit)
}

// seedInboxIntakeFake builds a filtering daemon serving n human-operated intake
// beads for operator id 100 (the "alice" viewer). Used to scale the fixture so
// the round-trip count can be asserted N-invariant.
func seedInboxIntakeFake(n int) *inboxFilteringDaemon {
	const humanID = int64(100)
	beads := make(map[string]*beadsapi.BeadDetail, n)
	for i := range n {
		id := "gd-intake-" + itoa(i)
		opID := humanID
		beads[id] = &beadsapi.BeadDetail{
			ID:                id,
			Title:             "human alice intake " + itoa(i),
			Type:              "task",
			Status:            "open",
			Category:          "intake",
			OperatedByID:      &opID,
			OperatedByDisplay: "test",
			Description:       "a sample intake",
			CreatedAt:         time.Now().Add(-1 * time.Hour),
			UpdatedAt:         time.Now(),
		}
	}
	return &inboxFilteringDaemon{fakeDaemon: &fakeDaemon{
		beads: beads,
		deps:  map[string][]beadsapi.Dependency{},
	}}
}

// inboxRenderRoundTrips renders /inbox once against an n-bead intake fixture and
// returns the daemon round-trip total (and per-method breakdown) it took.
func inboxRenderRoundTrips(t *testing.T, n int) (int, map[string]int) {
	t.Helper()
	lookup := &inboxFakeLookup{
		rows: map[string]struct {
			id    int64
			kind  string
			found bool
		}{
			"alice@example.com": {id: 100, kind: "human", found: true},
		},
	}
	counter := newInboxRoundTripCounter(seedInboxIntakeFake(n))
	mux := mkInboxMux(t, counter, lookup)

	req := inboxRequest(http.MethodGet, "/inbox", "alice@example.com")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("n=%d: expected 200, got %d", n, w.Code)
	}
	return counter.roundTrips(), counter.breakdown()
}

// TestInbox_PerfBudget is the G3.k regression test. Its plan-of-record intent
// (§7.5, V-R6 latency budget) is that an /inbox cold-load stays fast, and the
// concrete regression it exists to catch is an O(N) per-bead daemon roundtrip
// creeping into the index path (the docstring's original words).
//
// It used to assert that intent with a WALL-CLOCK percentile budget (p50 ≤ 50ms
// over 100 renders of a 4-bead fixture). That was intrinsically flaky: the fake
// daemon is in-memory so the true render cost is microseconds, and the 50ms/270ms
// budgets were really measuring the box's scheduler/GC jitter — under `-race` on
// a loaded box (make test runs `go test -race`, no `-short`, so this test does
// execute there) p50 crossed 50ms purely from CPU contention, with the code
// unchanged (gd-CabIbtunSQ; origin gd-AN27Ilha0B). A 4-bead fixture also cannot
// actually observe O(N) growth — the wall-clock assertion was a flaky proxy for
// a property that is deterministic when measured directly.
//
// So we now assert the underlying property head-on: the number of daemon
// round-trips one /inbox render issues is a small BOUNDED CONSTANT, INVARIANT in
// the intake-bead count. Every read method the render path can touch is counted
// (see inboxRoundTripCounter), so a re-introduced per-bead fan-out makes the
// count scale with N and reds this test — the exact regression the budget was
// guarding against — while scheduler jitter can no longer red it.
func TestInbox_PerfBudget(t *testing.T) {
	if testing.Short() {
		t.Skip("perf budget test skipped under -short")
	}

	// Scale the fixture 100×. If any index-path call is O(beads), the round-trip
	// total at 400 beads diverges from the total at 4.
	const (
		nSmall = 4
		nLarge = 400
	)
	small, smallBreakdown := inboxRenderRoundTrips(t, nSmall)
	large, largeBreakdown := inboxRenderRoundTrips(t, nLarge)

	if small != large {
		t.Errorf("REGRESSION G3.k: /inbox daemon round-trips are NOT bead-count invariant: "+
			"%d for %d beads vs %d for %d beads — an O(N) per-bead roundtrip crept into the index path\n"+
			"  %d-bead breakdown: %v\n  %d-bead breakdown: %v",
			small, nSmall, large, nLarge, nSmall, smallBreakdown, nLarge, largeBreakdown)
	}

	// The absolute budget: a fixed, tiny set of batched calls (the index list +
	// worthy/decision/agent/epic kind-queries + the MR-chip dep prewarm + the
	// followed-ids fetch), never a per-bead fan-out. Measured at 10 on current
	// HEAD; the ceiling leaves headroom for a future batched join without hiding
	// an O(N) regression (which the invariance check above catches regardless).
	const roundTripBudget = 16
	if large > roundTripBudget {
		t.Errorf("REGRESSION G3.k: /inbox render issued %d daemon round-trips, budget %d (breakdown: %v)",
			large, roundTripBudget, largeBreakdown)
	}
	t.Logf("inbox render daemon round-trips: %d (invariant across %d→%d beads; breakdown: %v)",
		large, nSmall, nLarge, largeBreakdown)
}

// TestInbox_AgentsRoster verifies that /inbox renders the operator-scoped
// agents roster: open agents (status=open|in_progress) and recently-closed
// agents (status=closed within inboxAgentsRecentWindow), each filtered to
// the viewer's operator id and rendering the assigned task title.
func TestInbox_AgentsRoster(t *testing.T) {
	humanID := int64(100)
	otherID := int64(101)
	now := time.Now()

	mkAgent := func(id, name, status, taskID, taskTitle string, op *int64, age time.Duration) *beadsapi.BeadDetail {
		return &beadsapi.BeadDetail{
			ID:           id,
			Title:        name,
			Type:         "agent",
			Status:       status,
			OperatedByID: op,
			Fields: map[string]string{
				"agent":      name,
				"role":       "crew",
				"task_id":    taskID,
				"task_title": taskTitle,
				"spawned_by": "gardener-1",
			},
			Labels:    []string{"project:gardenio"},
			UpdatedAt: now.Add(-age),
		}
	}

	// alice's open agent + alice's recent closed agent + alice's stale (>24h) closed agent + bob's open agent.
	beads := map[string]*beadsapi.BeadDetail{
		"gd-h1":      {ID: "gd-h1", Title: "intake", Type: "task", Status: "open", Category: "intake", OperatedByID: &humanID, UpdatedAt: now},
		"gd-agent-1": mkAgent("gd-agent-1", "alice-running", "in_progress", "gd-task-A", "Implement /agents page", &humanID, time.Minute),
		"gd-agent-2": mkAgent("gd-agent-2", "alice-recent", "closed", "gd-task-B", "Fix login bug", &humanID, 2*time.Hour),
		"gd-agent-3": mkAgent("gd-agent-3", "alice-stale", "closed", "gd-task-C", "Old task", &humanID, 48*time.Hour),
		"gd-agent-4": mkAgent("gd-agent-4", "bob-running", "in_progress", "gd-task-D", "Bob's task", &otherID, time.Minute),
	}
	daemon := &inboxFilteringDaemon{fakeDaemon: &fakeDaemon{beads: beads, deps: map[string][]beadsapi.Dependency{}}}

	lookup := &inboxFakeLookup{
		rows: map[string]struct {
			id    int64
			kind  string
			found bool
		}{
			"alice@example.com": {id: humanID, kind: "human", found: true},
		},
	}
	mux := mkInboxMux(t, daemon, lookup)

	req := inboxRequest(http.MethodGet, "/inbox", "alice@example.com")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	if !strings.Contains(body, "alice-running") {
		t.Errorf("inbox missing alice's open agent\n%s", body)
	}
	if !strings.Contains(body, "Implement /agents page") {
		t.Errorf("inbox missing task title for open agent")
	}
	if !strings.Contains(body, "alice-recent") {
		t.Errorf("inbox missing alice's recently-closed agent")
	}
	if !strings.Contains(body, "Fix login bug") {
		t.Errorf("inbox missing task title for recently-closed agent")
	}
	if strings.Contains(body, "alice-stale") {
		t.Errorf("inbox leaked alice's stale (>24h) closed agent")
	}
	if strings.Contains(body, "bob-running") {
		t.Errorf("inbox leaked bob's agent into alice's view")
	}
	if !strings.Contains(body, "gardener-1") {
		t.Errorf("inbox missing spawned_by attribution")
	}

	// Verify the handler queried agents scoped to alice's operator id.
	var sawAgentActive, sawAgentRecent bool
	for _, q := range daemon.fakeDaemon.listAllQ {
		if len(q.Types) == 1 && q.Types[0] == "agent" && q.OperatedByID == humanID {
			if slices.Contains(q.Statuses, "closed") {
				sawAgentRecent = true
			} else {
				sawAgentActive = true
			}
		}
	}
	if !sawAgentActive {
		t.Errorf("expected ListBeadsFiltered call with Types=[agent], OperatedByID=%d, open/in_progress; got %+v", humanID, daemon.fakeDaemon.listAllQ)
	}
	if !sawAgentRecent {
		t.Errorf("expected ListBeadsFiltered call with Types=[agent], OperatedByID=%d, closed; got %+v", humanID, daemon.fakeDaemon.listAllQ)
	}
}

// TestInbox_OpenEpicsSection verifies the unified Gmail-density stream
// (ka-0000000894) interleaves the operator's open and in-progress epics:
// each open epic surfaces as a row in the activity stream; rows link into
// the constellation drill at /inbox/{id}; closed epics and other operators'
// epics must not leak into the stream. The kind chip strip must include the
// "Epics" filter chip; selecting it must show the epic rows only.
func TestInbox_OpenEpicsSection(t *testing.T) {
	humanID := int64(100)
	otherID := int64(101)
	now := time.Now()

	mkEpic := func(id, title, status string, op *int64, priority int) *beadsapi.BeadDetail {
		return &beadsapi.BeadDetail{
			ID:           id,
			Title:        title,
			Type:         "epic",
			Status:       status,
			OperatedByID: op,
			Priority:     priority,
			UpdatedAt:    now,
		}
	}

	beads := map[string]*beadsapi.BeadDetail{
		"gd-i1":   {ID: "gd-i1", Title: "intake", Type: "task", Status: "open", Category: "intake", OperatedByID: &humanID, UpdatedAt: now},
		"gd-e1":   mkEpic("gd-e1", "alice epic A", "open", &humanID, 2),
		"gd-e2":   mkEpic("gd-e2", "alice epic B in progress", "in_progress", &humanID, 1),
		"gd-e-cl": mkEpic("gd-e-cl", "alice epic closed", "closed", &humanID, 1),
		"gd-e-bo": mkEpic("gd-e-bo", "bob epic", "open", &otherID, 1),
	}
	daemon := &inboxFilteringDaemon{fakeDaemon: &fakeDaemon{beads: beads, deps: map[string][]beadsapi.Dependency{}}}
	lookup := &inboxFakeLookup{
		rows: map[string]struct {
			id    int64
			kind  string
			found bool
		}{
			"alice@example.com":  {id: humanID, kind: "human", found: true},
			"newbie@example.com": {id: 200, kind: "human", found: true},
		},
	}

	t.Run("renders alice's open epics with constellation drill links", func(t *testing.T) {
		mux := mkInboxMux(t, daemon, lookup)
		req := inboxRequest(http.MethodGet, "/inbox", "alice@example.com")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
		}
		body := w.Body.String()

		// Kind chip strip must include the Epics filter chip.
		if !strings.Contains(body, `?kind=epic`) {
			t.Errorf("inbox missing Epics kind chip filter\n%s", body)
		}
		if !strings.Contains(body, "alice epic A") || !strings.Contains(body, "alice epic B in progress") {
			t.Errorf("inbox missing alice's open epics in the unified stream\n%s", body)
		}
		if strings.Contains(body, "alice epic closed") {
			t.Errorf("inbox leaked alice's closed epic into the stream")
		}
		if strings.Contains(body, "bob epic") {
			t.Errorf("inbox leaked bob's epic into alice's stream")
		}
		// Each row links into the constellation drill at /inbox/{id}.
		if !strings.Contains(body, `href="/inbox/gd-e1"`) || !strings.Contains(body, `href="/inbox/gd-e2"`) {
			t.Errorf("inbox open-epic rows missing /inbox/{id} drill href\n%s", body)
		}

		// Verify the predicate. The handler should issue Types=[epic],
		// Statuses=[open,in_progress], OperatedByID=alice.
		var sawEpicQuery bool
		for _, q := range daemon.fakeDaemon.listAllQ {
			if len(q.Types) == 1 && q.Types[0] == "epic" && q.OperatedByID == humanID &&
				slices.Contains(q.Statuses, "open") && slices.Contains(q.Statuses, "in_progress") {
				sawEpicQuery = true
				break
			}
		}
		if !sawEpicQuery {
			t.Errorf("expected ListBeadsFiltered call with Types=[epic], OperatedByID=%d, Statuses=[open,in_progress]; got %+v",
				humanID, daemon.fakeDaemon.listAllQ)
		}
	})

	t.Run("empty state for operator with no open epics", func(t *testing.T) {
		mux := mkInboxMux(t, daemon, lookup)
		req := inboxRequest(http.MethodGet, "/inbox", "newbie@example.com")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
		}
		body := w.Body.String()
		// Kind chip strip is always rendered; the Epics chip count must be 0.
		if !strings.Contains(body, `?kind=epic`) {
			t.Errorf("inbox missing Epics kind chip filter for empty operator\n%s", body)
		}
		if !strings.Contains(body, "Inbox zero. Nothing to triage.") {
			t.Errorf("inbox missing empty-state copy for operator with no rows\n%s", body)
		}
	})
}

// TestInbox_UnionsInboxWorthyWithIntake verifies that beads carrying the
// `flag:inbox-worthy` label appear in the inbox even when their category is
// not `intake`. This is the core ka-0000007385 acceptance: an operator can
// hand-promote a non-intake bead onto their inbox without retagging.
func TestInbox_UnionsInboxWorthyWithIntake(t *testing.T) {
	humanID := int64(100)
	beads := map[string]*beadsapi.BeadDetail{
		"gd-intake": {
			ID: "gd-intake", Title: "real intake", Type: "task", Status: "open",
			Category: "intake", OperatedByID: &humanID, UpdatedAt: time.Now(),
		},
		"gd-worthy": {
			ID: "gd-worthy", Title: "nominated action", Type: "task", Status: "open",
			Category: "action", OperatedByID: &humanID, UpdatedAt: time.Now(),
			Labels: []string{labelInboxWorthy},
		},
		"gd-other": {
			ID: "gd-other", Title: "ignored action", Type: "task", Status: "open",
			Category: "action", OperatedByID: &humanID, UpdatedAt: time.Now(),
		},
	}
	daemon := &inboxFilteringDaemon{fakeDaemon: &fakeDaemon{beads: beads, deps: map[string][]beadsapi.Dependency{}}}
	lookup := &inboxFakeLookup{rows: map[string]struct {
		id    int64
		kind  string
		found bool
	}{
		"alice@example.com": {id: humanID, kind: "human", found: true},
	}}
	mux := mkInboxMux(t, daemon, lookup)

	req := inboxRequest(http.MethodGet, "/inbox", "alice@example.com")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "gd-intake") {
		t.Errorf("inbox missing intake bead\n%s", body)
	}
	if !strings.Contains(body, "gd-worthy") {
		t.Errorf("inbox missing worthy-flagged bead (REGRESSION: union with category=intake broken)\n%s", body)
	}
	if strings.Contains(body, "gd-other") {
		t.Errorf("inbox leaked unrelated action bead gd-other\n%s", body)
	}

	var sawWorthyQuery bool
	for _, q := range daemon.fakeDaemon.listAllQ {
		if slices.Contains(q.Labels, labelInboxWorthy) && q.OperatedByID == humanID {
			sawWorthyQuery = true
			break
		}
	}
	if !sawWorthyQuery {
		t.Errorf("expected ListBeadsFiltered call with Labels=[%s] AND OperatedByID=%d; got %+v",
			labelInboxWorthy, humanID, daemon.fakeDaemon.listAllQ)
	}
}

// TestInbox_PinnedAppearsFirst verifies that a bead carrying
// `flag:inbox-pinned` sorts before non-pinned beads regardless of priority.
func TestInbox_PinnedAppearsFirst(t *testing.T) {
	humanID := int64(100)
	beads := map[string]*beadsapi.BeadDetail{
		"gd-aaa": {
			ID: "gd-aaa", Title: "high-priority normal", Type: "task", Status: "open",
			Category: "intake", OperatedByID: &humanID, Priority: 0, UpdatedAt: time.Now(),
		},
		"gd-zzz": {
			ID: "gd-zzz", Title: "low-priority pinned", Type: "task", Status: "open",
			Category: "intake", OperatedByID: &humanID, Priority: 5, UpdatedAt: time.Now(),
			Labels: []string{labelInboxPinned},
		},
	}
	daemon := &inboxFilteringDaemon{fakeDaemon: &fakeDaemon{beads: beads, deps: map[string][]beadsapi.Dependency{}}}
	lookup := &inboxFakeLookup{rows: map[string]struct {
		id    int64
		kind  string
		found bool
	}{
		"alice@example.com": {id: humanID, kind: "human", found: true},
	}}
	mux := mkInboxMux(t, daemon, lookup)

	req := inboxRequest(http.MethodGet, "/inbox", "alice@example.com")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	pinIdx := strings.Index(body, "gd-zzz")
	normalIdx := strings.Index(body, "gd-aaa")
	if pinIdx < 0 || normalIdx < 0 {
		t.Fatalf("expected both rows in body; pin=%d normal=%d\n%s", pinIdx, normalIdx, body)
	}
	if pinIdx > normalIdx {
		t.Errorf("REGRESSION: pinned bead gd-zzz did not sort before gd-aaa (pin@%d > normal@%d)", pinIdx, normalIdx)
	}
}

// TestInbox_ToggleInboxWorthy_AddsAndRemovesLabel verifies the POST endpoint
// flips the flag:inbox-worthy label idempotently — first call adds, second
// call removes.
func TestInbox_ToggleInboxWorthy_AddsAndRemovesLabel(t *testing.T) {
	humanID := int64(100)
	beads := map[string]*beadsapi.BeadDetail{
		"gd-flip": {
			ID: "gd-flip", Title: "to be flipped", Type: "task", Status: "open",
			Category: "action", OperatedByID: &humanID, UpdatedAt: time.Now(),
		},
	}
	daemon := &inboxFilteringDaemon{fakeDaemon: &fakeDaemon{beads: beads, deps: map[string][]beadsapi.Dependency{}}}
	lookup := &inboxFakeLookup{rows: map[string]struct {
		id    int64
		kind  string
		found bool
	}{
		"alice@example.com": {id: humanID, kind: "human", found: true},
	}}
	mux := mkInboxMux(t, daemon, lookup)

	// First toggle — adds the label.
	req := inboxRequest(http.MethodPost, "/b/gd-flip/inbox-worthy/toggle", "alice@example.com")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("first toggle: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if !slices.Contains(beads["gd-flip"].Labels, labelInboxWorthy) {
		t.Errorf("first toggle: expected label %s on bead, got %v", labelInboxWorthy, beads["gd-flip"].Labels)
	}
	if daemon.fakeDaemon.addLabelCalls != 1 {
		t.Errorf("first toggle: expected 1 AddLabel call, got %d", daemon.fakeDaemon.addLabelCalls)
	}

	// Second toggle — removes it.
	req = inboxRequest(http.MethodPost, "/b/gd-flip/inbox-worthy/toggle", "alice@example.com")
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("second toggle: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if slices.Contains(beads["gd-flip"].Labels, labelInboxWorthy) {
		t.Errorf("second toggle: expected label removed, got %v", beads["gd-flip"].Labels)
	}
	if daemon.fakeDaemon.removeLabelCalls != 1 {
		t.Errorf("second toggle: expected 1 RemoveLabel call, got %d", daemon.fakeDaemon.removeLabelCalls)
	}
}

// pinningDaemon persists profile_prefs writes onto a seeded user bead so the
// pin-toggle round-trip test can observe state across requests. It layers a
// mutating UpdateBeadFieldsTyped over inboxFilteringDaemon's ListBeadsFiltered
// (which findUserBeadByEmail relies on to resolve the viewer's user bead).
type pinningDaemon struct {
	*inboxFilteringDaemon
	userBead *beadsapi.BeadDetail
}

func (d *pinningDaemon) UpdateBeadFieldsTyped(_ context.Context, id string, fields map[string]any) error {
	if v, ok := fields["profile_prefs"].(string); ok && d.userBead != nil && id == d.userBead.ID {
		if d.userBead.Fields == nil {
			d.userBead.Fields = map[string]string{}
		}
		d.userBead.Fields["profile_prefs"] = v
	}
	return nil
}

// TestInbox_TogglePin_AddsAndRemovesFromViewerPinnedBeadIDs verifies the Pin
// button drives the viewer's profile_prefs.PinnedBeadIDs (which surfaces the
// bead in their Home #pinned section) rather than the flag:inbox-pinned label
// on the bead. First POST pins (adds the id + button flips to "Unpin"); the
// second unpins (removes the id + button reverts to "Pin"). The action bead
// never gains the inbox-pinned label — that rail-sort concern is decoupled.
func TestInbox_TogglePin_AddsAndRemovesFromViewerPinnedBeadIDs(t *testing.T) {
	humanID := int64(100)
	userBead := &beadsapi.BeadDetail{
		ID: "gd-user-alice", Type: "user", Kind: "config", Status: "open",
		Fields: map[string]string{"email": "alice@example.com"},
	}
	beads := map[string]*beadsapi.BeadDetail{
		"gd-pin": {
			ID: "gd-pin", Title: "to be pinned", Type: "task", Status: "open",
			Category: "intake", OperatedByID: &humanID, UpdatedAt: time.Now(),
		},
		"gd-user-alice": userBead,
	}
	daemon := &pinningDaemon{
		inboxFilteringDaemon: &inboxFilteringDaemon{fakeDaemon: &fakeDaemon{beads: beads, deps: map[string][]beadsapi.Dependency{}}},
		userBead:             userBead,
	}
	lookup := &inboxFakeLookup{rows: map[string]struct {
		id    int64
		kind  string
		found bool
	}{
		"alice@example.com": {id: humanID, kind: "human", found: true},
	}}
	mux := mkInboxMux(t, daemon, lookup)

	req := inboxRequest(http.MethodPost, "/b/gd-pin/inbox-pin/toggle", "alice@example.com")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("first pin: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if got := parseProfilePref(userBead.Fields).PinnedBeadIDs; !slices.Contains(got, "gd-pin") {
		t.Errorf("first pin: expected gd-pin in PinnedBeadIDs, got %v", got)
	}
	if !strings.Contains(w.Body.String(), ">Unpin<") {
		t.Errorf("first pin: expected button to render 'Unpin', body=%s", w.Body.String())
	}
	// The action bead must NOT gain the inbox-pinned label — that concern is
	// decoupled from the Home-page pin.
	if slices.Contains(beads["gd-pin"].Labels, labelInboxPinned) {
		t.Errorf("first pin: action bead unexpectedly carries %s: %v", labelInboxPinned, beads["gd-pin"].Labels)
	}

	req = inboxRequest(http.MethodPost, "/b/gd-pin/inbox-pin/toggle", "alice@example.com")
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("second pin: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if got := parseProfilePref(userBead.Fields).PinnedBeadIDs; slices.Contains(got, "gd-pin") {
		t.Errorf("second pin: expected gd-pin removed from PinnedBeadIDs, got %v", got)
	}
	if !strings.Contains(w.Body.String(), ">Pin<") {
		t.Errorf("second pin: expected button to render 'Pin', body=%s", w.Body.String())
	}
}

// TestInbox_ForwardedUnion is the ka-0000007204 regression test. The inbox
// query is the OR of:
//
//   - operated_by_id = :viewer (the existing intake-scan path)
//   - bead_followers row (operator_id = :viewer) — the forwarded path
//
// Both paths must surface beads on /inbox; the forwarded path must land at
// the top of the rail in follow created_at DESC (most-recently-forwarded
// first). The fakeDaemon returns followed IDs in the daemon's contracted
// order (DESC) — the handler's job is just to honor that order during sort.
//
// We also verify that a bead the operator both owns AND follows is not
// double-counted: a single row with Forwarded=true is rendered.
func TestInbox_ForwardedUnion(t *testing.T) {
	humanID := int64(100)
	otherID := int64(101)
	now := time.Now()

	mkBead := func(id, title string, opID *int64) *beadsapi.BeadDetail {
		return &beadsapi.BeadDetail{
			ID:                id,
			Title:             title,
			Type:              "task",
			Status:            "open",
			Category:          "intake",
			OperatedByID:      opID,
			OperatedByDisplay: "test",
			CreatedAt:         now.Add(-1 * time.Hour),
			UpdatedAt:         now,
		}
	}

	beads := map[string]*beadsapi.BeadDetail{
		// Alice's own intake — surfaces via the intake-scan path.
		"gd-mine": mkBead("gd-mine", "alice's intake", &humanID),
		// Bob's intakes — forwarded to alice (NOT scoped by operated_by_id).
		"gd-fwd-recent": mkBead("gd-fwd-recent", "freshly forwarded", &otherID),
		"gd-fwd-mid":    mkBead("gd-fwd-mid", "mid forwarded", &otherID),
		"gd-fwd-old":    mkBead("gd-fwd-old", "stale forwarded", &otherID),
		// Bob's intake that alice does NOT follow — must not appear.
		"gd-bob-private": mkBead("gd-bob-private", "bob private", &otherID),
		// Edge case: alice both owns AND follows this bead — must dedupe.
		"gd-dual": mkBead("gd-dual", "owned-and-followed", &humanID),
	}

	daemon := &inboxFilteringDaemon{
		fakeDaemon: &fakeDaemon{
			beads: beads,
			deps:  map[string][]beadsapi.Dependency{},
			// Daemon contract: IDs ordered by follow created_at DESC.
			followedBeadIDsByOp: map[int64][]string{
				humanID: {"gd-fwd-recent", "gd-fwd-mid", "gd-fwd-old", "gd-dual"},
			},
		},
	}

	lookup := &inboxFakeLookup{
		rows: map[string]struct {
			id    int64
			kind  string
			found bool
		}{
			"alice@example.com": {id: humanID, kind: "human", found: true},
		},
	}

	mux := mkInboxMux(t, daemon, lookup)
	req := inboxRequest(http.MethodGet, "/inbox", "alice@example.com")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	// Forwarded beads must surface — that is the whole point of the UNION.
	for _, id := range []string{"gd-fwd-recent", "gd-fwd-mid", "gd-fwd-old"} {
		if !strings.Contains(body, id) {
			t.Errorf("forwarded bead %s missing from inbox\n%s", id, body)
		}
	}
	// Intake-scan path is still functional.
	if !strings.Contains(body, "gd-mine") {
		t.Errorf("intake-scan bead gd-mine missing from inbox")
	}
	// Bob's bead that alice does NOT follow must not appear.
	if strings.Contains(body, "gd-bob-private") {
		t.Errorf("REGRESSION: non-followed bob intake leaked into alice's inbox")
	}

	// Daemon contract: ListFollowedBeadIDs called with alice's id.
	if daemon.fakeDaemon.followedBeadIDsCalls == 0 {
		t.Errorf("expected ListFollowedBeadIDs to be called for the inbox listing")
	}
	if daemon.fakeDaemon.followedBeadIDsLastOp != humanID {
		t.Errorf("expected ListFollowedBeadIDs called with op=%d, got %d",
			humanID, daemon.fakeDaemon.followedBeadIDsLastOp)
	}

	// Order assertion: freshly-forwarded must appear above older forwarded
	// rows, and forwarded rows must appear above the operator's own intake.
	// The body interleaves rail markup and the right-pane subgraph; we
	// extract the row IDs from the rail's <span class="bead-row-id"> tags
	// in source order (which is render order).
	idTag := `<span class="bead-row-id">`
	idx := 0
	order := []string{}
	for {
		next := strings.Index(body[idx:], idTag)
		if next < 0 {
			break
		}
		start := idx + next + len(idTag)
		end := strings.Index(body[start:], "</span>")
		if end < 0 {
			break
		}
		order = append(order, body[start:start+end])
		idx = start + end
	}

	pos := func(id string) int { return slices.Index(order, id) }
	if pos("gd-fwd-recent") < 0 {
		t.Fatalf("could not find gd-fwd-recent in render order: %v", order)
	}
	if pos("gd-fwd-recent") > pos("gd-fwd-mid") || pos("gd-fwd-mid") > pos("gd-fwd-old") {
		t.Errorf("forwarded order wrong: want recent < mid < old, got %v", order)
	}
	if pos("gd-fwd-old") > pos("gd-mine") {
		t.Errorf("forwarded rows must appear above intake-scan rows: got %v", order)
	}

	// Dedup: gd-dual (owned + followed) must appear exactly once.
	dualCount := strings.Count(body, ">gd-dual<")
	if dualCount != 1 {
		t.Errorf("expected gd-dual rendered exactly once (owned+followed dedup), got %d", dualCount)
	}

	// Forwarded marker must render on forwarded rows. ka-0000005380
	// condensed the badge to a single ↪ glyph with the full label on
	// title=/aria-label, so the assertion now matches the title attribute
	// (which is what surfaces the signal to hover + screen readers).
	if !strings.Contains(body, `title="forwarded to you"`) {
		t.Errorf("expected forwarded marker on at least one row\n%s", body)
	}
}

// TestInbox_ForwardedDaemonError is the failure-mode counterpart: when the
// follower lookup fails, the inbox still renders the intake-scan path
// rather than blanking the whole page. Critical for inbox availability —
// a daemon hiccup on the (newer) followers endpoint must not regress the
// (older) intake rail.
func TestInbox_ForwardedDaemonError(t *testing.T) {
	humanID := int64(100)
	now := time.Now()

	beads := map[string]*beadsapi.BeadDetail{
		"gd-mine": {
			ID: "gd-mine", Title: "alice's intake", Type: "task", Status: "open",
			Category: "intake", OperatedByID: &humanID, UpdatedAt: now,
		},
	}
	daemon := &inboxFilteringDaemon{
		fakeDaemon: &fakeDaemon{
			beads:              beads,
			deps:               map[string][]beadsapi.Dependency{},
			followedBeadIDsErr: context.DeadlineExceeded,
		},
	}
	lookup := &inboxFakeLookup{rows: map[string]struct {
		id    int64
		kind  string
		found bool
	}{
		"alice@example.com": {id: humanID, kind: "human", found: true},
	}}

	mux := mkInboxMux(t, daemon, lookup)
	req := inboxRequest(http.MethodGet, "/inbox", "alice@example.com")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "gd-mine") {
		t.Errorf("intake-scan path must still render when followers lookup errors")
	}
}

// TestInbox_StreamKindChips verifies the kind-chip strip (ka-0000000894):
// Six chips (All + Intakes + Decisions + Agents + Epics + Forwarded) must
// render with per-kind counts; ?kind=<name> filters the stream to one kind
// without dropping the other chips.
func TestInbox_StreamKindChips(t *testing.T) {
	humanID := int64(100)
	now := time.Now()

	beads := map[string]*beadsapi.BeadDetail{
		"gd-int1": {ID: "gd-int1", Title: "intake-one", Type: "task", Status: "open",
			Category: "intake", OperatedByID: &humanID, UpdatedAt: now},
		"gd-ep1": {ID: "gd-ep1", Title: "epic-one", Type: "epic", Status: "open",
			OperatedByID: &humanID, UpdatedAt: now.Add(-1 * time.Minute)},
		"gd-ag1": {ID: "gd-ag1", Title: "agent-one", Type: "agent", Status: "in_progress",
			OperatedByID: &humanID, UpdatedAt: now.Add(-2 * time.Minute)},
	}
	daemon := &inboxFilteringDaemon{fakeDaemon: &fakeDaemon{beads: beads, deps: map[string][]beadsapi.Dependency{}}}
	lookup := &inboxFakeLookup{rows: map[string]struct {
		id    int64
		kind  string
		found bool
	}{
		"alice@example.com": {id: humanID, kind: "human", found: true},
	}}
	mux := mkInboxMux(t, daemon, lookup)

	t.Run("all chip strip rendered with counts", func(t *testing.T) {
		req := inboxRequest(http.MethodGet, "/inbox", "alice@example.com")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", w.Code)
		}
		body := w.Body.String()
		for _, chip := range []string{">All<", ">Intakes<", ">Decisions<", ">Agents<", ">Epics<", ">Forwarded<"} {
			if !strings.Contains(body, chip) {
				t.Errorf("kind chip strip missing %q\n%s", chip, body)
			}
		}
		// All three rows must surface in the unified stream.
		for _, id := range []string{"gd-int1", "gd-ep1", "gd-ag1"} {
			if !strings.Contains(body, id) {
				t.Errorf("expected stream to include %s\n%s", id, body)
			}
		}
	})

	t.Run("kind=epic filters to epic rows only", func(t *testing.T) {
		req := inboxRequest(http.MethodGet, "/inbox?kind=epic", "alice@example.com")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", w.Code)
		}
		body := w.Body.String()
		// Sub the entire stream block out and inspect only the rendered <li> rows
		// (the chip strip and meta blocks still mention every kind).
		streamStart := strings.Index(body, `<ul class="inbox-stream`)
		if streamStart < 0 {
			// Filtered to epic only; if no rows match, stream wouldn't render.
			// But epic-one should be present, so this is a real failure.
			t.Fatalf("expected stream <ul> in body when kind=epic\n%s", body)
		}
		streamEnd := strings.Index(body[streamStart:], "</ul>")
		if streamEnd < 0 {
			t.Fatalf("malformed stream block")
		}
		streamHTML := body[streamStart : streamStart+streamEnd]
		if !strings.Contains(streamHTML, "gd-ep1") {
			t.Errorf("kind=epic should keep epic row gd-ep1\n%s", streamHTML)
		}
		if strings.Contains(streamHTML, "gd-int1") {
			t.Errorf("kind=epic should drop intake row gd-int1\n%s", streamHTML)
		}
		if strings.Contains(streamHTML, "gd-ag1") {
			t.Errorf("kind=epic should drop agent row gd-ag1\n%s", streamHTML)
		}
	})

	t.Run("invalid kind falls back to All", func(t *testing.T) {
		req := inboxRequest(http.MethodGet, "/inbox?kind=bogus", "alice@example.com")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", w.Code)
		}
		body := w.Body.String()
		for _, id := range []string{"gd-int1", "gd-ep1", "gd-ag1"} {
			if !strings.Contains(body, id) {
				t.Errorf("invalid kind must fall back to All; missing %s", id)
			}
		}
	})
}

// TestInbox_KindChipsCollapseEmpty verifies the ka-0000001480 compaction
// behavior: when >=4 of 6 kind chips have count==0, the empty chips collapse
// into a single "+N empty:" group. Each empty label remains a click target
// (a/href preserved). When the active chip happens to be one of the zeros
// (e.g. ?kind=intake on a quiet inbox), it is NOT collapsed so the user's
// own filter selection stays visible.
func TestInbox_KindChipsCollapseEmpty(t *testing.T) {
	t.Run("unit: below threshold renders no group", func(t *testing.T) {
		// Only 2 of 6 zero — full row.
		chips := []inboxKindChipView{
			{Label: "All", Count: 5},
			{Label: "Intakes", Count: 2},
			{Label: "Decisions", Count: 3},
			{Label: "Agents", Count: 0},
			{Label: "Epics", Count: 1},
			{Label: "Forwarded", Count: 0},
		}
		markCollapsedKindChips(chips)
		for _, c := range chips {
			if c.Collapsed {
				t.Errorf("below threshold: %s should not be Collapsed", c.Label)
			}
		}
		if got := collapsedKindChips(chips); len(got) != 0 {
			t.Errorf("expected no collapsed chips, got %d", len(got))
		}
	})

	t.Run("unit: 4 zeros collapses zeros, keeps non-zero", func(t *testing.T) {
		chips := []inboxKindChipView{
			{Label: "All", Count: 3},
			{Label: "Intakes", Count: 0},
			{Label: "Decisions", Count: 3},
			{Label: "Agents", Count: 0},
			{Label: "Epics", Count: 0},
			{Label: "Forwarded", Count: 0},
		}
		markCollapsedKindChips(chips)
		want := map[string]bool{
			"All":       false,
			"Intakes":   true,
			"Decisions": false,
			"Agents":    true,
			"Epics":     true,
			"Forwarded": true,
		}
		for _, c := range chips {
			if c.Collapsed != want[c.Label] {
				t.Errorf("%s: Collapsed=%v want=%v", c.Label, c.Collapsed, want[c.Label])
			}
		}
		if got := collapsedKindChips(chips); len(got) != 4 {
			t.Errorf("expected 4 collapsed chips, got %d", len(got))
		}
	})

	t.Run("unit: active zero stays visible", func(t *testing.T) {
		// User filtered to Intakes — count is 0 but the active chip must
		// remain visible to show the filter is in effect.
		chips := []inboxKindChipView{
			{Label: "All", Count: 3},
			{Label: "Intakes", Count: 0, Active: true},
			{Label: "Decisions", Count: 3},
			{Label: "Agents", Count: 0},
			{Label: "Epics", Count: 0},
			{Label: "Forwarded", Count: 0},
		}
		markCollapsedKindChips(chips)
		for _, c := range chips {
			if c.Active && c.Collapsed {
				t.Errorf("active chip %s must not be collapsed", c.Label)
			}
		}
		// Three non-active zeros still got collapsed.
		if got := collapsedKindChips(chips); len(got) != 3 {
			t.Errorf("expected 3 collapsed chips, got %d", len(got))
		}
	})

	t.Run("render: quiet inbox shows +N empty group", func(t *testing.T) {
		humanID := int64(100)
		now := time.Now()
		// Stream has only Decisions kind populated — Intakes/Agents/Epics/
		// Forwarded all == 0 → 4 of 6 zero → collapse expected.
		beads := map[string]*beadsapi.BeadDetail{
			"gd-dec1": {ID: "gd-dec1", Title: "decision-one", Type: "decision", Status: "open",
				Category: "decision", OperatedByID: &humanID, UpdatedAt: now},
		}
		daemon := &inboxFilteringDaemon{fakeDaemon: &fakeDaemon{beads: beads, deps: map[string][]beadsapi.Dependency{}}}
		lookup := &inboxFakeLookup{rows: map[string]struct {
			id    int64
			kind  string
			found bool
		}{
			"alice@example.com": {id: humanID, kind: "human", found: true},
		}}
		mux := mkInboxMux(t, daemon, lookup)
		req := inboxRequest(http.MethodGet, "/inbox", "alice@example.com")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", w.Code)
		}
		body := w.Body.String()
		if !strings.Contains(body, `<span class="inbox-kind-empty-group"`) {
			t.Errorf("expected +N empty group in body when 4 of 6 chips are zero\n%s", body)
		}
		if !strings.Contains(body, "+4 empty:") {
			t.Errorf("expected '+4 empty:' badge\n%s", body)
		}
		// Each empty label must still appear as a click target in the group.
		for _, label := range []string{"Intakes", "Agents", "Epics", "Forwarded"} {
			needle := `class="inbox-kind-empty-link" href=`
			if !strings.Contains(body, needle) || !strings.Contains(body, ">"+label+"</a>") {
				t.Errorf("expected collapsed %s to render as a link\n%s", label, body)
			}
		}
		// Non-zero chips ("All", "Decisions") remain in the main chip strip.
		if !strings.Contains(body, `>All<`) || !strings.Contains(body, `>Decisions<`) {
			t.Errorf("non-zero chips must still render outside the +N group\n%s", body)
		}
	})

	t.Run("render: active inbox renders full row", func(t *testing.T) {
		// Stream has at least one of each kind so only 0-1 chips are zero — no collapse.
		humanID := int64(100)
		now := time.Now()
		beads := map[string]*beadsapi.BeadDetail{
			"gd-int1": {ID: "gd-int1", Title: "intake-one", Type: "task", Status: "open",
				Category: "intake", OperatedByID: &humanID, UpdatedAt: now},
			"gd-ep1": {ID: "gd-ep1", Title: "epic-one", Type: "epic", Status: "open",
				OperatedByID: &humanID, UpdatedAt: now.Add(-1 * time.Minute)},
			"gd-ag1": {ID: "gd-ag1", Title: "agent-one", Type: "agent", Status: "in_progress",
				OperatedByID: &humanID, UpdatedAt: now.Add(-2 * time.Minute)},
		}
		daemon := &inboxFilteringDaemon{fakeDaemon: &fakeDaemon{beads: beads, deps: map[string][]beadsapi.Dependency{}}}
		lookup := &inboxFakeLookup{rows: map[string]struct {
			id    int64
			kind  string
			found bool
		}{
			"alice@example.com": {id: humanID, kind: "human", found: true},
		}}
		mux := mkInboxMux(t, daemon, lookup)
		req := inboxRequest(http.MethodGet, "/inbox", "alice@example.com")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		body := w.Body.String()
		if strings.Contains(body, `<span class="inbox-kind-empty-group"`) {
			t.Errorf("active inbox should NOT render +N empty group\n%s", body)
		}
	})
}

// TestInbox_ProjectDropdown verifies the project filter dropdown: options are
// derived from the unfiltered stream, ?project=X narrows rows to that project,
// and the selected option is marked.
func TestInbox_ProjectDropdown(t *testing.T) {
	humanID := int64(100)
	now := time.Now()
	mk := func(id, title, project string) *beadsapi.BeadDetail {
		labels := []string{}
		if project != "" {
			labels = append(labels, "project:"+project)
		}
		return &beadsapi.BeadDetail{
			ID: id, Title: title, Type: "task", Status: "open",
			Category: "intake", OperatedByID: &humanID,
			UpdatedAt: now, Labels: labels,
		}
	}
	beads := map[string]*beadsapi.BeadDetail{
		"gd-gas1": mk("gd-gas1", "gardenio intake one", "gardenio"),
		"gd-gas2": mk("gd-gas2", "gardenio intake two", "gardenio"),
		"gd-mono": mk("gd-mono", "monorepo intake", "monorepo"),
		"gd-bare": mk("gd-bare", "unlabeled intake", ""),
	}
	daemon := &inboxFilteringDaemon{fakeDaemon: &fakeDaemon{beads: beads, deps: map[string][]beadsapi.Dependency{}}}
	lookup := &inboxFakeLookup{rows: map[string]struct {
		id    int64
		kind  string
		found bool
	}{
		"alice@example.com": {id: humanID, kind: "human", found: true},
	}}
	mux := mkInboxMux(t, daemon, lookup)

	t.Run("dropdown rendered with project options and counts", func(t *testing.T) {
		req := inboxRequest(http.MethodGet, "/inbox", "alice@example.com")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", w.Code)
		}
		body := w.Body.String()
		if !strings.Contains(body, `id="inbox-project-select"`) {
			t.Fatalf("expected project <select> in body\n%s", body)
		}
		for _, want := range []string{`>All projects (4)<`, `>gardenio (2)<`, `>monorepo (1)<`, `>(unlabeled) (1)<`} {
			if !strings.Contains(body, want) {
				t.Errorf("project option missing: %q", want)
			}
		}
	})

	t.Run("project=gardenio filters rows and kind counts", func(t *testing.T) {
		req := inboxRequest(http.MethodGet, "/inbox?project=gardenio", "alice@example.com")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", w.Code)
		}
		body := w.Body.String()
		// Selected option must be marked.
		if !strings.Contains(body, `value="gardenio" selected`) {
			t.Errorf("selected option not marked\n%s", body)
		}
		// Stream block must contain only gardenio rows.
		streamStart := strings.Index(body, `<ul class="inbox-stream`)
		if streamStart < 0 {
			t.Fatalf("expected stream <ul>\n%s", body)
		}
		streamEnd := strings.Index(body[streamStart:], "</ul>")
		streamHTML := body[streamStart : streamStart+streamEnd]
		for _, want := range []string{"gd-gas1", "gd-gas2"} {
			if !strings.Contains(streamHTML, want) {
				t.Errorf("expected %s in filtered stream\n%s", want, streamHTML)
			}
		}
		for _, unwant := range []string{"gd-mono", "gd-bare"} {
			if strings.Contains(streamHTML, unwant) {
				t.Errorf("expected %s NOT in filtered stream\n%s", unwant, streamHTML)
			}
		}
		// Kind chip "All" count must reflect the project-filtered set (2),
		// not the full stream.
		if !strings.Contains(body, `>All<span class="chip-count">2</span>`) {
			t.Errorf("kind chip All-count should be 2 after project=gardenio\n%s", body)
		}
	})

	t.Run("project=(none) filters to unlabeled rows", func(t *testing.T) {
		req := inboxRequest(http.MethodGet, "/inbox?project=%28none%29", "alice@example.com")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", w.Code)
		}
		body := w.Body.String()
		streamStart := strings.Index(body, `<ul class="inbox-stream`)
		if streamStart < 0 {
			t.Fatalf("expected stream <ul>\n%s", body)
		}
		streamEnd := strings.Index(body[streamStart:], "</ul>")
		streamHTML := body[streamStart : streamStart+streamEnd]
		if !strings.Contains(streamHTML, "gd-bare") {
			t.Errorf("expected unlabeled bead gd-bare\n%s", streamHTML)
		}
		if strings.Contains(streamHTML, "gd-gas1") || strings.Contains(streamHTML, "gd-mono") {
			t.Errorf("expected labeled beads filtered out\n%s", streamHTML)
		}
	})

	t.Run("unknown project falls back to all projects", func(t *testing.T) {
		req := inboxRequest(http.MethodGet, "/inbox?project=does-not-exist", "alice@example.com")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", w.Code)
		}
		body := w.Body.String()
		for _, id := range []string{"gd-gas1", "gd-gas2", "gd-mono", "gd-bare"} {
			if !strings.Contains(body, id) {
				t.Errorf("unknown project must fall back to All; missing %s", id)
			}
		}
	})

	t.Run("project preserved in kind chip hrefs", func(t *testing.T) {
		req := inboxRequest(http.MethodGet, "/inbox?project=gardenio", "alice@example.com")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		body := w.Body.String()
		if !strings.Contains(body, "kind=intake&amp;project=gardenio") && !strings.Contains(body, "kind=intake&project=gardenio") {
			t.Errorf("kind chip hrefs should preserve project=gardenio\n%s", body)
		}
	})
}

// TestInbox_StreamActivityOrder verifies the unified Gmail-density sort order:
// pinned first, then forwarded (in ForwardOrder asc), then everything else by
// activity-timestamp DESC (newest first), with id asc as the stable tiebreak.
func TestInbox_StreamActivityOrder(t *testing.T) {
	humanID := int64(100)
	now := time.Now()
	mk := func(id, title string, t time.Time, labels ...string) *beadsapi.BeadDetail {
		return &beadsapi.BeadDetail{
			ID: id, Title: title, Type: "task", Status: "open",
			Category: "intake", OperatedByID: &humanID,
			UpdatedAt: t, Labels: labels,
		}
	}
	beads := map[string]*beadsapi.BeadDetail{
		"gd-old": mk("gd-old", "stale", now.Add(-3*time.Hour)),
		"gd-mid": mk("gd-mid", "midstream", now.Add(-1*time.Hour)),
		"gd-new": mk("gd-new", "fresh", now.Add(-1*time.Minute)),
		"gd-pin": mk("gd-pin", "pinned-but-stale", now.Add(-4*time.Hour), labelInboxPinned),
	}
	daemon := &inboxFilteringDaemon{fakeDaemon: &fakeDaemon{beads: beads, deps: map[string][]beadsapi.Dependency{}}}
	lookup := &inboxFakeLookup{rows: map[string]struct {
		id    int64
		kind  string
		found bool
	}{
		"alice@example.com": {id: humanID, kind: "human", found: true},
	}}
	mux := mkInboxMux(t, daemon, lookup)
	req := inboxRequest(http.MethodGet, "/inbox", "alice@example.com")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	idTag := `<span class="bead-row-id">`
	idx := 0
	order := []string{}
	for {
		next := strings.Index(body[idx:], idTag)
		if next < 0 {
			break
		}
		start := idx + next + len(idTag)
		end := strings.Index(body[start:], "</span>")
		if end < 0 {
			break
		}
		order = append(order, body[start:start+end])
		idx = start + end
	}
	pos := func(id string) int { return slices.Index(order, id) }
	if pos("gd-pin") < 0 {
		t.Fatalf("pinned row missing from stream: %v", order)
	}
	if pos("gd-pin") != 0 {
		t.Errorf("pinned row must be first; got order %v", order)
	}
	// Non-pinned rows ordered by activity DESC: new < mid < old in render.
	if pos("gd-new") > pos("gd-mid") || pos("gd-mid") > pos("gd-old") {
		t.Errorf("activity DESC order broken: want new<mid<old, got %v", order)
	}
}

// TestInbox_StreamDedupesAcrossKinds is the ka-0000000893 regression. Beads
// that surface from multiple sources — most commonly an epic / agent /
// decision bead also carrying flag:inbox-worthy, which the worthy-side of
// the intake union returns alongside the type-specific query — must render
// exactly once. The kept row is the type-specific kind (epic/agent/decision)
// so the operator sees the type's chrome instead of a generic intake row.
func TestInbox_StreamDedupesAcrossKinds(t *testing.T) {
	humanID := int64(100)
	now := time.Now()

	beads := map[string]*beadsapi.BeadDetail{
		// An epic that the operator owns AND that's flagged inbox-worthy.
		// Returned by the worthy-side of the intake union AND by the
		// open-epics query — must dedupe to one row, kept as kind=epic.
		"gd-epicW": {
			ID: "gd-epicW", Title: "operator-owned worthy epic",
			Type: "epic", Status: "open",
			OperatedByID: &humanID, UpdatedAt: now.Add(-1 * time.Hour),
			Labels: []string{"flag:inbox-worthy", "project:gardenio"},
		},
		// An agent the operator owns, also worthy. Same shape.
		"gd-agentW": {
			ID: "gd-agentW", Title: "operator-owned worthy agent",
			Type: "agent", Status: "in_progress",
			OperatedByID: &humanID, UpdatedAt: now.Add(-2 * time.Hour),
			Fields: map[string]string{"agent": "an-agent"},
			Labels: []string{"flag:inbox-worthy", "project:gardenio"},
		},
		// Plain intake row — no cross-kind collision, must still render.
		"gd-intakeP": {
			ID: "gd-intakeP", Title: "plain intake",
			Type: "task", Status: "open", Category: "intake",
			OperatedByID: &humanID, UpdatedAt: now.Add(-30 * time.Minute),
			Labels: []string{"project:gardenio"},
		},
	}

	daemon := &inboxFilteringDaemon{fakeDaemon: &fakeDaemon{
		beads: beads,
		deps:  map[string][]beadsapi.Dependency{},
	}}
	lookup := &inboxFakeLookup{rows: map[string]struct {
		id    int64
		kind  string
		found bool
	}{
		"alice@example.com": {id: humanID, kind: "human", found: true},
	}}

	mux := mkInboxMux(t, daemon, lookup)
	req := inboxRequest(http.MethodGet, "/inbox", "alice@example.com")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	// Slice out the activity stream <ul>...</ul> so the assertion isn't
	// fooled by mentions in chips, tooltips, or right-pane sections.
	streamStart := strings.Index(body, `<ul class="inbox-stream`)
	if streamStart < 0 {
		t.Fatalf("expected stream <ul> in body\n%s", body)
	}
	streamEnd := strings.Index(body[streamStart:], "</ul>")
	if streamEnd < 0 {
		t.Fatalf("malformed stream block")
	}
	streamHTML := body[streamStart : streamStart+streamEnd]

	for _, id := range []string{"gd-epicW", "gd-agentW", "gd-intakeP"} {
		needle := `data-bead-id="` + id + `"`
		count := strings.Count(streamHTML, needle)
		if count != 1 {
			t.Errorf("%s: rendered %d times in stream, want 1\nstream HTML:\n%s",
				id, count, streamHTML)
		}
	}

	// Cross-kind precedence: the kept row for the epic must carry the
	// epic kind (not the intake fallback) so the operator sees epic chrome.
	for _, kindCheck := range []struct {
		id   string
		kind string
	}{
		{"gd-epicW", "epic"},
		{"gd-agentW", "agent"},
	} {
		needle := `data-bead-id="` + kindCheck.id + `"`
		idx := strings.Index(streamHTML, needle)
		if idx < 0 {
			continue // already failed above
		}
		// Walk back to the opening <li class="..."> tag for this row.
		liStart := strings.LastIndex(streamHTML[:idx], "<li ")
		if liStart < 0 {
			t.Errorf("%s: cannot locate <li> opener", kindCheck.id)
			continue
		}
		liTag := streamHTML[liStart:idx]
		if !strings.Contains(liTag, "inbox-stream-row-kind-"+kindCheck.kind) {
			t.Errorf("%s: kept row should be kind=%s, got <li>=%q",
				kindCheck.id, kindCheck.kind, liTag)
		}
	}
}

// TestInbox_SummaryTiles_AboveTheFold guards the ka-0000004756 density fix.
// The /inbox page must render a compact summary-tiles row with one tile each
// for my-open / decisions / mentions / open-epics, drawn from already-loaded
// data (no new daemon queries). The tiles MUST appear above the chip-strip
// so a first-time operator sees real counts in the first viewport instead of
// blank space.
func TestInbox_SummaryTiles_AboveTheFold(t *testing.T) {
	lookup := &inboxFakeLookup{
		rows: map[string]struct {
			id    int64
			kind  string
			found bool
		}{
			"alice@example.com": {id: 100, kind: "human", found: true},
		},
	}
	daemon := &inboxFilteringDaemon{fakeDaemon: inboxFake()}
	mux := mkInboxMux(t, daemon, lookup)

	req := inboxRequest(http.MethodGet, "/inbox", "alice@example.com")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	tilesIdx := strings.Index(body, `class="inbox-summary-tiles"`)
	if tilesIdx < 0 {
		t.Fatalf("expected .inbox-summary-tiles to render on /inbox\n%s", body)
	}

	// Each labelled tile must be present.
	for _, label := range []string{"my open", "decisions", "mentions", "open epics"} {
		if !strings.Contains(body, ">"+label+"<") {
			t.Errorf("expected summary tile label %q in body", label)
		}
	}

	// Tiles MUST sit above the chip-strip so they're inside the operator's
	// first viewport. If a future refactor pushes them below the chips,
	// the density win is lost — fail loudly.
	// The scope strip is the page-level filter menu, so it carries the
	// distinct .filter-strip hook (ka-0000005049 / audit F5) alongside
	// .chip-strip. Match the wrapper by its stable role+label, not the exact
	// class string, so the F5 split doesn't silently break this ordering guard.
	chipIdx := strings.Index(body, `aria-label="Inbox scope" role="tablist"`)
	if chipIdx < 0 {
		t.Fatalf("expected inbox scope filter-strip to render on /inbox")
	}
	if tilesIdx > chipIdx {
		t.Errorf("summary tiles must render above chip-strip; tilesIdx=%d chipIdx=%d", tilesIdx, chipIdx)
	}

	// Drill view (/inbox/{id}) suppresses the tiles so the focused subgraph
	// keeps its own header hierarchy.
	drillReq := inboxRequest(http.MethodGet, "/inbox/gd-h1", "alice@example.com")
	drillW := httptest.NewRecorder()
	mux.ServeHTTP(drillW, drillReq)
	if drillW.Code != http.StatusOK {
		t.Fatalf("drill: expected 200, got %d", drillW.Code)
	}
	if strings.Contains(drillW.Body.String(), `class="inbox-summary-tiles"`) {
		t.Errorf("summary tiles should be suppressed on /inbox/{id} drill view")
	}
}

// TestInbox_TwoColumnLayoutWrapper guards the ka-0000004622 density fix.
// The /inbox proper view must wrap the activity stream + recent-mentions
// side rail in an .inbox-main-cols grid so wide viewports (>=1280px) lay
// both buckets out side-by-side instead of stacking vertically and burning
// the right half of the page on whitespace. The drill view (/inbox/{id})
// renders the constellation full-width and must NOT include the wrapper.
// The page-scoped .inbox-page wrapper must always be present so the
// skin-level body-widening rules (body:has(.inbox-page)) apply even when
// the bead stream is empty.
func TestInbox_TwoColumnLayoutWrapper(t *testing.T) {
	lookup := &inboxFakeLookup{
		rows: map[string]struct {
			id    int64
			kind  string
			found bool
		}{
			"alice@example.com": {id: 100, kind: "human", found: true},
		},
	}
	daemon := &inboxFilteringDaemon{fakeDaemon: inboxFake()}
	mux := mkInboxMux(t, daemon, lookup)

	req := inboxRequest(http.MethodGet, "/inbox", "alice@example.com")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	if !strings.Contains(body, `class="inbox-page"`) {
		t.Errorf("expected .inbox-page wrapper on /inbox so body-widening rules apply\n%s", body)
	}
	colsIdx := strings.Index(body, `class="inbox-main-cols"`)
	if colsIdx < 0 {
		t.Fatalf("expected .inbox-main-cols wrapper on /inbox proper view\n%s", body)
	}
	mainIdx := strings.Index(body, `class="inbox-main-col"`)
	if mainIdx < 0 || mainIdx < colsIdx {
		t.Errorf("expected .inbox-main-col inside .inbox-main-cols; colsIdx=%d mainIdx=%d", colsIdx, mainIdx)
	}
	// The activity-stream block must live inside the main column so the
	// grid lays out stream + side rail side-by-side at >=1280px. Match the
	// HTML attribute (not the CSS rule, which appears earlier in <style>).
	streamMetaIdx := strings.Index(body, `class="inbox-stream-meta-row"`)
	if streamMetaIdx < 0 || streamMetaIdx < mainIdx {
		t.Errorf("expected stream meta row inside .inbox-main-col; mainIdx=%d streamMetaIdx=%d", mainIdx, streamMetaIdx)
	}

	// Drill view: constellation renders full-width — the two-column wrapper
	// would split the focused subgraph in half. Suppress.
	drillReq := inboxRequest(http.MethodGet, "/inbox/gd-h1", "alice@example.com")
	drillW := httptest.NewRecorder()
	mux.ServeHTTP(drillW, drillReq)
	if drillW.Code != http.StatusOK {
		t.Fatalf("drill: expected 200, got %d", drillW.Code)
	}
	drillBody := drillW.Body.String()
	if strings.Contains(drillBody, `class="inbox-main-cols"`) {
		t.Errorf(".inbox-main-cols must NOT render on /inbox/{id} drill view (constellation is full-width)")
	}
	if !strings.Contains(drillBody, `class="inbox-page"`) {
		t.Errorf("expected .inbox-page wrapper on /inbox/{id} drill view so body-widening rules apply")
	}
}
