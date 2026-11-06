package server

import (
	"context"
	"encoding/json"
	"slices"
	"strings"

	"gardenio/kbeads/internal/model"
	"gardenio/kbeads/internal/store"
)

// The steward's memory, the steward-notes:<land> doc bead (steward amendment
// 2026-10-23 §3, step 9.2a gd-mN1TLhGpGe). Two narrow rights on it:
//
//   - The land's people (owner and member) read the whole bead and may edit
//     its Holds section only (owner Q3): a patch that changes nothing but
//     fields.content, and leaves every line outside "## Holds" as it was
//     (scopedActionEditHolds, scopedHoldsDenial). Editing someone else's
//     notes is the point, so the ownership gate is skipped as for a join.
//   - The land's steward, a new agent every run, writes the bead it never
//     created: a lands-tenant steward agent may update and comment on the
//     steward-notes bead of its own project, and no other doc
//     (stewardOwnsNotes, read by authorizeOwnership).
//
// The controller's twin is gardenio/controller/internal/steward (notes.go),
// which parses the same "## " sections.

// scopedActionEditHolds is the scoped-gate action for a content-only patch of
// a land's steward-notes bead. Only updateBead produces it
// (scopedUpdateAction).
const scopedActionEditHolds = "edit_holds"

const (
	typeDoc model.BeadType = "doc"
	// stewardNotesTitlePrefix + <land> is the notes bead's title; the
	// controller's steward.NotesTitle.
	stewardNotesTitlePrefix = "steward-notes:"
	// notesHoldsHeading is the Holds section's heading line.
	notesHoldsHeading = "## Holds"
)

// stewardNotesLand is the land a bead is the steward-notes bead of: a doc
// titled steward-notes:<land> that carries project:<land>. "" otherwise.
func stewardNotesLand(bead *model.Bead) string {
	if bead == nil || bead.Type != typeDoc {
		return ""
	}
	land, ok := strings.CutPrefix(bead.Title, stewardNotesTitlePrefix)
	if !ok || land == "" || !slices.Contains(bead.Labels, "project:"+land) {
		return ""
	}
	return land
}

// notesContentOnly reports whether a patch on a steward-notes bead changes
// fields.content and nothing else. bead must be the pre-patch state.
func notesContentOnly(bead *model.Bead, in *updateBeadInput) bool {
	return stewardNotesLand(bead) != "" && patchOnlyChangesFields([]string{"content"}, in, bead.Fields)
}

// splitHolds cuts notes content at its one "## Holds" section: the text
// before the heading, the section (the heading line up to the next "## "
// heading), and the rest. ok is false when there is not exactly one Holds
// heading.
func splitHolds(content string) (before, holds, after string, ok bool) {
	lines := strings.SplitAfter(content, "\n")
	start, end := -1, len(lines)
	for i, l := range lines {
		head := strings.TrimRight(l, " \t\r\n")
		if !strings.HasPrefix(head, "## ") {
			continue
		}
		switch {
		case head == notesHoldsHeading || strings.HasPrefix(head, notesHoldsHeading+" "):
			if start >= 0 {
				return "", "", "", false
			}
			start = i
		case start >= 0 && end == len(lines):
			end = i
		}
	}
	if start < 0 {
		return "", "", "", false
	}
	return strings.Join(lines[:start], ""), strings.Join(lines[start:end], ""), strings.Join(lines[end:], ""), true
}

// scopedHoldsDenial checks a scoped principal's content-only patch of a
// steward-notes bead: everything outside the Holds section must be byte for
// byte what is stored, so a person can say "never merge PR 12" but cannot
// rewrite the steward's answers, promises or what it has in flight. A patch
// built from a stale read lands here too, as a deny that says to re-read.
func scopedHoldsDenial(bead *model.Bead, patch json.RawMessage) *AuthzDenial {
	deny := func(msg string) *AuthzDenial {
		return &AuthzDenial{Action: scopedActionEditHolds, BeadType: string(bead.Type), Message: msg}
	}
	var p struct {
		Content *string `json:"content"`
	}
	if err := json.Unmarshal(patch, &p); err != nil || p.Content == nil {
		return deny("fields.content must be the notes as a string")
	}
	stored := parseFieldsToStringMap(bead.Fields)["content"]
	sb, _, sa, ok := splitHolds(stored)
	if !ok {
		return deny("the stored notes have no single " + notesHoldsHeading + " section; ask the land's steward or the operator in #support")
	}
	nb, _, na, ok := splitHolds(*p.Content)
	if !ok {
		return deny("the notes must keep exactly one " + notesHoldsHeading + " section")
	}
	if nb != sb || na != sa {
		return deny("a person may edit only the " + notesHoldsHeading + " section of " + bead.Title + "; the rest is the steward's. Re-read the notes and change only Holds")
	}
	return nil
}

// stewardOwnsNotes reports whether the caller is the lands-tenant steward of
// the land whose steward-notes bead this is. A tier lookup failure is false:
// the ordinary ownership gate then decides.
func stewardOwnsNotes(ctx context.Context, s store.Store, action string, bead *model.Bead) bool {
	if action != "update" && action != "comment" {
		return false
	}
	land := stewardNotesLand(bead)
	if land == "" {
		return false
	}
	caller, err := landsTenantCallerFrom(ctx, s)
	if err != nil || caller == nil || !caller.isSteward() {
		return false
	}
	return caller.Project == land
}
