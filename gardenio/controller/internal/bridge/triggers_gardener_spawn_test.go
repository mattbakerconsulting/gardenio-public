// Phase 3b/B6.1 (ka-0000005779): TriggerEngine actionSpawnAgent absorbs the
// scheduler-native spawn-time behavior — AssignBead pinning and loop-context
// field stamping — so gardeners routed through the bridge trigger engine
// behave identically to gardeners spawned by scheduler.spawnScheduledAgent.

package bridge

import (
	"context"
	"log/slog"
	"strconv"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// TestActionSpawnAgent_GardenerAssignBead_SkipsTaskCreation asserts that a
// gardener with AssignBead set spawns its agent against the pinned bead
// without creating a per-fire task and without writing triggered-by lineage
// onto the pinned bead. Mirrors
// scheduler.TestSpawnScheduledAgent_AssignBead_SkipsTaskCreation.
func TestActionSpawnAgent_GardenerAssignBead_SkipsTaskCreation(t *testing.T) {
	daemon, engine, _, _ := gardenerTrigger(t, GardenerSpec{
		AssignBead: "gd-epic-99",
		RunBudget:  5,
	})
	engine.Evaluate(context.Background())

	daemon.mu.Lock()
	defer daemon.mu.Unlock()

	if len(daemon.createdBeads) != 0 {
		t.Errorf("AssignBead set: expected zero CreateBead calls, got %d (titles=%v)",
			len(daemon.createdBeads), createdTitles(daemon.createdBeads))
	}
	if len(daemon.spawnedNames) != 1 {
		t.Fatalf("expected one spawn, got %d", len(daemon.spawnedNames))
	}
	if got := daemon.spawnedTaskIDs[0]; got != "gd-epic-99" {
		t.Errorf("agent task_id = %q, want %q", got, "gd-epic-99")
	}
	// triggered-by lineage must NOT be stamped on the assign bead — it's a
	// long-lived gardener home; per-fire deps would accumulate.
	for _, dep := range daemon.addedDeps {
		if dep.BeadID == "gd-epic-99" || dep.DependsOnID == "gd-epic-99" {
			t.Errorf("triggered-by lineage stamped on assign bead: %+v", dep)
		}
	}
}

// TestActionSpawnAgent_GardenerAssignBead_StampsLoopContextOnAgent asserts the
// AssignBead branch still stamps loop-context fields onto the spawned agent,
// since gb prime's "## Scheduled Loop Context" renderer keys off them.
func TestActionSpawnAgent_GardenerAssignBead_StampsLoopContextOnAgent(t *testing.T) {
	daemon, engine, _, _ := gardenerTrigger(t, GardenerSpec{
		AssignBead:    "gd-epic-99",
		Name:          "intake-gardener",
		TerminalGoal:  "drain the inbox",
		RunBudget:     5,
		MaxRuns:       10,
		RunsSpawned:   3,
		RunsCompleted: 2,
		LastAgentID:   "gd-prior-agent",
		LineageRoot:   "gd-root-agent",
		SlackChannel:  "C0AEJBFGQ4A",
		SlackUserID:   "U0OPERATOR1",
		UserBeadID:    "gd-user-1",
	})
	engine.Evaluate(context.Background())

	daemon.mu.Lock()
	defer daemon.mu.Unlock()

	if len(daemon.spawnedFields) != 1 {
		t.Fatalf("expected one spawn, got %d", len(daemon.spawnedFields))
	}
	got := daemon.spawnedFields[0]
	wantPairs := map[string]string{
		"schedule_id":             "gd-cap-1",
		"schedule_title":          "gardener trig",
		"schedule_name":           "intake-gardener",
		"schedule_terminal_goal":  "drain the inbox",
		"schedule_max_runs":       "10",
		"schedule_runs_spawned":   "4", // RunsSpawned + 1
		"schedule_runs_completed": "2",
		"schedule_prior_agent_id": "gd-prior-agent",
		// Neutral succession back-pointers (task ka-0000006931): written
		// ADDITIVELY alongside the untouched schedule_prior_agent_id.
		// prior_agent_id mirrors the prior iteration; lineage_root_id carries
		// the gardener's engine-owned LineageRoot.
		"prior_agent_id":         "gd-prior-agent",
		"lineage_root_id":        "gd-root-agent",
		"schedule_slack_channel": "C0AEJBFGQ4A",
		"slack_user_id":          "U0OPERATOR1",
		"user_bead_id":           "gd-user-1",
		"spawn_source":           "schedule",
		// Live-card footer navigability (ka-0000003131): resolved_by carries the
		// gardener's human-readable name (spec.Name) for display on the card.
		"resolved_by": "intake-gardener",
	}
	for k, want := range wantPairs {
		if got[k] != want {
			t.Errorf("agent field %q = %q, want %q", k, got[k], want)
		}
	}
	// schedule_prior_agent_id is LEFT UNTOUCHED (not migrated) — the additive
	// prior_agent_id does not replace it (acceptance criterion 2).
	if got["schedule_prior_agent_id"] == "" {
		t.Error("schedule_prior_agent_id must remain written, not migrated away")
	}
}

// TestActionSpawnAgent_Gardener_ResolvedByFallsBackToTitle asserts that when a
// gardener declares no name, resolved_by falls back to the trigger bead title so
// the card footer still shows a human-readable gardener (ka-0000003131).
func TestActionSpawnAgent_Gardener_ResolvedByFallsBackToTitle(t *testing.T) {
	daemon, engine, _, _ := gardenerTrigger(t, GardenerSpec{
		// Name intentionally empty — fall back to trig.Title ("gardener trig").
		AssignBead: "gd-epic-99",
		RunBudget:  5,
	})
	engine.Evaluate(context.Background())

	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	if len(daemon.spawnedFields) != 1 {
		t.Fatalf("expected one spawn, got %d", len(daemon.spawnedFields))
	}
	if got := daemon.spawnedFields[0]["resolved_by"]; got != "gardener trig" {
		t.Errorf("resolved_by = %q; want fallback to trigger title %q", got, "gardener trig")
	}
}

// TestActionSpawnAgent_GardenerNoAssignBead_CreatesTaskAndStampsLoopContext
// asserts that gardeners without AssignBead still create per-fire task beads
// (preserving the existing fall-back path the leak-fix MR 3018 cleans up),
// while still stamping loop-context onto the agent.
func TestActionSpawnAgent_GardenerNoAssignBead_CreatesTaskAndStampsLoopContext(t *testing.T) {
	daemon, engine, _, _ := gardenerTrigger(t, GardenerSpec{
		// AssignBead intentionally empty
		TerminalGoal: "loop forever",
		MaxRuns:      3,
		RunsSpawned:  0,
	})
	engine.Evaluate(context.Background())

	daemon.mu.Lock()
	defer daemon.mu.Unlock()

	if len(daemon.createdBeads) != 1 {
		t.Fatalf("no AssignBead: expected one per-fire task bead, got %d", len(daemon.createdBeads))
	}
	if len(daemon.spawnedFields) != 1 {
		t.Fatalf("expected one spawn, got %d", len(daemon.spawnedFields))
	}
	got := daemon.spawnedFields[0]
	if got["schedule_id"] != "gd-cap-1" {
		t.Errorf("expected schedule_id on agent, got %q", got["schedule_id"])
	}
	if got["schedule_runs_spawned"] != strconv.Itoa(1) {
		t.Errorf("schedule_runs_spawned = %q, want %q", got["schedule_runs_spawned"], "1")
	}
	if got["spawn_source"] != "schedule" {
		t.Errorf("spawn_source = %q, want \"schedule\"", got["spawn_source"])
	}
	// Single-claim guard (ka-0000004654): a per-fire task is single-claim work,
	// so the engine opts the daemon-side guard in.
	if got["single_claim_task"] != "1" {
		t.Errorf("per-fire gardener spawn: expected single_claim_task=1, got %q", got["single_claim_task"])
	}
}

// TestActionSpawnAgent_GardenerAssignBead_NoSingleClaimFlag asserts the
// AssignBead-pinned path does NOT opt into the single-claim guard: it
// intentionally reuses one long-lived task across runs under its own
// one_spawn_at_a_time gate, so the daemon-side single-claim dedup (which would
// always see the long-lived bead as "claimed") must stay off (ka-0000004654).
func TestActionSpawnAgent_GardenerAssignBead_NoSingleClaimFlag(t *testing.T) {
	daemon, engine, _, _ := gardenerTrigger(t, GardenerSpec{
		AssignBead: "gd-epic-99",
		RunBudget:  5,
	})
	engine.Evaluate(context.Background())

	daemon.mu.Lock()
	defer daemon.mu.Unlock()

	if len(daemon.spawnedFields) != 1 {
		t.Fatalf("expected one spawn, got %d", len(daemon.spawnedFields))
	}
	if got := daemon.spawnedFields[0]["single_claim_task"]; got != "" {
		t.Errorf("AssignBead path must NOT set single_claim_task, got %q", got)
	}
}

// TestActionSpawnAgent_NonGardener_NoLoopContext is the regression guard for
// jira/gitlab/webhook/slack-reaction source-only triggers (Gardener==nil).
// They must continue to create per-fire task beads and must NOT have any
// schedule_* fields stamped on their spawned agents.
func TestActionSpawnAgent_NonGardener_NoLoopContext(t *testing.T) {
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			{ID: "gd-plain", Type: "trigger", Status: "open", Title: "plain trig"},
		},
	}
	engine := NewTriggerEngine(TriggerEngineConfig{Daemon: daemon, Logger: slog.Default()})
	src := &mockTriggerSource{
		name: "mock",
		triggers: []TriggerDef{{
			ID:            "gd-plain",
			Title:         "plain trig",
			Source:        "mock",
			Action:        TriggerActionSpawnAgent,
			Project:       "monorepo",
			Role:          "crew",
			Prompt:        "do the thing",
			MaxConcurrent: 1,
			LastChecked:   pastLastChecked(),
			// Gardener: nil
		}},
		matches: map[string][]TriggerMatch{
			"gd-plain": {{Key: "M1", Title: "match one"}},
		},
	}
	engine.RegisterSource(src)
	engine.Evaluate(context.Background())

	daemon.mu.Lock()
	defer daemon.mu.Unlock()

	if len(daemon.createdBeads) != 1 {
		t.Fatalf("non-gardener trigger: expected per-fire task bead, got %d", len(daemon.createdBeads))
	}
	if len(daemon.spawnedFields) != 1 {
		t.Fatalf("expected one spawn, got %d", len(daemon.spawnedFields))
	}
	got := daemon.spawnedFields[0]
	forbidden := []string{
		"schedule_id",
		"schedule_title",
		"schedule_runs_spawned",
		"schedule_max_runs",
		"schedule_terminal_goal",
		"schedule_prior_agent_id",
		// The neutral succession pointers are gardener-loop-context too — a
		// non-gardener trigger spawn (which has no LastAgentID/LineageRoot chain)
		// must not carry them either (task ka-0000006931).
		"prior_agent_id",
		"lineage_root_id",
		"spawn_source",
	}
	for _, k := range forbidden {
		if _, ok := got[k]; ok {
			t.Errorf("non-gardener agent must not carry %q (got %q)", k, got[k])
		}
	}
	// Single-claim guard (ka-0000004654): non-gardener per-fire spawns target a
	// freshly-created task and are single-claim, so the flag is set.
	if got["single_claim_task"] != "1" {
		t.Errorf("non-gardener per-fire spawn: expected single_claim_task=1, got %q", got["single_claim_task"])
	}
}

// TestActionSpawnAgent_ExecutionTarget_StampsAgentField asserts a trigger
// carrying execution_target stamps it verbatim onto the spawned agent bead —
// the per-trigger runtime-backend opt-in (ka-0000003026): one gardener routes
// its iterations to lambda_microvm without a role/project flip. The laptop
// marker is the exception: it is reserved for gb runner sessions, which the
// controller never runs a pod for, so it is dropped (gd-SzPJDNTBXS).
func TestActionSpawnAgent_ExecutionTarget_StampsAgentField(t *testing.T) {
	for target, want := range map[string]string{
		"lambda_microvm": "lambda_microvm",
		"local":          "local",
		"laptop":         "",
	} {
		t.Run(target, func(t *testing.T) {
			daemon := &mockTriggerDaemon{
				beads: []*beadsapi.BeadDetail{
					{ID: "gd-mvm", Type: "trigger", Status: "open", Title: "microvm trig"},
				},
			}
			engine := NewTriggerEngine(TriggerEngineConfig{Daemon: daemon, Logger: slog.Default()})
			src := &mockTriggerSource{
				name: "mock",
				triggers: []TriggerDef{{
					ID:              "gd-mvm",
					Title:           "microvm trig",
					Source:          "mock",
					Action:          TriggerActionSpawnAgent,
					Project:         "gardenio",
					Role:            "plan-gardener",
					Prompt:          "do the thing",
					MaxConcurrent:   1,
					LastChecked:     pastLastChecked(),
					ExecutionTarget: target,
				}},
				matches: map[string][]TriggerMatch{
					"gd-mvm": {{Key: "M1", Title: "match one"}},
				},
			}
			engine.RegisterSource(src)
			engine.Evaluate(context.Background())

			daemon.mu.Lock()
			defer daemon.mu.Unlock()

			if len(daemon.spawnedFields) != 1 {
				t.Fatalf("expected one spawn, got %d", len(daemon.spawnedFields))
			}
			if got := daemon.spawnedFields[0]["execution_target"]; got != want {
				t.Errorf("agent field execution_target = %q, want %q", got, want)
			}
		})
	}
}

// TestActionSpawnAgent_NoExecutionTarget_NoAgentField is the inverse guard:
// a trigger without execution_target must not stamp the field at all, so the
// controller's role/project precedence layers stay in charge.
func TestActionSpawnAgent_NoExecutionTarget_NoAgentField(t *testing.T) {
	daemon, engine := triggerSpawnHarness(t, "slack:U0OPERATOR1")
	engine.Evaluate(context.Background())

	daemon.mu.Lock()
	defer daemon.mu.Unlock()

	if len(daemon.spawnedFields) != 1 {
		t.Fatalf("expected one spawn, got %d", len(daemon.spawnedFields))
	}
	if got, ok := daemon.spawnedFields[0]["execution_target"]; ok {
		t.Errorf("agent must not carry execution_target when the trigger sets none (got %q)", got)
	}
}

// TestActionSpawnAgent_SlackChannel_StampsScheduleChannel asserts a plain
// (non-gardener) schedule trigger carrying a top-level slack_channel stamps it
// onto the spawned agent bead as schedule_slack_channel — the field the
// slack-bridge routes the agent's card/thread/squawks off (bot_agents.go). This
// is the daily-standup-poster case: a bare `action: spawn_agent` schedule that
// needs its recurring posts to reach a non-project channel (ka-0000003647).
func TestActionSpawnAgent_SlackChannel_StampsScheduleChannel(t *testing.T) {
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			{ID: "gd-sched", Type: "trigger", Status: "open", Title: "daily standup"},
		},
	}
	engine := NewTriggerEngine(TriggerEngineConfig{Daemon: daemon, Logger: slog.Default()})
	src := &mockTriggerSource{
		name: "schedule",
		triggers: []TriggerDef{{
			ID:            "gd-sched",
			Title:         "daily standup",
			Source:        "schedule",
			Action:        TriggerActionSpawnAgent,
			Project:       "gardenio",
			Role:          "crew",
			Prompt:        "post the standup",
			MaxConcurrent: 1,
			LastChecked:   pastLastChecked(),
			SlackChannel:  "C0DAILY0STANDUP",
			// Gardener: nil — this is the plain-schedule path.
		}},
		matches: map[string][]TriggerMatch{
			"gd-sched": {{Key: "M1", Title: "match one"}},
		},
	}
	engine.RegisterSource(src)
	engine.Evaluate(context.Background())

	daemon.mu.Lock()
	defer daemon.mu.Unlock()

	if len(daemon.spawnedFields) != 1 {
		t.Fatalf("expected one spawn, got %d", len(daemon.spawnedFields))
	}
	if got := daemon.spawnedFields[0]["schedule_slack_channel"]; got != "C0DAILY0STANDUP" {
		t.Errorf("agent field schedule_slack_channel = %q, want %q (plain schedule top-level slack_channel)", got, "C0DAILY0STANDUP")
	}
}

// TestActionSpawnAgent_NoSlackChannel_NoScheduleChannel is the inverse guard: a
// trigger with no slack_channel must not stamp schedule_slack_channel, so the
// slack-bridge falls back to the project default channel (ka-0000003647).
func TestActionSpawnAgent_NoSlackChannel_NoScheduleChannel(t *testing.T) {
	daemon, engine := triggerSpawnHarness(t, "slack:U0OPERATOR1")
	engine.Evaluate(context.Background())

	daemon.mu.Lock()
	defer daemon.mu.Unlock()

	if len(daemon.spawnedFields) != 1 {
		t.Fatalf("expected one spawn, got %d", len(daemon.spawnedFields))
	}
	if got, ok := daemon.spawnedFields[0]["schedule_slack_channel"]; ok {
		t.Errorf("agent must not carry schedule_slack_channel when the trigger sets none (got %q)", got)
	}
}

// TestActionSpawnAgent_GardenerSlackChannel_WinsOverTopLevel asserts the
// documented precedence: when a gardener trigger carries a slack_channel BOTH at
// the top level and inside its gardener block, the gardener block wins because
// stampGardenerLoopContext stamps spec.SlackChannel after the top-level stamp
// (ka-0000003647).
func TestActionSpawnAgent_GardenerSlackChannel_WinsOverTopLevel(t *testing.T) {
	daemon, engine, src, _ := gardenerTrigger(t, GardenerSpec{
		AssignBead:   "gd-epic-99",
		RunBudget:    5,
		SlackChannel: "C0GARDENER0CHAN",
	})
	// Also set a DIFFERENT top-level slack_channel on the same trigger.
	src.triggers[0].SlackChannel = "C0TOPLEVEL0CHAN"
	engine.Evaluate(context.Background())

	daemon.mu.Lock()
	defer daemon.mu.Unlock()

	if len(daemon.spawnedFields) != 1 {
		t.Fatalf("expected one spawn, got %d", len(daemon.spawnedFields))
	}
	if got := daemon.spawnedFields[0]["schedule_slack_channel"]; got != "C0GARDENER0CHAN" {
		t.Errorf("schedule_slack_channel = %q, want gardener block channel to win over top-level", got)
	}
}

// createdTitles is a small assertion helper that pulls titles out of a slice
// of CreateBead requests for readable failure messages.
func createdTitles(reqs []beadsapi.CreateBeadRequest) []string {
	out := make([]string, 0, len(reqs))
	for _, r := range reqs {
		out = append(out, r.Title)
	}
	return out
}

// triggerSpawnHarness builds a non-gardener spawn_agent trigger whose trigger
// bead carries the given typed operator (OperatedBy). Used by the
// human-operator gate tests (task ka-0000002839).
func triggerSpawnHarness(t *testing.T, operatedBy string) (*mockTriggerDaemon, *TriggerEngine) {
	t.Helper()
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			{ID: "gd-plain", Type: "trigger", Status: "open", Title: "plain trig"},
		},
	}
	engine := NewTriggerEngine(TriggerEngineConfig{Daemon: daemon, Logger: slog.Default()})
	src := &mockTriggerSource{
		name: "mock",
		triggers: []TriggerDef{{
			ID:            "gd-plain",
			Title:         "plain trig",
			Source:        "mock",
			Action:        TriggerActionSpawnAgent,
			Project:       "monorepo",
			Role:          "crew",
			Prompt:        "do the thing",
			MaxConcurrent: 1,
			LastChecked:   pastLastChecked(),
			OperatedBy:    operatedBy,
		}},
		matches: map[string][]TriggerMatch{
			"gd-plain": {{Key: "M1", Title: "match one"}},
		},
	}
	engine.RegisterSource(src)
	return daemon, engine
}

// TestActionSpawnAgent_HumanOperator_StampsHumanFromTrigger asserts a trigger
// authored by a human stamps that human onto the spawned agent's operated_by.
func TestActionSpawnAgent_HumanOperator_StampsHumanFromTrigger(t *testing.T) {
	daemon, engine := triggerSpawnHarness(t, "slack:U0OPERATOR1")
	engine.Evaluate(context.Background())

	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	if len(daemon.spawnedFields) != 1 {
		t.Fatalf("expected one spawn, got %d", len(daemon.spawnedFields))
	}
	if got := daemon.spawnedFields[0]["operated_by"]; got != "slack:U0OPERATOR1" {
		t.Errorf("operated_by = %q; want slack:U0OPERATOR1 inherited from trigger", got)
	}
	// Live-card footer navigability (ka-0000003131): a HUMAN operator is also
	// mirrored onto `owner` so the slack-bridge can render "Owner: <human>".
	if got := daemon.spawnedFields[0]["owner"]; got != "slack:U0OPERATOR1" {
		t.Errorf("owner = %q; want slack:U0OPERATOR1 mirrored from the human operator", got)
	}
}

// TestActionSpawnAgent_NonHuman_NoOwner asserts a non-human operator is NOT
// mirrored onto `owner` — a synthetic/non-human id would render a meaningless
// owner line on the card (ka-0000003131).
func TestActionSpawnAgent_NonHuman_NoOwner(t *testing.T) {
	daemon, engine := triggerSpawnHarness(t, "jira:557058")
	engine.Evaluate(context.Background())

	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	if len(daemon.spawnedFields) != 1 {
		t.Fatalf("expected one spawn, got %d", len(daemon.spawnedFields))
	}
	if got := daemon.spawnedFields[0]["owner"]; got != "" {
		t.Errorf("non-human operator: owner must not be stamped, got %q", got)
	}
}

// TestActionSpawnAgent_Warn_AllowsNonHuman asserts the default (warn) mode
// still spawns when the trigger resolves no human, but does NOT stamp a
// non-human operated_by (the daemon would fail closed on it).
func TestActionSpawnAgent_Warn_AllowsNonHuman(t *testing.T) {
	// Default mode is warn (env unset). A jira:<accountId> operator is typed
	// but not human.
	daemon, engine := triggerSpawnHarness(t, "jira:557058")
	engine.Evaluate(context.Background())

	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	if len(daemon.spawnedNames) != 1 {
		t.Fatalf("warn mode: expected one spawn, got %d", len(daemon.spawnedNames))
	}
	if got := daemon.spawnedFields[0]["operated_by"]; got != "" {
		t.Errorf("warn mode: non-human operated_by must not be stamped, got %q", got)
	}
}

// TestActionSpawnAgent_Enforce_RefusesNoHuman asserts enforce mode refuses the
// spawn entirely when no human operator is resolvable.
func TestActionSpawnAgent_Enforce_RefusesNoHuman(t *testing.T) {
	t.Setenv("GARDENIO_REQUIRE_HUMAN_OPERATOR", "enforce")
	daemon, engine := triggerSpawnHarness(t, "") // no operator at all
	engine.Evaluate(context.Background())

	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	if len(daemon.spawnedNames) != 0 {
		t.Errorf("enforce mode: expected no spawn with no human operator, got %d", len(daemon.spawnedNames))
	}
}

// TestActionSpawnAgent_Enforce_AllowsHuman asserts enforce mode still spawns
// when a human operator IS resolvable.
func TestActionSpawnAgent_Enforce_AllowsHuman(t *testing.T) {
	t.Setenv("GARDENIO_REQUIRE_HUMAN_OPERATOR", "enforce")
	daemon, engine := triggerSpawnHarness(t, "oidc:matthew.baker@acme.example")
	engine.Evaluate(context.Background())

	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	if len(daemon.spawnedFields) != 1 {
		t.Fatalf("enforce mode: expected one spawn for human operator, got %d", len(daemon.spawnedFields))
	}
	if got := daemon.spawnedFields[0]["operated_by"]; got != "oidc:matthew.baker@acme.example" {
		t.Errorf("operated_by = %q; want the human operator", got)
	}
}
