package decisionview

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The invariant (epic gd-hmwSI9lwKe): a template that renders a decision
// RESOLVE CONTROL renders the decision's CONTEXT in the same template. A
// person is never asked to answer a question whose background they cannot
// see on that surface.
//
// The test enumerates every template unit that posts a resolve, across the
// three consumers of this package, and fails when one has no context block.
// A unit is a beads-viewer template file, or one {{define}} block of a Go
// file that holds its templates as strings (gardenhub, commons).
//
// knownGaps is the list of units that still break the rule; steps 2 and 3
// of the epic (gd-PNtilz1bgs, gd-OSI1uK35Wa) add the context block to each
// and remove it from the list. The list is checked EXACTLY: a unit that
// gains a context block must be removed, so the list never goes stale, and
// a new surface that posts a resolve without context cannot land unlisted.
var knownGaps = map[string]string{}

// resolveControl matches the markup every resolve control carries: the
// viewer's chips and one-tap forms (POST /b/{id}/decision/resolve,
// POST /plans/{id}/decisions/{d}/resolve, the /up keyboard chips), the
// Mailbox's data-act="resolve" buttons and the Commons card's resolve
// action and lightbox submit.
var resolveControl = regexp.MustCompile(
	`decision-resolve-chip|name="option_id"|data-up-option|/decision/resolve|/decisions/[^"\s]*/resolve|data-act="resolve"|name="action" value="resolve"|/garden/c/submit/`)

// contextBlock matches a template reading the decision's context: a
// .Context / .ContextHTML / .ContextText field or the decision_context
// partial.
var contextBlock = regexp.MustCompile(`\.(D\.)?(Context|ContextHTML|ContextText)\b|template "decision_context"`)

// evidenceSlot matches a template drawing the decision's evidence strip
// (gd-Pi7C83262N): an .Evidence / .EvidenceHTML field, which every consumer
// fills from decisionview.EvidenceStrip.
var evidenceSlot = regexp.MustCompile(`\.(D\.)?(Evidence|EvidenceHTML)\b`)

var defineRe = regexp.MustCompile(`\{\{-?\s*define\s+"([^"]+)"`)

type unit struct{ name, text string }

// units enumerates the template units of the three consumers.
func units(t *testing.T) []unit {
	t.Helper()
	var out []unit
	viewer := filepath.Join("..", "..", "cmd", "beads-viewer", "templates")
	for _, pat := range []string{"*.html", filepath.Join("partials", "*.html")} {
		files, err := filepath.Glob(filepath.Join(viewer, pat))
		if err != nil || len(files) == 0 {
			t.Fatalf("no viewer templates at %s/%s: %v", viewer, pat, err)
		}
		for _, f := range files {
			rel, _ := filepath.Rel(viewer, f)
			out = append(out, unit{"beads-viewer/" + filepath.ToSlash(rel), read(t, f)})
		}
	}
	for _, f := range []string{filepath.Join("..", "gardenhub", "render.go"), filepath.Join("..", "commons", "render.go")} {
		name := filepath.ToSlash(filepath.Join(filepath.Base(filepath.Dir(f)), filepath.Base(f)))
		out = append(out, splitDefines(name, read(t, f))...)
	}
	return out
}

// splitDefines cuts a Go file holding templates into one unit per
// {{define "name"}} block (the text up to the next define), with the text
// before the first define as the file itself.
func splitDefines(name, text string) []unit {
	locs := defineRe.FindAllStringSubmatchIndex(text, -1)
	if len(locs) == 0 {
		return []unit{{name, text}}
	}
	out := []unit{{name, text[:locs[0][0]]}}
	for i, loc := range locs {
		end := len(text)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		out = append(out, unit{name + "#" + text[loc[2]:loc[3]], text[loc[0]:end]})
	}
	return out
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func TestEveryResolveControlHasContext(t *testing.T) {
	var withControl []string
	gaps := map[string]bool{}
	for _, u := range units(t) {
		if !resolveControl.MatchString(u.text) {
			continue
		}
		withControl = append(withControl, u.name)
		if !contextBlock.MatchString(u.text) {
			gaps[u.name] = true
		}
	}
	sort.Strings(withControl)
	if len(withControl) < 8 {
		t.Fatalf("expected at least the eight known resolve surfaces, found %d: %v (did a marker change?)", len(withControl), withControl)
	}
	t.Logf("resolve surfaces: %s", strings.Join(withControl, ", "))
	for name := range gaps {
		if _, ok := knownGaps[name]; !ok {
			t.Errorf("%s renders a decision resolve control with no context block (no .Context/.ContextHTML/.ContextText, no decision_context partial): a decision is never answered without its context (gd-hmwSI9lwKe)", name)
		}
	}
	for name, step := range knownGaps {
		if !gaps[name] {
			t.Errorf("%s now has a context block (or no resolve control): remove it from knownGaps (it was waiting on %s)", name, step)
		}
	}
}

// TestEveryResolveControlHasEvidence extends the invariant to the evidence
// strip (gd-Pi7C83262N): a unit with a resolve control renders the evidence
// slot too, so the screenshots of the bead a decision is about show wherever
// it is answered. No unit is excused.
func TestEveryResolveControlHasEvidence(t *testing.T) {
	n := 0
	for _, u := range units(t) {
		if !resolveControl.MatchString(u.text) {
			continue
		}
		n++
		if !evidenceSlot.MatchString(u.text) {
			t.Errorf("%s renders a decision resolve control with no evidence slot (no .Evidence/.EvidenceHTML): a decision shows the screenshots of the bead it is about wherever it is answered (gd-Pi7C83262N)", u.name)
		}
	}
	if n < 8 {
		t.Fatalf("expected at least the eight known resolve surfaces, found %d (did a marker change?)", n)
	}
}

func TestSplitDefines(t *testing.T) {
	got := splitDefines("f.go", `head {{define "a"}}A{{end}} {{- define "b"}}B{{end}}`)
	if len(got) != 3 || got[0].name != "f.go" || got[1].name != "f.go#a" || !strings.Contains(got[1].text, "A") || strings.Contains(got[1].text, "B") || got[2].name != "f.go#b" {
		t.Errorf("%+v", got)
	}
}
