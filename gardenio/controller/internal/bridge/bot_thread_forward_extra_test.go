package bridge

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
)

func TestFetchMessageFiles_NilAPI(t *testing.T) {
	b := &Bot{
		api:    nil,
		logger: slog.Default(),
	}
	files := b.fetchMessageFiles(context.Background(), "C123", "1234.5678")
	if files != nil {
		t.Errorf("expected nil files when api is nil, got %v", files)
	}
}

func TestFetchMessageFiles_APIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "channel_not_found"})
	}))
	defer srv.Close()

	api := slack.New("xoxb-test", slack.OptionAPIURL(srv.URL+"/"))
	b := &Bot{
		api:    api,
		logger: slog.Default(),
	}

	files := b.fetchMessageFiles(context.Background(), "C-invalid", "1234.5678")
	if files != nil {
		t.Errorf("expected nil files on API error, got %v", files)
	}
}

func TestFetchMessageFiles_NoMatchingTimestamp(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": true,
			"messages": []map[string]any{
				{
					"ts":   "9999.9999",
					"text": "some message",
				},
			},
		})
	}))
	defer srv.Close()

	api := slack.New("xoxb-test", slack.OptionAPIURL(srv.URL+"/"))
	b := &Bot{
		api:    api,
		logger: slog.Default(),
	}

	files := b.fetchMessageFiles(context.Background(), "C123", "1234.5678")
	if files != nil {
		t.Errorf("expected nil files when no matching timestamp, got %v", files)
	}
}

func TestFetchMessageFiles_MatchingTimestampWithFiles(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": true,
			"messages": []map[string]any{
				{
					"ts":   "1234.5678",
					"text": "hello",
					"files": []map[string]any{
						{
							"id":       "F123",
							"name":     "test.png",
							"mimetype": "image/png",
							"size":     1024,
						},
					},
				},
			},
		})
	}))
	defer srv.Close()

	api := slack.New("xoxb-test", slack.OptionAPIURL(srv.URL+"/"))
	b := &Bot{
		api:    api,
		logger: slog.Default(),
	}

	files := b.fetchMessageFiles(context.Background(), "C123", "1234.5678")
	if len(files) != 1 {
		t.Fatalf("expected 1 file, got %d", len(files))
	}
	if files[0].ID != "F123" {
		t.Errorf("expected file ID=F123, got %s", files[0].ID)
	}
}

func TestSlackFilesToFields_WithImageFile(t *testing.T) {
	files := []slack.File{
		{ID: "F1", Mimetype: "image/png"},
		{ID: "F2", Mimetype: "text/plain"},
	}
	fields := slackFilesToFields(files)
	if fields == nil {
		t.Fatal("expected non-nil fields")
	}
	if fields["slack_attachment_count"] != "2" {
		t.Errorf("expected attachment count=2, got %s", fields["slack_attachment_count"])
	}
	if fields["slack_has_images"] != "true" {
		t.Error("expected slack_has_images=true")
	}
}

func TestSlackFilesToFields_NoImageFile(t *testing.T) {
	files := []slack.File{
		{ID: "F1", Mimetype: "text/plain"},
	}
	fields := slackFilesToFields(files)
	if fields == nil {
		t.Fatal("expected non-nil fields")
	}
	if _, ok := fields["slack_has_images"]; ok {
		t.Error("expected no slack_has_images when no images")
	}
}

func TestHandleThreadForward_MessageRefPersisted(t *testing.T) {
	daemon := newMockDaemon()
	daemon.beads["hq"] = &beadsapi.BeadDetail{
		ID:    "bd-agent-hq",
		Title: "hq",
		Type:  "agent",
		Fields: map[string]string{
			"agent": "hq",
		},
	}

	dir := t.TempDir()
	state, err := NewStateManager(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	_ = state.SetThreadAgent("C-test", "1111.2222", "hq")
	_ = state.SetListenThread("C-test", "1111.2222")

	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "user": map[string]any{
			"id":        "U-HUMAN",
			"real_name": "Test User",
		}})
	}))
	defer slackSrv.Close()

	b := newTestBot(daemon, slackSrv)
	b.state = state
	b.lastThreadNudge = make(map[string]time.Time)

	ev := &slackevents.MessageEvent{
		User:            "U-HUMAN",
		Channel:         "C-test",
		Text:            "follow up message",
		TimeStamp:       "1111.3333",
		ThreadTimeStamp: "1111.2222",
	}

	b.handleThreadForward(context.Background(), slackReplyInbound(ev, ""), "hq")

	var beadID string
	for id, bead := range daemon.beads {
		if bead.Type == "message" && hasLabel(bead.Labels, "slack-thread-reply") {
			beadID = id
			break
		}
	}
	if beadID == "" {
		t.Fatal("expected a thread-reply bead to be created")
	}

	ref, ok := state.GetChatMessage(beadID)
	if !ok {
		t.Fatal("expected message ref to be persisted in state")
	}
	if ref.ChannelID != "C-test" {
		t.Errorf("expected channel C-test, got %s", ref.ChannelID)
	}
	if ref.Timestamp != "1111.2222" {
		t.Errorf("expected thread ts 1111.2222, got %s", ref.Timestamp)
	}
}

func TestFetchMessageFiles_MultipleMessages_MatchesCorrectOne(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": true,
			"messages": []map[string]any{
				{
					"ts":   "1111.0000",
					"text": "wrong message",
					"files": []map[string]any{
						{"id": "WRONG", "name": "wrong.txt"},
					},
				},
				{
					"ts":   "1234.5678",
					"text": "right message",
					"files": []map[string]any{
						{"id": "RIGHT", "name": "right.txt"},
					},
				},
			},
		})
	}))
	defer srv.Close()

	api := slack.New("xoxb-test", slack.OptionAPIURL(srv.URL+"/"))
	b := &Bot{
		api:    api,
		logger: slog.Default(),
	}

	files := b.fetchMessageFiles(context.Background(), "C123", "1234.5678")
	if len(files) != 1 {
		t.Fatalf("expected 1 file, got %d", len(files))
	}
	if files[0].ID != "RIGHT" {
		t.Errorf("expected file ID=RIGHT, got %s", files[0].ID)
	}
}

func TestFetchMessageFiles_EmptyMessages(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":       true,
			"messages": []map[string]any{},
		})
	}))
	defer srv.Close()

	api := slack.New("xoxb-test", slack.OptionAPIURL(srv.URL+"/"))
	b := &Bot{
		api:    api,
		logger: slog.Default(),
	}

	files := b.fetchMessageFiles(context.Background(), "C123", "1234.5678")
	if files != nil {
		t.Errorf("expected nil files for empty messages, got %v", files)
	}
}
