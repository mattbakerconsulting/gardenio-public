package main

import (
	"bytes"
	"html"
	"net/http"
	"regexp"
	"strings"
)

// previewExcerptRunes is the rune cap for the description preview rendered
// in the hover popover. Matches the design doc's "first ~280 chars".
const previewExcerptRunes = 280

// previewTagPattern strips HTML tags from goldmark-rendered markdown so the
// excerpt is plain text without raw `<p>`/`<strong>` markup.
var previewTagPattern = regexp.MustCompile(`<[^>]*>`)

// excerpt renders text through the inline goldmark instance, strips HTML
// tags, decodes entities, collapses whitespace, and truncates at a rune
// boundary, appending an ellipsis when truncation occurs.
//
// Goldmark is reused (instead of a hand-rolled markdown stripper) so the
// preview matches the same rendering rules used elsewhere in the viewer —
// keeping the excerpt visually consistent with the full description.
func (s *Server) excerpt(text string, n int) string {
	if text == "" || n <= 0 {
		return ""
	}
	var buf bytes.Buffer
	var rendered string
	if err := s.mdInline.Convert([]byte(text), &buf); err != nil {
		rendered = text
	} else {
		rendered = buf.String()
	}
	plain := previewTagPattern.ReplaceAllString(rendered, "")
	plain = html.UnescapeString(plain)
	plain = strings.Join(strings.Fields(plain), " ")
	runes := []rune(plain)
	if len(runes) <= n {
		return plain
	}
	return strings.TrimRight(string(runes[:n]), " ") + "…"
}

// handleBeadPreview renders a small fragment used by the bead-link hover
// popover (preview_popover.js). The fragment includes the bead's type,
// status, id, title, and a short description excerpt.
//
// A missing or out-of-scope bead (beadMissing) renders the
// `bead_preview_notfound` fragment with HTTP 404, which the popover shows as
// it shows a preview; the two answer alike, so the popover never tells a
// caller that another land's bead exists.
// 500 from the daemon returns 500 with a plain text body so the client-side
// hover controller can drop the popover instead of injecting broken HTML.
func (s *Server) handleBeadPreview(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "Missing bead id", http.StatusBadRequest)
		return
	}
	bead, err := s.daemon.GetBead(r.Context(), id)
	if err != nil {
		if beadMissing(err) {
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusNotFound)
			s.renderPartial(w, r, "templates/partials/bead_preview.html", "bead_preview_notfound", map[string]any{
				"ID": id,
			})
			return
		}
		s.logger.Error("getting bead for preview", "id", id, "error", err)
		http.Error(w, "preview unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Cache-Control", "private, max-age=30")
	// Anchored preview (A5, ka-0000003033): `?anchor=c1-1` shows the anchored
	// section — the matching heading + the body that follows — instead of the
	// whole-bead excerpt. Falls through to the normal preview when the anchor
	// is absent or matches no heading.
	if anchor := r.URL.Query().Get("anchor"); anchor != "" {
		if heading, body, found := anchorSection(bead.Description, anchor); found {
			// Cache-Control already set above for all non-404 paths.
			s.renderPartial(w, r, "templates/partials/bead_preview.html", "bead_preview", map[string]any{
				"Bead":          bead,
				"Excerpt":       s.excerpt(body, previewExcerptRunes),
				"AnchorHeading": heading,
			})
			return
		}
	}
	if bead.Type == "merge-request" {
		s.renderPartial(w, r, "templates/partials/bead_preview_mr.html", "bead_preview_mr", map[string]any{
			"Bead":    bead,
			"Excerpt": s.excerpt(bead.Description, previewExcerptRunes),
			"MRURL":   firstNonEmpty(bead.Fields, "mr_url", "url", "gitlab_mr_url"),
			"MRState": firstNonEmpty(bead.Fields, "mr_state", "state"),
			"MRIID":   firstNonEmpty(bead.Fields, "mr_iid", "gitlab_mr_iid"),
			// External = read-only upstream-MR mirror (E-PROV FW-5,
			// ka-0000003146). Drives the "external · read-only" chip so the
			// preview makes clear this MR is human-owned upstream, not ours.
			"External": bead.Fields["external_mirror"] == "true" || bead.Fields["upstream"] == "true",
		})
		return
	}
	s.renderPartial(w, r, "templates/partials/bead_preview.html", "bead_preview", map[string]any{
		"Bead":    bead,
		"Excerpt": s.excerpt(bead.Description, previewExcerptRunes),
	})
}

// firstNonEmpty returns the first non-empty value among the named keys in m.
// Used by the MR preview to pick a working mr_url / mr_state / mr_iid across
// the slightly-varying field shapes the gitlab-bridge writes (canonical
// mr_url plus the older `url` / `gitlab_mr_url` aliases the type_mr render
// already falls back through).
func firstNonEmpty(m map[string]string, keys ...string) string {
	for _, k := range keys {
		if v := m[k]; v != "" {
			return v
		}
	}
	return ""
}
