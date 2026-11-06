package bridge

import (
	"testing"

	"gardenio/controller/internal/beadsapi"
)

func TestBuildDashboardHash_DistinguishesDoneFromIdle(t *testing.T) {
	idle := []beadsapi.AgentBead{
		{ID: "bd-1", AgentName: "bot-1", AgentState: "", Project: "gardenio"},
	}
	done := []beadsapi.AgentBead{
		{ID: "bd-1", AgentName: "bot-1", AgentState: "done", Project: "gardenio"},
	}
	rateLimited := []beadsapi.AgentBead{
		{ID: "bd-1", AgentName: "bot-1", AgentState: "rate_limited", Project: "gardenio"},
	}

	hashIdle := buildDashboardHash(idle, nil)
	hashDone := buildDashboardHash(done, nil)
	hashRateLimited := buildDashboardHash(rateLimited, nil)

	if hashIdle == hashDone {
		t.Errorf("idle and done should produce different hashes, both got %q", hashIdle)
	}
	if hashIdle == hashRateLimited {
		t.Errorf("idle and rate_limited should produce different hashes, both got %q", hashIdle)
	}
	if hashDone == hashRateLimited {
		t.Errorf("done and rate_limited should produce different hashes, both got %q", hashDone)
	}
}

func TestBuildDashboardHash_DistinguishesFailedFromDone(t *testing.T) {
	failed := []beadsapi.AgentBead{
		{ID: "bd-1", AgentName: "bot-1", AgentState: "failed", Project: "gardenio"},
	}
	done := []beadsapi.AgentBead{
		{ID: "bd-1", AgentName: "bot-1", AgentState: "done", Project: "gardenio"},
	}

	hashFailed := buildDashboardHash(failed, nil)
	hashDone := buildDashboardHash(done, nil)

	if hashFailed == hashDone {
		t.Errorf("failed and done should produce different hashes, both got %q", hashFailed)
	}
}

func TestFilterAgents(t *testing.T) {
	agents := []beadsapi.AgentBead{
		{AgentName: "gardenio-crew-1", Project: "gardenio"},
		{AgentName: "monorepo-gardener-1", Project: "monorepo"},
		{AgentName: "monorepo-crew-2", Project: "monorepo"},
		{AgentName: "no-project-agent", Project: ""},
		{AgentName: "gardenio-crew-2", Project: "gardenio"},
	}

	t.Run("nil filter passes all agents", func(t *testing.T) {
		result := filterAgents(agents, nil)
		if len(result) != len(agents) {
			t.Errorf("expected %d agents, got %d", len(agents), len(result))
		}
	})

	t.Run("filter includes matching projects and no-project agents", func(t *testing.T) {
		f := &dashboardFilter{projects: map[string]bool{"gardenio": true}}
		result := filterAgents(agents, f)
		if len(result) != 3 {
			t.Errorf("expected 3 agents (2 gardenio + 1 no-project), got %d", len(result))
		}
		for _, a := range result {
			if a.Project != "gardenio" && a.Project != "" {
				t.Errorf("unexpected project %q for agent %s", a.Project, a.AgentName)
			}
		}
	})

	t.Run("filter excludes non-matching projects", func(t *testing.T) {
		f := &dashboardFilter{projects: map[string]bool{"gardenio": true}}
		result := filterAgents(agents, f)
		for _, a := range result {
			if a.Project == "monorepo" {
				t.Errorf("monorepo agent %s should have been filtered out", a.AgentName)
			}
		}
	})

	t.Run("empty filter excludes all project agents", func(t *testing.T) {
		f := &dashboardFilter{projects: map[string]bool{}}
		result := filterAgents(agents, f)
		if len(result) != 1 {
			t.Errorf("expected 1 agent (no-project only), got %d", len(result))
		}
		if result[0].AgentName != "no-project-agent" {
			t.Errorf("expected no-project-agent, got %s", result[0].AgentName)
		}
	})

	t.Run("empty input returns empty", func(t *testing.T) {
		f := &dashboardFilter{projects: map[string]bool{"gardenio": true}}
		result := filterAgents(nil, f)
		if len(result) != 0 {
			t.Errorf("expected 0 agents, got %d", len(result))
		}
	})

	t.Run("excludes agents whose role is routed elsewhere", func(t *testing.T) {
		// monorepo project has channel_roles routing fsm-triage to a sibling
		// channel; the monorepo dashboard should not show fsm-triage agents.
		f := &dashboardFilter{
			projects: map[string]bool{"monorepo": true},
			excludedRoles: map[string]map[string]bool{
				"monorepo": {"fsm-triage": true},
			},
		}
		input := []beadsapi.AgentBead{
			{AgentName: "gardener", Project: "monorepo", Role: "monorepo-gardener"},
			{AgentName: "fsm", Project: "monorepo", Role: "fsm-triage"},
			{AgentName: "free", Project: "monorepo", Role: ""},
		}
		result := filterAgents(input, f)
		if len(result) != 2 {
			t.Fatalf("expected 2 agents (gardener + free), got %d", len(result))
		}
		for _, a := range result {
			if a.Role == "fsm-triage" {
				t.Errorf("fsm-triage agent %s should have been filtered out", a.AgentName)
			}
		}
	})
}

func TestBuildDashboardFilter(t *testing.T) {
	const monorepoCh = "C0AL2AFMFS5"
	const fsmCh = "C0ASCCM058R"
	const dmCh = "D0AG8CC037H"

	projects := map[string]beadsapi.ProjectInfo{
		"monorepo": {
			Name:          "monorepo",
			SlackChannels: []string{monorepoCh, fsmCh, dmCh},
			ChannelRoles: map[string][]string{
				monorepoCh: {"monorepo-gardener"},
				fsmCh:      {"fsm-triage"},
				dmCh:       {"crew"},
			},
		},
		"gardenio": {
			Name:          "gardenio",
			SlackChannels: []string{"C-other"},
		},
		"unscoped": {
			Name: "unscoped",
		},
	}

	t.Run("monorepo dashboard excludes roles routed elsewhere", func(t *testing.T) {
		f := buildDashboardFilter(projects, monorepoCh)

		if !f.projects["monorepo"] {
			t.Errorf("monorepo should be in projects filter for its own channel")
		}
		if f.projects["gardenio"] {
			t.Errorf("gardenio (different channel) should not be included")
		}
		if !f.projects["unscoped"] {
			t.Errorf("unscoped projects should pass through to all dashboards")
		}

		excl := f.excludedRoles["monorepo"]
		if excl == nil {
			t.Fatal("expected role exclusions for monorepo")
		}
		if !excl["fsm-triage"] {
			t.Errorf("fsm-triage should be excluded from monorepo dashboard")
		}
		if !excl["crew"] {
			t.Errorf("crew (DM-only) should be excluded from monorepo dashboard")
		}
		if excl["monorepo-gardener"] {
			t.Errorf("monorepo-gardener is mapped to this channel and must NOT be excluded")
		}
	})

	t.Run("fsm-triage dashboard includes only fsm-triage role from monorepo", func(t *testing.T) {
		f := buildDashboardFilter(projects, fsmCh)

		if !f.projects["monorepo"] {
			t.Errorf("monorepo lists fsmCh in slack_channel; should be included")
		}
		excl := f.excludedRoles["monorepo"]
		if excl == nil {
			t.Fatal("expected role exclusions for monorepo")
		}
		if !excl["monorepo-gardener"] {
			t.Errorf("monorepo-gardener (mapped to monorepoCh) should be excluded here")
		}
		if excl["fsm-triage"] {
			t.Errorf("fsm-triage (mapped to fsmCh) must NOT be excluded here")
		}
	})

	t.Run("multi-channel role allowed in any of its mapped channels", func(t *testing.T) {
		const chA = "Cmulti-a"
		const chB = "Cmulti-b"
		ps := map[string]beadsapi.ProjectInfo{
			"p": {
				Name:          "p",
				SlackChannels: []string{chA, chB},
				ChannelRoles: map[string][]string{
					chA: {"shared-role"},
					chB: {"shared-role"},
				},
			},
		}
		fA := buildDashboardFilter(ps, chA)
		if fA.excludedRoles["p"]["shared-role"] {
			t.Errorf("shared-role mapped to chA must not be excluded from chA dashboard")
		}
		fB := buildDashboardFilter(ps, chB)
		if fB.excludedRoles["p"]["shared-role"] {
			t.Errorf("shared-role mapped to chB must not be excluded from chB dashboard")
		}
	})
}
