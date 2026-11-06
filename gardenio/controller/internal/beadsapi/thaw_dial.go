package beadsapi

// thaw_dial.go — gVisor freeze→thaw IN-CLUSTER DNS-preresolve for the daemon
// HTTP client (bug ka-0000000696).
//
// THE BUG this closes: on a runsc thaw the gb supervisor process is restored
// (it never restarts), so its in-RAM beadsapi Client carries the frozen,
// BARE in-cluster beads URL (http://gardenio-beads.gardenio.svc.cluster.local:8080).
// The FIRST post-thaw heartbeat POST has to RESOLVE that name on the cold,
// just-restored CNI netns — but in-cluster kube-DNS is not yet routable for the
// first few seconds (kube-proxy hasn't re-programmed the DNAT rules, conntrack/
// ARP are cold). Each getaddrinfo blocks to its full timeout and returns
// EAI_AGAIN, burning the per-attempt connect clamp (GB_THAW_CONNECT_TIMEOUT_MS,
// default 2s from !4991) so the retry loop barely iterates inside the rehydrate
// deadline → "context deadline exceeded" → fail-closed.
//
// CRITICAL CONTRAST that pins the diagnosis: in the SAME thaw, coop's external
// aws_sts probe acks OK — EXTERNAL egress + public DNS survive the restore. It
// is specifically the IN-CLUSTER ClusterIP / kube-DNS path that is not yet up.
// And RANK3 evict_idle_conns acks OK, proving the keep-alive eviction (!4991)
// already works; the residual defect is one layer deeper — the FRESH dial's DNS.
//
// THE FIX (mirrors coop's proven NATS preresolve !3885 + per-attempt self-heal
// !4115, applied to the gb HTTP heartbeat): take DNS off the cold thaw path.
//   - At BOOT (netns healthy, kube-DNS reachable) resolve the in-cluster beads
//     host ONCE and cache the ClusterIP literal in this Client's RAM. The gVisor
//     freeze snapshots that heap value, so it survives the restore. A ClusterIP
//     VIP is stable for the Service lifetime, so the cached IP == what DNS would
//     return post-thaw — and it is NOT a Pod-A address, so it cannot pin a dead
//     4-tuple (distinct from the RANK3 keep-alive concern).
//   - On every dial to that host, rewrite the dial target to the cached IP
//     literal so connect() does ZERO DNS and lands the instant the netns gains a
//     route. The per-attempt 2s clamp + retryUntilDeadline loop then re-probes
//     ~25× (matching coop) instead of ~2×.
//   - Self-heal: with no cache (a degraded/no-creds boot that never prewarmed)
//     or every thawReresolveEvery-th attempt, try a bounded live re-resolve to
//     override a stale/missing cache (the !4115 lesson: a single frozen cache is
//     a single point of failure). A re-resolve miss never fails the dial — it
//     falls back to the cached literal, or to a normal hostname dial when there
//     is no cache at all.
//
// SCOPE GUARD: this rewrite is installed only when EnableThawDNSPrewarm is called
// (the gb thaw watcher does so, gVisor pods only) AND only ever touches the
// configured IN-CLUSTER beads host. Every other dial — external egress, the AVA
// loopback, any non-beads host — passes straight through with normal DNS, so the
// known-good external path (aws_sts:ok) is untouched and no public/CDN IP is ever
// pinned. On non-gVisor clients the dialer is never enabled and is a pure
// passthrough (one atomic load per dial), so boot/normal behavior is unchanged.

import (
	"context"
	"errors"
	"net"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// thawReresolveEvery re-runs a live DNS lookup on every Nth dial attempt to
	// the target host (in addition to the no-cache attempt), so a stale or
	// missing cache self-heals under the retry loop. The cached-IP fast path
	// runs on the other (N-1)/N attempts with zero DNS. Matches coop's !4115
	// "re-resolve every 3rd attempt" cadence.
	thawReresolveEvery = 3

	// thawReresolveTimeout bounds each live re-resolve so a dead resolver on the
	// cold post-restore netns cannot consume the whole per-attempt connect clamp
	// (GB_THAW_CONNECT_TIMEOUT_MS, default 2s) and starve the actual dial. Mirrors
	// coop's bounded 1.5s re-resolve in !4115.
	thawReresolveTimeout = 1500 * time.Millisecond
)

// ipResolver is the slice of net.Resolver the thaw dialer needs. *net.Resolver
// satisfies it; tests inject a fake to simulate a healthy boot resolver vs a
// cold post-restore netns that returns EAI_AGAIN.
type ipResolver interface {
	LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error)
}

// thawDialer wraps a transport's DialContext to rewrite the configured in-cluster
// beads host to a pre-resolved ClusterIP literal on the thaw path. Disabled by
// default (pure passthrough); EnableThawDNSPrewarm flips it on for gVisor pods.
type thawDialer struct {
	// inner is the transport's original DialContext (the cloned DefaultTransport
	// dialer, which already carries the 30s connect / 30s keep-alive defaults).
	// Every passthrough and every literal-IP dial goes through it.
	inner    func(ctx context.Context, network, addr string) (net.Conn, error)
	resolver ipResolver // nil → net.DefaultResolver

	// enabled gates the entire thaw rewrite. Checked first on every dial so a
	// disabled dialer (every non-gVisor client) costs one atomic load and nothing
	// else. Set true only by enable(), after host is populated.
	enabled atomic.Bool

	// attempts counts dials to the target host for the self-heal cadence.
	attempts atomic.Int64

	// mu guards host + cachedIP. host is set once in enable() and never mutated
	// after; cachedIP is refreshed by the boot prewarm and the self-heal path.
	mu       sync.RWMutex
	host     string // target in-cluster host (lower-cased, no port); "" when off
	cachedIP string // pre-resolved ClusterIP literal; "" until resolved
}

// newThawDialer wraps inner, supplying a standard dialer when inner is nil (a
// freshly-constructed *http.Transport whose DialContext was never set).
func newThawDialer(inner func(ctx context.Context, network, addr string) (net.Conn, error)) *thawDialer {
	if inner == nil {
		// Match http.DefaultTransport's dialer defaults so a nil-DialContext base
		// behaves identically to the stdlib default it replaces.
		std := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
		inner = std.DialContext
	}
	return &thawDialer{inner: inner}
}

// DialContext rewrites a dial to the target in-cluster host to the cached
// ClusterIP literal (zero DNS) when the thaw rewrite is enabled; every other
// dial passes straight through.
func (d *thawDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	// A nil receiver has no inner dialer to route through, so it cannot fall back
	// to passthrough — surface a clear error rather than nil-deref panicking on
	// d.inner. Unreachable in practice (New always installs a non-nil dialer), but
	// kept honest so a future regression fails loudly instead of crashing.
	if d == nil {
		return nil, errors.New("beadsapi: nil thaw dialer")
	}
	if !d.enabled.Load() {
		return d.passthrough(ctx, network, addr)
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return d.passthrough(ctx, network, addr)
	}
	// Already a literal IP (or our rewrite from a prior hop) → nothing to resolve.
	if net.ParseIP(host) != nil {
		return d.passthrough(ctx, network, addr)
	}

	d.mu.RLock()
	target := d.host
	cached := d.cachedIP
	d.mu.RUnlock()

	// Only the in-cluster beads host is rewritten. Every other host — external
	// egress, AVA loopback — dials normally so the known-good external path
	// (aws_sts:ok) is never touched and no public IP is pinned.
	if target == "" || !strings.EqualFold(host, target) {
		return d.passthrough(ctx, network, addr)
	}

	n := d.attempts.Add(1)
	// Self-heal: re-resolve when there is no cache yet (failed/degraded boot
	// prewarm) or periodically, to override a stale cache. A re-resolve miss
	// never fails the dial — we fall back to the cached literal below.
	if cached == "" || n%thawReresolveEvery == 0 {
		if ip := d.resolveOnce(ctx); ip != "" {
			d.mu.Lock()
			d.cachedIP = ip
			d.mu.Unlock()
			cached = ip
		}
	}

	if cached == "" {
		// No cache and the live resolve missed (cold netns, kube-DNS unreachable):
		// fall back to a normal hostname dial so the stdlib resolver gets a chance
		// and the bounded retry loop re-probes on the next attempt.
		return d.passthrough(ctx, network, addr)
	}
	// Dial the cached ClusterIP literal: zero DNS, lands the instant the netns
	// gains a route. The request still carries Host: <bare name> (we rewrite only
	// the TCP target, not the URL), so vhost routing is unaffected.
	return d.passthrough(ctx, network, net.JoinHostPort(cached, port))
}

func (d *thawDialer) passthrough(ctx context.Context, network, addr string) (net.Conn, error) {
	return d.inner(ctx, network, addr)
}

// resolveOnce does one bounded DNS lookup of the target host and returns a
// literal IP (IPv4 preferred — ClusterIP VIPs are IPv4 in our clusters), or ""
// on any miss. The lookup is clamped to thawReresolveTimeout (or the remaining
// ctx deadline, whichever is sooner) so it cannot eat the whole connect clamp.
func (d *thawDialer) resolveOnce(ctx context.Context) string {
	d.mu.RLock()
	host := d.host
	d.mu.RUnlock()
	if host == "" {
		return ""
	}
	rctx := ctx
	if dl, ok := ctx.Deadline(); !ok || time.Until(dl) > thawReresolveTimeout {
		var cancel context.CancelFunc
		rctx, cancel = context.WithTimeout(ctx, thawReresolveTimeout)
		defer cancel()
	}
	var res ipResolver = d.resolver
	if res == nil {
		res = net.DefaultResolver
	}
	addrs, err := res.LookupIPAddr(rctx, host)
	if err != nil || len(addrs) == 0 {
		return ""
	}
	for _, a := range addrs {
		if a.IP.To4() != nil {
			return a.IP.String()
		}
	}
	return addrs[0].IP.String()
}

// enable arms the thaw rewrite for host and runs the boot prewarm (one live
// resolve while the boot netns is still healthy) so the ClusterIP is cached in
// RAM before any freeze. Setting host before storing enabled=true means the
// DialContext load-acquire of enabled happens-after the host write.
func (d *thawDialer) enable(ctx context.Context, host string) {
	d.mu.Lock()
	d.host = host
	d.mu.Unlock()
	d.enabled.Store(true)
	if ip := d.resolveOnce(ctx); ip != "" {
		d.mu.Lock()
		d.cachedIP = ip
		d.mu.Unlock()
	}
}

// cachedTarget returns the armed host and its cached IP — for tests and logging.
func (d *thawDialer) cachedTarget() (host, ip string, enabled bool) {
	if d == nil {
		return "", "", false
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.host, d.cachedIP, d.enabled.Load()
}

// probeRoute opens (and immediately closes) one TCP connection to the armed
// in-cluster beads VIP on port, returning nil the instant connect() lands. It is
// the COLD-VIP-ROUTE half of the thaw self-heal that the DNS-preresolve does not
// cover (bug ka-0000002195): even with DNS off the cold path (we dial the cached
// ClusterIP literal here, so this is also zero-DNS and self-heals the DNS half),
// the FIRST connect to that VIP black-holes until kube-proxy reprograms the DNAT/
// conntrack rules for it on the just-restored netns. coop's external aws_sts probe
// acks OK in the same thaw, so general egress is up — it is specifically this
// cross-namespace ClusterIP route that is not yet programmed. A successful probe
// proves the route is live AND seeds a valid conntrack 4-tuple, so the heartbeat
// POST that immediately follows lands on a warm route instead of burning the
// rehydrate deadline on a black-holed SYN.
//
// probeRoute goes through DialContext (the same rewrite the heartbeat uses), so
// it inherits the cached-IP fast path and the periodic live re-resolve self-heal.
// It is a no-op (returns nil) when the dialer is disabled or unarmed — every
// non-gVisor client and any client whose base URL is external — so callers can
// invoke it unconditionally with zero behavior change off the thaw path.
//
// One probe = one attempt; the caller (the gb thaw handler) wraps probeRoute in
// the existing retry-under-deadline loop so a still-cold route is re-probed with
// the same 2s per-attempt clamp and capped backoff as the heartbeat, landing the
// instant the route is programmed.
func (d *thawDialer) probeRoute(ctx context.Context, port string) error {
	if d == nil || !d.enabled.Load() {
		return nil
	}
	d.mu.RLock()
	host := d.host
	d.mu.RUnlock()
	if host == "" {
		return nil
	}
	if port == "" {
		port = "8080"
	}
	// Route the probe through DialContext so it rewrites to the cached ClusterIP
	// literal (zero DNS) exactly like the heartbeat dial, and benefits from the
	// no-cache / periodic live re-resolve self-heal. A bare hostname target is
	// fine: if the cache is empty and DNS is still cold, DialContext falls back to
	// a hostname dial, which fails fast under the per-attempt clamp and is re-probed.
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(host, port))
	if err != nil {
		return err
	}
	return conn.Close()
}

// isInClusterHost reports whether host names an in-cluster Kubernetes Service,
// i.e. a target whose ClusterIP VIP is stable across a freeze and safe to pin.
// External hosts (a public FQDN) are NOT in-cluster: their address may be a
// load-balancer / CDN that rotates, and the external path already survives the
// thaw, so we leave them on normal DNS.
//
//   - bare single-label or dotted in-namespace name with no public TLD
//     (gardenio-beads, gardenio-beads.gardenio) → in-cluster
//   - *.svc, *.svc.cluster.local, *.cluster.local → in-cluster
//   - a literal IP → already resolved, treat as in-cluster (no-op rewrite)
//   - anything ending in a public-looking TLD with a dot → external
func isInClusterHost(host string) bool {
	host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	if host == "" {
		return false
	}
	if net.ParseIP(host) != nil {
		return true
	}
	if strings.HasSuffix(host, ".svc.cluster.local") ||
		strings.HasSuffix(host, ".cluster.local") ||
		strings.HasSuffix(host, ".svc") {
		return true
	}
	// A bare service name (no dot) is in-cluster. A "<name>.<namespace>" form
	// (exactly one dot, no public TLD) is also in-cluster; anything with a
	// public-looking multi-label suffix (e.g. gardenio.e2e.example.com) is
	// treated as external and left on normal DNS.
	switch strings.Count(host, ".") {
	case 0:
		return true
	case 1:
		// "<svc>.<ns>" — in-cluster. Guard against a 2-label public domain
		// (example.com) by requiring the right label not to look like a TLD;
		// in practice the gVisor gb client only ever carries the bare or FQDN
		// in-cluster form, so this branch is conservative.
		return !looksLikePublicTLD(host[strings.LastIndex(host, ".")+1:])
	default:
		return false
	}
}

// looksLikePublicTLD is a tiny heuristic for the one-dot case in isInClusterHost:
// common public TLDs that would mark a 2-label host as external. The gVisor gb
// client never actually carries such a host (it is the bare/FQDN in-cluster
// name), so this only hardens the guard against misconfiguration.
func looksLikePublicTLD(label string) bool {
	switch label {
	case "com", "net", "org", "io", "ai", "dev", "co", "app", "cloud":
		return true
	}
	return false
}

// hostFromURL extracts the bare hostname (no scheme, no port) from a daemon URL.
func hostFromURL(raw string) string {
	if raw == "" {
		return ""
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// portFromURL extracts the port from a daemon URL, defaulting to the scheme's
// well-known port (443 for https, else 8080 — the in-cluster beads daemon port)
// when the URL carries none. Used by WarmThawRouteOnce to target the right TCP
// port when priming the VIP route.
func portFromURL(raw string) string {
	if raw == "" {
		return "8080"
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "8080"
	}
	if p := u.Port(); p != "" {
		return p
	}
	if strings.EqualFold(u.Scheme, "https") {
		return "443"
	}
	return "8080"
}

// WarmThawRouteOnce makes ONE bounded TCP-connect probe to the in-cluster beads
// ClusterIP VIP to confirm/seed the cross-namespace route before the gb thaw
// handler fires its first heartbeat (bug ka-0000002195). It is the cold-VIP-route
// companion to the DNS-preresolve: the cached ClusterIP literal takes DNS off the
// cold path, but the first connect to that VIP still black-holes until kube-proxy
// programs its DNAT/conntrack on the just-restored netns. A successful probe
// proves the route is live and seeds a valid conntrack entry, so the heartbeat
// POST lands warm instead of timing out the rehydrate deadline.
//
// It is a no-op (returns nil) unless the thaw rewrite is armed — i.e. only on
// gVisor pods whose daemon URL is an in-cluster Service host (EnableThawDNSPrewarm
// arms it). On every non-gVisor / external client this returns nil immediately,
// so the gb handler can call it unconditionally with zero behavior change off the
// thaw path. ONE probe = ONE attempt; the gb handler wraps it in the existing
// retry-under-deadline loop so a still-cold route is re-probed under the same
// per-attempt clamp + capped backoff as the heartbeat.
func (c *Client) WarmThawRouteOnce(ctx context.Context) error {
	if c == nil || c.thawDial == nil {
		return nil
	}
	return c.thawDial.probeRoute(ctx, portFromURL(c.baseURL))
}
