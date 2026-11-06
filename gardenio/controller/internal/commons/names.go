package commons

import (
	"context"
	"errors"
	"html/template"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"

	"gardenio/controller/internal/beadsapi"
)

// Bead names (gd-YvdhZD2UtL, rubric C12): a bead a message or card links to
// reads as its short title (else its title), with its id in the link's
// title=, not as a bare id. The
// titles are read on the viewer's bearer, like a card's face, so a bead the
// viewer may not see reads exactly as one the daemon does not have: "not
// found", a chip that still links.

const (
	// nameReads bounds the beads one fragment reads for their titles; the
	// rest link under their ids.
	nameReads = 40
	// nameMax caps a link's label, in runes, and tipMax its tooltip, which
	// carries the title a label cut short, and the id.
	nameMax = 120
	tipMax  = 500
)

// beadNames is the titles of the beads a row links to, by id: "" for one the
// daemon does not return to the viewer. An id not in the map was not read (a
// failed or bounded read) and links under its id.
type beadNames map[string]string

// withNames reads the titles of the beads rows link to, after withCards (a
// card's face carries some of them). One read per distinct id, at most
// nameReads, cardReads at a time.
func (h *Handler) withNames(ctx context.Context, r *http.Request, rows []row) {
	var ids []string
	for i := range rows {
		ids = rows[i].beadRefs(ids)
	}
	names := h.readNames(ctx, h.scope(r), ids)
	if names == nil {
		return
	}
	for i := range rows {
		rows[i].Names = names
		if s := rows[i].M.Snap; s != nil && s.Place == "" {
			if id := snapPlaceID(s.At); id != "" && names[id] != "" {
				s.Place = clipRunes(names[id], nameMax)
			}
		}
	}
}

// readNames reads the names of ids on the viewer's bearer, once each and at
// most nameReads of them (sorted, so the same text names the same beads),
// cardReads at a time; nil when there is nothing to read. A bead reads as its
// short title, else its title (gd-QTukIA8Nvv: the workbench shows beads by
// their short titles, never as a bare id); one the daemon does not return,
// or of a land outside scope, is "" (not found).
func (h *Handler) readNames(ctx context.Context, scope Scope, ids []string) beadNames {
	slices.Sort(ids)
	ids = slices.Compact(ids)
	if len(ids) > nameReads {
		ids = ids[:nameReads]
	}
	if len(ids) == 0 {
		return nil
	}
	names := make(beadNames, len(ids))
	var mu sync.Mutex
	sem := make(chan struct{}, cardReads)
	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Add(1)
		sem <- struct{}{}
		go func(id string) {
			defer func() { <-sem; wg.Done() }()
			b, err := h.Beads.GetBead(ctx, id)
			title, ok := "", true
			switch {
			case err != nil:
				var ae *beadsapi.APIError
				ok = errors.As(err, &ae) && (ae.StatusCode == http.StatusNotFound || ae.StatusCode == http.StatusForbidden)
			case b != nil && (scope.Admin || scope.Allows(landOf(b.Labels))):
				title = clipRunes(firstNonEmpty(strings.TrimSpace(b.ShortTitle), strings.TrimSpace(b.Title)), tipMax)
			}
			if !ok {
				return
			}
			mu.Lock()
			names[id] = title
			mu.Unlock()
		}(id)
	}
	wg.Wait()
	return names
}

// beadRefs appends the bead ids the row's templates link: those in its words
// and its card's, the subject of a card with no face, an escalation's task and
// the plan a snapshot was taken at.
func (r *row) beadRefs(ids []string) []string {
	if r.M.Deleted {
		return ids
	}
	texts := []string{r.M.Text}
	if n := r.M.Notice; n != nil {
		texts = append(texts, n.Cause)
	}
	if c := r.Agent; c != nil {
		texts = append(texts, c.Cause)
	}
	if c := r.Decision; c != nil {
		texts = append(texts, c.Prompt, c.Context, c.Rationale)
	}
	if c := r.Escalation; c != nil {
		texts = append(texts, c.Title, c.Context, c.Reason)
		if c.Task != "" {
			ids = append(ids, c.Task)
		}
	}
	for _, t := range texts {
		ids = append(ids, textBeadIDs(t)...)
	}
	if r.M.SubjectID != "" && !r.HasFace() && r.M.Notice == nil {
		ids = append(ids, r.M.SubjectID)
	}
	if s := r.M.Snap; s != nil && s.Place == "" {
		if id := snapPlaceID(s.At); id != "" {
			ids = append(ids, id)
		}
	}
	return ids
}

// textBeadIDs is the bead ids linkify links in text.
func textBeadIDs(text string) []string {
	var ids []string
	for _, m := range links.FindAllStringSubmatchIndex(text, -1) {
		if m[4] >= 0 && beadLike(text[m[4]:m[5]]) {
			ids = append(ids, text[m[4]:m[5]])
		}
	}
	return ids
}

// beadLink is the link to bead id in the garden's bead view, labelled with its
// title from names; its tooltip is the title and the id, as the garden's own
// links carry them (gardenhub web/titles.js tip). A bead the daemon did not
// return is a "not found" chip that still links, which the garden leaves as it
// is (data-titles="off"); one not read links under its id, which the garden
// relabels when its farm knows the title.
func beadLink(id, base string, names beadNames) template.HTML {
	return beadAnchor("cm-bead", id, base, names)
}

// subjectLink is beadLink for the subject of a card with no face.
func subjectLink(id, base string, names beadNames) template.HTML {
	return beadAnchor("cm-card-subject cm-bead", id, base, names)
}

func beadAnchor(class, id, base string, names beadNames) template.HTML {
	eid := template.HTMLEscapeString(id)
	title, read := names[id]
	tip, label, more := eid, eid, ""
	switch {
	case !read:
	case title == "":
		class += " cm-bead--missing"
		tip, label, more = eid+": not found", `Bead not found <span class="cm-bead-id">`+eid+`</span>`, ` data-titles="off"`
	default:
		tip, label = template.HTMLEscapeString(title)+" ("+eid+")", template.HTMLEscapeString(clipRunes(title, nameMax))
	}
	return template.HTML(`<a class="` + class + `" href="` + template.HTMLEscapeString(base+"/garden?bead="+url.QueryEscape(id)) +
		`" data-bead="` + eid + `" title="` + tip + `"` + more + `>` + label + `</a>`)
}
