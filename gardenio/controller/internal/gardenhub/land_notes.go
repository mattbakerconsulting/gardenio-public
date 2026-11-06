package gardenhub

// land_notes.go — a land's steward notes (gd-q4NgYNGpy8, owner Q3 of 9.2a gd-mN1TLhGpGe), edited on the workbench's steward
// view (web/commons2.js, gd-LisW87zrnf): the land's steward-notes:<land> doc bead, read whole by the land's people, its
// Holds section edited by an owner or a member.
// GET /garden/notes/{land} answers the notes; POST /garden/notes/{land}/holds takes {"holds", "was"}, re-reads the notes,
// splices the new Holds body between the stored text before and after it and patches fields.content alone, so the
// steward's own end-of-run write is never undone. A Holds that moved since the editor opened is a 409 with the notes as
// they stand. Both are routeScoped in beads-viewer (scope before any read, 404 outside it), go to the daemon on the
// viewer's bearer, and kbeads checks the write again (edit_holds, steward_notes_authz.go). A person's edit wakes the steward.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/grafanaembed"
)

// NotesStore reads and writes a land's steward notes on the viewer's own bearer.
type NotesStore interface {
	notesLister
	UpdateBeadFields(ctx context.Context, beadID string, fields map[string]string) error
}

type notesLister interface {
	ListBeadsFiltered(ctx context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error)
}

const (
	notesHoldsHeading = "## Holds" // kbeads' notesHoldsHeading: the one section a person edits
	notesHoldsMax     = 4096       // a saved Holds body; the whole notes are held to steward.NotesCapBytes too (NotesOver)
)

// landNotesDoc is GET /garden/notes/{land}'s answer, and a save's.
type landNotesDoc struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Land    string `json:"land"`
	Content string `json:"content"`  // the whole notes, markdown
	Holds   string `json:"holds"`    // the Holds section's body
	HoldsOK bool   `json:"holds_ok"` // false without one single "## Holds" section, which no person's edit can pass
	Edit    bool   `json:"edit"`     // an owner or member of the land, never a guest (MayEditHolds); kbeads checks it again
	Saved   bool   `json:"saved,omitempty"`
	Error   string `json:"error,omitempty"` // why a save was refused (a 409), beside the notes as they stand
}

// splitNotesHolds cuts notes at their one Holds section by kbeads' splitHolds rule: only a line starting "## " is a
// heading, the Holds heading is "## Holds" alone or with more words after a space, and the section runs to the next
// heading. heading is the Holds line with its line end. ok is false without exactly one Holds heading.
func splitNotesHolds(content string) (before, heading, body, after string, ok bool) {
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
				return "", "", "", "", false
			}
			start = i
		case start >= 0 && end == len(lines):
			end = i
		}
	}
	if start < 0 {
		return "", "", "", "", false
	}
	return strings.Join(lines[:start], ""), lines[start], strings.Join(lines[start+1:end], ""), strings.Join(lines[end:], ""), true
}

// holdsText is a Holds body as the editor shows and compares it: \n line ends, no blank lines around it.
func holdsText(body string) string {
	return strings.Trim(strings.ReplaceAll(body, "\r\n", "\n"), "\n")
}

// spliceHolds is content with its Holds body replaced by holds, everything else byte for byte as stored, the body one
// block with a blank line before the next section (the steward's layout). ok is false without one single Holds section.
func spliceHolds(content, holds string) (string, bool) {
	before, heading, _, after, ok := splitNotesHolds(content)
	if !ok {
		return "", false
	}
	if !strings.HasSuffix(heading, "\n") {
		heading += "\n" // the notes ended on the heading
	}
	if h := holdsText(holds); h != "" {
		heading += h + "\n"
	}
	if after != "" {
		heading += "\n"
	}
	return before + heading + after, true
}

// findLandNotes is the land's steward-notes:<land> doc, the way steward's FindNotes reads it (one list by title and
// the land's label), or nil.
func findLandNotes(ctx context.Context, src notesLister, land string) (*beadsapi.BeadDetail, error) {
	title := StewardNotesPrefix + land
	res, err := src.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Labels: []string{"project:" + land}, Statuses: stewardNoteStatuses, Search: title,
	})
	if err != nil {
		return nil, err
	}
	for _, b := range res.Beads {
		if b != nil && b.Title == title {
			return b, nil
		}
	}
	return nil, nil
}

func notesDocOf(b *beadsapi.BeadDetail, land string, edit bool) landNotesDoc {
	// A notes bead written before docs kept a body has it in its description (steward.NotesBody).
	d := landNotesDoc{ID: b.ID, Title: b.Title, Land: land, Content: firstNonEmpty(b.Fields["content"], b.Description), Edit: edit}
	if _, _, body, _, ok := splitNotesHolds(b.Fields["content"]); ok {
		d.Holds, d.HoldsOK = holdsText(body), true
	}
	return d
}

// notesLand is the request's land when the viewer's scope admits it and notes are served, else "" with the answer
// written: 404 outside the scope, as for a land that does not exist.
func (h *Handler) notesLand(w http.ResponseWriter, r *http.Request) string {
	land := r.PathValue("land")
	switch {
	case !grafanaembed.ValidProject(land) || h.InLand == nil || !h.InLand(r, land):
		http.Error(w, "no such land", http.StatusNotFound)
	case h.Notes == nil:
		http.Error(w, "steward notes not enabled", http.StatusServiceUnavailable)
	default:
		return land
	}
	return ""
}

// readLandNotes is the land's notes bead, or nil with the answer written.
func (h *Handler) readLandNotes(ctx context.Context, w http.ResponseWriter, land string) *beadsapi.BeadDetail {
	b, err := findLandNotes(ctx, h.Notes, land)
	if err != nil {
		http.Error(w, "the beads daemon did not answer", http.StatusBadGateway)
	} else if b == nil {
		http.Error(w, "this land has no steward notes yet", http.StatusNotFound)
	}
	return b
}

func (h *Handler) mayEditHolds(r *http.Request, land string) bool {
	return h.MayEditHolds != nil && h.MayEditHolds(r, land)
}

// landNotes serves GET /garden/notes/{land}.
func (h *Handler) landNotes(w http.ResponseWriter, r *http.Request) {
	land := h.notesLand(w, r)
	if land == "" {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), beadViewTimeout)
	defer cancel()
	if b := h.readLandNotes(ctx, w, land); b != nil {
		writeDoc(w, "kbeads", notesDocOf(b, land, h.mayEditHolds(r, land)))
	}
}

// saveLandHolds serves POST /garden/notes/{land}/holds.
func (h *Handler) saveLandHolds(w http.ResponseWriter, r *http.Request) {
	land := h.notesLand(w, r)
	var in struct{ Holds, Was *string }
	switch {
	case land == "":
		return
	// A JSON body is a fetch from the page: a cross-site form cannot send one, and a browser saying another origin is refused.
	case crossOrigin(r) || !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json"):
		http.Error(w, "send the Holds as JSON from the garden page", http.StatusForbidden)
		return
	case !h.mayEditHolds(r, land):
		http.Error(w, "only the land's owner and members may edit its Holds", http.StatusForbidden)
		return
	case json.NewDecoder(io.LimitReader(r.Body, 4*notesHoldsMax)).Decode(&in) != nil || in.Holds == nil || in.Was == nil:
		http.Error(w, `send {"holds": "...", "was": "..."}`, http.StatusBadRequest)
		return
	}
	holds := holdsText(*in.Holds)
	if len(holds) > notesHoldsMax {
		http.Error(w, "Holds is too long: keep it under 4 KB", http.StatusRequestEntityTooLarge)
		return
	}
	if strings.HasPrefix(holds, "## ") || strings.Contains(holds, "\n## ") {
		http.Error(w, "a line in Holds cannot start with “## ”: that would start a section of its own", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), beadViewTimeout)
	defer cancel()
	// Read, splice, write; once more if kbeads refuses because the steward wrote in between (its outside-Holds check).
	for try := 0; ; try++ {
		b := h.readLandNotes(ctx, w, land)
		if b == nil {
			return
		}
		doc := notesDocOf(b, land, true)
		if !doc.HoldsOK || doc.Holds != holdsText(*in.Was) {
			doc.Error = "Someone changed Holds while you were editing. Here they are now, with your draft kept: look it over and save again."
			if !doc.HoldsOK {
				doc.Error = "These notes have no single “## Holds” section, so Holds cannot be edited here. Ask the land’s steward or the operator in #support."
			}
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(doc)
			return
		}
		next, _ := spliceHolds(b.Fields["content"], holds)
		over := len(next) - notesHoldsMax
		if h.NotesOver != nil {
			over = h.NotesOver(next)
		}
		if over > 0 && next != b.Fields["content"] {
			http.Error(w, fmt.Sprintf("the notes would pass the steward’s 4 KB cap even with old Answers rolled out: cut Holds by %d bytes", over), http.StatusRequestEntityTooLarge)
			return
		}
		var err error
		if next != b.Fields["content"] {
			err = h.Notes.UpdateBeadFields(ctx, b.ID, map[string]string{"content": next})
		}
		var ae *beadsapi.APIError
		refused := errors.As(err, &ae) && (ae.StatusCode == http.StatusForbidden || ae.StatusCode == http.StatusNotFound)
		switch {
		case refused && ae.StatusCode == http.StatusForbidden && try == 0:
			continue
		case refused:
			http.Error(w, "the beads daemon refused the change: "+strings.TrimSpace(ae.Message), http.StatusForbidden)
		case err != nil:
			http.Error(w, "the beads daemon did not take the change", http.StatusBadGateway)
		default:
			b.Fields["content"] = next
			doc = notesDocOf(b, land, true)
			doc.Saved = true
			writeDoc(w, "kbeads", doc)
		}
		return
	}
}
