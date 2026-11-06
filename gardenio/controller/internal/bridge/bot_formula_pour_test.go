package bridge

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"

	"github.com/slack-go/slack"
)

func seedFormula(daemon *mockDaemon, id, title string, vars []formulaVarDef, steps []formulaStep) {
	varsJSON, _ := json.Marshal(vars)
	stepsJSON, _ := json.Marshal(steps)
	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	daemon.beads[id] = &beadsapi.BeadDetail{
		ID:     id,
		Title:  title,
		Type:   "formula",
		Status: "open",
		Fields: map[string]string{
			"vars":  string(varsJSON),
			"steps": string(stepsJSON),
		},
	}
}

func TestInstantiateFormulaSteps_StepAssignee(t *testing.T) {
	daemon := newFormulaMockDaemon()
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.daemon = daemon

	formula := &beadsapi.BeadDetail{
		ID:    "gd-formula-assign",
		Title: "Assignee test",
	}

	steps := []formulaStep{
		{ID: "step1", Title: "Assigned step", Assignee: "bot-123"},
		{ID: "step2", Title: "Unassigned step"},
	}

	_, _, err := bot.instantiateFormulaSteps(
		context.Background(), formula, steps, nil, false, "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	taskBeads := filterTaskBeads(daemon.beads)
	if len(taskBeads) != 2 {
		t.Fatalf("expected 2 task beads, got %d", len(taskBeads))
	}

	var foundAssigned, foundUnassigned bool
	for _, b := range taskBeads {
		if b.Assignee == "bot-123" {
			foundAssigned = true
		}
		if b.Assignee == "" && b.Title == "Unassigned step" {
			foundUnassigned = true
		}
	}
	if !foundAssigned {
		t.Error("expected one step with assignee 'bot-123'")
	}
	if !foundUnassigned {
		t.Error("expected one step with no assignee")
	}
}

func TestHandleFormulaPour_RequiredVarMissing(t *testing.T) {
	daemon := newMockDaemon()
	seedFormula(daemon, "gd-f1", "Deploy",
		[]formulaVarDef{{Name: "env", Required: true}},
		[]formulaStep{{ID: "step1", Title: "Deploy to {{env}}"}},
	)
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.handleFormulaPour(context.Background(),
		slack.SlashCommand{ChannelID: "C1", UserID: "U1"},
		[]string{"gd-f1"}, false)

	// No molecule should be created — required var is missing.
	molBeads := filterBeadsByType(daemon.beads, "molecule")
	if len(molBeads) != 0 {
		t.Errorf("expected no molecule when required var missing, got %d", len(molBeads))
	}
}

func TestHandleFormulaPour_VarDefault(t *testing.T) {
	daemon := newFormulaMockDaemon()
	seedFormula(daemon.mockDaemon, "gd-f2", "Deploy {{env}}",
		[]formulaVarDef{{Name: "env", Default: "staging"}},
		[]formulaStep{{ID: "step1", Title: "Deploy to {{env}}"}},
	)
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.daemon = daemon

	bot.handleFormulaPour(context.Background(),
		slack.SlashCommand{ChannelID: "C1", UserID: "U1"},
		[]string{"gd-f2"}, false)

	// Wait briefly for the async goroutine.
	assertEventually(t, func() bool {
		daemon.mu.Lock()
		defer daemon.mu.Unlock()
		for _, b := range daemon.beads {
			if b.Type == "molecule" {
				return true
			}
		}
		return false
	}, "expected molecule to be created with default var")
}

func TestHandleFormulaPour_EnumValidation(t *testing.T) {
	daemon := newMockDaemon()
	seedFormula(daemon, "gd-f3", "Deploy",
		[]formulaVarDef{{Name: "env", Enum: []string{"staging", "prod"}}},
		[]formulaStep{{ID: "step1", Title: "Deploy"}},
	)
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.handleFormulaPour(context.Background(),
		slack.SlashCommand{ChannelID: "C1", UserID: "U1"},
		[]string{"gd-f3", "--var", "env=invalid"}, false)

	// No molecule should be created — enum validation should fail.
	molBeads := filterBeadsByType(daemon.beads, "molecule")
	if len(molBeads) != 0 {
		t.Errorf("expected no molecule for invalid enum value, got %d", len(molBeads))
	}
}

func TestHandleFormulaPour_ValidEnum(t *testing.T) {
	daemon := newFormulaMockDaemon()
	seedFormula(daemon.mockDaemon, "gd-f4", "Deploy",
		[]formulaVarDef{{Name: "env", Enum: []string{"staging", "prod"}}},
		[]formulaStep{{ID: "step1", Title: "Deploy to {{env}}"}},
	)
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.daemon = daemon

	bot.handleFormulaPour(context.Background(),
		slack.SlashCommand{ChannelID: "C1", UserID: "U1"},
		[]string{"gd-f4", "--var", "env=prod"}, false)

	assertEventually(t, func() bool {
		daemon.mu.Lock()
		defer daemon.mu.Unlock()
		for _, b := range daemon.beads {
			if b.Type == "molecule" {
				return true
			}
		}
		return false
	}, "expected molecule to be created with valid enum value")
}

func TestHandleFormulaPour_NoSteps(t *testing.T) {
	daemon := newMockDaemon()
	seedFormula(daemon, "gd-f5", "Empty formula",
		nil,
		[]formulaStep{},
	)
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.handleFormulaPour(context.Background(),
		slack.SlashCommand{ChannelID: "C1", UserID: "U1"},
		[]string{"gd-f5"}, false)

	molBeads := filterBeadsByType(daemon.beads, "molecule")
	if len(molBeads) != 0 {
		t.Errorf("expected no molecule for formula with no steps, got %d", len(molBeads))
	}
}

func TestHandleFormulaPour_ConditionalStepFiltering(t *testing.T) {
	daemon := newFormulaMockDaemon()
	seedFormula(daemon.mockDaemon, "gd-f6", "Conditional",
		[]formulaVarDef{{Name: "deploy", Default: "false"}},
		[]formulaStep{
			{ID: "build", Title: "Build"},
			{ID: "deploy", Title: "Deploy", Condition: "{{deploy}} == true"},
			{ID: "notify", Title: "Notify", DependsOn: []string{"deploy"}},
		},
	)
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.daemon = daemon

	bot.handleFormulaPour(context.Background(),
		slack.SlashCommand{ChannelID: "C1", UserID: "U1"},
		[]string{"gd-f6"}, false)

	// handleFormulaPour creates the molecule and its step beads in an async
	// goroutine, one bead at a time. Waiting only for the molecule to appear
	// (as the old predicate did) races the still-pending step writes: the
	// subsequent one-shot filterTaskBeads count could observe got=1 while the
	// `notify` step write was still in flight — the flake in ka-0000008180.
	// Poll for BOTH steps to be persisted (deploy skipped, so build + notify)
	// before asserting, and read the map under the lock the writers hold.
	// Same async-timing class as the sibling fix in 89a2e61c5 (gd-eGuZwhTBm8).
	assertEventually(t, func() bool {
		daemon.mu.Lock()
		defer daemon.mu.Unlock()
		var molecule, tasks int
		for _, b := range daemon.beads {
			switch b.Type {
			case "molecule":
				molecule++
			case "task":
				tasks++
			}
		}
		// deploy step is skipped (condition=false); notify's dependency on
		// deploy is removed. Expect exactly build + notify.
		return molecule >= 1 && tasks == 2
	}, "expected molecule + 2 steps (build + notify, deploy skipped)")

	// Final synchronous assertion for a precise failure message if the poll
	// timed out on a wrong count (read under the lock the writers hold).
	daemon.mu.Lock()
	taskBeads := filterTaskBeads(daemon.beads)
	daemon.mu.Unlock()
	if len(taskBeads) != 2 {
		t.Errorf("expected 2 steps (build + notify, deploy skipped), got %d", len(taskBeads))
	}
}

func TestHandleFormulaPour_AllStepsConditionallySkipped(t *testing.T) {
	daemon := newMockDaemon()
	seedFormula(daemon, "gd-f7", "All skipped",
		[]formulaVarDef{{Name: "flag", Default: "no"}},
		[]formulaStep{
			{ID: "step1", Title: "Step 1", Condition: "{{flag}} == yes"},
		},
	)
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.handleFormulaPour(context.Background(),
		slack.SlashCommand{ChannelID: "C1", UserID: "U1"},
		[]string{"gd-f7"}, false)

	// Should not create any molecule — all steps filtered.
	molBeads := filterBeadsByType(daemon.beads, "molecule")
	if len(molBeads) != 0 {
		t.Errorf("expected no molecule when all steps are conditionally skipped, got %d", len(molBeads))
	}
}

func TestHandleFormulaPour_VarSubstitutionInStep(t *testing.T) {
	daemon := newFormulaMockDaemon()
	seedFormula(daemon.mockDaemon, "gd-f8", "Deploy {{env}}",
		[]formulaVarDef{{Name: "env", Default: "staging"}, {Name: "region", Default: "us-east-1"}},
		[]formulaStep{
			{ID: "step1", Title: "Deploy to {{env}} in {{region}}", Description: "Target: {{env}}/{{region}}"},
		},
	)
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.daemon = daemon

	bot.handleFormulaPour(context.Background(),
		slack.SlashCommand{ChannelID: "C1", UserID: "U1"},
		[]string{"gd-f8", "--var", "env=prod"}, false)

	assertEventually(t, func() bool {
		daemon.mu.Lock()
		defer daemon.mu.Unlock()
		for _, b := range daemon.beads {
			if b.Type == "task" && b.Title == "Deploy to prod in us-east-1" {
				return true
			}
		}
		return false
	}, "expected step title to have vars substituted: 'Deploy to prod in us-east-1'")
}

func TestHandleFormulaPour_VarEqualsFlag(t *testing.T) {
	daemon := newFormulaMockDaemon()
	seedFormula(daemon.mockDaemon, "gd-f9", "Test",
		[]formulaVarDef{{Name: "env"}},
		[]formulaStep{{ID: "step1", Title: "Deploy to {{env}}"}},
	)
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.daemon = daemon

	bot.handleFormulaPour(context.Background(),
		slack.SlashCommand{ChannelID: "C1", UserID: "U1"},
		[]string{"gd-f9", "--var=env=prod"}, false)

	assertEventually(t, func() bool {
		daemon.mu.Lock()
		defer daemon.mu.Unlock()
		for _, b := range daemon.beads {
			if b.Type == "task" && b.Title == "Deploy to prod" {
				return true
			}
		}
		return false
	}, "expected --var=env=prod to parse correctly")
}

// --- resolveFormula tests ---

func TestResolveFormula_ByID(t *testing.T) {
	daemon := newMockDaemon()
	seedFormula(daemon, "gd-f10", "My Formula", nil, nil)
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	f, err := bot.resolveFormula(context.Background(), "gd-f10")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if f.Title != "My Formula" {
		t.Errorf("expected title 'My Formula', got %q", f.Title)
	}
}

func TestResolveFormula_ByIDNotFormula(t *testing.T) {
	daemon := newMockDaemon()
	daemon.mu.Lock()
	daemon.beads["gd-task-1"] = &beadsapi.BeadDetail{
		ID:   "gd-task-1",
		Type: "task",
	}
	daemon.mu.Unlock()

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	_, err := bot.resolveFormula(context.Background(), "gd-task-1")
	if err == nil {
		t.Fatal("expected error when resolving non-formula bead by ID")
	}
}

func TestResolveFormula_ByNameSearch(t *testing.T) {
	daemon := newMockDaemon()
	seedFormula(daemon, "gd-f11", "Deployment Pipeline", nil, nil)
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	f, err := bot.resolveFormula(context.Background(), "Deployment Pipeline")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if f.ID != "gd-f11" {
		t.Errorf("expected ID gd-f11, got %s", f.ID)
	}
}

func TestResolveFormula_NotFound(t *testing.T) {
	daemon := newMockDaemon()
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	_, err := bot.resolveFormula(context.Background(), "nonexistent")
	if err == nil {
		t.Fatal("expected error when formula not found")
	}
}

// --- handleFormulaCommand routing tests ---

func TestHandleFormulaCommand_Routing(t *testing.T) {
	tests := []struct {
		name    string
		text    string
		wantErr bool
	}{
		{"empty defaults to list", "", false},
		{"explicit list", "list", false},
		{"ls alias", "ls", false},
		{"help", "help", false},
		{"unknown", "unknown", false},
		{"show no args", "show", false},
		{"pour no args", "pour", false},
		{"wisp no args", "wisp", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			daemon := newMockDaemon()
			slackSrv := newFakeSlackServer(t)
			defer slackSrv.Close()
			bot := newTestBot(daemon, slackSrv)

			// Should not panic.
			bot.handleFormulaCommand(context.Background(), slack.SlashCommand{
				Command:   "/formula",
				Text:      tt.text,
				ChannelID: "C1",
				UserID:    "U1",
			})
		})
	}
}

// --- formula pour agent spawn tests ---

func TestHandleFormulaPour_SpawnsAgent(t *testing.T) {
	daemon := newFormulaMockDaemon()
	daemon.seedProjectWithChannel("gardenio", "C1")
	seedFormula(daemon.mockDaemon, "gd-spawn1", "Deploy service",
		nil,
		[]formulaStep{{ID: "step1", Title: "Build"}},
	)
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.daemon = daemon

	bot.handleFormulaPour(context.Background(),
		slack.SlashCommand{ChannelID: "C1", UserID: "U1"},
		[]string{"gd-spawn1"}, false)

	// Wait for the async goroutine to create the molecule and spawn an agent.
	assertEventually(t, func() bool {
		daemon.mu.Lock()
		defer daemon.mu.Unlock()
		for _, b := range daemon.beads {
			if b.Type == "agent" {
				return true
			}
		}
		return false
	}, "expected an agent bead to be spawned after formula pour")

	// Verify the agent bead fields.
	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	var agentBead *beadsapi.BeadDetail
	var molBead *beadsapi.BeadDetail
	for _, b := range daemon.beads {
		if b.Type == "agent" {
			agentBead = b
		}
		if b.Type == "molecule" {
			molBead = b
		}
	}
	if agentBead == nil {
		t.Fatal("no agent bead found")
	}
	if molBead == nil {
		t.Fatal("no molecule bead found")
	}
	if agentBead.Fields["project"] != "gardenio" {
		t.Errorf("expected agent project=gardenio, got %q", agentBead.Fields["project"])
	}
	if agentBead.Fields["task_id"] != molBead.ID {
		t.Errorf("expected agent task_id=%s, got %q", molBead.ID, agentBead.Fields["task_id"])
	}
}

func TestHandleFormulaPour_UsesAssignedAgent(t *testing.T) {
	daemon := newFormulaMockDaemon()
	daemon.seedProjectWithChannel("gardenio", "C1")

	// Seed formula with assigned_agent field.
	varsJSON, _ := json.Marshal([]formulaVarDef{})
	stepsJSON, _ := json.Marshal([]formulaStep{{ID: "step1", Title: "Do work"}})
	daemon.mu.Lock()
	daemon.beads["gd-spawn2"] = &beadsapi.BeadDetail{
		ID:     "gd-spawn2",
		Title:  "Custom Agent Formula",
		Type:   "formula",
		Status: "open",
		Fields: map[string]string{
			"vars":           string(varsJSON),
			"steps":          string(stepsJSON),
			"assigned_agent": "my-custom-agent",
		},
	}
	daemon.mu.Unlock()

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.daemon = daemon

	bot.handleFormulaPour(context.Background(),
		slack.SlashCommand{ChannelID: "C1", UserID: "U1"},
		[]string{"gd-spawn2"}, false)

	assertEventually(t, func() bool {
		daemon.mu.Lock()
		defer daemon.mu.Unlock()
		for _, b := range daemon.beads {
			if b.Type == "agent" {
				return true
			}
		}
		return false
	}, "expected an agent bead to be spawned with assigned_agent name")

	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	for _, b := range daemon.beads {
		if b.Type == "agent" {
			if b.Fields["agent"] != "my-custom-agent" {
				t.Errorf("expected agent name 'my-custom-agent', got %q", b.Fields["agent"])
			}
			return
		}
	}
}

func TestHandleFormulaPour_NoSpawnWithoutProject(t *testing.T) {
	daemon := newFormulaMockDaemon()
	// No project seeded — projectFromChannel will return "".
	seedFormula(daemon.mockDaemon, "gd-spawn3", "No project formula",
		nil,
		[]formulaStep{{ID: "step1", Title: "Build"}},
	)
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.daemon = daemon

	bot.handleFormulaPour(context.Background(),
		slack.SlashCommand{ChannelID: "C-unknown", UserID: "U1"},
		[]string{"gd-spawn3"}, false)

	// Wait for molecule to be created.
	assertEventually(t, func() bool {
		daemon.mu.Lock()
		defer daemon.mu.Unlock()
		for _, b := range daemon.beads {
			if b.Type == "molecule" {
				return true
			}
		}
		return false
	}, "expected molecule to be created")

	// Give a moment for any async agent spawn to happen.
	time.Sleep(100 * time.Millisecond)

	// Verify no agent was spawned.
	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	for _, b := range daemon.beads {
		if b.Type == "agent" {
			t.Error("expected no agent bead when project is empty")
		}
	}
}

func TestHandleFormulaPour_AgentNameFromFormulaTitle(t *testing.T) {
	daemon := newFormulaMockDaemon()
	daemon.seedProjectWithChannel("gardenio", "C1")
	seedFormula(daemon.mockDaemon, "gd-spawn4", "Deploy {{env}} service",
		[]formulaVarDef{{Name: "env", Default: "staging"}},
		[]formulaStep{{ID: "step1", Title: "Build"}},
	)
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.daemon = daemon

	bot.handleFormulaPour(context.Background(),
		slack.SlashCommand{ChannelID: "C1", UserID: "U1"},
		[]string{"gd-spawn4"}, false)

	assertEventually(t, func() bool {
		daemon.mu.Lock()
		defer daemon.mu.Unlock()
		for _, b := range daemon.beads {
			if b.Type == "agent" {
				return true
			}
		}
		return false
	}, "expected agent bead to be spawned")

	// Agent name should be derived from the var-substituted title "Deploy staging service".
	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	for _, b := range daemon.beads {
		if b.Type == "agent" {
			name := b.Fields["agent"]
			// generateAgentName("Deploy staging service") produces "deploy-staging-service-XXX"
			if len(name) == 0 {
				t.Error("expected non-empty agent name")
			}
			if name == "Deploy staging service" {
				t.Error("expected agent name to be slugified, not raw title")
			}
			return
		}
	}
}

// TestHandleFormulaPour_FromThreadAddsTriggerBeadDep verifies the D3 contract:
// when `/formula pour` is issued from inside a Slack thread, the resulting
// molecule has a `triggered-by` dep pointing at the canonical thread bead.
// This is the end-to-end glue between the socketmode payload (thread_ts) and
// the lineage edge written by instantiateFormulaSteps.
func TestHandleFormulaPour_FromThreadAddsTriggerBeadDep(t *testing.T) {
	daemon := newFormulaMockDaemon()
	daemon.seedProjectWithChannel("gardenio", "C1")
	seedFormula(daemon.mockDaemon, "gd-thr1", "Thread-triggered formula",
		nil,
		[]formulaStep{{ID: "step1", Title: "Build"}},
	)
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.daemon = daemon

	ctx := WithSlashThreadTS(context.Background(), "1700000000.123456")
	bot.handleFormulaPour(ctx,
		slack.SlashCommand{ChannelID: "C1", UserID: "U1"},
		[]string{"gd-thr1"}, false)

	// Wait for the async goroutine to create the molecule.
	assertEventually(t, func() bool {
		daemon.mu.Lock()
		defer daemon.mu.Unlock()
		for _, b := range daemon.beads {
			if b.Type == "molecule" {
				return true
			}
		}
		return false
	}, "expected molecule to be created")

	// Locate the thread bead by canonical label.
	var threadBead *beadsapi.BeadDetail
	daemon.mu.Lock()
	for _, b := range daemon.beads {
		for _, l := range b.Labels {
			if l == "slack-thread:C1:1700000000.123456" {
				threadBead = b
				break
			}
		}
		if threadBead != nil {
			break
		}
	}
	daemon.mu.Unlock()
	if threadBead == nil {
		t.Fatal("expected a thread bead to be created with canonical label")
	}

	// Locate the molecule.
	var mol *beadsapi.BeadDetail
	daemon.mu.Lock()
	for _, b := range daemon.beads {
		if b.Type == "molecule" {
			mol = b
			break
		}
	}
	daemon.mu.Unlock()
	if mol == nil {
		t.Fatal("expected a molecule bead")
	}

	// Find the triggered-by dep linking the molecule to the thread bead.
	// instantiateFormulaSteps records this lineage edge AFTER the molecule
	// bead is created, and both run in the async pour goroutine. The
	// assertEventually above only waits for the molecule to appear, so a
	// one-shot getDeps() read here races the still-pending AddDependency and
	// can observe deps=[] (or only the step parent-child edge) — the flake in
	// gd-eGuZwhTBm8. Poll for the dep instead of reading once.
	assertEventually(t, func() bool {
		for _, d := range daemon.getDeps() {
			if d.DepType == "triggered-by" && d.BeadID == mol.ID && d.DependsOnID == threadBead.ID {
				return true
			}
		}
		return false
	}, "expected triggered-by dep molecule="+mol.ID+" -> thread="+threadBead.ID)
}

// TestHandleFormulaPour_NoThreadNoTriggerDep asserts that pours issued from a
// channel (no thread_ts on the context) do not synthesize a thread bead nor a
// triggered-by lineage edge.
func TestHandleFormulaPour_NoThreadNoTriggerDep(t *testing.T) {
	daemon := newFormulaMockDaemon()
	daemon.seedProjectWithChannel("gardenio", "C1")
	seedFormula(daemon.mockDaemon, "gd-nothr1", "Channel-only formula",
		nil,
		[]formulaStep{{ID: "step1", Title: "Build"}},
	)
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.daemon = daemon

	bot.handleFormulaPour(context.Background(),
		slack.SlashCommand{ChannelID: "C1", UserID: "U1"},
		[]string{"gd-nothr1"}, false)

	assertEventually(t, func() bool {
		daemon.mu.Lock()
		defer daemon.mu.Unlock()
		for _, b := range daemon.beads {
			if b.Type == "molecule" {
				return true
			}
		}
		return false
	}, "expected molecule to be created")

	// Give async goroutines a moment to settle.
	time.Sleep(50 * time.Millisecond)

	for _, d := range daemon.getDeps() {
		if d.DepType == "triggered-by" {
			t.Errorf("expected no triggered-by dep for non-thread pour, got %+v", d)
		}
	}
	// No thread bead should have been created either.
	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	for _, b := range daemon.beads {
		for _, l := range b.Labels {
			if len(l) > len("slack-thread:") && l[:len("slack-thread:")] == "slack-thread:" {
				t.Errorf("expected no thread bead for non-thread pour, found one with label %q", l)
			}
		}
	}
}

// --- assertEventually helper ---

func assertEventually(t *testing.T, check func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Error(msg)
}
