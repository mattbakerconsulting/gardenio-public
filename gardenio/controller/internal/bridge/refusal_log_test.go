package bridge

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

func TestRefusalLogger_LogsRefusal(t *testing.T) {
	api := &captureSlackAPI{}
	daemon := &fakeFallbackDaemon{createID: "gd-ref1"}
	f := NewRefusalLogger(RefusalLogConfig{
		API:            api,
		Daemon:         daemon,
		OpsChannel:     "C-ops",
		BeadsViewerURL: "https://viewer.example",
		Logger:         slog.Default(),
	})
	if f == nil {
		t.Fatal("NewRefusalLogger returned nil")
	}

	evt := &messageEvent{
		EventType:      "refusal",
		SessionID:      "sess-1",
		StopReason:     "refusal",
		Model:          "claude-opus-5",
		RequestedModel: "claude-opus-5",
		Text:           "I can't help with that request.",
		FullLength:     31,
		Project:        "gardenio",
		AgentBeadID:    "gd-agent1",
	}
	f.Log(context.Background(), evt, "worker-1", "C-thread", "1700000000.111")

	// Slack: agent thread + ops channel.
	got := api.channels()
	if len(got) != 2 {
		t.Fatalf("Slack posts = %d, want 2 (thread + ops); channels=%v", len(got), got)
	}
	if got[0] != "C-thread" || got[1] != "C-ops" {
		t.Errorf("post channels = %v, want [C-thread C-ops]", got)
	}

	// Durable record on the surveys/ratings surface.
	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	if len(daemon.created) != 1 {
		t.Fatalf("CreateBead calls = %d, want 1", len(daemon.created))
	}
	rec := daemon.created[0]
	if rec.Type != "rating" {
		t.Errorf("record type = %q, want rating", rec.Type)
	}
	if !hasLabel(rec.Labels, refusalKindLabel) {
		t.Errorf("labels %v missing %q", rec.Labels, refusalKindLabel)
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
	if fields["source"] != refusalSource {
		t.Errorf("source = %v, want %q", fields["source"], refusalSource)
	}
	if fields["stop_reason"] != "refusal" {
		t.Errorf("stop_reason = %v, want refusal", fields["stop_reason"])
	}
	if fields["served_model"] != "claude-opus-5" {
		t.Errorf("served_model = %v", fields["served_model"])
	}
	if fields["feedback"] != "I can't help with that request." {
		t.Errorf("feedback = %v", fields["feedback"])
	}
	if fields["score"].(float64) != 0 {
		t.Errorf("score = %v, want 0", fields["score"])
	}
	// No fallback: served == requested, so served_by_fallback must be absent.
	if _, ok := fields["served_by_fallback"]; ok {
		t.Errorf("served_by_fallback set on a non-fallback refusal: %v", fields["served_by_fallback"])
	}
	// Linked to agent bead.
	if len(daemon.deps) != 1 || !strings.Contains(daemon.deps[0], "gd-ref1->gd-agent1:rates") {
		t.Errorf("deps = %v, want [gd-ref1->gd-agent1:rates]", daemon.deps)
	}
}

func TestRefusalLogger_FlagsSilentFallbackRefusal(t *testing.T) {
	api := &captureSlackAPI{}
	daemon := &fakeFallbackDaemon{createID: "gd-ref2"}
	f := NewRefusalLogger(RefusalLogConfig{API: api, Daemon: daemon, Logger: slog.Default()})

	// A refusal answered by a DIFFERENT served model than requested → mark it.
	evt := &messageEvent{
		EventType:      "refusal",
		SessionID:      "s",
		StopReason:     "refusal",
		Model:          "claude-opus-4-8",
		RequestedModel: "claude-opus-5-oauth",
		Text:           "refused",
	}
	f.Log(context.Background(), evt, "worker-1", "C-thread", "ts")

	if len(daemon.created) != 1 {
		t.Fatalf("CreateBead calls = %d, want 1", len(daemon.created))
	}
	var fields map[string]any
	if err := json.Unmarshal(daemon.created[0].Fields, &fields); err != nil {
		t.Fatalf("unmarshal fields: %v", err)
	}
	if fields["served_by_fallback"] != true {
		t.Errorf("served_by_fallback = %v, want true (opus-5 requested, opus-4-8 served)", fields["served_by_fallback"])
	}
}

func TestRefusalLogger_IgnoresNonRefusal(t *testing.T) {
	api := &captureSlackAPI{}
	daemon := &fakeFallbackDaemon{}
	f := NewRefusalLogger(RefusalLogConfig{API: api, Daemon: daemon, Logger: slog.Default()})

	// StopReason not "refusal" (defensive guard) → no record, no flag.
	evt := &messageEvent{EventType: "refusal", SessionID: "s", StopReason: "end_turn", Text: "done"}
	f.Log(context.Background(), evt, "worker-1", "C-thread", "ts")

	if n := len(api.channels()); n != 0 {
		t.Errorf("Slack posts = %d, want 0 on non-refusal stop_reason", n)
	}
	if len(daemon.created) != 0 {
		t.Errorf("CreateBead calls = %d, want 0 on non-refusal stop_reason", len(daemon.created))
	}
}

func TestRefusalLogger_DedupePerSessionSnippet(t *testing.T) {
	api := &captureSlackAPI{}
	daemon := &fakeFallbackDaemon{}
	f := NewRefusalLogger(RefusalLogConfig{API: api, Daemon: daemon, OpsChannel: "C-ops", Logger: slog.Default()})

	evt := &messageEvent{
		EventType:  "refusal",
		SessionID:  "sess-1",
		StopReason: "refusal",
		Model:      "claude-opus-5",
		Text:       "I can't do that.",
	}
	// Same refusal across three turns of one session → log once.
	f.Log(context.Background(), evt, "worker-1", "C-thread", "ts")
	f.Log(context.Background(), evt, "worker-1", "C-thread", "ts")
	f.Log(context.Background(), evt, "worker-1", "C-thread", "ts")

	if len(daemon.created) != 1 {
		t.Errorf("CreateBead calls = %d, want 1 (deduped per session+snippet)", len(daemon.created))
	}
	if n := len(api.channels()); n != 2 {
		t.Errorf("Slack posts = %d, want 2 (thread + ops for the one log)", n)
	}

	// A DIFFERENT refusal message later in the same session still logs.
	evt2 := &messageEvent{
		EventType:  "refusal",
		SessionID:  "sess-1",
		StopReason: "refusal",
		Model:      "claude-opus-5",
		Text:       "That violates policy.",
	}
	f.Log(context.Background(), evt2, "worker-1", "C-thread", "ts")
	if len(daemon.created) != 2 {
		t.Errorf("CreateBead calls = %d, want 2 (new refusal snippet logs)", len(daemon.created))
	}
}

func TestRefusalLogger_OpsOnlyWhenNoThread(t *testing.T) {
	api := &captureSlackAPI{}
	f := NewRefusalLogger(RefusalLogConfig{API: api, OpsChannel: "C-ops", Logger: slog.Default()})

	evt := &messageEvent{EventType: "refusal", SessionID: "s", StopReason: "refusal", Model: "claude-opus-5", Text: "no"}
	// No thread channel → only the ops channel is posted.
	f.Log(context.Background(), evt, "worker-1", "", "")
	got := api.channels()
	if len(got) != 1 || got[0] != "C-ops" {
		t.Errorf("channels = %v, want [C-ops]", got)
	}
}

func TestRefusalLogger_RecordWithoutModels(t *testing.T) {
	daemon := &fakeFallbackDaemon{createID: "gd-ref3"}
	f := NewRefusalLogger(RefusalLogConfig{Daemon: daemon, Logger: slog.Default()})

	// A refusal with no model metadata (older coop) still writes a record; the
	// model fields are simply omitted, not written empty.
	evt := &messageEvent{EventType: "refusal", SessionID: "s", StopReason: "refusal", Text: "denied", AgentBeadID: "gd-a"}
	f.Log(context.Background(), evt, "worker-1", "", "")

	if len(daemon.created) != 1 {
		t.Fatalf("CreateBead calls = %d, want 1", len(daemon.created))
	}
	var fields map[string]any
	if err := json.Unmarshal(daemon.created[0].Fields, &fields); err != nil {
		t.Fatalf("unmarshal fields: %v", err)
	}
	if _, ok := fields["served_model"]; ok {
		t.Errorf("served_model set with no model in event: %v", fields["served_model"])
	}
	if _, ok := fields["requested_model"]; ok {
		t.Errorf("requested_model set with no requested model in event: %v", fields["requested_model"])
	}
	if fields["stop_reason"] != "refusal" {
		t.Errorf("stop_reason = %v, want refusal", fields["stop_reason"])
	}
}

// TestRefusalHandlerRoutesToLogger verifies the AgentStreamManager refusal handler
// routes a refusal event to the logger with the session's thread anchor.
func TestRefusalHandlerRoutesToLogger(t *testing.T) {
	mock := &mockStreamClient{}
	mgr := NewAgentStreamManager(mock, testLogger())
	ctx := context.Background()

	api := &captureSlackAPI{}
	daemon := &fakeFallbackDaemon{createID: "gd-rec"}
	mgr.SetRefusalLogger(NewRefusalLogger(RefusalLogConfig{
		API:        api,
		Daemon:     daemon,
		OpsChannel: "C-ops",
		Logger:     testLogger(),
	}))

	mgr.StartSession(ctx, "worker-1", "C-thread", "thread-ts-1", "U1")

	mgr.HandleMessageEvent("worker-1", &messageEvent{
		EventType:   "refusal",
		SessionID:   "sess-1",
		StopReason:  "refusal",
		Model:       "claude-opus-5",
		Text:        "I won't do that.",
		AgentBeadID: "gd-agent1",
		Project:     "gardenio",
	})

	if len(daemon.created) != 1 {
		t.Fatalf("CreateBead calls = %d, want 1", len(daemon.created))
	}
	// Flag went to the session's thread channel + ops.
	got := api.channels()
	if len(got) != 2 || got[0] != "C-thread" || got[1] != "C-ops" {
		t.Errorf("flag channels = %v, want [C-thread C-ops]", got)
	}
}

func TestNewRefusalLogger_NilWhenUnconfigured(t *testing.T) {
	if f := NewRefusalLogger(RefusalLogConfig{Logger: slog.Default()}); f != nil {
		t.Error("expected nil logger when neither API nor Daemon set")
	}
	// A nil logger's Log is a safe no-op.
	var nilF *RefusalLogger
	nilF.Log(context.Background(), &messageEvent{StopReason: "refusal"}, "w", "c", "t")
}
