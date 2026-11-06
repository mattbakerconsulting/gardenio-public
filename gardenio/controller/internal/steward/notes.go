package steward

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/plangardener"
)

// The steward's memory (amendment 2026-10-23 §3, step 9.2a): one doc bead per
// land, steward-notes:<land>, markdown in fields.content with the four "## "
// sections the playbook names. The renderer puts it in full before the land
// snapshot; the playbook's last step writes it back with gb lands notes.
//
// Holds is the one section people edit too (owner Q3; kbeads
// steward_notes_authz.go lets a land owner or member change it and nothing
// else), so the steward's end-of-run write never carries Holds: MergeNotes
// keeps the stored section and applies only the steward's explicit adds and
// lifts, and a person's edit made during the run is not lost.

// Section headings, in the order the playbook names them.
const (
	SectionHolds    = "Holds"
	SectionAnswers  = "Answers"
	SectionPromises = "Promises"
	SectionInFlight = "In flight"
)

// sections is every section, with the heading text an empty notes bead gets.
var sections = []struct{ name, heading string }{
	{SectionHolds, "Holds"},
	{SectionAnswers, "Answers (newest first)"},
	{SectionPromises, "Promises"},
	{SectionInFlight, "In flight"},
}

// NotesCapBytes is the size the notes are held to so the prompt can carry
// them (§3: target 4 KB). Past it, the oldest Answers roll out.
const NotesCapBytes = 4096

// notesFormat is the doc bead's format field.
const notesFormat = "md"

// NotesBody is a notes bead's markdown: fields.content, where a doc keeps its
// body, or the description of a bead written before there was one.
func NotesBody(b *beadsapi.BeadDetail) string {
	if b == nil {
		return ""
	}
	if c := strings.TrimSpace(b.Fields["content"]); c != "" {
		return c
	}
	return strings.TrimSpace(b.Description)
}

// EmptyNotes is a new land's notes: the four sections, nothing in them.
func EmptyNotes(land string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Steward notes for %s. Read on every wake, written at the end of every run.\nThe land's people may edit Holds; the rest is the steward's.\n", land)
	for _, s := range sections {
		fmt.Fprintf(&b, "\n## %s\n", s.heading)
	}
	return b.String()
}

// notesDoc is notes split at their "## " headings.
type notesDoc struct {
	preamble []string
	sections []notesSection
}

type notesSection struct {
	heading string // the whole heading line, without its newline
	body    []string
}

// name is the known section this heading opens ("## Answers (newest first)"
// is Answers), or "".
func (s notesSection) name() string {
	h := strings.TrimSpace(strings.TrimPrefix(s.heading, "## "))
	for _, k := range sections {
		if h == k.name || strings.HasPrefix(h, k.name+" ") {
			return k.name
		}
	}
	return ""
}

// parseNotes splits content into lines under headings. Only "## " opens a
// section, the same rule kbeads' splitHolds reads Holds by.
func parseNotes(content string) notesDoc {
	var d notesDoc
	for _, l := range strings.Split(strings.TrimRight(content, "\n"), "\n") {
		if strings.HasPrefix(strings.TrimRight(l, " \t\r"), "## ") {
			d.sections = append(d.sections, notesSection{heading: strings.TrimRight(l, " \t\r")})
			continue
		}
		if n := len(d.sections); n > 0 {
			d.sections[n-1].body = append(d.sections[n-1].body, l)
		} else {
			d.preamble = append(d.preamble, l)
		}
	}
	return d
}

func (d notesDoc) String() string {
	var b strings.Builder
	for _, l := range trimBlank(d.preamble) {
		b.WriteString(l + "\n")
	}
	for i, s := range d.sections {
		if i > 0 || len(trimBlank(d.preamble)) > 0 {
			b.WriteString("\n")
		}
		b.WriteString(s.heading + "\n")
		for _, l := range trimBlank(s.body) {
			b.WriteString(l + "\n")
		}
	}
	return b.String()
}

// section is the index of the named section, or -1.
func (d notesDoc) section(name string) int {
	return slices.IndexFunc(d.sections, func(s notesSection) bool { return s.name() == name })
}

// trimBlank drops leading and trailing blank lines.
func trimBlank(lines []string) []string {
	for len(lines) > 0 && strings.TrimSpace(lines[0]) == "" {
		lines = lines[1:]
	}
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// entries groups a section body into its bullets: an entry is a "- " or "* "
// line and the lines under it. Lines before the first bullet are lead.
func entries(body []string) (lead []string, out [][]string) {
	for _, l := range trimBlank(body) {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "- ") || strings.HasPrefix(t, "* ") {
			out = append(out, []string{l})
			continue
		}
		if len(out) == 0 {
			lead = append(lead, l)
		} else {
			out[len(out)-1] = append(out[len(out)-1], l)
		}
	}
	return lead, out
}

func entryText(e []string) string {
	t := strings.TrimSpace(e[0])
	return strings.TrimSpace(t[2:])
}

// NotesEdit is one end-of-run write: the steward's own sections, and its
// changes to Holds.
type NotesEdit struct {
	// Written is the notes as the steward wrote them; its Holds section, if
	// any, is ignored. "" keeps the stored Answers, Promises and In flight.
	Written string
	// AddHolds are new holds, one line each.
	AddHolds []string
	// LiftHolds name holds to remove: each must match exactly one stored hold,
	// by its whole text or a part of it no other hold shares.
	LiftHolds []string
}

// MergeNotes is the notes after an edit: the written sections in place of the
// stored ones, the stored Holds with the edit's adds and lifts, every one of
// the four sections present.
func MergeNotes(stored string, edit NotesEdit) (string, error) {
	st := parseNotes(stored)
	out := st
	if strings.TrimSpace(edit.Written) != "" {
		out = parseNotes(edit.Written)
	}
	holds := notesSection{heading: "## Holds"}
	if i := st.section(SectionHolds); i >= 0 {
		holds = st.sections[i]
	}
	var err error
	if holds.body, err = editHolds(holds.body, edit.AddHolds, edit.LiftHolds); err != nil {
		return "", err
	}
	// The written Holds section is replaced by the stored one, wherever the
	// steward put it; written with none, Holds goes first.
	var secs []notesSection
	placed := false
	for _, s := range out.sections {
		if s.name() == SectionHolds {
			if !placed {
				secs = append(secs, holds)
				placed = true
			}
			continue
		}
		secs = append(secs, s)
	}
	if !placed {
		secs = append([]notesSection{holds}, secs...)
	}
	out.sections = secs
	for _, k := range sections {
		if out.section(k.name) < 0 {
			out.sections = append(out.sections, notesSection{heading: "## " + k.heading})
		}
	}
	return out.String(), nil
}

func editHolds(body []string, add, lift []string) ([]string, error) {
	lead, es := entries(body)
	for _, l := range lift {
		l = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(l), "- "))
		if l == "" {
			continue
		}
		match := -1
		for i, e := range es {
			if entryText(e) == l {
				match = i
				break
			}
		}
		if match < 0 {
			for i, e := range es {
				if strings.Contains(entryText(e), l) {
					if match >= 0 {
						return nil, fmt.Errorf("lifting hold %q: it matches more than one hold; give more of its text", l)
					}
					match = i
				}
			}
		}
		if match < 0 {
			return nil, fmt.Errorf("lifting hold %q: no hold says that", l)
		}
		es = slices.Delete(es, match, match+1)
	}
	for _, a := range add {
		a = strings.Join(strings.Fields(strings.TrimPrefix(strings.TrimSpace(a), "- ")), " ")
		if a == "" || slices.ContainsFunc(es, func(e []string) bool { return entryText(e) == a }) {
			continue
		}
		es = append(es, []string{"- " + a})
	}
	out := slices.Clone(lead)
	for _, e := range es {
		out = append(out, e...)
	}
	return out, nil
}

// CapNotes holds notes to max bytes by rolling the oldest Answers out (they
// are newest first, so from the bottom). rolled is what left, newest first.
// Notes still over the cap with no Answers left are an error: the steward
// shortens In flight or Promises and writes again.
func CapNotes(content string, max int) (capped string, rolled []string, err error) {
	d := parseNotes(content)
	capped = d.String()
	if len(capped) <= max {
		return capped, nil, nil
	}
	i := d.section(SectionAnswers)
	var lead []string
	var es [][]string
	if i >= 0 {
		lead, es = entries(d.sections[i].body)
	}
	var gone [][]string
	for len(capped) > max && len(es) > 0 {
		gone = append(gone, es[len(es)-1])
		es = es[:len(es)-1]
		body := slices.Clone(lead)
		for _, e := range es {
			body = append(body, e...)
		}
		d.sections[i].body = body
		capped = d.String()
	}
	if len(capped) > max {
		return "", nil, fmt.Errorf("the notes are %d bytes with every Answer rolled out; the cap is %d: shorten In flight or Promises and write again", len(capped), max)
	}
	for j := len(gone) - 1; j >= 0; j-- {
		rolled = append(rolled, strings.Join(gone[j], "\n"))
	}
	return capped, rolled, nil
}

// NotesOverBytes is how far content is past limit once every Answer is rolled
// out: 0 when CapNotes can hold it to limit, else the bytes to cut before
// CapNotes will (a person's Holds edit is refused past it, gardenhub
// land_notes.go).
func NotesOverBytes(content string, limit int) int {
	d := parseNotes(content)
	if i := d.section(SectionAnswers); i >= 0 {
		d.sections[i].body, _ = entries(d.sections[i].body)
	}
	return max(0, len(d.String())-limit)
}

// FindNotes is the land's steward-notes:<land> bead, or nil.
func FindNotes(ctx context.Context, c plangardener.Client, land string) (*beadsapi.BeadDetail, error) {
	return notesBead(ctx, c, land)
}

// EnsureNotes is the land's notes bead, created with empty sections when the
// land has none. created is true when this call made it.
func EnsureNotes(ctx context.Context, c plangardener.Client, land, createdBy string) (id string, created bool, err error) {
	if b, err := notesBead(ctx, c, land); err != nil {
		return "", false, err
	} else if b != nil {
		return b.ID, false, nil
	}
	fields, err := json.Marshal(map[string]string{"format": notesFormat, "content": EmptyNotes(land)})
	if err != nil {
		return "", false, fmt.Errorf("marshalling %s: %w", NotesTitle(land), err)
	}
	id, err = c.CreateBead(ctx, beadsapi.CreateBeadRequest{
		Title:      NotesTitle(land),
		ShortTitle: "steward notes",
		Type:       "doc",
		Kind:       "data",
		Labels:     []string{"project:" + land},
		Fields:     json.RawMessage(fields),
		CreatedBy:  createdBy,
	})
	if err != nil {
		return "", false, fmt.Errorf("creating %s: %w", NotesTitle(land), err)
	}
	return id, true, nil
}

// notesWriter is the write half of WriteNotes.
type notesWriter interface {
	UpdateBeadFields(ctx context.Context, beadID string, fields map[string]string) error
	AddComment(ctx context.Context, beadID, author, text string) error
}

// WriteResult reports a WriteNotes.
type WriteResult struct {
	ID     string
	Bytes  int
	Rolled []string
}

// WriteNotes applies an end-of-run edit to the land's notes bead: merge
// (MergeNotes), cap (CapNotes), write content and, when mark is set, the
// #machines high-water mark. Answers that roll out are kept as a comment on
// the bead, the land's decision history, before the content is written.
func WriteNotes(ctx context.Context, c plangardener.Client, w notesWriter, land string, edit NotesEdit, mark, author string) (*WriteResult, error) {
	b, err := notesBead(ctx, c, land)
	if err != nil {
		return nil, err
	}
	if b == nil {
		return nil, fmt.Errorf("land %s has no %s bead", land, NotesTitle(land))
	}
	merged, err := MergeNotes(NotesBody(b), edit)
	if err != nil {
		return nil, err
	}
	capped, rolled, err := CapNotes(merged, NotesCapBytes)
	if err != nil {
		return nil, err
	}
	if len(rolled) > 0 {
		text := fmt.Sprintf("Answers rolled out of %s (cap %d bytes), newest first:\n%s", NotesTitle(land), NotesCapBytes, strings.Join(rolled, "\n"))
		if err := w.AddComment(ctx, b.ID, author, text); err != nil {
			return nil, fmt.Errorf("keeping the rolled Answers on %s: %w", b.ID, err)
		}
	}
	fields := map[string]string{"content": capped}
	if mark = strings.TrimSpace(mark); mark != "" {
		fields[NotesHighWaterField] = mark
	}
	if err := w.UpdateBeadFields(ctx, b.ID, fields); err != nil {
		return nil, fmt.Errorf("writing %s: %w", b.ID, err)
	}
	return &WriteResult{ID: b.ID, Bytes: len(capped), Rolled: rolled}, nil
}
