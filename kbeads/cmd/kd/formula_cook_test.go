package main

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"gardenio/kbeads/internal/formula"
	"gardenio/kbeads/internal/model"
)

func TestCook_BasicExpansion(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	fields := makeFormulaFieldsWithPattern(t, []FormulaVarDef{
		{Name: "component", Required: true},
	}, []FormulaStep{
		{ID: "build", Title: "Build {{component}}", Type: "task"},
		{ID: "test", Title: "Test {{component}}", Type: "task", DependsOn: []string{"build"}},
	})

	mc.Beads["gd-formula-1"] = &model.Bead{
		ID:     "gd-formula-1",
		Type:   "formula",
		Title:  "Pipeline {{component}}",
		Fields: fields,
	}

	cmd := newCookCmd()
	cmd.SetArgs([]string{"gd-formula-1", "--var", "component=auth"})
	out := captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatalf("cook: %v", err)
		}
	})

	if !strings.Contains(out, "Build auth") {
		t.Errorf("expected substituted title 'Build auth', got:\n%s", out)
	}
	if !strings.Contains(out, "Test auth") {
		t.Errorf("expected substituted title 'Test auth', got:\n%s", out)
	}
	if !strings.Contains(out, "(after: build)") {
		t.Errorf("expected dependency info, got:\n%s", out)
	}
	// Should NOT create any beads.
	if len(mc.CreateBeadCalls) != 0 {
		t.Errorf("cook should not create beads, but got %d calls", len(mc.CreateBeadCalls))
	}
}

func TestCook_ShowsSkippedStepsVerbose(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	f := struct {
		Vars  []FormulaVarDef `json:"vars"`
		Steps []FormulaStep   `json:"steps"`
	}{
		Vars: []FormulaVarDef{{Name: "rollback"}},
		Steps: []FormulaStep{
			{ID: "deploy", Title: "Deploy", Type: "task"},
			{ID: "rollback", Title: "Rollback", Type: "task", Condition: "{{rollback}}"},
		},
	}
	data, _ := json.Marshal(f)

	mc.Beads["gd-formula-1"] = &model.Bead{
		ID:     "gd-formula-1",
		Type:   "formula",
		Title:  "Deploy flow",
		Fields: data,
	}

	// Without --verbose: skipped step hidden
	cmd := newCookCmd()
	cmd.SetArgs([]string{"gd-formula-1"})
	out := captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatalf("cook: %v", err)
		}
	})
	if strings.Contains(out, "[skipped]") {
		t.Errorf("without --verbose, should not show skipped steps:\n%s", out)
	}

	// With --verbose: skipped step shown
	cmd2 := newCookCmd()
	cmd2.SetArgs([]string{"gd-formula-1", "--verbose"})
	out2 := captureStdout(t, func() {
		if err := cmd2.Execute(); err != nil {
			t.Fatalf("cook verbose: %v", err)
		}
	})
	if !strings.Contains(out2, "[skipped]") {
		t.Errorf("with --verbose, should show skipped steps:\n%s", out2)
	}
	if !strings.Contains(out2, "Rollback") {
		t.Errorf("skipped step title missing:\n%s", out2)
	}
}

func TestCook_JSONOutput(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	fields := makeFormulaFieldsWithPattern(t, []FormulaVarDef{
		{Name: "name", Required: true},
	}, []FormulaStep{
		{ID: "step1", Title: "Do {{name}}", Type: "task"},
	})

	mc.Beads["gd-formula-1"] = &model.Bead{
		ID:     "gd-formula-1",
		Type:   "formula",
		Title:  "Test {{name}}",
		Fields: fields,
	}

	// Set jsonOutput for this test.
	oldJSON := jsonOutput
	jsonOutput = true
	defer func() { jsonOutput = oldJSON }()

	cmd := newCookCmd()
	cmd.SetArgs([]string{"gd-formula-1", "--var", "name=auth"})
	out := captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatalf("cook json: %v", err)
		}
	})

	var result map[string]any
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("cook --json output is not valid JSON: %v\n%s", err, out)
	}
	if result["formula_id"] != "gd-formula-1" {
		t.Errorf("formula_id = %v, want gd-formula-1", result["formula_id"])
	}
	steps, ok := result["steps"].([]any)
	if !ok || len(steps) != 1 {
		t.Errorf("expected 1 step in JSON output, got %v", result["steps"])
	}
}

// TestCook_VarValueWithCommas is the regression test for ka-0000003349: a
// --var VALUE containing commas must round-trip verbatim. The flag was
// previously StringSlice, which comma-split each value and turned
// "SUBJECT=a, b, c" into three tokens — the trailing "b"/" c" fragments then
// failed the key=value parse with `invalid --var " c": expected key=value`.
// StringArray keeps the whole value as one token. This test fails on the old
// StringSlice binding and passes on StringArray.
func TestCook_VarValueWithCommas(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	fields := makeFormulaFieldsWithPattern(t, []FormulaVarDef{
		{Name: "SUBJECT", Required: true},
	}, []FormulaStep{
		{ID: "step1", Title: "Handle: {{SUBJECT}}", Type: "task"},
	})

	mc.Beads["gd-formula-1"] = &model.Bead{
		ID:     "gd-formula-1",
		Type:   "formula",
		Title:  "Subject flow",
		Fields: fields,
	}

	cmd := newCookCmd()
	cmd.SetArgs([]string{"gd-formula-1", "--var", "SUBJECT=n/a (sysadmin flow, no subject)"})
	out := captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatalf("cook with comma var value: %v", err)
		}
	})

	if !strings.Contains(out, "Handle: n/a (sysadmin flow, no subject)") {
		t.Errorf("expected comma-containing var value to round-trip intact, got:\n%s", out)
	}
}

// TestCook_RepeatedVarFlags confirms StringArray still accumulates values
// across repeated --var occurrences (one value per flag), so multi-var pours
// are unaffected by the StringSlice→StringArray switch (ka-0000003349).
func TestCook_RepeatedVarFlags(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	fields := makeFormulaFieldsWithPattern(t, []FormulaVarDef{
		{Name: "role", Required: true},
		{Name: "site", Required: true},
	}, []FormulaStep{
		{ID: "step1", Title: "{{role}} at {{site}}", Type: "task"},
	})

	mc.Beads["gd-formula-1"] = &model.Bead{
		ID:     "gd-formula-1",
		Type:   "formula",
		Title:  "Role/site",
		Fields: fields,
	}

	cmd := newCookCmd()
	cmd.SetArgs([]string{
		"gd-formula-1",
		"--var", "role=sysadmin, on-call",
		"--var", "site=us-east, primary",
	})
	out := captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatalf("cook with repeated var flags: %v", err)
		}
	})

	if !strings.Contains(out, "sysadmin, on-call at us-east, primary") {
		t.Errorf("expected both comma-containing vars to round-trip, got:\n%s", out)
	}
}

func TestCook_MissingRequiredVar(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	fields := makeFormulaFieldsWithPattern(t, []FormulaVarDef{
		{Name: "version", Required: true},
	}, []FormulaStep{
		{ID: "step1", Title: "Release {{version}}", Type: "task"},
	})

	mc.Beads["gd-formula-1"] = &model.Bead{
		ID:     "gd-formula-1",
		Type:   "formula",
		Title:  "Release",
		Fields: fields,
	}

	cmd := newCookCmd()
	cmd.SetArgs([]string{"gd-formula-1"}) // no --var
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for missing required var, got nil")
	}
	if !strings.Contains(err.Error(), "required variable") {
		t.Errorf("expected 'required variable' error, got: %v", err)
	}
}

func TestCook_NonExistentFormula(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	mc.GetErr = fmt.Errorf("not found")

	cmd := newCookCmd()
	cmd.SetArgs([]string{"gd-nonexistent"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for non-existent formula, got nil")
	}
}

func TestCook_ShowsFormulaType(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	mc.Beads["gd-formula-1"] = &model.Bead{
		ID:     "gd-formula-1",
		Type:   "formula",
		Title:  "Typed formula",
		Fields: makeFormulaFieldsWithType(t, "workflow"),
	}

	cmd := newCookCmd()
	cmd.SetArgs([]string{"gd-formula-1"})
	out := captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatalf("cook: %v", err)
		}
	})

	if !strings.Contains(out, "Type: workflow") {
		t.Errorf("expected 'Type: workflow' in output:\n%s", out)
	}
}

// TestCook_ShowsGates checks cook previews each gate the way pour creates it:
// a decision gate step as a decision bead with its prompt and option ids, a
// manual gate step as a task with its gate type and timeout.
func TestCook_ShowsGates(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	fields := makeFormulaFieldsWithPattern(t, nil, []FormulaStep{
		{ID: "login", Title: "Sign in", Type: "task", Gate: &StepGate{Type: "manual", Timeout: "24h"}},
		{ID: "pick", Title: "Pick", DependsOn: []string{"login"}, Gate: &StepGate{
			Type:    "decision",
			Prompt:  "Which role?",
			Options: []formula.GateOption{{ID: "local", Label: "local"}, {ID: "crew", Label: "crew"}},
		}},
	})
	mc.Beads["gd-formula-1"] = &model.Bead{ID: "gd-formula-1", Type: "formula", Title: "Gated", Fields: fields}

	cmd := newCookCmd()
	cmd.SetArgs([]string{"gd-formula-1"})
	out := captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatalf("cook: %v", err)
		}
	})
	for _, want := range []string{
		"login: Sign in [task] {gate: manual, timeout 24h}",
		`pick: Pick [decision] (after: login) {gate: decision "Which role?" [local|crew]}`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("cook output missing %q:\n%s", want, out)
		}
	}

	oldJSON := jsonOutput
	jsonOutput = true
	defer func() { jsonOutput = oldJSON }()
	cmd2 := newCookCmd()
	cmd2.SetArgs([]string{"gd-formula-1"})
	out2 := captureStdout(t, func() {
		if err := cmd2.Execute(); err != nil {
			t.Fatalf("cook json: %v", err)
		}
	})
	var result struct {
		Steps []struct {
			ID   string    `json:"id"`
			Type string    `json:"type"`
			Gate *StepGate `json:"gate"`
		} `json:"steps"`
	}
	if err := json.Unmarshal([]byte(out2), &result); err != nil {
		t.Fatalf("cook --json output is not valid JSON: %v\n%s", err, out2)
	}
	if len(result.Steps) != 2 || result.Steps[0].Gate == nil || result.Steps[1].Gate == nil {
		t.Fatalf("expected two gated steps, got:\n%s", out2)
	}
	if got := result.Steps[0].Gate.Timeout; got != "24h" {
		t.Errorf("manual gate timeout = %q, want 24h", got)
	}
	if got := result.Steps[1].Type; got != "decision" {
		t.Errorf("decision gate step type = %q, want decision", got)
	}
}

// TestCook_OnboardingFormulas cooks the onboarding formulas shipped in
// gardenio/formulas/onboarding from disk with sample vars, so a broken file
// (bad JSON, a dangling needs, a missing gate prompt) fails here.
func TestCook_OnboardingFormulas(t *testing.T) {
	withMockClient(t, newMockClient())

	dir := filepath.Join("..", "..", "..", "gardenio", "formulas", "onboarding")
	cases := map[string][]string{
		"register-project.json": {"project=demo", "git_url=https://example.com/demo.git", "account=demo-anthropic"},
		"add-local-agent.json":  {"project=demo"},
		"first-task.json":       {"project=demo"},
		"enable-slack.json":     {"project=demo", "channel_id=C0123456789"},
	}
	for name, vars := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(dir, name)
			_, fields, err := loadFormulaFromFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := validateStepGates(fields.Steps); err != nil {
				t.Fatal(err)
			}
			gates := 0
			for _, s := range fields.Steps {
				if s.Gate == nil {
					continue
				}
				gates++
				// A manual or sync gate step must stay type task
				// (test-formula-gate-pour.sh, ka-0000006567).
				if s.Gate.Type != "decision" && s.Type != "task" {
					t.Errorf("step %q: %s gate step has type %q, want task", s.ID, s.Gate.Type, s.Type)
				}
			}
			if gates == 0 {
				t.Errorf("%s has no gated human steps", name)
			}

			args := []string{path}
			for _, v := range vars {
				args = append(args, "--var", v)
			}
			cmd := newCookCmd()
			cmd.SetArgs(args)
			out := captureStdout(t, func() {
				if err := cmd.Execute(); err != nil {
					t.Fatalf("cook %s: %v", name, err)
				}
			})
			if got := strings.Count(out, "{gate: "); got != gates {
				t.Errorf("cook printed %d gates, want %d:\n%s", got, gates, out)
			}
			if strings.Contains(out, "{{") {
				t.Errorf("unsubstituted var in cook output:\n%s", out)
			}
		})
	}
}
