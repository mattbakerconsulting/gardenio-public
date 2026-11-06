package gardenhub

// farm_delta.go — the delta catch-up (P1.4, gd-DuHnvvBYMr). A page that comes back to /garden/farm/events holding a
// version the stream has moved past (a phone tab back from the background, a stream that dropped, a laggard whose queue
// was collapsed) used to be sent the ring's patches since its version, one per refresh and each re-sending every row it
// touched, or, once the ring (ringCapacity) no longer reached back that far, a "reset": the whole document again, over a
// stream that is never compressed (text/event-stream, P1.1).
//
// Each stream (the whole farm's, and every cut's, farm_cut_stream.go) now keeps a deltaLog: per row (a plan, a seed, a
// gardener) the version it entered the stream's view and the version it last changed, per blob the version it last
// changed, and the rows removed (tombstones) with when. From it the stream answers a page at version since with ONE patch
// from since to now: the rows upserted after since, the rows removed after since that the page held (a row that came and
// went in between is never named, so a cut still never names a row its viewer was not sent), the blobs that moved, and
// the exits that explain those removals. web/plans.js applies it like any other patch: its prev is the page's version.
// Each row goes once, whole, at its current value; the stream sends it in place of the ring's replay when it is no
// bigger (stream.catchUp: a short replay of patches with phase stubs, below, can be smaller), and it never animates (it
// arrives in transitions.js's catch-up window, as the replay did).
//
// The log is bounded by deltaKeep tombstones, exits and issued versions. Past that the oldest are dropped and the floor
// rises to the newest of them; a page below the floor, at a version this stream never issued (another process, another
// cut's set), or from the future gets the ring's replay or the reset, as before.

import (
	"bytes"
	"encoding/json"
	"slices"
	"sort"
)

// deltaKeep bounds each of the log's lists. A tombstone is an id and two versions; the issued versions are one uint64
// each. At about one patch a second, 4096 versions reach back over an hour, where the ring reaches about four minutes.
const deltaKeep = 4096

// rowMark is when a row entered the view (born) and when it last changed or left it (at).
type rowMark struct{ born, at uint64 }

type deltaExit struct {
	v uint64
	e FarmExit
}

// deltaLog is the change log behind a stream's delta catch-up. It has no lock: the stream's owner calls it under the
// lock that guards the view it describes.
type deltaLog struct {
	floor  uint64                        // the oldest version a delta can start from
	issued []uint64                      // the versions published after floor, ascending
	rows   map[string]map[string]rowMark // set (plans, seeds, gardeners) -> id -> mark, for the rows in the view
	gone   map[string]map[string]rowMark // the same for the rows that left it (at is when)
	nGone  int
	blobs  map[string]uint64 // blob name -> the version it last changed
	exits  []deltaExit       // the exits published after floor, oldest first
}

var deltaSets = []string{"plans", "seeds", "gardeners"}

// newDeltaLog starts a log at version v over the view vw the stream holds at v.
func newDeltaLog(v uint64, vw cutView) *deltaLog {
	l := &deltaLog{floor: v, rows: map[string]map[string]rowMark{}, gone: map[string]map[string]rowMark{}, blobs: map[string]uint64{}}
	for i, m := range vw.sets() {
		l.rows[deltaSets[i]] = map[string]rowMark{}
		l.gone[deltaSets[i]] = map[string]rowMark{}
		for id := range m {
			l.rows[deltaSets[i]][id] = rowMark{born: v, at: v}
		}
	}
	return l
}

// sets is the view's row sets, in deltaSets order.
func (vw cutView) sets() [3]map[string]entry {
	return [3]map[string]entry{vw.plans, vw.seeds, vw.gardeners}
}

// blobList is the view's blobs and the patch fields they go in, in the document's order.
func (vw cutView) blobList(p *farmPatch) []struct {
	name string
	raw  []byte
	to   *json.RawMessage
} {
	return []struct {
		name string
		raw  []byte
		to   *json.RawMessage
	}{
		{"closed_plans", vw.closedPlans, &p.ClosedPlans}, {"meadow", vw.meadow, &p.Meadow}, {"wishes", vw.wishes, &p.Wishes}, {"home", vw.home, &p.Home},
		{"escalations", vw.escalations, &p.Escalations},
		{"releases", vw.releases, &p.Releases}, {"piles", vw.piles, &p.Piles}, {"meta", vw.meta, &p.Meta}, {"cost", vw.cost, &p.Cost},
	}
}

// farmPatch is one SSE "patch" body. A set (plans, seeds) carries per-entity
// upserts/removals; a singleton blob is present only when it changed, and then
// carries its full new value.
type farmPatch struct {
	V           uint64          `json:"v"`
	Prev        uint64          `json:"prev"`
	Plans       *setPatch       `json:"plans,omitempty"`
	Seeds       *setPatch       `json:"seeds,omitempty"`
	Gardeners   *setPatch       `json:"gardeners,omitempty"`
	ClosedPlans json.RawMessage `json:"closed_plans,omitempty"`
	Meadow      json.RawMessage `json:"meadow,omitempty"`
	Wishes      json.RawMessage `json:"wishes,omitempty"`
	Home        json.RawMessage `json:"home,omitempty"`
	Escalations json.RawMessage `json:"escalations,omitempty"`
	Releases    json.RawMessage `json:"releases,omitempty"`
	Piles       json.RawMessage `json:"piles,omitempty"`
	Meta        json.RawMessage `json:"meta,omitempty"`
	Cost        json.RawMessage `json:"cost,omitempty"`
	// Exits rides only on a patch whose gardeners.rm it explains; it is never
	// in a snapshot or a reset.
	Exits []FarmExit `json:"exits,omitempty"`
}

func (p farmPatch) empty() bool {
	return p.Plans == nil && p.Seeds == nil && p.Gardeners == nil && len(p.Exits) == 0 && p.ClosedPlans == nil && p.Meadow == nil && p.Wishes == nil && p.Home == nil && p.Escalations == nil && p.Releases == nil && p.Piles == nil && p.Meta == nil && p.Cost == nil
}

// note records the patch p published at version v: the ids it upserted per set (up, in deltaSets order), its removals
// and blobs as p carries them, and its exits.
func (l *deltaLog) note(v uint64, p farmPatch, up [3][]string) {
	l.issued = append(l.issued, v)
	for i, sp := range []*setPatch{p.Plans, p.Seeds, p.Gardeners} {
		set := deltaSets[i]
		for _, id := range up[i] {
			mk, ok := l.rows[set][id]
			if !ok {
				mk.born = v
				if _, was := l.gone[set][id]; was {
					delete(l.gone[set], id)
					l.nGone--
				}
			}
			mk.at = v
			l.rows[set][id] = mk
		}
		if sp == nil {
			continue
		}
		for _, id := range sp.Rm {
			mk := l.rows[set][id]
			delete(l.rows[set], id)
			mk.at = v
			if _, was := l.gone[set][id]; !was {
				l.nGone++
			}
			l.gone[set][id] = mk
		}
	}
	for _, b := range (cutView{}).blobList(&p) {
		if *b.to != nil {
			l.blobs[b.name] = v
		}
	}
	for _, e := range p.Exits {
		l.exits = append(l.exits, deltaExit{v: v, e: e})
	}
	l.prune()
}

// prune keeps each list within deltaKeep by raising the floor past the oldest half of the longest one.
func (l *deltaLog) prune() {
	if len(l.issued) > deltaKeep {
		l.raise(l.issued[len(l.issued)/2])
	}
	if len(l.exits) > deltaKeep {
		l.raise(l.exits[len(l.exits)/2].v)
	}
	if l.nGone > deltaKeep {
		var ats []uint64
		for _, g := range l.gone {
			for _, mk := range g {
				ats = append(ats, mk.at)
			}
		}
		slices.Sort(ats)
		l.raise(ats[len(ats)/2])
	}
}

// raise moves the floor up to version to (a version the log issued) and drops what only a page below it needed.
func (l *deltaLog) raise(to uint64) {
	if to <= l.floor {
		return
	}
	l.floor = to
	i, _ := slices.BinarySearch(l.issued, to)
	if i < len(l.issued) && l.issued[i] == to {
		i++
	}
	l.issued = slices.Clone(l.issued[i:])
	for _, g := range l.gone {
		for id, mk := range g {
			if mk.at <= to {
				delete(g, id)
				l.nGone--
			}
		}
	}
	j := 0
	for j < len(l.exits) && l.exits[j].v <= to {
		j++
	}
	l.exits = slices.Clone(l.exits[j:])
}

// covers reports whether since is a version the log can start a delta from: the floor or one it issued since.
func (l *deltaLog) covers(since uint64) bool {
	if since == l.floor {
		return true
	}
	_, ok := slices.BinarySearch(l.issued, since)
	return ok
}

// patch is the patch that takes a page holding version since to version v, whose view is vw; false when the log does
// not cover since.
func (l *deltaLog) patch(since, v uint64, vw cutView) (farmPatch, bool) {
	if since >= v || !l.covers(since) {
		return farmPatch{}, false
	}
	p := farmPatch{V: v, Prev: since}
	to := []**setPatch{&p.Plans, &p.Seeds, &p.Gardeners}
	for i, m := range vw.sets() {
		set := deltaSets[i]
		var sp setPatch
		var ids []string
		for id, mk := range l.rows[set] {
			if mk.at > since {
				ids = append(ids, id)
			}
		}
		sort.Strings(ids)
		for _, id := range ids {
			if e, ok := m[id]; ok {
				sp.Up = append(sp.Up, e.raw)
			}
		}
		for id, mk := range l.gone[set] {
			if mk.at > since && mk.born <= since {
				sp.Rm = append(sp.Rm, id)
			}
		}
		sort.Strings(sp.Rm)
		if len(sp.Up)+len(sp.Rm) > 0 {
			*to[i] = &sp
		}
	}
	for _, b := range vw.blobList(&p) {
		if l.blobs[b.name] > since && len(b.raw) > 0 {
			*b.to = b.raw
		}
	}
	if p.Gardeners != nil {
		for _, x := range l.exits {
			if x.v > since && slices.Contains(p.Gardeners.Rm, x.e.ID) {
				p.Exits = append(p.Exits, x.e)
			}
		}
	}
	return p, true
}

// msg is patch as the catch-up message for the stream.
func (l *deltaLog) msg(since, v uint64, vw cutView) (Msg, bool) {
	p, ok := l.patch(since, v, vw)
	if !ok {
		return Msg{}, false
	}
	data, _ := json.Marshal(p)
	return Msg{Event: "patch", V: v, Data: data}, true
}

// ---- phase stubs -----------------------------------------------------------
//
// A step that moves changes one phase of its plan, and the plan was re-sent whole: at 10x about 9 KB for a 2 KB phase.
// A live patch's plan upsert (planUps) now carries each phase the page already holds byte for byte as a stub,
// {"id":<phase id>,"same":1}, and an unchanged unphased row as {"same":1}; web/plans.js fillPlan puts back the ones it
// holds. Everything else in the entry is the document's own bytes. Only a patch from the previous version uses stubs:
// a reset or a delta catch-up (above) always carries whole plans, since the page's copy is older.

var (
	phasesKey   = []byte(`"phases":`)
	unphasedKey = []byte(`"unphased":`)
	sameStub    = []byte(`{"same":1}`)
)

// planUps is diffEntries' plan upserts (up, with their ids) as a live patch sends them: each plan old holds as
// planDelta of it.
func planUps(old map[string]entry, up []json.RawMessage, ids []string) []json.RawMessage {
	out := make([]json.RawMessage, len(up))
	for i, raw := range up {
		out[i] = raw
		if o, ok := old[ids[i]]; ok {
			out[i] = planDelta(o.raw, raw)
		}
	}
	return out
}

// planDelta is plan entry next for a page that holds prev: next with each phase that is byte for byte prev's phase of
// the same id, and an unphased row equal to prev's, replaced by a stub. next itself when nothing is the same or either
// does not parse.
func planDelta(prev, next []byte) []byte {
	var a, b map[string]json.RawMessage
	if json.Unmarshal(prev, &a) != nil || json.Unmarshal(next, &b) != nil {
		return next
	}
	var held, phs []json.RawMessage
	if json.Unmarshal(a["phases"], &held) != nil || json.Unmarshal(b["phases"], &phs) != nil {
		return next
	}
	byID := make(map[string][]byte, len(held))
	for _, ph := range held {
		if id := phaseID(ph); id != "" {
			byID[id] = ph
		}
	}
	out, stubbed := next, false
	parts := make([][]byte, len(phs))
	for i, ph := range phs {
		parts[i] = ph
		if id := phaseID(ph); id != "" && bytes.Equal(byID[id], ph) {
			parts[i], _ = json.Marshal(struct {
				ID   string `json:"id"`
				Same int    `json:"same"`
			}{id, 1})
			stubbed = true
		}
	}
	if stubbed {
		out = spliceValue(out, phasesKey, b["phases"], append(append([]byte{'['}, bytes.Join(parts, []byte{','})...), ']'))
	}
	if u := b["unphased"]; len(u) > 0 && bytes.Equal(a["unphased"], u) {
		out, stubbed = spliceValue(out, unphasedKey, u, sameStub), true
	}
	if !stubbed {
		return next
	}
	return out
}

// spliceValue is doc with the value was of its top-level key (key is `"name":`) replaced by now. The key cannot occur
// inside a string (its quotes would be escaped) and a plan has no nested key of that name, so the first is the one.
func spliceValue(doc, key, was, now []byte) []byte {
	at := bytes.Index(doc, append(slices.Clip(key), was...))
	if at < 0 {
		return doc
	}
	at += len(key)
	out := make([]byte, 0, len(doc)-len(was)+len(now))
	out = append(append(append(out, doc[:at]...), now...), doc[at+len(was):]...)
	return out
}

// phaseID is a phase entry's id, "" when it has none or does not parse.
func phaseID(raw []byte) string {
	var ph struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(raw, &ph)
	return ph.ID
}
