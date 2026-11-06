package bridge

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"gardenio/controller/internal/beadsapi"

	slackapi "github.com/slack-go/slack"
)

func TestNormalizeModelID(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", ""},
		{"   ", ""},
		{"claude-opus-5", "claude-opus-5"},
		{"claude-opus-5-oauth", "claude-opus-5"},
		{"CLAUDE-OPUS-5-OAUTH", "claude-opus-5"},
		{"  claude-opus-5-oauth  ", "claude-opus-5"},
		{"claude-opus-4-8-oauth", "claude-opus-4-8"},
		{"claude-opus-4-8-20260315", "claude-opus-4-8"},
		{"claude-opus-4-8-2026-03-15", "claude-opus-4-8"},
		{"claude-opus-5-oauth-20260315", "claude-opus-5"}, // both suffixes, either order
		{"claude-opus-5-20260315-oauth", "claude-opus-5"},
		{"anthropic/claude-opus-5", "claude-opus-5"},
		{"us.anthropic.claude-opus-5", "claude-opus-5"},
		{"eu.anthropic.claude-opus-5", "claude-opus-5"},
		{"bedrock-claude-opus-5", "claude-opus-5"},
		{"bedrock/claude-opus-5", "claude-opus-5"},
		{"us.anthropic.claude-opus-5-v1:0", "claude-opus-5"},
		{"claude-sonnet-4-6-oauth", "claude-sonnet-4-6"},
	}
	for _, c := range cases {
		if got := normalizeModelID(c.in); got != c.want {
			t.Errorf("normalizeModelID(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestModelGroup(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", ""},
		{"claude-opus-5-oauth", "opus"},
		{"claude-opus-4-8", "opus"},
		{"us.anthropic.claude-opus-5", "opus"},
		{"claude-sonnet-4-6-oauth", "sonnet"},
		{"claude-haiku-4-5", "haiku"},
		{"claude-fable-5-oauth", "fable"},
		{"bedrock-glm-5", ""},
	}
	for _, c := range cases {
		if got := modelGroup(c.in); got != c.want {
			t.Errorf("modelGroup(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestModelsDiverge(t *testing.T) {
	cases := []struct {
		name             string
		expected, served string
		want             bool
	}{
		// Alias equivalence — must MATCH (no divergence).
		{"oauth suffix equivalence", "claude-opus-5-oauth", "claude-opus-5", false},
		{"oauth both sides", "claude-opus-5-oauth", "claude-opus-5-oauth", false},
		{"dated snapshot equivalence", "claude-opus-4-8-oauth", "claude-opus-4-8-20260315", false},
		{"bedrock profile equivalence", "claude-opus-5-oauth", "us.anthropic.claude-opus-5", false},
		{"identical", "claude-opus-5", "claude-opus-5", false},
		// Real divergences — must FLAG.
		{"cross-tier opus->sonnet", "claude-opus-5-oauth", "claude-sonnet-4-6", true},
		{"within-tier opus5->opus48 (configmap fallback target)", "claude-opus-5-oauth", "claude-opus-4-8", true},
		{"opus->haiku", "claude-opus-5-oauth", "claude-haiku-4-5", true},
		// Fail-open — insufficient signal, never flag.
		{"expected empty", "", "claude-opus-5", false},
		{"served empty", "claude-opus-5", "", false},
		{"both empty", "", "", false},
		{"served whitespace", "claude-opus-5", "   ", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := modelsDiverge(c.expected, c.served); got != c.want {
				t.Errorf("modelsDiverge(%q, %q) = %v, want %v", c.expected, c.served, got, c.want)
			}
		})
	}
}

// captureSlackAPI records every PostMessageContext call.
type captureSlackAPI struct {
	mu    sync.Mutex
	posts []capturePost
}

type capturePost struct {
	channel string
}

func (c *captureSlackAPI) PostMessageContext(_ context.Context, channelID string, _ ...slackapi.MsgOption) (string, string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.posts = append(c.posts, capturePost{channel: channelID})
	return "", "1700000000.000100", nil
}

func (c *captureSlackAPI) channels() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.posts))
	for i, p := range c.posts {
		out[i] = p.channel
	}
	return out
}

// fakeFallbackDaemon records CreateBead + AddDependency calls.
type fakeFallbackDaemon struct {
	mu        sync.Mutex
	created   []beadsapi.CreateBeadRequest
	deps      []string
	createID  string
	createErr error
}

func (d *fakeFallbackDaemon) CreateBead(_ context.Context, req beadsapi.CreateBeadRequest) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.createErr != nil {
		return "", d.createErr
	}
	d.created = append(d.created, req)
	id := d.createID
	if id == "" {
		id = "gd-record1"
	}
	return id, nil
}

func (d *fakeFallbackDaemon) AddDependency(_ context.Context, beadID, dependsOnID, depType, _ string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.deps = append(d.deps, beadID+"->"+dependsOnID+":"+depType)
	return nil
}

func TestModelFallbackFlagger_FlagsDivergence(t *testing.T) {
	api := &captureSlackAPI{}
	daemon := &fakeFallbackDaemon{createID: "gd-fb1"}
	f := NewModelFallbackFlagger(ModelFallbackConfig{
		API:            api,
		Daemon:         daemon,
		OpsChannel:     "C-ops",
		BeadsViewerURL: "https://viewer.example",
		Logger:         slog.Default(),
	})
	if f == nil {
		t.Fatal("NewModelFallbackFlagger returned nil")
	}

	evt := &messageEvent{
		EventType:     "usage",
		SessionID:     "sess-1",
		ExpectedModel: "claude-opus-5-oauth",
		Model:         "claude-opus-4-8",
		Project:       "gardenio",
		AgentBeadID:   "gd-agent1",
	}
	f.Flag(context.Background(), evt, "worker-1", "C-thread", "1700000000.111")

	// Slack: agent thread + ops channel.
	got := api.channels()
	if len(got) != 2 {
		t.Fatalf("Slack posts = %d, want 2 (thread + ops); channels=%v", len(got), got)
	}
	if got[0] != "C-thread" || got[1] != "C-ops" {
		t.Errorf("post channels = %v, want [C-thread C-ops]", got)
	}

	// Durable record.
	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	if len(daemon.created) != 1 {
		t.Fatalf("CreateBead calls = %d, want 1", len(daemon.created))
	}
	rec := daemon.created[0]
	if rec.Type != "rating" {
		t.Errorf("record type = %q, want rating", rec.Type)
	}
	if !hasLabel(rec.Labels, modelFallbackKindLabel) {
		t.Errorf("labels %v missing %q", rec.Labels, modelFallbackKindLabel)
	}
	if !hasLabel(rec.Labels, "project:gardenio") {
		t.Errorf("labels %v missing project:gardenio", rec.Labels)
	}
	if !hasLabel(rec.Labels, "agent:gd-agent1") {
		t.Errorf("labels %v missing agent:gd-agent1", rec.Labels)
	}
	var fields map[string]any
	if err := json.Unmarshal(rec.Fields, &fields); err != nil {
		t.Fatalf("unmarshal fields: %v", err)
	}
	if fields["source"] != modelFallbackSource {
		t.Errorf("source = %v, want %q", fields["source"], modelFallbackSource)
	}
	if fields["expected_model"] != "claude-opus-5-oauth" {
		t.Errorf("expected_model = %v", fields["expected_model"])
	}
	if fields["served_model"] != "claude-opus-4-8" {
		t.Errorf("served_model = %v", fields["served_model"])
	}
	if fields["expected_group"] != "opus" || fields["served_group"] != "opus" {
		t.Errorf("groups = %v/%v, want opus/opus", fields["expected_group"], fields["served_group"])
	}
	if fields["score"].(float64) != 0 {
		t.Errorf("score = %v, want 0", fields["score"])
	}
	// Linked to agent bead.
	if len(daemon.deps) != 1 || !strings.Contains(daemon.deps[0], "gd-fb1->gd-agent1:rates") {
		t.Errorf("deps = %v, want [gd-fb1->gd-agent1:rates]", daemon.deps)
	}
}

func TestModelFallbackFlagger_NoFlagOnMatch(t *testing.T) {
	api := &captureSlackAPI{}
	daemon := &fakeFallbackDaemon{}
	f := NewModelFallbackFlagger(ModelFallbackConfig{API: api, Daemon: daemon, Logger: slog.Default()})

	// Alias-equivalent: served -oauth-stripped equals expected → no flag.
	evt := &messageEvent{
		EventType:     "usage",
		SessionID:     "sess-1",
		ExpectedModel: "claude-opus-5-oauth",
		Model:         "claude-opus-5",
	}
	f.Flag(context.Background(), evt, "worker-1", "C-thread", "ts")

	if n := len(api.channels()); n != 0 {
		t.Errorf("Slack posts = %d, want 0 on alias-equivalent match", n)
	}
	if len(daemon.created) != 0 {
		t.Errorf("CreateBead calls = %d, want 0 on match", len(daemon.created))
	}
}

func TestModelFallbackFlagger_FailOpenOnMissingExpected(t *testing.T) {
	api := &captureSlackAPI{}
	daemon := &fakeFallbackDaemon{}
	f := NewModelFallbackFlagger(ModelFallbackConfig{API: api, Daemon: daemon, Logger: slog.Default()})

	// No expected_model (older coop / no baked model): must never flag.
	evt := &messageEvent{EventType: "usage", SessionID: "s", Model: "claude-sonnet-4-6"}
	f.Flag(context.Background(), evt, "worker-1", "C-thread", "ts")

	if n := len(api.channels()); n != 0 {
		t.Errorf("Slack posts = %d, want 0 when expected model unknown", n)
	}
	if len(daemon.created) != 0 {
		t.Errorf("CreateBead calls = %d, want 0 when expected model unknown", len(daemon.created))
	}
}

func TestModelFallbackFlagger_DedupePerSession(t *testing.T) {
	api := &captureSlackAPI{}
	daemon := &fakeFallbackDaemon{}
	f := NewModelFallbackFlagger(ModelFallbackConfig{API: api, Daemon: daemon, OpsChannel: "C-ops", Logger: slog.Default()})

	evt := &messageEvent{
		EventType:     "usage",
		SessionID:     "sess-1",
		ExpectedModel: "claude-opus-5-oauth",
		Model:         "claude-sonnet-4-6",
	}
	// Same divergence fires across three usage turns → flag once.
	f.Flag(context.Background(), evt, "worker-1", "C-thread", "ts")
	f.Flag(context.Background(), evt, "worker-1", "C-thread", "ts")
	f.Flag(context.Background(), evt, "worker-1", "C-thread", "ts")

	if len(daemon.created) != 1 {
		t.Errorf("CreateBead calls = %d, want 1 (deduped per session)", len(daemon.created))
	}
	// 2 posts for the ONE flag (thread + ops), not 6.
	if n := len(api.channels()); n != 2 {
		t.Errorf("Slack posts = %d, want 2 (deduped per session)", n)
	}

	// A genuinely NEW divergence later in the same session still flags.
	evt2 := &messageEvent{
		EventType:     "usage",
		SessionID:     "sess-1",
		ExpectedModel: "claude-opus-5-oauth",
		Model:         "claude-haiku-4-5",
	}
	f.Flag(context.Background(), evt2, "worker-1", "C-thread", "ts")
	if len(daemon.created) != 2 {
		t.Errorf("CreateBead calls = %d, want 2 (new divergence flags)", len(daemon.created))
	}
}

func TestModelFallbackFlagger_OpsOnlyWhenNoThread(t *testing.T) {
	api := &captureSlackAPI{}
	f := NewModelFallbackFlagger(ModelFallbackConfig{API: api, OpsChannel: "C-ops", Logger: slog.Default()})

	evt := &messageEvent{
		EventType:     "usage",
		SessionID:     "s",
		ExpectedModel: "claude-opus-5-oauth",
		Model:         "claude-sonnet-4-6",
	}
	// No thread channel → only the ops channel is posted.
	f.Flag(context.Background(), evt, "worker-1", "", "")
	got := api.channels()
	if len(got) != 1 || got[0] != "C-ops" {
		t.Errorf("channels = %v, want [C-ops]", got)
	}
}

func TestModelFallbackFlagger_ThreadEqualsOpsPostsOnce(t *testing.T) {
	api := &captureSlackAPI{}
	f := NewModelFallbackFlagger(ModelFallbackConfig{API: api, OpsChannel: "C-same", Logger: slog.Default()})

	evt := &messageEvent{
		EventType:     "usage",
		SessionID:     "s",
		ExpectedModel: "claude-opus-5-oauth",
		Model:         "claude-sonnet-4-6",
	}
	// Agent thread lives in the same channel as ops → post once, not twice.
	f.Flag(context.Background(), evt, "worker-1", "C-same", "ts")
	if n := len(api.channels()); n != 1 {
		t.Errorf("Slack posts = %d, want 1 when thread == ops channel", n)
	}
}

// TestUsageHandlerFlagsFallback verifies the AgentStreamManager usage handler
// routes a divergent usage event to the flagger with the session's thread anchor.
func TestUsageHandlerFlagsFallback(t *testing.T) {
	mock := &mockStreamClient{}
	mgr := NewAgentStreamManager(mock, testLogger())
	ctx := context.Background()

	api := &captureSlackAPI{}
	daemon := &fakeFallbackDaemon{createID: "gd-rec"}
	mgr.SetModelFallbackFlagger(NewModelFallbackFlagger(ModelFallbackConfig{
		API:        api,
		Daemon:     daemon,
		OpsChannel: "C-ops",
		Logger:     testLogger(),
	}))

	mgr.StartSession(ctx, "worker-1", "C-thread", "thread-ts-1", "U1")

	// Divergent usage turn.
	mgr.HandleMessageEvent("worker-1", &messageEvent{
		EventType:     "usage",
		SessionID:     "sess-1",
		InputTokens:   1000,
		OutputTokens:  500,
		ExpectedModel: "claude-opus-5-oauth",
		Model:         "claude-sonnet-4-6",
		AgentBeadID:   "gd-agent1",
		Project:       "gardenio",
	})

	if len(daemon.created) != 1 {
		t.Fatalf("CreateBead calls = %d, want 1", len(daemon.created))
	}
	// Flag went to the session's thread channel + ops.
	got := api.channels()
	if len(got) != 2 || got[0] != "C-thread" || got[1] != "C-ops" {
		t.Errorf("flag channels = %v, want [C-thread C-ops]", got)
	}

	// A matching (alias-equivalent) turn must NOT flag.
	mgr.HandleMessageEvent("worker-1", &messageEvent{
		EventType:     "usage",
		SessionID:     "sess-1",
		InputTokens:   10,
		OutputTokens:  5,
		ExpectedModel: "claude-opus-5-oauth",
		Model:         "claude-opus-5",
	})
	if len(daemon.created) != 1 {
		t.Errorf("CreateBead calls = %d after alias-equivalent turn, want 1", len(daemon.created))
	}
}

func TestNewModelFallbackFlagger_NilWhenUnconfigured(t *testing.T) {
	if f := NewModelFallbackFlagger(ModelFallbackConfig{Logger: slog.Default()}); f != nil {
		t.Error("expected nil flagger when neither API nor Daemon set")
	}
	// A nil flagger's Flag is a safe no-op.
	var nilF *ModelFallbackFlagger
	nilF.Flag(context.Background(), &messageEvent{ExpectedModel: "a", Model: "b"}, "w", "c", "t")
}
