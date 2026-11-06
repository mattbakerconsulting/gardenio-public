package bridge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// TestDecisionEvidenceLine: the Slack card names where a decision's
// screenshots are, never an image block (gd-Pi7C83262N).
func TestDecisionEvidenceLine(t *testing.T) {
	m := newMockDaemon()
	m.beads["gd-EvTask01"] = &beadsapi.BeadDetail{ID: "gd-EvTask01", Title: "Grade <the> Porch"}
	m.beads["gd-EvBead02"] = &beadsapi.BeadDetail{ID: "gd-EvBead02", Title: "Before shots", Description: "https://v/image/ui/before.png"}
	m.beadComments = map[string][]beadsapi.Comment{"gd-EvTask01": {{Text: "https://v/image/ui/a.png https://v/image/ui/b.png"}}}
	b := &Bot{daemon: m, beadsViewerURL: "https://viewer"}

	bead := BeadEvent{ID: "gd-EvDec01", Fields: map[string]string{"requesting_task": "gd-EvTask01", "evidence": "gd-EvBead02"}}
	got := b.decisionEvidenceLine(context.Background(), bead)
	want := "1 screenshot on <https://viewer/gd-EvBead02|Before shots> · 2 on <https://viewer/gd-EvTask01|Grade &lt;the&gt; Porch>"
	if got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
	if got := b.decisionEvidenceLine(context.Background(), BeadEvent{ID: "gd-EvDec02", Fields: map[string]string{}}); got != "" {
		t.Errorf("no linked bead, no line: %q", got)
	}
	if got := b.decisionEvidenceLine(context.Background(), BeadEvent{ID: "gd-EvDec03", Fields: map[string]string{"requesting_task": "gd-Nothing1"}}); got != "" {
		t.Errorf("a linked bead with no pictures, no line: %q", got)
	}
}

func TestNotifyDecision_EvidenceLineNoImageBlocks(t *testing.T) {
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

	m := newMockDaemon()
	m.beads["gd-EvTask01"] = &beadsapi.BeadDetail{ID: "gd-EvTask01", Title: "Grade the Porch"}
	m.beadComments = map[string][]beadsapi.Comment{"gd-EvTask01": {{Text: "https://v/image/ui/a.png"}}}
	bot := newTestBot(m, slackSrv)
	bot.beadsViewerURL, bot.channel = "https://viewer", "C123"
	if err := bot.NotifyDecision(context.Background(), BeadEvent{ID: "gd-EvDec01", Type: "decision", Title: "Ship?",
		Fields: map[string]string{"prompt": "Ship?", "context": "See the task.", "requesting_task": "gd-EvTask01", "options": `[{"id":"y","short":"Yes"}]`}}); err != nil {
		t.Fatalf("NotifyDecision: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(posted) != 1 {
		t.Fatalf("posted %d messages, want 1", len(posted))
	}
	card := strings.NewReplacer(`\u003c`, "<", `\u003e`, ">").Replace(posted[0])
	ctxAt, lineAt := strings.Index(card, "See the task."), strings.Index(card, "1 screenshot on <https://viewer/gd-EvTask01|Grade the Porch>")
	if ctxAt < 0 || lineAt < ctxAt {
		t.Errorf("the evidence line follows the context: %s", posted[0])
	}
	if strings.Contains(posted[0], `"type":"image"`) || strings.Contains(posted[0], "image/ui/a.png") {
		t.Errorf("no image block, no private /image link (Slack fetches anonymously): %s", posted[0])
	}
}
