package main

import (
	"context"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// oldBridge wraps a legacy-routes-only bridge stub as a bridge that predates
// /api/conversation: the mux answers those routes with a plain-text 404.
func oldBridge(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/conversation/") {
			http.NotFound(w, r)
			return
		}
		h(w, r)
	}
}

// sayBridge is a bridge stub that serves POST /api/conversation/say and
// records what it got.
type sayBridge struct {
	paths  []string
	json   map[string]string
	fields map[string]string
	files  map[string]string
	resp   string
}

func (b *sayBridge) serve(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b.paths = append(b.paths, r.Method+" "+r.URL.Path)
		if r.URL.Path != "/api/conversation/say" {
			http.NotFound(w, r)
			return
		}
		mediaType, params, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if strings.HasPrefix(mediaType, "multipart/") {
			b.fields, b.files = map[string]string{}, map[string]string{}
			mr := multipart.NewReader(r.Body, params["boundary"])
			for {
				part, err := mr.NextPart()
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Errorf("reading multipart: %v", err)
					return
				}
				data, _ := io.ReadAll(part)
				if part.FormName() == "file" {
					b.files[part.FileName()] = string(data)
				} else {
					b.fields[part.FormName()] = string(data)
				}
			}
		} else if err := json.NewDecoder(r.Body).Decode(&b.json); err != nil {
			t.Errorf("decoding say body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		resp := b.resp
		if resp == "" {
			resp = `{"ok":true,"id":"gd-SAY1","channel":"C999","timestamp":"111.300"}`
		}
		_, _ = io.WriteString(w, resp)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func setSayEnv(t *testing.T, bridgeURL string) {
	t.Helper()
	t.Setenv("SLACK_BRIDGE_URL", bridgeURL)
	t.Setenv("SLACK_THREAD_CHANNEL", "C999")
	t.Setenv("SLACK_THREAD_TS", "111.222")
	t.Setenv("BOAT_AGENT_BEAD_ID", "gd-AGENT1")
	t.Setenv("KD_AGENT_ID", "")
	t.Setenv("BOAT_TRIGGER_BEAD_ID", "gd-TRIG1")
	t.Setenv("BOAT_PROJECT", "gardenio")
	old := actor
	actor = "crew-say"
	t.Cleanup(func() { actor = old })
	resetBridgeAgent(t)
}

// resetSayFlags clears gb say's package-level flag values.
func resetSayFlags(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		sayThread, sayChannel, sayFiles, sayTitle, sayAltText = "", "", nil, "", ""
		sayTo, sayAsk, sayTask, sayWake = "", false, "", false
		squawkNoComment, squawkToMR = false, false
	})
}

// gb slack reply is gb say into the home thread: one POST to
// /api/conversation/say naming the Slack thread, and no bead written by gb
// (the bridge writes it).
func TestSlackReply_SaysThroughConversation(t *testing.T) {
	b := &sayBridge{}
	setSayEnv(t, b.serve(t).URL)
	slackReplyCmd.SetContext(context.Background())

	out := captureStdout(t, func() error { return runSlackReply(slackReplyCmd, []string{"  on it\n"}) })

	if len(b.paths) != 1 || b.paths[0] != "POST /api/conversation/say" {
		t.Fatalf("bridge calls = %v, want one say", b.paths)
	}
	want := map[string]string{
		"agent": "crew-say", "agent_bead": "gd-AGENT1", "trigger_bead": "gd-TRIG1", "land": "gardenio",
		"channel": "C999", "thread_ts": "111.222", "text": "  on it\n",
	}
	for k, v := range want {
		if b.json[k] != v {
			t.Errorf("say %s = %q, want %q", k, b.json[k], v)
		}
	}
	if b.json["thread"] != "" {
		t.Errorf("say thread = %q, want none (Slack coordinates name the home thread)", b.json["thread"])
	}
	if strings.TrimSpace(string(out)) != "Reply posted." {
		t.Errorf("output = %q, want the alias's own", out)
	}
}

// A bridge's JSON refusal is the answer: no fallback to the legacy route.
func TestSlackReply_ConversationRefusalIsNotRetried(t *testing.T) {
	b := &sayBridge{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b.paths = append(b.paths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":"thread gd-X is in land \"other\""}`)
	}))
	t.Cleanup(srv.Close)
	setSayEnv(t, srv.URL)

	err := runSlackReply(slackReplyCmd, []string{"hi"})
	if err == nil || !strings.Contains(err.Error(), "403") || !strings.Contains(err.Error(), `land "other"`) {
		t.Fatalf("err = %v, want the bridge's 403 and its message", err)
	}
	if len(b.paths) != 1 {
		t.Fatalf("bridge calls = %v, want only the say", b.paths)
	}
}

// A bridge that predates /api/conversation gets the old reply route.
func TestSlackReply_OldBridgeFallsBack(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(oldBridge(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(slackReplyResult{OK: true, Timestamp: "111.225"})
	}))
	t.Cleanup(srv.Close)
	setSayEnv(t, srv.URL)

	captureStdout(t, func() error { return runSlackReply(slackReplyCmd, []string{"hi"}) })
	if len(paths) != 1 || paths[0] != "/api/slack/threads/reply" {
		t.Fatalf("legacy calls = %v, want the reply route", paths)
	}
}

func TestSay_Thread(t *testing.T) {
	resetSayFlags(t)
	b := &sayBridge{}
	setSayEnv(t, b.serve(t).URL)
	sayThread = "gd-HEAD1"
	squawkNoComment = true

	out := captureStdout(t, func() error { return runSay(sayCmd, []string{"see the chart"}) })

	if b.json["thread"] != "gd-HEAD1" || b.json["text"] != "see the chart" {
		t.Fatalf("say = %v, want a reply in gd-HEAD1", b.json)
	}
	if b.json["channel"] != "" || b.json["thread_ts"] != "" {
		t.Errorf("say = %v: an explicit thread must not carry the home thread's coordinates", b.json)
	}
	if strings.TrimSpace(string(out)) != "OK gd-SAY1" {
		t.Errorf("output = %q, want the message bead id", out)
	}
}

// gb say --thread <head> --wake asks the bridge to wake the thread's agent too,
// and prints what the bridge did with it.
func TestSay_Wake(t *testing.T) {
	resetSayFlags(t)
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decoding say body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true,"id":"gd-SAY1","woke":"Waking thread-ci with this thread's conversation."}`)
	}))
	t.Cleanup(srv.Close)
	setSayEnv(t, srv.URL)
	sayThread, sayWake = "gd-HEAD1", true
	squawkNoComment = true

	out := captureStdout(t, func() error { return runSay(sayCmd, []string{"CI is still red"}) })

	if got["thread"] != "gd-HEAD1" || got["wake"] != true || got["text"] != "CI is still red" {
		t.Fatalf("say = %v, want a wake in gd-HEAD1", got)
	}
	if !strings.Contains(string(out), "OK gd-SAY1") || !strings.Contains(string(out), "Waking thread-ci") {
		t.Errorf("output = %q, want the message id and the wake", out)
	}
}

func TestSay_Channel(t *testing.T) {
	resetSayFlags(t)
	b := &sayBridge{}
	setSayEnv(t, b.serve(t).URL)
	sayChannel = "#general"
	squawkNoComment = true

	captureStdout(t, func() error { return runSay(sayCmd, []string{"release cut"}) })

	if b.json["channel"] != "#general" || b.json["thread_ts"] != "" || b.json["thread"] != "" {
		t.Fatalf("say = %v, want a top-level post in #general", b.json)
	}
}

// gb say --file with no target goes to the home thread, as a multipart say.
func TestSay_FileToHomeThread(t *testing.T) {
	resetSayFlags(t)
	b := &sayBridge{resp: `{"ok":true,"id":"gd-SAY2","channel":"C999"}`}
	setSayEnv(t, b.serve(t).URL)
	sayFiles = []string{writeTempFile(t, "shot.png", []byte("PNGDATA"))}
	sayTitle, sayAltText = "Login page", "the login form"
	squawkNoComment = true

	captureStdout(t, func() error { return runSay(sayCmd, []string{"here it is"}) })

	want := map[string]string{
		"agent": "crew-say", "channel": "C999", "thread_ts": "111.222", "text": "here it is",
		"title": "Login page", "alt_txt": "the login form", "land": "gardenio",
	}
	for k, v := range want {
		if b.fields[k] != v {
			t.Errorf("say field %s = %q, want %q", k, b.fields[k], v)
		}
	}
	if b.files["shot.png"] != "PNGDATA" {
		t.Errorf("files = %v, want shot.png", b.files)
	}
}

func TestSay_FlagErrors(t *testing.T) {
	for name, set := range map[string]func(){
		"no text":             func() { sayThread = "gd-H" },
		"title on two files":  func() { sayFiles, sayTitle = []string{"a", "b"}, "t" },
		"to-mr with thread":   func() { sayThread, squawkToMR = "gd-H", true },
		"ask without to":      func() { sayAsk = true },
		"task without ask":    func() { sayTo, sayTask = "ada@example.com", "gd-T" },
		"wake without thread": func() { sayChannel, sayWake = "crew", true },
	} {
		t.Run(name, func(t *testing.T) {
			resetSayFlags(t)
			setSayEnv(t, "http://127.0.0.1:0")
			set()
			args := []string{"x"}
			if name == "no text" {
				args = []string{"  "}
			}
			if err := runSay(sayCmd, args); err == nil {
				t.Fatal("want an error")
			}
		})
	}
}

// gb slack upload is gb say --file into the home thread.
func TestSlackUpload_SaysThroughConversation(t *testing.T) {
	b := &sayBridge{}
	setSayEnv(t, b.serve(t).URL)
	cmd := newSlackUploadCmd()
	_ = cmd.Flags().Set("comment", "hello there")
	path := writeTempFile(t, "shot.png", []byte("PNGDATA"))

	out := captureStdout(t, func() error { return runSlackUpload(cmd, []string{path}) })

	if len(b.paths) != 1 || b.fields["text"] != "hello there" || b.fields["thread_ts"] != "111.222" || b.files["shot.png"] != "PNGDATA" {
		t.Fatalf("calls %v, fields %v, files %v: want one multipart say", b.paths, b.fields, b.files)
	}
	if !strings.Contains(string(out), "Uploaded shot.png to thread.") {
		t.Errorf("output = %q", out)
	}
}

// threadBridge serves GET /api/conversation/thread.
func threadBridge(t *testing.T, got *string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/conversation/thread" {
			http.NotFound(w, r)
			return
		}
		*got = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"thread":"gd-HEAD1","source":"beads","messages":[`+
			`{"author":"ada@example.com","text":"why is CI red?","timestamp":"1.000001"},`+
			`{"author":"crew-say","text":"progress","timestamp":"1.000002","is_bot":true,"subtype":"squawk"},`+
			`{"author":"crew-say","text":"a flaky test; rerun","timestamp":"1.000003","is_bot":true}]}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestThread_ByHead(t *testing.T) {
	var query string
	setSayEnv(t, threadBridge(t, &query).URL)

	out := string(captureStdout(t, func() error { return runThread(threadCmd, []string{"gd-HEAD1"}) }))

	if !strings.Contains(query, "thread=gd-HEAD1") || strings.Contains(query, "channel=") {
		t.Errorf("query = %q, want the head only", query)
	}
	if !strings.Contains(query, "agent=crew-say") {
		t.Errorf("query = %q, want the reading agent for the bridge's land wall", query)
	}
	if !strings.Contains(out, "**ada@example.com**: why is CI red?") || !strings.Contains(out, "a flaky test; rerun") {
		t.Errorf("output = %q, want the thread", out)
	}
	if strings.Contains(out, "progress") {
		t.Errorf("output = %q: squawks are noise by default", out)
	}
}

// gb slack thread is gb thread for the home thread: the Slack coordinates.
func TestSlackThread_ReadsThroughConversation(t *testing.T) {
	var query string
	setSayEnv(t, threadBridge(t, &query).URL)

	captureStdout(t, func() error { return runSlackThread(slackThreadCmd, nil) })

	if !strings.Contains(query, "channel=C999") || !strings.Contains(query, "ts=111.222") || strings.Contains(query, "source=") {
		t.Errorf("query = %q, want the home thread's coordinates and no source", query)
	}
}

func TestConversationUnavailable(t *testing.T) {
	for _, c := range []struct {
		status int
		body   string
		want   bool
	}{
		{http.StatusNotFound, "404 page not found\n", true},
		{http.StatusForbidden, "insufficient permissions\n", true},
		{http.StatusUnauthorized, "unauthorized\n", true},
		{http.StatusNotFound, `{"error":"thread gd-X not found"}`, false},
		{http.StatusForbidden, `{"error":"other land"}`, false},
		{http.StatusBadGateway, "bad gateway", false},
	} {
		got := conversationUnavailable(&http.Response{StatusCode: c.status}, []byte(c.body))
		if got != c.want {
			t.Errorf("conversationUnavailable(%d, %q) = %v, want %v", c.status, c.body, got, c.want)
		}
	}
}

// gb say --to --ask is one JSON say naming the target, with the task from
// BOAT_TASK_ID, and no home-thread coordinates: the DM is its own channel.
func TestSay_ToAsk(t *testing.T) {
	resetSayFlags(t)
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decoding say body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true,"id":"gd-ASK1","dm":"gd-DM1","channel":"D0ADA","timestamp":"1.2"}`)
	}))
	t.Cleanup(srv.Close)
	setSayEnv(t, srv.URL)
	t.Setenv("BOAT_TASK_ID", "gd-TASK1")
	sayTo, sayAsk = "ada@example.com", true
	squawkNoComment = true

	out := captureStdout(t, func() error { return runSay(sayCmd, []string{"ship it?"}) })

	if got["to"] != "ada@example.com" || got["ask"] != true || got["task"] != "gd-TASK1" || got["text"] != "ship it?" {
		t.Fatalf("say = %v, want an ask to ada about gd-TASK1", got)
	}
	if got["channel"] != nil || got["thread_ts"] != nil || got["thread"] != nil {
		t.Errorf("say = %v: a DM must not carry the home thread", got)
	}
	if !strings.Contains(string(out), "OK gd-ASK1") || !strings.Contains(string(out), "gb thread gd-ASK1") {
		t.Errorf("output = %q, want the ask id and how to read the answer", out)
	}
}

// A bridge that predates --to answers a say with no DM: gb says so rather
// than reporting a DM that never went.
func TestSay_ToOldBridge(t *testing.T) {
	resetSayFlags(t)
	b := &sayBridge{}
	setSayEnv(t, b.serve(t).URL)
	sayTo = "ada@example.com"
	squawkNoComment = true
	if err := runSay(sayCmd, []string{"hi"}); err == nil || !strings.Contains(err.Error(), "predates gb say --to") {
		t.Fatalf("err = %v, want the old-bridge hint", err)
	}
}
