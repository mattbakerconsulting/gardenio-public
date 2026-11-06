package bridge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"gardenio/controller/internal/podmanager"

	"github.com/slack-go/slack/slackevents"
)

// TestHandleAppMention_BackendOverride_StampsExecutionTarget locks in the
// --backend mention flag: a recognized backend is stamped onto the spawned
// agent bead's execution_target field (which resolveExecutionTarget reads to
// route the spawn), and --backend=microvm implies ultracode=on unless the
// operator explicitly passes --ultracode=off in the same mention.
func TestHandleAppMention_BackendOverride_StampsExecutionTarget(t *testing.T) {
	mv := podmanager.ExecutionTargetLambdaMicroVM
	k8s := podmanager.ExecutionTargetKubernetes

	cases := []struct {
		name          string
		text          string
		wantTarget    string
		wantUltracode string
	}{
		{"microvm implies ultracode on", "<@U-BOT> --backend=microvm build the feature", mv, "on"},
		{"microvm explicit ultracode off is honored", "<@U-BOT> --backend=microvm --ultracode=off build it", mv, "off"},
		{"kubernetes does not touch ultracode", "<@U-BOT> --backend=kubernetes just answer", k8s, ""},
		{"no backend flag leaves target empty", "<@U-BOT> just fix the bug", "", ""},
		{"unrecognized backend falls back to default", "<@U-BOT> --backend=vm fix it", "", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			daemon := newMockDaemon()
			daemon.seedProjectWithChannel("gardenio", "C-GARDENIO")

			slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/conversations.replies" {
					_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "messages": []map[string]any{}})
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "message_ts": "1234.5678"})
			}))
			defer slackSrv.Close()

			dir := t.TempDir()
			state, err := NewStateManager(filepath.Join(dir, "state.json"))
			if err != nil {
				t.Fatal(err)
			}

			bot := newTestBot(daemon, slackSrv)
			bot.state = state
			bot.botUserID = "U-BOT"

			bot.handleAppMention(context.Background(), &slackevents.AppMentionEvent{
				User:      "U123",
				Text:      tc.text,
				TimeStamp: "7777.8888",
				Channel:   "C-GARDENIO",
			})

			agentBeads := filterAgentBeads(daemon.beads)
			if len(agentBeads) != 1 {
				t.Fatalf("expected 1 agent bead, got %d", len(agentBeads))
			}
			for _, b := range agentBeads {
				if got := b.Fields["execution_target"]; got != tc.wantTarget {
					t.Errorf("execution_target field = %q, want %q", got, tc.wantTarget)
				}
				if got := b.Fields["ultracode"]; got != tc.wantUltracode {
					t.Errorf("ultracode field = %q, want %q", got, tc.wantUltracode)
				}
			}
		})
	}
}
