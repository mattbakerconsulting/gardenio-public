package main

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// ADR 0004 / gd-0D8aGbRwYp: the agent detail card badges the agent CLI the pod
// resolved to, falling back to the per-spawn override, and stays silent for a
// bead that carries neither (it predates the field).
func TestAgentHarnessBadge_DetailCard(t *testing.T) {
	cases := []struct {
		name   string
		fields map[string]string
		want   string // "" = no badge
	}{
		{"resolved codex", map[string]string{"role": "crew", "resolved_harness": "codex"}, "agent-harness-codex"},
		{"resolved gemini", map[string]string{"role": "crew", "resolved_harness": "gemini"}, "agent-harness-gemini"},
		{"resolved claude", map[string]string{"role": "crew", "resolved_harness": "claude"}, "agent-harness-claude"},
		{"override only", map[string]string{"role": "crew", "harness": "codex"}, "agent-harness-codex"},
		{"resolved wins over override", map[string]string{"role": "crew", "resolved_harness": "claude", "harness": "codex"}, "agent-harness-claude"},
		{"neither", map[string]string{"role": "crew"}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			mux := mkServer(t, agentSpawnBadgeFake(c.fields, nil))
			body := getSpawnBadgeBody(t, mux, "/gd-agent1")
			has := strings.Contains(body, `class="agent-harness agent-harness-`)
			if c.want == "" {
				if has {
					t.Fatalf("expected no harness badge, got body:\n%s", body)
				}
				return
			}
			if !strings.Contains(body, `class="agent-harness `+c.want+`"`) {
				t.Errorf("expected badge %q in body:\n%s", c.want, body)
			}
		})
	}
}

func TestAgentHarness(t *testing.T) {
	if got := agentHarness(map[string]string{"resolved_harness": " codex "}); got != "codex" {
		t.Errorf("got %q", got)
	}
	if got := agentHarness(nil); got != "" {
		t.Errorf("nil fields: got %q", got)
	}
}

// ADR 0005 §5 D / gd-sXOmQvpFWR: the /agents list row chips every non-claude
// harness (it used to hard-code codex), and leaves claude rows unchanged.
func TestAgentHarnessChip_ListRow(t *testing.T) {
	mk := func(id, harness string) *beadsapi.BeadDetail {
		return &beadsapi.BeadDetail{
			ID: id, Title: "crew-" + id, Type: "agent", Status: "in_progress",
			Labels: []string{"project:gardenio"}, CreatedAt: time.Now(), UpdatedAt: time.Now(),
			Fields: map[string]string{"agent": "crew-" + id, "role": "crew", "agent_state": "working", "project": "gardenio", "resolved_harness": harness},
		}
	}
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{mk("gd-g", "gemini"), mk("gd-x", "codex"), mk("gd-c", "claude")},
		Total: 3,
	}
	srv := NewServer(f, slog.Default(), "", "", "", &fakeScreenClient{})
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/agents", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{"agent-harness-gemini", "agent-harness-codex"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing chip %q in /agents body", want)
		}
	}
	if strings.Contains(body, "agent-harness-claude") {
		t.Errorf("claude rows must not carry a harness chip")
	}
}
