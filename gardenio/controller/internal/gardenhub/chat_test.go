package gardenhub

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// The Porch chat is the agent's thread in the Commons (Lands Commons I step 10, gd-EwISaXh6Xk): chat.js asks the Commons
// whether the agent has a thread and opens its node in the workbench (gd-ZeiSZV7j2Y). Pin the route on both sides, that the
// retired /agents/chat read path is gone, and that chat.js goes nowhere but the node and the garden's own bead deep link.
func TestChatOpensTheCommonsThread(t *testing.T) {
	for _, want := range []string{"'/garden/c/bead/'", "Accept: 'application/json'", "location.assign(base + '/garden/c/n/'", "'garden:open-bead'", "base + '/garden?bead='"} {
		if !strings.Contains(pageChatJS, want) {
			t.Errorf("chat.js missing %s", want)
		}
	}
	if n := strings.Count(pageChatJS, "location.href =") + strings.Count(pageChatJS, "location.assign("); n != 2 {
		t.Errorf("chat.js navigates in %d places; the node and the bead deep link are the only two allowed", n)
	}
	for name, src := range map[string]string{"chat.js": pageChatJS, "garden.html": pageHTML, "garden.js": pageAppJS} {
		if strings.Contains(src, "/agents/chat") {
			t.Errorf("%s still reads the retired /agents/chat", name)
		}
	}
	if !strings.Contains(pageHTML, `id="chat-open"`) || strings.Contains(pageHTML, `id="chat-msgs"`) {
		t.Error("garden.html: the Porch keeps its Open button and no conversation list of its own")
	}
}

// chat.js keeps its row logic pure so it runs without a browser. Skipped when node is not installed.
func TestChatLogicUnderNode(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	script := `
var C = require(process.argv[1]), assert = require("assert");
assert.strictEqual(C.agentOf("i-gd-agent1"), "gd-agent1", "a Porch row mirrors the Ledger item i-<agent>");
assert.strictEqual(C.agentOf("gd-agent1"), "", "anything else is no agent");
assert.strictEqual(C.agentOf(undefined), "");
assert.deepStrictEqual(C.placeOf({ land: "gardenio", channel: "general", thread: "gd-w1", href: "/garden/c/gardenio/general/t/gd-w1" }),
  { land: "gardenio", channel: "general", thread: "gd-w1" });
assert.strictEqual(C.placeOf(null), null);
assert.strictEqual(C.placeOf({ land: "gardenio", channel: "general" }), null, "no thread is no place");
assert.strictEqual(C.placeOf({ land: "", channel: "general", thread: "gd-w1" }), null);
assert.strictEqual(C.placeOf({ land: 1, channel: "general", thread: "gd-w1" }), null);

`
	chat, err := filepath.Abs(filepath.Join("web", "chat.js"))
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, "-e", script, chat).CombinedOutput()
	if err != nil {
		t.Fatalf("node chat logic test failed: %v\n%s", err, out)
	}
}

// The gardener's info card (#tip) is gone (spec N-D14, 3b R3 gd-KEI7GZcivQ), so no click listener may wait for it. Load
// chat.js, reason.js and wake.js under node with a stub document, fire each click listener once, and record every selector
// it asks the target for: none names #tip. No web/ source names #tip either, so nothing draws one for a listener to find.
func TestNoClickListenerWaitsForTheRetiredCard(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	script := `
var fs = require("fs"), path = require("path"), assert = require("assert"), web = process.argv[1], seen = [];
global.window = {}; global.fetch = function () {}; global.CustomEvent = function () {};
global.document = { body: { dataset: {} }, querySelector: function () { return null; }, querySelectorAll: function () { return []; },
  getElementById: function () { return { dataset: {}, addEventListener: function () {} }; },
  addEventListener: function (type, fn) {
    if (type !== "click") return;
    fn({ button: 0, preventDefault: function () {}, stopPropagation: function () {}, target: { closest: function (sel) { seen.push(sel); return null; } } });
  } };
["chat.js", "reason.js", "wake.js"].forEach(function (f) { require(path.join(web, f)); });
assert.ok(seen.indexOf("#d-porch > li") >= 0, "chat.js's click listeners ran: " + seen.join(" | "));
seen.forEach(function (sel) { assert.ok(!/#tip\b/.test(sel), "a click listener still waits for the retired card: " + sel); });
fs.readdirSync(web).filter(function (f) { return fs.statSync(path.join(web, f)).isFile(); }).forEach(function (f) {   // web/vendor is not ours
  assert.ok(!/#tip\b|id=["']tip["']|\.id\s*=\s*["']tip["']/.test(fs.readFileSync(path.join(web, f), "utf8")), f + " names the retired #tip card");
});
`
	web, err := filepath.Abs("web")
	if err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, "-e", script, web).CombinedOutput(); err != nil {
		t.Fatalf("node #tip listener test failed: %v\n%s", err, out)
	}
}

// The acceptance walk for gd-EwISaXh6Xk in a real browser: a Porch row goes to its agent's node in the workbench
// (gd-ZeiSZV7j2Y), and an agent bound to no thread opens its own bead without the URL leaving /garden. Chromium under playwright-core
// drives /garden on an httptest server with the Commons fixture land (commons_world_test.go) mounted beside it;
// testdata/porch_chat_browser.js holds the assertions. Needs node, playwright-core (gardenio/tests/gardenhub-browser/
// node_modules, or GARDENHUB_PW_MODULES) and a Chromium it can launch; without them the test skips, unless
// GARDENHUB_BROWSER_REQUIRED=1.
func TestPorchChatUnderChromium(t *testing.T) {
	skip := t.Skipf
	if os.Getenv("GARDENHUB_BROWSER_REQUIRED") == "1" {
		skip = t.Fatalf
	}
	node, err := exec.LookPath("node")
	if err != nil {
		skip("node not installed")
	}
	mods := os.Getenv("GARDENHUB_PW_MODULES")
	if mods == "" {
		mods, _ = filepath.Abs(filepath.Join("..", "..", "..", "tests", "gardenhub-browser", "node_modules"))
	}
	if _, err := os.Stat(filepath.Join(mods, "playwright-core")); err != nil {
		skip("playwright-core not installed under %s", mods)
	}

	// Two gardeners on the Porch: one bound to the fixture's Slack wish, one bound to nothing.
	src := newFake()
	a1, a2 := bead("a-1", "agent", 7), bead("a-2", "agent", 6)
	a1.Title, a1.Fields = "gardener-one", map[string]string{"agent_state": "working", "thread_id": "gd-wishPorch"}
	a2.Title, a2.Fields = "gardener-two", map[string]string{"agent_state": "idle"}
	src.set([]*beadsapi.BeadDetail{a1, a2}, nil)
	h := New(src, "", nil)
	if err := h.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.Start()
	defer h.Close()
	fw := fixtureWorld()
	fw.add(a1, a2) // the farm reads the same agents as the Hub, as both read one daemon: the Porch draws from its gardeners[]
	f := NewFarm(fw, nil)
	f.Start()
	defer f.Close()
	d := commonsWorld(goldenNow)
	d.Put(a1)
	d.Put(a2)

	var mu sync.Mutex
	var asked []string
	mux := http.NewServeMux()
	page := &Handler{Hub: h, Farm: f, Beads: &fakeBeads{beads: map[string]*beadsapi.BeadDetail{
		"a-2": {ID: "a-2", Title: "gardener-two", Type: "agent", Status: "in_progress", Description: "Idle, between tasks."}}}}
	page.Register(mux)
	mountCommons(mux, d, page, nil, nil)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if id, ok := strings.CutPrefix(r.URL.Path, "/garden/c/bead/"); ok {
			mu.Lock()
			asked = append(asked, id+" "+r.Header.Get("Accept"))
			mu.Unlock()
		}
		mux.ServeHTTP(w, r)
	}))
	defer srv.Close()

	script, err := filepath.Abs(filepath.Join("testdata", "porch_chat_browser.js"))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(node, script, srv.URL, mods)
	cmd.Env = os.Environ()
	out, err := cmd.CombinedOutput()
	if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 3 {
		skip("no Chromium for playwright-core: %s", strings.TrimSpace(string(out)))
	}
	if err != nil {
		t.Fatalf("browser walk failed: %v\n%s", err, out)
	}
	mu.Lock()
	defer mu.Unlock()
	if !slices.Contains(asked, "a-1 application/json") {
		t.Errorf("the chat did not ask the Commons for the agent's thread as JSON: %v", asked)
	}
	for _, a := range asked {
		if !strings.HasSuffix(a, " application/json") {
			t.Errorf("the chat asked the Commons for a thread as other than JSON: %q", a)
		}
	}
	t.Logf("%s", strings.TrimSpace(string(out)))
}
