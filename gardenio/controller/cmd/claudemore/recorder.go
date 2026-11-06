package main

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"gardenio/controller/internal/claudemore"
)

// nowUnixMs is a package var so tests can pin the timestamp; production uses the
// wall clock. Kept out of the cassette hash by design (see cassette.go).
var nowUnixMs = func() int64 { return time.Now().UnixMilli() }

// cassetteRecorder is the LIVE record path: it turns each proxied Exchange into a
// redacted, canonicalized Cassette and persists it to the CassetteStore. It
// implements the proxy's Recorder interface.
//
// Recording is strictly best-effort and MUST NOT affect the client: RecordExchange
// is invoked from the response-body Close (after the client has the bytes), does
// the redaction/canonicalization synchronously (cheap, in-memory), and dispatches
// the S3 Put on a bounded worker so a slow/broken store can never block or fail an
// agent's request (the cost-fence / A1 transparency invariant). A full work queue
// drops the record with a warn rather than applying backpressure.
type cassetteRecorder struct {
	store  claudemore.CassetteStore
	logger *slog.Logger
	// putCtx is the parent for each async S3 Put. It is DECOUPLED from the
	// signal/process context's cancellation (context.WithoutCancel) on purpose:
	// at SIGTERM the process ctx is cancelled BEFORE the deferred Close() runs
	// (main.go shutdown ordering), so if the Put inherited that cancellation
	// every queued Put would return context.Canceled instantly and the Close()
	// drain would store nothing — defeating its own guarantee (ka-0000000092).
	// A wedged store is still bounded per-Put by `timeout` below, so decoupling
	// from cancellation does not risk an unbounded drain.
	putCtx  context.Context
	timeout time.Duration
	work    chan claudemore.Cassette
	// done is closed by writeLoop when it returns (after the work channel is
	// drained). Close() blocks on it so a caller that Close()s at process
	// shutdown is guaranteed every queued cassette's Put has completed — the
	// writeLoop goroutine cannot be killed mid-Put by the process exiting.
	done chan struct{}
	// mu guards `closed`, which gates enqueue so a RecordExchange racing with
	// Close drops the cassette instead of panicking on a send to a closed
	// channel. The Put itself still happens on the worker, off the hot path.
	mu     sync.Mutex
	closed bool
	// onStored, when non-nil, is signaled after each Put attempt completes
	// (success or failure). Test-only hook so a test can wait for the async
	// write without sleeping.
	onStored func(err error)
}

const (
	// recorderQueueCapacity bounds in-flight cassette writes. Small: the store is
	// fast (single S3 Put) and the queue is a burst absorber, not a backlog.
	recorderQueueCapacity = 64
	// recorderPutTimeout caps a single S3 Put so a wedged store frees the worker.
	recorderPutTimeout = 15 * time.Second
)

// newCassetteRecorder builds a recorder and starts its background writer. Call
// Close to drain and stop the writer. ctx supplies request-scoped values but its
// CANCELLATION is intentionally dropped (context.WithoutCancel) for the Put
// parent: the caller threads in the process/signal context, and Close() runs at
// shutdown AFTER that context is already cancelled, so a Put that honored the
// cancellation (the S3 store does, via the AWS SDK) would abandon every queued
// cassette instead of draining it (ka-0000000092). Each Put stays bounded by
// recorderPutTimeout, so a wedged store still frees the writer promptly.
func newCassetteRecorder(ctx context.Context, store claudemore.CassetteStore, logger *slog.Logger) *cassetteRecorder {
	r := &cassetteRecorder{
		store:   store,
		logger:  logger,
		putCtx:  context.WithoutCancel(ctx),
		timeout: recorderPutTimeout,
		work:    make(chan claudemore.Cassette, recorderQueueCapacity),
		done:    make(chan struct{}),
	}
	go r.writeLoop()
	return r
}

// RecordExchange (Recorder interface) redacts + canonicalizes the exchange into a
// cassette and enqueues it for storage. It never blocks the caller beyond the
// in-memory transform; if the queue is full it drops the record with a warn.
func (r *cassetteRecorder) RecordExchange(ex Exchange) {
	// Only record successful Messages-API responses. A non-2xx (litellm budget
	// 429, upstream 5xx) is not a reusable cassette and would poison REPLAY; a
	// truncated capture is incomplete and unsafe to serve byte-for-byte.
	if ex.StatusCode < 200 || ex.StatusCode >= 300 {
		return
	}
	if ex.Truncated {
		r.logger.Warn("claudemore: dropping over-limit exchange from record",
			"path", ex.Path, "status", ex.StatusCode)
		return
	}

	c, err := r.buildCassette(ex)
	if err != nil {
		r.logger.Warn("claudemore: could not build cassette", "path", ex.Path, "error", err)
		return
	}

	r.enqueue(c)
}

// enqueue offers the cassette to the writer without blocking. Guarded by mu so a
// send can never race with Close closing the channel (send on a closed channel
// panics). A full queue or an already-closed recorder drops with a warn.
func (r *cassetteRecorder) enqueue(c claudemore.Cassette) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		r.logger.Warn("claudemore: recorder closed, dropping cassette",
			"hash", c.RequestHash, "model", c.Model)
		return
	}
	select {
	case r.work <- c:
	default:
		// Queue full: drop rather than block the (already-returned) client path.
		r.logger.Warn("claudemore: record queue full, dropping cassette",
			"hash", c.RequestHash, "model", c.Model)
	}
}

// buildCassette performs the pure transform: redact + canonicalize the request,
// redact the response, and assemble the redaction-safe Cassette.
func (r *cassetteRecorder) buildCassette(ex Exchange) (claudemore.Cassette, error) {
	req, model, hash, err := claudemore.CanonicalizeRequest(claudemore.RequestIdentity{
		Method:  ex.Method,
		Path:    ex.Path,
		Headers: flattenHeaders(ex.ReqHeaders),
		Body:    ex.ReqBody,
	})
	if err != nil {
		return claudemore.Cassette{}, err
	}

	resp := claudemore.CassetteResponse{
		StatusCode: ex.StatusCode,
		Headers:    redactHeaders(ex.RespHeaders),
		Streaming:  ex.Streaming,
		// Response body is redacted whole (F16 / R4-SEC-3): a reasoning model can
		// echo a key it was shown straight back into its completion.
		Body: claudemore.RedactBytes(ex.RespBody),
	}

	return claudemore.NewCassette(req, model, hash, resp, nowUnixMs()), nil
}

// writeLoop drains the work queue, storing each cassette with a bounded timeout.
// It closes r.done on return so Close() can block until every queued Put has
// completed (the writer is never killed mid-Put by process shutdown).
func (r *cassetteRecorder) writeLoop() {
	defer close(r.done)
	for c := range r.work {
		ctx, cancel := context.WithTimeout(r.putCtx, r.timeout)
		err := r.store.Put(ctx, c)
		cancel()
		if err != nil {
			r.logger.Warn("claudemore: cassette store failed",
				"hash", c.RequestHash, "model", c.Model, "error", err)
		} else {
			r.logger.Debug("claudemore: cassette stored", "hash", c.RequestHash, "model", c.Model)
		}
		if r.onStored != nil {
			r.onStored(err)
		}
	}
}

// Close stops the writer after draining queued cassettes, and BLOCKS until the
// writeLoop has processed every queued cassette and returned. Idempotent and
// safe to call concurrently with RecordExchange: it flips `closed` under mu (so
// a racing enqueue drops instead of sending on a closed channel) before closing
// the work channel, then waits on r.done so a caller closing the recorder at
// process shutdown is guaranteed no cassette is lost to a Put killed mid-flight.
// A second concurrent Close still blocks on the same done channel, so every
// caller observes a fully-drained writer before returning.
func (r *cassetteRecorder) Close() {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		<-r.done
		return
	}
	r.closed = true
	r.mu.Unlock()
	close(r.work)
	<-r.done
}

// responseHeaderAllowlist is the set of upstream response headers worth storing
// in a cassette so REPLAY can reproduce a faithful response. Volatile/hop-by-hop
// headers (Date, x-request-id, Set-Cookie, ratelimit counters) are dropped —
// they change every run and a byte-for-byte REPLAY of them would be misleading.
var responseHeaderAllowlist = map[string]bool{
	"content-type": true,
}

// redactHeaders lower-cases + allowlists response headers and redacts their
// values defensively. Returns nil when nothing survives so the cassette omits an
// empty map.
func redactHeaders(h http.Header) map[string]string {
	if len(h) == 0 {
		return nil
	}
	var out map[string]string
	for k, vs := range h {
		lk := strings.ToLower(k)
		if !responseHeaderAllowlist[lk] || len(vs) == 0 {
			continue
		}
		if out == nil {
			out = make(map[string]string)
		}
		out[lk] = claudemore.RedactString(vs[0])
	}
	return out
}

// flattenHeaders collapses an http.Header (multi-value) into the single-value
// map the canonicalizer consumes, keeping the first value per key. Only the
// canonicalizer's own allowlist survives into the cassette, so passing the full
// set here is safe.
func flattenHeaders(h http.Header) map[string]string {
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
