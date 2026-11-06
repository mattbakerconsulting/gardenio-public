package gardenhub

import (
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

// TestMailboxInHandEscalations: the Mailbox lists a desk or operator
// escalation marked in hand, which garden.js counts() leaves out of the
// "needs you" count and the pips; an owner one is counted as before
// (gd-bmOiteQi36).
func TestMailboxInHandEscalations(t *testing.T) {
	for _, c := range []struct {
		name   string
		fields map[string]string
		inHand bool
	}{
		{"a merge is the desk's", map[string]string{"operation_type": "gitlab_mr_merge"}, true},
		{"infra is the operators'", map[string]string{"operation_type": "helm_chart_change"}, true},
		{"filed for the owner", map[string]string{"operation_type": "gitlab_mr_merge", "audience": "owner"}, false},
		{"secret is the owner's whatever it says", map[string]string{"operation_type": "k8s_rbac", "audience": "operator", "sensitivity": "secret"}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			b := fb("gd-mbEsc01", "escalation", "open", "", 1)
			for k, v := range c.fields {
				b.Fields[k] = v
			}
			it := itemFromBead(b, SecDecision)
			if it.InHand != c.inHand {
				t.Fatalf("InHand = %v, want %v", it.InHand, c.inHand)
			}
			html := renderItem(it, "")
			if got := strings.Contains(html, `class="item inhand"`) && strings.Contains(html, ">in hand</span>"); got != c.inHand {
				t.Errorf("rendered in-hand mark = %v, want %v:\n%s", got, c.inHand, html)
			}
		})
	}
	// A decision is never in hand.
	if it := itemFromBead(fb("gd-mbDec01", "decision", "open", "", 1), SecDecision); it.InHand {
		t.Error("a decision was marked in hand")
	}
}

// TestMailboxSaysWhom: a Mailbox decision says whom it is asked of, by the
// operator's display name, never a raw id or address, and the page turns it
// into "asked of you" on the viewer's own; an owner escalation says it is for
// the owner. A prompt that only repeats the title is not printed again
// (gd-e8EAWMUiNw).
func TestMailboxSaysWhom(t *testing.T) {
	for _, c := range []struct{ display, want string }{
		{"Flo", ">asked of Flo</p>"},
		{"", ">asked of its operator</p>"},
		{"gardenio", ">asked of its operator</p>"},
		{"flo@example.com", ">asked of its operator</p>"},
	} {
		b := fb("gd-mbDec02", "decision", "open", "", 1)
		b.OperatedBy, b.OperatedByDisplay = "oidc:flo@example.com", c.display
		if html := renderItem(itemFromBead(b, SecDecision), ""); !strings.Contains(html, `<p class="who" data-who>`) || !strings.Contains(html, c.want) {
			t.Errorf("display %q: want %s in\n%s", c.display, c.want, html)
		}
	}
	b := fb("gd-mbDec03", "decision", "open", "", 1)
	b.OperatedBy = ""
	if html := renderItem(itemFromBead(b, SecDecision), ""); strings.Contains(html, `class="who"`) {
		t.Errorf("a decision with no operator named one:\n%s", html)
	}

	b = fb("gd-mbDec04", "decision", "open", "", 1)
	b.Title, b.Fields["prompt"] = "Ship it?", " Ship it? "
	if it := itemFromBead(b, SecDecision); it.Ask != "" {
		t.Errorf("a prompt equal to the title was kept: %q", it.Ask)
	}
	b.Fields["prompt"] = "Ship it? CI is green."
	if it := itemFromBead(b, SecDecision); it.Ask != "Ship it? CI is green." {
		t.Errorf("a prompt that says more was dropped: %q", it.Ask)
	}

	e := fb("gd-mbEsc02", "escalation", "open", "", 1)
	e.Fields["audience"] = "owner"
	if html := renderItem(itemFromBead(e, SecDecision), ""); !strings.Contains(html, `<p class="who">for the owner to approve</p>`) {
		t.Errorf("an owner escalation does not say whom it is for:\n%s", html)
	}
	e.Fields["operation_type"], e.Fields["audience"] = "gitlab_mr_merge", ""
	if html := renderItem(itemFromBead(e, SecDecision), ""); strings.Contains(html, `class="who"`) {
		t.Errorf("an in-hand escalation says it is for the owner:\n%s", html)
	}
}

// TestTagSaysAskedOfYou: garden.js tag marks the viewer's own decision and
// turns its "asked of <operator>" into "asked of you"; another's keeps its name.
func TestTagSaysAskedOfYou(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	src, err := os.ReadFile("web/garden.js")
	if err != nil {
		t.Fatal(err)
	}
	fn := regexp.MustCompile(`\nfunction tag\(li\)\{[^\n]*\n`).Find(src)
	if fn == nil {
		t.Fatal("tag not found in garden.js")
	}
	script := `
var assert = require("assert"), me = "oidc:me@example.com";
function li(op) {
  var cls = {}, who = { textContent: "asked of Flo" };
  return { dataset: { op: op }, who: who, classList: { toggle: function (c, on) { cls[c] = on; }, contains: function (c) { return !!cls[c]; } },
    querySelector: function (q) { return q === ".who[data-who]" ? who : null; } };
}
` + string(fn) + `
var mine = li("oidc:me@example.com"), theirs = li("oidc:flo@example.com");
tag(mine); tag(theirs);
assert.strictEqual(mine.who.textContent, "asked of you");
assert.strictEqual(theirs.who.textContent, "asked of Flo");
`
	if out, err := exec.Command(node, "-e", script).CombinedOutput(); err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
}

// TestReadSecSkipsInHand: garden.js readSec, which feeds the Mailbox beacons, the
// chore tour's asks and the field, leaves an in-hand Mailbox item out, as
// counts() does (#756 send-back). It runs the real function under node over a
// stub list.
func TestReadSecSkipsInHand(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	src, err := os.ReadFile("web/garden.js")
	if err != nil {
		t.Fatal(err)
	}
	fn := regexp.MustCompile(`(?s)\nfunction readSec\(sec\)\{.*?\n\}\n`).Find(src)
	if fn == nil {
		t.Fatal("readSec not found in garden.js")
	}
	script := `
var assert = require("assert"), STALE_S = 86400, scope = "all";
function li(id, cls) {
  var c = cls.split(" ");
  return { id: "i-" + id, dataset: { u: "0" }, classList: { contains: function (x) { return c.indexOf(x) >= 0; } },
    querySelector: function () { return { getAttribute: function () { return "/b/" + id; }, textContent: id }; } };
}
var L = { decision: { children: [li("d1", "item"), li("e1", "item inhand"), li("e2", "item mine"), li("e3", "item mine inhand")] } };
` + string(fn) + `
assert.deepStrictEqual(readSec("decision").map(function (x) { return x.id; }), ["d1", "e2"]);
scope = "mine";
assert.deepStrictEqual(readSec("decision").map(function (x) { return x.id; }), ["e2"]);
`
	if out, err := exec.Command(node, "-e", script).CombinedOutput(); err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
}
