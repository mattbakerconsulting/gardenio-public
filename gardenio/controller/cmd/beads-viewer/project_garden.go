package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/gardenhub"
	"gardenio/controller/internal/lands/purse"
	"gardenio/controller/internal/oidcauth"
)

// gardenProjects is the project panel's reader (gardenhub.ProjectReader, Lands workbench-glass spec WB-10, step 5.11):
// the sections /p/{project}'s hub shows, by the builders its lazy fragments use (project_hub_sections.go: agents,
// gardeners and their paused flags, the accounts and their cost rollup, the activity lists), the config its editor
// edits (project_config.go) with the hub's validity findings, the project's open decisions and escalations as the
// Queues panel's rows (queue_garden.go), the beads blocked on them (buildProjectSummary), and a tenant land's purse
// (internal/lands/purse, as GET /api/lands/{land}/purse reads it). All on the request's bearer, so the daemon cuts the
// answer as it cuts the hub. Each section degrades alone: one the daemon did not answer is named in Partial.
type gardenProjects struct{ s *Server }

// projectRecentStatuses is every state a bead moves through: the panel's "recently moved" list (the project's /feed)
// shows a close as readily as a claim.
var projectRecentStatuses = []string{"open", "in_progress", "blocked", "deferred", "closed"}

// Project implements gardenhub.ProjectReader.
func (g gardenProjects) Project(ctx context.Context, name string) (*gardenhub.ProjectDoc, error) {
	s := g.s
	projects, err := s.daemon.ListProjectBeads(ctx)
	if err != nil {
		return nil, err
	}
	info, isProject := projects[name]
	doc := &gardenhub.ProjectDoc{Name: name, CanEdit: isProject && info.BeadID != "" && projectConfigEditable(ctx), GardenerPresets: gardenGardenerPresets(),
		CanShare: isProject && info.BeadID != "" && windowShareOffered(ctx, name)} // a land window's target is the type:project bead
	if isProject {
		doc.SchedulesPaused, doc.TriggersPaused = !info.SchedulesEnabled, !info.TriggersEnabled
	}
	now := s.now()

	var mu sync.Mutex
	partial := func(what string) {
		mu.Lock()
		defer mu.Unlock()
		doc.Partial = append(doc.Partial, what)
	}
	var wg sync.WaitGroup
	run := func(f func()) { wg.Add(1); go func() { defer wg.Done(); f() }() }

	if isProject && info.BeadID != "" {
		run(func() {
			b, err := s.daemon.GetBead(ctx, info.BeadID)
			if err != nil || b == nil {
				s.logger.Warn("garden project: config bead", "project", name, "bead", info.BeadID, "error", err)
				partial("config")
				return
			}
			doc.Config = gardenProjectConfig(name, b)
		})
	}
	run(func() {
		rows, err := s.projectHubAgentRows(ctx, name)
		if err != nil {
			s.logger.Warn("garden project: agents", "project", name, "error", err)
			partial("agents")
			return
		}
		for _, a := range rows {
			doc.Agents = append(doc.Agents, gardenhub.ProjectAgent{ID: a.ID, Name: a.Name, Role: a.Role, State: a.AgentState, Status: a.Status, TaskID: a.TaskID, TaskTitle: a.TaskTitle, Ago: a.Ago})
		}
	})
	run(func() {
		rows, err := s.projectHubGardenerRows(ctx, name)
		if err != nil {
			s.logger.Warn("garden project: gardeners", "project", name, "error", err)
			partial("gardeners")
			return
		}
		for _, r := range rows {
			doc.Gardeners = append(doc.Gardeners, gardenhub.ProjectGardener{ID: r.ID, Name: r.Name, Kind: r.Kind, Status: r.Status, Enabled: r.Enabled, Goal: r.TerminalGoal, Runs: r.RunsSpawned})
		}
	})
	run(func() {
		accounts, err := s.projectAccountViews(ctx, name)
		if err != nil {
			s.logger.Warn("garden project: accounts", "project", name, "error", err)
			partial("accounts")
			return
		}
		rollup := buildProjectCostRollup(accounts) // marks a shared key's duplicates
		doc.Spend = &gardenhub.ProjectSpend{TodayUSD: rollup.DailyUSD, WeekUSD: rollup.WeeklyUSD, TotalUSD: rollup.TotalUSD, Has: rollup.HasSpend}
		for _, a := range accounts {
			row := gardenhub.ProjectAccount{Name: a.Name, Provider: a.Provider, Model: a.Model, Status: a.Status, Shared: a.CostDuplicate}
			if a.HasSpend {
				row.TodayUSD, row.WeekUSD, row.TotalUSD = usd(a.DailyCostUSD), usd(a.WeeklyCostUSD), usd(a.TotalCostUSD)
			}
			doc.Accounts = append(doc.Accounts, row)
		}
	})
	if isProject {
		run(func() {
			p, err := purse.Read(ctx, s.daemon, name, now)
			switch {
			case errors.Is(err, purse.ErrOperatorLand), errors.Is(err, purse.ErrNotLand):
				// an operator land has no purse; its tab shows the plot's spend and budget
				return
			case err != nil:
				s.logger.Warn("garden project: purse", "project", name, "error", err)
				u := purse.Unreadable(name, err, now) // unknown is the wall, and the tab says so
				doc.Purse = &u
			default:
				doc.Purse = &p
			}
			doc.Owner = landOwner(ctx, s.daemon, name, s.logger) // a land with a purse is a tenant's: name its owner
		})
	}
	run(func() {
		rows, err := s.projectHubActivityRows(ctx, name, []string{"in_progress"})
		if err != nil {
			s.logger.Warn("garden project: growing", "project", name, "error", err)
			partial("growing")
			return
		}
		doc.Growing = projectBeads(rows)
	})
	run(func() {
		rows, err := s.projectHubActivityRows(ctx, name, projectRecentStatuses)
		if err != nil {
			s.logger.Warn("garden project: recent", "project", name, "error", err)
			partial("recent")
			return
		}
		doc.Recent = projectBeads(rows)
	})
	run(func() {
		q, err := s.decisionsQueue(ctx, true)
		if err != nil {
			s.logger.Warn("garden project: decisions", "project", name, "error", err)
			partial("decisions")
			return
		}
		doc.Decisions = projectQueueRows(q.Rows, name)
	})
	run(func() {
		q, err := s.escalationsQueue(ctx, true)
		if err != nil {
			s.logger.Warn("garden project: escalations", "project", name, "error", err)
			partial("escalations")
			return
		}
		doc.Escalations = projectQueueRows(q.Rows, name)
	})
	run(func() {
		summary, err := buildProjectSummary(ctx, s.daemon, name, now)
		if err != nil || summary == nil {
			s.logger.Warn("garden project: blocked", "project", name, "error", err)
			partial("blocked")
			return
		}
		for _, b := range summary.BlockedBeads {
			doc.Blocked = append(doc.Blocked, gardenhub.ProjectBead{ID: b.ID, Title: b.Title, Status: "blocked", Ago: b.Ago})
		}
	})
	wg.Wait()

	// A name that is no project and has nothing in it is not there: the panel says so rather than six empty tabs.
	if !isProject && len(doc.Agents)+len(doc.Gardeners)+len(doc.Recent)+len(doc.Decisions)+len(doc.Escalations) == 0 && len(doc.Partial) == 0 {
		return nil, &beadsapi.APIError{StatusCode: http.StatusNotFound, Message: "no such project"}
	}
	return doc, nil
}

// projectConfigEditable is viewerCanEditProjectConfig for a request's context: a roleless session (whose writes go
// with the static token, which the daemon accepts for update/project) or a gardenio-admin.
func projectConfigEditable(ctx context.Context) bool {
	roles := oidcauth.RolesFromContext(ctx)
	return len(roles) == 0 || oidcauth.HasMinimumRole(roles, oidcauth.RoleAdmin)
}

// gardenProjectConfig is the config tab's view of a type:project bead: the editor's own fields
// (projectConfigViewFromBead) and the hub's validity findings, with no account list, as the hub's badge reads them.
func gardenProjectConfig(name string, b *beadsapi.BeadDetail) *gardenhub.ProjectConfig {
	v := projectConfigViewFromBead(name, b)
	c := &gardenhub.ProjectConfig{
		BeadID: b.ID, Status: b.Status, GitURL: v.GitURL, DefaultBranch: v.DefaultBranch, Image: v.Image, Account: v.Account, Model: v.Model,
		CPURequest: v.CPURequest, CPULimit: v.CPULimit, MemoryRequest: v.MemoryRequest, MemoryLimit: v.MemoryLimit, StorageClass: v.StorageClass,
		CardTheme: v.CardTheme, BudgetUSD: v.BudgetUSD, BudgetPeriod: v.BudgetPeriod, Docker: v.DockerEnabled, Postgres: v.PostgresEnabled,
		NeedsK8sAPI: v.NeedsK8sAPI, SchedulesEnabled: v.SchedulesEnabled, TriggersEnabled: v.TriggersEnabled,
	}
	for _, e := range v.Secrets {
		c.Secrets = append(c.Secrets, gardenhub.ProjectSecret{Env: e.Env, Secret: e.Secret, Key: e.Key})
	}
	for _, f := range buildProjectValidityReport(b.Fields, nil).Checks {
		c.Findings = append(c.Findings, gardenhub.ProjectFinding{Field: f.Field, Severity: string(f.Severity), Message: f.Message})
	}
	return c
}

func usd(v float64) *float64 { return &v }

// projectBeads is the activity rows as the panel's beads.
func projectBeads(rows []indexRecentItem) []gardenhub.ProjectBead {
	out := make([]gardenhub.ProjectBead, 0, len(rows))
	for _, r := range rows {
		out = append(out, gardenhub.ProjectBead{ID: r.ID, Title: r.Title, Type: r.Type, Status: r.Status, Ago: r.Ago})
	}
	return out
}

// projectQueueRows is the Queues panel's rows of one project.
func projectQueueRows(rows []gardenhub.QueueRow, project string) []gardenhub.QueueRow {
	out := make([]gardenhub.QueueRow, 0)
	for _, r := range rows {
		if r.Project == project {
			out = append(out, r)
		}
	}
	return out
}

// landOwnerReader reads a land's people (GET /v1/lands/{land}/members) on the viewer's bearer. *beadsapi.Client
// implements it.
type landOwnerReader interface {
	LandMembers(ctx context.Context, land string) ([]beadsapi.LandMember, error)
}

// landOwner is a tenant land's owner for the project panel's owner row (plan gd-0SUIdKXWV3 step 3): the first of the
// land's people with role owner, read on the viewer's bearer, so kbeads answers only for a land the viewer may see. Nil
// when the daemon cannot say, or names none.
func landOwner(ctx context.Context, daemon any, land string, logger *slog.Logger) *gardenhub.ProjectPerson {
	pp, ok := daemon.(landOwnerReader)
	if !ok {
		return nil
	}
	members, err := pp.LandMembers(ctx, land)
	if err != nil {
		logger.Warn("garden project: land owner", "project", land, "error", err)
		return nil
	}
	for _, m := range members {
		if m.Role == "owner" && strings.HasPrefix(m.Principal, "oidc:") {
			return &gardenhub.ProjectPerson{Principal: m.Principal, Name: m.Name}
		}
	}
	return nil
}
