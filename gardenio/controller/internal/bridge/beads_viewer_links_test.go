package bridge

// Integration tests for beads-viewer link rendering across the Slack handler
// surface. For each converted handler family, we exercise the real handler
// against a fake Slack server and assert the captured Slack call contains the
// viewer URL (when configured) — proving the link goes through the
// b.beadLink / b.beadTitleLink helpers all the way to the wire.
//
// A second table-driven test asserts byte-for-byte that with
// beadsViewerURL == "" the handler emits the pre-epic plain-text snapshot
// recorded under testdata/beads_viewer_links/. The snapshots are recorded
// on first run with the -update flag.
//
// Families NOT exercised here (no bead-ID interpolation in their visible
// Slack output as of gd-k5qtV1sCSV): permission, human_survey,
// human_survey_manual, squawk. If those families gain bead-ID rendering,
// add cases here.

import (
	"context"
	"encoding/json"
	"flag"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"gardenio/controller/internal/beadsapi"

	"github.com/slack-go/slack"
)

var updateBeadsViewerSnapshots = flag.Bool("update-bvl", false,
	"update the beads-viewer-links snapshot fixtures under testdata/beads_viewer_links/")

const (
	bvlViewerURL  = "https://viewer.example"
	bvlAgent      = "test-agent"
	bvlChannel    = "C-bvl-test"
	bvlAgentCard  = "1000.0001"
)

// bvlCall records one Slack API call with both text and blocks form values.
type bvlCall struct {
	Method   string
	Channel  string
	Text     string
	Blocks   string
	ThreadTS string
	TS       string
}

// bvlSlackServer is like capturingSlackServer but also records the blocks
// form value. Returns the server, a pointer to the calls slice, and a mutex.
func bvlSlackServer(t *testing.T) (*httptest.Server, *[]bvlCall, *sync.Mutex) {
	t.Helper()
	var calls []bvlCall
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		mu.Lock()
		calls = append(calls, bvlCall{
			Method:   strings.TrimPrefix(r.URL.Path, "/"),
			Channel:  r.FormValue("channel"),
			Text:     r.FormValue("text"),
			Blocks:   r.FormValue("blocks"),
			ThreadTS: r.FormValue("thread_ts"),
			TS:       r.FormValue("ts"),
		})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": true, "channel": bvlChannel, "ts": "2222.3333", "message_ts": "2222.3333",
		})
	}))
	return srv, &calls, &mu
}

// bvlGetCalls returns a copy of the recorded calls under the mutex.
func bvlGetCalls(calls *[]bvlCall, mu *sync.Mutex) []bvlCall {
	mu.Lock()
	defer mu.Unlock()
	return append([]bvlCall{}, *calls...)
}

// configureAgentThreading sets up the bot so handlers that fall back to the
// agent-card thread can post messages without resolving a real thread-bound
// agent bead. The mock daemon's FindAgentBead returns ErrAgentNotFound for
// unseeded agents, which lets the bot fall through to the agent-card branch.
func configureAgentThreading(b *Bot) {
	b.threadingMode = "agent"
	b.channel = bvlChannel
	b.agentCards[bvlAgent] = MessageRef{ChannelID: bvlChannel, Timestamp: bvlAgentCard}
	if b.beadMsgs == nil {
		b.beadMsgs = make(map[string]MessageRef)
	}
}

// caseSpec is one handler family case. Setup configures the bot/mock and
// invoke runs the handler. After invoke, captureFn is run on the recorded
// calls to extract a single artifact string for both link-presence
// assertion and snapshot comparison.
type caseSpec struct {
	name       string
	setup      func(t *testing.T, b *Bot, daemon *mockDaemon)
	invoke     func(t *testing.T, b *Bot)
	wantInArt  string // substring asserted when viewerURL is set
	captureFn  func(calls []bvlCall) string
	snapshot   string // testdata filename (without .txt)
}

// captureFirstBlocks returns the blocks form value from the first non-empty
// "blocks" call. Useful for handlers that build messages with MsgOptionBlocks.
func captureFirstBlocks(calls []bvlCall) string {
	for _, c := range calls {
		if c.Blocks != "" {
			return c.Blocks
		}
	}
	return ""
}

// captureFirstText returns the text form value from the first non-empty
// "text" call. Useful for text-only handlers (bead-activity, comment-forward).
func captureFirstText(calls []bvlCall) string {
	for _, c := range calls {
		if c.Text != "" {
			return c.Text
		}
	}
	return ""
}

// bvlCases enumerates every converted handler family. Each case must be
// re-runnable with the same setup against both a viewer-URL bot and a
// no-URL bot (the snapshot fallback case).
func bvlCases() []caseSpec {
	return []caseSpec{
		{
			name: "decisions_NotifyDecision",
			setup: func(t *testing.T, b *Bot, daemon *mockDaemon) {
				t.Helper()
				configureAgentThreading(b)
				// Disable agent threading so the agent-context block (which
				// is the one carrying the bead title link) is rendered.
				b.threadingMode = ""
			},
			invoke: func(t *testing.T, b *Bot) {
				t.Helper()
				opts, _ := json.Marshal([]string{"approve", "reject"})
				err := b.NotifyDecision(context.Background(), BeadEvent{
					ID:       "gd-decint",
					Type:     "decision",
					Title:    "Pick deploy strategy",
					Priority: 2,
					Assignee: bvlAgent,
					Fields: map[string]string{
						"prompt":  "Blue/green or rolling?",
						"options": string(opts),
					},
				})
				if err != nil {
					t.Fatalf("NotifyDecision: %v", err)
				}
			},
			wantInArt: bvlViewerURL + "/gd-decint",
			captureFn: captureFirstBlocks,
			snapshot:  "decisions_notify",
		},
		{
			name: "escalations_NotifyNewEscalation",
			setup: func(t *testing.T, b *Bot, daemon *mockDaemon) {
				t.Helper()
				configureAgentThreading(b)
			},
			invoke: func(t *testing.T, b *Bot) {
				t.Helper()
				err := b.NotifyNewEscalation(context.Background(), BeadEvent{
					ID:       "gd-escint",
					Type:     "escalation",
					Title:    "Reset prod cache",
					Priority: 1,
					Fields: map[string]string{
						"operation_type":   "write",
						"execution_target": "prod",
						"requesting_agent": bvlAgent,
					},
				})
				if err != nil {
					t.Fatalf("NotifyNewEscalation: %v", err)
				}
			},
			wantInArt: bvlViewerURL + "/gd-escint",
			captureFn: captureFirstBlocks,
			snapshot:  "escalations_new",
		},
		{
			name: "escalations_UpdateEscalationApproved",
			setup: func(t *testing.T, b *Bot, daemon *mockDaemon) {
				t.Helper()
				configureAgentThreading(b)
				// Pre-seed the message ref so UpdateEscalationApproved can
				// find an existing message to patch.
				b.messages["gd-escapp"] = MessageRef{
					ChannelID: bvlChannel, Timestamp: "2222.3333", Agent: bvlAgent,
				}
			},
			invoke: func(t *testing.T, b *Bot) {
				t.Helper()
				err := b.UpdateEscalationApproved(context.Background(), BeadEvent{
					ID:    "gd-escapp",
					Type:  "escalation",
					Title: "Drop migrations table",
					Fields: map[string]string{
						"approved_by_email": "ops@example.com",
						"approval_method":   "web_ui",
						"execution_target":  "prod",
					},
				})
				if err != nil {
					t.Fatalf("UpdateEscalationApproved: %v", err)
				}
			},
			wantInArt: bvlViewerURL + "/gd-escapp",
			captureFn: captureFirstBlocks,
			snapshot:  "escalations_approved",
		},
		{
			name: "escalations_NotifyEscalationResult",
			setup: func(t *testing.T, b *Bot, daemon *mockDaemon) {
				t.Helper()
				configureAgentThreading(b)
				b.messages["gd-escres"] = MessageRef{
					ChannelID: bvlChannel, Timestamp: "2222.3333", Agent: bvlAgent,
				}
			},
			invoke: func(t *testing.T, b *Bot) {
				t.Helper()
				err := b.NotifyEscalationResult(context.Background(), BeadEvent{
					ID:    "gd-escres",
					Type:  "escalation",
					Title: "Run migration",
					Fields: map[string]string{
						"result_status": "success",
						"executor":      "ops-bot",
					},
				})
				if err != nil {
					t.Fatalf("NotifyEscalationResult: %v", err)
				}
			},
			wantInArt: bvlViewerURL + "/gd-escres",
			captureFn: captureFirstBlocks,
			snapshot:  "escalations_result",
		},
		{
			name: "escalations_DismissEscalation",
			setup: func(t *testing.T, b *Bot, daemon *mockDaemon) {
				t.Helper()
				configureAgentThreading(b)
				b.messages["gd-escrej"] = MessageRef{
					ChannelID: bvlChannel, Timestamp: "2222.3333", Agent: bvlAgent,
				}
			},
			invoke: func(t *testing.T, b *Bot) {
				t.Helper()
				err := b.DismissEscalation(context.Background(), "gd-escrej")
				if err != nil {
					t.Fatalf("DismissEscalation: %v", err)
				}
			},
			wantInArt: bvlViewerURL + "/gd-escrej",
			captureFn: captureFirstBlocks,
			snapshot:  "escalations_dismissed",
		},
		{
			name: "beadactivity_NotifyBeadCreated",
			setup: func(t *testing.T, b *Bot, daemon *mockDaemon) {
				t.Helper()
				configureAgentThreading(b)
				enableVibeForAgent(daemon, bvlAgent) // bead activity posts only in vibe mode
			},
			invoke: func(t *testing.T, b *Bot) {
				t.Helper()
				b.NotifyBeadCreated(context.Background(), BeadEvent{
					ID:        "gd-actcre",
					Type:      "task",
					Title:     "Backfill audit log",
					CreatedBy: bvlAgent,
				})
			},
			wantInArt: bvlViewerURL + "/gd-actcre",
			captureFn: captureFirstText,
			snapshot:  "beadactivity_created",
		},
		{
			name: "beadactivity_NotifyBeadClaimed",
			setup: func(t *testing.T, b *Bot, daemon *mockDaemon) {
				t.Helper()
				configureAgentThreading(b)
				enableVibeForAgent(daemon, bvlAgent) // bead activity posts only in vibe mode
			},
			invoke: func(t *testing.T, b *Bot) {
				t.Helper()
				b.NotifyBeadClaimed(context.Background(), BeadEvent{
					ID:       "gd-actcla",
					Type:     "task",
					Title:    "Investigate flake",
					Assignee: bvlAgent,
				})
			},
			wantInArt: bvlViewerURL + "/gd-actcla",
			captureFn: captureFirstText,
			snapshot:  "beadactivity_claimed",
		},
		{
			name: "beadactivity_NotifyBeadClosed",
			setup: func(t *testing.T, b *Bot, daemon *mockDaemon) {
				t.Helper()
				configureAgentThreading(b)
				enableVibeForAgent(daemon, bvlAgent) // bead activity posts only in vibe mode
			},
			invoke: func(t *testing.T, b *Bot) {
				t.Helper()
				b.NotifyBeadClosed(context.Background(), BeadEvent{
					ID:       "gd-actclo",
					Type:     "task",
					Title:    "Ship the patch",
					Assignee: bvlAgent,
				})
			},
			wantInArt: bvlViewerURL + "/gd-actclo",
			captureFn: captureFirstText,
			snapshot:  "beadactivity_closed",
		},
		{
			name: "formula_handleFormulaList",
			setup: func(t *testing.T, b *Bot, daemon *mockDaemon) {
				t.Helper()
				daemon.mu.Lock()
				daemon.beads["gd-formula1"] = &beadsapi.BeadDetail{
					ID:     "gd-formula1",
					Title:  "Release gardenio",
					Type:   "formula",
					Status: "open",
				}
				daemon.mu.Unlock()
			},
			invoke: func(t *testing.T, b *Bot) {
				t.Helper()
				b.handleFormulaList(context.Background(), slack.SlashCommand{
					ChannelID: "C-cmd", UserID: "U-user",
				})
			},
			wantInArt: bvlViewerURL + "/gd-formula1",
			captureFn: captureFirstBlocks,
			snapshot:  "formula_list",
		},
		{
			name: "comment_forward_handleCommentAdded",
			setup: func(t *testing.T, b *Bot, daemon *mockDaemon) {
				t.Helper()
				configureAgentThreading(b)
				enableVibeForAgent(daemon, bvlAgent) // forwarded comments relay only in vibe mode
				// Parent bead resolves the assignee for agent routing.
				daemon.mu.Lock()
				daemon.beads["gd-comtgt"] = &beadsapi.BeadDetail{
					ID:       "gd-comtgt",
					Type:     "task",
					Title:    "Investigate p95 spike",
					Assignee: bvlAgent,
				}
				daemon.mu.Unlock()
			},
			invoke: func(t *testing.T, b *Bot) {
				t.Helper()
				cf := NewCommentForward(CommentForwardConfig{
					Daemon: b.daemon,
					Bot:    b,
					Logger: b.logger,
				})
				payload := []byte(`{"comment":{"id":42,"bead_id":"gd-comtgt","author":"other-agent","text":"Audit complete — rolling back.","created_at":"2026-04-25T12:00:00Z"}}`)
				cf.handleCommentAdded(context.Background(), payload)
			},
			wantInArt: bvlViewerURL + "/gd-comtgt",
			captureFn: captureFirstText,
			snapshot:  "comment_forward",
		},
		{
			name: "agents_wrapup_formatWrapUpSlack",
			setup: func(t *testing.T, b *Bot, daemon *mockDaemon) {
				t.Helper()
			},
			invoke: func(t *testing.T, b *Bot) {
				t.Helper()
				// formatWrapUpSlack is a pure function — push the rendered
				// string through the same artifact channel by posting it
				// via the fake server, so the snapshot path exercises it
				// uniformly.
				out := b.formatWrapUpSlack(Garden, `{"accomplishments":"shipped X","beads_closed":["gd-wrapaaa","gd-wrapbbb"]}`)
				_, _, err := b.api.PostMessageContext(context.Background(), bvlChannel,
					slack.MsgOptionText(out, false))
				if err != nil {
					t.Fatalf("PostMessageContext: %v", err)
				}
			},
			wantInArt: bvlViewerURL + "/gd-wrapaaa",
			captureFn: captureFirstText,
			snapshot:  "agents_wrapup",
		},
	}
}

// runBVLCase invokes one case against a freshly-built bot with the given
// viewerURL and returns the captured artifact string.
func runBVLCase(t *testing.T, c caseSpec, viewerURL string) string {
	t.Helper()
	daemon := newMockDaemon()
	srv, calls, mu := bvlSlackServer(t)
	t.Cleanup(srv.Close)

	b := newTestBot(daemon, srv)
	b.beadsViewerURL = viewerURL
	c.setup(t, b, daemon)
	c.invoke(t, b)

	return c.captureFn(bvlGetCalls(calls, mu))
}

// TestBeadsViewerLinks_HandlersIncludeViewerURL exercises every converted
// handler family with bot.beadsViewerURL set. Each case asserts that the
// captured Slack call contains the deep-link URL for the bead under test —
// proving the link goes through b.beadLink/b.beadTitleLink end-to-end.
func TestBeadsViewerLinks_HandlersIncludeViewerURL(t *testing.T) {
	for _, c := range bvlCases() {
		t.Run(c.name, func(t *testing.T) {
			art := runBVLCase(t, c, bvlViewerURL)
			if art == "" {
				t.Fatalf("no Slack call captured for %s", c.name)
			}
			if !strings.Contains(art, c.wantInArt) {
				t.Errorf("%s: expected artifact to contain %q\n--- artifact ---\n%s",
					c.name, c.wantInArt, art)
			}
		})
	}
}

// TestBeadsViewerLinks_EmptyViewerURLMatchesSnapshot asserts that with
// beadsViewerURL == "" each handler emits the byte-for-byte pre-epic
// plain-text artifact recorded under testdata/beads_viewer_links/. Run
// `go test ./internal/bridge -run BeadsViewerLinks_Empty -update-bvl` to
// (re-)record snapshots.
func TestBeadsViewerLinks_EmptyViewerURLMatchesSnapshot(t *testing.T) {
	for _, c := range bvlCases() {
		t.Run(c.name, func(t *testing.T) {
			art := runBVLCase(t, c, "")
			path := filepath.Join("testdata", "beads_viewer_links", c.snapshot+".txt")
			if *updateBeadsViewerSnapshots {
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatalf("mkdir: %v", err)
				}
				if err := os.WriteFile(path, []byte(art), 0o644); err != nil {
					t.Fatalf("write snapshot: %v", err)
				}
				return
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read snapshot %s: %v (run with -update-bvl to record)", path, err)
			}
			if art != string(want) {
				t.Errorf("%s: snapshot mismatch\n--- want (%s) ---\n%s\n--- got ---\n%s",
					c.name, path, string(want), art)
			}
			// With viewerURL empty the URL must NOT appear anywhere in the
			// rendered artifact — regression guard for the link helpers.
			if strings.Contains(art, "https://viewer.example") {
				t.Errorf("%s: viewer URL leaked into plain-text artifact:\n%s", c.name, art)
			}
		})
	}
}
