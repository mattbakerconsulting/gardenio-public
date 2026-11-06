package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"

	slackapi "github.com/slack-go/slack"
)

type fakeNoticePublisher struct {
	createReq  beadsapi.CreateBeadRequest
	createID   string
	createErr  error
	closeID    string
	closeCalls int
	closeErr   error
}

func (f *fakeNoticePublisher) CreateBead(_ context.Context, req beadsapi.CreateBeadRequest) (string, error) {
	f.createReq = req
	if f.createErr != nil {
		return "", f.createErr
	}
	if f.createID == "" {
		f.createID = "gd-test-1"
	}
	return f.createID, nil
}

func (f *fakeNoticePublisher) CloseBead(_ context.Context, beadID string, _ map[string]string) error {
	f.closeID = beadID
	f.closeCalls++
	return f.closeErr
}

func TestNewNoticeNotifier_PublishesClosedMessageBead(t *testing.T) {
	pub := &fakeNoticePublisher{createID: "gd-abc"}
	notifier := NewNoticeNotifier(pub, "trigger-engine", slog.Default())
	if notifier == nil {
		t.Fatal("expected non-nil notifier")
	}

	notifier(context.Background(), "my-trigger", "hello world", "")

	if pub.createReq.Type != "message" {
		t.Errorf("type = %q, want message", pub.createReq.Type)
	}
	if pub.createReq.Kind != "data" {
		t.Errorf("kind = %q, want data", pub.createReq.Kind)
	}
	if pub.createReq.Title != "hello world" {
		t.Errorf("title = %q, want %q", pub.createReq.Title, "hello world")
	}
	if pub.createReq.CreatedBy != "trigger-engine" {
		t.Errorf("created_by = %q, want trigger-engine", pub.createReq.CreatedBy)
	}
	if !hasLabel(pub.createReq.Labels, NoticeLabel) {
		t.Errorf("labels missing %q: %v", NoticeLabel, pub.createReq.Labels)
	}
	if !hasLabel(pub.createReq.Labels, "from:trigger-engine") {
		t.Errorf("labels missing from:trigger-engine: %v", pub.createReq.Labels)
	}

	var fields map[string]string
	if err := json.Unmarshal(pub.createReq.Fields, &fields); err != nil {
		t.Fatalf("fields not JSON: %v", err)
	}
	if fields["text"] != "hello world" {
		t.Errorf("fields.text = %q", fields["text"])
	}
	if fields["trigger_title"] != "my-trigger" {
		t.Errorf("fields.trigger_title = %q", fields["trigger_title"])
	}
	if fields["source_agent"] != "trigger-engine" {
		t.Errorf("fields.source_agent = %q", fields["source_agent"])
	}

	if pub.closeCalls != 1 {
		t.Errorf("CloseBead called %d times, want 1", pub.closeCalls)
	}
	if pub.closeID != "gd-abc" {
		t.Errorf("closed %q, want gd-abc", pub.closeID)
	}
}

func TestNewNoticeNotifier_SkipsEmptyMessage(t *testing.T) {
	pub := &fakeNoticePublisher{}
	notifier := NewNoticeNotifier(pub, "trigger-engine", slog.Default())

	notifier(context.Background(), "title", "", "")

	if pub.closeCalls != 0 {
		t.Errorf("CloseBead called %d times for empty message, want 0", pub.closeCalls)
	}
	if pub.createReq.Type != "" {
		t.Errorf("CreateBead invoked for empty message: %+v", pub.createReq)
	}
}

// TestNewNoticeNotifier_ChannelRoutesToNoticeChannelField verifies a non-empty
// channel is written to NoticeChannelField so slack-bridge's Notice watcher
// posts the notice to that channel instead of its default (ka-0000008204).
func TestNewNoticeNotifier_ChannelRoutesToNoticeChannelField(t *testing.T) {
	pub := &fakeNoticePublisher{}
	notifier := NewNoticeNotifier(pub, "trigger-engine", slog.Default())

	notifier(context.Background(), "my-trigger", "pipeline failed", "C073CG65QUD")

	var fields map[string]string
	if err := json.Unmarshal(pub.createReq.Fields, &fields); err != nil {
		t.Fatalf("fields not JSON: %v", err)
	}
	if got := fields[NoticeChannelField]; got != "C073CG65QUD" {
		t.Errorf("fields[%q] = %q, want C073CG65QUD", NoticeChannelField, got)
	}
}

// TestNewNoticeNotifier_EmptyChannelOmitsNoticeChannelField verifies an empty
// channel leaves NoticeChannelField unset — the Notice watcher then falls
// through to its configured default channel (unchanged legacy behavior).
func TestNewNoticeNotifier_EmptyChannelOmitsNoticeChannelField(t *testing.T) {
	pub := &fakeNoticePublisher{}
	notifier := NewNoticeNotifier(pub, "trigger-engine", slog.Default())

	notifier(context.Background(), "my-trigger", "pipeline failed", "")

	var fields map[string]string
	if err := json.Unmarshal(pub.createReq.Fields, &fields); err != nil {
		t.Fatalf("fields not JSON: %v", err)
	}
	if _, ok := fields[NoticeChannelField]; ok {
		t.Errorf("fields[%q] present for empty channel: %q", NoticeChannelField, fields[NoticeChannelField])
	}
}

func TestNewNoticeNotifier_NilDaemonReturnsNil(t *testing.T) {
	if got := NewNoticeNotifier(nil, "x", nil); got != nil {
		t.Error("expected nil notifier when daemon is nil")
	}
}

func TestNewNoticeNotifier_CreateErrorDoesNotPanic(t *testing.T) {
	pub := &fakeNoticePublisher{createErr: errors.New("boom")}
	notifier := NewNoticeNotifier(pub, "trigger-engine", slog.Default())
	notifier(context.Background(), "t", "msg", "") // should not panic
	if pub.closeCalls != 0 {
		t.Errorf("CloseBead called despite Create error")
	}
}

func TestNewNoticeNotifier_LongMessageTruncatedInTitle(t *testing.T) {
	pub := &fakeNoticePublisher{}
	notifier := NewNoticeNotifier(pub, "trigger-engine", slog.Default())

	long := make([]byte, 600)
	for i := range long {
		long[i] = 'x'
	}
	notifier(context.Background(), "t", string(long), "")

	if len(pub.createReq.Title) != 500 {
		t.Errorf("title length = %d, want 500", len(pub.createReq.Title))
	}
	// Full text preserved in fields.
	var fields map[string]string
	_ = json.Unmarshal(pub.createReq.Fields, &fields)
	if len(fields["text"]) != 600 {
		t.Errorf("fields.text length = %d, want 600", len(fields["text"]))
	}
}

type fakeSlackAPI struct {
	calls   int
	channel string
	err     error
}

func (f *fakeSlackAPI) PostMessageContext(_ context.Context, channelID string, _ ...slackapi.MsgOption) (string, string, error) {
	f.calls++
	f.channel = channelID
	return "", "", f.err
}

// marshalBeadEvent encodes a bead as the SSE wire payload: {"bead": {...}}.
func marshalBeadEvent(t *testing.T, id, btype string, labels []string, fields map[string]string) []byte {
	t.Helper()
	fieldsJSON, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("marshal fields: %v", err)
	}
	inner := map[string]any{
		"id":     id,
		"type":   btype,
		"labels": labels,
		"fields": json.RawMessage(fieldsJSON),
	}
	innerJSON, err := json.Marshal(inner)
	if err != nil {
		t.Fatalf("marshal bead: %v", err)
	}
	wrapper := map[string]json.RawMessage{"bead": innerJSON}
	out, err := json.Marshal(wrapper)
	if err != nil {
		t.Fatalf("marshal wrapper: %v", err)
	}
	return out
}

func TestNotice_PostsTriggerNoticeBeads(t *testing.T) {
	api := &fakeSlackAPI{}
	n := NewNotice(NoticeConfig{API: api, DefaultChannel: "C123", Logger: slog.Default()})
	if n == nil {
		t.Fatal("NewNotice returned nil")
	}

	data := marshalBeadEvent(t, "gd-1", "message",
		[]string{NoticeLabel, "from:trigger-engine"},
		map[string]string{"text": "hello"})
	n.handleClosed(context.Background(), data)

	if api.calls != 1 {
		t.Errorf("PostMessage calls = %d, want 1", api.calls)
	}
	if api.channel != "C123" {
		t.Errorf("channel = %q, want C123", api.channel)
	}
}

func TestNotice_RespectsChannelOverride(t *testing.T) {
	api := &fakeSlackAPI{}
	n := NewNotice(NoticeConfig{API: api, DefaultChannel: "C-default", Logger: slog.Default()})

	data := marshalBeadEvent(t, "gd-2", "message",
		[]string{NoticeLabel},
		map[string]string{"text": "hi", NoticeChannelField: "C-override"})
	n.handleClosed(context.Background(), data)

	if api.channel != "C-override" {
		t.Errorf("channel = %q, want C-override", api.channel)
	}
}

// TestNotice_ChannelOverrideUsesSchemaValidFieldName is the regression guard for
// ka-0000002479: an agent posts a top-level message to an ARBITRARY channel by
// creating a closed trigger-notice message bead whose override channel rides in
// the `channel` field. The watcher MUST read the same field name the daemon's
// type:message schema declares, or the create that sets the override is rejected
// 400 "<field>: unknown field" before the bead is closed and the override never
// reaches the watcher.
//
// Unlike TestNotice_RespectsChannelOverride, this pins the WIRE field name to the
// literal "channel" (not the NoticeChannelField constant), so flipping the
// constant back to the schema-invalid "slack_channel" — the original bug — reds
// this test even though it would still satisfy the constant-keyed test above.
func TestNotice_ChannelOverrideUsesSchemaValidFieldName(t *testing.T) {
	// The watcher reads the field the type:message schema declares. `channel` is
	// declared (kbeads config.go type:message, guarded by
	// TestBuiltinMessageProducerFieldsDeclared); `slack_channel` is rejected.
	if NoticeChannelField != "channel" {
		t.Fatalf("NoticeChannelField = %q, want %q — the daemon's type:message "+
			"schema declares `channel`; any other name 400s the agent's notice "+
			"create and re-breaks channel-root posting (ka-0000002479)",
			NoticeChannelField, "channel")
	}

	// An agent may target ANY channel id, not just the bridge default.
	for _, ch := range []string{"C02P59QCRD5", "C0AHE7C0E1G", "GARBITRARY123"} {
		api := &fakeSlackAPI{}
		n := NewNotice(NoticeConfig{API: api, DefaultChannel: "C-default", Logger: slog.Default()})

		data := marshalBeadEvent(t, "gd-notice-"+ch, "message",
			[]string{NoticeLabel, "from:agent"},
			// Literal "channel" — the field an agent-created type:message bead can
			// legally carry through daemon validation.
			map[string]string{"text": "review MR 1931", "channel": ch})
		n.handleClosed(context.Background(), data)

		if api.calls != 1 {
			t.Fatalf("channel %s: PostMessage calls = %d, want 1", ch, api.calls)
		}
		if api.channel != ch {
			t.Errorf("channel-root post routed to %q, want %q (arbitrary channel)", api.channel, ch)
		}
	}
}

func TestNotice_IgnoresNonNoticeBeads(t *testing.T) {
	api := &fakeSlackAPI{}
	n := NewNotice(NoticeConfig{API: api, DefaultChannel: "C123"})

	// Wrong type.
	d1 := marshalBeadEvent(t, "k", "task", []string{NoticeLabel}, map[string]string{"text": "t"})
	n.handleClosed(context.Background(), d1)

	// Missing label.
	d2 := marshalBeadEvent(t, "k2", "message", []string{"other"}, map[string]string{"text": "t"})
	n.handleClosed(context.Background(), d2)

	if api.calls != 0 {
		t.Errorf("PostMessage called %d times for non-notice beads", api.calls)
	}
}

func TestNotice_DedupesByBeadID(t *testing.T) {
	api := &fakeSlackAPI{}
	n := NewNotice(NoticeConfig{API: api, DefaultChannel: "C123"})

	data := marshalBeadEvent(t, "gd-dup", "message", []string{NoticeLabel}, map[string]string{"text": "once"})
	n.handleClosed(context.Background(), data)
	n.handleClosed(context.Background(), data)

	if api.calls != 1 {
		t.Errorf("PostMessage calls = %d, want 1 (dedup)", api.calls)
	}
}

func TestNotice_NoChannelNoDefault_SkipsPost(t *testing.T) {
	api := &fakeSlackAPI{}
	n := NewNotice(NoticeConfig{API: api})

	data := marshalBeadEvent(t, "gd-x", "message", []string{NoticeLabel}, map[string]string{"text": "t"})
	n.handleClosed(context.Background(), data)

	if api.calls != 0 {
		t.Errorf("PostMessage called %d times without channel, want 0", api.calls)
	}
}

func TestNewNotice_NilAPI(t *testing.T) {
	if got := NewNotice(NoticeConfig{API: nil}); got != nil {
		t.Error("NewNotice returned non-nil for nil API")
	}
}

// noticeProjectLabels returns every project:<land> label on a notice create.
func noticeProjectLabels(req beadsapi.CreateBeadRequest) []string {
	var out []string
	for _, l := range req.Labels {
		if strings.HasPrefix(l, "project:") {
			out = append(out, l)
		}
	}
	return out
}

// TestNewNoticeNotifier_ProjectLabelFromContext verifies a notice published
// under WithNoticeProject carries exactly one project:<land> label (so it is
// not projectless), that an untagged ctx leaves it unlabelled rather than
// guessing a land, and that a channel override never becomes fields.channel_id
// (which would hold the label to a kbeads channel's land).
func TestNewNoticeNotifier_ProjectLabelFromContext(t *testing.T) {
	pub := &fakeNoticePublisher{}
	notifier := NewNoticeNotifier(pub, "trigger-engine", slog.Default())

	notifier(WithNoticeProject(context.Background(), " gardenio "), "my-trigger", "hello", "C073CG65QUD")
	if got := noticeProjectLabels(pub.createReq); len(got) != 1 || got[0] != "project:gardenio" {
		t.Errorf("project labels = %v, want [project:gardenio] (labels %v)", got, pub.createReq.Labels)
	}
	var fields map[string]string
	if err := json.Unmarshal(pub.createReq.Fields, &fields); err != nil {
		t.Fatalf("fields not JSON: %v", err)
	}
	if _, ok := fields["channel_id"]; ok {
		t.Errorf("notice set fields.channel_id = %q; want the Slack channel only in %q", fields["channel_id"], NoticeChannelField)
	}

	for _, ctx := range []context.Context{context.Background(), WithNoticeProject(context.Background(), "  ")} {
		notifier(ctx, "fleet-sweep", "hello", "")
		if got := noticeProjectLabels(pub.createReq); len(got) != 0 {
			t.Errorf("no-land notice project labels = %v, want none", got)
		}
	}
}

// TestActionNotify_NoticeCarriesTriggerProject verifies a notify-action fire
// labels its trigger-notice bead with the firing trigger's project.
func TestActionNotify_NoticeCarriesTriggerProject(t *testing.T) {
	pub := &fakeNoticePublisher{}
	engine := NewTriggerEngine(TriggerEngineConfig{
		Daemon:   &mockTriggerDaemon{},
		Notifier: NewNoticeNotifier(pub, "trigger-engine", slog.Default()),
		Logger:   slog.Default(),
	})

	trig := TriggerDef{ID: "t1", Title: "Notify Trigger", Action: TriggerActionNotify, Project: "gardenio", Prompt: "hit {{key}}"}
	if err := engine.actionNotify(context.Background(), trig, TriggerMatch{Key: "K1", TemplateVars: map[string]string{"{{key}}": "K1"}}); err != nil {
		t.Fatalf("actionNotify: %v", err)
	}
	if got := noticeProjectLabels(pub.createReq); len(got) != 1 || got[0] != "project:gardenio" {
		t.Errorf("notice project labels = %v, want [project:gardenio] (labels %v)", got, pub.createReq.Labels)
	}
}

// TestNoticeBeadProject verifies a watchdog notice takes its bead's project
// field, else its project label, else nothing.
func TestNoticeBeadProject(t *testing.T) {
	cases := []struct {
		name string
		bead *beadsapi.BeadDetail
		want string
	}{
		{"nil bead", nil, ""},
		{"field wins", &beadsapi.BeadDetail{Fields: map[string]string{"project": "gardenio"}, Labels: []string{"project:other"}}, "gardenio"},
		{"label fallback", &beadsapi.BeadDetail{Labels: []string{"trigger", "project:other"}}, "other"},
		{"none", &beadsapi.BeadDetail{Labels: []string{"trigger"}}, ""},
	}
	for _, c := range cases {
		if got := noticeBeadProject(c.bead); got != c.want {
			t.Errorf("%s: noticeBeadProject = %q, want %q", c.name, got, c.want)
		}
	}
}
