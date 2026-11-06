package gardenhub

// transcript.go — what each gardener did (plan gd-jMl4oF2cJO step 7, gd-MsTDi7JYVs; spec
// docs/superpowers/specs/2026-10-28-bird-captions-design.md sections 11.1-11.3 and 12). Each gardener in the current set
// keeps a ring of its last calls (transcriptRows, or FARM_TRANSCRIPT_ROWS) beside its actState (activity.go), in memory only: a restart empties it,
// the agent's forgetting drops it and a new session clears it. A row is written when its call finishes (so it has dur and
// ok); a call whose Haiku caption is still out waits for it, up to transcriptHaikuWait, and a call that never finishes is
// written when activityOpenForget lets it go, with dur 0 and no ok.
//
// It is read two ways, both cut per viewer exactly like gardeners[] (SeesGardener, then the scoped cut's gardeners) and
// with bead ids masked out of a scoped viewer's captions:
//
//   - GET /garden/farm/activity/{id} and GET /garden/farm/activity?scope=: rows newest first (farmTranscript), paged
//     by an opaque per-row cursor (next older, prev newer; gd-LEmM2G7iaa) so rows sharing a second are never split.
//   - `event: call` on /garden/farm/events, only to a stream whose page holds a transcript open ("transcript" on POST
//     /garden/farm/events/view), batched to one event every transcriptEvery with at most transcriptRate rows; past that
//     the event says how many it skipped. A stream with no transcript open is sent no row, so the farm page's budget is
//     untouched. Shapes: gardenio/docs/homestead-farm-read-model.md, "Transcript".

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	transcriptRows      = 200                     // rows a gardener's ring keeps by default (FARM_TRANSCRIPT_ROWS, Helm beadsViewer.transcript.rows)
	maxTranscriptRows   = 1000                    // the most FARM_TRANSCRIPT_ROWS may set: a ring is about 30 KB at 200
	transcriptScopeRows = 100                     // rows one scope request returns at most
	transcriptEvery     = time.Second             // a stream holding a transcript is sent at most one call event this often
	transcriptRate      = 20                      // and at most this many rows in it; past them it counts skipped
	transcriptHaikuWait = 3500 * time.Millisecond // a finished call waits this long after its start for its Haiku caption (3 s timeout)
	maxTranscriptScope  = 128                     // bytes of a transcript scope a page names
)

// CallEvent is the SSE event of a transcript's live rows (FarmCalls).
const CallEvent = "call"

// CallRow is one row of a gardener's transcript: a call that finished. at is when it started (unix s), class and tool
// as on the activity event, cap its caption (the full captionMax runes, a late Haiku one included), dur its ms (0 for a
// call let go unfinished), ok false for PostToolUseFailure and absent when it never finished. id is set where rows of
// several gardeners meet: the scope endpoint and the call event. seq numbers the rows in the order they were written,
// across gardeners: with at it orders and pages them (rowKey), and it is not on the wire.
type CallRow struct {
	ID    string `json:"id,omitempty"`
	At    int64  `json:"at"`
	Class string `json:"class"`
	Tool  string `json:"tool"`
	Cap   string `json:"cap,omitempty"`
	Dur   int64  `json:"dur"`
	OK    *bool  `json:"ok,omitempty"`
	seq   uint64
}

// rowKey is where a row stands in a transcript: by at, then by seq. No two rows share one, so a page cut at a key
// splits no second.
type rowKey struct {
	at  int64
	seq uint64
}

func (r CallRow) key() rowKey { return rowKey{r.At, r.seq} }

// newestFirst orders x before y when x is the newer.
func (x rowKey) newestFirst(y rowKey) int {
	return cmp.Or(cmp.Compare(y.at, x.at), cmp.Compare(y.seq, x.seq))
}

// The two outcomes a finished row points at (never written through).
var callOK, callFailed = true, false

func outcome(ok bool) *bool {
	if ok {
		return &callOK
	}
	return &callFailed
}

// masked is r for a scoped viewer: its caption masked (maskScoped, spec 12).
func (r CallRow) masked() CallRow {
	r.Cap = maskScoped(r.Cap)
	return r
}

// FarmCalls is the SSE `call` event: the rows written since the stream's last one, newest first, and how many more were
// written in that time than one event carries (skipped, newest kept). Rows carry id.
type FarmCalls struct {
	Rows    []CallRow `json:"rows"`
	Skipped int       `json:"skipped,omitempty"`
}

// callRing is one gardener's last n rows (activity.ringRows) in the order they were written.
type callRing struct {
	rows []CallRow // once full, rows[head] is the oldest
	head int
}

// add writes row, keeping the last n. n is the same on every call (it is set before the farm starts).
func (r *callRing) add(row CallRow, n int) {
	if len(r.rows) < n {
		if len(r.rows) == cap(r.rows) { // grown by hand: append would round past n
			grown := make([]CallRow, len(r.rows), min(max(2*cap(r.rows), 8), n))
			copy(grown, r.rows)
			r.rows = grown
		}
		r.rows = append(r.rows, row)
		return
	}
	r.rows[r.head] = row
	r.head = (r.head + 1) % n
}

// each calls fn on every row, the oldest written first.
func (r *callRing) each(fn func(CallRow)) {
	for i := range r.rows {
		fn(r.rows[(r.head+i)%len(r.rows)])
	}
}

func (r *callRing) reset() { r.rows, r.head = nil, 0 }

// parkedRow is a finished call's row waiting for the caption Haiku is writing for it (lateCaption), until due.
type parkedRow struct {
	key   string
	start uint64
	row   CallRow
	due   time.Time
}

// transcriptScope is what a stream's transcript, or one scope request, covers: one gardener (kind "" and id), the farm,
// a project or a plot.
type transcriptScope struct {
	kind string // "", "farm", "project" or "plot"
	val  string // the gardener id, project or plot's plan (or task) id
}

// errTranscriptScope is a scope that names nothing a transcript can cover.
var errTranscriptScope = errors.New("bad transcript scope")

// parseTranscriptScope reads "farm", "project:<p>", "plot:<task id>" or a gardener id. ok is false for an empty value, an
// empty project or plot, and one past maxTranscriptScope bytes.
func parseTranscriptScope(s string) (transcriptScope, bool) {
	if s == "" || len(s) > maxTranscriptScope || strings.ContainsAny(s, " \t\r\n") {
		return transcriptScope{}, false
	}
	if s == "farm" {
		return transcriptScope{kind: "farm"}, true
	}
	for _, kind := range []string{"project", "plot"} {
		if v, ok := strings.CutPrefix(s, kind+":"); ok {
			return transcriptScope{kind: kind, val: v}, v != ""
		}
	}
	return transcriptScope{val: s}, true
}

// keeps says the scope covers gardener g (id). A plot is a plan's: a gardener is on it when it stands on the plan's
// plot or its task is the named one.
func (s transcriptScope) keeps(id string, g FarmGardener) bool {
	switch s.kind {
	case "farm":
		return true
	case "project":
		return g.Project == s.val
	case "plot":
		return g.Task == s.val || (g.Plot != nil && g.Plot.Plan == s.val)
	}
	return id == s.val
}

// finishLocked writes the row of the call c (key) of agent id that finished at at as e (its dur set); paired is false
// for a finish heard without its start (a restart, a start past maxActivityInflight), whose row starts dur before at.
func (a *activity) finishLocked(id string, st *actState, key string, c openCall, paired bool, e FarmActivity, ok bool, at, now time.Time) {
	if !paired {
		a.writeRowLocked(id, st, CallRow{At: at.Add(-time.Duration(e.Dur) * time.Millisecond).Unix(), Class: e.Doing, Tool: e.Tool, Dur: e.Dur, OK: outcome(ok)})
		return
	}
	row := CallRow{At: c.at.Unix(), Class: c.doing, Tool: c.tool, Cap: c.cap, Dur: e.Dur, OK: outcome(ok)}
	if due := c.at.Add(transcriptHaikuWait); c.haiku && c.cap == "" && due.After(now) && len(st.parked) < maxActivityInflight {
		st.parked = append(st.parked, parkedRow{key: key, start: c.start, row: row, due: due})
		return
	}
	a.writeRowLocked(id, st, row)
}

// lateRowLocked gives the call of st numbered start (key) the caption Haiku wrote for it: on the open call, so its row
// carries it, or on its parked row, which is then written. Whether or not it is still the newest start.
func (a *activity) lateRowLocked(id string, st *actState, start uint64, key, caption string) {
	if c, ok := st.open[key]; ok && c.start == start {
		c.cap, c.haiku = caption, false
		st.open[key] = c
		return
	}
	for i, p := range st.parked {
		if p.key == key && p.start == start {
			st.parked = slices.Delete(st.parked, i, i+1)
			p.row.Cap = caption
			a.writeRowLocked(id, st, p.row)
			return
		}
	}
}

// writeRowLocked adds row to gardener id's ring and queues it for each stream whose transcript covers it and whose cut
// keeps it. An agent that is not a gardener keeps no transcript.
func (a *activity) writeRowLocked(id string, st *actState, row CallRow) {
	g, ok := a.gardeners[id]
	if !ok {
		return
	}
	a.rows++
	row.seq = a.rows // a.rows numbers the rows written
	st.ring.add(row, a.ringRows)
	if a.trClients == 0 {
		return
	}
	row.ID = id
	for _, c := range a.clients {
		if c.tr == nil || !c.tr.keeps(id, g) || (c.keep != nil && !c.keep(g)) {
			continue
		}
		r := row
		if c.keep != nil {
			r = r.masked()
		}
		if len(c.trRows) >= transcriptRate {
			c.trRows = slices.Delete(c.trRows, 0, 1) // the oldest goes: the panel loads it on scroll
			c.trSkip++
		}
		c.trRows = append(c.trRows, r)
	}
}

// writeDueLocked writes the parked rows of agent id that are due.
func (a *activity) writeDueLocked(id string, st *actState, now time.Time) {
	for i := 0; i < len(st.parked); {
		if p := st.parked[i]; !now.Before(p.due) {
			st.parked = slices.Delete(st.parked, i, i+1)
			a.writeRowLocked(id, st, p.row) // Haiku did not answer in time: the tool word stands
			continue
		}
		i++
	}
}

// sendCallsLocked sends c its call event when one is due.
func (a *activity) sendCallsLocked(c *actClient, now time.Time) {
	if c.tr == nil || (len(c.trRows) == 0 && c.trSkip == 0) || now.Sub(c.trAt) < transcriptEvery {
		return
	}
	rows := slices.Clone(c.trRows)
	slices.Reverse(rows)
	data, _ := json.Marshal(FarmCalls{Rows: rows, Skipped: c.trSkip})
	m := ActivityMsg{Event: CallEvent, Data: data}
	if a.sendLocked(c, m) {
		a.trBytes += uint64(m.size())
	}
	c.trRows, c.trSkip, c.trAt = c.trRows[:0], 0, now
}

// setTranscript opens the transcript of scope on stream sid ("" closes it); false when there is no such stream. Rows
// already written are the endpoint's to serve: the stream is sent the ones written from now on.
func (a *activity) setTranscript(sid, scope string) (bool, error) {
	var tr *transcriptScope
	if scope != "" {
		s, ok := parseTranscriptScope(scope)
		if !ok {
			return false, errTranscriptScope
		}
		tr = &s
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	cid, ok := a.sids[sid]
	c := a.clients[cid]
	if !ok || c == nil {
		return false, nil
	}
	if (c.tr == nil) != (tr == nil) {
		if tr == nil {
			a.trClients--
		} else {
			a.trClients++
		}
	}
	if tr == nil || c.tr == nil || *tr != *c.tr {
		c.trRows, c.trSkip = nil, 0
	}
	c.tr = tr
	return true, nil
}

// transcriptQuery is what a transcript request asks for: rows that started at since or later and before before (0: no
// bound), at most limit; with a cursor, only the rows past it (older, or newer when cursor.newer).
type transcriptQuery struct {
	since, before int64
	limit         int
	cursor        *transcriptCursor
}

// transcriptCursor is a page's edge as the endpoints hand it out: a row's key and the way to page from it. On the wire
// it is opaque, "o" (older) or "n" (newer), the row's at, ".", its seq.
type transcriptCursor struct {
	key   rowKey
	newer bool
}

func (c transcriptCursor) String() string {
	dir := "o"
	if c.newer {
		dir = "n"
	}
	return fmt.Sprintf("%s%d.%d", dir, c.key.at, c.key.seq)
}

// parseTranscriptCursor reads a cursor String wrote; false for anything else.
func parseTranscriptCursor(s string) (transcriptCursor, bool) {
	var c transcriptCursor
	switch {
	case strings.HasPrefix(s, "n"):
		c.newer = true
	case !strings.HasPrefix(s, "o"):
		return c, false
	}
	at, seq, ok := strings.Cut(s[1:], ".")
	n, err := strconv.ParseInt(at, 10, 64)
	m, err2 := strconv.ParseUint(seq, 10, 64)
	if !ok || err != nil || err2 != nil || n < 0 {
		return c, false
	}
	c.key = rowKey{n, m}
	return c, true
}

// transcriptPage is the rows a query gets, newest first; more says rows past limit matched (on the way the cursor
// pages), next and prev are the cursors to the older and the newer rows ("" for none).
type transcriptPage struct {
	rows       []CallRow
	more       bool
	next, prev string
}

func (q transcriptQuery) wants(r CallRow) bool {
	return r.At >= q.since && (q.before == 0 || r.At < q.before)
}

// transcript is gardener id's page of the rows the query wants. ok is false for an id that is not a gardener or that
// keep does not keep, alike. A viewer with a cut (keep) gets bead ids masked.
func (a *activity) transcript(id string, keep func(FarmGardener) bool, q transcriptQuery) (p transcriptPage, ok bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	g, ok := a.gardeners[id]
	if !ok || (keep != nil && !keep(g)) {
		return transcriptPage{}, false
	}
	var rows []CallRow
	if st := a.agents[id]; st != nil {
		rows = a.rowsLocked(st, "", keep != nil, q, nil)
	}
	return pageOf(rows, q), true
}

// transcriptOf is the page of the rows of every gardener scope covers and keep keeps, the query's, each with its id.
func (a *activity) transcriptOf(scope transcriptScope, keep func(FarmGardener) bool, q transcriptQuery) transcriptPage {
	a.mu.Lock()
	defer a.mu.Unlock()
	var rows []CallRow
	for id, g := range a.gardeners {
		st := a.agents[id]
		if st == nil || !scope.keeps(id, g) || (keep != nil && !keep(g)) {
			continue
		}
		rows = a.rowsLocked(st, id, keep != nil, q, rows)
	}
	return pageOf(rows, q)
}

func (a *activity) rowsLocked(st *actState, id string, mask bool, q transcriptQuery, out []CallRow) []CallRow {
	st.ring.each(func(r CallRow) {
		if !q.wants(r) {
			return
		}
		r.ID = id
		if mask {
			r = r.masked()
		}
		out = append(out, r)
	})
	return out
}

// pageOf orders rows newest first (by at, then rows written later first) and cuts q's page from them: with no cursor
// the limit newest; past an older cursor the limit next older; past a newer cursor the limit next newer, the ones
// nearest it. next is set when older rows than the page matched, prev whenever the page has rows (newer ones may yet
// be written), and to the cursor itself for an empty page asked newer, to ask again later.
func pageOf(rows []CallRow, q transcriptQuery) transcriptPage {
	slices.SortFunc(rows, func(x, y CallRow) int { return x.key().newestFirst(y.key()) })
	i, j := 0, min(q.limit, len(rows)) // the page is rows[i:j]
	// past is the first row not newer than the cursor (newer), or the first older than it.
	past := func(newer bool) int {
		n, _ := slices.BinarySearchFunc(rows, q.cursor.key, func(r CallRow, k rowKey) int {
			if c := r.key().newestFirst(k); c != 0 || newer {
				return c
			}
			return -1 // the cursor's own row is behind an older cursor
		})
		return n
	}
	p := transcriptPage{}
	switch {
	case q.cursor == nil:
		p.more = j < len(rows)
	case q.cursor.newer:
		j = past(true)
		i = max(0, j-q.limit)
		p.more = i > 0
	default:
		i = past(false)
		j = min(i+q.limit, len(rows))
		p.more = j < len(rows)
	}
	switch p.rows = rows[i:j:j]; {
	case len(p.rows) > 0:
		if j < len(rows) {
			p.next = transcriptCursor{key: rows[j-1].key()}.String()
		}
		p.prev = transcriptCursor{key: rows[i].key(), newer: true}.String()
	case q.cursor != nil && q.cursor.newer:
		p.rows, p.prev = []CallRow{}, q.cursor.String()
	default:
		p.rows = []CallRow{}
	}
	return p
}

// SetTranscript opens the transcript scope ("<gardener id>", "farm", "project:<p>", "plot:<task id>"; "" closes it) on
// the activity stream sid, which is then sent `call` events. false when there is no such stream.
func (f *Farm) SetTranscript(sid, scope string) (bool, error) { return f.act.setTranscript(sid, scope) }

// CutKeeps is the gardeners a scoped cut keeps, for a request outside its stream (the transcript endpoints).
func (f *Farm) CutKeeps(cut FarmCut) func(FarmGardener) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	cs := f.cutStreamLocked(cut)
	cs.used = f.now()
	return cs.keeps
}

// transcriptKeep is the requester's cut of gardeners, as farmEvents builds it for the activity events.
func (h *Handler) transcriptKeep(r *http.Request) func(FarmGardener) bool {
	var keep func(FarmGardener) bool
	if h.SeesGardener != nil {
		keep = h.SeesGardener(r)
	}
	if cut := h.cutOf(r); cut.Scoped {
		keep = keepBoth(keep, h.Farm.CutKeeps(cut))
	}
	return keep
}

// transcriptQueryOf reads since, before, cursor and limit (at most max, the default); false for a number that is not one
// or a cursor the endpoints did not hand out.
func transcriptQueryOf(r *http.Request, maxRows int) (transcriptQuery, bool) {
	q := transcriptQuery{limit: maxRows}
	for name, dst := range map[string]*int64{"since": &q.since, "before": &q.before} {
		if v := r.URL.Query().Get(name); v != "" {
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil || n < 0 {
				return q, false
			}
			*dst = n
		}
	}
	if v := r.URL.Query().Get("cursor"); v != "" {
		c, ok := parseTranscriptCursor(v)
		if !ok {
			return q, false
		}
		q.cursor = &c
	}
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return q, false
		}
		q.limit = min(n, maxRows)
	}
	return q, true
}

// TranscriptDoc is the answer of the transcript endpoints: one gardener's rows (id) or a scope's (scope; each row has
// its id), newest first. more says rows past limit matched. next pages back: ask again with cursor=next for the rows
// older than these, none lost or repeated at one second; prev asks for the rows newer than these (pageOf).
type TranscriptDoc struct {
	ID    string    `json:"id,omitempty"`
	Scope string    `json:"scope,omitempty"`
	Rows  []CallRow `json:"rows"`
	More  bool      `json:"more,omitempty"`
	Next  string    `json:"next,omitempty"`
	Prev  string    `json:"prev,omitempty"`
}

func transcriptDoc(id, scope string, p transcriptPage) TranscriptDoc {
	return TranscriptDoc{ID: id, Scope: scope, Rows: p.rows, More: p.more, Next: p.next, Prev: p.prev}
}

// farmTranscript serves GET /garden/farm/activity/{id}?since=&before=&cursor=&limit= (at most the ring's rows). 404 for
// an id the viewer's cut does not keep, never a 403 that would say it exists.
func (h *Handler) farmTranscript(w http.ResponseWriter, r *http.Request) {
	if h.Farm == nil {
		http.Error(w, "farm read model not enabled", http.StatusServiceUnavailable)
		return
	}
	q, ok := transcriptQueryOf(r, h.Farm.act.ringRows)
	if !ok {
		http.Error(w, "bad query", http.StatusBadRequest)
		return
	}
	id := r.PathValue("id")
	p, ok := h.Farm.act.transcript(id, h.transcriptKeep(r), q)
	if !ok {
		http.NotFound(w, r)
		return
	}
	writeDoc(w, "activity", transcriptDoc(id, "", p))
}

// farmTranscriptScope serves GET /garden/farm/activity?scope=farm|project:<p>|plot:<task id>&since=&before=&cursor=
// &limit= (at most transcriptScopeRows): the rows of every gardener in the scope that the viewer's cut keeps, merged.
func (h *Handler) farmTranscriptScope(w http.ResponseWriter, r *http.Request) {
	if h.Farm == nil {
		http.Error(w, "farm read model not enabled", http.StatusServiceUnavailable)
		return
	}
	raw := r.URL.Query().Get("scope")
	if raw == "" {
		raw = "farm"
	}
	scope, ok := parseTranscriptScope(raw)
	q, qok := transcriptQueryOf(r, transcriptScopeRows)
	if !ok || !qok || scope.kind == "" {
		http.Error(w, "bad query", http.StatusBadRequest) // one gardener's is /garden/farm/activity/{id}
		return
	}
	writeDoc(w, "activity", transcriptDoc("", raw, h.Farm.act.transcriptOf(scope, h.transcriptKeep(r), q)))
}
