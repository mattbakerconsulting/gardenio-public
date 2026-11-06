package main

import (
	"strings"
	"testing"
)

func TestLinkifyHTML_BareID(t *testing.T) {
	got := string(LinkifyHTML("see gd-abc123 for details", ""))
	want := `see <a href="/gd-abc123">gd-abc123</a> for details`
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestLinkifyHTML_BasePath(t *testing.T) {
	got := string(LinkifyHTML("gd-aaa", "/beads"))
	want := `<a href="/beads/gd-aaa">gd-aaa</a>`
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestLinkifyHTML_MultipleIDs(t *testing.T) {
	got := string(LinkifyHTML("blocked by gd-aaa and gd-bbb; fixes gd-ccc", ""))
	for _, want := range []string{
		`<a href="/gd-aaa">gd-aaa</a>`,
		`<a href="/gd-bbb">gd-bbb</a>`,
		`<a href="/gd-ccc">gd-ccc</a>`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("result %q missing %q", got, want)
		}
	}
}

func TestLinkifyHTML_SkipsExistingAnchor(t *testing.T) {
	input := `already <a href="https://other/gd-aaa">gd-aaa</a> linked`
	got := string(LinkifyHTML(input, ""))
	if got != input {
		t.Errorf("should not modify IDs inside existing anchors: got %q", got)
	}
	// Must not produce a nested <a> tag.
	if strings.Contains(got, `<a href="/gd-aaa"><a`) || strings.Count(got, "<a ") != 1 {
		t.Errorf("expected a single anchor, got %q", got)
	}
}

func TestLinkifyHTML_MixedAnchorAndBare(t *testing.T) {
	input := `<a href="https://ex/gd-aaa">gd-aaa</a> then gd-bbb later`
	got := string(LinkifyHTML(input, ""))
	wantLinked := `<a href="/gd-bbb">gd-bbb</a>`
	if !strings.Contains(got, wantLinked) {
		t.Errorf("expected bare id to be linked: %q", got)
	}
	if !strings.Contains(got, `<a href="https://ex/gd-aaa">gd-aaa</a>`) {
		t.Errorf("existing anchor should be preserved: %q", got)
	}
}

func TestLinkifyHTML_InsideCodeBlock(t *testing.T) {
	// Per product requirement: linkify everywhere, including inside code blocks.
	input := `<pre><code>see gd-aaa in the log</code></pre>`
	got := string(LinkifyHTML(input, ""))
	if !strings.Contains(got, `<a href="/gd-aaa">gd-aaa</a>`) {
		t.Errorf("expected id inside code block to be linkified: %q", got)
	}
}

func TestLinkifyHTML_NoID(t *testing.T) {
	input := `<p>Nothing to see here.</p>`
	got := string(LinkifyHTML(input, ""))
	if got != input {
		t.Errorf("expected no change, got %q", got)
	}
}

func TestLinkifyHTML_AdjacentPunctuation(t *testing.T) {
	got := string(LinkifyHTML("see (gd-aaa, gd-bbb).", ""))
	for _, want := range []string{
		`(<a href="/gd-aaa">gd-aaa</a>`,
		`<a href="/gd-bbb">gd-bbb</a>).`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("result %q missing %q", got, want)
		}
	}
}

func TestLinkifyHTML_WordBoundary(t *testing.T) {
	// "prefix-gd-aaa" should still match gd-aaa per \b (hyphen is non-word).
	// But "xgd-aaa" must NOT be linkified (x is a word char).
	if got := string(LinkifyHTML("xgd-aaa", "")); strings.Contains(got, "<a ") {
		t.Errorf("xgd-aaa should not be treated as a bead id: %q", got)
	}
	if got := string(LinkifyHTML("prefix-gd-aaa", "")); !strings.Contains(got, `<a href="/gd-aaa">gd-aaa</a>`) {
		t.Errorf("prefix-gd-aaa should linkify the gd-aaa suffix: %q", got)
	}
}

func TestLinkifyText_EscapesBeforeLinkify(t *testing.T) {
	got := string(LinkifyText(`<b>see gd-aaa</b>`, ""))
	// The <b> must be escaped, and the id must still be linkified.
	if !strings.Contains(got, `&lt;b&gt;see `) {
		t.Errorf("expected HTML to be escaped, got %q", got)
	}
	if !strings.Contains(got, `<a href="/gd-aaa">gd-aaa</a>`) {
		t.Errorf("expected id to be linkified, got %q", got)
	}
	// And a raw <b> must not leak through.
	if strings.Contains(got, "<b>") {
		t.Errorf("raw <b> leaked into output: %q", got)
	}
}

func TestLinkifyHTML_AllowsAlphanumericIDs(t *testing.T) {
	got := string(LinkifyHTML("gd-AbC123xyZ ok", ""))
	if !strings.Contains(got, `<a href="/gd-AbC123xyZ">gd-AbC123xyZ</a>`) {
		t.Errorf("expected alphanumeric id to linkify, got %q", got)
	}
}

func TestLinkifyDescriptionHTML_BareID(t *testing.T) {
	got := string(LinkifyDescriptionHTML("see gd-aaa here", "", nil))
	want := `see <a href="/gd-aaa" id="mention-gd-aaa-1" data-bead-id="gd-aaa" class="mention">gd-aaa</a> here`
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestLinkifyDescriptionHTML_BasePath(t *testing.T) {
	got := string(LinkifyDescriptionHTML("gd-aaa", "/beads", nil))
	want := `<a href="/beads/gd-aaa" id="mention-gd-aaa-1" data-bead-id="gd-aaa" class="mention">gd-aaa</a>`
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestLinkifyDescriptionHTML_RunningCounter(t *testing.T) {
	got := string(LinkifyDescriptionHTML("first gd-aaa, then gd-bbb, then gd-aaa again", "", nil))
	for _, want := range []string{
		`id="mention-gd-aaa-1"`,
		`id="mention-gd-bbb-1"`,
		`id="mention-gd-aaa-2"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("expected anchor %q in %q", want, got)
		}
	}
}

func TestLinkifyDescriptionHTML_TypedMention(t *testing.T) {
	typed := map[string]string{"gd-aaa": "blocks"}
	got := string(LinkifyDescriptionHTML("blocks(gd-aaa) and gd-bbb", "", typed))
	if !strings.Contains(got, `data-relationship-type="blocks"`) {
		t.Errorf("expected data-relationship-type=blocks for gd-aaa: %q", got)
	}
	// gd-bbb is untyped — must NOT carry data-relationship-type.
	if !strings.Contains(got, `id="mention-gd-bbb-1" data-bead-id="gd-bbb" class="mention">gd-bbb</a>`) {
		t.Errorf("expected untyped anchor for gd-bbb, got %q", got)
	}
	if strings.Contains(got, `data-relationship-type="blocks">gd-bbb`) {
		t.Errorf("gd-bbb should not have a relationship type: %q", got)
	}
}

func TestLinkifyDescriptionHTML_SkipsExistingAnchor(t *testing.T) {
	input := `already <a href="https://other/gd-aaa">gd-aaa</a> linked`
	got := string(LinkifyDescriptionHTML(input, "", nil))
	if got != input {
		t.Errorf("should not modify IDs inside existing anchors: got %q", got)
	}
}

func TestLinkifyDescriptionHTML_TypedVerbEscaped(t *testing.T) {
	typed := map[string]string{"gd-aaa": `bad"verb`}
	got := string(LinkifyDescriptionHTML("gd-aaa", "", typed))
	if strings.Contains(got, `bad"verb`) {
		t.Errorf("verb should be HTML-escaped, got %q", got)
	}
	if !strings.Contains(got, `data-relationship-type="bad&#34;verb"`) {
		t.Errorf("expected escaped verb attribute, got %q", got)
	}
}

// MR-ref (!N) linkification — gd-5fgSXEGBKa.

const defaultMRAnchor2948 = `<a href="https://gitlab.com/example-org/gardenio/-/merge_requests/2948" target="_blank" rel="noopener">!2948</a>`

func TestLinkifyText_MRRef_BasicIID(t *testing.T) {
	got := string(LinkifyText("see !2948 landed", ""))
	want := `see ` + defaultMRAnchor2948 + ` landed`
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestLinkifyText_MRRef_StartOfLine(t *testing.T) {
	// Start of input.
	if got := string(LinkifyText("!2948 fixes it", "")); !strings.HasPrefix(got, defaultMRAnchor2948+" ") {
		t.Errorf("expected anchor at start of input, got %q", got)
	}
	// Start of a new line (preceded by \n which matches \s).
	got := string(LinkifyText("line one\n!2948 line two", ""))
	if !strings.Contains(got, "\n"+defaultMRAnchor2948+" ") {
		t.Errorf("expected anchor after newline, got %q", got)
	}
}

func TestLinkifyText_MRRef_InsideParens(t *testing.T) {
	got := string(LinkifyText("see (!2948) for details", ""))
	want := `see (` + defaultMRAnchor2948 + `) for details`
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestLinkifyText_MRRef_InsideBrackets(t *testing.T) {
	got := string(LinkifyText("see [!2948] for details", ""))
	want := `see [` + defaultMRAnchor2948 + `] for details`
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestLinkifyHTML_MRRef_SkipsInsideAnchor(t *testing.T) {
	input := `already <a href="https://example/mr/2948">!2948</a> linked`
	got := string(LinkifyHTML(input, ""))
	if got != input {
		t.Errorf("should not modify !N inside existing anchors: got %q", got)
	}
	if strings.Count(got, "<a ") != 1 {
		t.Errorf("expected a single anchor, got %q", got)
	}
}

func TestLinkifyHTML_MRRef_InsideCodeBlock(t *testing.T) {
	// Mirrors TestLinkifyHTML_InsideCodeBlock for beads: linkify inside code
	// spans too, since skipRe only carves out HTML tag markup and existing
	// <a> spans. The chunk passed to replaceMRIIDs is the inner text
	// `!2948`, which matches via the `^` branch of the leading-char class.
	input := `<pre><code>!2948</code></pre>`
	got := string(LinkifyHTML(input, ""))
	if !strings.Contains(got, defaultMRAnchor2948) {
		t.Errorf("expected !N inside code block to be linkified: %q", got)
	}
	// No double-wrap: the <code>/<pre> tags themselves are untouched.
	if !strings.Contains(got, `<pre><code><a `) || !strings.Contains(got, `</a></code></pre>`) {
		t.Errorf("expected anchor nested inside <code>: %q", got)
	}
}

func TestLinkifyText_MRRef_NoMatchWithoutSeparator(t *testing.T) {
	// `text!2948` — no leading separator → no link.
	got := string(LinkifyText("text!2948 still text", ""))
	if strings.Contains(got, "<a ") {
		t.Errorf("expected no anchor for text!2948, got %q", got)
	}
}

func TestLinkifyText_MRRef_SingleDigitNotLinked(t *testing.T) {
	// \d{2,} requires at least two digits.
	got := string(LinkifyText("only !9 here", ""))
	if strings.Contains(got, "<a ") {
		t.Errorf("expected no anchor for !9, got %q", got)
	}
}

func TestLinkifyText_MRRef_TrailingPunctuation(t *testing.T) {
	got := string(LinkifyText("done in !2948.", ""))
	want := `done in ` + defaultMRAnchor2948 + `.`
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestLinkifyText_MRRef_WithBeadID(t *testing.T) {
	// Combined: case-study title — bead-ID and MR-ref both linkified.
	got := string(LinkifyText("Tree-hugger landed (MR !2948, ka-0000003354 closed)", "/b"))
	if !strings.Contains(got, `<a href="/b/ka-0000003354">ka-0000003354</a>`) {
		t.Errorf("expected bead-ID anchor in %q", got)
	}
	if !strings.Contains(got, defaultMRAnchor2948) {
		t.Errorf("expected MR anchor in %q", got)
	}
}

func TestLinkifyText_MRRef_EnvOverride(t *testing.T) {
	t.Setenv("BEADS_VIEWER_DEFAULT_GITLAB_PROJECT_URL", "https://gitlab.example.com/group/proj/")
	got := string(LinkifyText("see !2948", ""))
	want := `see <a href="https://gitlab.example.com/group/proj/-/merge_requests/2948" target="_blank" rel="noopener">!2948</a>`
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestLinkifyDescriptionHTML_MRRef(t *testing.T) {
	got := string(LinkifyDescriptionHTML("see !2948 and gd-aaa", "", nil))
	if !strings.Contains(got, defaultMRAnchor2948) {
		t.Errorf("expected MR anchor in description: %q", got)
	}
	if !strings.Contains(got, `id="mention-gd-aaa-1"`) {
		t.Errorf("expected bead-ID mention anchor: %q", got)
	}
}

// MR-ref linkification with the multi-repo gate — ka-0000004231. When the
// bead's project has more than one repo (e.g. monorepo) the linkifier must
// NOT pick an arbitrary repo to link `!N` to; it renders plain text instead.

func TestLinkifyTextWithOpts_MRRef_SkipMRRefs(t *testing.T) {
	got := string(LinkifyTextWithOpts("see !897 landed", "", LinkifyOpts{SkipMRRefs: true}))
	if strings.Contains(got, "<a ") {
		t.Errorf("expected no MR anchor when SkipMRRefs=true, got %q", got)
	}
	if !strings.Contains(got, "!897") {
		t.Errorf("expected bare !897 to survive as plain text, got %q", got)
	}
}

func TestLinkifyTextWithOpts_MRRef_DefaultLinkifies(t *testing.T) {
	got := string(LinkifyTextWithOpts("see !2948 landed", "", LinkifyOpts{}))
	if !strings.Contains(got, defaultMRAnchor2948) {
		t.Errorf("expected default behavior to linkify !N, got %q", got)
	}
}

func TestLinkifyHTMLWithOpts_MRRef_SkipMRRefs(t *testing.T) {
	got := string(LinkifyHTMLWithOpts("see !897 landed and gd-aaa", "", LinkifyOpts{SkipMRRefs: true}))
	if strings.Contains(got, `merge_requests/897`) {
		t.Errorf("expected no MR anchor when SkipMRRefs=true, got %q", got)
	}
	// Bead-ID linkification must still run independently of the MR gate.
	if !strings.Contains(got, `<a href="/gd-aaa">gd-aaa</a>`) {
		t.Errorf("expected bead-ID anchor to remain, got %q", got)
	}
}

func TestLinkifyHTMLWithOpts_MRRef_SkipPreservesExistingURLs(t *testing.T) {
	// Markdown-rendered fully-qualified URLs come in as <a href="...">!N</a>
	// before the linkifier runs. SkipMRRefs must not strip or alter them —
	// the anchorRe skip path already preserves them, but verify the gate
	// doesn't regress this behavior.
	input := `already <a href="https://gitlab.com/Acme/Platform/site-deployment/-/merge_requests/897">!897</a> linked`
	got := string(LinkifyHTMLWithOpts(input, "", LinkifyOpts{SkipMRRefs: true}))
	if got != input {
		t.Errorf("existing anchor should be preserved verbatim under SkipMRRefs, got %q", got)
	}
}

func TestLinkifyDescriptionHTMLWithOpts_MRRef_SkipMRRefs(t *testing.T) {
	got := string(LinkifyDescriptionHTMLWithOpts("see !897 and gd-aaa", "", nil, LinkifyOpts{SkipMRRefs: true}))
	if strings.Contains(got, `merge_requests/897`) {
		t.Errorf("expected no MR anchor when SkipMRRefs=true, got %q", got)
	}
	if !strings.Contains(got, `id="mention-gd-aaa-1"`) {
		t.Errorf("expected bead-ID mention anchor to still render, got %q", got)
	}
}

func TestLinkifyDescriptionHTMLWithOpts_MRRef_DefaultLinkifies(t *testing.T) {
	got := string(LinkifyDescriptionHTMLWithOpts("see !2948", "", nil, LinkifyOpts{}))
	if !strings.Contains(got, defaultMRAnchor2948) {
		t.Errorf("expected default opts to linkify !N, got %q", got)
	}
}

// --- JIRA issue-key linkification (ka-0000001356) ---

func TestLinkifyHTML_JiraKey_Default(t *testing.T) {
	// FSM is in the default allowlist and must render as a clickable link to
	// the canonical acme JIRA instance.
	got := string(LinkifyHTML("reported in FSM-371 yesterday", ""))
	want := `reported in <a href="https://acme.atlassian.net/browse/FSM-371" target="_blank" rel="noopener">FSM-371</a> yesterday`
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestLinkifyHTML_JiraKey_AllAllowlistedPrefixes(t *testing.T) {
	for _, key := range []string{"FSM-1", "PE-1234", "DEVOPS-566"} {
		got := string(LinkifyHTML("see "+key, ""))
		want := `<a href="https://acme.atlassian.net/browse/` + key + `" target="_blank" rel="noopener">` + key + `</a>`
		if !strings.Contains(got, want) {
			t.Errorf("key %s not linkified: got %q, want substring %q", key, got, want)
		}
	}
}

func TestLinkifyHTML_JiraKey_NoFalsePositives(t *testing.T) {
	// ALL-CAPS-number tokens that are NOT allowlisted JIRA projects must stay
	// plain text — version strings, RFCs, HTTP codes, random acronyms.
	for _, tok := range []string{"HTTP-200", "RFC-1234", "ABC-99", "UTF-8", "FOO-1"} {
		got := string(LinkifyHTML("token "+tok+" here", ""))
		if strings.Contains(got, "/browse/") {
			t.Errorf("non-JIRA token %q was linkified: %q", tok, got)
		}
	}
}

func TestLinkifyHTML_JiraKey_DoesNotMatchBeadID(t *testing.T) {
	// Lowercase gd- bead IDs must only get the bead anchor, never a JIRA one.
	got := string(LinkifyHTML("see gd-abc123", ""))
	if strings.Contains(got, "/browse/") {
		t.Errorf("bead ID should not match JIRA matcher: %q", got)
	}
	if !strings.Contains(got, `<a href="/gd-abc123">gd-abc123</a>`) {
		t.Errorf("bead ID anchor missing: %q", got)
	}
}

func TestLinkifyHTML_JiraKey_NotInsideExistingAnchor(t *testing.T) {
	// A JIRA key already inside an <a>...</a> span (e.g. a markdown link or a
	// previously-rendered anchor) must not be double-wrapped.
	in := `<a href="https://example.com/FSM-371">FSM-371 detail</a>`
	got := string(LinkifyHTML(in, ""))
	if got != in {
		t.Errorf("JIRA key inside anchor should be untouched: got %q, want %q", got, in)
	}
}

func TestLinkifyHTML_JiraKey_AllowlistOverride(t *testing.T) {
	// BEADS_VIEWER_JIRA_PROJECT_KEYS overrides the default allowlist.
	t.Setenv("BEADS_VIEWER_JIRA_PROJECT_KEYS", "PIH,INGEST")
	got := string(LinkifyHTML("PIH-9 and FSM-1", ""))
	if !strings.Contains(got, `/browse/PIH-9"`) {
		t.Errorf("PIH-9 should be linkified under override: %q", got)
	}
	// FSM is no longer in the (overridden) allowlist.
	if strings.Contains(got, "/browse/FSM-1") {
		t.Errorf("FSM-1 should NOT be linkified under PIH,INGEST override: %q", got)
	}
}

func TestLinkifyHTML_JiraKey_BaseURLOverride(t *testing.T) {
	t.Setenv("BEADS_VIEWER_JIRA_BASE_URL", "https://acme.atlassian.net/")
	got := string(LinkifyHTML("FSM-7", ""))
	if !strings.Contains(got, `href="https://acme.atlassian.net/browse/FSM-7"`) {
		t.Errorf("base URL override not applied: %q", got)
	}
}

func TestLinkifyText_JiraKey_Escaped(t *testing.T) {
	// LinkifyText escapes the surrounding text but still linkifies the key.
	got := string(LinkifyText("a <b> & FSM-12", ""))
	if !strings.Contains(got, `/browse/FSM-12"`) {
		t.Errorf("FSM-12 not linkified through LinkifyText: %q", got)
	}
	if !strings.Contains(got, "&lt;b&gt;") {
		t.Errorf("surrounding text not HTML-escaped: %q", got)
	}
}

func TestLinkifyDescriptionHTML_JiraKey(t *testing.T) {
	got := string(LinkifyDescriptionHTML("blocked by FSM-371 and gd-aaa", "", nil))
	if !strings.Contains(got, `/browse/FSM-371"`) {
		t.Errorf("FSM-371 not linkified in description path: %q", got)
	}
	if !strings.Contains(got, `id="mention-gd-aaa-1"`) {
		t.Errorf("bead-ID rich anchor missing: %q", got)
	}
}

// --- Bead anchors: #label suffix (A2, epic ka-0000006913) ---

func TestLinkifyHTML_AnchorSuffix(t *testing.T) {
	// A #label suffix becomes a slugified href fragment; the anchor TEXT
	// preserves the label as written.
	got := string(LinkifyHTML("see ka-0000002402#c1-1 there", ""))
	want := `see <a href="/ka-0000002402#c1-1">ka-0000002402#c1-1</a> there`
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestLinkifyHTML_AnchorLabelSlugified(t *testing.T) {
	// `#C1.1` (as authored) must resolve to the same #c1-1 fragment the
	// heading transformer mints, while the visible text stays `#C1.1`.
	got := string(LinkifyHTML("ka-0000002402#C1.1", ""))
	want := `<a href="/ka-0000002402#c1-1">ka-0000002402#C1.1</a>`
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestLinkifyHTML_AnchorTrailingPeriodNotConsumed(t *testing.T) {
	// A sentence-ending period after the anchor must not be swallowed into
	// the label.
	got := string(LinkifyHTML("per gd-fnhtkrbdiq#r3.", ""))
	want := `per <a href="/gd-fnhtkrbdiq#r3">gd-fnhtkrbdiq#r3</a>.`
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestLinkifyHTML_BareIDStillUnchanged(t *testing.T) {
	// Regression guard: refs without a `#` linkify exactly as before.
	got := string(LinkifyHTML("gd-abc123", ""))
	want := `<a href="/gd-abc123">gd-abc123</a>`
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestLinkifyDescriptionHTML_AnchorKeepsBareDataBeadID(t *testing.T) {
	// The rich linkifier must key data-bead-id / mention id on the BARE id so
	// highlight-scroll and typed mentions still work for an anchored ref.
	got := string(LinkifyDescriptionHTML("ref ka-0000002402#C1.1 here", "", nil))
	for _, want := range []string{
		`href="/ka-0000002402#c1-1"`,
		`data-bead-id="ka-0000002402"`,
		`id="mention-ka-0000002402-1"`,
		`>ka-0000002402#C1.1</a>`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("result %q missing %q", got, want)
		}
	}
}

func TestSplitBeadAnchor(t *testing.T) {
	cases := []struct{ in, id, label string }{
		{"gd-abc123", "gd-abc123", ""},
		{"gd-abc123#c1-1", "gd-abc123", "c1-1"},
		{"gd-abc123#C1.1", "gd-abc123", "C1.1"},
	}
	for _, c := range cases {
		id, label := splitBeadAnchor(c.in)
		if id != c.id || label != c.label {
			t.Errorf("splitBeadAnchor(%q) = (%q,%q), want (%q,%q)", c.in, id, label, c.id, c.label)
		}
	}
}
