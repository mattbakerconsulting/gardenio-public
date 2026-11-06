package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStripTestScenarioTable(t *testing.T) {
	cases := []struct {
		name     string
		input    string
		want     string
		removed  bool
	}{
		{
			name:    "no test_scenario table",
			input:   "[claude]\nsession_id = \"x\"\n",
			want:    "[claude]\nsession_id = \"x\"\n",
			removed: false,
		},
		{
			name: "strips simple test_scenario block",
			input: `[test_scenario]
startup_prompt = "go"

[claude]
session_id = "x"
`,
			want: `[claude]
session_id = "x"
`,
			removed: true,
		},
		{
			name: "strips test_scenario subtable",
			input: `[test_scenario.timing]
inject_after_ms = 200

[claude]
ok = true
`,
			want: `[claude]
ok = true
`,
			removed: true,
		},
		{
			name: "preserves array-of-tables after test_scenario",
			input: `[test_scenario]
startup_prompt = "begin"
some_other_key = 1

[[responses]]
on = "x"
say = "y"
`,
			want: `[[responses]]
on = "x"
say = "y"
`,
			removed: true,
		},
		{
			name: "indented test_scenario header still removed",
			input: "  [test_scenario]\n  startup_prompt = \"x\"\n[claude]\nok = true\n",
			want:  "[claude]\nok = true\n",
			removed: true,
		},
		{
			name: "test_scenarios (different name) is not stripped",
			input: `[test_scenarios_other]
key = "v"
`,
			want: `[test_scenarios_other]
key = "v"
`,
			removed: false,
		},
		{
			name: "no trailing newline still emits all lines",
			input: `[test_scenario]
startup_prompt = "x"`,
			want: ``,
			removed: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, removed := stripTestScenarioTable([]byte(tc.input))
			if removed != tc.removed {
				t.Errorf("removed: got %v, want %v", removed, tc.removed)
			}
			if string(got) != tc.want {
				t.Errorf("output mismatch:\n--- got ---\n%s--- want ---\n%s", got, tc.want)
			}
		})
	}
}

func TestMaterializeClaudelessScenario_NoTestScenario(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "plain.toml")
	if err := os.WriteFile(src, []byte("[claude]\nok = true\n"), 0644); err != nil {
		t.Fatal(err)
	}
	got, cleanup, err := materializeClaudelessScenario(src)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if got != src {
		t.Errorf("expected unchanged path, got %q", got)
	}
}

func TestMaterializeClaudelessScenario_StripsAndCleansUp(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "scenario.toml")
	body := `[test_scenario]
startup_prompt = "go"

[claude]
session_id = "x"

[[responses]]
on = "y"
say = "z"
`
	if err := os.WriteFile(src, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}

	got, cleanup, err := materializeClaudelessScenario(src)
	if err != nil {
		t.Fatal(err)
	}
	if got == src {
		t.Fatal("expected a temp path different from src when test_scenario is present")
	}
	out, err := os.ReadFile(got)
	if err != nil {
		t.Fatalf("read temp: %v", err)
	}
	if strings.Contains(string(out), "test_scenario") {
		t.Errorf("temp scenario still contains test_scenario:\n%s", out)
	}
	if !strings.Contains(string(out), "[[responses]]") {
		t.Errorf("temp scenario lost [[responses]]:\n%s", out)
	}

	cleanup()
	if _, err := os.Stat(got); !os.IsNotExist(err) {
		t.Errorf("cleanup did not remove temp file: stat err=%v", err)
	}
}

// Sanity: every local-*.toml under gardenio/tests/e2e/claudeless/ must
// produce a claudeless-loadable file after materialization. We don't
// invoke claudeless here (it isn't always installed on the unit-test
// runner) but we verify the materialized output never carries the
// gardenio-side `[test_scenario]` table that breaks claudeless 0.4.0.
//
// Startup prompts now live in sidecar `.startup-prompt.txt` files
// (see TestAllLocalScenariosHaveStartupPrompt), so materialization is
// allowed to be a no-op for scenarios that never embedded a
// `[test_scenario]` table.
func TestMaterializeAllLocalScenariosDropTestScenario(t *testing.T) {
	paths, err := filepath.Glob("../../../tests/e2e/claudeless/local-*.toml")
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) == 0 {
		t.Skip("local-*.toml fixtures not found from this test cwd")
	}
	for _, p := range paths {
		t.Run(filepath.Base(p), func(t *testing.T) {
			got, cleanup, err := materializeClaudelessScenario(p)
			if err != nil {
				t.Fatal(err)
			}
			defer cleanup()
			out, err := os.ReadFile(got)
			if err != nil {
				t.Fatal(err)
			}
			for line := range strings.SplitSeq(string(out), "\n") {
				trim := strings.TrimLeft(line, " \t")
				if strings.HasPrefix(trim, "[test_scenario]") || strings.HasPrefix(trim, "[test_scenario.") {
					t.Errorf("scenario %s still has test_scenario header: %q", p, line)
				}
			}
		})
	}
}
