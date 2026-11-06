package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"gardenio/controller/internal/claudemore"
)

// replayServer is the Phase-2 task-2.1 (ka-0000005558) REPLAY handler: it serves
// recorded cassettes deterministically at $0 with litellm bypassed entirely. It
// keys each inbound request through the SAME canonicalization + hashing the LIVE
// recorder used (claudemore.CanonicalizeRequest), so a request that was recorded
// resolves to its cassette byte-for-byte.
//
// fail_on_miss (R2 §5): a cassette miss returns a loud, Anthropic-shaped error —
// NEVER a silent fall-back to LIVE. A REPLAY tier that quietly reached litellm on
// a miss would be neither $0 nor deterministic, so the miss is a hard failure the
// e2e harness (or the real `claude` binary) surfaces immediately. The missing
// (model, hash) is echoed in a diagnostic header + body so CI logs pinpoint which
// cassette to re-record.
type replayServer struct {
	store  claudemore.CassetteStore
	logger *slog.Logger
}

// newReplayServer builds a REPLAY handler over a cassette store. The store must
// be non-nil — buildReplayHandler guarantees this (REPLAY requires a store).
func newReplayServer(store claudemore.CassetteStore, logger *slog.Logger) *replayServer {
	return &replayServer{store: store, logger: logger}
}

// replayReadLimit caps how much of a request body REPLAY buffers to compute the
// lookup hash. It matches the LIVE recorder's maxCaptureBytes: a request larger
// than the recorder could capture was never recorded (the recorder drops
// over-limit exchanges), so it can only ever miss — reading past the cap would
// waste memory to reach the same miss. A body over the cap is treated as an
// unrecordable miss.
const replayReadLimit = maxCaptureBytes

// ServeHTTP looks up and serves the cassette for the inbound request. Any path
// that reaches here is a proxied Messages-API request (health endpoints are
// handled by the mux in run()).
func (s *replayServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// The Anthropic SDK issues a HEAD connectivity preflight to the base URL
	// (`HEAD /`) before its first Messages request. That probe is never a
	// recorded exchange — the LIVE recorder only ever captures POST message
	// round-trips — so routing it to the cassette lookup produces a spurious
	// fail_on_miss that has nothing to do with a missing cassette. Answer the
	// preflight with a 200 and the REPLAY marker so a client sees a reachable
	// endpoint and the miss signal stays reserved for a genuinely-missing POST
	// cassette (which is what a hooks e2e asserts on). A HEAD carries no body, so
	// there is nothing to canonicalize or serve.
	if r.Method == http.MethodHead {
		w.Header().Set("X-Claudemore-Replay", "preflight")
		w.WriteHeader(http.StatusOK)
		return
	}

	body, overLimit, err := readLimited(r.Body, replayReadLimit)
	if err != nil {
		s.logger.Warn("claudemore replay: reading request body", "path", r.URL.Path, "error", err)
		s.writeError(w, http.StatusBadRequest, "claudemore replay: could not read request body")
		return
	}
	if overLimit {
		// Unrecordable by construction — the recorder never persists an
		// over-limit exchange, so no cassette can match. Fail as a miss.
		s.logger.Warn("claudemore replay: request over capture limit, cannot match a cassette",
			"path", r.URL.Path, "limit", replayReadLimit)
		s.writeMiss(w, r, "", "")
		return
	}

	_, model, hash, err := claudemore.CanonicalizeRequest(claudemore.RequestIdentity{
		Method:  r.Method,
		Path:    r.URL.Path,
		Headers: firstHeaderValues(r.Header),
		Body:    body,
	})
	if err != nil {
		s.logger.Warn("claudemore replay: canonicalizing request", "path", r.URL.Path, "error", err)
		s.writeError(w, http.StatusBadRequest, "claudemore replay: could not canonicalize request")
		return
	}

	c, err := s.store.Get(r.Context(), model, hash)
	if err != nil {
		if errors.Is(err, claudemore.ErrCassetteNotFound) {
			s.logger.Warn("claudemore replay: cassette MISS (fail_on_miss)",
				"model", model, "hash", hash, "path", r.URL.Path)
			s.writeMiss(w, r, model, hash)
			return
		}
		// A store error (S3 unreachable, corrupt cassette) is also a hard failure
		// — REPLAY must never fall through to LIVE.
		s.logger.Error("claudemore replay: cassette store error", "model", model, "hash", hash, "error", err)
		s.writeError(w, http.StatusBadGateway,
			fmt.Sprintf("claudemore replay: cassette store error: %v", err))
		return
	}

	s.serveCassette(w, c, model, hash)
}

// serveCassette writes the cassette's recorded response byte-for-byte: the
// allowlisted response headers, the recorded status, then the body. For a
// streaming (SSE) cassette it flushes after the write so a streaming client sees
// the event stream promptly — the recorded body is the full concatenated stream,
// which the real `claude` binary's SSE parser consumes identically regardless of
// chunk boundaries.
func (s *replayServer) serveCassette(w http.ResponseWriter, c claudemore.Cassette, model, hash string) {
	for k, v := range c.Response.Headers {
		w.Header().Set(k, v)
	}
	// A REPLAY marker lets a client / e2e assert the response came from a
	// cassette and not from litellm. It is additive and never present on a LIVE
	// response, so it cannot be confused for real Anthropic output.
	w.Header().Set("X-Claudemore-Replay", "hit")
	status := c.Response.StatusCode
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	if len(c.Response.Body) > 0 {
		if _, err := w.Write(c.Response.Body); err != nil {
			s.logger.Warn("claudemore replay: writing cassette body", "model", model, "hash", hash, "error", err)
			return
		}
	}
	if c.Response.Streaming {
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
	}
}

// writeMiss emits the fail_on_miss response: a distinctive 424 Failed Dependency
// with an Anthropic-shaped error envelope and the missing (model, hash) in both a
// header and the body. 424 (not 404/5xx) reads as "a dependency — the cassette —
// is missing", is never what litellm returns, and is loud enough that a CI e2e or
// the real `claude` binary treats the run as failed rather than retrying forever.
func (s *replayServer) writeMiss(w http.ResponseWriter, r *http.Request, model, hash string) {
	w.Header().Set("X-Claudemore-Replay", "miss")
	if model != "" || hash != "" {
		// model comes straight from the (untrusted) request body JSON — sanitize
		// to a header-safe token so a crafted model tag cannot smuggle control
		// characters into the response header. hash is hex, always safe.
		w.Header().Set("X-Claudemore-Replay-Miss", headerSafe(model)+"/"+hash)
	}
	msg := fmt.Sprintf("claudemore replay: no cassette for request (fail_on_miss) — method=%s path=%s model=%s hash=%s; re-record this request or check the cassette set",
		r.Method, r.URL.Path, model, hash)
	s.writeError(w, http.StatusFailedDependency, msg)
}

// headerSafe collapses a value to printable ASCII header-token characters,
// replacing anything else (control chars, CR/LF, high bytes) with '_'. Cheap
// defense-in-depth for echoing an untrusted model tag into a response header.
func headerSafe(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r >= 0x20 && r < 0x7f && r != ' ' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

// writeError writes an Anthropic Messages-API error envelope with the given
// status. It sets the REPLAY marker header when the caller has not already set a
// more specific one (miss).
func (s *replayServer) writeError(w http.ResponseWriter, status int, message string) {
	if w.Header().Get("X-Claudemore-Replay") == "" {
		w.Header().Set("X-Claudemore-Replay", "error")
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	// json.Marshal the message so any quotes/control chars stay valid JSON.
	msgJSON, err := json.Marshal(message)
	if err != nil {
		msgJSON = []byte(`"claudemore replay error"`)
	}
	_, _ = fmt.Fprintf(w, `{"type":"error","error":{"type":"api_error","message":%s}}`, msgJSON)
}

// readLimited reads up to limit bytes from r, reporting whether the source had
// more than limit bytes (overLimit). It reads limit+1 bytes and checks for the
// extra so an exactly-at-limit body is NOT falsely flagged over-limit.
func readLimited(r io.Reader, limit int) (body []byte, overLimit bool, err error) {
	if r == nil {
		return nil, false, nil
	}
	b, err := io.ReadAll(io.LimitReader(r, int64(limit)+1))
	if err != nil {
		return nil, false, err
	}
	if len(b) > limit {
		return b[:limit], true, nil
	}
	return b, false, nil
}

// firstHeaderValues collapses an http.Header (multi-value) to the single-value
// map CanonicalizeRequest consumes, keeping the first value per key. Only the
// canonicalizer's own allowlist survives into the hash, so passing the full set
// is safe. Mirrors flattenHeaders in recorder.go (kept local so the two paths do
// not couple).
func firstHeaderValues(h http.Header) map[string]string {
	if len(h) == 0 {
		return nil
	}
	out := make(map[string]string, len(h))
	for k, vs := range h {
		if len(vs) > 0 {
			out[k] = vs[0]
		}
	}
	return out
}
