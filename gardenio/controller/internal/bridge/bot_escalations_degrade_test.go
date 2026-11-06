package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/slack-go/slack"

	"gardenio/controller/internal/beadsapi"
)

// newBlockRejectingSlackServer mimics the prod failure: chat.postMessage is
// rejected with invalid_blocks whenever the request carries a `blocks` payload,
// but succeeds for a plain-text-only post. That is exactly the asymmetry the
// degrade path depends on — the escalation content is fine, only the block
// rendering is refused.
//
// It also returns response_metadata.messages, which is where Slack puts the
// actual diagnosis and which slack-go's Error() drops on the floor.
func newBlockRejectingSlackServer(t *testing.T) (*httptest.Server, *[]bool) {
	t.Helper()
	var mu sync.Mutex
	// One entry per chat.postMessage call: true = the call carried blocks.
	var calls []bool

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if !strings.Contains(r.URL.Path, "chat.postMessage") {
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "message_ts": "1234.5678"})
			return
		}

		body, _ := io.ReadAll(r.Body)
		// slack-go posts form-encoded; a blocks payload shows up as a
		// non-empty `blocks=` parameter.
		hasBlocks := strings.Contains(string(body), "blocks=%5B") ||
			strings.Contains(string(body), `blocks=[`)

		mu.Lock()
		calls = append(calls, hasBlocks)
		mu.Unlock()

		if hasBlocks {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok":    false,
				"error": "invalid_blocks",
				"response_metadata": map[string]any{
					"messages": []string{"invalid_blocks_format: failed to match all allowed schemas [json-pointer:/blocks/1]"},
				},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "message_ts": "9999.1111", "channel": "C123"})
	}))
	return srv, &calls
}

// TestNotifyNewEscalation_DegradesToTextOnInvalidBlocks is the regression guard
// for ka-0000000674. Before the fix, a Slack invalid_blocks rejection returned
// an error that the caller (escalations.go handleCreated) only LOGGED — so the
// escalation was never delivered to any human and nothing retried. Measured
// over 7 days of prod data: 29 escalations were never notified, 13 still open.
//
// An escalation that silently reaches nobody defeats the entire purpose of the
// escalation handle, so a block-rendering failure must degrade to plain text
// rather than drop the ask.
func TestNotifyNewEscalation_DegradesToTextOnInvalidBlocks(t *testing.T) {
	daemon := newMockDaemon()
	daemon.beads["gd-esc-degrade"] = &beadsapi.BeadDetail{
		ID:     "gd-esc-degrade",
		Title:  "Merge !7289 — restores the wrap-up tldr banner",
		Type:   "escalation",
		Fields: map[string]string{"operation_type": "gitlab_mr_merge"},
	}
	slackSrv, calls := newBlockRejectingSlackServer(t)
	defer slackSrv.Close()
	bot := newTestBot(daemon, slackSrv)

	bead := BeadEvent{
		ID:     "gd-esc-degrade",
		Type:   "escalation",
		Title:  "Merge !7289 — restores the wrap-up tldr banner",
		Fields: map[string]string{"operation_type": "gitlab_mr_merge"},
	}

	// The whole point: this must NOT return an error.
	if err := bot.NotifyNewEscalation(context.Background(), bead); err != nil {
		t.Fatalf("NotifyNewEscalation must degrade to text, not fail: %v", err)
	}

	if len(*calls) < 2 {
		t.Fatalf("expected a blocks attempt followed by a text retry, got %d chat.postMessage call(s): %v", len(*calls), *calls)
	}
	if !(*calls)[0] {
		t.Errorf("first attempt should have carried blocks, got text-only")
	}
	if (*calls)[len(*calls)-1] {
		t.Errorf("retry should have been text-only (no blocks), got a blocks payload")
	}

	// The retry succeeded, so operator-visibility must be stamped — otherwise
	// an agent reading `kd show` still cannot tell the ask was delivered.
	got := daemon.beads["gd-esc-degrade"].Fields
	if got["slack_ts"] == "" {
		t.Errorf("slack_ts must be stamped after the text retry succeeds; fields=%v", got)
	}
	if got["notified_at"] == "" {
		t.Errorf("notified_at must be stamped after the text retry succeeds; fields=%v", got)
	}
}

// TestSlackBlockRenderError_RecoversSlackDiagnosis guards the diagnostic half of
// the fix. slack-go's SlackErrorResponse.Error() returns ONLY the bare code, so
// `fmt.Errorf("...: %w", err)` logs "invalid_blocks" with no indication of WHICH
// block Slack rejected. The detail lives in response_metadata.messages, and
// losing it is why these drops stayed unexplained.
func TestSlackBlockRenderError_RecoversSlackDiagnosis(t *testing.T) {
	respErr := slack.SlackErrorResponse{
		Err: "invalid_blocks",
		ResponseMetadata: slack.ResponseMetadata{
			Messages: []string{"invalid_blocks_format: failed to match all allowed schemas [json-pointer:/blocks/1]"},
		},
	}

	// Confirm the premise: the bare Error() really does discard the detail.
	if strings.Contains(respErr.Error(), "json-pointer") {
		t.Fatal("premise broken: slack-go now surfaces response_metadata in Error(); this helper may be redundant")
	}

	detail, isBlockErr := slackBlockRenderError(error(respErr))
	if !isBlockErr {
		t.Fatal("invalid_blocks must be recognized as a block-render error")
	}
	if !strings.Contains(detail, "json-pointer:/blocks/1") {
		t.Errorf("detail must name the offending block, got %q", detail)
	}

	// Wrapped errors must still be recognized — call sites wrap with %w.
	wrapped := errors.Join(errors.New("post escalation notification to Slack"), error(respErr))
	if _, ok := slackBlockRenderError(wrapped); !ok {
		t.Error("must unwrap a wrapped SlackErrorResponse")
	}

	// A prefix variant Slack may return must also match.
	if _, ok := slackBlockRenderError(error(slack.SlackErrorResponse{Err: "invalid_blocks_format"})); !ok {
		t.Error("invalid_blocks_format must match the prefix rule")
	}

	// Unrelated failures must NOT be treated as block errors — retrying
	// text-only would be wrong for e.g. a missing-scope or rate-limit error.
	for _, other := range []error{
		errors.New("connection reset"),
		error(slack.SlackErrorResponse{Err: "channel_not_found"}),
		error(slack.SlackErrorResponse{Err: "ratelimited"}),
	} {
		if _, ok := slackBlockRenderError(other); ok {
			t.Errorf("%v must not be classified as a block-render error", other)
		}
	}
}
