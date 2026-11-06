package bridge

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"

	"github.com/slack-go/slack"
)

func TestResolveOptionChoice_WithOptions(t *testing.T) {
	daemon := newMockDaemon()
	opts := []map[string]string{
		{"id": "a", "short": "Alpha", "label": "Option Alpha"},
		{"id": "b", "short": "Beta", "label": ""},
		{"id": "c", "short": "", "label": ""},
	}
	optsJSON, _ := json.Marshal(opts)
	daemon.mu.Lock()
	daemon.beads["dec-1"] = &beadsapi.BeadDetail{
		ID:     "dec-1",
		Type:   "decision",
		Fields: map[string]string{"options": string(optsJSON)},
	}
	daemon.mu.Unlock()

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()
	bot := newTestBot(daemon, slackSrv)

	tests := []struct {
		name        string
		beadID      string
		optIndex    string
		wantID      string // canonical id persisted in `chosen`
		wantDisplay string // human label shown in Slack
	}{
		// The id is what `chosen` must store; the display is label/short/id.
		{"label present", "dec-1", "1", "a", "Option Alpha"},
		{"falls back to short", "dec-1", "2", "b", "Beta"},
		{"falls back to id", "dec-1", "3", "c", "c"},
		{"out of range index", "dec-1", "99", "Option 99", "Option 99"},
		{"zero index", "dec-1", "0", "Option 0", "Option 0"},
		{"non-numeric index", "dec-1", "abc", "Option abc", "Option abc"},
		{"unknown bead (no options defined)", "unknown-bead", "1", "Option 1 (no options defined)", "Option 1 (no options defined)"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotID, gotDisplay := bot.resolveOptionChoice(context.Background(), tt.beadID, tt.optIndex)
			if gotID != tt.wantID {
				t.Errorf("resolveOptionChoice(%q, %q) id = %q, want %q", tt.beadID, tt.optIndex, gotID, tt.wantID)
			}
			if gotDisplay != tt.wantDisplay {
				t.Errorf("resolveOptionChoice(%q, %q) display = %q, want %q", tt.beadID, tt.optIndex, gotDisplay, tt.wantDisplay)
			}
		})
	}
}

func TestLookupArtifactType(t *testing.T) {
	daemon := newMockDaemon()
	opts := []map[string]string{
		{"id": "a", "label": "Option Alpha", "artifact_type": "report"},
		{"id": "b", "short": "Beta", "artifact_type": "plan"},
		{"id": "c", "label": "No Artifact"},
	}
	optsJSON, _ := json.Marshal(opts)
	daemon.mu.Lock()
	daemon.beads["dec-2"] = &beadsapi.BeadDetail{
		ID:     "dec-2",
		Type:   "decision",
		Fields: map[string]string{"options": string(optsJSON)},
	}
	daemon.mu.Unlock()

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()
	bot := newTestBot(daemon, slackSrv)

	tests := []struct {
		name   string
		beadID string
		chosen string
		want   string
	}{
		// The primary path now passes the option id (`chosen` is the id).
		{"match by id", "dec-2", "a", "report"},
		{"match by id b", "dec-2", "b", "plan"},
		// Label/short still resolve, keeping historical `chosen` values working.
		{"match by label", "dec-2", "Option Alpha", "report"},
		{"match by short fallback", "dec-2", "Beta", "plan"},
		{"no artifact_type", "dec-2", "No Artifact", ""},
		{"no artifact_type by id", "dec-2", "c", ""},
		{"no match", "dec-2", "Unknown", ""},
		{"unknown bead", "nonexistent", "anything", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := bot.lookupArtifactType(context.Background(), tt.beadID, tt.chosen)
			if got != tt.want {
				t.Errorf("lookupArtifactType(%q, %q) = %q, want %q", tt.beadID, tt.chosen, got, tt.want)
			}
		})
	}
}

func TestHandleDismiss(t *testing.T) {
	daemon := newMockDaemon()
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()
	bot := newTestBot(daemon, slackSrv)

	callback := slack.InteractionCallback{
		Channel: slack.Channel{
			GroupConversation: slack.GroupConversation{
				Conversation: slack.Conversation{ID: "C123"},
			},
		},
		User: slack.User{Name: "testuser"},
	}
	callback.MessageTs = "1234.5678"

	bot.handleDismiss(context.Background(), "dec-dismiss-1", callback)

	closed := daemon.getClosed()
	if len(closed) != 1 {
		t.Fatalf("expected 1 close call, got %d", len(closed))
	}
	if closed[0].BeadID != "dec-dismiss-1" {
		t.Errorf("expected beadID=dec-dismiss-1, got %s", closed[0].BeadID)
	}
	if closed[0].Fields["chosen"] != "dismissed" {
		t.Errorf("expected chosen=dismissed, got %s", closed[0].Fields["chosen"])
	}
	if closed[0].Fields["rationale"] != "Dismissed by @testuser via Slack" {
		t.Errorf("unexpected rationale: %s", closed[0].Fields["rationale"])
	}
}

func TestHandleBlockActions_DismissAction(t *testing.T) {
	daemon := newMockDaemon()
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()
	bot := newTestBot(daemon, slackSrv)

	callback := slack.InteractionCallback{
		Channel: slack.Channel{
			GroupConversation: slack.GroupConversation{
				Conversation: slack.Conversation{ID: "C123"},
			},
		},
		User: slack.User{Name: "alice"},
	}
	callback.MessageTs = "1111.2222"
	callback.ActionCallback.BlockActions = []*slack.BlockAction{
		{ActionID: "dismiss_decision", Value: "dec-block-1"},
	}

	bot.handleBlockActions(context.Background(), callback)

	closed := daemon.getClosed()
	if len(closed) != 1 {
		t.Fatalf("expected 1 close call, got %d", len(closed))
	}
	if closed[0].BeadID != "dec-block-1" {
		t.Errorf("expected beadID=dec-block-1, got %s", closed[0].BeadID)
	}
}

func TestHandleBlockActions_ResolveOptionOpensModal(t *testing.T) {
	daemon := newMockDaemon()
	opts := []map[string]string{
		{"id": "opt1", "label": "First Option"},
	}
	optsJSON, _ := json.Marshal(opts)
	daemon.mu.Lock()
	daemon.beads["dec-modal-1"] = &beadsapi.BeadDetail{
		ID:     "dec-modal-1",
		Type:   "decision",
		Fields: map[string]string{
			"options":  string(optsJSON),
			"question": "Which path?",
		},
	}
	daemon.mu.Unlock()

	// Track calls to views.open
	var openViewCalled bool
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/views.open" {
			openViewCalled = true
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":   true,
			"view": map[string]any{"id": "V123"},
		})
	}))
	defer slackSrv.Close()
	bot := newTestBot(daemon, slackSrv)

	callback := slack.InteractionCallback{
		Channel: slack.Channel{
			GroupConversation: slack.GroupConversation{
				Conversation: slack.Conversation{ID: "C123"},
			},
		},
		User:    slack.User{Name: "bob"},
		Message: slack.Message{Msg: slack.Msg{Timestamp: "9999.0000"}},
	}
	callback.TriggerID = "trigger-123"
	callback.ActionCallback.BlockActions = []*slack.BlockAction{
		{ActionID: "resolve_dec-modal-1_1", Value: "dec-modal-1:1"},
	}

	bot.handleBlockActions(context.Background(), callback)

	if !openViewCalled {
		t.Error("expected views.open to be called for resolve option")
	}
}

// TestOpenResolveModal_NotesPromptText verifies that the rationale input
// uses the new "Notes (optional)" framing — clarifying the field is optional
// and is for extra context, not a justification of the user's choice.
// See gd-dspCZK00xQ and gd-YnGhzz286H.
func TestOpenResolveModal_NotesPromptText(t *testing.T) {
	daemon := newMockDaemon()
	opts := []map[string]string{
		{"id": "opt1", "label": "First Option"},
	}
	optsJSON, _ := json.Marshal(opts)
	daemon.mu.Lock()
	daemon.beads["dec-prompt-1"] = &beadsapi.BeadDetail{
		ID:   "dec-prompt-1",
		Type: "decision",
		Fields: map[string]string{
			"options":  string(optsJSON),
			"question": "Which path?",
		},
	}
	daemon.mu.Unlock()

	var capturedBody string
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/views.open" {
			body, _ := io.ReadAll(r.Body)
			capturedBody = string(body)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":   true,
			"view": map[string]any{"id": "V123"},
		})
	}))
	defer slackSrv.Close()
	bot := newTestBot(daemon, slackSrv)

	callback := slack.InteractionCallback{
		Channel: slack.Channel{
			GroupConversation: slack.GroupConversation{
				Conversation: slack.Conversation{ID: "C123"},
			},
		},
		User:    slack.User{Name: "bob"},
		Message: slack.Message{Msg: slack.Msg{Timestamp: "9999.0000"}},
	}
	callback.TriggerID = "trigger-prompt"

	// id "opt1", display "First Option": the modal shows the display label,
	// metadata carries the id.
	bot.openResolveModal(context.Background(), "dec-prompt-1", "opt1", "First Option", callback)

	// slack-go posts views.open as JSON; substring-match the literal text.
	// Modal text must convey: optional + extra context, not justification.
	wants := []string{
		"Notes (optional)",
		"Add any extra context or notes (optional)",
		"not a justification",
		"First Option", // the human display label is shown to the operator
	}
	// The label must NOT use the old "Rationale" wording, which was unclear.
	notWants := []string{
		"Rationale (optional)",
		"Why this choice?",
	}
	for _, w := range wants {
		if !strings.Contains(capturedBody, w) {
			t.Errorf("modal payload missing %q\nbody: %s", w, capturedBody)
		}
	}
	for _, nw := range notWants {
		if strings.Contains(capturedBody, nw) {
			t.Errorf("modal payload still contains old text %q\nbody: %s", nw, capturedBody)
		}
	}
}

func TestHandleBlockActions_NoActions(t *testing.T) {
	daemon := newMockDaemon()
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()
	bot := newTestBot(daemon, slackSrv)

	callback := slack.InteractionCallback{}
	callback.ActionCallback.BlockActions = []*slack.BlockAction{}

	// Should not panic or error with empty actions.
	bot.handleBlockActions(context.Background(), callback)

	if len(daemon.getClosed()) != 0 {
		t.Error("expected no close calls for empty actions")
	}
}

func TestHandleBlockActions_ResolveOtherOpensModal(t *testing.T) {
	daemon := newMockDaemon()
	daemon.mu.Lock()
	daemon.beads["dec-other-1"] = &beadsapi.BeadDetail{
		ID:     "dec-other-1",
		Type:   "decision",
		Fields: map[string]string{"question": "What do you think?"},
	}
	daemon.mu.Unlock()

	var openViewCalled bool
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/views.open" {
			openViewCalled = true
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":   true,
			"view": map[string]any{"id": "V456"},
		})
	}))
	defer slackSrv.Close()
	bot := newTestBot(daemon, slackSrv)

	callback := slack.InteractionCallback{
		Channel: slack.Channel{
			GroupConversation: slack.GroupConversation{
				Conversation: slack.Conversation{ID: "C123"},
			},
		},
		User: slack.User{Name: "carol"},
	}
	callback.TriggerID = "trigger-456"
	callback.ActionCallback.BlockActions = []*slack.BlockAction{
		{ActionID: "resolve_other_dec-other-1"},
	}

	bot.handleBlockActions(context.Background(), callback)

	if !openViewCalled {
		t.Error("expected views.open to be called for other option")
	}
}

func TestHandleBlockActions_InvalidResolveValue(t *testing.T) {
	daemon := newMockDaemon()
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()
	bot := newTestBot(daemon, slackSrv)

	callback := slack.InteractionCallback{}
	callback.ActionCallback.BlockActions = []*slack.BlockAction{
		// Value missing the ":" separator — should be skipped.
		{ActionID: "resolve_dec-bad_1", Value: "no-colon-here"},
	}

	bot.handleBlockActions(context.Background(), callback)

	if len(daemon.getClosed()) != 0 {
		t.Error("expected no close calls for invalid resolve value")
	}
}

func TestHandleViewSubmission_ResolveDecision(t *testing.T) {
	daemon := newMockDaemon()
	daemon.mu.Lock()
	daemon.beads["dec-resolve-1"] = &beadsapi.BeadDetail{
		ID:     "dec-resolve-1",
		Type:   "decision",
		Fields: map[string]string{},
	}
	daemon.mu.Unlock()

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()
	bot := newTestBot(daemon, slackSrv)

	// Pre-populate message cache so updateMessageResolved can find it.
	bot.messages["dec-resolve-1"] = MessageRef{ChannelID: "C123", Timestamp: "1111.2222"}

	// Metadata now carries the option ID in the 2nd slot and the human display
	// label as the trailing 5th slot: beadID|chosenID|channelID|messageTS|display.
	callback := slack.InteractionCallback{
		User: slack.User{Name: "dave"},
		View: slack.View{
			CallbackID:      "resolve_decision",
			PrivateMetadata: "dec-resolve-1|alpha|C123|1111.2222|Option Alpha",
			State: &slack.ViewState{
				Values: map[string]map[string]slack.BlockAction{
					"rationale": {
						"rationale_input": {Value: "Looks correct"},
					},
				},
			},
		},
	}

	bot.handleViewSubmission(context.Background(), callback)

	closed := daemon.getClosed()
	if len(closed) != 1 {
		t.Fatalf("expected 1 close call, got %d", len(closed))
	}
	if closed[0].BeadID != "dec-resolve-1" {
		t.Errorf("expected beadID=dec-resolve-1, got %s", closed[0].BeadID)
	}
	// `chosen` stores the ID from the metadata, never the display label.
	if closed[0].Fields["chosen"] != "alpha" {
		t.Errorf("expected chosen=alpha (option id), got %s", closed[0].Fields["chosen"])
	}
	if closed[0].Fields["rationale"] != "Looks correct — @dave via Slack" {
		t.Errorf("unexpected rationale: %s", closed[0].Fields["rationale"])
	}
}

func TestHandleViewSubmission_ResolveDecisionNoRationale(t *testing.T) {
	daemon := newMockDaemon()
	daemon.mu.Lock()
	daemon.beads["dec-nr-1"] = &beadsapi.BeadDetail{
		ID:     "dec-nr-1",
		Type:   "decision",
		Fields: map[string]string{},
	}
	daemon.mu.Unlock()

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()
	bot := newTestBot(daemon, slackSrv)
	bot.messages["dec-nr-1"] = MessageRef{ChannelID: "C123", Timestamp: "1111.2222"}

	callback := slack.InteractionCallback{
		User: slack.User{Name: "eve"},
		View: slack.View{
			CallbackID:      "resolve_decision",
			PrivateMetadata: "dec-nr-1|Beta|C123|1111.2222",
			State: &slack.ViewState{
				Values: map[string]map[string]slack.BlockAction{
					"rationale": {
						"rationale_input": {Value: ""},
					},
				},
			},
		},
	}

	bot.handleViewSubmission(context.Background(), callback)

	closed := daemon.getClosed()
	if len(closed) != 1 {
		t.Fatalf("expected 1 close call, got %d", len(closed))
	}
	if closed[0].Fields["rationale"] != "Chosen by @eve via Slack" {
		t.Errorf("unexpected rationale: %s", closed[0].Fields["rationale"])
	}
}

func TestHandleViewSubmission_ResolveDecisionWithArtifactType(t *testing.T) {
	daemon := newMockDaemon()
	opts := []map[string]string{
		{"id": "a", "label": "Plan It", "artifact_type": "plan"},
	}
	optsJSON, _ := json.Marshal(opts)
	daemon.mu.Lock()
	daemon.beads["dec-art-1"] = &beadsapi.BeadDetail{
		ID:     "dec-art-1",
		Type:   "decision",
		Fields: map[string]string{"options": string(optsJSON)},
	}
	daemon.mu.Unlock()

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()
	bot := newTestBot(daemon, slackSrv)
	bot.messages["dec-art-1"] = MessageRef{ChannelID: "C123", Timestamp: "1111.2222"}

	callback := slack.InteractionCallback{
		User: slack.User{Name: "frank"},
		View: slack.View{
			CallbackID:      "resolve_decision",
			PrivateMetadata: "dec-art-1|a|C123|1111.2222|Plan It",
			State: &slack.ViewState{
				Values: map[string]map[string]slack.BlockAction{
					"rationale": {
						"rationale_input": {Value: ""},
					},
				},
			},
		},
	}

	bot.handleViewSubmission(context.Background(), callback)

	closed := daemon.getClosed()
	if len(closed) != 1 {
		t.Fatalf("expected 1 close call, got %d", len(closed))
	}
	// chosen stores the id; artifact_type resolves from that id.
	if closed[0].Fields["chosen"] != "a" {
		t.Errorf("expected chosen=a (option id), got %s", closed[0].Fields["chosen"])
	}
	if closed[0].Fields["required_artifact"] != "plan" {
		t.Errorf("expected required_artifact=plan, got %s", closed[0].Fields["required_artifact"])
	}
	if closed[0].Fields["artifact_status"] != "pending" {
		t.Errorf("expected artifact_status=pending, got %s", closed[0].Fields["artifact_status"])
	}
}

func TestHandleViewSubmission_ResolveOther(t *testing.T) {
	daemon := newMockDaemon()
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()
	bot := newTestBot(daemon, slackSrv)
	bot.messages["dec-other-sub-1"] = MessageRef{ChannelID: "C123", Timestamp: "1111.2222"}

	callback := slack.InteractionCallback{
		User: slack.User{Name: "grace"},
		View: slack.View{
			CallbackID:      "resolve_other",
			PrivateMetadata: "dec-other-sub-1|C123|1111.2222",
			State: &slack.ViewState{
				Values: map[string]map[string]slack.BlockAction{
					"response": {
						"response_input": {Value: "Custom answer here"},
					},
					"artifact_type": {
						"artifact_type_input": {
							SelectedOption: slack.OptionBlockObject{
								Value: "report",
							},
						},
					},
				},
			},
		},
	}

	bot.handleViewSubmission(context.Background(), callback)

	closed := daemon.getClosed()
	if len(closed) != 1 {
		t.Fatalf("expected 1 close call, got %d", len(closed))
	}
	if closed[0].BeadID != "dec-other-sub-1" {
		t.Errorf("expected beadID=dec-other-sub-1, got %s", closed[0].BeadID)
	}
	// Free-form text that matches no option lands in response_text with an
	// EMPTY chosen — never fabricating an option id from a sentence
	// (ka-0000000003).
	if closed[0].Fields["chosen"] != "" {
		t.Errorf("expected empty chosen for free-text response, got %q", closed[0].Fields["chosen"])
	}
	if closed[0].Fields["response_text"] != "Custom answer here" {
		t.Errorf("expected response_text='Custom answer here', got %q", closed[0].Fields["response_text"])
	}
	if closed[0].Fields["required_artifact"] != "report" {
		t.Errorf("expected required_artifact=report, got %s", closed[0].Fields["required_artifact"])
	}
}

func TestHandleViewSubmission_ResolveOtherNoArtifact(t *testing.T) {
	daemon := newMockDaemon()
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()
	bot := newTestBot(daemon, slackSrv)
	bot.messages["dec-other-noart"] = MessageRef{ChannelID: "C123", Timestamp: "1111.2222"}

	callback := slack.InteractionCallback{
		User: slack.User{Name: "heidi"},
		View: slack.View{
			CallbackID:      "resolve_other",
			PrivateMetadata: "dec-other-noart|C123|1111.2222",
			State: &slack.ViewState{
				Values: map[string]map[string]slack.BlockAction{
					"response": {
						"response_input": {Value: "My response"},
					},
					"artifact_type": {
						"artifact_type_input": {
							SelectedOption: slack.OptionBlockObject{
								Value: "none",
							},
						},
					},
				},
			},
		},
	}

	bot.handleViewSubmission(context.Background(), callback)

	closed := daemon.getClosed()
	if len(closed) != 1 {
		t.Fatalf("expected 1 close call, got %d", len(closed))
	}
	if _, ok := closed[0].Fields["required_artifact"]; ok {
		t.Errorf("expected no required_artifact for 'none', got %s", closed[0].Fields["required_artifact"])
	}
}

func TestHandleViewSubmission_ResolveOtherEmptyResponse(t *testing.T) {
	daemon := newMockDaemon()
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()
	bot := newTestBot(daemon, slackSrv)

	callback := slack.InteractionCallback{
		User: slack.User{Name: "ivan"},
		View: slack.View{
			CallbackID:      "resolve_other",
			PrivateMetadata: "dec-empty|C123|1111.2222",
			State: &slack.ViewState{
				Values: map[string]map[string]slack.BlockAction{
					"response": {
						"response_input": {Value: ""},
					},
				},
			},
		},
	}

	bot.handleViewSubmission(context.Background(), callback)

	// Empty response should be a no-op.
	if len(daemon.getClosed()) != 0 {
		t.Error("expected no close calls for empty response")
	}
}

func TestHandleViewSubmission_InvalidMetadata(t *testing.T) {
	daemon := newMockDaemon()
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()
	bot := newTestBot(daemon, slackSrv)

	callback := slack.InteractionCallback{
		User: slack.User{Name: "judy"},
		View: slack.View{
			CallbackID:      "resolve_decision",
			PrivateMetadata: "", // empty metadata
			State: &slack.ViewState{
				Values: map[string]map[string]slack.BlockAction{},
			},
		},
	}

	// Should not panic with invalid metadata.
	bot.handleViewSubmission(context.Background(), callback)

	if len(daemon.getClosed()) != 0 {
		t.Error("expected no close calls for invalid metadata")
	}
}

func TestHandleViewSubmission_UnknownCallbackID(t *testing.T) {
	daemon := newMockDaemon()
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()
	bot := newTestBot(daemon, slackSrv)

	callback := slack.InteractionCallback{
		View: slack.View{
			CallbackID: "unknown_callback",
			State:      &slack.ViewState{Values: map[string]map[string]slack.BlockAction{}},
		},
	}

	// Should not panic.
	bot.handleViewSubmission(context.Background(), callback)
	if len(daemon.getClosed()) != 0 {
		t.Error("expected no close calls for unknown callback")
	}
}

// TestNudgeDecisionAgent_FreeTextResponse verifies that a free-text answer
// (stored in response_text with an empty chosen — the ka-0000000003 shape)
// still reaches the agent: the nudge body must carry a "Response:" line and
// must NOT emit an empty "Chosen:" line.
func TestNudgeDecisionAgent_FreeTextResponse(t *testing.T) {
	var nudgeBody string
	coopSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/agent/nudge" {
			b, _ := io.ReadAll(r.Body)
			nudgeBody = string(b)
			w.WriteHeader(http.StatusOK)
			return
		}
		http.NotFound(w, r)
	}))
	defer coopSrv.Close()

	daemon := newMockDaemon()
	daemon.mu.Lock()
	daemon.beads["dec-freetext-1"] = &beadsapi.BeadDetail{
		ID:       "dec-freetext-1",
		Type:     "decision",
		Assignee: "ft-agent",
		Fields: map[string]string{
			"prompt":        "Which approach?",
			"response_text": "wait until hardening is proved",
		},
	}
	daemon.beads["ft-agent"] = &beadsapi.BeadDetail{
		ID:     "agent-bead-ft",
		Type:   "agent",
		Notes:  "coop_url: " + coopSrv.URL,
		Fields: map[string]string{"agent_state": "working"},
	}
	daemon.mu.Unlock()

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()
	bot := newTestBot(daemon, slackSrv)

	bot.nudgeDecisionAgent("dec-freetext-1")

	if !strings.Contains(nudgeBody, "wait until hardening is proved") {
		t.Errorf("nudge body missing the free-text response\nbody: %s", nudgeBody)
	}
	if strings.Contains(nudgeBody, "Chosen:") {
		t.Errorf("nudge body should not emit an empty Chosen line\nbody: %s", nudgeBody)
	}
}

func TestNudgeDecisionAgent(t *testing.T) {
	// Set up a coop server to receive the nudge.
	var nudgeReceived bool
	coopSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/agent/nudge" {
			nudgeReceived = true
			w.WriteHeader(http.StatusOK)
			return
		}
		http.NotFound(w, r)
	}))
	defer coopSrv.Close()

	daemon := newMockDaemon()
	daemon.mu.Lock()
	daemon.beads["dec-nudge-1"] = &beadsapi.BeadDetail{
		ID:       "dec-nudge-1",
		Type:     "decision",
		Assignee: "test-agent",
		Fields:   map[string]string{},
	}
	// FindAgentBead looks up by agent name.
	daemon.beads["test-agent"] = &beadsapi.BeadDetail{
		ID:     "agent-bead-1",
		Type:   "agent",
		Notes:  "coop_url: " + coopSrv.URL,
		Fields: map[string]string{"agent_state": "working"},
	}
	daemon.mu.Unlock()

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()
	bot := newTestBot(daemon, slackSrv)

	bot.nudgeDecisionAgent("dec-nudge-1")

	if !nudgeReceived {
		t.Error("expected nudge to be sent to coop server")
	}
}

func TestNudgeDecisionAgent_NoAssignee(t *testing.T) {
	daemon := newMockDaemon()
	daemon.mu.Lock()
	daemon.beads["dec-no-agent"] = &beadsapi.BeadDetail{
		ID:     "dec-no-agent",
		Type:   "decision",
		Fields: map[string]string{},
	}
	daemon.mu.Unlock()

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()
	bot := newTestBot(daemon, slackSrv)

	// Should not panic with no assignee or created_by.
	bot.nudgeDecisionAgent("dec-no-agent")
}

func TestNudgeDecisionAgent_NoCoopURL(t *testing.T) {
	daemon := newMockDaemon()
	daemon.mu.Lock()
	daemon.beads["dec-no-coop"] = &beadsapi.BeadDetail{
		ID:       "dec-no-coop",
		Type:     "decision",
		Assignee: "agent-no-coop",
		Fields:   map[string]string{},
	}
	// Agent found by FindAgentBead but has no coop_url.
	daemon.beads["agent-no-coop"] = &beadsapi.BeadDetail{
		ID:     "agent-no-coop-bead",
		Type:   "agent",
		Notes:  "",
		Fields: map[string]string{"agent_state": "working"},
	}
	daemon.mu.Unlock()

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()
	bot := newTestBot(daemon, slackSrv)

	// Should not panic with no coop_url.
	bot.nudgeDecisionAgent("dec-no-coop")
}

func TestNudgeDecisionAgent_UsesOwnContext(t *testing.T) {
	// Verify nudgeDecisionAgent creates its own context (not inherited).
	// This is the regression test for the bug where the goroutine used the
	// HTTP request context which was cancelled when the handler returned.
	var nudgeReceived bool
	coopSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/agent/nudge" {
			nudgeReceived = true
			w.WriteHeader(http.StatusOK)
			return
		}
		http.NotFound(w, r)
	}))
	defer coopSrv.Close()

	daemon := newMockDaemon()
	daemon.mu.Lock()
	daemon.beads["dec-ctx"] = &beadsapi.BeadDetail{
		ID:       "dec-ctx",
		Type:     "decision",
		Assignee: "ctx-agent",
		Fields:   map[string]string{},
	}
	daemon.beads["ctx-agent"] = &beadsapi.BeadDetail{
		ID:     "agent-ctx-bead",
		Type:   "agent",
		Notes:  "coop_url: " + coopSrv.URL,
		Fields: map[string]string{"agent_state": "working"},
	}
	daemon.mu.Unlock()

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()
	bot := newTestBot(daemon, slackSrv)

	// nudgeDecisionAgent doesn't take a context parameter — it creates its own.
	// This means it works even when launched from a goroutine after a handler returns.
	bot.nudgeDecisionAgent("dec-ctx")

	if !nudgeReceived {
		t.Error("expected nudge to succeed with its own context")
	}
}

func TestNudgeDecisionAgent_FallsBackToCreatedBy(t *testing.T) {
	var nudgeReceived bool
	coopSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/agent/nudge" {
			nudgeReceived = true
			w.WriteHeader(http.StatusOK)
			return
		}
		http.NotFound(w, r)
	}))
	defer coopSrv.Close()

	daemon := newMockDaemon()
	daemon.mu.Lock()
	daemon.beads["dec-created-by"] = &beadsapi.BeadDetail{
		ID:        "dec-created-by",
		Type:      "decision",
		CreatedBy: "fallback-agent",
		Fields:    map[string]string{},
	}
	daemon.beads["fallback-agent"] = &beadsapi.BeadDetail{
		ID:     "agent-fallback-bead",
		Type:   "agent",
		Notes:  "coop_url: " + coopSrv.URL,
		Fields: map[string]string{"agent_state": "working"},
	}
	daemon.mu.Unlock()

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()
	bot := newTestBot(daemon, slackSrv)

	bot.nudgeDecisionAgent("dec-created-by")

	if !nudgeReceived {
		t.Error("expected nudge to use CreatedBy as fallback when Assignee is empty")
	}
}

func TestReturnToAgentForEscalation(t *testing.T) {
	nudgeReceived := false
	var nudgeBody string

	coopSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/agent/nudge" {
			nudgeReceived = true
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			nudgeBody = body["message"]
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer coopSrv.Close()

	daemon := newMockDaemon()
	daemon.mu.Lock()
	daemon.beads["dec-esc-1"] = &beadsapi.BeadDetail{
		ID:     "dec-esc-1",
		Type:   "decision",
		Status: "open",
		Fields: map[string]string{
			"prompt":                   "Need AWS credentials to access prod S3 bucket",
			"requesting_agent_bead_id": "test-agent",
		},
		Labels: []string{"project:gardenio"},
	}
	daemon.beads["test-agent"] = &beadsapi.BeadDetail{
		ID:     "agent-bead-1",
		Type:   "agent",
		Notes:  "coop_url: " + coopSrv.URL,
		Fields: map[string]string{"agent": "test-agent", "agent_state": "working"},
	}
	daemon.mu.Unlock()

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()
	bot := newTestBot(daemon, slackSrv)

	callback := slack.InteractionCallback{
		Channel: slack.Channel{
			GroupConversation: slack.GroupConversation{
				Conversation: slack.Conversation{ID: "C123"},
			},
		},
		User: slack.User{Name: "operator"},
	}
	callback.Message.Msg.Timestamp = "1234.5678"

	bot.returnToAgentForEscalation(context.Background(), "dec-esc-1", callback)

	// Verify decision was closed with correct fields.
	closed := daemon.getClosed()
	if len(closed) != 1 {
		t.Fatalf("expected 1 close call, got %d", len(closed))
	}
	if closed[0].BeadID != "dec-esc-1" {
		t.Errorf("expected beadID=dec-esc-1, got %s", closed[0].BeadID)
	}
	if closed[0].Fields["chosen"] != "returned_to_agent_for_escalation" {
		t.Errorf("expected chosen=returned_to_agent_for_escalation, got %s", closed[0].Fields["chosen"])
	}
	if closed[0].Fields["rationale"] == "" {
		t.Error("expected rationale to be set")
	}

	// Verify nudge was sent to the agent.
	if !nudgeReceived {
		t.Fatal("expected nudge to be sent to the agent")
	}
	if nudgeBody == "" {
		t.Fatal("expected nudge body to be non-empty")
	}
	// Verify nudge contains key instructions.
	for _, want := range []string{"gb escalation create", "dec-esc-1", "AWS credentials"} {
		if !strings.Contains(nudgeBody, want) {
			t.Errorf("nudge body missing %q:\n%s", want, nudgeBody)
		}
	}
}

func TestReturnToAgentForEscalation_ClosedDecisionSkipped(t *testing.T) {
	daemon := newMockDaemon()
	daemon.mu.Lock()
	daemon.beads["dec-closed"] = &beadsapi.BeadDetail{
		ID:     "dec-closed",
		Type:   "decision",
		Status: "closed",
		Fields: map[string]string{"prompt": "Already resolved"},
	}
	daemon.mu.Unlock()

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()
	bot := newTestBot(daemon, slackSrv)

	callback := slack.InteractionCallback{
		User: slack.User{Name: "operator"},
	}

	bot.returnToAgentForEscalation(context.Background(), "dec-closed", callback)

	closed := daemon.getClosed()
	if len(closed) != 0 {
		t.Errorf("expected no close calls for already-closed decision, got %d", len(closed))
	}
}

func TestReturnToAgentForEscalation_FallbackToAssignee(t *testing.T) {
	nudgeReceived := false

	coopSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/agent/nudge" {
			nudgeReceived = true
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer coopSrv.Close()

	daemon := newMockDaemon()
	daemon.mu.Lock()
	daemon.beads["dec-no-req"] = &beadsapi.BeadDetail{
		ID:       "dec-no-req",
		Type:     "decision",
		Status:   "open",
		Assignee: "fallback-agent",
		Fields:   map[string]string{"prompt": "Need help"},
	}
	daemon.beads["fallback-agent"] = &beadsapi.BeadDetail{
		ID:     "agent-fb",
		Type:   "agent",
		Notes:  "coop_url: " + coopSrv.URL,
		Fields: map[string]string{"agent": "fallback-agent", "agent_state": "working"},
	}
	daemon.mu.Unlock()

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()
	bot := newTestBot(daemon, slackSrv)

	callback := slack.InteractionCallback{
		Channel: slack.Channel{
			GroupConversation: slack.GroupConversation{
				Conversation: slack.Conversation{ID: "C123"},
			},
		},
		User: slack.User{Name: "operator"},
	}
	callback.Message.Msg.Timestamp = "1234.5678"

	bot.returnToAgentForEscalation(context.Background(), "dec-no-req", callback)

	if !nudgeReceived {
		t.Error("expected nudge to fallback to Assignee when requesting_agent_bead_id is empty")
	}
}
