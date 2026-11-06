package main

import (
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTempFile creates a file with the given contents under a temp dir and
// returns its path.
func writeTempFile(t *testing.T, name string, data []byte) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatalf("write temp file: %v", err)
	}
	return p
}

func TestRunSlackUpload_Integration(t *testing.T) {
	var (
		gotChannel  string
		gotThreadTS string
		gotComment  string
		gotTitle    string
		gotAltTxt   string
		gotFileName string
		gotFileBody []byte
	)

	server := httptest.NewServer(oldBridge(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/slack/threads/upload" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || !strings.HasPrefix(mediaType, "multipart/") {
			t.Fatalf("expected multipart content-type, got %q (err %v)", r.Header.Get("Content-Type"), err)
		}
		mr := multipart.NewReader(r.Body, params["boundary"])
		for {
			part, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatalf("reading multipart: %v", err)
			}
			data, _ := io.ReadAll(part)
			switch part.FormName() {
			case "channel":
				gotChannel = string(data)
			case "thread_ts":
				gotThreadTS = string(data)
			case "initial_comment":
				gotComment = string(data)
			case "title":
				gotTitle = string(data)
			case "alt_txt":
				gotAltTxt = string(data)
			case "file":
				gotFileName = part.FileName()
				gotFileBody = data
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"file_id":"F12345","title":"shot.png"}`))
	}))
	defer server.Close()

	path := writeTempFile(t, "shot.png", []byte("PNGDATA"))

	t.Setenv("SLACK_BRIDGE_URL", server.URL)
	t.Setenv("SLACK_THREAD_CHANNEL", "C999")
	t.Setenv("SLACK_THREAD_TS", "111.222")

	// Fresh command instance so flag defaults apply.
	cmd := newSlackUploadCmd()
	if err := cmd.Flags().Set("comment", "hello there"); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Flags().Set("title", "My Title"); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Flags().Set("alt-text", "alt words"); err != nil {
		t.Fatal(err)
	}

	if err := runSlackUpload(cmd, []string{path}); err != nil {
		t.Fatalf("runSlackUpload: %v", err)
	}

	if gotChannel != "C999" {
		t.Errorf("channel = %q, want C999", gotChannel)
	}
	if gotThreadTS != "111.222" {
		t.Errorf("thread_ts = %q, want 111.222", gotThreadTS)
	}
	if gotComment != "hello there" {
		t.Errorf("initial_comment = %q, want %q", gotComment, "hello there")
	}
	if gotTitle != "My Title" {
		t.Errorf("title = %q, want %q", gotTitle, "My Title")
	}
	if gotAltTxt != "alt words" {
		t.Errorf("alt_txt = %q, want %q", gotAltTxt, "alt words")
	}
	if gotFileName != "shot.png" {
		t.Errorf("file name = %q, want shot.png", gotFileName)
	}
	if string(gotFileBody) != "PNGDATA" {
		t.Errorf("file body = %q, want PNGDATA", string(gotFileBody))
	}
}

func TestRunSlackUpload_MissingFile(t *testing.T) {
	t.Setenv("SLACK_BRIDGE_URL", "http://bridge:8090")
	t.Setenv("SLACK_THREAD_CHANNEL", "C999")
	t.Setenv("SLACK_THREAD_TS", "111.222")

	cmd := newSlackUploadCmd()
	err := runSlackUpload(cmd, []string{"/nonexistent/path/to/file.png"})
	if err == nil {
		t.Fatal("expected error for missing file, got nil")
	}
	if !strings.Contains(err.Error(), "stat") {
		t.Errorf("expected stat error, got %v", err)
	}
}

func TestRunSlackUpload_EmptyFile(t *testing.T) {
	path := writeTempFile(t, "empty.png", nil)
	t.Setenv("SLACK_BRIDGE_URL", "http://bridge:8090")
	t.Setenv("SLACK_THREAD_CHANNEL", "C999")
	t.Setenv("SLACK_THREAD_TS", "111.222")

	cmd := newSlackUploadCmd()
	err := runSlackUpload(cmd, []string{path})
	if err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("expected empty-file error, got %v", err)
	}
}

func TestRunSlackUpload_TooLarge(t *testing.T) {
	path := writeTempFile(t, "big.bin", []byte("0123456789"))
	t.Setenv("SLACK_BRIDGE_URL", "http://bridge:8090")
	t.Setenv("SLACK_THREAD_CHANNEL", "C999")
	t.Setenv("SLACK_THREAD_TS", "111.222")

	cmd := newSlackUploadCmd()
	if err := cmd.Flags().Set("max-bytes", "5"); err != nil {
		t.Fatal(err)
	}
	err := runSlackUpload(cmd, []string{path})
	if err == nil || !strings.Contains(err.Error(), "max-bytes") {
		t.Fatalf("expected too-large error, got %v", err)
	}
}

func TestRunSlackUpload_BridgeError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"failed to upload file: missing_scope"}`, http.StatusBadGateway)
	}))
	defer server.Close()

	path := writeTempFile(t, "shot.png", []byte("PNGDATA"))
	t.Setenv("SLACK_BRIDGE_URL", server.URL)
	t.Setenv("SLACK_THREAD_CHANNEL", "C999")
	t.Setenv("SLACK_THREAD_TS", "111.222")

	cmd := newSlackUploadCmd()
	err := runSlackUpload(cmd, []string{path})
	if err == nil {
		t.Fatal("expected error for non-200 bridge response, got nil")
	}
	if !strings.Contains(err.Error(), "502") || !strings.Contains(err.Error(), "missing_scope") {
		t.Errorf("expected 502 + missing_scope in error, got %v", err)
	}
}

// registeredSlackUploadFieldKeys is the set of bead Fields keys registered for
// the message type (proven by the working recordSlackReplyBead path in
// slack.go). buildSlackUploadFields must emit ONLY these keys — any other key
// makes kbeads reject the whole CreateBead with HTTP 400 (ka-0000005057).
var registeredSlackUploadFieldKeys = map[string]bool{
	"source_agent":         true,
	"slack_thread_channel": true,
	"slack_thread_ts":      true,
	"text":                 true,
}

func TestBuildSlackUploadFields_OnlyRegisteredKeys(t *testing.T) {
	cases := []struct {
		name    string
		comment string
		fileID  string
	}{
		{"comment and file", "here's the login page", "F12345"},
		{"comment only", "no file id here", ""},
		{"file only", "", "F67890"},
		{"neither", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fields := buildSlackUploadFields("agent-x", tc.comment, "C999", "111.222", tc.fileID)

			for k := range fields {
				if !registeredSlackUploadFieldKeys[k] {
					t.Errorf("field map contains unregistered key %q; only %v are allowed",
						k, registeredSlackUploadFieldKeys)
				}
			}
			// Regression guard: the original bug emitted these two keys.
			if _, ok := fields["filename"]; ok {
				t.Error(`field map must not contain "filename" (unregistered)`)
			}
			if _, ok := fields["slack_file_id"]; ok {
				t.Error(`field map must not contain "slack_file_id" (unregistered)`)
			}

			// Registered identity fields are always present.
			if fields["source_agent"] != "agent-x" {
				t.Errorf("source_agent = %q, want agent-x", fields["source_agent"])
			}
			if fields["slack_thread_channel"] != "C999" {
				t.Errorf("slack_thread_channel = %q, want C999", fields["slack_thread_channel"])
			}
			if fields["slack_thread_ts"] != "111.222" {
				t.Errorf("slack_thread_ts = %q, want 111.222", fields["slack_thread_ts"])
			}
		})
	}
}

func TestBuildSlackUploadFields_TextPreservesCommentAndFileID(t *testing.T) {
	cases := []struct {
		name     string
		comment  string
		fileID   string
		wantText string // "" means the text key must be absent
	}{
		{"comment and file", "here's the login page", "F12345", "here's the login page (file F12345)"},
		{"comment only", "no file id here", "", "no file id here"},
		{"file only", "", "F67890", "file F67890"},
		{"neither", "", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fields := buildSlackUploadFields("agent-x", tc.comment, "C999", "111.222", tc.fileID)
			got, ok := fields["text"]
			if tc.wantText == "" {
				if ok {
					t.Errorf("text = %q, want absent", got)
				}
				return
			}
			if !ok {
				t.Fatalf("text key missing, want %q", tc.wantText)
			}
			if got != tc.wantText {
				t.Errorf("text = %q, want %q", got, tc.wantText)
			}
			// The file id, when present, must survive into the lineage.
			if tc.fileID != "" && !strings.Contains(got, tc.fileID) {
				t.Errorf("text %q does not preserve file id %q", got, tc.fileID)
			}
		})
	}
}

func TestReadLimited(t *testing.T) {
	r := strings.NewReader("hello world this is long")
	got := readLimited(r, 5)
	if string(got) != "hello" {
		t.Errorf("readLimited = %q, want %q", string(got), "hello")
	}
}
