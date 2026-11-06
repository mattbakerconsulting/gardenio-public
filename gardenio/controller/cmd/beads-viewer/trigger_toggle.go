package main

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/oidcauth"
)

// handleToggleTriggerEnabled flips the `enabled` field on a schedule bead, a
// trigger:schedule bead, or an event-mode gardener (trigger:multi, see
// triggerToggleTarget). Re-renders the per-row toggle partial so htmx can
// swap it in place without a full page reload.
//
// Both LoadTriggers paths re-read the `enabled` field from the daemon on every
// reconcile tick, so a field flip propagates without a controller restart. We
// always write an explicit value to match the two loader conventions:
//
//   - type=schedule:        enabled iff f["enabled"] == "true"
//   - type=trigger label=…: enabled unless f["enabled"] == "false"
//
// Writing explicitly in both directions sidesteps the asymmetric default and
// stays a no-op for the loader contracts. The write must be a JSON bool —
// `enabled` is boolean in the type:trigger/type:schedule schema and the string
// "true"/"false" is 400-rejected by the daemon (ka-0000003627).
//
// After the flip we post a one-line audit comment to the bead so the human-
// visible history matches mutation_history: `enabled → <new> by <operator>`.
func (s *Server) handleToggleTriggerEnabled(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "Missing bead id", http.StatusBadRequest)
		return
	}
	email := oidcauth.EmailFromContext(r.Context())
	if email == "" {
		http.Error(w, "Unauthenticated", http.StatusUnauthorized)
		return
	}

	bead, err := s.daemon.GetBead(r.Context(), id)
	if err != nil {
		s.triggerToggleLoadError(w, id, err)
		return
	}

	// Guard: only schedule beads, trigger:schedule beads and event-mode
	// (trigger:multi) gardeners are valid targets — flipping `enabled` on a
	// task bead would be a no-op for any loader but still mutates state, which
	// we don't want from the /triggers page.
	if !triggerToggleTarget(bead) {
		http.Error(w, "Not a schedule, trigger:schedule or event-mode gardener bead", http.StatusBadRequest)
		return
	}

	// Closed beads don't render the toggle; reject a stray POST against one
	// rather than silently flipping a parked bead.
	if statusOpenSortKey(bead.Status) != 0 {
		http.Error(w, "Bead is closed", http.StatusConflict)
		return
	}

	row := buildTriggerRow(bead, s.now())

	enable := !row.Enabled
	next := strconv.FormatBool(enable)
	if err := s.daemon.UpdateBeadFieldsTyped(r.Context(), id, map[string]any{"enabled": enable}); err != nil {
		s.logger.Error("toggling enabled", "id", id, "actor", email, "next", next, "error", err)
		http.Error(w, "Failed to update enabled.", http.StatusBadGateway)
		return
	}

	// Audit comment matches the policy in ka-0000001354. Best-effort: log on
	// failure but still return the re-rendered toggle so the operator sees the
	// state change. mutation_history is the canonical record either way.
	auditText := fmt.Sprintf("enabled → %s by %s", next, email)
	if err := s.daemon.AddComment(r.Context(), id, email, auditText); err != nil {
		s.logger.Warn("posting enabled-toggle audit comment", "id", id, "actor", email, "next", next, "error", err)
	}

	updated, err := s.daemon.GetBead(r.Context(), id)
	if err != nil {
		s.logger.Warn("re-fetching bead after enabled toggle", "id", id, "error", err)
		fallback := *bead
		if fallback.Fields == nil {
			fallback.Fields = map[string]string{}
		}
		fallback.Fields["enabled"] = next
		updated = &fallback
	}
	s.renderTriggerToggle(w, r, updated, http.StatusOK)
}

// triggerToggleTarget reports whether the /triggers enable toggles (single and
// bulk) may flip `enabled` on b. Accepted:
//
//   - type=schedule (legacy schedules);
//   - type=trigger labelled trigger:schedule (timed triggers and timed
//     gardeners);
//   - type=trigger labelled trigger:multi that is a gardener (gd-fL4PM5z8kc).
//     Event mode (gardenermode.Set: trigger:multi, no cron, a bead_event entry
//     plus an optional fallback-watchdog cron entry) is now the default for plan
//     gardeners, so without this the toggle 400'd on every one of them. The
//     gardener test is isGardenerBead, the SAME predicate that admits a bead to
//     the /gardener/{id} enable/disable bar, the /gardeners pause toggle and
//     (through the plan's bound gardener) the /plans cockpit toggle — so this
//     admits no bead an operator could not already pause or resume there. A
//     trigger:multi bead that is not a gardener (no role:gardener label, no
//     gardener blob) is still refused, like every other non-schedule source.
//
// The write itself is unchanged and is the same patch setGardenerEnabledCore
// sends: a lone JSON-bool `enabled` on the signed-in operator's request (the
// routes are routeAdminOnly). `enabled` is not a spawn-lineage digest key
// (kbeads spawn_lineage_seal.go spawnLineageDigestKeys), so a disable leaves the
// seal valid, and an OIDC gardenio-admin re-enable keeps (re-stamps) it rather
// than tripping voidSpawnLineageOnReactivation. No reseal step is needed here.
func triggerToggleTarget(b *beadsapi.BeadDetail) bool {
	if b == nil {
		return false
	}
	switch {
	case b.Type == "schedule":
		return true
	case b.Type == "trigger" && hasLabel(b, "trigger:schedule"):
		return true
	case b.Type == "trigger" && hasLabel(b, "trigger:multi"):
		return isGardenerBead(b)
	}
	return false
}

// renderTriggerToggle writes the inline enable/disable toggle partial for one
// trigger row. Called both by handleToggleTriggerEnabled (POST response) and
// by /triggers render via the template helper.
func (s *Server) renderTriggerToggle(w http.ResponseWriter, r *http.Request, bead *beadsapi.BeadDetail, status int) {
	row := buildTriggerRow(bead, s.now())
	if status != http.StatusOK {
		w.WriteHeader(status)
	}
	s.renderPartial(w, r, "templates/partials/trigger_toggle.html", "trigger_toggle", map[string]any{
		"Row": row,
	})
}

// triggerToggleLoadError mirrors the assignee/labels editor error mapping.
func (s *Server) triggerToggleLoadError(w http.ResponseWriter, id string, err error) {
	var apiErr *beadsapi.APIError
	if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
		http.Error(w, "Bead not found", http.StatusNotFound)
		return
	}
	s.logger.Error("getting bead for enabled toggle", "id", id, "error", err)
	http.Error(w, "Failed to load bead", http.StatusInternalServerError)
}
