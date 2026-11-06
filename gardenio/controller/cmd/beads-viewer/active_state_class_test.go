package main

import (
	"strings"
	"testing"
)

// active_state_class_test.go — ka-0000005672 (audit F2).
//
// WHY THIS EXISTS. The beads-viewer filter/tab strips grew FOUR different
// class names for the SAME semantic — "this control is the currently-selected
// one":
//
//  1. `chip-active` — the pill-chip strips (project, project_hub, inbox,
//     triggers, agents, extern, search, plans). This is the canonical token
//     for the .chip component and stays; it is single-sourced via layout.html
//     `.chip-active` and owned by the F1/F5/F6 chip-strip work.
//  2. `is-active`   — the non-chip state modifier already shared across
//     ratings tabs, charts hub chips, maps scope/chips/toggles, gardeners
//     sort, deck slots, and the layout "Me" nav link.
//  3. `hub-tab-active` — a THIRD name, used ONLY by the project section tabs
//     (project.html / project_hub.html). Ad hoc; nothing else used it.
//  4. a bare `active` — a FOURTH name, used ONLY by the /triggers density
//     toggle and sort links. Ad hoc and dangerously generic.
//
// The F2 fix RETIRES the two ad-hoc outliers (#3 and #4) by folding them onto
// the shared `.is-active` state token. `.chip-active` (#1) is intentionally
// left in place: it is the chip component's own modifier, already consistent
// across every chip strip, and converging it is the job of the chip-strip
// consolidation (F1/F5/F6), not this bug.
//
// After F2 the codebase has exactly TWO active-state idioms, each with a clear
// owner: `.chip-active` on .chip pills, and `.is-active` everywhere else. This
// guard pins that: the retired class names must not reappear in any template,
// and the two converged surfaces must carry `.is-active`.

// retiredActiveClasses are the ad-hoc active-state class names F2 eliminated.
// Any reappearance in a template (markup, CSS, or inline JS) re-forks the
// active-state idiom and is caught here at author time.
var retiredActiveClasses = []struct {
	class   string
	comment string
}{
	{"hub-tab-active", "project section tabs — folded onto .is-active (use `hub-tab is-active`)"},
}

func TestActiveStateClass_RetiredClassesGone(t *testing.T) {
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
	// Also cover the one partial subdir we template from.
	if sub, err := templateFS.ReadDir("templates/partials"); err == nil {
		for _, e := range sub {
			if !e.IsDir() {
				files = append(files, "templates/partials/"+e.Name())
			}
		}
	}
	for _, rc := range retiredActiveClasses {
		for _, f := range files {
			src, err := templateFS.ReadFile(f)
			if err != nil {
				t.Fatalf("read %s: %v", f, err)
			}
			if strings.Contains(string(src), rc.class) {
				t.Errorf("%s still references retired active-state class %q.\n"+
					"F2 converged active-state idioms: use `.is-active` (%s).",
					f, rc.class, rc.comment)
			}
		}
	}
}

// convergedActiveSurfaces are the two surfaces F2 moved onto `.is-active`.
// They must now carry the canonical state token so the selected control keeps
// its highlight after the rename.
var convergedActiveSurfaces = []struct {
	file    string
	marker  string
	comment string
}{
	// project.html + project_hub.html section tabs are now single-sourced in the
	// shared section_tabs partial (ka-0000006647 / audit F7); the `.is-active`
	// token lives there once instead of duplicated in both pages.
	{"partials/section_tabs.html", `{{if .Active}} is-active{{end}}`, "shared section tab"},
	{"triggers.html", `{{if .Active}}class="is-active" aria-current="true"{{end}}`, "triggers density toggle"},
	{"triggers.html", `{{if .Active}} class="is-active"{{end}}`, "triggers sort link"},
}

func TestActiveStateClass_ConvergedSurfacesUseIsActive(t *testing.T) {
	for _, s := range convergedActiveSurfaces {
		src, err := templateFS.ReadFile("templates/" + s.file)
		if err != nil {
			t.Fatalf("read %s: %v", s.file, err)
		}
		if !strings.Contains(string(src), s.marker) {
			t.Errorf("%s (%s): converged surface missing canonical `.is-active` token.\n"+
				"expected substring: %q", s.file, s.comment, s.marker)
		}
	}
}
