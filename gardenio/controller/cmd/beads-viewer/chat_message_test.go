package main

// Unit tests for the shared message-bead chat projector (chat_message.go).
// These exercise the pure per-message text/author extractors that the
// bead-show chat thread and the user inbox both reuse. They were relocated
// here from maps_chat_test.go when the old /maps view was retired
// (ka-0000000247) — the projector outlived the maps page.

import (
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

func TestMessageBodyText_Precedence(t *testing.T) {
	cases := []struct {
		name string
		b    *beadsapi.BeadDetail
		want string
	}{
		{"fields.text wins", &beadsapi.BeadDetail{Fields: map[string]string{"text": "F"}, Description: "D", Title: "T"}, "F"},
		{"description next", &beadsapi.BeadDetail{Description: "D", Title: "T"}, "D"},
		{"title last", &beadsapi.BeadDetail{Title: "T"}, "T"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := messageBodyText(tc.b); got != tc.want {
				t.Errorf("messageBodyText = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestMessageAuthor_Precedence verifies source_agent → created_by → operated_by
// (provider-prefix stripped).
func TestMessageAuthor_Precedence(t *testing.T) {
	cases := []struct {
		name string
		b    *beadsapi.BeadDetail
		want string
	}{
		{"source_agent wins", &beadsapi.BeadDetail{Fields: map[string]string{"source_agent": "agent-x"}, CreatedBy: "cb"}, "agent-x"},
		{"created_by next", &beadsapi.BeadDetail{CreatedBy: "cb"}, "cb"},
		{"operated_by prefix stripped", &beadsapi.BeadDetail{OperatedBy: "slack:U123"}, "U123"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := messageAuthor(tc.b); got != tc.want {
				t.Errorf("messageAuthor = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestMessageKind_SlackConversationLabels verifies the slack-bridge
// conversation labels (slack-thread-reply / slack-mention / slack-thread-message)
// now classify as kind="slack" — previously they degraded to "message" because
// they carry no "source:slack" label and no fields.source (ka-0000000676).
func TestMessageKind_SlackConversationLabels(t *testing.T) {
	cases := []struct {
		name string
		b    *beadsapi.BeadDetail
		want string
	}{
		{"mail type wins", &beadsapi.BeadDetail{Type: "mail", Labels: []string{"slack-mention"}}, "mail"},
		{"squawk label wins over slack", &beadsapi.BeadDetail{Type: "message", Labels: []string{"squawk", "slack-thread-message"}}, "squawk"},
		{"thread reply → slack", &beadsapi.BeadDetail{Type: "message", Labels: []string{"slack-thread-reply", "slack-thread-message", "thread:gd-x"}}, "slack"},
		{"mention → slack", &beadsapi.BeadDetail{Type: "message", Labels: []string{"slack-mention", "slack-thread-message"}}, "slack"},
		{"legacy source:slack still works", &beadsapi.BeadDetail{Type: "message", Labels: []string{"source:slack"}}, "slack"},
		{"case-insensitive", &beadsapi.BeadDetail{Type: "message", Labels: []string{"Slack-Thread-Reply"}}, "slack"},
		{"plain message", &beadsapi.BeadDetail{Type: "message", Labels: []string{"from:agent-x"}}, "message"},
		{"nil", nil, "message"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := messageKind(tc.b); got != tc.want {
				t.Errorf("messageKind = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestMessageDirection classifies chat lines as user- vs agent-authored without
// relying on the inbox query wiring: slack-bridge conversation labels ⇒ from-user,
// fields.source_agent ⇒ from-agent, created_by=slack-bridge ⇒ from-user, else
// from-agent (ka-0000000676).
func TestMessageDirection(t *testing.T) {
	cases := []struct {
		name string
		b    *beadsapi.BeadDetail
		want string
	}{
		{
			"human slack reply (label + slack-bridge author, no source_agent)",
			&beadsapi.BeadDetail{Type: "message", CreatedBy: "slack-bridge", OperatedBy: "oidc:sky.bennett@acme.example", Labels: []string{"slack-thread-reply"}},
			directionFromUser,
		},
		{
			"human bot mention (slack-mention label)",
			&beadsapi.BeadDetail{Type: "message", CreatedBy: "slack-bridge", Labels: []string{"slack-mention", "slack-thread-message"}, Fields: map[string]string{"slack_user_id": "U09"}},
			directionFromUser,
		},
		{
			"agent squawk (source_agent set, human operated_by)",
			&beadsapi.BeadDetail{Type: "message", CreatedBy: "agent-x", OperatedBy: "slack:U07", Labels: []string{"from:agent-x", "squawk"}, Fields: map[string]string{"source_agent": "agent-x"}},
			directionFromAgent,
		},
		{
			"slack-bridge author without conversation label ⇒ from-user",
			&beadsapi.BeadDetail{Type: "message", CreatedBy: "slack-bridge"},
			directionFromUser,
		},
		{
			"plain agent mail (from label, no source_agent) ⇒ from-agent",
			&beadsapi.BeadDetail{Type: "mail", CreatedBy: "agent-y", Labels: []string{"from:agent-y"}},
			directionFromAgent,
		},
		{"nil ⇒ empty", nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := messageDirection(tc.b); got != tc.want {
				t.Errorf("messageDirection = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestProjectChatMessage_SetsDirectionAndSlackKind verifies the shared projector
// now stamps Direction + the slack kind so the bead-view chat panel gets
// agent-vs-user attribution without any inbox wiring (ka-0000000676).
func TestProjectChatMessage_SetsDirectionAndSlackKind(t *testing.T) {
	now := time.Date(2026, 7, 3, 0, 0, 0, 0, time.UTC)

	userReply := &beadsapi.BeadDetail{
		ID: "gd-reply", Type: "message", CreatedBy: "slack-bridge",
		OperatedBy: "oidc:sky.bennett@acme.example", OperatedByDisplay: "Sky Bennett",
		Labels: []string{"slack-thread-reply", "slack-thread-message"},
		Fields: map[string]string{"text": "is this related to gd-9PKRM3R6YQ?"},
	}
	m := projectChatMessage(userReply, "/beads", now, LinkifyOpts{})
	if m.Direction != directionFromUser {
		t.Errorf("user reply Direction = %q, want %q", m.Direction, directionFromUser)
	}
	if m.Kind != "slack" {
		t.Errorf("user reply Kind = %q, want slack", m.Kind)
	}
	// bead-ref linkification still fires on the body.
	if body := string(m.BodyHTML); !strings.Contains(body, `href="/beads/gd-9PKRM3R6YQ"`) {
		t.Errorf("user reply body not linkified: %q", body)
	}

	agentSquawk := &beadsapi.BeadDetail{
		ID: "gd-squawk", Type: "message", CreatedBy: "agent-x",
		OperatedBy: "slack:U07", Labels: []string{"from:agent-x", "squawk"},
		Fields: map[string]string{"text": "on it", "source_agent": "agent-x"},
	}
	sm := projectChatMessage(agentSquawk, "/beads", now, LinkifyOpts{})
	if sm.Direction != directionFromAgent {
		t.Errorf("agent squawk Direction = %q, want %q", sm.Direction, directionFromAgent)
	}
	if sm.Kind != "squawk" {
		t.Errorf("agent squawk Kind = %q, want squawk", sm.Kind)
	}
	if sm.Author != "agent-x" {
		t.Errorf("agent squawk Author = %q, want agent-x", sm.Author)
	}
}
