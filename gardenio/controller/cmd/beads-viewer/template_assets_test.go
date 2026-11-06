package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// TestNewServer_SharesParsedTemplates guards the loadTemplateAssets memoization
// (bug ka-0000005687): parsing the ~50-page template set costs ~0.7s of CPU,
// and the test suite constructs ~1100 Servers — re-parsing per construction is
// what pushed the suite past its package timeout on loaded machines. Two
// Servers with the same basePath must share the same parsed *Template values;
// a different basePath must get its own parse (the func map closes over it).
func TestNewServer_SharesParsedTemplates(t *testing.T) {
	a := NewServer(newFake(), testLogger(), "", "", "", &fakeScreenClient{})
	b := NewServer(newFake(), testLogger(), "", "", "", &fakeScreenClient{})
	if a.pages["feed.html"] != b.pages["feed.html"] {
		t.Error("same-basePath Servers must share the parsed page templates")
	}
	if a.partials["templates/partials/comments.html"] != b.partials["templates/partials/comments.html"] {
		t.Error("same-basePath Servers must share the parsed partial templates")
	}
	if a.widgetTemplates != b.widgetTemplates {
		t.Error("same-basePath Servers must share the parsed widget templates")
	}
	// The maps themselves must be per-Server copies so a test injecting a
	// probe page (vibe_mode_render_test.go) can't bleed into other Servers.
	a.pages["__probe_isolation__"] = nil
	if _, ok := b.pages["__probe_isolation__"]; ok {
		t.Error("pages map must be a per-Server copy, not the shared cache map")
	}

	c := NewServer(newFake(), testLogger(), "/beads", "", "", &fakeScreenClient{})
	if c.pages["feed.html"] == a.pages["feed.html"] {
		t.Error("different-basePath Servers must not share templates (funcs close over basePath)")
	}
}

// TestSharedTemplates_SurviveRenderAcrossServers proves the shared parsed set
// stays clonable after renders: html/template forbids Clone-after-Execute, so
// any code path that executed a shared template directly (instead of cloning
// first) would break every subsequent render on every other Server. Rendering
// the same page on two Servers back-to-back exercises exactly that.
func TestSharedTemplates_SurviveRenderAcrossServers(t *testing.T) {
	for i := range 2 {
		f := newFake()
		f.listResp = &beadsapi.ListBeadsResult{}
		srv := NewServer(f, testLogger(), "", "", "", &fakeScreenClient{})
		mux := http.NewServeMux()
		srv.RegisterRoutes(mux)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest("GET", "/agents", nil))
		if rec.Code != 200 {
			t.Fatalf("render %d: /agents = %d, want 200", i, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "<html") {
			t.Fatalf("render %d: /agents did not produce a page", i)
		}
	}
}
