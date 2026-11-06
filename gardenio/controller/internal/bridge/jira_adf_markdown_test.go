package bridge

// Tests for the markdown → ADF renderer behind the reply path (H1, epic
// ka-0000007401 / ka-0000004233): mapped constructs produce the right ADF
// blocks, unmappable constructs report ok=false so the caller degrades to
// plain text, and the client method preserves the G4b source-link anchor.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// mdBlocks is a test helper: convert and require success.
func mdBlocks(t *testing.T, markdown string) []any {
	t.Helper()
	blocks, ok := adfBlocksFromMarkdown(markdown)
	if !ok {
		t.Fatalf("adfBlocksFromMarkdown(%q) not ok, want rendered blocks", markdown)
	}
	return blocks
}

// blockJSON renders one ADF block to JSON for containment assertions.
func blockJSON(t *testing.T, block any) string {
	t.Helper()
	raw, err := json.Marshal(block)
	if err != nil {
		t.Fatalf("marshal block: %v", err)
	}
	return string(raw)
}

func TestADFFromMarkdown_BoldItalicCodeLinks(t *testing.T) {
	blocks := mdBlocks(t, "Fix is **up** in _MR_ `!99` — see [the diff](https://gitlab.example/mr/99).")
	if len(blocks) != 1 {
		t.Fatalf("got %d blocks, want 1 paragraph", len(blocks))
	}
	raw := blockJSON(t, blocks[0])
	for _, want := range []string{
		`"type":"paragraph"`,
		`"type":"strong"`,
		`"type":"em"`,
		`"type":"code"`,
		`"href":"https://gitlab.example/mr/99"`,
		`"text":"the diff"`,
	} {
		if !strings.Contains(raw, want) {
			t.Errorf("paragraph missing %s: %s", want, raw)
		}
	}
	// The literal markdown punctuation must be GONE — that's the whole point.
	for _, gone := range []string{"**", "](", "`!99`"} {
		if strings.Contains(raw, gone) {
			t.Errorf("paragraph still carries literal markdown %q: %s", gone, raw)
		}
	}
}

func TestADFFromMarkdown_HeadingListsAndFence(t *testing.T) {
	md := "## Findings\n\n- first\n- second\n\n1. do this\n\n```go\nfmt.Println(\"hi\")\n```"
	blocks := mdBlocks(t, md)
	if len(blocks) != 4 {
		t.Fatalf("got %d blocks, want heading+bulletList+orderedList+codeBlock: %s",
			len(blocks), blockJSON(t, blocks))
	}

	if raw := blockJSON(t, blocks[0]); !strings.Contains(raw, `"type":"heading"`) || !strings.Contains(raw, `"level":2`) {
		t.Errorf("block 0 not a level-2 heading: %s", raw)
	}
	if raw := blockJSON(t, blocks[1]); !strings.Contains(raw, `"type":"bulletList"`) || !strings.Contains(raw, `"text":"second"`) {
		t.Errorf("block 1 not the bullet list: %s", raw)
	}
	if raw := blockJSON(t, blocks[2]); !strings.Contains(raw, `"type":"orderedList"`) || !strings.Contains(raw, `"text":"do this"`) {
		t.Errorf("block 2 not the ordered list: %s", raw)
	}
	raw := blockJSON(t, blocks[3])
	if !strings.Contains(raw, `"type":"codeBlock"`) || !strings.Contains(raw, `"language":"go"`) {
		t.Errorf("block 3 not a go codeBlock: %s", raw)
	}
	if !strings.Contains(raw, `fmt.Println(\"hi\")`) {
		t.Errorf("codeBlock lost its content: %s", raw)
	}
}

func TestADFFromMarkdown_BlockquoteAndRule(t *testing.T) {
	blocks := mdBlocks(t, "> quoted wisdom\n\n---\n\nafter")
	if len(blocks) != 3 {
		t.Fatalf("got %d blocks, want blockquote+rule+paragraph", len(blocks))
	}
	if raw := blockJSON(t, blocks[0]); !strings.Contains(raw, `"type":"blockquote"`) {
		t.Errorf("block 0 not a blockquote: %s", raw)
	}
	if raw := blockJSON(t, blocks[1]); !strings.Contains(raw, `"type":"rule"`) {
		t.Errorf("block 1 not a rule: %s", raw)
	}
}

func TestADFFromMarkdown_NewlinesBecomeHardBreaks(t *testing.T) {
	// The plain-text renderer (adfInlineText) promotes newlines to hardBreaks;
	// the markdown renderer must preserve intra-paragraph line structure the
	// same way so attribution trailers stay on their own line.
	blocks := mdBlocks(t, "line one\nline two")
	if len(blocks) != 1 {
		t.Fatalf("got %d blocks, want 1", len(blocks))
	}
	raw := blockJSON(t, blocks[0])
	if !strings.Contains(raw, `"type":"hardBreak"`) {
		t.Errorf("soft line break not promoted to hardBreak: %s", raw)
	}
}

func TestADFFromMarkdown_UnmappableFallsBack(t *testing.T) {
	// Images are NO LONGER in this set (ka-0000001730) — see
	// TestADFFromMarkdown_UnresolvableImageDegradesToLink for why aborting on
	// one image was the wrong failure mode.
	for name, md := range map[string]string{
		"html":       "<div>block html</div>",
		"empty":      "",
		"whitespace": "   \n\t\n",
	} {
		if _, ok := adfBlocksFromMarkdown(md); ok {
			t.Errorf("%s: adfBlocksFromMarkdown(%q) ok, want fallback (ok=false)", name, md)
		}
	}
}

func TestADFFromMarkdown_PlainTextStillRenders(t *testing.T) {
	// A body with no markdown at all must still render (as one paragraph), so
	// the reply path takes the rich branch for ordinary prose too.
	blocks := mdBlocks(t, "just plain prose")
	if len(blocks) != 1 {
		t.Fatalf("got %d blocks, want 1", len(blocks))
	}
	if raw := blockJSON(t, blocks[0]); !strings.Contains(raw, `"text":"just plain prose"`) {
		t.Errorf("plain prose lost: %s", raw)
	}
}

// TestJiraClient_AddMarkdownComment_RendersRichBodyWithSourceLink pins the
// composed client method: G4b anchor paragraph first, then the markdown-typeset
// body blocks.
func TestJiraClient_AddMarkdownComment_RendersRichBodyWithSourceLink(t *testing.T) {
	client, capture := captureCommentBody(t)
	err := client.AddMarkdownCommentWithVisibilityAndSourceLink(
		context.Background(), "PE-42", "**bold** reply", CommentInternal, "10123")
	if err != nil {
		t.Fatal(err)
	}

	blocks := adfParagraphBlocks(t, capture())
	if len(blocks) != 2 {
		t.Fatalf("got %d blocks, want 2 (link + body): %v", len(blocks), blocks)
	}
	link := blockJSON(t, blocks[0])
	wantHref := client.BrowseURL("PE-42") + "?focusedCommentId=10123"
	if !strings.Contains(link, wantHref) || !strings.Contains(link, jiraReplySourceLinkText) {
		t.Errorf("link paragraph missing anchor: %s", link)
	}
	body := blockJSON(t, blocks[1])
	if !strings.Contains(body, `"type":"strong"`) || strings.Contains(body, "**") {
		t.Errorf("body not markdown-rendered: %s", body)
	}
}

// TestJiraClient_AddMarkdownComment_FallbackMatchesPlainVariant pins the
// degrade contract: an unmappable body posts the EXACT ADF the plain
// source-linked variant posts.
func TestJiraClient_AddMarkdownComment_FallbackMatchesPlainVariant(t *testing.T) {
	// Raw HTML, not an image: images became mappable in ka-0000001730, so an
	// image no longer exercises the degrade path this test exists to pin.
	md := "look: <div>raw html</div>"

	rich, captureRich := captureCommentBody(t)
	if err := rich.AddMarkdownCommentWithVisibilityAndSourceLink(
		context.Background(), "PE-42", md, CommentInternal, "10123"); err != nil {
		t.Fatal(err)
	}
	plain, capturePlain := captureCommentBody(t)
	if err := plain.AddCommentWithVisibilityAndSourceLink(
		context.Background(), "PE-42", md, CommentInternal, "10123"); err != nil {
		t.Fatal(err)
	}

	// Each capturing client runs its own httptest server, so the anchor hrefs
	// differ only by port — normalize both base URLs out before comparing.
	gotRich, _ := json.Marshal(captureRich())
	gotPlain, _ := json.Marshal(capturePlain())
	richNorm := strings.ReplaceAll(string(gotRich), rich.BrowseURL("PE-42"), "BASE")
	plainNorm := strings.ReplaceAll(string(gotPlain), plain.BrowseURL("PE-42"), "BASE")
	if richNorm != plainNorm {
		t.Errorf("fallback body diverged from plain variant:\nrich:  %s\nplain: %s", richNorm, plainNorm)
	}
}

// assertCodeMarkExclusive walks an ADF node tree and fails if any text node
// carries the `code` mark together with any mark other than `link`. ADF's
// schema declares `code` as excluding em/strong/strike/subsup/textColor/
// underline, so such a node makes Jira Cloud reject the whole document with a
// 400 — the root cause of the deterministic reply 502 in ka-0000000661.
func assertCodeMarkExclusive(t *testing.T, node any) {
	t.Helper()
	switch v := node.(type) {
	case map[string]any:
		if v["type"] == "text" {
			if marks, ok := v["marks"].([]any); ok {
				hasCode := false
				var others []string
				for _, m := range marks {
					mt := markType(m)
					if mt == "code" {
						hasCode = true
					} else if mt != "link" {
						others = append(others, mt)
					}
				}
				if hasCode && len(others) > 0 {
					t.Errorf("text node %q carries code mark alongside %v — invalid ADF, Jira 400s this (ka-0000000661)", v["text"], others)
				}
			}
		}
		for _, child := range v {
			assertCodeMarkExclusive(t, child)
		}
	case []any:
		for _, child := range v {
			assertCodeMarkExclusive(t, child)
		}
	}
}

// TestADFFromMarkdown_CodeMarkNeverCombinesWithFormatting is the regression for
// ka-0000000661 BUG 1: an agent's `gb jira reply` body with the exact failing
// shape — bold, backticks, a bullet list, an em-dash and an arrow, plus inline
// code nested INSIDE bold (**`check-mr`**) — previously produced a text node
// stacking `strong` + `code` marks, which Jira rejects with a 400 that the
// reply path surfaced as a bare, deterministic 502. Assert the whole document
// upholds the code-mark exclusivity invariant, and that the bold-wrapped code
// span still renders as code (just without the incompatible bold).
func TestADFFromMarkdown_CodeMarkNeverCombinesWithFormatting(t *testing.T) {
	md := "Here is the **status update** for `DEVOPS-738` — summary below:\n\n" +
		"- Fixed the **`check-mr`** CI job → it no longer warns\n" +
		"- Verified `gb jira reply` posts a comment\n" +
		"- Remaining work — none\n\n" +
		"See `foo()` and **bold** and *em* combined."

	blocks, ok := adfBlocksFromMarkdown(md)
	if !ok {
		t.Fatalf("adfBlocksFromMarkdown not ok for the failing shape, want rendered blocks")
	}
	doc := map[string]any{"version": 1, "type": "doc", "content": blocks}
	assertCodeMarkExclusive(t, doc)

	// The nested code span survives as `code` (its bold is dropped, not the span).
	raw := blockJSON(t, blocks)
	if !strings.Contains(raw, `"text":"check-mr"`) {
		t.Errorf("lost the check-mr code span: %s", raw)
	}
	if !strings.Contains(raw, `"type":"code"`) {
		t.Errorf("check-mr no longer rendered as inline code: %s", raw)
	}
}

// TestAppendCodeMark_DropsIncompatibleKeepsLink pins the mark-filter helper
// directly: given em+strong+link active, adding `code` keeps only link+code.
func TestAppendCodeMark_DropsIncompatibleKeepsLink(t *testing.T) {
	marks := []any{
		map[string]any{"type": "em"},
		map[string]any{"type": "strong"},
		map[string]any{"type": "link", "attrs": map[string]any{"href": "https://x"}},
	}
	got := appendCodeMark(marks)
	var types []string
	for _, m := range got {
		types = append(types, markType(m))
	}
	if len(types) != 2 || types[0] != "link" || types[1] != "code" {
		t.Errorf("appendCodeMark = %v, want [link code]", types)
	}
	// Input slice must be untouched (no aliasing across sibling walk branches).
	if len(marks) != 3 {
		t.Errorf("appendCodeMark mutated its input: len=%d", len(marks))
	}
}
