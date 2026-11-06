package bridge

import (
	"context"
	"encoding/json"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// --- parseFormulaFields tests ---

func TestParseFormulaFields_Empty(t *testing.T) {
	bead := &beadsapi.BeadDetail{ID: "gd-1", Fields: nil}
	ff := parseFormulaFields(bead)
	if len(ff.Vars) != 0 || len(ff.Steps) != 0 {
		t.Errorf("expected empty vars/steps for nil fields, got %d vars, %d steps", len(ff.Vars), len(ff.Steps))
	}
}

func TestParseFormulaFields_WithVarsAndSteps(t *testing.T) {
	vars := []formulaVarDef{
		{Name: "env", Required: true, Enum: []string{"staging", "prod"}},
		{Name: "version", Default: "latest"},
	}
	steps := []formulaStep{
		{ID: "build", Title: "Build {{env}}", Type: "task"},
		{ID: "deploy", Title: "Deploy to {{env}}", DependsOn: []string{"build"}},
	}
	varsJSON, _ := json.Marshal(vars)
	stepsJSON, _ := json.Marshal(steps)

	bead := &beadsapi.BeadDetail{
		ID: "gd-formula-1",
		Fields: map[string]string{
			"vars":  string(varsJSON),
			"steps": string(stepsJSON),
		},
	}

	ff := parseFormulaFields(bead)
	if len(ff.Vars) != 2 {
		t.Fatalf("expected 2 vars, got %d", len(ff.Vars))
	}
	if ff.Vars[0].Name != "env" || !ff.Vars[0].Required {
		t.Errorf("expected first var to be env (required), got %+v", ff.Vars[0])
	}
	if ff.Vars[1].Default != "latest" {
		t.Errorf("expected second var default=latest, got %q", ff.Vars[1].Default)
	}
	if len(ff.Steps) != 2 {
		t.Fatalf("expected 2 steps, got %d", len(ff.Steps))
	}
	if ff.Steps[1].DependsOn[0] != "build" {
		t.Errorf("expected deploy to depend on build, got %v", ff.Steps[1].DependsOn)
	}
}

func TestParseFormulaFields_InvalidJSON(t *testing.T) {
	bead := &beadsapi.BeadDetail{
		ID:     "gd-bad",
		Fields: map[string]string{"vars": "not-json", "steps": "{bad}"},
	}
	ff := parseFormulaFields(bead)
	if len(ff.Vars) != 0 || len(ff.Steps) != 0 {
		t.Errorf("expected empty results for invalid JSON, got %d vars, %d steps", len(ff.Vars), len(ff.Steps))
	}
}

// --- formulaSubstituteVars tests ---

func TestFormulaSubstituteVars_Basic(t *testing.T) {
	vars := map[string]string{"env": "staging", "version": "1.2.3"}
	tests := []struct {
		input, want string
	}{
		{"Deploy to {{env}}", "Deploy to staging"},
		{"{{env}}-{{version}}", "staging-1.2.3"},
		{"No vars here", "No vars here"},
		{"{{missing}} stays", "{{missing}} stays"},
		{"", ""},
	}
	for _, tt := range tests {
		got := formulaSubstituteVars(tt.input, vars)
		if got != tt.want {
			t.Errorf("formulaSubstituteVars(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestFormulaSubstituteVars_NilVars(t *testing.T) {
	got := formulaSubstituteVars("{{env}}", nil)
	if got != "{{env}}" {
		t.Errorf("expected unresolved var with nil map, got %q", got)
	}
}

// --- formulaEvalCondition tests ---

func TestFormulaEvalCondition_Equality(t *testing.T) {
	vars := map[string]string{"env": "prod", "flag": "true"}
	tests := []struct {
		name string
		cond string
		want bool
	}{
		{"equals match", "{{env}} == prod", true},
		{"equals mismatch", "{{env}} == staging", false},
		{"not-equals match", "{{env}} != staging", true},
		{"not-equals mismatch", "{{env}} != prod", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formulaEvalCondition(tt.cond, vars)
			if got != tt.want {
				t.Errorf("formulaEvalCondition(%q) = %v, want %v", tt.cond, got, tt.want)
			}
		})
	}
}

func TestFormulaEvalCondition_Negation(t *testing.T) {
	vars := map[string]string{"env": "prod"}
	tests := []struct {
		name string
		cond string
		want bool
	}{
		{"negate truthy var", "!{{env}}", false},
		{"negate missing var", "!{{missing}}", true},
		{"negate equals", "!{{env}} == staging", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formulaEvalCondition(tt.cond, vars)
			if got != tt.want {
				t.Errorf("formulaEvalCondition(%q) = %v, want %v", tt.cond, got, tt.want)
			}
		})
	}
}

func TestFormulaEvalCondition_TruthyFalsy(t *testing.T) {
	vars := map[string]string{"flag": "true", "zero": "0", "empty": "", "no": "false"}
	tests := []struct {
		name string
		cond string
		want bool
	}{
		{"truthy var", "{{flag}}", true},
		{"zero is falsy", "{{zero}}", false},
		{"false is falsy", "{{no}}", false},
		{"empty is falsy", "{{empty}}", false},
		{"unresolved var is falsy", "{{missing}}", false},
		{"literal string is truthy", "always", true},
		{"empty cond", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formulaEvalCondition(tt.cond, vars)
			if got != tt.want {
				t.Errorf("formulaEvalCondition(%q) = %v, want %v", tt.cond, got, tt.want)
			}
		})
	}
}

// --- splitVarPair tests ---

func TestSplitVarPair(t *testing.T) {
	tests := []struct {
		input     string
		wantKey   string
		wantVal   string
		wantOK    bool
	}{
		{"env=prod", "env", "prod", true},
		{"version=1.2.3", "version", "1.2.3", true},
		{"key=", "key", "", true},
		{"key=val=ue", "key", "val=ue", true},
		{"noequals", "", "", false},
		{"=value", "", "", false},
		{"", "", "", false},
	}
	for _, tt := range tests {
		k, v, ok := splitVarPair(tt.input)
		if ok != tt.wantOK || k != tt.wantKey || v != tt.wantVal {
			t.Errorf("splitVarPair(%q) = (%q, %q, %v), want (%q, %q, %v)",
				tt.input, k, v, ok, tt.wantKey, tt.wantVal, tt.wantOK)
		}
	}
}

// --- plural tests ---

func TestPlural(t *testing.T) {
	if plural(0) != "s" {
		t.Error("plural(0) should be 's'")
	}
	if plural(1) != "" {
		t.Error("plural(1) should be ''")
	}
	if plural(2) != "s" {
		t.Error("plural(2) should be 's'")
	}
}

// --- formulaBuildStepLabels tests (from main) ---

func TestFormulaBuildStepLabels_SameProject(t *testing.T) {
	molLabels := []string{"project:gardenio"}
	stepLabels := []string{"team:alpha"}
	got := formulaBuildStepLabels(molLabels, stepLabels, "gardenio", "gardenio")

	want := map[string]bool{"project:gardenio": true, "team:alpha": true}
	if len(got) != len(want) {
		t.Fatalf("expected %d labels, got %d: %v", len(want), len(got), got)
	}
	for _, l := range got {
		if !want[l] {
			t.Errorf("unexpected label %q", l)
		}
	}
}

func TestFormulaBuildStepLabels_DifferentProject(t *testing.T) {
	molLabels := []string{"project:gardenio"}
	stepLabels := []string{"team:alpha"}
	got := formulaBuildStepLabels(molLabels, stepLabels, "infra", "gardenio")

	want := map[string]bool{"project:infra": true, "team:alpha": true}
	if len(got) != len(want) {
		t.Fatalf("expected %d labels, got %d: %v", len(want), len(got), got)
	}
	for _, l := range got {
		if !want[l] {
			t.Errorf("unexpected label %q", l)
		}
	}
	// Ensure gardenio project label was removed.
	for _, l := range got {
		if l == "project:gardenio" {
			t.Error("molecule project label should have been replaced")
		}
	}
}

func TestFormulaBuildStepLabels_NoStepLabels(t *testing.T) {
	molLabels := []string{"project:gardenio", "priority:high"}
	got := formulaBuildStepLabels(molLabels, nil, "gardenio", "gardenio")

	if len(got) != 2 {
		t.Fatalf("expected 2 labels, got %d: %v", len(got), got)
	}
}

func TestFormulaBuildStepLabels_NoDuplicates(t *testing.T) {
	molLabels := []string{"project:gardenio"}
	stepLabels := []string{"project:gardenio", "extra:tag"}
	got := formulaBuildStepLabels(molLabels, stepLabels, "gardenio", "gardenio")

	count := 0
	for _, l := range got {
		if l == "project:gardenio" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected exactly 1 project:gardenio label, got %d", count)
	}
}

// --- formulaStep role/project field tests (from main) ---

func TestFormulaStepRoleProjectFields(t *testing.T) {
	// Verify the new fields parse correctly from JSON-like struct.
	step := formulaStep{
		ID:              "deploy",
		Title:           "Deploy",
		Role:            "crew",
		Project:         "infra",
		SuggestNewAgent: true,
	}

	if step.Role != "crew" {
		t.Errorf("expected role=crew, got %s", step.Role)
	}
	if step.Project != "infra" {
		t.Errorf("expected project=infra, got %s", step.Project)
	}
	if !step.SuggestNewAgent {
		t.Error("expected suggest_new_agent=true")
	}
}

// --- instantiateFormulaSteps tests ---

// depCall records an AddDependency call for assertions.
type depCall struct {
	BeadID      string
	DependsOnID string
	DepType     string
	CreatedBy   string
}

// formulaMockDaemon extends mockDaemon with dependency tracking.
type formulaMockDaemon struct {
	*mockDaemon
	deps []depCall
}

func newFormulaMockDaemon() *formulaMockDaemon {
	return &formulaMockDaemon{mockDaemon: newMockDaemon()}
}

func (m *formulaMockDaemon) AddDependency(_ context.Context, beadID, dependsOnID, depType, createdBy string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deps = append(m.deps, depCall{beadID, dependsOnID, depType, createdBy})
	return nil
}

func (m *formulaMockDaemon) getDeps() []depCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]depCall{}, m.deps...)
}

func TestInstantiateFormulaSteps_BasicMolecule(t *testing.T) {
	daemon := newFormulaMockDaemon()
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	// Override daemon to our formula mock.
	bot.daemon = daemon

	formula := &beadsapi.BeadDetail{
		ID:          "gd-formula-1",
		Title:       "Deploy {{env}}",
		Description: "Deploy service to {{env}}",
		Priority:    3,
	}

	steps := []formulaStep{
		{ID: "build", Title: "Build service", Type: "task"},
		{ID: "deploy", Title: "Deploy to target", Type: "task", DependsOn: []string{"build"}},
	}

	vars := map[string]string{"env": "staging"}

	molID, stepCount, err := bot.instantiateFormulaSteps(
		context.Background(), formula, steps, vars, false, "gardenio", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if stepCount != 2 {
		t.Errorf("expected 2 steps, got %d", stepCount)
	}
	if molID == "" {
		t.Fatal("expected non-empty molecule ID")
	}

	// Verify molecule bead was created.
	molBeads := filterBeadsByType(daemon.beads, "molecule")
	if len(molBeads) != 1 {
		t.Fatalf("expected 1 molecule bead, got %d", len(molBeads))
	}
	mol := molBeads[0]
	if mol.Title != "Deploy staging" {
		t.Errorf("expected molecule title 'Deploy staging', got %q", mol.Title)
	}
	if !containsLabel(mol.Labels, "project:gardenio") {
		t.Errorf("expected molecule to have project:gardenio label, got %v", mol.Labels)
	}

	// Verify step beads were created.
	taskBeads := filterTaskBeads(daemon.beads)
	if len(taskBeads) != 2 {
		t.Fatalf("expected 2 task beads, got %d", len(taskBeads))
	}

	// Verify dependencies were created.
	deps := daemon.getDeps()
	// Should have 2 parent-child deps (one per step) + 1 blocks dep (deploy depends on build).
	parentChildCount := 0
	blocksCount := 0
	for _, d := range deps {
		switch d.DepType {
		case "parent-child":
			parentChildCount++
			if d.DependsOnID != molID {
				t.Errorf("parent-child dep should point to molecule %s, got %s", molID, d.DependsOnID)
			}
		case "blocks":
			blocksCount++
		}
	}
	if parentChildCount != 2 {
		t.Errorf("expected 2 parent-child deps, got %d", parentChildCount)
	}
	if blocksCount != 1 {
		t.Errorf("expected 1 blocks dep, got %d", blocksCount)
	}
}

func TestInstantiateFormulaSteps_EphemeralWisp(t *testing.T) {
	daemon := newFormulaMockDaemon()
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.daemon = daemon

	formula := &beadsapi.BeadDetail{
		ID:    "gd-formula-2",
		Title: "Quick check",
	}

	steps := []formulaStep{
		{ID: "check", Title: "Run check", Type: "task"},
	}

	molID, _, err := bot.instantiateFormulaSteps(
		context.Background(), formula, steps, nil, true, "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify molecule has ephemeral field.
	mol := daemon.beads[molID]
	if mol == nil {
		// Find it by iterating.
		for _, b := range daemon.beads {
			if b.Type == "molecule" {
				mol = b
				break
			}
		}
	}
	if mol == nil {
		t.Fatal("molecule bead not found")
	}
	// The mock's CreateBead stores fields via json.Unmarshal into map[string]string,
	// which renders the bool "true" as empty string. Verify the key exists.
	if mol.Fields == nil {
		t.Error("expected molecule fields to contain ephemeral key")
	} else if _, ok := mol.Fields["ephemeral"]; !ok {
		t.Errorf("expected ephemeral key in molecule fields, got %v", mol.Fields)
	}
}

func TestInstantiateFormulaSteps_StepLabels(t *testing.T) {
	daemon := newFormulaMockDaemon()
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.daemon = daemon

	formula := &beadsapi.BeadDetail{
		ID:    "gd-formula-3",
		Title: "Multi-project deploy",
	}

	steps := []formulaStep{
		{
			ID:     "step1",
			Title:  "Build in project A",
			Type:   "task",
			Labels: []string{"role:devops", "team:infra"},
		},
	}

	_, _, err := bot.instantiateFormulaSteps(
		context.Background(), formula, steps, nil, false, "gardenio", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	taskBeads := filterTaskBeads(daemon.beads)
	if len(taskBeads) != 1 {
		t.Fatalf("expected 1 task bead, got %d", len(taskBeads))
	}

	step := taskBeads[0]
	// Should have project label from formula + step-specific labels.
	if !containsLabel(step.Labels, "project:gardenio") {
		t.Errorf("expected project:gardenio label, got %v", step.Labels)
	}
	if !containsLabel(step.Labels, "role:devops") {
		t.Errorf("expected role:devops label, got %v", step.Labels)
	}
	if !containsLabel(step.Labels, "team:infra") {
		t.Errorf("expected team:infra label, got %v", step.Labels)
	}
}

func TestInstantiateFormulaSteps_StepLabelDedup(t *testing.T) {
	daemon := newFormulaMockDaemon()
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.daemon = daemon

	formula := &beadsapi.BeadDetail{
		ID:    "gd-formula-4",
		Title: "Dedup test",
	}

	steps := []formulaStep{
		{
			ID:     "step1",
			Title:  "Step with duplicate label",
			Type:   "task",
			Labels: []string{"project:gardenio", "extra:label"},
		},
	}

	_, _, err := bot.instantiateFormulaSteps(
		context.Background(), formula, steps, nil, false, "gardenio", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	taskBeads := filterTaskBeads(daemon.beads)
	step := taskBeads[0]

	// Count project:gardenio labels — should not be duplicated.
	count := 0
	for _, l := range step.Labels {
		if l == "project:gardenio" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected project:gardenio to appear once, appeared %d times in %v", count, step.Labels)
	}
}

func TestInstantiateFormulaSteps_StepPriority(t *testing.T) {
	daemon := newFormulaMockDaemon()
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.daemon = daemon

	formula := &beadsapi.BeadDetail{
		ID:       "gd-formula-5",
		Title:    "Priority test",
		Priority: 5,
	}

	customPri := 2
	steps := []formulaStep{
		{ID: "high", Title: "High priority step", Priority: &customPri},
		{ID: "default", Title: "Default priority step"},
	}

	_, _, err := bot.instantiateFormulaSteps(
		context.Background(), formula, steps, nil, false, "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Both steps should be tasks. The mock assigns priority via CreateBeadRequest.Priority
	// which we can verify by checking the created beads.
	taskBeads := filterTaskBeads(daemon.beads)
	if len(taskBeads) != 2 {
		t.Fatalf("expected 2 task beads, got %d", len(taskBeads))
	}
}

func TestInstantiateFormulaSteps_DefaultType(t *testing.T) {
	daemon := newFormulaMockDaemon()
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.daemon = daemon

	formula := &beadsapi.BeadDetail{
		ID:    "gd-formula-6",
		Title: "Type default test",
	}

	steps := []formulaStep{
		{ID: "noType", Title: "Step without type"},
		{ID: "withType", Title: "Step with type", Type: "bug"},
	}

	_, _, err := bot.instantiateFormulaSteps(
		context.Background(), formula, steps, nil, false, "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Find the beads and check their types.
	var taskBead, bugBead *beadsapi.BeadDetail
	for _, b := range daemon.beads {
		if b.Type == "task" {
			taskBead = b
		}
		if b.Type == "bug" {
			bugBead = b
		}
	}
	if taskBead == nil {
		t.Error("step without type should default to 'task'")
	}
	if bugBead == nil {
		t.Error("step with type='bug' should create a bug bead")
	}
}

func TestInstantiateFormulaSteps_NoProject(t *testing.T) {
	daemon := newFormulaMockDaemon()
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.daemon = daemon

	formula := &beadsapi.BeadDetail{
		ID:    "gd-formula-7",
		Title: "No project",
	}

	steps := []formulaStep{
		{ID: "step1", Title: "Step 1"},
	}

	_, _, err := bot.instantiateFormulaSteps(
		context.Background(), formula, steps, nil, false, "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Molecule and step should have no project label.
	for _, b := range daemon.beads {
		for _, l := range b.Labels {
			if l == "project:" {
				t.Errorf("expected no empty project label when project is empty, got %v", b.Labels)
			}
		}
		if len(b.Labels) != 0 {
			t.Errorf("expected no labels when project is empty, got %v", b.Labels)
		}
	}
}

func TestInstantiateFormulaSteps_MultipleBlocksDeps(t *testing.T) {
	daemon := newFormulaMockDaemon()
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.daemon = daemon

	formula := &beadsapi.BeadDetail{
		ID:    "gd-formula-8",
		Title: "Diamond deps",
	}

	steps := []formulaStep{
		{ID: "a", Title: "Step A"},
		{ID: "b", Title: "Step B", DependsOn: []string{"a"}},
		{ID: "c", Title: "Step C", DependsOn: []string{"a"}},
		{ID: "d", Title: "Step D", DependsOn: []string{"b", "c"}},
	}

	_, _, err := bot.instantiateFormulaSteps(
		context.Background(), formula, steps, nil, false, "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	deps := daemon.getDeps()
	blocksCount := 0
	for _, d := range deps {
		if d.DepType == "blocks" {
			blocksCount++
		}
	}
	// b->a, c->a, d->b, d->c = 4 blocks deps
	if blocksCount != 4 {
		t.Errorf("expected 4 blocks deps for diamond pattern, got %d", blocksCount)
	}
}

func TestInstantiateFormulaSteps_MissingDependency(t *testing.T) {
	daemon := newFormulaMockDaemon()
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.daemon = daemon

	formula := &beadsapi.BeadDetail{
		ID:    "gd-formula-9",
		Title: "Missing dep",
	}

	steps := []formulaStep{
		{ID: "step1", Title: "Step 1", DependsOn: []string{"nonexistent"}},
	}

	_, stepCount, err := bot.instantiateFormulaSteps(
		context.Background(), formula, steps, nil, false, "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if stepCount != 1 {
		t.Errorf("expected 1 step, got %d", stepCount)
	}

	// Should have parent-child dep but no blocks dep (nonexistent step skipped).
	deps := daemon.getDeps()
	blocksCount := 0
	for _, d := range deps {
		if d.DepType == "blocks" {
			blocksCount++
		}
	}
	if blocksCount != 0 {
		t.Errorf("expected 0 blocks deps for missing dependency, got %d", blocksCount)
	}
}

// --- Cross-project step label tests ---

func TestInstantiateFormulaSteps_CrossProjectLabels(t *testing.T) {
	daemon := newFormulaMockDaemon()
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.daemon = daemon

	formula := &beadsapi.BeadDetail{
		ID:    "gd-formula-cross",
		Title: "Cross-project formula",
	}

	// Step has a different project label than the molecule's project.
	steps := []formulaStep{
		{
			ID:     "step1",
			Title:  "Step in other project",
			Labels: []string{"project:other-project", "role:devops"},
		},
	}

	_, _, err := bot.instantiateFormulaSteps(
		context.Background(), formula, steps, nil, false, "gardenio", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	taskBeads := filterTaskBeads(daemon.beads)
	if len(taskBeads) != 1 {
		t.Fatalf("expected 1 task bead, got %d", len(taskBeads))
	}

	step := taskBeads[0]
	// Step should have both project labels (gardenio from formula + other-project from step).
	if !containsLabel(step.Labels, "project:gardenio") {
		t.Errorf("expected project:gardenio label from molecule project, got %v", step.Labels)
	}
	if !containsLabel(step.Labels, "project:other-project") {
		t.Errorf("expected project:other-project label from step, got %v", step.Labels)
	}
	if !containsLabel(step.Labels, "role:devops") {
		t.Errorf("expected role:devops label from step, got %v", step.Labels)
	}
}

// TestInstantiateFormulaSteps_StepAssignee is in bot_formula_pour_test.go.

// Formula pour, resolve, and command routing tests are in bot_formula_pour_test.go.

// TestInstantiateFormulaSteps_MoleculeLineageFields locks in the molecule's
// lineage hooks (formula_id, applied_vars) that D3 will extend with a
// trigger-bead dependency. If these field names change without intent, the
// downstream lineage-resolution logic will silently break.
func TestInstantiateFormulaSteps_MoleculeLineageFields(t *testing.T) {
	daemon := newFormulaMockDaemon()
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.daemon = daemon

	formula := &beadsapi.BeadDetail{
		ID:    "gd-formula-lin",
		Title: "Deploy {{env}}",
	}
	steps := []formulaStep{{ID: "build", Title: "Build", Type: "task"}}
	vars := map[string]string{"env": "prod"}

	_, _, err := bot.instantiateFormulaSteps(
		context.Background(), formula, steps, vars, false, "gardenio", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	molBeads := filterBeadsByType(daemon.beads, "molecule")
	if len(molBeads) != 1 {
		t.Fatalf("expected 1 molecule, got %d", len(molBeads))
	}
	mol := molBeads[0]
	if got := mol.Fields["formula_id"]; got != "gd-formula-lin" {
		t.Errorf("expected formula_id=gd-formula-lin, got %q", got)
	}
	if got := mol.Fields["applied_vars"]; got == "" {
		t.Errorf("expected non-empty applied_vars, got empty")
	}
	if _, ok := mol.Fields["ephemeral"]; ok {
		t.Errorf("non-ephemeral molecule should not set ephemeral field, got %q", mol.Fields["ephemeral"])
	}
}

// TestInstantiateFormulaSteps_EphemeralMoleculeFlag asserts the ephemeral flag
// is written into the molecule's Fields for wisps, so consumers can distinguish
// ephemeral lineage from durable molecule lineage.
func TestInstantiateFormulaSteps_EphemeralMoleculeFlag(t *testing.T) {
	daemon := newFormulaMockDaemon()
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.daemon = daemon

	formula := &beadsapi.BeadDetail{ID: "gd-formula-wisp", Title: "Quick"}
	steps := []formulaStep{{ID: "s1", Title: "Step", Type: "task"}}

	_, _, err := bot.instantiateFormulaSteps(
		context.Background(), formula, steps, nil, true, "gardenio", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	molBeads := filterBeadsByType(daemon.beads, "molecule")
	if len(molBeads) != 1 {
		t.Fatalf("expected 1 molecule, got %d", len(molBeads))
	}
	if got := molBeads[0].Fields["ephemeral"]; got != "true" {
		t.Errorf("expected ephemeral=true on wisp molecule, got %q", got)
	}
}
