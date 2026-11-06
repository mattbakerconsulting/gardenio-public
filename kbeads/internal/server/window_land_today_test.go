package server

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gardenio/kbeads/internal/model"
	"gardenio/kbeads/internal/store"
)

// forget drops every built land view, as windowLandViewTTL passing would.
func (c *windowLandViewCache) forget() {
	c.mu.Lock()
	defer c.mu.Unlock()
	clear(c.entries)
}

// typeTestEvidence is the evidence bead type, which a plan's steps gather.
const typeTestEvidence model.BeadType = "evidence"

// rowStore counts the bead rows a land view reads, the history rows among
// them, and the list reads that would carry a row's whole fields or
// description.
type rowStore struct {
	store.Store
	mu      sync.Mutex
	rows    int
	history map[string]int
	heavy   int
}

// windowHistoryRow reports whether a land view has no use for b: a closed
// decision, escalation or agent, or a type it never renders.
func windowHistoryRow(b *model.Bead) string {
	switch b.Type {
	case model.TypeDecision, model.TypeEscalation, typeAgent:
		if b.Status == model.StatusClosed {
			return "closed " + string(b.Type)
		}
	case typeTestEvidence, typeDoc, model.TypeBundle:
		return string(b.Type)
	}
	return ""
}

func (r *rowStore) count(beads []*model.Bead) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rows += len(beads)
	for _, b := range beads {
		if h := windowHistoryRow(b); h != "" {
			r.history[h]++
		}
	}
}

func (r *rowStore) ListBeads(ctx context.Context, f model.BeadFilter) ([]*model.Bead, int, error) {
	if len(f.FieldsOnly) == 0 || !f.OmitDescription {
		r.mu.Lock()
		r.heavy++
		r.mu.Unlock()
	}
	got, n, err := r.Store.ListBeads(ctx, f)
	r.count(got)
	return got, n, err
}

func (r *rowStore) GetBeadsByIDs(ctx context.Context, ids []string) ([]*model.Bead, error) {
	got, err := r.Store.GetBeadsByIDs(ctx, ids)
	r.count(got)
	r.mu.Lock()
	r.heavy++
	r.mu.Unlock()
	return got, err
}

// todayLand is the gardenio land on 11-03, measured on gd-FC7tIACC5f: 34
// open plans, ~1,430 steps, ~1,900 escalations (52 open), ~150 decisions (2
// open), ~170 evidence beads, a narrative per plan, and ~5,000 assigned
// agents, almost all closed.
const (
	todayPlans          = 34
	todayPhases         = 2
	todaySteps          = 21 // per phase: 34 × 2 × 21 = 1,428
	todayAgentsPerStep  = 3  // plus one on every other step: ~5,000
	todayOpenEscEvery   = 27 // ~53 open escalations
	todayDecisionEvery  = 9  // ~160 decisions
	todayOpenDecisionAt = 2  // open decisions in the land
)

// seedTodayLand adds todayLand to claimlayer and returns the rows a land
// view needs: plans, phases, steps, open decisions and escalations, and open
// agents.
func seedTodayLand(f *scopedFixture) (need int) {
	base := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	n := 0
	add := func(id string, typ model.BeadType, status model.Status, parent string, labels []string, fields string) {
		n++
		at := base.Add(time.Duration(n) * time.Second)
		b := &model.Bead{ID: id, Type: typ, Kind: model.KindIssue, Title: "T " + id, Status: status, Fleet: "default",
			Labels: labels, CreatedAt: at, UpdatedAt: at, Description: "a long description"}
		if fields != "" {
			b.Fields = []byte(fields)
		}
		if status == model.StatusClosed {
			b.ClosedAt = &at
		}
		f.ms.beads[id], f.ms.labels[id] = b, labels
		if parent != "" {
			f.ms.deps[id] = append(f.ms.deps[id], &model.Dependency{BeadID: id, DependsOnID: parent, Type: model.DepParentChild})
		}
	}
	cl := []string{"project:claimlayer"}
	decisions, escalations := 0, 0
	for p := range todayPlans {
		plan := fmt.Sprintf("today-%02d", p)
		add(plan, model.TypeEpic, model.StatusInProgress, "", []string{"plan", "project:claimlayer"},
			fmt.Sprintf(`{"plan_state":"building","phase_order":["%s-ph0","%s-ph1"]}`, plan, plan))
		add(plan+"-bundle", model.TypeBundle, model.StatusOpen, plan, cl, `{"kind":"plan","headline":"h"}`)
		add(plan+"-doc", typeDoc, model.StatusOpen, plan+"-bundle", cl, `{"format":"md","content":"narrative"}`)
		need++
		for ph := range todayPhases {
			phase := fmt.Sprintf("%s-ph%d", plan, ph)
			add(phase, model.TypeEpic, model.StatusOpen, plan, cl, "")
			need++
			for st := range todaySteps {
				step := fmt.Sprintf("%s-s%02d", phase, st)
				status := model.StatusClosed
				if st >= todaySteps-4 {
					status = model.StatusOpen
				}
				add(step, model.TypeTask, status, phase, cl, "")
				need++
				for e := range 1 + st%3/2 {
					escalations++
					status := model.StatusClosed
					if escalations%todayOpenEscEvery == 0 {
						status = model.StatusOpen
						need++
					}
					add(fmt.Sprintf("%s-esc%d", step, e), model.TypeEscalation, status, step, cl,
						`{"operation_type":"gitlab_mr_merge","context":"`+fmt.Sprintf("%0800d", 0)+`"}`)
				}
				if st%todayDecisionEvery == 0 {
					decisions++
					status := model.StatusClosed
					if decisions <= todayOpenDecisionAt {
						status = model.StatusOpen
						need++
					}
					add(step+"-dec", model.TypeDecision, status, step, cl, `{"chosen":"y","options":[{"id":"y","label":"Yes"}]}`)
					add(step+"-ev", typeTestEvidence, model.StatusOpen, step, cl, "")
				}
				for a := range todayAgentsPerStep + st%2 {
					agent := fmt.Sprintf("%s-agent%d", step, a)
					status, state := model.StatusClosed, "done"
					if st == todaySteps-1 && a == 0 {
						status, state = model.StatusOpen, "working"
						need++
					}
					add(agent, typeAgent, status, "", nil,
						fmt.Sprintf(`{"agent":"crew-%s","agent_state":%q,"prompt":"%0400d"}`, agent, state, 0))
					f.ms.deps[agent] = append(f.ms.deps[agent], &model.Dependency{BeadID: agent, DependsOnID: step, Type: windowAssignedDep})
				}
			}
		}
	}
	return need
}

// TestWindow_LandViewTodaysGardenio: the land view of a land the size of
// gardenio on 11-03 reads only the rows it renders. The read budget is those
// rows plus the small seeded land's: the live view took 10-13 s because it
// read every closed escalation and decision and every agent ever assigned,
// 8.6k rows and ~57 MB, for a view that renders ~1.6k (gd-FC7tIACC5f).
// The mock store stands in for Postgres here, so the budget is counted in
// rows and field-heavy reads, not seconds.
func TestWindow_LandViewTodaysGardenio(t *testing.T) {
	f := newScopedFixture(t)
	seedWindowLand(t, f)
	need := seedTodayLand(f)
	rs := &rowStore{Store: f.ms, history: map[string]int{}}
	f.srv.store = rs
	ctx := context.Background()
	wf := windowFieldSet{Target: winLandProject, TargetKind: windowTargetLand, Land: "claimlayer"}

	start := time.Now()
	v, err := f.srv.buildLandWindowView(ctx, wf)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("land view of %d plans: %d rows, %v (mock store)", len(v.Plans), rs.rows, time.Since(start))

	const smallLand = 40 // seedWindowLand's plans, steps, decisions and crew
	if budget := need + smallLand; rs.rows > budget {
		t.Errorf("land view read %d bead rows, budget %d (the rows it renders)", rs.rows, budget)
	}
	if len(rs.history) > 0 {
		t.Errorf("land view read history it never renders: %v", rs.history)
	}
	if rs.heavy > 0 {
		t.Errorf("%d reads carried whole fields or descriptions; want only the projected fields", rs.heavy)
	}

	steps, crew, openEsc, openDec := 0, 0, 0, 0
	for _, p := range v.Plans {
		steps += p.StepsTotal
		crew += len(p.Crew)
		openEsc += p.OpenEscalations
		openDec += p.OpenDecisions
	}
	wantSteps := todayPlans * todayPhases * todaySteps
	if len(v.Plans) < todayPlans || steps < wantSteps || crew < todayPlans*todayPhases || openDec < todayOpenDecisionAt || openEsc < 50 {
		t.Errorf("land view: %d plans, %d steps, %d crew, %d open decisions, %d open escalations; want today's land whole",
			len(v.Plans), steps, crew, openDec, openEsc)
	}
	for _, p := range v.Plans {
		if p.Withheld {
			t.Errorf("plan %q withheld; its history should not count against the walk's bound", p.Title)
		}
	}

	// A second read within windowLandViewTTL reads only the target.
	rows := rs.rows
	if _, err := f.srv.buildLandWindowView(ctx, wf); err != nil {
		t.Fatal(err)
	}
	if rs.rows != rows {
		t.Errorf("a second read within the TTL read %d more rows", rs.rows-rows)
	}
}

// TestWindowLandViewCache: readers of one land share one build; a reader
// that gives up leaves the build running for the next; a build is served for
// windowLandViewTTL and a failed one not at all.
func TestWindowLandViewCache(t *testing.T) {
	var clock atomic.Int64
	clock.Store(time.Date(2026, 11, 3, 19, 0, 0, 0, time.UTC).UnixNano())
	c := &windowLandViewCache{now: func() time.Time { return time.Unix(0, clock.Load()) }}
	var builds atomic.Int64
	release := make(chan struct{})
	var buildCtxErr atomic.Value
	build := func(ctx context.Context) (*windowLandView, error) {
		builds.Add(1)
		<-release
		buildCtxErr.Store(fmt.Sprint(ctx.Err()))
		return &windowLandView{Land: "gardenio"}, nil
	}

	gaveUp, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := c.get(gaveUp, "gardenio", build)
		done <- err
	}()
	for builds.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("reader that gave up = %v, want context.Canceled", err)
	}
	var wg sync.WaitGroup
	for range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if v, err := c.get(context.Background(), "gardenio", build); err != nil || v.Land != "gardenio" {
				t.Errorf("waiting reader = %v, %v", v, err)
			}
		}()
	}
	close(release)
	wg.Wait()
	if n := builds.Load(); n != 1 {
		t.Fatalf("%d builds for four readers of one land, want 1", n)
	}
	if got := buildCtxErr.Load(); got != "<nil>" {
		t.Errorf("the build's context ended with its first reader: %v", got)
	}

	clock.Add(int64(windowLandViewTTL - time.Second))
	if _, err := c.get(context.Background(), "gardenio", build); err != nil || builds.Load() != 1 {
		t.Errorf("within the TTL: %v, %d builds; want the cached view", err, builds.Load())
	}
	clock.Add(int64(2 * time.Second))
	if _, err := c.get(context.Background(), "gardenio", build); err != nil || builds.Load() != 2 {
		t.Errorf("past the TTL: %v, %d builds; want a new build", err, builds.Load())
	}

	fail := func(context.Context) (*windowLandView, error) {
		builds.Add(1)
		return nil, errors.New("pq: canceling statement")
	}
	if _, err := c.get(context.Background(), "other", fail); err == nil {
		t.Fatal("failed build answered no error")
	}
	if _, err := c.get(context.Background(), "other", build); err != nil || builds.Load() != 4 {
		t.Errorf("after a failed build: %v, %d builds; want a new build", err, builds.Load())
	}
}
