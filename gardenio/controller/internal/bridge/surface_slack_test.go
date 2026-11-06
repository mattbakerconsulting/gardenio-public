package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/conversation"
	"gardenio/controller/internal/conversation/surfacetest"

	"github.com/slack-go/slack"
)

var updateSlackSurfaceGolden = flag.Bool("update-slack-surface", false,
	"rewrite testdata/slack_surface_calls.golden.json from the Slack surface's recorded calls")

// newTwinSurface returns a Slack surface whose client talks to a fresh
// slackmock twin.
func newTwinSurface(t *testing.T) (*slackSurface, *surfacetest.SlackTwin) {
	t.Helper()
	twin := surfacetest.NewSlackTwin()
	return newSlackSurface(slack.New("xoxb-test", slack.OptionAPIURL(twin.Serve(t)))), twin
}

// newTwinBot returns a test Bot whose Slack client talks to twin, for driving
// today's direct-call paths side by side with the surface.
func newTwinBot(t *testing.T, twin *surfacetest.SlackTwin) *Bot {
	t.Helper()
	srv := newFakeSlackServer(t)
	t.Cleanup(srv.Close)
	b := newTestBot(newMockDaemon(), srv)
	b.api = slack.New("xoxb-test", slack.OptionAPIURL(twin.Serve(t)))
	return b
}

var (
	twinChannel = conversation.ChannelRef{ID: "gd-chan-crew", Land: "gardenio", Slack: "C0CONTRACT"}
	twinPerson  = conversation.Principal{ID: "oidc:op@example.com", SlackUser: "U0OP"}
	twinTrigger = conversation.Trigger{Surface: conversation.SurfaceSlack, TriggerID: "1337.42.abcd", Channel: "C0CONTRACT"}
)

// TestSlackSurfaceContract runs the Surface contract against the Slack
// surface over the slackmock twin: the same contract the Recorder passes.
func TestSlackSurfaceContract(t *testing.T) {
	surfacetest.RunContract(t, surfacetest.Harness{
		New: func(t *testing.T) (conversation.Surface, surfacetest.Lookup) {
			s, twin := newTwinSurface(t)
			return s, twin.Lookup
		},
		Channel: twinChannel,
		To:      twinPerson,
		Trigger: twinTrigger,
	})
}

// TestSlackSurfaceCallsGolden pins the Web API calls (method + params) the
// Slack surface sends for one of each op. A later slice that moves a handler
// behind the surface compares that handler's calls the same way; this golden
// is the surface's own half (design section 5.2, "Slack payload golden").
// Regenerate with: go test ./internal/bridge -run TestSlackSurfaceCallsGolden -update-slack-surface
func TestSlackSurfaceCallsGolden(t *testing.T) {
	s, twin := newTwinSurface(t)
	s.render = func(_ context.Context, channel string, m conversation.Message) ([]slack.Block, error) {
		return []slack.Block{slack.NewSectionBlock(
			slack.NewTextBlockObject("mrkdwn", "*card* "+m.SubjectID+" in "+channel, false, false), nil, nil)}, nil
	}
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	root, err := s.Post(ctx, twinChannel, conversation.Message{Text: "top-level post"})
	must(err)
	_, err = s.Reply(ctx, root, conversation.Message{Text: "thread reply"}, false)
	must(err)
	_, err = s.Reply(ctx, root, conversation.Message{Text: "broadcast reply"}, true)
	must(err)
	card, err := s.Post(ctx, twinChannel, conversation.Message{Kind: conversation.KindCard, SubjectID: "gd-agent1", Text: "crew-1 is working"})
	must(err)
	must(s.Update(ctx, card, conversation.Message{Kind: conversation.KindCard, SubjectID: "gd-agent1", Text: "crew-1 is done"}))
	must(s.React(ctx, card, "white_check_mark", true))
	must(s.React(ctx, card, "white_check_mark", false))
	must(s.Delete(ctx, root))
	_, err = s.DM(ctx, twinPerson, conversation.Message{Text: "a DM"})
	must(err)
	must(s.Ephemeral(ctx, twinTrigger, twinPerson, conversation.Message{Text: "only you can see this"}))
	must(s.OpenModal(ctx, twinTrigger, surfacetest.ContractModal()))

	got, err := json.MarshalIndent(twin.Calls(), "", "  ")
	must(err)
	got = append(got, '\n')
	path := filepath.Join("testdata", "slack_surface_calls.golden.json")
	if *updateSlackSurfaceGolden {
		must(os.WriteFile(path, got, 0o644))
	}
	want, err := os.ReadFile(path)
	must(err)
	if string(got) != string(want) {
		t.Fatalf("Slack surface calls drifted from %s (rerun with -update-slack-surface if intended):\n%s", path, got)
	}
}

// sameCalls fails unless the two recordings hold the same Web API calls.
func sameCalls(t *testing.T, what string, today, surface []surfacetest.TwinCall) {
	t.Helper()
	if !reflect.DeepEqual(today, surface) {
		a, _ := json.MarshalIndent(today, "", "  ")
		b, _ := json.MarshalIndent(surface, "", "  ")
		t.Fatalf("%s: the surface's Slack payload differs from today's.\ntoday:\n%s\nsurface:\n%s", what, a, b)
	}
}

// The parity tests drive one of today's direct Slack paths and the matching
// Surface op against two identical twins and require identical calls: the
// proof that a handler can move behind the surface with no Slack-visible
// change (moved file, not shape) (gd-L7OpWmCUTh).

func TestSlackSurfaceParityReply(t *testing.T) {
	todayTwin, surfTwin := surfacetest.NewSlackTwin(), surfacetest.NewSlackTwin()
	b := newTwinBot(t, todayTwin)
	s := newSlackSurface(slack.New("xoxb-test", slack.OptionAPIURL(surfTwin.Serve(t))))
	ctx := context.Background()

	if err := b.replyInThread(ctx, "C0CREW", "1700000000.000001", "a reply"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Reply(ctx, conversation.Ref{Surface: conversation.SurfaceSlack, Channel: "C0CREW", TS: "1700000000.000001"},
		conversation.Message{Text: "a reply"}, false); err != nil {
		t.Fatal(err)
	}
	sameCalls(t, "replyInThread vs Reply", todayTwin.Calls(), surfTwin.Calls())
}

func TestSlackSurfaceParityDM(t *testing.T) {
	todayTwin, surfTwin := surfacetest.NewSlackTwin(), surfacetest.NewSlackTwin()
	b := newTwinBot(t, todayTwin)
	s := newSlackSurface(slack.New("xoxb-test", slack.OptionAPIURL(surfTwin.Serve(t))))
	ctx := context.Background()

	_, todayTS, err := b.SendDM(ctx, "U0OP", "hello in private")
	if err != nil {
		t.Fatal(err)
	}
	ref, err := s.DM(ctx, twinPerson, conversation.Message{Text: "hello in private"})
	if err != nil {
		t.Fatal(err)
	}
	if ref.TS != todayTS {
		t.Fatalf("DM ts %q, today %q", ref.TS, todayTS)
	}
	sameCalls(t, "SendDM vs DM", todayTwin.Calls(), surfTwin.Calls())

	// With the Bot's cached opener, a second DM skips conversations.open the
	// way SendDM does.
	todayTwin.Reset()
	surfTwin.Reset()
	s.openDM = func(ctx context.Context, user string) (string, error) { return "DU0OP", nil }
	if _, _, err := b.SendDM(ctx, "U0OP", "again"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DM(ctx, twinPerson, conversation.Message{Text: "again"}); err != nil {
		t.Fatal(err)
	}
	sameCalls(t, "cached SendDM vs DM", todayTwin.Calls(), surfTwin.Calls())
}

func TestSlackSurfaceParityEphemeral(t *testing.T) {
	todayTwin, surfTwin := surfacetest.NewSlackTwin(), surfacetest.NewSlackTwin()
	b := newTwinBot(t, todayTwin)
	s := newSlackSurface(slack.New("xoxb-test", slack.OptionAPIURL(surfTwin.Serve(t))))
	ctx := context.Background()

	cb := slack.InteractionCallback{User: slack.User{ID: "U0OP"}}
	b.postInteractionEphemeral(ctx, cb, "C0CREW", "not allowed")
	if err := s.Ephemeral(ctx, conversation.Trigger{Surface: conversation.SurfaceSlack, Channel: "C0CREW"}, twinPerson,
		conversation.Message{Text: "not allowed"}); err != nil {
		t.Fatal(err)
	}
	sameCalls(t, "postInteractionEphemeral vs Ephemeral", todayTwin.Calls(), surfTwin.Calls())
}

func TestSlackSurfaceParityOtherModal(t *testing.T) {
	todayTwin, surfTwin := surfacetest.NewSlackTwin(), surfacetest.NewSlackTwin()
	b := newTwinBot(t, todayTwin)
	s := newSlackSurface(slack.New("xoxb-test", slack.OptionAPIURL(surfTwin.Serve(t))))
	ctx := context.Background()

	cb := slack.InteractionCallback{TriggerID: "1337.42.abcd"}
	cb.Channel.ID = "C0DEC"
	cb.Message.Timestamp = "1700000000.000009"
	b.openOtherModal(ctx, "gd-dec1", cb)

	opts := []conversation.Option{{Value: "none", Label: "None (no artifact required)"}}
	for _, at := range beadsapi.ArtifactTypes {
		opts = append(opts, conversation.Option{Value: at, Label: at})
	}
	v := conversation.Modal{
		CallbackID: "resolve_other",
		Title:      "Custom Response",
		Submit:     "Submit",
		Close:      "Cancel",
		Private:    "gd-dec1|C0DEC|1700000000.000009",
		Fields: []conversation.Field{
			{Kind: conversation.FieldNote, Text: "*gd-dec1*"}, // the mock daemon has no such bead, so the question falls back to the id
			{Kind: conversation.FieldDivider},
			{Kind: conversation.FieldText, ID: "response", InputID: "response_input", Label: "Your Response",
				Placeholder: "Type your response...", Multiline: true},
			{Kind: conversation.FieldSelect, ID: "artifact_type", InputID: "artifact_type_input", Label: "Required Artifact Type",
				Hint: "What artifact will you produce?", Placeholder: "Choose artifact type...", Options: opts, Initial: "none"},
		},
	}
	if err := s.OpenModal(ctx, conversation.Trigger{Surface: conversation.SurfaceSlack, TriggerID: cb.TriggerID}, v); err != nil {
		t.Fatal(err)
	}
	if len(todayTwin.Calls()) != 1 {
		t.Fatalf("openOtherModal made %d calls, want one views.open", len(todayTwin.Calls()))
	}
	sameCalls(t, "openOtherModal vs OpenModal", todayTwin.Calls(), surfTwin.Calls())
}

func TestSlackSurfaceParityFeedbackModal(t *testing.T) {
	todayTwin, surfTwin := surfacetest.NewSlackTwin(), surfacetest.NewSlackTwin()
	b := newTwinBot(t, todayTwin)
	s := newSlackSurface(slack.New("xoxb-test", slack.OptionAPIURL(surfTwin.Serve(t))))
	ctx := context.Background()

	cb := slack.InteractionCallback{TriggerID: "1337.42.abcd"}
	cb.Channel.ID = "C0CREW"
	cb.Message.Timestamp = "1700000000.000003"
	b.openFeedbackModal(ctx, "gd-agent1", cb)

	v := conversation.Modal{
		CallbackID: humanSurveyFeedbackCallbackID,
		Title:      "Add Feedback",
		Submit:     "Submit",
		Close:      "Cancel",
		Private:    "gd-agent1|C0CREW|1700000000.000003",
		Fields: []conversation.Field{
			{Kind: conversation.FieldNote, Text: "*Feedback for agent session* `gd-agent1`"},
			{Kind: conversation.FieldText, ID: "feedback", InputID: "feedback_input", Label: "Your feedback",
				Placeholder: "What worked, what didn't…", Multiline: true},
		},
	}
	if err := s.OpenModal(ctx, conversation.Trigger{Surface: conversation.SurfaceSlack, TriggerID: cb.TriggerID}, v); err != nil {
		t.Fatal(err)
	}
	sameCalls(t, "openFeedbackModal vs OpenModal", todayTwin.Calls(), surfTwin.Calls())
}

func TestSlackSurfaceCards(t *testing.T) {
	ctx := context.Background()
	card := conversation.Message{Kind: conversation.KindCard, SubjectID: "gd-agent1", Text: "crew-1"}

	s, twin := newTwinSurface(t)
	if _, err := s.Post(ctx, twinChannel, card); !errors.Is(err, conversation.ErrUnsupported) {
		t.Fatalf("card with no renderer: err = %v, want ErrUnsupported", err)
	}
	if n := len(twin.Calls()); n != 0 {
		t.Fatalf("unrenderable card still called Slack %d times", n)
	}

	// A notice with no renderer is plain text.
	if _, err := s.Post(ctx, twinChannel, conversation.Message{Kind: conversation.KindNotice, Text: "crash"}); err != nil {
		t.Fatalf("notice: %v", err)
	}

	var gotChannel string
	s.render = func(_ context.Context, channel string, m conversation.Message) ([]slack.Block, error) {
		gotChannel = channel
		return []slack.Block{slack.NewDividerBlock()}, nil
	}
	ref, err := s.Post(ctx, twinChannel, card)
	if err != nil {
		t.Fatalf("card: %v", err)
	}
	if gotChannel != "C0CONTRACT" {
		t.Fatalf("renderer saw channel %q", gotChannel)
	}
	msgs := twin.Messages("C0CONTRACT")
	last := msgs[len(msgs)-1]
	if last[0] != ref.TS || last[1] != "crew-1" || last[2] == "" {
		t.Fatalf("card posted as %v, want fallback text plus blocks", last)
	}

	s.render = func(context.Context, string, conversation.Message) ([]slack.Block, error) {
		return nil, errors.New("subject gone")
	}
	if err := s.Update(ctx, ref, card); err == nil {
		t.Fatal("render failure did not fail the update")
	}

	if _, err := s.Post(ctx, twinChannel, conversation.Message{Kind: conversation.KindCard, SubjectID: "gd-agent1", Files: []conversation.File{{Name: "a.txt", Data: []byte("x")}}}); !errors.Is(err, conversation.ErrUnsupported) {
		t.Fatalf("files on a card: err = %v, want ErrUnsupported", err)
	}
	if _, err := s.Post(ctx, twinChannel, conversation.Message{Text: "x", Files: []conversation.File{{Name: "a.txt"}}}); !errors.Is(err, conversation.ErrEmptyMessage) {
		t.Fatalf("empty file: err = %v, want ErrEmptyMessage", err)
	}
}

// A notice sends exactly one text param on either face (gd-aMglRLWRfn): the
// terse face is Detail alone, so the Cause / Pod lines always show; the rich
// face is Text as the fallback plus the Detail section block. The twin keeps
// only the first value of a repeated param, so this reads the form directly.
func TestSlackSurfaceNoticeOneTextParam(t *testing.T) {
	s := newSlackSurface(nil)
	notice := conversation.Message{
		Kind:     conversation.KindNotice,
		Text:     "Agent died: crew-1 — oom",
		Detail:   ":warning: *Agent died: crew-1*\n> Cause: oom",
		Footnote: "Forensics on bead `gd-1`",
	}
	for _, tc := range []struct {
		rich       bool
		wantText   string
		wantBlocks bool
	}{
		{rich: false, wantText: notice.Detail},
		{rich: true, wantText: notice.Text, wantBlocks: true},
	} {
		m := notice
		m.Rich = tc.rich
		opts, err := s.msgOptions(context.Background(), "C0", m)
		if err != nil {
			t.Fatal(err)
		}
		_, form, err := slack.UnsafeApplyMsgOptions("xoxb-test", "C0", "", opts...)
		if err != nil {
			t.Fatal(err)
		}
		if got := form["text"]; len(got) != 1 || got[0] != tc.wantText {
			t.Errorf("rich=%v: text params = %q, want exactly [%q]", tc.rich, got, tc.wantText)
		}
		if got := form.Get("blocks") != ""; got != tc.wantBlocks {
			t.Errorf("rich=%v: blocks sent = %v, want %v", tc.rich, got, tc.wantBlocks)
		}
	}
}

// Files on a text message are shared through the files.upload v2 flow: one
// upload per file and one completeUploadExternal, so several files land as
// one message with the text as its comment. The ref has no ts (the upload
// API does not return the sharing message).
func TestSlackSurfaceUploadsFiles(t *testing.T) {
	s, twin := newTwinSurface(t)
	root := conversation.Ref{Surface: conversation.SurfaceSlack, Channel: "C0CREW", TS: "1700000000.000001"}
	ref, err := s.Reply(context.Background(), root, conversation.Message{Text: "two shots", Files: []conversation.File{
		{Name: "a.png", Title: "Before", AltText: "the old page", Data: []byte("AAA")},
		{Name: "b.png", Data: []byte("BB")},
	}}, false)
	if err != nil {
		t.Fatal(err)
	}
	if ref.Channel != "C0CREW" || ref.TS != "" {
		t.Fatalf("ref = %+v, want the channel and no ts", ref)
	}
	var methods []string
	for _, c := range twin.Calls() {
		methods = append(methods, c.Method)
	}
	want := []string{"files.getUploadURLExternal", "upload", "files.getUploadURLExternal", "upload", "files.completeUploadExternal"}
	if !reflect.DeepEqual(methods, want) {
		t.Fatalf("calls = %v, want %v", methods, want)
	}
	done := twin.Calls()[4].Params
	if done["files"] != `[{"id":"F00000001","title":"Before"},{"id":"F00000002","title":"b.png"}]` ||
		done["initial_comment"] != "two shots" || done["thread_ts"] != root.TS || done["channel_id"] != "C0CREW" {
		t.Fatalf("completeUploadExternal params = %v", done)
	}
	if alt := twin.Calls()[0].Params["alt_txt"]; alt != "the old page" {
		t.Fatalf("alt_txt = %q", alt)
	}
}

// TestSlackSurfaceUnderRouter is the seam end to end: a Router writing beads
// to a fake store and projecting through the Slack surface onto the twin.
func TestSlackSurfaceUnderRouter(t *testing.T) {
	s, twin := newTwinSurface(t)
	daemon := newMockDaemon()
	r := conversation.NewRouter(daemon, nil, s)
	d, err := r.Post(context.Background(), conversation.ChannelRef{Land: "gardenio", Slack: "C0CREW"},
		conversation.Message{Text: "routed", Author: conversation.Principal{ID: "agent:crew-1"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Failed) != 0 {
		t.Fatalf("projection failed: %v", d.Failed)
	}
	ref, ok := d.Ref(conversation.SurfaceSlack)
	if !ok {
		t.Fatal("no slack ref")
	}
	if got, ok := twin.Lookup(ref); !ok || got.Text != "routed" {
		t.Fatalf("twin shows %+v (%v)", got, ok)
	}
	bead, err := daemon.GetBead(context.Background(), d.BeadID)
	if err != nil {
		t.Fatal(err)
	}
	if bead.Fields["slack_ts"] != ref.TS || bead.Fields["slack_channel"] != "C0CREW" {
		t.Fatalf("bead fields = %v, want the slack ref", bead.Fields)
	}
}
