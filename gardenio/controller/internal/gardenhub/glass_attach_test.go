package gardenhub

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/coopmuxapi"
	"gardenio/controller/internal/glass"
)

// glassKeys is a fake coopmux keyboard: what was typed into which session.
type glassKeys struct {
	mu      sync.Mutex
	typed   []string
	keys    [][]string
	resized []string
	nudged  []string
	mice    []coopmuxapi.MouseEvent
	// tracking is whether the fake app takes the mouse.
	tracking bool
}

func (k *glassKeys) SendInput(_ context.Context, session, text string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.typed = append(k.typed, session+": "+text)
	return nil
}

func (k *glassKeys) SendKeys(_ context.Context, _ string, keys []string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.keys = append(k.keys, keys)
	return nil
}

func (k *glassKeys) SendMouse(_ context.Context, _ string, ev coopmuxapi.MouseEvent) (int, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if !k.tracking {
		return 0, nil
	}
	k.mice = append(k.mice, ev)
	return 9, nil
}

func (k *glassKeys) Resize(_ context.Context, session string, cols, rows uint16) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.resized = append(k.resized, session+": "+itoa(uint64(cols))+"x"+itoa(uint64(rows)))
	return nil
}

func (k *glassKeys) Nudge(_ context.Context, _, message string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.nudged = append(k.nudged, message)
	return nil
}

// newGlassAttachRig is the send rig with attach wired: an operator may attach on gardenio, only an admin elsewhere.
func newGlassAttachRig() (*glassSendRig, *glassKeys) {
	g := newGlassSendRig()
	k := &glassKeys{}
	g.h.Glass.Keyboard = k
	g.h.Glass.MayAttach = func(r *http.Request, project string) bool {
		t := glass.TierAdmin
		if project == "gardenio" {
			t = glass.TierOperator
		}
		return t.Allows(strings.Split(r.Header.Get("X-Test-Roles"), ","))
	}
	return g, k
}

func (g *glassSendRig) control(t *testing.T, agent, who, action string, extra ...string) (*httptest.ResponseRecorder, GlassControlled) {
	t.Helper()
	form := url.Values{"action": {action}}
	for i := 0; i+1 < len(extra); i += 2 {
		form.Set(extra[i], extra[i+1])
	}
	m := http.NewServeMux()
	g.h.Register(m)
	req := httptest.NewRequest(http.MethodPost, "/garden/glass/"+agent+"/control", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Test-Email", who)
	req.Header.Set("X-Test-Roles", "gardenio-operator")
	rec := httptest.NewRecorder()
	m.ServeHTTP(rec, req)
	var c GlassControlled
	_ = json.Unmarshal(rec.Body.Bytes(), &c)
	return rec, c
}

func as(who string) []string { return []string{"X-Test-Email", who} }

// Typing needs the keyboard: input before a claim is refused, after it goes to the agent's session, and after letting go
// it is refused again. Every step is audited.
func TestGlassAttach_TypingNeedsTheKeyboard(t *testing.T) {
	g, k := newGlassAttachRig()
	ls := url.Values{"type": {"input"}, "text": {"ls -la"}}
	if rec, s := g.send(t, "gd-agent1", "gardenio-operator", ls); rec.Code != http.StatusConflict || s.Result != "not_attached" {
		t.Fatalf("input before attaching: %d %+v", rec.Code, s)
	}
	rec, c := g.control(t, "gd-agent1", "op@example.com", "claim")
	if rec.Code != http.StatusOK || !c.OK || c.Result != glass.ControlClaimed || c.Control.Holder != "op@example.com" {
		t.Fatalf("claim: %d %+v", rec.Code, c)
	}
	if rec, s := g.send(t, "gd-agent1", "gardenio-operator", ls); rec.Code != http.StatusOK || !s.OK {
		t.Fatalf("input once attached: %d %+v", rec.Code, s)
	}
	if len(k.typed) != 1 || k.typed[0] != "gardenio-crew-one-abc: ls -la" {
		t.Fatalf("typed %q", k.typed)
	}
	if rec, c := g.control(t, "gd-agent1", "op@example.com", "renew"); rec.Code != http.StatusOK || c.Result != "renewed" {
		t.Fatalf("renew: %d %+v", rec.Code, c)
	}
	if rec, c := g.control(t, "gd-agent1", "op@example.com", "release"); rec.Code != http.StatusOK || c.Result != glass.ControlReleased || c.Control.Holder != "" {
		t.Fatalf("release: %d %+v", rec.Code, c)
	}
	if rec, _ := g.send(t, "gd-agent1", "gardenio-operator", ls); rec.Code != http.StatusConflict {
		t.Fatalf("input after letting go: %d", rec.Code)
	}
	if rec, c := g.control(t, "gd-agent1", "op@example.com", "renew"); rec.Code != http.StatusConflict || c.Result != "not_attached" {
		t.Fatalf("renewing a let-go keyboard: %d %+v", rec.Code, c)
	}
	var got []string
	for _, l := range g.auditLines(t) {
		got = append(got, l["subverb"].(string)+"="+l["result"].(string))
		if l["surface"] != "glass" || l["operator"] != "op@example.com" || l["audit_id"] == "" {
			t.Errorf("audit line %v", l)
		}
	}
	if want := "input=refused claim=claimed input=ok release=released input=refused"; strings.Join(got, " ") != want {
		t.Fatalf("audit = %q, want %q", strings.Join(got, " "), want)
	}
}

// Another person's keyboard is not taken by accident: a claim over it is refused naming them, their typing still goes,
// the newcomer's does not; taking it says so, and then only the new holder types.
func TestGlassAttach_OnePersonAtATime(t *testing.T) {
	g, k := newGlassAttachRig()
	if _, c := g.control(t, "gd-agent1", "alice@example.com", "claim"); !c.OK {
		t.Fatalf("alice's claim: %+v", c)
	}
	rec, c := g.control(t, "gd-agent1", "bob@example.com", "claim")
	if rec.Code != http.StatusConflict || c.Result != "held" || c.Control.Holder != "alice@example.com" || !strings.Contains(c.Note, "alice@example.com") {
		t.Fatalf("bob's claim over alice's: %d %+v", rec.Code, c)
	}
	if rec, s := g.send(t, "gd-agent1", "gardenio-operator", url.Values{"type": {"input"}, "text": {"x"}}, as("bob@example.com")...); rec.Code != http.StatusConflict || !strings.Contains(s.Note, "alice@example.com has the keyboard") {
		t.Fatalf("bob typing into alice's keyboard: %d %+v", rec.Code, s)
	}
	_, c = g.control(t, "gd-agent1", "bob@example.com", "claim", "take", "1")
	if !c.OK || c.Result != glass.ControlTaken || c.Control.From != "alice@example.com" || c.Control.Holder != "bob@example.com" {
		t.Fatalf("bob taking it: %+v", c)
	}
	if rec, _ := g.send(t, "gd-agent1", "gardenio-operator", url.Values{"type": {"input"}, "text": {"a"}}, as("alice@example.com")...); rec.Code != http.StatusConflict {
		t.Fatalf("alice typed after bob took the keyboard: %d", rec.Code)
	}
	if rec, _ := g.send(t, "gd-agent1", "gardenio-operator", url.Values{"type": {"input"}, "text": {"b"}}, as("bob@example.com")...); rec.Code != http.StatusOK {
		t.Fatalf("bob typing: %d", rec.Code)
	}
	if len(k.typed) != 1 || k.typed[0] != "gardenio-crew-one-abc: b" {
		t.Fatalf("typed %q", k.typed)
	}
	// alice letting go of what she no longer holds changes nothing
	if _, c := g.control(t, "gd-agent1", "alice@example.com", "release"); c.Result != "not_attached" || c.Control.Holder != "bob@example.com" {
		t.Fatalf("alice's release: %+v", c)
	}
}

// Who may attach: an operator on the operator's land, not on a tenant land (admin only until Fences), never a viewer;
// and an agent outside the viewer's lands is not there at all.
func TestGlassAttach_Gates(t *testing.T) {
	g, k := newGlassAttachRig()
	g.h.Glass.InScope = func(*http.Request, string) bool { return true }
	if rec, _ := g.control(t, "gd-other", "op@example.com", "claim"); rec.Code != http.StatusForbidden {
		t.Fatalf("an operator claiming on a tenant land: %d", rec.Code)
	}
	if rec, _ := g.control(t, "gd-idle", "op@example.com", "claim"); rec.Code != http.StatusConflict {
		t.Fatalf("a claim on an agent with no session: %d", rec.Code)
	}
	if rec, _ := g.control(t, "gd-agent1", "op@example.com", "grab"); rec.Code != http.StatusBadRequest {
		t.Fatalf("an unknown action: %d", rec.Code)
	}
	g.h.Glass.InScope = func(_ *http.Request, p string) bool { return p == "gardenio" }
	if rec, _ := g.control(t, "gd-other", "op@example.com", "claim"); rec.Code != http.StatusNotFound {
		t.Fatalf("a claim outside the viewer's lands: %d", rec.Code)
	}
	if rec, _ := g.send(t, "gd-agent1", "gardenio-viewer", url.Values{"type": {"input"}, "text": {"x"}}); rec.Code != http.StatusForbidden {
		t.Fatalf("a viewer typing: %d", rec.Code)
	}
	if rec, _ := g.control(t, "gd-agent1", "", "claim"); rec.Code != http.StatusForbidden {
		t.Fatalf("a claim by nobody in particular: %d", rec.Code)
	}
	if len(k.typed)+len(k.keys) != 0 {
		t.Fatal("a refused send typed")
	}
}

// Keys: named keys coop can type go on; Ctrl-C and its like (Esc too) ask first and go only once confirmed, a cancel is audited;
// typed text with a control character in it is refused (those are keys); resize and compact go too, compact confirmed.
func TestGlassAttach_KeysResizeCompact(t *testing.T) {
	g, k := newGlassAttachRig()
	if _, c := g.control(t, "gd-agent1", "op@example.com", "claim"); !c.OK {
		t.Fatal(c)
	}
	op := "gardenio-operator"
	if rec, s := g.send(t, "gd-agent1", op, url.Values{"type": {"keys"}, "keys": {"Tab, Up,Enter"}}); rec.Code != http.StatusOK || !s.OK {
		t.Fatalf("keys: %d %+v", rec.Code, s)
	}
	if rec, s := g.send(t, "gd-agent1", op, url.Values{"type": {"keys"}, "keys": {"Up,Ctrl-C"}}); rec.Code != http.StatusConflict || s.Result != "confirm" || !strings.Contains(s.Note, "Up Ctrl-C") {
		t.Fatalf("an unconfirmed Ctrl-C: %d %+v", rec.Code, s)
	}
	if rec, s := g.send(t, "gd-agent1", op, url.Values{"type": {"keys"}, "keys": {"Ctrl-C"}, "cancel": {"1"}}); rec.Code != http.StatusOK || s.Result != "canceled" {
		t.Fatalf("a canceled Ctrl-C: %d %+v", rec.Code, s)
	}
	if rec, _ := g.send(t, "gd-agent1", op, url.Values{"type": {"keys"}, "keys": {"Ctrl-C"}, "confirmed": {"1"}}); rec.Code != http.StatusOK {
		t.Fatalf("a confirmed Ctrl-C: %d", rec.Code)
	}
	if rec, s := g.send(t, "gd-agent1", op, url.Values{"type": {"keys"}, "keys": {"Esc"}}); rec.Code != http.StatusConflict || s.Result != "confirm" || !strings.Contains(s.Note, "Send Esc to ") {
		t.Fatalf("an unconfirmed Esc (Claude Code's interrupt): %d %+v", rec.Code, s)
	}
	if got, _ := json.Marshal(k.keys); string(got) != `[["Tab","Up","Enter"],["Ctrl-C"]]` {
		t.Fatalf("keys sent %s", got)
	}
	for _, bad := range []url.Values{
		{"type": {"keys"}, "keys": {"Alt-X"}},
		{"type": {"keys"}, "keys": {""}},
		{"type": {"keys"}, "keys": {strings.Repeat("a,", 40)}},
		{"type": {"input"}, "text": {"rm -rf /\r"}},
		{"type": {"input"}, "text": {"\x1b[201~"}},
		{"type": {"input"}, "text": {""}},
		{"type": {"resize"}, "cols": {"0"}, "rows": {"24"}},
		{"type": {"resize"}, "cols": {"80"}, "rows": {"x"}},
	} {
		if rec, _ := g.send(t, "gd-agent1", op, bad); rec.Code != http.StatusBadRequest {
			t.Errorf("%v: %d, want 400", bad, rec.Code)
		}
	}
	if len(k.typed) != 0 {
		t.Fatalf("typed %q", k.typed)
	}
	if rec, _ := g.send(t, "gd-agent1", op, url.Values{"type": {"resize"}, "cols": {"132"}, "rows": {"40"}}); rec.Code != http.StatusOK || len(k.resized) != 1 || k.resized[0] != "gardenio-crew-one-abc: 132x40" {
		t.Fatalf("resize: %d %q", rec.Code, k.resized)
	}
	if rec, s := g.send(t, "gd-agent1", op, url.Values{"type": {"compact"}}); rec.Code != http.StatusConflict || s.Result != "confirm" || len(k.nudged) != 0 {
		t.Fatalf("an unconfirmed compact: %d %+v", rec.Code, s)
	}
	if rec, _ := g.send(t, "gd-agent1", op, url.Values{"type": {"compact"}, "confirmed": {"1"}}); rec.Code != http.StatusOK || len(k.nudged) != 1 || k.nudged[0] != "/compact" {
		t.Fatalf("compact: %d %q", rec.Code, k.nudged)
	}
	results := map[string]int{}
	for _, l := range g.auditLines(t) {
		results[l["subverb"].(string)+"="+l["result"].(string)]++
	}
	for _, want := range []string{"keys=confirm_required", "keys=ux_canceled", "keys=ok", "compact=confirm_required", "compact=ok", "resize=ok"} {
		if results[want] == 0 {
			t.Errorf("no %s audit line: %v", want, results)
		}
	}
}

// Mouse (gd-5LOtAx1sh1): only from the holder, only an SGR press, release or wheel turn, read into a button and a cell for
// coop to encode (never raw bytes); a motion report or anything else is refused, and a click the app would not take says so.
func TestGlassAttach_Mouse(t *testing.T) {
	g, k := newGlassAttachRig()
	op := "gardenio-operator"
	click := url.Values{"type": {"mouse"}, "report": {"\x1b[<0;10;5M"}}
	if rec, s := g.send(t, "gd-agent1", op, click); rec.Code != http.StatusConflict || s.Result != "not_attached" {
		t.Fatalf("a click before attaching: %d %+v", rec.Code, s)
	}
	if _, c := g.control(t, "gd-agent1", "op@example.com", "claim"); !c.OK {
		t.Fatal(c)
	}
	if rec, s := g.send(t, "gd-agent1", op, click); rec.Code != http.StatusOK || s.Result != "not_tracking" || len(k.mice) != 0 {
		t.Fatalf("a click the app does not take: %d %+v", rec.Code, s)
	}
	k.tracking = true
	for _, r := range []string{"\x1b[<0;10;5M", "\x1b[<18;300;40m", "\x1b[<65;1;1M"} {
		if rec, s := g.send(t, "gd-agent1", op, url.Values{"type": {"mouse"}, "report": {r}}); rec.Code != http.StatusOK || s.Result != "delivered" {
			t.Fatalf("%q: %d %+v", r, rec.Code, s)
		}
	}
	got, _ := json.Marshal(k.mice)
	if string(got) != `[{"button":"left","col":10,"row":5},{"button":"right","col":300,"row":40,"release":true,"ctrl":true},{"button":"wheel_down","col":1,"row":1}]` {
		t.Fatalf("mouse sent %s", got)
	}
	for _, bad := range []string{"\x1b[<32;10;5M", "\x1b[<3;1;1M", "\x1b[<66;1;1M", "\x1b[<128;1;1M", "\x1b[<0;0;5M", "\x1b[<0;1;501M",
		"\x1b[M !!", "\x1b[<0;1;1M\x1b[<0;1;1M", "\x1b[<0;1;1Mx", "rm -rf /\r", ""} {
		if rec, _ := g.send(t, "gd-agent1", op, url.Values{"type": {"mouse"}, "report": {bad}}); rec.Code != http.StatusBadRequest {
			t.Errorf("%q: %d, want 400", bad, rec.Code)
		}
	}
	if len(k.mice) != 3 || len(k.typed) != 0 || len(k.keys) != 0 {
		t.Fatalf("sent %d mice, typed %q, keys %q", len(k.mice), k.typed, k.keys)
	}
	results := map[string]int{}
	for _, l := range g.auditLines(t) {
		results[l["subverb"].(string)+"="+l["result"].(string)]++
	}
	if results["mouse=ok"] != 3 || results["mouse=not_tracking"] != 1 {
		t.Errorf("mouse audit lines %v", results)
	}
}

// The panel's document says who holds the keyboard, and who the viewer is, so the Glass can tell its own claim from
// another's; can_attach follows MayAttach.
func TestGlassAttach_Document(t *testing.T) {
	g, _ := newGlassAttachRig()
	var d GlassDoc
	_ = json.Unmarshal(glassGet(t, g.h, "/garden/glass/gd-agent1", "gardenio-operator").Body.Bytes(), &d)
	if !d.CanAttach || d.Control != nil || d.You != "op@example.com" {
		t.Fatalf("doc before a claim: can_attach=%v control=%+v you=%q", d.CanAttach, d.Control, d.You)
	}
	g.control(t, "gd-agent1", "alice@example.com", "claim")
	d = GlassDoc{}
	_ = json.Unmarshal(glassGet(t, g.h, "/garden/glass/gd-agent1", "gardenio-operator").Body.Bytes(), &d)
	if d.Control == nil || d.Control.Holder != "alice@example.com" {
		t.Fatalf("doc while alice holds it: %+v", d.Control)
	}
	g.h.Glass.Keyboard = nil
	d = GlassDoc{}
	_ = json.Unmarshal(glassGet(t, g.h, "/garden/glass/gd-agent1", "gardenio-operator").Body.Bytes(), &d)
	if d.CanAttach {
		t.Fatal("attach offered with no keyboard to type with")
	}
}

func TestGlassAttachTier(t *testing.T) {
	if GlassAttachTier(nil, "gardenio") != glass.TierAdmin {
		t.Error("a land nobody can place is admin-only")
	}
	f := NewFarm(nil, nil)
	if GlassAttachTier(f, "gardenio") != glass.TierAdmin {
		t.Error("before the farm's first cost read, every land is admin-only")
	}
}

// glasskey.js's pure half under node: the routes it posts to, what xterm's typing becomes (text and named keys, in order,
// control characters never as text, sequences coop cannot name dropped), and what the banner says.
func TestGlassKeyLogicUnderNode(t *testing.T) {
	script := `
var K = require(process.argv[1]), assert = require("assert");
assert.strictEqual(K.sendURL("gd-a b"), "/garden/glass/gd-a%20b/send");
assert.strictEqual(K.controlURL("gd-a"), "/garden/glass/gd-a/control");

assert.deepStrictEqual(K.frames("ls -la"), [{ type: "input", text: "ls -la" }]);
assert.deepStrictEqual(K.frames("ls\r"), [{ type: "input", text: "ls" }, { type: "keys", keys: ["Enter"] }]);
assert.deepStrictEqual(K.frames("\x1b[A\x1b[B\x1bOC\x1b[D"), [{ type: "keys", keys: ["Up", "Down", "Right", "Left"] }], "arrows, both cursor modes");
assert.deepStrictEqual(K.frames("\x03"), [{ type: "keys", keys: ["Ctrl-C"] }]);
assert.deepStrictEqual(K.frames("\t\x7f\x1b"), [{ type: "keys", keys: ["Tab", "Backspace", "Esc"] }]);
assert.deepStrictEqual(K.frames("\x1b[3~\x1b[5~\x1b[15~"), [{ type: "keys", keys: ["Delete", "PageUp", "F5"] }]);
assert.deepStrictEqual(K.frames("\x1b[200~one\rtwo\x1b[201~"), [{ type: "input", text: "one" }, { type: "keys", keys: ["Enter"] }, { type: "input", text: "two" }],
  "a bracketed paste: its marks dropped, its lines typed");
assert.deepStrictEqual(K.frames("a\x1bxb"), [{ type: "input", text: "ab" }], "an Alt chord is dropped, not sent as Esc and a letter");
assert.deepStrictEqual(K.frames("\x1b[1;2A"), [], "Shift-Up: coop has no name for it");
assert.deepStrictEqual(K.frames("\x00\x1c\x9b"), [], "NUL, Ctrl-\\ and C1 are not typed");
assert.deepStrictEqual(K.frames("héllo 😀"), [{ type: "input", text: "héllo 😀" }]);
assert.deepStrictEqual(K.frames(""), []);
assert.deepStrictEqual(K.frames("a\x1b[<0;10;5M\x1b[<0;10;5mb"), [{ type: "input", text: "a" }, { type: "mouse", report: "\x1b[<0;10;5M" }, { type: "mouse", report: "\x1b[<0;10;5m" }, { type: "input", text: "b" }],
  "SGR mouse reports go as mouse, in order");
assert.deepStrictEqual(K.frames("\x1b[<32;11;5M\x1b[<35;12;5M"), [], "motion is dropped");
assert.deepStrictEqual(K.frames("\x1b[M !!x"), [{ type: "input", text: "x" }], "an X10 report is dropped whole, not typed");
K.frames("x\x1b[A\x03y").forEach(function (f) {
  if (f.type === "input") assert.ok(!/[\x00-\x1f\x7f]/.test(f.text), "no control character rides as text");
});

assert.deepStrictEqual(K.controlLine(null, "me@x"), { who: "", mine: false, text: "" });
var mine = K.controlLine({ holder: "me@x" }, "me@x");
assert.ok(mine.mine && /Attached as you \(me@x\)/.test(mine.text) && /Esc Esc/.test(mine.text));
var theirs = K.controlLine({ holder: "bob@x" }, "me@x");
assert.ok(!theirs.mine && theirs.text === "Attached as bob@x: they have the keyboard. You are watching.");
assert.strictEqual(K.lostLine({ holder: "bob@x", reason: "taken", from: "me@x" }, "me@x"), "bob@x took the keyboard. You are watching again.");
assert.ok(/lapsed/.test(K.lostLine({ reason: "expired", from: "me@x" }, "me@x")));
assert.strictEqual(K.lostLine({ holder: "me@x", reason: "claimed" }, "me@x"), "");
assert.strictEqual(K.fitCols(1000, 8), 125);
assert.strictEqual(K.fitCols(0, 8), 0);
assert.strictEqual(K.fitCols(10, 8), 2);
assert.ok(/click in its prompt to move the cursor/.test(mine.text));

// Click to move the cursor (gd-FpaO6Pb9bR): Claude Code's prompt between two rules, a wrapped second line under the caret's.
var screen = ["* Read(foo.go)", "", "────────────", "> fix the token exchange", "  and the tests", "────────────", "  ? for shortcuts"];
function at(y) { return y >= 0 && y < screen.length ? screen[y] : null; }
assert.ok(K.bound("") && K.bound("   ") && K.bound("──────") && K.bound("╭──╮") && K.bound(" ╰──╯"), "blank rows, rules and corners bound the box");
assert.ok(!K.bound("│ > hi │") && !K.bound("> hi") && !K.bound("  and the tests"), "a box side and text are inside");
assert.deepStrictEqual(K.rowKeys(at, 3, 3), []);
assert.deepStrictEqual(K.rowKeys(at, 3, 4), ["Down"]);
assert.deepStrictEqual(K.rowKeys(at, 4, 3), ["Up"]);
assert.strictEqual(K.rowKeys(at, 3, 0), null, "a click on the output above moves nothing: Up would reach the history");
assert.strictEqual(K.rowKeys(at, 3, 2), null, "nor on the rule");
assert.strictEqual(K.rowKeys(at, 4, 6), null, "nor under the box");
assert.strictEqual(K.rowKeys(at, 3, 9), null, "nor past the screen");
assert.strictEqual(K.rowKeys(at, 1, 1).length, 0, "on the caret's own row, nothing to move");
function cells(s) { return Array.from(s); }
var row = cells("> fix the token exchange      ");
assert.deepStrictEqual(K.colKeys(row, 24, 6), K.colKeys(row, 24, 6).map(function () { return "Left"; }));
assert.strictEqual(K.colKeys(row, 24, 6).length, 18, "left along the row, a key a character");
assert.deepStrictEqual(K.colKeys(row, 2, 5), ["Right", "Right", "Right"]);
assert.strictEqual(K.colKeys(row, 2, 28).length, 22, "a click past the text stops at its end");
assert.strictEqual(K.colKeys(row, 5, 0).length, 3, "a click on the prompt mark stops at the text's start");
assert.deepStrictEqual(K.colKeys(row, 6, 6), []);
var boxed = cells("│ > hi there   │");
assert.strictEqual(K.colKeys(boxed, 4, 15).length, 8, "to the end of the text, not into the box's side");
assert.strictEqual(K.colKeys(boxed, 6, 0).length, 2, "nor its side and mark on the left");
var wide = ["a", "漢", null, "字", null, "b", " "];
assert.strictEqual(K.colKeys(wide, 0, 5).length, 3, "a wide character is one key, not two");
assert.strictEqual(K.colKeys(wide, 0, 2).length, 1, "a click on its second half lands on its first");
assert.strictEqual(K.colKeys(wide, 5, 4).length, 1);
assert.strictEqual(K.soleInverse([false, false, true, false]), 2, "the drawn caret");
assert.strictEqual(K.soleInverse([true, true, false, false, true]), 4, "a run of inverse cells (a highlight) is not the caret");
assert.strictEqual(K.soleInverse([true, true, true]), -1);
assert.strictEqual(K.soleInverse([]), -1);

assert.deepStrictEqual(K.TOUCH.map(function (k) { return k[1]; }), ["Esc", "Tab", "ctrl", "Up", "Down", "Left", "Right", "paste"],
  "the touch row: the keys a phone's keyboard lacks (gd-2yJFWJnO1I)");
assert.strictEqual(K.ctrlOf("c"), "\x03");
assert.strictEqual(K.ctrlOf("Z"), "\x1a", "either case");
assert.strictEqual(K.ctrlOf("ab"), "", "only one letter");
assert.strictEqual(K.ctrlOf("1"), "");
assert.deepStrictEqual(K.frames(K.ctrlOf("d")), [{ type: "keys", keys: ["Ctrl-D"] }], "Ctrl held and d typed is Ctrl-D");

assert.ok(K.ESC_GAP > 0 && K.ESC_GAP < 1000 && K.BLUR_MS >= 30000 && K.RENEW_MS * 2 < 90000, "renewed well inside the lease");
`
	runNodeLogic(t, "glasskey.js", script)
}

// glasskey.js is the keyboard, and only it: it posts to the Glass's own /send and /control, opens no WebSocket and
// reaches no viewer route; it types only once attached (the terminal's onData is wired in arm, when the keyboard is held).
func TestGlassKeyWritesOnlyThroughTheGlass(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("web", "glasskey.js"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	for _, banned := range []string{"WebSocket", "/agents/", "innerHTML", ".onBinary", "'nudge'", "'respond'"} {
		if strings.Contains(s, banned) {
			t.Errorf("glasskey.js contains %q", banned)
		}
	}
	if strings.Count(s, "method: 'POST'") != 1 || strings.Count(s, ".onData(") != 1 || !strings.Contains(s, "if (on && !c.sub && t.onData)") {
		t.Error("glasskey.js posts in one place and wires typing only when the keyboard is held")
	}
}

// The card world's Glass offers attach on gd-GlassKey001 alone, live (the glassattach golden), and on none of the watch
// shots' agents.
func TestGlassAttachFixture(t *testing.T) {
	beads := &fakeBeads{beads: map[string]*beadsapi.BeadDetail{}}
	h := &Handler{BasePath: "/v", Beads: beads}
	h.Glass = fixtureGlass(t, beads)
	for id, want := range map[string]bool{"gd-GlassKey001": true, "gd-GlassLive01": false, "gd-GlassAsk001": false} {
		var d GlassDoc
		_ = json.Unmarshal(glassGet(t, h, "/garden/glass/"+id, "gardenio-operator").Body.Bytes(), &d)
		if d.CanAttach != want || !d.Live {
			t.Errorf("%s: can_attach %v live %v, want can_attach %v and live", id, d.CanAttach, d.Live, want)
		}
	}
}

// The Glass is a terminal you can use on a phone (M2.3, gd-2yJFWJnO1I): attached, a touch screen's key row has Esc, Tab,
// Ctrl, the arrows and Paste, 44 px each on one row, a tap keeps the phone's keyboard up, and a pinch on the terminal sizes
// its glyphs rather than zooming the page; a desktop shows none of those keys. What is asserted lives in
// testdata/glass_phone_browser.js.
func TestGlassPhoneUnderChromium(t *testing.T) {
	underChromium(t, "glass_phone_browser.js", func() *httptest.Server { return cardWorld(t, time.Now) })
}
