package healthsnapshot

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"
)

// DefaultCacheTTL is the server-side cache TTL for snapshot responses.
// Sized to match the fleet-collector aggregation TTL so opening the health
// page repeatedly costs ~nothing.
const DefaultCacheTTL = 10 * time.Second

// Handler caches Snapshot builds for cacheTTL and serves them as JSON.
type Handler struct {
	snap     *Snapshotter
	cacheTTL time.Duration

	mu       sync.Mutex
	cached   *Snapshot
	cachedAt time.Time
	now      func() time.Time
}

// NewHandler returns an HTTP handler for GET /healthz/snapshot.
// cacheTTL of 0 falls back to DefaultCacheTTL.
func NewHandler(snap *Snapshotter, cacheTTL time.Duration) *Handler {
	if cacheTTL <= 0 {
		cacheTTL = DefaultCacheTTL
	}
	return &Handler{snap: snap, cacheTTL: cacheTTL, now: time.Now}
}

// ServeHTTP returns the cached or freshly-built snapshot as JSON.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	snap := h.snapshot(r.Context())
	w.Header().Set("Content-Type", "application/json")
	// 10s cache header lets intermediate caches (e.g. the htmx fetch layer)
	// avoid hammering the controller; matches the server-side TTL.
	w.Header().Set("Cache-Control", "public, max-age=10")
	if err := json.NewEncoder(w).Encode(snap); err != nil {
		// Header already written; nothing useful to do but log via the
		// transport. Body will be truncated.
		return
	}
}

func (h *Handler) snapshot(ctx context.Context) Snapshot {
	h.mu.Lock()
	now := h.now()
	if h.cached != nil && now.Sub(h.cachedAt) < h.cacheTTL {
		cp := *h.cached
		h.mu.Unlock()
		return cp
	}
	h.mu.Unlock()

	// Build outside the lock so a slow K8s API call doesn't block readers.
	fresh := h.snap.Build(ctx)

	h.mu.Lock()
	h.cached = &fresh
	h.cachedAt = h.now()
	h.mu.Unlock()
	return fresh
}
