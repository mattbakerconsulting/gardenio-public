package main

import (
	"context"
	"net/http"
	"strings"

	"gardenio/controller/internal/beadsapi"
)

// heraldry_edit.go — POST /b/{id}/heraldry: the Workbench's Change arms
// (heraldry plan gd-N6I3Oa7uDg step 3.2; spec gardenio/docs/superpowers/specs/
// 2026-10-25-heraldry-design.md §7). Agents pick arms (R7); this is the
// correction an operator makes when they dislike what an agent chose. The form
// field heraldry is the arms in any form the daemon accepts (the control sends
// the canonical form), or "none" to remove them. The daemon is the one that
// checks them: the type may bear arms (R6), each emoji is in the library and
// on the shield once (R11), and no other bead of the project bears the same
// arms (R5); its refusal comes back in its own words.

// maxHeraldryForm bounds the posted value: five emoji of up to a few dozen
// bytes each (a ZWJ sequence), a colour and place names, with room to spare.
const maxHeraldryForm = 400

func (s *Server) handleUpdateHeraldry(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad form", http.StatusBadRequest)
		return
	}
	arms := strings.TrimSpace(r.PostForm.Get("heraldry"))
	switch {
	case arms == "":
		http.Error(w, `Give the arms, or "none" to remove them.`, http.StatusBadRequest)
		return
	case len(arms) > maxHeraldryForm:
		http.Error(w, "Those arms are too long.", http.StatusRequestEntityTooLarge)
		return
	}
	s.runBeadAction(w, r, func(ctx context.Context, id, _ string) error {
		return s.daemon.UpdateBead(ctx, id, beadsapi.UpdateBeadRequest{Heraldry: &arms})
	}, "Could not change the arms.")
}
