package gardenhub

import (
	"context"
	"encoding/json"
	"maps"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/commons/commonstest"
)

// wishBeads is the bead view's reader holding the fixture wish and its family (commonstest.WishFamily).
func wishBeads() *fakeBeads {
	f := &fakeBeads{beads: map[string]*beadsapi.BeadDetail{}, comments: map[string][]beadsapi.Comment{}}
	wish := wishFamily(time.Unix(1_790_000_000, 0))
	for _, b := range wish.Beads {
		f.beads[b.ID] = b
	}
	f.deps = append(f.deps, wish.Deps...)
	maps.Copy(f.comments, wish.Comments)
	return f
}

// GET /garden/bead/<head> of the fixture wish carries its kin, read once by thread_id: the two replies, three squawks
// and the card in kin.conversation, oldest first (plan gd-gO1fTT7oGC step gd-58mkDiWkNq).
func TestBeadViewCarriesKin(t *testing.T) {
	beads := wishBeads()
	rec, doc := getBead(t, beadMux(beads), wishSlack)
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if q := beads.listQ; q.Fields["thread_id"] != wishSlack || q.Sort != "-created_at" || q.Limit != 500 {
		t.Errorf("kin query = %+v, want thread_id=%s, -created_at, 500", q, wishSlack)
	}
	k, _ := doc["kin"].(map[string]any)
	var ids, vias []string
	for _, l := range k["conversation"].([]any) {
		m := l.(map[string]any)
		ids, vias = append(ids, m["id"].(string)), append(vias, m["via"].(string))
	}
	wantIDs := []string{commonstest.WishReply1, commonstest.WishCard, commonstest.WishSquawk1, commonstest.WishSquawk2,
		commonstest.WishReply2, commonstest.WishSquawk3}
	for i := range wantIDs {
		wantIDs[i] = wishSlack + wantIDs[i]
	}
	if !slices.Equal(ids, wantIDs) {
		t.Errorf("conversation = %v, want %v", ids, wantIDs)
	}
	if want := []string{"slack", "card", "squawk", "squawk", "slack", "squawk"}; !slices.Equal(vias, want) {
		t.Errorf("vias = %v, want %v", vias, want)
	}
	if k["head"] != nil || k["sessions"] != 1.0 || len(k["work"].([]any)) != 1 || len(k["agents"].([]any)) != 1 {
		t.Errorf("kin = %v, want no head, one session, the task and the agent", k)
	}

	// Read from the task, the kin name the head and where it was asked.
	_, doc = getBead(t, beadMux(beads), wishSlack+commonstest.WishTask)
	h, _ := doc["kin"].(map[string]any)["head"].(map[string]any)
	if h["id"] != wishSlack || h["where"] != "slack" {
		t.Errorf("task's kin head = %v, want %s asked in Slack", h, wishSlack)
	}
}

// A bead in no thread reads no kin; a reader without the list sends none; a failed list is named in Partial.
func TestBeadViewKinDegrades(t *testing.T) {
	beads := wishBeads()
	beads.beads["gd-lone"] = &beadsapi.BeadDetail{ID: "gd-lone", Type: "task", Status: "open"}
	if _, doc := getBead(t, beadMux(beads), "gd-lone"); doc["kin"] != nil || beads.listQ.Fields != nil {
		t.Errorf("a bead in no thread: kin = %v, list %+v, want neither", doc["kin"], beads.listQ)
	}
	if _, doc := getBead(t, beadMux(readOnlyBeads{beads}), wishSlack); doc["kin"] != nil || doc["partial"] != nil {
		t.Errorf("no lister: kin = %v, partial = %v, want neither", doc["kin"], doc["partial"])
	}
	beads.failList = true
	_, doc := getBead(t, beadMux(beads), wishSlack)
	if doc["kin"] != nil || !slices.Contains(doc["partial"].([]any), any("kin")) {
		t.Errorf("failed list: kin = %v, partial = %v, want kin in partial", doc["kin"], doc["partial"])
	}
}

// The panel's Conversation section (web/beadtalk.js talk, gd-tx2wMH48MV) of the fixture wish: one timeline, oldest first,
// of the person's replies, the crew's squawks and its card, with the head's two kd comments in time between them, each
// named as the Commons names its author; the card a state line naming its agent and its state, never its "Agent: ..." text.
func TestConversationUnderNode(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	d, err := readBead(context.Background(), wishBeads(), wishSlack)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(d)
	script := `
var V = require(process.argv[1]), assert = require("assert"), D = JSON.parse(process.argv[2]);
var agent = "gardenio-one-bead-view-k2p", lines = V.talk(D);
assert.deepStrictEqual(lines.map(function (l) { return [l.k, l.who, l.k === "msg" ? l.voice + " " + l.via : l.k === "card" ? l.state : ""]; }), [
  ["msg", "matt", "person in Slack"],
  ["comment", "thread-1789906400.000100", ""],
  ["comment", "matt", ""],
  ["card", agent, "working"],
  ["msg", agent, "agent squawk"],
  ["msg", agent, "agent squawk"],
  ["msg", "matt", "person in Slack"],
  ["msg", agent, "agent squawk"],
]);
assert.strictEqual(lines[3].agent, D.id + "Agt", "the card links its agent's panel");
assert.ok(lines.every(function (l, i) { return !i || lines[i - 1].at <= l.at; }), "oldest first");
assert.strictEqual(V.talk({ id: "gd-lone", comments: [{ author: "x", text: "y", at: 1 }] }), null, "a bead in no thread has no conversation");
assert.strictEqual(V.CONV_FOLD, 12);

// a line the Commons stream brings (web/commons2.js put): a new one named and voiced as the document names one; one the
// conversation has, drawn again with its words grown, deleted, or its card's state, keeping who and how it came
assert.deepStrictEqual(["oidc:ada@example.com", "slack:U1", "ada@example.com", agent, "agent:gardenio-steward-x1", "gardenio-steward-x1", "other-steward-x1"]
  .map(function (a) { return V.voice(a, "gardenio"); }), ["person", "person", "person", "agent", "steward", "steward", "agent"]);
var said = V.heard({ id: "m1", author: "agent:" + agent, at: 5, text: "first" }, null, "gardenio");
assert.deepStrictEqual([said.k, said.who, said.voice, said.text], ["msg", agent, "agent", "first"]);
var grown = V.heard({ id: "m1", text: "first\nsecond" }, lines[4], "gardenio");
assert.deepStrictEqual([grown.who, grown.via, grown.text], [lines[4].who, lines[4].via, "first\nsecond"]);
assert.notStrictEqual(lines[4].text, "first\nsecond", "the line it was drawn from is left as it is");
assert.ok(V.heard({ id: "m1", gone: true, text: null }, lines[4]).gone, "a tombstone deletes it");
assert.strictEqual(V.heard({ id: "m1", text: null }, lines[4]).text, lines[4].text, "a row that does not carry its words keeps them");
var card = V.heard({ id: lines[3].id, card: { agent: lines[3].agent, state: "", live: false } }, lines[3]);
assert.deepStrictEqual([card.k, card.who, card.state], ["card", agent, "left"]);
assert.strictEqual(V.heard({ id: "c2", author: "agent:x", card: { agent: "gd-a", state: "awaiting_input", live: true, name: "crew-a" } }).state, "awaiting input");
`
	js, _ := filepath.Abs(filepath.Join("web", "beadtalk.js"))
	out, err := exec.Command(node, "-e", script, js, string(raw)).CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
}

// Squawks and cards keep their voice in the panel's Conversation (gd-d3irroS7Q1): the fixture wish's three squawks are
// the crew's, named by its from: label and marked squawk, its card the one state line; and a line the Commons stream
// brings (web/commons2.js put reads the row's data-voice and data-via, commons render.go "message") keeps the voice and
// via the row says, so a squawk heard live is still a squawk and a Slack reply still the person's, not a Commons message.
func TestConversationVoicesUnderNode(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	d, err := readBead(context.Background(), wishBeads(), wishSlack)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(d)
	script := `
var V = require(process.argv[1]), assert = require("assert"), D = JSON.parse(process.argv[2]);
var agent = "gardenio-one-bead-view-k2p", lines = V.talk(D);
var sq = lines.filter(function (l) { return l.k === "msg" && l.via === "squawk"; });
assert.deepStrictEqual(sq.map(function (l) { return [l.who, l.voice]; }), [[agent, "agent"], [agent, "agent"], [agent, "agent"]], "three squawks, the crew's");
assert.deepStrictEqual(lines.filter(function (l) { return l.k === "card"; }).map(function (l) { return [l.who, l.state, l.text]; }), [[agent, "working", undefined]], "one card, a state line");
var heard = function (row) { var l = V.heard(row, null, "gardenio"); return [l.k, l.who, l.voice, l.via]; };
assert.deepStrictEqual(heard({ id: "s1", author: "agent:" + agent, voice: "agent", via: "squawk", text: "PR is up" }), ["msg", agent, "agent", "squawk"]);
assert.deepStrictEqual(heard({ id: "r1", author: "oidc:matt@example.com", voice: "person", via: "slack", text: "thanks" }), ["msg", "matt", "person", "in Slack"]);
assert.deepStrictEqual(heard({ id: "c1", author: "oidc:ada@example.com", voice: "", via: "", text: "hi" }), ["msg", "ada", "person", "in the Commons"], "a row with no via is a Commons message, voiced by its author");
assert.deepStrictEqual(heard({ id: "w1", author: "agent:gardenio-steward-x1", voice: "steward", via: "commons", text: "on it" }), ["msg", "gardenio-steward-x1", "steward", "in the Commons"]);
`
	js, _ := filepath.Abs(filepath.Join("web", "beadtalk.js"))
	out, err := exec.Command(node, "-e", script, js, string(raw)).CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
}

// The fixture wish's face (gd-zn6iUnPd9Q): who asked (the opener the bridge resolved into operated_by), in Slack's
// channel, the root text as its ask, and the counts of its kin; the generic fields no longer repeat the root text. Its
// task, with no thread label, keeps the work face.
func TestWishFace(t *testing.T) {
	d, err := readBead(context.Background(), wishBeads(), wishSlack)
	if err != nil {
		t.Fatal(err)
	}
	if d.Face == nil || d.Face.Kind != "wish" || d.Face.Wish == nil {
		t.Fatalf("face = %+v, want a wish", d.Face)
	}
	w, root := d.Face.Wish, wishBeads().beads[wishSlack].Fields["slack_root_text"]
	if w.Asker != "oidc:matt@example.com" || w.Where != "slack" || w.Channel != "C0GARDEN01" || w.Ask != root || w.At != 1_790_000_000-26*3600 {
		t.Errorf("wish = %+v, want matt's ask in Slack C0GARDEN01 at the fixture's t0", w)
	}
	if !w.Counted || w.Replies != 5 || w.AgentsLive != 1 || w.AgentsLeft != 0 || w.WorkOpen != 1 || w.WorkClosed != 0 || w.PRsMerged != 0 {
		t.Errorf("counts = %+v, want 5 replies, 1 agent live, 1 open", w)
	}
	for _, f := range d.Fields {
		if f.K == "slack_root_text" || f.V == root {
			t.Errorf("fields repeat the ask: %+v", f)
		}
	}
	if task, _ := readBead(context.Background(), wishBeads(), wishSlack+commonstest.WishTask); task.Face == nil || task.Face.Kind != "work" {
		t.Errorf("task face = %+v, want work", task.Face)
	}

	commons := &beadsapi.BeadDetail{ID: "gd-cw", Type: "task", Labels: []string{"project:gardenio", "thread"}, CreatedBy: "ada@example.com",
		Fields: map[string]string{"channel_id": "gd-chGeneral", "text": "a wish", "author": "oidc:ada@example.com"}}
	if c := wishFace(commons); c.Asker != "oidc:ada@example.com" || c.Where != "commons" || c.Ask != "a wish" {
		t.Errorf("Commons wish = %+v, want ada's ask in the Commons", c)
	}
	unread := wishFace(commons)
	if countWish(unread, nil); unread.Counted {
		t.Errorf("kin unread = %+v, want no counts", unread)
	}
}

// The wish face under node (web/wishface.js, installed into workbench.js FACES): the ask leads, one counts line links the Conversation and the
// Children sections, and who asked, where and when is one row. A reply points back up by Part of wish, the head's short
// title opening its panel (gd-UXDp9GUeMB), so its message face's In thread row is drawn only when the head was not read.
func TestWishFaceUnderNode(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	beads := wishBeads()
	beads.beads[wishSlack].ShortTitle = "bring the thread into the bead view"
	docs := map[string]*beadDoc{}
	for _, id := range []string{wishSlack, wishSlack + commonstest.WishReply1} {
		d, err := readBead(context.Background(), beads, id)
		if err != nil {
			t.Fatal(err)
		}
		docs[id] = d
	}
	raw, _ := json.Marshal(docs)
	script := `
var V = require(process.argv[1]), assert = require("assert"), D = JSON.parse(process.argv[2]), head = "gd-wishSlack";
require(process.argv[3]).install(V);
var t0 = D[head].face.wish.at, ctx = function (id) { return { id: id, now: t0 + 3600, base: "/v", has: function () { return false; } }; };
function text(parts) { return (parts || []).map(function (p) { return typeof p === "string" ? p : p.t || p.sm || p.b || ""; }).join(""); }
function row(v, k) { var r = (v.rows || []).filter(function (r) { return r[0] === k; })[0]; return r ? r[1] : null; }
var w = V.faceView(D[head], ctx(head));
assert.strictEqual(w.h, "Wish");
assert.strictEqual(w.said, true, "the panel's kind word says it, so the heading is for a screen reader only");
assert.strictEqual(V.kindWord(D[head], "task"), "wish");
assert.ok(/^the workbench and bead viewer in gardenio lands/.test(w.lead), "the ask leads: " + w.lead);
assert.strictEqual(text(w.sum), "5 replies · 1 agent at work · work: 1 open", "one counts line");
assert.deepStrictEqual(w.sum.filter(function (p) { return p.sec; }).map(function (p) { return p.sec; }), ["conversation", "children"], "each linked to its section");
assert.strictEqual(text(row(w, "Asked by")), "matt in Slack · 1h ago");
assert.strictEqual(w.rows.length, 1, "no other row repeats the thread");

var m = V.faceView(D[head + "Rp1"], ctx(head + "Rp1"));
assert.strictEqual(D[head + "Rp1"].kin.head.short_title, "bring the thread into the bead view", "the head's panel, by its short title, is Part of wish");
assert.strictEqual(row(m, "In thread"), null, "which the face does not repeat");
assert.deepStrictEqual(row(V.faceView(Object.assign({}, D[head + "Rp1"], { kin: null }), ctx(head + "Rp1")), "In thread"), [{ bead: head, t: "bring the thread into the bead view" }], "a head the panel could not read: the face's row");
`
	js, _ := filepath.Abs(filepath.Join("web", "workbench.js"))
	wish, _ := filepath.Abs(filepath.Join("web", "wishface.js"))
	out, err := exec.Command(node, "-e", script, js, string(raw), wish).CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
}

// mentionBeads is the bead view's reader with the daemon's reverse mentions: refs, by the bead they name.
type mentionBeads struct {
	*fakeBeads
	refs map[string][]beadsapi.BeadReferenceSummary
}

func (m mentionBeads) GetMentionedBy(_ context.Context, id string, _ int) ([]beadsapi.BeadReferenceSummary, error) {
	return m.refs[id], nil
}

func (m mentionBeads) BatchGetBeads(ctx context.Context, ids []string) (map[string]*beadsapi.BeadDetail, error) {
	out := map[string]*beadsapi.BeadDetail{}
	for _, id := range ids {
		if b, err := m.GetBead(ctx, id); err == nil {
			out[id] = b
		}
	}
	return out, nil
}

// The messages that name a bead without being of its thread join its conversation via mention (gd-UXDp9GUeMB, the
// viewer's bead chat): the fixture task, named by a squawk of another thread and by one of its own, carries the other
// once and its own not twice; the wish's reply count is the thread's; a bead in no thread that a message names has kin
// of that message alone (no_thread), and the panel draws it "mentions this".
func TestBeadViewMentionedBy(t *testing.T) {
	node, nodeErr := exec.LookPath("node")
	beads := wishBeads()
	task, sq1, elsewhere := wishSlack+commonstest.WishTask, wishSlack+commonstest.WishSquawk1, "gd-elsewhere1"
	beads.beads["gd-lone"] = &beadsapi.BeadDetail{ID: "gd-lone", Type: "task", Status: "open"}
	beads.beads[elsewhere] = &beadsapi.BeadDetail{ID: elsewhere, Type: "message", Labels: []string{"squawk", "from:other-crew"},
		CreatedBy: "other-crew", CreatedAt: time.Unix(1_790_000_000, 0), Fields: map[string]string{"text": "blocked on " + task, "thread_id": "gd-otherHead"}}
	msg := func(id string) beadsapi.BeadReferenceSummary {
		return beadsapi.BeadReferenceSummary{ID: id, Type: "message"}
	}
	src := mentionBeads{beads, map[string][]beadsapi.BeadReferenceSummary{task: {msg(sq1), msg(elsewhere)}, wishSlack: {msg(elsewhere)}, "gd-lone": {msg(elsewhere)}}}

	_, doc := getBead(t, beadMux(src), task)
	k, _ := doc["kin"].(map[string]any)
	var mentioned []string
	n := 0
	for _, l := range k["conversation"].([]any) {
		m := l.(map[string]any)
		if m["id"] == sq1 {
			n++
		}
		if m["via"] == "mention" {
			mentioned = append(mentioned, m["id"].(string))
		}
	}
	if !slices.Equal(mentioned, []string{elsewhere}) || n != 1 || k["head"] == nil || k["no_thread"] != nil {
		t.Errorf("task's kin = %v, want %s via mention, %s once, its head", k, elsewhere, sq1)
	}
	if d, _ := readBead(context.Background(), src, wishSlack); d.Face.Wish.Replies != 5 {
		t.Errorf("wish replies = %d, want the thread's 5, the mention aside", d.Face.Wish.Replies)
	}
	lone, err := readBead(context.Background(), src, "gd-lone")
	if err != nil || lone.Kin == nil || !lone.Kin.NoThread || len(lone.Kin.Conversation) != 1 || lone.Kin.Conversation[0].ID != elsewhere {
		t.Fatalf("lone kin = %+v (%v), want no thread and the one mention", lone.Kin, err)
	}
	if nodeErr != nil {
		t.Skip("node not installed")
	}
	raw, _ := json.Marshal(lone)
	script := `
var V = require(process.argv[1]), assert = require("assert"), D = JSON.parse(process.argv[2]);
assert.deepStrictEqual(V.talk(D).map(function (l) { return [l.k, l.who, l.via]; }), [["msg", "other-crew", "mentions this"]]);
`
	js, _ := filepath.Abs(filepath.Join("web", "beadtalk.js"))
	if out, err := exec.Command(node, "-e", script, js, string(raw)).CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
}

// closureBeads is the bead view's reader with the batch reads of a thread's parent-child closure.
type closureBeads struct{ mentionBeads }

func (c closureBeads) BatchGetDependencies(_ context.Context, ids []string, _ string) (map[string][]beadsapi.Dependency, error) {
	out := map[string][]beadsapi.Dependency{}
	for _, d := range c.deps {
		if slices.Contains(ids, d.DependsOnID) {
			out[d.DependsOnID] = append(out[d.DependsOnID], d)
		}
	}
	return out, nil
}

// The kin gain what the viewer's bead page reads beside thread_id (gd-GTqYxAem2S): the head's parent-child descendants
// that carry no thread_id (a child of the task and its own child, threads.go threadMembers), but not one bound to
// another thread; and an agent's own mail and squawks out of its thread by from:<agent> (agentAuthoredMessagesQuery),
// not one another writer labelled as its.
func TestBeadViewKinClosureAndAuthored(t *testing.T) {
	beads := wishBeads()
	task, agentID := wishSlack+commonstest.WishTask, wishSlack+commonstest.WishAgent
	agent := beads.beads[agentID].Title
	at := time.Unix(1_790_000_000, 0)
	for _, b := range []*beadsapi.BeadDetail{
		{ID: "gd-half", Type: "task", Status: "open", CreatedAt: at},
		{ID: "gd-halfSub", Type: "bug", Status: "open", CreatedAt: at.Add(time.Minute)},
		{ID: "gd-bound", Type: "task", Status: "open", CreatedAt: at, Fields: map[string]string{"thread_id": "gd-otherHead"}},
		{ID: "gd-agMail", Type: "mail", Title: "need a review", Labels: []string{"from:" + agent}, CreatedBy: agent, CreatedAt: at},
		{ID: "gd-agElse", Type: "message", Labels: []string{"squawk", "from:" + agent}, CreatedBy: "other-crew", CreatedAt: at,
			Fields: map[string]string{"text": "in its name"}},
	} {
		beads.beads[b.ID] = b
	}
	beads.deps = append(beads.deps, beadsapi.Dependency{BeadID: "gd-half", DependsOnID: task, Type: "parent-child"},
		beadsapi.Dependency{BeadID: "gd-halfSub", DependsOnID: "gd-half", Type: "parent-child"},
		beadsapi.Dependency{BeadID: "gd-bound", DependsOnID: wishSlack, Type: "parent-child"})
	src := closureBeads{mentionBeads{beads, nil}}

	d, err := readBead(context.Background(), src, wishSlack)
	if err != nil || d.Kin == nil {
		t.Fatalf("head = %+v (%v)", d, err)
	}
	var work []string
	for _, w := range d.Kin.Work {
		work = append(work, w.ID)
	}
	if !slices.Equal(work, []string{task, "gd-half", "gd-halfSub"}) {
		t.Errorf("head's work = %v, want %s and its two unbound descendants, not gd-bound", work, task)
	}

	d, err = readBead(context.Background(), src, agentID)
	if err != nil || d.Kin == nil {
		t.Fatalf("agent = %+v (%v)", d, err)
	}
	vias := map[string]string{}
	for _, l := range d.Kin.Conversation {
		vias[l.ID] = l.Via
	}
	if vias["gd-agMail"] != "mail" || vias["gd-agElse"] != "" || vias[wishSlack+commonstest.WishSquawk1] != "squawk" {
		t.Errorf("agent's conversation vias = %v, want its mail and thread squawks, not gd-agElse", vias)
	}
}
