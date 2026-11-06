package bridge

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/slack-go/slack"
)

func TestSlackChannelAPI_ListChannels_MethodNotAllowed(t *testing.T) {
	api := NewSlackChannelAPI(nil, slog.Default())
	mux := http.NewServeMux()
	api.RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodPost, "/api/slack/channels", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("got status %d, want %d", w.Code, http.StatusMethodNotAllowed)
	}
}

func TestSlackChannelAPI_History_MissingChannel(t *testing.T) {
	api := NewSlackChannelAPI(nil, slog.Default())
	mux := http.NewServeMux()
	api.RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/api/slack/channels/history", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("got status %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestSlackChannelAPI_History_MethodNotAllowed(t *testing.T) {
	api := NewSlackChannelAPI(nil, slog.Default())
	mux := http.NewServeMux()
	api.RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodPost, "/api/slack/channels/history?channel=C-test", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("got status %d, want %d", w.Code, http.StatusMethodNotAllowed)
	}
}

func TestSlackChannelAPI_Search_MissingParams(t *testing.T) {
	api := NewSlackChannelAPI(nil, slog.Default())
	mux := http.NewServeMux()
	api.RegisterRoutes(mux)

	tests := []struct {
		name string
		url  string
	}{
		{"missing both", "/api/slack/channels/search"},
		{"missing query", "/api/slack/channels/search?channel=C-test"},
		{"missing channel", "/api/slack/channels/search?q=hello"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tt.url, nil)
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, req)
			if w.Code != http.StatusBadRequest {
				t.Errorf("got status %d, want %d", w.Code, http.StatusBadRequest)
			}
		})
	}
}

func TestSlackChannelAPI_Search_MethodNotAllowed(t *testing.T) {
	api := NewSlackChannelAPI(nil, slog.Default())
	mux := http.NewServeMux()
	api.RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodPost, "/api/slack/channels/search?channel=C-test&q=hello", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("got status %d, want %d", w.Code, http.StatusMethodNotAllowed)
	}
}

func TestSlackChannelAPI_Thread_MissingParams(t *testing.T) {
	api := NewSlackChannelAPI(nil, slog.Default())
	mux := http.NewServeMux()
	api.RegisterRoutes(mux)

	tests := []struct {
		name string
		url  string
	}{
		{"missing both", "/api/slack/channels/thread"},
		{"missing ts", "/api/slack/channels/thread?channel=C-test"},
		{"missing channel", "/api/slack/channels/thread?ts=1111.2222"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tt.url, nil)
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, req)
			if w.Code != http.StatusBadRequest {
				t.Errorf("got status %d, want %d", w.Code, http.StatusBadRequest)
			}
		})
	}
}

func TestSlackChannelAPI_Thread_MethodNotAllowed(t *testing.T) {
	api := NewSlackChannelAPI(nil, slog.Default())
	mux := http.NewServeMux()
	api.RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodPost, "/api/slack/channels/thread?channel=C-test&ts=1.1", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("got status %d, want %d", w.Code, http.StatusMethodNotAllowed)
	}
}

func TestChannelInfo_JSON(t *testing.T) {
	info := ChannelInfo{
		ID:      "C12345",
		Name:    "general",
		Topic:   "General chat",
		Purpose: "Company-wide",
		Private: false,
	}

	data, err := json.Marshal(info)
	if err != nil {
		t.Fatalf("failed to marshal: %v", err)
	}

	var decoded ChannelInfo
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	if decoded.ID != info.ID {
		t.Errorf("ID = %q, want %q", decoded.ID, info.ID)
	}
	if decoded.Name != info.Name {
		t.Errorf("Name = %q, want %q", decoded.Name, info.Name)
	}
	if decoded.Private != info.Private {
		t.Errorf("Private = %v, want %v", decoded.Private, info.Private)
	}
}

func TestChannelMessage_JSON(t *testing.T) {
	msg := ChannelMessage{
		Author:    "U-TEST",
		Text:      "hello world",
		Timestamp: "1111.2222",
		IsBot:     false,
		ThreadTS:  "1111.1111",
		Files: []SlackFileInfo{
			{ID: "F1", Name: "file.txt", Mimetype: "text/plain", Size: 100, ProxyURL: "/api/slack/files/F1"},
		},
	}

	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("failed to marshal: %v", err)
	}

	var decoded ChannelMessage
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	if decoded.ThreadTS != "1111.1111" {
		t.Errorf("ThreadTS = %q, want %q", decoded.ThreadTS, "1111.1111")
	}
	if len(decoded.Files) != 1 {
		t.Fatalf("expected 1 file, got %d", len(decoded.Files))
	}
}

func TestSlackChannelAPI_Delete_MethodNotAllowed(t *testing.T) {
	api := NewSlackChannelAPI(nil, slog.Default())
	mux := http.NewServeMux()
	api.RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/api/slack/messages/delete", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("got status %d, want %d", w.Code, http.StatusMethodNotAllowed)
	}
}

func TestSlackChannelAPI_Delete_MissingFields(t *testing.T) {
	api := NewSlackChannelAPI(nil, slog.Default())
	mux := http.NewServeMux()
	api.RegisterRoutes(mux)

	tests := []struct {
		name string
		body string
	}{
		{"empty body", `{}`},
		{"missing timestamp", `{"channel":"C123"}`},
		{"missing channel", `{"timestamp":"1234.5678"}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/slack/messages/delete",
				strings.NewReader(tt.body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, req)
			if w.Code != http.StatusBadRequest {
				t.Errorf("got status %d, want %d", w.Code, http.StatusBadRequest)
			}
		})
	}
}

func TestSlackChannelAPI_Delete_InvalidJSON(t *testing.T) {
	api := NewSlackChannelAPI(nil, slog.Default())
	mux := http.NewServeMux()
	api.RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodPost, "/api/slack/messages/delete",
		strings.NewReader("not json"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("got status %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestExtractMessageText_PlainText(t *testing.T) {
	msg := slack.Msg{Text: "hello world"}
	got := extractMessageText(msg)
	if got != "hello world" {
		t.Errorf("got %q, want %q", got, "hello world")
	}
}

func TestExtractMessageText_AttachmentOnly(t *testing.T) {
	msg := slack.Msg{
		Attachments: []slack.Attachment{
			{
				Pretext: "Alert fired",
				Title:   "smoke-devel DOWN",
				Text:    "sponsor-api is not responding",
				Fields: []slack.AttachmentField{
					{Title: "Status", Value: "critical"},
					{Title: "Duration", Value: "6h"},
				},
			},
		},
	}
	got := extractMessageText(msg)
	if !strings.Contains(got, "Alert fired") {
		t.Errorf("missing pretext in %q", got)
	}
	if !strings.Contains(got, "smoke-devel DOWN") {
		t.Errorf("missing title in %q", got)
	}
	if !strings.Contains(got, "sponsor-api is not responding") {
		t.Errorf("missing text in %q", got)
	}
	if !strings.Contains(got, "Status: critical") {
		t.Errorf("missing field in %q", got)
	}
	if !strings.Contains(got, "Duration: 6h") {
		t.Errorf("missing field in %q", got)
	}
}

func TestExtractMessageText_BlocksOnly(t *testing.T) {
	msg := slack.Msg{
		Blocks: slack.Blocks{
			BlockSet: []slack.Block{
				slack.NewHeaderBlock(
					slack.NewTextBlockObject("plain_text", "Alert Header", false, false),
				),
				slack.NewSectionBlock(
					slack.NewTextBlockObject("mrkdwn", "Error rate exceeded threshold", false, false),
					[]*slack.TextBlockObject{
						slack.NewTextBlockObject("mrkdwn", "*P95*: 4.2s", false, false),
					},
					nil,
				),
			},
		},
	}
	got := extractMessageText(msg)
	if !strings.Contains(got, "Alert Header") {
		t.Errorf("missing header in %q", got)
	}
	if !strings.Contains(got, "Error rate exceeded threshold") {
		t.Errorf("missing section text in %q", got)
	}
	if !strings.Contains(got, "*P95*: 4.2s") {
		t.Errorf("missing section field in %q", got)
	}
}

func TestExtractMessageText_TextPlusAttachments(t *testing.T) {
	msg := slack.Msg{
		Text: "Fallback text",
		Attachments: []slack.Attachment{
			{Title: "Detail", Text: "More info here"},
		},
	}
	got := extractMessageText(msg)
	if !strings.Contains(got, "Fallback text") {
		t.Errorf("missing plain text in %q", got)
	}
	if !strings.Contains(got, "Detail") {
		t.Errorf("missing attachment title in %q", got)
	}
	if !strings.Contains(got, "More info here") {
		t.Errorf("missing attachment text in %q", got)
	}
}

func TestExtractMessageText_Empty(t *testing.T) {
	msg := slack.Msg{}
	got := extractMessageText(msg)
	if got != "" {
		t.Errorf("got %q, want empty string", got)
	}
}

func TestExtractMessageText_TextPlusBlocks_NoDoubleRender(t *testing.T) {
	// Slack mirrors message body into Text AND a RichTextBlock for any post
	// with formatting. Extracting both produces double-rendered output where
	// the second pass emits one bold/italic token per line. Regression check
	// for ka-0000006494.
	body := "Don't add the *approved-to-work* label"
	msg := slack.Msg{
		Text: body,
		Blocks: slack.Blocks{
			BlockSet: []slack.Block{
				slack.NewRichTextBlock("rt1",
					slack.NewRichTextSection(
						slack.NewRichTextSectionTextElement("Don't add the ", nil),
						slack.NewRichTextSectionTextElement("approved-to-work", &slack.RichTextSectionTextStyle{Bold: true}),
						slack.NewRichTextSectionTextElement(" label", nil),
					),
				),
			},
		},
	}
	got := extractMessageText(msg)
	if got != body {
		t.Errorf("got %q, want %q (block content should not be appended when Text is set)", got, body)
	}
	if strings.Count(got, "approved-to-work") != 1 {
		t.Errorf("expected exactly one occurrence of body content, got %q", got)
	}
}

func TestExtractMessageText_ContextBlock(t *testing.T) {
	msg := slack.Msg{
		Blocks: slack.Blocks{
			BlockSet: []slack.Block{
				slack.NewContextBlock("ctx1",
					slack.NewTextBlockObject("mrkdwn", "Sent by vmalert", false, false),
				),
			},
		},
	}
	got := extractMessageText(msg)
	if !strings.Contains(got, "Sent by vmalert") {
		t.Errorf("missing context text in %q", got)
	}
}

func TestSearchResult_JSON(t *testing.T) {
	sr := SearchResult{
		Message: ChannelMessage{Author: "U1", Text: "match", Timestamp: "3.0"},
		Before:  []ChannelMessage{{Author: "U2", Text: "before", Timestamp: "2.0"}},
		After:   []ChannelMessage{{Author: "U3", Text: "after", Timestamp: "4.0"}},
	}

	data, err := json.Marshal(sr)
	if err != nil {
		t.Fatalf("failed to marshal: %v", err)
	}

	var decoded SearchResult
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	if decoded.Message.Text != "match" {
		t.Errorf("Message.Text = %q, want %q", decoded.Message.Text, "match")
	}
	if len(decoded.Before) != 1 {
		t.Errorf("Before length = %d, want 1", len(decoded.Before))
	}
	if len(decoded.After) != 1 {
		t.Errorf("After length = %d, want 1", len(decoded.After))
	}
}
