package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"gardenio/controller/internal/beadsapi"
)

// The Window, live (Lands: the Window, plan gd-pz87vf3xsB, PW.7
// gd-nBkInNpy3r; owner decision gd-b3Vx5jVtFh). GET /w/{token}/live is an
// event stream that tells the page when plan.json changed, about a second
// after the plan did, instead of at the next 20 s poll.
//
// The viewer opens kbeads' GET /v1/windows/self/live on the window token
// (beadsapi.Client.WindowLive), the token's only route besides the view.
// Each of its "view" frames is the view route's body: the viewer builds it as
// it builds a read (buildWindowSnapshot, hygiene included), keeps it in the
// window's cache entry (windowCache.put), and sends the page one "changed"
// frame whose data is the new ETag. The page then fetches the fresh render
// (?refresh=1), which the cache serves without a daemon read. No bead data
// goes down this stream: only ETags, which are hashes of plan.json.
//
// Each keepalive kbeads sends is relayed as a "heartbeat" frame, the page's
// proof that the whole chain is flowing: window.js counts the stream live
// only while a frame arrived recently, and otherwise keeps polling, so a
// stream an edge buffers or a daemon that went quiet never strands the page
// (the viewer's own keepalive is a comment, which the page cannot see). When
// kbeads says the window is gone the page gets a "gone" frame and closes the
// stream for good; a reconnect would be the 404.
//
// A plan window streams; a land window's page polls (kbeads answers 404 for
// it). Every refusal is said with a status, so window.js keeps or goes back
// to its 20 s poll: the uniform 404 for a bad token or any daemon refusal
// (PW-D8), 429 past a cap, 503 when the daemon cannot be reached.
//
// Caps: windowLiveMaxPerIP streams per visitor address, which only the viewer
// sees (kbeads sees the viewer); windowLiveMaxTotal across the viewer; and
// kbeads' own per-window cap, which comes back as 429. The edge allows ten
// /w/ connections per address, so a visitor at this cap still has room for
// the page and plan.json. Nothing here logs the path or the token.

const (
	// windowLiveMaxPerIP caps one visitor address's live streams: a few tabs.
	windowLiveMaxPerIP = 3
	// windowLiveMaxTotal caps the viewer's live streams.
	windowLiveMaxTotal = 1024
	// windowLiveKeepalive is the comment line that keeps the stream open
	// through the edge and viewer-authproxy, whose read timeout is 60 s.
	windowLiveKeepalive = 15 * time.Second
	// windowLiveRetry is the EventSource reconnect delay after a stream ends
	// (kbeads ends each after 15 minutes).
	windowLiveRetry = 5 * time.Second
	// windowVisitorHops is the trusted proxies in front of /w/: the public
	// edge's ^~ /w/ location sets X-Forwarded-For to the address it saw,
	// replacing whatever the client sent, and viewer-authproxy's appends its
	// own peer, so the entry two from the right is the visitor's.
	windowVisitorHops = 2
)

var windowLiveStreams = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "gardenio_window_live_streams_total",
	Help: "Window live streams asked for, by outcome: opened, busy (a cap; the page polls), refused (the 404) or error (the daemon could not be reached).",
}, []string{"outcome"})

// windowStreamAPI is the daemon's window stream. *beadsapi.Client implements
// it; a windowAPI that does not has no live stream, and /live is the 404.
type windowStreamAPI interface {
	WindowLive(ctx context.Context, token string) (io.ReadCloser, error)
}

// windowLiveLimits counts open live streams, per visitor address and in all.
// The zero value is ready.
type windowLiveLimits struct {
	mu    sync.Mutex
	perIP map[string]int
	total int
}

// acquire takes a stream slot for ip, false at a cap.
func (l *windowLiveLimits) acquire(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.total >= windowLiveMaxTotal || l.perIP[ip] >= windowLiveMaxPerIP {
		return false
	}
	if l.perIP == nil {
		l.perIP = map[string]int{}
	}
	l.perIP[ip]++
	l.total++
	return true
}

func (l *windowLiveLimits) release(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.total--
	if l.perIP[ip]--; l.perIP[ip] <= 0 {
		delete(l.perIP, ip)
	}
}

// windowVisitorIP is the visitor's address as the public edge saw it (see
// windowVisitorHops), or the TCP peer when the request did not come through
// both proxies.
func windowVisitorIP(r *http.Request) string {
	if ip := clientIP(r, windowVisitorHops); ip != nil {
		return ip.String()
	}
	return r.RemoteAddr
}

func (s *Server) handleWindowLive(w http.ResponseWriter, r *http.Request) {
	setWindowHeaders(w)
	token := r.PathValue("token")
	var stream windowStreamAPI
	if s.window != nil {
		stream, _ = s.window.api.(windowStreamAPI)
	}
	if stream == nil || !wellFormedWindowToken(token) {
		s.renderWindowNotFound(w)
		return
	}
	ip := windowVisitorIP(r)
	if !s.window.live.acquire(ip) {
		windowLiveStreams.WithLabelValues("busy").Inc()
		http.Error(w, "Too many live connections: the page refreshes every 20 s instead.", http.StatusTooManyRequests)
		return
	}
	defer s.window.live.release(ip)

	body, err := stream.WindowLive(r.Context(), token)
	if err != nil {
		var apiErr *beadsapi.APIError
		switch {
		case errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusTooManyRequests:
			windowLiveStreams.WithLabelValues("busy").Inc()
			http.Error(w, "Too many live connections: the page refreshes every 20 s instead.", http.StatusTooManyRequests)
		case errors.As(err, &apiErr) && apiErr.StatusCode < 500:
			windowLiveStreams.WithLabelValues("refused").Inc()
			s.renderWindowNotFound(w)
		default:
			windowLiveStreams.WithLabelValues("error").Inc()
			http.Error(w, "Service Unavailable", http.StatusServiceUnavailable)
		}
		return
	}
	defer body.Close()
	windowLiveStreams.WithLabelValues("opened").Inc()

	ctx := r.Context()
	frames := make(chan windowFrame)
	go readWindowFrames(ctx, body, frames)

	rc := http.NewResponseController(w)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	if _, err := fmt.Fprintf(w, "retry: %d\n\n", windowLiveRetry.Milliseconds()); err != nil || rc.Flush() != nil {
		return
	}
	keepalive := time.NewTicker(windowLiveKeepalive)
	defer keepalive.Stop()
	sent := ""
	for {
		var err error
		select {
		case <-ctx.Done():
			return
		case <-keepalive.C:
			_, err = fmt.Fprint(w, ":\n\n")
		case fr, ok := <-frames:
			if !ok {
				// kbeads ended the stream; the page reconnects.
				return
			}
			switch fr.event {
			case windowFrameHeartbeat:
				_, err = fmt.Fprint(w, "event: heartbeat\ndata: ok\n\n")
			case windowFrameGone:
				_, _ = fmt.Fprint(w, "event: gone\ndata: gone\n\n")
				_ = rc.Flush()
				return
			default:
				snap, berr := buildWindowSnapshot(fr.data, s.window.now())
				if berr != nil || snap.land != nil {
					continue
				}
				s.window.put(token, snap)
				if snap.etag == sent {
					continue
				}
				sent = snap.etag
				_, err = fmt.Fprintf(w, "event: changed\ndata: %s\n\n", snap.etag)
			}
		}
		if err != nil || rc.Flush() != nil {
			return
		}
	}
}

// The kbeads frames readWindowFrames passes on: a "view" with its data, a
// keepalive comment as a heartbeat, and "gone".
const (
	windowFrameView      = "view"
	windowFrameHeartbeat = "heartbeat"
	windowFrameGone      = "gone"
)

type windowFrame struct {
	event string
	data  []byte
}

// readWindowFrames sends each "view" and "gone" frame on body, and each
// keepalive comment as a heartbeat, to frames, and closes frames when body
// ends.
func readWindowFrames(ctx context.Context, body io.Reader, frames chan<- windowFrame) {
	defer close(frames)
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	send := func(fr windowFrame) bool {
		select {
		case frames <- fr:
			return true
		case <-ctx.Done():
			return false
		}
	}
	event := ""
	var data []string
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			ok := true
			switch {
			case event == windowFrameView && len(data) > 0:
				ok = send(windowFrame{event: windowFrameView, data: []byte(strings.Join(data, "\n"))})
			case event == windowFrameGone:
				ok = send(windowFrame{event: windowFrameGone})
			}
			if !ok {
				return
			}
			event, data = "", nil
			continue
		}
		if strings.HasPrefix(line, ":") {
			if !send(windowFrame{event: windowFrameHeartbeat}) {
				return
			}
			continue
		}
		field, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "event":
			event = value
		case "data":
			data = append(data, value)
		}
	}
}
