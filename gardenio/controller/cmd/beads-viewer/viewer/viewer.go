// Package viewer hosts the OIDC-subject → operators(id) resolver shared by
// the beads-viewer's HTTP handlers and the (forthcoming) inbox surface.
//
// Inbox-v1 G2 (bead ka-0000003651, plan
// gardenio/plans/inbox-v1-option-b-sequencing.md §1) lifts what was an
// ad-hoc per-handler lookup into a single primitive so the inbox can build
// operator-id-keyed queries without duplicating the OIDC-collapse rules. The
// resolver is the only place that knows how to map a viewer's authenticated
// email to the operators(id) FK created by the Option B P1 substrate (ADR
// docs/adr/human-attribution-identity-v2.md §3.1, §5.1).
//
// Caching contract (G2.a):
//
//   - Per-OIDC-subject TTL-bounded cache. A successful resolve is cached
//     for the configured TTL (default 60s); a "no alias exists" miss is
//     cached for the same window so unprovisioned viewers don't slam the
//     daemon.
//   - The cache is explicitly invalidated by Invalidate(subject) when an
//     external observer (the viewer's SSE consumer, an admin tool, or a
//     test) detects an operators/operator_aliases mutation.
//   - The resolver is safe for concurrent use; callers do not need their
//     own lock around it.
//
// Sentinel-safety contract (G2.b):
//
//   - When the alias lookup returns an operator whose operators(kind) is
//     "system" or "fleet", the resolver refuses the result and reports
//     OperatorID(ctx) (0, false, error). This guards against an operator
//     row whose alias was manually pointed at a sentinel id — a contract
//     violation, but a recoverable one. The error wraps
//     ErrSentinelOperator so callers can distinguish it from "no alias"
//     (false, nil) and from transport errors.
//   - The corollary is that an OIDC-authenticated viewer can NEVER be
//     attributed as a system/fleet sentinel. Callers in operator_chip.go
//     and the inbox surface rely on this invariant.
package viewer

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/oidcauth"
)

// DefaultTTL is the default cache lifetime per G2.a.
const DefaultTTL = 60 * time.Second

// ErrSentinelOperator is wrapped in the error returned by OperatorID when
// the alias resolves to a kind ∈ {system, fleet} operator. See package doc
// G2.b for the contract.
var ErrSentinelOperator = errors.New("alias resolved to sentinel operator")

// AliasLookup is the read primitive the resolver depends on. The production
// implementation is *beadsapi.Client.LookupOperatorByAlias; tests substitute
// an in-memory fake.
type AliasLookup interface {
	LookupOperatorByAlias(ctx context.Context, kind, subject string) (*beadsapi.LookupOperatorByAliasResponse, error)
}

// EmailExtractor pulls the authenticated viewer's email from a request
// context. Production wiring uses oidcauth.EmailFromContext directly via
// the package-level helper; tests inject a fake to avoid full OIDC plumbing.
type EmailExtractor func(ctx context.Context) string

// cacheEntry is one (operator id, found?) result with an expiry stamp.
type cacheEntry struct {
	operatorID int64
	found      bool
	expiresAt  time.Time
}

// Resolver maps OIDC subjects (emails) to operators(id) FKs. Safe for
// concurrent use.
type Resolver struct {
	lookup  AliasLookup
	email   EmailExtractor
	ttl     time.Duration
	now     func() time.Time

	mu    sync.Mutex
	cache map[string]cacheEntry
}

// Option configures Resolver behavior at construction time.
type Option func(*Resolver)

// WithTTL overrides the cache TTL. Use for tests; production should accept
// the default.
func WithTTL(d time.Duration) Option {
	return func(r *Resolver) { r.ttl = d }
}

// WithClock overrides the time source. Tests use this to advance the cache
// past TTL deterministically.
func WithClock(now func() time.Time) Option {
	return func(r *Resolver) { r.now = now }
}

// WithEmailExtractor overrides the email extraction strategy. Default is
// oidcauth.EmailFromContext.
func WithEmailExtractor(fn EmailExtractor) Option {
	return func(r *Resolver) { r.email = fn }
}

// NewResolver constructs a Resolver. The lookup parameter is required.
func NewResolver(lookup AliasLookup, opts ...Option) *Resolver {
	r := &Resolver{
		lookup: lookup,
		email:  oidcauth.EmailFromContext,
		ttl:    DefaultTTL,
		now:    time.Now,
		cache:  make(map[string]cacheEntry),
	}
	for _, o := range opts {
		o(r)
	}
	return r
}

// OperatorID resolves the authenticated viewer in ctx to an operators(id)
// FK. Returns:
//
//   - (id, true, nil)  — viewer is provisioned; id is a non-sentinel human
//     operator.
//   - (0, false, nil)  — no viewer email in ctx (anonymous request) OR no
//     alias row exists for the viewer's email yet (unprovisioned).
//   - (0, false, err)  — transport error OR the alias points at a sentinel
//     operator (err wraps ErrSentinelOperator). The cache is NOT populated
//     on either error path; callers may retry.
func (r *Resolver) OperatorID(ctx context.Context) (int64, bool, error) {
	email := strings.TrimSpace(r.email(ctx))
	if email == "" {
		return 0, false, nil
	}
	if id, found, ok := r.cacheGet(email); ok {
		return id, found, nil
	}
	resp, err := r.lookup.LookupOperatorByAlias(ctx, "oidc", email)
	if err != nil {
		return 0, false, err
	}
	if !resp.Found {
		r.cachePut(email, cacheEntry{found: false, expiresAt: r.now().Add(r.ttl)})
		return 0, false, nil
	}
	if isSentinelKind(resp.OperatorKind) {
		return 0, false, fmt.Errorf("oidc:%s alias points at operator id %d (kind=%s): %w",
			email, resp.OperatorID, resp.OperatorKind, ErrSentinelOperator)
	}
	r.cachePut(email, cacheEntry{
		operatorID: resp.OperatorID,
		found:      true,
		expiresAt:  r.now().Add(r.ttl),
	})
	return resp.OperatorID, true, nil
}

// Invalidate evicts the cache entry for a single OIDC subject. Wire this
// from the SSE consumer when an operators/operator_aliases mutation event
// is observed; the next OperatorID call for that subject will re-fetch.
func (r *Resolver) Invalidate(subject string) {
	s := strings.TrimSpace(subject)
	if s == "" {
		return
	}
	r.mu.Lock()
	delete(r.cache, s)
	r.mu.Unlock()
}

// InvalidateAll evicts every cached entry. Use when a global operators
// mutation arrives (e.g. backfill complete) where per-subject targeting
// isn't possible.
func (r *Resolver) InvalidateAll() {
	r.mu.Lock()
	r.cache = make(map[string]cacheEntry)
	r.mu.Unlock()
}

func (r *Resolver) cacheGet(subject string) (int64, bool, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.cache[subject]
	if !ok {
		return 0, false, false
	}
	if !r.now().Before(e.expiresAt) {
		delete(r.cache, subject)
		return 0, false, false
	}
	return e.operatorID, e.found, true
}

func (r *Resolver) cachePut(subject string, e cacheEntry) {
	r.mu.Lock()
	r.cache[subject] = e
	r.mu.Unlock()
}

// isSentinelKind reports whether the operators(kind) is one of the
// pre-seeded sentinel kinds (migration 015 §3.2). Sentinels MUST NOT be
// returned to OIDC-authenticated callers.
func isSentinelKind(kind string) bool {
	switch kind {
	case "system", "fleet":
		return true
	default:
		return false
	}
}
