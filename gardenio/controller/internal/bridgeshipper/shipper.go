// Package bridgeshipper is the non-AVA webhook-audit transport (GAP-B of the
// webhook carve-out, task ka-0000000320, epic ka-0000000914). It streams the
// jira-bridge and gitlab-bridge pods' webhook receive-path slog JSON off the
// Kubernetes pod-log API and POSTs it to the already-live in-chart VictoriaLogs
// store (gardenio-victoria-logs:9428 /insert/jsonline), so the two inbound
// webhook doors that stay NON-AVA by design (design ka-0000005632,
// gardenio/docs/security/non-ava-webhook-carveout.md) have a durable, queryable
// audit record of every delivery and its outcome.
//
// # Why this exists
//
// AWS Verified Access (AVA) is the front door for interactive/browser access,
// but it is browser/cookie-only and cannot authenticate a headless third-party
// POSTer — so the Jira/GitLab inbound webhooks are a permanent, tightly-scoped
// non-AVA exception. The exception is accepted only because it is compensated by
// (1) structured LOGS of all non-AVA bridge traffic and (2) receive-path
// liveness METRICS. Control 2 (metrics) is fully wired; control 1's log stream
// existed only as slog-to-stdout with no in-chart pipeline routing it to a
// durable audit sink (GAP-B). This shipper is that pipeline.
//
// # Why stream stdout (not a sidecar, not a DaemonSet)
//
// This reuses the SANCTIONED stream -> filter -> sink shape of the edge-IDP L0
// transport (internal/traefikshipper) and its exact VictoriaLogs sink
// (internal/vlsink.HTTPSink): both bridges already write structured slog
// JSON to their container stdout, and VictoriaLogs is the in-chart audit store
// (the same sink traefik-log-shipper feeds). The alternatives
// were rejected for the same reasons the edge transport rejected them — a
// per-bridge sidecar is a larger blast radius than a single Deployment in the
// release namespace where regcred and the VictoriaLogs Service already live; no
// node log-collector DaemonSet exists in the cluster. The one structural
// difference from the Traefik transport is that this shipper streams TWO pods
// (the two bridge doors) concurrently and stamps each record with its logical door name so a single
// LogsQL/Grafana query charts both.
//
// The pod-log streamer and the VictoriaLogs sink are both interfaces, so the
// per-source stream loop, the webhook-audit filter, and the reconnect behaviour
// are unit-tested with fakes.
package bridgeshipper

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"gardenio/controller/internal/traefikshipper"
	"gardenio/controller/internal/vlsink"
)

// fieldSource is the field stamped onto every shipped record naming the logical
// non-AVA door it came from (e.g. "jira-bridge" / "gitlab-bridge"). It is the
// low-cardinality VictoriaLogs stream key, so one LogsQL query can chart both
// doors and an auditor can filter to a single door. Kept distinct from the slog
// default keys (time/level/msg) so it never collides with a bridge log field.
const fieldSource = "source"

// defaultMaxLine caps a single scanned stdout line. A structured webhook slog
// record is well under this; the cap is pure headroom that stops one pathological
// line from blowing the scanner buffer.
const defaultMaxLine = 1024 * 1024 // 1 MiB

// Source is one non-AVA bridge door to stream. Name is the logical door label
// stamped onto every shipped record (fieldSource); Stream opens the door pod's
// stdout follow stream; MsgPrefix, when non-empty, keeps ONLY records whose slog
// `msg` field has that prefix (the webhook receive-path records), dropping the
// bridge's other operational chatter (MR polling, nudge delivery, catch-up). An
// EMPTY MsgPrefix ships every well-formed JSON record from the pod — the
// fail-open direction (more audit data, never a silent drop of a door delivery)
// for a source whose prefix an operator has not pinned.
type Source struct {
	Name      string
	Stream    traefikshipper.LogStreamer
	MsgPrefix string
	// Redact, when non-nil, is applied to the final marshaled record bytes just
	// before they are batched, so a secret-shaped substring (sk-…/glpat-…/
	// token=…) is scrubbed before it lands in VictoriaLogs. The webhook-audit
	// doors leave this nil (their receive-path records are already scrubbed and
	// low-volume); the general core-component shipper wires it to
	// claudemore.RedactBytes — the same secret-shape contract coop and the
	// claudemore proxy enforce — because broad debug logs can echo a credential.
	// This is the redaction gate design §5 requires before a broad shipper is
	// enabled (design ka-0000005579, task ka-0000005209).
	Redact func([]byte) []byte
}

// Config is the shipper's runtime configuration (populated from env by the cmd).
// The knobs mirror the Traefik transport so the two stay operationally uniform.
type Config struct {
	// ReconnectBackoff is the wait before re-opening a pod-log stream after it
	// ends (pod restart, rollout, API hiccup). An audit control must not die when
	// a stream drops — it reconnects and keeps shipping.
	ReconnectBackoff time.Duration
	// StreamSinceSeconds bounds the re-read window on each (re)connect. On the
	// FIRST connect it back-fills this many seconds; on every reconnect it
	// re-reads at most this far so the disconnect gap is covered (at-least-once).
	StreamSinceSeconds int64
	// FlushInterval bounds how long filtered records wait before a partial batch
	// is POSTed, so a quiet door still ships promptly. Zero disables time-based
	// flushing (ship per read).
	FlushInterval time.Duration
	// MaxBatchLines flushes the batch once this many records accumulate, bounding
	// the POST body on a burst. Zero means no line cap (flush only on
	// FlushInterval / stream end).
	MaxBatchLines int
	// MaxLineBytes caps a single scanned stdout line; longer lines are skipped
	// rather than blowing the scanner buffer. Zero uses defaultMaxLine.
	MaxLineBytes int
}

// Shipper streams N non-AVA bridge doors and ships their webhook-audit records
// to VictoriaLogs. Each source is streamed independently, so one door's stream
// drop or reconnect never stalls another; all share the one sink.
type Shipper struct {
	cfg     Config
	sources []Source
	sink    vlsink.Sink
	log     *slog.Logger
}

// New builds a Shipper. sink is the VictoriaLogs jsonline sink (reuse
// vlsink.HTTPSink in prod); sources are the bridge doors to stream. A nil
// logger defaults to slog.Default(); a non-positive MaxLineBytes uses
// defaultMaxLine.
func New(cfg Config, sources []Source, sink vlsink.Sink, log *slog.Logger) *Shipper {
	if log == nil {
		log = slog.Default()
	}
	if cfg.MaxLineBytes <= 0 {
		cfg.MaxLineBytes = defaultMaxLine
	}
	return &Shipper{cfg: cfg, sources: sources, sink: sink, log: log}
}

// Run streams every source concurrently until ctx is cancelled, then returns
// ctx.Err(). Each source runs its own reconnect loop, so a transient stream or
// sink error on one door is logged and retried on that door's next connect
// without affecting the others.
func (s *Shipper) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	for _, src := range s.sources {
		wg.Add(1)
		go func(src Source) {
			defer wg.Done()
			s.runSource(ctx, src)
		}(src)
	}
	wg.Wait()
	return ctx.Err()
}

// runSource is one door's reconnect loop: (re)open its pod-log stream, ship its
// filtered records, and — on any non-cancellation end — wait ReconnectBackoff
// and reconnect. The first connect back-fills StreamSinceSeconds; every
// reconnect re-reads the same bounded window so the disconnect gap is covered
// (at-least-once).
func (s *Shipper) runSource(ctx context.Context, src Source) {
	for {
		if ctx.Err() != nil {
			return
		}
		if err := s.streamOnce(ctx, src); err != nil && ctx.Err() == nil {
			s.log.Error("bridge webhook-audit stream ended; will reconnect",
				"source", src.Name, "err", err, "backoff", s.cfg.ReconnectBackoff.String())
		}
		if ctx.Err() != nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(s.cfg.ReconnectBackoff):
		}
	}
}

// streamOnce opens one door's pod-log follow stream and ships its filtered
// webhook-audit records until the stream ends or ctx is cancelled. Returns nil
// on a clean stream end (EOF), or the underlying error otherwise. The scan runs
// in a goroutine (Read blocks) while the main loop selects on ctx + the flush
// ticker + the scanned-line channel — the same proven concurrency shape as the
// Traefik transport's streamOnce.
func (s *Shipper) streamOnce(ctx context.Context, src Source) error {
	rc, err := src.Stream.Stream(ctx, s.cfg.StreamSinceSeconds)
	if err != nil {
		return fmt.Errorf("open pod-log stream for %s: %w", src.Name, err)
	}
	defer rc.Close()

	scanner := bufio.NewScanner(rc)
	scanner.Buffer(make([]byte, 0, 64*1024), s.cfg.MaxLineBytes)

	var (
		batch bytes.Buffer
		lines int
	)
	var tickC <-chan time.Time
	if s.cfg.FlushInterval > 0 {
		t := time.NewTicker(s.cfg.FlushInterval)
		defer t.Stop()
		tickC = t.C
	}

	lineCh := make(chan []byte)
	scanErr := make(chan error, 1)
	go func() {
		defer close(lineCh)
		for scanner.Scan() {
			// Copy: the scanner reuses its buffer across Scan calls.
			b := append([]byte(nil), scanner.Bytes()...)
			select {
			case lineCh <- b:
			case <-ctx.Done():
				return
			}
		}
		scanErr <- scanner.Err()
	}()

	flush := func() error {
		if lines == 0 {
			return nil
		}
		if err := s.sink.Insert(ctx, batch.Bytes()); err != nil {
			// Do NOT drop the batch silently — return the error so streamOnce ends
			// and the source reconnects with a fresh sink attempt (at-least-once).
			return fmt.Errorf("insert to VictoriaLogs: %w", err)
		}
		batch.Reset()
		lines = 0
		return nil
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tickC:
			if err := flush(); err != nil {
				return err
			}
		case line, ok := <-lineCh:
			if !ok {
				// Stream ended: flush what we have, then report the scan result.
				if ferr := flush(); ferr != nil {
					return ferr
				}
				select {
				case err := <-scanErr:
					return err
				default:
					return nil
				}
			}
			out, keep := processLine(line, src)
			if !keep {
				continue
			}
			batch.Write(out)
			batch.WriteByte('\n')
			lines++
			if s.cfg.MaxBatchLines > 0 && lines >= s.cfg.MaxBatchLines {
				if err := flush(); err != nil {
					return err
				}
			}
		}
	}
}

// processLine turns one raw bridge stdout line into the record to ship, or drops
// it. It keeps a well-formed JSON object IFF (a) it is JSON — the bridges log
// structured slog JSON; any non-JSON runtime/panic output is not an audit
// record and is dropped so the /insert batch stays valid jsonline — AND (b) when
// the source pins a MsgPrefix, its slog `msg` field has that prefix (the webhook
// receive-path records), so the door's operational chatter (MR polling, nudge
// delivery, description catch-up) does not dilute the audit stream. A kept
// record is stamped with the door's logical name (fieldSource) so a single query
// charts both doors and an auditor can filter to one.
//
// An empty MsgPrefix keeps every JSON record from the pod — the fail-open
// direction: for an audit control, shipping extra operational records is
// tolerable where silently dropping a door delivery is not.
func processLine(line []byte, src Source) (out []byte, keep bool) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 || line[0] != '{' {
		return nil, false // not a JSON object — runtime/panic output, not an audit record.
	}
	var rec map[string]any
	if err := json.Unmarshal(line, &rec); err != nil {
		return nil, false // not well-formed JSON.
	}
	if src.MsgPrefix != "" {
		msg, _ := rec["msg"].(string)
		if !strings.HasPrefix(msg, src.MsgPrefix) {
			return nil, false // not a webhook receive-path record for this door.
		}
	}
	// Stamp the door name so streams partition and consumers can filter. Overwrite
	// any pre-existing "source" so the field is authoritative.
	rec[fieldSource] = src.Name
	b, err := json.Marshal(rec)
	if err != nil {
		// Re-marshal of an already-parsed record is vanishingly unlikely to fail;
		// fall back to the original bytes so a shippable audit record is never
		// dropped by the enrichment step (it just lacks the source stamp). Redact
		// the fallback too, so an enrichment miss never bypasses the scrub gate.
		if src.Redact != nil {
			return src.Redact(line), true
		}
		return line, true
	}
	if src.Redact != nil {
		b = src.Redact(b)
	}
	return b, true
}
