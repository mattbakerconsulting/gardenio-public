package hooks

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"testing"

	"gardenio/kbeads/internal/model"
	"gardenio/kbeads/internal/store"
)

// mockStore implements store.Store with only the methods needed for testing.
type mockStore struct {
	store.Store // embed to satisfy the full interface
	beads       []*model.Bead
	agentBeads  []*model.Bead // returned when querying type=agent
}

func (m *mockStore) ListBeads(_ context.Context, f model.BeadFilter) ([]*model.Bead, int, error) {
	if len(f.Type) > 0 && f.Type[0] == model.BeadType("agent") {
		return m.agentBeads, len(m.agentBeads), nil
	}
	return m.beads, len(m.beads), nil
}

func makeAdvice(id, title string, labels []string, fields map[string]any) *model.Bead {
	b, _ := json.Marshal(fields)
	return &model.Bead{
		ID:     id,
		Kind:   model.KindData,
		Type:   model.TypeAdvice,
		Title:  title,
		Status: model.StatusOpen,
		Labels: labels,
		Fields: b,
	}
}

func makeAgentBead(id, agentName string, fields map[string]any) *model.Bead {
	b, _ := json.Marshal(fields)
	return &model.Bead{
		ID:     id,
		Type:   model.BeadType("agent"),
		Title:  agentName,
		Status: model.StatusOpen,
		Fields: b,
	}
}

func TestHandleSessionEvent_NoAgent(t *testing.T) {
	h := NewHandler(&mockStore{}, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	resp := h.HandleSessionEvent(context.Background(), SessionEvent{})
	if resp.Block {
		t.Error("expected no block for empty event")
	}
}

func TestHandleSessionEvent_NoMatchingAdvice(t *testing.T) {
	s := &mockStore{beads: []*model.Bead{
		makeAdvice("gd-1", "unrelated", []string{"project:other"}, map[string]any{
			"hook_command": "echo hello",
			"hook_trigger": TriggerSessionEnd,
		}),
	}}
	h := NewHandler(s, slog.New(slog.NewTextHandler(os.Stderr, nil)))

	resp := h.HandleSessionEvent(context.Background(), SessionEvent{
		AgentID: "beads/crew/test-agent",
		Trigger: TriggerSessionEnd,
	})
	if resp.Block {
		t.Error("expected no block when no advice matches")
	}
}

func TestHandleSessionEvent_MatchingHookRuns(t *testing.T) {
	s := &mockStore{beads: []*model.Bead{
		makeAdvice("gd-2", "global advice", []string{"global"}, map[string]any{
			"hook_command":    "echo ran",
			"hook_trigger":    TriggerSessionEnd,
			"hook_on_failure": OnFailureIgnore,
		}),
	}}
	h := NewHandler(s, slog.New(slog.NewTextHandler(os.Stderr, nil)))

	resp := h.HandleSessionEvent(context.Background(), SessionEvent{
		AgentID: "beads/crew/test-agent",
		Trigger: TriggerSessionEnd,
	})
	if resp.Block {
		t.Error("expected no block for successful hook")
	}
}

func TestHandleSessionEvent_BlockOnFailure(t *testing.T) {
	s := &mockStore{beads: []*model.Bead{
		makeAdvice("gd-3", "blocking advice", []string{"global"}, map[string]any{
			"hook_command":    "exit 1",
			"hook_trigger":    TriggerSessionEnd,
			"hook_on_failure": OnFailureBlock,
		}),
	}}
	h := NewHandler(s, slog.New(slog.NewTextHandler(os.Stderr, nil)))

	resp := h.HandleSessionEvent(context.Background(), SessionEvent{
		AgentID: "beads/crew/test-agent",
		Trigger: TriggerSessionEnd,
	})
	if !resp.Block {
		t.Error("expected block when hook fails with on_failure=block")
	}
	if resp.Reason == "" {
		t.Error("expected non-empty reason for block")
	}
}

func TestHandleSessionEvent_WarnOnFailure(t *testing.T) {
	s := &mockStore{beads: []*model.Bead{
		makeAdvice("gd-4", "warning advice", []string{"global"}, map[string]any{
			"hook_command":    "exit 1",
			"hook_trigger":    TriggerSessionEnd,
			"hook_on_failure": OnFailureWarn,
		}),
	}}
	h := NewHandler(s, slog.New(slog.NewTextHandler(os.Stderr, nil)))

	resp := h.HandleSessionEvent(context.Background(), SessionEvent{
		AgentID: "beads/crew/test-agent",
		Trigger: TriggerSessionEnd,
	})
	if resp.Block {
		t.Error("expected no block for warn mode")
	}
	if len(resp.Warnings) == 0 {
		t.Error("expected warning message")
	}
}

func TestHandleSessionEvent_WrongTriggerSkipped(t *testing.T) {
	s := &mockStore{beads: []*model.Bead{
		makeAdvice("gd-5", "before-commit only", []string{"global"}, map[string]any{
			"hook_command":    "exit 1",
			"hook_trigger":    TriggerBeforeCommit,
			"hook_on_failure": OnFailureBlock,
		}),
	}}
	h := NewHandler(s, slog.New(slog.NewTextHandler(os.Stderr, nil)))

	resp := h.HandleSessionEvent(context.Background(), SessionEvent{
		AgentID: "beads/crew/test-agent",
		Trigger: TriggerSessionEnd, // wrong trigger
	})
	if resp.Block {
		t.Error("expected no block when trigger doesn't match")
	}
}

func TestHandleSessionEvent_AdviceSubscriptionsExtra(t *testing.T) {
	// Advice bead labeled "security" — only matches if agent subscribes to it.
	s := &mockStore{
		beads: []*model.Bead{
			makeAdvice("gd-10", "security advice", []string{"security"}, map[string]any{
				"hook_command":    "echo secure",
				"hook_trigger":    TriggerSessionEnd,
				"hook_on_failure": OnFailureBlock,
			}),
		},
		agentBeads: []*model.Bead{
			makeAgentBead("gd-a1", "test-agent", map[string]any{
				"agent":                "test-agent",
				"role":                 "crew",
				"project":              "beads",
				"advice_subscriptions": []string{"security"},
			}),
		},
	}
	h := NewHandler(s, slog.New(slog.NewTextHandler(os.Stderr, nil)))

	// Agent with advice_subscriptions=["security"] — should match.
	resp := h.HandleSessionEvent(context.Background(), SessionEvent{
		AgentID: "beads/crew/test-agent",
		Trigger: TriggerSessionEnd,
	})
	if resp.Block {
		t.Error("expected no block for matching advice_subscriptions (command succeeds)")
	}

	// Without agent bead (no custom subs) — should NOT match "security" label.
	s2 := &mockStore{
		beads: s.beads,
		// no agentBeads
	}
	h2 := NewHandler(s2, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	resp = h2.HandleSessionEvent(context.Background(), SessionEvent{
		AgentID: "beads/crew/test-agent",
		Trigger: TriggerSessionEnd,
	})
	if resp.Block {
		t.Error("expected no block when agent has no custom subscriptions")
	}
}

func TestHandleSessionEvent_AdviceSubscriptionsExclude(t *testing.T) {
	// Global advice — normally matches all agents.
	s := &mockStore{
		beads: []*model.Bead{
			makeAdvice("gd-11", "global advice", []string{"global"}, map[string]any{
				"hook_command":    "exit 1",
				"hook_trigger":    TriggerSessionEnd,
				"hook_on_failure": OnFailureBlock,
			}),
		},
		agentBeads: []*model.Bead{
			makeAgentBead("gd-a2", "test-agent", map[string]any{
				"agent":                        "test-agent",
				"role":                         "crew",
				"project":                      "beads",
				"advice_subscriptions_exclude": []string{"global"},
			}),
		},
	}
	h := NewHandler(s, slog.New(slog.NewTextHandler(os.Stderr, nil)))

	// Agent excludes "global" — should NOT match global advice.
	resp := h.HandleSessionEvent(context.Background(), SessionEvent{
		AgentID: "beads/crew/test-agent",
		Trigger: TriggerSessionEnd,
	})
	if resp.Block {
		t.Error("expected no block when agent excludes 'global' subscription")
	}
}

func TestHandleSessionEvent_ProjectScoping(t *testing.T) {
	s := &mockStore{beads: []*model.Bead{
		makeAdvice("gd-6", "beads-project advice", []string{"project:beads", "global"}, map[string]any{
			"hook_command":    "echo scoped",
			"hook_trigger":    TriggerSessionEnd,
			"hook_on_failure": OnFailureBlock,
		}),
	}}
	h := NewHandler(s, slog.New(slog.NewTextHandler(os.Stderr, nil)))

	// Agent in beads project — should match.
	resp := h.HandleSessionEvent(context.Background(), SessionEvent{
		AgentID: "beads/crew/test-agent",
		Trigger: TriggerSessionEnd,
	})
	if resp.Block {
		t.Error("expected no block for matching project (command succeeds)")
	}

	// Agent in different project — should NOT match.
	resp = h.HandleSessionEvent(context.Background(), SessionEvent{
		AgentID: "gastown/crew/other-agent",
		Trigger: TriggerSessionEnd,
	})
	if resp.Block {
		t.Error("expected no block for non-matching project")
	}
}
