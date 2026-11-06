package gardenhub

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The credential pool panel (web/creds.js, Lands workbench-glass spec GL-15, step 6.9, gd-zM1oXXxlYp).

// creds.js's pure half runs under node: an account's health in words and its chip's tone, a window as a percent and its
// meter's tone (CredentialPanel.tsx's 70% and 90%), the strategy in words, the line under the title (no session count
// when auto-assign is off), an account's line at a moment (its reset only while it is held back and still ahead, its
// key's expiry), and the panel's own failure words.
func TestCredsLogicUnderNode(t *testing.T) {
	runNodeLogic(t, "creds.js", `
var C = require(process.argv[1]), assert = require("assert");
assert.strictEqual(C.URL, "/garden/glass/credentials");
assert.deepStrictEqual(["healthy", "warming", "throttled", "missing", "odd", ""].map(C.statusWord), ["Healthy", "Warming", "Throttled", "No key", "Odd", "Unknown"]);
assert.deepStrictEqual(["healthy", "warming", "throttled", "missing", "odd"].map(C.statusTone), ["ok", "warn", "bad", "warn", ""]);
assert.deepStrictEqual([0.42, 0.005, 1, null, undefined, NaN].map(C.pct), ["42%", "1%", "100%", "", "", ""]);
assert.deepStrictEqual([0.69, 0.7, 0.89, 0.9, 1.2, null].map(C.meterTone), ["ok", "warn", "warn", "bad", "bad", ""]);
assert.deepStrictEqual([20, 2400, 3 * 3600, 3 * 86400].map(C.span), ["1 min", "40 min", "3 h", "3 days"]);
assert.strictEqual(C.strategyWord("least_loaded"), "New sessions go to the least loaded healthy account.");
assert.strictEqual(C.strategyWord("round_robin", 1), "New sessions take the healthy accounts in turn; the next is healthy account 2.");
assert.strictEqual(C.strategyWord("round_robin", null), "New sessions take the healthy accounts in turn.");
assert.strictEqual(C.strategyWord(""), "");
assert.strictEqual(C.summary({ total: 4, healthy: 1, sessions: 6 }), "4 accounts, 1 healthy · 6 sessions");
assert.strictEqual(C.summary({ total: 1, healthy: 1, sessions: 1 }), "1 account, 1 healthy · 1 session");
assert.strictEqual(C.summary({ total: 2, healthy: 2, sessions: 0, uncounted: true }), "2 accounts, 2 healthy");
var now = Date.parse("2026-10-26T04:20:00Z") / 1000, counted = {}, off = { uncounted: true };
assert.deepStrictEqual(C.accountLine({ provider: "claude", sessions: 4, assigned: 61, has_key: true, status: "healthy", throttle: "allowed", reset_at: "2026-10-26T05:00:00Z" }, counted, now),
  ["claude", "4 sessions now, 61 in all"], "a healthy account's reset is not news");
assert.deepStrictEqual(C.accountLine({ provider: "claude", sessions: 0, assigned: 52, has_key: true, status: "throttled", reset_at: "2026-10-26T05:00:00Z" }, counted, now),
  ["claude", "0 sessions now, 52 in all", "resets at 05:00 UTC"]);
assert.deepStrictEqual(C.accountLine({ provider: "claude", has_key: true, status: "throttled", reset_at: "2026-10-26T04:00:00Z" }, off, now), ["claude"], "a reset passed says nothing");
assert.deepStrictEqual(C.accountLine({ provider: "openai", has_key: false, status: "missing" }, off, now), ["openai", "no key set"]);
assert.deepStrictEqual(C.accountLine({ has_key: true, status: "healthy", expires_at: now + 7200 }, off, now), ["key expires in 2 h"]);
assert.deepStrictEqual(C.accountLine({ has_key: true, status: "healthy", expires_at: now - 1 }, off, now), ["key expired"]);
assert.deepStrictEqual(C.failure(403), { text: "The credential pool is for gardenio admins.", retry: false });
assert.strictEqual(C.failure(503).retry, false);
assert.strictEqual(C.failure(502).retry, true);
assert.strictEqual(C.failure(500), null);
`)
}

// The page inlines creds.js after the Workbench host, and its renderer registers the creds kind.
func TestPageCarriesCreds(t *testing.T) {
	if w, c := strings.Index(pageHTML, `{{asset "workbench.js"}}`), strings.Index(pageHTML, `{{asset "creds.js"}}`); c < 0 || c < w {
		t.Fatalf("creds.js must be inlined after workbench.js: %d %d", w, c)
	}
	mux := http.NewServeMux()
	(&Handler{Hub: New(newFake(), "/v", nil), BasePath: "/v"}).Register(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/garden", nil))
	for _, want := range []string{"var CREDS = ", "W.register('creds',"} {
		if !strings.Contains(inlined(t, rec.Body.String()), want) {
			t.Errorf("the page lacks %q", want)
		}
	}
}
