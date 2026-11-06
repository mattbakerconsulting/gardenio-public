package bridge

// Tests for inline-image support in the markdown → ADF renderer
// (ka-0000001730). Before this, ANY image made adfBlocksFromMarkdown return
// ok=false, and the caller's documented fallback is "post the plain-text form"
// — so one `![shot](url)` silently turned a whole rich comment into literal
// markdown source. These pin the new contract: a resolvable image becomes a
// real ADF mediaSingle, an unresolvable one degrades to a link, and neither
// takes the surrounding formatting down with it.

import (
	"encoding/json"
	"strings"
	"testing"
)

// mdMediaBlocks converts with an attachment map and requires success.
func mdMediaBlocks(t *testing.T, markdown string, media map[string]string) []any {
	t.Helper()
	blocks, ok := adfBlocksFromMarkdownWithMedia(markdown, media)
	if !ok {
		t.Fatalf("adfBlocksFromMarkdownWithMedia(%q) not ok, want rendered blocks", markdown)
	}
	return blocks
}

func allBlocksJSON(t *testing.T, blocks []any) string {
	t.Helper()
	raw, err := json.Marshal(blocks)
	if err != nil {
		t.Fatalf("marshal blocks: %v", err)
	}
	return string(raw)
}

// TestADFMedia_ResolvedImageBecomesMediaSingle is the core case: a screenshot
// the agent already attached to the issue renders inline.
func TestADFMedia_ResolvedImageBecomesMediaSingle(t *testing.T) {
	media := map[string]string{"before.png": "10501"}
	blocks := mdMediaBlocks(t, "![before](before.png)", media)

	got := allBlocksJSON(t, blocks)
	if !strings.Contains(got, `"type":"mediaSingle"`) {
		t.Errorf("want a mediaSingle block, got: %s", got)
	}
	if !strings.Contains(got, `"id":"10501"`) {
		t.Errorf("want the media node to reference attachment 10501, got: %s", got)
	}
	// An image must NOT also leave a link behind — that would double-render.
	if strings.Contains(got, `"type":"link"`) {
		t.Errorf("resolved image should not also emit a link mark: %s", got)
	}
}

// TestADFMedia_AttachmentSchemeResolves lets a caller that already holds an
// attachment id reference it without inventing a URL to key on.
func TestADFMedia_AttachmentSchemeResolves(t *testing.T) {
	blocks := mdMediaBlocks(t, "![after](attachment:10777)", nil)
	if got := allBlocksJSON(t, blocks); !strings.Contains(got, `"id":"10777"`) {
		t.Errorf("want attachment:<id> to resolve directly, got: %s", got)
	}
}

// TestADFMedia_ImageHoistedOutOfParagraph: ADF media is block-level, so an
// image written inside a sentence has to become a sibling block. The
// surrounding prose must survive intact and in order.
func TestADFMedia_ImageHoistedOutOfParagraph(t *testing.T) {
	media := map[string]string{"shot.png": "10501"}
	blocks := mdMediaBlocks(t, "The defect is gone: ![proof](shot.png) as shown.", media)

	if len(blocks) != 2 {
		t.Fatalf("want 2 blocks (paragraph + hoisted mediaSingle), got %d: %s",
			len(blocks), allBlocksJSON(t, blocks))
	}

	para := blockJSON(t, blocks[0])
	if !strings.Contains(para, `"type":"paragraph"`) {
		t.Errorf("first block should be the paragraph, got: %s", para)
	}
	for _, want := range []string{"The defect is gone: ", " as shown."} {
		if !strings.Contains(para, want) {
			t.Errorf("paragraph lost prose %q: %s", want, para)
		}
	}

	if media := blockJSON(t, blocks[1]); !strings.Contains(media, `"type":"mediaSingle"`) {
		t.Errorf("second block should be the hoisted media, got: %s", media)
	}
}

// TestADFMedia_UnresolvableImageDegradesToLink is the failure-mode fix. Losing
// the inline rendering of ONE image beats losing the formatting of everything
// around it, which is what the old abort-and-fall-back-to-plain-text did.
func TestADFMedia_UnresolvableImageDegradesToLink(t *testing.T) {
	blocks := mdMediaBlocks(t,
		"Evidence: ![before](https://viewer.example/beads/image/x.png) and **bold** survives.", nil)

	got := allBlocksJSON(t, blocks)
	if strings.Contains(got, `"type":"mediaSingle"`) {
		t.Errorf("an unresolvable image must not fabricate a media node: %s", got)
	}
	if !strings.Contains(got, `"href":"https://viewer.example/beads/image/x.png"`) {
		t.Errorf("want the image degraded to a clickable link so the reader can still reach it: %s", got)
	}
	if !strings.Contains(got, `"text":"before"`) {
		t.Errorf("want the alt text used as the link label: %s", got)
	}
	// The whole point: surrounding rich formatting is NOT collateral damage.
	if !strings.Contains(got, `"type":"strong"`) {
		t.Errorf("want the surrounding bold preserved — degrading one image must not plain-text the body: %s", got)
	}
}

func TestADFMedia_UnresolvableImageWithoutAltUsesURL(t *testing.T) {
	blocks := mdMediaBlocks(t, "![](https://example.com/x.png)", nil)
	if got := allBlocksJSON(t, blocks); !strings.Contains(got, `"text":"https://example.com/x.png"`) {
		t.Errorf("want the URL as the label when there is no alt text: %s", got)
	}
}

// TestADFMedia_PlainConverterStillRendersImages: the no-media-map entry point
// (adfBlocksFromMarkdown, used by every existing caller) must no longer abort.
func TestADFMedia_PlainConverterStillRendersImages(t *testing.T) {
	blocks, ok := adfBlocksFromMarkdown("look: ![img](https://example.com/x.png)")
	if !ok {
		t.Fatal("adfBlocksFromMarkdown should no longer abort on an image — that abort silently plain-texted whole comments")
	}
	if got := allBlocksJSON(t, blocks); !strings.Contains(got, `"href":"https://example.com/x.png"`) {
		t.Errorf("want the image degraded to a link: %s", got)
	}
}

// TestADFMedia_MultipleImagesKeepOrder: a before/after gallery is the actual
// use case, so ordering is load-bearing — a swapped pair inverts the proof.
func TestADFMedia_MultipleImagesKeepOrder(t *testing.T) {
	media := map[string]string{"before.png": "1001", "after.png": "1002"}
	blocks := mdMediaBlocks(t, "Before:\n\n![before](before.png)\n\nAfter:\n\n![after](after.png)", media)

	got := allBlocksJSON(t, blocks)
	iBefore := strings.Index(got, `"id":"1001"`)
	iAfter := strings.Index(got, `"id":"1002"`)
	if iBefore < 0 || iAfter < 0 {
		t.Fatalf("want both media nodes present: %s", got)
	}
	if iBefore > iAfter {
		t.Errorf("before/after order inverted — that flips the meaning of the proof: %s", got)
	}
}

// TestADFMedia_ImageInListItem: evidence often lands in a bullet list. The
// media hoists to a sibling of the LIST (ADF forbids mediaSingle inside a
// listItem), and the list itself must survive.
func TestADFMedia_ImageInListItem(t *testing.T) {
	media := map[string]string{"a.png": "2001"}
	blocks := mdMediaBlocks(t, "- baseline ![a](a.png)\n- second item", media)

	got := allBlocksJSON(t, blocks)
	if !strings.Contains(got, `"type":"bulletList"`) {
		t.Errorf("want the list preserved: %s", got)
	}
	if !strings.Contains(got, `"id":"2001"`) {
		t.Errorf("want the media node emitted: %s", got)
	}
	if !strings.Contains(got, "second item") {
		t.Errorf("want every list item preserved: %s", got)
	}
}

// TestADFMedia_MediaIsTopLevelNotNested pins the ADF structural rule: a
// mediaSingle must be a top-level block, never inside a paragraph's content.
func TestADFMedia_MediaIsTopLevelNotNested(t *testing.T) {
	media := map[string]string{"x.png": "3001"}
	blocks := mdMediaBlocks(t, "text ![x](x.png) more", media)

	var found bool
	for _, b := range blocks {
		m, ok := b.(map[string]any)
		if !ok {
			continue
		}
		if m["type"] == "mediaSingle" {
			found = true
		}
		// No paragraph may contain a media node.
		if m["type"] == "paragraph" {
			if raw := blockJSON(t, m); strings.Contains(raw, "media") {
				t.Errorf("paragraph must not nest a media node (ADF rejects it): %s", raw)
			}
		}
	}
	if !found {
		t.Errorf("want a top-level mediaSingle: %s", allBlocksJSON(t, blocks))
	}
}

func TestADFMedia_StillAbortsOnGenuinelyUnmappable(t *testing.T) {
	// Raw HTML has no ADF mapping and must still trigger the plain-text
	// fallback — the degrade path was narrowed to images, not removed.
	if _, ok := adfBlocksFromMarkdownWithMedia("<div>raw</div>", nil); ok {
		t.Error("want raw HTML to still report ok=false")
	}
}

func TestADFMedia_EmptyDestinationIsDropped(t *testing.T) {
	blocks, ok := adfBlocksFromMarkdownWithMedia("text ![alt]() tail", nil)
	if !ok {
		t.Fatal("an empty image destination should not abort the conversion")
	}
	got := allBlocksJSON(t, blocks)
	if strings.Contains(got, `"href":""`) {
		t.Errorf("must not emit a link to nowhere: %s", got)
	}
	if !strings.Contains(got, "tail") {
		t.Errorf("surrounding text should survive: %s", got)
	}
}
