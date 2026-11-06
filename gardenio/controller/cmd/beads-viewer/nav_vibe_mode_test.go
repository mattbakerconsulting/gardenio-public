package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// vibeUserResp returns a user-bead list result seeding a single operator
// (authorEmail) whose stored preferences enable VibeMode. Assign it to a
// fake's userResp field so the render seam's Types=[user] Kinds=[config] scan
// resolves the viewer as a vibe user — flipping the nav to the full experience
// — WITHOUT perturbing the fake's listResp / page-specific query answers. Pair
// with an X-Auth-Request-Email: authorEmail header so viewerVibeMode has an
// email to resolve. Used by the vibe-only page nav-link regressions below,
// which after gb-ux #3 (ka-0000003340) only carry their link in vibe mode.
func vibeUserResp() *beadsapi.ListBeadsResult {
	prefs, _ := json.Marshal(Preferences{VibeMode: true})
	return &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{{
			ID:     "gd-user-op",
			Type:   "user",
			Kind:   "config",
			Status: "open",
			Fields: map[string]string{
				"email":       authorEmail,
				"preferences": string(prefs),
			},
		}},
	}
}

// The nav bar is gated on VibeMode (gb-ux #3, ka-0000003340). Simple mode (the
// default) shows a minimal CORE set; the full ("vibe") experience shows every
// link. These lists are the operator-signed-off split (decision ka-0000007562)
// and are asserted verbatim against the rendered layout so a future nav edit
// that moves a link across the gate — in either direction — trips a test.

// navCoreLinkHrefs are the links visible in BOTH modes. Feed is home in
// simple mode (there is no wordmark/logo affordance — operator-intentional).
// Active joined the core set once it became a real work surface — a "Needs
// attention" pull plus per-project recency list (ka-0000002647); operator
// sign-off on decision ka-0000007562 ("I would like for Active to be in there
// but it needs a lot of work to be worthy").
var navCoreLinkHrefs = []string{
	`href="/active"`,
	`href="/feed"`,
	`href="/agents"`,
	`href="/threads"`,
	`href="/decisions"`,
	`href="/escalations"`,
	`href="/settings"`,
}

// navAlwaysOnControls are the three action controls that are core in every
// mode alongside the six core links. They are not {{if .VibeMode}}-gated.
var navAlwaysOnControls = []string{
	`class="nav-quick-spawn"`,  // + New Agent
	`<form class="nav-search"`, // Search form
	`class="nav-logout"`,       // log out
}

// navModeAgnosticChrome stays visible in both modes (ka-0000003340 notes) —
// it is not part of the VibeMode gate at all.
var navModeAgnosticChrome = []string{
	`class="auto-refresh-toggle"`,
	`class="skin-switcher"`,
	`class="kbd-help-button"`,
}

// navVibeOnlyLinkHrefs are the links that appear ONLY in vibe mode. Kept as
// href fragments so the assertion is anchored to the nav anchor, not a
// same-named heading elsewhere on the page. `/active?operated_by=me` (Me) and
// `/inbox` (Home) additionally sit inside the pre-existing {{if .ViewerEmail}}
// gate; the vibe arm below renders an authenticated viewer so they surface.
// NB: /plans and /formulas are CORE (always-on) links — not vibe-gated — so
// they belong in navCoreLinkHrefs' spirit, NOT here (layout.html makes both
// first-class in the default UI; bug ka-0000005902 / epic ka-0000007376).
// Active itself is now CORE (ka-0000002647) and lives in navCoreLinkHrefs; only
// the Me/Home derivatives of /active remain vibe-only.
var navVibeOnlyLinkHrefs = []string{
	`href="/"`,                      // beads
	`href="/active?operated_by=me"`, // Me
	`href="/inbox"`,                 // Home
	`href="/roles"`,
	`href="/telepathy"`,
	`href="/people"`, // Users
	`href="/extern"`,
	`href="/ratings"`,
	`href="/advice"`,
	`href="/triggers"`,
	`href="/gardeners"`,
	`href="/maps"`,
	`href="/costs"`,
	`href="/charts"`,
	`href="/health"`,
	`href="/trust"`,
	`href="/views"`, // My Views
}

// renderNav renders /active (a default-on surface that populates ViewerEmail,
// so the Me/Home links' {{if .ViewerEmail}} inner gate is satisfied) for an
// authenticated viewer whose stored VibeMode preference is `vibe`. Returns the
// full page HTML — the <nav> block is what the callers assert against.
func renderNav(t *testing.T, vibe bool) string {
	t.Helper()
	f := settingsFake(Preferences{VibeMode: flexBool(vibe)})
	mux := mkServer(t, f)
	req := httptest.NewRequest(http.MethodGet, "/active", nil)
	req.Header.Set("X-Auth-Request-Email", authorEmail)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /active (vibe=%v) = %d, want 200\nbody=%s", vibe, w.Code, w.Body.String())
	}
	return w.Body.String()
}

// TestNav_SimpleMode_ShowsOnlyCore asserts the default (VibeMode=false) nav
// shows exactly the CORE six links plus the always-on controls and the
// mode-agnostic chrome — and NONE of the nineteen vibe-only links, even though
// the viewer is authenticated (so their absence is the gate's doing, not the
// ViewerEmail gate's).
func TestNav_SimpleMode_ShowsOnlyCore(t *testing.T) {
	body := renderNav(t, false)
	nav := extractNav(t, body)

	for _, want := range navCoreLinkHrefs {
		if !strings.Contains(nav, want) {
			t.Errorf("simple-mode nav missing core link %q\nnav=%s", want, nav)
		}
	}
	for _, want := range navAlwaysOnControls {
		if !strings.Contains(nav, want) {
			t.Errorf("simple-mode nav missing always-on control %q\nnav=%s", want, nav)
		}
	}
	for _, want := range navModeAgnosticChrome {
		if !strings.Contains(nav, want) {
			t.Errorf("simple-mode nav missing mode-agnostic chrome %q\nnav=%s", want, nav)
		}
	}
	for _, absent := range navVibeOnlyLinkHrefs {
		if navContainsLink(nav, absent) {
			t.Errorf("simple-mode nav should NOT contain vibe-only link %q\nnav=%s", absent, nav)
		}
	}
}

// TestNav_VibeMode_ShowsEverything asserts that with VibeMode=true the nav
// carries the full set: the core six, the always-on controls, the mode-agnostic
// chrome, AND all nineteen vibe-only links.
func TestNav_VibeMode_ShowsEverything(t *testing.T) {
	body := renderNav(t, true)
	nav := extractNav(t, body)

	for _, want := range navCoreLinkHrefs {
		if !strings.Contains(nav, want) {
			t.Errorf("vibe-mode nav missing core link %q\nnav=%s", want, nav)
		}
	}
	for _, want := range navAlwaysOnControls {
		if !strings.Contains(nav, want) {
			t.Errorf("vibe-mode nav missing always-on control %q\nnav=%s", want, nav)
		}
	}
	for _, want := range navVibeOnlyLinkHrefs {
		if !navContainsLink(nav, want) {
			t.Errorf("vibe-mode nav missing vibe-only link %q\nnav=%s", want, nav)
		}
	}
}

// TestNav_LoggedOut_SimpleByDefault asserts an anonymous viewer (no user bead,
// VibeMode resolves to the false default) gets the simple nav — and that the
// two ViewerEmail-gated links stay hidden regardless.
func TestNav_LoggedOut_SimpleByDefault(t *testing.T) {
	f := settingsFake(Preferences{VibeMode: true}) // even a vibe user seeded…
	mux := mkServer(t, f)
	req := httptest.NewRequest(http.MethodGet, "/active", nil) // …but NO auth header
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /active (anon) = %d, want 200", w.Code)
	}
	nav := extractNav(t, w.Body.String())

	// Core links still render for anonymous viewers.
	for _, want := range navCoreLinkHrefs {
		if !strings.Contains(nav, want) {
			t.Errorf("anon nav missing core link %q\nnav=%s", want, nav)
		}
	}
	// Vibe-only links stay hidden — the anonymous viewer resolves to the
	// simple default (viewerVibeMode short-circuits to false with no email).
	for _, absent := range navVibeOnlyLinkHrefs {
		if navContainsLink(nav, absent) {
			t.Errorf("anon nav should NOT contain vibe-only link %q\nnav=%s", absent, nav)
		}
	}
}

// TestNav_MobileDensify_KeepsT17Links guards the reconciliation in
// ka-0000002644. The generalized phone-width nav densification (Step 1,
// ka-0000000866) shrinks the nav gap/font/padding at ≤640px and drops the
// lowest-traffic entries with `display: none`. As originally ported from the
// inbox treatment it also hid Settings, Advice, Triggers, Costs and My Views —
// which directly contradicted the older, operator-signed T17 responsive e2e
// contract (tests/beads-viewer-e2e/tests/responsive.spec.js, phase
// ka-0000001308), asserting those links stay VISIBLE at 375px. That e2e suite
// only runs on merge-request / scheduled / release pipelines, NOT on main-push,
// so the regression landed invisibly and then red-lit CI/Test on every
// subsequent MR.
//
// This unit test runs under `make test` (which DOES gate main-push), so it
// catches the same class of regression at the layer where it slipped through:
// it parses the ≤640px densify @media block out of the served CSS and asserts
// the T17-protected links are NOT among the hidden selectors, while the
// genuinely-secondary observability dashboards (Health, Ratings) plus
// the two non-navigation chrome buttons remain hidden so the densify keeps its
// space win.
func TestNav_MobileDensify_KeepsT17Links(t *testing.T) {
	body := renderNav(t, true) // vibe mode so every densified link is in the DOM
	block := extractDensifyMediaBlock(t, body)

	// The five navigable links T17 asserts remain reachable at 375px — they
	// must WRAP (the baseline skin nav is display:flex; flex-wrap:wrap), never
	// be display:none'd. If any of these reappears in the densify hide-set,
	// T17 breaks fleet-wide again.
	protected := []string{
		`nav a[href$="/settings"]`, // Settings — simple + vibe mode
		`nav a[href$="/advice"]`,   // Advice — vibe mode
		`nav a[href$="/triggers"]`, // Triggers — vibe mode
		`nav a[href$="/costs"]`,    // Costs — vibe mode
		`nav a[href$="/views"]`,    // My Views — vibe mode
	}
	for _, sel := range protected {
		if strings.Contains(block, sel) {
			t.Errorf("densify ≤640px block must NOT hide T17-protected link selector %q — it breaks responsive.spec.js T17 fleet-wide\nblock=%s", sel, block)
		}
	}

	// The densify must still earn its keep: the secondary observability links
	// and the two chrome buttons stay hidden at phone widths.
	stillHidden := []string{
		`nav a[href$="/health"]`,
		`nav a[href$="/ratings"]`,
		`nav .auto-refresh-toggle`,
		`nav .kbd-help-button`,
	}
	for _, sel := range stillHidden {
		if !strings.Contains(block, sel) {
			t.Errorf("densify ≤640px block should still hide secondary selector %q (space win)\nblock=%s", sel, block)
		}
	}
}

// extractDensifyMediaBlock returns the body of the generalized nav
// densification `@media (max-width: 640px)` rule — the one keyed on
// `html[data-skin] nav` (layout.html, ka-0000000866). It anchors on that
// selector (unique to the densify block) so it is not confused with the several
// other `@media (max-width: 640px)` rules skins carry, then returns the text up
// to the closing brace of the media query.
func extractDensifyMediaBlock(t *testing.T, body string) string {
	t.Helper()
	// The densify rule opens with the nav shrink line; find it, then walk back
	// to the enclosing @media and forward to its matching close brace.
	anchor := "html[data-skin] nav { gap:"
	ai := strings.Index(body, anchor)
	if ai < 0 {
		t.Fatalf("densify nav-shrink rule (%q) not found in served CSS", anchor)
	}
	start := strings.LastIndex(body[:ai], "@media")
	if start < 0 {
		t.Fatalf("no @media rule preceding the densify nav-shrink line")
	}
	// The media query ends at the first `}` at brace-depth 0 after start.
	depth := 0
	for i := start; i < len(body); i++ {
		switch body[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return body[start : i+1]
			}
		}
	}
	t.Fatalf("unterminated densify @media block")
	return ""
}

// extractNav returns the substring between the opening site <nav aria-label="Site"> and closing </nav>
// tags so link assertions are scoped to the nav bar and never match a same-URL
// anchor rendered in page content.
func extractNav(t *testing.T, body string) string {
	t.Helper()
	start := strings.Index(body, `<nav aria-label="Site">`)
	if start < 0 {
		t.Fatalf("no <nav> element in rendered page\nbody=%s", body)
	}
	end := strings.Index(body[start:], "</nav>")
	if end < 0 {
		t.Fatalf("unterminated <nav> element in rendered page")
	}
	return body[start : start+end]
}

// navContainsLink reports whether nav carries an anchor with exactly this href,
// matching on `href="…"` immediately followed by `>` or a space (so `href="/"`
// does not spuriously match `href="/agents"`, and `href="/active"` does not
// match `href="/active?operated_by=me"`).
func navContainsLink(nav, hrefFrag string) bool {
	// hrefFrag is like `href="/agents"`. A real anchor writes it followed by
	// `>` (no attrs) or ` ` (title=…), so anchor the match on that boundary.
	return strings.Contains(nav, hrefFrag+">") || strings.Contains(nav, hrefFrag+" ")
}
