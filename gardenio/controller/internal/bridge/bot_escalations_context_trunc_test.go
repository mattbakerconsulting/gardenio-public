package bridge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// TestNotifyNewEscalation_ContextRuneTruncation is the regression guard for
// ka-0000000920 (the 2nd call site of the ka-0000006181 byte-truncation bug).
//
// The escalation `context` field was truncated by BYTES: escCtx[:2897]. When
// the 2897-byte boundary lands in the middle of a multi-byte rune, the emitted
// mrkdwn is invalid UTF-8, Slack rejects the whole message with invalid_blocks,
// and the escalation is silently lost (measured: 1 of 29 dropped escalations in
// ka-0000000674). The fix truncates by runes via truncateRunes.
//
// Detection: a mid-rune byte cut leaves an incomplete UTF-8 sequence that Go's
// json.Marshal (and thus anything reading the payload back) renders as the
// replacement char U+FFFD. Rune-aware truncation never produces one. Both a
// byte-cut and a rune-cut yield a valid final string, so the presence of U+FFFD
// in the posted block text is the discriminating signature of the bug.
func TestNotifyNewEscalation_ContextRuneTruncation(t *testing.T) {
	daemon := newMockDaemon()

	var mu sync.Mutex
	var postedBlocks string
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chat.postMessage" {
			_ = r.ParseForm()
			mu.Lock()
			postedBlocks = r.FormValue("blocks")
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "channel": "C123", "ts": "1111.2222"})
	}))
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.channel = "C123"

	// Em-dashes are 3 bytes each. 4000 runes = 12000 bytes, so BOTH truncation
	// schemes clip: the byte cut at 2897 lands mid-rune (2897 % 3 != 0) and
	// corrupts, while the rune cut at 2900 stays clean. This isolates the defect
	// to the truncation strategy, not to whether truncation happens at all.
	longCtx := strings.Repeat("—", 4000)
	if err := bot.NotifyNewEscalation(context.Background(), BeadEvent{
		ID:       "gd-esc-runes",
		Type:     "escalation",
		Title:    "Rune-boundary escalation",
		Priority: 2,
		Fields: map[string]string{
			"operation_type": "gitlab_mr_merge",
			"context":        longCtx,
		},
	}); err != nil {
		t.Fatalf("NotifyNewEscalation: %v", err)
	}

	mu.Lock()
	blocks := postedBlocks
	mu.Unlock()

	if blocks == "" {
		t.Fatal("no blocks payload captured")
	}
	// The context block must not contain the Unicode replacement char — its
	// presence means a multi-byte rune was split (the invalid_blocks bug). The
	// slack-go client JSON-marshals the payload, so U+FFFD surfaces either as the
	// raw rune or, once JSON-escaped, as the ASCII literal \ufffd. Check both.
	if strings.ContainsRune(blocks, '�') || strings.Contains(strings.ToLower(blocks), `\ufffd`) {
		tail := blocks
		if len(tail) > 40 {
			tail = tail[len(tail)-40:]
		}
		t.Errorf("posted blocks contain U+FFFD replacement char — context was byte-truncated mid-rune: tail=%q", tail)
	}
	// Sanity: the em-dash context did make it into the payload (JSON-escaped as
	// —), so the assertion above is actually exercising the context block.
	if !strings.Contains(blocks, `—`) && !strings.Contains(blocks, "—") {
		t.Errorf("expected em-dash context in posted blocks, got none")
	}
}

// TestNotifyNewEscalation_ShortContextUnchanged confirms the common path — a
// context under the cap — is passed through untouched.
func TestNotifyNewEscalation_ShortContextUnchanged(t *testing.T) {
	daemon := newMockDaemon()

	var mu sync.Mutex
	var postedBlocks string
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chat.postMessage" {
			_ = r.ParseForm()
			mu.Lock()
			postedBlocks = r.FormValue("blocks")
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "channel": "C123", "ts": "1111.2222"})
	}))
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.channel = "C123"

	if err := bot.NotifyNewEscalation(context.Background(), BeadEvent{
		ID:    "gd-esc-short",
		Type:  "escalation",
		Title: "Short escalation",
		Fields: map[string]string{
			"operation_type": "gitlab_mr_merge",
			"context":        "short background — with an em-dash",
		},
	}); err != nil {
		t.Fatalf("NotifyNewEscalation: %v", err)
	}

	mu.Lock()
	blocks := postedBlocks
	mu.Unlock()
	if !strings.Contains(blocks, "short background") {
		t.Errorf("expected full short context in posted blocks, got %q", blocks)
	}
	if strings.Contains(blocks, "...") {
		t.Errorf("short context must not be truncated, got ellipsis: %q", blocks)
	}
}
