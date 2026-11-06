package main

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"regexp"
	"sync"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// agentNamePattern matches the shape of an agent name produced by
// `gb spawn` (alphanumeric + `-`/`_`, leading letter, length-bounded).
// Values that don't match — emails, OIDC subjects like `slack:U…`, full
// names with spaces — are never linkified, so the template helper
// short-circuits without a daemon round-trip and humans-as-assignees
// don't render as broken agent links.
var agentNamePattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]{1,99}$`)

// looksLikeAgentName reports whether name has the surface shape of an
// agent identifier. Non-matching strings short-circuit the linkify path.
func looksLikeAgentName(name string) bool {
	return agentNamePattern.MatchString(name)
}

// agentBeadFinder is the subset of beadsapi.Client used to resolve an
// agent name to its bead at template-render time. Implemented by the
// production daemon client and by fakeDaemon in tests.
type agentBeadFinder interface {
	FindMostRecentAgentBead(ctx context.Context, agentName string) (*beadsapi.BeadDetail, error)
	// FindAgentBeadsByNames is the batched form, called once per list-page
	// render to prewarm the resolver cache. Returns a map keyed by agent
	// name; missing names are absent (not an error). statuses empty means
	// "any status" — Prewarm passes nil for the active→closed fallback.
	FindAgentBeadsByNames(ctx context.Context, agentNames []string, statuses []string) (map[string]*beadsapi.BeadDetail, error)
}

// agentResolver memoizes name→bead-ID lookups for the lifetime of a
// single HTTP request. The render path constructs one per request and
// binds it into the template's `agentLink` func via Clone+Funcs, so
// list pages with many distinct agent names hit the daemon at most
// once per name.
//
// Misses are cached too (as the empty string) so a second occurrence
// of an unresolvable name on the same page doesn't repeat the call.
//
// Nil-safe: a nil resolver makes Resolve a no-op miss, which lets
// renderAgentLink fall back to plain-text rendering when no per-request
// resolver was wired in.
type agentResolver struct {
	ctx    context.Context
	finder agentBeadFinder
	now    func() time.Time
	mu     sync.Mutex
	cache  map[string]agentInfo // name → info; ID=="" means resolved-as-miss
}

// agentInfo is the cached result of one name→bead lookup. Alive mirrors
// the agents.html liveness predicate (state ∈ {working, spawning, yielding}
// AND UpdatedAt within agentStaleThreshold of now) so callers can render
// a live-agent affordance without a second daemon round-trip.
type agentInfo struct {
	ID    string
	Alive bool
}

// agentResolverCtxKey is the request-context key under which list-page
// handlers stash a prewarmed agentResolver so bindAgentLink can reuse it
// at template-render time. Pointer-keyed to avoid string-key collisions.
type agentResolverCtxKeyT struct{}

var agentResolverCtxKey = agentResolverCtxKeyT{}

// withAgentResolver returns ctx with resolver attached. List-page
// handlers call this after Prewarm so bindAgentLink reuses the warmed
// cache rather than constructing a fresh resolver per render.
func withAgentResolver(ctx context.Context, resolver *agentResolver) context.Context {
	if resolver == nil {
		return ctx
	}
	return context.WithValue(ctx, agentResolverCtxKey, resolver)
}

// agentResolverFromContext returns the prewarmed resolver, or nil when
// no handler attached one.
func agentResolverFromContext(ctx context.Context) *agentResolver {
	if ctx == nil {
		return nil
	}
	r, _ := ctx.Value(agentResolverCtxKey).(*agentResolver)
	return r
}

// newAgentResolver returns a per-request resolver bound to ctx.
// finder may be nil — Resolve degrades to a miss in that case.
func newAgentResolver(ctx context.Context, finder agentBeadFinder) *agentResolver {
	return &agentResolver{
		ctx:    ctx,
		finder: finder,
		now:    time.Now,
		cache:  make(map[string]agentInfo),
	}
}

// prewarmAgentLinksFromBeads creates a resolver, prewarms it with every
// agent-shaped name found on Assignee/Owner of the given beads, and
// attaches the resolver to r's context. Returns the new request so the
// caller can use it for downstream rendering. Used by list-page handlers
// to collapse N per-row daemon round-trips into one (ka-0000000899).
//
// Falls back gracefully: nil finder or empty input returns r unchanged.
func (s *Server) prewarmAgentLinksFromBeads(r *http.Request, beads []*beadsapi.BeadDetail) *http.Request {
	if r == nil || s == nil || s.daemon == nil || len(beads) == 0 {
		return r
	}
	names := make([]string, 0, len(beads)*2)
	for _, b := range beads {
		if b == nil {
			continue
		}
		if b.Assignee != "" {
			names = append(names, b.Assignee)
		}
		if b.Owner != "" {
			names = append(names, b.Owner)
		}
	}
	return s.prewarmAgentLinksForNames(r, names)
}

// prewarmAgentLinksForNames is the lower-level helper: it builds a
// resolver, prewarms it with names, and attaches it to r's context.
// Names that don't match looksLikeAgentName are skipped inside Prewarm.
func (s *Server) prewarmAgentLinksForNames(r *http.Request, names []string) *http.Request {
	if r == nil || s == nil || s.daemon == nil || len(names) == 0 {
		return r
	}
	resolver := newAgentResolver(r.Context(), s.daemon)
	resolver.Prewarm(names)
	return r.WithContext(withAgentResolver(r.Context(), resolver))
}

// Prewarm batches one daemon call to resolve every agent-shaped name in
// names and pre-populates the in-memory cache. Subsequent Resolve /
// ResolveInfo calls for those names hit the cache and never call the
// daemon. Names that don't match looksLikeAgentName are skipped (they'd
// short-circuit at render time anyway). Names already cached are
// skipped. Empty input, nil resolver, and nil finder are no-ops.
//
// Called from list-page handlers before render so a page with N distinct
// agent assignees collapses N per-row daemon round-trips into one batch
// (ka-0000000899).
func (r *agentResolver) Prewarm(names []string) {
	if r == nil || r.finder == nil || len(names) == 0 {
		return
	}
	// Collect distinct, agent-shaped, not-yet-cached names. Done under
	// the lock so concurrent Resolves don't double-fetch.
	r.mu.Lock()
	want := make([]string, 0, len(names))
	seen := make(map[string]struct{}, len(names))
	for _, n := range names {
		if n == "" || !looksLikeAgentName(n) {
			continue
		}
		if _, dup := seen[n]; dup {
			continue
		}
		seen[n] = struct{}{}
		if _, cached := r.cache[n]; cached {
			continue
		}
		want = append(want, n)
	}
	r.mu.Unlock()

	if len(want) == 0 {
		return
	}

	beads, err := r.finder.FindAgentBeadsByNames(r.ctx, want, nil)
	if err != nil {
		// Don't poison the cache on a batch error — Resolve falls back
		// to per-name lookups for any name that's still uncached.
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	for _, name := range want {
		bead, ok := beads[name]
		if !ok || bead == nil {
			r.cache[name] = agentInfo{}
			continue
		}
		r.cache[name] = agentInfo{ID: bead.ID, Alive: r.isAlive(bead)}
	}
}

// Resolve returns the bead ID for name, or "" with ok=false on any
// miss (no match, daemon error, nil resolver, blank/non-agent-shaped
// name). Repeat calls within the same request are served from cache.
func (r *agentResolver) Resolve(name string) (string, bool) {
	info, ok := r.ResolveInfo(name)
	return info.ID, ok
}

// ResolveInfo returns the bead ID and liveness for name. ok mirrors
// Resolve: false on any miss. Repeat calls within the same request hit
// the in-memory cache so a single render fires at most one daemon call
// per distinct name even when both agentLink and agentLiveIcon are
// invoked for the same row.
func (r *agentResolver) ResolveInfo(name string) (agentInfo, bool) {
	if r == nil || r.finder == nil || name == "" {
		return agentInfo{}, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if info, ok := r.cache[name]; ok {
		return info, info.ID != ""
	}
	bead, err := r.finder.FindMostRecentAgentBead(r.ctx, name)
	if err != nil || bead == nil {
		r.cache[name] = agentInfo{}
		return agentInfo{}, false
	}
	info := agentInfo{ID: bead.ID, Alive: r.isAlive(bead)}
	r.cache[name] = info
	return info, true
}

// isAlive mirrors the /agents predicate: agent_state ∈ {working,
// spawning, yielding} AND UpdatedAt within agentStaleThreshold of now.
// A bead with no UpdatedAt is treated as alive (matches isAgentStale's
// "zero time is not stale" guard) so test fixtures without timestamps
// behave the same as new agents that haven't pushed an update yet.
func (r *agentResolver) isAlive(b *beadsapi.BeadDetail) bool {
	if b == nil {
		return false
	}
	if _, ok := agentStateAlive[b.Fields["agent_state"]]; !ok {
		return false
	}
	if b.UpdatedAt.IsZero() {
		return true
	}
	return r.now().Sub(b.UpdatedAt) < agentStaleThreshold
}

// renderAgentLink returns HTML wrapping name in an <a> that points
// directly at the resolved agent bead's /{ka-id} URL. The resolver
// performs a per-request memoized name→bead lookup at render time, so
// the emitted href is stable and shareable (no /a/{name} 302 redirect).
//
// Falsy values render as a literal em-dash to match the existing
// empty-state styling in assignee_block / owner_block. Non-agent-shaped
// values (emails, OIDC subjects, full names) render as plain text.
//
// On resolver miss — no daemon, no match, or daemon error — the helper
// falls back to plain-text rendering rather than emitting a dead link.
// The hover popover (htmx) keeps working: the JS classifies the link as
// a bead link by its /{ka-id} path and fetches /b/{id}/preview, which
// already exists. /a/{name}/preview is preserved separately so typed
// mentions in description/comment markdown can still hover-preview.
func renderAgentLink(basePath, name string, resolver *agentResolver) template.HTML {
	if name == "" {
		return template.HTML(`<span class="empty">&mdash;</span>`)
	}
	escaped := template.HTMLEscapeString(name)
	if !looksLikeAgentName(name) {
		return template.HTML(escaped)
	}
	id, ok := resolver.Resolve(name)
	if !ok {
		return template.HTML(escaped)
	}
	href := basePath + "/" + url.PathEscape(id)
	return template.HTML(fmt.Sprintf(
		`<a class="agent-link" href="%s" data-agent="%s">%s</a>`,
		template.HTMLEscapeString(href),
		escaped,
		escaped,
	))
}

// renderAgentLiveIcon returns a small clickable dot linking to the
// agent's bead when name resolves to an alive agent (per the /agents
// liveness predicate). Returns the empty string for empty / non-agent-
// shaped values, resolver misses, and agents that are not currently
// alive — so list-view callers can blindly invoke the helper without
// per-row conditionals and only see an icon when there's a live agent
// to link to. The link target matches agentLink's href, so a row's
// dot and its assignee text both navigate to the same /{ka-id}.
func renderAgentLiveIcon(basePath, name string, resolver *agentResolver) template.HTML {
	if name == "" || !looksLikeAgentName(name) {
		return ""
	}
	info, ok := resolver.ResolveInfo(name)
	if !ok || !info.Alive {
		return ""
	}
	href := basePath + "/" + url.PathEscape(info.ID)
	return template.HTML(fmt.Sprintf(
		`<a class="agent-live-icon" href="%s" title="agent working: %s" aria-label="live agent %s">●</a>`,
		template.HTMLEscapeString(href),
		template.HTMLEscapeString(name),
		template.HTMLEscapeString(name),
	))
}

// handleAgentByNamePreview resolves an agent name and renders the same
// bead_preview fragment that /b/{id}/preview emits. Mirrors
// handleBeadPreview's response contract:
//   - 200 + bead_preview when the name resolves.
//   - 200 + bead_preview_notfound when no agent matches (the htmx
//     popover injects the "not found" markup just like for gd-… misses).
//   - 500 on a daemon error so the client controller drops the popover.
//
// agentLink itself now resolves names to bead IDs at render time and
// emits href="/{ka-id}" — the popover JS classifies that as a bead link
// and fetches /b/{id}/preview directly. This handler stays for typed
// mentions in description/comment markdown that surface as /a/{name}
// links via linkify, and for any external deep-link still using the
// name-keyed shape.
func (s *Server) handleAgentByNamePreview(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		http.Error(w, "Missing agent name", http.StatusBadRequest)
		return
	}
	bead, err := s.daemon.FindMostRecentAgentBead(r.Context(), name)
	if err != nil {
		if errors.Is(err, beadsapi.ErrAgentNotFound) {
			w.Header().Set("Cache-Control", "no-store")
			s.renderPartial(w, r, "templates/partials/bead_preview.html", "bead_preview_notfound", map[string]any{
				"ID": name,
			})
			return
		}
		s.logger.Error("resolving agent for preview", "name", name, "error", err)
		http.Error(w, "preview unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Cache-Control", "private, max-age=30")
	s.renderPartial(w, r, "templates/partials/bead_preview.html", "bead_preview", map[string]any{
		"Bead":    bead,
		"Excerpt": s.excerpt(bead.Description, previewExcerptRunes),
	})
}
