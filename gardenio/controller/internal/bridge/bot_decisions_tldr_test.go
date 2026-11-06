package bridge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"

	"github.com/slack-go/slack"
)

// --- tldrBlockquote / tldrMaxCharsFromFields unit tests ---

func TestTldrBlockquote_SingleLine(t *testing.T) {
	got := tldrBlockquote("Root cause: X. Fix: Y. Verified: Z.")
	want := "> Root cause: X. Fix: Y. Verified: Z."
	if got != want {
		t.Errorf("tldrBlockquote single-line = %q, want %q", got, want)
	}
}

func TestTldrBlockquote_MultiLine(t *testing.T) {
	got := tldrBlockquote("line one\nline two\nline three")
	want := "> line one\n> line two\n> line three"
	if got != want {
		t.Errorf("tldrBlockquote multi-line = %q, want %q", got, want)
	}
}

func TestTldrBlockquote_Empty(t *testing.T) {
	if got := tldrBlockquote(""); got != "> " {
		t.Errorf("tldrBlockquote empty = %q, want %q", got, "> ")
	}
}

func TestTldrMaxCharsFromFields_DefaultWhenMissing(t *testing.T) {
	tests := []struct {
		name   string
		fields map[string]string
	}{
		{"nil fields", nil},
		{"empty options", map[string]string{"options": "", "chosen": "tl"}},
		{"invalid json", map[string]string{"options": "not-json", "chosen": "tl"}},
		{"chosen not found", map[string]string{
			"options": `[{"id":"other","artifact_params":{"max_chars":200}}]`,
			"chosen":  "tl",
		}},
		{"no artifact_params", map[string]string{
			"options": `[{"id":"tl"}]`,
			"chosen":  "tl",
		}},
		{"no max_chars", map[string]string{
			"options": `[{"id":"tl","artifact_params":{"warn_at":100}}]`,
			"chosen":  "tl",
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tldrMaxCharsFromFields(tc.fields); got != 500 {
				t.Errorf("tldrMaxCharsFromFields = %d, want default 500", got)
			}
		})
	}
}

func TestTldrMaxCharsFromFields_CustomMax(t *testing.T) {
	fields := map[string]string{
		"options": `[{"id":"tl","artifact_params":{"max_chars":280}},{"id":"other"}]`,
		"chosen":  "tl",
	}
	if got := tldrMaxCharsFromFields(fields); got != 280 {
		t.Errorf("tldrMaxCharsFromFields = %d, want 280", got)
	}
}

// --- buildTldrBlocks shape tests ---

func TestPostTldrInline_Rendering(t *testing.T) {
	// 200-character body.
	body := strings.Repeat("x", 200)
	emoji := ":white_circle: :memo:"
	blocks := buildTldrBlocks("Deploy prod?", body, emoji, 500)

	if len(blocks) != 3 {
		t.Fatalf("expected 3 blocks (header, body, context), got %d", len(blocks))
	}

	headerSec, ok := blocks[0].(*slack.SectionBlock)
	if !ok {
		t.Fatalf("block 0 is %T, want *slack.SectionBlock", blocks[0])
	}
	if headerSec.Text == nil || headerSec.Text.Type != "mrkdwn" {
		t.Fatalf("header text = %+v, want mrkdwn", headerSec.Text)
	}
	wantHeader := ":white_circle: :memo: *TL;DR* — Deploy prod?"
	if headerSec.Text.Text != wantHeader {
		t.Errorf("header text = %q, want %q", headerSec.Text.Text, wantHeader)
	}

	bodySec, ok := blocks[1].(*slack.SectionBlock)
	if !ok {
		t.Fatalf("block 1 is %T, want *slack.SectionBlock", blocks[1])
	}
	if bodySec.Text == nil || bodySec.Text.Type != "mrkdwn" {
		t.Fatalf("body text = %+v, want mrkdwn", bodySec.Text)
	}
	if !strings.HasPrefix(bodySec.Text.Text, "> ") {
		t.Errorf("body should start with blockquote prefix, got %q...", bodySec.Text.Text[:20])
	}
	if strings.Contains(bodySec.Text.Text, "```") {
		t.Error("body must not contain code fences")
	}
	if !strings.Contains(bodySec.Text.Text, strings.Repeat("x", 200)) {
		t.Error("body must contain the full 200-char content")
	}

	ctxBlk, ok := blocks[2].(*slack.ContextBlock)
	if !ok {
		t.Fatalf("block 2 is %T, want *slack.ContextBlock", blocks[2])
	}
	if len(ctxBlk.ContextElements.Elements) == 0 {
		t.Fatal("context block has no elements")
	}
	ctxText, ok := ctxBlk.ContextElements.Elements[0].(*slack.TextBlockObject)
	if !ok {
		t.Fatalf("context element is %T, want *slack.TextBlockObject", ctxBlk.ContextElements.Elements[0])
	}
	wantCtx := "_Decision: Deploy prod? · 200 / 500 chars_"
	if ctxText.Text != wantCtx {
		t.Errorf("context text = %q, want %q", ctxText.Text, wantCtx)
	}

	// No divider anywhere.
	for i, blk := range blocks {
		if _, isDiv := blk.(*slack.DividerBlock); isDiv {
			t.Errorf("block %d is a DividerBlock — tldr must have no divider", i)
		}
	}
}

func TestPostTldrInline_MultiLineBodyBlockquote(t *testing.T) {
	body := "Root cause: bad migration.\nFix: rolled back.\nVerified: smoke green."
	blocks := buildTldrBlocks("Why did prod break?", body, ":white_circle: :memo:", 500)
	if len(blocks) != 3 {
		t.Fatalf("expected 3 blocks, got %d", len(blocks))
	}
	bodySec := blocks[1].(*slack.SectionBlock)
	want := "> Root cause: bad migration.\n> Fix: rolled back.\n> Verified: smoke green."
	if bodySec.Text.Text != want {
		t.Errorf("multi-line body = %q, want %q", bodySec.Text.Text, want)
	}
	// Every newline should be followed by "> " (i.e. no un-prefixed continuation lines).
	for i, line := range strings.Split(bodySec.Text.Text, "\n") {
		if !strings.HasPrefix(line, "> ") {
			t.Errorf("line %d %q does not have '> ' prefix", i, line)
		}
	}
}

// --- integration: PostReport(reportType="tldr") never calls canvas path ---

func TestPostReport_TldrNeverUsesCanvasEvenForLongContent(t *testing.T) {
	daemon := newMockDaemon()
	daemon.beads["dec-tldr"] = &beadsapi.BeadDetail{
		ID:       "dec-tldr",
		Type:     "decision",
		Title:    "Ship it?",
		Priority: 2,
		Fields: map[string]string{
			"options": `[{"id":"tl","artifact_type":"tldr","artifact_params":{"max_chars":500}}]`,
			"chosen":  "tl",
		},
	}

	var callPaths []string
	var postedBlocksJSON string
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		callPaths = append(callPaths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/conversations.history":
			// Return empty so PostReport falls through to the fallback path
			// (chat.postMessage) — we still want to assert no canvas call.
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "messages": []any{}})
		case "/chat.postMessage":
			postedBlocksJSON = r.FormValue("blocks")
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "ts": "9999.0000"})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		}
	}))
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	// Wire a real CanvasCreator so the fake server would see canvas calls if
	// they happened; we assert below they do not.
	bot.canvas = NewCanvasCreator(bot.api, bot.logger)
	bot.messages["dec-tldr"] = MessageRef{
		ChannelID: "C123",
		Timestamp: "1111.2222",
	}

	// 2000-char content — well over the canvas threshold. In production the
	// length validator would have rejected this, but the bridge must still
	// never route tldr to canvas.
	longContent := strings.Repeat("a", 2000)
	if err := bot.PostReport(context.Background(), "rpt-tldr", "dec-tldr", "tldr", longContent); err != nil {
		t.Fatalf("PostReport(tldr) returned error: %v", err)
	}

	for _, p := range callPaths {
		if strings.Contains(p, "canvases") || strings.Contains(p, "canvas") {
			t.Errorf("tldr must never call canvas endpoints, got call to %q", p)
		}
	}

	// Confirm the posted message used tldr block shape (blockquote, no code fence).
	if postedBlocksJSON == "" {
		t.Fatal("expected chat.postMessage fallback with blocks payload")
	}
	if strings.Contains(postedBlocksJSON, "```") {
		t.Error("tldr posted blocks must not contain code fences")
	}
	if !strings.Contains(postedBlocksJSON, "TL;DR") {
		t.Error("tldr posted blocks must contain TL;DR header")
	}
	if !strings.Contains(postedBlocksJSON, Garden.Semantic(SemNote)) {
		t.Error("tldr posted blocks must reference the themed note glyph")
	}
}
