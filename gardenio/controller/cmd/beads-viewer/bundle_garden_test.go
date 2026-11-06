package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/gardenhub"
)

// The bundle panel's reader and the R6 retirement (Lands workbench-glass spec WB-9, §2.6; step 5.10, gd-UFvVkR7Mwx).

func bundleIDs(d *gardenhub.BundleDoc) string {
	var ids []string
	for _, p := range d.Docs {
		ids = append(ids, p.ID+":"+p.Format)
	}
	return strings.Join(ids, " ")
}

// The reader sends the documents /bundle/{id} shows, raw and in their position order (an unplaced one last), a report
// child as markdown, and skips what is not a document or is gone; the produces edge names the pull request, the first
// parent mention the bead it was filed against, and a session screenshot comes as its image proxy path.
func TestGardenBundleReadsTheDocs(t *testing.T) {
	parent := "parent"
	f := newBundleFake()
	f.beads["gd-bundle-full"].Status = "open"
	f.beads["gd-doc-late"] = &beadsapi.BeadDetail{ID: "gd-doc-late", Type: "doc", Title: "No place", Fields: map[string]string{"content": "x"}}
	f.beads["gd-rep-0001"] = &beadsapi.BeadDetail{ID: "gd-rep-0001", Type: "report", Title: "The report", Fields: map[string]string{"format": "markdown", "content": "# R", "position": "0"}}
	f.beads["gd-shot-001"] = &beadsapi.BeadDetail{ID: "gd-shot-001", Type: "doc", Fields: map[string]string{"format": "image", "position": "4", "headline": "Phone",
		"content_url": "https://sess.s3.amazonaws.com/coop/sessions/images/a%20b.png"}}
	f.beads["gd-task-0001"] = &beadsapi.BeadDetail{ID: "gd-task-0001", Type: "task", Title: "Not a doc"}
	f.beads["gd-mr-000001"] = &beadsapi.BeadDetail{ID: "gd-mr-000001", Type: "merge-request", Fields: map[string]string{
		"mr_url": "https://github.com/o/r/pull/12", "mr_iid": "12", "mr_state": "merged", "mr_title": "Ship it"}}
	f.revDeps["gd-bundle-full"] = append(f.revDeps["gd-bundle-full"],
		beadsapi.Dependency{BeadID: "gd-doc-late", Type: "parent-child"},
		beadsapi.Dependency{BeadID: "gd-rep-0001", Type: "parent-child"},
		beadsapi.Dependency{BeadID: "gd-shot-001", Type: "parent-child"},
		beadsapi.Dependency{BeadID: "gd-task-0001", Type: "parent-child"},
		beadsapi.Dependency{BeadID: "gd-gone-0001", Type: "parent-child"},
		beadsapi.Dependency{BeadID: "gd-mr-000001", Type: "produces"},
	)
	f.mentions = map[string][]beadsapi.BeadReferenceSummary{"gd-bundle-full": {{ID: "gd-epic-0001", Title: "The epic", RelationshipType: &parent}}}
	srv := NewServer(f, slog.Default(), "/v", "", "", nil)
	srv.imageBucket, srv.imagePrefix = "sess", "coop/sessions"

	d, err := gardenBundles{srv}.Bundle(context.Background(), "gd-bundle-full")
	if err != nil {
		t.Fatal(err)
	}
	if want := "gd-rep-0001:md gd-doc-aaa:md gd-doc-bbb:diff gd-doc-ccc:code gd-shot-001:image gd-doc-late:"; bundleIDs(d) != want {
		t.Errorf("docs %q, want %q", bundleIDs(d), want)
	}
	if d.Title != "Three-doc bundle" || d.Kind != "plan" || d.Status != "open" || d.Description != "Bundle description text." || len(d.Partial) != 0 {
		t.Errorf("bundle %+v", d)
	}
	if d.Docs[1].Title != "First doc headline" || d.Docs[1].Content != "first content" || d.Docs[0].Title != "The report" {
		t.Errorf("headline, else title; content raw: %+v, %+v", d.Docs[1], d.Docs[0])
	}
	if got := d.Docs[4].URL; got != "/v/image/coop/sessions/images/a%20b.png" {
		t.Errorf("screenshot url %q, want the proxy path", got)
	}
	if d.MR == nil || d.MR.URL != "https://github.com/o/r/pull/12" || d.MR.Number != "12" || d.MR.State != "merged" || d.MR.Title != "Ship it" {
		t.Errorf("mr %+v", d.MR)
	}
	if d.Parent == nil || d.Parent.ID != "gd-epic-0001" || d.Parent.Title != "The epic" {
		t.Errorf("parent %+v", d.Parent)
	}

	// what is not a bundle, or not there, is not found; a daemon failure on the documents says so and keeps the bundle
	for _, id := range []string{"gd-doc-aaa", "gd-nothere99"} {
		var ae *beadsapi.APIError
		if _, err := (gardenBundles{srv}).Bundle(context.Background(), id); !errors.As(err, &ae) || ae.StatusCode != http.StatusNotFound {
			t.Errorf("%s: %v, want a 404", id, err)
		}
	}
	f.batchGetBeadsErr = errors.New("down")
	if d, err = (gardenBundles{srv}).Bundle(context.Background(), "gd-bundle-full"); err != nil || len(d.Docs) != 0 || strings.Join(d.Partial, ",") != "docs" {
		t.Errorf("batch down: err %v docs %d partial %v", err, len(d.Docs), d.Partial)
	}
	f.getBeadErr = errors.New("down")
	if _, err = (gardenBundles{srv}).Bundle(context.Background(), "gd-bundle-full"); err == nil {
		t.Error("the bundle bead unreadable: want an error")
	}
}

// The bundle page (R6) says "Open it in the garden" to a viewer who reaches /garden (default on), and with its redirect
// flag on answers 302 to the bundle panel, the page's own query kept; an htmx swap keeps the page, and the flags are its
// own.
func TestGardenBundleRetirement(t *testing.T) {
	newSrv := func(t *testing.T, env map[string]string) http.Handler {
		srv := NewServer(newBundleFake(), slog.Default(), "/v", "", "", nil)
		if env != nil {
			srv.tunables = newWatcherWithEnv(t, env)
		}
		srv.SetGardenHandler(&gardenhub.Handler{Hub: gardenhub.New(nil, "/v", nil), BasePath: "/v"})
		return edgeAuthMux(srv)
	}
	get := func(h http.Handler, path string, hx bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("X-Auth-Request-Email", "alice@example.com")
		if hx {
			req.Header.Set("HX-Request", "true")
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}
	banner := func(w *httptest.ResponseRecorder) bool {
		return strings.Contains(w.Body.String(), `id="garden-banner"`)
	}

	if w := get(newSrv(t, nil), "/bundle/gd-bundle-full", false); w.Code != http.StatusOK || !banner(w) || !strings.Contains(w.Body.String(), `href="/v/garden?bundle=gd-bundle-full"`) {
		t.Errorf("banner on by default: %d, banner %v", w.Code, banner(w))
	}
	if w := get(newSrv(t, map[string]string{gardenBundleBannerEnv: "false"}), "/bundle/gd-bundle-full", false); w.Code != http.StatusOK || banner(w) {
		t.Errorf("banner flag off: %d, banner %v", w.Code, banner(w))
	}
	if w := get(newSrv(t, map[string]string{gardenBeadBannerEnv: "false"}), "/bundle/gd-bundle-full", false); !banner(w) {
		t.Error("the bead banner's flag does not turn off the bundle one")
	}

	redirect := newSrv(t, map[string]string{gardenBundleRedirectEnv: "true"})
	for path, want := range map[string]string{
		"/bundle/gd-bundle-full":                  "/v/garden?bundle=gd-bundle-full",
		"/bundle/gd-bundle-full?at=gardenio&q=no": "/v/garden?at=gardenio&bundle=gd-bundle-full",
	} {
		if w := get(redirect, path, false); w.Code != http.StatusFound || w.Header().Get("Location") != want {
			t.Errorf("%s: %d to %q, want 302 to %q", path, w.Code, w.Header().Get("Location"), want)
		}
	}
	if w := get(redirect, "/bundle/gd-bundle-full", true); w.Code != http.StatusOK {
		t.Errorf("an htmx swap with the redirect on: %d, want the page", w.Code)
	}
	if w := get(newSrv(t, nil), "/bundle/gd-bundle-full", false); w.Code != http.StatusOK {
		t.Errorf("redirect off by default: %d, want the page", w.Code)
	}
}
