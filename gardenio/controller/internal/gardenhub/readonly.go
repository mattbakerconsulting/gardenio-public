package gardenhub

import "net/http"

// readOnly reports whether the requester sees /garden read-only (Handler.ReadOnly, gd-GuZ3tbnTFI).
func (h *Handler) readOnly(r *http.Request) bool { return h.ReadOnly != nil && h.ReadOnly(r) }

// can reports whether hook admits the requester to a write the page offers. A read-only viewer is never admitted: what
// the page offers follows the write routes, and every one of them is admin-only for a project-scoped human, so a button
// shown there could only fail.
func (h *Handler) can(r *http.Request, hook func(*http.Request) bool) bool {
	return hook != nil && !h.readOnly(r) && hook(r)
}
