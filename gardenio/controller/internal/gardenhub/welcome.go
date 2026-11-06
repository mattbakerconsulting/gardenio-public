package gardenhub

import (
	"encoding/json"
	"net/http"
)

// The welcome tour (Lands Track B step 9.14b, gd-aEIUgpyxpi; decision D3 gd-NLc4LzmCAE, on-claim-and-tour). The first
// time a land's person opens /garden, the page walks them round once: their land on the map, how to ask, and its
// #general, where the land's steward greeted them by name when they were let in (step 9.14); the run ends there. web/tour.js runs
// it on the chore tour's card (TOUR.welcome); the page carries body data-welcome (the land), data-welcome-role (their
// role there) and data-welcome-gate (it is the gatehouse) only while it is due.
//
// Whether it is due is the person's, not the browser's: kbeads keeps welcome_tour_seen_at on their own user bead
// (/v1/users/me/welcome-tour), so a second device does not run it again. The page reads it (Handler.Welcome) and, when
// the tour finishes or is dismissed, posts POST /garden/welcome, which stamps it (Handler.WelcomeSeen). Both go to the
// daemon on the person's own bearer; beads-viewer classes the route scoped, since a land's person is a scoped principal.

// WelcomeTour is the welcome tour a page is due: the land it shows the person round, their role there (owner, member
// or guest), and whether it is the gatehouse, where everyone with no land of their own starts as a guest (step 9.12).
// The zero value is none due.
type WelcomeTour struct {
	Land string
	Role string
	Gate bool
}

// welcome is the tour r's page is due; none without the hook.
func (h *Handler) welcome(r *http.Request) WelcomeTour {
	if h.Welcome == nil {
		return WelcomeTour{}
	}
	return h.Welcome(r)
}

// welcomeSeen answers POST /garden/welcome: the requester has had the welcome tour, so no page of theirs runs it again.
// JSON {"ok":true}, or {"ok":false,"error":...} with 503 when the page cannot record it (the tour then runs again on
// their next visit, which is the safe side).
func (h *Handler) welcomeSeen(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if h.WelcomeSeen == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "The welcome tour cannot be recorded here."})
		return
	}
	if err := h.WelcomeSeen(r); err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "The welcome tour could not be recorded; it will run again next time."})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}
