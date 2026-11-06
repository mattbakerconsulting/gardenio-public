package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// setupArmsTestDaemon installs a test daemon whose GetBead serves the given
// beads by id (each a JSON object). Everything else 404s.
func setupArmsTestDaemon(t *testing.T, beads ...map[string]any) {
	t.Helper()
	byID := map[string]map[string]any{}
	for _, b := range beads {
		byID[b["id"].(string)] = b
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/beads/") {
			if b, ok := byID[strings.TrimPrefix(r.URL.Path, "/v1/beads/")]; ok {
				_ = json.NewEncoder(w).Encode(b)
				return
			}
		}
		http.Error(w, "not found", http.StatusNotFound)
	}))
	oldDaemon := daemon
	c, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatalf("failed to create test client: %v", err)
	}
	daemon = c
	t.Cleanup(func() {
		daemon = oldDaemon
		srv.Close()
	})
}

func armsAgent(role, taskID string) map[string]any {
	return map[string]any{"id": "gd-agent", "type": "agent", "status": "open",
		"fields": map[string]any{"role": role, "task_id": taskID}}
}

// TestOutputArmsDuty_PlanWithoutArms is the step's done-when fixture: a plan
// gardener holding a plan with no arms, one armless phase, one armed phase and
// one closed armless phase gets the duty naming the plan and the open armless
// phase only.
func TestOutputArmsDuty_PlanWithoutArms(t *testing.T) {
	setupArmsTestDaemon(t,
		armsAgent("plan-gardener", "gd-PLAN"),
		map[string]any{"id": "gd-PLAN", "type": "epic", "status": "open", "labels": []string{"plan"},
			"fields": map[string]any{"phase_order": `["gd-PH1","gd-PH2","gd-PH3"]`}},
		map[string]any{"id": "gd-PH1", "type": "epic", "status": "open"},
		map[string]any{"id": "gd-PH2", "type": "epic", "status": "open", "heraldry": "blue 🌱 . 🔥 . 📦"},
		map[string]any{"id": "gd-PH3", "type": "epic", "status": "closed"},
	)
	var buf bytes.Buffer
	outputArmsDuty(&buf, "gd-agent", "plan-gardener")
	got := buf.String()
	for _, want := range []string{
		"## Arms",
		"plan gd-PLAN, phase gd-PH1.",
		"kd heraldry library --search",
		"kd heraldry check",
		"kd update <id> --heraldry",
		"this run's comment on gd-PLAN",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("arms duty missing %q; got:\n%s", want, got)
		}
	}
	for _, unwanted := range []string{"gd-PH2", "gd-PH3"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("arms duty names %s, which needs no arms; got:\n%s", unwanted, got)
		}
	}
}

// TestOutputArmsDuty_ArmedPlanSilent: a plan whose epic and phases all bear
// arms renders nothing, so the duty costs an armed plan no preamble bytes.
func TestOutputArmsDuty_ArmedPlanSilent(t *testing.T) {
	setupArmsTestDaemon(t,
		armsAgent("plan-gardener", "gd-PLAN"),
		map[string]any{"id": "gd-PLAN", "type": "epic", "status": "open", "heraldry": "blue 🌱 . 🔥 . 📦",
			"fields": map[string]any{"phase_order": `["gd-PH1"]`}},
		map[string]any{"id": "gd-PH1", "type": "epic", "status": "open", "heraldry": "red 🏅 . 🖌️ . 🔎"},
	)
	var buf bytes.Buffer
	outputArmsDuty(&buf, "gd-agent", "plan-gardener")
	if buf.Len() != 0 {
		t.Errorf("armed plan must render nothing, got:\n%s", buf.String())
	}
}

// TestOutputArmsDuty_CrewHoldingArmlessEpic: a crew whose task is an armless
// epic gets the one-line hint; one holding a plain task gets nothing.
func TestOutputArmsDuty_CrewHoldingArmlessEpic(t *testing.T) {
	setupArmsTestDaemon(t,
		armsAgent("crew", "gd-EPIC"),
		map[string]any{"id": "gd-EPIC", "type": "epic", "status": "in_progress"},
	)
	var buf bytes.Buffer
	outputArmsDuty(&buf, "gd-agent", "crew")
	got := buf.String()
	if !strings.Contains(got, "Your task gd-EPIC is an epic with no arms yet") || !strings.Contains(got, "kd update <id> --heraldry") {
		t.Errorf("crew holding an armless epic must get the one-line hint; got:\n%s", got)
	}
	if strings.Contains(got, "## Arms") || strings.Count(strings.TrimSpace(got), "\n") != 0 {
		t.Errorf("the crew hint must be one line, not the gardener section; got:\n%s", got)
	}

	setupArmsTestDaemon(t,
		armsAgent("crew", "gd-TASK"),
		map[string]any{"id": "gd-TASK", "type": "task", "status": "in_progress"},
	)
	buf.Reset()
	outputArmsDuty(&buf, "gd-agent", "crew")
	if buf.Len() != 0 {
		t.Errorf("crew holding a task must get nothing, got:\n%s", buf.String())
	}
}
