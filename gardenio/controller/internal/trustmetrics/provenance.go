package trustmetrics

import (
	"regexp"
	"strings"
)

// provenanceTrailerRe matches the lab-internal structured provenance trailer
// lines a peel-on-demand trickle MR carries (peel-trailer.sh emit_block, R2 §7;
// mirrors bridge.darkProvenanceTrailerRe): Dark-Bead:/Dark-MR:/Provenance-Hash:
// plus the plain Bead: anchor. These lines ARE the "receipt" — §3.4 / metric #7
// specify the verbosity ratio is computed with the provenance block EXCLUDED,
// so terse prose is not penalised for carrying a complete receipt, and #8's
// "structured provenance block" presence is detected from them.
var provenanceTrailerRe = regexp.MustCompile(`(?im)^[ \t]*(?:Dark-Bead|Dark-MR|Provenance-Hash|Bead|Peeled-From|Operator)[ \t]*:.*$`)

// provenanceFenceRe matches an explicit fenced provenance block delimited by
// HTML comment markers, an alternative structured-block convention some peel
// templates use:
//
//	<!-- provenance:begin -->
//	...
//	<!-- provenance:end -->
var provenanceFenceRe = regexp.MustCompile(`(?is)<!--\s*provenance:begin\s*-->.*?<!--\s*provenance:end\s*-->`)

// splitProvenance separates an MR description into its prose (review-facing
// rationale) and reports whether a structured provenance block is present.
//
// The returned prose has the provenance trailer lines and any fenced provenance
// block removed, so len(prose) is the verbosity-ratio (#7) numerator with
// provenance excluded. hasProvenance is true when at least one provenance
// trailer line OR a fenced provenance block was found — the description-side
// half of provenance completeness (#8); the bead-link half is joined separately.
func splitProvenance(description string) (prose string, hasProvenance bool) {
	if description == "" {
		return "", false
	}

	fenceStripped := provenanceFenceRe.ReplaceAllString(description, "")
	hasFence := fenceStripped != description

	hasTrailer := provenanceTrailerRe.MatchString(fenceStripped)
	trailerStripped := provenanceTrailerRe.ReplaceAllString(fenceStripped, "")

	prose = strings.TrimSpace(trailerStripped)
	return prose, hasFence || hasTrailer
}
