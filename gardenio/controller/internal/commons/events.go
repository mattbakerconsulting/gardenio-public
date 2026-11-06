package commons

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// The event stream, GET /garden/c/events, is server-sent events. Each event is
// named for what changed, and in the channel or under the root the viewer has
// open it carries the content too (stream.go):
//
//	id: <RFC3339Nano time>
//	event: message | card | channel | presence | typing | node
//	data: {"id":"…","channel":"…","thread":"…","land":"…", …content}
//
// presence names a channel (or, with an empty id and channel, only a land)
// whose people or agents changed; it is live only, never replayed. A stream
// that names ?land=<land> and optionally &channel=<name or id> puts its viewer
// in Presence there for as long as it stays open (presence.go). typing names
// a channel, and with thread a thread in it, where someone started or stopped
// typing; it is live only too (typing.go).
//
// After any replay it sends "replayed" ({"count":n}), and "ping" every
// heartbeatEvery and once at the start of a stream with no replay; both carry
// an id too, so a reconnect replays from the last moment the stream was known
// current. "reset" means the replay could not cover the gap (it reached
// further back than replayWindow, or past replayIDs beads): refetch whatever
// is on screen.
//
// Replay: ?since=<RFC3339> or the Last-Event-ID header the browser sends on
// reconnect replays the conversation beads that changed since then, from the
// daemon's durable event log read on the viewer's own bearer, each id once.
// The live subscription opens before the replay, so nothing falls between the
// two; an id may arrive twice, which putting a row in place makes harmless.
// A replayed message in the channel on screen carries its row too.

const (
	heartbeatEvery = 15 * time.Second
	// replayWindow is how far back a replay reaches; an older since resets.
	replayWindow = time.Hour
	// replaySlack widens every replay backwards: a live id can still be on its
	// way through the hub when a later heartbeat stamps the stream.
	replaySlack = 5 * time.Second
	// replayPage and replayEvents bound the durable log read.
	replayPage   = 200
	replayEvents = 2000
	// replayIDs bounds the distinct beads one replay attributes (one daemon
	// read each); more than that resets. replayReads of those reads run at once.
	replayIDs   = 200
	replayReads = 8
)

// events serves GET /garden/c/events.
func (h *Handler) events(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	sc := h.scope(r)
	bound := h.StreamContext
	if bound == nil {
		bound = context.WithCancel
	}
	ctx, cancel := bound(r.Context())
	defer cancel()

	live, stop := h.Hub.Subscribe(ctx, sc, streamWatch(r)...)
	defer stop()

	hd := w.Header()
	hd.Set("Content-Type", "text/event-stream")
	hd.Set("Cache-Control", "no-cache")
	hd.Set("Connection", "keep-alive")
	hd.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprint(w, ": commons\n\n")
	since, replaying := replaySince(r)
	if !replaying {
		// stamped from the first moment, so a page that sleeps before the first heartbeat still resumes with ?since=
		writeEvent(w, h.stamp(), "ping", "{}")
	}
	flusher.Flush()

	at, ch, err := h.streamSpot(ctx, r, sc)
	if err != nil {
		// ended, not carried on with ids only: the client asks again from the
		// ping above, and the replay covers the gap
		return
	}
	defer h.joinStream(r, at)()
	c := h.carrier(r, sc, ch)
	send := func(inv Invalidation) {
		if s, ok := c.dress(ctx, inv); ok {
			writeStreamed(w, s)
		}
	}

	if replaying {
		invs, complete := h.replay(ctx, sc, since)
		for _, inv := range invs {
			send(inv)
		}
		if !complete {
			writeEvent(w, "", "reset", "{}")
		}
		writeEvent(w, h.stamp(), "replayed", fmt.Sprintf(`{"count":%d}`, len(invs)))
		flusher.Flush()
	}

	tick := time.NewTicker(heartbeatEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			writeEvent(w, h.stamp(), "ping", "{}")
			flusher.Flush()
		case inv, ok := <-live:
			if !ok {
				live = nil // hub gone or not wired: keep the heartbeat
				continue
			}
			send(inv)
			flusher.Flush()
		}
	}
}

// replaySince reads the replay point from Last-Event-ID, else ?since=. The
// header wins: the browser's own reconnect of a stream opened with ?since=
// keeps that URL, and its Last-Event-ID is the newer point.
func replaySince(r *http.Request) (time.Time, bool) {
	for _, v := range []string{r.Header.Get("Last-Event-ID"), r.URL.Query().Get("since")} {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		if t, err := time.Parse(time.RFC3339Nano, v); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// replay returns the conversation beads that changed since then and that sc
// may see, oldest change first, each once at its newest change. complete is
// false when since is outside replayWindow or a bound was hit.
func (h *Handler) replay(ctx context.Context, sc Scope, since time.Time) ([]Invalidation, bool) {
	if h.Beads == nil {
		return nil, false
	}
	now := h.now()
	complete := true
	since = since.Add(-replaySlack)
	if since.Before(now.Add(-replayWindow)) {
		since, complete = now.Add(-replayWindow), false
	}
	latest := map[string]Invalidation{}
	var order []string
	var cursor int64
	for read := 0; ; {
		resp, err := h.Beads.ListGlobalEvents(ctx, beadsapi.ListGlobalEventsRequest{Since: since, Cursor: cursor, Limit: replayPage})
		if err != nil || resp == nil {
			return nil, false
		}
		for _, e := range resp.Events {
			if !strings.HasPrefix(e.Topic, "beads.bead.") || e.CreatedAt.Before(since) {
				continue
			}
			inv, ok := classify(e.Payload)
			if !ok {
				continue
			}
			inv.At = e.CreatedAt
			if _, seen := latest[inv.ID]; !seen {
				order = append(order, inv.ID)
			}
			latest[inv.ID] = inv
		}
		read += len(resp.Events)
		if !resp.HasMore || len(resp.Events) == 0 {
			break
		}
		next, err := strconv.ParseInt(strings.TrimSpace(resp.NextCursor), 10, 64)
		if err != nil || next <= cursor {
			break
		}
		if read >= replayEvents {
			complete = false
			break
		}
		cursor = next
	}
	if len(order) > replayIDs {
		return nil, false
	}
	lands := make([]string, len(order))
	var wg sync.WaitGroup
	sem := make(chan struct{}, replayReads)
	for i, id := range order {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer func() { <-sem; wg.Done() }()
			lands[i], _ = h.attribute(ctx, id)
		}()
	}
	wg.Wait()
	out := make([]Invalidation, 0, len(order))
	for i, id := range order {
		if !sc.Allows(lands[i]) {
			continue
		}
		inv := latest[id]
		inv.Land = lands[i]
		out = append(out, inv)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out, complete
}

// attribute reads a replayed bead's land on the viewer's own bearer, so the
// daemon refuses a bead outside the viewer's projects before the scope check.
func (h *Handler) attribute(ctx context.Context, id string) (string, bool) {
	ctx, cancel := context.WithTimeout(ctx, lookupTimeout)
	defer cancel()
	b, err := h.Beads.GetBead(ctx, id)
	if err != nil || b == nil {
		return "", false
	}
	land := landOf(b.Labels)
	return land, land != ""
}

// stamp is the event id of a heartbeat: the stream is current up to now, so a
// reconnect replays from here.
func (h *Handler) stamp() string { return h.now().UTC().Format(time.RFC3339Nano) }

func writeEvent(w http.ResponseWriter, id, name, data string) {
	if id != "" {
		_, _ = fmt.Fprintf(w, "id: %s\n", id)
	}
	_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, data)
}
