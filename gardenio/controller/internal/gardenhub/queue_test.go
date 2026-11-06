package gardenhub

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The Queues panel (Lands workbench-glass spec WB-6; step 5.5, gd-HuJdsFabSb).

type fakeQueues struct {
	asked []string
	doc   func(kind string, all bool) *QueueDoc
	err   error
}

func (f *fakeQueues) Queue(_ context.Context, kind string, all bool) (*QueueDoc, error) {
	a := kind
	if all {
		a += "+all"
	}
	f.asked = append(f.asked, a)
	if f.err != nil {
		return nil, f.err
	}
	return f.doc(kind, all), nil
}

// current says a panel document is never shown stale: never stored, or stored under an ETag and revalidated on every
// read (writeTagged: the bead view, the cockpit and the project panel).
func current(h http.Header) bool {
	cc := h.Get("Cache-Control")
	return cc == "no-store" || cc == "private, no-cache" && h.Get("ETag") != ""
}

func getQueue(t *testing.T, h *Handler, path string) (int, map[string]any) {
	t.Helper()
	mux := http.NewServeMux()
	h.Register(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
	if rec.Code != http.StatusOK {
		return rec.Code, nil
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") || !current(rec.Header()) {
		t.Errorf("%s: Content-Type %q, Cache-Control %q", path, ct, rec.Header().Get("Cache-Control"))
	}
	var doc map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("%s: %v\n%s", path, err, rec.Body.String())
	}
	return rec.Code, doc
}

// GET /garden/queue/{kind} asks the reader for that tab and the viewer's scope, sorts what it answers by attention, clips
// each row's text and says which writes the panel may offer.
func TestQueueViewReadsEachTab(t *testing.T) {
	f := &fakeQueues{doc: func(kind string, all bool) *QueueDoc {
		return &QueueDoc{All: all, Scoped: true, Rows: []QueueRow{
			{ID: "gd-old", Title: "old, not mine", Kind: "decision", Since: 100},
			{ID: "gd-mine2", Title: "mine, newer", Kind: "decision", Since: 300, NeedsYou: true, Why: "asked of you"},
			{ID: "gd-mine1", Title: "mine, older", Kind: "decision", Since: 200, NeedsYou: true, Why: "asked of you", Text: strings.Repeat("word ", 200)},
			{ID: "gd-noage", Title: "no age", Kind: "decision"},
		}}
	}}
	can := func(*http.Request) bool { return true }
	h := &Handler{Hub: New(newFake(), "/v", nil), BasePath: "/v", Queues: f, CanApproveEscalation: can, CanEdit: can}

	_, doc := getQueue(t, h, "/garden/queue/decisions")
	if doc["kind"] != "decisions" || doc["all"] != false || doc["edit"] != true || doc["approve"] != nil {
		t.Errorf("decisions doc: kind %v all %v edit %v approve %v", doc["kind"], doc["all"], doc["edit"], doc["approve"])
	}
	var ids []string
	for _, r := range doc["rows"].([]any) {
		ids = append(ids, r.(map[string]any)["id"].(string))
	}
	if got := strings.Join(ids, " "); got != "gd-mine1 gd-mine2 gd-old gd-noage" {
		t.Errorf("attention order %q, want needs-you first, then longest waiting, unknown age last", got)
	}
	if txt := doc["rows"].([]any)[0].(map[string]any)["text"].(string); len([]rune(txt)) > queueTextMax || !strings.HasSuffix(txt, "…") {
		t.Errorf("a long text is clipped to %d runes: %d", queueTextMax, len([]rune(txt)))
	}

	if _, doc = getQueue(t, h, "/garden/queue/escalations?all=1"); doc["approve"] != true || doc["all"] != true {
		t.Errorf("escalations?all=1: approve %v all %v", doc["approve"], doc["all"])
	}
	getQueue(t, h, "/garden/queue/inbox?all")
	getQueue(t, h, "/garden/queue/inbox?all=0")
	if got := strings.Join(f.asked, " "); got != "decisions escalations+all inbox+all inbox" {
		t.Errorf("reader asked %q", got)
	}

	if code, _ := getQueue(t, h, "/garden/queue/mail"); code != http.StatusNotFound {
		t.Errorf("an unknown tab: %d, want 404", code)
	}
	if code, _ := getQueue(t, &Handler{Hub: New(newFake(), "/v", nil), BasePath: "/v"}, "/garden/queue/inbox"); code != http.StatusServiceUnavailable {
		t.Errorf("no reader: %d, want 503", code)
	}
	if code, _ := getQueue(t, &Handler{Hub: New(newFake(), "/v", nil), BasePath: "/v", Queues: &fakeQueues{err: errors.New("down")}}, "/garden/queue/inbox"); code != http.StatusBadGateway {
		t.Errorf("a reader error: %d, want 502", code)
	}

	// no writes offered without the gates, and an empty tab is an empty list, never null
	f.doc = func(string, bool) *QueueDoc { return &QueueDoc{} }
	_, doc = getQueue(t, &Handler{Hub: New(newFake(), "/v", nil), BasePath: "/v", Queues: f}, "/garden/queue/escalations")
	if doc["approve"] != nil || doc["edit"] != nil {
		t.Errorf("no gates: approve %v edit %v", doc["approve"], doc["edit"])
	}
	if rows, ok := doc["rows"].([]any); !ok || len(rows) != 0 {
		t.Errorf("empty tab rows %#v, want []", doc["rows"])
	}
}

// A tab sends at most queueRowsMax rows, counting the rest.
func TestQueueViewCapsRows(t *testing.T) {
	f := &fakeQueues{doc: func(string, bool) *QueueDoc {
		d := &QueueDoc{More: 3}
		for i := 0; i < queueRowsMax+5; i++ {
			d.Rows = append(d.Rows, QueueRow{ID: "gd-" + strings.Repeat("x", i+1), Kind: "intake"})
		}
		return d
	}}
	_, doc := getQueue(t, &Handler{Hub: New(newFake(), "/v", nil), BasePath: "/v", Queues: f}, "/garden/queue/inbox")
	if n := len(doc["rows"].([]any)); n != queueRowsMax || doc["more"] != float64(8) {
		t.Errorf("%d rows, more %v; want %d and 8", n, doc["more"], queueRowsMax)
	}
}

// The page inlines queue.js after the Workbench it registers with, and the Mailbox carries its way into the panel.
func TestPageCarriesQueues(t *testing.T) {
	if i, j := strings.Index(pageHTML, `{{asset "workbench.js"}}`), strings.Index(pageHTML, `{{asset "queue.js"}}`); j < 0 || j < i {
		t.Fatalf("queue.js must be inlined after workbench.js: %d, %d", i, j)
	}
	mux := http.NewServeMux()
	(&Handler{Hub: New(newFake(), "/v", nil), BasePath: "/v"}).Register(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/garden", nil))
	body := inlined(t, rec.Body.String())
	for _, want := range []string{"var QUEUE = ", `id="mail-queues" href="/v/garden?queue=decisions"`} {
		if !strings.Contains(body, want) {
			t.Errorf("the page lacks %q", want)
		}
	}
}

// queue.js's pure half runs under node: the tab the Mailbox opens, a row's words, the writes each row offers, the answer
// box it carries, the empty lines, the address's all=1 and which lists changes refetch a tab.
func TestQueueLogicUnderNode(t *testing.T) {
	script := `
var Q = require(process.argv[1]), assert = require("assert");
assert.deepStrictEqual(Q.TABS.map(function (t) { return t[0]; }), ["inbox", "decisions", "escalations"]);
assert.ok(Q.isTab("inbox") && !Q.isTab("mail") && !Q.isTab(""));
assert.strictEqual(Q.tabWord("escalations"), "Escalations");
assert.strictEqual(Q.url("decisions", false), "/garden/queue/decisions");
assert.strictEqual(Q.url("decisions", true), "/garden/queue/decisions?all=1");

/* the Mailbox opens the tab that has something for the viewer */
assert.strictEqual(Q.pick(["escalation", "decision"]), "decisions");
assert.strictEqual(Q.pick(["escalation"]), "escalations");
assert.strictEqual(Q.pick([]), "inbox");
assert.strictEqual(Q.pick(), "inbox");

/* a row's words: why first (bold in the panel), then what, land, who, how long */
var now = 100000;
assert.deepStrictEqual(Q.line({ kind: "decision", needs_you: true, why: "asked of you", project: "gardenio", actor: "crew-1", since: now - 7200 }, now),
  ["asked of you", "decision", "gardenio", "crew-1", "waiting 2h"]);
assert.deepStrictEqual(Q.line({ kind: "escalation", operation: "gitlab_mr_merge", audience: "desk", sensitivity: "high", since: now - 90000 }, now),
  ["gitlab mr merge", "in hand: desk", "high", "waiting 1d"]);
assert.deepStrictEqual(Q.line({ kind: "escalation", audience: "owner" }, now), ["escalation"]);
assert.deepStrictEqual(Q.line({ kind: "intake", since: now - 30, mr: "merged" }, now), ["intake", "1m ago", "MR merged"]);
assert.deepStrictEqual(Q.line({ needs_you: true }, now), ["needs you"]);
/* an inbox row says its state (and that it is inbox-worthy); an open ask does not repeat "open", a decided one says so */
assert.deepStrictEqual(Q.line({ kind: "epic", status: "open", project: "gardenio", since: now - 90000 }, now), ["epic", "open", "gardenio", "1d ago"]);
assert.deepStrictEqual(Q.line({ kind: "intake", status: "in_progress", worthy: true }, now), ["intake", "in progress", "inbox-worthy"]);
assert.deepStrictEqual(Q.line({ kind: "decision", status: "open" }, now), ["decision"]);
assert.deepStrictEqual(Q.line({ kind: "escalation", status: "approved" }, now), ["escalation", "approved"]);

/* an escalation needs this viewer only when they may approve it; otherwise its row says who does */
var esc = { kind: "escalation", needs_you: true, why: "for the owner", audience: "owner" };
assert.strictEqual(Q.needs(esc, true), true);
assert.strictEqual(Q.needs(esc, false), false);
assert.strictEqual(Q.needs({ kind: "decision", needs_you: true }, false), true);
assert.strictEqual(Q.who(esc, true), "");
assert.strictEqual(Q.who(esc, false), "The owner approves or rejects this; your account cannot.");
assert.strictEqual(Q.who({ kind: "escalation", audience: "desk" }, false), "The merge desk approves or rejects this; your account cannot.");
assert.strictEqual(Q.who({ kind: "decision" }, false), "");

/* writes: pin and worthy on an inbox row (verbs, pressed while set), dismiss and reflag on a decision, nothing on an
   escalation, none without edit */
var inb = Q.verbs({ id: "gd-a", kind: "intake", pinned: true }, true);
assert.deepStrictEqual(inb.map(function (v) { return v.word + " " + v.path + " " + v.pressed; }), ["Unpin /b/gd-a/inbox-pin/toggle true", "Mark inbox-worthy /b/gd-a/inbox-worthy/toggle false"]);
var wor = Q.verbs({ id: "gd-a", kind: "forwarded", worthy: true }, true);
assert.deepStrictEqual([wor[0].word, wor[0].pressed, wor[1].word, wor[1].pressed], ["Pin", false, "Clear inbox-worthy", true]);
var dv = Q.verbs({ id: "gd-d", kind: "decision" }, true);
assert.deepStrictEqual(dv.map(function (v) { return v.act + " " + v.path + " " + !!v.arm + " " + !!v.reason + " " + !!v.gone; }),
  ["dismiss /b/gd-d/decision/dismiss true true true", "reflag /b/gd-d/decision/reflag true true false"]);
assert.deepStrictEqual(Q.verbs({ id: "gd-e", kind: "escalation" }, true), []);
assert.deepStrictEqual(Q.verbs({ id: "gd-a", kind: "intake" }, false), []);
assert.deepStrictEqual(Q.verbs({ id: "gd-a b", kind: "intake" }, true)[0].path, "/b/gd-a%20b/inbox-pin/toggle");

/* the answer box: every decision, an escalation only for an account that may approve */
assert.deepStrictEqual(Q.asks({ id: "gd-d", kind: "decision", title: "Ship?", options: [{ id: "y", text: "Yes" }] }, false), { kind: "decision", id: "gd-d", title: "Ship?", options: [{ id: "y", text: "Yes" }] });
assert.deepStrictEqual(Q.asks({ id: "gd-d", kind: "decision" }, false).options, []);
assert.strictEqual(Q.asks({ id: "gd-e", kind: "escalation" }, false), null);
assert.deepStrictEqual(Q.asks({ id: "gd-e", kind: "escalation", title: "Merge" }, true), { kind: "escalation", id: "gd-e", title: "Merge" });
assert.strictEqual(Q.asks({ id: "gd-a", kind: "intake" }, true), null);
/* an approved escalation (still in_progress until its executor closes it) is nobody's to answer here: no box, not "needs
   you", no "your account cannot", and its state said in place of the bead's status (gd-yuOG6cX6VI) */
assert.strictEqual(Q.asks({ id: "gd-e", kind: "escalation", title: "Merge", state: "approved" }, true), null);
assert.strictEqual(Q.asks({ id: "gd-e", kind: "escalation", state: "rejected" }, true), null);
assert.deepStrictEqual(Q.asks({ id: "gd-e", kind: "escalation", title: "Merge", state: "pending" }, true), { kind: "escalation", id: "gd-e", title: "Merge" });
assert.strictEqual(Q.needs({ kind: "escalation", needs_you: true, state: "approved" }, true), false);
assert.strictEqual(Q.needs({ kind: "escalation", needs_you: true, state: "pending" }, true), true);
assert.strictEqual(Q.who({ kind: "escalation", audience: "owner", state: "approved" }, false), "");
assert.deepStrictEqual(Q.line({ kind: "escalation", status: "in_progress", state: "approved" }, now), ["escalation", "approved"]);
assert.deepStrictEqual(Q.line({ kind: "escalation", status: "in_progress", state: "pending" }, now), ["escalation", "in progress"]);

assert.strictEqual(Q.empty("decisions", false), "No decision is waiting on you.");
assert.strictEqual(Q.empty("escalations", true), "No escalation is open.");
assert.strictEqual(Q.empty("inbox", false), "Your inbox is empty.");

/* "Everyone's" is all=1 in the address, every other parameter kept */
assert.strictEqual(Q.withAll("?queue=decisions&at=x", true), "?queue=decisions&at=x&all=1");
assert.strictEqual(Q.withAll("?queue=decisions&all=1&at=x", false), "?queue=decisions&at=x");
assert.strictEqual(Q.withAll("?all=1", false), "");
assert.strictEqual(Q.withAll("", true), "?all=1");

/* live: a lists change refetches a tab only when it concerns it (gd-6I6gmi6851) */
assert.ok(Q.touches("inbox", { reset: 1 }, []));
assert.ok(Q.touches("decisions", { rm: ["gd-d"] }, ["gd-d"]));   // answered elsewhere
assert.ok(!Q.touches("decisions", { rm: ["gd-x"] }, ["gd-d"]));
assert.ok(Q.touches("escalations", { up: [{ id: "gd-e", sec: "decision", ty: "escalation" }] }, []));   // a new escalation
assert.ok(!Q.touches("decisions", { up: [{ id: "gd-e", sec: "decision", ty: "escalation" }] }, []));
assert.ok(Q.touches("decisions", { up: [{ id: "gd-n", sec: "decision", ty: "" }] }, []));
assert.ok(!Q.touches("inbox", { up: [{ id: "gd-n", sec: "decision", ty: "decision" }, { id: "gd-w", sec: "work" }] }, ["gd-a"]));
assert.ok(Q.touches("inbox", { up: [{ id: "gd-a", sec: "agent" }] }, ["gd-a"]));
`
	runNodeLogic(t, "queue.js", script)
}

// fixtureQueues is the card world's Queues reader (card_browser_test.go): the Decisions tab the golden shoots, an inbox
// with a pinned row, and one escalation for the owner.
func fixtureQueues(now time.Time) QueueReader {
	ago := func(d time.Duration) int64 { return now.Add(-d).Unix() }
	return &fakeQueues{doc: func(kind string, all bool) *QueueDoc {
		switch kind {
		case QueueDecisions:
			return &QueueDoc{All: all, Scoped: true, Rows: []QueueRow{
				{ID: "gd-FaceDecide1", Title: "Which IdP goes first on the edge?", Kind: "decision", Type: "decision", Project: "gardenio", Status: "open",
					Text: "Keycloak is ready; GitHub needs the OAuth app. Pick the first one to roll out.", Since: ago(26 * time.Hour), NeedsYou: true, Why: "asked of you",
					Agent: &QueueRef{ID: "gd-agentBeadW", Title: "gardenio-73-github-idp"}, Task: &QueueRef{ID: "gd-EdgeIdP002", Title: "Roll the edge IdP out"},
					Options: []QueueOption{{ID: "kc", Text: "Keycloak"}, {ID: "gh", Text: "GitHub"}}},
				{ID: "gd-QueueDec001", Title: "Ship the queues panel today?", Kind: "decision", Type: "decision", Project: "gardenio", Status: "open",
					Text: "CI is green and the goldens are in.", Since: ago(2 * time.Hour), NeedsYou: true, Why: "asked of you", Actor: "crew-55",
					Options: []QueueOption{{ID: "ship", Text: "Ship it"}, {ID: "wait", Text: "Wait a day", Label: "Hold until the morning window"}}},
			}}
		case QueueEscalations:
			return &QueueDoc{All: all, Scoped: true, InHand: "1 in hand by the desk", Rows: []QueueRow{
				{ID: "gd-QueueEsc001", Title: "Merge PR 1025", Kind: "escalation", Type: "escalation", Project: "gardenio", Status: "open", Operation: "gitlab_mr_merge",
					Audience: "owner", Since: ago(3 * time.Hour), NeedsYou: true, Why: "for the owner"},
			}}
		}
		return &QueueDoc{Rows: []QueueRow{
			{ID: "gd-QueueInb001", Title: "Look at the night palette", Kind: "intake", Type: "task", Project: "gardenio", Status: "in_progress", Since: ago(5 * time.Hour), Pinned: true, NeedsYou: true, Why: "pinned"},
			{ID: "gd-QueueInb002", Title: "Bird poses for tool calls", Kind: "epic", Type: "epic", Project: "gardenio", Status: "open", Since: ago(30 * time.Hour)},
		}}
	}}
}

// The Queues panel in a real Chromium: the Mailbox opens it on the tab that has something, the tabs and Everyone's move
// the address, an answer takes its row away, a row opens its bead and Back returns, and a phone's close goes back to the
// Mailbox. What is asserted lives in testdata/queue_browser.js.
func TestQueuesUnderChromium(t *testing.T) {
	underChromium(t, "queue_browser.js", func() *httptest.Server {
		// one open decision in the Mailbox, so its link names the Decisions tab
		mail := bead("gd-mailD1", "decision", 0)
		mail.Title, mail.Status, mail.UpdatedAt = "Ship the mailbox fix?", "open", time.Now().Add(-5*time.Minute)
		mail.Fields = map[string]string{"prompt": "Ship the mailbox fix?", "options": `[{"id":"y","label":"Yes"}]`}
		return cardWorld(t, time.Now, mail)
	})
}
