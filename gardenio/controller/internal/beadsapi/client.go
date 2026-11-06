// Package beadsapi queries the beads daemon for bead state via HTTP/JSON.
package beadsapi

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gardenio/clicreds/avareauth"
	"gardenio/clicreds/beadswire"
	"gardenio/controller/internal/shorttitle"
	"github.com/openziti/sdk-golang/ziti"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"golang.org/x/sync/semaphore"
)

// ErrAgentNotFound is returned by FindAgentBead when no active agent bead
// matches the given agent name. This is expected during agent teardown.
var ErrAgentNotFound = errors.New("agent bead not found")

// AgentBead represents an active agent bead from the daemon.
type AgentBead struct {
	// ID is the bead identifier (e.g., "crew-town-crew-hq", "crew-gardenio-crew-k8s").
	ID string

	// Project is the project name (e.g., "town", "gardenio").
	Project string

	// Mode is the agent mode (e.g., "crew", "job").
	Mode string

	// Role is the agent role (e.g., "crew", "lead", "ops").
	Role string

	// Title is the bead title (e.g., "crew-gardenio-crew-k8s").
	Title string

	// AgentName is the agent's name within its role (e.g., "hq", "k8s").
	AgentName string

	// AgentState is the agent_state field (spawning, prewarmed, working, done, failed).
	AgentState string

	// PodPhase is the pod_phase field (pending, running, succeeded, failed).
	PodPhase string

	// CreatedAt is the bead creation timestamp (used for FIFO scheduling).
	CreatedAt time.Time

	// UpdatedAt is the last time the bead was modified (used for staleness detection).
	UpdatedAt time.Time

	// Metadata contains additional bead metadata from the daemon.
	Metadata map[string]string

	// Labels are the agent bead's labels (e.g. "project:gardenio", "role:crew",
	// and any surfaced backstop label such as "needs-gardener"). Populated from
	// the daemon list view; used by the needs-gardener backstop sweep for
	// idempotency and by the roster JSON output.
	Labels []string

	// GardenerID is the read-only DERIVED gardener-ownership projection
	// (persistent-agents Phase 1, task ka-0000002015). It resolves the canonical
	// owner from the bead's spawn-context fields — gardener_id ∨
	// schedule_id/trigger_id (legacy fallback) ∨ synthetic-gardener:<source> —
	// via DeriveGardenerID. It is total (never empty) and computed at list time,
	// so a legacy bead written before the at-spawn stamp still projects the same
	// owner. A synthetic value (see IsSyntheticGardenerID) marks an agent with no
	// real gardener — the leak the roster's --no-gardener filter surfaces.
	GardenerID string
}

// BeadLister lists active agent beads from the daemon.
type BeadLister interface {
	ListAgentBeads(ctx context.Context) ([]AgentBead, error)
}

// TokenSource provides a valid access token on demand. Implementations may
// cache and auto-refresh tokens (e.g., OAuth refresh_token flow).
type TokenSource interface {
	Token(ctx context.Context) (string, error)
}

// Config for the daemon HTTP client.
type Config struct {
	// HTTPAddr is the daemon HTTP address (e.g., "http://daemon:8080").
	// If the value does not start with "http", it is prefixed with "http://".
	HTTPAddr string

	// Token is an optional static Bearer token for authenticating with the daemon.
	// When set, it is sent as "Authorization: Bearer <token>" on every request.
	// If TokenSource is also set, TokenSource takes precedence.
	Token string

	// RefreshToken, when set together with Token (the static-token path),
	// enables transparent refresh-on-401: a 401 carrying
	// WWW-Authenticate: Bearer error="invalid_token", error_description=
	// "token expired" triggers a refresh exchange against
	// /v1/auth/refresh and a single retry with the new access token.
	// Concurrent expired-token responses funnel through one exchange.
	// Ignored when TokenSource is set — TokenSource owns its own refresh.
	RefreshToken string

	// TokenSource provides dynamic tokens (e.g., from an OAuth credential store).
	// When set, it is called on each request and takes precedence over Token.
	TokenSource TokenSource

	// ZitiIdentityPath is an optional enrolled OpenZiti identity JSON. When set
	// with ZitiService, the client dials the daemon through OpenZiti while
	// keeping HTTPAddr as the logical request origin.
	ZitiIdentityPath string

	// ZitiService is the OpenZiti service name to dial for daemon traffic.
	// Must be set together with ZitiIdentityPath.
	ZitiService string

	// ZitiTLSServerName optionally overrides TLS SNI/certificate verification
	// for HTTPS daemon origins reached through Ziti.
	ZitiTLSServerName string
}

// RefreshCallback is invoked after a successful refresh-token exchange so
// callers can persist the new tokens outside the client. newRefresh is
// non-empty only when the daemon rotated the refresh token (RFC 6749 §10.4).
type RefreshCallback func(newAccess, newRefresh string)

// Client queries the beads daemon via HTTP/JSON.
type Client struct {
	baseURL        string
	tokenSource    TokenSource
	actor          string
	operatedBy     string
	systemOperator string
	edgeAuthKey    []byte // shared HMAC key for edge-auth envelope signing
	edgeAuthKeyV2  []byte // v2 envelope key; empty falls back to edgeAuthKey
	httpClient     *http.Client
	sseClient      *http.Client // long-lived client with no timeout for SSE streams

	// baseTransport is the owned *http.Transport that underlies BOTH httpClient
	// (under the otelhttp wrap) and sseClient (directly). It owns the keep-alive
	// connection pool shared by the two clients. We keep a typed handle so the
	// gb thaw watcher can evict the idle pool on thaw via CloseIdleConnections —
	// httpClient.CloseIdleConnections would not reach it, because
	// otelhttp.Transport does not forward CloseIdleConnections to the base
	// (ka-0000001637).
	baseTransport *http.Transport

	// thawDial wraps baseTransport.DialContext to rewrite the in-cluster beads
	// host to a pre-resolved ClusterIP literal on the gVisor thaw path, taking
	// DNS off the cold post-restore netns (bug ka-0000000696). Disabled by
	// default (pure passthrough); EnableThawDNSPrewarm arms it for gVisor pods.
	// See thaw_dial.go.
	thawDial *thawDialer

	// batchShardLimiter caps this client's total in-flight sharded batch
	// requests against the daemon. Shared by every sharded batch helper (see
	// batch_shard.go) so concurrent joins cannot multiply the cap.
	batchShardLimiter *semaphore.Weighted

	// parents caches IsAncestor's parent-child lookups; nil (the default)
	// fetches every level. See EnableParentCache.
	parents *parentCache

	// batchIDCap is the per-request ID width the batch helpers currently
	// believe this daemon accepts. Zero means "use the compiled-in
	// BatchGetBeadsMaxIDs"; it is only ever ratcheted DOWN, by a daemon
	// rejecting an oversized request. See batch_cap.go.
	batchIDCap atomic.Int64

	// tokenMu guards token + refreshToken + onRefresh. Read briefly per
	// request; refresh exchanges serialise on refreshMu instead.
	tokenMu      sync.RWMutex
	token        string
	refreshToken string
	onRefresh    RefreshCallback

	// refreshMu serialises calls to /v1/auth/refresh so concurrent 401s
	// share a single exchange — the post-acquire token re-read short-circuits
	// peer goroutines that arrived while the first one held the lock.
	refreshMu sync.Mutex

	// sleep waits out the connection-reset and DB-auth retry backoffs in
	// doJSON. Nil means beadswire.SleepWithContext; tests swap it so they do
	// not wall-clock wait.
	sleep func(ctx context.Context, d time.Duration) error

	// requireUserBearerForWrites, when true, fail-closes mutating daemon calls
	// that arrive without a per-request user bearer (WithUserBearer) and lets a
	// present user bearer override the static read token on the wire. Toggled by
	// SetRequireUserBearerForWrites (BEADS_VIEWER_REQUIRE_USER_BEARER_WRITES).
	// See user_bearer.go and ka-0000006379. Default false (no behavior change).
	requireUserBearerForWrites bool
	// preferUserBearerForWrites makes a per-request user bearer (WithUserBearer)
	// override the client's own credential on mutating calls WITHOUT the
	// fail-closed gate: a bearer-less write still goes out on the client's
	// token. slack-bridge uses it while its autonomous writes (config seed, DM
	// delivery) are still on its own credential and only the human path
	// carries the linked person's token (gd-OZJ6H2S3xx); the human path
	// refuses an unlinked user before any write is attempted.
	preferUserBearerForWrites bool

	// requireUserBearerForReads is the read-side twin: reads authenticate as
	// the user bearer on ctx, and a read with no bearer and no WithSystemRead
	// marker fails closed. Toggled by SetRequireUserBearerForReads
	// (BEADS_VIEWER_REQUIRE_USER_BEARER_READS). Default false.
	requireUserBearerForReads bool

	// systemPrincipal, when set, authenticates reads marked WithSystemRead and
	// every EventStream instead of the static token. See SetSystemPrincipal.
	systemPrincipal TokenSource

	// mrUpsertLocks serialises UpsertMergeRequestBead calls per mr_url so the
	// client-side find-then-create critical section is atomic within this
	// process. The gitlab-bridge runs single-replica and fans out SSE handlers
	// concurrently, so two near-simultaneous bead.updated events (one per bead
	// linked to the same MR via `gb mr link`) would both miss the existing-MR
	// lookup and each INSERT a duplicate type=merge-request bead. Keyed on
	// canonical mr_url. Map of mr_url → *sync.Mutex; entries are not reclaimed
	// (the live MR-URL set is small and bounded). See bug ka-0000007606.
	mrUpsertLocks sync.Map

	// onDaemonVersion, when set, is invoked at most once with the daemon's
	// X-Kbeads-Version response header the first time it is observed. The gb
	// CLI uses it to warn about version skew without coupling the transport to
	// stdio (ka-0000002965). Guarded by versionOnce.
	onDaemonVersion func(daemonVersion string)
	versionOnce     sync.Once

	// onBeadList, when set, is told how long each GET /v1/beads call took
	// (SetOnBeadList).
	onBeadList BeadListObserver

	// getBeadCache is an OPTIONAL request-scoped memo for GetBead, armed by
	// BeginGetBeadCache and disarmed by EndGetBeadCache. When non-nil, GetBead
	// serves a cached copy for a repeated id instead of re-issuing the round-
	// trip. It exists for read-heavy renders that fan out over many helpers
	// which each independently re-fetch the SAME few beads — notably `gb prime`,
	// where a single render read the agent's own bead ~8 times (once per section
	// AND once inside advice enrichment) and the operator user bead twice, every
	// call returning the identical document (ka-0000002397).
	//
	// Nil by default, so long-lived clients (the controller, bridges) are
	// completely unaffected — they never see stale reads. Errors are never
	// cached. Guarded by getBeadCacheMu because a render may fan out concurrently.
	getBeadCacheMu sync.Mutex
	getBeadCache   map[string]*BeadDetail

	// zitiContext, when set, is the OpenZiti context backing the daemon
	// transport. It is released by Close(). Nil for plain-HTTP clients.
	zitiContext zitiDialer
}

// BeginGetBeadCache arms a fresh request-scoped GetBead memo on this client.
// Intended for a single read-heavy render (see the getBeadCache field). Call
// EndGetBeadCache (typically deferred) when the render returns so the memo never
// outlives it. Re-arming discards any prior contents.
func (c *Client) BeginGetBeadCache() {
	c.getBeadCacheMu.Lock()
	c.getBeadCache = map[string]*BeadDetail{}
	c.getBeadCacheMu.Unlock()
}

// EndGetBeadCache disarms the request-scoped GetBead memo so subsequent GetBead
// calls go straight to the daemon again.
func (c *Client) EndGetBeadCache() {
	c.getBeadCacheMu.Lock()
	c.getBeadCache = nil
	c.getBeadCacheMu.Unlock()
}

// New creates an HTTP client for querying the beads daemon.
func New(cfg Config) (*Client, error) {
	addr := cfg.HTTPAddr
	if addr == "" {
		return nil, fmt.Errorf("HTTPAddr is required")
	}
	if !strings.HasPrefix(addr, "http://") && !strings.HasPrefix(addr, "https://") {
		addr = "http://" + addr
	}
	addr = strings.TrimRight(addr, "/")

	ztx, zitiDial, tlsServerName, err := newDaemonZitiDialer(cfg)
	if err != nil {
		return nil, err
	}

	// Own a dedicated base transport (a clone of http.DefaultTransport, so we
	// inherit its proxy/keep-alive/timeout defaults) rather than sharing the
	// process-global http.DefaultTransport. Owning it gives us a typed handle to
	// the keep-alive connection pool so the gb thaw watcher can evict it on thaw
	// (CloseIdleConnections, ka-0000001637): on a checkpoint/restore the pre-freeze
	// idle conns are bound to Pod-A's now-dead 4-tuple, and reusing one to a
	// black-holed (no-RST) peer hangs to the 30s client Timeout. Eviction-by-handle
	// is the ONLY reliable path here — http.Client.CloseIdleConnections walks only
	// the outermost transport for a CloseIdleConnections method, and
	// otelhttp.Transport does not forward it to the base.
	base, _ := http.DefaultTransport.(*http.Transport)
	if base != nil {
		base = base.Clone()
	} else {
		base = &http.Transport{}
	}
	// Size the keep-alive pool for our actual traffic shape (ka-0000000376).
	// http.DefaultTransport leaves MaxIdleConnsPerHost at 0, which means
	// DefaultMaxIdleConnsPerHost — *2*. That default is tuned for a client
	// talking to many hosts; this client talks to exactly ONE (the daemon) and
	// issues concurrent batch shards against it. At 2, every request past the
	// second finds no idle conn to reuse, pays a fresh TCP+TLS handshake, and
	// then has its connection dropped instead of returned to the pool — so
	// fanning shards out concurrently bought far less than it should (measured:
	// an 8-way fan-out ran ~2x faster, where the same fan-out at the curl level
	// ran ~4.8x). Raising the per-host idle cap to match the shard concurrency
	// lets every concurrent shard reuse a warm connection.
	//
	// Kept >= batchShardConcurrency so a full-width fan-out never contends;
	// MaxIdleConns (100, inherited) still bounds the process-wide total.
	base.MaxIdleConnsPerHost = batchShardConcurrency * 2
	// When an OpenZiti identity+service is configured, dial the daemon through
	// the Ziti overlay instead of TCP. Ziti replaces the underlying dialer while
	// HTTPAddr stays the logical request origin (Host header), so bearer auth
	// is unaffected — Ziti only gates reachability. The dialer
	// is installed at the base transport so BOTH httpClient and sseClient reach
	// the daemon over the overlay and share one keep-alive pool.
	if zitiDial != nil {
		base.DialContext = zitiDial
		if tlsServerName != "" {
			if base.TLSClientConfig == nil {
				base.TLSClientConfig = &tls.Config{}
			} else {
				base.TLSClientConfig = base.TLSClientConfig.Clone()
			}
			base.TLSClientConfig.ServerName = tlsServerName
		}
	}
	// Wrap the base dialer so the gVisor thaw path can dial the in-cluster beads
	// ClusterIP literal instead of re-resolving its bare name on the cold
	// post-restore netns (bug ka-0000000696). Disabled until EnableThawDNSPrewarm
	// arms it, so this is a transparent passthrough (one atomic load per dial) on
	// every non-gVisor client. Installed at the base transport so BOTH httpClient
	// (heartbeat) and sseClient share the one rewrite.
	thawDial := newThawDialer(base.DialContext)
	base.DialContext = thawDial.DialContext
	// otelhttp wraps the base transport so every short-lived daemon call emits
	// a client span and injects W3C trace context, letting the kbeads server
	// continue the same trace. No-op until tracing.Init installs a real
	// provider. The long-lived SSE client is left uninstrumented — a single
	// open stream would otherwise hold one span for its entire lifetime. Both
	// clients sit above the SAME owned base, so they share one keep-alive pool
	// and one CloseIdleConnections eviction reaches both.
	return &Client{
		baseURL:       addr,
		token:         cfg.Token,
		refreshToken:  cfg.RefreshToken,
		tokenSource:   cfg.TokenSource,
		baseTransport: base,
		thawDial:      thawDial,
		zitiContext:   ztx,

		batchShardLimiter: newBatchShardLimiter(),
		httpClient: &http.Client{
			Timeout:   30 * time.Second,
			Transport: otelhttp.NewTransport(base),
		},
		sseClient: &http.Client{
			Timeout:   0,
			Transport: base,
		},
	}, nil
}

type zitiDialer interface {
	DialContext(ctx context.Context, serviceName string) (net.Conn, error)
	Close()
}

var newZitiDialer = func(identityPath string) (zitiDialer, error) {
	cfg, err := ziti.NewConfigFromFile(identityPath)
	if err != nil {
		return nil, err
	}
	ztx, err := ziti.NewContext(cfg)
	if err != nil {
		return nil, err
	}
	return zitiContextAdapter{Context: ztx}, nil
}

type zitiContextAdapter struct {
	ziti.Context
}

func (a zitiContextAdapter) DialContext(ctx context.Context, serviceName string) (net.Conn, error) {
	return a.Context.DialContext(ctx, serviceName)
}

// newDaemonZitiDialer builds an OpenZiti dialer for the daemon transport when
// cfg carries both a Ziti identity and service. It returns the live ziti
// context (for Close), a DialContext replacement that dials the configured
// service, and an optional TLS server-name override. When no Ziti fields are
// set it returns nils so the caller keeps its plain-TCP base transport.
// Identity and service must be set together, or it errors.
func newDaemonZitiDialer(cfg Config) (zitiDialer, func(ctx context.Context, network, addr string) (net.Conn, error), string, error) {
	identityPath := strings.TrimSpace(cfg.ZitiIdentityPath)
	serviceName := strings.TrimSpace(cfg.ZitiService)
	tlsServerName := strings.TrimSpace(cfg.ZitiTLSServerName)
	if identityPath == "" && serviceName == "" {
		return nil, nil, "", nil
	}
	if identityPath == "" || serviceName == "" {
		return nil, nil, "", fmt.Errorf("ziti identity and ziti service must be set together")
	}

	ztx, err := newZitiDialer(identityPath)
	if err != nil {
		return nil, nil, "", fmt.Errorf("initializing ziti transport: %w", err)
	}
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		return ztx.DialContext(ctx, serviceName)
	}
	return ztx, dial, tlsServerName, nil
}

// SetRefreshToken installs (or clears) a refresh token on the static-token
// path. When non-empty, doJSON will exchange this refresh for a new access
// token on a 401-with-expired-challenge and retry the failing request once.
// No-op when a TokenSource is configured (TokenSource owns its own refresh).
func (c *Client) SetRefreshToken(refreshToken string) {
	c.tokenMu.Lock()
	c.refreshToken = refreshToken
	c.tokenMu.Unlock()
}

// SetOnRefresh installs a callback fired after a successful refresh-token
// exchange. Receives (new access, new refresh); the latter is "" unless the
// daemon rotated. Synchronous — keep it short. Pass nil to clear.
func (c *Client) SetOnRefresh(cb RefreshCallback) {
	c.tokenMu.Lock()
	c.onRefresh = cb
	c.tokenMu.Unlock()
}

// StaticToken returns the client's current static-path access token, as last
// refreshed ("" with a TokenSource, or with no token). gb forwards a per-agent
// token to the slack-bridge with it, after a Me call has refreshed it.
func (c *Client) StaticToken() string {
	if c.tokenSource != nil {
		return ""
	}
	c.tokenMu.RLock()
	defer c.tokenMu.RUnlock()
	return c.token
}

// Close releases any OpenZiti context backing the HTTP client. It is a no-op
// for plain HTTP clients (satisfies the old interface contract).
func (c *Client) Close() error {
	if c.zitiContext != nil {
		c.zitiContext.Close()
		c.zitiContext = nil
	}
	return nil
}

// CloseIdleConnections evicts every idle keep-alive connection in the client's
// owned transport pool, forcing the next request to dial fresh (re-running DNS
// resolution and the TCP/TLS handshake). The gb thaw watcher calls this on each
// thaw epoch BEFORE its first heartbeat so a connection pooled against Pod-A's
// dead 4-tuple is never reused on the restored Pod-B netns (ka-0000001637): a
// reused conn to a black-holed (no-RST) peer would otherwise hang to the 30s
// client Timeout and blow a short rehydrate deadline.
//
// It evicts the base transport DIRECTLY rather than delegating to
// c.httpClient.CloseIdleConnections(): that stdlib call is a silent no-op here,
// because it only invokes CloseIdleConnections on the outermost transport that
// implements it, and otelhttp.Transport does not forward it to the base. Both
// httpClient and sseClient share this one base pool, so a single eviction
// covers both.
//
// Idempotent and safe on first start: CloseIdleConnections on a pool with no
// idle connections is a no-op, and baseTransport is always non-nil after New.
func (c *Client) CloseIdleConnections() {
	if c == nil || c.baseTransport == nil {
		return
	}
	c.baseTransport.CloseIdleConnections()
}

// EnableThawDNSPrewarm arms the gVisor-thaw in-cluster DNS-preresolve for this
// client's base dialer and runs the boot prewarm (bug ka-0000000696). Call it
// ONCE at boot from the gb thaw watcher (gVisor pods only), while the boot netns
// is still healthy, so the in-cluster beads ClusterIP is resolved and cached in
// RAM before any freeze. The gVisor freeze snapshots that cached literal; on
// thaw every dial to the configured beads host targets the cached ClusterIP
// directly — ZERO DNS on the cold post-restore netns where kube-DNS is not yet
// routable (the EAI_AGAIN class that times out the first heartbeat POST and
// fail-closes the rehydrate gate). The Host header is unchanged — only the TCP
// dial target is rewritten.
//
// No-op (leaves the dialer a passthrough) when the client's base URL is not an
// in-cluster Kubernetes Service host: an external daemon URL already survives
// the thaw (coop's aws_sts:ok proves external egress is up) and its address may
// be a rotating LB/CDN that must not be pinned. Safe to call on a freshly-booted
// pod (the prewarm resolve is just one extra lookup) and idempotent in effect
// (re-arming the same host simply refreshes the cache). ctx bounds the boot
// prewarm resolve.
func (c *Client) EnableThawDNSPrewarm(ctx context.Context) {
	if c == nil || c.thawDial == nil {
		return
	}
	host := hostFromURL(c.baseURL)
	if host == "" || net.ParseIP(host) != nil {
		// No host to resolve, or the base URL is already a literal IP (nothing
		// for DNS to do on thaw) — leave the dialer a passthrough.
		return
	}
	if !isInClusterHost(host) {
		// External daemon URL: do not pin its address. The external path already
		// survives the thaw; rewriting it could pin a stale LB/CDN IP.
		return
	}
	c.thawDial.enable(ctx, strings.ToLower(host))
}

// ThawDNSPrewarmTarget returns the armed in-cluster host, its cached ClusterIP
// literal (empty until the boot prewarm or a self-heal resolve lands one), and
// whether the thaw rewrite is enabled. For logging and tests.
func (c *Client) ThawDNSPrewarmTarget() (host, cachedIP string, enabled bool) {
	if c == nil {
		return "", "", false
	}
	return c.thawDial.cachedTarget()
}

// SetActor configures the X-Actor header sent with every request. The server
// honors this only for primary-static-token callers — it is ignored for OIDC
// and fleet-scoped tokens, so setting it unconditionally is safe. Empty string
// disables the header.
func (c *Client) SetActor(actor string) {
	c.actor = actor
}

// SetOnDaemonVersion installs a callback invoked at most once with the
// daemon's X-Kbeads-Version response header. The gb CLI uses it to warn about
// version skew when the local binary is materially behind the daemon
// (ka-0000002965). Pass nil to clear.
func (c *Client) SetOnDaemonVersion(cb func(daemonVersion string)) {
	c.onDaemonVersion = cb
}

// noteDaemonVersion fires the onDaemonVersion callback the first time a
// non-empty X-Kbeads-Version header is seen. Safe to call on every response.
func (c *Client) noteDaemonVersion(daemonVersion string) {
	if daemonVersion == "" || c.onDaemonVersion == nil {
		return
	}
	c.versionOnce.Do(func() { c.onDaemonVersion(daemonVersion) })
}

// SetOperatedBy configures the X-Operated-By header sent with every request.
// Carries the human operator id (e.g. "slack:U…") on whose behalf this call is
// made. The server honors this only for primary-static-token callers. Post-R2
// (ka-0000005772) the server fail-closes on write paths when no operator can
// be resolved, so primary-static-token callers MUST set this (or supply it via
// WithOperator per call) — leaving it empty surfaces as 400 / InvalidArgument.
// Per-call overrides via WithOperator(ctx, "slack:U…") take precedence.
// See docs/research/human-attribution-api.md (R6).
//
// The value MUST be a human-shaped id ("slack:U…" or "oidc:<local>@<domain>")
// — the daemon's sanitizeOperatedByHeader rejects sentinel shapes ("system:*",
// "fleet:*", bare agent names) and silently drops audit/event records for the
// affected mutation. For service-identity writes (e.g. controller housekeeping),
// use SetSystemOperator instead (ka-0000004577).
func (c *Client) SetOperatedBy(op string) {
	c.operatedBy = op
}

// SetSystemOperator configures the X-System-Operator header sent with every
// request. The daemon prefixes the value with "system:" and uses it as the
// last-resort operator ONLY when no human operator can be resolved from OIDC
// or X-Operated-By (ka-0000007616). The source label MUST match the daemon's
// sanitizeSystemOperatorHeader rules: ASCII lowercase letters, digits, or "-",
// length 1..64, with no leading/trailing/double "-".
//
// Use this for service-identity defaults (e.g. controller housekeeping,
// reconciler writes, status-reporter pod-phase updates) so the daemon stamps
// a system: sentinel without firing the X-Operated-By sanitizer-reject path
// (ka-0000004577). Per-call WithOperator (X-Operated-By) and WithSystemOperator
// still take precedence on the wire.
//
// Empty string disables the default header.
func (c *Client) SetSystemOperator(source string) {
	c.systemOperator = source
}

type operatorKeyType struct{}

// WithOperator returns ctx with the operator id attached. The next request
// using this context will send X-Operated-By: <op>, overriding the client-level
// default from SetOperatedBy. Used by bridges to stamp per-event operators
// (e.g. the Slack user who triggered a /spawn) without mutating shared client
// state.
func WithOperator(ctx context.Context, op string) context.Context {
	return context.WithValue(ctx, operatorKeyType{}, op)
}

// OperatorFromContext returns the operator id attached via WithOperator, or "".
func OperatorFromContext(ctx context.Context) string {
	v, _ := ctx.Value(operatorKeyType{}).(string)
	return v
}

type actorKeyType struct{}

// WithActor returns ctx with the actor identity attached. The next request
// using this context sends X-Actor: <actor>, overriding the client-level
// default from SetActor for that one call. The server honors X-Actor only for
// primary-static-token callers, using it as the audit ACTOR (comment.Author,
// CreatedBy, event actor) — distinct from the human OPERATOR carried by
// X-Operated-By. Setting it is safe for OIDC/fleet callers (ignored).
//
// Per-request (vs SetActor's client-wide default) so a shared client serving
// concurrent requests attributes each write to its own authenticated caller
// without racing on mutable client state — used by beads-viewer to stamp the
// logged-in user's email as the comment author (ka-0000006262).
func WithActor(ctx context.Context, actor string) context.Context {
	return context.WithValue(ctx, actorKeyType{}, actor)
}

// ActorFromContext returns the actor attached via WithActor, or "".
func ActorFromContext(ctx context.Context) string {
	v, _ := ctx.Value(actorKeyType{}).(string)
	return v
}

type systemOperatorKeyType struct{}

// WithSystemOperator returns ctx with a last-resort system-operator
// source label attached. The next request using this context will send
// X-System-Operator: <source>, which the daemon prefixes with "system:"
// and uses ONLY when no human operator can be resolved from OIDC or
// X-Operated-By (ka-0000007616).
//
// Use this on server-internal recording paths whose source record may
// carry no human attribution — most importantly the scheduler dual-write
// of legacy pre-attribution schedules. OIDC and WithOperator still take
// precedence on the server, so it is safe to set unconditionally on
// every dual-write call.
//
// Pass "" to clear the override for nested calls (rarely needed).
func WithSystemOperator(ctx context.Context, source string) context.Context {
	return context.WithValue(ctx, systemOperatorKeyType{}, source)
}

// SystemOperatorFromContext returns the source label attached via
// WithSystemOperator, or "" when none was set.
func SystemOperatorFromContext(ctx context.Context) string {
	v, _ := ctx.Value(systemOperatorKeyType{}).(string)
	return v
}

// SetEdgeAuthHMACKey installs the shared HMAC key the client uses to sign
// the edge-auth envelope on requests that carry an edge-auth operator
// (see WithEdgeAuthOperator). The key MUST be the same secret configured
// on the daemon's edge-auth verifier (DAEMON_HMAC_KEY in
// gardenio/helm/gardenio/). Pass nil to disable signing.
//
// When the key is set AND a context carries WithEdgeAuthOperator, sendOnce
// stamps X-Operator + X-Edge-Auth-Sig + X-Edge-Auth-Ts + X-Auth-Audit-Id
// in place of the X-Operated-By default. The daemon's EdgeAuthMiddleware
// then verifies and stamps AuthIdentity{Type:"edge", Subject:operator},
// which satisfies ownership for decision beads whose bead.OperatedBy
// matches (ka-0000007637).
func (c *Client) SetEdgeAuthHMACKey(key []byte) {
	c.edgeAuthKey = key
}

// SetEdgeAuthHMACKeyV2 installs the key a role-bearing (v2) envelope is
// signed with (WithEdgeAuthIdentity). It MUST match DAEMON_HMAC_KEY_V2 on the
// daemon. The v1 and v2 keys are split so a v1-only signer cannot mint a v2
// admin envelope (gd-dMof1PW1oJ); while it is unset, v2 signs with the
// SetEdgeAuthHMACKey key, as before the split.
func (c *Client) SetEdgeAuthHMACKeyV2(key []byte) {
	c.edgeAuthKeyV2 = key
}

func (c *Client) edgeAuthV2Key() []byte {
	if len(c.edgeAuthKeyV2) > 0 {
		return c.edgeAuthKeyV2
	}
	return c.edgeAuthKey
}

type edgeAuthOperatorKeyType struct{}

// WithEdgeAuthOperator returns ctx with an operator identity that the
// next request will assert via the edge-auth HMAC envelope. The operator
// MUST be in human-operator shape (e.g. "slack:U0OPERATOR1" or
// "oidc:matt@acme.example"). Requires SetEdgeAuthHMACKey to be configured
// — if no key is set, the headers are not stamped and the request falls
// back to the standard X-Operated-By path.
//
// Bridges use this on close-from-Slack-interaction so the daemon
// authorizes the close against the Slack user's identity rather than
// the bridge's fleet token. See ka-0000007637.
func WithEdgeAuthOperator(ctx context.Context, op string) context.Context {
	return context.WithValue(ctx, edgeAuthOperatorKeyType{}, op)
}

// EdgeAuthOperatorFromContext returns the operator id attached via
// WithEdgeAuthOperator, or "".
func EdgeAuthOperatorFromContext(ctx context.Context) string {
	v, _ := ctx.Value(edgeAuthOperatorKeyType{}).(string)
	return v
}

// EdgeAuthIdentity is a role-bearing (v2) edge-auth identity: the person the
// daemon should treat the request as, the mode that vouches for them, and the
// canonical gardenio roles they hold. Roles are authoritative to the daemon, so
// callers MUST source them from the identity provider the daemon trusts
// (Keycloak) and never from config or user input.
type EdgeAuthIdentity struct {
	// Operator is the human operator id ("oidc:<email>").
	Operator string
	// AuthMode names the trust path; the slack-bridge uses "slack".
	AuthMode string
	// Roles are canonical gardenio-viewer/-operator/-admin roles. Signed
	// sorted, deduplicated and comma-joined, as the daemon requires.
	Roles []string
}

type edgeAuthIdentityKeyType struct{}

// WithEdgeAuthIdentity returns ctx carrying a role-bearing edge-auth identity.
// The next request signs a v2 envelope (operator, mode, roles, HTTP method and
// path all bound into the HMAC). It takes precedence over WithEdgeAuthOperator
// and FAILS CLOSED: if the client has no HMAC key the request errors rather
// than falling back to the fleet token with an X-Operated-By header, which
// would silently drop the roles the caller asked to act with.
func WithEdgeAuthIdentity(ctx context.Context, id EdgeAuthIdentity) context.Context {
	return context.WithValue(ctx, edgeAuthIdentityKeyType{}, id)
}

// EdgeAuthIdentityFromContext returns the identity attached via
// WithEdgeAuthIdentity and whether one was set.
func EdgeAuthIdentityFromContext(ctx context.Context) (EdgeAuthIdentity, bool) {
	v, ok := ctx.Value(edgeAuthIdentityKeyType{}).(EdgeAuthIdentity)
	return v, ok
}

// signEdgeAuthV2 mirrors cmd/edge-auth/proxy.go signEdgeAuthV2 and the
// daemon's canonicalHMACV2 (kbeads/internal/edgeauth).
func signEdgeAuthV2(key []byte, operator, authMode, roles, auditID, method, path, ts string) string {
	m := hmac.New(sha256.New, key)
	_, _ = m.Write([]byte(strings.Join([]string{
		"v2", operator, authMode, roles, auditID, strings.ToUpper(method), path, ts,
	}, "\n")))
	return base64.StdEncoding.EncodeToString(m.Sum(nil))
}

// signEdgeAuth computes the base64 HMAC-SHA256 of
// operator\nauditID\npath\nts using key. Mirrors the signing in
// gardenio/controller/cmd/edge-auth/proxy.go and is verified by
// kbeads/internal/edgeauth/verify.go.
func signEdgeAuth(key []byte, operator, auditID, path, ts string) string {
	m := hmac.New(sha256.New, key)
	_, _ = m.Write([]byte(operator + "\n" + auditID + "\n" + path + "\n" + ts))
	return base64.StdEncoding.EncodeToString(m.Sum(nil))
}

// newEdgeAuthAuditID returns a fresh correlation id for the audit
// header. Hex-encoded random bytes — opaque to the daemon, which only
// uses it for log correlation.
func newEdgeAuthAuditID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(b[:])
}

// setAuth sets the Authorization header on req. TokenSource (dynamic) takes
// precedence over the static token. Returns the token actually written to
// the header — refreshAccessToken needs this to make its single-flight bail
// decision (a peer has already refreshed iff c.token has moved past it).
func (c *Client) setAuth(ctx context.Context, req *http.Request, overrideBearer string) (string, error) {
	// A per-request user bearer (write-bearer enforcement, ka-0000006379) wins
	// over both the dynamic TokenSource and the static read token: the write
	// must authenticate to the daemon AS THE USER so the OIDC RBAC + attribution
	// path runs instead of the privileged static-token bypass.
	if overrideBearer != "" {
		req.Header.Set("Authorization", "Bearer "+overrideBearer)
		return overrideBearer, nil
	}
	if c.tokenSource != nil {
		tok, err := c.tokenSource.Token(ctx)
		if err != nil {
			return "", err
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		return tok, nil
	}
	c.tokenMu.RLock()
	tok := c.token
	c.tokenMu.RUnlock()
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	return tok, nil
}

// activeStatuses is the set of statuses that represent non-closed beads.
var activeStatuses = []string{"open", "in_progress", "blocked", "deferred"}

// ActiveStatuses returns a fresh copy of the canonical non-closed status set —
// the same set the credential-account listers filter on (ListAccountBeads et
// al.). Callers that build a beadsapi.ListBeadsQuery directly (e.g. the metrics
// collector, which only has the ListBeadsFiltered surface) use this so their
// notion of an "active" bead stays in lockstep with the internal listers. A
// copy is returned so callers cannot mutate the shared backing slice.
func ActiveStatuses() []string {
	return slices.Clone(activeStatuses)
}

// ListAgentBeads queries the daemon for active agent beads (type=agent).
//
// It asks the daemon to omit each bead's description: AgentBead has nowhere
// to put it, so leaving it out changes nothing a caller can see.
func (c *Client) ListAgentBeads(ctx context.Context) ([]AgentBead, error) {
	return c.listAgentBeads(ctx, url.Values{"omit": {"description"}})
}

// LeanAgentOmitFields are the agent-bead fields ListAgentBeadsLean leaves
// out. Measured 2026-10-23 over 300 agent beads: prompt was 71% of the bytes
// and wrapup 12%; description 4% (gd-zXdZq4Traf).
var LeanAgentOmitFields = []string{"prompt", "wrapup"}

// ListAgentBeadsLean is ListAgentBeads without the LeanAgentOmitFields keys:
// Metadata["prompt"] and Metadata["wrapup"] read empty, everything else
// (fields, notes, labels) is the same. For the roster
// polls that re-list every active agent every 30-60 s and never read those —
// metrics, the prewarmed pool, the role cache, the stale reaper. The
// reconciler keeps ListAgentBeads: it hands Metadata["prompt"] to a recreated
// pod as BOAT_PROMPT.
func (c *Client) ListAgentBeadsLean(ctx context.Context) ([]AgentBead, error) {
	return c.listAgentBeads(ctx, url.Values{
		"omit":        {"description"},
		"omit_fields": {strings.Join(LeanAgentOmitFields, ",")},
	})
}

// LeanAgentLister adapts a Client so ListAgentBeads returns the lean
// projection, for consumers that take a narrow ListAgentBeads interface.
type LeanAgentLister struct{ *Client }

// ListAgentBeads implements the narrow lister interfaces with ListAgentBeadsLean.
func (l LeanAgentLister) ListAgentBeads(ctx context.Context) ([]AgentBead, error) {
	return l.ListAgentBeadsLean(ctx)
}

func (c *Client) listAgentBeads(ctx context.Context, extra url.Values) ([]AgentBead, error) {
	resp, err := c.listBeadsWithParams(ctx, []string{"agent"}, activeStatuses, nil, extra)
	if err != nil {
		return nil, fmt.Errorf("listing agent beads: %w", err)
	}

	var beads []AgentBead
	for _, b := range resp.Beads {
		fields := b.fieldsMap()
		project := fields["project"]
		mode := fields["mode"]
		role := fields["role"]
		name := fields["agent"]
		if mode == "" {
			mode = "crew"
		}
		if name == "" {
			continue
		}
		if role == "" {
			role = "crew"
		}
		// Merge bead fields and notes into metadata. Fields provide custom
		// overrides (e.g., mock_scenario, image); notes provide runtime state
		// (e.g., coop_url, pod_name). Notes take precedence over fields.
		meta := make(map[string]string, len(fields))
		for k, v := range fields {
			meta[k] = v
		}
		for k, v := range ParseNotes(b.Notes) {
			meta[k] = v
		}
		// Dark discriminator (ka-0000005242): stamp the dark-pod marker on the
		// reconciler path too, so a re-imaged/reconciled dark agent gets the
		// same Jira write-token fence + egress label the events path
		// (subscriber.buildEvent) applies. Derived from the bead's labels /
		// project — the same signal the dark-only MR-cap + budget controls use.
		if IsDark(b.Labels, project) {
			meta[MetaDarkPod] = "true"
		}
		beads = append(beads, AgentBead{
			ID:         b.ID,
			Title:      b.Title,
			Project:    project,
			Mode:       mode,
			Role:       role,
			AgentName:  name,
			AgentState: fields["agent_state"],
			PodPhase:   fields["pod_phase"],
			CreatedAt:  parseTimestamp(b.CreatedAt),
			UpdatedAt:  parseTimestamp(b.UpdatedAt),
			Metadata:   meta,
			Labels:     b.Labels,
			// Read-only derived gardener-ownership projection. Resolved from the
			// merged field/note metadata so a note-set gardener_id (or a legacy
			// bead's schedule_id/trigger_id fallback) is honored the same way the
			// at-spawn stamp would.
			GardenerID: DeriveGardenerID(meta),
		})
	}

	return beads, nil
}

// ListTaskBeads queries the daemon for active task beads (type=task).
func (c *Client) ListTaskBeads(ctx context.Context) ([]*BeadDetail, error) {
	resp, err := c.listBeads(ctx, []string{"task"}, activeStatuses)
	if err != nil {
		return nil, fmt.Errorf("listing task beads: %w", err)
	}
	beads := make([]*BeadDetail, 0, len(resp.Beads))
	for _, b := range resp.Beads {
		beads = append(beads, b.toDetail())
	}
	return beads, nil
}

// allTaskStatuses includes closed beads for MR matching — review comments
// can arrive after the original task bead is closed.
var allTaskStatuses = []string{"open", "in_progress", "blocked", "deferred", "closed"}

// ListTaskBeadsAll queries the daemon for task beads in ALL statuses,
// including closed. Used by GitLab webhook/sync handlers where review
// comments may arrive after the task is complete.
//
// PREFER ListTaskBeadsForMR when you only need the tasks bound to one MR. This
// call fetches the ENTIRE task corpus at full BeadDetail and is capped at
// clientListAllLimit, so on a large corpus it is both a memory spike and a
// silently-incomplete result set. See ka-0000007917 and advice ka-0000003555.
func (c *Client) ListTaskBeadsAll(ctx context.Context) ([]*BeadDetail, error) {
	resp, err := c.listBeads(ctx, []string{"task"}, allTaskStatuses)
	if err != nil {
		return nil, fmt.Errorf("listing all task beads: %w", err)
	}
	beads := make([]*BeadDetail, 0, len(resp.Beads))
	for _, b := range resp.Beads {
		beads = append(beads, b.toDetail())
	}
	return beads, nil
}

// mrTaskLookupLimit bounds the per-MR task lookup. A single MR realistically
// binds a handful of task beads (the producing task plus any mention tasks), so
// this is generous headroom rather than a functional cap. The same bound applies
// to the per-JIRA-key lookup.
const mrTaskLookupLimit = 500

// ListTaskBeadsForJiraKey returns task beads in ALL statuses (including closed)
// whose fields.jira_key equals jiraKey.
//
// Same rationale as ListTaskBeadsForMR: the JIRA comment/mention paths used to
// pull the entire task corpus per event and filter in Go, which is both a memory
// spike and silently lossy past the list cap (ka-0000007917, advice
// ka-0000003555). Closed beads are retained — a JIRA follow-up comment usually
// lands after the task closed.
func (c *Client) ListTaskBeadsForJiraKey(ctx context.Context, jiraKey string) ([]*BeadDetail, error) {
	if jiraKey == "" {
		return nil, nil
	}
	result, err := c.ListBeadsFiltered(ctx, ListBeadsQuery{
		Types:    []string{"task"},
		Statuses: allTaskStatuses,
		Fields:   map[string]string{"jira_key": jiraKey},
		Limit:    mrTaskLookupLimit,
	})
	if err != nil {
		return nil, fmt.Errorf("listing task beads for JIRA key %s: %w", jiraKey, err)
	}
	if result == nil {
		return nil, nil
	}
	return result.Beads, nil
}

// ListTaskBeadsForMR returns task beads in ALL statuses (including closed)
// whose fields.mr_url equals mrURL.
//
// This replaces the fetch-everything-then-filter-in-Go pattern in the GitLab
// note webhook path, which pulled the whole task corpus (10k rows of full
// BeadDetail out of 52k+ matches) on EVERY MR comment event and OOMKilled the
// 128Mi gitlab-bridge — while ALSO silently dropping matches past the cap
// (ka-0000007917). The equality predicate runs server-side, so the bridge
// receives only the rows it actually uses.
//
// Closed beads are retained deliberately: review comments legitimately arrive
// after the agent's task has closed.
func (c *Client) ListTaskBeadsForMR(ctx context.Context, mrURL string) ([]*BeadDetail, error) {
	if mrURL == "" {
		return nil, nil
	}
	result, err := c.ListBeadsFiltered(ctx, ListBeadsQuery{
		Types:    []string{"task"},
		Statuses: allTaskStatuses,
		Fields:   map[string]string{"mr_url": mrURL},
		Limit:    mrTaskLookupLimit,
	})
	if err != nil {
		return nil, fmt.Errorf("listing task beads for MR %s: %w", mrURL, err)
	}
	if result == nil {
		return nil, nil
	}
	return result.Beads, nil
}

// ListAssignedTask returns the first in-progress issue-kind bead claimed by
// agentName, or nil if none is found. Uses server-side kind=issue filtering
// to exclude infrastructure beads (agent, decision, project, etc.).
//
// Callers that must reason about EVERY claimed bead (rather than "is the agent
// holding anything at all?") want ListAssignedTasks — returning only the first
// of several claims silently hides the rest.
func (c *Client) ListAssignedTask(ctx context.Context, agentName string) (*BeadDetail, error) {
	tasks, err := c.ListAssignedTasks(ctx, agentName)
	if err != nil {
		return nil, err
	}
	if len(tasks) > 0 {
		return tasks[0], nil
	}
	return nil, nil
}

// ListAssignedTasks returns ALL in-progress issue-kind beads claimed by
// agentName (empty slice when none). Same server-side filter as
// ListAssignedTask — status=in_progress, assignee, kind=issue — but without
// the first-match truncation.
//
// The gb stop claimed-work gate needs the full set: it exempts a stop only
// when EVERY claimed bead is parked behind an open self-filed handle, so
// truncating to Beads[0] would let a genuinely abandoned second claim through
// whenever the first happened to be parked (ka-0000004133).
func (c *Client) ListAssignedTasks(ctx context.Context, agentName string) ([]*BeadDetail, error) {
	q := url.Values{}
	q.Set("status", "in_progress")
	q.Set("assignee", agentName)
	q.Set("kind", "issue")
	var resp listBeadsResponse
	if err := c.doJSON(ctx, http.MethodGet, "/v1/beads?"+q.Encode(), nil, &resp); err != nil {
		return nil, fmt.Errorf("listing assigned beads: %w", err)
	}
	tasks := make([]*BeadDetail, 0, len(resp.Beads))
	for _, b := range resp.Beads {
		tasks = append(tasks, b.toDetail())
	}
	return tasks, nil
}

// ResolveTicket looks up a bead by external ticket key (e.g., "PE-1234").
// It searches for beads with the label "jira:<key>". If no match is found
// by label, it falls back to a direct GetBead call (for gd-* bead IDs).
// Returns the matched bead detail or an error if the ticket cannot be resolved.
func (c *Client) ResolveTicket(ctx context.Context, ticketKey string) (*BeadDetail, error) {
	// For gd-* IDs, try a direct lookup first.
	if strings.HasPrefix(ticketKey, "gd-") || strings.HasPrefix(ticketKey, "ka-") {
		bead, err := c.GetBead(ctx, ticketKey)
		if err == nil && bead != nil && bead.ID != "" {
			return bead, nil
		}
	}

	// Search by jira label (e.g., "jira:PE-1234").
	result, err := c.ListBeadsFiltered(ctx, ListBeadsQuery{
		Labels: []string{"jira:" + ticketKey},
		Limit:  1,
	})
	if err != nil {
		return nil, fmt.Errorf("resolving ticket %q: %w", ticketKey, err)
	}
	if len(result.Beads) > 0 {
		return result.Beads[0], nil
	}

	return nil, fmt.Errorf("ticket %q not found", ticketKey)
}

// ListDecisionBeads queries the daemon for active decision beads (type=decision).
func (c *Client) ListDecisionBeads(ctx context.Context) ([]*BeadDetail, error) {
	resp, err := c.listBeads(ctx, []string{"decision"}, activeStatuses)
	if err != nil {
		return nil, fmt.Errorf("listing decision beads: %w", err)
	}
	beads := make([]*BeadDetail, 0, len(resp.Beads))
	for _, b := range resp.Beads {
		beads = append(beads, b.toDetail())
	}
	return beads, nil
}

// CreateBeadRequest contains the fields for creating a new bead.
type CreateBeadRequest struct {
	Title string `json:"title"`
	// ShortTitle is the <=40-rune name humans see for the bead (Slack prose,
	// the References footer, viewer lists). Empty means unset and is omitted,
	// so a daemon that predates the column never sees the key (gd-V5nGseRS5f).
	ShortTitle string `json:"short_title,omitempty"`
	// Heraldry is the bead's arms in any input form the daemon accepts (emoji,
	// placed, or canonical; heraldry spec §4.1). Empty means none and is
	// omitted, so a daemon that predates the column never sees the key.
	Heraldry    string          `json:"heraldry,omitempty"`
	Type        string          `json:"type"`
	Kind        string          `json:"kind,omitempty"`
	Description string          `json:"description,omitempty"`
	Assignee    string          `json:"assignee,omitempty"`
	Labels      []string        `json:"labels,omitempty"`
	Priority    int             `json:"priority,omitempty"`
	CreatedBy   string          `json:"created_by,omitempty"`
	Fields      json.RawMessage `json:"fields,omitempty"`
	// Status lets the caller create a bead in a non-open initial state
	// (typically "closed" for append-only ledger records like
	// trigger-audit events — ka-0000004100). Empty defaults to "open".
	Status string `json:"status,omitempty"`
	// Force bypasses content-shape guards on the server (currently the
	// decision ack-only / no-fork guard). Set via `gb decision create --force`.
	Force bool `json:"force,omitempty"`
	// IdempotencyKey, when set, makes the daemon do a lookup-or-return on
	// create instead of always inserting: the server returns any existing
	// non-deleted bead in the same project carrying the same key, and a
	// per-fleet partial unique index (kbeads migration 029) backstops the
	// concurrent-insert race so two racers converge on exactly ONE bead. This
	// is a process-independent dedup guard — it holds across separate binaries
	// (slack-bridge AND beads-viewer), unlike a process-local in-flight set.
	// See kbeads/internal/server/beads_idempotency.go (ka-0000002856).
	IdempotencyKey string `json:"idempotency_key,omitempty"`
	// ParentBeadID, when set, makes the daemon wire a parent-child dep from the
	// new bead to this one inside the create transaction and skip its
	// auto-parent step (D5, ka-0000007622). Create-time gates see the parent
	// only this way — e.g. BEADS_HERALDRY_GATE refusing an unarmed phase epic
	// of a plan (gd-kcCrpdrlm2); a dep added after the create is invisible to
	// them.
	ParentBeadID string `json:"parent_bead_id,omitempty"`
}

// CreateBead creates a new bead and returns its ID.
func (c *Client) CreateBead(ctx context.Context, req CreateBeadRequest) (string, error) {
	bead, err := c.CreateBeadDetail(ctx, req)
	if err != nil {
		return "", err
	}
	return bead.ID, nil
}

// CreateBeadDetail creates a bead and returns the full PERSISTED bead, not just
// its id. The daemon's create response carries the bead exactly as committed —
// labels read back within the create transaction (R2, ka-0000002898), fields,
// type — so a caller that must trust the persisted shape (e.g. the /plans
// invisible-orphan guard) can assert against this return value with no second
// GetBead round-trip. CreateBead delegates here and drops everything but the id
// for the many call sites that only need it.
func (c *Client) CreateBeadDetail(ctx context.Context, req CreateBeadRequest) (*BeadDetail, error) {
	// Canonical gardener-ownership stamp (persistent-agents Phase 1, task
	// ka-0000006626, design memo ka-0000001420 §2.3). Every agent bead gets a
	// canonical gardener_id AT SPAWN so the viewer's "Unowned" bucket is empty by
	// construction. This is the single create choke point every spawn path
	// funnels through — SpawnAgent AND the direct daemon.CreateBead(type=agent)
	// sites (thread cold-mint, pool prewarm, plan gardener, viewer spawns, thread
	// respawn) — so stamping here covers them all without touching each caller.
	// Soft, at-spawn only: it writes a field, never gates the create.
	req.Fields = stampAgentGardenerID(req.Type, req.Fields)
	// Short-title default (plan gd-YZATAIOhfW Task 6, D6). The same choke point
	// makes every in-repo creator — thread heads, plan steps, JIRA/GitLab
	// mention beads, decisions, escalations, agent beads — supply short_title,
	// so the daemon's enforce flip cannot strand one. A caller that set it
	// explicitly is never overridden; ungated types are left to the bridge.
	req.ShortTitle = defaultShortTitle(req)

	var bead beadJSON
	if err := c.doJSON(ctx, http.MethodPost, "/v1/beads", req, &bead); err != nil {
		return nil, fmt.Errorf("creating bead: %w", err)
	}
	return bead.toDetail(), nil
}

// defaultShortTitle returns req's short title: the caller's when it set one,
// else the agent handle or the de-noised title for a gated type, else "".
func defaultShortTitle(req CreateBeadRequest) string {
	if strings.TrimSpace(req.ShortTitle) != "" || !shorttitle.Gated(req.Type) {
		return req.ShortTitle
	}
	if req.Type == "agent" {
		return shorttitle.Handle(req.Title)
	}
	return shorttitle.Derive(req.Type, req.Title)
}

// SpawnAgent creates a new agent bead with the given name, project, and role.
// The bead starts in open status; the reconciler picks it up and schedules a pod.
// agentName is the agent identifier (e.g., "my-bot"). project is the project name
// (e.g., "gardenio"); if empty the daemon uses its default project.
// role is the agent role (e.g., "crew", "gardener"); if empty it defaults to "crew".
// taskID is an optional bead ID of a task to pre-assign this agent. When non-empty,
// the agent bead description is set to reference the task and a dependency is added
// (type "assigned") linking the agent bead to the task. The dependency is best-effort:
// if it fails the agent bead is still returned.
// customPrompt is an optional prompt string injected into the agent session at startup.
// When non-empty, it is stored in the bead's "prompt" field and passed as BOAT_PROMPT
// to the agent pod.
// extraFields is an optional map of additional fields to include in the agent bead
// (e.g., schedule metadata). These are merged after the standard fields.
//
// The returned ID may be an EXISTING agent rather than a newly-created one: both
// the name-dedup and single-claim guards below return an incumbent instead of
// creating a duplicate. This signature cannot express that difference — callers
// that surface the spawn to a human or a script should use SpawnAgentResult,
// which reports whether the agent was created or already existed.
func (c *Client) SpawnAgent(ctx context.Context, agentName, project, taskID, role, customPrompt string, extraFields ...map[string]string) (string, error) {
	res, err := c.SpawnAgentResult(ctx, agentName, project, taskID, role, customPrompt, extraFields...)
	if err != nil {
		return "", err
	}
	return res.BeadID, nil
}

// SpawnOutcome reports HOW a spawn resolved: whether a new agent bead was
// actually created, or an existing agent was returned by one of the two
// idempotence guards.
//
// This is the machine-readable half of the spawn contract. SpawnAgent returns
// only (beadID, error), which makes a fresh spawn and a guard-deduped no-op
// indistinguishable to a caller — a scripted gardener that dispatches crew has
// no way to tell "I created this agent" from "an agent was already on it", so
// it cannot report or branch on the difference (bug ka-0000001243).
type SpawnOutcome string

const (
	// SpawnCreated means a new agent bead was created by this call.
	SpawnCreated SpawnOutcome = "created"
	// SpawnExisting means no bead was created — an existing live agent was
	// returned by the name-dedup or single-claim guard. See SpawnResult.Reason.
	SpawnExisting SpawnOutcome = "existing"
)

// SpawnResult is the full result of a spawn attempt: the agent bead ID plus
// how it was resolved.
type SpawnResult struct {
	// BeadID is the agent bead — newly created, or the existing one a guard returned.
	BeadID string
	// AgentName is the name of the agent BeadID refers to. On SpawnExisting this
	// is the INCUMBENT's name, which differs from the name the caller requested:
	// the single-claim guard matches on task_id, so a caller that auto-generated
	// a fresh random name gets back the live agent that already holds the task.
	// Reporting the requested name there would name an agent that does not exist.
	AgentName string
	// Outcome is SpawnCreated or SpawnExisting.
	Outcome SpawnOutcome
	// Reason names the guard that returned an existing agent, for operator-facing
	// output: "name_dedup" (an active agent already has this name) or
	// "single_claim" (a live agent already holds this task). Empty on SpawnCreated.
	Reason string
}

// SpawnAgentResult is SpawnAgent with the resolution outcome reported. Prefer it
// over SpawnAgent wherever the caller surfaces the spawn to a human or a script
// and must not report a deduped no-op as a fresh dispatch.
func (c *Client) SpawnAgentResult(ctx context.Context, agentName, project, taskID, role, customPrompt string, extraFields ...map[string]string) (SpawnResult, error) {
	// Dedup guard: check if an active agent bead with this name already exists.
	// If so, return the existing bead ID to prevent orphaned duplicates.
	if existingID, err := c.findActiveAgentByName(ctx, agentName); err != nil {
		// List failed — log and proceed with creation (best-effort dedup).
		slog.Warn("SpawnAgent: dedup check failed, proceeding with creation",
			"agent", agentName, "error", err)
	} else if existingID != "" {
		slog.Info("SpawnAgent: returning existing active agent bead (dedup)",
			"agent", agentName, "existing_bead", existingID)
		// This guard matches BY name, so the incumbent's name is agentName.
		return SpawnResult{BeadID: existingID, AgentName: agentName, Outcome: SpawnExisting, Reason: "name_dedup"}, nil
	}

	// Single-claim guard (ka-0000004654): when the caller opts in via the
	// `single_claim_task` extra field AND a task is targeted, refuse to spawn a
	// SECOND crew for a task a live agent is already working. The name-dedup
	// above does not catch this: two gardener fires (e.g. a multi-source cron +
	// bead_event gardener) generate DIFFERENT random-suffixed agent names yet
	// target the SAME task bead, so both pass the name check and race to claim
	// the same work — the duplicate `terraform apply` collision this bug
	// describes. Keying the guard on task_id closes that window.
	//
	// Opt-in (not blanket) because fan-out gardeners intentionally put many crew
	// on sibling subtree children, and AssignBead-pinned gardeners reuse one
	// long-lived task across runs under their own one_spawn_at_a_time gate;
	// those paths must keep spawning. Single-claim work steps set the flag.
	//
	// Best-effort + fail-open: a query error logs and proceeds with creation,
	// matching the name-dedup posture so a transient daemon hiccup never wedges
	// legitimate spawns. The flag is consumed here and not persisted on the bead.
	if taskID != "" && extraFieldTrue(extraFields, "single_claim_task") {
		if existingID, existingName, err := c.findLiveAgentForTask(ctx, taskID); err != nil {
			slog.Warn("SpawnAgent: single-claim check failed, proceeding with creation",
				"agent", agentName, "task", taskID, "error", err)
		} else if existingID != "" {
			slog.Info("SpawnAgent: task already claimed by a live agent, returning existing (single-claim)",
				"agent", agentName, "task", taskID, "existing_bead", existingID,
				"existing_agent", existingName)
			// Report the INCUMBENT's name: agentName here is a freshly-generated
			// name for an agent that was never created, so returning it would name
			// a nonexistent agent.
			if existingName == "" {
				existingName = agentName
			}
			return SpawnResult{BeadID: existingID, AgentName: existingName, Outcome: SpawnExisting, Reason: "single_claim"}, nil
		}
	}

	// Default empty role to "crew" as documented in the function contract.
	// The downstream ListAgentBeads and SSE subscriber also default empty
	// roles, but fixing at the source prevents storing empty role fields.
	if role == "" {
		role = "crew"
	}
	fields := map[string]string{
		"agent":   agentName,
		"mode":    "crew",
		"role":    role,
		"project": project,
	}
	if customPrompt != "" {
		fields["prompt"] = customPrompt
	}
	if taskID != "" {
		fields["task_id"] = taskID
	}
	for _, extra := range extraFields {
		for k, v := range extra {
			// single_claim_task is a spawn-time control flag consumed by the
			// single-claim guard above, not bead data — don't persist it.
			if k == "single_claim_task" {
				continue
			}
			fields[k] = v
		}
	}
	markClaimTaskAtSpawn(fields, taskID, extraFields)
	// Task-shared succession (ka-0000007863): when this spawn targets a task a
	// CLOSED agent already worked, stamp prior_agent_id + lineage_root_id from
	// that predecessor so `gb prime` can hand the newcomer its predecessor's
	// handoff instead of dropping them into a room 56 agents have already been
	// through. Runs AFTER the single-claim guard (a LIVE incumbent short-
	// circuits above and never reaches here) and BEFORE marshalling, so it
	// covers every re-dispatch vector — gardener fan-out, formula step dispatch,
	// gitlab/jira resolvers, `gb spawn --task` — without touching any of them.
	// No-op when the caller already set prior_agent_id (successor/gardener/thread
	// paths know their own predecessor) or when the task has no prior occupant.
	c.stampTaskPredecessorLineage(ctx, taskID, fields)

	// Seat key (ka-0000007094): record the position this agent occupies —
	// (project, role, name-with-suffix-stripped) — so a recurring name becomes a
	// derivable fact instead of a coincidence visible only by eyeballing the
	// roster. 353 name bases were re-occupied over 7 days and 318 of them
	// carried no chain at all; the name was already the unit of persistence
	// (design memo ka-0000001420 §2.1), it just was never written down. Purely
	// additive: nothing reads it to gate a spawn.
	if seat := SeatKey(agentName, project, role); seat != "" && fields["seat_key"] == "" {
		fields["seat_key"] = seat
	}

	fieldsJSON, err := json.Marshal(fields)
	if err != nil {
		return SpawnResult{}, fmt.Errorf("marshalling agent fields: %w", err)
	}
	req := CreateBeadRequest{
		Title:      agentName,
		ShortTitle: shorttitle.Handle(agentName),
		Type:       "agent",
		Fields:     json.RawMessage(fieldsJSON),
	}
	if taskID != "" {
		req.Description = "Assigned to task: " + taskID
	} else if customPrompt != "" {
		req.Description = customPrompt
	}
	id, err := c.CreateBead(ctx, req)
	if err != nil {
		return SpawnResult{}, fmt.Errorf("spawning agent %q: %w", agentName, err)
	}
	if project != "" {
		// Best-effort: label the agent bead with its project so it appears in
		// project-scoped listings (kd list, gb ready with --project filter).
		if err := c.AddLabel(ctx, id, "project:"+project); err != nil {
			slog.Warn("failed to add project label to agent bead",
				"agent", agentName, "bead", id, "project", project, "error", err)
		}
	}
	// Best-effort: add role label(s) so gb prime advice matching can filter by role.
	// Supports comma-separated roles (e.g. "thread,crew").
	for _, r := range strings.Split(role, ",") {
		r = strings.TrimSpace(r)
		if r == "" {
			continue
		}
		if err := c.AddLabel(ctx, id, "role:"+r); err != nil {
			slog.Warn("failed to add role label to agent bead",
				"agent", agentName, "bead", id, "role", r, "error", err)
		}
	}
	if taskID != "" {
		// Best-effort: failure to link the task does not prevent agent creation.
		// The task reference is already captured in the bead description.
		if err := c.AddDependency(ctx, id, taskID, "assigned", agentName); err != nil {
			slog.Warn("failed to add task dependency to agent bead",
				"agent", agentName, "bead", id, "task", taskID, "error", err)
		}
		// Synchronous pre-claim (ka-0000002756): stamp the TARGET task bead
		// in_progress + assignee=agentName at spawn time, closing the ~3-minute
		// window where the bead sat open + unassigned between spawn and the crew's
		// own boot `kd claim`. During that window the bead is indistinguishable
		// from un-crewed work: `gb ready` filters only on status=open with no
		// assignee predicate (buildReadyQuery / no_open_deps), so an open bead
		// stays in the ready set until its STATUS leaves "open" — not when an
		// assignee is set. Two gardener lanes firing on overlapping cadences both
		// legitimately select it and double-dispatch an Opus crew onto one bug.
		//
		// This is the preventive half of the single-claim contract whose rejecting
		// half is findLiveAgentForTask above: gated on the SAME single_claim_task
		// opt-in so only single-claim dispatches (CLI `gb spawn --task`, the
		// trigger engine's per-fire spawn, formula step dispatch) pre-claim, while
		// fan-out gardeners and AssignBead-pinned gardeners — which intentionally put
		// many crew on sibling children or reuse one long-lived task — are
		// untouched. It writes exactly what `kd claim` writes (status=in_progress,
		// assignee=agentName == the crew's KD_ACTOR), so the crew's boot claim is
		// idempotent (kd claim's checkClaimable permits assignee==actor).
		//
		// The stamp is unconditional (no read-back guard) by design. The three
		// assignee states it can meet are all handled correctly:
		//   - Unclaimed (the overwhelming common case): the bead is stamped and
		//     leaves the ready queue — the fix.
		//   - Held by a LIVE agent: unreachable here. findLiveAgentForTask above
		//     already returned that incumbent BEFORE create, so a second spawn
		//     never reaches this line — a live claim is never clobbered.
		//   - Held by a TERMINAL/dead agent (the reclaim-after-crash case): the
		//     single-claim guard treats a done/failed incumbent as gone and lets a
		//     fresh crew spawn, so overwriting the stale assignee with the new
		//     crew is exactly what reclaim wants — a no-clobber read guard would
		//     instead wrongly leave the dead assignee stranded on the bead.
		//
		// Best-effort + fail-open, matching every other post-create step here: a
		// stamp failure logs and still returns the agent. The recovery path for a
		// spawn that pre-claims but whose pod never schedules (or dies mid-boot) is
		// already built — the stale-reaper's reopenStrandedIssueTasks /
		// clearStrandedOpenAssignees reopen a target whose assignee agent is gone —
		// so a failed spawn self-heals back into `gb ready` without a bespoke
		// self-expiring reservation field.
		if extraFieldTrue(extraFields, "single_claim_task") {
			inProgress := "in_progress"
			assignee := agentName
			if err := c.UpdateBead(ctx, taskID, UpdateBeadRequest{
				Status:   &inProgress,
				Assignee: &assignee,
			}); err != nil {
				slog.Warn("failed to pre-claim target task bead at spawn",
					"agent", agentName, "bead", id, "task", taskID, "error", err)
			}
		}
	}
	return SpawnResult{BeadID: id, AgentName: agentName, Outcome: SpawnCreated}, nil
}

// BeadDetail represents a full bead returned by the daemon.
type BeadDetail struct {
	ID         string `json:"id"`
	Slug       string `json:"slug,omitempty"`
	ShortTitle string `json:"short_title,omitempty"`
	// Heraldry is the bead's arms in the canonical stored form ("blue 🌱 . 🔥
	// . 📦", heraldry spec §4.1), "" for none. Opaque to the controller and
	// never shown: a surface prints HeraldryRun of it.
	Heraldry          string            `json:"heraldry,omitempty"`
	Title             string            `json:"title"`
	Kind              string            `json:"kind"`
	Type              string            `json:"type"`
	Status            string            `json:"status"`
	Assignee          string            `json:"assignee"`
	Owner             string            `json:"owner,omitempty"`
	Priority          int               `json:"priority"`
	Labels            []string          `json:"labels"`
	Notes             string            `json:"notes"`
	Fields            map[string]string `json:"fields"`
	Description       string            `json:"description"`
	CreatedBy         string            `json:"created_by"`
	ClosedBy          string            `json:"closed_by,omitempty"`
	OperatedBy        string            `json:"operated_by,omitempty"`
	OperatedByDisplay string            `json:"operated_by_display,omitempty"`
	OperatedByEmail   string            `json:"operated_by_email,omitempty"`
	// OperatedBySlug is the viewer-resolved canonical per-user-homepage
	// slug for the bead's operator (per-user-homepage M2 §6 /
	// ka-0000007787). The daemon does NOT populate this — beads-viewer
	// fills it in at handler time via a per-server slug cache backed by
	// the GetOperatorSlug daemon endpoint, so that the operator chip
	// rendering layer can emit `<a href="/u/{slug}">` links instead of
	// the legacy `/search?operated_by=…` filter href. Empty when the
	// operator has no resolvable slug (anonymous, agent, scheduler) or
	// when the slug lookup failed.
	OperatedBySlug string    `json:"-"`
	Fleet          string    `json:"fleet,omitempty"`
	DueAt          string    `json:"due_at,omitempty"`
	DeferUntil     string    `json:"defer_until,omitempty"`
	ClosedAt       string    `json:"closed_at,omitempty"`
	CreatedAt      time.Time `json:"created_at,omitempty"`
	UpdatedAt      time.Time `json:"updated_at,omitempty"`
	// Category is the materialized inbox-lens classification (intake /
	// intermediate / action / config) — populated by the daemon's BEFORE
	// trigger on writes (migration 020) and surfaced in the JSON response.
	Category string `json:"category,omitempty"`
	// OperatedByID is the operators(id) FK for human attribution (post-
	// migration 016 dual-write window). nil for legacy/anonymous rows.
	OperatedByID *int64 `json:"operated_by_id,omitempty"`
	// CreatedByOperatorID is the immortal originator FK — set once at
	// raise-time, distinct from OperatedByID which flips on dibs handoffs.
	CreatedByOperatorID *int64 `json:"created_by_operator_id,omitempty"`
	// HasReport is the daemon-computed predicate (R1, ka-0000007923): true
	// when this bead has a direct parent-child child whose type is one of
	// {report, rich-report, bundle} and is not deleted.
	HasReport bool `json:"has_report,omitempty"`
	// BlockedOnDecision is the daemon-computed predicate (R2, ka-0000001065):
	// true when at least one open type=decision bead is reachable via
	// parent-child (depth ≤ 2) from this bead, OR a type=agent bead under
	// this bead carries fields.decision_bead_id pointing at an open decision.
	BlockedOnDecision bool `json:"blocked_on_decision,omitempty"`
	// BlockedOnDecisionBeadID is the ka-id of the open decision that flagged
	// the row (lex-first when multiple match). Empty when BlockedOnDecision
	// is false.
	BlockedOnDecisionBeadID string `json:"blocked_on_decision_bead_id,omitempty"`
	// ParkedOnDecision is the daemon-computed TEXT-MENTION predicate
	// (ka-0000003797): true when at least one open type=decision bead names this
	// bead's id in its prose (title/description/notes/comment), via a
	// bead_references row. Distinct from BlockedOnDecision, which is edge-based —
	// a prose-only mention carries no blocking dep edge, so the bead stays in
	// `gb ready`. This is the signal that lets `gb ready` / `gb news` mark a row
	// PARKED behind an open operator decision. Requires WithParkedOnDecision.
	ParkedOnDecision bool `json:"parked_on_decision,omitempty"`
	// ParkedOnDecisionBeadID is the ka-id of the open decision that parks the
	// row (lex-first when multiple mention it). Empty when ParkedOnDecision is
	// false.
	ParkedOnDecisionBeadID string `json:"parked_on_decision_bead_id,omitempty"`
	// ParentID is the immediate parent-child parent (epic/container) id, set by
	// the daemon ONLY when the caller opts into the projection (ListBeadsQuery.
	// WithParent / `gb ready`). Empty means genuine orphan, not-opted-in, or an
	// old daemon — a dispatcher reasoning over `gb ready --json` reads a
	// non-empty value as "leaf under this epic" and empty as "floating"
	// (ka-0000006083). Never an N+1 kd show — the daemon batches it per page.
	ParentID string `json:"parent_id,omitempty"`
	// ParentTitle is ParentID's title when the parent is visible under the
	// caller's project scope; empty (with ParentID still set) when the parent is
	// out of scope or soft-deleted. Convenience label; ParentID is load-bearing.
	ParentTitle string `json:"parent_title,omitempty"`
	// SpawnLineageContent and SpawnLineageDigest are set by the daemon on a
	// trigger/schedule bead only: the spawn-relevant content a spawn-lineage seal
	// would cover and its sha256. `gb trigger reseal` shows the content and pins
	// the digest on the write. Empty on other bead types and on an old daemon.
	SpawnLineageContent map[string]any `json:"spawn_lineage_content,omitempty"`
	SpawnLineageDigest  string         `json:"spawn_lineage_digest,omitempty"`
}

// GetBead fetches a single bead by ID from the daemon. When a request-scoped
// memo is armed (BeginGetBeadCache), a repeated id is served from the memo
// instead of re-issuing the round-trip; errors are never cached.
func (c *Client) GetBead(ctx context.Context, beadID string) (*BeadDetail, error) {
	if b, ok := c.cachedBead(beadID); ok {
		return b, nil
	}
	var bead beadJSON
	if err := c.doJSON(ctx, http.MethodGet, "/v1/beads/"+url.PathEscape(beadID), nil, &bead); err != nil {
		return nil, fmt.Errorf("getting bead %s: %w", beadID, err)
	}
	detail := bead.toDetail()
	c.storeBead(beadID, detail)
	return detail, nil
}

// cachedBead returns a memoized bead when the request-scoped cache is armed and
// holds beadID. ok=false means either no armed cache or a cache miss.
func (c *Client) cachedBead(beadID string) (*BeadDetail, bool) {
	c.getBeadCacheMu.Lock()
	defer c.getBeadCacheMu.Unlock()
	if c.getBeadCache == nil {
		return nil, false
	}
	b, ok := c.getBeadCache[beadID]
	return b, ok
}

// storeBead records a successful fetch in the request-scoped cache when armed.
// A no-op when the cache is disarmed.
func (c *Client) storeBead(beadID string, detail *BeadDetail) {
	c.getBeadCacheMu.Lock()
	defer c.getBeadCacheMu.Unlock()
	if c.getBeadCache != nil {
		c.getBeadCache[beadID] = detail
	}
}

// BeadTaint is one row of GET /v1/beads/{id}/taints (docs/adr/taints.md
// §9.1 bead-page badges): a taint applied to this bead, with the
// propagation source and, for an inherited taint, the bead it was
// inherited from.
type BeadTaint struct {
	BeadID       string    `json:"bead_id"`
	TaintID      int64     `json:"taint_id"`
	TaintName    string    `json:"taint_name"`
	Source       string    `json:"source"`
	SourceBeadID string    `json:"source_bead_id,omitempty"`
	AppliedBy    string    `json:"applied_by"`
	AppliedAt    time.Time `json:"applied_at"`
}

// GetBeadTaints calls GET /v1/beads/{id}/taints, returning every taint
// applied to beadID that the caller may read (the same gate as GetBead).
func (c *Client) GetBeadTaints(ctx context.Context, beadID string) ([]BeadTaint, error) {
	var resp struct {
		Taints []BeadTaint `json:"taints"`
	}
	if err := c.doJSON(ctx, http.MethodGet, "/v1/beads/"+url.PathEscape(beadID)+"/taints", nil, &resp); err != nil {
		return nil, fmt.Errorf("getting taints for bead %s: %w", beadID, err)
	}
	return resp.Taints, nil
}

// TaintRegistryEntry is one row of GET /v1/taints — the taint registry
// visible to every viewer (docs/adr/taints.md §9.1 "/taints"). OwnerTeam is
// always populated regardless of the caller's role; ScrubPolicy is omitted
// here because it is only returned to the owner team or an admin and this
// client has no use for it.
//
// BeadCount/OpenScrubRequests/LastScrubAt are the live registry-page figures
// task gd-QXSHroFXwB adds, computed by the daemon fresh on every call under
// the caller's own read scope. BeadCount is nil — rendered as a dash by the
// /taints page — when the caller holds no grant for this taint at all,
// distinguishing "denied" from a genuine zero.
type TaintRegistryEntry struct {
	ID                int64      `json:"id"`
	Name              string     `json:"name"`
	Description       string     `json:"description,omitempty"`
	OwnerTeam         string     `json:"owner_team"`
	RetiredAt         *time.Time `json:"retired_at,omitempty"`
	BeadCount         *int       `json:"bead_count"`
	OpenScrubRequests int        `json:"open_scrub_requests"`
	LastScrubAt       *time.Time `json:"last_scrub_at,omitempty"`
	// CanApply reports whether the caller holds this taint's grant — task
	// gd-nOnc1Q8Zcv's registry-row "Apply taint" shortcut.
	CanApply bool `json:"can_apply"`
}

// ListTaints calls GET /v1/taints, returning the full taint registry.
func (c *Client) ListTaints(ctx context.Context) ([]TaintRegistryEntry, error) {
	var resp struct {
		Taints []TaintRegistryEntry `json:"taints"`
	}
	if err := c.doJSON(ctx, http.MethodGet, "/v1/taints", nil, &resp); err != nil {
		return nil, fmt.Errorf("listing taints: %w", err)
	}
	return resp.Taints, nil
}

// CreateTaintRequest is the body for POST /v1/taints. Mirrors the daemon's
// CreateTaintRequest (kbeads/internal/server/http_taints.go), but the
// viewer's "New taint" admin action (docs/adr/taints.md §9.1, task
// gd-QXSHroFXwB) only ever offers Name, OwnerTeam and a preset — ScrubPolicy
// and Effects stay CLI-only, so this client-side request has no fields for
// them.
type CreateTaintRequest struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	OwnerTeam   string `json:"owner_team"`
	Preset      string `json:"preset,omitempty"`
}

// CreatedTaint is the daemon's response body for a successful POST
// /v1/taints.
type CreatedTaint struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	OwnerTeam string    `json:"owner_team"`
	CreatedAt time.Time `json:"created_at"`
}

// ErrTaintCreateRequiresUserBearer is returned by CreateTaint when ctx
// carries no per-request user bearer (WithUserBearer). Taint creation always
// authenticates as the calling human, regardless of
// SetRequireUserBearerForWrites — see CreateTaint's doc comment — so a
// caller with no forwardable OIDC token cannot make this call at all.
var ErrTaintCreateRequiresUserBearer = errors.New("beadsapi: taint creation requires a per-request user bearer (none attached to this context)")

// RevokeRunner revokes every token of a laptop runner and tells its agents to
// stop (kbeads POST /v1/runners/{id}/revoke, spec §4.2 step 4).
func (c *Client) RevokeRunner(ctx context.Context, runnerID string) (revokedTokens, agentsKilled int, err error) {
	var out struct {
		RevokedTokens int `json:"revoked_tokens"`
		AgentsKilled  int `json:"agents_killed"`
	}
	if err := c.doJSON(ctx, http.MethodPost, "/v1/runners/"+url.PathEscape(runnerID)+"/revoke", map[string]any{}, &out); err != nil {
		return 0, 0, err
	}
	return out.RevokedTokens, out.AgentsKilled, nil
}

// CreateTaint calls POST /v1/taints AS THE CALLING HUMAN, always forcing the
// per-request user bearer (WithUserBearer) onto the wire rather than the
// client's usual static token + X-Operated-By relay.
//
// This is a deliberate exception to how every other admin viewer action
// authenticates. The daemon's callerMayCreateTaint check
// (kbeads/internal/server/http_taints.go) requires DIRECT OIDC
// authentication with RoleAdmin: by design it does not honor a relayed
// X-Operated-By identity, unlike the rest of the admin surface, because
// OwnerTeam is caller-supplied on this very request and creating a taint
// implicitly grants its owner team full read access with no further check.
// So this call cannot fall back to the static token the way SetTaint's
// sibling admin actions do — it fails closed with
// ErrTaintCreateRequiresUserBearer when ctx carries no user bearer (e.g. an
// AVA-routed edge with no forwardable raw bearer), rather than sending a
// request the daemon would reject anyway with a far less actionable 403.
func (c *Client) CreateTaint(ctx context.Context, req CreateTaintRequest) (*CreatedTaint, error) {
	bearer := UserBearerFromContext(ctx)
	if bearer == "" {
		return nil, ErrTaintCreateRequiresUserBearer
	}
	var result CreatedTaint
	if err := c.doJSONAsUser(ctx, http.MethodPost, "/v1/taints", bearer, req, &result); err != nil {
		return nil, fmt.Errorf("creating taint %q: %w", req.Name, err)
	}
	return &result, nil
}

// CreateScrubRequestInput is the request body for POST /v1/scrub-requests
// (docs/adr/taints.md §6.2/§7, kbeads http_scrub_requests_create.go). Scope
// must resolve to exactly one of the daemon's two accepted shapes: an
// explicit bead-id list (`{"bead_ids": [...]}`) or an age/label query
// (`{"older_than_days": N, "labels_any": [...]}`). There is no mode field —
// mode is policy-derived from the taint's scrub_policy, never caller-set.
//
// Used by both the /me/taints "request detaint for all of mine" action and
// the bead-page / taint-detail-page "Request scrub" action (task
// gd-nOnc1Q8Zcv) — both are ordinary bead-creation writes over the standard
// static-token + X-Operated-By relay.
type CreateScrubRequestInput struct {
	Taint string
	Kind  string // "detaint" or "purge"
	// BeadIDs, when non-empty, is marshaled as the scope's "bead_ids" array —
	// the shape the /me/taints page uses to scope a scrub request to exactly
	// the viewer's own beads carrying the taint.
	BeadIDs []string
}

// CreateScrubRequestResult is the response body for POST /v1/scrub-requests:
// the created scrub-request bead plus, for a kind=detaint request, the named
// taint's current detaint_mode (read fresh off the taint, not stored on the
// bead).
type CreateScrubRequestResult struct {
	Bead        *BeadDetail `json:"-"`
	ResolvedIDs []string    `json:"-"`
	DetaintMode string      `json:"detaint_mode,omitempty"`
}

// CreateScrubRequest calls POST /v1/scrub-requests, filing one scrub request
// for the given taint scoped to BeadIDs. The created bead's Fields carry
// resolved_bead_ids (applyScrubRequestGuard, kbeads scrub_request.go).
func (c *Client) CreateScrubRequest(ctx context.Context, in CreateScrubRequestInput) (*CreateScrubRequestResult, error) {
	scope, err := json.Marshal(struct {
		BeadIDs []string `json:"bead_ids"`
	}{BeadIDs: in.BeadIDs})
	if err != nil {
		return nil, fmt.Errorf("encoding scrub request scope: %w", err)
	}
	body := struct {
		Taint string          `json:"taint"`
		Kind  string          `json:"kind"`
		Scope json.RawMessage `json:"scope"`
	}{Taint: in.Taint, Kind: in.Kind, Scope: scope}

	var resp struct {
		beadJSON
		DetaintMode string `json:"detaint_mode,omitempty"`
	}
	if err := c.doJSON(ctx, http.MethodPost, "/v1/scrub-requests", body, &resp); err != nil {
		return nil, fmt.Errorf("creating scrub request for taint %s: %w", in.Taint, err)
	}
	bead := resp.beadJSON.toDetail()
	var resolved []string
	if raw, ok := bead.Fields["resolved_bead_ids"]; ok && raw != "" {
		_ = json.Unmarshal([]byte(raw), &resolved)
	}
	return &CreateScrubRequestResult{Bead: bead, ResolvedIDs: resolved, DetaintMode: resp.DetaintMode}, nil
}

// TaintDetail is the response shape of GET /v1/taints/{name} (docs/adr/
// taints.md §9.1, task gd-nOnc1Q8Zcv): the taint detail page's Policy tab
// source of truth. A caller who is neither the taint's owner team nor an
// admin gets a restricted response — only Name/Description/CanApply are
// populated, and Restricted is true, so the Policy tab can render "you
// cannot see this taint's policy" instead of an empty form.
type TaintDetail struct {
	ID          int64           `json:"id"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	OwnerTeam   string          `json:"owner_team"`
	ScrubPolicy json.RawMessage `json:"scrub_policy"`
	Effects     []string        `json:"effects"`
	CreatedAt   time.Time       `json:"created_at"`
	UpdatedAt   time.Time       `json:"updated_at"`
	RetiredAt   *time.Time      `json:"retired_at,omitempty"`
	// CanApply reports whether the caller holds this taint's grant (owner
	// team, admin, or an explicit taint_grants row) — the bead-page and
	// detail-page "Apply taint" action visibility (task gd-nOnc1Q8Zcv
	// acceptance criteria: "visible only to grant holders").
	CanApply bool `json:"can_apply"`
	// Restricted is true when the daemon returned the name/description/
	// can_apply-only view (caller is neither the owner team nor an admin).
	// OwnerTeam/ScrubPolicy/Effects/timestamps are zero-valued in that case.
	Restricted bool `json:"-"`
}

// GetTaint calls GET /v1/taints/{name} — the taint detail page's Policy tab.
func (c *Client) GetTaint(ctx context.Context, name string) (*TaintDetail, error) {
	var raw map[string]json.RawMessage
	if err := c.doJSON(ctx, http.MethodGet, "/v1/taints/"+url.PathEscape(name), nil, &raw); err != nil {
		return nil, fmt.Errorf("getting taint %q: %w", name, err)
	}
	det := &TaintDetail{}
	if _, ok := raw["owner_team"]; !ok {
		det.Restricted = true
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return nil, fmt.Errorf("re-encoding taint %q response: %w", name, err)
	}
	if err := json.Unmarshal(data, det); err != nil {
		return nil, fmt.Errorf("decoding taint %q response: %w", name, err)
	}
	return det, nil
}

// ErrTaintPolicyUpdateRequiresUserBearer is returned by UpdateTaintPolicy
// when ctx carries no per-request user bearer (WithUserBearer).
//
// Mirrors ErrTaintCreateRequiresUserBearer / CreateTaint: the daemon's
// callerMayManageTaintPolicy check (kbeads/internal/server/
// http_taint_grants.go) requires DIRECT OIDC authentication and does NOT
// honor a relayed X-Operated-By identity — unlike callerMayManageTaint,
// which the Grants tab's GrantTaint/RevokeTaint still use, and which DOES
// honor the relay. Operator review 2026-09-23 on PR #149: "on main only a
// direct OIDC admin sets scrub policy; do not let the static-token relay
// weaken that." So this call always forces the calling human's own OIDC
// bearer onto the wire via doJSONAsUser, failing closed with this error when
// the caller has no forwardable bearer, rather than sending a request the
// daemon would reject anyway with a far less actionable 403.
var ErrTaintPolicyUpdateRequiresUserBearer = errors.New("beadsapi: taint policy update requires a per-request user bearer (none attached to this context)")

// UpdateTaintPolicy calls PATCH /v1/taints/{name} — the Policy tab's save
// action, ALWAYS forcing the per-request user bearer onto the wire — see
// ErrTaintPolicyUpdateRequiresUserBearer's doc comment for why.
func (c *Client) UpdateTaintPolicy(ctx context.Context, name string, scrubPolicy json.RawMessage) (*TaintDetail, error) {
	bearer := UserBearerFromContext(ctx)
	if bearer == "" {
		return nil, ErrTaintPolicyUpdateRequiresUserBearer
	}
	req := struct {
		ScrubPolicy json.RawMessage `json:"scrub_policy"`
	}{ScrubPolicy: scrubPolicy}
	var det TaintDetail
	if err := c.doJSONAsUser(ctx, http.MethodPatch, "/v1/taints/"+url.PathEscape(name), bearer, req, &det); err != nil {
		return nil, fmt.Errorf("updating policy for taint %q: %w", name, err)
	}
	return &det, nil
}

// TaintGrantEntry is one row of GET /v1/taints/{name}/grants — the Grants
// tab (docs/adr/taints.md §9.1). Principal is the bare stored value (a team
// bead id, an operators.id rendered as text, an agent bead id, or a fleet
// name) — never the combined "<kind>:<value>" form.
type TaintGrantEntry struct {
	TaintID       int64      `json:"taint_id"`
	PrincipalKind string     `json:"principal_kind"`
	Principal     string     `json:"principal"`
	GrantedBy     int64      `json:"granted_by"`
	GrantedAt     time.Time  `json:"granted_at"`
	ExpiresAt     *time.Time `json:"expires_at,omitempty"`
}

// ListTaintGrants calls GET /v1/taints/{name}/grants. Gated daemon-side by
// canSeeTaintDetail (owner team or admin); returns every grant, including
// expired ones — the Grants tab renders expiry itself.
func (c *Client) ListTaintGrants(ctx context.Context, name string) ([]TaintGrantEntry, error) {
	var resp struct {
		Grants []TaintGrantEntry `json:"grants"`
	}
	if err := c.doJSON(ctx, http.MethodGet, "/v1/taints/"+url.PathEscape(name)+"/grants", nil, &resp); err != nil {
		return nil, fmt.Errorf("listing grants for taint %q: %w", name, err)
	}
	return resp.Grants, nil
}

// GrantTaintRequest is the body for POST /v1/taints/{name}/grants.
// Principal is the combined "<kind>:<value>" form the daemon resolves
// (e.g. "team:gd-acme", "operator:someone@example.com"). Until is ignored
// for an agent principal (always bound to the agent bead's close, ADR §5.2).
type GrantTaintRequest struct {
	Principal string     `json:"principal"`
	Until     *time.Time `json:"until,omitempty"`
}

// GrantTaint calls POST /v1/taints/{name}/grants — the Grants tab's grant
// action. Gated daemon-side by callerMayManageTaint (owner team or admin);
// granted_by is always server-derived.
func (c *Client) GrantTaint(ctx context.Context, name string, req GrantTaintRequest) (*TaintGrantEntry, error) {
	var grant TaintGrantEntry
	if err := c.doJSON(ctx, http.MethodPost, "/v1/taints/"+url.PathEscape(name)+"/grants", req, &grant); err != nil {
		return nil, fmt.Errorf("granting taint %q to %q: %w", name, req.Principal, err)
	}
	return &grant, nil
}

// RevokeTaint calls DELETE /v1/taints/{name}/grants/{principal} — the
// Grants tab's revoke action. principal is the combined "<kind>:<value>"
// form. Idempotent: revoking a grant that does not exist is not an error
// (removed reports false).
func (c *Client) RevokeTaint(ctx context.Context, name, principal string) (bool, error) {
	var resp struct {
		Removed bool `json:"removed"`
	}
	path := "/v1/taints/" + url.PathEscape(name) + "/grants/" + url.PathEscape(principal)
	if err := c.doJSON(ctx, http.MethodDelete, path, nil, &resp); err != nil {
		return false, fmt.Errorf("revoking taint %q from %q: %w", name, principal, err)
	}
	return resp.Removed, nil
}

// ApplyTaint calls POST /v1/beads/{id}/taints — the bead-page and
// taint-detail-page "Apply taint" action (task gd-nOnc1Q8Zcv acceptance
// criteria). Gated daemon-side by callerHoldsTaintGrant; applied_by is
// always server-derived. Idempotent: applying an already-applied taint
// returns the original applied_by/applied_at/source.
func (c *Client) ApplyTaint(ctx context.Context, beadID, taintName string) (*BeadTaint, error) {
	req := struct {
		Name string `json:"name"`
	}{Name: taintName}
	var bt BeadTaint
	if err := c.doJSON(ctx, http.MethodPost, "/v1/beads/"+url.PathEscape(beadID)+"/taints", req, &bt); err != nil {
		return nil, fmt.Errorf("applying taint %q to bead %s: %w", taintName, beadID, err)
	}
	return &bt, nil
}

// TaintBeadEntry is one row of GET /v1/taints/{name}/beads — the Beads tab
// (docs/adr/taints.md §9.1): an ordinary bead the caller is authorized to
// read, plus its propagation source for this specific taint. Source is one
// of the docs/adr/taints.md §4.2 propagation values ("explicit",
// "inherit:create", …); SourceBeadID links to the bead this taint was
// inherited from, when applicable.
type TaintBeadEntry struct {
	*BeadDetail
	Source       string `json:"source,omitempty"`
	SourceBeadID string `json:"source_bead_id,omitempty"`
}

// ListTaintBeads calls GET /v1/taints/{name}/beads. NOT owner/admin
// restricted daemon-side — scopeListFilter (the same read gate every
// GET /v1/beads call goes through) IS the authorization, so this can never
// list a bead the caller could not otherwise read.
func (c *Client) ListTaintBeads(ctx context.Context, name string, limit, offset int) ([]TaintBeadEntry, int, error) {
	path := "/v1/taints/" + url.PathEscape(name) + "/beads"
	q := url.Values{}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	if offset > 0 {
		q.Set("offset", strconv.Itoa(offset))
	}
	if len(q) > 0 {
		path += "?" + q.Encode()
	}

	var resp struct {
		Beads []struct {
			beadJSON
			Source       string `json:"source,omitempty"`
			SourceBeadID string `json:"source_bead_id,omitempty"`
		} `json:"beads"`
		Total int `json:"total"`
	}
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &resp); err != nil {
		return nil, 0, fmt.Errorf("listing beads for taint %q: %w", name, err)
	}
	out := make([]TaintBeadEntry, len(resp.Beads))
	for i, b := range resp.Beads {
		out[i] = TaintBeadEntry{BeadDetail: b.beadJSON.toDetail(), Source: b.Source, SourceBeadID: b.SourceBeadID}
	}
	return out, resp.Total, nil
}

// TaintScrubRequestEntry is one row of GET /v1/taints/{name}/scrub-requests
// — the Scrubs tab (docs/adr/taints.md §9.1): a scrub-request bead plus the
// taint's current scrub_policy rule resolution for it. CanApprove/CanReject
// mirror the daemon's own approve/reject authorization exactly (resolved
// mode + approvers membership), so this never shows a button the API would
// then refuse — except four-eyes' same-approver-as-requester check, which
// depends on which specific human clicks and so is enforced only at
// call time.
type TaintScrubRequestEntry struct {
	*BeadDetail
	ResolvedMode string `json:"resolved_mode,omitempty"`
	CanApprove   bool   `json:"can_approve"`
	CanReject    bool   `json:"can_reject"`
}

// ListTaintScrubRequests calls GET /v1/taints/{name}/scrub-requests. NOT
// owner/admin restricted daemon-side, same reasoning as ListTaintBeads —
// scopeListFilter is the authorization for the rows themselves.
//
// CanApprove/CanReject are a different story: the daemon's scrubApprover
// (kbeads/internal/server/http_scrub_requests.go) requires the CALLER'S OWN
// auth to resolve as OIDC — it deliberately does not honor a relayed
// X-Operated-By identity (same "no relay for this decision" reasoning as
// ApproveScrubRequest/RejectScrubRequest's forced-OIDC write path). So this
// call forwards the per-request user bearer when one is attached to ctx
// (WithUserBearer), letting the daemon see the human's real identity and
// compute real CanApprove/CanReject values; a caller with no forwardable
// bearer still gets the list via the ordinary relay, just with every row's
// CanApprove/CanReject correctly false rather than the request failing
// outright (operator review 2026-09-23 on PR #149: approve/reject buttons
// never appeared because this call was relay-only).
func (c *Client) ListTaintScrubRequests(ctx context.Context, name string, limit, offset int) ([]TaintScrubRequestEntry, int, error) {
	path := "/v1/taints/" + url.PathEscape(name) + "/scrub-requests"
	q := url.Values{}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	if offset > 0 {
		q.Set("offset", strconv.Itoa(offset))
	}
	if len(q) > 0 {
		path += "?" + q.Encode()
	}

	var resp struct {
		ScrubRequests []struct {
			beadJSON
			ResolvedMode string `json:"resolved_mode,omitempty"`
			CanApprove   bool   `json:"can_approve"`
			CanReject    bool   `json:"can_reject"`
		} `json:"scrub_requests"`
		Total int `json:"total"`
	}
	var err error
	if bearer := UserBearerFromContext(ctx); bearer != "" {
		err = c.doJSONAsUser(ctx, http.MethodGet, path, bearer, nil, &resp)
	} else {
		err = c.doJSON(ctx, http.MethodGet, path, nil, &resp)
	}
	if err != nil {
		return nil, 0, fmt.Errorf("listing scrub-requests for taint %q: %w", name, err)
	}
	out := make([]TaintScrubRequestEntry, len(resp.ScrubRequests))
	for i, b := range resp.ScrubRequests {
		out[i] = TaintScrubRequestEntry{
			BeadDetail:   b.beadJSON.toDetail(),
			ResolvedMode: b.ResolvedMode,
			CanApprove:   b.CanApprove,
			CanReject:    b.CanReject,
		}
	}
	return out, resp.Total, nil
}

// TaintAuditEntry is one row of GET /v1/taints/{name}/audit — the Audit tab
// (docs/adr/taints.md §9.1). Carries only ids, a truncated hash, actors, and
// timestamps — no prose, no diff (task gd-nOnc1Q8Zcv acceptance criteria).
type TaintAuditEntry struct {
	ID           int64     `json:"id"`
	TaintID      int64     `json:"taint_id"`
	Action       string    `json:"action"`
	BeadID       string    `json:"bead_id,omitempty"`
	RequestID    string    `json:"request_id,omitempty"`
	Actor        string    `json:"actor"`
	ApprovalMode string    `json:"approval_mode,omitempty"`
	ContentSHA   string    `json:"content_sha,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
}

// TaintAuditFilter narrows ListTaintAudit: Action/Actor are exact-match
// filters (empty means unfiltered), Limit/Offset page the result.
type TaintAuditFilter struct {
	Action string
	Actor  string
	Limit  int
	Offset int
}

// ListTaintAudit calls GET /v1/taints/{name}/audit. Gated daemon-side by
// canSeeTaintDetail (owner team or admin), same as the Grants tab.
func (c *Client) ListTaintAudit(ctx context.Context, name string, filter TaintAuditFilter) ([]TaintAuditEntry, int, error) {
	path := "/v1/taints/" + url.PathEscape(name) + "/audit"
	q := url.Values{}
	if filter.Action != "" {
		q.Set("action", filter.Action)
	}
	if filter.Actor != "" {
		q.Set("actor", filter.Actor)
	}
	if filter.Limit > 0 {
		q.Set("limit", strconv.Itoa(filter.Limit))
	}
	if filter.Offset > 0 {
		q.Set("offset", strconv.Itoa(filter.Offset))
	}
	if len(q) > 0 {
		path += "?" + q.Encode()
	}

	var resp struct {
		Audit []TaintAuditEntry `json:"audit"`
		Total int               `json:"total"`
	}
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &resp); err != nil {
		return nil, 0, fmt.Errorf("listing audit for taint %q: %w", name, err)
	}
	return resp.Audit, resp.Total, nil
}

// ErrScrubApprovalRequiresUserBearer is returned by ApproveScrubRequest and
// RejectScrubRequest when ctx carries no per-request user bearer
// (WithUserBearer).
//
// This mirrors ErrTaintCreateRequiresUserBearer / CreateTaint: the daemon's
// scrubApprover check (kbeads/internal/server/http_scrub_requests.go)
// STRUCTURALLY requires AuthIdentity.Type=="oidc" with a non-empty
// Subject — it does not honor the client's usual static-token +
// X-Operated-By relay AT ALL (unlike callerMayManageTaint/
// callerHoldsTaintGrant, which honor a genuinely relayed human identity).
// The daemon's own doc comment explains why: relaying scrub approval
// through the shared static token would let any holder of that token name
// an arbitrary "oidc:someone@x" identity and pass as a server-derived
// approver, defeating four-eyes on exactly the endpoint (destructive scrub)
// where it has to be real. So these two calls always force the calling
// human's own OIDC bearer onto the wire via doJSONAsUser, failing closed
// with this error when the caller has no forwardable bearer (e.g. an
// AVA-routed edge), rather than sending a request the daemon would reject
// anyway with a far less actionable 403.
var ErrScrubApprovalRequiresUserBearer = errors.New("beadsapi: scrub-request approval requires a per-request user bearer (none attached to this context)")

// ApproveScrubRequest calls POST /v1/scrub-requests/{id}/approve, ALWAYS
// forcing the per-request user bearer onto the wire — see
// ErrScrubApprovalRequiresUserBearer's doc comment for why.
func (c *Client) ApproveScrubRequest(ctx context.Context, beadID, approvalReason string) (*BeadDetail, error) {
	bearer := UserBearerFromContext(ctx)
	if bearer == "" {
		return nil, ErrScrubApprovalRequiresUserBearer
	}
	req := struct {
		ApprovalReason string `json:"approval_reason,omitempty"`
	}{ApprovalReason: approvalReason}
	var resp struct {
		Issue beadJSON `json:"issue"`
	}
	path := "/v1/scrub-requests/" + url.PathEscape(beadID) + "/approve"
	if err := c.doJSONAsUser(ctx, http.MethodPost, path, bearer, req, &resp); err != nil {
		return nil, fmt.Errorf("approving scrub-request %s: %w", beadID, err)
	}
	return resp.Issue.toDetail(), nil
}

// RejectScrubRequest calls POST /v1/scrub-requests/{id}/reject, ALWAYS
// forcing the per-request user bearer onto the wire — see
// ErrScrubApprovalRequiresUserBearer's doc comment for why.
func (c *Client) RejectScrubRequest(ctx context.Context, beadID, reason string) (*BeadDetail, error) {
	bearer := UserBearerFromContext(ctx)
	if bearer == "" {
		return nil, ErrScrubApprovalRequiresUserBearer
	}
	req := struct {
		Reason string `json:"reason"`
	}{Reason: reason}
	var resp struct {
		Issue beadJSON `json:"issue"`
	}
	path := "/v1/scrub-requests/" + url.PathEscape(beadID) + "/reject"
	if err := c.doJSONAsUser(ctx, http.MethodPost, path, bearer, req, &resp); err != nil {
		return nil, fmt.Errorf("rejecting scrub-request %s: %w", beadID, err)
	}
	return resp.Issue.toDetail(), nil
}

// RuntimeID returns the bead's persisted runtime_id field (the AWS MicroVM id a
// prior successful MicroVM launch stamped), or "" when the bead carries none.
// The MicroVM lifecycle's bind-once Launch guard consults it to refuse a
// re-launch that would re-mint an already-bound per-run identity
// (ka-0000006503). A fetch error is returned so the caller can fail open.
func (c *Client) RuntimeID(ctx context.Context, beadID string) (string, error) {
	bead, err := c.GetBead(ctx, beadID)
	if err != nil {
		return "", err
	}
	return bead.Fields["runtime_id"], nil
}

// BatchGetBeadsMaxIDs is the number of IDs this client puts in a single
// GET /v1/batch/beads request. BatchGetBeads shards larger requests into
// multiple round-trips below this cap.
//
// Raised 50 -> 500 (ka-0000001212), matching the daemon-side batchGetMaxIDs
// raised in kbeads !6159. This is the client half of the /plans latency work:
// the cost of a batch read is per-REQUEST, not per-row or per-byte (a 672-byte
// 1-ID request and an 84KB 50-ID request both cost ~20ms; GET /v1/beads at
// limit=50/500/1000 measured 5.55s/5.08s/5.10s). The /plans index hydrates
// 4522 plan children, so a cap of 50 spent ~91 round-trips against a flat
// ~20ms floor purely on request overhead; at 500 that is ~10.
//
// NOT a hard contract with the daemon. A daemon that accepts less says so
// (HTTP 400 "maximum N IDs per batch request") and the client ratchets itself
// down to N for the rest of its life — see batch_cap.go for why that matters
// more than the deploy ordering it protects.
const BatchGetBeadsMaxIDs = 500

// batchBeadResult mirrors the daemon's per-ID response shape from
// GET /v1/batch/beads (kbeads internal/server/http_batch.go).
type batchBeadResult struct {
	ID    string    `json:"id"`
	Bead  *beadJSON `json:"bead,omitempty"`
	Error string    `json:"error,omitempty"`
}

// batchBeadsResponse is the daemon's batch-fetch envelope.
type batchBeadsResponse struct {
	Results []batchBeadResult `json:"results"`
}

// BatchGetBeads fetches multiple beads in one (or a few) round-trips via
// GET /v1/batch/beads?ids=... The returned map is keyed by bead ID. IDs the
// daemon did not find (per-result `error: "not found"`, or simply absent from
// the response) are NOT in the map; callers must treat absence as the same
// signal as a per-bead fetch failure rather than a hard error. Requests with
// more than the client's effective per-request cap are sharded; a daemon that
// rejects that width narrows the client and the whole call is retried (see
// batch_cap.go).
func (c *Client) BatchGetBeads(ctx context.Context, ids []string) (map[string]*BeadDetail, error) {
	if len(ids) == 0 {
		return map[string]*BeadDetail{}, nil
	}
	var out map[string]*BeadDetail
	err := c.withBatchCapRetry(func(perRequest int) error {
		// Fresh accumulator per attempt: a cap-rejected attempt is discarded
		// whole, so results from its successful shards must not leak into the
		// retry.
		out = make(map[string]*BeadDetail, len(ids))
		var mu sync.Mutex
		return c.forEachShard(ctx, ids, perRequest, func(ctx context.Context, shard []string) error {
			q := url.Values{}
			q.Set("ids", strings.Join(shard, ","))
			var resp batchBeadsResponse
			if err := c.doJSON(ctx, http.MethodGet, "/v1/batch/beads?"+q.Encode(), nil, &resp); err != nil {
				return fmt.Errorf("batch-getting beads: %w", err)
			}
			// Decode outside the lock; only the map merge is serialized.
			decoded := make(map[string]*BeadDetail, len(resp.Results))
			for _, r := range resp.Results {
				if r.Bead == nil {
					continue
				}
				decoded[r.ID] = r.Bead.toDetail()
			}
			mu.Lock()
			defer mu.Unlock()
			maps.Copy(out, decoded)
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// GetBeadStatus fetches just the status field for a bead. Used by the status
// reporter to skip transient writes against already-closed beads during the
// K8s pod grace period (ka-0000005253).
func (c *Client) GetBeadStatus(ctx context.Context, beadID string) (string, error) {
	var bead beadJSON
	if err := c.doJSON(ctx, http.MethodGet, "/v1/beads/"+url.PathEscape(beadID), nil, &bead); err != nil {
		return "", fmt.Errorf("getting bead %s: %w", beadID, err)
	}
	return bead.Status, nil
}

// GetAgentState returns the bead's agent_state field (spawning, prewarmed,
// assigning, working, done, failed, ...) or "" when the field is unset. Used
// by the status reporter to detect prewarmed pods whose bead has already been
// assigned — the pod annotation goes stale because it is never cleared
// (ka-0000004312).
func (c *Client) GetAgentState(ctx context.Context, beadID string) (string, error) {
	var bead beadJSON
	if err := c.doJSON(ctx, http.MethodGet, "/v1/beads/"+url.PathEscape(beadID), nil, &bead); err != nil {
		return "", fmt.Errorf("getting bead %s: %w", beadID, err)
	}
	if len(bead.Fields) == 0 {
		return "", nil
	}
	var fields struct {
		AgentState string `json:"agent_state"`
	}
	if err := json.Unmarshal(bead.Fields, &fields); err != nil {
		return "", fmt.Errorf("parsing fields for bead %s: %w", beadID, err)
	}
	return fields.AgentState, nil
}

// GetAgentSessionID returns the agent bead's session identifier from the
// last_session_id field (the coop session id carried forward across resumes;
// see internal/bridge/bot_agent_kill.go). Returns "" when the bead carries no
// session id. Used to attach the session_id drill-down label to the
// gardenio_agent_lifecycle_events_total{event="oom_killed"} series so the FSM
// composite alert can resolve the OOM'd session's S3 recording key.
func (c *Client) GetAgentSessionID(ctx context.Context, beadID string) (string, error) {
	var bead beadJSON
	if err := c.doJSON(ctx, http.MethodGet, "/v1/beads/"+url.PathEscape(beadID), nil, &bead); err != nil {
		return "", fmt.Errorf("getting bead %s: %w", beadID, err)
	}
	if len(bead.Fields) == 0 {
		return "", nil
	}
	var fields struct {
		LastSessionID string `json:"last_session_id"`
	}
	if err := json.Unmarshal(bead.Fields, &fields); err != nil {
		return "", fmt.Errorf("parsing fields for bead %s: %w", beadID, err)
	}
	return fields.LastSessionID, nil
}

// ListActiveAgentBeadDetails returns all active agent beads as BeadDetail structs.
// This is useful for searching agent beads by field values (e.g., gitlab_mr_url).
func (c *Client) ListActiveAgentBeadDetails(ctx context.Context) ([]*BeadDetail, error) {
	resp, err := c.listBeads(ctx, []string{"agent"}, activeStatuses)
	if err != nil {
		return nil, fmt.Errorf("listing agent beads: %w", err)
	}
	var result []*BeadDetail
	for _, b := range resp.Beads {
		result = append(result, b.toDetail())
	}
	return result, nil
}

// findAgentByName calls GET /v1/agents-by-name/{name}, the indexed by-name
// lookup backed by the partial expression index idx_beads_fields_agent
// (migration 025). Returns ErrAgentNotFound on a 404 from the daemon, so
// callers can distinguish "no match" from a real failure. statuses, when
// non-empty, restricts the search to those bead statuses; an empty slice
// means "any status" daemon-side.
func (c *Client) findAgentByName(ctx context.Context, agentName string, statuses []string) (*beadJSON, error) {
	path := "/v1/agents-by-name/" + url.PathEscape(agentName)
	if len(statuses) > 0 {
		q := url.Values{}
		q.Set("status", strings.Join(statuses, ","))
		path += "?" + q.Encode()
	}
	var bead beadJSON
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &bead); err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
			return nil, fmt.Errorf("%w: %q", ErrAgentNotFound, agentName)
		}
		return nil, err
	}
	return &bead, nil
}

// FindAgentBead resolves the active agent bead whose fields.agent matches
// agentName via the indexed daemon endpoint. Returns ErrAgentNotFound when no
// active bead exists.
func (c *Client) FindAgentBead(ctx context.Context, agentName string) (*BeadDetail, error) {
	bead, err := c.findAgentByName(ctx, agentName, activeStatuses)
	if err != nil {
		return nil, err
	}
	return bead.toDetail(), nil
}

// FindAgentBeadAnyStatus resolves the agent bead whose fields.agent matches
// agentName regardless of bead status — including closed beads. Used by the
// card-prune loop to recover a finished agent's wrap-up after its bead has
// been closed: ListAgentBeads filters to active statuses, so a closed bead
// drops out of that list and the prune guard would otherwise flatten the
// beautiful wrap-up card to a bare one-liner (ka-0000004075). Returns
// ErrAgentNotFound when no bead with that agent name exists in any status.
func (c *Client) FindAgentBeadAnyStatus(ctx context.Context, agentName string) (*BeadDetail, error) {
	// Empty statuses → daemon matches any status (see handleFindAgentByName).
	bead, err := c.findAgentByName(ctx, agentName, nil)
	if err != nil {
		return nil, err
	}
	return bead.toDetail(), nil
}

// FindAgentBeadsByNames resolves a set of agent beads by name in a single
// daemon round-trip via GET /v1/agents-by-name:batch. Returns a map keyed
// by agent name; names with no matching bead are absent from the map (not
// an error). When statuses is empty the daemon considers any status.
//
// This is the batched form of FindAgentBead, used by beads-viewer list
// pages to prewarm the per-request agentResolver and collapse N per-row
// daemon round-trips into one (ka-0000000899).
func (c *Client) FindAgentBeadsByNames(ctx context.Context, agentNames []string, statuses []string) (map[string]*BeadDetail, error) {
	if len(agentNames) == 0 {
		return map[string]*BeadDetail{}, nil
	}
	// Deduplicate names client-side so we don't waste batch budget.
	seen := make(map[string]struct{}, len(agentNames))
	dedup := make([]string, 0, len(agentNames))
	for _, n := range agentNames {
		n = strings.TrimSpace(n)
		if n == "" {
			continue
		}
		if _, dup := seen[n]; dup {
			continue
		}
		seen[n] = struct{}{}
		dedup = append(dedup, n)
	}
	if len(dedup) == 0 {
		return map[string]*BeadDetail{}, nil
	}

	q := url.Values{}
	q.Set("names", strings.Join(dedup, ","))
	if len(statuses) > 0 {
		q.Set("status", strings.Join(statuses, ","))
	}
	path := "/v1/agents-by-name:batch?" + q.Encode()

	var resp struct {
		Agents []*beadJSON `json:"agents"`
	}
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &resp); err != nil {
		return nil, err
	}

	out := make(map[string]*BeadDetail, len(resp.Agents))
	for _, b := range resp.Agents {
		if b == nil {
			continue
		}
		detail := b.toDetail()
		if detail == nil || detail.Fields == nil {
			continue
		}
		name := detail.Fields["agent"]
		if name == "" {
			continue
		}
		out[name] = detail
	}
	return out, nil
}

// FindMostRecentAgentBead returns the most-recently-updated agent bead with
// fields.agent == agentName, considering both active and closed beads. Active
// beads are preferred — only when no active bead exists does it fall back to
// the most-recent closed match.
//
// Use this over FindAgentBead in contexts where stable agent metadata
// (project/role labels, advice subscriptions) must be resolved across session
// boundaries — e.g. advice priming on session-resume, where the previous
// session bead may have closed before the next session bead is created. In
// that window, FindAgentBead returns ErrAgentNotFound and the resolver
// silently loses the agent's project/role context, with the downstream effect
// that every project-scoped advice bead gets filtered out (see ka-0000007427).
func (c *Client) FindMostRecentAgentBead(ctx context.Context, agentName string) (*BeadDetail, error) {
	if bead, err := c.FindAgentBead(ctx, agentName); err == nil {
		return bead, nil
	} else if !errors.Is(err, ErrAgentNotFound) {
		return nil, err
	}
	// Fall back to the closed match — single indexed lookup, daemon returns the
	// most-recently-updated row for us.
	bead, err := c.findAgentByName(ctx, agentName, []string{"closed"})
	if err != nil {
		return nil, err
	}
	return bead.toDetail(), nil
}

// findActiveAgentByName checks if an active agent bead with the given name exists.
// Returns the bead ID if found, empty string if not found, or an error if the
// lookup itself failed. Unlike FindAgentBead, a "not found" result is not an error.
func (c *Client) findActiveAgentByName(ctx context.Context, agentName string) (string, error) {
	bead, err := c.findAgentByName(ctx, agentName, activeStatuses)
	if err != nil {
		if errors.Is(err, ErrAgentNotFound) {
			return "", nil
		}
		return "", err
	}
	return bead.ID, nil
}

// extraFieldTrue reports whether the named control flag is set to a truthy
// value ("1" or "true") in any of the variadic extra-field maps. Used by
// SpawnAgent to read opt-in spawn behavior (e.g. single_claim_task) out of the
// same extraFields plumbing callers already use for bead fields.
func extraFieldTrue(extraFields []map[string]string, key string) bool {
	for _, extra := range extraFields {
		switch extra[key] {
		case "1", "true":
			return true
		}
	}
	return false
}

// terminalAgentStates are the agent_state values that mean an agent has
// finished (and so is NOT holding its task_id claim anymore). An agent in any
// other state — including the empty string for a freshly-created bead that has
// not yet reported state — is treated as live.
var terminalAgentStates = map[string]bool{
	"done":   true,
	"failed": true,
}

// ProgressStaleThreshold is how long an agent's last_progress_at may lag behind
// now before the single-claim guard treats the agent as NOT live, so its task
// becomes re-dispatchable by a fresh crew (bug ka-0000000286: a livelocked crew
// whose pod stays "1/1 Running" and whose agent_state stays "working" squats its
// bead indefinitely because both the server-side single-claim guard and the
// gardener-side b2 dedup key on POD LIVENESS, which a silent stall preserves
// perfectly).
//
// The signal is last_progress_at — stamped on the agent bead only when a GENUINE
// turn lands (coop's request_count advances; see StampAgentProgress and the
// monitorExit loop in cmd/gb/init_agent.go). Unlike updated_at (which the
// rate-limit park cycle rewrites every backoff, so it stays fresh through a
// livelock) or agent_state (which stays "working"), last_progress_at freezes the
// instant real work stops — which is exactly the discriminator this guard needs.
//
// The threshold is set from real fleet turn cadence rather than guessed. Over 74
// recent sessions (2026-08-07) the median inter-turn gap was 1.34m, p90 3.03m,
// and the worst per-session average 6.58m; the longest single session ran 83m.
// The in-pod rate-limit backoff caps at 30m (rateLimitBackoffCap), so a healthy
// agent riding out a transient throttle can legitimately go one backoff-and-turn
// (~30m) without a fresh stamp. 90m is 3× that cap — the same "3× the expected
// cadence" safety factor the gardener-liveness sweep uses (livenessDefaultStaleFactor)
// — so it never races a working-but-slow or transiently-throttled agent, while
// still cutting the 12h squat down to ~90m. A genuinely wedged agent is the ONLY
// thing that goes 90m with no successful turn.
const ProgressStaleThreshold = 90 * time.Minute

// AgentProgressStalled reports whether an agent bead's last_progress_at is old
// enough (older than ProgressStaleThreshold relative to now) that the agent is
// no longer making progress and must not be treated as a live claim-holder.
//
// It FAILS OPEN in every ambiguous case, so it can only ever RELEASE a
// demonstrably-stalled claim, never steal a live one:
//   - No last_progress_at field (empty): the agent predates this signal (a bead
//     spawned by an older controller) or has simply not landed its first turn
//     yet (still booting / prewarmed). Either way we cannot prove a stall, so it
//     is treated as live. The existing 10m zombieWatchdog + the rate-limit
//     livelock bound already cover the never-worked case.
//   - Unparseable timestamp: a daemon-side glitch must not evict a healthy agent.
//   - Zero threshold: disables the check (used by tests / a kill-switch).
func AgentProgressStalled(fields map[string]string, now time.Time, threshold time.Duration) bool {
	if threshold <= 0 {
		return false
	}
	raw := fields["last_progress_at"]
	if raw == "" {
		return false
	}
	ts := parseTimestamp(raw)
	if ts.IsZero() {
		return false
	}
	return now.Sub(ts) > threshold
}

// findLiveAgentForTask returns the bead ID of an active, non-terminal agent
// already working the given task bead (fields.task_id == taskID), or "" when
// none exists. It is the single-claim guard that prevents a second crew from
// being spawned for a task another live agent is already on — the root cause of
// the duplicate-crew-per-molecule-step churn in ka-0000004654 (two crew both
// ran `terraform apply` against the same composition and fought over the state
// lock).
//
// Liveness is judged from agent_state, not just bead status: a crew that went
// silent mid-run leaves its agent bead open/in_progress but its agent_state
// flips to "failed" (or the bead is closed) — in which case the task is free to
// reclaim and this returns "". Statuses are restricted to activeStatuses so a
// closed agent bead never blocks a re-spawn.
//
// It returns the incumbent's bead ID AND its agent name. The name matters
// because this guard matches on task_id, not on name: the caller's requested
// name is typically a freshly-generated one that was never created, so only the
// incumbent's own name identifies the agent actually holding the task.
//
// Errors are returned to the caller, which logs and proceeds with creation
// (best-effort dedup, identical posture to the name-dedup path). A "not found"
// is not an error — it returns ("", "", nil).
func (c *Client) findLiveAgentForTask(ctx context.Context, taskID string) (id, name string, err error) {
	res, err := c.ListBeadsFiltered(ctx, ListBeadsQuery{
		Types:    []string{"agent"},
		Statuses: activeStatuses,
		Fields:   map[string]string{"task_id": taskID},
	})
	if err != nil {
		return "", "", err
	}
	if res == nil {
		return "", "", nil
	}
	now := time.Now()
	for _, b := range res.Beads {
		if b == nil {
			continue
		}
		if !AgentFieldsLive(b.Fields, now) {
			if AgentProgressStalled(b.Fields, now, ProgressStaleThreshold) {
				slog.Info("findLiveAgentForTask: skipping stalled agent (no progress within threshold), task is reclaimable",
					"task", taskID, "agent", b.Fields["agent"], "agent_bead", b.ID,
					"last_progress_at", b.Fields["last_progress_at"], "threshold", ProgressStaleThreshold)
			}
			continue
		}
		return b.ID, b.Fields["agent"], nil
	}
	return "", "", nil
}

// AgentFieldsLive is the single-claim guard's liveness test for an ACTIVE
// (non-closed) agent bead's fields: the agent is live unless its agent_state is
// terminal (done/failed) or its last successful turn is older than
// ProgressStaleThreshold (ka-0000000286: a livelocked crew is not working).
// Fails open on a missing or unparseable last_progress_at, as
// AgentProgressStalled does. The caller is responsible for the bead-status half
// (closed agent beads are never live). Exported so the escalation-resume gate
// judges "this task already has a live crew" exactly as SpawnAgent does
// (gd-nlR2YIDhB7).
func AgentFieldsLive(fields map[string]string, now time.Time) bool {
	if terminalAgentStates[fields["agent_state"]] {
		return false
	}
	return !AgentProgressStalled(fields, now, ProgressStaleThreshold)
}

// UpdateBeadFields updates typed fields on a bead via a read-modify-write cycle.
// The server applies ADDITIVE field-merge semantics on PATCH, so we send ONLY
// the keys being changed — never the full fields blob. Sending the full blob
// re-asserts every field the caller GET'd, which silently clobbers any other
// field a concurrent writer changed in the GET→PATCH window (lost update). The
// canonical victim: the trigger engine stamps `last_checked` on an enabled
// trigger and, by resending the whole blob, re-enables a trigger an operator
// just disabled — "trigger re-disabled: expected false, got true" (ka-0000004616).
//
// The GET is still needed to preserve the JSON type of the patched keys: if a
// field was stored as a JSON boolean, "true"/"false" updates keep it boolean;
// numeric fields stay numbers; new fields are sent as strings.
func (c *Client) UpdateBeadFields(ctx context.Context, beadID string, fields map[string]string) error {
	if len(fields) == 0 {
		return nil
	}
	// Fetch raw bead to preserve existing field types.
	var bead beadJSON
	if err := c.doJSON(ctx, http.MethodGet, "/v1/beads/"+url.PathEscape(beadID), nil, &bead); err != nil {
		return fmt.Errorf("reading bead %s for field update: %w", beadID, err)
	}

	// Parse existing fields preserving JSON types (bool stays bool, string stays string).
	var existing map[string]any
	if len(bead.Fields) > 0 {
		_ = json.Unmarshal(bead.Fields, &existing)
	}
	if existing == nil {
		existing = make(map[string]any)
	}

	// Build a PARTIAL patch: only the incoming keys, type-coerced against the
	// existing value so schema validation passes. Untouched keys are omitted so
	// the server's additive merge leaves them — and any concurrent writes to
	// them — alone.
	patch := make(map[string]any, len(fields))
	changed := false
	for k, v := range fields {
		var newVal any = v // default: plain string
		prev, ok := existing[k]
		if ok {
			if _, isBool := prev.(bool); isBool {
				newVal = (v == "true")
			} else if _, isNum := prev.(float64); isNum {
				// Preserve numeric types: if the existing value is a JSON
				// number and the new value parses as one, store it as a number
				// to satisfy schema validation.
				if n, err := strconv.ParseInt(v, 10, 64); err == nil {
					newVal = n
				} else if f, err := strconv.ParseFloat(v, 64); err == nil {
					newVal = f
				}
			}
		}
		patch[k] = newVal
		if !ok || !fieldValueEqual(prev, newVal) {
			changed = true
		}
	}

	// Skip the PATCH if every patched key already holds the target value. This
	// prevents no-op updates from firing SSE events and polluting the activity feed.
	if !changed {
		return nil
	}

	merged, err := json.Marshal(patch)
	if err != nil {
		return fmt.Errorf("marshalling fields patch for %s: %w", beadID, err)
	}
	body := map[string]json.RawMessage{
		"fields": merged,
	}
	if err := c.doJSON(ctx, http.MethodPatch, "/v1/beads/"+url.PathEscape(beadID), body, nil); err != nil {
		return fmt.Errorf("updating fields on bead %s: %w", beadID, err)
	}
	return nil
}

// fieldValueEqual reports whether a patched field value matches what is already
// stored. Existing values come from json.Unmarshal (bool, float64, string, …);
// the patched value is what UpdateBeadFields built (bool, int64, float64,
// string). Compare by JSON encoding so cross-type numeric forms (int64 vs
// float64) collapse to the same representation.
func fieldValueEqual(a, b any) bool {
	ab, err1 := json.Marshal(a)
	bb, err2 := json.Marshal(b)
	if err1 != nil || err2 != nil {
		return false
	}
	return bytes.Equal(ab, bb)
}

// UpdateBeadFieldsTyped updates typed fields on a bead. Unlike UpdateBeadFields,
// values are passed as any, allowing callers to provide proper JSON types (e.g.,
// bool for boolean-typed schema fields). Like UpdateBeadFields, it sends ONLY
// the keys being changed and relies on the server's additive field-merge, so a
// concurrent writer's changes to other fields are never clobbered (ka-0000004616).
func (c *Client) UpdateBeadFieldsTyped(ctx context.Context, beadID string, fields map[string]any) error {
	if len(fields) == 0 {
		return nil
	}
	var bead beadJSON
	if err := c.doJSON(ctx, http.MethodGet, "/v1/beads/"+url.PathEscape(beadID), nil, &bead); err != nil {
		return fmt.Errorf("reading bead %s for field update: %w", beadID, err)
	}

	var existing map[string]any
	if len(bead.Fields) > 0 {
		_ = json.Unmarshal(bead.Fields, &existing)
	}
	if existing == nil {
		existing = make(map[string]any)
	}

	// Build a PARTIAL patch: only the incoming keys, omitting untouched fields
	// so the server's additive merge leaves them (and any concurrent writes)
	// alone.
	patch := make(map[string]any, len(fields))
	changed := false
	for k, v := range fields {
		patch[k] = v
		prev, ok := existing[k]
		if !ok || !fieldValueEqual(prev, v) {
			changed = true
		}
	}

	// Skip the PATCH if every patched key already holds the target value. This
	// prevents no-op updates from firing SSE events and polluting the activity feed.
	if !changed {
		return nil
	}

	merged, err := json.Marshal(patch)
	if err != nil {
		return fmt.Errorf("marshalling fields patch for %s: %w", beadID, err)
	}

	body := map[string]json.RawMessage{
		"fields": merged,
	}
	if err := c.doJSON(ctx, http.MethodPatch, "/v1/beads/"+url.PathEscape(beadID), body, nil); err != nil {
		return fmt.Errorf("updating fields on bead %s: %w", beadID, err)
	}
	return nil
}

// IncrementBeadFields atomically adds numeric deltas to the bead's fields.
// This is safe under concurrency — the server performs the increment in a
// single SQL UPDATE, eliminating the read-modify-write race that occurs
// when using GetBead + UpdateBeadFields for counter-style fields.
func (c *Client) IncrementBeadFields(ctx context.Context, beadID string, deltas map[string]float64) error {
	if err := c.doJSON(ctx, http.MethodPost, "/v1/beads/"+url.PathEscape(beadID)+"/increment", deltas, nil); err != nil {
		return fmt.Errorf("incrementing fields on bead %s: %w", beadID, err)
	}
	return nil
}

// IncrementBeadFieldsReturning atomically adds numeric deltas to the bead's
// fields — the same collision-safe single SQL UPDATE as IncrementBeadFields —
// and returns the POST-increment bead. Use it for fetch-and-add allocation,
// where the caller needs the value its OWN increment landed on: the daemon
// serializes the UPDATE, so two concurrent callers each read back a distinct
// value and can never allocate the same sequence number. The daemon's
// /increment handler already returns the updated bead; the plain
// IncrementBeadFields variant just discards it.
func (c *Client) IncrementBeadFieldsReturning(ctx context.Context, beadID string, deltas map[string]float64) (*BeadDetail, error) {
	var bead beadJSON
	if err := c.doJSON(ctx, http.MethodPost, "/v1/beads/"+url.PathEscape(beadID)+"/increment", deltas, &bead); err != nil {
		return nil, fmt.Errorf("incrementing fields on bead %s: %w", beadID, err)
	}
	return bead.toDetail(), nil
}

// AppendBeadArrayField atomically appends element to the string-array field
// `field` (e.g. phase_order / step_order) on a bead, skipping the write if the
// element is already present (append-if-absent). The daemon performs the append
// in a single SQL UPDATE, so two concurrent appends of distinct elements can
// never clobber one another — no GetBead → UpdateBeadFieldsTyped race window.
// This is the GROW half of the plan-ordering correctness rail (ka-0000000988);
// use ReorderBeadArrayField for the reorder (permute) half.
func (c *Client) AppendBeadArrayField(ctx context.Context, beadID, field, element string) error {
	body := map[string]string{"element": element}
	path := "/v1/beads/" + url.PathEscape(beadID) + "/fields/" + url.PathEscape(field) + "/append"
	if err := c.doJSON(ctx, http.MethodPost, path, body, nil); err != nil {
		return fmt.Errorf("appending to %s on bead %s: %w", field, beadID, err)
	}
	return nil
}

// ReorderBeadArrayField compare-and-swaps the string-array field `field` on a
// bead from `expected` to `next`: the daemon writes `next` ONLY IF the field
// still equals `expected`. A reorder is a blind full-array rewrite, so under
// naive GetBead → UpdateBeadFieldsTyped a concurrent append is silently lost;
// the CAS makes the rewrite conditional. On a stale `expected` the daemon
// returns 409 Conflict, surfaced here as *APIError{StatusCode: 409} — the
// caller MUST re-read the bead, re-derive `next` against the fresh array, and
// retry. This is the REORDER half of the correctness rail (ka-0000000988).
func (c *Client) ReorderBeadArrayField(ctx context.Context, beadID, field string, expected, next []string) error {
	if expected == nil {
		expected = []string{}
	}
	if next == nil {
		next = []string{}
	}
	body := map[string]any{"expected": expected, "next": next}
	path := "/v1/beads/" + url.PathEscape(beadID) + "/fields/" + url.PathEscape(field) + "/reorder"
	if err := c.doJSON(ctx, http.MethodPost, path, body, nil); err != nil {
		return fmt.Errorf("reordering %s on bead %s: %w", field, beadID, err)
	}
	return nil
}

// IsCASConflict reports whether err is the 409 compare-and-swap conflict
// ReorderBeadArrayField returns when the caller's `expected` snapshot was
// stale. Callers use it to distinguish "re-read and retry" from a hard error.
func IsCASConflict(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusConflict
}

// UpdateBeadNotes updates the notes field of a bead.
func (c *Client) UpdateBeadNotes(ctx context.Context, beadID, notes string) error {
	body := map[string]any{
		"notes": notes,
	}
	if err := c.doJSON(ctx, http.MethodPatch, "/v1/beads/"+url.PathEscape(beadID), body, nil); err != nil {
		return fmt.Errorf("updating notes on bead %s: %w", beadID, err)
	}
	return nil
}

// UpdateBeadDescription updates the description field of a bead.
func (c *Client) UpdateBeadDescription(ctx context.Context, beadID, description string) error {
	body := map[string]any{
		"description": description,
	}
	if err := c.doJSON(ctx, http.MethodPatch, "/v1/beads/"+url.PathEscape(beadID), body, nil); err != nil {
		return fmt.Errorf("updating description on bead %s: %w", beadID, err)
	}
	return nil
}

// Reassign sets a bead's assignee. The mail subscriber uses it to claim mail
// the daemon parked for a runner (runner:<id>) for the agent joined on it.
func (c *Client) Reassign(ctx context.Context, beadID, assignee string) error {
	body := map[string]any{
		"assignee": assignee,
	}
	if err := c.doJSON(ctx, http.MethodPatch, "/v1/beads/"+url.PathEscape(beadID), body, nil); err != nil {
		return fmt.Errorf("reassigning bead %s: %w", beadID, err)
	}
	return nil
}

// ListAgentBeadsBySpawner returns active agent beads filtered by spawned_by field.
// If spawner is empty, all active agent beads are returned.
func (c *Client) ListAgentBeadsBySpawner(ctx context.Context, spawner string) ([]AgentBead, error) {
	var fields map[string]string
	if spawner != "" {
		fields = map[string]string{"spawned_by": spawner}
	}
	resp, err := c.listBeadsWithFields(ctx, []string{"agent"}, activeStatuses, fields)
	if err != nil {
		return nil, fmt.Errorf("listing agent beads: %w", err)
	}

	var beads []AgentBead
	for _, b := range resp.Beads {
		fm := b.fieldsMap()
		role := fm["role"]
		name := fm["agent"]
		if role == "" || name == "" {
			continue
		}
		mode := fm["mode"]
		if mode == "" {
			mode = "crew"
		}
		meta := make(map[string]string, len(fm))
		for k, v := range fm {
			meta[k] = v
		}
		for k, v := range ParseNotes(b.Notes) {
			meta[k] = v
		}
		beads = append(beads, AgentBead{
			ID:         b.ID,
			Title:      b.Title,
			Project:    fm["project"],
			Mode:       mode,
			Role:       role,
			AgentName:  name,
			AgentState: fm["agent_state"],
			PodPhase:   fm["pod_phase"],
			CreatedAt:  parseTimestamp(b.CreatedAt),
			UpdatedAt:  parseTimestamp(b.UpdatedAt),
			Metadata:   meta,
			Labels:     b.Labels,
		})
	}
	return beads, nil
}

// RequestAgentStop sets agent_state to "stop_requested" on the given agent bead.
func (c *Client) RequestAgentStop(ctx context.Context, beadID string) error {
	return c.UpdateBeadFields(ctx, beadID, map[string]string{"agent_state": "stop_requested"})
}

// UpdateAgentState updates the agent_state field of a bead.
func (c *Client) UpdateAgentState(ctx context.Context, beadID, state string) error {
	return c.UpdateBeadFields(ctx, beadID, map[string]string{"agent_state": state})
}

// StampAgentProgress records that the agent made GENUINE progress (a successful,
// usage-bearing turn landed) by writing the current time to last_progress_at as
// an RFC3339 timestamp. It is the heartbeat the progress-liveness guards
// (findLiveAgentForTask's single-claim check, gb news buildActiveAgents) consume
// to distinguish a working crew from a livelocked one whose pod stays "Running"
// and whose agent_state stays "working" (bug ka-0000000286).
//
// The caller is monitorExit (cmd/gb/init_agent.go), which already tracks the ONLY
// reliable progress discriminator — coop's request_count advancing — to bound the
// rate-limit livelock (ka-0000001209). A doomed rate-limit retry flips coop to
// "working" for its round-trip yet never advances request_count, so keying the
// stamp off that same signal makes it immune to the transient flip that fools
// state-based liveness. `now` is injected for testability.
func (c *Client) StampAgentProgress(ctx context.Context, beadID string, now time.Time) error {
	return c.UpdateBeadFields(ctx, beadID, map[string]string{
		"last_progress_at": now.UTC().Format(time.RFC3339),
	})
}

// MarkAgentFailed sets agent_state=failed and records failure_reason on the
// bead in a single update. Used by the reconciler to hard-fail agent beads on
// permanent K8s validation errors (e.g. DNS-1123 pod name violations) so they
// do not silently retry forever.
func (c *Client) MarkAgentFailed(ctx context.Context, beadID, reason string) error {
	return c.UpdateBeadFields(ctx, beadID, map[string]string{
		"agent_state":    "failed",
		"failure_reason": reason,
	})
}

// CloseBead closes a bead by ID, optionally setting fields at the same time.
// Fields are sent in the close request body so the server merges them without
// type-config validation — this allows decision beads to carry artifact-lifecycle
// fields (required_artifact, artifact_status, rationale) that are not declared
// in the type:decision schema.
func (c *Client) CloseBead(ctx context.Context, beadID string, fields map[string]string) error {
	return c.CloseBeadAs(ctx, beadID, "gardenio", fields)
}

// CloseBeadAs is like CloseBead but attributes the close to a specific actor
// (e.g. an authenticated operator email from the beads-viewer UI).
func (c *Client) CloseBeadAs(ctx context.Context, beadID, actor string, fields map[string]string) error {
	if actor == "" {
		actor = "gardenio"
	}
	body := map[string]any{
		"closed_by": actor,
	}
	for k, v := range fields {
		body[k] = v
	}
	if err := c.doJSON(ctx, http.MethodPost, "/v1/beads/"+url.PathEscape(beadID)+"/close", body, nil); err != nil {
		return fmt.Errorf("closing bead %s: %w", beadID, err)
	}
	return nil
}

// --- internal types and helpers ---

// beadJSON is the JSON representation of a bead from the HTTP API.
type beadJSON struct {
	ID                      string          `json:"id"`
	Slug                    string          `json:"slug,omitempty"`
	ShortTitle              string          `json:"short_title,omitempty"`
	Heraldry                string          `json:"heraldry,omitempty"`
	Title                   string          `json:"title"`
	Kind                    string          `json:"kind"`
	Type                    string          `json:"type"`
	Status                  string          `json:"status"`
	Assignee                string          `json:"assignee"`
	Owner                   string          `json:"owner,omitempty"`
	Priority                int             `json:"priority"`
	Labels                  []string        `json:"labels"`
	Notes                   string          `json:"notes"`
	Fields                  json.RawMessage `json:"fields"`
	Description             string          `json:"description"`
	CreatedBy               string          `json:"created_by"`
	ClosedBy                string          `json:"closed_by,omitempty"`
	OperatedBy              string          `json:"operated_by,omitempty"`
	OperatedByDisplay       string          `json:"operated_by_display,omitempty"`
	OperatedByEmail         string          `json:"operated_by_email,omitempty"`
	Fleet                   string          `json:"fleet,omitempty"`
	DueAt                   string          `json:"due_at,omitempty"`
	DeferUntil              string          `json:"defer_until,omitempty"`
	ClosedAt                string          `json:"closed_at,omitempty"`
	CreatedAt               string          `json:"created_at,omitempty"`
	UpdatedAt               string          `json:"updated_at,omitempty"`
	Category                string          `json:"category,omitempty"`
	OperatedByID            *int64          `json:"operated_by_id,omitempty"`
	CreatedByOperatorID     *int64          `json:"created_by_operator_id,omitempty"`
	HasReport               bool            `json:"has_report,omitempty"`
	BlockedOnDecision       bool            `json:"blocked_on_decision,omitempty"`
	BlockedOnDecisionBeadID string          `json:"blocked_on_decision_bead_id,omitempty"`
	ParkedOnDecision        bool            `json:"parked_on_decision,omitempty"`
	ParkedOnDecisionBeadID  string          `json:"parked_on_decision_bead_id,omitempty"`
	ParentID                string          `json:"parent_id,omitempty"`
	ParentTitle             string          `json:"parent_title,omitempty"`
	SpawnLineageContent     map[string]any  `json:"spawn_lineage_content,omitempty"`
	SpawnLineageDigest      string          `json:"spawn_lineage_digest,omitempty"`
}

// ParseFieldsJSON decodes a raw JSON object into a map[string]string.
// String values are kept as-is; complex values (arrays, objects, numbers)
// are re-marshaled to their JSON representation. Returns an empty (non-nil)
// map when raw is nil or empty.
func ParseFieldsJSON(raw json.RawMessage) map[string]string {
	if len(raw) == 0 {
		return make(map[string]string)
	}
	m := make(map[string]string)
	// Try direct string map first (fast path).
	if err := json.Unmarshal(raw, &m); err == nil {
		return m
	}
	// Fall back to map[string]any — re-marshal complex values.
	var anyMap map[string]any
	if err := json.Unmarshal(raw, &anyMap); err != nil {
		return make(map[string]string)
	}
	for k, v := range anyMap {
		switch v := v.(type) {
		case string:
			m[k] = v
		case nil:
			// JSON null means "unset". Re-marshaling would yield the literal
			// string "null", which consumers treat as a real value — e.g. a
			// role bead with `"model": null` overrode every pod under that
			// role to a model named "null" and 401'd all its spawns
			// (ka-0000001419 follow-up). Decode to "" like the fast path does.
			m[k] = ""
		default:
			if bs, err := json.Marshal(v); err == nil {
				m[k] = string(bs)
			} else {
				m[k] = fmt.Sprintf("%v", v)
			}
		}
	}
	return m
}

// fieldsMap decodes the JSON fields into a string map.
// Complex values (arrays, objects) are re-marshaled to JSON strings.
func (b *beadJSON) fieldsMap() map[string]string {
	return ParseFieldsJSON(b.Fields)
}

// updatedAtFormats lists timestamp formats the daemon may use, in preference order.
var updatedAtFormats = []string{
	time.RFC3339,
	"2006-01-02T15:04:05",
	"2006-01-02 15:04:05",
}

// parseTimestamp parses a timestamp string using common formats.
// Returns zero time if the string is empty or cannot be parsed.
func parseTimestamp(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	for _, layout := range updatedAtFormats {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

// toDetail converts a beadJSON to a BeadDetail.
func (b *beadJSON) toDetail() *BeadDetail {
	return &BeadDetail{
		ID:                      b.ID,
		Slug:                    b.Slug,
		ShortTitle:              b.ShortTitle,
		Heraldry:                b.Heraldry,
		Title:                   b.Title,
		Kind:                    b.Kind,
		Type:                    b.Type,
		Status:                  b.Status,
		Assignee:                b.Assignee,
		Owner:                   b.Owner,
		Priority:                b.Priority,
		Labels:                  b.Labels,
		Notes:                   b.Notes,
		Fields:                  b.fieldsMap(),
		Description:             b.Description,
		CreatedBy:               b.CreatedBy,
		ClosedBy:                b.ClosedBy,
		OperatedBy:              b.OperatedBy,
		OperatedByDisplay:       b.OperatedByDisplay,
		OperatedByEmail:         b.OperatedByEmail,
		Fleet:                   b.Fleet,
		DueAt:                   b.DueAt,
		DeferUntil:              b.DeferUntil,
		ClosedAt:                b.ClosedAt,
		CreatedAt:               parseTimestamp(b.CreatedAt),
		UpdatedAt:               parseTimestamp(b.UpdatedAt),
		Category:                b.Category,
		OperatedByID:            b.OperatedByID,
		CreatedByOperatorID:     b.CreatedByOperatorID,
		HasReport:               b.HasReport,
		BlockedOnDecision:       b.BlockedOnDecision,
		BlockedOnDecisionBeadID: b.BlockedOnDecisionBeadID,
		ParkedOnDecision:        b.ParkedOnDecision,
		ParkedOnDecisionBeadID:  b.ParkedOnDecisionBeadID,
		ParentID:                b.ParentID,
		ParentTitle:             b.ParentTitle,
		SpawnLineageContent:     b.SpawnLineageContent,
		SpawnLineageDigest:      b.SpawnLineageDigest,
	}
}

// listBeadsResponse is the JSON response from GET /v1/beads.
type listBeadsResponse struct {
	Beads []beadJSON `json:"beads"`
	Total int        `json:"total"`
}

// listBeads queries the daemon for beads matching the given type and status filters.
func (c *Client) listBeads(ctx context.Context, types, statuses []string) (*listBeadsResponse, error) {
	return c.listBeadsWithFields(ctx, types, statuses, nil)
}

// listBeadsWithFields is like listBeads but also supports field=key=value filters.
//
// It sends an EXPLICIT limit (clientListAllLimit = the daemon's max page size)
// rather than omitting the param. Every typed "list all active X" helper funnels
// through here (ListAgentBeads, ListTaskBeads, ListDecisionBeads, …), and an
// omitted limit made the daemon silently apply its defaultListBeadsLimit of
// 1000 with an HTTP 200 and no error (kbeads http_beads.go) — invisible to the
// caller. reconciler.go builds the DESIRED POD SET from ListAgentBeads and
// deletes pods absent from it, so a silent cap there is a correctness bug, not
// just a memory one (ka-0000003547 / H3, class ka-0000003555). The status filter
// keeps these populations well under the cap; warnOnSilentTruncation makes any
// breach loud rather than a silent gap.
func (c *Client) listBeadsWithFields(ctx context.Context, types, statuses []string, fields map[string]string) (*listBeadsResponse, error) {
	return c.listBeadsWithParams(ctx, types, statuses, fields, nil)
}

// listBeadsWithParams is listBeadsWithFields plus extra query params (the
// omit projections of ListAgentBeadsLean).
func (c *Client) listBeadsWithParams(ctx context.Context, types, statuses []string, fields map[string]string, extra url.Values) (*listBeadsResponse, error) {
	q := url.Values{}
	for k, v := range extra {
		q[k] = v
	}
	if len(types) > 0 {
		q.Set("type", strings.Join(types, ","))
	}
	if len(statuses) > 0 {
		q.Set("status", strings.Join(statuses, ","))
	}
	for k, v := range fields {
		q.Add("field", k+"="+v)
	}
	q.Set("limit", strconv.Itoa(clientListAllLimit))

	path := "/v1/beads"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}

	var resp listBeadsResponse
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &resp); err != nil {
		return nil, err
	}
	warnOnSilentTruncation(len(resp.Beads), resp.Total, "types", types, "statuses", statuses)
	return &resp, nil
}

// APIError represents an error response from the daemon HTTP API.
type APIError = beadswire.APIError

// doJSON performs an HTTP request with optional JSON body and decodes the JSON response.
// If result is nil, the response body is discarded (for responses where we don't need the body).
//
// When the static-token path is configured with a refresh token (Config.RefreshToken
// or SetRefreshToken), a 401 carrying WWW-Authenticate: Bearer error="invalid_token",
// error_description="token expired" triggers a refresh exchange and one retry.
// Concurrent expired-token responses funnel through a single exchange (refreshMu)
// so the daemon never sees a thundering herd of refresh requests.
func (c *Client) doJSON(ctx context.Context, method, path string, body any, result any) (err error) {
	if c.onBeadList != nil && isBeadListCall(method, path) {
		start := time.Now()
		defer func() { c.onBeadList(ctx, time.Since(start), err) }()
	}
	countListRequest(method, path)
	var bodyBytes []byte
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshaling request body: %w", err)
		}
		bodyBytes = data
	}

	// Write-bearer enforcement (ka-0000006379): when enabled, a mutating call
	// MUST carry a per-request user bearer, and that bearer overrides the static
	// read token on the wire so the daemon authenticates and attributes the
	// write as the human user. Fails closed (no request sent) when missing.
	// A per-call machine principal (WithMachinePrincipal) is the one other
	// override: see machine_principal_ctx.go for the precedence.
	overrideBearer, gateErr := c.wireCredential(ctx, method, path)
	if gateErr != nil {
		return gateErr
	}

	status, respBody, wwwAuth, retryAfter, usedToken, err := c.sendOnceRetryConn(ctx, method, path, bodyBytes, overrideBearer)
	if err != nil {
		return err
	}

	if c.shouldRefresh(status, wwwAuth) {
		if rerr := c.refreshAccessToken(ctx, usedToken); rerr != nil {
			// Surface the original 401 — the caller's mental model is "my
			// access token is bad" and that's what we should report. Refresh
			// failure is an implementation detail of the recovery attempt.
			return beadswire.DecodeResponse(status, respBody, result)
		}
		status, respBody, _, retryAfter, _, err = c.sendOnceRetryConn(ctx, method, path, bodyBytes, overrideBearer)
		if err != nil {
			return err
		}
	}
	if status == http.StatusForbidden && c.machineForbidden(ctx, overrideBearer, usedToken) {
		// A machine principal's cached token may predate a realm role Keycloak
		// has since attached (first-roll race): OnForbidden re-acquired it, so
		// retry once. The retry's own 403 is final.
		overrideBearer, gateErr = c.wireCredential(ctx, method, path)
		if gateErr != nil {
			return gateErr
		}
		status, respBody, _, retryAfter, _, err = c.sendOnceRetryConn(ctx, method, path, bodyBytes, overrideBearer)
		if err != nil {
			return err
		}
	}

	status, respBody, err = c.retryDBAuth(ctx, method, path, bodyBytes, overrideBearer, status, respBody, retryAfter)
	if err != nil {
		return err
	}
	return beadswire.DecodeResponse(status, respBody, result)
}

// retryDBAuth re-sends a request whose first answer was the daemon's
// transient DB-auth 503 (ka-0000006554, gd-W58VfF7TYi): a 503 + Retry-After
// means the daemon could not authenticate to Postgres, so the request changed
// nothing and a bounded re-send recovers a controller or gb write that would
// otherwise be dropped during a password rotation.
// beadswire.IsTransientDBAuth leaves out the 503s without a Retry-After and
// the statement-cancellation 503 (ka-0000000979); beadswire.DBAuthRetries
// says why this is safe for every method. Any other first answer is returned
// unchanged.
func (c *Client) retryDBAuth(ctx context.Context, method, path string, bodyBytes []byte, bearer string, status int, respBody []byte, retryAfter string) (int, []byte, error) {
	return beadswire.RetryDBAuth(ctx, beadswire.DBAuthRetries, beadswire.DBAuthRetryBackoff, c.sleep,
		status, respBody, retryAfter, func() (int, []byte, string, error) {
			s, b, _, ra, _, err := c.sendOnce(ctx, method, path, bodyBytes, bearer)
			return s, b, ra, err
		})
}

// machineForbidden reports whether a 403 on a request that carried usedToken
// was a machine principal's token that has just been re-acquired. The source is
// the per-call machine principal when it supplied the bearer, else the
// client's own TokenSource; the source ignores a token that is not its own.
func (c *Client) machineForbidden(ctx context.Context, overrideBearer, usedToken string) bool {
	var ts TokenSource
	if overrideBearer != "" {
		ts = MachinePrincipalFromContext(ctx)
	} else {
		ts = c.tokenSource
	}
	h, ok := ts.(MachineForbiddenHandler)
	return ok && h.OnForbidden(ctx, usedToken)
}

// sendOnceRetryConn wraps sendOnce with a bounded retry on a transport-level
// connection failure, scoped to idempotent methods only (see
// beadswire.ConnRetries).
func (c *Client) sendOnceRetryConn(ctx context.Context, method, path string, bodyBytes []byte, overrideBearer string) (status int, respBody []byte, wwwAuth, retryAfter, usedToken string, err error) {
	err = beadswire.RetryConn(ctx, method, c.sleep, func() error {
		var serr error
		status, respBody, wwwAuth, retryAfter, usedToken, serr = c.sendOnce(ctx, method, path, bodyBytes, overrideBearer)
		return serr
	})
	return status, respBody, wwwAuth, retryAfter, usedToken, err
}

// sendOnce performs a single HTTP round-trip and returns status, body, the
// WWW-Authenticate header (when present), the Retry-After header (when
// present — the daemon sets it on a transient DB-auth 503, ka-0000006554), and
// the access token actually used on the wire. The used-token is what refreshAccessToken needs to make its
// single-flight bail decision.
func (c *Client) sendOnce(ctx context.Context, method, path string, bodyBytes []byte, overrideBearer string) (int, []byte, string, string, string, error) {
	var bodyReader io.Reader
	if bodyBytes != nil {
		bodyReader = bytes.NewReader(bodyBytes)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bodyReader)
	if err != nil {
		return 0, nil, "", "", "", fmt.Errorf("creating request: %w", err)
	}
	if bodyBytes != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	setWindowViews(ctx, req)
	usedToken, err := c.setAuth(ctx, req, overrideBearer)
	if err != nil {
		return 0, nil, "", "", "", fmt.Errorf("setting auth: %w", err)
	}
	// Per-request WithActor(ctx, …) wins over the client-wide SetActor default
	// so a shared client can attribute each write to its own authenticated
	// caller (beads-viewer stamps the logged-in user's email; ka-0000006262).
	if actor := ActorFromContext(ctx); actor != "" {
		req.Header.Set("X-Actor", actor)
	} else if c.actor != "" {
		req.Header.Set("X-Actor", c.actor)
	}
	// Edge-auth envelope: when ctx carries WithEdgeAuthOperator AND the
	// client has an HMAC key, sign and stamp the envelope so the daemon
	// verifies the operator via the edge-auth path. Bridge close-from-
	// Slack-interaction uses this to assert the human's identity rather
	// than its own fleet token. See ka-0000007637.
	//
	// Never over a per-request user bearer (gd-dMof1PW1oJ): the daemon
	// takes the envelope over the bearer whenever X-Operator is present, so
	// a v1 envelope there would demote the human's own OIDC token, roles and
	// all, to a role-less edge identity (no privileged approvals). The bearer
	// already names the human, so the envelope adds nothing.
	edgeOp := EdgeAuthOperatorFromContext(ctx)
	id, hasEdgeID := EdgeAuthIdentityFromContext(ctx)
	if overrideBearer != "" {
		edgeOp, hasEdgeID = "", false
	}
	if hasEdgeID {
		if len(c.edgeAuthV2Key()) == 0 {
			return 0, nil, "", "", "", fmt.Errorf("edge-auth identity %q requested but no HMAC key is configured", id.Operator)
		}
		if id.Operator == "" || len(id.Roles) == 0 {
			return 0, nil, "", "", "", fmt.Errorf("edge-auth identity needs an operator and at least one role")
		}
		roles := append([]string(nil), id.Roles...)
		sort.Strings(roles)
		joined := strings.Join(roles, ",")
		ts := strconv.FormatInt(time.Now().Unix(), 10)
		auditID := newEdgeAuthAuditID()
		req.Header.Set("X-Operator", id.Operator)
		req.Header.Set("X-Auth-Audit-Id", auditID)
		req.Header.Set("X-Edge-Auth-Ts", ts)
		req.Header.Set("X-Edge-Auth-Version", "2")
		req.Header.Set("X-Edge-Auth-Mode", id.AuthMode)
		req.Header.Set("X-Edge-Auth-Roles", joined)
		req.Header.Set("X-Edge-Auth-Sig-V2",
			signEdgeAuthV2(c.edgeAuthV2Key(), id.Operator, id.AuthMode, joined, auditID, req.Method, req.URL.Path, ts))
	} else if edgeOp != "" && len(c.edgeAuthKey) > 0 {
		ts := strconv.FormatInt(time.Now().Unix(), 10)
		auditID := newEdgeAuthAuditID()
		sig := signEdgeAuth(c.edgeAuthKey, edgeOp, auditID, req.URL.Path, ts)
		req.Header.Set("X-Operator", edgeOp)
		req.Header.Set("X-Auth-Audit-Id", auditID)
		req.Header.Set("X-Edge-Auth-Sig", sig)
		req.Header.Set("X-Edge-Auth-Ts", ts)
	} else if op := OperatorFromContext(ctx); op != "" {
		req.Header.Set("X-Operated-By", op)
	} else if c.operatedBy != "" {
		req.Header.Set("X-Operated-By", c.operatedBy)
	}
	if so := SystemOperatorFromContext(ctx); so != "" {
		req.Header.Set("X-System-Operator", so)
	} else if c.systemOperator != "" {
		req.Header.Set("X-System-Operator", c.systemOperator)
	}
	if EscalateFromContext(ctx) {
		req.Header.Set("X-Escalate", "true")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, nil, "", "", "", fmt.Errorf("performing request: %w", err)
	}
	defer resp.Body.Close()

	// Observe the daemon's build version (ka-0000002965) before branching on
	// status — the header rides every response, including 204s and errors.
	c.noteDaemonVersion(resp.Header.Get("X-Kbeads-Version"))

	wwwAuth := resp.Header.Get("WWW-Authenticate")
	retryAfter := resp.Header.Get("Retry-After")

	if resp.StatusCode == http.StatusNoContent {
		return resp.StatusCode, nil, wwwAuth, retryAfter, usedToken, nil
	}

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, "", "", "", fmt.Errorf("reading response: %w", err)
	}
	// When the edge 3xx-bounces an unauthenticated call to the IdP, which is
	// Cloudflare-walled, the response body is a CF challenge / sign-in page,
	// not JSON. Surface the IdP-agnostic re-auth sentinel so callers print
	// "run gb login" instead of HTTP-403-ing a wall of Cloudflare HTML (and so
	// retry loops can bail fast — a challenge page is not a transient error).
	if reauthErr := avareauth.DetectFromResponse(resp, respBody); reauthErr != nil {
		return resp.StatusCode, respBody, wwwAuth, retryAfter, usedToken, reauthErr
	}
	return resp.StatusCode, respBody, wwwAuth, retryAfter, usedToken, nil
}

// shouldRefresh returns true when the response is the daemon's "your access
// token expired, refresh it" signal AND we're on the static-token path with
// a refresh token configured. TokenSource callers refresh internally; we
// don't second-guess them.
func (c *Client) shouldRefresh(status int, wwwAuth string) bool {
	if status != http.StatusUnauthorized {
		return false
	}
	if c.tokenSource != nil {
		return false
	}
	if !beadswire.IsExpiredAccessTokenChallenge(wwwAuth) {
		return false
	}
	c.tokenMu.RLock()
	defer c.tokenMu.RUnlock()
	return c.refreshToken != ""
}

// refreshAccessToken exchanges the current refresh token for a new access
// token (and a rotated refresh token, when the daemon enables rotation) and
// swaps the result onto the client. Concurrent callers serialise on
// refreshMu; the post-acquire token re-read short-circuits peer goroutines
// that arrived during a peer's exchange.
//
// staleToken is the access token that was on the wire for the failing
// request. After acquiring refreshMu we compare against c.token: if a peer
// already swapped it past staleToken we skip the exchange and let the retry
// use the freshly-installed token.
func (c *Client) refreshAccessToken(ctx context.Context, staleToken string) error {
	c.refreshMu.Lock()
	defer c.refreshMu.Unlock()

	c.tokenMu.RLock()
	current := c.token
	refresh := c.refreshToken
	c.tokenMu.RUnlock()
	if current != staleToken && current != "" {
		return nil
	}
	if refresh == "" {
		return fmt.Errorf("no refresh token configured")
	}

	rr, err := beadswire.ExchangeRefreshToken(ctx, c.httpClient, c.baseURL, refresh)
	if err != nil {
		return err
	}

	c.tokenMu.Lock()
	c.token = rr.AccessToken
	if rr.Rotated && rr.RefreshToken != "" {
		c.refreshToken = rr.RefreshToken
	}
	cb := c.onRefresh
	c.tokenMu.Unlock()
	if cb != nil {
		cb(rr.AccessToken, rr.RefreshToken)
	}
	return nil
}

// RegisterBridgeToken registers a project-scoped fleet token with the beads daemon.
// The token is associated with the given fleet name and restricted to the specified project.
// Only the admin token (controller's BEADS_DAEMON_TOKEN) can call this endpoint.
func (c *Client) RegisterBridgeToken(ctx context.Context, token, fleet, project string) error {
	return c.RegisterBridgeTokenWithTTL(ctx, token, fleet, project, 0)
}

// RegisterBridgeTokenWithTTL registers a project-scoped fleet token with a TTL.
// A zero ttl means the token never expires (server-side default behavior).
// Only the admin token (controller's BEADS_DAEMON_TOKEN) can call this endpoint.
func (c *Client) RegisterBridgeTokenWithTTL(ctx context.Context, token, fleet, project string, ttl time.Duration) error {
	body := map[string]any{
		"token":   token,
		"fleet":   fleet,
		"project": project,
	}
	if ttl > 0 {
		body["ttl"] = ttl.String()
	}
	if err := c.doJSON(ctx, http.MethodPost, "/v1/fleet-tokens", body, nil); err != nil {
		return fmt.Errorf("registering fleet token for %s: %w", fleet, err)
	}
	return nil
}

// BridgeTokenWithRefresh is the issued credential pair returned by
// RegisterBridgeTokenWithRefresh. RefreshTokenExpiresAt is zero when the
// daemon's refresh-token TTL is unbounded (server default 30d unless
// configured otherwise).
type BridgeTokenWithRefresh struct {
	RefreshToken          string
	RefreshTokenExpiresAt time.Time
}

// RegisterBridgeTokenWithRefresh registers a fleet token AND mints a
// companion refresh token in one round-trip (with_refresh:true on
// POST /v1/fleet-tokens). Used by agent provisioning so the controller can
// hand the agent pod both an access token (short TTL, e.g. 24h) and a
// refresh token (longer TTL) before the pod starts.
//
// accessTTL is the access-token expiry (zero = never expires); refreshTTL
// is the refresh-token expiry (zero = server default). Only the admin
// token can call this endpoint. Subject is the audit identity carried by
// the access token (and any refresh-rotated descendant); empty falls back
// to the fleet name on the daemon side (C1.4 back-compat). When privileged
// is true the daemon marks the token as a privileged service-fleet identity
// that may bypass per-bead ownership for system-managed graph writes
// (ka-0000001946).
func (c *Client) RegisterBridgeTokenWithRefresh(ctx context.Context, token, fleet, project, subject string, accessTTL, refreshTTL time.Duration, privileged bool) (*BridgeTokenWithRefresh, error) {
	body := map[string]any{
		"token":        token,
		"fleet":        fleet,
		"project":      project,
		"with_refresh": true,
	}
	if subject != "" {
		body["subject"] = subject
	}
	if privileged {
		body["privileged"] = true
	}
	if accessTTL > 0 {
		body["ttl"] = accessTTL.String()
	}
	if refreshTTL > 0 {
		body["refresh_ttl"] = refreshTTL.String()
	}
	var resp struct {
		RefreshToken          string `json:"refresh_token"`
		RefreshTokenExpiresAt string `json:"refresh_token_expires_at"`
	}
	if err := c.doJSON(ctx, http.MethodPost, "/v1/fleet-tokens", body, &resp); err != nil {
		return nil, fmt.Errorf("registering fleet token with refresh for %s: %w", fleet, err)
	}
	if resp.RefreshToken == "" {
		return nil, fmt.Errorf("registering fleet token for %s: server did not return refresh_token", fleet)
	}
	out := &BridgeTokenWithRefresh{RefreshToken: resp.RefreshToken}
	if resp.RefreshTokenExpiresAt != "" {
		t, err := time.Parse(time.RFC3339Nano, resp.RefreshTokenExpiresAt)
		if err != nil {
			return nil, fmt.Errorf("parsing refresh_token_expires_at %q: %w", resp.RefreshTokenExpiresAt, err)
		}
		out.RefreshTokenExpiresAt = t
	}
	return out, nil
}

// BridgeTokenInfo is the daemon's per-fleet summary returned by
// ListBridgeTokensDetailed. Token plaintext is never included. ExpiresAt is
// the zero time when the fleet's token has no expiry (back-compat seed path).
type BridgeTokenInfo struct {
	Fleet          string
	Project        string
	Subject        string
	ExpiresAt      time.Time
	CrossFleetRead bool
}

// ListBridgeTokensDetailed fetches per-fleet token metadata from the daemon
// (GET /v1/fleet-tokens?detailed=true). Only the admin token can call this
// endpoint. Used by the controller's fleet-token rotator to decide which
// fleet tokens are nearing TTL expiry.
func (c *Client) ListBridgeTokensDetailed(ctx context.Context) ([]BridgeTokenInfo, error) {
	var resp struct {
		Fleets []struct {
			Name           string `json:"name"`
			Project        string `json:"project"`
			Subject        string `json:"subject"`
			ExpiresAt      string `json:"expires_at"`
			CrossFleetRead bool   `json:"cross_fleet_read"`
		} `json:"fleets"`
	}
	if err := c.doJSON(ctx, http.MethodGet, "/v1/fleet-tokens?detailed=true", nil, &resp); err != nil {
		return nil, fmt.Errorf("listing fleet tokens (detailed): %w", err)
	}
	out := make([]BridgeTokenInfo, 0, len(resp.Fleets))
	for _, f := range resp.Fleets {
		info := BridgeTokenInfo{
			Fleet:          f.Name,
			Project:        f.Project,
			Subject:        f.Subject,
			CrossFleetRead: f.CrossFleetRead,
		}
		if f.ExpiresAt != "" {
			t, err := time.Parse(time.RFC3339Nano, f.ExpiresAt)
			if err != nil {
				return nil, fmt.Errorf("parsing expires_at %q for fleet %q: %w", f.ExpiresAt, f.Name, err)
			}
			info.ExpiresAt = t
		}
		out = append(out, info)
	}
	return out, nil
}

// RevokeBridgeToken removes a fleet token from the beads daemon.
// Returns nil if the token was successfully revoked or didn't exist.
func (c *Client) RevokeBridgeToken(ctx context.Context, fleet string) error {
	err := c.doJSON(ctx, http.MethodDelete, "/v1/fleet-tokens/"+url.PathEscape(fleet), nil, nil)
	if err != nil {
		// Treat 404 as success (token already revoked or never existed).
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
			return nil
		}
		return fmt.Errorf("revoking fleet token for %s: %w", fleet, err)
	}
	return nil
}

// ParseNotes parses "key: value" lines from a bead's notes field into a map.
func ParseNotes(notes string) map[string]string {
	if notes == "" {
		return nil
	}
	m := make(map[string]string)
	for _, line := range strings.Split(notes, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) == 2 {
			m[strings.TrimSpace(parts[0])] = strings.TrimSpace(parts[1])
		}
	}
	if len(m) == 0 {
		return nil
	}
	return m
}
