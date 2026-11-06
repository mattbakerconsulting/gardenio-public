package beadsapi

import (
	"context"
	"net/http"
	"strings"
	"time"
)

// BeadListObserver is told the wall time and outcome of one GET /v1/beads
// call, retries included. It runs on the caller's goroutine with the caller's
// ctx, after the response is decoded, so it must be cheap and must not call
// the client.
type BeadListObserver func(ctx context.Context, took time.Duration, err error)

// SetOnBeadList installs fn as the observer of every bead-list call this
// client makes (ListBeadsFiltered and the typed list helpers). The
// beads-viewer uses it to time its lists per call site, which the daemon
// cannot do: every viewer call reaches it as the same caller
// (gd-XOi5cqZP3I). Pass nil to clear. Not safe to call concurrently with
// requests; set it once at startup.
func (c *Client) SetOnBeadList(fn BeadListObserver) {
	c.onBeadList = fn
}

// isBeadListCall reports whether method and path are the daemon's bead list,
// GET /v1/beads with or without a query, and not a /v1/beads/... subroute.
func isBeadListCall(method, path string) bool {
	if method != http.MethodGet {
		return false
	}
	rest, ok := strings.CutPrefix(path, "/v1/beads")
	return ok && (rest == "" || rest[0] == '?')
}
