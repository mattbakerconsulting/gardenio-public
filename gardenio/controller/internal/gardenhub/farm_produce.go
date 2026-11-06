package gardenhub

import (
	"context"
	"sort"
	"strings"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/forgebeads"
)

// The produce bin (owner ask, Slack thread gd-HqcG2zcwnJ): what a plan has
// produced, or is about to. The farm's rows are open work only and nothing
// closed is ever sent as a bead, so a merged MR on a finished step, a bundle or
// a report is invisible to the document without this read. It runs in the
// slow tier: two lists across every status (work beads carrying an mr_url, and
// the bundle and report beads), each row walked up to its plan by parent-child
// edges. Open ancestors are in the rows; a closed one (a finished step, a
// closed sub-epic, a closed phase) is looked up once by its forward
// parent-child edge and cached, because closed is terminal.
const (
	// maxProduce bounds the items listed per plan (FarmPlan.Produce); the rest
	// are counted in FarmPlan.ProduceMore. It is a safety bound, not a page:
	// the bin lists every row and scrolls (owner ask gd-wgeINbOQMC, "can't
	// scroll entire list, many elided", after the 24 of gd-HqcG2zcwnJ), so it
	// sits above any real plan's produce, under the scan bound. The kind order
	// (produceLess) keeps the documents ahead of a cap, which drops the
	// oldest merge requests.
	maxProduce = 300
	// maxProduceScan bounds each of the two produce lists per slow tick
	// (newest first, so a truncated read drops the oldest produce).
	maxProduceScan = 600
	// maxProduceLookups bounds the closed ancestors resolved per slow tick. An
	// item whose lineage is not yet known waits for a later tick rather than
	// being guessed; the cache makes each lookup a one-off.
	maxProduceLookups = 60
	// maxProduceMRLookups bounds the steps whose merge request bead is read per
	// slow tick (resolveMRs). A merged or closed answer is cached, so after the
	// first tick only the open MRs are read again.
	maxProduceMRLookups = 200
)

// Kinds of produce (FarmProduce.Kind).
const (
	ProduceMR     = "mr"
	ProduceBundle = "bundle"
	ProduceReport = "report"
)

// produceTypes are the bead types that are produce in their own right (a
// merge request is read from the mr_url on the step that produced it).
var produceTypes = []string{"bundle", "report"}

// produceWorkTypes are the step types whose mr_url is a merge request the plan
// produced. A decision or an escalation never carries one.
var produceWorkTypes = []string{"task", "feature", "bug", "chore", "epic", "molecule"}

// produceStatuses is every status: produce outlives the step that made it.
var produceStatuses = append(append([]string{}, farmOpenStatuses...), "closed")

// produceRank orders the kinds in a bin (owner ask gd-wgeINbOQMC, "show
// reports before MRs"): the plan's documents first, a report (the most
// interesting) before a bundle, then its merge requests.
func produceRank(kind string) int {
	switch kind {
	case ProduceReport:
		return 0
	case ProduceBundle:
		return 1
	case ProduceMR:
		return 2
	}
	return 3
}

// produceLess orders a bin: by kind (produceRank), within a kind not ready
// first (an expected MR ahead of the merged ones), then newest first.
func produceLess(a, b FarmProduce) bool {
	if ra, rb := produceRank(a.Kind), produceRank(b.Kind); ra != rb {
		return ra < rb
	}
	if a.Ready != b.Ready {
		return !a.Ready
	}
	if a.Updated != b.Updated {
		return a.Updated > b.Updated
	}
	return a.ID < b.ID
}

// mrProduce is the merge request a step carries, or nil: no mr_url, or an MR
// that was closed without merging (compost, not produce). It is ready once
// merged; an opened or draft MR is expected produce, grayed in the bin.
func mrProduce(b *beadsapi.BeadDetail) *FarmProduce {
	mr := mrOf(b)
	if mr == nil {
		return nil
	}
	state := strings.ToLower(strings.TrimSpace(mr.State))
	merged := state == "merged" || strings.EqualFold(strings.TrimSpace(b.Fields["mr_merged"]), "true")
	if merged {
		state = "merged"
	} else if state == "closed" {
		return nil
	} else if state == "" {
		state = "opened"
	}
	title := strings.TrimSpace(b.Fields["mr_title"])
	if title == "" {
		title = b.Title
	}
	return &FarmProduce{
		ID: b.ID, Kind: ProduceMR, Title: title, URL: mr.URL, IID: mr.IID, State: state, Ready: merged,
		Task: b.ID, Updated: b.UpdatedAt.Unix(),
	}
}

// docProduce is a bundle or report bead as produce. It is ready by existing: a
// document is produced when it is written, whatever its bead status says.
func docProduce(b *beadsapi.BeadDetail) *FarmProduce {
	kind := ProduceBundle
	if b.Type == "report" {
		kind = ProduceReport
	}
	return &FarmProduce{ID: b.ID, Kind: kind, Title: b.Title, State: b.Status, Ready: true, Updated: b.UpdatedAt.Unix()}
}

// produceLineage resolves rows to plans. Open containers come from the rows;
// closed ones are learned by one forward parent-child lookup each and kept in
// the farm's cache across ticks.
type produceLineage struct {
	planSet  map[string]*beadsapi.BeadDetail
	phaseIdx map[string]string
	byID     map[string]*beadsapi.BeadDetail
	cache    map[string]string // closed container id -> its parent id ("" = none)
}

// parentOf is the next hop up, and whether it is known.
func (l *produceLineage) parentOf(id string) (string, bool) {
	if b := l.byID[id]; b != nil {
		return b.ParentID, true
	}
	p, ok := l.cache[id]
	return p, ok
}

// walk climbs from b to its plan. It returns the plan and phase ids, the step
// the row hangs off (its parent; "" when that is the plan or a phase itself),
// and, when the climb met an ancestor it does not know, that ancestor's id
// (plan "" then). A step nested under a sub-epic is a task of its phase, as
// the plot draws it, so the direct parent is the cell to zoom to.
func (l *produceLineage) walk(b *beadsapi.BeadDetail) (plan, phase, step, unknown string) {
	at := b.ParentID
	for depth := 0; at != "" && depth < farmMaxDepth; depth++ {
		if pid, ok := l.phaseIdx[at]; ok {
			if depth > 0 {
				step = b.ParentID
			}
			return pid, at, step, ""
		}
		if _, ok := l.planSet[at]; ok {
			if depth > 0 {
				step = b.ParentID
			}
			return at, "", step, ""
		}
		next, ok := l.parentOf(at)
		if !ok {
			return "", "", "", at
		}
		at = next
	}
	return "", "", "", ""
}

// readProduce lists the produce of every plan in planSet: FarmPlan.Produce and
// FarmPlan.ProduceMore keyed by plan id, the steps with ready produce hanging
// off them (the harvest's yield, uncapped), and the merged MRs of each wish's
// kin keyed by the wish (wishProduce). A row that reaches no plan (meadow
// produce, a seed's) is not produce of any plan, but it is still its wish's.
// A farm with no plan reads nothing, so its headstones list no MRs.
func (f *Farm) readProduce(ctx context.Context, planSet map[string]*beadsapi.BeadDetail, phaseIdx map[string]string, byID map[string]*beadsapi.BeadDetail) (map[string][]FarmProduce, map[string]int, map[string]bool, wishYield, error) {
	if len(planSet) == 0 {
		return map[string][]FarmProduce{}, map[string]int{}, map[string]bool{}, wishYield{}, nil
	}
	mrs, err := f.src.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types: produceWorkTypes, Statuses: produceStatuses, Fields: map[string]string{"mr_url": "*"},
		WithParent: true, Sort: "-updated_at", Limit: maxProduceScan, Omit: farmRowOmit, OmitFields: farmClosedOmitFields,
	})
	if err != nil {
		return nil, nil, nil, wishYield{}, err
	}
	mrRows, err := f.resolveMRs(ctx, mrs.Beads)
	if err != nil {
		return nil, nil, nil, wishYield{}, err
	}
	docs, err := f.src.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types: produceTypes, Statuses: produceStatuses,
		WithParent: true, Sort: "-updated_at", Limit: maxProduceScan, Omit: farmRowOmit, OmitFields: farmClosedOmitFields,
	})
	if err != nil {
		return nil, nil, nil, wishYield{}, err
	}
	rows := make([]*beadsapi.BeadDetail, 0, len(mrRows)+len(docs.Beads))
	for _, b := range mrRows {
		if b != nil && b.ParentID != "" {
			rows = append(rows, b)
		}
	}
	for _, b := range docs.Beads {
		if b != nil && b.ParentID != "" {
			rows = append(rows, b)
		}
	}
	if f.slow.lineage == nil {
		f.slow.lineage = map[string]string{}
	}
	lin := &produceLineage{planSet: planSet, phaseIdx: phaseIdx, byID: byID, cache: f.slow.lineage}

	// Climb, collect the ancestors nobody knows, learn them in one batched edge
	// read, climb again: one round per unknown hop, at most farmMaxDepth rounds
	// and maxProduceLookups ids per tick. Every answer is cached, so a chain is
	// learned once; a chain past the budget finishes on a later tick.
	lookups := 0
	for round := 0; round < farmMaxDepth && lookups < maxProduceLookups; round++ {
		var missing []string
		seenMissing := map[string]bool{}
		for _, b := range rows {
			if _, _, _, unk := lin.walk(b); unk != "" && !seenMissing[unk] {
				seenMissing[unk] = true
				missing = append(missing, unk)
			}
		}
		if len(missing) == 0 {
			break
		}
		if room := maxProduceLookups - lookups; len(missing) > room {
			missing = missing[:room]
		}
		lookups += len(missing)
		edges, err := f.src.BatchGetDependencies(ctx, missing, "forward")
		if err != nil {
			return nil, nil, nil, wishYield{}, err
		}
		for _, id := range missing {
			parent := ""
			for _, d := range edges[id] {
				if d.Type == "parent-child" && d.DependsOnID != "" {
					parent = d.DependsOnID
					break
				}
			}
			lin.cache[id] = parent
		}
	}

	out, more, yields := map[string][]FarmProduce{}, map[string]int{}, map[string]bool{}
	seenURL := map[string]map[string]bool{} // plan -> mr url -> listed
	for _, b := range rows {
		plan, phase, step, unk := lin.walk(b)
		if plan == "" || unk != "" {
			continue
		}
		var p *FarmProduce
		if b.Type == "bundle" || b.Type == "report" {
			p = docProduce(b)
			p.Task = step
		} else {
			p = mrProduce(b)
		}
		if p == nil {
			continue
		}
		if p.Kind == ProduceMR {
			if seenURL[plan] == nil {
				seenURL[plan] = map[string]bool{}
			}
			if seenURL[plan][p.URL] {
				continue
			}
			seenURL[plan][p.URL] = true
		}
		p.Phase = phase
		out[plan] = append(out[plan], *p)
		if p.Ready && p.Task != "" {
			yields[p.Task] = true
		}
	}
	for pid, list := range out {
		sort.Slice(list, func(i, j int) bool { return produceLess(list[i], list[j]) })
		if len(list) > maxProduce {
			more[pid] = len(list) - maxProduce
			list = list[:maxProduce]
		}
		out[pid] = list
	}
	return out, more, yields, wishProduce(mrRows), nil
}

// mrDone is a merge request's terminal state as its canonical bead records it.
type mrDone struct{ state, iid, title string }

// resolveMRs reads each row's merge request state from its canonical
// type=merge-request bead, reached by the row's produces edge (bug
// gd-9K2MsfmsNr). MR state lives there, not on the step: github-bridge writes
// only mr_url and forge onto the step (linkTask), and gitlab-bridge stopped
// stamping mr_state/mr_merged on task beads (kbeads auto_close.go, Gate C), so
// a merged GitHub PR read off its step looked opened and sat in the bin as
// "coming". A row whose own fields already say merged is kept as it is (a
// legacy stamp); any other row with an MR bead behind it is replaced by a copy
// carrying that bead's mr_state, and its mr_iid and mr_title where the step
// has none. Terminal is absorbing, so a merged or closed answer is cached by
// URL in f.slow.mrDone and never read again; an open MR is re-read each slow
// tick, at most maxProduceMRLookups rows of them. A row with no MR bead behind
// it keeps its own fields.
func (f *Farm) resolveMRs(ctx context.Context, rows []*beadsapi.BeadDetail) ([]*beadsapi.BeadDetail, error) {
	if f.slow.mrDone == nil {
		f.slow.mrDone = map[string]mrDone{}
	}
	out := append([]*beadsapi.BeadDetail(nil), rows...)
	var ask []string
	at := map[string]int{}
	for i, b := range out {
		if b == nil || mrOf(b) == nil || mergedOn(b) {
			continue
		}
		if d, ok := f.slow.mrDone[strings.TrimSpace(b.Fields["mr_url"])]; ok {
			out[i] = withMR(b, d)
			continue
		}
		if _, dup := at[b.ID]; !dup && len(ask) < maxProduceMRLookups {
			at[b.ID] = i
			ask = append(ask, b.ID)
		}
	}
	if len(ask) == 0 {
		return out, nil
	}
	edges, err := f.src.BatchGetDependencies(ctx, ask, "forward")
	if err != nil {
		return nil, err
	}
	var mrIDs []string
	seen := map[string]bool{}
	for _, id := range ask {
		for _, d := range edges[id] {
			if d.Type == forgebeads.DepProduces && d.DependsOnID != "" && !seen[d.DependsOnID] {
				seen[d.DependsOnID] = true
				mrIDs = append(mrIDs, d.DependsOnID)
			}
		}
	}
	if len(mrIDs) == 0 {
		return out, nil
	}
	mrBeads, err := f.src.BatchGetBeads(ctx, mrIDs)
	if err != nil {
		return nil, err
	}
	for _, id := range ask {
		b := out[at[id]]
		u := strings.TrimSpace(b.Fields["mr_url"])
		var best *beadsapi.BeadDetail
		for _, d := range edges[id] {
			m := mrBeads[d.DependsOnID]
			if d.Type != forgebeads.DepProduces || m == nil || m.Type != "merge-request" || strings.TrimSpace(m.Fields["mr_url"]) != u {
				continue
			}
			best = canonicalMRBead(best, m)
		}
		if best == nil {
			continue
		}
		d := mrDone{state: mrBeadState(best), iid: strings.TrimSpace(best.Fields["mr_iid"]), title: strings.TrimSpace(best.Fields["mr_title"])}
		out[at[id]] = withMR(b, d)
		if d.state == "merged" || d.state == "closed" {
			f.slow.mrDone[u] = d
		}
	}
	return out, nil
}

// withMRDone is b with the merge request state the cache already holds for its
// mr_url, or b itself. It costs no read: the harvest's closed steps take what
// the produce read learned.
func (f *Farm) withMRDone(b *beadsapi.BeadDetail) *beadsapi.BeadDetail {
	if b == nil || mrOf(b) == nil || mergedOn(b) {
		return b
	}
	if d, ok := f.slow.mrDone[strings.TrimSpace(b.Fields["mr_url"])]; ok {
		return withMR(b, d)
	}
	return b
}

// mergedOn says whether the step's own fields already record a merge.
func mergedOn(b *beadsapi.BeadDetail) bool {
	return strings.EqualFold(strings.TrimSpace(b.Fields["mr_state"]), "merged") ||
		strings.EqualFold(strings.TrimSpace(b.Fields["mr_merged"]), "true")
}

// withMR is a copy of b carrying d: mr_state always, mr_iid and mr_title only
// where b has none. The row is never written in place; it may be shared.
func withMR(b *beadsapi.BeadDetail, d mrDone) *beadsapi.BeadDetail {
	c := *b
	c.Fields = make(map[string]string, len(b.Fields)+3)
	for k, v := range b.Fields {
		c.Fields[k] = v
	}
	if d.state != "" {
		c.Fields["mr_state"] = d.state
	}
	if d.iid != "" && strings.TrimSpace(c.Fields["mr_iid"]) == "" {
		c.Fields["mr_iid"] = d.iid
	}
	if d.title != "" && strings.TrimSpace(c.Fields["mr_title"]) == "" {
		c.Fields["mr_title"] = d.title
	}
	return &c
}

// mrBeadState is an MR bead's state, with the merged predicate gb mr list uses
// (mr_merged, mr_outcome or mr_state).
func mrBeadState(m *beadsapi.BeadDetail) string {
	if m.Fields["mr_merged"] == "true" || m.Fields["mr_outcome"] == "merged" || m.Fields["mr_state"] == "merged" {
		return "merged"
	}
	return strings.ToLower(strings.TrimSpace(m.Fields["mr_state"]))
}

// canonicalMRBead picks which of two MR beads for one URL speaks for it, as
// gb's pickCanonicalMRBead does (duplicates are real, ka-0000002394): merged
// beats closed beats anything else, then the newer bead, then the larger id.
func canonicalMRBead(cur, next *beadsapi.BeadDetail) *beadsapi.BeadDetail {
	if cur == nil {
		return next
	}
	rank := func(m *beadsapi.BeadDetail) int {
		switch mrBeadState(m) {
		case "merged":
			return 2
		case "closed":
			return 1
		}
		return 0
	}
	if rc, rn := rank(cur), rank(next); rc != rn {
		if rn > rc {
			return next
		}
		return cur
	}
	if !next.UpdatedAt.Equal(cur.UpdatedAt) {
		if next.UpdatedAt.After(cur.UpdatedAt) {
			return next
		}
		return cur
	}
	if next.ID > cur.ID {
		return next
	}
	return cur
}

// wishYield is what each wish's kin merged (wishProduce), with the project:
// label of each item's bead, which the per-viewer cut admits it by
// (farm_cut.go).
type wishYield struct {
	byWish  map[string][]FarmProduce // wish id -> its kin's merged MRs
	project map[string]string        // produce id -> its bead's project: label
}

// wishProduce is what each wish's kin merged: the merged MRs among the
// produce read's MR rows whose thread_id names the wish, parented or loose,
// one per URL, newest first. A closed wish's headstone lists them
// (FarmClosedWish.Produce).
func wishProduce(rows []*beadsapi.BeadDetail) wishYield {
	out, seen := map[string][]FarmProduce{}, map[string]map[string]bool{}
	project := map[string]string{}
	for _, b := range rows {
		if b == nil {
			continue
		}
		wish := strings.TrimSpace(b.Fields["thread_id"])
		if wish == "" || wish == b.ID {
			continue
		}
		p := mrProduce(b)
		if p == nil || !p.Ready {
			continue
		}
		if seen[wish] == nil {
			seen[wish] = map[string]bool{}
		}
		if seen[wish][p.URL] {
			continue
		}
		seen[wish][p.URL] = true
		out[wish] = append(out[wish], *p)
		project[p.ID] = projectOf(b)
	}
	for _, list := range out {
		sort.Slice(list, func(i, j int) bool {
			if list[i].Updated != list[j].Updated {
				return list[i].Updated > list[j].Updated
			}
			return list[i].ID < list[j].ID
		})
	}
	return wishYield{byWish: out, project: project}
}
