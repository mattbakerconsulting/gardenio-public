// Package gardenhub is the backend for the /garden page: an in-memory snapshot
// read model of the fleet's live work, kept current by daemon bead events and
// fanned out to browsers as versioned SSE patches.
//
// Why it exists (gd-BKx5ddIilZ): a page that queries the daemon per request
// cannot reliably load in under a second, and a page that never refreshes goes
// stale. The two are solved separately:
//
//   - Load path: the page is rendered from the in-memory snapshot (no daemon or
//     DB call on the request path), so time-to-first-byte is a map read plus
//     string concatenation. Item fragments are rendered once per change, not
//     once per viewer.
//   - Freshness path: ONE upstream event stream marks the snapshot dirty when
//     an event can change it (classify, relevance.go); a coalescing refresher re-reads the daemon at most every refreshCoalesce,
//     diffs against the previous snapshot, bumps a version and broadcasts a
//     patch. Browsers connect with ?since=<version the page was rendered at>,
//     so there is no window between "page rendered" and "stream connected" in
//     which an update can be lost. A client that fell off the replay ring (or
//     talks to a restarted process) gets a full reset instead — always safe.
//
// Events are an invalidation signal, not the source of truth: a periodic
// reconcile re-reads the daemon even when no event arrived, so a lost event
// cannot leave the snapshot stale for longer than reconcileEvery.
package gardenhub

import (
	"context"
	"encoding/json"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/decisionview"
)

const (
	// eventTopics is the daemon topic globs used purely as a "something changed"
	// signal. beads.bead.> is the activity feed's upstream subscription and the
	// only family classify reads; comments, links and labels only move the open
	// bead panels (live.go, Lands WB-14).
	eventTopics = "beads.bead.>,beads.comment.>,beads.dependency.>,beads.label.>"

	// refreshCoalesce is the trailing window after the first event of a burst
	// before the daemon is re-read. A busy fleet emits many events a second;
	// this bounds the daemon load to ~1/refreshCoalesce queries regardless of
	// event rate while keeping end-to-end freshness well inside 1-2s.
	refreshCoalesce = 400 * time.Millisecond

	// reconcileEvery re-reads the daemon even with no events, repairing the
	// snapshot after a dropped event or a silently dead upstream stream.
	reconcileEvery = 30 * time.Second

	// reconnectWait is the pause before re-opening a closed upstream stream.
	reconnectWait = 2 * time.Second

	// retryAfterError is how soon a failed refresh is retried.
	retryAfterError = 2 * time.Second

	// listLimit caps each daemon list. The garden is a glance at what is alive
	// right now; a fleet beyond this is truncated (and logged) rather than slow.
	listLimit = 500

	// ringCapacity bounds how many patches are kept for ?since= replay. A
	// client further behind than this gets a reset.
	ringCapacity = 256

	// clientBuffer is the per-subscriber queue depth. A laggard that fills it
	// is collapsed to a single reset marker instead of blocking the hub.
	clientBuffer = 32

	refreshTimeout = 10 * time.Second
)

// Sections of the garden, in display order.
const (
	SecDecision = "decision" // open decisions and escalations: ripe, waiting to be harvested by a human (the Mailbox)
	SecWork     = "work"     // in-progress non-agent beads: growing
	SecAgent    = "agent"    // in-progress agent beads: the gardeners in the field
)

// Sections lists the display sections in order.
var Sections = []string{SecDecision, SecWork, SecAgent}

// Source is the slice of the beads daemon client the hub needs. Keeping it this
// narrow lets the hub be tested without the full viewer.
type Source interface {
	ListBeadsFiltered(ctx context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error)
	EventStream(ctx context.Context, topics string) (<-chan beadsapi.SSEEvent, error)
}

// Item is one plant in the garden. It is comparable on purpose: the refresh diff
// is a plain == between the old and new snapshot entry.
type Item struct {
	ID       string
	Section  string
	Title    string
	Type     string
	Project  string // the project: label (an agent's fields.project when it has none); data-pj on every row, "" for none
	Assignee string
	State    string // agent_state for agents
	Role     string // fields.role for agents: data-role on every agent row, so the Yard dresses a villager by its role before the farm document is in (gd-wJVEevAYPP)
	Operator string // lowercased operated_by alias, used for the per-user "mine" view
	Priority int
	Updated  int64 // unix seconds
	Blocked  bool
	Report   bool

	// Mailbox detail, set only for the decision section so the dock can answer in place.
	// Strings (not slices) keep Item comparable for the refresh diff.
	Ask     string // clipped prompt (decision) or context (escalation)
	Options string // raw decision options JSON
	// Context is a decision's background (decisionview.Context: the context
	// field, else the description), clipped to contextMax, so the Mailbox row
	// and its SSE patches carry what the question needs to be answered
	// (gd-hmwSI9lwKe: a resolve control is never shown without its context).
	Context string
	// Evidence is a decision's screenshot strip (decisionview.EvidenceStrip,
	// gd-Pi7C83262N), rendered once per refresh like the row: the images on
	// the beads it is about, under the context. "" when there are none.
	Evidence string
	OpType   string // escalation operation_type
	// InHand marks an escalation that is not the owner's (gd-bmOiteQi36): the
	// desk or the operators have it, so the Mailbox lists it without counting
	// it in "needs you" or the pips.
	InHand bool
	// Held names the agent lane holding a non-floor escalation
	// (AgentLaneHolder, e.g. "the merge desk"): its row says the lane
	// has it and offers only "Approve anyway", never a primary Approve
	// (gd-Mq0uDXHswK). "" keeps Approve.
	Held string
	// AskedOf names the operator a decision is asked of, "" when no name is
	// known; the page says "asked of you" on the viewer's own (web/garden.js).
	AskedOf string

	// Arms are the bead's line arms (line_arms.go, gd-PUTYxm5BtX): its epic's
	// run in the canonical form, how many emoji it holds and its plain-words
	// name, drawn before the title. Empty when the line bears none.
	Arms     string
	ArmsN    int
	ArmsName string
}

// Msg is one pre-serialised SSE message. Marshalling happens once per
// broadcast, not once per client.
type Msg struct {
	Event string // "patch" or "reset"
	V     uint64
	Data  []byte
}

type frag struct {
	ID   string `json:"id"`
	Sec  string `json:"sec"`
	HTML string `json:"html"`
}

type patch struct {
	V       uint64   `json:"v"`
	Prev    uint64   `json:"prev"`
	Upserts []frag   `json:"up,omitempty"`
	Removes []string `json:"rm,omitempty"`
}

type ringEntry struct {
	prev, v uint64
	msg     Msg
}

// Stats is the hub's observability surface (served at /garden/state).
type Stats struct {
	Version       uint64    `json:"version"`
	Items         int       `json:"items"`
	Clients       int       `json:"clients"`
	RingLen       int       `json:"ring_len"`
	BuiltAt       time.Time `json:"built_at"`
	SnapshotAgeMS int64     `json:"snapshot_age_ms"`
	Refreshes     uint64    `json:"refreshes"`
	Changes       uint64    `json:"changes"`
	Resets        uint64    `json:"resets"`
	LastRefreshMS int64     `json:"last_refresh_ms"`
	LastError     string    `json:"last_error,omitempty"`
	Upstream      bool      `json:"upstream_connected"`
	// LivePanels is the open bead panels subscribed to their bead (live.go).
	LivePanels int `json:"live_panels"`
}

// Hub holds the snapshot and the subscriber set. A nil *Hub is not usable;
// callers construct it with New.
type Hub struct {
	src      Source
	logger   *slog.Logger
	basePath string

	dirty chan struct{}

	mu        sync.RWMutex
	items     map[string]Item
	frags     map[string]string // item ID -> rendered fragment (kept in lockstep with items)
	version   uint64
	builtAt   time.Time
	ring      []ringEntry
	clients   map[int]chan Msg
	nextID    int
	resetMemo struct {
		v   uint64
		msg Msg
	}

	refreshes, changes, resets uint64
	lastRefresh                time.Duration
	lastErr                    string
	upstream                   bool

	// onEvent, when set, is called with what each upstream event can change
	// (when that is the farm or its roster) and with relevantAll on every
	// upstream (re)connect, so Farm rides this hub's ONE event subscription.
	onEvent func(Relevance)

	// moves tells the open bead panels that their bead moved (live.go).
	moves moves

	// line caches the line arms of the ledger's beads (line_arms.go).
	line *lineArms
	// evidence caches the Mailbox decisions' evidence images between
	// refreshes (dressEvidence).
	evidence *decisionview.EvidenceCache

	cancel context.CancelFunc
	done   chan struct{}
}

// OnEvent registers fn to be called with what each upstream event can change,
// and with relevantAll on every (re)connect. Call it before Start.
func (h *Hub) OnEvent(fn func(Relevance)) { h.onEvent = fn }

// New builds a hub. basePath prefixes the bead links inside rendered fragments.
func New(src Source, basePath string, logger *slog.Logger) *Hub {
	if logger == nil {
		logger = slog.Default()
	}
	return &Hub{
		src:      src,
		logger:   logger,
		basePath: basePath,
		dirty:    make(chan struct{}, 1),
		items:    map[string]Item{},
		frags:    map[string]string{},
		// The version base is wall-clock so a restarted process always issues
		// versions strictly above anything a previous incarnation handed out;
		// stale ?since= values then miss the ring and trigger a clean reset.
		version:  uint64(time.Now().UnixMilli()),
		clients:  map[int]chan Msg{},
		done:     make(chan struct{}),
		line:     newLineArms(),
		evidence: decisionview.NewEvidenceCache(0),
	}
}

// Start warms the snapshot synchronously (bounded by refreshTimeout) so the
// first page load never sees an empty garden, then launches the background
// refresher and the upstream event watcher.
func (h *Hub) Start() {
	// The hub reads for no human: with requireUserBearerForReads on, a read
	// carrying neither a bearer nor this marker is refused before it is sent,
	// which would leave /garden empty for everyone (gd-HMMeTk5yVz).
	ctx, cancel := context.WithCancel(beadsapi.WithSystemRead(context.Background()))
	h.cancel = cancel

	warm, wcancel := context.WithTimeout(ctx, refreshTimeout)
	if err := h.refresh(warm); err != nil {
		h.logger.Warn("garden hub: initial snapshot failed — will retry", "error", err)
		h.markDirty()
	}
	wcancel()

	go h.watch(ctx)
	go h.loop(ctx)
}

// Close stops the background goroutines and closes every subscriber channel.
func (h *Hub) Close() {
	if h == nil || h.cancel == nil {
		return
	}
	h.cancel()
	<-h.done
	h.moves.closeAll()
	h.mu.Lock()
	defer h.mu.Unlock()
	for id, ch := range h.clients {
		close(ch)
		delete(h.clients, id)
	}
}

func (h *Hub) markDirty() {
	select {
	case h.dirty <- struct{}{}:
	default:
	}
}

// watch keeps one upstream event stream open, reconnecting forever. An event
// only marks the snapshot dirty, and only when classify says it can change it;
// the snapshot is always re-read from the daemon (see package doc).
func (h *Hub) watch(ctx context.Context) {
	for ctx.Err() == nil {
		events, err := h.src.EventStream(ctx, eventTopics)
		if err != nil {
			h.setUpstream(false)
			h.logger.Warn("garden hub: opening upstream stream — retrying", "error", err)
			if !sleep(ctx, reconnectWait) {
				return
			}
			continue
		}
		h.setUpstream(true)
		h.markDirty() // events may have been missed while disconnected
		h.fireEvent(relevantAll)
		h.moves.touchAll()
	pump:
		for {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-events:
				if !ok {
					break pump
				}
				r := classify(ev)
				if r.Hub {
					h.markDirty()
				}
				h.fireEvent(r)
				if ev.Event == "beads.comment.added" {
					h.moves.touchComment(movedIDs(ev)...)
				} else {
					h.moves.touch(movedIDs(ev)...)
				}
			}
		}
		h.setUpstream(false)
		if !sleep(ctx, reconnectWait) {
			return
		}
	}
}

func (h *Hub) fireEvent(r Relevance) {
	if h.onEvent != nil && (r.Farm || r.Roster) {
		h.onEvent(r)
	}
}

func (h *Hub) setUpstream(v bool) {
	h.mu.Lock()
	h.upstream = v
	h.mu.Unlock()
}

// loop is the coalescing refresher.
func (h *Hub) loop(ctx context.Context) {
	defer close(h.done)
	reconcile := time.NewTicker(reconcileEvery)
	defer reconcile.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-reconcile.C:
		case <-h.dirty:
			// Trailing coalesce: let the burst finish, then read once.
			if !sleep(ctx, refreshCoalesce) {
				return
			}
			select {
			case <-h.dirty:
			default:
			}
		}
		rctx, cancel := context.WithTimeout(ctx, refreshTimeout)
		err := h.refresh(rctx)
		cancel()
		if err != nil {
			h.logger.Warn("garden hub: refresh failed", "error", err)
			// Serve the previous snapshot, retry soon.
			go func() {
				if sleep(ctx, retryAfterError) {
					h.markDirty()
				}
			}()
		}
	}
}

// refresh re-reads the daemon, diffs against the current snapshot and, when
// something changed, bumps the version and broadcasts a patch.
func (h *Hub) refresh(ctx context.Context) error {
	start := time.Now()
	next, err := h.fetch(ctx)
	if err != nil {
		h.mu.Lock()
		h.lastErr = err.Error()
		h.mu.Unlock()
		return err
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	h.refreshes++
	h.lastRefresh = time.Since(start)
	h.lastErr = ""
	h.builtAt = time.Now()

	var ups []frag
	for id, it := range next {
		if old, ok := h.items[id]; ok && old == it {
			continue
		}
		html := renderItem(it, h.basePath)
		h.frags[id] = html
		ups = append(ups, frag{ID: id, Sec: it.Section, HTML: html})
	}
	var rms []string
	for id := range h.items {
		if _, ok := next[id]; !ok {
			rms = append(rms, id)
			delete(h.frags, id)
		}
	}
	if len(ups) == 0 && len(rms) == 0 {
		return nil
	}
	h.items = next
	sort.Slice(ups, func(i, j int) bool { return ups[i].ID < ups[j].ID })
	sort.Strings(rms)

	prev := h.version
	h.version++
	h.changes++
	data, _ := json.Marshal(patch{V: h.version, Prev: prev, Upserts: ups, Removes: rms})
	msg := Msg{Event: "patch", V: h.version, Data: data}
	h.ring = append(h.ring, ringEntry{prev: prev, v: h.version, msg: msg})
	if len(h.ring) > ringCapacity {
		h.ring = h.ring[len(h.ring)-ringCapacity:]
	}
	for _, ch := range h.clients {
		send(ch, msg)
	}
	return nil
}

// send enqueues msg for one client. A client whose queue is full is collapsed
// to a single reset marker (Event "reset", nil Data): the handler answers it
// with a fresh full snapshot, so a laggard degrades to "latest state" and never
// blocks the hub or its peers.
func send(ch chan Msg, msg Msg) {
	select {
	case ch <- msg:
		return
	default:
	}
	for {
		select {
		case <-ch:
		default:
			select {
			case ch <- Msg{Event: "reset"}:
			default:
			}
			return
		}
	}
}

// fetch reads the four lists. None asks for its total: only truncation is
// logged, and MayHaveMore answers that without the daemon's full-set COUNT(*)
// (gd-I2zntgOsCq).
func (h *Hub) fetch(ctx context.Context) (map[string]Item, error) {
	active, err := h.src.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Statuses:              []string{"in_progress"},
		Limit:                 listLimit,
		ExcludeScheduled:      true,
		WithHasReport:         true,
		WithBlockedOnDecision: true,
		NoTotal:               true,
	})
	if err != nil {
		return nil, err
	}
	decisions, err := h.src.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"decision"},
		Statuses: []string{"open"},
		Limit:    listLimit,
		NoTotal:  true,
	})
	if err != nil {
		return nil, err
	}
	escalations, err := h.src.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"escalation"},
		Statuses: []string{"open"},
		Limit:    listLimit,
		NoTotal:  true,
	})
	if err != nil {
		return nil, err
	}
	// Agent beads are status=open from spawn until they close; only a claimed
	// task is in_progress. The in_progress list alone therefore never saw a
	// gardener (Phase 1 shipped with 0 drawn, even in the Everyone view).
	agents, err := h.src.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"agent"},
		Statuses: []string{"open", "in_progress"},
		Limit:    listLimit,
		NoTotal:  true,
	})
	if err != nil {
		return nil, err
	}
	out := make(map[string]Item, len(active.Beads)+len(decisions.Beads)+len(escalations.Beads)+len(agents.Beads))
	for _, b := range agents.Beads {
		if b != nil && b.Type == "agent" {
			out[b.ID] = itemFromBead(b, SecAgent)
		}
	}
	for _, b := range decisions.Beads {
		if b != nil {
			out[b.ID] = itemFromBead(b, SecDecision)
		}
	}
	for _, b := range escalations.Beads {
		if b != nil && EscalationState(b) == "pending" { // an approved or rejected one, still open, is nobody's to answer (gd-yuOG6cX6VI)
			out[b.ID] = itemFromBead(b, SecDecision)
		}
	}
	for _, b := range active.Beads {
		if b == nil || b.Type == "decision" || b.Type == "escalation" {
			continue // a letter in hand is the Mailbox's or the Escalations cell's, never a plant in the field (gd-oChRvR8mg2)
		}
		sec := SecWork
		if b.Type == "agent" {
			sec = SecAgent
		}
		out[b.ID] = itemFromBead(b, sec)
	}
	h.dressArms(ctx, out)
	h.dressEvidence(ctx, out, decisions.Beads)
	if active.MayHaveMore() || decisions.MayHaveMore() || escalations.MayHaveMore() || agents.MayHaveMore() {
		h.logger.Warn("garden hub: daemon list may be truncated", "in_progress", len(active.Beads), "decisions", len(decisions.Beads), "escalations", len(escalations.Beads), "agents", len(agents.Beads), "limit", listLimit)
	}
	return out, nil
}

// Snapshot is a consistent copy of the current state for rendering a page.
type Snapshot struct {
	V       uint64
	BuiltAt time.Time
	Frags   map[string][]string // section -> fragments, most recently updated first
	Counts  map[string]int
}

// Snapshot returns the current version and the rendered fragments, grouped by
// section and ordered newest-first. It performs no I/O.
func (h *Hub) Snapshot() Snapshot {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.snapshotLocked(nil)
}

// CutSnapshot is Snapshot for a project-scoped viewer: only the items whose project: label is in the cut's projects
// (an unlabelled item cannot be decided, so it goes), counted over what shows. An unscoped cut is Snapshot. The hub's
// stream (Subscribe) has no such cut, so GET /garden/events stays admin-only and a scoped page paints these lists once.
func (h *Hub) CutSnapshot(cut FarmCut) Snapshot {
	if !cut.Scoped {
		return h.Snapshot()
	}
	allow := allowIn(cut.Projects)
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.snapshotLocked(func(it Item) bool { return allow(it.Project) })
}

// snapshotLocked is the snapshot of the items keep admits; nil keeps every item.
func (h *Hub) snapshotLocked(keep func(Item) bool) Snapshot {
	by := map[string][]Item{}
	for _, it := range h.items {
		if keep != nil && !keep(it) {
			continue
		}
		by[it.Section] = append(by[it.Section], it)
	}
	s := Snapshot{V: h.version, BuiltAt: h.builtAt, Frags: map[string][]string{}, Counts: map[string]int{}}
	for sec, list := range by {
		sort.Slice(list, func(i, j int) bool {
			if list[i].Updated != list[j].Updated {
				return list[i].Updated > list[j].Updated
			}
			return list[i].ID < list[j].ID
		})
		for _, it := range list {
			s.Frags[sec] = append(s.Frags[sec], h.frags[it.ID])
		}
		s.Counts[sec] = len(list)
	}
	return s
}

type resetPayload struct {
	V    uint64            `json:"v"`
	HTML map[string]string `json:"html"`
}

// resetLocked builds (and memoises per version) the full-state reset message.
func (h *Hub) resetLocked() Msg {
	if h.resetMemo.v == h.version && h.resetMemo.msg.Data != nil {
		return h.resetMemo.msg
	}
	snap := h.snapshotLocked(nil)
	p := resetPayload{V: snap.V, HTML: map[string]string{}}
	for _, sec := range Sections {
		var b []byte
		for _, f := range snap.Frags[sec] {
			b = append(b, f...)
		}
		p.HTML[sec] = string(b)
	}
	data, _ := json.Marshal(p)
	msg := Msg{Event: "reset", V: h.version, Data: data}
	h.resetMemo.v, h.resetMemo.msg = h.version, msg
	return msg
}

// Reset returns a full-state reset message for the current version.
func (h *Hub) Reset() Msg {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.resets++
	return h.resetLocked()
}

// Subscribe registers a client that last saw version `since` (0 = unknown). It
// returns the catch-up messages to send first (zero, some patches, or one
// reset) and a live channel. The catch-up and the registration happen under one
// lock, so no patch can fall between them.
func (h *Hub) Subscribe(since uint64) (catchUp []Msg, live <-chan Msg, cancel func()) {
	h.mu.Lock()
	defer h.mu.Unlock()

	switch {
	case since == h.version:
		// Up to date: nothing to replay.
	default:
		if replay, ok := h.replayLocked(since); ok {
			catchUp = replay
		} else {
			h.resets++
			catchUp = []Msg{h.resetLocked()}
		}
	}

	id := h.nextID
	h.nextID++
	ch := make(chan Msg, clientBuffer)
	h.clients[id] = ch

	var once sync.Once
	cancel = func() {
		once.Do(func() {
			h.mu.Lock()
			defer h.mu.Unlock()
			if c, ok := h.clients[id]; ok {
				delete(h.clients, id)
				close(c)
			}
		})
	}
	return catchUp, ch, cancel
}

// replayLocked returns the chain of patches taking a client from `since` to the
// current version, or false when the ring cannot cover it.
func (h *Hub) replayLocked(since uint64) ([]Msg, bool) {
	if since == 0 || since > h.version {
		return nil, false
	}
	start := -1
	for i, e := range h.ring {
		if e.prev == since {
			start = i
			break
		}
	}
	if start < 0 {
		return nil, false
	}
	out := make([]Msg, 0, len(h.ring)-start)
	want := since
	for _, e := range h.ring[start:] {
		if e.prev != want {
			return nil, false
		}
		out = append(out, e.msg)
		want = e.v
	}
	if want != h.version {
		return nil, false
	}
	return out, true
}

// Stats returns a point-in-time view of the hub for diagnostics.
func (h *Hub) Stats() Stats {
	h.mu.RLock()
	defer h.mu.RUnlock()
	age := int64(-1)
	if !h.builtAt.IsZero() {
		age = time.Since(h.builtAt).Milliseconds()
	}
	return Stats{
		Version: h.version, Items: len(h.items), Clients: len(h.clients), RingLen: len(h.ring),
		BuiltAt: h.builtAt, SnapshotAgeMS: age, Refreshes: h.refreshes, Changes: h.changes,
		Resets: h.resets, LastRefreshMS: h.lastRefresh.Milliseconds(), LastError: h.lastErr,
		Upstream: h.upstream, LivePanels: h.moves.count(),
	}
}

// dressEvidence gives the Mailbox's decisions their evidence strips: the
// screenshots on the beads each is about, read through the cache so a refresh
// a few seconds after the last reads nothing. A source without comment reads
// (an older test fake) shows none, and a failed read shows the row without it.
func (h *Hub) dressEvidence(ctx context.Context, items map[string]Item, decisions []*beadsapi.BeadDetail) {
	src, ok := h.src.(decisionview.EvidenceReader)
	if !ok {
		return
	}
	for id, imgs := range h.evidence.LoadAll(ctx, src, decisions, 0) {
		it, ok := items[id]
		if !ok || it.Section != SecDecision {
			continue
		}
		it.Evidence = string(evidenceStrip(h.basePath, "mb", id, imgs))
		items[id] = it
	}
}

// dressArms gives the Mailbox's and the growing beads their line arms (line_arms.go), and the barn's agents their
// task's (badge.Node walks an agent on to its task; heraldry step 5.9, gd-QboM9sPYCg). A source without the batch reads
// shows none.
func (h *Hub) dressArms(ctx context.Context, items map[string]Item) {
	src, ok := h.src.(lineSource)
	if !ok {
		return
	}
	ids := make([]string, 0, len(items))
	for id := range items {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for id, a := range h.line.resolve(ctx, src, ids) {
		it := items[id]
		it.Arms, it.ArmsN, it.ArmsName = a.Canonical, a.N, a.Name
		items[id] = it
	}
}

func itemFromBead(b *beadsapi.BeadDetail, sec string) Item {
	it := Item{
		ID:       b.ID,
		Section:  sec,
		Title:    b.Title,
		Type:     b.Type,
		Assignee: b.Assignee,
		Operator: lower(b.OperatedBy),
		Priority: b.Priority,
		Updated:  b.UpdatedAt.Unix(),
		Blocked:  b.BlockedOnDecision,
		Report:   b.HasReport,
	}
	for _, l := range b.Labels {
		if len(l) > 8 && l[:8] == "project:" {
			it.Project = l[8:]
			break
		}
	}
	if sec == SecAgent {
		it.State = b.Fields["agent_state"]
		it.Role = strings.TrimSpace(b.Fields["role"])
		if it.Project == "" {
			// An agent bead may carry its project only as a field; the farm's gardener reads it the same way
			// (gardenerOf), so a gardener and its ledger row stand in the same project's barn (gd-GYo4PWlGUb).
			it.Project = strings.TrimSpace(b.Fields["project"])
		}
	}
	if sec == SecDecision {
		switch b.Type {
		case "decision":
			d := decisionview.From(b)
			it.Ask = clip(d.Prompt, askMax)
			it.Options = b.Fields["options"]
			it.Context = clipKeep(d.Context, contextMax)
			it.AskedOf = operatorName(b)
		case "escalation":
			it.Ask = clip(b.Fields["context"], askMax)
			it.OpType = b.Fields["operation_type"]
			it.InHand = !beadsapi.EscalationForOwner(b.Fields)
			it.Held = AgentLaneHolder(b.Fields)
		}
		if strings.TrimSpace(it.Ask) == strings.TrimSpace(it.Title) {
			it.Ask = "" // a prompt that only repeats the title says nothing the row's link does not
		}
	}
	return it
}

// operatorName is the bead's operator as a person reads it: the daemon's display
// name, never a raw id or address, and not the fleet's own name, which the
// daemon stamps on beads a human drove through Slack (ka-0000003240).
func operatorName(b *beadsapi.BeadDetail) string {
	n := strings.TrimSpace(b.OperatedByDisplay)
	if n == "" || strings.EqualFold(n, "gardenio") || strings.ContainsAny(n, ":@") {
		return ""
	}
	return n
}

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
