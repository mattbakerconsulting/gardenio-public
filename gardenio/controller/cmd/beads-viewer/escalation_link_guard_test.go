package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"gardenio/controller/internal/bridge"
)

// TestEscalationLinksResolveAgainstRealMux is the regression guard for the bug
// class in epic ka-0000000887: the Slack bridge shipped a beads-viewer TEMPLATE
// FILENAME ("/ui/escalations.html#<id>") as a URL PATH, and every emitted
// escalation link 404'd for months because NO test ever resolved an emitted URL
// against the viewer's real route table (task ka-0000001512).
//
// This test closes that loop MECHANICALLY. It:
//
//  1. Builds the escalation review URL through the bridge's REAL shared emitter
//     (bridge.EscalationReviewURL — the exact function all 5 bridge call sites
//     route through, ka-0000005895), for a spread of realistic bead IDs and for
//     both prod-shaped (base-path "/beads") and bare (e2e) web URLs.
//  2. Resolves the emitted path against the beads-viewer's ACTUAL
//     *http.ServeMux — the same mux RegisterRoutes builds in production, via the
//     shared mkRawMux helper — using mux.Handler(req), and asserts a real
//     registered pattern matched rather than net/http's built-in 404 handler
//     (which returns an EMPTY pattern).
//
// The load-bearing property (the operator was explicit about this): it consumes
// the REAL route table. A hand-maintained list of "expected routes" would have
// happily agreed that "/ui/escalations.html" was fine — that is exactly the rot
// that caused the bug. The negative control below proves the guard is not
// vacuous: the OLD dead shape must FAIL to resolve.
func TestEscalationLinksResolveAgainstRealMux(t *testing.T) {
	mux := mkRawMux(t, newFake())

	// Prod-shaped and e2e-shaped bases. Prod mounts the viewer under "/beads"
	// (Traefik strips the prefix before the appMux sees the request), so the
	// guard strips the base's own path before resolving — faithful to how a
	// real request reaches RegisterRoutes' bare patterns.
	bases := []string{
		"https://prod.gardenio.acme.example/beads", // prod (BEADS_WEB_URL, verified live)
		"https://gardenio.app.e2e.dev.acme.example/beads",
		"https://beads.example", // bare, no base path
	}
	// The emitters key every escalation link off the bead ID; exercise a spread
	// including the real prod-shaped gd- ids and edge-y single-token ids.
	beadIDs := []string{"gd-B07MJ9scWG", "gd-aaa", "ka-0000000887", "escalation-1"}

	for _, base := range bases {
		for _, id := range beadIDs {
			emitted := bridge.EscalationReviewURL(base, id)
			if emitted == "" {
				t.Fatalf("EscalationReviewURL(%q, %q) returned empty", base, id)
			}
			reqPath := muxPathFor(t, base, emitted)
			req := httptest.NewRequest("GET", reqPath, nil)
			h, pattern := mux.Handler(req)
			if h == nil || pattern == "" {
				t.Errorf("emitted escalation link %q (path %q) does NOT resolve to any registered beads-viewer route — this is the /ui/escalations.html-shipped-as-a-path bug class (epic ka-0000000887)", emitted, reqPath)
				continue
			}
			// It must land on the bead-show catch-all, which renders the
			// escalation card WITH the inline approve/reject control — the whole
			// point of the link. Anything else means the target moved.
			if pattern != "GET /{id}" {
				t.Errorf("emitted escalation link %q (path %q) routed to %q, want \"GET /{id}\" (handleBeadShow — the page carrying the approve/reject chips)", emitted, reqPath, pattern)
			}
		}
	}
}

// TestEscalationLinkGuardIsNotVacuous is the negative control. It proves the
// positive guard above actually DISCRIMINATES: a template filename shipped as a
// URL PATH — the bug class this whole epic fixed — must NOT reach a working
// beads-viewer page.
//
// RE-EXPRESSED for gd-bHXIn3K77W. The original control asserted the exact
// literal shape `/ui/escalations.html` resolves to NO route. That premise was
// invalidated — deliberately and correctly — by the compat redirect
// ka-0000005003 (route `GET /ui/{page}`, compat_redirect.go), which HEALS the
// backlog of already-sent `/ui/<top-level-page>.html` operator links with a 301
// to their canonical route. `/ui/escalations.html` now 301-redirects to
// `/escalations` instead of 404ing. That is the intended fix, not a regression:
// it rescues links already sitting in Slack/DM history, via a CLOSED allowlist
// of top-level pages (never a wildcard, never the parameterized detail pages).
// Subtest "epic shape is now intentionally healed" locks that behavior in.
//
// The bug class is still real, though, so the negative control must be
// re-expressed against a shape that is STILL genuinely dead. The escalation
// deep link's real target is the per-bead DETAIL page (bead_show.html, reached
// only via `GET /{id}` with a real id). Its template filename — like every
// parameterized detail template — is DELIBERATELY omitted from the compat
// allowlist (compat_redirect.go header), because it has no bare top-level route
// to heal to. So shipping THAT template filename as a path — the exact bug class,
// applied to the page the escalation link actually needs to reach — is still a
// hard 404. The compat heal is a bounded allowlist; it does not paper over an
// arbitrary `/ui/*.html`. If it ever did (allowlist widened to a wildcard, or a
// greedy `/ui/` heal), this control fails — the constructive proof below shows
// exactly that.
func TestEscalationLinkGuardIsNotVacuous(t *testing.T) {
	base := "https://prod.gardenio.acme.example/beads"

	// The epic's exact shape is now intentionally HEALED (ka-0000005003), not
	// dead — assert that, so a future removal/breakage of the compat redirect
	// (which would re-orphan the already-sent backlog) is caught here too.
	t.Run("epic shape is now intentionally healed", func(t *testing.T) {
		mux := mkRawMux(t, newFake())
		healedURL := base + "/ui/escalations.html#gd-B07MJ9scWG"
		reqPath := muxPathFor(t, base, healedURL)
		_, code := getBody(t, mux, reqPath)
		if code != http.StatusMovedPermanently {
			t.Fatalf("legacy %q (path %q) should 301-heal to /escalations via the compat redirect (ka-0000005003); got status %d", healedURL, reqPath, code)
		}
	})

	// The re-expressed negative control: the escalation DETAIL page's template
	// filename shipped as a path is STILL genuinely dead (hard 404). This is the
	// bug class — a template filename as a URL path — for the exact page the
	// escalation link must land on. The positive guard's teeth depend on this
	// staying dead: if it resolved to a working page, an emitter that shipped a
	// template filename could pass unnoticed.
	deadShape := func(t *testing.T, mux muxRouter) (int, string) {
		deadURL := base + "/ui/bead_show.html#gd-B07MJ9scWG"
		reqPath := muxPathFor(t, base, deadURL)
		req := httptest.NewRequest("GET", reqPath, nil)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		return w.Code, reqPath
	}

	t.Run("detail-template-as-path is still dead", func(t *testing.T) {
		mux := mkRawMux(t, newFake())
		code, reqPath := deadShape(t, mux)
		if code != http.StatusNotFound {
			t.Fatalf("the DEAD detail-template shape (path %q) returned status %d, want 404 — the compat redirect (ka-0000005003) is a CLOSED allowlist of top-level pages, so a parameterized detail template shipped as a path must NOT resolve to a working page. If it now does, the positive guard TestEscalationLinksResolveAgainstRealMux has lost its teeth.", reqPath, code)
		}
	})

	// Constructive non-vacuity proof (gd-bHXIn3K77W): reintroduce the bug class
	// and confirm the check above FAILS. A greedy `/ui/` heal — the failure mode
	// the original control named ("an accidental /ui/ route"), now generalized to
	// a wildcard rather than the bounded allowlist prod actually ships — makes the
	// dead detail-template shape resolve to a 200 page. The "still dead" check
	// must NOT see a 404 against such a mux; if it did, the control would be
	// vacuous.
	t.Run("check fails when the bug class is reintroduced", func(t *testing.T) {
		greedy := http.NewServeMux()
		greedy.HandleFunc("GET /ui/{page}", func(w http.ResponseWriter, r *http.Request) {
			// The regression: ANY /ui/*.html silently "works" (200), instead of
			// the closed-allowlist 301/404 the real handler enforces.
			w.WriteHeader(http.StatusOK)
		})
		code, _ := deadShape(t, greedy)
		if code == http.StatusNotFound {
			t.Fatalf("non-vacuity proof failed: a greedy /ui/ heal should make the dead detail-template shape resolve (got 200), but the control still saw a 404 — the control would not catch a widened allowlist")
		}
	})
}

// muxRouter is the minimal surface the negative control needs from both the real
// beads-viewer mux and the throwaway mux built for the non-vacuity proof.
type muxRouter interface {
	ServeHTTP(http.ResponseWriter, *http.Request)
}

// muxPathFor turns an emitted absolute URL into the path a real request presents
// to the beads-viewer's appMux: the URL path with the base's mount prefix
// stripped (Traefik strips it in prod) and any fragment/query dropped.
func muxPathFor(t *testing.T, base, emitted string) string {
	t.Helper()
	bu, err := url.Parse(base)
	if err != nil {
		t.Fatalf("bad base %q: %v", base, err)
	}
	eu, err := url.Parse(emitted)
	if err != nil {
		t.Fatalf("bad emitted URL %q: %v", emitted, err)
	}
	p := strings.TrimPrefix(eu.Path, strings.TrimRight(bu.Path, "/"))
	if p == "" || p[0] != '/' {
		p = "/" + p
	}
	return p
}
