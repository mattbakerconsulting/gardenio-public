package gardenhub

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/decisionview"
)

// Decision evidence on /garden (gd-Pi7C83262N): the screenshots on the bead a
// decision is about show under its context on the Mailbox row and in the bead
// panel.

// evidenceBeads is a decision asked by a task whose comment carries a viewer
// image link, and a picture from elsewhere the proxy does not serve.
func evidenceBeads() *fakeBeads {
	return &fakeBeads{
		beads: map[string]*beadsapi.BeadDetail{
			"gd-EvDecide1": {ID: "gd-EvDecide1", Type: "decision", Status: "open", Title: "Ship the Porch?",
				Fields: map[string]string{"prompt": "Ship it?", "context": "Shots on the task.", "requesting_task": "gd-EvTask01"}},
			"gd-EvTask01": {ID: "gd-EvTask01", Type: "task", Status: "in_progress", Title: "Grade the Porch"},
		},
		comments: map[string][]beadsapi.Comment{
			"gd-EvTask01": {{Text: "phone: https://v.example/image/ui/porch-phone.png and https://other.example/x.png"}},
		},
	}
}

// evidenceSource is the hub's fake source with the comment reads evidence needs.
type evidenceSource struct {
	*fakeSource
	b *fakeBeads
}

func (s evidenceSource) GetBead(ctx context.Context, id string) (*beadsapi.BeadDetail, error) {
	return s.b.GetBead(ctx, id)
}

func (s evidenceSource) GetComments(ctx context.Context, id string) ([]beadsapi.Comment, error) {
	return s.b.GetComments(ctx, id)
}

func TestMailboxRowShowsEvidence(t *testing.T) {
	fb := evidenceBeads()
	f := newFake()
	f.set(nil, []*beadsapi.BeadDetail{fb.beads["gd-EvDecide1"]})
	h := New(evidenceSource{f, fb}, "/v", nil)
	if err := h.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	row := h.frags["gd-EvDecide1"]
	ctxAt, evAt, actsAt := strings.Index(row, "Shots on the task."), strings.Index(row, `class="dec-ev"`), strings.Index(row, `class="acts"`)
	if ctxAt < 0 || evAt < ctxAt || actsAt < evAt {
		t.Fatalf("the strip sits under the context and above the answers:\n%s", row)
	}
	for _, want := range []string{`src="/v/image/ui/porch-phone.png"`, `from <a href="/v/garden?bead=gd-EvTask01">Grade the Porch</a>`, `id="dec-ev-mb-gd-EvDecide1-0"`} {
		if !strings.Contains(row, want) {
			t.Errorf("row lacks %q:\n%s", want, row)
		}
	}
	if strings.Contains(row, "other.example") {
		t.Errorf("a picture the proxy does not serve never loads from its host:\n%s", row)
	}

	// A source without comment reads (the older fakes) draws the row without a strip.
	h = New(f, "/v", nil)
	if err := h.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if row := h.frags["gd-EvDecide1"]; row == "" || strings.Contains(row, "dec-ev") {
		t.Errorf("no evidence reads, no strip, the row still drawn:\n%s", row)
	}
}

func TestBeadPanelFaceShowsEvidence(t *testing.T) {
	fb := evidenceBeads()
	cache := decisionview.NewEvidenceCache(0)
	doc, err := readBeadN(context.Background(), fb, "gd-EvDecide1", beadCommentsMax, cache)
	if err != nil {
		t.Fatal(err)
	}
	ev := doc.Face.Decision.Evidence
	if len(ev) != 2 || ev[0].URL != "https://v.example/image/ui/porch-phone.png" || ev[0].From != "gd-EvTask01" || ev[0].FromTitle != "Grade the Porch" {
		t.Errorf("the face carries the task's pictures (the page proxies or drops each, web/workbench.js evidenceStrip): %+v", ev)
	}
	fb.failCmt = true
	if doc, err = readBead(context.Background(), fb, "gd-EvDecide1"); err != nil || doc.Face.Decision.Evidence != nil {
		t.Errorf("failed evidence reads: the decision renders without it: %v %+v", err, doc)
	}
}

// TestPanelEvidenceStripUnderNode drives web/answer.js evidence, the bead panel's strip, over a minimal DOM: the pictures
// the proxy serves, each with its alt, its bead's link and a popover that it opens and its Close hides; none, no strip.
func TestPanelEvidenceStripUnderNode(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	script := `
var assert = require("assert");
function El(t) { this.tag = t; this.kids = []; this.attrs = {}; this.className = ""; this.textContent = ""; }
El.prototype.appendChild = function (c) { this.kids.push(c); return c; };
El.prototype.setAttribute = function (k, v) { this.attrs[k] = v; };
global.document = { createElement: function (t) { return new El(t); } };
function all(e, f, out) { out = out || []; if (f(e)) out.push(e); (e.kids || []).forEach(function (k) { all(k, f, out); }); return out; }
var A = require(process.argv[1]);
var src = function (u) { return u.indexOf("/image/") >= 0 ? "/v" + u.slice(u.indexOf("/image/")) : ""; };
var href = function (id) { return "/v/garden?bead=" + id; };
var s = A.evidence([{ url: "https://h/image/ui/a.png", alt: "phone", from: "gd-t", from_title: "Grade it" }, { url: "https://tracker/x.png", alt: "pixel" }], src, href);
assert.strictEqual(s.className, "dec-ev");
var thumbs = all(s, function (e) { return e.className === "dec-ev-thumb"; }), boxes = all(s, function (e) { return e.className === "dec-ev-box"; });
assert.strictEqual(thumbs.length, 1, "the picture the proxy does not serve is left out");
assert.strictEqual(thumbs[0].attrs.popovertarget, boxes[0].id, "the thumbnail opens its popover");
assert.strictEqual(boxes[0].attrs.popover, "");
var close = all(boxes[0], function (e) { return e.className === "dec-ev-close"; })[0];
assert.deepStrictEqual([close.attrs.popovertarget, close.attrs.popovertargetaction], [boxes[0].id, "hide"]);
var imgs = all(s, function (e) { return e.tag === "img"; });
assert.ok(imgs.length === 2 && imgs.every(function (i) { return i.src === "/v/image/ui/a.png" && i.alt === "phone" && i.loading === "lazy"; }), "thumbnail and lightbox load through the proxy, lazily");
var from = all(s, function (e) { return e.tag === "a" && e.className === "bl"; })[0];
assert.deepStrictEqual([from.href, from.textContent], ["/v/garden?bead=gd-t", "Grade it"]);
assert.strictEqual(all(s, function (e) { return e.className === "dec-ev-n"; })[0].textContent, "1");
assert.strictEqual(A.evidence([{ url: "https://tracker/x.png" }], src, href), null, "nothing the proxy serves: no strip");
console.log("ok");
`
	js, _ := filepath.Abs(filepath.Join("web", "answer.js"))
	out, err := exec.Command(node, "-e", script, js).CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "ok" {
		t.Fatalf("node: %v\n%s", err, out)
	}
}
