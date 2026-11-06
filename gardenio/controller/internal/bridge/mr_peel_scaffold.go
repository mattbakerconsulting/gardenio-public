package bridge

import (
	"fmt"
	"regexp"
	"strings"
)

// mr_peel_scaffold.go implements the deterministic half of the
// `gb mr peel <dark-bead> --concern <slice>` scaffolder (task ka-0000001847,
// R3 catalog /bundle/ka-0000005053 §2.5 / follow-up #26; master epic
// ka-0000005560). Deferred out of ka-0000005561, which shipped the trust-dossier
// template + presence/non-triviality linter.
//
// R3 flags single-concern peeling of already-merged dark state as the RISKIEST /
// most speculative piece and says it may stay a human-guided agent task. This
// scaffolder therefore does ONLY the deterministic, low-risk work: given a dark
// bead + a concern slice label, it PRE-FILLS a trust-dossier MR body — seeding
// the mechanical, extractable fields (Provenance anchors, Operator:/Bead:
// trailers, the concern boundary, a Summary seeded from the dark bead title) and
// leaving the JUDGMENT sections (Why / Alternatives considered / Risk assessment /
// Evidence) as explicit TODO stubs a human must supply. The scaffolder does NOT
// select which files form the slice, does NOT push a branch, and does NOT open an
// MR — it hands a filled skeleton to a human, exactly the "human-guided" posture
// R3 asked for.
//
// The rendered body is deliberately kept in lock-step with:
//   - .gitlab/merge_request_templates/trust-dossier.md (the DOSSIER block + trailers)
//   - scripts/lint-trust-dossier.sh (presence + non-triviality lint)
//   - ParseTrickleDossier / TrickleDarkMRReferenced / TrickleOperatorTrailer
//     (mr_trickle_gate.go — the trickle-gate resolvers that read this body back)
//
// so a scaffolded body opts into the dossier lint (via the DOSSIER markers) and
// its deterministic fields are seen by the trickle gate, while the TODO judgment
// sections keep the lint RED until a human fills them — which is the intended
// signal, not a bug.

// PeelScaffoldInput carries the deterministic facts a peel scaffold is built
// from. Only DarkBeadID and Concern are required; the rest are best-effort
// enrichment that render as TODO stubs when absent.
type PeelScaffoldInput struct {
	DarkBeadID    string // gd-… — the already-merged dark work being peeled (required)
	DarkBeadTitle string // dark bead title — seeds the Summary section
	Concern       string // --concern <slice> — the single concern this slice delivers (required)
	Operator      string // Operator: trailer value (human who requested the peel)
	RequestBead   string // Bead: trailer — the upstream/peel-request bead, if distinct from the dark bead
	DarkMRIID     string // dark MR IID (digits only) — "" when unknown
	SlugOverride  string // explicit peel slug; empty => derived from concern + dark bead
}

// PeelScaffold is the rendered scaffold: a suggested branch/slug, a suggested MR
// title, and the pre-filled trust-dossier MR body.
type PeelScaffold struct {
	Slug        string // peel branch slug (no "peel/" prefix), safe for refs/heads/peel/<slug>
	Branch      string // peel/<slug>
	Title       string // suggested Draft MR title
	Description string // the pre-filled trust-dossier MR body
}

// peelSlugCleanRe collapses any run of non [a-z0-9] characters to a single '-'.
var peelSlugCleanRe = regexp.MustCompile(`[^a-z0-9]+`)

// PeelSlugify reduces an arbitrary label to a git-ref-safe, lowercase slug
// component: lowercased, every non-alphanumeric run collapsed to a single '-',
// leading/trailing '-' trimmed. Returns "" for input with no alphanumerics.
func PeelSlugify(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = peelSlugCleanRe.ReplaceAllString(s, "-")
	return strings.Trim(s, "-")
}

// peelSlug derives the peel branch slug from the concern (primary) plus a short
// disambiguator from the dark bead id, so two different concerns peeled from the
// same dark bead — and the same concern peeled from two different dark beads —
// never collide on refs/heads/peel/<slug>. A caller-supplied override wins.
//
// The result is guaranteed non-empty, /-free, and never literally "main" (which
// peel-mr-cmd.sh's valid_slug refuses), so the scaffold's suggested branch is
// always a legal peel ref.
func peelSlug(concern, darkBeadID, override string) string {
	if s := PeelSlugify(override); s != "" {
		return dodgeMain(s)
	}
	concernSlug := PeelSlugify(concern)
	// The dark bead id (ka-XXXX) disambiguates; drop the redundant "gd-" prefix
	// so the slug reads "concern-xxxx" rather than "concern-ka-xxxx".
	beadSlug := PeelSlugify(strings.TrimPrefix(strings.TrimPrefix(darkBeadID, "gd-"), "ka-"))

	parts := make([]string, 0, 2)
	if concernSlug != "" {
		parts = append(parts, concernSlug)
	}
	if beadSlug != "" {
		parts = append(parts, beadSlug)
	}
	slug := strings.Join(parts, "-")
	if slug == "" {
		// Neither concern nor bead id yielded anything slug-able; fall back to a
		// stable literal so we still emit a legal (if generic) peel ref.
		slug = "slice"
	}
	return dodgeMain(slug)
}

// dodgeMain guarantees the slug is never literally the protected target branch
// name, which peel-mr-cmd.sh refuses as a peel slug.
func dodgeMain(slug string) string {
	if slug == "main" {
		return "main-slice"
	}
	return slug
}

// ScaffoldPeelDossier renders a pre-filled trust-dossier MR scaffold for a single
// concern peeled from a dark bead. It is PURE — no daemon, no GitLab, no I/O — so
// the CLI wrapper (gb mr peel) resolves the dark bead's facts and this function
// deterministically renders the body. Returns an error only for the two required
// inputs (dark bead id + concern); everything else degrades to a TODO stub.
func ScaffoldPeelDossier(in PeelScaffoldInput) (PeelScaffold, error) {
	darkBead := strings.TrimSpace(in.DarkBeadID)
	if darkBead == "" {
		return PeelScaffold{}, fmt.Errorf("a dark bead id is required (the already-merged dark work to peel from)")
	}
	concern := strings.TrimSpace(in.Concern)
	if concern == "" {
		return PeelScaffold{}, fmt.Errorf("a --concern <slice> is required (the single concern this slice delivers)")
	}

	slug := peelSlug(concern, darkBead, in.SlugOverride)
	branch := "peel/" + slug

	// Summary is seeded from the dark bead title (real, editable starting prose,
	// NOT an italic placeholder — so the dossier lint reads it as present). If we
	// have no title, fall back to a concern-scoped stub the human replaces.
	summary := strings.TrimSpace(in.DarkBeadTitle)
	if summary != "" {
		summary = fmt.Sprintf("The `%s` slice peeled from %s: %s", concern, darkBead, summary)
	} else {
		summary = fmt.Sprintf("The `%s` slice peeled from %s.", concern, darkBead)
	}

	// Provenance anchors — the durable, extractable half. Anchor on bead id + MR
	// IID, NEVER a SHA (the rebase gardener rewrites dark SHAs every cycle).
	darkMRLine := "- Dark MR: _TODO: dark GitLab MR URL/IID (this dark bead carries no mr_url)_"
	if iid := strings.TrimSpace(in.DarkMRIID); iid != "" {
		darkMRLine = fmt.Sprintf("- Dark MR: !%s", iid)
	}

	operator := strings.TrimSpace(in.Operator)
	operatorTrailer := operator
	if operatorTrailer == "" {
		operatorTrailer = "_TODO: name of the human who requested this peel (from BEADS_OPERATED_BY)_"
	}
	requestBead := strings.TrimSpace(in.RequestBead)
	if requestBead == "" {
		// Default the Bead: trailer to the dark bead itself — it is the concrete
		// originating request unless the caller names a distinct peel-request bead.
		requestBead = darkBead
	}

	var b strings.Builder
	b.WriteString("<!-- DOSSIER:BEGIN -->\n\n")

	b.WriteString("## Summary\n\n")
	b.WriteString(summary)
	b.WriteString("\n\n")

	b.WriteString("## Why\n\n")
	b.WriteString("_TODO (human): the problem this slice solves and why it was requested — distinct from Summary. " +
		"The scaffolder cannot infer intent; the trickle lint REJECTS an empty Why or a Why byte-equal to Summary._\n\n")

	b.WriteString("## Alternatives considered\n\n")
	b.WriteString("_TODO (human): at least one named alternative approach and why it was rejected._\n\n")

	b.WriteString("## Risk assessment\n\n")
	b.WriteString("_TODO (human): blast radius, rollback plan, and what could break._\n\n")

	b.WriteString("## Evidence\n\n")
	b.WriteString("<!-- Claim-matched evidence. Fill run-ID/artifact links by hand until the trickle-gate lands. Delete lines that do not apply. -->\n")
	b.WriteString("- Standalone build: _TODO: upstream-HEAD CI run link + run ID_\n")
	b.WriteString("- E2E: _TODO: run link + run ID_\n")
	b.WriteString("- Coverage: _TODO: patch-cov result — +X% patch cov, N bypasses_\n")
	b.WriteString("- /code-review: _TODO: review-provenance artifact link — findings + resolutions_\n")
	b.WriteString("- /security-review: _TODO: artifact link + Security-Scan: result_\n\n")

	b.WriteString("## Provenance\n\n")
	b.WriteString(darkMRLine)
	b.WriteString("\n")
	b.WriteString(fmt.Sprintf("- Dark bead: %s\n", darkBead))
	b.WriteString(fmt.Sprintf("- Peeled from: %s\n\n", darkBead))

	b.WriteString("<!-- DOSSIER:END -->\n\n")

	b.WriteString("## What this is NOT\n\n")
	b.WriteString(fmt.Sprintf(
		"This slice delivers ONLY the `%s` concern peeled from %s. It does NOT carry the other "+
			"concerns from that dark work — peel those as separate single-concern slices. "+
			"_TODO (human): name the specific files/areas this slice does NOT touch._\n\n",
		concern, darkBead))

	b.WriteString("---\n")
	b.WriteString(fmt.Sprintf("Operator: %s\n", operatorTrailer))
	b.WriteString(fmt.Sprintf("Bead: %s\n", requestBead))
	// Durable dark-provenance trailers (§7) — bead id + MR IID, never a SHA — so
	// the trickle gate's dark-MR-ancestry check sees the anchor deterministically.
	b.WriteString(fmt.Sprintf("Dark-Bead: %s\n", darkBead))
	if iid := strings.TrimSpace(in.DarkMRIID); iid != "" {
		b.WriteString(fmt.Sprintf("Dark-MR: %s\n", iid))
	}

	title := fmt.Sprintf("peel(%s): %s", concern, peelTitleFragment(in.DarkBeadTitle, darkBead))

	return PeelScaffold{
		Slug:        slug,
		Branch:      branch,
		Title:       title,
		Description: b.String(),
	}, nil
}

// peelTitleFragment returns a compact, single-line title fragment for the
// suggested MR title: the dark bead title trimmed to its first line, falling
// back to the dark bead id when there is no title.
func peelTitleFragment(title, fallback string) string {
	t := strings.TrimSpace(title)
	if t == "" {
		return fallback
	}
	if i := strings.IndexByte(t, '\n'); i >= 0 {
		t = strings.TrimSpace(t[:i])
	}
	return t
}
