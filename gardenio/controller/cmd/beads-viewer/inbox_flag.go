package main

import (
	"context"
	"fmt"
	"net/http"
	"slices"

	"gardenio/controller/internal/beadsapi"
)

// labelInboxWorthy nominates a bead for the operator's inbox even when its
// category is not `intake`. The inbox query unions this label with the
// category-based predicate so an operator can hand-promote an action bead
// without retagging its category. See ka-0000007385.
const labelInboxWorthy = "flag:inbox-worthy"

// labelInboxPinned pins an inbox row to the top of the rail. The simplest
// promote/demote mechanic: a single boolean label sorted before priority.
const labelInboxPinned = "flag:inbox-pinned"

// hasLabel reports whether the bead carries the given label.
func hasLabel(bead *beadsapi.BeadDetail, label string) bool {
	if bead == nil {
		return false
	}
	return slices.Contains(bead.Labels, label)
}

// toggleLabel adds or removes label from beadID based on whether bead
// currently carries it. Returns the action taken ("added" / "removed") for
// logging.
func (s *Server) toggleLabel(ctx context.Context, bead *beadsapi.BeadDetail, label string) error {
	if hasLabel(bead, label) {
		return s.daemon.RemoveLabel(ctx, bead.ID, label)
	}
	return s.daemon.AddLabel(ctx, bead.ID, label)
}

// handleToggleInboxWorthy flips the `flag:inbox-worthy` label on the bead.
// Re-renders the bead-actions toolbar (or the active-row partial when
// ?return=row) so htmx swaps update without a full page reload.
func (s *Server) handleToggleInboxWorthy(w http.ResponseWriter, r *http.Request) {
	s.runBeadAction(w, r, func(ctx context.Context, id, _ string) error {
		bead, err := s.daemon.GetBead(ctx, id)
		if err != nil {
			return err
		}
		return s.toggleLabel(ctx, bead, labelInboxWorthy)
	}, "Failed to toggle inbox-worthy flag.")
}

// handleToggleInboxPinned adds or removes the bead from the viewer's
// profile_prefs.PinnedBeadIDs, surfacing (or hiding) it in their Home
// (/u/{slug}#pinned) Pinned section. The button label says "Pin"/"Unpin"
// and its state reflects the viewer's pinned set, not any label on the bead.
//
// The historical `flag:inbox-pinned` label remains a separate concern: it
// reorders the inbox rail (see inbox.go / inbox_stream.go) and is no longer
// touched by this button. runBeadAction re-renders the toolbar after the
// mutation; buildBeadActions reads viewerPinnedSet fresh, so the swapped
// partial reflects the new pinned state.
func (s *Server) handleToggleInboxPinned(w http.ResponseWriter, r *http.Request) {
	s.runBeadAction(w, r, func(ctx context.Context, id, email string) error {
		return s.toggleViewerPin(ctx, email, id)
	}, "Failed to toggle pin.")
}

// toggleViewerPin adds beadID to the viewer's profile_prefs.PinnedBeadIDs
// when absent, or removes it when present. The viewer is resolved by their
// authenticated email; writes go only to the `profile_prefs` field via
// UpdateBeadFieldsTyped, leaving the rest of the user bead untouched.
func (s *Server) toggleViewerPin(ctx context.Context, email, beadID string) error {
	bead, err := s.findUserBeadByEmail(ctx, email)
	if err != nil {
		return err
	}
	if bead == nil {
		return fmt.Errorf("no user bead for %s", email)
	}

	prefs := parseProfilePref(bead.Fields)
	if idx := slices.Index(prefs.PinnedBeadIDs, beadID); idx >= 0 {
		prefs.PinnedBeadIDs = slices.Delete(prefs.PinnedBeadIDs, idx, idx+1)
	} else {
		prefs.PinnedBeadIDs = append(prefs.PinnedBeadIDs, beadID)
	}

	prefs = normalizeProfilePref(prefs)
	profileJSON, err := encodeProfilePref(prefs)
	if err != nil {
		return err
	}
	return s.daemon.UpdateBeadFieldsTyped(ctx, bead.ID, map[string]any{
		"profile_prefs": profileJSON,
	})
}

// viewerPinnedSet returns the set of bead ids the viewer has pinned to their
// Home page, keyed for O(1) membership tests by buildBeadActions. A missing
// viewer, lookup error, or empty pref list yields a nil set — the Pin button
// then renders in its default ("Pin") state rather than failing the render.
func (s *Server) viewerPinnedSet(ctx context.Context, email string) map[string]struct{} {
	if email == "" {
		return nil
	}
	bead, err := s.findUserBeadByEmail(ctx, email)
	if err != nil {
		s.logger.Warn("viewer pinned set: looking up user bead", "email", email, "error", err)
		return nil
	}
	if bead == nil {
		return nil
	}
	prefs := parseProfilePref(bead.Fields)
	if len(prefs.PinnedBeadIDs) == 0 {
		return nil
	}
	set := make(map[string]struct{}, len(prefs.PinnedBeadIDs))
	for _, id := range prefs.PinnedBeadIDs {
		set[id] = struct{}{}
	}
	return set
}
