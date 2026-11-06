package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveScenarioPath(t *testing.T) {
	tmp := t.TempDir()

	// Seed monorepo-layout scenario: <tmp>/gardenio/tests/e2e/claudeless/hello.toml
	monoDir := filepath.Join(tmp, "gardenio", "tests", "e2e", "claudeless")
	if err := os.MkdirAll(monoDir, 0o755); err != nil {
		t.Fatalf("mkdir mono: %v", err)
	}
	monoScenario := filepath.Join(monoDir, "hello.toml")
	if err := os.WriteFile(monoScenario, []byte("[claude]\n"), 0o644); err != nil {
		t.Fatalf("write mono: %v", err)
	}

	// Seed component-layout scenario: <tmp>/tests/e2e/claudeless/compname.toml
	compDir := filepath.Join(tmp, "tests", "e2e", "claudeless")
	if err := os.MkdirAll(compDir, 0o755); err != nil {
		t.Fatalf("mkdir comp: %v", err)
	}
	compScenario := filepath.Join(compDir, "compname.toml")
	if err := os.WriteFile(compScenario, []byte("[claude]\n"), 0o644); err != nil {
		t.Fatalf("write comp: %v", err)
	}

	// Seed custom-dir scenario: <tmp>/custom/custom.toml
	customDir := filepath.Join(tmp, "custom")
	if err := os.MkdirAll(customDir, 0o755); err != nil {
		t.Fatalf("mkdir custom: %v", err)
	}
	customScenario := filepath.Join(customDir, "custom.toml")
	if err := os.WriteFile(customScenario, []byte("[claude]\n"), 0o644); err != nil {
		t.Fatalf("write custom: %v", err)
	}

	tests := []struct {
		name        string
		value       string
		scenarioDir string
		dir         string
		wantAbs     string
		wantErr     string
	}{
		{
			name:    "bare name resolves via monorepo layout first",
			value:   "hello",
			dir:     tmp,
			wantAbs: monoScenario,
		},
		{
			name:    "bare name falls through to component layout",
			value:   "compname",
			dir:     tmp,
			wantAbs: compScenario,
		},
		{
			name:        "scenario-dir overrides default search",
			value:       "custom",
			scenarioDir: "custom",
			dir:         tmp,
			wantAbs:     customScenario,
		},
		{
			name:    "absolute path used verbatim",
			value:   monoScenario,
			dir:     tmp,
			wantAbs: monoScenario,
		},
		{
			name:    "relative path with slash resolved against dir",
			value:   "gardenio/tests/e2e/claudeless/hello.toml",
			dir:     tmp,
			wantAbs: monoScenario,
		},
		{
			name:    "toml suffix triggers path resolution",
			value:   "hello.toml",
			dir:     monoDir,
			wantAbs: monoScenario,
		},
		{
			name:    "missing scenario returns descriptive error",
			value:   "nope",
			dir:     tmp,
			wantErr: `scenario "nope" not found`,
		},
		{
			name:    "missing path returns error",
			value:   "/nonexistent/thing.toml",
			dir:     tmp,
			wantErr: "scenario file not found",
		},
		{
			name:    "empty value errors",
			value:   "",
			dir:     tmp,
			wantErr: "empty scenario",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveScenarioPath(tc.value, tc.scenarioDir, tc.dir)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("want error containing %q, got success with %s", tc.wantErr, got)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error %q does not contain %q", err.Error(), tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			wantAbs, _ := filepath.Abs(tc.wantAbs)
			if got != wantAbs {
				t.Fatalf("got %q, want %q", got, wantAbs)
			}
		})
	}
}

func TestLocalAgentEnv_TaskID(t *testing.T) {
	t.Run("non-empty task sets BOAT_TASK_ID override", func(t *testing.T) {
		// Even if parent env already has a value, --task overrides it.
		t.Setenv("BOAT_TASK_ID", "from-parent")
		env := localAgentEnv("agent", "gd-agent-bead", "crew", "/tmp", "http://127.0.0.1:8080", "gd-task-123", localCreds{HTTPURL: "http://localhost:8080"})
		got := findEnv(env, "BOAT_TASK_ID")
		if got != "gd-task-123" {
			t.Fatalf("BOAT_TASK_ID = %q, want %q", got, "gd-task-123")
		}
	})

	t.Run("empty task preserves parent BOAT_TASK_ID", func(t *testing.T) {
		// If no --task flag, inherit whatever the parent env set (may be empty).
		t.Setenv("BOAT_TASK_ID", "from-parent")
		env := localAgentEnv("agent", "gd-agent-bead", "crew", "/tmp", "http://127.0.0.1:8080", "", localCreds{HTTPURL: "http://localhost:8080"})
		got := findEnv(env, "BOAT_TASK_ID")
		if got != "from-parent" {
			t.Fatalf("BOAT_TASK_ID = %q, want parent value %q", got, "from-parent")
		}
	})
}

// TestLocalAgentEnv_CoopMuxURL verifies that --local mode disables coop's
// mux client by default but still honors an explicit COOP_MUX_URL override.
// See ka-0000002670: without this, every --local shutdown emits a noisy
// "mux deregistration failed" WARN against the 127.0.0.1:9800 default.
func TestLocalAgentEnv_CoopMuxURL(t *testing.T) {
	t.Run("unset parent disables mux via empty override", func(t *testing.T) {
		t.Setenv("COOP_MUX_URL", "")
		_ = os.Unsetenv("COOP_MUX_URL")
		env := localAgentEnv("agent", "gd-agent-bead", "crew", "/tmp", "http://127.0.0.1:8080", "", localCreds{HTTPURL: "http://localhost:8080"})
		got := findEnv(env, "COOP_MUX_URL")
		if got != "" {
			t.Fatalf("COOP_MUX_URL = %q, want empty (mux disabled in --local)", got)
		}
		// Must be present so coop sees the empty value (not unset → default).
		var found bool
		for _, e := range env {
			if strings.HasPrefix(e, "COOP_MUX_URL=") {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("COOP_MUX_URL not present in env; coop would fall back to 127.0.0.1:9800 default")
		}
	})

	t.Run("explicit parent value passes through", func(t *testing.T) {
		t.Setenv("COOP_MUX_URL", "http://my-mux:9800")
		env := localAgentEnv("agent", "gd-agent-bead", "crew", "/tmp", "http://127.0.0.1:8080", "", localCreds{HTTPURL: "http://localhost:8080"})
		got := findEnv(env, "COOP_MUX_URL")
		if got != "http://my-mux:9800" {
			t.Fatalf("COOP_MUX_URL = %q, want parent value http://my-mux:9800", got)
		}
	})

	t.Run("explicit empty parent value passes through", func(t *testing.T) {
		t.Setenv("COOP_MUX_URL", "")
		env := localAgentEnv("agent", "gd-agent-bead", "crew", "/tmp", "http://127.0.0.1:8080", "", localCreds{HTTPURL: "http://localhost:8080"})
		got := findEnv(env, "COOP_MUX_URL")
		if got != "" {
			t.Fatalf("COOP_MUX_URL = %q, want empty", got)
		}
	})
}

// TestStripS3EnvForScenario verifies that --test-scenario mode scrubs
// every COOP_S3_* env key inherited from the parent process. Without this,
// coop attempts to restore a prior session from S3 and prepends
// `--resume <id>` to the claudeless command line — claudeless 0.4.0
// exits within ~500ms and the scenario engine never sees user input.
// See gd-YO6VQ6NNUV for the original failure mode.
func TestStripS3EnvForScenario(t *testing.T) {
	env := []string{
		"PATH=/usr/bin",
		"COOP_S3_BUCKET=gardenio-sessions",
		"COOP_S3_PREFIX=coop/sessions",
		"COOP_S3_REGION=us-east-1",
		"COOP_S3_SESSION_KEY=gd-bead",
		"COOP_S3_RESUME_SESSION=sess-prior",
		"COOP_S3_UPLOAD_INTERVAL_MS=5000",
		"COOP_NATS_URL=ws://example",
		"BEADS_HTTP_URL=http://example",
	}
	got := stripS3EnvForScenario(env)
	for _, e := range got {
		key := strings.SplitN(e, "=", 2)[0]
		if strings.HasPrefix(key, "COOP_S3_") {
			t.Errorf("stripped env still contains %q", key)
		}
	}
	// Preserved keys must remain.
	for _, want := range []string{"PATH", "COOP_NATS_URL", "BEADS_HTTP_URL"} {
		if findEnv(got, want) == "" {
			t.Errorf("expected %s preserved, got missing", want)
		}
	}
}

func findEnv(env []string, key string) string {
	prefix := key + "="
	for _, e := range env {
		if v, ok := strings.CutPrefix(e, prefix); ok {
			return v
		}
	}
	return ""
}

func TestShellQuote(t *testing.T) {
	tests := map[string]string{
		"simple":              "'simple'",
		"with space":          "'with space'",
		"with'quote":          `'with'\''quote'`,
		"/path/to/file.toml":  "'/path/to/file.toml'",
	}
	for in, want := range tests {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %q, want %q", in, got, want)
		}
	}
}

// gd-zQ1SDfKUX5: a project-scoped token can only create a bead labelled with
// its project, so the project is settled, and checked against the token,
// before the agent bead is created.
func TestLocalAgentProject(t *testing.T) {
	cases := []struct {
		name, project, tokenProject, want, wantErr string
	}{
		{name: "unscoped, no project", want: ""},
		{name: "unscoped, project", project: "gardenio", want: "gardenio"},
		{name: "scoped, matching project", project: "gardenio", tokenProject: "gardenio", want: "gardenio"},
		{name: "scoped, no project", tokenProject: "gardenio", wantErr: "pass --project gardenio"},
		{name: "scoped, other project", project: "other", tokenProject: "gardenio", wantErr: `does not match the beads token's project "gardenio"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := localAgentProject(tc.project, tc.tokenProject)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want it to contain %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if got != tc.want {
				t.Fatalf("project = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestDockerAgentCreateRequest(t *testing.T) {
	req := dockerAgentCreateRequest("local-box", "gardenio")
	if req.Type != "task" || req.Assignee != "local-box" {
		t.Fatalf("bead shape wrong: %+v", req)
	}
	if got := strings.Join(req.Labels, ","); got != "kd:agent,project:gardenio" {
		t.Errorf("labels with project = %s", got)
	}
	if got := strings.Join(dockerAgentCreateRequest("local-box", "").Labels, ","); got != "kd:agent" {
		t.Errorf("labels without project = %s", got)
	}
}
