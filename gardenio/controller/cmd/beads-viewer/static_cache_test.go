package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"
)

// /assets/static/ answers with a content-hash ETag and no-cache, so a revisit of an unchanged file is a bodyless 304
// and a changed file is a new tag (P1.5, gd-YU9gGOMvur). Without it http.FileServer over the embed FS sent no
// validator at all, and every visit downloaded xterm.bundle.js and the page scripts again.
func TestStaticAssetsRevalidateByContentHash(t *testing.T) {
	fsys := fstest.MapFS{"xterm.bundle.js": {Data: []byte("var Terminal = 1;")}, "pwa/icon-192.png": {Data: []byte("png")}}
	h := http.StripPrefix("/assets/static/", staticCached(fsys))
	get := func(p, inm string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", p, nil)
		if inm != "" {
			req.Header.Set("If-None-Match", inm)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	first := get("/assets/static/xterm.bundle.js", "")
	etag := first.Header().Get("ETag")
	if first.Code != 200 || first.Body.String() != "var Terminal = 1;" || len(etag) != 34 || first.Header().Get("Cache-Control") != "no-cache" {
		t.Fatalf("first: code %d, ETag %q, Cache-Control %q, body %q", first.Code, etag, first.Header().Get("Cache-Control"), first.Body)
	}
	if again := get("/assets/static/xterm.bundle.js", etag); again.Code != http.StatusNotModified || again.Body.Len() != 0 {
		t.Errorf("revisit: code %d, %d bytes", again.Code, again.Body.Len())
	}
	if stale := get("/assets/static/xterm.bundle.js", `"old"`); stale.Code != 200 {
		t.Errorf("a changed file: code %d", stale.Code)
	}
	nested := get("/assets/static/pwa/icon-192.png", "")
	if nested.Header().Get("ETag") == "" || nested.Header().Get("ETag") == etag {
		t.Errorf("a nested file's ETag %q (xterm's %q)", nested.Header().Get("ETag"), etag)
	}
	if missing := get("/assets/static/nope.js", ""); missing.Code != 404 || missing.Header().Get("ETag") != "" {
		t.Errorf("a missing file: code %d, ETag %q", missing.Code, missing.Header().Get("ETag"))
	}
}

// /assets/static/ still serves xterm.js by its old names from gardenhub (overlayFS), tagged like the viewer's own files,
// so the agent page's terminal loads after the files moved to gardenhub/web/vendor (gd-BC3yZxTQ65).
func TestStaticServesGardenhubXterm(t *testing.T) {
	base := fstest.MapFS{"agent_terminal.js": {Data: []byte("1")}, "xterm.css": {Data: []byte("old")}, "pwa/icon-192.png": {Data: []byte("png")}}
	top := fstest.MapFS{"xterm.bundle.js": {Data: []byte("var Terminal = 1;")}, "xterm.css": {Data: []byte(".xterm{}")}}
	h := http.StripPrefix("/assets/static/", staticCached(overlayFS{base, top}))
	for p, want := range map[string]string{"xterm.bundle.js": "var Terminal = 1;", "xterm.css": ".xterm{}", "agent_terminal.js": "1", "pwa/icon-192.png": "png"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "/assets/static/"+p, nil))
		if rec.Code != 200 || rec.Body.String() != want || rec.Header().Get("ETag") == "" {
			t.Errorf("%s: code %d, ETag %q, body %q, want %q", p, rec.Code, rec.Header().Get("ETag"), rec.Body, want)
		}
	}
}
