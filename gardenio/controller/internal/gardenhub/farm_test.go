package gardenhub

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/heraldry"
)

var updateFixture = flag.Bool("update-farm-fixture", false, "rewrite testdata/farm_fixture.json from fixtureWorld")

const baseUnix int64 = 1_792_000_000

// fakeFarmSrc is an in-memory daemon that honours the filters the farm uses.
type fakeFarmSrc struct {
	mu    sync.Mutex
	beads []*beadsapi.BeadDetail
	deps  map[string][]beadsapi.Dependency

	lists, counts, histCalls, depCalls, getCalls int
	workLists                                    int // list reads of the work rows (readRows pages)
	deltas                                       int // list reads with an updated_after cursor
	closedLists                                  int // full list reads of the closed work rows (listClosedRows pages)
	unprojected                                  int // work-row list reads sent without the omit projection
	counted                                      int // row walks and roster reads that asked for a total
	events                                       chan beadsapi.SSEEvent
	failProduce                                  bool // the produce lists (farm_produce.go) error out
	failEvidence                                 bool // the evidence list (farm_harvest.go) errors out
	// releases is each release-enabled project's releases, any order (ReleaseSource, farm_releases_test.go);
	// relReads counts the ReleaseSource calls and failReleases makes them error out.
	releases     map[string][]beadsapi.Release
	relReads     int
	failReleases bool
}

func newFakeFarm() *fakeFarmSrc {
	return &fakeFarmSrc{deps: map[string][]beadsapi.Dependency{}, events: make(chan beadsapi.SSEEvent, 16)}
}

func (f *fakeFarmSrc) EventStream(context.Context, string) (<-chan beadsapi.SSEEvent, error) {
	return f.events, nil
}

func in(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func (f *fakeFarmSrc) ListBeadsFiltered(_ context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failProduce && (q.Fields["mr_url"] == "*" || in(q.Types, "bundle")) {
		return nil, errors.New("produce read failed")
	}
	if f.failEvidence && in(q.Types, "evidence") {
		return nil, errors.New("evidence read failed")
	}
	var m []*beadsapi.BeadDetail
	for _, b := range f.beads {
		if len(q.Types) > 0 && !in(q.Types, b.Type) {
			continue
		}
		if len(q.Statuses) > 0 && !in(q.Statuses, b.Status) {
			continue
		}
		if !q.UpdatedAfter.IsZero() && !b.UpdatedAt.After(q.UpdatedAfter) {
			continue
		}
		ok := true
		for _, l := range q.Labels {
			if !in(b.Labels, l) {
				ok = false
			}
		}
		for k, v := range q.Fields {
			switch v {
			case "*": // presence, as the daemon reads it (kbeads queries.go)
				if strings.TrimSpace(b.Fields[k]) == "" {
					ok = false
				}
			case "":
				if b.Fields[k] != "" {
					ok = false
				}
			default:
				if b.Fields[k] != v {
					ok = false
				}
			}
		}
		if ok {
			m = append(m, b)
		}
	}
	if q.CountOnly {
		f.counts++
		return &beadsapi.ListBeadsResult{Total: len(m)}, nil
	}
	f.tally(q)
	switch q.Sort {
	case "-created_at":
		sort.SliceStable(m, func(i, j int) bool { return m[i].CreatedAt.After(m[j].CreatedAt) })
	case "-updated_at":
		sort.SliceStable(m, func(i, j int) bool { return m[i].UpdatedAt.After(m[j].UpdatedAt) })
	}
	total := len(m)
	if q.Offset > len(m) {
		m = nil
	} else {
		m = m[q.Offset:]
	}
	if q.Limit > 0 && len(m) > q.Limit {
		m = m[:q.Limit]
	}
	if q.NoTotal && q.Limit > 0 && len(m) == q.Limit {
		total = -1 // a full uncounted page, as the daemon answers it
	}
	if !q.WithParent {
		cp := make([]*beadsapi.BeadDetail, len(m))
		for i, b := range m {
			c := *b
			c.ParentID = ""
			cp[i] = &c
		}
		m = cp
	}
	return &beadsapi.ListBeadsResult{Beads: m, Total: total}, nil
}

// tally bumps the list counters q falls under. The caller holds f.mu.
func (f *fakeFarmSrc) tally(q beadsapi.ListBeadsQuery) {
	f.lists++
	if in(q.Types, "task") {
		f.workLists++
		if !in(q.Omit, "description") || !in(q.Omit, "notes") {
			f.unprojected++
		}
	}
	if (slices.Equal(q.Types, farmWorkTypes) || (in(q.Types, "agent") && len(q.Fields) == 0)) && !q.NoTotal {
		f.counted++
	}
	if !q.UpdatedAfter.IsZero() {
		f.deltas++
	}
	if in(q.Types, "task") && len(q.Statuses) == 1 && q.Statuses[0] == "closed" {
		f.closedLists++
	}
}

func (f *fakeFarmSrc) BatchGetBeads(_ context.Context, ids []string) (map[string]*beadsapi.BeadDetail, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getCalls++
	out := map[string]*beadsapi.BeadDetail{}
	for _, b := range f.beads {
		if in(ids, b.ID) {
			out[b.ID] = b
		}
	}
	return out, nil
}

func (f *fakeFarmSrc) BatchGetDependencies(_ context.Context, ids []string, _ string) (map[string][]beadsapi.Dependency, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.depCalls++
	out := map[string][]beadsapi.Dependency{}
	for _, id := range ids {
		out[id] = f.deps[id]
		// The parent-child edge every bead with a parent carries.
		for _, b := range f.beads {
			if b.ID == id && b.ParentID != "" {
				out[id] = append(out[id], beadsapi.Dependency{BeadID: id, DependsOnID: b.ParentID, Type: "parent-child"})
			}
		}
	}
	return out, nil
}

func (f *fakeFarmSrc) BatchGetChildrenByStatus(_ context.Context, ids []string) (map[string]map[string]int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.histCalls++
	out := map[string]map[string]int{}
	for _, id := range ids {
		out[id] = map[string]int{}
	}
	for _, b := range f.beads {
		if h, ok := out[b.ParentID]; ok {
			h[b.Status]++
		}
	}
	return out, nil
}

func (f *fakeFarmSrc) add(bs ...*beadsapi.BeadDetail) {
	f.mu.Lock()
	f.beads = append(f.beads, bs...)
	f.mu.Unlock()
}

// drop takes bs out of the source again (by id): a scene's undo of an add.
func (f *fakeFarmSrc) drop(bs ...*beadsapi.BeadDetail) {
	gone := map[string]bool{}
	for _, b := range bs {
		gone[b.ID] = true
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	keep := f.beads[:0]
	for _, b := range f.beads {
		if !gone[b.ID] {
			keep = append(keep, b)
		}
	}
	f.beads = keep
}

func (f *fakeFarmSrc) mutate(id string, fn func(*beadsapi.BeadDetail)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, b := range f.beads {
		if b.ID == id {
			fn(b)
		}
	}
}

var seq int64

// The listed tasks are planted by created then id whatever their state, so a
// status change never moves a cell; the per-phase cap still keeps the moving
// ones (sortTasks) before planting (gd-wDdun2AVwq).
func TestSortPlanted(t *testing.T) {
	ts := []FarmTask{{ID: "c", State: TaskInProgress, Created: 3}, {ID: "a", State: TaskOpen, Created: 1}, {ID: "b", State: TaskBlocked, Created: 1}}
	sortPlanted(ts)
	if ts[0].ID != "a" || ts[1].ID != "b" || ts[2].ID != "c" {
		t.Fatalf("planted order: %+v", ts)
	}
	ts[0].State, ts[2].State, ts[1].Priority = TaskInProgress, TaskDeferred, 0
	sortPlanted(ts)
	if ts[0].ID != "a" || ts[1].ID != "b" || ts[2].ID != "c" {
		t.Fatalf("a status change moved a plant: %+v", ts)
	}
	sortTasks(ts)
	if ts[0].ID != "a" || ts[1].ID != "b" || ts[2].ID != "c" {
		t.Fatalf("the cap keeps the moving ones first: %+v", ts)
	}
}

// fb builds a bead. Created is strictly increasing so "-created_at" paging is
// deterministic; updated is baseUnix+upd.
func fb(id, typ, status, parent string, upd int64) *beadsapi.BeadDetail {
	seq++
	return &beadsapi.BeadDetail{
		ID: id, Title: "Title " + id, Type: typ, Status: status, ParentID: parent, Priority: 2,
		Labels: []string{"project:gardenio"}, Fields: map[string]string{},
		CreatedAt: time.Unix(baseUnix+seq, 0), UpdatedAt: time.Unix(baseUnix+upd, 0),
		OperatedBy: "OIDC:Matt@Example.com",
	}
}

func planBead(id, state, status string, phases ...string) *beadsapi.BeadDetail {
	b := fb(id, "epic", status, "", 50)
	b.Labels = append(b.Labels, "plan")
	if state != "" {
		b.Fields["plan_state"] = state
	}
	if len(phases) > 0 {
		raw, _ := json.Marshal(phases)
		b.Fields["phase_order"] = string(raw)
	}
	return b
}

func newTestFarm(src FarmSource) *Farm {
	f := NewFarm(src, nil)
	f.st.version = 1000
	f.dl = newDeltaLog(f.st.version, cutView{})
	f.now = func() time.Time { return time.Unix(baseUnix+10_000, 0) }
	return f
}

type snap struct {
	V           uint64          `json:"v"`
	BuiltAt     string          `json:"built_at"`
	Plans       []FarmPlan      `json:"plans"`
	Gardeners   []FarmGardener  `json:"gardeners"`
	Seeds       []FarmSeed      `json:"seeds"`
	ClosedPlans FarmClosedPlans `json:"closed_plans"`
	Meadow      FarmMeadow      `json:"meadow"`
	Wishes      FarmWishes      `json:"wishes"`
	Escalations FarmEscalations `json:"escalations"`
	Releases    FarmReleases    `json:"releases"`
	Piles       FarmPiles       `json:"piles"`
	Meta        FarmMeta        `json:"meta"`
}

func decodeSnap(t *testing.T, f *Farm) snap {
	t.Helper()
	_, b, _ := f.Snapshot()
	var s snap
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatalf("snapshot is not valid JSON: %v\n%s", err, b)
	}
	return s
}

func findPlan(s snap, id string) *FarmPlan {
	for i := range s.Plans {
		if s.Plans[i].ID == id {
			return &s.Plans[i]
		}
	}
	return nil
}

func findTask(ts []FarmTask, id string) *FarmTask {
	for i := range ts {
		if ts[i].ID == id {
			return &ts[i]
		}
	}
	return nil
}

// fixtureWorld is a small farm shaped like the real fleet: a stalled plan with
// phases, a draft and a ready seed, a finished plan, loose beads (one with an
// orphaned MR) and closed archives.
func fixtureWorld() *fakeFarmSrc {
	seq = 0
	f := newFakeFarm()

	// Plan A: building, three phases (the last one closed), an unphased step.
	f.add(planBead("gd-planA", "building", "in_progress", "gd-A1", "gd-A2", "gd-A3"))
	f.beads[len(f.beads)-1].Title = "Edge IdP rollout"
	// Its crew spend, as the controller rolled it up five minutes ago (gd-kcy4IFNzv5).
	setPlanSpend(f.beads[len(f.beads)-1], "12.3456", baseUnix+10_000-300, "3", "1")
	f.add(fb("gd-A1", "epic", "in_progress", "gd-planA", 40), fb("gd-A2", "epic", "open", "gd-planA", 40), fb("gd-A3", "epic", "closed", "gd-planA", 5))
	f.beads[1].Title, f.beads[2].Title, f.beads[3].Title = "Foundations", "Rollout", "Cutover"
	f.beads[1].Heraldry, f.beads[2].Heraldry = "🧭🔥📦", "🚀🔑📣" // two armed phases: their gardeners wear them (heraldry step 5.9)

	exch := fb("gd-A1a", "feature", "in_progress", "gd-A1", 900)
	exch.Title, exch.Assignee = "Token exchange for agent pods", "mo"
	exch.Fields["mr_url"], exch.Fields["mr_iid"], exch.Fields["mr_state"] = "https://example.com/mr/412", "!412", "opened"
	soak := fb("gd-A1b", "task", "in_progress", "gd-A1", 800)
	soak.Title, soak.Assignee = "Staging soak test for exchange", "mo"
	cutover := fb("gd-A2a", "task", "open", "gd-A2", 700)
	cutover.Title, cutover.Priority = "Flip production to the new IdP", 0
	announce := fb("gd-A2b", "task", "open", "gd-A2", 600)
	announce.Title = "Announce the cutover window"
	bug := fb("gd-A2c", "bug", "open", "gd-A2", 650)
	bug.Title, bug.Priority = "Refresh fails after clock skew", 0
	// A step nested one level below the phase still belongs to it.
	sub := fb("gd-A2d", "epic", "open", "gd-A2", 500)
	nested := fb("gd-A2e", "task", "deferred", "gd-A2d", 400)
	nested.Title = "Remove the old login page"
	dec := fb("gd-A2f", "decision", "open", "gd-A2", 950)
	dec.Title = "Where does the chat live?"
	dec.Fields["requesting_agent_bead_id"] = "gd-gAsk" // the yielding gardener is parked on it
	other := fb("gd-A9", "chore", "open", "gd-planA", 300)
	other.Title = "Rotate the staging signing key"
	closed1 := fb("gd-A1x", "task", "closed", "gd-A1", 20)
	closed2 := fb("gd-A1y", "task", "closed", "gd-A1", 21)
	f.add(exch, soak, cutover, announce, bug, sub, nested, dec, other, closed1, closed2)
	f.deps["gd-A2b"] = []beadsapi.Dependency{{BeadID: "gd-A2b", DependsOnID: "gd-A2a", Type: "blocks"}}
	f.deps["gd-A2a"] = []beadsapi.Dependency{{BeadID: "gd-A2a", DependsOnID: "gd-A1a", Type: "blocks"}, {BeadID: "gd-A2a", DependsOnID: "gd-gone", Type: "blocks"}, {BeadID: "gd-A2a", DependsOnID: "gd-A1", Type: "related"}}

	// Seeds: one frosted, one ready.
	f.add(planBead("gd-seed1", "draft", "open", "gd-S1a"), fb("gd-S1a", "epic", "open", "gd-seed1", 60), fb("gd-S1t", "task", "open", "gd-S1a", 61), fb("gd-S1u", "task", "open", "gd-S1a", 62))
	f.add(planBead("gd-seed2", "ready", "open"))
	f.beads[len(f.beads)-1].Title = "Local agents"

	// Finished plan.
	cp := planBead("gd-done1", "complete", "closed")
	cp.Title = "Taints"
	setPlanSpend(cp, "4.5000", baseUnix-3_600, "2", "0")
	f.add(cp)

	// The meadow: loose beads, one orphaned with a PR, one with a non-plan parent.
	m1 := fb("gd-M1", "task", "open", "", 100)
	m1.Title = "Slack thread: not totally convinced the nudge fires"
	m2 := fb("gd-M2", "task", "open", "", 200)
	m2.Title = "Provision the owner as admin in Keycloak"
	m2.Fields["mr_url"], m2.Fields["mr_iid"], m2.Fields["mr_state"] = "https://example.com/pr/198", "198", "opened"
	m3 := fb("gd-M3", "bug", "open", "gd-notaplan", 150)
	f.add(m1, m2, m3, fb("gd-notaplan", "epic", "open", "", 10))
	// Not farm work: never a plant. It is a gardener with nothing set, at home.
	f.add(fb("gd-agent", "agent", "in_progress", "", 1))

	// Archives: only counted.
	for i := range 47 {
		f.add(fb(fmt.Sprintf("gd-ce%d", i), "escalation", "closed", "", 1))
	}
	for i := range 8 {
		f.add(fb(fmt.Sprintf("gd-cd%d", i), "decision", "closed", "", 1))
	}
	addFixtureGardeners(f)

	// Plan A's produce bin (cursor-grid spec 5.6): the open MR on gd-A1a is coming; a finished step's merged MR and the plan's
	// narrative bundle are in it. Added last, so no other bead's created_at (and so the World layout) moves. Neither is a row:
	// the step is closed and a bundle is not farm work.
	closed2.Fields["mr_url"], closed2.Fields["mr_iid"], closed2.Fields["mr_state"] = "https://example.com/mr/405", "!405", "merged"
	closed2.Fields["mr_title"] = "Staging IdP behind the edge"
	bundle := fb("gd-Abun", "bundle", "open", "gd-planA", 450)
	bundle.Title = "Edge IdP rollout: the plan narrative"
	f.add(bundle)
	// The finished step's harvest (harvest spec §3): the signals github-bridge and the merge desk write at merge, and a
	// UX grade filed on it. gd-A1x has nothing recorded and no closed_at, so it is bare stubble closed at its updated.
	closed2.ClosedAt = time.Unix(baseUnix+21, 0).UTC().Format(time.RFC3339)
	closed2.Fields["ci_state"], closed2.Fields["tests_touched"], closed2.Fields["goldens_touched"], closed2.Fields["review_rounds"] = "success", "3", "2", "1"
	grade := fb("gd-Aev", "evidence", "closed", "gd-A1y", 22)
	grade.Title = "UX grade 5/5: staging login"
	grade.Fields["kind"], grade.Fields["step"], grade.Fields["subject"] = "ux", "gd-A1y", "staging login"
	grade.Fields["score"], grade.Fields["of"], grade.Fields["percent"] = "5", "5", "100"
	grade.Fields["urls"] = "https://example.com/image/a.png\nhttps://example.com/image/b.png"
	f.add(grade)

	addFixtureWishes(f) // last, so no other bead's created_at moves
	addFixtureReleases(f)
	return f
}

// agentBead builds a live agent bead. It does not touch seq, so adding agents
// to a world never moves the created_at (and so the World layout) of its plans.
func agentBead(id, name, role, state, task string, upd int64) *beadsapi.BeadDetail {
	b := &beadsapi.BeadDetail{
		ID: id, Title: name, Type: "agent", Status: "in_progress", Labels: []string{"project:gardenio"},
		Fields:    map[string]string{"agent": name, "role": role},
		CreatedAt: time.Unix(baseUnix, 0), UpdatedAt: time.Unix(baseUnix+upd, 0), OperatedBy: "OIDC:Matt@Example.com",
	}
	if state != "" {
		b.Fields["agent_state"] = state
	}
	if task != "" {
		b.Fields["task_id"] = task
	}
	return b
}

// addFixtureGardeners puts the agents of the fixture farm on its plots: three
// on phase A1 (two on one task), the plan gardener at plan A's gate, one on a
// step two hops below phase A2, one standing on the phase itself, an ask and a
// crash, and the ones at home (a seed's task, a meadow bead, a closed task, no
// task, and two thread agents, one down). Pool shells, a leaving agent and a closed bead are not gardeners. The plan
// gardener's trigger is here for its spend rollup.
func addFixtureGardeners(f *fakeFarmSrc) {
	w1 := agentBead("gd-gWork1", "g-work-1", "crew", "working", "gd-A1a", 9000)
	w1.Fields["last_progress_at"] = time.Unix(baseUnix+9_437, 0).UTC().Format(time.RFC3339)
	w1.Fields["thread_id"] = "gd-thread1"
	w2 := agentBead("gd-gWork2", "g-work-2", "crew", "working", "gd-A1b", 8000)
	w2.Fields["slack_thread_bead_id"] = "gd-legacy2"
	visit := agentBead("gd-gVisit", "g-visit", "crew", "working", "gd-A1a", 8500)
	visit.Labels = []string{"project:zeta"}
	foreman := agentBead("gd-gForeman", "g-foreman", "gardener,plan-gardener", "working", "gd-planA", 7000)
	nested := agentBead("gd-gNested", "g-nested", "crew", "working", "gd-A2e", 7100)
	phase := agentBead("gd-gPhase", "g-start", "infra", "spawning", "gd-A2", 7200)
	ask := agentBead("gd-gAsk", "g-ask", "crew", "yielding", "gd-A2f", 7300)
	down := agentBead("gd-gDown", "g-down", "crew", "failed", "gd-A9", 7400)
	seed := agentBead("gd-gSeed", "g-seed", "crew", "working", "gd-S1t", 7500)
	away := agentBead("gd-gAway", "g-away", "crew", "working", "gd-M1", 7600)
	away.Labels, away.Fields["project"] = nil, "zeta"
	late := agentBead("gd-gLate", "g-late", "crew", "working", "gd-A1y", 7700)
	idle := agentBead("gd-gIdle", "g-idle", "crew", "idle", "", 7800)
	// Thread agents (5b, gd-1kFVOmUjiK): the bridge sent them to answer a thread, so no task and no plot; they stand at the
	// barn "In the Commons" on their thread head. One answering, one the stale reaper found with no room in the cluster.
	thread := agentBead("gd-gThread", "g-thread", "thread", "working", "", 7850)
	thread.Fields["thread_id"], thread.Fields["spawn_source"] = "gd-wish2", "commons-thread"
	stuck := agentBead("gd-gThreadDown", "g-thread-down", "thread", "failed", "", 7860)
	stuck.Fields["thread_id"], stuck.Fields["spawn_source"], stuck.Fields["pod_scheduling_blocked"] = "gd-wish1", "commons-thread", "true"
	stuck.Fields[beadsapi.FieldCloseReason] = "stale: unschedulable, cluster capacity"
	pool := agentBead("gd-gPool1", "prewarmed-1", "crew", "prewarmed", "", 1)
	shell := agentBead("gd-gPool2", "prewarmed-2", "crew", "queued", "", 1)
	bye := agentBead("gd-gBye", "g-bye", "crew", "done", "gd-A1b", 1)
	gone := agentBead("gd-gGone", "g-gone", "crew", "working", "gd-A1b", 1)
	gone.Status = "closed"
	// Spend (gd-0VeG81uW3w): g-work-1 with its live estimate stuck at 0 (gd-UIlh0DR2VG), g-work-2 with one counted in
	// steps, the crash with its closed sessions, and the plan gardener a run of a trigger whose rollup the farm reads.
	w1.Fields["total_cost_usd"], w1.Fields["live_cost_usd"] = "3.20", "0"
	w2.Fields["total_cost_usd"], w2.Fields["live_cost_usd"] = "1.10", "2.37"
	down.Fields["total_cost_usd"] = "4.10"
	foreman.Fields["total_cost_usd"], foreman.Fields["trigger_id"] = "0.6665", "gd-trigA"
	trig := agentBead("gd-trigA", "Plan gardener for gd-planA", "", "", "", 1)
	trig.Type, trig.Status, trig.Fields = "trigger", "open", map[string]string{"gardener_spend_usd": "40.7901"}
	f.add(w1, w2, visit, foreman, nested, phase, ask, down, seed, away, late, idle, thread, stuck, pool, shell, bye, gone, trig)
}

func TestFarmStructure(t *testing.T) {
	src := fixtureWorld()
	f := newTestFarm(src)
	if err := f.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	s := decodeSnap(t, f)

	if len(s.Plans) != 1 || len(s.Seeds) != 2 {
		t.Fatalf("plans=%d seeds=%d, want 1 and 2", len(s.Plans), len(s.Seeds))
	}
	p := findPlan(s, "gd-planA")
	if p == nil || p.Title != "Edge IdP rollout" || p.State != "building" || p.Operator != "oidc:matt@example.com" {
		t.Fatalf("plan A wrong: %+v", p)
	}
	if got := len(p.Phases); got != 3 || p.Phases[0].ID != "gd-A1" || p.Phases[2].ID != "gd-A3" {
		t.Fatalf("phases not in phase_order: %+v", p.Phases)
	}
	p1, p2, p3 := p.Phases[0], p.Phases[1], p.Phases[2]
	if p1.Title != "Foundations" || p1.Counts.InProgress != 1 || p1.Counts.Review != 1 || p1.Counts.Closed != 2 {
		t.Fatalf("phase 1 counts wrong: %+v", p1.Counts)
	}
	// The closed steps are listed too, newest change first, as closed, never among the open tasks (gd-0Fj6E2vPql).
	if len(p1.Closed) != 2 || p1.Closed[0].ID != "gd-A1y" || p1.Closed[1].ID != "gd-A1x" || p1.Closed[0].State != TaskClosed || findTask(p1.Tasks, "gd-A1x") != nil {
		t.Fatalf("phase 1 closed steps wrong: %+v", p1.Closed)
	}
	// The MR-linked in-flight task is "review" and carries its MR.
	ex := findTask(p1.Tasks, "gd-A1a")
	if ex == nil || ex.State != TaskReview || ex.MR == nil || ex.MR.IID != "!412" || ex.Assignee != "mo" {
		t.Fatalf("review task wrong: %+v", ex)
	}
	if soak := findTask(p1.Tasks, "gd-A1b"); soak == nil || soak.State != TaskInProgress {
		t.Fatalf("in-progress task wrong: %+v", soak)
	}
	// The listed tasks are planted by created, then id, and carry created, so a
	// status change never moves a cell (gd-wDdun2AVwq); the closed steps carry it too.
	for i := range p1.Tasks {
		if p1.Tasks[i].Created == 0 {
			t.Fatalf("task without created: %+v", p1.Tasks[i])
		}
		if i > 0 && (p1.Tasks[i-1].Created > p1.Tasks[i].Created || (p1.Tasks[i-1].Created == p1.Tasks[i].Created && p1.Tasks[i-1].ID > p1.Tasks[i].ID)) {
			t.Fatalf("tasks not ordered by created then id: %+v", p1.Tasks)
		}
	}
	if p1.Closed[0].Created == 0 {
		t.Fatalf("closed step without created: %+v", p1.Closed[0])
	}
	// The nested step reached its phase through the sub-epic; the sub-epic is a task too.
	if nested := findTask(p2.Tasks, "gd-A2e"); nested == nil || nested.State != TaskDeferred {
		t.Fatalf("nested step missing from phase 2: %+v", p2.Tasks)
	}
	if findTask(p2.Tasks, "gd-A2d") == nil {
		t.Fatal("non-phase sub-epic should be a task of its phase")
	}
	if p2.Counts.Open != 5 || p2.Counts.Deferred != 1 {
		t.Fatalf("phase 2 counts wrong: %+v", p2.Counts)
	}
	// A closed phase is a count-only pile: present, no tasks.
	if p3.Status != "closed" || p3.Title != "Cutover" || len(p3.Tasks) != 0 {
		t.Fatalf("closed phase wrong: %+v", p3)
	}
	// The unphased step.
	if len(p.Unphased.Tasks) != 1 || p.Unphased.Tasks[0].ID != "gd-A9" {
		t.Fatalf("unphased wrong: %+v", p.Unphased)
	}
	// Decisions surface both as a signpost task and a plan counter.
	if p.OpenDecisions != 1 || findTask(p2.Tasks, "gd-A2f") == nil || findTask(p2.Tasks, "gd-A2f").Type != "decision" {
		t.Fatalf("decision wrong: open=%d", p.OpenDecisions)
	}
	if p.Counts.Closed != 2 {
		t.Fatalf("plan closed pile = %d, want 2", p.Counts.Closed)
	}

	// Edges: blocked-by only names beads still open; the dangling / related ones drop.
	if a := findTask(p2.Tasks, "gd-A2a"); a == nil || len(a.BlockedBy) != 1 || a.BlockedBy[0] != "gd-A1a" {
		t.Fatalf("blocked_by wrong: %+v", a)
	}
	if b := findTask(p2.Tasks, "gd-A2b"); b == nil || len(b.BlockedBy) != 1 || b.BlockedBy[0] != "gd-A2a" {
		t.Fatalf("blocked_by wrong: %+v", b)
	}

	// Seeds.
	for _, sd := range s.Seeds {
		switch sd.ID {
		case "gd-seed1":
			if sd.State != SeedFrosted || sd.Phases != 1 || sd.Tasks != 2 {
				t.Fatalf("frosted seed wrong: %+v", sd)
			}
		case "gd-seed2":
			if sd.State != SeedReady || sd.Title != "Local agents" {
				t.Fatalf("ready seed wrong: %+v", sd)
			}
		default:
			t.Fatalf("unexpected seed %s", sd.ID)
		}
	}

	// Meadow: loose + non-plan parent, newest first, MR flagged; agents and epics excluded.
	if s.Meadow.Total != 3 || s.Meadow.WithMR != 1 || s.Meadow.Shown[0].ID != "gd-M2" {
		t.Fatalf("meadow wrong: %+v", s.Meadow)
	}
	if s.Meadow.Shown[1].ID != "gd-M3" || s.Meadow.Shown[1].Parent != "gd-notaplan" {
		t.Fatalf("meadow parent wrong: %+v", s.Meadow.Shown[1])
	}

	// Piles are counts only.
	if s.Piles.ClosedEscalations != 47 || s.Piles.ClosedDecisions != 8 || s.Piles.ClosedPlans != 1 {
		t.Fatalf("piles wrong: %+v", s.Piles)
	}
	if s.ClosedPlans.Total != 1 || s.ClosedPlans.Shown[0].ID != "gd-done1" || s.ClosedPlans.Shown[0].Title != "Taints" {
		t.Fatalf("closed plans wrong: %+v", s.ClosedPlans)
	}
	_, raw, _ := f.Snapshot()
	if strings.Contains(string(raw), "gd-ce1\"") || strings.Count(string(raw), "\"gd-A1x\"") != 1 {
		t.Fatal("closed beads leaked into the payload as rows (a closed step appears once, in its phase's closed list)")
	}
	if s.Meta.Schema != FarmSchema || s.Meta.Truncated {
		t.Fatalf("meta wrong: %+v", s.Meta)
	}
}

func TestFarmDiffAndPatch(t *testing.T) {
	src := fixtureWorld()
	f := newTestFarm(src)
	ctx := context.Background()
	if err := f.refresh(ctx); err != nil {
		t.Fatal(err)
	}
	v1 := f.Stats().Version

	// Identical re-read: no bump.
	if err := f.refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if f.Stats().Version != v1 {
		t.Fatal("version bumped without a change")
	}

	_, live, cancel := f.Subscribe(v1)
	defer cancel()

	// One task moves: exactly that plan is re-sent, nothing else.
	src.mutate("gd-A2b", func(b *beadsapi.BeadDetail) { b.Status = "in_progress"; b.UpdatedAt = time.Unix(baseUnix+2000, 0) })
	if err := f.refresh(ctx); err != nil {
		t.Fatal(err)
	}
	m := <-live
	var p farmPatch
	if err := json.Unmarshal(m.Data, &p); err != nil {
		t.Fatal(err)
	}
	if p.Prev != v1 || p.V != v1+1 || p.Plans == nil || len(p.Plans.Up) != 1 || len(p.Plans.Rm) != 0 {
		t.Fatalf("unexpected patch %s", m.Data)
	}
	if p.Seeds != nil || p.Meadow != nil || p.Piles != nil || p.ClosedPlans != nil {
		t.Fatalf("untouched blobs were re-sent: %s", m.Data)
	}
	if !strings.Contains(string(p.Plans.Up[0]), `"gd-planA"`) {
		t.Fatalf("patch is not plan A: %s", p.Plans.Up[0])
	}

	// A seed is approved to build: it leaves seeds and enters plans in one patch.
	src.mutate("gd-seed2", func(b *beadsapi.BeadDetail) { b.Fields["plan_state"] = "building" })
	if err := f.refresh(ctx); err != nil {
		t.Fatal(err)
	}
	m = <-live
	p = farmPatch{}
	_ = json.Unmarshal(m.Data, &p)
	if p.Seeds == nil || len(p.Seeds.Rm) != 1 || p.Seeds.Rm[0] != "gd-seed2" || p.Plans == nil || len(p.Plans.Up) != 1 {
		t.Fatalf("promotion patch wrong: %s", m.Data)
	}
}

func TestFarmReplayAndReset(t *testing.T) {
	src := fixtureWorld()
	f := newTestFarm(src)
	ctx := context.Background()
	_ = f.refresh(ctx)
	v0 := f.Stats().Version
	for i := range 3 {
		src.mutate("gd-A2b", func(b *beadsapi.BeadDetail) { b.Title = fmt.Sprintf("rename %d", i) })
		if err := f.refresh(ctx); err != nil {
			t.Fatal(err)
		}
	}
	catch, _, cancel := f.Subscribe(v0)
	cancel()
	if len(catch) != 1 || catch[0].Event != "patch" || catch[0].V != f.Stats().Version || f.Stats().Deltas != 1 {
		t.Fatalf("catch-up from v%d = %+v, want one delta patch (farm_delta.go)", v0, catch)
	}
	for _, since := range []uint64{0, 1, f.Stats().Version + 5} {
		catch, _, cancel := f.Subscribe(since)
		cancel()
		if len(catch) != 1 || catch[0].Event != "reset" {
			t.Fatalf("since=%d: want one reset, got %+v", since, catch)
		}
		var s snap
		if err := json.Unmarshal(catch[0].Data, &s); err != nil || len(s.Plans) != 1 || s.V != f.Stats().Version {
			t.Fatalf("reset is not a full snapshot: %v %s", err, catch[0].Data)
		}
	}
}

func TestFarmClosedBlockerEdgeDropsImmediately(t *testing.T) {
	src := fixtureWorld()
	f := newTestFarm(src)
	ctx := context.Background()
	_ = f.refresh(ctx)
	if a := findTask(findPlan(decodeSnap(t, f), "gd-planA").Phases[1].Tasks, "gd-A2a"); len(a.BlockedBy) != 1 {
		t.Fatalf("setup: %+v", a)
	}
	src.mutate("gd-A1a", func(b *beadsapi.BeadDetail) { b.Status = "closed" })
	_ = f.refresh(ctx)
	p := findPlan(decodeSnap(t, f), "gd-planA")
	if a := findTask(p.Phases[1].Tasks, "gd-A2a"); len(a.BlockedBy) != 0 {
		t.Fatalf("edge to a closed blocker survived: %+v", a)
	}
	// And the closed count moved in the same refresh (a removal forces the slow tier).
	if p.Phases[0].Counts.Closed != 3 {
		t.Fatalf("closed pile not refreshed on close: %+v", p.Phases[0].Counts)
	}
}

func TestFarmSlowTierCadence(t *testing.T) {
	src := fixtureWorld()
	f := newTestFarm(src)
	ctx := context.Background()
	_ = f.refresh(ctx)
	_ = f.refresh(ctx)
	_ = f.refresh(ctx)
	if got := f.Stats().SlowRefreshes; got != 1 {
		t.Fatalf("slow tier ran %d times across 3 quiet refreshes, want 1", got)
	}
	// Time passes: due again.
	f.now = func() time.Time { return time.Unix(baseUnix+10_000, 0).Add(farmSlowEvery + time.Second) }
	_ = f.refresh(ctx)
	if got := f.Stats().SlowRefreshes; got != 2 {
		t.Fatalf("slow tier did not run when due: %d", got)
	}
	// A new bead (no removal) does not trigger it.
	src.add(fb("gd-new", "task", "open", "gd-A1", 3000))
	_ = f.refresh(ctx)
	if got := f.Stats().SlowRefreshes; got != 2 {
		t.Fatalf("an added bead forced the slow tier: %d", got)
	}
}

func TestFarmErrorKeepsPreviousSnapshot(t *testing.T) {
	src := fixtureWorld()
	f := newTestFarm(src)
	_ = f.refresh(context.Background())
	v := f.Stats().Version
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	bad := &errSrc{fakeFarmSrc: src}
	f.src = bad
	if err := f.refresh(ctx); err == nil {
		t.Fatal("want error")
	}
	if f.Stats().Version != v || f.Stats().LastError == "" || len(decodeSnap(t, f).Plans) != 1 {
		t.Fatal("snapshot not preserved across a failed refresh")
	}
}

type errSrc struct{ *fakeFarmSrc }

func (e *errSrc) ListBeadsFiltered(context.Context, beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	return nil, fmt.Errorf("daemon down")
}

// bigWorld builds nPlans building plans of phases x perPhase open tasks, plus
// drafts, a meadow and closed archives.
func bigWorld(nPlans, phases, perPhase, drafts, meadow, archives int) *fakeFarmSrc {
	seq = 0
	f := newFakeFarm()
	for p := range nPlans {
		pid := fmt.Sprintf("gd-P%03d", p)
		var phs []string
		for ph := range phases {
			phs = append(phs, fmt.Sprintf("%s-ph%d", pid, ph))
		}
		f.add(planBead(pid, "building", "in_progress", phs...))
		for ph, phid := range phs {
			f.add(fb(phid, "epic", "open", pid, 10))
			for t := range perPhase {
				st := "open"
				if t%5 == 0 {
					st = "in_progress"
				}
				tb := fb(fmt.Sprintf("%s-t%d", phid, t), "task", st, phid, int64(ph*100+t))
				tb.Title = "A realistically long task title that describes the work in about eighty chars"
				tb.Assignee = "gardenio-some-crew-agent-abc"
				f.add(tb)
			}
		}
	}
	for d := range drafts {
		pid := fmt.Sprintf("gd-D%03d", d)
		st := "draft"
		if d%2 == 0 {
			st = "ready"
		}
		f.add(planBead(pid, st, "open"))
	}
	for m := range meadow {
		f.add(fb(fmt.Sprintf("gd-L%04d", m), "task", "open", "", int64(m)))
	}
	for a := range archives {
		f.add(fb(fmt.Sprintf("gd-E%04d", a), "escalation", "closed", "", 1))
	}
	return f
}

// readySeedSteps gives each of bigWorld's ready drafts (the even gd-Dnnn) a
// description longer than seedSummaryMax and steps of an 80-char title, so its
// seed carries a full summary and steps (farm_seeds.go).
func readySeedSteps(src *fakeFarmSrc, drafts, steps int) {
	for d := 0; d < drafts; d += 2 {
		pid := fmt.Sprintf("gd-D%03d", d)
		src.mutate(pid, func(b *beadsapi.BeadDetail) { b.Description = strings.Repeat("A plan's narrative, said plainly. ", 30) })
		for s := range steps {
			tb := fb(fmt.Sprintf("%s-s%d", pid, s), "task", "open", pid, 5)
			tb.Title = "A realistically long step title that describes the work in about eighty chars"
			src.add(tb)
		}
	}
}

func TestFarmRealisticScaleStaysSmall(t *testing.T) {
	// 36 building plans, 25 drafts (13 ready, each with a full summary and 6
	// steps), 47+ archived escalations, 300 loose beads, and 14 closed steps per plan, each with a merged PR and a full harvest:
	// past the document's maxClosedListed, so only the newest are listed.
	src := bigWorld(36, 5, 4, 25, 300, 60)
	addHarvests(src, 36, 5, 14)
	readySeedSteps(src, 25, 6)
	f := newTestFarm(src)
	if err := f.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	s := decodeSnap(t, f)
	if len(s.Plans) != 36 || len(s.Seeds) != 25 || s.Meadow.Total != 300 || len(s.Meadow.Shown) != maxMeadowShown {
		t.Fatalf("plans=%d seeds=%d meadow=%d/%d", len(s.Plans), len(s.Seeds), len(s.Meadow.Shown), s.Meadow.Total)
	}
	if s.Piles.ClosedEscalations != 60 {
		t.Fatalf("archive pile = %d", s.Piles.ClosedEscalations)
	}
	if n := listedClosed(s); n != maxClosedListed {
		t.Fatalf("listed closed steps = %d, want the document cap %d", n, maxClosedListed)
	}
	_, raw, _ := f.Snapshot()
	t.Logf("typical snapshot: %d bytes (%d plans, %d tasks, %d closed steps listed)", len(raw), len(s.Plans), 36*5*4, maxClosedListed)
	// About 230 KB of open work plus at most maxClosedListed closed steps at
	// about 330 B with a harvest (gd-8fjCFsoAKI), plus about 21 B of created on
	// each listed task and closed step (gd-wDdun2AVwq).
	if len(raw) > 445_000 {
		t.Fatalf("typical farm snapshot is %d bytes, budget 445000", len(raw))
	}
	// A single plan patch stays tiny.
	var maxPlan int
	for _, e := range f.plans {
		if len(e.raw) > maxPlan {
			maxPlan = len(e.raw)
		}
	}
	if maxPlan > 12_000 {
		t.Fatalf("largest plan entry %d bytes", maxPlan)
	}
}

func TestFarmWorstCaseIsBounded(t *testing.T) {
	// 40 plans x 12 phases x 30 tasks = 14400 open tasks: far past the row cap,
	// more live agents than maxGardeners, and more closed steps per plan than
	// maxClosedPerPlan, each with a full harvest.
	src := bigWorld(40, 12, 30, 25, 1000, 200)
	addHarvests(src, 40, 12, maxClosedPerPlan+16)
	for i := range maxGardeners + 20 {
		a := agentBead(fmt.Sprintf("gd-agent%04d", i), fmt.Sprintf("gardenio-some-crew-agent-%04d", i), "crew", "working",
			fmt.Sprintf("gd-P%03d-ph%d-t%d", i%40, i%12, i%30), int64(i))
		a.Fields["thread_id"] = "gd-thread"
		src.add(a)
	}
	f := newTestFarm(src)
	if err := f.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	s := decodeSnap(t, f)
	if !s.Meta.Truncated || s.Meta.Rows > farmRowCap {
		t.Fatalf("worst case must be flagged truncated and row-capped: %+v", s.Meta)
	}
	if len(s.Gardeners) != maxGardeners || !s.Meta.GardenersTruncated {
		t.Fatalf("gardeners = %d, truncated = %v; want the newest %d, flagged", len(s.Gardeners), s.Meta.GardenersTruncated, maxGardeners)
	}
	for i := range s.Plans {
		if n := len(closedRows(&s.Plans[i])); n != maxClosedPerPlan {
			t.Fatalf("plan %s lists %d closed steps, want the plan cap %d", s.Plans[i].ID, n, maxClosedPerPlan)
		}
	}
	// pages + the agent read + the closed-plans tile + the closed-step pages + the open-PR read + the two produce lists
	// (farm_produce.go) + the evidence list (farm_harvest.go)
	if want := farmRowCap/farmPageSize + 1 + farmClosedRowCap/farmPageSize + 5; src.lists > want {
		t.Fatalf("%d list calls for the fast and slow tiers, want <= %d", src.lists, want)
	}
	_, raw, _ := f.Snapshot()
	t.Logf("worst-case snapshot: %d bytes, %d list calls, %d count calls", len(raw), src.lists, src.counts)
	if len(raw) > 700_000 {
		t.Fatalf("worst-case snapshot is %d bytes, budget 700000", len(raw))
	}
	// A reconcile of a quiet farm costs at most the fast tier again, not the slow one.
	before := src.lists
	_ = f.refresh(context.Background())
	if src.lists-before > farmRowCap/farmPageSize+1 {
		t.Fatalf("quiet refresh issued %d list calls", src.lists-before)
	}
}

func TestFarmPhaseTaskCap(t *testing.T) {
	seq = 0
	src := newFakeFarm()
	src.add(planBead("gd-P", "building", "in_progress", "gd-ph"), fb("gd-ph", "epic", "open", "gd-P", 1))
	for i := range 150 {
		src.add(fb(fmt.Sprintf("gd-t%03d", i), "task", "open", "gd-ph", int64(i)))
	}
	f := newTestFarm(src)
	_ = f.refresh(context.Background())
	ph := findPlan(decodeSnap(t, f), "gd-P").Phases[0]
	if len(ph.Tasks) != maxPhaseTasks || ph.More != 150-maxPhaseTasks || ph.Counts.Open != 150 {
		t.Fatalf("cap wrong: shown=%d more=%d counts=%+v", len(ph.Tasks), ph.More, ph.Counts)
	}
}

// TestFarmHeraldry pins heraldry spec §6.5: the plan, a phase (open, or closed
// and read by id) and a task carry the bead's canonical form byte for byte, and
// a bead with no arms carries no key.
func TestFarmHeraldry(t *testing.T) {
	seq = 0
	const planArms, phaseArms, closedArms, taskArms = "blue 🧭 . 🔥 . 📦", "sand . . 🧭 . .", "green 🌲 . . . 🐢", "red . ⚙️ . 🔑 ."
	src := newFakeFarm()
	plan := planBead("gd-P", "building", "in_progress", "gd-ph", "gd-done")
	plan.Heraldry = planArms
	ph, done := fb("gd-ph", "epic", "open", "gd-P", 1), fb("gd-done", "epic", "closed", "gd-P", 1)
	ph.Heraldry, done.Heraldry = phaseArms, closedArms
	armed, bare := fb("gd-t1", "task", "open", "gd-ph", 2), fb("gd-t2", "task", "open", "gd-ph", 3)
	armed.Heraldry = taskArms
	src.add(plan, ph, done, armed, bare, fb("gd-t3", "task", "open", "gd-P", 4))
	f := newTestFarm(src)
	if err := f.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	p := findPlan(decodeSnap(t, f), "gd-P")
	if p == nil || p.Heraldry != planArms || len(p.Phases) != 2 {
		t.Fatalf("plan arms wrong: %+v", p)
	}
	if got := p.Phases[0].Heraldry; got != phaseArms {
		t.Fatalf("open phase arms = %q, want %q", got, phaseArms)
	}
	if got := p.Phases[1]; got.Status != "closed" || got.Heraldry != closedArms {
		t.Fatalf("closed phase arms = %q (%s), want %q", got.Heraldry, got.Status, closedArms)
	}
	if a := findTask(p.Phases[0].Tasks, "gd-t1"); a == nil || a.Heraldry != taskArms {
		t.Fatalf("task arms wrong: %+v", a)
	}
	_, raw, _ := f.Snapshot()
	// the plan's and the phases' arms in plain words, for the plot's and the stakes' hots (step 4.3); tasks bear none (R6)
	for _, c := range []struct{ got, arms string }{{p.HeraldryName, planArms}, {p.Phases[0].HeraldryName, phaseArms}, {p.Phases[1].HeraldryName, closedArms}} {
		a, err := heraldry.Parse(c.arms)
		if err != nil {
			t.Fatal(err)
		}
		if c.got != a.PlainName() {
			t.Errorf("heraldry_name for %q = %q, want %q", c.arms, c.got, a.PlainName())
		}
	}
	if n := strings.Count(string(raw), `"heraldry_name":`); n != 3 {
		t.Fatalf("%d heraldry_name keys in the document, want 3 (the plan and its two phases):\n%s", n, raw)
	}
	if n := strings.Count(string(raw), `"heraldry":`); n != 4 {
		t.Fatalf("%d heraldry keys in the document, want 4 (the bare task and the Other row carry none):\n%s", n, raw)
	}
}

// Every plan, phase, task and listed closed step carries its short title beside
// its title (gd-MpWy5X34hh), so the cursor's Plan › Phase › Task bar names each
// part without another read; the key is left out where a bead has none, and a
// short title that changes re-sends its plan.
func TestFarmShortTitles(t *testing.T) {
	seq = 0
	src := newFakeFarm()
	plan := planBead("gd-P", "building", "in_progress", "gd-ph", "gd-done")
	plan.ShortTitle = "steady title bar "
	ph, done := fb("gd-ph", "epic", "open", "gd-P", 1), fb("gd-done", "epic", "closed", "gd-P", 1)
	ph.ShortTitle, done.ShortTitle = "carry short titles", "research"
	named, bare := fb("gd-t1", "task", "in_progress", "gd-ph", 2), fb("gd-t2", "task", "open", "gd-ph", 3)
	named.ShortTitle = "add short_title to the farm"
	reaped := fb("gd-t4", "task", "closed", "gd-ph", 1)
	reaped.ShortTitle = "read the daemon field"
	src.add(plan, ph, done, named, bare, fb("gd-t3", "task", "open", "gd-P", 4), reaped)
	f := newTestFarm(src)
	ctx := context.Background()
	if err := f.refresh(ctx); err != nil {
		t.Fatal(err)
	}
	p := findPlan(decodeSnap(t, f), "gd-P")
	if p == nil || len(p.Phases) != 2 {
		t.Fatalf("plan wrong: %+v", p)
	}
	if p.ShortTitle != "steady title bar" || p.Title != "Title gd-P" {
		t.Fatalf("plan short_title = %q (title %q), want it trimmed beside the title", p.ShortTitle, p.Title)
	}
	if got := p.Phases[0].ShortTitle; got != "carry short titles" {
		t.Fatalf("open phase short_title = %q", got)
	}
	if got := p.Phases[1]; got.Status != "closed" || got.ShortTitle != "research" {
		t.Fatalf("closed phase short_title = %q (%s), want %q", got.ShortTitle, got.Status, "research")
	}
	if a := findTask(p.Phases[0].Tasks, "gd-t1"); a == nil || a.ShortTitle != "add short_title to the farm" || a.Title != "Title gd-t1" {
		t.Fatalf("task short_title wrong: %+v", a)
	}
	if b := findTask(p.Phases[0].Tasks, "gd-t2"); b == nil || b.ShortTitle != "" {
		t.Fatalf("a task with none carries a short title: %+v", b)
	}
	if cs := p.Phases[0].Closed; len(cs) != 1 || cs[0].ID != "gd-t4" || cs[0].ShortTitle != "read the daemon field" {
		t.Fatalf("closed step short_title wrong: %+v", cs)
	}
	if p.Unphased.ShortTitle != "" {
		t.Fatalf("Other carries a short title: %q", p.Unphased.ShortTitle)
	}
	_, raw, _ := f.Snapshot()
	// the plan, its two phases, gd-t1 and the closed step: the bare tasks and the Other row leave the key out
	if n := strings.Count(string(raw), `"short_title":`); n != 5 {
		t.Fatalf("%d short_title keys in the document, want 5:\n%s", n, raw)
	}

	_, live, cancel := f.Subscribe(f.Stats().Version)
	defer cancel()
	src.mutate("gd-t2", func(b *beadsapi.BeadDetail) { b.ShortTitle = "name the bare step" })
	if err := f.refresh(ctx); err != nil {
		t.Fatal(err)
	}
	m := <-live
	var pt farmPatch
	if err := json.Unmarshal(m.Data, &pt); err != nil {
		t.Fatal(err)
	}
	if pt.Plans == nil || len(pt.Plans.Up) != 1 {
		t.Fatalf("a short title change did not re-send its plan: %s", m.Data)
	}
	var up FarmPlan
	if err := json.Unmarshal(pt.Plans.Up[0], &up); err != nil {
		t.Fatal(err)
	}
	if len(up.Phases) == 0 || up.ShortTitle != "steady title bar" || up.Phases[0].ShortTitle != "carry short titles" {
		t.Fatalf("patched plan lost its short titles: %s", pt.Plans.Up[0])
	}
	if b := findTask(up.Phases[0].Tasks, "gd-t2"); b == nil || b.ShortTitle != "name the bare step" {
		t.Fatalf("patch does not carry the new short title: %s", pt.Plans.Up[0])
	}
}

func TestFarmRequestPathNeverCallsDaemon(t *testing.T) {
	src := fixtureWorld()
	f := newTestFarm(src)
	_ = f.refresh(context.Background())
	calls := src.lists + src.counts + src.histCalls + src.depCalls + src.getCalls

	mux := http.NewServeMux()
	(&Handler{Hub: New(newFake(), "", nil), Farm: f}).Register(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/garden/farm", nil))
	if rec.Code != 200 || !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("code %d ct %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	if !strings.HasPrefix(rec.Header().Get("Server-Timing"), "snapshot;dur=") || rec.Header().Get("ETag") == "" {
		t.Fatalf("missing Server-Timing/ETag: %q", rec.Header().Get("Server-Timing"))
	}
	var s snap
	if err := json.Unmarshal(rec.Body.Bytes(), &s); err != nil || len(s.Plans) != 1 {
		t.Fatalf("bad body: %v", err)
	}
	req := httptest.NewRequest("GET", "/garden/farm", nil)
	req.Header.Set("If-None-Match", rec.Header().Get("ETag"))
	rec2 := httptest.NewRecorder()
	mux.ServeHTTP(rec2, req)
	if rec2.Code != http.StatusNotModified || rec2.Body.Len() != 0 {
		t.Fatalf("revisit = %d, want bodyless 304", rec2.Code)
	}
	if got := src.lists + src.counts + src.histCalls + src.depCalls + src.getCalls; got != calls {
		t.Fatal("serving the farm hit the daemon")
	}
}

func TestFarmSnapshotGzipRoundTrips(t *testing.T) {
	src := bigWorld(36, 5, 4, 25, 300, 60)
	f := newTestFarm(src)
	_ = f.refresh(context.Background())
	mux := http.NewServeMux()
	(&Handler{Farm: f}).Register(mux)
	req := httptest.NewRequest("GET", "/garden/farm", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Header().Get("Content-Encoding") != "gzip" || rec.Header().Get("Vary") != "Accept-Encoding" {
		t.Fatalf("headers: %v", rec.Header())
	}
	wire := rec.Body.Len()
	zr, err := gzip.NewReader(rec.Body)
	if err != nil {
		t.Fatal(err)
	}
	plain, _ := io.ReadAll(zr)
	_, want, _ := f.Snapshot()
	if !bytes.Equal(plain, want) {
		t.Fatal("gzip body differs from the plain snapshot")
	}
	t.Logf("wire size %d bytes for %d plain", wire, len(want))
	if wire > 40_000 {
		t.Fatalf("typical farm is %d bytes on the wire, budget 40000", wire)
	}
}

func TestFarmRoutesWithoutFarmAre503(t *testing.T) {
	mux := http.NewServeMux()
	(&Handler{}).Register(mux)
	for _, p := range []string{"/garden/farm", "/garden/farm/events"} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest("GET", p, nil))
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s = %d, want 503", p, rec.Code)
		}
	}
}

func TestFarmSSEEndToEndViaHubEvents(t *testing.T) {
	src := fixtureWorld()
	hubSrc := newFake()
	hubSrc.events = src.events
	f := NewFarm(src, nil)
	h := New(hubSrc, "", nil)
	h.OnEvent(f.InvalidateFor)
	h.Start()
	defer h.Close()
	f.Start()
	defer f.Close()
	v := f.Stats().Version

	mux := http.NewServeMux()
	(&Handler{Hub: h, Farm: f}).Register(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/garden/farm/events?since=" + itoa(v))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type %q", ct)
	}

	src.add(fb("gd-A2zz", "task", "open", "gd-A2", 5000))
	start := time.Now()
	src.events <- beadsapi.SSEEvent{Event: "bead.created"}

	got := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			if strings.HasPrefix(sc.Text(), "data: ") && strings.Contains(sc.Text(), "gd-A2zz") {
				got <- sc.Text()
				return
			}
		}
	}()
	select {
	case <-got:
		if d := time.Since(start); d > 2*time.Second {
			t.Fatalf("event->browser took %s, want <= 2s", d)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("farm patch never arrived")
	}
}

// TestFarmFixtureGolden pins the JSON the art track builds against, with every
// cost field (fixtureCost): the purse and its rows here, and the purse with the
// rollup down in testdata/farm_fixture_cost_down.json. Regenerate with:
// go test ./internal/gardenhub -run FarmFixtureGolden -update-farm-fixture
func TestFarmFixtureGolden(t *testing.T) {
	src := fixtureWorld()
	f := newTestFarm(src)
	f.SetReleaseSource(src) // the truck's releases (farm_releases.go)
	sp := fixtureCost(src, f.now())
	f.SetSpendSource(sp)
	if err := f.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, raw, _ := f.Snapshot()
	checkFixtureGolden(t, raw, "testdata/farm_fixture.json")

	// The rollup goes down a minute later: every row has=false with the last
	// good read as as_of; the account budgets and the purse are still known.
	sp.mu.Lock()
	sp.down = true
	sp.mu.Unlock()
	later := f.now().Add(farmCostEvery)
	f.now = func() time.Time { return later }
	f.slow.at = time.Time{}
	if err := f.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	checkFixtureGolden(t, farmDoc(t, f)["cost"], "testdata/farm_fixture_cost_down.json")
}

// listCalls reads the list counter the loop goroutine bumps.
func (f *fakeFarmSrc) listCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lists
}

// TestFarmFreshnessLatencyAndRate pins the cadence behind the 1-2 s freshness
// target: a lone event is read within farmSettle + one refresh, and a steady
// stream of events is read at most about once per farmMinGap, so the daemon
// never sees more than the fast tier's pages per second.
func TestFarmFreshnessLatencyAndRate(t *testing.T) {
	src := fixtureWorld()
	f := NewFarm(src, nil)
	f.Start()
	defer f.Close()

	// Quiet system, one event: the next version lands well inside 2 s.
	v := f.Stats().Version
	start := time.Now()
	f.Invalidate()
	src.mu.Lock()
	src.beads = append(src.beads, fb("gd-A2yy", "task", "open", "gd-A2", 6000))
	src.mu.Unlock()
	for f.Stats().Version == v {
		if time.Since(start) > 2*time.Second {
			t.Fatalf("lone event not applied after %s", time.Since(start))
		}
		time.Sleep(10 * time.Millisecond)
	}
	if d := time.Since(start); d > 1500*time.Millisecond {
		t.Fatalf("lone event took %s, want inside the 1-2 s target", d)
	}

	// A busy minute in miniature: an event every 20 ms for 3 s.
	before := src.listCalls()
	end := time.Now().Add(3 * time.Second)
	for time.Now().Before(end) {
		f.Invalidate()
		time.Sleep(20 * time.Millisecond)
	}
	runs := (src.listCalls() - before) / 2 // one work page and one agent read per refresh in this world
	if max := int(3*time.Second/farmMinGap) + 2; runs > max {
		t.Fatalf("%d refreshes in 3 s of steady events, want <= %d", runs, max)
	}
	if runs < 2 {
		t.Fatalf("only %d refreshes in 3 s of steady events: the farm is not keeping up", runs)
	}
}

// prBead is a type=merge-request bead as github-bridge writes it: open while the PR is open.
func prBead(id, url, status string, upd int64) *beadsapi.BeadDetail {
	b := fb(id, "merge-request", status, "", upd)
	b.Labels = nil
	b.Fields["mr_url"], b.Fields["forge"], b.Fields["mr_iid"], b.Fields["project_path"] = url, "github", strings.TrimPrefix(url, "https://github.com/o/r/pull/"), "o/r"
	b.Title = "PR " + id
	return b
}

// An open PR is an orphan when no open work bead carries it: its bead is closed (bead_closed, with that bead's project and
// close time, and its last assignee for the chore tour to ping) or no work bead links it (no_bead). A PR on an open row, a merged PR and a GitLab MR bead are not orphans.
// The bead lookups are bounded per slow tick and cached while the PR stays open (gd-ytTNc3Fu9M).
func TestFarmOrphanedPRs(t *testing.T) {
	seq = 0
	src := newFakeFarm()
	live := fb("gd-live", "task", "in_progress", "", 900)
	live.Fields["mr_url"] = "https://github.com/o/r/pull/1"
	gone := fb("gd-gone", "task", "closed", "", 700)
	gone.Labels, gone.Assignee = []string{"project:music-clonk"}, "flo"
	gone.Fields["mr_url"] = "https://github.com/o/r/pull/2"
	merged := fb("gd-merged", "task", "closed", "", 600)
	merged.Fields["mr_url"] = "https://github.com/o/r/pull/4"
	gitlab := prBead("gd-mr5", "https://gitlab.com/o/r/-/merge_requests/5", "open", 500)
	gitlab.Fields["forge"] = ""
	src.add(live, gone, merged, gitlab,
		prBead("gd-mr1", "https://github.com/o/r/pull/1", "open", 800),
		prBead("gd-mr2", "https://github.com/o/r/pull/2", "open", 750),
		prBead("gd-mr3", "https://github.com/o/r/pull/3", "open", 740),
		prBead("gd-mr4", "https://github.com/o/r/pull/4", "closed", 610))
	f := newTestFarm(src)
	if err := f.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	pl := decodeSnap(t, f).Piles
	if pl.OrphanMRTotal != 2 || len(pl.OrphanMRs) != 2 {
		t.Fatalf("orphans = %d %+v, want PRs 2 and 3", pl.OrphanMRTotal, pl.OrphanMRs)
	}
	o2, o3 := pl.OrphanMRs[0], pl.OrphanMRs[1]
	if o2.URL != "https://github.com/o/r/pull/2" || o2.Why != OrphanBeadClosed || o2.Bead != "gd-gone" || o2.Assignee != "flo" || o2.Project != "music-clonk" || o2.Since != baseUnix+700 || o2.IID != "2" || o2.Repo != "o/r" || o2.Title != "PR gd-mr2" {
		t.Fatalf("closed-bead orphan wrong: %+v", o2)
	}
	if o3.URL != "https://github.com/o/r/pull/3" || o3.Why != OrphanNoBead || o3.Bead != "" || o3.Project != "" || o3.Since == 0 {
		t.Fatalf("no-bead orphan wrong: %+v", o3)
	}

	// The next slow tick reuses the cached lookups: one list for the PRs, none per orphan.
	for i := 10; i < 10+maxOrphanLookups+5; i++ {
		src.add(prBead(fmt.Sprintf("gd-mrx%d", i), fmt.Sprintf("https://github.com/o/r/pull/%d", i), "open", int64(100+i)))
	}
	tick := func() int {
		f.now = func() time.Time { return f.slow.at.Add(farmSlowEvery) }
		before := src.lists
		if err := f.refresh(context.Background()); err != nil {
			t.Fatal(err)
		}
		return src.lists - before
	}
	pages := 2 // the fast tier's one page and the agent read, then the closed-plans tile, the closed-step page and the PR list
	if got := tick(); got != pages+3+maxOrphanLookups {
		t.Fatalf("tick with %d new PRs made %d list calls, want %d", maxOrphanLookups+5, got, pages+3+maxOrphanLookups)
	}
	if pl = decodeSnap(t, f).Piles; pl.OrphanMRTotal != 2+maxOrphanLookups {
		t.Fatalf("the lookup budget should defer 5 PRs to the next tick, total %d", pl.OrphanMRTotal)
	}
	if got := tick(); got != pages+3+5 {
		t.Fatalf("second tick made %d list calls, want %d", got, pages+3+5)
	}
	if pl = decodeSnap(t, f).Piles; pl.OrphanMRTotal != 2+maxOrphanLookups+5 || len(pl.OrphanMRs) != min(pl.OrphanMRTotal, maxOrphanMRs) {
		t.Fatalf("total %d shown %d", pl.OrphanMRTotal, len(pl.OrphanMRs))
	}

	// A closed PR leaves the read; reopening the bead makes its PR carried again.
	src.mutate("gd-gone", func(b *beadsapi.BeadDetail) { b.Status = "in_progress" })
	tick()
	for _, o := range decodeSnap(t, f).Piles.OrphanMRs {
		if o.Bead == "gd-gone" {
			t.Fatal("a reopened bead's PR is still an orphan")
		}
	}
}

func TestFarmRosterEventDoesNotReadRows(t *testing.T) {
	defer func(g time.Duration) { farmRosterGap = g }(farmRosterGap)
	farmRosterGap = 200 * time.Millisecond
	f := newFakeFarm()
	fm := NewFarm(f, nil)
	fm.Start()
	defer fm.Close()
	f.mu.Lock()
	before, listsBefore := f.workLists, f.lists
	f.mu.Unlock()
	if before == 0 {
		t.Fatal("warm-up did not read the work rows")
	}
	fm.InvalidateFor(Relevance{Roster: true})
	time.Sleep(farmRosterGap + 500*time.Millisecond)
	f.mu.Lock()
	afterRoster, listsAfter := f.workLists, f.lists
	f.mu.Unlock()
	if afterRoster != before {
		t.Fatalf("roster-only event re-read the work rows: %d -> %d", before, afterRoster)
	}
	if listsAfter == listsBefore {
		t.Fatal("roster-only event did not re-read the agents")
	}
	fm.InvalidateFor(Relevance{Farm: true})
	deadline := time.Now().Add(3 * time.Second)
	for {
		f.mu.Lock()
		n := f.workLists
		f.mu.Unlock()
		if n > afterRoster {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("farm event did not re-read the work rows (positive control)")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A work event that lands while a roster refresh waits out farmRosterGap is
// not held for the gap.
func TestFarmWorkEventDoesNotWaitForRosterGap(t *testing.T) {
	defer func(g time.Duration) { farmRosterGap = g }(farmRosterGap)
	farmRosterGap = time.Minute
	f := newFakeFarm()
	fm := NewFarm(f, nil)
	fm.Start()
	defer fm.Close()
	f.mu.Lock()
	before := f.workLists
	f.mu.Unlock()
	fm.InvalidateFor(Relevance{Roster: true})
	time.Sleep(50 * time.Millisecond) // the loop is now in the roster wait
	fm.InvalidateFor(Relevance{Farm: true})
	deadline := time.Now().Add(farmMinGap + 2*time.Second)
	for {
		f.mu.Lock()
		n := f.workLists
		f.mu.Unlock()
		if n > before {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("work event waited behind the roster gap")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A step parented to BOTH its phase and the plan (what a bulk re-parent onto
// the plan root produces, gd-RCwGfF1Z4k) reports the plan as its primary
// parent; the farm must still file it under its phase, and carry anything
// nested under it along. A phase edge to ANOTHER plan's phase does not count.
func TestFarmDualParentStepLandsInItsPhase(t *testing.T) {
	src := newFakeFarm()
	src.add(
		planBead("gd-planB", "building", "open", "gd-B1", "gd-B2"),
		fb("gd-B1", "epic", "open", "gd-planB", 1),
		fb("gd-B2", "epic", "open", "gd-planB", 1),
		planBead("gd-planC", "building", "open", "gd-C1"),
		fb("gd-C1", "epic", "open", "gd-planC", 1),
		// Primary parent is the plan; the phase edge only shows in deps.
		fb("gd-B1a", "task", "open", "gd-planB", 2),
		fb("gd-B1a-sub", "task", "open", "gd-B1a", 2),
		// Two same-plan phase edges: phase_order breaks the tie (B1 first).
		fb("gd-B2a", "task", "in_progress", "gd-planB", 3),
		// Edge to a phase of a different plan: stays unphased.
		fb("gd-Bx", "task", "open", "gd-planB", 4),
		// Plain unphased step, no phase edge at all.
		fb("gd-By", "task", "open", "gd-planB", 5),
	)
	pc := func(id, to string) beadsapi.Dependency {
		return beadsapi.Dependency{BeadID: id, DependsOnID: to, Type: "parent-child"}
	}
	src.deps["gd-B1a"] = []beadsapi.Dependency{pc("gd-B1a", "gd-planB"), pc("gd-B1a", "gd-B1")}
	src.deps["gd-B2a"] = []beadsapi.Dependency{pc("gd-B2a", "gd-B2"), pc("gd-B2a", "gd-planB"), pc("gd-B2a", "gd-B1")}
	src.deps["gd-Bx"] = []beadsapi.Dependency{pc("gd-Bx", "gd-planB"), pc("gd-Bx", "gd-C1")}
	src.deps["gd-By"] = []beadsapi.Dependency{pc("gd-By", "gd-planB"), {BeadID: "gd-By", DependsOnID: "gd-B1", Type: "blocks"}}

	f := newTestFarm(src)
	if err := f.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	s := decodeSnap(t, f)
	p := findPlan(s, "gd-planB")
	if p == nil || len(p.Phases) != 2 {
		t.Fatalf("plan B wrong: %+v", p)
	}
	b1, b2 := p.Phases[0], p.Phases[1]
	if findTask(b1.Tasks, "gd-B1a") == nil || findTask(b1.Tasks, "gd-B1a-sub") == nil || findTask(b1.Tasks, "gd-B2a") == nil {
		t.Fatalf("phase B1 should hold B1a, its sub-task and B2a (first in phase_order): %+v", b1.Tasks)
	}
	if len(b2.Tasks) != 0 {
		t.Fatalf("phase B2 should be empty: %+v", b2.Tasks)
	}
	if b1.Counts.Open != 2 || b1.Counts.InProgress != 1 {
		t.Fatalf("phase B1 counts wrong: %+v", b1.Counts)
	}
	if findTask(p.Unphased.Tasks, "gd-Bx") == nil || findTask(p.Unphased.Tasks, "gd-By") == nil || len(p.Unphased.Tasks) != 2 {
		t.Fatalf("unphased should be exactly Bx and By: %+v", p.Unphased.Tasks)
	}
	if c := findPlan(s, "gd-planC"); c == nil || len(c.Phases[0].Tasks) != 0 {
		t.Fatalf("plan C must not adopt Bx: %+v", c)
	}

	// The phase-edge read rides on the row read: a roster-only refresh reuses it.
	calls := src.depCalls
	if err := f.refreshOnce(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if src.depCalls != calls {
		t.Fatalf("roster-only refresh re-read phase edges: %d -> %d dep calls", calls, src.depCalls)
	}
	// Fixing the data (dropping the redundant plan edge) keeps the same answer.
	src.mutate("gd-B1a", func(b *beadsapi.BeadDetail) { b.ParentID = "gd-B1" })
	src.mu.Lock()
	src.deps["gd-B1a"] = []beadsapi.Dependency{pc("gd-B1a", "gd-B1")}
	src.mu.Unlock()
	if err := f.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s = decodeSnap(t, f); findTask(findPlan(s, "gd-planB").Phases[0].Tasks, "gd-B1a-sub") == nil {
		t.Fatalf("sub-task lost its phase after the data fix: %+v", findPlan(s, "gd-planB").Phases[0].Tasks)
	}
}
