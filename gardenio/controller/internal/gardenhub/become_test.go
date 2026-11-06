package gardenhub

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Become (plan gd-0SUIdKXWV3 step 3, gd-r5nRAIgagl): an admin's Become button beside a tenant (web/become.js).

// The body says data-admin, and data-become (the page the button opens), only where IsAdmin says so and there is a Become
// page; the two pages differ in their ETag, so an admin's page is never a 304 of a tenant's.
func TestPageAdminFlag(t *testing.T) {
	const page = "https://auth.example/gardenio/become/"
	etags := map[string]bool{}
	for _, c := range []struct {
		admin       bool
		become      string
		wantFlagged bool
	}{{false, page, false}, {true, page, true}, {true, "", false}} {
		mux := http.NewServeMux()
		(&Handler{Hub: New(newFake(), "/v", nil), BasePath: "/v", BecomeURL: c.become, IsAdmin: func(*http.Request) bool { return c.admin }}).Register(mux)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest("GET", "/garden", nil))
		body := rec.Body.String()
		if got := strings.Contains(body, ` data-admin="1" data-become="`+page+`"`); got != c.wantFlagged {
			t.Errorf("admin=%v become=%q: flagged = %v", c.admin, c.become, got)
		}
		if !c.wantFlagged && strings.Contains(body, "data-admin") {
			t.Errorf("admin=%v become=%q: the body says data-admin", c.admin, c.become)
		}
		etags[strings.Split(rec.Header().Get("ETag"), "-")[1]] = true // the viewer's part: each hub has its own snapshot version
	}
	if len(etags) != 2 {
		t.Errorf("an admin's page and a tenant's share an ETag's viewer part: %v", etags)
	}
	// become.js is page-land.js's, which web/lazy.js loads for an admin after page-workbench.js: project.js waits for it
	if !strings.Contains(pageAssets.byName["page-land.js"].src, pageBecomeJS) || !strings.Contains(pageProjectJS, `LAZY.need('land').then(owner`) {
		t.Error("become.js must be in page-land.js, and project.js must wait for it for the owner row's button")
	}
}

// become.js renders the button only on a page with data-admin (and a Become page), never for the viewer, and its click
// confirms before it goes to the Become page; project.js names a tenant land's owner. Run under node.
func TestBecomeUnderNode(t *testing.T) {
	runNodeLogic(t, "become.js", `
var assert = require("assert"), B = require(process.argv[1]);
var page = "https://auth.clonk.tv/gardenio/become/", admin = { admin: "1", become: page, me: "oidc:matt@example.com" };
function fakeDoc() {
  return { createElement: function (t) { var a = {}; return { tagName: t, attrs: a, setAttribute: function (k, v) { a[k] = String(v); } }; } };
}
assert.strictEqual(B.emailOf("oidc:Phono@Example.com"), "phono@example.com");
assert.strictEqual(B.emailOf("agent:gardenio-crew"), "");
assert.strictEqual(B.emailOf(""), "");
assert.ok(B.may(admin, "oidc:phono@example.com"), "an admin may become a tenant");
assert.ok(!B.may({ become: page, me: admin.me }, "oidc:phono@example.com"), "no data-admin, no button");
assert.ok(!B.may({ admin: "0", become: page }, "oidc:phono@example.com"), "data-admin must say 1");
assert.ok(!B.may({ admin: "1", me: admin.me }, "oidc:phono@example.com"), "no Become page, no button");
assert.ok(!B.may(admin, "oidc:MATT@example.com"), "never yourself");
assert.ok(!B.may(admin, "agent:gardenio-crew"), "never an agent");
var b = B.button(fakeDoc(), admin, "oidc:phono@example.com", "phonobaloon");
assert.ok(b, "the admin's page renders the button");
assert.strictEqual(b.tagName, "button");
assert.strictEqual(b.type, "button");
assert.strictEqual(b.textContent, "Become");
assert.strictEqual(b.attrs["data-become"], "oidc:phono@example.com");
assert.strictEqual(b.attrs["aria-label"], "Become phonobaloon");
assert.strictEqual(B.button(fakeDoc(), { become: page, me: admin.me }, "oidc:phono@example.com", "phonobaloon"), null, "a page without data-admin renders none");
assert.strictEqual(B.button(fakeDoc(), admin, admin.me, "matt"), null);
assert.strictEqual(B.href(page, "oidc:Phono+x@Example.com"), page + "?email=phono%2Bx%40example.com");
assert.strictEqual(B.href(page + "?a=1", "oidc:p@e.com"), page + "?a=1&email=p%40e.com");
assert.strictEqual(B.ask("phonobaloon"), "Become phonobaloon? Your browser signs in as them until you press Stop. Writes you make are marked as made by you on their behalf.");
var P = require(require("path").join(require("path").dirname(process.argv[1]), "project.js"));
assert.strictEqual(P.ownerWords({ principal: "oidc:phono@example.com", name: "phonobaloon" }), "Owner: phonobaloon");
assert.strictEqual(P.ownerWords({ principal: "oidc:phono@example.com" }), "Owner: phono@example.com");
assert.strictEqual(P.ownerWords(null), "");
`)
}

// The impersonation banner (plan gd-0SUIdKXWV3 step 4, gd-8HB0UjzwsL): only where Impersonator names an admin does the body
// say data-impersonator and the page draw "You are <tenant>, impersonated by <admin>" with Stop; each admin's page has its
// own ETag, so the banner never comes back as a 304 of another's page or of the tenant's own.
func TestPageImpersonationBanner(t *testing.T) {
	const stop = "https://gardenio.example/oauth2/sign_out?rd=x"
	etags := map[string]bool{}
	for _, c := range []struct {
		impersonator, stop   string
		wantBanner, wantStop bool
	}{{"", stop, false, false}, {"matt", stop, true, true}, {"ann", stop, true, true}, {"<b>x</b>", "", true, false}} {
		mux := http.NewServeMux()
		(&Handler{Hub: New(newFake(), "/v", nil), BasePath: "/v", StopURL: c.stop,
			Viewer:       func(*http.Request) string { return "oidc:phono@example.com" },
			Impersonator: func(*http.Request) string { return c.impersonator }}).Register(mux)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest("GET", "/garden", nil))
		body := rec.Body.String()
		if got := strings.Contains(body, `<div class="imp" id="imp"`); got != c.wantBanner {
			t.Errorf("impersonator %q: banner = %v, want %v", c.impersonator, got, c.wantBanner)
		}
		if got := strings.Contains(body, " data-impersonator="); got != c.wantBanner {
			t.Errorf("impersonator %q: data-impersonator = %v, want %v", c.impersonator, got, c.wantBanner)
		}
		if got := strings.Contains(body, `class="btn small imp-stop" href="`+stop+`"`); got != c.wantStop {
			t.Errorf("impersonator %q: Stop = %v, want %v", c.impersonator, got, c.wantStop)
		}
		if c.impersonator == "matt" && !strings.Contains(body, `You are <b>phono@example.com</b>, impersonated by <b>matt</b>`) {
			t.Error("the banner does not name the tenant and the admin")
		}
		if strings.Contains(body, "<b>x</b>") {
			t.Error("the admin's name is not escaped")
		}
		etags[strings.Split(rec.Header().Get("ETag"), "-")[1]] = true
	}
	if len(etags) != 4 {
		t.Errorf("impersonated pages share an ETag's viewer part: %v", etags)
	}
}
