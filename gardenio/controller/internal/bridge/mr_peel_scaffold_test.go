package bridge

import (
	"strings"
	"testing"
)

// TestScaffoldPeelDossier_RequiredInputs proves the two required inputs (dark
// bead id + concern) are enforced.
func TestScaffoldPeelDossier_RequiredInputs(t *testing.T) {
	if _, err := ScaffoldPeelDossier(PeelScaffoldInput{Concern: "scaffolder"}); err == nil {
		t.Fatal("expected error for missing dark bead id")
	}
	if _, err := ScaffoldPeelDossier(PeelScaffoldInput{DarkBeadID: "gd-ABC"}); err == nil {
		t.Fatal("expected error for missing concern")
	}
}

// TestScaffoldPeelDossier_FullyPopulated proves every deterministic field lands
// in the rendered body: the DOSSIER markers, all six required section headings,
// the provenance anchors, the concern boundary, and the trailers.
func TestScaffoldPeelDossier_FullyPopulated(t *testing.T) {
	got, err := ScaffoldPeelDossier(PeelScaffoldInput{
		DarkBeadID:    "ka-0000001847",
		DarkBeadTitle: "monorepo-dark: gb mr peel scaffolder",
		Concern:       "scaffolder",
		Operator:      "Matt Baker",
		RequestBead:   "gd-REQ999",
		DarkMRIID:     "42",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	d := got.Description

	// DOSSIER markers opt the body into the trust-dossier lint.
	for _, marker := range []string{"<!-- DOSSIER:BEGIN -->", "<!-- DOSSIER:END -->"} {
		if !strings.Contains(d, marker) {
			t.Errorf("body missing marker %q", marker)
		}
	}

	// All six required sections must be present, so the lint's presence check
	// only fails on triviality (the human-owned TODO sections), never absence.
	for _, section := range TrickleDossierSections {
		if !strings.Contains(d, "## "+section) {
			t.Errorf("body missing required section heading %q", section)
		}
	}

	// Deterministic provenance + trailers.
	for _, want := range []string{
		"- Dark MR: !42",
		"- Dark bead: ka-0000001847",
		"- Peeled from: ka-0000001847",
		"Operator: Matt Baker",
		"Bead: gd-REQ999",
		"Dark-Bead: ka-0000001847",
		"Dark-MR: 42",
	} {
		if !strings.Contains(d, want) {
			t.Errorf("body missing deterministic line %q\n---\n%s", want, d)
		}
	}

	// The concern boundary names the concern in the "What this is NOT" section.
	if !strings.Contains(d, "## What this is NOT") || !strings.Contains(d, "`scaffolder` concern") {
		t.Errorf("body missing concern boundary for scaffolder\n---\n%s", d)
	}

	// Suggested branch + title.
	if got.Branch != "peel/"+got.Slug {
		t.Errorf("Branch %q is not peel/<slug> for slug %q", got.Branch, got.Slug)
	}
	if !strings.HasPrefix(got.Title, "peel(scaffolder):") {
		t.Errorf("Title %q does not start with peel(scaffolder):", got.Title)
	}
}

// TestScaffoldPeelDossier_ReadBackByTrickleResolvers proves the scaffolded body
// round-trips through the trickle-gate resolvers that read it: the Operator:
// trailer, the dark-MR ancestry anchor, and the parsed dossier sections. This is
// the contract that keeps the scaffolder in lock-step with mr_trickle_gate.go.
func TestScaffoldPeelDossier_ReadBackByTrickleResolvers(t *testing.T) {
	got, err := ScaffoldPeelDossier(PeelScaffoldInput{
		DarkBeadID:    "gd-DARK1",
		DarkBeadTitle: "seed the summary",
		Concern:       "viewer render",
		Operator:      "Jane Operator",
		DarkMRIID:     "7",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	d := got.Description

	if op := TrickleOperatorTrailer(d); op != "Jane Operator" {
		t.Errorf("TrickleOperatorTrailer = %q, want %q", op, "Jane Operator")
	}
	if !TrickleDarkMRReferenced(d) {
		t.Error("TrickleDarkMRReferenced = false, want true (Dark-Bead:/Dark-MR: trailers present)")
	}

	dossier := ParseTrickleDossier(d)
	if len(dossier.MissingSections) != 0 {
		t.Errorf("ParseTrickleDossier reports missing sections %v, want none", dossier.MissingSections)
	}
	// Summary is real prose (seeded from the title); Why is a TODO stub that the
	// dossier body-cleaner strips to empty — the intended RED-until-filled signal.
	if dossier.Summary == "" {
		t.Error("Summary parsed empty; expected seeded prose")
	}
	if dossier.Why != "" {
		t.Errorf("Why parsed as %q; expected empty (human-owned TODO stub)", dossier.Why)
	}
}

// TestScaffoldPeelDossier_MissingEnrichmentDegrades proves the scaffold still
// renders (with TODO stubs) when the optional facts are absent.
func TestScaffoldPeelDossier_MissingEnrichmentDegrades(t *testing.T) {
	got, err := ScaffoldPeelDossier(PeelScaffoldInput{
		DarkBeadID: "gd-BARE",
		Concern:    "some-slice",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	d := got.Description

	// No dark MR IID -> a TODO Provenance line, and NO Dark-MR: trailer.
	if !strings.Contains(d, "- Dark MR: _TODO") {
		t.Errorf("expected TODO dark-MR provenance line when IID unknown\n---\n%s", d)
	}
	if strings.Contains(d, "\nDark-MR:") {
		t.Error("expected no Dark-MR: trailer when IID unknown")
	}
	// Operator unknown -> a TODO Operator: trailer.
	if !strings.Contains(d, "Operator: _TODO") {
		t.Errorf("expected TODO Operator: trailer when operator unknown\n---\n%s", d)
	}
	// Bead: trailer defaults to the dark bead when no request bead is given.
	if !strings.Contains(d, "Bead: gd-BARE") {
		t.Errorf("expected Bead: to default to the dark bead\n---\n%s", d)
	}
}

// TestPeelSlugify covers the git-ref-safe slug normalization.
func TestPeelSlugify(t *testing.T) {
	cases := map[string]string{
		"scaffolder":        "scaffolder",
		"Viewer Render":     "viewer-render",
		"  multi   space  ": "multi-space",
		"weird/chars!!":     "weird-chars",
		"---":               "",
		"":                  "",
	}
	for in, want := range cases {
		if got := PeelSlugify(in); got != want {
			t.Errorf("PeelSlugify(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestPeelSlug_Derivation proves the slug combines concern + dark bead, respects
// an override, and never collides with the protected target branch name.
func TestPeelSlug_Derivation(t *testing.T) {
	if got := peelSlug("scaffolder", "ka-0000001847", ""); got != "scaffolder-0000001847" {
		t.Errorf("peelSlug default = %q, want scaffolder-0000001847", got)
	}
	if got := peelSlug("scaffolder", "gd-ABC", "my-explicit-slug"); got != "my-explicit-slug" {
		t.Errorf("peelSlug override = %q, want my-explicit-slug", got)
	}
	// A slug that would resolve to the protected branch is dodged.
	if got := peelSlug("main", "gd-x", ""); strings.HasPrefix(got, "main") && got == "main" {
		t.Errorf("peelSlug must never yield the bare target branch name, got %q", got)
	}
	if got := peelSlug("", "gd-", ""); got == "" {
		t.Error("peelSlug must always yield a non-empty slug")
	}
}

// TestScaffoldPeelDossier_SlugInBranch ties the returned slug to a legal peel
// ref: no slashes, non-empty, not the bare target branch.
func TestScaffoldPeelDossier_SlugInBranch(t *testing.T) {
	got, err := ScaffoldPeelDossier(PeelScaffoldInput{DarkBeadID: "gd-Z", Concern: "main"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(got.Slug, "/") || got.Slug == "" || got.Slug == "main" {
		t.Errorf("illegal peel slug %q", got.Slug)
	}
}
