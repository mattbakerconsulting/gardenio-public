package gardenhub

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestMailboxHeldEscalation: a non-floor escalation an agent lane holds (a desk-held merge) says the lane has it and
// offers only a secondary "Approve anyway" that names the holder, no primary Approve; a floor item, one the owner
// holds and an unstamped one keep Approve (gd-Mq0uDXHswK, the F4 owner approval of gd-ayt3yO8V2k).
func TestMailboxHeldEscalation(t *testing.T) {
	const primary = `<button type="button" class="btn small" data-act="approve"`
	merge := map[string]string{"operation_type": "gitlab_mr_merge", "audience": "desk", "held_by": "coordinator:merge-desk",
		"approvers": `["coordinator:merge-desk","coordinator:laptop","operator"]`}
	for _, c := range []struct {
		name   string
		change map[string]string
		held   string
	}{
		{"a desk-held merge", nil, "the merge desk"},
		{"a laptop-held ops ask", map[string]string{"operation_type": "minio_bucket_quota", "audience": "operator", "held_by": "coordinator:laptop"}, "coordinator laptop"},
		{"a floor item the laptop holds", map[string]string{"operation_type": "credential_rotation", "held_by": "coordinator:laptop"}, ""},
		{"one the owner holds", map[string]string{"held_by": "operator", "audience": "owner"}, ""},
		{"an unstamped merge", map[string]string{"held_by": ""}, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			b := fb("gd-mbHeld01", "escalation", "open", "", 1)
			for k, v := range merge {
				b.Fields[k] = v
			}
			for k, v := range c.change {
				b.Fields[k] = v
			}
			it := itemFromBead(b, SecDecision)
			if it.Held != c.held {
				t.Fatalf("Held = %q, want %q", it.Held, c.held)
			}
			html := renderItem(it, "")
			if c.held == "" {
				if !strings.Contains(html, primary) || strings.Contains(html, "Approve anyway") {
					t.Errorf("want the primary Approve:\n%s", html)
				}
				return
			}
			if strings.Contains(html, primary) {
				t.Errorf("a held escalation offers a primary Approve:\n%s", html)
			}
			for _, want := range []string{
				`<p class="who held">` + strings.ToUpper(c.held[:1]) + c.held[1:] + ` has this; nothing waits on you.</p>`,
				`class="btn small ghost" data-act="approve" data-id="gd-mbHeld01" data-arm="Tap again to approve ahead of ` + c.held + `">Approve anyway</button>`,
			} {
				if !strings.Contains(html, want) {
					t.Errorf("missing %s in\n%s", want, html)
				}
			}
			if strings.Contains(html, "for the owner to approve") {
				t.Errorf("a held escalation says it is for the owner:\n%s", html)
			}
		})
	}
}

// TestAnswerBoxHeldUnderNode drives web/answer.js over a minimal DOM: box({held}) leads with the holder's line and offers
// a ghost "Approve anyway" whose confirm names the holder, and no primary Approve, while a box without held keeps Approve;
// nextAsk, where the bead view moves after an answer, skips in-hand Mailbox rows and wraps to the top (gd-Mq0uDXHswK).
func TestAnswerBoxHeldUnderNode(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	script := `
var assert = require("assert");
function El(t) { this.tag = t; this.kids = []; this.attrs = {}; this.dataset = {}; this.className = ""; this.textContent = ""; this.parentNode = null; }
El.prototype.appendChild = function (c) { c.parentNode = this; this.kids.push(c); return c; };
El.prototype.insertBefore = function (c, ref) { c.parentNode = this; var i = this.kids.indexOf(ref); this.kids.splice(i < 0 ? this.kids.length : i, 0, c); return c; };
El.prototype.setAttribute = function (k, v) { this.attrs[k] = v; };
global.document = { createElement: function (t) { return new El(t); } };
function all(e, f, out) { out = out || []; if (f(e)) out.push(e); (e.kids || []).forEach(function (k) { all(k, f, out); }); return out; }
var A = require(process.argv[1]);
var held = A.box({ kind: "escalation", id: "gd-m", title: "Merge PR #1", held: "the merge desk" });
assert.strictEqual(held.kids[0].className, "who held");
assert.strictEqual(held.kids[0].textContent, "The merge desk has this; nothing waits on you.");
var hb = all(held, function (e) { return e.tag === "button" && e.dataset.act === "approve"; });
assert.strictEqual(hb.length, 1);
assert.deepStrictEqual([hb[0].textContent, hb[0].className, hb[0].dataset.arm], ["Approve anyway", "btn small ghost", "Tap again to approve ahead of the merge desk"]);
var own = A.box({ kind: "escalation", id: "gd-o", title: "Rotate" });
var ob = all(own, function (e) { return e.tag === "button" && e.dataset.act === "approve"; });
assert.deepStrictEqual([ob[0].textContent, ob[0].className], ["Approve", "btn small"]);
assert.strictEqual(all(own, function (e) { return /\bheld\b/.test(e.className); }).length, 0);

function row(id, inhand) { return { id: id, cls: inhand ? ["item", "inhand"] : ["item"], classList: { contains: function (c) { return this.o.cls.indexOf(c) >= 0; } } }; }
var mail = { kids: [] };
["a", "b:in", "c:in", "d"].forEach(function (s) { var p = s.split(":"), r = row(p[0], !!p[1]); r.classList.o = r; r.parentNode = mail; mail.kids.push(r); });
mail.kids.forEach(function (r, i) { r.nextElementSibling = mail.kids[i + 1] || null; });
mail.firstElementChild = mail.kids[0];
var next = function (after) { var n = A.nextAsk(mail, after); return n ? n.id : null; };
assert.strictEqual(next(mail.kids[1]), "d", "past the in-hand rows");
assert.strictEqual(next(null), "a", "none after: the first from the top");
mail.kids[0].cls.push("inhand"); mail.kids[3].cls.push("inhand");
assert.strictEqual(next(mail.kids[1]), null, "only in-hand rows left: none");
console.log("ok");
`
	js, _ := filepath.Abs(filepath.Join("web", "answer.js"))
	out, err := exec.Command(node, "-e", script, js).CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "ok" {
		t.Fatalf("node: %v\n%s", err, out)
	}
}
