package main

import (
	"context"
	"net/http"
	"strings"
	"sync"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/gardenhub"
)

// gardenGardeners is the gardener panel's reader (gardenhub.GardenerReader, Lands workbench-glass spec WB-11, step 5.12):
// what /gardener/{id} shows, by the builders its page and fragments use: the header (gardenerViewFromBead, its goal state
// re-read live, its liveness from the life-thread), the life-thread (gardenerIterations) with each run's wrap-up as the
// invocations drawer reads it, the beads its runs touched (the /gardener/{id}/beads walk, by lane), the lifecycle bar's
// state (gardenerControlsViewFromBead), the config form's values (gardenerConfigViewFromBead) and its declared
// successors. All on the request's bearer, so the daemon cuts the answer as it cuts the page. A gardener the page would
// 404 (absent, an E2E fixture, a schedule mirror, or no gardener at all) is not there. The runs and the beads degrade
// alone: one the daemon did not answer is named in Partial.
type gardenGardeners struct{ s *Server }

// Gardener implements gardenhub.GardenerReader.
func (g gardenGardeners) Gardener(ctx context.Context, id string) (*gardenhub.GardenerDoc, error) {
	s := g.s
	b, err := s.daemon.GetBead(ctx, id)
	if err != nil {
		return nil, err
	}
	if b == nil || !isGardenerBead(b) || excludeFixtureGardener(b) || isScheduleMirrorGardenerBead(b) {
		return nil, &beadsapi.APIError{StatusCode: http.StatusNotFound, Message: "no such gardener"}
	}
	v, err := s.gardenerViewFromBead(ctx, b)
	if err != nil {
		return nil, err
	}
	s.populateLiveGoalState(ctx, &v, b)
	now := s.now()

	var (
		iters []gardenerIterationView
		ierr  error
		wraps map[string]string
	)
	iters, v.HistoryTruncated, ierr = s.gardenerIterations(ctx, id, now)
	if ierr != nil {
		s.logger.Warn("garden gardener: iterations", "id", id, "error", ierr)
	} else {
		v.IterationCount, v.DidNotRunCount, v.LiveCount = runIterationCount(iters), countDidNotRun(iters), countLiveIterations(iters)
		v.reconcileCounters()
		v.computeLiveness(freshestProgress(iters), now)
	}
	v.IterationsUnavailable = ierr != nil

	doc := gardenGardenerHeader(b, v)
	if ierr != nil {
		doc.Partial = append(doc.Partial, "runs", "beads")
		return doc, nil
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); wraps = s.invocationWrapupsByID(ctx, iters) }()
	var lanes []gardenerBeadLaneView
	go func() {
		defer wg.Done()
		seeds := make([]string, 0, len(iters)+1)
		seeds = append(seeds, id)
		runs := make(map[string]bool, len(iters))
		for _, it := range iters {
			seeds = append(seeds, it.ID)
			runs[it.ID] = true
		}
		nodes, _, _, _ := s.walkGardenerBeadGraph(ctx, seeds, gardenerBeadsDefaultDegree)
		touched := nodes[:0:0]
		for _, n := range nodes {
			if !runs[n.ID] { // its runs are the Runs tab; this one is what they touched
				touched = append(touched, n)
			}
		}
		lanes = gardenerBeadLanes(touched, id)
	}()
	wg.Wait()

	for _, it := range iters {
		doc.Runs = append(doc.Runs, gardenGardenerRun(it, wraps[it.ID]))
	}
	for _, l := range lanes {
		lane := gardenhub.GardenerLane{Lane: l.Lane, Label: l.Label, Count: l.Count, Beads: []gardenhub.ProjectBead{}}
		for _, n := range l.Nodes {
			lane.Beads = append(lane.Beads, gardenhub.ProjectBead{ID: n.ID, Title: n.Title, Type: n.Type, Status: n.Status})
		}
		doc.Beads = append(doc.Beads, lane)
	}
	return doc, nil
}

// gardenGardenerHeader is the panel's document from the gardener page's header, its lifecycle bar and its config form;
// the runs and beads are added by the caller.
func gardenGardenerHeader(b *beadsapi.BeadDetail, v gardenerView) *gardenhub.GardenerDoc {
	c := gardenerControlsViewFromBead(b)
	doc := &gardenhub.GardenerDoc{
		ID: b.ID, Title: b.Title, Name: v.Name, Project: beadProjectLabel(b), Status: b.Status,
		Enabled: c.Enabled, Closed: c.IsClosed, Standing: v.IsStanding, Mode: c.Mode, ModeAmbiguous: c.ModeAmbiguous,
		Goal: v.TerminalGoal, Prompt: v.Prompt, Chasing: v.Legible.ChasingGoal, OnLoopEnd: v.Legible.OnLoopEnd,
		Cron: v.Cron, Timezone: v.Timezone, NextFire: v.NextFireRelative, GoalState: v.GardenerGoalState(), Ended: v.TerminationReason(),
		RunsSpawned: v.RunsSpawned, RunsCompleted: v.RunsCompleted, MaxRuns: v.MaxRuns, RunBudget: v.RunBudget, Failures: v.ConsecutiveFailures,
		RunCount: v.IterationCount, LiveCount: v.LiveCount, DidNotRun: v.DidNotRunCount, EngineCount: v.EngineCount,
		Owner: v.OwnerDisplay, InFlight: c.HasInflightRun, LastRun: c.LastAgentID,
	}
	if v.HasSpend {
		doc.Spend, doc.SpendStale = v.SpendLabel, v.SpendStale
	}
	if v.Liveness.Overdue {
		doc.Overdue = firstNonEmptyStr(v.Liveness.OverdueDetail, "It should have fired again and has not.")
	}
	if v.Liveness.Silent {
		doc.Silent = firstNonEmptyStr(v.Liveness.SilentDetail, "It is firing but making no progress.")
	}
	for _, w := range v.Legible.Wakes {
		doc.Wakes = append(doc.Wakes, joinNonEmpty(w.Summary, w.Detail))
	}
	for _, st := range v.Legible.Stops {
		doc.Stops = append(doc.Stops, joinNonEmpty(st.SlotLabel, st.Summary))
	}
	for _, h := range parseChainSpec(b).Heirs() {
		doc.Successors = append(doc.Successors, successorAuditLabel(h))
	}
	f := gardenerConfigViewFromBead(b)
	doc.Config = &gardenhub.GardenerConfig{
		Prompt: f.Prompt, TerminalGoal: f.TerminalGoal, ExitCondition: f.ExitCondition, OnExit: f.OnExit, RunBudget: f.RunBudget, MaxRuns: f.MaxRuns,
		RetryOnFailure: f.RetryOnFailure, MaxRetries: f.MaxRetries, RunTimeout: f.RunTimeout, Timeout: f.Timeout, OneSpawnAtATime: f.OneSpawnAtATime,
		StopJSON: f.StopJSON, ProgressJSON: f.ProgressJSON, Conditions: f.Conditions, Cron: f.Cron, Cooldown: f.Cooldown, MaxConcurrent: f.MaxConcurrent,
	}
	return doc
}

// gardenGardenerRun is one life-thread run as the panel's row: its wrap-up's tl;dr as the headline when it has one (the
// invocations drawer's rule), its outcome once it ended, its cost and turns.
func gardenGardenerRun(it gardenerIterationView, wrapup string) gardenhub.GardenerRun {
	tldr, _ := parseWrapUpTLDRAndAccomplishments(wrapup)
	run := gardenhub.GardenerRun{
		ID: it.ID, Name: it.Name, Label: it.RunLabel, State: it.AgentState, Live: it.IsLive, DidNotRun: it.DidNotRun,
		Headline: firstNonEmptyStr(tldr, invocationHeadline(it.WrapUpSummary, it.DerivedOutcome, it.TaskTitle, it.Name, it.ID)),
		Ago:      it.Ago, TaskID: it.TaskID, TaskTitle: it.TaskTitle,
	}
	if !it.IsLive && !it.DidNotRun {
		run.Outcome = "ok"
		if it.RunOutcome == "fail" {
			run.Outcome = "fail"
		}
	}
	if it.Usage.HasCost {
		run.Cost = it.Usage.CostLabel
	}
	if it.Usage.HasTurns {
		run.Turns = it.Usage.TurnsLabel
	}
	return run
}

// joinNonEmpty is a and b joined by ": ", either alone when the other is empty.
func joinNonEmpty(a, b string) string {
	a, b = strings.TrimSpace(a), strings.TrimSpace(b)
	switch {
	case a == "":
		return b
	case b == "" || strings.EqualFold(a, b):
		return a
	}
	return a + ": " + b
}

// gardenGardenerPresets is the create form's presets (create_gardener.go) as the project panel offers them.
func gardenGardenerPresets() []gardenhub.GardenerPreset {
	ps := gardenerPresets()
	out := make([]gardenhub.GardenerPreset, 0, len(ps))
	for _, p := range ps {
		out = append(out, gardenhub.GardenerPreset{Key: p.Key, Label: p.Label, Description: p.Description})
	}
	return out
}
