package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSanitizeFilename(t *testing.T) {
	cases := map[string]string{
		"foo.png":         "foo.png",
		"a/b.png":         "b.png",
		"../../etc/passwd": "passwd",
		".":               "file",
		"":                "file",
	}
	for in, want := range cases {
		got := sanitizeFilename(in)
		if got != want {
			t.Errorf("sanitizeFilename(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFilenameFromContentDisposition(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{`attachment; filename="hello.png"`, "hello.png"},
		{`attachment; filename=hello.png`, "hello.png"},
		{`inline`, ""},
		{``, ""},
	}
	for _, c := range cases {
		got := filenameFromContentDisposition(c.in)
		if got != c.want {
			t.Errorf("filenameFromContentDisposition(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestResolveFileProxyURL(t *testing.T) {
	got := resolveFileProxyURL("http://bridge:8090/", "F12345")
	want := "http://bridge:8090/api/slack/files/F12345"
	if got != want {
		t.Errorf("resolveFileProxyURL = %q, want %q", got, want)
	}
}

func TestDownloadProxyFile(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/slack/files/") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Content-Disposition", `attachment; filename="screenshot.png"`)
		_, _ = w.Write([]byte("PNGDATA"))
	}))
	defer server.Close()

	dir := t.TempDir()
	dest := filepath.Join(dir, "out.png")
	final, err := downloadProxyFile(server.URL, "F12345", "screenshot.png", dest)
	if err != nil {
		t.Fatalf("downloadProxyFile: %v", err)
	}
	if final != dest {
		t.Errorf("final = %q, want %q", final, dest)
	}
	data, err := os.ReadFile(final)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(data) != "PNGDATA" {
		t.Errorf("downloaded body = %q, want %q", string(data), "PNGDATA")
	}
}

func TestDownloadProxyFile_DefaultPath(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte("BODY"))
	}))
	defer server.Close()

	tmp := t.TempDir()
	wd, _ := os.Getwd()
	defer os.Chdir(wd)
	if err := os.Chdir(tmp); err != nil {
		t.Fatal(err)
	}

	final, err := downloadProxyFile(server.URL, "F999", "img.png", "")
	if err != nil {
		t.Fatalf("downloadProxyFile: %v", err)
	}
	want := filepath.Join("slack-files", "F999-img.png")
	if final != want {
		t.Errorf("final = %q, want %q", final, want)
	}
	if _, err := os.Stat(final); err != nil {
		t.Errorf("expected file at %s, got: %v", final, err)
	}
}

func TestDownloadProxyFile_HTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"file not found"}`, http.StatusNotFound)
	}))
	defer server.Close()

	dir := t.TempDir()
	dest := filepath.Join(dir, "nope.png")
	_, err := downloadProxyFile(server.URL, "F404", "nope.png", dest)
	if err == nil {
		t.Fatal("expected error on 404, got nil")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("error should mention status code, got: %v", err)
	}
}

func TestListThreadFiles(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/slack/threads") {
			http.NotFound(w, r)
			return
		}
		msgs := []slackThreadMessage{
			{Author: "alice", Text: "look at this", Files: []slackFileInfo{
				{ID: "F1", Name: "screenshot.png", Mimetype: "image/png", Size: 1234, ProxyURL: "/api/slack/files/F1"},
			}},
			{Author: "bob", Text: "and this"},
			{Author: "carol", Text: "also", Files: []slackFileInfo{
				{ID: "F2", Name: "log.txt", Mimetype: "text/plain", Size: 99, ProxyURL: "/api/slack/files/F2"},
			}},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(msgs)
	}))
	defer server.Close()

	t.Setenv("SLACK_BRIDGE_URL", server.URL)
	t.Setenv("SLACK_THREAD_CHANNEL", "C123")
	t.Setenv("SLACK_THREAD_TS", "111.222")

	files, err := listThreadFiles()
	if err != nil {
		t.Fatalf("listThreadFiles: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("got %d files, want 2", len(files))
	}
	if files[0].ID != "F1" || files[1].ID != "F2" {
		t.Errorf("file IDs = [%s %s], want [F1 F2]", files[0].ID, files[1].ID)
	}
}

func TestFormatThreadMarkdown_SurfacesAttachments(t *testing.T) {
	msgs := []slackThreadMessage{
		{Author: "alice", Text: "screenshot incoming", Files: []slackFileInfo{
			{ID: "F1", Name: "ui.png", Mimetype: "image/png", Size: 2048},
		}},
	}
	out := formatThreadMarkdown(msgs, 4000, true, nil)
	if !strings.Contains(out, "[attachment: ui.png (image/png") {
		t.Errorf("expected attachment hint, got: %q", out)
	}
	if !strings.Contains(out, "file_id=F1") {
		t.Errorf("expected file_id=F1, got: %q", out)
	}
	if !strings.Contains(out, "gb slack files fetch-all") {
		t.Errorf("expected fetch-all tip, got: %q", out)
	}
}

func TestFormatThreadMarkdown_NoFilesNoTip(t *testing.T) {
	msgs := []slackThreadMessage{
		{Author: "alice", Text: "no attachments"},
	}
	out := formatThreadMarkdown(msgs, 4000, true, nil)
	if strings.Contains(out, "fetch-all") {
		t.Errorf("did not expect fetch-all tip when no files, got: %q", out)
	}
}
