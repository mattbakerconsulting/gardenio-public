// Package traefikshipper is the edge-IDP L0 transport: it streams the Traefik
// edge pod's JSON access-log lines off the Kubernetes pod-log API and POSTs them
// to the already-live VictoriaLogs store (gardenio-victoria-logs:9428
// /insert/jsonline) so the L1-L3 edge intrusion-detection layers
// (design ka-0000007287) have a queryable per-request record of who touches the
// public edge. Task ka-0000001563, epic ka-0000008216.
//
// # Why stream stdout (not a sidecar, not a DaemonSet)
//
// Traefik's access log (`logs.access`, format json, enabled by MR !7042 in
// the EKS-era platform terraform, since deleted) is written to the
// container's stdout. Getting it into VictoriaLogs needs a
// transport, and the design of record (ka-0000007287#c3) left the sink/transport
// shape open pending a real volume number. The measured edge volume is tiny
// (~0.13 req/s avg, ~11k req/day, 30d raw ≈ 231 MB ≈ 2% of the existing 10Gi
// vlsingle PVC — see the MR PROOF block), which rules OUT the heavier options and
// makes a single small streamer correct:
//
//   - A Traefik-pod SIDECAR was rejected: Traefik is a cluster addon installed
//     outside this chart (a terraform helm_release on EKS, the k3s bundled
//     addon today), NOT part of the gardenio app chart, and the `traefik` namespace has no regcred to pull a private
//     gardenio image. Adding a sidecar there means editing the addon's helm
//     values AND provisioning image-pull creds in a second namespace — a much
//     larger blast radius than a Deployment in gardenio-system, where regcred and
//     the VictoriaLogs Service already live.
//   - A node LOG-COLLECTOR DaemonSet (fluent-bit / vector / promtail) was
//     rejected: none exists in the cluster today (verified live), so "stdout
//     captured by pod-log collection" is not actually an available sink — it
//     would mean standing up a whole new cluster-wide agent for one low-volume
//     stream.
//
// So this shipper follows the single Traefik pod's pod-log stream, transforms
// each line and POSTs batches through vlsink.HTTPSink. The k8s client and the
// VictoriaLogs sink are both interfaces, so the stream loop, line filtering, and
// reconnect behaviour are unit-tested with fakes.
package traefikshipper

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"strings"
	"time"

	"gardenio/controller/internal/auditxform"
	"gardenio/controller/internal/vlsink"
)

// fieldXForwardedFor is the Traefik access-log field carrying the client's
// X-Forwarded-For header. On an edge fronted by a proxy that INJECTS XFF this
// holds the true public client IP (design ka-0000007287#c1-3). The gardenio prod
// edge does NOT inject it, though: the internet-facing webhook NLB is
// target-type=ip + externalTrafficPolicy=Cluster (SNATs the L4 peer) and neither
// it nor AWS Verified Access adds X-Forwarded-For (verified live, ka-0000000358 —
// real edge rows carry no XFF). There, the true client IP arrives via PROXY
// protocol and Traefik lands it in ClientHost instead (see fieldClientHost).
// Traefik prefixes request headers with `request_`. This field name lives HERE,
// in the L0 surface adapter, so internal/auditxform stays surface-agnostic and
// monorepo can reuse the resolver/field-contract without importing Traefik shapes
// (design ka-0000007287#c5).
const fieldXForwardedFor = "request_X-Forwarded-For"

// fieldClientHost is the Traefik access-log field holding the L4 peer address as
// Traefik resolved it. WITHOUT PROXY protocol this is the in-cluster hop the NLB
// SNATs to (10.x) or the AVA ENI — useless for geo. WITH PROXY protocol trusted
// on the entrypoint (traefik-webhooks NLB, L1c fix ka-0000000358), Traefik
// reconstructs the real client L4 address here. The enricher therefore uses
// ClientHost as the client IP ONLY when it is a globally-routable (public)
// address — a private/CGNAT/loopback value is the SNAT hop and is ignored, so
// internal and not-yet-PROXY-fronted traffic keeps emitting an empty client_ip
// exactly as before. XFF, when present, still takes precedence.
const fieldClientHost = "ClientHost"

// fieldClientUsername is the Traefik access-log field holding the authenticated
// user of the request ("-" when unauthenticated — the Traefik convention). It is
// lifted into the surface-agnostic auditxform.FieldPrincipal so the geomap's
// post-compromise panels and the L2 novel-geo-for-known-principal detection query
// ONE principal field across every edge surface (the AVA OCSF surface lifts its
// user identity into the same field). Keeping the Traefik field name HERE, in the
// L0 adapter, is what lets auditxform own only the normalized-event contract and
// never learn a Traefik shape (design ka-0000007287#c5).
const fieldClientUsername = "ClientUsername"

// defaultXFFDepth selects the RIGHTMOST X-Forwarded-For entry as the client IP —
// position 1 from the right, mirroring the `ipStrategy.depth: 1` that the live
// prod beads-ci allowlist middleware uses and that design ka-0000007287#c1-3 cites
// as load-bearing proof true client IPs arrive at Traefik. The rightmost entry is
// the address the trusted hop in front of Traefik observed, so a client-prepended
// spoof to the LEFT of the chain cannot move its apparent geo.
const defaultXFFDepth = 1

// LogStreamer opens a follow stream of the Traefik pod's stdout. It is the slice
// of the Kubernetes API the shipper needs, narrowed to one method so the stream
// loop is testable with a fake. The returned ReadCloser yields the raw container
// stdout as newline-delimited bytes; the caller closes it.
//
// sinceSeconds bounds how far back the stream re-reads on (re)connect: a small
// positive value back-fills recent lines a fresh pod would otherwise miss, and
// crucially bounds the RE-READ on reconnect so a transport blip does not silently
// drop the window between disconnect and reconnect. A duplicate line at the
// boundary is acceptable (VictoriaLogs de-dups nothing, but the L2 detections
// tolerate a bounded dup far better than a silent gap — the same at-least-once
// stance every vlsink shipper takes).
type LogStreamer interface {
	Stream(ctx context.Context, sinceSeconds int64) (io.ReadCloser, error)
}

// Config is the shipper's runtime configuration (populated from env by the cmd).
type Config struct {
	// ReconnectBackoff is the wait before re-opening the pod-log stream after it
	// ends (pod restart, rollout, API hiccup). A detective control must not die
	// when the stream drops — it reconnects and keeps shipping.
	ReconnectBackoff time.Duration
	// StreamSinceSeconds bounds the re-read window on each (re)connect. On the
	// FIRST connect it back-fills this many seconds; on every reconnect it
	// re-reads at most this far so the disconnect gap is covered (at-least-once).
	StreamSinceSeconds int64
	// FlushInterval bounds how long enriched lines wait before a partial batch is
	// POSTed, so a quiet edge still ships promptly rather than holding lines until
	// the batch fills. Zero disables time-based flushing (ship per read).
	FlushInterval time.Duration
	// MaxBatchLines flushes the batch once this many access lines accumulate,
	// bounding the POST body on a burst. Zero means no line cap (flush only on
	// FlushInterval / stream end).
	MaxBatchLines int
	// MaxLineBytes caps a single scanned log line; longer lines are skipped with a
	// warning rather than blowing the scanner buffer. Zero uses defaultMaxLine.
	MaxLineBytes int
}

const defaultMaxLine = 1024 * 1024 // 1 MiB — a single access-log record is < 2 KiB; this is pure headroom.

// Shipper streams Traefik access-log JSON lines and ships them to VictoriaLogs.
type Shipper struct {
	cfg    Config
	stream LogStreamer
	sink   vlsink.Sink
	log    *slog.Logger
	// asn is the OPTIONAL L1 geo/ASN resolver (edge-IDP L1, task ka-0000001136).
	// When non-nil, each shipped access row whose X-Forwarded-For client IP
	// resolves gets a scalar client_ip field AND — per the resolver — sourceASN
	// (any ASNResolver) and sourceCountry (a GeoResolver, non-empty country), the
	// group keys the geomap console (ka-0000003019) and the L2 novel-geo/ASN
	// detections query. A NIL resolver reproduces the exact byte-identical raw
	// shipping (no client_ip/sourceASN/sourceCountry stamped), so the transport is
	// unchanged until an operator wires a table — matching the deploy-raw-first,
	// enrich-second sequencing the plan-gardener resolved on ka-0000001136.
	asn auditxform.ASNResolver
}

// New builds a Shipper. sink is the VictoriaLogs jsonline sink (reuse
// vlsink.HTTPSink in prod); stream opens the Traefik pod-log follow stream.
// asn is the OPTIONAL L1 resolver (nil = byte-identical raw shipping, no
// client_ip/sourceASN/sourceCountry stamped); a non-nil resolver stamps those
// enriched fields from the row's X-Forwarded-For client IP.
func New(cfg Config, stream LogStreamer, sink vlsink.Sink, log *slog.Logger, asn auditxform.ASNResolver) *Shipper {
	if log == nil {
		log = slog.Default()
	}
	if cfg.MaxLineBytes <= 0 {
		cfg.MaxLineBytes = defaultMaxLine
	}
	return &Shipper{cfg: cfg, stream: stream, sink: sink, log: log, asn: asn}
}

// Run streams until ctx is cancelled, reconnecting after ReconnectBackoff when
// the pod-log stream ends. It returns ctx.Err() on cancellation; a transient
// stream/sink error is logged and retried on the next connect (a detective
// control must not exit on one bad stream).
func (s *Shipper) Run(ctx context.Context) error {
	// First connect back-fills StreamSinceSeconds; every reconnect re-reads the
	// same bounded window so the disconnect gap is covered (at-least-once).
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := s.streamOnce(ctx); err != nil && ctx.Err() == nil {
			s.log.Error("traefik access-log stream ended; will reconnect", "err", err, "backoff", s.cfg.ReconnectBackoff.String())
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(s.cfg.ReconnectBackoff):
		}
	}
}

// streamOnce opens one pod-log follow stream and ships its access lines until
// the stream ends or ctx is cancelled. Returns nil on a clean stream end (EOF),
// or the underlying error otherwise.
func (s *Shipper) streamOnce(ctx context.Context) error {
	rc, err := s.stream.Stream(ctx, s.cfg.StreamSinceSeconds)
	if err != nil {
		return fmt.Errorf("open pod-log stream: %w", err)
	}
	defer rc.Close()

	scanner := bufio.NewScanner(rc)
	scanner.Buffer(make([]byte, 0, 64*1024), s.cfg.MaxLineBytes)

	var (
		batch bytes.Buffer
		lines int
	)
	// A ticker drives time-based flushing so a quiet edge still ships promptly.
	// Guard nil channel when FlushInterval is 0 (ship per read, no timer).
	var tickC <-chan time.Time
	if s.cfg.FlushInterval > 0 {
		t := time.NewTicker(s.cfg.FlushInterval)
		defer t.Stop()
		tickC = t.C
	}

	// The scanner blocks on Read, so run it in a goroutine and select on ctx +
	// the flush ticker in the main loop. lineCh carries each accepted access line;
	// scanErr carries the terminal scan result.
	lineCh := make(chan []byte)
	scanErr := make(chan error, 1)
	go func() {
		defer close(lineCh)
		for scanner.Scan() {
			// Copy: scanner reuses its buffer across Scan calls.
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
			// Do NOT drop the batch silently — log and keep the lines so the next
			// flush retries them (at-least-once). Return the error so streamOnce
			// ends and Run reconnects with a fresh sink attempt.
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
			out, keep := s.processLine(line)
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

// acceptLine decides whether a raw Traefik stdout line is a JSON access record
// to ship, and returns the (compacted) bytes to ship. Traefik writes BOTH its
// own runtime logs and the access log to the same stdout, and the ONLY reliable
// discriminator is the POSITIVE presence of an access-record field:
//
//   - The ACCESS log is JSON (--accesslog.format=json). A real Traefik v3 access
//     record carries DownstreamStatus + RequestMethod (+ RequestPath, StartUTC,
//     entryPointName, ...) AND — verified against live prod — a top-level
//     "level":"info","msg":"","time":... too. So keying on the ABSENCE of "level"
//     is WRONG: it would drop every real access record. We must NOT do that.
//   - The RUNTIME log is zerolog CONSOLE format by default (--log.level with no
//     --log.format=json), e.g. `2026-… ERR Error configuring TLS error=…` — not
//     JSON, so it fails the JSON parse below and is dropped.
//   - Even if runtime logs were ever switched to JSON (--log.format=json), those
//     records carry level/msg/time but NOT DownstreamStatus/RequestMethod, so the
//     positive access-field requirement below still excludes them.
//
// Hence: keep a well-formed JSON object IFF it carries an access-defining field
// (DownstreamStatus or RequestMethod); drop everything else. This keeps runtime
// chatter out of the access stream the detections query.
//
// IMPORTANT (sampling policy, non-negotiable per ka-0000007287#c3): this filter
// keys ONLY on "is this an access record?", never on status code. Non-2xx
// records (probes, scans, auth failures) are the SIGNAL and are shipped in full.
func acceptLine(line []byte) (out []byte, keep bool) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 || line[0] != '{' {
		return nil, false // not a JSON object — a zerolog console runtime line.
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(line, &probe); err != nil {
		return nil, false // not JSON.
	}
	// Require at least one access-record-defining field so a stray non-access
	// JSON object (incl. a runtime log if --log.format=json were ever set) can
	// never sneak in. DownstreamStatus / RequestMethod are always present in a
	// Traefik access record and never in a runtime log.
	_, hasStatus := probe["DownstreamStatus"]
	_, hasMethod := probe["RequestMethod"]
	if !hasStatus && !hasMethod {
		return nil, false
	}
	// Compact to a single line (defensive — pod-log lines are already single
	// lines, but a re-marshal guarantees no embedded newline reaches the batch).
	var buf bytes.Buffer
	if err := json.Compact(&buf, line); err != nil {
		return nil, false
	}
	return buf.Bytes(), true
}

// processLine turns one raw Traefik stdout line into the (compacted, optionally
// L1-enriched) bytes to ship, or drops it. It first applies the access-record
// filter (acceptLine), then — when an L1 resolver is wired — stamps the geo/ASN
// enrichment fields (enrichLine). With a nil resolver it is byte-identical to
// shipping acceptLine's output directly, so raw shipping is unchanged until an
// operator wires a table.
func (s *Shipper) processLine(line []byte) (out []byte, keep bool) {
	out, keep = acceptLine(line)
	if !keep {
		return nil, false
	}
	if s.asn == nil {
		return out, true
	}
	return s.enrichLine(out), true
}

// enrichLine stamps the L1 edge log-field contract onto one accepted access
// record: the scalar client_ip (the true public client IP pulled from
// X-Forwarded-For) and, when the resolver covers it, sourceASN (any ASNResolver)
// and sourceCountry (a GeoResolver with a non-empty country). It NEVER drops or
// re-shapes the row: a record with no usable client IP, or an IP the resolver
// does not cover, ships unchanged (fail-open — the raw forensic record must reach
// VictoriaLogs regardless, and a Traefik access row has exactly one client per
// request, so it is never expanded per IP).
//
// The field NAMES reuse auditxform's constants (FieldClientIP / FieldSourceASN /
// FieldSourceCountry), so the edge rows carry the SAME field contract the geomap
// console and the shared detection library already query — the only
// Traefik-specific part is which raw field the client IP is read FROM, which is
// why this lives in the L0 adapter and not in auditxform (design
// ka-0000007287#c5).
//
// On any re-marshal failure it returns the input unchanged: enrichment must never
// turn a shippable raw row into a dropped one.
func (s *Shipper) enrichLine(line []byte) []byte {
	var rec map[string]any
	if err := json.Unmarshal(line, &rec); err != nil {
		return line // already validated as JSON by acceptLine; belt-and-braces.
	}

	// Prefer X-Forwarded-For (a proxy that injects it puts the trusted-hop-
	// observed client at depth 1). The gardenio edge doesn't inject XFF, so this
	// is normally empty and we fall back to a PROXY-protocol-reconstructed
	// ClientHost — but only when that peer is a public IP, so SNAT'd in-cluster
	// hops and AVA ENIs (private) still yield no client_ip (ka-0000000358).
	// Lift the authenticated user into the surface-agnostic principal contract so
	// the geomap's post-compromise panels and the L2 novel-geo detection attribute
	// a geo to a principal across every edge surface (ka-0000005007). A Traefik row
	// carries ClientUsername ("-" when unauthenticated); normalise a missing/empty
	// value to the "-" sentinel so the geomap's `${f_principal}:*` scope keeps
	// matching the row (design ka-0000007287#c5).
	rec[auditxform.FieldPrincipal] = principalFromClientUsername(rec[fieldClientUsername])

	ip := clientIPFromXFF(rec[fieldXForwardedFor])
	if ip == "" {
		ip = publicClientHost(rec[fieldClientHost])
	}
	if ip == "" {
		// No public client IP on this row (internal traffic has only the SNAT'd
		// ClientHost). Emit an empty client_ip so a consumer can tell an enriched
		// row from a pre-L1 one, but stamp no ASN/country. Ship unchanged shape.
		rec[auditxform.FieldClientIP] = ""
		return remarshalOr(rec, line)
	}
	rec[auditxform.FieldClientIP] = ip

	// A GeoResolver resolves {ASN, country} in one lookup; a plain ASNResolver
	// resolves only the ASN. Prefer the geo path so sourceCountry is emitted when
	// the wired table carries it.
	if geo, ok := s.asn.(auditxform.GeoResolver); ok {
		if g, found := geo.LookupGeo(ip); found {
			rec[auditxform.FieldSourceASN] = g.ASN
			if g.Country != "" {
				rec[auditxform.FieldSourceCountry] = g.Country
			}
		}
	} else if a, found := s.asn.LookupASN(ip); found {
		rec[auditxform.FieldSourceASN] = a
	}
	return remarshalOr(rec, line)
}

// principalFromClientUsername returns the authenticated principal from a Traefik
// row's ClientUsername field, normalising a missing, non-string, or empty value
// to the auditxform.PrincipalUnauthenticated sentinel ("-"). Traefik already
// writes "-" for an unauthenticated request, so an authenticated row passes its
// username through verbatim and an unauthenticated one lands on the same sentinel
// the AVA surface uses — keeping every row inside the geomap's `${f_principal}:*`
// scope (design ka-0000007287#c5).
func principalFromClientUsername(v any) string {
	s, ok := v.(string)
	if !ok || s == "" {
		return auditxform.PrincipalUnauthenticated
	}
	return s
}

// clientIPFromXFF extracts the true public client IP from a Traefik row's
// X-Forwarded-For field value, honouring the same rightmost-entry
// (ipStrategy.depth: 1) semantics the live prod allowlist uses (design
// ka-0000007287#c1-3): the rightmost address is the one the trusted hop in front
// of Traefik observed, so a client-prepended spoof to the LEFT cannot move the
// apparent geo. Returns the canonicalised IP string, or "" when the field is
// absent/empty or holds no parseable IP at the selected depth.
//
// Traefik serialises request_X-Forwarded-For as a scalar STRING (a comma-joined
// chain), which is the shape observed live; a plain string or a []any of strings
// is also handled defensively.
func clientIPFromXFF(v any) string {
	raw := xffToString(v)
	if raw == "" {
		return ""
	}
	parts := strings.Split(raw, ",")
	// depth 1 = rightmost entry, depth 2 = second-from-right, etc.
	idx := len(parts) - defaultXFFDepth
	if idx < 0 || idx >= len(parts) {
		return ""
	}
	addr, err := netip.ParseAddr(strings.TrimSpace(parts[idx]))
	if err != nil {
		// The entry at the trusted depth does not parse as an IP. Return "" rather
		// than falling back to a different (client-controlled, spoofable) position:
		// a detective control must not geolocate off an untrusted value. The row
		// still ships — it just carries no geo (fail-open on enrichment).
		return ""
	}
	return addr.Unmap().String()
}

// xffToString normalises the decoded X-Forwarded-For field into the comma-joined
// chain string. Traefik emits it as a scalar string; a []any of strings (a
// possible re-decode shape) is joined so the same depth logic applies.
func xffToString(v any) string {
	switch t := v.(type) {
	case string:
		return strings.TrimSpace(t)
	case []any:
		parts := make([]string, 0, len(t))
		for _, e := range t {
			if s, ok := e.(string); ok {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, ",")
	default:
		return ""
	}
}

// publicClientHost returns the Traefik ClientHost value ONLY when it parses as a
// globally-routable (public) IP — otherwise "". With PROXY protocol trusted on
// the entrypoint, Traefik reconstructs the real client L4 address into
// ClientHost, so a public value there IS the client IP even though no
// X-Forwarded-For header was injected (ka-0000000358). A private, loopback,
// link-local, CGNAT, or unspecified value is the SNAT'd in-cluster hop (10.x) or
// an AVA ENI, never an internet client, so it is rejected — internal traffic and
// any edge not yet fronted by PROXY protocol keep emitting an empty client_ip,
// exactly the pre-fix behaviour. Traefik serialises ClientHost as a bare host
// (no port); a scalar string is the only shape observed live, handled defensively.
func publicClientHost(v any) string {
	s, ok := v.(string)
	if !ok {
		return ""
	}
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	addr, err := netip.ParseAddr(s)
	if err != nil {
		// Defensive: ClientHost is normally a bare host, but tolerate a host:port
		// shape rather than dropping enrichment on it.
		if host, _, splitErr := net.SplitHostPort(s); splitErr == nil {
			if addr, err = netip.ParseAddr(strings.TrimSpace(host)); err != nil {
				return ""
			}
		} else {
			return ""
		}
	}
	if !isGloballyRoutable(addr) {
		return ""
	}
	return addr.Unmap().String()
}

// isGloballyRoutable reports whether addr is a public, internet-routable address —
// i.e. NOT private (RFC1918 / ULA), loopback, link-local, multicast,
// unspecified, or IPv4 shared/CGNAT (100.64.0.0/10). This is the gate that keeps
// a SNAT'd in-cluster hop or an AVA ENI out of the geo enricher.
func isGloballyRoutable(addr netip.Addr) bool {
	if !addr.IsValid() {
		return false
	}
	if addr.IsPrivate() || addr.IsLoopback() || addr.IsLinkLocalUnicast() ||
		addr.IsLinkLocalMulticast() || addr.IsMulticast() || addr.IsUnspecified() {
		return false
	}
	// IPv4 shared address space (100.64.0.0/10, RFC 6598 CGNAT) is not covered by
	// netip's IsPrivate; exclude it explicitly so a carrier-NAT hop never geolocates.
	if addr.Is4() && cgnat.Contains(addr) {
		return false
	}
	return true
}

// cgnat is RFC 6598 shared address space (carrier-grade NAT), which netip does
// not classify as private but which is never an internet-routable client.
var cgnat = netip.PrefixFrom(netip.AddrFrom4([4]byte{100, 64, 0, 0}), 10)

// remarshalOr re-encodes the enriched record to a single JSON line, falling back
// to the original bytes on the (vanishingly rare) marshal failure so enrichment
// never turns a shippable row into a dropped one.
func remarshalOr(rec map[string]any, orig []byte) []byte {
	b, err := json.Marshal(rec)
	if err != nil {
		return orig
	}
	return b
}
