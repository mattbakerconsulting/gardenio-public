package gardenhub

// farm_cut_stream.go — the per-viewer cut of the live farm stream (gd-UKO0ZpXeha, Costs phase 3 gd-MnQlEmbUq3), the
// stream half of farm_cut.go. A scoped viewer's /garden/farm/events is not the whole farm's stream filtered: it is a
// stream of its own, one per project set (a cutStream), whose patches are the difference between two cut views
// (cutViewLocked) of the farm. So what a scoped viewer is sent is exactly what its first-paint document holds:
//
//   - A row (a plan, a seed, a gardener) is upserted when it is in the cut and its cut entry changed, and removed only
//     when the viewer's last view had it. A removal never names a row the viewer was not sent.
//   - A blob (closed_plans, meadow, wishes, escalations, releases, piles, meta, cost) is sent whole, recut for the set, when its cut changed.
//   - exits ride on the patch whose gardeners.rm they explain, for the removed gardeners only, without their tldr,
//     tends and escalation (each can name a bead outside the set), and with their plot only when its plan was in view.
//
// The cut stream has its own version chain: its v moves only when its view does, so a viewer whose lands did not
// change hears nothing when another land moves, and its first-paint document (CutSnapshot) carries the cut's v, not
// the farm's. Every cut takes its versions from one counter (Farm.cutSeq), so a version names one set's state: a
// viewer whose set changed, or who was handed another set's v, never matches the ring and gets a reset of its own cut.
// The catch-up ring, its delta log (farm_delta.go) and Last-Event-ID resume are the stream's own, as for the whole farm
// (stream.go). Another set's version is not one this stream issued, so the log never covers it either.
//
// Every refresh advances every cut stream (advanceCutsLocked); a stream with no viewer is dropped once nobody has
// asked for it in farmArchiveWantTTL. The activity events (activity.go) to a scoped viewer go only for the gardeners
// in its view (cutStream.keeps), so a gardener the document drops is not named by its calls either.
//
// The whole farm's stream, its patches and its bytes are untouched: an unrestricted viewer subscribes to Farm.st as
// before.

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"slices"
	"sort"
	"sync/atomic"
	"time"
)

// cutStream is the stream of one project set.
type cutStream struct {
	projects []string // sorted, deduplicated
	st       *stream
	dl       *deltaLog // st's change log (farm_delta.go)
	gen      uint64    // the cut generation view was cut at
	view     cutView
	used     time.Time // when a request last asked for it, or its last viewer left
	doc      *cutMemo
	// in is the gardener ids in view, for the activity feed's keep, which runs under the feed's lock, not f.mu.
	in atomic.Pointer[map[string]bool]
}

// keeps is the stream's cut of gardeners for the activity feed.
func (cs *cutStream) keeps(g FarmGardener) bool {
	in := cs.in.Load()
	return in != nil && (*in)[g.ID]
}

func (cs *cutStream) setView(v cutView) {
	cs.view = v
	in := make(map[string]bool, len(v.gardeners))
	for id := range v.gardeners {
		in[id] = true
	}
	cs.in.Store(&in)
}

func (f *Farm) nextCutSeqLocked() uint64 {
	f.cutSeq++
	return f.cutSeq
}

// cutStreamLocked is the set's stream, made (at the current state) if there is none, and advanced to the current cut
// generation.
func (f *Farm) cutStreamLocked(cut FarmCut) *cutStream {
	key, projects := cut.key()
	now := f.now()
	if cs := f.cuts[key]; cs != nil {
		f.advanceCutLocked(cs, nil, true)
		return cs
	}
	if len(f.cuts) >= farmCutMemoCap {
		f.dropIdleCutsLocked()
	}
	if f.cuts == nil {
		f.cuts = map[string]*cutStream{}
	}
	cs := &cutStream{projects: projects, st: newStream(f.nextCutSeqLocked()), gen: f.cutGen, used: now}
	cs.setView(f.cutViewLocked(projects, now, true))
	cs.dl = newDeltaLog(cs.st.version, cs.view)
	f.cuts[key] = cs
	return cs
}

// dropIdleCutsLocked makes room for one more cut stream: it drops the streams without a viewer, least recently used
// first, until there is room. A stream with a viewer is never dropped, so the cap can be passed while every set in
// use has one.
func (f *Farm) dropIdleCutsLocked() {
	var idle []string
	for k, cs := range f.cuts {
		if len(cs.st.clients) == 0 {
			idle = append(idle, k)
		}
	}
	sort.Slice(idle, func(i, j int) bool { return f.cuts[idle[i]].used.Before(f.cuts[idle[j]].used) })
	for _, k := range idle {
		if len(f.cuts) < farmCutMemoCap {
			return
		}
		delete(f.cuts, k)
	}
}

// advanceCutsLocked moves every cut stream on to the cut state refreshOnce just published, with that refresh's exits,
// and drops a stream nobody has viewed or asked for in farmArchiveWantTTL.
func (f *Farm) advanceCutsLocked(exits []FarmExit) {
	now := f.now()
	for k, cs := range f.cuts {
		if len(cs.st.clients) == 0 && now.Sub(cs.used) >= farmArchiveWantTTL {
			delete(f.cuts, k)
			continue
		}
		f.advanceCutLocked(cs, exits, false)
	}
}

// advanceCutLocked recuts the stream's view when the cut state moved, and publishes the difference when there is one.
func (f *Farm) advanceCutLocked(cs *cutStream, exits []FarmExit, ask bool) {
	if cs.gen == f.cutGen {
		return
	}
	cs.gen = f.cutGen
	was, next := cs.view, f.cutViewLocked(cs.projects, f.now(), ask)
	patch := farmPatch{}
	var upIDs [3][]string
	var up []json.RawMessage
	var rm []string
	if up, upIDs[0], rm = diffEntries(was.plans, next.plans); len(up)+len(rm) > 0 {
		patch.Plans = &setPatch{Up: planUps(was.plans, up, upIDs[0]), Rm: rm}
	}
	if up, upIDs[1], rm = diffEntries(was.seeds, next.seeds); len(up)+len(rm) > 0 {
		patch.Seeds = &setPatch{Up: up, Rm: rm}
	}
	if up, upIDs[2], rm = diffEntries(was.gardeners, next.gardeners); len(up)+len(rm) > 0 {
		patch.Gardeners = &setPatch{Up: up, Rm: rm}
		patch.Exits = cutExits(exits, rm, was.plans, next.plans)
	}
	for _, b := range []struct {
		old, new []byte
		to       *json.RawMessage
	}{
		{was.closedPlans, next.closedPlans, &patch.ClosedPlans},
		{was.meadow, next.meadow, &patch.Meadow},
		{was.wishes, next.wishes, &patch.Wishes},
		{was.home, next.home, &patch.Home},
		{was.escalations, next.escalations, &patch.Escalations},
		{was.releases, next.releases, &patch.Releases},
		{was.piles, next.piles, &patch.Piles},
		{was.meta, next.meta, &patch.Meta},
		{was.cost, next.cost, &patch.Cost},
	} {
		if string(b.old) != string(b.new) {
			*b.to = b.new
		}
	}
	cs.setView(next)
	if patch.empty() {
		return
	}
	m := cs.st.publishAt(f.nextCutSeqLocked(), func(prev, v uint64) []byte {
		patch.V, patch.Prev = v, prev
		data, _ := json.Marshal(patch)
		return data
	})
	cs.dl.note(m.V, patch, upIDs)
}

// cutExits is the exits of a refresh as a cut sees them: the gardeners it removed only, without what can name a bead
// outside the set (tldr, tends, escalation), their plot only when its plan was or is in view.
func cutExits(exits []FarmExit, rm []string, was, next map[string]entry) []FarmExit {
	var out []FarmExit
	for _, e := range exits {
		if !slices.Contains(rm, e.ID) {
			continue
		}
		e.TLDR, e.Tends, e.Escalation = "", "", ""
		if e.Plot != nil {
			_, inWas := was[e.Plot.Plan]
			_, inNext := next[e.Plot.Plan]
			if !inWas && !inNext {
				e.Plot = nil
			}
		}
		out = append(out, e)
	}
	return out
}

// cutDocLocked is the stream's document at its version: its view, with gardeners[].doing baked in as snapshotLocked
// does, memoised per version, cut generation (built_at moves with every refresh) and activity epoch. Its ETag is a
// hash of its bytes without v and built_at, so two sets never share one and a revisit whose cut did not change is a
// 304.
func (f *Farm) cutDocLocked(cs *cutStream) *cutMemo {
	now := f.now()
	if m := cs.doc; m != nil && m.v == cs.st.version && m.gen == f.cutGen && (m.act == f.act.currentEpoch() || now.Sub(m.at) < activitySnapEvery) {
		return m
	}
	vw := cs.view
	doings, epoch := f.act.doings(vw.gardeners, now)
	for id, d := range doings {
		doings[id] = d.masked() // a caption's bead ids, amounts and hosts are not a scoped viewer's (maskScoped)
	}
	doc := struct {
		V           uint64            `json:"v"`
		BuiltAt     string            `json:"built_at"`
		Plans       []json.RawMessage `json:"plans"`
		Gardeners   []json.RawMessage `json:"gardeners"`
		Seeds       []json.RawMessage `json:"seeds"`
		ClosedPlans json.RawMessage   `json:"closed_plans"`
		Meadow      json.RawMessage   `json:"meadow"`
		Wishes      json.RawMessage   `json:"wishes"`
		Home        json.RawMessage   `json:"home"`
		Escalations json.RawMessage   `json:"escalations"`
		Releases    json.RawMessage   `json:"releases"`
		Piles       json.RawMessage   `json:"piles"`
		Meta        json.RawMessage   `json:"meta"`
		Cost        json.RawMessage   `json:"cost,omitempty"`
	}{
		V: cs.st.version, Plans: sortedRaw(vw.plans), Gardeners: sortedRaw(withDoing(vw.gardeners, doings)), Seeds: sortedRaw(vw.seeds),
		ClosedPlans: vw.closedPlans, Meadow: vw.meadow, Wishes: vw.wishes, Home: vw.home, Escalations: vw.escalations, Releases: vw.releases, Piles: vw.piles, Meta: vw.meta, Cost: vw.cost,
	}
	if !f.builtAt.IsZero() {
		doc.BuiltAt = f.builtAt.UTC().Format(time.RFC3339)
	}
	v, built := doc.V, doc.BuiltAt
	doc.V, doc.BuiltAt = 0, ""
	unstamped, _ := json.Marshal(doc)
	h := fnv.New64a()
	_, _ = h.Write(unstamped)
	doc.V, doc.BuiltAt = v, built
	b, _ := json.Marshal(doc)
	m := &cutMemo{v: cs.st.version, gen: f.cutGen, act: epoch, at: now, b: b, etag: fmt.Sprintf(`W/"s%016x"`, h.Sum64())}
	cs.doc = m
	return m
}

// cutResetLocked is the stream's "reset": its document.
func (f *Farm) cutResetLocked(cs *cutStream) Msg {
	return Msg{Event: "reset", V: cs.st.version, Data: f.cutDocLocked(cs).b}
}

// SubscribeCut is Subscribe for a scoped viewer: the stream of the farm cut to cut (cut.Scoped), from the version
// since of a document or patch of that cut (0 = unknown; another set's version is a reset). keep is the cut's
// gardeners, for SubscribeActivity. The catch-up and the registration happen under one lock.
func (f *Farm) SubscribeCut(cut FarmCut, since uint64) (catchUp []Msg, live <-chan Msg, keep func(FarmGardener) bool, cancel func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cs := f.cutStreamLocked(cut)
	catchUp, live, c := cs.st.subscribe(since, cs.deltaLocked, func() Msg { return f.cutResetLocked(cs) })
	return catchUp, live, cs.keeps, func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		c()
		cs.used = f.now()
	}
}

// deltaLocked is the stream's one patch from since to its current version (farm_delta.go).
func (cs *cutStream) deltaLocked(since uint64) (Msg, bool) {
	return cs.dl.msg(since, cs.st.version, cs.view)
}

// CutResume is Resume for a scoped viewer: the catch-up of its cut's stream from version since.
func (f *Farm) CutResume(cut FarmCut, since uint64) []Msg {
	f.mu.Lock()
	defer f.mu.Unlock()
	cs := f.cutStreamLocked(cut)
	return cs.st.catchUp(since, cs.deltaLocked, func() Msg { return f.cutResetLocked(cs) })
}

// CutReset is Reset for a scoped viewer: its cut's document at the cut's current version.
func (f *Farm) CutReset(cut FarmCut) Msg {
	f.mu.Lock()
	defer f.mu.Unlock()
	cs := f.cutStreamLocked(cut)
	cs.st.resets++
	return f.cutResetLocked(cs)
}

func (f *Farm) cutClientsLocked() int {
	n := 0
	for _, cs := range f.cuts {
		n += len(cs.st.clients)
	}
	return n
}

// keepBoth is a gardener cut that keeps what both a and b keep; a nil cut keeps everything.
func keepBoth(a, b func(FarmGardener) bool) func(FarmGardener) bool {
	if a == nil {
		return b
	}
	if b == nil {
		return a
	}
	return func(g FarmGardener) bool { return a(g) && b(g) }
}
