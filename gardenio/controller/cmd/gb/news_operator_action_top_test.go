package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// newsOpActionWireBead is the minimal daemon wire shape the fake returns for the
// operator-action-block sections (open decisions + open escalations).
type newsOpActionWireBead struct {
	ID        string            `json:"id"`
	Title     string            `json:"title"`
	Type      string            `json:"type"`
	Status    string            `json:"status"`
	Priority  int               `json:"priority"`
	Labels    []string          `json:"labels"`
	Fields    map[string]string `json:"fields"`
	CreatedAt string            `json:"created_at"`
}

// newsOpActionServer wires a fake daemon that returns the given decisions +
// escalations for their section queries and nothing else. It honors the sort +
// limit params the way the real daemon's SQL does.
func newsOpActionServer(t *testing.T, decisions, escalations []newsOpActionWireBead) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/beads", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		limitParam, _ := strconv.Atoi(q.Get("limit"))
		var rows []newsOpActionWireBead
		switch {
		case q.Get("type") == "decision" && q.Get("status") == "open":
			rows = decisions
		case q.Get("type") == "escalation" && q.Get("status") == "open":
			rows = escalations
		}
		total := len(rows)
		if limitParam > 0 && len(rows) > limitParam {
			rows = rows[:limitParam]
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"beads": rows, "total": total})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatalf("beadsapi.New: %v", err)
	}
	old := daemon
	daemon = c
	t.Cleanup(func() { daemon = old })
}

// TestNewsOperatorActionBlockLeadsAndUnsuppressible is the end-to-end regression
// for ka-0000002114. It proves the operator-action block (open P0/P1 escalations
// + open decisions):
//
//  1. leads the digest — the escalations heading appears before the decisions
//     heading, and both appear before any in-progress/closed section;
//  2. surfaces only P0/P1 escalations (a P2 is dropped from the top block);
//  3. cannot be suppressed by --no-decisions (deprecated no-op); and
//  4. survives the --active-only triage mode.
func TestNewsOperatorActionBlockLeadsAndUnsuppressible(t *testing.T) {
	decisions := []newsOpActionWireBead{
		{ID: "gd-dec1", Title: "merge gate MR !100", Type: "decision", Status: "open", Labels: []string{"project:gardenio"}, Fields: map[string]string{"prompt": "merge gate MR !100"}, CreatedAt: "2026-08-01T00:00:00Z"},
	}
	escalations := []newsOpActionWireBead{
		{ID: "gd-esc0", Title: "Merge MR !7642 — kd search covers comment bodies", Type: "escalation", Status: "open", Priority: 0, Labels: []string{"project:gardenio"}, Fields: map[string]string{"operation_type": "gitlab_mr_merge"}, CreatedAt: "2026-08-02T00:00:00Z"},
		{ID: "gd-esc1", Title: "Seed dailbert desk from PR#5", Type: "escalation", Status: "open", Priority: 1, Labels: []string{"project:gardenio"}, Fields: map[string]string{"operation_type": "gitlab_mr_merge"}, CreatedAt: "2026-08-03T00:00:00Z"},
		{ID: "gd-esc2", Title: "low-priority P2 escalation", Type: "escalation", Status: "open", Priority: 2, Labels: []string{"project:gardenio"}, Fields: map[string]string{"operation_type": "misc"}, CreatedAt: "2026-08-04T00:00:00Z"},
	}
	newsOpActionServer(t, decisions, escalations)

	// --- default mode with --no-decisions set (must STILL show the block). ---
	setNewsFlags(t, map[string]string{
		"project":      "gardenio",
		"all":          "true",
		"window":       "24h",
		"no-decisions": "true",
	})
	t.Cleanup(func() { _ = newsCmd.Flags().Set("no-decisions", "false") })

	out := string(captureStdout(t, func() error {
		return runNews(newsCmd, nil)
	}))

	escHeadingIdx := strings.Index(out, "Open escalations awaiting operator")
	decHeadingIdx := strings.Index(out, "Open decisions awaiting operator")
	if escHeadingIdx < 0 {
		t.Fatalf("escalation heading missing (--no-decisions must not suppress it):\n%s", out)
	}
	if decHeadingIdx < 0 {
		t.Fatalf("decision heading missing (--no-decisions must be a no-op):\n%s", out)
	}
	if escHeadingIdx > decHeadingIdx {
		t.Errorf("escalations must lead decisions; got esc@%d dec@%d\n%s", escHeadingIdx, decHeadingIdx, out)
	}
	if !strings.Contains(out, "gd-esc0 P0") || !strings.Contains(out, "gd-esc1 P1") {
		t.Errorf("both P0 and P1 escalations must appear:\n%s", out)
	}
	if strings.Contains(out, "gd-esc2") {
		t.Errorf("P2 escalation must NOT appear in the top block:\n%s", out)
	}
	if !strings.Contains(out, "[gitlab_mr_merge]") {
		t.Errorf("operation_type should be surfaced inline:\n%s", out)
	}
}

// TestNewsOperatorActionBlockSurvivesActiveOnly proves the block leads even the
// --active-only triage mode (previously it was dropped entirely).
func TestNewsOperatorActionBlockSurvivesActiveOnly(t *testing.T) {
	escalations := []newsOpActionWireBead{
		{ID: "gd-escA", Title: "P0 merge ask", Type: "escalation", Status: "open", Priority: 0, Labels: []string{"project:gardenio"}, Fields: map[string]string{"operation_type": "gitlab_mr_merge"}, CreatedAt: "2026-08-02T00:00:00Z"},
	}
	newsOpActionServer(t, nil, escalations)

	setNewsFlags(t, map[string]string{
		"project":     "gardenio",
		"all":         "true",
		"window":      "24h",
		"active-only": "true",
	})

	out := string(captureStdout(t, func() error {
		return runNews(newsCmd, nil)
	}))
	if !strings.Contains(out, "Open escalations awaiting operator") || !strings.Contains(out, "gd-escA P0") {
		t.Errorf("--active-only must still surface the P0 escalation block:\n%s", out)
	}
}

// TestNewsOperatorActionBlockJSON proves the --json path always carries
// open_escalations (P0/P1 only) alongside open_decisions.
func TestNewsOperatorActionBlockJSON(t *testing.T) {
	escalations := []newsOpActionWireBead{
		{ID: "gd-escJ0", Title: "P0 ask", Type: "escalation", Status: "open", Priority: 0, Labels: []string{"project:gardenio"}, Fields: map[string]string{"operation_type": "mint_secret"}, CreatedAt: "2026-08-02T00:00:00Z"},
		{ID: "gd-escJ2", Title: "P2 ask", Type: "escalation", Status: "open", Priority: 2, Labels: []string{"project:gardenio"}, Fields: map[string]string{"operation_type": "misc"}, CreatedAt: "2026-08-03T00:00:00Z"},
	}
	newsOpActionServer(t, nil, escalations)

	setNewsFlags(t, map[string]string{
		"project": "gardenio",
		"all":     "true",
		"window":  "24h",
	})
	oldJSON := jsonOutput
	jsonOutput = true
	t.Cleanup(func() { jsonOutput = oldJSON })

	out := string(captureStdout(t, func() error {
		return runNews(newsCmd, nil)
	}))
	var got newsJSONOutput
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("unmarshal news json: %v\n%s", err, out)
	}
	if len(got.OpenEscalations) != 1 {
		t.Fatalf("open_escalations = %d rows, want 1 (P0 only; P2 dropped): %+v", len(got.OpenEscalations), got.OpenEscalations)
	}
	e := got.OpenEscalations[0]
	if e.ID != "gd-escJ0" || e.Priority != 0 || e.OperationType != "mint_secret" {
		t.Errorf("open_escalations[0] = %+v, want id=gd-escJ0 priority=0 op=mint_secret", e)
	}
}
