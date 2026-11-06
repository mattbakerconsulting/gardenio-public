package gardenhub

import (
	"context"
	"regexp"
	"sort"
	"strings"

	"gardenio/controller/internal/beadsapi"
)

// Mentions (gd-vsCHW7UqmF, Commons UX rubric C12): the bead ids the panel's free text names (the description, the notes,
// the comments, the field values a face reads its text from, a harvest's wrap-up) are titled with the related beads, in
// the same bounded pass, and sent as a mentions map (id -> title), so an id that is neither related nor on the map still
// reads as its title. A bead the daemon does not return stays out of the map and reads as its id.
//
// The id rule is web/workbench.js runs()'s and web/titles.js split()'s: two lowercase letters, a dash and ten letters or
// digits with at least one capital or digit, a whole word, not inside a URL and not glued to a '/' (the last segment of a
// branch path, agent/some-branch/gd-AbCdEfGhIj, is the branch's, not a link).

const beadMentionsMax = 30 // mentioned beads titled per bead view

var (
	mentionID  = regexp.MustCompile(`\b[a-z]{2}-[A-Za-z0-9]{10}\b`)
	mentionURL = regexp.MustCompile("https?://[^\\s<>\"'`]+")
)

// mentionIDs appends to out the bead ids text names, in order and once each, up to max in all.
func mentionIDs(text string, out []string, max int) []string {
	if len(out) >= max || !strings.Contains(text, "-") {
		return out
	}
	urls := mentionURL.FindAllStringIndex(text, -1)
next:
	for _, m := range mentionID.FindAllStringIndex(text, -1) {
		i, j := m[0], m[1]
		if !strings.ContainsAny(text[i+3:j], "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789") {
			continue // "re-introduced": a word
		}
		if (i > 0 && text[i-1] == '/') || (j < len(text) && text[j] == '/') {
			continue // a path's segment
		}
		for _, u := range urls {
			if i >= u[0] && i < u[1] {
				continue next
			}
		}
		id := text[i:j]
		for _, o := range out {
			if o == id {
				continue next
			}
		}
		if out = append(out, id); len(out) >= max {
			break
		}
	}
	return out
}

// mentions is a rel for each bead b's text names (b itself left out: the panel has its title), for hydrate to title:
// the description, the notes, the comments sent (newest first), the field values in key order, then the crews' wrap-ups.
func mentions(b *beadsapi.BeadDetail, comments []beadsapi.Comment, crews harvestCrews) []beadRel {
	var ids []string
	scan := func(s string) { ids = mentionIDs(s, ids, beadMentionsMax+1) }
	scan(b.Description)
	scan(b.Notes)
	for i := len(comments) - 1; i >= 0; i-- {
		scan(comments[i].Text)
	}
	keys := make([]string, 0, len(b.Fields))
	for k := range b.Fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		scan(b.Fields[k])
	}
	for _, a := range crews.agents {
		scan(wrapTLDR(a.Fields["wrapup"]))
	}
	var out []beadRel
	for _, id := range ids {
		if id != b.ID && len(out) < beadMentionsMax {
			out = append(out, beadRel{ID: id})
		}
	}
	return out
}

// mentionTitles is what hydrate made of the mentions: the names (id -> short title, else title; gd-QTukIA8Nvv) and the
// ids the daemon did not return.
func mentionTitles(ment []beadRel, got map[string]*beadsapi.BeadDetail) (titles map[string]string, unseen []string) {
	for _, m := range ment {
		name := firstNonEmpty(m.ShortTitle, m.Title)
		switch {
		case name != "" && name != m.ID:
			if titles == nil {
				titles = map[string]string{}
			}
			titles[m.ID] = name
		case got[m.ID] == nil:
			unseen = append(unseen, m.ID)
		}
	}
	return titles, unseen
}

// bundleMentions titles the bead ids a bundle's prose names (its description and its markdown documents, the parts the
// panel reads as text), as the bead view titles its own, so "See gd-…" reads as the bead's title. The bundle, its
// documents and the bead it was filed for are titled already and left out.
func bundleMentions(ctx context.Context, src BeadReader, d *BundleDoc) {
	skip := map[string]bool{d.ID: true}
	if d.Parent != nil {
		skip[d.Parent.ID] = true
	}
	for _, p := range d.Docs {
		skip[p.ID] = true
	}
	var ids []string
	scan := func(s string) { ids = mentionIDs(s, ids, beadMentionsMax+len(skip)) }
	scan(d.Description)
	for _, p := range d.Docs {
		switch strings.ToLower(strings.TrimSpace(p.Format)) {
		case "md", "markdown", "report": // web/bundle.js FORMATS: the formats it reads as markdown
			scan(p.Content)
		}
	}
	var ment []beadRel
	for _, id := range ids {
		if !skip[id] && len(ment) < beadMentionsMax {
			ment = append(ment, beadRel{ID: id})
		}
	}
	if len(ment) == 0 {
		return
	}
	all := make([]*beadRel, len(ment))
	for i := range ment {
		all[i] = &ment[i]
	}
	d.Mentions, d.Unseen = mentionTitles(ment, hydrate(ctx, src, all))
}
