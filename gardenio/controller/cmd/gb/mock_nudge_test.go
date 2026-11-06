package main

import (
	"context"
	"testing"

	"gardenio/controller/internal/coopapi"
)

func TestClaudelessASCII(t *testing.T) {
	tests := []struct{ in, want string }{
		{"plain ascii\n\nbody", "plain ascii\n\nbody"},
		{"call `gb stop` with a wrap-up — do not end the turn", "call `gb stop` with a wrap-up - do not end the turn"},
		{"New mail from a: b — run 'kd show gd-1' to read", "New mail from a: b - run 'kd show gd-1' to read"},
		{"‘q’ “qq” wait… a→b 1–2 x", "'q' \"qq\" wait... a->b 1-2 x"},
		{"café \U0001F331", "caf? ?"},
	}
	for _, tt := range tests {
		if got := claudelessASCII(tt.in); got != tt.want {
			t.Errorf("claudelessASCII(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestMockSafeNudge(t *testing.T) {
	msg := "wrap-up — done"
	if got := mockSafeNudge("claudeless --scenario /scenarios/mail-reader.toml --dangerously-skip-permissions", msg); got != "wrap-up - done" {
		t.Errorf("claudeless command: got %q", got)
	}
	if got := mockSafeNudge("claude --dangerously-skip-permissions", msg); got != msg {
		t.Errorf("claude command must pass the prompt through, got %q", got)
	}
}

type recordingCoop struct{ got string }

func (c *recordingCoop) Nudge(_ context.Context, message string) (*coopapi.NudgeResponse, error) {
	c.got = message
	return &coopapi.NudgeResponse{Delivered: true}, nil
}

func (c *recordingCoop) Shutdown(context.Context) error { return nil }

func TestMailCoop_FoldsOnlyForClaudeless(t *testing.T) {
	const nudge = "New mail from bridge: Decision gd-1 resolved — run 'kd show gd-2' to read"

	rec := &recordingCoop{}
	if _, err := mailCoop(rec, "claudeless --scenario /scenarios/mail-reader.toml").Nudge(context.Background(), nudge); err != nil {
		t.Fatal(err)
	}
	if rec.got != "New mail from bridge: Decision gd-1 resolved - run 'kd show gd-2' to read" {
		t.Errorf("claudeless mail nudge not folded: %q", rec.got)
	}

	rec = &recordingCoop{}
	if _, err := mailCoop(rec, "claude --dangerously-skip-permissions").Nudge(context.Background(), nudge); err != nil {
		t.Fatal(err)
	}
	if rec.got != nudge {
		t.Errorf("claude mail nudge changed: %q", rec.got)
	}
}
