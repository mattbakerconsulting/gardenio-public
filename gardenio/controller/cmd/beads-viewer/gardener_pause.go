package main

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/oidcauth"
)

// gardenerPauseView is the minimal data the inline /gardeners pause/resume toggle
// needs. A gardener is a schedule / trigger:schedule bead under the hood, so
// "pause" is just `enabled: false` — but operators on the /gardeners page think
// in terms of *pausing a gardener*, not *disabling a trigger*, so the toggle
// wears that vocabulary. Paused is the inverse of the bead's effective
// `enabled` state (computed via buildTriggerRow so both enabled-default
// conventions resolve in one place).
//
// This is the list-row sibling of the detail page's gardener_controls bar
// (gardener_ops.go): same `enabled` field, same reconcile + audit semantics,
// but a single compact in-place toggle instead of the full lifecycle bar.
type gardenerPauseView struct {
	ID       string
	Paused   bool
	IsClosed bool
}

// gardenerPauseViewFromBead projects a gardener bead into the toggle view-model.
// It reuses buildTriggerRow's enabled derivation so the modern trigger:schedule
// default-on and legacy schedule default-off conventions both resolve correctly.
func (s *Server) gardenerPauseViewFromBead(b *beadsapi.BeadDetail) gardenerPauseView {
	row := buildTriggerRow(b, s.now())
	return gardenerPauseView{
		ID:       b.ID,
		Paused:   !row.Enabled,
		IsClosed: row.IsClosed,
	}
}

// handleGardenerPauseToggle flips the `enabled` field on a gardener bead from the
// /gardeners list — a paused gardener resumes (enabled:true), a running one
// pauses (enabled:false) — and re-renders the compact inline toggle so htmx can
// swap it in place. It is the list-row counterpart to handleGardenerEnable /
// handleGardenerDisable (gardener_ops.go), which re-render the detail page's
// control bar instead.
//
// We always write an explicit value to satisfy both loader conventions
// (trigger:schedule enabled-unless-"false"; legacy schedule
// enabled-iff-"true"). Both LoadGardeners paths re-read `enabled` every reconcile
// tick, so the flip propagates without a controller restart.
func (s *Server) handleGardenerPauseToggle(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	email := oidcauth.EmailFromContext(r.Context())
	if email == "" {
		http.Error(w, "Unauthenticated", http.StatusUnauthorized)
		return
	}

	bead, ok := s.loadGardenerTrigger(w, r, id)
	if !ok {
		return
	}
	if auditProbeDryRun(r) {
		writeAuditProbeAck(w, "pause-toggle")
		return
	}

	// Closed (retired) gardeners don't render the toggle; reject a stray POST
	// rather than silently re-enabling a parked gardener.
	if statusOpenSortKey(bead.Status) != 0 {
		http.Error(w, "Gardener is closed", http.StatusConflict)
		return
	}

	view := s.gardenerPauseViewFromBead(bead)

	// `enabled` is boolean in the type:trigger/type:schedule schema — a JSON
	// bool is required; the string "true"/"false" is 400-rejected by the
	// daemon's field validation (ka-0000003627).
	enable := view.Paused
	next := strconv.FormatBool(enable)
	if err := s.daemon.UpdateBeadFieldsTyped(r.Context(), id, map[string]any{"enabled": enable}); err != nil {
		s.logger.Error("gardener pause toggle", "id", id, "actor", email, "next", next, "error", err)
		http.Error(w, "Failed to update gardener.", http.StatusBadGateway)
		return
	}

	// Best-effort audit comment, phrased in pause vocabulary so the bead history
	// reads the way the operator acted. Mirrors the enable/disable audit policy
	// (ka-0000001354); mutation_history is the canonical record either way.
	action := "paused"
	if next == "true" {
		action = "resumed"
	}
	if err := s.daemon.AddComment(r.Context(), id, email, fmt.Sprintf("gardener %s by %s", action, email)); err != nil {
		s.logger.Warn("gardener pause audit comment", "id", id, "actor", email, "action", action, "error", err)
	}

	updated, err := s.daemon.GetBead(r.Context(), id)
	if err != nil {
		s.logger.Warn("re-fetching gardener after pause toggle", "id", id, "error", err)
		fallback := *bead
		if fallback.Fields == nil {
			fallback.Fields = map[string]string{}
		}
		fallback.Fields["enabled"] = next
		updated = &fallback
	}
	s.renderGardenerPauseToggle(w, r, updated, http.StatusOK)
}

// renderGardenerPauseToggle writes the inline pause/resume toggle partial for one
// gardener row. Called by handleGardenerPauseToggle (POST response) and by the
// /gardeners render via the template (which embeds the partial per row).
func (s *Server) renderGardenerPauseToggle(w http.ResponseWriter, r *http.Request, bead *beadsapi.BeadDetail, status int) {
	view := s.gardenerPauseViewFromBead(bead)
	if status != http.StatusOK {
		w.WriteHeader(status)
	}
	s.renderPartial(w, r, "templates/partials/gardener_pause_toggle.html", "gardener_pause_toggle", map[string]any{
		"Pause": view,
	})
}
