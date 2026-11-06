package gardenhub

// live.go — live panels (Lands workbench-glass spec WB-14; Phase 5 step 5.6, gd-uRytFwHkLT). An open bead panel
// subscribes by id to GET /garden/bead/{id}/events and is told only that its bead moved; it then fetches
// GET /garden/bead/{id} again as the viewer (Commons D8: events are invalidations carrying ids, the client refetches, one
// render path). The ids ride the Hub's ONE upstream subscription: a bead event names its bead, a comment or a label the
// bead it is on, and a dependency both ends (each end lists the other). A (re)connected upstream may have missed events,
// so it moves every open panel.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"gardenio/controller/internal/beadsapi"
)

const (
	// moveSettle folds a burst of events on one bead (a close is a comment, a status write and a dependency write) into
	// one "moved", so the panel fetches once.
	moveSettle = 300 * time.Millisecond
	// moveBuffer: one pending signal is all a subscriber needs; more of them say the same thing.
	moveBuffer = 1
)

// moves is the per-id fan-out: bead id -> the panels that have it open. Its own lock, so a panel connecting never waits
// on a snapshot refresh.
type moves struct {
	mu   sync.Mutex
	subs map[string]map[int]chan struct{}
	next int
	// whole marks a channel signalled since its last take by a move that was more than a comment added (gd-HprlA40a7z).
	whole map[<-chan struct{}]bool
}

// subscribe registers a panel showing id. cancel closes the channel; it is safe to call twice.
func (m *moves) subscribe(id string) (<-chan struct{}, func()) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.subs == nil {
		m.subs = map[string]map[int]chan struct{}{}
	}
	n := m.next
	m.next++
	ch := make(chan struct{}, moveBuffer)
	if m.subs[id] == nil {
		m.subs[id] = map[int]chan struct{}{}
	}
	m.subs[id][n] = ch
	return ch, func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		if c, ok := m.subs[id][n]; ok {
			delete(m.subs[id], n)
			if len(m.subs[id]) == 0 {
				delete(m.subs, id)
			}
			delete(m.whole, c)
			close(c)
		}
	}
}

// subscribeAll registers one panel showing every one of ids (the plan cockpit, plan.go): one channel, signalled when any
// of them moves. cancel closes it; it is safe to call twice.
func (m *moves) subscribeAll(ids []string) (<-chan struct{}, func()) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.subs == nil {
		m.subs = map[string]map[int]chan struct{}{}
	}
	n := m.next
	m.next++
	ch := make(chan struct{}, moveBuffer)
	var held []string
	for _, id := range ids {
		if id == "" || m.subs[id][n] != nil {
			continue
		}
		if m.subs[id] == nil {
			m.subs[id] = map[int]chan struct{}{}
		}
		m.subs[id][n] = ch
		held = append(held, id)
	}
	closed := false
	return ch, func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		if closed {
			return
		}
		closed = true
		found := len(held) == 0 // never registered, so closeAll cannot have closed it
		for _, id := range held {
			if m.subs[id][n] == ch {
				found = true
				delete(m.subs[id], n)
				if len(m.subs[id]) == 0 {
					delete(m.subs, id)
				}
			}
		}
		if found {
			delete(m.whole, ch)
			close(ch)
		}
	}
}

// poke signals ch without blocking: a signal already pending says the same thing.
func poke(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

// touch tells the panels showing any of ids that their bead moved.
func (m *moves) touch(ids ...string) { m.touchAs(true, ids) }

// touchComment is touch for a comment added to ids, which the bead view fetches alone (take).
func (m *moves) touchComment(ids ...string) { m.touchAs(false, ids) }

func (m *moves) touchAs(whole bool, ids []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, id := range ids {
		for _, ch := range m.subs[id] {
			m.mark(ch, whole)
		}
	}
}

// mark signals ch, noting a whole move. Called under mu.
func (m *moves) mark(ch chan struct{}, whole bool) {
	if whole {
		if m.whole == nil {
			m.whole = map[<-chan struct{}]bool{}
		}
		m.whole[ch] = true
	}
	poke(ch)
}

// take says whether ch was signalled since the last take by a whole move, not only comments added, and clears it.
func (m *moves) take(ch <-chan struct{}) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	w := m.whole[ch]
	delete(m.whole, ch)
	return w
}

// touchAll moves every open panel: the upstream (re)connected, and what it missed meanwhile is unknown.
func (m *moves) touchAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, set := range m.subs {
		for _, ch := range set {
			m.mark(ch, true)
		}
	}
}

// count is how many panels are subscribed (Stats).
func (m *moves) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	panels := map[int]bool{} // a cockpit is one panel under many ids (subscribeAll)
	for _, set := range m.subs {
		for n := range set {
			panels[n] = true
		}
	}
	return len(panels)
}

func (m *moves) closeAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	shut := map[chan struct{}]bool{} // a cockpit's one channel sits under each of its ids
	for id, set := range m.subs {
		for _, ch := range set {
			if !shut[ch] {
				shut[ch] = true
				close(ch)
			}
		}
		delete(m.subs, id)
	}
	m.whole = nil
}

// movePayload is every shape of upstream event that names a bead (kbeads/internal/events): BeadCreated, BeadUpdated,
// BeadClosed and BeadTransferred carry the bead; BeadDeleted, LabelAdded/Removed, CommentDeleted and DependencyRemoved its
// id (and a removed dependency the other end); CommentAdded and DependencyAdded the row.
type movePayload struct {
	Bead *struct {
		ID string `json:"id"`
	} `json:"bead"`
	BeadID      string `json:"bead_id"`
	DependsOnID string `json:"depends_on_id"`
	Comment     *struct {
		BeadID string `json:"bead_id"`
	} `json:"comment"`
	Dependency *struct {
		BeadID      string `json:"bead_id"`
		DependsOnID string `json:"depends_on_id"`
	} `json:"dependency"`
}

// movedIDs is the beads an upstream event moves, none when it names no bead.
func movedIDs(ev beadsapi.SSEEvent) []string {
	var p movePayload
	if len(ev.Data) == 0 || json.Unmarshal(ev.Data, &p) != nil {
		return nil
	}
	var out []string
	add := func(id string) {
		if id = strings.TrimSpace(id); id != "" {
			for _, o := range out {
				if o == id {
					return
				}
			}
			out = append(out, id)
		}
	}
	if p.Bead != nil {
		add(p.Bead.ID)
	}
	add(p.BeadID)
	add(p.DependsOnID)
	if p.Comment != nil {
		add(p.Comment.BeadID)
	}
	if p.Dependency != nil {
		add(p.Dependency.BeadID)
		add(p.Dependency.DependsOnID)
	}
	return out
}

// beadEvents serves GET /garden/bead/{id}/events: an SSE stream that says "moved" (data {"id"}) when the bead moves, and
// nothing else about it; {"id", "only": "comments"} when all that moved was comments added. It is only for a bead the viewer may read: the connect reads it once as the viewer, the same cut
// as GET /garden/bead/{id}, so an id the viewer cannot see is a 404 and never an oracle for when that bead changes.
func (h *Handler) beadEvents(w http.ResponseWriter, r *http.Request) {
	if h.Hub == nil || h.Beads == nil {
		http.Error(w, "live bead view not enabled", http.StatusServiceUnavailable)
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" || len(id) > 200 {
		http.Error(w, "bad bead id", http.StatusBadRequest)
		return
	}
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}
	// Subscribed before the read, so a move while it is out is not lost (the panel's own fetch may predate it).
	moved, stop := h.Hub.moves.subscribe(id)
	defer stop()
	ctx, cancel := context.WithTimeout(r.Context(), beadViewTimeout)
	b, err := h.Beads.GetBead(ctx, id)
	cancel()
	if err != nil || b == nil {
		var ae *beadsapi.APIError
		switch {
		case err == nil, errors.As(err, &ae) && (ae.StatusCode == http.StatusNotFound || ae.StatusCode == http.StatusForbidden):
			http.Error(w, "no such bead", http.StatusNotFound)
		default:
			http.Error(w, "the beads daemon did not answer", http.StatusBadGateway)
		}
		return
	}
	data, _ := json.Marshal(struct {
		ID string `json:"id"`
	}{id})
	// A burst that only added comments says so, and the panel fetches the new ones alone (beadView ?after=).
	comments, _ := json.Marshal(struct {
		ID   string `json:"id"`
		Only string `json:"only"`
	}{id, "comments"})

	hd := w.Header()
	hd.Set("Content-Type", "text/event-stream")
	hd.Set("Cache-Control", "no-cache")
	hd.Set("Connection", "keep-alive")
	hd.Set("X-Accel-Buffering", "no")
	fmt.Fprint(w, "retry: 2000\n: connected\n\n")
	fl.Flush()

	hb := time.NewTicker(heartbeatEvery)
	defer hb.Stop()
	var settle <-chan time.Time // armed by the first signal of a burst
	done := r.Context().Done()
	for {
		select {
		case <-done:
			return
		case _, ok := <-moved:
			if !ok {
				return
			}
			if settle == nil {
				settle = time.After(moveSettle)
			}
		case <-settle:
			settle = nil
			d := data
			if !h.Hub.moves.take(moved) {
				d = comments
			}
			fmt.Fprintf(w, "event: moved\ndata: %s\n\n", d)
			fl.Flush()
		case <-hb.C:
			fmt.Fprint(w, "event: hb\ndata: {}\n\n")
			fl.Flush()
		}
	}
}
