package bridge

import (
	"context"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// seedChannelBead adds a type=channel bead bound to a Slack channel in land.
func seedChannelBead(daemon *mockDaemon, id, land, slackChannel string) {
	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	daemon.beads[id] = &beadsapi.BeadDetail{
		ID:     id,
		Title:  "general",
		Type:   "channel",
		Status: "open",
		Labels: []string{"project:" + land},
		Fields: map[string]string{
			"name":             "general",
			"channel_kind":     "general",
			"slack_channel_id": slackChannel,
		},
	}
}

// seedLandDefaultChannel adds land's project bead naming channelID as the
// channel the land opens on.
func seedLandDefaultChannel(daemon *mockDaemon, land, channelID string) {
	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	daemon.beads["proj-"+land] = &beadsapi.BeadDetail{
		ID: "proj-" + land, Title: land, Type: "project", Status: "open",
		Fields: map[string]string{"land_default_channel": channelID},
	}
}

func beadFields(daemon *mockDaemon, id string) (map[string]string, []string) {
	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	b := daemon.beads[id]
	if b == nil {
		return nil, nil
	}
	return maps.Clone(b.Fields), slices.Clone(b.Labels)
}

func TestChannelBeadRefStampFor(t *testing.T) {
	ref := channelBeadRef{ID: "ch-1", Land: "gardenio"}
	cases := []struct {
		name      string
		labels    []string
		wantLabel string
		wantOK    bool
	}{
		{"no land adds the channel's", []string{"squawk"}, "project:gardenio", true},
		{"same land adds nothing", []string{"project:gardenio", "thread"}, "", true},
		{"duplicate same land", []string{"project:gardenio", "project:gardenio"}, "", true},
		{"other land refused", []string{"project:other"}, "", false},
		{"two lands refused", []string{"project:gardenio", "project:other"}, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ref.stampFor(tc.labels)
			if got != tc.wantLabel || ok != tc.wantOK {
				t.Fatalf("stampFor(%v) = (%q, %v), want (%q, %v)", tc.labels, got, ok, tc.wantLabel, tc.wantOK)
			}
		})
	}
}

// countingDaemon counts channel-bead lookups and can refuse field updates.
type countingDaemon struct {
	*mockDaemon
	channelLookups int
	refuseUpdate   map[string]bool
	updates        int
}

func (c *countingDaemon) ListBeadsFiltered(ctx context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	if slices.Contains(q.Types, "channel") {
		c.channelLookups++
	}
	return c.mockDaemon.ListBeadsFiltered(ctx, q)
}

func (c *countingDaemon) UpdateBeadFields(ctx context.Context, id string, fields map[string]string) error {
	c.updates++
	if c.refuseUpdate[id] {
		return &beadsapi.APIError{StatusCode: http.StatusForbidden, Message: "ownership denied"}
	}
	return c.mockDaemon.UpdateBeadFields(ctx, id, fields)
}

func TestResolveChannelBead_CachesHitsAndMisses(t *testing.T) {
	mock := newMockDaemon()
	seedChannelBead(mock, "ch-1", "gardenio", "C-BOUND")
	d := &countingDaemon{mockDaemon: mock}
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()
	b := newTestBot(d, slackSrv)

	for range 3 {
		ref, ok := b.resolveChannelBead(context.Background(), "C-BOUND")
		if !ok || ref != (channelBeadRef{ID: "ch-1", Land: "gardenio"}) {
			t.Fatalf("resolveChannelBead(C-BOUND) = (%+v, %v)", ref, ok)
		}
		if _, ok := b.resolveChannelBead(context.Background(), "C-UNBOUND"); ok {
			t.Fatal("an unbound Slack channel must not resolve")
		}
	}
	if d.channelLookups != 2 {
		t.Fatalf("channel lookups = %d, want 2 (one per Slack channel, misses cached too)", d.channelLookups)
	}
	if _, ok := b.resolveChannelBead(context.Background(), ""); ok {
		t.Fatal("an empty Slack channel must not resolve")
	}
}

func TestGetOrCreateThreadBead_StampsChannelID(t *testing.T) {
	mock := newMockDaemon()
	seedChannelBead(mock, "ch-1", "gardenio", "C-BOUND")
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()
	b := newTestBot(mock, slackSrv)
	ctx := context.Background()

	id, _, err := b.getOrCreateThreadBead(ctx, "gardenio", threadBeadFields{Channel: "C-BOUND", ThreadTS: "100.1"})
	if err != nil {
		t.Fatal(err)
	}
	fields, labels := beadFields(mock, id)
	if fields["channel_id"] != "ch-1" {
		t.Errorf("head channel_id = %q, want ch-1", fields["channel_id"])
	}
	if fields["slack_thread_channel"] != "C-BOUND" {
		t.Errorf("slack_thread_channel must still be written (dual-write), got %q", fields["slack_thread_channel"])
	}
	if !slices.Equal(labelLands(labels), []string{"gardenio"}) {
		t.Errorf("head lands = %v, want [gardenio]", labelLands(labels))
	}

	// No project: stamped, and the head gains no project label.
	id, _, err = b.getOrCreateThreadBead(ctx, "", threadBeadFields{Channel: "C-BOUND", ThreadTS: "100.2"})
	if err != nil {
		t.Fatal(err)
	}
	fields, labels = beadFields(mock, id)
	if fields["channel_id"] != "ch-1" {
		t.Errorf("unlabelled head channel_id = %q, want ch-1", fields["channel_id"])
	}
	if len(labelLands(labels)) != 0 {
		t.Errorf("a head must never gain a project label, got %v", labels)
	}

	// Another land: not stamped.
	id, _, err = b.getOrCreateThreadBead(ctx, "other", threadBeadFields{Channel: "C-BOUND", ThreadTS: "100.3"})
	if err != nil {
		t.Fatal(err)
	}
	if fields, _ = beadFields(mock, id); fields["channel_id"] != "" {
		t.Errorf("head in another land must not be stamped, got channel_id=%q", fields["channel_id"])
	}

	// Unbound Slack channel: not stamped.
	id, _, err = b.getOrCreateThreadBead(ctx, "gardenio", threadBeadFields{Channel: "C-UNBOUND", ThreadTS: "100.4"})
	if err != nil {
		t.Fatal(err)
	}
	if fields, _ = beadFields(mock, id); fields["channel_id"] != "" {
		t.Errorf("head in an unbound channel of a land with no default channel must not be stamped, got channel_id=%q", fields["channel_id"])
	}
}

// A head said where no channel bead is bound (a DM with the bot, a role
// channel) goes to its land's default channel, not to no channel: before, its
// conversation surfaced in the Commons instead of the land (gd-cI0IZN41aW's
// sibling report, owner 2026-11-03).
func TestGetOrCreateThreadBead_UnboundChannelUsesLandDefault(t *testing.T) {
	mock := newMockDaemon()
	seedChannelBead(mock, "ch-other", "other", "C-OTHER")
	seedLandDefaultChannel(mock, "gardenio", "ch-general")
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()
	b := newTestBot(mock, slackSrv)
	ctx := context.Background()

	id, _, err := b.getOrCreateThreadBead(ctx, "gardenio", threadBeadFields{Channel: "D-DM", ThreadTS: "200.1"})
	if err != nil {
		t.Fatal(err)
	}
	fields, labels := beadFields(mock, id)
	if fields["channel_id"] != "ch-general" {
		t.Errorf("DM head channel_id = %q, want the land default ch-general", fields["channel_id"])
	}
	if fields["slack_thread_channel"] != "D-DM" {
		t.Errorf("slack_thread_channel = %q, want D-DM kept", fields["slack_thread_channel"])
	}
	if !slices.Equal(labelLands(labels), []string{"gardenio"}) {
		t.Errorf("head lands = %v, want [gardenio]", labelLands(labels))
	}

	// No project: no land to default to, so no stamp.
	id, _, err = b.getOrCreateThreadBead(ctx, "", threadBeadFields{Channel: "D-DM", ThreadTS: "200.2"})
	if err != nil {
		t.Fatal(err)
	}
	if fields, _ = beadFields(mock, id); fields["channel_id"] != "" {
		t.Errorf("a landless head must not be stamped, got channel_id=%q", fields["channel_id"])
	}

	// Bound to another land's channel: the binding wins and refuses, no fallback.
	id, _, err = b.getOrCreateThreadBead(ctx, "gardenio", threadBeadFields{Channel: "C-OTHER", ThreadTS: "200.3"})
	if err != nil {
		t.Fatal(err)
	}
	if fields, _ = beadFields(mock, id); fields["channel_id"] != "" {
		t.Errorf("a head in another land's bound channel must not fall back, got channel_id=%q", fields["channel_id"])
	}
}

// A failed binding lookup is not read as "unbound": the head is left for the
// backfill rather than filed under the default of a channel that may be bound.
func TestHeadChannelFor_LookupFailureDoesNotFallBack(t *testing.T) {
	mock := newMockDaemon()
	seedLandDefaultChannel(mock, "gardenio", "ch-general")
	d := &failingChannelLookupDaemon{mockDaemon: mock}
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()
	b := newTestBot(d, slackSrv)

	if got := b.headChannelFor(context.Background(), "C-ANY", []string{"project:gardenio"}); got != "" {
		t.Fatalf("headChannelFor after a failed lookup = %q, want \"\"", got)
	}
}

type failingChannelLookupDaemon struct{ *mockDaemon }

func (f *failingChannelLookupDaemon) ListBeadsFiltered(ctx context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	if slices.Contains(q.Types, "channel") {
		return nil, &beadsapi.APIError{StatusCode: http.StatusServiceUnavailable, Message: "down"}
	}
	return f.mockDaemon.ListBeadsFiltered(ctx, q)
}

func TestWriteThreadConversationBead_StampsChannelAndLand(t *testing.T) {
	mock := newMockDaemon()
	seedChannelBead(mock, "ch-1", "gardenio", "C-BOUND")
	seedThreadHead(mock, "C-BOUND", "200.1")
	b := newDLQTestBot(t, mock)

	msgID, _, err := b.writeThreadConversationBead(context.Background(),
		replyInput("C-BOUND", "200.1", "200.2", "hello"), "hello", nil)
	if err != nil {
		t.Fatal(err)
	}
	fields, labels := beadFields(mock, msgID)
	if fields["channel_id"] != "ch-1" {
		t.Errorf("message channel_id = %q, want ch-1", fields["channel_id"])
	}
	if !slices.Equal(labelLands(labels), []string{"gardenio"}) {
		t.Errorf("a message naming a channel needs exactly its land label, got %v", labels)
	}
	if fields["slack_thread_channel"] != "C-BOUND" || fields["thread_id"] == "" {
		t.Errorf("existing fields must be unchanged, got %v", fields)
	}
}

func TestWriteThreadConversationBead_UnboundChannelUnchanged(t *testing.T) {
	mock := newMockDaemon()
	seedThreadHead(mock, "C-UNBOUND", "300.1")
	b := newDLQTestBot(t, mock)

	msgID, _, err := b.writeThreadConversationBead(context.Background(),
		replyInput("C-UNBOUND", "300.1", "300.2", "hello"), "hello", nil)
	if err != nil {
		t.Fatal(err)
	}
	fields, labels := beadFields(mock, msgID)
	if _, ok := fields["channel_id"]; ok {
		t.Errorf("unbound channel must not stamp channel_id, got %v", fields)
	}
	if len(labelLands(labels)) != 0 {
		t.Errorf("unbound channel must not add a land label, got %v", labels)
	}
}

// A daemon refusal of the stamped create (the binding changed under the cache)
// must not cost the human's reply: it is written again without the stamp.
func TestWriteThreadConversationBead_RefusedStampFallsBack(t *testing.T) {
	mock := newMockDaemon()
	seedChannelBead(mock, "ch-1", "gardenio", "C-BOUND")
	seedThreadHead(mock, "C-BOUND", "400.1")
	b := newDLQTestBot(t, mock)

	refused := 0
	mock.createBeadHook = func(_ int, req beadsapi.CreateBeadRequest) error {
		if strings.Contains(string(req.Fields), `"channel_id"`) {
			refused++
			return &beadsapi.APIError{StatusCode: http.StatusBadRequest, Message: "channel_id is not a channel bead"}
		}
		return nil
	}

	msgID, _, err := b.writeThreadConversationBead(context.Background(),
		replyInput("C-BOUND", "400.1", "400.2", "keep me"), "keep me", nil)
	if err != nil {
		t.Fatalf("refused stamp must fall back, got %v", err)
	}
	if refused != 1 {
		t.Errorf("stamped creates = %d, want 1 (a 4xx is not retried)", refused)
	}
	fields, labels := beadFields(mock, msgID)
	if fields["text"] != "keep me" {
		t.Errorf("fallback message text = %q", fields["text"])
	}
	if _, ok := fields["channel_id"]; ok || len(labelLands(labels)) != 0 {
		t.Errorf("fallback must be the unstamped message, got fields=%v labels=%v", fields, labels)
	}
}

func TestBackfillThreadHeadChannels(t *testing.T) {
	mock := newMockDaemon()
	seedChannelBead(mock, "ch-1", "gardenio", "C-BOUND")
	head := func(id, status, slackChannel string, labels []string, fields map[string]string) {
		f := map[string]string{"slack_thread_channel": slackChannel, "slack_thread_ts": "1.1"}
		for k, v := range fields {
			f[k] = v
		}
		mock.beads[id] = &beadsapi.BeadDetail{
			ID: id, Type: "task", Status: status,
			Labels: append([]string{threadKindLabel}, labels...), Fields: f,
		}
	}
	head("h-open", "open", "C-BOUND", []string{"project:gardenio"}, nil)
	head("h-progress", "in_progress", "C-BOUND", nil, nil)
	head("h-closed", "closed", "C-BOUND", []string{"project:gardenio"}, nil)
	head("h-other-land", "open", "C-BOUND", []string{"project:other"}, nil)
	head("h-unbound", "open", "C-UNBOUND", nil, nil)
	head("h-stamped", "open", "C-BOUND", nil, map[string]string{"channel_id": "ch-old"})
	head("h-refused", "open", "C-BOUND", nil, nil)
	head("h-dm", "open", "D-DM", []string{"project:gardenio"}, nil)
	seedLandDefaultChannel(mock, "gardenio", "ch-general")

	d := &countingDaemon{mockDaemon: mock, refuseUpdate: map[string]bool{"h-refused": true}}
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()
	b := newTestBot(d, slackSrv)

	if got := b.backfillThreadHeadChannels(context.Background()); got != 3 {
		t.Fatalf("stamped = %d, want 3 (h-open, h-progress, h-dm)", got)
	}
	want := map[string]string{
		"h-open": "ch-1", "h-progress": "ch-1", "h-closed": "", "h-other-land": "",
		"h-unbound": "", "h-stamped": "ch-old", "h-refused": "", "h-dm": "ch-general",
	}
	for id, ch := range want {
		fields, labels := beadFields(mock, id)
		if fields["channel_id"] != ch {
			t.Errorf("%s channel_id = %q, want %q", id, fields["channel_id"], ch)
		}
		if id == "h-progress" && len(labelLands(labels)) != 0 {
			t.Errorf("backfill must never add a project label to a head, got %v", labels)
		}
	}

	// A refused head is not retried on the next sweep.
	before := d.updates
	if got := b.backfillThreadHeadChannels(context.Background()); got != 0 {
		t.Fatalf("second sweep stamped %d, want 0", got)
	}
	if d.updates != before {
		t.Errorf("second sweep made %d updates, want 0 (refused head skipped, rest already stamped)", d.updates-before)
	}
}

// A stamped write that keeps failing (here: the unique-index race a DLQ replay
// hits when the reply first landed unstamped) is retried unstamped, which the
// daemon resolves against the existing bead.
func TestWriteThreadConversationBead_FailingStampedWriteFallsBack(t *testing.T) {
	mock := newMockDaemon()
	seedChannelBead(mock, "ch-1", "gardenio", "C-BOUND")
	seedThreadHead(mock, "C-BOUND", "500.1")
	b := newDLQTestBot(t, mock)

	mock.createBeadHook = func(_ int, req beadsapi.CreateBeadRequest) error {
		if strings.Contains(string(req.Fields), `"channel_id"`) {
			return &beadsapi.APIError{StatusCode: http.StatusInternalServerError, Message: "duplicate key value violates unique constraint"}
		}
		return nil
	}

	msgID, _, err := b.writeThreadConversationBead(context.Background(),
		replyInput("C-BOUND", "500.1", "500.2", "replayed"), "replayed", nil)
	if err != nil || msgID == "" {
		t.Fatalf("failing stamped write must fall back, got id=%q err=%v", msgID, err)
	}
	if fields, _ := beadFields(mock, msgID); fields["channel_id"] != "" {
		t.Errorf("fallback must be unstamped, got channel_id=%q", fields["channel_id"])
	}
}
