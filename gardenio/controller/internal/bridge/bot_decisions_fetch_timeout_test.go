package bridge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestFetchDecisionMessage_HonorsContextDeadline is a fault-injection regression
// for the SSE-dispatch wedge: fetchDecisionMessage reads a decision's Slack
// message via conversations.replies / conversations.history. It is reachable
// from the SSE read loop (readEvents -> dispatch -> handleReportClosed ->
// PostReport -> fetchDecisionMessage), and dispatch joins its handler goroutines
// with wg.Wait — so if that Slack read ignores the handler's 60s hctx and hangs
// (half-open / blackholed connection), the whole single-replica bridge event
// loop wedges indefinitely.
//
// Pre-fix, fetchDecisionMessage called the non-Context slack-go variants
// (GetConversationReplies / GetConversationHistory), which run on
// context.Background() and cannot be cancelled — so against an upstream that
// accepts the request but never responds, the call blocks past the caller's
// deadline and this test's assertion window elapses. Post-fix it calls the
// *Context variants, so the passed ctx's deadline aborts the in-flight read and
// the function returns promptly.
func TestFetchDecisionMessage_HonorsContextDeadline(t *testing.T) {
	for _, tc := range []struct {
		name     string
		ref      MessageRef
		wantPath string
	}{
		{
			name:     "thread_replies",
			ref:      MessageRef{ChannelID: "C123", Timestamp: "111.222", ThreadTS: "111.000"},
			wantPath: "/conversations.replies",
		},
		{
			name:     "channel_history",
			ref:      MessageRef{ChannelID: "C123", Timestamp: "111.222"},
			wantPath: "/conversations.history",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// release is closed during cleanup so a still-blocked handler always
			// unblocks — otherwise httptest.Server.Close() (which waits for
			// in-flight requests) would itself hang and mask the result.
			release := make(chan struct{})
			hit := make(chan struct{}, 1)

			slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == tc.wantPath {
					select {
					case hit <- struct{}{}:
					default:
					}
					// The "connected but no bytes" upstream: hold the response
					// until either the request ctx is cancelled (the property the
					// fix restores) or the test releases us on cleanup.
					select {
					case <-r.Context().Done():
					case <-release:
					}
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
			}))
			t.Cleanup(func() {
				close(release)
				slackSrv.Close()
			})

			bot := newTestBot(newMockDaemon(), slackSrv)

			// hctx analogue: a deadline far shorter than the assertion window.
			ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
			defer cancel()

			done := make(chan struct{})
			go func() {
				// We only assert the call RETURNS once ctx expires; a hung
				// upstream yields (empty, false), the correct fail-safe.
				_, _ = bot.fetchDecisionMessage(ctx, tc.ref, "dec-1")
				close(done)
			}()

			select {
			case <-hit:
			case <-time.After(2 * time.Second):
				t.Fatalf("expected the test to hit %s", tc.wantPath)
			}

			select {
			case <-done:
				// Returned within the window — ctx was honored.
			case <-time.After(3 * time.Second):
				t.Fatal("fetchDecisionMessage did not return after its ctx deadline — " +
					"Slack read ignored ctx and would wedge the SSE dispatch loop")
			}
		})
	}
}
