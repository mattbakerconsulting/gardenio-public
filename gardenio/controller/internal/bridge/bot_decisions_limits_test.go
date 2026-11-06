package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/slack-go/slack"
)

// jsbg1Re436Decision is a decision shaped like gd-Jsbg1Re436, the 3.11 go/no-go
// that never reached Slack (gd-fthcee65T7): about 5k chars of context naming a
// bead id every few lines, and three option labels of about 95 chars.
func jsbg1Re436Decision() BeadEvent {
	var ctx strings.Builder
	for i := 0; ctx.Len() < 5100; i++ {
		fmt.Fprintf(&ctx, "%d. Row walked on the data that backs it: PASS, see gd-Row%07d; FAIL (P1) gd-Bug%07d holds the fix.\n", i+1, i, i)
	}
	opts, _ := json.Marshal([]map[string]string{
		{"id": "nogo-fix", "label": "No-go - crew the two P1s, roll them, then re-walk 3.11", "artifact_type": "plan"},
		{"id": "go-followups", "label": "Go - accept Commons II with the 4 bugs as follow-ups, merge #815, close 3.11", "artifact_type": "report"},
		{"id": "hold-walk", "label": "Hold - I walk the click-through rows in a browser first (cards, reply, mention, Done), then decide", "artifact_type": "report"},
	})
	return BeadEvent{
		ID:       "gd-Jsbg1Re436",
		Type:     "decision",
		Title:    "Commons II parity (3.11): go or no-go?",
		Priority: 1,
		Assignee: "gardenio/crew/test-agent",
		Fields: map[string]string{
			"prompt":  "Commons II parity (3.11): go or no-go? 2 rows FAIL with P1 bugs (death notices gd-JGmXQuSndB; bridge stamps gd-2IEp8qx7bq)",
			"options": string(opts),
			"context": ctx.String(),
		},
	}
}

// newLinkifyTestBot is newTestBot with the production Slack client shape: the
// linkify transport rewrites every outgoing chat.postMessage, so the test sees
// the payload Slack would.
func newLinkifyTestBot(slackSrv *httptest.Server, viewer string) *Bot {
	bot := newTestBot(newMockDaemon(), slackSrv)
	bot.api = slack.New("xoxb-test", slack.OptionAPIURL(slackSrv.URL+"/"),
		slack.OptionHTTPClient(newSlackLinkifyClient(http.DefaultClient, kdRules(viewer))))
	bot.beadsViewerURL = viewer
	bot.channel = "C123"
	return bot
}

// assertBlockKitLimits fails t for every Block Kit limit blocks breaks:
// section text 3000, context element and section field 2000, header 150,
// button text 75, 50 blocks.
func assertBlockKitLimits(t *testing.T, blocksJSON string) {
	t.Helper()
	var blocks []map[string]any
	if err := json.Unmarshal([]byte(blocksJSON), &blocks); err != nil {
		t.Fatalf("blocks: %v", err)
	}
	if len(blocks) > 50 {
		t.Errorf("%d blocks, Slack takes 50", len(blocks))
	}
	text := func(o any) string {
		m, _ := o.(map[string]any)
		s, _ := m["text"].(string)
		return s
	}
	over := func(where, s string, limit int) {
		if n := utf8.RuneCountInString(s); n > limit {
			t.Errorf("%s is %d chars, limit %d", where, n, limit)
		}
	}
	for i, b := range blocks {
		switch b["type"] {
		case "section":
			over(fmt.Sprintf("blocks[%d].text", i), text(b["text"]), 3000)
			fields, _ := b["fields"].([]any)
			for j, f := range fields {
				over(fmt.Sprintf("blocks[%d].fields[%d]", i, j), text(f), 2000)
			}
			if acc, ok := b["accessory"].(map[string]any); ok && acc["type"] == "button" {
				over(fmt.Sprintf("blocks[%d].accessory.text", i), text(acc["text"]), 75)
			}
		case "context":
			els, _ := b["elements"].([]any)
			for j, e := range els {
				over(fmt.Sprintf("blocks[%d].elements[%d]", i, j), text(e), 2000)
			}
		case "header":
			over(fmt.Sprintf("blocks[%d].text", i), text(b["text"]), 150)
		case "actions":
			els, _ := b["elements"].([]any)
			for j, e := range els {
				m, _ := e.(map[string]any)
				over(fmt.Sprintf("blocks[%d].elements[%d].text", i, j), text(m["text"]), 75)
			}
		}
	}
}

// TestNotifyDecision_LongContextFitsBlockKitOnTheWire is the gd-fthcee65T7
// regression: the card clamped gd-Jsbg1Re436's context to 2900 chars, then
// the linkify transport turned its bead ids into links and the section went
// out at 3470 chars, so Slack refused the whole card with invalid_blocks.
// What Slack receives must fit every limit.
func TestNotifyDecision_LongContextFitsBlockKitOnTheWire(t *testing.T) {
	var mu sync.Mutex
	var posted []string
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chat.postMessage" {
			_ = r.ParseForm()
			mu.Lock()
			posted = append(posted, r.FormValue("blocks"))
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "channel": "C123", "ts": "1111.2222"})
	}))
	defer slackSrv.Close()

	bot := newLinkifyTestBot(slackSrv, "https://gardenio.clonk.tv/beads")
	if err := bot.NotifyDecision(context.Background(), jsbg1Re436Decision()); err != nil {
		t.Fatalf("NotifyDecision: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(posted) != 1 {
		t.Fatalf("posted %d messages, want 1", len(posted))
	}
	assertBlockKitLimits(t, posted[0])
	if !strings.Contains(posted[0], "<https://gardenio.clonk.tv/beads/gd-Jsbg1Re436|Read the full context>") {
		t.Errorf("cut context does not link to the full decision: %s", posted[0])
	}
	// The short sections keep their links: only an over-limit node is spared.
	if !strings.Contains(posted[0], "<https://gardenio.clonk.tv/beads/gd-JGmXQuSndB|gd-JGmXQuSndB>") {
		t.Errorf("header bead id lost its link: %s", posted[0])
	}
}

// TestNotifyDecision_InvalidBlocksRetriesWithoutContext: when Slack still
// refuses the card, the decision is asked anyway, as the same card without
// its context section, and Slack's reason is not discarded.
func TestNotifyDecision_InvalidBlocksRetriesWithoutContext(t *testing.T) {
	var mu sync.Mutex
	var posted []string
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/chat.postMessage" {
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
			return
		}
		_ = r.ParseForm()
		mu.Lock()
		posted = append(posted, r.FormValue("blocks"))
		n := len(posted)
		mu.Unlock()
		if n == 1 {
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "invalid_blocks",
				"response_metadata": map[string]any{"messages": []string{"[ERROR] must be less than 3001 characters [json-pointer:/blocks/1/text/text]"}}})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "channel": "C123", "ts": "2222.3333"})
	}))
	defer slackSrv.Close()

	bot := newLinkifyTestBot(slackSrv, "https://v")
	dec := jsbg1Re436Decision()
	if err := bot.NotifyDecision(context.Background(), dec); err != nil {
		t.Fatalf("NotifyDecision: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(posted) != 2 {
		t.Fatalf("posted %d messages, want the card then its retry", len(posted))
	}
	retry := posted[1]
	if strings.Contains(retry, "Row walked on the data") {
		t.Errorf("retry still carries the context section: %s", retry)
	}
	for _, want := range []string{"Context too long for Slack", "resolve_gd-Jsbg1Re436_1", "resolve_gd-Jsbg1Re436_3", "resolve_other_gd-Jsbg1Re436", "dismiss_decision"} {
		if !strings.Contains(retry, want) {
			t.Errorf("retry lacks %q: %s", want, retry)
		}
	}
	if ref, ok := bot.lookupMessage(dec.ID); !ok || ref.Timestamp != "2222.3333" {
		t.Errorf("decision tracked at %+v (ok=%v), want the retry's ts", ref, ok)
	}
}

func TestClampDecisionContext(t *testing.T) {
	if got := clampDecisionContext("short", "<v/gd-x|link>"); got != "short" {
		t.Errorf("short context changed: %q", got)
	}
	long := strings.Repeat("— ", 3000)
	got := clampDecisionContext(long, "<v/gd-x|Read the full context>")
	if n := utf8.RuneCountInString(got); n > decisionContextMax {
		t.Errorf("clamped context is %d runes, budget %d", n, decisionContextMax)
	}
	if !utf8.ValidString(got) {
		t.Error("clamped context is not valid UTF-8")
	}
	if !strings.HasSuffix(got, "<v/gd-x|Read the full context>_") {
		t.Errorf("clamped context does not end with the link: %q", got[len(got)-60:])
	}
}
