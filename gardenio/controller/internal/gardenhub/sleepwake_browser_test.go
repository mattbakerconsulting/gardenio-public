package gardenhub

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/commons/commonstest"
)

// TestSleepWakeUnderChromium is M3.2 of the Lands mobile pass (gd-Xo3Stftjv6, rubric C10): a connection that drops and
// comes back, and a browser that goes offline and online again. testdata/sleepwake_browser.js walks it on a phone: the page
// says in words that it is offline or reconnecting while it is, and reconnects at once when it is back online.
func TestSleepWakeUnderChromium(t *testing.T) {
	underChromium(t, "sleepwake_browser.js", func() *httptest.Server { return sleepWorld(t) })
}

// sleepWorld is cardWorld with the Commons live, behind the routes the walk drives it with: POST /test/sleep/say?ch=&id=
// says a message in a channel the way the daemon does (stored, logged for replay, and put on the live stream), /down cuts
// every connection and answers every request but /test/ 502 until /up, as the edge does while the viewer rolls.
func sleepWorld(t *testing.T) *httptest.Server {
	t.Helper()
	inner, rig := cardWorldRig(t, time.Now, "", nil, nil, true)
	inner.Close() // the rig's mux is served below, behind the outage switch
	var down atomic.Bool
	var srv *httptest.Server
	rig.mux.HandleFunc("POST /test/sleep/say", func(w http.ResponseWriter, r *http.Request) {
		d, now, id := rig.commons, time.Now(), r.URL.Query().Get("id")
		hello, _ := d.GetBead(beadsapi.WithUserBearer(context.Background(), commonsViewer), "gd-msgHello")
		b := &beadsapi.BeadDetail{ID: id, Type: "message", Status: "closed", Title: "Said while away " + id, CreatedAt: now, UpdatedAt: now,
			Fields: map[string]string{"channel_id": r.URL.Query().Get("ch"), "author": "oidc:flo@example.com", "text": "Said while away " + id + "."}}
		if hello != nil {
			b.Labels = hello.Labels
		}
		d.Put(b)
		d.Log("beads.bead.created", b, now)
		select {
		case rig.commonsEvents <- beadsapi.SSEEvent{Event: "beads.bead.created", Data: commonstest.Payload(b)}:
		default:
		}
		w.WriteHeader(http.StatusNoContent)
	})
	rig.mux.HandleFunc("POST /test/sleep/down", func(w http.ResponseWriter, r *http.Request) {
		down.Store(true)
		w.WriteHeader(http.StatusNoContent)
		go func() { time.Sleep(50 * time.Millisecond); srv.CloseClientConnections() }()
	})
	rig.mux.HandleFunc("POST /test/sleep/up", func(w http.ResponseWriter, r *http.Request) {
		down.Store(false)
		w.WriteHeader(http.StatusNoContent)
	})
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if down.Load() && !strings.HasPrefix(r.URL.Path, "/test/") {
			http.Error(w, "502 Bad Gateway", http.StatusBadGateway)
			return
		}
		rig.mux.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}
