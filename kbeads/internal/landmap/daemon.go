package landmap

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// daemon calls the daemon's own routes in-process. h is the daemon's route
// mux, below the auth chain: the map's request already went through it, so
// the inner request carries the caller's verified principal in its context
// and each route's role and scope gates judge it exactly as they would a kd
// call. Nothing from the outer request but its context, host and remote
// address goes in; the inner request carries Content-Type only.
type daemon struct {
	h http.Handler
}

// upstreamError is a daemon route answering with an error status. body is
// kept for the log line under the correlation id and never written out.
type upstreamError struct {
	route  Route
	status int
	body   []byte
}

func (e *upstreamError) Error() string {
	return fmt.Sprintf("%s: status %d", e.route, e.status)
}

// call runs route rt with args filling its {wildcards} in order, decoding a
// success body into out (when out is non-nil).
func (d daemon) call(outer *http.Request, rt routeID, args []string, query url.Values, body, out any) error {
	route := routeTable[rt]
	path, err := fillPattern(route.Pattern, args)
	if err != nil {
		return err
	}
	var rd io.Reader = http.NoBody
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(raw)
	}
	target := path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(outer.Context(), route.Method, target, rd)
	if err != nil {
		return err
	}
	req.Host = outer.Host
	req.RemoteAddr = outer.RemoteAddr
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	rec := &recorder{header: http.Header{}, status: http.StatusOK}
	d.h.ServeHTTP(rec, req)
	if rec.status >= 400 {
		raw := rec.body.Bytes()
		if len(raw) > 2048 {
			raw = raw[:2048]
		}
		return &upstreamError{route: route, status: rec.status, body: raw}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(rec.body.Bytes(), out); err != nil {
		return fmt.Errorf("%s: decode: %w", route, err)
	}
	return nil
}

// fillPattern substitutes args for the pattern's {wildcards}, in order.
func fillPattern(pattern string, args []string) (string, error) {
	segs := strings.Split(pattern, "/")
	n := 0
	for i, s := range segs {
		if strings.HasPrefix(s, "{") && strings.HasSuffix(s, "}") {
			if n >= len(args) {
				return "", fmt.Errorf("%s: missing argument %d", pattern, n)
			}
			segs[i] = url.PathEscape(args[n])
			n++
		}
	}
	if n != len(args) {
		return "", fmt.Errorf("%s: %d arguments for %d wildcards", pattern, len(args), n)
	}
	return strings.Join(segs, "/"), nil
}

// recorder is the inner request's ResponseWriter.
type recorder struct {
	header      http.Header
	status      int
	wroteHeader bool
	body        bytes.Buffer
}

func (r *recorder) Header() http.Header { return r.header }

func (r *recorder) WriteHeader(code int) {
	if !r.wroteHeader {
		r.status, r.wroteHeader = code, true
	}
}

func (r *recorder) Write(p []byte) (int, error) {
	r.WriteHeader(http.StatusOK)
	return r.body.Write(p)
}
