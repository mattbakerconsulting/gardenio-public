package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// errTestDaemon is the canned daemon failure used to exercise each fragment's
// best-effort (empty section + retry, never 500) degradation path.
var errTestDaemon = errors.New("daemon unavailable")

// hubSectionReq builds a GET request for a project-hub lazy fragment with the
// {name} path value set, so the handler resolves the project without a router.
func hubSectionReq(name string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/b/project/"+name+"/x", nil)
	r.SetPathValue("name", name)
	return r
}

func newHubServer(f *fakeDaemon) *Server {
	srv := NewServer(f, testLogger(), "", "", "", nil)
	srv.SetNow(func() time.Time { return time.Date(2026, 6, 22, 12, 0, 0, 0, time.UTC) })
	return srv
}

// --- Agents fragment -------------------------------------------------------

func TestProjectHubAgents_HappyPath(t *testing.T) {
	f := newFake()
	f.liveAgentsResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			{
				ID:     "gd-agent1",
				Title:  "agent-alpha",
				Type:   "agent",
				Status: "in_progress",
				Labels: []string{"project:gardenio"},
				Fields: map[string]string{
					"agent":       "agent-alpha",
					"role":        "crew",
					"agent_state": "working",
					"task_id":     "gd-task7",
					"task_title":  "Do the thing",
				},
			},
		},
		Total: 1,
	}
	srv := newHubServer(f)

	rec := httptest.NewRecorder()
	srv.handleProjectHubAgents(rec, hubSectionReq("gardenio"))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"agent-alpha", "crew", "working", "gd-task7", "Do the thing", "All agents"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q\n%s", want, body)
		}
	}
}

func TestProjectHubAgents_DropsUnengagedPrewarmed(t *testing.T) {
	f := newFake()
	f.liveAgentsResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			{
				ID:     "gd-warm",
				Title:  "warm-pod",
				Type:   "agent",
				Status: "open",
				Labels: []string{"project:gardenio"},
				Fields: map[string]string{"agent_state": "prewarmed"},
			},
		},
		Total: 1,
	}
	srv := newHubServer(f)

	rec := httptest.NewRecorder()
	srv.handleProjectHubAgents(rec, hubSectionReq("gardenio"))

	body := rec.Body.String()
	if strings.Contains(body, "warm-pod") {
		t.Errorf("unengaged prewarmed agent should be dropped\n%s", body)
	}
	if !strings.Contains(body, "No active agents") {
		t.Errorf("expected empty-state copy\n%s", body)
	}
}

func TestProjectHubAgents_Empty(t *testing.T) {
	f := newFake()
	f.liveAgentsResp = &beadsapi.ListBeadsResult{}
	srv := newHubServer(f)

	rec := httptest.NewRecorder()
	srv.handleProjectHubAgents(rec, hubSectionReq("gardenio"))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "No active agents") {
		t.Errorf("expected empty-state copy\n%s", rec.Body.String())
	}
}

func TestProjectHubAgents_DaemonError_Retry(t *testing.T) {
	f := newFake()
	f.listErr = errTestDaemon
	srv := newHubServer(f)

	rec := httptest.NewRecorder()
	srv.handleProjectHubAgents(rec, hubSectionReq("gardenio"))

	if rec.Code != http.StatusOK {
		t.Fatalf("best-effort: status = %d, want 200 (never 500)", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Retry") || !strings.Contains(body, "load agents") {
		t.Errorf("expected error + retry affordance\n%s", body)
	}
}

func TestProjectHubAgents_MissingName(t *testing.T) {
	srv := newHubServer(newFake())
	rec := httptest.NewRecorder()
	srv.handleProjectHubAgents(rec, hubSectionReq(""))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

// --- Gardeners fragment -----------------------------------------------------

func TestProjectHubGardeners_HappyPath_PausedBanner(t *testing.T) {
	f := newFake()
	f.triggerScheduleResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			{
				ID:     "gd-cap1",
				Title:  "nightly-drainer",
				Type:   "trigger",
				Status: "open",
				Labels: []string{"role:gardener", "project:gardenio"},
				Fields: map[string]string{"role": "gardener"},
			},
		},
		Total: 1,
	}
	// Schedules disabled on the project → paused banner.
	f.projectsResp = map[string]beadsapi.ProjectInfo{
		"gardenio": {Name: "gardenio", SchedulesEnabled: false, TriggersEnabled: true},
	}
	srv := newHubServer(f)

	rec := httptest.NewRecorder()
	srv.handleProjectHubGardeners(rec, hubSectionReq("gardenio"))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "nightly-drainer") {
		t.Errorf("body missing gardener\n%s", body)
	}
	if !strings.Contains(body, "paused") {
		t.Errorf("expected paused banner (schedules disabled)\n%s", body)
	}
}

func TestProjectHubGardeners_Empty(t *testing.T) {
	f := newFake()
	f.triggerScheduleResp = &beadsapi.ListBeadsResult{}
	f.projectsResp = map[string]beadsapi.ProjectInfo{
		"gardenio": {Name: "gardenio", SchedulesEnabled: true, TriggersEnabled: true},
	}
	srv := newHubServer(f)

	rec := httptest.NewRecorder()
	srv.handleProjectHubGardeners(rec, hubSectionReq("gardenio"))

	body := rec.Body.String()
	if !strings.Contains(body, "No gardeners or schedules") {
		t.Errorf("expected empty-state copy\n%s", body)
	}
	if strings.Contains(body, "paused") {
		t.Errorf("no paused banner when both flags enabled\n%s", body)
	}
}

func TestProjectHubGardeners_DaemonError_Retry(t *testing.T) {
	f := newFake()
	f.listErr = errTestDaemon
	srv := newHubServer(f)

	rec := httptest.NewRecorder()
	srv.handleProjectHubGardeners(rec, hubSectionReq("gardenio"))

	if rec.Code != http.StatusOK {
		t.Fatalf("best-effort: status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Retry") {
		t.Errorf("expected retry affordance\n%s", rec.Body.String())
	}
}

// --- Accounts fragment -----------------------------------------------------

func TestProjectHubAccounts_HappyPath(t *testing.T) {
	f := newFake()
	f.accountsByProjectResp = []beadsapi.AccountInfo{
		{Name: "claude-max-litellm", Provider: "anthropic", Model: "opus", Status: "active"},
	}
	// The full bead is re-fetched by name for buildCostAccountView.
	f.accountResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			{
				ID:     "gd-acct1",
				Title:  "claude-max-litellm",
				Type:   "account",
				Status: "open",
				Fields: map[string]string{"provider": "anthropic", "model": "opus", "status": "active"},
			},
		},
		Total: 1,
	}
	srv := newHubServer(f)

	rec := httptest.NewRecorder()
	srv.handleProjectHubAccounts(rec, hubSectionReq("gardenio"))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if f.accountsByProjectLastArg != "gardenio" {
		t.Errorf("ListAccountBeadsByProject called with %q, want gardenio", f.accountsByProjectLastArg)
	}
	body := rec.Body.String()
	for _, want := range []string{"claude-max-litellm", "anthropic", "gd-acct1"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q\n%s", want, body)
		}
	}
}

func TestProjectHubAccounts_NameOnlyWhenBeadMissing(t *testing.T) {
	f := newFake()
	f.accountsByProjectResp = []beadsapi.AccountInfo{
		{Name: "ghost-account", Provider: "anthropic", Model: "opus"},
	}
	// No matching bead returned → degrade to a name-only card, not a failure.
	f.accountResp = &beadsapi.ListBeadsResult{}
	srv := newHubServer(f)

	rec := httptest.NewRecorder()
	srv.handleProjectHubAccounts(rec, hubSectionReq("gardenio"))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "ghost-account") {
		t.Errorf("expected name-only card\n%s", body)
	}
}

func TestProjectHubAccounts_Empty(t *testing.T) {
	f := newFake()
	f.accountsByProjectResp = nil
	srv := newHubServer(f)

	rec := httptest.NewRecorder()
	srv.handleProjectHubAccounts(rec, hubSectionReq("gardenio"))

	if !strings.Contains(rec.Body.String(), "No accounts authorized") {
		t.Errorf("expected empty-state copy\n%s", rec.Body.String())
	}
}

func TestProjectHubAccounts_DaemonError_Retry(t *testing.T) {
	f := newFake()
	f.accountsByProjectErr = errTestDaemon
	srv := newHubServer(f)

	rec := httptest.NewRecorder()
	srv.handleProjectHubAccounts(rec, hubSectionReq("gardenio"))

	if rec.Code != http.StatusOK {
		t.Fatalf("best-effort: status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Retry") {
		t.Errorf("expected retry affordance\n%s", rec.Body.String())
	}
}

// --- Costs fragment --------------------------------------------------------

func TestProjectHubCosts_RollupSumsSpend(t *testing.T) {
	f := newFake()
	f.accountsByProjectResp = []beadsapi.AccountInfo{
		{Name: "acct-a"},
		{Name: "acct-b"},
	}
	f.accountResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			{ID: "gd-a", Title: "acct-a", Type: "account", Status: "open", Fields: map[string]string{
				"daily_cost_usd": "1.50", "weekly_cost_usd": "10.00", "total_cost_usd": "100.00",
			}},
			{ID: "gd-b", Title: "acct-b", Type: "account", Status: "open", Fields: map[string]string{
				"daily_cost_usd": "2.50", "weekly_cost_usd": "20.00", "total_cost_usd": "200.00",
			}},
		},
		Total: 2,
	}
	srv := newHubServer(f)

	rec := httptest.NewRecorder()
	srv.handleProjectHubCosts(rec, hubSectionReq("gardenio"))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	// 1.50 + 2.50 = 4.00 daily; 300.00 lifetime.
	for _, want := range []string{"$4.00", "$300.00", "acct-a", "acct-b"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q\n%s", want, body)
		}
	}
}

func TestProjectHubCosts_Empty(t *testing.T) {
	f := newFake()
	f.accountsByProjectResp = nil
	srv := newHubServer(f)

	rec := httptest.NewRecorder()
	srv.handleProjectHubCosts(rec, hubSectionReq("gardenio"))

	if !strings.Contains(rec.Body.String(), "No accounts to roll up") {
		t.Errorf("expected empty-state copy\n%s", rec.Body.String())
	}
}

func TestProjectHubCosts_DaemonError_Retry(t *testing.T) {
	f := newFake()
	f.accountsByProjectErr = errTestDaemon
	srv := newHubServer(f)

	rec := httptest.NewRecorder()
	srv.handleProjectHubCosts(rec, hubSectionReq("gardenio"))

	if rec.Code != http.StatusOK {
		t.Fatalf("best-effort: status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Retry") {
		t.Errorf("expected retry affordance\n%s", rec.Body.String())
	}
}

func TestBuildProjectCostRollup(t *testing.T) {
	got := buildProjectCostRollup([]costAccountView{
		{DailyCostUSD: 1, WeeklyCostUSD: 5, TotalCostUSD: 50},
		{DailyCostUSD: 2, WeeklyCostUSD: 7, TotalCostUSD: 70},
	})
	if got.DailyUSD != 3 || got.WeeklyUSD != 12 || got.TotalUSD != 120 {
		t.Errorf("rollup = %+v, want daily 3 / weekly 12 / total 120", got)
	}
	if !got.HasSpend || got.Accounts != 2 {
		t.Errorf("rollup = %+v, want HasSpend + 2 accounts", got)
	}

	zero := buildProjectCostRollup([]costAccountView{{Name: "no-spend"}})
	if zero.HasSpend {
		t.Errorf("zero-spend account should not set HasSpend: %+v", zero)
	}

	// Shared litellm_key: two accounts stamped the same per-key bill by
	// spend_sync must contribute it once, not twice (ka-0000003446). Owner is
	// the min name (glm < litellm).
	shared := buildProjectCostRollup([]costAccountView{
		{Name: "litellm-bedrock", LiteLLMKey: "sk-shared", DailyCostUSD: 10, WeeklyCostUSD: 20, TotalCostUSD: 100},
		{Name: "glm-bedrock", LiteLLMKey: "sk-shared", DailyCostUSD: 10, WeeklyCostUSD: 20, TotalCostUSD: 100},
	})
	if shared.DailyUSD != 10 || shared.WeeklyUSD != 20 || shared.TotalUSD != 100 {
		t.Errorf("shared-key rollup = %+v, want daily 10 / weekly 20 / total 100 (booked once)", shared)
	}
	// Account count stays per-account even though money deduped.
	if shared.Accounts != 2 {
		t.Errorf("shared-key rollup Accounts = %d, want 2 (per-account)", shared.Accounts)
	}
}

// --- Activity fragment -----------------------------------------------------

func TestProjectHubActivity_HappyPath_FiltersMetaTypes(t *testing.T) {
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			{ID: "gd-task1", Title: "Real task", Type: "task", Status: "in_progress", Labels: []string{"project:gardenio"}, UpdatedAt: time.Now()},
			{ID: "gd-adv1", Title: "Advice noise", Type: "advice", Status: "in_progress", Labels: []string{"project:gardenio"}, UpdatedAt: time.Now()},
		},
		Total: 2,
	}
	srv := newHubServer(f)

	rec := httptest.NewRecorder()
	srv.handleProjectHubActivity(rec, hubSectionReq("gardenio"))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Real task") {
		t.Errorf("expected actionable task in activity\n%s", body)
	}
	if strings.Contains(body, "Advice noise") {
		t.Errorf("non-actionable advice type should be filtered\n%s", body)
	}
}

func TestProjectHubActivity_Empty(t *testing.T) {
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{}
	srv := newHubServer(f)

	rec := httptest.NewRecorder()
	srv.handleProjectHubActivity(rec, hubSectionReq("gardenio"))

	if !strings.Contains(rec.Body.String(), "No recent in-progress activity") {
		t.Errorf("expected empty-state copy\n%s", rec.Body.String())
	}
}

func TestProjectHubActivity_DaemonError_Retry(t *testing.T) {
	f := newFake()
	f.listErr = errTestDaemon
	srv := newHubServer(f)

	rec := httptest.NewRecorder()
	srv.handleProjectHubActivity(rec, hubSectionReq("gardenio"))

	if rec.Code != http.StatusOK {
		t.Fatalf("best-effort: status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Retry") {
		t.Errorf("expected retry affordance\n%s", rec.Body.String())
	}
}

// --- Decisions fragment ----------------------------------------------------

func TestProjectHubDecisions_HappyPath(t *testing.T) {
	now := time.Date(2026, 6, 22, 12, 0, 0, 0, time.UTC)
	f := newFake()
	// buildProjectSummary reads listResp (Kinds=[issue], open+in_progress).
	f.listResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			{
				ID:                "gd-blocked1",
				Title:             "Waiting on a human",
				Type:              "task",
				Status:            "open",
				Labels:            []string{"project:gardenio"},
				BlockedOnDecision: true,
				UpdatedAt:         now.Add(-2 * time.Hour),
			},
		},
		Total: 1,
	}
	f.escalationsResp = []beadsapi.EscalationDetail{
		{Issue: &beadsapi.BeadDetail{
			ID:        "gd-esc1",
			Title:     "Grant IAM perm",
			Type:      "escalation",
			Status:    "open",
			Labels:    []string{"project:gardenio"},
			Fields:    map[string]string{"operation_type": "aws_iam"},
			CreatedAt: now.Add(-1 * time.Hour),
		}},
		// Foreign project escalation — must be filtered out.
		{Issue: &beadsapi.BeadDetail{
			ID:     "gd-esc2",
			Title:  "Other project escalation",
			Type:   "escalation",
			Status: "open",
			Labels: []string{"project:monorepo"},
			Fields: map[string]string{"operation_type": "k8s"},
		}},
	}
	srv := newHubServer(f)

	rec := httptest.NewRecorder()
	srv.handleProjectHubDecisions(rec, hubSectionReq("gardenio"))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"Waiting on a human", "Grant IAM perm", "aws_iam"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q\n%s", want, body)
		}
	}
	if strings.Contains(body, "Other project escalation") {
		t.Errorf("escalation from another project should be filtered\n%s", body)
	}
}

func TestProjectHubDecisions_Empty(t *testing.T) {
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{}
	f.escalationsResp = nil
	srv := newHubServer(f)

	rec := httptest.NewRecorder()
	srv.handleProjectHubDecisions(rec, hubSectionReq("gardenio"))

	if !strings.Contains(rec.Body.String(), "No decisions or escalations") {
		t.Errorf("expected empty-state copy\n%s", rec.Body.String())
	}
}

func TestProjectHubDecisions_BestEffortOnEscalationError(t *testing.T) {
	now := time.Date(2026, 6, 22, 12, 0, 0, 0, time.UTC)
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			{ID: "gd-b1", Title: "Blocked work", Type: "task", Status: "open",
				Labels: []string{"project:gardenio"}, BlockedOnDecision: true, UpdatedAt: now},
		},
		Total: 1,
	}
	f.escalationsErr = errTestDaemon // escalations fail; blocked list still renders.
	srv := newHubServer(f)

	rec := httptest.NewRecorder()
	srv.handleProjectHubDecisions(rec, hubSectionReq("gardenio"))

	if rec.Code != http.StatusOK {
		t.Fatalf("best-effort: status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Blocked work") {
		t.Errorf("blocked list should render despite escalation error\n%s", rec.Body.String())
	}
}
