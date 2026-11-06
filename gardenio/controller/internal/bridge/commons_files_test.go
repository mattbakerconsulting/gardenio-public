package bridge

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/slack-go/slack"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/conversation/surfacetest"
)

type staticTokens string

func (s staticTokens) Token(context.Context) (string, error) { return string(s), nil }

// fileID32 is a Commons file id (32 lowercase hex digits).
const fileID32 = "0123456789abcdef0123456789abcdef"

// stubViewer serves the machine file route from a path -> body table, only to
// the bearer it expects, and records the paths asked for.
type stubViewer struct {
	files map[string]string
	mu    sync.Mutex
	asked []string
}

func (v *stubViewer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	v.mu.Lock()
	v.asked = append(v.asked, r.URL.Path)
	v.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer machine-tok" {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	body, ok := v.files[r.URL.Path]
	if !ok {
		http.Error(w, "boom", http.StatusInternalServerError)
		return
	}
	_, _ = w.Write([]byte(body))
}

func viewerFileURL(land, ch, name string) string {
	return "https://gardenio.example/beads/garden/c/f/" + land + "/" + ch + "/" + fileID32 + "/" + name
}

func callsOf(twin *surfacetest.SlackTwin, method string) []surfacetest.TwinCall {
	var out []surfacetest.TwinCall
	for _, c := range twin.Calls() {
		if c.Method == method {
			out = append(out, c)
		}
	}
	return out
}

// TestCommonsMirror_UploadsFetchedFiles pins gd-qnF6mJZtkQ: a Commons
// message's files are fetched from the viewer's machine route on the bridge's
// machine principal and uploaded into Slack with the attributed text; a file
// over the cap, one whose fetch fails, and one of another channel stay links.
func TestCommonsMirror_UploadsFetchedFiles(t *testing.T) {
	b, daemon, twin := newMirrorBot(t)
	ctx := context.Background()
	prefix := "/internal/commons/f/gardenio/gd-chan-out/" + fileID32 + "/"
	viewer := &stubViewer{files: map[string]string{
		prefix + "a.png":    "PNGBYTES",
		prefix + "huge.bin": strings.Repeat("x", 64), // over the cap, though its ref says nothing
	}}
	srv := httptest.NewServer(viewer)
	defer srv.Close()
	files := NewCommonsFileFetcher(srv.URL+"/", staticTokens("machine-tok"), slog.Default())
	files.maxFile = 32
	mirror := NewCommonsMirror(CommonsMirrorConfig{Router: b.Conversation(), Bot: b, Files: files, Logger: slog.Default()})

	d := commonsBead("gd-cm-up", "gd-chan-out", "", "see attached")
	d.Fields["files"] = `[` +
		`{"name":"a.png","mimetype":"image/png","size":8,"url":"` + viewerFileURL("gardenio", "gd-chan-out", "a.png") + `"},` +
		`{"name":"big.pdf","size":4096,"url":"` + viewerFileURL("gardenio", "gd-chan-out", "big.pdf") + `"},` +
		`{"name":"huge.bin","url":"` + viewerFileURL("gardenio", "gd-chan-out", "huge.bin") + `"},` +
		`{"name":"broken.txt","url":"` + viewerFileURL("gardenio", "gd-chan-out", "broken.txt") + `"},` +
		`{"name":"foreign.png","url":"` + viewerFileURL("claimlayer", "cl-chgen", "foreign.png") + `"}]`
	daemon.beads[d.ID] = d
	mirror.handleEvent(ctx, beadEvent(d))

	if ps := posts(twin); len(ps) != 0 {
		t.Errorf("an upload must not also chat.postMessage: %+v", ps)
	}
	ups := callsOf(twin, "upload")
	if len(ups) != 1 || ups[0].Params["filename"] != "a.png" || ups[0].Params["content"] != "PNGBYTES" {
		t.Fatalf("uploads = %+v, want a.png's bytes only", ups)
	}
	done := callsOf(twin, "files.completeUploadExternal")
	if len(done) != 1 || done[0].Params["channel_id"] != "C0OUT" {
		t.Fatalf("completeUploadExternal = %+v", done)
	}
	want := "*ada* (via Commons)\nsee attached\n" +
		"<" + viewerFileURL("gardenio", "gd-chan-out", "big.pdf") + "|big.pdf>\n" +
		"<" + viewerFileURL("gardenio", "gd-chan-out", "huge.bin") + "|huge.bin>\n" +
		"<" + viewerFileURL("gardenio", "gd-chan-out", "broken.txt") + "|broken.txt>\n" +
		"<" + viewerFileURL("claimlayer", "cl-chgen", "foreign.png") + "|foreign.png>"
	if got := done[0].Params["initial_comment"]; got != want {
		t.Errorf("initial_comment =\n%q\nwant\n%q", got, want)
	}
	for _, p := range viewer.asked {
		if strings.Contains(p, "big.pdf") || strings.Contains(p, "foreign") {
			t.Errorf("fetched %s: a file over the cap by its ref, or of another channel, is never fetched", p)
		}
	}
}

// With no fetcher, or a viewer that refuses every fetch, the files stay links
// and the post is the one it always was.
func TestCommonsMirror_FetchFailureKeepsLinks(t *testing.T) {
	b, daemon, twin := newMirrorBot(t)
	ctx := context.Background()
	srv := httptest.NewServer(&stubViewer{})
	defer srv.Close()
	mirror := NewCommonsMirror(CommonsMirrorConfig{Router: b.Conversation(), Bot: b,
		Files: NewCommonsFileFetcher(srv.URL, staticTokens("wrong-tok"), slog.Default()), Logger: slog.Default()})

	d := commonsBead("gd-cm-nofetch", "gd-chan-out", "", "")
	d.Title = "Shared a.png"
	d.Fields["files"] = `[{"name":"a.png","url":"` + viewerFileURL("gardenio", "gd-chan-out", "a.png") + `"}]`
	daemon.beads[d.ID] = d
	mirror.handleEvent(ctx, beadEvent(d))

	if n := len(callsOf(twin, "files.completeUploadExternal")); n != 0 {
		t.Errorf("uploaded %d times after a refused fetch", n)
	}
	msgs := twin.Messages("C0OUT")
	if want := "*ada* (via Commons)\n<" + viewerFileURL("gardenio", "gd-chan-out", "a.png") + "|a.png>"; len(msgs) != 1 || msgs[0][1] != want {
		t.Errorf("C0OUT = %v, want the linked post %q", msgs, want)
	}
	if NewCommonsFileFetcher("", staticTokens("t"), nil) != nil || NewCommonsFileFetcher("http://v", nil, nil) != nil {
		t.Error("a fetcher with no viewer URL or no machine principal must be nil")
	}
}

// The sweep knows an upload copy: the attributed text with links for only the
// files Slack does not show on it.
func TestCommonsMirror_IsCopyOfAnUpload(t *testing.T) {
	a, pdf := fileRef{Name: "a.png", URL: "https://v.example/a.png"}, fileRef{Name: "b.pdf", URL: "https://v.example/b.pdf"}
	p := projection{text: "see attached", files: []fileRef{a, pdf}, local: "ada",
		attributed: viaCommonsWithFiles("Ada", "see attached", []fileRef{a, pdf})}
	p.msg.Author.Name = "Ada"

	upload := slack.Message{Msg: slack.Msg{Text: "*Ada* (via Commons)\nsee attached\n<https://v.example/b.pdf|b.pdf>", Files: []slack.File{{Name: "a.png"}}}}
	if !p.isCopyOf(upload) {
		t.Error("an upload of a.png with b.pdf linked is a copy")
	}
	local := slack.Message{Msg: slack.Msg{Text: "*ada* (via Commons)\nsee attached", Files: []slack.File{{Name: "a.png"}, {Name: "b.pdf"}}}}
	if !p.isCopyOf(local) {
		t.Error("an upload of both under the email local part is a copy")
	}
	other := slack.Message{Msg: slack.Msg{Text: "*Ada* (via Commons)\nsee attached", Files: []slack.File{{Name: "c.png"}}}}
	if p.isCopyOf(other) {
		t.Error("a post sharing none of p's files is not a copy")
	}
}

// TestThreadMessageForward_FileOnlyReplyLinksFiles pins part 2 of
// gd-qnF6mJZtkQ: a file-only Commons reply into a Slack-bound wish posts its
// files' links, not just "Shared a.png".
func TestThreadMessageForward_FileOnlyReplyLinksFiles(t *testing.T) {
	b, _, twin := newMirrorBot(t)
	fwd := NewThreadMessageForward(ThreadMessageForwardConfig{Router: b.Conversation(), Logger: slog.Default()})
	url := viewerFileURL("gardenio", "gd-chan-both", "a.png")
	fwd.handleClosed(context.Background(), marshalSSEBeadPayload(BeadEvent{
		ID: "gd-pane-file", Type: "message", Status: "closed", Title: "Shared a.png", CreatedBy: "ada@example.com",
		Labels: []string{ThreadSayLabel, "project:gardenio"},
		Fields: map[string]string{"origin": "commons", "author": "oidc:ada@example.com",
			"files":                `[{"name":"a.png","mimetype":"image/png","url":"` + url + `"}]`,
			"slack_thread_channel": "C0LIVE", "slack_thread_ts": "1690000000.000100"},
	}))
	ps := posts(twin)
	if len(ps) != 1 {
		t.Fatalf("posts = %+v", ps)
	}
	if got, want := ps[0].Params["text"], "*ada* (via Commons)\n<"+url+"|a.png>"; got != want {
		t.Errorf("file-only reply text = %q, want %q", got, want)
	}
}

// TestThreadMessageForward_UploadsFetchedFiles pins gd-WxRuyfdUtR: a farm
// postcard is a Commons reply into the plan's Slack-bound thread, and its
// picture is uploaded into that thread, attributed in the text, with the view
// link (and any file it could not fetch) as links under the caption.
func TestThreadMessageForward_UploadsFetchedFiles(t *testing.T) {
	b, _, twin := newMirrorBot(t)
	viewer := &stubViewer{files: map[string]string{"/internal/commons/f/gardenio/gd-chan-both/" + fileID32 + "/postcard.png": "PNGBYTES"}}
	srv := httptest.NewServer(viewer)
	defer srv.Close()
	fwd := NewThreadMessageForward(ThreadMessageForwardConfig{Router: b.Conversation(),
		Files: NewCommonsFileFetcher(srv.URL, staticTokens("machine-tok"), slog.Default()), Logger: slog.Default()})
	png, pdf := viewerFileURL("gardenio", "gd-chan-both", "postcard.png"), viewerFileURL("gardenio", "gd-chan-both", "notes.pdf")
	view := "https://gardenio.example/beads/garden?at=gardenio%2Fgd-plan&z=2"
	fwd.handleClosed(context.Background(), marshalSSEBeadPayload(BeadEvent{
		ID: "gd-pane-postcard", Type: "message", Status: "closed", Title: "Postcard", CreatedBy: "ada@example.com",
		Labels: []string{ThreadSayLabel, "project:gardenio"},
		Fields: map[string]string{"origin": "commons", "author": "oidc:ada@example.com", "channel_id": "gd-chan-both", "text": "Postcard: Time-lapse",
			"files":                `[{"name":"postcard.png","mimetype":"image/png","size":8,"url":"` + png + `"},{"name":"notes.pdf","url":"` + pdf + `"}]`,
			"snap":                 `{"kind":"farm","url":"` + view + `","at":"gardenio/gd-plan"}`,
			"slack_thread_channel": "C0LIVE", "slack_thread_ts": "1690000000.000100"},
	}))
	if ps := posts(twin); len(ps) != 0 {
		t.Errorf("an upload must not also chat.postMessage: %+v", ps)
	}
	ups := callsOf(twin, "upload")
	if len(ups) != 1 || ups[0].Params["filename"] != "postcard.png" || ups[0].Params["content"] != "PNGBYTES" {
		t.Fatalf("uploads = %+v, want postcard.png's bytes only", ups)
	}
	done := callsOf(twin, "files.completeUploadExternal")
	if len(done) != 1 || done[0].Params["channel_id"] != "C0LIVE" || done[0].Params["thread_ts"] != "1690000000.000100" {
		t.Fatalf("completeUploadExternal = %+v, want into C0LIVE's thread", done)
	}
	want := "*ada* (via Commons)\nPostcard: Time-lapse\n<" + pdf + "|notes.pdf>\n<" + view + "|Open the farm view at gardenio/gd-plan>"
	if got := done[0].Params["initial_comment"]; got != want {
		t.Errorf("initial_comment =\n%q\nwant\n%q", got, want)
	}
}

// The text an agent is handed carries the files' links.
func TestCommonsMessageInbound_CarriesFileLinks(t *testing.T) {
	url := viewerFileURL("gardenio", "gd-chan-both", "a.png")
	m := commonsMessage{ID: "gd-cm-f", Type: "message", Title: "Shared a.png", Labels: []string{"project:gardenio"},
		Fields: map[string]string{"origin": "commons", "thread_id": "gd-wish",
			"files": `[{"name":"a.png","url":"` + url + `"}]`}}
	hint := "\nfetch with: gb commons file gd-cm-f"
	if got, want := m.inbound().Text, "Shared a.png\na.png: "+url+hint; got != want {
		t.Errorf("inbound text = %q, want %q", got, want)
	}
	m.Fields["text"] = "look"
	if got, want := m.inbound().Text, "look\na.png: "+url+hint; got != want {
		t.Errorf("inbound text = %q, want %q", got, want)
	}
}

// The text an agent is nudged with for a farm snapshot (gd-WXY7Jm5ugY): each
// file as name (mimetype, size): url, and the command that fetches the bytes
// on the agent's own token, after the view the snapshot framed (gd-GbF2JS04h5).
// A Slack file is a permalink gb cannot fetch, so a message with only those
// carries no hint.
func TestCommonsMessageInbound_AgentReadsFilesAndSnap(t *testing.T) {
	pic := viewerFileURL("gardenio", "gd-chan-both", "farm-gardenio-yard-20261020-0230.png")
	m := commonsMessage{ID: "gd-cm-snap", Type: "message", Title: "Snapshot of the farm at yard", Labels: []string{"project:gardenio"},
		Fields: map[string]string{"origin": "commons", "thread_id": "gd-wish",
			"text": "which plot is stuck?",
			"files": `[{"name":"farm-gardenio-yard-20261020-0230.png","mimetype":"image/png","size":48213,"url":"` + pic + `"},` +
				`{"name":"notes.txt","size":512,"url":"` + viewerFileURL("gardenio", "gd-chan-both", "notes.txt") + `"}]`,
			"snap": `{"kind":"farm","url":"https://garden.example/garden?at=yard","at":"yard","z":2}`}}
	want := "which plot is stuck?\n" +
		"farm-gardenio-yard-20261020-0230.png (image/png, 47.1 KB): " + pic + "\n" +
		"notes.txt (512 B): " + viewerFileURL("gardenio", "gd-chan-both", "notes.txt") + "\n" +
		"Open the farm view at the Yard: https://garden.example/garden?at=yard\n" +
		"fetch with: gb commons file gd-cm-snap"
	if got := m.inbound().Text; got != want {
		t.Errorf("inbound text =\n%s\nwant\n%s", got, want)
	}

	m.Fields["files"] = `[{"name":"s.png","mimetype":"image/png","size":3145728,"url":"https://acme.slack.com/files/U1/F1/s.png"}]`
	if got, want := m.inbound().Text, "which plot is stuck?\ns.png (image/png, 3.0 MB): https://acme.slack.com/files/U1/F1/s.png\n"+
		"Open the farm view at the Yard: https://garden.example/garden?at=yard"; got != want {
		t.Errorf("slack-file inbound text = %q, want %q", got, want)
	}
}

var _ beadsapi.TokenSource = staticTokens("")
