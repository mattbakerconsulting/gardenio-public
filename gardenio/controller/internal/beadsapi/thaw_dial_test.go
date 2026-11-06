package beadsapi

// thaw_dial_test.go — RED→GREEN proofs for the gVisor freeze→thaw in-cluster
// DNS-preresolve on the daemon HTTP heartbeat client (bug ka-0000000696).
//
// THE DEFECT these gate: on a runsc thaw, coop's EXTERNAL aws_sts probe acks OK
// (external egress + public DNS survive the restore) but gb's heartbeat POST to
// the IN-CLUSTER beads ClusterIP times out — kube-DNS is not yet routable on the
// cold post-restore netns, so resolving the bare service name returns EAI_AGAIN
// and burns the per-attempt connect clamp. The fix pre-resolves the stable
// ClusterIP at boot, snapshots it across the freeze, and dials the cached literal
// on thaw so connect() does ZERO in-cluster DNS.
//
// The whole suite is offline: a fake resolver models "healthy boot" vs "cold
// post-restore netns" (EAI_AGAIN), and a loopback listener stands in for the
// beads ClusterIP. No node/cluster access.

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// fakeResolver records lookups and returns either a fixed IP (healthy) or an
// EAI_AGAIN-class error (cold netns), flippable mid-test to model the netns
// warming up post-thaw.
type fakeResolver struct {
	ip      string
	failing atomic.Bool
	lookups atomic.Int64
}

func (r *fakeResolver) LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error) {
	r.lookups.Add(1)
	if r.failing.Load() {
		// Mirror getaddrinfo EAI_AGAIN on a cold netns: a transient, retryable
		// "Temporary failure in name resolution".
		return nil, &net.DNSError{Err: "Temporary failure in name resolution", Name: host, IsTemporary: true}
	}
	return []net.IPAddr{{IP: net.ParseIP(r.ip)}}, nil
}

// newThawTestDialer builds a thawDialer whose inner dial records the host:port it
// was asked to connect to (so we can assert the rewrite happened) and then dials
// the real loopback target. dialTo is the literal address every accepted dial is
// redirected to (the test listener), so the connection actually completes.
func newThawTestDialer(t *testing.T, res ipResolver, dialTo string) (*thawDialer, *atomic.Pointer[string]) {
	t.Helper()
	var lastDial atomic.Pointer[string]
	std := &net.Dialer{Timeout: 2 * time.Second}
	inner := func(ctx context.Context, network, addr string) (net.Conn, error) {
		a := addr
		lastDial.Store(&a)
		// Redirect every dial to the real loopback listener so the conn completes
		// regardless of the (fake) host/IP in addr — we only care WHAT was dialed.
		return std.DialContext(ctx, network, dialTo)
	}
	d := newThawDialer(inner)
	d.resolver = res
	return d, &lastDial
}

// TestThawDialer_DialsCachedIP_WhenDNSColdOnThaw is the keystone RED→GREEN proof.
// Boot prewarm caches the ClusterIP while DNS is healthy; then the resolver goes
// cold (EAI_AGAIN), modeling the post-restore netns. A dial to the bare beads
// host MUST still target the cached IP literal and complete — proving DNS is off
// the cold thaw path.
//
// RED on unmodified source: there is no thaw dialer at all, so the bare name is
// handed straight to the stdlib resolver, which (cold netns) returns EAI_AGAIN
// and the dial fails.
func TestThawDialer_DialsCachedIP_WhenDNSColdOnThaw(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go acceptAndClose(ln)

	res := &fakeResolver{ip: "172.20.0.42"} // a stand-in stable ClusterIP VIP
	d, lastDial := newThawTestDialer(t, res, ln.Addr().String())

	// Boot prewarm: netns healthy, resolve + cache the ClusterIP.
	d.enable(context.Background(), "gardenio-beads.gardenio.svc.cluster.local")
	if _, ip, on := d.cachedTarget(); !on || ip != "172.20.0.42" {
		t.Fatalf("boot prewarm should cache ClusterIP; got enabled=%v ip=%q", on, ip)
	}

	// Freeze→thaw: netns is cold, kube-DNS unreachable.
	res.failing.Store(true)
	lookupsAfterBoot := res.lookups.Load()

	conn, err := d.DialContext(context.Background(), "tcp",
		net.JoinHostPort("gardenio-beads.gardenio.svc.cluster.local", "8080"))
	if err != nil {
		t.Fatalf("thaw dial MUST succeed via cached IP despite cold DNS, got: %v", err)
	}
	conn.Close()

	got := ""
	if p := lastDial.Load(); p != nil {
		got = *p
	}
	if want := "172.20.0.42:8080"; got != want {
		t.Fatalf("thaw dial must target cached ClusterIP literal %q, got %q", want, got)
	}
	// The cached-IP fast path runs every attempt except the periodic self-heal;
	// attempt #1 must NOT have issued a cold-netns lookup (n%3 != 0, cache present).
	if res.lookups.Load() != lookupsAfterBoot {
		t.Fatalf("first thaw dial issued a DNS lookup (%d>%d) — DNS not off the cold path",
			res.lookups.Load(), lookupsAfterBoot)
	}
}

// TestThawDialer_SelfHeals_WhenBootPrewarmMissed models a degraded/no-creds boot
// that never cached an IP (EnableThawDNSPrewarm ran while DNS was already down).
// With no cache, every dial attempt MUST try a live re-resolve; once the netns
// warms up (DNS recovers), a subsequent attempt resolves + caches + dials the
// literal. This is the !4115 self-heal lesson: a single frozen cache is a SPOF.
func TestThawDialer_SelfHeals_WhenBootPrewarmMissed(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go acceptAndClose(ln)

	res := &fakeResolver{ip: "172.20.0.99"}
	res.failing.Store(true) // boot prewarm runs while DNS is already down
	d, lastDial := newThawTestDialer(t, res, ln.Addr().String())

	d.enable(context.Background(), "gardenio-beads") // bare in-cluster name
	if _, ip, _ := d.cachedTarget(); ip != "" {
		t.Fatalf("degraded boot must NOT cache an IP, got %q", ip)
	}

	// First thaw dial: cache empty → live re-resolve attempted, still cold → it
	// must fall back to a hostname passthrough (not error out), so the bounded
	// retry loop gets another go.
	conn, err := d.DialContext(context.Background(), "tcp", net.JoinHostPort("gardenio-beads", "8080"))
	if err != nil {
		t.Fatalf("no-cache cold dial should fall back to hostname dial, got: %v", err)
	}
	conn.Close()
	if got := derefDial(lastDial); got != "gardenio-beads:8080" {
		t.Fatalf("no-cache cold dial should pass the hostname through, got %q", got)
	}

	// Netns warms up: DNS recovers. Next dial self-heals — resolves, caches, and
	// targets the literal.
	res.failing.Store(false)
	conn2, err := d.DialContext(context.Background(), "tcp", net.JoinHostPort("gardenio-beads", "8080"))
	if err != nil {
		t.Fatalf("self-heal dial after DNS recovery failed: %v", err)
	}
	conn2.Close()
	if got := derefDial(lastDial); got != "172.20.0.99:8080" {
		t.Fatalf("self-heal must dial the freshly-resolved ClusterIP literal, got %q", got)
	}
	if _, ip, _ := d.cachedTarget(); ip != "172.20.0.99" {
		t.Fatalf("self-heal must refresh the cache, got %q", ip)
	}
}

// TestThawDialer_PeriodicReresolve_OverridesStaleCache is the !4115 self-heal
// proof: a cache populated at boot with IP_A must be REFRESHED to IP_B on the
// periodic (every thawReresolveEvery-th) re-resolve once DNS reports the new
// value — a single frozen cache is a single point of failure. It also pins the
// CADENCE: dials that are not the Nth do NOT re-resolve (they ride the fast-path
// cached literal), so a regression that dropped the `n%thawReresolveEvery==0`
// term (re-resolving only on an empty cache) would turn this test RED.
func TestThawDialer_PeriodicReresolve_OverridesStaleCache(t *testing.T) {
	if thawReresolveEvery != 3 {
		t.Fatalf("test assumes thawReresolveEvery==3, got %d", thawReresolveEvery)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go acceptAndClose(ln)

	res := &fakeResolver{ip: "172.20.0.10"} // boot value (IP_A)
	d, lastDial := newThawTestDialer(t, res, ln.Addr().String())
	d.enable(context.Background(), "gardenio-beads.gardenio.svc.cluster.local")
	if _, ip, _ := d.cachedTarget(); ip != "172.20.0.10" {
		t.Fatalf("boot prewarm should cache IP_A, got %q", ip)
	}
	lookupsAfterBoot := res.lookups.Load()

	// The ClusterIP "changes" to IP_B (or: the boot cache was stale all along).
	res.ip = "172.20.0.20"

	host := net.JoinHostPort("gardenio-beads.gardenio.svc.cluster.local", "8080")
	// Dials 1 and 2 (n=1,2): fast path, cached IP_A, NO re-resolve.
	for i := 1; i <= 2; i++ {
		conn, err := d.DialContext(context.Background(), "tcp", host)
		if err != nil {
			t.Fatalf("dial %d: %v", i, err)
		}
		conn.Close()
		if got := derefDial(lastDial); got != "172.20.0.10:8080" {
			t.Fatalf("dial %d must ride cached IP_A (no re-resolve off-cadence), got %q", i, got)
		}
	}
	if res.lookups.Load() != lookupsAfterBoot {
		t.Fatalf("off-cadence dials must NOT re-resolve: lookups %d > %d", res.lookups.Load(), lookupsAfterBoot)
	}

	// Dial 3 (n=3, n%3==0): periodic self-heal → re-resolve, override to IP_B.
	conn, err := d.DialContext(context.Background(), "tcp", host)
	if err != nil {
		t.Fatalf("dial 3: %v", err)
	}
	conn.Close()
	if got := derefDial(lastDial); got != "172.20.0.20:8080" {
		t.Fatalf("3rd dial must re-resolve and target refreshed IP_B, got %q", got)
	}
	if _, ip, _ := d.cachedTarget(); ip != "172.20.0.20" {
		t.Fatalf("periodic re-resolve must refresh the cache to IP_B, got %q", ip)
	}
	if res.lookups.Load() <= lookupsAfterBoot {
		t.Fatalf("3rd dial must have issued a live lookup, lookups still %d", res.lookups.Load())
	}
}

// TestThawDialer_ExternalHostUntouched proves the scope guard: a dial to ANY
// host other than the armed in-cluster beads host passes straight through with
// normal DNS — the known-good external egress path (aws_sts:ok) is never
// rewritten and no public/CDN IP is ever pinned.
func TestThawDialer_ExternalHostUntouched(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go acceptAndClose(ln)

	res := &fakeResolver{ip: "172.20.0.7"}
	d, lastDial := newThawTestDialer(t, res, ln.Addr().String())
	d.enable(context.Background(), "gardenio-beads.gardenio.svc.cluster.local")

	// A different host (e.g. external AWS STS) must NOT be rewritten.
	conn, err := d.DialContext(context.Background(), "tcp", net.JoinHostPort("sts.amazonaws.com", "443"))
	if err != nil {
		t.Fatalf("external dial failed: %v", err)
	}
	conn.Close()
	if got := derefDial(lastDial); got != "sts.amazonaws.com:443" {
		t.Fatalf("external host must pass through untouched, got %q", got)
	}
}

// TestThawDialer_DisabledIsPurePassthrough proves that a dialer that was never
// enabled (every non-gVisor client) does not resolve or rewrite anything — boot/
// normal behavior is unchanged.
func TestThawDialer_DisabledIsPurePassthrough(t *testing.T) {
	res := &fakeResolver{ip: "172.20.0.1"}
	res.failing.Store(true) // would error if the disabled dialer ever resolved
	var dialed atomic.Pointer[string]
	inner := func(ctx context.Context, network, addr string) (net.Conn, error) {
		a := addr
		dialed.Store(&a)
		return nil, errors.New("inner-reached") // we only assert WHAT was dialed
	}
	d := newThawDialer(inner)
	d.resolver = res

	_, _ = d.DialContext(context.Background(), "tcp", "gardenio-beads:8080")
	if got := derefDial(&dialed); got != "gardenio-beads:8080" {
		t.Fatalf("disabled dialer must pass through verbatim, got %q", got)
	}
	if res.lookups.Load() != 0 {
		t.Fatalf("disabled dialer must not resolve, did %d lookups", res.lookups.Load())
	}
}

// TestThawDialer_ProbeRoute_DialsCachedVIP_AndReportsBlackHole is the dialer-half
// RED→GREEN proof for the cold-VIP-route fix (bug ka-0000002195). It models the
// half the DNS-preresolve does NOT cover: even with DNS off the cold path (we
// dial the cached ClusterIP literal), the first connect to that VIP black-holes
// until kube-proxy reprograms the DNAT/conntrack on the restored netns. probeRoute
// must (a) target the cached ClusterIP literal (zero DNS), (b) surface the
// black-hole as an error so the handler's retry re-probes, and (c) return nil the
// instant the route is programmed.
func TestThawDialer_ProbeRoute_DialsCachedVIP_AndReportsBlackHole(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go acceptAndClose(ln)

	// routeUp models kube-proxy having programmed the VIP DNAT: while down, a
	// connect to the VIP black-holes (errors); once up, it lands on the listener.
	var routeUp atomic.Bool
	var lastDial atomic.Pointer[string]
	std := &net.Dialer{Timeout: 2 * time.Second}
	inner := func(ctx context.Context, network, addr string) (net.Conn, error) {
		a := addr
		lastDial.Store(&a)
		if !routeUp.Load() {
			// Cold cross-ns VIP: SYN black-holes. Model it as a fast connect error
			// (the per-attempt clamp would turn a real black-hole into this).
			return nil, &net.OpError{Op: "dial", Err: errors.New("connect: no route to host (cold VIP)")}
		}
		return std.DialContext(ctx, network, ln.Addr().String())
	}
	d := newThawDialer(inner)
	d.resolver = &fakeResolver{ip: "172.20.0.77"} // stable ClusterIP VIP
	d.enable(context.Background(), "gardenio-beads.gardenio.svc.cluster.local")

	// Route still cold: probe must FAIL (so the handler retry loop keeps probing)
	// and must have targeted the cached ClusterIP literal, not the bare name.
	if err := d.probeRoute(context.Background(), "8080"); err == nil {
		t.Fatal("probeRoute must error while the VIP route is cold (black-holed connect)")
	}
	if got := derefDial(&lastDial); got != "172.20.0.77:8080" {
		t.Fatalf("probeRoute must target the cached ClusterIP VIP literal, got %q", got)
	}

	// kube-proxy programs the route: the next probe lands.
	routeUp.Store(true)
	if err := d.probeRoute(context.Background(), "8080"); err != nil {
		t.Fatalf("probeRoute must succeed once the VIP route is programmed, got %v", err)
	}
}

// TestThawDialer_ProbeRoute_DefaultsPort confirms probeRoute defaults the port to
// the in-cluster beads daemon port (8080) when handed an empty port — the gb
// handler relies on this so a daemon URL without an explicit port still primes
// the right VIP socket.
func TestThawDialer_ProbeRoute_DefaultsPort(t *testing.T) {
	var lastDial atomic.Pointer[string]
	inner := func(ctx context.Context, network, addr string) (net.Conn, error) {
		a := addr
		lastDial.Store(&a)
		return nil, errors.New("inner-reached")
	}
	d := newThawDialer(inner)
	d.resolver = &fakeResolver{ip: "172.20.0.88"}
	d.enable(context.Background(), "gardenio-beads")

	_ = d.probeRoute(context.Background(), "") // empty → default 8080
	if got := derefDial(&lastDial); got != "172.20.0.88:8080" {
		t.Fatalf("probeRoute with empty port must default to :8080, got %q", got)
	}
}

// TestThawDialer_ProbeRoute_NoopWhenDisabled proves that on every non-gVisor /
// external client (dialer never armed) probeRoute is a pure no-op: it returns nil
// WITHOUT dialing or resolving, so the gb handler can call WarmThawRouteOnce
// unconditionally with zero behavior change off the thaw path.
func TestThawDialer_ProbeRoute_NoopWhenDisabled(t *testing.T) {
	res := &fakeResolver{ip: "172.20.0.1"}
	res.failing.Store(true) // would error if the disabled probe ever resolved
	var dialed atomic.Bool
	inner := func(ctx context.Context, network, addr string) (net.Conn, error) {
		dialed.Store(true)
		return nil, errors.New("inner-reached")
	}
	d := newThawDialer(inner) // never enabled
	d.resolver = res

	if err := d.probeRoute(context.Background(), "8080"); err != nil {
		t.Fatalf("disabled probeRoute must be a nil no-op, got %v", err)
	}
	if dialed.Load() {
		t.Fatal("disabled probeRoute must not dial")
	}
	if res.lookups.Load() != 0 {
		t.Fatalf("disabled probeRoute must not resolve, did %d lookups", res.lookups.Load())
	}
}

// TestClient_WarmThawRouteOnce_NoopOnExternalClient is the public-API half: a
// Client built against an EXTERNAL daemon URL (the prod external host coop's
// aws_sts:ok proves is up) never arms the thaw rewrite, so WarmThawRouteOnce is a
// nil no-op — it must never probe / pin the known-good external path.
func TestClient_WarmThawRouteOnce_NoopOnExternalClient(t *testing.T) {
	c, err := New(Config{HTTPAddr: "https://gardenio.app.e2e.dev.acme.example/beads"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c.thawDial.resolver = &fakeResolver{ip: "172.20.0.5"}
	c.EnableThawDNSPrewarm(context.Background()) // external → leaves dialer disabled
	if err := c.WarmThawRouteOnce(context.Background()); err != nil {
		t.Fatalf("WarmThawRouteOnce on an external (unarmed) client must be a nil no-op, got %v", err)
	}
}

// TestClient_WarmThawRouteOnce_PrimesInClusterVIP is the end-to-end public-API
// proof: an in-cluster Client whose boot prewarm cached the ClusterIP makes a real
// TCP-connect probe to that VIP:port through WarmThawRouteOnce, landing on the
// stand-in server. portFromURL must carry the daemon URL's port into the probe.
func TestClient_WarmThawRouteOnce_PrimesInClusterVIP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	host, port, _ := net.SplitHostPort(srv.Listener.Addr().String())

	c, err := New(Config{HTTPAddr: "http://gardenio-beads:" + port})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c.thawDial.resolver = &fakeResolver{ip: host} // bare name → server IP
	c.EnableThawDNSPrewarm(context.Background())
	if _, ip, on := c.ThawDNSPrewarmTarget(); !on || ip != host {
		t.Fatalf("prewarm should arm + cache %q, got enabled=%v ip=%q", host, on, ip)
	}
	if err := c.WarmThawRouteOnce(context.Background()); err != nil {
		t.Fatalf("WarmThawRouteOnce must connect to the cached in-cluster VIP, got %v", err)
	}
}

// TestPortFromURL pins the port-resolution the warm-up relies on.
func TestPortFromURL(t *testing.T) {
	cases := map[string]string{
		"http://gardenio-beads:8080":                      "8080",
		"http://gardenio-beads.gardenio.svc.cluster.local": "8080", // no port → in-cluster default
		"https://gardenio.app.e2e.dev.acme.example/beads":      "443",  // https → 443
		"http://gardenio-beads:9999/v1/hooks/emit":        "9999",
		"": "8080",
	}
	for raw, want := range cases {
		if got := portFromURL(raw); got != want {
			t.Errorf("portFromURL(%q) = %q, want %q", raw, got, want)
		}
	}
}

// TestEnableThawDNSPrewarm_OnlyArmsForInClusterHost wires the public Client API:
// an in-cluster base URL arms the rewrite; an external base URL leaves it a
// passthrough so the external path is never pinned.
func TestEnableThawDNSPrewarm_OnlyArmsForInClusterHost(t *testing.T) {
	cases := []struct {
		name   string
		addr   string
		wantOn bool
	}{
		{"in-cluster FQDN", "http://gardenio-beads.gardenio.svc.cluster.local:8080", true},
		{"in-cluster bare", "http://gardenio-beads:8080", true},
		{"in-cluster ns-qualified", "http://gardenio-beads.gardenio:8080", true},
		{"external public host", "https://gardenio.app.e2e.dev.acme.example/beads", false},
		{"literal IP", "http://172.20.0.5:8080", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := New(Config{HTTPAddr: tc.addr})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			// Inject a fake resolver so the prewarm resolve is offline + deterministic.
			c.thawDial.resolver = &fakeResolver{ip: "172.20.0.5"}
			c.EnableThawDNSPrewarm(context.Background())
			_, _, on := c.ThawDNSPrewarmTarget()
			if on != tc.wantOn {
				t.Fatalf("EnableThawDNSPrewarm(%q): armed=%v, want %v", tc.addr, on, tc.wantOn)
			}
		})
	}
}

// TestClient_ThawHeartbeat_SurvivesColdInClusterDNS is the end-to-end proof on a
// real beadsapi.Client: boot prewarm caches the ClusterIP, the resolver goes
// cold, and an EmitHook (the heartbeat path) to the bare in-cluster host still
// completes against the listener via the cached literal.
//
// RED on unmodified source: no thaw dialer, so EmitHook's dial re-resolves the
// bare name on the cold resolver and fails with EAI_AGAIN.
func TestClient_ThawHeartbeat_SurvivesColdInClusterDNS(t *testing.T) {
	// A real HTTP server stands in for the beads daemon ClusterIP endpoint.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	// srv.Listener.Addr() is 127.0.0.1:PORT — the literal we want every rewritten
	// dial to land on. We configure the client with a BARE in-cluster host and a
	// fake resolver that maps that host to the server's IP at boot, then goes cold.
	host, port, _ := net.SplitHostPort(srv.Listener.Addr().String())

	// Build the client against the bare in-cluster name on the server's PORT so a
	// rewrite to host:port hits the live server.
	c, err := New(Config{HTTPAddr: "http://gardenio-beads:" + port})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c.thawDial.resolver = &fakeResolver{ip: host}

	c.EnableThawDNSPrewarm(context.Background()) // boot: cache host
	if _, ip, on := c.ThawDNSPrewarmTarget(); !on || ip != host {
		t.Fatalf("prewarm should cache %q, got enabled=%v ip=%q", host, on, ip)
	}

	// Thaw: resolver cold.
	c.thawDial.resolver.(*fakeResolver).failing.Store(true)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := c.EmitHook(ctx, EmitHookRequest{AgentBeadID: "gd-thaw", HookType: "PreToolUse"}); err != nil {
		t.Fatalf("thaw heartbeat MUST succeed via cached ClusterIP despite cold in-cluster DNS, got: %v", err)
	}
}

func TestIsInClusterHost(t *testing.T) {
	in := []string{
		"gardenio-beads",                            // bare service (0 dots)
		"gardenio-beads.gardenio",                    // <svc>.<ns> (1 dot, non-public TLD)
		"gardenio-beads.gardenio.svc",                // .svc suffix
		"gardenio-beads.gardenio.svc.cluster.local",  // full FQDN
		"GARDENIO-BEADS.GARDENIO.SVC.CLUSTER.LOCAL",  // case-insensitive
		"gardenio-beads.gardenio.svc.cluster.local.", // trailing-dot tolerated
		"172.20.0.5",                               // literal IP (already resolved)
	}
	for _, h := range in {
		if !isInClusterHost(h) {
			t.Errorf("isInClusterHost(%q) = false, want true", h)
		}
	}
	out := []string{
		"gardenio.app.e2e.dev.acme.example", // multi-label public host (the prod external daemon URL)
		"sts.amazonaws.com",           // external AWS (the aws_sts:ok egress path)
		"example.com",                 // 2-label, public TLD in allowlist → external
		"foo.io",                      // 2-label, public TLD in allowlist → external
		"beads.dev",                   // 2-label, public TLD in allowlist → external
		"",                            // empty
	}
	for _, h := range out {
		if isInClusterHost(h) {
			t.Errorf("isInClusterHost(%q) = true, want false", h)
		}
	}

	// Documented heuristic edge (thaw_dial.go isInClusterHost / looksLikePublicTLD):
	// a 2-label host whose right label is NOT a recognized public TLD is treated
	// as in-cluster (e.g. "<svc>.<ns>"). This branch is unreachable in prod (the
	// gVisor gb daemon URL is only ever the bare or .svc.cluster.local form), and
	// the self-heal re-resolve bounds any mis-pin. We lock the behavior so a future
	// reader knows it is intentional, not an accident.
	if !isInClusterHost("widget.internalns") {
		t.Errorf(`isInClusterHost("widget.internalns") = false, want true (documented <svc>.<ns> heuristic)`)
	}
}

// acceptAndClose accepts connections and immediately closes them — enough for a
// successful TCP connect in a dial test.
func acceptAndClose(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		c.Close()
	}
}

func derefDial(p *atomic.Pointer[string]) string {
	if v := p.Load(); v != nil {
		return *v
	}
	return ""
}
