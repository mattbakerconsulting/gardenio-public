package main

// Gardener firing-mode toggle (Task 7, epic ka-0000006020; spec
// docs/superpowers/specs/2026-07-13-gardener-mode-flip-design.md §Component 3).
//
// POST /gardener/{id}/mode with form value mode=timed|event flips a gardener
// trigger bead between clock-fired (trigger:schedule + cron) and event-fired
// (trigger:multi + subtree bead events + a slow fallback-cron watchdog). The
// whole bead rewrite — ordered writes that are the double-fire guard, the audit
// comment, the timed_cron backup/restore — lives in gardenermode.Set; this
// handler is only the HTTP shell: auth, input validation, mode detection, and
// re-rendering the operator control bar so the toggle swaps in place exactly
// like enable/disable does.
//
// Operator-gated at the route layer (server.go registers it behind
// operatorOnly), so read-scoped viewers see the current mode but no lever.

import (
	"net/http"
	"strings"

	"gardenio/controller/internal/gardenermode"
	"gardenio/controller/internal/oidcauth"
)

// handleGardenerModeToggle flips a gardener trigger bead's firing mode. It reads
// the target `mode` form value (rejecting anything but timed/event with 400),
// loads + validates the gardener bead, detects its current mode (an ambiguous
// both-/neither-label state is a 409 with the detector's message and zero
// writes), then delegates the bead rewrite to gardenermode.Set with the
// operator as the audit-comment author. A no-op flip (already in the requested
// mode) still re-renders the bar at 200 so the htmx swap succeeds.
func (s *Server) handleGardenerModeToggle(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	email := oidcauth.EmailFromContext(r.Context())
	if email == "" {
		http.Error(w, "Unauthenticated", http.StatusUnauthorized)
		return
	}

	// Parse + validate the target mode BEFORE touching the daemon — a bad value
	// is a client error, not a state we should load a bead to discover.
	var mode gardenermode.Mode
	switch strings.TrimSpace(r.FormValue("mode")) {
	case string(gardenermode.ModeTimed):
		mode = gardenermode.ModeTimed
	case string(gardenermode.ModeEvent):
		mode = gardenermode.ModeEvent
	default:
		http.Error(w, "mode must be timed or event", http.StatusBadRequest)
		return
	}

	bead, ok := s.loadGardenerTrigger(w, r, id)
	if !ok {
		return
	}
	if auditProbeDryRun(r) {
		writeAuditProbeAck(w, "mode")
		return
	}

	// An ambiguous label state (both or neither trigger label) has no
	// well-defined current mode — reject with 409 before any write, matching
	// Detect's contract. Surfacing the detector's own message keeps the error
	// text honest about which invariant the bead violates.
	if _, err := gardenermode.Detect(bead); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}

	// The *beadsapi.Client behind s.daemon satisfies gardenermode.Writer
	// (UpdateBeadFieldsTyped / AddLabel / RemoveLabel / AddComment). Set owns the
	// ordered writes + audit; we pass the canonical operator id as the author so
	// the audit comment matches the fleet's operator identity.
	if _, err := gardenermode.Set(r.Context(), s.daemon, bead, mode, gardenermode.Opts{
		Operator: canonicalOIDC(email),
	}); err != nil {
		s.logger.Error("gardener mode flip", "id", id, "actor", email, "mode", mode, "error", err)
		http.Error(w, "Failed to flip gardener mode.", http.StatusBadGateway)
		return
	}

	s.rerenderGardenerControls(w, r, id)
}
