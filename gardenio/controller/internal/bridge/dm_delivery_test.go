package bridge

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

func TestDMDelivery_DeliversDM(t *testing.T) {
	daemon := newMockDaemon()
	var postedTexts []string
	var mu sync.Mutex

	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/conversations.open":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok":      true,
				"channel": map[string]any{"id": "D-dm-test"},
			})
		case "/chat.postMessage":
			mu.Lock()
			postedTexts = append(postedTexts, r.FormValue("text"))
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok":      true,
				"channel": "D-dm-test",
				"ts":      "1234.5678",
			})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		}
	}))
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	delivery := NewDMDelivery(DMDeliveryConfig{
		Daemon: daemon,
		Bot:    bot,
		Logger: slog.Default(),
	})

	// Simulate a dm bead created event.
	payload := marshalSSEBeadPayload(BeadEvent{
		ID:   "dm-001",
		Type: "dm",
		Fields: map[string]string{
			"target_user":  "U-ENGINEER",
			"source_agent": "test-bot",
			"subject":      "Need review",
			"body":         "Please review PR #42",
		},
	})

	delivery.handleCreated(context.Background(), payload)

	mu.Lock()
	defer mu.Unlock()
	if len(postedTexts) != 1 {
		t.Fatalf("expected 1 posted message, got %d", len(postedTexts))
	}
	if postedTexts[0] == "" {
		t.Error("expected non-empty message text")
	}
}

func TestDMDelivery_SkipsNonDMBeads(t *testing.T) {
	daemon := newMockDaemon()
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	delivery := NewDMDelivery(DMDeliveryConfig{
		Daemon: daemon,
		Bot:    bot,
		Logger: slog.Default(),
	})

	// Send a non-dm bead — should be ignored.
	payload := marshalSSEBeadPayload(BeadEvent{
		ID:   "msg-001",
		Type: "message",
		Fields: map[string]string{
			"source_agent": "test-bot",
			"text":         "hello",
		},
	})

	delivery.handleCreated(context.Background(), payload)
	// No crash, no delivery — success.
}

func TestDMDelivery_DeduplicatesOnReconnect(t *testing.T) {
	daemon := newMockDaemon()
	var postCount int
	var mu sync.Mutex

	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/conversations.open":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok":      true,
				"channel": map[string]any{"id": "D-dm-test"},
			})
		case "/chat.postMessage":
			mu.Lock()
			postCount++
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok":      true,
				"channel": "D-dm-test",
				"ts":      "1234.5678",
			})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		}
	}))
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	delivery := NewDMDelivery(DMDeliveryConfig{
		Daemon: daemon,
		Bot:    bot,
		Logger: slog.Default(),
	})

	payload := marshalSSEBeadPayload(BeadEvent{
		ID:   "dm-dedup",
		Type: "dm",
		Fields: map[string]string{
			"target_user": "U-ENGINEER",
			"body":        "Hello",
		},
	})

	// First delivery.
	delivery.handleCreated(context.Background(), payload)
	// Second delivery (same ID — should be deduped).
	delivery.handleCreated(context.Background(), payload)

	mu.Lock()
	defer mu.Unlock()
	if postCount != 1 {
		t.Errorf("expected 1 delivery (dedup), got %d", postCount)
	}
}

func TestDMDelivery_MissingTargetUser(t *testing.T) {
	daemon := newMockDaemon()
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	delivery := NewDMDelivery(DMDeliveryConfig{
		Daemon: daemon,
		Bot:    bot,
		Logger: slog.Default(),
	})

	payload := marshalSSEBeadPayload(BeadEvent{
		ID:   "dm-no-target",
		Type: "dm",
		Fields: map[string]string{
			"body": "Hello",
		},
	})

	// Should not crash, should skip gracefully.
	delivery.handleCreated(context.Background(), payload)
}

func TestDMDelivery_GroupDM(t *testing.T) {
	daemon := newMockDaemon()
	var conversationUsers []string
	var postedTexts []string
	var mu sync.Mutex

	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/conversations.open":
			mu.Lock()
			conversationUsers = append(conversationUsers, r.FormValue("users"))
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok":      true,
				"channel": map[string]any{"id": "G-group-test"},
			})
		case "/chat.postMessage":
			mu.Lock()
			postedTexts = append(postedTexts, r.FormValue("text"))
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok":      true,
				"channel": "G-group-test",
				"ts":      "3333.4444",
			})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		}
	}))
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	delivery := NewDMDelivery(DMDeliveryConfig{
		Daemon: daemon,
		Bot:    bot,
		Logger: slog.Default(),
	})

	// Simulate a group dm bead with target_users field.
	payload := marshalSSEBeadPayload(BeadEvent{
		ID:   "dm-group-001",
		Type: "dm",
		Fields: map[string]string{
			"target_user":  "U-ALICE",
			"target_users": "U-ALICE,U-BOB,U-CHARLIE",
			"source_agent": "test-bot",
			"subject":      "Team update",
			"body":         "Sprint retro at 3pm",
		},
	})

	delivery.handleCreated(context.Background(), payload)

	mu.Lock()
	defer mu.Unlock()

	if len(postedTexts) != 1 {
		t.Fatalf("expected 1 posted message, got %d", len(postedTexts))
	}
	if postedTexts[0] == "" {
		t.Error("expected non-empty message text")
	}

	// Verify conversations.open was called with all three users.
	if len(conversationUsers) != 1 {
		t.Fatalf("expected 1 conversations.open call, got %d", len(conversationUsers))
	}
	if conversationUsers[0] != "U-ALICE,U-BOB,U-CHARLIE" {
		t.Errorf("expected conversations.open users=U-ALICE,U-BOB,U-CHARLIE, got %s", conversationUsers[0])
	}
}

func TestDMDelivery_GroupDM_SingleTargetFallsBack(t *testing.T) {
	// When target_users has only one user, should use regular SendDM path.
	daemon := newMockDaemon()
	var conversationUsers []string
	var mu sync.Mutex

	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/conversations.open":
			mu.Lock()
			conversationUsers = append(conversationUsers, r.FormValue("users"))
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok":      true,
				"channel": map[string]any{"id": "D-single"},
			})
		case "/chat.postMessage":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok":      true,
				"channel": "D-single",
				"ts":      "4444.5555",
			})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		}
	}))
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	delivery := NewDMDelivery(DMDeliveryConfig{
		Daemon: daemon,
		Bot:    bot,
		Logger: slog.Default(),
	})

	// target_users with a single user — should use SendDM, not SendGroupDM.
	payload := marshalSSEBeadPayload(BeadEvent{
		ID:   "dm-group-single",
		Type: "dm",
		Fields: map[string]string{
			"target_user":  "U-ALICE",
			"target_users": "U-ALICE",
			"source_agent": "test-bot",
			"body":         "Just for you",
		},
	})

	delivery.handleCreated(context.Background(), payload)

	mu.Lock()
	defer mu.Unlock()

	// Should use regular SendDM path (single user in conversations.open).
	if len(conversationUsers) != 1 {
		t.Fatalf("expected 1 conversations.open call, got %d", len(conversationUsers))
	}
	if conversationUsers[0] != "U-ALICE" {
		t.Errorf("expected conversations.open users=U-ALICE, got %s", conversationUsers[0])
	}
}

func TestBuildDMText_SubjectAndBody(t *testing.T) {
	bead := &BeadEvent{
		Fields: map[string]string{
			"subject":      "Need review",
			"body":         "Please review PR #42",
			"source_agent": "my-bot",
		},
	}
	text := buildDMText(bead)
	if text == "" {
		t.Error("expected non-empty text")
	}
	// Should contain formatted subject and body.
	if !strings.Contains(text, "Need review") {
		t.Error("expected text to contain subject")
	}
	if !strings.Contains(text, "PR #42") {
		t.Error("expected text to contain body")
	}
	if !strings.Contains(text, "my-bot") {
		t.Error("expected text to contain agent name")
	}
}

func TestBuildDMText_BodyOnly(t *testing.T) {
	bead := &BeadEvent{
		Fields: map[string]string{
			"body": "Quick question about the deploy",
		},
	}
	text := buildDMText(bead)
	if text == "" {
		t.Error("expected non-empty text")
	}
	if !strings.Contains(text, "Quick question") {
		t.Error("expected text to contain body")
	}
}

func TestBuildDMText_FallbackToTitle(t *testing.T) {
	bead := &BeadEvent{
		Title:  "Fallback title",
		Fields: map[string]string{},
	}
	text := buildDMText(bead)
	if text != "Fallback title" {
		t.Errorf("expected fallback to title, got %q", text)
	}
}

// ── Question DM tests ──────────────────────────────────────────────

func TestDMDelivery_QuestionModeStaysOpen(t *testing.T) {
	daemon := newMockDaemon()
	var postedTexts []string
	var mu sync.Mutex

	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/conversations.open":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok":      true,
				"channel": map[string]any{"id": "D-question-test"},
			})
		case "/chat.postMessage":
			mu.Lock()
			postedTexts = append(postedTexts, r.FormValue("text"))
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok":      true,
				"channel": "D-question-test",
				"ts":      "9999.0001",
			})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		}
	}))
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	delivery := NewDMDelivery(DMDeliveryConfig{
		Daemon: daemon,
		Bot:    bot,
		Logger: slog.Default(),
	})

	payload := marshalSSEBeadPayload(BeadEvent{
		ID:   "dm-q-001",
		Type: "dm",
		Fields: map[string]string{
			"target_user":     "U-ENGINEER",
			"source_agent":    "my-agent",
			"body":            "What database should we use?",
			"dm_mode":         "question",
			"timeout_minutes": "0",
		},
	})

	delivery.handleCreated(context.Background(), payload)

	mu.Lock()
	defer mu.Unlock()
	if len(postedTexts) != 1 {
		t.Fatalf("expected 1 posted message, got %d", len(postedTexts))
	}

	// Verify the bead was NOT closed (question DMs stay open for reply).
	bead := daemon.beads["dm-q-001"]
	if bead != nil && bead.Status == "closed" {
		t.Error("question DM bead should stay open, but was closed")
	}
}

func TestDMDelivery_HandleReply(t *testing.T) {
	daemon := newMockDaemon()
	// Seed the bead so UpdateBeadFields works during reply handling.
	daemon.beads["dm-q-reply"] = &beadsapi.BeadDetail{
		ID:     "dm-q-reply",
		Type:   "dm",
		Status: "open",
		Fields: map[string]string{"dm_mode": "question", "source_agent": "test-agent"},
	}

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	delivery := NewDMDelivery(DMDeliveryConfig{
		Daemon: daemon,
		Bot:    bot,
		Logger: slog.Default(),
	})

	// Track a question manually.
	delivery.trackQuestion("dm-q-reply", "D-ch-001", "1000.0001", "test-agent", "U-ENGINEER")

	// Simulate a reply.
	handled := delivery.HandleReply(context.Background(), "D-ch-001", "1000.0001", "Use PostgreSQL", "U-ENGINEER")
	if !handled {
		t.Fatal("expected reply to be handled")
	}

	// Verify bead was updated with reply fields.
	bead := daemon.beads["dm-q-reply"]
	if bead == nil {
		t.Fatal("bead not found")
	}
	if bead.Fields["replied"] != "true" {
		t.Error("expected replied=true")
	}
	if bead.Fields["reply_text"] != "Use PostgreSQL" {
		t.Errorf("expected reply_text='Use PostgreSQL', got %q", bead.Fields["reply_text"])
	}
	if bead.Fields["reply_ts"] != "1000.0001" {
		t.Errorf("expected reply_ts='1000.0001', got %q", bead.Fields["reply_ts"])
	}
	if bead.Fields["reply_user"] != "U-ENGINEER" {
		t.Errorf("expected reply_user='U-ENGINEER', got %q", bead.Fields["reply_user"])
	}

	// Verify bead was NOT closed (stays open for follow-up replies).
	for _, c := range daemon.closed {
		if c.BeadID == "dm-q-reply" {
			t.Error("bead should not be closed after first reply")
		}
	}
}

func TestDMDelivery_HandleReplyNoMatch(t *testing.T) {
	daemon := newMockDaemon()
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	delivery := NewDMDelivery(DMDeliveryConfig{
		Daemon: daemon,
		Bot:    bot,
		Logger: slog.Default(),
	})

	// Reply on an untracked thread — should return false.
	handled := delivery.HandleReply(context.Background(), "D-unknown", "0000.0000", "hello", "U-SOMEONE")
	if handled {
		t.Error("expected reply to not be handled for untracked thread")
	}
}

func TestDMDelivery_MultiReplyStoresComment(t *testing.T) {
	daemon := newMockDaemon()
	daemon.beads["dm-q-multi"] = &beadsapi.BeadDetail{
		ID:     "dm-q-multi",
		Type:   "dm",
		Status: "open",
		Fields: map[string]string{"dm_mode": "question", "source_agent": "agent-x"},
	}

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	delivery := NewDMDelivery(DMDeliveryConfig{
		Daemon: daemon,
		Bot:    bot,
		Logger: slog.Default(),
	})

	delivery.trackQuestion("dm-q-multi", "D-ch-002", "2000.0001", "agent-x", "U-PERSON")

	// First reply: sets bead fields.
	handled1 := delivery.HandleReply(context.Background(), "D-ch-002", "2000.0001", "First reply", "U-PERSON")
	if !handled1 {
		t.Fatal("expected first reply to be handled")
	}

	// Verify first reply set fields.
	bead := daemon.beads["dm-q-multi"]
	if bead.Fields["replied"] != "true" {
		t.Error("expected replied=true after first reply")
	}
	if bead.Fields["reply_text"] != "First reply" {
		t.Errorf("expected reply_text='First reply', got %q", bead.Fields["reply_text"])
	}

	// Second reply: should be handled and stored as a comment.
	handled2 := delivery.HandleReply(context.Background(), "D-ch-002", "2000.0001", "Follow-up info", "U-PERSON")
	if !handled2 {
		t.Fatal("expected second reply to be handled")
	}

	// Verify second reply was stored as a comment.
	comments := daemon.getComments()
	if len(comments) != 1 {
		t.Fatalf("expected 1 comment, got %d", len(comments))
	}
	if comments[0].BeadID != "dm-q-multi" {
		t.Errorf("expected comment on dm-q-multi, got %q", comments[0].BeadID)
	}
	if !strings.Contains(comments[0].Text, "Follow-up info") {
		t.Errorf("expected comment to contain 'Follow-up info', got %q", comments[0].Text)
	}

	// Verify bead was NOT closed (stays open for more replies).
	for _, c := range daemon.closed {
		if c.BeadID == "dm-q-multi" {
			t.Error("bead should not be closed after replies")
		}
	}
}

func TestBuildQuestionDMText_WithContext(t *testing.T) {
	bead := &BeadEvent{
		Fields: map[string]string{
			"body":         "What database should we use?",
			"source_agent": "my-agent",
			"task_context": "gd-TASK123",
		},
	}
	// Vibe mode carries the full framing.
	text := buildQuestionDMText(Garden, bead, true)
	if !strings.Contains(text, "Question from agent my-agent") {
		t.Error("expected text to contain agent name")
	}
	if !strings.Contains(text, "gd-TASK123") {
		t.Error("expected text to contain task context")
	}
	if !strings.Contains(text, "What database should we use?") {
		t.Error("expected text to contain question")
	}
	if !strings.Contains(text, "Reply in this thread") {
		t.Error("expected text to contain reply prompt")
	}

	// Simple (default) mode trims the preamble to the ask + a reply hint, but
	// keeps the question itself and the reply affordance actionable.
	terse := buildQuestionDMText(Garden, bead, false)
	if !strings.Contains(terse, "What database should we use?") {
		t.Error("terse text must still contain the question")
	}
	if !strings.Contains(terse, "Reply in this thread") {
		t.Error("terse text must keep the reply prompt (reply tracking guardrail)")
	}
	if strings.Contains(terse, "gd-TASK123") {
		t.Error("terse text should drop the task-context preamble")
	}
	if len(terse) >= len(text) {
		t.Errorf("terse (%d) should be shorter than full (%d)", len(terse), len(text))
	}
}

func TestBuildQuestionDMText_NoContext(t *testing.T) {
	bead := &BeadEvent{
		Fields: map[string]string{
			"body":         "Quick question",
			"source_agent": "bot-1",
		},
	}
	text := buildQuestionDMText(Garden, bead, true)
	if !strings.Contains(text, "Quick question") {
		t.Error("expected text to contain question")
	}
	if strings.Contains(text, "Context:") {
		t.Error("expected no context line when task_context is empty")
	}
}

func TestDMDelivery_QuestionIntegration(t *testing.T) {
	// End-to-end test: deliver a question DM, then simulate a reply.
	daemon := newMockDaemon()
	// Seed the bead so UpdateBeadFields works during reply handling.
	daemon.beads["dm-int-001"] = &beadsapi.BeadDetail{
		ID:     "dm-int-001",
		Type:   "dm",
		Status: "open",
		Fields: map[string]string{
			"target_user":     "U-ALICE",
			"source_agent":    "research-agent",
			"body":            "Which API version should I target?",
			"dm_mode":         "question",
			"timeout_minutes": "0",
		},
	}

	var postedTexts []string
	var mu sync.Mutex

	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/conversations.open":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok":      true,
				"channel": map[string]any{"id": "D-int-test"},
			})
		case "/chat.postMessage":
			mu.Lock()
			postedTexts = append(postedTexts, r.FormValue("text"))
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok":      true,
				"channel": "D-int-test",
				"ts":      "5555.0001",
			})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		}
	}))
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	delivery := NewDMDelivery(DMDeliveryConfig{
		Daemon: daemon,
		Bot:    bot,
		Logger: slog.Default(),
	})

	// Step 1: Deliver a question DM.
	payload := marshalSSEBeadPayload(BeadEvent{
		ID:   "dm-int-001",
		Type: "dm",
		Fields: map[string]string{
			"target_user":     "U-ALICE",
			"source_agent":    "research-agent",
			"body":            "Which API version should I target?",
			"dm_mode":         "question",
			"timeout_minutes": "0",
		},
	})
	delivery.handleCreated(context.Background(), payload)

	mu.Lock()
	if len(postedTexts) != 1 {
		t.Fatalf("expected 1 message after delivery, got %d", len(postedTexts))
	}
	mu.Unlock()

	// Step 2: Simulate the engineer replying in the DM thread.
	handled := delivery.HandleReply(context.Background(), "D-int-test", "5555.0001", "Use v3", "U-ALICE")
	if !handled {
		t.Fatal("expected reply to be handled")
	}

	// Step 3: Verify bead updated with reply.
	bead := daemon.beads["dm-int-001"]
	if bead == nil {
		t.Fatal("bead not found after reply")
	}
	if bead.Fields["replied"] != "true" {
		t.Error("expected replied=true after reply")
	}
	if bead.Fields["reply_text"] != "Use v3" {
		t.Errorf("expected reply_text='Use v3', got %q", bead.Fields["reply_text"])
	}
	if bead.Fields["reply_ts"] != "5555.0001" {
		t.Errorf("expected reply_ts='5555.0001', got %q", bead.Fields["reply_ts"])
	}

	// Step 4: Verify bead stays open (not closed on first reply).
	for _, c := range daemon.closed {
		if c.BeadID == "dm-int-001" {
			t.Error("bead should not be closed after first reply")
		}
	}

	// Step 5: Verify confirmation posted in thread.
	mu.Lock()
	defer mu.Unlock()
	if len(postedTexts) < 2 {
		t.Fatal("expected confirmation message posted after reply")
	}
	if !strings.Contains(postedTexts[1], "Reply captured") {
		t.Errorf("expected confirmation message, got %q", postedTexts[1])
	}
}

// ── Review DM tests ────────────────────────────────────────────────

func TestBuildReviewDMText_WithAllFields(t *testing.T) {
	bead := &BeadEvent{
		Fields: map[string]string{
			"mr_url":             "https://gitlab.com/org/repo/-/merge_requests/42",
			"source_agent":       "build-agent",
			"task_context":       "gd-TASK456",
			"mr_title":           "fix: handle nil pointer in auth",
			"mr_pipeline_status": "success",
			"body":               "Please review ASAP",
		},
	}
	// Vibe mode carries all fields.
	text := buildReviewDMText(Garden, bead, true)
	if !strings.Contains(text, "MR review request from agent build-agent") {
		t.Error("expected text to contain agent name")
	}
	if !strings.Contains(text, "gd-TASK456") {
		t.Error("expected text to contain task context")
	}
	if !strings.Contains(text, "fix: handle nil pointer in auth") {
		t.Error("expected text to contain MR title")
	}
	if !strings.Contains(text, "merge_requests/42") {
		t.Error("expected text to contain MR URL")
	}
	if !strings.Contains(text, "Pipeline: success") {
		t.Error("expected text to contain pipeline status")
	}
	if !strings.Contains(text, "Please review ASAP") {
		t.Error("expected text to contain custom message")
	}
	if !strings.Contains(text, "Reply in this thread") {
		t.Error("expected text to contain reply prompt")
	}

	// Simple (default) mode keeps the actionable core — MR title + URL + reply
	// hint — and drops the context/pipeline/custom-message framing.
	terse := buildReviewDMText(Garden, bead, false)
	if !strings.Contains(terse, "merge_requests/42") {
		t.Error("terse review must still contain the MR URL")
	}
	if !strings.Contains(terse, "fix: handle nil pointer in auth") {
		t.Error("terse review must still contain the MR title")
	}
	if !strings.Contains(terse, "Reply in this thread") {
		t.Error("terse review must keep the reply prompt (reply tracking guardrail)")
	}
	if strings.Contains(terse, "Pipeline: success") {
		t.Error("terse review should drop the pipeline line")
	}
	if strings.Contains(terse, "gd-TASK456") {
		t.Error("terse review should drop the task-context preamble")
	}
}

func TestBuildReviewDMText_MinimalFields(t *testing.T) {
	bead := &BeadEvent{
		Fields: map[string]string{
			"mr_url":       "https://gitlab.com/org/repo/-/merge_requests/10",
			"source_agent": "my-agent",
		},
	}
	text := buildReviewDMText(Garden, bead, true)
	if !strings.Contains(text, "MR review request from agent my-agent") {
		t.Error("expected text to contain agent name")
	}
	if !strings.Contains(text, "merge_requests/10") {
		t.Error("expected text to contain MR URL")
	}
	if strings.Contains(text, "Context:") {
		t.Error("expected no context line when task_context is empty")
	}
	if strings.Contains(text, "Pipeline:") {
		t.Error("expected no pipeline line when mr_pipeline_status is empty")
	}
}

func TestDMDelivery_ReviewModeStaysOpen(t *testing.T) {
	daemon := newMockDaemon()
	var postedTexts []string
	var mu sync.Mutex

	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/conversations.open":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok":      true,
				"channel": map[string]any{"id": "D-review-test"},
			})
		case "/chat.postMessage":
			mu.Lock()
			postedTexts = append(postedTexts, r.FormValue("text"))
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok":      true,
				"channel": "D-review-test",
				"ts":      "7777.0001",
			})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		}
	}))
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	delivery := NewDMDelivery(DMDeliveryConfig{
		Daemon: daemon,
		Bot:    bot,
		Logger: slog.Default(),
	})

	payload := marshalSSEBeadPayload(BeadEvent{
		ID:   "dm-rev-001",
		Type: "dm",
		Fields: map[string]string{
			"target_user":     "U-REVIEWER",
			"source_agent":    "build-agent",
			"mr_url":          "https://gitlab.com/org/repo/-/merge_requests/42",
			"dm_mode":         "review",
			"timeout_minutes": "0",
		},
	})

	delivery.handleCreated(context.Background(), payload)

	mu.Lock()
	defer mu.Unlock()
	if len(postedTexts) != 1 {
		t.Fatalf("expected 1 posted message, got %d", len(postedTexts))
	}

	// Verify the message contains review-specific content.
	if !strings.Contains(postedTexts[0], "MR review request") {
		t.Errorf("expected review DM text, got %q", postedTexts[0])
	}

	// Verify the bead was NOT closed (review DMs stay open for reply).
	bead := daemon.beads["dm-rev-001"]
	if bead != nil && bead.Status == "closed" {
		t.Error("review DM bead should stay open, but was closed")
	}
}

func TestDMDelivery_ReviewReplyIntegration(t *testing.T) {
	daemon := newMockDaemon()
	daemon.beads["dm-rev-int"] = &beadsapi.BeadDetail{
		ID:     "dm-rev-int",
		Type:   "dm",
		Status: "open",
		Fields: map[string]string{
			"target_user":     "U-REVIEWER",
			"source_agent":    "build-agent",
			"mr_url":          "https://gitlab.com/org/repo/-/merge_requests/42",
			"dm_mode":         "review",
			"timeout_minutes": "0",
		},
	}

	var postedTexts []string
	var mu sync.Mutex

	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/conversations.open":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok":      true,
				"channel": map[string]any{"id": "D-rev-int"},
			})
		case "/chat.postMessage":
			mu.Lock()
			postedTexts = append(postedTexts, r.FormValue("text"))
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok":      true,
				"channel": "D-rev-int",
				"ts":      "8888.0001",
			})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		}
	}))
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	delivery := NewDMDelivery(DMDeliveryConfig{
		Daemon: daemon,
		Bot:    bot,
		Logger: slog.Default(),
	})

	// Step 1: Deliver a review DM.
	payload := marshalSSEBeadPayload(BeadEvent{
		ID:   "dm-rev-int",
		Type: "dm",
		Fields: map[string]string{
			"target_user":     "U-REVIEWER",
			"source_agent":    "build-agent",
			"mr_url":          "https://gitlab.com/org/repo/-/merge_requests/42",
			"dm_mode":         "review",
			"timeout_minutes": "0",
		},
	})
	delivery.handleCreated(context.Background(), payload)

	mu.Lock()
	if len(postedTexts) != 1 {
		t.Fatalf("expected 1 message after delivery, got %d", len(postedTexts))
	}
	mu.Unlock()

	// Step 2: Simulate the reviewer replying in the DM thread.
	handled := delivery.HandleReply(context.Background(), "D-rev-int", "8888.0001", "LGTM, approved!", "U-REVIEWER")
	if !handled {
		t.Fatal("expected reply to be handled")
	}

	// Step 3: Verify bead updated with reply.
	bead := daemon.beads["dm-rev-int"]
	if bead == nil {
		t.Fatal("bead not found after reply")
	}
	if bead.Fields["replied"] != "true" {
		t.Error("expected replied=true after reply")
	}
	if bead.Fields["reply_text"] != "LGTM, approved!" {
		t.Errorf("expected reply_text='LGTM, approved!', got %q", bead.Fields["reply_text"])
	}

	// Step 4: Verify bead stays open (not closed on first reply).
	for _, c := range daemon.closed {
		if c.BeadID == "dm-rev-int" {
			t.Error("bead should not be closed after first reply")
		}
	}

	// Step 5: Verify confirmation posted in thread.
	mu.Lock()
	defer mu.Unlock()
	if len(postedTexts) < 2 {
		t.Fatal("expected confirmation message posted after reply")
	}
	if !strings.Contains(postedTexts[1], "Reply captured") {
		t.Errorf("expected confirmation message, got %q", postedTexts[1])
	}
}

// --- identity resolution tests ---

func TestDMDelivery_ResolvesGitLabUsername(t *testing.T) {
	daemon := newMockDaemon()
	// Seed a user identity: GitLab user "alice-gl" → Slack ID "U-ALICE".
	daemon.seedUser("Alice Smith", map[string]string{
		"slack_id":        "U-ALICE",
		"gitlab_username": "alice-gl",
		"email":           "alice@example.com",
	})

	var postedTo []string
	var mu sync.Mutex

	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/conversations.open":
			mu.Lock()
			postedTo = append(postedTo, r.FormValue("users"))
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok":      true,
				"channel": map[string]any{"id": "D-alice"},
			})
		case "/chat.postMessage":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok":      true,
				"channel": "D-alice",
				"ts":      "9999.0001",
			})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		}
	}))
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	delivery := NewDMDelivery(DMDeliveryConfig{
		Daemon: daemon,
		Bot:    bot,
		Logger: slog.Default(),
	})

	// Send a DM using GitLab username instead of Slack ID.
	payload := marshalSSEBeadPayload(BeadEvent{
		ID:   "dm-resolve-gl",
		Type: "dm",
		Fields: map[string]string{
			"target_user":  "alice-gl",
			"source_agent": "test-agent",
			"subject":      "Please review",
			"body":         "Check the MR",
		},
	})
	delivery.handleCreated(context.Background(), payload)

	mu.Lock()
	defer mu.Unlock()
	// The conversations.open call should have used the resolved Slack ID.
	if len(postedTo) == 0 {
		t.Fatal("expected conversations.open to be called")
	}
	if postedTo[0] != "U-ALICE" {
		t.Errorf("expected target resolved to U-ALICE, got %q", postedTo[0])
	}
}

func TestDMDelivery_ResolvesEmailToSlackID(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedUser("Bob", map[string]string{
		"slack_id":        "U-BOB",
		"github_username": "bob-gh",
		"email":           "bob@example.com",
	})

	var postedTo []string
	var mu sync.Mutex

	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/conversations.open":
			mu.Lock()
			postedTo = append(postedTo, r.FormValue("users"))
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok":      true,
				"channel": map[string]any{"id": "D-bob"},
			})
		case "/chat.postMessage":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok":      true,
				"channel": "D-bob",
				"ts":      "9999.0002",
			})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		}
	}))
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	delivery := NewDMDelivery(DMDeliveryConfig{
		Daemon: daemon,
		Bot:    bot,
		Logger: slog.Default(),
	})

	// Send a DM using email address.
	payload := marshalSSEBeadPayload(BeadEvent{
		ID:   "dm-resolve-email",
		Type: "dm",
		Fields: map[string]string{
			"target_user":  "bob@example.com",
			"source_agent": "test-agent",
			"subject":      "Question",
			"body":         "Quick question",
		},
	})
	delivery.handleCreated(context.Background(), payload)

	mu.Lock()
	defer mu.Unlock()
	if len(postedTo) == 0 {
		t.Fatal("expected conversations.open to be called")
	}
	if postedTo[0] != "U-BOB" {
		t.Errorf("expected target resolved to U-BOB, got %q", postedTo[0])
	}
}

func TestDMDelivery_SlackIDPassedThrough(t *testing.T) {
	daemon := newMockDaemon()

	var postedTo []string
	var mu sync.Mutex

	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/conversations.open":
			mu.Lock()
			postedTo = append(postedTo, r.FormValue("users"))
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok":      true,
				"channel": map[string]any{"id": "D-direct"},
			})
		case "/chat.postMessage":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok":      true,
				"channel": "D-direct",
				"ts":      "9999.0003",
			})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		}
	}))
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	delivery := NewDMDelivery(DMDeliveryConfig{
		Daemon: daemon,
		Bot:    bot,
		Logger: slog.Default(),
	})

	// Send a DM using a direct Slack user ID — should pass through without resolution.
	payload := marshalSSEBeadPayload(BeadEvent{
		ID:   "dm-direct-slack",
		Type: "dm",
		Fields: map[string]string{
			"target_user":  "U-DIRECT",
			"source_agent": "test-agent",
			"body":         "Direct message",
		},
	})
	delivery.handleCreated(context.Background(), payload)

	mu.Lock()
	defer mu.Unlock()
	if len(postedTo) == 0 {
		t.Fatal("expected conversations.open to be called")
	}
	if postedTo[0] != "U-DIRECT" {
		t.Errorf("expected U-DIRECT passed through, got %q", postedTo[0])
	}
}

// TestDMDelivery_SeenDedupAndGC verifies the dm-bead dedup set dedups on
// re-delivery and is TTL/size-bounded so it cannot leak for the process
// lifetime (ka-0000003049).
func TestDMDelivery_SeenDedupAndGC(t *testing.T) {
	delivery := NewDMDelivery(DMDeliveryConfig{
		Daemon: newMockDaemon(),
		Logger: slog.Default(),
	})

	if delivery.alreadySeen("dm-1") {
		t.Fatal("first alreadySeen(dm-1) = true, want false")
	}
	if !delivery.alreadySeen("dm-1") {
		t.Fatal("second alreadySeen(dm-1) = false, want true (dedup)")
	}
	if delivery.seen.len() != 1 {
		t.Fatalf("seen len = %d, want 1", delivery.seen.len())
	}
}

// TestDMDelivery_GCQuestions verifies stale reply-tracking entries are evicted
// past dmQuestionTTL, so the questions map cannot grow for the process lifetime
// (ka-0000003049). A late reply for an evicted entry still matches via the
// daemon fallback (findQuestionByThread), so eviction only bounds the fast path.
func TestDMDelivery_GCQuestions(t *testing.T) {
	delivery := NewDMDelivery(DMDeliveryConfig{
		Daemon: newMockDaemon(),
		Logger: slog.Default(),
	})

	delivery.trackQuestion("dm-old", "C-1", "100.0001", "agent", "U-1")
	delivery.trackQuestion("dm-fresh", "C-2", "200.0002", "agent", "U-2")

	// Backdate the "old" question past the TTL horizon.
	delivery.mu.Lock()
	delivery.questions["C-1:100.0001"].deliveredAt = time.Now().Add(-dmQuestionTTL - time.Hour)
	delivery.mu.Unlock()

	delivery.gcQuestions()

	delivery.mu.Lock()
	_, oldExists := delivery.questions["C-1:100.0001"]
	_, freshExists := delivery.questions["C-2:200.0002"]
	delivery.mu.Unlock()

	if oldExists {
		t.Errorf("stale question C-1 should have been gc'd")
	}
	if !freshExists {
		t.Errorf("fresh question C-2 should survive gc")
	}
}

// TestBuildDMText_Themed pins the question / review DM glyphs per theme
// (gd-anVGNChYkZ): the glyph stays glued to its verbal label, and Business is
// plain bracket text with no Garden glyph.
func TestBuildDMText_Themed(t *testing.T) {
	bead := &BeadEvent{Fields: map[string]string{"source_agent": "my-agent", "mr_url": "https://x/mr/1"}}
	cases := []struct {
		name string
		got  string
		want string
	}{
		{"garden question", buildQuestionDMText(Garden, bead, true), Garden.Semantic(SemQuestion) + " *Question from agent my-agent*"},
		{"business question", buildQuestionDMText(Business, bead, true), "[QUESTION] *Question from agent my-agent*"},
		{"garden review", buildReviewDMText(Garden, bead, true), Garden.Semantic(SemReview) + " *MR review request from agent my-agent*"},
		{"business review", buildReviewDMText(Business, bead, true), "[REVIEW] *MR review request from agent my-agent*"},
		{"business terse question", buildQuestionDMText(Business, &BeadEvent{Fields: map[string]string{}}, false), "[QUESTION] A question from an agent."},
		{"business terse review", buildReviewDMText(Business, bead, false), "[REVIEW] MR review request from agent my-agent"},
	}
	for _, c := range cases {
		if !strings.Contains(c.got, c.want) {
			t.Errorf("%s: want %q in:\n%s", c.name, c.want, c.got)
		}
	}
	for _, g := range []string{Garden.Semantic(SemQuestion), Garden.Semantic(SemReview)} {
		if strings.Contains(buildQuestionDMText(Business, bead, true)+buildReviewDMText(Business, bead, true), g) {
			t.Errorf("Business DM leaked Garden glyph %q", g)
		}
	}
}
