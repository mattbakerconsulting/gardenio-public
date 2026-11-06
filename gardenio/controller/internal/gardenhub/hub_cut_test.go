package gardenhub

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// The /garden page's lists for a project-scoped viewer (gd-l7yYxZpoZo): the hub's items cut to the viewer's projects,
// an unlabelled item dropped, the counts over what shows, and data-cut so the page does not open the hub's stream. An
// unrestricted viewer's page is byte for byte the page with no cut hook at all.
func TestPageListsCutPerViewer(t *testing.T) {
	in := func(b *beadsapi.BeadDetail, labels ...string) *beadsapi.BeadDetail { b.Labels = labels; return b }
	f := newFake()
	f.set(
		[]*beadsapi.BeadDetail{
			in(bead("gd-work", "task", 5), "project:gardenio"),
			in(bead("cl-work", "task", 6), "project:claimlayer"),
			in(bead("gd-agent", "agent", 7), "project:gardenio"),
			in(bead("cl-agent", "agent", 8), "project:claimlayer"),
			in(bead("nl-work", "task", 9)),
		},
		[]*beadsapi.BeadDetail{
			in(bead("gd-ask", "decision", 10), "project:gardenio"),
			in(bead("cl-ask", "decision", 11), "project:claimlayer"),
		},
	)
	h := New(f, "/v", nil)
	if err := h.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}

	cuts := map[string]FarmCut{
		"claimlayer": {Scoped: true, Projects: []string{"claimlayer"}},
		"both":       {Scoped: true, Projects: []string{"claimlayer", "gardenio"}},
		"none":       {Scoped: true},
	}
	serve := func(hd *Handler, who string) *httptest.ResponseRecorder {
		t.Helper()
		mux := http.NewServeMux()
		hd.Register(mux)
		req := httptest.NewRequest("GET", "/garden", nil)
		req.Header.Set("X-Who", who)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /garden as %q = %d", who, rec.Code)
		}
		return rec
	}
	hd := &Handler{Hub: h, BasePath: "/v", Cut: func(r *http.Request) FarmCut { return cuts[r.Header.Get("X-Who")] }}

	all := []string{"i-gd-work", "i-cl-work", "i-gd-agent", "i-cl-agent", "i-nl-work", "i-gd-ask", "i-cl-ask"}
	admin := serve(hd, "admin")
	for _, id := range all {
		if !strings.Contains(admin.Body.String(), id) {
			t.Fatalf("the admin page lacks %s: the fixture does not reach the lists", id)
		}
	}
	if strings.Contains(admin.Body.String(), "data-cut") {
		t.Error("the admin page says its lists are cut")
	}
	if plain := serve(&Handler{Hub: h, BasePath: "/v"}, "admin"); plain.Body.String() != admin.Body.String() || plain.Header().Get("ETag") != admin.Header().Get("ETag") {
		t.Error("an unrestricted viewer's page differs from the page with no cut hook")
	}

	mine := serve(hd, "claimlayer")
	body := mine.Body.String()
	for _, id := range []string{"i-cl-work", "i-cl-agent", "i-cl-ask"} {
		if !strings.Contains(body, id) {
			t.Errorf("the claimlayer page lacks its own %s", id)
		}
	}
	for _, leak := range []string{"gd-work", "gd-agent", "gd-ask", "nl-work"} {
		if strings.Contains(body, leak) {
			t.Errorf("the claimlayer page carries %s", leak)
		}
	}
	if !strings.Contains(body, `data-cut="1"`) {
		t.Error("the claimlayer page does not say its lists are cut")
	}
	if snap := h.CutSnapshot(cuts["claimlayer"]); snap.Counts[SecWork] != 1 || snap.Counts[SecAgent] != 1 || snap.Counts[SecDecision] != 1 {
		t.Errorf("claimlayer counts = %v, want one in each section", snap.Counts)
	}

	none := serve(hd, "none").Body.String()
	for _, id := range all {
		if strings.Contains(none, id) {
			t.Errorf("a viewer with no projects gets %s", id)
		}
	}

	// One hub version, two project sets: the ETags differ, so a revisit never 304s onto another set's lists.
	if serve(hd, "both").Header().Get("ETag") == mine.Header().Get("ETag") {
		t.Error("two project sets share an ETag at one hub version")
	}
}

// The ETag's viewer part is a 32-bit hash, so two viewers' ETags can collide (a scoped member can pick an email offline
// whose hash meets an admin's). The gzip page cache is keyed on the whole viewer, not the ETag: the member whose ETag
// equals the admin's still gets its own cut page, never the admin's cached one (merge desk on PR #1668).
func TestPageGzipNotSharedOnETagCollision(t *testing.T) {
	in := func(b *beadsapi.BeadDetail, labels ...string) *beadsapi.BeadDetail { b.Labels = labels; return b }
	f := newFake()
	f.set([]*beadsapi.BeadDetail{
		in(bead("gd-work", "task", 5), "project:gardenio"),
		in(bead("cl-work", "task", 6), "project:claimlayer"),
	}, nil)
	h := New(f, "/v", nil)
	if err := h.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}

	// A birthday search for an admin and a claimlayer member whose page hashes meet: about 2^16 tries a side.
	member := pageViewer{flags: 1 << 7, scoped: true, cut: "claimlayer"} // read-only, cut to claimlayer
	admins := map[uint32]string{}
	for i := range 1 << 17 {
		me := fmt.Sprintf("admin-%d@example.com", i)
		admins[pageViewer{me: me}.hash()] = me
	}
	var admin string
	for i := range 1 << 24 {
		member.me = fmt.Sprintf("member-%d@example.com", i)
		if a, ok := admins[member.hash()]; ok {
			admin = a
			break
		}
	}
	if admin == "" {
		t.Fatal("no colliding admin and member found")
	}

	mux := http.NewServeMux()
	(&Handler{
		Hub: h, BasePath: "/v",
		Viewer:   func(r *http.Request) string { return r.Header.Get("X-Who") },
		ReadOnly: func(r *http.Request) bool { return r.Header.Get("X-Who") == member.me },
		Cut: func(r *http.Request) FarmCut {
			if r.Header.Get("X-Who") == member.me {
				return FarmCut{Scoped: true, Projects: []string{"claimlayer"}}
			}
			return FarmCut{}
		},
	}).Register(mux)
	serve := func(who string) (etag, body string) {
		t.Helper()
		req := httptest.NewRequest("GET", "/garden", nil)
		req.Header.Set("X-Who", who)
		req.Header.Set("Accept-Encoding", "gzip")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK || rec.Header().Get("Content-Encoding") != "gzip" {
			t.Fatalf("GET /garden as %s = %d, Content-Encoding %q", who, rec.Code, rec.Header().Get("Content-Encoding"))
		}
		zr, err := gzip.NewReader(bytes.NewReader(rec.Body.Bytes()))
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(zr)
		return rec.Header().Get("ETag"), string(b)
	}

	adminTag, adminPage := serve(admin) // the admin's page is cached first
	if !strings.Contains(adminPage, "i-gd-work") {
		t.Fatal("the admin page lacks gd-work: the fixture does not reach the lists")
	}
	memberTag, memberPage := serve(member.me)
	if memberTag != adminTag {
		t.Fatalf("member ETag %s, admin %s: the forced collision did not happen", memberTag, adminTag)
	}
	if strings.Contains(memberPage, "gd-work") || !strings.Contains(memberPage, "i-cl-work") || !strings.Contains(memberPage, `data-cut="1"`) {
		t.Error("a member whose ETag collides with an admin's was served the admin's cached page")
	}
}

// An admin's Become session as alice and alice's own session are the same viewer but for the impersonator (plan
// gd-0SUIdKXWV3 step 4): at one hub version they get different ETags and different cached gzip pages, so alice is never
// served "impersonated by <admin>", and the admin's session never loses the banner and its Stop link (merge desk on
// PR #1753).
func TestPageImpersonatedNotSharedWithTenant(t *testing.T) {
	const alice, stop = "oidc:alice@example.com", "https://gardenio.example/oauth2/sign_out?rd=x"
	own, become := pageViewer{me: alice}, pageViewer{me: alice, impersonator: "matt"}
	if own.hash() == become.hash() {
		t.Error("the tenant's own page and an admin's Become page hash alike")
	}
	if (pageKey{"e", own}) == (pageKey{"e", become}) {
		t.Error("the tenant's own page and an admin's Become page share a gzip cache key")
	}

	f := newFake()
	f.set([]*beadsapi.BeadDetail{bead("gd-work", "task", 5)}, nil)
	h := New(f, "/v", nil)
	if err := h.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	(&Handler{
		Hub: h, BasePath: "/v", StopURL: stop,
		Viewer:       func(*http.Request) string { return alice },
		Impersonator: func(r *http.Request) string { return r.Header.Get("X-Admin") },
	}).Register(mux)
	serve := func(admin string) (etag, body string) {
		t.Helper()
		req := httptest.NewRequest("GET", "/garden", nil)
		req.Header.Set("X-Admin", admin)
		req.Header.Set("Accept-Encoding", "gzip")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK || rec.Header().Get("Content-Encoding") != "gzip" {
			t.Fatalf("GET /garden (impersonator %q) = %d, Content-Encoding %q", admin, rec.Code, rec.Header().Get("Content-Encoding"))
		}
		zr, err := gzip.NewReader(bytes.NewReader(rec.Body.Bytes()))
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(zr)
		return rec.Header().Get("ETag"), string(b)
	}

	ownTag, ownPage := serve("") // the tenant's own page is cached first
	becomeTag, becomePage := serve("matt")
	againTag, againPage := serve("")
	if ownTag == becomeTag {
		t.Errorf("the tenant's own page and an admin's Become page share ETag %s", ownTag)
	}
	if !strings.Contains(becomePage, "impersonated by <b>matt</b>") || !strings.Contains(becomePage, `class="btn small imp-stop" href="`+stop+`"`) {
		t.Error("the admin's Become page lost the banner or Stop: it was served the tenant's cached page")
	}
	if againTag != ownTag || strings.Contains(againPage, "impersonated by") || strings.Contains(ownPage, "data-impersonator") {
		t.Error("the tenant's own page names an impersonator: it was served the admin's cached page")
	}
}
