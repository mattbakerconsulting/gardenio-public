package bridge

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSlackThreadAPI_GetMessages_MissingParams(t *testing.T) {
	api := NewSlackThreadAPI(nil, slog.Default())
	mux := http.NewServeMux()
	api.RegisterRoutes(mux)

	tests := []struct {
		name string
		url  string
	}{
		{"missing both", "/api/slack/threads"},
		{"missing ts", "/api/slack/threads?channel=C-test"},
		{"missing channel", "/api/slack/threads?ts=1111.2222"},
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

func TestSlackThreadAPI_GetMessages_MethodNotAllowed(t *testing.T) {
	api := NewSlackThreadAPI(nil, slog.Default())
	mux := http.NewServeMux()
	api.RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodPost, "/api/slack/threads?channel=C-test&ts=1.1", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("got status %d, want %d", w.Code, http.StatusMethodNotAllowed)
	}
}

func TestSlackThreadAPI_PostReply_MissingFields(t *testing.T) {
	api := NewSlackThreadAPI(nil, slog.Default())
	mux := http.NewServeMux()
	api.RegisterRoutes(mux)

	tests := []struct {
		name string
		body replyRequest
	}{
		{"missing channel", replyRequest{ThreadTS: "1.1", Text: "hello"}},
		{"missing thread_ts", replyRequest{Channel: "C-test", Text: "hello"}},
		{"missing text", replyRequest{Channel: "C-test", ThreadTS: "1.1"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body, _ := json.Marshal(tt.body)
			req := httptest.NewRequest(http.MethodPost, "/api/slack/threads/reply", bytes.NewReader(body))
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, req)
			if w.Code != http.StatusBadRequest {
				t.Errorf("got status %d, want %d", w.Code, http.StatusBadRequest)
			}
		})
	}
}

func TestSlackThreadAPI_PostReply_MethodNotAllowed(t *testing.T) {
	api := NewSlackThreadAPI(nil, slog.Default())
	mux := http.NewServeMux()
	api.RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/api/slack/threads/reply", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("got status %d, want %d", w.Code, http.StatusMethodNotAllowed)
	}
}

func TestSlackThreadAPI_PostReply_InvalidBody(t *testing.T) {
	api := NewSlackThreadAPI(nil, slog.Default())
	mux := http.NewServeMux()
	api.RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodPost, "/api/slack/threads/reply",
		bytes.NewReader([]byte("not json")))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("got status %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestSlackThreadAPI_PostUpload_MethodNotAllowed(t *testing.T) {
	api := NewSlackThreadAPI(nil, slog.Default())
	mux := http.NewServeMux()
	api.RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/api/slack/threads/upload", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("got status %d, want %d", w.Code, http.StatusMethodNotAllowed)
	}
}

func TestSlackThreadAPI_PostUpload_InvalidBody(t *testing.T) {
	api := NewSlackThreadAPI(nil, slog.Default())
	mux := http.NewServeMux()
	api.RegisterRoutes(mux)

	// A POST that is not multipart/form-data — ParseMultipartForm should fail.
	req := httptest.NewRequest(http.MethodPost, "/api/slack/threads/upload",
		strings.NewReader("not multipart"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("got status %d, want %d", w.Code, http.StatusBadRequest)
	}
}

// newUploadRequest builds a multipart upload request with the given form
// fields. A non-empty fileBytes adds a "file" part named fileName.
func newUploadRequest(t *testing.T, fields map[string]string, fileName string, fileBytes []byte) *http.Request {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	for k, v := range fields {
		if err := mw.WriteField(k, v); err != nil {
			t.Fatalf("WriteField(%q): %v", k, err)
		}
	}
	if fileName != "" {
		part, err := mw.CreateFormFile("file", fileName)
		if err != nil {
			t.Fatalf("CreateFormFile: %v", err)
		}
		if _, err := part.Write(fileBytes); err != nil {
			t.Fatalf("write file part: %v", err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/slack/threads/upload", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	return req
}

func TestSlackThreadAPI_PostUpload_MissingFields(t *testing.T) {
	api := NewSlackThreadAPI(nil, slog.Default())
	mux := http.NewServeMux()
	api.RegisterRoutes(mux)

	tests := []struct {
		name   string
		fields map[string]string
		file   bool
	}{
		{"missing channel", map[string]string{"thread_ts": "1.1"}, true},
		{"missing thread_ts", map[string]string{"channel": "C-test"}, true},
		{"missing file part", map[string]string{"channel": "C-test", "thread_ts": "1.1"}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var fileName string
			var fileBytes []byte
			if tt.file {
				fileName = "x.png"
				fileBytes = []byte("PNGDATA")
			}
			req := newUploadRequest(t, tt.fields, fileName, fileBytes)
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, req)
			if w.Code != http.StatusBadRequest {
				t.Errorf("got status %d, want %d", w.Code, http.StatusBadRequest)
			}
		})
	}
}

func TestSlackThreadAPI_PostUpload_EmptyFile(t *testing.T) {
	api := NewSlackThreadAPI(nil, slog.Default())
	mux := http.NewServeMux()
	api.RegisterRoutes(mux)

	req := newUploadRequest(t,
		map[string]string{"channel": "C-test", "thread_ts": "1.1"}, "empty.png", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("got status %d, want %d (empty file should be rejected)", w.Code, http.StatusBadRequest)
	}
}

func TestUploadResponse_JSON(t *testing.T) {
	resp := uploadResponse{OK: true, FileID: "F999", Title: "shot.png"}
	data, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(data), `"file_id":"F999"`) {
		t.Errorf("expected file_id in payload, got %s", data)
	}

	// Title is omitempty.
	bare := uploadResponse{OK: true, FileID: "F1"}
	data, err = json.Marshal(bare)
	if err != nil {
		t.Fatalf("marshal bare: %v", err)
	}
	if strings.Contains(string(data), "title") {
		t.Errorf("expected title to be omitted when empty, got %s", data)
	}
}

func TestJSONEscape(t *testing.T) {
	cases := []struct{ in, want string }{
		{`plain`, `plain`},
		{`with "quotes"`, `with \"quotes\"`},
		{"with\nnewline", `with\nnewline`},
		{`back\slash`, `back\\slash`},
	}
	for _, c := range cases {
		if got := jsonEscape(c.in); got != c.want {
			t.Errorf("jsonEscape(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestSlackThreadAPI_FileProxy_MissingFileID(t *testing.T) {
	api := NewSlackThreadAPI(nil, slog.Default())
	mux := http.NewServeMux()
	api.RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/api/slack/files/", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("got status %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestSlackThreadAPI_FileProxy_MethodNotAllowed(t *testing.T) {
	api := NewSlackThreadAPI(nil, slog.Default())
	mux := http.NewServeMux()
	api.RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodPost, "/api/slack/files/F12345", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("got status %d, want %d", w.Code, http.StatusMethodNotAllowed)
	}
}

func TestSlackFileInfo_JSON(t *testing.T) {
	info := SlackFileInfo{
		ID:       "F12345",
		Name:     "screenshot.png",
		Mimetype: "image/png",
		Size:     1024,
		ProxyURL: "/api/slack/files/F12345",
	}

	data, err := json.Marshal(info)
	if err != nil {
		t.Fatalf("failed to marshal: %v", err)
	}

	var decoded SlackFileInfo
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	if decoded.ID != info.ID {
		t.Errorf("ID = %q, want %q", decoded.ID, info.ID)
	}
	if decoded.ProxyURL != info.ProxyURL {
		t.Errorf("ProxyURL = %q, want %q", decoded.ProxyURL, info.ProxyURL)
	}
}

func TestThreadMessage_WithFiles(t *testing.T) {
	msg := ThreadMessage{
		Author:    "U-TEST",
		Text:      "check this screenshot",
		Timestamp: "1111.2222",
		Files: []SlackFileInfo{
			{
				ID:       "F12345",
				Name:     "screenshot.png",
				Mimetype: "image/png",
				Size:     2048,
				ProxyURL: "/api/slack/files/F12345",
			},
		},
	}

	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("failed to marshal: %v", err)
	}

	var decoded ThreadMessage
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	if len(decoded.Files) != 1 {
		t.Fatalf("expected 1 file, got %d", len(decoded.Files))
	}
	if decoded.Files[0].Name != "screenshot.png" {
		t.Errorf("file name = %q, want %q", decoded.Files[0].Name, "screenshot.png")
	}
}

func TestThreadMessage_SubtypeJSON(t *testing.T) {
	tagged := ThreadMessage{
		Author:    "agent-1",
		Text:      ":speech_balloon: agent-1: progress",
		Timestamp: "1111.0001",
		IsBot:     true,
		Subtype:   SubtypeSquawk,
	}
	data, err := json.Marshal(tagged)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(data), `"subtype":"squawk"`) {
		t.Errorf("expected subtype in payload, got %s", data)
	}

	untagged := ThreadMessage{Author: "matt", Text: "hi", Timestamp: "1111.0002"}
	data, err = json.Marshal(untagged)
	if err != nil {
		t.Fatalf("marshal untagged: %v", err)
	}
	if strings.Contains(string(data), "subtype") {
		t.Errorf("expected subtype to be omitted when empty, got %s", data)
	}
}

func TestThreadMessage_WithoutFiles_OmitsField(t *testing.T) {
	msg := ThreadMessage{
		Author:    "U-TEST",
		Text:      "no files here",
		Timestamp: "1111.2222",
	}

	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("failed to marshal: %v", err)
	}

	// Verify "files" key is omitted (omitempty).
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}
	if _, ok := raw["files"]; ok {
		t.Error("expected 'files' to be omitted when empty")
	}
}
