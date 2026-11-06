package landmap

import (
	"context"
	"log/slog"
	"net/http"
	"slices"
	"strings"

	"gardenio/lands/contract/landsv1"
)

// apiPrefix is where the contract's paths begin on the daemon.
const apiPrefix = BaseURL + "/lands/v1/"

// Guard holds every response on a Lands API path to the contract (D.8
// gd-enMSApFne1), whoever writes it. The map's handlers write problem+json
// and only ResponseHeaders themselves; a request can also be answered before
// it reaches them, by the daemon's middleware (a 401 from auth, a 429 and its
// Retry-After from the rate limiter, the land door's 403, the request
// timeout) or by the daemon's mux (404 and 405 for a path the contract does
// not have, a redirect to a cleaned path). Each of those writes the daemon's
// own text and headers. Guard answers them instead with the problem for their
// status and drops their body; the daemon's version headers, stamped on every
// response, never leave either.
//
// Guard also counts every such response in Requests: it puts a tally on the
// request's context for the map to fill in, and counts it with the answer's
// code once the response is done.
//
// The server wraps its whole HTTP chain in Guard. Other paths pass through
// untouched: the land door also carries an agent's kd calls, which speak the
// daemon's own API.
func Guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, apiPrefix) {
			next.ServeHTTP(w, r)
			return
		}
		t := &tally{}
		g := &guardWriter{w: w, staged: http.Header{}, path: r.URL.Path}
		next.ServeHTTP(g, r.WithContext(context.WithValue(r.Context(), tallyKey{}, t)))
		if !g.wrote {
			// Nothing was written: net/http would send an empty 200.
			g.WriteHeader(http.StatusOK)
		}
		Requests.WithLabelValues(t.land, t.op, g.code, string(t.caller)).Inc()
	})
}

// guardWriter stages headers until the status is written, then lets through
// only a response the map wrote (writeBody stamps the contract version on it)
// and replaces any other with a problem.
type guardWriter struct {
	w       http.ResponseWriter
	staged  http.Header
	path    string
	wrote   bool
	foreign bool
	// code is the answer's problem code, or codeOK.
	code string
}

func (g *guardWriter) Header() http.Header { return g.staged }

func (g *guardWriter) WriteHeader(status int) {
	if g.wrote {
		return
	}
	g.wrote = true
	if g.staged.Get(landsv1.HeaderContract) != landsv1.ContractVersion {
		g.foreign = true
		code := codeForStatus(status)
		if status < http.StatusBadRequest || status == http.StatusMethodNotAllowed {
			// A redirect, an empty answer or the mux's 405: the contract has
			// no such response, or no such route.
			code = landsv1.NotFound
		}
		g.code = string(code)
		id := newCorrelationID()
		slog.Info("landmap: answered outside the map", "path", g.path, "status", status,
			"code", string(code), "correlation_id", id)
		clear(g.w.Header())
		writeProblem(g.w, code, id, "")
		return
	}
	g.code = codeOK
	if status >= http.StatusBadRequest {
		g.code = string(problemCodeOf(status))
	}
	h := g.w.Header()
	clear(h)
	for k, v := range g.staged {
		if slices.Contains(ResponseHeaders, k) {
			h[k] = v
		}
	}
	g.w.WriteHeader(status)
}

func (g *guardWriter) Write(p []byte) (int, error) {
	g.WriteHeader(http.StatusOK)
	if g.foreign {
		// The problem is already written; the original body is dropped.
		return len(p), nil
	}
	return g.w.Write(p)
}

// Flush lets a streamed map response (the land feed, D.9) through.
func (g *guardWriter) Flush() {
	if g.wrote && !g.foreign {
		if f, ok := g.w.(http.Flusher); ok {
			f.Flush()
		}
	}
}

// Unwrap lets http.ResponseController reach the connection's deadlines.
func (g *guardWriter) Unwrap() http.ResponseWriter { return g.w }
