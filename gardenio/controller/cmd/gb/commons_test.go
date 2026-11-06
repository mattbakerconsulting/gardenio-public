package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

const commonsTestFileID = "0123456789abcdef0123456789abcdef"

var commonsTestPNG = []byte("\x89PNG\r\n\x1a\nfarm")

// stubViewer serves the machine file route for one file of land gardenio, to
// the bearer "agent-token" only, as beads-viewer does for an agent of that
// land (an agent of another land gets 403).
func stubViewer(t *testing.T) (*httptest.Server, *[]string) {
	t.Helper()
	var asked []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = append(asked, r.URL.Path)
		if r.Header.Get("Authorization") != "Bearer agent-token" {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		if r.URL.Path != "/internal/commons/f/gardenio/gd-ch1/"+commonsTestFileID+"/farm.png" {
			http.Error(w, "That file is no longer here.", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(commonsTestPNG)
	}))
	t.Cleanup(srv.Close)
	return srv, &asked
}

func fileLink(land, name string) string {
	return "https://gardenio.example/beads/garden/c/f/" + land + "/gd-ch1/" + commonsTestFileID + "/" + name
}

func messageBead(files, snap string) func(context.Context, string) (*beadsapi.BeadDetail, error) {
	return func(_ context.Context, id string) (*beadsapi.BeadDetail, error) {
		f := map[string]string{"files": files}
		if snap != "" {
			f["snap"] = snap
		}
		return &beadsapi.BeadDetail{ID: id, Type: "message", Fields: f}, nil
	}
}

// A message id fetches its Commons files into the scratch dir on the agent's
// token, skips its Slack permalinks, and reports the snap (gd-WXY7Jm5ugY).
func TestCommonsFile_Message(t *testing.T) {
	srv, asked := stubViewer(t)
	tmp := t.TempDir()
	files := `[{"name":"farm.png","mimetype":"image/png","size":12,"url":"` + fileLink("gardenio", "farm.png") + `"},` +
		`{"name":"s.png","url":"https://acme.slack.com/files/U1/F1/s.png"}]`
	snap := `{"kind":"farm","url":"https://gardenio.example/garden?at=yard","at":"yard"}`
	f := commonsFileFetch{viewer: srv.URL, token: "agent-token", tmp: tmp, getBead: messageBead(files, snap), client: srv.Client()}

	res, err := f.run(context.Background(), "gd-msg1")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(tmp, "commons-files", "gd-msg1", "farm.png")
	if len(res.Files) != 1 || res.Files[0].Path != want || res.Files[0].MimeType != "image/png" || res.Files[0].Size != int64(len(commonsTestPNG)) {
		t.Fatalf("files = %+v, want one at %s", res.Files, want)
	}
	if got, _ := os.ReadFile(want); !bytes.Equal(got, commonsTestPNG) {
		t.Errorf("written bytes = %q", got)
	}
	if len(*asked) != 1 {
		t.Errorf("viewer asked %v, want only the Commons file", *asked)
	}
	out, _ := json.Marshal(res)
	for _, s := range []string{`"message":"gd-msg1"`, `"snap":{"kind":"farm"`, `"path":"` + want + `"`, `"name":"farm.png"`} {
		if !strings.Contains(string(out), s) {
			t.Errorf("json %s lacks %s", out, s)
		}
	}
}

// A file URL fetches that one file, to --out as a file path.
func TestCommonsFile_URLToPath(t *testing.T) {
	srv, _ := stubViewer(t)
	out := filepath.Join(t.TempDir(), "look.png")
	f := commonsFileFetch{viewer: srv.URL, token: "agent-token", out: out, client: srv.Client()}
	res, err := f.run(context.Background(), fileLink("gardenio", "farm.png"))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Files) != 1 || res.Files[0].Path != out || res.Snap != nil {
		t.Fatalf("result = %+v", res)
	}
	if got, _ := os.ReadFile(out); !bytes.Equal(got, commonsTestPNG) {
		t.Errorf("written bytes = %q", got)
	}
}

// The viewer's refusal is the command's error, with what it means, and no
// file is left behind.
func TestCommonsFile_Refused(t *testing.T) {
	srv, _ := stubViewer(t)
	dir := t.TempDir()
	f := commonsFileFetch{viewer: srv.URL, token: "other-land-token", out: dir, client: srv.Client()}
	_, err := f.run(context.Background(), fileLink("gardenio", "farm.png"))
	if err == nil || !strings.Contains(err.Error(), "403") || !strings.Contains(err.Error(), "own land") {
		t.Fatalf("err = %v, want the 403 explained", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("left %v behind", entries)
	}
}

func TestCommonsFile_NotAttachments(t *testing.T) {
	f := commonsFileFetch{viewer: "http://viewer", token: "t", tmp: t.TempDir()}
	if _, err := f.run(context.Background(), "https://acme.slack.com/files/U1/F1/s.png"); err == nil {
		t.Error("a Slack permalink was fetched")
	}
	f.getBead = messageBead(`[{"name":"s.png","url":"https://acme.slack.com/files/U1/F1/s.png"}]`, "")
	if _, err := f.run(context.Background(), "gd-msg2"); err == nil || !strings.Contains(err.Error(), "no Commons attachments") {
		t.Errorf("err = %v", err)
	}
	f.getBead = func(context.Context, string) (*beadsapi.BeadDetail, error) {
		return &beadsapi.BeadDetail{ID: "gd-t1", Type: "task"}, nil
	}
	if _, err := f.run(context.Background(), "gd-t1"); err == nil || !strings.Contains(err.Error(), "not a Commons message") {
		t.Errorf("err = %v", err)
	}
	// A name that would climb out of --out never reaches the disk.
	if _, err := f.run(context.Background(), "https://g.example/garden/c/f/gardenio/gd-ch1/"+commonsTestFileID+"/..%2F..%2Fx"); err == nil {
		t.Error("an unsafe file name was accepted")
	}
}
