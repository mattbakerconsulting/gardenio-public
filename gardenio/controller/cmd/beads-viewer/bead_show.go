package main

import (
	"html/template"
	"strings"

	"gardenio/controller/internal/beadsapi"
)

// operatedByBanner renders the prominent royal-purple "Operated by …" banner
// at the top of the bead detail page. Resolves the operator id via the same
// resolveOperatorChip helper as the metadata sidebar so the two stay in sync
// and surfaces three intentional states:
//
//   - resolved human (chip.Kind=="human" with a real Display) → royal-purple
//     banner with the human display name. A raw `slack:U…` alias is NEVER
//     rendered as the headline.
//   - unresolved human (chip.Kind=="human" but Display is empty / equal to
//     the raw alias / still carries a `slack:` prefix) → red, clearly-labeled
//     "(unresolved operator)" treatment so the regression is loud and
//     investigable instead of silently falling through to a raw id.
//   - no human operator (Kind=="scheduler" / "agent" / "empty") → empty
//     banner. The bead metadata sidebar already discloses those cases.
//
// The royal-purple color cue is wired through CSS variables on the
// `.bead-operated-by-banner` class in layout.html so the exact shade can be
// tuned in one place.
//
// Wired as the `operatedByBanner` template func; call site in bead_show.html
// is `{{operatedByBanner .Bead .BasePath .ViewerEmail}}`.
func operatedByBanner(bead *beadsapi.BeadDetail, basePath, viewerEmail string) template.HTML {
	if bead == nil {
		return ""
	}
	chip := resolveOperatorChip(bead.OperatedBy, bead.OperatedByDisplay, bead.OperatedByEmail, bead.OperatedBySlug, bead.CreatedBy, basePath, viewerEmail)
	if chip.Kind != "human" {
		return ""
	}
	display := strings.TrimSpace(chip.Display)
	raw := strings.TrimSpace(chip.RawID)
	if isUnresolvedOperatorDisplay(display, raw) {
		var b strings.Builder
		b.WriteString(`<div class="bead-operated-by-banner bead-operated-by-banner-unresolved" role="status" data-operated-by="`)
		b.WriteString(template.HTMLEscapeString(raw))
		b.WriteString(`" title="Operated_by id could not be resolved to a human name — regression worthy">`)
		b.WriteString(`<span class="bead-operated-by-label">Operated by</span> `)
		b.WriteString(`<strong class="bead-operated-by-unresolved">(unresolved operator)</strong>`)
		b.WriteString(`</div>`)
		return template.HTML(b.String())
	}
	var b strings.Builder
	b.WriteString(`<div class="bead-operated-by-banner" role="status" data-operated-by="`)
	b.WriteString(template.HTMLEscapeString(raw))
	b.WriteString(`">`)
	b.WriteString(`<span class="bead-operated-by-label">Operated by</span> `)
	b.WriteString(`<strong class="bead-operated-by-name">`)
	b.WriteString(template.HTMLEscapeString(display))
	b.WriteString(`</strong>`)
	if chip.IsViewerSelf {
		b.WriteString(` <span class="bead-operated-by-self">(you)</span>`)
	}
	b.WriteString(`</div>`)
	return template.HTML(b.String())
}

// isUnresolvedOperatorDisplay reports whether the resolved chip Display would
// leak a raw `slack:U…` alias (or other opaque id) onto the page. The bead
// JSON path that drives the banner — operated_by_display, falling back to
// operated_by — produces Display == RawID when the daemon could not attach a
// human name; in that case the banner must render the red regression
// treatment instead of the raw alias.
func isUnresolvedOperatorDisplay(display, raw string) bool {
	if display == "" {
		return true
	}
	if raw != "" && display == raw {
		return true
	}
	low := strings.ToLower(display)
	if strings.HasPrefix(low, "slack:") || strings.HasPrefix(low, "oidc:") {
		return true
	}
	return false
}
