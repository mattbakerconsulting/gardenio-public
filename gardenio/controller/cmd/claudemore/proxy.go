package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
)

// maxCaptureBytes caps how many bytes of a request or response body claudemore
// buffers for a cassette. Anthropic Messages requests/responses are well under
// this; the cap exists so a pathological or streaming response can never grow
// the record buffer without bound. A body that exceeds it is captured up to the
// cap and flagged Truncated — the bytes forwarded to the client are NEVER capped
// (capture tees a copy; it does not gate the stream).
const maxCaptureBytes = 8 << 20 // 8 MiB

// proxy is the thin Anthropic Messages-API reverse-proxy that fronts (never
// replaces) litellm. In LIVE pass-through mode it forwards every request to the
// configured upstream litellm URL byte-for-byte and streams the response back
// unchanged, so spend tracking, virtual keys, model-alias routing, and OAuth
// forwarding all stay in litellm exactly as before (R5 synthesis §2.1 prop 3,
// §5.2 cost fence). claudemore meters NO spend itself.
//
// The Recorder seam is where Phase-1 task 1.2 (cassette schema + S3 store) lands
// the record path. It is nil by default, so the MVP is a strict pass-through with
// zero behavior change — the load-bearing transparency property (A1). When a
// recorder IS attached, claudemore tees copies of the request and response
// bodies as they flow (never buffering the stream before the client) and hands
// the completed Exchange to the recorder — augmenting, never altering, the wire.
type proxy struct {
	upstream *url.URL
	rp       *httputil.ReverseProxy
	recorder Recorder
	logger   *slog.Logger
}

// Recorder is the extension point for the LIVE record path (Phase-1 task 1.2,
// ka-0000003571). The MVP ships with no recorder (pass-through only); a recorder
// captures the canonicalized request/response exchange to a cassette store
// WITHOUT altering the bytes forwarded to the client. Implementations MUST be
// non-blocking on the request hot path and MUST never mutate the proxied
// response — claudemore augments, it does not change behavior.
type Recorder interface {
	// RecordExchange is called once per round-trip, after the upstream response
	// body has been fully observed (EOF/close). It receives copies of the
	// request and response metadata and bodies; it must not block the client.
	RecordExchange(ex Exchange)
}

// Exchange is the redaction-UNSAFE raw view of one Messages-API round-trip handed
// to a Recorder — the recorder is responsible for redaction before anything is
// persisted (task 1.2 owns the cassette serialization, request
// canonicalization/hashing, redaction, and S3 persistence). Bodies are populated
// only when a recorder is attached, so the no-recorder MVP buffers nothing.
type Exchange struct {
	Method      string
	Path        string
	RawQuery    string
	ReqHeaders  http.Header
	ReqBody     []byte
	StatusCode  int
	RespHeaders http.Header
	RespBody    []byte
	// Streaming is true when the upstream response was Server-Sent Events
	// (text/event-stream) — REPLAY must reproduce the Content-Type + flush.
	Streaming bool
	// Truncated is true when either body exceeded maxCaptureBytes and the
	// cassette copy is incomplete (the forwarded bytes were never truncated).
	Truncated bool
}

// ctxKey is a private context key type so claudemore's request-capture value
// cannot collide with any other package's context values.
type ctxKey int

const reqCaptureKey ctxKey = iota

// reqCapture holds the inbound request identity plus a buffer the request-body
// tee fills as the body is forwarded upstream. By the time ModifyResponse runs
// (after RoundTrip has sent the whole request) buf holds the full request body.
type reqCapture struct {
	method   string
	path     string
	rawQuery string
	headers  http.Header
	buf      *bytes.Buffer
	trunc    bool
}

// newProxy builds a reverse-proxy to the given upstream litellm URL. The
// upstream is the litellm Service (e.g. http://gardenio-litellm:4000); claudemore
// inserts itself transparently in front of it. Returns an error if the URL is
// malformed so a misconfigured deploy fails loudly rather than silently
// black-holing agent traffic.
func newProxy(upstreamURL string, recorder Recorder, logger *slog.Logger) (*proxy, error) {
	if strings.TrimSpace(upstreamURL) == "" {
		return nil, fmt.Errorf("upstream URL is required")
	}
	u, err := url.Parse(upstreamURL)
	if err != nil {
		return nil, fmt.Errorf("invalid upstream URL %q: %w", upstreamURL, err)
	}
	if u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("upstream URL %q missing scheme or host", upstreamURL)
	}

	p := &proxy{upstream: u, recorder: recorder, logger: logger}

	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			// Preserve the inbound path, query, headers, and body unchanged —
			// claudemore is path-transparent. SetURL points the request at the
			// upstream host while keeping the original RequestURI path, exactly
			// as a Messages-API client expects (POST /v1/messages, etc.).
			pr.SetURL(u)
			pr.Out.Host = u.Host
			// Do NOT call SetXForwarded(): litellm's per-agent spend tagging and
			// OAuth forwarding key off the original Authorization / x-litellm-*
			// headers, which ReverseProxy already copies. Adding X-Forwarded-*
			// would be a behavior change; the cost fence (§5.2) requires none.
		},
		// FlushInterval -1 flushes each write immediately, which is mandatory for
		// Anthropic SSE streaming (stream:true) — without it the proxy would
		// buffer token deltas and break the real `claude` binary's stream parse.
		FlushInterval: -1,
		// ModifyResponse is the record seam. It observes the upstream response
		// WITHOUT mutating the bytes: it wraps resp.Body in a tee that captures a
		// copy as the client reads it, and fires the recorder once the body is
		// fully observed. It returns nil (response untouched), so attaching a
		// recorder cannot change client-visible behavior. In the no-recorder MVP
		// this is a no-op, keeping the path a strict pass-through.
		ModifyResponse: p.modifyResponse,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			logger.Warn("claudemore: upstream proxy error",
				"upstream", u.String(), "method", r.Method, "path", r.URL.Path, "error", err)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"type":"error","error":{"type":"api_error","message":"claudemore: upstream unavailable"}}`))
		},
	}
	p.rp = rp
	return p, nil
}

// modifyResponse is the record seam. With no recorder it is a no-op pass-through.
// With a recorder it wraps the response body so the exchange is recorded once the
// body has been fully streamed to the client.
func (p *proxy) modifyResponse(resp *http.Response) error {
	if p.recorder == nil {
		return nil
	}
	capture, _ := resp.Request.Context().Value(reqCaptureKey).(*reqCapture)

	streaming := strings.HasPrefix(
		strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Type"))),
		"text/event-stream")

	respHeaders := resp.Header.Clone()
	statusCode := resp.StatusCode

	// Wrap the body so bytes are captured as the client reads them — the stream
	// is never buffered ahead of the client. onDone fires exactly once when the
	// body is closed (ReverseProxy always closes it after copying), at which
	// point the request body (capture.buf) is also complete.
	orig := resp.Body
	resp.Body = &bodyCapture{
		rc:    orig,
		limit: maxCaptureBytes,
		onDone: func(respBody []byte, respTrunc, incomplete bool) {
			ex := Exchange{
				StatusCode:  statusCode,
				RespHeaders: respHeaders,
				RespBody:    respBody,
				Streaming:   streaming,
				// An incomplete read (client disconnect / upstream read error
				// before EOF) means the captured body is a prefix of the real
				// response — mark it Truncated so the recorder drops it rather
				// than persisting a partial 2xx as if complete (a poisoned cassette
				// REPLAY would serve byte-for-byte).
				Truncated: respTrunc || incomplete,
			}
			if capture != nil {
				ex.Method = capture.method
				ex.Path = capture.path
				ex.RawQuery = capture.rawQuery
				ex.ReqHeaders = capture.headers
				ex.ReqBody = capture.buf.Bytes()
				ex.Truncated = ex.Truncated || capture.trunc
			} else if resp.Request != nil {
				// Defensive: capture context missing (shouldn't happen when a
				// recorder is attached). Fall back to the outbound request.
				ex.Method = resp.Request.Method
				ex.Path = resp.Request.URL.Path
				ex.RawQuery = resp.Request.URL.RawQuery
			}
			p.recorder.RecordExchange(ex)
		},
	}
	return nil
}

// ServeHTTP forwards the request to the upstream. Health endpoints are handled
// by the mux in main; everything else is proxied. When a recorder is attached
// the request body is teed for the record path (zero-copy tee, no buffering of
// the forwarded stream); in the no-recorder MVP the request streams straight
// through with zero extra work.
func (p *proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if p.recorder == nil {
		p.rp.ServeHTTP(w, r)
		return
	}

	capture := &reqCapture{
		method:   r.Method,
		path:     r.URL.Path,
		rawQuery: r.URL.RawQuery,
		headers:  r.Header.Clone(),
		buf:      &bytes.Buffer{},
	}
	if r.Body != nil {
		lw := &limitedWriter{buf: capture.buf, limit: maxCaptureBytes, trunc: &capture.trunc}
		// TeeReader copies each read into the capture buffer as the upstream
		// Transport reads the body — the exact bytes forwarded are unchanged, so
		// Content-Length and transparency (A1) are preserved.
		r.Body = &teeReadCloser{r: io.TeeReader(r.Body, lw), closer: r.Body}
	}
	r = r.WithContext(context.WithValue(r.Context(), reqCaptureKey, capture))
	p.rp.ServeHTTP(w, r)
}

// teeReadCloser adapts an io.Reader (the tee) back to a ReadCloser by delegating
// Close to the original body — TeeReader itself is not a Closer.
//
// EOF is sticky: once the tee has returned io.EOF, later reads return io.EOF
// without touching the inbound body. The upstream Transport reads the body once
// more after the Content-Length bytes to confirm EOF, and the request bytes are
// already on the wire by then. If the upstream replies in that gap, ReverseProxy
// writes the response header, and the server's post-handler drain closes the
// inbound body. The confirming read would then fail with "invalid Read on closed
// Body", the Transport would close the upstream connection mid-response, and the
// recorder would drop the exchange as incomplete (gd-0A9m1x6uFQ).
type teeReadCloser struct {
	r      io.Reader
	closer io.Closer
	eof    bool
}

func (t *teeReadCloser) Read(p []byte) (int, error) {
	if t.eof {
		return 0, io.EOF
	}
	n, err := t.r.Read(p)
	if err == io.EOF {
		t.eof = true
	}
	return n, err
}

func (t *teeReadCloser) Close() error { return t.closer.Close() }

// limitedWriter buffers written bytes up to a cap, flagging truncation past it.
// It ALWAYS reports a full write so io.TeeReader never sees a short write and
// never errors the underlying read — capture must not perturb forwarding.
type limitedWriter struct {
	buf   *bytes.Buffer
	limit int
	trunc *bool
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	if room := w.limit - w.buf.Len(); room > 0 {
		if len(p) <= room {
			w.buf.Write(p)
		} else {
			w.buf.Write(p[:room])
			*w.trunc = true
		}
	} else if len(p) > 0 {
		*w.trunc = true
	}
	return len(p), nil
}

// bodyCapture wraps the upstream response body, teeing a bounded copy into buf as
// the client reads, and invoking onDone exactly once when the body is closed.
// `sawEOF` records whether the underlying body was read to completion; if Close
// fires without a clean EOF the capture is a prefix of the real response and
// onDone is told the capture is incomplete.
type bodyCapture struct {
	rc     io.ReadCloser
	buf    bytes.Buffer
	limit  int
	trunc  bool
	sawEOF bool
	closed bool
	onDone func(body []byte, truncated, incomplete bool)
}

func (b *bodyCapture) Read(p []byte) (int, error) {
	n, err := b.rc.Read(p)
	if n > 0 {
		if room := b.limit - b.buf.Len(); room > 0 {
			if n <= room {
				b.buf.Write(p[:n])
			} else {
				b.buf.Write(p[:room])
				b.trunc = true
			}
		} else {
			b.trunc = true
		}
	}
	if err == io.EOF {
		b.sawEOF = true
	}
	return n, err
}

func (b *bodyCapture) Close() error {
	err := b.rc.Close()
	if !b.closed {
		b.closed = true
		// Incomplete = closed before the body reached EOF (client canceled, or an
		// upstream read error). A truncated (over-limit) capture is separately
		// flagged; either way the recorder must not persist a partial 2xx.
		b.onDone(b.buf.Bytes(), b.trunc, !b.sawEOF)
	}
	return err
}
