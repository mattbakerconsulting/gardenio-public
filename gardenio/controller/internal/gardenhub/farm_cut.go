package gardenhub

// farm_cut.go — the per-viewer cut of the farm document (gd-6UG1OGlqYg, Costs phase 3 gd-MnQlEmbUq3, decided on
// gd-QPIHOFzCXc). A project-scoped human gets the document cut to their projects: plans, seeds, closed plans, meadow
// rows, gardeners and cost rows in those projects only, with the piles, meta.rows and the purse recomputed for that
// project set. The unrestricted document is untouched: it is still composed once per version by refreshOnce and served
// from snapshotLocked, byte for byte as before (TestFarmCutAdminGolden).
//
// The cut is built on request from what refreshOnce kept beside the raw entries (farmCutState), memoised per project
// set, and never reads the daemon on the request path. The one exception to "from memory" is the archives (the closed
// counts behind the piles and the closed plans), which are per project: a request names the projects it wants and the
// next refresh reads them (refreshArchives); until then the piles are {} and the closed plans empty, as before the
// first slow read.
//
// What a scoped viewer gets (the Authorization section of docs/homestead-farm-read-model.md is the prose version):
//
//   - A plan or a seed whose project is in the set. Inside it, placement decides: a step, a closed step, a letter or a
//     produce item shows unless its own project: label names a project outside the set. A dropped step leaves the
//     phase's counts, its "+N" crate, the plan's letters and its needs_human recomputed without it.
//   - blocked_by keeps only blockers whose project is in the set (an unlabelled blocker cannot be decided, so it goes),
//     and blocked_by_project only their entries. A removed blocker leaves its project's name, never its id, in
//     blocked_by_foreign ("" for an unlabelled one), so the client can name whom the step waits on (gd-U2pbNv3lLC,
//     decided on gd-pnmEvqmfBW, option named).
//   - Meadow rows in the set; a row's parent only when the parent is a row in the set.
//   - Gardeners of a project in the set, standing on no plot or on a plan in the set, whose task, tends and
//     escalation are each unset or a bead read in the set. Their thread (chat, thread_title) is dropped and their reason
//     cut to its kind in fixed words (cutReason): a thread title, a handle or a wrap-up can name a bead outside the set.
//     Their cost is kept only on an operator land (a cost row with no purse), without gardener_usd: that rolls up crew
//     the cut cannot place in the set. A tenant land, or a land the cost blob has no row for, gets no gardener cost, as
//     it gets no at-cost dollar figure on its cost row.
//   - A plan's or a seed's cost (crew spend, gd-kcy4IFNzv5) only on an operator land (a cost row with no purse), and
//     only when the cut hides none of its steps: the figure sums the crew of every step, so a hidden step's crew would
//     be in it. Otherwise it is the unknown cost ({"has":false,"usd":null}), as a tenant land's cost row carries no
//     at-cost dollar figure. A closed plan never carries its cost in a scoped document: the cut cannot see its steps.
//   - The cost rows of the set and the purse summed over them (FarmCost.ForProjects, scope "project"), never the
//     fleet's figures or account budgets; a fleet figure added to FarmCost later is not sent until ForProjects lists
//     it. A tenant land's row carries its purse credits only (Purse.CreditsOnly), no at-cost dollar figure and no
//     level, and adds nothing to the sums.
//   - The piles and the closed plans summed over the set's own archives; the orphaned PRs of the set.
//   - meta.rows is the open rows in the set; the rest of meta is about the read, not the fleet, and stays.
//   - The open and closed wishes of the set (wishSet.build), each per-project cap as the whole farm has it. Inside a
//     wish, a kin and a produce item show only when their own project: label is in the set (an unlabelled one cannot
//     be decided, so it goes), and a plot only when its plan is; kin_total, kin_closed, a headstone's kin and tending
//     are recounted over what shows. A headstone's reason is dropped: a kd close reason can name a bead outside the
//     set. open_total counts the set's open heads; closed_total is the sum of the set's own archives (the daemon's
//     count of closed heads per project), and until they are read the set's closed heads among the kept closed rows.
//   - The releases of the set's projects (cutReleases): a project's key whole, nothing of another project's.
//   - The escalations of the set (escalationSet.build): an escalation whose project: label is in the set (an
//     unlabelled one goes) and whose plan or seed, when it has one, is in the set too (as its steps are), its executor
//     named only when the executor's own project is in the set.
//
// Fail closed: a field this file does not list is not in a scoped document.

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"slices"
	"sort"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// FarmCut is the project set a farm document is cut to. The zero value is the whole farm (an unrestricted viewer).
// Scoped with no projects is an empty, valid farm (a scoped principal with no project groups, an applicant).
type FarmCut struct {
	Scoped   bool
	Projects []string
}

// key is the cut's memo key: its projects sorted and deduplicated.
func (c FarmCut) key() (string, []string) {
	ps := slices.Clone(c.Projects)
	slices.Sort(ps)
	ps = slices.Compact(ps)
	return strings.Join(ps, "\x00"), ps
}

const (
	// farmCutMemoCap bounds the scoped documents kept (one per project set in use at the current version).
	farmCutMemoCap = 32
	// farmArchiveWantTTL is how long a project's archives are kept fresh after the last scoped request that wanted them.
	farmArchiveWantTTL = 10 * time.Minute
)

// cutPlan is one plan as refreshOnce composed it, with what a cut needs that the capped entry lost.
type cutPlan struct {
	plan FarmPlan // the unrestricted plan, capped as sent
	raw  []byte   // its entry
	// full is every open step of a phase ("" = unphased), sorted, before maxPhaseTasks.
	full map[string][]FarmTask
	// waits is every letter of the plan, sorted, before maxWaiting.
	waits []FarmWait
	// execs is each escalation's executor, by escalation id, so a cut names one only from a project of the set.
	execs map[string]execRef
	// foreign is the project: label of a closed step or a produce item in the plan whose label is not the plan's.
	foreign map[string]string
}

// cutSeed is one seed as refreshOnce composed it.
type cutSeed struct {
	seed    FarmSeed
	raw     []byte
	foreign map[string]string // a step's or the decision's project: label, where it is not the seed's
}

// cutLoose is one meadow row, newest first, with its parent's project when the parent is an open row.
type cutLoose struct {
	t           FarmTask // Parent set
	parentProj  string
	parentKnown bool
}

// cutGardener is one gardener with the projects of the beads it names (task, tends, escalation): "" for a bead with
// no project: label or one the refresh did not read, which no cut admits.
type cutGardener struct {
	g    FarmGardener
	refs []string
}

// farmCutState is what refreshOnce keeps for the cut, replaced whole under f.mu and never mutated after.
type farmCutState struct {
	plans map[string]*cutPlan
	seeds map[string]*cutSeed
	loose []cutLoose
	// gardenerSet is the gardener set as last read well (refreshOnce keeps it across a failed read); composeCut
	// turns it into gardeners.
	gardenerSet map[string]FarmGardener
	gardeners   map[string]cutGardener
	orphans     []FarmOrphanMR // every orphaned PR found, before maxOrphanMRs
	wishes      *wishSet       // the wishes before the per-project caps (farm_wishes.go)
	letters     map[string]int // open decisions and escalations, by project (farm_home.go)
	escalations *escalationSet // the escalations before the cap (farm_escalations.go)
	releases    FarmReleases   // every project's releases (farm_releases.go); nil before the first read
	rows        map[string]int // open rows read, by project
	meta        FarmMeta
}

// projectArchive is one project's closed counts and newest closed plans (refreshArchives).
type projectArchive struct {
	at     time.Time
	piles  FarmPiles // the four counts only
	closed FarmClosedPlans
	wishes int // closed thread heads (FarmWishes.ClosedTotal)
}

// cutMemo is one scoped document: at its stream's version v, cut generation gen and activity epoch act.
type cutMemo struct {
	v, gen, act uint64
	at          time.Time
	b, gz       []byte
	etag        string
}

// cutView is the farm cut to one project set: the parts of its document, every blob set (an unread one is its
// empty value), so a patch can always carry a blob's new value.
type cutView struct {
	plans, seeds, gardeners                                                     map[string]entry
	closedPlans, meadow, wishes, home, escalations, releases, piles, meta, cost []byte
}

// labelIndex is every bead the refresh read, by id, to its project: label ("" when it has none).
func labelIndex(rows, closedRows []*beadsapi.BeadDetail) map[string]string {
	out := make(map[string]string, len(rows)+len(closedRows))
	for _, b := range closedRows {
		out[b.ID] = projectOf(b)
	}
	for _, b := range rows {
		out[b.ID] = projectOf(b)
	}
	return out
}

// foreignOf records into m the label of each id whose label is known, set, and not home.
func foreignOf(m map[string]string, labels map[string]string, home string, ids ...string) map[string]string {
	for _, id := range ids {
		if p, ok := labels[id]; ok && id != "" && p != "" && p != home {
			if m == nil {
				m = map[string]string{}
			}
			m[id] = p
		}
	}
	return m
}

// composeCut fills the per-plan and per-seed foreign labels and the row counts. It runs in refresh, before the state
// is published.
func composeCut(st *farmCutState, rows, closedRows []*beadsapi.BeadDetail) {
	labels := labelIndex(rows, closedRows)
	for _, cp := range st.plans {
		home := cp.plan.Project
		add := func(ph *FarmPhase) {
			for _, c := range ph.Closed {
				cp.foreign = foreignOf(cp.foreign, labels, home, c.ID)
			}
		}
		for i := range cp.plan.Phases {
			add(&cp.plan.Phases[i])
		}
		add(&cp.plan.Unphased)
		for _, pr := range cp.plan.Produce {
			cp.foreign = foreignOf(cp.foreign, labels, home, pr.ID, pr.Task)
		}
	}
	for _, cs := range st.seeds {
		for _, s := range cs.seed.Steps {
			cs.foreign = foreignOf(cs.foreign, labels, cs.seed.Project, s.ID)
		}
		if cs.seed.Decision != nil {
			cs.foreign = foreignOf(cs.foreign, labels, cs.seed.Project, cs.seed.Decision.ID)
		}
	}
	st.gardeners = make(map[string]cutGardener, len(st.gardenerSet))
	for id, g := range st.gardenerSet {
		cg := cutGardener{g: g}
		for _, ref := range []string{g.Task, g.Tends, g.Escalation} {
			if ref != "" {
				cg.refs = append(cg.refs, labels[ref])
			}
		}
		st.gardeners[id] = cg
	}
	st.rows = map[string]int{}
	for _, b := range rows {
		if p := projectOf(b); p != "" {
			st.rows[p]++
		}
	}
}

// ---- cutting --------------------------------------------------------------

// allowIn is the cut's membership test.
func allowIn(projects []string) func(string) bool {
	set := make(map[string]bool, len(projects))
	for _, p := range projects {
		set[p] = true
	}
	return func(p string) bool { return p != "" && set[p] }
}

// cutBlockers keeps the blockers whose project the cut admits: a blocker's project is its blocked_by_project entry,
// else the step's own. A blocker it removes is named by its project only, in blocked_by_foreign (gd-U2pbNv3lLC): one
// entry per project, sorted, "" for an unlabelled one. changed is false when every blocker stays.
func cutBlockers(t FarmTask, allow func(string) bool) (FarmTask, bool) {
	var kept, foreign []string
	for _, id := range t.BlockedBy {
		p, ok := t.BlockedByProject[id]
		if !ok {
			p = t.Project
		}
		if allow(p) {
			kept = append(kept, id)
		} else {
			foreign = append(foreign, p)
		}
	}
	if len(kept) == len(t.BlockedBy) {
		return t, false
	}
	slices.Sort(foreign)
	t.BlockedBy, t.BlockedByForeign = kept, slices.Compact(foreign)
	var bp map[string]string
	for _, id := range kept {
		if p, ok := t.BlockedByProject[id]; ok {
			if bp == nil {
				bp = map[string]string{}
			}
			bp[id] = p
		}
	}
	t.BlockedByProject = bp
	return t, true
}

// cut is the plan as the cut sees it: its entry unchanged when nothing in it is outside the cut.
func (cp *cutPlan) cut(allow func(string) bool, operator bool) []byte {
	keepID := func(id string) bool {
		p, ok := cp.foreign[id]
		return !ok || allow(p)
	}
	p := cp.plan
	changed := false
	dropped := map[string]bool{}
	hidClosed := false
	var letters [2]int // open decisions, open escalations among the steps kept
	cutPhase := func(ph FarmPhase) FarmPhase {
		var kept []FarmTask
		moved := false
		for _, t := range cp.full[ph.ID] {
			if t.Project != "" && !allow(t.Project) {
				dropped[t.ID], moved = true, true
				continue
			}
			if t2, ok := cutBlockers(t, allow); ok {
				t, moved = t2, true
			}
			switch t.Type {
			case "decision":
				letters[0]++
			case "escalation":
				letters[1]++
			}
			kept = append(kept, t)
		}
		var closed []FarmClosedStep
		for _, c := range ph.Closed {
			if keepID(c.ID) {
				closed = append(closed, c)
			} else {
				hidClosed = true
			}
		}
		if !moved && len(closed) == len(ph.Closed) {
			return ph
		}
		changed = true
		counts := FarmCounts{Closed: ph.Counts.Closed}
		for _, t := range kept {
			counts.add(t.State)
		}
		more := 0
		if len(kept) > maxPhaseTasks {
			more, kept = len(kept)-maxPhaseTasks, kept[:maxPhaseTasks]
		}
		if kept == nil {
			kept = []FarmTask{}
		}
		ph.Tasks, ph.More, ph.Counts, ph.Closed = kept, more, counts, closed
		return ph
	}
	phases := make([]FarmPhase, len(p.Phases))
	for i, ph := range p.Phases {
		phases[i] = cutPhase(ph)
	}
	unphased := cutPhase(p.Unphased)
	var produce []FarmProduce
	for _, pr := range p.Produce {
		if keepID(pr.ID) && keepID(pr.Task) && !dropped[pr.ID] && !dropped[pr.Task] {
			produce = append(produce, pr)
		}
	}
	if len(produce) != len(p.Produce) {
		changed = true
	}
	dropCost := !operator || len(dropped) > 0 || hidClosed
	if !changed {
		waiting, blanked := cp.cutExecutors(p.Waiting, allow)
		if !blanked && (!dropCost || !p.Cost.Has) {
			return cp.raw
		}
		if dropCost {
			p.Cost = FarmPlanCost{}
		}
		p.Waiting = waiting
		raw, _ := json.Marshal(p)
		return raw
	}
	if dropCost {
		p.Cost = FarmPlanCost{}
	}
	p.Phases, p.Unphased, p.Produce = phases, unphased, produce
	p.Counts = FarmCounts{}
	for _, ph := range phases {
		p.Counts.sum(ph.Counts)
	}
	p.Counts.sum(unphased.Counts)
	if len(dropped) > 0 {
		p.OpenDecisions, p.OpenEscalations = letters[0], letters[1]
		p.NeedsHuman, p.Waiting = finishWaits(cp.keptWaits(dropped))
	}
	p.Waiting, _ = cp.cutExecutors(p.Waiting, allow)
	raw, _ := json.Marshal(p)
	return raw
}

// keptWaits is every letter of the plan but the dropped ones, sorted, before maxWaiting.
func (cp *cutPlan) keptWaits(dropped map[string]bool) []FarmWait {
	var ws []FarmWait
	for _, w := range cp.waits {
		if !dropped[w.ID] {
			ws = append(ws, w)
		}
	}
	return ws
}

// cutExecutors is ws with an escalation's executor blanked where the executor's own project is not in the set, as
// escalationSet.build does; true when it blanked one (ws itself is never written: it is the uncut plan's).
func (cp *cutPlan) cutExecutors(ws []FarmWait, allow func(string) bool) ([]FarmWait, bool) {
	var out []FarmWait
	for i, w := range ws {
		if w.Executor == "" {
			continue
		}
		if ex := cp.execs[w.ID]; ex.project != "" && allow(ex.project) {
			continue
		}
		if out == nil {
			out = slices.Clone(ws)
		}
		out[i].Executor = ""
	}
	if out == nil {
		return ws, false
	}
	return out, true
}

// cut is the seed as the cut sees it.
func (cs *cutSeed) cut(allow func(string) bool, operator bool) []byte {
	if len(cs.foreign) == 0 && (operator || !cs.seed.Cost.Has) {
		return cs.raw
	}
	keep := func(id string) bool {
		p, ok := cs.foreign[id]
		return !ok || allow(p)
	}
	s := cs.seed
	var steps []FarmSeedStep
	for _, st := range s.Steps {
		if keep(st.ID) {
			steps = append(steps, st)
		}
	}
	changed := len(steps) != len(s.Steps)
	if (!operator || changed) && s.Cost.Has {
		s.Cost, changed = FarmSlimCost{}, true
	}
	s.Steps = steps
	if s.Decision != nil && !keep(s.Decision.ID) {
		s.Decision, changed = nil, true
	}
	if !changed {
		return cs.raw
	}
	raw, _ := json.Marshal(s)
	return raw
}

// cut is the gardener's entry as the cut sees it, nil when the gardener is not in it.
func (cg cutGardener) cut(st *farmCutState, allow func(string) bool, operator map[string]bool) []byte {
	g := cg.g
	if !allow(g.Project) {
		return nil
	}
	if g.Plot != nil {
		if cp := st.plans[g.Plot.Plan]; cp == nil || !allow(cp.plan.Project) {
			return nil
		}
	}
	for _, p := range cg.refs {
		if !allow(p) {
			return nil
		}
	}
	g.Chat, g.ThreadTitle, g.Reason = "", "", cutReason(g.Reason)
	g.Cost = cutGardenerCost(g.Cost, operator[g.Project])
	raw, _ := json.Marshal(g)
	return raw
}

// cutGardenerCost is a gardener's cost as a scoped viewer sees it: on an operator land its own figure without
// gardener_usd, its words rewritten to match; otherwise nil.
func cutGardenerCost(c *FarmGardenerCost, operator bool) *FarmGardenerCost {
	if c == nil || !operator {
		return nil
	}
	out := *c
	out.GardenerUSD = nil
	if out.SpentUSD == nil && !out.AtLeast {
		return nil
	}
	out.Text = gardenerCostText(&out)
	return &out
}

// cutReason is a gardener's reason as a scoped viewer gets it: its kind in fixed words, never its handle's link or a
// wrap-up's free text, either of which can name a bead outside the set (gd-U2pbNv3lLC). A down gardener keeps the head
// reasonOf chose from a fixed vocabulary (before ": "), a stale or starting one its words (no bead in them). A kind this
// does not list is dropped.
func cutReason(r *FarmReason) *FarmReason {
	if r == nil {
		return nil
	}
	out := &FarmReason{Kind: r.Kind}
	switch r.Kind {
	case ReasonDown:
		out.Text, _, _ = strings.Cut(r.Text, ": ")
	case ReasonParked:
		out.Text = "Waiting on a person"
	case ReasonSleeping:
		out.Text = "Sleeping"
	case ReasonStale, ReasonStarting:
		out.Text = r.Text
	default:
		return nil
	}
	return out
}

// operatorLands is the projects whose row of the cost blob carries no purse. A tenant land, and every land while the
// farm has no blob, is not in it.
func operatorLands(raw []byte) map[string]bool {
	var c FarmCost
	if len(raw) == 0 || json.Unmarshal(raw, &c) != nil {
		return nil
	}
	m := make(map[string]bool, len(c.Projects))
	for _, row := range c.Projects {
		if row.Purse == nil {
			m[row.Project] = true
		}
	}
	return m
}

// cutCost is the cost blob cut to the set (FarmCost.ForProjects): its rows only, a tenant land's in credits, and the
// purse summed over the set's rows in dollars, never the fleet's figures or account budgets.
func cutCost(raw []byte, allow func(string) bool) []byte {
	if len(raw) == 0 {
		return nil
	}
	var c FarmCost
	if json.Unmarshal(raw, &c) != nil {
		return nil
	}
	b, _ := json.Marshal(c.ForProjects(allow))
	return b
}

// cutViewLocked is the farm cut to projects (sorted, deduplicated) at the current cut generation: the parts of the
// scoped document and of its stream (farm_cut_stream.go). ask is whether an archive not read yet asks for a refresh;
// a refresh cutting its own views passes false, since the next one reads it anyway.
func (f *Farm) cutViewLocked(projects []string, now time.Time, ask bool) cutView {
	allow := allowIn(projects)
	st := f.cutSt
	if st == nil {
		st = &farmCutState{}
	}

	operator := operatorLands(f.cost)
	plans := map[string]entry{}
	for id, cp := range st.plans {
		if allow(cp.plan.Project) {
			plans[id] = entry{raw: cp.cut(allow, operator[cp.plan.Project]), key: f.plans[id].key}
		}
	}
	seeds := map[string]entry{}
	for id, cs := range st.seeds {
		if allow(cs.seed.Project) {
			seeds[id] = entry{raw: cs.cut(allow, operator[cs.seed.Project]), key: f.seeds[id].key}
		}
	}
	gardeners := map[string]entry{}
	var homeGardeners []FarmGardener
	for id := range f.gardeners {
		if raw := st.gardeners[id].cut(st, allow, operator); raw != nil {
			gardeners[id] = entry{raw: raw}
			homeGardeners = append(homeGardeners, st.gardeners[id].g)
		}
	}

	meadow := FarmMeadow{Shown: []FarmTask{}}
	for _, l := range st.loose {
		if !allow(l.t.Project) {
			continue
		}
		meadow.Total++
		if l.t.MR != nil {
			meadow.WithMR++
		}
		if len(meadow.Shown) < maxMeadowShown {
			t := l.t
			if !l.parentKnown || !allow(l.parentProj) {
				t.Parent = ""
			}
			meadow.Shown = append(meadow.Shown, t)
		}
	}
	meadowRaw, _ := json.Marshal(meadow)

	meta := st.meta
	meta.Rows = 0
	for _, p := range projects {
		meta.Rows += st.rows[p]
	}
	metaRaw, _ := json.Marshal(meta)

	var pilesRaw, closedRaw []byte
	archived := f.wantArchivesLocked(projects, now, ask)
	if archived {
		var piles FarmPiles
		closed := FarmClosedPlans{Shown: []FarmClosedPlan{}}
		for _, p := range projects {
			a := f.arch[p]
			piles.ClosedDecisions += a.piles.ClosedDecisions
			piles.ClosedEscalations += a.piles.ClosedEscalations
			piles.ClosedWork += a.piles.ClosedWork
			piles.ClosedPlans += a.closed.Total
			closed.Total += a.closed.Total
			closed.Shown = append(closed.Shown, a.closed.Shown...)
		}
		sort.SliceStable(closed.Shown, func(i, j int) bool {
			if closed.Shown[i].Updated != closed.Shown[j].Updated {
				return closed.Shown[i].Updated > closed.Shown[j].Updated
			}
			return closed.Shown[i].ID < closed.Shown[j].ID
		})
		if len(closed.Shown) > maxClosedPlansShown {
			closed.Shown = closed.Shown[:maxClosedPlansShown]
		}
		for i := range closed.Shown { // a copy: the archive's own entries keep their cost
			closed.Shown[i].Cost = FarmSlimCost{}
		}
		for _, o := range st.orphans {
			if allow(o.Project) {
				piles.OrphanMRTotal++
				if len(piles.OrphanMRs) < maxOrphanMRs {
					piles.OrphanMRs = append(piles.OrphanMRs, o)
				}
			}
		}
		pilesRaw, _ = json.Marshal(piles)
		closedRaw, _ = json.Marshal(closed)
	}

	wishes := FarmWishes{Open: []FarmWish{}, Closed: []FarmClosedWish{}}
	if ws := st.wishes; ws != nil {
		closedTotal := 0
		if archived {
			for _, p := range projects {
				closedTotal += f.arch[p].wishes
			}
		} else {
			for _, cw := range ws.closed {
				if allow(cw.w.Project) {
					closedTotal++
				}
			}
		}
		plotOK := func(p FarmPlot) bool {
			cp := st.plans[p.Plan]
			return cp != nil && allow(cp.plan.Project)
		}
		wishes = ws.build(allow, plotOK, closedTotal)
	}
	wishesRaw, _ := json.Marshal(wishes)
	escalationsRaw, _ := json.Marshal(st.cutEscalations(allow))
	releasesRaw, _ := json.Marshal(cutReleases(st.releases, allow))

	return cutView{
		plans: plans, seeds: seeds, gardeners: gardeners,
		closedPlans: orRaw(closedRaw, `{"total":0,"shown":[]}`),
		meadow:      meadowRaw,
		wishes:      wishesRaw,
		home:        homesRaw(st.letters, st.loose, homeGardeners, wishes, allow),
		escalations: escalationsRaw,
		releases:    releasesRaw,
		piles:       orRaw(pilesRaw, `{}`),
		meta:        metaRaw,
		cost:        cutCost(f.cost, allow),
	}
}

// cutEscalations is the escalations as the set sees them: a plan or seed is in the set when its project is.
func (st *farmCutState) cutEscalations(allow func(string) bool) FarmEscalations {
	return st.escalations.build(allow, func(id string) bool {
		if cp := st.plans[id]; cp != nil {
			return allow(cp.plan.Project)
		}
		cs := st.seeds[id]
		return cs != nil && allow(cs.seed.Project)
	})
}

// CutSnapshot is the document cut to cut, its ETag, its gzip and when the farm was built. An unscoped cut is the
// whole farm, exactly as Snapshot and SnapshotGzip serve it. It performs no I/O.
func (f *Farm) CutSnapshot(cut FarmCut) (etag string, body, gz []byte, built time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !cut.Scoped {
		v, b := f.snapshotLocked()
		if f.memo.gz == nil {
			f.memo.gz = gzipOf(b)
		}
		return farmTag(v, f.memo.act), b, f.memo.gz, f.builtAt
	}
	cs := f.cutStreamLocked(cut)
	cs.used = f.now()
	m := f.cutDocLocked(cs)
	if m.gz == nil {
		m.gz = gzipOf(m.b)
	}
	return m.etag, m.b, m.gz, f.builtAt
}

func gzipOf(b []byte) []byte {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, _ = zw.Write(b)
	_ = zw.Close()
	return buf.Bytes()
}

// ---- archives -------------------------------------------------------------

// wantArchivesLocked marks the projects' archives as wanted and reports whether every one has been read. With ask, a
// project never read asks for a refresh, so the archives follow within the settle window.
func (f *Farm) wantArchivesLocked(projects []string, now time.Time, ask bool) bool {
	if f.archWant == nil {
		f.archWant = map[string]time.Time{}
	}
	all := true
	for _, p := range projects {
		f.archWant[p] = now
		if _, ok := f.arch[p]; !ok {
			all = false
		}
	}
	if !all && ask {
		f.Invalidate()
	}
	return all
}

// refreshArchives reads the closed counts and the newest closed plans of every project a scoped request wanted
// within farmArchiveWantTTL, at most every farmSlowEvery each. A failed read keeps that project's last archive; it
// never fails the refresh. Nothing is read while no scoped viewer is about.
func (f *Farm) refreshArchives(ctx context.Context) {
	now := f.now()
	f.mu.Lock()
	var due []string
	for p, at := range f.archWant {
		if now.Sub(at) >= farmArchiveWantTTL {
			delete(f.archWant, p)
			delete(f.arch, p)
			continue
		}
		if a, ok := f.arch[p]; !ok || now.Sub(a.at) >= farmSlowEvery {
			due = append(due, p)
		}
	}
	f.mu.Unlock()
	sort.Strings(due)
	got := map[string]projectArchive{}
	for _, p := range due {
		a, err := f.readArchive(ctx, p)
		if err != nil {
			f.logger.Warn("garden farm: reading a project's archives failed", "project", p, "error", err)
			continue
		}
		got[p] = a
	}
	if len(got) == 0 {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.arch == nil {
		f.arch = map[string]projectArchive{}
	}
	for p, a := range got {
		f.arch[p] = a
	}
	f.cutGen++
}

// readArchive reads one project's closed counts, newest closed plans and closed wishes count: the queries refreshSlow
// and countClosedWishes make for the fleet, with the project's label.
func (f *Farm) readArchive(ctx context.Context, project string) (projectArchive, error) {
	label := []string{"project:" + project}
	a := projectArchive{at: f.now()}
	var err error
	if a.piles.ClosedDecisions, err = f.count(ctx, beadsapi.ListBeadsQuery{Types: []string{"decision"}, Statuses: []string{"closed"}, Labels: label}); err != nil {
		return a, err
	}
	if a.piles.ClosedEscalations, err = f.count(ctx, beadsapi.ListBeadsQuery{Types: []string{"escalation"}, Statuses: []string{"closed"}, Labels: label}); err != nil {
		return a, err
	}
	if a.piles.ClosedWork, err = f.count(ctx, beadsapi.ListBeadsQuery{Types: []string{"task", "feature", "bug", "chore"}, Statuses: []string{"closed"}, Labels: label}); err != nil {
		return a, err
	}
	if a.closed, err = f.readClosedPlans(ctx, label...); err != nil {
		return a, err
	}
	a.piles.ClosedPlans = a.closed.Total
	if a.wishes, err = f.count(ctx, beadsapi.ListBeadsQuery{Types: []string{"task"}, Labels: []string{wishLabel, label[0]}, Statuses: []string{"closed"}}); err != nil {
		return a, err
	}
	return a, nil
}
