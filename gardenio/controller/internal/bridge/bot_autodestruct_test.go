package bridge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/slack-go/slack"
)

func TestIsAutoDestructAllowed(t *testing.T) {
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()
	bot := newTestBot(newMockDaemon(), slackSrv)

	t.Run("denied when env empty", func(t *testing.T) {
		t.Setenv("AUTODESTRUCT_ALLOWED_USERS", "")
		if bot.isAutoDestructAllowed("U123") {
			t.Error("expected denied when AUTODESTRUCT_ALLOWED_USERS is empty")
		}
	})

	t.Run("allowed when user in list", func(t *testing.T) {
		t.Setenv("AUTODESTRUCT_ALLOWED_USERS", "U111,U123,U999")
		if !bot.isAutoDestructAllowed("U123") {
			t.Error("expected allowed for U123")
		}
	})

	t.Run("denied when user not in list", func(t *testing.T) {
		t.Setenv("AUTODESTRUCT_ALLOWED_USERS", "U111,U999")
		if bot.isAutoDestructAllowed("U123") {
			t.Error("expected denied for U123")
		}
	})

	t.Run("handles whitespace in list", func(t *testing.T) {
		t.Setenv("AUTODESTRUCT_ALLOWED_USERS", "U111, U123 ,U999")
		if !bot.isAutoDestructAllowed("U123") {
			t.Error("expected allowed for U123 with surrounding spaces")
		}
	})
}

func TestCallAutoDestruct(t *testing.T) {
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	t.Run("success", func(t *testing.T) {
		controllerSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer test-token" {
				t.Errorf("expected bearer token, got %q", r.Header.Get("Authorization"))
			}
			if r.Header.Get("X-Actor") != "slack:U123" {
				t.Errorf("expected actor slack:U123, got %q", r.Header.Get("X-Actor"))
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "destructed", "killed": 3})
		}))
		defer controllerSrv.Close()

		bot := newTestBot(newMockDaemon(), slackSrv)
		bot.controllerURL = controllerSrv.URL

		t.Setenv("AUTODESTRUCT_TOKEN", "test-token")

		result, err := bot.callAutoDestruct(context.Background(), "POST", "/autodestruct", "U123")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		var resp struct {
			Status string `json:"status"`
			Killed int    `json:"killed"`
		}
		if err := json.Unmarshal([]byte(result), &resp); err != nil {
			t.Fatalf("failed to parse response: %v", err)
		}
		if resp.Killed != 3 {
			t.Errorf("expected 3 killed, got %d", resp.Killed)
		}
	})

	t.Run("no controller URL", func(t *testing.T) {
		bot := newTestBot(newMockDaemon(), slackSrv)
		bot.controllerURL = ""

		t.Setenv("AUTODESTRUCT_TOKEN", "test-token")

		_, err := bot.callAutoDestruct(context.Background(), "POST", "/autodestruct", "U123")
		if err == nil {
			t.Fatal("expected error when controller URL is empty")
		}
	})

	t.Run("no token", func(t *testing.T) {
		bot := newTestBot(newMockDaemon(), slackSrv)
		bot.controllerURL = "http://localhost:9999"

		t.Setenv("AUTODESTRUCT_TOKEN", "")

		_, err := bot.callAutoDestruct(context.Background(), "POST", "/autodestruct", "U123")
		if err == nil {
			t.Fatal("expected error when token is empty")
		}
	})

	t.Run("controller returns error", func(t *testing.T) {
		controllerSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"something broke"}`))
		}))
		defer controllerSrv.Close()

		bot := newTestBot(newMockDaemon(), slackSrv)
		bot.controllerURL = controllerSrv.URL

		t.Setenv("AUTODESTRUCT_TOKEN", "test-token")

		_, err := bot.callAutoDestruct(context.Background(), "POST", "/autodestruct", "U123")
		if err == nil {
			t.Fatal("expected error on 500 response")
		}
	})
}

// --- handleAutoDestructCommand routing tests ---

func TestHandleAutoDestructCommand_Unauthorized(t *testing.T) {
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(newMockDaemon(), slackSrv)
	t.Setenv("AUTODESTRUCT_ALLOWED_USERS", "")

	// Should post ephemeral error without panic.
	bot.handleAutoDestructCommand(context.Background(), slack.SlashCommand{
		ChannelID: "C123",
		UserID:    "U999",
	})
}

func TestHandleAutoDestructCommand_RoutesToConfirm(t *testing.T) {
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(newMockDaemon(), slackSrv)
	t.Setenv("AUTODESTRUCT_ALLOWED_USERS", "U123")

	// Empty text -> default case -> autoDestructConfirm.
	bot.handleAutoDestructCommand(context.Background(), slack.SlashCommand{
		ChannelID: "C123",
		UserID:    "U123",
		Text:      "",
	})
}

func TestHandleAutoDestructCommand_RoutesToClear(t *testing.T) {
	ctrlSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/autodestruct/clear" && r.Method == "POST" {
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"status":"cleared"}`))
			return
		}
		w.WriteHeader(404)
	}))
	defer ctrlSrv.Close()

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(newMockDaemon(), slackSrv)
	bot.controllerURL = ctrlSrv.URL
	t.Setenv("AUTODESTRUCT_ALLOWED_USERS", "U123")
	t.Setenv("AUTODESTRUCT_TOKEN", "test-token")

	bot.handleAutoDestructCommand(context.Background(), slack.SlashCommand{
		ChannelID: "C123",
		UserID:    "U123",
		Text:      "clear",
	})
}

func TestHandleAutoDestructCommand_RoutesToStatus(t *testing.T) {
	ctrlSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/autodestruct/status" && r.Method == "GET" {
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"active":false}`))
			return
		}
		w.WriteHeader(404)
	}))
	defer ctrlSrv.Close()

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(newMockDaemon(), slackSrv)
	bot.controllerURL = ctrlSrv.URL
	t.Setenv("AUTODESTRUCT_ALLOWED_USERS", "U123")
	t.Setenv("AUTODESTRUCT_TOKEN", "test-token")

	bot.handleAutoDestructCommand(context.Background(), slack.SlashCommand{
		ChannelID: "C123",
		UserID:    "U123",
		Text:      "status",
	})
}

// --- autoDestructConfirm tests ---

func TestAutoDestructConfirm_PostsBlocks(t *testing.T) {
	var called bool
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "message_ts": "1234.5678"})
	}))
	defer slackSrv.Close()

	bot := newTestBot(newMockDaemon(), slackSrv)

	bot.autoDestructConfirm(context.Background(), slack.SlashCommand{
		ChannelID: "C123",
		UserID:    "U456",
	})

	if !called {
		t.Error("expected Slack API to be called for ephemeral message")
	}
}

// --- autoDestructClear tests ---

func TestAutoDestructClear_Success(t *testing.T) {
	ctrlSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/autodestruct/clear" {
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"status":"cleared"}`))
			return
		}
		w.WriteHeader(404)
	}))
	defer ctrlSrv.Close()

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(newMockDaemon(), slackSrv)
	bot.controllerURL = ctrlSrv.URL
	t.Setenv("AUTODESTRUCT_TOKEN", "test-token")

	bot.autoDestructClear(context.Background(), slack.SlashCommand{
		ChannelID: "C123",
		UserID:    "U456",
	})
}

func TestAutoDestructClear_Error(t *testing.T) {
	ctrlSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		_, _ = w.Write([]byte("internal error"))
	}))
	defer ctrlSrv.Close()

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(newMockDaemon(), slackSrv)
	bot.controllerURL = ctrlSrv.URL
	t.Setenv("AUTODESTRUCT_TOKEN", "test-token")

	bot.autoDestructClear(context.Background(), slack.SlashCommand{
		ChannelID: "C123",
		UserID:    "U456",
	})
}

// --- autoDestructStatus tests ---

func TestAutoDestructStatus_Success(t *testing.T) {
	ctrlSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/autodestruct/status" {
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"active":false,"killed":0}`))
			return
		}
		w.WriteHeader(404)
	}))
	defer ctrlSrv.Close()

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(newMockDaemon(), slackSrv)
	bot.controllerURL = ctrlSrv.URL
	t.Setenv("AUTODESTRUCT_TOKEN", "test-token")

	bot.autoDestructStatus(context.Background(), slack.SlashCommand{
		ChannelID: "C123",
		UserID:    "U456",
	})
}

func TestAutoDestructStatus_Error(t *testing.T) {
	ctrlSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		_, _ = w.Write([]byte("server error"))
	}))
	defer ctrlSrv.Close()

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(newMockDaemon(), slackSrv)
	bot.controllerURL = ctrlSrv.URL
	t.Setenv("AUTODESTRUCT_TOKEN", "test-token")

	bot.autoDestructStatus(context.Background(), slack.SlashCommand{
		ChannelID: "C123",
		UserID:    "U456",
	})
}

// --- handleAutoDestructButtonAction tests ---

func TestHandleAutoDestructButtonAction_Unauthorized(t *testing.T) {
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(newMockDaemon(), slackSrv)
	t.Setenv("AUTODESTRUCT_ALLOWED_USERS", "")

	callback := slack.InteractionCallback{
		User:    slack.User{ID: "U999"},
		Channel: slack.Channel{GroupConversation: slack.GroupConversation{Conversation: slack.Conversation{ID: "C123"}}},
	}

	bot.handleAutoDestructButtonAction(context.Background(), "autodestruct_confirm", callback)
}

func TestHandleAutoDestructButtonAction_Confirm(t *testing.T) {
	ctrlSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/autodestruct" && r.Method == "POST" {
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"killed":3}`))
			return
		}
		w.WriteHeader(404)
	}))
	defer ctrlSrv.Close()

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(newMockDaemon(), slackSrv)
	bot.controllerURL = ctrlSrv.URL
	t.Setenv("AUTODESTRUCT_ALLOWED_USERS", "U123")
	t.Setenv("AUTODESTRUCT_TOKEN", "test-token")

	callback := slack.InteractionCallback{
		User:    slack.User{ID: "U123"},
		Channel: slack.Channel{GroupConversation: slack.GroupConversation{Conversation: slack.Conversation{ID: "C123"}}},
	}

	bot.handleAutoDestructButtonAction(context.Background(), "autodestruct_confirm", callback)
}

func TestHandleAutoDestructButtonAction_ConfirmError(t *testing.T) {
	ctrlSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		_, _ = w.Write([]byte("boom"))
	}))
	defer ctrlSrv.Close()

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(newMockDaemon(), slackSrv)
	bot.controllerURL = ctrlSrv.URL
	t.Setenv("AUTODESTRUCT_ALLOWED_USERS", "U123")
	t.Setenv("AUTODESTRUCT_TOKEN", "test-token")

	callback := slack.InteractionCallback{
		User:    slack.User{ID: "U123"},
		Channel: slack.Channel{GroupConversation: slack.GroupConversation{Conversation: slack.Conversation{ID: "C123"}}},
	}

	bot.handleAutoDestructButtonAction(context.Background(), "autodestruct_confirm", callback)
}

func TestHandleAutoDestructButtonAction_Cancel(t *testing.T) {
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(newMockDaemon(), slackSrv)
	t.Setenv("AUTODESTRUCT_ALLOWED_USERS", "U123")

	callback := slack.InteractionCallback{
		User:    slack.User{ID: "U123"},
		Channel: slack.Channel{GroupConversation: slack.GroupConversation{Conversation: slack.Conversation{ID: "C123"}}},
	}

	bot.handleAutoDestructButtonAction(context.Background(), "autodestruct_cancel", callback)
}

// --- resolveSlackUser tests ---

func TestResolveSlackUser_RealName(t *testing.T) {
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "users.info") || r.FormValue("user") != "" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok":   true,
				"user": map[string]any{"id": "U123", "real_name": "Alice Smith", "name": "alice"},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "message_ts": "1234.5678"})
	}))
	defer slackSrv.Close()

	bot := newTestBot(newMockDaemon(), slackSrv)

	result := bot.resolveSlackUser("U123")
	if result != "Alice Smith" {
		t.Errorf("expected 'Alice Smith', got %q", result)
	}
}

func TestResolveSlackUser_FallbackToName(t *testing.T) {
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "users.info") || r.FormValue("user") != "" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok":   true,
				"user": map[string]any{"id": "U123", "real_name": "", "name": "alice"},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "message_ts": "1234.5678"})
	}))
	defer slackSrv.Close()

	bot := newTestBot(newMockDaemon(), slackSrv)

	result := bot.resolveSlackUser("U123")
	if result != "alice" {
		t.Errorf("expected 'alice', got %q", result)
	}
}

func TestResolveSlackUser_FallbackToMention(t *testing.T) {
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "users.info") || r.FormValue("user") != "" {
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "user_not_found"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "message_ts": "1234.5678"})
	}))
	defer slackSrv.Close()

	bot := newTestBot(newMockDaemon(), slackSrv)

	result := bot.resolveSlackUser("U999")
	if result != "<@U999>" {
		t.Errorf("expected '<@U999>', got %q", result)
	}
}
