package main

import (
	"strings"
	"testing"
)

// subnav_consolidation_test.go — ka-0000006647 (audit finding F7, umbrella).
//
// WHY THIS EXISTS. F1-F6 consolidated the UNLABELED page-level chip menus into
// one `filter_strip` partial. F7 folds the remaining sub-nav families — the
// LABELED multi-row control panels and the page-section tab bars — into two
// more shared partials, so every sub-nav on beads-viewer now flows through a
// single component instead of per-page hand-rolled markup:
//
//   * filter_strip  — unlabeled chip menu    (project, agents, triggers, inbox, ratings)
//   * filter_row    — labeled control panel   (plans, extern)
//   * section_tabs  — page-section tab bar     (project, project_hub)
//
// These guards pin the consolidation: the retired per-page idiom classes must
// not reappear (a new page re-forking the sub-nav is caught at author time),
// and the shared partials + CSS hooks must stay wired.

// retiredSubnavIdioms are the per-page sub-nav class names / template defines
// F7 eliminated. Any reappearance in a template re-forks the "different menu bar
// per page" drift the audit flagged.
var retiredSubnavIdioms = []struct {
	token   string
	comment string
}{
	{"rating-source-tabs", "/ratings source tabs — fold into the filter_strip partial"},
	{"plan-control-group", "/plans labeled rows — fold into the filter_row partial"},
	{"plan-control-label", "/plans row labels — use .filter-row-label"},
	{`class="plan-filters"`, "/plans control container — use .filter-rows"},
	{"extern-control-group", "/extern labeled rows — fold into the filter_row partial"},
	{"extern-control-label", "/extern row labels — use .filter-row-label"},
	{`class="extern-filters"`, "/extern control container — use .filter-rows"},
	{"project-hub-tabs", "project section tabs — fold into the section_tabs partial"},
	{`"hub-tab"`, "project section tab links — use .section-tab (in section_tabs)"},
	{`define "planControlRow"`, "retired — replaced by the shared filter_row partial"},
	{`define "extern_controlrow"`, "retired — replaced by the shared filter_row partial"},
}

// TestSubnavConsolidation_RetiredIdiomsGone asserts no template still carries a
// retired per-page sub-nav idiom. The shared partials (filter_row.html,
// section_tabs.html) legitimately mention the retired names in their doc
// comments, and layout.html documents the migration, so those are exempt.
func TestSubnavConsolidation_RetiredIdiomsGone(t *testing.T) {
	exempt := map[string]bool{
		"templates/partials/filter_row.html":   true,
		"templates/partials/section_tabs.html": true,
		"templates/layout.html":                true, // migration notes in CSS comments
	}
	files := allTemplateFiles(t)
	for _, ri := range retiredSubnavIdioms {
		for _, f := range files {
			if exempt[f] {
				continue
			}
			src, err := templateFS.ReadFile(f)
			if err != nil {
				t.Fatalf("read %s: %v", f, err)
			}
			if strings.Contains(string(src), ri.token) {
				t.Errorf("%s still references retired sub-nav idiom %q.\n"+
					"F7 consolidated the sub-nav into shared partials: %s.",
					f, ri.token, ri.comment)
			}
		}
	}
}

// sharedSubnavPartials are the F7 components and the marker that proves each is
// still the single source of its sub-nav family.
var sharedSubnavPartials = []struct {
	file   string
	define string
}{
	{"templates/partials/filter_row.html", `{{- define "filter_row" -}}`},
	{"templates/partials/section_tabs.html", `{{- define "section_tabs" -}}`},
}

// TestSubnavConsolidation_SharedPartialsDefined asserts the F7 partials exist
// and still define their template — a page templating an absent define fails to
// parse (server.go Must-panics), but this catches an accidental rename early.
func TestSubnavConsolidation_SharedPartialsDefined(t *testing.T) {
	for _, p := range sharedSubnavPartials {
		src, err := templateFS.ReadFile(p.file)
		if err != nil {
			t.Fatalf("read %s: %v", p.file, err)
		}
		if !strings.Contains(string(src), p.define) {
			t.Errorf("%s missing its define %q — the shared sub-nav component is gone.", p.file, p.define)
		}
	}
}

// TestSubnavConsolidation_HooksStyled asserts the shared CSS hooks the F7
// partials render against are actually defined in layout.html — a hook nothing
// styles is a silent no-op (same failure mode the F5 guard pins).
func TestSubnavConsolidation_HooksStyled(t *testing.T) {
	layout, err := templateFS.ReadFile("templates/layout.html")
	if err != nil {
		t.Fatalf("read layout.html: %v", err)
	}
	for _, hook := range []string{".filter-rows {", ".filter-row {", ".filter-row-label {", ".section-tabs {", ".section-tab {"} {
		if !strings.Contains(string(layout), hook) {
			t.Errorf("layout.html missing CSS rule for shared sub-nav hook %q — the F7 hook is inert without it.", hook)
		}
	}
}

// allTemplateFiles returns every template path under templates/ and
// templates/partials/ (mirrors the F2 guard's enumeration).
func allTemplateFiles(t *testing.T) []string {
	t.Helper()
	entries, err := templateFS.ReadDir("templates")
	if err != nil {
		t.Fatalf("read templates dir: %v", err)
	}
	var files []string
	for _, e := range entries {
		if !e.IsDir() {
			files = append(files, "templates/"+e.Name())
		}
	}
	sub, err := templateFS.ReadDir("templates/partials")
	if err != nil {
		t.Fatalf("read partials dir: %v", err)
	}
	for _, e := range sub {
		if !e.IsDir() {
			files = append(files, "templates/partials/"+e.Name())
		}
	}
	return files
}
