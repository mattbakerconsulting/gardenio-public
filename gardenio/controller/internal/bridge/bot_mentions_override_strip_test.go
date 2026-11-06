package bridge

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"

	"github.com/slack-go/slack/slackevents"
)

// TestStripLeadingOverrideFlags is the unit-level contract for the live-agent
// flag stripper (ka-0000001727): leading spawn-only flags are removed and the
// raw --model token is surfaced, while a flag buried mid-prompt is left as prose
// — matching handleThreadSpawn's leading-token parsers exactly.
func TestStripLeadingOverrideFlags(t *testing.T) {
	tests := []struct {
		name         string
		text         string
		wantStripped string
		wantRawModel string
	}{
		{
			name:         "no flags is a passthrough",
			text:         "keep going on the bug",
			wantStripped: "keep going on the bug",
			wantRawModel: "",
		},
		{
			name:         "leading --model is stripped and surfaced",
			text:         "--model fable keep going",
			wantStripped: "keep going",
			wantRawModel: "fable",
		},
		{
			name:         "model: colon syntax stripped and surfaced",
			text:         "model:opus investigate the alert",
			wantStripped: "investigate the alert",
			wantRawModel: "opus",
		},
		{
			name:         "--project and --role stripped, no model",
			text:         "--project gardenio --role qa run the tests",
			wantStripped: "run the tests",
			wantRawModel: "",
		},
		{
			name:         "listen + project + role + model + ultracode all stripped",
			text:         "--listen --project gardenio --role qa --model=sonnet --ultracode=on do the thing",
			wantStripped: "do the thing",
			wantRawModel: "sonnet",
		},
		{
			name:         "backend stripped alongside ultracode",
			text:         "--backend=microvm --ultracode=on do the thing",
			wantStripped: "do the thing",
			wantRawModel: "",
		},
		{
			name:         "flag buried mid-prompt is left as prose",
			text:         "please use --model fable here",
			wantStripped: "please use --model fable here",
			wantRawModel: "",
		},
		{
			name:         "unrecognized model token is still stripped and surfaced raw",
			text:         "--model gpt5 keep going",
			wantStripped: "keep going",
			wantRawModel: "gpt5",
		},
		{
			name:         "flag-only message strips to empty",
			text:         "--model fable",
			wantStripped: "",
			wantRawModel: "fable",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stripped, rawModel := stripLeadingOverrideFlags(tt.text)
			if stripped != tt.wantStripped {
				t.Errorf("stripped = %q, want %q", stripped, tt.wantStripped)
			}
			if rawModel != tt.wantRawModel {
				t.Errorf("rawModel = %q, want %q", rawModel, tt.wantRawModel)
			}
		})
	}
}

// postedText concatenates the text of every chat.postMessage /
// chat.postEphemeral call the recording Slack server observed, so a test can
// assert on the operator note the live-agent path posts. (recordingSlackServer
// itself is defined in squawk_status_test.go.)
func postedText(rec *recordingSlackServer) string {
	var parts []string
	for _, c := range rec.callsByMethod("chat.postMessage") {
		parts = append(parts, c.text)
	}
	for _, c := range rec.callsByMethod("chat.postEphemeral") {
		parts = append(parts, c.text)
	}
	return strings.Join(parts, "\n")
}

// liveMentionText returns the `text` field of the conversation bubble the
// live-agent mention path recorded for the bound agent. This is the exact
// content that is both rendered in /threads and forwarded to the pod via the
// nudge, so asserting on it proves the override flags never reached the agent.
func liveMentionText(t *testing.T, daemon *mockDaemon, agent string) string {
	t.Helper()
	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	for _, bead := range daemon.beads {
		if bead.Type == "message" && bead.Assignee == agent && hasLabel(bead.Labels, "slack-mention") {
			return bead.Fields["text"]
		}
	}
	t.Fatalf("no slack-mention message bead assigned to %q found", agent)
	return ""
}

// TestHandleAppMention_LiveAgent_StripsOverrideFlags is the acceptance for
// ka-0000001727: when @gardenio addresses an ALREADY-RUNNING agent (here a
// thread-bound live agent), the leading spawn-only override flags must be
// stripped from the text before it is recorded as a conversation bubble and
// forwarded to the pod — they used to leak verbatim into the prompt. A --model
// override additionally posts an operator note explaining a running pod can't
// hot-swap its baked model.
func TestHandleAppMention_LiveAgent_StripsOverrideFlags(t *testing.T) {
	cases := []struct {
		name          string
		text          string
		wantForward   string
		wantModelNote bool
	}{
		{
			name:          "model flag stripped + note",
			text:          "<@U-BOT> --model fable keep going on the bug",
			wantForward:   "keep going on the bug",
			wantModelNote: true,
		},
		{
			name:          "project+role flags stripped, no note",
			text:          "<@U-BOT> --project gardenio --role qa run the tests",
			wantForward:   "run the tests",
			wantModelNote: false,
		},
		{
			name:          "listen flag stripped, no note",
			text:          "<@U-BOT> --listen watch the deploy",
			wantForward:   "watch the deploy",
			wantModelNote: false,
		},
		{
			name:          "no flags is unchanged",
			text:          "<@U-BOT> just keep working",
			wantForward:   "just keep working",
			wantModelNote: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			daemon := newMockDaemon()
			daemon.beads["thread-agent"] = &beadsapi.BeadDetail{
				ID:    "bd-agent-thread",
				Title: "crew-gardenio-crew-thread-agent",
				Type:  "agent",
				Fields: map[string]string{
					"agent":   "thread-agent",
					"project": "gardenio",
					"role":    "crew",
				},
			}

			dir := t.TempDir()
			state, err := NewStateManager(filepath.Join(dir, "state.json"))
			if err != nil {
				t.Fatal(err)
			}
			// Bind the thread to the live agent so routing takes the live-agent path.
			_ = state.SetThreadAgent("C-test", "1000.0000", "thread-agent")

			rec := newRecordingSlackServer(t)
			defer rec.Close()

			b := newTestBot(daemon, rec.srv)
			b.state = state
			b.botUserID = "U-BOT"
			b.lastThreadNudge = make(map[string]time.Time)

			b.handleAppMention(context.Background(), &slackevents.AppMentionEvent{
				User:            "U-USER",
				Text:            tc.text,
				TimeStamp:       "2000.5555",
				ThreadTimeStamp: "1000.0000",
				Channel:         "C-test",
			})

			if got := liveMentionText(t, daemon, "thread-agent"); got != tc.wantForward {
				t.Errorf("forwarded bubble text = %q, want %q", got, tc.wantForward)
			}

			joined := postedText(rec)
			gotNote := strings.Contains(joined, "can't switch its model") ||
				strings.Contains(joined, "ANTHROPIC_MODEL")
			if gotNote != tc.wantModelNote {
				t.Errorf("model-switch note posted = %v, want %v (posts: %q)", gotNote, tc.wantModelNote, joined)
			}
		})
	}
}

// TestHandleAppMention_NamedLiveAgent_StripsModelFlag covers the Priority-1
// resolveAgentFromText path: naming a live agent AND passing --model. The agent
// name is stripped by resolveAgentFromText; the remaining --model flag must then
// be stripped by the live-agent path so only the real instruction reaches the
// pod, and the model note is posted.
func TestHandleAppMention_NamedLiveAgent_StripsModelFlag(t *testing.T) {
	daemon := newMockDaemon()
	// Title == the short name so resolveAgentFromText -> FindAgentBead resolves to
	// the mock's "hq" key (the mock keys beads by short agent name and its
	// extractAgentName only splits on "/").
	daemon.beads["hq"] = &beadsapi.BeadDetail{
		ID:    "bd-agent-hq",
		Title: "hq",
		Type:  "agent",
		Fields: map[string]string{
			"agent":   "hq",
			"project": "gardenio",
			"role":    "crew",
		},
	}

	dir := t.TempDir()
	state, err := NewStateManager(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}

	rec := newRecordingSlackServer(t)
	defer rec.Close()

	b := newTestBot(daemon, rec.srv)
	b.state = state
	b.botUserID = "U-BOT"
	b.lastThreadNudge = make(map[string]time.Time)

	// Top-level mention naming the live agent "hq" with a --model override.
	b.handleAppMention(context.Background(), &slackevents.AppMentionEvent{
		User:      "U-USER",
		Text:      "<@U-BOT> hq --model opus dig into the timeout",
		TimeStamp: "3000.1111",
		Channel:   "C-random",
	})

	if got := liveMentionText(t, daemon, "hq"); got != "dig into the timeout" {
		t.Errorf("forwarded bubble text = %q, want %q", got, "dig into the timeout")
	}
	if joined := postedText(rec); !strings.Contains(joined, "ANTHROPIC_MODEL") {
		t.Errorf("expected model-switch note referencing ANTHROPIC_MODEL, got: %q", joined)
	}
}
