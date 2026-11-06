package main

import (
	"strings"
	"testing"
)

// renderSameDocMD renders markdown through the anchor-aware goldmark instance
// and then the full Linkify pipeline (which ends with the L2 pass), mirroring
// the production description render path.
func renderSameDocMD(t *testing.T, src string) string {
	t.Helper()
	html := renderAnchorMD(t, src)
	return string(LinkifyHTML(html, ""))
}

func TestAutoLinkSameDocLabels_LinksBareTokenMatchingHeading(t *testing.T) {
	out := renderSameDocMD(t, strings.Join([]string{
		"### R3. cycle-budget cap",
		"",
		"As described in R3 above, the cap holds.",
	}, "\n"))

	// The heading itself keeps its id and is NOT self-linked.
	if !strings.Contains(out, `<h3 id="r3">`) {
		t.Fatalf("heading missing stable id\n%s", out)
	}
	// The bare body reference becomes a same-doc anchor.
	if !strings.Contains(out, `<a href="#r3" class="samedoc-anchor">R3</a>`) {
		t.Errorf("bare R3 body reference not auto-linked\n%s", out)
	}
}

func TestAutoLinkSameDocLabels_DottedLabel(t *testing.T) {
	out := renderSameDocMD(t, strings.Join([]string{
		"### C1.1 — TTL field",
		"",
		"See C1.1 for the TTL.",
	}, "\n"))
	if !strings.Contains(out, `<a href="#c1-1" class="samedoc-anchor">C1.1</a>`) {
		t.Errorf("dotted label C1.1 not auto-linked to #c1-1\n%s", out)
	}
}

func TestAutoLinkSameDocLabels_NoHeadingNoLink(t *testing.T) {
	// R3 appears in body but there is no matching heading anywhere in the
	// fragment: it must stay plain text (the anti-cross-doc-misfire guarantee).
	out := renderSameDocMD(t, "As described in R3 above, the cap holds.")
	if strings.Contains(out, "samedoc-anchor") {
		t.Errorf("R3 auto-linked with no matching heading in fragment\n%s", out)
	}
	if strings.Contains(out, `href="#r3"`) {
		t.Errorf("R3 got a #r3 href with no matching heading\n%s", out)
	}
}

func TestAutoLinkSameDocLabels_HeadingNotSelfLinked(t *testing.T) {
	// A label token inside its OWN heading is the target, not a reference —
	// it must never be wrapped in an anchor.
	out := renderSameDocMD(t, "### C1.1 — TTL field\n")
	if strings.Contains(out, "samedoc-anchor") {
		t.Errorf("heading label token was self-linked\n%s", out)
	}
}

func TestAutoLinkSameDocLabels_ConservativeMatcher(t *testing.T) {
	// Prose words, lowercase tokens, and bare section numbers must NOT match
	// the ^[A-Z][0-9] conservative matcher even when a same-slug heading
	// exists.
	out := renderSameDocMD(t, strings.Join([]string{
		"### 6.3 Retention",
		"### Overview",
		"",
		"Overview text mentions 6.3 and c1 and Retention.",
	}, "\n"))
	if strings.Contains(out, "samedoc-anchor") {
		t.Errorf("conservative matcher linked a non-label token\n%s", out)
	}
}

func TestAutoLinkSameDocLabels_DoesNotDoubleWrapExistingAnchors(t *testing.T) {
	// A bead ref carrying a matching-looking suffix must be linkified once by
	// the bead-id pass; the L2 pass must not re-wrap tokens inside it.
	out := renderSameDocMD(t, strings.Join([]string{
		"### C1 shape",
		"",
		"See ka-0000002402#c1 and C1 below.",
	}, "\n"))
	// The bead ref is a single anchor (bead-id pass), not nested.
	if strings.Contains(out, `<a href="/ka-0000002402#c1"><a`) ||
		strings.Contains(out, "</a></a>") {
		t.Errorf("nested/double-wrapped anchor produced\n%s", out)
	}
	// The bare C1 body token still auto-links.
	if !strings.Contains(out, `<a href="#c1" class="samedoc-anchor">C1</a>`) {
		t.Errorf("bare C1 body reference not auto-linked\n%s", out)
	}
}

func TestAutoLinkSameDocLabels_SkipsMermaidSource(t *testing.T) {
	// A label token inside mermaid diagram source must survive byte-for-byte.
	html := `<h3 id="c1">C1</h3>` +
		`<pre class="mermaid-diagram"><code>graph TD; C1--&gt;B</code></pre>` +
		`<p>C1 in prose</p>`
	out := string(LinkifyHTML(html, ""))
	if strings.Contains(out, `mermaid-diagram"><code>graph TD; <a`) {
		t.Errorf("mermaid source was linkified\n%s", out)
	}
	// Prose C1 outside the diagram still links.
	if !strings.Contains(out, `<a href="#c1" class="samedoc-anchor">C1</a>`) {
		t.Errorf("prose C1 not auto-linked\n%s", out)
	}
}

func TestAutoLinkSameDocLabels_WordBounded(t *testing.T) {
	// A token embedded in a larger word (XC1, C1X-ish) must not be linked.
	out := renderSameDocMD(t, strings.Join([]string{
		"### C1 shape",
		"",
		"The word MYC1 and C1000 differ from C1.",
	}, "\n"))
	// C1 (standalone) links; C1000 slugs to c1000 (no such heading) so no link;
	// MYC1 does not start with the matcher shape.
	if strings.Count(out, "samedoc-anchor") != 1 {
		t.Errorf("expected exactly one same-doc link (standalone C1)\n%s", out)
	}
	if !strings.Contains(out, `<a href="#c1" class="samedoc-anchor">C1</a>`) {
		t.Errorf("standalone C1 not linked\n%s", out)
	}
}

func TestAutoLinkSameDocLabels_NoHeadingsFastPath(t *testing.T) {
	// A plain fragment with no headings is returned unchanged (no id scan hits).
	in := "<p>Just some prose with R3 and C1 tokens.</p>"
	if got := autoLinkSameDocLabels(in); got != in {
		t.Errorf("fragment without headings was modified\n got: %s\nwant: %s", got, in)
	}
}

func TestReplaceSameDocLabels_Unit(t *testing.T) {
	ids := map[string]bool{"c1-1": true, "r3": true}
	cases := []struct {
		in   string
		want string
	}{
		{"see C1.1 now", `see <a href="#c1-1" class="samedoc-anchor">C1.1</a> now`},
		{"see R3 now", `see <a href="#r3" class="samedoc-anchor">R3</a> now`},
		{"see C2 now", "see C2 now"},             // no matching heading
		{"see overview now", "see overview now"}, // not a label token
		{"", ""},
	}
	for _, c := range cases {
		if got := replaceSameDocLabels(c.in, ids); got != c.want {
			t.Errorf("replaceSameDocLabels(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
