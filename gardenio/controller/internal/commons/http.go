package commons

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"gardenio/controller/internal/servertiming"
)

// readTimeout bounds the daemon reads behind one fragment.
const readTimeout = 8 * time.Second

// channels serves GET /garden/c, the band's channel list (retired, ADR
// gardenio/docs/adr-commons-band-replaced.md): the viewer's steward, which
// holds the land's #general.
func (h *Handler) channels(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, h.BasePath+"/garden/c/s", http.StatusFound)
}

// timeline serves GET /garden/c/{land}/{channel}, the band's channel address
// (retired, gd-1keb7z0nBn): a redirect to the land's steward, which holds its
// #general. It answers no fragment; the workbench reads a channel's threads.
func (h *Handler) timeline(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, h.BasePath+"/garden/c/s/"+url.PathEscape(r.PathValue("land")), http.StatusFound)
}

// page hands a browser's navigation to a workbench address (a node, a
// steward) to Page, which serves the workbench's page there. The page itself
// fetches the same URL as a fragment, with X-Commons: fragment; anything that
// does not ask for HTML (curl, the tests) gets the fragment too.
func (h *Handler) page(w http.ResponseWriter, r *http.Request) bool {
	if h.Page == nil {
		return false
	}
	w.Header().Add("Vary", "Accept, X-Commons")
	if !navigation(r) {
		return false
	}
	return h.Page(w, r)
}

// moved answers a browser's navigation to a thread, one of the band's
// addresses, with a redirect to the workbench address that replaced it
// (gd-ZeiSZV7j2Y), so the links in Slack and in beads keep working. A fragment
// request is not a navigation and gets the fragment.
func (h *Handler) moved(w http.ResponseWriter, r *http.Request, to string) bool {
	w.Header().Add("Vary", "Accept, X-Commons")
	if !navigation(r) {
		return false
	}
	http.Redirect(w, r, to, http.StatusFound)
	return true
}

// navigation reports whether r is a browser navigating, not the workbench
// asking for a fragment.
func navigation(r *http.Request) bool {
	return r.Header.Get("X-Commons") == "" && r.Header.Get("HX-Request") == "" &&
		strings.Contains(r.Header.Get("Accept"), "text/html")
}

// thread serves GET /garden/c/{land}/{channel}/t/{thread}: the root and its
// replies, oldest first, the workbench's transcript (web/commons2.js). A
// navigation goes to the thread's node.
func (h *Handler) thread(w http.ResponseWriter, r *http.Request) {
	if h.moved(w, r, h.nodeHref(strings.TrimSpace(r.PathValue("thread")))) || !h.ready(w) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), readTimeout)
	defer cancel()
	ch, err := readChannel(ctx, h.Beads, h.scope(r), r.PathValue("land"), r.PathValue("channel"))
	if err != nil {
		h.fail(w, err)
		return
	}
	root, replies, err := readThread(ctx, h.Beads, ch, strings.TrimSpace(r.PathValue("thread")))
	if err != nil {
		h.fail(w, err)
		return
	}
	rows := make([]row, len(replies)+1)
	rows[0] = row{M: root, Base: h.BasePath}
	for i, m := range replies {
		rows[i+1] = row{M: m, Base: h.BasePath}
	}
	h.withCards(ctx, h.self(r), rows)
	h.withNames(ctx, r, rows)
	h.withDone(r, rows)
	h.writeTagged(w, r, "thread", struct {
		Base    string
		Channel Channel
		Root    row
		Rows    []row
		Compose compose
	}{h.BasePath, ch, rows[0], rows[1:], h.composeFor(r, ch, root.ID)})
}

// message serves GET /garden/c/m/{id}, a link to one message: a redirect to
// the node of its thread, once the viewer may read it. It answers no fragment:
// the event stream carries a message (stream.go).
func (h *Handler) message(w http.ResponseWriter, r *http.Request) {
	if !h.ready(w) {
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" || len(id) > 200 {
		http.Error(w, "bad message id", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), readTimeout)
	defer cancel()
	m, err := readMessage(ctx, h.Beads, h.scope(r), id)
	if err != nil {
		h.fail(w, err)
		return
	}
	http.Redirect(w, r, h.nodeHref(firstNonEmpty(m.Thread, m.ID)), http.StatusFound)
}

// messageRow reads one message, card or thread head on the viewer's bearer and
// dresses it as the thread renders it: what the event stream carries.
func (h *Handler) messageRow(ctx context.Context, r *http.Request, sc Scope, id string) (row, error) {
	m, err := readMessage(ctx, h.Beads, sc, id)
	if err == nil && m.Thread == "" {
		err = countReplies(ctx, h.Beads, &m)
	}
	if err != nil {
		return row{}, err
	}
	// The channel is addressed by id here: resolving its name would cost a
	// read per message, and the channel route takes either.
	rows := []row{newRow(m, h.BasePath, m.Channel)}
	h.withCards(ctx, h.self(r), rows)
	h.withNames(ctx, r, rows)
	h.withDone(r, rows)
	return rows[0], nil
}

func (h *Handler) ready(w http.ResponseWriter) bool {
	if h.Beads == nil {
		http.Error(w, "commons not enabled", http.StatusServiceUnavailable)
		return false
	}
	return true
}

func (h *Handler) fail(w http.ResponseWriter, err error) {
	if errors.Is(err, errNotFound) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	http.Error(w, "the beads daemon did not answer", http.StatusBadGateway)
}

func (h *Handler) write(w http.ResponseWriter, name string, data any) {
	h.writeTagged(w, nil, name, data)
}

// writeTagged is write with an ETag (a hash of the body) when r is a GET: the
// workbench asks again with If-None-Match on every open, and an unchanged node
// or thread is a 304 with no body (step 5, gd-Z05Hye7yP4). no-cache, private:
// the browser keeps it for this viewer and always asks first.
func (h *Handler) writeTagged(w http.ResponseWriter, r *http.Request, name string, data any) {
	servertiming.Lap(w, "kbeads") // everything the route did before is its daemon reads
	body, err := render(name, data)
	if err != nil {
		http.Error(w, "render failed", http.StatusInternalServerError)
		return
	}
	servertiming.Lap(w, "template")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if r == nil || r.Method != http.MethodGet {
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(body)
		return
	}
	sum := sha256.Sum256(body)
	tag := `"` + hex.EncodeToString(sum[:12]) + `"`
	w.Header().Set("Cache-Control", "private, no-cache")
	w.Header().Set("ETag", tag)
	w.Header().Add("Vary", "Cookie, Authorization")
	if r.Header.Get("If-None-Match") == tag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	_, _ = w.Write(body)
}

// writeJSON writes doc as a route's JSON answer, after its headers: the route's reads so far are its "kbeads" phase on
// the Server-Timing header, the encode its "encode". The bytes are json.Encoder's.
func writeJSON(w http.ResponseWriter, doc any) {
	servertiming.Lap(w, "kbeads")
	var b bytes.Buffer
	_ = json.NewEncoder(&b).Encode(doc)
	servertiming.Lap(w, "encode")
	_, _ = w.Write(b.Bytes())
}
