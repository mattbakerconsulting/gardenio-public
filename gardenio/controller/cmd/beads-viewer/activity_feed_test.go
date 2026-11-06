package main

import (
	"testing"
	"time"

	"github.com/nats-io/nats.go"
)

func TestNewActivityFeed_NilWhenUnconfigured(t *testing.T) {
	if NewActivityFeed("", "", "", func([]byte) {}, nil) != nil {
		t.Error("no NATS URL: want a nil feed")
	}
	if NewActivityFeed("nats://x:4222", "", "", nil, nil) != nil {
		t.Error("no sink: want a nil feed")
	}
	var f *ActivityFeed
	if err := f.Start(); err != nil {
		t.Errorf("nil Start: %v", err)
	}
	f.Close()
}

// The feed hears every agent's tool and subagent hooks on core NATS and nothing else (gd-H4jk9KgUAG).
func TestActivityFeed_EndToEndNATS(t *testing.T) {
	url := startEmbeddedNATS(t)
	got := make(chan string, 16)
	f := NewActivityFeed(url, "", "", func(b []byte) { got <- string(b) }, nil)
	if err := f.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer f.Close()

	pub, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("publisher connect: %v", err)
	}
	defer pub.Close()
	for _, s := range []string{
		"hooks.agent-a.Stop", "hooks.agent-a.UserPromptSubmit", "coop.events.messages.agent-a.thinking", // not read
		"hooks.agent-a.PreToolUse", "hooks.agent-b.PostToolUse", "hooks.agent-b.PostToolUseFailure",
		"hooks.agent-a.SubagentStart", "hooks.agent-a.SubagentStop",
	} {
		if err := pub.Publish(s, []byte(s)); err != nil {
			t.Fatalf("publish %s: %v", s, err)
		}
	}
	_ = pub.Flush()

	want := map[string]bool{"hooks.agent-a.PreToolUse": true, "hooks.agent-b.PostToolUse": true, "hooks.agent-b.PostToolUseFailure": true, "hooks.agent-a.SubagentStart": true, "hooks.agent-a.SubagentStop": true}
	deadline := time.After(5 * time.Second)
	for len(want) > 0 {
		select {
		case s := <-got:
			if !want[s] {
				t.Fatalf("feed heard %q", s)
			}
			delete(want, s)
		case <-deadline:
			t.Fatalf("not heard: %v", want)
		}
	}
	select {
	case s := <-got:
		t.Errorf("feed heard %q", s)
	case <-time.After(200 * time.Millisecond):
	}
}

// A live thought names its agent bead to OnThought; one without an agent bead does not.
func TestThinkingHub_OnThought(t *testing.T) {
	h := newTestHub()
	var heard []string
	h.OnThought(func(id string) { heard = append(heard, id) })
	h.handleMessage(&nats.Msg{Subject: "coop.events.messages.a.thinking", Data: []byte(`{"actor":"a","text":"hm","agent_bead_id":"gd-a"}`)})
	h.handleMessage(&nats.Msg{Subject: "coop.events.messages.b.thinking", Data: []byte(`{"actor":"b","text":"hm"}`)})
	h.handleMessage(&nats.Msg{Subject: "coop.events.messages.c.thinking", Data: []byte(`{"actor":"c","text":"","agent_bead_id":"gd-c"}`)})
	if len(heard) != 1 || heard[0] != "gd-a" {
		t.Errorf("heard %v, want [gd-a]", heard)
	}
	var nilHub *ThinkingHub
	nilHub.OnThought(func(string) {})
}
