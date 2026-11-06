package bridge

import (
	"context"
	"path/filepath"
	"testing"
)

func TestParseThreadAgentKey(t *testing.T) {
	tests := []struct {
		key    string
		wantCh string
		wantTS string
	}{
		{"C123:1111.2222", "C123", "1111.2222"},
		{"C-general:3333.4444", "C-general", "3333.4444"},
		{"", "", ""},
		{"nocolon", "", ""},
		{"C123:", "C123", ""},
		{":1111.2222", "", "1111.2222"},
	}

	for _, tt := range tests {
		ch, ts := parseThreadAgentKey(tt.key)
		if ch != tt.wantCh || ts != tt.wantTS {
			t.Errorf("parseThreadAgentKey(%q) = (%q, %q), want (%q, %q)",
				tt.key, ch, ts, tt.wantCh, tt.wantTS)
		}
	}
}

func TestValidateThreadBindings_NilState(t *testing.T) {
	daemon := newMockDaemon()
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()
	b := newTestBot(daemon, slackSrv)
	b.state = nil

	// Should not panic.
	b.validateThreadBindings(context.Background())
}

func TestValidateThreadBindings_NilAPI(t *testing.T) {
	dir := t.TempDir()
	state, err := NewStateManager(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}

	_ = state.SetThreadAgent("C-good", "1111.2222", "agent-a")
	_ = state.SetListenThread("C-good", "1111.2222")
	_ = state.SetThreadAgent("C-archived", "3333.4444", "agent-b")

	daemon := newMockDaemon()
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()
	b := newTestBot(daemon, slackSrv)
	b.state = state
	b.api = nil // nil API = skip validation

	b.validateThreadBindings(context.Background())

	// Both bindings should survive since API is nil.
	if _, ok := state.GetThreadAgent("C-good", "1111.2222"); !ok {
		t.Error("C-good binding should survive when API is nil")
	}
	if _, ok := state.GetThreadAgent("C-archived", "3333.4444"); !ok {
		t.Error("C-archived binding should survive when API is nil")
	}
}

func TestValidateThreadBindings_EmptyBindings(t *testing.T) {
	dir := t.TempDir()
	state, err := NewStateManager(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}

	daemon := newMockDaemon()
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()
	b := newTestBot(daemon, slackSrv)
	b.state = state

	// Should not panic with zero bindings.
	b.validateThreadBindings(context.Background())
}

func TestValidateThreadBindings_AccessibleThreadsSurvive(t *testing.T) {
	dir := t.TempDir()
	state, err := NewStateManager(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}

	_ = state.SetThreadAgent("C-test", "1111.2222", "agent-a")

	daemon := newMockDaemon()
	// The fake Slack server returns OK for all API calls,
	// so GetConversationReplies will succeed — threads are "accessible".
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()
	b := newTestBot(daemon, slackSrv)
	b.state = state

	b.validateThreadBindings(context.Background())

	// Thread should survive since the API returns success.
	if _, ok := state.GetThreadAgent("C-test", "1111.2222"); !ok {
		t.Error("accessible thread binding should survive validation")
	}
}
