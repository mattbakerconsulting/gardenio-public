package main

import (
	"strings"
	"testing"
)

// TestTemplatesDeclareBodyWidthClass enforces that every top-level template
// using {{template "layout" .}} declares an explicit body-width class so the
// baseline / minimal-refined / docs-notion / tufte skins can scope their
// max-width via body:has(...) selectors.
//
// See ka-0000004713: before this guardrail, new admin pages silently fell
// back to the narrow reading column (820–960px) instead of widening to fill
// the viewport. Declaring intent at the template level — rather than relying
// on opt-in — makes new-page width regressions impossible to merge.
//
// Allowed marker classes (one per template, anywhere in the file):
//
//   wide-page         — full-width admin/listing surfaces (default expectation)
//   reading-col       — narrow reading column (login flows, rendered docs)
//   inbox-page        — the /inbox shell (custom width)
//   inbox-stream      — inbox stream sub-surfaces
//   bead-show-layout  — the per-bead detail page
//   rich-report-layout — embedded rich-report partials at top level
//   advice-layout     — the advice surface
//
// Skipped: layout.html (the layout itself) and login.html (defines its own
// standalone template that does not include layout).
func TestTemplatesDeclareBodyWidthClass(t *testing.T) {
	const dir = "templates"
	entries, err := templateFS.ReadDir(dir)
	if err != nil {
		t.Fatalf("read embedded templates dir: %v", err)
	}

	skip := map[string]bool{
		"layout.html": true,
		"login.html":  true,
	}

	markers := []string{
		"wide-page",
		"reading-col",
		"inbox-page",
		"inbox-stream",
		"bead-show-layout",
		"rich-report-layout",
		"advice-layout",
	}

	var checked int
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".html") {
			continue
		}
		if skip[name] {
			continue
		}
		body, err := templateFS.ReadFile(dir + "/" + name)
		if err != nil {
			t.Errorf("%s: read: %v", name, err)
			continue
		}
		src := string(body)
		// Only enforce on templates that compose the shared layout — standalone
		// {{define "..."}} templates (e.g. login.html) are excluded by the
		// skip map above; this is a belt-and-suspenders check for any future
		// standalone template we forget to skip.
		if !strings.Contains(src, `{{template "layout" .}}`) {
			continue
		}
		checked++
		var found bool
		for _, m := range markers {
			if strings.Contains(src, m) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("%s: missing body-width marker class — add one of %v "+
				"(e.g. `<span class=\"wide-page\" hidden></span>` right after `{{define \"content\"}}`); "+
				"see ka-0000004713 for context", name, markers)
		}
	}

	if checked == 0 {
		t.Fatal("no layout-composing templates were checked — embed.FS wiring may have regressed")
	}
}
