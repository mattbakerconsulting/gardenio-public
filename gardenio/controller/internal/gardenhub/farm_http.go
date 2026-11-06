package gardenhub

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"gardenio/controller/internal/servertiming"
)

// farmPage serves the full farm snapshot as JSON. Same freshness contract as
// the /garden page: rendered from memory, ETag keyed on the snapshot version (a
// revisit at the same version is a bodyless 304), Server-Timing for the budget (snapshot, gzip, snapshot-age).
// The ETag also names the activity epoch gardeners[].doing was baked at, so a revisit sees what the gardeners are
// doing now.
//
// A scoped requester (h.Cut) gets the document cut to its projects (farm_cut.go), under an ETag that hashes the cut's
// content, so one viewer's ETag never matches another set's body; Cache-Control stays private. Everyone else gets the
// one viewer-neutral document, exactly as before the cut.
func (h *Handler) farmPage(w http.ResponseWriter, r *http.Request) {
	if h.Farm == nil {
		http.Error(w, "farm read model not enabled", http.StatusServiceUnavailable)
		return
	}
	if cut := h.cutOf(r); cut.Scoped {
		h.farmPageCut(w, r, cut)
		return
	}
	etag, body, built := h.Farm.snapshotTag()
	servertiming.Lap(w, "snapshot")
	snapshotAge(w, built)
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "private, no-cache")
	w.Header().Set("Vary", "Accept-Encoding")
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		_, gz := h.Farm.SnapshotGzip()
		servertiming.Lap(w, "gzip")
		w.Header().Set("Content-Encoding", "gzip")
		_, _ = w.Write(gz)
		return
	}
	_, _ = w.Write(body)
}

// snapshotAge notes how old the served snapshot is on the Server-Timing header; nothing before the first build.
func snapshotAge(w http.ResponseWriter, built time.Time) {
	if !built.IsZero() {
		servertiming.Note(w, "snapshot-age", time.Since(built))
	}
}

// cutOf is the requester's cut; the whole farm without h.Cut.
func (h *Handler) cutOf(r *http.Request) FarmCut {
	if h.Cut == nil {
		return FarmCut{}
	}
	return h.Cut(r)
}

// farmPageCut is farmPage for a scoped requester.
func (h *Handler) farmPageCut(w http.ResponseWriter, r *http.Request, cut FarmCut) {
	etag, body, gz, built := h.Farm.CutSnapshot(cut) // the cut and its gzip are built here, once per cut and version
	servertiming.Lap(w, "snapshot")
	snapshotAge(w, built)
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "private, no-cache")
	w.Header().Set("Vary", "Accept-Encoding")
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		w.Header().Set("Content-Encoding", "gzip")
		_, _ = w.Write(gz)
		return
	}
	_, _ = w.Write(body)
}

// farmEvents streams the farm's versioned SSE patches. Connect with
// ?since=<v of the snapshot you hold> (or rely on Last-Event-ID): the catch-up
// is one patch from that version to now (farm_delta.go), else the ring's missed
// patches, else one "reset" whose data is the full snapshot document. A stream
// collapsed for lagging is caught up the same way from the last version it was
// sent, and the patches queued behind that catch-up are skipped.
//
// Between them come the ephemeral `activity` events (activity.go): no `id:` line, so they never move Last-Event-ID
// and are outside the patch chain; a reconnect does not replay them. They are cut by SeesGardener, like gardeners[],
// and then to what the page says it can see: the stream opens with a `hello` event naming its id, which the page
// POSTs to /garden/farm/events/view with its view (farmEventsView); until then it is sent every call its cut keeps.
//
// A scoped requester (h.Cut) is on its cut's own stream (farm_cut_stream.go): the patches, resets and catch-up of the
// document farmPage cuts for it, under that document's versions, and activity only for the gardeners in it.
func (h *Handler) farmEvents(w http.ResponseWriter, r *http.Request) {
	if h.Farm == nil {
		http.Error(w, "farm read model not enabled", http.StatusServiceUnavailable)
		return
	}
	cut := h.cutOf(r)
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}
	raw := r.Header.Get("Last-Event-ID")
	if raw == "" {
		raw = r.URL.Query().Get("since")
	}
	since, _ := strconv.ParseUint(raw, 10, 64)

	hd := w.Header()
	hd.Set("Content-Type", "text/event-stream")
	hd.Set("Cache-Control", "no-cache")
	hd.Set("Connection", "keep-alive")
	hd.Set("X-Accel-Buffering", "no")

	var keep func(FarmGardener) bool
	if h.SeesGardener != nil {
		keep = h.SeesGardener(r)
	}
	var (
		catchUp []Msg
		live    <-chan Msg
		cancel  func()
		resume  = h.Farm.Resume
	)
	if cut.Scoped {
		var inCut func(FarmGardener) bool
		catchUp, live, inCut, cancel = h.Farm.SubscribeCut(cut, since)
		keep = keepBoth(keep, inCut)
		resume = func(since uint64) []Msg { return h.Farm.CutResume(cut, since) }
	}
	sid, acts, cancelActs := h.Farm.SubscribeActivity(keep)
	defer cancelActs()
	if !cut.Scoped {
		catchUp, live, cancel = h.Farm.Subscribe(since)
	}
	defer cancel()

	fmt.Fprint(w, "retry: 2000\n: connected\n\n")
	hello, _ := json.Marshal(map[string]string{"sid": sid})
	fmt.Fprintf(w, "event: hello\ndata: %s\n\n", hello)
	at := since // the version the page holds once what was written so far is applied
	for _, m := range catchUp {
		writeMsg(w, m)
		at = m.V
	}
	fl.Flush()

	hb := time.NewTicker(heartbeatEvery)
	defer hb.Stop()
	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case m, ok := <-live:
			if !ok {
				return
			}
			if m.Event == "reset" && m.Data == nil {
				// Collapsed laggard: catch it up from the last version it was sent.
				for _, c := range resume(at) {
					writeMsg(w, c)
					at = c.V
				}
				fl.Flush()
				continue
			}
			if m.V <= at {
				continue // queued behind a catch-up that already carries it
			}
			writeMsg(w, m)
			at = m.V
			fl.Flush()
		case a, ok := <-acts:
			if !ok {
				return
			}
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", a.Event, a.Data)
			fl.Flush()
		case <-hb.C:
			fmt.Fprint(w, "event: hb\ndata: {}\n\n")
			fl.Flush()
		}
	}
}

// farmEventsView takes a page's view for its activity stream (gd-fBGGNdgrLc): {"sid": the hello event's, "level":
// 0-3, "ids": the gardeners in view and a margin round it}. EventSource cannot send once it is open, so the view comes
// here and the stream stays up (a reconnect would lose the birds' quiet start and the catch-up). It only ever narrows
// what the stream sends, so a stale or unknown sid is a 404 and nothing else; an id that is not a gardener is ignored.
//
// "transcript": "<gardener id>" | "farm" | "project:<p>" | "plot:<task id>" opens that transcript on the stream, which is
// then sent its rows as `call` events (transcript.go), still only for gardeners its cut keeps; "" closes it. A body with
// a transcript and no level or ids leaves the view as it was. Opening one is a 403 when h.MayTranscript says no.
func (h *Handler) farmEventsView(w http.ResponseWriter, r *http.Request) {
	if h.Farm == nil {
		http.Error(w, "farm read model not enabled", http.StatusServiceUnavailable)
		return
	}
	if crossOrigin(r) {
		http.Error(w, "cross-origin", http.StatusForbidden)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 32<<10)
	var in struct {
		SID        string   `json:"sid"`
		Level      *int     `json:"level"`
		IDs        []string `json:"ids"`
		Transcript *string  `json:"transcript"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "bad view", http.StatusBadRequest)
		return
	}
	if in.Transcript != nil && *in.Transcript != "" && h.MayTranscript != nil && !h.MayTranscript(r) {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	found := true
	if in.Transcript != nil {
		ok, err := h.Farm.SetTranscript(in.SID, *in.Transcript)
		if err != nil {
			http.Error(w, "bad transcript", http.StatusBadRequest)
			return
		}
		found = ok
	}
	if found && (in.Transcript == nil || in.Level != nil || in.IDs != nil) {
		v := ActivityView{IDs: in.IDs}
		if in.Level != nil {
			v.Level = *in.Level
		}
		found = h.Farm.SetActivityView(in.SID, v)
	}
	if !found {
		http.Error(w, "no such stream", http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
