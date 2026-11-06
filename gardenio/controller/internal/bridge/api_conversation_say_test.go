package bridge

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"io/fs"
	"log/slog"
	"maps"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/conversation/surfacetest"

	"github.com/slack-go/slack"
)

var updateAgentSayGolden = flag.Bool("update-agent-say", false,
	"rewrite testdata/agent_say_calls.golden.json from the legacy reply/upload routes' recorded calls")

const (
	sayRootTS   = "1690000000.000100"
	sayChannel  = "C0CREWSAY"
	sayHeadID   = "gd-head1"
	sayChanBead = "gd-chan-crew"
)

// newSayAPI returns the agent API over a test Bot whose Slack client talks to
// twin, with one seeded channel bead (#crew in gardenio, bound to sayChannel)
// and one wish head in it, bound to the Slack thread sayRootTS. The bridge
// knows the speaking agent, crew-say, as an agent of gardenio.
func newSayAPI(t *testing.T, twin *surfacetest.SlackTwin) (*ConversationAgentAPI, *mockDaemon) {
	t.Helper()
	srv := newFakeSlackServer(t)
	t.Cleanup(srv.Close)
	daemon := newMockDaemon()
	b := newTestBot(daemon, srv)
	b.api = slack.New("xoxb-test", slack.OptionAPIURL(twin.Serve(t)))
	b.agentProject["crew-say"] = "gardenio"

	daemon.beads[sayChanBead] = &beadsapi.BeadDetail{
		ID: sayChanBead, Type: "channel", Status: "open", Labels: []string{"project:gardenio"},
		Fields: map[string]string{"name": "crew", "channel_kind": "crew", "slack_channel_id": sayChannel, "mirror": "both"},
	}
	daemon.beads[sayHeadID] = &beadsapi.BeadDetail{
		ID: sayHeadID, Type: "task", Status: "open", Title: "Slack thread: why is CI red?",
		Labels: []string{threadKindLabel, threadBeadLabel(sayChannel, sayRootTS), "project:gardenio"},
		Fields: map[string]string{
			"slack_thread_channel": sayChannel, "slack_thread_ts": sayRootTS,
			"channel_id": sayChanBead, "slack_root_text": "why is CI red?", "operated_by": "oidc:ada@example.com",
		},
	}
	return NewConversationAgentAPI(b, NewSlackThreadAPI(b.api, slog.Default()), slog.Default()), daemon
}

func (a *ConversationAgentAPI) mux() *http.ServeMux {
	mux := http.NewServeMux()
	a.RegisterRoutes(mux)
	return mux
}

func postSay(t *testing.T, h http.Handler, body map[string]string) (*httptest.ResponseRecorder, sayResponse) {
	t.Helper()
	raw, _ := json.Marshal(body)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/conversation/say", bytes.NewReader(raw)))
	var resp sayResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	return rr, resp
}

// multipartBody is a multipart form of fields and one file.
func multipartBody(t *testing.T, fields map[string]string, filename, content string) (*bytes.Buffer, string) {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	for _, k := range slices.Sorted(maps.Keys(fields)) {
		if err := mw.WriteField(k, fields[k]); err != nil {
			t.Fatal(err)
		}
	}
	part, err := mw.CreateFormFile("file", filename)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = part.Write([]byte(content))
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	return &body, mw.FormDataContentType()
}

func sayAgentBody(extra map[string]string) map[string]string {
	body := map[string]string{"agent": "crew-say", "agent_bead": "gd-agent1", "trigger_bead": "gd-trig1", "land": "gardenio"}
	for k, v := range extra {
		body[k] = v
	}
	return body
}

// sayMessageBeads returns the message beads the router wrote.
func sayMessageBeads(d *mockDaemon) []*beadsapi.BeadDetail {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []*beadsapi.BeadDetail
	for _, b := range d.beads {
		if b.Type == "message" {
			out = append(out, b)
		}
	}
	return out
}

// TestAgentSayCallsGolden is slice 5's Slack payload golden (design spec
// section 5.2): gb slack reply and gb slack upload move from the
// /api/slack/threads routes to /api/conversation/say, and the Slack Web API
// calls must not change. The golden is recorded from the legacy routes; the
// say route must send byte-identical calls.
// Regenerate with: go test ./internal/bridge -run TestAgentSayCallsGolden -update-agent-say
func TestAgentSayCallsGolden(t *testing.T) {
	const file, comment = "PNGDATA", "here is the login page"

	// Today: the legacy routes.
	legacyTwin := surfacetest.NewSlackTwin()
	legacy := NewSlackThreadAPI(slack.New("xoxb-test", slack.OptionAPIURL(legacyTwin.Serve(t))), slog.Default())
	legacyMux := http.NewServeMux()
	legacy.RegisterRoutes(legacyMux)
	raw, _ := json.Marshal(map[string]string{"channel": sayChannel, "thread_ts": sayRootTS, "text": "  on it\n"})
	rr := httptest.NewRecorder()
	legacyMux.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/slack/threads/reply", bytes.NewReader(raw)))
	if rr.Code != http.StatusOK {
		t.Fatalf("legacy reply: %d %s", rr.Code, rr.Body)
	}
	body, ct := multipartBody(t, map[string]string{
		"channel": sayChannel, "thread_ts": sayRootTS, "initial_comment": comment, "title": "Login page", "alt_txt": "the login form",
	}, "shot.png", file)
	req := httptest.NewRequest(http.MethodPost, "/api/slack/threads/upload", body)
	req.Header.Set("Content-Type", ct)
	rr = httptest.NewRecorder()
	legacyMux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("legacy upload: %d %s", rr.Code, rr.Body)
	}

	// After: the say route, as gb slack reply and gb slack upload call it.
	sayTwin := surfacetest.NewSlackTwin()
	a, _ := newSayAPI(t, sayTwin)
	mux := a.mux()
	if rr, _ := postSay(t, mux, sayAgentBody(map[string]string{"channel": sayChannel, "thread_ts": sayRootTS, "text": "  on it\n"})); rr.Code != http.StatusOK {
		t.Fatalf("say reply: %d %s", rr.Code, rr.Body)
	}
	body, ct = multipartBody(t, sayAgentBody(map[string]string{
		"channel": sayChannel, "thread_ts": sayRootTS, "text": comment, "title": "Login page", "alt_txt": "the login form",
	}), "shot.png", file)
	req = httptest.NewRequest(http.MethodPost, "/api/conversation/say", body)
	req.Header.Set("Content-Type", ct)
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("say upload: %d %s", rr.Code, rr.Body)
	}

	sameCalls(t, "legacy reply+upload vs say", legacyTwin.Calls(), sayTwin.Calls())

	got, err := json.MarshalIndent(legacyTwin.Calls(), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	path := filepath.Join("testdata", "agent_say_calls.golden.json")
	if *updateAgentSayGolden {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("%s missing (rerun with -update-agent-say)", path)
	}
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("agent reply/upload Slack calls drifted from %s (rerun with -update-agent-say only if intended):\n%s", path, got)
	}
}

// A say into a Slack thread is a message bead first: in the channel's land,
// in the thread (head found by its slack-thread label), in the channel bead
// bound to the Slack channel, with the Slack ref and lineage stamped.
func TestAgentSayWritesMessageBead(t *testing.T) {
	twin := surfacetest.NewSlackTwin()
	a, daemon := newSayAPI(t, twin)

	rr, resp := postSay(t, a.mux(), sayAgentBody(map[string]string{"channel": sayChannel, "thread_ts": sayRootTS, "text": "rerunning the flaky test"}))
	if rr.Code != http.StatusOK || !resp.OK {
		t.Fatalf("say: %d %s", rr.Code, rr.Body)
	}
	msgs := sayMessageBeads(daemon)
	if len(msgs) != 1 {
		t.Fatalf("message beads = %d, want 1", len(msgs))
	}
	m := msgs[0]
	posted := twin.Messages(sayChannel)
	if len(posted) != 1 || posted[0][1] != "rerunning the flaky test" {
		t.Fatalf("Slack messages = %v, want the reply", posted)
	}
	want := map[string]string{
		"thread_id": sayHeadID, "channel_id": sayChanBead, "author": "agent:crew-say", "origin": "cli",
		"kind": "text", "text": "rerunning the flaky test", "slack_channel": sayChannel, "slack_ts": posted[0][0],
	}
	for k, v := range want {
		if m.Fields[k] != v {
			t.Errorf("bead field %s = %q, want %q (fields %v)", k, m.Fields[k], v, m.Fields)
		}
	}
	if !slices.Equal(m.Labels, []string{"project:gardenio"}) {
		t.Errorf("bead labels = %v, want exactly the land", m.Labels)
	}
	if resp.ID != m.ID || resp.Thread != sayHeadID || resp.Timestamp != posted[0][0] || resp.Channel != sayChannel {
		t.Errorf("response = %+v, want the bead, head and Slack ref", resp)
	}
	var lineage []string
	for _, d := range daemon.getDeps() {
		if d.BeadID == m.ID && d.DepType == "triggered-by" {
			lineage = append(lineage, d.DependsOnID)
		}
	}
	if !slices.Equal(lineage, []string{"gd-agent1", "gd-trig1"}) {
		t.Errorf("triggered-by lineage = %v, want the agent and trigger beads", lineage)
	}
}

// The legacy reply route and the say route share one deduper: the same reply
// through both posts once.
func TestAgentSayDedupSharedWithLegacyReply(t *testing.T) {
	twin := surfacetest.NewSlackTwin()
	a, daemon := newSayAPI(t, twin)
	legacy := http.NewServeMux()
	a.threads.RegisterRoutes(legacy)

	raw, _ := json.Marshal(map[string]string{"channel": sayChannel, "thread_ts": sayRootTS, "text": "interrupted; resuming"})
	rr := httptest.NewRecorder()
	legacy.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/slack/threads/reply", bytes.NewReader(raw)))
	if rr.Code != http.StatusOK {
		t.Fatalf("legacy reply: %d", rr.Code)
	}
	_, resp := postSay(t, a.mux(), sayAgentBody(map[string]string{"channel": sayChannel, "thread_ts": sayRootTS, "text": "interrupted; resuming"}))
	if !resp.Deduped || resp.Timestamp == "" {
		t.Fatalf("say = %+v, want deduped onto the legacy post", resp)
	}
	if n := len(twin.Messages(sayChannel)); n != 1 {
		t.Fatalf("Slack messages = %d, want 1", n)
	}
	if n := len(sayMessageBeads(daemon)); n != 0 {
		t.Fatalf("message beads = %d, want none for a collapsed say", n)
	}
}

// gb say --thread names the head; a message id in the thread names it too.
func TestAgentSayByThreadHead(t *testing.T) {
	twin := surfacetest.NewSlackTwin()
	a, daemon := newSayAPI(t, twin)
	daemon.beads["gd-msg1"] = &beadsapi.BeadDetail{ID: "gd-msg1", Type: "message", Status: "closed",
		Labels: []string{"project:gardenio"}, Fields: map[string]string{"thread_id": sayHeadID}}

	for i, target := range []string{sayHeadID, "gd-msg1"} {
		rr, resp := postSay(t, a.mux(), sayAgentBody(map[string]string{"thread": target, "text": "reply " + target}))
		if rr.Code != http.StatusOK || resp.Thread != sayHeadID {
			t.Fatalf("say --thread %s: %d %s", target, rr.Code, rr.Body)
		}
		calls := twin.Calls()
		last := calls[len(calls)-1]
		if last.Method != "chat.postMessage" || last.Params["thread_ts"] != sayRootTS || last.Params["channel"] != sayChannel {
			t.Fatalf("say %d posted %+v, want a reply under the head's Slack root", i, last)
		}
	}
}

// Section 6.2 rule 4: an agent speaks only in its own land when it names the
// target by bead. Slack coordinates (its own thread) are exempt. The land is
// the bridge's record of the agent: an agent of "other" that reports
// land=gardenio is refused on gardenio's channel.
func TestAgentSayLandWall(t *testing.T) {
	twin := surfacetest.NewSlackTwin()
	a, _ := newSayAPI(t, twin)
	a.bot.agentProject["crew-say"] = "other"
	body := sayAgentBody(map[string]string{"land": "gardenio", "text": "hi"})

	body["thread"] = sayHeadID
	if rr, _ := postSay(t, a.mux(), body); rr.Code != http.StatusForbidden || !strings.Contains(rr.Body.String(), `"error"`) {
		t.Fatalf("thread in another land: %d %s, want a JSON 403", rr.Code, rr.Body)
	}
	delete(body, "thread")
	for _, ch := range []string{sayChanBead, sayChannel} {
		body["channel"] = ch
		if rr, _ := postSay(t, a.mux(), body); rr.Code != http.StatusForbidden {
			t.Fatalf("channel %s in another land: %d %s, want 403", ch, rr.Code, rr.Body)
		}
	}
	if n := len(twin.Calls()); n != 0 {
		t.Fatalf("Slack calls = %d, want none for a refused say", n)
	}
	body["channel"], body["thread_ts"] = sayChannel, sayRootTS
	if rr, _ := postSay(t, a.mux(), body); rr.Code != http.StatusOK {
		t.Fatalf("own thread by Slack coordinates: %d %s, want 200", rr.Code, rr.Body)
	}
}

// The wall fails closed: an agent neither the bridge nor the daemon can place
// names no target by bead, whatever land it reports, and reaches by Slack
// coordinates only a thread it is bound to.
func TestAgentSayLandWallUnknownLand(t *testing.T) {
	twin := surfacetest.NewSlackTwin()
	a, daemon := newSayAPI(t, twin)
	delete(a.bot.agentProject, "crew-say")
	body := sayAgentBody(map[string]string{"land": "gardenio", "text": "hi"})
	byCoords := sayAgentBody(map[string]string{"land": "gardenio", "text": "hi", "channel": sayChannel, "thread_ts": sayRootTS})

	for name, extra := range map[string]map[string]string{
		"thread":        {"thread": sayHeadID},
		"channel bead":  {"channel": sayChanBead},
		"channel name":  {"channel": "#crew"},
		"Slack channel": {"channel": sayChannel},
	} {
		if rr, _ := postSay(t, a.mux(), mergeBody(body, extra)); rr.Code != http.StatusForbidden {
			t.Errorf("unknown land, %s: %d %s, want 403", name, rr.Code, rr.Body)
		}
	}
	if rr, _ := postSay(t, a.mux(), byCoords); rr.Code != http.StatusForbidden {
		t.Errorf("unknown land, unbound Slack thread: %d %s, want 403", rr.Code, rr.Body)
	}
	if n := len(twin.Calls()); n != 0 {
		t.Fatalf("Slack calls = %d, want none for a refused say", n)
	}

	// An agent bead with no land, bound to the thread: its own thread only.
	daemon.beads["crew-say"] = &beadsapi.BeadDetail{ID: "gd-agent1", Type: "agent", Status: "open",
		Fields: map[string]string{"agent": "crew-say", "slack_thread_channel": sayChannel, "slack_thread_ts": sayRootTS}}
	if rr, _ := postSay(t, a.mux(), byCoords); rr.Code != http.StatusOK {
		t.Fatalf("own bound thread by Slack coordinates: %d %s, want 200", rr.Code, rr.Body)
	}
	if rr, _ := postSay(t, a.mux(), mergeBody(body, map[string]string{"thread": sayHeadID})); rr.Code != http.StatusForbidden {
		t.Errorf("own thread by bead, unknown land: %d %s, want 403", rr.Code, rr.Body)
	}

	// The bridge's thread binding counts too.
	delete(daemon.beads, "crew-say")
	sm, err := NewStateManager(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	a.bot.state = sm
	if err := sm.SetThreadAgent(sayChannel, sayRootTS, "gardenio/crew/crew-say"); err != nil {
		t.Fatal(err)
	}
	if rr, _ := postSay(t, a.mux(), byCoords); rr.Code != http.StatusOK {
		t.Fatalf("thread bound in bridge state: %d %s, want 200", rr.Code, rr.Body)
	}
}

// The daemon's record places an agent the bridge has not cached: its agent
// bead's land, not the one it reports.
func TestAgentSayLandFromAgentBead(t *testing.T) {
	twin := surfacetest.NewSlackTwin()
	a, daemon := newSayAPI(t, twin)
	delete(a.bot.agentProject, "crew-say")
	daemon.beads["crew-say"] = &beadsapi.BeadDetail{ID: "gd-agent1", Type: "agent", Status: "open",
		Labels: []string{"project:gardenio"}, Fields: map[string]string{"agent": "crew-say"}}

	if rr, _ := postSay(t, a.mux(), sayAgentBody(map[string]string{"land": "other", "thread": sayHeadID, "text": "hi"})); rr.Code != http.StatusOK {
		t.Fatalf("agent bead in gardenio, reported other: %d %s, want 200", rr.Code, rr.Body)
	}
	daemon.beads["crew-say"].Labels = []string{"project:other"}
	if rr, _ := postSay(t, a.mux(), sayAgentBody(map[string]string{"land": "gardenio", "thread": sayHeadID, "text": "again"})); rr.Code != http.StatusForbidden {
		t.Fatalf("agent bead in other, reported gardenio: %d %s, want 403", rr.Code, rr.Body)
	}
}

func mergeBody(body, extra map[string]string) map[string]string {
	out := maps.Clone(body)
	maps.Copy(out, extra)
	return out
}

// gb say --channel posts top-level in a channel of the agent's land, named by
// name, channel bead id or bound Slack id.
func TestAgentSayChannelPost(t *testing.T) {
	twin := surfacetest.NewSlackTwin()
	a, daemon := newSayAPI(t, twin)

	for _, name := range []string{"#crew", "crew", sayChanBead, sayChannel} {
		rr, resp := postSay(t, a.mux(), sayAgentBody(map[string]string{"channel": name, "text": "release cut via " + name}))
		if rr.Code != http.StatusOK {
			t.Fatalf("say --channel %s: %d %s", name, rr.Code, rr.Body)
		}
		if resp.Thread != "" {
			t.Errorf("say --channel %s: thread %q, want a top-level post", name, resp.Thread)
		}
		calls := twin.Calls()
		last := calls[len(calls)-1]
		if last.Params["channel"] != sayChannel || last.Params["thread_ts"] != "" {
			t.Fatalf("say --channel %s posted %+v, want top-level in %s", name, last, sayChannel)
		}
	}
	for _, m := range sayMessageBeads(daemon) {
		if m.Fields["channel_id"] != sayChanBead || m.Fields["thread_id"] != "" {
			t.Errorf("channel post bead fields = %v, want channel_id and no thread", m.Fields)
		}
	}

	if rr, _ := postSay(t, a.mux(), sayAgentBody(map[string]string{"channel": "nope", "text": "x"})); rr.Code != http.StatusNotFound {
		t.Errorf("unknown channel: %d, want 404", rr.Code)
	}
	daemon.beads[sayChanBead].Status = "closed"
	if rr, _ := postSay(t, a.mux(), sayAgentBody(map[string]string{"channel": sayChanBead, "text": "x"})); rr.Code != http.StatusUnprocessableEntity {
		t.Errorf("archived channel: %d, want 422", rr.Code)
	}
}

// A thread started in the Commons has no Slack root: the say is its bead
// alone, and the Commons renders it.
func TestAgentSayCommonsOnlyThread(t *testing.T) {
	twin := surfacetest.NewSlackTwin()
	a, daemon := newSayAPI(t, twin)
	daemon.beads["gd-chat1"] = &beadsapi.BeadDetail{ID: "gd-chat1", Type: "message", Status: "closed",
		Labels: []string{"project:gardenio"}, Fields: map[string]string{"channel_id": sayChanBead, "origin": "commons", "author": "oidc:ada@example.com", "text": "anyone around?"}}

	rr, resp := postSay(t, a.mux(), sayAgentBody(map[string]string{"thread": "gd-chat1", "text": "here"}))
	if rr.Code != http.StatusOK || resp.ID == "" || resp.Timestamp != "" {
		t.Fatalf("say: %d %s, want a bead and no Slack copy", rr.Code, rr.Body)
	}
	if n := len(twin.Calls()); n != 0 {
		t.Fatalf("Slack calls = %d, want none", n)
	}
	if f := daemon.beads[resp.ID].Fields; f["thread_id"] != "gd-chat1" || f["channel_id"] != sayChanBead {
		t.Fatalf("bead fields = %v", f)
	}
}

func TestAgentSayRefusals(t *testing.T) {
	a, _ := newSayAPI(t, surfacetest.NewSlackTwin())
	for name, c := range map[string]struct {
		body map[string]string
		want int
	}{
		"no agent":  {map[string]string{"thread": sayHeadID, "text": "x"}, http.StatusBadRequest},
		"no target": {sayAgentBody(map[string]string{"text": "x"}), http.StatusBadRequest},
		"no text":   {sayAgentBody(map[string]string{"thread": sayHeadID, "text": "  "}), http.StatusBadRequest},
	} {
		if rr, _ := postSay(t, a.mux(), c.body); rr.Code != c.want {
			t.Errorf("%s: %d %s, want %d", name, rr.Code, rr.Body, c.want)
		}
	}
}

// The thread read routes, as crew-say reads them: the conversation route and
// the legacy alias, which takes Slack coordinates only.
const (
	readConv   = "/api/conversation/thread?agent=crew-say&"
	readLegacy = "/api/slack/threads?agent=crew-say&"
)

// get GETs target.
func get(h http.Handler, target string) *httptest.ResponseRecorder {
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, target, nil))
	return rr
}

// readThread GETs a thread as crew-say.
func readThread(h http.Handler, query string) *httptest.ResponseRecorder {
	return get(h, readConv+query)
}

func getThread(t *testing.T, h http.Handler, query string) threadResponse {
	t.Helper()
	rr := readThread(h, query)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET thread?%s: %d %s", query, rr.Code, rr.Body)
	}
	var resp threadResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	return resp
}

// gb thread reads a Slack thread from Slack, and its beads on request; a
// Commons-only thread from its beads.
func TestAgentThreadRead(t *testing.T) {
	twin := surfacetest.NewSlackTwin()
	a, daemon := newSayAPI(t, twin)
	mux := a.mux()
	if rr, _ := postSay(t, mux, sayAgentBody(map[string]string{"thread": sayHeadID, "text": "a flaky test; rerun"})); rr.Code != http.StatusOK {
		t.Fatalf("say: %d", rr.Code)
	}

	slackRead := getThread(t, mux, "channel="+sayChannel+"&ts="+sayRootTS)
	if slackRead.Source != threadSourceSlack || slackRead.Thread != sayHeadID {
		t.Fatalf("Slack thread read = %+v, want source slack and the head", slackRead)
	}
	if n := len(slackRead.Messages); n != 1 || slackRead.Messages[0].Text != "a flaky test; rerun" {
		t.Fatalf("Slack thread messages = %+v", slackRead.Messages)
	}

	daemon.beads["gd-sq1"] = &beadsapi.BeadDetail{ID: "gd-sq1", Type: "message", Status: "closed", Title: "progress",
		Labels: []string{"squawk", "project:gardenio"}, Fields: map[string]string{"thread_id": sayHeadID, "source_agent": "crew-say"}}
	beadsRead := getThread(t, mux, "thread="+sayHeadID+"&source=beads")
	if beadsRead.Source != threadSourceBeads || len(beadsRead.Messages) != 3 {
		t.Fatalf("beads read = %+v, want the head and two messages", beadsRead)
	}
	root := beadsRead.Messages[0]
	if root.Text != "why is CI red?" || root.Author != "ada@example.com" || root.IsBot {
		t.Errorf("root = %+v, want the opener's words", root)
	}
	byText := map[string]ThreadMessage{}
	for _, m := range beadsRead.Messages[1:] {
		byText[m.Text] = m
	}
	if m := byText["a flaky test; rerun"]; m.Author != "crew-say" || !m.IsBot {
		t.Errorf("say message = %+v, want the agent", m)
	}
	if m := byText["progress"]; m.Subtype != "squawk" {
		t.Errorf("squawk message = %+v, want subtype squawk", m)
	}

	daemon.beads["gd-chat1"] = &beadsapi.BeadDetail{ID: "gd-chat1", Type: "message", Status: "closed", Title: "anyone around?",
		Labels: []string{"project:gardenio"}, Fields: map[string]string{"channel_id": sayChanBead, "author": "oidc:ada@example.com"}}
	if got := getThread(t, mux, "thread=gd-chat1"); got.Source != threadSourceBeads || len(got.Messages) != 1 {
		t.Errorf("Commons-only thread read = %+v, want its beads", got)
	}
}

// legacyMux serves the agent API and the legacy thread API it walls, as the
// bridge registers them.
func (a *ConversationAgentAPI) legacyMux() *http.ServeMux {
	mux := a.mux()
	a.threads.RegisterRoutes(mux)
	return mux
}

// Reads are land-walled too (gd-8uSFC2v7l4): an agent of another land reads
// gardenio's thread by none of its names, Slack coordinates included, nor a
// thread in a Slack channel no land claims, and neither Slack nor the beads
// are read for it. The legacy GET /api/slack/threads reads through the same
// wall (gd-3uBuj2h7Ar). Its own thread it reads wherever it is.
func TestAgentThreadReadLandWall(t *testing.T) {
	twin := surfacetest.NewSlackTwin()
	a, daemon := newSayAPI(t, twin)
	mux := a.legacyMux()
	a.bot.agentProject["crew-say"] = "other"
	daemon.beads["gd-msg1"] = &beadsapi.BeadDetail{ID: "gd-msg1", Type: "message", Status: "closed",
		Labels: []string{"project:gardenio"}, Fields: map[string]string{"thread_id": sayHeadID}}
	coords := "channel=" + sayChannel + "&ts=" + sayRootTS

	for name, target := range map[string]string{
		"head id":                         readConv + "thread=" + sayHeadID,
		"head id, beads":                  readConv + "thread=" + sayHeadID + "&source=beads",
		"message id":                      readConv + "thread=gd-msg1",
		"channel bead":                    readConv + "channel=" + sayChanBead,
		"Slack channel":                   readConv + "channel=" + sayChannel,
		"Slack thread":                    readConv + coords,
		"Slack thread, beads":             readConv + coords + "&source=beads",
		"unclaimed Slack channel":         readConv + "channel=C0NOLAND&ts=" + sayRootTS,
		"legacy Slack thread":             readLegacy + coords,
		"legacy unclaimed Slack channel":  readLegacy + "channel=C0NOLAND&ts=" + sayRootTS,
		"legacy Slack thread, full agent": "/api/slack/threads?agent=gardenio/crew/crew-say&" + coords,
	} {
		if rr := get(mux, target); rr.Code != http.StatusForbidden || !strings.Contains(rr.Body.String(), `"error"`) {
			t.Errorf("%s in another land: %d %s, want a JSON 403", name, rr.Code, rr.Body)
		}
	}
	if n := len(twin.Calls()); n != 0 {
		t.Fatalf("Slack calls = %d, want none for a refused read", n)
	}

	// No reader, no read.
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/conversation/thread?thread="+sayHeadID, nil))
	if rr.Code != http.StatusBadRequest {
		t.Errorf("no agent: %d %s, want 400", rr.Code, rr.Body)
	}

	// Its own thread in another land's channel: bound on its agent bead...
	daemon.beads["crew-say"] = &beadsapi.BeadDetail{ID: "gd-agent1", Type: "agent", Status: "open",
		Labels: []string{"project:other"}, Fields: map[string]string{"agent": "crew-say", "slack_thread_channel": sayChannel, "slack_thread_ts": sayRootTS}}
	if got := getThread(t, mux, coords); got.Thread != sayHeadID {
		t.Fatalf("own thread bound on the agent bead = %+v, want the head", got)
	}
	if rr := get(mux, readLegacy+coords); rr.Code != http.StatusOK {
		t.Fatalf("legacy, own thread bound on the agent bead: %d %s, want 200", rr.Code, rr.Body)
	}
	// ...or in the bridge's state.
	delete(daemon.beads, "crew-say")
	sm, err := NewStateManager(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	a.bot.state = sm
	if err := sm.SetThreadAgent(sayChannel, sayRootTS, "gardenio/crew/crew-say"); err != nil {
		t.Fatal(err)
	}
	if got := getThread(t, mux, coords+"&source=beads"); got.Source != threadSourceBeads {
		t.Fatalf("own thread bound in bridge state = %+v, want its beads", got)
	}
	// A target named by bead must still be in its land.
	if rr := readThread(mux, "thread="+sayHeadID); rr.Code != http.StatusForbidden {
		t.Errorf("own thread by head id in another land: %d %s, want 403", rr.Code, rr.Body)
	}

	// An agent of gardenio reads it by every name.
	a.bot.agentProject["crew-say"] = "gardenio"
	for _, query := range []string{"thread=" + sayHeadID, "thread=gd-msg1", coords} {
		if got := getThread(t, mux, query); got.Thread != sayHeadID {
			t.Errorf("own land, %s = %+v, want the head", query, got)
		}
	}
	if rr := get(mux, readLegacy+coords); rr.Code != http.StatusOK {
		t.Errorf("legacy, own land: %d %s, want 200", rr.Code, rr.Body)
	}
}

// A legacy read that names no agent (a gb that predates ?agent=) is placed by
// its target alone and reaches only an operator land's thread: a tenant
// land's thread, one in a Slack channel no land claims, and one in a land the
// bridge cannot look up are refused before Slack is read (gd-3uBuj2h7Ar).
func TestLegacyThreadReadNoAgent(t *testing.T) {
	twin := surfacetest.NewSlackTwin()
	a, daemon := newSayAPI(t, twin)
	mux := a.legacyMux()
	project := &beadsapi.BeadDetail{ID: "gd-proj1", Type: "project", Status: "open", Title: "gardenio", Fields: map[string]string{}}
	daemon.beads[project.ID] = project
	coords := "/api/slack/threads?channel=" + sayChannel + "&ts=" + sayRootTS

	for _, tc := range []struct {
		name, target string
		setup        func()
	}{
		{"tenant land", coords, func() { project.Fields["land_kind"] = "tenant" }},
		{"unknown land kind", coords, func() { project.Fields["land_kind"] = "orchard" }},
		{"land with no project", coords, func() { delete(daemon.beads, project.ID) }},
		{"unclaimed Slack channel", "/api/slack/threads?channel=C0NOLAND&ts=" + sayRootTS, func() {}},
	} {
		tc.setup()
		if rr := get(mux, tc.target); rr.Code != http.StatusForbidden || !strings.Contains(rr.Body.String(), `"error"`) {
			t.Errorf("no agent, %s: %d %s, want a JSON 403", tc.name, rr.Code, rr.Body)
		}
		project.Fields = map[string]string{}
		daemon.beads[project.ID] = project
	}
	if n := len(twin.Calls()); n != 0 {
		t.Fatalf("Slack calls = %d, want none for a refused read", n)
	}

	// An operator land's thread reads as it always has: land_kind unset or
	// "operator".
	for _, kind := range []string{"", beadsapi.LandKindOperator} {
		project.Fields["land_kind"] = kind
		rr := get(mux, coords)
		var msgs []ThreadMessage
		if rr.Code != http.StatusOK || json.Unmarshal(rr.Body.Bytes(), &msgs) != nil {
			t.Errorf("no agent, operator land (land_kind %q): %d %s, want the thread", kind, rr.Code, rr.Body)
		}
	}
}

// A legacy thread API that no conversation API walls (a bridge with no bot)
// reads no thread.
func TestLegacyThreadReadUnwalled(t *testing.T) {
	twin := surfacetest.NewSlackTwin()
	legacy := NewSlackThreadAPI(slack.New("xoxb-test", slack.OptionAPIURL(twin.Serve(t))), slog.Default())
	mux := http.NewServeMux()
	legacy.RegisterRoutes(mux)
	if rr := get(mux, "/api/slack/threads?agent=crew-say&channel="+sayChannel+"&ts="+sayRootTS); rr.Code != http.StatusServiceUnavailable {
		t.Errorf("unwalled read: %d %s, want 503", rr.Code, rr.Body)
	}
	if n := len(twin.Calls()); n != 0 {
		t.Fatalf("Slack calls = %d, want none", n)
	}
}

// The read wall fails closed as the say wall does: a reader neither the
// bridge nor the daemon can place reads only the thread it is bound to.
func TestAgentThreadReadLandWallUnknownLand(t *testing.T) {
	twin := surfacetest.NewSlackTwin()
	a, daemon := newSayAPI(t, twin)
	mux := a.legacyMux()
	delete(a.bot.agentProject, "crew-say")
	coords := "channel=" + sayChannel + "&ts=" + sayRootTS

	for _, target := range []string{readConv + "thread=" + sayHeadID, readConv + "channel=" + sayChanBead, readConv + coords, readLegacy + coords} {
		if rr := get(mux, target); rr.Code != http.StatusForbidden {
			t.Errorf("unknown land, %s: %d %s, want 403", target, rr.Code, rr.Body)
		}
	}
	if n := len(twin.Calls()); n != 0 {
		t.Fatalf("Slack calls = %d, want none for a refused read", n)
	}

	daemon.beads["crew-say"] = &beadsapi.BeadDetail{ID: "gd-agent1", Type: "agent", Status: "open",
		Fields: map[string]string{"agent": "crew-say", "slack_thread_channel": sayChannel, "slack_thread_ts": sayRootTS}}
	if got := getThread(t, mux, coords); got.Thread != sayHeadID {
		t.Fatalf("own bound thread, unknown land = %+v, want the head", got)
	}
	if rr := readThread(mux, "thread="+sayHeadID); rr.Code != http.StatusForbidden {
		t.Errorf("own thread by head id, unknown land: %d %s, want 403", rr.Code, rr.Body)
	}
}
