package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

func TestNudgeAgent_CreatesInterruptMail(t *testing.T) {
	daemon := newMockDaemon()
	daemon.beads["test-agent"] = &beadsapi.BeadDetail{ID: "gd-a1", Title: "test-agent", Fields: map[string]string{"agent_state": "working"}}

	err := NudgeAgent(context.Background(), daemon, slog.Default(), "test-agent", "Slack mention (bead gd-x): please look\nsecond line")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	req := daemon.lastCreateBead()
	if req.Type != "mail" || req.Assignee != "test-agent" {
		t.Fatalf("expected mail to test-agent, got type=%q assignee=%q", req.Type, req.Assignee)
	}
	if req.Title != "Slack mention (bead gd-x): please look" {
		t.Fatalf("title should be the first line: %q", req.Title)
	}
	if !hasLabel(req.Labels, "delivery:interrupt") || !hasLabel(req.Labels, "from:slack-bridge") {
		t.Fatalf("labels wrong: %v", req.Labels)
	}
	if req.Description != "Slack mention (bead gd-x): please look\nsecond line" {
		t.Fatalf("description should carry the whole message")
	}
}

func TestNudgeAgent_EmptyAgentName_ReturnsError(t *testing.T) {
	err := NudgeAgent(context.Background(), nil, slog.Default(), "", "msg")
	if err == nil {
		t.Fatal("expected error for empty agent name")
	}
}

func TestNudgeAgent_NoCoopURL_StillDelivers(t *testing.T) {
	daemon := newMockDaemon()
	daemon.beads["test-agent"] = &beadsapi.BeadDetail{ID: "gd-a1", Title: "test-agent", Notes: ""}
	if err := NudgeAgent(context.Background(), daemon, slog.Default(), "test-agent", "msg"); err != nil {
		t.Fatalf("mail delivery must not depend on coop_url: %v", err)
	}
}

func TestNudgeAgent_DeadAgentState_ReturnsError(t *testing.T) {
	for _, state := range []string{"done", "failed", "stopping", "budget_exceeded"} {
		t.Run(state, func(t *testing.T) {
			daemon := newMockDaemon()
			daemon.beads["dead-agent"] = &beadsapi.BeadDetail{
				ID:     "dead-agent",
				Fields: map[string]string{"agent_state": state},
			}

			err := NudgeAgent(context.Background(), daemon, slog.Default(), "dead-agent", "msg")
			if err == nil {
				t.Fatal("expected error for dead agent")
			}
			if !strings.Contains(err.Error(), "not nudgeable") {
				t.Errorf("expected 'not nudgeable' error, got: %v", err)
			}
		})
	}
}

// TestNudgeAgent_StopRequested_ReturnsNotNudgeable covers the lame-duck
// shutdown-race (ka-0000002972): a thread agent that has called `gb stop`
// (stop_requested="true") but whose agent_state has NOT yet flipped to a
// terminal value still reports "working". Mail written for it would never be
// read, so it must be classified ErrAgentNotNudgeable (the mention path then
// session-resume respawns) and no mail written.
func TestNudgeAgent_StopRequested_ReturnsNotNudgeable(t *testing.T) {
	daemon := newMockDaemon()
	daemon.beads["lameduck-agent"] = &beadsapi.BeadDetail{
		ID: "lameduck-agent",
		Fields: map[string]string{
			"agent_state":    "working", // lag: not yet flipped to done
			"stop_requested": "true",    // gb stop already ran
		},
	}

	err := NudgeAgent(context.Background(), daemon, slog.Default(), "lameduck-agent", "msg")
	if !errors.Is(err, ErrAgentNotNudgeable) {
		t.Fatalf("expected error to wrap ErrAgentNotNudgeable, got: %v", err)
	}
	daemon.mu.Lock()
	n := len(daemon.mails)
	daemon.mu.Unlock()
	if n != 0 {
		t.Errorf("no mail may be written for a stop-requested agent, got %d", n)
	}
}

func TestNudgeAgentMail_ReturnsMailID(t *testing.T) {
	daemon := newMockDaemon()
	daemon.beads["working-agent"] = &beadsapi.BeadDetail{
		ID:     "working-agent",
		Fields: map[string]string{"agent_state": "working"},
	}

	id, err := NudgeAgentMail(context.Background(), daemon, slog.Default(), "working-agent", "msg")
	if err != nil {
		t.Fatalf("unexpected error for working agent: %v", err)
	}
	daemon.mu.Lock()
	n := len(daemon.mails)
	daemon.mu.Unlock()
	if n != 1 || id != "mail-1" {
		t.Fatalf("id=%q mails=%d, want one mail and its id", id, n)
	}
}

func TestAgentLooksDead(t *testing.T) {
	for _, c := range []struct {
		name string
		bead *beadsapi.BeadDetail
		want bool
	}{
		{"nil", nil, true},
		{"closed", &beadsapi.BeadDetail{Status: "closed", Fields: map[string]string{"agent_state": "working"}}, true},
		{"done", &beadsapi.BeadDetail{Status: "open", Fields: map[string]string{"agent_state": "done"}}, true},
		{"stop requested", &beadsapi.BeadDetail{Status: "open", Fields: map[string]string{"agent_state": "working", "stop_requested": "true"}}, true},
		{"working", &beadsapi.BeadDetail{Status: "open", Fields: map[string]string{"agent_state": "working"}}, false},
		{"spawning, no coop_url", &beadsapi.BeadDetail{Status: "open", Fields: map[string]string{"agent_state": "spawning"}}, false},
	} {
		if got := agentLooksDead(c.bead); got != c.want {
			t.Errorf("%s: agentLooksDead = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestIsAgentStartingUp(t *testing.T) {
	tests := []struct {
		state string
		want  bool
	}{
		{"spawning", true},
		{"assigning", true},
		{"working", false},
		{"done", false},
		{"failed", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := isAgentStartingUp(tt.state); got != tt.want {
			t.Errorf("isAgentStartingUp(%q) = %v, want %v", tt.state, got, tt.want)
		}
	}
}

// subscriberNudge is a NudgeFunc for resolver tests that stands in for the
// agent's mail subscriber: it hands message to the coop_url on the agent's
// bead (notes, then fields), as the pod's mailsub would on loopback. A bead
// with no coop_url is a pod that is not up: the mail would wait, so nothing
// is delivered and no error is returned.
func subscriberNudge(find func(ctx context.Context, agentName string) (*beadsapi.BeadDetail, error)) NudgeFunc {
	return func(ctx context.Context, agentName, message string) error {
		b, err := find(ctx, agentName)
		if err != nil {
			return err
		}
		url := beadsapi.ParseNotes(b.Notes)["coop_url"]
		if url == "" {
			url = b.Fields["coop_url"]
		}
		if url == "" {
			return nil
		}
		body, _ := json.Marshal(map[string]string{"message": message})
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url+"/api/v1/agent/nudge", bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return fmt.Errorf("coop nudge status %d", resp.StatusCode)
		}
		return nil
	}
}
