package bridge

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoTemplateFilenameInViewerURLs is the cheap belt-and-suspenders companion
// to the mux-resolution guard in cmd/beads-viewer (task ka-0000001512). The
// mux-resolution test proves the SHARED helper's shape resolves; this test
// stops anyone re-inlining the original mistake at a call site the shared helper
// no longer covers.
//
// The bug (epic ka-0000000887): a viewer TEMPLATE FILENAME — "/ui/escalations.html"
// — was concatenated with a viewer base URL (beadsWebURL / beadsViewerURL) and
// shipped as a link. The viewer serves those templates at extension-less ROUTE
// paths ("/escalations"), so the filename shape 404s.
//
// The rule: in this package, a line that builds a URL from a viewer base var
// must not carry a "/ui/" segment or a ".html" template-filename literal. This
// is intentionally line-scoped to viewer-base-URL construction so it does NOT
// flag the bridge's own /ui/ SPA mount (cmd/slack-bridge) or unrelated HTML.
func TestNoTemplateFilenameInViewerURLs(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}

	// The viewer base URLs a link is built from. A line mentioning one of these
	// is building a URL toward the beads-viewer host.
	viewerBaseVars := []string{"beadsWebURL", "beadsViewerURL"}
	// Template-filename / bridge-SPA-path tells that must never appear in such a
	// line — the viewer registers neither a "/ui/" prefix nor a ".html" route.
	forbidden := []string{"/ui/", ".html"}

	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue // tests may legitimately reference the dead shape as a control
		}
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		for i, line := range strings.Split(string(data), "\n") {
			var buildsViewerURL bool
			for _, v := range viewerBaseVars {
				if strings.Contains(line, v) {
					buildsViewerURL = true
					break
				}
			}
			if !buildsViewerURL {
				continue
			}
			for _, bad := range forbidden {
				if strings.Contains(line, bad) {
					t.Errorf("%s:%d builds a beads-viewer URL with a template-filename/SPA literal %q — the viewer serves pages at extension-less routes, so this 404s. Route through EscalationReviewURL / a real route path instead (epic ka-0000000887).\n  %s",
						f, i+1, bad, strings.TrimSpace(line))
				}
			}
		}
	}
}
