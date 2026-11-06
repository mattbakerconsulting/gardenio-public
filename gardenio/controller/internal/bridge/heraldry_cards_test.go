package bridge

// heraldry_cards_test.go pins heraldry step 5.5a (gd-nLVyw2i9fN; spec
// gardenio/docs/superpowers/specs/2026-10-25-heraldry-design.md R9, Q5, Q9):
// Slack shows a bead's arms as the emoji run before its name, read through
// heraldry.Parse; a card whose subject bears arms puts the run and the
// subject's short title on its header line; and no card carries a picture.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/slack-go/slack"

	"gardenio/controller/internal/beadsapi"
)

func headerSection(text string) *slack.SectionBlock {
	return slack.NewSectionBlock(slack.NewTextBlockObject("mrkdwn", text, false, false), nil, nil)
}

// TestArmsRun_ReadsThroughParse: the run comes from heraldry.Parse, so the
// stored six-token form and the run form (step 5.3) both read as the run,
// and a value that is not arms reads as none, where a split of the string
// would print it.
func TestArmsRun_ReadsThroughParse(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{planArms, "💵👛🧾"},
		{epicArms, "🧭⚓"},
		{"💵👛🧾", "💵👛🧾"},
		{"🧭\U0001F6E1⚓", "🧭🛡️⚓"}, // the library's spelling, VS16 restored
		{"💵💵👛", ""},              // an emoji twice is not arms
		{"", ""},
		{"  ", ""},
		// Theme glyphs, not library charges: a string split would show them.
		{"blue 🌱 . 🔥 . 📦", ""},
		{"plaid 🧭 . . . .", ""},
	} {
		if got := armsRun(tc.in); got != tc.want {
			t.Errorf("armsRun(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestBeadTitleLinkArms(t *testing.T) {
	b := &Bot{beadsViewerURL: "https://beads.test"}
	if got, want := b.beadTitleLinkArms("gd-1", "Harbour", planArms), "💵👛🧾 <https://beads.test/gd-1|Harbour>"; got != want {
		t.Errorf("armed: %q, want %q", got, want)
	}
	if got, want := b.beadTitleLinkArms("gd-1", "Harbour", ""), "<https://beads.test/gd-1|Harbour>"; got != want {
		t.Errorf("unarmed: %q, want %q", got, want)
	}
	if got, want := b.beadTitleLinkArms("gd-1", "Harbour", "blue 🌱 . 🔥 . 📦"), "<https://beads.test/gd-1|Harbour>"; got != want {
		t.Errorf("not arms: %q, want %q", got, want)
	}
}

func TestWithSubjectRun_OnTheHeaderLine(t *testing.T) {
	b := &Bot{beadsViewerURL: "https://beads.test"}
	armed := &beadsapi.BeadDetail{ID: "gd-EpicArms01", Type: "epic", Title: "Phase 2: harbour the whole fleet before the storm", ShortTitle: "Harbour the fleet", Heraldry: epicArms}
	run := "🧭⚓ <https://beads.test/gd-EpicArms01|Harbour the fleet>"

	head := headerSection("🍃 working · _gardenio_ · *gd-1*\nsecond line")
	ctxBlock := slack.NewContextBlock("", slack.NewTextBlockObject("mrkdwn", "ctx", false, false))
	in := []slack.Block{head, ctxBlock}

	out := b.withSubjectRun(in, armed)
	if got, want := out[0].(*slack.SectionBlock).Text.Text, "🍃 working · _gardenio_ · *gd-1* · "+run+"\nsecond line"; got != want {
		t.Errorf("header %q, want %q", got, want)
	}
	if head.Text.Text != "🍃 working · _gardenio_ · *gd-1*\nsecond line" {
		t.Error("withSubjectRun edited the caller's section")
	}
	if out[1] != ctxBlock || len(out) != 2 {
		t.Error("withSubjectRun changed the rest of the card")
	}
	if out[0].(*slack.SectionBlock).Accessory != nil {
		t.Error("the card carries an accessory; arms are text now")
	}

	// A card that leads with a header block (the wrap-up) takes it on the
	// first mrkdwn section below.
	hb := slack.NewHeaderBlock(slack.NewTextBlockObject("plain_text", "Done — shipped", false, false))
	plain := slack.NewSectionBlock(slack.NewTextBlockObject("plain_text", "plain", false, false), nil, nil)
	out = b.withSubjectRun([]slack.Block{hb, plain, headerSection("restart")}, armed)
	if got := out[2].(*slack.SectionBlock).Text.Text; got != "restart · "+run {
		t.Errorf("wrap-up section %q, want the run on it", got)
	}
	if out[1] != plain {
		t.Error("withSubjectRun wrote into a plain_text section")
	}

	// No arms, or no section in the head, leaves the card alone.
	for _, subject := range []*beadsapi.BeadDetail{nil, {ID: "gd-1", Type: "task"}, {ID: "gd-2", Type: "epic", Heraldry: "blue 🌱 . 🔥 . 📦"}} {
		if out := b.withSubjectRun(in, subject); out[0] != head {
			t.Errorf("subject %+v changed the card", subject)
		}
	}
	far := []slack.Block{hb, ctxBlock, ctxBlock, headerSection("body")}
	if out := b.withSubjectRun(far, armed); out[3] != far[3] {
		t.Error("withSubjectRun reached past the card's head")
	}
}

func TestParseBeadEvent_CarriesHeraldry(t *testing.T) {
	data, _ := json.Marshal(map[string]any{"bead": map[string]any{"id": "gd-1", "type": "epic", "heraldry": planArms}})
	if ev := ParseBeadEvent(data); ev == nil || ev.Heraldry != planArms {
		t.Fatalf("ParseBeadEvent = %+v, want heraldry %q", ev, planArms)
	}
}

// recordingSlack is a Slack Web API that takes every call and records each
// chat.postMessage's blocks.
func recordingSlack(t *testing.T) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var posts []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "chat.postMessage") {
			mu.Lock()
			posts = append(posts, r.FormValue("blocks"))
			mu.Unlock()
		}
		_, _ = io.WriteString(w, `{"ok":true,"channel":"C1","ts":"1700000000.000100","message_ts":"1700000000.000100"}`)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), posts...)
	}
}

// armedPlan puts an epic bearing planArms in daemon, the subject of the cards
// below.
func armedPlan(daemon *mockDaemon) {
	daemon.beads["gd-PlanArmed01"] = &beadsapi.BeadDetail{
		ID: "gd-PlanArmed01", Type: "epic", Status: "open", Title: "Harbour the fleet before the storm",
		ShortTitle: "Harbour the fleet", Heraldry: planArms,
	}
}

// postedHeader is the first line of the first section of the posted card.
func postedHeader(t *testing.T, blocksJSON string) string {
	t.Helper()
	var blocks slack.Blocks
	if err := json.Unmarshal([]byte(blocksJSON), &blocks); err != nil {
		t.Fatal(err)
	}
	for _, bl := range blocks.BlockSet {
		if sec, ok := bl.(*slack.SectionBlock); ok && sec.Text != nil {
			head, _, _ := strings.Cut(sec.Text.Text, "\n")
			return head
		}
	}
	t.Fatalf("card has no section: %s", blocksJSON)
	return ""
}

func assertRunCard(t *testing.T, posts []string, run, id string) {
	t.Helper()
	if len(posts) == 0 {
		t.Fatal("no card posted")
	}
	if head := postedHeader(t, posts[0]); !strings.HasSuffix(head, " · "+run+" <https://beads.test/"+id+"|Harbour the fleet>") {
		t.Errorf("header line %q, want the run %s before the subject's linked short title", head, run)
	}
	if strings.Contains(posts[0], "slack_file") || strings.Contains(posts[0], `"accessory":{"type":"image"`) {
		t.Errorf("the card carries a picture; arms are text: %s", posts[0])
	}
}

func TestNotifyNewEscalation_RunOfTheRequestingTask(t *testing.T) {
	daemon := newMockDaemon()
	armedPlan(daemon)
	fields := map[string]string{"operation_type": "helm_deploy", "requesting_task": "gd-PlanArmed01"}
	daemon.beads["gd-esc-armed"] = &beadsapi.BeadDetail{ID: "gd-esc-armed", Type: "escalation", Title: "Roll the cut", Fields: fields}
	srv, posts := recordingSlack(t)
	b := newTestBot(daemon, srv)
	b.channel = "C1"
	b.beadsViewerURL = "https://beads.test"

	if err := b.NotifyNewEscalation(context.Background(), BeadEvent{ID: "gd-esc-armed", Type: "escalation", Title: "Roll the cut", Fields: fields}); err != nil {
		t.Fatal(err)
	}
	assertRunCard(t, posts(), "💵👛🧾", "gd-PlanArmed01")
}

func TestNotifyDecision_RunOfTheRequestingTask(t *testing.T) {
	daemon := newMockDaemon()
	armedPlan(daemon)
	srv, posts := recordingSlack(t)
	b := newTestBot(daemon, srv)
	b.channel = "C1"
	b.beadsViewerURL = "https://beads.test"

	opts, _ := json.Marshal([]string{"ship", "wait"})
	if err := b.NotifyDecision(context.Background(), BeadEvent{
		ID: "gd-dec-armed", Type: "decision", Title: "Ship it?", Priority: 2,
		Fields: map[string]string{"prompt": "Ship it?", "options": string(opts), "requesting_task": "gd-PlanArmed01"},
	}); err != nil {
		t.Fatal(err)
	}
	assertRunCard(t, posts(), "💵👛🧾", "gd-PlanArmed01")
}

func TestEnsureAgentCard_RunOfTheAgentsTask(t *testing.T) {
	daemon := newMockDaemon()
	daemon.beads["gd-PhaseArmed1"] = &beadsapi.BeadDetail{
		ID: "gd-PhaseArmed1", Type: "epic", Kind: "issue", Status: "in_progress",
		Assignee: "crew-armed", Title: "Phase 2: the renderer", ShortTitle: "Harbour the fleet", Heraldry: epicArms,
	}
	srv, posts := recordingSlack(t)
	b := newTestBot(daemon, srv)
	b.beadsViewerURL = "https://beads.test"
	b.agentState["crew-armed"] = "working"

	if _, err := b.ensureAgentCard(context.Background(), "crew-armed", "C1"); err != nil {
		t.Fatal(err)
	}
	assertRunCard(t, posts(), "🧭⚓", "gd-PhaseArmed1")
}

// lineDaemon is mockDaemon with forward edges, so the card can walk a task's
// line (badge.LineReader).
type lineDaemon struct {
	*mockDaemon
	deps     map[string][]beadsapi.Dependency
	depCalls int
}

func (d *lineDaemon) GetDependencies(_ context.Context, id string) ([]beadsapi.Dependency, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.depCalls++
	return d.deps[id], nil
}

// A task bears no arms (R6): the agent card shows its line's, the nearest
// armed ancestor's run, before the task's linked short title; a task whose
// line bears none leaves the header as it was (gd-ASLjQPkHJz).
func TestEnsureAgentCard_RunOfTheTasksLine(t *testing.T) {
	daemon := &lineDaemon{mockDaemon: newMockDaemon(), deps: map[string][]beadsapi.Dependency{
		"gd-TaskLine01": {{BeadID: "gd-TaskLine01", DependsOnID: "gd-PhaseBare1", Type: "parent-child"}},
		"gd-PhaseBare1": {{BeadID: "gd-PhaseBare1", DependsOnID: "gd-PlanArmed01", Type: "parent-child"}},
	}}
	armedPlan(daemon.mockDaemon)
	daemon.beads["gd-PhaseBare1"] = &beadsapi.BeadDetail{ID: "gd-PhaseBare1", Type: "epic", Status: "open", Title: "Phase 1"}
	daemon.beads["gd-TaskLine01"] = &beadsapi.BeadDetail{
		ID: "gd-TaskLine01", Type: "task", Kind: "issue", Status: "in_progress",
		Assignee: "crew-line", Title: "Draw the harbour chart", ShortTitle: "Harbour the fleet",
	}
	daemon.beads["gd-TaskLone01"] = &beadsapi.BeadDetail{
		ID: "gd-TaskLone01", Type: "task", Kind: "issue", Status: "in_progress",
		Assignee: "crew-lone", Title: "A loose task", ShortTitle: "Loose task",
	}
	srv, posts := recordingSlack(t)
	b := newTestBot(daemon, srv)
	b.beadsViewerURL = "https://beads.test"
	b.agentState["crew-line"] = "working"
	b.agentState["crew-lone"] = "working"

	if _, err := b.ensureAgentCard(context.Background(), "crew-line", "C1"); err != nil {
		t.Fatal(err)
	}
	assertRunCard(t, posts(), "💵👛🧾", "gd-TaskLine01")

	if _, err := b.ensureAgentCard(context.Background(), "crew-lone", "C1"); err != nil {
		t.Fatal(err)
	}
	all := posts()
	if head := postedHeader(t, all[len(all)-1]); strings.Contains(head, "gd-TaskLone01") || strings.Contains(head, " ·  ") {
		t.Errorf("a task with no armed line adds nothing to the header: %q", head)
	}

	// The card is redrawn often: the line is walked once per task per TTL.
	before := daemon.depCalls
	if got := b.taskLineArms(context.Background(), daemon.beads["gd-TaskLine01"]); got != planArms {
		t.Errorf("cached line arms %q, want %q", got, planArms)
	}
	if daemon.depCalls != before {
		t.Errorf("a cached task's line was read again (%d more)", daemon.depCalls-before)
	}
}
