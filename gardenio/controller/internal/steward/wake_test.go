package steward

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"image"
	"image/png"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/bridge"
	"gardenio/controller/internal/commons"
	"gardenio/controller/internal/commons/commonstest"
	"gardenio/controller/internal/scheduler"
)

var update = flag.Bool("update", false, "rewrite the goldens in testdata/")

// snapFile is a Commons file link to a farm snapshot in tomato-patch's
// #general, as the say route records it.
const snapFile = "https://lands.example/beads/garden/c/f/" + land + "/" + general + "/0123456789abcdef0123456789abcdef/farm-tomato-patch-tp-plan.png"

// snapFields are a snapshot message's files and snap, as the Commons writes
// them (internal/commons snap.go).
func snapFields() map[string]string {
	files, _ := json.Marshal([]map[string]any{{"name": "farm-tomato-patch-tp-plan.png", "mimetype": "image/png", "size": 48213, "url": snapFile}})
	snap, _ := json.Marshal(map[string]any{
		"kind": "farm", "url": "https://lands.example/beads/garden?at=" + land + "/tp-plan&z=2", "at": land + "/tp-plan", "z": 2,
		"who": "all", "night": true, "taken_at": "2026-10-23T09:30:00Z",
		"framed": []map[string]string{
			{"kind": "project", "id": "tp-proj", "title": land},
			{"kind": "plan", "id": "tp-plan", "title": "Add a watering schedule"},
			{"kind": "bead", "id": "tp-s1", "title": "Water the beds"},
		},
	})
	return map[string]string{"files": string(files), "snap": string(snap), "origin": "commons"}
}

// wakeFarm is the test farm with a #crew channel and the messages that wake
// the steward.
func wakeFarm(msgs ...*beadsapi.BeadDetail) getterFarm {
	d := newFarm()
	d.beads = append(d.beads, &beadsapi.BeadDetail{ID: crew, Type: "channel", Title: "#crew", Status: "open",
		Labels: []string{"project:" + land}, Fields: map[string]string{"name": "crew"}})
	d.beads = append(d.beads, msgs...)
	return getterFarm{d}
}

func wakeBead(id, channel, text string, fields map[string]string) *beadsapi.BeadDetail {
	f := map[string]string{"channel_id": channel, "author": "oidc:ada@example.com", "kind": "text"}
	if text != "" {
		f["text"] = text
	}
	for k, v := range fields {
		f[k] = v
	}
	return &beadsapi.BeadDetail{ID: id, Type: "message", Title: "Snapshot of the farm at " + land + "/tp-plan", Status: "closed",
		Labels: []string{"project:" + land}, CreatedBy: f["author"], Fields: f}
}

var stewardTrig = bridge.TriggerDef{ID: "tp-trig-1", Role: Role, Project: land, Labels: []string{MarkerLabel, LandLabel(land)}}

// wakeSection is the prompt's "## What woke you" section, "" when it has none.
func wakeSection(t *testing.T, prompt string) string {
	t.Helper()
	i := strings.Index(prompt, "## What woke you")
	if i < 0 {
		return ""
	}
	j := strings.Index(prompt[i:], "## The land now")
	if j < 0 {
		t.Fatalf("the wake section is not before the land snapshot:\n%s", prompt)
	}
	return prompt[i : i+j]
}

func checkGolden(t *testing.T, name, got string) {
	t.Helper()
	p := filepath.Join("testdata", name)
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("%v (run go test -update to write it)", err)
	}
	if got != string(want) {
		t.Fatalf("%s drifted (go test -update rewrites it):\n--- got\n%s\n--- want\n%s", name, got, want)
	}
}

// A snapshot sent to #general with no words renders with the view link,
// where the camera was, what it framed, where gb commons file writes the
// picture, the default ask and the thread to answer in.
func TestRenderPrompt_WakeSnapGolden(t *testing.T) {
	farm := wakeFarm(wakeBead("tp-m1", general, "", snapFields()))
	got, err := TriggerPromptRenderer(farm)(context.Background(), stewardTrig,
		bridge.FireEvent{Entry: WakeGeneral, BeadID: "tp-m1", EventType: "created"})
	if err != nil {
		t.Fatal(err)
	}
	checkGolden(t, "wake_snap.golden", wakeSection(t, got))
	// It sits after the notes and #machines, before the land.
	if n, w := strings.Index(got, "## #machines since your last run"), strings.Index(got, "## What woke you"); n < 0 || w < n {
		t.Fatalf("the wake renders after #machines:\n%s", got)
	}
}

func TestRenderPrompt_WakeAskAndThread(t *testing.T) {
	reply := wakeBead("tp-m2", general, "is the north bed too dry?", snapFields())
	reply.Fields["thread_id"] = "tp-head"
	mention := wakeBead("tp-m3", crew, "@steward what is this?\nsecond line", snapFields())
	plain := wakeBead("tp-m4", general, "can we add a compost plan?", nil)
	long := wakeBead("tp-m5", general, strings.Repeat("é", wakeTextMax), nil)
	slackFile := wakeBead("tp-m6", general, "from slack", map[string]string{
		"files": `[{"name":"shot.png","mimetype":"image/png","url":"https://acme.slack.com/files/U1/F1/shot.png"}]`})
	farm := wakeFarm(reply, mention, plain, long, slackFile)
	render := func(entry, id string) string {
		t.Helper()
		got, err := TriggerPromptRenderer(farm)(context.Background(), stewardTrig, bridge.FireEvent{Entry: entry, BeadID: id, EventType: "created"})
		if err != nil {
			t.Fatal(err)
		}
		return wakeSection(t, got)
	}

	got := render(WakeGeneral, "tp-m2")
	for _, want := range []string{"> is the north bed too dry?", "gb say --thread tp-head \"<answer>\"", "gb commons file tp-m2"} {
		if !strings.Contains(got, want) {
			t.Errorf("a snap reply with words: missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, DefaultSnapAsk) {
		t.Errorf("their words are the ask, not the default:\n%s", got)
	}

	got = render(WakeMention, "tp-m3")
	for _, want := range []string{"An @steward mention in #crew (tp-m3)", "> @steward what is this?\n> second line", "gb say --thread tp-m3 "} {
		if !strings.Contains(got, want) {
			t.Errorf("a mention: missing %q:\n%s", want, got)
		}
	}

	got = render(WakeGeneral, "tp-m4")
	if !strings.Contains(got, "> can we add a compost plan?") || strings.Contains(got, "gb commons file") ||
		strings.Contains(got, "snapshot") || !strings.Contains(got, "so the message,\nthe ask") {
		t.Errorf("a plain message has no files to fetch:\n%s", got)
	}

	got = render(WakeGeneral, "tp-m5")
	if !strings.Contains(got, "(cut here; read the whole message with gb thread tp-m5)") || len(got) > wakeTextMax+2<<10 {
		t.Errorf("a long message is cut (%d bytes):\n%.300s", len(got), got)
	}

	got = render(WakeGeneral, "tp-m6")
	if !strings.Contains(got, "- shot.png (image/png)") || strings.Contains(got, "gb commons file") {
		t.Errorf("a Slack permalink is listed, not fetched:\n%s", got)
	}
}

// Only a #general or mention wake renders a message, and only one of the
// steward's own land.
func TestRenderPrompt_WakeRendersOnlyTheLandsMessage(t *testing.T) {
	other := wakeBead("bf-m1", "bf-general", "", snapFields())
	other.Labels = []string{"project:bean-field"}
	notMsg := &beadsapi.BeadDetail{ID: "tp-task", Type: "task", Title: "x", Labels: []string{"project:" + land}}
	farm := wakeFarm(wakeBead("tp-m1", general, "", snapFields()), other, notMsg)
	for name, ev := range map[string]bridge.FireEvent{
		"another land's snap":       {Entry: WakeGeneral, BeadID: "bf-m1"},
		"a bead that is no message": {Entry: WakeGeneral, BeadID: "tp-task"},
		"a missing bead":            {Entry: WakeGeneral, BeadID: "tp-gone"},
		"another wake":              {Entry: WakeMachines, BeadID: "tp-m1"},
		"the digest":                {Entry: WakeDigest},
		"no event":                  {},
	} {
		got, err := TriggerPromptRenderer(farm)(context.Background(), stewardTrig, ev)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if s := wakeSection(t, got); s != "" {
			t.Errorf("%s rendered a wake:\n%s", name, s)
		}
	}
	// A source with no single-bead read renders the land without the wake.
	got, err := TriggerPromptRenderer(farm.fakeDaemon)(context.Background(), stewardTrig, bridge.FireEvent{Entry: WakeGeneral, BeadID: "tp-m1"})
	if err != nil || wakeSection(t, got) != "" {
		t.Fatalf("no getter: err %v, wake %q", err, wakeSection(t, got))
	}
}

// The path end to end on the real pieces: ada snaps the farm with a question
// into tomato-patch's #general through the Commons say route; that bead's
// created event wakes the steward once (and bean-field's steward not at
// all); the fire renders the playbook with the message, its picture where gb
// commons file writes it, and the thread; and the steward's answer, posted
// where the playbook says, is in that thread.
func TestSnapToStewardEndToEnd(t *testing.T) {
	t0 := time.Date(2026, 10, 23, 9, 0, 0, 0, time.UTC)
	d := &commonstest.Daemon{}
	d.Members = map[string][]beadsapi.LandMember{land: {{Principal: "oidc:ada@example.com", Name: "ada"}}}
	for _, b := range newFarm().beads {
		cp := *b
		d.Put(&cp)
	}
	store := &memStore{objs: map[string][]byte{}}
	h := &commons.Handler{
		Beads: d, Writes: d, Files: store, BasePath: "/beads",
		Scope: func(*http.Request) commons.Scope { return commons.Scope{Lands: []string{land}} },
		Now:   func() time.Time { return t0 },
		Gate: commons.Gate{
			Self: func(*http.Request) string { return "oidc:ada@example.com" },
			May:  func(*http.Request, commons.Channel, string) bool { return true },
		},
	}
	mux := http.NewServeMux()
	h.Register(mux)

	// 1. The Commons: a snapshot with a prompt, as the farm's Snap button posts it.
	var pic bytes.Buffer
	_ = png.Encode(&pic, image.NewRGBA(image.Rect(0, 0, 2, 2)))
	snapCtx := `{"kind":"farm","url":"/beads/garden?at=tomato-patch/tp-plan&z=2","at":"tomato-patch/tp-plan","z":2,"who":"all",` +
		`"framed":[{"kind":"plan","id":"tp-plan","title":"Add a watering schedule"}]}`
	before := len(d.Created)
	if code, body := multipartPost(t, mux, "/garden/c/"+land+"/general/say",
		formPart{name: "text", body: []byte("is the north bed too dry?")},
		formPart{name: "file", file: "farm.png", ct: "image/png", body: pic.Bytes()},
		formPart{name: "context", body: []byte(snapCtx)}); code != http.StatusCreated {
		t.Fatalf("say = %d %s", code, body)
	}
	if len(d.Created) != before+1 {
		t.Fatalf("the say wrote %d beads, want 1", len(d.Created)-before)
	}
	var msg *beadsapi.BeadDetail
	for _, b := range d.Beads {
		if b.Type == "message" && b.Fields["channel_id"] == general {
			msg = b
		}
	}
	if msg == nil || msg.Fields["snap"] == "" || msg.Fields["files"] == "" {
		t.Fatalf("no snap message in #general: %+v", msg)
	}

	// 2. The wake: the created event fires the steward's #general entry once;
	// bean-field's steward never sees it.
	ev := bridge.BeadEvent{ID: msg.ID, Type: msg.Type, Status: msg.Status, CreatedBy: msg.CreatedBy, Labels: msg.Labels, Fields: msg.Fields}
	if got := fired(t, "beads.bead.created", &ev); len(got) != 1 || got[0] != WakeGeneral {
		t.Fatalf("the snap fired %q, want one %s", got, WakeGeneral)
	}
	fire := fireOf(t, land, chans, &ev)
	if fire.Entry != WakeGeneral || fire.BeadID != msg.ID {
		t.Fatalf("fire = %+v", fire)
	}
	if got := firedIn(t, "bean-field", Channels{General: "bf-general"}, &ev); len(got) != 0 {
		t.Fatalf("bean-field's steward woke on tomato-patch's snap: %q", got)
	}

	// 3. The prompt: the message, the picture's local path, the thread.
	prompt, err := TriggerPromptRenderer(d)(context.Background(), stewardTrig, fire)
	if err != nil {
		t.Fatal(err)
	}
	wake := wakeSection(t, prompt)
	for _, want := range []string{
		"A message in #general (" + msg.ID + ") from oidc:ada@example.com:\n> is the north bed too dry?",
		"It is a snapshot of the farm at tomato-patch/tp-plan, zoom level 2 of 3, showing everyone's work.",
		"The view: http://example.com/beads/garden?at=tomato-patch/tp-plan&z=2",
		"- plan tp-plan: Add a watering schedule",
		"gb commons file " + msg.ID,
		".../commons-files/" + msg.ID + "/farm.png",
	} {
		if !strings.Contains(wake, want) {
			t.Errorf("the wake is missing %q:\n%s", want, wake)
		}
	}
	// gb commons file writes the picture where the prompt says.
	var refs []struct{ URL string }
	_ = json.Unmarshal([]byte(msg.Fields["files"]), &refs)
	if _, _, machine, ok := bridge.CommonsFileRoute(refs[0].URL); !ok || filepath.Base(machine) != "farm.png" {
		t.Fatalf("the picture's link %q is not a Commons file of farm.png", refs[0].URL)
	}

	// 4. The answer: the steward runs the reply the playbook gives it, and it
	// lands in the snap's thread, not as a new post.
	m := regexp.MustCompile(`gb say --thread (\S+) "<answer>"`).FindStringSubmatch(wake)
	if m == nil {
		t.Fatalf("no in-thread reply command:\n%s", wake)
	}
	if code, body := multipartPost(t, mux, "/garden/c/t/"+m[1]+"/reply",
		formPart{name: "text", body: []byte("The north bed is dry at the far end; I would water it tonight.")}); code != http.StatusCreated {
		t.Fatalf("reply in thread %s = %d %s", m[1], code, body)
	}
	answer := d.Created[len(d.Created)-1]
	var af map[string]string
	_ = json.Unmarshal(answer.Fields, &af)
	if af["thread_id"] != msg.ID || af["channel_id"] != general {
		t.Fatalf("the answer went to thread %q channel %q, want the snap's thread %s in #general", af["thread_id"], af["channel_id"], msg.ID)
	}
}

// fireOf is the FireEvent a steward's wake entries hand the renderer for one
// event: the entry that fired and the bead, as the engine's multi match
// records them (bridge TestFirePrompt_HandsTheRendererTheWake).
func fireOf(t *testing.T, name string, ch Channels, b *bridge.BeadEvent) bridge.FireEvent {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	src := bridge.NewMultiTriggerSource(logger, nil)
	src.InjectBeadEvent("beads.bead.created", b)
	entries := WakeTriggers(name, ch, DigestCron(name, DefaultDigestHour), "UTC")
	tctx := scheduler.TriggerEvalContext{TriggerID: "steward", Project: name, Triggers: entries}
	decs := scheduler.EvaluateTriggersAll(context.Background(), src.Engine(), nil, logger, tctx, evalAt)
	if len(decs) != 1 || decs[0].Event == nil {
		t.Fatalf("want one bead-event fire, got %+v", decs)
	}
	return bridge.FireEvent{Entry: entries[decs[0].EntryIndex].Label, BeadID: decs[0].Event.BeadID, EventType: decs[0].Event.EventType}
}

// firedIn is fired for another land's steward.
func firedIn(t *testing.T, name string, ch Channels, b *bridge.BeadEvent) []string {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	src := bridge.NewMultiTriggerSource(logger, nil)
	src.InjectBeadEvent("beads.bead.created", b)
	tctx := scheduler.TriggerEvalContext{TriggerID: "steward", Project: name, Triggers: WakeTriggers(name, ch, DigestCron(name, DefaultDigestHour), "UTC")}
	var out []string
	for _, d := range scheduler.EvaluateTriggersAll(context.Background(), src.Engine(), nil, logger, tctx, evalAt) {
		out = append(out, tctx.Triggers[d.EntryIndex].Label)
	}
	return out
}

type formPart struct {
	name, file, ct string
	body           []byte
}

func multipartPost(t *testing.T, h http.Handler, path string, parts ...formPart) (int, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for _, p := range parts {
		hd := textproto.MIMEHeader{}
		cd := `form-data; name="` + p.name + `"`
		if p.file != "" {
			cd += `; filename="` + p.file + `"`
		}
		hd.Set("Content-Disposition", cd)
		if p.ct != "" {
			hd.Set("Content-Type", p.ct)
		}
		w, err := mw.CreatePart(hd)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write(p.body)
	}
	_ = mw.Close()
	r := httptest.NewRequest("POST", path, &buf)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec.Code, rec.Body.String()
}

// memStore is a FileStore in memory.
type memStore struct {
	mu   sync.Mutex
	objs map[string][]byte
}

func (m *memStore) PutFile(_ context.Context, key, _ string, body []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objs[key] = body
	return nil
}

func (m *memStore) GetFile(_ context.Context, key string) (commons.StoredFile, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.objs[key]
	if !ok {
		return commons.StoredFile{}, commons.ErrNoFile
	}
	return commons.StoredFile{Body: io.NopCloser(bytes.NewReader(b)), ContentType: "image/png"}, nil
}

func (m *memStore) DeleteFile(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.objs, key)
	return nil
}

// A person-joined wake (step 9.14) renders the provisioner's notice under
// "What woke you" and sends the run to duty 1's greeting; a post that is not
// the provisioner's person-joined notice in this land renders nothing.
func TestRenderPrompt_PersonJoined(t *testing.T) {
	joined := wakeBead("tp-j1", machines, JoinedText("Ada Lovelace", "guest"), map[string]string{
		"author": OpenedAuthor, "kind": "notice", "origin": OriginPersonJoined})
	forged := wakeBead("tp-j2", machines, "Mallory joined the land as a member.", map[string]string{"origin": OriginPersonJoined})
	opened := wakeBead("tp-j3", machines, OpenedText(land), map[string]string{"author": OpenedAuthor, "kind": "notice", "origin": OriginLandOpened})
	other := wakeBead("bf-j1", "bf-machines", JoinedText("Bob", "guest"), map[string]string{"author": OpenedAuthor, "origin": OriginPersonJoined})
	other.Labels = []string{"project:bean-field"}
	farm := wakeFarm(joined, forged, opened, other)
	render := func(id string) string {
		t.Helper()
		got, err := TriggerPromptRenderer(farm)(context.Background(), stewardTrig, bridge.FireEvent{Entry: WakePersonJoined, BeadID: id, EventType: "created"})
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	got := render("tp-j1")
	w := wakeSection(t, got)
	for _, want := range []string{"A person joined this land (tp-j1", "> Ada Lovelace joined the land as a guest.", "duty 1, the greeting for a person who joined"} {
		if !strings.Contains(w, want) {
			t.Errorf("missing %q in the wake:\n%s", want, w)
		}
	}
	if !strings.Contains(got, "- "+WakePersonJoined+": a person joined the land") || !strings.Contains(got, "When a person joined (the "+WakePersonJoined+" wake)") {
		t.Errorf("the playbook does not say what a person-joined wake does:\n%s", got)
	}
	for _, id := range []string{"tp-j2", "tp-j3", "bf-j1"} {
		if s := wakeSection(t, render(id)); s != "" {
			t.Errorf("%s rendered a wake:\n%s", id, s)
		}
	}
}
