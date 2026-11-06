package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// edge_reauth_gate_test.go — behavioral coverage for the edge-reauth reload
// gate in templates/layout.html (task ka-0000004168). The gate is browser JS, so
// like echarts_catalog_overlay_test.go we drive it under `node` with minimal DOM
// stubs. It skips (not fails) when node is unavailable so `go test` stays green
// on a toolchain without it; CI ships node so the gate runs there.
//
// What it locks in — the CORE of the fix: reloadForEdgeReauth() must fire ONLY
// for a genuine user action (a mutation, or a click/submit navigation) and must
// NOT fire for a passive background poll / prefetch / search-as-you-type, which
// is what was yanking readers off the page they were reading when the AVA edge
// session lapsed mid-read. It also pins that the passive SSE entry point
// (bvReauthOnEdgeExpiry) defers rather than reloads.

// runEdgeReauthJS extracts the edge-reauth <script> block from layout.html and
// evaluates it under node with stubs for the browser globals it touches, then
// runs `body` and returns combined stdout. The body can inspect two spies:
//
//	global.__reloads  — count of window.location.reload() calls the gate made
//	fire(event, detail) — dispatch an htmx-style event with the given detail
func runEdgeReauthJS(t *testing.T, body string) string {
	t.Helper()
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not on PATH; skipping edge-reauth gate behavioral test")
	}
	raw, err := os.ReadFile("templates/layout.html")
	if err != nil {
		t.Fatalf("read layout.html: %v", err)
	}
	const startMark = "var EDGE_REAUTH_COOLDOWN_KEY = 'bv-edge-reauth-reload-at';"
	const endMark = "// Skin switcher: wire the <select>"
	html := string(raw)
	si := strings.Index(html, startMark)
	ei := strings.Index(html, endMark)
	if si < 0 || ei < 0 || ei <= si {
		t.Fatalf("could not locate edge-reauth script block in layout.html (si=%d ei=%d)", si, ei)
	}
	snippet := html[si:ei]

	// DOM/browser stubs. document.addEventListener records handlers into a map
	// so the test body can `fire` them; window.location.reload increments a spy;
	// sessionStorage is an in-memory map so the cooldown works across calls.
	prog := `
const __handlers = {};
global.__reloads = 0;
const __store = {};
global.sessionStorage = {
  getItem(k) { return Object.prototype.hasOwnProperty.call(__store, k) ? __store[k] : null; },
  setItem(k, v) { __store[k] = String(v); },
  removeItem(k) { delete __store[k]; },
};
const __nodes = {};
function __el(tag) {
  return { tag, style: {}, children: [], attrs: {}, listeners: {}, parentNode: null,
    setAttribute(k, v) { this.attrs[k] = v; },
    addEventListener(t, fn) { this.listeners[t] = fn; },
    appendChild(c) { c.parentNode = this; this.children.push(c); if (c.id) __nodes[c.id] = c; return c; },
    removeChild(c) { this.children = this.children.filter(x => x !== c); delete __nodes[c.id]; c.parentNode = null; } };
}
global.document = {
  addEventListener(type, fn) { (__handlers[type] = __handlers[type] || []).push(fn); },
  activeElement: null,
  createElement: __el,
  getElementById(id) { return __nodes[id] || null; },
  body: __el('body'),
};
global.bannerShown = function() { return document.getElementById('bv-session-expired') !== null; };
global.window = { location: null };
global.window.location = { pathname: '/b/gd-x', search: '', reload() { global.__reloads++; } };
// Fire an htmx-style event to every registered handler for its type.
global.fire = function(type, detail) {
  const evt = { type, detail };
  (__handlers[type] || []).forEach(fn => fn(evt));
};
` + snippet + `
` + body
	cmd := exec.Command("node", "-e", prog)
	cmd.Dir = "."
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("node eval failed: %v\n%s", err, out)
	}
	return string(out)
}

// resetReloadsBetweenCases resets the reload spy and clears the cooldown so each
// scenario is independent (the 10s sessionStorage cooldown would otherwise
// swallow every reload after the first).
const resetReloadsBetweenCases = `
function reset() { global.__reloads = 0; global.sessionStorage.removeItem('bv-edge-reauth-reload-at'); document.activeElement = null; const b = document.getElementById('bv-session-expired'); if (b) document.body.removeChild(b); }
`

func TestEdgeReauth_GatesPassivePolls(t *testing.T) {
	out := runEdgeReauthJS(t, resetReloadsBetweenCases+`
function run(label, type, detail) {
  reset();
  fire(type, detail);
  console.log(label + '=' + global.__reloads);
}

// --- Passive GET background poll (hx-trigger="every Ns"): NO triggeringEvent.
// This is the exact case that was dumping readers on the landing page. Both the
// sendError (AVA network 303) and beforeSwap (401/status-0) shapes must be quiet.
run('POLL_SENDERR', 'htmx:sendError', { requestConfig: { verb: 'get', triggeringEvent: undefined } });
run('POLL_BEFORESWAP', 'htmx:beforeSwap', { xhr: { status: 0 }, requestConfig: { verb: 'get', triggeringEvent: undefined } });

// --- Prefetch (hx-trigger="mouseover once") — user hovered, did not navigate.
run('PREFETCH', 'htmx:sendError', { requestConfig: { verb: 'get', triggeringEvent: { type: 'mouseover' } } });

// --- Search-as-you-type (keyup) — a read, not a navigation.
run('SEARCH_KEYUP', 'htmx:sendError', { requestConfig: { verb: 'get', triggeringEvent: { type: 'keyup' } } });

// --- A write (gd-C2Sl6ClxAY) must NOT silently reload: that drops the POST with
// no word. It shows the session-expired notice instead (see the banner tests).
run('WRITE_POST', 'htmx:sendError', { requestConfig: { verb: 'post', triggeringEvent: { type: 'submit' } } });
run('WRITE_DELETE_BEFORESWAP', 'htmx:beforeSwap', { xhr: { status: 401 }, requestConfig: { verb: 'delete', triggeringEvent: { type: 'click' } } });

// --- Real navigation click (boosted GET) — user clicked a link, MUST reload.
run('CLICK_NAV', 'htmx:sendError', { requestConfig: { verb: 'get', triggeringEvent: { type: 'click' } } });

// --- Unknown provenance (no requestConfig) — err toward reload so the write
// self-heal can never silently regress on an older htmx / synthetic event.
run('NO_REQUESTCONFIG', 'htmx:sendError', {});
`)

	want := map[string]string{
		"POLL_SENDERR=0":            "passive background poll (sendError) must NOT reload",
		"POLL_BEFORESWAP=0":         "passive background poll (beforeSwap) must NOT reload",
		"PREFETCH=0":                "mouseover prefetch must NOT reload",
		"SEARCH_KEYUP=0":            "search-as-you-type keyup must NOT reload",
		"WRITE_POST=0":              "a POST write must NOT silently reload (gd-C2Sl6ClxAY): the notice replaces the reload",
		"WRITE_DELETE_BEFORESWAP=0": "a DELETE write (beforeSwap 401) must NOT silently reload",
		"CLICK_NAV=1":               "real click navigation must reload",
		"NO_REQUESTCONFIG=1":        "unknown provenance must err toward reload",
	}
	for token, why := range want {
		if !strings.Contains(out, token) {
			t.Errorf("expected %q (%s)\noutput:\n%s", token, why, out)
		}
	}
}

// TestEdgeReauth_BeforeSwapAlwaysSuppressesSwap pins that an auth-failure
// beforeSwap never swaps the OAuth redirect / error page inline, even for a
// passive poll that (correctly) does not reload — otherwise a poller would paint
// a login page into a fragment.
func TestEdgeReauth_BeforeSwapAlwaysSuppressesSwap(t *testing.T) {
	out := runEdgeReauthJS(t, resetReloadsBetweenCases+`
reset();
const passive = { xhr: { status: 401 }, shouldSwap: true, requestConfig: { verb: 'get', triggeringEvent: undefined } };
fire('htmx:beforeSwap', passive);
console.log('PASSIVE_SHOULDSWAP=' + passive.shouldSwap);
console.log('PASSIVE_RELOADS=' + global.__reloads);

reset();
const action = { xhr: { status: 401 }, shouldSwap: true, requestConfig: { verb: 'post', triggeringEvent: { type: 'submit' } } };
fire('htmx:beforeSwap', action);
console.log('ACTION_SHOULDSWAP=' + action.shouldSwap);
console.log('ACTION_RELOADS=' + global.__reloads);

reset();
const nav = { xhr: { status: 401 }, shouldSwap: true, requestConfig: { verb: 'get', triggeringEvent: { type: 'click' } } };
fire('htmx:beforeSwap', nav);
console.log('NAV_SHOULDSWAP=' + nav.shouldSwap);
console.log('NAV_RELOADS=' + global.__reloads);
`)
	for _, w := range []string{
		"PASSIVE_SHOULDSWAP=false", // never paint the redirect inline
		"PASSIVE_RELOADS=0",        // but stay put — the reader is not interrupted
		"ACTION_SHOULDSWAP=false",
		"ACTION_RELOADS=0", // a write shows the notice instead of dropping silently
		"NAV_SHOULDSWAP=false",
		"NAV_RELOADS=1", // a click navigation still re-auths by reload
	} {
		if !strings.Contains(out, w) {
			t.Errorf("missing %q in output:\n%s", w, out)
		}
	}
}

// TestEdgeReauth_PassiveSSEExpiryDefers pins that the shared SSE entry point
// (bvReauthOnEdgeExpiry) — invoked when a long-lived stream detects an expired
// edge session — DEFERS rather than force-reloads, since a stream expiring is a
// passive event while the user is just watching (ka-0000004168). The mid-edit
// guard is also exercised.
func TestEdgeReauth_PassiveSSEExpiryDefers(t *testing.T) {
	out := runEdgeReauthJS(t, resetReloadsBetweenCases+`
reset();
console.log('SSE_RESULT=' + window.bvReauthOnEdgeExpiry());
console.log('SSE_RELOADS=' + global.__reloads);

// Mid-edit: still returns false and does not reload.
reset();
document.activeElement = { tagName: 'TEXTAREA', isContentEditable: false };
console.log('SSE_MIDEDIT_RESULT=' + window.bvReauthOnEdgeExpiry());
console.log('SSE_MIDEDIT_RELOADS=' + global.__reloads);
`)
	for _, w := range []string{
		"SSE_RESULT=false",         // no reload performed => hint/banner remains
		"SSE_RELOADS=0",            // passive stream expiry never yanks the reader
		"SSE_MIDEDIT_RESULT=false", // mid-edit guard also declines
		"SSE_MIDEDIT_RELOADS=0",
	} {
		if !strings.Contains(out, w) {
			t.Errorf("missing %q in output:\n%s", w, out)
		}
	}
}

// TestEdgeReauth_CooldownPreventsReloadLoop pins the cooldown: two back-to-back
// user actions within the window reload only once, so a failing re-auth can't
// spin the tab.
func TestEdgeReauth_CooldownPreventsReloadLoop(t *testing.T) {
	out := runEdgeReauthJS(t, `
global.__reloads = 0;
global.sessionStorage.removeItem('bv-edge-reauth-reload-at');
const action = { requestConfig: { verb: 'get', triggeringEvent: { type: 'click' } } };
fire('htmx:sendError', action);
fire('htmx:sendError', action);
console.log('COOLDOWN_RELOADS=' + global.__reloads);
`)
	if !strings.Contains(out, "COOLDOWN_RELOADS=1") {
		t.Errorf("expected the cooldown to collapse two rapid reloads into one\noutput:\n%s", out)
	}
}

// TestEdgeReauth_WriteShowsSessionExpiredNotice pins gd-C2Sl6ClxAY: a write that
// hits an expired sign-in (a 401, or status 0 when the edge answered with a
// cross-origin OAuth redirect the XHR cannot read) must tell the user it was NOT
// saved and offer a sign-in that keeps this page (and its draft) alive — never
// reload and drop the POST silently. A passive poll stays quiet, and the notice
// clears once any request succeeds again (the user signed in in the other tab).
func TestEdgeReauth_WriteShowsSessionExpiredNotice(t *testing.T) {
	out := runEdgeReauthJS(t, resetReloadsBetweenCases+`
const post = { requestConfig: { verb: 'post', triggeringEvent: { type: 'submit' } } };

reset();
fire('htmx:beforeSwap', Object.assign({ xhr: { status: 401 } }, post));
console.log('W401_BANNER=' + bannerShown());
console.log('W401_RELOADS=' + global.__reloads);

reset();
fire('htmx:sendError', post);
console.log('W0_BANNER=' + bannerShown());
console.log('W0_RELOADS=' + global.__reloads);

// Content: says the change was not saved, the draft is kept, and the link goes
// to the sign-in start in a NEW tab, returning to this page.
const bar = document.getElementById('bv-session-expired');
const text = bar.children.map(c => c.textContent || '').join('|');
console.log('TEXT_NOT_SAVED=' + /not saved/.test(text));
console.log('TEXT_DRAFT_KEPT=' + /draft is kept/.test(text));
const link = bar.children.find(c => c.tag === 'a');
console.log('LINK_HREF=' + link.href);
console.log('LINK_TARGET=' + link.target);

// A second failure does not stack another banner.
fire('htmx:sendError', post);
console.log('BANNER_COUNT=' + document.body.children.length);

// Passive poll: no banner.
reset();
fire('htmx:sendError', { requestConfig: { verb: 'get', triggeringEvent: undefined } });
console.log('POLL_BANNER=' + bannerShown());

// Click navigation reloads, no banner.
reset();
fire('htmx:sendError', { requestConfig: { verb: 'get', triggeringEvent: { type: 'click' } } });
console.log('NAV_BANNER=' + bannerShown());
console.log('NAV_RELOADS=' + global.__reloads);

// The notice clears itself after a successful request.
reset();
fire('htmx:sendError', post);
console.log('BEFORE_OK=' + bannerShown());
fire('htmx:afterRequest', { xhr: { status: 200 } });
console.log('AFTER_OK=' + bannerShown());
`)
	for _, w := range []string{
		"W401_BANNER=true", "W401_RELOADS=0",
		"W0_BANNER=true", "W0_RELOADS=0",
		"TEXT_NOT_SAVED=true", "TEXT_DRAFT_KEPT=true",
		"LINK_HREF=/oauth2/start?rd=%2Fb%2Fgd-x", "LINK_TARGET=_blank",
		"BANNER_COUNT=1",
		"POLL_BANNER=false",
		"NAV_BANNER=false", "NAV_RELOADS=1",
		"BEFORE_OK=true", "AFTER_OK=false",
	} {
		if !strings.Contains(out, w) {
			t.Errorf("missing %q in output:\n%s", w, out)
		}
	}
}
